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
- **Crypto:** Go standard library (`crypto/aes`, `crypto/cipher` GCM, and `hash/crc32`
  with the Castagnoli table; since #1946 `crypto/mldsa`, `crypto/mlkem`, `crypto/ecdh` and
  `crypto/sha3` for ML-DSA, ML-KEM and X-Wing). There is no Tink on the server. SLH-DSA, which
  Go lacks and Google offers only inside Tink, is Cloudflare's CIRCL v1.6.5 (BSD-3-Clause),
  copied into `third_party/circl-slhdsa` with one constructor added for a client's SHA-256
  digest ([PROVENANCE.md](../third_party/circl-slhdsa/PROVENANCE.md)).
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
It was built and run against CloudBurrow's KMS compat suite on 2026-11-19
([amendment](#amendment-gcp-kms-emulator-v0102-run-against-cloudburrows-kms-compat-suite-1712)).

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

*Superseded in part on 2026-11-19 by a measured run ([amendment](#amendment-gcp-kms-emulator-v0102-run-against-cloudburrows-kms-compat-suite-1712)); it corrects the `UpdateCryptoKeyPrimaryVersion` agreement below and answers the "not measured" items.*

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

- **`EC_SIGN_SECP256K1_SHA256`**, the one algorithm it had and CloudBurrow lacked. It uses
  `github.com/decred/dcrd/dcrec/secp256k1/v4` with low-S normalisation
  ([secp256k1.go][kmsemu-secp]). Serving it meant adding that module to CloudBurrow's
  `go.mod`, which needed the user's approval; it would be a new ticket, written against
  Google's docs, not a port. **Since #1719 CloudBurrow serves it** (the user approved the
  module, v4.4.1, on 2026-11-17), written against Google's algorithms page: HSM and
  HSM_SINGLE_TENANT keys only, lower-S DER signatures, and a secp256k1
  SubjectPublicKeyInfo from `GetPublicKey`. That algorithm gap is closed.
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

## Amendment: gcp-kms-emulator v0.10.2 run against CloudBurrow's KMS compat suite (#1712)

Recorded 2026-11-19. **Evidence and a recommendation; the decision is the user's.** The
[Cloud KMS amendment](#amendment-cloud-kms-is-built-not-reused-309) stands until the user
changes it.

**Why a second pass.** The [2026-11-17 amendment](#amendment-gcp-kms-emulator-v0102-re-evaluated-1712)
read their code without running it, and left three questions open: does the server behave
as read when the official client drives it; how big and how heavy is it; does its import
accept Google's AES-KWP envelope. Running third-party code is now allowed for parity work,
so this pass builds the server, runs it and answers all three.

**Method.**
- Source: the `v0.10.2` tag tarball from the project's own GitHub (commit [`5372c004`][kmsemu],
  the newest commit on its default branch on 2026-11-19), unpacked into an empty scratch
  directory. `LICENSE` is Apache-2.0. The Makefile, Dockerfile and `cmd/server-dual` were
  read before building. Nothing in them runs scripts, uses cgo or `go:generate`, or
  calls the network at start-up. `IAM_MODE` defaults to `off`.
- Build: `go build ./cmd/server-dual` (Go 1.27, darwin/arm64). Run on loopback ports 29200
  (gRPC) and 29201 (REST), with `GCP_KMS_DATA_DIR` set to a scratch directory.
- Tests: CloudBurrow's own official-SDK suite, `go test -tags compat -run '^TestKMS'
  ./test/compat/` with `CLOUDBURROW_TEST_KMS` pointed at their gRPC port. A second run pointed
  the `rest` subtests at their REST port, because their server splits the two protocols
  across two ports and CloudBurrow serves both on one. No other `CLOUDBURROW_TEST_*`
  variable was set, so tests that need Resource Manager, the control plane, the CLI or a
  restart skipped. No gcloud or Terraform test ran.
- Baseline: the same 52 `TestKMS*` tests on CloudBurrow in CI
  ([run 44560144998](https://github.com/cloudburrow/cloudburrow/actions/runs/44560144998),
  main `6f115532`, "served" shard). 50 passed. The two restart tests skip in that step
  because CI runs them in its own stop-and-start step. No KMS test failed.
- Afterwards, the download, the binary and the state directory were deleted. No container
  or image was made: the image facts below come from the registry manifest.

### Test results

| Run | Passed | Failed | Skipped (no endpoint) |
|---|---|---|---|
| CloudBurrow, CI baseline | 50 top-level tests | 0 | 2 (restart tests run in their own CI step) |
| gcp-kms-emulator, gRPC cases (49) | 9 | 33 | 7 |
| gcp-kms-emulator, REST cases (29) | 2 | 18, plus 9 that could not run | — |

The 9 REST cases that could not run do gRPC setup on the REST port, so they time out.
That is the two-port split, not a measured difference. Two of the 18 REST failures
(`TestKMSReadsAgreeOverREST`, `TestKMSJSONIsServedOnTheSamePort`) fail only because REST
is on a second port.

**Passed against it:** `TestKMSResources` (gRPC create, get, list, primary move),
`TestKMSGetCryptoKeyVersion`, `TestKMSRestoreCryptoKeyVersion` (gRPC and REST),
`TestKMSMacSignAndVerify` (gRPC and REST), `TestKMSTinkEnvelopeEncryption` (gRPC),
`TestKMSFullViewAddsNothingForSoftwareKeys`, `TestKMSOtherServicesAreUnimplemented`,
`TestKMSUnimplementedContract`.

**Failed, by cause** (their error first, then what the test expects; CloudBurrow passes each):

| Cause | Tests | Their response → expected |
|---|---|---|
| Service not served | `EkmConnectionsAreStored`, `EkmIamPolicyIsStoredNotEnforced`, `SingleTenantHsm`, `TrustedKeyWrapping`, `IamPolicyIsStoredNotEnforced`, `Locations` | gRPC UNIMPLEMENTED "unknown service google.cloud.kms.v1.EkmService" (likewise `HsmManagement`, `google.iam.v1.IAMPolicy`, `google.cloud.location.Locations`); REST 404 "Not Found" |
| Method not served | `DeleteKeyRing`, `DeleteCryptoKeyLeavesARetiredResource` | UNIMPLEMENTED; REST **501 "Method Not Allowed"** |
| Algorithm not served | `AsymmetricSignVerifiesWithThePublicKeyPEM` (PSS), `RawEncryptAndDecrypt` (CBC, CTR), `DecapsulateMLKEM`, `ExternalKeys`, `OutOfScopeKeyOptionsAreUnimplemented` (ML-DSA, X-Wing) | **INTERNAL** / REST 500 "failed to generate key material: unsupported algorithm: …" → created, or UNIMPLEMENTED or INVALID_ARGUMENT naming it |
| Requested state ignored | `VersionCanStartDisabled`, `PrimaryMustBeEnabled`, `VersionStateFilter` | `CreateCryptoKeyVersion` with `state: DISABLED` returns ENABLED ([storage.go:461][kmsemu-createversion]). So "make a DISABLED version primary" succeeds and moves the primary. Their `UpdateCryptoKeyPrimaryVersion` does check state ([storage.go:508][kmsemu-primary]); it never sees a DISABLED version. |
| Destroy scheduling | `DestroyCryptoKeyVersion` (gRPC and REST) | `destroy_time` is 30 days out, not the key's 24 h `destroy_scheduled_duration`; destroying again succeeds → FAILED_PRECONDITION |
| Versions start ENABLED | `AsymmetricVersionsStartPendingGeneration` (gRPC and REST) | ENABLED with `generate_time` → PENDING_GENERATION |
| Create fields dropped | `CreateCryptoKeyFields`, `RotationSchedule` (gRPC and REST), `OutOfScopeKeyOptionsAreUnimplemented` (`import_only`, `crypto_key_backend`, `AES_WRAPPING`) | Accepted and dropped: `destroy_scheduled_duration` reads back as 30 days, `rotation_period` 12 h is accepted and reads back empty, `ASYMMETRIC_SIGN` with no algorithm succeeds |
| Protection level | `HSMKeysAndKeyAccessJustifications` (gRPC and REST), `Secp256k1SignsAndVerifiesWithThePublicKey` (gRPC and REST) | An HSM key reports SOFTWARE, and its `key_access_justifications_policy` is dropped. A SOFTWARE or unspecified secp256k1 key is created; Google offers secp256k1 at HSM only → INVALID_ARGUMENT |
| `Encrypt`/`Decrypt` | `Encrypt`, `Decrypt`, `DecryptFollowsTheVersionLifecycle` | CRC32C mismatch: INVALID_ARGUMENT "request corrupted in transit", a message that does not name the field; Decrypt given a version name → NOT_FOUND; a DISABLED or DESTROY_SCHEDULED version's ciphertext → INVALID_ARGUMENT "Decryption failed: the ciphertext is invalid." instead of FAILED_PRECONDITION |
| Caller errors still INTERNAL | `AsymmetricDecryptOpensWhatThePublicKeyEncrypted` (gRPC and REST) | Junk ciphertext → **INTERNAL "crypto/rsa: decryption error"** / REST 500 → INVALID_ARGUMENT |
| Codes that differ | `UpdateCryptoKeyVersion` (gRPC and REST), `UpdateCryptoKey` (gRPC and REST), `GenerateRandomBytes`, `NamesAreValidatedBeforeLookup` | A mask without `state` → FAILED_PRECONDITION; `destroy_scheduled_duration` updated although IMMUTABLE; 4 bytes and SOFTWARE random accepted; a malformed name → NOT_FOUND |
| Import | `ImportManuallyWrappedKeys` (gRPC and REST) | A key wrapped as `RSA_OAEP_3072_SHA1_AES_256` documents (RSA-OAEP-SHA1 of an AES key, then AES-KWP) → INVALID_ARGUMENT "failed to unwrap key". Their import does plain RSA-OAEP-SHA256 of the target key, whatever the job's `import_method` says ([importjob.go:152][kmsemu-unwrap]). So keys wrapped as Google documents, or by `gcloud kms keys versions import`, do not import. |
| Lists | `NameAndLabelFilters`, `OrderByName` | `filter` and `order_by` ignored. A list reports `"totalSize":0` beside two rings. |

The three open questions from 2026-11-17 are now answered. The server behaves as read,
with one correction: the earlier amendment listed `UpdateCryptoKeyPrimaryVersion` to a
non-ENABLED version among the cases where the two agree. The check is there, but a version
created DISABLED is stored ENABLED, so in practice the call succeeds. Import does not
accept Google's envelope. Footprint is in the table below.

### Comparison

| Criterion | gcp-kms-emulator v0.10.2 (measured 2026-11-19) | CloudBurrow built-in |
|---|---|---|
| Services | `KeyManagementService` only | All seven KMS services, 63 methods ([coverage/kms.md](coverage/kms.md)) |
| Methods | 28 of the 36 `KeyManagementService` methods; delete, retired resources, Decapsulate and trusted wrapping missing | All 36 |
| Purposes and algorithms | Symmetric, RSA PKCS#1 sign, EC P-256, P-384, secp256k1 (at any protection level), Ed25519, RSA-OAEP decrypt, HMAC, raw AES-GCM. Anything else is INTERNAL at create. | All of those, plus PSS, raw PKCS#1, AES-CBC and CTR, ML-KEM-768, AES-256-KWP wrapping; secp256k1 at HSM only (#1719) |
| Versions | Created ENABLED whatever is asked; no PENDING_GENERATION; destroy fixed at 30 days and never reaches DESTROYED | Requested state honoured; PENDING_GENERATION; key's duration; swept to DESTROYED |
| Import | Plain RSA-OAEP-SHA256 only; Google's AES-KWP envelope refused | Google's envelope (Verified) |
| MAC | Passes CloudBurrow's test, gRPC and REST | Verified |
| Raw encryption | GCM only | GCM, CBC, CTR |
| EKM, Autokey, HSM management | Not served | Verified |
| Errors | NOT_FOUND, ALREADY_EXISTS, the purpose and version-state FAILED_PRECONDITIONs match. Unsupported algorithms and RSA decrypt failures are INTERNAL/500; unserved REST methods are 501 "Method Not Allowed" | Per the dated rows in [coverage/kms.md](coverage/kms.md#unverified-error-codes) |
| Persistence | Measured: state survived a restart through `GCP_KMS_DATA_DIR`. One `state.json` (0600), rewritten whole after each change, with 22 `privateKeyDer` fields in plaintext after the run. No reset and no snapshot. | Kubernetes Secrets, restart Verified, `/admin/reset` per project, `state save`/`load` |
| Protocols | gRPC and REST on two ports; both listen on all interfaces (`[::]:29200`, `:29201`), with no bind-address flag | One port, the instance's bind address |
| Licence | Apache-2.0 (checked in the tarball's `LICENSE`) | — |
| Maintenance | No commits since v0.10.2 (2026-11-04); 2 contributors, one in practice; 9 stars, 0 open issues; it answered #1690 with a fix | — |
| Architectures and footprint | Registry manifest for `0.10.2`: `linux/amd64` and `linux/arm64`, Alpine, 22.1 MB compressed (arm64). The published image is the **gRPC-only** variant (the Dockerfile's default `VARIANT=grpc`); REST needs a custom build. No release binaries. The darwin/arm64 dual binary is 21.9 MB; RSS was 18 MB after the run and 23 MB after a restart that loaded state; it was ready in under a second. | Inside the CloudBurrow binary, every platform it ships |
| Embedding | Public API is `Register(*grpc.Server)` and `NewGatewayHandler(grpcAddr)`; storage is `internal/`. In process: a new module (plus `gcp-emulator-auth` and `grpc-gateway`) and a second store beside CloudBurrow's. Out of process: a container or child process on two more ports. Either way, no reset, save/load, Kubernetes storage, fake clock, fault injection or in-process access for the console and CMEK callers. | Called in process (`cmd/cloudburrow`): the console's KMS pages, Secret Manager CMEK, Private CA's KMS-backed keys and IAM enforcement |

### Recommendation: keep the built-in

1. **Measured coverage.** Against CloudBurrow's KMS suite it passes 9 of 49 gRPC cases and
   2 of 20 REST cases that could run. CloudBurrow passes every one. Each failure has a
   cause in the table above, and none comes from the harness alone, apart from the 9 REST
   cases noted.
2. **The departures a user would inherit are now observed, not inferred.** Requested
   version states are ignored, destroy scheduling is fixed and never completes, HSM keys
   say SOFTWARE, and create fields are dropped with success. Google-wrapped imports are
   refused, and three classes of caller error are INTERNAL. The #309 amendment's claim
   that each candidate departs from Google's documented behaviour holds on measured
   evidence.
3. **Adopting it would not remove any CloudBurrow code.** Six services, eight methods,
   the algorithms it lacks, reset, snapshots and the in-process CMEK callers would all stay
   CloudBurrow's. Adoption would add a second store, a second port and a new dependency.
4. **Adopt for parts: nothing left to take.** Its one unique feature, secp256k1, has been
   CloudBurrow's since #1719, and CloudBurrow serves it at HSM only, as Google documents.
   MAC and the basic symmetric path, the parts that pass, are already Verified in
   CloudBurrow. Its error wording ("… is not enabled, current state is: DISABLED.") is
   still unconfirmed against Google, so there is nothing to copy there either.

**For the user to decide:** keep (recommended), adopt, or adopt for parts. **Revisit** if a
release serves the EKM, Autokey and HSM management services, honours requested version
states and `destroy_scheduled_duration`, accepts Google's import envelope and exposes its
store for reset and snapshots.

[kmsemu-createversion]: https://github.com/blackwell-systems/gcp-kms-emulator/blob/5372c0046ce0c253717a0d77a127b01e70a9658d/internal/storage/storage.go#L455-L466
[kmsemu-primary]: https://github.com/blackwell-systems/gcp-kms-emulator/blob/5372c0046ce0c253717a0d77a127b01e70a9658d/internal/storage/storage.go#L508-L510
[kmsemu-unwrap]: https://github.com/blackwell-systems/gcp-kms-emulator/blob/5372c0046ce0c253717a0d77a127b01e70a9658d/internal/storage/importjob.go#L152

## Amendment: an ORC reader module for BigQuery's ORC loads (#1713)

BigQuery's ORC loads needed a Go reader of ORC files, and none was in `go.mod` (Apache Arrow's
Go module reads Parquet and Avro, not ORC). The user approved adding one module (2026-11-17).
The candidates on the Go module proxy, compared on 2026-11-17:

| Module | Last version | Licence | CGO | Its own dependencies | Verdict |
|---|---|---|---|---|---|
| `github.com/scritchley/orc` | v0.0.0-20210513144143-06dddf1ad665 | MIT | none | `golang/protobuf`, `golang/snappy`, both already in CloudBurrow's module graph | **Adopted** |
| `github.com/crphang/orc` | v0.0.6 (2021-03) | MIT | none | the same | A fork of the above, pinning old versions in its `go.mod`; no reason to prefer it |
| `github.com/Max-Cheng/orc` | 2024-10 pseudo-version | MIT | none | imports `github.com/scritchley/orc/proto`, so it needs that module too, and `klauspost/compress` | Rejected: two new modules, no `go.mod` of its own |
| `github.com/arussellsaw/orc`, `github.com/graphaelli/gorc`, `github.com/athum/orc` | 2017–2019 | various | none | — | Older forks or stubs |

No Apache-maintained Go ORC reader exists (Apache ORC ships Java and C++). `scritchley/orc` is the
most complete and the original the forks derive from. It is inactive since 2021, so CloudBurrow
relies on its stable parts and works round its known faults rather than its high-level reader:
it uses the module's protocol buffer messages and column decoders (run-length integers v1 and v2,
byte and boolean run lengths, direct and dictionary strings, floats, timestamps), and reads the
file's tail, decompresses each stream and puts the column readers together itself
(`internal/bigqueryfront/orcfile.go`). That avoids the module's reader's limits: it reads only
NONE, ZLIB and SNAPPY (CloudBurrow adds ZSTD with `klauspost/compress`, LZ4 with
`pierrec/lz4/v4`, both already in the graph, and LZO with a decoder of its own), reads a NULL DATE
as 1970-01-01, reads a decimal past 63 bits wrongly, reads map entries one ahead after the first
row, fails an empty BINARY at a stream's end, and counts timestamps from 2015-01-01 UTC rather than
in the writer's zone. Each workaround is checked against Apache ORC's example files and the rows
ORC's tools print for them (`TestORCExamples`). A corrupt file's panic in a decoder is recovered
as a 400. `govulncheck ./...` reports nothing for it, and adding it added no other module to the
graph (`golang/protobuf` moved from the graph into `go.mod`'s list because the module has no
`go.mod`).

## Amendment: an MP3 decoder module for Vertex AI EmbedContent audio (#1721)

Google documents "MP3, WAV" as `gemini-embedding-2`'s audio formats, and its samples send
`mime_type` `audio/mpeg`. Go's standard library has no MP3 decoder, and none was in `go.mod`. The
user approved adding one module (2026-11-17). The candidates on the Go module proxy, compared on
2026-11-17:

| Module | Last version | Licence | CGO | Its own dependencies | Verdict |
|---|---|---|---|---|---|
| `github.com/hajimehoshi/go-mp3` | v0.3.4 (2022-11) | Apache 2.0 | none | none in the decoder's packages (`go.mod` names `hajimehoshi/oto/v2`, used only by its example, so `go.sum` gains that module's `go.mod` hash and one of an old `golang.org/x/sys`, and no code) | **Adopted** |
| `github.com/gopxl/beep/v2` (`mp3`) | v2.1.1 (2025-01) | MIT | none | wraps `hajimehoshi/go-mp3`, and brings `oto/v3`, `tcell`, an Ogg Vorbis and a FLAC decoder | Rejected: the same decoder behind a playback library |
| `github.com/tosone/minimp3` | v1.1.0 (2026-10) | MIT (minimp3: CC0) | yes | none | Rejected: cgo binding of the C minimp3 |
| `github.com/kvark128/minimp3` | 2022-04 pseudo-version | MIT | yes | none | Rejected: cgo binding of the C minimp3 |

`go-mp3` is the pure Go MP3 decoder the Ebitengine game library ships (its author's), and the one
the other Go audio libraries wrap. It decodes MPEG-1 and MPEG-2 Layer III (16 to 48 kHz; mono,
stereo, joint and intensity stereo) to 16-bit stereo PCM, and scans the frames first, so a clip's
length is known before it is decoded. It is quiet since 2022, so CloudBurrow uses only its
`NewDecoder`, `SampleRate`, `Length` and `Read`, and does itself what it leaves out
(`internal/embedding/mp3.go`): it reads the first frame's header for the channel mode (the decoder
copies a mono stream to both channels; a mono stream is read back as one channel, a stereo one
downmixed as a stereo WAV is), and the Info/Xing frame's LAME tag for the encoder's delay and
padding, trimmed as ffmpeg trims them (the Info frame, then the delay plus the decoder's 529
samples, and the padding at the end), so an MP3 lines up sample for sample with the WAV it was
encoded from. A panic of the decoder on a corrupt frame is recovered as `INVALID_ARGUMENT`.
MPEG-2.5 (8, 11.025 and 12 kHz), which `go-mp3` refuses, was refused naming it until #1734
(below). Checked against ffmpeg 8: the same 16 kHz mono MP3 decodes to the same samples within one
16-bit step (`TestDecodeMP3`). `govulncheck ./...` (v1.8.0) reports nothing in it.

### MPEG-2.5 (#1734): go-mp3 copied into third_party and extended

MPEG-2.5 is the unofficial extension of MPEG-2's low sample rates that LAME uses below 16 kHz (8,
11.025 and 12 kHz, for low bit rates). No other decoder on the Go module proxy fits (the table
above: the others are cgo bindings or wrap `go-mp3`), and `go-mp3` is quiet since 2022, so its
decoder is copied into `third_party/go-mp3` (Apache 2.0 allows it; the changed files say so) and
extended there, and the module leaves `go.mod`. MPEG-2.5 differs from MPEG-2 only in its sample
rates (MPEG-1's quartered), its scalefactor band tables and, at 8 kHz, the wider bands where a short
block's first Huffman region and a mixed block's long part end; ffmpeg's decoder is the reference
for each ([PROVENANCE.md](../third_party/go-mp3/PROVENANCE.md)). Checked against ffmpeg 9 on
libmp3lame streams with start and short blocks: 8 kHz mono within one 16-bit step, 11.025 and 12
kHz joint stereo within two a channel (`TestDecodeMPEG25MP3`), and through the official SDK
(`TestEmbedContentEmbedsMPEG25MP3ThroughThePredictionClient`).

## Amendment: HEIC, HEIF, AVIF and animated WebP decoders for Vertex AI EmbedContent (#1724)

Google's [Get multimodal embeddings](https://docs.cloud.google.com/vertex-ai/generative-ai/docs/embeddings/get-multimodal-embeddings)
(re-read 2026-11-17) lists "JPEG, PNG, WebP, BMP, HEIC, HEIF, AVIF" for `gemini-embedding-2`, and
says nothing of animation. `golang.org/x/image` (#1651) decodes still WebP and BMP only. The user
approved adding the module(s) needed for HEIC, HEIF, AVIF and animated WebP (2026-11-17), pure Go
preferred: the storage image and the default build are `CGO_ENABLED=0`, and the release CLIs are
statically linked ([install.md](install.md#how-the-cli-is-linked)). The candidates on the Go module
proxy, compared on 2026-11-17:

| Module | Last version | Licence | How it decodes | Its own dependencies | Verdict |
|---|---|---|---|---|---|
| `github.com/gen2brain/h265` | v0.2.5 (2026-11-11) | MIT | Pure Go HEVC decoder and HEIF container reader (a port of the Rust `rust_h265`/`oxideav-h265`; byte-exact on the JCT-VC HEVC v1 conformance suite, per its README); Go assembly for NEON/AVX2, `-tags noasm` for none | **none** | **Adopted** for HEIC and HEIF: 2.3 MB of source, adds no other module, decodes alpha, grids, `clap`/`irot`/`imir`, 8–16 bits and image sequences |
| `github.com/gen2brain/heic` | v0.7.2 (2026-09-15) | MIT (the Rust `heic` crate in WASM) | WASM under wazero; but first tries a system `libheif` through `purego` | `ebitengine/purego`, `tetratelabs/wazero`, `golang.org/x/sys` | Rejected: see `purego` below; its own README points to `h265` for pure Go |
| `github.com/gen2brain/avif` | v0.6.0 (2026-07-05) | MIT; libavif BSD-2-Clause, dav1d BSD-2-Clause, aom BSD-2-Clause | libavif + dav1d in WASM (3.5 MB, 1.3 MB gzipped) under wazero; but first tries a system `libavif` through `purego`, unless built with `-tags nodynamic` | `ebitengine/purego`, `tetratelabs/wazero`, `golang.org/x/sys` | **Not adopted as a module; its WASM path vendored** (#1729, below) |
| `github.com/KarpelesLab/goavif` | v0.1.0 (2026-04-18) | MIT | Pure Go AV1 and AVIF | — | Rejected: unmaintained (created and last pushed within three days in April 2026, one star; inter prediction listed as limited) |
| `github.com/Kagami/go-avif`, `github.com/f0reth/go-avif` | 2022; — | — | libaom through cgo; libavif through `purego` | — | Rejected: cgo, or a system library |

**Why AVIF is not served.** `gen2brain/avif` (and `gen2brain/heic`) import `ebitengine/purego` in
every build without `-tags nodynamic`, to load a system libavif when one is installed. With
`CGO_ENABLED=0`, `purego` links its `fakecgo` runtime shim and imports libc dynamically: a probe
binary that only imports `gen2brain/avif`, built `CGO_ENABLED=0 GOOS=linux`, is "dynamically
linked, interpreter /lib/ld-linux-aarch64.so.1", and statically linked with `-tags nodynamic`. So
adding it would silently break the static Linux CLIs (the release check reads only the recorded
`CGO_ENABLED=0`, which still passes), and the decode would depend on whichever libavif the host has.
Serving AVIF needs one of: `-tags nodynamic` on every build of `cmd/cloudburrow` (release, Makefile,
CI, `go install` instructions) with a check that the Linux CLI is static; or a vendored copy of
its WASM path without `purego`; or a maintained pure Go AV1 decoder. That decision is the user's;
`image/avif` stays `INVALID_ARGUMENT` naming the field until then.

**Update 2026-11-17 (#1729): the WASM path is vendored.** The user chose the second: serve AVIF
fully by vendoring `gen2brain/avif` v0.6.0's WebAssembly decode path, with no build tag. From the
module zip the Go module proxy serves (commit `1fd2a9a5`), `avif.go`, `avif_wazero.go`,
`isobmff.go`, `orientation.go`, `lib/avif.wasm.gz` (libavif 1.4.2, dav1d 1.5.3, aom 3.14.1 and
libyuv, built upstream with wasi-sdk; CloudBurrow compiles no C) and the MIT, libavif and aom licence
texts are copied into `internal/avif`, unmodified but for deletions: the `if dynamic` branches that
called the `purego` loader (whose files, `avif_dynamic.go` and `purego_*.go`, are not copied), the
exported `Dynamic()`, and `avif_wazero.go`'s `!wasm2go` build constraint.
`third_party/avif/PROVENANCE.md` (deleted with the decoder in #1736; in git history) records every file's upstream
and committed SHA-256, and `TestVendoredAVIFMatchesProvenance` checks them. No module is added:
`github.com/tetratelabs/wazero` v1.12.0 (Apache-2.0), already required for go-pdfium (#1723, below),
becomes a direct requirement. `TestLinuxBinariesNeverImportPurego` (#1722)
asserts that `CGO_ENABLED=0 GOOS=linux go list -deps ./cmd/cloudburrow` names no
`github.com/ebitengine/purego`, and the linux/amd64 and linux/arm64 CLIs build "statically linked"
(`file`, 2026-11-17). The stripped linux/amd64 CLI grows by 1,335,296 bytes (348.0 to 349.4 MB) and the linux/arm64 one by
1,376,256 bytes (340.5 to 341.9 MB; the 1.3 MB blob and the glue; wazero was already linked for PDFium). The blob also
holds aom's encoder, unused here, and links libyuv, whose BSD-3-Clause text the module does not
ship (NOTICE names it). A 10- or 12-bit AVIF over 13312 × 13312 does not fit the module's 4 GiB of
memory (#1736). `govulncheck ./...` reports nothing for wazero or the vendored code.

**Animated WebP** needs no module: the first `ANMF` frame's VP8/VP8L bitstream (with its `ALPH`) is
rewrapped as a still WebP, decoded with `golang.org/x/image/webp`, converted as libwebp converts it
(`internal/embedding/webp.go`) and placed on a transparent canvas (`internal/embedding/webpanim.go`).
Google's page does not say how an animated image is treated, and documents no refusal; CloudBurrow
embeds the first frame, the picture a still-image reader shows (an image sequence in a HEIC is
taken the same way), checked pixel for pixel against libwebp 1.6's `dwebp` of that frame.

`h265`'s HEIC decode upsamples 4:2:0 chroma bilinearly (libavif's 9-3-3-1 weights), matching
`heif-dec -C bilinear` (libheif 1.23.7) within 0.05 per channel on the unit fixture; libheif's
default is nearest-neighbour. Its `go.mod` says `go 1.26.4`, so CloudBurrow's `go` line moves from
1.26.0 to 1.26.4 (CI builds with 1.26.8 and 1.27). `govulncheck ./...` reports nothing for it.

## Amendment: video decoders and a PDF renderer for EmbedContent (#1722, #1723)

EmbeddingGemma 2 embeds a video as frames through its vision encoder, and Google's
[multimodal embeddings page](https://docs.cloud.google.com/vertex-ai/generative-ai/docs/embeddings/get-multimodal-embeddings)
(re-read 2026-11-17) takes, for `gemini-embedding-2`, MP4 and MOV video in AV1, H.264, H.265 or
VP9 (1 video a request, up to 120 frames, 1 frame a second unless `video_metadata.fps` says
otherwise, `start_offset` and `end_offset` bounding it; `audio_track_extraction` off by
default), and one PDF of up to 6 pages a request, each page "rendered as an image", with
`document_ocr` adding the text OCR reads. The user approved adding the modules needed
(2026-11-17). CloudBurrow's default build and its storage image have cgo off, so a decoder must be
pure Go or WebAssembly. The candidates on the Go module proxy, compared on 2026-11-17:

| Need | Module | Version | Licence | cgo | Covers | Verdict |
|---|---|---|---|---|---|---|
| MP4/MOV demuxing | `github.com/Eyevinn/mp4ff` | v0.63.0 (2026-11-13) | MIT | none | progressive and fragmented ISO BMFF, `avcC`, `hvcC`, edit lists | **Adopted** (its only requirement, `go-test/deep`, is for its tests) |
| H.264 | `github.com/oops1/go.264` | v1.14.0 (2026-11-10) | BSD 2-Clause | none, but its Linux hardware encoders import `ebitengine/purego`, which links libc dynamically into a `CGO_ENABLED=0` Linux binary | High profile, CABAC and CAVLC, P and B slices, weighted prediction, progressive 8-bit 4:2:0; bit-exact against ffmpeg on 29 clips (its claim) | **Adopted**, copied into `third_party/go264` (its sources less `hwaccel_linux.go` and the NVENC and VA-API backends, imports rewritten, so every build stays static without `-tags go264_nohwaccel` and `go install ...@latest` still works; `TestLinuxBinariesNeverImportPurego`): measured bit-exact here too (70 dB PSNR, the YUV-to-RGB rounding, against ffmpeg on the model card's 720p High-profile clip with B-frames) |
| H.264 | `github.com/go-avkit/h264` | v0.10.0 | BSD 3-Clause | none | go.264's decoder imported, 69 of 73 JVT streams | Rejected: its `go.mod` needs Go 1.27.1, and CloudBurrow builds with Go 1.26 |
| H.264 | `github.com/Eyevinn/hi264` | v0.12.3 | MIT | none | IDR and P_Skip only | Rejected: "full P/B-frame decoding is out of scope" |
| H.265 | `github.com/gen2brain/h265` | v0.2.5 (2026-11-11) | MIT | none | HEVC v1 and range extensions (8–16 bit, all chroma formats), inter prediction, open GOPs; byte-exact on the JCT-VC conformance suite (its claim) | **Adopted**: measured bit-exact against ffmpeg here (70 dB PSNR) on an x265 MOV with B-frames and a CRA with RASL pictures; raises `go.mod`'s `go` line to 1.26.4 |
| VP9 | `github.com/thesyncim/govpx` | 2026-10-18 pseudo-version | BSD 3-Clause | none | VP8 and VP9 profile 0, the VP9 conformance corpus (its claim) | **Blocked**: its `go.mod` needs Go 1.27.0 (`toolchain go1.27.2`); CloudBurrow's CI still builds main with Go 1.26. Usable once Go 1.26 is dropped (adopted as a copy since #1731, below) |
| VP9 | `github.com/go-avkit/vp9` | v0.2.2 | BSD 3-Clause | none | headers and superframes only | Rejected: no pixels |
| AV1 | `github.com/KarpelesLab/goavif` | v0.1.0 (2026-04) | MIT | none | AV1 intra and single-reference translational inter | Rejected: compound, warped and global motion, which libaom and SVT-AV1 use, repeat the previous frame instead (its README) |
| AV1 | `github.com/gen2brain/avif` | v0.6.0 | MIT (libavif and libaom BSD 2-Clause) | none (WASM on wazero) | AVIF stills and AVIS sequences | Rejected for video: it reads only AVIF containers, not an MP4's AV1 track (`gen2brain/gav1d` adopted since #1731, below) |
| any | `codeberg.org/gruf/go-ffmpreg` | 2026-09 | GPL-3.0 (ffmpeg built with `--enable-gpl`) | none (WASM on wazero) | ffmpeg 5.1 | Rejected: GPL |
| PDF | `github.com/klippa-app/go-pdfium` | v1.21.4 (2026-10-21) | MIT; PDFium BSD 3-Clause and Apache 2.0 | none in its `webassembly` package (PDFium compiled to WASM, run on `tetratelabs/wazero` v1.12.0, Apache 2.0) | rendering, page sizes, text | **Adopted** |
| PDF | `github.com/gen2brain/go-fitz` | — | AGPL-3.0 (MuPDF) | cgo | rendering | Rejected: AGPL and cgo |
| PDF | `github.com/pdfcpu/pdfcpu`, `rsc.io/pdf`, `github.com/ledongthuc/pdf` | — | Apache 2.0 / BSD | none | parsing, text | Rejected: no renderer |
| PDF | `github.com/unidoc/unipdf` | — | commercial | none | rendering | Rejected: licence |

So H.264 and H.265 in MP4 or MOV are served, and VP9 and AV1 stay refused naming why. The PDF
renderer is 5.8 MB of WebAssembly embedded in the binary, compiled once, the first time a PDF is
embedded. `document_ocr` reads the text PDFium finds on each page, which is the text a born-digital
PDF holds; CloudBurrow has no OCR engine, so a PDF with no text on any page (a scan) is refused
with `document_ocr` rather than embedded without the text Google would have read. A video's audio
track (`audio_track_extraction`) stays refused: an MP4's audio is AAC, which none of these decode
(served since #1732, below).
`govulncheck ./...` reports nothing for any of them. `go mod tidy` added `tetratelabs/wazero` and
`jolestar/go-commons-pool/v2` as indirect requirements.

## Amendment: an AAC decoder for a video's audio track (#1732)

Google's [multimodal embeddings page](https://docs.cloud.google.com/vertex-ai/generative-ai/docs/embeddings/get-multimodal-embeddings)
(re-read 2026-11-17) documents `audio_track_extraction` ("Extract audio from video input"), off by
default, the audio subject to the 180 seconds a request; it does not say how the audio is placed.
EmbeddingGemma 2's model card does not either; Transformers' processors (`load_audio_from_video`,
which `Gemma4Processor` takes) load a video's audio as a clip of its own with its audio placeholder
right after the video's, and CloudBurrow does the same. An MP4's or MOV's audio is nearly always
AAC-LC; MP3 and PCM, the formats Google lists for audio, also occur. The user approved media
decoder modules (2026-11-17). The candidates on the Go module proxy, compared on 2026-11-17:

| Module | Version | Licence | cgo | Covers | Verdict |
|---|---|---|---|---|---|
| `github.com/arabian9ts/aac-go` | v0.1.0 (2026-08-11) | Apache 2.0 | none, no dependencies | AAC-LC decoder and encoder, ADTS, mono and stereo, all 13 rates, long and short windows, TNS, M/S, intensity stereo, PNS; HE-AAC, more than 2 channels and 960-sample frames refused with `ErrUnsupported` | **Adopted**: within one 16-bit step of ffmpeg 9 a channel on ffmpeg's own AAC-LC (16 kHz stereo, M/S and short blocks; PNS, which is noise each decoder draws, at the encoder's level), `TestDecodeVideoAudioTrack`. Its `go.mod` says `go 1.26.5`, which raises CloudBurrow's `go` line from 1.26.4 (CI builds with 1.26.8 and 1.27). One release, one day of commits and no stars: the measured agreement with ffmpeg is what it is adopted on; if it needs a fix, copy it into `third_party/` as `go-mp3` is |
| `github.com/tphakala/go-aac` | v0.7.2 (2026-11-05) | LGPL-2.1 (a port of ffmpeg's) | none | AAC-LC | Rejected: LGPL, not one of the permissive licences approved |
| `github.com/thesyncim/goaac` | 2026-06 pseudo-version | GPL (a port of FAAD2) | none, but `modernc.org/libc` | AAC-LC, HE-AAC | Rejected: GPL |
| fdk-aac, FAAD2, ffmpeg in WASM | — | Fraunhofer, GPL, LGPL | — | — | Rejected: licence (and C code) |

An MP4 carries AAC as raw access units; each is given the 7-byte ADTS header its
AudioSpecificConfig (`esds`, read with `mp4ff`) describes and decoded. The edit list's first media
time (the encoder's priming, 1024 samples from ffmpeg) is dropped and its duration kept, as ffmpeg
does. MP3 tracks (`mp4a` with the MPEG-1/2 audio object types, or `.mp3`) go through
`third_party/go-mp3`, QuickTime linear PCM (`lpcm`, `sowt`, `twos`, `in24`, `in32`, `fl32`, `fl64`)
is read directly. Opus, AC-3, E-AC-3, ALAC, FLAC and HE-AAC tracks are refused naming the codec:
Opus has no complete pure Go decoder with a permissive licence, and the others no Go decoder at
all. `govulncheck ./...` reports nothing for `aac-go`.

## Amendment: VP9 and AV1 decoders for EmbedContent video (#1731)

Google's [multimodal embeddings page](https://docs.cloud.google.com/vertex-ai/generative-ai/docs/embeddings/get-multimodal-embeddings)
(re-read 2026-11-17) lists "Codec: AV1, H264, H265, VP9" for `gemini-embedding-2` video, so the
VP9 and AV1 refusals of #1722 are a gap. The user approved pure Go, permissively licensed decoders,
copied into `third_party/` where a module cannot be required (2026-11-17). Compared on the Go module
proxy on 2026-11-17:

| Need | Module | Version | Licence | cgo | Covers | Verdict |
|---|---|---|---|---|---|---|
| VP9 | `github.com/thesyncim/govpx` | v0.0.0-20261018202535-159dce5b931a | BSD 3-Clause (a libvpx port; libvpx's licence and patent grant included) | none, standard library only | VP9 profile 0 (8-bit 4:2:0), superframes, hidden frames, every inter mode | **Adopted as a copy** in `third_party/govpx` ([PROVENANCE.md](../third_party/govpx/PROVENANCE.md)): its `go.mod` needs Go 1.27, which only its `GOEXPERIMENT=simd` files use; the decoder is copied without them and without the encoders, two changes recorded. Bit-exact against ffmpeg 9 on a two-pass libvpx clip with alternate reference frames (180 frames by MD5) |
| AV1 | `github.com/gen2brain/gav1d` | v0.2.7 (2026-11-11) | BSD 2-Clause (a port of dav1d; dav1d's patent grant in `PATENTS`) | none, no dependencies | every AV1 tool, 8 to 12 bits, 4:0:0 to 4:4:4, film grain | **Adopted**: bit-exact against ffmpeg 9's libdav1d on an SVT-AV1 clip (180 frames by MD5); its `go.mod` says `go 1.26.4`, below CloudBurrow's line. Pure Go, so no WebAssembly blob is needed (it could also serve AVIF) |
| AV1 | `github.com/KarpelesLab/goavif` | v0.1.0 | MIT | none | intra and single-reference inter | Rejected, as in #1722 |
| AV1 | dav1d or libaom compiled to WASM on wazero | — | BSD 2-Clause | none | complete | Not needed: `gav1d` is complete in pure Go and needs no blob |

VP9 profiles 1 to 3 (4:2:2, 4:4:4, 10 and 12 bits), which `govpx` does not decode, are refused
naming the profile; libvpx and every browser's encoder default to profile 0. Neither copy imports
`ebitengine/purego`, so the `CGO_ENABLED=0` Linux CLI stays statically linked. `govulncheck ./...`
reports nothing for either.

## Amendment: AVIF on gav1d instead of the WebAssembly decoder (#1736)

The vendored WebAssembly build of `gen2brain/avif` (#1729) could not decode a 10- or 12-bit AVIF
over 13312 × 13312 in its 4 GiB of memory, while Google takes 16384 × 16384. `gen2brain/gav1d`
v0.2.7, required since #1731 for AV1 video, has an `avif` package of its own: an ISOBMFF reader
(grids, clap, irot, imir, alpha items, AVIS sequences, nclx and ICC), dav1d's decoder ported to
Go, and a YUV-to-RGB converter, with no cgo, no WebAssembly, no `purego` and no dependencies. It
decodes every #1729 fixture pixel-identically to libavif 1.4.2's references: a lossless AVIF (the
PNG exactly), 4:2:0 (against `avifdec -u nearest`, with CloudBurrow converting 8-bit planes as
libyuv does, nearest chroma), 10-bit 4:4:4 (against `avifdec -d 16`, gav1d's own 16-bit
conversion), alpha, an AVIS sequence's first frame and a clap + irot + imir transform. A 10-bit
16384 × 16384 AVIF (a 2 × 2 grid written by `avifenc`; libaom cannot encode one 16384 × 16384 frame,
"Failed to allocate lag buffers") decodes in 1.2 s at a peak RSS of 3.8 GB on an M4 Max, two
512 × 512 regions identical to `avifdec -d 16`'s (which peaked at 2.9 GB). So `internal/avif`, its
3.5 MB blob and `third_party/avif` are deleted, `UNIMPLEMENTED` for deep AVIFs is gone, and
`tetratelabs/wazero` is again only go-pdfium's (indirect) requirement. One deep 4:2:0 or 4:2:2
difference remains: gav1d upsamples its chroma bilinearly, as libavif's own converter does, where
libavif with libyuv would take the nearest sample; 8-bit images, nearly every AVIF, convert as
before. Upstream nit: `avif.DecodeConfig` reports a deep grid as 8-bit (the grid item has no
`av1C`); CloudBurrow does not use the colour model. `govulncheck ./...` reports nothing for it.

## Amendment: Opus, AC-3 and HE-AAC tracks, VP9 profiles 1 to 3, and copying aac-go (#1739)

Compared on the Go module proxy on 2026-11-17, for a video's audio track and VP9's other profiles,
under the user's approval of permissive pure Go decoders:

| Need | Module | Version | Licence | cgo | Verdict |
|---|---|---|---|---|---|
| Opus | `github.com/kazzmir/opus-go` | v1.7.0 (2026-09-25) | BSD 3-Clause (libopus BSD 3-Clause) | none: libopus transpiled to Go by ccgo, no dependencies, `go 1.24.0` | **Adopted**: a 48 kHz stereo libopus track decodes within 0.71 of a 16-bit step of ffmpeg 9's libopus decode (0.16 of its native decoder's) |
| Opus | `github.com/thesyncim/gopus` | v0.2.3 | BSD 3-Clause | none | Not needed: its `go.mod` needs Go 1.27.3 |
| Opus | `github.com/pion/opus` | v0.1.0 | MIT | none | Rejected: SILK only, no CELT |
| AC-3, E-AC-3 | `github.com/gravity-zero/ac3go` | 2026-10-07 pseudo-version | AGPL-3.0 | none | Rejected: AGPL |
| AC-3 | `github.com/moon-wind/mediacommon` | — | MIT | none | Rejected: headers only, no decoder |
| HE-AAC | `github.com/tphakala/go-aac`, `github.com/skrashevich/go-aac` | v0.7.2, v0.1.0 | LGPL-2.1, LGPL-3.0 | none | Rejected: LGPL, and neither decodes SBR |
| HE-AAC | `github.com/llehouerou/go-faad2` | v0.3.1 | GPL-2.0 (FAAD2 in WASM) | none | Rejected: GPL |
| HE-AAC | `github.com/Comcast/gaad` | — | Apache 2.0 | none | Rejected: parses SBR, decodes nothing |
| VP9 profiles 1–3 | `thesyncim/govpx` (latest is the copied 2026-10-18 commit) | — | BSD 3-Clause | none | Not available: its reconstruction is 8-bit 4:2:0 only |

So Opus tracks (mono and stereo, mapping family 0) are served, and AC-3, E-AC-3, HE-AAC tracks and
VP9 profiles 1 to 3 stay refused naming the codec or profile: each needs a decoder written for
CloudBurrow (AC-3 from ATSC A/52, SBR and PS on the copied aac-go, high-bit-depth and 4:2:2/4:4:4
reconstruction in the copied govpx). `arabian9ts/aac-go` is copied into `third_party/aac-go`
([PROVENANCE.md](../third_party/aac-go/PROVENANCE.md)): it has one release and no activity since,
and SBR would be added there. `govulncheck ./...` reports nothing for `opus-go`.

**Follow-up (#1739, 2026-11-17): the decoders written here.** With no permissive module to adopt,
each is written for CloudBurrow, standard library only, from a public specification:

- **VP9 profiles 1 to 3**: `third_party/govpx/vp9hbd`, a VP9 decoder following the VP9 bitstream
  specification's decoding process for every profile (8, 10 and 12 bits; 4:2:0, 4:2:2, 4:4:0,
  4:4:4), reusing govpx's tables; bit-exact against ffmpeg 9 on 180-frame clips of each profile
  ([PROVENANCE.md](../third_party/govpx/PROVENANCE.md)). govpx's own decoder still decodes profile 0.
- **HE-AAC**: SBR and parametric stereo added to `third_party/aac-go` from ISO/IEC 14496-3, with
  PacketVideo's OpenCORE AAC decoder (Apache 2.0) as a reference for the PS Huffman tables and the
  QMF window's even coefficients ([PROVENANCE.md](../third_party/aac-go/PROVENANCE.md)). Parametric
  stereo with 34 bands, IPD/OPD or mixing procedure B stays refused naming the tool: their
  normative tables are printed only in ISO/IEC 14496-3 itself and in LGPL code (not read), and
  AudioToolbox, the only HE-AACv2 encoder at hand, never emits them.
- **AC-3 and E-AC-3**: `internal/ac3`, from ATSC A/52 as ETSI TS 102 366 V1.4.1 publishes it
  (with Annex E), within 3e-5 of ffmpeg 9's float decode at high bit rates.
- **Surround Opus**: `opus-go`'s multistream decoder, mapping family 1.

## Amendment: replacing opus-go (#1811)

`github.com/kazzmir/opus-go` v1.7.0 (its newest release and its master, 2026-11-18) is unsafe in
any build, not only under `-race` (kazzmir/opus-go#79, open): its libc shim grows the decoder's
pseudo-stack by copying it, leaving the addresses already handed out pointing at the freed old one,
and the transpiled decoder passes `uintptr(unsafe.Pointer(&local))` down its call chains, which a
goroutine stack that grows or shrinks mid-call leaves pointing at the old stack. Built with
`-gcflags=github.com/kazzmir/opus-go/...=-d=maymorestack=runtime.mayMoreStackMove` (the runtime
moves the stack at every function entry), it decoded va-opus51.mp4 0.76 of full scale from ffmpeg,
va-opus.mp4 17947 16-bit steps, and aborted on a deeper stack. Compared on 2026-11-18:

| Option | Verdict |
|---|---|
| Wait for upstream | Rejected: master still fails as above; its uintptr-to-typed-pointer conversions (#73 to #83) are partial |
| Patch a copy of opus-go in third_party | Rejected: the pseudo-stack is one function, but its 46,500-line transpiled decoder (`opuscc`) has 442 `uintptr(unsafe.Pointer(&...))` conversions, the locals among them to be told from globals and heap fields and each moved to non-moving memory by hand, and again on each upstream release, and checkptr still rejects the shim's uintptr memory |
| `github.com/thesyncim/gopus` v0.2.3, `github.com/tphakala/go-opus` v1.4.0 (BSD 3-Clause) | Not usable: their `go.mod` need Go 1.27.3 and 1.27, and CI builds with 1.26 |
| `github.com/darui3018823/opus` v1.8.0 (BSD 2-Clause) | **Adopted**: pure Go written for Go (no transpiled C, no `unsafe`, no cgo in what is linked; its libopus harness is behind the `opusref` tag), standard library only, `go 1.24.11`; CELT, SILK, hybrid, multistream families 0, 1 and 255; its README reports sample-exact output against libopus 1.6.1's float decoder on all 12 RFC 8251 vectors |

Measured here: within 1.9e-7 of opus-go's samples (unmoved) on every Opus test track (5.1, 7.1 and
stereo CELT, stereo hybrid), its 5.1 and 7.1 downmixes within 6e-5 of ffmpeg 9's libopus decode at
48 kHz, identical samples at every stack depth with the collector running and with the stack moved at
every function entry (`TestDecodeOpusOnMovingStacks`), 400,000 random packets without a panic, and
`make test-race` with checkptr on everywhere, so #1808's checkptr exemption is gone.

## Amendment: OCR for `document_ocr` on scanned PDFs (#1733)

Google documents that `document_ocr` adds the text OCR reads from each PDF page. CloudBurrow now
reads a page with no text layer with **Tesseract 5.5.4**, the LSTM engine Google developed and
open-sourced (Apache-2.0), compiled here to a WASI WebAssembly module and run on wazero, like
PDFium: no cgo, and the Linux CLI stays static.

| Candidate | Licence | Runtime | Result |
|---|---|---|---|
| Tesseract 5.5.4 + Leptonica 1.87.0, built by `third_party/tesseract-wasm/build.sh` with wasi-sdk 34 | Apache-2.0; BSD-2-Clause | WASI module on `tetratelabs/wazero` (already required) | **Adopted**: CER 1.5-2.0% on the 300 DPI test scan, about 0.8 s a page on one instance, six pages in about 3 s in parallel (6 s and 14 s before #1761 dropped wazero's close-on-context-done checks) |
| tesseract.js core (Emscripten build) | Apache-2.0 | needs Emscripten's JavaScript glue | Rejected: not a WASI module; a prebuilt binary not built here |
| `github.com/Danlock/gogosseract` | MIT | wazero, tesseract.js's Emscripten build | Rejected: wraps a prebuilt third-party module and its Emscripten glue |
| `github.com/otiai10/gosseract` | MIT | cgo, libtesseract | Rejected: cgo |
| PaddleOCR PP-OCRv5 (ONNX) | Apache-2.0 | ONNX Runtime (`-tags onnx` builds only) | Rejected: not Google-authored, and only in the ONNX build |

Models: tessdata_fast 4.1.0 `eng` and `osd` (Apache-2.0), embedded gzipped (6.2 MB). A page is
rendered again at 300 DPI, orientation and script detection turns an upside-down page back
(automatic page segmentation reads a page turned a quarter as vertical text), and the text is read
in reading order. A page orientation and script detection finds in another script is read with
tessdata_fast 4.1.0's model of that script (`script/Cyrillic`, `script/HanS`, ... 36 in all,
Apache-2.0), fetched by `internal/localai` the first time one is needed into `<state-dir>/models`
and checked against its pinned SHA-256 (CER 0.0% Cyrillic, 1.8% Greek on generated scans). Provenance: `third_party/tesseract-wasm/PROVENANCE.md`; `dependencies.json`
`tesseractWasm`.
