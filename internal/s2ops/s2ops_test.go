package s2ops

import (
	"math"
	"testing"

	"github.com/golang/geo/s1"
	"github.com/golang/geo/s2"
)

func ll(lng, lat float64) s2.Point { return s2.PointFromLatLng(s2.LatLngFromDegrees(lat, lng)) }

func loopArea(l []s2.Point) float64 { return s2.LoopFromPoints(l).Area() }

func square(x0, y0, x1, y1 float64) []s2.Point {
	return []s2.Point{ll(x0, y0), ll(x1, y0), ll(x1, y1), ll(x0, y1)}
}

// TestOverlayAreas checks union, intersection and difference of two
// overlapping squares by their areas, and that their loops start as
// S2Builder's do.
func TestOverlayAreas(t *testing.T) {
	a, b := square(0, 0, 2, 2), square(1, 1, 3, 3)
	ref := s2.OriginPoint()
	area := func(ls [][]s2.Point) float64 {
		s := 0.0
		for _, l := range ls {
			s += loopArea(l)
		}
		return s
	}
	u, _, err := Overlay([][]s2.Point{a, b}, ref, 0, 1)
	if err != nil || len(u) != 1 {
		t.Fatalf("union = %v, %v", u, err)
	}
	i, _, err := Overlay([][]s2.Point{a, b}, ref, 0, 2)
	if err != nil || len(i) != 1 {
		t.Fatalf("intersection = %v, %v", i, err)
	}
	rb := []s2.Point{b[3], b[2], b[1], b[0]}
	d, _, err := Overlay([][]s2.Point{a, rb}, ref, 0, 1)
	if err != nil || len(d) != 1 {
		t.Fatalf("difference = %v, %v", d, err)
	}
	if got, want := area(u), loopArea(a)+loopArea(b)-area(i); math.Abs(got-want) > 1e-12 {
		t.Errorf("area(union) = %v, want %v", got, want)
	}
	if got, want := area(d), loopArea(a)-area(i); math.Abs(got-want) > 1e-12 {
		t.Errorf("area(difference) = %v, want %v", got, want)
	}
	// The union's loop ends with the highest input edge's piece (b's last,
	// 7, from (1, 3) to where it crosses a).
	if n := len(u[0]); n != 8 || u[0][n-1] != ll(1, 3) {
		t.Errorf("union = %v, want 8 vertices ending at (1, 3)", u[0])
	}
	// Disjoint inputs: intersection empty, union two loops in input order.
	c := square(5, 5, 6, 6)
	if i, full, _ := Overlay([][]s2.Point{a, c}, ref, 0, 2); len(i) != 0 || full {
		t.Errorf("disjoint intersection = %v, full %v", i, full)
	}
	if u, _, _ := Overlay([][]s2.Point{a, c}, ref, 0, 1); len(u) != 2 || u[0][0] != a[0] || u[1][0] != c[0] {
		t.Errorf("disjoint union = %v", u)
	}
}

// TestBufferPointSides checks S2BufferOperation's regular polygons: 4n
// sides for n segments a quarter circle, and the tolerance examples' 5 and
// 23 sides, every vertex at the radius.
func TestBufferPointSides(t *testing.T) {
	const earth = 6371010.0
	for _, c := range []struct {
		frac  float64
		sides int
	}{
		{ErrorFractionForSegments(8), 8},
		{ErrorFractionForSegments(32), 32},
		{ClampErrorFraction(0.25), 5},
		{ClampErrorFraction(0.01), 23},
	} {
		p := ll(1, 2)
		r := s1.Angle(100 / earth)
		ls, full, same, err := Buffer(BufferOptions{Radius: r, ErrorFraction: c.frac}, []s2.Point{p}, nil, nil, false)
		if err != nil || full || same || len(ls) != 1 {
			t.Fatalf("Buffer = %v %v %v %v", ls, full, same, err)
		}
		if len(ls[0]) != c.sides {
			t.Errorf("error fraction %v: %d sides, want %d", c.frac, len(ls[0]), c.sides)
		}
		for _, v := range ls[0] {
			if d := float64(p.Distance(v)); math.Abs(d-float64(r)) > 1e-15 {
				t.Errorf("vertex at %v, want %v", d, r)
			}
		}
	}
}

// TestBufferLine checks a buffered line's area against 2rL + pi r^2 (round
// caps, as many sides as the circle) and 2rL (flat), and one side's half.
func TestBufferLine(t *testing.T) {
	const earth = 6371010.0
	line := []s2.Point{ll(0, 0), ll(0.1, 0)}
	r := s1.Angle(100 / earth)
	length := float64(line[0].Distance(line[1]))
	for _, c := range []struct {
		cap  EndCap
		side Side
		want float64
	}{
		{CapFlat, SideBoth, 2 * float64(r) * length},
		{CapFlat, SideLeft, float64(r) * length},
		{CapFlat, SideRight, float64(r) * length},
	} {
		ls, _, _, err := Buffer(BufferOptions{Radius: r, ErrorFraction: ErrorFractionForSegments(32), EndCap: c.cap, Side: c.side}, nil, [][]s2.Point{line}, nil, false)
		if err != nil || len(ls) != 1 {
			t.Fatalf("Buffer = %v, %v", ls, err)
		}
		if got := loopArea(ls[0]); math.Abs(got-c.want)/c.want > 1e-6 {
			t.Errorf("%v/%v: area %v, want %v", c.cap, c.side, got, c.want)
		}
	}
}

// TestPolylines checks S2Builder's undirected path assembly on the
// reference's ST_UNION example.
func TestPolylines(t *testing.T) {
	p, q, r := ll(-122.12, 47.67), ll(-122.19, 47.69), ll(-100.19, 47.69)
	got := Polylines([]Edge{{p, q, 0}, {p, r, 2}, {p, q, 4}})
	if len(got) != 1 || len(got[0]) != 3 || got[0][0] != r || got[0][1] != p || got[0][2] != q {
		t.Errorf("Polylines = %v", got)
	}
}

// TestSimplify checks BigQuery's reference examples of ST_SIMPLIFY.
func TestSimplify(t *testing.T) {
	const earth = 6371010.0
	tri := [][]s2.Point{{ll(0, 0), ll(0.1, 0), ll(0.1, 0.1)}}
	if s := Simplify(s1.Angle(1000/earth), nil, nil, tri); len(s.Loops) != 1 || len(s.Loops[0]) != 3 || s.Loops[0][0] != ll(0, 0) {
		t.Errorf("1 km: %+v", s)
	}
	if s := Simplify(s1.Angle(10000/earth), nil, nil, tri); len(s.Loops) != 0 || len(s.Lines) != 1 || len(s.Lines[0]) != 2 || s.Lines[0][0] != ll(0, 0) || s.Lines[0][1] != ll(0.1, 0.1) {
		t.Errorf("10 km: %+v", s)
	}
	if s := Simplify(s1.Angle(100000/earth), nil, nil, tri); len(s.Points) != 1 || s.Points[0] != ll(0, 0) || len(s.Lines)+len(s.Loops) != 0 {
		t.Errorf("100 km: %+v", s)
	}
	line := [][]s2.Point{{ll(0, 0), ll(0.05, 0), ll(0.1, 0), ll(0.15, 0), ll(2, 0)}}
	if s := Simplify(s1.Angle(1/earth), nil, line, nil); len(s.Lines) != 1 || len(s.Lines[0]) != 2 {
		t.Errorf("line: %+v", s)
	}
}
