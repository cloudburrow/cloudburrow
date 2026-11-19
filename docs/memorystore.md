# Memorystore: a real Valkey, not the Memorystore API

`--services memorystore` runs [Valkey](https://valkey.io), the open-source Redis-compatible
server, in the cluster (#296). It follows the [Cloud SQL](cloudsql.md) decision (#121): Google
publishes no Memorystore emulator, so what runs is the kind of server Memorystore itself runs,
giving an application a real data plane at a stable local address. Since #1302 the
Memorystore admin API describes that server, and creates, resizes and deletes instances of
your own, each a server of its own running its `redis_version`: Redis 6.2 for `REDIS_6_X`,
Redis 7.0 for `REDIS_7_0`, and Valkey 8.1 for `REDIS_7_2`, the default (§4).

## 1. What you get

| | |
|---|---|
| Server | Valkey 8.1.10, `valkey/valkey:8.1-alpine` pinned by digest (linux/amd64 and arm64) |
| Address | `REDIS_HOST:REDIS_PORT` on the host (default `127.0.0.1:9016`, `--port-memorystore`), plus `memorystore.cloudburrow.svc.cluster.local:6379` in-cluster |
| Authentication | **none**; see below |
| Durability | an append-only file on a PersistentVolumeClaim in `--mode persistent`; nothing on disk in `--mode ephemeral` |

```sh
cloudburrow up --services memorystore
eval "$(cloudburrow env)"
valkey-cli -h "$REDIS_HOST" -p "$REDIS_PORT" PING   # or redis-cli
```

Any Redis client works. The compat suite uses `github.com/redis/go-redis/v9`:

```go
rdb := redis.NewClient(&redis.Options{Addr: os.Getenv("REDIS_HOST") + ":" + os.Getenv("REDIS_PORT")})
```

SET/GET, MULTI/EXEC, PUBLISH/SUBSCRIBE and Lua `EVAL` are all tested (`test/compat/memorystore_test.go`).
They are Valkey's own and behave as Valkey does.

## 2. Durability, measured

CI writes a key, stops the persistent compat instance, starts it again and reads the key back.
Then it starts the instance in `--mode ephemeral` and checks that the key is gone.

- **Persistent**: `--appendonly yes --appendfsync everysec` on a volume. A crash can lose up to
  one second of writes, which is Valkey's documented trade-off for this setting.
- **Ephemeral**: `--save "" --appendonly no`. Nothing is written to disk, so every restart
  starts empty.

`cloudburrow state save` does not capture Memorystore, created instances included, and
`cloudburrow reset` does not empty it. Use Valkey's own `BGSAVE`, or the append-only file on
the volume.

## 3. What you do not get

**Not all of the Memorystore admin API (`redis.googleapis.com`).** Instances are BASIC or
STANDARD_HA tier (§5) and `REDIS_3_2`, `REDIS_4_0`, `REDIS_5_0`, `REDIS_6_X`, `REDIS_7_0` or `REDIS_7_2`, with or without in-transit
encryption (§6) and with or without a customer-managed key (§7), with maintenance windows and
events (§4a): no IAM.
Each is 501.
`REDIS_3_2`, `REDIS_4_0` and `REDIS_5_0` (#1524) run Redis 3.2.12, 4.0.14 and 5.0.14, the last
release of each line the Docker official `redis` image built; what Memorystore documents for later
versions only is refused as it documents it (LFU policies, `activedefrag` and `lfu-*` need 4.0,
`stream-node-*` and read replicas 5.0), and in-transit encryption on them is 501, as Redis before 6.0
has no TLS of its own (#1768). Export and import to and from Cloud Storage RDB
files, and RDB persistence, are served (#1384) when the instance runs `storage` too.

**No authentication on the server `up` runs.** Protected mode is off and there is no password,
for the same reason Cloud SQL uses `trust`: nothing in CloudBurrow authenticates a request, and a
password printed in the banner would imply otherwise. The host endpoint is bound to loopback. Do
not expose it. An instance you create with `auth_enabled` does require AUTH (§4).

## 4. The admin API

`--services memorystore` also serves `google.cloud.redis.v1.CloudRedis` on
`--port-memorystore-admin` (default `9021`), gRPC and JSON on one port, with
`google.longrunning.Operations` for its operations, exported as
`CLOUDBURROW_MEMORYSTORE_ENDPOINT` (#1302). `cloudburrow gcloud-setup` points `gcloud redis` at
it, and `cloudburrow terraform` points `google_redis_instance` at it.

**The instance `up` runs** is `projects/<default project>/locations/us-central1/instances/memorystore`:
host and port are `REDIS_HOST:REDIS_PORT`, `redis_version` and `memory_size_gb` are read from the
server (`REDIS_7_2`, 1 GB: the server runs with `--maxmemory 1gb` and Memorystore's default
`volatile-lru` eviction), and the state is READY while it answers. It has no AUTH
(`GetInstanceAuthString` returns an empty string), and it cannot be updated or deleted:
`UpdateInstance` and `DeleteInstance` on it are FAILED_PRECONDITION. Run `up` without
`memorystore` in `--services` to have none.

**Instances you create** are each a server of their own: a Deployment and Service in the
cluster, with the settings of the server `up` runs and the image of the instance's
`redis_version` (`redis:6.2` for `REDIS_6_X`, `redis:7.0` for `REDIS_7_0`, and the same Valkey 8.1
image for `REDIS_7_2`, the default), reached from the host through a tunnel of their own on an
OS-assigned port, which the instance's `host` and `port` report.

```go
c, _ := redis.NewCloudRedisClient(ctx, option.WithEndpoint(os.Getenv("CLOUDBURROW_MEMORYSTORE_ENDPOINT")),
	option.WithoutAuthentication(), option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
op, _ := c.CreateInstance(ctx, &redispb.CreateInstanceRequest{
	Parent: "projects/" + project + "/locations/us-central1", InstanceId: "cache",
	Instance: &redispb.Instance{Tier: redispb.Instance_BASIC, MemorySizeGb: 2, AuthEnabled: true}})
inst, _ := op.Wait(ctx) // READY, once its server answers
auth, _ := c.GetInstanceAuthString(ctx, &redispb.GetInstanceAuthStringRequest{Name: inst.Name})
rdb := goredis.NewClient(&goredis.Options{Addr: fmt.Sprintf("%s:%d", inst.Host, inst.Port), Password: auth.AuthString})
```

```sh
gcloud redis instances create cache --region=us-central1 --size=2 --tier=basic --enable-auth
```

| Field | What it does |
|---|---|
| `tier` | BASIC, one server; or STANDARD_HA, a primary and replicas (§5) |
| `replica_count`, `read_replicas_mode`, `alternative_location_id` | STANDARD_HA only (§5) |
| `memory_size_gb` | 1 to 300: the server's `maxmemory` |
| `redis_version` | empty or `REDIS_7_2`, what Valkey 8.1 is; any other is 501 |
| `transit_encryption_mode` | `SERVER_AUTHENTICATION`: TLS only, with a CA of the instance's own, which `server_ca_certs` lists (§6) |
| `auth_enabled` | `--requirepass` with a generated string (a UUID, as Google's), which `GetInstanceAuthString` returns. It is in the server's Deployment arguments in the cluster |
| `redis_configs` | `maxmemory-policy` (default `volatile-lru`), `notify-keyspace-events`, `activedefrag`, `lfu-decay-time`, `lfu-log-factor`, `stream-node-max-bytes`, `stream-node-max-entries`, and `maxmemory-gb` (a `maxmemory` below the size); another is INVALID_ARGUMENT |
| `display_name`, `labels`, `location_id` | kept and returned; placement is not real, so any region is accepted |

`UpdateInstance` changes `display_name`, `labels`, `memory_size_gb` and `redis_configs`; the size
and configs are set on the running server with `CONFIG SET`, so it keeps its keys, and a config
removed goes back to its default. `DeleteInstance` removes the server, its Service and its volume.

**Network** (#1906). `authorized_network` (default the project's `default` network; a bare name
is taken as one of the instance's project), `connect_mode` (`DIRECT_PEERING`, the default, or
`PRIVATE_SERVICE_ACCESS`) and `reserved_ip_range` are kept and returned. With `DIRECT_PEERING` the
range is an IPv4 CIDR block of internal addresses of at least `/29` (`/28` with read replicas), as
Google documents; with `PRIVATE_SERVICE_ACCESS` it may also be the name of an allocated range;
unset, a `/29` (`/28`) in 10.0.0.0/8 is chosen, the same for the same instance. Anything else is
INVALID_ARGUMENT, and none of the three can be updated. **What they cannot change here:** there
is no VPC, so they are stored and checked only. Every local client reaches the instance at its
`host` and `port`, the host's address (not one of the range), whatever network it is authorized
on; the network is not looked up (Compute Engine is not served), and Google's restricted ranges
are not checked.

**Modes.** A created instance follows the instance's mode, as the server `up` runs does. In
persistent mode its record is kept in `<state-dir>/<name>/memorystore`, its server has a volume
of its own, and after `stop` and `up` it is READY again (REPAIRING while its server starts), with
its AUTH string. **Its keys follow its `persistence_config`, as on Memorystore (#1911)**: without
persistence (`DISABLED`, the default) the server keeps no file (`--save "" --appendonly no`, and
whatever a previous run left on the volume is removed before it starts), so a Basic tier
instance's data is lost whenever its server restarts (a maintenance, a failed pod, `stop` and
`up`), as Google documents of an instance without RDB snapshots; with RDB the data comes back
from the last snapshot, written at the snapshot period (`save <period> 1`) and, as Valkey and
Redis do, when the server stops gracefully. A Standard tier instance's node that restarts syncs
from the current primary (§5), so only a restart of every node at once (`stop` and `up`) loses
the data of an instance without persistence. In ephemeral mode nothing is kept:
`up` removes the servers a previous run left, keeping their volumes as the server `up` runs keeps
its own, so a later persistent run has them back.

The console's Memorystore page lists the instances and has **Create instance**, **Edit** and
**Delete**, **Export** and **Import** on a created instance's page, and **Flush** (FLUSHALL) on
the instance `up` runs; the API itself has no flush. `ExportInstance` writes a `BGSAVE` snapshot
to a `gs://` object; `ImportInstance` replaces the instance's data with an RDB file's, loaded by
the server itself (`--services memorystore,storage`). `persistence_config` RDB sets the server's
save point to the snapshot period; `DISABLED` removes it (tests:
`TestMemorystoreDataAcrossServerRestart`, `TestMemorystoreCreatedAcrossRestart`,
`TestMemorystoreRDBAcrossRestart`). `UpgradeInstance` moves an instance to a later version (REDIS_3_2 is Redis 3.2, REDIS_4_0 Redis 4.0, REDIS_5_0 Redis 5.0, REDIS_6_X Redis 6.2, REDIS_7_0 Redis
7.0, REDIS_7_2 Valkey 8.1) with its data, and **Upgrade** on its page does too (#1386).
`FailoverInstance` promotes a Standard tier instance's replica (§5); `RescheduleMaintenance` moves
a scheduled maintenance event (§4a). See
[compatibility.md](compatibility.md#memorystore-for-redis--googlecloudredisv1).

## 4a. Maintenance (#1524)

Memorystore schedules maintenance updates itself; CloudBurrow's servers are pinned images, so no
update ever comes, and an event is scheduled locally instead: CloudBurrow's own control endpoint
(not a Memorystore API), on the admin port,

    POST /cloudburrow/v1/projects/P/locations/R/instances/I:scheduleMaintenance  {"startTime": "RFC 3339, optional"}
    POST /cloudburrow/v1/projects/P/locations/R/instances/I:cancelMaintenance

or **Schedule maintenance (CloudBurrow)** and **Cancel maintenance (CloudBurrow)** on a created
instance's page. From then on it behaves as Google documents
([about maintenance](https://docs.cloud.google.com/memorystore/docs/redis/about-maintenance),
[find and set maintenance windows](https://docs.cloud.google.com/memorystore/docs/redis/find-and-set-maintenance-windows),
read 2026-11-18):

- `maintenance_policy` holds one weekly window, a day and UTC start hour, one hour long (Edit's
  maintenance window on the page). An event with no start given starts in the first window at
  least a week away, or a week on, to the hour, with no window.
- `maintenance_schedule`: `start_time`, `end_time` an hour on, `can_reschedule` and
  `schedule_deadline_time`, one week after the originally scheduled time.
- `RescheduleMaintenance` (`gcloud redis instances reschedule-maintenance`, **Reschedule
  maintenance** on the page): `IMMEDIATE` applies it now; `NEXT_AVAILABLE_WINDOW` defers it a week,
  to the next window; `SPECIFIC_TIME` to a time within one week after the original time. At most
  twice, never past the deadline, and not when less than an hour remains (FAILED_PRECONDITION;
  `can_reschedule` is then false). With nothing scheduled it is FAILED_PRECONDITION.
- At its start the instance is `MAINTENANCE`: a Basic tier instance is unavailable while its
  server's pod is replaced; a Standard tier instance fails over to a replica (as an automatic
  failover does). Then it is `READY` with no `maintenance_schedule`. An event that comes due while
  an operation is in progress runs once it is done.

Google documents no error wording for the refusals, and no operation for the update itself, so
none is listed.

### Redis Cluster and Valkey maintenance (#1524)

Memorystore for Redis Cluster's `RescheduleClusterMaintenance` and Memorystore for Valkey's
`RescheduleMaintenance` work the same way, with each product's own rules
([cluster](https://docs.cloud.google.com/memorystore/docs/cluster/about-maintenance),
[Valkey](https://docs.cloud.google.com/memorystore/docs/valkey/about-maintenance), and their
find-and-set-maintenance-windows pages, read 2026-11-18). An event is scheduled through the same
control endpoint, on a cluster's name, or on a Valkey instance's with `{"api": "memorystore"}`
(an instance name that no Memorystore for Redis instance has is a Valkey instance's anyway):

    POST /cloudburrow/v1/projects/P/locations/R/clusters/C:scheduleMaintenance   {"startTime": "optional"}
    POST /cloudburrow/v1/projects/P/locations/R/instances/I:scheduleMaintenance  {"api": "memorystore"}
    ... :cancelMaintenance

or **Schedule maintenance (CloudBurrow)** on the cluster's or instance's page, where **Set
maintenance window** sets `maintenance_policy`.

- `maintenance_schedule` is `start_time` (the first one-hour weekly window a week away, or a week
  on, to the hour, with no window) and `end_time` an hour on. Neither API has
  `can_reschedule` or `schedule_deadline_time`.
- `IMMEDIATE` ("Update now") applies it now; `SPECIFIC_TIME` ("Custom day and time") moves it to a
  time between the original time and 14 days after it ("You can reschedule maintenance within 14
  days of the originally scheduled time"), INVALID_ARGUMENT otherwise. Neither page limits how many
  times. Not when less than an hour remains (FAILED_PRECONDITION); FAILED_PRECONDITION with nothing
  scheduled. gcloud: `gcloud redis clusters reschedule-maintenance` and `gcloud memorystore
  instances reschedule-maintenance`, `--reschedule-type=immediate|specific-time`.
- At its start the update is "a gradual deployment and create-before-destroy lifecycle strategy":
  shard by shard, node by node, replicas first, a new node joins the shard and replicates its
  primary; a primary hands over to its new node in a coordinated failover (`CLUSTER FAILOVER`; on a
  CLUSTER_DISABLED instance writes are paused, the replicas caught up, and the new node promoted,
  so the primary and reader endpoints reach the new nodes); then the old node is removed. The data
  is kept. A shard with no replica gets a new node first too. The state stays `ACTIVE` meanwhile
  (neither State enum has MAINTENANCE), and `UpdateCluster`/`UpdateInstance` waits until it is
  done.

UNVERIFIED (re-read 2026-11-18, the pages above, which give none): the error codes and wording, the
operations' verbs (`rescheduleClusterMaintenance`, `rescheduleMaintenance`), the state during the
update, and the default window with no policy. Unlike Google's Private Service Connect endpoints,
which "aren't affected by maintenance", a Redis cluster's or CLUSTER mode Valkey instance's
discovery endpoint is its first primary's host address here, so it moves with the replaced node;
cluster clients follow the topology. Tests: `TestRedisClusterRescheduleMaintenance`,
`TestValkeyRescheduleMaintenance` (official Go clients and gcloud),
`TestMemorystoreClusterAndValkeyMaintenanceThroughThePage` (browser), unit
`TestValkeyMaintenanceRules`, all in the memorystore shard.

## 5. Standard tier, read replicas and failover (#1385)

A `STANDARD_HA` instance is a primary and its replicas, each a server of its own: a Deployment
and Service per node (`<server>-n0`, `-n1`, ...), kept in step by the servers' own asynchronous
replication (`REPLICAOF`, with `masterauth` when AUTH is on). `nodes` lists them, `node-0` in
`location_id`, `node-1` in `alternative_location_id` (default another zone of the region), and
the others in the region's zones in turn; `current_location_id` is the primary's zone.

- **host and port** are an endpoint on the host that carries every connection to the current
  primary. In the cluster, the instance's Service (`<server>`) selects the primary's pod.
- **Read replicas**: `read_replicas_mode: READ_REPLICAS_ENABLED`, `replica_count` 1 to 5
  (default 2), on an instance of 5 GB or more, as Memorystore requires. `read_endpoint` and
  `read_endpoint_port` are a second host endpoint that spreads connections over the replicas, one
  each in turn; a replica refuses writes (`READONLY`). Without read replicas `replica_count` is 1
  and there is no read endpoint. Once enabled, read replicas cannot be disabled.
- **`UpdateInstance`** of `replica_count` starts replicas, which sync from the primary, or removes
  the highest numbered; the instance is UPDATING until they are. `read_replicas_mode` enables
  read replicas on a Standard tier instance. `secondary_ip_range` is accepted and not kept: there
  are no VPC addresses here.
- **`FailoverInstance`** (`gcloud redis instances failover`) promotes the replica with the
  greatest replication offset: `REPLICAOF NO ONE` on it, `REPLICAOF` it on every other node, the
  old primary among them, the Service re-pointed. The endpoint stays at the same host and port
  and drops the connections it carried, so clients reconnect and reach the new primary.
  `LIMITED_DATA_LOSS` (the default) aborts the operation FAILED_PRECONDITION when the greatest
  offset delta between the primary and a replica is 30 MB or more, or a replica is not
  replicating, as Memorystore documents; `FORCE_DATA_LOSS` does not check. The instance is
  FAILING_OVER meanwhile.
- **Automatic failover** (#1412): when the primary fails, the instance fails over on its own, as
  Memorystore's does, to the replica with the least replication lag (the greatest offset). The
  primary is checked every second, INFO replication through its tunnel; once it has not answered
  as a primary three checks in a row (its pod is gone or not Ready, or it came back as a replica)
  the replica is promoted as `FailoverInstance` promotes one with `FORCE_DATA_LOSS`, since the
  primary cannot be asked its offset. The instance is REPAIRING meanwhile and READY after; `host`
  and `port` stay the same and drop their connections; `current_location_id` moves. Writes the
  replicas had not received are lost, as Memorystore documents of its asynchronous replication.
  No operation is recorded (Memorystore records none for an automated repair); `up.log` has a
  line. Google documents no detection window, and says an instance is unavailable for 30 seconds
  on average during one; here it is a few seconds. Every node starts as a replica of the
  instance's Service, so a node whose pod restarts, the old primary among them, comes back as a
  replica of the current primary and syncs from it: a restarted primary never comes back empty as
  the primary, which would empty its replicas. A BASIC instance never fails over: its server
  restarts, empty or from its persistence.

```sh
gcloud redis instances create ha --region=us-central1 --tier=standard --size=5 \
  --read-replicas-mode=read-replicas-enabled --replica-count=2
gcloud redis instances failover ha --region=us-central1 --data-protection-mode=limited-data-loss
```

The console's Create instance has **Tier**, **Enable read replicas** and **Replicas**; a Standard
tier instance's page shows its read endpoint, zones and nodes, offers **Failover** with the data
protection mode, and Edit has **Replicas** (or **Enable read replicas**).

`ImportInstance`, `ExportInstance` and `UpgradeInstance` act on the primary, whose replicas sync
its data; an upgrade starts every node again on the new version.

## 6. In-transit encryption (#1413)

`transit_encryption_mode: SERVER_AUTHENTICATION` serves TLS only, as Memorystore does: the server
listens on port 6378 in the cluster (`--port 0 --tls-port 6378`) and refuses plain TCP. Its
certificate is issued by a Certificate Authority of the instance's own, generated at create as the
Cloud SQL connector's are (ECDSA P-256), valid ten years as Memorystore's; the server certificate is
valid 180 days and is issued again each time the server starts. `server_ca_certs` lists the CA, and
a client trusts it:

```go
inst, _ := c.GetInstance(ctx, &redispb.GetInstanceRequest{Name: name})
pool := x509.NewCertPool()
pool.AppendCertsFromPEM([]byte(inst.ServerCaCerts[0].Cert))
rdb := goredis.NewClient(&goredis.Options{Addr: fmt.Sprintf("%s:%d", inst.Host, inst.Port),
	TLSConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}})
```

```sh
gcloud redis instances create secure --region=us-central1 --size=1 --transit-encryption-mode=server-authentication
gcloud redis instances describe secure --region=us-central1 --format='value(serverCaCerts[0].cert)' > server-ca.pem
HOST=$(gcloud redis instances describe secure --region=us-central1 --format='value(host)')
PORT=$(gcloud redis instances describe secure --region=us-central1 --format='value(port)')
redis-cli -h "$HOST" -p "$PORT" --tls --cacert server-ca.pem PING
```

Terraform's `transit_encryption_mode = "SERVER_AUTHENTICATION"` does the same, and
`server_ca_certs[0].cert` is the CA. The certificate names the host's loopback (`127.0.0.1`, where
`host` and `port` are) and the instance's Services in the cluster. A Standard tier instance's nodes
replicate over TLS. Clients are not asked for a certificate, as SERVER_AUTHENTICATION has none;
the mode is set at create and cannot be changed. The CA and its key are
kept with the instance's record in persistent mode; the server's certificate and key are in its
Deployment's environment in the cluster, as the AUTH string is in its arguments.

The console's Create instance has **Enable in-transit encryption**, and the instance's page shows
**In-transit encryption** and a **Server CA** tab with the CA's PEM.

**Server CA rotation (#1463).** As Memorystore documents: five years after an instance is created
a new CA is made and listed in `server_ca_certs` beside the old one, which stays valid until it
expires (ten years after its creation) and is then no longer listed; the server's certificate is
issued by the new CA when it is next issued, which here is when the server starts again. Install
every listed CA on your clients (a bundle of all of them, in any order). Google has no API to
rotate sooner, only a support request; the console's **Rotate server CA**, on the page of an
instance with in-transit encryption, is that request: the new CA is listed at once and the server
is started again with a certificate of it (a short connection drop, as Memorystore's certificate
rotation is). It is not a maintenance event (§4a).

## 7. Customer-managed encryption keys (#1463)

`customer_managed_key` (gcloud `--customer-managed-key`, Terraform `customer_managed_key`, the
console's **Customer-managed encryption key**) names a Cloud KMS key, which must be in the
instance's region (`projects/P/locations/us-central1/keyRings/R/cryptoKeys/K`; another region is
INVALID_ARGUMENT, as Google's rule is). It is set at create only. Run `up` with `kms` in
`--services`; without it the create is FAILED_PRECONDITION. Each instance gets a 256-bit data key
of its own, which Cloud KMS `Encrypt` wraps with that key; only the wrapped data key is kept. As
Memorystore encrypts "metadata related to security features like AUTH and in-transit
encryption" with the key, the instance's AUTH string and its CA's private key are kept sealed
with the data key (AES-256-GCM) in the instance's record, never in the clear.

The key is checked (the data key unwrapped by `Decrypt`) at create, each time CloudBurrow starts
the instance again, and every two seconds. While the key version that wrapped the data key is
disabled or destroyed, or the key cannot be used, the instance is **suspended**, as Memorystore
suspends one: no READY state (`STATE_UNSPECIFIED`, the console's "No state"),
`suspension_reasons: [CUSTOMER_MANAGED_KEY_ISSUE]`, a status message saying why, and its server
stopped, so no client reaches the data; updates, imports, exports and failovers are
FAILED_PRECONDITION, and delete still works. Once the version is enabled again the instance
resumes on its own: its server runs again (REPAIRING) and it is READY. In persistent mode the
server's volume is kept while it is suspended, so the data comes back; in ephemeral mode it has
none and comes back empty. A destroyed version cannot be restored, so the instance stays
suspended, as Memorystore's does (a new instance and an import are the way back).

What is not encrypted with the key: the RDB snapshots and append-only file on the server's own
volume are written by Valkey, in the cluster, and are not encrypted by CloudBurrow (they are only
served while the key can be used); an export is a plain RDB file, as Memorystore protects an
export with the destination bucket's own CMEK, not the instance's. The console's instance page
shows **Encryption** (the key) and, while suspended, **Suspension reasons**.

## 8. Memorystore for Redis Cluster (#1519)

`google.cloud.redis.cluster.v1.CloudRedisCluster` is served on the same admin port as the
instances, gRPC and JSON, with the same operations. A cluster is a real Redis Cluster: `shard_count`
primaries with `replica_count` replicas each, every node a Valkey server in cluster mode, a
Deployment and Service of its own, the 16384 slots spread evenly over the primaries.

```sh
gcloud redis clusters create orders --region=us-central1 --shard-count=3 --replica-count=1 \
  --node-type=redis-shared-core-nano --network=projects/PROJECT/global/networks/default
gcloud redis clusters describe orders --region=us-central1 --format='value(discoveryEndpoints[0])'
```

Start a cluster client (go-redis `ClusterClient`, redis-py `RedisCluster`, Lettuce, ...) from the
`discoveryEndpoints[0]` address and port: it reads `CLUSTER SLOTS` and follows MOVED to every node.
Each node serves clients on one port that is the same in the cluster and on the host; the nodes
gossip on their pod addresses (bus port 16379).

Host clients and clients inside the Kubernetes cluster are each given addresses they reach
(#1614), with no address on any network. On an instance bound to `127.0.0.1` (the default), each
node binds its pod's address and `::1` only, not `127.0.0.1`; its host tunnel listens on
`127.0.0.1` and `[::1]` at the node's port, and the kubelet's port forwarding dials the pod's
`localhost` over IPv4 first and over IPv6 when nothing listens there, so a host client's
connections reach the node over IPv6. Valkey announces one client address per address family, so
each node announces `::1` to IPv6 clients (`cluster-announce-client-ipv6`): the host's clients, which
`CLUSTER SLOTS` and MOVED send to `[::1]:PORT`, through the tunnels; and its Service's cluster IP to
IPv4 clients (`cluster-announce-client-ipv4`): the pods', which reach the node through its Service
(`redis-c<cluster>-<hash>-<n>.<namespace>.svc.cluster.local`, at the node's own port; the first
node's is the discovery endpoint's). Start
an in-cluster client from any node's Service. With in-transit encryption, a node's certificate
names `::1` and its Service's cluster IP too. `TestMemorystoreRedisClusterHostAndPodClients`
proves both end to end: a go-redis `ClusterClient` on the host and `valkey-cli -c` in a pod write
and read each other's keys in every shard. A Valkey 7.2 node (`VALKEY_7_2`, #1557) has no
per-family announce and announces the host's address alone, as the hostname.

An instance started with `--bind-address <the host's LAN address> --allow-remote` announces that
address to every client instead: a pod reached a listener on the host's LAN address (measured
2026-11-17 on Docker Desktop for macOS), as Google's node addresses are reachable from the whole
VPC, but it publishes every endpoint of the instance on that network. Other routes were measured
on 2026-11-17 and not taken: a `*.localhost` hostname resolves through macOS's resolver but not
Go's own ("no such host"); the host reaches neither `host.docker.internal` (192.168.65.254, through
which a pod reaches the host's `127.0.0.1`) nor the kind node's Docker address; and a loopback alias
(`sudo ifconfig lo0 alias ...`) needs root.

`UpdateCluster` of `shard_count` adds primaries and moves slots, keys and all, to them (or moves
them off the primaries removed), as `redis-cli --cluster reshard` does; `replica_count` adds or
forgets replicas; `node_type` sets each node's `maxmemory` (1.4, 6.5, 13 or 58 GB) and
`redis_configs` are set on every running node. Shards and replicas change in separate updates,
as Google requires. Terraform's `google_redis_cluster` works through `redis_custom_endpoint`, and
the console has a Memorystore Redis Cluster page.

**In-transit encryption** (#1549): `--transit-encryption-mode=server-authentication`
(`TRANSIT_ENCRYPTION_MODE_SERVER_AUTHENTICATION`) gives the cluster a CA of its own, as §6 gives an
instance one, and every node a server certificate of it, issued each time the node starts, for
the host's address and the node's Service. Each node serves TLS only, on its one port; the nodes
gossip (`tls-cluster`) and replicate over TLS. `GetClusterCertificateAuthority` returns the CA as
`managedServerCa.caCerts[0].certificates`; trust it in the cluster client:

```go
ca, _ := c.GetClusterCertificateAuthority(ctx, &clusterpb.GetClusterCertificateAuthorityRequest{Name: name + "/certificateAuthority"})
pool := x509.NewCertPool()
pool.AppendCertsFromPEM([]byte(ca.GetManagedServerCa().GetCaCerts()[0].GetCertificates()[0]))
rc := redis.NewClusterClient(&redis.ClusterOptions{Addrs: []string{discovery},
	TLSConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}})
```

It cannot be changed after the cluster is created, as Google's cannot.

**Server CA modes** (#1907): `server_ca_mode` and `server_ca_pool`, with in-transit encryption.
`SERVER_CA_MODE_GOOGLE_MANAGED_PER_INSTANCE_CA` (the default) is the cluster's own CA above.
`SERVER_CA_MODE_GOOGLE_MANAGED_SHARED_CA` is one CA per project and region, made with the first
such cluster and kept: every such cluster's node certificates are its, and
`GetSharedRegionalCertificateAuthority` (`projects/P/locations/R/sharedRegionalCertificateAuthority`)
returns it, as each cluster's `GetClusterCertificateAuthority` does, so one bundle trusts every
cluster of the region. `SERVER_CA_MODE_CUSTOMER_MANAGED_CAS_CA` with
`server_ca_pool=projects/P/locations/R/caPools/POOL` has this instance's Certificate Authority
Service (`--services ...,privateca`, in process) issue each node's certificate when the node
starts (a configuration-based request for a fresh P-256 key, the node's names and addresses in the
SANs), so it is listed among the pool's certificates and chains to the pool's CAs, which the
nodes trust; `GetClusterCertificateAuthority` is `FAILED_PRECONDITION` ("not supported for
customer-managed CAs") and clients trust the pool's root, as Google documents. Refused as Google
documents: a customer-managed mode without a pool ("server_ca_pool must be provided ..."), a pool
with another mode, a pool outside the cluster's region, a pool that does not exist (`NOT_FOUND`,
"Resource ... not found."), one with no enabled CA (`FAILED_PRECONDITION`), and either non-default
mode without in-transit encryption. The mode and pool cannot be changed. Not enforced: the
service agent `service-PROJECT_NUMBER@cloud-redis.iam.gserviceaccount.com` and its
`roles/privateca.certificateRequester` on the pool, as no call here is authorized. Not served:
the weekly rotation and `rotate_server_certificate` (a node's certificate is issued again each
time it starts); a Memorystore for Valkey instance's server CA modes stay UNIMPLEMENTED.
Tests: `TestRedisClusterServerCAModes` (official clients and go-redis over TLS), unit
`TestClusterServerCA`.

**Automatic failover** (#1910). Google: when a shard's primary fails, a replica is promoted
automatically, and the failed node is replaced. Valkey's own cluster failover needs the votes of
a majority of the primaries, so a cluster of one or two shards never fails over on its own; so
CloudBurrow checks every node of a Redis cluster (and of a Valkey instance in `CLUSTER` mode)
every second, `CLUSTER MYID` and `ROLE` through its tunnel. A primary that has not answered three
checks in a row, or answers with another node ID (its pod started again, empty: a node's cluster
configuration is not kept), has failed: unless a replica of its shard already serves as the
primary, the replica with the greatest replication offset is promoted (`CLUSTER FAILOVER
TAKEOVER`, which needs no votes), and `discovery_endpoints` moves to it when it is the first
shard's. A node that started again rejoins: its settings are set again, its old identity is
forgotten on every other node, it `MEET`s the cluster and replicates its shard's primary (emptied
first), so the shard keeps its replica count; a shard with no replica takes the node back as its
primary, with the slots its old identity had and what it loaded from its persistence. The cluster
stays `ACTIVE`; writes the replica had not received are lost, as with Google's asynchronous
replication. Roles changed in the cluster itself (Valkey's own failover, a client's `CLUSTER
FAILOVER`) are taken into the record. Measured: failed over 3 to 4 seconds after the primary's pod
was deleted, the node back as a replica about 10 seconds later. Tests:
`TestRedisClusterAutomaticFailover`, `TestValkeyClusterModeAutomaticFailover` (one shard, one
replica; official clients and go-redis), and for a Standard tier instance
`TestMemorystoreAutomaticFailoverReadEndpoint` (its read endpoint keeps serving reads at the same
address after the failover).

**IAM auth** (#1614): with `AUTH_MODE_IAM_AUTH` a client sends an access token as its AUTH
password, with no username, `default` or the principal's email, as Google documents. The token is
checked: the nodes take it only when this instance minted it (its metadata server, its token
endpoint for the ADC fixture, or IAM Credentials' `generateAccessToken`), it has not expired, and
the project's IAM policy grants its account `roles/redis.dbConnectionUser` (a Redis cluster) or
`roles/memorystore.dbConnectionUser` (a Valkey instance) as `serviceAccount:` or `user:`. Anything
else is refused `WRONGPASS invalid username-password pair or user is disabled`, and no AUTH is
`NOAUTH Authentication required`; a connection already authenticated keeps working after its token
expires or the role is taken back, as Google documents. Nothing is asked of Google: the instance
keeps the SHA-256 of every token it mints, with its account and expiry
(`<state-dir>/<name>/issued-tokens.json`, so a cached token outlives `stop` and `up`), and gives
every node's default user, beside a password of the cluster's own, the hashes of the live tokens
of the principals granted the role, and each such principal an ACL user named by its email. A new
token is on the nodes before it is returned to the client; policy changes and expiries reach them
within seconds (Google: "may take a few minutes"). Not served: a condition on the binding
(Resource Manager refuses conditions, so a grant is project-wide), basic roles (`roles/owner` is
not expanded), groups and domains (Google does not support them either). A Redis cluster's
`authorization_mode` changes between `AUTH_MODE_DISABLED` and `AUTH_MODE_IAM_AUTH` on the running
nodes, in an update of its own. A cluster or instance kept from before #1614 with IAM auth still
takes any AUTH string until it is created again. `TestMemorystoreIAMAuthChecksTheToken`.

**Token auth** (#1549): with `AUTH_MODE_TOKEN_AUTH` a client logs in as a token auth user
with a token the service generates: `AddTokenAuthUser`, then `AddAuthToken`, and
`GetAuthToken` / `ListAuthTokens` return the token. Each user is a Valkey ACL user on every
node (`ACL SETUSER <user> reset on ><token>... ~* &* +@all`), set again on a node that starts
and on every node whenever a user or token changes. The nodes' default user has a password
only CloudBurrow's admin plane knows (and the replicas, `masterauth`), so a client without a
token is refused (`NOAUTH`), and one with a deleted or wrong token too (`WRONGPASS`):

```go
rc := redis.NewClusterClient(&redis.ClusterOptions{Addrs: []string{discovery}, Username: "app", Password: token.GetToken()})
```

The mode cannot be changed to or from token auth after create (UNIMPLEMENTED).

**ACL policies** (#1549): `CreateAclPolicy` with rules, a username and a Valkey ACL rule
each (`on >secret ~app:* +@read`), and a cluster's `acl_policy` (at create or by
`UpdateCluster`) make each rule a Valkey ACL user on every node (`ACL SETUSER <user> reset
<rule>`). `UpdateAclPolicy` is a new revision (`GetAclPolicyRevision`,
`ListAclPolicyRevisions`) set on every node of every attached cluster; detaching deletes the
users; an attached policy cannot be deleted.

**Backups** (#1549): each cluster has a backup collection,
`projects/P/locations/L/backupCollections/<cluster UID>`, which outlives it.
`BackupCluster` (`gcloud redis clusters create-backup`) keeps an RDB snapshot of each shard's
primary (`BGSAVE`, then the file, read from the node's container) in the instance's
Memorystore state: on disk in persistent mode, in memory in ephemeral mode. `ListBackups`,
`GetBackup` and `DeleteBackup` act on them; a backup past its `ttl` (default 100 years) is
removed. `ExportBackup` writes the files to a bucket of this instance's Cloud Storage, as
`gs://BUCKET/<backup ID>/<file>` (`--services memorystore,storage`). A cluster created with
`managed_backup_source` (a backup) or `gcs_source` (RDB files) loads them once its nodes
serve: each file is served by a second server on a loopback port in the first primary's
container, which `MIGRATE`s every key to the node that owns its slot, so a backup restores
into any shard count. `automated_backup_config` is kept and returned, and its daily backup is
taken at the start hour (UTC), kept for its `retention` (default 35 days).

```sh
gcloud redis clusters create-backup orders --region=us-central1 --backup-id=nightly --ttl=7d
gcloud redis clusters backups list --backup-collection=COLLECTION --region=us-central1
gcloud memorystore instances create cache --location=us-central1 --authorization-mode=token-auth ...
gcloud memorystore instances create-token-auth-user cache --location=us-central1 --token-auth-user=app
gcloud memorystore instances token-auth-users create-auth-token app --instance=cache --location=us-central1
```

Terraform's `automated_backup_config` works on both resources. The console's Create has **Token
authentication**, **Restore from backup** and (a cluster's) **ACL policy**; a page has **Token
auth** and **Backups** tabs and their actions (add and delete users and tokens, back up now,
export and delete a backup; on a cluster, an **ACL policies** tab to create, update, delete and
attach the location's policies). gcloud's Redis Cluster token auth commands are
alpha and beta only, on `v1beta1`, which is not served; use the API or the console.

**CMEK** (`kms_key`, #1557), for a cluster and a Valkey instance (§9) alike: the key must be a
CryptoKey in the cluster's region that this instance's Cloud KMS (`--services ...,kms`) can use;
an `Encrypt` with it wraps the cluster's data key (256-bit AES), kept only wrapped. What the key
protects: the cluster's kept secrets (token auth users' tokens, the default user's password, the
in-transit CA's private key), sealed with the data key and never kept in the clear, and each
backup's RDB files, sealed with it (the backup keeps the wrapped data key, so it is restored or
exported only while the key can be used). What it does not: the nodes' data in memory, which
Valkey serves to clients as it is; an export, written to the bucket as plain RDB files under the
bucket's own encryption; and the rest of the cluster's record (its shape, labels, configs). The
key is checked every 2 seconds: once it cannot be used (disabled, destroyed, its version
disabled), the cluster is suspended as a Memorystore for Redis instance (§6) and a Cloud SQL
instance are: not `ACTIVE`, `encryption_info.kms_key_primary_state` says why, and its nodes are
stopped, their data first kept as RDB files sealed with the data key; once the key can be used
again new nodes are formed (`UPDATING`) and the data loaded, then `ACTIVE`.

Not served: cross-cluster replication (UNIMPLEMENTED: a Valkey cluster-mode replica replicates
only a primary of its own cluster, `CLUSTER REPLICATE` taking a node its gossip knows, so one
cluster cannot be another's secondary).

**Across a restart** (#1557). After `cloudburrow stop` and `up` (persistent mode, the same state
directory) a cluster is formed again with its users, policy and backups (`CREATING` until it
serves). Its data follows its `persistence_config`, set on every node (RDB: `save <period> 1`;
AOF: `appendonly yes` with its `appendfsync`): each node keeps its files on its own volume, in a
directory of its formation (`/data/g<N>`), and each formation after a restart starts empty in the
next one. **With RDB or AOF**, the previous formation's files of each shard's primary are served by
a loopback server in that node's container and their keys moved (`MIGRATE`) to the primaries that
own their slots now, then the previous directory is removed: every key the files held comes back
(RDB: as of the last snapshot, which Valkey also writes when it stops; AOF: as of the last fsync).
**Without persistence** (`DISABLED`, the default) nothing is loaded and the cluster comes back
empty, as Google documents: without persistence a node's data is lost when it restarts, and nodes
do not load snapshots made before persistence was disabled. In ephemeral mode nothing is kept.
`TestMemorystoreClustersAcrossRestart` checks both, for Redis clusters and Valkey instances in
either mode.

**gcloud and Terraform** (#1557): `gcloud redis clusters create --kms-key` and
`google_redis_cluster`'s `kms_key` are tested (`TestGcloudMemorystore1557`,
`TestTerraformMemorystore1557`); the console's Create cluster has **Customer-managed encryption
key** and **Persistence**, and the cluster's page shows its Encryption, key and Persistence.

## 9. Memorystore for Valkey (#1519)

`google.cloud.memorystore.v1.Memorystore` (memorystore.googleapis.com) is served on the same
admin port: gRPC beside CloudRedis, so `cloud.google.com/go/memorystore/apiv1` takes
`CLOUDBURROW_MEMORYSTORE_ENDPOINT` as it is; its JSON API under `/memorystore`, since its paths
are CloudRedis's (`/v1/projects/*/locations/*/instances`). `cloudburrow gcloud-setup` writes
`api_endpoint_overrides/memorystore = http://HOST:PORT/memorystore/`, and `cloudburrow
terraform` sets `memorystore_custom_endpoint = http://HOST:PORT/memorystore/v1/`.

```sh
gcloud memorystore instances create cache --location=us-central1 --shard-count=3 --replica-count=1 \
  --node-type=shared-core-nano --psc-auto-connections=network=projects/PROJECT/global/networks/default,projectId=PROJECT
```

An instance is built as a Redis cluster is (§8), of the same Valkey 8.1 image:

- **`CLUSTER` mode** (the default): `shard_count` primaries (1 to 250) with `replica_count`
  replicas each (0 to 5), the slots spread over the primaries. `endpoints` asked with a
  `psc_auto_connection` come back with a `CONNECTION_TYPE_DISCOVERY` connection, the first
  primary's host address and port, which `discovery_endpoints[0]` repeats; start a cluster client
  there.
- **`CLUSTER_DISABLED` mode**: one primary (`shard_count` 1) and `replica_count` replicas, plain
  Valkey servers kept in step by `REPLICAOF`. The connections are `CONNECTION_TYPE_PRIMARY` and
  `CONNECTION_TYPE_READER`, two host addresses that outlive any one node (#1614): the primary
  endpoint carries each connection to the current primary, and the reader endpoint spreads
  connections over the replicas, one each in turn, as Google's "connect to any of the available
  replicas". In the cluster each node's pod is labelled with its role
  (`cloudburrow.dev/memorystore-role` `primary` or `replica`), and Services named after the
  instance's servers with `-primary` and `-reader` select by it, on port 6379 (6378 with in-transit
  encryption); replicas replicate the primary at its node's Service, which follows its pod.
- **Automatic failover** (#1614), as §5's for the Redis instances: the primary is checked every
  second, INFO replication and server through its tunnel. Once it has not answered as a primary
  three checks in a row (its pod is gone or not Ready), or at once when it answers with a `run_id`
  other than the last (its server started again, its replicas' stream gone), the replica with the
  most of its data among those fit to promote is promoted (`REPLICAOF NO ONE`, then every other
  node `REPLICAOF` it), the roles are
  labelled again, and the primary endpoint drops its connections, so a client reconnects to the
  same address and reaches the new primary. From the primary's first failed check until it answers
  again as the same primary, or a replica is promoted, the primary endpoint carries no
  connections, so a write never reaches a pod that started again empty. Google says "automatic failovers can take time on the
  order of tens of seconds" and that client connections are likely to be reset; here writes
  resume in a few seconds (3 to 4 measured). The instance stays `ACTIVE`; writes the replicas had
  not received are lost, as replication is asynchronous; `up.log` has a line. A node that comes
  back answering as a primary (its pod started again) is made a replica of the current primary,
  and a new pod is labelled within five seconds. Google documents no manual failover method for
  Memorystore for Valkey (`google.cloud.memorystore.v1` has none), so there is none here. A
  secondary of cross-instance replication, and an instance being migrated into, are not failed
  over: their primary replicates another server. `CLUSTER` mode is unchanged.
- **One failover per failure** (#1657). A new replication ID with the same `run_id` is not a
  failure: a primary takes one when promoted, and again when it gains a replica it has no backlog
  for, which a promoted node did about a second after the old primary's node rejoined it, and the
  health check then failed over a second time, back to that node, still empty or syncing (seen in
  `TestMemorystoreValkeyCrossInstanceFollowsFailover`). A replica is not promoted while it loads
  the primary's dataset (`master_sync_in_progress:1`), when it has not synced since it started
  (`master_link_status:down` with `master_link_down_since_seconds:-1`, a node that rejoined empty),
  when it lost the primary more than ten seconds before the primary began failing, or when it is
  30 MB or more behind the replica with the most. Google documents only that the system "elects a
  replica to be the new primary"
  ([High availability and replicas](https://docs.cloud.google.com/memorystore/docs/valkey/ha-and-replicas),
  re-read 2026-10-19), so this follows Valkey's own failover, which passes over a replica whose
  data is too old. With none fit there is no failover: the checks go on, the primary endpoint
  fenced, and a primary that started again with none fit is kept. For 15 seconds after a failover
  the instance settles: only a new primary that stops answering or starts again is failed over.
  Each failover is recorded in `/admin/events` (`service=memorystore&kind=failover`, the instance
  as its target, `from` and `to` the nodes' servers); `TestMemorystoreValkeyAutomaticFailover`
  (one replica, written to throughout) finds exactly one per pod deletion, and every write
  acknowledged after it read back.
- Connections come back in the form they were asked in: `endpoints`, or the deprecated
  `psc_auto_connections`; the Terraform provider reads each back as what it was configured with.

`UpdateInstance` changes `shard_count` (slots and keys move), `replica_count`, `node_type` (1.4,
6.5, 13 or 58 GB of `maxmemory`), `engine_configs` and `persistence_config`, set on every running
node, and keeps `labels`, `deletion_protection_enabled`, `endpoints`, zone distribution and
maintenance policy.

**Zone distribution (#1908).** A Redis cluster's and a Valkey instance's
`zone_distribution_config` is kept and checked: `SINGLE_ZONE` needs `zone`, a zone of the
instance's region, and a Valkey instance's `zones` (MULTI_ZONE) are zones of its region, each
once; it is immutable (an update that changes it is `INVALID_ARGUMENT`). There are no zones
locally, so each node's server has Valkey's `availability-zone` set to its zone, which `HELLO`
returns to clients: `SINGLE_ZONE` puts every node in `zone`; `MULTI_ZONE` spreads each shard's
nodes over region-a, -b and -c (or `zones`), its first node in zone *shard mod 3*, and a Valkey
instance's `active_zones` lists the zones its nodes are in. A Memorystore for Redis instance's
`location_id` must be a zone of its region. `persistence_config` RDB is `save <period> 1`, AOF `appendonly yes` with its
`appendfsync`; after a restart of CloudBurrow an instance comes back with its data when it has
RDB or AOF persistence, and empty without, as a Redis cluster does (§8, Across a restart). The
console's **Memorystore for Valkey** page lists, creates (either mode), edits and deletes
instances.

**Engine versions** (#1557): `VALKEY_7_2` runs Valkey 7.2.14 (`valkey/valkey:7.2-alpine`),
`VALKEY_8_0` (the default) Valkey 8.1 and `VALKEY_9_0` Valkey 9.0.6 (`valkey/valkey:9.0-alpine`),
each pinned by digest, for amd64 and arm64. Valkey 7.2 has no `cluster-announce-client-ipv4`, so
a 7.2 node in `CLUSTER` mode announces the host's address to clients as its hostname
(`cluster-preferred-endpoint-type hostname`). An `UpdateInstance` of `engine_version` to a later
version is a rolling upgrade (#1563): new nodes of the new image join as replicas of the old ones,
the first of each shard is promoted once it has synced, and the old nodes, which serve every write
until then, are removed. In `CLUSTER` mode the promotion is a `CLUSTER FAILOVER` (the old primary
pauses its clients until the new node has its offset); in `CLUSTER_DISABLED` mode the old
primary's writes are paused (`CLIENT PAUSE WRITE`) until the new node has caught up, which then
stops replicating while the old primary replicates it, so a write sent to it afterwards is
READONLY rather than lost. Every acknowledged write is kept
(`TestMemorystoreValkeyUpgradeKeepsWrites`, `TestMemorystoreValkeyClusterUpgradeKeepsWrites`).
The endpoints are the new nodes' host addresses afterwards; a `CLUSTER_DISABLED` instance's primary
and reader endpoints keep their addresses and point at the new nodes before the old ones are
removed and the operation is done (#1669). A downgrade is INVALID_ARGUMENT.

**Cross-instance replication** (#1557), for `CLUSTER_DISABLED` instances: an instance created with `cross_instance_replication_config` `instance_role`
`SECONDARY` and `primary_instance` (an `ACTIVE` `CLUSTER_DISABLED` instance of the same
`authorization_mode`) has its primary node replicate the primary instance's (`REPLICAOF`), its
replicas chained to it: it serves the primary's keys and refuses writes (READONLY). Each member
reports the `membership`. `UpdateInstance` of `cross_instance_replication_config` (alone)
switches over (a secondary to `PRIMARY`, every other member as its `secondary_instances`) and
detaches (a secondary to `NONE`; a primary to `NONE`, or to fewer `secondary_instances`). A
primary with secondaries is not deleted (FAILED_PRECONDITION). With in-transit encryption
(#1563) a secondary's `transit_encryption_mode` must be its primary's (else INVALID_ARGUMENT);
each member keeps its own CA, which its clients trust, and its nodes trust every member's CA
(given to a node when it starts, and to the running nodes by `CONFIG SET tls-ca-cert-file`
before a switchover), so they replicate over TLS (`tls-replication`) whichever member is the
primary (`TestMemorystoreValkeyCrossInstanceReplicationTLS`). A secondary replicates its primary
instance at that instance's in-cluster primary Service (#1614), which selects whichever node is
its primary, and every node's certificate covers that Service's name, so the secondary keeps
replicating, over TLS too, across the primary instance's automatic failover, with no relinking
(`TestMemorystoreValkeyCrossInstanceFollowsFailover`: the primary instance's primary pod deleted,
a key written at its promoted replica is read on the secondary; a switchover after it); a primary
instance without a replica, which does not fail over, is replicated at its node's own Service.
A switchover or detach holds every member it touches for its whole run (#1642): none is failed
over automatically while its nodes are re-pointed (the former primary's primary node, made a
replica of the new primary, would otherwise look failed), and while any member has an operation
in progress, its automatic failover included, the switchover or detach, and a new secondary of
it, is FAILED_PRECONDITION, to be tried again, as Google's `memorystore.v1` API refuses an
operation on an instance with one running ("Backup call will be rejected if there is an ongoing
backup or update operation", `BackupInstance`) (`TestValkeySwitchoverFencesHealthCheck`).
The former primary, a secondary afterwards, is no longer health-checked, so no fence of its
primary endpoint outlives the switchover (#1652): a check that began before the switchover and
ends after it, finding the primary node a replica, does not fence it, and a fence left on an
instance that is no longer checked (a secondary, a migration's target) is lifted at the next
check, so its primary endpoint reaches its primary node instead of closing every connection
(`TestValkeySwitchoverCheckLeavesEndpointOpen`).
In `CLUSTER` mode it is 501, as a
Redis cluster's (§8): Google documents it with the same shards and slots on both, but a
cluster-mode Valkey node refuses `REPLICAOF` ("REPLICAOF not allowed in cluster mode", measured
in `TestMemorystoreValkeyClusterUpgradeKeepsWrites` on 8.1) and its replicas follow only primaries
of their own cluster, so a shard cannot replicate another cluster's primary.

**Migration** (`StartMigration`, `FinishMigration`, #1557) into a `CLUSTER_DISABLED` instance:
its primary replicates `self_managed_source`'s `ip_address` and `port` (state `MIGRATING`,
`migration_config.state` `REPLICATION_ESTABLISHED` once linked); `FinishMigration` waits until
its offset reaches the source's (unless `force`), stops replicating, and the instance is `ACTIVE`
and `MIGRATED`. A source that is a server of this CloudBurrow instance (a Memorystore for Redis
instance, a node of a Valkey instance or Redis cluster) is named by the host address its clients
use and reached at its in-cluster address; any other address is replicated from as it is.
`network_attachment` is checked and kept. A source with AUTH is not served:
`self_managed_source` has no credentials (rechecked in googleapis' `memorystore/v1` proto,
#1563), and Google's guide has AUTH and TLS turned off on the source. A `CLUSTER` mode target is
501: Google's migration replicates each source shard into the target shard of the same slots,
continuously until `FinishMigration`, which a cluster-mode node cannot do (it refuses
`REPLICAOF`), and a keyspace scan would copy the keys once and lose the writes made during the
migration. A target with in-transit encryption is 501 too (with the reason).

**gcloud, Terraform and the console** (#1557). gcloud 587.0.0's `memorystore instances create
--kms-key`, `--engine-version`, `--cross-instance-replication-config-role=secondary
--primary-instance` (with `--transit-encryption-mode=server-authentication` too,
`TestGcloudMemorystore1563`), `instances update --engine-version` and
`--cross-instance-replication-config-role=none`, `start-migration` and `finish-migration` are
tested (`TestGcloudMemorystore1557`); `UpdateInstance` takes a message field's own paths in its
`update_mask` (`cross_instance_replication_config.instance_role`), as gcloud sends them.
`google_memorystore_instance`'s `kms_key`, `engine_version` (changed in place, the same `uid`) and
`cross_instance_replication_config` apply and plan empty (`TestTerraformMemorystore1557`; with
`transit_encryption_mode` `SERVER_AUTHENTICATION` on both, `TestTerraformMemorystore1563`); the
provider has no migration fields. The console's Create instance has **Engine version**,
**Persistence**, **Customer-managed encryption key** and **Secondary of (cross-instance
replication)**; Edit has **Engine version** (an upgrade); the instance's page shows its
Encryption, key and key state, its cross-instance replication role, primary, secondaries and
membership, and a migration's state and source; a secondary's page has **Switchover** and
**Detach**, a `CLUSTER_DISABLED` instance's **Start migration** (source IP and port) and, while
migrating, **Finish migration**; **Enable in-transit encryption** with **Secondary of** makes a
TLS secondary (`TestConsoleValkeyReplicationMigrationVersions`, `TestConsoleValkeySecondaryWithTLS`,
`TestMemorystoreValkeySecondaryThroughThePage`).

Not served: node types whose size is not modelled here and server CA modes, each 501 with the
reason.

`TOKEN_AUTH`, backups (`BackupInstance`, `ListBackupCollections`, `GetBackupCollection`,
`ListBackups`, `GetBackup`, `DeleteBackup`, `ExportBackup`, `automated_backup_config`) and
restore through `managed_backup_source` or `gcs_source` are a Redis cluster's (§8), for an
instance in either mode: a `CLUSTER_DISABLED` instance's backup is its primary's file, and a
restore MIGRATEs the keys to its primary. ACL policies (`google.cloud.memorystore.v1`
`aclPolicies`, since #1578) are a Redis cluster's too: an instance's `acl_policy`, at create or
through `UpdateInstance`, makes each rule a Valkey ACL user on every node; an update of the policy
sets the new rules on every node of each attached instance (`internal/service/redisadmin/memorystoreacl.go`).

In-transit encryption (`SERVER_AUTHENTICATION`) and `IAM_AUTH` are a Redis cluster's (§8): a CA of
the instance's own, which `GetCertificateAuthority` returns as `managedServerCa` (gRPC
`{instance}/certificateAuthority`, JSON `/memorystore/v1/{instance}/certificateAuthority`), every
node serving TLS only, a `CLUSTER_DISABLED` instance's replicas replicating over TLS; `IAM_AUTH`
checks the token as a Redis cluster's does (§8, #1614). The console's Create has **Enable in-transit encryption** and
**IAM authentication**, and the instance's page a **Server CA** tab.
