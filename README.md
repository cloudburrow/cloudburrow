# CloudBurrow

[![CI](https://github.com/cloudburrow/cloudburrow/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/cloudburrow/cloudburrow/actions/workflows/ci.yml?query=branch%3Amain)

A local Google Cloud emulator for development and testing.

CloudBurrow runs a local Kubernetes cluster that speaks Google Cloud APIs, so you can build
and test applications on your own machine with the official Google Cloud SDKs, without
deploying to GCP for every change. Because it is a real Kubernetes cluster, `kubectl`, Helm
charts and operators work against it directly.

## Status

**Pre-release, and working.** No binaries are published yet — build from source. The release
pipeline is in place: attested archives, one `checksums.txt`, a Homebrew formula, and an
installer that refuses an archive it cannot verify ([docs/install.md](docs/install.md)). The
acceptance workflow passes end to end: upload an object, publish an event, a Cloud Run worker
receives it and reads the object, writes a result, and the result is read back. Every step goes
through an official Google SDK, including inside the worker.

What is supported is recorded **per operation** in
[docs/compatibility.md](docs/compatibility.md), and every row marked `Verified` names the test
that proves it. An operation is only called supported once a merged test drives it through an
official Google Cloud client library; implemented-but-undriven operations are listed as such.
[docs/status.md](docs/status.md) is the one-page version, and
[docs/coverage](docs/coverage/README.md) lists every RPC of every service with its status,
generated from the proto surface.

## What runs

Started by default:

| Service | Backed by | Scope |
|---|---|---|
| **Cloud Storage** | CloudBurrow | Buckets, objects, versioning, soft delete, retention and holds, lifecycle, CORS, IAM, HMAC keys, the XML API with multipart uploads, signed URLs, and **notifications to Pub/Sub** |
| **Pub/Sub** | Google's own emulator | Topics, subscriptions, publish, pull, StreamingPull, push delivery |
| **Cloud Tasks** | CloudBurrow | Queues, tasks, pause/resume, HTTP dispatch with retry |
| **Cloud Run** | Knative Serving, behind a Cloud Run v2 adapter | Deploys real containers; env from Secret Manager; min-instances verified; max-instances, concurrency, timeout and resource limits mapped but not load-tested |
| **Secret Manager** | CloudBurrow | Secrets and versions over gRPC and JSON. **Not a secret store** — nothing is authenticated |

Opt-in with `--services`:

| Service | Backed by | Scope |
|---|---|---|
| **Firestore** / **Datastore** | Google's own emulators | Documents, entities, collections and kinds |
| **Bigtable** | Google's own emulator | Tables, column families, rows |
| **Spanner** | Google's own emulator | Instances, databases, DDL, queries |
| **Cloud KMS** | CloudBurrow | Symmetric keys, Encrypt and Decrypt. **Not a security boundary** — key material is stored unencrypted |
| **Cloud SQL** | PostgreSQL in the cluster | A real PostgreSQL reached with an ordinary driver — **not** the Cloud SQL Admin API, which has no emulator |

And locally, on runtimes CloudBurrow builds:

- **Vertex AI text generation** — a subset of `generateContent` and `streamGenerateContent`,
  verified through the official `genai` SDK. The one model that runs today is a **community**
  conversion, labelled as one everywhere it appears; every Google-published artifact is gated.
  See [docs/generation.md](docs/generation.md).
- **Vertex AI custom prediction** — Google's serving contract (`AIP_*` routes,
  `instances` → `predictions`) on CloudBurrow's own runtime. See
  [docs/prediction.md](docs/prediction.md).

## The console

`cloudburrow up` serves a local web console, by default at <http://127.0.0.1:9090>, laid out
after the Google Cloud console and labelled **LOCAL** on every screen.

It is a **view**, not a second system: everything it shows is read through the same APIs an
SDK client uses. It covers every service above — list and detail pages down to individual
documents, rows, revisions and secret versions; create, edit and lifecycle actions where the
backend supports them; read-only SQL editors for Cloud SQL and Spanner; query builders for
Firestore, Datastore and Bigtable; live Kubernetes workloads, pods, nodes and storage; metric
history per node and per pod; a Logs Explorer; and search across all of it.

A control appears only where the backend can perform the operation. What the console can and
cannot show, and why, is in [docs/console-parity.md](docs/console-parity.md).

## Quick start

Needs **Docker**, **kind** and **kubectl**, plus **Go** to build. Give Docker at least 4 CPU and
6 GB — the full stack measures about 1.5 GiB and 2.1 CPU.

```sh
git clone https://github.com/cloudburrow/cloudburrow.git
cd cloudburrow
make build

./bin/cloudburrow doctor      # checks prerequisites, changes nothing
./bin/cloudburrow up          # creates the cluster and runs in the foreground
```

In another shell, point Google tooling at it:

```sh
eval "$(./bin/cloudburrow env)"
```

That exports endpoint overrides, the project, a local metadata server and a generated ADC file.
The credentials authorise nothing; they exist so tooling that insists on them runs offline. See
[docs/install.md](docs/install.md) and [docs/credentials.md](docs/credentials.md).

`stop`, `reset` and `delete` are distinct commands, and none implies another.

## What will not work

Worth reading before you rely on it — the full list is in [docs/status.md](docs/status.md):

- **No IAM enforcement, anywhere.** No policy is evaluated and no identity is checked. Do not
  use CloudBurrow to test whether your permissions are correct. Per
  [ADR-0006](docs/adr/0006-iam-policy-surface.md), Secret Manager and Cloud Tasks store IAM
  policies, so code and Terraform that manage them run, and so does bucket IAM on the builtin
  Cloud Storage server (#504), not yet the default. Stored, never enforced.
- **Knative is not Cloud Run.** Mapped configuration is mapped and tested; anything the adapter
  cannot map is **refused with the field named**, not silently dropped.
- **Cloud KMS is not a security boundary, and has no IAM.** Key material is stored
  unencrypted; its IAM methods return `Unimplemented` ([ADR-0006](docs/adr/0006-iam-policy-surface.md)
  does not extend policy storage to KMS); no HSM or EKM.
- **Pub/Sub state does not survive a restart** — a limitation of Google's emulator, measured.
- **Signed URLs are accepted on shape alone**, so signing correctness cannot be tested here.
- **No Cloud Run Jobs or GKE management APIs.** BigQuery is an opt-in community emulator that
  serves one project and keeps nothing across a restart; see
  [compatibility.md](docs/compatibility.md#bigquery-is-a-community-emulator).

## How it fits together

```
 your machine                          │  local Kubernetes cluster (kind)
 ──────────────────────────────────────┼─────────────────────────────────────────
  official Google SDKs ────────────────┼──▶ Cloud Storage   (CloudBurrow)
                                       │    Pub/Sub         (Google's emulator)
  cloudburrow CLI                      │    Cloud Run       (Knative Serving)
   ├─ cluster lifecycle                │    Firestore, Datastore, Bigtable,
   ├─ Cloud Tasks          (ours)      │      Spanner       (Google's emulators, opt-in)
   ├─ Secret Manager       (ours)      │    Cloud SQL       (PostgreSQL, opt-in)
   ├─ Cloud Run v2 adapter (ours)      │    local AI runtimes
   ├─ metadata server + ADC fixture    │
   └─ web console                      │
  kubectl / helm / operators ──────────┼──▶ Kubernetes API  (direct)
```

**Reuse over rewrite.** Google's own emulators and a maintained Cloud Storage implementation
are integrated rather than reimplemented; CloudBurrow builds only where no upstream exists, with
the evidence recorded in [docs/upstream-evaluation.md](docs/upstream-evaluation.md).

## Documentation

- [docs/status.md](docs/status.md) — what works and what does not, on one page
- [docs/compatibility.md](docs/compatibility.md) — per-operation status for every service, with the test behind each claim
- [docs/architecture.md](docs/architecture.md) — architecture and the compatibility contract
- [docs/install.md](docs/install.md) — installation and first run
- [docs/configuration.md](docs/configuration.md) — flags, environment variables, config file and command semantics
- [docs/credentials.md](docs/credentials.md) — `cloudburrow env`, the ADC fixture and the local metadata server
- [docs/networking.md](docs/networking.md) — the ingress gateway, service URLs and what resolves them
- [docs/cloudsql.md](docs/cloudsql.md) — what the Cloud SQL service is, and is not
- [docs/console-parity.md](docs/console-parity.md) — what the console must match, and what could not be evidenced
- [docs/console-verification.md](docs/console-verification.md) — the parity checklist walked on a live stack, and what it found
- [docs/local-ai.md](docs/local-ai.md) — the local AI runtimes and model provenance
- [docs/generation.md](docs/generation.md) — the exact `generateContent` surface
- [docs/embeddings.md](docs/embeddings.md) — why embeddings are blocked, and on what
- [docs/prediction.md](docs/prediction.md) — Vertex custom prediction on the owned runtime
- [docs/playground.md](docs/playground.md) — the console's generation playground
- [docs/functions-and-builds.md](docs/functions-and-builds.md) — Functions Framework and source builds
- [docs/upstream-evaluation.md](docs/upstream-evaluation.md) — which upstream components are reused, and the measurements behind each choice
- [docs/api-contracts.md](docs/api-contracts.md) — which API definitions are built against, and how they are pinned
- [docs/local-verification.md](docs/local-verification.md) — the stand-up that verified the Kubernetes architecture end to end
- [docs/adr/](docs/adr/) — architecture decision records
- [dependencies.json](dependencies.json) — pinned component inventory
- [AGENTS.md](AGENTS.md) — instructions for contributors and AI coding agents

## Building from source

Requires Go (minimum version pinned in [go.mod](go.mod)).

```sh
go install github.com/cloudburrow/cloudburrow/cmd/cloudburrow@latest
```

or from a clone:

| Command | Purpose |
|---|---|
| `make build` | Build the binary into `bin/` |
| `make fmt` / `make fmt-check` | Format, or fail if unformatted |
| `make vet` | `go vet` |
| `make test` | Unit tests |
| `make test-race` | Unit tests with the race detector |
| `make test-compat` | Official-SDK compatibility tests (build tag `compat`) |
| `make test-e2e` | The acceptance workflow end to end (build tag `e2e`) |
| `make test-integration` | Tests that create a real cluster (build tag `integration`; needs Docker, kind, kubectl) |
| `make test-upstream` | Upstream-component probes for the reuse audit (build tag `upstream`) |
| `make check` | Formatting, vet and race tests — what CI runs first |

`make help` lists every target.

## Continuous integration

Every pull request runs formatting, `go vet`, race tests and a build on **Go 1.26 and 1.27**
(Linux) and Go 1.27 (macOS), plus `go mod tidy`/`verify` and a `govulncheck` scan. `make check`
does not run the tidy check, so run `go mod tidy` before pushing a change that touches imports.

Cluster and official-SDK suites are slower and need Docker, so they run on `main` and on
demand; add the `run-integration` label to run them on a pull request. Failures upload bounded
diagnostics.

No CI job receives publishing credentials, and none can reach Google Cloud.

## Contributing

Work is tracked by issue on the [project board](https://github.com/orgs/cloudburrow/projects/1),
one issue per pull request. Read [AGENTS.md](AGENTS.md) before starting — it covers scope,
testing expectations, and the project's rule against claiming unverified compatibility.

## License

[Apache License 2.0](LICENSE). See [NOTICE](NOTICE) for attribution and trademark notes.

## Domains

- cloudburrow.com
- cloudburrow.dev

Independent community project. Not affiliated with or endorsed by Google.
