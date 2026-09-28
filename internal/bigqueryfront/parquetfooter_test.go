package bigqueryfront

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixture reads a Parquet file of testdata/parquet, written by Apache
// Arrow's Parquet writer (gen.py).
func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "parquet", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func fixtureColumns(t *testing.T, name string, opts pqOptions) []*pqCol {
	t.Helper()
	b := fixture(t, name)
	elems, err := readParquetSchema(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	cols, err := parquetColumns(elems, opts)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return cols
}

// describeCols writes columns as "name MODE TYPE", a RECORD's fields in
// parentheses, and " 501" after one the front does not load.
func describeCols(cols []*pqCol) string {
	var s []string
	for _, c := range cols {
		d := c.Name + " " + c.Mode + " " + c.Type
		if len(c.Fields) > 0 {
			d += " (" + describeSchema(c.Fields) + ")"
		}
		if c.notHere != "" {
			d += " 501"
		}
		s = append(s, d)
	}
	return strings.Join(s, ", ")
}

func describeSchema(fs []field) string {
	var s []string
	for _, f := range fs {
		d := f.Name + " " + f.Mode + " " + f.Type
		if len(f.Fields) > 0 {
			d += " (" + describeSchema(f.Fields) + ")"
		}
		s = append(s, d)
	}
	return strings.Join(s, ", ")
}

// TestParquetFooterSchema (#988, #1004, #1005): the schema read from the
// footer of files Arrow wrote is the one it wrote (gen.py, and pyarrow's
// own printout of each file's schema), as BigQuery's conversion table
// types it and its rules for groups, LIST and MAP nest it, with the
// load's options.
func TestParquetFooterSchema(t *testing.T) {
	infer := pqOptions{listInference: true}
	for _, c := range []struct {
		file string
		opts pqOptions
		want string
	}{
		{"ab.parquet", pqOptions{}, "a NULLABLE INTEGER, b NULLABLE STRING"},
		{"ab_required.parquet", pqOptions{}, "a REQUIRED INTEGER, b REQUIRED STRING"},
		{"upper.parquet", pqOptions{}, "A NULLABLE INTEGER, B NULLABLE STRING"},
		{"types.parquet", pqOptions{}, "bool NULLABLE BOOLEAN, int32 NULLABLE INTEGER, int8 NULLABLE INTEGER, uint16 NULLABLE INTEGER, " +
			"int64 NULLABLE INTEGER, float NULLABLE FLOAT, double NULLABLE FLOAT, string NULLABLE STRING, " +
			"bytes NULLABLE BYTES, fixed NULLABLE BYTES, date NULLABLE DATE, time_ms NULLABLE TIME, " +
			"time_us NULLABLE TIME, ts_ms NULLABLE TIMESTAMP, ts_us NULLABLE TIMESTAMP"},
		{"int96.parquet", pqOptions{}, "t NULLABLE TIMESTAMP"},
		{"ts_ns.parquet", pqOptions{}, "t NULLABLE  501"},
		{"decimal.parquet", pqOptions{}, "d NULLABLE NUMERIC"},
		{"decimal.parquet", pqOptions{decimalTypes: []string{"STRING"}}, "d NULLABLE STRING 501"},
		{"decimal_wide.parquet", pqOptions{}, "d NULLABLE NUMERIC"},
		{"decimal_wide.parquet", pqOptions{decimalTypes: []string{"NUMERIC", "BIGNUMERIC"}}, "d NULLABLE BIGNUMERIC"},
		{"decimal256.parquet", pqOptions{decimalTypes: []string{"BIGNUMERIC"}}, "d NULLABLE BIGNUMERIC"},
		{"decimal_int.parquet", pqOptions{}, "d32 NULLABLE NUMERIC, d64 NULLABLE NUMERIC"},
		{"uint64.parquet", pqOptions{}, "u NULLABLE INTEGER"},
		{"json.parquet", pqOptions{}, "j NULLABLE  501"},
		{"enum.parquet", pqOptions{}, "e NULLABLE BYTES"},
		{"enum.parquet", pqOptions{enumAsString: true}, "e NULLABLE STRING"},
		{"struct.parquet", pqOptions{}, "s NULLABLE RECORD (x NULLABLE INTEGER)"},
		{"list.parquet", pqOptions{}, "l NULLABLE RECORD (list REPEATED RECORD (element NULLABLE INTEGER))"},
		{"list.parquet", infer, "l REPEATED INTEGER"},
		{"list_list.parquet", infer, "l REPEATED INTEGER 501"},
		{"nested.parquet", pqOptions{}, "id NULLABLE INTEGER, " +
			"s NULLABLE RECORD (x NULLABLE INTEGER, y NULLABLE STRING, z NULLABLE RECORD (q NULLABLE BOOLEAN)), " +
			"l NULLABLE RECORD (list REPEATED RECORD (element NULLABLE INTEGER)), " +
			"ls NULLABLE RECORD (list REPEATED RECORD (element NULLABLE RECORD (x NULLABLE INTEGER, y NULLABLE STRING))), " +
			"m NULLABLE RECORD (key_value REPEATED RECORD (key REQUIRED STRING, value NULLABLE INTEGER)), " +
			"sl NULLABLE RECORD (l NULLABLE RECORD (list REPEATED RECORD (element NULLABLE INTEGER)))"},
		{"nested.parquet", pqOptions{listInference: true, mapArray: true}, "id NULLABLE INTEGER, " +
			"s NULLABLE RECORD (x NULLABLE INTEGER, y NULLABLE STRING, z NULLABLE RECORD (q NULLABLE BOOLEAN)), " +
			"l REPEATED INTEGER, ls REPEATED RECORD (x NULLABLE INTEGER, y NULLABLE STRING), " +
			"m REPEATED RECORD (key REQUIRED STRING, value NULLABLE INTEGER), sl NULLABLE RECORD (l REPEATED INTEGER)"},
	} {
		if got := describeCols(fixtureColumns(t, c.file, c.opts)); got != c.want {
			t.Errorf("%s %+v:\n got %s\nwant %s", c.file, c.opts, got, c.want)
		}
	}
	// The Parquet types, as the 501 names them.
	for file, want := range map[string]string{"ts_ns.parquet": "INT64 (TIMESTAMP(NANOS))", "json.parquet": "BYTE_ARRAY (JSON)"} {
		if cols := fixtureColumns(t, file, pqOptions{}); !strings.Contains(cols[0].notHere, want) {
			t.Errorf("%s: %q, want it to name %s", file, cols[0].notHere, want)
		}
	}
}

// TestParquetFooterRefusesOtherData: data that is not a Parquet file, or
// whose footer is cut or lies about its length, is an error, never a
// schema or a panic.
func TestParquetFooterRefusesOtherData(t *testing.T) {
	good := fixture(t, "ab.parquet")
	n := binary.LittleEndian.Uint32(good[len(good)-8:])
	lie := bytes.Clone(good)
	binary.LittleEndian.PutUint32(lie[len(lie)-8:], uint32(len(good)))
	cut := bytes.Clone(good)
	// The metadata keeps its length but loses its first half.
	start := len(good) - 8 - int(n)
	copy(cut[start:], bytes.Repeat([]byte{0xff}, int(n)/2))
	for name, b := range map[string][]byte{
		"empty":       nil,
		"csv":         []byte("a,b\n1,x\n2,y\n3,z\n"),
		"magic only":  []byte("PAR1PAR1"),
		"no end":      good[:len(good)-1],
		"length lies": lie,
		"garbled":     cut,
	} {
		if _, err := readParquetSchema(bytes.NewReader(b), int64(len(b))); err == nil {
			t.Errorf("%s: no error", name)
		} else if name != "garbled" && !errors.Is(err, errNotParquet) {
			t.Errorf("%s: %v, want errNotParquet", name, err)
		}
	}
	// Every truncation of the metadata is an error.
	meta := good[start : len(good)-8]
	for i := range len(meta) {
		if _, err := parseFileMetaData(meta[:i]); err == nil {
			t.Fatalf("metadata cut at %d of %d: no error", i, len(meta))
		}
	}
}

// FuzzParquetFooter: no metadata makes the reader panic or loop.
func FuzzParquetFooter(f *testing.F) {
	for _, name := range []string{"ab.parquet", "types.parquet", "list.parquet", "nested.parquet"} {
		b, err := os.ReadFile(filepath.Join("testdata", "parquet", name))
		if err != nil {
			f.Fatal(err)
		}
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		elems, err := readParquetSchema(bytes.NewReader(b), int64(len(b)))
		if err == nil {
			_, _ = parquetColumns(elems, pqOptions{listInference: true, mapArray: true})
		}
	})
}
