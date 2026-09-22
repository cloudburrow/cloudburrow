package main

import "testing"

func TestParseCPUQuantity(t *testing.T) {
	cases := map[string]float64{"16": 16, "500m": 0.5, "1": 1, "2500m": 2.5, "": 0, "bogus": 0}
	for in, want := range cases {
		if got := parseCPUQuantity(in); got != want {
			t.Errorf("parseCPUQuantity(%q) = %v, want %v", in, got, want)
		}
	}
}

// Binary and decimal suffixes differ by 7% at gigabyte scale, which is enough
// to misreport a machine's memory by gigabytes.
func TestParseMemoryQuantity(t *testing.T) {
	cases := map[string]int64{
		"41002188Ki": 41002188 * 1024,
		"1Gi":        1 << 30,
		"1G":         1000 * 1000 * 1000,
		"1Mi":        1 << 20,
		"1024":       1024,
		"":           0,
		"bogus":      0,
	}
	for in, want := range cases {
		if got := parseMemoryQuantity(in); got != want {
			t.Errorf("parseMemoryQuantity(%q) = %d, want %d", in, got, want)
		}
	}
	if parseMemoryQuantity("1Gi") == parseMemoryQuantity("1G") {
		t.Error("binary and decimal suffixes must not resolve to the same value")
	}
}
