# zetasketch (HyperLogLog++), fixed

Source: `github.com/bsm/zetasketch` v0.2.0 (commit `c7359d8c8be71f3e36bb5d9d5d45c0d1798cffdd`,
2026-06-16, the Go module proxy's `v0.2.0.zip`, go.sum `h1:Y/Y9dC3gEzfxm7iJma2yr+N8MnfNi5oSUv0Dyfl+GSA=`),
a Go port of Google's Java ZetaSketch (https://github.com/google/zetasketch, Copyright 2019 Google
LLC), Apache License 2.0 (`LICENSE`). Google publishes ZetaSketch for Java only.

CloudBurrow computes Bigtable's HyperLogLog++ aggregate column families with it (#1893,
`internal/bigtablefront/aggregates.go`): the cell's state is ZetaSketch's `AggregatorStateProto`,
which this port reads and writes. It is copied here (import path
`github.com/cloudburrow/cloudburrow/third_party/zetasketch`) because v0.2.0's sparse merge loses
values (see below). Copied: `zetasketch.go`, `hll.go`, `hllplus/`, `internal/hash/`,
`internal/zetasketch/` (the generated protos and their `.proto` sources) and the tests; left out:
`hllplus/testdata/proto.bin`, which no test reads, the `Makefile` and `README.md`. The import paths
`github.com/bsm/zetasketch/...` are rewritten to this directory's.

Changes (each marked "Modified by CloudBurrow" in the file, as the Apache License asks):

- `hllplus/sparse.go`: `Flush` merged the buffer into the stored list by a loop that dropped a
  stored value whenever every buffered value left was smaller than it (50 distinct values
  estimated as 10 once the sketch had been read between adds). It now merges the two sorted lists
  and keeps the largest value of each sparse index, as the Java `Encoding.Sparse.dedupe` does.
- `hllplus/hllplus_test.go`: the expected estimates that changed with that fix (each within 0.02%
  of the true count; upstream's numbers were measured on its own code, not Java's).

To update: copy the new version's sources the same way, reapply the changes above, record the
version, sum and commit in `dependencies.json`, and run `go test ./third_party/zetasketch/...
./internal/bigtablefront/`.
