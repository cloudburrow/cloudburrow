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

func fixtureColumns(t *testing.T, name string) []pqColumn {
	t.Helper()
	b := fixture(t, name)
	elems, err := readParquetSchema(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	cols, err := parquetColumns(elems)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return cols
}

// TestParquetFooterSchema (#988): the schema read from the footer of files
// Arrow wrote is the one it wrote (gen.py, and pyarrow's own printout of
// each file's schema), as BigQuery's conversion table types it.
func TestParquetFooterSchema(t *testing.T) {
	describe := func(cols []pqColumn) string {
		var s []string
		for _, c := range cols {
			d := c.Name + " " + c.Mode + " " + c.Type
			if c.notHere != "" {
				d += " 501"
			}
			s = append(s, d)
		}
		return strings.Join(s, ", ")
	}
	for _, c := range []struct{ file, want string }{
		{"ab.parquet", "a NULLABLE INTEGER, b NULLABLE STRING"},
		{"ab_required.parquet", "a REQUIRED INTEGER, b REQUIRED STRING"},
		{"upper.parquet", "A NULLABLE INTEGER, B NULLABLE STRING"},
		{"types.parquet", "bool NULLABLE BOOLEAN, int32 NULLABLE INTEGER, int8 NULLABLE INTEGER, uint16 NULLABLE INTEGER, " +
			"int64 NULLABLE INTEGER, float NULLABLE FLOAT, double NULLABLE FLOAT, string NULLABLE STRING, " +
			"bytes NULLABLE BYTES 501, fixed NULLABLE BYTES 501, date NULLABLE DATE, time_ms NULLABLE TIME 501, " +
			"time_us NULLABLE TIME 501, ts_ms NULLABLE TIMESTAMP 501, ts_us NULLABLE TIMESTAMP"},
		{"int96.parquet", "t NULLABLE TIMESTAMP 501"},
		{"ts_ns.parquet", "t NULLABLE  501"},
		{"decimal.parquet", "d NULLABLE NUMERIC 501"},
		{"uint64.parquet", "u NULLABLE INTEGER 501"},
		{"json.parquet", "j NULLABLE  501"},
		{"list.parquet", "l NULLABLE RECORD 501"},
		{"struct.parquet", "s NULLABLE RECORD 501"},
	} {
		if got := describe(fixtureColumns(t, c.file)); got != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.file, got, c.want)
		}
	}
	// The Parquet types, as the 501 names them.
	cols := fixtureColumns(t, "types.parquet")
	for i, want := range map[int]string{9: "FIXED_LEN_BYTE_ARRAY", 11: "INT32 (TIME(MILLIS))", 13: "INT64 (TIMESTAMP(MILLIS))"} {
		if !strings.Contains(cols[i].notHere, want) {
			t.Errorf("column %s: %q, want it to name %s", cols[i].Name, cols[i].notHere, want)
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
	for _, name := range []string{"ab.parquet", "types.parquet", "list.parquet"} {
		b, err := os.ReadFile(filepath.Join("testdata", "parquet", name))
		if err != nil {
			f.Fatal(err)
		}
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		elems, err := readParquetSchema(bytes.NewReader(b), int64(len(b)))
		if err == nil {
			_, _ = parquetColumns(elems)
		}
	})
}
