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

// S2Builder's output layers, ported from s2geometry commit
// 677c8cebcff11ba11accc0b02005bc214e06e9fd:
// src/s2/s2builderutil_s2polygon_layer.cc, s2builderutil_s2polyline_vector_layer.cc,
// s2builderutil_s2point_vector_layer.cc, s2builderutil_closed_set_normalizer.cc
// (Copyright 2017 Google Inc.), s2builderutil_find_polygon_degeneracies.cc
// (Copyright 2018 Google Inc.) and s2builderutil_graph_shape.h (Copyright 2017
// Google Inc.). Layers write golang/geo types; S2Error is a Go error.

import (
	"sort"

	"github.com/golang/geo/s2"
)

// PolygonLayerOptions is s2builderutil::S2PolygonLayer::Options (default:
// directed edges, no validation).
type PolygonLayerOptions struct {
	EdgeType EdgeType
	Validate bool
	// LabelSetIDs and LabelSetLexicon, when set, receive each output loop's
	// edge labels (the label_set_ids constructor).
	LabelSetIDs     *[][]LabelSetID
	LabelSetLexicon *IDSetLexicon
}

type polygonLayer struct {
	out *(*s2.Polygon)
	o   PolygonLayerOptions
}

// NewPolygonLayer is S2PolygonLayer writing *out.
func NewPolygonLayer(out **s2.Polygon, o PolygonLayerOptions) Layer {
	return &polygonLayer{out: out, o: o}
}

// GraphOptions keeps duplicate edges (for clearer errors) and discards
// degenerate edges and sibling pairs.
func (l *polygonLayer) GraphOptions() GraphOptions {
	return GraphOptions{l.o.EdgeType, DegenerateEdgesDiscard, DuplicateEdgesKeep, SiblingPairsDiscard, true}
}

func (l *polygonLayer) appendLoops(g *Graph, edgeLoops [][]EdgeID, loops []*s2.Loop) []*s2.Loop {
	for _, el := range edgeLoops {
		vs := make([]s2.Point, 0, len(el))
		for _, e := range el {
			vs = append(vs, g.Vertex(g.Edge(e).First))
		}
		loops = append(loops, s2.LoopFromPoints(vs))
	}
	return loops
}

func (l *polygonLayer) appendEdgeLabels(g *Graph, edgeLoops [][]EdgeID) {
	if l.o.LabelSetIDs == nil {
		return
	}
	f := NewLabelFetcher(g, l.o.EdgeType)
	for _, el := range edgeLoops {
		ids := make([]LabelSetID, 0, len(el))
		for _, e := range el {
			ids = append(ids, l.o.LabelSetLexicon.Add(f.Fetch(e)...))
		}
		*l.o.LabelSetIDs = append(*l.o.LabelSetIDs, ids)
	}
}

type loopOrigin struct {
	index          int
	containsOrigin bool
}

func (l *polygonLayer) initLoopMap(loops []*s2.Loop) map[*s2.Loop]loopOrigin {
	if l.o.LabelSetIDs == nil {
		return nil
	}
	m := map[*s2.Loop]loopOrigin{}
	for i, lp := range loops {
		m[lp] = loopOrigin{i, lp.ContainsOrigin()}
	}
	return m
}

func (l *polygonLayer) reorderEdgeLabels(m map[*s2.Loop]loopOrigin) {
	if l.o.LabelSetIDs == nil {
		return
	}
	p := *l.out
	newIDs := make([][]LabelSetID, len(*l.o.LabelSetIDs))
	for i := 0; i < p.NumLoops(); i++ {
		lp := p.Loop(i)
		old := m[lp]
		newIDs[i] = (*l.o.LabelSetIDs)[old.index]
		if lp.ContainsOrigin() != old.containsOrigin {
			ids := newIDs[i]
			for a, b := 0, len(ids)-2; a < b; a, b = a+1, b-1 {
				ids[a], ids[b] = ids[b], ids[a]
			}
		}
	}
	*l.o.LabelSetIDs = newIDs
}

// Build is S2PolygonLayer::Build.
func (l *polygonLayer) Build(g *Graph) error {
	if l.o.LabelSetIDs != nil {
		*l.o.LabelSetIDs = nil
	}
	var m map[*s2.Loop]loopOrigin
	if g.NumEdges() == 0 {
		full, err := g.IsFullPolygon()
		if full {
			*l.out = s2.FullPolygon()
		} else {
			*l.out = s2.PolygonFromLoops(nil)
		}
		if err != nil {
			return err
		}
	} else if g.Options().EdgeType == EdgeTypeDirected {
		edgeLoops, err := g.GetDirectedLoops(LoopTypeSimple)
		if err != nil {
			return err
		}
		loops := l.appendLoops(g, edgeLoops, nil)
		l.appendEdgeLabels(g, edgeLoops)
		m = l.initLoopMap(loops)
		*l.out = s2.PolygonFromOrientedLoops(loops)
	} else {
		components, err := g.GetUndirectedComponents(LoopTypeSimple)
		if err != nil {
			return err
		}
		var loops []*s2.Loop
		for _, c := range components {
			loops = l.appendLoops(g, c[0], loops)
			l.appendEdgeLabels(g, c[0])
		}
		m = l.initLoopMap(loops)
		for _, lp := range loops {
			lp.Normalize()
		}
		*l.out = s2.PolygonFromLoops(loops)
	}
	l.reorderEdgeLabels(m)
	if l.o.Validate {
		return (*l.out).Validate()
	}
	return nil
}

// PolylineVectorLayerOptions is S2PolylineVectorLayer::Options.
type PolylineVectorLayerOptions struct {
	EdgeType       EdgeType
	PolylineType   PolylineType
	DuplicateEdges DuplicateEdges
	SiblingPairs   SiblingPairs
	Validate       bool
	// LabelSetIDs and LabelSetLexicon, when set, receive each output
	// polyline's edge labels.
	LabelSetIDs     *[][]LabelSetID
	LabelSetLexicon *IDSetLexicon
}

// DefaultPolylineVectorLayerOptions is S2PolylineVectorLayer::Options():
// directed, PATH, keep duplicates, keep sibling pairs.
func DefaultPolylineVectorLayerOptions() PolylineVectorLayerOptions {
	return PolylineVectorLayerOptions{EdgeType: EdgeTypeDirected, PolylineType: PolylineTypePath, DuplicateEdges: DuplicateEdgesKeep, SiblingPairs: SiblingPairsKeep}
}

type polylineVectorLayer struct {
	out *[]*s2.Polyline
	o   PolylineVectorLayerOptions
}

// NewPolylineVectorLayer is S2PolylineVectorLayer appending to *out.
func NewPolylineVectorLayer(out *[]*s2.Polyline, o PolylineVectorLayerOptions) Layer {
	return &polylineVectorLayer{out: out, o: o}
}

func (l *polylineVectorLayer) GraphOptions() GraphOptions {
	return GraphOptions{l.o.EdgeType, DegenerateEdgesDiscard, l.o.DuplicateEdges, l.o.SiblingPairs, true}
}

// Build is S2PolylineVectorLayer::Build.
func (l *polylineVectorLayer) Build(g *Graph) error {
	var err error
	for _, ep := range g.GetPolylines(l.o.PolylineType) {
		vs := make(s2.Polyline, 0, len(ep)+1)
		vs = append(vs, g.Vertex(g.Edge(ep[0]).First))
		for _, e := range ep {
			vs = append(vs, g.Vertex(g.Edge(e).Second))
		}
		if l.o.Validate && err == nil {
			err = vs.Validate()
		}
		*l.out = append(*l.out, &vs)
		if l.o.LabelSetIDs != nil {
			f := NewLabelFetcher(g, l.o.EdgeType)
			ids := make([]LabelSetID, 0, len(ep))
			for _, e := range ep {
				ids = append(ids, l.o.LabelSetLexicon.Add(f.Fetch(e)...))
			}
			*l.o.LabelSetIDs = append(*l.o.LabelSetIDs, ids)
		}
	}
	return err
}

// PointVectorLayerOptions is S2PointVectorLayer::Options. Its zero value
// merges duplicates, as the C++ default does.
type PointVectorLayerOptions struct {
	DuplicateEdges DuplicateEdges
	// LabelSetIDs and LabelSetLexicon, when set, receive each point's labels.
	LabelSetIDs     *[]LabelSetID
	LabelSetLexicon *IDSetLexicon
}

type pointVectorLayer struct {
	out *[]s2.Point
	o   PointVectorLayerOptions
}

// NewPointVectorLayer is S2PointVectorLayer appending to *out.
func NewPointVectorLayer(out *[]s2.Point, o PointVectorLayerOptions) Layer {
	return &pointVectorLayer{out: out, o: o}
}

func (l *pointVectorLayer) GraphOptions() GraphOptions {
	return GraphOptions{EdgeTypeDirected, DegenerateEdgesKeep, l.o.DuplicateEdges, SiblingPairsKeep, true}
}

// Build is S2PointVectorLayer::Build.
func (l *pointVectorLayer) Build(g *Graph) error {
	f := NewLabelFetcher(g, EdgeTypeDirected)
	var err error
	for e := 0; e < g.NumEdges(); e++ {
		edge := g.Edge(EdgeID(e))
		if edge.First != edge.Second {
			err = errBuilder("Found non-degenerate edges")
			continue
		}
		*l.out = append(*l.out, g.Vertex(edge.First))
		if l.o.LabelSetIDs != nil {
			*l.o.LabelSetIDs = append(*l.o.LabelSetIDs, l.o.LabelSetLexicon.Add(f.Fetch(EdgeID(e))...))
		}
	}
	return err
}

// GraphShape is s2builderutil::GraphShape: a graph's edges as an s2.Shape
// of dimension 1, one chain per edge.
type GraphShape struct {
	// s2.Shape has unexported methods; embedding a golang/geo shape
	// supplies them. Every exported method is overridden below.
	s2.Shape
	g *Graph
}

// NewGraphShape returns g's edges as an s2.Shape.
func NewGraphShape(g *Graph) s2.Shape {
	return &GraphShape{Shape: s2.LaxPolylineFromPoints(nil), g: g}
}

func (s *GraphShape) NumEdges() int { return s.g.NumEdges() }
func (s *GraphShape) Edge(e int) s2.Edge {
	ge := s.g.Edge(EdgeID(e))
	return s2.Edge{V0: s.g.Vertex(ge.First), V1: s.g.Vertex(ge.Second)}
}
func (s *GraphShape) Dimension() int { return 1 }
func (s *GraphShape) ReferencePoint() s2.ReferencePoint {
	return s2.OriginReferencePoint(false)
}
func (s *GraphShape) NumChains() int             { return s.g.NumEdges() }
func (s *GraphShape) Chain(i int) s2.Chain       { return s2.Chain{Start: i, Length: 1} }
func (s *GraphShape) ChainEdge(i, j int) s2.Edge { return s.Edge(i) }
func (s *GraphShape) ChainPosition(e int) s2.ChainPosition {
	return s2.ChainPosition{ChainID: e, Offset: 0}
}
func (s *GraphShape) IsEmpty() bool     { return s.g.NumEdges() == 0 }
func (s *GraphShape) IsFull() bool      { return false }
func (s *GraphShape) typeTag() int32    { return 0 }
func (s *GraphShape) privateInterface() {}

// PolygonDegeneracy is s2builderutil::PolygonDegeneracy.
type PolygonDegeneracy struct {
	EdgeID EdgeID
	IsHole bool
}

type degeneracyComponent struct {
	root         VertexID
	rootSign     int
	degeneracies []PolygonDegeneracy
}

// degeneracyFinder is DegeneracyFinder.
type degeneracyFinder struct {
	g                  *Graph
	in                 *VertexInMap
	out                *VertexOutMap
	isVertexUsed       []bool
	isEdgeDegeneracy   []bool
	isVertexUnbalanced []bool
}

// FindPolygonDegeneracies is s2builderutil::FindPolygonDegeneracies: the
// degenerate edges and sibling pairs of a polygon graph, each classified as
// a shell or a hole, sorted by edge id.
func FindPolygonDegeneracies(g *Graph) ([]PolygonDegeneracy, error) {
	if g.Options().DegenerateEdges == DegenerateEdgesDiscard && g.Options().SiblingPairs == SiblingPairsDiscard {
		return nil, nil
	}
	f := &degeneracyFinder{g: g, in: NewVertexInMap(g), out: NewVertexOutMap(g)}
	return f.run()
}

// IsFullyDegenerate is s2builderutil::IsFullyDegenerate.
func IsFullyDegenerate(g *Graph) bool {
	edges := g.Edges()
	for _, edge := range edges {
		if edge.First == edge.Second {
			continue
		}
		r := edge.Reverse()
		i := sort.Search(len(edges), func(i int) bool { return !edgeLess(edges[i], r) })
		if i == len(edges) || edges[i] != r {
			return false
		}
	}
	return true
}

func (f *degeneracyFinder) run() ([]PolygonDegeneracy, error) {
	n := f.computeDegeneracies()
	if n == 0 {
		return nil, nil
	}
	g := f.g
	if n == g.NumEdges() {
		isHole, err := g.IsFullPolygon()
		result := make([]PolygonDegeneracy, g.NumEdges())
		for e := range result {
			result[e] = PolygonDegeneracy{EdgeID(e), isHole}
		}
		return result, err
	}
	var components []degeneracyComponent
	knownVertex := VertexID(-1)
	knownVertexSign := 0
	numUnknownSigns := 0
	f.isVertexUsed = make([]bool, g.NumVertices())
	for e := 0; e < g.NumEdges(); e++ {
		if f.isEdgeDegeneracy[e] {
			root := g.Edge(EdgeID(e)).First
			if f.isVertexUsed[root] {
				continue
			}
			c := f.buildComponent(root)
			if c.rootSign == 0 {
				numUnknownSigns++
			} else {
				knownVertex = root
				knownVertexSign = c.rootSign
			}
			components = append(components, c)
		}
	}
	if numUnknownSigns > 0 {
		if knownVertexSign == 0 {
			knownVertex = f.findUnbalancedVertex()
			knownVertexSign = f.containsVertexSign(knownVertex)
		}
		// Upstream builds a shape index above 25 unknown signs
		// (ComputeUnknownSignsIndexed); the result is the same, so the
		// brute-force version serves both.
		f.computeUnknownSignsBruteForce(knownVertex, knownVertexSign, components)
	}
	return f.mergeDegeneracies(components), nil
}

func (f *degeneracyFinder) computeDegeneracies() int {
	g := f.g
	f.isEdgeDegeneracy = make([]bool, g.NumEdges())
	f.isVertexUnbalanced = make([]bool, g.NumVertices())
	num := 0
	inEdgeIDs := f.in.InEdgeIDs()
	n := g.NumEdges()
	for in, out := 0, 0; out < n; out++ {
		outEdge := g.Edge(EdgeID(out))
		if outEdge.First == outEdge.Second {
			f.isEdgeDegeneracy[out] = true
			num++
		} else {
			for in < n && edgeLess(g.Edge(inEdgeIDs[in]).Reverse(), outEdge) {
				in++
			}
			if in < n && g.Edge(inEdgeIDs[in]).Reverse() == outEdge {
				f.isEdgeDegeneracy[out] = true
				num++
			} else {
				f.isVertexUnbalanced[outEdge.First] = true
			}
		}
	}
	return num
}

func (f *degeneracyFinder) buildComponent(root VertexID) degeneracyComponent {
	g := f.g
	result := degeneracyComponent{root: root}
	type item struct {
		v          VertexID
		sameInside bool
	}
	frontier := []item{{root, true}}
	f.isVertexUsed[root] = true
	for len(frontier) > 0 {
		v0, v0SameInside := frontier[len(frontier)-1].v, frontier[len(frontier)-1].sameInside
		frontier = frontier[:len(frontier)-1]
		if result.rootSign == 0 && f.isVertexUnbalanced[v0] {
			v0Sign := f.containsVertexSign(v0)
			if v0SameInside {
				result.rootSign = v0Sign
			} else {
				result.rootSign = -v0Sign
			}
		}
		lo, hi := f.out.EdgeIDs(v0)
		for e := lo; e < hi; e++ {
			v1 := g.Edge(e).Second
			sameInside := v0SameInside != f.crossingParity(v0, v1, false)
			if f.isEdgeDegeneracy[e] {
				result.degeneracies = append(result.degeneracies, PolygonDegeneracy{e, sameInside})
			}
			if f.isVertexUsed[v1] {
				continue
			}
			sameInside = sameInside != f.crossingParity(v1, v0, true)
			frontier = append(frontier, item{v1, sameInside})
			f.isVertexUsed[v1] = true
		}
	}
	return result
}

// refDir is S2::RefDir (golang/geo's unexported Point.referenceDir).
func refDir(a s2.Point) s2.Point { return s2.Ortho(a) }

func (f *degeneracyFinder) crossingParity(v0, v1 VertexID, includeSame bool) bool {
	g := f.g
	crossings := 0
	p0, p1 := g.Vertex(v0), g.Vertex(v1)
	p0Ref := refDir(p0)
	for _, edge := range f.out.Edges(v0) {
		if edge.Second == v1 {
			if includeSame {
				crossings++
			}
		} else if s2.OrderedCCW(p0Ref, g.Vertex(edge.Second), p1, p0) {
			crossings++
		}
	}
	for _, e := range f.in.EdgeIDs(v0) {
		edge := g.Edge(e)
		if edge.First == v1 {
			if includeSame {
				crossings++
			}
		} else if s2.OrderedCCW(p0Ref, g.Vertex(edge.First), p1, p0) {
			crossings++
		}
	}
	return crossings&1 != 0
}

func (f *degeneracyFinder) findUnbalancedVertex() VertexID {
	for v := 0; v < f.g.NumVertices(); v++ {
		if f.isVertexUnbalanced[v] {
			return VertexID(v)
		}
	}
	return -1
}

func (f *degeneracyFinder) containsVertexSign(v0 VertexID) int {
	g := f.g
	q := s2.NewContainsVertexQuery(g.Vertex(v0))
	for _, edge := range f.out.Edges(v0) {
		q.AddEdge(g.Vertex(edge.Second), 1)
	}
	for _, e := range f.in.EdgeIDs(v0) {
		q.AddEdge(g.Vertex(g.Edge(e).First), -1)
	}
	return q.ContainsVertex()
}

func (f *degeneracyFinder) computeUnknownSignsBruteForce(knownVertex VertexID, knownVertexSign int, components []degeneracyComponent) {
	g := f.g
	for i := range components {
		c := &components[i]
		if c.rootSign != 0 {
			continue
		}
		inside := knownVertexSign > 0
		crosser := s2.NewEdgeCrosser(g.Vertex(knownVertex), g.Vertex(c.root))
		for e := 0; e < g.NumEdges(); e++ {
			if f.isEdgeDegeneracy[e] {
				continue
			}
			edge := g.Edge(EdgeID(e))
			if crosser.EdgeOrVertexCrossing(g.Vertex(edge.First), g.Vertex(edge.Second)) {
				inside = !inside
			}
		}
		if inside {
			c.rootSign = 1
		} else {
			c.rootSign = -1
		}
	}
}

func (f *degeneracyFinder) mergeDegeneracies(components []degeneracyComponent) []PolygonDegeneracy {
	var result []PolygonDegeneracy
	for _, c := range components {
		invert := c.rootSign < 0
		for _, d := range c.degeneracies {
			result = append(result, PolygonDegeneracy{d.EdgeID, d.IsHole != invert})
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].EdgeID != result[j].EdgeID {
			return result[i].EdgeID < result[j].EdgeID
		}
		return !result[i].IsHole && result[j].IsHole
	})
	return result
}

// ClosedSetNormalizerOptions is ClosedSetNormalizer::Options.
type ClosedSetNormalizerOptions struct {
	SuppressLowerDimensions bool // default true
}

// DefaultClosedSetNormalizerOptions is ClosedSetNormalizer::Options().
func DefaultClosedSetNormalizerOptions() ClosedSetNormalizerOptions {
	return ClosedSetNormalizerOptions{SuppressLowerDimensions: true}
}

// ClosedSetNormalizer is s2builderutil::ClosedSetNormalizer: it turns the
// point, polyline and polygon graphs of a closed-model boolean operation
// into a normalized set (degenerate polygon shells demoted, coincident
// lower-dimensional pieces suppressed).
type ClosedSetNormalizer struct {
	options           ClosedSetNormalizerOptions
	graphOptionsOut   [3]GraphOptions
	graphOptionsIn    [3]GraphOptions
	inEdges2          []EdgeID
	isSuppressedV     []bool
	newEdges          [3][]GraphEdge
	newInputEdgeIDs   [3][]InputEdgeIDSetID
	newInputIDLexicon *IDSetLexicon
}

var normalizerSentinel = GraphEdge{1<<31 - 1, 1<<31 - 1}

// NewClosedSetNormalizer is ClosedSetNormalizer(options, graph_options_out).
func NewClosedSetNormalizer(options ClosedSetNormalizerOptions, out [3]GraphOptions) *ClosedSetNormalizer {
	n := &ClosedSetNormalizer{options: options, graphOptionsOut: out, graphOptionsIn: out}
	for dim := 0; dim < 3; dim++ {
		n.graphOptionsIn[dim].AllowVertexFiltering = false
	}
	n.graphOptionsIn[1].DegenerateEdges = DegenerateEdgesDiscardExcess
	n.graphOptionsIn[2].DegenerateEdges = DegenerateEdgesDiscardExcess
	n.graphOptionsIn[2].SiblingPairs = SiblingPairsDiscardExcess
	return n
}

// GraphOptions are the options the input graphs must be built with.
func (n *ClosedSetNormalizer) GraphOptions() [3]GraphOptions { return n.graphOptionsIn }

// Run is ClosedSetNormalizer::Run.
func (n *ClosedSetNormalizer) Run(g [3]*Graph) ([3]*Graph, error) {
	if n.options.SuppressLowerDimensions {
		n.inEdges2 = g[2].GetInEdgeIDs()
		n.isSuppressedV = make([]bool, len(g[0].Vertices()))
		for dim := 1; dim <= 2; dim++ {
			for _, edge := range g[dim].Edges() {
				if edge.First != edge.Second {
					n.isSuppressedV[edge.First] = true
					n.isSuppressedV[edge.Second] = true
				}
			}
		}
	}
	err := n.normalizeEdges(g)
	var modified [3]bool
	anyModified := false
	for dim := 2; dim >= 0; dim-- {
		if len(n.newEdges[dim]) != g[dim].NumEdges() {
			anyModified = true
		}
		modified[dim] = anyModified
	}
	var out [3]*Graph
	if !anyModified {
		for dim := 0; dim < 3; dim++ {
			out[dim] = NewGraph(n.graphOptionsOut[dim], g[dim].Vertices(), g[dim].Edges(), g[dim].InputEdgeIDSetIDs(), g[dim].InputEdgeIDSetLexicon(), g[dim].LabelSetIDs(), g[dim].LabelSetLexicon(), g[dim].IsFullPolygonPredicate())
		}
		return out, err
	}
	n.newInputIDLexicon = cloneIDSetLexicon(g[0].InputEdgeIDSetLexicon())
	for dim := 0; dim < 3; dim++ {
		if modified[dim] {
			if e := ProcessEdges(&n.graphOptionsOut[dim], &n.newEdges[dim], &n.newInputEdgeIDs[dim], n.newInputIDLexicon); e != nil && err == nil {
				err = e
			}
		}
		out[dim] = NewGraph(n.graphOptionsOut[dim], g[dim].Vertices(), n.newEdges[dim], n.newInputEdgeIDs[dim], n.newInputIDLexicon, g[dim].LabelSetIDs(), g[dim].LabelSetLexicon(), g[dim].IsFullPolygonPredicate())
	}
	return out, err
}

func advanceEdge(g *Graph, e *EdgeID) GraphEdge {
	*e++
	if int(*e) == g.NumEdges() {
		return normalizerSentinel
	}
	return g.Edge(*e)
}

func advanceIncoming(g *Graph, inEdges []EdgeID, i *int) GraphEdge {
	*i++
	if *i == len(inEdges) {
		return normalizerSentinel
	}
	return g.Edge(inEdges[*i]).Reverse()
}

func edgeLE(a, b GraphEdge) bool { return !edgeLess(b, a) }

func (n *ClosedSetNormalizer) normalizeEdges(g [3]*Graph) error {
	degeneracies, err := FindPolygonDegeneracies(g[2])
	di := 0
	e0, e1, e2 := EdgeID(-1), EdgeID(-1), EdgeID(-1)
	inE2 := -1
	edge0 := advanceEdge(g[0], &e0)
	edge1 := advanceEdge(g[1], &e1)
	edge2 := advanceEdge(g[2], &e2)
	inEdge2 := advanceIncoming(g[2], n.inEdges2, &inE2)
	for {
		if edgeLE(edge2, edge1) && edgeLE(edge2, edge0) {
			if edge2 == normalizerSentinel {
				break
			}
			if di == len(degeneracies) || degeneracies[di].EdgeID != e2 {
				n.addEdge(2, g[2], e2)
				for n.options.SuppressLowerDimensions && edge1 == edge2 {
					edge1 = advanceEdge(g[1], &e1)
				}
			} else {
				isHole := degeneracies[di].IsHole
				di++
				if !isHole {
					if edge2.First != edge2.Second {
						n.addEdge(1, g[2], e2)
						for edge1 == edge2 {
							n.addEdge(1, g[1], e1)
							edge1 = advanceEdge(g[1], &e1)
						}
					} else if !n.isSuppressed(edge2.First) {
						n.addEdge(0, g[2], e2)
					}
				}
			}
			edge2 = advanceEdge(g[2], &e2)
		} else if edgeLE(edge1, edge0) {
			if edge1.First != edge1.Second {
				for edgeLess(inEdge2, edge1) {
					inEdge2 = advanceIncoming(g[2], n.inEdges2, &inE2)
				}
				if edge1 != inEdge2 {
					n.addEdge(1, g[1], e1)
				}
			} else {
				if !n.isSuppressed(edge1.First) {
					n.addEdge(0, g[1], e1)
				}
				if g[1].Options().EdgeType == EdgeTypeUndirected {
					e1++
				}
			}
			edge1 = advanceEdge(g[1], &e1)
		} else {
			if !n.isSuppressed(edge0.First) {
				n.addEdge(0, g[0], e0)
			}
			edge0 = advanceEdge(g[0], &e0)
		}
	}
	return err
}

func (n *ClosedSetNormalizer) addEdge(newDim int, g *Graph, e EdgeID) {
	n.newEdges[newDim] = append(n.newEdges[newDim], g.Edge(e))
	n.newInputEdgeIDs[newDim] = append(n.newInputEdgeIDs[newDim], g.InputEdgeIDSetID(e))
}

func (n *ClosedSetNormalizer) isSuppressed(v VertexID) bool {
	return n.options.SuppressLowerDimensions && n.isSuppressedV[v]
}

// normalizeClosedSetImpl is NormalizeClosedSetImpl: the three returned
// layers collect their graphs, and the last one built runs the normalizer
// and builds the output layers.
type normalizeClosedSetImpl struct {
	outputLayers [3]Layer
	normalizer   *ClosedSetNormalizer
	graphs       [3]*Graph
	graphsLeft   int
}

type dimensionLayer struct {
	dimension    int
	graphOptions GraphOptions
	impl         *normalizeClosedSetImpl
}

func (l *dimensionLayer) GraphOptions() GraphOptions { return l.graphOptions }
func (l *dimensionLayer) Build(g *Graph) error       { return l.impl.build(l.dimension, g) }

func (impl *normalizeClosedSetImpl) build(dimension int, g *Graph) error {
	impl.graphs[dimension] = g
	impl.graphsLeft--
	if impl.graphsLeft > 0 {
		return nil
	}
	output, err := impl.normalizer.Run(impl.graphs)
	for dim := 0; dim < 3; dim++ {
		if e := impl.outputLayers[dim].Build(output[dim]); e != nil {
			err = e
		}
	}
	return err
}

// NormalizeClosedSet is s2builderutil::NormalizeClosedSet: three layers
// (points, polylines, polygons) whose graphs are normalized by a
// ClosedSetNormalizer before being passed to outputLayers.
func NormalizeClosedSet(outputLayers [3]Layer) []Layer {
	return NormalizeClosedSetWithOptions(outputLayers, DefaultClosedSetNormalizerOptions())
}

// NormalizeClosedSetWithOptions is NormalizeClosedSet with options.
func NormalizeClosedSetWithOptions(outputLayers [3]Layer, o ClosedSetNormalizerOptions) []Layer {
	impl := &normalizeClosedSetImpl{
		outputLayers: outputLayers,
		normalizer:   NewClosedSetNormalizer(o, [3]GraphOptions{outputLayers[0].GraphOptions(), outputLayers[1].GraphOptions(), outputLayers[2].GraphOptions()}),
		graphsLeft:   3,
	}
	in := impl.normalizer.GraphOptions()
	out := make([]Layer, 3)
	for dim := 0; dim < 3; dim++ {
		out[dim] = &dimensionLayer{dim, in[dim], impl}
	}
	return out
}

// cloneIDSetLexicon copies a lexicon (IdSetLexicon's copy constructor).
func cloneIDSetLexicon(l *IDSetLexicon) *IDSetLexicon {
	c := NewIDSetLexicon()
	for _, set := range l.values {
		c.Add(set...)
	}
	return c
}
