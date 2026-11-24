# CloudBurrow

[![CI](https://github.com/cloudburrow/cloudburrow/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/cloudburrow/cloudburrow/actions/workflows/ci.yml?query=branch%3Amain)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

A local Google Cloud emulator for development and testing.

CloudBurrow runs a local Kubernetes cluster that speaks Google Cloud APIs, so you can build
and test applications on your own machine with the official Google Cloud SDKs, without
deploying to GCP for every change. The SDKs tested today are Go's (`test/compat`), Python's
(`test/compat-python`) and Node.js's (`test/compat-node`: Storage, Pub/Sub, Cloud Tasks, Secret
Manager and Firestore), all run against a live instance in CI. No other language is tested, and
Java is not planned (#718). Because it is a real Kubernetes cluster, `kubectl`, Helm
charts and operators work against it directly. Website: [cloudburrow.com](https://cloudburrow.com).

## Status

**Released: [the latest release](https://github.com/cloudburrow/cloudburrow/releases/latest)**, with a new one every one to two days
([docs/ci.md](docs/ci.md#releasing)). This page describes **v0.6.0**, tagged 2026-11-23. Every archive,
and the one `checksums.txt`, carries a GitHub build attestation, and the installer refuses an
archive it cannot verify. The acceptance workflow passes end to end: upload an object, publish an
event, a Cloud Run worker receives it and reads the object, writes a result, and the result is read
back. Every step goes through an official Google SDK, including inside the worker.

What is supported is recorded **per operation** in
[docs/compatibility.md](docs/compatibility.md), and every row marked `Verified` names the test
that proves it. Only a `Verified` row is a support claim: an operation is called supported once a
merged test drives it through an official Google Cloud client library or tool, and
implemented-but-undriven operations are listed as such. [docs/status.md](docs/status.md) is the
one-page version, [docs/gaps.md](docs/gaps.md) lists every remaining gap, and
[docs/coverage](docs/coverage/README.md) lists every RPC of every served API with its status,
generated from the proto surface: 2,152 of 2,382 methods Verified across 58 API surfaces.

## Install

```sh
brew install cloudburrow/tap/cloudburrow        # macOS and Linux
```

or the install script, which verifies the SHA-256 against `checksums.txt` (and the attestation
when `gh` is installed) and installs to `~/.local/bin`:

```sh
curl -fsSL https://raw.githubusercontent.com/cloudburrow/cloudburrow/main/scripts/install.sh | sh
# a specific release, elsewhere:
curl -fsSL https://raw.githubusercontent.com/cloudburrow/cloudburrow/main/scripts/install.sh | sh -s -- --version vX.Y.Z --prefix /usr/local
```

Both install the latest release; `--version` takes any tag on the
[releases page](https://github.com/cloudburrow/cloudburrow/releases). To build from source, see
[Building from source](#building-from-source). Signing, notarization and verifying a download by
hand are in [docs/install.md](docs/install.md).

**Requirements:** macOS or Linux, on arm64 or amd64, with **Docker**, **kind** (v0.33.0) and
**kubectl**; Go only to build from source. Give Docker at least 4 CPU and 6 GB: the default stack
measures about 1.5 GiB and 2.1 CPU, and each opt-in service adds to it
([Footprint](docs/status.md#footprint)). Windows is unsupported outside WSL2, and untested inside
it. `cloudburrow doctor` checks all of it and changes nothing
([Prerequisites](docs/install.md#prerequisites)).

## Quick start

```sh
cloudburrow doctor      # checks prerequisites, changes nothing
cloudburrow up          # creates the cluster and runs in the foreground
```

In another shell, point the Google client libraries at it:

```sh
eval "$(cloudburrow env)"
```

That exports endpoint overrides, the project, a local metadata server and a generated ADC file.
The credentials authorise nothing; they exist so tooling that insists on them runs offline. See
[docs/credentials.md](docs/credentials.md). Opt-in services are chosen with `--services`, for
example `cloudburrow up --services storage,pubsub,firestore,bigquery`
([docs/configuration.md](docs/configuration.md)).

`stop`, `reset` and `delete` are distinct commands, and none implies another.

## Using gcloud with no Google login

CloudBurrow never needs a Google account. `gcloud-setup` writes a gcloud configuration of its
own, `cloudburrow-<name>`, with the instance's project, credentials disabled
(`auth/disable_credentials`) and endpoint overrides for the services whose gcloud use is Verified,
and selects it for the current shell only:

```sh
eval "$(cloudburrow gcloud-setup)"      # sets CLOUDSDK_ACTIVE_CONFIG_NAME for this shell
gcloud storage ls
gcloud pubsub topics list
eval "$(cloudburrow gcloud-teardown)"   # back to your own configuration
```

Your default configuration and `active_config` are never touched, teardown removes only a file
carrying CloudBurrow's marker, and a same-named configuration you wrote yourself is refused, never
overwritten. A command for a service with no local override would go to Google unauthenticated and
be refused there. The overrides written, and which gcloud commands are Verified, are in
[docs/credentials.md](docs/credentials.md#a-gcloud-configuration-of-its-own-gcloud-setup) and
[docs/status.md](docs/status.md#languages-and-tools).

The console has the same thing built in: **Activate terminal** in its toolbar opens a Cloud
Shell-style drawer, with tabs, whose shell runs in a pod in the instance's own cluster, never on
your machine. It uses Google's Cloud SDK image (pinned by digest) with `gcloud-setup`'s overrides
at the in-cluster addresses, credentials disabled and no host credential or host path, scoped to
the toolbar's project. Unlike Cloud Shell it has no Google sign-in, no home directory beyond the
pod and a read-only kubectl ([compatibility.md](docs/compatibility.md#console)).
`cloudburrow terraform` does the same for Terraform and OpenTofu.

## What runs

Started by default:

| Service | Backed by | Scope |
|---|---|---|
| **Cloud Storage** | CloudBurrow | JSON, XML and gRPC (`google.storage.v2`) APIs on one port: buckets, objects, versioning, soft delete, retention and holds, lifecycle, CORS, CMEK and CSEK, IAM (stored), HMAC keys, multipart uploads, signed URLs, **notifications to Pub/Sub**; with it, the Storage Batch Operations, Insights and Transfer Service APIs ([docs/storage-companions.md](docs/storage-companions.md)) |
| **Pub/Sub** | Google's own emulator, behind CloudBurrow's front | Topics, subscriptions, publish, pull, StreamingPull, push delivery, IAM (stored) |
| **Cloud Tasks** | CloudBurrow | Queues, tasks, pause/resume, HTTP dispatch with retry and rate limits |
| **Cloud Run** | Knative Serving and Kubernetes, behind CloudBurrow's Cloud Run v2 and v1 adapters | Services and revisions with real containers, traffic splits, env from Secret Manager, volumes; **jobs and executions** as Kubernetes batch Jobs; **worker pools and instances**. Min-instances is verified; max-instances, concurrency, timeout and resource limits are mapped but not load-tested |
| **Artifact Registry, Cloud Build, Cloud Functions, Eventarc** | CloudBurrow, on the Cloud Run port | Repositories (Docker, language packages, Go modules, KFP) with cleanup policies and vulnerability scanning (Container Analysis, Google's OSV-Scanner run locally); builds and `gcloud run deploy --source` through Google Buildpacks; Cloud Functions v1 and v2 with Pub/Sub and Storage triggers; Eventarc triggers; Secure Source Manager and Developer Connect ([docs/functions-and-builds.md](docs/functions-and-builds.md)) |
| **Secret Manager** | CloudBurrow | Secrets and versions over gRPC and JSON. **Not a secret store**: nothing is authenticated |
| **Vertex AI (local)** | CloudBurrow's runtimes, on the local AI endpoint | Online prediction, the Model Registry and Endpoints, **Model Garden deploys of Hugging Face models**, Document AI processors and Speech-to-Text; generation and embeddings when enabled ([below](#local-ai)) |

Always served, whatever `--services` says:

| Service | Backed by | Scope |
|---|---|---|
| **Resource Manager** | CloudBurrow | v3 and v1: projects, folders, the local organization, tags, liens; `gcloud projects` and Terraform's `google_project` |
| **IAM** | CloudBurrow | Custom and predefined roles, service accounts and keys, IAM Credentials token minting, deny and principal access boundary policies, organization policies, Workload and Workforce Identity Federation with STS, Cloud Identity groups. Policies are **stored, not enforced**, unless `--iam-enforcement=on` ([below](#what-will-not-work)) |
| **Service Usage, API Keys, Cloud Quotas, Cloud Billing, Site Verification** | CloudBurrow | The project-administration APIs, at Google's paths ([docs/serviceusage.md](docs/serviceusage.md), [docs/apikeys.md](docs/apikeys.md), [docs/billing.md](docs/billing.md)) |

Opt-in with `--services`:

| Service (`--services` name) | Backed by | Scope |
|---|---|---|
| **Firestore** (`firestore`) | Google's own emulator, with CloudBurrow's Admin API and front | Documents, queries, transactions, listeners, pipelines, the Admin API (databases, indexes, backups, PITR, CMEK); **Firestore with MongoDB compatibility** over the MongoDB wire protocol; the Firebase Rules API. State survives a restart in persistent mode |
| **Datastore** (`datastore`) | Google's own emulator, with CloudBurrow's front | Entities, kinds, GQL, aggregations, the Admin API (indexes, export and import) |
| **Bigtable** (`bigtable`) | Google's own emulator, with CloudBurrow's front | Tables, rows, the instance admin API, backups, snapshots, authorized views, change streams, GoogleSQL for Bigtable |
| **Spanner** (`spanner`) | Google's own emulator, with CloudBurrow's front | Instances, databases, DDL, queries, transactions, backups, query modes, graph queries |
| **BigQuery** (`bigquery`) | A community emulator, `goccy/bigquery-emulator`, built from source with CloudBurrow's patches | Datasets, tables, jobs, GoogleSQL, the Storage Read and Write APIs, BigQuery ML on local models; with it, Analytics Hub and the Data Transfer Service. In memory: nothing survives a restart ([compatibility.md](docs/compatibility.md#bigquery-is-a-community-emulator)) |
| **Cloud SQL** (`cloudsql`, `cloudsql-mysql`) | Real PostgreSQL and MySQL servers in the cluster | Google publishes no Cloud SQL emulator, so each instance is a real server, reached with an ordinary driver, the Cloud SQL connectors or the Auth Proxy; CloudBurrow serves the Cloud SQL Admin API over them (75 of 80 methods Verified). SQL Server only once its user accepts Microsoft's licence, on an amd64 node ([docs/cloudsql.md](docs/cloudsql.md)) |
| **Memorystore** (`memorystore`) | Valkey, and Redis 6.2 and 7.0 for those versions | The Memorystore for Redis, Redis Cluster and Valkey admin APIs; each instance you create is a real server of its own, reached with any Redis client ([docs/memorystore.md](docs/memorystore.md)) |
| **Cloud Scheduler** (`scheduler`) | CloudBurrow | Cron jobs in IANA time zones to HTTP and Pub/Sub targets, with retries and locally minted OIDC and OAuth tokens |
| **Cloud Logging** (`logging`) | CloudBurrow | Write, read, tail, sinks, exclusions, buckets, views and log-based metrics, in memory (20,000 entries); the Observability API |
| **Cloud KMS** (`kms`) | CloudBurrow | Symmetric, asymmetric and MAC keys, import jobs, Autokey, EKM connections, KMS Inventory. **Not a security boundary**: key material is stored unencrypted |
| **Kubernetes Engine** (`gke`) | CloudBurrow; each cluster is a kind cluster of its own | Clusters and node pools, `gcloud container` and Terraform ([docs/gke.md](docs/gke.md)) |
| **Compute Engine** (`compute`) | CloudBurrow | VPC networks, subnets, firewall rules, routes and addresses; **VM instances** as systemd containers with disks, snapshots, SSH and startup scripts; the Network Connectivity API ([docs/compute.md](docs/compute.md)) |
| **Cloud Monitoring** (`monitoring`) | CloudBurrow | Metric descriptors, time series, alerting, uptime checks and dashboards ([docs/monitoring.md](docs/monitoring.md)) |
| **Cloud Trace** (`trace`) | CloudBurrow | Trace v1 and v2, and OTLP trace ingestion ([docs/trace.md](docs/trace.md)) |
| **Workflows** (`workflows`) | CloudBurrow's interpreter of Google's syntax | Workflows and executions, whose calls reach this instance only ([docs/workflows.md](docs/workflows.md)) |
| **Certificate Authority Service** (`privateca`) | CloudBurrow | CA pools, CAs with locally generated keys, certificates ([docs/privateca.md](docs/privateca.md)) |
| **Parameter Manager** (`parametermanager`) | CloudBurrow | Parameters and versions, with Secret Manager references ([docs/parametermanager.md](docs/parametermanager.md)) |
| **Dataform** (`dataform`) | CloudBurrow, compiling with Google's Dataform core | v1 and v1beta1, running on this instance's BigQuery ([docs/dataform.md](docs/dataform.md)) |
| **Backup and DR** (`backupdr`) | CloudBurrow | Backup plans and vaults for Cloud SQL and Compute Engine ([docs/backupdr.md](docs/backupdr.md)) |
| **App Hub** (`apphub`) | CloudBurrow | Applications over the instance's Cloud Run services and jobs and Kubernetes Engine clusters |

Each row is a summary: the per-operation status, and what each service refuses, is in
[docs/compatibility.md](docs/compatibility.md).

### Local AI

Vertex AI runs on this machine, on runtimes CloudBurrow builds or pins; nothing is sent to Google.

- **Text generation**: a subset of `generateContent` and `streamGenerateContent`, verified through
  the official `genai` SDK, on Gemma 4 E2B with `--local-ai-model`. The model is a **community**
  conversion, labelled as one everywhere it appears; every Google-published artifact is gated.
  See [docs/generation.md](docs/generation.md).
- **Embeddings** with `--local-ai-embeddings`, on a community ONNX conversion of EmbeddingGemma
  ([docs/embeddings.md](docs/embeddings.md)).
- **Model Garden**: `ModelGardenService.Deploy` of a public Hugging Face model, gRPC, REST or the
  console's **Deploy**, served by the engine Google's Model Garden picks for its task (vLLM, Text
  Embeddings Inference, llama.cpp for GGUF, MLX, the PyTorch Inference container, ONNX Runtime),
  answering `:predict`, `:rawPredict`, `:streamRawPredict` and the OpenAI-compatible
  `chat/completions` ([docs/prediction.md](docs/prediction.md#model-garden-hugging-face-models-1840)).
- **Custom prediction**: Google's serving contract (`AIP_*` routes, `instances` → `predictions`)
  for your own container, with the Model Registry and Endpoints
  ([docs/prediction.md](docs/prediction.md)).

### Apple GPU (Metal)

On a Mac with Apple silicon, local AI runs on the GPU. A Linux container cannot reach Metal, so
CloudBurrow runs the model server as a supervised process on the Mac itself, bound to 127.0.0.1,
and the cluster reaches it as it would a pod.

| Workload | On the Apple GPU | Measured on an M4 Max |
|---|---|---|
| Model Garden, GGUF models | llama.cpp's `llama-server` (official macOS arm64 build, pinned by SHA-256) on Metal | Qwen2.5-0.5B-Instruct Q4_K_M: **243 tokens/s** on the GPU against **8.5 tokens/s** on CPU in the cluster (28.6×) |
| Model Garden, MLX models | mlx-lm on MLX's Metal backend (hash-locked) | Qwen2.5-0.5B-Instruct 4-bit: 234–245 tokens/s |
| Gemini generation (`--local-ai-model`) | A macOS build of CloudBurrow's LiteRT-LM runner on its GPU backend (WebGPU on Metal), in the darwin/arm64 archive's `litert-lm/` and installed by Homebrew (the install script copies the CLI alone; set `CLOUDBURROW_LITERT_LM_DIR` to the unpacked directory) | Gemma 4 E2B: **150 tokens/s** on the GPU against **32 tokens/s** on CPU (4.7×) |

- **Choosing it:** `cloudburrow up --local-ai-accelerator metal|cpu` sets the default; it is
  `metal` on macOS on Apple silicon and `cpu` everywhere else, and `metal` is refused on any other
  machine. A deploy's own `machine_spec` decides for itself, as on Google: a GPU type such as
  `NVIDIA_L4` (the one Model Garden deploys small open models on) runs on the Apple GPU, and a
  machine spec with no accelerator on CPU. The console's deploy form offers **Accelerator type**
  "Apple GPU (Metal), local", chosen by default when the Mac has one, and the Endpoints page shows
  it.
- **What stays on CPU:** embeddings (ONNX Runtime's Core ML provider was measured slower than its
  CPU provider on the shipped int8 model), the Model Garden runtimes with no Metal build (vLLM,
  Text Embeddings Inference, PyTorch, ONNX) and custom containers. If the GPU delegate fails,
  generation falls back to CPU and `up` logs why.
- NVIDIA GPUs are untested, and nothing configures them.

Details and evidence: [docs/prediction.md](docs/prediction.md#apple-gpu-metal) and
[docs/generation.md](docs/generation.md#on-the-apple-gpu-macos-on-apple-silicon-2159).

## The console

`cloudburrow up` serves a local web console, by default at <http://127.0.0.1:9090>, laid out
after the Google Cloud console and labelled **LOCAL** on every screen.

It is a **view**, not a second system: everything it shows is read through the same APIs an
SDK client uses. It covers the services above: list and detail pages down to individual
documents, rows, revisions and secret versions; create, edit and lifecycle actions where the
backend supports them; SQL editors for Cloud SQL and Spanner, read-only unless switched
to a confirmed Read-write mode; query builders for Firestore, Datastore and Bigtable; Model Garden
deploys and a generation playground; IAM & Admin; live Kubernetes workloads, pods, nodes and
storage; metric history; a Logs Explorer; the Cloud Shell-style terminal; and search across all of
it.

A control appears only where the backend can perform the operation. What the console can and
cannot show, and why, is in [docs/console-parity.md](docs/console-parity.md).

## What will not work

Worth reading before you rely on it. The full list is in [docs/status.md](docs/status.md), and
every remaining gap in [docs/gaps.md](docs/gaps.md):

- **IAM policies are stored, not enforced, by default.** Do not use CloudBurrow to test whether
  your permissions are correct. Per [ADR-0006](docs/adr/0006-iam-policy-surface.md), services
  store policies so code and Terraform that manage them run, and by default nothing reads them,
  apart from named checks on tokens this instance minted (Memorystore IAM auth, Firestore with
  MongoDB compatibility, IAM Credentials, BigQuery). `cloudburrow up --iam-enforcement=on` checks
  the caller's permission before each mapped method, against the stored policies
  ([compatibility.md](docs/compatibility.md#iam-enforcement)). Either way, the checks prove this
  instance applies its own rule, not that your policy is right on Google.
- **Knative is not Cloud Run.** Mapped configuration is mapped and tested; anything the adapter
  cannot map is **refused with the field named**, not silently dropped.
- **Cloud KMS is not a security boundary.** Key material is stored unencrypted; HSM and EKM keys
  are served with no real HSM or external key manager behind them.
- **Pub/Sub state does not survive a restart**: a limitation of Google's emulator, measured.
- **Signed URLs are verified, but only against local keys.** A bad signature, an unknown key or
  an expired URL is 403; a URL signed here is accepted by no Google service.
- **Nothing calls Google.** Remote Artifact Registry repositories over public upstreams, and
  anything else that would reach a third-party system, are refused by design.
- **BigQuery is a community emulator**, patched, not Google's engine, and keeps nothing across a
  restart ([compatibility.md](docs/compatibility.md#bigquery-is-a-community-emulator)).
- **Compute Engine VMs are containers**, with no kernel of their own: what needs one is refused or
  documented, never faked ([docs/compute.md](docs/compute.md#vm-instances)).

## How it fits together

```
 your machine                              │  local Kubernetes cluster (kind)
 ──────────────────────────────────────────┼──────────────────────────────────────────
  official Google SDKs, gcloud, Terraform ─┼──▶ Cloud Storage   (CloudBurrow)
                                           │    Pub/Sub         (Google's emulator)
  cloudburrow CLI                          │    Cloud Run       (Knative Serving)
   ├─ cluster lifecycle                    │    Firestore, Datastore, Bigtable,
   ├─ Cloud Tasks, Secret Manager, KMS,    │      Spanner       (Google's emulators, opt-in)
   │  Scheduler, Logging, IAM, ... (ours)  │    BigQuery        (community emulator, opt-in)
   ├─ Cloud Run adapters           (ours)  │    Cloud SQL, Memorystore (real servers, opt-in)
   ├─ metadata server + ADC fixture        │    GKE clusters, VMs (opt-in)
   ├─ Apple GPU model servers (macOS)      │    console terminal, local AI runtimes
   └─ web console                          │
  kubectl / helm / operators ──────────────┼──▶ Kubernetes API  (direct)
```

**Reuse over rewrite.** Google's own emulators and a maintained Cloud Storage implementation
are integrated rather than reimplemented; CloudBurrow builds only where no upstream exists, with
the evidence recorded in [docs/upstream-evaluation.md](docs/upstream-evaluation.md).

## Documentation

- [cloudburrow.com](https://cloudburrow.com) — the project website
- [docs/status.md](docs/status.md) — what works and what does not, on one page
- [docs/gaps.md](docs/gaps.md) — every remaining gap, by service, with why it is open
- [docs/coverage](docs/coverage/README.md) — every RPC of every served API, with its status
- [docs/compatibility.md](docs/compatibility.md) — per-operation status for every service, with the test behind each claim
- [docs/architecture.md](docs/architecture.md) — architecture and the compatibility contract
- [docs/install.md](docs/install.md) — installation and first run
- [docs/configuration.md](docs/configuration.md) — flags, environment variables, config file and command semantics
- [docs/credentials.md](docs/credentials.md) — `cloudburrow env`, the ADC fixture and the local metadata server
- [docs/ci.md](docs/ci.md) — running CloudBurrow in CI: the GitHub Action, or plain shell
- [docs/examples/python.md](docs/examples/python.md) — Python client code for Storage, Pub/Sub, Cloud Tasks and Secret Manager, run as written by the Python suite in CI
- [docs/examples/node.md](docs/examples/node.md) — Node.js client code for Storage, Pub/Sub, Cloud Tasks, Secret Manager and Firestore, run as written by the Node.js suite
- [docs/networking.md](docs/networking.md) — the ingress gateway, service URLs and what resolves them
- [docs/cloudsql.md](docs/cloudsql.md) — what the Cloud SQL service is, and is not
- [docs/memorystore.md](docs/memorystore.md), [docs/gke.md](docs/gke.md), [docs/compute.md](docs/compute.md), [docs/monitoring.md](docs/monitoring.md), [docs/trace.md](docs/trace.md), [docs/workflows.md](docs/workflows.md), [docs/dataform.md](docs/dataform.md), [docs/backupdr.md](docs/backupdr.md), [docs/privateca.md](docs/privateca.md), [docs/parametermanager.md](docs/parametermanager.md) — one page per service where it needs more than compatibility.md
- [docs/console-parity.md](docs/console-parity.md) — what the console must match, and what could not be evidenced
- [docs/console-verification.md](docs/console-verification.md) — the parity checklist walked on a live stack, and what it found
- [docs/local-ai.md](docs/local-ai.md) — the local AI runtimes and model provenance
- [docs/generation.md](docs/generation.md) — the exact `generateContent` surface
- [docs/embeddings.md](docs/embeddings.md) — the embedding routes: which is served, and why the official one is blocked
- [docs/prediction.md](docs/prediction.md) — custom prediction, the Model Registry, Model Garden's Hugging Face deploys and the Apple GPU
- [docs/playground.md](docs/playground.md) — the console's generation playground
- [docs/functions-and-builds.md](docs/functions-and-builds.md) — Functions Framework and source builds
- [docs/upstream-evaluation.md](docs/upstream-evaluation.md) — which upstream components are reused, and the measurements behind each choice
- [docs/api-contracts.md](docs/api-contracts.md) — which API definitions are built against, and how they are pinned
- [docs/local-verification.md](docs/local-verification.md) — the dated macOS/arm64 run of CI's compat shards (`make verify-local`), and the 2026-09-20 stand-up that first verified the Kubernetes architecture
- [docs/adr/](docs/adr/) — architecture decision records
- [dependencies.json](dependencies.json) — pinned component inventory
- [CHANGELOG.md](CHANGELOG.md) — what changed in each release; each release's notes are its section
- [SECURITY.md](SECURITY.md) — supported versions and how to report a vulnerability privately
- [AGENTS.md](AGENTS.md) — instructions for contributors and AI coding agents

## Building from source

Requires Go (minimum version pinned in [go.mod](go.mod)), plus Docker, kind and kubectl to run.

```sh
git clone https://github.com/cloudburrow/cloudburrow.git
cd cloudburrow
make build
./bin/cloudburrow doctor
./bin/cloudburrow up
```

`go install` works too, with the caveat below:

```sh
go install github.com/cloudburrow/cloudburrow/cmd/cloudburrow@latest
```

A CLI built this way has **no embedded Cloud Storage server**: the Linux storage binaries it
embeds are build outputs, made by `make build` and by each release, and never committed (#585).
With Cloud Storage enabled, as it is by default, `cloudburrow doctor` fails its
`embedded storage` row, and `up` refuses before it creates a cluster, naming the fix (#686):
use a [release](docs/install.md) or `make build` (or `make storage-binaries` before `go build`),
which include it, or pass `--services` without `storage`. The same goes for BigQuery, whose
emulator CloudBurrow builds from source with patches and embeds (#1061): `make bigquery-binaries`,
which `make build` runs.

From a clone:

| Command | Purpose |
|---|---|
| `make build` | Build the binary into `bin/` |
| `make fmt` / `make fmt-check` | Format, or fail if unformatted |
| `make vet` | `go vet` |
| `make cross` | Build and vet every package for darwin and linux on amd64 and arm64 |
| `make test` | Unit tests |
| `make test-race` | Unit tests with the race detector; `RACE_PKGS=./internal/x/` for one package |
| `make test-compat` | Official-SDK compatibility tests (build tag `compat`), against a running instance: `eval "$(scripts/compat-env.sh <its flags>)"` first; see [test/compat/README.md](test/compat/README.md) |
| `make test-e2e` | The acceptance workflow end to end (build tag `e2e`) |
| `make test-integration` | Tests that create a real cluster (build tag `integration`; needs Docker, kind, kubectl) |
| `make test-upstream` | Upstream-component probes for the reuse audit (build tag `upstream`) |
| `make check` | Formatting, vet and race tests — what CI runs first |

`make help` lists every target.

## Continuous integration

Every pull request runs the fast jobs: formatting, `go vet`, race tests and a build on Go 1.27
(Linux), `make cross` for all four release targets, plus `go mod tidy`/`verify` and a
`govulncheck` scan. Every push to `main` also runs them on Go 1.26 and on macOS. `make check`
does not run the tidy check, so run `go mod tidy` before pushing a change that touches imports.

The cluster and official-SDK suites are slower and need Docker, so they run on every push to
`main` and on demand; add the `run-integration` label to run them on a pull request. A release
is cut only from a `main` commit whose full run passed. Failures upload bounded diagnostics.

No CI job receives publishing credentials, and none can reach Google Cloud.

## Contributing

Work is tracked by issue on the [project board](https://github.com/orgs/cloudburrow/projects/1),
one issue per pull request. Read [AGENTS.md](AGENTS.md) before starting — it covers scope,
testing expectations, and the project's rule against claiming unverified compatibility.
[CONTRIBUTING.md](CONTRIBUTING.md) says how contributions are licensed (MIT, inbound = outbound)
and how to sign off your commits (Developer Certificate of Origin).

## Security

To report a vulnerability, use GitHub's private vulnerability reporting, not a public issue. See
[SECURITY.md](SECURITY.md) for supported versions, what is in scope and how quickly to expect a
response.

## License

CloudBurrow is licensed under the [MIT License](LICENSE). Releases v0.1.0 to v0.5.0 were first
published under the Apache License 2.0, and their CloudBurrow code is now available under MIT too. Third-party code keeps its own licence: see
[NOTICE](NOTICE) for attribution and trademark notes, and [dependencies.json](dependencies.json).

## Domains

- cloudburrow.com
- cloudburrow.dev

Independent community project. Not affiliated with or endorsed by Google.
