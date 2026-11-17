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

// S2Builder::Graph, ported from src/s2/s2builder_graph.cc and the inline
// functions of src/s2/s2builder_graph.h (s2geometry commit
// 677c8cebcff11ba11accc0b02005bc214e06e9fd). Memory tracking is dropped;
// S2Error is returned as a Go error.

import (
	"math"
	"slices"
	"sort"

	"github.com/golang/geo/s2"
)

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

// NoInputEdgeID is S2Builder::Graph::kNoInputEdgeId.
const NoInputEdgeID = InputEdgeID(math.MaxInt32)

// NewGraph is Graph's constructor. The slices are referenced, not copied.
func NewGraph(options GraphOptions, vertices []s2.Point, edges []GraphEdge, inputEdgeIDSetIDs []InputEdgeIDSetID, inputEdgeIDSetLexicon *IDSetLexicon, labelSetIDs []LabelSetID, labelSetLexicon *IDSetLexicon, isFullPolygonPredicate IsFullPolygonPredicate) *Graph {
	if inputEdgeIDSetLexicon == nil {
		inputEdgeIDSetLexicon = NewIDSetLexicon()
	}
	if labelSetLexicon == nil {
		labelSetLexicon = NewIDSetLexicon()
	}
	return &Graph{
		options:                options,
		numVertices:            len(vertices),
		vertices:               vertices,
		edges:                  edges,
		inputEdgeIDSetIDs:      inputEdgeIDSetIDs,
		inputEdgeIDSetLexicon:  inputEdgeIDSetLexicon,
		labelSetIDs:            labelSetIDs,
		labelSetLexicon:        labelSetLexicon,
		isFullPolygonPredicate: isFullPolygonPredicate,
	}
}

func (g *Graph) Options() GraphOptions                      { return g.options }
func (g *Graph) NumVertices() int                           { return g.numVertices }
func (g *Graph) Vertex(v VertexID) s2.Point                 { return g.vertices[v] }
func (g *Graph) Vertices() []s2.Point                       { return g.vertices }
func (g *Graph) NumEdges() int                              { return len(g.edges) }
func (g *Graph) Edge(e EdgeID) GraphEdge                    { return g.edges[e] }
func (g *Graph) Edges() []GraphEdge                         { return g.edges }
func (g *Graph) InputEdgeIDSetID(e EdgeID) InputEdgeIDSetID { return g.inputEdgeIDSetIDs[e] }
func (g *Graph) InputEdgeIDSetIDs() []InputEdgeIDSetID      { return g.inputEdgeIDSetIDs }
func (g *Graph) InputEdgeIDSetLexicon() *IDSetLexicon       { return g.inputEdgeIDSetLexicon }
func (g *Graph) LabelSetIDs() []LabelSetID                  { return g.labelSetIDs }
func (g *Graph) LabelSetLexicon() *IDSetLexicon             { return g.labelSetLexicon }

// IsFullPolygonPredicate returns the graph's predicate.
func (g *Graph) IsFullPolygonPredicate() IsFullPolygonPredicate { return g.isFullPolygonPredicate }

// InputEdgeIDs is input_edge_ids(e): the input edges that snapped to e.
func (g *Graph) InputEdgeIDs(e EdgeID) []int32 {
	return g.inputEdgeIDSetLexicon.IDSet(g.inputEdgeIDSetIDs[e])
}

// LabelSetID is label_set_id(e) for an input edge.
func (g *Graph) LabelSetID(e InputEdgeID) LabelSetID {
	if len(g.labelSetIDs) == 0 {
		return emptySetID
	}
	return g.labelSetIDs[e]
}

// Labels is labels(e) for an input edge.
func (g *Graph) Labels(e InputEdgeID) []Label { return g.labelSetLexicon.IDSet(g.LabelSetID(e)) }

// IsFullPolygon calls the graph's IsFullPolygonPredicate.
func (g *Graph) IsFullPolygon() (bool, error) {
	if g.isFullPolygonPredicate == nil {
		return IsFullPolygonUnspecified(g)
	}
	return g.isFullPolygonPredicate(g)
}

// stableLessThan is Graph::StableLessThan.
func stableLessThan(a, b GraphEdge, ai, bi EdgeID) bool {
	if a.First < b.First {
		return true
	}
	if b.First < a.First {
		return false
	}
	if a.Second < b.Second {
		return true
	}
	if b.Second < a.Second {
		return false
	}
	return ai < bi
}

func iota32(n int) []EdgeID {
	s := make([]EdgeID, n)
	for i := range s {
		s[i] = EdgeID(i)
	}
	return s
}

// GetInEdgeIDs is Graph::GetInEdgeIds: edge ids sorted by reversed edge.
func (g *Graph) GetInEdgeIDs() []EdgeID {
	in := iota32(g.NumEdges())
	sort.Slice(in, func(i, j int) bool {
		ai, bi := in[i], in[j]
		return stableLessThan(g.edges[ai].Reverse(), g.edges[bi].Reverse(), ai, bi)
	})
	return in
}

// GetSiblingMap is Graph::GetSiblingMap.
func (g *Graph) GetSiblingMap() []EdgeID {
	in := g.GetInEdgeIDs()
	g.MakeSiblingMap(in)
	return in
}

// MakeSiblingMap is Graph::MakeSiblingMap, converting inEdgeIDs in place.
func (g *Graph) MakeSiblingMap(inEdgeIDs []EdgeID) {
	if g.options.EdgeType == EdgeTypeDirected {
		return
	}
	if g.options.DegenerateEdges == DegenerateEdgesDiscard {
		return
	}
	for e := 0; e < g.NumEdges(); e++ {
		v := g.edges[e].First
		if g.edges[e].Second == v {
			inEdgeIDs[e] = EdgeID(e + 1)
			inEdgeIDs[e+1] = EdgeID(e)
			e++
		}
	}
}

// VertexOutMap is Graph::VertexOutMap.
type VertexOutMap struct {
	edges     []GraphEdge
	edgeBegin []EdgeID
}

// NewVertexOutMap is VertexOutMap(g).
func NewVertexOutMap(g *Graph) *VertexOutMap {
	m := &VertexOutMap{edges: g.edges, edgeBegin: make([]EdgeID, 0, g.numVertices+1)}
	e := 0
	for v := 0; v <= g.numVertices; v++ {
		for e < g.NumEdges() && int(g.edges[e].First) < v {
			e++
		}
		m.edgeBegin = append(m.edgeBegin, EdgeID(e))
	}
	return m
}

// Degree is the out-degree of v.
func (m *VertexOutMap) Degree(v VertexID) int { return int(m.edgeBegin[v+1] - m.edgeBegin[v]) }

// EdgeIDs is edge_ids(v): the half-open range of v's outgoing edge ids.
func (m *VertexOutMap) EdgeIDs(v VertexID) (begin, end EdgeID) {
	return m.edgeBegin[v], m.edgeBegin[v+1]
}

// Edges is edges(v).
func (m *VertexOutMap) Edges(v VertexID) []GraphEdge { return m.edges[m.edgeBegin[v]:m.edgeBegin[v+1]] }

// EdgeIDsTo is edge_ids(v0, v1): the range of edges from v0 to v1.
func (m *VertexOutMap) EdgeIDsTo(v0, v1 VertexID) (begin, end EdgeID) {
	b, e := m.edgeBegin[v0], m.edgeBegin[v0+1]
	es := m.edges[b:e]
	lo := sort.Search(len(es), func(i int) bool { return es[i].Second >= v1 })
	hi := sort.Search(len(es), func(i int) bool { return es[i].Second > v1 })
	return b + EdgeID(lo), b + EdgeID(hi)
}

// VertexInMap is Graph::VertexInMap.
type VertexInMap struct {
	inEdgeIDs   []EdgeID
	inEdgeBegin []EdgeID
}

// NewVertexInMap is VertexInMap(g).
func NewVertexInMap(g *Graph) *VertexInMap {
	m := &VertexInMap{inEdgeIDs: g.GetInEdgeIDs(), inEdgeBegin: make([]EdgeID, 0, g.numVertices+1)}
	e := 0
	for v := 0; v <= g.numVertices; v++ {
		for e < g.NumEdges() && int(g.edges[m.inEdgeIDs[e]].Second) < v {
			e++
		}
		m.inEdgeBegin = append(m.inEdgeBegin, EdgeID(e))
	}
	return m
}

// Degree is the in-degree of v.
func (m *VertexInMap) Degree(v VertexID) int { return int(m.inEdgeBegin[v+1] - m.inEdgeBegin[v]) }

// EdgeIDs is edge_ids(v): the ids of the edges ending at v.
func (m *VertexInMap) EdgeIDs(v VertexID) []EdgeID {
	return m.inEdgeIDs[m.inEdgeBegin[v]:m.inEdgeBegin[v+1]]
}

// InEdgeIDs is in_edge_ids().
func (m *VertexInMap) InEdgeIDs() []EdgeID { return m.inEdgeIDs }

// LabelFetcher is Graph::LabelFetcher.
type LabelFetcher struct {
	g          *Graph
	edgeType   EdgeType
	siblingMap []EdgeID
}

// NewLabelFetcher is LabelFetcher(g, edge_type).
func NewLabelFetcher(g *Graph, edgeType EdgeType) *LabelFetcher {
	f := &LabelFetcher{g: g, edgeType: edgeType}
	if edgeType == EdgeTypeUndirected {
		f.siblingMap = g.GetSiblingMap()
	}
	return f
}

// Fetch returns the sorted, distinct labels of edge e.
func (f *LabelFetcher) Fetch(e EdgeID) []Label {
	var labels []Label
	for _, id := range f.g.InputEdgeIDs(e) {
		labels = append(labels, f.g.Labels(id)...)
	}
	if f.edgeType == EdgeTypeUndirected {
		for _, id := range f.g.InputEdgeIDs(f.siblingMap[e]) {
			labels = append(labels, f.g.Labels(id)...)
		}
	}
	if len(labels) > 1 {
		slices.Sort(labels)
		labels = slices.Compact(labels)
	}
	return labels
}

// MinInputEdgeID is min_input_edge_id(e).
func (g *Graph) MinInputEdgeID(e EdgeID) InputEdgeID {
	ids := g.InputEdgeIDs(e)
	if len(ids) == 0 {
		return NoInputEdgeID
	}
	return ids[0]
}

// GetMinInputEdgeIDs is Graph::GetMinInputEdgeIds.
func (g *Graph) GetMinInputEdgeIDs() []InputEdgeID {
	out := make([]InputEdgeID, g.NumEdges())
	for e := range out {
		out[e] = g.MinInputEdgeID(EdgeID(e))
	}
	return out
}

// GetInputEdgeOrder is Graph::GetInputEdgeOrder.
func (g *Graph) GetInputEdgeOrder(inputIDs []InputEdgeID) []EdgeID {
	order := iota32(len(inputIDs))
	sort.Slice(order, func(i, j int) bool {
		a, b := order[i], order[j]
		if inputIDs[a] != inputIDs[b] {
			return inputIDs[a] < inputIDs[b]
		}
		return a < b
	})
	return order
}

// vertexEdge is the VertexEdge struct of s2builder_graph.cc.
type vertexEdge struct {
	incoming bool
	index    EdgeID
	endpoint VertexID
	rank     int32
}

// addVertexEdges is AddVertexEdges.
func addVertexEdges(outBegin, outEnd, inBegin, inEnd EdgeID, v1 VertexID, v0Edges []vertexEdge) []vertexEdge {
	rank := int32(0)
	for inEnd-inBegin > outEnd-outBegin {
		inEnd--
		v0Edges = append(v0Edges, vertexEdge{true, inEnd, v1, rank})
		rank++
	}
	for inEnd > inBegin {
		v0Edges = append(v0Edges, vertexEdge{false, outBegin, v1, rank})
		outBegin++
		rank++
		inEnd--
		v0Edges = append(v0Edges, vertexEdge{true, inEnd, v1, rank})
		rank++
	}
	for outEnd > outBegin {
		v0Edges = append(v0Edges, vertexEdge{false, outBegin, v1, rank})
		outBegin++
		rank++
	}
	return v0Edges
}

func edgeMin(a, b GraphEdge) GraphEdge {
	if edgeLess(b, a) {
		return b
	}
	return a
}

// GetLeftTurnMap is Graph::GetLeftTurnMap: for each incoming edge, the next
// outgoing edge clockwise around their shared vertex.
func (g *Graph) GetLeftTurnMap(inEdgeIDs []EdgeID) ([]EdgeID, error) {
	n := g.NumEdges()
	leftTurnMap := make([]EdgeID, n)
	for i := range leftTurnMap {
		leftTurnMap[i] = -1
	}
	if n == 0 {
		return leftTurnMap, nil
	}
	var err error
	var v0Edges []vertexEdge
	var eIn, eOut []EdgeID
	out, in := 0, 0
	sentinel := GraphEdge{VertexID(g.numVertices), VertexID(g.numVertices)}
	outEdge := g.edges[out]
	inEdge := g.edges[inEdgeIDs[in]]
	minEdge := edgeMin(outEdge, inEdge.Reverse())
	for minEdge != sentinel {
		v0 := minEdge.First
		for ; minEdge.First == v0; minEdge = edgeMin(outEdge, inEdge.Reverse()) {
			v1 := minEdge.Second
			outBegin, inBegin := out, in
			for outEdge == minEdge {
				out++
				if out == n {
					outEdge = sentinel
				} else {
					outEdge = g.edges[out]
				}
			}
			for inEdge.Reverse() == minEdge {
				in++
				if in == n {
					inEdge = sentinel
				} else {
					inEdge = g.edges[inEdgeIDs[in]]
				}
			}
			if v0 != v1 {
				v0Edges = addVertexEdges(EdgeID(outBegin), EdgeID(out), EdgeID(inBegin), EdgeID(in), v1, v0Edges)
			} else {
				for ; inBegin < in; inBegin++ {
					leftTurnMap[inBegin] = EdgeID(inBegin)
				}
			}
		}
		if len(v0Edges) == 0 {
			continue
		}
		minEndpoint := v0Edges[0].endpoint
		rest := v0Edges[1:]
		sort.Slice(rest, func(i, j int) bool {
			a, b := rest[i], rest[j]
			if a.endpoint == b.endpoint {
				return a.rank < b.rank
			}
			if a.endpoint == minEndpoint {
				return true
			}
			if b.endpoint == minEndpoint {
				return false
			}
			return !s2.OrderedCCW(g.Vertex(a.endpoint), g.Vertex(b.endpoint), g.Vertex(minEndpoint), g.Vertex(v0))
		})
		for _, e := range v0Edges {
			if e.incoming {
				eIn = append(eIn, inEdgeIDs[e.index])
			} else if len(eIn) > 0 {
				leftTurnMap[eIn[len(eIn)-1]] = e.index
				eIn = eIn[:len(eIn)-1]
			} else {
				eOut = append(eOut, e.index)
			}
		}
		slices.Reverse(eOut)
		for len(eOut) > 0 && len(eIn) > 0 {
			leftTurnMap[eIn[len(eIn)-1]] = eOut[len(eOut)-1]
			eOut = eOut[:len(eOut)-1]
			eIn = eIn[:len(eIn)-1]
		}
		if len(eIn) > 0 && err == nil {
			err = errBuilder("Given edges do not form loops (indegree != outdegree)")
		}
		eIn = eIn[:0]
		eOut = eOut[:0]
		v0Edges = v0Edges[:0]
	}
	return leftTurnMap, err
}

// CanonicalizeLoopOrder is Graph::CanonicalizeLoopOrder: rotates loop so
// that its highest input edge id comes last.
func CanonicalizeLoopOrder(minInputIDs []InputEdgeID, loop []EdgeID) {
	if len(loop) == 0 {
		return
	}
	pos := 0
	sawGap := false
	for i := 1; i < len(loop); i++ {
		cmp := int64(minInputIDs[loop[i]]) - int64(minInputIDs[loop[pos]])
		if cmp < 0 {
			sawGap = true
		} else if cmp > 0 || !sawGap {
			pos = i
			sawGap = false
		}
	}
	pos++
	if pos == len(loop) {
		pos = 0
	}
	rotated := append(append([]EdgeID(nil), loop[pos:]...), loop[:pos]...)
	copy(loop, rotated)
}

// CanonicalizeVectorOrder is Graph::CanonicalizeVectorOrder.
func CanonicalizeVectorOrder(minInputIDs []InputEdgeID, chains [][]EdgeID) {
	sort.Slice(chains, func(i, j int) bool {
		a, b := chains[i][0], chains[j][0]
		if minInputIDs[a] != minInputIDs[b] {
			return minInputIDs[a] < minInputIDs[b]
		}
		return a < b
	})
}

// GetDirectedLoops is Graph::GetDirectedLoops.
func (g *Graph) GetDirectedLoops(loopType LoopType) ([][]EdgeID, error) {
	leftTurnMap, err := g.GetLeftTurnMap(g.GetInEdgeIDs())
	if err != nil {
		return nil, err
	}
	minInputIDs := g.GetMinInputEdgeIDs()
	var pathIndex []int
	if loopType == LoopTypeSimple {
		pathIndex = make([]int, g.numVertices)
		for i := range pathIndex {
			pathIndex[i] = -1
		}
	}
	var loops [][]EdgeID
	var path []EdgeID
	for start := EdgeID(0); int(start) < g.NumEdges(); start++ {
		if leftTurnMap[start] < 0 {
			continue
		}
		for e := start; leftTurnMap[e] >= 0; {
			path = append(path, e)
			next := leftTurnMap[e]
			leftTurnMap[e] = -1
			if loopType == LoopTypeSimple {
				pathIndex[g.edges[e].First] = len(path) - 1
				loopStart := pathIndex[g.edges[e].Second]
				if loopStart >= 0 {
					loop := append([]EdgeID(nil), path[loopStart:]...)
					path = path[:loopStart]
					for _, e2 := range loop {
						pathIndex[g.edges[e2].First] = -1
					}
					CanonicalizeLoopOrder(minInputIDs, loop)
					loops = append(loops, loop)
				}
			}
			e = next
		}
		if loopType != LoopTypeSimple {
			loop := append([]EdgeID(nil), path...)
			CanonicalizeLoopOrder(minInputIDs, loop)
			loops = append(loops, loop)
			path = path[:0]
		}
	}
	CanonicalizeVectorOrder(minInputIDs, loops)
	return loops, nil
}

// GetDirectedComponents is Graph::GetDirectedComponents.
func (g *Graph) GetDirectedComponents(degenerateBoundaries DegenerateBoundaries) ([][][]EdgeID, error) {
	siblingMap := g.GetSiblingMap()
	leftTurnMap, err := g.GetLeftTurnMap(siblingMap)
	if err != nil {
		return nil, err
	}
	minInputIDs := g.GetMinInputEdgeIDs()
	var frontier []EdgeID
	var pathIndex []int
	if degenerateBoundaries == DegenerateBoundariesDiscard {
		pathIndex = make([]int, g.NumEdges())
		for i := range pathIndex {
			pathIndex[i] = -1
		}
	}
	var components [][][]EdgeID
	for start := EdgeID(0); int(start) < g.NumEdges(); start++ {
		if leftTurnMap[start] < 0 {
			continue
		}
		var component [][]EdgeID
		frontier = append(frontier, start)
		for len(frontier) > 0 {
			e := frontier[len(frontier)-1]
			frontier = frontier[:len(frontier)-1]
			if leftTurnMap[e] < 0 {
				continue
			}
			var path []EdgeID
			for leftTurnMap[e] >= 0 {
				path = append(path, e)
				next := leftTurnMap[e]
				leftTurnMap[e] = -1
				sibling := siblingMap[e]
				if leftTurnMap[sibling] >= 0 {
					frontier = append(frontier, sibling)
				}
				if degenerateBoundaries == DegenerateBoundariesDiscard {
					pathIndex[e] = len(path) - 1
					siblingIndex := pathIndex[sibling]
					if siblingIndex >= 0 {
						if siblingIndex == len(path)-2 {
							path = path[:siblingIndex]
						} else {
							loop := append([]EdgeID(nil), path[siblingIndex+1:len(path)-1]...)
							path = path[:siblingIndex]
							for _, e2 := range loop {
								pathIndex[e2] = -1
							}
							CanonicalizeLoopOrder(minInputIDs, loop)
							component = append(component, loop)
						}
					}
				}
				e = next
			}
			if degenerateBoundaries == DegenerateBoundariesDiscard {
				for _, e2 := range path {
					pathIndex[e2] = -1
				}
			}
			// Upstream pushes the path even when peeling left it empty
			// (and then reads its first edge); an empty path is skipped.
			if len(path) > 0 {
				CanonicalizeLoopOrder(minInputIDs, path)
				component = append(component, path)
			}
		}
		if len(component) == 0 {
			continue
		}
		CanonicalizeVectorOrder(minInputIDs, component)
		components = append(components, component)
	}
	sort.Slice(components, func(i, j int) bool {
		return minInputIDs[components[i][0][0]] < minInputIDs[components[j][0][0]]
	})
	return components, nil
}

func markEdgeUsed(slot int) EdgeID { return EdgeID(-1 - slot) }

// GetUndirectedComponents is Graph::GetUndirectedComponents.
func (g *Graph) GetUndirectedComponents(loopType LoopType) ([][2][][]EdgeID, error) {
	siblingMap := g.GetInEdgeIDs()
	leftTurnMap, err := g.GetLeftTurnMap(siblingMap)
	if err != nil {
		return nil, err
	}
	g.MakeSiblingMap(siblingMap)
	minInputIDs := g.GetMinInputEdgeIDs()
	type slotted struct {
		e    EdgeID
		slot int
	}
	var frontier []slotted
	var pathIndex []int
	if loopType == LoopTypeSimple {
		pathIndex = make([]int, g.numVertices)
		for i := range pathIndex {
			pathIndex[i] = -1
		}
	}
	var components [][2][][]EdgeID
	for minStart := EdgeID(0); int(minStart) < g.NumEdges(); minStart++ {
		if leftTurnMap[minStart] < 0 {
			continue
		}
		var component [2][][]EdgeID
		frontier = append(frontier, slotted{minStart, 0})
		for len(frontier) > 0 {
			start, slot := frontier[len(frontier)-1].e, frontier[len(frontier)-1].slot
			frontier = frontier[:len(frontier)-1]
			if leftTurnMap[start] < 0 {
				continue
			}
			var path []EdgeID
			for e := start; leftTurnMap[e] >= 0; {
				path = append(path, e)
				next := leftTurnMap[e]
				leftTurnMap[e] = markEdgeUsed(slot)
				sibling := siblingMap[e]
				if leftTurnMap[sibling] >= 0 {
					frontier = append(frontier, slotted{sibling, 1 - slot})
				} else if leftTurnMap[sibling] != markEdgeUsed(1-slot) {
					return nil, errBuilder("Given undirected edges do not form loops")
				}
				if loopType == LoopTypeSimple {
					pathIndex[g.edges[e].First] = len(path) - 1
					loopStart := pathIndex[g.edges[e].Second]
					if loopStart >= 0 {
						loop := append([]EdgeID(nil), path[loopStart:]...)
						path = path[:loopStart]
						for _, e2 := range loop {
							pathIndex[g.edges[e2].First] = -1
						}
						CanonicalizeLoopOrder(minInputIDs, loop)
						component[slot] = append(component[slot], loop)
					}
				}
				e = next
			}
			if loopType != LoopTypeSimple {
				CanonicalizeLoopOrder(minInputIDs, path)
				component[slot] = append(component[slot], path)
			}
		}
		CanonicalizeVectorOrder(minInputIDs, component[0])
		CanonicalizeVectorOrder(minInputIDs, component[1])
		if minInputIDs[component[0][0][0]] > minInputIDs[component[1][0][0]] {
			component[0], component[1] = component[1], component[0]
		}
		components = append(components, component)
	}
	sort.Slice(components, func(i, j int) bool {
		return minInputIDs[components[i][0][0][0]] < minInputIDs[components[j][0][0][0]]
	})
	return components, nil
}

// GetPolylines is Graph::GetPolylines.
func (g *Graph) GetPolylines(polylineType PolylineType) [][]EdgeID {
	b := newPolylineBuilder(g)
	if polylineType == PolylineTypePath {
		return b.buildPaths()
	}
	return b.buildWalks()
}

// polylineBuilder is Graph::PolylineBuilder.
type polylineBuilder struct {
	g           *Graph
	in          *VertexInMap
	out         *VertexOutMap
	siblingMap  []EdgeID
	minInputIDs []InputEdgeID
	directed    bool
	edgesLeft   int
	used        []bool
	excessUsed  map[VertexID]int
}

func newPolylineBuilder(g *Graph) *polylineBuilder {
	b := &polylineBuilder{
		g:           g,
		in:          NewVertexInMap(g),
		out:         NewVertexOutMap(g),
		minInputIDs: g.GetMinInputEdgeIDs(),
		directed:    g.options.EdgeType == EdgeTypeDirected,
		used:        make([]bool, g.NumEdges()),
		excessUsed:  map[VertexID]int{},
	}
	if b.directed {
		b.edgesLeft = g.NumEdges()
	} else {
		b.edgesLeft = g.NumEdges() / 2
		b.siblingMap = slices.Clone(b.in.InEdgeIDs())
		g.MakeSiblingMap(b.siblingMap)
	}
	return b
}

func (b *polylineBuilder) isInterior(v VertexID) bool {
	if b.directed {
		return b.in.Degree(v) == 1 && b.out.Degree(v) == 1
	}
	return b.out.Degree(v) == 2
}

func (b *polylineBuilder) excessDegree(v VertexID) int {
	if b.directed {
		return b.out.Degree(v) - b.in.Degree(v)
	}
	return b.out.Degree(v) % 2
}

func (b *polylineBuilder) buildPaths() [][]EdgeID {
	var polylines [][]EdgeID
	edges := b.g.GetInputEdgeOrder(b.minInputIDs)
	for _, e := range edges {
		if !b.used[e] && !b.isInterior(b.g.edges[e].First) {
			polylines = append(polylines, b.buildPath(e))
		}
	}
	for _, e := range edges {
		if b.edgesLeft == 0 {
			break
		}
		if b.used[e] {
			continue
		}
		p := b.buildPath(e)
		CanonicalizeLoopOrder(b.minInputIDs, p)
		polylines = append(polylines, p)
	}
	CanonicalizeVectorOrder(b.minInputIDs, polylines)
	return polylines
}

func (b *polylineBuilder) buildPath(e EdgeID) []EdgeID {
	var polyline []EdgeID
	start := b.g.edges[e].First
	for {
		polyline = append(polyline, e)
		b.used[e] = true
		if !b.directed {
			b.used[b.siblingMap[e]] = true
		}
		b.edgesLeft--
		v := b.g.edges[e].Second
		if !b.isInterior(v) || v == start {
			break
		}
		lo, hi := b.out.EdgeIDs(v)
		if b.directed {
			e = lo
		} else {
			for e2 := lo; e2 < hi; e2++ {
				if !b.used[e2] {
					e = e2
				}
			}
		}
	}
	return polyline
}

func (b *polylineBuilder) buildWalks() [][]EdgeID {
	var polylines [][]EdgeID
	edges := b.g.GetInputEdgeOrder(b.minInputIDs)
	for _, e := range edges {
		if b.used[e] {
			continue
		}
		v := b.g.edges[e].First
		excess := b.excessDegree(v)
		if excess <= 0 {
			continue
		}
		excess -= b.excessUsed[v]
		if b.directed && excess <= 0 || !b.directed && excess%2 == 0 {
			continue
		}
		b.excessUsed[v]++
		polylines = append(polylines, b.buildWalk(v))
		last := polylines[len(polylines)-1]
		b.excessUsed[b.g.edges[last[len(last)-1]].Second]--
	}
	if b.edgesLeft > 0 {
		for i := range polylines {
			b.maximizeWalk(&polylines[i])
		}
	}
	for i := 0; i < len(edges) && b.edgesLeft > 0; i++ {
		e := edges[i]
		if b.used[e] {
			continue
		}
		v := b.g.edges[e].First
		id := b.minInputIDs[e]
		excess := 0
		for j := i; j < len(edges) && b.minInputIDs[edges[j]] == id; j++ {
			e2 := edges[j]
			if b.used[e2] {
				continue
			}
			if b.g.edges[e2].First == v {
				excess++
			}
			if b.g.edges[e2].Second == v {
				excess--
			}
		}
		if excess == 1 || b.g.edges[e].Second == v {
			p := b.buildWalk(v)
			b.maximizeWalk(&p)
			polylines = append(polylines, p)
		}
	}
	CanonicalizeVectorOrder(b.minInputIDs, polylines)
	return polylines
}

func (b *polylineBuilder) buildWalk(v VertexID) []EdgeID {
	var polyline []EdgeID
	for {
		bestEdge := EdgeID(-1)
		bestOutID := InputEdgeID(math.MaxInt32)
		lo, hi := b.out.EdgeIDs(v)
		for e := lo; e < hi; e++ {
			if b.used[e] || b.minInputIDs[e] >= bestOutID {
				continue
			}
			bestOutID = b.minInputIDs[e]
			bestEdge = e
		}
		if bestEdge < 0 {
			return polyline
		}
		excess := b.excessDegree(v) - b.excessUsed[v]
		if b.directed && excess < 0 || !b.directed && excess%2 == 1 {
			for _, e := range b.in.EdgeIDs(v) {
				if !b.used[e] && b.minInputIDs[e] <= bestOutID {
					return polyline
				}
			}
		}
		polyline = append(polyline, bestEdge)
		b.used[bestEdge] = true
		if !b.directed {
			b.used[b.siblingMap[bestEdge]] = true
		}
		b.edgesLeft--
		v = b.g.edges[bestEdge].Second
	}
}

func (b *polylineBuilder) maximizeWalk(polyline *[]EdgeID) {
	for i := 0; i <= len(*polyline); i++ {
		var v VertexID
		if i == 0 {
			v = b.g.edges[(*polyline)[i]].First
		} else {
			v = b.g.edges[(*polyline)[i-1]].Second
		}
		lo, hi := b.out.EdgeIDs(v)
		for e := lo; e < hi; e++ {
			if !b.used[e] {
				loop := b.buildWalk(v)
				p := *polyline
				*polyline = append(append(append([]EdgeID(nil), p[:i]...), loop...), p[i:]...)
				break
			}
		}
	}
}

// ProcessEdges is Graph::ProcessEdges: applies options' degenerate,
// duplicate and sibling-pair rules to edges (sorted on return).
func ProcessEdges(options *GraphOptions, edges *[]GraphEdge, inputIDs *[]InputEdgeIDSetID, idSetLexicon *IDSetLexicon) error {
	p := newEdgeProcessor(*options, edges, inputIDs, idSetLexicon)
	err := p.run()
	if options.SiblingPairs == SiblingPairsRequire || options.SiblingPairs == SiblingPairsCreate {
		options.EdgeType = EdgeTypeDirected
	}
	return err
}

// edgeProcessor is Graph::EdgeProcessor.
type edgeProcessor struct {
	options      GraphOptions
	edges        *[]GraphEdge
	inputIDs     *[]InputEdgeIDSetID
	idSetLexicon *IDSetLexicon
	outEdges     []EdgeID
	inEdges      []EdgeID
	newEdges     []GraphEdge
	newInputIDs  []InputEdgeIDSetID
}

func newEdgeProcessor(options GraphOptions, edges *[]GraphEdge, inputIDs *[]InputEdgeIDSetID, lex *IDSetLexicon) *edgeProcessor {
	p := &edgeProcessor{options: options, edges: edges, inputIDs: inputIDs, idSetLexicon: lex}
	es := *edges
	p.outEdges = iota32(len(es))
	sort.Slice(p.outEdges, func(i, j int) bool {
		a, b := p.outEdges[i], p.outEdges[j]
		return stableLessThan(es[a], es[b], a, b)
	})
	p.inEdges = iota32(len(es))
	sort.Slice(p.inEdges, func(i, j int) bool {
		a, b := p.inEdges[i], p.inEdges[j]
		return stableLessThan(es[a].Reverse(), es[b].Reverse(), a, b)
	})
	p.newEdges = make([]GraphEdge, 0, len(es))
	p.newInputIDs = make([]InputEdgeIDSetID, 0, len(es))
	return p
}

func (p *edgeProcessor) addEdge(e GraphEdge, id InputEdgeIDSetID) {
	p.newEdges = append(p.newEdges, e)
	p.newInputIDs = append(p.newInputIDs, id)
}

func (p *edgeProcessor) addEdges(n int, e GraphEdge, id InputEdgeIDSetID) {
	for i := 0; i < n; i++ {
		p.addEdge(e, id)
	}
}

func (p *edgeProcessor) copyEdges(outBegin, outEnd int) {
	for i := outBegin; i < outEnd; i++ {
		p.addEdge((*p.edges)[p.outEdges[i]], (*p.inputIDs)[p.outEdges[i]])
	}
}

func (p *edgeProcessor) mergeInputIDs(outBegin, outEnd int) InputEdgeIDSetID {
	if outEnd-outBegin == 1 {
		return (*p.inputIDs)[p.outEdges[outBegin]]
	}
	var tmp []int32
	for i := outBegin; i < outEnd; i++ {
		tmp = append(tmp, p.idSetLexicon.IDSet((*p.inputIDs)[p.outEdges[i]])...)
	}
	return p.idSetLexicon.Add(tmp...)
}

func (p *edgeProcessor) run() error {
	es := *p.edges
	numEdges := len(es)
	if numEdges == 0 {
		return nil
	}
	var err error
	o := p.options
	out, in := 0, 0
	sentinel := GraphEdge{math.MaxInt32, math.MaxInt32}
	outEdge := es[p.outEdges[out]]
	inEdge := es[p.inEdges[in]]
	for {
		edge := edgeMin(outEdge, inEdge.Reverse())
		if edge == sentinel {
			break
		}
		outBegin, inBegin := out, in
		for outEdge == edge {
			out++
			if out == numEdges {
				outEdge = sentinel
			} else {
				outEdge = es[p.outEdges[out]]
			}
		}
		for inEdge.Reverse() == edge {
			in++
			if in == numEdges {
				inEdge = sentinel
			} else {
				inEdge = es[p.inEdges[in]]
			}
		}
		nOut := out - outBegin
		nIn := in - inBegin
		if edge.First == edge.Second {
			if o.DegenerateEdges == DegenerateEdgesDiscard {
				continue
			}
			if o.DegenerateEdges == DegenerateEdgesDiscardExcess &&
				((outBegin > 0 && es[p.outEdges[outBegin-1]].First == edge.First) ||
					(out < numEdges && es[p.outEdges[out]].First == edge.First) ||
					(inBegin > 0 && es[p.inEdges[inBegin-1]].Second == edge.First) ||
					(in < numEdges && es[p.inEdges[in]].Second == edge.First)) {
				continue
			}
			merge := o.DuplicateEdges == DuplicateEdgesMerge || o.DegenerateEdges == DegenerateEdgesDiscardExcess
			if o.EdgeType == EdgeTypeUndirected && (o.SiblingPairs == SiblingPairsRequire || o.SiblingPairs == SiblingPairsCreate) {
				n := nOut / 2
				if merge {
					n = 1
				}
				p.addEdges(n, edge, p.mergeInputIDs(outBegin, out))
			} else if merge {
				n := 1
				if o.EdgeType == EdgeTypeUndirected {
					n = 2
				}
				p.addEdges(n, edge, p.mergeInputIDs(outBegin, out))
			} else if o.SiblingPairs == SiblingPairsDiscard || o.SiblingPairs == SiblingPairsDiscardExcess {
				p.addEdges(nOut, edge, p.mergeInputIDs(outBegin, out))
			} else {
				p.copyEdges(outBegin, out)
			}
		} else if o.SiblingPairs == SiblingPairsKeep {
			if nOut > 1 && o.DuplicateEdges == DuplicateEdgesMerge {
				p.addEdge(edge, p.mergeInputIDs(outBegin, out))
			} else {
				p.copyEdges(outBegin, out)
			}
		} else if o.SiblingPairs == SiblingPairsDiscard {
			if o.EdgeType == EdgeTypeDirected {
				if nOut <= nIn {
					continue
				}
				n := nOut - nIn
				if o.DuplicateEdges == DuplicateEdgesMerge {
					n = 1
				}
				p.addEdges(n, edge, p.mergeInputIDs(outBegin, out))
			} else {
				if nOut&1 == 0 {
					continue
				}
				p.addEdge(edge, p.mergeInputIDs(outBegin, out))
			}
		} else if o.SiblingPairs == SiblingPairsDiscardExcess {
			if o.EdgeType == EdgeTypeDirected {
				if nOut < nIn {
					continue
				}
				n := max(1, nOut-nIn)
				if o.DuplicateEdges == DuplicateEdgesMerge {
					n = 1
				}
				p.addEdges(n, edge, p.mergeInputIDs(outBegin, out))
			} else {
				n := 2
				if nOut&1 != 0 {
					n = 1
				}
				p.addEdges(n, edge, p.mergeInputIDs(outBegin, out))
			}
		} else {
			// REQUIRE or CREATE.
			if err == nil && o.SiblingPairs == SiblingPairsRequire &&
				((o.EdgeType == EdgeTypeDirected && nOut != nIn) || (o.EdgeType != EdgeTypeDirected && nOut&1 != 0)) {
				err = errBuilder("Expected all input edges to have siblings, but some were missing")
			}
			if o.DuplicateEdges == DuplicateEdgesMerge {
				p.addEdge(edge, p.mergeInputIDs(outBegin, out))
			} else if o.EdgeType == EdgeTypeUndirected {
				p.addEdges((nOut+1)/2, edge, p.mergeInputIDs(outBegin, out))
			} else {
				p.copyEdges(outBegin, out)
				if nIn > nOut {
					p.addEdges(nIn-nOut, edge, emptySetID)
				}
			}
		}
	}
	*p.edges = p.newEdges
	*p.inputIDs = p.newInputIDs
	return err
}

// FilterVertices is Graph::FilterVertices: the vertices edges use, with the
// edges renumbered to them.
func FilterVertices(vertices []s2.Point, edges *[]GraphEdge, tmp *[]VertexID) []s2.Point {
	used := make([]VertexID, 0, 2*len(*edges))
	for _, e := range *edges {
		used = append(used, e.First, e.Second)
	}
	slices.Sort(used)
	used = slices.Compact(used)
	if cap(*tmp) < len(vertices) {
		*tmp = make([]VertexID, len(vertices))
	}
	vmap := (*tmp)[:len(vertices)]
	*tmp = vmap
	newVertices := make([]s2.Point, len(used))
	for i, u := range used {
		newVertices[i] = vertices[u]
		vmap[u] = VertexID(i)
	}
	for i := range *edges {
		e := &(*edges)[i]
		e.First = vmap[e.First]
		e.Second = vmap[e.Second]
	}
	return newVertices
}

// MakeSubgraph is Graph::MakeSubgraph: a graph on the same vertices and
// labels with newEdges processed under newOptions.
func (g *Graph) MakeSubgraph(newOptions GraphOptions, newEdges *[]GraphEdge, newInputEdgeIDSetIDs *[]InputEdgeIDSetID, newInputEdgeIDSetLexicon *IDSetLexicon, isFullPolygonPredicate IsFullPolygonPredicate) (*Graph, error) {
	if g.options.EdgeType == EdgeTypeDirected && newOptions.EdgeType == EdgeTypeUndirected {
		n := len(*newEdges)
		for i := 0; i < n; i++ {
			*newEdges = append(*newEdges, (*newEdges)[i].Reverse())
			*newInputEdgeIDSetIDs = append(*newInputEdgeIDSetIDs, emptySetID)
		}
	}
	err := ProcessEdges(&newOptions, newEdges, newInputEdgeIDSetIDs, newInputEdgeIDSetLexicon)
	return NewGraph(newOptions, g.vertices, *newEdges, *newInputEdgeIDSetIDs, newInputEdgeIDSetLexicon, g.labelSetIDs, g.labelSetLexicon, isFullPolygonPredicate), err
}
