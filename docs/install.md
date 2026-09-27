# Installing CloudBurrow

> **Current release: [v0.1.0](https://github.com/cloudburrow/cloudburrow/releases/tag/v0.1.0).**
> Install it with Homebrew or the install script below, or [build from source](#build-and-run).

## Install a release

Every release has one archive per platform (darwin and linux, arm64 and amd64), a
`checksums.txt` listing them all, and a **GitHub build attestation** for each archive and for the
checksum list. The attestation proves an archive was built by this repository's release workflow.
A checksum served beside the archive proves only that the two agree.

**Only a commit CI has passed is released.** The release workflow's first job,
`require-ci-green`, looks up the tagged commit's `ci-green` check (the one check the merge queue
requires, which fails when any CI job failed) and stops the release before anything is built
unless one has succeeded for that exact SHA. A commit merged through the merge queue already has
one: the queue tested that same commit. If CI for the commit is still running, the job waits for
it, checking once a minute for up to 45 minutes; a commit with no CI run, or none that passed,
fails at once with an error naming the SHA.

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

On macOS, see [Gatekeeper](#gatekeeper) for whether a release's binaries are signed and
notarized, and what to do with one that is not.

## Gatekeeper

**What is signed.** The release workflow can sign both macOS binaries (darwin arm64 and amd64)
with a Developer ID Application certificate, with the hardened runtime and a secure timestamp,
and have Apple notarize them. It does so **only once the maintainer has added the Apple signing
secrets to the repository** (listed below). Until then, and for any release built without them,
the macOS binaries are **unsigned and not notarized**; each release's notes say which it is. Only
the archives published by this repository's release workflow are ever signed: a binary you build
from source, or one from a fork's release, is not.

Signing happens before `checksums.txt`, the Homebrew formula and the build attestations are
computed, so all three describe the signed archives, and `gh attestation verify` covers them as
it covers every other archive.

**How to check a release binary**, after extracting the archive:

```sh
codesign -dv --verbose=2 cloudburrow   # Authority=Developer ID Application: ..., and a Timestamp= line
spctl -a -t exec -vv cloudburrow       # accepted, source=Notarized Developer ID
gh attestation verify cloudburrow_v0.1.0_darwin_arm64.tar.gz --repo cloudburrow/cloudburrow
```

A command-line binary cannot carry a stapled notarization ticket, so the first time it runs
Gatekeeper looks the ticket up online; `spctl` does the same.

**An unsigned binary** (a release from before signing was configured, a fork's release, or your
own `make build`) is not blocked when you build it yourself: a binary compiled on your Mac carries
no quarantine attribute. A downloaded unsigned archive does, and macOS refuses to run it. Verify
it first, with the checksum and `gh attestation verify` above, and only then remove the
quarantine attribute from that one file:

```sh
xattr -d com.apple.quarantine cloudburrow
```

This switches off Gatekeeper's check for that binary, which is exactly the check a tampered
download would be caught by. Do it only for a file you have verified, never as a habit for
anything that fails to open, and prefer the install script or Homebrew, which verify before
installing and download with `curl`, which sets no quarantine attribute.

**For the maintainer: the repository secrets.** Signing turns on when all six are set, and the
job fails, rather than silently shipping unsigned binaries, if only some are:

| Secret | Value |
|---|---|
| `MACOS_SIGNING_CERT_P12_BASE64` | The Developer ID Application certificate and its private key, exported as `.p12`, base64-encoded |
| `MACOS_SIGNING_CERT_PASSWORD` | The `.p12` export password |
| `MACOS_SIGNING_IDENTITY` | The identity name, such as `Developer ID Application: Name (TEAMID)` |
| `APPLE_NOTARY_KEY_P8_BASE64` | An App Store Connect API key (`AuthKey_<id>.p8`), base64-encoded |
| `APPLE_NOTARY_KEY_ID` | That key's ID |
| `APPLE_NOTARY_ISSUER_ID` | The App Store Connect issuer ID |

With none set, the signing job passes with a notice and the release publishes unsigned archives.
A manual dispatch of the release workflow signs and notarizes too, without publishing, so signing
can be checked before a tag. It is held to the same CI gate, so dispatch it on a commit CI has
passed.

## Prerequisites

| Tool | Why | Check |
|---|---|---|
| **Docker** | Runs the cluster nodes | `docker info` |
| **kind** v0.33.0 | Creates the cluster | `kind version` |
| **kubectl** | Cluster operations | `kubectl version --client` |
| **Go** | Building from source | `go version` |

Both `arm64` and `amd64` are supported. Verified on macOS (Docker Desktop) and Linux.

**Windows is unsupported outside WSL2, and untested inside it.** The CLI does not compile for
Windows (`GOOS=windows go build ./cmd/cloudburrow` fails in `internal/hooks`, `internal/doctor`
and `internal/localai`), and no release has a Windows archive. Under WSL2 the Linux release is
what would run; nobody has verified that it does.

**Resource budget, measured:** the full stack — cluster, Knative, both storage endpoints,
Pub/Sub, 20+ pods — uses about **1.5 GiB** of memory and requests roughly **2.1 CPU**. Knative's
own guidance for a local install is 3 CPU / 3 GB, which this is consistent with. Give Docker
at least 4 CPU and 6 GB.

`cloudburrow doctor` checks all of this for you — see [Before the first run](#before-the-first-run).

## Build from source

Needs the Go toolchain in the [Prerequisites](#prerequisites) table above; nothing else in these instructions does.

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

**Disk space on macOS** is measured on the host volume backing the Docker VM
disk, not inside the VM — the VM's own filesystem is not visible from the host. The line
says which one it measured rather than presenting an unlabelled number.

CloudBurrow **never prunes Docker on your behalf**, so a low-disk warning tells you to free
space rather than doing it for you.

It also reports, one line each, whether the hosts a first `up` downloads from answer —
`reach registry docker.io`, `reach registry gcr.io`, `reach registry ghcr.io` and
`reach github.com`, each `reachable`, `unreachable` (a warning, with the error) or `unknown` — and
an `offline cache` line saying whether `cloudburrow prefetch` has stored everything `up` needs.
None of these blocks: a complete cache makes the network unnecessary.

First start pulls the Kubernetes node image and installs Knative, so expect a few minutes.
Later starts reuse the cluster. Without network access, see
[Offline and air-gapped use](#offline-and-air-gapped-use).

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
| `cloudburrow up` | Create the environment and run in the foreground; `--offline` uses only the cache |
| `cloudburrow prefetch` | Store what `up` downloads in the state directory, for [offline use](#offline-and-air-gapped-use) |
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

## Offline and air-gapped use

A first `up` downloads: the kind node image from Docker Hub, each enabled backend's image from
its registry (gcr.io, ghcr.io, Docker Hub), Knative's release YAMLs from GitHub and the images
those YAMLs name from gcr.io and Docker Hub. Later starts reuse the cluster and download nothing,
but a `delete` and `up`, or a new machine, downloads it all again. `cloudburrow prefetch` stores
all of it in the state directory, and `up --offline` then needs none of those hosts (#604).

1. On a machine that can reach the network, with **the same `cloudburrow` binary** and the same
   `--services` you will run offline:

   ```sh
   cloudburrow prefetch --services storage,pubsub,tasks,run,secretmanager,bigtable
   ```

   It prints every artifact it stored and its size. Run it again at any time; what is already
   stored is kept.

2. Copy the state directory (`~/.cloudburrow`, or your `--state-dir`) to the offline machine.
   Only its `cache/` directory is needed; it is shared by every instance in that state directory.

3. On the offline machine, which needs Docker, kind and kubectl as always:

   ```sh
   cloudburrow up --offline --services storage,pubsub,tasks,run,secretmanager,bigtable
   ```

   `up --offline` checks the cache before it creates anything, and when an artifact is missing it
   refuses, naming it and where it was expected. `cloudburrow doctor` reports the same, as an
   `offline cache` line.

Without `--offline`, `up` still prefers what is cached and downloads only what is not.

What `prefetch` stores in `<state dir>/cache`, and how:

- **Images `up` gives Docker** — the kind node image, and the builtin Cloud Storage server's image,
  which this CLI builds from the storage server it embeds on a digest-pinned distroless base — are
  saved with `docker save`. `up` loads them with `docker load` when Docker does not have them.
- **Images the cluster runs** — each backend's, and Knative's — are pulled by a throwaway kind node
  (`cloudburrow-prefetch-<random>`, deleted afterwards) and exported by its containerd, the runtime
  that imports them at `up`. Docker is not used for these: `docker save` wrote the Spanner
  emulator's image with no layers and exited 0 (Docker 29.8.0, measured), so every archive is also
  checked against its own manifest before it is kept. `up` imports each into the node before
  anything that runs it is applied.
- **Knative's YAMLs** are kept only when they match the sha256 CloudBurrow pins, and are checked
  against it again every time `up` applies them.

Every image is the reference CloudBurrow pins by digest, or the one a checksummed Knative YAML
names, with one exception: Kourier's YAML names its Envoy gateway by tag,
`docker.io/envoyproxy/envoy:v1.37-latest`. Prefetch stores what that tag resolved to when it ran,
and `prefetch` marks the line.

Sizes, **measured** by `cloudburrow prefetch` on linux/arm64 (Docker Desktop 29.8.0 on macOS),
the size of each archive in the cache. linux/amd64 sizes were not measured and will differ.

| Artifact | Needed for | Size |
|---|---|---|
| `kindest/node:v1.36.4@sha256:099e0493…` | every instance | 335.4 MiB |
| `dev.local/cloudburrow-storage:<hash>` (built by this CLI) | Cloud Storage | 7.1 MiB |
| `gcr.io/google.com/cloudsdktool/google-cloud-cli@sha256:3294e8a5…` | Pub/Sub, Firestore, Datastore, Bigtable | 350.7 MiB |
| `serving-crds.yaml`, `serving-core.yaml` (Knative Serving `knative-v1.23.0`) | Cloud Run | 0.4 MiB, 0.5 MiB |
| `kourier.yaml` (net-kourier `knative-v1.23.0`) | Cloud Run | 24.8 KiB |
| Knative `queue`, `activator`, `autoscaler`, `controller`, `webhook` images | Cloud Run | 10.5, 18.2, 18.4, 20.9, 18.1 MiB |
| Knative `kourier` image | Cloud Run | 20.2 MiB |
| `docker.io/envoyproxy/envoy:v1.37-latest` (by tag; see above) | Cloud Run | 61.9 MiB |
| `gcr.io/cloud-spanner-emulator/emulator@sha256:c6f3402f…` | Spanner | 57.9 MiB |
| `postgres:17-alpine@sha256:b0f9560a…` | Cloud SQL (PostgreSQL) | 109.7 MiB |
| `mysql:8.4@sha256:0744ee5e…` | Cloud SQL for MySQL | 222.9 MiB |
| `ghcr.io/goccy/bigquery-emulator@sha256:f4e428d2…` | BigQuery | 86.3 MiB |
| `valkey/valkey:8.1-alpine@sha256:081c2f5c…` | Memorystore | 17.1 MiB |

The default services plus Bigtable are 13 artifacts, **862.3 MiB**; every service is 18,
1356.3 MiB. Cloud Tasks, Secret Manager, Cloud KMS, Cloud Scheduler, Cloud Logging and Resource
Manager run in the CLI and need nothing.

What this is tested to do, and what it is not:

- **Tested:** `TestPrefetchThenUpOfflineWithNoEgress` (build tag `integration`, run with
  `CLOUDBURROW_TEST_OFFLINE=1`) prefetches, creates the cluster, cuts the node's outbound network
  with iptables inside the node, and `up --offline --detach` for the default services plus Bigtable
  reaches ready with no image pulled by the kubelet. Unit tests prove `up --offline` refuses
  before creating anything when an artifact is missing, and that with a complete cache it runs no
  `docker pull`, no pull inside the node, and no download.
- **Not tested:** a machine with no network at all. The test cuts the node's network, not the
  Docker daemon's or the CLI's; that those make no request is shown by the unit tests, not by a
  network that refuses them. A Docker network created with `--internal` could not be used: kind's
  node entrypoint needs the network's gateway and the node exits at start (kind v0.33.0).
- **Docker's containerd image store** is what this was measured with (Docker Desktop 29.8.0).
  Docker's classic image store was not tested. If a loaded image cannot then be found by its
  pinned reference, `up` stops and says so rather than pulling it.
- **Not covered:** the local AI runtime image (`--local-ai-model`), the buildpacks builder that
  `gcloud run deploy --source` uses, and your own Cloud Run images. An image a Cloud Run service
  names must already be in the cluster, as it must be without `--offline`.
- **Architecture:** prefetch stores the images for its own Docker daemon's architecture. Prefetch
  on a machine of the same architecture as the offline one.

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

**What accumulates across upgrades.** `up` builds the in-cluster storage image on your Docker
daemon, tagged with a hash of the CLI binary, so every new release (or `make build`) adds a
`dev.local/cloudburrow-storage:<hash>` tag and leaves the old one. They are CloudBurrow's own and
safe to remove when no instance is running: `up` rebuilds the one it needs.

```sh
docker images 'dev.local/cloudburrow-storage' --format '{{.Repository}}:{{.Tag}} {{.CreatedSince}}'
docker images 'dev.local/cloudburrow-storage' -q | xargs -r docker rmi
```

## Uninstalling

Each step removes something the one before leaves behind. Pass each instance the same `--name`
and `--state-dir` it was started with.

1. **Every instance.** Each is a directory under the state directory (`~/.cloudburrow` by
   default) and a kind cluster named `cloudburrow-<name>`:

   ```sh
   ls ~/.cloudburrow                                  # one directory per instance name
   kind get clusters | grep '^cloudburrow-'           # one cluster per instance
   cloudburrow delete --name <name>                   # for each; add --state-dir if you used one
   ```

   `delete` stops the instance, deletes its cluster and removes its state directory.

2. **gcloud configurations**, for each instance you ran `gcloud-setup` for:

   ```sh
   eval "$(cloudburrow gcloud-teardown --name <name>)"
   ```

   or remove the files it wrote, `configurations/config_cloudburrow-*` under gcloud's configuration
   directory (`gcloud info --format='value(config.paths.global_config_dir)'`). Your default
   configuration was never changed.

3. **The binary**, the way you installed it:

   ```sh
   brew uninstall cloudburrow && brew untap cloudburrow/tap   # Homebrew
   rm "$HOME/.local/bin/cloudburrow"                          # install.sh, or <prefix>/bin with --prefix
   ```

4. **Host-side state**, including any directory you passed as `--state-dir`. It also holds the
   offline cache `cloudburrow prefetch` wrote (`<state-dir>/cache`):

   ```sh
   rm -rf ~/.cloudburrow
   ```

5. **Images CloudBurrow built or pulled** onto your Docker daemon. `up` builds one
   `dev.local/cloudburrow-storage` tag per CLI build, and `prefetch` and `up --offline` load the
   pinned kind node image. The local AI runtime is `cloudburrow/litert-lm:local` from
   `make litert-lm`, or `ghcr.io/cloudburrow/litert-lm` from a release:

   ```sh
   docker images 'dev.local/cloudburrow-*' -q | xargs -r docker rmi
   docker images 'cloudburrow/litert-lm' -q | xargs -r docker rmi
   docker images 'ghcr.io/cloudburrow/litert-lm' -q | xargs -r docker rmi
   ```

   Remove the `kindest/node` image only if nothing else on the machine uses kind.
