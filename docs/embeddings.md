# Local embeddings: why they do not work yet

Issue: [#41](https://github.com/cloudburrow/cloudburrow/issues/41) · Investigated 2026-09-21
· Re-checked 2026-09-22, unchanged (§6)

**Embeddings are blocked.** This page records what was tried and what exactly fails, because
the previous statement of the blocker was wrong and sent the reader in the wrong direction.

---

## 1. The blocker that was recorded was false

[local-ai.md](local-ai.md) said *"every embedding artifact is gated"*. It is not. Three are
ungated and download with no credentials at all:

| Artifact | Gated | Result |
|---|---|---|
| `kontextdev/embeddinggemma-300m-litertlm` | **no** | downloads, **no tokenizer in the bundle** |
| `litert-community/EmbeddingGemma-300M-Tensor-G4-NPU` | **no** | downloads, rejected by the engine |
| `litert-community/embeddinggemma-300m` | `auto` | 401 — and holds NPU-specific `.tflite`, not a bundle |

The distinction matters. "Gated" tells a developer to go and get a token. That would not have
helped: the artifacts that need no token do not work either, for a reason that has nothing to
do with access.

## 2. What actually fails

### The ungated bundle has no tokenizer

```
$ embedding_litert_lm_main --model_path=embeddinggemma-300m.litertlm
Main execution failed: NOT_FOUND: No tokenizer found in the model
```

Google's own inspector confirms it — **one section**, the model, and nothing else:

```
$ litert-lm-peek --litertlm_file embeddinggemma-300m.litertlm
  Sections (1)
  Section 0:  model_type: tf_lite_embedder   Data Type: TFLiteModel
```

The publisher's own build config, published beside it, shows why:

```toml
# Section 2: HuggingFace Tokenizer (if available)
# Uncomment if you have the tokenizer.json
```

It was never included. The same file also declares `license: apache-2.0` and
`author = "Google"`. Both are wrong: these are `google/embeddinggemma-300m` weights, the Gemma
terms apply, and the conversion is not Google's. The catalogue records the correct licence.

### Rebuilding it correctly gets one step further, and then stops

The missing tokenizer is fixable. The embedder `.tflite` is ungated in that same repository,
and an ungated, correctly-licensed `tokenizer.json` is in `onnx-community/embeddinggemma-300m-ONNX`.
Rebuilt with Google's official `litert-lm-builder`:

```toml
[[section]]
section_type = "TFLiteModel"
model_type   = "EMBEDDER"
data_path    = "embedder.tflite"

[[section]]
section_type = "HF_Tokenizer"
data_path    = "tokenizer.json"
```

```
$ litert-lm-builder toml --path bundle.toml output --path embeddinggemma-300m-cb.litertlm
LiteRT-LM file successfully created
```

The tokenizer error is gone and the engine initialises. Then:

```
Initializing EmbeddingEngine...
Main execution failed: INVALID_ARGUMENT:
└ [runtime/core/embedding_engine_impl.cc:464]
└ Input tensor bytes must be 4 but got 2048
```

### It is the export, not the packaging

The identical error comes from `litert-community/EmbeddingGemma-300M-Tensor-G4-NPU`, which is a
different bundle, by a different publisher, **with** its tokenizer and with 768 dimensions
rather than 256. Two independent artifacts failing the same way at the same line is the
signature, not the packaging.

2048 bytes is `512 × int32`: a fixed `[1, 512]` token input. The engine wants a 4-byte input
tensor — a single token, decided at run time. The available conversions export a fixed-length
encoder; the runtime at commit `02e5030` expects a dynamic one.

No flag bridges it. `--max_input_length`, `--min_input_length`, `--input_overflow_strategy`,
`--activation_data_type` and `--use_mmap` were each tried: they produce the same error, or
`NOT_FOUND` when the signature filter prunes everything.

Corroboration: the Tensor-G4 repository ships its **own hand-written C engine**
(`engine/embed_npu.c`, `engine/embed_hetero.c`) rather than using the stock one. Somebody else
hit this and wrote around it.

## 3. What is actually blocked, precisely

| | |
|---|---|
| **Embedding runtime on Linux** | **Available.** `embedding_litert_lm_main` builds and ships in the runtime image. It loads models, initialises the engine and reports errors properly. |
| **Bundling tools** | **Available and working.** `litert-lm-builder` produced a valid bundle from ungated parts on the first try. |
| **A compatible model** | **Blocked.** Every obtainable conversion exports a fixed-length encoder signature the engine rejects. The one conversion by the runtime's own publisher is gated, and holds NPU-specific `.tflite` rather than a bundle. |

So the honest sentence is: **not "no runtime" and not "no access", but no obtainable artifact
exported the way the runtime requires.**

## 4. What would change it

- **An ungated `.litertlm` embedder exported with a dynamic input signature.** That is the whole
  requirement. Re-checking is cheap: build the runtime, run it, read the error.
- A token for `litert-community/embeddinggemma-300m` would let its contents be tested — but its
  files are per-NPU `.tflite`, so it may not help either. Unverified, and not claimed.
- A runtime release that accepts fixed-length encoders.

`make deps-check` cannot watch for any of this: none is a version bump.

## 5. Why no endpoint was built

No Vertex embedding endpoint is served. An endpoint with no model behind it would be a
plausible stub, which [AGENTS.md](../AGENTS.md) forbids, and the acceptance criteria for #41
require a **tested** model/runtime pair, output dimensions, finite values and a retrieval
quality check — every one of which needs a model that runs.

The catalogue lists all three artifacts with no runtime and the reason attached, and the
console reports them as *"no compatible model: the embedding runtime builds, but every
obtainable artifact is rejected"* rather than as a missing runtime.

## 6. Re-checked 2026-09-22 — unchanged

The finding above says re-checking is cheap, so it was re-checked a day later. Nothing has
moved, and this records what was looked at so the next reader does not repeat it.

**The runtime already has every relevant change.** The pinned ref `02e5030` is dated
**2026-09-21**, which is after all ten of the embedding-engine commits on
`runtime/core/embedding_engine_impl.cc` — including the three that touch signature selection
directly (`3842dadbc9` "Select text encoder signatures from bundle metadata", `dc911c3dbf`
"Return an error during signature selection when target capacity exceeds available",
`c532bf70a5` "Resolve and validate embedding engine settings when streaming"). The only newer
commit on that file, `4cf0bf515d` of 2026-09-22, adds TTS, ASR and generic-binary sections and
has nothing to do with encoder signatures. **Upgrading the pin would change nothing here.**

**The artifacts are unchanged.** `litert-community/embeddinggemma-300m` is still `gated: auto`
and still holds no `.litertlm` at all. The other two are byte-identical to what was tested.
A fresh search of every `embeddinggemma` repository on Hugging Face turned up no new
`.litertlm` embedder from any publisher.

**And the error is the same error**, reproduced today against the shipped image:

```
$ docker run --rm -v "$PWD:/models" \
    --entrypoint /usr/local/bin/embedding_litert_lm_main cloudburrow/litert-lm:local \
    --model_path=/models/EmbeddingGemma-300M_seq512_Google_Tensor_G4.litertlm \
    --backend=cpu --input_prompt="hello"
Main execution failed: INVALID_ARGUMENT: ERROR: [runtime/engine/embedding_litert_lm_main.cc:369]
└ ERROR: [runtime/core/embedding_engine_impl.cc:464]
└ Input tensor bytes must be 4 but got 2048
```

The ungated `kontextdev` bundle still fails earlier, on the missing tokenizer:

```
Main execution failed: NOT_FOUND: ERROR: [runtime/engine/embedding_litert_lm_main.cc:265]
└ No tokenizer found in the model for model type: TF_LITE_PREFILL_DECODE
```

**One thing is new, and it is worse.** The Tensor-G4 repository's **seq256** bundle — the
sibling of the seq512 one quoted above — does not return that error. It **segfaults**:

```
$ ... --model_path=/models/EmbeddingGemma-300M_seq256_Google_Tensor_G4.litertlm ...
Loading model from: /models/EmbeddingGemma-300M_seq256_Google_Tensor_G4.litertlm
Using ScopedFile.
$ echo $?
139
```

Nothing is printed between loading the file and the crash. That is recorded rather than
diagnosed: it is another reason not to build an endpoint on these artifacts, not a lead.

## 7. Reproducing this

```sh
make litert-lm
curl -LO https://huggingface.co/kontextdev/embeddinggemma-300m-litertlm/resolve/main/embeddinggemma-300m.litertlm
docker run --rm -v "$PWD:/models" \
  --entrypoint /usr/local/bin/embedding_litert_lm_main cloudburrow/litert-lm:local \
  --model_path=/models/embeddinggemma-300m.litertlm --backend=cpu --input_prompt="hello"
```

`pip install litert-lm` provides `litert-lm-peek` and `litert-lm-builder`.
