package bigtablefront

import (
	"encoding/hex"
	"strings"
	"testing"
)

func mustWKT(t *testing.T, g *geog, err error) string {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	s, err := g.wkt()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// The reference's ST_GEOGFROM examples (geography_functions, re-read
// 2026-11-17).
func TestGeoReferenceParsers(t *testing.T) {
	g, err := parseWKT("POLYGON((0 0, 0 2, 2 2, 2 0, 0 0))", false, false)
	if got, want := mustWKT(t, g, err), "POLYGON((2 0, 2 2, 0 2, 0 0, 2 0))"; got != want {
		t.Errorf("WKT: %s, want %s", got, want)
	}
	b, _ := hex.DecodeString("010100000000000000000000400000000000001040")
	g, err = parseWKB(b, false, false)
	if got, want := mustWKT(t, g, err), "POINT(2 4)"; got != want {
		t.Errorf("WKB: %s, want %s", got, want)
	}
	g, err = parseGeoJSON(`{ "type": "Polygon", "coordinates": [ [ [2, 0], [2, 2], [1, 2], [0, 2], [0, 0], [2, 0] ] ] }`)
	if got, want := mustWKT(t, g, err), "POLYGON((2 0, 2 2, 1 2, 0 2, 0 0, 2 0))"; got != want {
		t.Errorf("GeoJSON: %s, want %s", got, want)
	}
}

func TestGeoHashReference(t *testing.T) {
	if got := geohashEncode(llPoint(-122.35, 47.62), 10); got != "c22yzugqw7" {
		t.Errorf("ST_GEOHASH = %s, want c22yzugqw7", got)
	}
	if got := geohashEncode(llPoint(-122.35, 47.62), 0); len(got) != 20 {
		t.Errorf("default maxchars: %s", got)
	}
	p, err := geohashDecode("c22yzugqw7")
	if err != nil {
		t.Fatal(err)
	}
	if got := geohashEncode(p, 10); got != "c22yzugqw7" {
		t.Errorf("decode/encode = %s", got)
	}
	if _, err := geohashDecode("c22a"); err == nil {
		t.Error("invalid character accepted")
	}
}

func TestGeoWKTRoundTrips(t *testing.T) {
	for _, s := range []string{
		"POINT(1 2)",
		"LINESTRING(1 1, 2 1, 3 2, 3 3)",
		"MULTIPOINT(1 2, 3 4)",
		"MULTILINESTRING((1 2, 3 4), (5 6, 7 8))",
		"POLYGON((0 0, 10 0, 10 10, 0 0), (8 6, 6 2, 4 2, 8 6))",
		"MULTIPOLYGON(((0 0, 1 0, 1 1, 0 0)), ((5 5, 6 5, 6 6, 5 5)))",
		"GEOMETRYCOLLECTION(POINT(1 2), LINESTRING(3 4, 5 6))",
		"GEOMETRYCOLLECTION EMPTY",
	} {
		g, err := parseWKT(s, false, false)
		if got := mustWKT(t, g, err); got != s {
			t.Errorf("WKT %s -> %s", s, got)
			continue
		}
		b, err := g.wkb()
		if err != nil {
			t.Fatal(err)
		}
		g2, err := parseWKB(b, false, false)
		if got := mustWKT(t, g2, err); got != s {
			t.Errorf("WKB %s -> %s", s, got)
		}
		j, err := g.geoJSON()
		if err != nil {
			t.Fatal(err)
		}
		// GeoJSON's planar edges gain vertices off meridians and the
		// equator; these shapes have none to gain.
		if strings.Contains("POINT(1 2) MULTIPOINT(1 2, 3 4) GEOMETRYCOLLECTION EMPTY", s) {
			g3, err := parseGeoJSON(j)
			if got := mustWKT(t, g3, err); got != s {
				t.Errorf("GeoJSON %s -> %s -> %s", s, j, got)
			}
		}
		k, err := g.kml()
		if err != nil {
			t.Fatal(err)
		}
		g4, err := parseKML(k)
		if got := mustWKT(t, g4, err); got != s && s != "GEOMETRYCOLLECTION EMPTY" {
			t.Errorf("KML %s -> %s -> %s", s, k, got)
		}
	}
	for _, s := range []string{"MULTIPOINT((1 2), (3 4))", "multipoint ((1 2),(3 4))"} {
		g, err := parseWKT(s, false, false)
		if got := mustWKT(t, g, err); got != "MULTIPOINT(1 2, 3 4)" {
			t.Errorf("%s -> %s", s, got)
		}
	}
	for _, s := range []string{"POINT EMPTY", "LINESTRING EMPTY", "POLYGON EMPTY", "MULTIPOINT EMPTY", "GEOMETRYCOLLECTION(POINT EMPTY)"} {
		g, err := parseWKT(s, false, false)
		if got := mustWKT(t, g, err); got != "GEOMETRYCOLLECTION EMPTY" {
			t.Errorf("%s -> %s", s, got)
		}
	}
}

func TestGeoFormatsRefuse(t *testing.T) {
	for _, s := range []string{"POINT Z (1 2 3)", "POINTZ(1 2 3)", "POINT(1 2 3)", "POINT M (1 2 3)", "POINT(1 91)", "CIRCLE(1 2)", "POINT(1 2) x"} {
		if _, err := parseWKT(s, false, false); err == nil {
			t.Errorf("%s accepted", s)
		}
	}
	if _, err := parseWKT("POINT(1 2)", true, true); err == nil {
		t.Error("oriented and planar accepted")
	}
	for _, s := range []string{`{"type": "Feature", "geometry": null}`, `{"type": "Point", "coordinates": [1, 2, 3]}`} {
		if _, err := parseGeoJSON(s); err == nil {
			t.Errorf("%s accepted", s)
		}
	}
	b, _ := hex.DecodeString("01e9030000000000000000f03f00000000000000400000000000000840") // POINT Z
	if _, err := parseWKB(b, false, false); err == nil {
		t.Error("WKB POINT Z accepted")
	}
}

func TestGeoFormatOutputs(t *testing.T) {
	g, _ := parseWKT("GEOMETRYCOLLECTION(POINT(1 2), LINESTRING(3 4, 3 5))", false, false)
	j, _ := g.geoJSON()
	if want := `{ "type": "GeometryCollection", "geometries": [ { "type": "Point", "coordinates": [1, 2] }, { "type": "LineString", "coordinates": [ [3, 4], [3, 5] ] } ] }`; j != want {
		t.Errorf("GeoJSON %s", j)
	}
	k, _ := g.kml()
	if want := "<MultiGeometry><Point><coordinates>1,2</coordinates></Point><LineString><coordinates>3,4 3,5</coordinates></LineString></MultiGeometry>"; k != want {
		t.Errorf("KML %s", k)
	}
	b, _ := pointGeog(llPoint(2, 4)).wkb()
	if got := hex.EncodeToString(b); got != "010100000000000000000000400000000000001040" {
		t.Errorf("WKB %s", got)
	}
	// a long edge off the equator gains vertices in GeoJSON.
	g, _ = parseWKT("LINESTRING(0 40, 40 40)", false, false)
	j, _ = g.geoJSON()
	if strings.Count(j, "[") < 5 {
		t.Errorf("not densified: %s", j)
	}
	// planar WKT input: densified, so it has more vertices than the input.
	g, _ = parseWKT("LINESTRING(0 40, 40 40)", false, true)
	if len(g.lines[0]) <= 2 {
		t.Errorf("planar not densified: %d", len(g.lines[0]))
	}
	kg, err := parseKML("<Polygon><outerBoundaryIs><LinearRing><coordinates>0,0 10,0 10,10 0,0</coordinates></LinearRing></outerBoundaryIs><innerBoundaryIs><LinearRing><coordinates>4,2 6,2 8,6 4,2</coordinates></LinearRing></innerBoundaryIs></Polygon>")
	if got := mustWKT(t, kg, err); got != "POLYGON((0 0, 10 0, 10 10, 0 0), (8 6, 6 2, 4 2, 8 6))" {
		t.Errorf("KML polygon %s", got)
	}
}

// GoogleSQL orders a multipoint's points by S2 cell: BigQuery's GoogleSQL
// reference prints ST_GEOGFROMTEXT('MULTIPOINT(2 11, 4 12, 0 15, 1 9, 1 12)')
// as MULTIPOINT(1 9, 4 12, 2 11, 1 12, 0 15) (its ST_CONVEXHULL example).
func TestGeoMultipointOrder(t *testing.T) {
	g, err := parseWKT("MULTIPOINT(2 11, 4 12, 0 15, 1 9, 1 12)", false, false)
	if got, want := mustWKT(t, g, err), "MULTIPOINT(1 9, 4 12, 2 11, 1 12, 0 15)"; got != want {
		t.Errorf("%s, want %s", got, want)
	}
}

// The polygon of 1- and 2-degree edges of the reference's ST_GEOGFROM
// example stays within 10 m of its planar edges, so it keeps its vertices.
func TestGeoJSONDensifyTolerance(t *testing.T) {
	in := []lngLatPt{{2, 0}, {2, 2}, {1, 2}, {0, 2}, {0, 0}, {2, 0}}
	if got := densifyPlanar(in); len(got) != len(in) {
		t.Errorf("densified to %v", got)
	}
}
