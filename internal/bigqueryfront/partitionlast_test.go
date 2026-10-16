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
	_, _, invalid, notBuilt := partitionSchema(old, []field{{Name: "n", Type: "STRING"}}, nil, "p:d.t$20240101", "")
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

// #1273: a column left out through a partition: relaxed with
// ALLOW_FIELD_RELAXATION, invalid without it when REQUIRED, 501 when
// NULLABLE or the partitioning column; columns reordered and a RECORD's
// fields changed stay 501.
func TestPartitionSchemaLeftOut(t *testing.T) {
	old := []field{{Name: "d", Type: "DATE"}, {Name: "a", Type: "STRING", Mode: "REQUIRED"}, {Name: "b", Type: "INTEGER"}}
	relax := []string{"ALLOW_FIELD_RELAXATION"}
	target, changed, invalid, notBuilt := partitionSchema(old, []field{{Name: "d", Type: "DATE"}, {Name: "b", Type: "INTEGER"}}, relax, "t", "d")
	if invalid != "" || notBuilt != "" || !changed || len(target) != 3 || target[1].Mode != "NULLABLE" {
		t.Errorf("a left out, relaxed: %v %v %q %q", target, changed, invalid, notBuilt)
	}
	if _, _, invalid, _ := partitionSchema(old, []field{{Name: "d", Type: "DATE"}, {Name: "b", Type: "INTEGER"}}, nil, "t", "d"); !strings.Contains(invalid, "Field a is missing") {
		t.Errorf("REQUIRED a left out without the option: %q", invalid)
	}
	if _, _, _, notBuilt := partitionSchema(old, old[:2], nil, "t", "d"); notBuilt == "" {
		t.Error("NULLABLE b left out without the option: built")
	}
	if _, _, _, notBuilt := partitionSchema(old, old[1:], relax, "t", "d"); notBuilt == "" {
		t.Error("the partitioning column left out: built")
	}
	if _, _, _, notBuilt := partitionSchema(old, []field{old[0], old[2], old[1]}, relax, "t", "d"); !strings.Contains(notBuilt, "order") {
		t.Errorf("reordered: %q", notBuilt)
	}
	rec := []field{{Name: "r", Type: "RECORD", Fields: []field{{Name: "x", Type: "INTEGER"}}}}
	if _, _, _, notBuilt := partitionSchema(rec, []field{{Name: "r", Type: "RECORD", Fields: []field{{Name: "x", Type: "INTEGER"}, {Name: "y", Type: "INTEGER"}}}},
		[]string{"ALLOW_FIELD_ADDITION"}, "t", ""); notBuilt == "" {
		t.Error("a RECORD's fields changed: built")
	}
}
