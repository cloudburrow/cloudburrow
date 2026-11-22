# Custom prediction containers

CloudBurrow runs **Vertex AI custom prediction containers** — images that honour Google's
serving contract — on its own Kubernetes runtime.

This page records the decision [#42](https://github.com/cloudburrow/cloudburrow/issues/42)
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
"another unmanaged runtime" [#42](https://github.com/cloudburrow/cloudburrow/issues/42)
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

`AIP_STORAGE_URI` is set for a model deployed through the Model Registry (#1940): `UploadModel`'s
`artifactUri`, a directory in the instance's Cloud Storage, is copied into the managed bucket
`cloudburrow-vertex-models-{project}` and the variable names that copy, which the container reads
through `STORAGE_EMULATOR_HOST`. A container deployed directly as a Cloud Run service sets what it
sets itself; nothing is copied for it, and nothing is fetched from Google.

## The Model Registry and Endpoints

`ModelService` and `EndpointService` (`gcloud ai models` and `gcloud ai endpoints`, the SDKs'
`Model.upload` and `Endpoint.deploy`, Terraform's `google_vertex_ai_endpoint`, and Agent Platform →
Model Registry and Endpoints in the console) keep models with versions and aliases and endpoints with
a traffic split. `DeployModel` runs the version's serving container as a Cloud Run service in the
instance's cluster with Google's `AIP_*` variables and its health route as the startup probe; Predict,
RawPredict and Explain reach the deployed model the traffic split picks. `ExportModel` copies a
version's artifacts to Cloud Storage and its serving image (`image_destination`) into this
instance's Artifact Registry. Google's prebuilt prediction containers
(`us-docker.pkg.dev/vertex-ai/prediction/...`) are never downloaded: such a URI names the image of
the local `DOCKER` repository `projects/vertex-ai/locations/us/repositories/prediction`, which you
push it (or a server of the same contract) to, and `DeployModel` refuses it until that repository
holds it. The methods and their limits are in
[compatibility.md](compatibility.md#model-registry-and-endpoints-1940).

## Deploying a predictor

A predictor is deployed like any other container — through the Cloud Run v2 API, onto
Knative:

```go
c.CreateService(ctx, &runpb.CreateServiceRequest{
    Parent:    "projects/my-project/locations/us-central1",
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

The URI the API advertises is the **cluster ingress** address, which `cloudburrow up`
publishes on a host port (default `9080`, see [networking.md](networking.md)):

```sh
curl -s -X POST http://predictor.default.cloudburrow.localhost:9080/v1/predict \
  -d '{"instances":[1,2,3.5]}'
{"predictions":[2,4,7],"deployedModelId":"doubler-v1"}
```

Where `*.cloudburrow.localhost` does not resolve — notably a Go binary built with
`CGO_ENABLED=0` — name the service in the `Host` header instead, which needs no resolver:

```sh
curl -s -X POST http://127.0.0.1:9080/v1/predict \
  -H 'Host: predictor.default.cloudburrow.localhost' \
  -d '{"instances":[1,2,3.5]}'
```

A workload **inside** the cluster needs none of that and uses
`http://predictor.default.svc.cluster.local`.

## Endpoint lifecycle

Vertex has no local-endpoint resource to mirror — its `LocalEndpoint` is a Python object
wrapping a Docker container, not an API resource. `internal/prediction` therefore reports
what CloudBurrow actually runs, so the console pages
([#43](https://github.com/cloudburrow/cloudburrow/issues/43)–[#49](https://github.com/cloudburrow/cloudburrow/issues/49))
render real state rather than a Vertex-shaped status no API returns:

| State | Meaning |
|---|---|
| `PENDING` | The revision exists but is not serving yet. Knative reports `Unknown` while coming up, and calling that a failure would make every healthy deployment look broken for its first seconds. |
| `READY` | The container passed its health route and has a replica. |
| `SCALED_TO_ZERO` | Healthy with no replica. Requests still succeed; the first pays cold start. |
| `FAILED` | Terminal. The revision will not become ready without a new deployment. |

Only `FAILED` is terminal, so a caller polling a healthy endpoint that scales to zero does
not mistake it for a dead one.

### Startup failure takes up to 240 seconds to report

A container that exits at startup **is** reported, with its own output:

```
startup failure reported after 4m2s:
  reason="COMMON_REASON_UNDEFINED"
  message="Revision \"compat-predictor-broken-00001\" failed with message:
           Container failed with: 2026/09/21 14:33:56 FAIL_STARTUP is set: refusing to start"
```

but only once Knative's `progress-deadline` expires. The `reason` enum is undefined because
the adapter maps no Cloud Run `CommonReason` — Knative's reasons (`RevisionFailed`,
`ExitCode1`) have no Cloud Run equivalents, and inventing a mapping would put a wrong enum
where a caller looks first. The message carries the truth instead. Until the deadline the
endpoint reports `PENDING`, which is honest — Kubernetes is still restarting the container —
but slow.

Knative's default deadline is `600s`, and the Cloud Run v2 surface exposes no field that
shortens it: the failure used to be reported after **602 seconds, measured** (#568). `up` now
sets `progress-deadline` to `240s` in Knative's `config-deployment`, on a fresh install and on
a cluster that already has Knative, so an existing cluster gets it on its next `up`. 240 s is
Cloud Run's own default: its startup probe gives a container `timeoutSeconds: 240` with
`periodSeconds: 240` and `failureThreshold: 1` before the deployment fails. A container that
genuinely needs longer to start fails here where it would fail on Cloud Run too.

What CloudBurrow also improves is the message: Knative's top-level `Ready`
condition says only `Configuration "x" does not have any ready Revision`, so the adapter
prefers the `ConfigurationsReady` message, which names the revision and quotes the container's
own log line.

## Versus Vertex AI's own APIs

Since #1940 the Model Registry and Endpoints are served (above). A Cloud Run service deployed
directly, as below, is still an endpoint for Predict by its service ID, without a registry
`Endpoint` or `DeployedModel` behind it.

Online prediction is served (#1193): with Cloud Run enabled, the local AI endpoint (the
`local AI:` line `up` prints) serves `google.cloud.aiplatform.v1.PredictionService` over gRPC,
and the same names as JSON under `/v1`. The endpoint ID is the Cloud Run service ID, and the
request is relayed to the container's predict route through the cluster ingress:

```go
c, _ := aiplatform.NewPredictionClient(ctx,
    option.WithEndpoint("127.0.0.1:<local AI port>"),
    option.WithoutAuthentication(),
    option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
resp, err := c.Predict(ctx, &aiplatformpb.PredictRequest{
    Endpoint:  "projects/my-project/locations/us-central1/endpoints/predictor",
    Instances: []*structpb.Value{structpb.NewNumberValue(1)},
})
```

`Predict` checks the contract's one-prediction-per-instance rule and returns the container's
own refusals in its words; `RawPredict` relays the body unchanged, and `StreamRawPredict` and
the tensor methods (`DirectPredict`, `ServerStreamingPredict`, …) relay it as one message
(#1285). On a publisher model,
`RawPredict` takes the local model's native body instead (#1284; see
[compatibility.md](compatibility.md#vertex-ai-predictionservice--googlecloudaiplatformv1)).

| Vertex surface | Status |
|---|---|
| Serving contract (`AIP_*`, `/predict`, `/health`) | **Verified** |
| `aiplatform.Model` registry (upload, list, version) | **Not supported** |
| `aiplatform.Endpoint` / `DeployedModel` | **Not supported** |
| `PredictionService.Predict` / `RawPredict` | **Verified** over gRPC on the local AI endpoint, as `projects/{p}/locations/{l}/endpoints/{service}` (#1193); see [compatibility.md](compatibility.md#vertex-ai-predictionservice--googlecloudaiplatformv1) |
| `containerSpec.grpcPorts` | **Verified**: the service's container port named `h2c` (#1477); see [A model's gRPC server](#a-models-grpc-server) |
| `PredictionService.DirectRawPredict` / `StreamDirectRawPredict` / `StreamingRawPredict` | **Verified**, relayed to the container's gRPC server (#1477) |
| `PredictionService.Explain` | Sampled Shapley only, from the container's predictions, with the spec in `explanation_spec_override` since there is no `DeployedModel` to carry one (#1454); see [compatibility.md](compatibility.md#vertex-ai-predictionservice--googlecloudaiplatformv1) |
| `LocalModel.build_cpr_model` | Available to you; CloudBurrow does not invoke it |
| `LocalModel.deploy_to_local_endpoint` | **Declined by design** — see [The decision](#the-decision) |
| Model Garden, tuning, batch prediction | **Not supported** |

## A model's gRPC server

Vertex AI's `containerSpec.grpcPorts` names the port a model's container serves gRPC on: Vertex
sends `DirectRawPredict`, `StreamDirectRawPredict` and `StreamingRawPredict` there, and its health
checks too. A model here is a Cloud Run service, and Cloud Run serves a container port end to end
over HTTP/2 cleartext when the port is named `h2c` (#1475). So a model's `grpcPorts` are deployed
as its service's port named `h2c` (#1477):

```go
c.CreateService(ctx, &runpb.CreateServiceRequest{
    Parent:    "projects/my-project/locations/us-central1",
    ServiceId: "model",
    Service: &runpb.Service{Template: &runpb.RevisionTemplate{
        Containers: []*runpb.Container{{
            Image: "dev.local/my-grpc-model:v1",
            Ports: []*runpb.ContainerPort{{Name: "h2c", ContainerPort: 8500}},
            Env:   []*runpb.EnvVar{{Name: "AIP_DEPLOYED_MODEL_ID", Values: &runpb.EnvVar_Value{Value: "m1"}}},
        }},
    }},
})
```

or `gcloud run deploy model --image=... --port=8500 --use-http2`, or **Use HTTP/2 end-to-end** on
the console's Cloud Run deploy form. An `AIP_*` variable claims the contract, as for any prediction
container. Cloud Run has one container port, so the gRPC port is also the one Knative probes, as
Vertex sends its health checks to the first `grpcPorts` entry.

The three methods then call `method_name` on that server, through the cluster ingress with the
service's host as `:authority`, sending `input` as the request message's bytes and returning each
response message as `output`, undecoded:

```go
resp, err := c.DirectRawPredict(ctx, &aiplatformpb.DirectRawPredictRequest{
    Endpoint:   "projects/my-project/locations/us-central1/endpoints/model",
    MethodName: "/tensorflow.serving.PredictionService/Predict",
    Input:      requestBytes, // a serialized tensorflow.serving.PredictRequest
})
```

`StreamDirectRawPredict` and `StreamingRawPredict` are one bidirectional stream to `method_name`:
each request's `input` is sent as it arrives and each message the container sends is returned as it
arrives. The container's own status comes back with its code and message. A model with no `h2c`
port is `FAILED_PRECONDITION`; see
[compatibility.md](compatibility.md#vertex-ai-predictionservice--googlecloudaiplatformv1) for the
rest. The console's Online prediction page offers **DirectRawPredict** on such an endpoint: a method
name and base64 input, and the output bytes or the container's status.

## Model Garden: Hugging Face models (#1840)

`ModelGardenService.Deploy` with a `hugging_face_model_id` (or the console's **Deploy** on a model
downloaded in Model Garden) creates an Endpoint and a Model and serves the model with the engine
Google's Model Garden uses for its task:

| Hugging Face task | Engine | Image (pinned in `dependencies.json`) |
|---|---|---|
| text-generation, text2text-generation, image-text-to-text | vLLM, CPU build (Apache-2.0) | `vllm/vllm-openai-cpu:v0.34.0` (linux/arm64, linux/amd64) |
| feature-extraction, sentence-similarity | Text Embeddings Inference, CPU build (Apache-2.0) | `ghcr.io/huggingface/text-embeddings-inference:cpu-1.9` (linux/amd64; emulated on arm64) |

The arguments are Model Garden's (`--max-model-len=4096`, capped at the model's own context,
`--dtype=auto`, `--max-num-seqs=256`, `--disable-log-stats`); the GPU-only ones have no CPU
meaning. The container, `cloudburrow-<instance>-mg-<endpoint>`, runs in the Docker the cluster runs
in, publishes its port on 127.0.0.1 only and mounts the downloaded files read-only; HF_TOKEN is
never passed in (the files are already local). A model not yet downloaded is downloaded first, with
the same SHA-256 checks and 50 GB-free rule as Model Garden's Download. A token in the request
(`model_config.hugging_face_access_token`) is refused: set `HF_TOKEN` where CloudBurrow runs.

The endpoint answers, over gRPC and JSON:

- `:predict`: on vLLM, Model Garden's instances `{"prompt", "max_tokens", "temperature", "top_p",
  "top_k", "raw_response"}`, answered as `"Prompt:\n…\nOutput:\n…"` (the text alone with
  `raw_response`), or `{"@requestFormat": "chatCompletions", "messages": [...]}`, answered with the
  chat completion; on TEI, `{"inputs": "..."}` instances, each answered with its vectors.
- `:rawPredict`: an OpenAI completions (`prompt`) or chat (`messages`) body to vLLM, TEI's own
  `{"inputs"}` to TEI, or a `{"instances"}` body answered as `:predict`.
- `:streamRawPredict`: vLLM's server-sent events.
- `POST /v1beta1/{endpoint}/chat/completions` (and v1beta1 `ChatCompletions`): the OpenAI-compatible
  path; point the OpenAI client's base URL at `http://<local AI>/v1beta1/{endpoint}`.

`UndeployModel` and the console's **Undeploy** remove the container; `DeleteEndpoint` and
`DeleteModel` remove the rest. The downloaded files stay until Model Garden's **Delete files**.
`cloudburrow stop` removes the containers and `up` starts them again.

## Accelerators

| Host | What happens |
|---|---|
| CPU (amd64, arm64) | Works: the default everywhere but a Mac with Apple silicon, and what `machine_spec` with no accelerator (`local-cpu`) asks for. |
| macOS on Apple silicon | **The Apple GPU, through Metal, on the host** (#2155): see [Apple GPU (Metal)](#apple-gpu-metal). |
| NVIDIA | Untested. kind can be configured with the NVIDIA container runtime, and Kubernetes can schedule `nvidia.com/gpu` — CloudBurrow does neither, and claims nothing it has not run. On Linux a GPU machine spec runs on CPU. |

Metal is not reachable from a Linux container: Docker Desktop runs the cluster inside a Linux VM,
and that VM has no Metal device. NVIDIA container flags on a Mac would be ignored, not fail, so
CloudBurrow never passes them. The Apple GPU is reached by running the model server as a process
on the Mac itself, not in a container.

## Apple GPU (Metal)

On macOS with Apple silicon (#2155) a Model Garden deployment on a GPU runs on the Mac's GPU:

| Model | Host runtime | Pinned in `dependencies.json` |
|---|---|---|
| GGUF (llama.cpp's engine) | `llama-server` from llama.cpp's official macOS arm64 release archive (MIT), the same build b12211 as the cluster's image, `--n-gpu-layers 999` | `localai.hostLlamaCpp`: the archive's SHA-256 |
| MLX (`library_name: mlx`) | `python -m mlx_lm server` (MIT), MLX's Metal backend | `localai.hostMLX`: `internal/hostai/mlx-lock.txt`, every package by version and SHA-256 |

**Choosing it.** `machine_spec.accelerator_type` decides, as on Google: a GPU type (`NVIDIA_L4`,
`accelerator_count` 1; any NVIDIA type is accepted) is the Apple GPU, machine type `local-metal`; a
machine spec with no accelerator is CPU, `local-cpu`; a TPU type, or a count with no type, is
`INVALID_ARGUMENT`. A deploy with no `machine_spec` takes the instance's default,
`cloudburrow up --local-ai-accelerator metal|cpu` (`localAI.accelerator`), which is `metal` on a Mac
with Apple silicon and `cpu` everywhere else; `metal` is refused on any other machine. The console's
deploy form offers **Accelerator type** "Apple GPU (Metal), local", chosen by default when the Mac
has one, or "None (CPU)". Runtimes with no Metal build (vLLM, Text Embeddings Inference, the
PyTorch and ONNX runtimes) run on CPU in the cluster even on a GPU spec, and the deployed model's
details say so.

**How it runs.** The first deploy downloads the runtime into the user cache
(`~/Library/Caches/cloudburrow/hostai`, or `CLOUDBURROW_HOSTAI_DIR`): the llama.cpp archive is
checked against its SHA-256 before it is unpacked; mlx-lm is installed with
`--require-hashes --no-deps` into a virtual environment of CloudBurrow's own, with `uv` if it is on
`PATH`, else an official python.org or Homebrew Python 3.11–3.13. Nothing is installed system-wide.
The model's files are downloaded from the Hub to `<state-dir>/host-models/mg-<endpoint>` (the
GGUF file alone, or the MLX repository's weights and tokenizer), with the 50 GB-free rule. The
server binds 127.0.0.1 only, on a free port, with `HF_HUB_OFFLINE=1`. A supervisor runs it in its
own process group, checks its health every 5 s, restarts it with back-off when it exits or fails
three checks, and writes its output to Cloud Logging as the endpoint's
`aiplatform.googleapis.com/prediction_container` log. `UndeployModel` and `cloudburrow stop` stop
it, process group and all; a server a crashed instance left is found by its pid file and stopped
on the next `up`.

**Reaching it.** The cluster reaches the server as it would a pod: the Service `mg-<endpoint>` has
no selector and an EndpointSlice at the host's address on Docker's network
(`host.docker.internal`), so `:predict`, `:rawPredict`, `:streamRawPredict` and the OpenAI-compatible
`chat/completions` work unchanged. The Endpoints page shows **Accelerator** "Apple GPU (Metal),
local", and the deployed model's details its machine type, accelerator type and count, and the
chip.

**Measured** on an Apple M4 Max, generating 128 tokens with Qwen/Qwen2.5-0.5B-Instruct-GGUF
(Q4_K_M), by test/compat/modelgardenmetal2155_test.go: **243 tokens/s on the Apple GPU** against
**8.5 tokens/s on CPU in the cluster** (llama.cpp's image, the same build), 28.6×; the server logs
`using device MTL0 (Apple M4 Max)` and `offloaded 25/25 layers to GPU`.
mlx-community/Qwen2.5-0.5B-Instruct-4bit on MLX's Metal backend: 234–245 tokens/s.

**What stays on CPU, and why.**

- Gemini on LiteRT-LM (`cloudburrow_generate`) runs in its container: LiteRT-LM's macOS release has
  a GPU backend, but CloudBurrow's runner is built from source with its own patches (per-request
  sampler, logprobs, constraints) for Linux only; a macOS build of the runner is #2159.
- Embeddings on ONNX Runtime (the `onnx` build) stay on CPU: ONNX Runtime's Core ML execution
  provider was measured slower than its CPU provider on the shipped int8 EmbeddingGemma model
  (8 × 128 tokens: 782 ms on CPU, 989 ms with Core ML on CPU and GPU, 3028 ms on all units),
  because Core ML has no int8 matrix-multiply kernels and the graph is split back to CPU.
- Custom containers deployed with `DeployModel` run in the cluster on CPU, whatever their machine
  spec: a container cannot reach Metal.

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
| `registry.k8s.io/e2e-test-images/agnhost` | 2.53, `sha256:99c6b4bb…1a85` | Apache-2.0 | Pulled by the cluster for the gRPC model tests (#1477); not redistributed |

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
    prediction endpoint ready at http://compat-predictor.default.cloudburrow.localhost
      (predict .../v1/predict)
--- PASS: TestPredictionEndpointServesTheVertexContract (5.92s)
=== RUN   TestPredictionDeadlineBoundsASlowPrediction
    slow prediction correctly failed the caller: context deadline exceeded
--- PASS: TestPredictionDeadlineBoundsASlowPrediction (6.59s)
=== RUN   TestPredictionEndpointDeletionIsComplete
--- PASS: TestPredictionEndpointDeletionIsComplete (3.43s)
=== RUN   TestPredictionStartupFailureIsReported
    startup failure reported after 4m2s: ... FAIL_STARTUP is set: refusing to start
--- PASS: TestPredictionStartupFailureIsReported (244.35s)
```

The contract test uses **non-default routes** (`/healthz`, `/v1/predict`) and asserts that
the default routes return 404, so it proves the `AIP_*` environment is actually plumbed
through rather than that the fixture happens to agree with CloudBurrow's assumptions.

`test/compat/predictiondirectraw_test.go`, the gRPC model (#1477): agnhost's
`grpc-health-checking` deployed with its port named `h2c`, called through the official
`aiplatform` PredictionClient and through the console, on one instance with `--services run`
(2026-10-16, darwin/arm64):

```
=== RUN   TestPredictionClientReachesAModelsGRPCServer
--- PASS: TestPredictionClientReachesAModelsGRPCServer (19.65s)
=== RUN   TestConsoleDirectRawPredictThroughTheConsoleAPI
--- PASS: TestConsoleDirectRawPredictThroughTheConsoleAPI (3.45s)
```
