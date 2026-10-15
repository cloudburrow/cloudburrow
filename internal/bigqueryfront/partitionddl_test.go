package bigqueryfront

import (
	"reflect"
	"strings"
	"testing"
)

// TestPartitionAndClusterClausesAreRead: #1272.
func TestPartitionAndClusterClausesAreRead(t *testing.T) {
	types := map[string]string{"d": "DATE", "ts": "TIMESTAMP", "dt": "DATETIME", "n": "INT64", "s": "STRING", "f": "FLOAT64"}
	for _, c := range []struct {
		sql           string
		time, rangeP  map[string]any
		clustering    []string
		refused       string
		noPartitionBy bool
	}{
		{sql: "CREATE TABLE p.t (d DATE, s STRING) PARTITION BY d CLUSTER BY s", time: map[string]any{"type": "DAY", "field": "d"}, clustering: []string{"s"}},
		{sql: "CREATE OR REPLACE TABLE p.t (ts TIMESTAMP) PARTITION BY DATE(ts) OPTIONS(description='x')", time: map[string]any{"type": "DAY", "field": "ts"}},
		{sql: "CREATE TABLE p.t (ts TIMESTAMP) PARTITION BY TIMESTAMP_TRUNC(ts, HOUR)", time: map[string]any{"type": "HOUR", "field": "ts"}},
		{sql: "CREATE TABLE p.t (dt DATETIME) PARTITION BY DATETIME_TRUNC(dt, MONTH)", time: map[string]any{"type": "MONTH", "field": "dt"}},
		{sql: "CREATE TABLE p.t (d DATE) PARTITION BY DATE_TRUNC(d, YEAR)", time: map[string]any{"type": "YEAR", "field": "d"}},
		{sql: "CREATE TABLE p.t (n INT64) PARTITION BY _PARTITIONDATE", time: map[string]any{"type": "DAY"}},
		{sql: "CREATE TABLE p.t (n INT64) PARTITION BY TIMESTAMP_TRUNC(_PARTITIONTIME, HOUR)", time: map[string]any{"type": "HOUR"}},
		{sql: "CREATE TABLE p.t (n INT64) PARTITION BY RANGE_BUCKET(n, GENERATE_ARRAY(-10, 100, 10)) CLUSTER BY n, s",
			rangeP: map[string]any{"field": "n", "range": map[string]any{"start": "-10", "end": "100", "interval": "10"}}, clustering: []string{"n", "s"}},
		{sql: "CREATE TABLE p.t (n INT64) CLUSTER BY s", clustering: []string{"s"}, noPartitionBy: true},
		{sql: "CREATE TABLE p.t (s STRING) PARTITION BY s", refused: "PARTITION BY expression must be"},
		{sql: "CREATE TABLE p.t (d DATE) PARTITION BY DATE_TRUNC(d, DAY)", refused: "PARTITION BY expression must be"},
		{sql: "CREATE TABLE p.t (d DATE) PARTITION BY x", refused: "Unrecognized name: x"},
		{sql: "CREATE TABLE p.t (f FLOAT64) CLUSTER BY f", refused: "Clustering on a column of type FLOAT64", noPartitionBy: true},
		{sql: "CREATE TABLE p.t (n INT64) CLUSTER BY n, s, d, ts, dt", refused: "Too many clustering columns: 5", noPartitionBy: true},
	} {
		d, msg, ok := parseDDLOptions(c.sql)
		if !ok || msg != "" || !d.sets() {
			t.Fatalf("%s: parsed %v %q %v", c.sql, ok, msg, d.sets())
		}
		if (d.partition == nil) != c.noPartitionBy {
			t.Fatalf("%s: PARTITION BY read %v", c.sql, d.partition)
		}
		var spec partitionSpec
		refused := ""
		if d.partition != nil {
			spec.time, spec.rangeP, refused = partitionOf(d.partition, types)
		}
		if refused == "" && d.cluster != nil {
			spec.clustering, refused = clusteringOf(d.cluster, types)
			if refused == "" && !reflect.DeepEqual(spec.clustering["fields"], c.clustering) {
				t.Errorf("%s: clustering %v", c.sql, spec.clustering)
			}
		}
		if c.refused != "" {
			if !strings.Contains(refused, c.refused) {
				t.Errorf("%s: refused %q, want %q", c.sql, refused, c.refused)
			}
			continue
		}
		if refused != "" || !reflect.DeepEqual(spec.time, c.time) || !reflect.DeepEqual(spec.rangeP, c.rangeP) {
			t.Errorf("%s: %q %v %v", c.sql, refused, spec.time, spec.rangeP)
		}
	}
	if d, _, ok := parseDDLOptions("CREATE TABLE p.t (a INT64)"); ok && d.sets() {
		t.Error("a CREATE TABLE with no clauses was read as setting something")
	}
	d, _, _ := parseDDLOptions("CREATE TABLE p.t PARTITION BY d CLUSTER BY s AS SELECT DATE '2024-01-01' AS d, 'x' AS s")
	if len(d.partition) != 1 || len(d.cluster) != 1 || !strings.Contains(d.query, "SELECT DATE") || d.columns != nil {
		t.Errorf("CTAS read as %+v", d)
	}
}
