// Copyright 2016 Google Inc. All Rights Reserved.
// Copyright 2026 The CloudBurrow Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS-IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package s2ops

// PORT STUBS (#1735): the exported API of the S2Builder, Graph, layer and
// S2BooleanOperation ports, so that the files porting them can be written
// in parallel. Each stub moves into its own file (builder.go,
// builder_graph.go, builder_layers.go, boolean_operation.go) with its body
// ported, keeping the signature; this file is deleted when it is empty.

import (
	"github.com/golang/geo/s2"
)

// ---- builder_graph.go (src/s2/s2builder_graph.{h,cc}) ----

// LoopType is S2Builder::Graph::LoopType.
type LoopType int

const (
	LoopTypeSimple LoopType = iota
	LoopTypeCircuit
)

// PolylineType is S2Builder::Graph::PolylineType.
type PolylineType int

const (
	PolylineTypePath PolylineType = iota
	PolylineTypeWalk
)

// DegenerateBoundaries is S2Builder::Graph::DegenerateBoundaries.
type DegenerateBoundaries int

const (
	DegenerateBoundariesDiscard DegenerateBoundaries = iota
	DegenerateBoundariesKeep
)

// NewGraph is Graph's constructor.
func NewGraph(options GraphOptions, vertices []s2.Point, edges []GraphEdge, inputEdgeIDSetIDs []InputEdgeIDSetID, inputEdgeIDSetLexicon *IDSetLexicon, labelSetIDs []LabelSetID, labelSetLexicon *IDSetLexicon, isFullPolygonPredicate IsFullPolygonPredicate) *Graph {
	panic("port stub")
}

// ProcessEdges is Graph::ProcessEdges.
func ProcessEdges(options *GraphOptions, edges *[]GraphEdge, inputIDs *[]InputEdgeIDSetID, idSetLexicon *IDSetLexicon) error {
	panic("port stub")
}

// FilterVertices is Graph::FilterVertices.
func FilterVertices(vertices []s2.Point, edges *[]GraphEdge, tmp *[]VertexID) []s2.Point {
	panic("port stub")
}

func (g *Graph) Options() GraphOptions                           { panic("port stub") }
func (g *Graph) NumVertices() int                                { panic("port stub") }
func (g *Graph) Vertex(v VertexID) s2.Point                      { panic("port stub") }
func (g *Graph) Vertices() []s2.Point                            { panic("port stub") }
func (g *Graph) NumEdges() int                                   { panic("port stub") }
func (g *Graph) Edge(e EdgeID) GraphEdge                         { panic("port stub") }
func (g *Graph) Edges() []GraphEdge                              { panic("port stub") }
func (g *Graph) InputEdgeIDs(e EdgeID) []int32                   { panic("port stub") }
func (g *Graph) InputEdgeIDSetID(e EdgeID) InputEdgeIDSetID      { panic("port stub") }
func (g *Graph) InputEdgeIDSetIDs() []InputEdgeIDSetID           { panic("port stub") }
func (g *Graph) InputEdgeIDSetLexicon() *IDSetLexicon            { panic("port stub") }
func (g *Graph) MinInputEdgeID(e EdgeID) InputEdgeID             { panic("port stub") }
func (g *Graph) GetMinInputEdgeIDs() []InputEdgeID               { panic("port stub") }
func (g *Graph) GetInputEdgeOrder(minIDs []InputEdgeID) []EdgeID { panic("port stub") }
func (g *Graph) LabelSetIDs() []LabelSetID                       { panic("port stub") }
func (g *Graph) LabelSetLexicon() *IDSetLexicon                  { panic("port stub") }
func (g *Graph) Labels(e InputEdgeID) []Label                    { panic("port stub") }
func (g *Graph) IsFullPolygonPredicate() IsFullPolygonPredicate  { panic("port stub") }
func (g *Graph) IsFullPolygon() (bool, error)                    { panic("port stub") }
func (g *Graph) GetInEdgeIDs() []EdgeID                          { panic("port stub") }
func (g *Graph) GetSiblingMap() []EdgeID                         { panic("port stub") }
func (g *Graph) MakeSiblingMap(inEdgeIDs []EdgeID)               { panic("port stub") }
func (g *Graph) GetLeftTurnMap(inEdgeIDs []EdgeID) ([]EdgeID, error) {
	panic("port stub")
}
func (g *Graph) GetDirectedLoops(loopType LoopType) ([][]EdgeID, error) { panic("port stub") }
func (g *Graph) GetDirectedComponents(b DegenerateBoundaries) ([][][]EdgeID, error) {
	panic("port stub")
}
func (g *Graph) GetUndirectedComponents(loopType LoopType) ([][2][][]EdgeID, error) {
	panic("port stub")
}
func (g *Graph) GetPolylines(polylineType PolylineType) [][]EdgeID { panic("port stub") }

// VertexOutMap is Graph::VertexOutMap.
type VertexOutMap struct {
	edges     []GraphEdge
	edgeBegin []EdgeID
}

func NewVertexOutMap(g *Graph) *VertexOutMap                   { panic("port stub") }
func (m *VertexOutMap) Degree(v VertexID) int                  { panic("port stub") }
func (m *VertexOutMap) EdgeIDs(v VertexID) (begin, end EdgeID) { panic("port stub") }
func (m *VertexOutMap) EdgeIDsTo(v0, v1 VertexID) (begin, end EdgeID) {
	panic("port stub")
}

// VertexInMap is Graph::VertexInMap.
type VertexInMap struct {
	inEdgeIDs   []EdgeID
	inEdgeBegin []EdgeID
}

func NewVertexInMap(g *Graph) *VertexInMap         { panic("port stub") }
func (m *VertexInMap) Degree(v VertexID) int       { panic("port stub") }
func (m *VertexInMap) EdgeIDs(v VertexID) []EdgeID { panic("port stub") }
func (m *VertexInMap) InEdgeIDs() []EdgeID         { panic("port stub") }

// ---- builder.go (src/s2/s2builder.{h,cc}) ----

// BuilderOptions is S2Builder::Options.
type BuilderOptions struct {
	SnapFunction          s2.Snapper
	SplitCrossingEdges    bool
	IntersectionTolerance float64 // radians (S1Angle)
	SimplifyEdgeChains    bool
	Idempotent            bool
}

// DefaultBuilderOptions is S2Builder::Options(): IdentitySnapFunction(0),
// idempotent.
func DefaultBuilderOptions() BuilderOptions {
	return BuilderOptions{SnapFunction: s2.NewIdentitySnapper(0), Idempotent: true}
}

// Builder is S2Builder.
type Builder struct{ todo int }

func NewBuilder(o BuilderOptions) *Builder                            { panic("port stub") }
func (b *Builder) Options() BuilderOptions                            { panic("port stub") }
func (b *Builder) StartLayer(l Layer)                                 { panic("port stub") }
func (b *Builder) AddEdge(v0, v1 s2.Point)                            { panic("port stub") }
func (b *Builder) AddPoint(v s2.Point)                                { panic("port stub") }
func (b *Builder) AddPolyline(vs []s2.Point)                          { panic("port stub") }
func (b *Builder) AddLoop(vs []s2.Point)                              { panic("port stub") }
func (b *Builder) AddShape(shape s2.Shape)                            { panic("port stub") }
func (b *Builder) AddIsFullPolygonPredicate(p IsFullPolygonPredicate) { panic("port stub") }
func (b *Builder) ForceVertex(v s2.Point)                             { panic("port stub") }
func (b *Builder) PushLabel(l Label)                                  { panic("port stub") }
func (b *Builder) PopLabel()                                          { panic("port stub") }
func (b *Builder) SetLabel(l Label)                                   { panic("port stub") }
func (b *Builder) ClearLabels()                                       { panic("port stub") }
func (b *Builder) Build() error                                       { panic("port stub") }
func (b *Builder) Reset()                                             { panic("port stub") }
func (b *Builder) NumInputEdges() int                                 { panic("port stub") }
func (b *Builder) InputEdge(e InputEdgeID) (s2.Point, s2.Point)       { panic("port stub") }

// ---- builder_layers.go (s2builderutil_s2polygon_layer, _s2polyline_vector_layer,
// _s2point_vector_layer, _closed_set_normalizer) ----

// PolygonLayerOptions is s2builderutil::S2PolygonLayer::Options.
type PolygonLayerOptions struct {
	EdgeType EdgeType // default directed
	// ValidateOutput etc. as needed.
}

// NewPolygonLayer is S2PolygonLayer writing *out.
func NewPolygonLayer(out **s2.Polygon, o PolygonLayerOptions) Layer { panic("port stub") }

// PolylineVectorLayerOptions is S2PolylineVectorLayer::Options.
type PolylineVectorLayerOptions struct {
	EdgeType       EdgeType
	PolylineType   PolylineType
	DuplicateEdges DuplicateEdges
	SiblingPairs   SiblingPairs
}

// DefaultPolylineVectorLayerOptions is S2PolylineVectorLayer::Options().
func DefaultPolylineVectorLayerOptions() PolylineVectorLayerOptions {
	return PolylineVectorLayerOptions{EdgeTypeDirected, PolylineTypePath, DuplicateEdgesKeep, SiblingPairsKeep}
}

func NewPolylineVectorLayer(out *[]*s2.Polyline, o PolylineVectorLayerOptions) Layer {
	panic("port stub")
}

// PointVectorLayerOptions is S2PointVectorLayer::Options.
type PointVectorLayerOptions struct {
	DuplicateEdges DuplicateEdges
}

func NewPointVectorLayer(out *[]s2.Point, o PointVectorLayerOptions) Layer { panic("port stub") }

// NormalizeClosedSet is s2builderutil::NormalizeClosedSet: three layers
// (points, polylines, polygons) wrapped by a ClosedSetNormalizer.
func NormalizeClosedSet(outputLayers [3]Layer) []Layer { panic("port stub") }

// ---- boolean_operation.go (src/s2/s2boolean_operation.{h,cc}) ----

// OpType is S2BooleanOperation::OpType.
type OpType int

const (
	OpUnion OpType = iota
	OpIntersection
	OpDifference
	OpSymmetricDifference
)

// PolygonModel is S2BooleanOperation::PolygonModel.
type PolygonModel int

const (
	PolygonModelOpen PolygonModel = iota
	PolygonModelSemiOpen
	PolygonModelClosed
)

// PolylineModel is S2BooleanOperation::PolylineModel.
type PolylineModel int

const (
	PolylineModelOpen PolylineModel = iota
	PolylineModelSemiOpen
	PolylineModelClosed
)

// BooleanOptions is S2BooleanOperation::Options.
type BooleanOptions struct {
	SnapFunction                  s2.Snapper
	PolygonModel                  PolygonModel
	PolylineModel                 PolylineModel
	PolylineLoopsHaveBoundaries   bool
	SplitAllCrossingPolylineEdges bool
}

// DefaultBooleanOptions is S2BooleanOperation::Options().
func DefaultBooleanOptions() BooleanOptions {
	return BooleanOptions{SnapFunction: s2.NewIdentitySnapper(0), PolygonModel: PolygonModelSemiOpen, PolylineModel: PolylineModelClosed, PolylineLoopsHaveBoundaries: true}
}

// BooleanOperation is S2BooleanOperation.
type BooleanOperation struct{ todo int }

// NewBooleanOperation is the constructor taking one layer per dimension
// (points, polylines, polygons), or a single layer.
func NewBooleanOperation(op OpType, layers []Layer, o BooleanOptions) *BooleanOperation {
	panic("port stub")
}

// Build is S2BooleanOperation::Build(a, b).
func (op *BooleanOperation) Build(a, b *s2.ShapeIndex) error { panic("port stub") }

// IsEmptyResult is S2BooleanOperation::IsEmpty(op, a, b, options).
func IsEmptyResult(op OpType, a, b *s2.ShapeIndex, o BooleanOptions) bool { panic("port stub") }
