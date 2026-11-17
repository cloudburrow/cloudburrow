package bigtablefront

import (
	"encoding/hex"
	"fmt"
	"math"
	"sort"
	"strings"

	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
	"github.com/golang/geo/r3"
	"github.com/golang/geo/s1"
	"github.com/golang/geo/s2"
)

// GoogleSQL for Bigtable's geography functions (#1720), as its reference
// documents them (re-read 2026-11-17:
// https://cloud.google.com/bigtable/docs/reference/sql/geography_functions):
// constructors, parsers, formatters, accessors, predicates, measures,
// transformations, the aggregates ST_ACCUM, ST_CENTROID_AGG and ST_EXTENT,
// and S2_CELLIDFROMPOINT and S2_COVERINGCELLIDS. "All GoogleSQL geography
// functions return NULL if any input argument is NULL."
//
// Refused UNIMPLEMENTED, naming the reason: the point-set operations
// (ST_UNION, ST_UNION_AGG, ST_INTERSECTION, ST_DIFFERENCE), ST_BUFFER,
// ST_BUFFERWITHTOLERANCE and ST_SIMPLIFY, which need S2's
// S2BooleanOperation, S2BufferOperation and S2Builder simplification, which
// github.com/golang/geo does not port; and ST_REGIONSTATS, which reads
// Earth Engine rasters.

// geoRefused are the listed functions the emulator does not run, and why.
var geoRefused = map[string]string{
	"ST_UNION":               "a point-set union (S2BooleanOperation, which github.com/golang/geo does not port)",
	"ST_UNION_AGG":           "a point-set union (S2BooleanOperation, which github.com/golang/geo does not port)",
	"ST_INTERSECTION":        "a point-set intersection (S2BooleanOperation, which github.com/golang/geo does not port)",
	"ST_DIFFERENCE":          "a point-set difference (S2BooleanOperation, which github.com/golang/geo does not port)",
	"ST_BUFFER":              "a buffer (S2BufferOperation, which github.com/golang/geo does not port)",
	"ST_BUFFERWITHTOLERANCE": "a buffer (S2BufferOperation, which github.com/golang/geo does not port)",
	"ST_SIMPLIFY":            "a topology-preserving simplification (S2Builder's, which github.com/golang/geo does not port)",
	"ST_REGIONSTATS":         "Earth Engine raster statistics",
}

// geoAggregates are the geography aggregates the emulator runs.
var geoAggregates = map[string]bool{"ST_ACCUM": true, "ST_CENTROID_AGG": true, "ST_EXTENT": true}

// absent is an optional argument a call leaves out (named arguments).
type absent struct{}

type absentArg struct{}

func (absentArg) check(*scope) (*btpb.Type, error) { return nil, nil }
func (absentArg) eval(*env) (any, error)           { return absent{}, nil }
func (absentArg) String() string                   { return "absent" }

// placeNamed puts named arguments in their places, the left-out optional
// ones absent.
func (c *call) placeNamed() error {
	if len(c.names) == 0 {
		return nil
	}
	if c.fn.params == nil {
		return fmt.Errorf("Named arguments are not supported by function %s", c.name)
	}
	args := make([]expr, 0, len(c.fn.params))
	for i, n := range c.names {
		if n == "" {
			args = append(args, c.args[i])
		}
	}
	for i, n := range c.names {
		if n == "" {
			continue
		}
		at := -1
		for j, p := range c.fn.params {
			if p == n {
				at = j
			}
		}
		if at < 0 {
			return fmt.Errorf("Named argument %s does not exist in the signature of function %s", n, c.name)
		}
		if at < len(args) {
			if _, ok := args[at].(absentArg); !ok {
				return fmt.Errorf("Named argument %s of function %s is also given by position", n, c.name)
			}
		}
		for len(args) <= at {
			args = append(args, absentArg{})
		}
		args[at] = c.args[i]
	}
	c.args, c.names = args, nil
	return nil
}

// opt is the value of optional argument i, or nil when left out.
func opt(vs []any, i int) any {
	if i >= len(vs) {
		return nil
	}
	if _, ok := vs[i].(absent); ok {
		return nil
	}
	return vs[i]
}

func optBool(vs []any, i int) bool {
	b, _ := opt(vs, i).(bool)
	return b
}

func geoErr(fn string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %v", fn, err)
}

// geoSig types a geography function: kinds per argument ("GEO" a
// GEOGRAPHY, "ARRAY<GEO>" an array of them), min required.
func geoSig(ret *btpb.Type, min int, kinds ...string) func(*call, []*btpb.Type) (*btpb.Type, error) {
	return func(c *call, ts []*btpb.Type) (*btpb.Type, error) {
		if len(ts) < min || len(ts) > len(kinds) {
			return nil, sigErr(c, ts)
		}
		for i, t := range ts {
			if t == nil {
				continue
			}
			ok := false
			for _, k := range strings.Split(kinds[i], "|") {
				switch k {
				case "GEO":
					ok = ok || isGeography(t)
				case "ARRAY<GEO>":
					ok = ok || isKind(t, "ARRAY<") && isGeography(t.GetArrayType().GetElementType())
				default:
					ok = ok || argOK(t, k)
				}
			}
			if !ok {
				return nil, sigErr(c, ts)
			}
		}
		return ret, nil
	}
}

// noSpheroid refuses use_spheroid => TRUE, which "currently only supports
// the value FALSE".
func noSpheroid(fn string, vs []any, i int) error {
	if optBool(vs, i) {
		return fmt.Errorf("%s: use_spheroid only supports the value FALSE", fn)
	}
	return nil
}

func init() {
	geo := func(params []string, sig func(*call, []*btpb.Type) (*btpb.Type, error), run func(c *call, vs []any) (any, error)) *fnDef {
		return &fnDef{params: params, sig: sig, run: func(c *call, vs []any, _ *env) (any, error) { return run(c, vs) }}
	}
	g1 := func(ret *btpb.Type, f func(g *geog) (any, error)) *fnDef {
		return geo(nil, geoSig(ret, 1, "GEO"), func(c *call, vs []any) (any, error) { return f(asGeog(vs[0])) })
	}
	pred := func(f func(a, b *geog) bool) *fnDef {
		return geo(nil, geoSig(boolType, 2, "GEO", "GEO"), func(c *call, vs []any) (any, error) {
			return f(asGeog(vs[0]), asGeog(vs[1])), nil
		})
	}
	parse := func(params []string, kinds []string, f func(c *call, vs []any) (*geog, error)) *fnDef {
		return geo(params, geoSig(geographyType, 1, kinds...), func(c *call, vs []any) (any, error) {
			g, err := f(c, vs)
			if err != nil {
				return nil, geoErr(c.name, err)
			}
			return g, nil
		})
	}
	wkOpts := func(c *call, vs []any) (oriented, planar bool, err error) {
		oriented, planar = optBool(vs, 1), optBool(vs, 2)
		if oriented && planar {
			return false, false, fmt.Errorf("oriented and planar can't be TRUE at the same time")
		}
		if oriented && optBool(vs, 3) {
			return false, false, fmt.Errorf("oriented and make_valid can't be TRUE at the same time")
		}
		return oriented, planar, nil
	}
	fs := map[string]*fnDef{
		// Constructors.
		"ST_GEOGPOINT": geo(nil, geoSig(geographyType, 2, "FLOAT64", "FLOAT64"), func(c *call, vs []any) (any, error) {
			lng, lat := toFloat(vs[0]), toFloat(vs[1])
			if lat < -90 || lat > 90 || math.IsNaN(lat) || math.IsNaN(lng) || math.IsInf(lng, 0) {
				return nil, fmt.Errorf("ST_GEOGPOINT: latitude must be in the range [-90, 90], not %v", lat)
			}
			return pointGeog(llPoint(lng, lat)), nil
		}),
		"ST_MAKELINE": geo(nil, func(c *call, ts []*btpb.Type) (*btpb.Type, error) {
			if len(ts) == 1 {
				return geoSig(geographyType, 1, "ARRAY<GEO>")(c, ts)
			}
			return geoSig(geographyType, 2, "GEO", "GEO")(c, ts)
		}, func(c *call, vs []any) (any, error) {
			parts, ok := geoList(vs)
			if !ok {
				return nil, nil
			}
			var verts []s2.Point
			for _, g := range parts {
				switch {
				case len(g.points) == 1 && len(g.lines) == 0 && !g.hasPoly():
					verts = append(verts, g.points[0])
				case len(g.lines) == 1 && len(g.points) == 0 && !g.hasPoly():
					verts = append(verts, g.lines[0]...)
				default:
					return nil, fmt.Errorf("ST_MAKELINE: each input must be exactly one point or one linestring")
				}
			}
			out := &geog{}
			return out, geoErr(c.name, out.addLine(verts))
		}),
		"ST_MAKEPOLYGON": geo(nil, geoSig(geographyType, 1, "GEO", "ARRAY<GEO>"), func(c *call, vs []any) (any, error) {
			rings := [][]s2.Point{}
			add := func(g *geog) error {
				switch {
				case g.isEmpty():
					rings = append(rings, nil)
				case len(g.lines) == 1 && len(g.points) == 0 && !g.hasPoly():
					rings = append(rings, g.lines[0])
				default:
					return fmt.Errorf("ST_MAKEPOLYGON: each ring must be a linestring")
				}
				return nil
			}
			if err := add(asGeog(vs[0])); err != nil {
				return nil, err
			}
			if len(vs) > 1 {
				for _, h := range vs[1].([]any) {
					if h == nil {
						return nil, nil
					}
					if err := add(asGeog(h)); err != nil {
						return nil, err
					}
				}
			}
			out := &geog{}
			return out, geoErr(c.name, out.addPolygons([][][]s2.Point{rings}, false))
		}),
		"ST_MAKEPOLYGONORIENTED": geo(nil, geoSig(geographyType, 1, "ARRAY<GEO>"), func(c *call, vs []any) (any, error) {
			rings := [][]s2.Point{}
			for _, h := range vs[0].([]any) {
				if h == nil {
					return nil, nil
				}
				g := asGeog(h)
				switch {
				case g.isEmpty():
					rings = append(rings, nil)
				case len(g.lines) == 1 && len(g.points) == 0 && !g.hasPoly():
					rings = append(rings, g.lines[0])
				default:
					return nil, fmt.Errorf("ST_MAKEPOLYGONORIENTED: each ring must be a linestring")
				}
			}
			if len(rings) == 0 {
				return nil, fmt.Errorf("ST_MAKEPOLYGONORIENTED: the array has no rings")
			}
			out := &geog{}
			return out, geoErr(c.name, out.addPolygons([][][]s2.Point{rings}, true))
		}),
		// Parsers.
		"ST_GEOGFROMTEXT": parse([]string{"wkt_string", "oriented", "planar", "make_valid"}, []string{"STRING", "BOOL", "BOOL", "BOOL"}, func(c *call, vs []any) (*geog, error) {
			oriented, planar, err := wkOpts(c, vs)
			if err != nil {
				return nil, err
			}
			return parseWKT(vs[0].(string), oriented, planar)
		}),
		"ST_GEOGFROMWKB": parse([]string{"wkb", "oriented", "planar", "make_valid"}, []string{"BYTES|STRING", "BOOL", "BOOL", "BOOL"}, func(c *call, vs []any) (*geog, error) {
			oriented, planar, err := wkOpts(c, vs)
			if err != nil {
				return nil, err
			}
			b, err := wkbBytes(vs[0])
			if err != nil {
				return nil, err
			}
			return parseWKB(b, oriented, planar)
		}),
		"ST_GEOGFROMGEOJSON": parse([]string{"geojson_string", "make_valid"}, []string{"STRING", "BOOL"}, func(c *call, vs []any) (*geog, error) {
			return parseGeoJSON(vs[0].(string))
		}),
		"ST_GEOGFROMKML": parse(nil, []string{"STRING"}, func(c *call, vs []any) (*geog, error) {
			return parseKML(vs[0].(string))
		}),
		"ST_GEOGFROM": parse(nil, []string{"STRING|BYTES"}, func(c *call, vs []any) (*geog, error) {
			if b, ok := vs[0].([]byte); ok {
				return parseWKB(b, false, false)
			}
			s := strings.TrimSpace(vs[0].(string))
			switch {
			case strings.HasPrefix(s, "{"):
				return parseGeoJSON(s)
			case isHex(s):
				b, _ := hex.DecodeString(s)
				return parseWKB(b, false, false)
			}
			return parseWKT(s, false, false)
		}),
		"ST_GEOGPOINTFROMGEOHASH": parse(nil, []string{"STRING"}, func(c *call, vs []any) (*geog, error) {
			p, err := geohashDecode(vs[0].(string))
			if err != nil {
				return nil, err
			}
			return pointGeog(p), nil
		}),
		// Formatters.
		"ST_ASTEXT": g1(stringType, func(g *geog) (any, error) { s, err := g.wkt(); return s, geoErr("ST_ASTEXT", err) }),
		"ST_ASGEOJSON": g1(stringType, func(g *geog) (any, error) {
			s, err := g.geoJSON()
			return s, geoErr("ST_ASGEOJSON", err)
		}),
		"ST_ASBINARY": g1(bytesType, func(g *geog) (any, error) { b, err := g.wkb(); return b, geoErr("ST_ASBINARY", err) }),
		"ST_ASKML":    g1(stringType, func(g *geog) (any, error) { s, err := g.kml(); return s, geoErr("ST_ASKML", err) }),
		"ST_GEOHASH": geo([]string{"geography_expression", "maxchars"}, geoSig(stringType, 1, "GEO", "INT64"), func(c *call, vs []any) (any, error) {
			g := asGeog(vs[0])
			if g.isEmpty() {
				return nil, nil
			}
			p, err := singlePoint(c.name, g)
			if err != nil {
				return nil, err
			}
			n := 20
			if m, ok := opt(vs, 1).(int64); ok && m >= 1 && m <= 20 {
				n = int(m)
			}
			return geohashEncode(p, n), nil
		}),
		// Accessors.
		"ST_DIMENSION":     g1(int64Type, func(g *geog) (any, error) { return int64(g.dimension()), nil }),
		"ST_ISEMPTY":       g1(boolType, func(g *geog) (any, error) { return g.isEmpty(), nil }),
		"ST_NUMGEOMETRIES": g1(int64Type, func(g *geog) (any, error) { return int64(g.numGeometries()), nil }),
		"ST_NUMPOINTS":     g1(int64Type, func(g *geog) (any, error) { return int64(len(g.vertices())), nil }),
		"ST_NPOINTS":       g1(int64Type, func(g *geog) (any, error) { return int64(len(g.vertices())), nil }),
		"ST_ISCOLLECTION":  g1(boolType, func(g *geog) (any, error) { return g.numGeometries() > 1, nil }),
		"ST_GEOMETRYTYPE":  g1(stringType, func(g *geog) (any, error) { return geometryType(g), nil }),
		"ST_ISCLOSED": g1(boolType, func(g *geog) (any, error) {
			if g.isEmpty() {
				return false, nil
			}
			for _, l := range g.lines {
				if l[0] != l[len(l)-1] {
					return false, nil
				}
			}
			return !g.hasPoly() || g.poly.IsFull(), nil
		}),
		"ST_ISRING": g1(boolType, func(g *geog) (any, error) {
			if len(g.lines) != 1 || len(g.points) > 0 || g.hasPoly() {
				return false, nil
			}
			l := g.lines[0]
			return l[0] == l[len(l)-1] && simpleLine(l), nil
		}),
		"ST_X": g1(float64Type, func(g *geog) (any, error) {
			p, err := singlePoint("ST_X", g)
			if err != nil {
				return nil, err
			}
			lng, _ := lngLat(p)
			return round15(lng), nil
		}),
		"ST_Y": g1(float64Type, func(g *geog) (any, error) {
			p, err := singlePoint("ST_Y", g)
			if err != nil {
				return nil, err
			}
			_, lat := lngLat(p)
			return round15(lat), nil
		}),
		"ST_STARTPOINT": g1(geographyType, func(g *geog) (any, error) {
			l, err := singleLine("ST_STARTPOINT", g)
			if err != nil {
				return nil, err
			}
			return pointGeog(l[0]), nil
		}),
		"ST_ENDPOINT": g1(geographyType, func(g *geog) (any, error) {
			l, err := singleLine("ST_ENDPOINT", g)
			if err != nil {
				return nil, err
			}
			return pointGeog(l[len(l)-1]), nil
		}),
		"ST_POINTN": geo(nil, geoSig(geographyType, 2, "GEO", "INT64"), func(c *call, vs []any) (any, error) {
			l, err := singleLine(c.name, asGeog(vs[0]))
			if err != nil {
				return nil, err
			}
			n := vs[1].(int64)
			i := n - 1
			if n < 0 {
				i = int64(len(l)) + n
			}
			if n == 0 || i < 0 || i >= int64(len(l)) {
				return nil, fmt.Errorf("ST_POINTN: the linestring has no vertex at index %d", n)
			}
			return pointGeog(l[i]), nil
		}),
		"ST_DUMP": geo(nil, geoSig(arrayType(geographyType), 1, "GEO", "INT64"), func(c *call, vs []any) (any, error) {
			dim := int64(-1)
			if len(vs) > 1 {
				dim = vs[1].(int64)
			}
			return dump(asGeog(vs[0]), int(dim)), nil
		}),
		"ST_DUMPPOINTS": g1(arrayType(geographyType), func(g *geog) (any, error) {
			out := []any{}
			for _, v := range g.vertices() {
				out = append(out, pointGeog(v))
			}
			return out, nil
		}),
		// Predicates.
		"ST_INTERSECTS": pred(intersects),
		"ST_DISJOINT":   pred(func(a, b *geog) bool { return !intersects(a, b) }),
		"ST_COVERS":     pred(covers),
		"ST_COVEREDBY":  pred(func(a, b *geog) bool { return covers(b, a) }),
		"ST_CONTAINS":   pred(contains),
		"ST_WITHIN":     pred(func(a, b *geog) bool { return contains(b, a) }),
		"ST_EQUALS": pred(func(a, b *geog) bool {
			if a.isEmpty() || b.isEmpty() {
				return a.isEmpty() && b.isEmpty()
			}
			return covers(a, b) && covers(b, a)
		}),
		"ST_TOUCHES": pred(func(a, b *geog) bool { return intersects(a, b) && !interiorsIntersect(a, b) }),
		"ST_DWITHIN": geo([]string{"geography_1", "geography_2", "distance", "use_spheroid"}, geoSig(boolType, 3, "GEO", "GEO", "FLOAT64", "BOOL"), func(c *call, vs []any) (any, error) {
			if err := noSpheroid(c.name, vs, 3); err != nil {
				return nil, err
			}
			a, b := asGeog(vs[0]), asGeog(vs[1])
			if a.isEmpty() || b.isEmpty() {
				return false, nil
			}
			return geoDistance(a, b) <= toFloat(vs[2]), nil
		}),
		"ST_INTERSECTSBOX": geo(nil, geoSig(boolType, 5, "GEO", "FLOAT64", "FLOAT64", "FLOAT64", "FLOAT64"), func(c *call, vs []any) (any, error) {
			lng1, lat1, lng2, lat2 := toFloat(vs[1]), toFloat(vs[2]), toFloat(vs[3]), toFloat(vs[4])
			if lat1 < -90 || lat2 > 90 || lat1 > lat2 {
				return nil, fmt.Errorf("ST_INTERSECTSBOX: latitudes must be in [-90, 90], the first not above the second")
			}
			if lng2 < lng1 {
				lng2 += 360
			}
			if lng2-lng1 >= 360 {
				lng1, lng2 = -180, 180
			}
			return intersects(asGeog(vs[0]), boxGeog(lng1, lat1, lng2, lat2)), nil
		}),
		"ST_HAUSDORFFDWITHIN": geo([]string{"geography_1", "geography_2", "distance", "directed"}, geoSig(boolType, 3, "GEO", "GEO", "FLOAT64", "BOOL"), func(c *call, vs []any) (any, error) {
			d, ok := hausdorff(asGeog(vs[0]), asGeog(vs[1]), optBool(vs, 3))
			if !ok {
				return nil, nil
			}
			return d <= toFloat(vs[2]), nil
		}),
		// Measures.
		"ST_DISTANCE": geo([]string{"geography_1", "geography_2", "use_spheroid"}, geoSig(float64Type, 2, "GEO", "GEO", "BOOL"), func(c *call, vs []any) (any, error) {
			if optBool(vs, 2) {
				return nil, unsupportedErr{"ST_DISTANCE on the WGS84 spheroid (use_spheroid => TRUE)"}
			}
			a, b := asGeog(vs[0]), asGeog(vs[1])
			if a.isEmpty() || b.isEmpty() {
				return nil, nil
			}
			return geoDistance(a, b), nil
		}),
		"ST_MAXDISTANCE": geo([]string{"geography_1", "geography_2", "use_spheroid"}, geoSig(float64Type, 2, "GEO", "GEO", "BOOL"), func(c *call, vs []any) (any, error) {
			if err := noSpheroid(c.name, vs, 2); err != nil {
				return nil, err
			}
			a, b := asGeog(vs[0]), asGeog(vs[1])
			if a.isEmpty() || b.isEmpty() {
				return nil, nil
			}
			var m s1.Angle
			bv := b.vertices()
			for _, p := range a.vertices() {
				for _, q := range bv {
					if d := p.Distance(q); d > m {
						m = d
					}
				}
			}
			return meters(m), nil
		}),
		"ST_LENGTH": geo([]string{"geography_expression", "use_spheroid"}, geoSig(float64Type, 1, "GEO", "BOOL"), func(c *call, vs []any) (any, error) {
			if err := noSpheroid(c.name, vs, 1); err != nil {
				return nil, err
			}
			var sum float64
			for _, l := range asGeog(vs[0]).lines {
				sum += lineLength(l)
			}
			return sum, nil
		}),
		"ST_PERIMETER": geo([]string{"geography_expression", "use_spheroid"}, geoSig(float64Type, 1, "GEO", "BOOL"), func(c *call, vs []any) (any, error) {
			if err := noSpheroid(c.name, vs, 1); err != nil {
				return nil, err
			}
			var sum float64
			for _, p := range asGeog(vs[0]).polygons() {
				for _, r := range p {
					if len(r) > 0 {
						sum += lineLength(append(append([]s2.Point(nil), r...), r[0]))
					}
				}
			}
			return sum, nil
		}),
		"ST_AREA": geo([]string{"geography_expression", "use_spheroid"}, geoSig(float64Type, 1, "GEO", "BOOL"), func(c *call, vs []any) (any, error) {
			if err := noSpheroid(c.name, vs, 1); err != nil {
				return nil, err
			}
			g := asGeog(vs[0])
			if !g.hasPoly() {
				return 0.0, nil
			}
			return g.poly.Area() * earthRadiusMeters * earthRadiusMeters, nil
		}),
		"ST_AZIMUTH": geo(nil, geoSig(float64Type, 2, "GEO", "GEO"), func(c *call, vs []any) (any, error) {
			p1, err := singlePoint(c.name, asGeog(vs[0]))
			if err != nil {
				return nil, err
			}
			p2, err := singlePoint(c.name, asGeog(vs[1]))
			if err != nil {
				return nil, err
			}
			az, ok := azimuth(p1, p2)
			if !ok {
				return nil, nil
			}
			return az, nil
		}),
		"ST_ANGLE": geo(nil, geoSig(float64Type, 3, "GEO", "GEO", "GEO"), func(c *call, vs []any) (any, error) {
			var ps [3]s2.Point
			for i := range ps {
				p, err := singlePoint(c.name, asGeog(vs[i]))
				if err != nil {
					return nil, err
				}
				ps[i] = p
			}
			a1, ok1 := azimuth(ps[1], ps[0])
			a3, ok3 := azimuth(ps[1], ps[2])
			if !ok1 || !ok3 {
				return nil, nil
			}
			a := math.Mod(a3-a1+2*math.Pi, 2*math.Pi)
			if a >= 2*math.Pi {
				a = 0
			}
			return a, nil
		}),
		"ST_BOUNDINGBOX": g1(boxType, func(g *geog) (any, error) {
			if g.isEmpty() {
				return nil, nil
			}
			return boxValue(g.rectBound(), false), nil
		}),
		"ST_HAUSDORFFDISTANCE": geo([]string{"geography_1", "geography_2", "directed"}, geoSig(float64Type, 2, "GEO", "GEO", "BOOL"), func(c *call, vs []any) (any, error) {
			d, ok := hausdorff(asGeog(vs[0]), asGeog(vs[1]), optBool(vs, 2))
			if !ok {
				return nil, nil
			}
			return d, nil
		}),
		"ST_LINELOCATEPOINT": geo(nil, geoSig(float64Type, 2, "GEO", "GEO"), func(c *call, vs []any) (any, error) {
			l, err := singleLine(c.name, asGeog(vs[0]))
			if err != nil {
				return nil, err
			}
			p, err := singlePoint(c.name, asGeog(vs[1]))
			if err != nil {
				return nil, err
			}
			pl := s2.Polyline(l)
			proj, next := pl.Project(p)
			return pl.Uninterpolate(proj, next), nil
		}),
		// Transformations.
		"ST_LINEINTERPOLATEPOINT": geo(nil, geoSig(geographyType, 2, "GEO", "FLOAT64"), func(c *call, vs []any) (any, error) {
			g, f := asGeog(vs[0]), toFloat(vs[1])
			if f < 0 || f > 1 || math.IsNaN(f) {
				return nil, fmt.Errorf("ST_LINEINTERPOLATEPOINT: the fraction must be in [0, 1], not %v", f)
			}
			if g.isEmpty() {
				return &geog{}, nil
			}
			l, err := singleLine(c.name, g)
			if err != nil {
				return nil, err
			}
			pl := s2.Polyline(l)
			p, _ := pl.Interpolate(f)
			return pointGeog(p), nil
		}),
		"ST_LINESUBSTRING": geo(nil, geoSig(geographyType, 3, "GEO", "FLOAT64", "FLOAT64"), func(c *call, vs []any) (any, error) {
			l, err := singleLine(c.name, asGeog(vs[0]))
			if err != nil {
				return nil, err
			}
			f0, f1 := toFloat(vs[1]), toFloat(vs[2])
			if f0 < 0 || f0 > 1 || f1 < 0 || f1 > 1 || f1 < f0 {
				return nil, fmt.Errorf("ST_LINESUBSTRING: the fractions must be in [0, 1], the end not before the start")
			}
			return lineSubstring(l, f0, f1), nil
		}),
		"ST_CENTROID": g1(geographyType, func(g *geog) (any, error) {
			if g.isEmpty() {
				return &geog{}, nil
			}
			return pointGeog(centroidSum(g, g.dimension()).point()), nil
		}),
		"ST_CLOSESTPOINT": geo([]string{"geography_1", "geography_2", "use_spheroid"}, geoSig(geographyType, 2, "GEO", "GEO", "BOOL"), func(c *call, vs []any) (any, error) {
			if err := noSpheroid(c.name, vs, 2); err != nil {
				return nil, err
			}
			a, b := asGeog(vs[0]), asGeog(vs[1])
			if a.isEmpty() || b.isEmpty() {
				return nil, nil
			}
			return pointGeog(closestPoint(a, b)), nil
		}),
		"ST_CONVEXHULL": g1(geographyType, func(g *geog) (any, error) {
			if g.isEmpty() {
				return nil, nil
			}
			return convexHull(g)
		}),
		"ST_BOUNDARY": g1(geographyType, func(g *geog) (any, error) {
			out := &geog{}
			for _, l := range g.lines {
				if l[0] != l[len(l)-1] {
					out.points = appendDistinct(out.points, l[0], l[len(l)-1])
				}
			}
			for _, p := range g.polygons() {
				for _, r := range p {
					if len(r) > 0 {
						out.lines = append(out.lines, append(append([]s2.Point(nil), r...), r[0]))
					}
				}
			}
			return out, nil
		}),
		"ST_EXTERIORRING": g1(geographyType, func(g *geog) (any, error) {
			ps := g.polygons()
			if len(ps) != 1 || len(g.points) > 0 || len(g.lines) > 0 {
				return nil, fmt.Errorf("ST_EXTERIORRING: the argument must be a single polygon")
			}
			if len(ps[0]) == 0 {
				return &geog{}, nil
			}
			r := ps[0][0]
			return &geog{lines: [][]s2.Point{append(append([]s2.Point(nil), r...), r[0])}}, nil
		}),
		"ST_INTERIORRINGS": g1(arrayType(geographyType), func(g *geog) (any, error) {
			ps := g.polygons()
			if len(ps) != 1 || len(g.points) > 0 || len(g.lines) > 0 {
				return nil, fmt.Errorf("ST_INTERIORRINGS: the argument must be a single polygon")
			}
			out := []any{}
			for i, r := range ps[0] {
				if i > 0 {
					out = append(out, &geog{lines: [][]s2.Point{append(append([]s2.Point(nil), r...), r[0])}})
				}
			}
			return out, nil
		}),
		"ST_SNAPTOGRID": geo(nil, geoSig(geographyType, 2, "GEO", "FLOAT64"), func(c *call, vs []any) (any, error) {
			size := toFloat(vs[1])
			if !(size > 0) {
				return nil, fmt.Errorf("ST_SNAPTOGRID: the grid size must be positive")
			}
			n := math.Round(math.Log10(size))
			n = math.Max(-9, math.Min(-1, n)) // "10^n, where -10 < n < 0"
			return snapToGrid(asGeog(vs[0]), n)
		}),
		// S2 functions.
		"S2_CELLIDFROMPOINT": geo([]string{"point_geography", "level"}, geoSig(int64Type, 1, "GEO", "INT64"), func(c *call, vs []any) (any, error) {
			p, err := singlePoint(c.name, asGeog(vs[0]))
			if err != nil {
				return nil, err
			}
			level := int64(30)
			if l, ok := opt(vs, 1).(int64); ok {
				level = l
			}
			if level < 0 || level > 30 {
				return nil, fmt.Errorf("S2_CELLIDFROMPOINT: level must be in [0, 30], not %d", level)
			}
			return int64(s2.CellFromPoint(p).ID().Parent(int(level))), nil
		}),
		"S2_COVERINGCELLIDS": geo([]string{"geography", "min_level", "max_level", "max_cells", "buffer"},
			geoSig(arrayType(int64Type), 1, "GEO", "INT64", "INT64", "INT64", "FLOAT64"), func(c *call, vs []any) (any, error) {
				return coveringCellIDs(asGeog(vs[0]), vs)
			}),
	}
	for name, why := range geoRefused {
		fs[name] = &fnDef{sig: func(c *call, ts []*btpb.Type) (*btpb.Type, error) {
			return nil, unsupportedErr{"the geography function " + c.name + ", " + why + ","}
		}}
		_ = name
	}
	for k, v := range fs {
		functions[k] = v
	}
	for k := range geoAggregates {
		moreAggregates[k] = true
		aggregates[k] = true
	}
}

// boxType is ST_BOUNDINGBOX's and ST_EXTENT's STRUCT.
var boxType = structType([]string{"xmin", "ymin", "xmax", "ymax"}, []*btpb.Type{float64Type, float64Type, float64Type, float64Type})

func isHex(s string) bool {
	if len(s) == 0 || len(s)%2 != 0 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

func wkbBytes(v any) ([]byte, error) {
	switch x := v.(type) {
	case []byte:
		return x, nil
	case string:
		b, err := hex.DecodeString(strings.TrimSpace(x))
		if err != nil {
			return nil, fmt.Errorf("the WKB string is not hexadecimal: %v", err)
		}
		return b, nil
	}
	return nil, fmt.Errorf("the WKB must be BYTES or a hexadecimal STRING")
}

// geoList is ST_MAKELINE's inputs: two GEOGRAPHYs or an array of them;
// ok is false when the array or an element is NULL.
func geoList(vs []any) ([]*geog, bool) {
	if len(vs) == 2 {
		return []*geog{asGeog(vs[0]), asGeog(vs[1])}, true
	}
	var out []*geog
	for _, e := range vs[0].([]any) {
		if e == nil {
			return nil, false
		}
		out = append(out, asGeog(e))
	}
	return out, true
}

func geometryType(g *geog) string {
	np, nl, npoly := len(g.points), len(g.lines), len(g.polygons())
	switch {
	case np == 1 && nl == 0 && npoly == 0:
		return "ST_Point"
	case np > 1 && nl == 0 && npoly == 0:
		return "ST_MultiPoint"
	case np == 0 && nl == 1 && npoly == 0:
		return "ST_LineString"
	case np == 0 && nl > 1 && npoly == 0:
		return "ST_MultiLineString"
	case np == 0 && nl == 0 && npoly == 1:
		return "ST_Polygon"
	case np == 0 && nl == 0 && npoly > 1:
		return "ST_MultiPolygon"
	}
	return "ST_GeometryCollection"
}

// simpleLine is whether a linestring passes through no point twice but
// its closing endpoint.
func simpleLine(l []s2.Point) bool {
	n := len(l) - 1
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			adjacent := j == i+1 || i == 0 && j == n-1 && l[0] == l[n]
			if adjacent {
				if j == i+1 && l[i] == l[j+1] { // doubles back
					return false
				}
				continue
			}
			if s2.CrossingSign(l[i], l[i+1], l[j], l[j+1]) != s2.DoNotCross || l[i] == l[j] || l[i] == l[j+1] || l[i+1] == l[j] || l[i+1] == l[j+1] {
				return false
			}
		}
	}
	return true
}

// dump is ST_DUMP: each point, linestring and polygon of g, of dimension
// dim (-1 for all).
func dump(g *geog, dim int) []any {
	out := []any{}
	if dim == -1 || dim == 0 {
		for _, p := range g.points {
			out = append(out, pointGeog(p))
		}
	}
	if dim == -1 || dim == 1 {
		for _, l := range g.lines {
			out = append(out, &geog{lines: [][]s2.Point{l}})
		}
	}
	if dim == -1 || dim == 2 {
		for _, p := range g.polygons() {
			one := &geog{}
			if len(p) == 0 {
				one.poly = s2.FullPolygon()
			} else {
				// WKT's rings: the shell, then the holes reversed.
				_ = one.addPolygons([][][]s2.Point{p}, true)
			}
			out = append(out, one)
		}
	}
	return out
}

func appendDistinct(ps []s2.Point, add ...s2.Point) []s2.Point {
	for _, a := range add {
		dup := false
		for _, p := range ps {
			dup = dup || p == a
		}
		if !dup {
			ps = append(ps, a)
		}
	}
	return ps
}

// tiny is the distance under which a point is on an edge: S2's
// intersection error (s2.IntersectionError, about 8e-16 radians).
const tiny = 1e-15

func onSegment(p, a, b s2.Point) bool { return s2.DistanceFromSegment(p, a, b) <= tiny }

func onLines(p s2.Point, lines [][]s2.Point) bool {
	for _, l := range lines {
		for i := 0; i+1 < len(l); i++ {
			if onSegment(p, l[i], l[i+1]) {
				return true
			}
		}
	}
	return false
}

// onBoundary is whether p is on a polygon's boundary.
func (g *geog) onBoundary(p s2.Point) bool {
	if !g.hasPoly() {
		return false
	}
	for _, l := range g.poly.Loops() {
		vs := l.Vertices()
		for i := range vs {
			if onSegment(p, vs[i], vs[(i+1)%len(vs)]) {
				return true
			}
		}
	}
	return false
}

// polyCovers is whether p is in a polygon's interior or on its boundary.
func (g *geog) polyCovers(p s2.Point) bool {
	return g.hasPoly() && (g.poly.ContainsPoint(p) || g.onBoundary(p))
}

// pointIn is whether p is a point of g (its closure).
func (g *geog) pointIn(p s2.Point) bool {
	for _, q := range g.points {
		if q == p {
			return true
		}
	}
	return onLines(p, g.lines) || g.polyCovers(p)
}

// interiorPoint is whether p is in g's interior: a point itself, a line
// but its endpoints, a polygon but its boundary.
func (g *geog) interiorPoint(p s2.Point) bool {
	for _, q := range g.points {
		if q == p {
			return true
		}
	}
	for _, l := range g.lines {
		closed := l[0] == l[len(l)-1]
		if onLines(p, [][]s2.Point{l}) && (closed || p != l[0] && p != l[len(l)-1]) {
			return true
		}
	}
	return g.hasPoly() && g.poly.ContainsPoint(p) && !g.onBoundary(p)
}

func edges(g *geog) [][2]s2.Point {
	var es [][2]s2.Point
	for _, l := range g.lines {
		for i := 0; i+1 < len(l); i++ {
			es = append(es, [2]s2.Point{l[i], l[i+1]})
		}
	}
	if g.hasPoly() {
		for _, l := range g.poly.Loops() {
			vs := l.Vertices()
			for i := range vs {
				es = append(es, [2]s2.Point{vs[i], vs[(i+1)%len(vs)]})
			}
		}
	}
	return es
}

// intersects is ST_INTERSECTS: a point both have.
func intersects(a, b *geog) bool {
	if a.isEmpty() || b.isEmpty() {
		return false
	}
	if a.hasPoly() && a.poly.IsFull() || b.hasPoly() && b.poly.IsFull() {
		return true
	}
	for _, v := range b.vertices() {
		if a.pointIn(v) {
			return true
		}
	}
	for _, v := range a.vertices() {
		if b.pointIn(v) {
			return true
		}
	}
	eb := edges(b)
	for _, x := range edges(a) {
		for _, y := range eb {
			if s2.CrossingSign(x[0], x[1], y[0], y[1]) != s2.DoNotCross {
				return true
			}
		}
	}
	return false
}

// covers is ST_COVERS: no point of b outside a. A line or polygon of b is
// covered when its vertices are and its edges cross no boundary of a, and
// (lines) its edges' midpoints are on a.
func covers(a, b *geog) bool {
	if a.isEmpty() || b.isEmpty() {
		return false
	}
	if a.hasPoly() && a.poly.IsFull() {
		return true
	}
	for _, p := range b.points {
		if !a.pointIn(p) {
			return false
		}
	}
	ea := edges(a)
	for _, l := range b.lines {
		for i := 0; i+1 < len(l); i++ {
			if !segmentCovered(a, ea, l[i], l[i+1]) {
				return false
			}
		}
	}
	if b.hasPoly() {
		if b.poly.IsFull() || !a.hasPoly() {
			return false
		}
		if !a.poly.Contains(b.poly) {
			// S2's containment is semi-open: a shared boundary can still be
			// covered; check vertices and edge midpoints.
			for _, e := range edges(b) {
				if !segmentCovered(a, ea, e[0], e[1]) {
					return false
				}
			}
			if !a.poly.Intersects(b.poly) && !a.polyCovers(b.poly.Loop(0).Vertex(0)) {
				return false
			}
		}
	}
	return true
}

// segmentCovered is whether the edge p-q lies in a.
func segmentCovered(a *geog, ea [][2]s2.Point, p, q s2.Point) bool {
	if !a.pointIn(p) || !a.pointIn(q) {
		return false
	}
	mid := s2.Interpolate(0.5, p, q)
	if !a.pointIn(mid) {
		return false
	}
	if a.hasPoly() {
		for _, e := range ea {
			if s2.CrossingSign(p, q, e[0], e[1]) == s2.Cross && !onLines(s2.Point{Vector: crossPoint(p, q, e[0], e[1])}, a.lines) {
				return false
			}
		}
	}
	return true
}

func crossPoint(a, b, c, d s2.Point) r3.Vector { return s2.Intersection(a, b, c, d).Vector }

// interiorsIntersect is whether a's and b's interiors share a point.
func interiorsIntersect(a, b *geog) bool {
	if a.hasPoly() && b.hasPoly() && a.poly.Intersects(b.poly) {
		// S2's polygon model is semi-open, so shared boundaries alone do
		// not intersect: an overlap of their interiors.
		return true
	}
	for _, x := range [][2]*geog{{a, b}, {b, a}} {
		g, h := x[0], x[1]
		for _, p := range g.points {
			if h.interiorPoint(p) {
				return true
			}
		}
		for _, l := range g.lines {
			for i := 0; i+1 < len(l); i++ {
				mid := s2.Interpolate(0.5, l[i], l[i+1])
				if h.interiorPoint(mid) {
					return true
				}
				for _, l2 := range h.lines {
					for j := 0; j+1 < len(l2); j++ {
						if s2.CrossingSign(l[i], l[i+1], l2[j], l2[j+1]) == s2.Cross {
							return true
						}
					}
				}
				if h.hasPoly() {
					for _, e := range edges(&geog{poly: h.poly}) {
						if s2.CrossingSign(l[i], l[i+1], e[0], e[1]) == s2.Cross {
							return true
						}
					}
				}
			}
			for _, v := range l[1 : len(l)-1] {
				if h.interiorPoint(v) {
					return true
				}
			}
		}
	}
	return false
}

// contains is ST_CONTAINS: b covered by a, and their interiors intersect.
func contains(a, b *geog) bool { return covers(a, b) && interiorsIntersect(a, b) }

// distance is the shortest distance in meters between a and b.
func geoDistance(a, b *geog) float64 {
	// The target side does not see its polygons' interiors: ask both ways.
	one := func(x, y *geog) s1.Angle {
		q := s2.NewClosestEdgeQuery(x.shapeIndex(), s2.NewClosestEdgeQueryOptions().IncludeInteriors(true))
		return q.Distance(s2.NewMinDistanceToShapeIndexTarget(y.shapeIndex())).Angle()
	}
	return meters(min(one(a, b), one(b, a)))
}

// distToGeog is the shortest distance from p to g.
func distToGeog(p s2.Point, idx *s2.ShapeIndex) s1.Angle {
	q := s2.NewClosestEdgeQuery(idx, s2.NewClosestEdgeQueryOptions().IncludeInteriors(true))
	return q.Distance(s2.NewMinDistanceToPointTarget(p)).Angle()
}

// hausdorff is the discrete Hausdorff distance: the greatest distance from
// a vertex of one to the other; undirected, the greater both ways.
func hausdorff(a, b *geog, directed bool) (float64, bool) {
	if a.isEmpty() || b.isEmpty() {
		return 0, false
	}
	one := func(x, y *geog) float64 {
		idx := y.shapeIndex()
		var m s1.Angle
		for _, v := range x.vertices() {
			if d := distToGeog(v, idx); d > m {
				m = d
			}
		}
		return meters(m)
	}
	d := one(a, b)
	if !directed {
		d = math.Max(d, one(b, a))
	}
	return d, true
}

func lineLength(l []s2.Point) float64 {
	var a s1.Angle
	for i := 0; i+1 < len(l); i++ {
		a += l[i].Distance(l[i+1])
	}
	return meters(a)
}

// azimuth is the initial bearing from p to q, clockwise from north in
// [0, 2pi); ok is false for equal or antipodal points.
func azimuth(p, q s2.Point) (float64, bool) {
	if p == q || p.Vector == q.Mul(-1) {
		return 0, false
	}
	a, b := s2.LatLngFromPoint(p), s2.LatLngFromPoint(q)
	dl := (b.Lng - a.Lng).Radians()
	y := math.Sin(dl) * math.Cos(b.Lat.Radians())
	x := math.Cos(a.Lat.Radians())*math.Sin(b.Lat.Radians()) - math.Sin(a.Lat.Radians())*math.Cos(b.Lat.Radians())*math.Cos(dl)
	az := math.Atan2(y, x)
	if az < 0 {
		az += 2 * math.Pi
	}
	if az >= 2*math.Pi {
		az = 0
	}
	return az, true
}

// rectBound is the smallest latitude-longitude rectangle around g.
func (g *geog) rectBound() s2.Rect {
	r := s2.EmptyRect()
	for _, p := range g.points {
		r = r.AddPoint(s2.LatLngFromPoint(p))
	}
	for _, l := range g.lines {
		pl := s2.Polyline(l)
		r = r.Union(pl.RectBound())
	}
	if g.hasPoly() {
		r = r.Union(g.poly.RectBound())
	}
	return r
}

// boxValue is a rectangle as ST_BOUNDINGBOX's STRUCT: across the
// antimeridian xmax is above 180; extent widens a span of 180 degrees or
// more to [-180, 180].
func boxValue(r s2.Rect, extent bool) []any {
	lo, hi := r.Lng.Lo*180/math.Pi, r.Lng.Hi*180/math.Pi
	if r.Lng.IsFull() || extent && r.Lng.Length() >= math.Pi {
		lo, hi = -180, 180
	} else if r.Lng.IsInverted() {
		hi += 360
	}
	// to 15 significant digits, as WKT's coordinates (UNVERIFIED, re-read 2026-11-17: geography_functions).
	return []any{round15(lo), round15(r.Lat.Lo * 180 / math.Pi), round15(hi), round15(r.Lat.Hi * 180 / math.Pi)}
}

// boxGeog is ST_INTERSECTSBOX's rectangle as a polygon, its edges of
// constant latitude densified to a tenth of a degree.
func boxGeog(lng1, lat1, lng2, lat2 float64) *geog {
	if lng2-lng1 >= 360 || lat1 == lat2 || lng1 == lng2 {
		// a degenerate or full-width box: its points, and its edges as lines.
		g := &geog{}
		_ = g.addLine(latLine(lng1, lng2, lat1))
		_ = g.addLine(latLine(lng2, lng1+360*math.Floor((lng2-lng1)/360), lat2))
		if lat1 == -90 || lat2 == 90 || lng2-lng1 >= 360 {
			_ = g.addLine([]s2.Point{llPoint(lng1, lat1), llPoint(lng1, lat2)})
		}
		return g
	}
	ring := latLine(lng1, lng2, lat1)
	ring = append(ring, latLine(lng2, lng1, lat2)...)
	g := &geog{}
	if err := g.addPolygons([][][]s2.Point{{ring}}, true); err != nil {
		_ = g.addLine(ring)
	}
	return g
}

// latLine are points along a constant latitude from lng a to lng b.
func latLine(a, b, lat float64) []s2.Point {
	n := int(math.Ceil(math.Abs(b-a)/0.1)) + 1
	var ps []s2.Point
	for i := 0; i < n; i++ {
		ps = append(ps, llPoint(a+(b-a)*float64(i)/float64(n-1), lat))
	}
	return ps
}

// lineSubstring is the part of l between two fractions of its length.
func lineSubstring(l []s2.Point, f0, f1 float64) *geog {
	pl := s2.Polyline(l)
	total := pl.Length()
	a0, a1 := s1.Angle(f0)*total, s1.Angle(f1)*total
	var out []s2.Point
	var at s1.Angle
	pointAt := func(d s1.Angle) s2.Point {
		var acc s1.Angle
		for i := 0; i+1 < len(l); i++ {
			e := l[i].Distance(l[i+1])
			if acc+e >= d {
				return s2.InterpolateAtDistance(d-acc, l[i], l[i+1])
			}
			acc += e
		}
		return l[len(l)-1]
	}
	out = append(out, pointAt(a0))
	for i := 0; i+1 < len(l); i++ {
		at += l[i].Distance(l[i+1])
		if at > a0 && at < a1 {
			out = append(out, l[i+1])
		}
	}
	out = append(out, pointAt(a1))
	g := &geog{}
	_ = g.addLine(out)
	return g
}

// centroidAcc accumulates a centroid: points summed as unit vectors,
// edges and polygons weighted by length and area, as S2 does.
type centroidAcc struct {
	v   r3.Vector
	dim int
}

func (c centroidAcc) point() s2.Point {
	if c.v.Norm() == 0 {
		return s2.PointFromCoords(1, 0, 0) // "deterministic but otherwise arbitrary"
	}
	return s2.Point{Vector: c.v.Normalize()}
}

func centroidSum(g *geog, dim int) centroidAcc {
	c := centroidAcc{dim: dim}
	switch dim {
	case 0:
		for _, p := range g.points {
			c.v = c.v.Add(p.Vector)
		}
	case 1:
		for _, l := range g.lines {
			for i := 0; i+1 < len(l); i++ {
				c.v = c.v.Add(s2.EdgeTrueCentroid(l[i], l[i+1]).Vector)
			}
		}
	case 2:
		c.v = g.poly.Centroid().Vector
	}
	return c
}

// closestPoint is the point of a nearest to b.
func closestPoint(a, b *geog) s2.Point {
	for _, v := range a.vertices() {
		if b.pointIn(v) {
			return v
		}
	}
	best, bestD := s2.Point{}, s1.Angle(math.Inf(1))
	try := func(p s2.Point, d s1.Angle) {
		if d < bestD {
			best, bestD = p, d
		}
	}
	ea, eb := edges(a), edges(b)
	for _, p := range a.points {
		for _, q := range b.vertices() {
			try(p, p.Distance(q))
		}
		for _, e := range eb {
			try(p, s2.DistanceFromSegment(p, e[0], e[1]))
		}
	}
	for _, e := range ea {
		for _, q := range b.points {
			x := s2.Project(q, e[0], e[1])
			try(x, x.Distance(q))
		}
		for _, f := range eb {
			x, y := s2.EdgePairClosestPoints(e[0], e[1], f[0], f[1])
			try(x, x.Distance(y))
		}
	}
	return best
}

// convexHull is ST_CONVEXHULL: a point for one point, a linestring for
// collinear points, else a polygon.
func convexHull(g *geog) (any, error) {
	vs := g.vertices()
	var distinct []s2.Point
	distinct = appendDistinct(distinct, vs...)
	if len(distinct) <= 2 && !g.hasPoly() {
		// S2's hull of one point or two is a degenerate loop: the point and
		// two offset vertices, or the edge's ends and midpoint. GoogleSQL
		// returns its vertices reversed from the second, as a point or a
		// linestring: the reference's POINT(0.999999999999943 1) of POINT(1 1)
		// and LINESTRING(2 2, 1.49988573656168 1.5000570914792, 1 1) of
		// LINESTRING(1 1, 2 2).
		q := s2.NewConvexHullQuery()
		for _, v := range distinct {
			q.AddPoint(v)
		}
		h := q.ConvexHull().Vertices()
		if len(h) == 3 {
			if len(distinct) == 1 {
				return pointGeog(h[1]), nil
			}
			return &geog{lines: [][]s2.Point{{h[1], h[0], h[2]}}}, nil
		}
	}
	q := s2.NewConvexHullQuery()
	for _, v := range distinct {
		q.AddPoint(v)
	}
	if g.hasPoly() {
		q.AddPolygon(g.poly)
	}
	hull := q.ConvexHull()
	if hull.IsFull() {
		return &geog{poly: s2.FullPolygon()}, nil
	}
	collinear := true
	for i := 2; i < len(distinct) && collinear; i++ {
		collinear = math.Abs(distinct[i].Dot(distinct[0].Cross(distinct[1].Vector))) < tiny
	}
	if collinear {
		// the two points furthest apart.
		var a, b s2.Point
		var m s1.Angle = -1
		for i := range distinct {
			for j := i + 1; j < len(distinct); j++ {
				if d := distinct[i].Distance(distinct[j]); d > m {
					a, b, m = distinct[i], distinct[j], d
				}
			}
		}
		line := []s2.Point{a, b}
		return &geog{lines: [][]s2.Point{line}}, nil
	}
	out := &geog{}
	return out, out.addPolygons([][][]s2.Point{{hull.Vertices()}}, true)
}

// snapToGrid rounds every vertex to a 10^n degree grid.
func snapToGrid(g *geog, n float64) (*geog, error) {
	grid := math.Pow(10, n)
	snap := func(p s2.Point) s2.Point {
		lng, lat := lngLat(p)
		return llPoint(math.Round(lng/grid)*grid, math.Round(lat/grid)*grid)
	}
	out := &geog{}
	for _, p := range g.points {
		out.points = appendDistinct(out.points, snap(p))
	}
	for _, l := range g.lines {
		var vs []s2.Point
		for _, v := range l {
			vs = append(vs, snap(v))
		}
		if err := out.addLine(vs); err != nil {
			return nil, err
		}
	}
	var polys [][][]s2.Point
	for _, p := range g.polygons() {
		var rings [][]s2.Point
		for i, r := range p {
			var vs []s2.Point
			for _, v := range r {
				vs = append(vs, snap(v))
			}
			if i > 0 {
				for x, y := 0, len(vs)-1; x < y; x, y = x+1, y-1 {
					vs[x], vs[y] = vs[y], vs[x]
				}
			}
			rings = append(rings, vs)
		}
		polys = append(polys, rings)
	}
	if len(polys) > 0 {
		if err := out.addPolygons(polys, true); err != nil {
			return nil, fmt.Errorf("ST_SNAPTOGRID: %v", err)
		}
	}
	return out, nil
}

// coveringCellIDs is S2_COVERINGCELLIDS: at most max_cells (default 8) S2
// cells of levels min_level (0) to max_level (30) covering g, expanded by
// buffer meters (0).
func coveringCellIDs(g *geog, vs []any) (any, error) {
	minL, maxL, maxCells, buffer := int64(0), int64(30), int64(8), 0.0
	if v, ok := opt(vs, 1).(int64); ok {
		minL = v
	}
	if v, ok := opt(vs, 2).(int64); ok {
		maxL = v
	}
	if v, ok := opt(vs, 3).(int64); ok {
		maxCells = v
	}
	if v := opt(vs, 4); v != nil {
		buffer = toFloat(v)
	}
	if minL < 0 || minL > 30 || maxL < 0 || maxL > 30 || minL > maxL {
		return nil, fmt.Errorf("S2_COVERINGCELLIDS: levels must be in [0, 30], min_level not above max_level")
	}
	if maxCells < 1 {
		return nil, fmt.Errorf("S2_COVERINGCELLIDS: max_cells must be positive")
	}
	if buffer < 0 || math.IsNaN(buffer) {
		return nil, fmt.Errorf("S2_COVERINGCELLIDS: buffer must be nonnegative")
	}
	out := []any{}
	if g.isEmpty() {
		return out, nil
	}
	rc := &s2.RegionCoverer{MinLevel: int(minL), MaxLevel: int(maxL), LevelMod: 1, MaxCells: int(maxCells)}
	var region s2.Region
	if buffer > 0 {
		region = &bufferedRegion{idx: g.shapeIndex(), cap: g.capBound(), r: metersAngle(buffer)}
	} else {
		var rs s2.RegionUnion
		for _, p := range g.points {
			rs = append(rs, s2.CellFromPoint(p))
		}
		for _, l := range g.lines {
			pl := s2.Polyline(l)
			rs = append(rs, &pl)
		}
		if g.hasPoly() {
			rs = append(rs, g.poly)
		}
		region = rs
	}
	cu := rc.Covering(region)
	sort.Slice(cu, func(i, j int) bool { return cu[i] < cu[j] })
	for _, id := range cu {
		out = append(out, int64(id))
	}
	return out, nil
}

func (g *geog) capBound() s2.Cap {
	return g.rectBound().CapBound()
}

// bufferedRegion is a GEOGRAPHY grown by r: a cell intersects it when it
// is within r of the geography.
type bufferedRegion struct {
	idx *s2.ShapeIndex
	cap s2.Cap
	r   s1.Angle
}

func (b *bufferedRegion) CapBound() s2.Cap {
	return s2.CapFromCenterAngle(b.cap.Center(), b.cap.Radius()+b.r)
}
func (b *bufferedRegion) RectBound() s2.Rect            { return b.CapBound().RectBound() }
func (b *bufferedRegion) ContainsCell(s2.Cell) bool     { return false }
func (b *bufferedRegion) ContainsPoint(p s2.Point) bool { return distToGeog(p, b.idx) <= b.r }
func (b *bufferedRegion) CellUnionBound() []s2.CellID   { return b.CapBound().CellUnionBound() }
func (b *bufferedRegion) IntersectsCell(c s2.Cell) bool {
	q := s2.NewClosestEdgeQuery(b.idx, s2.NewClosestEdgeQueryOptions().IncludeInteriors(true))
	return q.Distance(s2.NewMinDistanceToCellTarget(c)).Angle() <= b.r
}

// aggregateGeo computes ST_ACCUM, ST_CENTROID_AGG and ST_EXTENT.
func (c *call) aggregateGeo(envs []env) (any, error) {
	var gs []*geog
	acc := []any{}
	for _, e0 := range envs {
		e := e0
		v, err := c.args[0].eval(&e)
		if err != nil {
			return nil, err
		}
		if c.name == "ST_ACCUM" {
			acc = append(acc, v)
			continue
		}
		if v != nil {
			gs = append(gs, asGeog(v))
		}
	}
	switch c.name {
	case "ST_ACCUM":
		return acc, nil
	case "ST_EXTENT":
		r := s2.EmptyRect()
		for _, g := range gs {
			if !g.isEmpty() {
				r = r.Union(g.rectBound())
			}
		}
		if r.IsEmpty() {
			return nil, nil
		}
		return boxValue(r, true), nil
	}
	// ST_CENTROID_AGG: the centroid of the inputs' highest dimension.
	dim := -1
	for _, g := range gs {
		dim = max(dim, g.dimension())
	}
	if dim < 0 {
		return nil, nil
	}
	var sum r3.Vector
	for _, g := range gs {
		if g.dimension() == dim {
			sum = sum.Add(centroidSum(g, dim).v)
		}
	}
	return pointGeog(centroidAcc{v: sum}.point()), nil
}

// checkGeoAgg types ST_ACCUM, ST_CENTROID_AGG and ST_EXTENT.
func (c *call) checkGeoAgg(ts []*btpb.Type) error {
	if len(ts) != 1 || ts[0] != nil && !isGeography(ts[0]) {
		return sigErr(c, ts)
	}
	if c.distinct {
		return fmt.Errorf("Aggregate functions with DISTINCT cannot be used with arguments of type GEOGRAPHY")
	}
	switch c.name {
	case "ST_ACCUM":
		c.t = arrayType(geographyType)
	case "ST_CENTROID_AGG":
		c.t = geographyType
	case "ST_EXTENT":
		c.t = boxType
	}
	return nil
}
