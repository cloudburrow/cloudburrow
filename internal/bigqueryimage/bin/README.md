`make bigquery-binaries` (run by `make build` and the release) writes
`bigquery-emulator-linux-amd64.gz`, `bigquery-emulator-linux-arm64.gz` and
`bigquery-emulator-licenses.txt.gz` here with `go run ./tools/bqengine`, from
the pinned sources and patches in `third_party/bigquery-emulator`, and the
CLI embeds them (#1061). They are build outputs and are not committed.
