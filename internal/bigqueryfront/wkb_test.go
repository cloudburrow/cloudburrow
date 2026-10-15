package bigqueryfront

import (
	"bytes"
	"encoding/hex"
	"os"
	"testing"
)

// TestWKTToWKB (#957): the WKT tabledata.list gives, as little-endian WKB.
func TestWKTToWKB(t *testing.T) {
	for wkt, want := range map[string]string{
		"POINT(1 2)":           "0101000000000000000000f03f0000000000000040",
		"LINESTRING(1 2, 3 4)": "010200000002000000000000000000f03f000000000000004000000000000008400000000000001040",
		"POLYGON((0 0, 1 0, 1 1, 0 0))": "01030000000100000004000000" + "00000000000000000000000000000000" +
			"000000000000f03f0000000000000000" + "000000000000f03f000000000000f03f" + "00000000000000000000000000000000",
		"MULTIPOINT(1 2, 3 4)": "0104000000020000000101000000000000000000f03f0000000000000040" +
			"010100000000000000000008400000000000001040",
		"MULTIPOINT((1 2))":              "0104000000010000000101000000000000000000f03f0000000000000040",
		"GEOMETRYCOLLECTION EMPTY":       "010700000000000000",
		"GEOMETRYCOLLECTION(POINT(1 2))": "0107000000010000000101000000000000000000f03f0000000000000040",
		"MULTIPOLYGON(((0 0, 1 0, 1 1, 0 0)))": "01060000000100000001030000000100000004000000" +
			"00000000000000000000000000000000" + "000000000000f03f0000000000000000" + "000000000000f03f000000000000f03f" +
			"00000000000000000000000000000000",
	} {
		got, err := wktToWKB(wkt)
		if err != nil {
			t.Errorf("%s: %v", wkt, err)
			continue
		}
		if hex.EncodeToString(got) != want {
			t.Errorf("%s:\n got %x\nwant %s", wkt, got, want)
		}
	}
	for _, bad := range []string{"POINT(1 2 3)", "CIRCLE(1)", "POINT(1 2", "POINT(1 2) x"} {
		if _, err := wktToWKB(bad); err == nil {
			t.Errorf("%s was written", bad)
		}
	}
}

// TestParquetExtractOfNativeGeography (#957): with
// nativeGeographyExportEnabled a GEOGRAPHY column, top-level and in a
// RECORD, is BYTE_ARRAY with the GEOGRAPHY logical type (edges spherical)
// and WKB values; other columns keep theirs. CLOUDBURROW_GEO_PARQUET names
// a file to write it to, to read it with another Parquet reader by hand.
func TestParquetExtractOfNativeGeography(t *testing.T) {
	fields := []field{{Name: "i", Type: "INT64"}, {Name: "g", Type: "GEOGRAPHY"},
		{Name: "r", Type: "RECORD", Fields: []field{{Name: "s", Type: "STRING"}, {Name: "h", Type: "GEOGRAPHY", Mode: "REPEATED"}}}}
	v := func(x any) map[string]any { return map[string]any{"v": x} }
	rows := [][]any{
		{"1", "POINT(1 2)", map[string]any{"f": []any{v("x"), v([]any{v("LINESTRING(1 2, 3 4)")})}}},
		{"2", nil, nil},
	}
	for _, codec := range []string{"", "SNAPPY"} {
		data, msg := encodeExtract(writtenExtract{parquet: true, parquetCodec: codec, parquetNativeGeography: true, fields: fields}, rows)
		if msg != "" {
			t.Fatal(msg)
		}
		if p := os.Getenv("CLOUDBURROW_GEO_PARQUET"); p != "" && codec == "" {
			_ = os.WriteFile(p, data, 0o644)
		}
		schema, err := readParquetSchema(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			t.Fatal(err)
		}
		geo := map[string]bool{}
		for _, e := range schema {
			if e.Logical != nil && e.Logical.Kind == 18 {
				if e.Type != pqByteArray {
					t.Errorf("%s is GEOGRAPHY of type %d", e.Name, e.Type)
				}
				geo[e.Name] = true
			}
			if e.Name == "s" && (e.Logical == nil || e.Logical.Kind != pqLogicalString) {
				t.Errorf("s lost its STRING type: %+v", e.Logical)
			}
		}
		if !geo["g"] || !geo["element"] || len(geo) != 2 {
			t.Errorf("GEOGRAPHY columns %v", geo)
		}
		if !bytes.Contains(data, []byte{0x01, 0x01, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0xf0, 0x3f}) && codec == "" {
			t.Errorf("no WKB POINT(1 2) in the file")
		}
	}
}
