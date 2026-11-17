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

// Tests ported from src/s2/s2boolean_operation_test.cc (commit
// 677c8cebcff11ba11accc0b02005bc214e06e9fd), with the parts of
// s2textformat (MakeIndex) and s2builderutil::IndexMatchingLayer
// (s2builderutil_testing.cc) they rely on.

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/golang/geo/s1"
	"github.com/golang/geo/s2"
)

// boParsePoint is s2textformat::MakePoint: "lat:lng" in degrees.
func boParsePoint(t testing.TB, s string) s2.Point {
	t.Helper()
	lat, lng, ok := strings.Cut(strings.TrimSpace(s), ":")
	if !ok {
		t.Fatalf("bad point %q", s)
	}
	la, err1 := strconv.ParseFloat(strings.TrimSpace(lat), 64)
	ln, err2 := strconv.ParseFloat(strings.TrimSpace(lng), 64)
	if err1 != nil || err2 != nil {
		t.Fatalf("bad point %q", s)
	}
	return s2.PointFromLatLng(s2.LatLngFromDegrees(la, ln))
}

// boParsePoints is s2textformat::ParsePoints: comma-separated points.
func boParsePoints(t testing.TB, s string) []s2.Point {
	t.Helper()
	var out []s2.Point
	for _, p := range strings.Split(s, ",") {
		if strings.TrimSpace(p) == "" {
			continue
		}
		out = append(out, boParsePoint(t, p))
	}
	return out
}

// boSplit is s2textformat's SplitString: split, trim, drop empties.
func boSplit(s string, sep string) []string {
	var out []string
	for _, p := range strings.Split(s, sep) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// boLaxPolygon is s2textformat::MakeLaxPolygon.
func boLaxPolygon(t testing.TB, s string) *s2.LaxPolygon {
	t.Helper()
	if s == "empty" {
		return s2.LaxPolygonFromPoints(nil)
	}
	var loops [][]s2.Point
	for _, l := range boSplit(s, ";") {
		if l == "full" {
			loops = append(loops, []s2.Point{})
			continue
		}
		loops = append(loops, boParsePoints(t, l))
	}
	return s2.LaxPolygonFromPoints(loops)
}

// boMakeIndex is s2textformat::MakeIndex: "points # polylines # polygons",
// items separated by '|'.
func boMakeIndex(t testing.TB, s string) *s2.ShapeIndex {
	t.Helper()
	parts := strings.Split(s, "#")
	if len(parts) != 3 {
		t.Fatalf("bad index %q", s)
	}
	index := s2.NewShapeIndex()
	var points []s2.Point
	for _, p := range boSplit(parts[0], "|") {
		points = append(points, boParsePoint(t, p))
	}
	if len(points) > 0 {
		pv := s2.PointVector(points)
		index.Add(&pv)
	}
	for _, l := range boSplit(parts[1], "|") {
		index.Add(s2.LaxPolylineFromPoints(boParsePoints(t, l)))
	}
	for _, l := range boSplit(parts[2], "|") {
		index.Add(boLaxPolygon(t, l))
	}
	return index
}

func boEdgeLess(a, b s2.Edge) bool { return a.Cmp(b) < 0 }

func boEdgesString(es []s2.Edge) string {
	var parts []string
	for _, e := range es {
		la, lb := s2.LatLngFromPoint(e.V0), s2.LatLngFromPoint(e.V1)
		parts = append(parts, fmt.Sprintf("[%g:%g, %g:%g]", la.Lat.Degrees(), la.Lng.Degrees(), lb.Lat.Degrees(), lb.Lng.Degrees()))
	}
	return strings.Join(parts, "; ")
}

// indexMatchingLayer is s2builderutil::IndexMatchingLayer: it checks that
// the graph's edges are those of the shapes of one dimension of an index,
// as a multiset.
type indexMatchingLayer struct {
	options   GraphOptions
	index     *s2.ShapeIndex
	dimension int
}

func (l *indexMatchingLayer) GraphOptions() GraphOptions { return l.options }

func (l *indexMatchingLayer) Build(g *Graph) error {
	var actual, expected []s2.Edge
	for _, e := range g.Edges() {
		actual = append(actual, s2.Edge{V0: g.Vertex(e.First), V1: g.Vertex(e.Second)})
	}
	for id := numShapeIDs(l.index) - 1; id >= 0; id-- {
		shape := l.index.Shape(id)
		if shape == nil || (l.dimension >= 0 && shape.Dimension() != l.dimension) {
			continue
		}
		for e := shape.NumEdges() - 1; e >= 0; e-- {
			expected = append(expected, shape.Edge(e))
		}
	}
	sort.Slice(actual, func(i, j int) bool { return boEdgeLess(actual[i], actual[j]) })
	sort.Slice(expected, func(i, j int) bool { return boEdgeLess(expected[i], expected[j]) })

	// The edges are a multiset, so we can't use std::set_difference.
	var missing, extra []s2.Edge
	for ai, ei := 0, 0; ai < len(actual) || ei < len(expected); {
		if ei == len(expected) || (ai < len(actual) && boEdgeLess(actual[ai], expected[ei])) {
			extra = append(extra, actual[ai])
			ai++
		} else if ai == len(actual) || boEdgeLess(expected[ei], actual[ai]) {
			missing = append(missing, expected[ei])
			ei++
		} else {
			ai++
			ei++
		}
	}
	if len(missing) == 0 && len(extra) == 0 {
		return nil
	}
	return fmt.Errorf("dimension %d: missing edges: %s; extra edges: %s", l.dimension, boEdgesString(missing), boEdgesString(extra))
}

// boExpectResult is ExpectResult: the operation's output, by dimension,
// matches expected, and IsEmpty agrees with it.
func boExpectResult(t *testing.T, opType OpType, options BooleanOptions, a, b, expected *s2.ShapeIndex, what string) {
	t.Helper()
	var layers []Layer
	for dim := 0; dim < 3; dim++ {
		// Since all S2Builder polygon layers require DISCARD or
		// DISCARD_EXCESS for degenerate edges, we intentionally do not
		// require any specific multiplicity for degenerate edges and sibling
		// pairs of dimension 2.
		opts := GraphOptions{EdgeTypeDirected, DegenerateEdgesKeep, DuplicateEdgesKeep, SiblingPairsKeep, true}
		if dim == 2 {
			opts.DegenerateEdges = DegenerateEdgesDiscardExcess
			opts.SiblingPairs = SiblingPairsDiscardExcess
		}
		layers = append(layers, &indexMatchingLayer{opts, expected, dim})
	}
	op := NewBooleanOperation(opType, layers, options)
	if err := op.Build(a, b); err != nil {
		t.Errorf("%s %v failed: %v", what, opType, err)
	}
	// Now try the same thing with boolean output.
	if got, want := IsEmptyResult(opType, a, b, options), numShapeIDs(expected) == 0; got != want {
		t.Errorf("%s %v: IsEmpty = %v, want %v", what, opType, got, want)
	}
}

func boExpect(t *testing.T, opType OpType, options BooleanOptions, a, b, expected string) {
	t.Helper()
	boExpectResult(t, opType, options, boMakeIndex(t, a), boMakeIndex(t, b), boMakeIndex(t, expected),
		fmt.Sprintf("%q %q -> %q", a, b, expected))
}

// boExpect4 runs union, intersection, difference and symmetric difference.
func boExpect4(t *testing.T, options BooleanOptions, a, b, union, intersection, difference, symDifference string) {
	t.Helper()
	boExpect(t, OpUnion, options, a, b, union)
	boExpect(t, OpIntersection, options, a, b, intersection)
	boExpect(t, OpDifference, options, a, b, difference)
	boExpect(t, OpSymmetricDifference, options, a, b, symDifference)
}

func boOptions(f func(*BooleanOptions)) BooleanOptions {
	o := DefaultBooleanOptions()
	if f != nil {
		f(&o)
	}
	return o
}

// boRoundToE is RoundToE: the expected intersections were computed in
// lat-lng space, so results are snapped to E(exp) lat-lngs.
func boRoundToE(exp int) BooleanOptions {
	o := DefaultBooleanOptions()
	o.SnapFunction = boIntLatLngSnapper{s2.NewIntLatLngSnapper(exp), exp}
	return o
}

// boIntLatLngSnapper is s2builderutil::IntLatLngSnapFunction. golang/geo's
// IntLatLngSnapper.SnapPoint rounds radians rather than degrees, so the
// snapping itself is redone here as the C++ does it (the radius and
// separations are golang/geo's, which match the C++).
type boIntLatLngSnapper struct {
	s2.IntLatLngSnapper
	exponent int
}

func (sf boIntLatLngSnapper) SnapPoint(p s2.Point) s2.Point {
	from := math.Pow10(sf.exponent)
	to := math.Pow(10, -float64(sf.exponent))
	ll := s2.LatLngFromPoint(p)
	lat := math.Round(ll.Lat.Degrees() * from)
	lng := math.Round(ll.Lng.Degrees() * from)
	return s2.PointFromLatLng(s2.LatLngFromDegrees(lat*to, lng*to))
}

func TestBooleanOpTypeAccessor(t *testing.T) {
	op := NewBooleanOperation(OpIntersection, nil, DefaultBooleanOptions())
	if op.OpType() != OpIntersection {
		t.Errorf("OpType = %v", op.OpType())
	}
}

func TestBooleanDegeneratePolylines(t *testing.T) {
	a, b := "# 0:0, 0:0 #", "# #"
	for _, m := range []PolylineModel{PolylineModelOpen, PolylineModelSemiOpen, PolylineModelClosed} {
		boExpect(t, OpUnion, boOptions(func(o *BooleanOptions) { o.PolylineModel = m }), a, b, a)
	}
}

func TestBooleanDegeneratePolygons(t *testing.T) {
	a, b := "# # 0:0, 0:5, 5:5, 5:0; 1:1; 2:2, 3:3; 6:6; 7:7, 8:8", "# #"
	for _, m := range []PolygonModel{PolygonModelOpen, PolygonModelSemiOpen, PolygonModelClosed} {
		boExpect(t, OpUnion, boOptions(func(o *BooleanOptions) { o.PolygonModel = m }), a, b, a)
	}
}

func TestBooleanPointPoint(t *testing.T) {
	boExpect4(t, DefaultBooleanOptions(), "0:0 | 1:0 # #", "0:0 | 2:0 # #",
		"0:0 | 0:0 | 1:0 | 2:0 # #", "0:0 | 0:0 # #", "1:0 # #", "1:0 | 2:0 # #")
}

func TestBooleanPointOpenPolyline(t *testing.T) {
	o := boOptions(func(o *BooleanOptions) { o.PolylineModel = PolylineModelOpen })
	a := "0:0 | 1:0 | 2:0 | 3:0 | 4:0 | 5:0 # #"
	b := "# 0:0, 1:0, 2:0 | 3:0, 3:0 | 4:0, 5:0, 4:0 #"
	boExpect4(t, o, a, b,
		"0:0 | 2:0 | 3:0 | 4:0 # 0:0, 1:0, 2:0 | 3:0, 3:0 | 4:0, 5:0, 4:0 #",
		"1:0 | 5:0 # #",
		"0:0 | 2:0 | 3:0 | 4:0 # #",
		"0:0 | 2:0 | 3:0 | 4:0# 0:0, 1:0, 2:0 | 3:0, 3:0 | 4:0, 5:0, 4:0 #")
}

func TestBooleanPointOpenPolylineLoopBoundariesFalse(t *testing.T) {
	o := boOptions(func(o *BooleanOptions) {
		o.PolylineModel = PolylineModelOpen
		o.PolylineLoopsHaveBoundaries = false
	})
	a := "0:0 | 1:0 | 2:0 | 3:0 | 4:0 | 5:0 # #"
	b := "# 0:0, 1:0, 2:0 | 3:0, 3:0 | 4:0, 5:0, 4:0 #"
	boExpect4(t, o, a, b,
		"0:0 | 2:0 | 3:0 # 0:0, 1:0, 2:0 | 3:0, 3:0 | 4:0, 5:0, 4:0 #",
		"1:0 | 4:0 | 5:0 # #",
		"0:0 | 2:0 | 3:0 # #",
		"0:0 | 2:0 | 3:0 # 0:0, 1:0, 2:0 | 3:0, 3:0 | 4:0, 5:0, 4:0 #")
}

func TestBooleanPointSemiOpenPolyline(t *testing.T) {
	for _, bv := range []bool{false, true} {
		o := boOptions(func(o *BooleanOptions) {
			o.PolylineModel = PolylineModelSemiOpen
			o.PolylineLoopsHaveBoundaries = bv
		})
		a := "0:0 | 1:0 | 2:0 | 3:0 | 4:0 | 5:0 # #"
		b := "# 0:0, 1:0, 2:0 | 3:0, 3:0 | 4:0, 5:0, 4:0 #"
		boExpect4(t, o, a, b,
			"2:0 | 3:0 # 0:0, 1:0, 2:0 | 3:0, 3:0 | 4:0, 5:0, 4:0 #",
			"0:0 | 1:0 | 4:0 | 5:0 # #",
			"2:0 | 3:0 # #",
			"2:0 | 3:0 # 0:0, 1:0, 2:0 | 3:0, 3:0 | 4:0, 5:0, 4:0 #")
	}
}

func TestBooleanPointClosedPolyline(t *testing.T) {
	for _, bv := range []bool{false, true} {
		o := boOptions(func(o *BooleanOptions) {
			o.PolylineModel = PolylineModelClosed
			o.PolylineLoopsHaveBoundaries = bv
		})
		a := "0:0 | 1:0 | 2:0 | 3:0 | 4:0 | 5:0 # #"
		b := "# 0:0, 1:0, 2:0 | 3:0, 3:0 | 4:0, 5:0, 4:0 #"
		boExpect4(t, o, a, b,
			"# 0:0, 1:0, 2:0 | 3:0, 3:0 | 4:0, 5:0, 4:0 #",
			"0:0 | 1:0 | 2:0 | 3:0 | 4:0 | 5:0 # #",
			"# #",
			"# 0:0, 1:0, 2:0 | 3:0, 3:0 | 4:0, 5:0, 4:0 #")
	}
}

func TestBooleanPointPolygonInterior(t *testing.T) {
	boExpect4(t, DefaultBooleanOptions(), "1:1 | 4:4 # #", "# # 0:0, 0:3, 3:0",
		"4:4 # # 0:0, 0:3, 3:0", "1:1 # #", "4:4 # #", "4:4 # # 0:0, 0:3, 3:0")
}

func TestBooleanPointOpenPolygonVertex(t *testing.T) {
	o := boOptions(func(o *BooleanOptions) { o.PolygonModel = PolygonModelOpen })
	boExpect4(t, o, "0:1 | 1:0 # #", "# # 0:0, 0:1, 1:0",
		"0:1 | 1:0 # # 0:0, 0:1, 1:0", "# #", "0:1 | 1:0 # #", "0:1 | 1:0 # # 0:0, 0:1, 1:0")
}

func TestBooleanPointSemiOpenPolygonVertex(t *testing.T) {
	o := boOptions(func(o *BooleanOptions) { o.PolygonModel = PolygonModelSemiOpen })
	boExpect4(t, o, "0:1 | 1:0 # #", "# # 0:0, 0:1, 1:0",
		"1:0 # # 0:0, 0:1, 1:0", "0:1 # #", "1:0 # #", "1:0 # # 0:0, 0:1, 1:0")
}

func TestBooleanPointClosedPolygonVertex(t *testing.T) {
	o := boOptions(func(o *BooleanOptions) { o.PolygonModel = PolygonModelClosed })
	boExpect4(t, o, "0:1 | 1:0 # #", "# # 0:0, 0:1, 1:0",
		"# # 0:0, 0:1, 1:0", "0:1 | 1:0 # #", "# #", "# # 0:0, 0:1, 1:0")
}

const (
	boPolylineVertexA = "# 0:0, 0:1, 0:2 | 0:3, 0:4, 0:3 #"
	boPolylineVertexB = "# 0:0, 1:0 | -1:1, 0:1, 1:1 | -1:2, 0:2 | 1:3, 0:3, 1:3 | 0:4, 1:4, 0:4 #"
	boPolylineVertexU = "# 0:0, 0:1, 0:2 | 0:0, 1:0 | -1:1, 0:1, 1:1 | -1:2, 0:2 | 0:3, 0:4, 0:3 | 1:3, 0:3, 1:3 | 0:4, 1:4, 0:4 #"
)

func TestBooleanPolylineVertexOpenPolylineVertex(t *testing.T) {
	o := boOptions(func(o *BooleanOptions) { o.PolylineModel = PolylineModelOpen })
	boExpect4(t, o, boPolylineVertexA, boPolylineVertexB, boPolylineVertexU,
		"# 0:1, 0:1 | 0:1, 0:1 #", "# 0:0, 0:1, 0:2 | 0:3, 0:4, 0:3 #", boPolylineVertexU)
}

func TestBooleanPolylineVertexOpenPolylineVertexLoopBoundariesFalse(t *testing.T) {
	o := boOptions(func(o *BooleanOptions) {
		o.PolylineModel = PolylineModelOpen
		o.PolylineLoopsHaveBoundaries = false
	})
	boExpect4(t, o, boPolylineVertexA, boPolylineVertexB, boPolylineVertexU,
		"# 0:1, 0:1 | 0:1, 0:1 | 0:3, 0:3 | 0:3, 0:3 | 0:4, 0:4 | 0:4, 0:4 #",
		"# 0:0, 0:1, 0:2 | 0:3, 0:4, 0:3 #", boPolylineVertexU)
}

func TestBooleanPolylineVertexSemiOpenPolylineVertex(t *testing.T) {
	for _, bv := range []bool{false, true} {
		o := boOptions(func(o *BooleanOptions) {
			o.PolylineModel = PolylineModelSemiOpen
			o.PolylineLoopsHaveBoundaries = bv
		})
		boExpect4(t, o, boPolylineVertexA, boPolylineVertexB, boPolylineVertexU,
			"# 0:0, 0:0 | 0:0, 0:0 | 0:1, 0:1 | 0:1, 0:1 | 0:3, 0:3 | 0:3, 0:3 | 0:4, 0:4 | 0:4, 0:4 #",
			"# 0:0, 0:1, 0:2 | 0:3, 0:4, 0:3 #", boPolylineVertexU)
	}
}

func TestBooleanPolylineVertexClosedPolylineVertex(t *testing.T) {
	o := boOptions(func(o *BooleanOptions) { o.PolylineModel = PolylineModelClosed })
	boExpect4(t, o, boPolylineVertexA, boPolylineVertexB, boPolylineVertexU,
		"# 0:0, 0:0 | 0:0, 0:0 | 0:1, 0:1 | 0:1, 0:1 | 0:2, 0:2 | 0:2, 0:2 | 0:3, 0:3 | 0:3, 0:3 | 0:3, 0:3 | 0:4, 0:4 | 0:4, 0:4 | 0:4, 0:4 #",
		"# 0:0, 0:1, 0:2 | 0:3, 0:4, 0:3 #", boPolylineVertexU)
}

func TestBooleanPolylineVertexClosedPolylineVertexLoopBoundariesFalse(t *testing.T) {
	o := boOptions(func(o *BooleanOptions) {
		o.PolylineModel = PolylineModelClosed
		o.PolylineLoopsHaveBoundaries = false
	})
	boExpect4(t, o, boPolylineVertexA, boPolylineVertexB, boPolylineVertexU,
		"# 0:0, 0:0 | 0:0, 0:0 | 0:1, 0:1 | 0:1, 0:1 | 0:2, 0:2 | 0:2, 0:2 | 0:3, 0:3 | 0:3, 0:3 | 0:4, 0:4 | 0:4, 0:4 #",
		"# 0:0, 0:1, 0:2 | 0:3, 0:4, 0:3 #", boPolylineVertexU)
}

const boVertexTestPolygon = "0:0, 0:1, 0:2, 0:3, 0:4, 0:5, 5:5, 5:4, 5:3, 5:2, 5:1, 5:0"

const boPolylineVsPolygonA = "# 1:1, 0:1 | 0:2, 1:2 | -1:3, 0:3 | 0:4, -1:4 | 6:1, 5:1 | 5:2, 6:2 | 4:3, 5:3 | 5:4, 4:4 #"

func TestBooleanPolylineVertexOpenPolygonVertex(t *testing.T) {
	o := boOptions(func(o *BooleanOptions) { o.PolygonModel = PolygonModelOpen })
	b := "# # " + boVertexTestPolygon
	diff := "# 0:1, 0:1 | 0:2, 0:2 | -1:3, 0:3 | 0:4, -1:4| 6:1, 5:1 | 5:2, 6:2 | 5:3, 5:3 | 5:4, 5:4 #"
	boExpect4(t, o, boPolylineVsPolygonA, b, diff+boVertexTestPolygon,
		"# 1:1, 0:1 | 0:2, 1:2 | 4:3, 5:3 | 5:4, 4:4 #", diff, diff+boVertexTestPolygon)
}

func TestBooleanPolylineVertexOpenPolygonClosedPolylineVertex(t *testing.T) {
	o := boOptions(func(o *BooleanOptions) { o.PolygonModel = PolygonModelOpen })
	suffix := "-2:0, 0:1 | -2:1, 0:2 | -2:2, 0:3 | -2:3, 0:4 | 7:0, 5:1 | 7:1, 5:2 | 7:2, 5:3 | 7:3, 5:4 # " + boVertexTestPolygon
	b := "# " + suffix
	prefix := "# -1:3, 0:3 | 0:4, -1:4 | 6:1, 5:1 | 5:2, 6:2"
	boExpect4(t, o, boPolylineVsPolygonA, b,
		prefix+" | 0:1, 0:1 | 0:2, 0:2 | 5:3, 5:3 | 5:4, 5:4 | "+suffix,
		"# 1:1, 0:1 | 0:2, 1:2 | 0:3, 0:3 | 0:4, 0:4| 5:1, 5:1 | 5:2, 5:2 | 4:3, 5:3 | 5:4, 4:4| 0:1, 0:1 | 0:2, 0:2 | 0:3, 0:3 | 0:4, 0:4| 5:1, 5:1 | 5:2, 5:2 | 5:3, 5:3 | 5:4, 5:4 #",
		prefix+" #", prefix+" | "+suffix)
}

func TestBooleanPolylineVertexSemiOpenPolygonVertex(t *testing.T) {
	o := boOptions(func(o *BooleanOptions) { o.PolygonModel = PolygonModelSemiOpen })
	b := "# # " + boVertexTestPolygon
	diff := "# -1:3, 0:3 | 0:4, -1:4 | 6:1, 5:1 | 5:2, 6:2 | 5:3, 5:3 | 5:4, 5:4 #"
	boExpect4(t, o, boPolylineVsPolygonA, b, diff+boVertexTestPolygon,
		"# 1:1, 0:1 | 0:2, 1:2 | 0:3, 0:3 | 0:4, 0:4 | 4:3, 5:3 | 5:4, 4:4 #", diff, diff+boVertexTestPolygon)
}

func TestBooleanPolylineVertexClosedPolygonVertex(t *testing.T) {
	o := boOptions(func(o *BooleanOptions) { o.PolygonModel = PolygonModelClosed })
	b := "# # " + boVertexTestPolygon
	diff := "# -1:3, 0:3 | 0:4, -1:4 | 6:1, 5:1 | 5:2, 6:2 #"
	boExpect4(t, o, boPolylineVsPolygonA, b, diff+boVertexTestPolygon,
		"# 1:1, 0:1 | 0:2, 1:2 | 0:3, 0:3 | 0:4, 0:4| 5:1, 5:1 | 5:2, 5:2 | 4:3, 5:3 | 5:4, 4:4 #", diff, diff+boVertexTestPolygon)
}

func TestBooleanPolylineEdgePolylineEdgeCrossing(t *testing.T) {
	boExpect4(t, boRoundToE(1), "# 0:0, 2:2 #", "# 2:0, 0:2 #",
		"# 0:0, 1:1, 2:2 | 2:0, 1:1, 0:2 #", "# 1:1, 1:1 | 1:1, 1:1 #",
		"# 0:0, 1:1, 2:2 #", "# 0:0, 1:1, 2:2 | 2:0, 1:1, 0:2 #")
}

func TestBooleanPolylineEdgePolylineEdgeOverlap(t *testing.T) {
	o := boOptions(func(o *BooleanOptions) { o.PolygonModel = PolygonModelOpen })
	boExpect4(t, o, "# 0:0, 1:0, 2:0, 2:5 | 3:0, 3:0 | 6:0, 5:0, 4:0 #", "# 0:0, 1:0, 2:0 | 3:0, 3:0 | 4:0, 5:0 #",
		"# 0:0, 1:0, 2:0, 2:5 | 0:0, 1:0, 2:0 | 3:0, 3:0 | 3:0, 3:0 | 6:0, 5:0, 4:0 | 4:0, 5:0 #",
		"# 0:0, 1:0, 2:0 | 0:0, 1:0, 2:0 | 3:0, 3:0 | 3:0, 3:0 | 5:0, 4:0 | 4:0, 5:0 #",
		"# 2:0, 2:5 | 6:0, 5:0 #", "# 2:0, 2:5 | 6:0, 5:0 #")
}

const (
	boLoopA = "# 0:0, 0:1, 1:0, 0:0 | 2:2, 2:3, 3:2, 2:2 #"
	boLoopB = "# 0:0, 0:0 | 0:0, 0:1 | 2:2, 2:2 | 2:2, 3:2 #"
	boLoopU = "# 0:0, 0:1, 1:0, 0:0 | 0:0, 0:0 | 0:0, 0:1 | 2:2, 2:3, 3:2, 2:2 | 2:2, 2:2 | 2:2, 3:2 #"
)

func TestBooleanPolylineLoopMultipleOpenPolylineEdge(t *testing.T) {
	o := boOptions(func(o *BooleanOptions) { o.PolylineModel = PolylineModelOpen })
	boExpect4(t, o, boLoopA, boLoopB, boLoopU,
		"# 0:0, 0:1 | 0:0, 0:1 | 2:2, 3:2 | 3:2, 2:2 #",
		"# 0:1, 1:0, 0:0 | 2:2, 2:3, 3:2 #",
		"# 0:1, 1:0, 0:0 | 0:0, 0:0 | 2:2, 2:3, 3:2 | 2:2, 2:2 #")
}

func TestBooleanPolylineLoopMultipleSemiOpenPolylineEdge(t *testing.T) {
	o := boOptions(func(o *BooleanOptions) { o.PolylineModel = PolylineModelSemiOpen })
	boExpect4(t, o, boLoopA, boLoopB, boLoopU,
		"# 0:0, 0:0 | 0:0, 0:1 | 0:0, 0:1 | 2:2, 2:2 | 2:2, 2:2 | 2:2, 3:2 | 3:2, 2:2 #",
		"# 0:1, 1:0, 0:0 | 2:2, 2:3, 3:2 #", "# 0:1, 1:0, 0:0 | 2:2, 2:3, 3:2 #")
}

func TestBooleanPolylineLoopMultipleClosedPolylineEdge(t *testing.T) {
	o := boOptions(func(o *BooleanOptions) { o.PolylineModel = PolylineModelClosed })
	boExpect4(t, o, boLoopA, boLoopB, boLoopU,
		"# 0:0, 0:0 | 0:0, 0:0 | 0:0, 0:1 | 0:0, 0:1 | 2:2, 2:2 | 2:2, 2:2 | 2:2, 3:2 | 3:2, 2:2 #",
		"# 0:1, 1:0, 0:0 | 2:2, 2:3, 3:2 #", "# 0:1, 1:0, 0:0 | 2:2, 2:3, 3:2 #")
}

func TestBooleanPolylineLoopMultiplePolylineEdgeLoopBoundariesFalse(t *testing.T) {
	for _, m := range []PolylineModel{PolylineModelOpen, PolylineModelSemiOpen, PolylineModelClosed} {
		o := boOptions(func(o *BooleanOptions) {
			o.PolylineModel = m
			o.PolylineLoopsHaveBoundaries = false
		})
		boExpect4(t, o, boLoopA, boLoopB, boLoopU,
			"# 0:0, 0:0 | 0:0, 0:1 | 0:0, 0:1 | 2:2, 2:2 | 2:2, 3:2 | 3:2, 2:2 #",
			"# 0:1, 1:0, 0:0 | 2:2, 2:3, 3:2 #", "# 0:1, 1:0, 0:0 | 2:2, 2:3, 3:2 #")
	}
}

func TestBooleanPolylineEdgeOpenPolygonEdgeOverlap(t *testing.T) {
	o := boOptions(func(o *BooleanOptions) { o.PolygonModel = PolygonModelOpen })
	boExpect4(t, o, "# 1:1, 1:3, 3:3 | 3:3, 1:3 # ", "# # 1:1, 1:3, 3:3, 3:1",
		"# 1:1, 1:3, 3:3 | 3:3, 1:3 # 1:1, 1:3, 3:3, 3:1", "# #",
		"# 1:1, 1:3, 3:3 | 3:3, 1:3 #", "# 1:1, 1:3, 3:3 | 3:3, 1:3 # 1:1, 1:3, 3:3, 3:1")
}

func TestBooleanPolylineEdgeSemiOpenPolygonEdgeOverlap(t *testing.T) {
	o := boOptions(func(o *BooleanOptions) { o.PolygonModel = PolygonModelSemiOpen })
	boExpect4(t, o, "# 1:1, 1:3, 3:3 | 3:3, 1:3 # ", "# # 1:1, 1:3, 3:3, 3:1",
		"# 1:1, 1:1 | 3:3, 3:3 | 3:3, 1:3 # 1:1, 1:3, 3:3, 3:1", "# 1:3, 1:3 | 1:1, 1:3, 3:3 #",
		"# 1:1, 1:1 | 3:3, 3:3 | 3:3, 1:3 #", "# 1:1, 1:1 | 3:3, 3:3 | 3:3, 1:3 # 1:1, 1:3, 3:3, 3:1")
}

func TestBooleanPolylineEdgeClosedPolygonEdgeOverlap(t *testing.T) {
	o := boOptions(func(o *BooleanOptions) { o.PolygonModel = PolygonModelClosed })
	boExpect4(t, o, "# 1:1, 1:3, 3:3 | 3:3, 1:3 # ", "# # 1:1, 1:3, 3:3, 3:1",
		"# # 1:1, 1:3, 3:3, 3:1", "# 1:1, 1:3, 3:3 | 3:3, 1:3 #", "# #", "# # 1:1, 1:3, 3:3, 3:1")
}

func TestBooleanPolygonVertexMatching(t *testing.T) {
	o := boOptions(func(o *BooleanOptions) {
		o.PolylineModel = PolylineModelClosed
		o.PolygonModel = PolygonModelClosed
	})
	boExpect(t, OpUnion, o, "# 0:0, 1:1 # ", "# # 0:0, 1:1", "# # 0:0, 1:1")
}

func TestBooleanPolylineEdgePolygonInterior(t *testing.T) {
	boExpect4(t, DefaultBooleanOptions(), "# 1:1, 2:2 | 3:3, 3:3 | 6:6, 7:7 | 8:8, 8:8 # ", "# # 0:0, 0:5, 5:5, 5:0",
		"# 6:6, 7:7 | 8:8, 8:8 # 0:0, 0:5, 5:5, 5:0", "# 1:1, 2:2 | 3:3, 3:3 #",
		"# 6:6, 7:7 | 8:8, 8:8 #", "# 6:6, 7:7 | 8:8, 8:8 # 0:0, 0:5, 5:5, 5:0")
}

func TestBooleanPolylineEdgeIsolatedStartVertexPlusInteriorCrossing(t *testing.T) {
	boExpect(t, OpDifference, boRoundToE(1), "# 0:0, 0:10, 0:4 # ", "# # 0:0, -5:5, 5:5", "# 0:0, 0:0 | 0:5, 0:10, 0:5 #")
}

func TestBooleanPolygonEdgeIsolatedStartVertexPlusInteriorCrossing(t *testing.T) {
	o := boRoundToE(1)
	o.PolygonModel = PolygonModelClosed
	boExpect(t, OpIntersection, o, "# # 0:0, 5:5, -5:5", "# # 1:4, 0:0, 0:8", "# # 0:0; 0:5, 0:8, 0.8:5")
}

func TestBooleanPolygonVertexOpenPolygonVertex(t *testing.T) {
	o := boOptions(func(o *BooleanOptions) { o.PolygonModel = PolygonModelOpen })
	a, b := "# # 0:0, 0:5, 1:5, 0:0, 2:5, 3:5", "# # 0:0, 5:3, 5:2"
	boExpect4(t, o, a, b, "# # 0:0, 0:5, 1:5, 0:0, 2:5, 3:5, 0:0, 5:3, 5:2", "# #",
		"# # 0:0, 0:5, 1:5, 0:0, 2:5, 3:5", "# # 0:0, 0:5, 1:5, 0:0, 2:5, 3:5, 0:0, 5:3, 5:2")
}

func TestBooleanPolygonVertexSemiOpenPolygonVertex(t *testing.T) {
	o := boOptions(func(o *BooleanOptions) { o.PolygonModel = PolygonModelSemiOpen })
	a, b := "# # 0:0, 0:5, 1:5, 0:0, 2:5, 3:5", "# # 0:0, 5:3, 5:2"
	boExpect4(t, o, a, b, "# # 0:0, 0:5, 1:5, 0:0, 2:5, 3:5, 0:0, 5:3, 5:2", "# #",
		"# # 0:0, 0:5, 1:5, 0:0, 2:5, 3:5", "# # 0:0, 0:5, 1:5, 0:0, 2:5, 3:5, 0:0, 5:3, 5:2")
}

func TestBooleanPolygonVertexClosedPolygonVertex(t *testing.T) {
	o := boOptions(func(o *BooleanOptions) { o.PolygonModel = PolygonModelClosed })
	a, b := "# # 0:0, 0:5, 1:5, 0:0, 2:5, 3:5", "# # 0:0, 5:3, 5:2"
	boExpect4(t, o, a, b, "# # 0:0, 0:5, 1:5, 0:0, 2:5, 3:5, 0:0, 5:3, 5:2", "# # 0:0",
		"# # 0:0, 0:5, 1:5, 0:0, 2:5, 3:5", "# # 0:0, 0:5, 1:5, 0:0, 2:5, 3:5, 0:0, 5:3, 5:2")
	boExpect(t, OpDifference, o, b, a, "# # 0:0, 5:3, 5:2")
}

func TestBooleanPolygonEdgePolygonEdgeCrossing(t *testing.T) {
	boExpect4(t, boRoundToE(2), "# # 0:0, 0:2, 2:2, 2:0", "# # 1:1, 1:3, 3:3, 3:1",
		"# # 0:0, 0:2, 1:2, 1:3, 3:3, 3:1, 2:1, 2:0", "# # 1:1, 1:2, 2:2, 2:1",
		"# # 0:0, 0:2, 1:2, 1:1, 2:1, 2:0", "# # 0:0, 0:2, 1:2, 1:1, 2:1, 2:0; 1:2, 1:3, 3:3, 3:1, 2:1, 2:2")
}

func TestBooleanPolygonEdgeOpenPolygonEdgeOverlap(t *testing.T) {
	o := boOptions(func(o *BooleanOptions) { o.PolygonModel = PolygonModelOpen })
	boExpect4(t, o, "# # 0:0, 0:4, 2:4, 2:0", "# # 0:0, 1:1, 2:0; 0:4, 1:5, 2:4",
		"# # 0:0, 0:4, 2:4, 2:0; 0:4, 1:5, 2:4", "# # 0:0, 1:1, 2:0",
		"# # 0:0, 0:4, 2:4, 2:0, 1:1", "# # 0:0, 0:4, 2:4, 2:0, 1:1; 0:4, 1:5, 2:4")
}

func TestBooleanPolygonEdgeSemiOpenPolygonEdgeOverlap(t *testing.T) {
	o := boOptions(func(o *BooleanOptions) { o.PolygonModel = PolygonModelSemiOpen })
	boExpect4(t, o, "# # 0:0, 0:4, 2:4, 2:0", "# # 0:0, 1:1, 2:0; 0:4, 1:5, 2:4",
		"# # 0:0, 0:4, 1:5, 2:4, 2:0", "# # 0:0, 1:1, 2:0",
		"# # 0:0, 0:4, 2:4, 2:0, 1:1", "# # 0:0, 0:4, 2:4, 2:0, 1:1; 0:4, 1:5, 2:4")
}

func TestBooleanPolygonEdgeClosedPolygonEdgeOverlap(t *testing.T) {
	o := boOptions(func(o *BooleanOptions) { o.PolygonModel = PolygonModelClosed })
	boExpect4(t, o, "# # 0:0, 0:4, 2:4, 2:0", "# # 0:0, 1:1, 2:0; 0:4, 1:5, 2:4",
		"# # 0:0, 0:4, 1:5, 2:4, 2:0", "# # 0:0, 1:1, 2:0; 0:4, 2:4",
		"# # 0:0, 0:4, 2:4, 2:0, 1:1", "# # 0:0, 0:4, 2:4, 2:0, 1:1; 0:4, 1:5, 2:4")
}

func TestBooleanPolygonPolygonInterior(t *testing.T) {
	boExpect4(t, DefaultBooleanOptions(), "# # 0:0, 0:4, 4:4, 4:0", "# # 1:1, 1:2, 2:2, 2:1; 5:5, 5:6, 6:6, 6:5",
		"# # 0:0, 0:4, 4:4, 4:0; 5:5, 5:6, 6:6, 6:5", "# # 1:1, 1:2, 2:2, 2:1",
		"# # 0:0, 0:4, 4:4, 4:0; 2:1, 2:2, 1:2, 1:1",
		"# # 0:0, 0:4, 4:4, 4:0; 2:1, 2:2, 1:2, 1:1; 5:5, 5:6, 6:6, 6:5")
}

func TestBooleanPolygonEdgesDegenerateAfterSnapping(t *testing.T) {
	boExpect4(t, boRoundToE(0), "# # 0:-1, 0:1, 0.1:1, 0.1:-1", "# # -1:0.1, 1:0.1, 1:0, -1:0",
		"# # 0:-1, 0:0, 0:1, 0:0 | -1:0, 0:0, 1:0, 0:0", "# # 0:0",
		"# # 0:-1, 0:0, 0:1, 0:0", "# # 0:-1, 0:0, 0:1, 0:0 | -1:0, 0:0, 1:0, 0:0")
}

func TestBooleanThreeOverlappingBars(t *testing.T) {
	boExpect4(t, boRoundToE(2), "# # 0:0, 0:2, 3:2, 3:0; 0:3, 0:5, 3:5, 3:3", "# # 1:1, 1:4, 2:4, 2:1",
		"# # 0:0, 0:2, 1:2, 1:3, 0:3, 0:5, 3:5, 3:3, 2:3, 2:2, 3:2, 3:0",
		"# # 1:1, 1:2, 2:2, 2:1; 1:3, 1:4, 2:4, 2:3",
		"# # 0:0, 0:2, 1:2, 1:1, 2:1, 2:2, 3:2, 3:0; 0:3, 0:5, 3:5, 3:3, 2:3, 2:4, 1:4, 1:3",
		"# # 0:0, 0:2, 1:2, 1:1, 2:1, 2:2, 3:2, 3:0; 0:3, 0:5, 3:5, 3:3, 2:3, 2:4, 1:4, 1:3; 1:2, 1:3, 2:3, 2:2")
}

func TestBooleanFourOverlappingBars(t *testing.T) {
	boExpect4(t, boRoundToE(2), "# # 1:88, 1:93, 2:93, 2:88; -1:88, -1:93, 0:93, 0:88", "# # -2:89, -2:90, 3:90, 3:89; -2:91, -2:92, 3:92, 3:91",
		"# # -1:88, -1:89, -2:89, -2:90, -1:90, -1:91, -2:91, -2:92, -1:92, -1:93, 0:93, 0:92, 1:92, 1:93, 2:93, 2:92, 3:92, 3:91, 2:91, 2:90, 3:90, 3:89, 2:89, 2:88, 1:88, 1:89, 0:89, 0:88; 0:90, 1:90, 1:91, 0:91",
		"# # 1:89, 1:90, 2:90, 2:89; 1:91, 1:92, 2:92, 2:91; -1:89, -1:90, 0:90, 0:89; -1:91, -1:92, 0:92, 0:91",
		"# # 1:88, 1:89, 2:89, 2:88; 1:90, 1:91, 2:91, 2:90; 1:92, 1:93, 2:93, 2:92; -1:88, -1:89, 0:89, 0:88; -1:90, -1:91, 0:91, 0:90; -1:92, -1:93, 0:93, 0:92",
		"# # 1:88, 1:89, 2:89, 2:88; -1:88, -1:89, 0:89, 0:88; 1:90, 1:91, 2:91, 2:90; -1:90, -1:91, 0:91, 0:90; 1:92, 1:93, 2:93, 2:92; -1:92, -1:93, 0:93, 0:92; -2:89, -2:90, -1:90, -1:89; -2:91, -2:92, -1:92, -1:91; 0:89, 0:90, 1:90, 1:89; 0:91, 0:92, 1:92, 1:91; 2:89, 2:90, 3:90, 3:89; 2:91, 2:92, 3:92, 3:91")
}

func TestBooleanOverlappingDoughnuts(t *testing.T) {
	boExpect4(t, boRoundToE(1), "# # -1:-93, -1:-89, 3:-89, 3:-93; 0:-92, 2:-92, 2:-90, 0:-90", "# # -3:-91, -3:-87, 1:-87, 1:-91; -2:-90, 0:-90, 0:-88, -2:-88",
		"# # -1:-93, -1:-91, -3:-91, -3:-87, 1:-87, 1:-89, 3:-89, 3:-93; 0:-92, 2:-92, 2:-90, 1:-90, 1:-91, 0:-91; -2:-90, -1:-90, -1:-89, 0:-89, 0:-88, -2:-88",
		"# # -1:-91, -1:-90, 0:-90, 0:-91; 0:-90, 0:-89, 1:-89, 1:-90",
		"# # -1:-93, -1:-91, 0:-91, 0:-92, 2:-92, 2:-90, 1:-90, 1:-89, 3:-89, 3:-93; -1:-90, -1:-89, 0:-89, 0:-90",
		"# # -1:-93, -1:-91, 0:-91, 0:-92, 2:-92, 2:-90, 1:-90, 1:-89, 3:-89, 3:-93; -3:-91, -3:-87, 1:-87, 1:-89, 0:-89, 0:-88,-2:-88,-2:-90,-1:-90,-1:-91; -1:-90, -1:-89, 0:-89, 0:-90; 1:-91, 0:-91, 0:-90, 1:-90")
}

func TestBooleanPolylineEnteringRectangle(t *testing.T) {
	boExpect4(t, boRoundToE(1), "# 0:0, 2:2 #", "# # 1:1, 1:3, 3:3, 3:1",
		"# 0:0, 1:1 # 1:1, 1:3, 3:3, 3:1", "# 1:1, 2:2 #", "# 0:0, 1:1 #", "# 0:0, 1:1 # 1:1, 1:3, 3:3, 3:1")
}

func TestBooleanPolylineCrossingRectangleTwice(t *testing.T) {
	boExpect4(t, boRoundToE(1), "# 0:-5, 0:5, 5:0, -5:0 #", "# # 1:1, 1:-1, -1:-1, -1:1",
		"# 0:-5, 0:-1 | 0:1, 0:5, 5:0, 1:0 | -1:0, -5:0 # 1:1, 1:0, 1:-1, 0:-1, -1:-1, -1:0, -1:1, 0:1",
		"# 0:-1, 0:1 | 1:0, -1:0 #",
		"# 0:-5, 0:-1 | 0:1, 0:5, 5:0, 1:0 | -1:0, -5:0 #",
		"# 0:-5, 0:-1 | 0:1, 0:5, 5:0, 1:0 | -1:0, -5:0 # 1:1, 1:0, 1:-1, 0:-1, -1:-1, -1:0, -1:1, 0:1")
}

func TestBooleanSelfIntersectingPolylines(t *testing.T) {
	// Note that the polylines are split at their mutual crossing (2:4) by
	// default; split_all_crossing_polyline_edges also splits them at their
	// self-crossings.
	o := boRoundToE(1)
	a, b := "# 0:2, 4:2, 2:0, 2:5 #", "# 0:4, 5:4, 3:6, 3:3 #"
	boExpect4(t, o, a, b,
		"# 0:2, 4:2, 2:0, 2:4, 2:5 | 0:4, 2:4, 5:4, 3:6, 3:3 #", "# 2:4, 2:4 | 2:4, 2:4 #",
		"# 0:2, 4:2, 2:0, 2:4, 2:5 #", "# 0:2, 4:2, 2:0, 2:4, 2:5 | 0:4, 2:4, 5:4, 3:6, 3:3 #")
	o.SplitAllCrossingPolylineEdges = true
	boExpect4(t, o, a, b,
		"# 0:2, 2:2, 4:2, 2:0, 2:2, 2:4, 2:5 | 0:4, 2:4, 3:4, 5:4, 3:6, 3:4, 3:3 #", "# 2:4, 2:4 | 2:4, 2:4 #",
		"# 0:2, 2:2, 4:2, 2:0, 2:2, 2:4, 2:5 #", "# 0:2, 2:2, 4:2, 2:0, 2:2, 2:4, 2:5 | 0:4, 2:4, 3:4, 5:4, 3:6, 3:4, 3:3 #")
}

// boPolygonResultLayer records whether a polygon graph (degenerate edges
// and sibling pairs discarded, as by LaxPolygonLayer with
// DegenerateBoundaries::DISCARD) is empty, full, or has edges.
type boPolygonResultLayer struct {
	numEdges int
	full     bool
}

func (l *boPolygonResultLayer) GraphOptions() GraphOptions {
	return GraphOptions{EdgeTypeDirected, DegenerateEdgesDiscard, DuplicateEdgesKeep, SiblingPairsDiscard, true}
}

func (l *boPolygonResultLayer) Build(g *Graph) error {
	l.numEdges = g.NumEdges()
	if l.numEdges == 0 {
		full, err := g.IsFullPolygon()
		if err != nil {
			return err
		}
		l.full = full
	}
	return nil
}

// boExpectPolygon is ExpectPolygon for results that are empty ("") or full
// ("full"), with IdentitySnapFunction(1.1 degrees).
func boExpectPolygon(t *testing.T, opType OpType, aStr, bStr, expected string) {
	t.Helper()
	a := boMakeIndex(t, "# # "+aStr)
	b := boMakeIndex(t, "# # "+bStr)
	var out boPolygonResultLayer
	o := DefaultBooleanOptions()
	o.SnapFunction = s2.NewIdentitySnapper(1.1 * s1.Degree)
	if err := NewBooleanOperation(opType, []Layer{&out}, o).Build(a, b); err != nil {
		t.Fatalf("%v %q %q: %v", opType, aStr, bStr, err)
	}
	got := ""
	switch {
	case out.numEdges > 0:
		got = fmt.Sprintf("%d edges", out.numEdges)
	case out.full:
		got = "full"
	}
	if got != expected {
		t.Errorf("%v(%q, %q) = %q, want %q", opType, aStr, bStr, got, expected)
	}
}

func TestBooleanFullAndEmptyResults(t *testing.T) {
	kEmpty, kFull := "", "full"
	kShell1 := "10:0, 10:10, 20:10"
	kHole1 := "10:0, 20:10, 10:10"
	kShell1Minus := "11:2, 11:9, 18:9"
	kShell1Plus := "9:-2, 9:11, 22:11"
	kShell2 := "10:20, 10:30, 20:30"
	kHole2 := "10:20, 20:30, 10:30"
	kNorthHemi := "0:0, 0:120, 0:-120"
	kSouthHemi := "0:0, 0:-120, 0:120"
	kSouthHemiPlus := "0.5:0, 0.5:-120, 0.5:120"
	k6FaceShell1 := "0:-45, 45:0, 45:90, 0:135, -45:180, -45:-90"
	k6FaceHole1 := "0:-45, -45:-90, -45:180, 0:135, 45:90, 45:0"
	k6FaceShell1Minus := "-1:-45, 44:0, 44:90, -1:135, -46:180, -46:-90"
	k6FaceShell1Plus := "1:-45, 46:0, 46:90, 1:135, -44:180, -44:-90"
	kAlmostEmpty1 := "2:0, 2:10, 3:0"
	kAlmostFull1 := "2:0, 3:0, 2:10"
	kAlmostEmpty2 := "4:0, 4:10, 5:0"
	kAlmostFull2 := "4:0, 5:0, 4:10"
	k6FaceAlmostEmpty1 := k6FaceShell1Minus + "; " + k6FaceHole1

	boExpectPolygon(t, OpUnion, kEmpty, kEmpty, kEmpty)
	boExpectPolygon(t, OpUnion, kAlmostEmpty1, kAlmostEmpty2, kEmpty)
	boExpectPolygon(t, OpUnion, k6FaceAlmostEmpty1, k6FaceAlmostEmpty1, kEmpty)
	boExpectPolygon(t, OpUnion, kEmpty, kFull, kFull)
	boExpectPolygon(t, OpUnion, kFull, kFull, kFull)
	boExpectPolygon(t, OpUnion, kFull, kShell1, kFull)
	boExpectPolygon(t, OpUnion, kHole1, kHole2, kFull)
	boExpectPolygon(t, OpUnion, kHole1, kShell1, kFull)
	boExpectPolygon(t, OpUnion, kHole1, kShell1Minus, kFull)
	boExpectPolygon(t, OpUnion, k6FaceHole1, k6FaceShell1Minus, kFull)

	boExpectPolygon(t, OpIntersection, kEmpty, kEmpty, kEmpty)
	boExpectPolygon(t, OpIntersection, kEmpty, kFull, kEmpty)
	boExpectPolygon(t, OpIntersection, kFull, kEmpty, kEmpty)
	boExpectPolygon(t, OpIntersection, kEmpty, kHole1, kEmpty)
	boExpectPolygon(t, OpIntersection, kShell1, kShell2, kEmpty)
	boExpectPolygon(t, OpIntersection, kShell1, kHole1, kEmpty)
	boExpectPolygon(t, OpIntersection, k6FaceShell1, k6FaceHole1, kEmpty)
	boExpectPolygon(t, OpIntersection, kShell1Plus, kHole1, kEmpty)
	boExpectPolygon(t, OpIntersection, k6FaceShell1Plus, k6FaceHole1, kEmpty)
	boExpectPolygon(t, OpIntersection, kFull, kFull, kFull)
	boExpectPolygon(t, OpIntersection, kAlmostFull1, kAlmostFull2, kFull)

	boExpectPolygon(t, OpDifference, kEmpty, kEmpty, kEmpty)
	boExpectPolygon(t, OpDifference, kEmpty, kFull, kEmpty)
	boExpectPolygon(t, OpDifference, kFull, kFull, kEmpty)
	boExpectPolygon(t, OpDifference, kEmpty, kShell1, kEmpty)
	boExpectPolygon(t, OpDifference, kShell1, kFull, kEmpty)
	boExpectPolygon(t, OpDifference, kShell1, kShell1, kEmpty)
	boExpectPolygon(t, OpDifference, kShell1, kHole2, kEmpty)
	boExpectPolygon(t, OpDifference, k6FaceShell1, k6FaceShell1Plus, kEmpty)
	boExpectPolygon(t, OpDifference, kShell1Plus, kShell1, kEmpty)
	boExpectPolygon(t, OpDifference, k6FaceShell1Plus, k6FaceShell1, kEmpty)
	boExpectPolygon(t, OpDifference, kFull, kEmpty, kFull)
	boExpectPolygon(t, OpDifference, kAlmostFull1, kAlmostEmpty2, kFull)

	boExpectPolygon(t, OpSymmetricDifference, kEmpty, kEmpty, kEmpty)
	boExpectPolygon(t, OpSymmetricDifference, kFull, kFull, kEmpty)
	boExpectPolygon(t, OpSymmetricDifference, kShell1, kShell1, kEmpty)
	boExpectPolygon(t, OpSymmetricDifference, kNorthHemi, kNorthHemi, kEmpty)
	boExpectPolygon(t, OpSymmetricDifference, k6FaceShell1, k6FaceShell1, kEmpty)
	boExpectPolygon(t, OpSymmetricDifference, kShell1Plus, kShell1, kEmpty)
	boExpectPolygon(t, OpSymmetricDifference, k6FaceShell1Plus, k6FaceShell1, kEmpty)
	boExpectPolygon(t, OpSymmetricDifference, k6FaceShell1Minus, k6FaceShell1, kEmpty)
	boExpectPolygon(t, OpSymmetricDifference, kFull, kEmpty, kFull)
	boExpectPolygon(t, OpSymmetricDifference, kEmpty, kFull, kFull)
	boExpectPolygon(t, OpSymmetricDifference, kShell1, kHole1, kFull)
	boExpectPolygon(t, OpSymmetricDifference, kAlmostEmpty1, kAlmostFull1, kFull)
	boExpectPolygon(t, OpSymmetricDifference, kShell1Plus, kHole1, kFull)
	boExpectPolygon(t, OpSymmetricDifference, kAlmostFull1, kAlmostEmpty2, kFull)
	boExpectPolygon(t, OpSymmetricDifference, kNorthHemi, kSouthHemi, kFull)
	boExpectPolygon(t, OpSymmetricDifference, kNorthHemi, kSouthHemiPlus, kFull)
}

func boTestEqual(t *testing.T, a, b string) bool {
	return BooleanEquals(boMakeIndex(t, a), boMakeIndex(t, b), DefaultBooleanOptions())
}

func TestBooleanEquals(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"# #", "# #", true},
		{"# # full", "# # full", true},
		{"# #", "# # full", false},
		{"0:0 # #", "# #", false},
		{"0:0 # #", "# # full", false},
		{"# 0:0, 1:1 #", "# #", false},
		{"# 0:0, 1:1 #", "# # full", false},
		{"# # 0:0, 0:1, 1:0 ", "# #", false},
		{"# # 0:0, 0:1, 1:0 ", "# # full", false},
	} {
		if got := boTestEqual(t, c.a, c.b); got != c.want {
			t.Errorf("Equals(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestBooleanContainsAndIntersectsEmptyAndFull(t *testing.T) {
	empty := boMakeIndex(t, "# #")
	full := boMakeIndex(t, "# # full")
	o := DefaultBooleanOptions()
	if !BooleanContains(empty, empty, o) || BooleanContains(empty, full, o) || !BooleanContains(full, empty, o) || !BooleanContains(full, full, o) {
		t.Error("Contains with empty and full")
	}
	if BooleanIntersects(empty, empty, o) || BooleanIntersects(empty, full, o) || BooleanIntersects(full, empty, o) || !BooleanIntersects(full, full, o) {
		t.Error("Intersects with empty and full")
	}
}

func TestBooleanOptionsFieldsCopied(t *testing.T) {
	o := DefaultBooleanOptions()
	o.PolylineLoopsHaveBoundaries = false
	if NewBooleanOperation(OpUnion, nil, o).Options().PolylineLoopsHaveBoundaries {
		t.Error("polyline_loops_have_boundaries not copied")
	}
}

// boUnionLoops is ComputeTestUnion: the union of two lax polygons must
// build and be non-empty.
func boUnionLoops(t *testing.T, aLoops, bLoops [][]s2.Point, snapRadius s1.Angle) {
	t.Helper()
	a, b := s2.NewShapeIndex(), s2.NewShapeIndex()
	a.Add(s2.LaxPolygonFromPoints(aLoops))
	b.Add(s2.LaxPolygonFromPoints(bLoops))
	var out boPolygonResultLayer
	o := DefaultBooleanOptions()
	o.SnapFunction = s2.NewIdentitySnapper(snapRadius)
	if err := NewBooleanOperation(OpUnion, []Layer{&out}, o).Build(a, b); err != nil {
		t.Fatal(err)
	}
	if out.numEdges == 0 && !out.full {
		t.Error("union is empty")
	}
}

func boPts(vs ...[3]float64) []s2.Point {
	var out []s2.Point
	for _, v := range vs {
		out = append(out, s2.PointFromCoords(v[0], v[1], v[2]))
	}
	return out
}

// kIntersectionMergeRadius is S2::kIntersectionMergeRadius.
const boIntersectionMergeRadius = s1.Angle(2 * kIntersectionError)

func TestBooleanGetCrossedVertexIndexBug1(t *testing.T) {
	boUnionLoops(t, [][]s2.Point{boPts(
		[3]float64{-0.38306437985388492, -0.74921955334206214, 0.54030708099846292},
		[3]float64{-0.3830643798552798, -0.74921955334134249, 0.5403070809984718},
		[3]float64{-0.38306437985529124, -0.74921955334136414, 0.54030708099843361},
		[3]float64{-0.38306437985389635, -0.74921955334208379, 0.54030708099842473},
	)}, [][]s2.Point{boPts(
		[3]float64{-0.38306437985390962, -0.74921955334210588, 0.54030708099838465},
		[3]float64{-0.38306437985527797, -0.74921955334134205, 0.54030708099847369},
		[3]float64{-0.38306437985527941, -0.74921955334134405, 0.54030708099847014},
		[3]float64{-0.38306437985391095, -0.74921955334210777, 0.54030708099838098},
	)}, boIntersectionMergeRadius)
}

func TestBooleanGetCrossedVertexIndexBug2(t *testing.T) {
	boUnionLoops(t, [][]s2.Point{boPts(
		[3]float64{-0.3837392878495085, -0.7477800800281974, 0.5418201831546835},
		[3]float64{-0.38373928785696076, -0.7477800800212292, 0.54182018315902258},
		[3]float64{-0.38373928785701278, -0.74778008002124685, 0.5418201831589613},
		[3]float64{-0.38373928785703426, -0.7477800800212544, 0.54182018315893576},
		[3]float64{-0.38373947205489456, -0.74778014227795497, 0.5418199667802881},
		[3]float64{-0.38373947204434411, -0.74778014228781997, 0.54181996677414512},
		[3]float64{-0.38373947205872994, -0.74778014228185352, 0.54181996677219124},
		[3]float64{-0.38373947218468357, -0.74778014288930306, 0.54181996584462788},
		[3]float64{-0.3837396702525171, -0.74778021044361542, 0.54181973233114322},
		[3]float64{-0.38373967023137123, -0.74778021046333043, 0.54181973231891067},
		[3]float64{-0.38373947216030285, -0.74778014290791484, 0.54181996583620895},
		[3]float64{-0.38373947217087578, -0.74778014289805739, 0.54181996584232528},
		[3]float64{-0.38373947215649007, -0.74778014290402395, 0.54181996584427927},
		[3]float64{-0.3837394720305386, -0.74778014229658485, 0.5418199667718262},
		[3]float64{-0.38373928783585998, -0.74778008004095942, 0.54182018314673686},
		[3]float64{-0.38373928784641037, -0.7477800800310942, 0.54182018315287972},
		[3]float64{-0.38373928783578648, -0.74778008004093421, 0.54182018314682368},
		[3]float64{-0.383739287835765, -0.74778008004092666, 0.54182018314684921},
	)}, [][]s2.Point{boPts(
		[3]float64{-0.38373923813692823, -0.7477800632164362, 0.54182024156551456},
		[3]float64{-0.3837392878569364, -0.74778008002122087, 0.54182018315905123},
		[3]float64{-0.38373928784640354, -0.74778008003106944, 0.54182018315291858},
		[3]float64{-0.38373928784638789, -0.74778008003108642, 0.54182018315290648},
		[3]float64{-0.38373928784638023, -0.74778008003109453, 0.54182018315290048},
		[3]float64{-0.38373928783692102, -0.74778008004124585, 0.54182018314559},
		[3]float64{-0.38373928783691913, -0.74778008004124541, 0.54182018314559188},
		[3]float64{-0.38373928784636568, -0.74778008003110774, 0.54182018315289271},
		[3]float64{-0.38373928784637329, -0.74778008003109953, 0.54182018315289848},
		[3]float64{-0.38373928783583561, -0.74778008004095109, 0.5418201831467655},
		[3]float64{-0.38373923811582744, -0.74778006323616641, 0.54182024155322883},
		[3]float64{-0.38373857650312843, -0.74777983961840766, 0.54182101875399913},
		[3]float64{-0.38373857652422921, -0.74777983959867744, 0.54182101876628486},
	)}, boIntersectionMergeRadius)
}

func TestBooleanGetCrossedVertexIndexBug3(t *testing.T) {
	boUnionLoops(t, [][]s2.Point{boPts(
		[3]float64{1, 0, 2.4678234835261742e-72},
		[3]float64{0.99984769515639127, 0.017452406437283512, 1.8530922845942552e-27},
		[3]float64{0.99740259703611311, 0.069881849826437858, 0.017452406437283512},
	)}, [][]s2.Point{boPts(
		[3]float64{0.99999999999999989, 2.4674476220564615e-72, 2.4678234835261742e-72},
		[3]float64{0.99999999999999989, 2.8837981406657438e-169, 2.4678234835261742e-72},
		[3]float64{1, 2.8837981406657432e-169, 2.4678234835261742e-72},
	)}, 0)
}

func TestBooleanGetCrossedVertexIndexBug4(t *testing.T) {
	boUnionLoops(t, [][]s2.Point{append(boPts(
		[3]float64{0.62233331065911901, -0.0014161759526823048, 0.78275107466533156},
		[3]float64{0.6223328557578689, -0.0014164217071954736, 0.78275143589379825},
	), boParsePoint(t, "51.51317:-0.1306"))}, [][]s2.Point{append(boPts(
		[3]float64{0.62233331033809591, -0.001416176126110953, 0.78275107492024998},
		[3]float64{0.62233331033809591, -0.0014161761261109063, 0.78275107492025009},
	), boParsePoint(t, "51.52:-0.12"), boParsePoint(t, "51.52:-0.14"))}, 0)
}

func TestBooleanGetCrossedVertexIndexBug5(t *testing.T) {
	boUnionLoops(t, [][]s2.Point{boPts(
		[3]float64{0.99984769515639127, 0, 0.017452406437283512},
		[3]float64{0.99923861495548261, 0.017441774902830158, 0.034899496702500969},
		[3]float64{0.99847743863945992, 0.052327985223313139, 0.017452406437283512},
		[3]float64{0.99802119662406841, 0.034851668155187324, 0.052335956242943835},
	)}, [][]s2.Point{boPts(
		[3]float64{0.99802119662406841, 0.034851668155187324, 0.052335956242943835},
		[3]float64{0.99619692339885657, 0.052208468483931986, 0.069756473744125302},
		[3]float64{0.99802098681615425, 0.034839714972148959, 0.052347914334467859},
		[3]float64{0.99741208276778681, 0.017411821260589495, 0.069756473744125302},
		[3]float64{0.99741219210106513, 0.017411340538768819, 0.069755030419252628},
		[3]float64{0.99741211642315963, 0.017409893252357169, 0.069756473744125302},
		[3]float64{0.99984769515639116, 4.9500424645560228e-16, 0.017452406437284993},
		[3]float64{0.99984769515639127, 3.7368529835165677e-16, 0.017452406437284632},
		[3]float64{0.99984769515639116, 3.3065924905014365e-16, 0.017452406437284504},
		[3]float64{0.99984769515639127, 9.9060035932242025e-16, 0.017452406437284504},
		[3]float64{0.99969541350954794, 0.017449748351250485, 0.017452406437283512},
	), boPts(
		[3]float64{0.99984769515639116, 3.3065924905014365e-16, 0.017452406437284504},
		[3]float64{0.99984769515639116, 3.3006856770496304e-16, 0.017452406437284504},
		[3]float64{0.99984769515639127, 0, 0.017452406437284504},
		[3]float64{0.99984769515639127, 0, 0.017452406437283512},
	)}, 0)
}

// boDegeneracyIndex is DegeneracyCoverageTest::MakeIndex.
func boDegeneracyIndex(chars string) *s2.ShapeIndex {
	a := s2.PointFromCoords(1, 0, 0)
	b := s2.PointFromCoords(0, 0, 1)
	c := s2.PointFromCoords(0, 1, 0)
	negC := s2.Point{Vector: c.Mul(-1)}
	index := s2.NewShapeIndex()
	lax := func(loops ...[]s2.Point) { index.Add(s2.LaxPolygonFromPoints(loops)) }
	for i := 0; i < len(chars); i++ {
		switch ch := chars[i]; ch {
		case '.':
		case 'p':
			pv := s2.PointVector{a}
			index.Add(&pv)
		case 'P':
			index.Add(s2.LaxPolylineFromPoints([]s2.Point{a, a}))
		case 'B':
			index.Add(s2.LaxPolylineFromPoints([]s2.Point{b, b}))
		case 'u':
			index.Add(s2.LaxPolylineFromPoints([]s2.Point{a, b}))
		case 'd':
			index.Add(s2.LaxPolylineFromPoints([]s2.Point{b, a}))
		case 's':
			lax([]s2.Point{a})
		case 'S':
			lax([]s2.Point{a, b})
		case 'U':
			lax([]s2.Point{a, b, negC})
		case 'D':
			lax([]s2.Point{b, a, c})
		case '~':
			i++
			switch chars[i] {
			case 'U':
				lax([]s2.Point{negC, b, a})
			case 'D':
				lax([]s2.Point{c, a, b})
			default:
				panic("Unsupported character for ~ operator")
			}
		case 'Q':
			lax([]s2.Point{a, c, b, negC})
		case 'H':
			lax([]s2.Point{a, b}, []s2.Point{})
		case 'h':
			lax([]s2.Point{a}, []s2.Point{})
		case '*':
			lax([]s2.Point{})
		default:
			panic("Unknown degeneracy coverage symbol: " + string(ch))
		}
	}
	return index
}

const boInputChars = ".pPudsSUDHh*"

// boDegeneracyCoverage is DegeneracyCoverageTest::Run.
func boDegeneracyCoverage(t *testing.T, opType OpType, polygonModel PolygonModel, rules []string) {
	t.Helper()
	symmetric := opType != OpDifference
	polylineModels := []PolylineModel{PolylineModelOpen, PolylineModelSemiOpen, PolylineModelClosed}
	polylineLoopOptions := []bool{true, false}
	const lineChars = "Pud"
	options := DefaultBooleanOptions()
	options.PolygonModel = polygonModel
	for i := 0; i < len(boInputChars); i++ {
		ch0 := boInputChars[i]
		row := strings.Fields(rules[i])
		if row[0] != string(ch0) || row[1] != "|" {
			t.Fatalf("bad rule row %q", rules[i])
		}
		row = row[2:]
		limit := len(boInputChars)
		if symmetric {
			limit = i + 1
		}
		if len(row) != limit {
			t.Fatalf("rule row %q has %d entries, want %d", rules[i], len(row), limit)
		}
		for j := 0; j < limit; j++ {
			ch1 := boInputChars[j]
			numLineModels := 1
			if strings.IndexByte(lineChars, ch0) >= 0 || strings.IndexByte(lineChars, ch1) >= 0 {
				numLineModels = 3
			}
			for k := 0; k < numLineModels; k++ {
				options.PolylineModel = polylineModels[k]
				numLoopOptions := 1
				if ch0 == 'P' || ch1 == 'P' {
					numLoopOptions = 2
				}
				for m := 0; m < numLoopOptions; m++ {
					options.PolylineLoopsHaveBoundaries = polylineLoopOptions[m]
					code := row[j]
					choices := strings.FieldsFunc(code, func(r rune) bool { return r == '<' || r == '>' })
					result := choices[0]
					if len(choices) > 1 {
						in0 := BooleanContains(boDegeneracyIndex(string(ch0)), boDegeneracyIndex("p"), options)
						in1 := BooleanContains(boDegeneracyIndex(string(ch1)), boDegeneracyIndex("p"), options)
						if (strings.Contains(code, "<") && !in0) || (strings.Contains(code, ">") && !in1) {
							result = choices[1]
						}
					}
					if parts := strings.Split(result, "|"); len(parts) > 1 {
						result = parts[k]
					}
					what := fmt.Sprintf("%v %v polyline=%v loopsHaveBoundaries=%v inputs %c,%c expected %s",
						opType, polygonModel, options.PolylineModel, options.PolylineLoopsHaveBoundaries, ch0, ch1, result)
					boExpectResult(t, opType, options, boDegeneracyIndex(string(ch0)), boDegeneracyIndex(string(ch1)), boDegeneracyIndex(result), what)
					if symmetric && j != i {
						boExpectResult(t, opType, options, boDegeneracyIndex(string(ch1)), boDegeneracyIndex(string(ch0)), boDegeneracyIndex(result), what+" (swapped)")
					}
				}
			}
		}
	}
}

func TestBooleanDegeneracyCoverageOpenIntersection(t *testing.T) {
	boDegeneracyCoverage(t, OpIntersection, PolygonModelOpen, []string{
		". |  .",
		"p |  .    pp",
		"P |  .    p<.   PP",
		"u |  .    p<.   PP<.  uu",
		"d |  .    p<.   PP<.  ud    dd",
		"s |  .     .     .     .     .     s",
		"S |  .     .     .     .     .     .     S",
		"U |  .     .     .     .     .     .     .     U",
		"D |  .     .     .     .     .     .     .     .     D",
		"H |  .     .     .     .     .     .     .     U     D     H",
		"h |  .     .     .     u     d     .     S     U     D     H     h",
		"* |  .     p     P     u     d     s     S     U     D     H     h     *",
	})
}

func TestBooleanDegeneracyCoverageSemiOpenIntersection(t *testing.T) {
	boDegeneracyCoverage(t, OpIntersection, PolygonModelSemiOpen, []string{
		". |  .",
		"p |  .    pp",
		"P |  .    p<.   PP",
		"u |  .    p<.   PP<.  uu",
		"d |  .    p<.   PP<.  ud    dd",
		"s |  .     .     .     .     .     s",
		"S |  .    p<.   P<.    .     .    s<.    S",
		"U |  .    p<.   P<.    u    P<>.  s<.    .     U",
		"D |  .    p<.   P<.   P<>.   d    s<.    .     .     D",
		"H |  .    p<.   P<.    u     d    s<.    .     U     D     H",
		"h |  .     p     P     u     d     .     S     U     D     H     h",
		"* |  .     p     P     u     d     s     S     U     D     H     h     *",
	})
}

func TestBooleanDegeneracyCoverageClosedIntersection(t *testing.T) {
	boDegeneracyCoverage(t, OpIntersection, PolygonModelClosed, []string{
		". |  .",
		"p |  .    pp",
		"P |  .    p<.   PP",
		"u |  .    p<.   PP<.  uu",
		"d |  .    p<.   PP<.  ud    dd",
		"s |  .     p     P    P>.   P>.    s",
		"S |  .     p     P     u     d     s     S",
		"U |  .     p     P     u     d     s     S     U",
		"D |  .     p     P     u     d     s     S     S     D",
		"H |  .     p     P     u     d     s     S     U     D     H",
		"h |  .     p     P     u     d     s     S     U     D     H     h",
		"* |  .     p     P     u     d     s     S     U     D     H     h     *",
	})
}

func TestBooleanDegeneracyCoverageOpenUnion(t *testing.T) {
	boDegeneracyCoverage(t, OpUnion, PolygonModelOpen, []string{
		". |  .",
		"p |  p    pp",
		"P |  P   P<Pp   PP",
		"u |  u   u<up   Pu    uu",
		"d |  d   d<dp   Pd    ud    dd",
		"s |  s    ps    Ps    us    ds     s",
		"S |  S    pS    PS    uS    dS     S     S",
		"U |  U    pU    PU    uU    dU     U     U     U",
		"D |  D    pD    PD    uD    dD     D     D    UD     D",
		"H |  H    pH    PH    uH    dH     H     H     H     H     H",
		"h |  h    ph    Ph   Ph>h  Ph>h    h     h     h     h     h     h",
		"* |  *     *     *     *     *     *     *     *     *     *     *     *",
	})
}

func TestBooleanDegeneracyCoverageSemiOpenUnion(t *testing.T) {
	boDegeneracyCoverage(t, OpUnion, PolygonModelSemiOpen, []string{
		". |  .",
		"p |  p    pp",
		"P |  P   P<Pp   PP",
		"u |  u   u<up   Pu    uu",
		"d |  d   d<dp   Pd    ud    dd",
		"s |  s    ps    Ps    us    ds     s",
		"S |  S   S<pS  S<PS   uS    dS     S     S",
		"U |  U   U<pU  U<PU U|U|BU  dU     U     U     U",
		"D |  D   D<pD  D<PD   uD  D|BD|PBD D     D     Q     D",
		"H |  H   H<pH  H<PH    H     H     H     *     *     *     H",
		"h |  h     h     h     h     h     *    *>h   *>h   *>h   *>h    h",
		"* |  *     *     *     *     *     *     *     *     *     *     *     *",
	})
}

func TestBooleanDegeneracyCoverageClosedUnion(t *testing.T) {
	boDegeneracyCoverage(t, OpUnion, PolygonModelClosed, []string{
		". |  .",
		"p |  p    pp",
		"P |  P   P<Pp   PP",
		"u |  u   u<up   Pu    uu",
		"d |  d   d<dp   Pd    ud    dd",
		"s |  s     s     s    us    ds     s",
		"S |  S     S     S     S     S     S     S",
		"U |  U     U     U     U     U     U     U     U",
		"D |  D     D     D     D     D     D     D     Q     D",
		"H |  H     H     H     H     H     H     *     *     *     H",
		"h |  h     h     h     h     h     *     *     *     *     *     h",
		"* |  *     *     *     *     *     *     *     *     *     *     *     *",
	})
}

func TestBooleanDegeneracyCoverageOpenDifference(t *testing.T) {
	boDegeneracyCoverage(t, OpDifference, PolygonModelOpen, []string{
		". |  .     .     .     .     .     .     .     .     .     .     .     .",
		"p |  p     .    .>p   .>p   .>p    p     p     p     p     p     p     .",
		"P |  P     P     .    .>P   .>P    P     P     P     P     P     P     .",
		"u |  u     u     u     .   .|P|.   u     u     u     u     u    P<.    .",
		"d |  d     d     d   .|B|.   .     d     d     d     d     d    P<.    .",
		"s |  s     s     s     s     s     .     s     s     s     s     s     .",
		"S |  S     S     S     S     S     S     .     S     S     S     .     .",
		"U |  U     U     U     U     U     U     U     .     U     .     .     .",
		"D |  D     D     D     D     D     D     D     D     .     .     .     .",
		"H |  H     H     H     H     H     H     H    ~U    ~D     .     .     .",
		"h |  h     h     h     h     h     h     H    ~U    ~D     S     .     .",
		"* |  *     *     *     *     *     h     H    ~U    ~D     S     s     .",
	})
}

func TestBooleanDegeneracyCoverageSemiOpenDifference(t *testing.T) {
	boDegeneracyCoverage(t, OpDifference, PolygonModelSemiOpen, []string{
		". |  .     .     .     .     .     .     .     .     .     .     .     .",
		"p |  p     .    .>p   .>p   .>p    p     p    .>p   .>p    .     .     .",
		"P |  P     P     .    .>P   .>P    P     P    .>P   .>P    .     .     .",
		"u |  u     u     u     .   .|P|.   u     u   .|.|B   u     .     .     .",
		"d |  d     d     d   .|B|.   .     d     d     d   .|B|PB  .     .     .",
		"s |  s     s     s     s     s     .    .>s   .>s   .>s   .>s    s     .",
		"S |  S     S     S     S     S     S     .     .     .     S    s<.    .",
		"U |  U     U     U     U     U     U     U     .     U     .    s<.    .",
		"D |  D     D     D     D     D     D     D     D     .     .    s<.    .",
		"H |  H     H     H     H     H     H     H    ~U    ~D     .    s<.    .",
		"h |  h     h     h     h     h     h     H    ~U    ~D     S     .     .",
		"* |  *     *     *     *     *     h     H    ~U    ~D     S     s     .",
	})
}

func TestBooleanDegeneracyCoverageClosedDifference(t *testing.T) {
	boDegeneracyCoverage(t, OpDifference, PolygonModelClosed, []string{
		". |  .     .     .     .     .     .     .     .     .     .     .     .",
		"p |  p     .    .>p   .>p   .>p    .     .     .     .     .     .     .",
		"P |  P     P     .    .>P   .>P    .     .     .     .     .     .     .",
		"u |  u     u     u     .   .|P|.   u     .     .     .     .     .     .",
		"d |  d     d     d   .|B|.   .     d     .     .     .     .     .     .",
		"s |  s     s     s     s     s     .     .     .     .     .     s     .",
		"S |  S     S     S     S     S     S     .     .     .     S     .     .",
		"U |  U     U     U     U     U     U     U     .     U     .     .     .",
		"D |  D     D     D     D     D     D     D     D     .     .     .     .",
		"H |  H     H     H     H     H     H     H    ~U    ~D     .     .     .",
		"h |  h     h     h     h     h     h     H    ~U    ~D     S     .     .",
		"* |  *     *     *     *     *     h     H    ~U    ~D     S     s     .",
	})
}

func TestBooleanDegeneracyCoverageOpenSymmetricDifference(t *testing.T) {
	boDegeneracyCoverage(t, OpSymmetricDifference, PolygonModelOpen, []string{
		". |  .",
		"p |  p     .",
		"P |  P   P<Pp    .",
		"u |  u   u<up  u<uP    .",
		"d |  d   d<dp  d<dP .|PB|.   .",
		"s |  s    sp    sP    su    sd     .",
		"S |  S    Sp    SP    Su    Sd     S     .",
		"U |  U    Up    UP    Uu    Ud     U     U     .",
		"D |  D    Dp    DP    Du    Dd     D     D    UD     .",
		"H |  H    Hp    HP    Hu    Hd     H     H    ~U    ~D     .",
		"h |  h    hp    hP   hP>h  hP>h    h     H    ~U    ~D     S     .",
		"* |  *     *     *     *     *     h     H    ~U    ~D     S     s     .",
	})
}

func TestBooleanDegeneracyCoverageSemiOpenSymmetricDifference(t *testing.T) {
	boDegeneracyCoverage(t, OpSymmetricDifference, PolygonModelSemiOpen, []string{
		". |  .",
		"p |  p     .",
		"P |  P   P<Pp    .",
		"u |  u   u<up  u<uP    .",
		"d |  d   d<dp  d<dP .|PB|.   .",
		"s |  s    sp    sP    su    sd     .",
		"S |  S    Sp    SP    Su    Sd     S     .",
		"U |  U   U<Up  U<UP U|U|UB  Ud     U     U     .",
		"D |  D   D<Dp  D<DP   Du  D|BD|PBD D     D    UD     .",
		"H |  H     H     H     H     H     H     H    ~U    ~D     .",
		"h |  h     h     h     h     h     h     H    ~U    ~D     S     .",
		"* |  *     *     *     *     *     h     H    ~U    ~D     S     s     .",
	})
}

func TestBooleanDegeneracyCoverageClosedSymmetricDifference(t *testing.T) {
	boDegeneracyCoverage(t, OpSymmetricDifference, PolygonModelClosed, []string{
		". |  .",
		"p |  p     .",
		"P |  P   P<Pp    .",
		"u |  u   u<up  u<uP    .",
		"d |  d   d<dp  d<dP .|PB|.   .",
		"s |  s     s     s    su    sd     .",
		"S |  S     S     S     S     S     S     .",
		"U |  U     U     U     U     U     U     U     .",
		"D |  D     D     D     D     D     D     D    UD     .",
		"H |  H     H     H     H     H     H     H    ~U    ~D     .",
		"h |  h     h     h     h     h     h     H    ~U    ~D     S     .",
		"* |  *     *     *     *     *     h     H    ~U    ~D     S     s     .",
	})
}
