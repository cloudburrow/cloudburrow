# Built locally and loaded into the cluster; never pushed to a registry.
# Base images are pinned by digest, the same ones testdata/predictor uses
# (dependencies.json build.predictorFixtureGo and predictorFixtureBase).
FROM golang:1.27-alpine@sha256:8a5910f31396cd4d89662f56c68b3ae31d374308270a1c3bd96672ee5ed43414 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY main.go ./
RUN CGO_ENABLED=0 go build -trimpath -o /worker main.go

FROM gcr.io/distroless/static-debian12@sha256:d75cdd72874d4790092fcb1b058493ecf6bb5bf2b2b897045b00ff01d91843f2
COPY --from=build /worker /worker
ENTRYPOINT ["/worker"]
