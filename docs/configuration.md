# Configuration

CloudBurrow resolves configuration from four sources. Precedence, highest first:

```
flags  >  environment  >  config file  >  defaults
```

A flag only participates when it is actually present on the command line, so an unset flag
never overwrites an environment variable with its zero value.

The config file is located by the same rule: `--config`, then `CLOUDBURROW_CONFIG`, then
`./cloudburrow.json` when it exists. A file named explicitly but missing is an error; the
conventional `./cloudburrow.json` is skipped silently when absent. Unknown keys are rejected
rather than ignored, because a silently dropped typo sends you debugging the wrong thing.

Configuration is fully validated **before a cluster is created or a port is bound**, and
**all problems are reported at once** rather than one per restart.

## Prerequisites

| Tool | Why | Notes |
|---|---|---|
| Docker | Runs the cluster nodes | Docker Desktop (macOS) or Docker Engine (Linux). Checked only when a command needs it. |
| kind | Creates the cluster | v0.33.0 is the tested version. |
| kubectl | Talks to the cluster | Used for readiness, version and namespace operations. |

`arm64` and `amd64` are both supported; the pinned `kindest/node` image publishes both.

**Node privileges are not pod privileges.** The kind node runs as a privileged container —
that is how kind works, and it is unavoidable. It says nothing about your workloads:
application pods get no host mounts, no Docker socket and no privileged mode by default.

## Commands

| Command | Meaning |
|---|---|
| `doctor` | Check workstation prerequisites. **Changes nothing.** Exits non-zero only on problems that will stop `up`. |
| `up` | Create the environment if absent, install components, wait for readiness, report endpoints. |
| `status` | Report the configured instance, its endpoints, and per-service persistence. |
| `stop` | Stop the cluster **without destroying it.** State a backend persists survives. |
| `reset` | Destroy CloudBurrow-managed state, **keeping the cluster.** Cancels work before deleting state. |
| `delete` | Destroy the cluster CloudBurrow created. |

**These three are distinct and none implies another.** `stop` is not `delete`, and `reset`
does not remove the cluster. Only `reset` and `delete` destroy anything.

`reset` deletes the managed namespace only. It refuses any namespace that does not carry
`cloudburrow.dev/owned=true`, and refuses `default`, `kube-system`, `kube-public` and
`kube-node-lease` outright — so it can never remove something CloudBurrow did not create.

`up` is idempotent. Against a running cluster it does nothing but refresh the kubeconfig;
against a stopped one it **starts** rather than recreates, so volumes and workloads survive.

## Endpoints and SDK configuration

Every backend has **two addresses, and they are not interchangeable**:

| Caller | Address form |
|---|---|
| A client on your machine | `127.0.0.1:<port>` |
| A workload inside the cluster | `<service>.<namespace>.svc.cluster.local:<port>` |

A pod's loopback is the pod itself, so handing a workload the host address produces a
connection that cannot be made. `cloudburrow up` prints both forms for every service.

**Environment variables only exist for two of the four services:**

| Service | Variable | Value |
|---|---|---|
| Cloud Storage | `STORAGE_EMULATOR_HOST` | `http://127.0.0.1:<port>` — **with the scheme** |
| Pub/Sub | `PUBSUB_EMULATOR_HOST` | `127.0.0.1:<port>` — bare host:port |
| Cloud Tasks | **none exists** | explicit endpoint in client options |
| Cloud Run | **none exists** | explicit endpoint in client options |

The storage value carries a scheme because the official clients disagree: Python uses the
value verbatim and requires one, while Go prepends `http://` when it is absent. The form with
a scheme satisfies both.

Cloud Tasks and Cloud Run have no emulator variable in any official client. That is a real
ergonomic limit of redirecting Google SDKs locally, not something CloudBurrow can paper over.

## Cluster ingress

`--port-ingress` (default `9080`, `CLOUDBURROW_PORT_INGRESS`) publishes the Knative gateway
on the host, so a browser can open a Cloud Run service URL without a `Host` header or a
port-forward. Unlike the SDK endpoints, which are port-forwards, this is a real published
port on the cluster node — **and it is fixed when the cluster is created.**

Changing it requires `cloudburrow delete` then `cloudburrow up`. `up` reports an
unpublished gateway explicitly rather than leaving a port that refuses connections.

`--port-ingress 0` publishes nothing: kind cannot be asked to choose a host port and report
it back, so an OS-assigned ingress port is not offered.

Services are named `<service>.<namespace>.cloudburrow.localhost`. See
[networking.md](networking.md) for what resolves that, and for the `Host`-header path that
always works.

## Local images

Knative resolves image tags to digests **by contacting the registry**, so an image built
locally and loaded into the cluster fails with `failed to resolve image to digest: 401
Unauthorized`. Knative skips that resolution for `dev.local/`, `ko.local/` and `kind.local/`.

CloudBurrow therefore rewrites a bare local reference to `dev.local/<name>:<tag>`, loads it
with `kind load docker-image`, and sets `imagePullPolicy: Never` — a locally loaded image
must never be pulled, because no registry can serve it. References that name a real registry
are left alone.

Before loading, it checks that the image **exists locally**, carries an **explicit tag or
digest**, and **matches the node architecture**. An `amd64` image on `arm64` nodes is
reported as an architecture mismatch with the `--platform` flag to fix it, rather than
surfacing later as an opaque `ImagePullBackOff`.

## Ownership and safety

- CloudBurrow only ever acts on clusters named `cloudburrow` or `cloudburrow-<name>`. The
  prefix is enforced in the constructor *and* re-checked on the destructive path.
- It writes and uses an **explicit kubeconfig** and **never changes your current
  kubecontext**. `kubectl` behaves identically before and after.
- A failed `up` cleans up the partial cluster it created, and nothing else.
- `delete` removes only its own kubeconfig file.

## Settings

| Flag | Environment | File key | Default | Meaning |
|---|---|---|---|---|
| `--name` | `CLOUDBURROW_NAME` | `name` | `cloudburrow` | Instance name. Scopes the cluster, namespace and every owned resource. |
| `--bind-address` | `CLOUDBURROW_BIND_ADDRESS` | `bindAddress` | `127.0.0.1` | IP literal host endpoints are published on. Hostnames are rejected. |
| `--allow-remote` | `CLOUDBURROW_ALLOW_REMOTE` | `allowRemote` | `false` | Required to bind a non-loopback address. See the warning below. |
| `--port-control` | `CLOUDBURROW_PORT_CONTROL` | `endpoints.control` | `9000` | Health, readiness and admin. Always loopback. |
| `--port-storage` | `CLOUDBURROW_PORT_STORAGE` | `endpoints.storage` | `9001` | Cloud Storage host endpoint. |
| `--port-pubsub` | `CLOUDBURROW_PORT_PUBSUB` | `endpoints.pubsub` | `9002` | Pub/Sub host endpoint. |
| `--port-tasks` | `CLOUDBURROW_PORT_TASKS` | `endpoints.tasks` | `9003` | Cloud Tasks host endpoint. |
| `--port-run` | `CLOUDBURROW_PORT_RUN` | `endpoints.run` | `9004` | Cloud Run host endpoint. |
| `--cluster-provider` | `CLOUDBURROW_CLUSTER_PROVIDER` | `cluster.provider` | `kind` | Only `kind` is supported (ADR-0005). |
| `--node-image` | `CLOUDBURROW_NODE_IMAGE` | `cluster.nodeImage` | `kindest/node:v1.36.4` | Pinned node image, which fixes the Kubernetes version. **Must carry a tag or digest.** |
| `--namespace` | `CLOUDBURROW_NAMESPACE` | `cluster.namespace` | `cloudburrow` | Namespace for managed workloads. |
| `--kubeconfig` | `CLOUDBURROW_KUBECONFIG_PATH` | `cluster.kubeconfig` | `<state-dir>/<name>/kubeconfig` | Explicit kubeconfig path. **Never the developer's default file.** |
| `--mode` | `CLOUDBURROW_MODE` | `mode` | `persistent` | `ephemeral` or `persistent`. See below. |
| `--state-dir` | `CLOUDBURROW_STATE_DIR` | `stateDir` | `~/.cloudburrow` | Host-side artifacts only. Application state lives in the cluster. |
| `--services` | `CLOUDBURROW_SERVICES` | `services` | all | Comma-separated subset of `storage,pubsub,tasks,run`. |
| `--shutdown-timeout` | `CLOUDBURROW_SHUTDOWN_TIMEOUT` | `shutdownTimeout` | `30s` | Bounded drain window on shutdown. |
| `--ready-timeout` | `CLOUDBURROW_READY_TIMEOUT` | `readyTimeout` | `5m` | Bounded wait for cluster components to become ready. |
| `--log-level` | `CLOUDBURROW_LOG_LEVEL` | `logLevel` | `info` | `debug`, `info`, `warn`, `error`. |
| `--config` | `CLOUDBURROW_CONFIG` | — | — | Path to a JSON config file. |

**Any port may be set to `0`** to request an OS-assigned port. Several may be `0` at once —
they are not treated as duplicates. Together with `--name`, this is what lets two independent
instances run side by side.

**The node image must be pinned.** An untagged reference resolves to a mutable `latest`, which
ADR-0005 forbids in anything reproducible, so validation rejects it.

## Persistence is per service, not global

`--mode persistent` provisions durable volumes **for backends that can use them**. It is not a
promise that everything survives:

| Service | Persistence |
|---|---|
| `storage` | Survives restart in persistent mode |
| `tasks` | Survives restart in persistent mode |
| `run` | Workload definitions live in the Kubernetes API |
| `pubsub` | **Never survives a restart** |

Pub/Sub is not an oversight. The [upstream audit](upstream-evaluation.md) measured Google's
emulator losing a topic across a restart *even when given `--data-dir`*. CloudBurrow cannot
inherit a guarantee its backend does not have, so the model refuses to express one and
`cloudburrow status` says so plainly.

## Exposure

CloudBurrow performs **no authentication**. It ignores `Authorization` headers, validates no
signature, and never reads application default credentials.

`--allow-remote` therefore exposes an unauthenticated emulator — and the cluster it
manages — to anyone who can reach the address. It is opt-in, warns loudly at startup, and
should not be used on a shared network. The control port stays on loopback regardless.

**CloudBurrow never changes your global kubecontext.** It writes and uses an explicit
kubeconfig, and only ever acts on clusters and resources it created, identified by the
`cloudburrow.dev/owned` and `cloudburrow.dev/instance` labels.

## Admin API

The control port also serves the admin API. It is **loopback-only whatever the bind address**,
and is refused on service ports — reset destroys data, so an application pod can reach the
service APIs it needs and cannot reach the endpoint that wipes state.

| Endpoint | Purpose |
|---|---|
| `POST /admin/reset` | Destroy CloudBurrow-managed state, keeping the cluster |
| `POST /admin/seed` | Create resources from a seed document |
| `GET /admin/events` | Recent events, newest first, filterable by `service` |

```sh
curl -X POST localhost:9000/admin/seed -d '{
  "components": {"tasks": {"queues": ["projects/p/locations/us-central1/queues/work"]}}
}'
curl -X POST localhost:9000/admin/reset
curl "localhost:9000/admin/events?service=tasks&limit=20"
```

**Seeding validates every component name before creating anything**, so an unknown name cannot
leave a half-populated environment. **Reset attempts every component even when one fails** and
reports per-component results: a partial reset that claimed success would leave you debugging
state you believed was cleared.

## Health and readiness

The control port serves two endpoints:

- `GET /healthz` — process liveness. `200` whenever the process is serving.
- `GET /readyz` — actual initialisation. `200` only when every component started, `503`
  otherwise, with a per-component breakdown and the underlying cause.

A process that is alive but failed mandatory startup answers `200` on health and `503` on
readiness, so a caller can tell "alive but broken" from "not listening" — and never proceeds
against a half-initialised instance.
