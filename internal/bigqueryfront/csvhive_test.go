package bigqueryfront

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"strings"
	"testing"
)

// TestCSVHiveRecords (#1547): each record gets its file's keys' values in
// the keys' columns, wherever the schema puts them, and the record's width
// is counted without them.
func TestCSVHiveRecords(t *testing.T) {
	h := &hiveFiles{keys: []hiveKey{{"dt", "DATE"}, {"n", "INTEGER"}},
		vals: map[string][]string{"gs://b/t/dt=2024-01-02/n=7/a.csv": {"2024-01-02", "7"}}}
	cols := []field{{Name: "a", Type: "STRING"}, {Name: "dt", Type: "DATE"}, {Name: "b", Type: "STRING"}, {Name: "n", Type: "INTEGER"}}
	c, why := csvHiveOf(h, cols, csvOptions{})
	if why != "" {
		t.Fatal(why)
	}
	d, _ := dialectOf("", nil, false, nil)
	d.quotedNewlines, d.keepControl, d.hive = true, true, c
	var out bytes.Buffer
	cw := csv.NewWriter(&out)
	st := &csvState{width: len(cols), keyVals: c.values("gs://b/t/dt=2024-01-02/n=7/a.csv")}
	if err := dialectRecords(cw, strings.NewReader("x,y\nz,w\n"), d, cols, 0, st); err != nil {
		t.Fatal(err)
	}
	cw.Flush()
	if got, want := out.String(), "x,2024-01-02,y,7\nz,2024-01-02,w,7\n"; got != want {
		t.Errorf("got %q want %q", got, want)
	}
	// A record as wide as the columns, keys and all, is too wide.
	err := dialectRecords(csv.NewWriter(&out), strings.NewReader("x,y,z,w\n"), d, cols, 0, &csvState{width: len(cols)})
	if err == nil || !strings.Contains(err.Error(), "more than the 2 columns") {
		t.Errorf("a record of 4 values: %v", err)
	}
	for _, tc := range []struct {
		cols []field
		o    csvOptions
		want string
	}{
		{nil, csvOptions{}, "autodetect"},
		{cols, csvOptions{SourceColumnMatch: "NAME"}, "NAME"},
		{cols, csvOptions{DateFormat: "MM/DD/YYYY"}, "format"},
		{cols[:1], csvOptions{}, "no column for partition key dt"},
	} {
		if _, why := csvHiveOf(h, tc.cols, tc.o); !strings.HasPrefix(why, "!") || !strings.Contains(why, tc.want) {
			t.Errorf("%v %+v: %q", tc.cols, tc.o, why)
		}
	}
}

// TestManifestURIs (#1547): a manifest lists one URI a line; a wildcard,
// a line that is no gs:// URI and a manifest listing none are refused.
func TestManifestURIs(t *testing.T) {
	got, why := manifestURIs(strings.NewReader("gs://b/a.csv\r\n\n  gs://b/c.csv\n"), "gs://b/m")
	if why != "" || fmt.Sprint(got) != "[gs://b/a.csv gs://b/c.csv]" {
		t.Errorf("got %v %q", got, why)
	}
	for data, want := range map[string]string{"gs://b/*.csv\n": "Wildcard", "a.csv\n": "not a gs:// URI", "\n\n": "lists no URI"} {
		if _, why := manifestURIs(strings.NewReader(data), "gs://b/m"); !strings.Contains(why, want) {
			t.Errorf("%q: %q", data, why)
		}
	}
}
