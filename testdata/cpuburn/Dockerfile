# The binary is compiled on the host (GOOS=linux, CGO_ENABLED=0) and copied
# in, so the image needs no base and no network at build time.
FROM scratch
COPY cpuburn /cpuburn
ENTRYPOINT ["/cpuburn"]
