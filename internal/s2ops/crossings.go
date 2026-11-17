// Copyright 2005 Google Inc. All Rights Reserved.
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

// Ported from src/s2/s2edge_crossings.cc (SignedVertexCrossing,
// RobustCrossProd and its helpers) and src/s2/s2edge_crosser.h
// (SignedEdgeOrVertexCrossing), which github.com/golang/geo does not port.

import (
	"math"
	"math/big"

	"github.com/golang/geo/r3"
	"github.com/golang/geo/s2"
)

// dblErrS2 is s2pred::DBL_ERR (half of DBL_EPSILON).
const dblErrS2 = 0x1p-53

// RobustCrossProdError is S2::kRobustCrossProdError (6 * DBL_ERR).
const RobustCrossProdError = 6 * dblErrS2

// SignedVertexCrossing is S2::SignedVertexCrossing: like VertexCrossing,
// but +1 if both edges are outgoing or both incoming at the shared vertex
// (and the crossing counts), -1 if one is incoming and the other outgoing.
func SignedVertexCrossing(a, b, c, d s2.Point) int {
	if a == b || c == d {
		return 0
	}
	// See VertexCrossing. The sign of the crossing is +1 if both edges are
	// outgoing or both edges are incoming with respect to the common vertex
	// and -1 otherwise.
	if a == c {
		if b == d || s2.OrderedCCW(s2.Ortho(a), d, b, a) {
			return 1
		}
		return 0
	}
	if b == d {
		if s2.OrderedCCW(s2.Ortho(b), c, a, b) {
			return 1
		}
		return 0
	}
	if a == d {
		if b == c || s2.OrderedCCW(s2.Ortho(a), c, b, a) {
			return -1
		}
		return 0
	}
	if b == c {
		if s2.OrderedCCW(s2.Ortho(b), d, a, b) {
			return -1
		}
		return 0
	}
	// SignedVertexCrossing called with 4 distinct vertices.
	return 0
}

// SignedEdgeOrVertexCrossing is S2EdgeCrosser::SignedEdgeOrVertexCrossing
// for the edge AB and CD: 0 if they do not cross, -1 if AB crosses CD from
// left to right, +1 from right to left; at a shared vertex,
// SignedVertexCrossing.
func SignedEdgeOrVertexCrossing(a, b, c, d s2.Point) int {
	switch s2.CrossingSign(a, b, c, d) {
	case s2.DoNotCross:
		return 0
	case s2.Cross:
		// When AB crosses CD, the crossing sign is Sign(ABC).
		return int(s2.RobustSign(a, b, c))
	}
	return SignedVertexCrossing(a, b, c, d)
}

// RobustCrossProd is S2::RobustCrossProd: a vector orthogonal to a and b
// (not necessarily unit length), stable for nearly parallel inputs and
// symbolically perturbed for exactly proportional ones. Go has no long
// double, so the long-double stage upstream tries before exact arithmetic is
// skipped (as on platforms where long double is double); the exact stage's
// result is the same direction.
func RobustCrossProd(a, b s2.Point) s2.Point {
	if r, ok := stableCrossProd(a.Vector, b.Vector); ok {
		return s2.Point{Vector: r}
	}
	if a == b {
		return s2.Ortho(a)
	}
	return s2.Point{Vector: exactCrossProd(a, b)}
}

// stableCrossProd is internal::GetStableCrossProd in double precision.
func stableCrossProd(a, b r3.Vector) (r3.Vector, bool) {
	const tErr = dblErrS2
	kMinNorm := (32 * math.Sqrt(3) * dblErrS2) / (RobustCrossProdError/tErr - (1 + 2*math.Sqrt(3)))
	r := a.Sub(b).Cross(a.Add(b))
	return r, r.Norm2() >= kMinNorm*kMinNorm
}

func exactCrossProd(a, b s2.Point) r3.Vector {
	ax, bx := r3.PreciseVectorFromVector(a.Vector), r3.PreciseVectorFromVector(b.Vector)
	xf := ax.Cross(bx)
	if !xf.IsZero() {
		return normalizableFromExact(xf)
	}
	if pointLess(a, b) {
		return ensureNormalizable(symbolicCrossProdSorted(a, b))
	}
	return ensureNormalizable(symbolicCrossProdSorted(b, a)).Mul(-1)
}

// symbolicCrossProdSorted is SymbolicCrossProdSorted (a < b, a x b == 0
// exactly): the cross product under S2's symbolic perturbations.
func symbolicCrossProdSorted(a, b s2.Point) r3.Vector {
	if b.X != 0 || b.Y != 0 { // da[2]
		return r3.Vector{X: -b.Y, Y: b.X, Z: 0}
	}
	if b.Z != 0 { // da[1]
		return r3.Vector{X: b.Z, Y: 0, Z: 0}
	}
	if a.X != 0 || a.Y != 0 { // db[2]
		return r3.Vector{X: a.Y, Y: -a.X, Z: 0}
	}
	return r3.Vector{X: 1, Y: 0, Z: 0} // db[2] * da[1]
}

func isNormalizable(p r3.Vector) bool {
	return math.Max(math.Abs(p.X), math.Max(math.Abs(p.Y), math.Abs(p.Z))) >= math.Ldexp(1, -242)
}

func ensureNormalizable(p r3.Vector) r3.Vector {
	if !isNormalizable(p) {
		pMax := math.Max(math.Abs(p.X), math.Max(math.Abs(p.Y), math.Abs(p.Z)))
		return p.Mul(math.Ldexp(2, -1-math.Ilogb(pMax)))
	}
	return p
}

// normalizableFromExact is NormalizableFromExact.
func normalizableFromExact(xf r3.PreciseVector) r3.Vector {
	x := xf.Vector()
	if isNormalizable(x) {
		return x
	}
	// Scale so that the largest component magnitude is in [0.5, 1).
	exp := math.MinInt
	for _, c := range []*big.Float{xf.X, xf.Y, xf.Z} {
		if c.Sign() != 0 {
			exp = max(exp, c.MantExp(nil))
		}
	}
	if exp == math.MinInt {
		return r3.Vector{}
	}
	scale := func(c *big.Float) float64 {
		f, _ := new(big.Float).SetMantExp(c, -exp).Float64()
		return f
	}
	return r3.Vector{X: scale(xf.X), Y: scale(xf.Y), Z: scale(xf.Z)}
}
