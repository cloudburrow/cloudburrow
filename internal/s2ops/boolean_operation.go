// Copyright 2017 Google Inc. All Rights Reserved.
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

// A port of S2BooleanOperation, src/s2/s2boolean_operation.{h,cc} of
// https://github.com/google/s2geometry (commit
// 677c8cebcff11ba11accc0b02005bc214e06e9fd), by Eric Veach.
//
// Boolean operations are implemented by constructing the boundary of the
// result and then using S2Builder to assemble the edges. The boundary is
// obtained by clipping each of the two input regions to the interior or
// exterior of the other region. Whole input edges are passed to S2Builder,
// which resolves their intersections; the portions of each edge that do not
// belong to the result are then removed by EdgeClippingLayer, using the
// crossing information recorded here.
//
// Memory tracking (S2MemoryTracker) is not ported. Precision::SNAPPED,
// conservative_output and source_id_lexicon are not implemented upstream
// either.

import (
	"maps"
	"math"
	"slices"
	"sort"

	"github.com/golang/geo/s2"
)

// OpType is S2BooleanOperation::OpType.
type OpType int

// The supported operation types.
const (
	OpUnion               OpType = iota // Contained by either region.
	OpIntersection                      // Contained by both regions.
	OpDifference                        // Contained by the first region but not the second.
	OpSymmetricDifference               // Contained by one region but not the other.
)

// PolygonModel is S2BooleanOperation::PolygonModel: whether polygons are
// considered to contain their vertices and/or edges.
type PolygonModel int

const (
	// PolygonModelOpen: polygons do not contain their vertices or edges.
	PolygonModelOpen PolygonModel = iota
	// PolygonModelSemiOpen: polygon point containment is defined such that
	// if several polygons tile the region around a vertex, then exactly
	// one of those polygons contains that vertex. Similarly polygons
	// contain all of their edges, but none of their reversed edges.
	PolygonModelSemiOpen
	// PolygonModelClosed: polygons contain all their vertices, edges, and
	// reversed edges.
	PolygonModelClosed
)

// PolylineModel is S2BooleanOperation::PolylineModel: whether polylines are
// considered to contain their endpoints.
type PolylineModel int

const (
	// PolylineModelOpen: polylines do not contain their first or last
	// vertex.
	PolylineModelOpen PolylineModel = iota
	// PolylineModelSemiOpen: polylines contain all vertices except the last.
	PolylineModelSemiOpen
	// PolylineModelClosed: polylines contain all of their vertices.
	PolylineModelClosed
)

// BooleanOptions is S2BooleanOperation::Options.
type BooleanOptions struct {
	// SnapFunction is the function used for snap rounding the output
	// during the call to Build(). DEFAULT: IdentitySnapFunction(0).
	SnapFunction s2.Snapper
	// PolygonModel defines whether polygons contain their boundaries.
	// DEFAULT: PolygonModelSemiOpen.
	PolygonModel PolygonModel
	// PolylineModel defines whether polylines contain their vertices.
	// DEFAULT: PolylineModelClosed.
	PolylineModel PolylineModel
	// PolylineLoopsHaveBoundaries specifies whether a polyline loop is
	// considered to have a non-empty boundary. DEFAULT: true.
	PolylineLoopsHaveBoundaries bool
	// SplitAllCrossingPolylineEdges specifies that a new vertex should be
	// added whenever a polyline edge crosses another polyline edge.
	// DEFAULT: false.
	SplitAllCrossingPolylineEdges bool
}

// DefaultBooleanOptions is S2BooleanOperation::Options().
func DefaultBooleanOptions() BooleanOptions {
	return BooleanOptions{SnapFunction: s2.NewIdentitySnapper(0), PolygonModel: PolygonModelSemiOpen, PolylineModel: PolylineModelClosed, PolylineLoopsHaveBoundaries: true}
}

// String is S2BooleanOperation::OpTypeToString.
func (t OpType) String() string {
	switch t {
	case OpUnion:
		return "UNION"
	case OpIntersection:
		return "INTERSECTION"
	case OpDifference:
		return "DIFFERENCE"
	case OpSymmetricDifference:
		return "SYMMETRIC DIFFERENCE"
	}
	return "Unknown OpType"
}

// String is S2BooleanOperation::PolygonModelToString.
func (m PolygonModel) String() string {
	switch m {
	case PolygonModelOpen:
		return "OPEN"
	case PolygonModelSemiOpen:
		return "SEMI_OPEN"
	case PolygonModelClosed:
		return "CLOSED"
	}
	return "Unknown PolygonModel"
}

// String is S2BooleanOperation::PolylineModelToString.
func (m PolylineModel) String() string {
	switch m {
	case PolylineModelOpen:
		return "OPEN"
	case PolylineModelSemiOpen:
		return "SEMI_OPEN"
	case PolylineModelClosed:
		return "CLOSED"
	}
	return "Unknown PolylineModel"
}

// A collection of special InputEdgeIds that allow the GraphEdgeClipper state
// modifications to be inserted into the list of edge crossings.
const (
	kSetInside   InputEdgeID = -1
	kSetInvertB  InputEdgeID = -2
	kSetReverseA InputEdgeID = -3
)

// crossingInputEdge represents an input edge B that crosses some other input
// edge A. It stores the input edge id of edge B and also whether it crosses
// edge A from left to right (or vice versa).
type crossingInputEdge struct {
	inputID     InputEdgeID
	leftToRight bool
}

// inputEdgeCrossing is one element of InputEdgeCrossings.
type inputEdgeCrossing struct {
	first  InputEdgeID
	second crossingInputEdge
}

// crossingGraphEdge is CrossingGraphEdge.
type crossingGraphEdge struct {
	id       EdgeID
	aIndex   int
	outgoing bool
	dst      VertexID
}

// getInputEdgeChainOrder returns a vector of EdgeIds sorted by input edge id.
// When more than one output edge has the same input edge id (i.e., the input
// edge snapped to a chain of edges), the edges are sorted so that they form
// a directed edge chain.
func getInputEdgeChainOrder(g *Graph, inputIDs []InputEdgeID) []EdgeID {
	// First, sort the edges so that the edges corresponding to each input edge
	// are consecutive.
	order := g.GetInputEdgeOrder(inputIDs)

	// Now sort the group of edges corresponding to each input edge in edge
	// chain order (e.g. AB, BC, CD).
	type vEdge struct {
		v VertexID
		e EdgeID
	}
	var vmap []vEdge                         // Map from source vertex to edge id.
	indegree := make([]int, g.NumVertices()) // Restricted to current input edge.
	for begin, end := 0, 0; begin < len(order); begin = end {
		// Gather the edges that came from a single input edge.
		inputID := inputIDs[order[begin]]
		for end = begin; end < len(order); end++ {
			if inputIDs[order[end]] != inputID {
				break
			}
		}
		if end-begin == 1 {
			continue
		}

		// Build a map from the source vertex of each edge to its edge id,
		// and also compute the indegree at each vertex considering only the
		// edges that came from the current input edge.
		for i := begin; i < end; i++ {
			e := order[i]
			vmap = append(vmap, vEdge{g.Edge(e).First, e})
			indegree[g.Edge(e).Second]++
		}
		sort.Slice(vmap, func(i, j int) bool {
			if vmap[i].v != vmap[j].v {
				return vmap[i].v < vmap[j].v
			}
			return vmap[i].e < vmap[j].e
		})

		// Find the starting edge for building the edge chain.
		next := EdgeID(g.NumEdges())
		for i := begin; i < end; i++ {
			e := order[i]
			if indegree[g.Edge(e).First] == 0 {
				next = e
			}
		}
		// Build the edge chain.
		for i := begin; ; {
			order[i] = next
			v := g.Edge(next).Second
			indegree[v] = 0 // Clear as we go along.
			i++
			if i == end {
				break
			}
			k := sort.Search(len(vmap), func(j int) bool {
				return vmap[j].v > v || (vmap[j].v == v && vmap[j].e >= 0)
			})
			next = vmap[k].e
		}
		vmap = vmap[:0]
	}
	return order
}

// graphEdgeClipper is GraphEdgeClipper: given a set of clipping instructions
// encoded as a set of edge crossings, it determines which graph edges
// correspond to clipped portions of input edges and removes them.
type graphEdgeClipper struct {
	g               *Graph
	in              *VertexInMap
	out             *VertexOutMap
	inputDimensions []int8
	inputCrossings  []inputEdgeCrossing
	newEdges        *[]GraphEdge
	newInputEdgeIDs *[]InputEdgeIDSetID
	inputIDs        []InputEdgeID // every graph edge has exactly one input edge
	order           []EdgeID      // Graph edges sorted in input edge id order.
	rank            []int         // The rank of each graph edge within order.
}

func newGraphEdgeClipper(g *Graph, inputDimensions []int8, inputCrossings []inputEdgeCrossing, newEdges *[]GraphEdge, newInputEdgeIDs *[]InputEdgeIDSetID) *graphEdgeClipper {
	c := &graphEdgeClipper{
		g:               g,
		in:              NewVertexInMap(g),
		out:             NewVertexOutMap(g),
		inputDimensions: inputDimensions,
		inputCrossings:  inputCrossings,
		newEdges:        newEdges,
		newInputEdgeIDs: newInputEdgeIDs,
		inputIDs:        g.InputEdgeIDSetIDs(),
	}
	c.order = getInputEdgeChainOrder(g, c.inputIDs)
	c.rank = make([]int, len(c.order))
	for i, e := range c.order {
		c.rank[e] = i
	}
	return c
}

func (c *graphEdgeClipper) addEdge(edge GraphEdge, inputEdgeID InputEdgeID) {
	*c.newEdges = append(*c.newEdges, edge)
	*c.newInputEdgeIDs = append(*c.newInputEdgeIDs, inputEdgeID)
}

// run is GraphEdgeClipper::Run.
func (c *graphEdgeClipper) run() {
	var aVertices []VertexID
	var aNumCrossings []int
	var aIsolated []bool
	var bInputEdges []crossingInputEdge
	var bEdges [][]crossingGraphEdge

	inside := false
	invertB := false
	reverseA := false
	next := 0
	for i := 0; i < len(c.order); i++ {
		// For each input edge (the "A" input edge), gather all the input edges
		// that cross it (the "B" input edges).
		aInputID := c.inputIDs[c.order[i]]
		edge0 := c.g.Edge(c.order[i])
		bInputEdges = bInputEdges[:0]
		for ; next < len(c.inputCrossings); next++ {
			if c.inputCrossings[next].first != aInputID {
				break
			}
			s := c.inputCrossings[next].second
			switch {
			case s.inputID >= 0:
				bInputEdges = append(bInputEdges, s)
			case s.inputID == kSetInside:
				inside = s.leftToRight
			case s.inputID == kSetInvertB:
				invertB = s.leftToRight
			default: // kSetReverseA
				reverseA = s.leftToRight
			}
		}
		// Optimization for degenerate edges.
		// TODO(ericv): If the output layer for this edge dimension specifies
		// DegenerateEdges::DISCARD, then remove the edge here.
		if edge0.First == edge0.Second {
			if len(bInputEdges)&1 != 0 {
				inside = !inside
			}
			c.addEdge(edge0, aInputID)
			continue
		}
		// Optimization for the case where there are no crossings.
		if len(bInputEdges) == 0 {
			// In general the caller only passes edges that are part of the
			// output (i.e., we could emit edge0 unconditionally); polygon
			// edges that are not emitted are passed for polyline/polygon
			// operations.
			if inside {
				if reverseA {
					c.addEdge(edge0.Reverse(), aInputID)
				} else {
					c.addEdge(edge0, aInputID)
				}
			}
			continue
		}
		// Walk along the chain of snapped edges for input edge A, and at each
		// vertex collect all the incident edges that belong to one of the
		// crossing edge chains (the "B" input edges).
		aVertices = aVertices[:0]
		aVertices = append(aVertices, edge0.First)
		bEdges = bEdges[:0]
		for range bInputEdges {
			bEdges = append(bEdges, nil)
		}
		c.gatherIncidentEdges(aVertices, 0, bInputEdges, bEdges)
		for ; i < len(c.order) && c.inputIDs[c.order[i]] == aInputID; i++ {
			aVertices = append(aVertices, c.g.Edge(c.order[i]).Second)
			c.gatherIncidentEdges(aVertices, len(aVertices)-1, bInputEdges, bEdges)
		}
		i--

		// Now for each B edge chain, decide which vertex of the A chain it
		// crosses, and keep track of the number of signed crossings at each A
		// vertex.
		aNumCrossings = slices.Grow(aNumCrossings[:0], len(aVertices))[:len(aVertices)]
		clear(aNumCrossings)
		aIsolated = slices.Grow(aIsolated[:0], len(aVertices))[:len(aVertices)]
		clear(aIsolated)
		for bi := range bInputEdges {
			leftToRight := bInputEdges[bi].leftToRight
			aIndex := c.getCrossedVertexIndex(aVertices, bEdges[bi], leftToRight)
			if aIndex >= 0 {
				// Keep track of the number of signed crossings (see above).
				isLine := c.inputDimensions[bInputEdges[bi].inputID] == 1
				sign := 1
				if isLine {
					sign = 0
				} else if leftToRight == invertB {
					sign = -1
				}
				aNumCrossings[aIndex] += sign

				// Any polyline or polygon vertex that has at least one crossing
				// but no adjacent emitted edge may be emitted as an isolated
				// vertex.
				aIsolated[aIndex] = true
			}
			// else: TODO(b/112043775): fix this condition ("Failed to get
			// crossed vertex index."), logged as DFATAL upstream.
		}

		// Finally, we iterate through the A edge chain, keeping track of the
		// number of times the current point is inside the B polygon, and we
		// emit A edges whenever this number is greater than zero (or less
		// than zero, if reverseA is true).
		multiplicity := b2i(inside) + aNumCrossings[0]
		for ai := 1; ai < len(aVertices); ai++ {
			if multiplicity != 0 {
				aIsolated[ai-1] = false
				aIsolated[ai] = false
			}
			edgeCount := multiplicity
			if reverseA {
				edgeCount = -multiplicity
			}
			// Output any forward edges required.
			for k := 0; k < edgeCount; k++ {
				c.addEdge(GraphEdge{aVertices[ai-1], aVertices[ai]}, aInputID)
			}
			// Output any reverse edges required.
			for k := edgeCount; k < 0; k++ {
				c.addEdge(GraphEdge{aVertices[ai], aVertices[ai-1]}, aInputID)
			}
			multiplicity += aNumCrossings[ai]
		}
		// Multiplicities other than 0 or 1 can only occur in the edge interior.
		inside = multiplicity != 0

		// Output any isolated polyline vertices.
		// TODO(ericv): Only do this if an output layer wants degenerate edges.
		if c.inputDimensions[aInputID] != 0 {
			for ai := range aVertices {
				if aIsolated[ai] {
					c.addEdge(GraphEdge{aVertices[ai], aVertices[ai]}, aInputID)
				}
			}
		}
	}
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// gatherIncidentEdges is GraphEdgeClipper::GatherIncidentEdges: given the
// vertices of the snapped edge chain for an input edge A and the set of
// input edges B that cross input edge A, this method gathers all of the
// snapped edges of B that are incident to a given snapped vertex of A.
func (c *graphEdgeClipper) gatherIncidentEdges(a []VertexID, ai int, bInputEdges []crossingInputEdge, bEdges [][]crossingGraphEdge) {
	find := func(id InputEdgeID) int {
		k := sort.Search(len(bInputEdges), func(j int) bool { return bInputEdges[j].inputID >= id })
		if k < len(bInputEdges) && bInputEdges[k].inputID == id {
			return k
		}
		return -1
	}
	// Examine all of the edges incident to the given vertex of A. If any
	// edge comes from a B input edge, append it to the appropriate vector.
	for _, e := range c.in.EdgeIDs(a[ai]) {
		if k := find(c.inputIDs[e]); k >= 0 {
			bEdges[k] = append(bEdges[k], crossingGraphEdge{e, ai, false, c.g.Edge(e).First})
		}
	}
	begin, end := c.out.EdgeIDs(a[ai])
	for e := begin; e < end; e++ {
		if k := find(c.inputIDs[e]); k >= 0 {
			bEdges[k] = append(bEdges[k], crossingGraphEdge{e, ai, true, c.g.Edge(e).Second})
		}
	}
}

// getVertexRank returns the "vertex rank" of the shared vertex associated
// with the given CrossingGraphEdge. Recall that graph edges are sorted in
// input edge order, and that the rank of an edge is its position in this
// order (rank[e]). VertexRank(e) is defined such that VertexRank(e.src) ==
// rank[e] and VertexRank(e.dst) == rank[e] + 1.
func (c *graphEdgeClipper) getVertexRank(e crossingGraphEdge) int {
	return c.rank[e.id] + b2i(!e.outgoing)
}

// getCrossedVertexIndex is GraphEdgeClipper::GetCrossedVertexIndex: given
// an edge chain A that is crossed by another edge chain B (where
// "leftToRight" indicates whether B crosses A from left to right), this
// method decides which vertex of A the crossing takes place at. Returns -1
// on failure.
func (c *graphEdgeClipper) getCrossedVertexIndex(a []VertexID, b []crossingGraphEdge, leftToRight bool) int {
	if len(a) == 0 || len(b) == 0 {
		// Logged as DFATAL upstream.
		return -1
	}
	// TODO(b/316414071): Rework this algorithm.

	// The reason this calculation is tricky is that after snapping, the A and
	// B chains may meet and separate several times. The algorithm below finds
	// the range of vertex ranks acceptable for the crossing, then picks the
	// smallest vertex id in that range.

	// First, check whether edge A is degenerate.
	n := len(a)
	if n == 1 {
		return 0
	}

	// If edge chain B is incident to only one vertex of A, we're done.
	if b[0].aIndex == b[len(b)-1].aIndex {
		return b[0].aIndex
	}

	// Determine whether the B chain visits the first and last shared vertices
	// in reverse order.
	bReversed := c.getVertexRank(b[0]) > c.getVertexRank(b[len(b)-1])

	// Examine each incident B edge and use it to narrow the range of
	// positions where the crossing could occur in the B chain.
	lo, hi := -1, len(c.order)              // Vertex ranks of acceptable crossings
	bFirst, bLast := EdgeID(-1), EdgeID(-1) // "b" subchain connecting "a" endpoints
	for _, e := range b {
		ai := e.aIndex
		if ai == 0 {
			if e.outgoing != bReversed && e.dst != a[1] {
				bFirst = e.id
			}
		} else if ai == n-1 {
			if e.outgoing == bReversed && e.dst != a[n-2] {
				bLast = e.id
			}
		} else {
			// This B edge is incident to an interior vertex of the A chain.
			// First check whether this edge is identical (or reversed) to an
			// edge in the A chain, in which case it does not create any
			// restrictions.
			if e.dst == a[ai-1] || e.dst == a[ai+1] {
				continue
			}

			// Otherwise we can test which side of the A chain the edge lies on.
			onLeft := s2.OrderedCCW(c.g.Vertex(a[ai+1]), c.g.Vertex(e.dst), c.g.Vertex(a[ai-1]), c.g.Vertex(a[ai]))

			// Every B edge that is incident to an interior vertex of the A
			// chain restricts the crossing position, since it must be on the
			// "pre-crossing" or "post-crossing" side of the A chain.
			if leftToRight == onLeft {
				// This is a pre-crossing edge, so the crossing cannot be before
				// the destination vertex of this edge.
				lo = max(lo, c.rank[e.id]+1)
			} else {
				// This is a post-crossing edge, so the crossing cannot be after
				// the source vertex of this edge.
				hi = min(hi, c.rank[e.id])
			}
		}
	}
	// There is one special case. If a subchain of B connects the first and
	// last vertices of A, then together with the edges of A this forms a
	// loop whose orientation determines the side of A it is on.
	if bFirst >= 0 && bLast >= 0 {
		// Swap the edges if necessary so that they are in B chain order.
		if bReversed {
			bFirst, bLast = bLast, bFirst
		}

		// The B subchain connects the first and last vertices of A. Test
		// whether the chain includes any interior vertices of A. We do this
		// indirectly by testing whether any edge of B has restricted the
		// range of allowable crossing vertices.
		hasInteriorVertex := false
		for _, e := range b {
			if e.aIndex > 0 && e.aIndex < n-1 && c.rank[e.id] >= c.rank[bFirst] && c.rank[e.id] <= c.rank[bLast] {
				hasInteriorVertex = true
				break
			}
		}
		if !hasInteriorVertex {
			// The B subchain is not incident to any interior vertex of A.
			onLeft := c.edgeChainOnLeft(a, bFirst, bLast)
			if leftToRight == onLeft {
				lo = max(lo, c.rank[bLast]+1)
			} else {
				hi = min(hi, c.rank[bFirst])
			}
		}
	}

	// Otherwise we choose the smallest shared VertexId in the acceptable range,
	// in order to ensure that both chains choose the same crossing vertex.
	best := -1
	for _, e := range b {
		ai := e.aIndex
		vrank := c.getVertexRank(e)
		if vrank >= lo && vrank <= hi && (best < 0 || a[ai] < a[best]) {
			best = ai
		}
	}
	return best
}

// edgeChainOnLeft is GraphEdgeClipper::EdgeChainOnLeft: given edge chains A
// and B that form a loop (after possibly reversing the direction of chain
// B), returns true if chain B is to the left of chain A. Chain A is given
// as a sequence of vertices, while chain B is specified as the first and
// last edges of the chain.
func (c *graphEdgeClipper) edgeChainOnLeft(a []VertexID, bFirst, bLast EdgeID) bool {
	// TODO(b/317065708): Accumulate the angle instead of collecting vertices.

	// Gather all the interior vertices of the B subchain.
	var loop []VertexID
	for i := c.rank[bFirst]; i < c.rank[bLast]; i++ {
		loop = append(loop, c.g.Edge(c.order[i]).Second)
	}
	// Possibly reverse the chain so that it forms a loop when "a" is appended.
	if c.g.Edge(bLast).Second != a[0] {
		slices.Reverse(loop)
	}
	loop = append(loop, a...)
	// Duplicate the first two vertices to simplify vertex indexing.
	for j := 0; j < 2; j++ {
		loop = append(loop, loop[j])
	}
	// Now B is to the left of A if and only if the loop is counterclockwise.
	sum := 0.0
	for i := 2; i < len(loop); i++ {
		sum += turnAngle(c.g.Vertex(loop[i-2]), c.g.Vertex(loop[i-1]), c.g.Vertex(loop[i]))
	}
	return sum > 0
}

// edgeClippingLayer is EdgeClippingLayer: given a set of clipping
// instructions encoded as a set of intersections between input edges,
// determines which graph edges correspond to clipped portions of input
// edges and removes them. It assembles the remaining edges into a new
// Graph and passes the result to the given output layer(s) for assembly.
type edgeClippingLayer struct {
	layers          []Layer
	inputDimensions *[]int8
	inputCrossings  *[]inputEdgeCrossing
}

// GraphOptions keeps all edges, including degenerate ones, so that the
// correspondence between input edge crossings and output edge crossings can
// be figured out.
func (l *edgeClippingLayer) GraphOptions() GraphOptions {
	return GraphOptions{EdgeTypeDirected, DegenerateEdgesKeep, DuplicateEdgesKeep, SiblingPairsKeep, true}
}

func cloneLexicon(l *IDSetLexicon) *IDSetLexicon {
	if l == nil {
		return NewIDSetLexicon()
	}
	return &IDSetLexicon{values: slices.Clone(l.values), index: maps.Clone(l.index)}
}

func (l *edgeClippingLayer) Build(g *Graph) error {
	// The bulk of the work is handled by GraphEdgeClipper.
	var newEdges []GraphEdge
	var newInputEdgeIDs []InputEdgeIDSetID
	newGraphEdgeClipper(g, *l.inputDimensions, *l.inputCrossings, &newEdges, &newInputEdgeIDs).run()

	// Construct one or more subgraphs from the clipped edges and pass them to
	// the given output layer(s). We start with a copy of the input edge id
	// set lexicon because this is necessary in order to get the same input
	// edge ids in the subgraph(s) as in the original graph.
	lexicon := cloneLexicon(g.InputEdgeIDSetLexicon())
	var firstErr error
	note := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if len(l.layers) == 1 {
		ng, err := g.MakeSubgraph(l.layers[0].GraphOptions(), &newEdges, &newInputEdgeIDs, lexicon, g.IsFullPolygonPredicate())
		note(err)
		if ng != nil {
			note(l.layers[0].Build(ng))
		}
		return firstErr
	}
	// The Graph objects must be valid until the last Build() call completes.
	var layerEdges [3][]GraphEdge
	var layerInputEdgeIDs [3][]InputEdgeIDSetID
	// Separate the edges according to their dimension.
	for i := range newEdges {
		d := (*l.inputDimensions)[newInputEdgeIDs[i]]
		layerEdges[d] = append(layerEdges[d], newEdges[i])
		layerInputEdgeIDs[d] = append(layerInputEdgeIDs[d], newInputEdgeIDs[i])
	}
	for d := 0; d < 3; d++ {
		ng, err := g.MakeSubgraph(l.layers[d].GraphOptions(), &layerEdges[d], &layerInputEdgeIDs[d], lexicon, g.IsFullPolygonPredicate())
		note(err)
		if ng != nil {
			note(l.layers[d].Build(ng))
		}
	}
	return firstErr
}

// sourceID identifies an edge from one of the two input S2ShapeIndexes. It
// is S2BooleanOperation::SourceId.
type sourceID struct {
	regionID int32
	shapeID  int32
	edgeID   int32
}

func specialSourceID(specialEdgeID int32) sourceID { return sourceID{0, 0, specialEdgeID} }

// sourceEdgeCrossing represents an input edge that crosses some other edge;
// it crosses the edge from left to right iff the second element is true.
type sourceEdgeCrossing struct {
	id          sourceID
	leftToRight bool
}

// kSentinel is a sentinel value used to mark the end of vectors.
var kSentinel = s2.ShapeEdgeID{ShapeID: math.MaxInt32, EdgeID: 0}

// noShapeEdgeID is a default-constructed ShapeEdgeId (-1, -1).
var noShapeEdgeID = s2.ShapeEdgeID{ShapeID: -1, EdgeID: -1}

func shapeEdgeIDMin(a, b s2.ShapeEdgeID) s2.ShapeEdgeID {
	if b.Cmp(a) < 0 {
		return b
	}
	return a
}

// indexCrossing represents a pair of intersecting S2ShapeIndex edges.
type indexCrossing struct {
	a, b s2.ShapeEdgeID
	// True if S2::CrossingSign(a_edge, b_edge) > 0.
	isInteriorCrossing bool
	// True if "a_edge" crosses "b_edge" from left to right. Undefined if
	// isInteriorCrossing is false.
	leftToRight bool
	// Equal to S2::VertexCrossing(a_edge, b_edge). Undefined if "a_edge" and
	// "b_edge" do not share exactly one vertex or either edge is degenerate.
	isVertexCrossing bool
}

func indexCrossingLess(x, y indexCrossing) bool {
	if c := x.a.Cmp(y.a); c != 0 {
		return c < 0
	}
	return x.b.Cmp(y.b) < 0
}

// crossingIterator is a helper class for iterating through the edges from
// region B that cross a particular edge from region A. It caches
// information from the current shape, chain, and edge so that it doesn't
// need to be looked up repeatedly.
type crossingIterator struct {
	bIndex            *s2.ShapeIndex
	crossings         []indexCrossing
	pos               int
	bShape            s2.Shape
	bShapeID          int32
	bDimension        int
	bInfo             chainInfo // Computed on demand.
	crossingsComplete bool
}

// chainInfo is information about the chain to which an edge belongs.
type chainInfo struct {
	chainID int // chain id
	start   int // starting edge id
	limit   int // limit edge id
}

// newCrossingIterator creates an iterator over crossing edge pairs (a, b)
// where "b" is an edge from "bIndex". "crossingsComplete" indicates that
// "crossings" contains all edge crossings between the two regions (rather
// than a subset).
func newCrossingIterator(bIndex *s2.ShapeIndex, crossings []indexCrossing, crossingsComplete bool) *crossingIterator {
	it := &crossingIterator{bIndex: bIndex, crossings: crossings, bShapeID: -1, crossingsComplete: crossingsComplete}
	it.update()
	return it
}

func (it *crossingIterator) next() {
	it.pos++
	it.update()
}

func (it *crossingIterator) done(id s2.ShapeEdgeID) bool { return it.aID() != id }

// isInteriorCrossing is true if this crossing occurs at a point interior to
// both edges.
func (it *crossingIterator) isInteriorCrossing() bool { return it.crossings[it.pos].isInteriorCrossing }

// isVertexCrossing is equal to S2::VertexCrossing(a_edge, b_edge), provided
// that a_edge and b_edge have exactly one vertex in common and neither edge
// is degenerate.
func (it *crossingIterator) isVertexCrossing() bool { return it.crossings[it.pos].isVertexCrossing }

// leftToRight is true if a_edge crosses b_edge from left to right (for
// interior crossings).
func (it *crossingIterator) leftToRight() bool { return it.crossings[it.pos].leftToRight }

func (it *crossingIterator) aID() s2.ShapeEdgeID { return it.crossings[it.pos].a }
func (it *crossingIterator) bID() s2.ShapeEdgeID { return it.crossings[it.pos].b }
func (it *crossingIterator) bEdgeID() int        { return int(it.bID().EdgeID) }
func (it *crossingIterator) bEdge() s2.Edge      { return it.bShape.Edge(it.bEdgeID()) }

// bChainInfo returns a description of the chain to which the current B
// edge belongs.
func (it *crossingIterator) bChainInfo() chainInfo {
	if it.bInfo.chainID < 0 {
		it.bInfo.chainID = chainPosition(it.bShape, it.bEdgeID()).ChainID
		chain := it.bShape.Chain(it.bInfo.chainID)
		it.bInfo.start = chain.Start
		it.bInfo.limit = chain.Start + chain.Length
	}
	return it.bInfo
}

// update updates information about the B shape whenever it changes.
func (it *crossingIterator) update() {
	if it.aID() != kSentinel && it.bID().ShapeID != it.bShapeID {
		it.bShapeID = it.bID().ShapeID
		it.bShape = it.bIndex.Shape(it.bShapeID)
		it.bDimension = it.bShape.Dimension()
		it.bInfo.chainID = -1 // Computed on demand.
	}
}

// pointCrossingResult describes the relationship between a point from region
// A and a set of crossing edges from region B. For example, "matchesPolygon"
// indicates whether a polygon vertex from region B matches the given point.
type pointCrossingResult struct {
	// Note that "matchesPolyline" is true only if the point matches a
	// polyline vertex of B *and* the polyline contains that vertex, whereas
	// "matchesPolygon" is true if the point matches any polygon vertex.
	matchesPoint    bool // Matches point.
	matchesPolyline bool // Matches contained polyline vertex.
	matchesPolygon  bool // Matches polygon vertex.
}

// edgeCrossingResult describes the relationship between an edge (a0, a1)
// from region A and a set of crossing edges from region B.
type edgeCrossingResult struct {
	// These fields indicate that (a0, a1) exactly matches an edge of B.
	matchesPolyline bool // Matches polyline edge (either direction).

	// These fields indicate that a B polyline contains the degenerate
	// polyline (a0, a0) or (a1, a1).
	a0MatchesPolyline bool // B polyline contains (a0, a0)
	a1MatchesPolyline bool // B polyline contains (a1, a1)

	// These fields indicate that a vertex of (a0, a1) matches a polygon
	// vertex of B.
	a0MatchesPolygon bool // a0 matches polygon vertex.
	a1MatchesPolygon bool // a1 matches polygon vertex.

	// When a pair of polygons share an edge in the same or reverse
	// direction, these fields identify the matching B edge.
	polygonMatchID s2.ShapeEdgeID // B polygon edge that matches (a0, a1).
	siblingMatchID s2.ShapeEdgeID // B polygon edge that matches (a1, a0).
	a0LoopMatchID  s2.ShapeEdgeID // B polygon edge that matches (a0, a0).

	// These fields count the number of edge crossings at a0, a1, and the
	// interior of (a0, a1).
	a0Crossings       int // Count of polygon crossings at a0.
	a1Crossings       int // Count of polygon crossings at a1.
	interiorCrossings int // Count of polygon crossings in edge interior.
}

func newEdgeCrossingResult() edgeCrossingResult {
	return edgeCrossingResult{polygonMatchID: noShapeEdgeID, siblingMatchID: noShapeEdgeID, a0LoopMatchID: noShapeEdgeID}
}

// Convenience functions to test whether a matching edge was found.
func (r *edgeCrossingResult) matchesPolygon() bool { return r.polygonMatchID.EdgeID >= 0 }
func (r *edgeCrossingResult) matchesSibling() bool { return r.siblingMatchID.EdgeID >= 0 }
func (r *edgeCrossingResult) loopMatchesA0() bool  { return r.a0LoopMatchID.EdgeID >= 0 }

// crossingProcessor is a helper class that processes all the edges from one
// region that cross a specific edge of the other region. It outputs the
// appropriate edges to an S2Builder, and outputs other information required
// by GraphEdgeClipper to the given vectors.
type crossingProcessor struct {
	// Constructor parameters:
	polygonModel                PolygonModel
	polylineModel               PolylineModel
	polylineLoopsHaveBoundaries bool

	// The output of the crossingProcessor consists of a subset of the input
	// edges that are emitted to "builder", and some auxiliary information
	// that allows GraphEdgeClipper to determine which segments of those
	// input edges belong to the output.
	builder         *Builder // (nil if boolean output was requested)
	inputDimensions *[]int8
	inputCrossings  *[]inputEdgeCrossing

	// Fields set by startBoundary:
	aRegionID, bRegionID           int32
	invertA, invertB, invertResult bool
	isUnion                        bool // True if this is a UNION operation.

	// Fields set by startShape:
	aShape     s2.Shape
	aDimension int

	// Fields set by startChain:
	chainID    int
	chainStart int
	chainLimit int

	// Fields updated by processEdge:

	// A temporary representation of inputCrossings that is used internally
	// until all necessary edges from *both* polygons have been emitted to the
	// S2Builder.
	sourceEdgeCrossings []sourceInputCrossing

	// A set of edges that cross the current edge being processed by
	// processEdge() but that have not yet been associated with a particular
	// S2Builder edge.
	pendingSourceEdgeCrossings []sourceEdgeCrossing

	// A map that translates from sourceID (the edge id of an edge from one
	// of the two input regions) to InputEdgeId (the edge id of an edge in
	// the S2Builder input).
	sourceIDMap map[sourceID]InputEdgeID

	// For each edge in region B that defines a degenerate loop (either a
	// point loop or a sibling pair), indicates whether that loop represents
	// a shell or a hole.
	isDegenerateHole map[s2.ShapeEdgeID]bool

	// Indicates whether the point being processed along the current edge
	// chain is in the polygonal interior of the opposite region.
	inside bool

	// The value that "inside" would have just before the end of the
	// previous edge added to S2Builder.
	prevInside bool

	// The maximum edge id of any edge in the current chain whose v0 vertex
	// has already been emitted.
	v0EmittedMaxEdgeID int

	// True if the first vertex of the current chain has been emitted.
	chainV0Emitted bool
}

type sourceInputCrossing struct {
	first  InputEdgeID
	second sourceEdgeCrossing
}

func newCrossingProcessor(polygonModel PolygonModel, polylineModel PolylineModel, polylineLoopsHaveBoundaries bool, builder *Builder, inputDimensions *[]int8, inputCrossings *[]inputEdgeCrossing) *crossingProcessor {
	return &crossingProcessor{
		polygonModel:                polygonModel,
		polylineModel:               polylineModel,
		polylineLoopsHaveBoundaries: polylineLoopsHaveBoundaries,
		builder:                     builder,
		inputDimensions:             inputDimensions,
		inputCrossings:              inputCrossings,
		sourceIDMap:                 map[sourceID]InputEdgeID{},
		isDegenerateHole:            map[s2.ShapeEdgeID]bool{},
	}
}

func (cp *crossingProcessor) inputEdgeID() InputEdgeID { return InputEdgeID(len(*cp.inputDimensions)) }

// isV0Isolated returns true if the edges on either side of the first vertex
// of the current edge have not been emitted.
func (cp *crossingProcessor) isV0Isolated(aID s2.ShapeEdgeID) bool {
	return !cp.inside && cp.v0EmittedMaxEdgeID < int(aID.EdgeID)
}

// isChainLastVertexIsolated returns true if "aID" is the last edge of the
// current chain, and the edges on either side of the last vertex have not
// been emitted (including the possibility that the chain forms a loop).
func (cp *crossingProcessor) isChainLastVertexIsolated(aID s2.ShapeEdgeID) bool {
	return int(aID.EdgeID) == cp.chainLimit-1 && !cp.chainV0Emitted && cp.v0EmittedMaxEdgeID <= int(aID.EdgeID)
}

// polylineContainsV0 returns true if the given polyline edge contains "v0",
// taking into account the specified PolylineModel.
func (cp *crossingProcessor) polylineContainsV0(edgeID, chainStart int) bool {
	return cp.polylineModel != PolylineModelOpen || edgeID > chainStart
}

func (cp *crossingProcessor) isDegenerate(aID s2.ShapeEdgeID) bool {
	_, ok := cp.isDegenerateHole[aID]
	return ok
}

func (cp *crossingProcessor) addCrossing(c sourceEdgeCrossing) {
	cp.sourceEdgeCrossings = append(cp.sourceEdgeCrossings, sourceInputCrossing{cp.inputEdgeID(), c})
}

func (cp *crossingProcessor) addInteriorCrossing(c sourceEdgeCrossing) {
	// Crossing edges are queued until the S2Builder edge that they are
	// supposed to be associated with is created (see addEdge() and
	// processPointCrossings() for details).
	cp.pendingSourceEdgeCrossings = append(cp.pendingSourceEdgeCrossings, c)
}

func (cp *crossingProcessor) setClippingState(parameter InputEdgeID, state bool) {
	cp.addCrossing(sourceEdgeCrossing{specialSourceID(parameter), state})
}

// addEdge supports "early exit" in the case of boolean results by returning
// false as soon as the result is known to be non-empty.
func (cp *crossingProcessor) addEdge(aID s2.ShapeEdgeID, a s2.Edge, dimension int, interiorCrossings int) bool {
	if cp.builder == nil {
		return false // Boolean output.
	}
	if interiorCrossings > 0 {
		// Add the edges that cross this edge to the output so that
		// GraphEdgeClipper can find them.
		for _, c := range cp.pendingSourceEdgeCrossings {
			cp.sourceEdgeCrossings = append(cp.sourceEdgeCrossings, sourceInputCrossing{cp.inputEdgeID(), c})
		}
		// Build a map that translates temporary edge ids (sourceID) to the
		// representation used by EdgeClippingLayer (InputEdgeId).
		cp.sourceIDMap[sourceID{cp.aRegionID, aID.ShapeID, aID.EdgeID}] = cp.inputEdgeID()
	}
	// Set the GraphEdgeClipper's "inside" state to match ours.
	if cp.inside != cp.prevInside {
		cp.setClippingState(kSetInside, cp.inside)
	}
	*cp.inputDimensions = append(*cp.inputDimensions, int8(dimension))
	cp.builder.AddEdge(a.V0, a.V1)
	if interiorCrossings&1 != 0 {
		cp.inside = !cp.inside
	}
	cp.prevInside = cp.inside
	return true
}

// addPointEdge supports "early exit" in the case of boolean results by
// returning false as soon as the result is known to be non-empty.
func (cp *crossingProcessor) addPointEdge(p s2.Point, dimension int) bool {
	if cp.builder == nil {
		return false // Boolean output.
	}
	if !cp.prevInside {
		cp.setClippingState(kSetInside, true)
	}
	*cp.inputDimensions = append(*cp.inputDimensions, int8(dimension))
	cp.builder.AddEdge(p, p)
	cp.prevInside = true
	return true
}

// startBoundary starts processing edges from the given region. "invertA",
// "invertB", and "invertResult" indicate whether region A, region B, and/or
// the result should be inverted, which allows operations such as union and
// difference to be implemented. For example, union is ~(~A & ~B).
//
// This method should be called in pairs, once to process the edges from
// region A and once to process the edges from region B.
func (cp *crossingProcessor) startBoundary(aRegionID int32, invertA, invertB, invertResult bool) {
	cp.aRegionID = aRegionID
	cp.bRegionID = 1 - aRegionID
	cp.invertA = invertA
	cp.invertB = invertB
	cp.invertResult = invertResult
	cp.isUnion = invertB && invertResult

	// Specify to GraphEdgeClipper how these edges should be clipped.
	cp.setClippingState(kSetReverseA, invertA != invertResult)
	cp.setClippingState(kSetInvertB, invertB)
}

// startShape starts processing edges from the given shape.
func (cp *crossingProcessor) startShape(aShape s2.Shape) {
	cp.aShape = aShape
	cp.aDimension = aShape.Dimension()
}

// startChain starts processing edges from the given chain.
func (cp *crossingProcessor) startChain(chainID int, chain s2.Chain, inside bool) {
	cp.chainID = chainID
	cp.chainStart = chain.Start
	cp.chainLimit = chain.Start + chain.Length
	cp.inside = inside
	cp.v0EmittedMaxEdgeID = chain.Start - 1 // No edges emitted yet.
	cp.chainV0Emitted = false
}

// processEdge processes the given edge "aID". "it" should be positioned to
// the set of edges from the other region that cross "aID" (if any).
//
// Supports "early exit" in the case of boolean results by returning false
// as soon as the result is known to be non-empty.
func (cp *crossingProcessor) processEdge(aID s2.ShapeEdgeID, it *crossingIterator) bool {
	// chain_edge() is faster than edge() when there are multiple chains.
	a := chainEdge(cp.aShape, cp.chainID, int(aID.EdgeID)-cp.chainStart)
	switch cp.aDimension {
	case 0:
		return cp.processEdge0(aID, a, it)
	case 1:
		return cp.processEdge1(aID, a, it)
	default:
		return cp.processEdge2(aID, a, it)
	}
}

// processEdge0 processes an edge of dimension 0 (i.e., a point) from region
// A.
func (cp *crossingProcessor) processEdge0(aID s2.ShapeEdgeID, a s2.Edge, it *crossingIterator) bool {
	// When a region is inverted, all points and polylines are discarded.
	if cp.invertA != cp.invertResult {
		cp.skipCrossings(aID, it)
		return true
	}
	r := cp.processPointCrossings(aID, a.V0, it)

	// "contained" indicates whether the current point is inside the
	// polygonal interior of the opposite region, using semi-open boundaries.
	contained := cp.inside != cp.invertB
	if r.matchesPolygon && cp.polygonModel != PolygonModelSemiOpen {
		contained = cp.polygonModel == PolygonModelClosed
	}
	if r.matchesPolyline {
		contained = true
	}

	// The output of UNION includes duplicate values, so ensure that points
	// are not suppressed by other points.
	if r.matchesPoint && !cp.isUnion {
		contained = true
	}

	// Test whether the point is contained after region B is inverted.
	if contained == cp.invertB {
		return true // Don't exit early.
	}
	return cp.addPointEdge(a.V0, 0)
}

// processPointCrossings returns a summary of the relationship between a
// point from region A and a set of crossing edges from region B (see
// pointCrossingResult).
func (cp *crossingProcessor) processPointCrossings(aID s2.ShapeEdgeID, a0 s2.Point, it *crossingIterator) pointCrossingResult {
	var r pointCrossingResult
	for ; !it.done(aID); it.next() {
		switch it.bDimension {
		case 0:
			r.matchesPoint = true
		case 1:
			if cp.polylineEdgeContainsVertex(a0, it, 0) {
				r.matchesPolyline = true
			}
		default:
			r.matchesPolygon = true
		}
	}
	return r
}

// processEdge1 processes an edge of dimension 1 (i.e., a polyline edge) from
// region A.
func (cp *crossingProcessor) processEdge1(aID s2.ShapeEdgeID, a s2.Edge, it *crossingIterator) bool {
	// When a region is inverted, all points and polylines are discarded.
	if cp.invertA != cp.invertResult {
		cp.skipCrossings(aID, it)
		return true
	}
	// Evaluate whether the start vertex should belong to the output, in case
	// it needs to be emitted as an isolated vertex.
	r := cp.processEdgeCrossings(aID, a, it)
	a0Inside := cp.isPolylineVertexInside(r.a0MatchesPolyline, r.a0MatchesPolygon)

	// Test whether the entire polyline edge should be emitted (or not
	// emitted) because it matches a polyline or polygon edge.
	isDegenerate := a.V0 == a.V1
	if r.a0Crossings&1 != 0 {
		cp.inside = !cp.inside
	}
	if cp.inside != cp.isPolylineEdgeInside(&r, isDegenerate) {
		cp.inside = !cp.inside // Invert the inside state.
		r.a1Crossings++        // Restore the correct (semi-open) state later.
	}

	// If neither edge adjacent to v0 was emitted, and this polyline contains
	// v0, and the other region contains v0, then emit an isolated vertex.
	if !cp.polylineLoopsHaveBoundaries && int(aID.EdgeID) == cp.chainStart &&
		a.V0 == chainEdge(cp.aShape, cp.chainID, cp.chainLimit-cp.chainStart-1).V1 {
		// This is the first vertex of a polyline loop, so we can't decide if
		// it is isolated until we process the last polyline edge.
		cp.chainV0Emitted = cp.inside
	} else if cp.isV0Isolated(aID) && !isDegenerate &&
		cp.polylineContainsV0(int(aID.EdgeID), cp.chainStart) && a0Inside {
		if !cp.addPointEdge(a.V0, 1) {
			return false
		}
	}

	// Test whether the entire edge or any part of it belongs to the output.
	if cp.inside || r.interiorCrossings > 0 {
		// Note: updates "inside" to correspond to the state just before a1.
		if !cp.addEdge(aID, a, 1 /*dimension*/, r.interiorCrossings) {
			return false
		}
	}
	// Remember whether the edge portion just before "a1" was emitted, so that
	// we can decide whether "a1" need to be emitted as an isolated vertex.
	if cp.inside {
		cp.v0EmittedMaxEdgeID = int(aID.EdgeID) + 1
	}

	// Verify that edge crossings are being counted correctly.
	if r.a1Crossings&1 != 0 {
		cp.inside = !cp.inside
	}

	// Special case to test whether the last vertex of a polyline should be
	// emitted as an isolated vertex.
	if it.crossingsComplete && !isDegenerate && cp.isChainLastVertexIsolated(aID) &&
		(cp.polylineModel == PolylineModelClosed ||
			(!cp.polylineLoopsHaveBoundaries && a.V1 == chainEdge(cp.aShape, cp.chainID, cp.chainStart).V0)) &&
		cp.isPolylineVertexInside(r.a1MatchesPolyline, r.a1MatchesPolygon) {
		if !cp.addPointEdge(a.V1, 1) {
			return false
		}
	}
	return true
}

// isPolylineVertexInside returns true if the current point being processed
// (which must be a polyline vertex) is contained by the opposite region
// (after inversion if "invertB" is true). "matchesPolyline" and
// "matchesPolygon" indicate whether the vertex matches a polyline/polygon
// vertex of the opposite region.
func (cp *crossingProcessor) isPolylineVertexInside(matchesPolyline, matchesPolygon bool) bool {
	// Initially "contained" indicates whether the current point is inside the
	// polygonal interior of region B using semi-open boundaries.
	contained := cp.inside != cp.invertB

	// For UNION the output includes duplicate polylines. The test below
	// ensures that isolated polyline vertices are not suppressed by other
	// polyline vertices in the output.
	if matchesPolyline && !cp.isUnion {
		contained = true
	} else if matchesPolygon && cp.polygonModel != PolygonModelSemiOpen {
		contained = cp.polygonModel == PolygonModelClosed
	}
	// Finally, invert the result if the opposite region should be inverted.
	return contained != cp.invertB
}

// isPolylineEdgeInside returns true if the current polyline edge is
// contained by the opposite region (after inversion if "invertB" is true).
func (cp *crossingProcessor) isPolylineEdgeInside(r *edgeCrossingResult, isDegenerate bool) bool {
	// Initially "contained" indicates whether the current point (just past
	// a0) is inside the polygonal interior of region B using semi-open
	// boundaries.
	contained := cp.inside != cp.invertB

	// Note that if r.matchesPolyline and isUnion is true, then "contained"
	// will be false (unless there is also a matching polygon edge) since
	// polyline edges are not allowed in the interior of B. In this case we
	// leave "contained" as false since it causes both matching edges to be
	// emitted.
	if r.matchesPolyline && !cp.isUnion {
		contained = true
	} else if isDegenerate {
		// First allow the polygon boundary model to override the semi-open
		// rules. Note that a polygon vertex (dimension 2) is considered to
		// completely contain degenerate OPEN and SEMI_OPEN polylines
		// (dimension 1) even though the latter do not contain any points.
		// This is because dimension 2 points are considered to be a strict
		// superset of dimension 1 points.
		if cp.polygonModel != PolygonModelSemiOpen && r.a0MatchesPolygon {
			contained = cp.polygonModel == PolygonModelClosed
		}
		// Note that r.a0MatchesPolyline is true if and only if some B
		// polyline contains the degenerate polyline (a0, a0).
		if r.a0MatchesPolyline && !cp.isUnion {
			contained = true
		}
	} else if r.matchesPolygon() {
		// In the SEMI_OPEN model, polygon sibling pairs cancel each other and
		// have no effect on point or edge containment.
		if !(cp.polygonModel == PolygonModelSemiOpen && r.matchesSibling()) {
			contained = cp.polygonModel != PolygonModelOpen
		}
	} else if r.matchesSibling() {
		contained = cp.polygonModel == PolygonModelClosed
	}
	// Finally, invert the result if the opposite region should be inverted.
	return contained != cp.invertB
}

// processEdge2 processes an edge of dimension 2 (i.e., a polygon edge) from
// region A.
func (cp *crossingProcessor) processEdge2(aID s2.ShapeEdgeID, a s2.Edge, it *crossingIterator) bool {
	// In order to keep only one copy of any shared polygon edges, we only
	// output shared edges when processing the second region.
	emitShared := cp.aRegionID == 1

	// Degeneracies such as isolated vertices and sibling pairs can only be
	// created by intersecting CLOSED polygons or unioning OPEN polygons.
	createDegen := (cp.polygonModel == PolygonModelClosed && !cp.invertA && !cp.invertB) ||
		(cp.polygonModel == PolygonModelOpen && cp.invertA && cp.invertB)

	// In addition, existing degeneracies are kept when an open boundary is
	// subtracted. Note that "keepDegenB" is only defined for completeness.
	// It is needed to ensure that the "reverse subtraction operator" (B - A)
	// preserves degeneracies correctly, however in practice this operator is
	// only used internally to implement symmetric difference, and in that
	// situation the preserved degeneracy is always removed from the final
	// result because it overlaps other geometry.
	keepDegenA := cp.polygonModel == PolygonModelOpen && cp.invertB
	keepDegenB := cp.polygonModel == PolygonModelOpen && cp.invertA

	r := cp.processEdgeCrossings(aID, a, it)

	// If only one region is inverted, matching/sibling relations are
	// reversed.
	if cp.invertA != cp.invertB {
		r.polygonMatchID, r.siblingMatchID = r.siblingMatchID, r.polygonMatchID
	}

	isPoint := a.V0 == a.V1
	if !emitShared {
		// Remember the shell/hole status of each degenerate edge of region B
		// that is incident to A.
		if r.loopMatchesA0() {
			cp.isDegenerateHole[r.a0LoopMatchID] = cp.inside
			if isPoint {
				return true
			}
		}

		// Point loops are handled in the first pass for non-semi-open models.
		if cp.polygonModel != PolygonModelSemiOpen {
			if isPoint && r.a0MatchesPolygon {
				return true
			}
		}
	}
	if r.a0Crossings&1 != 0 {
		cp.inside = !cp.inside
	}
	if !emitShared {
		// Defer processing A edges that match or are siblings to an edge of B
		// until the second pass.
		if r.matchesPolygon() || r.matchesSibling() {
			// For sibling pairs, also remember their shell/hole status.
			if r.matchesPolygon() && r.matchesSibling() {
				cp.isDegenerateHole[r.polygonMatchID] = cp.inside
				cp.isDegenerateHole[r.siblingMatchID] = cp.inside
			}
			if r.a1Crossings&1 != 0 {
				cp.inside = !cp.inside
			}
			return true
		}
	}

	// Remember whether the B geometry represents a sibling pair hole.
	isBHole := r.matchesPolygon() && r.matchesSibling() && cp.inside

	// At this point, "inside" indicates whether the initial part of the A
	// edge is contained by the B geometry using semi-open rules. The
	// following code implements the various other polygon boundary rules by
	// changing the value of "inside" when necessary to indicate whether the
	// current A edge should be emitted to the output or not.
	semiOpenInside := cp.inside
	if isPoint {
		if r.loopMatchesA0() {
			// Both sides are point loops. The edge is kept only:
			//  - for closed intersection, open union, and open difference;
			//  - if A and B are both holes or both shells.
			cp.inside = createDegen || keepDegenA || (cp.inside == cp.isDegenerateHole[r.a0LoopMatchID])
		} else if r.a0MatchesPolygon {
			// A point loop in A matches a polygon vertex in B. Note that this
			// code can emit an extra isolated vertex if A represents a point
			// hole, but this doesn't matter (see comments on the call to
			// addPointEdge below).
			if cp.polygonModel != PolygonModelSemiOpen {
				cp.inside = createDegen || keepDegenA
			}
		}
	} else if r.matchesPolygon() {
		if cp.isDegenerate(aID) {
			// The A edge has a sibling. The edge is kept only:
			//  - for closed intersection, open union, and open difference;
			//  - if the A sibling pair is a hole and the B edge is a hole or
			//    in the interior, or the A sibling pair is a shell and the B
			//    edge is a shell (B is degenerate) or not in the interior.
			cp.inside = createDegen || keepDegenA ||
				((!r.matchesSibling() || cp.inside) == cp.isDegenerateHole[aID])
		} else {
			// Matching edges are kept unless the B geometry is a sibling pair.
			// In the SEMI_OPEN model they are kept only if B is a shell (the
			// "inside" state is false).
			if !r.matchesSibling() || createDegen || keepDegenB {
				cp.inside = true
			}
		}
	} else if r.matchesSibling() {
		if cp.isDegenerate(aID) {
			// The A edge has a sibling. The edge is kept only if A is a sibling
			// pair shell and the operation is closed intersection, open union,
			// or open difference.
			cp.inside = (createDegen || keepDegenA) && !cp.isDegenerateHole[aID]
		} else {
			cp.inside = createDegen
		}
	}
	if cp.inside != semiOpenInside {
		r.a1Crossings++ // Restores the correct (semi-open) state later.
	}

	// Test whether the first vertex of this edge should be emitted as an
	// isolated degenerate vertex. This is only needed in the second pass
	// when:
	//  - a0 matches a vertex of the B polygon;
	//  - the initial part of the A edge will not be emitted; and
	//  - the operation is closed intersection or open union, or open
	//    difference and the B geometry is a point loop.
	//
	// The logic does not attempt to avoid redundant extra vertices (e.g.
	// the extra code in processEdge1() that checks whether the vertex is
	// the endpoint of the preceding emitted edge) since these will be
	// removed during S2Builder::Graph creation by DegenerateEdges::DISCARD
	// or DISCARD_EXCESS (which are necessary in any case due to snapping).
	if emitShared && r.a0MatchesPolygon && !cp.inside &&
		(createDegen || (keepDegenB && r.loopMatchesA0())) {
		if !cp.addPointEdge(a.V0, 2) {
			return false
		}
	}

	// Since we skipped edges in the first pass that only had a sibling pair
	// match in the B geometry, we sometimes need to emit the sibling pair of
	// an edge in the second pass. This happens only if:
	//  - the operation is closed intersection, open union, or open
	//    difference;
	//  - the A geometry is not a sibling pair (since otherwise we will
	//    process that edge as well); and
	//  - the B geometry is not a sibling pair hole (since then only one edge
	//    should be emitted).
	if r.matchesSibling() && (createDegen || keepDegenB) && !cp.isDegenerate(aID) && !isBHole {
		sibling := s2.Edge{V0: a.V1, V1: a.V0}
		if !cp.addEdge(r.siblingMatchID, sibling, 2 /*dimension*/, 0) {
			return false
		}
	}

	// Test whether the entire edge or any part of it belongs to the output.
	if cp.inside || r.interiorCrossings > 0 {
		// Note: updates "inside" to correspond to the state just before a1.
		if !cp.addEdge(aID, a, 2 /*dimension*/, r.interiorCrossings) {
			return false
		}
	}
	if r.a1Crossings&1 != 0 {
		cp.inside = !cp.inside
	}
	return true
}

// skipCrossings skips any crossings that were not needed to determine the
// result.
func (cp *crossingProcessor) skipCrossings(aID s2.ShapeEdgeID, it *crossingIterator) {
	for !it.done(aID) {
		it.next()
	}
}

// processEdgeCrossings returns a summary of the relationship between a test
// edge from region A and a set of crossing edges from region B (see
// edgeCrossingResult).
//
// NOTE(ericv): This could be optimized considerably for the case of
// polyline/polygon operations, by skipping crossings that don't affect the
// result.
func (cp *crossingProcessor) processEdgeCrossings(aID s2.ShapeEdgeID, a s2.Edge, it *crossingIterator) edgeCrossingResult {
	cp.pendingSourceEdgeCrossings = cp.pendingSourceEdgeCrossings[:0]
	r := newEdgeCrossingResult()
	if it.done(aID) {
		return r
	}

	for ; !it.done(aID); it.next() {
		// Polyline and polygon "inside" states are not affected by point
		// geometry.
		if it.bDimension == 0 {
			continue
		}
		b := it.bEdge()
		if it.isInteriorCrossing() {
			// The crossing occurs in the edge interior. The condition below
			// says that (1) polyline crossings don't affect the polygon
			// "inside" state, and (2) subtracting a crossing polyline from a
			// polyline has no effect.
			if cp.aDimension <= it.bDimension && !(cp.invertB != cp.invertResult && it.bDimension == 1) {
				src := sourceID{cp.bRegionID, it.bID().ShapeID, it.bID().EdgeID}
				cp.addInteriorCrossing(sourceEdgeCrossing{src, it.leftToRight()})
			}
			if it.bDimension == 1 {
				r.interiorCrossings += 2
			} else {
				r.interiorCrossings++
			}
		} else if it.bDimension == 1 {
			// The polygon "inside" state is not affected by polyline geometry.
			if cp.aDimension == 2 {
				continue
			}
			if (a.V0 == b.V0 && a.V1 == b.V1) || (a.V0 == b.V1 && a.V1 == b.V0) {
				r.matchesPolyline = true
			}
			if (a.V0 == b.V0 || a.V0 == b.V1) && cp.polylineEdgeContainsVertex(a.V0, it, 1) {
				r.a0MatchesPolyline = true
			}
			if (a.V1 == b.V0 || a.V1 == b.V1) && cp.polylineEdgeContainsVertex(a.V1, it, 1) {
				r.a1MatchesPolyline = true
			}
		} else {
			if a.V0 == a.V1 || b.V0 == b.V1 {
				// There are no edge crossings since at least one edge is
				// degenerate.
				if a.V0 == b.V0 && a.V0 == b.V1 {
					r.a0LoopMatchID = it.bID()
				}
			} else if a.V0 == b.V0 && a.V1 == b.V1 {
				r.a0Crossings++
				r.polygonMatchID = it.bID()
			} else if a.V0 == b.V1 && a.V1 == b.V0 {
				r.a0Crossings++
				r.siblingMatchID = it.bID()
			} else if it.isVertexCrossing() {
				if a.V0 == b.V0 || a.V0 == b.V1 {
					r.a0Crossings++
				} else {
					r.a1Crossings++
				}
			}
			if a.V0 == b.V0 || a.V0 == b.V1 {
				r.a0MatchesPolygon = true
			}
			if a.V1 == b.V0 || a.V1 == b.V1 {
				r.a1MatchesPolygon = true
			}
		}
	}
	return r
}

// polylineEdgeContainsVertex returns true if the vertex "v" is contained by
// the polyline edge referred to by the crossingIterator "it", taking into
// account the PolylineModel. "dimension" is 0 or 1 according to whether "v"
// should be modeled as a point or as a degenerate polyline. (This only
// makes a difference when the containing polyline is degenerate, since the
// polyline AA contains itself in all boundary models but contains the point
// A only in the CLOSED model.)
//
// REQUIRES: it.bDimension == 1
// REQUIRES: "v" is an endpoint of it.bEdge()
func (cp *crossingProcessor) polylineEdgeContainsVertex(v s2.Point, it *crossingIterator, dimension int) bool {
	// Closed polylines contain all their vertices.
	if cp.polylineModel == PolylineModelClosed {
		return true
	}

	// Note that the code below is structured so that it.bEdge() is not
	// usually needed (since accessing the edge can be relatively expensive).
	bChain := it.bChainInfo()
	bEdgeID := it.bEdgeID()

	// The last polyline vertex is never contained. (For polyline loops, it is
	// sufficient to treat the first vertex as begin contained.) This case
	// also handles degenerate polylines (polylines with one edge where v0 ==
	// v1), which are considered to contain their single vertex only when
	// "dimension" is 1.
	if bEdgeID == bChain.limit-1 && v == it.bEdge().V1 &&
		(dimension == 0 || bEdgeID > 0 || v != it.bEdge().V0) {
		return false
	}

	// Otherwise all interior vertices are contained. The first polyline
	// vertex is contained if either the polyline model is not OPEN, or the
	// polyline forms a loop and polylineLoopsHaveBoundaries is false.
	if cp.polylineContainsV0(bEdgeID, bChain.start) {
		return true
	}
	if v != it.bEdge().V0 {
		return true
	}
	if cp.polylineLoopsHaveBoundaries {
		return false
	}
	return v == chainEdge(it.bShape, bChain.chainID, bChain.limit-bChain.start-1).V1
}

// doneBoundaryPair translates the temporary representation of crossing edges
// (sourceID) into the format expected by EdgeClippingLayer (InputEdgeId).
// This method should be called after each pair of calls to startBoundary.
// (The only operation that requires two pairs of calls is
// SYMMETRIC_DIFFERENCE.)
func (cp *crossingProcessor) doneBoundaryPair() {
	// Add entries that translate the "special" crossings.
	cp.sourceIDMap[specialSourceID(kSetInside)] = kSetInside
	cp.sourceIDMap[specialSourceID(kSetInvertB)] = kSetInvertB
	cp.sourceIDMap[specialSourceID(kSetReverseA)] = kSetReverseA
	for _, tmp := range cp.sourceEdgeCrossings {
		id := cp.sourceIDMap[tmp.second.id]
		*cp.inputCrossings = append(*cp.inputCrossings, inputEdgeCrossing{tmp.first, crossingInputEdge{id, tmp.second.leftToRight}})
	}
	cp.sourceEdgeCrossings = nil
	clear(cp.sourceIDMap)
}

// BooleanOperation is S2BooleanOperation.
type BooleanOperation struct {
	options BooleanOptions
	opType  OpType
	// The input regions.
	regions [2]*s2.ShapeIndex
	// The output consists either of zero layers, one layer, or three layers.
	layers []Layer
	// The following field is set if and only if there are no output layers.
	resultEmpty *bool
}

// NewBooleanOperation is the constructor taking one layer (all output
// edges) or three layers (points, polylines and polygons, by dimension).
func NewBooleanOperation(op OpType, layers []Layer, o BooleanOptions) *BooleanOperation {
	return &BooleanOperation{options: o, opType: op, layers: layers}
}

// OpType returns the operation type.
func (op *BooleanOperation) OpType() OpType { return op.opType }

// Options returns the options.
func (op *BooleanOperation) Options() BooleanOptions { return op.options }

// Build executes the given operation on regions a and b, sending the
// result to the layers.
func (op *BooleanOperation) Build(a, b *s2.ShapeIndex) error {
	op.regions[0] = a
	op.regions[1] = b
	return newBooleanImpl(op).build()
}

// IsEmptyResult is S2BooleanOperation::IsEmpty: whether the result of the
// given operation is empty.
func IsEmptyResult(opType OpType, a, b *s2.ShapeIndex, o BooleanOptions) bool {
	var resultEmpty bool
	op := &BooleanOperation{options: o, opType: opType, resultEmpty: &resultEmpty}
	_ = op.Build(a, b)
	return resultEmpty
}

// BooleanIntersects is S2BooleanOperation::Intersects.
func BooleanIntersects(a, b *s2.ShapeIndex, o BooleanOptions) bool {
	return !IsEmptyResult(OpIntersection, b, a, o)
}

// BooleanContains is S2BooleanOperation::Contains.
func BooleanContains(a, b *s2.ShapeIndex, o BooleanOptions) bool {
	return IsEmptyResult(OpDifference, b, a, o)
}

// BooleanEquals is S2BooleanOperation::Equals.
func BooleanEquals(a, b *s2.ShapeIndex, o BooleanOptions) bool {
	return IsEmptyResult(OpSymmetricDifference, b, a, o)
}

// booleanImpl is S2BooleanOperation::Impl.
type booleanImpl struct {
	op *BooleanOperation

	// The S2Builder options used to construct the output.
	builderOptions BuilderOptions

	// The S2Builder used to construct the output. Note that the S2Builder
	// object is created only when isBooleanOutput() is false.
	builder *Builder

	// A vector specifying the dimension of each edge added to S2Builder.
	inputDimensions []int8

	// The set of all input edge crossings, which is used by
	// EdgeClippingLayer to construct the clipped output polygon.
	inputCrossings []inputEdgeCrossing

	// A vector containing all pairs of crossing edges from the two input
	// regions (including edge pairs that share a common vertex). The first
	// element of each pair is an edge from "indexCrossingsFirstRegionID",
	// while the second element of each pair is an edge from the other
	// region.
	indexCrossings []indexCrossing

	// Indicates that the first element of each crossing edge pair in
	// "indexCrossings" corresponds to an edge from the given region. This
	// field is negative if indexCrossings has not been computed yet.
	indexCrossingsFirstRegionID int

	// Temporary storage used in getChainStarts(), declared here to avoid
	// repeatedly allocating memory.
	tmpCrossings []indexCrossing
}

// kAllFacesMask is a bit mask representing all six faces of the S2 cube.
const kAllFacesMask = 0x3f

func newBooleanImpl(op *BooleanOperation) *booleanImpl {
	return &booleanImpl{op: op, indexCrossingsFirstRegionID: -1}
}

func (b *booleanImpl) isBooleanOutput() bool { return b.op.resultEmpty != nil }

// addBoundary clips the boundary of A to the interior of the opposite region
// B and adds the resulting edges to the output. Optionally, any combination
// of region A, region B, and the result may be inverted, which allows
// operations such as union and difference to be implemented.
//
// Note that when an input region is inverted with respect to the output
// (e.g., invertA != invertResult), all polygon edges are reversed and all
// points and polylines are discarded, since the complement of such objects
// cannot be represented. (If you want to compute the complement of points
// or polylines, you can use S2LaxPolygonShape to represent your geometry as
// degenerate polygons instead.)
//
// This method must be called an even number of times (first to clip A to B
// and then to clip B to A), calling doneBoundaryPair() after each pair.
//
// Supports "early exit" in the case of boolean results by returning false
// as soon as the result is known to be non-empty.
func (b *booleanImpl) addBoundary(aRegionID int, invertA, invertB, invertResult bool, aChainStarts []s2.ShapeEdgeID, cp *crossingProcessor) bool {
	aIndex := b.op.regions[aRegionID]
	bIndex := b.op.regions[1-aRegionID]
	if !b.initIndexCrossings(aRegionID) {
		return false
	}
	cp.startBoundary(int32(aRegionID), invertA, invertB, invertResult)

	// Walk the boundary of region A and build a list of all edge crossings.
	// We also keep track of whether the current vertex is inside region B.
	chainStartIter := 0
	nextCrossingIter := newCrossingIterator(bIndex, b.indexCrossings, true /*crossingsComplete*/)
	nextID := shapeEdgeIDMin(aChainStarts[chainStartIter], nextCrossingIter.aID())
	for nextID != kSentinel {
		aShapeID := nextID.ShapeID
		aShape := aIndex.Shape(aShapeID)
		cp.startShape(aShape)
		for nextID.ShapeID == aShapeID {
			// TODO(ericv): Special handling of dimension 0? Can omit most of
			// this code, including the loop, since all chains are of length 1.
			edgeID := int(nextID.EdgeID)
			chainPosition := chainPosition(aShape, edgeID)
			chainID := chainPosition.ChainID
			chain := aShape.Chain(chainID)
			startInside := nextID == aChainStarts[chainStartIter]
			if startInside {
				chainStartIter++
			}
			cp.startChain(chainID, chain, startInside)
			chainLimit := chain.Start + chain.Length
			for edgeID < chainLimit {
				aID := s2.ShapeEdgeID{ShapeID: aShapeID, EdgeID: int32(edgeID)}
				if !cp.processEdge(aID, nextCrossingIter) {
					return false
				}
				if cp.inside {
					edgeID++
				} else if nextCrossingIter.aID().ShapeID == aShapeID && int(nextCrossingIter.aID().EdgeID) < chainLimit {
					edgeID = int(nextCrossingIter.aID().EdgeID)
				} else {
					break
				}
			}
			nextID = shapeEdgeIDMin(aChainStarts[chainStartIter], nextCrossingIter.aID())
		}
	}
	return true
}

// getChainStarts returns the first edge of each edge chain from "aRegionID"
// whose first vertex is contained by opposite region's polygons (using the
// semi-open boundary model). Each input region and the result region are
// inverted as specified (invertA, invertB, invertResult) before testing for
// containment. The algorithm uses these "chain starts" in conjunction with
// the index crossings to determine which edges are emitted.
//
// Supports "early exit" in the case of boolean results by returning false
// as soon as the result is known to be non-empty.
func (b *booleanImpl) getChainStarts(aRegionID int, invertA, invertB, invertResult bool, cp *crossingProcessor, chainStarts *[]s2.ShapeEdgeID) bool {
	aIndex := b.op.regions[aRegionID]
	bIndex := b.op.regions[1-aRegionID]

	if b.isBooleanOutput() {
		// If boolean output is requested, then we use the crossingProcessor
		// to determine whether the first edge of each chain will be emitted
		// to the output region. This lets us terminate the operation early
		// in many cases.
		cp.startBoundary(int32(aRegionID), invertA, invertB, invertResult)
	}

	// If region B has no two-dimensional shapes and is not inverted, then by
	// definition no chain starts are contained. However if boolean output is
	// requested then we check for containment anyway, since as a side
	// effect we may discover that the result region is non-empty and
	// terminate the entire operation early.
	bHasInterior := hasInterior(bIndex)
	if bHasInterior || invertB || b.isBooleanOutput() {
		query := s2.NewContainsPointQuery(bIndex, s2.VertexModelSemiOpen)
		numShapeIDs := numShapeIDs(aIndex)
		for shapeID := int32(0); shapeID < numShapeIDs; shapeID++ {
			aShape := aIndex.Shape(shapeID)
			if aShape == nil {
				continue
			}

			// If region A is being subtracted from region B, points and
			// polylines in region A can be ignored since these shapes never
			// contribute to the output (they can only remove edges from
			// region B).
			if invertA != invertResult && aShape.Dimension() < 2 {
				continue
			}

			if b.isBooleanOutput() {
				cp.startShape(aShape)
			}
			numChains := aShape.NumChains()
			for chainID := 0; chainID < numChains; chainID++ {
				chain := aShape.Chain(chainID)
				if chain.Length == 0 {
					continue
				}
				a := s2.ShapeEdge{ID: s2.ShapeEdgeID{ShapeID: shapeID, EdgeID: int32(chain.Start)}, Edge: chainEdge(aShape, chainID, 0)}
				inside := (bHasInterior && query.Contains(a.Edge.V0)) != invertB
				if inside {
					*chainStarts = append(*chainStarts, s2.ShapeEdgeID{ShapeID: shapeID, EdgeID: int32(chain.Start)})
				}
				if b.isBooleanOutput() {
					cp.startChain(chainID, chain, inside)
					if !b.processIncidentEdges(a, bIndex, cp) {
						return false
					}
				}
			}
		}
	}
	*chainStarts = append(*chainStarts, kSentinel)
	return true
}

func (b *booleanImpl) processIncidentEdges(a s2.ShapeEdge, bIndex *s2.ShapeIndex, cp *crossingProcessor) bool {
	b.tmpCrossings = b.tmpCrossings[:0]
	visitIncidentEdges(bIndex, a.Edge.V0, func(be s2.ShapeEdge) bool {
		return b.addIndexCrossing(a, be, false /*isInterior*/, &b.tmpCrossings)
	})
	// Fast path for the common case where there are no incident edges. We
	// return false (terminating early) if the first chain edge will be
	// emitted.
	if len(b.tmpCrossings) == 0 {
		return !cp.inside
	}
	// Otherwise we invoke the full crossingProcessor logic to determine
	// whether the first chain edge will be emitted.
	if len(b.tmpCrossings) > 1 {
		sort.Slice(b.tmpCrossings, func(i, j int) bool { return indexCrossingLess(b.tmpCrossings[i], b.tmpCrossings[j]) })
	}
	b.tmpCrossings = append(b.tmpCrossings, indexCrossing{a: kSentinel, b: kSentinel})
	nextCrossing := newCrossingIterator(bIndex, b.tmpCrossings, false /*crossingsComplete*/)
	return cp.processEdge(a.ID, nextCrossing)
}

// hasInterior returns true if any shape of the given index has dimension 2.
func hasInterior(index *s2.ShapeIndex) bool {
	for s := numShapeIDs(index) - 1; s >= 0; s-- {
		if shape := index.Shape(s); shape != nil && shape.Dimension() == 2 {
			return true
		}
	}
	return false
}

func (b *booleanImpl) addIndexCrossing(a, bb s2.ShapeEdge, isInterior bool, crossings *[]indexCrossing) bool {
	c := indexCrossing{a: a.ID, b: bb.ID}
	if isInterior {
		c.isInteriorCrossing = true
		if s2.RobustSign(a.Edge.V0, a.Edge.V1, bb.Edge.V0) == s2.CounterClockwise {
			c.leftToRight = true
		}
		b.builder.AddIntersection(s2.Intersection(a.Edge.V0, a.Edge.V1, bb.Edge.V0, bb.Edge.V1))
	} else {
		// TODO(ericv): This field is only needed for polygon-related pairs.
		if s2.VertexCrossing(a.Edge.V0, a.Edge.V1, bb.Edge.V0, bb.Edge.V1) {
			c.isVertexCrossing = true
		}
	}
	*crossings = append(*crossings, c)
	return true // Continue visiting.
}

// initIndexCrossings initializes indexCrossings to the set of crossing edge
// pairs such that the first element of each pair is an edge from
// "regionID".
//
// Supports "early exit" in the case of boolean results by returning false
// as soon as the result is known to be non-empty.
func (b *booleanImpl) initIndexCrossings(regionID int) bool {
	if regionID == b.indexCrossingsFirstRegionID {
		return true
	}
	if b.indexCrossingsFirstRegionID < 0 {
		// TODO(ericv): This would be more efficient if VisitCrossingEdgePairs()
		// returned the sign (+1 or -1) of the interior crossing, i.e.
		// "int interior_crossing_sign" rather than "bool is_interior".
		if !visitCrossingEdgePairs(b.op.regions[0], b.op.regions[1], s2.CrossingTypeAll,
			func(a, bb s2.ShapeEdge, isInterior bool) bool {
				// For all supported operations (union, intersection, and
				// difference), if the input edges have an interior crossing
				// then the output is guaranteed to have at least one edge.
				if isInterior && b.isBooleanOutput() {
					return false
				}
				return b.addIndexCrossing(a, bb, isInterior, &b.indexCrossings)
			}) {
			return false
		}
		if len(b.indexCrossings) > 1 {
			sort.Slice(b.indexCrossings, func(i, j int) bool { return indexCrossingLess(b.indexCrossings[i], b.indexCrossings[j]) })
			b.indexCrossings = slices.CompactFunc(b.indexCrossings, func(x, y indexCrossing) bool { return x.a == y.a && x.b == y.b })
		}
		// Add a sentinel value to simplify the loop logic.
		b.indexCrossings = append(b.indexCrossings, indexCrossing{a: kSentinel, b: kSentinel})
		b.indexCrossingsFirstRegionID = 0
	}
	if regionID != b.indexCrossingsFirstRegionID {
		for i := range b.indexCrossings {
			c := &b.indexCrossings[i]
			c.a, c.b = c.b, c.a
			// The following predicates get inverted when the edges are swapped.
			c.leftToRight = !c.leftToRight
			c.isVertexCrossing = !c.isVertexCrossing
		}
		sort.Slice(b.indexCrossings, func(i, j int) bool { return indexCrossingLess(b.indexCrossings[i], b.indexCrossings[j]) })
		b.indexCrossingsFirstRegionID = regionID
	}
	return true
}

// addBoundaryPair supports "early exit" in the case of boolean results by
// returning false as soon as the result is known to be non-empty.
func (b *booleanImpl) addBoundaryPair(invertA, invertB, invertResult bool, cp *crossingProcessor) bool {
	// Optimization: if the operation is DIFFERENCE or SYMMETRIC_DIFFERENCE
	// and both input regions are identical, then the result is empty.
	t := b.op.opType
	if t == OpDifference || t == OpSymmetricDifference {
		if b.areRegionsIdentical() {
			return true
		}
	}
	// else if isBooleanOutput(): TODO(ericv): Add a quick check here.
	var aStarts, bStarts []s2.ShapeEdgeID
	if !b.getChainStarts(0, invertA, invertB, invertResult, cp, &aStarts) ||
		!b.getChainStarts(1, invertB, invertA, invertResult, cp, &bStarts) ||
		!b.addBoundary(0, invertA, invertB, invertResult, aStarts, cp) ||
		!b.addBoundary(1, invertB, invertA, invertResult, bStarts, cp) {
		return false
	}
	if !b.isBooleanOutput() {
		cp.doneBoundaryPair()
	}
	return true
}

// areRegionsIdentical returns true if both input regions are identical
// (same shapes, chains and vertices).
func (b *booleanImpl) areRegionsIdentical() bool {
	a := b.op.regions[0]
	bb := b.op.regions[1]
	if a == bb {
		return true
	}

	// Note that we need to compare the dimensions and chain structure
	// before decoding any S2Points.
	numShapeIDs := numShapeIDs(a)
	if numShapeIDs != numShapeIDsOf(bb) {
		return false
	}
	for s := int32(0); s < numShapeIDs; s++ {
		aShape := a.Shape(s)
		bShape := bb.Shape(s)
		if aShape == nil || bShape == nil {
			if (aShape == nil) != (bShape == nil) {
				return false
			}
			continue
		}
		dimension := aShape.Dimension()
		if dimension != bShape.Dimension() {
			return false
		}
		numChains := aShape.NumChains()
		if numChains != bShape.NumChains() {
			return false
		}
		numEdges := aShape.NumEdges()
		if numEdges != bShape.NumEdges() {
			return false
		}
		if dimension == 0 {
			continue // All chains are of length 1.
		}
		for c := 0; c < numChains; c++ {
			if aShape.Chain(c).Length != bShape.Chain(c).Length {
				return false
			}
		}
	}
	// Next compare vertex positions.
	for s := int32(0); s < numShapeIDs; s++ {
		aShape := a.Shape(s)
		bShape := bb.Shape(s)
		if aShape == nil {
			continue
		}
		numChains := aShape.NumChains()
		for c := 0; c < numChains; c++ {
			aChain := aShape.Chain(c)
			for i := 0; i < aChain.Length; i++ {
				aEdge := chainEdge(aShape, c, i)
				bEdge := chainEdge(bShape, c, i)
				if aEdge.V0 != bEdge.V0 || aEdge.V1 != bEdge.V1 {
					return false
				}
			}
		}
		// Note that we don't need to compare ReferencePoints, because the
		// chains and edges completely define the shape.
	}
	return true
}

func numShapeIDsOf(index *s2.ShapeIndex) int32 { return numShapeIDs(index) }

// buildOpType is S2BooleanOperation::Impl::BuildOpType.
func (b *booleanImpl) buildOpType(opType OpType) bool {
	// CrossingProcessor does the real work of emitting the output edges.
	o := b.op.options
	cp := newCrossingProcessor(o.PolygonModel, o.PolylineModel, o.PolylineLoopsHaveBoundaries, b.builder, &b.inputDimensions, &b.inputCrossings)
	switch opType {
	case OpUnion:
		// A | B == ~(~A & ~B)
		return b.addBoundaryPair(true, true, true, cp)
	case OpIntersection:
		// A & B
		return b.addBoundaryPair(false, false, false, cp)
	case OpDifference:
		// A - B = A & ~B
		//
		// Note that degeneracies are implemented such that the symmetric
		// operation (-B + A) also produces correct results. This can be
		// tested by swapping the arguments here.
		return b.addBoundaryPair(false, true, false, cp)
	case OpSymmetricDifference:
		// Compute the union of (A - B) and (B - A).
		return b.addBoundaryPair(false, true, false, cp) && b.addBoundaryPair(true, false, false, cp)
	}
	panic("Invalid S2BooleanOperation::OpType")
}

// isFullPolygonResult returns whether the result is the full polygon, given
// that it has no polygon edges: S2BooleanOperation::Impl::IsFullPolygonResult.
//
// The result can be empty or full only when the input polygons are empty,
// full, or snapped away. This is decided with heuristics based on the cube
// faces the inputs cover and on their areas. (The graph is not used.)
func (b *booleanImpl) isFullPolygonResult(*Graph) (bool, error) {
	a := b.op.regions[0]
	bb := b.op.regions[1]
	switch b.op.opType {
	case OpUnion:
		return b.isFullPolygonUnion(a, bb), nil
	case OpIntersection:
		return b.isFullPolygonIntersection(a, bb), nil
	case OpDifference:
		return b.isFullPolygonDifference(a, bb), nil
	case OpSymmetricDifference:
		return b.isFullPolygonSymmetricDifference(a, bb), nil
	}
	panic("Invalid S2BooleanOperation::OpType")
}

func (b *booleanImpl) isFullPolygonUnion(a, bb *s2.ShapeIndex) bool {
	// See comments in isFullPolygonResult(). The most common case is that
	// neither input polygon is empty but the result is empty due to
	// snapping.

	// The result can be full only if the union of the two input geometries
	// intersects all six faces of the S2 cube.
	if (faceMask(a) | faceMask(bb)) != kAllFacesMask {
		return false
	}

	// The union area satisfies:
	//
	//   max(A, B) <= Union(A, B) <= min(4*Pi, A + B)
	//
	// where A, B can refer to a polygon or its area. We then choose the
	// result that assumes the smallest amount of error.
	aArea, bArea := indexArea(a), indexArea(bb)
	minArea := math.Max(aArea, bArea)
	maxArea := math.Min(4*math.Pi, aArea+bArea)
	return minArea > 4*math.Pi-maxArea
}

func (b *booleanImpl) isFullPolygonIntersection(a, bb *s2.ShapeIndex) bool {
	// The most common case is that the result is empty.

	// The result can be full only if each input geometry intersects all six
	// faces of the S2 cube.
	if (faceMask(a) & faceMask(bb)) != kAllFacesMask {
		return false
	}

	// The intersection area satisfies:
	//
	//   A + B - 4*Pi <= Intersection(A, B) <= min(A, B)
	aArea, bArea := indexArea(a), indexArea(bb)
	minArea := math.Max(0, aArea+bArea-4*math.Pi)
	maxArea := math.Min(aArea, bArea)
	return minArea > 4*math.Pi-maxArea
}

func (b *booleanImpl) isFullPolygonDifference(a, bb *s2.ShapeIndex) bool {
	// The most common case is that the result is empty.

	// The result can be full only if the first geometry intersects all six
	// faces of the S2 cube.
	if faceMask(a) != kAllFacesMask {
		return false
	}

	// The difference area satisfies:
	//
	//   A - B <= Difference(A, B) <= min(A, 4*Pi - B)
	aArea, bArea := indexArea(a), indexArea(bb)
	minArea := math.Max(0, aArea-bArea)
	maxArea := math.Min(aArea, 4*math.Pi-bArea)
	return minArea > 4*math.Pi-maxArea
}

func (b *booleanImpl) isFullPolygonSymmetricDifference(a, bb *s2.ShapeIndex) bool {
	// The most common case is that the result is empty.

	// The result can be full only if the union of the two input geometries
	// intersects all six faces of the S2 cube.
	aMask := faceMask(a)
	bMask := faceMask(bb)
	if (aMask | bMask) != kAllFacesMask {
		return false
	}

	// The symmetric difference area satisfies:
	//
	//   |A - B| <= SymmetricDifference(A, B) <= 4*Pi - |4*Pi - (A + B)|
	aArea, bArea := indexArea(a), indexArea(bb)
	minArea := math.Abs(aArea - bArea)
	maxArea := 4*math.Pi - math.Abs(4*math.Pi-(aArea+bArea))

	// Now we choose the result that assumes the smallest amount of error
	// (minArea in the "full" case, and maxArea in the "empty" case).
	// However in the case of symmetric difference these two errors may be
	// equal, meaning that the result is ambiguous. This happens when both
	// polygons have area 2*Pi. Furthermore, this can happen even when the
	// areas are not exactly 2*Pi due to snapping and area calculation
	// errors.
	//
	// To determine whether the result is ambiguous, we compute a rough
	// estimate of the maximum expected area error (including errors due to
	// snapping), using the worst-case error bound for a hemisphere defined
	// by 4 vertices.
	edgeSnapRadius := b.builderOptions.SnapFunction.SnapRadius().Radians() + b.builderOptions.IntersectionTolerance
	hemisphereAreaError := 2*math.Pi*edgeSnapRadius + 40*dblEpsilon // GetCurvatureMaxError

	// The following sign is the difference between the error needed for an
	// empty result and the error needed for a full result. It is negative
	// if an empty result is possible, positive if a full result is
	// possible, and zero if both results are possible.
	errorSign := minArea - (4*math.Pi - maxArea)
	if math.Abs(errorSign) <= hemisphereAreaError {
		// Handling the ambiguous case correctly requires a more
		// sophisticated algorithm (see below), but we can at least handle
		// the simple cases by testing whether both input geometries
		// intersect all 6 cube faces. If not, then the result is definitely
		// full.
		if (aMask & bMask) != kAllFacesMask {
			return true
		}

		// Otherwise both regions have area 2*Pi and intersect all 6 cube
		// faces. We choose "empty" in this case under the assumption that it
		// is more likely that the user is computing the difference between
		// two nearly identical polygons.
		//
		// TODO(ericv): Implement a robust algorithm based on examining the
		// edge snapping results directly, or alternatively add another
		// heuristic (such as testing containment of random points, or using
		// a larger bit mask in the tests above, e.g. a 24-bit mask
		// representing all level 1 cells).
		return false
	}
	return errorSign > 0
}

// dblEpsilon is DBL_EPSILON.
const dblEpsilon = 0x1p-52

// kIntersectionError is S2::kIntersectionError (8 * DBL_ERR), in radians.
const kIntersectionError = 8 * dblErrS2

func (b *booleanImpl) doBuild() error {
	b.builderOptions = BuilderOptions{
		SnapFunction:          b.op.options.SnapFunction,
		IntersectionTolerance: kIntersectionError,
	}
	if b.builderOptions.SnapFunction == nil {
		b.builderOptions.SnapFunction = s2.NewIdentitySnapper(0)
	}
	if b.op.options.SplitAllCrossingPolylineEdges {
		b.builderOptions.SplitCrossingEdges = true
	}
	// TODO(ericv): Ideally idempotent() should be true, but existing clients
	// expect vertices closer than the full "snap_radius" to be snapped.
	b.builderOptions.Idempotent = false

	if b.isBooleanOutput() {
		// BuildOpType() returns true if and only if the result has no edges.
		*b.op.resultEmpty = b.buildOpType(b.op.opType)
		if *b.op.resultEmpty {
			full, _ := b.isFullPolygonResult(nil)
			*b.op.resultEmpty = !full
		}
		return nil
	}
	b.builder = NewBuilder(b.builderOptions)
	b.builder.StartLayer(&edgeClippingLayer{layers: b.op.layers, inputDimensions: &b.inputDimensions, inputCrossings: &b.inputCrossings})

	// Polygons with no edges are assumed to be empty. It is the
	// responsibility of clients to check for full polygons.
	b.builder.AddIsFullPolygonPredicate(b.isFullPolygonResult)
	b.buildOpType(b.op.opType)

	// Release memory that is no longer needed.
	b.indexCrossings = nil
	return b.builder.Build()
}

func (b *booleanImpl) build() error { return b.doBuild() }
