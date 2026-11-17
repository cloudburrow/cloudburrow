package bigtablefront

import (
	"fmt"
	"math"
	"strconv"

	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
	"github.com/golang/geo/s1"
	"github.com/golang/geo/s2"
)

// GoogleSQL for Bigtable's GEOGRAPHY values (#1720), on Google's S2 library
// (github.com/golang/geo): "Geography functions operate on or generate
// GoogleSQL GEOGRAPHY values" (docs re-read 2026-11-17:
// https://cloud.google.com/bigtable/docs/reference/sql/geography_functions).
//
// Bigtable's Type message has no kind for a GEOGRAPHY, and the reference's
// data types do not list one, so a query computes with GEOGRAPHY values but
// cannot return one: a result column of a GEOGRAPHY type is refused, and
// ST_ASTEXT, ST_ASGEOJSON or ST_ASBINARY return its text or bytes.

// geographyType marks a GEOGRAPHY inside the engine. It reuses the Proto
// kind with a schema bundle no table can name, so kind() can tell it apart.
var geographyType = &btpb.Type{Kind: &btpb.Type_ProtoType{ProtoType: &btpb.Type_Proto{SchemaBundleId: geoBundle, MessageName: "GEOGRAPHY"}}}

const geoBundle = "\x00cloudburrow.geography"

func isGeography(t *btpb.Type) bool {
	p := t.GetProtoType()
	return p != nil && p.GetSchemaBundleId() == geoBundle
}

// hasGeography is whether t is or holds a GEOGRAPHY.
func hasGeography(t *btpb.Type) bool {
	switch {
	case isGeography(t):
		return true
	case t.GetArrayType() != nil:
		return hasGeography(t.GetArrayType().GetElementType())
	case t.GetStructType() != nil:
		for _, f := range t.GetStructType().GetFields() {
			if hasGeography(f.GetType()) {
				return true
			}
		}
	case t.GetMapType() != nil:
		return hasGeography(t.GetMapType().GetValueType())
	}
	return false
}

// earthRadiusMeters is the sphere the measures use. The reference says
// only that they measure "on the surface of a perfect sphere"; this radius
// is the one that reproduces GoogleSQL's documented ST_HAUSDORFFDISTANCE
// results (sqlgeo_test.go), S2's mean Earth radius (S2Earth::RadiusMeters).
const earthRadiusMeters = 6371010.0

// geog is a GEOGRAPHY: a set of points, linestrings and polygons on the
// sphere, with geodesic edges. Its polygons are one S2 polygon, whose
// loops have the interior on their left (holes are normalized, as S2
// keeps them; WKT writes them reversed).
type geog struct {
	points []s2.Point
	lines  [][]s2.Point
	poly   *s2.Polygon // nil when it has no polygon
}

func (g *geog) hasPoly() bool { return g.poly != nil && (g.poly.IsFull() || g.poly.NumLoops() > 0) }

func (g *geog) isEmpty() bool { return len(g.points) == 0 && len(g.lines) == 0 && !g.hasPoly() }

// dimension is the dimension of its highest-dimensional part, -1 if empty.
func (g *geog) dimension() int {
	switch {
	case g.hasPoly():
		return 2
	case len(g.lines) > 0:
		return 1
	case len(g.points) > 0:
		return 0
	}
	return -1
}

// polygons are its polygons' rings as WKT writes them: each polygon's
// shell first (interior on the left), then its holes (reversed, so that
// they turn the other way). The full sphere is one polygon of no rings.
func (g *geog) polygons() [][][]s2.Point {
	if !g.hasPoly() {
		return nil
	}
	if g.poly.IsFull() {
		return [][][]s2.Point{{}}
	}
	var out [][][]s2.Point
	index := map[int]int{} // a shell loop's polygon in out
	for k, l := range g.poly.Loops() {
		vs := append([]s2.Point(nil), l.Vertices()...)
		if l.IsHole() {
			// golang/geo's Polygon.Parent walks past the shell at this pin.
			parent := k - 1
			for parent >= 0 && g.poly.Loop(parent).IsHole() {
				parent--
			}
			for i, j := 0, len(vs)-1; i < j; i, j = i+1, j-1 {
				vs[i], vs[j] = vs[j], vs[i]
			}
			if pi, ok := index[parent]; ok {
				out[pi] = append(out[pi], vs)
				continue
			}
		}
		index[k] = len(out)
		out = append(out, [][]s2.Point{vs})
	}
	return out
}

// numGeometries counts its points, linestrings and polygons.
func (g *geog) numGeometries() int {
	return len(g.points) + len(g.lines) + len(g.polygons())
}

// llPoint is the point at a longitude and latitude in degrees.
func llPoint(lng, lat float64) s2.Point {
	return s2.PointFromLatLng(s2.LatLngFromDegrees(lat, lng))
}

// lngLat are a point's longitude and latitude in degrees.
func lngLat(p s2.Point) (float64, float64) {
	ll := s2.LatLngFromPoint(p)
	return ll.Lng.Degrees(), ll.Lat.Degrees()
}

// coord writes a coordinate as GoogleSQL's WKT does, to 15 significant
// digits (the reference's results: POINT(0.999999999999943 1)).
func coord(v float64) string {
	if v == 0 {
		return "0"
	}
	return strconv.FormatFloat(v, 'g', 15, 64)
}

// pointGeog is a GEOGRAPHY of one point.
func pointGeog(p s2.Point) *geog { return &geog{points: []s2.Point{p}} }

// dedup drops consecutive equal vertices.
func dedup(vs []s2.Point) []s2.Point {
	var out []s2.Point
	for _, v := range vs {
		if len(out) > 0 && out[len(out)-1] == v {
			continue
		}
		out = append(out, v)
	}
	return out
}

// lineOrPoint adds a linestring's vertices to g: a line of one distinct
// vertex is a point, as the reference's ST_MAKELINE example shows.
func (g *geog) addLine(vs []s2.Point) error {
	vs = dedup(vs)
	switch len(vs) {
	case 0:
		return nil
	case 1:
		g.points = append(g.points, vs[0])
		return nil
	}
	for i := 1; i < len(vs); i++ {
		if vs[i].Vector == vs[i-1].Mul(-1) {
			return fmt.Errorf("Edge %d has antipodal vertices", i-1)
		}
	}
	g.lines = append(g.lines, vs)
	return nil
}

// ring is a polygon ring's loop: the closing vertex dropped, at least
// three distinct vertices. Unless oriented, it is normalized so that it
// encloses at most half the sphere ("the polygon with the smaller area").
func ring(vs []s2.Point, oriented bool) (*s2.Loop, error) {
	vs = dedup(vs)
	if len(vs) > 1 && vs[0] == vs[len(vs)-1] {
		vs = vs[:len(vs)-1]
	}
	if len(vs) == 0 {
		return s2.FullLoop(), nil
	}
	if len(vs) < 3 {
		return nil, fmt.Errorf("Polygon loop should have at least three distinct vertices")
	}
	l := s2.LoopFromPoints(vs)
	if err := l.Validate(); err != nil {
		return nil, fmt.Errorf("Invalid polygon loop: %v", err)
	}
	if !oriented {
		l.Normalize()
	}
	return l, nil
}

// addPolygons adds polygons, each its rings (shell first, then holes), to
// g. Unless oriented, each ring is taken as the smaller region it bounds
// and the holes are cut from the shell; oriented, every ring has the
// polygon's interior on its left.
func (g *geog) addPolygons(polys [][][]s2.Point, oriented bool) error {
	var loops []*s2.Loop
	if g.poly != nil {
		for _, l := range g.poly.Loops() {
			loops = append(loops, l)
		}
	}
	full := false
	for _, rings := range polys {
		for i, r := range rings {
			l, err := ring(r, oriented)
			if err != nil {
				return err
			}
			if l.IsFull() {
				if i == 0 {
					full = true
				}
				continue
			}
			if oriented && i > 0 {
				// holes turn the other way: keep them as S2 does, normalized.
				l.Invert()
			}
			loops = append(loops, l)
		}
	}
	if full && len(loops) == 0 {
		g.poly = s2.FullPolygon()
		return nil
	}
	if full {
		// a full shell with holes: the holes' complement.
		for _, l := range loops {
			l.Invert()
		}
	}
	if len(loops) == 0 {
		return nil
	}
	p := s2.PolygonFromLoops(loops)
	if err := p.Validate(); err != nil {
		return fmt.Errorf("Invalid polygon: %v", err)
	}
	g.poly = p
	return nil
}

// meters converts an angle on the sphere to meters.
func meters(a s1.Angle) float64 { return a.Radians() * earthRadiusMeters }

// metersAngle converts meters on the sphere to an angle.
func metersAngle(m float64) s1.Angle { return s1.Angle(m / earthRadiusMeters) }

// shapeIndex indexes its points, lines and polygon.
func (g *geog) shapeIndex() *s2.ShapeIndex {
	idx := s2.NewShapeIndex()
	if len(g.points) > 0 {
		pv := s2.PointVector(append([]s2.Point(nil), g.points...))
		idx.Add(&pv)
	}
	for _, l := range g.lines {
		pl := s2.Polyline(append([]s2.Point(nil), l...))
		idx.Add(&pl)
	}
	if g.hasPoly() {
		idx.Add(g.poly)
	}
	return idx
}

// vertices are its points and every line and polygon vertex.
func (g *geog) vertices() []s2.Point {
	vs := append([]s2.Point(nil), g.points...)
	for _, l := range g.lines {
		vs = append(vs, l...)
	}
	for _, p := range g.polygons() {
		for _, r := range p {
			vs = append(vs, r...)
			if len(r) > 0 {
				vs = append(vs, r[0])
			}
		}
	}
	return vs
}

func asGeog(v any) *geog {
	g, _ := v.(*geog)
	return g
}

// singlePoint is the point of a GEOGRAPHY of exactly one point.
func singlePoint(fn string, g *geog) (s2.Point, error) {
	if len(g.points) != 1 || len(g.lines) > 0 || g.hasPoly() {
		return s2.Point{}, fmt.Errorf("%s: the argument must be a single point geography", fn)
	}
	return g.points[0], nil
}

// singleLine is the linestring of a GEOGRAPHY of exactly one linestring.
func singleLine(fn string, g *geog) ([]s2.Point, error) {
	if len(g.lines) != 1 || len(g.points) > 0 || g.hasPoly() {
		return nil, fmt.Errorf("%s: the argument must be a linestring geography", fn)
	}
	return g.lines[0], nil
}

func round15(v float64) float64 {
	if v == 0 || math.IsNaN(v) || math.IsInf(v, 0) {
		return v
	}
	f, _ := strconv.ParseFloat(strconv.FormatFloat(v, 'g', 15, 64), 64)
	return f
}
