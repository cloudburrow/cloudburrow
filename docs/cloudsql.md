# Cloud SQL

Issue: [#121](https://github.com/cloudburrow/cloudburrow/issues/121) · Added 2026-09-22

**A real PostgreSQL running in CloudBurrow's cluster. Not the Cloud SQL Admin API.**

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

## 3. What you do not get

**None of the Cloud SQL Admin API.** No instances, no connection names, no
`gcloud sql instances create`, no IAM database authentication, no automatic backups, no read
replicas, no maintenance windows, no Auth Proxy path.

An application that talks SQL will work. An application that calls the Cloud SQL Admin API
will not, and CloudBurrow does not pretend otherwise: there is no `sqladmin` endpoint, and
the support matrix says so.

[#121](https://github.com/cloudburrow/cloudburrow/issues/121) tracks whether to implement
that API later. This is deliberately the smaller thing.

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

**The console screen** (#868) is Cloud SQL's **MySQL** page, at `/cloudsql-mysql`, beside
**PostgreSQL**. It lists the one server, with its version, and its databases (MySQL's own are
left out), then a database's tables, views, routines, users, server settings and live activity,
and a table's columns and indexes, all from `information_schema`, `performance_schema` and
`mysql.user` (never its password column). It reads as root, from the instance's credentials.

Create and drop are `CREATE DATABASE` and `DROP DATABASE`. A created database is granted to
`cloudburrow`, so the application can open it. As on PostgreSQL, the initial database cannot
be dropped here.

The SQL editor is read-only, and MySQL enforces it rather than the console. Each statement runs
inside `START TRANSACTION READ ONLY`, but that alone is not enough in MySQL: DDL commits the
transaction implicitly and then runs. So the statement also runs as `cloudburrow_console`, an
account the screen creates holding only `SELECT` and `SHOW VIEW` on the database being queried.
A write or DDL gets MySQL's own `command denied` (1142). That account appears on the Users tab
and survives `reset`. Verified by `TestConsoleCloudSQLMySQLSchemaAndReadOnlyQuery` and
`TestConsoleCloudSQLMySQLCreateAndDropDatabase`, and in a browser by
`TestCloudSQLMySQLCreateQueryAndDropThroughThePage`.

**Not supported:**
- **`cloudburrow state save`** does not capture it. Use `mysqldump`.
- **The Cloud SQL Admin API**, for either engine.
