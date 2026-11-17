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

// The predicates S2Builder needs that github.com/golang/geo does not port,
// from src/s2/s2predicates.cc (CompareEdgeDistance, CompareEdgeDirections,
// EdgeCircumcenterSign, GetVoronoiSiteExclusion, with their triage and
// exact stages) and src/s2/s2edge_distances.cc
// (GetUpdateMinDistanceMaxError, IsEdgeBNearEdgeA). Upstream's long double
// stage is skipped: Go has no long double, and the exact stage that follows
// decides every case the long double stage would have, with the same
// answer.

import (
	"math"
	"math/big"

	"github.com/golang/geo/r3"
	"github.com/golang/geo/s1"
	"github.com/golang/geo/s2"
)

// IntersectionError is S2::kIntersectionError (8 * DBL_ERR radians).
const IntersectionError = s1.Angle(8 * dblErrS2)

// IntersectionMergeRadius is S2::kIntersectionMergeRadius.
const IntersectionMergeRadius = 2 * IntersectionError

const sqrt3 = 1.7320508075688772935274463415058723669428052538103806280558

// tErr is rounding_epsilon<double>() (= DBL_ERR).
const tErr = dblErrS2

// k45Length2 is k45Degrees.length2().
const k45Length2 = 2 - math.Sqrt2

// Excluded is s2pred::Excluded.
type Excluded int

const (
	ExcludedFirst Excluded = iota
	ExcludedSecond
	ExcludedNeither
	ExcludedUncertain
)

// sign is s2pred::Sign(a, b, c) as an int (with symbolic perturbation).
func sign(a, b, c s2.Point) int { return int(s2.RobustSign(a, b, c)) }

func vlessThan(a, b r3.Vector) bool {
	if a.X != b.X {
		return a.X < b.X
	}
	if a.Y != b.Y {
		return a.Y < b.Y
	}
	return a.Z < b.Z
}

func getClosestVertex(x, a0, a1 r3.Vector) (r3.Vector, float64) {
	a0x2 := a0.Sub(x).Norm2()
	a1x2 := a1.Sub(x).Norm2()
	if a0x2 < a1x2 || (a0x2 == a1x2 && vlessThan(a0, a1)) {
		return a0, a0x2
	}
	return a1, a1x2
}

func getCosDistance(x, y r3.Vector) (float64, float64) {
	c := x.Dot(y)
	return c, 9.5*dblErrS2*math.Abs(c) + 1.5*dblErrS2
}

func getSin2Distance(x, y r3.Vector) (float64, float64) {
	n := x.Sub(y).Cross(x.Add(y))
	sin2 := 0.25 * n.Norm2()
	e := ((21+4*sqrt3)*dblErrS2*sin2 +
		32*sqrt3*dblErrS2*dblErrS2*math.Sqrt(sin2) +
		768*dblErrS2*dblErrS2*dblErrS2*dblErrS2)
	return sin2, e
}

func triageCompareCosDistance(x, y r3.Vector, r2 float64) int {
	cosXY, cosXYErr := getCosDistance(x, y)
	cosR := 1 - 0.5*r2
	cosRErr := 2 * tErr * cosR
	diff := cosXY - cosR
	e := cosXYErr + cosRErr
	if diff > e {
		return -1
	}
	if diff < -e {
		return 1
	}
	return 0
}

func triageCompareSin2Distance(x, y r3.Vector, r2 float64) int {
	sin2XY, sin2XYErr := getSin2Distance(x, y)
	sin2R := r2 * (1 - 0.25*r2)
	sin2RErr := 3 * tErr * sin2R
	diff := sin2XY - sin2R
	e := sin2XYErr + sin2RErr
	if diff > e {
		return 1
	}
	if diff < -e {
		return -1
	}
	return 0
}

func triageCompareDistance(x, y r3.Vector, r2 float64) int {
	s := triageCompareCosDistance(x, y, r2)
	if s == 0 && r2 < k45Length2 {
		s = triageCompareSin2Distance(x, y, r2)
	}
	return s
}

func triageCompareLineSin2Distance(x, a0, a1 r3.Vector, r2 float64, n r3.Vector, n1, n2 float64) int {
	if r2 >= 2.0 {
		return -1
	}
	n2sin2R := n2 * r2 * (1 - 0.25*r2)
	n2sin2RErr := 6 * tErr * n2sin2R
	cv, ax2 := getClosestVertex(x, a0, a1)
	xDn := x.Sub(cv).Dot(n)
	xDn2 := xDn * xDn
	c1 := (((3.5+2*sqrt3)*n1 + 32*sqrt3*dblErrS2) * tErr * math.Sqrt(ax2))
	xDn2Err := 4*tErr*xDn2 + (2*math.Abs(xDn)+c1)*c1
	// T_ERR == DBL_ERR for double.
	n2sin2RErr += 8 * dblErrS2 * n2sin2R
	diff := xDn2 - n2sin2R
	e := xDn2Err + n2sin2RErr
	if diff > e {
		return 1
	}
	if diff < -e {
		return -1
	}
	return 0
}

func triageCompareLineCos2Distance(x, a0, a1 r3.Vector, r2 float64, n r3.Vector, n1, n2 float64) int {
	if r2 >= 2.0 {
		return -1
	}
	cosR := 1 - 0.5*r2
	n2cos2R := n2 * cosR * cosR
	n2cos2RErr := 7 * tErr * n2cos2R
	xn := crossNF(x, n)
	m2 := dotNF(xn, xn)
	m1 := math.Sqrt(m2)
	m1Err := ((1+8/sqrt3)*n1 + 32*sqrt3*dblErrS2) * tErr
	m2Err := 3*tErr*m2 + (2*m1+m1Err)*m1Err
	n2cos2RErr += 8 * dblErrS2 * n2cos2R
	diff := m2 - n2cos2R
	e := m2Err + n2cos2RErr
	if diff > e {
		return -1
	}
	if diff < -e {
		return 1
	}
	return 0
}

func triageCompareLineDistance(x, a0, a1 r3.Vector, r2 float64, n r3.Vector, n1, n2 float64) int {
	if r2 < k45Length2 {
		return triageCompareLineSin2Distance(x, a0, a1, r2, n, n1, n2)
	}
	return triageCompareLineCos2Distance(x, a0, a1, r2, n, n1, n2)
}

func triageCompareEdgeDistance(x, a0, a1 r3.Vector, r2 float64) int {
	n := crossNF(a0.Sub(a1), a0.Add(a1))
	m := crossNF(n, x)
	a0Dir := a0.Sub(x)
	a1Dir := a1.Sub(x)
	a0Sign := dotNF(a0Dir, m)
	a1Sign := dotNF(a1Dir, m)
	n2 := dotNF(n, n)
	n1 := math.Sqrt(n2)
	n1Err := ((3.5+8/sqrt3)*n1 + 32*sqrt3*dblErrS2) * tErr
	a0SignErr := n1Err * math.Sqrt(dotNF(a0Dir, a0Dir))
	a1SignErr := n1Err * math.Sqrt(dotNF(a1Dir, a1Dir))
	if a0Sign < a0SignErr && a1Sign > -a1SignErr {
		if a0Sign > -a0SignErr || a1Sign < a1SignErr {
			vertexSign := min(triageCompareDistance(x, a0, r2), triageCompareDistance(x, a1, r2))
			lineSign := triageCompareLineDistance(x, a0, a1, r2, n, n1, n2)
			if vertexSign == lineSign {
				return lineSign
			}
			return 0
		}
		return triageCompareLineDistance(x, a0, a1, r2, n, n1, n2)
	}
	return min(triageCompareDistance(x, a0, r2), triageCompareDistance(x, a1, r2))
}

// ---- exact arithmetic ----

func xf(f float64) *big.Float { return new(big.Float).SetPrec(big.MaxPrec).SetFloat64(f) }
func xmul(a, b *big.Float) *big.Float {
	return new(big.Float).SetPrec(big.MaxPrec).Mul(a, b)
}
func xsub(a, b *big.Float) *big.Float {
	return new(big.Float).SetPrec(big.MaxPrec).Sub(a, b)
}
func xadd(a, b *big.Float) *big.Float {
	return new(big.Float).SetPrec(big.MaxPrec).Add(a, b)
}
func toExact(p s2.Point) r3.PreciseVector { return r3.PreciseVectorFromVector(p.Vector) }

func arePointsLinearlyDependent(x, y r3.PreciseVector) bool { return x.Cross(y).IsZero() }

func exactCompareLineDistance(x, a0, a1 r3.PreciseVector, r2 float64) int {
	if r2 >= 2.0 {
		return -1
	}
	n := a0.Cross(a1)
	sinD := x.Dot(n)
	xr2 := xf(r2)
	sin2R := xmul(xr2, xsub(xf(1), xmul(xf(0.25), xr2)))
	cmp := xsub(xmul(sinD, sinD), xmul(xmul(sin2R, x.Norm2()), n.Norm2()))
	return cmp.Sign()
}

func exactCompareEdgeDistance(x, a0, a1 s2.Point, r s1.ChordAngle) int {
	negX := s2.Point{Vector: x.Mul(-1)}
	if a0 != negX && a1 != negX &&
		CompareEdgeDirections(a0, a1, a0, x) > 0 &&
		CompareEdgeDirections(a0, a1, x, a1) > 0 {
		return exactCompareLineDistance(toExact(x), toExact(a0), toExact(a1), float64(r))
	}
	return min(s2.CompareDistance(x, a0, r), s2.CompareDistance(x, a1, r))
}

// CompareEdgeDistance is s2pred::CompareEdgeDistance: -1, 0 or +1 as the
// distance from x to edge a0a1 is less than, equal to or greater than r.
func CompareEdgeDistance(x, a0, a1 s2.Point, r s1.ChordAngle) int {
	s := triageCompareEdgeDistance(x.Vector, a0.Vector, a1.Vector, float64(r))
	if s != 0 {
		return s
	}
	if a0 == a1 {
		return s2.CompareDistance(x, a0, r)
	}
	return exactCompareEdgeDistance(x, a0, a1, r)
}

func triageCompareEdgeDirections(a0, a1, b0, b1 r3.Vector) int {
	na := a0.Sub(a1).Cross(a0.Add(a1))
	nb := b0.Sub(b1).Cross(b0.Add(b1))
	naLen, nbLen := na.Norm(), nb.Norm()
	cosAB := na.Dot(nb)
	cosABErr := ((5+4*sqrt3)*naLen*nbLen + 32*sqrt3*dblErrS2*(naLen+nbLen)) * tErr
	if cosAB > cosABErr {
		return 1
	}
	if cosAB < -cosABErr {
		return -1
	}
	return 0
}

// CompareEdgeDirections is s2pred::CompareEdgeDirections: the sign of the
// dot product of the two edges' normals.
func CompareEdgeDirections(a0, a1, b0, b1 s2.Point) int {
	s := triageCompareEdgeDirections(a0.Vector, a1.Vector, b0.Vector, b1.Vector)
	if s != 0 {
		return s
	}
	if a0 == a1 || b0 == b1 {
		return 0
	}
	return toExact(a0).Cross(toExact(a1)).Dot(toExact(b0).Cross(toExact(b1))).Sign()
}

func getCircumcenter(a, b, c r3.Vector) (r3.Vector, float64) {
	abDiff, abSum := a.Sub(b), a.Add(b)
	bcDiff, bcSum := b.Sub(c), b.Add(c)
	nab := abDiff.Cross(abSum)
	nabLen := nab.Norm()
	abLen := abDiff.Norm()
	nbc := bcDiff.Cross(bcSum)
	nbcLen := nbc.Norm()
	bcLen := bcDiff.Norm()
	mab := nab.Cross(abSum)
	mbc := nbc.Cross(bcSum)
	e := (((16+24*sqrt3)*tErr+8*dblErrS2*(abLen+bcLen))*nabLen*nbcLen +
		128*sqrt3*dblErrS2*tErr*(nabLen+nbcLen) +
		3*4096*dblErrS2*dblErrS2*tErr*tErr)
	return mab.Cross(mbc), e
}

func triageEdgeCircumcenterSign(x0, x1, a, b, c r3.Vector, abcSign int) int {
	z, zErr := getCircumcenter(a, b, c)
	nx := x0.Sub(x1).Cross(x0.Add(x1))
	result := float64(abcSign) * nx.Dot(z)
	zLen := z.Norm()
	nxLen := nx.Norm()
	nxErr := ((1+2*sqrt3)*nxLen + 32*sqrt3*dblErrS2) * tErr
	resultErr := ((3*tErr*nxLen+nxErr)*zLen + zErr*nxLen)
	if result > resultErr {
		return 1
	}
	if result < -resultErr {
		return -1
	}
	return 0
}

func clampSign(v int) int { return max(-1, min(1, v)) }

func exactEdgeCircumcenterSign(x0, x1, a, b, c r3.PreciseVector, abcSign int) int {
	if arePointsLinearlyDependent(x0, x1) {
		return 0
	}
	nx := x0.Cross(x1)
	dab := nx.Dot(a.Cross(b))
	dbc := nx.Dot(b.Cross(c))
	dca := nx.Dot(c.Cross(a))
	abc2 := xmul(a.Norm2(), xmul(dbc, dbc))
	bca2 := xmul(b.Norm2(), xmul(dca, dca))
	cab2 := xmul(c.Norm2(), xmul(dab, dab))
	lhs3Sgn, rhs3Sgn := dab.Sign(), -dbc.Sign()
	lhs2Sgn := clampSign(lhs3Sgn - rhs3Sgn)
	if lhs2Sgn == 0 && lhs3Sgn != 0 {
		lhs2Sgn = xsub(cab2, abc2).Sign() * lhs3Sgn
	}
	rhs2Sgn := -dca.Sign()
	result := clampSign(lhs2Sgn - rhs2Sgn)
	if result == 0 && lhs2Sgn != 0 {
		lhs4Sgn := dab.Sign() * dbc.Sign()
		rhs4 := xsub(xsub(bca2, cab2), abc2)
		result = clampSign(lhs4Sgn - rhs4.Sign())
		if result == 0 && lhs4Sgn != 0 {
			result = xsub(xmul(xmul(xf(4), abc2), cab2), xmul(rhs4, rhs4)).Sign() * lhs4Sgn
		}
		result *= lhs2Sgn
	}
	return abcSign * result
}

// unperturbedSign is s2pred::UnperturbedSign: Sign without symbolic
// perturbation (0 when the points are exactly collinear).
func unperturbedSign(a, b, c s2.Point) int {
	// The triage and stable stages of golang/geo's RobustSign return a
	// non-zero sign only when it is certain; otherwise compute exactly.
	det := toExact(a).Cross(toExact(b)).Dot(toExact(c))
	return det.Sign()
}

func symbolicEdgeCircumcenterSign(x0, x1, a, b, c s2.Point) int {
	if a == b || b == c || c == a {
		return 0
	}
	if pointLess(b, a) {
		a, b = b, a
	}
	if pointLess(c, b) {
		b, c = c, b
	}
	if pointLess(b, a) {
		a, b = b, a
	}
	if s := unperturbedSign(x0, x1, a); s != 0 {
		return s
	}
	if s := unperturbedSign(x0, x1, b); s != 0 {
		return s
	}
	return unperturbedSign(x0, x1, c)
}

// EdgeCircumcenterSign is s2pred::EdgeCircumcenterSign.
func EdgeCircumcenterSign(x0, x1, a, b, c s2.Point) int {
	abcSign := sign(a, b, c)
	s := triageEdgeCircumcenterSign(x0.Vector, x1.Vector, a.Vector, b.Vector, c.Vector, abcSign)
	if s != 0 {
		return s
	}
	if x0 == x1 || a == b || b == c || c == a {
		return 0
	}
	s = exactEdgeCircumcenterSign(toExact(x0), toExact(x1), toExact(a), toExact(b), toExact(c), abcSign)
	if s != 0 {
		return s
	}
	return symbolicEdgeCircumcenterSign(x0, x1, a, b, c)
}

func triageVoronoiSiteExclusion(a, b, x0, x1 r3.Vector, r2 float64) Excluded {
	n := x0.Sub(x1).Cross(x0.Add(x1))
	n2 := n.Norm2()
	n1 := math.Sqrt(n2)
	DnErr := ((3.5+2*sqrt3)*n1 + 32*sqrt3*dblErrS2) * tErr
	cosR := 1 - 0.5*r2
	sin2R := r2 * (1 - 0.25*r2)
	n2sin2R := n2 * sin2R
	cva, ax2 := getClosestVertex(a, x0, x1)
	aDn := a.Sub(cva).Dot(n)
	aDn2 := aDn * aDn
	aDnErr := DnErr * math.Sqrt(ax2)
	ra2 := n2sin2R - aDn2
	ra2Err := (8*dblErrS2+4*tErr)*aDn2 + (2*math.Abs(aDn)+aDnErr)*aDnErr + 6*tErr*n2sin2R
	minRa2 := ra2 - ra2Err
	if minRa2 < 0 {
		return ExcludedUncertain
	}
	ra := math.Sqrt(ra2)
	raErr := 1.5*tErr*ra + 0.5*ra2Err/math.Sqrt(minRa2)
	cvb, bx2 := getClosestVertex(b, x0, x1)
	bDn := b.Sub(cvb).Dot(n)
	bDn2 := bDn * bDn
	bDnErr := DnErr * math.Sqrt(bx2)
	rb2 := n2sin2R - bDn2
	rb2Err := (8*dblErrS2+4*tErr)*bDn2 + (2*math.Abs(bDn)+bDnErr)*bDnErr + 6*tErr*n2sin2R
	minRb2 := rb2 - rb2Err
	if minRb2 < 0 {
		return ExcludedUncertain
	}
	rb := math.Sqrt(rb2)
	rbErr := 1.5*tErr*rb + 0.5*rb2Err/math.Sqrt(minRb2)
	lhs3 := cosR * (rb - ra)
	absLhs3 := math.Abs(lhs3)
	lhs3Err := cosR*(raErr+rbErr) + 3*tErr*absLhs3
	aXb := a.Sub(b).Cross(a.Add(b))
	aXb1 := aXb.Norm()
	sinD := 0.5 * aXb.Dot(n)
	sinDErr := (4*dblErrS2+(2.5+2*sqrt3)*tErr)*aXb1*n1 + 16*sqrt3*dblErrS2*tErr*(aXb1+n1)
	result := absLhs3 - sinD
	resultErr := lhs3Err + sinDErr
	if result < -resultErr {
		return ExcludedNeither
	}
	if sinD < -sinDErr {
		const r90 = 2.0
		ca := triageCompareCosDistance(a, x0, r90)
		cb := triageCompareCosDistance(b, x1, r90)
		if ca < 0 && cb < 0 {
			return ExcludedNeither
		}
		if ca <= 0 && cb <= 0 {
			return ExcludedUncertain
		}
		if ca > 0 {
			return ExcludedFirst
		}
		return ExcludedSecond
	}
	if sinD <= sinDErr {
		return ExcludedUncertain
	}
	cosD := a.Dot(b)*n2 - aDn*bDn
	cosDErr := ((8*dblErrS2+5*tErr)*math.Abs(aDn)+aDnErr)*math.Abs(bDn) +
		(math.Abs(aDn)+aDnErr)*bDnErr + (8*dblErrS2+8*tErr)*n2
	if cosD <= -cosDErr {
		return ExcludedNeither
	}
	if cosD < cosDErr {
		return ExcludedUncertain
	}
	if result <= resultErr {
		return ExcludedUncertain
	}
	if lhs3 > 0 {
		return ExcludedFirst
	}
	return ExcludedSecond
}

// exactCompareDistanceRight is ExactCompareDistance(x, y, r90): with
// cos(r) == 0 it reduces to the sign of -x.y.
func exactCompareDistanceRight(x, y r3.PreciseVector) int { return -x.Dot(y).Sign() }

func preciseLess(a, b r3.PreciseVector) bool {
	if c := a.X.Cmp(b.X); c != 0 {
		return c < 0
	}
	if c := a.Y.Cmp(b.Y); c != 0 {
		return c < 0
	}
	return a.Z.Cmp(b.Z) < 0
}

func exactVoronoiSiteExclusion(a, b, x0, x1 r3.PreciseVector, r2f float64) Excluded {
	n := x0.Cross(x1)
	rhs2 := a.Cross(b).Dot(n)
	rhs2Sgn := rhs2.Sign()
	if rhs2Sgn < 0 {
		ca := exactCompareDistanceRight(a, x0)
		cb := exactCompareDistanceRight(b, x1)
		if ca < 0 && cb < 0 {
			return ExcludedNeither
		}
		if ca > 0 {
			return ExcludedFirst
		}
		return ExcludedSecond
	}
	r2 := xf(r2f)
	n2 := n.Norm2()
	aDn := a.Dot(n)
	bDn := b.Dot(n)
	cosD := xsub(xmul(a.Dot(b), n2), xmul(aDn, bDn))
	if cosD.Sign() < 0 {
		return ExcludedNeither
	}
	a2 := a.Norm2()
	b2 := b.Norm2()
	n2sin2R := xmul(xmul(r2, xsub(xf(1), xmul(xf(0.25), r2))), n2)
	sa2 := xmul(b2, xsub(xmul(n2sin2R, a2), xmul(aDn, aDn)))
	sb2 := xmul(a2, xsub(xmul(n2sin2R, b2), xmul(bDn, bDn)))
	lhs2Sgn := xsub(sb2, sa2).Sign()
	if lhs2Sgn == 0 {
		return ExcludedNeither
	}
	cosR := xsub(xf(1), xmul(xf(0.5), r2))
	cos2R := xmul(cosR, cosR)
	lhs3 := xsub(xmul(cos2R, xadd(sa2, sb2)), xmul(rhs2, rhs2))
	if lhs3.Sign() < 0 {
		return ExcludedNeither
	}
	lhs4 := xmul(lhs3, lhs3)
	rhs4 := xmul(xmul(xmul(xmul(xf(4), cos2R), cos2R), sa2), sb2)
	result := xsub(lhs4, rhs4).Sign()
	if result < 0 {
		return ExcludedNeither
	}
	if result == 0 {
		// a > b is the points' lexicographic order.
		if (lhs2Sgn > 0) == preciseLess(b, a) {
			return ExcludedNeither
		}
	}
	if lhs2Sgn > 0 {
		return ExcludedFirst
	}
	return ExcludedSecond
}

// GetVoronoiSiteExclusion is s2pred::GetVoronoiSiteExclusion: which of the
// sites a and b (a closer to x0) is excluded from snapping edge x0x1 with
// snap radius r.
func GetVoronoiSiteExclusion(a, b, x0, x1 s2.Point, r s1.ChordAngle) Excluded {
	if s2.CompareDistances(x1, a, b) < 0 {
		return ExcludedSecond
	}
	res := triageVoronoiSiteExclusion(a.Vector, b.Vector, x0.Vector, x1.Vector, float64(r))
	if res != ExcludedUncertain {
		return res
	}
	return exactVoronoiSiteExclusion(toExact(a), toExact(b), toExact(x0), toExact(x1), float64(r))
}

func getUpdateMinInteriorDistanceMaxError(dist s1.ChordAngle) float64 {
	if dist >= s1.RightChordAngle {
		return 0
	}
	b := math.Min(1.0, 0.5*float64(dist))
	a := math.Sqrt(b * (2 - b))
	const eps = 2 * dblErrS2
	return ((2.5+2*sqrt3+8.5*a)*a +
		(2+2*sqrt3/3+6.5*(1-b))*b +
		(23+16/sqrt3)*eps) * eps
}

// GetUpdateMinDistanceMaxError is S2::GetUpdateMinDistanceMaxError.
func GetUpdateMinDistanceMaxError(dist s1.ChordAngle) float64 {
	return math.Max(getUpdateMinInteriorDistanceMaxError(dist), dist.MaxPointError())
}

// IsEdgeBNearEdgeA is S2::IsEdgeBNearEdgeA.
func IsEdgeBNearEdgeA(a0, a1, b0, b1 s2.Point, tolerance s1.Angle) bool {
	aOrtho := s2.Point{Vector: normalizeNF(RobustCrossProd(a0, a1).Vector)}
	aNearestB0 := Project(b0, a0, a1, aOrtho.Vector)
	aNearestB1 := Project(b1, a0, a1, aOrtho.Vector)
	if sign(aOrtho, aNearestB0, aNearestB1) < 0 {
		aOrtho = s2.Point{Vector: aOrtho.Mul(-1)}
	}
	b0Distance := b0.Distance(aNearestB0)
	b1Distance := b1.Distance(aNearestB1)
	if b0Distance > tolerance || b1Distance > tolerance {
		return false
	}
	bOrtho := s2.Point{Vector: normalizeNF(RobustCrossProd(b0, b1).Vector)}
	planarAngle := aOrtho.Distance(bOrtho)
	if planarAngle <= tolerance {
		return true
	}
	if planarAngle >= s1.Angle(math.Pi/2) {
		return (b0.Distance(a0) < b0.Distance(a1)) == (b1.Distance(a0) < b1.Distance(a1))
	}
	furthest := s2.Point{Vector: normalizeNF(crossNF(bOrtho.Vector, RobustCrossProd(aOrtho, bOrtho).Vector))}
	furthestInv := s2.Point{Vector: furthest.Mul(-1)}
	return !((sign(bOrtho, b0, furthest) > 0 && sign(furthest, b1, bOrtho) > 0) ||
		(sign(bOrtho, b0, furthestInv) > 0 && sign(furthestInv, b1, bOrtho) > 0))
}

// GetIntersection is S2::GetIntersection (src/s2/s2edge_crossings.cc): the
// crossing point of edges a0a1 and b0b1, which must cross (CrossingSign >
// 0), within IntersectionError of the true point. Unlike golang/geo's
// s2.Intersection it breaks ties between equally long edges as
// internal::CompareEdges does and orients the result as upstream does.
func GetIntersection(a0, a1, b0, b1 s2.Point) s2.Point {
	if r, ok := getIntersectionStable(a0.Vector, a1.Vector, b0.Vector, b1.Vector); ok {
		return s2.Point{Vector: r}
	}
	return getIntersectionExact(a0, a1, b0, b1)
}

// compareEdgesForIntersection is internal::CompareEdges.
func compareEdgesForIntersection(a0, a1, b0, b1 r3.Vector) bool {
	if vlessThan(a1, a0) {
		a0, a1 = a1, a0
	}
	if vlessThan(b1, b0) {
		b0, b1 = b1, b0
	}
	return vlessThan(a0, b0) || (a0 == b0 && vlessThan(a1, b1))
}

func getIntersectionStable(a0, a1, b0, b1 r3.Vector) (r3.Vector, bool) {
	aLen2 := dotNF(a1.Sub(a0), a1.Sub(a0))
	bLen2 := dotNF(b1.Sub(b0), b1.Sub(b0))
	if aLen2 < bLen2 || (aLen2 == bLen2 && compareEdgesForIntersection(a0, a1, b0, b1)) {
		return getIntersectionStableSorted(b0, b1, a0, a1)
	}
	return getIntersectionStableSorted(a0, a1, b0, b1)
}

func getProjection(x, aNorm r3.Vector, aNormLen float64, a0, a1 r3.Vector) (float64, float64) {
	x0 := x.Sub(a0)
	x1 := x.Sub(a1)
	x0Dist2 := dotNF(x0, x0)
	x1Dist2 := dotNF(x1, x1)
	var dist, result float64
	if x0Dist2 < x1Dist2 || (x0Dist2 == x1Dist2 && vlessThan(x0, x1)) {
		dist = math.Sqrt(x0Dist2)
		result = dotNF(x0, aNorm)
	} else {
		dist = math.Sqrt(x1Dist2)
		result = dotNF(x1, aNorm)
	}
	e := (((3.5+2*sqrt3)*aNormLen+32*sqrt3*dblErrS2)*dist + 1.5*math.Abs(result)) * tErr
	return result, e
}

func getIntersectionStableSorted(a0, a1, b0, b1 r3.Vector) (r3.Vector, bool) {
	aNorm := crossNF(a0.Sub(a1), a0.Add(a1))
	aNormLen := math.Sqrt(dotNF(aNorm, aNorm))
	bLen := math.Sqrt(dotNF(b1.Sub(b0), b1.Sub(b0)))
	b0Dist, b0Err := getProjection(b0, aNorm, aNormLen, a0, a1)
	b1Dist, b1Err := getProjection(b1, aNorm, aNormLen, a0, a1)
	if b0Dist < b1Dist {
		b0Dist, b1Dist = -b0Dist, -b1Dist
	}
	distSum := b0Dist - b1Dist
	errSum := b0Err + b1Err
	if distSum <= errSum {
		return r3.Vector{}, false
	}
	x := r3.Vector{
		X: float64(b0Dist*b1.X) - float64(b1Dist*b0.X),
		Y: float64(b0Dist*b1.Y) - float64(b1Dist*b0.Y),
		Z: float64(b0Dist*b1.Z) - float64(b1Dist*b0.Z),
	}
	e := bLen*math.Abs(b0Dist*b1Err-b1Dist*b0Err)/(distSum-errSum) + 2*tErr*distSum
	xLen2 := dotNF(x, x)
	if xLen2 < 0x1p-1022 { // numeric_limits<double>::min()
		return r3.Vector{}, false
	}
	xLen := math.Sqrt(xLen2)
	if e > (IntersectionError.Radians()-tErr)*xLen {
		return r3.Vector{}, false
	}
	inv := 1 / xLen
	return r3.Vector{X: inv * x.X, Y: inv * x.Y, Z: inv * x.Z}, true
}

// dotNF and crossNF are r3.Vector's Dot and Cross with every product
// rounded (an explicit float64 conversion stops the Go compiler fusing a
// multiply and an add, which it may do on arm64): upstream compiles S2 with
// floating-point contraction off (s2/_fp_contract_off.h), and computed
// points must round as upstream's do.
func dotNF(a, b r3.Vector) float64 {
	return float64(float64(a.X*b.X)+float64(a.Y*b.Y)) + float64(a.Z*b.Z)
}

func crossNF(a, b r3.Vector) r3.Vector {
	return r3.Vector{
		X: float64(a.Y*b.Z) - float64(a.Z*b.Y),
		Y: float64(a.Z*b.X) - float64(a.X*b.Z),
		Z: float64(a.X*b.Y) - float64(a.Y*b.X),
	}
}

func getIntersectionExact(a0, a1, b0, b1 s2.Point) s2.Point {
	aNormXF := toExact(a0).Cross(toExact(a1))
	bNormXF := toExact(b0).Cross(toExact(b1))
	xXF := aNormXF.Cross(bNormXF)
	if !xXF.IsZero() {
		return s2.Point{Vector: normalizableFromExact(xXF).Normalize().Mul(float64(sign(a0, a1, b1)))}
	}
	aNorm := normalizableFromExact(aNormXF).Normalize()
	bNorm := normalizableFromExact(bNormXF).Normalize()
	if (aNorm == r3.Vector{}) {
		aNorm = symbolicCrossProd(a0, a1)
	}
	if (bNorm == r3.Vector{}) {
		bNorm = symbolicCrossProd(b0, b1)
	}
	x := s2.Point{Vector: r3.Vector{X: 10, Y: 10, Z: 10}}
	an, bn := s2.Point{Vector: aNorm}, s2.Point{Vector: bNorm}
	if s2.OrderedCCW(b0, a0, b1, bn) && pointLess(a0, x) {
		x = a0
	}
	if s2.OrderedCCW(b0, a1, b1, bn) && pointLess(a1, x) {
		x = a1
	}
	if s2.OrderedCCW(a0, b0, a1, an) && pointLess(b0, x) {
		x = b0
	}
	if s2.OrderedCCW(a0, b1, a1, an) && pointLess(b1, x) {
		x = b1
	}
	return x
}

// symbolicCrossProd is S2::internal::SymbolicCrossProd.
func symbolicCrossProd(a, b s2.Point) r3.Vector {
	if pointLess(a, b) {
		return ensureNormalizable(symbolicCrossProdSorted(a, b))
	}
	return ensureNormalizable(symbolicCrossProdSorted(b, a)).Mul(-1)
}

// normalizeNF is Vector3::Normalize without fused operations.
func normalizeNF(v r3.Vector) r3.Vector {
	n := math.Sqrt(dotNF(v, v))
	if n != 0 {
		n = 1.0 / n
	}
	return r3.Vector{X: v.X * n, Y: v.Y * n, Z: v.Z * n}
}

// Project is S2::Project(x, a, b, a_cross_b): the point of edge ab
// closest to x.
func Project(x, a, b s2.Point, aCrossB r3.Vector) s2.Point {
	if x == a || x == b {
		return x
	}
	n := s2.Point{Vector: normalizeNF(aCrossB)}
	p := s2.Point{Vector: normalizeNF(crossNF(RobustCrossProd(n, x).Vector, n.Vector))}
	if sign(p, n, a) > 0 && sign(p, n, b) < 0 {
		return p
	}
	xa, xb := x.Sub(a.Vector), x.Sub(b.Vector)
	if dotNF(xa, xa) <= dotNF(xb, xb) {
		return a
	}
	return b
}

// ProjectToEdge is S2::Project(x, a, b).
func ProjectToEdge(x, a, b s2.Point) s2.Point { return Project(x, a, b, RobustCrossProd(a, b).Vector) }
