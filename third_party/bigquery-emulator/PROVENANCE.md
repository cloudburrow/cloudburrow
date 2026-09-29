# The BigQuery emulator CloudBurrow builds

CloudBurrow's BigQuery is **goccy/bigquery-emulator v0.8.1**, a community emulator (Google
publishes none), **built by CloudBurrow from its source with patches to it and to the SQL engine it links**
(#1061). This directory is everything that build reads besides the Go module proxy; no source
of the emulator or its dependencies is committed here, only the pins and the patches.

## Where each piece comes from

| Module | Version | Commit the tag names | Licence | Changed |
|---|---|---|---|---|
| [`github.com/goccy/bigquery-emulator`](https://github.com/goccy/bigquery-emulator) | v0.8.1 | `a531d3deb716eaba4972f9afa88e03e2c0f1a1af` | MIT | four patches |
| [`github.com/goccy/googlesqlite`](https://github.com/goccy/googlesqlite) | v0.3.1 | `36f6275991c003cde752014fa886eae33df6615d` | MIT (one file Apache-2.0, below) | thirteen patches |
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
   it, to `internal/bigqueryimage/bin/`, which the CLI embeds, each with a `.inputs` stamp: a hash
   of the pins and patches, the go command that built it, and its architecture. A later run keeps
   a binary whose stamp matches.

`make bigquery-binaries BQENGINE_FLAGS=-prebuilt` builds nothing: it checks that the binaries in
place were built from these sources, by any go command, and fails otherwise. CI builds the
binaries once per workflow run, cached by the same inputs, and its other jobs use them that way;
each release builds them from source once, for its four CLIs (#1087,
[docs/ci.md](../../docs/ci.md#the-embedded-bigquery-emulator)).

`up` then builds `dev.local/cloudburrow-bigquery:<content hash>` from the binary for the node's
architecture on the digest-pinned distroless base the storage image uses, and loads it into kind;
nothing is pulled but that base, and `cloudburrow prefetch` stores the built image for
`up --offline`. The image carries the licence bundle at `/licenses/bigquery-emulator.txt`, and
each release archive carries it as `LICENSES-bigquery-emulator.txt`.

## The patches

Each patch starts with a description of the bug, the CloudBurrow issue, and its licence, and
carries a regression test that fails without it and passes with it, run in the module's copy
under `build/` once `go run ./tools/bqengine -prepare-only` has written it, for example:

```sh
cd third_party/bigquery-emulator
GOFLAGS=-mod=readonly GOWORK=off go test -tags http2legacy github.com/goccy/bigquery-emulator/internal/metadata
```

| Patch | What it fixes | Whose |
|---|---|---|
| [bigquery-emulator 0001](patches/bigquery-emulator/0001-decode-base64-bytes.patch) | A BYTES value streamed, loaded or written from a query's rows was stored as the bytes of its base64 text, so bytes that are not UTF-8 could not be written at all (#1065, #1075) | CloudBurrow's, MIT |
| [bigquery-emulator 0002](patches/bigquery-emulator/0002-read-jobs-when-asked-expire-results.patch) | Every request read every job the emulator had run, with its whole result, and each job rewrote the project's list of jobs, so every request got slower with each job (0.27 s for a `SELECT 1` after 40 queries of 50,000 rows); and no result was ever dropped. A job is now read when a request names it, its result only by `jobs.getQueryResults`, and a result is kept up to 24 hours and while the results kept come to 256 MiB (#1086) | CloudBurrow's, MIT |
| [bigquery-emulator 0003](patches/bigquery-emulator/0003-empty-repeated-fields.patch) | A REPEATED column a streamed row or a load's record left out was stored NULL, and a null one was refused, where BigQuery writes an empty array (#1124) | CloudBurrow's, MIT |
| [bigquery-emulator 0004](patches/bigquery-emulator/0004-bigquery-wkt-text.patch) | A GEOGRAPHY streamed or loaded as WKT text read back as given (`POINT (3 4)`), where BigQuery writes `POINT(3 4)` (#1119) | CloudBurrow's, MIT |
| [bigquery-emulator 0005](patches/bigquery-emulator/0005-nested-geography-wkt.patch) | A GEOGRAPHY given as WKT text inside a RECORD or REPEATED column of a streamed row or a load was stored as given (`POINT (1 2)`); it is now written in BigQuery's form at any depth, as 0004 does at the top level (#1138); needs 0001 and 0004 | CloudBurrow's, MIT |
| [googlesqlite 0001](patches/googlesqlite/0001-cast-nested-struct-fields-upstream-pr-76.patch) | A RECORD inside a REPEATED RECORD, streamed or loaded, left the table unreadable ("failed to convert struct from array") (#900) | **Not CloudBurrow's**: goccy/googlesqlite pull request [#76](https://github.com/goccy/googlesqlite/pull/76), by Masaaki Goshima (@goccy), the project's author, at its head `63e6793e1f7994d80e40bc3b976fac1889aaebf0`, unchanged; open upstream, under the project's MIT licence |
| [googlesqlite 0002](patches/googlesqlite/0002-keep-table-function-handles-alive.patch) | A garbage collection freed a table function the catalog still used, and its next call trapped or crashed the emulator, losing every dataset (#1043, #1047) | CloudBurrow's, MIT |
| [googlesqlite 0003](patches/googlesqlite/0003-drop-table-function.patch) | `DROP TABLE FUNCTION` was refused ("Statement not supported: DropTableFunctionStatement"), so a table function could not be dropped or replaced (#976, #986) | CloudBurrow's, MIT |
| [googlesqlite 0004](patches/googlesqlite/0004-keep-nan.patch) | Every FLOAT64 NaN read back NULL: SQLite turns a NaN it is bound, or that a function returns, into NULL (#1066); and an infinity inside an ARRAY or a STRUCT failed "json: unsupported value" (#1077) | CloudBurrow's, MIT |
| [googlesqlite 0005](patches/googlesqlite/0005-unary-minus.patch) | `-a` of anything but a literal failed "no such function: googlesqlite_unary_minus" | CloudBurrow's, MIT |
| [googlesqlite 0006](patches/googlesqlite/0006-bare-path-sub-catalogs-upstream-pr-80.patch) | Every catalog rebuild (each DROP of a table, view or function) registered the whole builtin function set again into each project's and dataset's sub-catalog, in WebAssembly memory that only grows (#1057, #1017) | **Not CloudBurrow's**: goccy/googlesqlite pull request [#80](https://github.com/goccy/googlesqlite/pull/80), by @cp-ant, at its head `fc357c6e11c02f7f9ddc52a940e82bc50039bcb3`, unchanged but for its hunk's line numbers; open upstream, under the project's MIT licence |
| [googlesqlite 0007](patches/googlesqlite/0007-functions-of-every-dataset.patch) | With a default dataset, a call of another dataset's SQL function or table function failed "no such column: <project>": only the default dataset's were inlined (#1107, #1123) | CloudBurrow's, MIT |
| [googlesqlite 0008](patches/googlesqlite/0008-function-by-one-quoted-path.patch) | A function or table function named by one quoted path (`` `project.dataset.fn`(x) ``) was "Function not found" (#1122) | CloudBurrow's, MIT |
| [googlesqlite 0009](patches/googlesqlite/0009-interval-signs-and-sub-second-parts.patch) | A negative INTERVAL of less than an hour was written, stored and read back positive; `INTERVAL n MILLISECOND` and `MICROSECOND` were refused (#1126) | CloudBurrow's: MIT, and Apache-2.0 for its change to `internal/intervalvalue/intervalvalue.go`, which googlesqlite copied from cloud.google.com/go/bigquery (Copyright 2022 Google LLC) |
| [googlesqlite 0010](patches/googlesqlite/0010-interval-comparison.patch) | `=`, `<` and the other comparisons of two INTERVALs failed "unsupported eq operator for interval value" (#1120); needs 0009 | CloudBurrow's, MIT |
| [googlesqlite 0011](patches/googlesqlite/0011-null-arguments.patch) | Functions that panicked (the emulator answering 500) or failed on a NULL argument; IN and IN UNNEST FALSE where a comparison with NULL decides (#1109, #1121) | CloudBurrow's, MIT |
| [googlesqlite 0012](patches/googlesqlite/0012-bigquery-wkt.patch) | A GEOGRAPHY was written `POINT (1 2)` and an empty one `POINT EMPTY`, where BigQuery writes `POINT(1 2)` and `GEOMETRYCOLLECTION EMPTY` (#1119) | CloudBurrow's, MIT |
| [googlesqlite 0013](patches/googlesqlite/0013-typed-json-encoding.patch) | TO_JSON_STRING and TO_JSON did not follow GoogleSQL's JSON encodings: a BOOL read from a table was `1`, a DATE, DATETIME, TIME or TIMESTAMP was unquoted (not JSON), a fractional or wide NUMERIC, a wide INT64, an infinity or NaN, an INTERVAL and a RANGE were written otherwise than the table says (#1116) | CloudBurrow's, MIT |
| [googlesqlite 0014](patches/googlesqlite/0014-json-builders-typed-values.patch) | JSON_OBJECT, JSON_ARRAY, JSON_SET, JSON_ARRAY_APPEND and JSON_ARRAY_INSERT wrote a BOOL read from a table as `1` and a DATE, DATETIME, TIME or TIMESTAMP unquoted (not JSON), and JSON_SET of a DATE failed; their values now take TO_JSON's encoding (#1127); needs 0013 | CloudBurrow's, MIT |
| [googlesqlite 0015](patches/googlesqlite/0015-nan-sort-order.patch) | A FLOAT64 NaN sorted after +inf in ORDER BY and in an aggregate's ORDER BY, where BigQuery sorts NULL, NaN, -inf ... +inf (#1089); needs 0004 | CloudBurrow's, MIT |
| [go-googlesql 0001](patches/go-googlesql/0001-unsigned-wasm-addresses.patch) | Once the engine's WebAssembly heap passed 2 GiB, every call panicked "slice bounds out of range", addressing it with signed 32-bit offsets (#989) | CloudBurrow's, MIT |

Nothing here has been filed or proposed upstream; whether to is the maintainer's decision (#974).
When an upstream release carries a fix, drop its patch and bump the pin in sources.json, go.mod,
dependencies.json and `internal/bigqueryimage.Version` together; `go test ./tools/bqengine
./internal/bigqueryimage` fails while they disagree.
