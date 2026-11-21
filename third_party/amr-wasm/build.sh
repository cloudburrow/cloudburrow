#!/usr/bin/env bash
# Builds amr.wasm: opencore-amr's AMR-NB and AMR-WB decoders (Apache-2.0,
# from Android's OpenCORE) compiled as C with wasi-sdk to a single-threaded
# WASI (wasip1) reactor module that internal/speech runs on wazero.
#
#   third_party/amr-wasm/build.sh [--check]
#
# Every input is pinned by version and SHA-256 and fetched from its
# project's own release (opencore-amr's SourceForge release page, wasi-sdk's
# GitHub release, the same archives as third_party/whisper-wasm). The build
# compiles the sources directly with the SDK's clang (no configure), the
# source lists read from opencore-amr's own amrnb/Makefile.am and
# amrwb/Makefile.am, decoders only. --check rebuilds and fails unless the
# result matches amr.wasm.sha256. WORK (default a temporary directory) holds
# the downloads and objects.
set -euo pipefail

OPENCORE_AMR_VERSION=0.1.6
OPENCORE_AMR_SHA256=483eb4061088e2b34b358e47540b5d495a96cd468e361050fae615b1809dc4a1
WASI_SDK_VERSION=34.0
WASI_SDK_TAG=wasi-sdk-34
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

fetch "https://downloads.sourceforge.net/project/opencore-amr/opencore-amr/opencore-amr-$OPENCORE_AMR_VERSION.tar.gz" \
	"opencore-amr-$OPENCORE_AMR_VERSION.tar.gz" "$OPENCORE_AMR_SHA256"
fetch "https://github.com/WebAssembly/wasi-sdk/releases/download/$WASI_SDK_TAG/wasi-sdk-$WASI_SDK_VERSION-$host.tar.gz" \
	"wasi-sdk-$WASI_SDK_VERSION-$host.tar.gz" "$sdk_sha"
fetch "https://github.com/WebAssembly/wasi-sdk/releases/download/$WASI_SDK_TAG/wasi-sysroot-$WASI_SDK_VERSION.tar.gz" \
	"wasi-sysroot-$WASI_SDK_VERSION.tar.gz" "$WASI_SYSROOT_SHA256"
fetch "https://github.com/WebAssembly/wasi-sdk/releases/download/$WASI_SDK_TAG/libclang_rt-$WASI_SDK_VERSION.tar.gz" \
	"libclang_rt-$WASI_SDK_VERSION.tar.gz" "$WASI_LIBCLANG_RT_SHA256"

for d in "opencore-amr-$OPENCORE_AMR_VERSION" "wasi-sdk-$WASI_SDK_VERSION-$host" \
	"wasi-sysroot-$WASI_SDK_VERSION" "libclang_rt-$WASI_SDK_VERSION"; do
	[ -d "$d" ] || tar xzf "$d.tar.gz"
done
sdk=$work/wasi-sdk-$WASI_SDK_VERSION-$host
sysroot=$work/wasi-sysroot-$WASI_SDK_VERSION
rtdir=$("$sdk/bin/clang" --target=wasm32-wasip1 -print-resource-dir)/lib/wasm32-unknown-wasip1
mkdir -p "$rtdir"
cp "$work/libclang_rt-$WASI_SDK_VERSION/wasm32-unknown-wasip1/libclang_rt.builtins.a" "$rtdir/libclang_rt.builtins.a"
src=$work/opencore-amr-$OPENCORE_AMR_VERSION
amr=$src/opencore/codecs_v2/audio/gsm_amr

# The include paths of amrnb/Makefile.am and amrwb/Makefile.am; the .cpp
# sources compiled as C99, as their COMPILE_AS_C option does.
inc=(-I"$src/oscl" -I"$amr/amr_nb/dec/src" -I"$amr/amr_nb/common/include" -I"$amr/amr_nb/dec/include"
	-I"$amr/common/dec/include" -I"$amr/amr_nb/enc/src" -I"$amr/amr_wb/dec/src" -I"$amr/amr_wb/dec/include"
	-I"$src/amrnb" -I"$src/amrwb")
cc=("$sdk/bin/clang" --target=wasm32-wasip1 --sysroot="$sysroot" -O2 -DNDEBUG -DDISABLE_AMRNB_ENCODER
	"${inc[@]}" -ffile-prefix-map="$work"=/build -ffile-prefix-map="$here"=/src -Wno-everything)

obj=$work/obj
rm -rf "$obj"
mkdir -p "$obj"

# sources <Makefile.am> <prefix>: the decoder sources the Makefile lists
# (not its comments, not the encoder's), as paths under the source tree.
sources() {
	grep -v '^[[:space:]]*#' "$1" | grep -oE '\$\((DEC_SRC_DIR|COMMON_SRC_DIR)\)/[A-Za-z0-9_]+\.cpp' |
		sed "s|\$(DEC_SRC_DIR)|$2/dec/src|; s|\$(COMMON_SRC_DIR)|$2/common/src|" | LC_ALL=C sort -u
}
nb=$(sources "$src/amrnb/Makefile.am" "$amr/amr_nb")
wb=$(sources "$src/amrwb/Makefile.am" "$amr/amr_wb")
n=0
for f in $nb "$src/amrnb/wrapper.cpp"; do
	"${cc[@]}" -x c -std=c99 -c "$f" -o "$obj/nb_$(basename "$f" .cpp).o"
	n=$((n + 1))
done
for f in $wb "$src/amrwb/wrapper.cpp"; do
	"${cc[@]}" -x c -std=c99 -c "$f" -o "$obj/wb_$(basename "$f" .cpp).o"
	n=$((n + 1))
done
"${cc[@]}" -std=c99 -c "$here/src/cbamr.c" -o "$obj/cbamr.o"
echo "compiled $n opencore-amr sources"

out=$work/amr.wasm
"$sdk/bin/clang" --target=wasm32-wasip1 --sysroot="$sysroot" -O2 -mexec-model=reactor -Wl,--strip-all \
	-Wl,-z,stack-size=1048576 -Wl,--initial-memory=4194304 \
	$(cd "$obj" && ls ./*.o | LC_ALL=C sort | sed "s|^\./|$obj/|") -o "$out"

sum=$(sha256 "$out")
echo "amr.wasm $(wc -c <"$out" | tr -d ' ') bytes, SHA-256 $sum"
if [ "$check" = 1 ]; then
	want=$(cut -d' ' -f1 "$here/amr.wasm.sha256")
	if [ "$sum" != "$want" ]; then echo "build.sh: rebuilt amr.wasm is $sum, committed $want" >&2; exit 1; fi
	echo "matches amr.wasm.sha256"
else
	cp "$out" "$here/amr.wasm"
	echo "$sum  amr.wasm" >"$here/amr.wasm.sha256"
fi
