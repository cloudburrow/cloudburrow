# internal/s2ops provenance

Ported from Google's S2 Geometry Library, <https://github.com/google/s2geometry>,
commit `677c8cebcff11ba11accc0b02005bc214e06e9fd` (2026-09-25), under the
Apache License, Version 2.0 (the same licence as CloudBurrow; each file keeps
Google's copyright line beside CloudBurrow's). Read on 2026-11-17, file by
file from raw.githubusercontent.com, for #1728 and #1735, because
github.com/golang/geo (the approved module, pinned and at its latest commit)
ports neither S2BooleanOperation, S2BufferOperation, S2WindingOperation nor
S2Builder: its `s2/builder.go` holds only option constants and
`builder_snapper.go` the snap functions, which these ports use (except
`IntLatLngSnapper`, whose `SnapPoint` rounds radians instead of degrees; the
tests carry their own).

Changes common to every file: S2MemoryTracker is dropped; S2Error is a Go
error (`*BuilderError` with upstream's text where upstream names one); Go has
no `long double`, so the predicates' long-double stage is skipped and the exact
stage, which decides every case it would have with the same answer, runs
instead; and because upstream compiles S2 without floating-point contraction
(`s2/_fp_contract_off.h`) while the Go compiler may fuse a multiply and an add
on arm64, code that computes output coordinates rounds every product
explicitly (`dotNF`, `crossNF`, `normalizeNF`).

| File | Upstream source | What was taken | Changes |
| --- | --- | --- | --- |
| `boolean_operation.go` | `src/s2/s2boolean_operation.{h,cc}` | The whole of S2BooleanOperation: OpType, PolygonModel, PolylineModel, the options (`polyline_loops_have_boundaries`, `split_all_crossing_polyline_edges`), CrossingIterator, CrossingProcessor, EdgeClippingLayer, the full/empty polygon decision, AreRegionsIdentical, IsEmpty and its fast paths, Build. | `Precision::SNAPPED`, `conservative_output` and `source_id_lexicon` are not implemented, as upstream does not implement them either. |
| `boolean_operation_test.go` | `src/s2/s2boolean_operation_test.cc` | The point, polyline and polygon cases of every model (touching and crossing included), the overlapping bars and doughnuts, polylines entering and crossing rectangles, self-intersecting polylines, full and empty results, GetCrossedVertexIndex bugs 1–5, the degeneracy-coverage tables. | Not ported: the meridian-splitting test (it needs indexed layers and the text printer), the SourceId accessors, GetCrossedVertexIndex bug 6. |
| `builder.go` | `src/s2/s2builder.{h,cc}`, `src/s2/s2polyline_simplifier.cc` | The whole of S2Builder: options, labels, the input methods, Build, site selection (`ChooseSites`, `ChooseInitialSites`, `SortInputVertices`, `AddForcedSites`), `AddEdgeCrossings`, `CollectSiteEdges`, `AddExtraSites`, `SnapEdge`, idempotency, `BuildLayers`, and `simplify_edge_chains` (EdgeChainSimplifier, S2PolylineSimplifier). | S2PointIndex and S2ClosestPointQuery are a cell grid and S2ClosestEdgeQuery golang/geo's EdgeQuery or a scan, each returning the same candidates upstream's conservative queries do (every candidate is then tested with the exact predicates); AddExtraSites visits edges in increasing id order (upstream iterates a hash set). |
| `builder_test.go` | `src/s2/s2builder_test.cc` | 27 cases: snapping, idempotency, simplification, labels and the regression bugs. | |
| `builder_graph.go` | `src/s2/s2builder_graph.{h,cc}` | The whole of S2Builder::Graph: ProcessEdges (EdgeProcessor), FilterVertices, VertexOutMap, VertexInMap, LabelFetcher, sibling and left-turn maps, IsFullPolygon, CanonicalizeLoopOrder, CanonicalizeVectorOrder, GetDirectedLoops, GetDirectedComponents, GetUndirectedComponents, GetPolylines (PolylineBuilder), GetInputEdgeOrder, MakeSubgraph. | GetDirectedComponents skips an empty path rather than reading its first edge. |
| `builder_graph_test.go` | `src/s2/s2builder_graph_test.cc` | The tests that build a Graph directly: LabelsRequestedButNotProvided, the ProcessEdges cases, MakeSubgraph. | |
| `builder_layers.go` | `src/s2/s2builderutil_s2polygon_layer.cc`, `s2builderutil_s2polyline_vector_layer.cc`, `s2builderutil_s2point_vector_layer.cc`, `s2builderutil_closed_set_normalizer.cc`, `s2builderutil_find_polygon_degeneracies.cc`, `s2builderutil_graph_shape.h` | S2PolygonLayer (directed loops through `s2.PolygonFromOrientedLoops`, as InitOriented), S2PolylineVectorLayer, S2PointVectorLayer, ClosedSetNormalizer and NormalizeClosedSet, FindPolygonDegeneracies, IsFullyDegenerate, GraphShape. | Layers write golang/geo types; FindPolygonDegeneracies always computes its signs by brute force (upstream switches to an index above 25 unknown signs, with the same result); GraphShape embeds an `s2.LaxPolyline` because `s2.Shape` has unexported methods. |
| `builder_layers_test.go` | `src/s2/s2builderutil_*_layer_test.cc`, `s2builderutil_closed_set_normalizer_test.cc` | Loop, polyline, component and normalizer cases on hand-built graphs. | |
| `winding_operation.go` | `src/s2/s2winding_operation.{h,cc}`, `src/s2/s2builderutil_get_snapped_winding_delta.cc` | S2WindingOperation (WindingOracle, WindingLayer), GetSnappedWindingDelta, FindFirstVertexId. | WindingOracle counts crossings with every graph edge instead of building an index after the first query (the same sums). |
| `buffer.go` | `src/s2/s2buffer_operation.cc` | `Options::set_circle_segments`, `Init`'s step angles, `GetMaxEdgeSpan`, `SetInputVertex`, `AddOffsetVertex`, `CloseBufferRegion`, `UpdateRefWinding`, `AddPoint`, `GetEdgeAxis`, `AddVertexArc`, `AddEdgeArc`, `BufferEdgeAndVertex`, the end caps, `BufferLoop`, `AddPolyline`, `Build` (the paths go to S2WindingOperation with the positive winding rule and an S2PolygonLayer), and `S2::GetPointOnRay`. | Translated onto golang/geo's types. |
| `predicates.go`, `predicates_test.go` | `src/s2/s2predicates.cc`, `src/s2/s2edge_distances.cc`, `src/s2/s2edge_crossings.cc`, `s2predicates_test.cc` | CompareEdgeDistance, CompareEdgeDirections, EdgeCircumcenterSign, GetVoronoiSiteExclusion (triage and exact stages), GetUpdateMinDistanceMaxError, IsEdgeBNearEdgeA, Project, GetIntersection (and its exact stage), kIntersectionError, kIntersectionMergeRadius; the Coverage tests. | golang/geo's `s2.Intersection` breaks ties differently from upstream, so the port's GetIntersection is used. |
| `crossings.go` | `src/s2/s2edge_crossings.cc`, `src/s2/s2edge_crosser.h` | SignedVertexCrossing, RobustCrossProd (GetStableCrossProd, ExactCrossProd, SymbolicCrossProdSorted, EnsureNormalizable, NormalizableFromExact), SignedEdgeOrVertexCrossing. | |
| `shapeutil.go` | `src/s2/s2shapeutil_visit_crossing_edge_pairs.cc`, `src/s2/s2contains_point_query.h`, `src/s2/s2shape_measures.cc`, `src/s2/s2shape_index_measures.cc`, `src/s2/s2measures.cc` | VisitCrossingEdgePairs, VisitIncidentEdges, S2::GetArea of an index, S2::TurnAngle. | Index-cell walks are golang/geo's CrossingEdgeQuery or brute force, which visit the same edge pairs; `chainEdge` and `chainPosition` work around golang/geo's `PointVector.ChainEdge` and `LaxPolygon.ChainPosition`, which return wrong values for some arguments. |
| `builder_types.go` | `src/s2/s2builder.h`, `src/s2/s2builder_graph.h`, `src/s2/id_set_lexicon.h` | The id types, GraphOptions and its enums, Layer, IsFullPolygonPredicate, IdSetLexicon. | IdSetLexicon keys its sets by value (golang/geo's unexported one keys them by a hash alone). |
