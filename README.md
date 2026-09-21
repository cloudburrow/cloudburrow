# CloudBurrow

A local Google Cloud emulator for development and testing.

CloudBurrow runs a local Kubernetes cluster that speaks Google Cloud APIs, so you can build
and test applications on your own machine with familiar Google Cloud SDKs without deploying
to GCP for every change. Because it is a real Kubernetes cluster, `kubectl`, Helm charts and
operators work against it directly.

## Status

Early planning. **No emulator service is implemented yet** and no installable release is
available. The repository currently contains the architecture contract, the package skeleton,
and a CLI that reports its version and help.

Every operation of every service is marked `Planned` in
[docs/compatibility.md](docs/compatibility.md). An operation is only described as supported
once a merged test drives it through an official Google Cloud client library.

## How it fits together

```
 your machine                        │  local Kubernetes cluster (kind)
 ────────────────────────────────────┼──────────────────────────────────────
  official Google SDKs ──────────────┼──▶ Cloud Storage    (fake-gcs-server)
  cloudburrow CLI                    │    Pub/Sub          (Google's emulator)
   ├─ cluster lifecycle              │    Cloud Run        (Knative Serving)
   ├─ Cloud Tasks  (ours)            │
   └─ Cloud Run v2 adapter (ours)    │
  kubectl / helm / operators ────────┼──▶ Kubernetes API (direct)
```

## Planned direction

- A local Kubernetes cluster (kind) as the foundation, with native `kubectl`, Helm and
  operator support.
- Initial focus on Cloud Storage, Pub/Sub, Cloud Tasks and Cloud Run.
- Cloud Run v2 requests served through an adapter onto Knative Serving.
- **Reuse over rewrite**: Google's own Pub/Sub emulator and a maintained Cloud Storage
  implementation are integrated rather than reimplemented. See
  [the upstream evaluation](docs/upstream-evaluation.md) for the measurements behind each
  choice.
- Local resource setup, event inspection, and repeatable resets for tests.
- Compatibility tests using official Google Cloud client libraries.

The first target workflow is uploading a file, publishing an event, running a worker, and saving the result locally. These are planned capabilities, not currently supported features.

## Documentation

- [docs/architecture.md](docs/architecture.md) — architecture and the compatibility contract
- [docs/compatibility.md](docs/compatibility.md) — per-operation status for every service
- [docs/upstream-evaluation.md](docs/upstream-evaluation.md) — which upstream components we reuse, and the measurements behind those decisions
- [docs/adr/](docs/adr/) — architecture decision records
- [docs/functions-and-builds.md](docs/functions-and-builds.md) — Functions Framework and source builds
- [docs/local-ai.md](docs/local-ai.md) — local AI audit: why inference is not viable yet
- [docs/prediction.md](docs/prediction.md) — Vertex custom prediction containers on the owned runtime
- [docs/status.md](docs/status.md) — what works and what does not
- [docs/install.md](docs/install.md) — installation and first run
- [docs/api-contracts.md](docs/api-contracts.md) — which API definitions we build against, and how they are pinned
- [docs/configuration.md](docs/configuration.md) — flags, environment variables, config file and command semantics
- [docs/local-verification.md](docs/local-verification.md) — the stand-up that verified the Kubernetes architecture end to end
- [dependencies.json](dependencies.json) — pinned component inventory
- [AGENTS.md](AGENTS.md) — instructions for contributors and AI coding agents

## Building from source

Requires Go (minimum version is pinned in [go.mod](go.mod)). Creating a local environment
additionally needs **Docker**, **kind** and **kubectl** — see
[docs/configuration.md](docs/configuration.md#prerequisites).

```sh
git clone https://github.com/identity-wael/cloudburrow.git
cd cloudburrow
make build
./bin/cloudburrow version
```

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
| `make check` | What CI runs: `fmt-check` + `vet` + `test-race` |

`make help` lists every target.

## Continuous integration

Every pull request runs formatting, `go vet`, race tests and a build on **Go 1.26 and 1.27**
(Linux) and Go 1.27 (macOS), plus `go mod tidy`/`verify` and a `govulncheck` scan.

Cluster and official-SDK suites are slower and need Docker, so they run on `main` and on
demand; add the `run-integration` label to run them on a pull request. Failures upload
bounded diagnostics.

No CI job receives publishing credentials, and none can reach Google Cloud.

## Contributing

Work is tracked by issue, one issue per pull request. Read [AGENTS.md](AGENTS.md) before
starting — it covers scope, testing expectations, and the project's rule against claiming
unverified compatibility.

## License

[Apache License 2.0](LICENSE). See [NOTICE](NOTICE) for attribution and trademark notes.

## Domains

- cloudburrow.com
- cloudburrow.dev

Independent community project. Not affiliated with or endorsed by Google.
