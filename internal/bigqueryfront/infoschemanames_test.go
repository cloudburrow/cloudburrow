package bigqueryfront

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
)

// TestPushedNameFilter (#1510): the dataset and table names a query of a
// view can read are those of = and IN conjuncts of the WHERE of the one
// SELECT whose only FROM item is the view; anything else pushes nothing.
func TestPushedNameFilter(t *testing.T) {
	set := func(s ...string) map[string]bool {
		if s == nil {
			return nil
		}
		m := map[string]bool{}
		for _, v := range s {
			m[v] = true
		}
		return m
	}
	const ts = "`region-us`.INFORMATION_SCHEMA.TABLE_STORAGE"
	for _, c := range []struct {
		sql             string
		schemas, tables map[string]bool
		none            bool
	}{
		{sql: "SELECT * FROM " + ts + " WHERE table_schema = 'd1'", schemas: set("d1")},
		{sql: "SELECT * FROM " + ts + " WHERE table_schema = 'd1' ORDER BY table_name DESC", schemas: set("d1")},
		{sql: "SELECT * FROM " + ts + " WHERE table_schema = 'd1' AND table_name = 't'", schemas: set("d1"), tables: set("t")},
		{sql: "SELECT * FROM " + ts + " WHERE table_schema IN ('d1', \"d2\") AND table_name != 'p'", schemas: set("d1", "d2")},
		{sql: "SELECT * FROM " + ts + " AS s WHERE s.table_schema = 'd1' AND (s.table_name IN ('a', 'b'))", schemas: set("d1"), tables: set("a", "b")},
		{sql: "SELECT * FROM " + ts + " s WHERE 'd1' = table_schema", schemas: set("d1")},
		{sql: "SELECT * FROM " + ts + " WHERE table_schema IN ('d1', 'd2') AND table_schema = 'd2'", schemas: set("d2")},
		{sql: "SELECT * FROM " + ts + " WHERE table_schema = 'd1' AND table_schema = 'd2'", schemas: map[string]bool{}},
		{sql: "SELECT DISTINCT table_schema FROM `region-us`.INFORMATION_SCHEMA.TABLES WHERE table_schema IN ('a', 'b')", schemas: set("a", "b")},
		{sql: "SELECT * FROM `region-us`.INFORMATION_SCHEMA.ROUTINES WHERE routine_schema = 'd1'", schemas: set("d1")},
		{sql: "SELECT * FROM `region-us`.INFORMATION_SCHEMA.PARAMETERS WHERE specific_schema = 'd1' AND specific_name = 'f'", schemas: set("d1")},
		{sql: "SELECT * FROM (SELECT * FROM " + ts + " WHERE table_schema = 'd1') WHERE total_rows > 0", schemas: set("d1")},
		// Nothing pushed down: an OR, another FROM item, a column of
		// another table, a value that is not a plain literal, no WHERE,
		// a view of no table names.
		{sql: "SELECT * FROM " + ts + " WHERE table_schema = 'd1' OR table_name = 't'", none: true},
		{sql: "SELECT * FROM " + ts + " AS s JOIN d.t ON TRUE WHERE s.table_schema = 'd1'", none: true},
		{sql: "SELECT * FROM " + ts + ", d.t WHERE table_schema = 'd1'", none: true},
		{sql: "SELECT * FROM " + ts + " AS s WHERE x.table_schema = 'd1'", none: true},
		{sql: "SELECT * FROM " + ts + " WHERE table_schema = CONCAT('d', '1')", none: true},
		{sql: "SELECT * FROM " + ts + " WHERE table_schema = r'd1'", none: true},
		{sql: "SELECT * FROM " + ts + " WHERE table_schema LIKE 'd%'", none: true},
		{sql: "SELECT * FROM " + ts, none: true},
		{sql: "SELECT * FROM " + ts + " WHERE NOT table_schema = 'd1'", none: true},
		{sql: "SELECT * FROM `region-us`.INFORMATION_SCHEMA.SCHEMATA WHERE schema_name = 'd1'", none: true},
		{sql: "SELECT * FROM `region-us`.INFORMATION_SCHEMA.ROUTINES WHERE table_schema = 'd1'", none: true},
	} {
		refs := informationSchemaJobs(c.sql)
		if strings.Contains(c.sql, ".SCHEMATA ") {
			refs = []jobsRef{{pos: strings.Index(c.sql, "`region-us`"), end: strings.Index(c.sql, " WHERE"), view: "SCHEMATA"}}
		}
		if len(refs) != 1 {
			t.Fatalf("%s: %d references", c.sql, len(refs))
		}
		got := pushedNameFilter(c.sql, refs[0].pos, refs[0].end, refs[0].alias, refs[0].view)
		if c.none {
			if got != nil {
				t.Errorf("%s: pushed %+v, want nothing", c.sql, got)
			}
			continue
		}
		if got == nil || !reflect.DeepEqual(got.schemas, c.schemas) || !reflect.DeepEqual(got.tables, c.tables) {
			t.Errorf("%s: pushed %+v, want schemas %v, tables %v", c.sql, got, c.schemas, c.tables)
		}
	}
	// A dataset-qualified view: its table names.
	sql := "SELECT * FROM d1.INFORMATION_SCHEMA.PARTITIONS WHERE table_name = 'p'"
	refs := informationSchemaViews(sql)
	if len(refs) != 1 {
		t.Fatalf("%s: %d references", sql, len(refs))
	}
	if got := pushedNameFilter(sql, refs[0].pos, refs[0].end, refs[0].alias, refs[0].view); got == nil || !reflect.DeepEqual(got.tables, set("p")) {
		t.Errorf("%s: pushed %+v", sql, got)
	}
}

// TestNameFilterKeys (#1510): two filters of the same names have one key,
// and no filter keeps everything.
func TestNameFilterKeys(t *testing.T) {
	a := &nameFilter{schemas: map[string]bool{"x": true, "y": true}}
	b := &nameFilter{schemas: map[string]bool{"y": true, "x": true}}
	c := &nameFilter{tables: map[string]bool{"x": true, "y": true}}
	var none *nameFilter
	if a.key() != b.key() || a.key() == c.key() || none.key() != "" || a.key() == "" {
		t.Errorf("keys %q %q %q %q", a.key(), b.key(), c.key(), none.key())
	}
	if !none.schema("any") || !none.table("any") || !a.table("any") || !a.schema("x") || a.schema("z") || c.table("z") {
		t.Error("filter keeps the wrong names")
	}
	if !(*datasetMeta)(nil).keeps("t") || (&datasetMeta{only: c}).keeps("z") || !(&datasetMeta{only: c}).keeps("x") {
		t.Error("datasetMeta keeps the wrong tables")
	}
}

// TestRegionRowsReadOnlyTheFilteredTables (#1510): a region view of one
// dataset's table reads no other dataset, and no other table's tables.get.
func TestRegionRowsReadOnlyTheFilteredTables(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	engine := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		p := strings.TrimPrefix(r.URL.Path, "/bigquery/v2/projects/p")
		switch {
		case p == "/datasets":
			_, _ = w.Write([]byte(`{"datasets":[{"datasetReference":{"datasetId":"d1"}},{"datasetReference":{"datasetId":"d2"}}]}`))
		case p == "/datasets/d1" || p == "/datasets/d2":
			_, _ = w.Write([]byte(`{"location":"US"}`))
		case strings.HasSuffix(p, "/tables"):
			_, _ = w.Write([]byte(`{"tables":[{"tableReference":{"tableId":"a"}},{"tableReference":{"tableId":"b"}}]}`))
		case strings.Contains(p, "/tables/"):
			_, _ = w.Write([]byte(`{"type":"TABLE","schema":{"fields":[{"name":"n","type":"INTEGER"}]}}`))
		default:
			http.NotFound(w, r)
		}
	})
	f := front{next: engine, base: "/bigquery/v2/projects/p"}
	r := httptest.NewRequest(http.MethodPost, "http://bq/bigquery/v2/projects/p/queries", nil)
	read := func(only *nameFilter) (string, []string) {
		paths = nil
		sub, st, msg := f.regionRows(r, &datasetMeta{only: only}, "TABLES", "", inRegion("us", infoSchemaBuilders["TABLES"]))
		if st != 0 {
			t.Fatalf("regionRows: %d %s", st, msg)
		}
		var gets []string
		for _, p := range paths {
			if strings.Contains(p, "/tables/") {
				gets = append(gets, strings.TrimPrefix(p, "/bigquery/v2/projects/p/datasets/"))
			}
		}
		sort.Strings(gets)
		return sub, gets
	}
	if _, gets := read(nil); !reflect.DeepEqual(gets, []string{"d1/tables/a", "d1/tables/b", "d2/tables/a", "d2/tables/b"}) {
		t.Errorf("no filter read %v", gets)
	}
	sub, gets := read(&nameFilter{schemas: map[string]bool{"d2": true}, tables: map[string]bool{"b": true}})
	if !reflect.DeepEqual(gets, []string{"d2/tables/b"}) {
		t.Errorf("d2.b read %v", gets)
	}
	if strings.Contains(sub, "'d1'") || !strings.Contains(sub, "'d2', 'b'") || strings.Contains(sub, "'a'") {
		t.Errorf("d2.b rows %s", sub)
	}
	for _, p := range paths {
		if strings.Contains(p, "/datasets/d1") {
			t.Errorf("d2.b read %s", p)
		}
	}
	if sub, gets := read(&nameFilter{schemas: map[string]bool{}}); len(gets) != 0 || !strings.Contains(sub, "UNNEST(ARRAY<"+tablesColumns+">[])") {
		t.Errorf("no dataset read %v: %s", gets, sub)
	}
}

// TestMaterializedViewsReadNoBaseTable (#1510): MATERIALIZED_VIEWS reads
// the tables.get of no table tables.list gives as a TABLE.
func TestMaterializedViewsReadNoBaseTable(t *testing.T) {
	var gets []string
	engine := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/bigquery/v2/projects/p/datasets/d")
		switch {
		case p == "":
			_, _ = w.Write([]byte(`{}`))
		case p == "/tables":
			_, _ = w.Write([]byte(`{"tables":[{"tableReference":{"tableId":"t"},"type":"TABLE"},` +
				`{"tableReference":{"tableId":"mv"},"type":"MATERIALIZED_VIEW"},{"tableReference":{"tableId":"v"},"type":"VIEW"}]}`))
		default:
			gets = append(gets, strings.TrimPrefix(p, "/tables/"))
			_, _ = w.Write([]byte(`{"type":"MATERIALIZED_VIEW","materializedView":{"query":"SELECT 1"}}`))
		}
	})
	f := front{next: engine, base: "/bigquery/v2/projects/p"}
	r := httptest.NewRequest(http.MethodPost, "http://bq/bigquery/v2/projects/p/queries", nil)
	if _, st, msg := infoSchemaBuilders["MATERIALIZED_VIEWS"](f, r, "d", metaFor(&datasetMeta{}, "MATERIALIZED_VIEWS", nil)); st != 0 {
		t.Fatalf("%d %s", st, msg)
	}
	if sort.Strings(gets); !reflect.DeepEqual(gets, []string{"mv", "v"}) {
		t.Errorf("read %v, want mv and v", gets)
	}
	if shared := (&datasetMeta{}); metaFor(shared, "TABLES", nil) != shared {
		t.Error("TABLES with no filter does not share the query's datasetMeta")
	}
}
