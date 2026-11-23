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

Terraform's `google_alloydb_*` resources reach it through `cloudburrow terraform`
(`alloydb_custom_endpoint`). The console has AlloyDB's Clusters and Backups pages under Databases.

## The engine: PostgreSQL, not AlloyDB Omni

Google publishes AlloyDB Omni as the `google/alloydbomni` image (15, 16 and 17; amd64, and arm64
in preview). Its use is governed by Google's AlloyDB Omni licence terms, which an end user accepts;
CloudBurrow never accepts a licence on anyone's behalf, in code, tests or CI. So each instance is
the PostgreSQL server Cloud SQL for PostgreSQL uses here, of the cluster's major version
(`POSTGRES_14` to `POSTGRES_17`; `POSTGRES_16` by default). The differences that follow:

- AlloyDB's own engine features are absent: the columnar engine, the `google_ml_integration`
  extension and model endpoint management, `alloydb_scann`, adaptive autovacuum, the AlloyDB
  query plan cache and index advisor. Their flags (`google_columnar_engine.*`, `alloydb.*`,
  `google_db_advisor.enabled`, `google_ml_integration.*`) are accepted, validated and stored, and
  PostgreSQL keeps them as placeholder settings with no effect.
- `alloydbsuperuser` is an ordinary role, made at a primary's creation, which a cluster's users
  can be granted; as on Google, it is not PostgreSQL's superuser.
- PostgreSQL trusts connections from inside the instance, as Cloud SQL's servers here do, so a
  built-in user's password is set but a wrong one is not refused.
- An instance's machine (vCPUs, memory) is recorded; the server takes what the machine gives it.

## Instances and read pools

A cluster's primary instance is one PostgreSQL server; `REGIONAL` availability is recorded (with
a standby node in `nodes`), and failover swaps the nodes' names and restarts the server. A read
pool is one streaming replica of the primary whatever its `node_count`, which is recorded; it
takes the primary's `max_connections`, `max_worker_processes`, `max_wal_senders`,
`max_prepared_transactions` and `max_locks_per_transaction` where those are higher than its own,
as a hot standby must. `STOP`/`ALWAYS` activation stops and starts the server.

## Connecting

`GetConnectionInfo` gives every instance this host's address (or none, with its PSC DNS name, in a
Private Service Connect cluster), and `GenerateClientCertificate` signs the connector's key with
one certificate authority per instance, kept in `alloydb-ca.json` in a persistent instance. The
proxy on port 5433 speaks the connectors' protocol: TLS 1.3 with the client certificate, then, when
the certificate was asked for with `use_metadata_exchange`, the length-prefixed
`MetadataExchangeRequest` and its response. `AUTO_IAM` (IAM database authentication) checks the
OAuth 2.0 token against those this instance minted, and needs an IAM user of the cluster for its
principal.

Every instance of a private-services-access cluster has the same address here, and the connectors
send no server name for an address, so the proxy takes such a connection to the cluster's primary.
A read pool is reached through the connectors by its PSC DNS name (`WithPSC()`), whose name is
under `alloydb-psc.localhost` and so resolves to this host: give the cluster
`psc_config.psc_enabled`. Any instance answers `ExecuteSql`, and the console shows each server's
local PostgreSQL address.

## Backups and recovery

An on-demand or automated backup is a dump of the primary, kept with the cluster's users in the
instance's state directory; restoring it makes a new cluster whose primary, once created, holds
the data and the users. Continuous backup keeps the primary's write-ahead log from a base taken at
the primary's creation, so a point-in-time restore recovers to any moment from then on within the
recovery window. Automated backups follow `automated_backup_policy.weekly_schedule` (each minute,
`up` takes the ones due) and its time- or quantity-based retention.

## Not served

`ExportCluster`, `ImportCluster`, `CreateSecondaryCluster`, `CreateSecondaryInstance`,
`PromoteCluster` and `SwitchoverCluster` answer `UNIMPLEMENTED`, as does managed connection
pooling. Backup and DR protection of AlloyDB clusters is #1524. [gaps.md](gaps.md) lists what is
open; [compatibility.md](compatibility.md) has the per-method evidence.
