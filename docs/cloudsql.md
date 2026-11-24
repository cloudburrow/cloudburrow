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

What answers 501, saying why: IAM groups (`CLOUD_IAM_GROUP`; IAM users and service accounts are served, see [IAM database authentication](#iam-database-authentication-1944)), `pointInTimeRestore` and `restoreBackup` with `backupdrBackup` unless the instance runs `--services backupdr` ([backupdr.md](backupdr.md), #1433), SQL Server
versions (their images are linux/amd64 only), `MYSQL_5_7` and `MYSQL_5_6` on a node that cannot run linux/amd64 images, and
their replicas, high availability, point-in-time recovery and upgrades anywhere (see [Database versions](#database-versions-and-major-version-upgrades-1381)), BAK and TDE imports and exports (SQL Server's files),
and promoting or switching over to a PostgreSQL read replica upgraded on its own before its primary is upgraded (#1524); read
replicas, high availability and point-in-time recovery on PostgreSQL 9.6 to 11 are served since #1524, with
`recovery.conf` and `pg_ctl promote`. A legacy failover replica
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

- **Storage shrink (#1535)**: `projects.instances.getDiskShrinkConfig` answers
  `minimalTargetSizeGb`, measured from the server's databases' size on disk (rounded up to a GB,
  at least 10 GB); `performDiskShrink` sets a created primary's `settings.dataDiskSizeGb` to a
  smaller `targetSizeGb`, no smaller than that; `resetReplicaSize` sets a read replica's to its
  primary's. `dataDiskSizeGb` is recorded as given: the server's volume has no fixed size.
  Through `gcloud sql instances get-storage-shrink-config` and `perform-storage-shrink`, and the
  console's Storage shrink config, Shrink storage and Reset replica size.

Refused with 501, each naming what is missing:

| Methods | Why |
|---|---|
| Entra ID certificates, `acquireSsrsLease`, `releaseSsrsLease` | SQL Server features its SQL Server instances (#1916) do not have: Microsoft's image, SQL authentication only |

### Maintenance (#1524)

Google never updates CloudBurrow's servers (each is its version's pinned image), so a maintenance event is scheduled locally instead, standing in for the update Google would schedule. This is **CloudBurrow's own control endpoint, not a Cloud SQL API**:

```
POST http://<sqladmin>/cloudburrow/v1/projects/PROJECT/instances/INSTANCE:scheduleMaintenance
{"startTime": "2026-12-06T03:00:00Z", "scheduleDeadlineTime": "..."}   # both optional
POST http://<sqladmin>/cloudburrow/v1/projects/PROJECT/instances/INSTANCE:cancelMaintenance
```

or the console's **Schedule maintenance (CloudBurrow)** and **Cancel maintenance (CloudBurrow)** on a created instance's page. From then on the event behaves as Google documents it (docs.cloud.google.com/sql/docs/mysql/maintenance and set-maintenance-window, read 2026-11-18):

- `instances.get` reports `scheduledMaintenance` (`startTime`, `canReschedule`, `canDefer`, `scheduleDeadlineTime`). With no `startTime`, the event starts in the first `settings.maintenanceWindow` (day 1 Monday to 7 Sunday, 0 any day; hour UTC) at least a week away, the notice Google gives. The deadline is 28 days after the start, 7 on the `week5` update track.
- `instances.rescheduleMaintenance` (and `gcloud sql reschedule-maintenance`): `IMMEDIATE` starts it now; `NEXT_AVAILABLE_WINDOW` moves it to the next window after its current start; `SPECIFIC_TIME` to `scheduleTime`, no earlier than the current start (gcloud checks this itself: "Must be after original scheduled time") and no later than the deadline. The answer is a `RESCHEDULE_MAINTENANCE` operation. It is 400 with no scheduled event (the instances `up` runs never have one), within 24 hours of the start (`canReschedule` false), or before the current start or past the deadline; the messages for the first two are gcloud's own client-side ones.
- At its start the instance is `MAINTENANCE`, its server restarted (connections dropped) in a `MAINTENANCE` operation, then `RUNNABLE`, with no `scheduledMaintenance`. It is skipped when a deny maintenance period holds the start (a replica's primary's too) or the instance is stopped, and waits while another operation runs.
- `settings.denyMaintenancePeriods` holds at most one period, of 1 to 90 days, `yyyy-mm-dd` or recurring `mm-dd`; `maintenanceWindow` day and hour are checked.

UNVERIFIED (re-read 2026-11-18: https://docs.cloud.google.com/sql/docs/mysql/set-maintenance-window and /maintenance give none of them): the errors' wording; `NEXT_AVAILABLE_WINDOW` with no window set moves the event a week; `canDefer` (deprecated) equals `canReschedule`.

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
Add server CA and Rotate server CA.

An instance created (or patched) with `settings.ipConfiguration.serverCaMode`
`GOOGLE_MANAGED_CAS_CA` (#1614) is signed instead by its region's shared CA hierarchy, a
subordinate CA under a root, which CloudBurrow generates locally and keeps beside the instances'
own CAs, standing in for Google's CAS-hosted ones. `connectSettings` then answers that chain as
`serverCaCert`, the mode, and the instance's DNS name in `dnsNames` (also on the instance as
`dnsName` and `dnsNames`), which the connectors check the server certificate's SAN against.
Its server certificate is rotated rather than its CA: `listServerCertificates` lists the active,
upcoming and previous versions, `addServerCertificate` adds an upcoming one (replacing one never
rotated in), and `rotateServerCertificate` makes the upcoming one, or with `nextVersion` the
previous one, the one the listener presents, as `gcloud sql ssl server-certs create`, `list`,
`rotate` and `rollback` do. `connect.resolve` answers the connect settings of the instance a DNS
name names, in its region. On a `GOOGLE_MANAGED_INTERNAL_CA` instance the server certificate
methods are 400, and on a CAS instance `addServerCa` and `rotateServerCa` are (CloudBurrow's own
messages). The console has Server CA mode on Create instance and Edit instance, and on a CAS
instance's page its server certificates, Add server certificate, Rotate server certificate and
Resolve DNS name. Client certificates are still signed by the instance's own CA.

**Customer-managed CAs (#1834).** `serverCaMode` `CUSTOMER_MANAGED_CAS_CA` with `serverCaPool`
`projects/PROJECT/locations/REGION/caPools/POOL` (`gcloud sql instances create|patch
--server-ca-mode=CUSTOMER_MANAGED_CAS_CA --server-ca-pool=...`, the console's Server CA pool) has a
CA of a pool of this instance's Certificate Authority Service ([privateca.md](privateca.md)) issue
the server certificate, so it needs `privateca` in `--services` (412 otherwise). As
[Google documents](https://docs.cloud.google.com/sql/docs/postgres/customer-managed-ca), Cloud SQL
requests it as a configuration-based certificate with an EC P-384 key and the instance's DNS name in
the subject alternative names: it is a `certificates.create` on the pool, listed among the pool's
certificates (`cloudsql-INSTANCE-...`, Enterprise tier), signed by the pool's first enabled CA. The
listener presents it with the CAs between it and the root, so it verifies to the pool's root
(a subordinate CA under a root of another pool works). `connect.get` answers the mode and the issuing
CA chains as `serverCaCert`, `listServerCertificates` each version's issuing CA in `caCerts`, and
`listServerCas` those chains; `addServerCertificate` has the pool issue an upcoming certificate and
`rotateServerCertificate` (gcloud's `server-certs rotate` and `rollback`) makes it the presented one.
The pool is checked as Google documents, each with its words: the format ("Invalid format for server
CA pool"), its region ("The instance's server CA pool must be in the same region as the instance."),
that it exists (404, "parent resource ... not found") and has an enabled CA ("There are no enabled
CAs in the CaPool"), and the pool's own issuance policy (configuration-based requests and DNS names in
SANs allowed, the key type) through the issuance itself. MySQL 5.7 is 400 ("supported only on MySQL
8.0.30 and later"). Such an instance cannot be patched back to `GOOGLE_MANAGED_INTERNAL_CA` (400);
it moves to `GOOGLE_MANAGED_CAS_CA` with `serverCaPool` `""`, and a pool left set with another mode is
400. Changing the pool issues a certificate from the new one. Not enforced: the service agent
(`service-PROJECT@gcp-sa-cloud-sql.iam.gserviceaccount.com`) and its
`roles/privateca.certificateRequester` on the pool, as no call here is authorized (ADR-0004) and the
pool's IAM policy is stored, not enforced (ADR-0006). `serverCertificateRotationMode`
`AUTOMATIC_ROTATION_DURING_MAINTENANCE` rotates the server certificate in a maintenance run
(§ maintenance, #1524) when the active one expires within 180 days, as Google's authorize-ssl
page says ("automatic server certificate rotation to run during maintenance updates up to 180 days
before expiry", shared CA and customer-managed CA instances only): to the upcoming version
`addServerCertificate` added, or to one issued then; the old one stays listed as the previous
version. A skipped maintenance (a deny period, a stopped instance) rotates nothing, as "If
maintenance is skipped, rotation doesn't happen automatically in that window". UNVERIFIED
(re-read 2026-11-18): that the rotated-out version stays listed and that an upcoming one is used.
Manual rotation is `addServerCertificate` and `rotateServerCertificate`. Disabling the pool's CA later does not revoke the
certificate the server already presents.

The servers are reached through the connector path, so the certificates
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
| `databaseVersion` | `POSTGRES_9_6` to `POSTGRES_18`, `MYSQL_5_6` and `MYSQL_5_7` (#1524, where the node runs linux/amd64), `MYSQL_8_0` or `MYSQL_8_4` (#1381, #1462): see [Database versions](#database-versions-and-major-version-upgrades-1381). The engine's service must be enabled (412 otherwise). `MYSQL_5_6`, `MYSQL_5_7`, `MYSQL_8_0_*` minor pins and any other `POSTGRES_*` or `MYSQL_*` are 501 saying why; another value is 400. `SQLSERVER_*` (#1916) is 412 FAILED_PRECONDITION until you accept Microsoft's licence, then served where the node runs linux/amd64: see [Cloud SQL for SQL Server](#cloud-sql-for-sql-server). |
| `region` | Recorded and answered, and in the connection name (`us-central1` when left out). Every server runs in the one cluster. |
| `rootPassword` | MySQL's `'root'@'%'` password (empty leaves `root` with none, as Cloud SQL does). `root` is yours: change its password or delete it, as the hashicorp/google provider does after every insert; CloudBurrow manages the instance through an account of its own (see [CloudBurrow's MySQL account](#cloudburrows-mysql-account-1993)). PostgreSQL's user is `postgres`, trusted locally like the server `up` runs. |
| `settings.tier` | Required, recorded and answered. The server is not sized by it. |
| `settings.databaseFlags` | Passed to the server: `-c name=value` to `postgres`, `--name=value` to `mysqld`. A patch that changes them starts the server again with them; a flag the server rejects fails the operation with its words. `cloudsql.*` flags (IAM authentication, Cloud SQL's extensions) are 501. |
| `settings.userLabels`, `deletionProtectionEnabled` | Recorded and answered. `delete` of a protected instance is 400, as on Cloud SQL. |
| `diskEncryptionConfiguration` | A customer-managed encryption key: see [Customer-managed encryption keys](#customer-managed-encryption-keys-1538). |
| Every other field | Applied, recorded, ignored as output only, or refused by name: see [Request fields](#request-fields-1538). |
| `masterInstanceName` | A read replica: see [Read replicas](#read-replicas-1330). |
| `settings.activationPolicy` | `NEVER` stops the instance and `ALWAYS` starts it (#1875), as `gcloud sql instances patch --activation-policy=never` / `always` and the console's **Stop** / **Start** do ([start-stop-restart-instance](https://docs.cloud.google.com/sql/docs/mysql/start-stop-restart-instance)). Each is an `UPDATE` operation. A stopped instance's server is scaled to no pods (a `REGIONAL` instance's standby too), its tunnel closed and its volume kept: connections fail, and the data is there when it starts again. The API answers it `RUNNABLE` with `activationPolicy` `NEVER` ("The instance is running, or has been stopped by owner"; gcloud shows it `STOPPED`). What its server would have to serve (users, databases, `restart`, exports and imports, `backupRuns.insert`, `executeSql`, a new read replica of it, a major version upgrade, ...) is 400 "Invalid request since instance is not running."; its record (get, patch, delete, certificates, backup records) is served. An instance created with `NEVER` is initialised, then stopped. `NEVER` on a read replica is 400 ("NEVER is not supported for read replica instances"); `ON_DEMAND`, deprecated, is 400. |
| `settings.passwordValidationPolicy` | Applied (#1876): see [Password policy](#password-policy-1876). |
| `settings.availabilityType` | `ZONAL` (the default) or `REGIONAL`, a high-availability instance with a synchronous standby: see [High availability](#high-availability-1408). Another value is 400. |
| `replicaConfiguration.failoverTarget`, `failoverReplica` | 400, as Cloud SQL answers (#1462): the legacy high-availability configuration, deprecated for every instance on 2025-01-13, with which "you can't create instances" ([configure-legacy-ha](https://docs.cloud.google.com/sql/docs/mysql/configure-legacy-ha)). `REGIONAL` is high availability. |

The operation `insert` returns is `RUNNING` until the server serves through its tunnel, then
`DONE` (or `DONE` with the server's error, and the instance `FAILED`). `delete` removes the
server, its Service and its volume. `restart` restarts its pod.

#### CloudBurrow's MySQL account (#1993)

Cloud SQL manages a MySQL instance through system accounts of its own (`root@localhost`,
`root@127.0.0.1` and `root@::1` are "used to provide the managed database service";
[users](https://docs.cloud.google.com/sql/docs/mysql/users)), which "you can't delete,
modify, or assign database roles to", and which gcloud and the console cannot display. Its
`'root'@'%'` is the user's, and may be deleted. A created MySQL instance here is the same:

- CloudBurrow signs in as **`cloudsqladmin`**, at `localhost`, `127.0.0.1` and `::1` only, with
  every privilege and the password the instance was created with (`rootPassword`). The Admin
  API (through the instance's tunnel, a `kubectl port-forward` that arrives from the pod's
  loopback) and every script in the server's container (backups, exports and imports,
  point-in-time recovery, replicas, the standby, major version upgrades) use it. It is a name of
  its own, not `root` at a loopback host: MySQL matches the most specific host first, so a
  `root@127.0.0.1` would take every `root` sign-in through the tunnel, and refuse the password
  `users.update` gave `'root'@'%'`.
- It is made, with the binary log off, each time each server starts: the primary, a read
  replica (its `super_read_only` lifted meanwhile), the standby, and a physically restored copy
  (in its `--init-file`). An instance created before #1993 gets it at its next start (the next
  `up`, or a stop and start); until then CloudBurrow signs in as `root`, which it could not
  delete.
- `users.list` and `users.get` leave out `cloudsqladmin`, `root` at the loopback hosts,
  `cloudsqlreplica` and `cloudsqlstandby`; `users.insert` of one of their names is 400.
- `users.delete` of `'root'@'%'` succeeds, and the instance keeps working: `TestCloudSQLMySQLRootDeleted`
  deletes it and runs an export, an import, a backup and its restore, a clone and a restart,
  and `TestBackupDRRestoreToANewInstance`'s Terraform MySQL subtest applies a
  `google_sql_database_instance`, whose provider deletes it. A major version upgrade and a new
  standby keep it deleted.

`up`'s own MySQL server (`--services cloudsql-mysql`) is still reached as `root`, which
`users.delete` refuses there, with its `cloudburrow` user.

### Password policy (#1876)

`settings.passwordValidationPolicy` is taken on `insert`, `patch` and `update` and answered as
set; a `patch` of some of its fields keeps the others, as `gcloud sql instances patch
--password-policy-*` sends only the flags given. As Google documents
([built-in-authentication](https://docs.cloud.google.com/sql/docs/postgres/built-in-authentication)),
it acts only once `enablePasswordPolicy` is set; then `users.insert` and `users.update` refuse
with 400 a password that is shorter than `minLength`, lacks one of `COMPLEXITY_DEFAULT`'s
lowercase, uppercase, numeric and non-alphanumeric characters, contains the user's name
(`disallowUsernameSubstring`, case-insensitive), is one of the user's last `reuseInterval`
passwords, or (PostgreSQL) changes a password set less than `passwordChangeInterval` ago.
Refused with 400: `reuseInterval` outside 0 to 100, or above 0 on `MYSQL_5_7` (8.0 and later
only); `passwordChangeInterval` on MySQL ("only supported for PostgreSQL") or not a duration;
another `complexity`. The previous passwords are kept only as SHA-256 hashes in the instance's
record. Not as Google: a password set in SQL (`ALTER USER`, `ALTER ROLE ... PASSWORD`) is not
checked, as the policy is the Admin API's here, not a server plugin; the 400's wording is
CloudBurrow's (Google documents none). A user's own `passwordPolicy` is the next section.

### A user's own password policy and dual passwords (#2009)

`users.insert` and `users.update` take `User.passwordPolicy`
([built-in-authentication](https://docs.cloud.google.com/sql/docs/mysql/built-in-authentication);
`gcloud sql users set-password-policy`), and `users.get` and `list` answer it with its `status`.
An update sends only the fields given; the others stay, and any update of the policy unlocks the
user, as the Users page's Unlock does (the console's Unlock sends the policy unchanged). Each part
is the server's own:

- **MySQL 8.0 and later**: `allowedFailedAttempts` with `enableFailedAttemptsCheck` is the account's
  `FAILED_LOGIN_ATTEMPTS n PASSWORD_LOCK_TIME UNBOUNDED`, so MySQL itself refuses the account,
  even with its right password, after n failed sign-ins in a row, until it is unlocked;
  `status.locked` is read from the server's error log (`performance_schema.error_log`, MySQL's
  `MY-013525` refusals since the last unlock and the server's start; the policy turns on
  `log_error_verbosity` 3, which logs them), as MySQL keeps the lock in memory only.
  `passwordExpirationDuration` is `PASSWORD EXPIRE INTERVAL` in whole days, rounded up (MySQL counts
  days, 1 to 65535), with `status.passwordExpirationTime` from `password_last_changed`;
  `enablePasswordVerification` is `PASSWORD REQUIRE CURRENT`. MySQL 5.7 takes only the expiration;
  5.6 none (400).
- **PostgreSQL**: `passwordExpirationDuration` is the role's `VALID UNTIL`, set again from each
  password change, and `status.passwordExpirationTime` is it. A role with a policy is a member of
  `cloudsqlpasswordpolicy`, which `pg_hba.conf` holds to its password (`md5`), so the server checks
  the password and its expiry rather than trusting the connection, as it does other users.
  `enablePasswordVerification` is 400 ("supported only for MySQL").
- **PostgreSQL's lock after failed attempts** (#2014): `allowedFailedAttempts` with
  `enableFailedAttemptsCheck` locks the role once its password has been tried incorrectly that many
  times in a row: it loses its login privilege (`NOLOGIN`), which is what `status.locked` reports
  ("If true, user does not have login privileges"), so even its right password is then refused
  (`role "NAME" is not permitted to log in`). PostgreSQL does not count failed sign-ins itself; its
  log says each one (`password authentication failed for user`), on the server container's stderr,
  which CloudBurrow follows through the cluster (`kubectl logs --follow`) while a role of that
  server has the lock. The count is kept in the role's setting `cloudsql.failed_sign_ins`, and a
  sign-in that passes forgets it (the policy turns `log_connections` on, through `ALTER SYSTEM`, for
  its `connection authorized` lines; a `log_connections` database flag of `off` keeps the count
  running across sign-ins). A `users.update` of the policy (the Users page's Unlock), a new
  password, or `ALTER USER NAME WITH LOGIN`, as Google documents, unlocks it. The count runs on a
  primary only (a replica's users are its primary's), and only while CloudBurrow runs: a failed
  sign-in while it is stopped is not counted.

`users.update` with `dualPasswordType` `DUAL_PASSWORD` and a password is MySQL's Retain current
password (`ALTER USER ... RETAIN CURRENT PASSWORD`, `gcloud sql users set-password
--retain-password`): the old and the new password both sign in, and `users.get` answers
`dualPasswordType` `DUAL_PASSWORD`; `NO_DUAL_PASSWORD` without a password discards the old one
(`DISCARD OLD PASSWORD`, `--discard-dual-password`), and with one sets it and discards the old.
`DUAL_PASSWORD` without a password, on `users.insert`, on PostgreSQL or before MySQL 8.0 is 400.
Tested by `TestCloudSQLUserPasswordPolicy` (compat) and, in the console (Add user account's Set user
password policy; a user's Change password with Retain current password, Set password policy,
Unlock and Discard old password; the Users list's Password status), by
`TestCloudSQLConsoleUserPasswordPolicyAndRetainedPassword` (browser). Not as Google: a policy set
in SQL is read back but a lock set by `ACCOUNT LOCK` is not reported; the 400s' wording is
CloudBurrow's.

### Request fields (#1538)

Every settable field of the request bodies served, `DatabaseInstance` and its `Settings`, `User`,
`Database`, `BackupRun`, `Backup` and `SslCertsInsertRequest`, is one of four things, never accepted
and dropped: **applied** (it changes what the server does or answers), **recorded** (stored and
answered as sent, with nothing behind it), **ignored** (output only, as Cloud SQL ignores it in a
request), or **refused** with 400 or 501 naming it. A field the discovery document does not have is
400. `TestSQLFieldTablesMatchTheDiscoveryDocument` checks the lists in
`internal/service/sqladmin/fields.go` against the pinned discovery document, and
`TestSQLFieldsNeverSilentlyDropped` sends each one.

| Field | What happens |
|---|---|
| `settings.settingsVersion` | On `patch` and `update`, a value that is not the instance's is 412, Cloud SQL's guard against a concurrent change. |
| `settings.retainBackupsOnDelete` | `true` keeps the instance's backups after `delete` (they stay listed by `Backups.ListBackups`); otherwise only a `FINAL` one is kept. |
| `settings.finalBackupConfig` | `enabled` makes a `delete` without `enableFinalBackup` take a `FINAL` backup, kept `retentionDays` (30 when not given). |
| `settings.databaseReplicationEnabled` | A read replica's replication state, set by `stopReplica` and `startReplica`; a different value in a patch is 400 naming those methods, and `true` on a primary is 400. |
| `settings.dataApiAccess`, `edition`, `dataDiskSizeGb`, `backupConfiguration`, `activationPolicy`, `availabilityType`, `passwordValidationPolicy` | Applied: see the sections on the Data API, point-in-time recovery, storage shrink, backups, creating instances (stop and start), high availability and the password policy. |
| `settings.ipConfiguration` | Recorded (authorized networks, `ipv4Enabled`, `sslMode`, private network). `serverCaMode` `GOOGLE_MANAGED_INTERNAL_CA` and `GOOGLE_MANAGED_CAS_CA` are served (#1614), and `CUSTOMER_MANAGED_CAS_CA` with `serverCaPool` (#1834, below); `serverCertificateRotationMode` is checked and recorded. A patch whose `ipConfiguration` names none of the three keeps them. |
| `settings.maintenanceWindow`, `denyMaintenancePeriods`, `autoUpgradeEnabled`, `insightsConfig`, `performanceCaptureConfig`, `dataCacheConfig`, `dataDiskType`, `dataDiskProvisionedIops`, `dataDiskProvisionedThroughput`, `storageAutoResize`, `storageAutoResizeLimit`, `locationPreference`, `pricingPlan`, `replicationType`, `replicationLagMaxSeconds`, `crashSafeReplicationEnabled`, `acceleratedReplicaMode`, `enableDataplexIntegration`, `authorizedGaeApplications` | Recorded and answered. They describe the machine, maintenance and monitoring Cloud SQL would give the instance; nothing here acts on them. |
| `settings.connectorEnforcement` `REQUIRED` | 501: the server is reached through its host tunnel as well as the connectors, and direct connections are not refused. `NOT_REQUIRED` is taken. |
| `settings.connectionPoolConfig` | Managed connection pooling (#2141): see [Managed connection pooling](#managed-connection-pooling). |
| `settings.enableGoogleMlIntegration`, `readPoolAutoScaleConfig` | 501 when enabled: no Vertex AI integration or read pools. |
| `settings.activeDirectoryConfig`, `advancedMachineFeatures`, `collation`, `entraidConfig`, `sqlServerAuditConfig`, `timeZone` | 400: SQL Server settings. |
| `replicationCluster.failoverDrReplicaName` | Applied on a MySQL or PostgreSQL primary: see DR replicas and switchover. `drReplica` is answered on the DR replica; `psaWriteEndpoint`, a private services access DNS name, is never answered, as no instance has a private IP. |
| `databaseCenterIntegrationEnabled`, `geminiConfig`, `maintenanceVersion`, `nodeCount`, `switchTransactionLogsToCloudStorageEnabled`, `tags` | 501 when set, each saying what is not served. |
| `databaseCenterIntegration` `ENABLED` | 501, as `databaseCenterIntegrationEnabled` `true` is: Database Center is not served. `DISABLED` and `DATABASE_CENTER_INTEGRATION_UNSPECIFIED` are taken (#1700). |
| `skipPrecheck` | Taken. A major version upgrade here has no separate precheck step to skip: what `preCheckMajorVersionUpgrade` reports is the validation `instances.patch` makes either way (#1700). |
| `connectionName`, `ipAddresses`, `state`, `gceZone`, `serverCaCert`, `selfLink` and the other output fields | Ignored, as Cloud SQL ignores them. |
| `users`: `databaseRoles` | 501. `serverRoles` and `sqlserverUserDetails` are 400 (SQL Server). `passwordPolicy` and `dualPasswordType` are served (#2009). |
| `databases`: `sqlserverDatabaseDetails` | 400 (SQL Server). |
| `backupRuns.insert`: `backupKind` `PHYSICAL` | 501: a backup is a logical dump. `location` is taken (see Backup locations); `diskEncryptionConfiguration` is 400 (a backup inherits its instance's), `timeZone` 400 (SQL Server). |

### Customer-managed encryption keys (#1538)

`diskEncryptionConfiguration.kmsKeyName` on `instances.insert` (Terraform's `encryption_key_name`,
the console's **Customer-managed encryption key**) is a CryptoKey of this instance's Cloud KMS, so it
needs `kms` in `--services` (412 otherwise). As Cloud SQL requires, the key is in the instance's
region (another location is 400), and it cannot be added to an instance, changed or removed after
it is created (400). The instance gets a data key of its own, wrapped by the CryptoKey's primary
version and kept only wrapped; it answers `diskEncryptionConfiguration` and
`diskEncryptionStatus.kmsKeyVersionName`, the version that wrapped it.

The key is checked, by unwrapping the data key, whenever the instance is read and before its server
is started (created, restarted, patched in a way that restarts it, or brought back by `up`). Cloud
SQL documents that "if Cloud SQL is unable to access the key (such as if you disable the key
version), Cloud SQL suspends the instance", so while the version is disabled or destroyed the
instance answers `SUSPENDED` with `suspensionReason` `KMS_KEY_ISSUE`, a create or a start is refused
naming the key, and `connectSettings` and `generateEphemeralCert` are 412, so the connectors cannot
reach it. It is `RUNNABLE` again once the version is enabled. A read replica in its primary's region
inherits the primary's key (another key is 400); one in another region needs a key of that region;
a clone keeps its source's key.

**The data volume itself is not encrypted with the key**: the server's files are on an ordinary
volume of the cluster, and a server already running keeps answering direct connections through its
host tunnel while its key is disabled. What the key controls is whether the instance can be started
and reached through the API and the connectors.

**Re-encryption (#1614).** `instances.reencrypt` (`gcloud sql instances reencrypt`, the console's
**Re-encrypt**) unwraps the instance's data key and wraps it again with the CryptoKey's current
primary version, which `diskEncryptionStatus.kmsKeyVersionName` then names, so the version that
wrapped it before can be disabled or destroyed without suspending the instance. It is a `REENCRYPT`
operation, and the server keeps running. An instance without a key is 400, and a data key whose
wrapping version is disabled cannot be unwrapped, so it is refused naming the key.

**Backups' keys (#1956).** A backup of a CMEK instance inherits its key and is wrapped with the
key's *latest primary version* when it is taken ("Automated and manual backups use the latest
primary key version"), even when the instance itself is still on an older one: the instance's data
key is wrapped again for the backup, which answers `diskEncryptionConfiguration.kmsKeyName` and
`diskEncryptionStatus.kmsKeyVersionName` (a backup run) or `kmsKey` and `kmsKeyVersion` (the
project's Backups API). `reencrypt` alone leaves the backups on their versions, as on Cloud SQL
("Backups aren't automatically re-encrypted with the new key version when you re-encrypt your
instance"). With `backupReencryptionConfig` it re-encrypts the instance and then wraps its backups of
`backupType` (`AUTOMATED`, the default, or `ON_DEMAND`) again with the primary version, newest
first, at most `backupLimit` of them (0: every one; Google documents the field only as "Backup
re-encryption limit"). A backup whose version is disabled or destroyed cannot be restored
(`restoreBackup`, to the instance or a new one, is 400 naming the version) nor re-encrypted (the
whole call is refused and nothing changes), as Google documents that the backup's data is lost
while its key version is gone. The dump files themselves are not encrypted with the key, as the
data volume is not.

### Backup locations (#1956)

A backup is kept in a location: the region or multi-region (`us`, `eu`, `asia`) that an on-demand
backup names (`backupRuns.insert`'s and `backups.create`'s `location`, `gcloud sql backups create
--location`), else `settings.backupConfiguration.location` (`gcloud sql instances patch
--backup-location`, Terraform's `backup_configuration.location`, the console's Edit **Backup
location**), else the multi-region nearest the instance, `us` for `us-central1`, as Cloud SQL's
default is. Every backup is still a dump on this host: the location is recorded and answered, and
the console's Backups page shows it ("Multi-region: us", "Region: europe-west1"). A location that is
no region or multi-region is 400. A chosen location other than the instance's region must be
allowed by `gcp.resourceLocations` or the backup fails ("Backups fail if they are in a region the
organization policy does not allow"); one in the instance's region always succeeds. Changing the
configured location does not move the backups already taken.

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
  `super_read_only` on, so a write is refused by the server. Its `rootPassword` is the
  primary's, and so is its `cloudsqladmin` password (#1993), made on the replica itself.

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

**Demoting an instance** (#1524), the reverse direction, for MySQL: a standalone instance
(not a replica, with no replicas of its own, not `REGIONAL`) becomes a read replica of the server
a source representation instance records. Its server restarts as a replica (a server ID of its
own, GTIDs on, read-only); the source is verified first, as for `startExternalSync`.

- **`demoteMaster`** (`DEMOTE_MASTER`), with `demoteMasterContext.masterInstanceName` the source
  representation instance and optionally `replicaConfiguration.mysqlReplicaConfiguration`'s
  `username` and `password` (else the source representation instance's), keeps the instance's
  data and replicates from the source by GTID auto-positioning. `verifyGtidConsistency`, true
  unless sent false, refuses it 400 when the instance has executed a transaction the source has
  not (`GTID_SUBSET`).
- **`demote`** (`DEMOTE`), with `demoteContext.sourceRepresentativeInstanceName`, is Database
  Migration Service's step for a migration to an existing instance: the instance is left a
  replica waiting for `startExternalSync` (as the source representation instance's `username` and
  `password`), which dumps the source's databases into it and starts
  replicating. Its own databases that the source has not stay.

`promoteReplica` makes it a primary again. In the console, a standalone MySQL instance's page has
**Demote** (either method); gcloud and Terraform have no demote command or resource.

**DR replicas and switchover** (#1614), for MySQL, and for PostgreSQL 12 and later (#1524), as
Cloud SQL documents advanced disaster recovery for an Enterprise Plus primary
(docs.cloud.google.com/sql/docs/mysql/use-advanced-disaster-recovery and
docs.cloud.google.com/sql/docs/postgres/use-advanced-disaster-recovery):

- **Designating a DR replica**: `instances.patch` of the primary with
  `replicationCluster.failoverDrReplicaName` (the replica's name, or `PROJECT:NAME`), as
  `gcloud sql instances patch --failover-dr-replica-name` and Terraform's `replication_cluster`
  send it. Both instances must be Enterprise Plus (`settings.edition`; Cloud SQL's default is
  Enterprise Plus for MySQL 8.4 and PostgreSQL 16 to 18, Enterprise for MySQL 8.0 and
  PostgreSQL 12 to 15), and the replica a direct (not cascading),
  non-external read replica of the primary in another region, with none of the `replicate_*`
  flags; otherwise 400. A designation on a replica, or on a new instance, is 400. The primary then
  answers `replicationCluster.failoverDrReplicaName` as `PROJECT:NAME`, and the replica
  `replicationCluster.drReplica` true. Designating another replica moves it; `""`, or
  `replicationCluster` sent null (`--clear-failover-dr-replica-name`), removes it, and so do
  deleting or promoting the replica. While designated, the replica's binary logging cannot be
  turned off (`backupConfiguration.binaryLogEnabled` false, `skip_log_bin`), 400.
- **`instances.switchover`** (`SWITCHOVER`), called on the DR replica, with `dbTimeout` (default
  10 minutes, at most 24 hours), reverses the roles with no data lost: the primary is made
  read-only, the replica waits for every transaction the primary has executed
  (`WAIT_FOR_EXECUTED_GTID_SET`, bounded by `dbTimeout`) and is promoted, and the old primary then
  replicates from it by GTID auto-positioning, keeping its data, as `demoteMaster`'s replica does.
  Both servers are changed online, without a restart. Afterwards the new primary is
  `CLOUD_SQL_INSTANCE` with the old one in `replicaNames` and as its DR replica
  (`failoverDrReplicaName`), and the old primary is `READ_REPLICA_INSTANCE` with
  `masterInstanceName` the new primary, `drReplica` true and automated backups off. A switchover
  back reverses it again. If the replica does not catch up within `dbTimeout`, the primary takes
  writes again and the operation fails.
- **PostgreSQL switchover** (#1524): the primary is made read-only (`default_transaction_read_only`,
  reloaded), its client sessions are ended, so no write is in flight, and it checkpoints; the
  replica waits until it has replayed the primary's WAL up to there (`pg_last_wal_replay_lsn`,
  bounded by `dbTimeout`) and is promoted (`pg_promote`, a new timeline). A PostgreSQL server
  that has diverged from another becomes its standby only from a copy of its data directory, so
  the old primary's server is started again and its data replaced by a `pg_basebackup` of the
  new primary (an epoch recorded beside the data directory, `rebuildEpoch`, says it was rebuilt,
  so a restart keeps its data), and it streams from the new primary as a hot standby, as
  Google's "the original primary is reconfigured as a read replica of the new primary". The
  instances then answer as for MySQL. If the replica does not catch up, the primary takes writes
  again; if the old primary cannot be rebuilt, the new primary stays the primary and the old one
  is `FAILED`, naming why. The primary's other read replicas (#1614) stay its replicas, so they
  are cascading replicas of the new primary through it, as a MySQL primary's are; Google's DR
  pages say the old primary is "reconfigured as a read replica" and nothing of its other
  replicas, which keep their `masterInstanceName`. Their WAL went on past the point the DR
  replica was promoted at (the old primary's shutdown checkpoint reaches them as its server
  stops to be rebuilt), so they cannot follow the new timeline: each is `MAINTENANCE` during the
  switchover and, once the old primary serves again, rebuilt from a `pg_basebackup` of it, its
  own cascading replicas after it from them; a stopped replica (`stopReplica`) stays stopped.
- **Refused**: switchover of an instance that is not its primary's DR replica (400, also when no
  DR replica is designated), of a primary (400), while either instance is not `RUNNABLE` (409), or
  while the replica's replication is stopped (400); a REGIONAL primary's DR replica and switchover
  are 501 (the old primary would be a read replica, which here is one server); a switchover of a
  PostgreSQL primary while one of its other read replicas is not `RUNNABLE` is 409.
- **Not as Google**: there is no write endpoint (`psaWriteEndpoint`, a DNS name that follows the
  primary): clients connect to the new primary by its own connection name. No backup is taken
  of the new primary after the switchover, and point-in-time recovery is not required of the
  primary. A primary's other read replicas stay its replicas, so they are cascading replicas of
  the new primary (a PostgreSQL primary's are rebuilt, above).
- **gcloud**: `gcloud sql instances patch --failover-dr-replica-name` and
  `--clear-failover-dr-replica-name`, and `gcloud sql instances switchover --db-timeout`. In the
  console, a MySQL or PostgreSQL primary with replicas has **Designate DR replica** and **Remove DR replica**,
  and its DR replica's page **Switchover**, confirmed by typing the primary's instance ID.

**Blue-green deployments** (#1614), for MySQL, as Cloud SQL documents them
([about](https://docs.cloud.google.com/sql/docs/mysql/about-blue-green-deployments),
[create](https://docs.cloud.google.com/sql/docs/mysql/create-blue-green-deployment),
[switchover](https://docs.cloud.google.com/sql/docs/mysql/switchover-blue-green-deployment)):

- **`blueGreenDeployments.create`** (`CREATE_BLUE_GREEN_DEPLOYMENT`) in the blue instance's region
  makes the green instance, `BLUE-green-XXXXXXXX` (the blue name truncated to 48 characters and 8
  hex digits), a read replica of the blue one with `requestedConfig.databaseVersion` applied
  (MySQL 8.0 to `MYSQL_8_4` only). The blue instance must be a RUNNABLE MySQL primary with
  automated backups and binary logging and no read replicas (400 otherwise). Here a green name
  longer than 55 characters (a blue name over 40) is 501: the server's Kubernetes names hold it.
- **The state**: `PROVISIONING` while the green instance starts, then `SWITCHOVER_READY` while
  its replication runs (both threads `ON`), and `SWITCHOVER_NOT_READY` with `errorDetail` when it
  is stopped or broken, or either instance is gone. `deploymentMappings` holds the one pair; the
  `DETAILED` view adds its `diffs` (`database_version`, `tier`, `edition`, `availability_type`).
  Both instances answer `deploymentInfo` (`PRE_SWITCHOVER`, `POST_SWITCHOVER`).
- **`switchover`** (`SWITCHOVER_BLUE_GREEN_DEPLOYMENT`) makes the blue instance read-only, waits
  up to a minute for the green one to apply its GTIDs (else it fails, "replication lag is too
  high", and the blue instance takes writes again), promotes the green one, and makes the blue
  one writable. The blue instance's name, connection name and backups then belong to the green
  server, and the green name to the old blue server, a standalone read and write instance:
  each record names its server (`Spec.Server`), so neither server restarts.
- **A `REGIONAL` blue instance** (#1614): the green instance is "a clone of the blue
  environment", so it is `REGIONAL` too, with a primary and a secondary zone: its primary server
  replicates from the blue instance's primary (through the instance's Service, so it follows a
  failover of the blue instance), and its standby, in the secondary zone, from it,
  semi-synchronously, as a `REGIONAL` instance's standby does (the standby's user is created
  with the replica's binary log off, so the green instance has no transaction the blue one
  lacks). After the switchover both names keep their standbys, and `instances.failover` of
  either moves its zones with no committed row lost. Google's pages do not say how a green
  instance's availability is set; they call it a clone, and its diffs list `availability_type`.
  Google documents blue-green deployments for MySQL only: "Cloud SQL for PostgreSQL and Cloud
  SQL for SQL Server aren't supported", so a PostgreSQL source is 400.
- **`delete`** before a switchover deletes the green instance; after one it deletes the record,
  and with `deleteOldSource` the old blue server (the green name) too.
- In the console, a MySQL primary's page has **Create blue-green deployment**, and either
  instance's page the deployment's role and state, **Switch over blue-green** and **Delete
  blue-green deployment**.

**A physical sync** (#1570), for MySQL: `startExternalSync` with `migrationType` `PHYSICAL`
gives the replica a Percona XtraBackup backup of the source instead of a dump, and then
replicates by GTID auto-positioning from the backup's GTID set (`xtrabackup_binlog_info`).

- **The backup** is the folder `xtrabackup --backup --target-dir` writes, uploaded to Cloud
  Storage, and the source representation instance's `dumpFilePath` names the folder
  (`gs://BUCKET/FOLDER`). A replica created with a folder that holds `xtrabackup_checkpoints` is
  not seeded from it as from a dump: it waits for the physical sync. With no `dumpFilePath`, and a
  source that is a CloudBurrow instance's server (`sqlinst-NAME:3306`, or `cloudsql-mysql:3306`),
  the sync takes the backup itself, with the source's volume mounted and the replication user.
- **How it runs** on a persistent instance: a Kubernetes Job of the official multi-arch `percona/percona-xtrabackup` image
  of the replica's major version (`8.0`, XtraBackup 8.0.35, or `8.4`, 8.4.0; pinned by digest, and
  pulled by the node the first time), on the replica's volume while its server is stopped: the
  backup's files are first written onto that volume, then `xtrabackup --prepare` and
  `--copy-back` replace the data directory, without the source's `auto.cnf` (its server UUID)
  and `mysqld-auto.cnf`. The server starts on the copy with an `--init-file` that sets `root`'s
  password back to the instance's (the copy has the source's accounts), unlogged.
- **On an ephemeral instance** (`--mode ephemeral`, #1614) the server `up` runs for
  `--services cloudsql-mysql` keeps its data in its container's writable layer, with no volume,
  so that Job, which mounts the source's volume, cannot take its backup. Instead the replica's
  server, idle and empty until the sync, is started again on its volume (a created instance's
  server has one in either mode, #1740) with a start that swaps a restored data directory in
  before mysqld starts and passes the `--init-file`. XtraBackup runs in an ephemeral container
  (Kubernetes' debug containers) added to the replica's pod with that volume mounted: it
  prepares the backup and copies it back beside the live data directory, then mysqld is stopped
  and its container, started again, swaps the copy in. When the sync takes the backup, a second
  ephemeral container is added to the source server's pod, in its process namespace: the
  source's data files are reached through its mysqld's `/proc/PID/root`, linked at
  `/var/lib/mysql` so the paths the server reports hold, and `xtrabackup --backup
  --stream=xbstream` over the pod's loopback streams the backup through the host into the
  replica's ephemeral container, which unpacks it (`xbstream -x`). Ephemeral containers stay in
  their pod's spec, ended, until the pod goes. The restored data is on the replica's volume, so
  it is there after the replica's server restarts, as any created instance's is.
- **`verifyExternalSyncSettings`** with `migrationType` `PHYSICAL` answers
  `UNSUPPORTED_MIGRATION_TYPE` (a PostgreSQL replica, or `syncMode` `OFFLINE`),
  `CONNECTION_FAILURE`, `INCOMPATIBLE_DATABASE_VERSION` (the source's or the backup's
  `xtrabackup_info` `server_version` of another major version), `INCOMPATIBLE_DATABASE_MINOR_VERSION`
  (a source newer than the replica's server, which cannot open its files), `BINLOG_NOT_ENABLED`,
  `UNSUPPORTED_GTID_MODE`, `UNSUPPORTED_BINLOG_FORMAT`, `UNSUPPORTED_DATABASE_SETTINGS`
  (`innodb_page_size` other than 16384, `lower_case_table_names` other than 0, which a physical
  copy keeps), `UNSUPPORTED_STORAGE_ENGINE` (a table not in InnoDB), `INVALID_FILE_INFO` (a folder
  without `xtrabackup_checkpoints`, `xtrabackup_info` or `xtrabackup_binlog_info`, an unextracted
  `.xbstream` or a qpress backup, or no `dumpFilePath` for a source outside CloudBurrow),
  `INSUFFICIENT_PRIVILEGE` and `REPLICA_ALREADY_SETUP`. The replication user needs
  `REPLICATION SLAVE`, `REPLICATION CLIENT` and `SELECT` (so every table's engine can be seen),
  and, when the sync takes the backup, XtraBackup's `RELOAD`, `PROCESS`, `BACKUP_ADMIN` and
  `SELECT` on `performance_schema.log_status`. A zstd-compressed backup is decompressed first.
- In the console, a MySQL replica's **Verify external sync settings** and **Start external sync**
  have a **Migration type**, `LOGICAL` or `PHYSICAL`.

`demoteMaster` with `skipReplicationSetup` (#1614) demotes the instance with its data but sets up
no replication: it installs Cloud SQL's stored procedures in the server's `mysql` schema instead,
and the replication is set up with them as Cloud SQL's custom import guide does, after an import
whose GTIDs are the instance's own (so the GTID consistency check is not made):
`CALL mysql.resetMaster()`, `CALL mysql.skipTransactionWithGtid('<the source's executed GTIDs>')`,
`CALL mysql.setupExternalSourceAutoPosition('host', 3306, 'user', 'password', 1, false, false)`
(or `mysql.setupExternalSource` with a binary log file and position) and
`CALL mysql.startReplication()`, which also makes the replica `super_read_only`;
`mysql.stopReplication()` stops it. `promoteReplica` removes them. Both are made outside the
binary log, so they give the instance no GTIDs of its own. The bodies are CloudBurrow's
(the guide gives the calls); `USE_SSL_CLIENT_AUTH` is refused and `mysql.skipReplicationError` is
not installed. The console's Demote has **Skip replication setup**.

`demote` of a PostgreSQL instance (#1614) leaves it a replica of the source representation
instance's server, waiting for `startExternalSync`, which first drops the instance's databases
named as the migrated ones, as Database Migration Service documents for an existing destination,
then copies and subscribes as for any PostgreSQL replica of an external server; its other
databases stay. The console's Demote is on a PostgreSQL instance's page too.

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
| `MYSQL_5_6` | `mysql:5.6`, its linux/amd64 manifest (see below) | `MYSQL_5_6_51` |
| `MYSQL_5_7` | `mysql:5.7`, its linux/amd64 manifest (see below) | `MYSQL_5_7_44` |
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

**MySQL 5.7 (#1524)** is in extended support on Cloud SQL. Its official image, `mysql:5.7`, was last
built in 2023-12 (5.7.44), and only for linux/amd64. CloudBurrow pins that platform's manifest by
digest, so a linux/arm64 node can pull it too. A `MYSQL_5_7` instance is created where the
cluster's node runs linux/amd64 binaries: natively, or through an x86-64 `binfmt_misc` handler
registered with the `F` flag. Docker Desktop on Apple silicon registers Rosetta and
`qemu-x86_64` that way (measured 2026-11-18). The node is asked once. On a node that cannot run
the image, `instances.insert` is 501 and names why. The server starts with `ulimit -n 1048576`,
because mysqld 5.7 sizes its tables from the open-files limit, and containerd's default made it
OOMKilled.

A MySQL 5.7 instance serves its server, databases, users, database flags, restarts and
connections, backups and their restores, `instances.clone` and restores to a new instance (their
`mysqldump` steps are 8.0's, with the accounts from `SHOW CREATE USER`), and a major version
upgrade to `MYSQL_8_0` (#1524). Cloud SQL upgrades MySQL one major version at a time ("You can't
upgrade directly from MySQL 5.6 to MySQL 8.0 or from MySQL 5.7 to MySQL 8.4"), so its
`upgradableDatabaseVersions` is the next version only, and an upgrade that skips one is 400; the
console's Upgrade action offers the same. Read replicas (cascading ones too), `REGIONAL` and its
failover, `binaryLogEnabled` with clones to a point in time or to binary log coordinates,
`demote`/`demoteMaster`, external replicas synced with `mysqldump` and blue-green deployments
without a version change (Cloud SQL's "configuration staging", "MySQL 5.7 and later") are served
too (#1524): CloudBurrow's MySQL 8.0 steps run in 5.7's statements (`CHANGE MASTER TO`, `START
SLAVE`, `SHOW SLAVE STATUS`, `rpl_semi_sync_master`). A 5.7 or 5.6 server runs with its binary log
(`mysql-bin`, row format, `log_slave_updates`) and GTIDs on from its start, as every Cloud SQL for
MySQL instance has GTIDs and MySQL 5.6 turns them on only at startup, so GTID consistency is
enforced, as on Cloud SQL; its binary log starts afresh after its initialisation. What 8.0 keeps
with `SET PERSIST` is `SET GLOBAL` there: a REGIONAL instance's semi-synchronous replication is set
again when its server restarts. A point-in-time recovery stops at the first transaction committed
after the target by the binary log event's time, in seconds, as 5.7's and 5.6's GTID events carry no
commit timestamp (8.0.1 added it). A physical (XtraBackup) external sync stays 501: CloudBurrow's
XtraBackup images are 8.0's and 8.4's. `flags.list` answers no flags for it. The console's version
picker offers it, and its instance page has the same Create read replica, high availability, point-in-
time clone, Demote and Create blue-green deployment actions as 8.0's.

**MySQL 5.6 (#1524)** is served the same way. Its official image, `mysql:5.6`, was last built in
2021-12 (5.6.51), only for linux/amd64, and is pinned by that manifest's digest
(`docker buildx imagetools inspect mysql:5.6`, 2026-11-18). It starts under the same open-files
limit, and serves and refuses what a MySQL 5.7 instance does; its upgrade is to `MYSQL_5_7`. Two
more are 400, as Cloud SQL documents them: a blue-green deployment ("your instance must run Cloud SQL
for MySQL version 5.7, 8.0, or 8.4"), and a replica of a 5.6 read replica, as Cloud SQL logs a
replica's binary log on "MySQL 5.7 and later only"; the console offers neither. MySQL 5.6 has no
`super_read_only`, so a 5.6 replica is `read_only`; no `WAIT_FOR_EXECUTED_GTID_SET`, so a failover
waits with `WAIT_UNTIL_SQL_THREAD_AFTER_GTIDS`; no lossless (`AFTER_SYNC`) semi-synchronous
replication, so a REGIONAL 5.6 instance's standby acknowledges after the commit; no TLS certificate
of its own, so its replicas connect in the clear; and no `CREATE USER IF NOT EXISTS`, so its
replication users are made with `GRANT ... IDENTIFIED BY`.
`users.update` changes a password with `SET PASSWORD FOR ... = PASSWORD(...)`, because MySQL 5.6's
`ALTER USER` only expires one. MySQL 5.6 has no `SHOW CREATE USER`: a backup, a clone and an upgrade
carry each account as `CREATE USER ... IDENTIFIED BY PASSWORD` with its password hash, which 5.6
and 5.7 both read, and its `SHOW GRANTS` without that clause.

`databaseInstalledVersion` is read from the server. `maintenanceVersion` and
`availableMaintenanceVersions` are not answered: they name Cloud SQL's own releases
(`POSTGRES_17_11.R20250727.00_00`), which no server here is.

What is 501, each checked with `docker buildx imagetools inspect` on 2026-10-16 rather than assumed:

| `databaseVersion` | Why |
|---|---|
| `MYSQL_5_7`, `MYSQL_5_6` | Only on a node that runs no linux/amd64 binaries (see above): `mysql:5.7` and `mysql:5.6` are linux/amd64 only |
| `MYSQL_8_0_18` to `MYSQL_8_0_4x` | one 8.0 minor runs, `MYSQL_8_0` (8.0.46, the official image's); the 501 says to use it |
| `SQLSERVER_*` | Served once its user accepts Microsoft's licence (412 until then), on a node that runs linux/amd64 images: see [Cloud SQL for SQL Server](#cloud-sql-for-sql-server) |

A created primary or read replica answers `upgradableDatabaseVersions`, every newer served version of its
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
new server. **A PostgreSQL replica is upgraded on its own too (#1524)**, as Cloud SQL documents:
"you must upgrade the read replicas before you upgrade the primary instance", and "The
replicated databases of an upgraded replica are available only after you upgrade the primary
instance." A PostgreSQL replica is a physical standby streaming its primary's WAL, which does not
cross major versions, so its `MAJOR_VERSION_UPGRADE` replaces its server with an empty one of the
newer version. The replica stays the primary's, `RUNNABLE`, at the new version, and replicates
nothing until the primary is upgraded to that version. The primary's upgrade, with or without
`includeReplicasForMajorVersionUpgrade`, then rebuilds it from the upgraded primary with
`pg_basebackup`, and it replicates again. The primary is not upgraded to an older version than a
replica upgraded on its own (400, with or without `includeReplicasForMajorVersionUpgrade`): its
rebuild would roll the replica back, and "After you upgrade a replica, you can't roll back the
database version of the replica" (upgrade-major-db-version-inplace, re-read 2026-11-18; the
refusal's wording is UNVERIFIED, re-read 2026-11-18). `includeReplicasForMajorVersionUpgrade` on the replica
does the same to its own cascading replicas. In between, `promoteReplica`, `stopReplica`,
`startReplica` and a `switchover` to it are 501: Google does not document what they do then.
An external replica (of a source representation instance) cannot be upgraded in place, as Cloud
SQL documents; that is 400 (the message is CloudBurrow's own). The console's instance page
offers **Upgrade** with the versions the instance can go to and **Include replicas**, and the
Create instance form's version picker lists every served version.

**The instances `up` runs stay as they are:** `postgres` and `mysql` cannot be patched,
restarted or deleted through the API; each answers 400 naming the `--services` flag that
starts and stops it.

**A created instance keeps its data while it exists, in either mode** (#1740). Its server has
a volume (a PersistentVolumeClaim of its own, deleted with the instance), so its users,
databases, procedures and rows are there after every restart Google makes without losing them:
a database-flag change, a tier or edition change, `instances.restart`, a failover, a demotion
or promotion. **What lasts past `stop` follows the instance's mode.** In persistent mode the
instance's record is kept in `cloudsql-instances.json` in the instance directory, so the
instance is there, with its data, after `stop` and `up`. In ephemeral mode no record is kept:
the next `up` removes every server an earlier run created, with its volume, and `delete`
removes the cluster and every volume in it. `TestCloudSQLInstanceWrites` checks that a user,
a database and a row are there after a flag change and after a restart, on both engines.

The console's **Cloud SQL instances** page lists every instance, with Create instance,
Delete and Restart; a created instance's page has **Edit instance** (tier, labels, database
flags, deletion protection, automated backups and their start time, sent as one
`instances.patch`).

### Backups, restores and clones (#1330)

`backupRuns.insert`, `list`, `get` and `delete`, `instances.restoreBackup` and
`instances.clone` are served, for created instances and for the servers `up` runs, so
`gcloud sql backups create`, `list`, `restore` and `delete` work.

- **A backup is a real dump**, taken with the mechanism `cloudburrow state save` uses for
  Cloud SQL: `pg_dump -Fc` of each database (or `mysqldump --single-transaction --routines
  --triggers --events` of each database but MySQL's own), run inside the server's pod, and
  kept on the host in `cloudsql-backups/<id>/` in the instance directory. A created
  PostgreSQL instance's `postgres` database is included, since Cloud SQL gives it to the
  application; on the server `up` runs it is the maintenance database and is left out.
- **A restore really restores**: `restoreBackup` drops every application database of the
  target and loads the backup's, ending the sessions connected to them, so the rows written
  after the backup are gone. `restoreBackupContext.instanceId` may name another instance of
  the same engine, as on Cloud SQL. **A created MySQL instance's backup has its users**
  (#1993): Cloud SQL's restore "takes the source instance's settings, databases, and users"
  ([restore](https://docs.cloud.google.com/sql/docs/mysql/backup-recovery/restore)), so a
  restore, a clone or a restore to a new instance drops the target's accounts and gives it the
  source's, with their passwords and grants (a deleted `'root'@'%'` stays deleted), and its
  IAM users; the system accounts are the target's own. `PROXY` grants and MySQL 5.6's
  accounts are not carried, and a backup taken before #1993 leaves the target's accounts as
  they are. PostgreSQL's roles and the MySQL server `up` runs keep the target's users. A MySQL
  backup runs as `cloudsqladmin` (`root` on `up`'s server, with the password it was started with).
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
  window. The log lives on the server's volume, so a restart of the server keeps it, in either
  mode, and deleting the instance deletes it.
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
  time before the failover recovers from a base taken after it, or is 400). The standby has a
  volume of its own in either mode, so a restart of its server keeps its data.

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
`cloudburrow env` exports it as `CLOUDBURROW_CLOUDSQL_CONNECTOR_ENDPOINT`. `up` records the
address in the instance's state directory (`cloudsql-connector.json`), so
`cloudburrow env --name … --state-dir …` reports it without the flag being passed again (#1571);
an `up` at the default removes the record.

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
  "${GOOGLE_CLOUD_PROJECT}:us-central1:postgres?port=6432" "${GOOGLE_CLOUD_PROJECT}:us-central1:mysql?port=6306"
psql "host=127.0.0.1 port=6432 user=cloudburrow dbname=cloudburrow sslmode=disable"
```

`--sqladmin-api-endpoint` is the Admin API's URL, which `cloudburrow env` exports as
`CLOUDSDK_API_ENDPOINT_OVERRIDES_SQL`, and the project is the `GOOGLE_CLOUD_PROJECT` it exports.
Keep the braces: in zsh, macOS's default shell, an unbraced variable followed by `:us-central1`
takes `:u` as its (uppercase) modifier, so a project `demo-a` gives `DEMO-As-central1:postgres`,
which the proxy refuses as a connection name. Any `--token` will do, as tokens are not checked; pass one,
so the proxy does not go looking for Google credentials. `--auto-iam-authn` signs in as an IAM user, with a token CloudBurrow minted, by the path the Go
connector's `WithIAMAuthN` takes (see [IAM database authentication](#iam-database-authentication-1944);
the proxy itself has not been run with it). The proxy then
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
log coordinates. CI's sql shard installs it and runs the test. Connecting to the server directly, as above, still
works for built-in users and needs none of this.

### IAM database authentication (#1944)

As Google documents it for PostgreSQL and MySQL
([iam-authentication](https://docs.cloud.google.com/sql/docs/postgres/iam-authentication),
[add-manage-iam-users](https://docs.cloud.google.com/sql/docs/mysql/add-manage-iam-users),
[iam-logins](https://docs.cloud.google.com/sql/docs/mysql/iam-logins)), and decided in
[ADR-0004](adr/0004-local-access-and-no-authentication.md)'s amendment of 2026-11-19:

- `users.insert` of type `CLOUD_IAM_USER` or `CLOUD_IAM_SERVICE_ACCOUNT`, with the principal's email
  as `name`, creates a database user named as Google names it. PostgreSQL: the email in lower case,
  a service account's without `.gserviceaccount.com` (`sa@project.iam`), a member of
  `cloudsqliamuser` or `cloudsqliamserviceaccount`. MySQL: the part before the `@` (at most 32
  characters), with the email answered as `iamEmail`. It has no privileges until you `GRANT` them.
- The instance's flag must be on: `cloudsql.iam_authentication` (PostgreSQL) or
  `cloudsql_iam_authentication` (MySQL), set on an instance created through the Admin API. It is kept
  in the settings, never passed to the server, and changing it restarts nothing. The servers `up`
  runs have no flags, so they take no IAM sign-in.
- A sign-in needs an access token **this instance minted** (IAM Credentials `generateAccessToken`, the
  metadata server, the ADC fixture's token endpoint), still live, for the user's own principal,
  which holds `cloudsql.instances.login` on the instance (`roles/cloudsql.instanceUser`). The token
  is checked by its SHA-256 against the instance's log of minted tokens, never with Google.
- Through the connector listener: the Go connector's `WithIAMAuthN` and the Auth Proxy's
  `--auto-iam-authn` send the token to `generateEphemeralCert`, whose certificate carries its hash
  (a URI SAN, `urn:cloudburrow:cloudsql:iam-token-sha256:…`) and expires with it; or the client
  sends the token as its password (PostgreSQL's cleartext password; MySQL's `mysql_clear_password`,
  the `mysql` client's `--enable-cleartext-plugin`, go-sql-driver's `allowCleartextPasswords`). The
  listener then signs in to the server as the user with a password of the user's own, derived from
  a key kept owner-only in the state directory (`cloudsql-iam-role-key`). PostgreSQL's
  `pg_hba.conf` requires that password of the two IAM groups ahead of its trust line, so the
  server's own port refuses an IAM user whatever is sent.
- `executeSql` with `autoIamAuthn` signs in as the caller's IAM user, the account of the request's
  bearer token, checked the same way; `user` and `passwordSecretVersion` are then ignored.
- A refused sign-in is the server's: PostgreSQL `FATAL 28000 Cloud SQL IAM service account
  authentication failed for user "…"`, MySQL `ERROR 1045 Access denied for user …`, with the reason.

Not served: `CLOUD_IAM_GROUP` (501), and a token sent as a password to the server's own port.

### Cloud Run's Cloud SQL volume (#1346)

A Cloud Run service or job that names an instance in a `cloudSqlInstance` volume gets what Cloud
Run gives it: a Unix socket at `<mountPath>/<connection name>`, PostgreSQL's at
`/cloudsql/<connection name>/.s.PGSQL.5432`. An application connects as it would on Cloud Run:

```sh
psql -h "/cloudsql/${PROJECT}:us-central1:postgres" -U cloudburrow -d cloudburrow
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
  --add-cloudsql-instances="${GOOGLE_CLOUD_PROJECT}:us-central1:postgres"
```

MySQL's socket is `/cloudsql/<connection name>` itself:

```sh
mysql -S "/cloudsql/${PROJECT}:us-central1:mysql" -u cloudburrow cloudburrow
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

## 8. Cloud SQL for SQL Server

<a id="cloud-sql-for-sql-server"></a>
Cloud SQL for SQL Server (#1916) runs **Microsoft's SQL Server container image**,
`mcr.microsoft.com/mssql/server` of the instance's release (2017, 2019, 2022 or 2025), pinned by
the digest of each tag's one linux/amd64 manifest. Every version Google lists is accepted:
`SQLSERVER_2017_*`, `SQLSERVER_2019_*` and `SQLSERVER_2022_*` in `EXPRESS`, `WEB`, `STANDARD` and
`ENTERPRISE`, and `SQLSERVER_2025_EXPRESS`, `STANDARD` and `ENTERPRISE` (Google lists no 2025 Web).
SQL Server instances are created through the Cloud SQL Admin API, which is served with
`--services cloudsql` or `cloudsql-mysql`.

### You accept Microsoft's licence, nobody else

The image starts only with `ACCEPT_EULA` set, which "confirm[s] your acceptance of the
[End-User Licensing Agreement](https://go.microsoft.com/fwlink/?LinkId=746388)"
([Microsoft](https://learn.microsoft.com/sql/linux/configure/environment-variables)). On Cloud SQL
Google holds the licence; here **the user who provisions SQL Server accepts it**, and CloudBurrow
never sets it by default or on anyone's behalf, its tests and CI included. You accept it either:

- **for every SQL Server instance** this CloudBurrow instance creates: `cloudburrow up
  --accept-mssql-eula`, or `CLOUDBURROW_ACCEPT_MSSQL_EULA=Y` (exactly `Y`). A configuration file
  cannot accept it: a repository can ship `./cloudburrow.json`, and you have not necessarily read it.
- **for one instance**: the console's Create instance page, SQL Server engine, check
  *I accept Microsoft's licence terms for SQL Server*, which links Microsoft's End-User Licensing
  Agreement and [the editions' licensing terms](https://www.microsoft.com/licensing/terms/productoffering/SQLServer).
  It sends `X-CloudBurrow-Accept-MSSQL-EULA: Y` with that one `instances.insert`.

Who accepted (the user running CloudBurrow), when, and how is recorded on the instance
(`mssqlEulaAcceptance` in `cloudsql-instances.json`), and `cloudburrow status` shows it, for `up` and
for each SQL Server instance (`mssql_eula` in `status --format json`). The server is given
`ACCEPT_EULA=Y` only for an instance with that record.

Without an acceptance, `instances.insert` of a `SQLSERVER_*` version is **412 FAILED_PRECONDITION**
naming the licence and how to accept it, and nothing is created. UNVERIFIED (re-read 2026-11-19,
docs.cloud.google.com/sql/docs/sqlserver/create-instance): Google documents no such refusal, as it
holds the licence; 412 FAILED_PRECONDITION is what this API answers for another missing precondition
of the local server (an engine whose service is not enabled), and the wording is CloudBurrow's.

### Which edition runs locally, and why

Cloud SQL licenses Standard, Enterprise and Web through Google. Locally nobody holds those licences,
so those editions run Microsoft's free **Developer** edition, which has Enterprise's features and is
licensed for development and test only, which is what a local emulator is for. Do not use these
instances in production. `MSSQL_PID`, per Microsoft's
[edition table](https://learn.microsoft.com/sql/linux/configure/environment-variables#sql-server-editions):

| Google edition | 2017, 2019, 2022 | 2025 |
|---|---|---|
| `EXPRESS` | `Express` (free for any use) | `Express` |
| `WEB` | `Developer` | (not offered) |
| `STANDARD` | `Developer` (Enterprise's features: a Standard instance here allows features Standard lacks) | `StandardDeveloper` (Standard's features) |
| `ENTERPRISE` | `Developer` | `EnterpriseDeveloper` |

The console's version picker names the local edition beside each version.

### linux/amd64 only

Microsoft supports the image only on x86-64 hosts: "Emulation or translation environments (for
example, Rosetta 2, Prism, or QEMU) aren't tested or supported"
([Microsoft](https://learn.microsoft.com/sql/linux/install-upgrade/quickstart-install-docker)). As for
MySQL 5.7 and 5.6, an instance is served where the cluster's node runs linux/amd64 binaries, natively
or through the binfmt_misc emulation Docker Desktop registers on Apple silicon (*Use Rosetta for
x86_64/amd64 emulation*, or qemu-user-static), which the provisioner checks on the node; elsewhere it
is 501 saying why. Under emulation it runs unsupported by Microsoft, and slower.

### What a SQL Server instance serves

- `instances.insert` (with `rootPassword`, required, as `gcloud sql instances create --root-password`
  is for SQL Server), `get`, `list`, `patch` of its database flags, `restart`, stop and start, `delete`.
- **The `sqlserver` user**, with `rootPassword`, a member of the `CustomerDbRootRole` server role, as
  on Cloud SQL. UNVERIFIED (re-read 2026-11-19, docs.cloud.google.com/sql/docs/sqlserver/users): the
  role's exact permissions; it is granted `CREATE ANY DATABASE`, `ALTER ANY LOGIN`, `ALTER ANY SERVER
  ROLE`, `ALTER ANY CONNECTION`, `VIEW ANY DATABASE`, `VIEW ANY DEFINITION`, `VIEW SERVER STATE`,
  `ALTER TRACE` and `ALTER SERVER STATE`. CloudBurrow reaches the server as `sa`, with a password it
  generates, keeps in the record and never answers.
- **Databases**: `databases.insert` (with `collation`), `list`, `get`, `patch` of the collation,
  `delete`; the system databases are not listed and cannot be deleted; `charset` is 400.
- **Users**: `users.insert` (a SQL login in `CustomerDbRootRole`), `list`, `update` of the
  password, `delete`.
- **Flags**: [Google's SQL Server flags](https://docs.cloud.google.com/sql/docs/sqlserver/flags),
  checked against Google's ranges (another flag, or a value out of range, is 400), answered by
  `flags.list`. sp_configure options are set with `sp_configure` and `RECONFIGURE`, trace flags with
  `DBCC TRACEON(n, -1)` at each start; Cloud SQL's own settings (`cloud sql xe *`, `cloudsql enable
  linked servers`, `cloud sql enable polybase`, `cloud sql enable sp_help_revlogin`) are recorded only.
- **The Cloud SQL connectors and the Auth Proxy**: the instance's tunnel is SQL Server's port 1433,
  reached through the connector listener at 3307 like the other engines.

501, saying why: read replicas, `REGIONAL` high availability, backups (on demand, automated and
final), restores, point-in-time recovery, clones, imports and exports, major version upgrades,
demotion, blue-green deployments, source representation instances and customer-managed keys.
CloudBurrow's steps for those are `pg_dump`, `mysqldump` and PostgreSQL's and MySQL's statements,
and none is built yet for SQL Server's `BACKUP DATABASE` and its replication.

### Running the SQL Server tests

The compat tests that start SQL Server run only for a person who has accepted Microsoft's licence.
To run them, accept it yourself for both the instance and the test:

```sh
export CLOUDBURROW_ACCEPT_MSSQL_EULA=Y   # you accept Microsoft's SQL Server licence
cloudburrow up --name mssql --services cloudsql,cloudsql-mysql,storage --state-dir /tmp/mssql-state
eval "$(scripts/compat-env.sh mssql 2>/dev/null)"
go test -tags compat -run 'TestCloudSQLServer' -v ./test/compat/
```

`TestCloudSQLServerInstance` then creates a `SQLSERVER_2022_EXPRESS` instance (its first pull is
about 460 MB), checks it becomes `RUNNABLE`, makes a database and a user, and queries the server as
`sqlserver` through the official Go connector, reading back a database flag. Without the variable it
skips, and `TestCloudSQLServerRefusedWithoutEULA` checks the refusal instead; the unit tests in
`internal/service/sqladmin`, `internal/config` and `cmd/cloudburrow` always check the refusal and that
`ACCEPT_EULA` is never set without an acceptance on record.

## Managed connection pooling

`settings.connectionPoolConfig.connectionPoolingEnabled` on a Cloud SQL Enterprise Plus PostgreSQL instance runs a real pooler, PgBouncer 1.26.1 (ISC licence, the pinned `edoburu/pgbouncer` image), in a container of the instance's pod, as Google documents (<https://docs.cloud.google.com/sql/docs/postgres/managed-connection-pooling>, read 2026-11-22):

- Port 6432 is the pooler, 5432 stays the server's. The Cloud SQL connectors and the Auth Proxy (3307) reach the pooler while pooling is on. The pooler's host address is logged when it starts (`... pools connections: its pooler (port 6432) is at 127.0.0.1:PORT`).
- A user signs in with the password it has on the server (SCRAM or MD5): the pooler reads it from the server's `pg_shadow`.
- The flags are Google's, with Google's defaults: `pool_mode` (`transaction`, the default, or `session`), `max_pool_size` (50), `min_pool_size` (0), `max_client_connections` (5000), `max_prepared_statements` (0), `client_connection_idle_timeout` (0), `server_connection_idle_timeout` (600), `query_wait_timeout` (120), `server_lifetime` (3600) and `ignore_startup_parameters`. The three timeouts take 0 to 2147483 seconds. An unknown flag, a repeated one or a value out of range is 400.
- `poolerCount` (output only) is the tier's vCPUs divided by 4, at least 1. One PgBouncer process serves them all here.
- Turning pooling on or off restarts the instance, as Google documents. So does changing a flag here, whether or not Google's does (the page does not say).
- In transaction mode, Google's limitations are PgBouncer's: `SET`, `LISTEN`, session advisory locks and prepared statements without `max_prepared_statements` do not carry across transactions.

An Enterprise instance is refused 400. SQL Server is refused 400.

A VM reaches the pooler on 6432 at the instance's private IP and at its Private Service Connect endpoints, beside the server's 5432: both ports are in the VM's `private-services` NAT chain.

### Cloud SQL for MySQL

Cloud SQL for MySQL's managed connection pooling is, as Google documents it (<https://docs.cloud.google.com/sql/docs/mysql/managed-connection-pooling> and <https://docs.cloud.google.com/sql/docs/mysql/configure-mcp>, read 2026-11-22), "a server-side thread pool rather than an external connection multiplexer": "each open client connection still maintains a dedicated server session and counts toward the `max_connections` limit". The community MySQL server has no thread pool, so CloudBurrow runs one in front of it, in the instance's pod (`internal/mysqlpool`, run from the cloudburrow-storage image as `mysql-thread-pool`):

- The instance's Service's 3306 targets the thread pool, so every path a client takes goes through it: the host tunnel, the connectors and the Auth Proxy (3307), the private IP and the Private Service Connect endpoints. Clients keep connecting on 3306.
- Each client connection signs in to mysqld with a session of its own (the authentication exchange is relayed untouched, TLS included: the pool terminates the client's TLS with a certificate it makes at start, as mysqld does, and signs in to mysqld over TLS too).
- A statement runs only while it holds one of `max_pool_size` threads, from the command's first byte to the last packet of its answer, read by the MySQL client/server protocol (result sets, multiple results, prepared statements, cursors' fetches, `LOAD DATA LOCAL INFILE`). Between statements a connection holds no thread. A connection inside a transaction is given the next thread first. Replication sessions (`COM_BINLOG_DUMP`, `COM_REGISTER_SLAVE`) hold none.
- `max_pool_size` is the only flag, as Google documents. Its default "depends on the number of vCPU cores" (CloudBurrow's: one thread a vCPU, UNVERIFIED, re-read 2026-11-22); it takes a whole number from 1 (the page gives no range). Any other flag, PostgreSQL's included, is 400.
- Enterprise Plus only: an Enterprise instance is 400. Enabling it restarts the instance, as Google documents, and so does changing `max_pool_size` here. `thread_cache_size` defaults to 0 while it is on, unless a database flag sets it. `poolerCount` is not answered.
- Not built: the pool's metrics in Cloud Monitoring (`threads`, `pending_connection`, `avg_wait_time`); the pool serves its figures on a pod-local page only.

`TestCloudSQLMySQLManagedConnectionPooling` (compat, the official `sqladmin/v1` client and Cloud SQL Go connector for MySQL) shows it pools: with `max_pool_size` 2, eight clients at once have eight sessions (connection IDs), and the server, by its own clock (`SYSDATE(6)` at each statement's start and end), runs at most two of their half-second statements at a time; turned off, more than two run together.

The console has Connections' Enable Managed Connection Pool, with Advanced pooling options (MySQL's: `max_pool_size` alone), on the create page and in Edit. The Overview's Connect to this instance shows whether the instance pools connections.

`TestCloudSQLManagedConnectionPooling` (compat, the official `sqladmin/v1` client and Cloud SQL Go connector) shows it pools. Eight clients signed in through the connector share at most `max_pool_size` 2 server connections. With pooling off, each client has a server connection of its own. `TestCloudSQLPrivateIPAndPSCReachableFromAVM` (compat) shows a VM reaching the pooler of one instance at its private IP and of another at a Private Service Connect endpoint, each answering PostgreSQL's SSLRequest on 6432.

