# whisper-wasm: whisper.cpp compiled to WebAssembly

CloudBurrow transcribes audio for BigQuery ML's `ML.TRANSCRIBE` (#1799) with whisper.cpp, the
C/C++ port of OpenAI's Whisper speech recognition model, compiled by `build.sh` to a
single-threaded WASI (wasip1) reactor module, `whisper.wasm`, that `internal/speech` runs on
wazero (github.com/tetratelabs/wazero, already required). No cgo: the Linux CLI stays static.
Google publishes no speech recognition model that runs locally (Chirp is served only by the
Speech-to-Text API, which CloudBurrow never calls), so a third-party model is used, as the
user's 2026-11-17 rule on third-party code allows. No prebuilt WebAssembly build is used.

| Input | Version | Source (official) | SHA-256 | Licence |
|---|---|---|---|---|
| whisper.cpp (with its ggml) | 1.9.5 (tag object `d2a764c6`) | https://github.com/ggml-org/whisper.cpp/archive/refs/tags/v1.9.5.tar.gz | `ff1a9053feb509ff9d7729703355541ae9690073a6b1c40eb692c962e0dc1720` | MIT (`licenses/whisper.cpp-LICENSE`) |
| wasi-sdk (clang, wasi-libc, libc++, libc++abi, libunwind) | 34.0 | https://github.com/WebAssembly/wasi-sdk/releases/tag/wasi-sdk-34, the same archives and digests as `third_party/tesseract-wasm` | per host in `build.sh`; sysroot `9d813544eeebe38b7b8f2244ed591de46b6db812c6dd1a257ff9f0d2a905a2be`, builtins `eee3e634dcf71aa22b1333391623cf5c9965a637dc428a27b1a858c026c587f1` | toolchain Apache-2.0 WITH LLVM-exception; linked in: wasi-libc MIT (`licenses/wasi-libc-LICENSE-MIT`), its musl part MIT (`licenses/musl-COPYRIGHT`), libc++, libc++abi and libunwind Apache-2.0 WITH LLVM-exception (`licenses/libcxx-LICENSE.TXT`) |
| Whisper base, multilingual, ggml q8_0 (`ggml-base-q8_0.bin`) | revision `5359861c739e955e79d9a303bcbc70fb988958b1` of the official whisper.cpp model repository | https://huggingface.co/ggerganov/whisper.cpp/resolve/5359861c739e955e79d9a303bcbc70fb988958b1/ggml-base-q8_0.bin | `c577b9a86e7e048a0b7eada054f4dd79a56bbfa911fbdacf900ac5b567cbb7d9` (81768585 bytes) | MIT (OpenAI's Whisper weights; the repository's licence) |

The model is not committed or embedded: `internal/localai` (`whisper.go`) fetches it the first
time audio is transcribed, into `<state-dir>/models`, and checks it against the SHA-256 above,
which is Hugging Face's LFS object ID for the file and matched the file downloaded from the
revision on 2026-11-18. `internal/speech/testdata/jfk.wav` is whisper.cpp's own sample
(`samples/jfk.wav` of the same release: eleven seconds of John F. Kennedy's 1961 inaugural
address, a US government work in the public domain). `internal/speech/testdata/adaptation.wav`
(16 kHz LINEAR16, "We met Siobhan Nyhuis at the Kildrummy station.") was generated for the
adaptation test on 2026-11-19 with macOS's `say` text-to-speech; it contains no third-party
recording.

`whisper.wasm` (SHA-256 in `whisper.wasm.sha256`) is built from the first two only. The build
compiles ggml's base and CPU backend (with its WebAssembly SIMD128 quantised kernels,
`ggml-cpu/arch/wasm/quants.c`) and `src/whisper.cpp` directly with the SDK's clang at
`-O2 -msimd128`, without cmake. whisper.cpp and ggml throw C++ exceptions, so they are compiled
with `-fwasm-exceptions` in the standard exnref form (`-mllvm -wasm-use-legacy-eh=false`) and
linked with the sysroot's exception-handling libc++ and libunwind; wazero runs them with
`experimental.CoreFeaturesExceptionHandling`. The module is single-threaded: wasm32-wasip1
has no threads, and every transcription uses one (`n_threads = 1`). As for tesseract.wasm, the
sysroot and builtins come from the release's host-independent archives and paths are mapped
away; checked 2026-11-18 on macOS arm64. CloudBurrow's own sources, under this repository's
licence:

- `src/cbwhisper.cpp`: the module's exports (`cb_init`, `cb_full`, `cb_full_adapt`,
  `cb_n_segments`, `cb_segment_text`, `cb_segment_times`, `cb_token_data`, `cb_lang`, ...), a thin
  layer over `include/whisper.h`. `cb_full_adapt` (#1930, Speech-to-Text adaptation) passes an
  initial prompt (`initial_prompt`, `carry_initial_prompt`) and installs whisper.cpp's
  `logits_filter_callback` with a phrase-biasing filter: each phrase is tokenized with
  `whisper_tokenize`, and the logit of the token that starts it, or continues it where the
  decoded text ends with a prefix of it, is raised by the phrase's bias;
- `whisperwasm.go`: embeds the module.

To rebuild: `third_party/whisper-wasm/build.sh` (needs bash, curl, tar and a SHA-256 tool;
`WORK=dir` keeps the downloads). `build.sh --check` rebuilds and fails unless the result matches
`whisper.wasm.sha256`; CI's `whisper-wasm` job runs it (not a required check). To update an
input: change its version and SHA-256 in `build.sh` (or the model's pin in
`internal/localai/whisper.go`), rebuild, record it here and in `dependencies.json`
(`whisperWasm`), and run `go test ./internal/speech/ ./internal/service/vertexai/`.
