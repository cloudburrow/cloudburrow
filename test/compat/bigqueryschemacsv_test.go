//go:build compat

package compat

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
	"google.golang.org/api/googleapi"
)

// The tests in this file are #951 and #952, each driven through the
// official Go client and measured against the pinned emulator first (the
// measurements are in internal/bigqueryfront and docs/compatibility.md).

// datasetExists reports whether datasets.get finds ds, and deletes it with
// its contents when the test ends.
func datasetExists(t *testing.T, ctx context.Context, ds *bigquery.Dataset) (*bigquery.DatasetMetadata, bool) {
	t.Helper()
	t.Cleanup(func() { _ = ds.DeleteWithContents(context.Background()) })
	md, err := ds.Metadata(ctx)
	var e *googleapi.Error
	if errors.As(err, &e) && e.Code == http.StatusNotFound {
		return nil, false
	}
	if err != nil {
		t.Fatalf("Metadata of %s: %v", ds.DatasetID, err)
	}
	return md, true
}

// TestBigQueryCreateSchemaMakesTheDataset (#951): CREATE SCHEMA of a
// dataset that does not exist makes it, through jobs.query and
// jobs.insert, with its description, friendly_name, labels and location;
// a script that makes a dataset and then a table in it runs, also after a
// DROP SCHEMA IF EXISTS of it; a script that fails leaves no dataset; an
// option the front cannot apply, or a CREATE SCHEMA an earlier statement
// already names, is 501 and makes nothing. Measured first: the emulator
// ran CREATE SCHEMA of a new dataset as done and made none.
func TestBigQueryCreateSchemaMakesTheDataset(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ctx := h.Context()
	prefix := strings.ReplaceAll(h.Project(), "-", "_")

	opts := c.Dataset(prefix + "_opts")
	sql := "CREATE SCHEMA " + opts.DatasetID + " OPTIONS(description='made by DDL', friendly_name='Made', " +
		"labels=[('team', 'data'), ('env', 'test')], location='EU')"
	if err := bqRun(ctx, c, sql, false); err != nil {
		t.Fatalf("CREATE SCHEMA with OPTIONS (jobs.query): %v", err)
	}
	md, ok := datasetExists(t, ctx, opts)
	if !ok {
		t.Fatal("CREATE SCHEMA made no dataset")
	}
	if md.Description != "made by DDL" || md.Name != "Made" || md.Location != "EU" ||
		!reflect.DeepEqual(md.Labels, map[string]string{"team": "data", "env": "test"}) {
		t.Errorf("the dataset reads description %q, name %q, location %q, labels %v", md.Description, md.Name, md.Location, md.Labels)
	}

	job := c.Dataset(prefix + "_job")
	if err := bqRun(ctx, c, "CREATE SCHEMA IF NOT EXISTS "+job.DatasetID, true); err != nil {
		t.Fatalf("CREATE SCHEMA IF NOT EXISTS (jobs.insert): %v", err)
	}
	if _, ok := datasetExists(t, ctx, job); !ok {
		t.Error("CREATE SCHEMA IF NOT EXISTS through jobs.insert made no dataset")
	}

	for name, script := range map[string]string{
		"_script": "CREATE SCHEMA %[1]s; CREATE TABLE %[1]s.t (a INT64); INSERT INTO %[1]s.t (a) VALUES (1)",
		"_redo":   "DROP SCHEMA IF EXISTS %[1]s CASCADE; CREATE SCHEMA %[1]s; CREATE TABLE %[1]s.t (a INT64); INSERT INTO %[1]s.t (a) VALUES (1)",
	} {
		ds := c.Dataset(prefix + name)
		if err := bqRun(ctx, c, strings.ReplaceAll(script, "%[1]s", ds.DatasetID), false); err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if _, ok := datasetExists(t, ctx, ds); !ok {
			t.Errorf("%s: no dataset", name)
			continue
		}
		if n := countRows(t, h, ds.Table("t")); n != 1 {
			t.Errorf("%s: the table has %d rows, want 1", name, n)
		}
	}

	failed := c.Dataset(prefix + "_failed")
	if err := bqRun(ctx, c, "CREATE SCHEMA "+failed.DatasetID+"; SELECT * FROM "+failed.DatasetID+".missing", false); err == nil {
		t.Error("a script reading a missing table succeeded")
	}
	if _, ok := datasetExists(t, ctx, failed); ok {
		t.Error("the failed script left its dataset")
	}

	for what, sql := range map[string]string{
		"an option the front does not apply":   "CREATE SCHEMA %s OPTIONS(default_table_expiration_days=1)",
		"DEFAULT COLLATE":                      "CREATE SCHEMA %s DEFAULT COLLATE 'und:ci'",
		"a dataset an earlier statement names": "SELECT * FROM %[1]s.t; CREATE SCHEMA %[1]s",
	} {
		ds := c.Dataset(prefix + "_refused")
		sql = strings.ReplaceAll(strings.ReplaceAll(sql, "%[1]s", ds.DatasetID), "%s", ds.DatasetID)
		for _, insert := range []bool{false, true} {
			err := bqRun(ctx, c, sql, insert)
			wantReason(t, what, err, http.StatusNotImplemented, "notImplemented")
		}
		if _, ok := datasetExists(t, ctx, ds); ok {
			t.Errorf("%s made the dataset", what)
		}
	}
}

// TestBigQueryCSVLoadOtherOptions (#952): allowQuotedNewlines,
// encoding ISO-8859-1, maxBadRecords, ignoreUnknownValues, nullMarkers,
// preserveAsciiControlCharacters and sourceColumnMatch NAME are honoured,
// and a REQUIRED column is held to having a value; what the front cannot
// carry out (another encoding, timeZone and the formats, a control
// character without preserveAsciiControlCharacters) is 501 and loads
// nothing. Measured first: the emulator used none of these options; it
// loaded a quoted newline whatever allowQuotedNewlines said, stored 0xE9
// as U+FFFD under ISO-8859-1, failed the load on the first bad record
// whatever maxBadRecords or ignoreUnknownValues said, loaded the values of
// nullMarkers as text, and stored NULL in a REQUIRED column.
func TestBigQueryCSVLoadOtherOptions(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	required := bigquery.Schema{
		{Name: "a", Type: bigquery.IntegerFieldType},
		{Name: "b", Type: bigquery.StringFieldType, Required: true},
	}
	load := func(table, data string, schema bigquery.Schema, set func(*bigquery.ReaderSource)) error {
		src := bigquery.NewReaderSource(strings.NewReader(data))
		src.Schema = schema
		set(src)
		return runLoad(ctx, ds.Table(table).LoaderFrom(src))
	}
	for _, c2 := range []struct {
		table, data string
		schema      bigquery.Schema
		set         func(*bigquery.ReaderSource)
		want        [][]bigquery.Value
	}{
		{"quoted_newlines", "1,\"x\ny\"\n", loadSchema, func(s *bigquery.ReaderSource) { s.AllowQuotedNewlines = true },
			[][]bigquery.Value{{int64(1), "x\ny"}}},
		{"latin1", "1,caf\xe9\n", loadSchema, func(s *bigquery.ReaderSource) { s.Encoding = bigquery.ISO_8859_1 },
			[][]bigquery.Value{{int64(1), "café"}}},
		{"max_bad", "1,x\n2\n3,y,z\n4,w\n", loadSchema, func(s *bigquery.ReaderSource) { s.MaxBadRecords = 2 },
			[][]bigquery.Value{{int64(1), "x"}, {int64(4), "w"}}},
		{"ignore_unknown", "1,x,extra\n", loadSchema, func(s *bigquery.ReaderSource) { s.IgnoreUnknownValues = true },
			[][]bigquery.Value{{int64(1), "x"}}},
		{"null_markers", "1,NA\n2,-\n3,x\n", loadSchema, func(s *bigquery.ReaderSource) { s.NullMarkers = []string{"NA", "-"} },
			[][]bigquery.Value{{int64(1), nil}, {int64(2), nil}, {int64(3), "x"}}},
		{"control_kept", "1,a\x01b\n", loadSchema, func(s *bigquery.ReaderSource) { s.PreserveASCIIControlCharacters = true },
			[][]bigquery.Value{{int64(1), "a\x01b"}}},
		{"by_name", "b,a\nx,1\n", loadSchema, func(s *bigquery.ReaderSource) {
			s.SkipLeadingRows = 1
			s.SourceColumnMatch = bigquery.SourceColumnMatchName
		}, [][]bigquery.Value{{int64(1), "x"}}},
		{"required_jagged", "1\n2,y\n", required, func(s *bigquery.ReaderSource) { s.AllowJaggedRows = true; s.MaxBadRecords = 1 },
			[][]bigquery.Value{{int64(2), "y"}}},
	} {
		if err := load(c2.table, c2.data, c2.schema, c2.set); err != nil {
			t.Errorf("%s: %v", c2.table, err)
			continue
		}
		got := rows(t, ctx, c, "SELECT a, b FROM `"+ds.DatasetID+"."+c2.table+"` ORDER BY a")
		if !reflect.DeepEqual(got, c2.want) {
			t.Errorf("%s holds %v, want %v", c2.table, got, c2.want)
		}
	}

	for _, c2 := range []struct {
		table, data string
		schema      bigquery.Schema
		set         func(*bigquery.ReaderSource)
		code        int
		reason      string
	}{
		{"no_quoted_newlines", "1,\"x\ny\"\n", loadSchema, func(*bigquery.ReaderSource) {}, http.StatusBadRequest, "invalid"},
		{"too_many_bad", "1\n2\n3,x\n", loadSchema, func(s *bigquery.ReaderSource) { s.MaxBadRecords = 1 }, http.StatusBadRequest, "invalid"},
		{"required_empty", "1,\n", required, func(*bigquery.ReaderSource) {}, http.StatusBadRequest, "invalid"},
		{"utf16", "1,x\n", loadSchema, func(s *bigquery.ReaderSource) { s.Encoding = "UTF-16LE" }, http.StatusNotImplemented, "notImplemented"},
		{"time_zone", "1,x\n", loadSchema, func(s *bigquery.ReaderSource) { s.TimeZone = "America/Los_Angeles" }, http.StatusNotImplemented, "notImplemented"},
		{"date_format", "1,x\n", loadSchema, func(s *bigquery.ReaderSource) { s.DateFormat = "MM/DD/YYYY" }, http.StatusNotImplemented, "notImplemented"},
		{"control", "1,a\x01b\n", loadSchema, func(*bigquery.ReaderSource) {}, http.StatusNotImplemented, "notImplemented"},
		{"bad_value_max_bad", "x,y\n", loadSchema, func(s *bigquery.ReaderSource) { s.MaxBadRecords = 1 }, http.StatusNotImplemented, "notImplemented"},
	} {
		err := load(c2.table, c2.data, c2.schema, c2.set)
		if c2.reason == "invalid" {
			var be *bigquery.Error
			if err == nil || !(errors.As(err, &be) && be.Reason == "invalid" || strings.Contains(err.Error(), "invalid")) {
				t.Errorf("%s: %v, want invalid", c2.table, err)
			}
		} else {
			wantReason(t, c2.table, err, c2.code, c2.reason)
		}
		if tableExists(t, ctx, ds.Table(c2.table)) && countRows(t, h, ds.Table(c2.table)) != 0 {
			t.Errorf("the failed load %s loaded rows", c2.table)
		}
	}
}
