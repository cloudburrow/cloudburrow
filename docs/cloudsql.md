# Cloud SQL

Issue: [#121](https://github.com/cloudburrow/cloudburrow/issues/121) · Added 2026-09-22

**A real PostgreSQL running in CloudBurrow's cluster, and the parts of the Cloud SQL Admin API
that can be served faithfully over it (#1301).**

That distinction is the whole of this page, and it is repeated on the screen itself, because
everything else here emulates a Google API and this does not.

---

## 1. Why this is different from every other backend

Google publishes no Cloud SQL emulator. Checked against the installed gcloud rather than
assumed:

```
$ gcloud emulators --help                 firestore, spanner
$ gcloud components list | grep -i sql    Cloud SQL Proxy v2 — cloud-sql-proxy
```

The only Cloud SQL tool Google ships is the **Auth Proxy**, whose own README describes it as
connecting to *"the private IP of the Cloud SQL instance"* — a real instance in GCP. That is
precisely what CloudBurrow must never require.

So there is nothing of Google's to reuse, and the reuse-first rule in
[ADR-0005](adr/0005-kubernetes-foundation-and-upstream-reuse.md) has nothing to point at. What
runs instead is **the database Cloud SQL runs underneath**: PostgreSQL, in the cluster, at a
stable local address.

It is the only backend here that is not a Google-published component, and
`dependencies.json` records that.

## 2. What you get

| | |
|---|---|
| Server | PostgreSQL **17.11**, `postgres:17-alpine`, digest-pinned |
| Address | `127.0.0.1:9019` on the host (`--port-cloudsql`, moved by `--port-base`), plus `cloudsql.cloudburrow.svc.cluster.local:5432` in the cluster |
| User / database | `cloudburrow` / `cloudburrow` |
| Authentication | **trust** — see below |
| Durability | a PersistentVolumeClaim in `--mode persistent`; the only opt-in backend that keeps its data |

```sh
cloudburrow up --services cloudsql
eval "$(cloudburrow env)"      # PGHOST, PGPORT, PGUSER, PGDATABASE, PGSSLMODE, CLOUDBURROW_CLOUDSQL_URL
psql -c 'SELECT 1'             # every setting from the PG* variables
```

The port is fixed (#584), as MySQL's is, so it is the same after every restart and
`cloudburrow env` can export it without asking the running instance: `PGHOST`, `PGPORT`,
`PGUSER` and `PGDATABASE`, read by libpq, `psql`, pgx, lib/pq, psycopg and node-postgres, plus
`PGSSLMODE=disable` (the server has no TLS) and the same settings as one connection string in
`CLOUDBURROW_CLOUDSQL_URL`. No `PGPASSWORD` is exported because there is no password; see
[below](#4-authentication-is-trust-on-purpose). With `--port-cloudsql 0` the port is
OS-assigned again: `up` prints it and `env` reads it from the running instance, while
`env --offline` leaves the variables out and says so on stderr.

**State snapshots.** `cloudburrow state save` captures every application database with
`pg_dump`, and `state load` replaces them with `pg_restore`. Both run inside the server's pod,
so the host needs no PostgreSQL client (#311). A load drops each database that exists now,
including ones made since the save, and ends the sessions connected to it. See
[compatibility.md](compatibility.md#state-snapshots).

An ordinary driver connects and works. Verified with a plain `psql` client, nothing of
CloudBurrow's involved:

```
CREATE TABLE
INSERT 0 2
 widgets
---------
       2
 PostgreSQL 17.11 on aarch64-unknown-linux-musl
```

## 3. The Cloud SQL Admin API, and what you do not get

**Most of the Cloud SQL Admin API.** Since #1301 CloudBurrow serves `sqladmin` v1 (under
`/v1/`) and v1beta4 (under `/sql/v1beta4/`) on the `sqladmin` port, `9020`
(`--port-sqladmin`), over the servers it runs. `cloudburrow env` exports it as
`CLOUDBURROW_SQLADMIN_ENDPOINT`, and `CLOUDSDK_API_ENDPOINT_OVERRIDES_SQL` for gcloud. What is
served:

- `instances.list` and `instances.get`: one instance per running server, `postgres`
  (`POSTGRES_17`) and `mysql` (`MYSQL_8_4`), in every project, `RUNNABLE`, with a connection
  name `{project}:us-central1:{instance}` and a `PRIMARY` address of `127.0.0.1`. The port is
  `PGPORT` or `MYSQL_PORT`, not 5432 or 3306.
- `databases.*` and `users.*`: `CREATE`, `ALTER` and `DROP` run in the server, and what is
  listed is the server's own catalogue. On PostgreSQL a user's password is stored but not
  checked (see below); a database's encoding cannot be changed after it is created.
- `operations.get` and `operations.list`: every method finishes before it answers, so its
  operation is `DONE`. Errors are the HTTP answer, not an operation that carries them.

```sh
eval "$(cloudburrow env)"
curl -s http://$CLOUDBURROW_SQLADMIN_ENDPOINT/v1/projects/$GOOGLE_CLOUD_PROJECT/instances
```

```go
svc, _ := sqladmin.NewService(ctx,
    option.WithEndpoint("http://"+os.Getenv("CLOUDBURROW_SQLADMIN_ENDPOINT")+"/"),
    option.WithoutAuthentication())
_, err := svc.Databases.Insert(project, "postgres", &sqladmin.Database{Name: "orders"}).Do()
```

What answers 501, saying why: IAM database authentication (CloudBurrow has no identities, ADR-0004), `pointInTimeRestore` and `restoreBackup` with `backupdrBackup` unless the instance runs `--services backupdr` ([backupdr.md](backupdr.md), #1433), a physical
(`PHYSICAL`) external sync (see [External primaries](#external-primaries-1501-1524)), `MYSQL_5_6`, `MYSQL_5_7`
and SQL Server versions (their images are linux/amd64 only), BAK and TDE imports and exports (SQL Server's files),
upgrading a PostgreSQL read replica on its own (streaming replication does not cross major versions), and high
availability, read replicas and point-in-time recovery on PostgreSQL 9.6 to 11 (#1462). A legacy failover replica
(`failoverReplica`, `failoverTarget`) is 400, as Cloud SQL refuses one since 2025, and so is changing a PostgreSQL
database's charset or collation, which Cloud SQL documents is not set through the API. See
[coverage/sqladmin.md](coverage/sqladmin.md) for every method, and the next section for the
methods #1462 refuses and why.

### The Data API, upgrade pre-checks and the rest of the instance verbs (#1462)

- **`instances.executeSql`** runs statements in the instance's own server, as the payload's
  `user`, connecting with the password held by `passwordSecretVersion` in this instance's
  Secret Manager (start CloudBurrow with `secretmanager` in `--services`; without it the call is
  412 naming the service). As on Cloud SQL the instance needs `settings.dataApiAccess`
  `ALLOW_DATA_API`; the servers `up` runs cannot have it, so create an instance for it. Each
  statement gives one result: its columns, typed by the server, and its rows, a NULL as
  `nullValue`; a statement that returns no rows gives its command tag as `message`. More rows
  than `rowLimit` is 400 unless `partialResultMode` is `ALLOW_PARTIAL_RESULT`, which returns the
  first `rowLimit` rows marked `partialResult`. A statement the database refuses ends the run,
  and its error is the response's `status` (code 3, or 16 when the server refuses the user's
  password); the results before it are returned. `autoIamAuthn` is 501 (ADR-0004). PostgreSQL
  trusts local connections, so there the password is read but not checked, as for every
  connection (section 4). `gcloud sql instances execute-sql --user --password-secret-version`
  works.
- **`instances.preCheckMajorVersionUpgrade`** checks the upgrade `instances.patch` would make
  (section "Database versions and major version upgrades"): the operation's
  `preCheckMajorVersionUpgradeContext.preCheckResponse` has an `ERROR` naming what would refuse
  it, such as read replicas not yet upgraded, and nothing when the upgrade would go ahead. A
  target that is not a newer version of the engine is 400. It does not run `pg_upgrade --check`.
- **`instances.truncateLog`** truncates MySQL's `mysql.general_log` (`MYSQL_GENERAL_TABLE`) or
  `mysql.slow_log` (`MYSQL_SLOW_TABLE`); on PostgreSQL it is 400, as on Cloud SQL.

Refused with 501, each naming what is missing:

| Methods | Why |
|---|---|
| `blueGreenDeployments.*` | the switchover moves the blue instance's write endpoint (IP address and DNS name) to the green one: Google-managed networking |
| `connect.resolve` | needs the instance's Cloud DNS records (Private Service Connect and write endpoint DNS names) |
| `listServerCertificates`, `addServerCertificate`, `rotateServerCertificate` | per-instance server certificates are for `GOOGLE_MANAGED_CAS_CA` and `CUSTOMER_MANAGED_CAS_CA` instances, issued by Certificate Authority Service; instances here are `GOOGLE_MANAGED_INTERNAL_CA`, rotated with `addServerCa` and `rotateServerCa` |
| Entra ID certificates, `acquireSsrsLease`, `releaseSsrsLease` | SQL Server features; CloudBurrow runs PostgreSQL and MySQL |
| `demote`, `demoteMaster` | they make a Cloud SQL instance a replica of an external primary, to migrate back out of Cloud SQL; the other direction, `startExternalSync`, is served (#1501) |
| `instances.switchover` | swaps the primary with a cross-region disaster recovery replica of an Enterprise Plus replication cluster; CloudBurrow has one region |
| `reencrypt` | the server's volume is not encrypted with a customer-managed Cloud KMS key |
| `getDiskShrinkConfig`, `performDiskShrink`, `resetReplicaSize` | storage is the server's volume in the cluster, not a provisioned persistent disk |
| `rescheduleMaintenance` | no Google-managed maintenance is ever scheduled |

### Imports and exports (#1380)

`instances.export` and `instances.import`, and so `gcloud sql export sql|csv` and
`gcloud sql import sql|csv`, write to and read from a `gs://` file in the instance's own
Cloud Storage, so they need `--services storage` beside `cloudsql` or `cloudsql-mysql`;
without it they answer 501. The bucket must exist, and an export to a file that exists
fails its operation, as on Google.

```sh
gcloud sql export sql postgres gs://my-bucket/dump.sql.gz --database=cloudburrow
gcloud sql import sql postgres gs://my-bucket/dump.sql.gz --database=other
gcloud sql export csv mysql gs://my-bucket/rows.csv --database=cloudburrow --query='SELECT * FROM t'
gcloud sql import csv mysql gs://my-bucket/rows.csv --database=cloudburrow --table=t
```

The server's own tools do the work, in its pod: `pg_dump` and `psql`, or `mysqldump` and
`mysql`, for SQL files (gzip when the name ends in `.gz`); `COPY` for PostgreSQL's CSV;
`SELECT ... INTO OUTFILE` and `LOAD DATA INFILE`, through the server's
`secure_file_priv` directory, for MySQL's, with Cloud SQL's default quoting and the
`csvExportOptions`/`csvImportOptions` characters (hex codes, such as `2C`). A PostgreSQL SQL
export names exactly one database; a MySQL one without databases exports every application
database. An import or export runs in the background, and `operations.cancel` stops it while
it runs; a cancelled MySQL import can leave the rows it
already loaded, as Google warns. BAK and TDE files answer 501: they are SQL Server's, read and
written only by a SQL Server server, and SQL Server's image is linux/amd64 only (#1462). The console's instance page
has Export and Import, with the parallel and `masterData` options below.

**Parallel imports and exports (#1421).** `sqlExportOptions.parallel` (with `threads`, 4 when
none is given) writes the directory Cloud SQL documents under the `uri`, which names a folder
and must be empty: `pg_dump -Fd -j <threads>` of the one database named for PostgreSQL
(`toc.dat` and a file per table), MySQL Shell's `util.dumpSchemas` (or `util.dumpTables` with
`tables`) for MySQL (`@.json`, `@.done.json`, per-table `.tsv.zst` chunks). The dump is made in
the server's container and each of its files written as an object under the folder.
`sqlImportOptions.parallel` reads every object under the folder back and restores it:
`pg_restore -j <threads> --no-owner --no-acl`, with `postgresImportOptions.clean` and
`ifExists` as `--clean` and `--if-exists`; `util.loadDump` for MySQL, with `local_infile` on
while it loads and set back after. `threads` without `parallel`, and `clean`/`ifExists`
outside a PostgreSQL parallel import, are 400. `gcloud sql export sql --parallel --threads`
and `gcloud sql import sql --parallel` work.

**`mysqlExportOptions.masterData` (#1421)** is `mysqldump --source-data` (MySQL 8.4's name for
`--master-data`): 1 writes the binary log coordinates as a `CHANGE REPLICATION SOURCE TO`
statement, with `--set-gtid-purged` on where the server has GTIDs (every Cloud SQL server
does; `AUTO` writes none on one without), and 2 writes it as a comment. Any other value, with
`parallel`, or on PostgreSQL, is 400; on a server without binary logging the export fails with
mysqldump's words.

### Client certificates, tiers and flags (#1380)

`sslCerts.insert` (`gcloud sql ssl client-certs create`) signs a client certificate with the
instance's server CA, the one the connectors trust, and returns its private key once;
`list`, `get`, `delete` and `instances.resetSslConfig` (which deletes them all and reissues
the server certificate) are served, and the console's instance page has a Client
certificates tab. `addServerCa` and `rotateServerCa` rotate the server CA the connector path
signs with (connectors pick it up from `connectSettings` on their next dial), and
`listServerCas` lists the active, upcoming and rotated-out CAs; the console shows them, with
Add server CA and Rotate server CA. The servers are reached through the connector path, so the certificates
are records the servers do not check on a direct connection. `tiers.list` and `flags.list`
answer the lists Google documents for each version served (`POSTGRES_13` to `POSTGRES_18`,
`MYSQL_8_0`, `MYSQL_8_4`, #1381): a flag lists every served version it applies to, from the
version notes in Google's flag tables.

### Creating instances (#1330)

`instances.insert`, `patch`, `update`, `delete` and `restart` are served, so
`gcloud sql instances create` and Terraform's `google_sql_database_instance` work. Each
created instance is **a server of its own**: a Deployment in the cluster named
`sqlinst-<name>`, from the same image and the same component code as the server `up` runs
for `--services cloudsql` or `cloudsql-mysql` on the image of its `databaseVersion`, with its
own host tunnel, its own `connectionName` (`{project}:{region}:{name}`), its own server CA,
and an address the connectors reach through 3307.

| Field | What happens |
|---|---|
| `databaseVersion` | `POSTGRES_9_6` to `POSTGRES_18`, `MYSQL_8_0` or `MYSQL_8_4` (#1381, #1462): see [Database versions](#database-versions-and-major-version-upgrades-1381). The engine's service must be enabled (412 otherwise). `MYSQL_5_6`, `MYSQL_5_7`, `MYSQL_8_0_*` minor pins, `SQLSERVER_*` and any other `POSTGRES_*` or `MYSQL_*` are 501 saying why; another value is 400. |
| `region` | Recorded and answered, and in the connection name (`us-central1` when left out). Every server runs in the one cluster. |
| `rootPassword` | MySQL's `root` password (empty leaves `root` with none, as Cloud SQL does). PostgreSQL's user is `postgres`, trusted locally like the server `up` runs. |
| `settings.tier` | Required, recorded and answered. The server is not sized by it. |
| `settings.databaseFlags` | Passed to the server: `-c name=value` to `postgres`, `--name=value` to `mysqld`. A patch that changes them starts the server again with them; a flag the server rejects fails the operation with its words. `cloudsql.*` flags (IAM authentication, Cloud SQL's extensions) are 501. |
| `settings.userLabels`, `deletionProtectionEnabled`, other settings | Recorded and answered. `delete` of a protected instance is 400, as on Cloud SQL. |
| `masterInstanceName` | A read replica: see [Read replicas](#read-replicas-1330). |
| `settings.availabilityType` | `ZONAL` (the default) or `REGIONAL`, a high-availability instance with a synchronous standby: see [High availability](#high-availability-1408). Another value is 400. |
| `replicaConfiguration.failoverTarget`, `failoverReplica` | 400, as Cloud SQL answers (#1462): the legacy high-availability configuration, deprecated for every instance on 2025-01-13, with which "you can't create instances" ([configure-legacy-ha](https://docs.cloud.google.com/sql/docs/mysql/configure-legacy-ha)). `REGIONAL` is high availability. |

The operation `insert` returns is `RUNNING` until the server serves through its tunnel, then
`DONE` (or `DONE` with the server's error, and the instance `FAILED`). `delete` removes the
server, its Service and its volume. `restart` restarts its pod.

### Read replicas (#1330)

`instances.insert` with `masterInstanceName` (and `instanceType: READ_REPLICA_INSTANCE`, which
may be left out) creates a replica: a server of its own, as above, replicating from its primary
by the engine's own replication. `databaseVersion` and `tier` default to the primary's.

- **PostgreSQL:** the primary gets a replication role, `cloudsqlreplica`, and a `replication`
  line for it in `pg_hba.conf`; the replica's data directory is a `pg_basebackup -R -X stream`
  of the primary, taken once, so it starts as a hot standby streaming from it. A write on it is
  refused by the server (`cannot execute INSERT in a read-only transaction`).
- **MySQL:** the primary's GTIDs are turned on by MySQL's online procedure (`SET PERSIST
  gtid_mode`, as every Cloud SQL for MySQL instance has them) and it gets a `cloudsqlreplica`
  user; the replica, with its own `server_id`, loads the primary's databases with
  `mysqldump --set-gtid-purged=ON` and replicates with `SOURCE_AUTO_POSITION`, with
  `super_read_only` on, so a write is refused by the server. Its `root` is the primary's.

The replica answers `instanceType: READ_REPLICA_INSTANCE`, `masterInstanceName`
(`{project}:{primary}`), `replicaConfiguration` and `settings.databaseReplicationEnabled`; the
primary answers `replicaNames`, and `delete` of a primary with replicas is 400 until they are
deleted or promoted. `stopReplica` and `startReplica` pause and resume replication
(`pg_wal_replay_pause` and `resume`; `STOP REPLICA` and `START REPLICA`). `promoteReplica`
makes it a standalone primary (`pg_promote()`; `STOP REPLICA`, `RESET REPLICA ALL` and
`read_only` off, persisted). The connector reaches a replica like any instance. On the console, an
instance's page offers **Create read replica**, and a replica's **Promote**, **Stop replication**
and **Start replication**.

The primary may be a created instance or a server `up` runs (`postgres` or `mysql`): their
config allows it (PostgreSQL's `wal_level` is `replica` by default; MySQL's binary log is on,
and GTIDs are turned on online, persisted in the server's data directory). A replica of a
replica of another project's instance or of an external server is 501; a failover replica is 400 (#1462).
Point-in-time recovery is served for created instances (below).

#### Cascading and cross-region replicas (#1381)

A replica's `masterInstanceName` may name a replica: a **cascading replica**, which replicates
from that replica, as Cloud SQL's do, up to Cloud SQL's four levels under a primary (a fifth
is 400). PostgreSQL's is a `pg_basebackup` of the standby, streaming from it (cascading
replication; the standby's replication role and `pg_hba.conf` line are its primary's). MySQL's
replicates by GTID from the replica, which logs what it applies (`log_replica_updates`); its
`cloudsqlreplica` user is created there with the binary log off for the session, so the
replica gains no GTID of its own. Promoting a replica that has replicas makes it a primary
they go on replicating from (PostgreSQL's standbys follow the new timeline), as Cloud SQL
documents. A replica of a replica of a server `up` runs is 501.

A replica's `region` may differ from its primary's: a **cross-region replica**. It answers
that `region`, `gceZone` (`{region}-a`) and connection name (`{project}:{region}:{name}`),
and `connectSettings` its region, so the connectors reach it by that name; a replica created
without one is in its primary's region. The server runs in the same cluster as every other:
the region is not a place here, and replication has none of the latency a real cross-region
replica has. On the console, a replica's page also offers **Create read replica**, with a
**Region** field.

### External primaries (#1501, #1524)

Cloud SQL's replication from a MySQL or PostgreSQL server outside Cloud SQL
([MySQL](https://docs.cloud.google.com/sql/docs/mysql/replication/configure-replication-from-external),
[PostgreSQL](https://docs.cloud.google.com/sql/docs/postgres/replication/configure-replication-from-external))
is served. For MySQL:

1. **A source representation instance**: `instances.insert` with `instanceType`
   `ON_PREMISES_INSTANCE` (or just `onPremisesConfiguration`), a MySQL `databaseVersion` and
   `onPremisesConfiguration.hostPort`, `username` and `password` (the replication user). It is
   a record of the outside server: no server runs for it, `get` and `list` answer it
   (`backendType` `EXTERNAL`, the password never), `delete` removes it once it has no replica,
   and other methods on it are 400. Terraform's `google_sql_source_representation_instance`
   applies. `hostPort` is dialed as given from the replica's pod; another CloudBurrow instance is
   `sqlinst-NAME:3306` (its Service in the cluster, `cloudsql-mysql:3306` for the server `up`
   runs) or the address of its host tunnel, which are mapped to each other.
2. **A replica of it**: `instances.insert` with `masterInstanceName` naming it (and
   optionally `replicaConfiguration.mysqlReplicaConfiguration.username` and `password`, which
   win over the source's). It is a MySQL server of its own, read-only, idle until the sync starts;
   `replicaConfiguration.mysqlReplicaConfiguration.username` is answered.
3. **`verifyExternalSyncSettings`** connects to the source from this machine as the
   replication user and answers each problem as Cloud SQL names it: `CONNECTION_FAILURE`,
   `INCOMPATIBLE_DATABASE_VERSION` (another major version), `BINLOG_NOT_ENABLED`,
   `UNSUPPORTED_GTID_MODE` (not `ON`), `UNSUPPORTED_BINLOG_FORMAT` (not `ROW`),
   `INSUFFICIENT_PRIVILEGE` (one of `REPLICATION SLAVE`, `REPLICATION CLIENT`, `SELECT`,
   `SHOW VIEW`, `RELOAD`, `TRIGGER` on `*.*` missing) and `REPLICA_ALREADY_SETUP`.
   `verifyConnectionOnly` keeps the connection's.
4. **`startExternalSync`** (`syncMode` `ONLINE`, the default) verifies first unless
   `skipVerification`, then dumps the source's databases into the replica with `mysqldump
   --single-transaction --set-gtid-purged=ON` and starts GTID replication from it (`SOURCE_SSL`);
   the operation is `START_EXTERNAL_SYNC`, done once the replica replicates. Writes on the source
   reach the replica afterwards.
5. **`promoteReplica`** ends the migration: the replica stops replicating and takes writes.

**Seeding from a dump file** (#1524): with `onPremisesConfiguration.dumpFilePath` (or the
replica's `mysqlReplicaConfiguration.dumpFilePath`), a `gs://` dump in the instance's Cloud
Storage written with the source's GTIDs as purged (`mysqldump --set-gtid-purged=ON`), the
replica resets its GTIDs, loads the dump when it is created, and then replicates from the source
on its own, with no `startExternalSync`; its creation is done once it replicates. It needs
`--services storage`; without it the replica's creation is 501.

**`syncMode` `OFFLINE`** (#1524), for MySQL and PostgreSQL: `startExternalSync` copies the
source's databases once (`mysqldump --set-gtid-purged=OFF`, or `pg_dump` of the schema and then
the rows) and does not replicate after. Its verification skips the binary log, GTID,
`wal_level` and replication-privilege checks.

For PostgreSQL (#1524):

1. **The source** needs `wal_level` `logical` (on a Cloud SQL source, the flag
   `cloudsql.logical_decoding=on`, which a CloudBurrow instance takes and restarts its server
   with), a `max_replication_slots`, `max_wal_senders` and `max_worker_processes` of at least
   its number of databases, and a replication user with `REPLICATION` that can read every table.
   Google's steps also install pglogical; CloudBurrow's images have none, so the sync uses
   PostgreSQL's built-in logical replication, and `cloudsql.enable_pglogical` is recorded
   without effect. The publication it makes needs the user to be a superuser, or to own the
   tables and have `CREATE` on each database.
2. **A source representation instance** with a `POSTGRES_` `databaseVersion`, `hostPort`
   (`sqlinst-NAME:5432` for another CloudBurrow instance, `cloudsql:5432` for the server `up`
   runs), `username`, `password` and optionally `selectedObjects` (the databases; none is every
   database but the templates). `dumpFilePath` is 400: Google documents it for MySQL only.
3. **A replica of it**, a PostgreSQL server of its own of the source's major version or later.
4. **`verifyExternalSyncSettings`** answers `CONNECTION_FAILURE`,
   `INCOMPATIBLE_DATABASE_VERSION` (a source newer than the replica, or before 10),
   `INVALID_WAL_LEVEL`, `INSUFFICIENT_MAX_REPLICATION_SLOTS`, `INSUFFICIENT_MAX_WAL_SENDERS`,
   `INSUFFICIENT_MAX_WORKER_PROCESSES`, `INSUFFICIENT_PRIVILEGE` and `REPLICA_ALREADY_SETUP`.
5. **`startExternalSync`** copies each database's schema (`pg_dump --schema-only --no-owner
   --no-privileges`), publishes its tables on the source and subscribes the replica's database
   to them, which copies the rows and goes on replicating them.
6. **`promoteReplica`** drops the subscriptions, and with them their slots on the source;
   `stopReplica` and `startReplica` disable and enable them.

Not as Google: sequences are set by the schema copy but not replicated after (pglogical
synchronises them); users are not migrated, as on Cloud SQL; the publication stays on the
source after the promotion.

501, naming why: `migrationType` `PHYSICAL` (an XtraBackup physical backup), and `demote` and
`demoteMaster` (the reverse direction; Google's custom import demotes a promoted instance and
then sets replication up with Cloud SQL's `mysql.*` stored procedures, which CloudBurrow's
servers do not have).

### Database versions and major version upgrades (#1381)

A created instance's `databaseVersion` picks its server's image, the official image of that
major version at the minor version Cloud SQL lists for it, each pinned by digest
(`internal/components.CloudSQLVersionImages`). A node pulls an image the first time an instance
of its version is created, so that create takes longer.

| `databaseVersion` | Image | `databaseInstalledVersion` |
|---|---|---|
| `POSTGRES_9_6` | `postgres:9.6-alpine` | `POSTGRES_9_6_24` |
| `POSTGRES_10` | `postgres:10-alpine` | `POSTGRES_10_23` |
| `POSTGRES_11` | `postgres:11-alpine` | `POSTGRES_11_22` |
| `POSTGRES_12` | `postgres:12-alpine` | `POSTGRES_12_22` |
| `POSTGRES_13` | `postgres:13-alpine` | `POSTGRES_13_23` |
| `POSTGRES_14` | `postgres:14-alpine` | `POSTGRES_14_24` |
| `POSTGRES_15` | `postgres:15-alpine` | `POSTGRES_15_19` |
| `POSTGRES_16` | `postgres:16-alpine` | `POSTGRES_16_15` |
| `POSTGRES_17` | `postgres:17-alpine` (the server `up` runs) | `POSTGRES_17_11` |
| `POSTGRES_18` | `postgres:18-alpine` | `POSTGRES_18_6` |
| `MYSQL_8_0` | `mysql:8.0` | `MYSQL_8_0_46` |
| `MYSQL_8_4` | `mysql:8.4` (the server `up` runs) | `MYSQL_8_4_11` |


**PostgreSQL 9.6 to 12 (#1462)** are past their end of life upstream; Cloud SQL still offers them
(in extended support), and so does CloudBurrow, on each official image's last build, the final
minor release, whose index still carries linux/amd64 and linux/arm64. On 9.6, 10 and 11, high
availability (`REGIONAL`), read replicas and point-in-time recovery are 501: CloudBurrow promotes
a standby with `pg_promote()` and recovers to a point in time with `recovery.signal`, both new in
PostgreSQL 12. Everything else (databases, users, backups, imports and exports, major version
upgrades) is served, and an upgrade to 12 or later gives the instance the rest. `flags.list` for
9.6 to 12 is 501: the flag table here is Cloud SQL's for 13 to 18; `databaseFlags` are passed to
the server, which checks them.

`databaseInstalledVersion` is read from the server. `maintenanceVersion` and
`availableMaintenanceVersions` are not answered: they name Cloud SQL's own releases
(`POSTGRES_17_11.R20250727.00_00`), which no server here is.

What is 501, each checked with `docker buildx imagetools inspect` on 2026-10-16 rather than assumed:

| `databaseVersion` | Why |
|---|---|
| `MYSQL_5_7` | `mysql:5.7` is linux/amd64 only, last built 2023-12 (5.7.44), and no longer built |
| `MYSQL_5_6` | `mysql:5.6` is linux/amd64 only, last built 2021-12 (5.6.51) |
| `MYSQL_8_0_18` to `MYSQL_8_0_4x` | one 8.0 minor runs, `MYSQL_8_0` (8.0.46, the official image's); the 501 says to use it |
| `SQLSERVER_2017_*`, `SQLSERVER_2019_*`, `SQLSERVER_2022_*` | SQL Server has no Docker Hub official image; Microsoft's `mcr.microsoft.com/mssql/server` (2017, 2019, 2022) is linux/amd64 only |

A created primary answers `upgradableDatabaseVersions`, every newer served version of its
engine, and **`instances.patch` with a newer `databaseVersion` is a major version upgrade**, as
Cloud SQL's in-place upgrade (`gcloud sql instances patch --database-version`, Terraform's
`database_version`): a `MAJOR_VERSION_UPGRADE` operation, the instance `MAINTENANCE` meanwhile.
The server's data is dumped in its pod and kept on the host, the server is replaced by one of
the new version on a fresh volume, and the dump is loaded into it:

- **PostgreSQL:** `pg_dumpall`, so every role with its password hash and every database with its
  owners and privileges carry over, as they do through `pg_upgrade`.
- **MySQL:** the accounts (`SHOW CREATE USER` with the authentication string, `SHOW GRANTS`)
  and `mysqldump` of every database but the system schemas, which the new server initialises as
  its own version's. MySQL 8.4 removed `SET_USER_ID`; as its in-place upgrade does, it becomes
  `SET_ANY_DEFINER` and `ALLOW_NONEXISTENT_DEFINER`.

If the new server cannot start or load the dump, the old version is started again from the same
dump and the operation fails with the reason, so the instance keeps its data. A downgrade or a
change of engine is 400.

**An instance with read replicas (#1421)** is upgraded as Cloud SQL documents: without
`includeReplicasForMajorVersionUpgrade` its replicas must be upgraded first, so a primary with
a replica on the older version is 400 saying so; with it the primary is upgraded, and then
every replica under it, level by level for cascading replicas, is rebuilt on the new version
from its upgraded primary (Cloud SQL "recreates the replicas"), each `MAINTENANCE` meanwhile.
A replica that fails is `FAILED` and the operation's error names it; the primary stays
upgraded. **A MySQL replica is upgraded on its own (#1462)**, before its primary, as Cloud SQL
documents: `instances.patch` of the replica with the newer version is a `MAJOR_VERSION_UPGRADE`
that rebuilds it on that version from its primary, which goes on replicating to it (MySQL
replicates from a source to a replica of the next version). The primary is then upgraded
without `includeReplicasForMajorVersionUpgrade`, and each of its replicas is rebuilt from the
new server. **Upgrading a PostgreSQL replica on its own is 501**: it is a physical standby
streaming its primary's WAL, which PostgreSQL does not replicate across major versions; upgrade
the primary with its replicas instead. The console's instance page
offers **Upgrade** with the versions the instance can go to and **Include replicas**, and the
Create instance form's version picker lists every served version.

**The instances `up` runs stay as they are:** `postgres` and `mysql` cannot be patched,
restarted or deleted through the API; each answers 400 naming the `--services` flag that
starts and stops it.

**Persistence follows the instance's mode.** In persistent mode a created instance's server
has a volume, and the instance's record is kept in `cloudsql-instances.json` in the instance
directory, so it is there, with its data, after `stop` and `up`. In ephemeral mode neither is
kept, and the next `up` removes what an earlier run created.

The console's **Cloud SQL instances** page lists every instance, with Create instance,
Delete and Restart; a created instance's page has **Edit instance** (tier, labels, database
flags, deletion protection, automated backups and their start time, sent as one
`instances.patch`).

### Backups, restores and clones (#1330)

`backupRuns.insert`, `list`, `get` and `delete`, `instances.restoreBackup` and
`instances.clone` are served, for created instances and for the servers `up` runs, so
`gcloud sql backups create`, `list`, `restore` and `delete` work.

- **A backup is a real dump**, taken with the mechanism `cloudburrow snapshot` uses for
  Cloud SQL: `pg_dump -Fc` of each database (or `mysqldump --single-transaction --routines
  --triggers --events` of each database but MySQL's own), run inside the server's pod, and
  kept on the host in `cloudsql-backups/<id>/` in the instance directory. A created
  PostgreSQL instance's `postgres` database is included, since Cloud SQL gives it to the
  application; on the server `up` runs it is the maintenance database and is left out.
- **A restore really restores**: `restoreBackup` drops every application database of the
  target and loads the backup's, ending the sessions connected to them, so the rows written
  after the backup are gone. `restoreBackupContext.instanceId` may name another instance of
  the same engine, as on Cloud SQL. **Users are not in a backup**: roles and MySQL's
  `mysql` database stay the target's (Cloud SQL restores them too). A MySQL backup runs as
  `root` with the password the server was started with; one changed afterwards through
  `users.update` makes the backup fail with MySQL's words.
- **`instances.clone`** creates a new instance with the source's settings, starts its server,
  and loads a dump of the source taken then; with `pointInTime` it recovers the source to that
  time (next section), and with `binLogCoordinates` (#1421) to a position in a MySQL source's
  binary log: the newest base at or before the coordinates is loaded and the binary log
  replayed up to the first transaction starting at or after `binLogPosition`, so every
  transaction that started before it is in the clone. It needs `binaryLogEnabled`, as Cloud SQL
  does; `binLogCoordinates` on PostgreSQL, with `pointInTime`, on an instance without binary
  logging, or before its first base is 400. `databaseNames` is SQL Server's ("(SQL Server
  only) Clone only the specified databases"), and is 400 on PostgreSQL and MySQL. `gcloud sql
  instances clone --bin-log-file-name --bin-log-position` works, and the console's page of a
  MySQL instance with binary logging has **Clone to binary log position**.
- **`settings.backupConfiguration`** (`enabled`, `startTime`, `backupRetentionSettings`) is
  recorded and answered. **An automated backup is taken at `startTime` (UTC) only if
  CloudBurrow is running then**; a start time that passes while it is stopped is not made up
  afterwards. The newest `retainedBackups` automated backups (7 by default) are kept;
  `backupRetentionSettings.retentionUnit` other than `COUNT`, or `retainedBackups` outside 1 to
  365, is 400, as on Cloud SQL. On-demand backups are kept until they are deleted.
  `pointInTimeRecoveryEnabled` and `binaryLogEnabled` turn on point-in-time recovery (next
  section).
- **Persistence follows the instance's mode**: in persistent mode the dumps and their records
  (`cloudsql-backups/backups.json`) survive `stop` and `up`; in ephemeral mode the next `up`
  removes them. Deleting a created instance deletes its backups, except a `FINAL` one (below).

The operation is `BACKUP_VOLUME`, `RESTORE_VOLUME` or `CLONE`, `RUNNING` until the dump or the
load is done. The console's instance page has a **Backups** tab with **Create backup**,
**Restore**, **Delete backup** and **Clone**.

**The project-level Backups API (#1462).** `projects/{project}/backups/{id}` names the same
backups: `CreateBackup` takes an `ON_DEMAND` one as `backupRuns.insert` does, `ListBackups`
lists the project's (filter by `instance`, `type` and `state`, joined by `AND`), `GetBackup`,
`UpdateBackup` and `DeleteBackup` read, change and delete one, and `restoreBackup` with `backup`
restores one by its name. `ttlDays` or `expiryTime` expire a backup on the retention clock.
`UpdateBackup` changes any backup's `description`, and the retention (`ttl_days`,
`expiry_time`) of a `FINAL` backup only, as Cloud SQL documents. **Final backups**:
`instances.delete` with `enableFinalBackup` dumps the instance before it is removed (when the
dump fails the instance is kept), and the `FINAL` backup outlives the instance, with
`instanceDeletionTime`, for `finalBackupTtlDays` (30 by default) or until
`finalBackupExpiryTime`. `gcloud sql backups list` without `--instance`, and `describe` by name,
use this API. The console's instance page offers **Delete with final backup**, **Edit project
backup**, **Restore project backup** and **Delete project backup**, which reach a final backup
whose instance is gone from any instance's page.

### Point-in-time recovery (#1330)

A created instance with `settings.backupConfiguration.pointInTimeRecoveryEnabled`
(PostgreSQL) or `binaryLogEnabled` (MySQL) keeps the engine's own transaction log beside its
data, on the server's volume, and `instances.clone` with `cloneContext.pointInTime` makes a new
instance holding the source's databases as they were at that time, to the microsecond. So
`gcloud sql instances create --enable-point-in-time-recovery` (or `--enable-bin-log`) and
`gcloud sql instances clone SRC DEST --point-in-time T` work.

- **PostgreSQL** runs with `archive_mode=on` and an `archive_command` that copies each
  finished WAL segment into `cloudsql-pitr/wal/` next to the data directory; turning it on
  through `instances.patch` restarts the server with it. A **base** is a `pg_basebackup` into
  `cloudsql-pitr/base/`. A recovery forces a commit and a WAL switch on the source, so the log
  past the target is archived, copies the newest base taken before the target into a scratch
  data directory, and starts a second `postgres` on it, on a Unix socket only, with
  `restore_command`, `recovery_target_time` and `recovery_target_action = 'promote'`.
  PostgreSQL replays the WAL up to the target (a commit at exactly the target is kept,
  `recovery_target_inclusive`); the recovered server is dumped with `pg_dump` as a backup is,
  loaded into the clone, and stopped and removed.
- **MySQL**'s binary log is on in MySQL 8.4 by default; a **base** is a `mysqldump
  --single-transaction --source-data` of the application databases into
  `cloudsql-pitr/base/`, which records the binary log position it is consistent with. The
  image has no `mysqlbinlog`, so the replay is MySQL's own replication applier: the clone
  loads the base, then replicates from the source as a temporary replication user, from the
  base's position, with `START REPLICA UNTIL` the position of the first transaction committed
  after the target, and stops there; the replication setup is then removed. The stop position
  is found by reading the binary logs' (anonymous) GTID events, whose
  `immediate_commit_timestamp` is in microseconds (`firstCommitAfter`). Account changes made
  after the base are replayed too, as they are part of the binary log.
- **Bases are taken** when the instance's server first serves with recovery on, and with each
  of its backups (on demand or automated). A base's time is the first whole second after it
  was complete, by the server's clock. **A base that fails fails its backup**: the backup run
  is `FAILED`, with the error, and its operation fails.
- **The recovery window** is reported by `projects.instances.getLatestRecoveryTime` as
  Google documents it: `earliestRecoveryTime` is the oldest base's time, and
  `latestRecoveryTime` is the server's clock now. A `pointInTime` outside it is 400, naming the
  window. The log lives on the server's volume, so deleting the instance deletes it, and in
  ephemeral mode a restart of the server's pod loses it with the data.
- **Retention:** `transactionLogRetentionDays` is applied as Google documents it: 1 to 7 days
  on the Enterprise edition, 1 to 35 on Enterprise Plus (out of range is 400), 7 or 14 days
  by default, and returned with the default when it is not set. A sweep, each minute, after
  each base and after the clock is advanced, drops every base older than the newest one
  complete by the window's start, with the log only those needed: PostgreSQL's archived WAL
  segments before the kept base's start (`pg_archivecleanup` with its `backup_label`), MySQL's
  binary logs before the kept base's file (`PURGE BINARY LOGS TO`). `earliestRecoveryTime` is
  then the later of the oldest base and the window's start.
- **The retention clock** is a CloudBurrow extension, so a test need not wait days, as Secret
  Manager's clock is (#1278): `POST /cloudburrow/v1/clock:advance` on the Cloud SQL Admin
  endpoint, with `{"duration": "86400s"}`, ages the log already kept by that much, sweeps, and
  answers `{"now": …}`. The servers' own clocks do not move, so a `pointInTime` is still read on
  them: log written before the advance is a day older, log written after it ages from when it
  was written. The clock only moves forward, and starts again at the real time on `up`.
- **What is not recovered to a time:** the servers `up` runs (`postgres`, `mysql`) keep no
  log, and a clone to a time of one is 400 saying so; a read replica takes no base.
- **The Backup and DR Service** (#1433, #1425): with `--services backupdr`, a backup plan
  association enrolls an instance in a backup vault and its backups are real backup runs kept as
  that service's ([backupdr.md](backupdr.md)). `instances.pointInTimeRestore` (gcloud's `sql
  instances point-in-time-restore DATASOURCE TARGET POINT_IN_TIME`) checks its request as Google
  does (the datasource's format, `targetInstance`, `pointInTime`, `preferredSecondaryZone` not
  equal to `preferredZone`, each a 400 naming the field), resolves the datasource
  (`projects/…/locations/…/backupVaults/…/dataSources/…`) to its instance and creates the target
  by the clone-to-a-time path above; `instances.restoreBackup` with `backupdrBackup` restores a
  Backup and DR backup into an instance, even after its source was deleted. Without
  `--services backupdr` both answer 501 saying the service is not served, creating nothing; the
  same recovery is then `instances.clone` with `cloneContext.pointInTime`.

### High availability (#1408)

On Cloud SQL, `availabilityType: REGIONAL` makes an instance a primary and a standby in a second
zone of the region, its disk replicated synchronously to the standby, so a transaction is
committed only once both have it; `instances.failover` moves the instance to the standby, which
keeps the connection name and the IP address, with no committed transaction lost.

This used to be 501, on the reasoning that a standby built from CloudBurrow's replicas, which
replicate asynchronously, could lose transactions in a failover. **The user's decision
(2026-10-16):** build it on the engines' own synchronous replication instead, so a failover
loses no committed transaction, as Google's regional standby does not; commits on an HA instance
wait for the standby, as on Google. The design:

- **Two servers, one endpoint.** A `REGIONAL` instance `NAME` is two Deployments,
  `sqlinst-NAME` (the server it was created with) and `sqlha-NAME`, each with its own volume.
  The instance's Service, `sqlinst-NAME`, selects whichever is the primary, so the host tunnel,
  the connector route on 3307 and the instance's read replicas always reach the primary, as
  Memorystore's Standard tier does (#1385). The standby serves no client and is not an instance:
  `instances.list` does not list it, and it is not in `replicaNames`. `gceZone` is the primary's
  zone and `secondaryGceZone` the standby's (`REGION-a`, `REGION-b`; every server runs in the one
  kind cluster, so the zones are reported, as replica regions are, #1381).
- **PostgreSQL:** the standby is a `pg_basebackup` of the primary streaming from the Service as
  `application_name` `cloudsqlstandby`; the primary has `synchronous_standby_names =
  'cloudsqlstandby'` and `synchronous_commit = on`, so a commit returns once the standby has
  flushed its WAL to disk. That is Cloud SQL's guarantee (the regional disk is written in both
  zones before a write is acknowledged); `remote_apply` would also wait for the standby to
  replay it, which Cloud SQL does not promise and the standby, serving no reads, does not need.
  Read replicas stream as `walreceiver`, so a replica never stands in for the standby.
- **MySQL:** lossless semi-synchronous replication (`rpl_semi_sync_source` and
  `rpl_semi_sync_replica`), GTID-based, from the user `cloudsqlstandby`:
  `rpl_semi_sync_source_wait_point = AFTER_SYNC` (the commit becomes visible only once the
  standby has acknowledged it), `wait_for_replica_count = 1`, `wait_no_replica = ON`, and
  `rpl_semi_sync_source_timeout = 4294967295` ms, the largest MySQL takes. MySQL cannot be told
  never to fall back to asynchronous replication: when a commit has waited that long, about
  49.7 days, with no acknowledgement, it falls back, and `Rpl_semi_sync_source_status` turns
  `OFF`. Until then a commit with no standby connected waits. The standby is filled from the
  primary as a read replica is (a `mysqldump` with its GTIDs as purged), and its accounts are
  copied with `SHOW CREATE USER` and `SHOW GRANTS`, with the binary log off, so they are there
  when it is promoted.
- **Failover.** `instances.failover` (with `failoverContext.settingsVersion`: a stale one is
  412, as on Cloud SQL; a `ZONAL` instance is 400) answers a `FAILOVER` operation that: points
  the Service at the standby; stops the primary's server; promotes the standby
  (`pg_promote()`; MySQL stops its receiver, waits for every received transaction to be applied,
  and drops its source); and rebuilds the old primary from the new one as the new standby, as
  Cloud SQL makes a new standby after a failover. The connection name, the host port and the
  connector reach the new primary; clients reconnect to the same endpoint. `gceZone` and
  `secondaryGceZone` swap. The promoted server's Deployment is not changed, so it does not
  restart; each server records the failover it was last rebuilt at, and keeps its data across
  restarts otherwise.
- **The window with no standby.** From the promotion until the new standby is streaming, the
  new primary has none, and commits do not wait: PostgreSQL's `synchronous_standby_names` and
  MySQL's semi-synchronous source are set once the standby streams, and the operation is
  `DONE` then. No failover can happen in that window, as there is nothing to fail over to, so
  no committed transaction can be lost; it is the same as Cloud SQL rebuilding a standby.
- **ZONAL ⇄ REGIONAL.** A patch to `REGIONAL` adds the standby (the primary does not restart),
  and to `ZONAL` turns synchronous replication off and removes the standby; the instance keeps
  the server it was left on.
- **Backups, import and export, point-in-time recovery, upgrades** run in the server the Service
  selects, so in the primary after a failover. Read replicas follow the primary: they replicate
  from the Service (PostgreSQL follows the new timeline: the old primary is shut down fast, so
  it has sent every replica the same WAL as the standby, and an HA primary keeps
  `wal_keep_size = 256MB`, so a replica reconnecting finds the new timeline's WAL). A read
  replica cannot be `REGIONAL` itself (501).
- **Not built:** automatic failover when the primary's pod fails (a pod that fails is restarted
  in place, as for a `ZONAL` instance; fail over with `instances.failover`), and the
  point-in-time recovery log of the old primary is not carried to the new one (a recovery to a
  time before the failover recovers from a base taken after it, or is 400). A MySQL standby
  whose server restarts without its volume (an ephemeral instance) starts empty and is filled
  again only by the next failover or `REGIONAL` patch; until then its primary's commits wait.

`TestCloudSQLHighAvailability` checks it on both engines through the official client and
connector: rows written before a failover are all there after it, through the same connection
name, and the new primary has a synchronous standby of its own.

The console's instance page shows **Point-in-time recovery**, the earliest and latest recovery
times, and a **Point-in-time restore** action (a new instance ID and a time); **Create
instance** and **Edit instance** have a **Point-in-time recovery** checkbox, and **Edit
instance** has **Retained automated backups** and **Transaction log retention (days)**, sent in
the same `instances.patch` and refused, in the API's words, out of range.

### The Cloud SQL connectors (#1330)

The official connectors reach both servers by connection name, as they reach Cloud SQL. Point
the connector's Admin API at CloudBurrow and change nothing else:

```go
import (
    "cloud.google.com/go/cloudsqlconn"
    "cloud.google.com/go/cloudsqlconn/postgres/pgxv5"
)

cleanup, err := pgxv5.RegisterDriver("cloudsql-postgres",
    cloudsqlconn.WithAdminAPIEndpoint("http://"+os.Getenv("CLOUDBURROW_SQLADMIN_ENDPOINT")+"/"))
db, err := sql.Open("cloudsql-postgres",
    "host="+project+":us-central1:postgres user=cloudburrow dbname=cloudburrow sslmode=disable")
```

MySQL is the same with `cloud.google.com/go/cloudsqlconn/mysql/mysql`, the instance `mysql`,
and `MYSQL_PASSWORD`. The connector still wants a credential to send. CloudBurrow checks none,
so ADC, the metadata server or `cloudsqlconn.WithTokenSource` all work.

What happens underneath is what Cloud SQL does:

1. `connectSettings` returns the instance's **server CA**, the connector listener's host
   (`127.0.0.1` unless `--cloudsql-connector` says otherwise) as its `PRIMARY` address,
   and a DNS name, `{instance}.{project}.us-central1.sql.cloudburrow.internal`. Each instance's
   CA is generated on the host the first time it is needed and kept, with its key, in
   `cloudsql-{instance}-server-ca.json` in the instance directory (mode 0600). It is never
   printed, and it stays the same across restarts, so a connector's cached copy stays valid.
2. `generateEphemeralCert` signs the connector's public key with that CA, for one hour.
3. The connector dials that address at **port `3307`** with TLS 1.3. A listener in the `cloudburrow` process
   picks the instance by the DNS name. It presents a certificate signed by the instance's CA,
   with `project:instance` as its CN, which is what the connector checks. It requires the client
   certificate that was issued for that project's instance, and then forwards the connection to
   the server.

#### Running two instances with the connectors

**Port 3307 is fixed.** It is a constant in every connector (`serverProxyPort` in
`cloud.google.com/go/cloudsqlconn`, and the same in the Java and Python connectors and the Auth
Proxy), so `--port-base` does not move it. What Cloud SQL varies between instances is the
address, and that is what a second CloudBurrow instance varies too (#1364):
`--cloudsql-connector host:port` (`CLOUDBURROW_CLOUDSQL_CONNECTOR`, `cloudsqlConnector` in the
file) is where the listener binds, and `connectSettings` reports its host. When it is set,
`cloudburrow env` exports it as `CLOUDBURROW_CLOUDSQL_CONNECTOR_ENDPOINT`.

- **Linux: another loopback address, nothing else changes.** All of `127.0.0.0/8` is routed to
  `lo` with no setup, so `--cloudsql-connector 127.0.0.2:3307` binds without root, and an
  unmodified connector dials `127.0.0.2:3307`. This is as faithful as the first instance.
- **macOS: another port, and a dialer in the application.** Only `127.0.0.1` exists on `lo0`;
  adding `127.0.0.2` takes `sudo ifconfig lo0 alias 127.0.0.2`, which CloudBurrow does not ask
  for (if you run it yourself, the Linux form works). So the second instance takes another port,
  `--cloudsql-connector 127.0.0.1:23307`, and the application redirects the connector's dial,
  which still goes to the reported address at 3307. The Go connector supports that with its own
  documented option, `WithDialFunc`, and still runs the whole handshake itself:

  ```go
  listener := os.Getenv("CLOUDBURROW_CLOUDSQL_CONNECTOR_ENDPOINT") // 127.0.0.1:23307
  cloudsqlconn.WithDialFunc(func(ctx context.Context, network, _ string) (net.Conn, error) {
      return (&net.Dialer{}).DialContext(ctx, network, listener)
  })
  ```

  The Java and Python connectors and the Auth Proxy document no option that changes the port
  they dial, so an application using them reaches only the instance on 3307: run it against
  that instance, one at a time.

The option that needs no root and changes nothing in the application is the loopback address, so
that is the one to use where it exists. A port is the fallback where it does not, and it is not
the default: an unmodified connector against a moved port would dial whichever instance holds
3307 and fail its CA check. `TestCloudSQLConnectorDialsBothServers` redirects only when the
instance's listener is on another port.

An instance whose address is taken still starts, and the rest of the Admin API serves.
`connectSettings` and `generateEphemeralCert` answer 503 naming the CloudBurrow instance that
holds it (each listener records its instance name and PID in the user's temporary directory
while it serves) and the setting that moves it.

Verified with `cloud.google.com/go/cloudsqlconn` on both servers, with Google's Auth Proxy
(`cloud-sql-proxy` 2.19.0) in a pod, below, and with the same release's binary on the host.

**The Auth Proxy binary (#1421).** Google's `cloud-sql-proxy` runs against an instance unchanged,
pointed at it by its own flags:

```sh
export CLOUDBURROW_TEST_CLOUD_SQL_PROXY=$(make -s cloud-sql-proxy)   # pinned, SHA-256 checked
$CLOUDBURROW_TEST_CLOUD_SQL_PROXY --sqladmin-api-endpoint "$CLOUDSDK_API_ENDPOINT_OVERRIDES_SQL" --token local \
  "$PROJECT:us-central1:postgres?port=6432" "$PROJECT:us-central1:mysql?port=6306"
psql "host=127.0.0.1 port=6432 user=cloudburrow dbname=cloudburrow sslmode=disable"
```

`--sqladmin-api-endpoint` is the Admin API's URL, which `cloudburrow env` exports as
`CLOUDSDK_API_ENDPOINT_OVERRIDES_SQL`. Any `--token` will do, as tokens are not checked; pass one,
so the proxy does not go looking for Google credentials. Leave
`--auto-iam-authn` off: it is IAM database authentication, which is 501 (below). The proxy then
calls `connectSettings` and `generateEphemeralCert` and dials the reported address at 3307. It
has no flag for that port (its `--port` is the local listener's), so it reaches only the instance
whose connector holds 3307. Against an instance started with `--cloudsql-connector` on another
port, use a connector with a dial option, as above.

`make cloud-sql-proxy` downloads the release from Google's bucket
(`storage.googleapis.com/cloud-sql-connectors/cloud-sql-proxy/v2.19.0/`) to
`bin/cloud-sql-proxy/`, and checks it against the SHA-256 Google's release page lists before
making it executable. Pinned for linux/amd64, linux/arm64 and darwin/arm64, it is the release the
Cloud Run sidecar image is. `TestCloudSQLAuthProxyBinary` reads, through it, the rows a parallel
import restored (pgx on PostgreSQL, go-sql-driver/mysql on MySQL) and those of a clone to binary
log coordinates. CI's sql shard installs it and runs the test. IAM database authentication (`WithIAMAuthN`, an
`access_token` in `generateEphemeralCert`, users of type `CLOUD_IAM_USER` and the like) is 501: it
signs a database user in as a Google identity, and CloudBurrow has no identities
([ADR-0004](adr/0004-local-access-and-no-authentication.md)). Connecting to the server directly,
as above, still works and needs none of this.

### Cloud Run's Cloud SQL volume (#1346)

A Cloud Run service or job that names an instance in a `cloudSqlInstance` volume gets what Cloud
Run gives it: a Unix socket at `<mountPath>/<connection name>`, PostgreSQL's at
`/cloudsql/<connection name>/.s.PGSQL.5432`. An application connects as it would on Cloud Run:

```sh
psql -h /cloudsql/$PROJECT:us-central1:postgres -U cloudburrow -d cloudburrow
```

```go
Volumes: []*runpb.Volume{{Name: "cloudsql", VolumeType: &runpb.Volume_CloudSqlInstance{
    CloudSqlInstance: &runpb.CloudSqlInstance{Instances: []string{project + ":us-central1:postgres"}}}}},
// and on the container:
VolumeMounts: []*runpb.VolumeMount{{Name: "cloudsql", MountPath: "/cloudsql"}},
```

It is done the way Google does it. Google's Cloud SQL Auth Proxy runs beside the container:
`gcr.io/cloud-sql-connectors/cloud-sql-proxy:2.19.0`, pinned by digest, which the kind node pulls
like a backend's image (and `cloudburrow prefetch` stores for `--offline`). It runs with
`--unix-socket /cloudsql`, `--sqladmin-api-endpoint` at this Admin API as pods reach it, and the
connection names. The volume is an `emptyDir` it shares with the container. The proxy gets a
token from the metadata server, as on Cloud Run, then calls `connectSettings` and
`generateEphemeralCert` and dials port 3307 with TLS 1.3.

A pod cannot reach the host's `127.0.0.1:3307`, so pods get their own connector listener. It
runs on any free port, and the `cloudburrow-host` Service maps port 3307 to it. A
`connectSettings` request that comes through `cloudburrow-host` names that Service's cluster IP
as the `PRIMARY` address. The TLS server name and certificates do not change. So a Cloud SQL
volume works even while another CloudBurrow instance holds the host's 3307.

In a service the proxy is a second container. In a job's task it is a Kubernetes native sidecar,
so the task completes when its container exits. The connection names must be
`project:us-central1:postgres` or `project:us-central1:mysql`, of an enabled server. Anything
else is `INVALID_ARGUMENT` when the service or job is written; with Cloud SQL off, it is
`FAILED_PRECONDITION`. Worker pools and instances refuse the volume. The console's deploy, edit
and job forms have a **Cloud SQL connections** field, which mounts the volume at `/cloudsql`, as
`gcloud run deploy --add-cloudsql-instances` does.

`gcloud run deploy --add-cloudsql-instances`, `--set-cloudsql-instances` and
`--remove-cloudsql-instances` work too. gcloud writes Cloud Run v1's
`run.googleapis.com/cloudsql-instances` annotation on the revision template, and that is the same
volume: named `cloudsql`, mounted at `/cloudsql` in every container, with the same proxy beside
it. `services describe` shows the annotation; the v2 API shows the volume, and the console's
Cloud SQL connections field shows its instances.

```sh
gcloud run deploy web --image=$IMAGE --region=us-central1 \
  --add-cloudsql-instances=$PROJECT:us-central1:postgres
```

MySQL's socket is `/cloudsql/<connection name>` itself:

```sh
mysql -S /cloudsql/$PROJECT:us-central1:mysql -u cloudburrow cloudburrow
```

Verified by `TestRunServiceCloudSQLVolume` and `TestRunJobCloudSQLVolume` (PostgreSQL),
`TestRunServiceCloudSQLMySQLVolume` (MySQL) and `TestGcloudRunDeployCloudSQL` (gcloud and the v1
annotation).

## 4. Authentication is trust, on purpose

The server runs with `POSTGRES_HOST_AUTH_METHOD=trust`. Any connection is accepted as the
`cloudburrow` user with no password.

That is consistent with the rest of CloudBurrow, which authenticates nothing: the metadata
server's credentials authorise nothing, the emulators accept no credentials, and a password
here would be a shared secret that implied a guarantee while being printed in the startup
banner anyway.

The consequence is the same as for every other endpoint and is not softened: **the port is
bound to loopback and must not be exposed.** `--allow-remote` is the only way to bind
anything else, and it says what it does.

## 5. The console screen

Lists the server's databases with their owner, encoding and size, read from `pg_database` —
the server's own catalogue, so the screen reports what PostgreSQL holds rather than what
CloudBurrow believes it holds. Opening one lists its tables from `information_schema`, with
column counts and sizes.

Create and drop are offered because `CREATE DATABASE` and `DROP DATABASE` are real
operations. The database the server was initialised with is refused:

```
"cloudburrow" is the database the server was initialised with and cannot be dropped here
```

Identifiers are validated before being quoted, rather than relying on quoting alone. The
form's pattern is the first guard and the validator is the second, because one of them will
eventually be edited.

**The SQL editor** (a database's **Query** tab, and a table's) reads by default. Each statement
runs inside a `READ ONLY` transaction, so PostgreSQL itself refuses a write; the console does not
inspect the SQL to decide.

**Read-write** is a mode the editor is switched to, never inferred from the text, and it is not
remembered: every visit starts read-only (#995, as in Spanner Studio since #798 and the BigQuery
editor since #994). Its Run button reads **Run statement**, and each run asks first, naming the
database (`Write to database cloudburrow on the local PostgreSQL server?`) and showing the
statement; Cancel sends nothing. The statement runs as `cloudburrow`, the application's user, in
one transaction that is committed when it succeeds and rolled back when it fails. The text is sent
as one query, so several statements separated by semicolons run in that one transaction, and a
failure in any of them leaves nothing of the others. The answer is PostgreSQL's own command tag,
with the rows it affected where the tag counts them — `Committed. PostgreSQL answered INSERT 0 2:
2 rows affected.`, `Committed. PostgreSQL answered CREATE TABLE.` — or PostgreSQL's own error, such
as a duplicate key's `SQLSTATE 23505`. A statement PostgreSQL does not run in a transaction block,
such as `CREATE DATABASE` or `VACUUM`, is refused with PostgreSQL's error; databases are created
with **Create database**. A statement is at most 32 KiB.

Verified through the console API, read back with pgx, by `TestConsoleCloudSQLReadWriteEditor`
(`CREATE TABLE`, `INSERT`, `UPDATE`, `DELETE`, `DROP TABLE`, a duplicate key, a two-statement write
rolled back, and the read-only mode's refusal), and in headless Chrome by
`TestCloudSQLReadWriteEditorsThroughThePage`.

## 6. Reproducing this

```sh
cloudburrow up --services cloudsql --name demo
eval "$(cloudburrow env --name demo)"

psql -c "CREATE TABLE widgets (id serial primary key, name text);" \
  -c "SELECT version();"
```

`TestCloudSQLFromTheExportedPGVariables` (compat) connects with pgx given only the `PG*`
variables `env` exported, and with `CLOUDBURROW_CLOUDSQL_URL`, and runs `SELECT 1`.

No live Google endpoint is contacted at any point, and none exists to contact: there is no
Cloud SQL emulator to reach for.

## 7. Cloud SQL for MySQL

Issue: [#297](https://github.com/cloudburrow/cloudburrow/issues/297)

`--services cloudsql-mysql` runs **MySQL 8.4.11** (`mysql:8.4`, the LTS line, digest-pinned,
linux/amd64 and arm64) beside or instead of PostgreSQL. It is a separate service, not an
engine switch on `cloudsql`, so each engine has its own address, port flag, lifecycle and reset.
The terms are the same: **a real MySQL, not the Cloud SQL Admin API.**

| | |
|---|---|
| Server | MySQL **8.4.11** |
| Address | `127.0.0.1:9017` on the host (`--port-cloudsql-mysql`), plus `cloudsql-mysql.cloudburrow.svc.cluster.local:3306` in the cluster |
| User / database | `cloudburrow` / `cloudburrow` |
| Password | **generated per instance** and kept in `<state-dir>/<instance>/cloudsql-mysql.json` (0600) |
| Durability | a PersistentVolumeClaim in `--mode persistent`; nothing in `--mode ephemeral` |

```sh
cloudburrow up --services cloudsql-mysql
eval "$(cloudburrow env)"      # MYSQL_HOST, MYSQL_PORT, MYSQL_USER, MYSQL_PASSWORD, MYSQL_DATABASE
mysql -h "$MYSQL_HOST" -P "$MYSQL_PORT" -u "$MYSQL_USER" -p"$MYSQL_PASSWORD" "$MYSQL_DATABASE"
```

**Why it has a password when PostgreSQL has none.** The MySQL image has no trust mode. The
alternative, `MYSQL_ALLOW_EMPTY_PASSWORD`, names exactly what it is. So the password is
generated locally, once, and never leaves the machine. It authorises nothing beyond this local
server. The password is kept rather than regenerated, because MySQL writes it into its data
directory when it initialises. A new one on the next start would lock a persistent instance
out of its own data.

`cloudburrow env` exports it. `cloudburrow status` names the file that holds it and does not
print it: status output is what people paste into issues, and `cloudburrow diagnose` collects it.

**Reset.** `cloudburrow reset` (or `/admin/reset?service=cloudsql-mysql`) drops every database
except MySQL's own (`mysql`, `information_schema`, `performance_schema`, `sys`), then recreates
an empty `cloudburrow`. The application's user keeps its grant, because the grant is on the
database's name.

**Verified** by `test/compat/cloudsqlmysql_test.go`:
- `go-sql-driver/mysql` from the host;
- the `mysql` client from a pod through the Service name;
- a reset that removes the table.

Durability is measured in CI. A row written before `stop` is read back after `up` in persistent
mode, and is absent after `up --mode ephemeral`.

**Ready means a login** (#1262), for MySQL and PostgreSQL alike. `up` reports ready only once
the application's user has logged in to its database through the host address `up` prints and
run `SELECT 1`. An accepted TCP connection is not enough: after `stop` and `up` the tunnel
once carried a connection that mysqld never answered, and the first query ended in
`unexpected EOF`. The same login gates every re-establishment of the tunnel. `up` waits up
to three minutes for it, then fails, naming the driver's error.

**The console screen** (#868) is Cloud SQL's **MySQL** page, at `/cloudsql-mysql`, beside
**PostgreSQL**. It lists the one server, with its version, and its databases (MySQL's own are
left out), then a database's tables, views, routines, users, server settings and live activity,
and a table's columns and indexes, all from `information_schema`, `performance_schema` and
`mysql.user` (never its password column). It reads as root, from the instance's credentials.

Create and drop are `CREATE DATABASE` and `DROP DATABASE`. A created database is granted to
`cloudburrow`, so the application can open it. As on PostgreSQL, the initial database cannot
be dropped here.

The SQL editor reads by default, and MySQL enforces it rather than the console. Each statement runs
inside `START TRANSACTION READ ONLY`, but that alone is not enough in MySQL: DDL commits the
transaction implicitly and then runs. So the statement also runs as `cloudburrow_console`, an
account the screen creates holding only `SELECT` and `SHOW VIEW` on the database being queried.
A write or DDL gets MySQL's own `command denied` (1142). That account appears on the Users tab
and survives `reset`. Verified by `TestConsoleCloudSQLMySQLSchemaAndReadOnlyQuery` and
`TestConsoleCloudSQLMySQLCreateAndDropDatabase`, and in a browser by
`TestCloudSQLMySQLCreateQueryAndDropThroughThePage`.

**Read-write** (#995) is the same opt-in, confirmed mode as on PostgreSQL, and it does not widen
`cloudburrow_console`: a write runs as a different login, `cloudburrow`, the application's user,
with exactly its privileges — every one on its own database and on databases created here, none on
the others, where a write is MySQL's own access-denied error. One statement runs per click, with
MySQL's default autocommit, so it is committed when it succeeds; MySQL commits a schema change
(`CREATE`, `ALTER`, `DROP`) as it runs, so there is no transaction to roll one back, in MySQL or
here. A second statement after a semicolon is MySQL's own syntax error. The answer is MySQL's
rows-affected count, `Committed as cloudburrow. MySQL answered: 2 rows affected.` (0 for DDL, as
the `mysql` client reports it), or MySQL's own error, such as a duplicate key's 1062. Verified
through the console API, read back with `go-sql-driver/mysql`, by
`TestConsoleCloudSQLMySQLReadWriteEditor`: `CREATE TABLE`, `INSERT` (with `CURRENT_USER()`
recorded as `cloudburrow`), `UPDATE`, `DELETE` and `DROP TABLE` commit, a duplicate key is 1062,
and afterwards the read-only mode still refuses an `INSERT` and a `CREATE TABLE` with 1142 for
`cloudburrow_console`; in headless Chrome by `TestCloudSQLReadWriteEditorsThroughThePage`.

**Not supported:**
- **`cloudburrow state save`** does not capture it. Use `mysqldump`.
- **Creating or changing instances, and the connector path, through the Cloud SQL Admin API**,
  for either engine; instance reads, databases, users and operations are served (#1301, see
  section 3).
