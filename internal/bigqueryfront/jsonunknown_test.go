package bigqueryfront

import (
	"strings"
	"testing"
)

// A JSON load's record with a field outside the schema is bad (#1339): it
// fails the load, 400 invalid, unless ignoreUnknownValues is set, which
// passes it on for the emulator to drop the field; under maxBadRecords it
// is left out and listed. Nested fields are checked in a RECORD and in
// each element of a REPEATED one; names match without case; a field the
// load's schema adds to the table's is known; a null unknown field is 501.
func TestJSONLoadUnknownFields(t *testing.T) {
	const schema = `{"fields":[{"name":"i","type":"INTEGER"},{"name":"r","type":"RECORD","mode":"REPEATED",` +
		`"fields":[{"name":"x","type":"STRING"}]}]}`
	job := func(extra string) string {
		return `{"jobReference":{"projectId":"p","jobId":"j1"},"configuration":{"load":{"sourceFormat":"NEWLINE_DELIMITED_JSON",` +
			`"destinationTable":{"datasetId":"ds","tableId":"t"},"schema":` + schema + extra + `}}}`
	}
	for _, c := range []struct {
		name, extra, data string
		code              int
		sent, inBody      string
	}{
		{"known", "", `{"i":1,"R":[{"X":"a"}]}` + "\n", 200, `{"i":1,"R":[{"X":"a"}]}` + "\n", ""},
		{"unknown", "", `{"i":1,"zz":2}` + "\n", 400, "", "No such field: zz"},
		{"nested", "", `{"i":1,"r":[{"x":"a"},{"x":"b","y":1}]}` + "\n", 400, "", "No such field: r.y"},
		{"ignored", `,"ignoreUnknownValues":true`, `{"i":1,"zz":2}` + "\n", 200, `{"i":1,"zz":2}` + "\n", ""},
		{"left out", `,"maxBadRecords":1`, `{"i":1,"zz":2}` + "\n" + `{"i":2}` + "\n", 200, `{"i":2}` + "\n", ""},
		{"too many", `,"maxBadRecords":1`, `{"zz":1}` + "\n" + `{"zz":2}` + "\n", 400, "", "maxBadRecords is 1"},
		{"null", "", `{"i":1,"zz":null}` + "\n", 501, "", "null"},
	} {
		emu := &valuesEmulator{}
		w := upload(t, Wrap(emu), job(c.extra), c.data)
		if w.Code != c.code || !strings.Contains(w.Body.String(), c.inBody) {
			t.Errorf("%s: %d %s, want %d with %q", c.name, w.Code, w.Body, c.code, c.inBody)
			continue
		}
		if c.code == 200 && (len(emu.data) != 1 || emu.data[0] != c.sent) {
			t.Errorf("%s: sent %q, want %q", c.name, emu.data, c.sent)
		}
		if c.name == "left out" && !strings.Contains(w.Body.String(), "It was left out (1 of maxBadRecords 1)") {
			t.Errorf("left out: the job does not list the bad record: %s", w.Body)
		}
	}

	// Into a table without the column, a load whose schema adds it.
	emu := &valuesEmulator{schema: `{"fields":[{"name":"i","type":"INTEGER"}]}`}
	j := strings.Replace(job(`,"schemaUpdateOptions":["ALLOW_FIELD_ADDITION"]`), schema,
		`{"fields":[{"name":"i","type":"INTEGER"},{"name":"n","type":"STRING"}]}`, 1)
	if w := upload(t, Wrap(emu), j, `{"i":1,"n":"x"}`+"\n"); w.Code != 200 {
		t.Errorf("a field the load's schema adds: %d %s", w.Code, w.Body)
	}
}
