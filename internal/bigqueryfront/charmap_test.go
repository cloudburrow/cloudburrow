package bigqueryfront

import (
	"net/http"
	"strings"
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
// refuse is 400 under STRICT and 501 under V1 and V2 (where the names
// given are the file's once replaced, #1373: one still refused).
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
		{[]field{{Name: "a.b"}}, "V2", http.StatusBadRequest}, // #1547
		{[]field{{Name: "r", Fields: []field{{Name: "a.b"}}}}, "V1", http.StatusBadRequest},
		{[]field{{Name: "a.b!"}}, "V1", http.StatusNotImplemented},
		{[]field{{Name: "1a"}}, "V1", http.StatusNotImplemented},
		{[]field{{Name: "a_"}, {Name: "r", Fields: []field{{Name: "b_c"}}}}, "V1", 0},
	} {
		if got, _ := parquetCharMapNames(c.fields, c.charMap); got != c.want {
			t.Errorf("parquetCharMapNames(%v, %q) = %d, want %d", c.fields, c.charMap, got, c.want)
		}
	}
}

// TestParquetNamesUnderCharMap (#1373): under V1 and V2 a Parquet file's
// names, at every depth, have each unsupported character but a period
// replaced with "_"; names the same once replaced are 501 (notHere).
func TestParquetNamesUnderCharMap(t *testing.T) {
	leaf := func(name string) pqElement {
		return pqElement{Name: name, Type: pqInt64, Repetition: pqOptional, Children: pqNone, Converted: pqNone,
			Scale: pqNone, Precision: pqNone}
	}
	group := func(name string, n int) pqElement {
		return pqElement{Name: name, Type: pqNone, Repetition: pqOptional, Children: n, Converted: pqNone,
			Scale: pqNone, Precision: pqNone}
	}
	root := pqElement{Name: "schema", Type: pqNone, Repetition: pqNone, Children: 4, Converted: pqNone,
		Scale: pqNone, Precision: pqNone}
	elems := []pqElement{root, leaf("first name"), leaf("a!"), leaf("x.y"), group("r-ec?", 1), leaf("in ner!")}
	describe := func(cols []*pqCol) string {
		fs := make([]field, len(cols))
		for i, c := range cols {
			fs[i] = c.field
		}
		return describeFields(fs)
	}
	for _, c := range []struct{ charMap, want string }{
		{"", "first name, a!, x.y, r-ec?"},
		{"V1", "first_name, a_, x.y, r_ec_"},
		{"V2", "first name, a_, x.y, r-ec_"},
	} {
		cols, err := parquetColumns(elems, pqOptions{charMap: c.charMap})
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, col := range cols {
			names = append(names, col.Name)
		}
		if got := strings.Join(names, ", "); got != c.want {
			t.Errorf("%q: names %s, want %s (%s)", c.charMap, got, c.want, describe(cols))
		}
		if c.charMap == "" {
			continue // r-ec? is refused, not read (parquetCharMapNames answers it)
		}
		inner := map[string]string{"V1": "in_ner_", "V2": "in ner_"}[c.charMap]
		if r := cols[3]; len(r.kids) != 1 || r.kids[0].Name != inner || r.Fields[0].Name != inner {
			t.Errorf("%q: the RECORD's field is %+v, want %q", c.charMap, r.Fields, inner)
		}
		if cols[1].notHere != "" {
			t.Errorf("%q: a_ is not loaded: %s", c.charMap, cols[1].notHere)
		}
	}
	same := []pqElement{root, leaf("a!"), leaf("a?"), leaf("b"), leaf("c")}
	cols, err := parquetColumns(same, pqOptions{charMap: "V1"})
	if err != nil {
		t.Fatal(err)
	}
	if cols[0].notHere != "" || !strings.Contains(cols[1].notHere, "both \"a_\"") {
		t.Errorf("a! and a? under V1: %q, %q, want the second 501", cols[0].notHere, cols[1].notHere)
	}
}
