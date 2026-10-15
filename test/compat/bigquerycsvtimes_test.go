//go:build compat

package compat

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"cloud.google.com/go/bigquery"
)

// TestBigQueryCSVLoadTimeFormatsAndEncodings (#1325): a CSV load's
// timeZone, dateFormat, datetimeFormat, timeFormat and timestampFormat
// are read as BigQuery documents them (format elements, "the only
// compatible DATE format"; a TIMESTAMP with no zone is in timeZone), and
// a file in UTF-16BE, UTF-16LE, UTF-32BE or UTF-32LE loads as its text,
// from a local file and from Cloud Storage, read back through the
// official client. A value outside its format fails the load; a C-style
// format is 501.
//
// covers: bigquery.jobs.insert, bigquery.jobs.get, bigquery.tables.insert, bigquery.jobs.query
func TestBigQueryCSVLoadTimeFormatsAndEncodings(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	sc := instanceStorage(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	bucket := sc.Bucket(strings.ReplaceAll(h.Project(), "_", "-") + "-bq-csvtimes")
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
	put := func(name string, data []byte) *bigquery.GCSReference {
		t.Helper()
		w := bucket.Object(name).NewWriter(ctx)
		if _, err := w.Write(data); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		objects = append(objects, name)
		return bigquery.NewGCSReference("gs://" + bucket.BucketName() + "/" + name)
	}
	schema := bigquery.Schema{
		{Name: "d", Type: bigquery.DateFieldType}, {Name: "dt", Type: bigquery.DateTimeFieldType},
		{Name: "tm", Type: bigquery.TimeFieldType}, {Name: "ts", Type: bigquery.TimestampFieldType},
		{Name: "plain", Type: bigquery.TimestampFieldType}, {Name: "note", Type: bigquery.StringFieldType},
	}
	run := func(table string, src bigquery.LoadSource) error {
		if err := ds.Table(table).Create(ctx, &bigquery.TableMetadata{Schema: schema}); err != nil {
			t.Fatalf("create %s: %v", table, err)
		}
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
	read := func(table string) string {
		return strings.Join(queryRows(t, ctx, c, h.Project(), ds.DatasetID, "SELECT CAST(d AS STRING), CAST(dt AS STRING), "+
			"CAST(tm AS STRING), FORMAT_TIMESTAMP('%FT%TZ', ts, 'UTC'), FORMAT_TIMESTAMP('%FT%TZ', plain, 'UTC'), note FROM "+table), " / ")
	}
	formats := func(fc *bigquery.FileConfig) {
		fc.SourceFormat = bigquery.CSV
		fc.SkipLeadingRows = 1
		fc.TimeZone = "America/New_York"
		fc.DateFormat = "MM/DD/YYYY"
		fc.DatetimeFormat = "DD.MM.YYYY HH24:MI"
		fc.TimeFormat = "HH12:MI P.M."
		fc.TimestampFormat = "YYYY/MM/DD HH24:MI:SS"
	}
	data := "d,dt,tm,ts,plain,note\n12/03/2018,03.12.2018 15:30,03:30 P.M.,2020/07/01 12:00:00,2020/07/01 12:00:00,x\n"
	want := "2018-12-03|2018-12-03 15:30:00|15:30:00|2020-07-01T16:00:00Z|2020-07-01T16:00:00Z|x"

	local := bigquery.NewReaderSource(strings.NewReader(data))
	formats(&local.FileConfig)
	if err := run("formats_file", local); err != nil {
		t.Fatalf("a load with the formats from a file: %v", err)
	}
	if got := read("formats_file"); got != want {
		t.Errorf("from a file the rows read back %q, want %q", got, want)
	}
	gcs := put("formats.csv", []byte(data))
	formats(&gcs.FileConfig)
	if err := run("formats_gcs", gcs); err != nil {
		t.Fatalf("a load with the formats from Cloud Storage: %v", err)
	}
	if got := read("formats_gcs"); got != want {
		t.Errorf("from Cloud Storage the rows read back %q, want %q", got, want)
	}

	// timeZone alone: a TIMESTAMP with no zone is in it, one with a zone
	// keeps its own.
	zoned := bigquery.NewReaderSource(strings.NewReader("2020-01-02,2020-01-02 03:04:05,03:04:05," +
		"2020-07-01 12:00:00,2020-07-01 12:00:00+02:00,y\n"))
	zoned.SourceFormat, zoned.TimeZone = bigquery.CSV, "Asia/Tokyo"
	if err := run("zone", zoned); err != nil {
		t.Fatalf("a load with timeZone: %v", err)
	}
	if got, want := read("zone"), "2020-01-02|2020-01-02 03:04:05|03:04:05|2020-07-01T03:00:00Z|2020-07-01T10:00:00Z|y"; got != want {
		t.Errorf("with timeZone the rows read back %q, want %q", got, want)
	}

	bad := bigquery.NewReaderSource(strings.NewReader("2018-12-03,,,,,z\n"))
	bad.SourceFormat, bad.DateFormat = bigquery.CSV, "MM/DD/YYYY"
	if err := run("bad_date", bad); err == nil {
		t.Error("a DATE outside dateFormat loaded")
	}
	cstyle := bigquery.NewReaderSource(strings.NewReader("12/03/2018,,,,,z\n"))
	cstyle.SourceFormat, cstyle.DateFormat = bigquery.CSV, "%m/%d/%Y"
	wantHTTPStatus(t, "a C-style dateFormat", run("cstyle", cstyle), http.StatusNotImplemented)

	// The encodings: \u00e9 and an emoji, with a byte-order mark.
	text := "\ufeff2020-01-02,,,,,caf\u00e9 \U0001F600\n"
	encode := map[bigquery.Encoding]func() []byte{
		"UTF-16BE": func() []byte { return utf16Bytes(text, true) },
		"UTF-16LE": func() []byte { return utf16Bytes(text, false) },
		"UTF-32BE": func() []byte { return utf32Bytes(text, true) },
		"UTF-32LE": func() []byte { return utf32Bytes(text, false) },
	}
	for enc, data := range encode {
		name := strings.ToLower(strings.ReplaceAll(string(enc), "-", "_"))
		local := bigquery.NewReaderSource(strings.NewReader(string(data())))
		local.SourceFormat, local.Encoding = bigquery.CSV, enc
		ref := put(name+".csv", data())
		ref.SourceFormat, ref.Encoding = bigquery.CSV, enc
		for table, src := range map[string]bigquery.LoadSource{name + "_file": local, name + "_gcs": ref} {
			if err := run(table, src); err != nil {
				t.Errorf("%s: %v", table, err)
				continue
			}
			if got := read(table); got != "2020-01-02|<nil>|<nil>|<nil>|<nil>|caf\u00e9 \U0001F600" {
				t.Errorf("%s: the rows read back %q", table, got)
			}
		}
	}
}

func utf16Bytes(s string, big bool) []byte {
	var out []byte
	for _, u := range utf16.Encode([]rune(s)) {
		if big {
			out = append(out, byte(u>>8), byte(u))
		} else {
			out = append(out, byte(u), byte(u>>8))
		}
	}
	return out
}

func utf32Bytes(s string, big bool) []byte {
	var out []byte
	for _, r := range s {
		b := []byte{byte(r >> 24), byte(r >> 16), byte(r >> 8), byte(r)}
		if !big {
			b[0], b[1], b[2], b[3] = b[3], b[2], b[1], b[0]
		}
		out = append(out, b...)
	}
	return out
}
