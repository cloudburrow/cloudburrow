# Functions and source builds

Two optional capabilities, both reusing Google-published components. Neither implies any
Cloud Functions or Cloud Build **API** parity: nothing here serves a Google API, and no build
or function is addressable as a Cloud resource.

## Functions Framework (#32)

An ordinary [Functions Framework](https://github.com/GoogleCloudPlatform/functions-framework-go)
function runs unchanged. The fixture in `testdata/function` contains no CloudBurrow-specific
code — that is the point.

**Tested language subset: Go only.** Other runtimes are published by Google and would
plausibly work, but untested is untested, so nothing else is claimed.

| Signature | Status | Evidence |
|---|---|---|
| `http` | Partial | `TestFunctionsFrameworkBuiltWithBuildpacks`: `POST /` returns the handler's JSON; a body-less request still answers. Gated, **not yet run** |
| `cloudevent` | Partial | `TestFunctionsFrameworkBuiltWithBuildpacks`: a Google-schema CloudEvent in binary mode reaches the handler, which logs its type, subject and data. Gated, **not yet run** |
| Errors | Partial | `TestFunctionsFrameworkBuiltWithBuildpacks`: a request without `ce-*` headers to a CloudEvent function returns **HTTP 400**. Gated, **not yet run** |

The test is in `test/compat` and runs only with `CLOUDBURROW_TEST_FUNCTIONS=1`, against an
instance whose cluster nodes are amd64, with `pack` and Docker installed. It is too slow for
every merge, so no CI shard runs it; see
[compatibility.md](compatibility.md#what-ci-does-not-run-and-why). Until a run is recorded,
these rows are Partial: earlier results came from runs by hand, which are not evidence.

Built images deploy through the established Knative path, like any other image.

**Not supported, and not implied by a working handler:**

- **The Cloud Functions management API.** There is no `projects.locations.functions` surface.
  Deploy the built image as a Cloud Run service instead.
- **Eventarc trigger management.** Nothing creates or routes triggers. Delivery is whatever
  you point at the function — a Pub/Sub push subscription, for instance.

## Google Buildpacks (#33)

`pack` plus Google's builder turns source into a runnable image with no Dockerfile.

**The builder is pinned by digest** and recorded in `dependencies.json`.

### The architecture constraint

**Google's builder is published for `linux/amd64` only.** On an arm64 host it runs under
emulation and produces an **amd64** image, which then needs an amd64-capable node to run.
CloudBurrow reports this rather than failing obscurely later.

That is not a refusal: a build on arm64 completed when tried by hand, though no automated
test covers the emulated path. It is slower, and the output is amd64.

### A registry is required

`pack`'s export to the Docker daemon fails on an arm64 host with this builder:

```
failed to fetch base layers: ... reference gcr.io/buildpacks/google-22/run:latest
was found but does not provide the specified platform (linux/amd64)
```

The build therefore always `--publish`es. **A local registry is not a cloud registry** — this
still satisfies "no cloud registry" and keeps everything on the machine:

```sh
docker network create cb-build
docker run -d --name cb-registry --network cb-build -p 127.0.0.1:5001:5000 registry:2

pack build cb-registry:5000/my-function:test --publish \
  --platform linux/amd64 --network cb-build --insecure-registry cb-registry:5000 \
  --path ./testdata/function \
  --builder gcr.io/buildpacks/builder:google-22 \
  --env GOOGLE_FUNCTION_TARGET=Hello \
  --env GOOGLE_FUNCTION_SIGNATURE_TYPE=http
```

The registry must share a Docker network with the build, because the buildpack lifecycle runs
**inside a container** — `127.0.0.1` there is the lifecycle container, not your machine.

### Behaviour and evidence

| Behaviour | Status |
|---|---|
| Build Go source with no Dockerfile | Partial — `TestFunctionsFrameworkBuiltWithBuildpacks` builds `testdata/function` through `internal/buildpacks`; gated, **not yet run** |
| Resulting image runs and answers | Partial — the same test deploys it through the Cloud Run v2 client and expects `{"greeting":"hello CloudBurrow"}`; gated, **not yet run** |
| Cache reuse on rebuild | **Verified** — `TestFunctionsRebuildReusesLayers` rebuilds unchanged source and asserts pack's log reports reused layers (`Reusing layer '…'`), and that the first build reported none; Verified by a dated run (2026-09-27, macOS/arm64 Docker Desktop, emulated amd64 builder, pack 0.40.9), the rebuild reusing 6 layers; gated, not run in CI |
| Failed build diagnostics | **Verified** — `TestFunctionsBuildBrokenModulePathFails` builds a `go.mod` with the module path `brokenmodule/function` and asserts the returned `build failed` error carries the buildpack's message naming that path; Verified by a dated run (2026-09-27, macOS/arm64 Docker Desktop, emulated amd64 builder, pack 0.40.9); gated, not run in CI |
| Initial download vs cached operation | First build pulls the builder and run images; rebuilds reuse layers (asserted by `TestFunctionsRebuildReusesLayers`; on the 2026-09-27 run the first build took 1m48s and the rebuild 1m32s, both emulated) |

The two build-only tests are in `test/compat` too and are gated by the same
`CLOUDBURROW_TEST_FUNCTIONS=1`. They need `pack` and Docker but no cluster, since they build
into a local registry and run nothing; like the test above, no CI shard runs them. Their
Verified status rests on one dated run (2026-09-27, macOS/arm64 Docker Desktop, emulated amd64 builder, pack 0.40.9), listed in
[What CI does not run](compatibility.md#what-ci-does-not-run-and-why).

### Not covered

Languages other than Go · the emulated arm64 build path, which only a run by hand has
exercised (the gated test needs amd64 nodes) · fully offline builds, since the first build
must fetch the builder.
