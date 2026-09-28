package bigqueryfront

import (
	"encoding/json"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// sameJSON reports whether two JSON texts hold the same value.
func sameJSON(a, b string) bool {
	var x, y any
	return json.Unmarshal([]byte(a), &x) == nil && json.Unmarshal([]byte(b), &y) == nil && reflect.DeepEqual(x, y)
}

// TestFloatColumnsAreMadeFloat64 (#1000): tables.insert, a load's schema
// and a load with autodetect make each FLOAT column FLOAT64 in the
// emulator, and the table then reads back FLOAT; the other legacy names,
// and tables.update and tables.patch, are sent as they are.
func TestFloatColumnsAreMadeFloat64(t *testing.T) {
	setup := func() *stateEmulator {
		e := newStateEmulator()
		e.datasets["ds"] = true
		return e
	}
	const floats = `{"fields":[{"name":"f","type":"FLOAT"},{"name":"i","type":"INTEGER"},{"name":"b","type":"BOOLEAN"},` +
		`{"name":"r","type":"RECORD","fields":[{"name":"x","type":"float"}]}]}`

	// tables.insert.
	e := setup()
	code, got := do(t, Wrap(e), "POST", base+"/datasets/ds/tables",
		`{"tableReference":{"projectId":"p","datasetId":"ds","tableId":"t"},"description":"d","schema":`+floats+`}`)
	if code != 200 {
		t.Fatalf("tables.insert: %d %v", code, got)
	}
	if !e.sent(`^POST \S+/datasets/ds/tables .*"name":"f","type":"FLOAT64".*"name":"x","type":"FLOAT64"`) ||
		!e.sent(`^POST \S+/datasets/ds/tables .*"description":"d"`) || e.sent(`^POST \S+/tables .*"type":"(FLOAT|float)"`) {
		t.Errorf("tables.insert was not sent with FLOAT64: %v", e.log)
	}
	if !e.sent(`^POST \S+/tables .*"type":"INTEGER".*"type":"BOOLEAN".*"type":"RECORD"`) {
		t.Errorf("the other legacy names were changed: %v", e.log)
	}
	if !sameJSON(e.tables["ds.t"], `{"type":"TABLE","schema":`+floats+`}`) {
		t.Errorf("the table reads %s, want the client's schema", e.tables["ds.t"])
	}

	// Without FLOAT: sent as it is, once.
	for _, body := range []string{
		`{"tableReference":{"tableId":"u"},"schema":{"fields":[{"name":"f","type":"FLOAT64"}]}}`,
		`{"tableReference":{"tableId":"u"},"schema":{"fields":[{"name":"n","type":"INTEGER"}]}}`,
	} {
		e := setup()
		if code, got := do(t, Wrap(e), "POST", base+"/datasets/ds/tables", body); code != 200 {
			t.Errorf("%s: %d %v", body, code, got)
		}
		if len(e.log) != 1 || !strings.HasSuffix(e.log[0], body) {
			t.Errorf("%s: sent %v, want it as it is", body, e.log)
		}
	}

	// tables.patch and tables.update that add a FLOAT column (#1010): the
	// table is made again through createTable, the column FLOAT64, and
	// the client's request is then sent as it is. One that adds nothing
	// is sent as it is, alone.
	for _, method := range []string{"PATCH", "PUT"} {
		e := setup()
		e.tables["ds.t"] = `{"type":"TABLE","schema":{"fields":[{"name":"f","type":"FLOAT"}]}}`
		body := `{"schema":{"fields":[{"name":"f","type":"FLOAT"},{"name":"g","type":"FLOAT"}]}}`
		if code, got := do(t, Wrap(e), method, base+"/datasets/ds/tables/t", body); code != 200 {
			t.Errorf("%s: %d %v", method, code, got)
		}
		if !e.sent(`^POST \S+/datasets/ds/tables .*"name":"g","type":"FLOAT64".*"tableId":"t"`) ||
			!strings.HasSuffix(e.log[len(e.log)-1], body) || !strings.HasPrefix(e.log[len(e.log)-1], method) {
			t.Errorf("%s: sent %v", method, e.log)
		}
		e = setup()
		e.tables["ds.t"] = `{"type":"TABLE","schema":{"fields":[{"name":"f","type":"FLOAT"}]}}`
		body = `{"schema":{"fields":[{"name":"f","type":"FLOAT","description":"d"}]}}`
		if code, got := do(t, Wrap(e), method, base+"/datasets/ds/tables/t", body); code != 200 {
			t.Errorf("%s: %d %v", method, code, got)
		}
		if e.sent("FLOAT64") || !strings.HasSuffix(e.log[len(e.log)-1], body) {
			t.Errorf("%s of a description was changed: %v", method, e.log)
		}
	}

	// A load's schema: FLOAT64 sent, FLOAT read back; a field the load
	// sent as FLOAT64 stays so.
	e = setup()
	code, got = do(t, Wrap(e), "POST", base+"/jobs", `{"jobReference":{"projectId":"p","jobId":"l1"},"configuration":{"load":{`+
		`"sourceFormat":"NEWLINE_DELIMITED_JSON","destinationTable":{"projectId":"p","datasetId":"ds","tableId":"ld"},`+
		`"schema":{"fields":[{"name":"f","type":"FLOAT"},{"name":"g","type":"FLOAT64"}]}}}}`)
	if reason, done := jobState(got); code != 200 || !done || reason != "" {
		t.Fatalf("a load with a FLOAT field: %d %v", code, got)
	}
	if !e.sent(`^POST \S+/jobs .*"name":"f","type":"FLOAT64"`) {
		t.Errorf("the load was not sent with FLOAT64: %v", e.log)
	}
	if want := `{"type":"TABLE","schema":{"fields":[{"name":"f","type":"FLOAT"},{"name":"g","type":"FLOAT64"}]}}`; !sameJSON(e.tables["ds.ld"], want) {
		t.Errorf("the loaded table reads %s, want %s", e.tables["ds.ld"], want)
	}

	// A load with autodetect that detected a FLOAT column: the table is
	// made again with it FLOAT64, with its rows, and reads FLOAT.
	e = setup()
	e.detected = `{"fields":[{"mode":"NULLABLE","name":"f","type":"FLOAT"},{"mode":"NULLABLE","name":"n","type":"STRING"}]}`
	e.loaded = 3
	code, got = do(t, Wrap(e), "POST", base+"/jobs", `{"jobReference":{"projectId":"p","jobId":"l2"},"configuration":{"load":{`+
		`"sourceFormat":"AVRO","autodetect":true,"destinationTable":{"projectId":"p","datasetId":"ds","tableId":"ad"}}}}`)
	if reason, done := jobState(got); code != 200 || !done || reason != "" {
		t.Fatalf("a load with autodetect: %d %v", code, got)
	}
	if !e.sent(`^POST \S+/datasets/ds/tables .*"tableId":"ad".*`) || !e.sent(`^POST \S+/datasets/ds/tables .*"type":"FLOAT64"`) ||
		e.rows["ds.ad"] != 3 || !sameJSON(e.tables["ds.ad"], `{"type":"TABLE","schema":`+e.detected+`}`) {
		t.Errorf("the detected table: %s, %d rows; sent %v", e.tables["ds.ad"], e.rows["ds.ad"], e.log)
	}
	for k := range e.tables {
		if strings.Contains(k, "_cloudburrow_") {
			t.Errorf("a scratch table was left: %s", k)
		}
	}
	// ... and when that fails, the job fails, naming why.
	e = setup()
	e.detected = `{"fields":[{"name":"f","type":"FLOAT"}]}`
	e.fail = regexp.MustCompile(`^INSERT`)
	h := Wrap(e)
	code, got = do(t, h, "POST", base+"/jobs", `{"jobReference":{"projectId":"p","jobId":"l3"},"configuration":{"load":{`+
		`"sourceFormat":"AVRO","autodetect":true,"destinationTable":{"projectId":"p","datasetId":"ds","tableId":"ad"}}}}`)
	if reason, _ := jobState(got); code != 200 || reason != "backendError" {
		t.Errorf("a failed remake: %d %v", code, got)
	}
	if _, ok := e.tables["ds.ad"]; !ok {
		t.Errorf("a remake that failed before the delete lost the table")
	}
	if code, got := do(t, h, "GET", base+"/jobs/l3", ""); code != 200 {
		t.Errorf("jobs.get of the failed load: %d %v", code, got)
	} else if reason, _ := jobState(got); reason != "backendError" {
		t.Errorf("jobs.get of the failed load: %v", got)
	}
}
