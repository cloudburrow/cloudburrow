#!/usr/bin/env bash
# Builds tesseract.wasm: Tesseract OCR with Leptonica, compiled with wasi-sdk
# to a WASI (wasip1) reactor module that internal/ocr runs on wazero.
#
#   third_party/tesseract-wasm/build.sh [--check]
#
# Every input is pinned by version and SHA-256 and fetched from its project's
# own GitHub release or tag. The build compiles the sources directly with the
# SDK's clang (no cmake or autotools), so it needs only bash, curl, tar and
# shasum or sha256sum. --check rebuilds and fails unless the result matches
# tesseract.wasm.sha256 (CI's tesseract-wasm job). WORK (default a temporary
# directory) holds the downloads and objects; JOBS sets the parallelism.
set -euo pipefail

TESSERACT_VERSION=5.5.4
TESSERACT_SHA256=a8dbf8b8c8ee427ff6ad1d99116502feaf59440af1adb893caf60a6576826cf4
LEPTONICA_VERSION=1.87.0
LEPTONICA_SHA256=c73363397f96eb1295602bf44d708a994ad42046c791bf03ea0505d829bdb6a7
WASI_SDK_VERSION=34.0
WASI_SDK_TAG=wasi-sdk-34

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

fetch "https://github.com/tesseract-ocr/tesseract/archive/refs/tags/$TESSERACT_VERSION.tar.gz" \
	"tesseract-$TESSERACT_VERSION.tar.gz" "$TESSERACT_SHA256"
fetch "https://github.com/DanBloomberg/leptonica/releases/download/$LEPTONICA_VERSION/leptonica-$LEPTONICA_VERSION.tar.gz" \
	"leptonica-$LEPTONICA_VERSION.tar.gz" "$LEPTONICA_SHA256"
fetch "https://github.com/WebAssembly/wasi-sdk/releases/download/$WASI_SDK_TAG/wasi-sdk-$WASI_SDK_VERSION-$host.tar.gz" \
	"wasi-sdk-$WASI_SDK_VERSION-$host.tar.gz" "$sdk_sha"

for d in "tesseract-$TESSERACT_VERSION" "leptonica-$LEPTONICA_VERSION" "wasi-sdk-$WASI_SDK_VERSION-$host"; do
	[ -d "$d" ] || tar xzf "$d.tar.gz"
done
sdk=$work/wasi-sdk-$WASI_SDK_VERSION-$host
tess=$work/tesseract-$TESSERACT_VERSION
lept=$work/leptonica-$LEPTONICA_VERSION

# Generated headers: Leptonica's config (no codec libraries) and endianness,
# Tesseract's version.
gen=$work/gen
mkdir -p "$gen/leptonica" "$gen/tesseract"
cp "$here/src/leptonica_config_auto.h" "$gen/leptonica/config_auto.h"
sed 's/@APPLE_UNIVERSAL_BUILD@/0/; s/@ENDIANNESS@/L_LITTLE_ENDIAN/' "$lept/src/endianness.h.in" >"$gen/leptonica/endianness.h"
cp "$lept"/src/*.h "$gen/leptonica/"
IFS=. read -r major minor micro <<<"$TESSERACT_VERSION"
sed "s/@GENERIC_MAJOR_VERSION@/$major/; s/@GENERIC_MINOR_VERSION@/$minor/; s/@GENERIC_MICRO_VERSION@/$micro/; s/@PACKAGE_VERSION@/$TESSERACT_VERSION/" \
	"$tess/include/tesseract/version.h.in" >"$gen/tesseract/version.h"
cp "$tess"/include/tesseract/*.h "$gen/tesseract/"

# Fixed flags, and paths mapped away, so the module is the same on every host.
common=(--target=wasm32-wasip1 --sysroot="$sdk/share/wasi-sysroot" -O2 -msimd128 -DNDEBUG
	-fno-exceptions -D_WASI_EMULATED_SIGNAL -D_WASI_EMULATED_PROCESS_CLOCKS -D_WASI_EMULATED_MMAN
	-ffile-prefix-map="$work"=/build -ffile-prefix-map="$here"=/src -Wno-everything)
cc=("$sdk/bin/clang" "${common[@]}" -std=gnu11 -DHAVE_CONFIG_H -I"$gen/leptonica")
cxx=("$sdk/bin/clang++" "${common[@]}" -std=c++17 -DGRAPHICS_DISABLED -DTESSERACT_DISABLE_DEBUG_FONTS
	-DHAVE_CONFIG_H -I"$gen" -I"$gen/leptonica")

obj=$work/obj
rm -rf "$obj"
mkdir -p "$obj"
jobs=${JOBS:-$(getconf _NPROCESSORS_ONLN 2>/dev/null || echo 4)}

# Leptonica: every library source; the codec readers compile to their stubs.
lept_srcs=$(cd "$lept/src" && ls ./*.c | sed 's|^\./||' | LC_ALL=C sort)
# Tesseract: the library without training, the SIMD back ends (WebAssembly
# has the generic ones) or the viewer's sockets (svutil; GRAPHICS_DISABLED).
tess_srcs=$(cd "$tess/src" && find api arch ccmain ccstruct ccutil classify cutil dict lstm textord wordrec viewer \
	-name '*.cpp' | LC_ALL=C sort | grep -Ev '^arch/(dotproduct|intsimdmatrix)(avx|avx2|avx512|fma|neon|sse|rvv)\.cpp$|^viewer/svutil\.cpp$')

tess_inc=()
for d in api arch ccmain ccstruct ccutil classify cutil dict lstm textord viewer wordrec; do tess_inc+=(-I"$tess/src/$d"); done
: >"$gen/config_auto.h" # Tesseract: defaults (legacy engine kept: OSD needs it)

printf '%s\n' $lept_srcs | xargs -P "$jobs" -I{} sh -c 's=$0 o=$1; shift; "$@" -c "$s/src/{}" -o "$o/lept_$(echo {} | tr / _).o"' "$lept" "$obj" "${cc[@]}"
printf '%s\n' $tess_srcs | xargs -P "$jobs" -I{} sh -c 's=$0 o=$1; shift; "$@" -c "$s/src/{}" -o "$o/tess_$(echo {} | tr / _).o"' "$tess" "$obj" "${cxx[@]}" "${tess_inc[@]}"
"${cxx[@]}" -c "$here/src/cbocr.cpp" -o "$obj/cbocr.o"
"${cc[@]}" -c "$here/src/wasi_stubs.c" -o "$obj/wasi_stubs.o"

out=$work/tesseract.wasm
"$sdk/bin/clang++" "${common[@]}" -mexec-model=reactor -Wl,--strip-all -Wl,-z,stack-size=8388608 \
	-Wl,--initial-memory=67108864 -Wl,--max-memory=4294967296 \
	-lwasi-emulated-signal -lwasi-emulated-process-clocks -lwasi-emulated-mman \
	$(cd "$obj" && ls ./*.o | LC_ALL=C sort | sed "s|^\./|$obj/|") -o "$out"

sum=$(sha256 "$out")
echo "tesseract.wasm $(wc -c <"$out" | tr -d ' ') bytes, SHA-256 $sum"
if [ "$check" = 1 ]; then
	want=$(cut -d' ' -f1 "$here/tesseract.wasm.sha256")
	if [ "$sum" != "$want" ]; then echo "build.sh: rebuilt tesseract.wasm is $sum, committed $want" >&2; exit 1; fi
	echo "matches tesseract.wasm.sha256"
else
	cp "$out" "$here/tesseract.wasm"
	echo "$sum  tesseract.wasm" >"$here/tesseract.wasm.sha256"
fi
