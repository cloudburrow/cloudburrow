# tesseract-wasm: Tesseract OCR compiled to WebAssembly

CloudBurrow reads scanned PDF pages for Vertex AI EmbedContent's `document_ocr` (#1733) with
Tesseract, the LSTM OCR engine Google developed and open-sourced, compiled by `build.sh` to a
WASI (wasip1) reactor module, `tesseract.wasm`, that `internal/ocr` runs on wazero
(github.com/tetratelabs/wazero, already required). No cgo: the Linux CLI stays static. Neither
tesseract.js nor any other prebuilt WebAssembly build is used.

| Input | Version | Source (official) | SHA-256 | Licence |
|---|---|---|---|---|
| Tesseract | 5.5.4 (tag object `c5065e4c`, commit `7e548e6eff172f38f97664afe7f7ce40db36c8d6`) | https://github.com/tesseract-ocr/tesseract/archive/refs/tags/5.5.4.tar.gz | `a8dbf8b8c8ee427ff6ad1d99116502feaf59440af1adb893caf60a6576826cf4` | Apache-2.0 (`licenses/tesseract-LICENSE`) |
| Leptonica | 1.87.0 | https://github.com/DanBloomberg/leptonica/releases/download/1.87.0/leptonica-1.87.0.tar.gz | `c73363397f96eb1295602bf44d708a994ad42046c791bf03ea0505d829bdb6a7` | BSD-2-Clause (`licenses/leptonica-LICENSE`) |
| wasi-sdk (clang, wasi-libc, libc++) | 34.0 (wasi-libc `2e6fb9d8`, llvm-project `895aa2c8`) | https://github.com/WebAssembly/wasi-sdk/releases/tag/wasi-sdk-34: the host's `wasi-sdk-34.0-<host>.tar.gz` for clang, and the host-independent `wasi-sysroot-34.0.tar.gz` and `libclang_rt-34.0.tar.gz` for what is linked in | per host in `build.sh`; sysroot `9d813544eeebe38b7b8f2244ed591de46b6db812c6dd1a257ff9f0d2a905a2be`, builtins `eee3e634dcf71aa22b1333391623cf5c9965a637dc428a27b1a858c026c587f1` (the release's own digests) | toolchain Apache-2.0 WITH LLVM-exception; linked in: wasi-libc MIT (`licenses/wasi-libc-LICENSE-MIT`), its musl part MIT (`licenses/musl-COPYRIGHT`), libc++ Apache-2.0 WITH LLVM-exception (`licenses/libcxx-LICENSE.TXT`) |
| tessdata_fast `eng.traineddata` | 4.1.0 (tag object `a8ba5063`) | https://github.com/tesseract-ocr/tessdata_fast/raw/4.1.0/eng.traineddata | `7d4322bd2a7749724879683fc3912cb542f19906c83bcc1a52132556427170b2` | Apache-2.0 (`licenses/tessdata_fast-LICENSE`) |
| tessdata_fast `osd.traineddata` | 4.1.0 | https://github.com/tesseract-ocr/tessdata_fast/raw/4.1.0/osd.traineddata | `9cf5d576fcc47564f11265841e5ca839001e7e6f38ff7f7aacf46d15a96b00ff` | Apache-2.0 |

| tessdata_fast `script/*.traineddata` (36 models: every script but Latin) | 4.1.0 (commit `65727574dfcd264acbb0c3e07860e4e9e9b22185`) | https://raw.githubusercontent.com/tesseract-ocr/tessdata_fast/65727574dfcd264acbb0c3e07860e4e9e9b22185/script/ | each pinned in `internal/localai/tessdata.go` | Apache-2.0 |

The script models are not committed: `internal/localai` fetches one the first time a page in its
script is read (orientation and script detection names the script; `internal/ocr`'s
`ScriptModels` maps it to a model) into `<state-dir>/models`, and checks it against the SHA-256
pinned in `internal/localai/tessdata.go`. Those SHA-256s and sizes were measured on 2026-11-17
on the files downloaded from the commit above, each of which also matched the git blob ID
GitHub's contents API reports for it.

The two embedded models are committed gzipped (`tessdata/*.traineddata.gz`, `gzip -9 -n`) so that each
tracked file stays under 5 MB; `internal/ocr` decompresses them into memory and checks the
SHA-256s above before loading them.

`tesseract.wasm` (SHA-256 in `tesseract.wasm.sha256`) is built from the first three only, and is
the same bytes on every host: checked 2026-11-17 on macOS arm64, and in Linux arm64 and x86_64
containers (Debian bookworm), each giving the committed SHA-256. Two things made it so. Each host's
SDK carries its own build of the sysroot and compiler-rt (the macOS ones differ, if only in the
source paths in libc++abi's assertions), so the sysroot and builtins come from the release's
host-independent archives instead. And at `-O2` the x86_64 Linux clang orders two stack
temporaries of `ccstruct/quadlsq.cpp`'s `long double` (fp128, soft-float calls in wasm32) the other
way from the arm64 ones; that one file is compiled at `-O0`, where every host agrees. The
build compiles every Leptonica library source and Tesseract's library sources (`src/` without
`training/`, the x86, Arm and RISC-V SIMD back ends, and `viewer/svutil.cpp`, the viewer's
sockets) directly with the SDK's clang at `-O2 -msimd128`, `-fno-exceptions`,
`GRAPHICS_DISABLED`, and with the legacy engine kept, which orientation and script detection
needs. Leptonica is configured without any image codec library (`src/leptonica_config_auto.h`):
Tesseract receives raw pixels. CloudBurrow's own sources, under this repository's licence:

- `src/cbocr.cpp`: the module's exports (`cb_init`, `cb_set_image`, `cb_set_psm`, `cb_get_text`,
  `cb_osd`, ...), a thin layer over `tesseract::TessBaseAPI`;
- `src/wasi_stubs.c`: `system` and `mkstemp`, which WASI lacks and Leptonica refers to for its
  debug display and temporary files, failing with `ENOSYS`;
- `tesseractwasm.go`: embeds the module and `tessdata/`.

To rebuild: `third_party/tesseract-wasm/build.sh` (needs bash, curl, tar and a SHA-256 tool;
`WORK=dir` keeps the downloads). `build.sh --check` rebuilds and fails unless the result matches
`tesseract.wasm.sha256`; CI's `tesseract-wasm` job runs it (not a required check). To update an
input: change its version and SHA-256 in `build.sh`, rebuild, record it here and in
`dependencies.json` (`tesseractWasm`), and run `go test ./internal/ocr/ ./internal/embedding/`.
