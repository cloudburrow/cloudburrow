# Memorystore: a real Valkey, not the Memorystore API

`--services memorystore` runs [Valkey](https://valkey.io), the open-source Redis-compatible
server, in the cluster (#296). It follows the [Cloud SQL](cloudsql.md) decision (#121): Google
publishes no Memorystore emulator, so what runs is the kind of server Memorystore itself runs,
giving an application a real data plane at a stable local address. Since #1302 the
Memorystore admin API describes that one server, and does nothing else (§4).

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

**Only the read half of the Memorystore admin API (`redis.googleapis.com`).** There is one
instance and it cannot be created, resized, upgraded, deleted, exported, imported or failed over;
no AUTH strings, no in-transit encryption, no read replicas, no maintenance windows and no IAM.

**No authentication.** Protected mode is off and there is no password, for the same reason Cloud
SQL uses `trust`: nothing in CloudBurrow authenticates a request, and a password printed in the
banner would imply otherwise. The host endpoint is bound to loopback. Do not expose it.

## 4. The admin API

`--services memorystore` also serves `google.cloud.redis.v1.CloudRedis` on
`--port-memorystore-admin` (default `9021`), gRPC and JSON on one port, exported as
`CLOUDBURROW_MEMORYSTORE_ENDPOINT` (#1302). It describes the one running server as
`projects/<default project>/locations/us-central1/instances/memorystore`: host and port are
`REDIS_HOST:REDIS_PORT`, `redis_version` and `memory_size_gb` are read from the server
(`REDIS_7_2`, 1 GB: the server runs with `--maxmemory 1gb` and Memorystore's default
`volatile-lru` eviction), and the state is READY while it answers.

```go
c, _ := redis.NewCloudRedisClient(ctx, option.WithEndpoint(os.Getenv("CLOUDBURROW_MEMORYSTORE_ENDPOINT")),
	option.WithoutAuthentication(), option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
inst, _ := c.GetInstance(ctx, &redispb.GetInstanceRequest{Name: "projects/" + project + "/locations/us-central1/instances/memorystore"})
```

`GetInstanceAuthString` is FAILED_PRECONDITION, since AUTH is off. Every method that would create,
change or remove an instance is UNIMPLEMENTED. The console's Memorystore page shows the instance
and has **Flush** (FLUSHALL); the API itself has no flush. See
[compatibility.md](compatibility.md#memorystore-for-redis--googlecloudredisv1).
