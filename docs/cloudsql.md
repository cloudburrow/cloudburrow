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

What answers 501, saying why: IAM database authentication (CloudBurrow has no identities, ADR-0004), `pointInTimeRestore` (a Backup and DR datasource), failover
replicas and high availability, cascading, cross-region and external-primary replicas, the project-level Backups API, imports and exports, and SSL certificates. See
[coverage/sqladmin.md](coverage/sqladmin.md) for every method.

### Creating instances (#1330)

`instances.insert`, `patch`, `update`, `delete` and `restart` are served, so
`gcloud sql instances create` and Terraform's `google_sql_database_instance` work. Each
created instance is **a server of its own**: a Deployment in the cluster named
`sqlinst-<name>`, from the same image and the same component code as the server `up` runs
for `--services cloudsql` or `cloudsql-mysql`, with its own host tunnel, its own
`connectionName` (`{project}:us-central1:{name}`), its own server CA, and an address the
connectors reach through 3307.

| Field | What happens |
|---|---|
| `databaseVersion` | `POSTGRES_17` or `MYSQL_8_4`, the versions of the images CloudBurrow pins; the engine's service must be enabled (412 otherwise). Any other `POSTGRES_*` or `MYSQL_*` is 501 saying so; another value is 400. |
| `rootPassword` | MySQL's `root` password (empty leaves `root` with none, as Cloud SQL does). PostgreSQL's user is `postgres`, trusted locally like the server `up` runs. |
| `settings.tier` | Required, recorded and answered. The server is not sized by it. |
| `settings.databaseFlags` | Passed to the server: `-c name=value` to `postgres`, `--name=value` to `mysqld`. A patch that changes them starts the server again with them; a flag the server rejects fails the operation with its words. `cloudsql.*` flags (IAM authentication, Cloud SQL's extensions) are 501. |
| `settings.userLabels`, `deletionProtectionEnabled`, other settings | Recorded and answered. `delete` of a protected instance is 400, as on Cloud SQL. |
| `masterInstanceName` | A read replica: see [Read replicas](#read-replicas-1330). |
| `replicaConfiguration.failoverTarget`, `failoverReplica`, `availabilityType: REGIONAL`, `instances.failover` | 501, saying why (below). |

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
replica, of another project's or region's instance, or a failover replica is 501.
Point-in-time recovery is served for created instances (below).

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
  time (next section). A clone to `binLogCoordinates` and a clone of some databases only are 501.
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
  removes them. Deleting a created instance deletes its backups.

The operation is `BACKUP_VOLUME`, `RESTORE_VOLUME` or `CLONE`, `RUNNING` until the dump or the
load is done. The console's instance page has a **Backups** tab with **Create backup**,
**Restore**, **Delete backup** and **Clone**.

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
  `instances.pointInTimeRestore` is 501: it restores from a Backup and DR Service datasource
  (`projects/…/backupVaults/…/dataSources/…`), which is not served.

### High availability is 501 (#1330)

On Cloud SQL, `availabilityType: REGIONAL` makes an instance a primary and a standby in a second
zone of the region, its disk replicated synchronously to the standby, so a transaction is
committed only once both have it; `instances.failover` (and an outage) moves the instance to the
standby, which keeps the connection name and the IP address, with no committed transaction
lost. A standby built from CloudBurrow's read replicas would not be that: they replicate
asynchronously (streaming replication, GTID binary log replication), so a failover could lose
the transactions not yet replicated, and they run in one zone of one kind cluster, with their
own names. So `availabilityType: REGIONAL`, `failoverReplica`,
`replicaConfiguration.failoverTarget` and `instances.failover` are 501 and say this; use a
`ZONAL` instance and, to read elsewhere, read replicas.

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

Verified with `cloud.google.com/go/cloudsqlconn` on both servers, and with Google's Auth Proxy
(`cloud-sql-proxy` 2.19.0) in a pod, below. IAM database authentication (`WithIAMAuthN`, an
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
`FAILED_PRECONDITION`. Worker pools and instances refuse the volume, and so does v1's
`run.googleapis.com/cloudsql-instances` annotation. The console's deploy, edit and job forms
have a **Cloud SQL connections** field, which mounts the volume at `/cloudsql`, as
`gcloud run deploy --add-cloudsql-instances` does.

Verified for PostgreSQL by `TestRunServiceCloudSQLVolume` and `TestRunJobCloudSQLVolume`. MySQL
renders the same way, with its socket at `/cloudsql/<connection name>`, but no compat test runs
it yet.

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
