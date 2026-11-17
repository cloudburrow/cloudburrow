# internal/s2ops provenance

Ported from Google's S2 Geometry Library, <https://github.com/google/s2geometry>,
commit `677c8cebcff11ba11accc0b02005bc214e06e9fd` (2026-09-25), under the
Apache License, Version 2.0 (the same licence as CloudBurrow; the files keep
Google's copyright line beside CloudBurrow's). Read on 2026-11-17 for #1728,
because github.com/golang/geo (the approved module, pinned and at its latest
commit `5e8d696`) ports neither S2BooleanOperation, S2BufferOperation nor
S2Builder: its `s2/builder.go` holds only option constants and
`builder_snapper.go` the snap functions.

| File | Upstream source | What was taken | Changes |
| --- | --- | --- | --- |
| `buffer.go` | `src/s2/s2buffer_operation.cc` (Eric Veach) | `Options::set_circle_segments`, `Init`'s step angles, `GetMaxEdgeSpan`, `SetInputVertex`, `AddOffsetVertex`, `CloseBufferRegion`, `UpdateRefWinding`, `AddPoint`, `AddVertexArc`, `AddEdgeArc`, `BufferEdgeAndVertex`, the end caps, `BufferLoop`, `AddPolyline` | Translated to Go on golang/geo's types; memory tracking dropped; the paths go to `Overlay` (positive winding rule) instead of S2WindingOperation and S2Builder. |
| `order.go` | `src/s2/s2builder_graph.cc` | `CanonicalizeLoopOrder`, `CanonicalizeVectorOrder`, `PolylineBuilder::BuildPaths` (undirected edges), `SortInputVertices`' order | Translated to Go over a small edge graph of its own. |
| `overlay.go` | none | | CloudBurrow's own: splits edges at their crossings with golang/geo's exact predicates, merges coincident edges, measures the winding number on each side, keeps the boundary of {w >= k}. Not a port of S2BooleanOperation. |
| `simplify.go` | none | | CloudBurrow's own: site selection as `S2Builder::ChooseInitialSites` does for an `IdentitySnapFunction`, then greedy chain simplification. Not a port of S2Builder's `EdgeChainSimplifier`. |
