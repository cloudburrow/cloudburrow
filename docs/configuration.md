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

## Commands

| Command | Meaning |
|---|---|
| `up` | Create the environment if absent, install components, wait for readiness, report endpoints. |
| `status` | Report the configured instance, its endpoints, and per-service persistence. |
| `stop` | Stop the cluster **without destroying it.** State a backend persists survives. |
| `reset` | Destroy CloudBurrow-managed state, **keeping the cluster.** Cancels work before deleting state. |
| `delete` | Destroy the cluster CloudBurrow created. |

**These three are distinct and none implies another.** `stop` is not `delete`, and `reset`
does not remove the cluster. Only `reset` and `delete` destroy anything.

Cluster operations are not implemented yet (issue #9). `stop`, `reset` and `delete` validate
their configuration and then fail with a clear message naming that issue — they never report
success for work they did not do.

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

## Health and readiness

The control port serves two endpoints:

- `GET /healthz` — process liveness. `200` whenever the process is serving.
- `GET /readyz` — actual initialisation. `200` only when every component started, `503`
  otherwise, with a per-component breakdown and the underlying cause.

A process that is alive but failed mandatory startup answers `200` on health and `503` on
readiness, so a caller can tell "alive but broken" from "not listening" — and never proceeds
against a half-initialised instance.
