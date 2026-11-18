#!/usr/bin/env bash
# Builds whisper.wasm: whisper.cpp (with its ggml) compiled with wasi-sdk to
# a single-threaded WASI (wasip1) reactor module that internal/speech runs
# on wazero.
#
#   third_party/whisper-wasm/build.sh [--check]
#
# Every input is pinned by version and SHA-256 and fetched from its
# project's own GitHub release or tag. The build compiles the sources
# directly with the SDK's clang (no cmake), so it needs only bash, curl, tar
# and shasum or sha256sum. --check rebuilds and fails unless the result
# matches whisper.wasm.sha256. WORK (default a temporary directory) holds
# the downloads and objects; JOBS sets the parallelism.
set -euo pipefail

WHISPER_VERSION=1.9.5
WHISPER_SHA256=ff1a9053feb509ff9d7729703355541ae9690073a6b1c40eb692c962e0dc1720
WASI_SDK_VERSION=34.0
WASI_SDK_TAG=wasi-sdk-34
# As in third_party/tesseract-wasm/build.sh: the sysroot (wasi-libc,
# libc++) and compiler-rt builtins come from the release's host-independent
# archives, so the module is the same bytes on every host.
WASI_SYSROOT_SHA256=9d813544eeebe38b7b8f2244ed591de46b6db812c6dd1a257ff9f0d2a905a2be
WASI_LIBCLANG_RT_SHA256=eee3e634dcf71aa22b1333391623cf5c9965a637dc428a27b1a858c026c587f1

here=$(cd "$(dirname "$0")" && pwd)
check=0
[ "${1:-}" = --check ] && check=1

case "$(uname -s)-$(uname -m)" in
Linux-x86_64) host=x86_64-linux sdk_sha=b761e3a0721dbae9c09a0059e5fdb2bf917d1b4a8a7b430fb3b5aafb0984b2c4 ;;
Linux-aarch64 | Linux-arm64) host=arm64-linux sdk_sha=f7e243dff54d60bcc576e94d6166b69f410f2500ae4a9ceef34315be10e77971 ;;
Darwin-arm64) host=arm64-macos sdk_sha=9c59398106b417f8f14913380fdf0097a8cc0ff4af9eb3ce0065a859e88d49e9 ;;
Darwin-x86_64) host=x86_64-macos sdk_sha=87d27fa8adc68dee59bfbf2e22a6d34ef717c34d6bf1d8af2a56fc929d9ce0eb ;;
*) echo "build.sh: no wasi-sdk release for $(uname -s) $(uname -m)" >&2; exit 1 ;;
esac

sha256() { if command -v sha256sum >/dev/null; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi; }

work=${WORK:-$(mktemp -d)}
mkdir -p "$work"
cd "$work"

fetch() { # url file sha256
	if [ ! -f "$2" ]; then curl -fsSL --retry 3 -o "$2.part" "$1" && mv "$2.part" "$2"; fi
	got=$(sha256 "$2")
	if [ "$got" != "$3" ]; then echo "build.sh: $2 has SHA-256 $got, want $3" >&2; exit 1; fi
}

fetch "https://github.com/ggml-org/whisper.cpp/archive/refs/tags/v$WHISPER_VERSION.tar.gz" \
	"whisper.cpp-$WHISPER_VERSION.tar.gz" "$WHISPER_SHA256"
fetch "https://github.com/WebAssembly/wasi-sdk/releases/download/$WASI_SDK_TAG/wasi-sdk-$WASI_SDK_VERSION-$host.tar.gz" \
	"wasi-sdk-$WASI_SDK_VERSION-$host.tar.gz" "$sdk_sha"
fetch "https://github.com/WebAssembly/wasi-sdk/releases/download/$WASI_SDK_TAG/wasi-sysroot-$WASI_SDK_VERSION.tar.gz" \
	"wasi-sysroot-$WASI_SDK_VERSION.tar.gz" "$WASI_SYSROOT_SHA256"
fetch "https://github.com/WebAssembly/wasi-sdk/releases/download/$WASI_SDK_TAG/libclang_rt-$WASI_SDK_VERSION.tar.gz" \
	"libclang_rt-$WASI_SDK_VERSION.tar.gz" "$WASI_LIBCLANG_RT_SHA256"

for d in "whisper.cpp-$WHISPER_VERSION" "wasi-sdk-$WASI_SDK_VERSION-$host" \
	"wasi-sysroot-$WASI_SDK_VERSION" "libclang_rt-$WASI_SDK_VERSION"; do
	[ -d "$d" ] || tar xzf "$d.tar.gz"
done
sdk=$work/wasi-sdk-$WASI_SDK_VERSION-$host
sysroot=$work/wasi-sysroot-$WASI_SDK_VERSION
rtdir=$("$sdk/bin/clang" --target=wasm32-wasip1 -print-resource-dir)/lib/wasm32-unknown-wasip1
cp "$work/libclang_rt-$WASI_SDK_VERSION/wasm32-unknown-wasip1/libclang_rt.builtins.a" "$rtdir/libclang_rt.builtins.a"
src=$work/whisper.cpp-$WHISPER_VERSION
ggml=$src/ggml

# Generated header: ggml's version (whisper.cpp's CMake writes the same).
gen=$work/gen
mkdir -p "$gen"
sed "s/@GGML_VERSION@/0.0.0/; s/@GGML_BUILD_COMMIT@/v$WHISPER_VERSION/" "$ggml/src/ggml-version.h.in" >"$gen/ggml-version.h"

# Fixed flags, and paths mapped away, so the module is the same on every
# host. GGML_USE_CPU registers the CPU backend, the only one; the wasm
# quants (arch/wasm/quants.c) use SIMD128.
common=(--target=wasm32-wasip1 --sysroot="$sysroot" -O2 -msimd128 -DNDEBUG
	-fwasm-exceptions -mllvm -wasm-use-legacy-eh=false -D_WASI_EMULATED_SIGNAL -D_WASI_EMULATED_PROCESS_CLOCKS -D_WASI_EMULATED_MMAN
	-D_GNU_SOURCE -DGGML_USE_CPU -DGGML_SCHED_MAX_COPIES=4 -DGGML_VERSION_NO_BUILD
	-DWHISPER_VERSION=\"$WHISPER_VERSION\"
	-I"$gen" -I"$ggml/include" -I"$ggml/src" -I"$ggml/src/ggml-cpu" -I"$src/include"
	-ffile-prefix-map="$work"=/build -ffile-prefix-map="$here"=/src -Wno-everything)
cc=("$sdk/bin/clang" "${common[@]}" -std=gnu11)
cxx=("$sdk/bin/clang++" "${common[@]}" -std=c++17)

obj=$work/obj
rm -rf "$obj"
mkdir -p "$obj"
jobs=${JOBS:-$(getconf _NPROCESSORS_ONLN 2>/dev/null || echo 4)}

# ggml-base and ggml (ggml/src/CMakeLists.txt), the CPU backend without the
# x86 AMX, KleidiAI, llamafile or HBM extras (ggml-cpu/CMakeLists.txt,
# with its wasm quants), and libwhisper (src/whisper.cpp; aneforge is Apple's
# Neural Engine and compiles to nothing elsewhere).
srcs="ggml/src/ggml.c ggml/src/ggml.cpp ggml/src/ggml-alloc.c ggml/src/ggml-backend.cpp ggml/src/ggml-backend-meta.cpp
ggml/src/ggml-opt.cpp ggml/src/ggml-threading.cpp ggml/src/ggml-quants.c ggml/src/gguf.cpp ggml/src/ggml-backend-reg.cpp
ggml/src/ggml-cpu/ggml-cpu.c ggml/src/ggml-cpu/ggml-cpu.cpp ggml/src/ggml-cpu/repack.cpp ggml/src/ggml-cpu/quants.c
ggml/src/ggml-cpu/traits.cpp ggml/src/ggml-cpu/binary-ops.cpp ggml/src/ggml-cpu/unary-ops.cpp ggml/src/ggml-cpu/vec.cpp
ggml/src/ggml-cpu/ops.cpp ggml/src/ggml-cpu/tiled/tiled.cpp ggml/src/ggml-cpu/tiled/tiled-kernel.cpp
ggml/src/ggml-cpu/arch/wasm/quants.c src/whisper.cpp src/aneforge/whisper-aneforge.cpp"

c_srcs=$(printf '%s\n' $srcs | grep '\.c$' | LC_ALL=C sort)
cxx_srcs=$(printf '%s\n' $srcs | grep '\.cpp$' | LC_ALL=C sort)
printf '%s\n' $c_srcs | xargs -P "$jobs" -I{} sh -c 's=$0 o=$1; shift; "$@" -c "$s/{}" -o "$o/$(echo {} | tr / _).o"' "$src" "$obj" "${cc[@]}"
printf '%s\n' $cxx_srcs | xargs -P "$jobs" -I{} sh -c 's=$0 o=$1; shift; "$@" -c "$s/{}" -o "$o/$(echo {} | tr / _).o"' "$src" "$obj" "${cxx[@]}"
"${cxx[@]}" -c "$here/src/cbwhisper.cpp" -o "$obj/cbwhisper.o"

out=$work/whisper.wasm
"$sdk/bin/clang++" "${common[@]}" -mexec-model=reactor -Wl,--strip-all -Wl,-z,stack-size=8388608 \
	-Wl,--initial-memory=67108864 -Wl,--max-memory=4294967296 \
	-lwasi-emulated-signal -lwasi-emulated-process-clocks -lwasi-emulated-mman -ldl -lunwind \
	$(cd "$obj" && ls ./*.o | LC_ALL=C sort | sed "s|^\./|$obj/|") -o "$out"

sum=$(sha256 "$out")
echo "whisper.wasm $(wc -c <"$out" | tr -d ' ') bytes, SHA-256 $sum"
if [ "$check" = 1 ]; then
	want=$(cut -d' ' -f1 "$here/whisper.wasm.sha256")
	if [ "$sum" != "$want" ]; then echo "build.sh: rebuilt whisper.wasm is $sum, committed $want" >&2; exit 1; fi
	echo "matches whisper.wasm.sha256"
else
	cp "$out" "$here/whisper.wasm"
	echo "$sum  whisper.wasm" >"$here/whisper.wasm.sha256"
fi
