package main

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

// The decoder keeps what the kubelet actually sent.
//
// It used to keep three numbers — node CPU, node memory, and the LENGTH of
// the pods array — and discard the rest of a response already fetched and
// paid for. The fixture is a real capture from this project's kind node.
func TestKubeletSummaryKeepsWhatItWasSent(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("testdata/kubelet-summary.json")
	if err != nil {
		t.Fatal(err)
	}
	var s kubeletSummary
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if s.Node.CPU.UsageCoreNanoSeconds == 0 {
		t.Error("the node's cumulative counter was dropped; without it there is no rate")
	}
	if s.Node.CPU.Time == "" {
		t.Error("the kubelet's own timestamp was dropped; a rate over the host " +
			"clock is wrong by however long the call took")
	}
	if s.Node.Network.RxBytes == 0 && s.Node.Network.TxBytes == 0 {
		t.Error("node network counters were dropped")
	}
	if s.Node.FS.CapacityBytes == 0 {
		t.Error("node filesystem capacity was dropped")
	}

	if len(s.Pods) == 0 {
		t.Fatal("no pods decoded")
	}
	for _, p := range s.Pods {
		if p.PodRef.Namespace == "" || p.PodRef.Name == "" {
			t.Error("a pod reading cannot be joined without its namespace and name")
		}
		if p.CPU.UsageNanoCores == 0 && p.CPU.UsageCoreNanoSeconds == 0 {
			t.Errorf("%s reports no CPU at all", p.PodRef.Name)
		}
		if p.Memory.WorkingSetBytes == 0 {
			t.Errorf("%s reports no memory", p.PodRef.Name)
		}
	}
}

// A rate is an average over an interval, and there are intervals it cannot
// be taken across.
func TestCPURateRefusesWhatItCannotMeasure(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)

	t.Run("two normal samples", func(t *testing.T) {
		// One core-second of CPU over ten seconds is 0.1 cores.
		cores, ok := cpuRate(1_000_000_000, 2_000_000_000, base, base.Add(10*time.Second))
		if !ok {
			t.Fatal("a normal interval was refused")
		}
		if cores < 0.0999 || cores > 0.1001 {
			t.Errorf("cores = %v, want 0.1", cores)
		}
	})

	t.Run("a counter reset breaks the series", func(t *testing.T) {
		// The container restarted. There is no rate across that boundary, and
		// inventing one draws a spike that never happened.
		if _, ok := cpuRate(9_000_000_000, 1_000_000_000, base, base.Add(10*time.Second)); ok {
			t.Error("a decreasing counter produced a rate")
		}
	})

	t.Run("a zero-length interval", func(t *testing.T) {
		if _, ok := cpuRate(1, 2, base, base); ok {
			t.Error("dividing by no time produced a rate")
		}
	})

	t.Run("time going backwards", func(t *testing.T) {
		if _, ok := cpuRate(1, 2, base.Add(10*time.Second), base); ok {
			t.Error("a negative interval produced a rate")
		}
	})

	t.Run("no predecessor", func(t *testing.T) {
		if _, ok := cpuRate(0, 5_000_000_000, time.Time{}, base); ok {
			t.Error("the first sample produced a rate with nothing to compare against")
		}
	})
}
