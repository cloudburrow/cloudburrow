package bigqueryfront

import (
	"encoding/json"
	"strings"
	"testing"
)

// #1273: a query into a partition that is not one query is refused as
// BigQuery refuses a destination for it.
func TestNotOneQuery(t *testing.T) {
	for sql, want := range map[string]string{
		"SELECT 1":                              "",
		"WITH a AS (SELECT 1) SELECT * FROM a;": "",
		"SELECT 1; SELECT 2":                    "scripts",
		"INSERT INTO d.t (n) VALUES (1)":        "DML",
		"DELETE FROM d.t WHERE TRUE":            "DML",
		"CREATE TABLE d.t (n INT64)":            "DDL",
	} {
		got := notOneQuery(sql)
		if want == "" && got != "" || want != "" && !strings.Contains(got, want) {
			t.Errorf("notOneQuery(%q) = %q, want %q", sql, got, want)
		}
	}
}

// #1273: a column retyped through a partition fails, invalid.
func TestPartitionSchemaRetyped(t *testing.T) {
	old := []field{{Name: "n", Type: "INTEGER"}}
	_, _, invalid, notBuilt := partitionSchema(old, []field{{Name: "n", Type: "STRING"}}, nil, "p:d.t$20240101")
	if notBuilt != "" || !strings.Contains(invalid, "changed type from INT64 to STRING") {
		t.Errorf("retyped: invalid %q, notBuilt %q", invalid, notBuilt)
	}
}

// #1273: addColumns of an ingestion-time partitioned table keeps the
// rows' times in one more scratch column.
func TestIngestionTimeColumn(t *testing.T) {
	if u := ingestionUnitOf(map[string]any{"timePartitioning": map[string]any{"type": "HOUR"}}); u != "HOUR" {
		t.Errorf("hourly: %q", u)
	}
	if u := ingestionUnitOf(map[string]any{"timePartitioning": map[string]any{"type": "DAY", "field": "d"}}); u != "" {
		t.Errorf("by a column: %q", u)
	}
	raw, err := withIngestionTimeColumn(json.RawMessage(`{"fields":[{"name":"n","type":"INTEGER"}]}`))
	if err != nil || !strings.Contains(string(raw), ingestionTimeColumn) {
		t.Errorf("withIngestionTimeColumn: %s, %v", raw, err)
	}
}
