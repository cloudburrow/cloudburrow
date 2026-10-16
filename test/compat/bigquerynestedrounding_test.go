//go:build compat

package compat

import (
	"math/big"
	"reflect"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
	"cloud.google.com/go/bigquery/storage/managedwriter"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

// TestBigQueryNestedNumericRounding (#1409), through the official Go
// client: a parameterized NUMERIC(P, S) field of a STRUCT, an element of an
// ARRAY<NUMERIC(P, S)> and a field of an ARRAY<STRUCT> are rounded on write
// as a top-level column is (#1401), by the field's roundingMode, else the
// table's defaultRoundingMode, else ROUND_HALF_AWAY_FROM_ZERO, and a value
// with more than P digits once rounded is refused OUT_OF_RANGE
// (https://cloud.google.com/bigquery/docs/schemas#rounding_mode: "You can
// set the rounding_mode option on a top-level column or a STRUCT field").
// Each write path is checked: DML INSERT and UPDATE (through jobs.query and
// a query job), tabledata.insertAll (an invalid row), a load, a query's
// destination table and the Storage Write API; then CREATE TABLE and
// ALTER TABLE ADD COLUMN with OPTIONS (rounding_mode) on a column and on a
// STRUCT's field, which tables.get reads back.
//
// The row is s STRUCT<a NUMERIC(2), e NUMERIC(2) ROUND_HALF_EVEN>,
// r ARRAY<NUMERIC(2)> and x ARRAY<STRUCT<e NUMERIC(2) ROUND_HALF_EVEN>>,
// with s = (2.5, 2.5), r = [2.5, 3.5], x = [(2.5), (3.5)]: under
// ROUND_HALF_AWAY_FROM_ZERO it reads back s = (3, 2), r = [3, 4],
// x = [(2), (4)], and with the table's ROUND_HALF_EVEN s = (2, 2),
// r = [2, 4].
//
// covers: bigquery.tables.insert, bigquery.tables.patch, bigquery.tables.get, bigquery.jobs.query, bigquery.jobs.insert, bigquery.tabledata.insertAll
func TestBigQueryNestedNumericRounding(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	inner := bigquery.Schema{
		{Name: "a", Type: bigquery.NumericFieldType, Precision: 2},
		{Name: "e", Type: bigquery.NumericFieldType, Precision: 2, RoundingMode: bigquery.RoundHalfEven},
	}
	elem := bigquery.Schema{{Name: "e", Type: bigquery.NumericFieldType, Precision: 2, RoundingMode: bigquery.RoundHalfEven}}
	schema := bigquery.Schema{
		{Name: "id", Type: bigquery.IntegerFieldType},
		{Name: "s", Type: bigquery.RecordFieldType, Schema: inner},
		{Name: "r", Type: bigquery.NumericFieldType, Precision: 2, Repeated: true},
		{Name: "x", Type: bigquery.RecordFieldType, Repeated: true, Schema: elem},
	}
	n := 0
	table := func(mode bigquery.RoundingMode) (*bigquery.Table, string) {
		t.Helper()
		n++
		tbl := ds.Table("nr" + strings.Repeat("x", n))
		if err := tbl.Create(ctx, &bigquery.TableMetadata{Schema: schema}); err != nil {
			t.Fatal(err)
		}
		name := ds.DatasetID + "." + tbl.TableID
		if mode != "" {
			if err := bqRun(ctx, c, "ALTER TABLE "+name+" SET OPTIONS (default_rounding_mode = '"+string(mode)+"')", false); err != nil {
				t.Fatal(err)
			}
		}
		return tbl, name
	}
	check := func(what, name string, want ...string) {
		t.Helper()
		got := queryRows(t, ctx, c, project, ds.DatasetID, "SELECT id, CAST(s.a AS STRING), CAST(s.e AS STRING), "+
			"ARRAY_TO_STRING(ARRAY(SELECT CAST(v AS STRING) FROM UNNEST(r) v WITH OFFSET o ORDER BY o), ','), "+
			"ARRAY_TO_STRING(ARRAY(SELECT CAST(v.e AS STRING) FROM UNNEST(x) v WITH OFFSET o ORDER BY o), ',') FROM "+name+" ORDER BY id")
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: rows %q, want %q", what, got, want)
		}
	}
	const away, even = "1|3|2|3,4|2,4", "1|2|2|2,4|2,4"
	const values = "1, STRUCT(NUMERIC '2.5' AS a, NUMERIC '2.5' AS e), [NUMERIC '2.5', 3.5], [STRUCT(NUMERIC '2.5' AS e), STRUCT(NUMERIC '3.5' AS e)]"
	refused := func(what string, err error) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), "OUT_OF_RANGE") {
			t.Errorf("%s: 99.5 into a NUMERIC(2, 0) field: %v, want OUT_OF_RANGE", what, err)
		}
	}
	overflows := map[string]string{
		"STRUCT field":        "STRUCT(NUMERIC '99.5' AS a, NUMERIC '1' AS e), [], []",
		"ARRAY element":       "NULL, [NUMERIC '1', 99.5], []",
		"ARRAY<STRUCT> field": "NULL, [], [STRUCT(NUMERIC '1' AS e), STRUCT(NUMERIC '-99.5' AS e)]",
	}

	t.Run("DML", func(t *testing.T) {
		for _, insert := range []bool{false, true} {
			_, name := table("")
			run := func(sql string) error { return bqRun(ctx, c, sql, insert) }
			if err := run("INSERT INTO " + name + " (id, s, r, x) VALUES (" + values + ")"); err != nil {
				t.Fatal(err)
			}
			check("DML INSERT", name, away)
			if err := run("UPDATE " + name + " SET s = STRUCT(s.a + 1 AS a, s.e + 1.5 AS e), " +
				"r = ARRAY(SELECT v + 0.5 FROM UNNEST(r) v WITH OFFSET o ORDER BY o), x = [STRUCT(NUMERIC '4.5' AS e)] WHERE true"); err != nil {
				t.Fatal(err)
			}
			// s.a 3 + 1, s.e 2 + 1.5 = 3.5 to even, r 3.5 and 4.5 away from zero.
			check("DML UPDATE", name, "1|4|4|4,5|4")
			for what, v := range overflows {
				refused("DML INSERT, "+what, run("INSERT INTO "+name+" (id, s, r, x) VALUES (2, "+v+")"))
			}
			refused("DML UPDATE", run("UPDATE "+name+" SET r = [NUMERIC '99.5'] WHERE true"))
			check("after the refused DML", name, "1|4|4|4,5|4")
		}
	})

	t.Run("insertAll", func(t *testing.T) {
		for _, mode := range []bigquery.RoundingMode{"", bigquery.RoundHalfEven} {
			tbl, name := table(mode)
			half, threeHalf := big.NewRat(5, 2), big.NewRat(7, 2)
			if err := tbl.Inserter().Put(ctx, &bigquery.ValuesSaver{Schema: schema, Row: []bigquery.Value{1,
				[]bigquery.Value{half, half}, []bigquery.Value{half, threeHalf},
				[]bigquery.Value{[]bigquery.Value{half}, []bigquery.Value{threeHalf}}}}); err != nil {
				t.Fatalf("insertAll: %v", err)
			}
			want := away
			if mode != "" {
				want = even
			}
			check("insertAll, table mode "+string(mode), name, want)
			big1, over := big.NewRat(1, 1), big.NewRat(199, 2)
			for what, row := range map[string][]bigquery.Value{
				"STRUCT field":        {2, []bigquery.Value{over, big1}, nil, nil},
				"ARRAY element":       {2, nil, []bigquery.Value{big1, over}, nil},
				"ARRAY<STRUCT> field": {2, nil, nil, []bigquery.Value{[]bigquery.Value{over}}},
			} {
				refused("insertAll, "+what, tbl.Inserter().Put(ctx, &bigquery.ValuesSaver{Schema: schema, Row: row}))
			}
			check("after the refused rows", name, want)
		}
	})

	t.Run("load", func(t *testing.T) {
		tbl, name := table(bigquery.RoundHalfEven)
		load := func(data string) error {
			src := bigquery.NewReaderSource(strings.NewReader(data))
			src.SourceFormat = bigquery.JSON
			job, err := tbl.LoaderFrom(src).Run(ctx)
			if err != nil {
				return err
			}
			st, err := job.Wait(ctx)
			if err == nil {
				err = st.Err()
			}
			return err
		}
		if err := load(`{"id": 1, "s": {"a": "2.5", "e": 2.5}, "r": ["2.5", 3.5], "x": [{"e": "2.5"}, {"e": "3.5"}]}` + "\n"); err != nil {
			t.Fatalf("load: %v", err)
		}
		check("load, table ROUND_HALF_EVEN", name, even)
		refused("load", load(`{"id": 2, "x": [{"e": "99.5"}]}`+"\n"))
		check("after the refused load", name, even)
	})

	t.Run("query destination", func(t *testing.T) {
		tbl, name := table("")
		q := c.Query("SELECT 1 AS id, STRUCT(NUMERIC '2.5' AS a, NUMERIC '2.5' AS e) AS s, [NUMERIC '2.5', 3.5] AS r, " +
			"[STRUCT(NUMERIC '2.5' AS e), STRUCT(NUMERIC '3.5' AS e)] AS x")
		q.Dst = tbl
		q.WriteDisposition = bigquery.WriteAppend
		if err := runQueryJob(ctx, q); err != nil {
			t.Fatalf("query to a destination: %v", err)
		}
		check("query destination", name, away)
		q = c.Query("SELECT 2 AS id, STRUCT(NUMERIC '99.5' AS a, NUMERIC '1' AS e) AS s")
		q.Dst = tbl
		q.WriteDisposition = bigquery.WriteAppend
		refused("query destination", runQueryJob(ctx, q))
	})

	t.Run("Storage Write API", func(t *testing.T) {
		tbl, name := table("")
		m, dp := writerSchema(t, h, tbl)
		mw := managedWriter(t, h, project)
		ms, err := mw.NewManagedStream(ctx, managedwriter.WithDestinationTable(writeTablePath(tbl)),
			managedwriter.WithType(managedwriter.DefaultStream), managedwriter.WithSchemaDescriptor(dp))
		if err != nil {
			t.Fatalf("NewManagedStream: %v", err)
		}
		defer ms.Close()
		num := func(fd protoreflect.FieldDescriptor, v string) protoreflect.Value {
			if fd.Kind() == protoreflect.StringKind {
				return protoreflect.ValueOfString(v)
			}
			return protoreflect.ValueOfBytes(numericBytes(t, v))
		}
		row := func(id int64, a, e string, r []string, x []string) []byte {
			msg := dynamicpb.NewMessage(m)
			msg.Set(m.Fields().ByName("id"), protoreflect.ValueOfInt64(id))
			if a != "" {
				sfd := m.Fields().ByName("s")
				s := dynamicpb.NewMessage(sfd.Message())
				s.Set(sfd.Message().Fields().ByName("a"), num(sfd.Message().Fields().ByName("a"), a))
				s.Set(sfd.Message().Fields().ByName("e"), num(sfd.Message().Fields().ByName("e"), e))
				msg.Set(sfd, protoreflect.ValueOfMessage(s))
			}
			rfd := m.Fields().ByName("r")
			for _, v := range r {
				msg.Mutable(rfd).List().Append(num(rfd, v))
			}
			xfd := m.Fields().ByName("x")
			efd := xfd.Message().Fields().ByName("e")
			for _, v := range x {
				el := dynamicpb.NewMessage(xfd.Message())
				el.Set(efd, num(efd, v))
				msg.Mutable(xfd).List().Append(protoreflect.ValueOfMessage(el))
			}
			b, _ := proto.Marshal(msg)
			return b
		}
		if _, err := appendResult(t, h, ms, [][]byte{row(1, "2.5", "2.5", []string{"2.5", "3.5"}, []string{"2.5", "3.5"})}); err != nil {
			t.Fatalf("AppendRows: %v", err)
		}
		check("Storage Write API", name, away)
		_, err = appendResult(t, h, ms, [][]byte{row(2, "", "", []string{"1", "99.5"}, nil)})
		refused("Storage Write API, ARRAY element", err)
		_, err = appendResult(t, h, ms, [][]byte{row(3, "99.5", "1", nil, nil)})
		refused("Storage Write API, STRUCT field", err)
		check("after the refused appends", name, away)
	})

	t.Run("DDL", func(t *testing.T) {
		// CREATE TABLE with a STRUCT field's rounding_mode, and ALTER TABLE
		// ADD COLUMN with a column's and a STRUCT field's.
		name := ds.DatasetID + ".nrddl"
		for _, sql := range []string{
			"CREATE TABLE " + name + " (id INT64, s STRUCT<a NUMERIC(2), e NUMERIC(2) OPTIONS (rounding_mode = 'ROUND_HALF_EVEN')>, " +
				"r ARRAY<NUMERIC(2)>, x ARRAY<STRUCT<e NUMERIC(2) OPTIONS (rounding_mode = 'ROUND_HALF_EVEN')>>)",
			"ALTER TABLE " + name + " ADD COLUMN c NUMERIC(3, 1) OPTIONS (rounding_mode = 'ROUND_HALF_EVEN'), " +
				"ADD COLUMN d STRUCT<f BIGNUMERIC(2) OPTIONS (rounding_mode = 'ROUND_HALF_AWAY_FROM_ZERO')>",
			"INSERT INTO " + name + " (id, s, r, x, c, d) VALUES (" + values + ", NUMERIC '0.25', STRUCT(BIGNUMERIC '2.5' AS f))",
		} {
			if err := bqRun(ctx, c, sql, false); err != nil {
				t.Fatalf("%s: %v", sql, err)
			}
		}
		check("CREATE TABLE", name, away)
		if got := queryRows(t, ctx, c, project, ds.DatasetID, "SELECT CAST(c AS STRING), CAST(d.f AS STRING) FROM "+name); !reflect.DeepEqual(got, []string{"0.2|3"}) {
			t.Errorf("ADD COLUMN: rows %q, want [0.2|3]", got)
		}
		refused("ADD COLUMN c", bqRun(ctx, c, "INSERT INTO "+name+" (id, c) VALUES (2, NUMERIC '99.95')", false))
		refused("ADD COLUMN d.f", bqRun(ctx, c, "INSERT INTO "+name+" (id, d) VALUES (2, STRUCT(BIGNUMERIC '99.5' AS f))", false))
		md, err := ds.Table("nrddl").Metadata(ctx)
		if err != nil {
			t.Fatal(err)
		}
		s, x, col, d := md.Schema[1].Schema, md.Schema[3].Schema, md.Schema[4], md.Schema[5].Schema
		if s[0].Precision != 2 || s[1].RoundingMode != bigquery.RoundHalfEven || md.Schema[2].Precision != 2 ||
			x[0].RoundingMode != bigquery.RoundHalfEven || col.Precision != 3 || col.Scale != 1 || col.RoundingMode != bigquery.RoundHalfEven ||
			d[0].Precision != 2 || d[0].RoundingMode != bigquery.RoundHalfAwayFromZero {
			t.Errorf("schema read back: s %+v %+v, r %+v, x %+v, c %+v, d %+v", s[0], s[1], md.Schema[2], x[0], col, d[0])
		}
		for sql, want := range map[string]string{
			"ALTER TABLE " + name + " ADD COLUMN g NUMERIC(2) OPTIONS (rounding_mode = 'UP')":         "rounding_mode",
			"ALTER TABLE " + name + " ADD COLUMN g INT64 OPTIONS (rounding_mode = 'ROUND_HALF_EVEN')": "rounding_mode",
		} {
			if err := bqRun(ctx, c, sql, false); err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "400") {
				t.Errorf("%s: %v, want a 400 about %s", sql, err, want)
			}
		}
	})
}
