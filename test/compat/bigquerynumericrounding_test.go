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

// TestBigQueryNumericRounding (#1401), through the official Go client: a
// value written to a parameterized NUMERIC(P, S) column is rounded to its
// scale, by the column's roundingMode, else the table's
// defaultRoundingMode, else ROUND_HALF_AWAY_FROM_ZERO, and a value with
// more than P digits is refused OUT_OF_RANGE
// (https://cloud.google.com/bigquery/docs/reference/standard-sql/data-types#parameterized_decimal_type,
// https://cloud.google.com/bigquery/docs/schemas#rounding_mode). 2.5 and
// 3.5 written to a NUMERIC(2, 0) column read back 3 and 4 under
// ROUND_HALF_AWAY_FROM_ZERO and 2 and 4 under ROUND_HALF_EVEN, by each
// write path: DML INSERT, UPDATE and MERGE, tabledata.insertAll, a load,
// the Storage Write API and a query's destination table; 99.5 (100 once
// rounded) is refused by each, insertAll's as an invalid row. A table made
// by CREATE TABLE keeps its columns' NUMERIC(P) and rounding_mode and its
// default_rounding_mode, and rounds by them; tables.get reads them back.
//
// covers: bigquery.tables.insert, bigquery.tables.patch, bigquery.tables.get, bigquery.jobs.query, bigquery.jobs.insert, bigquery.tabledata.insertAll, bigquery.tabledata.list
func TestBigQueryNumericRounding(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	// a takes the table's mode, e is ROUND_HALF_EVEN of its own.
	schema := bigquery.Schema{
		{Name: "id", Type: bigquery.IntegerFieldType},
		{Name: "a", Type: bigquery.NumericFieldType, Precision: 2},
		{Name: "e", Type: bigquery.NumericFieldType, Precision: 2, RoundingMode: bigquery.RoundHalfEven},
	}
	n := 0
	table := func(mode bigquery.RoundingMode) (*bigquery.Table, string) {
		t.Helper()
		n++
		tbl := ds.Table("r" + strings.Repeat("x", n))
		if err := tbl.Create(ctx, &bigquery.TableMetadata{Schema: schema}); err != nil {
			t.Fatal(err)
		}
		name := ds.DatasetID + "." + tbl.TableID
		if mode != "" {
			// The Go client's TableMetadata has no defaultRoundingMode.
			if err := bqRun(ctx, c, "ALTER TABLE "+name+" SET OPTIONS (default_rounding_mode = '"+string(mode)+"')", false); err != nil {
				t.Fatal(err)
			}
		}
		return tbl, name
	}
	rows := func(name string) []string {
		t.Helper()
		return queryRows(t, ctx, c, project, ds.DatasetID, "SELECT id, CAST(a AS STRING), CAST(e AS STRING) FROM "+name+" ORDER BY id")
	}
	check := func(what, name string, want ...string) {
		t.Helper()
		if got := rows(name); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: rows %q, want %q", what, got, want)
		}
	}
	away := []string{"1|3|2", "2|4|4"}
	even := []string{"1|2|2", "2|4|4"}
	refused := func(what string, err error) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), "OUT_OF_RANGE") {
			t.Errorf("%s: 99.5 into NUMERIC(2, 0): %v, want OUT_OF_RANGE", what, err)
		}
	}

	t.Run("DML", func(t *testing.T) {
		for _, insert := range []bool{false, true} {
			_, name := table("")
			run := func(sql string) error { return bqRun(ctx, c, sql, insert) }
			if err := run("INSERT INTO " + name + " (id, a, e) VALUES (1, 2.5, 2.5), (2, 3.5, 3.5)"); err != nil {
				t.Fatal(err)
			}
			check("DML INSERT", name, away...)
			if err := run("UPDATE " + name + " SET a = a - 0.5, e = e - 1.5 WHERE true"); err != nil {
				t.Fatal(err)
			}
			check("DML UPDATE", name, "1|3|0", "2|4|2")
			if err := run("MERGE " + name + " T USING (SELECT 1 AS id, NUMERIC '6.5' AS v UNION ALL SELECT 3, NUMERIC '7.5') S ON T.id = S.id " +
				"WHEN MATCHED THEN UPDATE SET a = S.v, e = S.v WHEN NOT MATCHED THEN INSERT (id, a, e) VALUES (S.id, S.v, S.v)"); err != nil {
				t.Fatal(err)
			}
			check("DML MERGE", name, "1|7|6", "2|4|2", "3|8|8")
			refused("DML INSERT", run("INSERT INTO "+name+" (id, a) VALUES (4, 99.5)"))
			refused("DML UPDATE", run("UPDATE "+name+" SET e = 99.5 WHERE id = 1"))
			check("after the refused DML", name, "1|7|6", "2|4|2", "3|8|8")
		}
	})

	t.Run("defaultRoundingMode", func(t *testing.T) {
		// The table's mode rounds a, and e keeps its own, by DML and a load.
		_, name := table(bigquery.RoundHalfEven)
		if err := bqRun(ctx, c, "INSERT INTO "+name+" (id, a, e) VALUES (1, 2.5, 2.5)", false); err != nil {
			t.Fatal(err)
		}
		tbl, name2 := table(bigquery.RoundHalfEven)
		src := bigquery.NewReaderSource(strings.NewReader(`{"id": 2, "a": "3.5", "e": "3.5"}` + "\n"))
		src.SourceFormat = bigquery.JSON
		job, err := tbl.LoaderFrom(src).Run(ctx)
		if err == nil {
			var st *bigquery.JobStatus
			if st, err = job.Wait(ctx); err == nil {
				err = st.Err()
			}
		}
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		check("DML INSERT, table ROUND_HALF_EVEN", name, "1|2|2")
		check("load, table ROUND_HALF_EVEN", name2, "2|4|4")
		md, err := tbl.Metadata(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if md.Schema[2].RoundingMode != bigquery.RoundHalfEven || md.Schema[1].Precision != 2 {
			t.Errorf("schema read back: a %+v, e %+v", md.Schema[1], md.Schema[2])
		}
	})

	t.Run("DDL", func(t *testing.T) {
		// The table's defaultRoundingMode, set by DDL and changed by
		// ALTER TABLE SET OPTIONS.
		name := ds.DatasetID + ".ddl"
		for _, sql := range []string{
			"CREATE TABLE " + name + " (id INT64, a NUMERIC(2), e NUMERIC(2) OPTIONS (rounding_mode = 'ROUND_HALF_AWAY_FROM_ZERO')) OPTIONS (default_rounding_mode = 'ROUND_HALF_EVEN')",
			"INSERT INTO " + name + " (id, a, e) VALUES (1, 2.5, 2.5)",
			"ALTER TABLE " + name + " SET OPTIONS (default_rounding_mode = 'ROUND_HALF_AWAY_FROM_ZERO')",
			"INSERT INTO " + name + " (id, a, e) VALUES (2, 2.5, 2.5)",
		} {
			if err := bqRun(ctx, c, sql, false); err != nil {
				t.Fatalf("%s: %v", sql, err)
			}
		}
		check("DDL", name, "1|2|3", "2|3|3")
		refused("DDL table", bqRun(ctx, c, "INSERT INTO "+name+" (id, a) VALUES (3, 99.5)", false))
		md, err := ds.Table("ddl").Metadata(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if a, e := md.Schema[1], md.Schema[2]; a.Precision != 2 || e.Precision != 2 || e.RoundingMode != bigquery.RoundHalfAwayFromZero {
			t.Errorf("DDL table's schema: a %+v, e %+v", a, e)
		}
	})

	t.Run("insertAll", func(t *testing.T) {
		for _, mode := range []bigquery.RoundingMode{"", bigquery.RoundHalfEven} {
			tbl, name := table(mode)
			if err := tbl.Inserter().Put(ctx, []*bigquery.ValuesSaver{
				{Schema: schema, Row: []bigquery.Value{1, big.NewRat(5, 2), big.NewRat(5, 2)}},
				{Schema: schema, Row: []bigquery.Value{2, big.NewRat(7, 2), big.NewRat(7, 2)}},
			}); err != nil {
				t.Fatalf("insertAll: %v", err)
			}
			want := away
			if mode != "" {
				want = even
			}
			check("insertAll, table mode "+string(mode), name, want...)
			refused("insertAll", tbl.Inserter().Put(ctx, &bigquery.ValuesSaver{Schema: schema, Row: []bigquery.Value{3, big.NewRat(199, 2), nil}}))
		}
	})

	t.Run("load", func(t *testing.T) {
		tbl, name := table("")
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
		if err := load(`{"id": 1, "a": "2.5", "e": "2.5"}` + "\n" + `{"id": 2, "a": 3.5, "e": 3.5}` + "\n"); err != nil {
			t.Fatalf("load: %v", err)
		}
		check("load", name, away...)
		refused("load", load(`{"id": 3, "a": "99.5"}`+"\n"))
		check("after the refused load", name, away...)
	})

	t.Run("query destination", func(t *testing.T) {
		tbl, name := table("")
		q := c.Query("SELECT 1 AS id, NUMERIC '2.5' AS a, NUMERIC '2.5' AS e UNION ALL SELECT 2, 3.5, 3.5")
		q.Dst = tbl
		q.WriteDisposition = bigquery.WriteAppend
		if err := runQueryJob(ctx, q); err != nil {
			t.Fatalf("query to a destination: %v", err)
		}
		check("query destination", name, away...)
		q = c.Query("SELECT 3 AS id, NUMERIC '99.5' AS a, NUMERIC '1' AS e")
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
		row := func(id int64, a, e string) []byte {
			msg := dynamicpb.NewMessage(m)
			msg.Set(m.Fields().ByName("id"), protoreflect.ValueOfInt64(id))
			for col, v := range map[string]string{"a": a, "e": e} {
				fd := m.Fields().ByName(protoreflect.Name(col))
				if fd.Kind() == protoreflect.StringKind {
					msg.Set(fd, protoreflect.ValueOfString(v))
				} else {
					msg.Set(fd, protoreflect.ValueOfBytes(numericBytes(t, v)))
				}
			}
			b, _ := proto.Marshal(msg)
			return b
		}
		if _, err := appendResult(t, h, ms, [][]byte{row(1, "2.5", "2.5"), row(2, "3.5", "3.5")}); err != nil {
			t.Fatalf("AppendRows: %v", err)
		}
		check("Storage Write API", name, away...)
		_, err = appendResult(t, h, ms, [][]byte{row(3, "99.5", "1")})
		refused("Storage Write API", err)
	})
}

// numericBytes is a NUMERIC as the Storage Write API carries it in BYTES:
// the value times 10^9, little-endian two's complement.
func numericBytes(t *testing.T, s string) []byte {
	t.Helper()
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		t.Fatalf("numeric %q", s)
	}
	r.Mul(r, new(big.Rat).SetInt64(1e9))
	v := new(big.Int).Quo(r.Num(), r.Denom())
	if v.Sign() < 0 {
		t.Fatalf("numericBytes handles only non-negative values, not %s", s)
	}
	be := v.Bytes()
	out := make([]byte, len(be)+1)
	for i, b := range be {
		out[len(be)-1-i] = b
	}
	return out
}
