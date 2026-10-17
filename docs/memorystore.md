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
encryption (§6) and with or without a customer-managed key (§7): no maintenance windows and no IAM.
Each is 501.
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
rotation is). `RescheduleMaintenance` has nothing to do with it.

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
Each node serves clients on one port that is the same in the cluster and on the host, and
announces the host's address to clients (`cluster-announce-client-ipv4`); the nodes gossip on
their pod addresses (bus port 16379). A client inside the Kubernetes cluster therefore gets host
addresses from `CLUSTER SLOTS`, which it cannot reach: in the cluster, reach the nodes by pod
address. Valkey announces one client address per address family (`cluster-announce-client-ipv4`,
`-ipv6`), and both the host's tunnels and the pods reach a node over IPv4, so no setting
serves both (#1549); the discovery endpoint is the host's too.

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

**IAM auth:** with `AUTH_MODE_IAM_AUTH` a client sends an access token as its AUTH string. The
instance's IAM is not enforced (ADR-0006), so the nodes' default user has no password and takes
any AUTH string, a token CloudBurrow's credentials issued among them, without checking it.

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
`BackupCluster` (`gcloud redis clusters backup`) keeps an RDB snapshot of each shard's
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
  Valkey servers kept in step by `REPLICAOF`. The connections are `CONNECTION_TYPE_PRIMARY` (the
  primary) and `CONNECTION_TYPE_READER`, which here is the first replica's address, not a balancer
  over all of them. There is no automatic failover (§5's is the Redis instances').
- Connections come back in the form they were asked in: `endpoints`, or the deprecated
  `psc_auto_connections`; the Terraform provider reads each back as what it was configured with.

`UpdateInstance` changes `shard_count` (slots and keys move), `replica_count`, `node_type` (1.4,
6.5, 13 or 58 GB of `maxmemory`), `engine_configs` and `persistence_config`, set on every running
node, and keeps `labels`, `deletion_protection_enabled`, `endpoints`, zone distribution and
maintenance policy. `persistence_config` RDB is `save <period> 1`, AOF `appendonly yes` with its
`appendfsync`; after a restart of CloudBurrow an instance comes back with its data when it has
RDB or AOF persistence, and empty without, as a Redis cluster does (§8, Across a restart). The
console's **Memorystore for Valkey** page lists, creates (either mode), edits and deletes
instances.

**Engine versions** (#1557): `VALKEY_7_2` runs Valkey 7.2.14 (`valkey/valkey:7.2-alpine`),
`VALKEY_8_0` (the default) Valkey 8.1 and `VALKEY_9_0` Valkey 9.0.6 (`valkey/valkey:9.0-alpine`),
each pinned by digest, for amd64 and arm64. Valkey 7.2 has no `cluster-announce-client-ipv4`, so
a 7.2 node in `CLUSTER` mode announces the host's address to clients as its hostname
(`cluster-preferred-endpoint-type hostname`). An `UpdateInstance` of `engine_version` to a later
version takes an RDB snapshot of each shard's primary, forms new nodes of the new image, loads the
snapshots, then removes the old nodes, which serve until then: writes made during the upgrade,
after the snapshot, are not carried over. A downgrade is INVALID_ARGUMENT.

**Cross-instance replication** (#1557), for `CLUSTER_DISABLED` instances without in-transit
encryption: an instance created with `cross_instance_replication_config` `instance_role`
`SECONDARY` and `primary_instance` (an `ACTIVE` `CLUSTER_DISABLED` instance of the same
`authorization_mode`) has its primary node replicate the primary instance's (`REPLICAOF`), its
replicas chained to it: it serves the primary's keys and refuses writes (READONLY). Each member
reports the `membership`. `UpdateInstance` of `cross_instance_replication_config` (alone)
switches over (a secondary to `PRIMARY`, every other member as its `secondary_instances`) and
detaches (a secondary to `NONE`; a primary to `NONE`, or to fewer `secondary_instances`). A
primary with secondaries is not deleted (FAILED_PRECONDITION). In `CLUSTER` mode it is 501, as a
Redis cluster's (§8).

**Migration** (`StartMigration`, `FinishMigration`, #1557) into a `CLUSTER_DISABLED` instance:
its primary replicates `self_managed_source`'s `ip_address` and `port` (state `MIGRATING`,
`migration_config.state` `REPLICATION_ESTABLISHED` once linked); `FinishMigration` waits until
its offset reaches the source's (unless `force`), stops replicating, and the instance is `ACTIVE`
and `MIGRATED`. A source that is a server of this CloudBurrow instance (a Memorystore for Redis
instance, a node of a Valkey instance or Redis cluster) is named by the host address its clients
use and reached at its in-cluster address; any other address is replicated from as it is.
`network_attachment` is checked and kept. A source with AUTH (`self_managed_source` has no
credentials), a `CLUSTER` mode target and in-transit encryption are not served (501 with the
reason).

**gcloud, Terraform and the console** (#1557). gcloud 587.0.0's `memorystore instances create
--kms-key`, `--engine-version`, `--cross-instance-replication-config-role=secondary
--primary-instance`, `instances update --engine-version` and
`--cross-instance-replication-config-role=none`, `start-migration` and `finish-migration` are
tested (`TestGcloudMemorystore1557`); `UpdateInstance` takes a message field's own paths in its
`update_mask` (`cross_instance_replication_config.instance_role`), as gcloud sends them.
`google_memorystore_instance`'s `kms_key`, `engine_version` (changed in place, the same `uid`) and
`cross_instance_replication_config` apply and plan empty (`TestTerraformMemorystore1557`); the
provider has no migration fields. The console's Create instance has **Engine version**,
**Persistence**, **Customer-managed encryption key** and **Secondary of (cross-instance
replication)**; Edit has **Engine version** (an upgrade); the instance's page shows its
Encryption, key and key state, its cross-instance replication role, primary, secondaries and
membership, and a migration's state and source; a secondary's page has **Switchover** and
**Detach**, a `CLUSTER_DISABLED` instance's **Start migration** (source IP and port) and, while
migrating, **Finish migration** (`TestConsoleValkeyReplicationMigrationVersions`,
`TestMemorystoreValkeySecondaryThroughThePage`).

Not served: node types whose size is not modelled here and server CA modes, each 501 with the
reason.

`TOKEN_AUTH`, backups (`BackupInstance`, `ListBackupCollections`, `GetBackupCollection`,
`ListBackups`, `GetBackup`, `DeleteBackup`, `ExportBackup`, `automated_backup_config`) and
restore through `managed_backup_source` or `gcs_source` are a Redis cluster's (§8), for an
instance in either mode: a `CLUSTER_DISABLED` instance's backup is its primary's file, and a
restore MIGRATEs the keys to its primary. Memorystore for Valkey has no ACL policies.

In-transit encryption (`SERVER_AUTHENTICATION`) and `IAM_AUTH` are a Redis cluster's (§8): a CA of
the instance's own, which `GetCertificateAuthority` returns as `managedServerCa` (gRPC
`{instance}/certificateAuthority`, JSON `/memorystore/v1/{instance}/certificateAuthority`), every
node serving TLS only, a `CLUSTER_DISABLED` instance's replicas replicating over TLS; any AUTH
string is taken under `IAM_AUTH`. The console's Create has **Enable in-transit encryption** and
**IAM authentication**, and the instance's page a **Server CA** tab.
