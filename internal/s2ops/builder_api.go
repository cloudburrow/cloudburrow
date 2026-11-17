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
