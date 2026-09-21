# Custom prediction containers

CloudBurrow runs **Vertex AI custom prediction containers** — images that honour Google's
serving contract — on its own Kubernetes runtime.

This page records the decision [#42](https://github.com/identity-wael/cloudburrow/issues/42)
required, the evidence behind it, and what is and is not supported.

## The decision

> **Google's tooling is used for the contract and for container construction and testing.
> Execution goes through CloudBurrow's owned runtime, which is Knative in the cluster.**

The Vertex SDK offers both. `LocalModel.build_cpr_model` builds an image;
`LocalModel.deploy_to_local_endpoint` runs one. The first is container construction, the
second is execution — and only the second conflicts with the architecture.

`deploy_to_local_endpoint` returns a `LocalEndpoint` that starts a container directly:

```
google/cloud/aiplatform/docker_utils/run.py:242
    container = client.containers.run(
        ...
        detach=True,
```

That container is owned by nothing. It is outside the cluster, so it is outside cluster
ownership, readiness, `cloudburrow reset` and `cloudburrow delete`. A crash leaves it
orphaned, and the only way to find it again is a Docker-wide search — which is exactly the
"another unmanaged runtime" [#42](https://github.com/identity-wael/cloudburrow/issues/42)
warns about, and exactly the global Docker cleanup the same issue forbids.

Declining it costs nothing, because a container that honours the contract runs unchanged as
a Knative service. The interface is Google's; the runtime is ours.

## The contract

A custom prediction container reads four environment variables and serves two routes.
CloudBurrow's constants are not a transcription of the documentation — they were checked
against the SDK's own definitions in `google-cloud-aiplatform[prediction]` **2.1.3**:

```
$ python -c "from google.cloud.aiplatform.constants import prediction as pc; ..."
AIP_HEALTH_ROUTE = 'AIP_HEALTH_ROUTE'
AIP_HTTP_PORT = 'AIP_HTTP_PORT'
AIP_PREDICT_ROUTE = 'AIP_PREDICT_ROUTE'
AIP_STORAGE_URI = 'AIP_STORAGE_URI'
DEFAULT_AIP_HTTP_PORT = 8080
DEFAULT_LOCAL_HEALTH_ROUTE = '/health'
DEFAULT_LOCAL_PREDICT_ROUTE = '/predict'
```

`internal/prediction` holds the same names and defaults, plus the request and response
shapes:

| Direction | Shape |
|---|---|
| Request | `{"instances": [...], "parameters": {...}}` — at least one instance |
| Response | `{"predictions": [...], "deployedModelId": "..."}` — **one prediction per instance** |

The one-per-instance rule is enforced, not assumed. A container returning a different count
has produced results that cannot be matched to inputs, and silently accepting that would let
a caller mis-attribute predictions. `prediction.ValidateResponse` refuses it.

`AIP_STORAGE_URI` is recognised but nothing fetches from it: it names a GCS prefix, and the
offline test path downloads no cloud artifacts and loads no credentials.

## Deploying a predictor

A predictor is deployed like any other container — through the Cloud Run v2 API, onto
Knative:

```go
c.CreateService(ctx, &runpb.CreateServiceRequest{
    Parent:    "projects/p/locations/us-central1",
    ServiceId: "predictor",
    Service: &runpb.Service{Template: &runpb.RevisionTemplate{
        Containers: []*runpb.Container{{
            Image: "dev.local/my-predictor:v1",
            Env: []*runpb.EnvVar{
                {Name: "AIP_HTTP_PORT", Values: &runpb.EnvVar_Value{Value: "8080"}},
                {Name: "AIP_HEALTH_ROUTE", Values: &runpb.EnvVar_Value{Value: "/healthz"}},
                {Name: "AIP_PREDICT_ROUTE", Values: &runpb.EnvVar_Value{Value: "/v1/predict"}},
            },
        }},
    }},
})
```

Everything the Cloud Run adapter supports applies, and everything it refuses is refused here
too — see [compatibility.md](compatibility.md#cloud-run--googlecloudrunv2).

### Reaching the endpoint

The URI the API advertises (`http://<service>.<namespace>.127.0.0.1.sslip.io`) is the
**cluster ingress** address. CloudBurrow does not publish the Knative gateway on a host port,
so that URI resolves from your machine but nothing is listening on port 80. A host-side
caller reaches it through a port-forward, naming the service in the `Host` header:

```sh
kubectl --kubeconfig <state>/kubeconfig -n kourier-system \
  port-forward svc/kourier-internal 8080:80 &

curl -s -X POST http://127.0.0.1:8080/v1/predict \
  -H 'Host: predictor.default.127.0.0.1.sslip.io' \
  -d '{"instances":[1,2,3.5]}'
{"predictions":[2,4,7],"deployedModelId":"doubler-v1"}
```

A workload **inside** the cluster needs none of that and uses
`http://predictor.default.svc.cluster.local`.

## Endpoint lifecycle

Vertex has no local-endpoint resource to mirror — its `LocalEndpoint` is a Python object
wrapping a Docker container, not an API resource. `internal/prediction` therefore reports
what CloudBurrow actually runs, so the console pages
([#43](https://github.com/identity-wael/cloudburrow/issues/43)–[#49](https://github.com/identity-wael/cloudburrow/issues/49))
render real state rather than a Vertex-shaped status no API returns:

| State | Meaning |
|---|---|
| `PENDING` | The revision exists but is not serving yet. Knative reports `Unknown` while coming up, and calling that a failure would make every healthy deployment look broken for its first seconds. |
| `READY` | The container passed its health route and has a replica. |
| `SCALED_TO_ZERO` | Healthy with no replica. Requests still succeed; the first pays cold start. |
| `FAILED` | Terminal. The revision will not become ready without a new deployment. |

Only `FAILED` is terminal, so a caller polling a healthy endpoint that scales to zero does
not mistake it for a dead one.

### Startup failure takes 600 seconds to report

A container that exits at startup **is** reported, with its own output:

```
startup failure reported after 10m1s:
  reason="COMMON_REASON_UNDEFINED"
  message="Revision \"compat-predictor-broken-00001\" failed with message:
           Container failed with: 2026/09/21 14:33:56 FAIL_STARTUP is set: refusing to start"
```

but only after **602 seconds**, measured. The `reason` enum is undefined because the adapter
maps no Cloud Run `CommonReason` — Knative's reasons (`RevisionFailed`, `ExitCode1`) have no
Cloud Run equivalents, and inventing a mapping would put a wrong enum where a caller looks
first. The message carries the truth instead. Knative declares a revision failed only when its
`progress-deadline` expires, which defaults to `600s`, and the Cloud Run v2 surface exposes
no field that shortens it. Until then the endpoint reports `PENDING`, which is honest —
Kubernetes is still restarting the container — but slow.

This is an inherited upstream behaviour, not a CloudBurrow choice, and it is recorded rather
than papered over. What CloudBurrow *does* improve is the message: Knative's top-level `Ready`
condition says only `Configuration "x" does not have any ready Revision`, so the adapter
prefers the `ConfigurationsReady` message, which names the revision and quotes the container's
own log line.

## Versus Vertex AI's own APIs

Nothing in the Vertex **management** plane is implemented. There is no model registry, no
`Endpoint` resource, no `DeployedModel`, no online prediction service. Those calls do not
reach CloudBurrow at all — they are not served, so a client pointed at CloudBurrow fails to
connect rather than receiving a plausible stub.

| Vertex surface | Status |
|---|---|
| Serving contract (`AIP_*`, `/predict`, `/health`) | **Verified** |
| `aiplatform.Model` registry (upload, list, version) | **Not supported** |
| `aiplatform.Endpoint` / `DeployedModel` | **Not supported** |
| `PredictionService.Predict` / `RawPredict` API | **Not supported** — the container is called directly |
| `LocalModel.build_cpr_model` | Available to you; CloudBurrow does not invoke it |
| `LocalModel.deploy_to_local_endpoint` | **Declined by design** — see [The decision](#the-decision) |
| Model Garden, tuning, batch prediction | **Not supported** |

## Accelerators

**CPU only. No GPU path exists, on any platform.**

| Host | What happens |
|---|---|
| CPU (amd64, arm64) | Works. This is the only tested path. |
| NVIDIA | Untested. kind can be configured with the NVIDIA container runtime, and Kubernetes can schedule `nvidia.com/gpu` — CloudBurrow does neither, and claims nothing it has not run. |
| Apple Silicon | **No GPU access, and none is achievable this way.** Metal is not reachable from a Linux container: Docker Desktop runs the cluster inside a Linux VM, and that VM has no Metal device. |

Passing NVIDIA container flags on a Mac would neither fail loudly nor enable anything — it
would simply be ignored. **NVIDIA container flags do not establish Metal acceleration**, and
the two must not be conflated: a GPU-capable image is not a GPU-capable runtime.

## Cleanup

The fixture image is built in the developer's Docker context and loaded into **CloudBurrow's
own kind cluster** with `kind load docker-image`. Endpoints are deleted through
`DeleteService` in per-test cleanup, which runs even when the deployment itself failed.

- No container is started outside the cluster, so there is nothing to orphan.
- **No global Docker cleanup is ever performed.** CloudBurrow never runs `docker system
  prune` or removes images it did not create; other containers and images on the machine are
  none of its business.
- `cloudburrow delete` removes the cluster, and with it every endpoint deployed into it.

## Pinned versions and redistribution

| Component | Version | Licence | Redistribution |
|---|---|---|---|
| `google-cloud-aiplatform[prediction]` | 2.1.3 | Apache-2.0 | Developer-installed for contract verification; not redistributed |
| `golang:1.27-alpine` | `sha256:8a5910f3…3414` | BSD-3-Clause (Go) / MIT (Alpine) | Pull-only build stage; not redistributed |
| `gcr.io/distroless/static-debian12` | `sha256:d75cdd72…43f2` | Apache-2.0 | Pull-only base; not redistributed |

Both base images are pinned by digest in `testdata/predictor/Dockerfile`, so the fixture that
proved the contract is the fixture a later run rebuilds. Both digests are multi-arch manifest
lists and resolve on amd64 and arm64.

**No model weights are involved.** The fixture computes `n * 2`; it downloads nothing, and
the gated-artifact problems described in [local-ai.md](local-ai.md) do not arise here.

## Evidence

`test/compat/prediction_test.go`, run against a live cluster through the official Cloud Run
SDK:

```
=== RUN   TestPredictionEndpointServesTheVertexContract
    prediction endpoint ready at http://compat-predictor.default.127.0.0.1.sslip.io
      (predict .../v1/predict)
--- PASS: TestPredictionEndpointServesTheVertexContract (5.92s)
=== RUN   TestPredictionDeadlineBoundsASlowPrediction
    slow prediction correctly failed the caller: context deadline exceeded
--- PASS: TestPredictionDeadlineBoundsASlowPrediction (6.59s)
=== RUN   TestPredictionEndpointDeletionIsComplete
--- PASS: TestPredictionEndpointDeletionIsComplete (3.43s)
=== RUN   TestPredictionStartupFailureIsReported
    startup failure reported after 10m1s: ... FAIL_STARTUP is set: refusing to start
--- PASS: TestPredictionStartupFailureIsReported (602.64s)
```

The contract test uses **non-default routes** (`/healthz`, `/v1/predict`) and asserts that
the default routes return 404, so it proves the `AIP_*` environment is actually plumbed
through rather than that the fixture happens to agree with CloudBurrow's assumptions.
