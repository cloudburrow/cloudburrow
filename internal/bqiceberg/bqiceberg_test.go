package bqiceberg

import (
	"context"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWriteReadBack writes two snapshots and reads them back with
// iceberg-go, as a reader of the files would.
func TestWriteReadBack(t *testing.T) {
	ctx := context.Background()
	s := &Store{Root: t.TempDir()}
	loc := "gs://bucket/tables/t1"
	cols := []Column{{Name: "id", Type: "INT64", Required: true}, {Name: "name", Type: "STRING"},
		{Name: "amount", Type: "NUMERIC"}, {Name: "day", Type: "DATE"}, {Name: "at", Type: "TIMESTAMP"},
		{Name: "ok", Type: "BOOL"}, {Name: "f", Type: "FLOAT64"}, {Name: "b", Type: "BYTES"},
		{Name: "dt", Type: "DATETIME"}, {Name: "tm", Type: "TIME"}}
	rows := [][]any{
		{int64(1), "a", big.NewInt(1500000000), int32(19000), int64(1_600_000_000_000_000), true, 1.5, []byte("x"), int64(5), int64(7)},
		{int64(2), nil, nil, nil, nil, nil, nil, nil, nil, nil},
	}
	data, err := s.Write(ctx, loc, cols, rows)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 || !strings.HasPrefix(data[0], loc+"/data/") || !strings.HasSuffix(data[0], ".parquet") {
		t.Fatalf("data files %v", data)
	}
	if _, err := s.Write(ctx, loc, cols, rows[:1]); err != nil {
		t.Fatal(err)
	}
	files, ok, err := s.Metadata(ctx, loc)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	if files[0] != loc+"/metadata/v3.metadata.json" || len(files) < 3 {
		t.Fatalf("metadata %v", files)
	}
	for _, f := range files {
		if _, err := os.Stat(s.LocalPath(f)); err != nil {
			t.Fatal(err)
		}
	}
	at, err := ReadAll(ctx, s.Root, files[0])
	if err != nil {
		t.Fatal(err)
	}
	defer at.Release()
	if at.NumRows() != 1 || at.NumCols() != int64(len(cols)) {
		t.Fatalf("read %d rows, %d columns", at.NumRows(), at.NumCols())
	}
	// A column added.
	cols2 := append(cols, Column{Name: "extra", Type: "STRING"})
	if _, err := s.Write(ctx, loc, cols2, [][]any{append(rows[0], "e")}); err != nil {
		t.Fatal(err)
	}
	files, _, _ = s.Metadata(ctx, loc)
	at2, err := ReadAll(ctx, s.Root, files[0])
	if err != nil {
		t.Fatal(err)
	}
	defer at2.Release()
	if at2.NumCols() != int64(len(cols2)) || at2.NumRows() != 1 {
		t.Fatalf("after ALTER: %d rows %d cols", at2.NumRows(), at2.NumCols())
	}
	_ = filepath.Join
}

func TestUnsupportedType(t *testing.T) {
	s := &Store{Root: t.TempDir()}
	if _, err := s.Write(context.Background(), "gs://b/t", []Column{{Name: "j", Type: "JSON"}}, nil); err == nil {
		t.Fatal("JSON column written")
	}
}

// TestWriteNestedTypes (#2104): a STRUCT column is an Iceberg struct, an
// ARRAY an Iceberg list of required elements, an ARRAY of STRUCT a list of
// structs; a NULL array is written as the empty one, and a second write of
// the same schema evolves nothing.
func TestWriteNestedTypes(t *testing.T) {
	ctx := context.Background()
	s := &Store{Root: t.TempDir()}
	loc := "gs://bucket/tables/nested"
	addr := Column{Name: "addr", Type: "RECORD", Fields: []Column{{Name: "city", Type: "STRING"}, {Name: "zip", Type: "INT64", Required: true}}}
	cols := []Column{{Name: "id", Type: "INT64", Required: true}, addr,
		{Name: "tags", Type: "STRING", Repeated: true},
		{Name: "items", Type: "RECORD", Repeated: true, Fields: []Column{{Name: "sku", Type: "STRING"}, {Name: "n", Type: "INT64"}}}}
	rows := [][]any{
		{int64(1), []any{"Paris", int64(75001)}, []any{"a", "b"}, []any{[]any{"x", int64(2)}, []any{"y", nil}}},
		{int64(2), nil, nil, []any{}},
	}
	sc, err := Schema(cols)
	if err != nil {
		t.Fatal(err)
	}
	if got := sc.String(); !strings.Contains(got, "struct<") || !strings.Contains(got, "list<") {
		t.Fatalf("schema %s", got)
	}
	if _, err := s.Write(ctx, loc, cols, rows); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write(ctx, loc, cols, rows[:1]); err != nil {
		t.Fatal(err)
	}
	files, ok, err := s.Metadata(ctx, loc)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	if files[0] != loc+"/metadata/v3.metadata.json" {
		t.Fatalf("a second write of the same schema evolved it: %v", files)
	}
	at, err := ReadAll(ctx, s.Root, files[0])
	if err != nil {
		t.Fatal(err)
	}
	defer at.Release()
	if at.NumRows() != 1 || at.NumCols() != 4 {
		t.Fatalf("read %d rows, %d columns", at.NumRows(), at.NumCols())
	}
	if got := at.Schema().Field(3).Type.String(); !strings.Contains(got, "list<") || !strings.Contains(got, "struct<") {
		t.Fatalf("items read as %s", got)
	}
	// A column added after the nested ones.
	cols2 := append(cols, Column{Name: "extra", Type: "STRING"})
	if _, err := s.Write(ctx, loc, cols2, [][]any{append(rows[0], "e")}); err != nil {
		t.Fatal(err)
	}
}
