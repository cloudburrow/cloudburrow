package bigqueryfront

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestDetectJSONSchema(t *testing.T) {
	sample := `{"id":"12","name":"a","n":1,"x":1.5,"ok":true,"d":"2020-01-02","tm":"12:34:56","ts":"2020-01-02 03:04:05 UTC","r":{"k":1,"s":["a"]},"tags":[1,2],"none":null}
{"ID":13,"name":"b","n":2.5,"x":2,"ok":"false","d":"2020-01-03","ts":"2020-01-02T03:04Z","r":{"k":"2","z":{"q":true}},"tags":[],"mixed":"12"}

{"mixed":"abc","e":[]}
`
	fields, code, msg := detectJSONSchema([]byte(sample), "the data")
	if code != 0 {
		t.Fatalf("%d %s", code, msg)
	}
	got, _ := json.Marshal(fields)
	want := `[{"name":"id","type":"INTEGER","mode":"NULLABLE","fields":null},{"name":"name","type":"STRING","mode":"NULLABLE","fields":null},` +
		`{"name":"n","type":"FLOAT","mode":"NULLABLE","fields":null},{"name":"x","type":"FLOAT","mode":"NULLABLE","fields":null},` +
		`{"name":"ok","type":"BOOLEAN","mode":"NULLABLE","fields":null},{"name":"d","type":"DATE","mode":"NULLABLE","fields":null},` +
		`{"name":"tm","type":"TIME","mode":"NULLABLE","fields":null},{"name":"ts","type":"TIMESTAMP","mode":"NULLABLE","fields":null},` +
		`{"name":"r","type":"RECORD","mode":"NULLABLE","fields":[{"name":"k","type":"INTEGER","mode":"NULLABLE","fields":null},` +
		`{"name":"s","type":"STRING","mode":"REPEATED","fields":null},{"name":"z","type":"RECORD","mode":"NULLABLE","fields":[` +
		`{"name":"q","type":"BOOLEAN","mode":"NULLABLE","fields":null}]}]},{"name":"tags","type":"INTEGER","mode":"REPEATED","fields":null},` +
		`{"name":"none","type":"STRING","mode":"NULLABLE","fields":null},{"name":"mixed","type":"STRING","mode":"NULLABLE","fields":null},` +
		`{"name":"e","type":"STRING","mode":"REPEATED","fields":null}]`
	if string(got) != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
	for _, bad := range []string{
		`{"a":1}` + "\n" + `{"a":"x"}`,
		`{"a":true}` + "\n" + `{"a":1}`,
		`{"a":{"b":1}}` + "\n" + `{"a":1}`,
		`{"a":[1]}` + "\n" + `{"a":1}`,
		`{"a":[[1]]}`,
		`{"a":{}}`,
		`{"a":"2020/01/02"}`,
		`{"a":"12:34"}`,
		`{"a":99999999999999999999}`,
		`{"a":"2020-01-02"}` + "\n" + `{"a":"2020-01-02 03:04:05"}`,
	} {
		if _, code, _ := detectJSONSchema([]byte(bad), "x"); code != http.StatusNotImplemented {
			t.Errorf("%q: %d, want 501", bad, code)
		}
	}
	if _, code, msg := detectJSONSchema([]byte("[1]\n"), "x"); code != http.StatusBadRequest || !strings.Contains(msg, "line 1") {
		t.Errorf("an array line: %d %s", code, msg)
	}
}

func TestDetectedTimestamps(t *testing.T) {
	fields := []field{{Name: "ts", Type: "TIMESTAMP"}, {Name: "r", Type: "RECORD", Fields: []field{{Name: "t", Type: "TIMESTAMP"}}}}
	obj := map[string]any{"ts": "2020-01-02 03:04:05.5 -05:00", "r": map[string]any{"t": "2020-01-02T03:04"}}
	if !detectedTimestamps(fields, obj) {
		t.Fatal("nothing changed")
	}
	if obj["ts"] != "2020-01-02T08:04:05.5Z" || obj["r"].(map[string]any)["t"] != "2020-01-02T03:04:00Z" {
		t.Errorf("got %v", obj)
	}
}
