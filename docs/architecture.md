# CloudBurrow architecture

Status: accepted for the first release cycle · Revised 2026-09-20 by #25 · Module map and
rules revised 2026-09-27 by #588
Supersedes the single-process, all-custom-Go, direct-Docker design recorded in
[ADR-0001](adr/0001-independent-go-implementation.md)–[ADR-0003](adr/0003-state-and-persistence.md).

This document is the contract the remaining issues implement against. Where it and an
implementation disagree, one of the two is a bug.

> **This document is the design, not the support matrix.** Most of it is implemented. What
> works is recorded per operation in [`compatibility.md`](compatibility.md), where a row is
> `Verified` only when a merged test drives it through an official Google client, and
> summarised in [`status.md`](status.md). Anything here that is not built yet says so.

---

## 1. What CloudBurrow is

A local Kubernetes cluster that speaks Google Cloud APIs.

Applications built with official Google Cloud SDKs run end to end on a developer machine with
no GCP project, no credentials and no network egress — and the same cluster is a real
Kubernetes cluster, so `kubectl`, Helm charts and operators work against it directly.

Two distinct capabilities, deliberately not conflated:

- **GCP API compatibility** — Cloud Storage, Pub/Sub, Cloud Tasks and Cloud Run v2 answered
  locally for official SDKs.
- **Native Kubernetes portability** — ordinary manifests, Helm charts and operators applied to
  the cluster without going through CloudBurrow at all.

### 1.1 Reuse is the default

CloudBurrow does not rewrite working upstream components to keep everything in one language.
The [upstream reuse audit](upstream-evaluation.md) decided each service on measured evidence:

| Service | Approach | Component |
|---|---|---|
| Pub/Sub | Integrate | Google `cloud-pubsub-emulator` 0.8.35 |
| Cloud Storage | Build | CloudBurrow's own server, to the discovery document (#485; replaced an upstream emulator in #519) |
| Cloud Tasks | Build | no viable upstream found |
| Cloud Run | Adapt onto upstream | Knative Serving v1.23.0 |

Building instead of integrating requires a **specific unmet requirement and measured
evidence**. An upstream being Java, or having a clock we cannot inject, is an integration
constraint — not a reason to replace it.

### 1.2 The acceptance workflow

The release is judged by one workflow (#19):

1. **Upload** an object to Cloud Storage with an official client.
2. **Publish** an event describing it to Pub/Sub.
3. **Run** a worker — a Cloud Run service backed by Knative — that receives the event and
   reads the object.
4. **Save** the result as a new object, read back from the host with an official client.

Every step uses an official SDK. If any step needs a CloudBurrow-specific client, the release
has failed its own test.

### 1.3 Non-goals

Out of contract, not merely unscheduled:

- **Full IAM enforcement.** Stored policies are not enforced
  ([ADR-0006](adr/0006-iam-policy-surface.md)), except by the checks it lists: Memorystore's
  IAM auth, Firestore with MongoDB compatibility's sign-ins, IAM Credentials on service
  accounts the IAM API created, and BigQuery's per-project IAM of a minted token's caller, each
  against this instance's own minted tokens and the project's stored policy (`internal/iamcond`,
  `internal/iamroles`), and BigQuery remote models' connection permissions; and, with
  `up --iam-enforcement=on` (#1879, off by default), the methods `internal/iameval` maps on Secret
  Manager, Cloud KMS, Cloud Tasks, Cloud Storage, Pub/Sub, Cloud Run invoke, BigQuery sharing, Cloud
  Scheduler, Workflows, Cloud Logging, Cloud Monitoring, Memorystore, the Cloud SQL Admin API, the
  Cloud Run Admin API (v2 and v1), Cloud Functions, Artifact Registry and Cloud Build, and
  `iam.serviceAccounts.actAs` where an API attaches a service account
  ([compatibility.md](compatibility.md#iam-enforcement)).
- **GKE-specific APIs**, Google-managed load balancing, storage classes and identity.
- **BigQuery beyond what its community emulator does.** Firestore, Spanner, Bigtable and
  Datastore now ship as opt-in Google emulators. BigQuery ships as `goccy/bigquery-emulator`,
  because Google publishes none, built by CloudBurrow with patches to its SQL engine (#1061), and
  inherits its other limits (docs/compatibility.md).
- **Production durability.** CloudBurrow is a development tool. Before 1.0 the stored state
  of the latest release loads in the next, and no further back (docs/install.md, Upgrading).
- **A promise that an unmodified GKE manifest runs unchanged.** Endpoint configuration and
  local overlays legitimately differ. See §7.

---

## 2. Shape of the system

```
 host                                    │  local Kubernetes cluster (kind)
 ────────────────────────────────────────┼──────────────────────────────────────────
  cloudburrow CLI                        │   ┌─ cloudburrow namespace ──────────┐
   ├─ cluster lifecycle (create/delete)  │   │  Pub/Sub emulator      (Service) │
   ├─ component install + readiness      │   │  Cloud Storage (ours)  (Service) │
   ├─ endpoint reporting                 │   │  Cloud Tasks (ours)    (Service) │
   └─ explicit kubeconfig, own context   │   │  Cloud Run v2 adapter  (Service) │
                                         │   └──────────────────────────────────┘
  kubectl / helm / operators ────────────┼──▶ Kubernetes API (direct, unmediated)
                                         │   ┌─ knative-serving ────────────────┐
                                         │   │  Knative Serving + net-kourier   │
                                         │   └──────────────────────────────────┘
```

**The host CLI owns**: cluster lifecycle, component installation, readiness aggregation,
endpoint discovery and reporting, and reset. It holds no application state.

**The cluster owns**: every service backend, its persistence, and all workload execution.

---

## 3. Module boundaries

The package set as of 2026-09-27, one line each, taken from each package's doc comment. A
row marked **Planned** names a package that does not exist yet.

| Path | Purpose |
|---|---|
| `cmd/cloudburrow/` | The CLI: argument parsing, and the wiring that builds every in-process service, the console, the admin API and their observers for `up`; also `env`, `terraform`, `gcloud-setup`, `logs`, `state` and the other subcommands. Tested in the package. |
| `cmd/cloudburrow-storage/` | The builtin Cloud Storage server alone: cross-built for Linux, embedded in the CLI, and run by the in-cluster storage Deployment (#514). `cloudburrow-storage bigquery-front` is BigQuery's validating front (#902) `cloudburrow-storage pubsub-front` the Pub/Sub front (#873) and `cloudburrow-storage bigtable-front` the Bigtable front (#1206), each from the same image in its emulator's pod. |
| `internal/ac3/` | An AC-3 and E-AC-3 decoder written from ATSC A/52, for a video's audio track in Vertex AI EmbedContent (#1739). |
| `internal/adapter/run/` | Cloud Run v2 API mapped onto Knative Serving (services, revisions) and Kubernetes batch Jobs (jobs, executions); refuses what it cannot map (ADR-0005). |
| `internal/admin/` | The loopback-only control API: seed, reset, event inspection. |
| `internal/aipfilter/` | The shared list `filter` (AIP-160) and `order_by` (AIP-132) evaluator (#1943): compiles a filter or order against a resource's message descriptor (INVALID_ARGUMENT for an unknown field, a wrong-typed value or bad syntax), matches and sorts protocol buffer resources, the Locations mixin's filter, and an order-preserving pager. |
| `internal/apicontract/` | Pins the Google API contracts CloudBurrow implements against, and where each comes from. |
| `internal/authplatform/` | Google Auth Platform's OAuth app and clients (the console's Branding, Audience, Clients and Data Access) and the authorization, token-grant and revocation endpoints the metadata server serves for them (#2030). |
| `internal/quotalimit/` | The quota limits APIs enforce, read from the Cloud Quotas API's granted preferences, and the usage its safety checks compare with (#2030). |
| `internal/apienable/` | Refuses calls to an API disabled in their project with Google's `SERVICE_DISABLED` (#1914): the gRPC interceptors and HTTP gate the in-process services and the in-cluster servers wrap, the Service Usage server's decision, and the host endpoint the in-cluster servers ask, with a one-second cache. |
| `internal/apierror/` | One internal cause mapped to a Google-style gRPC status and JSON error body. |
| `internal/archtest/` | Tests only (#672): enforces rules 1–3 below against the module, each with an explicit list of today's exceptions. |
| `internal/bigqueryfront/` | The checks in front of the BigQuery emulator (#861), run in its pod by `cloudburrow-storage bigquery-front` (#902): IDs, schemas, DDL, jobs and `insertAll` rows are refused as BigQuery refuses them, with 400 or 409 and per-row `insertErrors`, and what the emulator would report done without doing is 501. |
| `internal/bqiceberg/` | Writes the Parquet data files and Apache Iceberg metadata of BigQuery tables for Apache Iceberg (#1427) with `apache/iceberg-go`, on a local mirror the BigQuery front uploads to the instance's Cloud Storage. |
| `internal/bigqueryimage/` | Builds the in-cluster BigQuery emulator's image from the patched goccy/bigquery-emulator builds the CLI embeds (#1061), and says which architectures a CLI embeds. |
| `internal/bigqueryimage/bigqueryimagetest/` | Tests only: stand-in embedded emulator builds, present or missing (#1061). |
| `internal/buildpacks/` | Turns source into a runnable image with Google Buildpacks and `pack`. |
| `internal/calllog/` | The call log the Pub/Sub, Bigtable and call fronts keep in their pods (#1266, #1267, #1288): each call's method, resource, code, duration and `traceparent`, never a payload, in a bounded ring at `/_cloudburrow/calls?after=N` that the CLI reads through the tunnel into `/metrics`, `/admin/events` and traces. |
| `internal/cmek/` | The customer-managed encryption key helper Cloud Logging, Spanner, Document AI and Speech-to-Text share (#1950): checks a key in the instance's Cloud KMS (exists, ENCRYPT_DECRYPT, primary version ENABLED) and, with IAM enforcement on, the service agent's `cloudkms.cryptoKeyVersions.useToEncrypt`/`useToDecrypt` through the host's evaluator; seals a value with a fresh data key wrapped by the key, so a disabled or destroyed version refuses the read. |
| `internal/cluster/` | The local kind cluster: create, discover, stop, start, delete, with an explicit kubeconfig. |
| `internal/components/` | Installs and manages the in-cluster backends and Knative Serving from pinned manifests. |
| `internal/config/` | Configuration model, precedence (flags over environment over file over defaults), validation. |
| `internal/console/` | The local web console: a view over the same surfaces an SDK uses, never a store of its own. |
| `internal/datastoreversions/` | The versions of the Datastore emulator's entities (#1967), kept by the Datastore pod's call front, which the emulator does not keep: every commit is recorded with its time, a key's entity before its first recorded commit beside it, so lookups, queries (over a snapshot written into the emulator), aggregations and read-only transactions at an earlier `read_time`, and exports and clones at a snapshot time, read the entities as they were. Kept in the front's state directory, pruned to the retention and bounded in size, and discarded when the emulator's entities are not the ones it was kept for. |
| `internal/debugpprof/` | Serves the Go runtime's profiles on a loopback address when `CLOUDBURROW_DEBUG_PPROF` names one, for a soak or a leak hunt ([soak.md](soak.md)); off by default. |
| `internal/doctor/` | Diagnoses a workstation before a cluster is created. |
| `internal/embedding/` | Runs the community ONNX int8 conversion of EmbeddingGemma for the Vertex-style `:predict` embedding endpoint (#41), labelled as NOT an official Google artifact; the ONNX Runtime and tokenizer need cgo and the `onnx` build tag, and the default build answers 501. |
| `internal/onnxmodel/` | Runs an imported ONNX model on ONNX Runtime for BigQuery ML's `MODEL_TYPE='ONNX'` (#1711), sharing the runtime `internal/embedding` loads; cgo and the `onnx` build tag, the default build answers `ErrNotCompiled`. `onnxgen/` hand-writes the tiny test models as ONNX protobuf. The BigQuery front, which runs in the cluster without it, reaches it through `internal/onnxrunner`. |
| `internal/tflitemodel/` | Reads and runs an imported TensorFlow Lite model for BigQuery ML's `MODEL_TYPE='TENSORFLOW_LITE'` (#1755): LiteRT's `schema.fbs` FlatBuffer, its float builtin operators run in float32 as LiteRT runs them, in pure Go, so the CGO-less front runs it in every build. |
| `internal/xgbmodel/` | Reads and runs an imported XGBoost model for BigQuery ML's `MODEL_TYPE='XGBOOST'` (#1711): XGBoost's documented model document as JSON or UBJSON, predicted as XGBoost 3.1.0 predicts (gbtree, DART, gblinear; categorical splits; each objective's transform), in pure Go, so the CGO-less front runs it in every build. |
| `internal/onnxrunner/` | The ONNX runner (#1711): a small JSON-over-HTTP API that loads a model's bytes, keyed by their SHA-256, into a cache bounded by bytes (least recently used evicted), and runs it on `internal/onnxmodel`. The onnx build's CLI serves it on `--port-onnx` (default 9025) with BigQuery enabled and publishes it through `cloudburrow-host`, as Resource Manager is (#1392); the BigQuery front calls it with its `Client`, given the address by `--onnx`, and sends a model again when the runner answers that it no longer holds it. |
| `internal/imagelabel/` | Vision's `LABEL_DETECTION` (#1799): Google's MediaPipe EfficientNet-Lite0 image classifier, run on `internal/tflitemodel`, its 1,000 ImageNet labels read from the model file; the file is fetched once from storage.googleapis.com and pinned by SHA-256 in `internal/localai`. |
| `internal/imagedetect/` | Vision's `OBJECT_LOCALIZATION` and `FACE_DETECTION` (#1799): Google's MediaPipe EfficientDet-Lite0 object detector (COCO's 80 labels) and BlazeFace short-range face detector, run on `internal/tflitemodel`, with MediaPipe's anchor, box-decoding and non-maximum-suppression steps; each file is fetched once from storage.googleapis.com and pinned by SHA-256 in `internal/localai`. |
| `internal/syntaxnet/` | Natural Language's `analyzeSyntax` and BigQuery ML's `ANALYZE_SYNTAX` (#1799): SyntaxNet's Parsey McParseface, Google's English part-of-speech tagger and dependency parser, run in pure Go: its TensorFlow V1 checkpoints read directly, its feed-forward networks, features, arc-standard and tagger transitions and beam search reimplemented, and an offset-keeping Penn Treebank tokenizer; the model files are fetched once from tensorflow/models and pinned by SHA-256 in `internal/localai`. |
| `internal/ocr/` | Tesseract OCR (`third_party/tesseract-wasm`, a WASI module on wazero, no cgo) for EmbedContent's `document_ocr` on scanned PDF pages (#1733): orientation and script detection, then the embedded `eng` model or the tessdata_fast script model `internal/localai` fetches on demand. |
| `internal/officedoc/` | Reads HTML, DOCX, PPTX, XLSX and XLSM in pure Go (archive/zip, encoding/xml, golang.org/x/net/html) into the blocks the local Document AI Layout Parser lays out: titles, headings, paragraphs, lists, tables with spans, page headers and footers, pages (#1799). |
| `internal/langid/` | Identifies a text's language with Google's MediaPipe Language Detector (embedded from `third_party/mediapipe-langid`, 110 languages), run in pure Go on `internal/tflitemodel` with ports of its two custom operators, for BigQuery `ML.TRANSLATE`'s `DETECT_LANGUAGE` and `detected_language_code` (#1799). |
| `internal/speaker/` | Speaker diarization for Speech-to-Text (#2131): Kaldi filter-bank features in Go, 3D-Speaker's CAM++ speaker embeddings on ONNX Runtime in the `-tags onnx` build (a stub elsewhere, whose diarization is 501), and agglomerative clustering of windows over whisper's words honouring min and max speaker counts; the model is fetched once into `<state-dir>/models` and pinned by SHA-256 in `internal/localai`. |
| `internal/speech/` | Transcribes audio with whisper.cpp compiled to a WASI module (`third_party/whisper-wasm`, on wazero, no cgo), decoding AMR and AMR-WB with opencore-amr's decoders and denoising with RNNoise, each compiled the same way (`third_party/amr-wasm`, `third_party/rnnoise-wasm`, #1799), for the local Vertex AI endpoint's Speech-to-Text v2 recognize route and BigQuery `ML.TRANSCRIBE` (#1799); its ggml model is fetched once into `<state-dir>/models` and pinned by SHA-256 in `internal/localai`. |
| `internal/speechconfig/` | Reads and checks Speech-to-Text v2's `RecognitionConfig` for local transcription, shared by the recognize route and `ML.TRANSCRIBE` without importing `internal/speech` (#1799). |
| `internal/sparkproc/` | BigQuery's stored procedures for Apache Spark (#2104): a JSON-over-HTTP API that runs one procedure's job (code or files from Cloud Storage, arguments, properties) and answers its OUT values or failure. The CLI serves it on `--port-spark` (default 9049) with BigQuery enabled and publishes it through `cloudburrow-host`; each job is `spark-submit --master local[*]` in the pinned `apache/spark` image of its runtime version (or its `container_image`, read from the instance's Artifact Registry with go-containerregistry, the runtime's Spark and Java mounted into it) with Google's BigQuery connector for that Spark, an `EXTERNAL SECURITY INVOKER` job's connector sending the caller's token or one minted for its custom service account, on a `docker network create --internal` network whose one peer relays the instance's BigQuery REST and Storage API ports (TLS for gRPC, a CA made per run), so the CLI stays static and Spark reaches nothing else. The BigQuery front's `--spark` names it; the driver's output goes to Cloud Logging. |
| `internal/tfconvert/` | The TensorFlow SavedModel converter (#1775): a JSON-over-HTTP API that takes a tar of a SavedModel's files and answers a bundle (the SavedModel with its signature, and its TensorFlow Lite flatbuffer or why TensorFlow Lite cannot take it, `flex` or `convert`), or a refusal (`format`, `signature`, `custom`, `ragged`, `sparse`, `limit`, `unavailable`); and the TensorFlow runtime (`/v1/models`, `/v1/run`), long-lived predictor containers that run a bundle's SavedModel on TensorFlow for ML.PREDICT and ML.EXPLAIN_PREDICT. The CLI serves it on `--port-tfconvert` (default 9027) with BigQuery enabled and publishes it through `cloudburrow-host`; each conversion is a `docker run`, on the `--internal` Docker network `cloudburrow-tfconvert` (no route out), of the image built from `deploy/tfconvert` on Google's pinned `tensorflow/tensorflow:2.20.0` (Google's `tf.lite.TFLiteConverter` from PyPI `tensorflow==2.20.0`), so the CLI stays static. The BigQuery front's `--tfconvert` names it, and `internal/tflitemodel` runs the result. |
| `internal/faultgate/` | Fault rules for the servers in the cluster (#2113): the host endpoint the Pub/Sub, Bigtable, Spanner, Firestore and Datastore fronts and the storage server ask for the `/admin/faults` rules and a decision, with a one-second cache, the gRPC interceptor and HTTP gate they wrap, and storage's `Faulter`. |
| `internal/videodecode/` | Decodes the AVI, FLV, MPEG and WMV video multimodalembedding@001's `:predict` takes (#1651) with FFmpeg in a locally built image (its `Dockerfile`: Debian's pinned `ffmpeg` package on the official `debian:trixie-slim`), `docker run --network none --read-only`: ffprobe lists the frames, ffmpeg decodes the sampled ones to raw planes, and `internal/embedding` samples, converts and patches them as it does an MP4's. FFmpeg runs as a separate program; none of it is linked or in the repository. |
| `internal/frontready/` | The readiness path a front serves for the backend beside it (#1114): the BigQuery and Pub/Sub emulators listen on their pod's loopback alone, so no other pod reaches them past their fronts, and Kubernetes, which probes at the pod's IP, asks their readiness of the front, which dials them on loopback. |
| `internal/frontstate/` | The state directory the Spanner, Bigtable and Firestore fronts keep their records and data copies in (#1514), on a size-limited emptyDir of their pod: one records file replaced whole by a rename, and a file per backup, snapshot or deleted table, written once; and the reset path `cloudburrow reset` clears it through. |
| `internal/hooks/` | Runs the `ready.d` and `shutdown.d` lifecycle script directories (#285). |
| `internal/grpchttp/` | Builds gRPC servers that are served through `ServeHTTP` (h2c) with `WaitForHandlers`, and stops them so `Stop` waits for every call's handler, its stats `End` included, before returning (#1804). `grpchttptest` holds the shared regression-test helpers. |
| `internal/hostguard/` | Refuses HTTP requests whose Host header names a host CloudBurrow does not answer to: the DNS-rebinding defence on every HTTP listener (#676). |
| `internal/unserved/` | The local-only guard (#1975): the inventories of every endpoint setting of the pinned hashicorp/google, google-beta and gcloud (`scripts/endpoint-inventory.sh`), and the refusal the Resource Manager port serves at `/_cloudburrow/unserved/` for every one this instance does not serve, which `cloudburrow terraform`, `env --format terraform` and `gcloud-setup` point them at. |
| `internal/nbruntime/` | BigQuery Studio's notebook runtimes (#2094), CloudBurrow's stand-in for a Colab Enterprise runtime: a notebook's Connect builds (once) an image from `image/` (the pinned `python:3.12-slim` with Google's `google-cloud-bigquery` and `bigquery-magics`, IPython, pandas and matplotlib) and runs `kernel.py`, an IPython shell answering one cell per line on stdin with its nbformat outputs, on a `docker network create --internal` network whose one peer (`relay.py`) relays the instance's BigQuery REST port; Interrupt, Restart and Disconnect, and a 180-minute idle shutdown. The console reaches it through `POST /api/studio/bigquery?call=runtime` (`cmd/cloudburrow/consolebigquerynotebooks.go`). |
| `internal/hostrelay/` | TCP relay that lets pods reach services the CLI serves on loopback (#575). |
| `internal/iamcheck/` | How the servers CloudBurrow runs in the cluster (Cloud Storage, Pub/Sub's front) ask the host's one evaluator, `internal/iameval`, whether a request's caller holds a permission, under `up --iam-enforcement=on` (#1879). |
| `internal/iamgate/` | IAM enforcement in the fronts beside the Google emulators (Firestore's and Spanner's call fronts, Bigtable's front): each method's documented permissions on the resource its request names, over gRPC and REST, decided by the host's evaluator through `internal/iamcheck` (#1879). |
| `internal/iamcond/` | Compiles and evaluates IAM Conditions: the CEL subset Google accepts on a role binding (#1742, #1745). |
| `internal/wif/` | Workload Identity Federation (#1941): CEL attribute mappings and conditions, OIDC token verification against a JWKS, SAML 2.0 assertion verification against IdP metadata (#2065, XML-DSig by goxmldsig, xmlenc decryption), and the `principal://` and `principalSet://` identifiers of a federated subject. |
| `internal/wif/samltest/` | A throwaway SAML 2.0 IdP for tests (#2065): an in-memory signing key, its metadata, and signed or encrypted responses. |
| `internal/iameval/` | CloudBurrow's one evaluator of IAM allow policies (#1879): whether a principal holds a permission on a resource, and why, for `--iam-enforcement`, Policy Troubleshooter and Policy Analyzer. |
| `internal/grafeas/` | Container Analysis (#1912): the Grafeas v1 API's notes and occurrences, their AIP-160 filters and IAM, and the vulnerability summary, persisted in the instance's state directory. |
| `internal/iampolicy/` | IAM policy storage without enforcement (ADR-0006). |
| `internal/iamroles/` | IAM's permission catalog and the permissions of the roles CloudBurrow's permission checks evaluate (#1743). |
| `internal/filestoreserver/` | Filestore's NFS servers (#2192): an NFS-Ganesha image built locally from `image/` (Debian's pinned packages on `debian:trixie-slim`), one privileged container per instance on the `kind` Docker network at a fixed address, its share on a Docker volume labelled `io.cloudburrow.filestore` (kept by `stop` in persistent mode, removed by `delete`); snapshots copied beside the share, backups as tar files in the instance's state directory. |
| `internal/dockergc/` | The `io.cloudburrow.instance` label on the images and volumes CloudBurrow makes in the host's Docker, their removal on `stop` and `delete`, and `cloudburrow gc`'s listing of what older builds left (#2152). |
| `internal/images/` | Gets locally built images into the cluster without a registry. |
| `internal/k8s/` | The one kubectl Runner (#599): kubeconfig, context and namespace fixed at construction, typed not-found and forbidden errors, the ownership labels, long-running port-forwards, streamed exec and log following, and an interactive exec on a pseudo-terminal for the console's terminal (#781). Every kubectl CloudBurrow runs outside `internal/cluster` goes through it: Secret Manager, Cloud KMS, the Cloud Run adapter, `internal/netfwd`, `internal/images`, `internal/components`, and in `cmd/cloudburrow` the console's reads and log follower, `logs`, `diagnose`, the Cloud SQL snapshots and the cluster-host Service; and `internal/terminal`. |
| `internal/lifecycle/` | Startup ordering, readiness, bounded shutdown, ownership of background workers, and the narrow interfaces by which one service reaches another. |
| `internal/localai/` | Acquisition of local AI model artifacts, kept apart from their execution. |
| `internal/isoconformance/` | Test-only (#1754): fetches ISO/IEC 14496-26 audio conformance streams and reference decodes at test time, by HTTP range requests out of ISO's zips, into the user's cache directory, pinned by SHA-256 and ISO's MD5 (`dependencies.json`); only with `CLOUDBURROW_TEST_DOWNLOADS` set, else the test skips. ISO's files are never committed. |
| `internal/hfmodels/` | Hugging Face for Model Garden: the anonymous Hub API's search (task, library, licence filters, sorts, cursor pages) and model cards with a sanitised README (#2148); the check that an end user's token opens a gated model; and host downloads (#1840): model info (licence, gated status, revision, files), a free-disk floor, SHA-256-checked download with progress, delete. |
| `internal/hostai/` | The host runtimes for the Apple GPU (#2155): whether this machine has Metal (macOS on Apple silicon) and its chip; llama.cpp's official macOS arm64 archive, downloaded once, checked against its pinned SHA-256 and unpacked with no path leaving the tree; mlx-lm's virtual environment from the embedded hash lock (uv, or an official Python); and a supervisor that runs a server in its own process group on 127.0.0.1 with health checks, restart with back-off, its output as log lines, a pid file that `Reap` uses to stop servers a crashed instance left, and stop on undeploy and `cloudburrow stop`. Also finds the macOS arm64 `cloudburrow_generate` (#2159, `LiteRTLMRunner`: `$CLOUDBURROW_LITERT_LM_DIR`, `litert-lm/` beside the CLI, or Homebrew's `libexec/cloudburrow/litert-lm`) that `cmd/cloudburrow` runs on LiteRT-LM's GPU backend for Gemini generation. Used by `internal/modelgarden/host.go` and `cmd/cloudburrow/localaimetal.go`. |
| `internal/modelgarden/` | ModelGardenService.Deploy's Hugging Face deployments (#1840, #2148): the Endpoint, Model and DeployedModel records (`<state-dir>/modelgarden/deployments.json`), the engine Model Garden picks per task and library (vLLM CPU, Text Embeddings Inference CPU), run as a Deployment and Service on the instance's cluster (the pod pulls the weights from the Hub; a gated model's token is a Secret) reached through a netfwd tunnel, or with `docker run` beside the cluster for a model downloaded to the host, or, for a GPU machine spec on a Mac, as a host process on the Apple GPU (`host.go`, #2155: llama.cpp or mlx-lm on Metal, published to the cluster as a selector-less Service and EndpointSlice at the host), and the translation of Vertex's predict bodies to the engines' routes. Served by `internal/service/vertexai/modelgarden.go`. |
| `internal/egress/` | The outbound dispatchers' local-only guard (#2015): a `*.googleapis.com` URL goes to this instance's endpoint of a served API and every other Google host is refused, before any lookup and on every redirect; `egresstest/` is the loopback-only sink their tests dial through. |
| `internal/localhost/` | Dials any name under `.localhost` on loopback without a DNS lookup, for the dispatchers the CLI runs (#714). |
| `internal/locations/` | The `google.cloud.location.Locations` mixin Cloud KMS and Cloud Scheduler serve (#1237, #1291), and the regions and zones services share without importing Compute Engine. |
| `internal/logfilter/` | Cloud Logging's query language (#2026): one parser and evaluator for every Logging filter (entries.list, tail, sinks, exclusions, log views, log-based metrics, CopyLogEntries) and the console's Logs Explorer. |
| `internal/lro/` | Long-running operations: pending, completed and failed. |
| `internal/marketplace/` | The console's Marketplace (#2149): catalogue sources behind one interface (search, product, versions, Configure parameters, deploy plan), Docker Hub's public anonymous search, repository, tag and registry-config reads, Helm charts from Artifact Hub, Rancher's and user-added repositories' `index.yaml` (installed, upgraded and uninstalled with the Helm SDK), and a plan's deploy through the `internal/k8s` runner as an `app.k8s.io/v1beta1` Application with its owned components; the Applications' list, page, status and delete. `marketplacetest` serves Docker Hub's and Artifact Hub's recorded answers, a small chart and its repository, an in-memory kubectl and a Helm over it for the tests. |
| `internal/metadata/` | A local GCE metadata server and the fixture credentials that point Google tooling at CloudBurrow. |
| `internal/metrics/` | Counts the calls CloudBurrow serves itself, in the Prometheus text format (#292). |
| `internal/netfwd/` | Publishes in-cluster Services on host loopback addresses (port-forward tunnels), with the Host check in front of the tunnels that carry HTTP (#725). |
| `internal/orgpolicy/` | Evaluates organization policies where the services ask (#1890): a constraint's effective policy down the hierarchy, the checks each enforced list or boolean constraint makes, and custom constraints' CEL conditions on the fields each service documents (#1891, #1895). |
| `internal/paging/` | Pagination with deterministic ordering; invalid page tokens refused. |
| `internal/prediction/` | The Vertex AI custom prediction container contract. |
| `internal/prefetch/` | Offline cache of the node, backend and Knative artifacts a first `up` downloads (#604). |
| `internal/bigtablefront/` | The front the Bigtable pod runs before Google's emulator (`cloudburrow-storage bigtable-front`): serves the fifteen RPCs the emulator has no implementation of and would panic on, losing every table (backups, schema bundles, `UndeleteTable`, change streams from a change log it keeps of the writes it decodes, #1523), and passes every other call through undecoded (#1206); serves the emulator's readiness path on the same port. |
| `internal/callfront/` | The call front the Spanner, Firestore and Datastore pods run before Google's emulators (`cloudburrow-storage call-front`, #1288): serves the Service's port, the emulator on the pod's loopback behind it, passes every request through unread (`netfwd.PassThrough`: gRPC over h2c, REST over HTTP/1.1) and keeps each call in an `internal/calllog` ring, so the CLI measures every client's calls, a pod's included. |
| `internal/firestoreadmin/` | The Firestore Admin API (#1331), which the Firestore emulator does not serve: the Firestore pod's call front (`--firestore-admin`) answers `google.firestore.admin.v1.FirestoreAdmin` and `google.longrunning.Operations` over gRPC, and their REST paths, itself, and passes the documents API to the emulator. Databases, composite indexes, field overrides and backup schedules (#1461, which take no backups) are kept in memory, as the emulator keeps its documents; operations are returned done; deleting a database clears its documents through the emulator's reset endpoint. Export, import, bulk delete, backups, restores, clones and user credentials answer `UNIMPLEMENTED`, naming why. The front also counts the documents each call reads, writes and deletes and each query's executions (`usage.go`, #2048), which the CLI serves as Cloud Monitoring's Firestore metrics and the console's Usage and Query insights, and sends server client libraries' calls as the emulator's owner, so the rules the Firebase Rules API releases bind Firebase client SDK calls alone (`clientauth.go`). |
| `internal/queryfront/` | The query RPCs Google's emulators do not serve (#1405), answered in the Datastore and Firestore pods' call front (`--queries datastore`, `--queries firestore`) from calls they do serve, over gRPC and REST: Datastore `RunAggregationQuery` (COUNT with `up_to`, SUM, AVG) computed over the nested query run through the emulator's `RunQuery`, every batch paged through; Firestore `PartitionQuery` as evenly spaced cursors at document references from the collection group's names. A GQL aggregation (#1461) is answered the same way over the nested query the emulator parses; explain options answer `UNIMPLEMENTED`. |
| `internal/s2ops/` | The S2 operations github.com/golang/geo does not port, for GoogleSQL for Bigtable's `ST_UNION`, `ST_UNION_AGG`, `ST_INTERSECTION`, `ST_DIFFERENCE`, `ST_BUFFER`, `ST_BUFFERWITHTOLERANCE` and `ST_SIMPLIFY` (#1728, #1735): Go ports of the S2 C++ library's `S2BooleanOperation`, `S2Builder` with its snapping and edge-chain simplification, `S2Builder::Graph`, the `S2PolygonLayer`, `S2PolylineVectorLayer`, `S2PointVectorLayer` and `ClosedSetNormalizer` layers, `S2WindingOperation` and `S2BufferOperation`, with S2's unit tests (Apache-2.0; its PROVENANCE.md). |
| `internal/spannerfront/` | The Spanner instance updates the emulator refuses (#1461), answered in the Spanner pod's call front (`--spanner-instances`) over gRPC and the admin REST port: `UpdateInstance` of the display name, compute capacity and labels, kept in memory and laid over the emulator's `GetInstance` and `ListInstances`, its done operation answered by `GetOperation`; `UpdateDatabase` of a database's drop protection, enforced on `DropDatabase` and `DeleteInstance`. The admin methods the emulator refuses with an empty message (backups, backup schedules, instance configurations, `MoveInstance`, …) are refused `UNIMPLEMENTED` by the front, naming why. |
| `internal/pubsubfront/` | The front the Pub/Sub pod runs before Google's emulator (`cloudburrow-storage pubsub-front`), for gRPC and the REST API on one port: passes every call through, and enforces subscription expiration, which the emulator stores and never acts on (#873); relays the emulator's pushes so a successful one counts as activity, refuses exactly-once delivery on a push subscription, and reads and restores the expiration clocks and the projects named for `cloudburrow state` (#880), over REST as over gRPC (#908), routing each connection by its first request, gRPC to grpc-go's own transport and anything else to net/http, which routes each request by its content type, so REST over h2c is served (#909, #950); applies the labels updates the emulator refuses (#949); keeps what the emulator does not store in a file on the pod's emptyDir, so a restart of the front alone loses nothing (#898). |
| `internal/osvscan/` | Artifact Registry's vulnerability scanning (#1912): the pinned OSV-Scanner and OSV database snapshot (prefetched from a release's mirror, else upstream, SHA-256 checked, #1917), the offline scan of an image archive, and the conversion of its findings into Artifact Analysis's occurrences. |
| `internal/ratequota/` | Per-project, per-minute request quotas (#2063): each service's documented metrics and defaults, the configured and admin-set limits, and Google's `RESOURCE_EXHAUSTED` (`RATE_LIMIT_EXCEEDED`) refusal. |
| `internal/registry/` | Artifact Registry's Docker registry: the Docker Registry HTTP API v2 over a content-addressed store in the instance's state directory (#1383). |
| `internal/resource/` | Google resource-name parsing and formatting, project and location scoping. |
| `internal/rpmfixture/` | Builds small, installable `.rpm` packages in Go for tests (#1539): a signature header with the header and payload digests, an immutable-region header with files, dependencies and changelog, and a gzip cpio payload; rpm and dnf install them with `gpgcheck=0`. |
| `internal/sched/` | Cancellable background work, due-time scheduling, retry and backoff over an injected clock. |
| `internal/service/kms/` | Cloud KMS, built by us (#309). |
| `internal/service/kmsinventory/` | The Cloud KMS Inventory API (#2053): key usage from the products' CMEK resources. |
| `internal/locations/` | The `google.cloud.location.Locations` mixin's gRPC registration, shared by Cloud Scheduler and Cloud KMS (#1237, #1291), and the regions and zones services share without importing Compute Engine (Cloud SQL). |
| `internal/service/logging/` | Cloud Logging write and read (`LoggingServiceV2`), built by us (#304). |
| `internal/service/resourcemanager/` | The project registry, and the Resource Manager v3 and v1 Projects APIs over it (#298, #301). |
| `internal/service/redisadmin/` | The Memorystore for Redis admin API, built by us over the running Valkey server and a server per created instance (Redis 6.2 or 7.0, or Valkey, by its `redis_version`, #1386), which `cmd/cloudburrow` provisions in the cluster (#1302); the Memorystore for Valkey and Redis Cluster APIs too (#1519). |
| `internal/rediscmek/` | `cloudburrow-storage memorystore-cmek` (#2113): seals a Memorystore for Redis server's RDB and AOF files with the instance's data key (AES-256-GCM), unwrapped by Cloud KMS `Decrypt`: the command of a CMEK instance's, Redis cluster's or Valkey instance's server container, copied in from the storage image (`components.Sealed`), the server's own directory a memory volume and its persistent volume holding the sealed copies alone. |
| `internal/redistlsfront/` | The TLS endpoint of a Memorystore for Redis server before Redis 6.0 with in-transit encryption (#1768): `cloudburrow-storage redis-tls-front`, a second container in the server's pod, serving TLS 1.2 or later on port 6378 with the instance CA's server certificate while the server listens on the pod's loopback alone; a Standard tier replica replicates through a loopback relay the front forwards over TLS to its primary. |
| `internal/service/scheduler/` | Cloud Scheduler, built by us (#302). |
| `internal/service/secrets/` | Secret Manager v1, built by us; with a cluster, versions are kept as Kubernetes Secrets in the workload namespace. |
| `internal/service/backupdr/` | The Backup and DR Service API v1 for Cloud SQL (vaults, plans, associations, data sources, backups over Cloud SQL backup runs), built by us (#1433). |
| `internal/service/storagejobs/` | The Cloud Storage companion APIs, built by us over the instance's buckets: Storage Batch Operations jobs, Storage Insights inventory reports and Storage Transfer Service jobs with local agent pools, on one shared clock (#1957). |
| `internal/service/datatransfer/` | The BigQuery Data Transfer Service v1 (scheduled queries and dataset copies: transfer configs, runs, logs, BigQuery's schedule syntax and table templates), built by us (#1948); runs execute on the instance's BigQuery through an `Executor` `cmd/cloudburrow` wires. |
| `internal/service/billing/` | Cloud Billing API v1 (local billing accounts and subaccounts, move, projects' billing info whose unlink disables the project's billable APIs, the billing account IAM policy, a Cloud Catalog of the served APIs at a local price of 0) and the Budget API v1 (budgets with thresholds, filters and Pub/Sub notification rules, actual spend 0), built by us (#2109). |
| `internal/service/cloudquotas/` | The Cloud Quotas API v1 (quota infos of the APIs CloudBurrow serves, quota preferences granted at once with Google's safety checks, quota adjuster settings), built by us; the source `internal/quotalimit` reads (#2030). |
| `internal/service/siteverification/` | The Site Verification API v1 (FILE and META tokens checked against sites on this machine; verified sites and their owners), built by us (#2030). |
| `internal/service/apikeys/` | The API Keys API v2 (each project's API keys and their restrictions), built by us; `Authorize` is Google's decision on a request that carries a key, with its API_KEY_* errors (#1915). |
| `internal/service/firebaserules/` | The Firebase Rules API v1 (#2048), built by us over the Firestore emulator: rulesets compiled by the emulator, a `cloud.firestore` release set as the project's rules there (`PUT /emulator/v1/projects/{p}:securityRules`), and `projects.test` evaluated by the emulator on a scratch project with unsigned Firebase ID tokens. |
| `internal/service/gke/` | The Kubernetes Engine API v1 (clusters, node pools, operations), built by us (#1913); each cluster is a kind cluster that `internal/cluster`'s `GKE` makes and resizes. |
| `internal/service/analyticshub/` | BigQuery sharing, formerly Analytics Hub, v1 (data exchanges, listings, subscriptions, their IAM policies), built by us (#1960); a subscription's linked dataset is made in the instance's BigQuery through a `Linker` `cmd/cloudburrow` wires, and the BigQuery front (`internal/bigqueryfront/hublinks.go`) keeps it a read-only pointer with the listing's egress restrictions. |
| `internal/service/compute/` | The Compute Engine API v1's VPC networking (networks, subnetworks, firewall rules, routes, addresses, operations, regions and zones), built by us (#1936); records other services check their network fields against, no traffic. |
| `internal/service/workflows/` | Workflows and Workflow Executions v1, with an interpreter of Google's workflow syntax; executions call this instance's endpoints only, built by us (#1986). |
| `internal/service/dataform/` | Dataform v1 and v1beta1 (repositories, workspaces and their git and branches, compilation results built with Google's Dataform core in a Node container, release and workflow configs and their table update triggers, invocations run on the instance's BigQuery, folders and team folders), built by us (#2105). |
| `internal/service/parametermanager/` | Parameter Manager v1 (parameters and versions, global and regional; formats checked; `RenderParameterVersion` resolves `__REF__` secret references through the instance's Secret Manager as the parameter's own identity), built by us (#2054). |
| `internal/service/apphub/` | App Hub v1 (service project attachments and boundaries, discovered services and workloads, global and regional applications with their attributes, registered services and workloads, extended metadata schemas), built by us (#2110); it discovers what `cmd/cloudburrow` hands it: Cloud Run services and jobs and Kubernetes Engine clusters. |
| `internal/service/filestore/` | Filestore v1 (instances in the documented tiers with their capacity, share, protocol, network and export option rules; snapshots, revert; regional backups and restore, in place for basic tiers; operations and locations), built by us (#2192); each instance's NFS server is a `Backend` (`internal/filestoreserver` in the CLI, `MemoryBackend` in tests). |
| `internal/service/trace/` | Cloud Trace (`cloudtrace.googleapis.com` v1 and v2: span writes, trace reads with the v1 filter language) and the Telemetry API's OTLP trace ingestion (OTLP/gRPC and HTTP), on one port, built by us (#2118). |
| `internal/service/privateca/` | Certificate Authority Service v1 (CA pools, CAs, certificates, real X.509 signed in process), built by us (#1834). |
| `internal/service/monitoring/` | Cloud Monitoring API v3 MetricService (metric and monitored resource descriptors, time series with alignment and reduction, log-based and request metrics), built by us (#1939). |
| `internal/service/networkconnectivity/` | The Network Connectivity API v1's service connectivity automation (#2141): service connection policies, maps, classes and tokens, the endpoints a map's consumer configs get through Compute Engine's forwarding rules, and Cloud SQL's automatic endpoints (`pscAutoConnections`) as a hidden map of its own; served on the Compute Engine port over gRPC and JSON. Hubs, policy-based routes, internal ranges and data transfer answer UNIMPLEMENTED. |
| `internal/service/serviceusage/` | The Service Usage API v1 (which APIs each project has enabled), built by us; enabling an API not served is refused, and a disabled API's calls are refused SERVICE_DISABLED, through `internal/apienable` (#1352, #1914). |
| `internal/service/sqladmin/` | The Cloud SQL Admin API (sqladmin v1 and v1beta4) over the PostgreSQL and MySQL servers, built by us (#1301). |
| `internal/service/storage/` | Cloud Storage, built by us to Google's spec (#485), the only storage backend (#519). |
| `internal/service/tasks/` | Cloud Tasks queues, tasks and HTTP dispatch, built by us. |
| `internal/service/vertexai/` | The subset of Vertex AI `generateContent` the local runtime can perform. |
| `internal/storageimage/` | Builds the in-cluster Cloud Storage image from the embedded `cmd/cloudburrow-storage` binaries (#514), and says which architectures a CLI embeds (#686). |
| `internal/storageimage/storageimagetest/` | Tests only: stand-in embedded binaries, present, missing or placeholder (#686). |
| `internal/storageserver/` | Runs the builtin Cloud Storage server as a process. |
| `internal/supervisor/` | `cloudburrow-storage supervise`: runs a backend's process inside its container and starts it again within a second when it ends or the front beside it fails its liveness path, so Kubernetes' restart back-off never applies (the BigQuery emulator, #1091); `install-self` copies the binary in from an init container. |
| `internal/store/` | Resource metadata storage: in-memory and durable modes, atomic multi-key commits, single-instance ownership of a data directory. |
| `internal/sysparams/` | Google's REST system parameters (#1954): the `fields` / `X-Goog-FieldMask` / `x-goog-fieldmask` mask parser and pruners, `prettyPrint`, `$.xgafv`, JSONP `callback`, and `Wrap` for the hand-written JSON fronts. |
| `internal/telemetry/` | OpenTelemetry traces of the requests CloudBurrow serves itself (#313). |
| `internal/terminal/` | The console's Cloud Shell-style terminal (#781): the pinned Cloud SDK pod in the instance's namespace, with the instance's pod environment and gcloud configuration and read-only kubectl, and the interactive `kubectl exec` into it, through the `internal/k8s` runner. |
| `internal/transport/grpc/` | The gRPC server CloudBurrow's own services run on: one listener, shared interceptors and message-size limit. |
| `internal/transport/rest/` | Shared HTTP plumbing for CloudBurrow's own REST surfaces: routing, size limits, JSON, Google-style error bodies. |
| `internal/trust/` | Which directories' configuration and hooks the developer has agreed to run (#598). |
| `internal/version/` | Build identification, injected at link time. |
| `tools/bqengine/` | Builds the Linux BigQuery emulator the CLI embeds from `third_party/bigquery-emulator`: pinned, checksummed modules with CloudBurrow's patches applied strictly (#1061). |
| `third_party/bigquery-emulator/` | The pins (a Go module of its own), patches, licences and provenance of the BigQuery emulator CloudBurrow builds (#1061); no upstream source is committed. |
| `tools/coverage/` | Generates per-service API coverage from the proto surface (#288); `make docs-check` fails when it is stale. |
| `tools/depcheck/` | Reports every `dependencies.json` component as candidate, current, skipped (with the reason) or unreachable; discovery only (#703). |
| `tools/docsmap/` | Fails `make docs-check` when this module map drifts from the tree, or a Console subject in compatibility.md is both Verified and Not supported (#588). |
| `tools/doclinks/` | Fails `make docs-check` on a relative Markdown link to a path that does not exist (#520). |
| `tools/osvmirror/` | Keeps vulnerability scanning prefetchable after OSV drops a pinned database generation (#1917): `stage` builds a release's mirror of the scanner and the pinned snapshots (release.yml), `fetch` prefetches them into a cache (CI's run shard), `bump` moves the pins to OSV's live generations (osv-pins.yml). |
| `test/compat/` | Official Go SDK compatibility tests against a running instance (tag `compat`). |
| `test/compat-python/` | Official Python client suite (`make compat-python`). |
| `test/compat-node/` | Official Node.js client suite (`make compat-node`), run in compat CI's `storage` and `emulators` shards (#589). |
| `test/k8s/` | Native Kubernetes, Helm and Knative portability (tag `integration`). |
| `test/e2e/` | The acceptance workflow of §1.2 (tag `e2e`). |
| `test/upstream/` | Probes measuring third-party components (tag `upstream`). |
| `test/oracle/` | Differential oracles: Google's fakekms and storage-testbench (tag `oracle`). |
| `test/install/` | `scripts/install.sh`, the formula renderer and the release workflow, against a fake release. |
| `test/actionselftest/` | Runs after the setup-cloudburrow action, with only the environment it exported (#282). |
| `test/localai/` | Local generation against the real runtime and a real model (tag `localai`). |
| `test/repo/` | Checks over the repository's own files. |
| `test/tools/` | Test-only helpers (`pubsubfake`), never shipped. |

Rules:

1. **No service or adapter package depends on another's.** A package under
   `internal/service/` or `internal/adapter/` does not import, directly or transitively,
   another one there. A cross-service need goes through a narrow interface declared by the
   consumer and wired in `internal/lifecycle` or `cmd/cloudburrow`; the Cloud Run adapter's
   `SecretResolver` is the pattern. There is no exception: the retry schedule Cloud Scheduler
   shared with Cloud Tasks moved into `internal/sched` (#672).
2. **Only `internal/cluster` and `internal/k8s` exec kubectl.** ADR-0005 makes some packages
   Kubernetes-aware by design, and that is not a violation: `internal/adapter/run` translates
   Cloud Run into Knative Serving and batch Jobs, `internal/components` installs the in-cluster
   backends and Knative, and `internal/service/secrets` keeps versions as Kubernetes Secrets so
   Cloud Run revisions can reference them (Cloud KMS keeps its keys the same way). They may build Kubernetes objects, but reach
   the cluster through the one runner in `internal/k8s` (#599) instead of their own.
   Other service packages speak to endpoints, not to pods. The port-forward tunnels
   (`internal/netfwd`), image loading (`internal/images`) and the component installer
   (`internal/components`) use the same runner, and so does `cmd/cloudburrow`: the console's
   reads and log follower, `logs`, the Cloud SQL snapshots and the cluster-host Service. No
   package outside `internal/cluster` and `internal/k8s` execs kubectl, and
   `internal/archtest` fails on one that does; its allow-list is empty.
3. **`cmd/` borrows no service's kubectl helper.** Wiring in `cmd/cloudburrow` does not take
   its kubectl runner from a service or adapter package, and does not exec kubectl itself: it
   builds an `internal/k8s` runner for its own calls and hands one to each service (#599).
4. **Pure Go units must be testable without a cluster.** Config, resource names, error
   mapping, paging and scheduling have unit tests that never touch Kubernetes. A change that
   makes them require a cluster is a design regression.
5. **`cmd/cloudburrow` holds wiring and CLI behaviour, and tests it.** It is not a thin
   dispatcher: it builds every in-process service and implements the subcommands, and it is
   tested in the package (`go test ./cmd/cloudburrow`; the few tests that need a cluster are
   behind the `integration` tag). Logic that is not about the CLI or about
   wiring belongs in `internal/`.

Rules 1–3 are checked by `internal/archtest` (#672), each against an explicit list of today's
exceptions that fails when a new one appears or a listed one is gone; rules 4 and 5 are
enforced by review. `make docs-check` checks the table above against the tree
(`tools/docsmap`, #588): every listed path exists, every package under `internal/` is listed,
and a **Planned** row fails once its package lands.

---

## 4. Endpoints and SDK configuration

Separation of surfaces is unchanged in spirit from
[ADR-0002](adr/0002-transport-and-routing.md) — it now comes from distinct Kubernetes
Services rather than a fixed host port map.

Each backend is reached on the host through a stable local address that the CLI reports after
startup. Ports are not fixed constants: they are allocated and reported, which is what makes
parallel instances possible.

### 4.1 Client configuration is per-language and per-service

Verified against client source in #1 and unchanged by this revision:

| Client | Service | Mechanism | Notes |
|---|---|---|---|
| Go | Storage | `STORAGE_EMULATOR_HOST` | Scheme optional; client appends `storage/v1/`. |
| Go | Storage (gRPC) | `STORAGE_EMULATOR_HOST_GRPC` | Bare host:port. CloudBurrow serves gRPC as h2c on the HTTP endpoint's port (#1321), so it is the same address. |
| Go / Python | Pub/Sub | `PUBSUB_EMULATOR_HOST` | Sets endpoint, insecure transport, disables auth. |
| Python | Storage | `STORAGE_EMULATOR_HOST` | **Scheme required** — value used verbatim. |
| Go / Python | Cloud Tasks | **None exists** | Explicit endpoint in client options. |
| Go / Python | Cloud Run | **None exists** | Explicit endpoint in client options. |
| Node | All | See [compatibility.md](compatibility.md#nodejs-client-libraries) | Tested by `test/compat-node` (#589); [credentials.md](credentials.md#nodejs-clients) gives each client's mechanism. |
| Java | All | **Untested, no plan** | No Java test exists and none is planned (#718). CloudBurrow makes no Java claim and documents no Java mechanism. |

Two consequences that must appear in user documentation, not as footnotes: Go and Python
disagree on whether `STORAGE_EMULATOR_HOST` includes a scheme (publish the form with a
scheme, which both accept), and **Cloud Tasks and Cloud Run cannot be redirected by
environment variable at all**.

### 4.2 Addressing

- **Host → service:** the reported local address.
- **In-cluster → service:** the Kubernetes Service DNS name. Workloads deployed into the
  cluster use this form, which differs from the host form.
- **Adapter → workload:** the Knative-assigned URL, discovered after readiness, never assumed.

Environment injected into application workloads uses the in-cluster form. The acceptance
workflow exercises both directions.

**A backend's advertised address is part of its configuration, not an afterthought.** Verified
the hard way (#26): the official storage client *follows* `mediaLink` on download, so a server
that advertises one fixed address serves one audience, and the upstream emulator used until #519
needed a second Deployment for in-cluster reads. CloudBurrow's own storage server builds
`mediaLink` and `selfLink` from each request's `Host`, so one Deployment answers the host,
through the tunnel, and the cluster, by its Service name (#514).

### 4.3 Local images must bypass tag resolution

Knative resolves image tags to digests by contacting the registry. A locally built image
loaded straight into the cluster has no registry, so the revision fails with
`failed to resolve image to digest: ... 401 Unauthorized`.

Images intended for local execution therefore use a registry prefix Knative skips
(`dev.local/`, `ko.local/`, `kind.local/`), or CloudBurrow runs a local registry. Verified:
the identical image failed as `cloudburrow-worker:verify` and succeeded as
`dev.local/cloudburrow-worker:verify`. Implemented in #26.

---

## 5. Cluster ownership and safety

A tool that creates and deletes clusters can destroy work that is not its own.

- CloudBurrow acts **only on clusters it created**, identified by name prefix and labels.
- It **never changes the global current kubecontext.** Every operation uses an explicit
  kubeconfig and context. A developer's `kubectl` behaves identically before and after.
- It **never mutates or deletes clusters, namespaces or resources it does not own.**
- **Cluster-node privileges are not application-pod privileges.** The kind node container is
  necessarily privileged; application pods are not, and receive no host mounts and no Docker
  socket by default.
- **Destructive operations are explicit and named.** `stop` does not delete. `reset` destroys
  state. `delete` destroys the cluster. None of the three implies another.

---

## 6. Lifecycle, state and persistence

| Operation | Meaning |
|---|---|
| `up` | Create the cluster if absent, install components, wait for readiness, report endpoints. |
| `status` | Report actual component readiness and resolved endpoints. |
| `stop` | Stop the cluster without destroying it. State that a backend persists survives. |
| `reset` | Destroy all CloudBurrow-managed state, keeping the cluster. Cancels work *before* deleting state. |
| `delete` | Destroy the cluster CloudBurrow created. |

**Persistence is per backend and must be reported truthfully.** This is not a formality:

- **Pub/Sub does not persist.** The audit measured a topic `NotFound` after restart *even
  with `--data-dir`*. We cannot inherit durability the backend lacks, and the CLI and
  compatibility matrix say so plainly rather than implying otherwise.
- **Cloud Storage persists** in persistent mode, in the server's own store on a PVC, and starts
  empty in ephemeral mode whatever is on disk (#512) — both measured across a restart.
- **Cloud Tasks** is ours; its persistence is our decision (#15/#16).

Readiness reflects what actually initialised. A component whose mandatory startup failed is
never reported ready.

`up` starts what does not depend on each other at the same time (#1588): the storage and
BigQuery images load together, every tunnel binds at once, and Knative installs in the
background from the moment the cluster answers, while the backends roll out; the `knative`
component then waits for it. `kind create` returns once the API server is up, without
`--wait`: the node becomes Ready while the images load and Knative's manifests apply, and the
`cluster-nodes` component waits for it, as `--wait` did, before the backends are scheduled.
`up` is ready, and `wait` returns, only once every one of them is.

---

## 7. Support matrix: three separate things

Conflating these is how a tool ends up over-promising. They are tracked separately in
[`compatibility.md`](compatibility.md):

1. **GCP API compatibility** — does an official SDK call behave correctly? Promoted only by an
   SDK-driven test.
2. **Native Kubernetes and Helm portability** — do ordinary manifests, charts and operators
   work? Tested by #29, independently of any GCP API.
3. **Knative feature coverage** — which Cloud Run v2 behaviors does the adapter actually map?
   Revision traffic and scaling are tested in #30.

**Knative is not Cloud Run.** It is the closest available model. No blanket claim is made
that it reproduces Cloud Run semantics, and untested behavior is not claimed at all.

---

## 8. Dependencies and reproducibility

[`dependencies.json`](../dependencies.json) is the single inventory. Every component records
its source, version, immutable identity, license, redistribution terms, update feed and
verification mechanism.

- **Verified mode** — the pinned set. All ordinary, release and offline builds use it, and
  need no network.
- **Latest-candidate mode** — discovery only. The scheduled Dependencies workflow runs
  `tools/depcheck`, which lists newer GitHub releases in the job summary; no CI job runs a
  suite against a candidate. A candidate is tested only when a reviewed pull request moves
  the pin, and that pull request's ordinary CI is the test. Never used for release artifacts
  until promoted into the verified set.

**A mutable tag is for discovery only, never a release pin.** An entry whose digest is `null`
is not yet reproducible, and the inventory lists those explicitly rather than implying
otherwise. Pulled images carry a digest and release YAMLs a per-file `manifests` hash; `up`
downloads each Knative manifest, checks its sha256 against the pin and applies it from stdin,
refusing one that differs (#597). `TestPinsMatchTheInventory` fails when a Go constant and the
inventory disagree. `TestDockerfilesPinTheInventory` does the same for the repository's
Dockerfiles: every `FROM` carries an inventory digest, and every download is checked against an
inventory checksum (#687).

Reference combination — **stood up and verified end to end on 2026-09-20** (see
[the 2026-09-20 record](https://github.com/cloudburrow/cloudburrow/blob/15762298ef4e164275ce769e87d5823649aa0758/docs/local-verification.md); the current macOS run is in
[`docs/local-verification.md`](local-verification.md)):

| Component | Version | Verified |
|---|---|---|
| kind | v0.33.0 | Cluster ready in 37.6 s |
| Kubernetes (`kindest/node`) | v1.36.4 | Server reports v1.36.4 |
| Knative Serving | v1.23.0 | All 4 deployments Available; no version complaint |
| net-kourier | v1.23.0 | Knative Service served HTTP 200 from the host |

Knative Serving v1.23.0 enforces `DefaultKubernetesMinVersion = "v1.34.0"`; upstream states no
maximum, so none is assumed. v1.36.4 satisfies it, confirmed by the controller starting
without the version check firing.

**Measured resource budget** for the full stack (kind + Knative + Kourier + both emulator
backends + one workload, 19 pods): **1.5 GiB** resident in the node container, **2125 m CPU
and 1370 Mi** in aggregate pod requests. That is the real floor a developer pays, and it is
substantially heavier than the superseded single-process design.

**Redistribution is not the same as licensing.** Google's Pub/Sub emulator is **not
established as open source** — the Apache-2.0 LICENSE inside its JAR belongs to bundled
dependencies, and its own classes have no published source. CloudBurrow therefore runs the
official digest-pinned image or directs the user to `gcloud components install`, and does not
vendor the binary.

---

## 9. Testing rules

- **Owned scheduling logic keeps an injected clock.** Retry, backoff and due-time behavior are
  tested deterministically by advancing virtual time.
- **External components get bounded polling.** Readiness and events are awaited with explicit
  deadlines, because another process's clock cannot be advanced. This replaces the earlier
  blanket prohibition on sleeping, which was written when every component was ours.
- **Unit tests must not require a cluster.** Cluster-dependent tests are tagged and separate.
- **An operation is supported only when an official SDK drives it.** Unchanged, and the reason
  `compatibility.md` exists.

---

## 10. Open questions

1. **Cloud Tasks has no upstream** — concluded from not finding one, which is weaker than the
   other audit findings. Re-tested before #15.
2. **Pub/Sub non-persistence**: surface honestly as memory-only, or reconstruct state
   CloudBurrow-side? Decided in #27.
3. **Knative + Kubernetes v1.36.4 is pinned but unverified**; #28 is the gate.
4. **Resource budget** for the full stack is measured in #28, not projected.
