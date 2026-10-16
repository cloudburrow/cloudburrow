package bigqueryfront

import (
	"net/http"
	"testing"
)

// TestCharMapFormat (#1336): columnNameCharacterMap is refused for a load
// format other than CSV and Parquet.
func TestCharMapFormat(t *testing.T) {
	for _, c := range []struct {
		charMap, format string
		refused         bool
	}{
		{"", "NEWLINE_DELIMITED_JSON", false},
		{"COLUMN_NAME_CHARACTER_MAP_UNSPECIFIED", "AVRO", false},
		{"V1", "", false},
		{"V2", "CSV", false},
		{"STRICT", "PARQUET", false},
		{"V2", "NEWLINE_DELIMITED_JSON", true},
		{"V1", "AVRO", true},
		{"STRICT", "ORC", true},
	} {
		if got := charMapFormat(c.charMap, c.format) != ""; got != c.refused {
			t.Errorf("charMapFormat(%q, %q) refused %v, want %v", c.charMap, c.format, got, c.refused)
		}
	}
}

// TestParquetCharMapNames (#1336): a Parquet file's column name the rules
// refuse is 400 under STRICT and 501 under V1 and V2.
func TestParquetCharMapNames(t *testing.T) {
	good := []field{{Name: "a"}, {Name: "first name"}}
	bad := []field{{Name: "a!"}}
	for _, c := range []struct {
		fields  []field
		charMap string
		want    int
	}{
		{good, "", 0},
		{good, "V2", 0},
		{[]field{{Name: "first name"}}, "V1", http.StatusNotImplemented},
		{bad, "", http.StatusBadRequest},
		{bad, "STRICT", http.StatusBadRequest},
		{bad, "V1", http.StatusNotImplemented},
		{bad, "V2", http.StatusNotImplemented},
	} {
		if got, _ := parquetCharMapNames(c.fields, c.charMap); got != c.want {
			t.Errorf("parquetCharMapNames(%v, %q) = %d, want %d", c.fields, c.charMap, got, c.want)
		}
	}
}
