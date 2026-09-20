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
| **Cloud Storage** | **Integrate + adapt** | `fsouza/fake-gcs-server` v1.56.1 | No official Google GCS emulator exists. This one implements the hard parts correctly: preconditions, resumable upload, compose, ranged reads, durability. |
| **Cloud Tasks** | **Build** | — | No official emulator and no viable community implementation found. This is a demonstrated gap. |
| **Cloud Run** | **Integrate via adapter** | Knative Serving v1.23.0 | Workload execution comes from Kubernetes/Knative; CloudBurrow supplies the Cloud Run v2 API adapter. |

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
