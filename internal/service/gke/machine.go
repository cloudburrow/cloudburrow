package gke

import (
	"context"
	"regexp"
	"strconv"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// A node's machine (#2100): a pool's nodes get their machine type's vCPUs
// and memory, and the kubelet reserves what GKE reserves of them, so the
// scheduler sees the allocatable resources a GKE node of that type has.
// GKE's reservations, for node versions before 1.37.2-gke.1325000 (the
// served versions; re-read 2026-11-22:
// https://docs.cloud.google.com/kubernetes-engine/docs/concepts/plan-node-sizes):
//
//   - memory: "255 MiB for machines with less than 1 GiB of memory", else
//     25% of the first 4 GiB, 20% of the next 4 GiB, 10% of the next 8 GiB,
//     6% of the next 112 GiB and 2% of the rest; and "an extra 100 MiB is
//     reserved for Pod eviction thresholds";
//   - CPU: 6% of the first core, 1% of the next, 0.5% of the next two and
//     0.25% of every core above four; for shared-core E2 machine types
//     "GKE still reserves a total of 1060 millicores".

// NodeMachine is what a pool's nodes are given: the machine type's vCPUs
// and memory, and what the kubelet reserves of them (the eviction
// threshold apart).
type NodeMachine struct {
	MilliCPU, MemoryMiB                 int
	ReservedMilliCPU, ReservedMemoryMiB int
	// EvictionMiB is the memory eviction threshold.
	EvictionMiB int
}

// MachineConfigurer is what a Backend does for machine types.
type MachineConfigurer interface {
	// ConfigureMachine records the machine pool's nodes get from now on.
	ConfigureMachine(ctx context.Context, kind, pool string, m NodeMachine) error
}

// MachineShape is a predefined machine type's vCPUs and memory, and
// whether it is shared-core.
type MachineShape struct {
	CPUs, MemoryMiB int
	Shared          bool
}

// customMachineRE is a custom machine type: [SERIES-]custom-CPUS-MEMORY_MB,
// with -ext for extended memory.
var customMachineRE = regexp.MustCompile(`^(?:([a-z][a-z0-9]*)-)?custom-(\d+)-(\d+)(?:-ext)?$`)

// predefinedShapeRE is a predefined type outside the catalog:
// SERIES-standard|highmem|highcpu-CPUS.
var predefinedShapeRE = regexp.MustCompile(`^([a-z][a-z0-9]*)-(standard|highmem|highcpu)-(\d+)$`)

// machineOf is the machine a machine type names, false for one whose
// shape is unknown here.
// lookup is Compute Engine's catalog of predefined types (Options).
func machineOf(name string, lookup func(string) (MachineShape, bool)) (NodeMachine, bool) {
	cpus, memMiB, shared := 0, 0, false
	var m MachineShape
	ok := false
	if lookup != nil {
		m, ok = lookup(name)
	}
	if ok {
		cpus, memMiB, shared = m.CPUs, m.MemoryMiB, m.Shared
	} else if g := customMachineRE.FindStringSubmatch(name); g != nil {
		cpus, _ = strconv.Atoi(g[2])
		memMiB, _ = strconv.Atoi(g[3])
	} else if g := predefinedShapeRE.FindStringSubmatch(name); g != nil {
		cpus, _ = strconv.Atoi(g[3])
		// GiB per vCPU of the shape, as Compute Engine's general-purpose
		// series give them (N1's standard is 3.75 GB).
		perCPU := map[string]int{"standard": 4096, "highmem": 8192, "highcpu": 1024}[g[2]]
		if g[1] == "n1" {
			perCPU = map[string]int{"standard": 3840, "highmem": 6656, "highcpu": 922}[g[2]]
		}
		memMiB = cpus * perCPU
	} else {
		return NodeMachine{}, false
	}
	if cpus <= 0 || memMiB <= 0 {
		return NodeMachine{}, false
	}
	return NodeMachine{MilliCPU: cpus * 1000, MemoryMiB: memMiB,
		ReservedMilliCPU: reservedCPU(cpus*1000, shared), ReservedMemoryMiB: reservedMemory(memMiB), EvictionMiB: 100}, true
}

// reservedCPU is GKE's CPU reservation, in millicores, of a machine.
func reservedCPU(milli int, shared bool) int {
	if shared {
		return 1060
	}
	// Tiers in thousandths of a millicore: 6%, 1%, 0.5% twice, 0.25%.
	tiers := []struct{ upTo, permille int }{{1000, 60}, {2000, 10}, {4000, 5}, {1 << 30, 25}}
	total, prev := 0, 0
	for _, t := range tiers {
		if milli <= prev {
			break
		}
		n := min(milli, t.upTo) - prev
		if t.permille == 25 {
			total += n * 25 / 10000
		} else {
			total += n * t.permille / 1000
		}
		prev = t.upTo
	}
	return total
}

// reservedMemory is GKE's memory reservation, in MiB, of a machine.
func reservedMemory(mib int) int {
	if mib < 1024 {
		return 255
	}
	tiers := []struct {
		upTo int
		pct  float64
	}{{4 << 10, 0.25}, {8 << 10, 0.20}, {16 << 10, 0.10}, {128 << 10, 0.06}, {1 << 40, 0.02}}
	total, prev := 0.0, 0
	for _, t := range tiers {
		if mib <= prev {
			break
		}
		total += float64(min(mib, t.upTo)-prev) * t.pct
		prev = t.upTo
	}
	return int(total)
}

// configureMachine records pool's machine type for the nodes it gets.
func (s *Server) configureMachine(ctx context.Context, kind, pool, machineType string) error {
	mc, ok := s.backend.(MachineConfigurer)
	if !ok {
		return nil
	}
	m, ok := machineOf(machineType, s.opts.MachineType)
	if !ok {
		return nil
	}
	if err := mc.ConfigureMachine(ctx, kind, pool, m); err != nil {
		return status.Errorf(codes.Internal, "Setting node pool %q's machine type failed: %v", pool, err)
	}
	return nil
}
