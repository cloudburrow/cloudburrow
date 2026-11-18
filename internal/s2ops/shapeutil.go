// Copyright 2013 Google Inc. All Rights Reserved.
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

// The s2shapeutil and S2ContainsPointQuery helpers S2BooleanOperation needs
// that github.com/golang/geo keeps unexported: VisitCrossingEdgePairs
// (src/s2/s2shapeutil_visit_crossing_edge_pairs.cc), VisitIncidentEdges
// (src/s2/s2contains_point_query.h), S2::GetArea for an index
// (src/s2/s2shape_measures.cc, src/s2/s2shape_index_measures.cc) and
// S2::TurnAngle (src/s2/s2measures.cc). The index-cell walks of the C++ are
// replaced by golang/geo's CrossingEdgeQuery (or brute force), which visit
// the same edge pairs.

import (
	"math"
	"sort"

	"github.com/golang/geo/s2"
)

// numShapeIDs is S2ShapeIndex::num_shape_ids(): one more than the largest
// shape id handed out (golang/geo keeps the counter unexported; ids are
// never reused, so this probes past the live shapes).
func numShapeIDs(index *s2.ShapeIndex) int32 {
	n := int32(index.Len())
	for index.Shape(n) != nil {
		n++
	}
	return n
}

// crossingEdgePairVisitor is s2shapeutil::EdgePairVisitor.
type crossingEdgePairVisitor func(a, b s2.ShapeEdge, isInterior bool) bool

// visitCrossingEdgePairs is s2shapeutil::VisitCrossingEdgePairs(a_index,
// b_index, type, visitor): every pair of an edge of a and an edge of b that
// intersect (CrossingSign >= 0 for CrossingType::ALL, > 0 for INTERIOR),
// with isInterior set when the crossing is interior to both edges. Pairs are
// visited in (a, b) order.
func visitCrossingEdgePairs(aIndex, bIndex *s2.ShapeIndex, crossingType s2.CrossingType, visitor crossingEdgePairVisitor) bool {
	minSign := s2.MaybeCross
	if crossingType == s2.CrossingTypeInterior {
		minSign = s2.Cross
	}
	bShapes := map[s2.Shape]int32{}
	nb := numShapeIDs(bIndex)
	for id := int32(0); id < nb; id++ {
		if sh := bIndex.Shape(id); sh != nil {
			bShapes[sh] = id
		}
	}
	query := s2.NewCrossingEdgeQuery(bIndex)
	na := numShapeIDs(aIndex)
	type cand struct {
		id   s2.ShapeEdgeID
		edge s2.Edge
	}
	for aid := int32(0); aid < na; aid++ {
		aShape := aIndex.Shape(aid)
		if aShape == nil {
			continue
		}
		for e := 0; e < aShape.NumEdges(); e++ {
			a := aShape.Edge(e)
			var cs []cand
			if a.V0 == a.V1 {
				// Degenerate edges: test every edge exactly.
				for bid := int32(0); bid < nb; bid++ {
					bShape := bIndex.Shape(bid)
					if bShape == nil {
						continue
					}
					for f := 0; f < bShape.NumEdges(); f++ {
						cs = append(cs, cand{s2.ShapeEdgeID{ShapeID: bid, EdgeID: int32(f)}, bShape.Edge(f)})
					}
				}
			} else {
				for sh, ids := range query.CrossingsEdgeMap(a.V0, a.V1, s2.CrossingTypeAll) {
					bid := bShapes[sh]
					for _, f := range ids {
						cs = append(cs, cand{s2.ShapeEdgeID{ShapeID: bid, EdgeID: int32(f)}, sh.Edge(f)})
					}
				}
			}
			sort.Slice(cs, func(i, j int) bool { return cs[i].id.Cmp(cs[j].id) < 0 })
			for i, c := range cs {
				if i > 0 && cs[i-1].id == c.id {
					continue
				}
				sign := s2.CrossingSign(a.V0, a.V1, c.edge.V0, c.edge.V1)
				if sign == s2.DoNotCross || (minSign == s2.Cross && sign != s2.Cross) {
					continue
				}
				as := s2.ShapeEdge{ID: s2.ShapeEdgeID{ShapeID: aid, EdgeID: int32(e)}, Edge: a}
				bs := s2.ShapeEdge{ID: c.id, Edge: c.edge}
				if !visitor(as, bs, sign == s2.Cross) {
					return false
				}
			}
		}
	}
	return true
}

// visitIncidentEdges is S2ContainsPointQuery::VisitIncidentEdges: every
// edge of index with p as an endpoint, in (shape, edge) order.
func visitIncidentEdges(index *s2.ShapeIndex, p s2.Point, visitor func(b s2.ShapeEdge) bool) bool {
	n := numShapeIDs(index)
	for id := int32(0); id < n; id++ {
		sh := index.Shape(id)
		if sh == nil {
			continue
		}
		for e := 0; e < sh.NumEdges(); e++ {
			edge := sh.Edge(e)
			if edge.V0 == p || edge.V1 == p {
				if !visitor(s2.ShapeEdge{ID: s2.ShapeEdgeID{ShapeID: id, EdgeID: int32(e)}, Edge: edge}) {
					return false
				}
			}
		}
	}
	return true
}

// faceMask is GetFaceMask (s2boolean_operation.cc): a bit for each cube face
// some index cell lies on.
func faceMask(index *s2.ShapeIndex) uint8 {
	var mask uint8
	for it := index.Iterator(); !it.Done(); it.Next() {
		mask |= 1 << it.CellID().Face()
	}
	return mask
}

// shapeArea is S2::GetArea(const S2Shape&): the sum of the signed areas of
// the chains of a two-dimensional shape, plus 4*Pi if negative.
func shapeArea(shape s2.Shape) float64 {
	if shape.Dimension() != 2 {
		return 0
	}
	area := 0.0
	for c := 0; c < shape.NumChains(); c++ {
		ch := shape.Chain(c)
		if ch.Length == 0 {
			// The full loop.
			area += 4 * math.Pi
			continue
		}
		if ch.Length < 3 {
			continue
		}
		vs := make([]s2.Point, ch.Length)
		for i := 0; i < ch.Length; i++ {
			vs[i] = chainEdge(shape, c, i).V0
		}
		a := s2.LoopFromPoints(vs).Area()
		// Loop.Area is in [0, 4*Pi]; S2::GetSignedArea is in [-2*Pi, 2*Pi].
		if a > 2*math.Pi {
			a -= 4 * math.Pi
		}
		area += a
	}
	if area < 0 {
		area += 4 * math.Pi
	}
	return math.Min(area, 4*math.Pi)
}

// indexArea is S2::GetArea(const S2ShapeIndex&).
func indexArea(index *s2.ShapeIndex) float64 {
	area := 0.0
	n := numShapeIDs(index)
	for id := int32(0); id < n; id++ {
		if sh := index.Shape(id); sh != nil {
			area += shapeArea(sh)
		}
	}
	return area
}

// turnAngle is S2::TurnAngle, on RobustCrossProd as the C++ is.
func turnAngle(a, b, c s2.Point) float64 {
	angle := RobustCrossProd(a, b).Angle(RobustCrossProd(b, c).Vector).Radians()
	if s2.RobustSign(a, b, c) == s2.CounterClockwise {
		return angle
	}
	return -angle
}

// chainEdge is S2Shape::chain_edge(chain_id, offset), computed through
// Edge() because golang/geo's PointVector.ChainEdge returns the wrong edge
// for chains other than the first.
func chainEdge(shape s2.Shape, chainID, offset int) s2.Edge {
	return shape.Edge(shape.Chain(chainID).Start + offset)
}

// chainPosition is S2Shape::chain_position(edge_id), computed from Chain()
// because golang/geo's LaxPolygon.ChainPosition returns the wrong chain id
// for shapes of more than one loop.
func chainPosition(shape s2.Shape, edgeID int) s2.ChainPosition {
	// The chains are sorted by start; find the last one starting at or
	// before edgeID that is not empty.
	n := shape.NumChains()
	i := sort.Search(n, func(i int) bool { return shape.Chain(i).Start > edgeID }) - 1
	for i > 0 && shape.Chain(i).Length == 0 {
		i--
	}
	return s2.ChainPosition{ChainID: i, Offset: edgeID - shape.Chain(i).Start}
}
