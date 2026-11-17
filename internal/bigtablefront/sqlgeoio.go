package bigtablefront

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/golang/geo/s2"
)

// The external formats of a GEOGRAPHY (#1720): WKT (ST_GEOGFROMTEXT,
// ST_ASTEXT), WKB (ST_GEOGFROMWKB, ST_ASBINARY), GeoJSON
// (ST_GEOGFROMGEOJSON, ST_ASGEOJSON), KML (ST_GEOGFROMKML, ST_ASKML) and
// GeoHash (ST_GEOGPOINTFROMGEOHASH, ST_GEOHASH), as GoogleSQL for
// Bigtable's geography reference documents them (docs re-read 2026-11-17:
// https://cloud.google.com/bigtable/docs/reference/sql/geography_functions).

// lngLatPt is a position as the formats write it: longitude, latitude.
type lngLatPt [2]float64

// geoParts collects what a parser read before it becomes a geog.
type geoParts struct {
	points []lngLatPt
	lines  [][]lngLatPt
	polys  [][][]lngLatPt
}

// planarTolerance is how far, in meters, a planar edge and the geodesic
// edges standing for it may part: "GoogleSQL adds additional points to the
// line where necessary so that the resulting sequence of edges remains
// within 10 meters of the original edge".
const planarTolerance = 10.0

// densifyPlanar adds vertices to a chain of planar (lng/lat) edges so that
// its geodesic edges stay within 10 m of them.
func densifyPlanar(vs []lngLatPt) []lngLatPt {
	if len(vs) < 2 {
		return vs
	}
	out := []lngLatPt{vs[0]}
	var rec func(a, b lngLatPt, depth int)
	rec = func(a, b lngLatPt, depth int) {
		mp := lngLatPt{(a[0] + b[0]) / 2, (a[1] + b[1]) / 2}
		pa, pb := llPoint(a[0], a[1]), llPoint(b[0], b[1])
		mg := s2.Point{Vector: pa.Add(pb.Vector).Normalize()}
		if depth < 24 && meters(mg.Distance(llPoint(mp[0], mp[1]))) > planarTolerance {
			rec(a, mp, depth+1)
			rec(mp, b, depth+1)
			return
		}
		out = append(out, b)
	}
	for i := 1; i < len(vs); i++ {
		rec(vs[i-1], vs[i], 0)
	}
	return out
}

func checkLngLat(p lngLatPt) error {
	if math.IsNaN(p[0]) || math.IsNaN(p[1]) || math.IsInf(p[0], 0) || math.IsInf(p[1], 0) {
		return fmt.Errorf("Invalid coordinates: %v %v", p[0], p[1])
	}
	if p[1] < -90 || p[1] > 90 {
		return fmt.Errorf("Latitude %v is out of range [-90, 90]", p[1])
	}
	return nil
}

// build turns the parts into a geog; planar parts are densified first.
func (ps *geoParts) build(oriented, planar bool) (*geog, error) {
	if oriented && planar {
		return nil, fmt.Errorf("oriented and planar cannot both be TRUE")
	}
	conv := func(vs []lngLatPt) ([]s2.Point, error) {
		if planar {
			vs = densifyPlanar(vs)
		}
		out := make([]s2.Point, 0, len(vs))
		for _, v := range vs {
			if err := checkLngLat(v); err != nil {
				return nil, err
			}
			out = append(out, llPoint(v[0], v[1]))
		}
		return out, nil
	}
	g := &geog{}
	for _, p := range ps.points {
		if err := checkLngLat(p); err != nil {
			return nil, err
		}
		g.points = append(g.points, llPoint(p[0], p[1]))
	}
	// GoogleSQL orders a multipoint's points by S2 cell and drops repeats:
	// BigQuery's GoogleSQL reference prints MULTIPOINT(2 11, 4 12, 0 15,
	// 1 9, 1 12) as MULTIPOINT(1 9, 4 12, 2 11, 1 12, 0 15).
	sort.SliceStable(g.points, func(i, j int) bool {
		return s2.CellFromPoint(g.points[i]).ID() < s2.CellFromPoint(g.points[j]).ID()
	})
	g.points = dedup(g.points)
	for _, l := range ps.lines {
		vs, err := conv(l)
		if err != nil {
			return nil, err
		}
		if err := g.addLine(vs); err != nil {
			return nil, err
		}
	}
	var polys [][][]s2.Point
	for _, p := range ps.polys {
		var rings [][]s2.Point
		for _, r := range p {
			vs, err := conv(r)
			if err != nil {
				return nil, err
			}
			rings = append(rings, vs)
		}
		polys = append(polys, rings)
	}
	if len(polys) > 0 {
		if err := g.addPolygons(polys, oriented); err != nil {
			return nil, err
		}
	}
	return g, nil
}

// ---- WKT ----

type wktParser struct {
	toks []string
	pos  int
}

func wktTokens(s string) []string {
	var toks []string
	i := 0
	for i < len(s) {
		c := rune(s[i])
		switch {
		case unicode.IsSpace(c):
			i++
		case c == '(' || c == ')' || c == ',':
			toks = append(toks, string(c))
			i++
		default:
			j := i
			for j < len(s) && !unicode.IsSpace(rune(s[j])) && s[j] != '(' && s[j] != ')' && s[j] != ',' {
				j++
			}
			toks = append(toks, s[i:j])
			i = j
		}
	}
	return toks
}

func (p *wktParser) peek() string {
	if p.pos < len(p.toks) {
		return p.toks[p.pos]
	}
	return ""
}

func (p *wktParser) next() string {
	t := p.peek()
	if p.pos < len(p.toks) {
		p.pos++
	}
	return t
}

func (p *wktParser) expect(t string) error {
	if got := p.next(); got != t {
		if got == "" {
			got = "end of input"
		}
		return fmt.Errorf("WKT: expected %q, found %q", t, got)
	}
	return nil
}

// empty consumes EMPTY.
func (p *wktParser) empty() bool {
	if strings.EqualFold(p.peek(), "EMPTY") {
		p.pos++
		return true
	}
	return false
}

func (p *wktParser) coord() (lngLatPt, error) {
	var nums []float64
	for {
		t := p.peek()
		if t == "" || t == "," || t == ")" || t == "(" {
			break
		}
		f, err := strconv.ParseFloat(t, 64)
		if err != nil {
			return lngLatPt{}, fmt.Errorf("WKT: invalid number %q", t)
		}
		nums = append(nums, f)
		p.pos++
	}
	switch len(nums) {
	case 2:
		return lngLatPt{nums[0], nums[1]}, nil
	case 3, 4:
		return lngLatPt{}, fmt.Errorf("WKT: three-dimensional geometries and geometries with M coordinates are not supported")
	}
	return lngLatPt{}, fmt.Errorf("WKT: a coordinate must have two numbers")
}

func (p *wktParser) coordList() ([]lngLatPt, error) {
	if p.empty() {
		return nil, nil
	}
	if err := p.expect("("); err != nil {
		return nil, err
	}
	var vs []lngLatPt
	for {
		c, err := p.coord()
		if err != nil {
			return nil, err
		}
		vs = append(vs, c)
		if p.peek() != "," {
			break
		}
		p.pos++
	}
	return vs, p.expect(")")
}

func (p *wktParser) polygonBody() ([][]lngLatPt, error) {
	if p.empty() {
		return nil, nil
	}
	if err := p.expect("("); err != nil {
		return nil, err
	}
	var rings [][]lngLatPt
	for {
		r, err := p.coordList()
		if err != nil {
			return nil, err
		}
		if r != nil {
			rings = append(rings, r)
		}
		if p.peek() != "," {
			break
		}
		p.pos++
	}
	return rings, p.expect(")")
}

// list parses "( item, item, ... )" or EMPTY.
func (p *wktParser) list(item func() error) error {
	if p.empty() {
		return nil
	}
	if err := p.expect("("); err != nil {
		return err
	}
	for {
		if err := item(); err != nil {
			return err
		}
		if p.peek() != "," {
			break
		}
		p.pos++
	}
	return p.expect(")")
}

func (p *wktParser) geometry(ps *geoParts) error {
	typ := strings.ToUpper(p.next())
	if m := strings.ToUpper(p.peek()); m == "Z" || m == "M" || m == "ZM" {
		return fmt.Errorf("WKT: three-dimensional geometries and geometries with M coordinates are not supported")
	}
	for _, suf := range []string{"ZM", "Z", "M"} {
		if strings.HasSuffix(typ, suf) && wktType(strings.TrimSuffix(typ, suf)) {
			return fmt.Errorf("WKT: three-dimensional geometries and geometries with M coordinates are not supported")
		}
	}
	switch typ {
	case "POINT":
		if p.empty() {
			return nil
		}
		if err := p.expect("("); err != nil {
			return err
		}
		c, err := p.coord()
		if err != nil {
			return err
		}
		ps.points = append(ps.points, c)
		return p.expect(")")
	case "LINESTRING":
		vs, err := p.coordList()
		if err != nil {
			return err
		}
		if vs != nil {
			ps.lines = append(ps.lines, vs)
		}
		return nil
	case "POLYGON":
		rings, err := p.polygonBody()
		if err != nil {
			return err
		}
		if rings != nil {
			ps.polys = append(ps.polys, rings)
		}
		return nil
	case "MULTIPOINT":
		return p.list(func() error {
			if p.empty() {
				return nil
			}
			paren := p.peek() == "("
			if paren {
				p.pos++
			}
			c, err := p.coord()
			if err != nil {
				return err
			}
			ps.points = append(ps.points, c)
			if paren {
				return p.expect(")")
			}
			return nil
		})
	case "MULTILINESTRING":
		return p.list(func() error {
			vs, err := p.coordList()
			if vs != nil {
				ps.lines = append(ps.lines, vs)
			}
			return err
		})
	case "MULTIPOLYGON":
		return p.list(func() error {
			rings, err := p.polygonBody()
			if rings != nil {
				ps.polys = append(ps.polys, rings)
			}
			return err
		})
	case "GEOMETRYCOLLECTION":
		return p.list(func() error { return p.geometry(ps) })
	case "":
		return fmt.Errorf("WKT: unexpected end of input")
	}
	return fmt.Errorf("WKT: unknown geometry type %q", typ)
}

func wktType(t string) bool {
	switch t {
	case "POINT", "LINESTRING", "POLYGON", "MULTIPOINT", "MULTILINESTRING", "MULTIPOLYGON", "GEOMETRYCOLLECTION":
		return true
	}
	return false
}

// parseWKT reads a WKT geometry (ST_GEOGFROMTEXT).
func parseWKT(s string, oriented, planar bool) (*geog, error) {
	p := &wktParser{toks: wktTokens(s)}
	ps := &geoParts{}
	if err := p.geometry(ps); err != nil {
		return nil, err
	}
	if p.pos != len(p.toks) {
		return nil, fmt.Errorf("WKT: unexpected %q after the geometry", p.peek())
	}
	return ps.build(oriented, planar)
}

func wktCoords(vs []s2.Point, closed bool) string {
	var b strings.Builder
	b.WriteString("(")
	write := func(i int, v s2.Point) {
		if i > 0 {
			b.WriteString(", ")
		}
		lng, lat := lngLat(v)
		b.WriteString(coord(lng) + " " + coord(lat))
	}
	for i, v := range vs {
		write(i, v)
	}
	if closed && len(vs) > 0 {
		write(len(vs), vs[0])
	}
	b.WriteString(")")
	return b.String()
}

func wktPolygonBody(rings [][]s2.Point) string {
	parts := make([]string, len(rings))
	for i, r := range rings {
		parts[i] = wktCoords(r, true)
	}
	return "(" + strings.Join(parts, ", ") + ")"
}

var errFullWKT = fmt.Errorf("the full geography cannot be written in this format")

// wkt writes the geography as ST_ASTEXT does.
func (g *geog) wkt() (string, error) {
	if g.isEmpty() {
		return "GEOMETRYCOLLECTION EMPTY", nil
	}
	if g.poly != nil && g.poly.IsFull() {
		return "", errFullWKT
	}
	polys := g.polygons()
	kinds := 0
	for _, n := range []int{len(g.points), len(g.lines), len(polys)} {
		if n > 0 {
			kinds++
		}
	}
	var elems []string
	for _, p := range g.points {
		elems = append(elems, "POINT"+wktCoords([]s2.Point{p}, false))
	}
	for _, l := range g.lines {
		elems = append(elems, "LINESTRING"+wktCoords(l, false))
	}
	for _, p := range polys {
		elems = append(elems, "POLYGON"+wktPolygonBody(p))
	}
	if kinds > 1 {
		return "GEOMETRYCOLLECTION(" + strings.Join(elems, ", ") + ")", nil
	}
	if len(elems) == 1 {
		return elems[0], nil
	}
	var parts []string
	switch {
	case len(g.points) > 0:
		for _, p := range g.points {
			parts = append(parts, strings.Trim(wktCoords([]s2.Point{p}, false), "()"))
		}
		return "MULTIPOINT(" + strings.Join(parts, ", ") + ")", nil
	case len(g.lines) > 0:
		for _, l := range g.lines {
			parts = append(parts, wktCoords(l, false))
		}
		return "MULTILINESTRING(" + strings.Join(parts, ", ") + ")", nil
	}
	for _, p := range polys {
		parts = append(parts, wktPolygonBody(p))
	}
	return "MULTIPOLYGON(" + strings.Join(parts, ", ") + ")", nil
}

// ---- WKB ----

type wkbReader struct {
	b   []byte
	pos int
}

func (r *wkbReader) byteOrder() (binary.ByteOrder, error) {
	if r.pos >= len(r.b) {
		return nil, fmt.Errorf("WKB: unexpected end of input")
	}
	o := r.b[r.pos]
	r.pos++
	switch o {
	case 0:
		return binary.BigEndian, nil
	case 1:
		return binary.LittleEndian, nil
	}
	return nil, fmt.Errorf("WKB: invalid byte order %d", o)
}

func (r *wkbReader) u32(o binary.ByteOrder) (uint32, error) {
	if r.pos+4 > len(r.b) {
		return 0, fmt.Errorf("WKB: unexpected end of input")
	}
	v := o.Uint32(r.b[r.pos:])
	r.pos += 4
	return v, nil
}

func (r *wkbReader) f64(o binary.ByteOrder) (float64, error) {
	if r.pos+8 > len(r.b) {
		return 0, fmt.Errorf("WKB: unexpected end of input")
	}
	v := math.Float64frombits(o.Uint64(r.b[r.pos:]))
	r.pos += 8
	return v, nil
}

func (r *wkbReader) point(o binary.ByteOrder) (lngLatPt, error) {
	x, err := r.f64(o)
	if err != nil {
		return lngLatPt{}, err
	}
	y, err := r.f64(o)
	return lngLatPt{x, y}, err
}

func (r *wkbReader) points(o binary.ByteOrder) ([]lngLatPt, error) {
	n, err := r.u32(o)
	if err != nil {
		return nil, err
	}
	if int(n) > (len(r.b)-r.pos)/16 {
		return nil, fmt.Errorf("WKB: unexpected end of input")
	}
	vs := make([]lngLatPt, 0, n)
	for i := uint32(0); i < n; i++ {
		v, err := r.point(o)
		if err != nil {
			return nil, err
		}
		vs = append(vs, v)
	}
	return vs, nil
}

func (r *wkbReader) geometry(ps *geoParts, want uint32) error {
	o, err := r.byteOrder()
	if err != nil {
		return err
	}
	typ, err := r.u32(o)
	if err != nil {
		return err
	}
	if typ&0xE0000000 != 0 || typ > 1000 {
		return fmt.Errorf("WKB: three-dimensional geometries and geometries with M coordinates are not supported")
	}
	if want != 0 && typ != want {
		return fmt.Errorf("WKB: a part of type %d where %d is expected", typ, want)
	}
	switch typ {
	case 1:
		p, err := r.point(o)
		if err != nil {
			return err
		}
		if !(math.IsNaN(p[0]) && math.IsNaN(p[1])) { // NaN coordinates are POINT EMPTY
			ps.points = append(ps.points, p)
		}
		return nil
	case 2:
		vs, err := r.points(o)
		if err != nil {
			return err
		}
		if len(vs) > 0 {
			ps.lines = append(ps.lines, vs)
		}
		return nil
	case 3:
		n, err := r.u32(o)
		if err != nil {
			return err
		}
		var rings [][]lngLatPt
		for i := uint32(0); i < n; i++ {
			vs, err := r.points(o)
			if err != nil {
				return err
			}
			if len(vs) > 0 {
				rings = append(rings, vs)
			}
		}
		if len(rings) > 0 {
			ps.polys = append(ps.polys, rings)
		}
		return nil
	case 4, 5, 6, 7:
		n, err := r.u32(o)
		if err != nil {
			return err
		}
		part := map[uint32]uint32{4: 1, 5: 2, 6: 3, 7: 0}[typ]
		for i := uint32(0); i < n; i++ {
			if err := r.geometry(ps, part); err != nil {
				return err
			}
		}
		return nil
	}
	return fmt.Errorf("WKB: unknown geometry type %d", typ)
}

// parseWKB reads a WKB geometry (ST_GEOGFROMWKB).
func parseWKB(b []byte, oriented, planar bool) (*geog, error) {
	r := &wkbReader{b: b}
	ps := &geoParts{}
	if err := r.geometry(ps, 0); err != nil {
		return nil, err
	}
	if r.pos != len(b) {
		return nil, fmt.Errorf("WKB: %d bytes after the geometry", len(b)-r.pos)
	}
	return ps.build(oriented, planar)
}

type wkbWriter struct{ bytes.Buffer }

func (w *wkbWriter) header(typ uint32) {
	w.WriteByte(1)
	_ = binary.Write(w, binary.LittleEndian, typ)
}

func (w *wkbWriter) u32(v int) { _ = binary.Write(w, binary.LittleEndian, uint32(v)) }

func (w *wkbWriter) pt(p s2.Point) {
	lng, lat := lngLat(p)
	_ = binary.Write(w, binary.LittleEndian, round15(lng))
	_ = binary.Write(w, binary.LittleEndian, round15(lat))
}

func (w *wkbWriter) line(vs []s2.Point, closed bool) {
	n := len(vs)
	if closed && n > 0 {
		n++
	}
	w.u32(n)
	for _, v := range vs {
		w.pt(v)
	}
	if closed && len(vs) > 0 {
		w.pt(vs[0])
	}
}

func (w *wkbWriter) polygon(rings [][]s2.Point) {
	w.header(3)
	w.u32(len(rings))
	for _, r := range rings {
		w.line(r, true)
	}
}

// wkb writes the geography as ST_ASBINARY does: little-endian WKB.
func (g *geog) wkb() ([]byte, error) {
	if g.poly != nil && g.poly.IsFull() {
		return nil, errFullWKT
	}
	w := &wkbWriter{}
	polys := g.polygons()
	kinds := 0
	for _, n := range []int{len(g.points), len(g.lines), len(polys)} {
		if n > 0 {
			kinds++
		}
	}
	total := len(g.points) + len(g.lines) + len(polys)
	writeAll := func() {
		for _, p := range g.points {
			w.header(1)
			w.pt(p)
		}
		for _, l := range g.lines {
			w.header(2)
			w.line(l, false)
		}
		for _, p := range polys {
			w.polygon(p)
		}
	}
	switch {
	case total == 0 || kinds > 1:
		w.header(7)
		w.u32(total)
		writeAll()
	case total == 1:
		writeAll()
	case len(g.points) > 0:
		w.header(4)
		w.u32(total)
		writeAll()
	case len(g.lines) > 0:
		w.header(5)
		w.u32(total)
		writeAll()
	default:
		w.header(6)
		w.u32(total)
		writeAll()
	}
	return w.Bytes(), nil
}

// ---- GeoJSON ----

type geoJSONObj struct {
	Type        string            `json:"type"`
	Coordinates json.RawMessage   `json:"coordinates"`
	Geometries  []json.RawMessage `json:"geometries"`
}

func geoJSONPos(raw json.RawMessage) (lngLatPt, error) {
	var nums []float64
	if err := json.Unmarshal(raw, &nums); err != nil {
		return lngLatPt{}, fmt.Errorf("GeoJSON: a position must be an array of numbers")
	}
	if len(nums) != 2 {
		return lngLatPt{}, fmt.Errorf("GeoJSON: a position must have exactly two elements, longitude and latitude")
	}
	return lngLatPt{nums[0], nums[1]}, nil
}

func geoJSONPositions(raw json.RawMessage) ([]lngLatPt, error) {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("GeoJSON: expected an array of positions")
	}
	var vs []lngLatPt
	for _, it := range items {
		v, err := geoJSONPos(it)
		if err != nil {
			return nil, err
		}
		vs = append(vs, v)
	}
	return vs, nil
}

func geoJSONRings(raw json.RawMessage) ([][]lngLatPt, error) {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("GeoJSON: expected an array of rings")
	}
	var rings [][]lngLatPt
	for _, it := range items {
		vs, err := geoJSONPositions(it)
		if err != nil {
			return nil, err
		}
		if len(vs) > 0 {
			rings = append(rings, vs)
		}
	}
	return rings, nil
}

func geoJSONGeometry(raw json.RawMessage, ps *geoParts) error {
	var o geoJSONObj
	if err := json.Unmarshal(raw, &o); err != nil {
		return fmt.Errorf("GeoJSON: invalid JSON: %v", err)
	}
	var items []json.RawMessage
	multi := strings.HasPrefix(o.Type, "Multi")
	if multi {
		if err := json.Unmarshal(o.Coordinates, &items); err != nil {
			return fmt.Errorf("GeoJSON: %s needs an array of coordinates", o.Type)
		}
	}
	switch o.Type {
	case "Point":
		p, err := geoJSONPos(o.Coordinates)
		if err != nil {
			return err
		}
		ps.points = append(ps.points, p)
	case "MultiPoint":
		for _, it := range items {
			p, err := geoJSONPos(it)
			if err != nil {
				return err
			}
			ps.points = append(ps.points, p)
		}
	case "LineString":
		vs, err := geoJSONPositions(o.Coordinates)
		if err != nil {
			return err
		}
		if len(vs) > 0 {
			ps.lines = append(ps.lines, vs)
		}
	case "MultiLineString":
		for _, it := range items {
			vs, err := geoJSONPositions(it)
			if err != nil {
				return err
			}
			if len(vs) > 0 {
				ps.lines = append(ps.lines, vs)
			}
		}
	case "Polygon":
		rings, err := geoJSONRings(o.Coordinates)
		if err != nil {
			return err
		}
		if len(rings) > 0 {
			ps.polys = append(ps.polys, rings)
		}
	case "MultiPolygon":
		for _, it := range items {
			rings, err := geoJSONRings(it)
			if err != nil {
				return err
			}
			if len(rings) > 0 {
				ps.polys = append(ps.polys, rings)
			}
		}
	case "GeometryCollection":
		for _, it := range o.Geometries {
			if err := geoJSONGeometry(it, ps); err != nil {
				return err
			}
		}
	case "Feature", "FeatureCollection":
		return fmt.Errorf("GeoJSON: only geometry objects are accepted, not a %s", o.Type)
	default:
		return fmt.Errorf("GeoJSON: unknown geometry type %q", o.Type)
	}
	return nil
}

// parseGeoJSON reads a GeoJSON geometry (ST_GEOGFROMGEOJSON). GeoJSON's
// edges are planar, so they are densified to stay within 10 m; the
// reference's ST_GEOGFROM example (a polygon of 1- and 2-degree edges near
// the equator) keeps its vertices, which this reproduces, each edge's
// geodesic lying within 10 m of its planar segment.
func parseGeoJSON(s string) (*geog, error) {
	ps := &geoParts{}
	if err := geoJSONGeometry(json.RawMessage(s), ps); err != nil {
		return nil, err
	}
	return ps.build(false, true)
}

// densifyGeodesic adds vertices to a chain of geodesic edges so that the
// planar (lng/lat) edges through its vertices stay within 10 m of them.
func densifyGeodesic(vs []s2.Point) []s2.Point {
	if len(vs) < 2 {
		return vs
	}
	out := []s2.Point{vs[0]}
	var rec func(a, b s2.Point, depth int)
	rec = func(a, b s2.Point, depth int) {
		mg := s2.Point{Vector: a.Add(b.Vector).Normalize()}
		alng, alat := lngLat(a)
		blng, blat := lngLat(b)
		if math.Abs(blng-alng) > 180 {
			// the planar segment wraps the antimeridian.
			if blng < alng {
				blng += 360
			} else {
				alng += 360
			}
		}
		mp := llPoint((alng+blng)/2, (alat+blat)/2)
		if depth < 24 && meters(mg.Distance(mp)) > planarTolerance {
			rec(a, mg, depth+1)
			rec(mg, b, depth+1)
			return
		}
		out = append(out, b)
	}
	for i := 1; i < len(vs); i++ {
		rec(vs[i-1], vs[i], 0)
	}
	return out
}

func geoJSONCoord(p s2.Point) string {
	lng, lat := lngLat(p)
	return "[" + coord(lng) + ", " + coord(lat) + "]"
}

func geoJSONLine(vs []s2.Point) string {
	vs = densifyGeodesic(vs)
	parts := make([]string, len(vs))
	for i, v := range vs {
		parts[i] = geoJSONCoord(v)
	}
	return "[ " + strings.Join(parts, ", ") + " ]"
}

func geoJSONPolygon(rings [][]s2.Point) string {
	parts := make([]string, len(rings))
	for i, r := range rings {
		closed := append(append([]s2.Point(nil), r...), r[0])
		parts[i] = geoJSONLine(closed)
	}
	return "[ " + strings.Join(parts, ", ") + " ]"
}

func geoJSONGeom(typ, coords string) string {
	return `{ "type": "` + typ + `", "coordinates": ` + coords + ` }`
}

// geoJSON writes the geography as ST_ASGEOJSON does.
func (g *geog) geoJSON() (string, error) {
	if g.poly != nil && g.poly.IsFull() {
		return "", errFullWKT
	}
	polys := g.polygons()
	kinds := 0
	for _, n := range []int{len(g.points), len(g.lines), len(polys)} {
		if n > 0 {
			kinds++
		}
	}
	var elems []string
	for _, p := range g.points {
		elems = append(elems, geoJSONGeom("Point", geoJSONCoord(p)))
	}
	for _, l := range g.lines {
		elems = append(elems, geoJSONGeom("LineString", geoJSONLine(l)))
	}
	for _, p := range polys {
		elems = append(elems, geoJSONGeom("Polygon", geoJSONPolygon(p)))
	}
	if len(elems) == 0 || kinds > 1 {
		if len(elems) == 0 {
			return `{ "type": "GeometryCollection", "geometries": [  ] }`, nil
		}
		return `{ "type": "GeometryCollection", "geometries": [ ` + strings.Join(elems, ", ") + ` ] }`, nil
	}
	if len(elems) == 1 {
		return elems[0], nil
	}
	var parts []string
	switch {
	case len(g.points) > 0:
		for _, p := range g.points {
			parts = append(parts, geoJSONCoord(p))
		}
		return geoJSONGeom("MultiPoint", "[ "+strings.Join(parts, ", ")+" ]"), nil
	case len(g.lines) > 0:
		for _, l := range g.lines {
			parts = append(parts, geoJSONLine(l))
		}
		return geoJSONGeom("MultiLineString", "[ "+strings.Join(parts, ", ")+" ]"), nil
	}
	for _, p := range polys {
		parts = append(parts, geoJSONPolygon(p))
	}
	return geoJSONGeom("MultiPolygon", "[ "+strings.Join(parts, ", ")+" ]"), nil
}

// ---- KML ----

type kmlNode struct {
	XMLName xml.Name
	Nodes   []kmlNode `xml:",any"`
	Text    string    `xml:",chardata"`
}

func (n *kmlNode) child(name string) *kmlNode {
	for i := range n.Nodes {
		if n.Nodes[i].XMLName.Local == name {
			return &n.Nodes[i]
		}
	}
	return nil
}

func kmlCoords(n *kmlNode) ([]lngLatPt, error) {
	c := n.child("coordinates")
	if c == nil {
		return nil, fmt.Errorf("KML: <%s> has no <coordinates>", n.XMLName.Local)
	}
	var vs []lngLatPt
	for _, f := range strings.Fields(c.Text) {
		parts := strings.Split(f, ",")
		if len(parts) < 2 || len(parts) > 3 {
			return nil, fmt.Errorf("KML: invalid coordinate %q", f)
		}
		lng, err1 := strconv.ParseFloat(parts[0], 64)
		lat, err2 := strconv.ParseFloat(parts[1], 64)
		if err1 != nil || err2 != nil {
			return nil, fmt.Errorf("KML: invalid coordinate %q", f)
		}
		vs = append(vs, lngLatPt{lng, lat})
	}
	return vs, nil
}

func kmlGeometry(n *kmlNode, ps *geoParts) error {
	switch n.XMLName.Local {
	case "Point":
		vs, err := kmlCoords(n)
		if err != nil {
			return err
		}
		if len(vs) != 1 {
			return fmt.Errorf("KML: a <Point> has exactly one coordinate")
		}
		ps.points = append(ps.points, vs[0])
	case "LineString":
		vs, err := kmlCoords(n)
		if err != nil {
			return err
		}
		if len(vs) > 0 {
			ps.lines = append(ps.lines, vs)
		}
	case "Polygon":
		var rings [][]lngLatPt
		for _, b := range []string{"outerBoundaryIs", "innerBoundaryIs"} {
			for i := range n.Nodes {
				if n.Nodes[i].XMLName.Local != b {
					continue
				}
				lr := n.Nodes[i].child("LinearRing")
				if lr == nil {
					return fmt.Errorf("KML: <%s> has no <LinearRing>", b)
				}
				vs, err := kmlCoords(lr)
				if err != nil {
					return err
				}
				rings = append(rings, vs)
			}
			if b == "outerBoundaryIs" && len(rings) != 1 {
				return fmt.Errorf("KML: a <Polygon> has exactly one <outerBoundaryIs>")
			}
		}
		ps.polys = append(ps.polys, rings)
	case "MultiGeometry":
		for i := range n.Nodes {
			if err := kmlGeometry(&n.Nodes[i], ps); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("KML: unsupported element <%s>", n.XMLName.Local)
	}
	return nil
}

// parseKML reads a KML geometry (ST_GEOGFROMKML): Point, LineString and
// Polygon with their coordinates and boundaries only, and MultiGeometry.
func parseKML(s string) (*geog, error) {
	var n kmlNode
	if err := xml.Unmarshal([]byte(s), &n); err != nil {
		return nil, fmt.Errorf("KML: %v", err)
	}
	ps := &geoParts{}
	if err := kmlGeometry(&n, ps); err != nil {
		return nil, err
	}
	return ps.build(false, false)
}

func kmlCoord(p s2.Point) string {
	lng, lat := lngLat(p)
	return strconv.FormatFloat(round15(lng), 'g', -1, 64) + "," + strconv.FormatFloat(round15(lat), 'g', -1, 64)
}

func kmlCoordList(vs []s2.Point, closed bool) string {
	parts := make([]string, 0, len(vs)+1)
	for _, v := range vs {
		parts = append(parts, kmlCoord(v))
	}
	if closed && len(vs) > 0 {
		parts = append(parts, kmlCoord(vs[0]))
	}
	return "<coordinates>" + strings.Join(parts, " ") + "</coordinates>"
}

// kml writes the geography as ST_ASKML does.
func (g *geog) kml() (string, error) {
	if g.poly != nil && g.poly.IsFull() {
		return "", errFullWKT
	}
	var elems []string
	for _, p := range g.points {
		elems = append(elems, "<Point>"+kmlCoordList([]s2.Point{p}, false)+"</Point>")
	}
	for _, l := range g.lines {
		elems = append(elems, "<LineString>"+kmlCoordList(l, false)+"</LineString>")
	}
	for _, rings := range g.polygons() {
		var b strings.Builder
		b.WriteString("<Polygon>")
		for i, r := range rings {
			tag := "innerBoundaryIs"
			if i == 0 {
				tag = "outerBoundaryIs"
			}
			b.WriteString("<" + tag + "><LinearRing>" + kmlCoordList(r, true) + "</LinearRing></" + tag + ">")
		}
		b.WriteString("</Polygon>")
		elems = append(elems, b.String())
	}
	if len(elems) == 1 {
		return elems[0], nil
	}
	return "<MultiGeometry>" + strings.Join(elems, "") + "</MultiGeometry>", nil
}

// ---- GeoHash ----

const geohashAlphabet = "0123456789bcdefghjkmnpqrstuvwxyz"

// geohashEncode is ST_GEOHASH: maxchars outside 1 to 20 means 20.
func geohashEncode(p s2.Point, maxchars int) string {
	if maxchars < 1 || maxchars > 20 {
		maxchars = 20
	}
	lng, lat := lngLat(p)
	lng, lat = round15(lng), round15(lat)
	lngR, latR := [2]float64{-180, 180}, [2]float64{-90, 90}
	var b strings.Builder
	even := true
	bit, ch := 0, 0
	for b.Len() < maxchars {
		r, v := &latR, lat
		if even {
			r, v = &lngR, lng
		}
		mid := (r[0] + r[1]) / 2
		ch <<= 1
		if v >= mid {
			ch |= 1
			r[0] = mid
		} else {
			r[1] = mid
		}
		even = !even
		if bit++; bit == 5 {
			b.WriteByte(geohashAlphabet[ch])
			bit, ch = 0, 0
		}
	}
	return b.String()
}

// geohashDecode is ST_GEOGPOINTFROMGEOHASH: the middle of the GeoHash's box.
func geohashDecode(h string) (s2.Point, error) {
	if h == "" {
		return s2.Point{}, fmt.Errorf("Invalid GeoHash: empty")
	}
	lngR, latR := [2]float64{-180, 180}, [2]float64{-90, 90}
	even := true
	for _, c := range strings.ToLower(h) {
		v := strings.IndexRune(geohashAlphabet, c)
		if v < 0 {
			return s2.Point{}, fmt.Errorf("Invalid GeoHash character %q in %q", c, h)
		}
		for i := 4; i >= 0; i-- {
			r := &latR
			if even {
				r = &lngR
			}
			mid := (r[0] + r[1]) / 2
			if v>>uint(i)&1 == 1 {
				r[0] = mid
			} else {
				r[1] = mid
			}
			even = !even
		}
	}
	return llPoint((lngR[0]+lngR[1])/2, (latR[0]+latR[1])/2), nil
}
