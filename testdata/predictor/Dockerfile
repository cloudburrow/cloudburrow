# Base images are pinned by digest, so the fixture that proved the contract is
# the fixture a later run rebuilds. Both are multi-arch manifest lists, so the
# same digest resolves on amd64 and arm64.
FROM golang:1.27-alpine@sha256:8a5910f31396cd4d89662f56c68b3ae31d374308270a1c3bd96672ee5ed43414 AS build
WORKDIR /src
COPY main.go .
RUN go mod init predictor && CGO_ENABLED=0 go build -trimpath -o /predictor main.go

FROM gcr.io/distroless/static-debian12@sha256:d75cdd72874d4790092fcb1b058493ecf6bb5bf2b2b897045b00ff01d91843f2
COPY --from=build /predictor /predictor
ENTRYPOINT ["/predictor"]
