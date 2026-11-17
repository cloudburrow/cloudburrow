// Copyright 2020 Google Inc. All Rights Reserved.
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

// Ported from src/s2/s2winding_operation.cc (Eric Veach) and
// src/s2/s2builderutil_get_snapped_winding_delta.cc: the S2Builder layer
// S2BufferOperation hands its offset paths to.

import (
	"sort"

	"github.com/golang/geo/s2"
)

// WindingRule is S2WindingOperation::WindingRule.
type WindingRule int

const (
	WindingPositive WindingRule = iota
	WindingNegative
	WindingNonZero
	WindingOdd
)

// WindingOptions is S2WindingOperation::Options.
type WindingOptions struct {
	SnapFunction        s2.Snapper
	IncludeDegeneracies bool
}

// WindingOperation is S2WindingOperation.
type WindingOperation struct {
	options        WindingOptions
	builder        *Builder
	refInputEdgeID InputEdgeID
	refWindingIn   int
	rule           WindingRule
}

// NewWindingOperation is S2WindingOperation(result_layer, options).
func NewWindingOperation(resultLayer Layer, o WindingOptions) *WindingOperation {
	if o.SnapFunction == nil {
		o.SnapFunction = s2.NewIdentitySnapper(0)
	}
	op := &WindingOperation{options: o}
	bo := DefaultBuilderOptions()
	bo.SnapFunction = o.SnapFunction
	bo.SplitCrossingEdges = true
	op.builder = NewBuilder(bo)
	op.builder.StartLayer(&windingLayer{op: op, resultLayer: resultLayer})
	return op
}

// AddLoop is S2WindingOperation::AddLoop.
func (op *WindingOperation) AddLoop(loop []s2.Point) { op.builder.AddLoop(loop) }

// Build is S2WindingOperation::Build: refP's winding number is refWinding.
func (op *WindingOperation) Build(refP s2.Point, refWinding int, rule WindingRule) error {
	// The reference point must be an S2Builder input vertex in order to
	// determine how its winding number is affected by snapping.
	op.refInputEdgeID = InputEdgeID(op.builder.NumInputEdges())
	op.builder.AddPoint(refP)
	op.refWindingIn = refWinding
	op.rule = rule
	return op.builder.Build()
}

// windingOracle is WindingOracle: winding numbers after snapping.
type windingOracle struct {
	g          *Graph
	refP       s2.Point
	refWinding int
}

func newWindingOracle(refInputEdgeID InputEdgeID, refWindingIn int, builder *Builder, g *Graph) (*windingOracle, error) {
	refIn, _ := builder.InputEdge(refInputEdgeID)
	refV := FindFirstVertexID(refInputEdgeID, g)
	o := &windingOracle{g: g, refP: g.Vertex(refV)}
	d, err := GetSnappedWindingDelta(refIn, refV, nil, builder, g)
	if err != nil {
		return nil, err
	}
	o.refWinding = refWindingIn + d
	return o, nil
}

// windingNumber is GetWindingNumber. Upstream switches to an
// S2ShapeIndex after the first query for speed; counting every edge gives
// the same sums.
func (o *windingOracle) windingNumber(p s2.Point) int {
	w := o.refWinding
	for e := 0; e < o.g.NumEdges(); e++ {
		ed := o.g.Edge(EdgeID(e))
		w += SignedEdgeOrVertexCrossing(o.refP, p, o.g.Vertex(ed.First), o.g.Vertex(ed.Second))
	}
	o.refP = p
	o.refWinding = w
	return w
}

// windingLayer is WindingLayer.
type windingLayer struct {
	op                 *WindingOperation
	resultLayer        Layer
	resultEdges        []GraphEdge
	resultInputEdgeIDs []InputEdgeIDSetID
}

func (l *windingLayer) GraphOptions() GraphOptions {
	return GraphOptions{EdgeTypeDirected, DegenerateEdgesKeep, DuplicateEdgesKeep, SiblingPairsKeep, true}
}

func (l *windingLayer) Build(g *Graph) error {
	oracle, err := newWindingOracle(l.op.refInputEdgeID, l.op.refWindingIn, l.op.builder, g)
	if err != nil {
		return err
	}
	var newEdges []GraphEdge
	var newInputEdgeIDs []InputEdgeIDSetID
	lex := g.InputEdgeIDSetLexicon().clone()
	// Copy all of the edges except the reference edge.
	for e := 0; e < g.NumEdges(); e++ {
		if ids := g.InputEdgeIDs(EdgeID(e)); len(ids) > 0 && ids[0] == l.op.refInputEdgeID {
			continue
		}
		newEdges = append(newEdges, g.Edge(EdgeID(e)))
		newInputEdgeIDs = append(newInputEdgeIDs, g.InputEdgeIDSetID(EdgeID(e)))
	}
	newOptions := GraphOptions{EdgeTypeDirected, DegenerateEdgesDiscardExcess, DuplicateEdgesMerge, SiblingPairsCreate, true}
	newGraph, err := g.MakeSubgraph(newOptions, &newEdges, &newInputEdgeIDs, lex, nil)
	if err != nil {
		return err
	}
	if err := l.computeBoundary(newGraph, oracle); err != nil {
		return err
	}
	isFull := func(*Graph) (bool, error) { return l.matchesRule(oracle.refWinding), nil }
	resultLex := newGraph.InputEdgeIDSetLexicon().clone()
	resultGraph, err := newGraph.MakeSubgraph(l.resultLayer.GraphOptions(), &l.resultEdges, &l.resultInputEdgeIDs, resultLex, isFull)
	if err != nil {
		return err
	}
	return l.resultLayer.Build(resultGraph)
}

func (l *windingLayer) computeBoundary(g *Graph, oracle *windingOracle) error {
	siblingMap := g.GetSiblingMap()
	leftTurnMap, err := g.GetLeftTurnMap(siblingMap)
	if err != nil {
		return err
	}
	edgeWinding := make([]int, g.NumEdges())
	var frontier []EdgeID
	for eMin := 0; eMin < g.NumEdges(); eMin++ {
		if leftTurnMap[eMin] < 0 {
			continue // Already visited.
		}
		v0 := g.Edge(EdgeID(eMin)).Second
		e0 := l.containingLoopEdge(v0, EdgeID(eMin), g, leftTurnMap, siblingMap)
		edgeWinding[e0] = oracle.windingNumber(g.Vertex(v0))
		frontier = append(frontier, e0)
		for len(frontier) > 0 {
			e := frontier[len(frontier)-1]
			frontier = frontier[:len(frontier)-1]
			if leftTurnMap[e] < 0 {
				continue
			}
			winding := edgeWinding[e]
			for leftTurnMap[e] >= 0 {
				sibling := siblingMap[e]
				windingMinus := len(g.InputEdgeIDs(e))
				windingPlus := len(g.InputEdgeIDs(sibling))
				siblingWinding := winding - windingMinus + windingPlus
				if (l.matchesRule(winding) && !l.matchesRule(siblingWinding)) || l.matchesDegeneracy(winding, windingMinus, windingPlus) {
					l.resultEdges = append(l.resultEdges, g.Edge(e))
					l.resultInputEdgeIDs = append(l.resultInputEdgeIDs, g.InputEdgeIDSetID(e))
				}
				next := leftTurnMap[e]
				leftTurnMap[e] = -1
				if leftTurnMap[sibling] >= 0 {
					edgeWinding[sibling] = siblingWinding
					frontier = append(frontier, sibling)
				}
				e = next
			}
		}
	}
	return nil
}

// containingLoopEdge is GetContainingLoopEdge.
func (l *windingLayer) containingLoopEdge(v VertexID, start EdgeID, g *Graph, leftTurnMap, siblingMap []EdgeID) EdgeID {
	if g.Edge(start).First == v {
		return start
	}
	e0 := start
	for {
		e1 := leftTurnMap[e0]
		if g.Edge(e0).First == g.Edge(e1).Second ||
			s2.AngleContainsVertex(g.Vertex(g.Edge(e0).First), g.Vertex(v), g.Vertex(g.Edge(e1).Second)) {
			return e0
		}
		e0 = siblingMap[e1]
	}
}

func (l *windingLayer) matchesRule(w int) bool {
	switch l.op.rule {
	case WindingPositive:
		return w > 0
	case WindingNegative:
		return w < 0
	case WindingNonZero:
		return w != 0
	default:
		return w&1 != 0
	}
}

func (l *windingLayer) matchesDegeneracy(winding, windingMinus, windingPlus int) bool {
	if !l.op.options.IncludeDegeneracies {
		return false
	}
	if windingMinus != windingPlus {
		return false
	}
	if l.op.rule == WindingOdd {
		return windingPlus&1 != 0
	}
	return winding == 0
}

// clone copies a lexicon (IdSetLexicon's copy constructor).
func (l *IDSetLexicon) clone() *IDSetLexicon {
	c := NewIDSetLexicon()
	c.values = append(c.values, l.values...)
	for k, v := range l.index {
		c.index[k] = v
	}
	return c
}

// ---- s2builderutil_get_snapped_winding_delta.cc ----

// InputEdgeFilter is s2builderutil::InputEdgeFilter: input edges for
// which it returns true are ignored.
type InputEdgeFilter func(InputEdgeID) bool

type edgeSnap struct {
	input     [2]s2.Point
	vIn, vOut VertexID
}

// inputVertexEdgeMap is the btree_multimap<S2Point, EdgeSnap>: entries
// sorted by the input edge's first vertex, equal keys in insertion order.
type inputVertexEdgeMap struct{ entries []edgeSnap }

func (m *inputVertexEdgeMap) insert(s edgeSnap) {
	i := sort.Search(len(m.entries), func(i int) bool { return pointLess(s.input[0], m.entries[i].input[0]) })
	m.entries = append(m.entries, edgeSnap{})
	copy(m.entries[i+1:], m.entries[i:])
	m.entries[i] = s
}

func (m *inputVertexEdgeMap) erase(i int) { m.entries = append(m.entries[:i], m.entries[i+1:]...) }

func (m *inputVertexEdgeMap) find(p s2.Point) int {
	i := sort.Search(len(m.entries), func(i int) bool { return !pointLess(m.entries[i].input[0], p) })
	if i < len(m.entries) && m.entries[i].input[0] == p {
		return i
	}
	return -1
}

func buildChain(refV VertexID, g *Graph, m *inputVertexEdgeMap) (chainIn, chainOut []s2.Point, err error) {
	it := -1
	for i, snap := range m.entries {
		if snap.vIn >= 0 {
			chainOut = append(chainOut, g.Vertex(snap.vIn))
			it = i
			break
		}
	}
	if it < 0 {
		it = 0 // Pick an arbitrary edge to start a closed loop.
	}
	snap := m.entries[it]
	m.erase(it)
	chainIn = append(chainIn, snap.input[0], snap.input[1])
	chainOut = append(chainOut, g.Vertex(refV))
	if snap.vOut >= 0 {
		chainOut = append(chainOut, g.Vertex(snap.vOut))
		return chainIn, chainOut, nil
	}
	for chainIn[len(chainIn)-1] != chainIn[0] {
		i := m.find(chainIn[len(chainIn)-1])
		if i < 0 {
			return nil, nil, errBuilder("Input edges (after filtering) do not form loops")
		}
		snap := m.entries[i]
		m.erase(i)
		chainIn = append(chainIn, snap.input[1])
		if snap.vOut >= 0 {
			chainOut = append(chainOut, g.Vertex(snap.vOut))
			break
		}
	}
	return chainIn, chainOut, nil
}

// edgeWindingDelta is GetEdgeWindingDelta: the signed crossings of AB with
// the chain.
func edgeWindingDelta(a, b s2.Point, chain []s2.Point) int {
	delta := 0
	for i := 1; i < len(chain); i++ {
		delta += SignedEdgeOrVertexCrossing(a, b, chain[i-1], chain[i])
	}
	return delta
}

// connector is GetConnector.
func connector(b0, b1, b1Snapped s2.Point) s2.Point {
	if b1Snapped.Dot(b1.Vector) >= 0 {
		return b1
	}
	x := s2.Point{Vector: RobustCrossProd(b0, b1).Cross(b1Snapped.Vector).Normalize()}
	if x.Dot(s2.Interpolate(0.5, b0, b1).Vector) >= 0 {
		return x
	}
	return s2.Point{Vector: x.Mul(-1)}
}

// GetSnappedWindingDelta is s2builderutil::GetSnappedWindingDelta (the
// overload that finds the incident edges by brute force): the change in
// winding number of refIn, an input vertex that snapped to refV.
func GetSnappedWindingDelta(refIn s2.Point, refV VertexID, filter InputEdgeFilter, builder *Builder, g *Graph) (int, error) {
	var incident []EdgeID
	for e := 0; e < g.NumEdges(); e++ {
		if ed := g.Edge(EdgeID(e)); ed.First == refV || ed.Second == refV {
			incident = append(incident, EdgeID(e))
		}
	}
	byInput := map[InputEdgeID]*edgeSnap{}
	var order []InputEdgeID
	for _, e := range incident {
		edge := g.Edge(e)
		for _, id := range g.InputEdgeIDs(e) {
			if filter != nil && filter(id) {
				continue
			}
			snap, ok := byInput[id]
			if !ok {
				snap = &edgeSnap{vIn: -1, vOut: -1}
				byInput[id] = snap
				order = append(order, id)
			}
			v0, v1 := builder.InputEdge(id)
			snap.input = [2]s2.Point{v0, v1}
			if edge.First != refV {
				snap.vIn = edge.First
			}
			if edge.Second != refV {
				snap.vOut = edge.Second
			}
		}
	}
	sort.Slice(order, func(i, j int) bool { return order[i] < order[j] })
	m := &inputVertexEdgeMap{}
	for _, id := range order {
		m.insert(*byInput[id])
	}
	refOut := g.Vertex(refV)
	delta := 0
	for len(m.entries) > 0 {
		chainIn, chainOut, err := buildChain(refV, g, m)
		if err != nil {
			return 0, err
		}
		if len(chainOut) == 1 {
			z := s2.Ortho(refOut)
			delta += 0 - edgeWindingDelta(z, refIn, chainIn)
			continue
		}
		n := len(chainIn)
		za := s2.Point{Vector: RobustCrossProd(chainIn[0], chainIn[1]).Normalize()}
		zb := s2.Point{Vector: RobustCrossProd(chainIn[n-2], chainIn[n-1]).Normalize()}
		if za.Dot(refOut.Vector) > 0 {
			za = s2.Point{Vector: za.Mul(-1)}
		}
		if zb.Dot(refOut.Vector) > 0 {
			zb = s2.Point{Vector: zb.Mul(-1)}
		}
		a0c := connector(chainIn[1], chainIn[0], chainOut[0])
		b1c := connector(chainIn[n-2], chainIn[n-1], chainOut[2])
		chainZ := []s2.Point{chainOut[0], chainOut[1], chainIn[1], chainIn[0], a0c, chainOut[0]}
		delta += edgeWindingDelta(za, zb, chainZ)
		diff := append([]s2.Point(nil), chainOut...)
		diff = append(diff, b1c)
		for i := n - 1; i >= 0; i-- {
			diff = append(diff, chainIn[i])
		}
		diff = append(diff, a0c, chainOut[0])
		delta += edgeWindingDelta(zb, refIn, diff)
		delta += edgeWindingDelta(refIn, refOut, chainOut)
	}
	return delta, nil
}

// FindFirstVertexID is s2builderutil::FindFirstVertexId: the output vertex
// the source of the given input edge snapped to, or -1.
func FindFirstVertexID(inputEdgeID InputEdgeID, g *Graph) VertexID {
	excess := map[VertexID]int{}
	for e := 0; e < g.NumEdges(); e++ {
		for _, id := range g.InputEdgeIDs(EdgeID(e)) {
			if id == inputEdgeID {
				excess[g.Edge(EdgeID(e)).First]++
				excess[g.Edge(EdgeID(e)).Second]--
				break
			}
		}
	}
	if len(excess) == 0 {
		return -1
	}
	keys := make([]VertexID, 0, len(excess))
	for v := range excess {
		keys = append(keys, v)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	for _, v := range keys {
		if excess[v] == 1 {
			return v
		}
	}
	return keys[0]
}
