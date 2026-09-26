# Cloud Storage differential oracle (storage-testbench)

`scripts/oracle-storage.sh` sends the same HTTP exchanges to CloudBurrow's
builtin Cloud Storage server and to Google's
[storage-testbench](https://github.com/googleapis/storage-testbench/tree/f1fbebcec2e7003bca4459639c424abf8bc0945c)
v0.64.0, and compares, for every exchange, the status, selected response
headers and the fields of a success body. First, each server's own values are
normalised: times, generations, etags, IDs, the host, and the testbench's
`x_emulator_*` metadata (#497).

Each test covers:

- `TestOracleResumableHandshakes`: session start, the status query before and
  after a chunk (308, and 200 with `X-Http-Status-Code-Override` under
  `X-GUploader-No-308`), the final chunk, a query after completion, and a
  one-shot session.
- `TestOracleSuccessShapes`: multipart and media insert, get, patch, compose,
  and whole, ranged, open, suffix and unsatisfiable downloads (JSON and XML).
- `TestOracleRewriteTokens`: a rewrite split by `maxBytesRewrittenPerCall`,
  call by call.
- `TestOracleAllowlistIsExplicit` and `TestOracleNormalizes` check the
  comparison itself and need no server.

**Agreement with the testbench is not an observation of Google, and never
removes an `// unverified:` annotation.** The testbench's own README calls it
"not an officially supported Google product". Where the two servers differ,
Google's docs decide. Every known difference is explained in
`allowlist_test.go`, by an entry for one step (`get body.contentType`) or for
any step (`* body.selfLink`) that covers a field and everything under it,
with a reason citing the docs. An unlisted difference fails, and so does an
entry that explained nothing in a full run. The behaviours deliberately left out are in `notCompared`:

- error bodies;
- versioning;
- holds and retention;
- non-empty delete;
- paging.

## How it runs

The workflow `.github/workflows/storage-oracle.yml` runs it on pull requests
that touch the storage server or the oracle, nightly, and on demand. It is
not a job in CI, and `make check` does not build the `oracle` tag.

The script does four things:

1. Pulls the image by the digest recorded in `dependencies.json`
   (`components.testOracles.storageTestbench`).
2. Starts the testbench with `--network none`.
3. Runs the static test binary in a second container that shares only the
   testbench's loopback. The builtin server runs inside the test binary.
4. Removes the container it started.

## Adding a case

Add a step to `steps_test.go`. If the servers differ, check the docs:

- If the builtin server is wrong, fix it.
- If the testbench departs from the docs, or the docs are silent, add an
  allowlist entry that says which.

Review testbench releases quarterly. A new digest goes in `dependencies.json`.
