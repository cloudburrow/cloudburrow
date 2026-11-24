# The Spanner emulator CloudBurrow patches

CloudBurrow's Spanner runs **Google's Cloud Spanner emulator**. Since #2029 it is **the same
release built from its source with patches**, shipped the way the patched BigQuery emulator is:
each release builds it once per architecture (`.github/workflows/release.yml`, job
`spanner-emulator`), every CLI embeds both builds (`internal/spannerimage`), and `up` builds the
image from the one for the cluster's nodes on a distroless base, pinned by digest, and loads it into kind. A CLI built without them (`go build`, or `make build`, which
does not run the hours-long C++ build) runs Google's prebuilt image, pinned by digest
(`internal/components.SpannerImage`, dependencies.json `spannerEmulator`), and `up` warns that
it returns a GQL `RETURN ... ORDER BY` out of order and refuses the graph features below. This
directory holds everything the build reads besides the two upstream sources. No upstream source
is committed here: only the pins and the patches.

## Where each piece comes from

| Source | Version | Commit or archive | Licence | Changed |
|---|---|---|---|---|
| [GoogleCloudPlatform/cloud-spanner-emulator](https://github.com/GoogleCloudPlatform/cloud-spanner-emulator) | v1.5.58 (the release Google's pinned image is) | `a081a7e71aba5f4d05fc8c6b21ef1a269dcbac28` | Apache-2.0 | three patches |
| [google/googlesql](https://github.com/google/googlesql), as the emulator builds it | 2026.7.2 | release archive, sha256 `f734fae677d672c61a278d562473bf8ae2d017f47d4dd2b37fa261e513685278` (the emulator's MODULE.bazel pins its integrity) | Apache-2.0 | one patch, applied after the emulator's own `build/bazel/googlesql.patch` |

Every other dependency is the emulator's own choice in its MODULE.bazel, fetched by Bazel as it
pins them. The build's `licenses.txt.gz` (written as the emulator's Dockerfile writes it) carries
their licences; the image has it where upstream's has it, and each release archive has it as
`LICENSES-spanner-emulator.txt`. Upstream's image copies the build image's GCC 13 libstdc++ over
distroless Debian 12's older one; CloudBurrow's uses distroless `cc-debian13`, whose libstdc++
(GCC 14) has every symbol the build needs, so no library of the compiler's is embedded or
shipped. (Linking libstdc++ statically, `BAZEL_LINKLIBS=-l%:libstdc++.a`, broke the link of
ICU's build tools through rules_foreign_cc: measured.)

## The patches

Each patch begins with a header naming the upstream files it changes and why. They apply in
[sources.json](sources.json)'s order.

| Patch | Upstream files | What and why |
|---|---|---|
| `patches/googlesql/0001-gql-order-and-graph-element-arguments.patch` | `googlesql/reference_impl/algebrizer_graph.cc`, `googlesql/analyzer/function_signature_matcher.cc` | A GQL `RETURN ... ORDER BY` came back with its first two rows swapped. The emulator evaluates with `scramble_undefined_orderings`, and GoogleSQL never marks a graph query's result ordered. The algebrizer now reads the order from the query's final scan. Plain SQL's ORDER BY was never affected (measured). The patch also lets GQL's functions take graph elements, so `COUNT(t)` of a group variable works, as Spanner documents. |
| `patches/cloud-spanner-emulator/0001-apply-cloudburrow-googlesql-patch.patch` | `MODULE.bazel` | Lists the GoogleSQL patch (copied to `build/bazel/cloudburrow_googlesql.patch`) after the emulator's own. |
| `patches/cloud-spanner-emulator/0002-graph-predicates-and-cheapest-path.patch` | `backend/query/feature_filter/gsql_supported_functions.cc`, `backend/query/analyzer_options.cc`, `backend/query/query_engine.cc` | Turns on Spanner Graph's documented `PROPERTY_EXISTS`, `ALL_DIFFERENT`, `SAME`, `IS SOURCE OF` and `IS DESTINATION OF`, which were missing from the emulator's supported functions. Also turns on `ANY CHEAPEST` and an edge's `COST` (`FEATURE_SQL_GRAPH_CHEAPEST_PATH`). GoogleSQL's evaluator already implements all of them. |
| `patches/cloud-spanner-emulator/0003-graph-regression-tests.patch` | `backend/query/query_engine_test.cc` | Regression tests for the above, on the emulator's own test graph. |

## How it is built and checked

`make spanner-binaries` (and the release job) runs
[scripts/spanner-emulator-binaries.sh](../../scripts/spanner-emulator-binaries.sh), which:

1. clones the emulator at the pinned tag and checks that the tag is the pinned commit;
2. applies the emulator patches in order with `git apply`, and writes the GoogleSQL patches,
   without their headers, to `build/bazel/cloudburrow_googlesql.patch`;
3. builds with the emulator's own `build/docker/Dockerfile.ubuntu` (Bazel 7.6.1, GCC 13) up to
   its Bazel step, followed by [Dockerfile.tail](Dockerfile.tail) (derived from the rest of it,
   Apache-2.0): one Bazel build of both binaries whose output base is a BuildKit cache mount,
   so an interrupted build keeps what it compiled, with `--jobs` from `BAZEL_JOBS` (default 4,
   since GoogleSQL's generated parser alone takes several gigabytes to compile);
4. exports the two binaries and the licence bundle (the tail's `binaries` stage) and packs them
   as `internal/spannerimage/bin/spanner-emulator-1.5.58-cb.<hash>-linux-<arch>.tar.gz`, which
   the next `go build` embeds. The hash is the first 12 hex digits of the SHA-256 of the
   patches, in order. `internal/spannerimage.PatchHash` holds the same value, and its test
   recomputes it from the patches, so a CLI never embeds a build of other patches.

`-check` stops after step 2. `SPANNER_ARCH=amd64` builds for another architecture (natively, on a
Docker of that architecture: under emulation it would not finish). `SPANNER_PATCH_TESTS=1` (the
script's `-test`) also runs the regression tests of patch 0003, `bazel test
--test_filter='*Gql*' //backend/query:query_engine_test`, in the same Bazel output base (the
tail's `patch-tests` stage).

At `up`, `internal/spannerimage` checks the embedded archive (exactly those three files, the
binaries ELF executables for the nodes' architecture), tags the image by the archive's and the
Dockerfile's content hash, and builds it unless Docker already has it.
