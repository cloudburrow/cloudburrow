# Local AI: audit and current status

Issue: #39 · Audited 2026-09-21

**Conclusion: local AI inference is not viable in CloudBurrow today.** Three independent
findings block it, all verified against publisher APIs rather than inferred. The acquisition
machinery is implemented and tested; the runtime is not, because no runtime exists that can
run in a CloudBurrow pod.

This page exists so the reason is on record. A half-working AI stack that quietly returned
canned text would be the worst possible outcome for a project whose value is honest status.

## Finding 1 — LiteRT-LM publishes no current Linux binary

CloudBurrow runs workloads in a **Linux** Kubernetes cluster. LiteRT-LM's releases:

| Release | Date | Platforms shipped |
|---|---|---|
| v0.17.1 | 2026-09-16 | iOS/macOS frameworks only |
| v0.17.0 | 2026-09-09 | macOS arm64 |
| v0.16.1 | 2026-08-18 | macOS arm64 |
| v0.15.0 | 2026-08-04 | macOS arm64 |
| v0.14.0 | 2026-07-08 | macOS arm64 |
| **v0.11.0** | **2026-05-07** | **linux_x86_64**, android, ios, macos, windows |

The last Linux binary is **v0.11.0, four months and six releases ago**. A macOS binary cannot
run in a Linux pod.

Options, none currently taken: ship the stale v0.11.0; build from source (a C++/Bazel build
that would become CloudBurrow's largest dependency by far); or run inference on the host,
which contradicts the architecture every other service follows.

## Finding 2 — provenance is split, and the names do not tell you

The issue warned specifically against assuming a community conversion is Google-published.
That warning was justified.

| Repository | Organisation | Verified as Google? |
|---|---|---|
| `google/gemma-3n-E2B-it-litert-lm` | `google` | **Yes** |
| `google/gemma-3n-E4B-it-litert-lm` | `google` | **Yes** |
| `google/embeddinggemma-300m` | `google` | **Yes** |
| `litert-community/gemma-4-E2B-it-litert-lm` | `litert-community` | **No** — `isVerified: false` |
| `litert-community/Gemma3-1B-IT` | `litert-community` | **No** |

`litert-community` is "LiteRT Community (FKA TFLite)" and is **not** a verified Google
organisation, despite hosting the most-downloaded `.litertlm` Gemma conversions and naming
them after Google models.

`internal/localai` therefore records `Publisher` as a field and a test asserts that a
community conversion is never reported as Google-published.

## Finding 3 — every Gemma artifact is gated

All Gemma repositories report `gated: manual`: they require accepting the Gemma licence and
being granted access. **Acquisition can never be automatic.**

This satisfies the issue's requirement that "starting the core stack must not automatically
download large weights" — the gating enforces it — but it also means acquisition needs a user
token, and a gated model without one is refused up front rather than attempted and failing
with an opaque 401 after a long wait.

## What is implemented

`internal/localai` does the part that is genuinely useful and verifiable today:

- **A catalogue** recording, per artifact: publisher (verified provenance, not hosting
  location), access (open or gated), licence, modality, the **specific artifact file** —
  repositories hold several and they are not interchangeable — and any runtime.
- **Gated refusal** with an actionable message naming the licence page and the token variable.
- **Disk preflight** with a margin, because filling a developer's disk and failing at 98% is
  much worse than refusing up front.
- **Checksum verification** on download and on cache inspection. Where no checksum is pinned,
  the status says the contents are **unverified** rather than claiming they are fine.
- **Atomic caching** via a temporary file and rename, so an interrupted download never leaves
  a partial artifact that looks cached.
- **Recovery**: a corrupt entry is detected and removable, and removing an absent one is not
  an error.
- **`Runnable()` returns false for every catalogued model**, with the specific reason. A
  catalogue entry is not a promise that CloudBurrow can execute it.

## What is not implemented, and why

- **No inference.** No runtime can execute these artifacts in a CloudBurrow pod (Finding 1).
- **No benchmarks.** The issue asks for measured memory, latency and context limits. Without
  a runtime there is nothing to measure, and inventing figures would be worse than having
  none.
- **No pinned checksums.** The artifacts are gated, so they could not be downloaded and
  hashed. The code supports pinning; the values are absent and the status reports that
  honestly.
- **#40 (generation API) and #41 (embeddings) depend on this**, so both inherit the block.
  `embeddinggemma-300m` has a further gap: it ships as safetensors, and LiteRT-LM supporting
  generation does not imply embedding support — exactly as #41 anticipated.

## What would unblock it

Any one of: LiteRT-LM resuming Linux releases; a decision to build it from source for Linux;
or an explicit decision to run inference on the host outside the cluster model. Each is a
scoping decision rather than an implementation detail, which is why this audit stops here
rather than picking one.

---

## Re-verification, 2026-09-21

The original audit concluded that local inference is not viable. That conclusion was
re-tested from scratch, because a blocker nobody re-checks becomes a habit. It holds, and the
evidence is now stronger and more specific than before.

### 1. LiteRT-LM still publishes no Linux binary

Not "not recently" — **not in any of the last five releases**, including one from five days
before this check:

```
$ curl -s https://api.github.com/repos/google-ai-edge/LiteRT-LM/releases?per_page=5
v0.17.1  2026-09-16  ['CLiteRTLM.xcframework.zip', 'CLiteRTLM_mac.xcframework.zip']
v0.17.0  2026-09-09  ['CLiteRTLM.xcframework.zip', 'CLiteRTLM_mac.xcframework.zip',
                      'litert_lm_main.macos_arm64']
v0.16.1  2026-08-18  ['litert_lm_main.macos_arm64']
v0.16.0  2026-08-11  [... 'litert_lm_main.macos_arm64' ...]
v0.15.0  2026-08-04  [... 'litert_lm_main.macos_arm64' ...]
```

Every artifact is macOS or iOS. A cluster pod runs Linux, so none of these can execute in
one — which is the specific thing CloudBurrow would need.

### 2. The Google-published Linux runtimes that *do* exist cannot run a language model

This is the obvious objection — *"there are Google wheels for Linux"* — and it was checked
rather than waved away. There are. They are the wrong kind.

| Package | Linux wheels | LLM inference API |
|---|---|---|
| `ai-edge-litert` 2.2.0 (Google AI Edge Authors) | `manylinux_2_27_x86_64`, `_aarch64` | **No.** Installed and imported: it exposes `Interpreter`, `SignatureRunner`, `Delegate`. `ai_edge_litert.llm`, `.genai` and `.lm` do not exist. |
| `mediapipe` 1.0.1 (The MediaPipe Authors) | `manylinux_2_28_x86_64`, `_aarch64` | **No.** Installed and imported: `mediapipe.tasks.python.genai` — the module that held `LlmInference` — **is not present** in 1.0.1. `mediapipe.tasks.python.text` exists and has no `LlmInference`. |
| `gemma` 4.0.1 (Google DeepMind) | none — source only | Not evaluated: it is a research library, not a serving runtime. |

`ai-edge-litert` runs a tensor graph. A `.litertlm` language model needs the LiteRT-LM
engine, which is the thing with no Linux build.

### 3. Every Google-published artifact is still gated

```
google/gemma-3n-E2B-it-litert-lm     gated=manual  author=google         downloads=4,572
google/embeddinggemma-300m           gated=manual  author=google         downloads=2,719,957
litert-community/gemma-4-E2B-it-...  gated=False   author=litert-community  downloads=1,221,596
```

The only ungated artifact is the **community** one. `litert-community` is not the verified
`google` organisation, whatever the model name suggests, and CloudBurrow does not report a
community conversion as Google-published.

So even if a Linux runtime appeared tomorrow, an automatic download of a Google model still
would not be possible without a credential the developer supplies.

### What would change this

Any one of:

- A Linux artifact in a LiteRT-LM release.
- An LLM inference API in a Google-published Linux wheel.
- A Google-published, ungated Gemma artifact in a format a Linux runtime can execute.

`make deps-check` does not watch for these, because none of them is a version bump; they
are the questions above, asked again.
