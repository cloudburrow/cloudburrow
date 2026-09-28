# The BigQuery emulator CloudBurrow builds

CloudBurrow's BigQuery is **goccy/bigquery-emulator v0.8.1**, a community emulator (Google
publishes none), **built by CloudBurrow from its source with patches to it and to the SQL engine it links**
(#1061). This directory is everything that build reads besides the Go module proxy; no source
of the emulator or its dependencies is committed here, only the pins and the patches.

## Where each piece comes from

| Module | Version | Commit the tag names | Licence | Changed |
|---|---|---|---|---|
| [`github.com/goccy/bigquery-emulator`](https://github.com/goccy/bigquery-emulator) | v0.8.1 | `a531d3deb716eaba4972f9afa88e03e2c0f1a1af` | MIT | one patch |
| [`github.com/goccy/googlesqlite`](https://github.com/goccy/googlesqlite) | v0.3.1 | `36f6275991c003cde752014fa886eae33df6615d` | MIT | five patches |
| [`github.com/goccy/go-googlesql`](https://github.com/goccy/go-googlesql) | v0.3.0 | `eb229fca73e7dca3fc9e8e8be733d14a565f912c` | MIT | one patch |
| every other module the emulator links | v0.8.1's `go.sum` | | each its own | no |

The three MIT licence texts are in [licenses/](licenses/). Each version is v0.8.1's own choice:
[go.mod](go.mod) requires the emulator and nothing else, and its build list for the command
linked is, module by module, the same as v0.8.1's own (checked with `go list -deps`), except that
the three modules above are replaced by the patched copies.

## How it is built and checked

`make bigquery-binaries` (part of `make build` and of each release) runs
[tools/bqengine](../../tools/bqengine), which:

1. downloads each patched module at its version through the go command, which checks it against
   the checksum database, and checks the download's `h1:` hash, and the hash of its files in the
   module cache, against [sources.json](sources.json);
2. copies it to `build/` (not committed) and applies its patches in order, strictly: a hunk that
   does not match its lines exactly fails the build, with no fuzz and no search;
3. checks that [go.mod](go.mod)'s replacements are in force, so a version bump that forgot the
   patches cannot build unpatched;
4. builds `github.com/goccy/bigquery-emulator/cmd/bigquery-emulator` for linux/amd64 and
   linux/arm64 with `CGO_ENABLED=0 -trimpath -mod=readonly`, every other module fetched and
   checked against [go.sum](go.sum), and the build tag `http2legacy` (golang.org/x/net v0.54.0,
   which v0.8.1 pins, leaves its HTTP/2 server out of a Go 1.27 build without it);
5. writes each binary gzip-compressed, and the licence and notice files of every module linked into
   it, to `internal/bigqueryimage/bin/`, which the CLI embeds.

`up` then builds `dev.local/cloudburrow-bigquery:<content hash>` from the binary for the node's
architecture on the digest-pinned distroless base the storage image uses, and loads it into kind;
nothing is pulled but that base, and `cloudburrow prefetch` stores the built image for
`up --offline`. The image carries the licence bundle at `/licenses/bigquery-emulator.txt`, and
each release archive carries it as `LICENSES-bigquery-emulator.txt`.

## The patches

Each patch starts with a description of the bug, the CloudBurrow issue, and its licence, and
carries a regression test that fails without it and passes with it (run in the module's copy
under `build/`).

| Patch | What it fixes | Whose |
|---|---|---|
| [bigquery-emulator 0001](patches/bigquery-emulator/0001-decode-base64-bytes.patch) | A BYTES value streamed, loaded or written from a query's rows was stored as the bytes of its base64 text, so bytes that are not UTF-8 could not be written at all (#1065, #1075) | CloudBurrow's, MIT |
| [googlesqlite 0001](patches/googlesqlite/0001-cast-nested-struct-fields-upstream-pr-76.patch) | A RECORD inside a REPEATED RECORD, streamed or loaded, left the table unreadable ("failed to convert struct from array") (#900) | **Not CloudBurrow's**: goccy/googlesqlite pull request [#76](https://github.com/goccy/googlesqlite/pull/76), by Masaaki Goshima (@goccy), the project's author, at its head `63e6793e1f7994d80e40bc3b976fac1889aaebf0`, unchanged; open upstream, under the project's MIT licence |
| [googlesqlite 0002](patches/googlesqlite/0002-keep-table-function-handles-alive.patch) | A garbage collection freed a table function the catalog still used, and its next call trapped or crashed the emulator, losing every dataset (#1043, #1047) | CloudBurrow's, MIT |
| [googlesqlite 0003](patches/googlesqlite/0003-drop-table-function.patch) | `DROP TABLE FUNCTION` was refused ("Statement not supported: DropTableFunctionStatement"), so a table function could not be dropped or replaced (#976, #986) | CloudBurrow's, MIT |
| [googlesqlite 0004](patches/googlesqlite/0004-keep-nan.patch) | Every FLOAT64 NaN read back NULL: SQLite turns a NaN it is bound, or that a function returns, into NULL (#1066); and an infinity inside an ARRAY or a STRUCT failed "json: unsupported value" (#1077) | CloudBurrow's, MIT |
| [googlesqlite 0005](patches/googlesqlite/0005-unary-minus.patch) | `-a` of anything but a literal failed "no such function: googlesqlite_unary_minus" | CloudBurrow's, MIT |
| [go-googlesql 0001](patches/go-googlesql/0001-unsigned-wasm-addresses.patch) | Once the engine's WebAssembly heap passed 2 GiB, every call panicked "slice bounds out of range", addressing it with signed 32-bit offsets (#989) | CloudBurrow's, MIT |

Nothing here has been filed or proposed upstream; whether to is the maintainer's decision (#974).
When an upstream release carries a fix, drop its patch and bump the pin in sources.json, go.mod,
dependencies.json and `internal/bigqueryimage.Version` together; `go test ./tools/bqengine
./internal/bigqueryimage` fails while they disagree.
