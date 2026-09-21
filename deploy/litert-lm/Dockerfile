# Build LiteRT-LM for Linux from Google's source.
#
# No LiteRT-LM release publishes a Linux artifact, but Linux is a documented,
# supported build target — `bazel build //runtime/engine:litert_lm_main`, and
# the target's BUILD file carries an explicit @platforms//os:linux case. The
# gap upstream is packaging, not capability, so CloudBurrow builds it.
#
# Debian 13 (trixie) rather than 12: Abseil needs C++20 <source_location>,
# which bookworm's default clang 14 does not have. Trixie ships clang 19 as
# the default, so the toolchain is right without patching alternatives —
# a base chosen to match the requirement beats a base worked around.
FROM debian:trixie-slim AS build

RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates curl git git-lfs clang lld python3 python3-dev \
      unzip zip build-essential pkg-config \
    && rm -rf /var/lib/apt/lists/*

# Bazelisk honours the repo's .bazelversion (7.6.1), so the build uses the
# version upstream tests with rather than whatever is newest.
ARG BAZELISK_VERSION=v1.25.0
ARG TARGETARCH
RUN curl -fsSL -o /usr/local/bin/bazel \
      "https://github.com/bazelbuild/bazelisk/releases/download/${BAZELISK_VERSION}/bazelisk-linux-${TARGETARCH}" \
    && chmod +x /usr/local/bin/bazel

# Pinned by commit, not by branch: a build that silently tracked main would
# make the binary unreproducible.
ARG LITERT_LM_REF=02e503079dda30b9c2c61289998a069a5099d4ea
RUN git lfs install --skip-repo \
    && git clone https://github.com/google-ai-edge/LiteRT-LM.git /src \
    && cd /src && git checkout "${LITERT_LM_REF}"

WORKDIR /src
# The binary is not self-contained: it links libGemmaModelConstraintProvider.so,
# which upstream ships prebuilt per platform rather than compiling, and finds it
# through a RUNPATH into Bazel's output tree. Copying only the binary produces an
# image that builds cleanly and dies on first run with
#   error while loading shared libraries: libGemmaModelConstraintProvider.so
# so the prebuilt libraries for this platform are staged alongside it. The whole
# directory goes, not just the one link-time dependency, because the other
# libraries are loaded on demand by non-CPU backends and a missing one would
# surface as an opaque dlopen failure much later.
RUN case "${TARGETARCH}" in \
      amd64) prebuilt=linux_x86_64 ;; \
      arm64) prebuilt=linux_arm64 ;; \
      *) echo "no upstream prebuilt libraries for ${TARGETARCH}" >&2; exit 1 ;; \
    esac \
    && bazel build //runtime/engine:litert_lm_main \
    && cp bazel-bin/runtime/engine/litert_lm_main /usr/local/bin/litert_lm_main \
    && mkdir -p /usr/local/lib/litert \
    && cp "prebuilt/${prebuilt}"/*.so /usr/local/lib/litert/ \
    && bazel clean --expunge

# The runtime image carries the binary and nothing that built it.
FROM debian:trixie-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates libstdc++6 \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /usr/local/bin/litert_lm_main /usr/local/bin/litert_lm_main
COPY --from=build /usr/local/lib/litert/ /usr/local/lib/litert/
# ldconfig rather than LD_LIBRARY_PATH: the variable is inherited by every
# child process and by anything the user runs in this image, and the loader
# consults the cache without it.
RUN echo /usr/local/lib/litert > /etc/ld.so.conf.d/litert.conf && ldconfig
ENTRYPOINT ["/usr/local/bin/litert_lm_main"]
