# The binary is compiled on the host with the repository's own pinned module
# versions and copied in, so the image needs no network at build time and the
# SDK it exercises is exactly the one the host tests use.
FROM gcr.io/distroless/static-debian12@sha256:d75cdd72874d4790092fcb1b058493ecf6bb5bf2b2b897045b00ff01d91843f2
COPY spannerprobe /spannerprobe
ENTRYPOINT ["/spannerprobe"]
