# amr-wasm: opencore-amr's AMR decoders compiled to WebAssembly

CloudBurrow decodes Speech-to-Text's `AMR` and `AMR_WB` audio (#1799) with the AMR-NB and AMR-WB
decoders of opencore-amr, the library its project extracted from Android's OpenCORE multimedia
framework (PacketVideo's codecs, Apache-2.0). `build.sh` compiles them, as C99 (the library's own
`COMPILE_AS_C` option), to a single-threaded WASI (wasip1) reactor module, `amr.wasm`, that
`internal/speech/amr.go` runs on wazero (already required). No cgo: the Linux CLI stays static.
Google publishes no AMR decoder; FFmpeg's are LGPL, which CloudBurrow never links, so the
permissively licensed opencore-amr is used, as the user's 2026-11-17 rule on third-party code
allows. No prebuilt WebAssembly build is used.

| Input | Version | Source (official) | SHA-256 | Licence |
|---|---|---|---|---|
| opencore-amr | 0.1.6 | https://downloads.sourceforge.net/project/opencore-amr/opencore-amr/opencore-amr-0.1.6.tar.gz (the project's SourceForge release page) | `483eb4061088e2b34b358e47540b5d495a96cd468e361050fae615b1809dc4a1` | Apache-2.0 (`licenses/opencore-amr-LICENSE`, `licenses/opencore-NOTICE`) |
| wasi-sdk (clang, wasi-libc) | 34.0 | https://github.com/WebAssembly/wasi-sdk/releases/tag/wasi-sdk-34, the same archives and digests as `third_party/whisper-wasm` | per host in `build.sh`; sysroot `9d813544eeebe38b7b8f2244ed591de46b6db812c6dd1a257ff9f0d2a905a2be`, builtins `eee3e634dcf71aa22b1333391623cf5c9965a637dc428a27b1a858c026c587f1` | toolchain Apache-2.0 WITH LLVM-exception; linked in: wasi-libc MIT (`licenses/wasi-libc-LICENSE-MIT`), its musl part MIT (`licenses/musl-COPYRIGHT`) |

`amr.wasm` (SHA-256 in `amr.wasm.sha256`) is built from these only: the decoder sources that
`amrnb/Makefile.am` and `amrwb/Makefile.am` list (the encoder's excluded,
`-DDISABLE_AMRNB_ENCODER`), their `wrapper.cpp` interfaces and CloudBurrow's
`src/cbamr.c` (the exports `cb_nb_init`, `cb_nb_decode`, `cb_wb_init`, `cb_wb_decode`, ...,
under this repository's licence), at `-O2`, paths mapped away. Rebuilt and matched on macOS
arm64, 2026-11-20.

Test data: `internal/speech/testdata/jfk.amr` and `jfk.awb` are whisper.cpp's JFK sample (public
domain, see `third_party/whisper-wasm/PROVENANCE.md`), encoded on 2026-11-20 at 12.2 kbit/s with
opencore-amr 0.1.6's `test/amrnb-enc` (from 8 kHz, resampled with macOS `afconvert`) and at 23.85
kbit/s with vo-amrwbenc 0.1.3's `amrwb-enc` (Apache-2.0,
https://downloads.sourceforge.net/project/opencore-amr/vo-amrwbenc/vo-amrwbenc-0.1.3.tar.gz,
SHA-256 `5652b391e0f0e296417b841b02987d3fd33e6c0af342c69542cbb016a71d9d4e`), both built natively
for that alone; neither encoder is in CloudBurrow. `TestAMRDecode` checks the module's samples
against the SHA-256 of what opencore-amr's native `amrnb-dec` and `amrwb-dec` write for them.
