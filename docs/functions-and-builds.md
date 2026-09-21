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
| `http` | **Verified** | `POST /` returns the handler's JSON; a body-less request still answers |
| `cloudevent` | **Verified** | A Google-schema CloudEvent in binary mode reaches the handler with its type, subject and data intact |
| Errors | **Verified** | A request without `ce-*` headers to a CloudEvent function returns **HTTP 400** |

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

That is not a refusal — the build genuinely works on arm64, and is verified below. It is
slower, and the output is amd64.

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

### Verified behaviour

| Behaviour | Result |
|---|---|
| Build Go source with no Dockerfile | **Verified** |
| Resulting image runs and answers | **Verified** — `{"greeting":"hello CloudBurrow"}` |
| Cache reuse on rebuild | **Verified** — 15 layers reported reused |
| Failed build diagnostics | **Verified** — an invalid module path produced a precise, actionable message |
| Initial download vs cached operation | First build pulls the builder and run images; rebuilds reuse cached layers |

### Not covered

Languages other than Go · `pack` builds on a native amd64 host (the emulated path is what was
tested here) · fully offline builds, since the first build must fetch the builder.
