package bigqueryfront

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// TestInfinityRecords (#1077): a JSON load's RECORD value that holds
// ±Infinity, at any depth and in a REPEATED RECORD, is written as the list
// of its fields in the schema's order (the form the emulator stores); a
// RECORD without one, and every other column, is left as it was.
func TestInfinityRecords(t *testing.T) {
	var fields []field
	_ = json.Unmarshal([]byte(`[{"name":"id","type":"INTEGER"},{"name":"f","type":"FLOAT"},`+
		`{"name":"r","type":"RECORD","fields":[{"name":"g","type":"FLOAT64"},{"name":"h","type":"INTEGER"},`+
		`{"name":"s","type":"RECORD","fields":[{"name":"x","type":"FLOAT"}]},{"name":"a","type":"FLOAT","mode":"REPEATED"}]},`+
		`{"name":"rr","type":"RECORD","mode":"REPEATED","fields":[{"name":"g","type":"FLOAT"}]},`+
		`{"name":"plain","type":"RECORD","fields":[{"name":"g","type":"FLOAT"}]}]`), &fields)
	for _, c := range []struct{ in, want string }{
		{`{"id":1,"f":"Infinity","r":{"G":"-Infinity","h":2}}`,
			`{"f":"Infinity","id":1,"r":[{"g":"-Infinity"},{"h":2},{"s":null},{"a":null}]}`},
		{`{"r":{"s":{"x":"inf"}}}`, `{"r":[{"g":null},{"h":null},{"s":[{"x":"inf"}]},{"a":null}]}`},
		{`{"r":{"a":[1,"-Infinity"]}}`, `{"r":[{"g":null},{"h":null},{"s":null},{"a":[1,"-Infinity"]}]}`},
		{`{"rr":[{"g":1.5},{"g":"Infinity"}]}`, `{"rr":[[{"g":1.5}],[{"g":"Infinity"}]]}`},
		{`{"plain":{"g":1.5},"r":{"g":"Infinityx"}}`, ``},
	} {
		obj := map[string]any{}
		dec := json.NewDecoder(strings.NewReader(c.in))
		dec.UseNumber()
		_ = dec.Decode(&obj)
		changed := infinityRecords(fields, obj)
		if c.want == "" {
			if changed {
				t.Errorf("%s: changed to %s", c.in, mustJSON(obj))
			}
			continue
		}
		if got := mustJSON(obj); !changed || got != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.in, got, c.want)
		}
	}
}

// TestJSONLoadSendsInfinityRecordsAsAList (#1077): through the front, a
// JSON upload's record with -Infinity in a RECORD reaches the emulator in
// the list form, and the other records unchanged.
func TestJSONLoadSendsInfinityRecordsAsAList(t *testing.T) {
	emu := &valuesEmulator{}
	job := valuesLoad("NEWLINE_DELIMITED_JSON", "")
	data := `{"id":1,"r":{"g":"-Infinity"}}` + "\n" + `{"id":2,"f":1.5}` + "\n"
	if w := upload(t, Wrap(emu), job, data); w.Code != 200 {
		t.Fatalf("upload: %d %s", w.Code, w.Body)
	}
	want := `{"id":1,"r":[{"x":null},{"g":"-Infinity"}]}` + "\n" + `{"id":2,"f":1.5}` + "\n"
	if len(emu.data) != 1 || emu.data[0] != want {
		t.Errorf("sent %q, want %q", emu.data, want)
	}
}

// TestInfinityRows (#1077): the emulator's "+Inf" and "-Inf" in a FLOAT64
// cell, at the top level, in a RECORD and in a REPEATED column, are
// written "Infinity" and "-Infinity" in jobs.query's and
// jobs.getQueryResults's answers and tabledata.list's rows; a STRING cell
// that reads "+Inf" is not changed, nor a row whose cells do not match the
// schema.
func TestInfinityRows(t *testing.T) {
	const schema = `{"fields":[{"name":"s","type":"STRING"},{"name":"f","type":"FLOAT"},` +
		`{"name":"r","type":"RECORD","fields":[{"name":"g","type":"FLOAT64"}]},{"name":"a","type":"FLOAT","mode":"REPEATED"},` +
		`{"name":"rr","type":"RECORD","mode":"REPEATED","fields":[{"name":"g","type":"FLOAT"}]}]}`
	row := `{"f":[{"v":"+Inf"},{"v":"+Inf"},{"v":{"f":[{"v":"-Inf"}]}},{"v":[{"v":"1.5"},{"v":"-Inf"}]},` +
		`{"v":[{"v":{"f":[{"v":"+Inf"}]}}]}]}`
	want := `{"f":[{"v":"+Inf"},{"v":"Infinity"},{"v":{"f":[{"v":"-Infinity"}]}},{"v":[{"v":"1.5"},{"v":"-Infinity"}]},` +
		`{"v":[{"v":{"f":[{"v":"Infinity"}]}}]}]}`
	rec := newRecorder()
	rec.WriteHeader(200)
	rec.body.WriteString(`{"jobComplete":true,"schema":` + schema + `,"rows":[` + row + `,{"f":[{"v":"+Inf"}]}]}`)
	w := newRecorder()
	infinityAnswer(w, rec)
	var got struct {
		Rows []json.RawMessage `json:"rows"`
	}
	if err := json.Unmarshal(w.body.Bytes(), &got); err != nil || len(got.Rows) != 2 {
		t.Fatalf("answer %s: %v", w.body.Bytes(), err)
	}
	if !jsonEqual(got.Rows[0], want) || !jsonEqual(got.Rows[1], `{"f":[{"v":"+Inf"}]}`) {
		t.Errorf("jobs.query rows\n got %s\nwant %s", got.Rows, want)
	}
	rows := infinityTableRows([]byte(`{"schema":`+schema+`}`), []json.RawMessage{json.RawMessage(row)})
	if !jsonEqual(rows[0], want) {
		t.Errorf("tabledata.list row\n got %s\nwant %s", rows[0], want)
	}
	// An answer with no infinity is passed on byte for byte.
	rec = newRecorder()
	rec.body.WriteString(`{"rows":[{"f":[{"v":"1"}]}], "schema":` + schema + `}`)
	w = newRecorder()
	infinityAnswer(w, rec)
	if !bytes.Equal(w.body.Bytes(), rec.body.Bytes()) {
		t.Errorf("changed %s", w.body.Bytes())
	}
}

func jsonEqual(a []byte, b string) bool {
	var x, y any
	return json.Unmarshal(a, &x) == nil && json.Unmarshal([]byte(b), &y) == nil && mustJSON(x) == mustJSON(y)
}
