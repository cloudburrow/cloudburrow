//go:build compat

package compat

import (
	"encoding/json"
	"math"
	"reflect"
	"testing"

	"cloud.google.com/go/bigquery"
)

// wideRow is a row of TestBigQueryInserterKeepsWideNumbers, which the
// official client's Inserter sends with each int64 as a JSON number.
type wideRow struct {
	I   int64   `bigquery:"i"`
	W   int64   `bigquery:"w"`
	R   []int64 `bigquery:"r"`
	Rec struct {
		X int64 `bigquery:"x"`
	} `bigquery:"rec"`
}

// numbersRow is a ValueSaver whose NUMERIC and BIGNUMERIC values are JSON
// numbers, as a client that saves decoded JSON sends them.
type numbersRow struct{ n, b string }

func (r numbersRow) Save() (map[string]bigquery.Value, string, error) {
	return map[string]bigquery.Value{"i": 3, "n": json.Number(r.n), "b": json.Number(r.b)}, "", nil
}

// TestBigQueryInserterKeepsWideNumbers (#1129): an INT64 beyond 2^53 that
// the official Go client's Inserter streams (tabledata.insertAll, the
// value a JSON number) is stored with every digit, at the top level, in a
// REPEATED column and in a RECORD, and so are a NUMERIC and a BIGNUMERIC
// sent as JSON numbers; read back with Table.Read. Measured first through
// the front: 9007199254740993 read back 9007199254740992, the NUMERIC
// 12345678901234567890.123456789 12345678901234567168.
func TestBigQueryInserterKeepsWideNumbers(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	tbl := ds.Table("wide")
	schema := bigquery.Schema{
		{Name: "i", Type: bigquery.IntegerFieldType},
		{Name: "w", Type: bigquery.IntegerFieldType},
		{Name: "r", Type: bigquery.IntegerFieldType, Repeated: true},
		{Name: "rec", Type: bigquery.RecordFieldType, Schema: bigquery.Schema{{Name: "x", Type: bigquery.IntegerFieldType}}},
		{Name: "n", Type: bigquery.NumericFieldType},
		{Name: "b", Type: bigquery.BigNumericFieldType},
	}
	if err := tbl.Create(ctx, &bigquery.TableMetadata{Schema: schema}); err != nil {
		t.Fatal(err)
	}
	one := wideRow{I: 1, W: 9007199254740993, R: []int64{9007199254740995, math.MinInt64}}
	one.Rec.X = math.MaxInt64
	two := wideRow{I: 2, W: -9007199254740993, R: []int64{}}
	two.Rec.X = 1<<53 + 1
	if err := tbl.Inserter().Put(ctx, []wideRow{one, two}); err != nil {
		t.Fatalf("Inserter.Put: %v", err)
	}
	if err := tbl.Inserter().Put(ctx, numbersRow{n: "12345678901234567890.123456789", b: "1234567890123456789012345678.12345678901"}); err != nil {
		t.Fatalf("Inserter.Put of a ValueSaver: %v", err)
	}
	got := readRows(t, tbl.Read(ctx), "Table.Read")
	want := []string{
		"1|9007199254740993|[9007199254740995 -9223372036854775808]|[9223372036854775807]|<nil>|<nil>",
		"2|-9007199254740993|[]|[9007199254740993]|<nil>|<nil>",
		"3|<nil>|[]|<nil>|12345678901234567890123456789/1000000000|123456789012345678901234567812345678901/100000000000",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Table.Read:\n%v\nwant\n%v", got, want)
	}
}

// TestBigQueryDatasetIDIsTheDefaultDataset (#1137): @@dataset_id reads the
// query's default dataset's ID, through jobs.query and a query job, whose
// jobs.get shows the client's text; with no default dataset it is NULL;
// and in a query with the default dataset two that calls one.fn, which
// the front sends with no default dataset (#1107). Measured first through
// the front: SELECT @@dataset_id with a default dataset read NULL.
func TestBigQueryDatasetIDIsTheDefaultDataset(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	base := validationDataset(t, h, c)
	ctx := h.Context()
	one, two := base.DatasetID+"_one", base.DatasetID+"_two"
	for _, d := range []string{one, two} {
		if err := c.Dataset(d).Create(ctx, nil); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Dataset(d).DeleteWithContents(ctx) })
	}
	if err := bqRun(ctx, c, "CREATE FUNCTION "+one+".fn(x INT64) AS (x + 1)", false); err != nil {
		t.Fatal(err)
	}
	for _, q := range []struct{ dataset, sql, want string }{
		{two, "SELECT @@dataset_id", two},
		{one, "SELECT @@dataset_id = '" + one + "', '@@dataset_id'", "true|@@dataset_id"},
		{"", "SELECT @@dataset_id IS NULL", "true"},
		{two, "SELECT " + one + ".fn(1), @@dataset_id", "2|" + two},
	} {
		if got := queryRows(t, ctx, c, project, q.dataset, q.sql); !reflect.DeepEqual(got, []string{q.want}) {
			t.Errorf("%s with the default dataset %q: %v, want %s", q.sql, q.dataset, got, q.want)
		}
	}
	q := c.Query("SELECT @@dataset_id AS d")
	q.DefaultProjectID, q.DefaultDatasetID = project, two
	job, err := q.Run(ctx)
	if err != nil {
		t.Fatalf("the query job: %v", err)
	}
	it, err := job.Read(ctx)
	if err != nil {
		t.Fatalf("the query job's rows: %v", err)
	}
	if got := readRows(t, it, "the query job"); !reflect.DeepEqual(got, []string{two}) {
		t.Errorf("the query job read %v, want [%s]", got, two)
	}
	again, err := c.JobFromID(ctx, job.ID())
	if err != nil {
		t.Fatalf("jobs.get: %v", err)
	}
	if cfg, err := again.Config(); err != nil {
		t.Errorf("the job's configuration: %v", err)
	} else if qc, ok := cfg.(*bigquery.QueryConfig); !ok || qc.Q != "SELECT @@dataset_id AS d" {
		t.Errorf("jobs.get shows %+v, want the client's text", cfg)
	}
}
