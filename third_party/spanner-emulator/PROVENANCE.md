# The Spanner emulator CloudBurrow patches

CloudBurrow's Spanner runs **Google's Cloud Spanner emulator**. By default that is Google's
image, pinned by digest (`internal/components.SpannerImage`, dependencies.json
`spannerEmulator`). Since #2029 CloudBurrow can also **build the same release from its source
with patches**. When Docker has that image for the cluster's architecture, `up` loads it into the
cluster and the Spanner pod runs it (`internal/spannerimage`). This directory holds everything
the build reads besides the two upstream sources. No upstream source is committed here: only the
pins and the patches.

## Where each piece comes from

| Source | Version | Commit or archive | Licence | Changed |
|---|---|---|---|---|
| [GoogleCloudPlatform/cloud-spanner-emulator](https://github.com/GoogleCloudPlatform/cloud-spanner-emulator) | v1.5.58 (the release Google's pinned image is) | `a081a7e71aba5f4d05fc8c6b21ef1a269dcbac28` | Apache-2.0 | three patches |
| [google/googlesql](https://github.com/google/googlesql), as the emulator builds it | 2026.7.2 | release archive, sha256 `f734fae677d672c61a278d562473bf8ae2d017f47d4dd2b37fa261e513685278` (the emulator's MODULE.bazel pins its integrity) | Apache-2.0 | one patch, applied after the emulator's own `build/bazel/googlesql.patch` |

Every other dependency is the emulator's own choice in its MODULE.bazel, fetched by Bazel as it
pins them. The image's `licenses.txt.gz` (written by the emulator's Dockerfile) carries their
licences.

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

`make spanner-emulator-image` runs [scripts/spanner-emulator-image.sh](../../scripts/spanner-emulator-image.sh),
which:

1. clones the emulator at the pinned tag and checks that the tag is the pinned commit;
2. applies the emulator patches in order with `git apply`, and writes the GoogleSQL patches,
   without their headers, to `build/bazel/cloudburrow_googlesql.patch`;
3. builds with the emulator's own `build/docker/Dockerfile.ubuntu` (Bazel 7.6.1, GCC 13), and
   tags the image `dev.local/cloudburrow-spanner-emulator:1.5.58-cb.<hash>`. The hash is the
   first 12 hex digits of the SHA-256 of the patches, in order. `internal/spannerimage.PatchHash`
   holds the same value, and its test recomputes it from the patches.

`-check` stops after step 2. `SPANNER_PLATFORM=linux/amd64` builds for another architecture.
