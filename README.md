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
- [docs/local-verification.md](docs/local-verification.md) — the stand-up that verified the Kubernetes architecture end to end
- [dependencies.json](dependencies.json) — pinned component inventory
- [AGENTS.md](AGENTS.md) — instructions for contributors and AI coding agents

## Building from source

Requires Go (minimum version is pinned in [go.mod](go.mod)). Docker and a local Kubernetes
cluster are needed for the cluster-backed features, which are not implemented yet.

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
| `make test-integration` | Tests requiring Docker (build tag `integration`) |
| `make test-compat` | Official-SDK compatibility tests (build tag `compat`) |
| `make test-upstream` | Upstream-component probes for the reuse audit (build tag `upstream`) |
| `make check` | What CI runs: `fmt-check` + `vet` + `test-race` |

`make help` lists every target.

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
