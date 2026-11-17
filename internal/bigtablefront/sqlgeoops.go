package bigtablefront

import (
	"fmt"
	"math"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/s2ops"
	"github.com/golang/geo/s2"
)

// GoogleSQL for Bigtable's point-set operations, buffers and
// simplification (#1728, #1735), on internal/s2ops's ports of S2's
// S2BooleanOperation, S2Builder, S2BufferOperation and S2WindingOperation:
// ST_UNION, ST_UNION_AGG, ST_INTERSECTION, ST_DIFFERENCE, ST_BUFFER,
// ST_BUFFERWITHTOLERANCE and ST_SIMPLIFY, as
// https://cloud.google.com/bigtable/docs/reference/sql/geography_functions
// documents them (read 2026-11-17): point-set union and intersection, and
// "the closure of the point set difference", so closed polygons and closed
// polylines (S2BooleanOperation's CLOSED models), with the result
// normalized as a closed set (s2builderutil::ClosedSetNormalizer: a point
// or line covered by a higher-dimensional part is dropped, and degenerate
// polygon pieces become lines and points). Two polygons sharing an edge
// intersect in that edge; ST_UNION splits linestrings where they cross.
//
// UNVERIFIED (docs re-read 2026-11-17: https://cloud.google.com/bigtable/docs/reference/sql/geography_functions):
// the reference documents no error message texts; the messages here name
// the argument and the bound.

// closedSetOutput collects the three layers' output as a geog.
type closedSetOutput struct {
	points []s2.Point
	lines  []*s2.Polyline
	poly   *s2.Polygon
}

// layers are the point, polyline and polygon layers GoogleSQL's results
// are built with, wrapped by NormalizeClosedSet: polylines are undirected
// paths with duplicates merged.
func (o *closedSetOutput) layers() []s2ops.Layer {
	lo := s2ops.DefaultPolylineVectorLayerOptions()
	lo.EdgeType = s2ops.EdgeTypeUndirected
	lo.DuplicateEdges = s2ops.DuplicateEdgesMerge
	return s2ops.NormalizeClosedSet([3]s2ops.Layer{
		s2ops.NewPointVectorLayer(&o.points, s2ops.PointVectorLayerOptions{DuplicateEdges: s2ops.DuplicateEdgesMerge}),
		s2ops.NewPolylineVectorLayer(&o.lines, lo),
		s2ops.NewPolygonLayer(&o.poly, s2ops.PolygonLayerOptions{}),
	})
}

func (o *closedSetOutput) geog() *geog {
	g := &geog{points: o.points}
	for _, l := range o.lines {
		g.lines = append(g.lines, []s2.Point(*l))
	}
	if o.poly != nil && (o.poly.IsFull() || o.poly.NumLoops() > 0) {
		g.poly = o.poly
	}
	return g
}

// booleanOptions are GoogleSQL's S2BooleanOperation options: closed
// polygons and polylines, crossing polylines split.
func booleanOptions() s2ops.BooleanOptions {
	o := s2ops.DefaultBooleanOptions()
	o.PolygonModel = s2ops.PolygonModelClosed
	o.PolylineModel = s2ops.PolylineModelClosed
	o.SplitAllCrossingPolylineEdges = true
	return o
}

// booleanOp runs S2BooleanOperation on a and b.
func booleanOp(op s2ops.OpType, a, b *geog) (*geog, error) {
	var out closedSetOutput
	if err := s2ops.NewBooleanOperation(op, out.layers(), booleanOptions()).Build(a.shapeIndex(), b.shapeIndex()); err != nil {
		return nil, err
	}
	return out.geog(), nil
}

// geoUnion is ST_UNION and ST_UNION_AGG: the point-set union, folded
// pairwise.
func geoUnion(gs []*geog) (*geog, error) {
	acc := &geog{}
	if len(gs) == 1 {
		return booleanOp(s2ops.OpUnion, gs[0], acc)
	}
	for _, g := range gs {
		u, err := booleanOp(s2ops.OpUnion, acc, g)
		if err != nil {
			return nil, err
		}
		acc = u
	}
	return acc, nil
}

// geoIntersection is ST_INTERSECTION: the point-set intersection.
func geoIntersection(a, b *geog) (*geog, error) {
	return booleanOp(s2ops.OpIntersection, a, b)
}

// geoDifference is ST_DIFFERENCE: the closure of a's points not in b.
func geoDifference(a, b *geog) (*geog, error) {
	return booleanOp(s2ops.OpDifference, a, b)
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

// geoBuffer is ST_BUFFER and ST_BUFFERWITHTOLERANCE, on S2BufferOperation
// (whose default snap function, IdentitySnapFunction(0), keeps every offset
// vertex where S2BufferOperation computes it).
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
	var loops [][]s2.Point
	contains := false
	if g.hasPoly() {
		loops = g.orientedLoops()
		contains = g.poly.ContainsPoint(s2.OriginPoint())
	}
	p, unchanged, err := s2ops.Buffer(o, g.points, g.lines, loops, contains)
	if err != nil {
		return nil, err
	}
	if unchanged {
		return g, nil
	}
	if p == nil || !p.IsFull() && p.NumLoops() == 0 {
		return &geog{}, nil
	}
	return &geog{poly: p}, nil
}

// geoSimplify is ST_SIMPLIFY: S2Builder with IdentitySnapFunction(tolerance)
// and simplify_edge_chains over every part of the input at once (so that
// no new crossings appear between parts), normalized as a closed set.
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
	bo := s2ops.DefaultBuilderOptions()
	bo.SnapFunction = s2.NewIdentitySnapper(r)
	bo.SimplifyEdgeChains = true
	b := s2ops.NewBuilder(bo)
	var out closedSetOutput
	ls := out.layers()
	b.StartLayer(ls[0])
	for _, p := range g.points {
		b.AddPoint(p)
	}
	b.StartLayer(ls[1])
	for _, l := range g.lines {
		b.AddPolyline(l)
	}
	b.StartLayer(ls[2])
	if g.hasPoly() {
		b.AddShape(g.poly)
	}
	b.AddIsFullPolygonPredicate(s2ops.IsFullPolygon(false))
	if err := b.Build(); err != nil {
		return nil, err
	}
	return out.geog(), nil
}

// radiusOpt is a buffer radius in meters as an angle.
func radiusOpt(fn string, m float64) (s2ops.BufferOptions, error) {
	if math.IsNaN(m) || math.IsInf(m, 0) {
		return s2ops.BufferOptions{}, fmt.Errorf("%s: buffer_radius must be finite, not %v", fn, m)
	}
	return s2ops.BufferOptions{Radius: metersAngle(m)}, nil
}

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
