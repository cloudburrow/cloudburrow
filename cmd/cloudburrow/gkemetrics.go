package main

// Kubernetes Engine system metrics in Cloud Monitoring (#2100).
//
// GKE writes its clusters' system metrics, kubernetes.io/*, to Cloud
// Monitoring while system metrics are enabled (re-read 2026-11-21:
// https://docs.cloud.google.com/monitoring/api/metrics_kubernetes). The
// gkeMetricsSampler does that for the served clusters: once a minute it
// reads every node's kubelet summary (/stats/summary, through the API
// server's node proxy) and records the readings it holds, on the
// k8s_container, k8s_pod and k8s_node resources, into Cloud Monitoring's
// kubernetes.io built-in metrics. A cluster whose monitoring is none is
// read no longer.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/lifecycle"
	"github.com/cloudburrow/cloudburrow/internal/service/gke"
	"github.com/cloudburrow/cloudburrow/internal/service/monitoring"
)

// gkeMetricsInterval is how often the summaries are read:
// CLOUDBURROW_GKE_METRICS_INTERVAL (a Go duration, for tests), else 60
// seconds, as GKE samples them.
func gkeMetricsInterval() time.Duration {
	if d, err := time.ParseDuration(os.Getenv("CLOUDBURROW_GKE_METRICS_INTERVAL")); err == nil && d >= time.Second {
		return d
	}
	return 60 * time.Second
}

// gkeSummary is what the sampler reads of a kubelet's /stats/summary.
type gkeSummary struct {
	Node struct {
		NodeName  string          `json:"nodeName"`
		StartTime time.Time       `json:"startTime"`
		CPU       *summaryCPU     `json:"cpu"`
		Memory    *summaryMem     `json:"memory"`
		Network   *summaryNet     `json:"network"`
		Fs        *summaryFs      `json:"fs"`
		Runtime   *summaryRuntime `json:"runtime"`
	} `json:"node"`
	Pods []struct {
		PodRef struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		} `json:"podRef"`
		StartTime  time.Time `json:"startTime"`
		Containers []struct {
			Name      string      `json:"name"`
			StartTime time.Time   `json:"startTime"`
			CPU       *summaryCPU `json:"cpu"`
			Memory    *summaryMem `json:"memory"`
			Rootfs    *summaryFs  `json:"rootfs"`
			Logs      *summaryFs  `json:"logs"`
		} `json:"containers"`
		Network          *summaryNet `json:"network"`
		EphemeralStorage *summaryFs  `json:"ephemeral-storage"`
	} `json:"pods"`
}

type summaryCPU struct {
	Time                 time.Time `json:"time"`
	UsageCoreNanoSeconds *uint64   `json:"usageCoreNanoSeconds"`
}

type summaryMem struct {
	Time            time.Time `json:"time"`
	UsageBytes      *uint64   `json:"usageBytes"`
	WorkingSetBytes *uint64   `json:"workingSetBytes"`
}

type summaryIface struct {
	Name    string  `json:"name"`
	RxBytes *uint64 `json:"rxBytes"`
	TxBytes *uint64 `json:"txBytes"`
}

type summaryNet struct {
	Time time.Time `json:"time"`
	summaryIface
	Interfaces []summaryIface `json:"interfaces"`
}

type summaryFs struct {
	Time           time.Time `json:"time"`
	AvailableBytes *uint64   `json:"availableBytes"`
	CapacityBytes  *uint64   `json:"capacityBytes"`
	UsedBytes      *uint64   `json:"usedBytes"`
}

type summaryRuntime struct {
	ImageFs *summaryFs `json:"imageFs"`
}

func orTime(t, def time.Time) time.Time {
	if t.IsZero() {
		return def
	}
	return t
}

// memoryPoints are GKE's memory/used_bytes by memory_type: the working set
// is what cannot be evicted, the rest of the usage what can.
func memoryPoints(metric string, res map[string]string, m *summaryMem, now time.Time) []monitoring.GKEPoint {
	if m == nil || m.WorkingSetBytes == nil {
		return nil
	}
	at := orTime(m.Time, now)
	ws := int64(*m.WorkingSetBytes)
	out := []monitoring.GKEPoint{{Metric: metric, MetricLabels: map[string]string{"memory_type": "non-evictable"}, Resource: res, End: at, Int: ws}}
	if m.UsageBytes != nil {
		ev := int64(*m.UsageBytes) - ws
		if ev < 0 {
			ev = 0
		}
		out = append(out, monitoring.GKEPoint{Metric: metric, MetricLabels: map[string]string{"memory_type": "evictable"}, Resource: res, End: at, Int: ev})
	}
	return out
}

// summaryPoints are the readings of one node's summary, for target t;
// allocatable is the node's allocatable ephemeral storage, or -1.
func summaryPoints(t gke.TelemetryTarget, s gkeSummary, allocatable int64, now time.Time) []monitoring.GKEPoint {
	var out []monitoring.GKEPoint
	base := func() map[string]string {
		return map[string]string{"project_id": t.Project, "location": t.Location, "cluster_name": t.Cluster}
	}
	n := s.Node
	node := base()
	node["node_name"] = n.NodeName
	if c := n.CPU; c != nil && c.UsageCoreNanoSeconds != nil {
		out = append(out, monitoring.GKEPoint{Metric: "kubernetes.io/node/cpu/core_usage_time", Resource: node,
			Start: n.StartTime, End: orTime(c.Time, now), Double: float64(*c.UsageCoreNanoSeconds) / 1e9})
	}
	out = append(out, memoryPoints("kubernetes.io/node/memory/used_bytes", node, n.Memory, now)...)
	if nw := n.Network; nw != nil {
		var rx, tx uint64
		ifaces := nw.Interfaces
		if len(ifaces) == 0 {
			ifaces = []summaryIface{nw.summaryIface}
		}
		have := false
		for _, i := range ifaces {
			if i.RxBytes != nil && i.TxBytes != nil {
				rx, tx, have = rx+*i.RxBytes, tx+*i.TxBytes, true
			}
		}
		if have {
			at := orTime(nw.Time, now)
			out = append(out,
				monitoring.GKEPoint{Metric: "kubernetes.io/node/network/received_bytes_count", Resource: node, Start: n.StartTime, End: at, Int: int64(rx)},
				monitoring.GKEPoint{Metric: "kubernetes.io/node/network/sent_bytes_count", Resource: node, Start: n.StartTime, End: at, Int: int64(tx)})
		}
	}
	if fs := n.Fs; fs != nil {
		at := orTime(fs.Time, now)
		if fs.UsedBytes != nil {
			out = append(out, monitoring.GKEPoint{Metric: "kubernetes.io/node/ephemeral_storage/used_bytes", Resource: node, End: at, Int: int64(*fs.UsedBytes)})
		}
		if fs.CapacityBytes != nil {
			out = append(out, monitoring.GKEPoint{Metric: "kubernetes.io/node/ephemeral_storage/total_bytes", Resource: node, End: at, Int: int64(*fs.CapacityBytes)})
		}
		if allocatable >= 0 {
			out = append(out, monitoring.GKEPoint{Metric: "kubernetes.io/node/ephemeral_storage/allocatable_bytes", Resource: node, End: at, Int: allocatable})
		}
	}
	for _, p := range s.Pods {
		pod := base()
		pod["namespace_name"], pod["pod_name"] = p.PodRef.Namespace, p.PodRef.Name
		if nw := p.Network; nw != nil {
			ifaces := nw.Interfaces
			if len(ifaces) == 0 && nw.Name != "" {
				ifaces = []summaryIface{nw.summaryIface}
			}
			at := orTime(nw.Time, now)
			for _, i := range ifaces {
				if i.RxBytes == nil || i.TxBytes == nil {
					continue
				}
				l := map[string]string{"interface": i.Name}
				out = append(out,
					monitoring.GKEPoint{Metric: "kubernetes.io/pod/network/received_bytes_count", MetricLabels: l, Resource: pod, Start: p.StartTime, End: at, Int: int64(*i.RxBytes)},
					monitoring.GKEPoint{Metric: "kubernetes.io/pod/network/sent_bytes_count", MetricLabels: map[string]string{"interface": i.Name}, Resource: pod, Start: p.StartTime, End: at, Int: int64(*i.TxBytes)})
			}
		}
		if es := p.EphemeralStorage; es != nil && es.UsedBytes != nil {
			out = append(out, monitoring.GKEPoint{Metric: "kubernetes.io/pod/ephemeral_storage/used_bytes", Resource: pod, End: orTime(es.Time, now), Int: int64(*es.UsedBytes)})
		}
		for _, c := range p.Containers {
			ctr := base()
			ctr["namespace_name"], ctr["pod_name"], ctr["container_name"] = p.PodRef.Namespace, p.PodRef.Name, c.Name
			if cpu := c.CPU; cpu != nil && cpu.UsageCoreNanoSeconds != nil {
				out = append(out, monitoring.GKEPoint{Metric: "kubernetes.io/container/cpu/core_usage_time", Resource: ctr,
					Start: c.StartTime, End: orTime(cpu.Time, now), Double: float64(*cpu.UsageCoreNanoSeconds) / 1e9})
			}
			out = append(out, memoryPoints("kubernetes.io/container/memory/used_bytes", ctr, c.Memory, now)...)
			// A container's ephemeral storage is its writable layer and
			// its logs, as the kubelet counts it.
			var used uint64
			have := false
			at := now
			for _, fs := range []*summaryFs{c.Rootfs, c.Logs} {
				if fs != nil && fs.UsedBytes != nil {
					used, have, at = used+*fs.UsedBytes, true, orTime(fs.Time, now)
				}
			}
			if have {
				out = append(out, monitoring.GKEPoint{Metric: "kubernetes.io/container/ephemeral_storage/used_bytes", Resource: ctr, End: at, Int: int64(used)})
			}
		}
	}
	return out
}

// parseQuantity reads a Kubernetes quantity of bytes ("102400Ki", "5G",
// "1048576"); -1 when it cannot.
func parseQuantity(q string) int64 {
	mult := map[string]float64{"Ki": 1 << 10, "Mi": 1 << 20, "Gi": 1 << 30, "Ti": 1 << 40, "Pi": 1 << 50,
		"k": 1e3, "K": 1e3, "M": 1e6, "G": 1e9, "T": 1e12, "P": 1e15}
	for _, suf := range []string{"Ki", "Mi", "Gi", "Ti", "Pi", "k", "K", "M", "G", "T", "P"} {
		if strings.HasSuffix(q, suf) {
			v, err := strconv.ParseFloat(strings.TrimSuffix(q, suf), 64)
			if err != nil {
				return -1
			}
			return int64(v * mult[suf])
		}
	}
	v, err := strconv.ParseFloat(q, 64)
	if err != nil {
		return -1
	}
	return int64(v)
}

// nodeAllocatable reads `kubectl get nodes -o json`: each node's
// allocatable ephemeral storage.
func nodeAllocatable(raw []byte) (map[string]int64, error) {
	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Status struct {
				Allocatable map[string]string `json:"allocatable"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, err
	}
	out := map[string]int64{}
	for _, n := range list.Items {
		out[n.Metadata.Name] = -1
		if q, ok := n.Status.Allocatable["ephemeral-storage"]; ok {
			out[n.Metadata.Name] = parseQuantity(q)
		}
	}
	return out, nil
}

// gkeMetricsSampler records the served clusters' system metrics.
type gkeMetricsSampler struct {
	targets func() []gke.TelemetryTarget
	// kubectl runs kubectl against a kind cluster's kubeconfig.
	kubectl func(ctx context.Context, kind string, args ...string) (string, error)
	builtin *monitoring.GKEBuiltin
	every   time.Duration

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

func newGKEMetricsSampler(dir string, gkeSvc *gkeService, mon *monitoringService) *gkeMetricsSampler {
	if gkeSvc == nil || mon == nil {
		return nil
	}
	return &gkeMetricsSampler{
		targets: func() []gke.TelemetryTarget {
			if api := gkeSvc.API(); api != nil {
				return api.Telemetry()
			}
			return nil
		},
		kubectl: gkeKubectl(dir),
		builtin: mon.gkeMetrics(),
		every:   gkeMetricsInterval(),
	}
}

// gkeKubectl runs kubectl with a Kubernetes Engine cluster's kubeconfig,
// kept in dir/<kind>/kubeconfig (internal/cluster's GKE.Dir).
func gkeKubectl(dir string) func(ctx context.Context, kind string, args ...string) (string, error) {
	return func(ctx context.Context, kind string, args ...string) (string, error) {
		return kubeRunner(filepath.Join(dir, kind, "kubeconfig"), "").Do(ctx, "", args...)
	}
}

func (m *gkeMetricsSampler) Name() string { return "gke-metrics" }

func (m *gkeMetricsSampler) register(coord *lifecycle.Coordinator) {
	if m != nil {
		coord.Register(m)
	}
}

func (m *gkeMetricsSampler) Start(ctx context.Context) error {
	if m == nil {
		return nil
	}
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	done := make(chan struct{})
	m.mu.Lock()
	m.cancel, m.done = cancel, done
	m.mu.Unlock()
	go func() {
		defer close(done)
		t := time.NewTicker(m.every)
		defer t.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-t.C:
				m.sample(runCtx)
			}
		}
	}()
	return nil
}

func (m *gkeMetricsSampler) Stop(ctx context.Context) error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	cancel, done := m.cancel, m.done
	m.cancel = nil
	m.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	select {
	case <-done:
	case <-ctx.Done():
	}
	return nil
}

// sample reads every node summary of the clusters whose system metrics
// are on.
func (m *gkeMetricsSampler) sample(ctx context.Context) {
	for _, t := range m.targets() {
		if !t.SystemMetrics {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		raw, err := m.kubectl(cctx, t.Kind, "get", "nodes", "-o", "json")
		if err != nil {
			cancel()
			continue
		}
		alloc, err := nodeAllocatable([]byte(raw))
		if err != nil {
			cancel()
			continue
		}
		for node, a := range alloc {
			out, err := m.kubectl(cctx, t.Kind, "get", "--raw", "/api/v1/nodes/"+node+"/proxy/stats/summary")
			if err != nil {
				continue
			}
			var s gkeSummary
			if json.Unmarshal([]byte(out), &s) != nil {
				continue
			}
			if s.Node.NodeName == "" {
				s.Node.NodeName = node
			}
			m.builtin.Record(summaryPoints(t, s, a, time.Now())...)
		}
		cancel()
	}
}
