# Memorystore: a real Valkey, not the Memorystore API

`--services memorystore` runs [Valkey](https://valkey.io), the open-source Redis-compatible
server, in the cluster (#296). It follows the [Cloud SQL](cloudsql.md) decision (#121): Google
publishes no Memorystore emulator, so what runs is the kind of server Memorystore itself runs,
giving an application a real data plane at a stable local address. Since #1302 the
Memorystore admin API describes that server, and creates, resizes and deletes instances of
your own, each a Valkey server of its own (§4).

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
STANDARD_HA tier (§5) and `REDIS_6_X`, `REDIS_7_0` or `REDIS_7_2`, with or without in-transit
encryption (§6): no customer-managed keys, no maintenance windows and no IAM. Each is 501.
`REDIS_3_2`, `REDIS_4_0` and `REDIS_5_0` are 501 too: they are end of life, and the Docker official
`redis` image no longer builds them (3.2 last in October 2018, 4.0 in April 2020, 5.0 in November
2022), so there is no maintained image to run. Export and import to and from Cloud Storage RDB
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
(`GetInstanceAuthString` is FAILED_PRECONDITION), and it cannot be updated or deleted:
`UpdateInstance` and `DeleteInstance` on it are FAILED_PRECONDITION. Run `up` without
`memorystore` in `--services` to have none.

**Instances you create** are each a Valkey server of their own: a Deployment and Service in the
cluster, of the same image and settings, reached from the host through a tunnel of their own on
an OS-assigned port, which the instance's `host` and `port` report.

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

**Modes.** A created instance follows the instance's mode, as the server `up` runs does. In
persistent mode its record is kept in `<state-dir>/<name>/memorystore`, its server keeps an
append-only file on a volume of its own, and after `stop` and `up` it is READY again (REPAIRING
while its server starts), with its AUTH string and its keys. In ephemeral mode nothing is kept:
`up` removes the servers a previous run left, keeping their volumes as the server `up` runs keeps
its own, so a later persistent run has them back.

The console's Memorystore page lists the instances and has **Create instance**, **Edit** and
**Delete**, **Export** and **Import** on a created instance's page, and **Flush** (FLUSHALL) on
the instance `up` runs; the API itself has no flush. `ExportInstance` writes a `BGSAVE` snapshot
to a `gs://` object; `ImportInstance` replaces the instance's data with an RDB file's, loaded by
the server itself (`--services memorystore,storage`). `persistence_config` RDB sets the server's
save point to the snapshot period, in place of the append-only file. `UpgradeInstance` moves an instance to a later version (REDIS_6_X is Redis 6.2, REDIS_7_0 Redis
7.0, REDIS_7_2 Valkey 8.1) with its data, and **Upgrade** on its page does too (#1386).
`FailoverInstance` promotes a Standard tier instance's replica (§5); `RescheduleMaintenance` is
FAILED_PRECONDITION, since no maintenance is ever scheduled. See
[compatibility.md](compatibility.md#memorystore-for-redis--googlecloudredisv1).

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
redis-cli -h "$HOST" -p "$PORT" --tls --cacert server-ca.pem PING
```

Terraform's `transit_encryption_mode = "SERVER_AUTHENTICATION"` does the same, and
`server_ca_certs[0].cert` is the CA. The certificate names the host's loopback (`127.0.0.1`, where
`host` and `port` are) and the instance's Services in the cluster. A Standard tier instance's nodes
replicate over TLS. Clients are not asked for a certificate, as SERVER_AUTHENTICATION has none;
the mode is set at create and cannot be changed; no CA rotation is served. The CA and its key are
kept with the instance's record in persistent mode; the server's certificate and key are in its
Deployment's environment in the cluster, as the AUTH string is in its arguments.

The console's Create instance has **Enable in-transit encryption**, and the instance's page shows
**In-transit encryption** and a **Server CA** tab with the CA's PEM.
