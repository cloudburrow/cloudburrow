package bigqueryfront

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/scritchley/orc"
	orcproto "github.com/scritchley/orc/proto"
)

// orcExamples is the examples directory of the ORC module (#1713): files
// the Apache ORC project's Java writer made for its own tests
// (https://github.com/apache/orc/tree/main/examples, Apache-2.0), with
// the rows its tools print for each in expected/, as the module ships
// them.
func orcExamples(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "github.com/scritchley/orc").Output()
	dir := filepath.Join(strings.TrimSpace(string(out)), "examples")
	if err != nil || strings.TrimSpace(string(out)) == "" {
		t.Skipf("the ORC module's directory is not known: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Skipf("the ORC module's examples: %v", err)
	}
	return dir
}

func openORCFile(t *testing.T, path string) *orcFile {
	t.Helper()
	fh, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fh.Close() })
	st, _ := fh.Stat()
	o, err := openORC(fh, st.Size())
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return o
}

// orcExpect is v, a value the front read of a column of type n, as the
// Apache ORC tools print it in JSON (numbers as big.Rat strings, so 1 and
// 1.0 compare equal).
func orcExpect(n *orcNode, v any) any {
	if v == nil {
		return nil
	}
	if n.single != nil {
		return orcExpect(n.single, v.(orcUnion).value)
	}
	switch n.kind {
	case orcproto.Type_LIST:
		var out []any
		for _, x := range v.([]any) {
			out = append(out, orcExpect(n.elem, x))
		}
		return out
	case orcproto.Type_MAP:
		var out []any
		for _, e := range v.([]orcMapEntry) {
			out = append(out, map[string]any{"key": orcExpect(n.fields[0].node, e.key), "value": orcExpect(n.fields[1].node, e.value)})
		}
		return out
	case orcproto.Type_STRUCT:
		out := map[string]any{}
		for i, x := range v.([]any) {
			out[n.fields[i].name] = orcExpect(n.fields[i].node, x)
		}
		return out
	case orcproto.Type_UNION:
		u := v.(orcUnion)
		return map[string]any{"tag": ratString(fmt.Sprint(u.tag)), "value": orcExpect(n.fields[u.tag].node, u.value)}
	case orcproto.Type_DATE:
		return time.Unix(v.(int64)*86400, 0).UTC().Format("2006-01-02")
	case orcproto.Type_TIMESTAMP:
		return v.(time.Time).Format("2006-01-02 15:04:05.999999999")
	case orcproto.Type_BINARY:
		var out []any
		for _, b := range v.([]byte) {
			out = append(out, ratString(fmt.Sprint(b)))
		}
		return out
	case orcproto.Type_DECIMAL:
		d := v.(orcDecimal)
		return new(big.Rat).SetFrac(d.unscaled, new(big.Int).Exp(big.NewInt(10), big.NewInt(d.scale), nil)).RatString()
	case orcproto.Type_FLOAT:
		return orcFloat32(float32(v.(orc.Float)))
	case orcproto.Type_STRING, orcproto.Type_VARCHAR, orcproto.Type_CHAR, orcproto.Type_BOOLEAN:
		return v
	}
	return ratString(fmt.Sprint(v))
}

// orcFloat32 is a FLOAT column's value: Java prints a float32 by its own
// rule, so the expected row's number is compared to within a float32's
// precision.
type orcFloat32 float32

// orcSame compares got (orcExpect) with want (jsonExpect).
func orcSame(got, want any) bool {
	switch g := got.(type) {
	case orcFloat32:
		w, ok := want.(string)
		if !ok {
			return false
		}
		r, ok := new(big.Rat).SetString(w)
		if !ok {
			return false
		}
		f, _ := r.Float64()
		return math.Abs(f-float64(g)) <= 1e-6*math.Abs(f)
	case []any:
		w, ok := want.([]any)
		if !ok || len(w) != len(g) {
			return false
		}
		for i := range g {
			if !orcSame(g[i], w[i]) {
				return false
			}
		}
		return true
	case map[string]any:
		w, ok := want.(map[string]any)
		if !ok || len(w) != len(g) {
			return false
		}
		for k := range g {
			if !orcSame(g[k], w[k]) {
				return false
			}
		}
		return true
	}
	return reflect.DeepEqual(got, want)
}

func ratString(s string) string {
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return s
	}
	return r.RatString()
}

// jsonExpect is a line of the expected rows in orcExpect's form.
func jsonExpect(v any) any {
	switch x := v.(type) {
	case json.Number:
		return ratString(x.String())
	case string:
		if t, err := time.Parse("2006-01-02 15:04:05.999999999", x); err == nil {
			return t.Format("2006-01-02 15:04:05.999999999")
		}
		return x
	case []any:
		if len(x) == 0 {
			return []any(nil)
		}
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = jsonExpect(e)
		}
		return out
	case map[string]any:
		out := map[string]any{}
		for k, e := range x {
			out[k] = jsonExpect(e)
		}
		return out
	}
	return v
}

// TestORCExamples reads Apache ORC's example files, of every compression
// kind BigQuery reads but none (Zlib, Snappy, LZO, LZ4) and every type,
// and compares each row with the one Apache ORC's tools print.
func TestORCExamples(t *testing.T) {
	dir := orcExamples(t)
	for _, name := range []string{
		"TestOrcFile.test1",                 // ZLIB: every primitive, struct, list, map
		"TestOrcFile.testSnappy",            // SNAPPY
		"TestVectorOrcFile.testLz4",         // LZ4
		"TestVectorOrcFile.testLzo",         // LZO
		"TestOrcFile.testUnionAndTimestamp", // union, timestamp, decimal, NULLs
		"TestOrcFile.testDate1900",          // dates and timestamps before 1970
		"decimal",                           // decimals
		"nulls-at-end-snappy",               // NULLs at the end of stripes
		"TestOrcFile.emptyFile",             // no rows
	} {
		t.Run(name, func(t *testing.T) {
			o := openORCFile(t, filepath.Join(dir, name+".orc"))
			cols, err := orcColumns(o.types, pqOptions{decimalTypes: []string{"BIGNUMERIC"}})
			if err != nil {
				t.Fatal(err)
			}
			gz, err := os.Open(filepath.Join(dir, "expected", name+".jsn.gz"))
			if err != nil {
				t.Fatal(err)
			}
			defer gz.Close()
			zr, err := gzip.NewReader(gz)
			if err != nil {
				t.Fatal(err)
			}
			sc := bufio.NewScanner(zr)
			sc.Buffer(nil, 1<<20)
			root := &orcNode{kind: orcproto.Type_STRUCT, fields: cols}
			n, err := o.rows(func(vals []any) error {
				if !sc.Scan() {
					return errors.New("more rows than expected")
				}
				d := json.NewDecoder(bytes.NewReader(sc.Bytes()))
				d.UseNumber()
				var want any
				if err := d.Decode(&want); err != nil {
					return err
				}
				got := orcExpect(root, vals)
				if !orcSame(got, jsonExpect(want)) {
					return fmt.Errorf("row %s:\n got %v\nwant %v", sc.Text(), got, jsonExpect(want))
				}
				// And every value writes as a literal of its column.
				m, err := orcRow(cols, vals, "")
				if err != nil {
					return err
				}
				for _, f := range orcTableFields(cols) {
					if _, err := literalFor(f, m[strings.ToLower(f.Name)]); err != nil {
						return err
					}
				}
				return nil
			})
			if err != nil {
				t.Fatalf("after %d rows: %v", n, err)
			}
			if sc.Scan() {
				t.Fatalf("read %d rows, fewer than expected", n)
			}
			if n == 0 && name != "TestOrcFile.emptyFile" {
				t.Fatal("read no rows")
			}
		})
	}
}

func TestORCSchemaOfTest1(t *testing.T) {
	o := openORCFile(t, filepath.Join(orcExamples(t), "TestOrcFile.test1.orc"))
	cols, err := orcColumns(o.types, pqOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got := describeFields(orcTableFields(cols))
	for _, want := range []string{"boolean1 BOOLEAN", "byte1 INTEGER", "float1 FLOAT", "bytes1 BYTES", "string1 STRING",
		"middle RECORD", "list REPEATED RECORD", "map REPEATED RECORD"} {
		if !strings.Contains(got, want) {
			t.Errorf("schema %s lacks %q", got, want)
		}
	}
}

// writeORC writes rows of schema with the module's writer.
func writeORC(t *testing.T, schema string, rows ...[]any) []byte {
	t.Helper()
	td, err := orc.ParseSchema(schema)
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	w, err := orc.NewWriter(&b, orc.SetSchema(td))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if err := w.Write(r...); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestORCWrittenRows(t *testing.T) {
	data := writeORC(t, "struct<id:int,name:string,day:date,at:timestamp,tags:array<string>,attrs:map<string,int>,u:uniontype<int,string>>",
		[]any{int64(1), "a", time.Date(2024, 2, 29, 0, 0, 0, 0, time.UTC), time.Date(2024, 1, 2, 3, 4, 5, 123456789, time.UTC),
			[]any{"x", "y"}, map[string]any{"k": int64(7)}, orc.UnionValue{Tag: 0, Value: int64(5)}},
		[]any{int64(2), nil, nil, nil, []any{}, nil, orc.UnionValue{Tag: 1, Value: "s"}},
		[]any{int64(3), "c", time.Date(1969, 12, 31, 0, 0, 0, 0, time.UTC), nil, nil, map[string]any{"m": int64(9)}, orc.UnionValue{Tag: 0, Value: nil}},
	)
	o, err := openORC(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	cols, err := orcColumns(o.types, pqOptions{})
	if err != nil {
		t.Fatal(err)
	}
	fields := orcTableFields(cols)
	if got := describeFields(fields); !strings.Contains(got, "u RECORD") || !strings.Contains(got, "attrs REPEATED RECORD") {
		t.Errorf("schema %s", got)
	}
	var lines []string
	if _, err := o.rows(func(vals []any) error {
		m, err := orcRow(cols, vals, "")
		if err != nil {
			return err
		}
		var parts []string
		for _, f := range fields {
			s, err := literalFor(f, m[strings.ToLower(f.Name)])
			if err != nil {
				return err
			}
			parts = append(parts, s)
		}
		lines = append(lines, strings.Join(parts, ", "))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"1, 'a', DATE '2024-02-29', TIMESTAMP '2024-01-02 03:04:05.123456+00', ARRAY<STRING>['x', 'y'], " +
			"ARRAY<STRUCT<`key` STRING, `value` INT64>>[STRUCT<`key` STRING, `value` INT64>('k', 7)], STRUCT<`field_0` INT64, `field_1` STRING>(5, NULL)",
		"2, NULL, NULL, NULL, ARRAY<STRING>[], ARRAY<STRUCT<`key` STRING, `value` INT64>>[], " +
			"STRUCT<`field_0` INT64, `field_1` STRING>(NULL, 's')",
		"3, 'c', DATE '1969-12-31', NULL, ARRAY<STRING>[], ARRAY<STRUCT<`key` STRING, `value` INT64>>[" +
			"STRUCT<`key` STRING, `value` INT64>('m', 9)], STRUCT<`field_0` INT64, `field_1` STRING>(NULL, NULL)",
	}
	if !reflect.DeepEqual(lines, want) {
		t.Errorf("rows:\n%s\nwant\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
}

func TestORCSchemaRefusals(t *testing.T) {
	for _, c := range []struct{ schema, want string }{
		{"struct<a:array<array<int>>>", "list of lists"},
		{"struct<a:array<map<string,int>>>", "list of maps"},
	} {
		data := writeORC(t, c.schema)
		o, err := openORC(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			t.Fatal(err)
		}
		_, err = orcColumns(o.types, pqOptions{})
		var se *avSchemaError
		if !errors.As(err, &se) || se.status != 400 || !strings.Contains(se.msg, c.want) {
			t.Errorf("%s: %v, want 400 %q", c.schema, err, c.want)
		}
	}
	// A decimal decimalTargetTypes makes STRING is 501.
	o := openORCFile(t, filepath.Join(orcExamples(t), "decimal.orc"))
	_, err := orcColumns(o.types, pqOptions{decimalTypes: []string{"STRING"}})
	var se *avSchemaError
	if !errors.As(err, &se) || se.status != 501 {
		t.Errorf("decimal as STRING: %v, want 501", err)
	}
}

func TestORCNotORC(t *testing.T) {
	for _, data := range [][]byte{nil, []byte("PAR1 not orc"), []byte("ORC\x00\x01garbage\x05"),
		append([]byte("ORC"), bytes.Repeat([]byte{0xff}, 64)...)} {
		if _, err := openORC(bytes.NewReader(data), int64(len(data))); !errors.Is(err, errNotORC) {
			t.Errorf("%q: %v, want errNotORC", data, err)
		}
	}
	// A file cut short reads as not ORC, never panics.
	data := writeORC(t, "struct<a:int,b:string>", []any{int64(1), "x"}, []any{int64(2), "y"})
	for cut := 1; cut < len(data); cut++ {
		trunc := data[cut:]
		if o, err := openORC(bytes.NewReader(trunc), int64(len(trunc))); err == nil {
			_, _ = o.rows(func([]any) error { return nil })
		}
		mangled := append([]byte(nil), data...)
		mangled[cut] ^= 0x5a
		if o, err := openORC(bytes.NewReader(mangled), int64(len(mangled))); err == nil {
			_, _ = o.rows(func([]any) error { return nil })
		}
	}
}

func TestORCDecimalAndDateLiterals(t *testing.T) {
	n := &orcNode{kind: orcproto.Type_DECIMAL, bq: "NUMERIC", scalar: pqScalar{bq: "NUMERIC", scale: 2}}
	if got, err := n.literal(orcDecimal{unscaled: big.NewInt(-12345), scale: 2}, "d"); err != nil || got != "NUMERIC '-123.45'" {
		t.Errorf("decimal: %q %v", got, err)
	}
	if got, err := n.literal(orcDecimal{unscaled: big.NewInt(5), scale: 0}, "d"); err != nil || got != "NUMERIC '5.00'" {
		t.Errorf("decimal rescaled: %q %v", got, err)
	}
	d := &orcNode{kind: orcproto.Type_DATE, bq: "DATE"}
	if _, err := d.literal(int64(3000000), "d"); err == nil || !strings.Contains(err.Error(), "invalid date value") {
		t.Errorf("a date past 9999: %v", err)
	}
	s := &orcNode{kind: orcproto.Type_STRING, bq: "STRING"}
	if _, err := s.literal("\xff", "s"); err == nil || !strings.Contains(err.Error(), "UTF-8") {
		t.Errorf("a string not UTF-8: %v", err)
	}
}

// TestORCZstdChunks reads a ZSTD stream: chunks of the file's codec, one
// stored as it is (the header's low bit), as ORC frames them.
func TestORCZstdChunks(t *testing.T) {
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	first, second := bytes.Repeat([]byte("orc zstd "), 100), []byte("kept as it is")
	frame := func(b []byte, original bool) []byte {
		h := len(b) << 1
		if original {
			h |= 1
		}
		return append([]byte{byte(h), byte(h >> 8), byte(h >> 16)}, b...)
	}
	stream := append(frame(enc.EncodeAll(first, nil), false), frame(second, true)...)
	kind := orcproto.CompressionKind_ZSTD
	o := &orcFile{ps: &orcproto.PostScript{Compression: &kind}}
	got, err := o.decompress(stream)
	if err != nil {
		t.Fatal(err)
	}
	if want := append(append([]byte(nil), first...), second...); !bytes.Equal(got, want) {
		t.Errorf("got %q", got)
	}
	if _, err := o.decompress(frame([]byte("not zstd"), false)); !errors.Is(err, errNotORC) {
		t.Errorf("a corrupt chunk: %v", err)
	}
}
