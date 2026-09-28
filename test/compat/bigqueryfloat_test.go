//go:build compat

package compat

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
)

// floatValues are FLOAT64 values a 32-bit float cannot hold: beyond its
// range, beyond its precision, and the smallest ones.
var floatValues = []float64{0.1, 0.30000000000000004, 1.7976931348623157e308, -2.2250738585072014e-308, 16777217,
	123456789.12345679, 5e-324, -1e300}

// TestBigQueryFloatColumnsTakeFloat64 (#1000), through the official Go
// client. Measured first: a FLOAT column (what the Go client sends for
// bigquery.FloatFieldType) made through tables.insert, a load's schema or
// a CSV load with autodetect was the engine's 32-bit FLOAT, which refused
// FLOAT64 values: "Value has type DOUBLE which cannot be inserted into
// column f, which has type FLOAT". Now each such column takes FLOAT64
// values through INSERT with a query parameter, INSERT ... SELECT of a
// FLOAT64 column, a FLOAT64 struct field, streaming, and loads, and reads
// every value back exactly; the tables read back FLOAT, as they were made.
func TestBigQueryFloatColumnsTakeFloat64(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	d := ds.DatasetID

	tbl := ds.Table("floats")
	schema := bigquery.Schema{
		{Name: "id", Type: bigquery.IntegerFieldType},
		{Name: "f", Type: bigquery.FloatFieldType},
		{Name: "r", Type: bigquery.RecordFieldType, Schema: bigquery.Schema{{Name: "x", Type: bigquery.FloatFieldType}}},
	}
	if err := tbl.Create(ctx, &bigquery.TableMetadata{Schema: schema}); err != nil {
		t.Fatalf("Table.Create: %v", err)
	}
	meta, err := tbl.Metadata(ctx)
	if err != nil {
		t.Fatalf("Table.Metadata: %v", err)
	}
	if got := schemaText(meta.Schema); got != schemaText(schema) {
		t.Errorf("the table reads %s, want %s", got, schemaText(schema))
	}

	want := map[int64]float64{}
	// INSERT with a FLOAT64 query parameter, into the column and the
	// struct field.
	for i, v := range floatValues {
		q := c.Query("INSERT INTO " + d + ".floats (id, f, r) VALUES (@id, @f, STRUCT(@f AS x))")
		q.Parameters = []bigquery.QueryParameter{{Name: "id", Value: int64(i)}, {Name: "f", Value: v}}
		if _, err := q.Read(ctx); err != nil {
			t.Fatalf("INSERT of %v: %v", v, err)
		}
		want[int64(i)] = v
	}
	// A literal CAST AS FLOAT64, and INSERT ... SELECT of a DDL FLOAT64
	// table's column.
	if err := bqRun(ctx, c, "INSERT INTO "+d+".floats (id, f) VALUES (100, CAST(0.1 AS FLOAT64))", false); err != nil {
		t.Errorf("INSERT of CAST(0.1 AS FLOAT64): %v", err)
	}
	want[100] = 0.1
	if err := bqRun(ctx, c, "CREATE TABLE "+d+".src (id INT64, f FLOAT64)", false); err != nil {
		t.Fatal(err)
	}
	for i, v := range floatValues {
		q := c.Query("INSERT INTO " + d + ".src (id, f) VALUES (@id, @f)")
		q.Parameters = []bigquery.QueryParameter{{Name: "id", Value: int64(200 + i)}, {Name: "f", Value: v}}
		if _, err := q.Read(ctx); err != nil {
			t.Fatalf("INSERT into the FLOAT64 table: %v", err)
		}
		want[int64(200+i)] = v
	}
	if err := bqRun(ctx, c, "INSERT INTO "+d+".floats (id, f) SELECT id, f FROM "+d+".src", true); err != nil {
		t.Errorf("INSERT ... SELECT of a FLOAT64 column (query job): %v", err)
	}
	// Streaming.
	var rows []*bigquery.ValuesSaver
	for i, v := range floatValues {
		rows = append(rows, &bigquery.ValuesSaver{Schema: schema[:2], Row: []bigquery.Value{int64(300 + i), v}})
		want[int64(300+i)] = v
	}
	if err := tbl.Inserter().Put(ctx, rows); err != nil {
		t.Errorf("tabledata.insertAll: %v", err)
	}
	// A load with the schema, appending.
	var csv strings.Builder
	for i, v := range floatValues {
		fmt.Fprintf(&csv, "%d,%s\n", 400+i, strconv.FormatFloat(v, 'g', -1, 64))
		want[int64(400+i)] = v
	}
	load := func(table string, src *bigquery.ReaderSource) {
		t.Helper()
		lctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		l := ds.Table(table).LoaderFrom(src)
		l.WriteDisposition = bigquery.WriteAppend
		job, err := l.Run(lctx)
		if err != nil {
			t.Fatalf("load into %s: %v", table, err)
		}
		st, err := job.Wait(lctx)
		if err == nil {
			err = st.Err()
		}
		if err != nil {
			t.Fatalf("load into %s: %v", table, err)
		}
	}
	src := bigquery.NewReaderSource(strings.NewReader(csv.String()))
	src.Schema = schema[:2]
	load("floats", src)

	check := func(table string, want map[int64]float64) {
		t.Helper()
		it, err := c.Query("SELECT id, f FROM " + d + "." + table + " ORDER BY id").Read(ctx)
		if err != nil {
			t.Fatalf("read %s: %v", table, err)
		}
		got := map[int64]float64{}
		for {
			var row []bigquery.Value
			err := it.Next(&row)
			if errors.Is(err, iterator.Done) {
				break
			}
			if err != nil {
				t.Fatalf("read %s: %v", table, err)
			}
			id, _ := row[0].(int64)
			f, ok := row[1].(float64)
			if !ok {
				t.Errorf("%s row %d: f is %T %v", table, id, row[1], row[1])
			}
			got[id] = f
		}
		for id, v := range want {
			if g, ok := got[id]; !ok || g != v {
				t.Errorf("%s row %d: f read back %v (there %v), want %v", table, id, g, ok, v)
			}
		}
		if len(got) != len(want) {
			t.Errorf("%s has %d rows, want %d", table, len(got), len(want))
		}
	}
	check("floats", want)
	for i, v := range floatValues {
		if got := bqValue(ctx, c, fmt.Sprintf("SELECT r.x FROM %s.floats WHERE id = %d", d, i)); got != fmt.Sprint([]bigquery.Value{v}) {
			t.Errorf("r.x of row %d read back %s, want %v", i, got, v)
		}
	}

	// A load with the schema into a new table, and a CSV load with
	// autodetect: each takes FLOAT64 afterwards and reads back FLOAT.
	src = bigquery.NewReaderSource(strings.NewReader(csv.String()))
	src.Schema = schema[:2]
	load("loaded", src)
	var auto strings.Builder
	auto.WriteString("id,f\n")
	auto.WriteString(csv.String())
	src = bigquery.NewReaderSource(strings.NewReader(auto.String()))
	src.AutoDetect = true
	load("detected", src)
	for _, table := range []string{"loaded", "detected"} {
		m, err := ds.Table(table).Metadata(ctx)
		if err != nil {
			t.Fatalf("%s: %v", table, err)
		}
		if len(m.Schema) != 2 || m.Schema[1].Type != bigquery.FloatFieldType {
			t.Errorf("%s reads %s, want f FLOAT", table, schemaText(m.Schema))
		}
		q := c.Query("INSERT INTO " + d + "." + table + " (id, f) VALUES (999, @f)")
		q.Parameters = []bigquery.QueryParameter{{Name: "f", Value: 16777217.0}}
		if _, err := q.Read(ctx); err != nil {
			t.Errorf("INSERT of a FLOAT64 into %s: %v", table, err)
		}
		w := map[int64]float64{999: 16777217}
		for i, v := range floatValues {
			w[int64(400+i)] = v
		}
		check(table, w)
	}
}

// TestBigQueryDropSchemaDropsRoutinesInsertFunctions (#1001), through the
// official Go client. Measured first: a function made through
// routines.insert (Routine.Create) was callable, and DROP SCHEMA neither
// counted it for RESTRICT nor dropped it for CASCADE (the front knew only
// the functions CREATE FUNCTION statements made). Now RESTRICT fails
// resourceInUse, and CASCADE drops the dataset and the function.
func TestBigQueryDropSchemaDropsRoutinesInsertFunctions(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	name := ds.DatasetID + "_rt"
	rds := c.Dataset(name)
	if err := rds.Create(ctx, nil); err != nil {
		t.Fatalf("create dataset: %v", err)
	}
	t.Cleanup(func() { _ = rds.DeleteWithContents(context.Background()) })
	err := rds.Routine("viarest").Create(ctx, &bigquery.RoutineMetadata{
		Type:      "SCALAR_FUNCTION",
		Language:  "SQL",
		Body:      "x * 10",
		Arguments: []*bigquery.RoutineArgument{{Name: "x", DataType: &bigquery.StandardSQLDataType{TypeKind: "INT64"}}},
	})
	if err != nil {
		t.Fatalf("Routine.Create: %v", err)
	}
	if got := bqValue(ctx, c, "SELECT "+name+".viarest(2)"); got != "[20]" {
		t.Fatalf("viarest(2) = %s, want [20]", got)
	}
	for _, insert := range []bool{false, true} {
		if got := errReason(bqRun(ctx, c, "DROP SCHEMA "+name, insert)); got != "resourceInUse" {
			t.Errorf("DROP SCHEMA of a dataset with a function Routine.Create made (query job %v): %q, want resourceInUse", insert, got)
		}
	}
	if err := bqRun(ctx, c, "DROP SCHEMA "+name+" CASCADE", false); err != nil {
		t.Fatalf("DROP SCHEMA CASCADE: %v", err)
	}
	_, err = rds.Metadata(ctx)
	var ge *googleapi.Error
	if !errors.As(err, &ge) || ge.Code != http.StatusNotFound {
		t.Errorf("after DROP SCHEMA CASCADE, datasets.get: %v, want 404", err)
	}
	if got := bqValue(ctx, c, "SELECT "+name+".viarest(2)"); !strings.Contains(got, "Function not found") {
		t.Errorf("after DROP SCHEMA CASCADE, viarest(2) = %s, want not found", got)
	}
}
