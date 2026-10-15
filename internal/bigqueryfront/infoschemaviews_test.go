package bigqueryfront

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestInformationSchemaViewsReferencesAreFound: #1274.
func TestInformationSchemaViewsReferencesAreFound(t *testing.T) {
	for _, c := range []struct {
		sql      string
		datasets []string
		alias    []bool
	}{
		{"SELECT * FROM ds.INFORMATION_SCHEMA.VIEWS", []string{"ds"}, []bool{false}},
		{"SELECT v.table_name FROM `p.ds.INFORMATION_SCHEMA.VIEWS` v WHERE 1=1", []string{"ds"}, []bool{true}},
		{"SELECT * FROM ds.information_schema.views AS x JOIN e.INFORMATION_SCHEMA.VIEWS USING (table_name)", []string{"ds", "e"}, []bool{true, false}},
		{"SELECT * FROM ds.INFORMATION_SCHEMA.TABLES", []string{"ds"}, []bool{false}},
		{"SELECT * FROM ds.INFORMATION_SCHEMA.PARTITIONS", []string{"ds"}, []bool{false}},
		{"SELECT * FROM `region-us`.INFORMATION_SCHEMA.TABLES", nil, nil},
		{"SELECT * FROM ds.INFORMATION_SCHEMA.NO_SUCH_VIEW", nil, nil},
		// #1286: in a script the engine's own TABLES is kept, the front's ROUTINES read.
		{"CREATE TABLE ds.t (a INT64); SELECT * FROM ds.INFORMATION_SCHEMA.TABLES; SELECT * FROM ds.INFORMATION_SCHEMA.ROUTINES;", []string{"ds"}, []bool{false}},
		{"SELECT 'ds.INFORMATION_SCHEMA.VIEWS'", nil, nil},
	} {
		refs := informationSchemaViews(c.sql)
		if len(refs) != len(c.datasets) {
			t.Fatalf("%s: %+v", c.sql, refs)
		}
		for i, ref := range refs {
			if ref.dataset != c.datasets[i] || ref.alias != c.alias[i] {
				t.Errorf("%s: ref %d %+v", c.sql, i, ref)
			}
		}
	}
}

// TestInformationSchemaRows: #1286, the rows of TABLES, COLUMNS,
// COLUMN_FIELD_PATHS, TABLE_OPTIONS, ROUTINES and PARAMETERS have one value
// per column of their row types.
func TestInformationSchemaRows(t *testing.T) {
	var table infoTable
	if err := json.Unmarshal([]byte(`{"tableId":"t","type":"TABLE","creationTime":"1700000000000","description":"d \"x\"",
		"labels":{"b":"2","a":"1"},"expirationTime":"1800000000000",
		"timePartitioning":{"type":"DAY","field":"d","expirationMs":"172800000"},"clustering":{"fields":["s"]},
		"schema":{"fields":[{"name":"d","type":"DATE","mode":"REQUIRED"},{"name":"s","type":"STRING","maxLength":"10"},
		{"name":"r","type":"RECORD","mode":"REPEATED","fields":[{"name":"x","type":"INTEGER","description":"xd"}]}]}}`), &table); err != nil {
		t.Fatal(err)
	}
	count := func(columns string) int { return len(splitTop(mustLex(t, columns[len("STRUCT<"):len(columns)-1]))) }
	check := func(name, columns string, rows [][]string, want int) {
		t.Helper()
		if len(rows) != want {
			t.Fatalf("%s: %d rows, want %d: %v", name, len(rows), want, rows)
		}
		for _, r := range rows {
			if len(r) != count(columns) {
				t.Errorf("%s: %d values for %d columns: %v", name, len(r), count(columns), r)
			}
		}
	}
	check("TABLES", tablesColumns, tablesRows("p", "ds", table), 1)
	cols := columnsRows("p", "ds", table)
	check("COLUMNS", columnsColumns, cols, 3)
	if cols[2][6] != "'ARRAY<STRUCT<x INT64>>'" || cols[1][6] != "'STRING(10)'" || cols[0][5] != "'NO'" || cols[0][13] != "'YES'" || cols[1][14] != "1" {
		t.Errorf("COLUMNS: %v", cols)
	}
	check("COLUMN_FIELD_PATHS", fieldPathsColumns, fieldPathsRows("p", "ds", table), 4)
	opts := tableOptionsRows("p", "ds", table)
	check("TABLE_OPTIONS", tableOptionsColumns, opts, 4)
	if got := tableDDL("p", "ds", table); !strings.Contains(got, "PARTITION BY d\nCLUSTER BY s\nOPTIONS(") {
		t.Errorf("ddl: %s", got)
	}
	var fn map[string]any
	if err := json.Unmarshal([]byte(`{"routineReference":{"routineId":"f"},"routineType":"SCALAR_FUNCTION","language":"SQL",
		"arguments":[{"name":"x","dataType":{"typeKind":"INT64"}},{"name":"y","argumentKind":"ANY_TYPE"}],
		"returnType":{"typeKind":"INT64"},"definitionBody":"x + 1","creationTime":"1700000000000"}`), &fn); err != nil {
		t.Fatal(err)
	}
	check("ROUTINES", routinesColumns, routinesRows("p", "ds", fn), 1)
	params := parametersRows("p", "ds", fn)
	check("PARAMETERS", parametersColumns, params, 3)
	if params[0][3] != "0" || params[0][5] != "'YES'" || params[2][7] != "'ANY TYPE'" {
		t.Errorf("PARAMETERS: %v", params)
	}
}

// TestInformationSchemaMoreRows: #1328, the rows of the constraint,
// snapshot and routine option views have one value per column.
func TestInformationSchemaMoreRows(t *testing.T) {
	var table infoTable
	if err := json.Unmarshal([]byte(`{"tableId":"o","type":"TABLE","creationTime":"1700000000000",
		"tableConstraints":{"primaryKey":{"columns":["id","k"]},"foreignKeys":[{"name":"fk1",
		"referencedTable":{"projectId":"p","datasetId":"d","tableId":"c"},
		"columnReferences":[{"referencingColumn":"cid","referencedColumn":"id"}]}]},
		"snapshotDefinition":{"baseTableReference":{"projectId":"p","datasetId":"d","tableId":"b"},"snapshotTime":"2026-10-15T00:00:00Z"}}`), &table); err != nil {
		t.Fatal(err)
	}
	count := func(columns string) int { return len(splitTop(mustLex(t, columns[len("STRUCT<"):len(columns)-1]))) }
	check := func(name, columns string, rows [][]string, want int) {
		t.Helper()
		if len(rows) != want {
			t.Fatalf("%s: %d rows, want %d: %v", name, len(rows), want, rows)
		}
		for _, r := range rows {
			if len(r) != count(columns) {
				t.Errorf("%s: %d values for %d columns: %v", name, len(r), count(columns), r)
			}
		}
	}
	check("TABLE_CONSTRAINTS", tableConstraintsColumns, tableConstraintsRows("p", "d", table), 2)
	check("KEY_COLUMN_USAGE", keyColumnUsageColumns, keyColumnUsageRows("p", "d", table), 3)
	check("CONSTRAINT_COLUMN_USAGE", constraintColumnUsageColumns, constraintColumnUsageRows("p", "d", table), 3)
	check("TABLE_SNAPSHOTS", snapshotsColumns, snapshotsRows("p", "d", table), 1)
	if r := tableConstraintsRows("p", "d", table)[0]; r[2] != "'o.pk$'" || r[6] != "'PRIMARY KEY'" {
		t.Errorf("primary key row %v", r)
	}
	def := map[string]any{"routineReference": map[string]any{"routineId": "f"}, "description": "d",
		"importedLibraries": []any{"gs://b/a.js"}}
	rows := routineOptionsRows("p", "d", def)
	check("ROUTINE_OPTIONS", routineOptionsColumns, rows, 2)
	if rows[1][5] != `'["gs://b/a.js"]'` {
		t.Errorf("library %v", rows[1])
	}
	if id, ok := partitionIDExpr(partitionMeta{}); !ok || id != "" {
		t.Errorf("unpartitioned id %q", id)
	}
}
