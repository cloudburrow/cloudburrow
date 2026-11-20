package main

// The Data tab's values beyond GoogleSQL's scalars (#1859, round 2): ARRAY
// columns and PostgreSQL-dialect databases. Google's table Data page edits
// each value as text; an array is written as a JSON array ([1, 2, null],
// ["a", "b"]). The value is bound as a typed parameter (a
// spanner.GenericColumnValue of the column's own type), so it is the
// database, not this file, that decides what it holds. A PostgreSQL
// database's catalogue is information_schema in lowercase with table_schema
// 'public', its identifiers are quoted "so", and its parameters are $1, $2.
//
// Spanner has no STRUCT columns ("STRUCT is not supported as a column
// type"; a STRUCT exists in queries only), so the Data tab has none to edit;
// PROTO and ENUM columns are read and written through the database's
// proto bundle (consolespannerproto.go, #2017).

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"cloud.google.com/go/spanner"
	sppb "cloud.google.com/go/spanner/apiv1/spannerpb"
	"google.golang.org/api/iterator"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// spannerDialect is how a database's SQL is written.
type spannerDialect struct{ pg bool }

// spannerDialectOfClient asks the database its dialect.
func spannerDialectOfClient(ctx context.Context, c *spanner.Client) spannerDialect {
	it := c.Single().Query(ctx, spanner.Statement{SQL: "SELECT option_value FROM information_schema.database_options WHERE option_name = 'database_dialect'"})
	defer it.Stop()
	row, err := it.Next()
	if err != nil {
		return spannerDialect{}
	}
	var v spanner.NullString
	if row.Column(0, &v) != nil {
		return spannerDialect{}
	}
	return spannerDialect{pg: v.StringVal == "POSTGRESQL"}
}

// quote is an identifier in the dialect.
func (d spannerDialect) quote(name string) string {
	if d.pg {
		return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
	}
	return "`" + strings.ReplaceAll(name, "`", "\\`") + "`"
}

// binder names a statement's parameters in the dialect: @name in GoogleSQL,
// $n (bound as pn) in PostgreSQL.
type spannerBinder struct {
	d      spannerDialect
	params map[string]any
}

func (b *spannerBinder) bind(name string, v any) string {
	if b.d.pg {
		n := len(b.params) + 1
		b.params[fmt.Sprintf("p%d", n)] = v
		return fmt.Sprintf("$%d", n)
	}
	b.params[name] = v
	return "@" + name
}

// spannerColumnType is the Spanner type of a column's SPANNER_TYPE (either
// dialect's spelling), nil when the Data tab does not write it.
func spannerColumnType(t string) *sppb.Type {
	s := strings.TrimSpace(t)
	up := strings.ToUpper(s)
	if strings.HasPrefix(up, "ARRAY<") && strings.HasSuffix(up, ">") {
		el := spannerColumnType(s[len("ARRAY<") : len(s)-1])
		if el == nil || el.GetCode() == sppb.TypeCode_ARRAY {
			return nil
		}
		return &sppb.Type{Code: sppb.TypeCode_ARRAY, ArrayElementType: el}
	}
	if strings.HasSuffix(s, "[]") {
		el := spannerColumnType(strings.TrimSuffix(s, "[]"))
		if el == nil || el.GetCode() == sppb.TypeCode_ARRAY {
			return nil
		}
		return &sppb.Type{Code: sppb.TypeCode_ARRAY, ArrayElementType: el}
	}
	if i := strings.IndexByte(up, '('); i >= 0 {
		up = strings.TrimSpace(up[:i])
	}
	code := map[string]sppb.TypeCode{
		"INT64": sppb.TypeCode_INT64, "FLOAT64": sppb.TypeCode_FLOAT64, "FLOAT32": sppb.TypeCode_FLOAT32,
		"BOOL": sppb.TypeCode_BOOL, "STRING": sppb.TypeCode_STRING, "BYTES": sppb.TypeCode_BYTES,
		"DATE": sppb.TypeCode_DATE, "TIMESTAMP": sppb.TypeCode_TIMESTAMP, "NUMERIC": sppb.TypeCode_NUMERIC,
		"JSON": sppb.TypeCode_JSON,
		// PostgreSQL's spellings.
		"BIGINT": sppb.TypeCode_INT64, "DOUBLE PRECISION": sppb.TypeCode_FLOAT64, "REAL": sppb.TypeCode_FLOAT32,
		"BOOLEAN": sppb.TypeCode_BOOL, "CHARACTER VARYING": sppb.TypeCode_STRING, "TEXT": sppb.TypeCode_STRING,
		"BYTEA": sppb.TypeCode_BYTES, "TIMESTAMP WITH TIME ZONE": sppb.TypeCode_TIMESTAMP,
	}
	switch {
	case up == "NUMERIC" && s != up:
		// PostgreSQL's numeric (its catalogue spells types in lowercase).
		return &sppb.Type{Code: sppb.TypeCode_NUMERIC, TypeAnnotation: sppb.TypeAnnotationCode_PG_NUMERIC}
	case up == "JSONB":
		return &sppb.Type{Code: sppb.TypeCode_JSON, TypeAnnotation: sppb.TypeAnnotationCode_PG_JSONB}
	}
	if c, ok := code[up]; ok {
		return &sppb.Type{Code: c}
	}
	return nil
}

// spannerWireValue is raw, a value as the Data tab's text field holds it,
// as Spanner's wire encoding of type t: an INT64 and the string-encoded
// types as their text, a float as a number, a BOOL as true or false, BYTES
// as base64 (as the Data tab shows them), an ARRAY as a JSON array of its
// elements, null for NULL; a PROTO as its message's JSON and an ENUM by
// value name, through the bundle ps.
func spannerWireValue(t *sppb.Type, raw string) (*structpb.Value, error) {
	return spannerProtos{}.wireValue(t, raw)
}

func (ps spannerProtos) wireValue(t *sppb.Type, raw string) (*structpb.Value, error) {
	if t.GetCode() == sppb.TypeCode_ARRAY {
		var elems []any
		dec := json.NewDecoder(strings.NewReader(raw))
		dec.UseNumber()
		if err := dec.Decode(&elems); err != nil {
			return nil, fmt.Errorf("an ARRAY is written as a JSON array, such as [1, 2] or [\"a\", \"b\"]: %v", err)
		}
		list := &structpb.ListValue{}
		for i, e := range elems {
			var s string
			switch x := e.(type) {
			case nil:
				list.Values = append(list.Values, structpb.NewNullValue())
				continue
			case string:
				s = x
			case json.Number:
				s = x.String()
			case bool:
				s = strconv.FormatBool(x)
			default:
				b, _ := json.Marshal(x)
				s = string(b)
			}
			v, err := ps.wireValue(t.GetArrayElementType(), s)
			if err != nil {
				return nil, fmt.Errorf("element %d: %w", i, err)
			}
			list.Values = append(list.Values, v)
		}
		return structpb.NewListValue(list), nil
	}
	switch t.GetCode() {
	case sppb.TypeCode_PROTO, sppb.TypeCode_ENUM:
		return ps.protoWire(t, raw)
	case sppb.TypeCode_FLOAT64, sppb.TypeCode_FLOAT32:
		f, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
		switch {
		case err != nil:
			return nil, fmt.Errorf("%q is not a number", raw)
		case math.IsNaN(f):
			return structpb.NewStringValue("NaN"), nil
		case math.IsInf(f, 1):
			return structpb.NewStringValue("Infinity"), nil
		case math.IsInf(f, -1):
			return structpb.NewStringValue("-Infinity"), nil
		}
		return structpb.NewNumberValue(f), nil
	case sppb.TypeCode_BOOL:
		b, err := strconv.ParseBool(strings.TrimSpace(raw))
		if err != nil {
			return nil, fmt.Errorf("%q is not true or false", raw)
		}
		return structpb.NewBoolValue(b), nil
	case sppb.TypeCode_INT64:
		if _, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64); err != nil {
			return nil, fmt.Errorf("%q is not a whole number", raw)
		}
		return structpb.NewStringValue(strings.TrimSpace(raw)), nil
	case sppb.TypeCode_BYTES:
		if _, err := base64.StdEncoding.DecodeString(raw); err != nil {
			return nil, fmt.Errorf("BYTES are written as base64, as the Data tab shows them: %v", err)
		}
		return structpb.NewStringValue(raw), nil
	case sppb.TypeCode_JSON:
		if !json.Valid([]byte(raw)) {
			return nil, fmt.Errorf("%q is not JSON", raw)
		}
		return structpb.NewStringValue(raw), nil
	}
	return structpb.NewStringValue(raw), nil
}

// spannerTypedParam is raw as a parameter of the column's type.
func spannerTypedParam(colType, raw string) (spanner.GenericColumnValue, error) {
	return spannerProtos{}.typedParam(colType, raw)
}

func (ps spannerProtos) typedParam(colType, raw string) (spanner.GenericColumnValue, error) {
	t := ps.columnType(colType)
	if t == nil {
		return spanner.GenericColumnValue{}, fmt.Errorf("the Data tab does not write %s columns", colType)
	}
	v, err := ps.wireValue(t, raw)
	if err != nil {
		return spanner.GenericColumnValue{}, err
	}
	return spanner.GenericColumnValue{Type: t, Value: v}, nil
}

// spannerTableColumnsIn is spannerTableColumns in the database's dialect.
func spannerTableColumnsIn(ctx context.Context, c *spanner.Client, d spannerDialect, table string) ([]spannerColumn, error) {
	if !d.pg {
		return spannerTableColumns(ctx, c, table)
	}
	it := c.Single().Query(ctx, spanner.Statement{
		SQL: `SELECT c.column_name, c.spanner_type, c.is_nullable, COALESCE(k.ordinal_position, 0), COALESCE(c.is_generated, 'NEVER')
		      FROM information_schema.columns c
		      LEFT JOIN information_schema.index_columns k
		        ON k.table_schema = c.table_schema AND k.table_name = c.table_name AND k.column_name = c.column_name
		           AND k.index_name = 'PRIMARY_KEY'
		      WHERE c.table_schema = 'public' AND c.table_name = $1 ORDER BY c.ordinal_position`,
		Params: map[string]any{"p1": table}})
	defer it.Stop()
	var out []spannerColumn
	for {
		row, err := it.Next()
		if errors.Is(err, iterator.Done) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		var col spannerColumn
		var nullable, generated string
		if err := row.Columns(&col.Name, &col.Type, &nullable, &col.Key, &generated); err != nil {
			return nil, err
		}
		col.Nullable, col.Generated = nullable == "YES", generated != "NEVER"
		out = append(out, col)
	}
}

// spannerTypedColumn says whether a column is written as a typed parameter
// rather than through spannerCastExpr (spannerProtos.typed).
func spannerTypedColumn(d spannerDialect, t string) bool {
	return spannerProtos{}.typed(d, t)
}

// spannerPGTableDetail is a PostgreSQL database's table page: the Schema,
// Indexes and Data tabs from its lowercase information_schema.
func (p spannerProvider) spannerPGTableDetail(ctx context.Context, c *spanner.Client, d spannerDialect, instanceID, dbID, table string) console.Detail {
	columns := console.Listing{Columns: []string{"Type", "Nullable", "Key", "Generated"}, NameColumn: "Column", Noun: "columns"}
	cols, err := spannerTableColumnsIn(ctx, c, d, table)
	if err != nil {
		columns.Unavailable = "reading columns: " + err.Error()
	}
	for _, col := range cols {
		key := "—"
		if col.Key > 0 {
			key = fmt.Sprintf("PK %d", col.Key)
		}
		columns.Items = append(columns.Items, console.Resource{Name: col.Name, Fields: map[string]string{
			"Type": col.Type, "Nullable": yesNo(col.Nullable), "Key": key, "Generated": yesNo(col.Generated)}})
	}
	columns.Total = len(columns.Items)

	indexes := console.Listing{Columns: []string{"Type", "Unique", "State", "Columns"}, NameColumn: "Index", Noun: "indexes"}
	rows, err := spannerRowsParams(ctx, c, spanner.Statement{
		SQL: `SELECT index_name, index_type, is_unique, COALESCE(index_state, '') FROM information_schema.indexes
		      WHERE table_schema = 'public' AND table_name = $1 ORDER BY index_name`,
		Params: map[string]any{"p1": table}})
	if err != nil {
		indexes.Unavailable = "reading indexes: " + err.Error()
	}
	idxCols, err := spannerRowsParams(ctx, c, spanner.Statement{
		SQL: `SELECT index_name, column_name FROM information_schema.index_columns
		      WHERE table_schema = 'public' AND table_name = $1 ORDER BY index_name, ordinal_position`,
		Params: map[string]any{"p1": table}})
	if err != nil && indexes.Unavailable == "" {
		indexes.Unavailable = "reading index columns: " + err.Error()
	}
	byIndex := map[string][]string{}
	for _, r := range idxCols {
		byIndex[r[0]] = append(byIndex[r[0]], r[1])
	}
	for _, r := range rows {
		indexes.Items = append(indexes.Items, console.Resource{Name: r[0], Fields: map[string]string{
			"Type": r[1], "Unique": spannerYes(r[2]), "State": orDash(r[3]), "Columns": strings.Join(byIndex[r[0]], ", ")}})
	}
	indexes.Total = len(indexes.Items)
	return console.Detail{
		Summary: []console.Property{
			{Label: "Database", Value: dbID},
			{Label: "Instance", Value: instanceID},
			{Label: "Dialect", Value: "PostgreSQL"},
			{Label: "Columns", Value: fmt.Sprint(columns.Total)},
			{Label: "Indexes", Value: fmt.Sprint(indexes.Total)},
		},
		Sections: []console.Section{
			{ID: "columns", Label: "Schema", Listing: columns},
			{ID: "indexes", Label: "Indexes", Listing: indexes},
			p.spannerTableData(ctx, c, d, instanceID, dbID, table),
		},
	}
}

// spannerRowsParams is spannerRows for a statement with parameters.
func spannerRowsParams(ctx context.Context, c *spanner.Client, st spanner.Statement) ([][]string, error) {
	it := c.Single().Query(ctx, st)
	defer it.Stop()
	var out [][]string
	for {
		row, err := it.Next()
		if errors.Is(err, iterator.Done) {
			return out, nil
		}
		if err != nil {
			return out, err
		}
		cells := make([]string, row.Size())
		for i := range cells {
			var v spanner.GenericColumnValue
			if err := row.Column(i, &v); err != nil {
				return out, err
			}
			cells[i] = renderSpannerValue(v)
			if cells[i] == "—" {
				cells[i] = ""
			}
		}
		out = append(out, cells)
	}
}
