//go:build compat

package compat

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
)

// TestBigQueryLoadFieldsOutsideTheSchema (#1339): a NEWLINE_DELIMITED_JSON
// load, from a local file and from Cloud Storage, of a record with a field
// the schema does not have (at the top or inside a REPEATED RECORD) fails,
// as BigQuery treats it as a bad record unless ignoreUnknownValues is set
// (JobConfigurationLoad): with it the record loads without the field, and
// with maxBadRecords 1 the record is left out, listed in the job's
// status.errors, and the others load. A load with autodetect is held to
// the schema detected from its first 500 records, so a field first seen
// after them fails the load too, and loads under ignoreUnknownValues. A
// CSV record with more values than columns follows the same rule.
//
// covers: bigquery.jobs.insert, bigquery.jobs.get, bigquery.tables.get, bigquery.tables.insert, bigquery.jobs.query
func TestBigQueryLoadFieldsOutsideTheSchema(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	sc := instanceStorage(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	bucket := sc.Bucket(strings.ReplaceAll(h.Project(), "_", "-") + "-bq-unknown")
	if err := bucket.Create(ctx, h.Project(), nil); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	var objects []string
	t.Cleanup(func() {
		for _, o := range objects {
			_ = bucket.Object(o).Delete(context.Background())
		}
		_ = bucket.Delete(context.Background())
	})
	put := func(name, data string) {
		t.Helper()
		w := bucket.Object(name).NewWriter(ctx)
		if _, err := w.Write([]byte(data)); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("write object: %v", err)
		}
		objects = append(objects, name)
	}
	schema := bigquery.Schema{
		{Name: "i", Type: bigquery.IntegerFieldType},
		{Name: "r", Type: bigquery.RecordFieldType, Repeated: true, Schema: bigquery.Schema{{Name: "x", Type: bigquery.StringFieldType}}},
	}
	// sources returns the data as a local file and as an object, set by set.
	sources := func(name, data string, format bigquery.DataFormat, set func(*bigquery.FileConfig)) map[string]bigquery.LoadSource {
		put(name, data)
		file := bigquery.NewReaderSource(strings.NewReader(data))
		file.SourceFormat = format
		set(&file.FileConfig)
		gcs := bigquery.NewGCSReference("gs://" + bucket.BucketName() + "/" + name)
		gcs.SourceFormat = format
		set(&gcs.FileConfig)
		return map[string]bigquery.LoadSource{"file": file, "gcs": gcs}
	}
	run := func(table string, src bigquery.LoadSource) (*bigquery.JobStatus, error) {
		lctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		job, err := ds.Table(table).LoaderFrom(src).Run(lctx)
		if err != nil {
			return nil, err
		}
		st, err := job.Wait(lctx)
		if err == nil {
			err = st.Err()
		}
		return st, err
	}
	makeTable := func(name string, s bigquery.Schema) {
		t.Helper()
		if err := ds.Table(name).Create(ctx, &bigquery.TableMetadata{Schema: s}); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}
	read := func(table, sql string) string {
		return strings.Join(queryRows(t, ctx, c, h.Project(), ds.DatasetID, strings.ReplaceAll(sql, "{T}", table)), " / ")
	}
	withSchema := func(more func(*bigquery.FileConfig)) func(*bigquery.FileConfig) {
		return func(fc *bigquery.FileConfig) {
			fc.Schema = schema
			more(fc)
		}
	}
	none := func(*bigquery.FileConfig) {}

	top := `{"i":1,"r":[{"x":"a"}]}` + "\n" + `{"i":2,"zz":5}` + "\n"
	nested := `{"i":1,"r":[{"x":"a"},{"x":"b","y":1}]}` + "\n"
	for _, c := range []struct {
		name, data string
		set        func(*bigquery.FileConfig)
		fails      string // the field named in the failure, or ""
		rows       string
		listed     int
	}{
		{"top", top, withSchema(none), "zz", "", 0},
		{"nested", nested, withSchema(none), "r.y", "", 0},
		{"ignored", top, withSchema(func(fc *bigquery.FileConfig) { fc.IgnoreUnknownValues = true }), "", "1|1 / 2|0", 0},
		{"left_out", top, withSchema(func(fc *bigquery.FileConfig) { fc.MaxBadRecords = 1 }), "", "1|1", 1},
	} {
		for from, src := range sources(c.name+".json", c.data, bigquery.JSON, c.set) {
			table := c.name + "_" + from
			makeTable(table, schema)
			st, err := run(table, src)
			if c.fails != "" {
				if err == nil || !strings.Contains(err.Error(), "No such field: "+c.fails) {
					t.Errorf("%s: the load: %v, want it failed naming %s", table, err, c.fails)
				}
				if n := countRows(t, h, ds.Table(table)); n != 0 {
					t.Errorf("%s: the failed load wrote %d rows", table, n)
				}
				continue
			}
			if err != nil {
				t.Errorf("%s: the load: %v", table, err)
				continue
			}
			if got := read(table, "SELECT i, ARRAY_LENGTH(r) FROM {T} ORDER BY i"); got != c.rows {
				t.Errorf("%s: the rows read back %q, want %q", table, got, c.rows)
			}
			if len(st.Errors) != c.listed {
				t.Errorf("%s: status.errors %v, want %d", table, st.Errors, c.listed)
			}
		}
	}

	// Autodetect: a field first seen after the 500 records sampled.
	var b strings.Builder
	for i := 0; i < 500; i++ {
		fmt.Fprintf(&b, "{\"a\":%d}\n", i)
	}
	late := b.String() + `{"a":500,"late":"x"}` + "\n"
	detect := func(more func(*bigquery.FileConfig)) func(*bigquery.FileConfig) {
		return func(fc *bigquery.FileConfig) {
			fc.AutoDetect = true
			more(fc)
		}
	}
	for from, src := range sources("late.json", late, bigquery.JSON, detect(none)) {
		table := "late_" + from
		if _, err := run(table, src); err == nil || !strings.Contains(err.Error(), "No such field: late") {
			t.Errorf("%s: the load: %v, want it failed naming late", table, err)
		}
	}
	for from, src := range sources("late_ok.json", late, bigquery.JSON, detect(func(fc *bigquery.FileConfig) { fc.IgnoreUnknownValues = true })) {
		table := "late_ok_" + from
		if _, err := run(table, src); err != nil {
			t.Errorf("%s: the load: %v", table, err)
			continue
		}
		md, err := ds.Table(table).Metadata(ctx)
		if err != nil || describeSchema(md.Schema) != "a:INTEGER:NULLABLE" {
			t.Errorf("%s: the detected schema: %+v %v", table, md, err)
		}
		if got := read(table, "SELECT COUNT(*), MAX(a) FROM {T}"); got != "501|500" {
			t.Errorf("%s: the rows read back %q", table, got)
		}
	}

	// CSV: a record with more values than columns.
	csvSchema := bigquery.Schema{{Name: "i", Type: bigquery.IntegerFieldType}, {Name: "s", Type: bigquery.StringFieldType}}
	wide := "1,a\n2,b,extra\n"
	for _, c := range []struct {
		name   string
		ignore bool
		rows   string
	}{{"csv_wide", false, ""}, {"csv_wide_ok", true, "1|a / 2|b"}} {
		set := func(fc *bigquery.FileConfig) { fc.Schema, fc.IgnoreUnknownValues = csvSchema, c.ignore }
		for from, src := range sources(c.name+".csv", wide, bigquery.CSV, set) {
			table := c.name + "_" + from
			makeTable(table, csvSchema)
			_, err := run(table, src)
			if c.rows == "" {
				if err == nil {
					t.Errorf("%s: a record with an extra value loaded", table)
				}
				continue
			}
			if err != nil {
				t.Errorf("%s: the load: %v", table, err)
			} else if got := read(table, "SELECT i, s FROM {T} ORDER BY i"); got != c.rows {
				t.Errorf("%s: the rows read back %q, want %q", table, got, c.rows)
			}
		}
	}
}
