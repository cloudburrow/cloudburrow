package s2ops

import (
	"math"
	"testing"

	"github.com/golang/geo/s1"
	"github.com/golang/geo/s2"
)

func ll(lng, lat float64) s2.Point { return s2.PointFromLatLng(s2.LatLngFromDegrees(lat, lng)) }

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
		poly, same, err := Buffer(BufferOptions{Radius: r, ErrorFraction: c.frac}, []s2.Point{p}, nil, nil, false)
		if err != nil || same || poly == nil || poly.IsFull() || poly.NumLoops() != 1 {
			t.Fatalf("Buffer = %v %v %v", poly, same, err)
		}
		vs := poly.Loop(0).Vertices()
		if len(vs) != c.sides {
			t.Errorf("error fraction %v: %d sides, want %d", c.frac, len(vs), c.sides)
		}
		for _, v := range vs {
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
		poly, _, err := Buffer(BufferOptions{Radius: r, ErrorFraction: ErrorFractionForSegments(32), EndCap: c.cap, Side: c.side}, nil, [][]s2.Point{line}, nil, false)
		if err != nil || poly == nil || poly.NumLoops() != 1 {
			t.Fatalf("Buffer = %v, %v", poly, err)
		}
		if got := poly.Area(); math.Abs(got-c.want)/c.want > 1e-6 {
			t.Errorf("%v/%v: area %v, want %v", c.cap, c.side, got, c.want)
		}
	}
}
