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
RUN bazel build //runtime/engine:litert_lm_main \
    && cp bazel-bin/runtime/engine/litert_lm_main /usr/local/bin/litert_lm_main \
    && bazel clean --expunge

# The runtime image carries the binary and nothing that built it.
FROM debian:trixie-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates libstdc++6 \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /usr/local/bin/litert_lm_main /usr/local/bin/litert_lm_main
ENTRYPOINT ["/usr/local/bin/litert_lm_main"]
