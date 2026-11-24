`make logging-agent-binaries` (run by `make build` and the release) writes
`cloudburrow-logging-agent-linux-amd64` and `cloudburrow-logging-agent-linux-arm64`
here, from `cmd/cloudburrow-logging-agent`, and the CLI embeds them (#2100).
They are build outputs and are not committed.
