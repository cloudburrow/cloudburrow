# Installing CloudBurrow

> **Current release: [v0.1.0](https://github.com/cloudburrow/cloudburrow/releases/tag/v0.1.0).**
> Install it with Homebrew or the install script below, or [build from source](#build-and-run).

## Install a release

Every release has one archive per platform (darwin and linux, arm64 and amd64), a
`checksums.txt` listing them all, and a **GitHub build attestation** for each archive and for the
checksum list. The attestation proves an archive was built by this repository's release workflow.
A checksum served beside the archive proves only that the two agree.

**Homebrew** (macOS and Linux):

```sh
brew install cloudburrow/tap/cloudburrow
cloudburrow version
```

**Install script.** It detects your OS and architecture, verifies the SHA-256 against
`checksums.txt`, and verifies the attestation too when the GitHub CLI (`gh`) is installed. It
installs to `~/.local/bin`, or to `<dir>/bin` with `--prefix <dir>`. **It refuses to install an
archive whose checksum does not match, or that has no entry in `checksums.txt`.**

```sh
curl -fsSL https://raw.githubusercontent.com/cloudburrow/cloudburrow/main/scripts/install.sh | sh
# a specific release, elsewhere:
curl -fsSL https://raw.githubusercontent.com/cloudburrow/cloudburrow/main/scripts/install.sh | sh -s -- --version v0.1.0 --prefix /usr/local
```

**Verify a download by hand:**

```sh
sha256sum -c checksums.txt --ignore-missing        # shasum -a 256 -c on macOS
gh attestation verify cloudburrow_v0.1.0_darwin_arm64.tar.gz --repo cloudburrow/cloudburrow
```

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

`cloudburrow doctor` checks all of this for you — see [Before the first run](#before-the-first-run).

## Build from source

Needs the Go toolchain in the table below; nothing else in these instructions does.

## Build and run

```sh
git clone https://github.com/cloudburrow/cloudburrow.git
cd cloudburrow
make build

./bin/cloudburrow up
```

## Before the first run

```sh
./bin/cloudburrow doctor
```

It changes nothing and exits non-zero only when something will actually stop `up`:

```
  ok      docker                   /usr/local/bin/docker
  ok      kind                     /opt/homebrew/bin/kind (v0.33.0)
  ok      kubectl                  /usr/local/bin/kubectl
  ok      docker daemon            29.8.0 (Docker Desktop)
  ok      docker memory            39.1 GiB
  ok      docker cpus              16
  ok      disk space               24.2 GiB free on /Users/wael (the host volume backing
                                   the Docker VM disk, not the VM filesystem)
  ok      port control             127.0.0.1:9000 is free
  ...
All checks passed.
```

It takes the same flags as `up`, so the ports it checks are the ports `up` would bind: the control,
metadata, console, Resource Manager and ingress ports always, each enabled service's, and the local
generation endpoint's when a model is configured. `up` runs the same port check before it creates a
cluster, and refuses with this report when one is taken.

| Level | Meaning |
|---|---|
| `ok` | Passed. |
| `warn` | Will probably work. An untested kind release, or disk below the 20 GiB margin. |
| `FAIL` | `up` will not succeed. Missing binary, stopped daemon, too little memory or CPU, taken port. **Exit code 1.** |
| `unknown` | Could not be measured. Deliberately not `ok` — an unanswerable question is not a passing answer. Does not block. |

Only `FAIL` blocks. Each non-`ok` line is followed by what to do about it.

**Disk space on macOS and Windows** is measured on the host volume backing the Docker VM
disk, not inside the VM — the VM's own filesystem is not visible from the host. The line
says which one it measured rather than presenting an unlabelled number.

CloudBurrow **never prunes Docker on your behalf**, so a low-disk warning tells you to free
space rather than doing it for you.

First start pulls the Kubernetes node image and installs Knative, so expect a few minutes.
Later starts reuse the cluster.

`up` prints everything you need:

```
cloudburrow "cloudburrow"
  control:    http://127.0.0.1:9000  (health: /healthz, readiness: /readyz)
  admin:      http://127.0.0.1:9000/admin/{reset,seed,events,state,faults}  (loopback only; Authorization: Bearer from ~/.cloudburrow/cloudburrow/admin-token)
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

### Pointing gcloud, Terraform and the SDKs at it

```sh
eval "$(./bin/cloudburrow env)"
```

Without this, Google's libraries find your **real** credentials and reach the network. See
[credentials.md](credentials.md).

### Cloud Run service URLs

A deployed service is served through the cluster ingress, published on `127.0.0.1:9080`:

```sh
curl http://hello.default.cloudburrow.localhost:9080/
```

The port is fixed when the cluster is created (`--port-ingress`), and `up` says so when a
cluster predates it. See [networking.md](networking.md), including which resolvers actually
resolve `*.cloudburrow.localhost` — **Go binaries built with `CGO_ENABLED=0` do not**.

### Two addresses, and they are not interchangeable

Code on **your machine** uses `127.0.0.1:<port>`. Code running **inside the cluster** must use
the in-cluster address — a pod's loopback is the pod itself. `up` prints both.

Cloud Storage is one server for both: in the cluster it is `storage.<namespace>.svc.cluster.local:4443`,
and it builds each response's links from the address the client used (#514).

### The storage scheme

`STORAGE_EMULATOR_HOST` must include `http://`. The Go client prepends a scheme when it is
absent, but the Python client uses the value verbatim — so the form with a scheme is the one
that works for both.

## Optional emulators

Firestore, Datastore, Bigtable and Spanner are available and **off by default**:

```sh
cloudburrow up --services storage,pubsub,firestore,spanner
```

Each is a Google-published emulator with an official environment variable, printed by `up`:

```
export FIRESTORE_EMULATOR_HOST=127.0.0.1:...
export DATASTORE_EMULATOR_HOST=127.0.0.1:...
export BIGTABLE_EMULATOR_HOST=127.0.0.1:...
export SPANNER_EMULATOR_HOST=127.0.0.1:...
```

**All four are in-memory.** Nothing they hold survives a restart, whatever `--mode` says.

Bigtable, like the others, starts from its pinned image with no network access (#611).

## Commands

| Command | Effect |
|---|---|
| `cloudburrow doctor` | Check workstation prerequisites, changing nothing |
| `cloudburrow diagnose -o bundle.tar.gz` | Collect a redacted diagnostics bundle to attach to a bug report |
| `cloudburrow env` | Print the environment that points Google tooling at this instance |
| `cloudburrow up` | Create the environment and run in the foreground |
| `cloudburrow status` | Report the instance, its endpoints and per-service persistence |
| `cloudburrow stop` | Stop the cluster, **preserving** state |
| `cloudburrow reset` | Destroy managed state, **keeping** the cluster (through the running `up` when there is one) |
| `cloudburrow seed <file>` | Create the resources in a seed document in the running `up` |
| `cloudburrow events` | Print the running `up`'s recent admin events |
| `cloudburrow delete` | Destroy the cluster |

These are distinct and none implies another.

## Running two environments

Give each a name, and every instance after the first its own block of ports with `--port-base`:

```sh
cloudburrow up --detach                              # the default instance, ports 9000-9090
cloudburrow up --detach --name beta --port-base 9100 # control 9100, ingress 9180, console 9190
eval "$(cloudburrow env --name beta --port-base 9100)"
```

The name gives each its own cluster, namespace, kubeconfig and state; `--port-base` moves every
fixed port, the ingress and console included. Pass both to every command for that instance. If a
port is still taken, `up` names each one before creating a cluster. See
[Running two instances](configuration.md#running-two-instances).

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
- **Loopback by default.** `--allow-remote` (or `CLOUDBURROW_ALLOW_REMOTE=true`) is required to
  bind anything else, and exposes an unauthenticated emulator plus a cluster to whoever can reach
  it. A config file cannot confirm it, and a `./cloudburrow.json` in the working directory cannot
  name a non-loopback address.
- **A repository's config and hooks need your trust.** `up` refuses a discovered
  `./cloudburrow.json` or `.cloudburrow/hooks` scripts until you have read them and run
  `cloudburrow trust` (or `up --trust`); a change asks again. Hooks get a minimal environment, not
  your shell's. See [Trust](configuration.md#trust).
- **Your kubecontext is never changed.** CloudBurrow writes its own kubeconfig.
- **Only its own clusters are touched**, identified by the `cloudburrow` name prefix and
  `cloudburrow.dev/owned` labels.

## Upgrading

Install the new release as you installed the first, then run `cloudburrow up` as usual. Every
cluster is stamped with the versions it was built from: a ConfigMap `cloudburrow-stamp` in
`kube-system` records the CLI that last ran `up`, the node image the cluster was created with and
the Knative release last applied. `up` compares them with the new release's pins:

| What changed in the release | What `up` does to an existing cluster |
|---|---|
| The backends (Pub/Sub, storage, the optional emulators) | Re-applies them, as on every `up` |
| The Knative release, and Cloud Run is enabled | Applies the new Serving and Kourier manifests in place, verified by their pinned hashes, then records the new release |
| The node image (the Kubernetes version) | **Refuses to start**, naming both images |

**A node image cannot change in place.** When the pinned one differs from the cluster's, `up`
exits non-zero and says so. Either recreate the cluster, which **destroys it and every piece of
state in it**:

```sh
cloudburrow delete
cloudburrow up
```

or keep the old cluster for now by pinning its image, `--node-image` or `cluster.nodeImage`, to the
one the message names. The same image with and without its digest counts as the same, so a
cluster created before the pin carried a digest is not refused.

**Knative is only upgraded, never downgraded.** A cluster with a newer Knative than the CLI pins,
because an older release ran `up` against it, is refused rather than overwritten; run the newer
release, or `cloudburrow delete`. Knative itself supports upgrading one minor release at a time; a
release that pins a Knative more than one minor ahead of your cluster's may fail to apply, and
then `cloudburrow delete` is the way through.

**A cluster from before the stamp** is handled rather than refused: its node image is read from the
node container, its Knative is treated as unrecorded and the pinned manifests are applied in place,
and both are then stamped. If the node image cannot be read, `up` says it is unknown and goes on.

`cloudburrow status` prints what the cluster is stamped with and what the next `up` will do about a
difference; `status --format json` has it under `cluster.versions`, beside `cluster.pinned`, and a
diagnose bundle carries it in `kubernetes/versions.json`.

## Uninstalling

```sh
cloudburrow delete          # remove the cluster
rm -rf ~/.cloudburrow       # remove host-side state
```
