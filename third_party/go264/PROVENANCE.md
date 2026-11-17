# go.264, without its Linux hardware backends

Source: `github.com/oops1/go.264` v1.14.0 (commit `cc9c9d0309d6e8a0e80bb182c14d2e3e13276aa2`,
the Go module proxy's `v1.14.0.zip`), by Valery Chukalin, BSD 2-Clause (`LICENSE`).

Fetched through the go command (go.sum `h1:CLElIjsfp3sI6s1YWVl08vjacB4h3G56EeYhPaNk/Wc=`).
CloudBurrow decodes H.264 video with it for Vertex AI EmbedContent (#1722). This directory is part
of CloudBurrow's module (import path `github.com/cloudburrow/cloudburrow/third_party/go264`; a
`replace` directive would make `go install ...@latest` refuse the module), and holds the module's
non-test sources unchanged except that the import paths `github.com/oops1/go.264/...` are rewritten
to that path and these are left out:

- `hwaccel_linux.go`, `internal/hwaccel/nvenc` and `internal/hwaccel/vaapi`: the Linux NVENC and
  VA-API encoder backends. They import `github.com/ebitengine/purego`, which makes a
  `CGO_ENABLED=0` Linux binary dynamically linked against libc. Upstream leaves them out with
  `-tags go264_nohwaccel`; leaving them out here keeps every build static without a tag
  (`test/repo`'s `TestLinuxBinariesNeverImportPurego`). CloudBurrow decodes on the CPU only
  (`ForceSoftware`), so nothing it runs changes.
- `cmd/`, `internal/testutil/`, the tests and their test data.

To update: copy the new version's sources the same way, leave out the same files, rewrite the
imports, record the version, sum and commit in `dependencies.json`, and run
`go test ./internal/embedding/ ./test/repo/`.
