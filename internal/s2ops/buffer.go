// Copyright 2020 Google Inc. All Rights Reserved.
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

// Ported from src/s2/s2buffer_operation.cc of the S2 Geometry Library
// (https://github.com/google/s2geometry, commit
// 677c8cebcff11ba11accc0b02005bc214e06e9fd; author ericv@google.com). The
// offset curves and the reference point's winding number follow the C++;
// the resulting paths go to Overlay (overlay.go) in place of
// S2WindingOperation.

package s2ops

import (
	"errors"
	"math"

	"github.com/golang/geo/s1"
	"github.com/golang/geo/s2"
)

// EndCap and Side are S2BufferOperation's EndCapStyle and PolylineSide.
type EndCap int

const (
	CapRound EndCap = iota
	CapFlat
)

type Side int

const (
	SideBoth Side = iota
	SideLeft
	SideRight
)

const (
	dblErr = 0x1p-53 // s2pred::DBL_ERR
	// MinErrorFraction is S2BufferOperation::Options::kMinErrorFraction.
	MinErrorFraction = 1e-6
	// MaxCircleSegments is S2BufferOperation::Options::kMaxCircleSegments.
	MaxCircleSegments = 1570.7963267948965
	minRequestedError = s1.Angle(2 * dblErr)
	// kGetPointOnLineError + kGetPointOnRayPerpendicularError.
	maxAbsInterpolationError = s1.Angle((4 + 2/1.7320508075688772 + 6 + 3) * dblErr)
)

// BufferOptions are S2BufferOperation::Options.
type BufferOptions struct {
	Radius        s1.Angle
	ErrorFraction float64
	EndCap        EndCap
	Side          Side
}

// ErrorFractionForSegments is Options::set_circle_segments' conversion.
func ErrorFractionForSegments(n float64) float64 {
	n = math.Max(2, math.Min(MaxCircleSegments, n))
	return 1 - math.Cos(math.Pi/n) + 1e-15
}

// ClampErrorFraction is Options::set_error_fraction's clamp.
func ClampErrorFraction(e float64) float64 { return math.Max(MinErrorFraction, math.Min(1, e)) }

type buffer struct {
	opt                             BufferOptions
	sign                            int
	absRadius                       s1.ChordAngle
	vertexStep, pointStep, edgeStep s1.ChordAngle
	ref                             s2.Point
	refWinding                      int
	haveInputStart, haveOffsetStart bool
	inputStart, offsetStart         s2.Point
	sweepA, sweepB                  s2.Point
	path                            []s2.Point
	loops                           [][]s2.Point
	polygonLayers                   int
}

func newBuffer(opt BufferOptions) *buffer {
	b := &buffer{opt: opt, ref: s2.OriginPoint()}
	switch {
	case opt.Radius > 0:
		b.sign = 1
	case opt.Radius < 0:
		b.sign = -1
	}
	abs := opt.Radius.Abs()
	requested := s1.Angle(math.Max(float64(minRequestedError), opt.ErrorFraction*float64(abs)))
	maxErr := maxAbsInterpolationError + requested
	switch {
	case abs <= maxErr:
		b.absRadius = 0
		b.sign = 0
	case abs+maxErr >= math.Pi:
		b.absRadius = s1.StraightChordAngle
	default:
		b.absRadius = s1.ChordAngleFromAngle(abs)
		step := maxEdgeSpan(abs, requested)
		b.vertexStep = s1.ChordAngleFromAngle(step)
		b.pointStep = s1.ChordAngleFromAngle(s1.Angle(2*math.Pi/math.Ceil(2*math.Pi/float64(step)) + 1e-15))
		edgeRadius := s1.Angle(math.Pi/2) - abs
		if edgeRadius > maxErr {
			b.edgeStep = s1.ChordAngleFromAngle(maxEdgeSpan(edgeRadius, requested))
		}
	}
	return b
}

func maxEdgeSpan(radius, requested s1.Angle) s1.Angle {
	step := s1.Angle(2*math.Pi/3 + 1e-15)
	minRadius := radius - requested
	if radius < math.Pi/2 {
		step = min(step, s1.Angle(2*math.Acos(math.Tan(float64(minRadius))/math.Tan(float64(radius)))))
	} else if minRadius > math.Pi/2 {
		step = min(step, s1.Angle(2*math.Acos(math.Tan(float64(radius))/math.Tan(float64(minRadius)))))
	}
	return step
}

// pointOnRay is S2::GetPointOnRay with an S1ChordAngle.
func pointOnRay(origin, dir s2.Point, r s1.ChordAngle) s2.Point {
	return s2.Point{Vector: origin.Mul(r.Cos()).Add(dir.Mul(r.Sin())).Normalize()}
}

func unit(v s2.Point) s2.Point { return s2.Point{Vector: v.Normalize()} }

func cross(a, b s2.Point) s2.Point { return s2.Point{Vector: a.Cross(b.Vector)} }

func (b *buffer) setInputVertex(a s2.Point) {
	if b.haveInputStart {
		b.updateRefWinding(b.sweepA, b.sweepB, a)
	} else {
		b.inputStart = a
		b.haveInputStart = true
	}
	b.sweepA = a
}

func (b *buffer) addOffsetVertex(p s2.Point) {
	b.path = append(b.path, p)
	if b.haveOffsetStart {
		b.updateRefWinding(b.sweepA, b.sweepB, p)
	} else {
		b.offsetStart = p
		b.haveOffsetStart = true
	}
	b.sweepB = p
}

func (b *buffer) closeBufferRegion() {
	if b.haveOffsetStart && b.haveInputStart {
		b.updateRefWinding(b.sweepA, b.sweepB, b.inputStart)
		b.updateRefWinding(b.inputStart, b.sweepB, b.offsetStart)
	}
}

func (b *buffer) outputPath() {
	if len(b.path) > 0 {
		b.loops = append(b.loops, b.path)
	}
	b.path = nil
	b.haveInputStart = false
	b.haveOffsetStart = false
}

func (b *buffer) updateRefWinding(p, q, r s2.Point) {
	sign := int(s2.RobustSign(p, q, r))
	if sign == 0 {
		return
	}
	inside := angleContainsVertex(p, q, r) == (sign > 0)
	c := s2.NewEdgeCrosser(q, b.ref)
	inside = inside != c.EdgeOrVertexCrossing(p, q)
	inside = inside != c.EdgeOrVertexCrossing(q, r)
	inside = inside != c.EdgeOrVertexCrossing(r, p)
	if inside {
		b.refWinding += sign
	}
}

// angleContainsVertex is S2::AngleContainsVertex: whether the wedge ABC
// contains the "reference direction" at B.
func angleContainsVertex(a, b, c s2.Point) bool {
	refB := s2.Ortho(b)
	return !s2.OrderedCCW(refB, c, a, b)
}

func (b *buffer) addPoint(p s2.Point) {
	if b.sign < 0 {
		return
	}
	if b.absRadius >= s1.StraightChordAngle {
		b.refWinding++
		return
	}
	if b.sign == 0 {
		b.path = append(b.path, p)
	} else {
		b.setInputVertex(p)
		start := s2.Ortho(p)
		angle := s1.ChordAngle(0)
		for q := 0; q < 4; q++ {
			rotate := unit(cross(p, start))
			for ; angle < s1.RightChordAngle; angle = angle.Add(b.pointStep) {
				dir := pointOnRay(start, rotate, angle)
				b.addOffsetVertex(pointOnRay(p, dir, b.absRadius))
			}
			angle = angle.Sub(s1.RightChordAngle)
			start = rotate
		}
		b.closeBufferRegion()
	}
	b.outputPath()
}

func (b *buffer) edgeAxis(p, q s2.Point) s2.Point {
	return s2.Point{Vector: q.PointCross(p).Normalize().Mul(float64(b.sign))}
}

func (b *buffer) addVertexArc(v, start, end s2.Point) {
	rotate := s2.Point{Vector: v.Cross(start.Vector).Normalize().Mul(float64(b.sign))}
	angle, span := s1.ChordAngle(0), s2.ChordAngleBetweenPoints(start, end)
	for {
		dir := pointOnRay(start, rotate, angle)
		b.addOffsetVertex(pointOnRay(v, dir, b.absRadius))
		angle = angle.Add(b.vertexStep)
		if !(angle < span) {
			break
		}
	}
}

func (b *buffer) closeVertexArc(v, end s2.Point) { b.addOffsetVertex(pointOnRay(v, end, b.absRadius)) }

func (b *buffer) addEdgeArc(p, q s2.Point) {
	axis := b.edgeAxis(p, q)
	if b.edgeStep == 0 {
		b.addOffsetVertex(axis)
	} else {
		rotate := s2.Point{Vector: p.Cross(axis.Vector).Normalize().Mul(float64(b.sign))}
		angle, span := s1.ChordAngle(0), s2.ChordAngleBetweenPoints(p, q)
		for {
			x := pointOnRay(p, rotate, angle)
			b.addOffsetVertex(pointOnRay(x, axis, b.absRadius))
			angle = angle.Add(b.edgeStep)
			if !(angle < span) {
				break
			}
		}
	}
	b.setInputVertex(q)
}

func (b *buffer) closeEdgeArc(p, q s2.Point) {
	if b.edgeStep != 0 {
		b.addOffsetVertex(pointOnRay(q, b.edgeAxis(p, q), b.absRadius))
	}
}

func (b *buffer) bufferEdgeAndVertex(p, q, r s2.Point) {
	b.addEdgeArc(p, q)
	if b.sign*int(s2.RobustSign(p, q, r)) >= 0 {
		start, end := b.edgeAxis(p, q), b.edgeAxis(q, r)
		b.addVertexArc(q, start, end)
		if b.edgeStep == 0 {
			b.closeVertexArc(q, end)
		}
	} else {
		b.closeEdgeArc(p, q)
		b.addOffsetVertex(q)
	}
}

func (b *buffer) addStartCap(p, q s2.Point) {
	axis := b.edgeAxis(p, q)
	neg := s2.Point{Vector: axis.Mul(-1)}
	if b.opt.EndCap == CapFlat {
		if b.opt.Side == SideBoth {
			b.addOffsetVertex(pointOnRay(p, neg, b.absRadius))
		}
		return
	}
	if b.opt.Side == SideBoth {
		b.addVertexArc(p, neg, axis)
	} else {
		b.addVertexArc(p, unit(cross(axis, p)), axis)
	}
}

func (b *buffer) addEndCap(p, q s2.Point) {
	axis := b.edgeAxis(p, q)
	if b.opt.EndCap == CapFlat {
		b.closeEdgeArc(p, q)
		return
	}
	if b.opt.Side == SideBoth {
		b.addVertexArc(q, axis, s2.Point{Vector: axis.Mul(-1)})
	} else {
		end := unit(cross(q, axis))
		b.addVertexArc(q, axis, end)
		b.closeVertexArc(q, end)
	}
}

func (b *buffer) bufferLoop(loop []s2.Point) {
	if len(loop) == 0 {
		return
	}
	if len(loop) == 1 {
		b.addPoint(loop[0])
		return
	}
	if b.absRadius >= s1.StraightChordAngle {
		if b.sign > 0 {
			b.refWinding++
		}
		return
	}
	if b.sign == 0 {
		b.path = append(b.path[:0], loop...)
	} else {
		n := len(loop)
		b.setInputVertex(loop[0])
		for i := 0; i < n; i++ {
			b.bufferEdgeAndVertex(loop[i], loop[(i+1)%n], loop[(i+2)%n])
		}
		b.closeBufferRegion()
	}
	b.outputPath()
}

func (b *buffer) addPolyline(line []s2.Point) {
	if b.opt.Side == SideLeft {
		r := make([]s2.Point, len(line))
		for i, p := range line {
			r[len(line)-1-i] = p
		}
		line = r
	}
	if b.sign < 0 {
		return
	}
	n := len(line)
	if n <= 1 {
		return
	}
	if n == 2 && line[0] == line[1] {
		b.addPoint(line[0])
		return
	}
	if b.absRadius >= s1.StraightChordAngle {
		b.refWinding++
		return
	}
	if b.sign == 0 {
		b.path = append(b.path[:0], line[:n-1]...)
		for i := n - 1; i > 0; i-- {
			b.path = append(b.path, line[i])
		}
	} else {
		b.setInputVertex(line[0])
		b.addStartCap(line[0], line[1])
		for i := 0; i < n-2; i++ {
			b.bufferEdgeAndVertex(line[i], line[i+1], line[i+2])
		}
		b.addEdgeArc(line[n-2], line[n-1])
		b.addEndCap(line[n-2], line[n-1])
		if b.opt.Side == SideBoth {
			for i := n - 3; i >= 0; i-- {
				b.bufferEdgeAndVertex(line[i+2], line[i+1], line[i])
			}
			b.addEdgeArc(line[1], line[0])
			b.closeBufferRegion()
		} else {
			for i := n - 1; i >= 0; i-- {
				b.path = append(b.path, line[i])
			}
		}
	}
	b.outputPath()
}

// ErrNegativeLayers is S2BufferOperation::Build's refusal.
var ErrNegativeLayers = errors.New("Negative buffer radius requires at most one polygon layer")

// Buffer buffers points, polylines and one polygon (its loops with the
// interior on their left; containsRef whether it contains s2.OriginPoint())
// as S2BufferOperation does, and returns the buffered region's loops (the
// region on their left), or full for the whole sphere. unchanged reports a
// radius within the permitted error, for which S2BufferOperation returns its
// input, degeneracies included.
func Buffer(opt BufferOptions, points []s2.Point, lines [][]s2.Point, polygon [][]s2.Point, containsRef bool) (loops [][]s2.Point, full, unchanged bool, err error) {
	b := newBuffer(opt)
	for _, p := range points {
		b.addPoint(p)
	}
	for _, l := range lines {
		b.addPolyline(l)
	}
	for _, l := range polygon {
		b.bufferLoop(l)
	}
	if containsRef {
		b.refWinding++
	}
	if b.sign == 0 && opt.Radius >= 0 {
		return nil, false, true, nil
	}
	if b.sign < 0 && len(polygon) == 0 {
		return nil, false, false, nil
	}
	loops, full, err = Overlay(b.loops, b.ref, b.refWinding, 1)
	return loops, full, false, err
}
