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

// Layer tests on hand-built graphs, after the cases of
// s2builderutil_s2polygon_layer_test.cc, s2builderutil_s2polyline_vector_layer_test.cc,
// s2builderutil_s2point_vector_layer_test.cc and
// s2builderutil_closed_set_normalizer_test.cc (upstream builds the graphs
// with S2Builder).

import (
	"reflect"
	"testing"

	"github.com/golang/geo/s2"
)

// layerGraph builds the graph a layer asks for from directed edges with
// input ids 0, 1, 2, ... (ProcessEdges applies the layer's options).
func layerGraph(l Layer, vs []s2.Point, in []GraphEdge, full IsFullPolygonPredicate) *Graph {
	opts := l.GraphOptions()
	edges := append([]GraphEdge(nil), in...)
	lex := NewIDSetLexicon()
	var ids []InputEdgeIDSetID
	for i := range in {
		ids = append(ids, InputEdgeIDSetID(i))
	}
	if opts.EdgeType == EdgeTypeUndirected {
		for i := range in {
			edges = append(edges, in[i].Reverse())
			ids = append(ids, emptySetID)
		}
	}
	// ProcessEdges sorts the edges, as S2Builder relies on it to.
	if err := ProcessEdges(&opts, &edges, &ids, lex); err != nil {
		panic(err)
	}
	return NewGraph(opts, vs, edges, ids, lex, nil, NewIDSetLexicon(), full)
}

func TestPolygonLayer(t *testing.T) {
	// A square with a square hole, and a sibling pair that the layer
	// discards.
	vs := llPoints([2]float64{0, 0}, [2]float64{0, 10}, [2]float64{10, 10}, [2]float64{10, 0},
		[2]float64{2, 2}, [2]float64{8, 2}, [2]float64{8, 8}, [2]float64{2, 8}, [2]float64{20, 20})
	var p *s2.Polygon
	l := NewPolygonLayer(&p, PolygonLayerOptions{Validate: true})
	// The shell runs counter-clockwise, the hole clockwise.
	in := []GraphEdge{{0, 1}, {1, 2}, {2, 3}, {3, 0}, {4, 5}, {5, 6}, {6, 7}, {7, 4}, {0, 8}, {8, 0}}
	if err := l.Build(layerGraph(l, vs, in, nil)); err != nil {
		t.Fatal(err)
	}
	if p.NumLoops() != 2 || p.Loop(1).NumVertices() != 4 || !p.Loop(1).IsHole() {
		t.Fatalf("polygon: %d loops", p.NumLoops())
	}
	if area := p.Area(); area > 0.1 {
		t.Fatalf("area %v", area)
	}

	// No edges: full or empty from the predicate.
	if err := l.Build(layerGraph(l, vs, nil, IsFullPolygon(true))); err != nil || !p.IsFull() {
		t.Fatalf("full: %v %v", p.IsFull(), err)
	}
	if err := l.Build(layerGraph(l, vs, nil, IsFullPolygon(false))); err != nil || !p.IsEmpty() {
		t.Fatalf("empty: %v %v", p.IsEmpty(), err)
	}
	if err := l.Build(layerGraph(l, vs, nil, nil)); err == nil {
		t.Fatal("no error without a full-polygon predicate")
	}

	// Undirected edges: loops are normalized.
	ul := NewPolygonLayer(&p, PolygonLayerOptions{EdgeType: EdgeTypeUndirected})
	if err := ul.Build(layerGraph(ul, vs, []GraphEdge{{0, 1}, {1, 2}, {2, 3}, {3, 0}}, nil)); err != nil {
		t.Fatal(err)
	}
	if p.NumLoops() != 1 || p.Area() > 2*3.14159 {
		t.Fatalf("undirected polygon not normalized: %d loops, area %v", p.NumLoops(), p.Area())
	}
}

func TestPolylineVectorLayer(t *testing.T) {
	vs := llPoints([2]float64{0, 0}, [2]float64{0, 1}, [2]float64{0, 2}, [2]float64{1, 1}, [2]float64{5, 5})
	var out []*s2.Polyline
	l := NewPolylineVectorLayer(&out, DefaultPolylineVectorLayerOptions())
	// Two polylines meeting at vertex 1 (a branch point), and a degenerate
	// edge, which the layer discards.
	if err := l.Build(layerGraph(l, vs, []GraphEdge{{0, 1}, {1, 2}, {3, 1}, {4, 4}}, nil)); err != nil {
		t.Fatal(err)
	}
	var got [][]s2.Point
	for _, p := range out {
		got = append(got, []s2.Point(*p))
	}
	want := [][]s2.Point{{vs[0], vs[1]}, {vs[1], vs[2]}, {vs[3], vs[1]}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("paths %v, want %v", got, want)
	}
	// As walks, 0-1-2 continues through 1.
	out = nil
	o := DefaultPolylineVectorLayerOptions()
	o.PolylineType = PolylineTypeWalk
	wl := NewPolylineVectorLayer(&out, o)
	if err := wl.Build(layerGraph(wl, vs, []GraphEdge{{0, 1}, {1, 2}, {3, 1}}, nil)); err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 || len(*out[0]) != 3 {
		t.Fatalf("walks %v", out)
	}
	// Undirected: the polyline keeps the input direction.
	out = nil
	o = DefaultPolylineVectorLayerOptions()
	o.EdgeType = EdgeTypeUndirected
	ul := NewPolylineVectorLayer(&out, o)
	if err := ul.Build(layerGraph(ul, vs, []GraphEdge{{2, 1}, {1, 0}}, nil)); err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || !reflect.DeepEqual([]s2.Point(*out[0]), []s2.Point{vs[2], vs[1], vs[0]}) {
		t.Fatalf("undirected %v", out)
	}
}

func TestPointVectorLayer(t *testing.T) {
	vs := llPoints([2]float64{0, 0}, [2]float64{0, 1})
	var out []s2.Point
	l := NewPointVectorLayer(&out, PointVectorLayerOptions{})
	if err := l.Build(layerGraph(l, vs, []GraphEdge{{1, 1}, {0, 0}, {1, 1}}, nil)); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(out, []s2.Point{vs[0], vs[1]}) {
		t.Fatalf("merged points %v", out)
	}
	out = nil
	kl := NewPointVectorLayer(&out, PointVectorLayerOptions{DuplicateEdges: DuplicateEdgesKeep})
	if err := kl.Build(layerGraph(kl, vs, []GraphEdge{{1, 1}, {0, 0}, {1, 1}}, nil)); err != nil || len(out) != 3 {
		t.Fatalf("kept points %v %v", out, err)
	}
	if err := kl.Build(layerGraph(kl, vs, []GraphEdge{{0, 1}}, nil)); err == nil {
		t.Fatal("no error for a non-degenerate edge")
	}
}

// normalizerGraphs builds the three input graphs of a ClosedSetNormalizer.
func normalizerGraphs(n *ClosedSetNormalizer, vs []s2.Point, dims [3][]GraphEdge) [3]*Graph {
	var g [3]*Graph
	lex := NewIDSetLexicon()
	id := 0
	for dim := 0; dim < 3; dim++ {
		opts := n.GraphOptions()[dim]
		edges := append([]GraphEdge(nil), dims[dim]...)
		var ids []InputEdgeIDSetID
		for range edges {
			ids = append(ids, InputEdgeIDSetID(id))
			id++
		}
		if err := ProcessEdges(&opts, &edges, &ids, lex); err != nil {
			panic(err)
		}
		g[dim] = NewGraph(opts, vs, edges, ids, lex, nil, NewIDSetLexicon(), IsFullPolygon(false))
	}
	return g
}

func TestClosedSetNormalizer(t *testing.T) {
	vs := llPoints([2]float64{0, 0}, [2]float64{0, 1}, [2]float64{1, 0}, [2]float64{3, 3}, [2]float64{4, 4}, [2]float64{5, 5}, [2]float64{6, 6})
	out := [3]GraphOptions{
		{EdgeTypeDirected, DegenerateEdgesKeep, DuplicateEdgesMerge, SiblingPairsKeep, true},
		{EdgeTypeDirected, DegenerateEdgesDiscard, DuplicateEdgesKeep, SiblingPairsKeep, true},
		{EdgeTypeDirected, DegenerateEdgesDiscard, DuplicateEdgesKeep, SiblingPairsDiscard, true},
	}
	n := NewClosedSetNormalizer(DefaultClosedSetNormalizerOptions(), out)
	g := normalizerGraphs(n, vs, [3][]GraphEdge{
		// points: one on the triangle (suppressed), one isolated (kept).
		{{0, 0}, {6, 6}},
		// polylines: one along a triangle edge (suppressed), one apart.
		{{0, 1}, {3, 4}},
		// polygon: triangle 0-1-2, plus a degenerate shell 4-5-4 (a
		// sibling pair, demoted to a polyline edge).
		{{0, 1}, {1, 2}, {2, 0}, {4, 5}, {5, 4}},
	})
	res, err := n.Run(g)
	if err != nil {
		t.Fatal(err)
	}
	if got := res[0].Edges(); !reflect.DeepEqual(got, []GraphEdge{{6, 6}}) {
		t.Errorf("points %v", got)
	}
	if got := res[1].Edges(); !reflect.DeepEqual(got, []GraphEdge{{3, 4}, {4, 5}, {5, 4}}) {
		t.Errorf("polylines %v", got)
	}
	if got := res[2].Edges(); !reflect.DeepEqual(got, []GraphEdge{{0, 1}, {1, 2}, {2, 0}}) {
		t.Errorf("polygon %v", got)
	}

	// Without suppression the coincident pieces stay.
	n = NewClosedSetNormalizer(ClosedSetNormalizerOptions{}, out)
	g = normalizerGraphs(n, vs, [3][]GraphEdge{{{0, 0}}, {{0, 1}}, {{0, 1}, {1, 2}, {2, 0}}})
	res, err = n.Run(g)
	if err != nil {
		t.Fatal(err)
	}
	if len(res[0].Edges()) != 1 || len(res[1].Edges()) != 1 || len(res[2].Edges()) != 3 {
		t.Errorf("unsuppressed: %v %v %v", res[0].Edges(), res[1].Edges(), res[2].Edges())
	}
}

// TestNormalizeClosedSetLayers checks that the three layers hand the
// normalized graphs to the output layers once all three are built.
func TestNormalizeClosedSetLayers(t *testing.T) {
	vs := llPoints([2]float64{0, 0}, [2]float64{0, 1}, [2]float64{1, 0}, [2]float64{5, 5})
	var pts []s2.Point
	var lines []*s2.Polyline
	var poly *s2.Polygon
	layers := NormalizeClosedSet([3]Layer{
		NewPointVectorLayer(&pts, PointVectorLayerOptions{}),
		NewPolylineVectorLayer(&lines, DefaultPolylineVectorLayerOptions()),
		NewPolygonLayer(&poly, PolygonLayerOptions{}),
	})
	lex := NewIDSetLexicon()
	inputs := [3][]GraphEdge{{{1, 1}, {3, 3}}, nil, {{0, 1}, {1, 2}, {2, 0}}}
	id := 0
	for dim, l := range layers {
		opts := l.GraphOptions()
		edges := append([]GraphEdge(nil), inputs[dim]...)
		var ids []InputEdgeIDSetID
		for range edges {
			ids = append(ids, InputEdgeIDSetID(id))
			id++
		}
		if err := ProcessEdges(&opts, &edges, &ids, lex); err != nil {
			t.Fatal(err)
		}
		if err := l.Build(NewGraph(opts, vs, edges, ids, lex, nil, NewIDSetLexicon(), IsFullPolygon(false))); err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(pts, []s2.Point{vs[3]}) || len(lines) != 0 || poly == nil || poly.NumLoops() != 1 {
		t.Fatalf("points %v lines %v polygon %v", pts, lines, poly)
	}
}

func TestFindPolygonDegeneracies(t *testing.T) {
	vs := llPoints([2]float64{0, 0}, [2]float64{0, 10}, [2]float64{10, 0}, [2]float64{2, 2}, [2]float64{3, 3}, [2]float64{20, 20}, [2]float64{21, 21})
	opts := GraphOptions{EdgeTypeDirected, DegenerateEdgesDiscardExcess, DuplicateEdgesKeep, SiblingPairsDiscardExcess, false}
	// Triangle 0-1-2 (whichever orientation contains 3), a sibling pair
	// 3-4 inside it, and a sibling pair 5-6 outside.
	tri := []GraphEdge{{0, 1}, {1, 2}, {2, 0}}
	if s2.LoopFromPoints([]s2.Point{vs[0], vs[1], vs[2]}).ContainsPoint(vs[3]) == false {
		tri = []GraphEdge{{0, 2}, {2, 1}, {1, 0}}
	}
	edges := append(append([]GraphEdge(nil), tri...), GraphEdge{3, 4}, GraphEdge{4, 3}, GraphEdge{5, 6}, GraphEdge{6, 5})
	var ids []InputEdgeIDSetID
	for i := range edges {
		ids = append(ids, InputEdgeIDSetID(i))
	}
	lex := NewIDSetLexicon()
	if err := ProcessEdges(&opts, &edges, &ids, lex); err != nil {
		t.Fatal(err)
	}
	g := NewGraph(opts, vs, edges, ids, lex, nil, NewIDSetLexicon(), IsFullPolygon(false))
	ds, err := FindPolygonDegeneracies(g)
	if err != nil {
		t.Fatal(err)
	}
	holes := map[GraphEdge]bool{}
	for _, d := range ds {
		holes[g.Edge(d.EdgeID)] = d.IsHole
	}
	want := map[GraphEdge]bool{{3, 4}: true, {4, 3}: true, {5, 6}: false, {6, 5}: false}
	if !reflect.DeepEqual(holes, want) {
		t.Fatalf("degeneracies %v, want %v", holes, want)
	}
	if IsFullyDegenerate(g) {
		t.Fatal("fully degenerate")
	}
}
