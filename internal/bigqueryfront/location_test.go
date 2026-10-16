package bigqueryfront

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestDatasetInsertGetsTheDefaultLocation(t *testing.T) {
	for _, c := range []struct{ body, want string }{
		{`{"datasetReference":{"datasetId":"d"}}`, `"location":"US"`},
		{`{"datasetReference":{"datasetId":"d"},"location":""}`, `"location":"US"`},
		{`{"datasetReference":{"datasetId":"d"},"location":"EU"}`, `"location":"EU"`},
	} {
		emu := &fakeEmulator{}
		do(t, Wrap(emu), "POST", base+"/datasets", c.body)
		if len(emu.writes) != 1 || !strings.Contains(emu.writes[0], c.want) {
			t.Errorf("%s: the emulator was sent %q, want %s", c.body, emu.writes, c.want)
		}
	}
}

func TestWithLocationKeepsALocation(t *testing.T) {
	for _, c := range []struct {
		in      string
		want    string
		changed bool
	}{{`{}`, "US", true}, {`{"location":""}`, "US", true}, {`{"location":"asia-northeast1"}`, "asia-northeast1", false}} {
		var m map[string]json.RawMessage
		_ = json.Unmarshal([]byte(c.in), &m)
		changed := withLocation(m)
		var got string
		_ = json.Unmarshal(m["location"], &got)
		if got != c.want || changed != c.changed {
			t.Errorf("%s: %q (changed %v), want %q (%v)", c.in, got, changed, c.want, c.changed)
		}
	}
}

func TestJobDatasetsOrder(t *testing.T) {
	var conf map[string]any
	_ = json.Unmarshal([]byte(`{"copy":{"destinationTable":{"datasetId":"dst","tableId":"t"},"sourceTables":[{"datasetId":"a","tableId":"t"},{"projectId":"other","datasetId":"b","tableId":"t"}]},
		"query":{"query":"SELECT * FROM q.t JOIN `+"`p.r.u`"+` USING (x)","useLegacySql":false,"defaultDataset":{"datasetId":"def"}}}`), &conf)
	got := jobDatasets(conf, "p", false)
	want := []string{"dst", "a", "def", "q", "r"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("jobDatasets = %v, want %v", got, want)
	}
	// jobs.query's useLegacySql is true unless set: its text is not read.
	var req map[string]any
	_ = json.Unmarshal([]byte(`{"query":"SELECT * FROM [q.t]"}`), &req)
	if got := jobDatasets(req, "p", true); len(got) != 0 {
		t.Errorf("a legacy query named %v", got)
	}
}

// A dataset the emulator holds without a location (state saved before
// #1371) reads back in the default, and so does its table.
func TestStoredDatasetWithoutLocationReadsBackInTheDefault(t *testing.T) {
	emu := &fakeEmulator{datasets: map[string]bool{"d": true}, schema: `{"fields":[{"name":"a","type":"STRING"}]}`}
	h := Wrap(emu)
	for _, path := range []string{base + "/datasets/d", base + "/datasets/d/tables/t"} {
		code, got := do(t, h, "GET", path, "")
		if code != 200 || got["location"] != "US" {
			t.Errorf("GET %s: %d %v, want location US", path, code, got)
		}
	}
}
