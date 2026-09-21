# Local AI: audit and current status

Issue: #39 · Audited 2026-09-21 · **Conclusion corrected the same day — see
[§4](#4-correction-local-inference-is-viable-and-this-ran)**

> **Current status: local text generation works.** The runtime was built from Google's source
> and run on Linux; the measured output is in §4. What remains blocked is narrower: there is
> no ungated **embedding** model, and no ungated **Google-published** model of any kind, so
> generation runs on a **community** conversion and is labelled as one.

The original audit below concluded that local inference was **not viable**. That conclusion
was **wrong**, and the sections that led to it are kept unedited so the mistake is legible:
it checked what upstream *published* and never tried building what it did not. Section 4
records what actually happened when someone did.

This page exists so the reasoning is on record — including where it failed. A half-working
AI stack that quietly returned canned text would be the worst outcome for a project whose
value is honest status; so is a blocker that was never re-tested.

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

Options, none taken **at the time of the audit** — building from source was dismissed as too
large, and that dismissal is what §4 overturns: ship the stale v0.11.0; build from source (a
C++/Bazel build that would become CloudBurrow's largest dependency by far); or run inference
on the host,
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

### 4. Correction: local inference **is** viable, and this ran

Everything above is accurate about what LiteRT-LM **publishes**. The conclusion drawn from it
— "local inference is not viable" — was **wrong**, and it was wrong in the direction that
costs a user a feature they could have had.

Linux is a documented, supported build target. Built from source at commit
`02e5030` (2026-09-21) and run:

```
$ bazel build //runtime/engine:litert_lm_main
INFO: Build completed successfully, 3111 total actions
$ ls -la bazel-bin/runtime/engine/litert_lm_main
-r-xr-xr-x 1 root root 30097248 Sep 21 17:33 litert_lm_main

$ litert_lm_main --backend=cpu --model_path=/models/gemma-4-E2B-it.litertlm \
    --input_prompt="Name three Google Cloud storage services. Answer in one short sentence."

input_prompt: Name three Google Cloud storage services. Answer in one short sentence.
Three Google Cloud storage services are Cloud Storage, Cloud Storage for Bigtable,
and Cloud Storage for Spanner.

  Time to first token: 0.30 s
  Prefill Speed: 80.75 tokens/sec      (22 tokens)
  Decode Speed:  31.81 tokens/sec      (23 tokens)
  Init Total: 235.76 ms
```

Linux, CPU backend, in a container, on arm64. The answer is factually wrong — that is a
small quantised model's quality, not the runtime's, and CloudBurrow does not present model
output as correct. (The artifact carries no quantisation label and we did not verify one, so
this says "quantised", not "int4".)

**Both binaries build.** `//runtime/engine:embedding_litert_lm_main` builds too (21.9 MB) and
takes `--input_prompt --backend=cpu`, so embeddings have a runtime as well.

#### The same run, from the shipped image

The run above was inside the **build** container, where Bazel's output tree is still present.
That is not what we ship, and the difference mattered: the first runtime image built cleanly
and then died immediately.

```
$ docker run --rm -v ./models:/models cloudburrow/litert-lm:local --model_path=...
/usr/local/bin/litert_lm_main: error while loading shared libraries:
libGemmaModelConstraintProvider.so: cannot open shared object file
```

`litert_lm_main` is not self-contained. It links a library upstream ships **prebuilt** per
platform, under `prebuilt/linux_{arm64,x86_64}/`, and locates it through a `RUNPATH` pointing
into Bazel's output directory — which a multi-stage build discards. The fix stages those
libraries into `/usr/local/lib/litert` and registers the directory with `ldconfig`.

A build that succeeds is not a runtime that runs, and only running what we actually ship
showed the difference. After the fix, `make litert-lm` produces a 274 MB image, and:

```
$ docker run --rm -v ./models:/models cloudburrow/litert-lm:local \
    --model_path=/models/gemma-4-E2B-it.litertlm --backend=cpu \
    --input_prompt="Explain in about 150 words what a container image is and why
                    reproducible builds matter."

A **container image** is a read-only, versioned template that packages an application
and all its dependencies (code, libraries, runtime, configuration) into a single,
portable unit. [...] Reproducibility guarantees consistency across development,
testing, and production.

  Time to first token: 0.39 s
  Prefill Speed: 78.43 tokens/sec      (28 tokens)
  Decode Speed:  31.98 tokens/sec      (144 tokens)
  Init Total: 306.55 ms
```

Two things about these numbers, because the earlier ones invite a wrong reading:

- **They are warm-cache.** XNNPACK writes a 788 MB `.xnnpack_cache` beside the model on first
  use. Cold, the same prompt initialises in 3.27 s rather than 0.31 s.
- **Decode speed depends on how much is generated.** A two-token answer measured 6.89 tok/s
  on the identical binary, because the first token's fixed cost is averaged over two tokens.
  The 144-token figure is the representative one; a short-answer benchmark is not.

This run answered correctly, and the earlier one did not. Neither fact is a claim about
quality — see [compatibility.md](compatibility.md), where output quality stays **Not
claimed**.

#### What the mistake was

Two steps, each reasonable, and the join between them wrong:

1. *LiteRT-LM publishes no Linux artifact* — true, verified across five releases.
2. Therefore *there is no Linux runtime* — **false**. A missing prebuilt artifact is a
   packaging gap. The build guide's first Linux instruction is `bazel build
   //runtime/engine:litert_lm_main`, and the target's `BUILD` file carries an explicit
   `@platforms//os:linux` case.

The audit checked what was published and never tried building what was not. "Not shipped"
was read as "not possible".

#### What is actually blocked, precisely

| | |
|---|---|
| **Generation runtime on Linux** | **Available.** Built and run above. |
| **Embedding runtime on Linux** | **Available.** `embedding_litert_lm_main` builds. |
| **A runnable generation model** | **Available, community-published.** `litert-community/gemma-4-E2B-it.litertlm`, 2.59 GB, `gated: false`, downloads without credentials. |
| **A runnable embedding model** | **Blocked**, but not for this reason — see [embeddings.md](embeddings.md). Three embedding artifacts turned out to be **ungated**; they fail on the encoder signature the runtime requires, not on access. |
| **A Google-published runnable model** | **Blocked.** All `gated: manual`. The working model is a **community** conversion and CloudBurrow labels it as one. |

So the honest blocker is narrower than it was: not "no runtime", but "no ungated **embedding**
model", and "no ungated **Google-published** model of any kind".

#### Cost

The binary is not published, so it is built. Measured end to end at **6m17s** from a cold
Docker cache on an Apple M4 Max with 16 CPUs given to the daemon — 4m41s of it Bazel, 5,142
actions — and paid once. Fewer cores will take longer, so read it as a floor.
Debian 13 (trixie) is the base — Abseil needs C++20 `<source_location>`, which Debian 12's
default clang 14 lacks and trixie's default clang 19.1.7 has.

### What would still change things

- **An ungated embedding artifact exported with a dynamic input signature** — would unblock embeddings (#41). Ungated ones exist; none is exported the way the runtime needs. See [embeddings.md](embeddings.md).
- **A Google-published ungated artifact** — would let CloudBurrow default to a Google model rather than a community conversion.
- A prebuilt Linux artifact in a LiteRT-LM release — would remove the one-off build, nothing more.

`make deps-check` cannot watch for any of these, because none is a version bump.
