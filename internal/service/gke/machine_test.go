package gke

import "testing"

// GKE's reservations (concepts/plan-node-sizes): a 4-vCPU node keeps
// 3920m allocatable, as the page's kubectl sample shows; shared-core E2
// types reserve 1060m; memory follows the tiers.
func TestMachineReservations(t *testing.T) {
	if got := reservedCPU(4000, false); got != 80 {
		t.Errorf("4 vCPUs reserve %dm, want 80m", got)
	}
	if got := reservedCPU(1000, false); got != 60 {
		t.Errorf("1 vCPU reserves %dm, want 60m", got)
	}
	if got := reservedCPU(8000, false); got != 90 {
		t.Errorf("8 vCPUs reserve %dm, want 90m", got)
	}
	if got := reservedCPU(2000, true); got != 1060 {
		t.Errorf("a shared-core machine reserves %dm, want 1060m", got)
	}
	for mib, want := range map[int]int{512: 255, 4096: 1024, 8192: 1843, 16384: 2662, 32768: 3645} {
		if got := reservedMemory(mib); got != want {
			t.Errorf("%d MiB reserves %d MiB, want %d", mib, got, want)
		}
	}
	lookup := func(name string) (MachineShape, bool) {
		if name == "e2-medium" {
			return MachineShape{CPUs: 2, MemoryMiB: 4096, Shared: true}, true
		}
		return MachineShape{}, false
	}
	for name, want := range map[string]NodeMachine{
		"e2-medium":         {2000, 4096, 1060, 1024, 100},
		"e2-custom-4-8192":  {4000, 8192, 80, 1843, 100},
		"n2-highmem-2":      {2000, 16384, 70, 2662, 100},
		"n1-standard-1":     {1000, 3840, 60, 960, 100},
		"custom-2-4096-ext": {2000, 4096, 70, 1024, 100},
	} {
		got, ok := machineOf(name, lookup)
		if !ok || got != want {
			t.Errorf("machineOf(%q) = %+v, %v; want %+v", name, got, ok, want)
		}
	}
	if _, ok := machineOf("a3-megagpu-8g", lookup); ok {
		t.Error("an unknown shape has no machine")
	}
}
