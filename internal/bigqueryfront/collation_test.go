package bigqueryfront

import (
	"encoding/json"
	"testing"
)

// TestCollated (#1459): a new table takes its dataset's default collation
// unless it names one, and its top-level STRING columns the table's,
// unless they name one; since #1516 a RECORD's STRING fields and a
// REPEATED STRING take it too.
func TestCollated(t *testing.T) {
	for _, c := range []struct{ table, dataset, want string }{
		{`{"schema":{"fields":[{"name":"s","type":"STRING"},{"name":"n","type":"INTEGER"},{"name":"r","type":"RECORD","fields":[{"name":"x","type":"STRING"}]},{"name":"a","type":"STRING","mode":"REPEATED"},{"name":"k","type":"STRING","collation":"und:ci"}]}}`,
			"und:ci",
			`{"defaultCollation":"und:ci","schema":{"fields":[{"collation":"und:ci","name":"s","type":"STRING"},{"name":"n","type":"INTEGER"},{"fields":[{"collation":"und:ci","name":"x","type":"STRING"}],"name":"r","type":"RECORD"},{"collation":"und:ci","mode":"REPEATED","name":"a","type":"STRING"},{"collation":"und:ci","name":"k","type":"STRING"}]}}`},
		// The table names none (''): its columns stay as they are.
		{`{"defaultCollation":"","schema":{"fields":[{"name":"s","type":"STRING"}]}}`, "und:ci",
			`{"defaultCollation":"","schema":{"fields":[{"name":"s","type":"STRING"}]}}`},
		// No dataset collation: the table's own applies to its columns.
		{`{"defaultCollation":"und:ci","schema":{"fields":[{"name":"s","type":"STRING"}]}}`, "",
			`{"defaultCollation":"und:ci","schema":{"fields":[{"collation":"und:ci","name":"s","type":"STRING"}]}}`},
		{`{"schema":{"fields":[{"name":"s","type":"STRING"}]}}`, "", `{"schema":{"fields":[{"name":"s","type":"STRING"}]}}`},
	} {
		var tb map[string]any
		if err := json.Unmarshal([]byte(c.table), &tb); err != nil {
			t.Fatal(err)
		}
		collated(tb, c.dataset)
		if got, _ := json.Marshal(tb); string(got) != c.want {
			t.Errorf("%s in %q:\n got %s\nwant %s", c.table, c.dataset, got, c.want)
		}
	}
}
