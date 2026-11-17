# Upstream reuse evaluation

Issue: #24 · Date of measurements: 2026-09-20 · Status: complete, pending review

Reuse is the default. CloudBurrow owns the local developer experience, integration, and
compatibility behavior that is genuinely missing upstream. It does not rewrite working
components to keep everything in one language.

Every number below was measured on the machine described in
[§7](#7-measurement-environment-and-reproduction), not estimated. The probes that produced
them are committed in [`test/upstream/`](../test/upstream/) and re-runnable with
`make test-upstream`.

---

## 1. Decisions

| MVP service | Decision | Component | Why |
|---|---|---|---|
| **Pub/Sub** | **Integrate** | Google `cloud-pubsub-emulator` 0.8.35 | Google's own implementation passes every data-plane probe, including StreamingPull. Rewriting it would be unjustified. |
| **Cloud Storage** | **Build to spec** ([amendment](#amendment-cloud-storage-is-built-not-reused-485)) | `fsouza/fake-gcs-server` v1.56.1 until the cut-over (#519); Google `storage-testbench` as a CI-only oracle | No official Google GCS emulator exists. fake-gcs-server was adopted first, but it drops bucket fields with HTTP 200, never advances metageneration and refuses versioning in persistent mode; storage-testbench is unsupported, memory-only and delivers no notifications. Reuse would mean owning a fork. |
| **Cloud Tasks** | **Build** | — | No official emulator and no viable community implementation found. This is a demonstrated gap. |
| **Cloud Run** | **Integrate via adapter** | Knative Serving v1.23.0 | Workload execution comes from Kubernetes/Knative; CloudBurrow supplies the Cloud Run v2 API adapter. |
| **Cloud KMS** | **Build** | none viable | No Google emulator serves the KMS API, and every third-party one departs from Google's documented behaviour in ways a user would inherit. See [the amendment](#amendment-cloud-kms-is-built-not-reused-309). |
| **Secret Manager**, **Cloud Scheduler**, **Cloud Logging**, **Resource Manager** | **Build** | none reused | No Google emulator for any of them. Each shares state with a piece CloudBurrow already owns, which a separate emulator process could not. Recorded after the fact, without probes; see [the amendment](#amendment-secret-manager-cloud-scheduler-cloud-logging-and-resource-manager-are-built-not-reused-588). |

The rule applied throughout: **replacing a viable upstream requires a specific unmet
requirement plus measured evidence.** "It is written in Java" and "its clock cannot be
injected" are not grounds for rejection — they are integration constraints, handled in
[§6](#6-adapter-interfaces).

---

## 2. What official Google emulators actually exist

From `gcloud components list` (SDK 585.0.0):

| Component | Download size | In MVP scope? |
|---|---|---|
| `pubsub-emulator` | 50.6 MiB | **Yes — adopted** |
| `cloud-firestore-emulator` | 66.9 MiB | Future extension only |
| `cloud-datastore-emulator` | 36.2 MiB | Future extension only |
| `bigtable` | 8.3 MiB | Future extension only |

**There is no Cloud Storage emulator, no Cloud Tasks emulator, and no Cloud Run emulator in
the official inventory.** That single fact shapes the whole plan: exactly one of our four MVP
services has a first-party local implementation.

Firestore, Datastore, Bigtable and Spanner are recorded as *future extensions*. Listing them
here does not add them to the release.

---

## 3. Pub/Sub — Google emulator (adopted)

**Identity.** `cloud-pubsub-emulator-0.8.35-all.jar`, a single fat JAR, Java. 54 MB on disk
after an 8 s install via `gcloud components install pubsub-emulator`.

It describes itself on startup, and the wording is worth quoting because it sets expectations
better than any claim we could make:

> `[pubsub] This is the Google Pub/Sub fake.`
> `[pubsub] Implementation may be incomplete or differ from the real system.`
> `[pubsub] INFO: IAM integration is disabled. IAM policy methods and ACL checks are not supported`

**Measured behavior** (probes in `test/upstream/pubsub_probe_test.go`, driven by the official
`cloud.google.com/go/pubsub/v2` SDK):

| Capability | Result |
|---|---|
| Publish → synchronous `Pull` → `Acknowledge` | **OK** |
| **StreamingPull** (the Go/Python default path) | **OK** — all 25 distinct messages delivered |
| Push delivery to a local HTTP target | **OK** — 221-byte envelope received |
| Project isolation; `NotFound` for absent topics | **OK** |
| `Seek(time)` | **OK** |
| `CreateSnapshot` | **OK** |
| Schema service (`ListSchemas`) | Present; returns empty |
| `GetIamPolicy` | `Unimplemented` — matches its own startup notice |
| Resource-name validation | Enforces real rules — rejected a 2-character topic ID with `InvalidArgument` |

StreamingPull working is the decisive result. It is the path both major SDKs use by default
and the hardest part of the Pub/Sub surface; an upstream that lacked it would be worth little.

**Persistence: `--data-dir` does not persist Pub/Sub state.** A topic created against a
`--data-dir` instance was **gone after restart** (`NotFound`), and the directory contained
only `env.yaml`. This is the most important limitation found, because the flag's name implies
otherwise. CloudBurrow must therefore either present Pub/Sub as non-durable or reconstruct
state itself on restart — it cannot inherit durability it does not have. Decided in #25/#27.

**Cost.** Startup to accepting TCP: **0.34 s** direct, **0.60 s** through the `gcloud`
wrapper (mean of 3 runs each). Idle RSS **156 MB** — the JVM is the price of reuse here.

---

## 4. Cloud Storage — no official emulator

### 4.1 Firebase Storage emulator: rejected, with evidence

The starting hypothesis was that the Firebase Storage emulator might cover GCS. It does not.

| Request | Result |
|---|---|
| `POST /storage/v1/b` (create bucket) | **HTTP 501 Not Implemented** |
| `GET /storage/v1/b` (list buckets) | HTTP 404 |
| `GET /storage/v1/b/{bucket}/o` (list objects) | HTTP 200 `{"kind":"storage#objects"}` |

**It has no bucket management at all.** Buckets are implicit in the Firebase model, so
`buckets.insert`, `buckets.list` and `buckets.delete` — all required by our matrix — simply
do not exist. The object surface is partially present, which makes this a genuine subset, not
a near-miss.

Two further costs, both measured:

- **It requires network access on first run.** Starting it downloaded
  `cloud-storage-rules-runtime-v1.1.3.jar` (52.9 MB) into `~/.cache/firebase/emulators/`.
  An offline developer cannot start it cold.
- **447 MB resident** (Node 318 MB + Java rules runtime 127 MB), versus 16 MB for the
  alternative below.

### 4.2 fake-gcs-server: adopted (superseded)

*Superseded:* CloudBurrow built its own Cloud Storage server (#485) and removed fake-gcs-server in
#519. What follows is the original evaluation, kept as the record; its probe test was retired with it.

`fsouza/fake-gcs-server` v1.56.1 — **BSD-2-Clause**, Go, actively maintained (last push
2026-09-18, 1.4k stars), `linux/arm64` image 80.9 MB, **idle RSS 16.4 MB**.

Measured with the official `cloud.google.com/go/storage` SDK
(`test/upstream/storage_probe_test.go`):

| Capability | Result |
|---|---|
| Bucket create/get; object upload, download, delete | **OK** |
| Prefix + delimiter listing | **OK** |
| **Generation preconditions** | **OK** — `DoesNotExist` on an existing object and a stale `GenerationMatch` both return **HTTP 412 `conditionNotMet`**; generation advances on a matched overwrite |
| **Resumable upload** (16 MiB in 256 KiB chunks) | **OK** — CRC32C and MD5 both returned |
| Ranged read | **OK** — byte-exact |
| `compose` | **OK** |
| `copy` | **OK** |
| **Durability** (filesystem backend + volume, across restart) | **OK — object survived** |
| Signed URLs | **Signatures are NOT verified** — a request with `X-Goog-Signature=deadbeef` returned HTTP 200 and the object body |

Preconditions are the result that decided this. They are what makes concurrent writes
correct, and they are the first thing a shallow emulator omits. This one returns the right
status *and* the right reason code.

The signed-URL finding is a limitation to publish, not a defect to hide — and it matches the
limitation CloudBurrow already planned to declare for its own implementation.

---

## 5. Component inventory

Feeds and verification mechanisms below are the input #31 consumes.

| Component | Version | Source | License | Redistribution | Arch | Install size | Idle RSS | Update feed | Verification |
|---|---|---|---|---|---|---|---|---|---|
| Google Pub/Sub emulator | 0.8.35 | gcloud SDK component | See §5.1 — **not established as open source** | **User-side install or official image; do not bundle** | JVM-portable | 54 MB (50.6 MiB dl) | 156 MB | gcloud component snapshot; SDK release notes | gcloud component manager; image digest |
| google-cloud-cli emulators image | 585.0.0-emulators | `gcr.io/google.com/cloudsdktool/google-cloud-cli` | Google Cloud CLI terms | Pull, do not re-publish | **linux/amd64 + linux/arm64** | — | — | GCR tag list | `sha256:3294e8a543de846703594a8a89bfe009e0e9ffbdd2e495d00133f3de14cabcc0` |
| fake-gcs-server | v1.56.1 | `github.com/fsouza/fake-gcs-server` | **BSD-2-Clause** | **Permitted with attribution** | linux/arm64 + amd64 | 80.9 MB image | 16.4 MB | GitHub releases | Image digest; module checksum |
| Knative Serving | v1.23.0 | `github.com/knative/serving` | Apache-2.0 | Permitted | multi-arch | — | not yet measured (#28) | GitHub releases | Release YAML checksum |
| Knative net-kourier | v1.23.0 | `knative-extensions/net-kourier` | Apache-2.0 | Permitted | multi-arch | — | not yet measured (#28) | GitHub releases | Release YAML checksum |
| kind | v0.33.0 | `kubernetes-sigs/kind` | Apache-2.0 | Permitted | darwin/linux arm64+amd64 | — | — | GitHub releases | Release checksum |
| Kubernetes node image | v1.37.0 | `kindest/node` | Apache-2.0 | Permitted | arm64 + amd64 | — | — | kind release notes | Image digest |
| Official Go SDKs | pubsub/v2 v2.7.0, storage v1.68.0 | `googleapis/google-cloud-go` | Apache-2.0 | Permitted | — | — | — | Go module proxy | `go.sum` |

kind v0.33.0 publishes node images for Kubernetes **v1.34.11, v1.35.8, v1.36.4 and v1.37.0**;
current upstream stable is **v1.37.0**. Pinning one *tested* combination of
kind + Kubernetes + Knative + networking is #25's deliverable, not a claim made here.

### 5.1 Licensing versus redistribution

These are different questions and the audit keeps them apart.

The Google Cloud CLI states it is Apache-2.0 **"unless otherwise specified by an alternate
license file."** Inside the emulator JAR, `META-INF/LICENSE.txt` is the Apache-2.0 text and
`META-INF/NOTICE.txt` is Netty's — both belong to **bundled dependencies**. The emulator's own
classes live under `com.google.cloud.pubsub.testing.v1` (e.g. `FakePubsubServer`), and no
corresponding public source repository was found.

**Therefore: do not describe the Google Pub/Sub emulator as open source.** No evidence
supports that claim, and the AC forbids asserting it without evidence.

The practical consequence is a design constraint, not a blocker. CloudBurrow will **not
redistribute the emulator binary**. It will either:

1. run the official `google-cloud-cli:*-emulators` image, pinned by digest, or
2. direct the user to `gcloud components install pubsub-emulator` on their own machine.

Both keep distribution with Google. Option 1 is preferred under the Kubernetes plan, and its
multi-arch index means Apple Silicon and x86 are both covered. If a future release wants to
bundle the binary, that needs written permission, tracked separately.

---

## 6. Adapter interfaces

Reuse is only safe behind narrow interfaces. Each adopted component is wrapped so CloudBurrow
controls lifecycle and reporting, and so a component can be replaced without touching service
code. Implemented under #27; specified here.

| Concern | Contract | Notes for adopted components |
|---|---|---|
| **Readiness** | Report ready only when the backend answers a real request | Bounded polling with a deadline. Pub/Sub ~0.34–0.60 s, but the adapter must never assume a fixed delay. |
| **Endpoint discovery** | Expose the address the SDK should use | Differs host-side vs in-cluster; both forms must be produced. |
| **Reset** | Destroy all state and return to a known-empty condition | Pub/Sub has no reset API → restart the pod. Reset cancels work *before* deleting state. |
| **Persistence** | Declare truthfully whether state survives restart | **Pub/Sub: does not persist**, `--data-dir` notwithstanding. fake-gcs-server: persists with a filesystem backend and a volume. |
| **Logs** | Surface backend logs for diagnosis | Pod logs. |
| **Capability reporting** | Name unsupported operations honestly | e.g. Pub/Sub IAM is `Unimplemented`; GCS signed URLs are unverified. These go in `compatibility.md`, not into a footnote. |

**Testing rule that follows from reuse.** Owned scheduling logic keeps deterministic
injected-clock unit tests. External components get **bounded readiness/event polling with
explicit deadlines** — as in the committed probes — because we cannot advance another
process's clock. This replaces the previous blanket no-sleep rule, and is carried into
AGENTS.md by #25.

---

## 7. Measurement environment and reproduction

Apple M4 Max, 48 GB RAM, macOS (Darwin 27.0.0), arm64 · Docker 29.8.0 · Java OpenJDK 25.0.2 ·
Node v24.21.0 · Go 1.27.1 · gcloud SDK 585.0.0.

```sh
gcloud components install pubsub-emulator   # required by the Pub/Sub probes
make test-upstream                          # runs every probe in this document
```

Probes are behind the `upstream` build tag, so ordinary builds and `make check` do not run
them. They test **third-party software**, not CloudBurrow, and are kept separate from the
compatibility suite for that reason.

### Two corrections made during measurement

Recorded because both would have produced false numbers in this document:

1. **An early Pub/Sub startup reading of 21–36 s was a measurement artifact.** The polling
   loop used `/dev/tcp`, which is a bash feature; this shell is zsh, so the check always
   failed and the loop simply ran to exhaustion. Re-measured with a real socket connect, the
   figure is 0.34 s / 0.60 s.
2. **The first probe run failed against the emulator and the emulator was right.** Topic IDs
   of `t1`/`s1` were rejected with `InvalidArgument`; Pub/Sub requires 3–255 character
   resource IDs. The fix was to the probe. This counts as evidence *for* the emulator's
   fidelity.

## 8. Open items for #25

1. Pin one tested kind + Kubernetes + Knative + networking combination; confirm Knative
   v1.23.0's supported Kubernetes range rather than assuming v1.37.0 is in it.
2. Decide how Pub/Sub non-persistence is surfaced: honest "memory-only", or CloudBurrow-side
   reconstruction.
3. Measure Knative and kind resource budgets (#28) — the only inventory cells still empty.
4. Confirm whether Cloud Tasks has any viable upstream before committing to **build**; the
   current search found none, and that conclusion should be re-tested before #15.


## Amendment: fake-gcs-server publishes Pub/Sub notifications (#79)

The original audit recorded that CloudBurrow would need its own object-mutation dispatcher,
and [#79](https://github.com/cloudburrow/cloudburrow/issues/79) was written on that basis.
**The premise does not hold for 1.56.1.** Measured directly:

```
$ fake-gcs-server -event.pubsub-project-id evt -event.pubsub-topic gcs-events \
    -event.list finalize,delete,metadataUpdate
$ <upload an object, then pull the subscription>
ATTRIBUTES:
  bucketId = evt-bucket
  eventTime = 2026-09-21T15:38:42Z
  eventType = OBJECT_FINALIZE
  objectGeneration = 1790005122939291
  objectId = hello.txt
  payloadFormat = JSON_API_V1
DATA:
  {"kind":"storage#object","id":"evt-bucket/hello.txt#...","name":"hello.txt", ...}
```

That is exactly the attribute set and payload Cloud Storage sends. Reimplementing it would
have produced something less faithful than what upstream already emits, so CloudBurrow reuses
it and builds only what is genuinely missing.

**What is missing** is routing: the flags take a *single* topic for the whole server, while
the API lets each bucket register several `notificationConfigs` with different topics,
filters and custom attributes. CloudBurrow therefore points the backend at one internal topic
and fans events out from there (`internal/service/storagenotify`).

*Corrected by #486:* this said the `notificationConfigs` management API itself was absent
upstream. That is no longer true: since
[6f935cea](https://github.com/fsouza/fake-gcs-server/commit/6f935ceaab04d41cd20e17ae5e439c310a1745e8)
(#2157, 2026-03-31), included in v1.56.1, fake-gcs-server keeps an in-memory, per-bucket
`notificationConfigs` registry and publishes to each config's topic. It is not durable, and
CloudBurrow still serves the API in front of the backend.

---

## Amendment: Cloud KMS is built, not reused (#309)

[#309](https://github.com/cloudburrow/cloudburrow/issues/309) first proposed recording that
"no Google or maintained third-party GCP KMS emulator exists". **That premise was false.**
Several third-party emulators exist, and one Google-written fake exists. Cloud KMS is built
anyway. The reason is not their absence: each one, read at the commit below, departs from
Google's documented behaviour in a way a CloudBurrow user would inherit. Reusing any of them
would mean owning a fork. The full comparison is the council report on #309; the evidence
that decides it is recorded here.

**Google publishes no Cloud KMS emulator.** `gcloud emulators` covers Bigtable, Datastore,
Firestore, Pub/Sub and Spanner only (checked in the pinned Cloud SDK image and in the
[reference](https://cloud.google.com/sdk/gcloud/reference/beta/emulators)).

| Candidate | Licence | Why it is not reused | Evidence (file:line at a full commit SHA) |
|---|---|---|---|
| Google `kms-integrations/fakekms` | Apache-2.0 | Test infrastructure for Google's PKCS#11 and CNG libraries. Encrypt, Decrypt, UpdateCryptoKeyPrimaryVersion and RestoreCryptoKeyVersion are not on its method allowlist, so they return UNIMPLEMENTED. It listens on localhost only. | [interceptor.go:42-51](https://github.com/GoogleCloudPlatform/kms-integrations/blob/de849afa57f6e46c1268fbced15c161c532bff8b/fakekms/interceptor.go#L42-L51), [fakekms.go:110](https://github.com/GoogleCloudPlatform/kms-integrations/blob/de849afa57f6e46c1268fbced15c161c532bff8b/fakekms/fakekms.go#L110) |
| `blackwell-systems/gcp-kms-emulator` | Apache-2.0 | Two contributors, one of them for doc comments. At #309 (commit `8f63728`) untyped storage errors became INTERNAL, including "primary version is not enabled", which a caller cannot tell apart from a crash. **Fixed in [v0.10.1/v0.10.2](https://github.com/blackwell-systems/gcp-kms-emulator/releases/tag/v0.10.2)** (#1690): a disabled, destroyed or missing primary version is FAILED_PRECONDITION in Cloud KMS wording ("… is not enabled, current state is: DISABLED."), an invalid ciphertext or AAD and a bad page token are INVALID_ARGUMENT, and REST errors use Google's error shape. | At v0.10.2: [server.go:58-76](https://github.com/blackwell-systems/gcp-kms-emulator/blob/5372c0046ce0c253717a0d77a127b01e70a9658d/internal/server/server.go#L58-L76), [storage.go:320-327](https://github.com/blackwell-systems/gcp-kms-emulator/blob/5372c0046ce0c253717a0d77a127b01e70a9658d/internal/storage/storage.go#L320-L327), [errors.go:46-50](https://github.com/blackwell-systems/gcp-kms-emulator/blob/5372c0046ce0c253717a0d77a127b01e70a9658d/internal/storage/errors.go#L46-L50). Before: [server.go:57-70](https://github.com/blackwell-systems/gcp-kms-emulator/blob/8f6372808a3e798bec0bdbcd6183915c963b2ada/internal/server/server.go#L57-L70), [storage.go:315](https://github.com/blackwell-systems/gcp-kms-emulator/blob/8f6372808a3e798bec0bdbcd6183915c963b2ada/internal/storage/storage.go#L315) |
| `floci-io/floci-gcp` (KMS module) | MIT | Symmetric Decrypt never checks version state, so a DISABLED or DESTROY_SCHEDULED version still decrypts. Its asymmetric decrypt does check (line 277). It is a whole-GCP Java bundle. | [CloudKmsService.java:243-253](https://github.com/floci-io/floci-gcp/blob/9e63ba20136cfddbc258b7f156645933caf9805a/src/main/java/io/floci/gcp/services/cloudkms/CloudKmsService.java#L243-L253) |
| `winor30/fake-cloud-kms` | Apache-2.0 | No version state machine: every version is ENABLED, and there is no UpdateCryptoKeyVersion, DestroyCryptoKeyVersion or RestoreCryptoKeyVersion. | [service.go:140,168,255](https://github.com/winor30/fake-cloud-kms/blob/c0ef10425b22da3e257b052605139f7c315e54c5/service/service.go#L140) |
| `slokam-ai/localgcp` (KMS) | MIT | One contributor. "Encryption" is XOR with the key. | [store.go:263-283](https://github.com/slokam-ai/localgcp/blob/851a7e08bb0981b207139131c8207f1bd10cd5b2/internal/kms/store.go#L263-L283) |
| Config Connector `mockgcp/mockkms` | Apache-2.0 | Admin RPCs only: no Encrypt, Decrypt, ListKeyRings, ListCryptoKeys or UpdateCryptoKeyPrimaryVersion. Tied to the Config Connector test harness. | [mockkms/](https://github.com/GoogleCloudPlatform/k8s-config-connector/tree/f84fce70215779c4e01e19dbcc5bafdca0b559b8/mockgcp/mockkms) |
| `k8s-cloudkms-plugin` `testutils/fakekms` | Apache-2.0 | Returns the plaintext as the ciphertext. It is a test double for the Kubernetes KMS plugin, not a KMS. | [fakekms.go:118-133](https://github.com/GoogleCloudPlatform/k8s-cloudkms-plugin/blob/a88bafe6cfcc8727e6a7243dfa16f978b1c9a468/testutils/fakekms/fakekms.go#L118-L133) |
| Kubernetes KMS (v2 plugin API) | — | A different API: Status, Encrypt and Decrypt on one key, for the API server's encryption at rest, over a Unix socket. It has no key rings, keys, versions or AAD. | [kubernetes/kms api.proto](https://github.com/kubernetes/kms/blob/release-1.37/apis/v2/api.proto) |
| Tink as the server's crypto | Apache-2.0 | A crypto library, not a server. Google's Cloud KMS ciphertext format is unpublished, so Tink gives no fidelity that the Go standard library does not. | — |

**What is built, and with what.** CloudBurrow implements `google.cloud.kms.v1` itself, to the
behaviour in Google's docs and protos, finishing the draft on the `feat/309-kms` branch.
- **Crypto:** Go standard library only (`crypto/aes`, `crypto/cipher` GCM, and `hash/crc32`
  with the Castagnoli table). There is no Tink on the server.
- **Protos:** the official generated `cloud.google.com/go/kms` package, pinned in `go.mod`
  when the service lands (#385).
- **fakekms** is a reference and a test oracle only (#419, #420). It is never shipped in the
  runtime image, ported code keeps its Apache-2.0 header, and its error codes are not taken
  as ground truth.
- **No third-party runtime image** is added for KMS.

**Revisit** if floci-gcp's KMS module gains version-state and CRC32C checks on Decrypt and
can be shown to run KMS-only, or if Google publishes a KMS emulator.
gcp-kms-emulator was re-read at v0.10.2 on 2026-11-17
([amendment](#amendment-gcp-kms-emulator-v0102-re-evaluated-1712)).

### fakekms as a differential oracle (#419)

`make oracle-kms` compares CloudBurrow's KMS with Google's fakekms, vendored at
kms-integrations `de849afa57` in its own module
([test/oracle/fakekms](../test/oracle/fakekms/README.md)); the root module does
not depend on it and the binary contains none of it. Agreement with fakekms is
**not** an observation of Google and never removes an UNVERIFIED annotation.
fakekms stays a reference, not the server: running it measured that its module
cannot be built from the Go proxy (its fault protos are generated by Bazel and
not committed), that it refuses `labels` on CreateCryptoKey, and that a request
carrying a map field crashed it until patched.

---

## Amendment: Cloud Storage is built, not reused (#485)

§4.2 adopted `fake-gcs-server` because "no official Google GCS emulator exists". That is still
true, but measurement since has shown the adopted server departs from Google's documented
behaviour in ways a CloudBurrow user inherits ([#373](https://github.com/cloudburrow/cloudburrow/issues/373),
[#374](https://github.com/cloudburrow/cloudburrow/issues/374), [#321](https://github.com/cloudburrow/cloudburrow/issues/321)).
The maintainer decided on #374 that CloudBurrow builds its own Cloud Storage server, as for
[Cloud KMS](#amendment-cloud-kms-is-built-not-reused-309). A council (report on
[#485](https://github.com/cloudburrow/cloudburrow/issues/485)) tested that decision against
ADR-0005 and **upheld it, narrowly**: reuse is refused because every candidate would mean
owning a fork, not because the candidates are unofficial.

**Google publishes no Cloud Storage emulator.** `gcloud beta emulators` covers Bigtable,
Datastore, Firestore, Pub/Sub and Spanner only
([reference](https://docs.cloud.google.com/sdk/gcloud/reference/beta/emulators)).

| Candidate | Licence | Why it is not reused | Evidence (file:line at a full commit SHA) |
|---|---|---|---|
| `fsouza/fake-gcs-server` v1.56.1 (in use) | BSD-2-Clause | Every object reports metageneration `"1"`, so a metadata patch never advances it. A bucket patch decodes only `defaultEventBasedHold` and `versioning.enabled`, so `labels`, `storageClass`, `cors`, `lifecycle`, `retentionPolicy` and `website` are dropped with HTTP 200, and an omitted field resets a kept one. The filesystem backend, which persistent mode uses, refuses versioning. Any request carrying `X-Goog-Algorithm` is treated as signed, with no signature check. | [response.go:222](https://github.com/fsouza/fake-gcs-server/blob/896ece51e49eed0946089f3089f6343855c0d106/fakestorage/response.go#L222), [bucket.go:59-73](https://github.com/fsouza/fake-gcs-server/blob/896ece51e49eed0946089f3089f6343855c0d106/fakestorage/bucket.go#L59-L73), [fs.go:87,143](https://github.com/fsouza/fake-gcs-server/blob/896ece51e49eed0946089f3089f6343855c0d106/internal/backend/fs.go#L87), [upload.go:185-192](https://github.com/fsouza/fake-gcs-server/blob/896ece51e49eed0946089f3089f6343855c0d106/fakestorage/upload.go#L185-L192) |
| Google `googleapis/storage-testbench` v0.64.0 | Apache-2.0 | The strongest reference, and the widest (JSON and gRPC v2, metageneration and all four preconditions, labels, CORS, lifecycle, retention, HMAC keys). But its README says it is "not an officially supported Google product", "expected to be used by Storage library maintainers", with no support for filed issues, and it ships as v0.x with no stability promise. It keeps all state in process memory, never publishes notifications to Pub/Sub, and `objects.list` returns no `nextPageToken`. Old generations are kept on unversioned buckets and are reachable with `versions=true` or `generation=` (plain GET and list return only the live one). | [README.md:3-7](https://github.com/googleapis/storage-testbench/blob/f1fbebcec2e7003bca4459639c424abf8bc0945c/README.md?plain=1#L3-L7), [rest_server.py:506-517](https://github.com/googleapis/storage-testbench/blob/f1fbebcec2e7003bca4459639c424abf8bc0945c/testbench/rest_server.py#L506-L517), [database.py:467-517](https://github.com/googleapis/storage-testbench/blob/f1fbebcec2e7003bca4459639c424abf8bc0945c/testbench/database.py#L467-L517) |
| `oittaa/gcp-storage-emulator` | BSD-3-Clause | One human maintainer in practice. It implements no preconditions, so concurrent-write guards silently pass, and its `testIamPermissions` is a stub that grants everything with no etag check on `setIamPolicy`. | [buckets.py:257-303](https://github.com/oittaa/gcp-storage-emulator/blob/dddc30909ee52d7c5c2ae22bb41150b327027f9e/src/gcp_storage_emulator/handlers/buckets.py#L257-L303), [objects.py:221-473](https://github.com/oittaa/gcp-storage-emulator/blob/dddc30909ee52d7c5c2ae22bb41150b327027f9e/src/gcp_storage_emulator/handlers/objects.py#L221-L473) |
| LocalStack | — | Emulates AWS, Snowflake and Azure, not Google Cloud; its open-source repository is archived. | [docs.localstack.cloud](https://docs.localstack.cloud/) |

The testbench's success-path fidelity is **mixed, not lower**: it keeps fields fake-gcs-server
drops. The decision rests on fork cost and on the documented-semantics departures above.

**What is built, and where it runs.** CloudBurrow implements the Cloud Storage JSON API v1,
the batch endpoint and an XML subset itself, to Google's docs and discovery document, in one
Go package. gRPC `google.storage.v2` is deferred. It runs in-process for tests and, because
workloads must reach it in-cluster, as a single in-cluster Deployment built from
**CloudBurrow's own digest-pinned image**. `mediaLink` and `selfLink` are built from the
request's `Host`, which removes the second (internal) Deployment fake-gcs-server needs.
fake-gcs-server stays behind a backend switch until the builtin server passes every storage
compat suite, then it is removed (#519).

**storage-testbench is a reference and CI-only differential oracle**, as fakekms is for KMS.
It runs only in an opt-in CI tier, pinned by image digest or rebuilt from `f1fbebce` against
a hash-locked requirements file (its transitive dependencies otherwise float), and is never
the runtime. Agreement with it is not an observation of Google.

**Revisit** if Google publishes a supported Cloud Storage emulator, or if storage-testbench
gains durable state, notification delivery and a stability promise.

---

## Amendment: Secret Manager, Cloud Scheduler, Cloud Logging and Resource Manager are built, not reused (#588)

Recorded 2026-09-27. These four were built without an entry here, so this records the decision
after the fact. **It is weaker evidence than the Cloud KMS and Cloud Storage amendments.** Each
issue concluded "Google publishes no emulator, so building is justified"; no third-party
candidate was probed, and nothing below was measured. The candidates were read on 2026-09-27,
from their READMEs and repository metadata at the commits linked, to record what existed and
why none would have been reused. They were not run.

| Service | Built | Issue |
|---|---|---|
| Secret Manager | 2026-09-21 | #93 (the audit, #24, found no Google emulator) |
| Resource Manager | registry 2026-09-22 (#113); v3 API 2026-09-24 (#298); v1 (#301) | #298, #301 |
| Cloud Scheduler | 2026-09-24 | #302 |
| Cloud Logging | 2026-09-24 | #304 |

**Google publishes no emulator for any of the four.** `gcloud beta emulators` covers Bigtable,
Datastore, Firestore, Pub/Sub and Spanner only
([reference](https://docs.cloud.google.com/sdk/gcloud/reference/beta/emulators)).

**The requirement a separate emulator does not meet.** Each of the four shares state with a
piece CloudBurrow already owns, and a reused emulator would bring a store of its own:

- **Secret Manager** keeps versions as Kubernetes Secrets in the workload namespace, so a Cloud
  Run revision's `secretKeyRef` resolves to them (`internal/adapter/run/mapping.go`,
  `SecretResolver`). An emulator keeping secrets in its own process could not back a revision's
  environment.
- **Cloud Scheduler** retries on the schedule Cloud Tasks computes and publishes Pub/Sub
  targets to the local Pub/Sub emulator.
- **Cloud Logging** is the store the console's Logs Explorer reads, so API-written entries sit
  beside pod logs.
- **Resource Manager** serves the project registry the console creates and lists projects
  in. The console is a view, never a second store (`internal/console`), so a separate
  Resource Manager would have made two answers to "which projects exist".

All four also run in the CLI process, so `/admin/reset`, seeding, `state save` and `load`,
`/metrics`, the request log, tracing and fault injection cover them (#600). A proxied upstream
process gets none of that, as Pub/Sub and the opt-in emulators show (status.md).

| Candidate | Licence | Serves | Why it is not reused |
|---|---|---|---|
| [`kortschak/scheduler`](https://github.com/kortschak/scheduler/tree/59494b2647a88763ddce39fd54a9f6abb1044b53) | not detected by GitHub | Scheduler | Not the API: jobs come from a YAML file and are published to Pub/Sub on a cron, with no `google.cloud.scheduler.v1` surface and no HTTP targets. No commit since April 2021. |
| [`floci-io/floci-gcp`](https://github.com/floci-io/floci-gcp/tree/37feff1c91a495ad432ece2c5ed70884c300963a) | MIT | Secret Manager, Scheduler, Logging, Resource Manager, among many others | The widest candidate and actively maintained (21 contributors). A whole-GCP Java bundle serving every API on one port, as the KMS amendment found; reusing one service means running the bundle, with its own store. |
| [`GuitarWag/gcp-local`](https://github.com/GuitarWag/gcp-local/tree/ed1c0821d0bdd4bd8344a4efa3d6da213ac9fea4) | MIT | Secret Manager, Scheduler, Logging | One contributor, created 2026-05. A whole-platform single binary with its own BoltDB store. |
| [`slokam-ai/localgcp`](https://github.com/slokam-ai/localgcp/tree/851a7e08bb0981b207139131c8207f1bd10cd5b2) | MIT | Secret Manager, Logging | One contributor. A whole-platform binary. Its KMS "encryption" is XOR (KMS amendment), which does not recommend its other services. Its README lists Secret Manager IAM and replication, and Logging sinks, metrics and tail, as unsupported. |
| [LocalCloud](https://github.com/LocalGCloud/localcloud-cli) | proprietary | Scheduler, Logging, Resource Manager (per its [docs contract](https://github.com/LocalGCloud/LocalGCloud.github.io/blob/main/src/data/docs-contract.snapshot.json)) | Licensed as proprietary. The server ships as a Docker image; the public repository is its CLI. It cannot be forked or redistributed. |
| [`blackwell-systems/gcp-secret-manager-emulator`](https://github.com/blackwell-systems/gcp-secret-manager-emulator/tree/1918ec95bf24c6ed57a729b8e421cd3bb6a63d7f) | Apache-2.0 | Secret Manager | Two contributors. gRPC and REST, with optional IAM enforcement that ADR-0006 declines. A separate process with its own store, which a Cloud Run revision cannot reference. |
| [`charlesgreen/gsm`](https://github.com/charlesgreen/gsm/tree/a6ab46bf027fe188eff612c3e142b8b58f9f69bc) | MIT | Secret Manager | Two contributors. REST only, while the Go and Python clients default to gRPC. |
| Config Connector [`mockgcp`](https://github.com/GoogleCloudPlatform/k8s-config-connector/tree/32ebdff9011fece2de001a6223d69ad3fc89a8d5/mockgcp) (`mocksecretmanager`, `mocklogging`, `mockresourcemanager`) | Apache-2.0 | Secret Manager, Logging, Resource Manager | Built for the Config Connector test harness, as the KMS amendment found for `mockkms`. There is no Scheduler mock. |
| `cloud.google.com/go/logging` [`internal/testing`](https://github.com/googleapis/google-cloud-go/tree/967a532a6d1f3ffe5210c7f972b82b20ef663c2d/logging/internal/testing) | Apache-2.0 | Logging | A fake for the Go client's own tests, under `internal/`, so no other module can import it. |

**Revisit** if Google publishes an emulator for any of the four, or if a candidate can run one
service alone with its state reachable by the rest of CloudBurrow. floci-gcp is the one to
re-read first. A probe in `test/upstream/`, as for Pub/Sub, would turn this record into
measured evidence.

---

## Amendment: gcp-kms-emulator v0.10.2 re-evaluated (#1712)

Recorded 2026-11-17. **This records evidence and a recommendation; the decision is the
user's, and the [Cloud KMS amendment](#amendment-cloud-kms-is-built-not-reused-309) stands
until the user changes it.**

**Why.** The KMS amendment's only recorded departure for `blackwell-systems/gcp-kms-emulator`
was that caller errors became INTERNAL. Its maintainer fixed that in v0.10.1 and v0.10.2
([#1690](https://github.com/cloudburrow/cloudburrow/issues/1690)), so its row no longer
supported the claim that every third-party emulator departs from Google. The user asked for
a fresh evaluation ([#1712](https://github.com/cloudburrow/cloudburrow/issues/1712)).

**Method: read, not run.** The release notes, README, `docs/FEATURES.md`, `CHANGELOG.md`
and source were read at tag `v0.10.2`, commit
[`5372c004`][kmsemu], through the GitHub API. Nothing of theirs was downloaded as a
module, built or run: executing third-party code needs the user's approval. Every
behaviour below is read from their code. Where only running it would settle a claim, the
row says **not measured**. CloudBurrow's side comes from [coverage/kms.md](coverage/kms.md),
the KMS rows of [compatibility.md](compatibility.md#cloud-kms--googlecloudkmsv1), and the
tests named.

### Comparison

| Criterion | gcp-kms-emulator v0.10.2 | CloudBurrow built-in |
|---|---|---|
| Services served | `KeyManagementService` only ([register.go:31][kmsemu-register], [server-dual main.go:96][kmsemu-dual]). No `IAMPolicy`, `Locations`, `EkmService`, `Autokey`, `AutokeyAdmin` or `HsmManagement`: a client calling them gets UNIMPLEMENTED for an unknown service. | 63 methods over all seven services, every one Verified ([coverage/kms.md](coverage/kms.md)). |
| `KeyManagementService` methods | 28 of the 33 in the `cloud.google.com/go/kms` v1.27.0 it pins ([README.md:117][kmsemu-readme-ops]). `Decapsulate` returns UNIMPLEMENTED ([server.go:463-470][kmsemu-decap]). Missing: `DeleteCryptoKey`, `DeleteCryptoKeyVersion`, `GetRetiredResource`, `ListRetiredResources`. `DeleteKeyRing` and the trusted-wrapping pair are not in v1.27.0 at all (33 methods in its [service_grpc.pb.go][g-kms127]). | All 36 at v1.35.0, those three included. |
| Algorithms | `GOOGLE_SYMMETRIC_ENCRYPTION`; `RSA_SIGN_PKCS1_*` (4); `EC_SIGN_P256_SHA256`, `EC_SIGN_P384_SHA384`, **`EC_SIGN_SECP256K1_SHA256`**, `EC_SIGN_ED25519`; `RSA_DECRYPT_OAEP_*` (8); `HMAC_*` (5); `AES_128_GCM`, `AES_256_GCM` ([keymaterial.go:45-146][kmsemu-keymat]). | All of those except secp256k1 (UNIMPLEMENTED: Go's standard library has no secp256k1), plus `RSA_SIGN_PSS_*`, `RSA_SIGN_RAW_PKCS1_*`, AES CBC and CTR for raw encryption, `ML_KEM_768` decapsulation and `AES_256_KWP` wrapping. |
| Protection levels | Every version and response says SOFTWARE, even when `version_template.protection_level` asks for HSM ([storage.go:266-281][kmsemu-createkey], [server.go:278][kmsemu-encrypt]). | SOFTWARE; HSM and HSM_SINGLE_TENANT (*no HSM*); EXTERNAL and EXTERNAL_VPC (*the instance acts as the EKM*). |
| `CreateCryptoKey` fields | Only `purpose`, `version_template` and `labels` reach storage ([server.go:224-227][kmsemu-server-create]). `rotation_period`, `next_rotation_time`, `destroy_scheduled_duration`, `skip_initial_version_creation`, `import_only`, `crypto_key_backend` and `key_access_justifications_policy` are dropped and the call succeeds. Purpose and algorithm are not checked against each other: `ASYMMETRIC_SIGN` with no algorithm gets an AES key ([storage.go:225-231][kmsemu-createkey]). Every key gets a primary, though resources.proto says only ENCRYPT_DECRYPT keys have one ([storage.go:751-768][kmsemu-keyproto]; [resources.proto:130-133][g-primary]). | Each field is stored and validated, or refused as UNIMPLEMENTED by name (`TestKMSCreateCryptoKeyFields`, `TestKMSRotationSchedule`, `TestKMSOutOfScopeKeyOptionsAreUnimplemented`). |
| Version states and destroy scheduling | `DestroyCryptoKeyVersion` sets `destroy_time` to now plus a fixed 30 days, whatever the key's `destroy_scheduled_duration` ([storage.go:653][kmsemu-destroy]). **Nothing ever moves a version to DESTROYED**: the only use of that state is a check ([storage.go:635][kmsemu-destroy]), so key material is never removed. Destroying a version already DESTROY_SCHEDULED succeeds ([storage.go:639-650][kmsemu-destroy]). `RestoreCryptoKeyVersion` does not look at `destroy_time` ([storage.go:682-686][kmsemu-restore]), and Get and List omit it ([storage.go:530-537][kmsemu-getversion]). New versions, asymmetric ones included, are ENABLED at once. | `destroy_time` comes from the key's duration; the state is computed from the clock and a sweep removes the material at `destroy_time` (`TestAScheduledVersionIsDestroyedAtItsDestroyTime`, on a fake clock); asymmetric versions start PENDING_GENERATION (`TestKMSAsymmetricVersionsStartPendingGeneration`). |
| `Encrypt` and `Decrypt` | `Encrypt` looks the name up as a CryptoKey only, so a CryptoKeyVersion name, which [service.proto:1324-1330][g-encrypt] allows, is NOT_FOUND ([storage.go:304-313][kmsemu-encrypt-storage]). `Decrypt` tries every ENABLED version and skips the rest, so a ciphertext of a DISABLED or DESTROY_SCHEDULED version is INVALID_ARGUMENT "the ciphertext is invalid" ([storage.go:371-382][kmsemu-decrypt]). CRC32C is checked on both. | Encrypt by key or version name; Decrypt refuses a version that is not ENABLED with FAILED_PRECONDITION (`TestKMSEncrypt`, `TestKMSDecryptFollowsTheVersionLifecycle`). |
| MAC, raw, random | `MacSign`/`MacVerify` with a purpose and state check. `RawEncrypt` is GCM only and ignores a caller's IV ([storage/raw.go][kmsemu-raw]). `GenerateRandomBytes` takes 1 to 1024 bytes and ignores `protection_level` ([random.go:12][kmsemu-random]). | MAC the same; raw GCM, CBC and CTR (`TestKMSRawEncryptAndDecrypt`); random 8 to 1024 at HSM only (`TestKMSGenerateRandomBytes`). |
| Import jobs | `CreateImportJob`, `GetImportJob`, `ListImportJobs`, `ImportCryptoKeyVersion` with RSA-OAEP wrapping; asymmetric key import is listed as not implemented ([FEATURES.md][kmsemu-features]). | Verified (`TestKMSImportManuallyWrappedKeys`). Whether their wrapping accepts the AES-KWP envelope Google's sample builds is **not measured**. |
| EKM, Autokey, single-tenant HSM | Not served. | Verified (`TestKMSEkmConnectionsAreStored`, `TestKMSExternalKeys`, `TestKMSAutokeyProvisionsKeysOnDemand`, `TestKMSSingleTenantHsm`). |
| IAM | Optional *enforcement* against their separate IAM emulator, chosen by `IAM_MODE` and `IAM_EMULATOR_HOST`, with the principal in an `x-emulator-principal` header ([server.go:103-147][kmsemu-iam]). No `GetIamPolicy`, `SetIamPolicy` or `TestIamPermissions`. | The three IAM RPCs on key rings, keys, EKM connections and `ekmConfig`: *stored, not enforced* ([ADR-0006](adr/0006-iam-policy-surface.md)). |
| Locations | Not served. | `ListLocations`, `GetLocation` (`TestKMSLocations`). |
| `key_access_justifications_policy` | Dropped at create; `UpdateCryptoKey` ignores the path ([storage.go:722-737][kmsemu-updatekey]). | Stored, and enforced for a call's own reason (`TestKMSHSMKeysAndKeyAccessJustifications`). |
| Lists and names | `page_size` and an integer-offset `page_token`; `filter`, `order_by` and `view` are ignored ([storage.go:55-82][kmsemu-paginate]). Names are only checked for being non-empty, so a malformed name is NOT_FOUND, and `CreateKeyRing` stores whatever `parent` it is given ([server.go:150-167][kmsemu-createring]). | `filter` (partial), `order_by` and `view`; every name parsed before lookup, INVALID_ARGUMENT when malformed (`TestKMSNamesAreValidatedBeforeLookup`). |
| Persistence | In memory by default. With `GCP_KMS_DATA_DIR`, the whole state is rewritten to one `state.json` (temp file and rename) after every change, with private keys in plaintext ([persistence.go:91-160][kmsemu-persist]). No reset API. | Kubernetes Secrets in the managed namespace; restart durability Verified and measured (`TestKMSAcrossRestart`); `/admin/reset`, per project; `state save`/`load`. |
| REST and gRPC | gRPC on 9090 and REST on 8080: two ports, the REST side a grpc-gateway that dials the gRPC port ([gateway.go:58][kmsemu-gateway]). REST errors use Google's shape since v0.10.2. | One port for both, as `cloudkms.googleapis.com` (`TestKMSJSONIsServedOnTheSamePort`), through the shared transcoder. |
| Licence | Apache-2.0. | Compatible; a port would keep their header, as fakekms code does. |
| Maintenance | 107 commits; 2 contributors (the maintainer 105, `baktun14` 2, for doc comments); 17 releases from v0.1.0 (2026-01-26) to v0.10.2 (2026-11-04), four of them since June; 9 stars, 3 forks, 0 open issues. Answered #1690 with a fix within weeks. One maintainer in practice. | — |
| Architectures | Images for `linux/amd64` and `linux/arm64` on `ghcr.io`, from [docker-publish.yml:60][kmsemu-docker]; no binaries attached to releases; otherwise `go install`. Image size, idle RSS and start time are **not measured**. | Part of the CloudBurrow binary. |
| How it would be embedded | Its root package exports only `Register(*grpc.Server)` and `NewGatewayHandler(grpcAddr)` ([register.go][kmsemu-register], [gateway.go][kmsemu-gw-root]); storage and server are under `internal/`. Configuration is environment variables, and the clock is `time.Now`. In-process it would mean a **new Go module** in `go.mod`, with `gcp-emulator-auth`, `dcrd/secp256k1` and `grpc-gateway` behind it ([go.mod:5-13][kmsemu-gomod]); out of process, a **new container**. Either needs the user's approval. Neither form gives CloudBurrow reset, state save/load, Kubernetes-backed storage, an injectable clock, fault injection or the in-process access the console and the CMEK users (Secret Manager, Memorystore) have today. | In the CLI process; `cmd/cloudburrow/kms.go`, the console and the CMEK services call it directly. |

### Error codes against CloudBurrow's rows

Where the two agree: NOT_FOUND and ALREADY_EXISTS for missing and duplicate resources;
FAILED_PRECONDITION for `Encrypt` on a primary that is not ENABLED or missing, for the wrong
key purpose, and for `UpdateCryptoKeyPrimaryVersion` to a version that is not ENABLED; INVALID_ARGUMENT for a tampered
ciphertext, a wrong AAD and a CRC32C mismatch; FAILED_PRECONDITION for restoring an ENABLED
version. Their messages say "`<version>` is not enabled, current state is: DISABLED." and
"`<key>` has incorrect key purpose …" ([errors.go:46-56][kmsemu-errors]), which the
changelog calls Cloud KMS's wording; CloudBurrow's are its own. No recorded observation
shows which is Google's verbatim.

Where they differ. † marks a code that Google does not document either: CloudBurrow's tests
assert it, and it is listed, with its dated annotation, under "Unverified error codes" in
[coverage/kms.md](coverage/kms.md#unverified-error-codes).

| Case | gcp-kms-emulator v0.10.2 | CloudBurrow |
|---|---|---|
| `UpdateCryptoKeyVersion` mask without `state`, or a target of DESTROYED | FAILED_PRECONDITION ([storage.go:592-606][kmsemu-updateversion]) | INVALID_ARGUMENT † |
| `DestroyCryptoKeyVersion` on a DESTROY_SCHEDULED version | succeeds | FAILED_PRECONDITION † |
| `Decrypt` of a DISABLED or DESTROY_SCHEDULED version's ciphertext | INVALID_ARGUMENT | FAILED_PRECONDITION † |
| `Decrypt` given a CryptoKeyVersion name | NOT_FOUND | INVALID_ARGUMENT † |
| `Encrypt` given a CryptoKeyVersion name | NOT_FOUND | succeeds, as service.proto documents |
| A malformed name | NOT_FOUND | INVALID_ARGUMENT † |
| `AsymmetricDecrypt` of a ciphertext that does not decrypt | **INTERNAL**: the `rsa.DecryptOAEP` error is returned untyped ([asymmetric.go:276][kmsemu-asymdecrypt]) | INVALID_ARGUMENT † |
| `AsymmetricSign` with a digest of another hash | signed: the digest's hash is never compared with the algorithm's ([asymmetric.go:187-210][kmsemu-asymsign]); for RSA, a digest of the wrong length is **INTERNAL** | INVALID_ARGUMENT † |
| `CreateCryptoKey` with an algorithm it lacks (PSS, raw PKCS#1, CBC, ML-KEM) | **INTERNAL** "failed to generate key material: unsupported algorithm" ([keymaterial.go:146][kmsemu-keymat], [storage.go:231-234][kmsemu-createkey]) | created, or UNIMPLEMENTED naming the algorithm |
| `CreateCryptoKey` `ASYMMETRIC_SIGN` with no algorithm | succeeds, with an AES key | INVALID_ARGUMENT † |
| `UpdateCryptoKey` with `destroy_scheduled_duration`, IMMUTABLE in [resources.proto:199-200][g-dsd], or an unknown path | applied, or silently ignored ([storage.go:722-737][kmsemu-updatekey]) | INVALID_ARGUMENT † |
| `GenerateRandomBytes` of 1 to 7 bytes, or not at HSM | succeeds | INVALID_ARGUMENT † |

### What CloudBurrow would lose

Read from their code, not run; behind CloudBurrow's endpoint these tests would fail:

- **Services it does not serve:** `TestKMSEkmConnectionsAreStored`, `TestKMSExternalKeys`,
  `TestKMSEkmIamPolicyIsStoredNotEnforced`, `TestKMSAutokeyProvisionsKeysOnDemand`,
  `TestKMSSingleTenantHsm`, `TestKMSIamPolicyIsStoredNotEnforced`, `TestKMSLocations`, and
  their gcloud and Terraform counterparts.
- **Methods it does not serve:** `TestKMSDecapsulateMLKEM`, `TestKMSTrustedKeyWrapping`,
  `TestKMSDeleteCryptoKeyLeavesARetiredResource`, `TestKMSDeleteKeyRing`.
- **Behaviour above:** `TestKMSHSMKeysAndKeyAccessJustifications`, `TestKMSRotationSchedule`,
  `TestKMSCreateCryptoKeyFields`, `TestKMSNamesAreValidatedBeforeLookup`,
  `TestKMSUpdateCryptoKeyVersion`, `TestKMSDestroyCryptoKeyVersion`, `TestKMSEncrypt`,
  `TestKMSDecryptFollowsTheVersionLifecycle`, `TestKMSUpdateCryptoKey`, `TestKMSOrderByName`,
  `TestKMSVersionStateFilter`, `TestKMSNameAndLabelFilters`,
  `TestKMSAsymmetricSignVerifiesWithThePublicKeyPEM` (PSS),
  `TestKMSAsymmetricDecryptOpensWhatThePublicKeyEncrypted`, `TestKMSRawEncryptAndDecrypt`
  (CBC, CTR), `TestKMSGenerateRandomBytes`, `TestKMSAsymmetricVersionsStartPendingGeneration`,
  `TestKMSJSONIsServedOnTheSamePort`.
- **State:** `TestKMSResources` (Secrets in the namespace), `TestKMSAcrossRestart`,
  `TestKMSProjectResetClearsOnlyThatProject`.
- **In-process users:** the console's KMS pages and the Secret Manager and Memorystore CMEK
  paths import `internal/service/kms`, and its 81 unit tests (fake-clock destroy, filters,
  names, the durable store) have no counterpart to run against.

Likely to pass, **not measured**: `TestKMSTinkEnvelopeEncryption` (it needs both CRC32C
flags, which they set) and the plain create, get, list, encrypt and decrypt path of
`TestKMSResources`.

### Recommendation: keep the built-in

1. **It serves a subset.** One service of seven and 28 of CloudBurrow's 63 methods. EKM,
   Autokey, IAM policies, locations, single-tenant HSM, deletion, retired resources,
   trusted wrapping and ML-KEM would have to stay CloudBurrow's anyway, so adoption would
   mean two KMS stores side by side, not one fewer.
2. **It still departs from Google's documented behaviour where users would inherit it:**
   `destroy_scheduled_duration` is ignored, DESTROYED is never reached, `Encrypt` refuses a
   CryptoKeyVersion name, asymmetric keys report a primary, HSM keys report SOFTWARE, and
   create fields are dropped with success. Three caller errors are still INTERNAL. The KMS
   amendment's claim holds again, on new evidence.
3. **Embedding costs more than it saves.** A new module or container (user approval either
   way), configuration by environment variable, a second port for REST, and none of
   reset, save/load, the Kubernetes store, the fake clock or fault injection.
4. **The project is healthy for its size but has one maintainer**, as at #309.

**Parts worth taking, without adopting the server** (each is the user's call):

- **`EC_SIGN_SECP256K1_SHA256`**, the one algorithm it has and CloudBurrow lacks. It uses
  `github.com/decred/dcrd/dcrec/secp256k1/v4` with low-S normalisation
  ([secp256k1.go][kmsemu-secp]). Serving it would mean adding that module to CloudBurrow's
  `go.mod`, which needs the user's approval; it would be a new ticket, written against
  Google's docs, not a port.
- **Error wording.** If the user wants it, a recorded observation of Cloud KMS's
  "is not enabled, current state is:" and "has incorrect key purpose" messages would settle
  whether CloudBurrow should match them.

**Not measured, for the user:** whether their server behaves as read here when driven by
the official client (only running it would show); image size, idle RSS and start time;
whether their import accepts Google's AES-KWP envelope. All three need their code run, with
the user's approval.

**Revisit** if a later release serves the other six services, honours
`destroy_scheduled_duration` and reaches DESTROYED, and exposes its store for reset and
snapshots.

[kmsemu]: https://github.com/blackwell-systems/gcp-kms-emulator/tree/5372c0046ce0c253717a0d77a127b01e70a9658d
[kmsemu-register]: https://github.com/blackwell-systems/gcp-kms-emulator/blob/5372c0046ce0c253717a0d77a127b01e70a9658d/register.go#L31
[kmsemu-gw-root]: https://github.com/blackwell-systems/gcp-kms-emulator/blob/5372c0046ce0c253717a0d77a127b01e70a9658d/gateway.go
[kmsemu-dual]: https://github.com/blackwell-systems/gcp-kms-emulator/blob/5372c0046ce0c253717a0d77a127b01e70a9658d/cmd/server-dual/main.go#L96
[kmsemu-readme-ops]: https://github.com/blackwell-systems/gcp-kms-emulator/blob/5372c0046ce0c253717a0d77a127b01e70a9658d/README.md?plain=1#L117
[kmsemu-features]: https://github.com/blackwell-systems/gcp-kms-emulator/blob/5372c0046ce0c253717a0d77a127b01e70a9658d/docs/FEATURES.md
[kmsemu-gomod]: https://github.com/blackwell-systems/gcp-kms-emulator/blob/5372c0046ce0c253717a0d77a127b01e70a9658d/go.mod#L5-L13
[kmsemu-docker]: https://github.com/blackwell-systems/gcp-kms-emulator/blob/5372c0046ce0c253717a0d77a127b01e70a9658d/.github/workflows/docker-publish.yml#L60
[kmsemu-decap]: https://github.com/blackwell-systems/gcp-kms-emulator/blob/5372c0046ce0c253717a0d77a127b01e70a9658d/internal/server/server.go#L463-L470
[kmsemu-iam]: https://github.com/blackwell-systems/gcp-kms-emulator/blob/5372c0046ce0c253717a0d77a127b01e70a9658d/internal/server/server.go#L103-L147
[kmsemu-createring]: https://github.com/blackwell-systems/gcp-kms-emulator/blob/5372c0046ce0c253717a0d77a127b01e70a9658d/internal/server/server.go#L150-L167
[kmsemu-server-create]: https://github.com/blackwell-systems/gcp-kms-emulator/blob/5372c0046ce0c253717a0d77a127b01e70a9658d/internal/server/server.go#L224-L227
[kmsemu-encrypt]: https://github.com/blackwell-systems/gcp-kms-emulator/blob/5372c0046ce0c253717a0d77a127b01e70a9658d/internal/server/server.go#L272-L279
[kmsemu-gateway]: https://github.com/blackwell-systems/gcp-kms-emulator/blob/5372c0046ce0c253717a0d77a127b01e70a9658d/internal/gateway/gateway.go#L58
[kmsemu-paginate]: https://github.com/blackwell-systems/gcp-kms-emulator/blob/5372c0046ce0c253717a0d77a127b01e70a9658d/internal/storage/storage.go#L55-L82
[kmsemu-createkey]: https://github.com/blackwell-systems/gcp-kms-emulator/blob/5372c0046ce0c253717a0d77a127b01e70a9658d/internal/storage/storage.go#L202-L282
[kmsemu-encrypt-storage]: https://github.com/blackwell-systems/gcp-kms-emulator/blob/5372c0046ce0c253717a0d77a127b01e70a9658d/internal/storage/storage.go#L304-L313
[kmsemu-decrypt]: https://github.com/blackwell-systems/gcp-kms-emulator/blob/5372c0046ce0c253717a0d77a127b01e70a9658d/internal/storage/storage.go#L370-L382
[kmsemu-getversion]: https://github.com/blackwell-systems/gcp-kms-emulator/blob/5372c0046ce0c253717a0d77a127b01e70a9658d/internal/storage/storage.go#L527-L540
[kmsemu-updateversion]: https://github.com/blackwell-systems/gcp-kms-emulator/blob/5372c0046ce0c253717a0d77a127b01e70a9658d/internal/storage/storage.go#L583-L607
[kmsemu-destroy]: https://github.com/blackwell-systems/gcp-kms-emulator/blob/5372c0046ce0c253717a0d77a127b01e70a9658d/internal/storage/storage.go#L627-L672
[kmsemu-restore]: https://github.com/blackwell-systems/gcp-kms-emulator/blob/5372c0046ce0c253717a0d77a127b01e70a9658d/internal/storage/storage.go#L674-L703
[kmsemu-updatekey]: https://github.com/blackwell-systems/gcp-kms-emulator/blob/5372c0046ce0c253717a0d77a127b01e70a9658d/internal/storage/storage.go#L722-L737
[kmsemu-keyproto]: https://github.com/blackwell-systems/gcp-kms-emulator/blob/5372c0046ce0c253717a0d77a127b01e70a9658d/internal/storage/storage.go#L751-L768
[kmsemu-errors]: https://github.com/blackwell-systems/gcp-kms-emulator/blob/5372c0046ce0c253717a0d77a127b01e70a9658d/internal/storage/errors.go#L46-L56
[kmsemu-keymat]: https://github.com/blackwell-systems/gcp-kms-emulator/blob/5372c0046ce0c253717a0d77a127b01e70a9658d/internal/storage/keymaterial.go#L45-L146
[kmsemu-asymsign]: https://github.com/blackwell-systems/gcp-kms-emulator/blob/5372c0046ce0c253717a0d77a127b01e70a9658d/internal/storage/asymmetric.go#L187-L210
[kmsemu-asymdecrypt]: https://github.com/blackwell-systems/gcp-kms-emulator/blob/5372c0046ce0c253717a0d77a127b01e70a9658d/internal/storage/asymmetric.go#L276
[kmsemu-raw]: https://github.com/blackwell-systems/gcp-kms-emulator/blob/5372c0046ce0c253717a0d77a127b01e70a9658d/internal/storage/raw.go
[kmsemu-random]: https://github.com/blackwell-systems/gcp-kms-emulator/blob/5372c0046ce0c253717a0d77a127b01e70a9658d/internal/storage/random.go#L12
[kmsemu-persist]: https://github.com/blackwell-systems/gcp-kms-emulator/blob/5372c0046ce0c253717a0d77a127b01e70a9658d/internal/storage/persistence.go#L91-L160
[kmsemu-secp]: https://github.com/blackwell-systems/gcp-kms-emulator/blob/5372c0046ce0c253717a0d77a127b01e70a9658d/internal/storage/secp256k1.go
[g-encrypt]: https://github.com/googleapis/googleapis/blob/0b8cd308f11dda6e1adb1a6df69e82e3227dc361/google/cloud/kms/v1/service.proto#L1324-L1330
[g-primary]: https://github.com/googleapis/googleapis/blob/0b8cd308f11dda6e1adb1a6df69e82e3227dc361/google/cloud/kms/v1/resources.proto#L130-L133
[g-dsd]: https://github.com/googleapis/googleapis/blob/0b8cd308f11dda6e1adb1a6df69e82e3227dc361/google/cloud/kms/v1/resources.proto#L199-L200
[g-kms127]: https://github.com/googleapis/google-cloud-go/blob/kms/v1.27.0/kms/apiv1/kmspb/service_grpc.pb.go
