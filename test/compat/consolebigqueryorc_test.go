//go:build compat

package compat

import (
	"fmt"
	"strings"
	"testing"
)

// TestConsoleBigQueryLoadORCThroughTheForm (#1713): the console's load
// forms offer ORC. Load from Cloud Storage on a dataset's page makes a
// table of an ORC file's schema, and Load from a file appends an uploaded
// ORC file; both read back through the official Go client.
//
// covers: bigquery.jobs.insert, bigquery.jobs.get, bigquery.jobs.query
func TestConsoleBigQueryLoadORCThroughTheForm(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	addr := consoleAddr(t, h)
	ctx := h.Context()
	ds := validationDataset(t, h, c)
	schema := "struct<id:bigint,name:string,tags:array<string>>"
	bucket := loadBucket(t, h, "bq-orc-console", map[string]string{
		"people.orc": orcFile(t, schema, true, []any{int64(1), "ann", []any{"a"}}, []any{int64(2), nil, []any{}}),
	})

	_, fields, _ := consoleJobResult(t, addr, project, []string{ds.DatasetID}, "load", map[string]string{
		"tableId": "people", "uris": "gs://" + bucket.BucketName() + "/people.orc", "format": "ORC",
		"writeDisposition": "WRITE_APPEND",
	})
	if fields["State"] != "Succeeded" || fields["Output rows"] != "2" {
		t.Errorf("the ORC load answered %v, want Succeeded with 2 output rows", fields)
	}
	if got, want := loadedSchema(t, ctx, ds.Table("people")), "id INTEGER, name STRING, tags STRING REPEATED"; got != want {
		t.Errorf("the ORC table's schema: got %s want %s", got, want)
	}

	code, out := consoleLoadFile(t, addr, project, []string{ds.DatasetID, "people"}, map[string]string{
		"format": "ORC", "writeDisposition": "WRITE_APPEND",
	}, "more.orc", []byte(orcFile(t, schema, false, []any{int64(3), "cy", []any{"b", "c"}})))
	if _, fields := loadFileJob(t, code, out, project); fields["Output rows"] != "1" {
		t.Errorf("the uploaded ORC load answered %v, want 1 output row", fields)
	}
	got := fmt.Sprint(rows(t, ctx, c, "SELECT id, name, ARRAY_LENGTH(tags) FROM "+ds.DatasetID+".people ORDER BY id"))
	if want := "[[1 ann 1] [2 <nil> 0] [3 cy 2]]"; !strings.EqualFold(got, want) {
		t.Errorf("the table reads %s, want %s", got, want)
	}
}
