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

// Cases ported from src/s2/s2predicates_test.cc. Upstream's long double
// stage does not exist here, so a case upstream decides in long double is
// expected to reach the exact stage (dbl undecided).

import (
	"math"
	"testing"

	"github.com/golang/geo/r3"
	"github.com/golang/geo/s1"
	"github.com/golang/geo/s2"
)

type precision int

const (
	precDouble precision = iota
	precLongDouble
	precExact
	precSymbolic
)

func pp(x, y, z float64) s2.Point { return s2.Point{Vector: r3.Vector{X: x, Y: y, Z: z}.Normalize()} }

func neg(p s2.Point) s2.Point { return s2.Point{Vector: p.Mul(-1)} }

func caRadians(r float64) s1.ChordAngle { return s1.ChordAngleFromAngle(s1.Angle(r)) }

func TestPredicateCompareEdgeDistance(t *testing.T) {
	const eps = 2 * dblErrS2
	for i, c := range []struct {
		x, a0, a1 s2.Point
		r         s1.ChordAngle
		sign      int
		prec      precision
	}{
		{pp(1, 1e-10, 1e-15), pp(1, 0, 0), pp(0, 1, 0), caRadians(1e-15 + eps), -1, precDouble},
		{pp(1, 1, 1e-15), pp(1, 0, 0), pp(0, 1, 0), caRadians(1e-15 + eps), -1, precLongDouble},
		{pp(1, 1, 1e-40), pp(1, 0, 0), pp(0, 1, 0), caRadians(1e-40), -1, precExact},
		{pp(1, 1, 0), pp(1, 0, 0), pp(0, 1, 0), 0, 0, precExact},
		{pp(1e-15, 0, 1), pp(1, 0, 0), pp(0, 1, 0), caRadians(math.Pi/2 - 1e-15 - 3*eps), 1, precDouble},
		{pp(1e-15, 0, 1), pp(1, 0, 0), pp(0, 1, 0), caRadians(math.Pi/2 - 1e-15 - eps), 1, precLongDouble},
		{pp(1e-40, 0, 1), pp(1, 0, 0), pp(0, 1, 0), s1.RightChordAngle, -1, precExact},
		{pp(0, 0, 1), pp(1, 0, 0), pp(0, 1, 0), s1.RightChordAngle, 0, precExact},
		{pp(1e-15, -1, 0), pp(1, 0, 0), pp(1, 1, 0), s1.RightChordAngle, -1, precDouble},
		{pp(-1, -1, 1), pp(1, 0, 0), pp(1, 1, 0), s1.RightChordAngle, 1, precDouble},
		{pp(1e-18, -1, 0), pp(1, 0, 0), pp(1, 1, 0), s1.RightChordAngle, -1, precLongDouble},
		{pp(1e-100, -1, 0), pp(1, 0, 0), pp(1, 1, 0), s1.RightChordAngle, -1, precExact},
		{pp(0, -1, 0), pp(1, 0, 0), pp(1, 1, 0), s1.RightChordAngle, 0, precExact},
		{pp(-1, 0, 0), pp(1, 0, 0), pp(1, 1, 0), s1.RightChordAngle, 1, precDouble},
		{pp(-1, 0, 0), pp(1, 0, 0), pp(1e-18, 1, 0), s1.RightChordAngle, 1, precLongDouble},
		{pp(-1, 0, 0), pp(1, 0, 0), pp(1e-100, 1, 0), s1.RightChordAngle, 1, precExact},
		{pp(0, -1, 0), pp(1, 0, 0), pp(0, 1, 0), s1.RightChordAngle, 0, precExact},
	} {
		dbl := triageCompareEdgeDistance(c.x.Vector, c.a0.Vector, c.a1.Vector, float64(c.r))
		exact := exactCompareEdgeDistance(c.x, c.a0, c.a1, c.r)
		if exact != c.sign {
			t.Errorf("case %d: exact %d, want %d", i, exact, c.sign)
		}
		if dbl != 0 && dbl != exact {
			t.Errorf("case %d: double %d, exact %d", i, dbl, exact)
		}
		if (dbl != 0) != (c.prec == precDouble) {
			t.Errorf("case %d: double decided %v, upstream precision %d", i, dbl != 0, c.prec)
		}
		if got := CompareEdgeDistance(c.x, c.a0, c.a1, c.r); got != c.sign {
			t.Errorf("case %d: CompareEdgeDistance %d, want %d", i, got, c.sign)
		}
	}
}

func TestPredicateCompareEdgeDirections(t *testing.T) {
	for i, c := range []struct {
		a0, a1, b0, b1 s2.Point
		sign           int
		prec           precision
	}{
		{pp(1, 0, 0), pp(1, 1, 0), pp(1, -1, 0), pp(1, 0, 0), 1, precDouble},
		{pp(1, 0, 1.5e-15), pp(1, 1, 0), pp(0, -1, 0), pp(0, 0, 1), 1, precDouble},
		{pp(1, 0, 1e-18), pp(1, 1, 0), pp(0, -1, 0), pp(0, 0, 1), 1, precLongDouble},
		{pp(1, 0, 1e-50), pp(1, 1, 0), pp(0, -1, 0), pp(0, 0, 1), 1, precExact},
		{pp(1, 0, 0), pp(1, 1, 0), pp(0, -1, 0), pp(0, 0, 1), 0, precExact},
	} {
		a0, a1, b0, b1 := c.a0, c.a1, c.b0, c.b1
		dbl := triageCompareEdgeDirections(a0.Vector, a1.Vector, b0.Vector, b1.Vector)
		if (dbl != 0) != (c.prec == precDouble) {
			t.Errorf("case %d: double decided %v, upstream precision %d", i, dbl != 0, c.prec)
		}
		s := c.sign
		for j, got := range []int{
			CompareEdgeDirections(a0, a1, b0, b1), CompareEdgeDirections(b0, b1, a0, a1),
			CompareEdgeDirections(neg(a0), neg(a1), b0, b1), CompareEdgeDirections(a0, a1, neg(b0), neg(b1)),
			-CompareEdgeDirections(a1, a0, b0, b1), -CompareEdgeDirections(a0, a1, b1, b0),
			-CompareEdgeDirections(neg(a0), a1, b0, b1), -CompareEdgeDirections(a0, neg(a1), b0, b1),
			-CompareEdgeDirections(a0, a1, neg(b0), b1), -CompareEdgeDirections(a0, a1, b0, neg(b1)),
		} {
			if got != s {
				t.Errorf("case %d variant %d: %d, want %d", i, j, got, s)
			}
		}
	}
}

func TestPredicateEdgeCircumcenterSign(t *testing.T) {
	for i, c := range []struct {
		x0, x1, a, b, cc s2.Point
		sign             int
		prec             precision
	}{
		{pp(1, 0, 0), pp(1, 1, 0), pp(0, 0, 1), pp(1, 0, 1), pp(0, 1, 1), 1, precDouble},
		{pp(1, 0, 0), pp(1, 1, 0), pp(0, 0, -1), pp(1, 0, -1), pp(0, 1, -1), -1, precDouble},
		{pp(1, -1, 0), pp(1, 1, 0), pp(1, -1e-5, 1), pp(1, 1e-5, -1), pp(1, 1-1e-5, 1e-5), -1, precDouble},
		{pp(1, -1, 0), pp(1, 1, 0), pp(1, -1e-5, 1), pp(1, 1e-5, -1), pp(1, 1-1e-9, 1e-5), -1, precLongDouble},
		{pp(1, -1, 0), pp(1, 1, 0), pp(1, -1e-5, 1), pp(1, 1e-5, -1), pp(1, 1-1e-15, 1e-5), -1, precExact},
		{pp(1, -1, 0), pp(1, 1, 0), pp(1, -1e-5, 1), pp(1, 1e-5, -1), pp(1, 1, 1e-5), 1, precSymbolic},
		{pp(1, -1, 0), pp(1, 1, 0), pp(0, -1, 0), pp(0, 0, -1), pp(0, 0, 1), -1, precSymbolic},
		{pp(0, -1, 1), pp(0, 1, 1), pp(0, 1, 0), pp(0, -1, 0), pp(1, 0, 0), -1, precSymbolic},
	} {
		x0, x1, a, b, cc := c.x0, c.x1, c.a, c.b, c.cc
		abc := sign(a, b, cc)
		dbl := triageEdgeCircumcenterSign(x0.Vector, x1.Vector, a.Vector, b.Vector, cc.Vector, abc)
		exact := exactEdgeCircumcenterSign(toExact(x0), toExact(x1), toExact(a), toExact(b), toExact(cc), abc)
		if (dbl != 0) != (c.prec == precDouble) {
			t.Errorf("case %d: double decided %v, upstream precision %d", i, dbl != 0, c.prec)
		}
		if (exact == 0) != (c.prec == precSymbolic) {
			t.Errorf("case %d: exact %d, upstream precision %d", i, exact, c.prec)
		}
		s := c.sign
		for j, got := range []int{
			EdgeCircumcenterSign(x0, x1, a, b, cc), EdgeCircumcenterSign(x0, x1, a, cc, b),
			EdgeCircumcenterSign(x0, x1, b, a, cc), EdgeCircumcenterSign(x0, x1, b, cc, a),
			EdgeCircumcenterSign(x0, x1, cc, a, b), EdgeCircumcenterSign(x0, x1, cc, b, a),
			-EdgeCircumcenterSign(x1, x0, a, b, cc), EdgeCircumcenterSign(neg(x0), neg(x1), a, b, cc),
		} {
			if got != s {
				t.Errorf("case %d variant %d: %d, want %d", i, j, got, s)
			}
		}
		if exact != 0 {
			if got := EdgeCircumcenterSign(x0, x1, neg(a), neg(b), neg(cc)); got != -s {
				t.Errorf("case %d: negated sites %d, want %d", i, got, -s)
			}
		}
	}
}

func TestPredicateVoronoiSiteExclusion(t *testing.T) {
	deg := func(d float64) s1.ChordAngle { return s1.ChordAngleFromAngle(s1.Angle(d) * s1.Degree) }
	for i, c := range []struct {
		a, b, x0, x1 s2.Point
		r            s1.ChordAngle
		want         Excluded
		prec         precision
	}{
		{pp(1, -1e-5, 0), pp(1, -2e-5, 0), pp(1, 0, 0), pp(1, 1, 0), caRadians(1e-3), ExcludedSecond, precDouble},
		{pp(1, 1, 1e-30), pp(1, 1, -1e-20), pp(1, 0, 0), pp(1, 1, 0), caRadians(1e-10), ExcludedSecond, precDouble},
		{pp(1, -1e-10, 1e-5), pp(1, 1e-10, -1e-5), pp(1, -1, 0), pp(1, 1, 0), caRadians(1e-4), ExcludedNeither, precDouble},
		{pp(1, -1e-10, 1e-5), pp(1, 1e-10, -1e-5), pp(1, -1, 0), pp(1, 1, 0), caRadians(1e-5), ExcludedNeither, precLongDouble},
		{pp(1, -1e-17, 1e-5), pp(1, 1e-17, -1e-5), pp(1, -1, 0), pp(1, 1, 0), caRadians(1e-4), ExcludedNeither, precLongDouble},
		{pp(1, -1e-20, 1e-5), pp(1, 1e-20, -1e-5), pp(1, -1, 0), pp(1, 1, 0), caRadians(1e-5), ExcludedNeither, precExact},
		{pp(1, -1e-6, 1.0049999999e-5), pp(1, 0, -1e-5), pp(1, -1, 0), pp(1, 1, 0), caRadians(1.005e-5), ExcludedFirst, precDouble},
		{pp(1, -1.00105e-6, 1.0049999999e-5), pp(1, 0, -1e-5), pp(1, -1, 0), pp(1, 1, 0), caRadians(1.005e-5), ExcludedFirst, precLongDouble},
		{pp(1, -1e-6, 1.005e-5), pp(1, 0, -1e-5), pp(1, -1, 0), pp(1, 1, 0), caRadians(1.005e-5), ExcludedFirst, precLongDouble},
		{pp(1, -1e-31, 1.005e-30), pp(1, 0, -1e-30), pp(1, -1, 0), pp(1, 1, 0), caRadians(1.005e-30), ExcludedFirst, precExact},
		{pp(1, -1e-5, 1e-4), pp(1, -1.00000001e-5, 0), pp(-1, -1, 0), pp(1, 0, 0), caRadians(1), ExcludedFirst, precDouble},
		{pp(1, 1e-10, 0.1), pp(1, -1e-10, 1e-8), pp(-1, -1, 0), pp(1, 0, 0), caRadians(1), ExcludedFirst, precDouble},
		{pp(1, 2e-10, 0.1), pp(1, 1e-10, 0), pp(-1, -1, 0), pp(1, 0, 0), caRadians(1), ExcludedFirst, precDouble},
		{pp(1, 1.1, 0), pp(1, 1.01, 0.01), pp(-1, -1, 0), pp(1, 0, 0), caRadians(1), ExcludedFirst, precDouble},
		{pp(1, 1.1, 0), pp(1, -1, 0), pp(-1, 0, 0), pp(1, -1e-10, 0), deg(70), ExcludedFirst, precDouble},
		{pp(-1, 0.1, 0.001), pp(1, 1.1, 0), pp(-1, -1, 0), pp(1, 0, 0), caRadians(1), ExcludedNeither, precDouble},
		{pp(-1, 0.1, 0), pp(1, 1.1, 0.001), pp(-1, -1, 0), pp(1, 0, 0), caRadians(1), ExcludedNeither, precDouble},
		{pp(0, 1, 1), pp(1, 0, 1), pp(0, 1, 1), pp(1, 0, -1), s1.ChordAngleFromSquaredLength(1), ExcludedNeither, precExact},
		{pp(0, 1, 1), pp(-1, 0, 1), pp(0, 1, 1), pp(-1, 0, -1), s1.ChordAngleFromSquaredLength(1), ExcludedSecond, precExact},
	} {
		a, b, x0, x1 := c.a, c.b, c.x0, c.x1
		if s2.CompareDistances(x1, a, b) < 0 {
			if c.want != ExcludedSecond || c.prec != precDouble {
				t.Errorf("case %d: decided by distance, want %d", i, c.want)
			}
		} else {
			dbl := triageVoronoiSiteExclusion(a.Vector, b.Vector, x0.Vector, x1.Vector, float64(c.r))
			exact := exactVoronoiSiteExclusion(toExact(a), toExact(b), toExact(x0), toExact(x1), float64(c.r))
			if exact != c.want {
				t.Errorf("case %d: exact %d, want %d", i, exact, c.want)
			}
			if dbl != ExcludedUncertain && dbl != exact {
				t.Errorf("case %d: double %d, exact %d", i, dbl, exact)
			}
			if (dbl != ExcludedUncertain) != (c.prec == precDouble) {
				t.Errorf("case %d: double decided %v, upstream precision %d", i, dbl != ExcludedUncertain, c.prec)
			}
		}
		if got := GetVoronoiSiteExclusion(a, b, x0, x1, c.r); got != c.want {
			t.Errorf("case %d: GetVoronoiSiteExclusion %d, want %d", i, got, c.want)
		}
		swapped := c.want
		switch c.want {
		case ExcludedFirst:
			swapped = ExcludedSecond
		case ExcludedSecond:
			swapped = ExcludedFirst
		}
		if s2.CompareDistances(x1, b, a) < 0 {
			if got := GetVoronoiSiteExclusion(b, a, x1, x0, c.r); got != swapped {
				t.Errorf("case %d: swapped %d, want %d", i, got, swapped)
			}
		}
	}
}
