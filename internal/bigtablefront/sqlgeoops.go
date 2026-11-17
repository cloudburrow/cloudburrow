package bigtablefront

import (
	"fmt"
	"math"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/s2ops"
	"github.com/golang/geo/s2"
)

// GoogleSQL for Bigtable's point-set operations, buffers and
// simplification (#1728), on internal/s2ops: ST_UNION, ST_UNION_AGG,
// ST_INTERSECTION, ST_DIFFERENCE, ST_BUFFER, ST_BUFFERWITHTOLERANCE and
// ST_SIMPLIFY, as https://cloud.google.com/bigtable/docs/reference/sql/geography_functions
// documents them (read 2026-11-17).
//
// UNVERIFIED (docs re-read 2026-11-17: https://cloud.google.com/bigtable/docs/reference/sql/geography_functions):
// the reference prints results for its examples only. Beyond them, which
// lower-dimensional pieces survive where boundaries merely touch (two
// polygons sharing only an edge intersect in a line in GoogleSQL; here in
// nothing), whether crossing linestrings are split at their crossings by
// ST_UNION, the order of MULTIPOINT members, buffer vertices' last digits
// (GoogleSQL's snap function is not documented) and ST_SIMPLIFY's
// topology checks against neighbouring geometry are not measured against
// Google.

// orientedLoops are g's polygon rings with the interior on their left
// (WKT's order: holes turn the other way).
func (g *geog) orientedLoops() [][]s2.Point {
	var out [][]s2.Point
	for _, p := range g.polygons() {
		for _, r := range p {
			if len(r) > 0 {
				out = append(out, r)
			}
		}
	}
	return out
}

func (g *geog) containsOrigin() bool { return g.hasPoly() && g.poly.ContainsPoint(s2.OriginPoint()) }

// polyFromLoops is a polygon of oriented loops, or the full polygon.
func polyFromLoops(loops [][]s2.Point, full bool) (*s2.Polygon, error) {
	if full {
		return s2.FullPolygon(), nil
	}
	var ls []*s2.Loop
	for _, l := range loops {
		if len(l) >= 3 {
			ls = append(ls, s2.LoopFromPoints(l))
		}
	}
	if len(ls) == 0 {
		return nil, nil
	}
	p := s2.PolygonFromOrientedLoops(ls)
	if err := p.Validate(); err != nil {
		return nil, fmt.Errorf("the result is not a valid polygon: %v", err)
	}
	return p, nil
}

// lineEdges are the edges of lines, numbered from id.
func lineEdges(lines [][]s2.Point, id *int) []s2ops.Edge {
	var es []s2ops.Edge
	for _, l := range lines {
		for i := 0; i+1 < len(l); i++ {
			es = append(es, s2ops.Edge{A: l[i], B: l[i+1], ID: *id})
			*id++
		}
		*id++
	}
	return es
}

func polyEdges(g *geog) []s2ops.Edge {
	var es []s2ops.Edge
	for _, e := range edges(&geog{poly: g.poly}) {
		es = append(es, s2ops.Edge{A: e[0], B: e[1]})
	}
	return es
}

func mid(e s2ops.Edge) s2.Point { return s2.Interpolate(0.5, e.A, e.B) }

// assemble builds a result: its polygon, the line pieces kept, and the
// points, each lower-dimensional part dropped where a higher one covers it.
func assemble(loops [][]s2.Point, full bool, pieces []s2ops.Edge, points []s2.Point) (*geog, error) {
	p, err := polyFromLoops(loops, full)
	if err != nil {
		return nil, err
	}
	out := &geog{poly: p}
	var keep []s2ops.Edge
	for _, e := range pieces {
		if e.A != e.B && !out.polyCovers(mid(e)) {
			keep = append(keep, e)
		}
	}
	out.lines = s2ops.Polylines(keep)
	var pts []s2.Point
	for _, q := range points {
		if !out.pointIn(q) {
			pts = appendDistinct(pts, q)
		}
	}
	s2ops.SortVertices(pts)
	out.points = pts
	return out, nil
}

// geoUnion is ST_UNION and ST_UNION_AGG: the point-set union.
func geoUnion(gs []*geog) (*geog, error) {
	var loops [][]s2.Point
	refW := 0
	var lines []s2ops.Edge
	var cut []s2ops.Edge
	var points []s2.Point
	id := 0
	for _, g := range gs {
		if g.hasPoly() && g.poly.IsFull() {
			return &geog{poly: s2.FullPolygon()}, nil
		}
		points = append(points, g.points...)
		id += len(g.points)
		lines = append(lines, lineEdges(g.lines, &id)...)
		loops = append(loops, g.orientedLoops()...)
		cut = append(cut, polyEdges(g)...)
		if g.containsOrigin() {
			refW++
		}
	}
	res, full, err := s2ops.Overlay(loops, s2.OriginPoint(), refW, 1)
	if err != nil {
		return nil, err
	}
	return assemble(res, full, s2ops.Split(lines, cut), points)
}

// geoIntersection is ST_INTERSECTION: the point-set intersection.
func geoIntersection(a, b *geog) (*geog, error) {
	if a.isEmpty() || b.isEmpty() {
		return &geog{}, nil
	}
	if a.hasPoly() && a.poly.IsFull() {
		return b, nil
	}
	if b.hasPoly() && b.poly.IsFull() {
		return a, nil
	}
	var loops [][]s2.Point
	full := false
	if a.hasPoly() && b.hasPoly() {
		refW := 0
		for _, g := range []*geog{a, b} {
			if g.containsOrigin() {
				refW++
			}
		}
		var err error
		loops, full, err = s2ops.Overlay(append(a.orientedLoops(), b.orientedLoops()...), s2.OriginPoint(), refW, 2)
		if err != nil {
			return nil, err
		}
	}
	id := len(a.points)
	la := lineEdges(a.lines, &id)
	id += len(b.points)
	lb := lineEdges(b.lines, &id)
	var pieces []s2ops.Edge
	for _, e := range s2ops.Split(la, append(polyEdges(b), lb...)) {
		if b.pointIn(mid(e)) {
			pieces = append(pieces, e)
		}
	}
	for _, e := range s2ops.Split(lb, polyEdges(a)) {
		if a.polyCovers(mid(e)) {
			pieces = append(pieces, e)
		}
	}
	var points []s2.Point
	for _, p := range a.points {
		if b.pointIn(p) {
			points = append(points, p)
		}
	}
	for _, p := range b.points {
		if a.pointIn(p) {
			points = append(points, p)
		}
	}
	for _, x := range la {
		for _, y := range lb {
			if s2.CrossingSign(x.A, x.B, y.A, y.B) == s2.Cross {
				points = append(points, s2.Intersection(x.A, x.B, y.A, y.B))
			}
		}
	}
	return assemble(loops, full, pieces, points)
}

// geoDifference is ST_DIFFERENCE: the closure of a's points not in b.
func geoDifference(a, b *geog) (*geog, error) {
	if a.isEmpty() {
		return &geog{}, nil
	}
	if b.isEmpty() {
		return a, nil
	}
	if b.hasPoly() && b.poly.IsFull() {
		return &geog{}, nil
	}
	var loops [][]s2.Point
	full := false
	if a.hasPoly() {
		ls := a.orientedLoops()
		for _, l := range b.orientedLoops() {
			r := make([]s2.Point, len(l))
			for i, p := range l {
				r[len(l)-1-i] = p
			}
			ls = append(ls, r)
		}
		refW := 0
		if a.containsOrigin() || a.poly.IsFull() {
			refW++
		}
		if b.containsOrigin() {
			refW--
		}
		if a.poly.IsFull() {
			ls = ls[0:0]
			for _, l := range b.orientedLoops() {
				r := make([]s2.Point, len(l))
				for i, p := range l {
					r[len(l)-1-i] = p
				}
				ls = append(ls, r)
			}
		}
		var err error
		loops, full, err = s2ops.Overlay(ls, s2.OriginPoint(), refW, 1)
		if err != nil {
			return nil, err
		}
	}
	id := len(a.points)
	la := lineEdges(a.lines, &id)
	id += len(b.points)
	lb := lineEdges(b.lines, &id)
	var pieces []s2ops.Edge
	for _, e := range s2ops.Split(la, append(polyEdges(b), lb...)) {
		if !b.pointIn(mid(e)) {
			pieces = append(pieces, e)
		}
	}
	var points []s2.Point
	for _, p := range a.points {
		if !b.pointIn(p) {
			points = append(points, p)
		}
	}
	return assemble(loops, full, pieces, points)
}

// bufferOpts reads ST_BUFFER's and ST_BUFFERWITHTOLERANCE's endcap and
// side.
func bufferOpts(fn string, vs []any, at int, o *s2ops.BufferOptions) error {
	if s, ok := opt(vs, at).(string); ok {
		switch strings.ToUpper(s) {
		case "ROUND":
			o.EndCap = s2ops.CapRound
		case "FLAT":
			o.EndCap = s2ops.CapFlat
		default:
			return fmt.Errorf("%s: endcap must be ROUND or FLAT, not %q", fn, s)
		}
	}
	if s, ok := opt(vs, at+1).(string); ok {
		switch strings.ToUpper(s) {
		case "BOTH":
			o.Side = s2ops.SideBoth
		case "LEFT":
			o.Side = s2ops.SideLeft
		case "RIGHT":
			o.Side = s2ops.SideRight
		default:
			return fmt.Errorf("%s: side must be BOTH, LEFT or RIGHT, not %q", fn, s)
		}
	}
	return nil
}

// geoBuffer is ST_BUFFER and ST_BUFFERWITHTOLERANCE, on S2BufferOperation.
func geoBuffer(g *geog, o s2ops.BufferOptions) (*geog, error) {
	if g.isEmpty() {
		return &geog{}, nil
	}
	if g.hasPoly() && g.poly.IsFull() {
		if o.Radius < 0 {
			return &geog{poly: s2.FullPolygon()}, nil
		}
		return g, nil
	}
	loops, full, unchanged, err := s2ops.Buffer(o, g.points, g.lines, g.orientedLoops(), g.containsOrigin())
	if err != nil {
		return nil, err
	}
	if unchanged {
		return g, nil
	}
	p, err := polyFromLoops(loops, full)
	if err != nil {
		return nil, err
	}
	return &geog{poly: p}, nil
}

// geoSimplify is ST_SIMPLIFY.
func geoSimplify(g *geog, tolerance float64) (*geog, error) {
	if math.IsNaN(tolerance) || tolerance < 0 {
		return nil, fmt.Errorf("ST_SIMPLIFY: tolerance_meters must be non-negative, not %v", tolerance)
	}
	r := metersAngle(tolerance)
	if r > s2ops.MaxSnapRadius {
		return nil, fmt.Errorf("ST_SIMPLIFY: tolerance_meters must be at most %v, not %v", meters(s2ops.MaxSnapRadius), tolerance)
	}
	if tolerance == 0 || g.isEmpty() || g.hasPoly() && g.poly.IsFull() {
		return g, nil
	}
	s := s2ops.Simplify(r, g.points, g.lines, g.orientedLoops())
	p, err := polyFromLoops(s.Loops, false)
	if err != nil {
		return nil, err
	}
	return &geog{points: s.Points, lines: s.Lines, poly: p}, nil
}

// radiusOpt is a buffer radius in meters as an angle.
func radiusOpt(fn string, m float64) (s2ops.BufferOptions, error) {
	if math.IsNaN(m) || math.IsInf(m, 0) {
		return s2ops.BufferOptions{}, fmt.Errorf("%s: buffer_radius must be finite, not %v", fn, m)
	}
	return s2ops.BufferOptions{Radius: metersAngle(m)}, nil
}
