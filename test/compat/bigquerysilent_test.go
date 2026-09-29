//go:build compat

package compat

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	bq "google.golang.org/api/bigquery/v2"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
)

// twoDatasets makes two datasets for a test, removed with their contents
// when it ends.
func twoDatasets(t *testing.T, h *Harness, c *bigquery.Client) (*bigquery.Dataset, *bigquery.Dataset) {
	t.Helper()
	base := strings.ReplaceAll(h.Project(), "-", "_")
	var out []*bigquery.Dataset
	for _, s := range []string{"_one", "_two"} {
		ds := c.Dataset(base + s)
		if err := ds.Create(h.Context(), nil); err != nil {
			t.Fatalf("create dataset %s: %v", ds.DatasetID, err)
		}
		t.Cleanup(func() { _ = ds.DeleteWithContents(context.Background()) })
		out = append(out, ds)
	}
	return out[0], out[1]
}

// readTable reads every row of tbl through Table.Read (tabledata.list),
// pageSize rows a page when it is not 0, each row's values joined by
// "|", sorted.
func readTable(t *testing.T, ctx context.Context, tbl *bigquery.Table, pageSize int) []string {
	t.Helper()
	it := tbl.Read(ctx)
	if pageSize > 0 {
		it.PageInfo().MaxSize = pageSize
	}
	return readRows(t, it, tbl.FullyQualifiedName())
}

func readRows(t *testing.T, it *bigquery.RowIterator, what string) []string {
	t.Helper()
	var rows []string
	for {
		var row []bigquery.Value
		err := it.Next(&row)
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatalf("read %s: %v", what, err)
		}
		parts := make([]string, len(row))
		for i, v := range row {
			parts[i] = fmt.Sprint(v)
		}
		rows = append(rows, strings.Join(parts, "|"))
	}
	sort.Strings(rows)
	return rows
}

// queryRows runs sql through jobs.query with dataset as its default
// dataset ("" for none) and returns its rows as readRows does.
func queryRows(t *testing.T, ctx context.Context, c *bigquery.Client, project, dataset, sql string) []string {
	t.Helper()
	q := c.Query(sql)
	if dataset != "" {
		q.DefaultProjectID, q.DefaultDatasetID = project, dataset
	}
	it, err := q.Read(ctx)
	if err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return readRows(t, it, sql)
}

// runIn runs sql with dataset as its default dataset, through jobs.query,
// or (insert) as a query job, and returns its error.
func runIn(ctx context.Context, c *bigquery.Client, project, dataset, sql string, insert bool) error {
	qctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	q := c.Query(sql)
	if dataset != "" {
		q.DefaultProjectID, q.DefaultDatasetID = project, dataset
	}
	if !insert {
		_, err := q.Read(qctx)
		return err
	}
	job, err := q.Run(qctx)
	if err != nil {
		return err
	}
	st, err := job.Wait(qctx)
	if err != nil {
		return err
	}
	return st.Err()
}

// TestBigQuerySameTableIDInTwoDatasets (#1015), through the official Go
// client: two datasets each with a table named same, of other columns and
// rows, are each read and written as themselves. Measured first against
// the pinned emulator: tabledata.list (Table.Read) of the second table
// read the first one's rows; a query with the second dataset as its
// default dataset read the first table, an INSERT failed "Column a is not
// present in table ..._same", a DELETE and a TRUNCATE emptied the first
// table; a query with no default dataset read it too.
//
// covers: bigquery.tabledata.list
func TestBigQuerySameTableIDInTwoDatasets(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	// Each step has a context of its own (h.Context, 60 seconds), not one
	// for the whole test (#1051): late in the BigQuery suite each of its
	// many reads was slow (tabledata.list's sharedID asked tables.get of
	// every dataset, #1017, until #1063 kept the answer), and together
	// they outran one 60-second context where no step did.
	ctx := h.Context()
	one, two := twoDatasets(t, h, c)

	// The first table through tables.insert, the second by DDL.
	first := one.Table("same")
	if err := first.Create(ctx, &bigquery.TableMetadata{Schema: bigquery.Schema{{Name: "s", Type: bigquery.StringFieldType}}}); err != nil {
		t.Fatalf("Table.Create: %v", err)
	}
	if err := first.Inserter().Put(ctx, []*bigquery.ValuesSaver{{Schema: bigquery.Schema{{Name: "s", Type: bigquery.StringFieldType}}, Row: []bigquery.Value{"from one"}}}); err != nil {
		t.Fatalf("Inserter.Put: %v", err)
	}
	second := two.Table("same")
	if err := bqRun(ctx, c, "CREATE TABLE "+two.DatasetID+".same (a INT64, b STRING)", false); err != nil {
		t.Fatal(err)
	}
	if err := bqRun(ctx, c, "INSERT INTO "+two.DatasetID+".same (a, b) VALUES (1, 'x'), (2, 'y'), (3, 'z')", false); err != nil {
		t.Fatal(err)
	}
	wantOne := []string{"from one"}
	check := func(when string, wantTwo []string) {
		t.Helper()
		began := time.Now()
		defer func() { t.Logf("%s: the reads took %v", when, time.Since(began).Round(time.Millisecond)) }()
		if got := readTable(t, h.Context(), first, 0); !reflect.DeepEqual(got, wantOne) {
			t.Errorf("%s: Table.Read of %s: %q, want %q", when, first.FullyQualifiedName(), got, wantOne)
		}
		if got := readTable(t, h.Context(), second, 0); !reflect.DeepEqual(got, wantTwo) {
			t.Errorf("%s: Table.Read of %s: %q, want %q", when, second.FullyQualifiedName(), got, wantTwo)
		}
		// Paged, a row a page (maxResults and pageToken).
		if got := readTable(t, h.Context(), second, 1); !reflect.DeepEqual(got, wantTwo) {
			t.Errorf("%s: Table.Read of %s a row a page: %q, want %q", when, second.FullyQualifiedName(), got, wantTwo)
		}
		if got := queryRows(t, h.Context(), c, project, two.DatasetID, "SELECT a, b FROM same"); !reflect.DeepEqual(got, wantTwo) {
			t.Errorf("%s: SELECT with %s the default dataset: %q, want %q", when, two.DatasetID, got, wantTwo)
		}
		if got := queryRows(t, h.Context(), c, project, one.DatasetID, "SELECT * FROM same"); !reflect.DeepEqual(got, wantOne) {
			t.Errorf("%s: SELECT with %s the default dataset: %q, want %q", when, one.DatasetID, got, wantOne)
		}
	}
	check("made", []string{"1|x", "2|y", "3|z"})

	// Written with the second dataset the default one: DML through
	// jobs.query and as query jobs.
	for _, s := range []struct {
		sql    string
		insert bool
	}{
		{"INSERT INTO same (a, b) VALUES (4, 'w')", false},
		{"UPDATE same SET b = 'Y' WHERE a = 2", true},
		{"DELETE FROM same WHERE a = 1", false},
		{"CREATE TABLE src AS SELECT 5 AS a, 'v' AS b", false},
		{"MERGE same t USING src s ON t.a = s.a WHEN NOT MATCHED THEN INSERT (a, b) VALUES (s.a, s.b)", true},
	} {
		if err := runIn(h.Context(), c, project, two.DatasetID, s.sql, s.insert); err != nil {
			t.Fatalf("%s: %v", s.sql, err)
		}
	}
	check("after DML", []string{"2|Y", "3|z", "4|w", "5|v"})

	// A query job with a destination, reading the second table by its
	// bare name, and a job's text as the client wrote it.
	q := c.Query("SELECT a FROM same WHERE a > 3")
	q.DefaultProjectID, q.DefaultDatasetID = project, two.DatasetID
	ctx = h.Context()
	job, err := q.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	it, err := job.Read(ctx)
	if err != nil {
		t.Fatalf("jobs.getQueryResults: %v", err)
	}
	if got := readRows(t, it, "the query job"); !reflect.DeepEqual(got, []string{"4", "5"}) {
		t.Errorf("the query job read %q, want [4 5]", got)
	}
	j, err := c.JobFromID(ctx, job.ID())
	if err != nil {
		t.Fatal(err)
	}
	if conf, err := j.Config(); err != nil || conf.(*bigquery.QueryConfig).Q != "SELECT a FROM same WHERE a > 3" {
		t.Errorf("jobs.get shows the query as %+v %v, want the client's", conf, err)
	}

	// A load, appending to the second table.
	src := bigquery.NewReaderSource(strings.NewReader("6,u\n"))
	src.Schema = bigquery.Schema{{Name: "a", Type: bigquery.IntegerFieldType}, {Name: "b", Type: bigquery.StringFieldType}}
	l := second.LoaderFrom(src)
	l.WriteDisposition = bigquery.WriteAppend
	ctx = h.Context()
	lj, err := l.Run(ctx)
	if err == nil {
		var st *bigquery.JobStatus
		if st, err = lj.Wait(ctx); err == nil {
			err = st.Err()
		}
	}
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	check("after a load", []string{"2|Y", "3|z", "4|w", "5|v", "6|u"})

	// TRUNCATE TABLE with the second dataset the default one.
	if err := runIn(h.Context(), c, project, two.DatasetID, "TRUNCATE TABLE same", false); err != nil {
		t.Fatalf("TRUNCATE TABLE: %v", err)
	}
	check("after TRUNCATE", nil)

	// No default dataset: BigQuery refuses the name.
	err = runIn(h.Context(), c, project, "", "SELECT * FROM same", false)
	if code, reason := apiError(err); code != http.StatusBadRequest || reason != "invalid" ||
		!strings.Contains(err.Error(), `Table "same" must be qualified with a dataset`) {
		t.Errorf("a query with no default dataset: %v, want 400 invalid", err)
	}
	err = runIn(h.Context(), c, project, "", "SELECT * FROM same", true)
	if errReason(err) != "invalid" {
		t.Errorf("a query job with no default dataset: %v, want it failed invalid", err)
	}
	check("after the refused queries", nil)
}

// TestBigQueryExtractReadsItsOwnTable (#1015): an extract of a table
// whose table ID another dataset has too, made first, writes its own rows.
// Measured first against the pinned emulator: its extract reads the table
// by its bare ID, as its tabledata.list does.
func TestBigQueryExtractReadsItsOwnTable(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	sc := instanceStorage(t, h)
	ctx := h.Context()
	one, two := twoDatasets(t, h, c)
	bucket := sc.Bucket(strings.ReplaceAll(h.Project(), "_", "-") + "-bq-same")
	if err := bucket.Create(ctx, h.Project(), nil); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	t.Cleanup(func() {
		_ = bucket.Object("two.csv").Delete(context.Background())
		_ = bucket.Delete(context.Background())
	})
	for _, sql := range []string{
		"CREATE TABLE " + one.DatasetID + ".same AS SELECT 'from one' AS s",
		"CREATE TABLE " + two.DatasetID + ".same AS SELECT 2 AS a, 'from two' AS b",
	} {
		if err := bqRun(ctx, c, sql, false); err != nil {
			t.Fatal(err)
		}
	}
	e := two.Table("same").ExtractorTo(bigquery.NewGCSReference("gs://" + bucket.BucketName() + "/two.csv"))
	job, err := e.Run(ctx)
	if err == nil {
		var st *bigquery.JobStatus
		if st, err = job.Wait(ctx); err == nil {
			err = st.Err()
		}
	}
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	r, err := bucket.Object("two.csv").NewReader(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	b, _ := io.ReadAll(r)
	if want := "a,b\n2,from two\n"; string(b) != want {
		t.Errorf("the extract wrote %q, want %q", b, want)
	}
}

// TestBigQueryExecuteImmediate (#1011), through the official Go client,
// through jobs.query and as query jobs: EXECUTE IMMEDIATE of a string
// literal, or of a script variable set to one, runs its statement, with
// its USING values; one the front cannot carry out is 501, and nothing
// runs. Measured first against the pinned emulator: every EXECUTE
// IMMEDIATE was answered done and ran nothing (no rows, no table, no
// function).
func TestBigQueryExecuteImmediate(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	d := ds.DatasetID
	for _, insert := range []bool{false, true} {
		read := func(sql string) []string {
			t.Helper()
			q := c.Query(sql)
			var it *bigquery.RowIterator
			var err error
			if insert {
				var job *bigquery.Job
				if job, err = q.Run(ctx); err == nil {
					it, err = job.Read(ctx)
				}
			} else {
				it, err = q.Read(ctx)
			}
			if err != nil {
				t.Fatalf("%s (query job %v): %v", sql, insert, err)
			}
			return readRows(t, it, sql)
		}
		for sql, want := range map[string][]string{
			"EXECUTE IMMEDIATE 'SELECT 1 AS a'":                                          {"1"},
			"EXECUTE IMMEDIATE 'SELECT @a + 1 AS x' USING 1 AS a":                        {"2"},
			"EXECUTE IMMEDIATE 'SELECT ? * 10, ?' USING 4, 'four'":                       {"40|four"},
			"DECLARE s STRING DEFAULT 'SELECT \"dq\" AS v'; EXECUTE IMMEDIATE s":         {"dq"},
			"DECLARE n INT64 DEFAULT 7; EXECUTE IMMEDIATE 'SELECT @v AS v' USING n AS v": {"7"},
		} {
			if got := read(sql); !reflect.DeepEqual(got, want) {
				t.Errorf("%s (query job %v): %q, want %q", sql, insert, got, want)
			}
		}
		suffix := "q"
		if insert {
			suffix = "j"
		}
		// DDL: a table, and functions alone, in BEGIN ... END and from a
		// variable.
		for _, sql := range []string{
			"EXECUTE IMMEDIATE 'CREATE TABLE " + d + ".made_" + suffix + " (x INT64)'",
			"EXECUTE IMMEDIATE \"CREATE FUNCTION " + d + ".f1_" + suffix + "(x INT64) AS (x + 1)\"",
			"BEGIN EXECUTE IMMEDIATE 'CREATE FUNCTION " + d + ".f2_" + suffix + "(x INT64) AS (x + 2)'; END",
			"DECLARE q STRING; SET q = 'CREATE FUNCTION " + d + ".f3_" + suffix + "(x INT64) AS (x + 3)'; EXECUTE IMMEDIATE q",
			// SQL built by CONCAT or || of literals (#1037).
			"EXECUTE IMMEDIATE CONCAT('CREATE TABLE " + d + ".concat_" + suffix + "', ' (x INT64)')",
			"EXECUTE IMMEDIATE 'CREATE TABLE " + d + ".pipes_" + suffix + "' || ' (x INT64)'",
		} {
			if err := runIn(ctx, c, project, "", sql, insert); err != nil {
				t.Errorf("%s: %v", sql, err)
			}
		}
		for _, name := range []string{"made_", "concat_", "pipes_"} {
			if _, err := ds.Table(name + suffix).Metadata(ctx); err != nil {
				t.Errorf("the table EXECUTE IMMEDIATE made (%s, query job %v): %v", name, insert, err)
			}
		}
		sql := fmt.Sprintf("SELECT %[1]s.f1_%[2]s(0), %[1]s.f2_%[2]s(0), %[1]s.f3_%[2]s(0)", d, suffix)
		if got := read(sql); !reflect.DeepEqual(got, []string{"1|2|3"}) {
			t.Errorf("the functions EXECUTE IMMEDIATE made (query job %v): %q", insert, got)
		}

		// 501, before anything runs.
		for _, sql := range []string{
			"EXECUTE IMMEDIATE FORMAT('SELECT %d', 1)",
			"DECLARE x INT64; EXECUTE IMMEDIATE 'SELECT 1' INTO x",
			"DECLARE q STRING DEFAULT (SELECT 'SELECT 1'); EXECUTE IMMEDIATE q",
			"EXECUTE IMMEDIATE 'SELECT @a' USING 1 + 1 AS a",
			"CREATE TABLE " + d + ".not_made_" + suffix + " (x INT64); EXECUTE IMMEDIATE FORMAT('SELECT %d', 1)",
		} {
			err := runIn(ctx, c, project, "", sql, insert)
			if code, reason := apiError(err); code != http.StatusNotImplemented || reason != "notImplemented" {
				t.Errorf("%s (query job %v): %v, want 501 notImplemented", sql, insert, err)
			}
		}
		if _, err := ds.Table("not_made_" + suffix).Metadata(ctx); err == nil {
			t.Errorf("a script refused 501 made its table (query job %v)", insert)
		}
		// A parameter with no value: 400.
		err := runIn(ctx, c, project, "", "EXECUTE IMMEDIATE 'SELECT @b' USING 1 AS a", insert)
		if !insert {
			if code, _ := apiError(err); code != http.StatusBadRequest {
				t.Errorf("a parameter with no value: %v, want 400", err)
			}
		} else if errReason(err) != "invalidQuery" {
			t.Errorf("a parameter with no value (query job): %v, want invalidQuery", err)
		}
	}

	// jobs.get shows the client's text.
	q := c.Query("EXECUTE IMMEDIATE 'SELECT 1 AS a'")
	job, err := q.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := job.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	j, err := c.JobFromID(ctx, job.ID())
	if err != nil {
		t.Fatal(err)
	}
	if conf, err := j.Config(); err != nil || conf.(*bigquery.QueryConfig).Q != "EXECUTE IMMEDIATE 'SELECT 1 AS a'" {
		t.Errorf("jobs.get shows %+v %v, want the client's text", conf, err)
	}
}

// TestBigQueryTableUpdateAddsColumns (#1010, #1013): a schema update that
// adds columns, through the official Go client's Table.Update
// (tables.patch) and the generated client's Tables.Update (tables.update),
// adds them to the table: its rows read NULL (or empty) in them, and it is
// written through Inserter.Put, DML and a load, with and without the new
// columns, and read back through Table.Read and a query; its description,
// labels and creation time are kept. What BigQuery refuses is 400, and
// what the front does not carry out 501 (a new column or field before the
// table's own), the table unchanged. Since #1036 fields added to a
// RECORD, two levels down and in a REPEATED RECORD, keep every row as it
// was, the new fields NULL, and take writes; a schema given in another
// order keeps the table's. Measured
// first against the pinned emulator: the update changed only tables.get's
// schema, and every later write failed ("Column g is not present in
// table", Inserter.Put 500).
//
// covers: bigquery.tables.patch, bigquery.tables.update
func TestBigQueryTableUpdateAddsColumns(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	tbl := ds.Table("grown")
	schema := bigquery.Schema{
		{Name: "id", Type: bigquery.IntegerFieldType, Required: true},
		{Name: "f", Type: bigquery.FloatFieldType},
	}
	if err := tbl.Create(ctx, &bigquery.TableMetadata{Schema: schema, Description: "kept", Labels: map[string]string{"k": "v"}}); err != nil {
		t.Fatalf("Table.Create: %v", err)
	}
	if err := bqRun(ctx, c, "INSERT INTO "+ds.DatasetID+".grown (id, f) VALUES (1, 0.5), (2, CAST(0.1 AS FLOAT64))", false); err != nil {
		t.Fatal(err)
	}
	before, err := tbl.Metadata(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Table.Update (tables.patch) adding a STRING, a REPEATED STRING and a
	// RECORD, and relaxing id to NULLABLE.
	grown := bigquery.Schema{
		{Name: "id", Type: bigquery.IntegerFieldType},
		{Name: "f", Type: bigquery.FloatFieldType, Description: "a float"},
		{Name: "g", Type: bigquery.StringFieldType},
		{Name: "tags", Type: bigquery.StringFieldType, Repeated: true},
		{Name: "r", Type: bigquery.RecordFieldType, Schema: bigquery.Schema{{Name: "x", Type: bigquery.FloatFieldType}}},
	}
	meta, err := tbl.Update(ctx, bigquery.TableMetadataToUpdate{Schema: grown}, before.ETag)
	if err != nil {
		t.Fatalf("Table.Update: %v", err)
	}
	if got := schemaText(meta.Schema); got != schemaText(grown) {
		t.Errorf("Table.Update answered %s, want %s", got, schemaText(grown))
	}
	meta, err = tbl.Metadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := schemaText(meta.Schema); got != schemaText(grown) {
		t.Errorf("the table reads %s, want %s", got, schemaText(grown))
	}
	if meta.Description != "kept" || !reflect.DeepEqual(meta.Labels, map[string]string{"k": "v"}) || !meta.CreationTime.Equal(before.CreationTime) {
		t.Errorf("the table's description %q, labels %v, creation time %v; want kept, k=v, %v", meta.Description, meta.Labels,
			meta.CreationTime, before.CreationTime)
	}
	if got, want := readTable(t, ctx, tbl, 0), []string{"1|0.5|<nil>|[]|<nil>", "2|0.1|<nil>|[]|<nil>"}; !reflect.DeepEqual(got, want) {
		t.Errorf("the rows after the update: %q, want %q", got, want)
	}

	// Written with and without the new columns.
	put := []*bigquery.ValuesSaver{
		{Schema: grown, Row: []bigquery.Value{int64(3), 1.5, "streamed", []bigquery.Value{"a", "b"}, []bigquery.Value{2.5}}},
		{Schema: grown[:2], Row: []bigquery.Value{int64(4), 4.5}},
		{Schema: grown[2:3], Row: []bigquery.Value{"no id"}},
	}
	if err := tbl.Inserter().Put(ctx, put); err != nil {
		t.Fatalf("Inserter.Put: %v", err)
	}
	if err := bqRun(ctx, c, "INSERT INTO "+ds.DatasetID+".grown (id, g, tags, r) VALUES (5, 'dml', ['c'], STRUCT(CAST(0.25 AS FLOAT64) AS x))", true); err != nil {
		t.Fatalf("INSERT of the new columns: %v", err)
	}
	src := bigquery.NewReaderSource(strings.NewReader("6,6.5,loaded\n"))
	src.Schema = grown[:3]
	l := tbl.LoaderFrom(src)
	l.WriteDisposition = bigquery.WriteAppend
	lj, err := l.Run(ctx)
	if err == nil {
		var st *bigquery.JobStatus
		if st, err = lj.Wait(ctx); err == nil {
			err = st.Err()
		}
	}
	if err != nil {
		t.Fatalf("a load with the new column: %v", err)
	}
	want := []string{"1|0.5|<nil>|[]|<nil>", "2|0.1|<nil>|[]|<nil>", "3|1.5|streamed|[a b]|[2.5]", "4|4.5|<nil>|[]|<nil>",
		"5|<nil>|dml|[c]|[0.25]", "6|6.5|loaded|[]|<nil>", "<nil>|<nil>|no id|[]|<nil>"}
	sort.Strings(want)
	if got := readTable(t, ctx, tbl, 0); !reflect.DeepEqual(got, want) {
		t.Errorf("Table.Read: %q, want %q", got, want)
	}
	if got := queryRows(t, ctx, c, project, "", "SELECT id, g FROM "+ds.DatasetID+".grown WHERE g IS NOT NULL"); !reflect.DeepEqual(got,
		[]string{"3|streamed", "5|dml", "6|loaded", "<nil>|no id"}) {
		t.Errorf("SELECT of the new column: %q", got)
	}

	// tables.update (PUT), through the generated client, adding h.
	svc, err := bq.NewService(ctx, option.WithEndpoint(h.Endpoint(EnvBigQuery)), option.WithoutAuthentication())
	if err != nil {
		t.Fatalf("bigquery/v2 NewService: %v", err)
	}
	full, err := svc.Tables.Get(project, ds.DatasetID, "grown").Context(ctx).Do()
	if err != nil {
		t.Fatal(err)
	}
	full.Schema.Fields = append(full.Schema.Fields, &bq.TableFieldSchema{Name: "h", Type: "INTEGER"})
	if _, err := svc.Tables.Update(project, ds.DatasetID, "grown", full).Context(ctx).Do(); err != nil {
		t.Fatalf("Tables.Update: %v", err)
	}
	if err := bqRun(ctx, c, "UPDATE "+ds.DatasetID+".grown SET h = id * 10 WHERE id IS NOT NULL", false); err != nil {
		t.Fatalf("UPDATE of the column tables.update added: %v", err)
	}
	if got := queryRows(t, ctx, c, project, "", "SELECT id, h FROM "+ds.DatasetID+".grown WHERE id <= 2"); !reflect.DeepEqual(got, []string{"1|10", "2|20"}) {
		t.Errorf("SELECT of h: %q", got)
	}

	// Refused as BigQuery refuses them (400), or not carried out (501):
	// the table is unchanged.
	meta, err = tbl.Metadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	current := meta.Schema
	for _, s := range []struct {
		name   string
		schema bigquery.Schema
		code   int
		reason string
	}{
		{"a column left out", current[1:], 400, "invalid"},
		{"a type changed", append(bigquery.Schema{{Name: "id", Type: bigquery.StringFieldType}}, current[1:]...), 400, "invalid"},
		{"a REQUIRED column added", append(append(bigquery.Schema{}, current...), &bigquery.FieldSchema{Name: "req", Type: bigquery.StringFieldType, Required: true}), 400, "invalid"},
		{"made REQUIRED", append(bigquery.Schema{{Name: "id", Type: bigquery.IntegerFieldType, Required: true}}, current[1:]...), 400, "invalid"},
		{"a new column before the table's", append(bigquery.Schema{{Name: "early", Type: bigquery.StringFieldType}}, current...), 501, "notImplemented"},
		{"a new field before a RECORD's", append(append(bigquery.Schema{}, current[:4]...),
			&bigquery.FieldSchema{Name: "r", Type: bigquery.RecordFieldType, Schema: bigquery.Schema{{Name: "y", Type: bigquery.StringFieldType}, {Name: "x", Type: bigquery.FloatFieldType}}},
			current[5]), 501, "notImplemented"},
	} {
		_, err := tbl.Update(ctx, bigquery.TableMetadataToUpdate{Schema: s.schema}, "")
		if code, reason := apiError(err); code != s.code || reason != s.reason {
			t.Errorf("%s: %v, want %d %s", s.name, err, s.code, s.reason)
		}
	}
	if meta, err := tbl.Metadata(ctx); err != nil || schemaText(meta.Schema) != schemaText(current) {
		t.Errorf("after the refused updates the table reads %v %v, want %s", meta, err, schemaText(current))
	}
	if got := readTable(t, ctx, tbl, 0); len(got) != len(want) {
		t.Errorf("after the refused updates the table has %d rows, want %d", len(got), len(want))
	}

	// Fields added to a RECORD, at any depth (#1036): the rows are kept
	// exactly, the new fields NULL (a REPEATED one empty), a NULL RECORD
	// NULL, and a REPEATED RECORD's elements in order.
	const kept = "SELECT id, f, g, tags, r IS NULL, r.x, h FROM %s.grown ORDER BY id, g"
	keptRows := queryRows(t, ctx, c, project, "", fmt.Sprintf(kept, ds.DatasetID))
	withRecords := append(append(bigquery.Schema{}, current[:4]...),
		&bigquery.FieldSchema{Name: "r", Type: bigquery.RecordFieldType, Schema: bigquery.Schema{{Name: "x", Type: bigquery.FloatFieldType},
			{Name: "n", Type: bigquery.RecordFieldType, Schema: bigquery.Schema{{Name: "z", Type: bigquery.IntegerFieldType}}}}},
		current[5],
		&bigquery.FieldSchema{Name: "rr", Type: bigquery.RecordFieldType, Repeated: true, Schema: bigquery.Schema{{Name: "k", Type: bigquery.StringFieldType}}})
	if _, err := tbl.Update(ctx, bigquery.TableMetadataToUpdate{Schema: withRecords}, ""); err != nil {
		t.Fatalf("Table.Update adding a RECORD field and a REPEATED RECORD: %v", err)
	}
	if err := bqRun(ctx, c, "INSERT INTO "+ds.DatasetID+".grown (id, r, rr) VALUES (7, STRUCT(7.5 AS x, STRUCT(70 AS z) AS n), "+
		"[STRUCT('k1' AS k), STRUCT('k2' AS k)])", false); err != nil {
		t.Fatalf("INSERT of the new fields: %v", err)
	}
	deeper := append(append(bigquery.Schema{}, withRecords[:4]...),
		&bigquery.FieldSchema{Name: "r", Type: bigquery.RecordFieldType, Schema: bigquery.Schema{{Name: "x", Type: bigquery.FloatFieldType},
			{Name: "n", Type: bigquery.RecordFieldType, Schema: bigquery.Schema{{Name: "z", Type: bigquery.IntegerFieldType},
				{Name: "w", Type: bigquery.BytesFieldType}}}, {Name: "q", Type: bigquery.StringFieldType, Repeated: true}}},
		withRecords[5],
		&bigquery.FieldSchema{Name: "rr", Type: bigquery.RecordFieldType, Repeated: true, Schema: bigquery.Schema{{Name: "k", Type: bigquery.StringFieldType},
			{Name: "v", Type: bigquery.NumericFieldType}}})
	// Given in another order: the table keeps its own.
	reordered := append(bigquery.Schema{deeper[1], deeper[0]}, deeper[2:]...)
	if _, err := tbl.Update(ctx, bigquery.TableMetadataToUpdate{Schema: reordered}, ""); err != nil {
		t.Fatalf("Table.Update adding fields two levels down: %v", err)
	}
	if meta, err := tbl.Metadata(ctx); err != nil || schemaText(meta.Schema) != schemaText(deeper) {
		t.Errorf("after adding fields the table reads %v %v, want %s", meta, err, schemaText(deeper))
	}
	var others []string // the rows but 7
	for _, r := range queryRows(t, ctx, c, project, "", fmt.Sprintf(kept, ds.DatasetID)) {
		if !strings.HasPrefix(r, "7|") {
			others = append(others, r)
		}
	}
	if !reflect.DeepEqual(others, keptRows) {
		t.Errorf("after adding fields the rows read %q, want %q", others, keptRows)
	}
	if got := queryRows(t, ctx, c, project, "", "SELECT id, r.x, r.n.z, r.n.w IS NULL, r.q, rr[SAFE_OFFSET(0)].k, rr[SAFE_OFFSET(1)].k, "+
		"(SELECT COUNT(*) FROM UNNEST(rr) e WHERE e.v IS NULL) FROM "+ds.DatasetID+".grown WHERE id IN (3, 7) ORDER BY id"); !reflect.DeepEqual(got,
		[]string{"3|2.5|<nil>|true|[]|<nil>|<nil>|0", "7|7.5|70|true|[]|k1|k2|2"}) {
		t.Errorf("the RECORD fields after adding fields: %q", got)
	}
	if err := bqRun(ctx, c, "INSERT INTO "+ds.DatasetID+".grown (id, r, rr) VALUES (8, STRUCT(8.5 AS x, STRUCT(80 AS z, b'\\xff\\x00' AS w) AS n, ['q1'] AS q), "+
		"[STRUCT('k3' AS k, NUMERIC '1.25' AS v)])", false); err != nil {
		t.Fatalf("INSERT of the fields added two levels down: %v", err)
	}
	if got := queryRows(t, ctx, c, project, "", "SELECT TO_HEX(r.n.w), r.q[OFFSET(0)], CAST(rr[OFFSET(0)].v AS STRING) FROM "+ds.DatasetID+
		".grown WHERE id = 8"); !reflect.DeepEqual(got, []string{"ff00|q1|1.25"}) {
		t.Errorf("the fields added two levels down: %q", got)
	}
}
