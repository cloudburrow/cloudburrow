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

### 4.2 fake-gcs-server: adopted

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
| `blackwell-systems/gcp-kms-emulator` | Apache-2.0 | One contributor. Untyped storage errors become INTERNAL, including "primary version is not enabled", which a caller cannot tell apart from a crash. | [server.go:57-70](https://github.com/blackwell-systems/gcp-kms-emulator/blob/8f6372808a3e798bec0bdbcd6183915c963b2ada/internal/server/server.go#L57-L70), [storage.go:315](https://github.com/blackwell-systems/gcp-kms-emulator/blob/8f6372808a3e798bec0bdbcd6183915c963b2ada/internal/storage/storage.go#L315) |
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
