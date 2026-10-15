//go:build compat

package compat

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
)

// TestBigQueryJSONAutodetectLoad (#1324): a NEWLINE_DELIMITED_JSON load
// with autodetect into a new table, from a local file and from Cloud
// Storage, makes the table with the schema BigQuery documents detecting
// (https://cloud.google.com/bigquery/docs/schema-detect): BOOLEAN,
// INTEGER, FLOAT, DATE, TIME and TIMESTAMP from JSON values and from
// strings that hold them, STRING otherwise and for a column only ever
// null, a RECORD from an object, a REPEATED column from an array, every
// column NULLABLE; and loads every row. A mix of types BigQuery does not
// document detecting is 501, with no table left.
//
// covers: bigquery.jobs.insert, bigquery.jobs.get, bigquery.tables.get, bigquery.jobs.query
func TestBigQueryJSONAutodetectLoad(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	sc := instanceStorage(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	bucket := sc.Bucket(strings.ReplaceAll(h.Project(), "_", "-") + "-bq-detect")
	if err := bucket.Create(ctx, h.Project(), nil); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	t.Cleanup(func() {
		_ = bucket.Object("rows.json").Delete(context.Background())
		_ = bucket.Delete(context.Background())
	})
	data := `{"id":"12","name":"ann","score":1,"ok":true,"day":"2020-01-02","clock":"12:34:56","ts":"2020-01-02 03:04:05 -05:00","addr":{"city":"Oslo","zip":"0150"},"tags":["a","b"],"none":null}
{"id":13,"name":"bob","score":2.5,"ok":"false","day":"2020-01-03","ts":"2020-01-02T03:04Z","addr":{"city":"Rome"},"tags":[]}
`
	w := bucket.Object("rows.json").NewWriter(ctx)
	if _, err := w.Write([]byte(data)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("write object: %v", err)
	}
	run := func(table string, src bigquery.LoadSource) error {
		lctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		job, err := ds.Table(table).LoaderFrom(src).Run(lctx)
		if err != nil {
			return err
		}
		st, err := job.Wait(lctx)
		if err == nil {
			err = st.Err()
		}
		return err
	}
	local := func(data string) bigquery.LoadSource {
		s := bigquery.NewReaderSource(strings.NewReader(data))
		s.SourceFormat, s.AutoDetect = bigquery.JSON, true
		return s
	}
	gcs := bigquery.NewGCSReference("gs://" + bucket.BucketName() + "/rows.json")
	gcs.SourceFormat, gcs.AutoDetect = bigquery.JSON, true

	want := "id:INTEGER:NULLABLE name:STRING:NULLABLE score:FLOAT:NULLABLE ok:BOOLEAN:NULLABLE day:DATE:NULLABLE " +
		"clock:TIME:NULLABLE ts:TIMESTAMP:NULLABLE addr:RECORD:NULLABLE(city:STRING:NULLABLE zip:INTEGER:NULLABLE) " +
		"tags:STRING:REPEATED none:STRING:NULLABLE"
	for table, src := range map[string]bigquery.LoadSource{"from_file": local(data), "from_gcs": gcs} {
		if err := run(table, src); err != nil {
			t.Fatalf("%s: the load: %v", table, err)
		}
		md, err := ds.Table(table).Metadata(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if got := describeSchema(md.Schema); got != want {
			t.Errorf("%s: the detected schema is\n %s\nwant\n %s", table, got, want)
		}
		rows := queryRows(t, ctx, c, h.Project(), ds.DatasetID, "SELECT id, name, score, ok, day, clock, "+
			"FORMAT_TIMESTAMP('%FT%TZ', ts, 'UTC'), addr.city, addr.zip, ARRAY_LENGTH(tags), none IS NULL FROM "+table)
		if got := strings.Join(rows, " / "); got != "12|ann|1|true|2020-01-02|12:34:56|2020-01-02T08:04:05Z|Oslo|150|2|true / "+
			"13|bob|2.5|false|2020-01-03|<nil>|2020-01-02T03:04:00Z|Rome|<nil>|0|true" {
			t.Errorf("%s: the rows read back %q", table, got)
		}
	}

	err := run("mixed", local(`{"a":1}`+"\n"+`{"a":"x"}`+"\n"))
	wantHTTPStatus(t, "a column of numbers and text", err, http.StatusNotImplemented)
	if tableExists(t, ctx, ds.Table("mixed")) {
		t.Error("the refused load left its table")
	}
}

// describeSchema writes a schema as name:TYPE:MODE, a RECORD's fields in
// parentheses.
func describeSchema(s bigquery.Schema) string {
	parts := make([]string, len(s))
	for i, f := range s {
		mode := "NULLABLE"
		switch {
		case f.Repeated:
			mode = "REPEATED"
		case f.Required:
			mode = "REQUIRED"
		}
		parts[i] = fmt.Sprintf("%s:%s:%s", f.Name, f.Type, mode)
		if len(f.Schema) > 0 {
			parts[i] += "(" + describeSchema(f.Schema) + ")"
		}
	}
	return strings.Join(parts, " ")
}
