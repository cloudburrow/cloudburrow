# Local embeddings: why they do not work yet

> **Update 2026-09-28 (#41).** By maintainer decision CloudBurrow now builds on a
> **community** conversion: `onnx-community/embeddinggemma-300m-ONNX`, file
> `onnx/model_quantized.onnx` (int8) with its `.onnx_data` and `tokenizer.json`,
> pinned at revision `5090578d9565bb06545b4552f76e6bc2c93e4a66` with each file's SHA-256
> in `internal/localai/catalog.go`. It is **NOT an official Google artifact**; the Gemma
> terms apply to its weights. It is served as model `embeddinggemma-300m-onnx-community`.
>
> - Build: `make build-onnx` writes `bin/cloudburrow-onnx` (cgo; it fetches the Hugging Face
>   `libtokenizers.a` of the github.com/daulet/tokenizers release go.mod pins, checked by
>   SHA-256, and fetches no model). The ONNX build workflow (`.github/workflows/onnx.yml`)
>   builds it on every push to main and nightly, never on pull requests. Run with
>   `ONNXRUNTIME_LIB=/path/to/libonnxruntime.{so,dylib}`. The default build has no ONNX
>   Runtime and answers every `:predict` 501.
> - The same `bin/cloudburrow-onnx` also runs BigQuery ML's imported ONNX models
>   (`CREATE MODEL … OPTIONS (MODEL_TYPE='ONNX')`, #1711) with BigQuery enabled: its ONNX
>   runner listens on `--port-onnx` (default 9025) and is published to the BigQuery front
>   through `cloudburrow-host`. No model download is involved; the default build answers 501.
> - Enable: `cloudburrow up -local-ai-embeddings`. The pinned files (~330 MB) are fetched
>   over HTTPS into `<state-dir>/models` and verified before use.
> - Use: `POST /v1/projects/{p}/locations/{l}/publishers/google/models/embeddinggemma-300m-onnx-community:predict`
>   or `:embedContent` (#1244), PredictionService `Predict` or `EmbedContent` over gRPC, or the
>   console's Vertex AI → Embeddings, which shows each whole vector, its dimension and norm,
>   its first values, the token count and `truncated` flag as the API returns them
>   (`usageMetadata.promptTokenCount` and `truncated` from `:embedContent`,
>   `statistics.token_count` and `statistics.truncated` from `:predict`, #1396), and the
>   pairwise cosine similarity (#1305).
> - Manage: the console's Vertex AI → Local models (#1306) lists every catalogued model with
>   its download state and size. **Download** is offered only for a pinned, ungated model —
>   today this one — after you confirm its exact size, and fetches the same pinned files
>   `-local-ai-embeddings` does, each verified by SHA-256; **Load** and **Unload** change what
>   the running endpoint serves (the embedding runtime only; a generation model is the
>   container `-local-ai-model` starts, which has no unload); **Delete** removes the local copy.
>   The Studio (Vertex AI → Studio) lists each pinned or served model's state — not
>   downloaded, downloading with its progress, downloaded, loaded — and links to Local models
>   and Embeddings (#1396).
> - Script it: the same operations are on the control port's admin API, behind the
>   instance's admin token like every `/admin` route (#1396), and drive the same manager the
>   console does, so a download started on one is the one the other reports:
>   `GET /admin/local-models` lists the states, and
>   `POST /admin/local-models/{id}/{download|cancel|load|unload|delete}` runs one; a download
>   needs `{"confirmBytes": N}`, the model's exact pinned size, and verifies every file.
>
>   ```sh
>   TOKEN=$(cat <state-dir>/<name>/admin-token)
>   curl -s -H "Authorization: Bearer $TOKEN" http://127.0.0.1:9000/admin/local-models
>   curl -s -X POST -H "Authorization: Bearer $TOKEN" \
>     http://127.0.0.1:9000/admin/local-models/embeddinggemma-300m-onnx-community/unload
>   ```
> - Measured against the real model on 2026-10-15 and 2026-10-16: see the Local AI section of
>   [compatibility.md](compatibility.md#local-ai).
>
> **Update 2026-10-19 (#1454): images.** A second community conversion is pinned:
> `onnx-community/embeddinggemma-2-ONNX` at revision `daa72c51243991dfcaf9f9137d2c573d8f7790c0`,
> the int8 text model (`onnx/model_quantized.onnx` and its `.onnx_data`), the int8 vision encoder
> (`onnx/vision_encoder_quantized.onnx` and its `.onnx_data`) and `tokenizer.json`, 541 MB, each by
> SHA-256. It converts Google DeepMind's open, ungated **EmbeddingGemma 2**
> ([google/embeddinggemma-2](https://huggingface.co/google/embeddinggemma-2), Apache 2.0, as is the
> conversion), which embeds text, images, video and audio into one 768-dimensional space. It is
> **NOT an official Google artifact**, and is served as `embeddinggemma-2-onnx-community`.
>
> - Get it: Vertex AI → Local models → **Download**, then **Load** (or the admin API's
>   `download` and `load`), in an instance started with `-local-ai-embeddings` from a
>   `make build-onnx` binary. `-local-ai-embeddings` itself still fetches only the first model.
> - Use: `EmbedContent` (gRPC or REST `:embedContent`) with text parts and `inline_data` PNG or
>   JPEG parts, in the shape Google documents for `gemini-embedding-2`; one `Content` is one
>   vector. `:predict` embeds text. The console's Embeddings page has an **Images** picker.
> - Images are prepared as the model's processor prepares them and checked against
>   Transformers.js 4.3.1 on the same files: identical vectors for an image that is not resized,
>   cosine 0.9991 for one that is (sharp's resampling against PIL's).
> - Not served: video, PDF, MP3, `document_ocr`, `audio_track_extraction` and image types beyond
>   PNG and JPEG (#1651). See the Vertex AI PredictionService section of
>   [compatibility.md](compatibility.md).
>
> **Update 2026-10-19 (#1651): audio and Cloud Storage.** The audio encoder of the same revision is
> pinned too (`onnx/audio_encoder_quantized.onnx` and its `.onnx_data`, 340 MB, by SHA-256), so the
> model is now seven files, 882 MB. A model downloaded before this has the first five: **Download**
> again fetches only the two new files, and until then audio is refused as a type the model does
> not embed.
>
> - Use: an `inline_data` part with `mime_type` `audio/wav`, alone or among text and image parts,
>   up to 180 seconds a request (Google's limit for `gemini-embedding-2`). A `file_data` part with a
>   `gs://bucket/object` URI, the form Google's samples use, is read from the instance's own Cloud
>   Storage (an instance started with `storage`) and embedded as if its bytes were `inline_data` of
>   the part's `mime_type`, or of the object's content type when the part names none. The console's
>   Embeddings page has an **Audio** picker and a **Cloud Storage objects** box.
> - What the model contains, read from the pinned revision's file list and model card on
>   2026-10-19: a text model, a vision encoder and an audio encoder (each in fp32, fp16, q8, q4 and
>   q4f16). There is no video encoder: the model card's video path is frames at 1 fps (at most 32,
>   uniformly subsampled) through the vision encoder at 140 soft tokens each, without the audio
>   track ("Video frames go through the vision encoder"). Decoding those frames needs an MP4/MOV
>   demuxer and an H.264, H.265, VP9 or AV1 decoder, none of which is in Go's standard library or
>   the approved modules, so video stays refused, and with it `audio_track_extraction`, which
>   Google defines as extracting a video's audio. PDF (and `document_ocr`) needs a renderer, which
>   is not available either. (MP3 is served since #1721, below.)
> - Audio is prepared as Transformers.js 4.3.1's `EmbeddingGemma2Processor` prepares it: stereo
>   downmixed as its `load_audio` does, resampled to 16 kHz with a Kaiser-windowed sinc (the
>   reference expects 16 kHz input and leaves resampling to the browser or ffmpeg), then
>   `Gemma4AudioFeatureExtractor`'s 128-bin log-mel features (frame 320, hop 160, FFT 512,
>   semicausal padding, masked partial frames); the audio encoder makes 25 soft tokens a second.
>   On the same files it gave identical vectors (cosine 1.000000) for the model card's `jfk.wav` at
>   16 kHz mono, with text, twice, and as a 44-second clip; 0.999821 for the original 44.1 kHz
>   stereo file against ffmpeg's resampling. The reference truncates a clip at 30 seconds by
>   default (the feature extractor's `max_length`, inherited from Gemma 3n); CloudBurrow embeds up
>   to Google's 180 seconds, which the model card's "~327 seconds" of context allows.
>
> **Update 2026-11-17 (#1721): MP3 audio.** An `inline_data` or `file_data` part of `mime_type`
> `audio/mpeg` (the type Google's samples send, or `audio/mp3`) is decoded with
> `github.com/hajimehoshi/go-mp3` (pure Go, Apache 2.0; see
> [upstream-evaluation.md](upstream-evaluation.md)): MPEG-1 and MPEG-2 Layer III, mono read as one
> channel and stereo downmixed as a stereo WAV is, the encoder delay and padding a LAME or Lavc
> Info frame records trimmed as ffmpeg trims them, then resampled and prepared as WAV is, within
> the same 180 seconds a request. The model card's `jfk.wav` encoded at 64 kbit/s gives the WAV's
> 279 tokens and a vector at cosine 0.999290 to the WAV's. MPEG-2.5 (8 to 12 kHz) is decoded too
> (#1734): `go-mp3` is copied into `third_party/go-mp3` and extended to it, checked against ffmpeg.
>
> **Update 2026-10-19 (#1651): WebP and BMP images.** Google's
> [Get multimodal embeddings](https://docs.cloud.google.com/vertex-ai/generative-ai/docs/embeddings/get-multimodal-embeddings)
> (re-read 2026-10-19, last updated 2026-10-15) lists "JPEG, PNG, WebP, BMP, HEIC, HEIF, AVIF" for
> `gemini-embedding-2`. With the maintainer's approval (2026-10-19), `golang.org/x/image` v0.47.0
> joins go.mod for its WebP and BMP decoders; it adds no other module.
>
> - Use: an `inline_data` or `file_data` part with `mime_type` `image/webp` (lossy, lossless, with or
>   without alpha) or `image/bmp`, decoded and then prepared exactly as PNG and JPEG are. The
>   console's **Images** picker accepts them.
> - A lossy WebP is YUV 4:2:0 in BT.601 limited range. `golang.org/x/image/webp` returns it as an
>   `image.YCbCr`, whose conversion is JPEG's full-range formula with the nearest chroma sample;
>   that gave cosine 0.965 against the reference on a lossy shape. CloudBurrow converts it as libwebp
>   does instead (bilinear "fancy" chroma upsampling, then libwebp's integer conversion), which is
>   pixel-identical to libwebp 1.6's `dwebp` on the unit fixtures, odd sizes and alpha included.
> - Checked against Transformers.js 4.3.1 on the same ONNX files and the same image files (CPU; sharp
>   decodes the WebPs with libwebp, and the BMPs are read as raw pixels, since sharp reads no BMP):
>   identical vectors (cosine 1.000000) for a 768 × 768 lossless WebP, lossy WebP and BMP; 0.999446
>   and 0.999502 for a 1000 × 600 lossy WebP and BMP, which are resized (the PNG of that scene gives
>   0.999502: sharp's resampling against PIL's).
> - Not served: an animated WebP (`golang.org/x/image` decodes no animation; refused with
>   `INVALID_ARGUMENT` saying so; served as its first frame since #1724), and HEIC, HEIF and AVIF,
>   for which neither Go's standard library nor `golang.org/x/image` has a decoder (HEIC and HEIF
>   served since #1724).
>
> **Update 2026-11-17 (#1732): a video's audio track.** With `embed_content_config`'s
> `audio_track_extraction` (off by default, as on Google), a video's first sound track is decoded
> (AAC-LC with `github.com/arabian9ts/aac-go`, MP3, or QuickTime PCM), cut to `video_metadata`'s
> offsets and embedded as an audio clip right after the video's frames, as EmbeddingGemma 2's
> processor loads a video's audio; it counts against the 180 seconds of audio a request. Opus,
> AC-3 and HE-AAC tracks are refused naming the codec. Since #1739 a mono or stereo Opus track is
> decoded too (`github.com/kazzmir/opus-go`, libopus transpiled to Go), within 0.71 of a 16-bit
> step of ffmpeg's libopus decode; `aac-go` is copied into `third_party/aac-go`. See
> [upstream-evaluation.md](upstream-evaluation.md).
>
> **Update 2026-11-17 (#1731): VP9 and AV1 video.** The four codecs Google lists for video are
> decoded: VP9 with `github.com/thesyncim/govpx` (copied into `third_party/govpx` for Go 1.26;
> profile 0, 8-bit 4:2:0, the profile libvpx and browsers encode by default; profiles 1 to 3 are
> refused naming the profile) and AV1 with `github.com/gen2brain/gav1d` (a pure Go port of dav1d;
> 8 to 12 bits, every chroma format). Both are bit-exact against ffmpeg on the unit fixtures; frames
> are then sampled and prepared as H.264's are. The sea turtle clip in VP9 and AV1 gives the H.264
> clip's 368 tokens and vectors at cosine 0.995 and 0.993 to it.
>
> **Update 2026-11-17 (#1724): HEIC, HEIF and animated WebP.** With the maintainer's approval
> (2026-11-17), `github.com/gen2brain/h265` v0.2.5 joins go.mod: a pure Go HEVC decoder and HEIF
> reader, MIT, no cgo and no dependencies ([upstream-evaluation.md](upstream-evaluation.md#amendment-heic-heif-avif-and-animated-webp-decoders-for-vertex-ai-embedcontent-1724)
> compares the candidates).
>
> - Use: an `inline_data` or `file_data` part with `mime_type` `image/heic` or `image/heif`, decoded
>   with the container's crop, rotation and mirror applied (as libheif applies them), then prepared
>   as PNG is; an image sequence gives its first frame. Chroma is upsampled bilinearly, as
>   `heif-dec -C bilinear` does (libheif's default is nearest-neighbour). The console's **Images**
>   picker accepts `.heic` and `.heif` files.
> - An animated WebP is embedded as its first frame, pixel-identical to libwebp's `dwebp` of that
>   frame, placed on a transparent canvas. Google's page says nothing of animation and documents no
>   refusal, so it is not refused.
> - Not checked against Transformers.js: the sharp it bundles reads no HEVC-coded HEIC.
> - Not served: AVIF, still `INVALID_ARGUMENT`. Its cgo-free decoder (`github.com/gen2brain/avif`)
>   would link libc dynamically into the static Linux CLI through `purego` unless every build sets
>   `-tags nodynamic`; that choice is the maintainer's (served since #1729, below).
>
> **Update 2026-11-17 (#1729): AVIF.** The user chose to vendor `github.com/gen2brain/avif` v0.6.0's
> WebAssembly decode path (libavif 1.4.2 and dav1d 1.5.3 compiled to WASM, and its Go glue) into
> `internal/avif` without its `purego` loader, run on `github.com/tetratelabs/wazero` v1.12.0 (pure
> Go, Apache 2.0), so no build tag is needed and the Linux CLI stays statically linked
> (`third_party/avif/PROVENANCE.md` (deleted with the decoder in #1736; in git history) records the files, their
> hashes and the deletions).
>
> - Use: an `inline_data` or `file_data` part with `mime_type` `image/avif`, 8 to 12 bits, with or
>   without alpha, decoded with the primary item's crop, rotation and mirror applied (as `heif-dec`
>   and ImageMagick apply them; libavif only reports them), then prepared as PNG is; an AVIS image
>   sequence gives its first frame. The console's **Images** picker accepts `.avif` files.
> - 4:2:0 chroma is upsampled as libavif's default (`AUTOMATIC`) does in this build, which has
>   libyuv: nearest-neighbour, within one step of `avifdec -u nearest`. Homebrew's `avifdec` (no
>   libyuv) defaults to bilinear: 3.5 a channel apart on average on a sharp-edged 32 × 24 fixture.
> - The first AVIF compiles the 3.5 MB module (about half a second on an M4 Max); each decode
>   instantiates it afresh. A 10- or 12-bit AVIF over 13312 × 13312 does not fit the module's 4 GiB
>   of memory and is `UNIMPLEMENTED` saying so (#1736); 8-bit AVIFs decode up to 16384 × 16384.
>   (Both superseded by #1736, below.)
> - Not checked against Transformers.js.
>
> **Update 2026-11-17 (#1736): AVIF on gav1d.** AVIF is now decoded with `github.com/gen2brain/gav1d`
> (already required for AV1 video, #1731): a pure Go port of dav1d with an AVIF reader, so the
> WebAssembly module, its 4 GiB memory ceiling and `internal/avif` are gone, and every AVIF Google
> takes decodes, a 10-bit 16384 × 16384 one at a peak RSS of 3.8 GB in 1.2 s on an M4 Max (avifdec:
> 2.9 GB, 10 s). An 8-bit image's YUV is converted as libavif with libyuv converts it (nearest
> chroma), a deeper one with gav1d's converter, bilinear like libavif's own; on the #1729 fixtures
> both are pixel-identical to libavif 1.4.2's references, crop, rotation, mirror, alpha and an AVIS
> sequence's first frame included.
>
> Everything below records why the official LiteRT route is still blocked.

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

> **Update 2026-11-17 (#1733): `document_ocr` on scanned PDFs.** A PDF page with no text layer (a
> scan) is no longer refused with `document_ocr`: it is rendered again at 300 DPI and read with
> Tesseract OCR (5.5.4, compiled to WebAssembly and run on wazero, `internal/ocr`), and its text
> follows the page's image as text tokens, counted against the request's token limit like any
> other text. A page with a text layer keeps PDFium's text. The models are tessdata_fast `eng`
> and `osd`, embedded in the binary, so only the Latin script is read; an upside-down page is
> turned back first. About 5 seconds a page. **Verified** 2026-11-17 with the official `aiplatform`
> client (`TestEmbedContentEmbedsVideoAndPDFThroughThePredictionClient`: the scanned invoice gains 195
> tokens of OCR text). The console's Embeddings page has a **PDFs' text (OCR)** box that sends
> `document_ocr` with each PDF (`TestEmbeddingsPageReadsAScannedPDFWithOCR`).
