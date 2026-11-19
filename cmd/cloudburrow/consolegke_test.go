package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// gkeKubeconfig is a kubeconfig file naming a local API server, for the
// cluster page's endpoint.
func gkeKubeconfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte("apiVersion: v1\nclusters:\n- cluster:\n    server: https://127.0.0.1:20243\n  name: kind-cloudburrow-t\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestGKEWorkloadsHideSystemObjectsByDefault: Google's Workloads page
// filters Kubernetes system objects out by default, lists top-level
// workloads only (a Job a CronJob made is not one), and carries the
// Cluster column; "Kubernetes system objects: Shown" lists them.
func TestGKEWorkloadsHideSystemObjectsByDefault(t *testing.T) {
	dir := t.TempDir()
	saved := kubeFixtures["deployments,statefulsets,daemonsets,replicasets,jobs,cronjobs"]
	t.Cleanup(func() { kubeFixtures["deployments,statefulsets,daemonsets,replicasets,jobs,cronjobs"] = saved })
	kubeFixtures["deployments,statefulsets,daemonsets,replicasets,jobs,cronjobs"] = `{"items":[
	  {"kind":"Deployment","metadata":{"name":"web","namespace":"default"},"spec":{"replicas":2},"status":{"readyReplicas":2}},
	  {"kind":"Deployment","metadata":{"name":"coredns","namespace":"kube-system"},"spec":{"replicas":1},"status":{"readyReplicas":1}},
	  {"kind":"Deployment","metadata":{"name":"storage","namespace":"cloudburrow"},"spec":{"replicas":1},"status":{"readyReplicas":1}},
	  {"kind":"CronJob","metadata":{"name":"nightly","namespace":"default"},"status":{}},
	  {"kind":"Job","metadata":{"name":"nightly-1","namespace":"default","ownerReferences":[{"kind":"CronJob","controller":true}]},"status":{"succeeded":1}}]}`
	fakeKubectl(t, dir)
	p := gkeWorkloads(filepath.Join(dir, "kubeconfig"), "cloudburrow-t", "cloudburrow")

	names := func(l console.Listing) string {
		var out []string
		for _, r := range l.Items {
			out = append(out, r.Name+":"+r.Fields["Type"]+":"+r.Fields["Cluster"])
		}
		return strings.Join(out, ",")
	}
	l, err := p.List(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if got := names(l); got != "web:Deployment:cloudburrow-t,nightly:Cron Job:cloudburrow-t" {
		t.Errorf("default rows %q, want only the user's top-level workloads", got)
	}
	if strings.Join(l.Columns, ",") != "Type,Pods,Namespace,Cluster" {
		t.Errorf("columns %v", l.Columns)
	}
	if len(l.Filters) != 1 || l.Filters[0].Key != gkeSystemFilter || l.Filters[0].Selected != "hidden" {
		t.Errorf("filters %+v, want Kubernetes system objects hidden", l.Filters)
	}
	shown, err := p.List(console.WithScopeValue(context.Background(), gkeSystemFilter, "shown"), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(shown.Items) != 4 {
		t.Errorf("with system objects shown: %q, want 4 rows", names(shown))
	}
}

// TestGKEWorkloadTabsFollowGoogle: a workload's page is Overview
// (containers and managed pods), Details, Observability, Revision
// history, Events, Logs, YAML.
func TestGKEWorkloadTabsFollowGoogle(t *testing.T) {
	d := gkeWorkloadTabs(console.Detail{Sections: []console.Section{
		{ID: "containers", Label: "Containers"}, {ID: "configuration", Label: "Configuration"},
		{ID: "events", Label: "Events"}, {ID: "pods", Label: "Managed pods"},
		{ID: "revisions", Label: "Revision history"}, {ID: "object", Label: "YAML"},
	}}, nil)
	var tabs []string
	for _, s := range d.Sections {
		tab := s.Label
		if s.Group != "" {
			tab = s.Group + "/" + s.Label
		}
		tabs = append(tabs, tab)
	}
	want := "Overview/Containers,Overview/Managed pods,Details,Observability,Revision history,Events,Logs,YAML"
	if got := strings.Join(tabs, ","); got != want {
		t.Errorf("tabs %s, want %s", got, want)
	}
}

// TestGKEDeployRefusesBeforeKubectl: Deploy checks the name, the image,
// the namespace and the replica count as the API would, and never deploys
// into a system namespace.
func TestGKEDeployRefusesBeforeKubectl(t *testing.T) {
	p := gkeWorkloads("", "cloudburrow-t", "cloudburrow")
	for _, v := range []map[string]string{
		{"name": "Web", "image": "nginx"},
		{"name": "web", "image": ""},
		{"name": "web", "image": "nginx", "namespace": "kube-system"},
		{"name": "web", "image": "nginx", "replicas": "-1"},
		{"name": "web", "image": "nginx", "cluster": "other"},
		{"name": "web", "image": "nginx"}, // no kubeconfig
	} {
		if _, err := p.Create(context.Background(), "", v); err == nil {
			t.Errorf("Deploy %v succeeded, want a refusal", v)
		}
	}
	label, fields := p.CreateForm()
	if label != "Deploy" || len(fields) == 0 || fields[0].Label != "Image path" {
		t.Errorf("Deploy form %q %+v", label, fields)
	}
	if label, _ := gkeServices("", "c", "").CreateForm(); label != "" {
		t.Errorf("Services has a create form %q; Google's Services tab creates through Expose", label)
	}
}

// TestGKEClusterPage: the cluster list is this instance's one cluster with
// Google's columns; its page has Details, Nodes, Storage, Observability
// and Logs, Connect gives the kubectl commands for the instance's
// kubeconfig, and Edit, Upgrade, Delete and Create cluster are refused
// with the reason.
func TestGKEClusterPage(t *testing.T) {
	dir := t.TempDir()
	saved := kubeFixtures["nodes"]
	t.Cleanup(func() { kubeFixtures["nodes"] = saved })
	kubeFixtures["nodes"] = `{"items":[{"kind":"Node","metadata":{"name":"cloudburrow-t-control-plane","creationTimestamp":"2026-01-01T00:00:00Z",
	  "labels":{"node-role.kubernetes.io/control-plane":""}},
	  "status":{"capacity":{"cpu":"8","memory":"16Gi"},"allocatable":{"cpu":"8","memory":"16Gi","ephemeral-storage":"100Gi"},
	  "nodeInfo":{"kubeletVersion":"v1.33.1"},"conditions":[{"type":"Ready","status":"True"}]}}]}`
	fakeKubectl(t, dir)
	kc := gkeKubeconfig(t)
	p := gkeClustersProvider{name: "cloudburrow-t", kubeconfig: kc}
	ctx := context.Background()

	l, err := p.List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Items) != 1 || l.Items[0].Status != "Running" || l.Items[0].Fields["Total vCPUs"] != "8" ||
		l.Items[0].Fields["Total memory"] != "16.00 GB" || l.Items[0].Fields["Number of nodes"] != "1" {
		t.Errorf("clusters %+v", l.Items)
	}
	d, err := p.Detail(ctx, "", []string{"cloudburrow-t"})
	if err != nil || d.Unavailable != "" {
		t.Fatalf("detail: %v %q", err, d.Unavailable)
	}
	var tabs []string
	for _, s := range d.Sections {
		tabs = append(tabs, s.Label)
	}
	if got := strings.Join(tabs, ","); got != "Details,Nodes,Storage,Observability,Logs" {
		t.Errorf("tabs %s", got)
	}
	if n := d.Sections[1].Listing.Items; len(n) != 1 || n[0].Link != "/kubernetes/nodes/cloudburrow-t-control-plane" {
		t.Errorf("Nodes tab %+v, want the node linking to its page", n)
	}
	acts := p.DetailActions(ctx, "", []string{"cloudburrow-t"})
	byID := map[string]console.Action{}
	for _, a := range acts {
		byID[a.ID] = a
	}
	if c := byID["connect"]; len(c.Fields) != 1 || !strings.Contains(c.Fields[0].Default, "KUBECONFIG="+kc) ||
		!strings.Contains(c.Fields[0].Help, "https://127.0.0.1:20243") {
		t.Errorf("Connect %+v", c)
	}
	for _, id := range []string{"edit", "upgrade", "delete"} {
		if byID[id].Unavailable == "" {
			t.Errorf("%s is offered as performable", id)
		}
		if err := p.ActAt(ctx, "", []string{"cloudburrow-t"}, id, nil); err == nil || !strings.Contains(err.Error(), "Not available locally") {
			t.Errorf("%s: %v, want it refused", id, err)
		}
	}
	if err := p.ActAt(ctx, "", []string{"cloudburrow-t"}, "connect", nil); err != nil {
		t.Errorf("Connect: %v", err)
	}
	if _, err := p.Create(ctx, "", map[string]string{"mode": "Standard"}); err == nil || !strings.Contains(err.Error(), "#1913") {
		t.Errorf("Create cluster: %v, want it refused naming #1913", err)
	}
	if d, _ := p.Detail(ctx, "", []string{"other"}); d.Unavailable == "" {
		t.Error("a cluster this instance does not have opened")
	}
}

// TestGKESecretsShowKeysNotValues: Secrets & ConfigMaps lists both kinds by
// Type, and a Secret's page names its keys and never its values.
func TestGKESecretsShowKeysNotValues(t *testing.T) {
	dir := t.TempDir()
	fakeKubectl(t, dir)
	p := gkeConfig(filepath.Join(dir, "kubeconfig"), "c", "")
	ctx := context.Background()
	l, err := p.List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Items) != 2 || l.Items[0].Fields["Type"] != "Secret: Opaque" || l.Items[1].Fields["Type"] != "Config Map" {
		t.Errorf("rows %+v", l.Items)
	}
	d, err := p.Detail(ctx, "", []string{"creds"})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range d.Sections {
		if strings.Contains(s.Text, "c2VjcmV0") {
			t.Errorf("tab %s shows the secret's value", s.Label)
		}
		for _, r := range s.Listing.Items {
			if strings.Contains(r.Fields["Value"], "c2VjcmV0") {
				t.Errorf("tab %s shows the secret's value", s.Label)
			}
		}
	}
	if d.Sections[0].Label != "Details" || d.Sections[0].Listing.Items[0].Name != "password" {
		t.Errorf("Details %+v", d.Sections[0])
	}
}

// TestGKEObjectBrowserParsesAPIResources reads `kubectl api-resources -o
// wide`, whose SHORTNAMES column may be empty.
func TestGKEObjectBrowserParsesAPIResources(t *testing.T) {
	out := "NAME          SHORTNAMES   APIVERSION   NAMESPACED   KIND         VERBS\n" +
		"pods          po           v1           true         Pod          create,get,list\n" +
		"bindings                   v1           true         Binding      create\n" +
		"deployments   deploy       apps/v1      true         Deployment   list\n"
	got := parseAPIResources(out)
	if len(got) != 3 || got[0].qualified() != "pods" || got[0].kind != "Pod" ||
		got[2].qualified() != "deployments.apps" || got[2].group != "apps" || got[1].kind != "Binding" {
		t.Errorf("parsed %+v", got)
	}
}
