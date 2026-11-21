package main

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// TestGKEWorkloadsListDeletesTheChecked (#2070): every Workloads row can
// be checked, the list's Delete takes the checked ones and is confirmed
// with a click, as Google's "select one or more Deployments to delete.
// Click Delete. When prompted to confirm, click Delete."; a Kubernetes
// system object among them refuses the whole selection with the reason,
// before anything is deleted.
func TestGKEWorkloadsListDeletesTheChecked(t *testing.T) {
	ctx := context.Background()
	f := useFakeKube(t, gkeWorkloadFixture)
	p := gkeWorkloads("/kc", "cloudburrow-t", "cloudburrow")

	l, err := p.List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range l.Items {
		if !r.Selectable {
			t.Errorf("row %s cannot be checked", r.Name)
		}
	}
	sel := p.ListSelectActions(ctx, "")
	if len(sel) != 1 || sel[0].ID != gkeDeleteSelected || sel[0].Label != "Delete" || !sel[0].Destructive ||
		sel[0].ConfirmStyle != console.DeleteConfirmClick || sel[0].SelectionField != "workloads" {
		t.Fatalf("list selection actions = %+v", sel)
	}
	if len(sel[0].Fields) != 1 || sel[0].Fields[0].Type != "hidden" {
		t.Errorf("the list's Delete opens a form: %+v", sel[0].Fields)
	}
	// The row's and the page's Delete are confirmed the same way, by a click.
	for _, a := range p.DetailActions(ctx, "", []string{"web"}) {
		if a.ID == "delete" && (a.ConfirmStyle != console.DeleteConfirmClick || a.Confirm != "" || !a.Leaves) {
			t.Errorf("web's Delete = %+v", a)
		}
	}

	// Each checked workload is deleted in its namespace, as its kind.
	n := len(f.calls)
	if err := p.ActAt(ctx, "", nil, gkeDeleteSelected, map[string]string{"workloads": "web\nagent\n\nweb"}); err != nil {
		t.Fatal(err)
	}
	var deletes []string
	for _, c := range f.calls[n:] {
		if strings.Contains(c, " delete ") {
			deletes = append(deletes, c[strings.Index(c, "delete "):])
		}
	}
	if got := strings.Join(deletes, "; "); got != "delete deployment/web -n default; delete daemonset/agent -n default" {
		t.Errorf("deleted %q", got)
	}

	// A system object checked: refused with the reason, nothing deleted.
	n = len(f.calls)
	err = p.ActAt(ctx, "", nil, gkeDeleteSelected, map[string]string{"workloads": "web\ncoredns"})
	if err == nil || !strings.Contains(err.Error(), "kube-system holds Kubernetes system objects") || !strings.Contains(err.Error(), "nothing was deleted") {
		t.Errorf("a selection with a system object = %v", err)
	}
	for _, c := range f.calls[n:] {
		if strings.Contains(c, " delete ") {
			t.Errorf("ran %q after a refused selection", c)
		}
	}
	if err := p.ActAt(ctx, "", nil, gkeDeleteSelected, map[string]string{"workloads": ""}); err == nil {
		t.Error("an empty selection was accepted")
	}
	if err := p.ActAt(ctx, "", nil, gkeDeleteSelected, map[string]string{"workloads": "missing"}); err == nil {
		t.Error("a selection of no workload was accepted")
	}
}

func gkeTestPod(ns, name string, cpuNS uint64, mem, disk int64, at string) console.PodMetrics {
	return console.PodMetrics{Namespace: ns, Name: name, CPUCoreNanoSeconds: cpuNS, MemoryWorkingSetBytes: mem,
		EphemeralStorageUsedBytes: disk, At: at}
}

func gkeTestValues(s console.ChartSeries) string {
	var out []string
	for _, p := range s.Points {
		if p.Value == nil {
			out = append(out, "-")
			continue
		}
		out = append(out, strconv.FormatFloat(*p.Value, 'f', -1, 64))
	}
	return strings.Join(out, ",")
}

// TestGKEWorkloadOverviewUtilization (#2070): a workload's Overview tab
// charts CPU, Memory and Disk, each the sum of its managed pods' kubelet
// readings (CPU the rate between counters); a sample missing a pod's
// reading is a gap, never a partial sum; a metric no pod reports, or an
// instance with no readings, says so in the chart.
func TestGKEWorkloadOverviewUtilization(t *testing.T) {
	t0 := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	clock := t0
	series := console.NewSeries(10, func() time.Time { return clock })
	add := func(sec int, pods ...console.PodMetrics) {
		clock = t0.Add(time.Duration(sec) * time.Second)
		series.Add(console.Metrics{Pods: pods})
	}
	ts := func(sec int) string { return t0.Add(time.Duration(sec) * time.Second).Format(time.RFC3339) }
	add(0, gkeTestPod("default", "web-a", 1e9, 100, 10, ts(0)), gkeTestPod("default", "web-b", 2e9, 200, 20, ts(0)),
		gkeTestPod("default", "other", 9e9, 900, 90, ts(0)))
	// 10s on: web-a used 0.5 cores, web-b 0.25.
	add(10, gkeTestPod("default", "web-a", 6e9, 110, 11, ts(10)), gkeTestPod("default", "web-b", 4.5e9, 210, 21, ts(10)))
	// web-b has no memory reading: the memory sum is a gap.
	add(20, gkeTestPod("default", "web-a", 11e9, 120, 12, ts(20)), gkeTestPod("default", "web-b", 7e9, 0, 22, ts(20)))

	got := gkeUtilization(series, []string{"default/web-a", "default/web-b"})
	if len(got) != 3 || got[0].Label != "CPU" || got[1].Label != "Memory" || got[2].Label != "Disk" {
		t.Fatalf("series %+v", got)
	}
	for i, want := range []string{"-,0.75,0.75", "300,320,-", "30,32,34"} {
		if v := gkeTestValues(got[i]); v != want {
			t.Errorf("%s = %s, want %s", got[i].Label, v, want)
		}
		if got[i].Unavailable != "" {
			t.Errorf("%s unavailable: %s", got[i].Label, got[i].Unavailable)
		}
	}

	// No ephemeral storage reported: Disk says so rather than drawing zeros.
	bare := console.NewSeries(10, func() time.Time { return clock })
	bare.Add(console.Metrics{Pods: []console.PodMetrics{gkeTestPod("default", "web-a", 1e9, 100, 0, ts(0))}})
	if d := gkeUtilization(bare, []string{"default/web-a"})[2]; !strings.Contains(d.Unavailable, "no ephemeral storage") || len(d.Points) != 0 {
		t.Errorf("Disk without readings = %+v", d)
	}
	for _, c := range []struct {
		series *console.Series
		pods   []string
		want   string
	}{{nil, []string{"default/web-a"}, "keeps no kubelet readings"}, {series, nil, "no managed pods"}} {
		for _, s := range gkeUtilization(c.series, c.pods) {
			if !strings.Contains(s.Unavailable, c.want) {
				t.Errorf("%s = %q, want %q", s.Label, s.Unavailable, c.want)
			}
		}
	}

	// The charts lead the Overview tab, before Containers and Managed pods.
	p := gkeWorkloads("", "cloudburrow-t", "cloudburrow").withSeries(series)
	d := p.withUtilization(gkeWorkload{ns: "default", name: "web"}, gkeWorkloadTabs(console.Detail{Sections: []console.Section{
		{ID: "containers", Label: "Containers"},
		{ID: "pods", Label: "Managed pods", Listing: console.Listing{Items: []console.Resource{{Name: "web-a"}, {Name: "web-b"}}}},
		{ID: "object", Label: "YAML"},
	}}, nil))
	var tabs []string
	for _, s := range d.Sections {
		tabs = append(tabs, s.Group+"/"+s.Label)
	}
	if want := "Overview/Utilization,Overview/Containers,Overview/Managed pods"; !strings.HasPrefix(strings.Join(tabs, ","), want) {
		t.Errorf("tabs %v, want %s first", tabs, want)
	}
	if d.Sections[0].Kind != console.KindChart || gkeTestValues(d.Sections[0].Series[0]) != "-,0.75,0.75" {
		t.Errorf("Utilization %+v", d.Sections[0])
	}
}

// TestKubeletSummaryReadsEphemeralStorage (#2070): the pod's
// "ephemeral-storage" usedBytes, which a workload's Disk chart sums.
func TestKubeletSummaryReadsEphemeralStorage(t *testing.T) {
	var s kubeletSummary
	raw := `{"pods":[{"podRef":{"name":"web-a","namespace":"default"},"ephemeral-storage":{"usedBytes":4096}}]}`
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		t.Fatal(err)
	}
	if len(s.Pods) != 1 || s.Pods[0].EphemeralStorage.UsedBytes != 4096 {
		t.Errorf("decoded %+v", s.Pods)
	}
}

// TestGKEObjectBrowserAPIGroupTree (#2070): Object Browser's filter is
// Google's API group and Resource Kind tree, core first, each group's
// kinds once and sorted, on the list in place of the shared filter bar.
func TestGKEObjectBrowserAPIGroupTree(t *testing.T) {
	tree := gkeAPIGroupTree([]console.Resource{
		{Name: "deployments.apps", Fields: map[string]string{"API group": "apps", "Kind": "Deployment"}},
		{Name: "daemonsets.apps", Fields: map[string]string{"API group": "apps", "Kind": "DaemonSet"}},
		{Name: "pods", Fields: map[string]string{"API group": "core", "Kind": "Pod"}},
		{Name: "configmaps", Fields: map[string]string{"API group": "core", "Kind": "ConfigMap"}},
		{Name: "jobs.batch", Fields: map[string]string{"API group": "batch", "Kind": "Job"}},
	})
	if tree.Label != "API groups" || tree.Column != "API group" || tree.ChildColumn != "Kind" {
		t.Errorf("tree %+v", tree)
	}
	var got []string
	for _, g := range tree.Groups {
		got = append(got, g.Label+":"+strings.Join(g.Children, "+"))
	}
	if want := "core:ConfigMap+Pod,apps:DaemonSet+Deployment,batch:Job"; strings.Join(got, ",") != want {
		t.Errorf("groups %s, want %s", strings.Join(got, ","), want)
	}

	useFakeKube(t, "NAME          SHORTNAMES   APIVERSION   NAMESPACED   KIND         VERBS\n"+
		"pods          po           v1           true         Pod          list\n"+
		"deployments   deploy       apps/v1      true         Deployment   list\n")
	l, err := gkeObjectBrowser{kubeconfig: "/kc"}.List(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if l.CheckTree == nil || len(l.CheckTree.Groups) != 2 || l.CheckTree.Groups[1].Label != "apps" {
		t.Errorf("Object Browser's tree %+v", l.CheckTree)
	}
}
