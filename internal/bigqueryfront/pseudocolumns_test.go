package bigqueryfront

import "testing"

// TestNamedPseudoColumn (#1458): a query of the client names the engine's
// pseudo-columns as identifiers, in any case and quoted, but not in a
// string literal or comment.
func TestNamedPseudoColumn(t *testing.T) {
	for sql, want := range map[string]string{
		"SELECT _PARTITION_STREAMED FROM d.t":            "_PARTITION_STREAMED",
		"SELECT t._partition_buffered FROM d.t AS t":     "_partition_buffered",
		"SELECT `t._PARTITION_STREAMED` FROM d.t AS t":   "_PARTITION_STREAMED",
		"SELECT * FROM d.t WHERE `_PARTITION_BUFFERED`":  "_PARTITION_BUFFERED",
		"SELECT '_PARTITION_STREAMED' AS s":              "",
		"SELECT 1 -- _PARTITION_BUFFERED":                "",
		"SELECT _PARTITIONTIME, _PARTITIONDATE FROM d.t": "",
		"SELECT _PARTITION_STREAMED_X FROM d.t":          "",
	} {
		if got, _ := namedPseudoColumn(sql); got != want {
			t.Errorf("namedPseudoColumn(%q) = %q, want %q", sql, got, want)
		}
	}
	if _, pos := namedPseudoColumn("SELECT\n  _PARTITION_STREAMED"); lineCol("SELECT\n  _PARTITION_STREAMED", pos) != "[2:3]" {
		t.Errorf("position %d", pos)
	}
}
