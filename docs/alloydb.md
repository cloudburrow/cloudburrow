# AlloyDB for PostgreSQL

`--services alloydb` serves the AlloyDB Admin API (`alloydb.googleapis.com`) on `--port-alloydb`
(default `9063`), gRPC and JSON on one port, for v1, v1beta and v1alpha (#2191). Its clusters'
instances are PostgreSQL servers in the instance's cluster, and the AlloyDB connectors (the Go,
Java and Python connectors and the AlloyDB Auth Proxy) reach them through the connectors'
server-side proxy on port 5433. `env` exports:

| Variable | For |
|---|---|
| `CLOUDBURROW_ALLOYDB_ENDPOINT` | the client libraries' `WithEndpoint` (`http://` + it for REST), and the connectors' admin API endpoint |
| `CLOUDSDK_API_ENDPOINT_OVERRIDES_ALLOYDB` | `gcloud alloydb` |
| `CLOUDBURROW_ALLOYDB_CONNECTOR_ENDPOINT` | only when another instance holds port 5433: the address the connectors' listener took instead |
| `CLOUDBURROW_ALLOYDB_TLS_ENDPOINT` | the same API over TLS, for the Java and Python connectors, which open only TLS channels |
| `CLOUDBURROW_ALLOYDB_CA_FILE` | the certificate authority of the TLS endpoint and of the clusters' servers, PEM |

Terraform's `google_alloydb_*` resources reach it through `cloudburrow terraform`
(`alloydb_custom_endpoint`). The console has AlloyDB's Clusters and Backups pages under Databases:
a cluster's page creates its instances, read pools and secondary clusters, edits an instance
(machine, flags, Managed Connection Pooling), exports and imports, promotes and switches over a
secondary, and backs up and restores.

## The engine: PostgreSQL, or AlloyDB Omni

Google publishes AlloyDB Omni, AlloyDB's own engine, as the
[`google/alloydbomni`](https://hub.docker.com/r/google/alloydbomni) image. Its use is governed by
Google's [AlloyDB Omni License Terms](https://cloud.google.com/terms/alloydb-omni-marketplace-terms)
(free for development, evaluation and non-commercial use), which only you, the end user, may
accept. CloudBurrow never accepts them on anyone's behalf, in code, tests or CI. So an instance runs
one of two engines (#2207):

- **PostgreSQL** (the default, and what every cluster ran before #2207): the PostgreSQL server
  Cloud SQL for PostgreSQL uses here, of the cluster's major version (`POSTGRES_14` to
  `POSTGRES_17`; `POSTGRES_16` by default). The differences listed below follow.
- **AlloyDB Omni**, once you accept the terms: Google's image of the cluster's major version,
  pinned by digest in `dependencies.json` (15.18.0, 16.14.0 and 17.10.0, linux/amd64 and
  linux/arm64, arm64 in preview as Google marks it; Google publishes no Omni of PostgreSQL 14, and
  its PostgreSQL 18 images are behind a registration form, so a `POSTGRES_14` cluster runs
  PostgreSQL 14 whatever you accepted). The server keeps Omni's own `shared_preload_libraries`, so
  AlloyDB's engine features are there: the columnar engine, the `google_ml_integration` extension
  and model endpoint management, `alloydb_scann`, the query plan cache and the index advisor. Their
  flags (`google_columnar_engine.*`, `google_db_advisor.enabled`, `google_ml_integration.*`) take
  effect as Omni's settings. Its container's shared memory is sized for the columnar engine's store
  (`google_columnar_engine.memory_size_in_mb`, 1024 MB by default, plus 256 MB), and it asks for the
  2 GB of memory Google's requirements name. Calling remote models needs network access and
  credentials this instance does not give the server.

### Accepting the terms

Accept them yourself in one of these ways; each is recorded with the cluster as who accepted (the
user running CloudBurrow), when (UTC) and how, and `cloudburrow status` shows it (`status --format
json` as `alloydb_omni_terms`):

- for every new cluster of `POSTGRES_15`, `16` or `17`: `cloudburrow up --accept-alloydb-omni-terms`,
  `CLOUDBURROW_ACCEPT_ALLOYDB_OMNI_TERMS=Y` (exactly `Y`), or `"alloydb": {"acceptOmniTerms": true}`
  in a configuration file you name with `--config` or `CLOUDBURROW_CONFIG`. A `./cloudburrow.json`
  found in the working directory, which a repository can ship, cannot accept them: `up` refuses it;
- for one cluster: the console's Create cluster checkbox, "I accept the AlloyDB Omni licence terms",
  which links Google's terms; or the request header `X-CloudBurrow-Accept-AlloyDB-Omni-Terms: Y` on
  `CreateCluster` or `RestoreCluster` (gRPC metadata `x-cloudburrow-accept-alloydb-omni-terms`),
  which the checkbox sends.

A cluster's engine is chosen when it is created and kept: a cluster made before you accepted stays
on PostgreSQL, and one made under an acceptance keeps running Omni after an `up` without one. A
secondary cluster runs its primary's engine, under its primary's acceptance, as its servers
replicate the primary's. A backup or point-in-time restore makes a new cluster whose engine is
chosen as any new cluster's; a dump of an Omni cluster that uses Omni's extensions loads into a
PostgreSQL cluster only without them. The console shows each cluster's Database engine.

### What an explicit Omni request is

Google's AlloyDB API has no engine field: every Google cluster runs AlloyDB's engine, so nothing in a
request Google documents asks for Omni rather than PostgreSQL. CloudBurrow therefore takes as
explicit only its own request headers, on `CreateCluster` and `RestoreCluster`:

| Header (gRPC metadata in lower case) | Without an acceptance | With `up`'s acceptance |
|---|---|---|
| none | PostgreSQL | Omni (PostgreSQL for `POSTGRES_14`) |
| `X-CloudBurrow-AlloyDB-Engine: OMNI` | `FAILED_PRECONDITION`, saying how to accept; no cluster is made | Omni |
| `X-CloudBurrow-AlloyDB-Engine: POSTGRES` | PostgreSQL | PostgreSQL |
| `X-CloudBurrow-Accept-AlloyDB-Omni-Terms: Y` | Omni, accepted by this request | Omni, accepted by this request |

An explicit Omni request for `POSTGRES_14`, an engine other than `OMNI` or `POSTGRES`, an acceptance
other than `Y`, or both `Y` and `POSTGRES`, is `INVALID_ARGUMENT`. AlloyDB's engine flags are not
taken as a request for Omni: clusters and Terraform configurations made before #2207 carry them, and
on PostgreSQL they are still accepted and stored, as before. With the Go client, set a header with
`callctx.SetHeaders(ctx, "x-cloudburrow-alloydb-engine", "OMNI")`
(`github.com/googleapis/gax-go/v2/callctx`), which sends it over gRPC and REST; the name must be in
lower case there, as gRPC metadata keys are.

### Running the Omni test yourself

No test of CloudBurrow's, and no CI job, runs Omni, so the Omni engine is Implemented, not
Verified. `TestAlloyDBOmniEngine` runs it, and skips unless you opt in. Only if you accept Google's
AlloyDB Omni License Terms, run, from a CloudBurrow checkout (it pulls the Omni images of 16 and 17,
about 1 GB each):

```sh
make build
export CLOUDBURROW_ACCEPT_ALLOYDB_OMNI_TERMS=Y
bin/cloudburrow up --detach --name omni --services alloydb --accept-alloydb-omni-terms
eval "$(scripts/compat-env.sh --name omni --services alloydb)"
go test -tags compat -count=1 -timeout 40m -run '^TestAlloyDBOmniEngine$' ./test/compat/
bin/cloudburrow stop --name omni && bin/cloudburrow delete --name omni
```

It creates a `POSTGRES_16` cluster whose primary runs with the columnar engine, index advisor and
model support flags on, checks the server is Omni 16 with `google_columnar_engine`,
`google_ml_integration` and `alloydb_scann` (and creates them), that each flag took effect, and
that a table goes into the column store; then a `POSTGRES_17` cluster asked for by
`X-CloudBurrow-AlloyDB-Engine: OMNI`, which runs Omni 17, and a `POSTGRES_14` one, which runs
PostgreSQL. Without the opt-in, `TestAlloyDBOmniRefusedWithoutAcceptance` runs instead.

### Differences on the PostgreSQL engine

- AlloyDB's own engine features are absent: the columnar engine, the `google_ml_integration`
  extension and model endpoint management, `alloydb_scann`, adaptive autovacuum, the AlloyDB
  query plan cache and index advisor. Their flags (`google_columnar_engine.*`, `alloydb.*`,
  `google_db_advisor.enabled`, `google_ml_integration.*`) are accepted, validated and stored, and
  PostgreSQL keeps them as placeholder settings with no effect.

### On both engines

- `alloydbsuperuser` is an ordinary role, made at a primary's creation, which a cluster's users
  can be granted; as on Google, it is not PostgreSQL's superuser.
- The server trusts connections from inside the instance, as Cloud SQL's servers here do, so a
  built-in user's password is set but a wrong one is not refused.
- An instance's machine (vCPUs, memory) is recorded; the server takes what the machine gives it.

## Instances and read pools

A cluster's primary instance is one PostgreSQL server; `REGIONAL` availability is recorded (with
a standby node in `nodes`), and failover swaps the nodes' names and restarts the server. A read
pool is one streaming replica of the primary whatever its `node_count`, which is recorded; it
takes the primary's `max_connections`, `max_worker_processes`, `max_wal_senders`,
`max_prepared_transactions` and `max_locks_per_transaction` where those are higher than its own,
as a hot standby must. `STOP`/`ALWAYS` activation stops and starts the server.

## Managed connection pooling

`connection_pool_config.enabled` on a primary or read pool instance runs a pooler beside its
server: PgBouncer (`edoburu/pgbouncer`, ISC licence), as Cloud SQL's managed pooling does here
(#2141). Clients reach it on port 6432 at the instance's address; the connectors and the Auth
Proxy are pooled too, in a pool of their own. Google's pooling flags (`pool_mode`,
`max_pool_size`, `min_pool_size`, `max_client_connections`, `client_connection_idle_timeout`,
`server_connection_idle_timeout`, `query_wait_timeout`, `max_prepared_statements`,
`ignore_startup_parameters`, `server_lifetime`, `stats_users`) are checked against Google's
ranges and become PgBouncer's settings; `pooler_count` is the machine's vCPUs / 4, at least one.
Changing the configuration restarts the instance. A secondary instance is not pooled.

## Connecting

`GetConnectionInfo` gives every instance this host's address (or none, with its PSC DNS name, in a
Private Service Connect cluster), and `GenerateClientCertificate` signs the connector's key with
one certificate authority per instance, kept in `alloydb-ca.json` in a persistent instance. The
proxy on port 5433 speaks the connectors' protocol: TLS 1.3 with the client certificate, then, when
the certificate was asked for with `use_metadata_exchange`, the length-prefixed
`MetadataExchangeRequest` and its response. `AUTO_IAM` (IAM database authentication) checks the
OAuth 2.0 token against those this instance minted, and needs an IAM user of the cluster for its
principal.

On Google each instance has an IP address of its own, where it serves PostgreSQL on 5432 (and its
pooler on 6432), and the connectors reach it on 5433; they send no TLS server name for an address,
so the address dialled is what tells the instances apart. Here each instance of a
private-services-access cluster takes a loopback address of its own, from 127.0.0.2 onwards, and
the server listens there on 5433, 5432 and 6432; deleting the instance frees it. On Linux every
address of 127.0.0.0/8 is the host's. On macOS only 127.0.0.1 is, until an administrator aliases
more onto `lo0` (`sudo ifconfig lo0 alias 127.0.0.2 up`, one per address); set
`CLOUDBURROW_ALLOYDB_INSTANCE_ADDRESSES` (comma-separated) to choose the addresses instances take,
in order. An instance that finds no free address shares the connectors' address, 127.0.0.1, where
a connection reaches the one instance of the client certificate's cluster with no address of its
own, else the cluster's primary; a read pool is then reached by its PSC DNS name (`WithPSC()`),
under `alloydb-psc.localhost`, which resolves to this host: give the cluster
`psc_config.psc_enabled`.

A direct connection on 5432 or 6432 negotiates TLS as PostgreSQL does (an `SSLRequest`, or
PostgreSQL 17's direct TLS) with a certificate the cluster's CA signs for the address. A plaintext
one is refused unless `ssl_config.ssl_mode` is `ALLOW_UNENCRYPTED_AND_ENCRYPTED`, and every one is
refused when `client_connection_config.require_connectors` is set. Any instance answers
`ExecuteSql`, and the console shows each server's local PostgreSQL address.

Google's connectors work unchanged, each pointed at this instance's admin API;
`TestAlloyDBConnectorsSendRealSQL` runs each, pinned, in a container with no route out, writing
and reading rows, and counts every attempt to leave:

- The Go connector (`cloud.google.com/go/alloydbconn`): `WithAdminAPIEndpoint("http://" +
  CLOUDBURROW_ALLOYDB_ENDPOINT)`, a token source, `WithOptOutOfBuiltInTelemetry()`, and
  `WithHTTPClient(&http.Client{})`. The opt-out alone still leaves `NewDialer` opening a Cloud
  Monitoring gRPC client, which connects to `monitoring.googleapis.com`; an HTTP client makes that
  client fail to start, so nothing leaves the machine.
- The Python connector (`google-cloud-alloydb-connector`, 1.15.0):
  `Connector(credentials=..., alloydb_api_endpoint=CLOUDBURROW_ALLOYDB_TLS_ENDPOINT)` with
  `GRPC_DEFAULT_SSL_ROOTS_FILE_PATH` naming `CLOUDBURROW_ALLOYDB_CA_FILE`. Its credentials need an
  expiry (it logs it).
- The Java connector (`com.google.cloud:alloydb-jdbc-connector`, 1.4.1): a `ConnectorConfig` with
  `withAdminServiceEndpoint(CLOUDBURROW_ALLOYDB_TLS_ENDPOINT)` and credentials, registered by name
  (`alloydbNamedConnector`), and a JVM trust store holding the CA.
- The AlloyDB Auth Proxy (v1.18.0): `--alloydbadmin-api-endpoint http://` +
  `CLOUDBURROW_ALLOYDB_ENDPOINT`, `--token` and `--disable-built-in-telemetry`. Its connector
  still opens the Cloud Monitoring client above, which tries `monitoring.googleapis.com` once, and
  the proxy has no option that stops it: alloydbconn v1.20.0's `NewDialer` creates that client
  whatever its options, and v1.18.0's `--disable-metrics` and `--disable-traces` apply only with
  `--telemetry-project` (which is not passed, so the proxy's own Cloud Monitoring and Cloud Trace
  exporters stay off; re-read 2026-11-24 in the v1.18.0 `cmd/root.go`). Run it where nothing
  leaves the machine, as the tests do, in a container on an `--internal` Docker network.

## Export and import

`ExportCluster` writes one Cloud Storage object of this instance (`--services storage` with
`alloydb`) from a database: SQL, a `pg_dump` with `tables`, `schema_only`,
`clean_target_objects` and `if_exist_target_objects`; or CSV, the rows of `select_query`, with
the delimiter, quote and escape characters as hex codes (`2C`, or `0x2C`). `ImportCluster` reads
one: SQL through `psql` (a `\connect` in the file moves to another database; the database is
optional) or CSV into `table` and its `columns`, as `user` when one is given, answering
`bytes_downloaded`. An object whose name ends in `.gz` is compressed or decompressed. Both run on
the cluster's primary instance.

## Cross-region replication

`CreateSecondaryCluster` makes a secondary of a primary cluster (at most five, in any region; the
primary's backup policies are copied, and it takes no initial user), and
`CreateSecondaryInstance` its one secondary instance: a streaming replica of the primary's
server, which refuses writes, with the primary's users. A secondary cluster takes read pools. The
primary cluster is not deleted while it has secondaries, nor the secondary instance on its own.
`PromoteCluster` makes a secondary a standalone primary cluster, its server promoted.
`SwitchoverCluster` swaps the roles: the secondary's server is promoted, the old primary's is
rebuilt as its replica, its read pools follow, and the other secondaries replicate the new
primary.


## Backups and recovery

An on-demand or automated backup is a dump of the primary, kept with the cluster's users in the
instance's state directory; restoring it makes a new cluster whose primary, once created, holds
the data and the users. Continuous backup keeps the primary's write-ahead log from a base taken at
the primary's creation, so a point-in-time restore recovers to any moment from then on within the
recovery window. Automated backups follow `automated_backup_policy.weekly_schedule` (each minute,
`up` takes the ones due) and its time- or quantity-based retention.

## Not served

Backup and DR protection of AlloyDB clusters is #1524. [gaps.md](gaps.md) lists what is open;
[compatibility.md](compatibility.md) has the per-method evidence.
