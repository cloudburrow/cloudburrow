package main

import (
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

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

// TestPodCPURateRefusesWhatIsNotARate.
//
// A rate needs two readings and an interval. Each case here would otherwise
// produce a number, and every one of those numbers would be wrong in a way that
// looks plausible on a chart.
func TestPodCPURateRefusesWhatIsNotARate(t *testing.T) {
	at := func(seconds int) string {
		return time.Date(2026, 9, 22, 12, 0, seconds, 0, time.UTC).Format(time.RFC3339)
	}
	reading := func(seconds int, counter uint64) *console.PodMetrics {
		return &console.PodMetrics{At: at(seconds), CPUCoreNanoSeconds: counter}
	}

	// Two good readings five seconds apart, one core of work: one core.
	if got := podCPURate(reading(0, 1e9), reading(5, 6e9)); got == nil || *got != 1 {
		t.Fatalf("rate = %v, want 1 core", got)
	}

	for name, c := range map[string]struct{ prev, now *console.PodMetrics }{
		// The first reading has no interval behind it.
		"no previous": {nil, reading(5, 6e9)},
		// A sample the pod was absent from, which must not be read as zero use.
		"absent previous": {reading(0, 0), reading(5, 6e9)},
		"absent current":  {reading(0, 1e9), reading(5, 0)},
		// A container restart resets the counter.
		"counter reset": {reading(0, 6e9), reading(5, 1e9)},
		// Two readings with the same timestamp divide by zero.
		"no interval": {reading(5, 1e9), reading(5, 6e9)},
		// Time going backwards is not an interval either.
		"backwards": {reading(5, 1e9), reading(0, 6e9)},
	} {
		if got := podCPURate(c.prev, c.now); got != nil {
			t.Errorf("%s: rate = %v, want no rate at all", name, *got)
		}
	}
}

// TestPodLimitsAreZeroWhenAnyContainerHasNone.
//
// A ceiling summed from two of three containers is a ceiling the pod does not
// have, and a chart scaled to it would show headroom that does not exist.
func TestPodLimitsAreZeroWhenAnyContainerHasNone(t *testing.T) {
	both := podFixture(t, `{"spec":{"containers":[
		{"resources":{"limits":{"cpu":"500m","memory":"256Mi"}}},
		{"resources":{"limits":{"cpu":"1","memory":"256Mi"}}}]}}`)
	if got := podLimitCores(both); got != 1.5 {
		t.Errorf("cores = %v, want 1.5", got)
	}
	if got := podLimitBytes(both); got != 512<<20 {
		t.Errorf("bytes = %d, want %d", got, 512<<20)
	}

	partial := podFixture(t, `{"spec":{"containers":[
		{"resources":{"limits":{"cpu":"500m","memory":"256Mi"}}},
		{"resources":{}}]}}`)
	if got := podLimitCores(partial); got != 0 {
		t.Errorf("cores = %v, want 0 when a container has no limit", got)
	}
	if got := podLimitBytes(partial); got != 0 {
		t.Errorf("bytes = %d, want 0 when a container has no limit", got)
	}
}
