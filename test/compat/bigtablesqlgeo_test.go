//go:build compat

package compat

import (
	"encoding/hex"
	"fmt"
	"strings"
	"testing"

	"cloud.google.com/go/bigtable"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestBigtableSQLGeography (#1720) runs GoogleSQL for Bigtable's geography
// functions through the official client's PrepareStatement, Bind and
// Execute, against the reference's examples
// (https://cloud.google.com/bigtable/docs/reference/sql/geography_functions)
// and GoogleSQL's BigQuery reference for the measures and S2 cell IDs: a
// table's cells parsed into GEOGRAPHY values, WKT, GeoJSON and WKB out,
// predicates, distances and areas on S2's Earth sphere, S2 cells, bound
// FLOAT64 parameters, the point-set operations, buffers and ST_SIMPLIFY
// (#1728: ST_UNION, ST_UNION_AGG over rows, ST_INTERSECTION, ST_DIFFERENCE,
// ST_BUFFER, ST_BUFFERWITHTOLERANCE, ST_SIMPLIFY, to the references'
// printed results; #1735: touching and crossing shapes as S2's
// S2BooleanOperation gives them), and the refusals: a GEOGRAPHY result column, which
// Bigtable's Type cannot carry (INVALID_ARGUMENT), and ST_REGIONSTATS
// (UNIMPLEMENTED).
//
// covers: google.bigtable.v2.Bigtable/PrepareQuery, google.bigtable.v2.Bigtable/ExecuteQuery
func TestBigtableSQLGeography(t *testing.T) {
	h := New(t)
	iac := bigtableInstanceAdmin(t, h) // asks for the Bigtable endpoint first
	ctx := h.Context()
	const id = "cb-inst-sqlgeo"
	if err := iac.CreateInstance(ctx, &bigtable.InstanceConf{InstanceId: id, DisplayName: "SQL geography",
		ClusterId: id + "-c1", Zone: "us-central1-b", NumNodes: 1, StorageType: bigtable.SSD}); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	t.Cleanup(func() { _ = iac.DeleteInstance(h.Context(), id) })
	ac, err := bigtable.NewAdminClient(ctx, h.Project(), id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ac.Close() })
	c, err := bigtable.NewClient(ctx, h.Project(), id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })

	// places: each row a WKT shape in loc:wkt.
	if err := ac.CreateTable(ctx, "places"); err != nil {
		t.Fatal(err)
	}
	if err := ac.CreateColumnFamily(ctx, "places", "loc"); err != nil {
		t.Fatal(err)
	}
	for key, wkt := range map[string]string{
		"a-origin":   "POINT(0 0)",
		"b-seattle":  "POINT(-122.35 47.62)",
		"c-triangle": "POLYGON((1 1, 20 1, 10 20, 1 1))",
		"d-line":     "LINESTRING(1 1, 5 5)",
		"e-path1":    "LINESTRING(-122.12 47.67, -122.19 47.69)",
		"e-path2":    "LINESTRING(-122.12 47.67, -100.19 47.69)",
		"e-path3":    "LINESTRING(-122.12 47.67, -122.19 47.69)",
	} {
		m := bigtable.NewMutation()
		m.Set("loc", "wkt", bigtable.Now(), []byte(wkt))
		if err := c.Open("places").Apply(ctx, key, m); err != nil {
			t.Fatal(err)
		}
	}

	run := func(sql string, params map[string]bigtable.SQLType, bind map[string]any) string {
		t.Helper()
		ps, err := c.PrepareStatement(ctx, sql, params)
		if err != nil {
			t.Fatalf("PrepareStatement(%s): %v", sql, err)
		}
		bs, err := ps.Bind(bind)
		if err != nil {
			t.Fatalf("Bind: %v", err)
		}
		var rows []string
		if err := bs.Execute(ctx, func(r bigtable.ResultRow) bool {
			var cols []string
			for i := range r.Metadata.Columns {
				var v any
				if err := r.GetByIndex(i, &v); err != nil {
					t.Fatalf("%s: GetByIndex(%d): %v", sql, i, err)
				}
				switch x := v.(type) {
				case []byte:
					if r.Metadata.Columns[i].Name == "wkb" {
						cols = append(cols, hex.EncodeToString(x))
					} else {
						cols = append(cols, string(x))
					}
				case *string:
					cols = append(cols, deref(x))
				default:
					cols = append(cols, sqlShow1704(v))
				}
			}
			rows = append(rows, strings.Join(cols, " "))
			return true
		}); err != nil {
			t.Fatalf("Execute(%s): %v", sql, err)
		}
		return strings.Join(rows, "\n")
	}
	expect := func(sql, want string) {
		t.Helper()
		if got := run(sql, nil, nil); got != want {
			t.Errorf("%s =\n%s\nwant\n%s", sql, got, want)
		}
	}

	g := "ST_GEOGFROMTEXT(CAST(loc['wkt'] AS STRING))"
	expect("SELECT _key, ST_ASTEXT("+g+") AS wkt, ST_GEOMETRYTYPE("+g+") AS kind FROM places WHERE _key < 'e'",
		"a-origin POINT(0 0) ST_Point\nb-seattle POINT(-122.35 47.62) ST_Point\nc-triangle POLYGON((1 1, 20 1, 10 20, 1 1)) ST_Polygon\nd-line LINESTRING(1 1, 5 5) ST_LineString")
	// ST_CONTAINS and ST_COVERS of the reference's three points.
	expect("SELECT ST_CONTAINS("+g+", ST_GEOGPOINT(0, 0)) AS c0, ST_CONTAINS("+g+", ST_GEOGPOINT(1, 1)) AS c1, ST_CONTAINS("+g+", ST_GEOGPOINT(10, 10)) AS c10, ST_COVERS("+g+", ST_GEOGPOINT(1, 1)) AS v1 FROM places WHERE _key = 'c-triangle'",
		"false false true true")
	expect("SELECT ST_GEOHASH("+g+", 10) AS h FROM places WHERE _key = 'b-seattle'", "c22yzugqw7")
	expect("SELECT ST_ASTEXT(ST_GEOGFROM('POLYGON((0 0, 0 2, 2 2, 2 0, 0 0))')) AS p, ST_ASTEXT(ST_GEOGFROM(FROM_HEX('010100000000000000000000400000000000001040'))) AS q",
		"POLYGON((2 0, 2 2, 0 2, 0 0, 2 0)) POINT(2 4)")
	expect("SELECT ST_ASBINARY(ST_GEOGPOINT(2, 4)) AS wkb, ST_ASGEOJSON(ST_GEOGPOINT(2, 4)) AS j", `010100000000000000000000400000000000001040 { "type": "Point", "coordinates": [2, 4] }`)
	// Measures on S2's Earth sphere, and GoogleSQL's documented
	// ST_HAUSDORFFDISTANCE (BigQuery's reference).
	expect("SELECT ST_HAUSDORFFDISTANCE(ST_GEOGFROMTEXT('LINESTRING(20 90, 30 90, 60 10, 90 10)'), ST_GEOGFROMTEXT('LINESTRING(20 70, 70 60, 10 70, 70 70)'), directed => TRUE) AS d",
		"5.802892745488612e+06")
	expect("SELECT _key, ST_DWITHIN("+g+", ST_GEOGPOINT(0, 0), 200000) AS near FROM places WHERE _key < 'e'",
		"a-origin true\nb-seattle false\nc-triangle true\nd-line true")
	if got := run("SELECT ROUND(ST_DISTANCE("+g+", ST_GEOGPOINT(@lng, @lat)), 3) AS m FROM places WHERE _key = 'a-origin'",
		map[string]bigtable.SQLType{"lng": bigtable.Float64SQLType{}, "lat": bigtable.Float64SQLType{}},
		map[string]any{"lng": 1.0, "lat": 0.0}); got != "111195.101" {
		t.Errorf("ST_DISTANCE with parameters = %s, want 111195.101", got)
	}
	expect("SELECT ST_ASTEXT(ST_LINEINTERPOLATEPOINT("+g+", 0.5)) AS mid FROM places WHERE _key = 'd-line'", "POINT(2.99633827268976 3.00182528336078)")
	// S2 cells (BigQuery's reference's values).
	expect("SELECT S2_CELLIDFROMPOINT(ST_GEOGPOINT(-122, 47)) AS c30, S2_CELLIDFROMPOINT(ST_GEOGPOINT(-122, 47), level => 10) AS c10",
		"6093613931972369317 6093613287902019584")
	expect("SELECT S2_COVERINGCELLIDS(ST_GEOGFROMTEXT('LINESTRING(-122.12 47.67, -122.19 47.69)'), min_level => 12) AS cells",
		"[6093384954555662336,6093390709811838976,6093390735581642752,6093390740145045504,6093390791416217600,6093390812891054080,6093390817187069952,6093496378892222464]")

	// #1728: the references' examples of the point-set operations, buffers
	// and ST_SIMPLIFY (BigQuery's reference for ST_SIMPLIFY), and
	// ST_UNION_AGG over the table's rows.
	l1, l2 := "ST_GEOGFROMTEXT('LINESTRING(-122.12 47.67, -122.19 47.69)')", "ST_GEOGFROMTEXT('LINESTRING(-122.12 47.67, -100.19 47.69)')"
	expect("SELECT ST_ASTEXT(ST_UNION("+l1+", "+l2+")) AS u", "LINESTRING(-100.19 47.69, -122.12 47.67, -122.19 47.69)")
	expect("SELECT ST_ASTEXT(ST_UNION_AGG("+g+")) AS u FROM places WHERE _key >= 'e-'", "LINESTRING(-100.19 47.69, -122.12 47.67, -122.19 47.69)")
	big, small := "ST_GEOGFROMTEXT('POLYGON((0 0, 10 0, 10 10, 0 0))')", "ST_GEOGFROMTEXT('POLYGON((4 2, 6 2, 8 6, 4 2))')"
	expect("SELECT ST_ASTEXT(ST_DIFFERENCE("+big+", "+small+")) AS d, ST_ASTEXT(ST_INTERSECTION("+big+", "+small+")) AS i",
		"POLYGON((0 0, 10 0, 10 10, 0 0), (8 6, 6 2, 4 2, 8 6)) POLYGON((4 2, 6 2, 8 6, 4 2))")
	expect("SELECT ST_NUMPOINTS(ST_BUFFER(ST_GEOGFROMTEXT('POINT(1 2)'), 50, 2)) AS eight_sides, ST_NUMPOINTS(ST_BUFFER(ST_GEOGFROMTEXT('POINT(100 2)'), 50)) AS thirty_two_sides", "9 33")
	expect("SELECT ST_NUMPOINTS(ST_BUFFERWITHTOLERANCE(ST_GEOGFROMTEXT('POINT(1 2)'), 100, 25)) AS five_sides, ST_NUMPOINTS(ST_BUFFERWITHTOLERANCE(ST_GEOGFROMTEXT('POINT(100 2)'), 100, 1)) AS twenty_four_sides", "6 24")
	expect("SELECT ST_ASTEXT(ST_SIMPLIFY(ST_GEOGFROMTEXT('LINESTRING(0 0, 0.05 0, 0.1 0, 0.15 0, 2 0)'), 1)) AS s", "LINESTRING(0 0, 2 0)")
	tri := "ST_GEOGFROMTEXT('POLYGON((0 0, 0.1 0, 0.1 0.1, 0 0))')"
	expect("SELECT ST_ASTEXT(ST_SIMPLIFY("+tri+", 1000)) AS a, ST_ASTEXT(ST_SIMPLIFY("+tri+", 10000)) AS b, ST_ASTEXT(ST_SIMPLIFY("+tri+", 100000)) AS c",
		"POLYGON((0 0, 0.1 0, 0.1 0.1, 0 0)) LINESTRING(0 0, 0.1 0.1) POINT(0 0)")
	// #1735: S2BooleanOperation's closed models where shapes touch or
	// cross, which the reference prints no result for: touching squares
	// intersect in their shared edge or corner, crossing linestrings are
	// split at the crossing, MULTIPOINT members come in S2Builder's order,
	// and ST_SIMPLIFY snaps a line through a neighbouring point rather than
	// across it.
	sq0, sqR, sqC := "ST_GEOGFROMTEXT('POLYGON((0 0, 1 0, 1 1, 0 1, 0 0))')", "ST_GEOGFROMTEXT('POLYGON((1 0, 2 0, 2 1, 1 1, 1 0))')", "ST_GEOGFROMTEXT('POLYGON((1 1, 2 1, 2 2, 1 2, 1 1))')"
	expect("SELECT ST_ASTEXT(ST_INTERSECTION("+sq0+", "+sqR+")) AS edge, ST_ASTEXT(ST_INTERSECTION("+sq0+", "+sqC+")) AS corner, ST_ASTEXT(ST_UNION("+sq0+", "+sqR+")) AS u",
		"LINESTRING(1 0, 1 1) POINT(1 1) POLYGON((1 1, 0 1, 0 0, 1 0, 2 0, 2 1, 1 1))")
	x1, x2 := "ST_GEOGFROMTEXT('LINESTRING(0 0, 2 2)')", "ST_GEOGFROMTEXT('LINESTRING(0 2, 2 0)')"
	expect("SELECT ST_ASTEXT(ST_UNION("+x1+", "+x2+")) AS u, ST_ASTEXT(ST_INTERSECTION("+x1+", "+x2+")) AS i",
		"MULTILINESTRING((0 0, 1 1.00045705372412), (1 1.00045705372412, 2 2), (1 1.00045705372412, 2 0), (0 2, 1 1.00045705372412)) POINT(1 1.00045705372412)")
	expect("SELECT ST_ASTEXT(ST_UNION(ST_GEOGFROMTEXT('MULTIPOINT(30 3, -100 1, 5 60)'), ST_GEOGPOINT(2, -2))) AS mp", "MULTIPOINT(30 3, 2 -2, 5 60, -100 1)")
	expect("SELECT ST_ASTEXT(ST_SIMPLIFY(ST_GEOGFROMTEXT('GEOMETRYCOLLECTION(POINT(0.25 0.002), LINESTRING(0 0, 0.5 0.03, 1 0))'), 4000)) AS s", "LINESTRING(0 0, 0.25 0.002, 1 0)")

	for sql, code := range map[string]codes.Code{
		"SELECT ST_GEOGPOINT(1, 2) AS g":                      codes.InvalidArgument,
		"SELECT ST_REGIONSTATS(ST_GEOGPOINT(1, 2), 'x') AS r": codes.Unimplemented,
		"SELECT ST_CLUSTERDBSCAN(_key, 1, 1) FROM places":     codes.Unimplemented,
	} {
		ps, err := c.PrepareStatement(ctx, sql, nil)
		if err == nil {
			var bs *bigtable.BoundStatement
			if bs, err = ps.Bind(nil); err == nil {
				err = bs.Execute(ctx, func(bigtable.ResultRow) bool { return true })
			}
		}
		if status.Code(err) != code {
			t.Errorf("%s: %v, want %v", sql, err, code)
		}
	}
}

func deref(s *string) string {
	if s == nil {
		return "NULL"
	}
	return fmt.Sprint(*s)
}
