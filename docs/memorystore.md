# Memorystore: a real Valkey, not the Memorystore API

`--services memorystore` runs [Valkey](https://valkey.io), the open-source Redis-compatible
server, in the cluster (#296). It follows the [Cloud SQL](cloudsql.md) decision (#121): Google
publishes no Memorystore emulator, so what runs is the kind of server Memorystore itself runs,
giving an application a real data plane at a stable local address, **and none of the
Memorystore admin API**.

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

`cloudburrow state save` does not capture Memorystore. Use Valkey's own `BGSAVE`, or the
append-only file on the volume.

## 3. What you do not get

**None of the Memorystore admin API (`redis.googleapis.com`).** This means no instances, no
`gcloud redis instances create`, no AUTH strings, no in-transit encryption, no read replicas,
no maintenance windows, no export or import, and no IAM. An application that calls that API
will fail, because no such endpoint is served.

**No authentication.** Protected mode is off and there is no password, for the same reason Cloud
SQL uses `trust`: nothing in CloudBurrow authenticates a request, and a password printed in the
banner would imply otherwise. The host endpoint is bound to loopback. Do not expose it.
