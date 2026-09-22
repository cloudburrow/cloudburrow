# Cloud SQL

Issue: [#121](https://github.com/identity-wael/cloudburrow/issues/121) · Added 2026-09-22

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
| Address | a loopback host port, plus `cloudsql.cloudburrow.svc.cluster.local:5432` in-cluster |
| User / database | `cloudburrow` / `cloudburrow` |
| Authentication | **trust** — see below |
| Durability | a PersistentVolumeClaim in `--mode persistent`; the only opt-in backend that keeps its data |

```sh
cloudburrow up --services cloudsql
psql "postgres://cloudburrow@127.0.0.1:<port>/cloudburrow?sslmode=disable"
```

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

[#121](https://github.com/identity-wael/cloudburrow/issues/121) tracks whether to implement
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
# the startup banner prints the host port

psql "postgres://cloudburrow@127.0.0.1:<port>/cloudburrow?sslmode=disable" \
  -c "CREATE TABLE widgets (id serial primary key, name text);" \
  -c "SELECT version();"
```

No live Google endpoint is contacted at any point, and none exists to contact: there is no
Cloud SQL emulator to reach for.
