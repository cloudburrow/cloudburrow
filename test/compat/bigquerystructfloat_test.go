//go:build compat

package compat

import (
	"fmt"
	"math/big"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
)

// fieldTypeText is a result field's type as the Go client reads it, with a
// RECORD's fields: "FLOAT", "RECORD<x FLOAT, n INTEGER>", "[]RECORD<...>".
func fieldTypeText(f *bigquery.FieldSchema) string {
	s := string(f.Type)
	if f.Type == bigquery.RecordFieldType {
		var parts []string
		for _, sub := range f.Schema {
			parts = append(parts, sub.Name+" "+fieldTypeText(sub))
		}
		s += "<" + strings.Join(parts, ", ") + ">"
	}
	if f.Repeated {
		s = "[]" + s
	}
	return s
}

// numberText is a scalar the Go client read, as text: an INT64 or FLOAT64
// by %v, a NUMERIC or BIGNUMERIC as its exact decimal.
func numberText(v bigquery.Value) string {
	if r, ok := v.(*big.Rat); ok {
		return strings.TrimSuffix(strings.TrimRight(r.FloatString(9), "0"), ".")
	}
	return fmt.Sprintf("%v", v)
}

// covers: bigquery.jobs.query, bigquery.jobs.insert
//
// TestBigQueryStructFloatFieldsKeepTheirType (#1484), through the official
// Go client. Measured first: a whole FLOAT64 (3.0) in a STRUCT field or an
// ARRAY element came back from the engine as an INT64, so s.x / 2 was 1,
// s.x * 0.5 was 0 and s.x + 0.5 was 3, for a literal, a column, CAST, an
// expression, a nested STRUCT, ARRAY<STRUCT>, a subquery's STRUCT and a
// stored STRUCT column. Now each is FLOAT64, read back with a FLOAT result
// schema; a STRUCT returned to the client keeps its fields' types and
// values; and +, -, * and / of INT64, NUMERIC, BIGNUMERIC and FLOAT64,
// outside and inside STRUCTs, and IEEE_DIVIDE, have BigQuery's documented
// result types
// (https://cloud.google.com/bigquery/docs/reference/standard-sql/operators#arithmetic_operators).
func TestBigQueryStructFloatFieldsKeepTheirType(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	tbl := "`" + ds.DatasetID + ".t`"
	for _, q := range []string{
		"CREATE TABLE " + tbl + " (f FLOAT64, i INT64, n NUMERIC, b BIGNUMERIC, s STRUCT<x FLOAT64, k INT64>, a ARRAY<FLOAT64>)",
		"INSERT INTO " + tbl + " (f, i, n, b, s, a) VALUES (3.0, 3, 3, 3, STRUCT(3.0 AS x, 3 AS k), [3.0, 4.0])",
	} {
		if err := bqRun(ctx, c, q, false); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	// one runs a query of one row and returns its first column's value and
	// result type.
	one := func(sql string) (bigquery.Value, string, error) {
		it, err := c.Query(sql).Read(ctx)
		if err != nil {
			return nil, "", err
		}
		var row []bigquery.Value
		if err := it.Next(&row); err != nil {
			return nil, "", err
		}
		if len(row) == 0 || len(it.Schema) == 0 {
			return nil, "", fmt.Errorf("no columns")
		}
		return row[0], fieldTypeText(it.Schema[0]), nil
	}

	// Each form of a FLOAT64 STRUCT field (or ARRAY element), divided,
	// multiplied and added.
	forms := map[string]string{
		"literal":           "(SELECT STRUCT(3.0 AS x) AS s)",
		"CAST":              "(SELECT STRUCT(CAST(3 AS FLOAT64) AS x) AS s)",
		"column":            "(SELECT STRUCT(f AS x) AS s FROM " + tbl + ")",
		"expression":        "(SELECT STRUCT(f * 1 AS x) AS s FROM " + tbl + ")",
		"CAST of a column":  "(SELECT STRUCT(CAST(i AS FLOAT64) AS x) AS s FROM " + tbl + ")",
		"stored STRUCT":     "(SELECT s FROM " + tbl + ")",
		"nested STRUCT":     "(SELECT STRUCT(STRUCT(CAST(i AS FLOAT64) AS x).x AS x) AS s FROM " + tbl + ")",
		"ARRAY<STRUCT>":     "(SELECT e AS s FROM (SELECT [STRUCT(CAST(i AS FLOAT64) AS x)] AS a FROM " + tbl + "), UNNEST(a) e)",
		"OFFSET":            "(SELECT a[OFFSET(0)] AS s FROM (SELECT [STRUCT(f AS x)] AS a FROM " + tbl + "))",
		"subquery's STRUCT": "(SELECT (SELECT AS STRUCT CAST(i AS FLOAT64) AS x) AS s FROM " + tbl + ")",
	}
	for name, from := range forms {
		for _, op := range []struct{ expr, want string }{{"s.x / 2", "1.5"}, {"s.x * 0.5", "1.5"}, {"s.x + 0.5", "3.5"}, {"IEEE_DIVIDE(s.x, 2)", "1.5"}} {
			sql := "SELECT " + op.expr + " AS v FROM " + from
			v, typ, err := one(sql)
			if err != nil {
				t.Errorf("%s: %s: %v", name, sql, err)
				continue
			}
			if typ != "FLOAT" || numberText(v) != op.want {
				t.Errorf("%s: %s = %v (%T) of type %s, want FLOAT %s", name, sql, v, v, typ, op.want)
			}
		}
	}
	// Two levels of STRUCT, and an ARRAY<FLOAT64>'s elements.
	for sql, want := range map[string]string{
		"SELECT s.y.x / 2 FROM (SELECT STRUCT(STRUCT(CAST(i AS FLOAT64) AS x) AS y) AS s FROM " + tbl + ")": "1.5",
		"SELECT e * 0.5 FROM " + tbl + ", UNNEST(a) e ORDER BY e LIMIT 1":                                   "1.5",
		"SELECT a[OFFSET(1)] / 8 FROM " + tbl:                                                               "0.5",
	} {
		v, typ, err := one(sql)
		if err != nil {
			t.Errorf("%s: %v", sql, err)
		} else if typ != "FLOAT" || numberText(v) != want {
			t.Errorf("%s = %v (%T) of type %s, want FLOAT %s", sql, v, v, typ, want)
		}
	}

	// A STRUCT and an ARRAY<STRUCT> returned to the client: the schema and
	// the values, an INT64 field staying INT64.
	v, typ, err := one("SELECT STRUCT(CAST(i AS FLOAT64) AS x, i AS k, s.x / 2 AS h, [STRUCT(f AS y)] AS r) AS s FROM " + tbl)
	if err != nil {
		t.Fatalf("STRUCT result: %v", err)
	}
	if want := "RECORD<x FLOAT, k INTEGER, h FLOAT, r []RECORD<y FLOAT>>"; typ != want {
		t.Errorf("the STRUCT's result type is %s, want %s", typ, want)
	}
	if got := fmt.Sprintf("%#v", v); got != `[]bigquery.Value{3, 3, 1.5, []bigquery.Value{[]bigquery.Value{3}}}` {
		t.Errorf("the STRUCT reads %s", got)
	}
	if fs, ok := v.([]bigquery.Value); !ok || len(fs) != 4 {
		t.Errorf("the STRUCT reads %#v", v)
	} else {
		if _, ok := fs[0].(float64); !ok {
			t.Errorf("field x reads %T, want float64", fs[0])
		}
		if _, ok := fs[1].(int64); !ok {
			t.Errorf("field k reads %T, want int64", fs[1])
		}
	}

	// The documented result types of +, -, * and /: FLOAT64 if either
	// operand is, else BIGNUMERIC if either is, else NUMERIC if either is,
	// else INT64, and / of INT64s is FLOAT64. IEEE_DIVIDE is FLOAT64.
	types := []struct{ name, lit, col, schema string }{
		{"INT64", "3", "i", "INTEGER"},
		{"NUMERIC", "NUMERIC '3'", "n", "NUMERIC"},
		{"BIGNUMERIC", "BIGNUMERIC '3'", "b", "BIGNUMERIC"},
		{"FLOAT64", "CAST(3 AS FLOAT64)", "f", "FLOAT"},
	}
	rank := map[string]int{"INTEGER": 0, "NUMERIC": 1, "BIGNUMERIC": 2, "FLOAT": 3}
	ops := []struct{ op, want string }{{"+", "6"}, {"-", "0"}, {"*", "9"}, {"/", "1"}}
	for _, x := range types {
		for _, y := range types {
			for _, o := range ops {
				want := x.schema
				if rank[y.schema] > rank[want] {
					want = y.schema
				}
				if o.op == "/" && want == "INTEGER" {
					want = "FLOAT"
				}
				for _, sql := range []string{
					fmt.Sprintf("SELECT %s %s %s", x.lit, o.op, y.lit),
					fmt.Sprintf("SELECT %s %s %s FROM %s", x.col, o.op, y.col, tbl),
					fmt.Sprintf("SELECT s.p %s s.q FROM (SELECT STRUCT(%s AS p, %s AS q) AS s FROM %s)", o.op, x.col, y.col, tbl),
					fmt.Sprintf("SELECT STRUCT(%s %s %s AS v).v", x.lit, o.op, y.lit),
				} {
					v, typ, err := one(sql)
					if err != nil {
						t.Errorf("%s: %v", sql, err)
					} else if typ != want || numberText(v) != o.want {
						t.Errorf("%s %s %s: %s = %s of type %s, want %s %s", x.name, o.op, y.name, sql, numberText(v), typ, want, o.want)
					}
				}
			}
			sql := fmt.Sprintf("SELECT IEEE_DIVIDE(s.p, s.q) FROM (SELECT STRUCT(%s AS p, %s AS q) AS s FROM %s)", x.col, y.col, tbl)
			v, typ, err := one(sql)
			if err != nil {
				t.Errorf("%s: %v", sql, err)
			} else if typ != "FLOAT" || numberText(v) != "1" {
				t.Errorf("%s = %s of type %s, want FLOAT 1", sql, numberText(v), typ)
			}
		}
	}
}
