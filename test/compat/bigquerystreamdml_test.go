//go:build compat

package compat

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
)

// advanceBigQueryClock moves the BigQuery front's clock forward by d, a
// CloudBurrow extension (POST /cloudburrow/bigquery/v2/clock:advance,
// #1370), so a test passes BigQuery's 30 minutes at once. It returns the
// front's clock after the move; a d of 0 only reads it. The clock is the
// instance's, which earlier tests may have moved.
func advanceBigQueryClock(t *testing.T, h *Harness, d time.Duration) time.Time {
	t.Helper()
	body, _ := json.Marshal(map[string]float64{"seconds": d.Seconds()})
	resp, err := http.Post(strings.TrimSuffix(h.Endpoint(EnvBigQuery), "/")+"/cloudburrow/bigquery/v2/clock:advance",
		"application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("clock:advance: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("clock:advance: %s", resp.Status)
	}
	var got struct {
		Time time.Time `json:"time"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil || got.Time.IsZero() {
		t.Fatalf("clock:advance: %v, %+v", err, got)
	}
	return got.Time.UTC()
}

// TestBigQueryDMLOfRecentlyStreamedRows (#1370): BigQuery documents that
// "rows that were recently written using the BigQuery Storage Write API
// (REST)" (tabledata.insertAll) "can't be modified" by UPDATE, DELETE,
// MERGE or TRUNCATE: "the recent writes are those that occurred within the
// last 30 minutes. All other rows in the table remain modifiable". On a
// table that is not partitioned, which has no streaming buffer mark, a
// streamed row is refused for 30 minutes, the table's other rows are not,
// and after 30 minutes (the front's clock advanced) the row is modifiable;
// so is a row streamed to a table a CREATE TABLE statement made.
// Measured first: DML changed a streamed row of such a table at once.
//
// covers: bigquery.tabledata.insertAll, bigquery.jobs.insert, bigquery.jobs.query
func TestBigQueryDMLOfRecentlyStreamedRows(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ctx := h.Context()
	ds := validationDataset(t, h, c)
	tbl := ds.Table("sdplain")
	if err := tbl.Create(ctx, &bigquery.TableMetadata{
		Schema: bigquery.Schema{{Name: "n", Type: bigquery.IntegerFieldType}}}); err != nil {
		t.Fatal(err)
	}
	p := "`" + ds.DatasetID + ".sdplain`"
	exec := func(sql string) error { return bqRun(ctx, c, sql, true) }
	if err := exec("INSERT INTO " + p + " (n) VALUES (0)"); err != nil {
		t.Fatal(err)
	}
	type row struct {
		N int64 `bigquery:"n"`
	}
	if err := tbl.Inserter().Put(ctx, []*row{{N: 1}, {N: 2}}); err != nil {
		t.Fatal(err)
	}
	if got := queryRows(t, ctx, c, "", "", "SELECT n FROM "+p+" ORDER BY n"); strings.Join(got, ",") != "0,1,2" {
		t.Fatalf("rows %v, want 0, 1, 2 read at once", got)
	}
	for _, sql := range []string{
		"UPDATE " + p + " SET n = 10 WHERE n = 1",
		"DELETE FROM " + p + " WHERE n >= 0",
		"MERGE " + p + " t USING (SELECT 2 AS n) s ON t.n = s.n WHEN MATCHED THEN DELETE",
		"TRUNCATE TABLE " + p,
	} {
		if err := exec(sql); err == nil || !strings.Contains(err.Error(), "streaming buffer") {
			t.Errorf("%s: %v, want the streaming buffer's error", sql, err)
		}
	}
	if err := exec("UPDATE " + p + " SET n = 5 WHERE n = 0"); err != nil {
		t.Errorf("UPDATE of a row not streamed: %v", err)
	}
	advanceBigQueryClock(t, h, 29*time.Minute)
	if err := exec("DELETE FROM " + p + " WHERE n = 2"); err == nil {
		t.Error("DELETE 29 minutes after the stream: no error, want the streaming buffer's")
	}
	advanceBigQueryClock(t, h, 2*time.Minute)
	if err := exec("UPDATE " + p + " SET n = 10 WHERE n = 1"); err != nil {
		t.Errorf("UPDATE 31 minutes after the stream: %v", err)
	}
	if got := queryRows(t, ctx, c, "", "", "SELECT n FROM "+p+" ORDER BY n"); strings.Join(got, ",") != "10,2,5" {
		// queryRows sorts the rows as strings.
		t.Errorf("rows %v, want 10, 2, 5", got)
	}

	// A table made by a CREATE TABLE statement holds its streamed rows
	// too, and leaves the stream time out of its schema.
	if err := exec("CREATE TABLE `" + ds.DatasetID + ".sdddl` (n INT64)"); err != nil {
		t.Fatal(err)
	}
	ddl := ds.Table("sdddl")
	if err := ddl.Inserter().Put(ctx, []*row{{N: 1}}); err != nil {
		t.Fatal(err)
	}
	q := "`" + ds.DatasetID + ".sdddl`"
	if err := exec("DELETE FROM " + q + " WHERE n = 1"); err == nil || !strings.Contains(err.Error(), "streaming buffer") {
		t.Errorf("DELETE of a row streamed to a CREATE TABLE table: %v, want the streaming buffer's error", err)
	}
	if md, err := ddl.Metadata(ctx); err != nil || len(md.Schema) != 1 || md.Schema[0].Name != "n" {
		t.Errorf("CREATE TABLE table's schema: %v (%v), want n alone", md, err)
	}
	if got := queryRows(t, ctx, c, "", "", "SELECT column_name FROM `"+ds.DatasetID+
		"`.INFORMATION_SCHEMA.COLUMNS WHERE table_name = 'sdddl'"); strings.Join(got, ",") != "n" {
		t.Errorf("INFORMATION_SCHEMA.COLUMNS %v, want n alone", got)
	}
	advanceBigQueryClock(t, h, 31*time.Minute)
	if err := exec("DELETE FROM " + q + " WHERE n = 1"); err != nil {
		t.Errorf("DELETE 31 minutes after the stream: %v", err)
	}
}
