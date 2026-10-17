package bigqueryfront

import "testing"

// TestLogicalBytesExpr: each type's documented size, NULL as 0, a
// repeated column the sum of its elements, a RECORD the sum of its fields.
func TestLogicalBytesExpr(t *testing.T) {
	got := logicalBytesExpr([]field{
		{Name: "n", Type: "INTEGER"},
		{Name: "s", Type: "STRING"},
		{Name: "a", Type: "INT64", Mode: "REPEATED"},
		{Name: "r", Type: "RECORD", Fields: []field{{Name: "b", Type: "BOOL"}, {Name: "g", Type: "GEOGRAPHY"}}},
	})
	want := "IF(`n` IS NULL, 0, 8) + IF(`s` IS NULL, 0, 2 + BYTE_LENGTH(`s`)) + " +
		"IFNULL((SELECT SUM(IF(e_ IS NULL, 0, 8)) FROM UNNEST(`a`) AS e_), 0) + " +
		"IF(`r` IS NULL, 0, IF(`r`.`b` IS NULL, 0, 1) + IF(`r`.`g` IS NULL, 0, 16 + 24 * ST_NUMPOINTS(`r`.`g`)))"
	if got != want {
		t.Errorf("logicalBytesExpr:\n got %s\nwant %s", got, want)
	}
	if got := logicalBytesExpr(nil); got != "0" {
		t.Errorf("logicalBytesExpr(nil) = %s, want 0", got)
	}
}
