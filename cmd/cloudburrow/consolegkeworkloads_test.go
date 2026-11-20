package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/container/apiv1/containerpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/gke"
)

const gkeWorkloadFixture = `{"items":[
  {"kind":"Deployment","metadata":{"name":"web","namespace":"default"},
   "spec":{"replicas":2,"strategy":{"rollingUpdate":{"maxSurge":"25%","maxUnavailable":1}},
     "template":{"spec":{"containers":[{"name":"nginx","image":"nginx:1"}]}}},"status":{"readyReplicas":2}},
  {"kind":"DaemonSet","metadata":{"name":"agent","namespace":"default"},
   "spec":{"template":{"spec":{"containers":[{"name":"a","image":"busybox"}]}}}},
  {"kind":"Job","metadata":{"name":"once","namespace":"default"},"spec":{},"status":{"succeeded":1}},
  {"kind":"Deployment","metadata":{"name":"coredns","namespace":"kube-system"},"spec":{"replicas":1}}]}`

func gkeActionIDs(as []console.Action) string {
	var out []string
	for _, a := range as {
		id := a.ID
		if a.Unavailable != "" {
			id += "(off)"
		}
		out = append(out, id)
	}
	return strings.Join(out, ",")
}

// TestGKEWorkloadActionsFollowGoogle (#1857): a Deployment has Edit
// replicas, Autoscale, Expose, Rolling update and Delete; a Daemon Set
// Expose, Rolling update and Delete; a Job Delete; a system object's are
// refused; and each runs the kubectl command Google's console's call
// amounts to, in the workload's namespace.
func TestGKEWorkloadActionsFollowGoogle(t *testing.T) {
	ctx := context.Background()
	f := useFakeKube(t, gkeWorkloadFixture)
	p := gkeWorkloads("/kc", "cloudburrow-t", "cloudburrow")

	for name, want := range map[string]string{
		"web":     "scale,autoscale,expose,rollingupdate,delete,edityaml",
		"agent":   "expose,rollingupdate,delete,edityaml",
		"once":    "delete,edityaml",
		"coredns": "delete(off)",
	} {
		if got := gkeActionIDs(p.DetailActions(ctx, "", []string{name})); got != want {
			t.Errorf("%s's actions = %s, want %s", name, got, want)
		}
	}
	l, err := p.List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range l.Items {
		if r.Name == "web" && (gkeActionIDs(r.Actions) != "scale,autoscale,expose,rollingupdate,delete" || strings.Join(r.Target, "/") != "web") {
			t.Errorf("web's row actions = %s at %v", gkeActionIDs(r.Actions), r.Target)
		}
	}

	for _, c := range []struct {
		name, action string
		v            map[string]string
		want         string
	}{
		{"web", "scale", map[string]string{"replicas": "5"}, "scale deployment/web --replicas=5 -n default"},
		{"web", "autoscale", map[string]string{"min": "1", "max": "4", "cpu": "70"}, "autoscale deployment/web --min=1 --max=4 --cpu-percent=70 -n default"},
		{"web", "expose", map[string]string{"port": "80", "targetPort": "8080", "protocol": "TCP", "type": "Load balancer", "service": "web-service"},
			"expose deployment/web --port=80 --target-port=8080 --protocol=TCP --type=LoadBalancer --name=web-service -n default"},
		{"web", "rollingupdate", map[string]string{"image-nginx": "nginx:2"}, "set image deployment/web nginx=nginx:2 -n default"},
		{"agent", "delete", nil, "delete daemonset/agent -n default"},
	} {
		if err := p.ActAt(ctx, "", []string{c.name}, c.action, c.v); err != nil {
			t.Errorf("%s %s: %v", c.action, c.name, err)
			continue
		}
		if got := f.last(t); !strings.HasSuffix(got, c.want) {
			t.Errorf("%s %s ran %q, want …%q", c.action, c.name, got, c.want)
		}
	}
	// Rolling update's strategy is a merge patch before the image.
	if err := p.ActAt(ctx, "", []string{"web"}, "rollingupdate", map[string]string{"minReady": "5", "maxSurge": "2", "maxUnavailable": "0"}); err != nil {
		t.Fatal(err)
	}
	if got := f.last(t); !strings.Contains(got, `patch deployment/web --type merge -p {"spec":{"minReadySeconds":5,"strategy":{"rollingUpdate":{"maxSurge":2,"maxUnavailable":0},"type":"RollingUpdate"}}}`) {
		t.Errorf("rolling update ran %q", got)
	}
	for _, c := range []struct{ name, action string }{{"coredns", "delete"}, {"once", "scale"}, {"agent", "autoscale"}} {
		if err := p.ActAt(ctx, "", []string{c.name}, c.action, map[string]string{"replicas": "1", "min": "1", "max": "2"}); err == nil {
			t.Errorf("%s on %s was accepted", c.action, c.name)
		}
	}
	if err := p.ActAt(ctx, "", []string{"web"}, "autoscale", map[string]string{"min": "3", "max": "2"}); err == nil {
		t.Error("a maximum below the minimum was accepted")
	}
	// The YAML tab's Edit replaces the object, in its namespace, from stdin.
	if err := p.ActAt(ctx, "", []string{"web"}, "edityaml", map[string]string{"yaml": "kind: Deployment\n"}); err != nil {
		t.Fatal(err)
	}
	if got := f.last(t); !strings.HasSuffix(got, "replace -n default -f -") {
		t.Errorf("Edit YAML ran %q", got)
	}
}

// TestGKEServedClusterTabsAndUpgrades (#1857): a served cluster's page has
// Google's tabs (Details, Nodes holding Node pools and Nodes, Storage,
// Observability, Logs), Create cluster makes the extra node pools asked
// for, the control plane's Upgrade offers the newer versions served, and a
// node pool's Upgrade the versions up to the control plane's.
func TestGKEServedClusterTabsAndUpgrades(t *testing.T) {
	ctx := context.Background()
	be := &consoleGKEBackend{}
	api, err := gke.NewServer(be, gke.Options{Instance: "cloudburrow-t", Versions: []gke.Version{
		{GKE: "1.35.2-gke.0", Image: "kindest/node:v1.35.2"}, {GKE: "1.36.4-gke.0", Image: "kindest/node:v1.36.4"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer api.Close()
	p := gkeClustersProvider{name: "cloudburrow-t", kubeconfig: "/nonexistent", svc: &gkeService{api: api}, gkeDir: t.TempDir()}
	_, fields := p.CreateForm()
	var pools *console.Field
	for i := range fields {
		if fields[i].Name == "pools" {
			pools = &fields[i]
		}
	}
	if pools == nil || pools.Type != console.GroupFieldType || pools.AddLabel != "Add node pool" || pools.ShowWhen != "mode=Standard" {
		t.Fatalf("Create cluster's node pools = %+v", pools)
	}
	if _, err := p.Create(ctx, "pc", map[string]string{"mode": "Standard", "name": "std-2", "locationType": "Zonal", "zone": "us-central1-a",
		"channel": "Regular channel", "version": "1.35.2-gke.0", "nodes": "1", "machine": "e2-medium",
		"pools": `[{"name":"pool-b","nodes":"2","machine":"e2-small","image":"UBUNTU_CONTAINERD"}]`}); err != nil {
		t.Fatalf("Create Standard with two pools: %v", err)
	}
	if _, err := p.Create(ctx, "pc", map[string]string{"mode": "Standard", "name": "std-3", "locationType": "Zonal", "zone": "us-central1-a",
		"nodes": "1", "pools": `[{"name":"","nodes":"1"}]`}); err != nil && !strings.Contains(err.Error(), "no name") {
		t.Errorf("a nameless pool: %v", err)
	} else if err == nil {
		t.Error("a nameless node pool was accepted")
	}
	name := "projects/pc/locations/us-central1-a/clusters/std-2"
	deadline := time.Now().Add(5 * time.Second)
	var c *containerpb.Cluster
	for time.Now().Before(deadline) {
		c, _ = api.GetCluster(ctx, &containerpb.GetClusterRequest{Name: name})
		if c.GetStatus() == containerpb.Cluster_RUNNING {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(c.GetNodePools()) != 2 || c.GetNodePools()[1].GetName() != "pool-b" {
		t.Fatalf("pools = %v", c.GetNodePools())
	}

	d, err := p.Detail(ctx, "pc", []string{"std-2"})
	if err != nil {
		t.Fatal(err)
	}
	var tabs []string
	for _, s := range d.Sections {
		label := s.Label
		if s.Group != "" {
			label = s.Group + ">" + s.Label
		}
		tabs = append(tabs, label)
		if s.ID == "details" {
			for _, prop := range s.Groups[0].Properties {
				if prop.Label == "Version" && (prop.Action != "upgrade" || prop.ActionText != "Upgrade available") {
					t.Errorf("Version = %+v, want Upgrade available", prop)
				}
			}
		}
		if s.ID == "nodepools" {
			for _, r := range s.Listing.Items {
				for _, a := range r.Actions {
					if a.ID == "upgrade" && a.Unavailable == "" {
						t.Errorf("%s's Upgrade is offered while it runs the control plane's version", r.Name)
					}
				}
			}
		}
	}
	if got := strings.Join(tabs, ","); got != "Details,Nodes>Node pools,Nodes>Nodes,Storage,Observability,Logs,Operations" {
		t.Errorf("tabs = %s", got)
	}
	var upgrade console.Action
	for _, a := range p.DetailActions(ctx, "pc", []string{"std-2"}) {
		if a.ID == "upgrade" {
			upgrade = a
		}
	}
	if upgrade.Unavailable != "" || len(upgrade.Fields) != 1 || strings.Join(upgrade.Fields[0].Options, ",") != "1.36.4-gke.0" {
		t.Fatalf("Upgrade = %+v", upgrade)
	}
	// The served versions run one kind node image each: the API refuses
	// moving a cluster across them, and the console says so.
	if err := p.ActAt(ctx, "pc", []string{"std-2"}, "upgrade", map[string]string{"version": "1.36.4-gke.0"}); err == nil ||
		!strings.Contains(err.Error(), "not available locally") {
		t.Errorf("Upgrade = %v", err)
	}

	// A pool behind the control plane is offered the versions up to it.
	np := &containerpb.NodePool{Name: "old", Version: "1.35.2-gke.0"}
	cp := &containerpb.Cluster{CurrentMasterVersion: "1.36.4-gke.0"}
	versions := []string{"1.35.2-gke.0", "1.36.4-gke.0", "1.37.0-gke.0"}
	if a := gkePoolUpgrade(cp, np, versions); a.Unavailable != "" || strings.Join(a.Fields[0].Options, ",") != "1.36.4-gke.0" {
		t.Errorf("pool Upgrade = %+v", a)
	}
	cp.CurrentMasterVersion = "1.35.2-gke.0"
	if a := gkePoolUpgrade(cp, np, versions); !strings.Contains(a.Unavailable, "upgrade the control plane first") {
		t.Errorf("pool Upgrade at the control plane's version = %+v", a)
	}
	if got := strings.Join(gkeNewer(versions, "1.35.2-gke.0"), ","); got != "1.37.0-gke.0,1.36.4-gke.0" {
		t.Errorf("newer = %s", got)
	}
	if got := gkeCoresText(0.85 + 0.1); got != "0.95" {
		t.Errorf("0.85 + 0.1 cores reads %s", got)
	}
}
