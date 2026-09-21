# Built locally and loaded into the cluster; never pushed to a registry.
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY main.go ./
RUN CGO_ENABLED=0 go build -trimpath -o /worker main.go

FROM gcr.io/distroless/static-debian12
COPY --from=build /worker /worker
ENTRYPOINT ["/worker"]
