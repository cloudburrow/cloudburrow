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

// Cases ported from src/s2/s2builder_test.cc.

import (
	"fmt"
	"math"
	"math/rand/v2"
	"strconv"
	"strings"
	"testing"

	"github.com/golang/geo/r3"
	"github.com/golang/geo/s1"
	"github.com/golang/geo/s2"
)

// btPoint parses s2textformat's "lat:lng".
func btPoint(t *testing.T, s string) s2.Point {
	t.Helper()
	parts := strings.Split(strings.TrimSpace(s), ":")
	if len(parts) != 2 {
		t.Fatalf("bad point %q", s)
	}
	lat, err1 := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
	lng, err2 := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
	if err1 != nil || err2 != nil {
		t.Fatalf("bad point %q", s)
	}
	return s2.PointFromLatLng(s2.LatLngFromDegrees(lat, lng))
}

func btPoints(t *testing.T, s string) []s2.Point {
	t.Helper()
	var out []s2.Point
	for _, p := range strings.Split(s, ",") {
		if strings.TrimSpace(p) != "" {
			out = append(out, btPoint(t, p))
		}
	}
	return out
}

// btString is s2textformat::ToString of a vertex list.
func btString(ps []s2.Point) string {
	var parts []string
	for _, p := range ps {
		ll := s2.LatLngFromPoint(p)
		parts = append(parts, strconv.FormatFloat(ll.Lat.Degrees(), 'g', 15, 64)+":"+strconv.FormatFloat(ll.Lng.Degrees(), 'g', 15, 64))
	}
	return strings.Join(parts, ", ")
}

// polylineLayerOptions stands in for S2PolylineLayer::Options(), which
// builds one polyline with PolylineType::WALK.
func polylineLayerOptions() PolylineVectorLayerOptions {
	o := DefaultPolylineVectorLayerOptions()
	o.PolylineType = PolylineTypeWalk
	return o
}

func polylineStrings(out []*s2.Polyline) []string {
	var s []string
	for _, p := range out {
		s = append(s, btString(*p))
	}
	return s
}

func TestBuilderSimpleVertexMerging(t *testing.T) {
	snapRadius := 0.5 * s1.Degree
	b := NewBuilder(BuilderOptions{SnapFunction: s2.NewIdentitySnapper(snapRadius), Idempotent: true})
	var out *s2.Polygon
	b.StartLayer(NewPolygonLayer(&out, PolygonLayerOptions{}))
	b.AddLoop(btPoints(t, "0:0, 0.2:0.2, 0.1:0.2, 0.1:0.9, 0:1, 0.1:1.1, 0.9:1, 1:1, 1:0.9"))
	if err := b.Build(); err != nil {
		t.Fatal(err)
	}
	want := btPoints(t, "0:0, 0:1, 1:0.9")
	if out == nil || out.NumLoops() != 1 || out.Loop(0).NumVertices() != 3 {
		t.Fatalf("got %v", out)
	}
	for _, v := range out.Loop(0).Vertices() {
		near := false
		for _, w := range want {
			if v.Distance(w) <= snapRadius {
				near = true
			}
		}
		if !near {
			t.Errorf("vertex %s not within the snap radius of the expected loop", btString([]s2.Point{v}))
		}
	}
}

func TestBuilderSimpleS2CellIDSnapping(t *testing.T) {
	level := s2.MaxDiagMetric.MinLevel(2 * (1 * s1.Degree).Radians())
	sf := s2.CellIDSnapperForLevel(level)
	b := NewBuilder(BuilderOptions{SnapFunction: sf, Idempotent: true})
	var out *s2.Polygon
	b.StartLayer(NewPolygonLayer(&out, PolygonLayerOptions{}))
	in := btPoints(t, "2:2, 3:4, 2:6, 4:5, 6:6, 5:4, 6:2, 4:3")
	b.AddLoop(in)
	if err := b.Build(); err != nil {
		t.Fatal(err)
	}
	if out == nil || out.NumLoops() != 1 {
		t.Fatalf("got %v", out)
	}
	for _, v := range out.Loop(0).Vertices() {
		if c := cellIDFromPoint(v).Parent(level).Point(); c != v {
			t.Errorf("vertex %v is not a level-%d cell center", v, level)
		}
	}
}

func TestBuilderSimpleIntLatLngSnapping(t *testing.T) {
	b := NewBuilder(BuilderOptions{SnapFunction: newIntLatLngSnapper(0), Idempotent: true})
	var out *s2.Polygon
	b.StartLayer(NewPolygonLayer(&out, PolygonLayerOptions{}))
	b.AddLoop(btPoints(t, "2.01:2.09, 3.24:4.49, 1.78:6.25, 3.51:5.49, 6.11:6.11, 5.22:3.88, 5.55:2.49, 4.49:2.51"))
	if err := b.Build(); err != nil {
		t.Fatal(err)
	}
	if out == nil || out.NumLoops() != 1 {
		t.Fatalf("got %v", out)
	}
	// ExpectPolygonsEqual: the same loop, up to its starting vertex.
	got, want := btString(out.Loop(0).Vertices()), "2:2, 3:4, 2:6, 4:5, 6:6, 5:4, 6:2, 4:3"
	if !strings.Contains(want+", "+want, got) {
		t.Errorf("got %s, want %s", got, want)
	}
}

func buildPolylines(t *testing.T, o BuilderOptions, lo PolylineVectorLayerOptions, inputs []string, forced []string) []string {
	t.Helper()
	b := NewBuilder(o)
	var outs [][]*s2.Polyline
	outs = make([][]*s2.Polyline, len(inputs))
	for i, in := range inputs {
		b.StartLayer(NewPolylineVectorLayer(&outs[i], lo))
		b.AddPolyline(btPoints(t, in))
	}
	for _, f := range forced {
		b.ForceVertex(btPoint(t, f))
	}
	if err := b.Build(); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, o := range outs {
		got = append(got, strings.Join(polylineStrings(o), " | "))
	}
	return got
}

func TestBuilderIdempotencySnapsInadequatelySeparatedVertices(t *testing.T) {
	got := buildPolylines(t, BuilderOptions{SnapFunction: s2.NewIdentitySnapper(1 * s1.Degree), Idempotent: true},
		polylineLayerOptions(), []string{"0:0, 0:0.9, 0:2"}, nil)
	if got[0] != "0:0, 0:2" {
		t.Errorf("got %q", got[0])
	}
}

func TestBuilderIdempotencySnapsIdenticalVerticesWithZeroSnapRadius(t *testing.T) {
	b := NewBuilder(DefaultBuilderOptions())
	var out *s2.Polygon
	b.StartLayer(NewPolygonLayer(&out, PolygonLayerOptions{}))
	b.AddPolyline(btPoints(t, "0:1, 1:0"))
	b.AddPolyline(btPoints(t, "0:0, 0:1"))
	b.AddEdge(btPoint(t, "0:1"), btPoint(t, "0:1"))
	b.AddPolyline(btPoints(t, "1:0, 0:0"))
	if err := b.Build(); err != nil {
		t.Fatal(err)
	}
	if out == nil || out.NumLoops() != 1 || btString(out.Loop(0).Vertices()) != "0:0, 0:1, 1:0" {
		t.Errorf("got %v", out)
	}
}

func TestBuilderNearbyIntersectionSnappedWithZeroSnapRadius(t *testing.T) {
	o := DefaultBuilderOptions()
	o.IntersectionTolerance = IntersectionError.Radians()
	b := NewBuilder(o)
	var out []*s2.Polyline
	b.StartLayer(NewPolylineVectorLayer(&out, polylineLayerOptions()))
	b.AddPolyline(btPoints(t, "0:0, 0:10"))
	b.AddIntersection(btPoint(t, "1e-16:5"))
	if err := b.Build(); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(polylineStrings(out), " | "); got != "0:0, 1e-16:5, 0:10" {
		t.Errorf("got %q", got)
	}
}

func TestBuilderSelfIntersectingPolyline(t *testing.T) {
	o := BuilderOptions{SnapFunction: newIntLatLngSnapper(1), SplitCrossingEdges: true, Idempotent: true}
	got := buildPolylines(t, o, polylineLayerOptions(), []string{"3:1, 1:3, 1:1, 3:3"}, nil)
	if got[0] != "3:1, 2:2, 1:3, 1:1, 2:2, 3:3" {
		t.Errorf("got %q", got[0])
	}
}

// testPolylineLayersBothEdgeTypes is TestPolylineLayersBothEdgeTypes: one
// layer per input polyline, for directed and undirected edges.
func testPolylineLayersBothEdgeTypes(t *testing.T, inputs, want []string, o BuilderOptions) {
	t.Helper()
	for _, et := range []EdgeType{EdgeTypeDirected, EdgeTypeUndirected} {
		lo := polylineLayerOptions()
		lo.EdgeType = et
		got := buildPolylines(t, o, lo, inputs, nil)
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("edge type %d, layer %d: got %q, want %q", et, i, got[i], want[i])
			}
		}
	}
}

func TestBuilderSimplifyOneEdge(t *testing.T) {
	o := BuilderOptions{SnapFunction: s2.NewIdentitySnapper(1 * s1.Degree), SimplifyEdgeChains: true}
	testPolylineLayersBothEdgeTypes(t, []string{"0:0, 1:0.5, 2:-0.5, 3:0.5, 4:-0.5, 5:0"}, []string{"0:0, 5:0"}, o)
}

func TestBuilderSimplifyTwoLayers(t *testing.T) {
	o := BuilderOptions{SnapFunction: s2.NewIdentitySnapper(0.5 * s1.Degree), SplitCrossingEdges: true, SimplifyEdgeChains: true}
	testPolylineLayersBothEdgeTypes(t, []string{"-2:-1, -1:0, 1:0, 2:1", "1:-2, 0:-1, 0:1, -1:2"},
		[]string{"-2:-1, 0:0, 2:1", "1:-2, 0:0, -1:2"}, o)
}

func TestBuilderSimplifyKeepsEdgeVertexSeparation(t *testing.T) {
	o := BuilderOptions{SnapFunction: s2.NewIdentitySnapper(1 * s1.Degree), SimplifyEdgeChains: true}
	testPolylineLayersBothEdgeTypes(t, []string{"0:-10, 0.99:0, 0:10", "-5:-5, -0.2:0, -5:5"},
		[]string{"0:-10, 0.99:0, 0:10", "-5:-5, -0.2:0, -5:5"}, o)
}

func TestBuilderSimplifyOneLoop(t *testing.T) {
	for _, et := range []EdgeType{EdgeTypeDirected, EdgeTypeUndirected} {
		snapRadius := 1 * s1.Degree
		b := NewBuilder(BuilderOptions{SnapFunction: s2.NewIdentitySnapper(snapRadius), SimplifyEdgeChains: true})
		var out *s2.Polygon
		b.StartLayer(NewPolygonLayer(&out, PolygonLayerOptions{EdgeType: et}))
		in := s2.RegularLoop(s2.PointFromCoords(1, 0, 0), 20*s1.Degree, 1000)
		b.AddLoop(in.Vertices())
		if err := b.Build(); err != nil {
			t.Fatal(err)
		}
		if out == nil || out.NumLoops() != 1 {
			t.Fatalf("edge type %d: got %v", et, out)
		}
		n := out.Loop(0).NumVertices()
		if n < 10 || n > 12 {
			t.Errorf("edge type %d: %d vertices, want 10..12", et, n)
		}
		for _, v := range out.Loop(0).Vertices() {
			if d := loopBoundaryDistance(in, v); d > snapRadius {
				t.Errorf("edge type %d: vertex %v is %v from the input", et, v, d)
			}
		}
	}
}

func TestBuilderSimplifyKeepsForcedVertices(t *testing.T) {
	o := BuilderOptions{SnapFunction: s2.NewIdentitySnapper(1e-15), SimplifyEdgeChains: true}
	got := buildPolylines(t, o, polylineLayerOptions(), []string{"0:0, 0:1, 0:2, 0:3"}, []string{"0:1"})
	if got[0] != "0:0, 0:1, 0:3" {
		t.Errorf("got %q", got[0])
	}
}

func TestBuilderTopologyPreservedWithForcedVertices(t *testing.T) {
	b := NewBuilder(BuilderOptions{SnapFunction: s2.NewIdentitySnapper(IntersectionError), Idempotent: false})
	lo := DefaultPolylineVectorLayerOptions()
	lo.PolylineType = PolylineTypeWalk
	var out []*s2.Polyline
	b.StartLayer(NewPolylineVectorLayer(&out, lo))
	d := IntersectionError.Degrees()
	p := func(lat, lng float64) s2.Point { return s2.PointFromLatLng(s2.LatLngFromDegrees(lat, lng)) }
	b.AddEdge(p(0, -1), p(0, 46))
	b.ForceVertex(p(0.99*d, 0))
	b.ForceVertex(p(0.99*d, 45))
	b.ForceVertex(p(1.03*d, 22.5))
	b.AddEdge(p(1.03*d, 22.5), p(10, 22.5))
	if err := b.Build(); err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 {
		t.Fatalf("got %d polylines", len(out))
	}
	if got := btString(*out[0]); got != "0:-1, 5.03799861543821e-14:0, 0:22.5, 5.03799861543821e-14:45, 0:46" {
		t.Errorf("polyline 0: %q", got)
	}
	if got := btString(*out[1]); got != "5.24155411505188e-14:22.5, 10:22.5" {
		t.Errorf("polyline 1: %q", got)
	}
	a, c := *out[0], *out[1]
	if s2.CrossingSign(a[1], a[2], c[0], c[1]) != s2.DoNotCross {
		t.Errorf("the snapped edges cross")
	}
}

func TestBuilderPushPopLabel(t *testing.T) {
	b := NewBuilder(DefaultBuilderOptions())
	var got []string
	b.StartLayer(&graphRecorder{o: DefaultGraphOptions(), f: func(g *Graph) {
		for e := 0; e < g.NumEdges(); e++ {
			for _, id := range g.InputEdgeIDs(EdgeID(e)) {
				got = append(got, fmt.Sprint(g.Labels(id)))
			}
		}
	}})
	b.AddEdge(btPoint(t, "0:0"), btPoint(t, "0:1"))
	b.PushLabel(1)
	b.AddEdge(btPoint(t, "0:1"), btPoint(t, "0:2"))
	b.PushLabel(2)
	b.AddEdge(btPoint(t, "0:2"), btPoint(t, "0:3"))
	b.PopLabel()
	b.AddEdge(btPoint(t, "0:3"), btPoint(t, "0:4"))
	if err := b.Build(); err != nil {
		t.Fatal(err)
	}
	if s := strings.Join(got, " "); s != "[] [1] [1 2] [1]" {
		t.Errorf("labels %s", s)
	}
}

// graphRecorder is a test layer that inspects the Graph it is given.
type graphRecorder struct {
	o GraphOptions
	f func(*Graph)
}

func (r *graphRecorder) GraphOptions() GraphOptions { return r.o }
func (r *graphRecorder) Build(g *Graph) error {
	r.f(g)
	return nil
}

// intLatLngSnapper is IntLatLngSnapFunction, which rounds degrees
// (golang/geo's IntLatLngSnapper rounds radians, so its SnapPoint moves
// vertices further than its own snap radius).
type intLatLngSnapper struct {
	s2.IntLatLngSnapper
	from, to float64
}

func newIntLatLngSnapper(exponent int) intLatLngSnapper {
	power := 1.0
	for i := 0; i < exponent; i++ {
		power *= 10
	}
	return intLatLngSnapper{s2.NewIntLatLngSnapper(exponent), power, 1 / power}
}

func (sf intLatLngSnapper) SnapPoint(p s2.Point) s2.Point {
	in := s2.LatLngFromPoint(p)
	lat := math.Round(in.Lat.Degrees() * sf.from)
	lng := math.Round(in.Lng.Degrees() * sf.from)
	return s2.PointFromLatLng(s2.LatLngFromDegrees(lat*sf.to, lng*sf.to))
}

func TestBuilderMaxEdgeDeviation(t *testing.T) {
	o := BuilderOptions{SnapFunction: s2.NewIdentitySnapper(1 * s1.Degree), Idempotent: true}
	if got, want := o.MaxEdgeDeviation(), 1.1*s1.Degree; math.Abs(float64(got-want)) > 1e-15 {
		t.Errorf("MaxEdgeDeviation %v, want %v", got, want)
	}
}

func loopBoundaryDistance(l *s2.Loop, p s2.Point) s1.Angle {
	d := s1.InfChordAngle()
	vs := l.Vertices()
	for i := range vs {
		d, _ = s2.UpdateMinDistance(p, vs[i], vs[(i+1)%len(vs)], d)
	}
	return d.Angle()
}

// testPolylineVector is TestPolylineVector: every input in one
// S2PolylineVectorLayer.
func testPolylineVector(t *testing.T, inputs, want []string, lo PolylineVectorLayerOptions, o BuilderOptions) {
	t.Helper()
	b := NewBuilder(o)
	var out []*s2.Polyline
	b.StartLayer(NewPolylineVectorLayer(&out, lo))
	for _, in := range inputs {
		b.AddPolyline(btPoints(t, in))
	}
	if err := b.Build(); err != nil {
		t.Fatal(err)
	}
	if got, w := strings.Join(polylineStrings(out), "; "), strings.Join(want, "; "); got != w {
		t.Errorf("got %q, want %q", got, w)
	}
}

func TestBuilderMaxEdgeDeviationRandom(t *testing.T) {
	o := BuilderOptions{SnapFunction: s2.NewIdentitySnapper(0), SplitCrossingEdges: true, Idempotent: false}
	if o.EdgeSnapRadius() != IntersectionError {
		t.Fatalf("edge snap radius %v", o.EdgeSnapRadius())
	}
	maxDeviation := o.MaxEdgeDeviation()
	rnd := rand.New(rand.NewPCG(1, 2))
	randPoint := func() r3.Vector {
		return r3.Vector{X: 2*rnd.Float64() - 1, Y: 2*rnd.Float64() - 1, Z: 2*rnd.Float64() - 1}.Normalize()
	}
	const iters = 50
	effective := 0
	for iter := 0; iter < iters; iter++ {
		b := NewBuilder(o)
		var out []*s2.Polyline
		lo := polylineLayerOptions()
		b.StartLayer(NewPolylineVectorLayer(&out, lo))
		a := s2.Point{Vector: randPoint()}
		var bb, c s2.Point
		for {
			bb = s2.Point{Vector: a.Mul(-1).Add(randPoint().Mul(5e-16)).Normalize()}
			if bb.Vector != a.Mul(-1) {
				break
			}
		}
		for {
			c = s2.Point{Vector: a.Add(randPoint().Mul(5e-16)).Normalize()}
			if c != a {
				break
			}
		}
		b.AddEdge(a, bb)
		b.ForceVertex(c)
		if err := b.Build(); err != nil {
			t.Fatal(err)
		}
		if len(out) != 1 {
			t.Fatalf("iteration %d: %d polylines", iter, len(out))
		}
		vs := *out[0]
		n := len(vs)
		if vs[0] != a || vs[n-1] != bb {
			t.Errorf("iteration %d: endpoints moved", iter)
		}
		for i := 0; i+1 < n; i++ {
			if !IsEdgeBNearEdgeA(a, bb, vs[i], vs[i+1], maxDeviation) {
				t.Errorf("iteration %d: edge %d deviates", iter, i)
			}
		}
		if n > 2 {
			effective++
		}
	}
	if effective*5 < iters {
		t.Errorf("only %d of %d iterations split the edge", effective, iters)
	}
}

func TestBuilderIdempotencySnapsUnsnappedVertices(t *testing.T) {
	sf := newIntLatLngSnapper(0)
	p := func(lat, lng float64) s2.Point { return s2.PointFromLatLng(s2.LatLngFromDegrees(lat, lng)) }
	for _, in := range [][2]s2.Point{{p(0, 0), p(0.01, 0.6)}, {p(0.01, 0.4), p(0, 1)}} {
		b := NewBuilder(BuilderOptions{SnapFunction: sf, Idempotent: true})
		var out []*s2.Polyline
		b.StartLayer(NewPolylineVectorLayer(&out, polylineLayerOptions()))
		b.AddPolyline(in[:])
		if err := b.Build(); err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(polylineStrings(out), "; "); got != "0:0, 0:1" {
			t.Errorf("got %q", got)
		}
	}
}

func TestBuilderIdempotencyDoesNotSnapAdequatelySeparatedEdges(t *testing.T) {
	o := BuilderOptions{SnapFunction: newIntLatLngSnapper(0), Idempotent: true}
	in := btPoints(t, "1.49:0, 0:2, 0.49:3")
	for i := 0; i < 2; i++ {
		b := NewBuilder(o)
		var out *s2.Polygon
		b.StartLayer(NewPolygonLayer(&out, PolygonLayerOptions{}))
		b.AddLoop(in)
		if err := b.Build(); err != nil {
			t.Fatal(err)
		}
		if out == nil || out.NumLoops() != 1 || btString(out.Loop(0).Vertices()) != "1:0, 0:2, 0:3" {
			t.Fatalf("pass %d: got %v", i, out)
		}
		in = out.Loop(0).Vertices()
	}
}

func TestBuilderSimplifyRemovesSiblingPairs(t *testing.T) {
	o := BuilderOptions{SnapFunction: newIntLatLngSnapper(0), Idempotent: true}
	lo := DefaultPolylineVectorLayerOptions()
	lo.SiblingPairs = SiblingPairsDiscard
	testPolylineVector(t, []string{"0:0, 0:10", "0:10, 0.6:5, 0:0"}, []string{"0:0, 0:10, 1:5, 0:0"}, lo, o)
	o.SimplifyEdgeChains = true
	testPolylineVector(t, []string{"0:0, 0:10", "0:10, 0.6:5, 0:0"}, nil, lo, o)
}

func TestBuilderSimplifyMergesDuplicateEdges(t *testing.T) {
	o := BuilderOptions{SnapFunction: newIntLatLngSnapper(0), Idempotent: true}
	lo := DefaultPolylineVectorLayerOptions()
	lo.DuplicateEdges = DuplicateEdgesMerge
	testPolylineVector(t, []string{"0:0, 0:10", "0:0, 0.6:5, 0:10"}, []string{"0:0, 0:10", "0:0, 1:5, 0:10"}, lo, o)
	o.SimplifyEdgeChains = true
	testPolylineVector(t, []string{"0:0, 0:10", "0:0, 0.6:5, 0:10"}, []string{"0:0, 0:10"}, lo, o)
}

func TestBuilderSimplifyAvoidsBacktrackingVertices(t *testing.T) {
	o := BuilderOptions{SnapFunction: s2.NewIdentitySnapper(1 * s1.Degree), SimplifyEdgeChains: true}
	testPolylineLayersBothEdgeTypes(t, []string{"0:0, 1:0.1, 1:2", "0:1.05, -10:1.05"},
		[]string{"0:0, 1:0.1, 1:2", "0:1.05, -10:1.05"}, o)
}

func vp(x, y, z float64) s2.Point { return s2.Point{Vector: r3.Vector{X: x, Y: y, Z: z}} }

func TestBuilderSeparationSitesRegressionBug(t *testing.T) {
	b := NewBuilder(BuilderOptions{SnapFunction: s2.NewIdentitySnapper(0), SplitCrossingEdges: true, Idempotent: true})
	lo := DefaultPolylineVectorLayerOptions()
	lo.PolylineType = PolylineTypeWalk
	var out []*s2.Polyline
	b.StartLayer(NewPolylineVectorLayer(&out, lo))
	in := [][]s2.Point{
		{vp(0.99482894039096326, 0.087057485575229562, 0.05231035811301657),
			vp(0.19008255728509718, 0.016634125542513145, 0.98162718344766398)},
		{vp(0.99802098666373784, 0.052325259429907504, 0.034873735164620751),
			vp(0.99585181570926085, 0.087146997393412709, 0.026164135641767797),
			vp(0.99939172130835197, 6.9770704216017258e-20, 0.034873878194564757),
			vp(0.99939172130835197, 1.7442676054004314e-202, 0.034873878194564757),
			vp(0.99939172130835197, 2.4185105853059967e-57, 0.034873878194564757),
			vp(0.99939091697091686, 0, 0.034896920724182809),
			vp(0.99543519482327569, 0.088840224357046416, 0.034873879097925588)},
		{vp(-0.86549861898490243, 0.49969586065415578, 0.034873878194564757),
			vp(0.99939172130835197, 1.542605867912342e-181, 0.034873878194564757),
			vp(0.99939172130835197, 1.5426058679123417e-281, 0.034873878194564757),
			vp(0.99939172130835197, 1.5426058504696658e-231, 0.034873878194564757),
			vp(0.19080899537654492, 3.3302452117433465e-113, 0.98162718344766398)},
		{vp(0.99802098660295513, 0.052325259426720727, 0.034873736908888363),
			vp(0.99558688908226523, 0.08712381366290145, 0.034873878194564757),
			vp(0.99939172130835197, 1.0221039496805218e-23, 0.034873878194564757),
			vp(0.99939172127682907, 3.4885352106908273e-20, 0.034873879097925602),
			vp(0.99391473614090387, 0.10448593114531293, 0.03487387954694085)},
	}
	for _, l := range in {
		for i := 0; i+1 < len(l); i++ {
			b.AddEdge(l[i], l[i+1])
		}
	}
	if err := b.Build(); err != nil {
		t.Fatal(err)
	}
}

func TestBuilderVoronoiSiteExclusionBugs(t *testing.T) {
	for _, c := range []struct {
		radius        float64
		line          string
		forced        []string
		want, wantAlt string
	}{
		{64.83, "29.40:173.03, -18.02:-5.83", []string{"25.84:131.46", "-29.23:-166.58"}, "25.84:131.46, -18.02:-5.83", ""},
		// Upstream expects ...374 except on Windows, whose C library
		// rounds to ...373.
		{67.75, "47.06:-175.17, -47.59:10.57", []string{"36.36:47.63", "-28.34:-72.46"},
			"47.06:-175.17, -34.4968065428191:69.7125289482374", "47.06:-175.17, -34.4968065428191:69.7125289482373"},
	} {
		got := buildPolylines(t, BuilderOptions{SnapFunction: s2.NewIdentitySnapper(s1.Angle(c.radius) * s1.Degree), Idempotent: true},
			polylineLayerOptions(), []string{c.line}, c.forced)
		if got[0] != c.want && got[0] != c.wantAlt {
			t.Errorf("radius %v: got %q, want %q", c.radius, got[0], c.want)
		}
	}
}

func TestBuilderIncorrectSeparationSiteBug(t *testing.T) {
	b := NewBuilder(BuilderOptions{SnapFunction: s2.NewIdentitySnapper(0), SplitCrossingEdges: true, Idempotent: false})
	var out []*s2.Polyline
	b.StartLayer(NewPolylineVectorLayer(&out, polylineLayerOptions()))
	b.AddEdge(vp(-0.50094438964076704, -0.86547947317509455, 0), vp(1, 1.7786363250284876e-322, 4.7729929394856611e-65))
	b.ForceVertex(vp(1, 0, -4.7729929394856611e-65))
	b.ForceVertex(vp(1, 2.2603503297237029e-320, 4.7729929394856619e-65))
	if err := b.Build(); err != nil {
		t.Fatal(err)
	}
}

func TestBuilderMaxSnapRadiusCanSnapAtLevel0(t *testing.T) {
	if r := s2.CellIDSnapperForLevel(0).SnapRadius(); r > MaxSnapRadius {
		t.Errorf("level 0 snap radius %v exceeds %v", r, MaxSnapRadius)
	}
}
