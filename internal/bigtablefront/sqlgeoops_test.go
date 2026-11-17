package bigtablefront

import (
	"strings"
	"testing"
)

// TestSQLGeographyOperations runs the reference examples of ST_UNION,
// ST_UNION_AGG, ST_DIFFERENCE, ST_BUFFER and ST_BUFFERWITHTOLERANCE
// (https://cloud.google.com/bigtable/docs/reference/sql/geography_functions,
// read 2026-11-17) and of ST_SIMPLIFY, which that page gives none of, from
// GoogleSQL's BigQuery reference
// (https://cloud.google.com/bigquery/docs/reference/standard-sql/geography_functions),
// printed as the references print them. ST_INTERSECTION has no example in
// either; its cases check the identities with ST_DIFFERENCE and ST_UNION.
// Rows the references read FROM UNNEST run one expression each.
func TestSQLGeographyOperations(t *testing.T) {
	run, prepare := sqlFixture(t)
	l1 := "ST_GEOGFROMTEXT('LINESTRING(-122.12 47.67, -122.19 47.69)')"
	l2 := "ST_GEOGFROMTEXT('LINESTRING(-122.12 47.67, -100.19 47.69)')"
	tri := "ST_GEOGFROMTEXT('POLYGON((0 0, 0.1 0, 0.1 0.1, 0 0))')"
	big := "ST_GEOGFROMTEXT('POLYGON((0 0, 10 0, 10 10, 0 0))')"
	small := "ST_GEOGFROMTEXT('POLYGON((4 2, 6 2, 8 6, 4 2))')"
	pairs := [][2]string{
		// ST_UNION and ST_UNION_AGG.
		{"SELECT ST_ASTEXT(ST_UNION(" + l1 + ", " + l2 + "))", "LINESTRING(-100.19 47.69, -122.12 47.67, -122.19 47.69)"},
		{"SELECT ST_ASTEXT(ST_UNION([" + l1 + ", " + l2 + ", " + l1 + "]))", "LINESTRING(-100.19 47.69, -122.12 47.67, -122.19 47.69)"},
		{"SELECT ST_ASTEXT(ST_UNION([" + l1 + ", NULL, " + l2 + "])), ST_UNION(" + l1 + ", NULL) IS NULL", "LINESTRING(-100.19 47.69, -122.12 47.67, -122.19 47.69) true"},
		// ST_DIFFERENCE.
		{"SELECT ST_ASTEXT(ST_DIFFERENCE(" + big + ", " + small + "))", "POLYGON((0 0, 10 0, 10 10, 0 0), (8 6, 6 2, 4 2, 8 6))"},
		{"SELECT ST_ASTEXT(ST_DIFFERENCE(" + small + ", " + big + "))", "GEOMETRYCOLLECTION EMPTY"},
		// ST_INTERSECTION: the smaller polygon inside the larger, and
		// disjoint inputs (an empty GEOGRAPHY).
		{"SELECT ST_ASTEXT(ST_INTERSECTION(" + big + ", " + small + ")), ST_ASTEXT(ST_INTERSECTION(ST_GEOGPOINT(1, 1), ST_GEOGPOINT(2, 2)))", "POLYGON((4 2, 6 2, 8 6, 4 2)) GEOMETRYCOLLECTION EMPTY"},
		{"SELECT ST_EQUALS(ST_UNION(" + big + ", " + small + "), " + big + "), ST_ASTEXT(ST_INTERSECTION(" + l2 + ", ST_GEOGPOINT(-122.12, 47.67)))", "true POINT(-122.12 47.67)"},
		// ST_BUFFER and ST_BUFFERWITHTOLERANCE.
		{"SELECT ST_NUMPOINTS(ST_BUFFER(ST_GEOGFROMTEXT('POINT(1 2)'), 50, 2)), ST_NUMPOINTS(ST_BUFFER(ST_GEOGFROMTEXT('POINT(100 2)'), 50))", "9 33"},
		{"SELECT ST_NUMPOINTS(ST_BUFFERWITHTOLERANCE(ST_GEOGFROMTEXT('POINT(1 2)'), 100, 25)), ST_NUMPOINTS(ST_BUFFERWITHTOLERANCE(ST_GEOGFROMTEXT('POINT(100 2)'), 100, 1))", "6 24"},
		// ST_SIMPLIFY.
		{"SELECT ST_ASTEXT(ST_SIMPLIFY(ST_GEOGFROMTEXT('LINESTRING(0 0, 0.05 0, 0.1 0, 0.15 0, 2 0)'), 1))", "LINESTRING(0 0, 2 0)"},
		{"SELECT ST_ASTEXT(ST_SIMPLIFY(" + tri + ", 1000))", "POLYGON((0 0, 0.1 0, 0.1 0.1, 0 0))"},
		{"SELECT ST_ASTEXT(ST_SIMPLIFY(" + tri + ", 10000))", "LINESTRING(0 0, 0.1 0.1)"},
		{"SELECT ST_ASTEXT(ST_SIMPLIFY(" + tri + ", 100000))", "POINT(0 0)"},
	}
	for _, p := range pairs {
		if got := joined(run(p[0], nil, nil)); got != p[1] {
			t.Errorf("%s =\n%s\nwant\n%s", p[0], got, p[1])
		}
	}
	for sql, want := range map[string]string{
		"SELECT ST_ASTEXT(ST_SIMPLIFY(ST_GEOGPOINT(1, 2), -1))":                        "non-negative",
		"SELECT ST_ASTEXT(ST_SIMPLIFY(ST_GEOGPOINT(1, 2), 8e6))":                       "at most",
		"SELECT ST_ASTEXT(ST_BUFFER(ST_GEOGPOINT(1, 2), 1, use_spheroid => TRUE))":     "use_spheroid",
		"SELECT ST_ASTEXT(ST_BUFFER(ST_GEOGPOINT(1, 2), 1, endcap => 'SQUARE'))":       "endcap",
		"SELECT ST_ASTEXT(ST_BUFFERWITHTOLERANCE(ST_GEOGPOINT(1, 2), 1, 0))":           "tolerance_meters",
		"SELECT ST_ASTEXT(ST_UNION(ST_GEOGPOINT(1, 2)))":                               "No matching signature",
		"SELECT ST_ASTEXT(ST_INTERSECTION(ST_GEOGPOINT(1, 2), ST_GEOGPOINT(1, 2), 3))": "No matching signature",
	} {
		err := prepare(sql)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", sql, err, want)
		}
	}
}
