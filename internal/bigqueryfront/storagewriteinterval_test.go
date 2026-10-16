package bigqueryfront

import (
	"math"
	"testing"
)

// TestStorageWriteIntervalStrings: a string written to an INTERVAL column,
// in the canonical format or as an INTERVAL literal, is written as its
// canonical text; one that is neither, or out of range, fails.
func TestStorageWriteIntervalStrings(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"1-2 3 4:5:6", "1-2 3 4:5:6"},
		{"1-2 3 4:5:6.789", "1-2 3 4:5:6.789"},
		{"-1-2 -3 -4:5:6.5", "-1-2 -3 -4:5:6.5"},
		{"+0-0 0 +0:0:1", "0-0 0 0:0:1"},
		{"0-0 0 0:0:0.0000019", "0-0 0 0:0:0.000001"},
		{"0-0 0 100:0:0", "0-0 0 100:0:0"},
		{"10000-0 3660000 87840000:0:0", "10000-0 3660000 87840000:0:0"},
		{" 1-0 0 0:0:0 ", "1-0 0 0:0:0"},
		{"INTERVAL 5 DAY", "0-0 5 0:0:0"},
		{"interval -25 month", "-2-1 0 0:0:0"},
		{"INTERVAL 2 QUARTER", "0-6 0 0:0:0"},
		{"INTERVAL 2 WEEK", "0-0 14 0:0:0"},
		{"INTERVAL 90 MINUTE", "0-0 0 1:30:0"},
		{"INTERVAL -1 SECOND", "0-0 0 -0:0:1"},
		{"INTERVAL 1500 MILLISECOND", "0-0 0 0:0:1.5"},
		{"INTERVAL 7 MICROSECOND", "0-0 0 0:0:0.000007"},
		{"INTERVAL '10:20:30.52' HOUR TO SECOND", "0-0 0 10:20:30.52"},
		{"INTERVAL '1-2' YEAR TO MONTH", "1-2 0 0:0:0"},
		{"INTERVAL '-1-2' YEAR TO MONTH", "-1-2 0 0:0:0"},
		{"INTERVAL '1 5:30' DAY TO MINUTE", "0-0 1 5:30:0"},
		{"INTERVAL '1-2 3 4:5:6' YEAR TO SECOND", "1-2 3 4:5:6"},
		{"INTERVAL '-90:30' MINUTE TO SECOND", "0-0 0 -1:30:30"},
	} {
		got, err := writeIntervalText(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("%q: %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
	for _, in := range []string{
		"", "abc", "1-2", "1-2 3", "1-2 3 4:5", "1-2 3 4:5:6x", "1-12 0 0:0:0", "0-0 0 0:60:0", "0-0 0 0:0:60",
		"1-2 3 4:5:6.1234567890", "10000-1 0 0:0:0", "0-0 3660001 0:0:0", "0-0 0 87840001:0:0", "0-0 0 87840000:0:1",
		"P1Y", "INTERVAL 5 FORTNIGHT", "INTERVAL 1.5 DAY", "INTERVAL 10001 YEAR", "INTERVAL 9223372036854775807 HOUR",
		"INTERVAL '1-2' MONTH TO YEAR", "INTERVAL '1-12' YEAR TO MONTH", "INTERVAL '1:60' HOUR TO MINUTE", "INTERVAL '1 2' DAY TO MINUTE",
		"1-2 3 4:5:-6", "99999999999999999999-0 0 0:0:0",
	} {
		if got, err := writeIntervalText(in); err == nil {
			t.Errorf("%q: %q, want an error", in, got)
		}
	}
}

// TestStorageWriteIntervalDurations: a google.protobuf.Duration is the
// INTERVAL's time part, below a microsecond cut; a Duration that is not
// valid, or out of INTERVAL's range, fails.
func TestStorageWriteIntervalDurations(t *testing.T) {
	for _, tc := range []struct {
		secs, nanos int64
		want        string
	}{
		{0, 0, "0-0 0 0:0:0"},
		{90061, 500_000_000, "0-0 0 25:1:1.5"},
		{-90061, -500_000_000, "0-0 0 -25:1:1.5"},
		{0, -1000, "0-0 0 -0:0:0.000001"},
		{0, 999, "0-0 0 0:0:0"},
		{315_576_000_000, 0, "0-0 0 87660000:0:0"},
	} {
		got, err := durationIntervalText(tc.secs, tc.nanos)
		if err != nil || got != tc.want {
			t.Errorf("Duration{%d, %d}: %q, %v; want %q", tc.secs, tc.nanos, got, err, tc.want)
		}
	}
	for _, d := range [][2]int64{{1, -1}, {-1, 1}, {0, 1e9}, {0, -1e9}, {316_224_000_001, 0}, {math.MinInt64, 0}, {math.MaxInt64, 0}} {
		if got, err := durationIntervalText(d[0], d[1]); err == nil {
			t.Errorf("Duration{%d, %d}: %q, want an error", d[0], d[1], got)
		}
	}
}
