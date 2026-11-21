# rnnoise-wasm: Xiph's RNNoise compiled to WebAssembly

CloudBurrow serves Speech-to-Text's `denoiserConfig.denoiseAudio` (#1799), "Denoise audio before
sending to the transcription model", with RNNoise, Xiph.Org's recurrent-network noise suppressor,
and the model its release builds in. `build.sh` compiles it to a single-threaded WASI (wasip1)
reactor module, `rnnoise.wasm`, that `internal/speech/denoise.go` runs on wazero (already
required). No cgo: the Linux CLI stays static. Google publishes no local denoiser, so a
permissively licensed third-party one is used, as the user's 2026-11-17 rule on third-party code
allows. No prebuilt WebAssembly build is used.

| Input | Version | Source (official) | SHA-256 | Licence |
|---|---|---|---|---|
| RNNoise, with its built-in model (`src/rnnoise_data.c`) | 0.2 | https://github.com/xiph/rnnoise/releases/download/v0.2/rnnoise-0.2.tar.gz (the release asset) | `90fce4b00b9ff24c08dbfe31b82ffd43bae383d85c5535676d28b0a2b11c0d37` | BSD-3-Clause (`licenses/rnnoise-COPYING`) |
| wasi-sdk (clang, wasi-libc) | 34.0 | https://github.com/WebAssembly/wasi-sdk/releases/tag/wasi-sdk-34, the same archives and digests as `third_party/whisper-wasm` | per host in `build.sh`; sysroot `9d813544eeebe38b7b8f2244ed591de46b6db812c6dd1a257ff9f0d2a905a2be`, builtins `eee3e634dcf71aa22b1333391623cf5c9965a637dc428a27b1a858c026c587f1` | toolchain Apache-2.0 WITH LLVM-exception; linked in: wasi-libc MIT (`licenses/wasi-libc-LICENSE-MIT`), its musl part MIT (`licenses/musl-COPYRIGHT`) |

`rnnoise.wasm` (SHA-256 in `rnnoise.wasm.sha256`) is built from these only: the sources
`Makefile.am`'s `RNNOISE_SOURCES` lists (no x86 run-time dispatch: the portable path), at `-O2`,
paths mapped away, and CloudBurrow's own `src/cbrnnoise.c` (the exports) and `src/os_support.h`
(the one macro, `OPUS_CLEAR`, the portable path of `src/vec.h` takes from an `os_support.h` the
release tarball does not ship), under this repository's licence. Rebuilt and matched on macOS
arm64, 2026-11-20. `TestDenoise` measured the output two frames (20 ms) behind the input, which
`Denoise` takes off.
