# Build LiteRT-LM for Linux from Google's source.
#
# Three binaries: litert_lm_main for generation, embedding_litert_lm_main for
# embeddings, and cloudburrow_generate (runner/, #1797), litert_lm_main's
# generation path with the sampler (temperature, top-k, top-p, seed) and the
# token limit set per request through LiteRT-LM's own C++ API. litert_lm_main
# builds its session with SessionConfig::CreateDefault() and has no flag for
# the sampler, so the API is the only way to them. The runner is compiled here,
# inside the image, so the CLI stays static.
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
#
# Both stages pin the tag's multi-arch index digest, not the tag: this image is
# a release artifact, and a mutable tag is for discovery only, never a release
# pin. dependencies.json's localai.litertLMBase records the same digest, and
# TestDockerfilesPinTheInventory fails when the two disagree. Re-resolve with
# `docker buildx imagetools inspect debian:trixie-slim`.
FROM debian:trixie-slim@sha256:a99cfc517144bc59b1978475ec53b46ecabec7e43635402ee5b77cc54cd1b20a AS build

RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates curl git git-lfs clang lld python3 python3-dev \
      unzip zip build-essential pkg-config \
    && rm -rf /var/lib/apt/lists/*

# Bazelisk honours the repo's .bazelversion (7.6.1), so the build uses the
# version upstream tests with rather than whatever is newest.
#
# The download is checked against a per-architecture SHA-256 before it is made
# executable, and a mismatch fails the build: the release asset is otherwise
# trusted on TLS alone. The sums are written here rather than taken as build
# args so a caller cannot swap them, and they are dependencies.json's
# localai.bazelisk checksums (TestDockerfilesPinTheInventory). A new version
# needs new sums: bazelisk publishes none, so hash the two release assets.
ARG BAZELISK_VERSION=v1.25.0
ARG TARGETARCH
RUN case "${TARGETARCH}" in \
      amd64) sum=fd8fdff418a1758887520fa42da7e6ae39aefc788cf5e7f7bb8db6934d279fc4 ;; \
      arm64) sum=4c8d966e40ac2c4efcc7f1a5a5cceef2c0a2f16b957e791fa7a867cce31e8fcb ;; \
      *) echo "no bazelisk checksum for ${TARGETARCH}" >&2; exit 1 ;; \
    esac \
    && curl -fsSL -o /usr/local/bin/bazel \
      "https://github.com/bazelbuild/bazelisk/releases/download/${BAZELISK_VERSION}/bazelisk-linux-${TARGETARCH}" \
    && echo "${sum}  /usr/local/bin/bazel" | sha256sum -c - \
    && chmod +x /usr/local/bin/bazel

# Pinned by commit, not by branch: a build that silently tracked main would
# make the binary unreproducible.
#
# 0082833d is main on 2026-11-18. It is not a release: no release contains
# a789b7d0 ("Apply --max_output_tokens in litert_lm_main", 2026-11-05), the
# fix #1797 needs. v0.19.1 was cut from release/v0.19.0, which diverged from
# main before it. Move to the first release that contains a789b7d0.
ARG LITERT_LM_REF=0082833dc28014489ceaa33eacaf5edbffbae518
RUN git lfs install --skip-repo \
    && git clone https://github.com/google-ai-edge/LiteRT-LM.git /src \
    && cd /src && git checkout "${LITERT_LM_REF}"

WORKDIR /src
# The runner builds as the package //cloudburrow inside upstream's workspace,
# beside the litert_lm_main it is derived from, with that target's own
# exported-symbols list (upstream does not export the file across packages).
COPY runner/ /src/cloudburrow/
RUN cp runtime/engine/litert_lm_main.exported_symbols cloudburrow/
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
    && bazel build //runtime/engine:litert_lm_main //runtime/engine:embedding_litert_lm_main \
      //cloudburrow:cloudburrow_generate \
    && cp bazel-bin/runtime/engine/litert_lm_main /usr/local/bin/litert_lm_main \
    && cp bazel-bin/cloudburrow/cloudburrow_generate /usr/local/bin/cloudburrow_generate \
    && cp bazel-bin/runtime/engine/embedding_litert_lm_main /usr/local/bin/embedding_litert_lm_main \
    && mkdir -p /usr/local/lib/litert \
    && cp "prebuilt/${prebuilt}"/*.so /usr/local/lib/litert/ \
    && bazel clean --expunge

# The runtime image carries the binary and nothing that built it.
FROM debian:trixie-slim@sha256:a99cfc517144bc59b1978475ec53b46ecabec7e43635402ee5b77cc54cd1b20a
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates libstdc++6 \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /usr/local/bin/litert_lm_main /usr/local/bin/litert_lm_main
COPY --from=build /usr/local/bin/embedding_litert_lm_main /usr/local/bin/embedding_litert_lm_main
COPY --from=build /usr/local/bin/cloudburrow_generate /usr/local/bin/cloudburrow_generate
COPY --from=build /usr/local/lib/litert/ /usr/local/lib/litert/
# ldconfig rather than LD_LIBRARY_PATH: the variable is inherited by every
# child process and by anything the user runs in this image, and the loader
# consults the cache without it.
RUN echo /usr/local/lib/litert > /etc/ld.so.conf.d/litert.conf && ldconfig
ENTRYPOINT ["/usr/local/bin/litert_lm_main"]
