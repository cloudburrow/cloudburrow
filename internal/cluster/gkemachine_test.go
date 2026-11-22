package cluster

import (
	"reflect"
	"testing"
)

// A node's kubelet reserves what lies beyond its machine type, plus GKE's
// own reservation, and the pool's own settings win over the machine's.
func TestMachineKubelet(t *testing.T) {
	m := &NodeMachine{MilliCPU: 2000, MemoryMiB: 4096, ReservedMilliCPU: 1060, ReservedMemoryMiB: 1024, EvictionMiB: 100}
	got := machineKubelet(m, 8000, 16384)
	want := map[string]any{
		"systemReserved": map[string]any{"cpu": "7060m", "memory": "13312Mi"},
		"evictionHard":   map[string]any{"memory.available": "100Mi"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("machineKubelet = %v, want %v", got, want)
	}
	if f := m.dockerLimits(); !reflect.DeepEqual(f, []string{"--cpus", "2", "--memory", "4096m", "--memory-swap", "4096m"}) {
		t.Errorf("dockerLimits = %v", f)
	}
	merged := mergeKubelet(map[string]any{"evictionHard": map[string]any{"memory.available": "200Mi"}, "podPidsLimit": 1024}, got)
	if eh := merged["evictionHard"].(map[string]any)["memory.available"]; eh != "200Mi" {
		t.Errorf("the pool's eviction threshold is %v, want its own 200Mi", eh)
	}
	if merged["systemReserved"] == nil || merged["podPidsLimit"] != 1024 {
		t.Errorf("merged = %v", merged)
	}
	// A machine larger than the host keeps most of the host.
	big := machineKubelet(&NodeMachine{MilliCPU: 32000, MemoryMiB: 131072, ReservedMilliCPU: 150, ReservedMemoryMiB: 9000, EvictionMiB: 100}, 4000, 8192)
	if r := big["systemReserved"].(map[string]any); r["cpu"] != "150m" || r["memory"] != "7836Mi" {
		t.Errorf("a machine larger than the host reserves %v", r)
	}
}

// A pool's Static memory manager keeps its reservedMemory equal to the
// reservations and eviction threshold once the machine's are added.
func TestMachineKubeletStaticMemoryManager(t *testing.T) {
	pool := map[string]any{"memoryManagerPolicy": "Static", "kubeReserved": map[string]any{"memory": "100Mi"},
		"evictionHard":   map[string]any{"memory.available": "100Mi"},
		"reservedMemory": []any{map[string]any{"numaNode": 0, "limits": map[string]any{"memory": "200Mi"}}}}
	got := mergeKubelet(pool, map[string]any{"systemReserved": map[string]any{"cpu": "500m", "memory": "1000Mi"},
		"evictionHard": map[string]any{"memory.available": "100Mi"}})
	rm := got["reservedMemory"].([]any)[0].(map[string]any)["limits"].(map[string]any)["memory"]
	if rm != "1200Mi" {
		t.Errorf("reservedMemory = %v, want 1200Mi", rm)
	}
	if pool["reservedMemory"].([]any)[0].(map[string]any)["limits"].(map[string]any)["memory"] != "200Mi" {
		t.Error("the pool's settings were changed")
	}
}
