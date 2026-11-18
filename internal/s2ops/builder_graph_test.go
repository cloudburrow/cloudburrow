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

// Ported from src/s2/s2builder_graph_test.cc (s2geometry commit
// 677c8cebcff11ba11accc0b02005bc214e06e9fd): the tests that build a Graph
// directly.

import (
	"reflect"
	"slices"
	"testing"

	"github.com/golang/geo/s2"
)

func TestGraphLabelsRequestedButNotProvided(t *testing.T) {
	opts := GraphOptions{EdgeTypeDirected, DegenerateEdgesKeep, DuplicateEdgesKeep, SiblingPairsKeep, true}
	g := NewGraph(opts, []s2.Point{s2.PointFromCoords(1, 0, 0)}, []GraphEdge{{0, 0}}, []InputEdgeIDSetID{0}, NewIDSetLexicon(), nil, NewIDSetLexicon(), nil)
	if len(g.LabelSetIDs()) != 0 || g.LabelSetID(0) != emptySetID || len(g.Labels(0)) != 0 {
		t.Fatal("labels present")
	}
	if l := NewLabelFetcher(g, EdgeTypeDirected).Fetch(0); len(l) != 0 {
		t.Fatalf("Fetch = %v", l)
	}
}

type testEdge struct {
	first, second VertexID
	ids           []InputEdgeID
}

func te(a, b VertexID, ids ...InputEdgeID) testEdge { return testEdge{a, b, ids} }

func testProcessEdges(t *testing.T, input, expected []testEdge, options *GraphOptions, wantErr bool) {
	t.Helper()
	var edges []GraphEdge
	var ids []InputEdgeIDSetID
	lex := NewIDSetLexicon()
	for _, e := range input {
		edges = append(edges, GraphEdge{e.first, e.second})
		ids = append(ids, lex.Add(e.ids...))
	}
	err := ProcessEdges(options, &edges, &ids, lex)
	if (err != nil) != wantErr {
		t.Fatalf("error = %v, want error %v", err, wantErr)
	}
	if len(edges) != len(ids) {
		t.Fatalf("%d edges, %d id sets", len(edges), len(ids))
	}
	if len(edges) != len(expected) {
		t.Fatalf("edges %v, want %v", edges, expected)
	}
	for i, e := range expected {
		if (GraphEdge{e.first, e.second}) != edges[i] {
			t.Fatalf("edge %d = %v, want %v", i, edges[i], e)
		}
		got := lex.IDSet(ids[i])
		if !(len(got) == 0 && len(e.ids) == 0) && !slices.Equal(got, e.ids) {
			t.Fatalf("edge %d ids = %v, want %v", i, got, e.ids)
		}
	}
}

func gopts(t EdgeType, d DegenerateEdges, u DuplicateEdges, s SiblingPairs) GraphOptions {
	return GraphOptions{t, d, u, s, true}
}

func TestGraphProcessEdges(t *testing.T) {
	D, U := EdgeTypeDirected, EdgeTypeUndirected
	o := gopts(D, DegenerateEdgesDiscard, DuplicateEdgesKeep, SiblingPairsKeep)
	testProcessEdges(t, []testEdge{te(0, 0), te(0, 0)}, nil, &o, false)

	o = gopts(D, DegenerateEdgesKeep, DuplicateEdgesKeep, SiblingPairsKeep)
	testProcessEdges(t, []testEdge{te(0, 0), te(0, 0)}, []testEdge{te(0, 0), te(0, 0)}, &o, false)

	o = gopts(D, DegenerateEdgesKeep, DuplicateEdgesMerge, SiblingPairsKeep)
	testProcessEdges(t, []testEdge{te(0, 0, 1), te(0, 0, 2)}, []testEdge{te(0, 0, 1, 2)}, &o, false)

	// MergeUndirectedDuplicateDegenerateEdges.
	o = gopts(U, DegenerateEdgesKeep, DuplicateEdgesMerge, SiblingPairsKeep)
	testProcessEdges(t, []testEdge{te(0, 0, 1), te(0, 0), te(0, 0), te(0, 0, 2)}, []testEdge{te(0, 0, 1, 2), te(0, 0, 1, 2)}, &o, false)

	// ConvertedUndirectedDegenerateEdges.
	o = gopts(U, DegenerateEdgesKeep, DuplicateEdgesKeep, SiblingPairsRequire)
	testProcessEdges(t, []testEdge{te(0, 0, 1), te(0, 0), te(0, 0), te(0, 0, 2)}, []testEdge{te(0, 0, 1, 2), te(0, 0, 1, 2)}, &o, false)
	if o.EdgeType != D {
		t.Fatal("not converted to directed")
	}

	// MergeConvertedUndirectedDuplicateDegenerateEdges.
	o = gopts(U, DegenerateEdgesKeep, DuplicateEdgesMerge, SiblingPairsRequire)
	testProcessEdges(t, []testEdge{te(0, 0, 1), te(0, 0), te(0, 0), te(0, 0, 2)}, []testEdge{te(0, 0, 1, 2)}, &o, false)
	if o.EdgeType != D {
		t.Fatal("not converted to directed")
	}

	// DiscardExcessConnectedDegenerateEdges.
	o = gopts(D, DegenerateEdgesDiscardExcess, DuplicateEdgesKeep, SiblingPairsKeep)
	testProcessEdges(t, []testEdge{te(0, 0), te(0, 1)}, []testEdge{te(0, 1)}, &o, false)
	testProcessEdges(t, []testEdge{te(0, 0), te(1, 0)}, []testEdge{te(1, 0)}, &o, false)
	testProcessEdges(t, []testEdge{te(0, 1), te(1, 1)}, []testEdge{te(0, 1)}, &o, false)
	testProcessEdges(t, []testEdge{te(1, 0), te(1, 1)}, []testEdge{te(1, 0)}, &o, false)

	// DiscardExcessIsolatedDegenerateEdges.
	testProcessEdges(t, []testEdge{te(0, 0, 1), te(0, 0, 2)}, []testEdge{te(0, 0, 1, 2)}, &o, false)

	// DiscardExcessUndirectedIsolatedDegenerateEdges.
	o = gopts(U, DegenerateEdgesDiscardExcess, DuplicateEdgesKeep, SiblingPairsKeep)
	testProcessEdges(t, []testEdge{te(0, 0, 1), te(0, 0), te(0, 0, 2), te(0, 0)}, []testEdge{te(0, 0, 1, 2), te(0, 0, 1, 2)}, &o, false)

	// DiscardExcessConvertedUndirectedIsolatedDegenerateEdges.
	o = gopts(U, DegenerateEdgesDiscardExcess, DuplicateEdgesKeep, SiblingPairsRequire)
	testProcessEdges(t, []testEdge{te(0, 0, 1), te(0, 0, 2), te(0, 0, 3), te(0, 0)}, []testEdge{te(0, 0, 1, 2, 3)}, &o, false)
	if o.EdgeType != D {
		t.Fatal("not converted to directed")
	}

	// SiblingPairsDiscardMergesDegenerateEdgeLabels.
	o = gopts(D, DegenerateEdgesKeep, DuplicateEdgesKeep, SiblingPairsDiscard)
	three := []testEdge{te(0, 0, 1, 2, 3), te(0, 0, 1, 2, 3), te(0, 0, 1, 2, 3)}
	testProcessEdges(t, []testEdge{te(0, 0, 1), te(0, 0, 2), te(0, 0, 3)}, three, &o, false)
	o.SiblingPairs = SiblingPairsDiscardExcess
	testProcessEdges(t, []testEdge{te(0, 0, 1), te(0, 0, 2), te(0, 0, 3)}, three, &o, false)

	// KeepSiblingPairs, MergeDuplicateSiblingPairs.
	o = gopts(D, DegenerateEdgesDiscard, DuplicateEdgesKeep, SiblingPairsKeep)
	testProcessEdges(t, []testEdge{te(0, 1), te(1, 0)}, []testEdge{te(0, 1), te(1, 0)}, &o, false)
	o = gopts(D, DegenerateEdgesDiscard, DuplicateEdgesMerge, SiblingPairsKeep)
	testProcessEdges(t, []testEdge{te(0, 1), te(0, 1), te(1, 0)}, []testEdge{te(0, 1), te(1, 0)}, &o, false)

	// DiscardSiblingPairs.
	o = gopts(D, DegenerateEdgesDiscard, DuplicateEdgesKeep, SiblingPairsDiscard)
	testProcessEdges(t, []testEdge{te(0, 1), te(1, 0)}, nil, &o, false)
	testProcessEdges(t, []testEdge{te(0, 1), te(0, 1), te(1, 0), te(1, 0)}, nil, &o, false)
	testProcessEdges(t, []testEdge{te(0, 1), te(0, 1), te(0, 1), te(1, 0)}, []testEdge{te(0, 1), te(0, 1)}, &o, false)
	testProcessEdges(t, []testEdge{te(0, 1), te(1, 0), te(1, 0), te(1, 0)}, []testEdge{te(1, 0), te(1, 0)}, &o, false)

	// DiscardSiblingPairsMergeDuplicates.
	o = gopts(D, DegenerateEdgesDiscard, DuplicateEdgesMerge, SiblingPairsDiscard)
	testProcessEdges(t, []testEdge{te(0, 1), te(0, 1), te(1, 0), te(1, 0)}, nil, &o, false)
	testProcessEdges(t, []testEdge{te(0, 1), te(0, 1), te(0, 1), te(1, 0)}, []testEdge{te(0, 1)}, &o, false)
	testProcessEdges(t, []testEdge{te(0, 1), te(1, 0), te(1, 0), te(1, 0)}, []testEdge{te(1, 0)}, &o, false)

	// DiscardUndirectedSiblingPairs.
	o = gopts(U, DegenerateEdgesDiscard, DuplicateEdgesKeep, SiblingPairsDiscard)
	testProcessEdges(t, []testEdge{te(0, 1), te(1, 0)}, []testEdge{te(0, 1), te(1, 0)}, &o, false)
	testProcessEdges(t, []testEdge{te(0, 1), te(0, 1), te(1, 0), te(1, 0)}, nil, &o, false)
	testProcessEdges(t, []testEdge{te(0, 1), te(0, 1), te(0, 1), te(1, 0), te(1, 0), te(1, 0)}, []testEdge{te(0, 1), te(1, 0)}, &o, false)

	// DiscardExcessSiblingPairs.
	o = gopts(D, DegenerateEdgesDiscard, DuplicateEdgesKeep, SiblingPairsDiscardExcess)
	testProcessEdges(t, []testEdge{te(0, 1), te(1, 0)}, []testEdge{te(0, 1), te(1, 0)}, &o, false)
	testProcessEdges(t, []testEdge{te(0, 1), te(0, 1), te(1, 0), te(1, 0)}, []testEdge{te(0, 1), te(1, 0)}, &o, false)
	testProcessEdges(t, []testEdge{te(0, 1), te(0, 1), te(0, 1), te(1, 0)}, []testEdge{te(0, 1), te(0, 1)}, &o, false)
	testProcessEdges(t, []testEdge{te(0, 1), te(1, 0), te(1, 0), te(1, 0)}, []testEdge{te(1, 0), te(1, 0)}, &o, false)

	// DiscardExcessSiblingPairsMergeDuplicates.
	o = gopts(D, DegenerateEdgesDiscard, DuplicateEdgesMerge, SiblingPairsDiscardExcess)
	testProcessEdges(t, []testEdge{te(0, 1), te(0, 1), te(1, 0), te(1, 0)}, []testEdge{te(0, 1), te(1, 0)}, &o, false)
	testProcessEdges(t, []testEdge{te(0, 1), te(0, 1), te(0, 1), te(1, 0)}, []testEdge{te(0, 1)}, &o, false)
	testProcessEdges(t, []testEdge{te(0, 1), te(1, 0), te(1, 0), te(1, 0)}, []testEdge{te(1, 0)}, &o, false)

	// DiscardExcessUndirectedSiblingPairs.
	o = gopts(U, DegenerateEdgesDiscard, DuplicateEdgesKeep, SiblingPairsDiscardExcess)
	testProcessEdges(t, []testEdge{te(0, 1), te(1, 0)}, []testEdge{te(0, 1), te(1, 0)}, &o, false)
	testProcessEdges(t, []testEdge{te(0, 1), te(0, 1), te(1, 0), te(1, 0)}, []testEdge{te(0, 1), te(0, 1), te(1, 0), te(1, 0)}, &o, false)
	testProcessEdges(t, []testEdge{te(0, 1), te(0, 1), te(0, 1), te(1, 0), te(1, 0), te(1, 0)}, []testEdge{te(0, 1), te(1, 0)}, &o, false)

	// CreateSiblingPairs.
	o = gopts(D, DegenerateEdgesDiscard, DuplicateEdgesKeep, SiblingPairsCreate)
	testProcessEdges(t, []testEdge{te(0, 1)}, []testEdge{te(0, 1), te(1, 0)}, &o, false)
	testProcessEdges(t, []testEdge{te(0, 1), te(0, 1)}, []testEdge{te(0, 1), te(0, 1), te(1, 0), te(1, 0)}, &o, false)

	// RequireSiblingPairs.
	o = gopts(D, DegenerateEdgesDiscard, DuplicateEdgesKeep, SiblingPairsRequire)
	testProcessEdges(t, []testEdge{te(0, 1), te(1, 0)}, []testEdge{te(0, 1), te(1, 0)}, &o, false)
	testProcessEdges(t, []testEdge{te(0, 1)}, []testEdge{te(0, 1), te(1, 0)}, &o, true)

	// CreateUndirectedSiblingPairs.
	o = gopts(D, DegenerateEdgesDiscard, DuplicateEdgesKeep, SiblingPairsCreate)
	testProcessEdges(t, []testEdge{te(0, 1), te(1, 0)}, []testEdge{te(0, 1), te(1, 0)}, &o, false)
	o.EdgeType = U
	testProcessEdges(t, []testEdge{te(0, 1), te(0, 1), te(1, 0), te(1, 0)}, []testEdge{te(0, 1), te(1, 0)}, &o, false)
	o.EdgeType = U
	testProcessEdges(t, []testEdge{te(0, 1), te(0, 1), te(0, 1), te(1, 0), te(1, 0), te(1, 0)}, []testEdge{te(0, 1), te(0, 1), te(1, 0), te(1, 0)}, &o, false)
	if o.EdgeType != D {
		t.Fatal("not converted to directed")
	}

	// CreateSiblingPairsMergeDuplicates, CreateUndirectedSiblingPairsMergeDuplicates.
	o = gopts(D, DegenerateEdgesDiscard, DuplicateEdgesMerge, SiblingPairsCreate)
	testProcessEdges(t, []testEdge{te(0, 1)}, []testEdge{te(0, 1), te(1, 0)}, &o, false)
	testProcessEdges(t, []testEdge{te(0, 1), te(0, 1)}, []testEdge{te(0, 1), te(1, 0)}, &o, false)
	o.EdgeType = U
	testProcessEdges(t, []testEdge{te(0, 1), te(0, 1), te(0, 1), te(1, 0), te(1, 0), te(1, 0)}, []testEdge{te(0, 1), te(1, 0)}, &o, false)
}

func llPoints(lls ...[2]float64) []s2.Point {
	var ps []s2.Point
	for _, ll := range lls {
		ps = append(ps, s2.PointFromLatLng(s2.LatLngFromDegrees(ll[0], ll[1])))
	}
	return ps
}

func TestGraphMakeSubgraph(t *testing.T) {
	vertices := llPoints([2]float64{0, 0}, [2]float64{0, 1}, [2]float64{1, 1})
	check := func(g *Graph, lex *IDSetLexicon, newOptions GraphOptions, edges []GraphEdge, ids []InputEdgeIDSetID, wantEdges []GraphEdge, wantIDs []InputEdgeIDSetID) {
		t.Helper()
		ng, err := g.MakeSubgraph(newOptions, &edges, &ids, lex, nil)
		if err != nil {
			t.Fatal(err)
		}
		if &ng.Vertices()[0] != &g.Vertices()[0] {
			t.Error("vertices copied")
		}
		if ng.Options() != newOptions {
			t.Errorf("options %v, want %v", ng.Options(), newOptions)
		}
		if !reflect.DeepEqual(ng.Edges(), wantEdges) || !reflect.DeepEqual(ng.InputEdgeIDSetIDs(), wantIDs) {
			t.Errorf("edges %v ids %v, want %v %v", ng.Edges(), ng.InputEdgeIDSetIDs(), wantEdges, wantIDs)
		}
	}
	// UndirectedToUndirected.
	opts := gopts(EdgeTypeUndirected, DegenerateEdgesKeep, DuplicateEdgesKeep, SiblingPairsKeep)
	edges := []GraphEdge{{0, 0}, {0, 0}, {1, 2}, {2, 1}}
	ids := []InputEdgeIDSetID{0, 0, 1, 1}
	lex := NewIDSetLexicon()
	g := NewGraph(opts, vertices, edges, ids, lex, nil, NewIDSetLexicon(), nil)
	check(g, lex, gopts(EdgeTypeUndirected, DegenerateEdgesDiscard, DuplicateEdgesKeep, SiblingPairsKeep),
		slices.Clone(edges), slices.Clone(ids), []GraphEdge{{1, 2}, {2, 1}}, []InputEdgeIDSetID{1, 1})

	// DirectedToUndirected.
	opts = gopts(EdgeTypeDirected, DegenerateEdgesKeep, DuplicateEdgesKeep, SiblingPairsKeep)
	edges = []GraphEdge{{0, 0}, {0, 1}, {1, 2}, {1, 2}, {2, 1}}
	ids = []InputEdgeIDSetID{1, 2, 3, 3, 3}
	g = NewGraph(opts, vertices, edges, ids, lex, nil, NewIDSetLexicon(), nil)
	check(g, lex, gopts(EdgeTypeUndirected, DegenerateEdgesKeep, DuplicateEdgesKeep, SiblingPairsDiscardExcess),
		slices.Clone(edges), slices.Clone(ids),
		[]GraphEdge{{0, 0}, {0, 0}, {0, 1}, {1, 0}, {1, 2}, {2, 1}},
		[]InputEdgeIDSetID{1, 1, 2, emptySetID, 3, 3})
}

// TestGraphLoopsAndPolylines checks GetDirectedLoops, the left-turn rule,
// CanonicalizeLoopOrder and GetPolylines on hand-built graphs (upstream
// builds these through S2Builder).
func TestGraphLoopsAndPolylines(t *testing.T) {
	// Two triangles sharing vertex 0: 0-1-2 and 0-3-4, counter-clockwise.
	vs := llPoints([2]float64{0, 0}, [2]float64{0, 1}, [2]float64{1, 0.5}, [2]float64{0, -1}, [2]float64{-1, -0.5})
	edges := []GraphEdge{{0, 1}, {0, 3}, {1, 2}, {2, 0}, {3, 4}, {4, 0}}
	// input ids in loop order: 0->1 is 0, 1->2 is 1, 2->0 is 2, 0->3 is 3...
	ids := []InputEdgeIDSetID{0, 3, 1, 2, 4, 5}
	opts := gopts(EdgeTypeDirected, DegenerateEdgesDiscard, DuplicateEdgesKeep, SiblingPairsDiscard)
	g := NewGraph(opts, vs, edges, ids, NewIDSetLexicon(), nil, NewIDSetLexicon(), nil)
	loops, err := g.GetDirectedLoops(LoopTypeSimple)
	if err != nil {
		t.Fatal(err)
	}
	want := [][]EdgeID{{0, 2, 3}, {1, 4, 5}}
	if !reflect.DeepEqual(loops, want) {
		t.Fatalf("simple loops %v, want %v", loops, want)
	}
	circuits, err := g.GetDirectedLoops(LoopTypeCircuit)
	if err != nil {
		t.Fatal(err)
	}
	// Left turns keep the two triangles apart at their shared vertex.
	if !reflect.DeepEqual(circuits, want) {
		t.Fatalf("circuits %v", circuits)
	}

	// An unbalanced vertex is an error.
	bad := NewGraph(opts, vs, []GraphEdge{{0, 1}, {1, 2}}, []InputEdgeIDSetID{0, 1}, NewIDSetLexicon(), nil, NewIDSetLexicon(), nil)
	if _, err := bad.GetDirectedLoops(LoopTypeSimple); err == nil {
		t.Fatal("no error for open chain")
	}

	// A polyline 3->0->1->2 with ids in order: one path.
	pl := NewGraph(gopts(EdgeTypeDirected, DegenerateEdgesDiscard, DuplicateEdgesKeep, SiblingPairsKeep), vs,
		[]GraphEdge{{0, 1}, {1, 2}, {3, 0}}, []InputEdgeIDSetID{1, 2, 0}, NewIDSetLexicon(), nil, NewIDSetLexicon(), nil)
	if p := pl.GetPolylines(PolylineTypePath); !reflect.DeepEqual(p, [][]EdgeID{{2, 0, 1}}) {
		t.Fatalf("paths %v", p)
	}
	if p := pl.GetPolylines(PolylineTypeWalk); !reflect.DeepEqual(p, [][]EdgeID{{2, 0, 1}}) {
		t.Fatalf("walks %v", p)
	}
	// The same undirected (sibling pairs labelled in the input direction).
	upl := NewGraph(gopts(EdgeTypeUndirected, DegenerateEdgesDiscard, DuplicateEdgesKeep, SiblingPairsKeep), vs,
		[]GraphEdge{{0, 1}, {0, 3}, {1, 0}, {1, 2}, {2, 1}, {3, 0}},
		[]InputEdgeIDSetID{1, emptySetID, emptySetID, 2, emptySetID, 0}, NewIDSetLexicon(), nil, NewIDSetLexicon(), nil)
	if p := upl.GetPolylines(PolylineTypePath); !reflect.DeepEqual(p, [][]EdgeID{{5, 0, 3}}) {
		t.Fatalf("undirected paths %v", p)
	}

	// CanonicalizeLoopOrder: input ids (7, 7, 4, 5, 6, 7) rotate to start at 4.
	minIDs := []InputEdgeID{7, 7, 4, 5, 6, 7}
	loop := []EdgeID{0, 1, 2, 3, 4, 5}
	CanonicalizeLoopOrder(minIDs, loop)
	if !reflect.DeepEqual(loop, []EdgeID{2, 3, 4, 5, 0, 1}) {
		t.Fatalf("canonical loop %v", loop)
	}
}

func TestGraphUndirectedComponents(t *testing.T) {
	// A triangle given as undirected edges: two complements, the one
	// following the input order first.
	vs := llPoints([2]float64{0, 0}, [2]float64{0, 1}, [2]float64{1, 0.5})
	edges := []GraphEdge{{0, 1}, {0, 2}, {1, 0}, {1, 2}, {2, 0}, {2, 1}}
	ids := []InputEdgeIDSetID{0, emptySetID, emptySetID, 1, 2, emptySetID}
	g := NewGraph(gopts(EdgeTypeUndirected, DegenerateEdgesDiscard, DuplicateEdgesKeep, SiblingPairsKeep), vs, edges, ids, NewIDSetLexicon(), nil, NewIDSetLexicon(), nil)
	cs, err := g.GetUndirectedComponents(LoopTypeSimple)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 1 || !reflect.DeepEqual(cs[0][0], [][]EdgeID{{0, 3, 4}}) || len(cs[0][1]) != 1 || len(cs[0][1][0]) != 3 {
		t.Fatalf("components %v", cs)
	}
}
