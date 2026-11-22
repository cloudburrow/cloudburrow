package cluster

import (
	"context"
	"fmt"
	"maps"
	"strconv"
	"strings"
)

// NodeMachine is a pool's machine type's resources (#2100): its vCPUs and
// memory, which the node container is limited to, and what GKE reserves
// of them, which the kubelet keeps from pods.
type NodeMachine struct {
	MilliCPU, MemoryMiB                 int
	ReservedMilliCPU, ReservedMemoryMiB int
	EvictionMiB                         int
}

// dockerLimits are the docker run flags that give a node m's resources.
func (m *NodeMachine) dockerLimits() []string {
	if m == nil {
		return nil
	}
	return []string{"--cpus", strconv.FormatFloat(float64(m.MilliCPU)/1000, 'f', -1, 64),
		"--memory", strconv.Itoa(m.MemoryMiB) + "m", "--memory-swap", strconv.Itoa(m.MemoryMiB) + "m"}
}

// machineKubelet is the kubelet configuration that makes a node's
// allocatable m's: the kubelet counts the host's CPUs and memory as the
// node's capacity (cAdvisor reads /proc, which a container limit does not
// change), so what lies beyond the machine is reserved with GKE's own
// reservation. hostMilli and hostMiB are the capacity the kubelet sees.
func machineKubelet(m *NodeMachine, hostMilli, hostMiB int) map[string]any {
	if m == nil {
		return nil
	}
	cpu := max(hostMilli-m.MilliCPU, 0) + m.ReservedMilliCPU
	mem := max(hostMiB-m.MemoryMiB, 0) + m.ReservedMemoryMiB
	// The reservation never takes everything: a machine larger than the
	// host keeps the host's own capacity, less GKE's share of it.
	cpu = min(cpu, max(hostMilli-100, 0))
	mem = min(mem, max(hostMiB-m.EvictionMiB-256, 0))
	return map[string]any{
		"systemReserved": map[string]any{"cpu": strconv.Itoa(cpu) + "m", "memory": strconv.Itoa(mem) + "Mi"},
		"evictionHard":   map[string]any{"memory.available": strconv.Itoa(m.EvictionMiB) + "Mi"},
	}
}

// mergeKubelet is the pool's kubelet settings with the machine's
// reservations; the pool's own evictionHard keys stay.
func mergeKubelet(pool, machine map[string]any) map[string]any {
	if len(machine) == 0 {
		return pool
	}
	out := maps.Clone(pool)
	if out == nil {
		out = map[string]any{}
	}
	for k, v := range machine {
		if have, ok := out[k].(map[string]any); ok {
			merged := maps.Clone(v.(map[string]any))
			maps.Copy(merged, have)
			out[k] = merged
			continue
		}
		out[k] = v
	}
	return out
}

// hostCapacity is the CPUs (millicores) and memory (MiB) the kubelet of
// node counts as its capacity.
func (g *GKE) hostCapacity(ctx context.Context, node string) (int, int, error) {
	out, err := g.runner().Run(ctx, "docker", "exec", node, "sh", "-c",
		`nproc --all; awk '/^MemTotal:/ {print $2}' /proc/meminfo`)
	if err != nil {
		return 0, 0, err
	}
	f := strings.Fields(out)
	if len(f) != 2 {
		return 0, 0, fmt.Errorf("read node %s's capacity: %q", node, out)
	}
	cpus, err1 := strconv.Atoi(f[0])
	kib, err2 := strconv.Atoi(f[1])
	if err1 != nil || err2 != nil {
		return 0, 0, fmt.Errorf("read node %s's capacity: %q", node, out)
	}
	return cpus * 1000, kib / 1024, nil
}
