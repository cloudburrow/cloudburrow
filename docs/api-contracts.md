# API contracts

The contract is the **published API definition**, never another emulator's observed
behaviour. A divergence from another emulator is not automatically our bug; a divergence from
an official client always is.

## Reuse applies to code generation too

Google publishes maintained, generated Go packages for these APIs. CloudBurrow **consumes
them** rather than running `protoc` itself.

That is the same reuse-first rule as everywhere else (ADR-0005), and it removes a whole
category of work: no generation pipeline, no pinned plugin versions, no generated code in the
tree to keep separate from handwritten code, and no regeneration step that could produce a
dirty diff. Pinning is handled by `go.mod` and `go.sum`, which `go mod verify` and CI already
check.

The trade-off is real and worth stating: we cannot regenerate against an arbitrary
`googleapis` revision without waiting for Google to publish it. For an emulator tracking
released APIs, that is the right side of the trade.

## Surfaces

| Service | Role | Source | Go package | REST surface |
|---|---|---|---|---|
| `google.cloud.tasks.v2` | **Implement** | googleapis @ pinned commit | `cloud.google.com/go/cloudtasks/apiv2/cloudtaskspb` | Handwritten, from the proto's `google.api.http` annotations |
| `google.cloud.run.v2` | **Adapt** | googleapis @ pinned commit | `cloud.google.com/go/run/apiv2/runpb` | Handwritten; translated to Knative Serving resources |
| `google.pubsub.v1` | Upstream | Google Pub/Sub emulator 0.8.35 | `pubsub/v2/apiv1/pubsubpb` (tests only) | Not served by CloudBurrow |
| Cloud Storage JSON API v1 | Upstream | fake-gcs-server v1.56.1 | `cloud.google.com/go/storage` (tests only) | Not served by CloudBurrow |

**Only two services need a server surface.** Pub/Sub and Cloud Storage are served by upstream
components; generating a server for them would imply CloudBurrow answers those calls, which
it does not.

Cloud Storage is deliberately the **JSON API v1**, not `google.storage.v2`: the JSON API is
what official clients use by default, and it is discovery-based rather than proto-defined.

## Pinning

`googleapis` publishes **no release tags**, so an explicit commit is pinned rather than a
branch — a branch would be exactly the mutable reference ADR-0005 forbids. The commit is
recorded in `dependencies.json` and as `apicontract.GoogleapisCommit`, and a test fails if it
is ever set to `master`, `main`, `HEAD` or `latest`, or is not a 40-character SHA.

Generated packages are pinned by `go.mod`/`go.sum`. `go mod verify` runs in CI.

## Drift detection

`internal/apicontract` holds compile-time references to **every type the Cloud Tasks
implementation and the Cloud Run adapter are built on**. If an upgrade removes or renames one,
the build fails there — loudly, at the contract boundary — instead of somewhere subtle at
runtime.

Tests additionally confirm the types are genuine protobuf messages that round-trip and carry
the expected descriptors (`google.cloud.tasks.v2.Task`, `google.cloud.run.v2.Service`), so a
hand-rolled struct that merely looked similar could not pass.

## Cloud Storage upload and download protocols

The JSON API's transfer protocols are not covered by proto annotations and need explicit
attention:

- **Resumable upload** — session URLs and chunk offsets. The default path for large objects
  in most clients. Verified for 8 MiB in 256 KiB chunks; **interruption and resume are not
  yet covered**.
- **Ranged download** — range requests and partial content. Verified byte-exact.
- **Advertised URLs** — the client *follows* the `mediaLink` the server returns, so the
  backend must advertise an address the caller can reach. See
  [local-verification.md §5.2](local-verification.md).
