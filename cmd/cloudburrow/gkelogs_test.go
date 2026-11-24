package main

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/logagent"
	"github.com/cloudburrow/cloudburrow/internal/service/gke"
)

// fakeLogCluster records what the reconciler does to one cluster.
type fakeLogCluster struct {
	applied   []string
	kubectl   [][]string
	configMap string
	nodes     map[string]string
	has       map[string]bool
	loads     []string
}

func (f *fakeLogCluster) Apply(_ context.Context, _, m string) error {
	f.applied = append(f.applied, m)
	if strings.Contains(m, "kind: ConfigMap") {
		f.configMap = m
	}
	return nil
}

func (f *fakeLogCluster) Kubectl(_ context.Context, _ string, args ...string) (string, error) {
	f.kubectl = append(f.kubectl, args)
	return "", nil
}

func (f *fakeLogCluster) Nodes(context.Context, string) (map[string]string, error) {
	return f.nodes, nil
}

func (f *fakeLogCluster) Image(context.Context, string) (string, error) {
	return logagentimage0, nil
}

const logagentimage0 = "dev.local/cloudburrow-logging-agent:0123456789abcdef"

func (f *fakeLogCluster) Load(_ context.Context, _, node, _ string) (bool, error) {
	if f.has[node] {
		return false, nil
	}
	f.has[node] = true
	f.loads = append(f.loads, node)
	return true, nil
}

func (f *fakeLogCluster) daemonSets() int {
	n := 0
	for _, m := range f.applied {
		if strings.Contains(m, "kind: DaemonSet") {
			n++
		}
	}
	return n
}

func (f *fakeLogCluster) deleted(what string) int {
	n := 0
	for _, a := range f.kubectl {
		if len(a) > 1 && a[0] == "delete" && a[1] == what {
			n++
		}
	}
	return n
}

// TestGKELogAgentsFollowTheClustersLogging (#2100): while a cluster's
// logs are on, its fluentbit-gke DaemonSet runs the embedded agent image,
// loaded into every node (a node added later too, whose waiting pod is
// made again), writing to the instance's Cloud Logging; its ConfigMap
// names what is shipped. With logging none the ConfigMap says since when
// it is off and the DaemonSet is removed; back on, it ships from the
// change that turned it on, so a pod's first line, written just after,
// is not lost. Nothing is applied again while nothing changes.
func TestGKELogAgentsFollowTheClustersLogging(t *testing.T) {
	t0 := time.Date(2026, 11, 22, 13, 0, 0, 0, time.UTC)
	target := gke.TelemetryTarget{Name: "projects/p/locations/us-central1-a/clusters/c1", Project: "p", Location: "us-central1-a",
		Cluster: "c1", Kind: "k", SystemLogs: true, WorkloadLogs: true, ControlPlaneLogs: map[string]bool{}}
	fc := &fakeLogCluster{nodes: map[string]string{"k-control-plane": "id0", "k-worker": "id1"}, has: map[string]bool{"k-control-plane": true}}
	now := t0
	f := &gkeLogAgents{
		targets:  func() []gke.TelemetryTarget { return []gke.TelemetryTarget{target} },
		endpoint: func() string { return "http://192.168.65.254:28420" },
		cluster:  fc, now: func() time.Time { return now }, state: map[string]*gkeLogState{},
	}
	ctx := context.Background()
	f.sweep(ctx)
	if fc.daemonSets() != 1 || !strings.Contains(fc.applied[len(fc.applied)-1], "image: "+logagentimage0) ||
		!strings.Contains(fc.applied[len(fc.applied)-1], `value: "http://192.168.65.254:28420"`) {
		t.Fatalf("applied = %q", fc.applied)
	}
	if len(fc.loads) != 1 || fc.loads[0] != "k-worker" {
		t.Errorf("loaded into %v, want the worker, which lacked the image", fc.loads)
	}
	cfg := configOf(t, fc.configMap)
	if !cfg.Ships(logagent.Workload, t0.Add(-time.Hour)) || cfg.Ships("apiserver", t0) || cfg.Cluster != "c1" {
		t.Errorf("config = %+v", cfg)
	}
	n := len(fc.applied)
	f.sweep(ctx)
	if len(fc.applied) != n {
		t.Errorf("an unchanged sweep applied %q", fc.applied[n:])
	}

	// A node added: loaded, and its pod made again.
	fc.nodes["k-worker2"] = "id2"
	f.sweep(ctx)
	if len(fc.loads) != 2 || fc.deleted("pod") != 2 {
		t.Errorf("a new node: loads %v, pod deletions %d", fc.loads, fc.deleted("pod"))
	}

	// Logging none.
	off := t0.Add(time.Minute)
	target.SystemLogs, target.WorkloadLogs, target.LoggingChanged = false, false, off
	now = off.Add(time.Second)
	f.sweep(ctx)
	cfg = configOf(t, fc.configMap)
	if cfg.Any() || !cfg.Ships(logagent.Workload, off) || cfg.Ships(logagent.Workload, off.Add(time.Millisecond)) {
		t.Errorf("with none: %+v", cfg)
	}
	if fc.deleted("daemonset") != 1 {
		t.Errorf("the DaemonSet is not removed with logging none: %v", fc.kubectl)
	}
	f.sweep(ctx)
	if fc.deleted("daemonset") != 1 {
		t.Errorf("removed again: %v", fc.kubectl)
	}

	// Back on: from the change, a little before this sweep.
	on := t0.Add(2 * time.Minute)
	target.SystemLogs, target.WorkloadLogs, target.LoggingChanged = true, true, on
	now = on.Add(2 * time.Second)
	f.sweep(ctx)
	cfg = configOf(t, fc.configMap)
	if !cfg.Ships(logagent.Workload, on.Add(115*time.Millisecond)) || cfg.Ships(logagent.Workload, on.Add(-time.Second)) {
		t.Errorf("back on: %+v", cfg)
	}
	if fc.daemonSets() != 2 {
		t.Errorf("the DaemonSet is not applied again: %d", fc.daemonSets())
	}

	// A new process reads the configuration back, and keeps its times.
	fc.configMap = ""
	g := &gkeLogAgents{targets: f.targets, endpoint: f.endpoint, now: func() time.Time { return now.Add(time.Hour) },
		cluster: &readBack{fakeLogCluster: fc, value: cfg.Marshal()}, state: map[string]*gkeLogState{}}
	n = len(fc.applied)
	g.sweep(ctx)
	for _, m := range fc.applied[n:] {
		if strings.Contains(m, "kind: ConfigMap") {
			t.Errorf("a restarted CLI rewrote the configuration: %s", m)
		}
	}
}

// readBack answers the ConfigMap's read with value.
type readBack struct {
	*fakeLogCluster
	value string
}

func (r *readBack) Kubectl(ctx context.Context, kind string, args ...string) (string, error) {
	if len(args) > 1 && args[0] == "get" && args[1] == "configmap" {
		return r.value, nil
	}
	return r.fakeLogCluster.Kubectl(ctx, kind, args...)
}

func configOf(t *testing.T, manifest string) logagent.Config {
	t.Helper()
	i := strings.Index(manifest, logagent.ConfigKey+": ")
	if i < 0 {
		t.Fatalf("no configuration in %q", manifest)
	}
	v := strings.TrimSpace(manifest[i+len(logagent.ConfigKey)+2:])
	s, err := strconv.Unquote(v)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := logagent.ParseConfig(s)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}
