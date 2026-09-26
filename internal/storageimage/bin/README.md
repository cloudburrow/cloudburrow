`make storage-binaries` (run by `make build` and the release) writes
`cloudburrow-storage-linux-amd64` and `cloudburrow-storage-linux-arm64` here,
from `cmd/cloudburrow-storage`, and the CLI embeds them (#514). They are
build outputs and are not committed.
