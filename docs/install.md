# Installing CloudBurrow

> **Status: pre-release.** No binaries are published yet. Build from source.

## Prerequisites

| Tool | Why | Check |
|---|---|---|
| **Docker** | Runs the cluster nodes | `docker info` |
| **kind** v0.33.0 | Creates the cluster | `kind version` |
| **kubectl** | Cluster operations | `kubectl version --client` |
| **Go** | Building from source | `go version` |

Both `arm64` and `amd64` are supported. Verified on macOS (Docker Desktop) and Linux.

**Resource budget, measured:** the full stack — cluster, Knative, both storage endpoints,
Pub/Sub, 20+ pods — uses about **1.5 GiB** of memory and requests roughly **2.1 CPU**. Knative's
own guidance for a local install is 3 CPU / 3 GB, which this is consistent with. Give Docker
at least 4 CPU and 6 GB.

## Build and run

```sh
git clone https://github.com/identity-wael/cloudburrow.git
cd cloudburrow
make build

./bin/cloudburrow up
```

First start pulls the Kubernetes node image and installs Knative, so expect a few minutes.
Later starts reuse the cluster.

`up` prints everything you need:

```
cloudburrow "cloudburrow"
  control:    http://127.0.0.1:9000  (health: /healthz, readiness: /readyz)
  admin:      http://127.0.0.1:9000/admin/{reset,seed,events}  (loopback only)
  cluster:    cloudburrow (kind, Kubernetes v1.36.4)
  kubeconfig: ~/.cloudburrow/cloudburrow/kubeconfig

  endpoints:
    pubsub   host 127.0.0.1:9002   in-cluster pubsub.cloudburrow.svc.cluster.local:8085
    storage  host 127.0.0.1:9001   in-cluster storage-internal.cloudburrow.svc.cluster.local:4443

  configure official SDKs on this machine:
    export PUBSUB_EMULATOR_HOST=127.0.0.1:9002
    export STORAGE_EMULATOR_HOST=http://127.0.0.1:9001
```

## Connecting your application

**Cloud Storage and Pub/Sub** are redirected by environment variable:

```sh
export STORAGE_EMULATOR_HOST=http://127.0.0.1:9001   # the scheme matters, see below
export PUBSUB_EMULATOR_HOST=127.0.0.1:9002           # no scheme
```

**Cloud Tasks and Cloud Run have no emulator environment variable** in any official client.
They need an explicit endpoint in code:

```go
c, err := cloudtasks.NewClient(ctx,
    option.WithEndpoint("127.0.0.1:9003"),
    option.WithoutAuthentication(),
    option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
```

That is a real limitation of redirecting Google SDKs locally, not something CloudBurrow can
work around.

### Two addresses, and they are not interchangeable

Code on **your machine** uses `127.0.0.1:<port>`. Code running **inside the cluster** must use
the in-cluster address — a pod's loopback is the pod itself. `up` prints both.

Note that in-cluster clients use a **different storage endpoint** (`storage-internal`). One
`fake-gcs-server` process can serve object reads to only one audience, so CloudBurrow runs a
second one over the same volume. Both see the same objects.

### The storage scheme

`STORAGE_EMULATOR_HOST` must include `http://`. The Go client prepends a scheme when it is
absent, but the Python client uses the value verbatim — so the form with a scheme is the one
that works for both.

## Commands

| Command | Effect |
|---|---|
| `cloudburrow up` | Create the environment and run in the foreground |
| `cloudburrow status` | Report the instance, its endpoints and per-service persistence |
| `cloudburrow stop` | Stop the cluster, **preserving** state |
| `cloudburrow reset` | Destroy managed state, **keeping** the cluster |
| `cloudburrow delete` | Destroy the cluster |

These are distinct and none implies another.

## Running two environments

Give each a name; ports are OS-assigned with `0`:

```sh
cloudburrow up --name alpha --port-control 0 --port-storage 0 --port-pubsub 0 &
cloudburrow up --name beta  --port-control 0 --port-storage 0 --port-pubsub 0 &
```

Each gets its own cluster, namespace, kubeconfig and ports.

## What is not persisted

`--mode persistent` provisions volumes **for backends that can use them**:

| Service | Survives restart? |
|---|---|
| Cloud Storage | Yes |
| Cloud Tasks | Yes |
| Cloud Run | Workload definitions live in the Kubernetes API |
| **Pub/Sub** | **No — never** |

Google's Pub/Sub emulator loses state on restart even when given `--data-dir`; we measured it.
CloudBurrow cannot inherit durability its backend lacks, so `cloudburrow status` says so
plainly.

## Safety

- **No authentication.** `Authorization` headers are ignored; no credentials are ever read.
- **Loopback by default.** `--allow-remote` is required to bind anything else, and exposes an
  unauthenticated emulator plus a cluster to whoever can reach it.
- **Your kubecontext is never changed.** CloudBurrow writes its own kubeconfig.
- **Only its own clusters are touched**, identified by the `cloudburrow` name prefix and
  `cloudburrow.dev/owned` labels.

## Uninstalling

```sh
cloudburrow delete          # remove the cluster
rm -rf ~/.cloudburrow       # remove host-side state
```
