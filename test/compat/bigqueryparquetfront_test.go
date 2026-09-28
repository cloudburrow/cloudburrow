//go:build compat

package compat

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	"cloud.google.com/go/civil"
	bq "google.golang.org/api/bigquery/v2"
	"google.golang.org/api/googleapi"
)

// The Parquet loads the front carries out itself (#1004, #1005, #1006):
// it reads the file's rows and inserts them, because the pinned emulator's
// own Parquet load gets these wrong. Measured first, through the official
// Go client and against the image directly, each given BigQuery's schema
// in the job: TIME, TIMESTAMP(MILLIS), BYTES and INT96 loaded wrong or
// were refused (#988); a LIST read without list inference failed "failed
// to convert struct from string"; with list inference a NULL list read
// back NULL; a MAP left the emulator hung until it was restarted; and a
// file's A and B loaded NULL into a and b.

func loadedSchema(t *testing.T, ctx context.Context, tbl *bigquery.Table) string {
	t.Helper()
	md, err := tbl.Metadata(ctx)
	if err != nil {
		t.Fatalf("%s: %v", tbl.TableID, err)
	}
	var describe func(bigquery.Schema) string
	describe = func(s bigquery.Schema) string {
		var parts []string
		for _, f := range s {
			d := f.Name + " " + string(f.Type)
			switch {
			case f.Repeated:
				d += " REPEATED"
			case f.Required:
				d += " REQUIRED"
			}
			if len(f.Schema) > 0 {
				d += " (" + describe(f.Schema) + ")"
			}
			parts = append(parts, d)
		}
		return strings.Join(parts, ", ")
	}
	return describe(md.Schema)
}

func loadStats(t *testing.T, ctx context.Context, l *bigquery.Loader) *bigquery.LoadStatistics {
	t.Helper()
	lctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	job, err := l.Run(lctx)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	st, err := job.Wait(lctx)
	if err != nil || st.Err() != nil {
		t.Fatalf("load: %v %v", err, st.Err())
	}
	ls, _ := st.Statistics.Details.(*bigquery.LoadStatistics)
	if ls == nil {
		t.Fatalf("load: no load statistics in %+v", st.Statistics)
	}
	// jobs.get reads the same job back.
	again, err := job.Status(lctx)
	if err != nil || again.State != bigquery.Done || again.Err() != nil {
		t.Errorf("jobs.get of the load: %+v %v", again, err)
	}
	if cfg, err := job.Config(); err != nil {
		t.Errorf("the load's configuration: %v", err)
	} else if lc, ok := cfg.(*bigquery.LoadConfig); !ok {
		t.Errorf("the load's configuration is %T", cfg)
	} else if src, ok := lc.Src.(*bigquery.ReaderSource); ok && src.SourceFormat != bigquery.Parquet {
		t.Errorf("the load's source format is %q", src.SourceFormat)
	}
	return ls
}

// TestBigQueryParquetConvertedTypes (#1005): the Parquet types the
// emulator loads wrongly, written by Apache Arrow, load with the type
// BigQuery's conversion table gives each, and read back as written, NULLs
// included: TIME(MILLIS) and TIME(MICROS), TIMESTAMP(MILLIS), BYTE_ARRAY
// and FIXED_LEN_BYTE_ARRAY BYTES (0xFF too), unsigned INT64 up to INT64's
// maximum, FLOAT and DOUBLE with the infinities, INT96, DECIMAL as INT32,
// INT64 and fixed bytes into NUMERIC and, by decimalTargetTypes,
// BIGNUMERIC, and ENUM as BYTES and, with enumAsString, STRING. An
// unsigned INT64 above INT64's maximum fails the job, "invalid", and
// makes no table.
func TestBigQueryParquetConvertedTypes(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	q := func(sql string) string {
		return fmt.Sprint(rows(t, ctx, c, strings.ReplaceAll(sql, "DS.", ds.DatasetID+".")))
	}

	conv := ds.Table("converted")
	ls := loadStats(t, ctx, conv.LoaderFrom(parquetUpload(parquetFixture(t, "converted.parquet"), nil)))
	if ls.OutputRows != 3 || ls.InputFiles != 1 || ls.InputFileBytes != int64(len(parquetFixture(t, "converted.parquet"))) {
		t.Errorf("statistics: %+v", ls)
	}
	if got, want := loadedSchema(t, ctx, conv), "id INTEGER, time_ms TIME, time_us TIME, ts_ms TIMESTAMP, bytes BYTES, "+
		"fixed BYTES, uint64 INTEGER, float FLOAT, double FLOAT"; got != want {
		t.Errorf("schema:\n got %s\nwant %s", got, want)
	}
	if got, want := q("SELECT id, FORMAT_TIME('%H:%M:%E6S', time_ms), FORMAT_TIME('%H:%M:%E6S', time_us), "+
		"FORMAT_TIMESTAMP('%Y-%m-%d %H:%M:%E6S', ts_ms), TO_HEX(bytes), TO_HEX(fixed), uint64, CAST(float AS STRING), "+
		"CAST(double AS STRING) FROM DS.converted ORDER BY id"),
		"[[1 01:02:03.004000 01:02:03.004567 2024-01-02 03:04:05.123000 0001 6162 1 1.5 0.1] "+
			"[2 23:59:59.999000 00:00:00.000000 1969-12-31 23:59:59.999000 ff ff00 9223372036854775807 +Inf -Inf] "+
			"[3 <nil> <nil> <nil> <nil> <nil> <nil> <nil> <nil>]]"; got != want {
		t.Errorf("rows:\n got %s\nwant %s", got, want)
	}
	// Read as the table's rows (tabledata.list), typed by the client.
	it := conv.Read(ctx)
	var first []bigquery.Value
	if err := it.Next(&first); err != nil {
		t.Fatal(err)
	}
	if tm, ok := first[1].(civil.Time); !ok || (tm != civil.Time{Hour: 1, Minute: 2, Second: 3, Nanosecond: 4000000} &&
		tm != civil.Time{Hour: 23, Minute: 59, Second: 59, Nanosecond: 999000000}) {
		t.Errorf("tabledata.list: time_ms %T %v", first[1], first[1])
	}
	if b, ok := first[4].([]byte); !ok || (string(b) != "\x00\x01" && string(b) != "\xff") {
		t.Errorf("tabledata.list: bytes %T %v", first[4], first[4])
	}

	for _, c2 := range []struct {
		file    string
		decimal []bigquery.DecimalTargetType
		enum    bool
		schema  string
		sql     string
		want    string
	}{
		{"int96.parquet", nil, false, "t TIMESTAMP", "SELECT FORMAT_TIMESTAMP('%Y-%m-%d %H:%M:%E6S', t) FROM DS.x",
			"[[2024-01-02 03:04:05.123456] [2024-01-02 03:04:05.123456]]"},
		{"decimal.parquet", nil, false, "d NUMERIC", "SELECT CAST(d AS STRING) FROM DS.x ORDER BY d", "[[-2.5] [1.25]]"},
		{"decimal_int.parquet", nil, false, "d32 NUMERIC, d64 NUMERIC", "SELECT CAST(d32 AS STRING), CAST(d64 AS STRING) FROM DS.x ORDER BY d32",
			"[[<nil> <nil>] [-2.5 -0.000001] [1.25 123456789012.345678]]"},
		{"decimal_wide.parquet", []bigquery.DecimalTargetType{bigquery.NumericTargetType, bigquery.BigNumericTargetType}, false,
			"d BIGNUMERIC", "SELECT CAST(d AS STRING) FROM DS.x ORDER BY d", "[[-12345678901234567890.123456789] [1.5]]"},
		{"decimal256.parquet", []bigquery.DecimalTargetType{bigquery.BigNumericTargetType}, false, "d BIGNUMERIC",
			"SELECT CAST(d AS STRING) FROM DS.x ORDER BY d", "[[-1] [12345678901234567890123456789012345678.12]]"},
		{"enum.parquet", nil, false, "e BYTES", "SELECT TO_HEX(e) FROM DS.x ORDER BY e", "[[<nil>] [] [726564]]"},
		{"enum.parquet", nil, true, "e STRING", "SELECT e FROM DS.x ORDER BY e", "[[<nil>] [] [red]]"},
	} {
		name := strings.TrimSuffix(c2.file, ".parquet")
		if c2.enum {
			name += "_s"
		}
		tbl := ds.Table("t_" + name)
		src := bigquery.NewReaderSource(strings.NewReader(parquetFixture(t, c2.file)))
		src.SourceFormat = bigquery.Parquet
		if c2.enum {
			src.ParquetOptions = &bigquery.ParquetOptions{EnumAsString: true}
		}
		l := tbl.LoaderFrom(src)
		l.DecimalTargetTypes = c2.decimal
		if err := runLoad(ctx, l); err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if got := loadedSchema(t, ctx, tbl); got != c2.schema {
			t.Errorf("%s: schema %s, want %s", name, got, c2.schema)
		}
		if got := q(strings.ReplaceAll(c2.sql, "DS.x", "DS."+tbl.TableID)); got != c2.want {
			t.Errorf("%s:\n got %s\nwant %s", name, got, c2.want)
		}
	}

	big := ds.Table("uint64_big")
	err := runLoad(ctx, big.LoaderFrom(parquetUpload(parquetFixture(t, "uint64_big.parquet"), nil)))
	var be *bigquery.Error
	if !errors.As(err, &be) || be.Reason != "invalid" || !strings.Contains(be.Message, "exceeds the maximum INTEGER value") {
		t.Errorf("an unsigned INT64 above the maximum: %v", err)
	}
	if tableExists(t, ctx, big) {
		t.Error("an unsigned INT64 above the maximum: the table was made")
	}
}

// TestBigQueryParquetNestedColumns (#1004): groups, LIST and MAP columns,
// written by Apache Arrow with NULLs at each level, load with the schema
// BigQuery's documentation gives them and read back as written: without
// list inference a group is a RECORD and a LIST or MAP a RECORD holding a
// REPEATED RECORD; with parquetOptions.enableListInference a LIST is a
// REPEATED column, a NULL list an empty one; with mapTargetType
// ARRAY_OF_STRUCT (through the generated client, as the Go client has no
// such field) a MAP is a REPEATED RECORD of key and value. From an upload
// and from Cloud Storage. With list inference a NULL element, and a LIST
// of LISTs, are 501 and make no table.
func TestBigQueryParquetNestedColumns(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	nested := parquetFixture(t, "nested.parquet")
	json := func(table string) string {
		return fmt.Sprint(rows(t, ctx, c, "SELECT TO_JSON_STRING(t) FROM "+ds.DatasetID+"."+table+" AS t ORDER BY t.id"))
	}

	plain := ds.Table("plain")
	if err := runLoad(ctx, plain.LoaderFrom(parquetUpload(nested, nil))); err != nil {
		t.Fatalf("without inference: %v", err)
	}
	if got, want := loadedSchema(t, ctx, plain), "id INTEGER, s RECORD (x INTEGER, y STRING, z RECORD (q BOOLEAN)), "+
		"l RECORD (list RECORD REPEATED (element INTEGER)), ls RECORD (list RECORD REPEATED (element RECORD (x INTEGER, y STRING))), "+
		"m RECORD (key_value RECORD REPEATED (key STRING REQUIRED, value INTEGER)), "+
		"sl RECORD (l RECORD (list RECORD REPEATED (element INTEGER)))"; got != want {
		t.Errorf("without inference, schema:\n got %s\nwant %s", got, want)
	}
	if got, want := json("plain"), `[[{"id":1,"s":{"x":1,"y":"a","z":{"q":true}},"l":{"list":[{"element":1},{"element":2}]},`+
		`"ls":{"list":[{"element":{"x":1,"y":"a"}},{"element":{"x":2,"y":null}}]},"m":{"key_value":[{"key":"a","value":1},{"key":"b","value":null}]},`+
		`"sl":{"l":{"list":[{"element":1},{"element":2}]}}}] `+
		`[{"id":2,"s":{"x":null,"y":"b","z":null},"l":{"list":[]},"ls":{"list":[]},"m":{"key_value":[]},"sl":{"l":null}}] `+
		`[{"id":3,"s":null,"l":null,"ls":null,"m":null,"sl":null}] `+
		`[{"id":4,"s":{"x":4,"y":null,"z":{"q":null}},"l":{"list":[{"element":3}]},"ls":{"list":[{"element":{"x":3,"y":"c"}}]},`+
		`"m":{"key_value":[{"key":"c","value":3}]},"sl":{"l":{"list":[]}}}]]`; got != want {
		t.Errorf("without inference, rows:\n got %s\nwant %s", got, want)
	}

	inferred := ds.Table("inferred")
	src := bigquery.NewReaderSource(strings.NewReader(nested))
	src.SourceFormat = bigquery.Parquet
	src.ParquetOptions = &bigquery.ParquetOptions{EnableListInference: true}
	if err := runLoad(ctx, inferred.LoaderFrom(src)); err != nil {
		t.Fatalf("with list inference: %v", err)
	}
	if got, want := loadedSchema(t, ctx, inferred), "id INTEGER, s RECORD (x INTEGER, y STRING, z RECORD (q BOOLEAN)), "+
		"l INTEGER REPEATED, ls RECORD REPEATED (x INTEGER, y STRING), "+
		"m RECORD (key_value RECORD REPEATED (key STRING REQUIRED, value INTEGER)), sl RECORD (l INTEGER REPEATED)"; got != want {
		t.Errorf("with list inference, schema:\n got %s\nwant %s", got, want)
	}
	if got, want := json("inferred"), `[[{"id":1,"s":{"x":1,"y":"a","z":{"q":true}},"l":[1,2],"ls":[{"x":1,"y":"a"},{"x":2,"y":null}],`+
		`"m":{"key_value":[{"key":"a","value":1},{"key":"b","value":null}]},"sl":{"l":[1,2]}}] `+
		`[{"id":2,"s":{"x":null,"y":"b","z":null},"l":[],"ls":[],"m":{"key_value":[]},"sl":{"l":[]}}] `+
		`[{"id":3,"s":null,"l":[],"ls":[],"m":null,"sl":null}] `+
		`[{"id":4,"s":{"x":4,"y":null,"z":{"q":null}},"l":[3],"ls":[{"x":3,"y":"c"}],"m":{"key_value":[{"key":"c","value":3}]},"sl":{"l":[]}}]]`; got != want {
		t.Errorf("with list inference, rows:\n got %s\nwant %s", got, want)
	}
	// The client reads the REPEATED and RECORD values (tabledata.list).
	it := inferred.Read(ctx)
	var row []bigquery.Value
	if err := it.Next(&row); err != nil {
		t.Fatal(err)
	}
	if l, ok := row[2].([]bigquery.Value); !ok || fmt.Sprint(l) != "[1 2]" {
		t.Errorf("tabledata.list: l %T %v", row[2], row[2])
	}

	// mapTargetType ARRAY_OF_STRUCT, through the generated client.
	svc := generatedBigQuery(t, h)
	job := &bq.Job{Configuration: &bq.JobConfiguration{Load: &bq.JobConfigurationLoad{
		SourceFormat:     "PARQUET",
		DestinationTable: &bq.TableReference{ProjectId: project, DatasetId: ds.DatasetID, TableId: "maps"},
		ParquetOptions:   &bq.ParquetOptions{MapTargetType: "ARRAY_OF_STRUCT"},
	}}}
	got, err := svc.Jobs.Insert(project, job).Media(strings.NewReader(nested), googleapi.ContentType("application/octet-stream")).Context(ctx).Do()
	if err != nil || got.Status == nil || got.Status.State != "DONE" || got.Status.ErrorResult != nil {
		t.Fatalf("ARRAY_OF_STRUCT: %+v %v", got, err)
	}
	if s := loadedSchema(t, ctx, ds.Table("maps")); !strings.Contains(s, "m RECORD REPEATED (key STRING REQUIRED, value INTEGER)") {
		t.Errorf("ARRAY_OF_STRUCT, schema %s", s)
	}
	if got, want := fmt.Sprint(rows(t, ctx, c, "SELECT id, TO_JSON_STRING(m) FROM "+ds.DatasetID+".maps ORDER BY id")),
		`[[1 [{"key":"a","value":1},{"key":"b","value":null}]] [2 []] [3 []] [4 [{"key":"c","value":3}]]]`; got != want {
		t.Errorf("ARRAY_OF_STRUCT, rows %s, want %s", got, want)
	}

	// From Cloud Storage.
	bucket := loadBucket(t, h, "bq-parquet-nested", map[string]string{"nested.parquet": nested})
	gcs := bigquery.NewGCSReference("gs://" + bucket.BucketName() + "/nested.parquet")
	gcs.SourceFormat = bigquery.Parquet
	if err := runLoad(ctx, ds.Table("from_gcs").LoaderFrom(gcs)); err != nil {
		t.Fatalf("from Cloud Storage: %v", err)
	}
	if got, want := json("from_gcs"), json("plain"); got != want {
		t.Errorf("from Cloud Storage:\n got %s\nwant %s", got, want)
	}

	for _, c2 := range []struct{ file, msg string }{
		{"list_null.parquet", "a NULL element"},
		{"list_list.parquet", "LIST of lists"},
	} {
		tbl := ds.Table("t_" + strings.TrimSuffix(c2.file, ".parquet"))
		s := bigquery.NewReaderSource(strings.NewReader(parquetFixture(t, c2.file)))
		s.SourceFormat = bigquery.Parquet
		s.ParquetOptions = &bigquery.ParquetOptions{EnableListInference: true}
		err := runLoad(ctx, tbl.LoaderFrom(s))
		wantReason(t, c2.file, err, http.StatusNotImplemented, "notImplemented")
		if err == nil || !strings.Contains(err.Error(), c2.msg) {
			t.Errorf("%s: %v, want %q", c2.file, err, c2.msg)
		}
		if tableExists(t, ctx, tbl) {
			t.Errorf("%s: the table was made", c2.file)
		}
	}
}

// TestBigQueryParquetSchemaChanges (#1006): a Parquet load that changes its
// table's schema does so as BigQuery documents, through the official Go
// client: a file's columns named as the table's in another case load into
// them; ALLOW_FIELD_ADDITION adds the file's new column at the end, NULL
// in the table's rows, and the table keeps its description and labels;
// ALLOW_FIELD_RELAXATION makes a REQUIRED column NULLABLE; WRITE_TRUNCATE
// gives the table the file's schema; WRITE_TRUNCATE_DATA replaces the rows
// and keeps the table's schema.
func TestBigQueryParquetSchemaChanges(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	i, s := bigquery.IntegerFieldType, bigquery.StringFieldType
	q := func(sql string) string {
		return fmt.Sprint(rows(t, ctx, c, strings.ReplaceAll(sql, "DS.", ds.DatasetID+".")))
	}
	table := func(name string, schema bigquery.Schema) *bigquery.Table {
		tbl := ds.Table(name)
		if err := tbl.Create(ctx, &bigquery.TableMetadata{Schema: schema, Description: "kept", Labels: map[string]string{"k": "v"}}); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Query("INSERT " + ds.DatasetID + "." + name + " (a, b) VALUES (0, 'old')").Read(ctx); err != nil {
			t.Fatal(err)
		}
		return tbl
	}
	ab := bigquery.Schema{{Name: "a", Type: i}, {Name: "b", Type: s}}

	upper := table("upper", ab)
	if ls := loadStats(t, ctx, upper.LoaderFrom(parquetUpload(parquetFixture(t, "upper.parquet"), nil))); ls.OutputRows != 3 {
		t.Errorf("names in another case: %+v", ls)
	}
	if got := q("SELECT a, b FROM DS.upper ORDER BY a"); got != "[[0 old] [1 x] [2 y] [3 z]]" {
		t.Errorf("names in another case: %s", got)
	}

	added := table("added", ab)
	l := added.LoaderFrom(parquetUpload(parquetFixture(t, "abc.parquet"), nil))
	l.SchemaUpdateOptions = []string{"ALLOW_FIELD_ADDITION"}
	if err := runLoad(ctx, l); err != nil {
		t.Fatalf("ALLOW_FIELD_ADDITION: %v", err)
	}
	if got := loadedSchema(t, ctx, added); got != "a INTEGER, b STRING, c BOOLEAN" {
		t.Errorf("ALLOW_FIELD_ADDITION: schema %s", got)
	}
	if got := q("SELECT a, b, c FROM DS.added ORDER BY a"); got != "[[0 old <nil>] [4 w true]]" {
		t.Errorf("ALLOW_FIELD_ADDITION: %s", got)
	}
	if md, err := added.Metadata(ctx); err != nil || md.Description != "kept" || md.Labels["k"] != "v" {
		t.Errorf("ALLOW_FIELD_ADDITION: the table is now %+v %v", md, err)
	}

	relaxed := table("relaxed", bigquery.Schema{{Name: "a", Type: i, Required: true}, {Name: "b", Type: s}})
	l = relaxed.LoaderFrom(parquetUpload(parquetFixture(t, "ab.parquet"), nil))
	l.SchemaUpdateOptions = []string{"ALLOW_FIELD_RELAXATION"}
	if err := runLoad(ctx, l); err != nil {
		t.Fatalf("ALLOW_FIELD_RELAXATION: %v", err)
	}
	if got := loadedSchema(t, ctx, relaxed); got != "a INTEGER, b STRING" {
		t.Errorf("ALLOW_FIELD_RELAXATION: schema %s", got)
	}
	if got := q("SELECT COUNT(*) FROM DS.relaxed"); got != "[[4]]" {
		t.Errorf("ALLOW_FIELD_RELAXATION: %s rows", got)
	}

	replaced := table("replaced", ab)
	l = replaced.LoaderFrom(parquetUpload(parquetFixture(t, "ab_required.parquet"), nil))
	l.WriteDisposition = bigquery.WriteTruncate
	if err := runLoad(ctx, l); err != nil {
		t.Fatalf("WRITE_TRUNCATE: %v", err)
	}
	if got := loadedSchema(t, ctx, replaced); got != "a INTEGER REQUIRED, b STRING REQUIRED" {
		t.Errorf("WRITE_TRUNCATE: schema %s", got)
	}
	if got := q("SELECT a, b FROM DS.replaced ORDER BY a"); got != "[[1 x] [2 y] [3 z]]" {
		t.Errorf("WRITE_TRUNCATE: %s", got)
	}

	data := table("data", ab)
	l = data.LoaderFrom(parquetUpload(parquetFixture(t, "upper.parquet"), nil))
	l.WriteDisposition = bigquery.TableWriteDisposition("WRITE_TRUNCATE_DATA")
	if err := runLoad(ctx, l); err != nil {
		t.Fatalf("WRITE_TRUNCATE_DATA: %v", err)
	}
	if got := loadedSchema(t, ctx, data); got != "a INTEGER, b STRING" {
		t.Errorf("WRITE_TRUNCATE_DATA: schema %s", got)
	}
	if got := q("SELECT a, b FROM DS.data ORDER BY a"); got != "[[1 x] [2 y] [3 z]]" {
		t.Errorf("WRITE_TRUNCATE_DATA: %s", got)
	}
}
