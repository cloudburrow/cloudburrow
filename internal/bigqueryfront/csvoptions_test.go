package bigqueryfront

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

// The tests in this file are #952.

const requiredB = `{"fields":[{"name":"a","type":"INTEGER"},{"name":"b","type":"STRING","mode":"REQUIRED"}]}`

func loadJobWith(schema, extra string) string {
	return `{"jobReference":{"projectId":"p","jobId":"j1"},"configuration":{"load":{"sourceFormat":"CSV",` +
		`"destinationTable":{"datasetId":"ds","tableId":"t"},"schema":` + schema + extra + `}}}`
}

// TestCSVLoadOptions (#952): allowQuotedNewlines, encoding ISO-8859-1,
// maxBadRecords, ignoreUnknownValues, nullMarkers,
// preserveAsciiControlCharacters and sourceColumnMatch NAME are carried
// out on the data the front passes on, and a REQUIRED column gets a value.
func TestCSVLoadOptions(t *testing.T) {
	for _, c := range []struct {
		name, job, data, want string
	}{
		{"quoted newline allowed", loadJob(`,"allowQuotedNewlines":true`), "1,\"a\nb\"\n", `[{"a":"1","b":"a\nb"}]`},
		{"ISO-8859-1", loadJob(`,"encoding":"ISO-8859-1"`), "1,caf\xe9\n", `[{"a":"1","b":"café"}]`},
		{"UTF-8", loadJob(`,"encoding":"UTF-8"`), "1,café\n", `[{"a":"1","b":"café"}]`},
		{"maxBadRecords", loadJob(`,"maxBadRecords":2`), "1,x\n2\n3,y,z\n4,w\n", `[{"a":"1","b":"x"},{"a":"4","b":"w"}]`},
		{"maxBadRecords as a string", loadJob(`,"maxBadRecords":"1"`), "1,x\n2\n", `[{"a":"1","b":"x"}]`},
		{"ignoreUnknownValues", loadJob(`,"ignoreUnknownValues":true`), "1,x,extra,more\n2,y\n", `[{"a":"1","b":"x"},{"a":"2","b":"y"}]`},
		{"nullMarkers", loadJob(`,"nullMarkers":["NA","-"]`), "1,NA\n-,y\n2,\"NA\"\n", `[{"a":"1","b":""},{"a":"","b":"y"},{"a":"2","b":"NA"}]`},
		{"nullMarkers with the empty string", loadJob(`,"nullMarkers":["","NA"]`), "1,\n", `[{"a":"1","b":""}]`},
		{"control characters kept", loadJob(`,"preserveAsciiControlCharacters":true`), "1,a\x01b\x00\n", `[{"a":"1","b":"a\u0001b\u0000"}]`},
		{"a tab in a value", loadJob(""), "1,a\tb\n", `[{"a":"1","b":"a\tb"}]`},
		{"sourceColumnMatch NAME", loadJob(`,"sourceColumnMatch":"NAME","skipLeadingRows":1`), "B,a\nx,1\ny,2\n", `[{"a":"1","b":"x"},{"a":"2","b":"y"}]`},
		{"NAME, the header the last skipped row", loadJob(`,"sourceColumnMatch":"NAME","skipLeadingRows":3`), "title\nnote,x\nb,a\nx,1\n", `[{"a":"1","b":"x"}]`},
		{"NAME, jagged", loadJob(`,"sourceColumnMatch":"NAME","skipLeadingRows":"1","allowJaggedRows":true,"fieldDelimiter":"|"`), "b|a\nx\n", `[{"a":"","b":"x"}]`},
		{"POSITION", loadJob(`,"sourceColumnMatch":"POSITION","skipLeadingRows":1`), "b,a\n1,x\n", `[{"a":"1","b":"x"}]`},
		{"REQUIRED, jagged, maxBadRecords", loadJobWith(requiredB, `,"allowJaggedRows":true,"maxBadRecords":1`), "1\n2,y\n", `[{"a":"2","b":"y"}]`},
		{"REQUIRED given", loadJobWith(requiredB, ""), ",y\n", `[{"a":"","b":"y"}]`},
	} {
		emu := &csvEmulator{}
		w := upload(t, Wrap(emu), c.job, c.data)
		if w.Code != 200 {
			t.Errorf("%s: %d %s", c.name, w.Code, w.Body)
			continue
		}
		if got, _ := json.Marshal(emu.rows); string(got) != c.want {
			t.Errorf("%s: loaded %s, want %s", c.name, got, c.want)
		}
	}
}

// TestCSVLoadOptionsRefused (#952): what BigQuery refuses is 400, what the
// front cannot carry out is 501 naming it, and neither loads a row.
func TestCSVLoadOptionsRefused(t *testing.T) {
	for _, c := range []struct {
		name, job, data string
		code            int
		reason          string
		sent            bool // whether the load reaches the emulator (its data then cut)
	}{
		{"quoted newline", loadJob(""), "1,\"a\nb\"\n", 400, "invalid", true},
		{"quoted newline, maxBadRecords", loadJob(`,"maxBadRecords":5`), "1,\"a\nb\"\n", 501, "notImplemented", true},
		{"quoted newline in a skipped row", loadJob(`,"skipLeadingRows":1`), "\"a\nb\",c\n1,x\n", 501, "notImplemented", true},
		{"a row too wide", loadJob(""), "1,x,y\n", 400, "invalid", true},
		{"a row too short", loadJob(""), "1,x\n2\n", 400, "invalid", true},
		{"more bad records than maxBadRecords", loadJob(`,"maxBadRecords":1`), "1\n2\n3,x\n", 400, "invalid", true},
		{"REQUIRED empty", loadJobWith(requiredB, ""), "1,\n", 400, "invalid", true},
		{"REQUIRED missing, jagged", loadJobWith(requiredB, `,"allowJaggedRows":true`), "1\n", 400, "invalid", true},
		{"REQUIRED empty quoted STRING", loadJobWith(requiredB, ""), "1,\"\"\n", 501, "notImplemented", true},
		{"REQUIRED null marker", loadJobWith(requiredB, `,"nullMarker":"NA"`), "1,NA\n", 400, "invalid", true},
		{"empty value, nullMarkers without it", loadJob(`,"nullMarkers":["NA"]`), "1,\n", 501, "notImplemented", true},
		{"control character", loadJob(""), "1,a\x01b\n", 501, "notImplemented", true},
		{"NUL", loadJob(""), "1,a\x00b\n", 501, "notImplemented", true},
		{"NAME header naming another column", loadJob(`,"sourceColumnMatch":"NAME","skipLeadingRows":1`), "c,a\nx,1\n", 501, "notImplemented", true},
		{"NAME header naming a column twice", loadJob(`,"sourceColumnMatch":"NAME","skipLeadingRows":1`), "a,A\nx,1\n", 501, "notImplemented", true},
		{"NAME without a header row", loadJob(`,"sourceColumnMatch":"NAME"`), "a,b\n1,x\n", 501, "notImplemented", false},
		{"sourceColumnMatch unknown", loadJob(`,"sourceColumnMatch":"BEST"`), "1,x\n", 501, "notImplemented", false},
		{"nullMarker and nullMarkers", loadJob(`,"nullMarker":"NA","nullMarkers":["-"]`), "1,x\n", 400, "invalid", false},
		{"encoding unknown", loadJob(`,"encoding":"UTF-7"`), "1,x\n", 400, "invalid", false},
		{"timeZone unknown", loadJob(`,"timeZone":"Mars/Olympus"`), "1,x\n", 400, "invalid", false},
		{"dateFormat with a time", loadJob(`,"dateFormat":"MM/DD/YYYY HH24"`), "1,x\n", 400, "invalid", false},
		// #1338: detected by the front, which does not resolve a mix of
		// types BigQuery does not document (#1457: columns all STRING are
		// now read as BigQuery documents, TestDetectCSVHeader).
		{"timeZone with autodetect, mixed types", `{"configuration":{"load":{"sourceFormat":"CSV","autodetect":true,"timeZone":"UTC",` +
			`"destinationTable":{"projectId":"p","datasetId":"d","tableId":"t"}}}}`, "a\n1\ntrue\n", 501, "notImplemented", false},
		{"maxBadRecords negative", loadJob(`,"maxBadRecords":-1`), "1,x\n", 400, "invalid", false},
	} {
		emu := &csvEmulator{}
		w := upload(t, Wrap(emu), c.job, c.data)
		var got struct {
			Error struct {
				Errors []rowError `json:"errors"`
			} `json:"error"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &got)
		if w.Code != c.code || len(got.Error.Errors) != 1 || got.Error.Errors[0].Reason != c.reason || len(emu.rows) != 0 {
			t.Errorf("%s: %d %s, %d rows; want %d %s", c.name, w.Code, w.Body, len(emu.rows), c.code, c.reason)
		}
		if !c.sent && emu.loads != 0 {
			t.Errorf("%s: the load was sent", c.name)
		}
	}

	// A load with maxBadRecords that the emulator fails is 501: the value
	// it failed on may be a bad record BigQuery leaves out.
	failing := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		http.Error(w, `{"error":{"code":400,"message":"strconv.ParseInt: invalid syntax"}}`, http.StatusBadRequest)
	})
	for _, c := range []struct {
		extra string
		code  int
	}{{`,"maxBadRecords":1`, 501}, {"", 400}} {
		w := upload(t, Wrap(failing), loadJob(c.extra), "x,y\n")
		if w.Code != c.code {
			t.Errorf("the emulator failing a load with %q: %d %s, want %d", c.extra, w.Code, w.Body, c.code)
		}
	}
}

// TestCSVBadValues (#1457): with maxBadRecords, a record holding a value
// its column's type refuses is bad and left out; a BOOL in any of the
// forms BigQuery documents is written as true or false; one bad record
// more than maxBadRecords fails the load, 400 invalid.
func TestCSVBadValues(t *testing.T) {
	const typed = `{"fields":[{"name":"i","type":"INTEGER"},{"name":"f","type":"FLOAT"},{"name":"b","type":"BOOLEAN"},` +
		`{"name":"d","type":"DATE"},{"name":"t","type":"TIME"}]}`
	data := "1,1.5,yes,2024-01-02,01:02:03\n" +
		"x,1,t,2024-01-02,01:02:03\n" + // INTEGER
		"2,one,t,2024-01-02,01:02:03\n" + // FLOAT
		"3,1,maybe,2024-01-02,01:02:03\n" + // BOOLEAN
		"4,1,N,2024-02-30,01:02:03\n" + // DATE
		"5,1,F,2024-01-02,25:00:00\n" + // TIME
		"6,-2e3,0,,\n"
	emu := &csvEmulator{}
	if w := upload(t, Wrap(emu), loadJobWith(typed, `,"maxBadRecords":5`), data); w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	want := `[{"b":"true","d":"2024-01-02","f":"1.5","i":"1","t":"01:02:03"},{"b":"false","d":"","f":"-2e3","i":"6","t":""}]`
	if got, _ := json.Marshal(emu.rows); string(got) != want {
		t.Errorf("loaded %s, want %s", got, want)
	}
	emu = &csvEmulator{}
	if w := upload(t, Wrap(emu), loadJobWith(typed, `,"maxBadRecords":4`), data); w.Code != 400 || len(emu.rows) != 0 {
		t.Errorf("one bad record too many: %d %s, %d rows", w.Code, w.Body, len(emu.rows))
	}
}
