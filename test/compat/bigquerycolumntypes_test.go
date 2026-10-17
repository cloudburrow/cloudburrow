//go:build compat

package compat

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	"cloud.google.com/go/civil"
	bq "google.golang.org/api/bigquery/v2"
	"google.golang.org/api/option"
)

// rangeText writes a RANGE value as "[start, end)", a bound nil as
// UNBOUNDED and a TIMESTAMP bound in UTC, or "<nil>" for a NULL.
func rangeText(v bigquery.Value) string {
	r, ok := v.(*bigquery.RangeValue)
	if !ok || r == nil {
		return fmt.Sprint(v)
	}
	bound := func(b bigquery.Value) string {
		switch b := b.(type) {
		case nil:
			return "UNBOUNDED"
		case time.Time:
			return b.UTC().Format(time.RFC3339)
		}
		return fmt.Sprint(b)
	}
	return "[" + bound(r.Start) + ", " + bound(r.End) + ")"
}

// rangeRows runs sql through jobs.query and writes each row's values
// joined by "|", a RANGE as rangeText, a REPEATED one's elements in [].
func rangeRows(t *testing.T, ctx context.Context, c *bigquery.Client, sql string) []string {
	t.Helper()
	_, rows, err := infoSchemaRead(ctx, c, sql, false)
	if err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	var out []string
	for _, row := range rows {
		parts := make([]string, len(row))
		for i, v := range row {
			if arr, ok := v.([]bigquery.Value); ok {
				var es []string
				for _, e := range arr {
					es = append(es, rangeText(e))
				}
				parts[i] = "[" + strings.Join(es, " ") + "]"
				continue
			}
			parts[i] = rangeText(v)
		}
		out = append(out, strings.Join(parts, "|"))
	}
	return out
}

// TestBigQueryRangeColumnsThroughTheAPI (#1492): a table whose schema
// has RANGE<DATE>, RANGE<DATETIME> and RANGE<TIMESTAMP> columns, a
// REPEATED one and one in a RECORD, is made by tables.insert, its schema
// read back by tables.get, rows of it streamed (tabledata.insertAll, the
// Go client's RangeValue), loaded from newline-delimited JSON and CSV and
// written by DML, and read back by a query with the RANGE functions;
// tables.patch and tables.update each add a RANGE column, and COLUMNS
// lists every one with its RANGE<T> data_type
// (https://cloud.google.com/bigquery/docs/reference/rest/v2/tables#TableFieldSchema.FIELDS.range_element_type).
// Measured first: tables.insert failed 500 "Type not found:
// TYPE_UNKNOWN", which the Go client retried for 60 s, and insertAll of a
// RangeValue 400 "found a JSON object".
//
// covers: bigquery.tables.insert, bigquery.tables.get, bigquery.tables.patch, bigquery.tables.update, bigquery.tabledata.insertAll, bigquery.jobs.insert, bigquery.jobs.query
func TestBigQueryRangeColumnsThroughTheAPI(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	ctx := h.Context()
	ds := validationDataset(t, h, c)
	rng := func(name string, elem bigquery.FieldType) *bigquery.FieldSchema {
		return &bigquery.FieldSchema{Name: name, Type: bigquery.RangeFieldType, RangeElementType: &bigquery.RangeElementType{Type: elem}}
	}
	ra := rng("ra", bigquery.DateFieldType)
	ra.Repeated = true
	schema := bigquery.Schema{
		{Name: "id", Type: bigquery.IntegerFieldType},
		rng("rd", bigquery.DateFieldType),
		rng("rdt", bigquery.DateTimeFieldType),
		rng("rts", bigquery.TimestampFieldType),
		ra,
		{Name: "st", Type: bigquery.RecordFieldType, Schema: bigquery.Schema{rng("x", bigquery.DateFieldType)}},
	}
	tbl := ds.Table("ranges")
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	err := tbl.Create(cctx, &bigquery.TableMetadata{Schema: schema})
	cancel()
	if err != nil {
		t.Fatalf("tables.insert of RANGE columns: %v", err)
	}
	md, err := tbl.Metadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	describe := func(s bigquery.Schema) string {
		var parts []string
		var walk func(prefix string, s bigquery.Schema)
		walk = func(prefix string, s bigquery.Schema) {
			for _, f := range s {
				d := prefix + f.Name + " " + string(f.Type)
				if f.RangeElementType != nil {
					d += "<" + string(f.RangeElementType.Type) + ">"
				}
				if f.Repeated {
					d += " REPEATED"
				}
				parts = append(parts, d)
				walk(prefix+f.Name+".", f.Schema)
			}
		}
		walk("", s)
		return strings.Join(parts, ", ")
	}
	if got, want := describe(md.Schema), "id INTEGER, rd RANGE<DATE>, rdt RANGE<DATETIME>, rts RANGE<TIMESTAMP>, ra RANGE<DATE> REPEATED, st RECORD, st.x RANGE<DATE>"; got != want {
		t.Errorf("tables.get schema: %s, want %s", got, want)
	}

	// tabledata.insertAll: the Go client sends a RangeValue as an object
	// of start and end, a bound nil for UNBOUNDED.
	d := func(y int, m time.Month, day int) civil.Date { return civil.Date{Year: y, Month: m, Day: day} }
	ts := time.Date(2024, 4, 11, 10, 30, 0, 0, time.UTC)
	if err := tbl.Inserter().Put(ctx, []*bigquery.ValuesSaver{{Schema: schema, InsertID: "1", Row: []bigquery.Value{
		1,
		&bigquery.RangeValue{Start: d(2020, 1, 1), End: d(2020, 2, 1)},
		&bigquery.RangeValue{Start: civil.DateTime{Date: d(2020, 1, 1), Time: civil.Time{Hour: 10}}},
		&bigquery.RangeValue{Start: ts, End: ts.Add(time.Hour)},
		[]bigquery.Value{&bigquery.RangeValue{End: d(2021, 1, 1)}},
		[]bigquery.Value{&bigquery.RangeValue{Start: d(2022, 1, 1), End: d(2022, 6, 1)}},
	}}}); err != nil {
		t.Fatalf("tabledata.insertAll of RANGE values: %v", err)
	}
	// Loads: newline-delimited JSON (objects of start and end), and CSV
	// (the text "[start, end)") into a table of the flat columns, as CSV
	// holds no REPEATED or RECORD column.
	flat := ds.Table("flat_ranges")
	if err := flat.Create(ctx, &bigquery.TableMetadata{Schema: schema[:4]}); err != nil {
		t.Fatalf("tables.insert of RANGE columns: %v", err)
	}
	for _, l := range []struct {
		to     *bigquery.Table
		format bigquery.DataFormat
		data   string
	}{
		{tbl, bigquery.JSON, `{"id": 2, "rd": {"start": "2023-03-01", "end": null}, "rts": {"start": "2024-04-11 10:30:00 UTC", "end": "2024-04-11T11:30:00Z"}, "ra": [{"start": "2023-01-01", "end": "2023-02-01"}], "st": {"x": {"start": null, "end": "2023-05-01"}}}` + "\n"},
		{flat, bigquery.CSV, "3,\"[2023-07-01, 2023-08-01)\",\"[2023-07-01T00:00:00, UNBOUNDED)\",\"[UNBOUNDED, 2024-04-11 10:30:00 UTC)\"\n"},
	} {
		src := bigquery.NewReaderSource(strings.NewReader(l.data))
		src.SourceFormat = l.format
		ld := l.to.LoaderFrom(src)
		ld.WriteDisposition = bigquery.WriteAppend
		lctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		job, err := ld.Run(lctx)
		if err == nil {
			var st *bigquery.JobStatus
			if st, err = job.Wait(lctx); err == nil {
				err = st.Err()
			}
		}
		cancel()
		if err != nil {
			t.Errorf("%s load of RANGE values: %v", l.format, err)
		}
	}
	if got := rangeRows(t, ctx, c, "SELECT * FROM `"+ds.DatasetID+".flat_ranges`"); !reflect.DeepEqual(got,
		[]string{"3|[2023-07-01, 2023-08-01)|[2023-07-01T00:00:00, UNBOUNDED)|[UNBOUNDED, 2024-04-11T10:30:00Z)"}) {
		t.Errorf("the CSV load's row: %q", got)
	}
	// DML, through jobs.insert.
	if err := bqRun(ctx, c, "INSERT `"+ds.DatasetID+".ranges` (id, rd, rts) VALUES (4, RANGE<DATE> '[2025-01-01, 2025-12-31)', "+
		"RANGE(TIMESTAMP '2025-01-01 00:00:00 UTC', NULL))", true); err != nil {
		t.Errorf("INSERT of RANGE values: %v", err)
	}
	got := rangeRows(t, ctx, c, "SELECT id, rd, rdt, rts, ra, st.x FROM `"+ds.DatasetID+".ranges` ORDER BY id")
	want := []string{
		"1|[2020-01-01, 2020-02-01)|[2020-01-01T10:00:00, UNBOUNDED)|[2024-04-11T10:30:00Z, 2024-04-11T11:30:00Z)|[[UNBOUNDED, 2021-01-01)]|[2022-01-01, 2022-06-01)",
		"2|[2023-03-01, UNBOUNDED)|<nil>|[2024-04-11T10:30:00Z, 2024-04-11T11:30:00Z)|[[2023-01-01, 2023-02-01)]|[UNBOUNDED, 2023-05-01)",
		"4|[2025-01-01, 2025-12-31)|<nil>|[2025-01-01T00:00:00Z, UNBOUNDED)|[]|<nil>",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("rows:\n got %q\nwant %q", got, want)
	}
	if got := rangeRows(t, ctx, c, "SELECT id, RANGE_START(rd), RANGE_CONTAINS(rd, DATE '2020-01-15') FROM `"+ds.DatasetID+
		".ranges` WHERE rd IS NOT NULL ORDER BY id"); !reflect.DeepEqual(got, []string{"1|2020-01-01|true", "2|2023-03-01|false",
		"4|2025-01-01|false"}) {
		t.Errorf("RANGE_START and RANGE_CONTAINS: %q", got)
	}

	// tables.patch (Table.Update) adds a RANGE<DATETIME> column, and
	// tables.update (PUT, the generated client) a RANGE<TIMESTAMP> one; the
	// rows read NULL in them and DML writes them.
	added := append(md.Schema, rng("patched", bigquery.DateTimeFieldType))
	if _, err := tbl.Update(ctx, bigquery.TableMetadataToUpdate{Schema: added}, md.ETag); err != nil {
		t.Fatalf("tables.patch adding a RANGE column: %v", err)
	}
	svc, err := bq.NewService(ctx, option.WithEndpoint(h.Endpoint(EnvBigQuery)), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	full, err := svc.Tables.Get(project, ds.DatasetID, "ranges").Context(ctx).Do()
	if err != nil {
		t.Fatal(err)
	}
	full.Schema.Fields = append(full.Schema.Fields, &bq.TableFieldSchema{Name: "updated", Type: "RANGE",
		RangeElementType: &bq.TableFieldSchemaRangeElementType{Type: "TIMESTAMP"}})
	if _, err := svc.Tables.Update(project, ds.DatasetID, "ranges", full).Context(ctx).Do(); err != nil {
		t.Fatalf("tables.update adding a RANGE column: %v", err)
	}
	if err := bqRun(ctx, c, "UPDATE `"+ds.DatasetID+".ranges` SET patched = RANGE(DATETIME '2026-01-01 00:00:00', NULL), "+
		"updated = RANGE(NULL, TIMESTAMP '2026-01-01 00:00:00 UTC') WHERE id = 1", false); err != nil {
		t.Fatalf("UPDATE of the added RANGE columns: %v", err)
	}
	if got := rangeRows(t, ctx, c, "SELECT id, patched, updated FROM `"+ds.DatasetID+".ranges` ORDER BY id"); !reflect.DeepEqual(got,
		[]string{"1|[2026-01-01T00:00:00, UNBOUNDED)|[UNBOUNDED, 2026-01-01T00:00:00Z)", "2|<nil>|<nil>", "4|<nil>|<nil>"}) {
		t.Errorf("the added RANGE columns: %q", got)
	}
	if md, err = tbl.Metadata(ctx); err != nil {
		t.Fatal(err)
	}
	if got, want := describe(md.Schema), "id INTEGER, rd RANGE<DATE>, rdt RANGE<DATETIME>, rts RANGE<TIMESTAMP>, ra RANGE<DATE> REPEATED, "+
		"st RECORD, st.x RANGE<DATE>, patched RANGE<DATETIME>, updated RANGE<TIMESTAMP>"; got != want {
		t.Errorf("tables.get schema after the updates: %s, want %s", got, want)
	}

	_, rows, err := infoSchemaRead(ctx, c, "SELECT column_name, data_type FROM `"+ds.DatasetID+
		"`.INFORMATION_SCHEMA.COLUMNS WHERE table_name = 'ranges' ORDER BY ordinal_position", false)
	if err != nil {
		t.Fatal(err)
	}
	var cols []string
	for _, r := range rows {
		cols = append(cols, fmt.Sprintf("%v %v", r[0], r[1]))
	}
	if want := []string{"id INT64", "rd RANGE<DATE>", "rdt RANGE<DATETIME>", "rts RANGE<TIMESTAMP>", "ra ARRAY<RANGE<DATE>>",
		"st STRUCT<x RANGE<DATE>>", "patched RANGE<DATETIME>", "updated RANGE<TIMESTAMP>"}; !reflect.DeepEqual(cols, want) {
		t.Errorf("COLUMNS:\n got %q\nwant %q", cols, want)
	}
}

// TestBigQueryParameterizedColumnsByDDL (#1493): CREATE TABLE, CREATE
// TABLE AS SELECT and ALTER TABLE ADD COLUMN keep the parameters of
// STRING(L), BYTES(L), NUMERIC(P, S) and BIGNUMERIC(P, S), at the top
// level, in a STRUCT and as an ARRAY's element: tables.get reads
// maxLength, precision and scale, and COLUMNS and COLUMN_FIELD_PATHS show
// the parameterized type, as for a table made by tables.insert
// (https://cloud.google.com/bigquery/docs/reference/standard-sql/data-types#parameterized_data_types).
// Measured first: CREATE TABLE (ps STRING(10)) made the table with no
// maxLength, and STRUCT<a STRING(3)> failed 400 "GOOGLESQL_RET_CHECK
// failure ... annotations != nullptr". tabledata.insertAll of a value
// longer than its column's maximum length is refused, as BigQuery refuses
// it.
//
// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.tables.get, bigquery.tabledata.insertAll
func TestBigQueryParameterizedColumnsByDDL(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ctx := h.Context()
	ds := validationDataset(t, h, c)
	for _, sql := range []string{
		"CREATE TABLE `" + ds.DatasetID + ".ddl` (ps STRING(10), pb BYTES(5), pn NUMERIC(5, 2), pbn BIGNUMERIC(40, 10), " +
			"st STRUCT<a STRING(3), b BYTES(2), n NUMERIC(4, 1)>, arr ARRAY<STRING(4)>)",
		"CREATE TABLE `" + ds.DatasetID + ".ctas` (ps STRING(10), pn NUMERIC(5, 2)) AS SELECT 'abc' AS ps, NUMERIC '1.5' AS pn",
		"ALTER TABLE `" + ds.DatasetID + ".ddl` ADD COLUMN q BYTES(3), ADD COLUMN qs STRUCT<z STRING(7)>",
	} {
		if err := bqRun(ctx, c, sql, true); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	describe := func(name string) string {
		md, err := ds.Table(name).Metadata(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var parts []string
		var walk func(prefix string, s bigquery.Schema)
		walk = func(prefix string, s bigquery.Schema) {
			for _, f := range s {
				d := prefix + f.Name + " " + string(f.Type)
				switch {
				case f.MaxLength != 0:
					d += fmt.Sprintf("(%d)", f.MaxLength)
				case f.Precision != 0:
					d += fmt.Sprintf("(%d, %d)", f.Precision, f.Scale)
				}
				parts = append(parts, d)
				walk(prefix+f.Name+".", f.Schema)
			}
		}
		walk("", md.Schema)
		return strings.Join(parts, ", ")
	}
	if got, want := describe("ddl"), "ps STRING(10), pb BYTES(5), pn NUMERIC(5, 2), pbn BIGNUMERIC(40, 10), st RECORD, "+
		"st.a STRING(3), st.b BYTES(2), st.n NUMERIC(4, 1), arr STRING(4), q BYTES(3), qs RECORD, qs.z STRING(7)"; got != want {
		t.Errorf("tables.get of the CREATE TABLE table:\n got %s\nwant %s", got, want)
	}
	if got, want := describe("ctas"), "ps STRING(10), pn NUMERIC(5, 2)"; got != want {
		t.Errorf("tables.get of the CREATE TABLE AS SELECT table: %s, want %s", got, want)
	}
	for _, v := range []struct {
		view, name string
		want       []string
	}{
		{"COLUMNS", "column_name", []string{"ctas.pn NUMERIC(5, 2)", "ctas.ps STRING(10)", "ddl.arr ARRAY<STRING(4)>",
			"ddl.pb BYTES(5)", "ddl.pbn BIGNUMERIC(40, 10)", "ddl.pn NUMERIC(5, 2)", "ddl.ps STRING(10)", "ddl.q BYTES(3)",
			"ddl.qs STRUCT<z STRING(7)>", "ddl.st STRUCT<a STRING(3), b BYTES(2), n NUMERIC(4, 1)>"}},
		{"COLUMN_FIELD_PATHS", "field_path", []string{"ctas.pn NUMERIC(5, 2)", "ctas.ps STRING(10)", "ddl.arr ARRAY<STRING(4)>",
			"ddl.pb BYTES(5)", "ddl.pbn BIGNUMERIC(40, 10)", "ddl.pn NUMERIC(5, 2)", "ddl.ps STRING(10)", "ddl.q BYTES(3)",
			"ddl.qs STRUCT<z STRING(7)>", "ddl.qs.z STRING(7)", "ddl.st STRUCT<a STRING(3), b BYTES(2), n NUMERIC(4, 1)>",
			"ddl.st.a STRING(3)", "ddl.st.b BYTES(2)", "ddl.st.n NUMERIC(4, 1)"}},
	} {
		_, rows, err := infoSchemaRead(ctx, c, "SELECT table_name, "+v.name+", data_type FROM `"+ds.DatasetID+
			"`.INFORMATION_SCHEMA."+v.view, true)
		if err != nil {
			t.Fatalf("%s: %v", v.view, err)
		}
		var got []string
		for _, r := range rows {
			got = append(got, fmt.Sprintf("%v.%v %v", r[0], r[1], r[2]))
		}
		sort.Strings(got)
		if !reflect.DeepEqual(got, v.want) {
			t.Errorf("%s:\n got %q\nwant %q", v.view, got, v.want)
		}
	}

	// The limits hold for a stream: a value within them is written, one
	// over them refused.
	if err := ds.Table("ddl").Inserter().Put(ctx, []*bigquery.ValuesSaver{{
		Schema: bigquery.Schema{{Name: "ps", Type: bigquery.StringFieldType}, {Name: "pb", Type: bigquery.BytesFieldType}},
		Row:    []bigquery.Value{"ten chars!", []byte("12345")}}}); err != nil {
		t.Errorf("insertAll within the limits: %v", err)
	}
	for _, row := range []map[string]bigquery.Value{{"ps": "eleven char"}, {"pb": []byte("123456")}} {
		var schema bigquery.Schema
		var vals []bigquery.Value
		for k, v := range row {
			typ := bigquery.StringFieldType
			if k == "pb" {
				typ = bigquery.BytesFieldType
			}
			schema = append(schema, &bigquery.FieldSchema{Name: k, Type: typ})
			vals = append(vals, v)
		}
		err := ds.Table("ddl").Inserter().Put(ctx, []*bigquery.ValuesSaver{{Schema: schema, Row: vals}})
		if err == nil {
			t.Errorf("insertAll of %v over its column's maximum length was taken", row)
		}
	}
}
