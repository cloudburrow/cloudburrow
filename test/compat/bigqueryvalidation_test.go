//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
)

// The tests in this file are #861: the emulator behind CloudBurrow's
// BigQuery validates almost nothing, and CloudBurrow's front
// (internal/bigqueryfront) refuses what BigQuery refuses. Each drives one
// behaviour through the official Go client.

// validationDataset creates a dataset named after the test's project, so
// runs cannot collide in the emulator's one project.
func validationDataset(t *testing.T, h *Harness, c *bigquery.Client) *bigquery.Dataset {
	t.Helper()
	ds := c.Dataset(strings.ReplaceAll(h.Project(), "-", "_"))
	if err := ds.Create(h.Context(), nil); err != nil {
		t.Fatalf("create dataset: %v", err)
	}
	t.Cleanup(func() { _ = ds.DeleteWithContents(h.Context()) })
	return ds
}

// validationTable creates a table with a REQUIRED INTEGER, a REPEATED
// INTEGER, a NUMERIC and a STRING column.
func validationTable(t *testing.T, h *Harness, ds *bigquery.Dataset, name string) *bigquery.Table {
	t.Helper()
	tbl := ds.Table(name)
	schema := bigquery.Schema{
		{Name: "id", Type: bigquery.IntegerFieldType, Required: true},
		{Name: "tags", Type: bigquery.IntegerFieldType, Repeated: true},
		{Name: "amount", Type: bigquery.NumericFieldType},
		{Name: "note", Type: bigquery.StringFieldType},
	}
	if err := tbl.Create(h.Context(), &bigquery.TableMetadata{Schema: schema}); err != nil {
		t.Fatalf("create table: %v", err)
	}
	return tbl
}

// mapRow is one row as insertAll's JSON carries it, uninterpreted by the
// client, so a wrong-type value reaches the server as it was written.
type mapRow map[string]bigquery.Value

func (r mapRow) Save() (map[string]bigquery.Value, string, error) { return r, "", nil }

func wantHTTPStatus(t *testing.T, what string, err error, code int) {
	t.Helper()
	var e *googleapi.Error
	if !errors.As(err, &e) || e.Code != code {
		t.Errorf("%s returned %v, want HTTP %d", what, err, code)
	}
}

// rowReasons returns each failed row's index and its errors' reasons, as
// the Go client reports insertErrors.
func rowReasons(t *testing.T, err error) map[int]string {
	t.Helper()
	var multi bigquery.PutMultiError
	if !errors.As(err, &multi) {
		t.Fatalf("insert returned %v, want a PutMultiError (insertErrors)", err)
	}
	out := map[int]string{}
	for _, row := range multi {
		var reasons []string
		for _, e := range row.Errors {
			var be *bigquery.Error
			if errors.As(e, &be) {
				reasons = append(reasons, be.Reason)
			}
		}
		sort.Strings(reasons)
		out[row.RowIndex] = strings.Join(reasons, ",")
	}
	return out
}

// countRows reads the table through tabledata.list and returns how many
// rows it has, failing the test if the table cannot be read.
func countRows(t *testing.T, h *Harness, tbl *bigquery.Table) int {
	t.Helper()
	it := tbl.Read(h.Context())
	n := 0
	for {
		var row []bigquery.Value
		err := it.Next(&row)
		if errors.Is(err, iterator.Done) {
			return n
		}
		if err != nil {
			t.Fatalf("read table: %v", err)
		}
		n++
	}
}

// TestBigQueryRefusesInvalidDatasetID: a hyphen is not allowed in a dataset
// ID ("Letters (uppercase or lowercase), numbers, and underscores"). The
// emulator created it.
func TestBigQueryRefusesInvalidDatasetID(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := c.Dataset(strings.ReplaceAll(h.Project(), "-", "_") + "-bad")
	err := ds.Create(h.Context(), nil)
	if err == nil {
		_ = ds.Delete(h.Context())
	}
	wantHTTPStatus(t, "creating dataset "+ds.DatasetID, err, http.StatusBadRequest)
}

// TestBigQueryDuplicateDatasetIs409: BigQuery answers a dataset that exists
// with 409. The emulator answered 500, which the client retried until its
// deadline.
func TestBigQueryDuplicateDatasetIs409(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	wantHTTPStatus(t, "creating the dataset again", ds.Create(h.Context(), nil), http.StatusConflict)
}

// TestBigQueryRefusesInvalidTableID: '!' is not in the table ID's Unicode
// categories (L, M, N, Pc, Pd, Zs), while letters of any script, a dash and
// a space are.
func TestBigQueryRefusesInvalidTableID(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	schema := bigquery.Schema{{Name: "a", Type: bigquery.StringFieldType}}
	wantHTTPStatus(t, "creating table t!", ds.Table("t!").Create(h.Context(), &bigquery.TableMetadata{Schema: schema}),
		http.StatusBadRequest)
	if err := ds.Table("étudiant-01 x").Create(h.Context(), &bigquery.TableMetadata{Schema: schema}); err != nil {
		t.Errorf("a table ID BigQuery accepts was refused: %v", err)
	}
}

// TestBigQueryRefusesInvalidFieldName: '!' is not allowed in a column name.
// A space is, under BigQuery's flexible column names, so it is accepted
// and the column is readable.
func TestBigQueryRefusesInvalidFieldName(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	bad := bigquery.Schema{{Name: "a!", Type: bigquery.StringFieldType}}
	wantHTTPStatus(t, "creating a table with column a!", ds.Table("bang").Create(h.Context(), &bigquery.TableMetadata{Schema: bad}),
		http.StatusBadRequest)

	spaced := ds.Table("spaced")
	if err := spaced.Create(h.Context(), &bigquery.TableMetadata{Schema: bigquery.Schema{{Name: "first name", Type: bigquery.StringFieldType}}}); err != nil {
		t.Fatalf("a column name with a space, which BigQuery accepts, was refused: %v", err)
	}
	if err := spaced.Inserter().Put(h.Context(), mapRow{"first name": "Ada"}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if n := countRows(t, h, spaced); n != 1 {
		t.Errorf("read back %d rows, want 1", n)
	}
}

// TestBigQueryDuplicateColumnIs400: two columns whose names differ only in
// case are duplicates ("Duplicate column names are not allowed even if the
// case differs"). The emulator answered 500.
func TestBigQueryDuplicateColumnIs400(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	schema := bigquery.Schema{{Name: "a", Type: bigquery.StringFieldType}, {Name: "A", Type: bigquery.StringFieldType}}
	wantHTTPStatus(t, "creating a table with columns a and A", ds.Table("dup").Create(h.Context(), &bigquery.TableMetadata{Schema: schema}),
		http.StatusBadRequest)
}

// TestBigQueryInsertRefusesMissingRequiredValue: a row without its
// REQUIRED value is an insertErrors entry, and is not stored.
func TestBigQueryInsertRefusesMissingRequiredValue(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	tbl := validationTable(t, h, validationDataset(t, h, c), "required")
	err := tbl.Inserter().Put(h.Context(), mapRow{"note": "no id"})
	if got := rowReasons(t, err); got[0] != "invalid" {
		t.Errorf("insertErrors %v, want row 0 invalid", got)
	}
	if n := countRows(t, h, tbl); n != 0 {
		t.Errorf("the table has %d rows, want 0", n)
	}
}

// TestBigQueryInsertRefusesWrongTypeValue: "x" in a NUMERIC column and
// "abc" in an INTEGER one are invalid rows, not a stored 0 or a 500.
func TestBigQueryInsertRefusesWrongTypeValue(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	tbl := validationTable(t, h, validationDataset(t, h, c), "types")
	ins := tbl.Inserter()
	ins.SkipInvalidRows = true
	err := ins.Put(h.Context(), []mapRow{{"id": 1, "amount": "x"}, {"id": "abc"}})
	if got := rowReasons(t, err); got[0] != "invalid" || got[1] != "invalid" {
		t.Errorf("insertErrors %v, want rows 0 and 1 invalid", got)
	}
	if n := countRows(t, h, tbl); n != 0 {
		t.Errorf("the table has %d rows, want 0", n)
	}
}

// TestBigQueryTableReadableAfterRefusedRepeatedValue: a string element in a
// REPEATED INTEGER column is refused, and the table can still be read. The
// emulator stored it, and every read of the table failed afterwards.
func TestBigQueryTableReadableAfterRefusedRepeatedValue(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	tbl := validationTable(t, h, validationDataset(t, h, c), "repeated")
	if err := tbl.Inserter().Put(h.Context(), mapRow{"id": 1, "tags": []bigquery.Value{1, 2}}); err != nil {
		t.Fatalf("valid insert: %v", err)
	}
	err := tbl.Inserter().Put(h.Context(), mapRow{"id": 2, "tags": []bigquery.Value{"q"}})
	if got := rowReasons(t, err); got[0] != "invalid" {
		t.Errorf("insertErrors %v, want row 0 invalid", got)
	}
	if n := countRows(t, h, tbl); n != 1 {
		t.Errorf("the table has %d rows, want the 1 valid one", n)
	}
}

// TestBigQueryInsertAllWithoutSkipInvalidRowsInsertsNothing: by default a
// batch with an invalid row fails whole. Every row is reported: the bad
// one "invalid", the others "stopped".
func TestBigQueryInsertAllWithoutSkipInvalidRowsInsertsNothing(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	tbl := validationTable(t, h, validationDataset(t, h, c), "batch")
	err := tbl.Inserter().Put(h.Context(), []mapRow{{"id": 1}, {"id": "bad"}, {"id": 3}})
	got := rowReasons(t, err)
	if got[0] != "stopped" || got[1] != "invalid" || got[2] != "stopped" {
		t.Errorf("insertErrors %v, want 0 stopped, 1 invalid, 2 stopped", got)
	}
	if n := countRows(t, h, tbl); n != 0 {
		t.Errorf("the table has %d rows, want 0", n)
	}
}

// TestBigQueryInsertAllSkipInvalidRows: with skipInvalidRows the valid rows
// are inserted and only the invalid one is reported, at its own index.
func TestBigQueryInsertAllSkipInvalidRows(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	tbl := validationTable(t, h, validationDataset(t, h, c), "skip")
	ins := tbl.Inserter()
	ins.SkipInvalidRows = true
	err := ins.Put(h.Context(), []mapRow{{"id": 1}, {"id": "bad"}, {"id": 3}})
	got := rowReasons(t, err)
	if len(got) != 1 || got[1] != "invalid" {
		t.Errorf("insertErrors %v, want only row 1 invalid", got)
	}
	if n := countRows(t, h, tbl); n != 2 {
		t.Errorf("the table has %d rows, want the 2 valid ones", n)
	}
}

// TestBigQueryInsertAllIgnoreUnknownValues: a field the table does not have
// makes the row invalid, unless ignoreUnknownValues is set, when the field
// is dropped and the row inserted.
func TestBigQueryInsertAllIgnoreUnknownValues(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	tbl := validationTable(t, h, validationDataset(t, h, c), "unknown")
	err := tbl.Inserter().Put(h.Context(), mapRow{"id": 1, "extra": "x"})
	if got := rowReasons(t, err); got[0] != "invalid" {
		t.Errorf("insertErrors %v, want row 0 invalid", got)
	}
	ins := tbl.Inserter()
	ins.IgnoreUnknownValues = true
	if err := ins.Put(h.Context(), mapRow{"id": 2, "extra": "x"}); err != nil {
		t.Errorf("with IgnoreUnknownValues: %v", err)
	}
	if n := countRows(t, h, tbl); n != 1 {
		t.Errorf("the table has %d rows, want 1", n)
	}
}

// readAll reads every row of it within a bound: a table the emulator cannot
// read answers 500, which the client retries until its deadline.
func readAll(t *testing.T, h *Harness, it func(context.Context) *bigquery.RowIterator) ([][]bigquery.Value, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(h.Context(), 20*time.Second)
	defer cancel()
	rows := it(ctx)
	var out [][]bigquery.Value
	for {
		var row []bigquery.Value
		err := rows.Next(&row)
		if errors.Is(err, iterator.Done) {
			return out, nil
		}
		if err != nil {
			return out, err
		}
		out = append(out, row)
	}
}

// TestBigQueryNestedRecordsInRepeatedRecordsReadBack (#874, #881): a table
// with a RECORD inside a REPEATED RECORD, or a REPEATED RECORD inside a
// RECORD, is created as BigQuery creates it, and rows written to it by a DML
// INSERT are read back, through tabledata.list and a query, with their
// values; so is a load job's into a REPEATED RECORD inside a RECORD.
// Streaming a value into such a RECORD is what the emulator cannot store
// readably (measured: every later read failed with 500 "failed to scan
// rows"), so tabledata.insertAll with one is 501, at once, nothing is
// inserted and the table stays readable; a row with null there, or an
// empty array, is streamed and read back. A load into a RECORD inside a
// REPEATED RECORD, which the emulator stored unreadably in some runs
// (measured), is 501 and loads nothing.
func TestBigQueryNestedRecordsInRepeatedRecordsReadBack(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	rec := func(name string, repeated bool, fields ...*bigquery.FieldSchema) *bigquery.FieldSchema {
		return &bigquery.FieldSchema{Name: name, Type: bigquery.RecordFieldType, Repeated: repeated, Schema: fields}
	}
	str := func(name string) *bigquery.FieldSchema {
		return &bigquery.FieldSchema{Name: name, Type: bigquery.StringFieldType}
	}
	for _, tc := range []struct {
		table   string
		schema  bigquery.Schema
		dml     string
		load    string
		want    string
		stream  mapRow // a value where the emulator cannot store one
		streamd mapRow // null or empty there
		loads   bool   // whether a load job is taken
	}{
		{
			table:   "rec_in_repeated",
			schema:  bigquery.Schema{rec("a", true, str("n"), rec("b", false, str("s")))},
			dml:     "INSERT %s (a) VALUES ([STRUCT('x' AS n, STRUCT('y' AS s) AS b)])",
			load:    `{"a":[{"n":"x","b":{"s":"y"}}]}`,
			want:    `[[["x",["y"]]]]`,
			stream:  mapRow{"a": []bigquery.Value{map[string]bigquery.Value{"n": "x", "b": map[string]bigquery.Value{"s": "y"}}}},
			streamd: mapRow{"a": []bigquery.Value{map[string]bigquery.Value{"n": "x", "b": nil}}},
		},
		{
			table:   "repeated_in_rec",
			schema:  bigquery.Schema{rec("a", false, str("n"), rec("b", true, str("s")))},
			dml:     "INSERT %s (a) VALUES (STRUCT('x' AS n, [STRUCT('y' AS s)] AS b))",
			load:    `{"a":{"n":"x","b":[{"s":"y"}]}}`,
			want:    `[["x",[["y"]]]]`,
			stream:  mapRow{"a": map[string]bigquery.Value{"n": "x", "b": []bigquery.Value{map[string]bigquery.Value{"s": "y"}}}},
			streamd: mapRow{"a": map[string]bigquery.Value{"n": "x", "b": []bigquery.Value{}}},
			loads:   true,
		},
	} {
		t.Run(tc.table, func(t *testing.T) {
			tbl := ds.Table(tc.table)
			if err := tbl.Create(ctx, &bigquery.TableMetadata{Schema: tc.schema}); err != nil {
				t.Fatalf("create: %v", err)
			}
			md, err := tbl.Metadata(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if got := md.Schema[0].Schema[1]; got.Type != bigquery.RecordFieldType || got.Repeated != tc.schema[0].Schema[1].Repeated {
				t.Errorf("the nested RECORD reads back as %+v", got)
			}

			// Streamed, a value there is refused at once and nothing lands.
			start := time.Now()
			err = tbl.Inserter().Put(ctx, []mapRow{tc.stream})
			wantHTTPStatus(t, "streaming a nested RECORD value", err, http.StatusNotImplemented)
			if took := time.Since(start); took > 10*time.Second {
				t.Errorf("the 501 took %s: it was retried", took)
			}
			if err := tbl.Inserter().Put(ctx, []mapRow{tc.streamd}); err != nil {
				t.Fatalf("streaming a row with nothing there: %v", err)
			}

			// DML and a load job write the value itself.
			q := c.Query(fmt.Sprintf(tc.dml, "`"+ds.DatasetID+"."+tc.table+"`"))
			job, err := q.Run(ctx)
			if err != nil {
				t.Fatalf("DML INSERT: %v", err)
			}
			if st, err := job.Wait(ctx); err != nil || st.Err() != nil {
				t.Fatalf("DML INSERT: %v %v", err, st)
			}
			src := bigquery.NewReaderSource(strings.NewReader(tc.load + "\n"))
			src.SourceFormat = bigquery.JSON
			load, err := tbl.LoaderFrom(src).Run(ctx)
			want := 2
			if tc.loads {
				if err != nil {
					t.Fatalf("load: %v", err)
				}
				if st, err := load.Wait(ctx); err != nil || st.Err() != nil {
					t.Fatalf("load: %v %v", err, st)
				}
				want = 3
			} else {
				wantHTTPStatus(t, "a load into a RECORD inside a REPEATED RECORD", err, http.StatusNotImplemented)
			}

			listed, err := readAll(t, h, func(ctx context.Context) *bigquery.RowIterator { return tbl.Read(ctx) })
			if err != nil {
				t.Fatalf("tabledata.list: %v", err)
			}
			selected, err := readAll(t, h, func(ctx context.Context) *bigquery.RowIterator {
				it, err := c.Query("SELECT * FROM `" + ds.DatasetID + "." + tc.table + "`").Read(ctx)
				if err != nil {
					t.Fatalf("query: %v", err)
				}
				return it
			})
			if err != nil {
				t.Fatalf("query: %v", err)
			}
			for what, rows := range map[string][][]bigquery.Value{"tabledata.list": listed, "SELECT": selected} {
				values := 0
				for _, row := range rows {
					if b, _ := json.Marshal(row); string(b) == tc.want {
						values++
					}
				}
				if len(rows) != want || values != want-1 {
					t.Errorf("%s: %d rows, %d of them %s, want %d, all but the streamed one %s: %v", what, len(rows), values, tc.want, want, tc.want, rows)
				}
			}
		})
	}

	// RECORDs nested with none REPEATED, and a REPEATED RECORD of scalars,
	// are streamed as well as written.
	tbl := ds.Table("readable")
	schema := bigquery.Schema{rec("a", false, str("s"), rec("b", false, rec("c", false, str("s")))),
		rec("list", true, str("s"), &bigquery.FieldSchema{Name: "tags", Type: bigquery.StringFieldType, Repeated: true})}
	if err := tbl.Create(ctx, &bigquery.TableMetadata{Schema: schema}); err != nil {
		t.Fatalf("create a table of RECORDs: %v", err)
	}
	row := mapRow{
		"a":    map[string]bigquery.Value{"s": "x", "b": map[string]bigquery.Value{"c": map[string]bigquery.Value{"s": "y"}}},
		"list": []bigquery.Value{map[string]bigquery.Value{"s": "z", "tags": []bigquery.Value{"p", "q"}}},
	}
	if err := tbl.Inserter().Put(ctx, []mapRow{row}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if n := countRows(t, h, tbl); n != 1 {
		t.Errorf("the table reads back %d rows, want 1", n)
	}
}

// TestBigQueryJobsAreHeldToTheNamingRules (#881): a table made by a DDL
// statement or a load job is held to the rules tables.insert and
// datasets.insert are. The emulator ran CREATE TABLE ds.`t!`, a column
// `a!` and CREATE SCHEMA `bad-name`, and loaded rows into a table "t!"
// (measured); each is now 400 and nothing is made. Names BigQuery takes,
// a space in a column and a dash in a table, are made.
func TestBigQueryJobsAreHeldToTheNamingRules(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	run := func(sql string) error {
		qctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		_, err := c.Query(sql).Read(qctx)
		return err
	}
	path := func(table string) string { return "`" + ds.DatasetID + "." + table + "`" }
	for sql, what := range map[string]string{
		"CREATE TABLE " + path("t!") + " (a STRING)":                            "a table named t!",
		"CREATE TABLE " + path("cols") + " (`a!` STRING)":                       "a column named a!",
		"CREATE TABLE " + path("deep") + " (a STRUCT<`b!` STRING>)":             "a STRUCT field named b!",
		"CREATE SCHEMA `" + strings.ReplaceAll(h.Project(), "-", "_") + "-bad`": "a dataset named with a hyphen",
	} {
		wantHTTPStatus(t, "DDL making "+what, run(sql), http.StatusBadRequest)
	}
	for _, table := range []string{"t!", "cols", "deep"} {
		if _, err := ds.Table(table).Metadata(ctx); err == nil {
			t.Errorf("the refused table %s was made", table)
		}
	}
	if _, err := c.Dataset(strings.ReplaceAll(h.Project(), "-", "_") + "-bad").Metadata(ctx); err == nil {
		t.Error("the refused dataset was made")
	}
	if err := run("CREATE TABLE " + path("ok table-1") + " (`first name` STRING, n INT64)"); err != nil {
		t.Fatalf("DDL with names BigQuery takes: %v", err)
	}
	if md, err := ds.Table("ok table-1").Metadata(ctx); err != nil || md.Schema[0].Name != "first name" {
		t.Errorf("the DDL table reads back as %v %v", md, err)
	}

	load := func(table string, schema bigquery.Schema) error {
		src := bigquery.NewReaderSource(strings.NewReader(`{"a":"x"}` + "\n"))
		src.SourceFormat = bigquery.JSON
		src.Schema = schema
		job, err := ds.Table(table).LoaderFrom(src).Run(ctx)
		if err != nil {
			return err
		}
		st, err := job.Wait(ctx)
		if err != nil {
			return err
		}
		return st.Err()
	}
	one := bigquery.Schema{{Name: "a", Type: bigquery.StringFieldType}}
	wantHTTPStatus(t, "a load into t!", load("t!", one), http.StatusBadRequest)
	wantHTTPStatus(t, "a load with a column named twice", load("twice", bigquery.Schema{
		{Name: "a", Type: bigquery.StringFieldType}, {Name: "A", Type: bigquery.StringFieldType}}), http.StatusBadRequest)
	for _, table := range []string{"t!", "twice"} {
		if _, err := ds.Table(table).Metadata(ctx); err == nil {
			t.Errorf("the refused load made %s", table)
		}
	}
	if err := load("loaded-1", one); err != nil {
		t.Fatalf("a load into a table BigQuery can name: %v", err)
	}
	if n := countRows(t, h, ds.Table("loaded-1")); n != 1 {
		t.Errorf("the load wrote %d rows, want 1", n)
	}
}
