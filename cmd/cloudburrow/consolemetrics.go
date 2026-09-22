package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/identity-wael/cloudburrow/internal/console"
)

// clusterMetrics reads real utilisation from the cluster.
//
// The source is the kubelet's own /stats/summary, reached through the API
// server's node proxy. That matters: metrics-server is not installed in a kind
// cluster, so `kubectl top` answers "Metrics API not available" — and a
// dashboard showing CPU would otherwise have to estimate one, which is the
// kind of number that looks authoritative and is not.
//
// Capacity comes from the node object. Both halves are therefore the cluster's
// own figures.
func clusterMetrics(kubeconfig string) console.MetricsSource {
	return func(ctx context.Context) console.Metrics {
		m := console.Metrics{Collected: time.Now().UTC().Format(time.RFC3339)}

		raw, err := kubectlJSON(ctx, kubeconfig, "", "nodes")
		if err != nil {
			m.Unavailable = "cannot read nodes: " + err.Error()
			return m
		}
		var nodes struct {
			Items []map[string]any `json:"items"`
		}
		if err := json.Unmarshal(raw, &nodes); err != nil {
			m.Unavailable = "cannot decode nodes: " + err.Error()
			return m
		}

		for _, item := range nodes.Items {
			meta := nested(item, "metadata")
			name := str(meta, "name")
			if name == "" {
				continue
			}
			status := nested(item, "status")
			capacity := nested(status, "capacity")

			n := console.NodeMetrics{
				Name:             name,
				CPUCapacityCores: parseCPUQuantity(str(capacity, "cpu")),
				MemoryTotalBytes: parseMemoryQuantity(str(capacity, "memory")),
				Ready:            nodeReady(status),
			}
			// Usage is best-effort: a node whose kubelet will not answer still
			// appears, with its capacity and no usage, rather than vanishing
			// from a panel that claims to show the cluster.
			if summary, err := nodeUsage(ctx, kubeconfig, name); err == nil {
				n.CPUUsedCores = summary.Node.CPU.UsageNanoCores / 1e9
				n.CPUCoreNanoSeconds = summary.Node.CPU.UsageCoreNanoSeconds
				n.MemoryUsedBytes = summary.Node.Memory.WorkingSetBytes
				n.At = summary.Node.CPU.Time
				n.Pods = len(summary.Pods)
				n.NetworkRxBytes = summary.Node.Network.RxBytes
				n.NetworkTxBytes = summary.Node.Network.TxBytes
				n.FilesystemUsedBytes = summary.Node.FS.UsedBytes
				n.FilesystemCapacityBytes = summary.Node.FS.CapacityBytes

				for _, pod := range summary.Pods {
					if pod.PodRef.Name == "" {
						continue
					}
					m.Pods = append(m.Pods, console.PodMetrics{
						Namespace:             pod.PodRef.Namespace,
						Name:                  pod.PodRef.Name,
						CPUUsedCores:          pod.CPU.UsageNanoCores / 1e9,
						CPUCoreNanoSeconds:    pod.CPU.UsageCoreNanoSeconds,
						MemoryWorkingSetBytes: pod.Memory.WorkingSetBytes,
						At:                    pod.CPU.Time,
					})
				}
			}
			m.Nodes = append(m.Nodes, n)
		}

		if len(m.Nodes) == 0 {
			m.Unavailable = "the cluster reported no nodes"
		}
		return m
	}
}

// kubeletSummary is the shape of /stats/summary that this console reads.
//
// The decoder used to keep three numbers — node CPU, node memory, and the
// LENGTH of the pods array — and discard everything else in a response that
// had already been fetched and paid for. Every per-pod and per-container
// reading was in there, unread: 23 of 23 pods on this node report both, with
// the cumulative counter and the kubelet's own clock.
type kubeletSummary struct {
	Node struct {
		CPU    kubeletCPU    `json:"cpu"`
		Memory kubeletMemory `json:"memory"`
		// Node-scoped, and named as such wherever they are shown: a node's
		// network and filesystem counters are not any pod's.
		Network struct {
			RxBytes int64 `json:"rxBytes"`
			TxBytes int64 `json:"txBytes"`
		} `json:"network"`
		FS struct {
			UsedBytes     int64 `json:"usedBytes"`
			CapacityBytes int64 `json:"capacityBytes"`
		} `json:"fs"`
	} `json:"node"`
	Pods []struct {
		PodRef struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		} `json:"podRef"`
		CPU    kubeletCPU    `json:"cpu"`
		Memory kubeletMemory `json:"memory"`
	} `json:"pods"`
}

type kubeletCPU struct {
	// Time is the kubelet's own timestamp. A rate divided by the interval
	// between two host clock readings is wrong by however long the call took.
	Time                 string  `json:"time"`
	UsageNanoCores       float64 `json:"usageNanoCores"`
	UsageCoreNanoSeconds uint64  `json:"usageCoreNanoSeconds"`
}

type kubeletMemory struct {
	Time            string `json:"time"`
	WorkingSetBytes int64  `json:"workingSetBytes"`
}

// nodeUsage reads one node's kubelet summary and keeps all of it.
func nodeUsage(ctx context.Context, kubeconfig, node string) (kubeletSummary, error) {
	path := fmt.Sprintf("/api/v1/nodes/%s/proxy/stats/summary", node)
	out, err := kubectlRaw(ctx, kubeconfig, path)
	if err != nil {
		return kubeletSummary{}, err
	}
	var summary kubeletSummary
	if err := json.Unmarshal(out, &summary); err != nil {
		return kubeletSummary{}, fmt.Errorf("decode kubelet summary: %w", err)
	}
	return summary, nil
}

// cpuRate is the average cores used between two cumulative readings.
//
// The instantaneous usageNanoCores is a sample of whatever the process was
// doing at the moment the kubelet looked. A counter difference over the
// interval is what actually happened in between, which is the only number a
// chart point can honestly claim.
//
// A counter that went backwards means the container restarted; there is no
// rate across that boundary, and inventing one would draw a spike that never
// occurred. ok is false rather than the value being clamped.
func cpuRate(prevCounter, counter uint64, prev, now time.Time) (cores float64, ok bool) {
	if prev.IsZero() || now.IsZero() || !now.After(prev) {
		return 0, false
	}
	if counter < prevCounter {
		return 0, false
	}
	elapsed := now.Sub(prev).Seconds()
	if elapsed <= 0 {
		return 0, false
	}
	return float64(counter-prevCounter) / 1e9 / elapsed, true
}

func nodeReady(status map[string]any) bool {
	conds, ok := status["conditions"].([]any)
	if !ok {
		return false
	}
	for _, c := range conds {
		cond, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if str(cond, "type") == "Ready" {
			return str(cond, "status") == "True"
		}
	}
	return false
}

// parseCPUQuantity reads Kubernetes CPU quantities: whole cores, or millicores
// with an "m" suffix.
func parseCPUQuantity(v string) float64 {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if strings.HasSuffix(v, "m") {
		n, err := strconv.ParseFloat(strings.TrimSuffix(v, "m"), 64)
		if err != nil {
			return 0
		}
		return n / 1000
	}
	n, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0
	}
	return n
}

// parseMemoryQuantity reads Kubernetes memory quantities.
//
// Both binary (Ki, Mi, Gi) and decimal (K, M, G) suffixes are handled, because
// the API uses whichever the node reported and mixing them up misreports
// memory by 7% at gigabyte scale.
func parseMemoryQuantity(v string) int64 {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	multipliers := []struct {
		suffix string
		mult   int64
	}{
		{"Ki", 1 << 10}, {"Mi", 1 << 20}, {"Gi", 1 << 30}, {"Ti", 1 << 40},
		{"K", 1000}, {"M", 1000 * 1000}, {"G", 1000 * 1000 * 1000}, {"T", 1e12},
	}
	for _, m := range multipliers {
		if strings.HasSuffix(v, m.suffix) {
			n, err := strconv.ParseFloat(strings.TrimSuffix(v, m.suffix), 64)
			if err != nil {
				return 0
			}
			return int64(n * float64(m.mult))
		}
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// kubectlRaw fetches an API path directly.
//
// The node proxy endpoint has no typed `kubectl get` equivalent, so this is
// the supported way to reach it. Same binary, same kubeconfig, same
// permissions as every other read the console makes.
func kubectlRaw(ctx context.Context, kubeconfig, path string) ([]byte, error) {
	if kubeconfig == "" {
		return nil, fmt.Errorf("no kubeconfig: the cluster has not started")
	}
	out, err := exec.CommandContext(ctx, "kubectl",
		"--kubeconfig", kubeconfig, "get", "--raw", path).Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return nil, fmt.Errorf("kubectl get --raw %s: %s", path, strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, fmt.Errorf("kubectl get --raw %s: %w", path, err)
	}
	return out, nil
}
