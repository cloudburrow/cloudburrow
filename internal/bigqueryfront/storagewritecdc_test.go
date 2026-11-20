package bigqueryfront

import "testing"

// The ordering examples of
// https://docs.cloud.google.com/bigquery/docs/change-data-capture (#1999).
func TestChangeSequenceOrder(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{
		{"77", "7B", -1},
		{"FFF/B", "FFF/ABC", -1},
		{"BA/FFFFFFFF", "ABC", -1},
		{"a", "A", 0},
		{"0/0/0/0", "0/0/0/0", 0},
		{"FFFFFFFFFFFFFFFF/FFFFFFFFFFFFFFFF/FFFFFFFFFFFFFFFF/FFFFFFFFFFFFFFFF", "1", 1},
		{"1", "1/0", -1},
	} {
		a, err := parseChangeSeq(c.a)
		if err != nil {
			t.Fatal(err)
		}
		b, err := parseChangeSeq(c.b)
		if err != nil {
			t.Fatal(err)
		}
		if got := a.compare(b); got != c.want {
			t.Errorf("compare(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
	for _, bad := range []string{"", "XYZ", "1/2/3/4/5", "12345678901234567", "1//2", "/1", "-1"} {
		if _, err := parseChangeSeq(bad); err == nil {
			t.Errorf("parseChangeSeq(%q) is accepted", bad)
		}
	}
}
