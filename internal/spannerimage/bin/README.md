`make spanner-binaries` (scripts/spanner-emulator-binaries.sh) and the
release's `spanner-emulator` job write
`spanner-emulator-<version>-linux-amd64.tar.gz` and
`spanner-emulator-<version>-linux-arm64.tar.gz` here: Google's Cloud Spanner
emulator built from source with the patches in `third_party/spanner-emulator`
(#2029), and the CLI embeds them. `<version>` is `internal/spannerimage.Version`,
so a CLI ignores an archive built from other patches. They are build outputs
and are not committed. Without one for the cluster's architecture, `up` runs
Google's prebuilt image and warns what it gets wrong.
