package bigqueryfront

import (
	"regexp"
	"strings"
	"testing"
)

const testKey = "projects/p/locations/us/keyRings/r/cryptoKeys/k"

// TestCopyJobKeys: a copy job's destinationEncryptionConfiguration
// (#1762, copykey.go).
func TestCopyJobKeys(t *testing.T) {
	const schema = `{"fields":[{"name":"a","type":"INTEGER"}]}`
	setup := func() *stateEmulator {
		e := newStateEmulator()
		e.datasets["ds"] = true
		e.datasets[resultsDataset] = true
		e.tables["ds.s"] = `{"type":"TABLE","schema":` + schema + `}`
		e.tables["ds.plain"] = `{"type":"TABLE","schema":` + schema + `}`
		e.tables["ds.keyed"] = `{"type":"TABLE","schema":` + schema + `,"encryptionConfiguration":{"kmsKeyName":"` + testKey + `"}}`
		e.rows["ds.s"], e.rows["ds.plain"], e.rows["ds.keyed"] = 2, 1, 1
		return e
	}
	job := func(dest, key, write string) string {
		extra := ""
		if key != "" {
			extra += `,"destinationEncryptionConfiguration":{"kmsKeyName":"` + key + `"}`
		}
		if write != "" {
			extra += `,"writeDisposition":"` + write + `"`
		}
		return `{"jobReference":{"projectId":"p","jobId":"k"},"configuration":{"copy":{"sourceTable":` +
			`{"projectId":"p","datasetId":"ds","tableId":"s"},"destinationTable":{"projectId":"p","datasetId":"ds","tableId":"` +
			dest + `"}` + extra + `}}}`
	}
	other := "projects/p/locations/us/keyRings/r/cryptoKeys/other"
	for _, c := range []struct {
		name, dest, key, write string
		reason                 string
		wantKey                string // the destination's key after, "-" for none
		rows                   int
	}{
		{"a new table", "new", testKey, "", "", testKey, 2},
		{"a global key", "new", "projects/p/locations/global/keyRings/r/cryptoKeys/k", "", "",
			"projects/p/locations/global/keyRings/r/cryptoKeys/k", 2},
		{"a key of another region", "new", "projects/p/locations/europe-west1/keyRings/r/cryptoKeys/k", "", "invalid", "", 0},
		{"WRITE_APPEND, the same key", "keyed", testKey, "WRITE_APPEND", "", testKey, 3},
		{"WRITE_APPEND, another key", "keyed", other, "WRITE_APPEND", "invalid", testKey, 1},
		{"WRITE_APPEND into default encryption", "plain", testKey, "WRITE_APPEND", "invalid", "-", 1},
		{"WRITE_APPEND with no key", "keyed", "", "WRITE_APPEND", "", testKey, 3},
		{"WRITE_TRUNCATE, another key", "keyed", other, "WRITE_TRUNCATE", "", other, 2},
		{"WRITE_TRUNCATE into default encryption", "plain", testKey, "WRITE_TRUNCATE", "", testKey, 2},
		{"WRITE_TRUNCATE with no key", "keyed", "", "WRITE_TRUNCATE", "", "-", 2},
	} {
		e := setup()
		code, got := do(t, Wrap(e), "POST", base+"/jobs", job(c.dest, c.key, c.write))
		reason, done := jobState(got)
		if code != 200 || !done || reason != c.reason {
			t.Errorf("%s: %d %v", c.name, code, got)
			continue
		}
		// The fake keeps a table's schema alone: what made it is read from
		// the tables.insert sent.
		made := e.sent(`^POST /bigquery/v2/projects/p/datasets/ds/tables `)
		keyed := e.sent(`^POST /bigquery/v2/projects/p/datasets/ds/tables .*"kmsKeyName":"` + regexp.QuoteMeta(c.wantKey) + `"`)
		switch {
		case c.wantKey == "" && made:
			t.Errorf("%s: a table was made: %v", c.name, e.log)
		case c.wantKey == "-" && e.sent(`^POST /bigquery/v2/projects/p/datasets/ds/tables .*kmsKeyName`):
			t.Errorf("%s: the table was given a key: %v", c.name, e.log)
		case c.wantKey != "" && c.wantKey != "-" && strings.HasPrefix(c.write, "WRITE_APPEND") && made:
			t.Errorf("%s: the table was made again: %v", c.name, e.log)
		case c.wantKey != "" && c.wantKey != "-" && !strings.HasPrefix(c.write, "WRITE_APPEND") && !keyed:
			t.Errorf("%s: the table was not made with key %s: %v", c.name, c.wantKey, e.log)
		}
		if e.rows["ds."+c.dest] != c.rows {
			t.Errorf("%s: %d rows, want %d", c.name, e.rows["ds."+c.dest], c.rows)
		}
	}
}

func TestKeyLocation(t *testing.T) {
	for key, want := range map[string]string{
		testKey: "us",
		"projects/p/locations/us/keyRings/r/cryptoKeys/k/cryptoKeyVersions/1": "",
		"projects/p/locations//keyRings/r/cryptoKeys/k":                       "",
		"k": "",
	} {
		if got := keyLocation(key); got != want {
			t.Errorf("keyLocation(%q) = %q, want %q", key, got, want)
		}
	}
}
