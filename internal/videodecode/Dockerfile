# The video frame decoder of multimodalembedding@001's AVI, FLV, MPEG and
# WMV video (#1651). Google takes those containers; their codecs (MPEG-1
# and MPEG-2 video, MPEG-4 Part 2, H.263 and Sorenson Spark, VP6, Windows
# Media Video and VC-1, Motion JPEG, H.264...) have no permissively
# licensed Go decoder, so the host's PredictionService runs this image,
# `docker run --network none --read-only`, to list a video's frames
# (ffprobe) and decode the sampled ones to raw planar YUV (ffmpeg). The
# CLI links none of FFmpeg (LGPL/GPL) and the repo holds none of its code:
# FFmpeg is Debian's package, run as a separate program in a container.
#
# The base is the Docker official debian image, pinned by its multi-arch
# index digest (dependencies.json's vertexai.videoDecodeBase records the
# same digest); ffmpeg is pinned to one Debian version through
# snapshot.debian.org, so a rebuild installs the same build.
# Re-resolve with `docker buildx imagetools inspect debian:trixie-slim`.
FROM debian:trixie-slim@sha256:a72b4433de1cf7d6bae0c4cee74d0547f58601a643b3375f070c51600d63b6b2

RUN printf '%s\n' \
      'deb [check-valid-until=no] http://snapshot.debian.org/archive/debian/20261120T000000Z trixie main' \
      'deb [check-valid-until=no] http://snapshot.debian.org/archive/debian/20261120T000000Z trixie-updates main' \
      'deb [check-valid-until=no] http://snapshot.debian.org/archive/debian-security/20261120T000000Z trixie-security main' \
      > /etc/apt/sources.list \
 && rm -f /etc/apt/sources.list.d/debian.sources \
 && apt-get update \
 && apt-get install -y --no-install-recommends ffmpeg=7:7.1.5-0+deb13u1 \
 && rm -rf /var/lib/apt/lists/*

# No network when it runs: it reads /in/video and writes to stdout.
USER 65534:65534
