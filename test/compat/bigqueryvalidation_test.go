//go:build compat

package compat

import (
	"errors"
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

// TestBigQueryRefusesRecordNestingTheEmulatorCannotRead (#874): the
// emulator takes a RECORD in a REPEATED RECORD, and a REPEATED RECORD in a
// RECORD, and then fails every read of the table once a row holds a value
// there (500 "failed to scan rows", measured), so the front refuses such a
// schema as not implemented, 501, at once. RECORDs nested with none
// REPEATED, and a REPEATED RECORD of scalars, are created, written and read
// back.
func TestBigQueryRefusesRecordNestingTheEmulatorCannotRead(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	rec := func(name string, repeated bool, fields ...*bigquery.FieldSchema) *bigquery.FieldSchema {
		return &bigquery.FieldSchema{Name: name, Type: bigquery.RecordFieldType, Repeated: repeated, Schema: fields}
	}
	str := &bigquery.FieldSchema{Name: "s", Type: bigquery.StringFieldType}
	for name, schema := range map[string]bigquery.Schema{
		"rec_in_repeated": {rec("a", true, rec("b", false, str))},
		"repeated_in_rec": {rec("a", false, rec("b", true, str))},
	} {
		start := time.Now()
		err := ds.Table(name).Create(h.Context(), &bigquery.TableMetadata{Schema: schema})
		wantHTTPStatus(t, "creating "+name, err, http.StatusNotImplemented)
		if took := time.Since(start); took > 10*time.Second {
			t.Errorf("%s was refused after %s: the 501 was retried", name, took)
		}
	}

	tbl := ds.Table("readable")
	schema := bigquery.Schema{rec("a", false, str, rec("b", false, rec("c", false, str))),
		rec("list", true, str, &bigquery.FieldSchema{Name: "tags", Type: bigquery.StringFieldType, Repeated: true})}
	if err := tbl.Create(h.Context(), &bigquery.TableMetadata{Schema: schema}); err != nil {
		t.Fatalf("create a table of RECORDs the emulator reads: %v", err)
	}
	row := mapRow{
		"a":    map[string]bigquery.Value{"s": "x", "b": map[string]bigquery.Value{"c": map[string]bigquery.Value{"s": "y"}}},
		"list": []bigquery.Value{map[string]bigquery.Value{"s": "z", "tags": []bigquery.Value{"p", "q"}}},
	}
	if err := tbl.Inserter().Put(h.Context(), []mapRow{row}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if n := countRows(t, h, tbl); n != 1 {
		t.Errorf("the table reads back %d rows, want 1", n)
	}
}
