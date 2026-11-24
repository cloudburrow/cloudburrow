# The binary is compiled on the host (CGO off, so static) and copied in:
# the image needs no base and no network at build time.
FROM scratch
COPY vpcprobe /vpcprobe
ENTRYPOINT ["/vpcprobe"]
