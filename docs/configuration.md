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

**A config file cannot expose the emulator.** `allowRemote: true` in any config file is refused:
only `--allow-remote` or `CLOUDBURROW_ALLOW_REMOTE=true` confirm a non-loopback bind. A
`./cloudburrow.json` that `up` found in the working directory cannot set a non-loopback
`bindAddress` either; pass `--bind-address` (or `CLOUDBURROW_BIND_ADDRESS`). A file you name with
`--config` or `CLOUDBURROW_CONFIG` may set one, but it still needs the flag or variable to confirm
it. A cloned repository's file therefore cannot put an unauthenticated emulator on your network
([Exposure](#exposure)).

**A discovered `./cloudburrow.json` must be trusted before `up` uses it**, as must hooks you did
not name; see [Trust](#trust). A file named with `--config` or `CLOUDBURROW_CONFIG` is your own
choice and needs no trust record.

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
| `env` | Print the environment that points Google tooling at this instance. **Fails, printing nothing, when the instance is not running**; `--offline` prints the configured endpoints instead ([`cloudburrow env`](credentials.md#cloudburrow-env)). **Changes nothing** beyond writing the credentials fixture. |
| `doctor` | Check workstation prerequisites. **Changes nothing.** Exits non-zero only on problems that will stop `up`. |
| `diagnose` | Write a redacted bundle for a bug report (`-o bundle.tar.gz`): version, configuration, doctor, readiness, status, the cluster's stamped and pinned versions, pods, events, recent logs and admin events, with `manifest.json` recording every step that failed or was skipped. It never reads the kubeconfig's contents, Kubernetes Secrets, the ADC key, Secret Manager payloads or Cloud KMS key material, and pod env values are removed. Works against a stopped instance with configuration and doctor output only. |
| `up` | Create the environment if absent, install components, wait for readiness, report endpoints. Runs in the foreground; `--detach` runs it in the background. |
| `trust` | Trust this directory's `./cloudburrow.json` and the hooks you did not name, as they are now; `up --trust` does the same and starts. See [Trust](#trust). |
| `wait` | Wait until a running instance is ready. **Changes nothing.** |
| `logs` | Print emulator, component and Cloud Run workload logs. **Changes nothing.** |
| `state save <file>` / `state load <file>` | Save the running instance's Cloud Storage, Cloud Tasks, Cloud Scheduler, Secret Manager, project and Cloud SQL (PostgreSQL) state to an archive, or replace it with one. See [compatibility.md](compatibility.md#state-snapshots) for what is captured. **The archive holds secret values.** |
| `status` | Report the configured instance, its endpoints, and per-service persistence. `--format json` for a script (below). |
| `stop` | End the running `up`, if any, then stop the cluster **without destroying it.** State a backend persists survives. |
| `reset` | Destroy CloudBurrow-managed state, **keeping the cluster.** Cancels work before deleting state. With an `up` running it goes through the [admin API](#admin-api) (`--service`, `--project`, `--reseed`), so pods and port-forwards stay up; with none running it deletes the managed namespace. |
| `seed <file>` | Create the resources in a [seed document](#admin-api) in the running `up`. `--if-not-exists` skips resources that exist. |
| `events` | Print the running `up`'s recent [admin events](#admin-api), newest first (`--service`, `--kind`, `--since 10m`, `--limit`, `--format json`). **Changes nothing.** |
| `delete` | Destroy the cluster CloudBurrow created. |
| `storage-server` | Run CloudBurrow's own Cloud Storage server alone (`--listen`, default `127.0.0.1:4443`; `--host` for virtual-hosted XML; `--allow-remote` for a non-loopback address). It is built to Google's spec (#485) and is what `up` runs in the cluster (#514, #519): every JSON API method from Google's discovery document is routed, and each one not built answers **501 `notImplemented`** naming it; XML requests answer an XML `<Error>`. **Not yet a working Cloud Storage.** |

### Running in the background

`up` hosts Cloud Tasks, Secret Manager, the metadata server and every port-forward in its own
process, so it keeps running. `up --detach` starts it in the background, in a session of its own
so it outlives the shell, and **returns only once `/readyz` answers 200**. Output goes to `up.log`
in the instance directory. On failure, or when `--detach-timeout` (default `10m`) passes, it
prints the tail of that log. A half-started process is terminated rather than left behind. For an
instance that is already running, `up --detach` does nothing, and still exits 0 only once the
instance is ready. A foreground `up` for a running instance is refused.

Each running `up` records its pid and control address in `up.json` in the instance directory,
which serves as its pidfile. `stop` sends that process SIGTERM, waits for its bounded drain, and
removes the file, then stops the cluster. Before signalling, it checks that the pid is still a
CloudBurrow process, so a stale file cannot lead it to signal a pid the system has since reused.

`wait [--timeout 5m]` polls `/readyz`. It finds the control port in `up.json`, so an OS-assigned
one works, and prints the per-component breakdown. `up --detach` exits with the same codes:

| Exit | Meaning |
|---|---|
| `0` | Ready: every component started. |
| `1` | Failed: a component failed, or the process exited during startup. |
| `2` | Timed out while starting; the components not yet ready are named. (`2` is also a usage error, as for every command.) |

```sh
cloudburrow up --detach --services storage,pubsub
eval "$(cloudburrow env)"
go test ./...
cloudburrow stop
```

### Logs

`cloudburrow logs` prints what the instance's pods and its own in-process services have logged:

```sh
cloudburrow logs                                  # everything of this instance's, in time order
cloudburrow logs --service pubsub --tail 20
cloudburrow logs --service run --resource hello --follow
cloudburrow logs --service tasks --since 10m      # an in-process service: its lines of up.log
cloudburrow logs --format json                    # one object per line
```

- **Emulators and components:** pods in the managed namespace labelled `cloudburrow.dev/owned`,
  every container by name.
- **Cloud Run services:** only Knative services labelled as this instance's, found by their
  Cloud Run name.
- **The services `up` serves itself** (Cloud Tasks, Secret Manager, Cloud KMS, Cloud Scheduler,
  Cloud Logging, Resource Manager and the metadata server) log to `up.log` in the instance
  directory. It's timestamped, written by a detached `up` and teed by a foreground one, so
  `--since` applies to it as well. `--service kms` (and the others) shows that service's request
  lines, `<service>.<Method> => <CODE>`; failed calls are logged at `info`, every call at
  `--log-level debug`. The metadata server writes no request lines, so `--service metadata`, like
  `--service cloudburrow`, shows the whole of `up.log`.
- **Cloud Run** is both: `--service run` shows the adapter's request lines from `up.log` and every
  one of this instance's Cloud Run services' pods; `--resource` narrows it to one service's pods.

**It reads only this instance's cluster:** every kubectl call passes the instance's own
`--kubeconfig`, and `KUBECONFIG` is removed from kubectl's environment, so no other context can be
reached. Credentials in a line are redacted as the console redacts them. It exits non-zero, saying
so, when the instance is not running. `--follow` streams until interrupted and exits 130.
### Status for scripts

`cloudburrow status --format json` prints one versioned object with:
- the instance, project, mode and bind address;
- the control, console and ingress URLs;
- the cluster's state and, when it is running, the versions it is stamped with
  (`cluster.versions`: CLI, node image, Knative; `stamped` is false for a cluster from before the
  stamp) beside this CLI's pins (`cluster.pinned`); see [Upgrading](install.md#upgrading);
- **each enabled service**: its host endpoint (the bound one when running), the environment
  variable that points a client at it, its persistence, whether it is ready, and if not, the
  components it is waiting for;
- the per-component readiness from `/readyz`.

`schema_version` changes when a field is removed, renamed or changes meaning. A new optional
field, such as `hooks`, is added without changing it. It is `2` since the cluster's versions were
added (#601). Golden files in `cmd/cloudburrow/testdata`
pin the shape.

```sh
cloudburrow status --format json | jq -r '.services[] | select(.id=="storage") | .endpoint'
```

Unlike the human form, which prints the same as before and always exits 0, the JSON form exits
with the state:

| Exit | `state` | Meaning |
|---|---|---|
| `0` | `ready` | The instance is running and every component is ready. |
| `2` | — | Usage error. |
| `3` | `not_running` | No `up` is running for this instance. |
| `4` | `starting` or `failed` | An `up` is running, but not every component is ready. |

**These three are distinct and none implies another.** `stop` is not `delete`, and `reset`
does not remove the cluster. Only `reset` and `delete` destroy anything.

With an `up` running, `reset` is `POST /admin/reset` on it (#586): each service clears its own
state through its own API, and the cluster, its pods and the port-forwards are untouched, so
the instance keeps serving. `--service`, `--project` and `--reseed` scope it as the
[admin API](#admin-api) describes; they need a running `up`, and without one are refused
rather than widened into a full wipe.

With no `up` running, `reset` deletes the managed namespace only. It refuses any namespace that
does not carry `cloudburrow.dev/owned=true`, and refuses `default`, `kube-system`, `kube-public`
and `kube-node-lease` outright — so it can never remove something CloudBurrow did not create.

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

**Of the default services, only Cloud Storage and Pub/Sub have an emulator variable:**

| Service | Variable | Value |
|---|---|---|
| Cloud Storage | `STORAGE_EMULATOR_HOST` | `http://127.0.0.1:<port>` — **with the scheme** |
| Pub/Sub | `PUBSUB_EMULATOR_HOST` | `127.0.0.1:<port>` — bare host:port |
| Cloud Tasks | **none exists** | explicit endpoint in client options |
| Cloud Run | **none exists** | explicit endpoint in client options |
| Secret Manager | **none exists** | explicit endpoint in client options |

The opt-in Firestore, Datastore, Bigtable and Spanner emulators have their own
(`FIRESTORE_EMULATOR_HOST`, `DATASTORE_EMULATOR_HOST`, `BIGTABLE_EMULATOR_HOST`,
`SPANNER_EMULATOR_HOST`), exported by `cloudburrow env` when the service is enabled.

The storage value carries a scheme because the official clients disagree: Python uses the
value verbatim and requires one, while Go prepends `http://` when it is absent. The form with
a scheme satisfies both.

Cloud Tasks, Cloud Run and Secret Manager have no emulator variable in any official client. That is a real
ergonomic limit of redirecting Google SDKs locally, not something CloudBurrow can paper over.

### Other output formats

`cloudburrow env --format <format>`:

| Format | For |
|---|---|
| `shell` (default) | `eval "$(cloudburrow env)"` |
| `plain` | `KEY=value` lines: **the `docker run --env-file` format**, and dotenv loaders that take the same |
| `json` | A script, with `jq` |
| `terraform` | `eval "$(cloudburrow env --format terraform)"` before `terraform`. It exports the google provider's own variables: `GOOGLE_PROJECT`, a fixture `GOOGLE_OAUTH_ACCESS_TOKEN`, and `GOOGLE_*_CUSTOM_ENDPOINT` **only for services whose Terraform support is Verified** in compatibility.md. The same table drives `cloudburrow terraform`, and other enabled services are named in a comment. |
| `docker-compose` | An `environment:` map to paste under a service, with loopback addresses rewritten to `host.docker.internal`. `GOOGLE_APPLICATION_CREDENTIALS` is left out because it names a host path. |
| `kubernetes` | A container's `env:` list of **in-cluster** addresses, for a pod in the instance's cluster; see below. |

#### `--format kubernetes`

An `env:` list to paste under a container in a Kubernetes manifest — a list rather than a
ConfigMap, so it needs no second object to apply and keep in step. It holds, for every service
**the running instance** recorded as bound (not whatever this invocation's `--services` says):

| Variable | Value |
|---|---|
| `STORAGE_EMULATOR_HOST` | `http://storage.<namespace>.svc.cluster.local:<port>` |
| `PUBSUB_EMULATOR_HOST`, `FIRESTORE_`, `DATASTORE_`, `BIGTABLE_`, `SPANNER_EMULATOR_HOST` | `<service>.<namespace>.svc.cluster.local:<port>` |
| `CLOUDBURROW_BIGQUERY_ENDPOINT`, `CLOUDBURROW_BIGQUERY_STORAGE_ENDPOINT` | the BigQuery emulator's Service |
| `CLOUDBURROW_TASKS_ENDPOINT`, `_SECRETMANAGER_`, `_KMS_`, `_SCHEDULER_`, `_LOGGING_`, `_RESOURCEMANAGER_ENDPOINT` | `cloudburrow-host.<namespace>.svc.cluster.local:<port>`, when Cloud Run is enabled ([networking.md](networking.md#reaching-your-machine-from-a-pod)) |
| `GCE_METADATA_HOST` | the metadata server at `cloudburrow-host`, likewise |
| `GOOGLE_CLOUD_PROJECT` | the instance's project |

A service with no address a pod can use is left out rather than given a host address: a
CLI-hosted service without Cloud Run, Cloud SQL, Memorystore. **No credential is included**:
`GOOGLE_APPLICATION_CREDENTIALS` names a host path, and a pod asks the metadata server for a
token instead.

It needs a running instance, and **`--offline` is refused** for this format: the CLI-hosted
services' in-cluster addresses are what a running `up` published, which configuration cannot
know.

**Cloud Run services deployed through CloudBurrow are given these already** (#576), in every
revision and job task, with `GOOGLE_CLOUD_PROJECT` set to the service's own project. A variable
the service sets itself wins over an injected one of the same name. The Cloud Run API —
`GetService`, `ListServices`, `GetRevision`, and jobs and executions — reports **only the
service's own** variables, as Cloud Run keeps its platform variables (`K_SERVICE`, `PORT`) out of
the resource. So a Terraform plan sees no drift, and a read-modify-write client never sends an
injected value back to freeze it. To see what a revision was given, read the Knative Service:
`kubectl --kubeconfig <state>/<name>/kubeconfig -n default get ksvc <service> -o yaml` shows the
variables in the container's `env` and the record in the template annotation
`cloudburrow.dev/injected-env`.

**Whether a container can reach those addresses depends on the engine.** CloudBurrow binds
loopback by default.
- **Docker Desktop** (macOS, Windows): `host.docker.internal` reaches the host's loopback, so the
  compose map works as printed.
- **Linux:** `host.docker.internal` needs `extra_hosts: ["host.docker.internal:host-gateway"]`, and
  even then it reaches the host's bridge address, not its loopback. Either run the container with
  `network_mode: host` and use `plain` unchanged, or bind CloudBurrow to an address the bridge
  reaches, with the exposure warnings that apply ([Exposure](#exposure)).

With host networking, `plain` works as an `--env-file` as it stands. The compat suite proves it on
Linux (`TestAContainerListsBucketsFromPlainEnvFile`). To check by hand on Docker Desktop, where host
networking does not reach the host's loopback, use the compose map's rewritten addresses:

```sh
cloudburrow env --format plain | sed 's#127.0.0.1#host.docker.internal#g' > cb.env
docker run --rm --env-file cb.env --entrypoint sh curlimages/curl -c \
  'curl -fsS "$STORAGE_EMULATOR_HOST/storage/v1/b?project=$GOOGLE_CLOUD_PROJECT"'
```

## Lifecycle hooks

Scripts in the hooks directory run on the host when the instance becomes ready and before it stops:

```
.cloudburrow/hooks/          # --hooks-dir, CLOUDBURROW_HOOKS_DIR, config "hooksDir"
  ready.d/
    10-buckets.sh            # run once every component has started
    20-topics.sh
  shutdown.d/
    10-export.sh             # run before anything is stopped
```

- **When.** `ready.d` runs on **every `up`**, once every component has started, so a script must
  be safe to run again. `/readyz` turns green only once the ready hooks have finished, so `wait`
  and `up --detach` return after them. `shutdown.d` runs when `up` is stopping, whether by `stop`,
  Ctrl-C or SIGTERM. It runs before anything is stopped, while storage, Pub/Sub and the cluster
  still answer.
- **What.** Every executable file in the stage directory, in lexical order of its name. A file
  that is not executable is listed as skipped, not run. A missing directory runs nothing.
- **Environment.** A minimal environment, not `up`'s own: of `up`'s environment only `PATH`,
  `HOME`, `USER`, `LOGNAME`, `SHELL`, `LANG`, `LC_*`, `TMPDIR`, `TERM` and `TZ`, plus the
  variables named by `--hook-env` (`CLOUDBURROW_HOOK_ENV`, config `hookEnv`; comma-separated, for
  example `--hook-env RUNNER_TEMP,KUBECONFIG`). Nothing else your shell holds, such as tokens or
  cloud credentials, reaches a hook. On top of that, every variable `cloudburrow env` prints for
  this instance, with the ports it actually bound, so `STORAGE_EMULATOR_HOST`,
  `PUBSUB_EMULATOR_HOST`, `GOOGLE_CLOUD_PROJECT` and `GOOGLE_APPLICATION_CREDENTIALS` point at
  this instance whatever the shell or `--hook-env` had. The working directory is the stage
  directory.
- **Trust.** Hooks in a directory you named, with `--hooks-dir`, `CLOUDBURROW_HOOKS_DIR` or
  `hooksDir` in a file you named with `--config`, run as given. The default `.cloudburrow/hooks`,
  or a `hooksDir` from a discovered `./cloudburrow.json`, comes with the checkout, so `up`
  refuses to start until you have trusted its scripts; see [Trust](#trust).
- **Failure.** Each script has a time limit (`--hook-timeout`, default `5m`). A script that exits
  non-zero is reported with its name and exit status. One that runs past its limit is killed,
  together with anything it started, and reported. **Either way, the remaining scripts still
  run**, and the instance stays up.
- **Output.** Each line is prefixed with `[ready.d/<name>]` in `up`'s output, and so in `up.log`
  and `cloudburrow logs --service cloudburrow`. The outcomes are listed by `cloudburrow status`,
  in the `hooks` field of `status --format json`, and as the `init` readiness component.
- **Where they run.** On the host, as you, never in the cluster. A hook gets no cluster
  credentials or Docker socket it did not already have.

## Trust

`up` acts on two things a directory can carry without your naming them: `./cloudburrow.json`,
which chooses images, paths and ports, and the scripts in `.cloudburrow/hooks` (or the `hooksDir`
that file sets), which run on your machine as you. Running `up` in a cloned repository is then
the same as running its `make`: you would be running its code. So the first `up` in such a
directory lists them and exits non-zero, before a cluster, port or file is created:

```
cloudburrow: not trusted: /src/app has files `up` would act on that you have not trusted:
  config  /src/app/cloudburrow.json
  hook    /src/app/.cloudburrow/hooks/ready.d/10-buckets.sh
```

Read them, then run `cloudburrow trust` (or `cloudburrow up --trust`, which also starts). The
record is a SHA-256 of the files' paths, executable bits and contents, kept per directory in
`trust.json` in the state directory (`~/.cloudburrow`, or the one `--state-dir` or
`CLOUDBURROW_STATE_DIR` names; never one the discovered file chose, which would let a repository
vouch for itself). Editing a script, adding one, making a file executable or changing the config
file asks again. Only scripts that would run count: a file that is not executable is not listed.

What you name yourself needs no record: a config file given with `--config` or
`CLOUDBURROW_CONFIG`, and hooks given with `--hooks-dir`, `CLOUDBURROW_HOOKS_DIR` or `hooksDir` in
such a file. That is how CI passes its fixtures (`--hooks-dir test/compat/testdata/hooks`). The
check runs when `up` starts; a script edited while the instance is running is not checked again
until the next `up`.

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

## Secret Manager

`--port-secrets` (default `9006`, `CLOUDBURROW_PORT_SECRETS`) serves Secret Manager v1 over
**gRPC and JSON on the same port**, as Google's own endpoint does. It is in the default
service set; `--services` can exclude it.

Payloads are stored as **Kubernetes Secrets in the workload namespace**, which is what lets a
Cloud Run revision reference one with `valueFrom.secretKeyRef`. They cannot live in the
managed namespace: a `secretKeyRef` cannot cross namespaces. `cloudburrow reset` removes them
through the running `up`'s Secret Manager, or, with none running, by ownership label rather
than by namespace.

**It is not a secret store.** CloudBurrow authenticates nothing, so anything written there is
readable by any caller that can reach the endpoint. It exists so an application whose code
fetches configuration from Secret Manager can run locally. See
[compatibility.md](compatibility.md#secret-manager--googlecloudsecretmanagerv1).

## Cloud KMS

`--port-kms` (default `9018`, `CLOUDBURROW_PORT_KMS`, `endpoints.kms` in the config file)
serves Cloud KMS v1 over gRPC, and Encrypt and Decrypt over JSON too, on the same port. It is opt-in: `--services kms`.
`cloudburrow env` exports `CLOUDBURROW_KMS_ENDPOINT` for `option.WithEndpoint`.

Key rings, keys, versions and their key material are stored as **Kubernetes Secrets labelled
`cloudburrow.dev/service=kms` in the managed namespace**. `cloudburrow reset` deletes the keys,
through the running `up` or, with none running, with that namespace: ciphertext encrypted before
a reset cannot be decrypted after it. `up` prints which store is in use.

**It is not a key management system.** Key material is kept unencrypted in those Secrets, and
anyone who can read them, or reach the endpoint, can use or read every key. There is no HSM,
no external key manager, and IAM is not enforced. It exists so an application that encrypts
with Cloud KMS can run locally. Never use it to protect real data. See
[compatibility.md](compatibility.md#cloud-kms--googlecloudkmsv1).

## Console

`--port-firestore` (default `9010`), `--port-datastore` (`9011`), `--port-bigtable` (`9012`) and
`--port-spanner` (`9013`), `--port-bigquery` (`9014`), `--port-bigquery-storage` (`9015`) and
`--port-memorystore` (`9016`), `--port-cloudsql-mysql` (`9017`) and `--port-cloudsql` (`9019`) —
with `CLOUDBURROW_PORT_FIRESTORE` and so on, and config keys `endpoints.firestore` etc. — are the
host ports of the opt-in emulators. They are fixed so that
`cloudburrow env`, a separate process, can export each enabled emulator's `*_EMULATOR_HOST` with
the address `up` binds. Set one to `0` for an OS-assigned port, and `env` will leave that variable
out and say so on stderr: an empty or guessed value would send the client to real Google.

**Memorystore** (`--services memorystore`) is exported as `REDIS_HOST` and `REDIS_PORT`, the
variables Redis clients read by convention; no Google library reads them, because an application
reaches Memorystore with an ordinary Redis client. See [memorystore.md](memorystore.md).

**Cloud SQL for MySQL** (`--services cloudsql-mysql`) is exported as `MYSQL_HOST`,
`MYSQL_PORT`, `MYSQL_USER`, `MYSQL_PASSWORD` and `MYSQL_DATABASE`, with the instance's generated
password. See [cloudsql.md](cloudsql.md#7-cloud-sql-for-mysql).

**Cloud SQL for PostgreSQL** (`--services cloudsql`) is exported as `PGHOST`, `PGPORT`, `PGUSER`,
`PGDATABASE` and `PGSSLMODE=disable`, the variables libpq, `psql` and the drivers modelled on it
read, and as one connection string, `CLOUDBURROW_CLOUDSQL_URL`. There is no `PGPASSWORD`: the
server trusts every connection. See [cloudsql.md](cloudsql.md#2-what-you-get).

**Resource Manager v3** is always served, on `--port-resourcemanager` (default `9007`). `env`
exports `CLOUDBURROW_RESOURCEMANAGER_ENDPOINT`, to pass to `option.WithEndpoint`, and
`CLOUDSDK_API_ENDPOINT_OVERRIDES_CLOUDRESOURCEMANAGER`. Only the v3 Projects API is served; see
[compatibility.md](compatibility.md#resource-manager--googlecloudresourcemanagerv3-projects-only).

**BigQuery has no emulator variable in any official client library**, so `env` exports
CloudBurrow's own: `CLOUDBURROW_BIGQUERY_ENDPOINT` for REST and
`CLOUDBURROW_BIGQUERY_STORAGE_ENDPOINT` for the gRPC Storage Read API. It also exports
`CLOUDSDK_API_ENDPOINT_OVERRIDES_BIGQUERY`, which gcloud reads and the client libraries ignore.
Application code passes the endpoint explicitly, **with the instance's project**, since the
emulator serves no other:

```go
client, err := bigquery.NewClient(ctx, os.Getenv("GOOGLE_CLOUD_PROJECT"),
    option.WithEndpoint(os.Getenv("CLOUDBURROW_BIGQUERY_ENDPOINT")),
    option.WithoutAuthentication())
```

`--port-console` (default `9090`, `CLOUDBURROW_PORT_CONSOLE`) serves the web console, and
`up` prints the URL. The assets are embedded in the binary, so it works with no network.

It creates, edits and deletes as well as reads, and it is a **view**: every resource it shows is read through the
same surfaces an SDK client uses, so nothing it displays can disagree with what a client
sees. The API refuses cross-site and cross-origin requests — loopback is reachable from any
page the browser has open, so binding loopback is not by itself protection.

**Logs Explorer** streams live over Server-Sent Events. Credentials are redacted **before an
entry is stored**, messages are truncated at 2 KiB and the buffer holds 2000 entries in
memory — nothing is persisted.

Only the `default` and managed namespaces are followed. The Kubernetes control plane,
kourier and Knative's own components were measured at **90% of the buffer**, pushing out the
lines that explain a developer's failure; the Events screen and `kubectl logs` still reach
them.

See [console-parity.md](console-parity.md) for the parity checklist.

## Storage notifications

Object mutations are published to Pub/Sub by the storage server itself (#506): it serves the
`notificationConfigs` API on the same port as the rest of the Storage API, and writes each
event to an outbox in the same transaction as the mutation, then publishes it to the cluster's
Pub/Sub emulator after commit, retrying a failed publish. In persistent mode configurations
and undelivered events survive a restart.

See [compatibility.md](compatibility.md#cloud-storage--notifications-to-pubsub) for what is
and is not delivered.

## Storage signed URLs

HMAC-signed URLs verify against the HMAC keys the Storage API creates. RSA-signed URLs (V2 and
`GOOG4-RSA-SHA256`, the only V4 scheme the official Go client produces) verify against a public
key registered for the signing service account; a URL for any other account is 403 (#509).

`up` registers the ADC fixture's own key (#577), so a URL signed with the credentials
`cloudburrow env` exports verifies with no setup:

```go
url, _ := storage.SignedURL(bucket, object, &storage.SignedURLOptions{
    GoogleAccessID: fixture.ClientEmail, PrivateKey: []byte(fixture.PrivateKey), ...})
```

To verify URLs signed by another service account, name its certificate or public key (PEM) in
the configuration file; relative paths are resolved against the directory `up` runs in, and a
file that is not a PEM certificate or RSA public key fails `up` before anything is created:

```json
{"storage": {"signingCerts": {"signer@my-project.iam.gserviceaccount.com": "certs/signer.pem"}}}
```

The keys are public, so they are passed to the storage Deployment in its arguments
(`--signing-key`), not mounted. `cloudburrow storage-server` takes the same as
`--signing-cert email=path.pem`.

## Credentials and metadata

`--port-metadata` (default `9005`, `CLOUDBURROW_PORT_METADATA`) serves a local GCE metadata
server, and `up` writes an ADC fixture into the instance directory.

**Neither authenticates anything.** They exist so that `gcloud`, Terraform and the Google
SDKs — which insist on having credentials — can run offline. `eval "$(cloudburrow env)"`
exports them. See [credentials.md](credentials.md), which also records why exporting them
matters: without the fixture, Google's libraries find your **real** credentials and reach
the network.

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
| `--config` | `CLOUDBURROW_CONFIG` | — | `./cloudburrow.json` when it exists | Path to a JSON config file. A file named here or by the variable must exist; the discovered one is skipped when absent. See [Trust](#trust). |
| `--name` | `CLOUDBURROW_NAME` | `name` | `cloudburrow` | Instance name. Scopes the cluster, namespace and every owned resource. |
| `--project` | `CLOUDBURROW_PROJECT` | `project` | derived from `--name` | Default project ID: what the console opens on, the ADC fixture and metadata server report, and `env` exports. Must be a valid project ID. See [the default project](#the-default-project). |
| `--bind-address` | `CLOUDBURROW_BIND_ADDRESS` | `bindAddress` | `127.0.0.1` | IP literal host endpoints are published on. Hostnames are rejected. A discovered `./cloudburrow.json` may set only a loopback address. |
| `--allow-remote` | `CLOUDBURROW_ALLOW_REMOTE` | — | `false` | Required to bind a non-loopback address. **Flag or environment only**: a config file setting `allowRemote` to true is refused. See [Exposure](#exposure). |
| `--port-base` | `CLOUDBURROW_PORT_BASE` | `portBase` | `0` (the defaults, from `9000`) | Moves every fixed default port so the layout starts here: control at the base, each `900x`/`901x` port the same distance above it, ingress at base+80, console at base+90. A port set explicitly by flag, environment or `endpoints.*` is not moved; an OS-assigned one (`0`, and Local AI's by default) stays OS-assigned. `1024`–`65445`. See [Running two instances](#running-two-instances). |
| `--port-control` | `CLOUDBURROW_PORT_CONTROL` | `endpoints.control` | `9000` | Health, readiness and admin. Always loopback. |
| `--port-storage` | `CLOUDBURROW_PORT_STORAGE` | `endpoints.storage` | `9001` | Cloud Storage host endpoint. |
| `--port-pubsub` | `CLOUDBURROW_PORT_PUBSUB` | `endpoints.pubsub` | `9002` | Pub/Sub host endpoint. |
| `--port-tasks` | `CLOUDBURROW_PORT_TASKS` | `endpoints.tasks` | `9003` | Cloud Tasks host endpoint. |
| `--port-run` | `CLOUDBURROW_PORT_RUN` | `endpoints.run` | `9004` | Cloud Run host endpoint. |
| `--port-metadata` | `CLOUDBURROW_PORT_METADATA` | `endpoints.metadata` | `9005` | The local GCE metadata server. See [Credentials and metadata](#credentials-and-metadata). |
| `--port-secrets` | `CLOUDBURROW_PORT_SECRETS` | `endpoints.secrets` | `9006` | Secret Manager host endpoint. See [Secret Manager](#secret-manager). |
| `--port-resourcemanager` | `CLOUDBURROW_PORT_RESOURCEMANAGER` | `endpoints.resourceManager` | `9007` | Resource Manager v3 Projects API host endpoint. |
| `--port-scheduler` | `CLOUDBURROW_PORT_SCHEDULER` | `endpoints.scheduler` | `9008` | Cloud Scheduler host endpoint (`--services scheduler`). |
| `--port-logging` | `CLOUDBURROW_PORT_LOGGING` | `endpoints.logging` | `9009` | Cloud Logging host endpoint (`--services logging`). |
| `--port-firestore` | `CLOUDBURROW_PORT_FIRESTORE` | `endpoints.firestore` | `9010` | Firestore emulator host endpoint (`--services firestore`). |
| `--port-datastore` | `CLOUDBURROW_PORT_DATASTORE` | `endpoints.datastore` | `9011` | Datastore emulator host endpoint (`--services datastore`). |
| `--port-bigtable` | `CLOUDBURROW_PORT_BIGTABLE` | `endpoints.bigtable` | `9012` | Bigtable emulator host endpoint (`--services bigtable`). |
| `--port-spanner` | `CLOUDBURROW_PORT_SPANNER` | `endpoints.spanner` | `9013` | Spanner emulator host endpoint (`--services spanner`). |
| `--port-bigquery` | `CLOUDBURROW_PORT_BIGQUERY` | `endpoints.bigquery` | `9014` | BigQuery emulator REST host endpoint (`--services bigquery`). |
| `--port-bigquery-storage` | `CLOUDBURROW_PORT_BIGQUERY_STORAGE` | `endpoints.bigqueryStorage` | `9015` | BigQuery Storage Read API gRPC host endpoint (`--services bigquery`). |
| `--port-memorystore` | `CLOUDBURROW_PORT_MEMORYSTORE` | `endpoints.memorystore` | `9016` | Memorystore (Valkey, RESP) host endpoint (`--services memorystore`). |
| `--port-cloudsql-mysql` | `CLOUDBURROW_PORT_CLOUDSQL_MYSQL` | `endpoints.cloudsqlMySQL` | `9017` | Cloud SQL for MySQL host endpoint (`--services cloudsql-mysql`). |
| `--port-kms` | `CLOUDBURROW_PORT_KMS` | `endpoints.kms` | `9018` | Cloud KMS host endpoint (`--services kms`). See [Cloud KMS](#cloud-kms). |
| `--port-cloudsql` | `CLOUDBURROW_PORT_CLOUDSQL` | `endpoints.cloudsql` | `9019` | Cloud SQL for PostgreSQL host endpoint (`--services cloudsql`). `0` = OS-assigned: `env` then exports `PGPORT` only from a running instance. Moves with `--port-base`. See [cloudsql.md](cloudsql.md). |
| `--port-ingress` | `CLOUDBURROW_PORT_INGRESS` | `endpoints.ingress` | `9080` | Host port of the cluster ingress gateway. **Fixed when the cluster is created**; `0` publishes nothing. See [Cluster ingress](#cluster-ingress). |
| `--port-console` | `CLOUDBURROW_PORT_CONSOLE` | `endpoints.console` | `9090` | The web console. See [Console](#console). |
| `--cluster-provider` | `CLOUDBURROW_CLUSTER_PROVIDER` | `cluster.provider` | `kind` | Only `kind` is supported (ADR-0005). |
| `--node-image` | `CLOUDBURROW_NODE_IMAGE` | `cluster.nodeImage` | `kindest/node:v1.36.4@sha256:099e…aed` | Pinned node image (the multi-arch index digest, #597), which fixes the Kubernetes version. **Must carry a tag or digest.** |
| `--namespace` | `CLOUDBURROW_NAMESPACE` | `cluster.namespace` | `cloudburrow` | Namespace for managed workloads. |
| `--kubeconfig` | `CLOUDBURROW_KUBECONFIG_PATH` | `cluster.kubeconfig` | `<state-dir>/<name>/kubeconfig` | Explicit kubeconfig path. **Never the developer's default file.** |
| `--mode` | `CLOUDBURROW_MODE` | `mode` | `persistent` | `ephemeral` or `persistent`. See below. |
| `--state-dir` | `CLOUDBURROW_STATE_DIR` | `stateDir` | `~/.cloudburrow` | Host-side artifacts only. Application state lives in the cluster. |
| `--services` | `CLOUDBURROW_SERVICES` | `services` | all | Comma-separated list. The defaults are `storage,pubsub,tasks,run,secretmanager`; the opt-in services are `firestore`, `datastore`, `bigtable`, `spanner`, `cloudsql`, `bigquery`, `memorystore`, `cloudsql-mysql`, `scheduler`, `logging` and `kms`. |
| `--shutdown-timeout` | `CLOUDBURROW_SHUTDOWN_TIMEOUT` | `shutdownTimeout` | `30s` | Bounded drain window on shutdown. |
| `--ready-timeout` | `CLOUDBURROW_READY_TIMEOUT` | `readyTimeout` | `5m` | Bounded wait for cluster components to become ready. |
| `--log-level` | `CLOUDBURROW_LOG_LEVEL` | `logLevel` | `info` | `trace`, `debug`, `info`, `warn`, `error`. See [Request logging](#request-logging). |
| `--hooks-dir` | `CLOUDBURROW_HOOKS_DIR` | `hooksDir` | `.cloudburrow/hooks` | Directory of `ready.d` and `shutdown.d` scripts. The default, or a `hooksDir` from a discovered file, needs [trust](#trust). See [Lifecycle hooks](#lifecycle-hooks). |
| `--hook-timeout` | `CLOUDBURROW_HOOK_TIMEOUT` | `hookTimeout` | `5m` | Time limit for each hook script; one that runs past it is killed. |
| `--hook-env` | `CLOUDBURROW_HOOK_ENV` | `hookEnv` | none | Comma-separated variables of `up`'s environment passed to hooks beyond the minimal set. |
| `--seed-file` | `CLOUDBURROW_SEED_FILE` | `seedFile` | unset | Seed document (the `/admin/seed` body) applied every time `up` starts. See [Admin API](#admin-api). |
| `--local-ai-model` | — | `localAI.modelPath` | unset | Host path to a `.litertlm` model. Setting it enables the local generation endpoint; unset, nothing is bound. `up` fails at startup if the file does not exist (#602). See [generation.md](generation.md). |
| `--local-ai-model-id` | — | `localAI.modelId` | the catalogue ID for the artifact's filename | The model ID clients must request, and the one reported as the model that ran. |
| `--local-ai-image` | — | `localAI.image` | a release: `ghcr.io/cloudburrow/litert-lm@sha256:…`; a checkout build: `cloudburrow/litert-lm:local` | The runtime image. A release CLI defaults to the image **its own release published**, pinned by digest (the digest is in the release notes) and pulled once, the first time it is needed. A build from a checkout defaults to `cloudburrow/litert-lm:local`, which `make litert-lm` builds, is published nowhere and is never pulled, so `up` fails at startup if it is absent. Any other image is pulled once. See [generation.md](generation.md). |
| `--local-ai-alias` | — | `localAI.aliases` | none | Comma-separated model IDs answered by the configured model. An explicit substitution; nothing is aliased by default. |
| `--port-localai` | — | `endpoints.localAI` | `0` (OS-assigned) | Host port of the local generation endpoint. Requires `--local-ai-model`. |
| `--detach` | — | — | `false` | `up` only: run in the background and return once `/readyz` answers 200. See [Running in the background](#running-in-the-background). |
| `--detach-timeout` | — | — | `10m` | `up` only: how long `--detach` waits for readiness. |
| `--offline` | — | — | `false` | `up` only: use only the artifacts `cloudburrow prefetch` stored in the state directory, refusing before anything is created if one is missing. See [install.md](install.md#offline-and-air-gapped-use). |
| `--trust` | — | — | `false` | `up` only: trust this directory's `./cloudburrow.json` and hooks as they are now, then start. See [Trust](#trust). |

**Refused variables.** `CLOUDBURROW_STORAGE_BACKEND` (file key `storage.backend`) was removed:
CloudBurrow's own Cloud Storage server is the only backend (#519), so `up` refuses to start while
it is set. It has no flag.

The table is checked: `TestConfigurationDocCoversEveryFlagAndVariable` in `internal/config` and
`TestConfigurationDocCoversUpOwnFlags` in `cmd/cloudburrow` fail when a flag or `CLOUDBURROW_*`
variable has no row here.

### Request logging

`up` writes one line to stderr per request to an in-process API (Cloud Tasks, Cloud Run,
Secret Manager and Cloud KMS, #392; Cloud Scheduler, Cloud Logging and Resource Manager, #587), in
the form `<service>.<Method> => <code> (<message>)`. It's in `up.log` too, and
`cloudburrow logs --service <service>` shows one service's lines:

```
INFO  tasks.GetQueue => NOT_FOUND (queue projects/p/locations/l/queues/q not found)
DEBUG tasks.ListQueues => OK
```

- `info` (the default) logs only requests that fail.
- `debug` logs every request.
- `trace` also logs each request's gRPC metadata. The values of `authorization`,
  `proxy-authorization`, `metadata-flavor`, `cookie`, `x-goog-api-key` and
  `x-goog-iam-authorization-token` are replaced with `[REDACTED]`.
- Request and response bodies are never logged at any level, so a secret payload cannot
  reach the log.

The same level applies to CloudBurrow's own log lines. An invalid level is reported by
config loading together with every other invalid setting. Only unary gRPC calls have the
request line so far: JSON requests and streaming calls do not. Requests to upstream emulators (Storage, Pub/Sub and the opt-in
backends) appear in those emulators' own logs, which `cloudburrow logs` reads.

### Tracing

`up` exports OpenTelemetry traces **only when** `OTEL_EXPORTER_OTLP_ENDPOINT` or
`OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` is set (#313). With neither set no exporter is created
and nothing is dialled, which is tested. Spans go to that endpoint and nowhere else.

| Variable | Meaning |
|---|---|
| `OTEL_EXPORTER_OTLP_ENDPOINT` / `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` | The collector, such as `http://127.0.0.1:4318`. Setting either turns tracing on. |
| `OTEL_EXPORTER_OTLP_PROTOCOL` / `OTEL_EXPORTER_OTLP_TRACES_PROTOCOL` | `http/protobuf` (the default) or `grpc`. Anything else stops `up` with an error. |
| `OTEL_EXPORTER_OTLP_HEADERS`, `…_TIMEOUT`, `…_INSECURE` and the rest | Read by the OpenTelemetry SDK's exporter, as it documents them. |

**Traced hops:**
- **Cloud Tasks, Cloud Run, Secret Manager, Cloud KMS (#393), Cloud Scheduler, Cloud Logging and
  Resource Manager (#600) gRPC calls**: one server span per call, named `<service>/<Method>`,
  continuing an incoming `traceparent`. `up` names these services on stderr when tracing is on.
- **Cloud Tasks dispatch:** a client span per attempt, a child of the `CreateTask` call's span,
  with `traceparent` injected into the HTTP request. A task whose own headers already set
  `traceparent` keeps it.

**Not traced:**
- the Secret Manager, Cloud Tasks, Cloud KMS and Resource Manager JSON APIs, and Cloud Scheduler
  jobs' deliveries to their targets;
- Storage, Pub/Sub and the opt-in emulators, which are upstream processes behind a raw
  port-forward;
- the metadata server, the console and the control API.

A trace that passes through one of these breaks at that hop.

**Any port may be set to `0`** to request an OS-assigned port. Several may be `0` at once —
they are not treated as duplicates.

### Running two instances

A second instance needs its own name **and** its own ports. The name gives it its own cluster,
namespace, kubeconfig and state; it does not move a single port, so a second `up` at the defaults
collides with the first on every one of them. `--port-base` moves them all at once:

```sh
cloudburrow up --name beta --port-base 9100
eval "$(cloudburrow env --name beta --port-base 9100)"
```

`beta` then serves control on `9100`, Storage on `9101`, Pub/Sub on `9102` and so on, the ingress
on `9180` and the console on `9190`: the default layout moved by 100. Pass the same `--port-base`
(or set `CLOUDBURROW_PORT_BASE`, or `portBase` in the file) to every command for that instance, so
`env`, `status` and `doctor` compute the same addresses `up` binds. A multiple of 100 keeps each
instance's block clear of the next.

Before creating anything, `up` checks every fixed port it is about to bind — the always-on
surfaces, each enabled service, the console, and the ingress the cluster will publish — and, if any
is taken, fails naming each one with the flag that moves it. `doctor` checks the same set. An
explicit `--port-*` still wins over the base, and validation names the endpoint an explicit port
collides with.

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

Only you can opt in: `--allow-remote` or `CLOUDBURROW_ALLOW_REMOTE=true`. A config file cannot,
and a `./cloudburrow.json` found in the working directory cannot even name a non-loopback
`bindAddress`, so running `up` in someone else's repository never exposes anything
([Configuration](#configuration)).

**CloudBurrow never changes your global kubecontext.** It writes and uses an explicit
kubeconfig, and only ever acts on clusters and resources it created, identified by the
`cloudburrow.dev/owned` and `cloudburrow.dev/instance` labels.

## Admin API

The control port also serves the admin API. It is **loopback-only whatever the bind address**,
and is refused on service ports — reset destroys data, so an application pod can reach the
service APIs it needs and cannot reach the endpoint that wipes state.

**Every `/admin` route needs the instance's admin token** (#553): `up` mints one per instance
and keeps it owner-only in `<state-dir>/<name>/admin-token`, removed at `stop`. Send it as
`Authorization: Bearer <token>`; without it, or with a wrong one, the answer is 401 and nothing
is touched. `/healthz`, `/readyz` and `/metrics` stay open. The token exists because a loopback
bind is not the wall it looks like: on Docker Desktop a workload in the cluster reaches the
host's loopback through `host.docker.internal`, admin API included (measured on #553).
`cloudburrow reset`, `seed`, `events`, `state` and `diagnose` send it themselves, never
printing it, and it is never written into the runtime file or a diagnose bundle.

| Endpoint | Purpose |
|---|---|
| `POST /admin/reset` | Destroy CloudBurrow-managed state, keeping the cluster |
| `POST /admin/seed` | Create resources from a seed document |
| `GET /admin/events` | Recent events, newest first, filterable by `service`, `kind` and `since` |
| `GET /metrics` | Request counters and latency histograms for every service with a call observer: Cloud Tasks, Cloud Run, Secret Manager, Cloud KMS, Cloud Scheduler, Cloud Logging, Resource Manager and the builtin Cloud Storage server, in the Prometheus text format: `cloudburrow_requests_total{service,method,code}`, `cloudburrow_request_duration_seconds`, and `cloudburrow_service_measured{service} 0` for each enabled service with none (Pub/Sub and the opt-in emulators, whose calls go over a port-forward and are not seen). The unmeasured set is derived from the observers `up` registers, and the console's `/monitoring` page reads the same one (#600) |

From the CLI, against the running `up` of the configured instance (`--name`):

```sh
cloudburrow seed seed.json                   # POST /admin/seed; --if-not-exists to repeat safely
cloudburrow reset                            # POST /admin/reset
cloudburrow reset --service pubsub --project p
cloudburrow reset --reseed                   # reset, then re-apply up's --seed-file
cloudburrow events --service tasks --limit 20
cloudburrow events --since 10m --format json # one JSON object per line
```

Each exits 0 on success and 1 on a refusal or a failure, printing the API's error, and
per component for a partial reset. With no `up` running, `seed` and `events` exit 1, and
`reset` falls back to deleting the managed namespace ([above](#commands)).

The same with curl:

```sh
TOKEN="Authorization: Bearer $(cat ~/.cloudburrow/cloudburrow/admin-token)"
curl -H "$TOKEN" -X POST localhost:9000/admin/seed -d '{
  "components": {"tasks": {"queues": ["projects/p/locations/us-central1/queues/work"]}}
}'
curl -H "$TOKEN" -X POST localhost:9000/admin/reset
curl -H "$TOKEN" -X POST "localhost:9000/admin/reset?service=pubsub&project=p"
curl -H "$TOKEN" "localhost:9000/admin/events?service=tasks&limit=20"
```

**What is recorded.** Every API call on a port CloudBurrow serves itself — Cloud Tasks,
Secret Manager, the Cloud Run v2 adapter, Cloud KMS, Cloud Scheduler and Cloud Logging, over gRPC
and JSON — is an event with `kind`
`request`, the method as its `target`, and `detail` holding the resource it addressed, the
project, the canonical status code (`NOT_FOUND`, not gRPC's `NotFound`), `duration_ms` and the
transport. No payload and no query string is ever recorded: a Secret Manager value read
through the API does not reach the event log. Storage, Pub/Sub and the opt-in emulators are
**not** recorded, because their traffic goes over a raw port-forward to an upstream process.

`since` takes an RFC 3339 timestamp and returns only events strictly after it, so a caller
polling with the last `time` it saw gets each event once. The ring holds the most recent 1000
events.

**What a reset clears.** Cloud Tasks queues and tasks; every Cloud Storage bucket and object,
and the notification configurations on them; Pub/Sub subscriptions, snapshots and topics; and
Secret Manager secrets with all their versions. Each is cleared through the service's own API,
so a reset is only what a client could have done one call at a time. Two services are
deliberately left out:

- **Pub/Sub's internal event topic** survives, because it carries storage notifications to
  their topics and deleting it would stop them silently until the next restart.
- **Pub/Sub state in a project CloudBurrow has never seen** survives a full reset. The emulator
  cannot list projects, so "every project" means the instance's default project plus the
  registered ones. Resetting that project by name with `project=` does reach it.

`service=` narrows a reset to the services named. It may be repeated or comma-separated.
Services always reset in registration order (tasks, storage, pubsub, secretmanager) whatever
order they are named in: storage goes before Pub/Sub so that notification configurations are
removed before the topics they point at. `project=` limits a reset to one project, for the
services that can honour it: Cloud Tasks, Pub/Sub and Secret Manager. **Cloud Storage cannot**,
because its backend lists every bucket whatever project is asked for. A project-scoped reset
that includes storage is refused with a 400 naming it, rather than guessed at. An unknown
service name is refused the same way. Either refusal resets nothing.

**Seeding.** One document can seed Cloud Tasks queues, Cloud Storage buckets and objects,
Pub/Sub topics and subscriptions, Secret Manager secrets with their versions, and Cloud
Scheduler jobs (#600). The schema is
[`seed.schema.json`](seed.schema.json):

```sh
curl -X POST localhost:9000/admin/seed -d '{"components": {
  "storage": {"buckets": [{"name": "assets", "objects": [
    {"name": "config.json", "content": "{\"debug\": true}", "contentType": "application/json"},
    {"name": "logo.png", "contentBase64": "iVBORw0KGgo="}]}]},
  "pubsub": {"topics": [{"name": "projects/dev-project/topics/orders"}],
    "subscriptions": [{"name": "projects/dev-project/subscriptions/orders-sub",
      "topic": "projects/dev-project/topics/orders", "ackDeadlineSeconds": 30}]},
  "secretmanager": {"secrets": [{"name": "projects/dev-project/secrets/api-key",
    "versions": [{"data": "s3cret"}]}]},
  "scheduler": {"jobs": [{"name": "projects/dev-project/locations/us-central1/jobs/nightly",
    "schedule": "0 3 * * *", "timeZone": "Europe/London",
    "httpTarget": {"uri": "http://127.0.0.1:8080/nightly", "httpMethod": "POST", "body": "{}"}}]}
}}'
```

Each resource is created through the service's own API, so a seed can reach no state the
API would refuse, and a seeded upload triggers notifications as a client's does. A Scheduler
job takes the REST API's field names (`body` and `data` also have `bodyBase64` and
`dataBase64` forms), is checked by the same validation `CreateJob` applies, and is created
`ENABLED` with its next run computed from its schedule; output-only fields such as `state`
are not taken.

- **Every document is validated before anything is created.** An unknown component, an unknown
  field or a malformed value is a 400 that names the component and the field, and nothing is
  seeded.
- **Fields the emulator is not known to honour are refused by name**, never dropped: Pub/Sub
  `schemaSettings`, `kmsKeyName`, `bigqueryConfig`, `cloudStorageConfig` and
  `enableExactlyOnceDelivery`, and a Scheduler job's `appEngineHttpTarget`, `oauthToken` and
  `oidcToken`, which the API itself refuses as `UNIMPLEMENTED`. Accepting a schema and ignoring
  it would promise validation the application never gets. Bucket `labels`, `location` and `storageClass` are seeded and kept (#503).
- **Re-seeding a resource that exists is a 409.** Set `ifNotExists: true` on a component to skip
  existing resources instead, which makes a seed safe to repeat; `?ifNotExists=true` on the
  request (`cloudburrow seed --if-not-exists`) sets it on every component. Objects are checked one by one.
  A secret that exists is skipped whole, versions included: versions have no names, so adding
  them again would duplicate them.
- Components are seeded in name order. A failure part-way through, such as a conflict, reports
  which components were already seeded.

**A seed file at startup.** `up --seed-file seed.json` (`CLOUDBURROW_SEED_FILE`, config
`seedFile`) applies a seed document, the same body `/admin/seed` takes, every time the instance
starts:

- **Validated before anything is created.** An unknown component, a component for a service that
  is not enabled, or a bad field makes `up` exit non-zero with the error, before a cluster exists
  or a credential is written. Nothing is seeded.
- **Applied once the services start and before the instance reports ready**, so `wait` and
  `up --detach` return with it in place, and ready.d hooks see it.
- **Idempotent.** Every component is applied with `ifNotExists`. A persistent instance restarted
  with the same file already has what the file declares, and that is not an error. A resource
  that has since been changed is left as it is.

`POST /admin/reset?reseed=true` resets, then re-applies the startup seed, so the instance holds
exactly what the file declares and nothing created since. With `service=`, only the named services
are reset and reseeded. It is refused, resetting nothing, when `up` had no seed file, or together
with `project=`, since a seed file is not scoped to one project.

**Reset attempts every component even when one fails** and
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

## The default project

Every instance serves one project by default. It is the project the console
opens on, the one the ADC fixture and the metadata server report, the one
`cloudburrow env` exports as `GOOGLE_CLOUD_PROJECT` and `CLOUDSDK_CORE_PROJECT`,
and the one the project registry is seeded with. `up` and `status` both print it.

It is the instance name **when the name is also a valid project ID** — 6–30
characters, lowercase letters, digits and hyphens, starting with a letter and not
ending with a hyphen. The default name, `cloudburrow`, qualifies, as does any
name that worked before this rule was written down.

A name that does not qualify is still a valid instance name — the instance-name
rule allows 1–32 characters and a leading digit — so the project is derived from
it, deterministically, by the smallest change that qualifies:

| Instance name | Default project | Why |
|---|---|---|
| `cloudburrow` | `cloudburrow` | Already a valid project ID |
| `demo` | `demo-local` | Shorter than 6 characters |
| `1box` | `cb-1box` | Starts with a digit |
| 31–32 characters | the first 30 | Longer than 30, with any trailing hyphen trimmed |

`--project` sets it explicitly, and wins over both. An invalid `--project` is
refused when the configuration loads rather than at the first API call.

**Why this exists.** The name used to be the project unconditionally. An instance
started with `--name demo` came up healthy, opened the console on project `demo`,
and then had every Cloud Run deploy and Cloud Tasks operation in that project
refused as a malformed resource name — with nothing pointing at the instance name
as the cause ([#255](https://github.com/cloudburrow/cloudburrow/issues/255)).

