//go:build compat

package compat

import (
	"context"
	"os"
	"os/exec"
	"sort"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/container/apiv1/containerpb"
	"google.golang.org/grpc/codes"
)

// gkeNodesByZone is each node of the pool, by its
// topology.kubernetes.io/zone label.
func gkeNodesByZone(t *testing.T, kubeconfig, pool string) map[string]string {
	t.Helper()
	out := gkeKubectl(t, kubeconfig, "", "get", "nodes", "-l", "cloud.google.com/gke-nodepool="+pool, "-o",
		`jsonpath={range .items[*]}{.metadata.name}={.metadata.labels.topology\.kubernetes\.io/zone}{"\n"}{end}`)
	m := map[string]string{}
	for _, l := range strings.Fields(out) {
		k, v, _ := strings.Cut(l, "=")
		m[k] = v
	}
	return m
}

// zoneCounts is how many nodes each zone has, as "zone:n" joined by
// commas in zone order.
func zoneCounts(nodes map[string]string) string {
	c := map[string]int{}
	for _, z := range nodes {
		c[z]++
	}
	var out []string
	for z, n := range c {
		out = append(out, z+":"+string(rune('0'+n)))
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

// TestGKENodeZones (#2100), through the official Go client over gRPC and
// REST, as Google's managing-clusters page documents a cluster's node
// locations: every node pool has its node count in each of the cluster's
// zones, and "nodes are immediately added or removed". A zonal cluster
// made with locations us-central1-a and -b has one default-pool node in
// each, labelled topology.kubernetes.io/zone (and region) from the start.
// SetLocations adding us-central1-c adds a node there and leaves the two
// others where they were; removing us-central1-b deletes that zone's node,
// and the nodes of -a and -c are the same nodes, in the same zones. A node
// pool made afterwards (CreateNodePool) has a node in each zone, labelled;
// SetNodePoolSize to two puts two in each zone, the first nodes kept.
// covers: google.container.v1.ClusterManager/SetLocations, google.container.v1.ClusterManager/CreateNodePool, google.container.v1.ClusterManager/SetNodePoolSize
func TestGKENodeZones(t *testing.T) {
	h := New(t)
	g, r := gkeClients(t, h)
	if _, err := exec.LookPath("kubectl"); err != nil {
		t.Skip("kubectl is required")
	}
	ctx := context.Background()
	project := h.Project()
	const zone, cluster = "us-central1-a", "zones-2100"
	parent := "projects/" + project + "/locations/" + zone
	name := parent + "/clusters/" + cluster
	t.Cleanup(func() {
		if op, err := g.DeleteCluster(context.Background(), &containerpb.DeleteClusterRequest{Name: name}); err == nil {
			gkeWait(t, g, project, zone, op)
		}
	})
	op, err := g.CreateCluster(ctx, &containerpb.CreateClusterRequest{Parent: parent, Cluster: &containerpb.Cluster{
		Name: cluster, MaintenancePolicy: gkeNoAutoUpgrade(), Locations: []string{zone, "us-central1-b"},
		MasterAuth: &containerpb.MasterAuth{ClientCertificateConfig: &containerpb.ClientCertificateConfig{IssueClientCertificate: true}},
		NodePools:  []*containerpb.NodePool{{Name: "default-pool", InitialNodeCount: 1}}}})
	if err != nil {
		t.Fatal(err)
	}
	gkeWait(t, g, project, zone, op)
	c, err := g.GetCluster(ctx, &containerpb.GetClusterRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	kc := gkeKubeconfig(t, c)
	made := gkeNodesByZone(t, kc, "default-pool")
	if got := zoneCounts(made); got != "us-central1-a:1,us-central1-b:1" {
		t.Fatalf("a new cluster's nodes by zone = %s (%v), want one in each of its zones", got, made)
	}
	region := gkeKubectl(t, kc, "", "get", "nodes", "-l", "cloud.google.com/gke-nodepool=default-pool", "-o",
		`jsonpath={.items[*].metadata.labels.topology\.kubernetes\.io/region}`)
	if region != "us-central1 us-central1" {
		t.Errorf("the nodes' region labels = %q", region)
	}

	op, err = r.SetLocations(ctx, &containerpb.SetLocationsRequest{Name: name, Locations: []string{zone, "us-central1-b", "us-central1-c"}})
	if err != nil {
		t.Fatalf("SetLocations (add -c): %v", err)
	}
	gkeWait(t, g, project, zone, op)
	three := gkeNodesByZone(t, kc, "default-pool")
	if got := zoneCounts(three); got != "us-central1-a:1,us-central1-b:1,us-central1-c:1" {
		t.Fatalf("after adding us-central1-c, nodes by zone = %s", got)
	}
	for n, z := range made {
		if three[n] != z {
			t.Errorf("node %s moved from %s to %q when a zone was added", n, z, three[n])
		}
	}

	op, err = g.SetLocations(ctx, &containerpb.SetLocationsRequest{Name: name, Locations: []string{zone, "us-central1-c"}})
	if err != nil {
		t.Fatalf("SetLocations (remove -b): %v", err)
	}
	gkeWait(t, g, project, zone, op)
	two := gkeNodesByZone(t, kc, "default-pool")
	if got := zoneCounts(two); got != "us-central1-a:1,us-central1-c:1" {
		t.Fatalf("after removing us-central1-b, nodes by zone = %s", got)
	}
	for n, z := range three {
		switch {
		case z == "us-central1-b" && two[n] != "":
			t.Errorf("us-central1-b's node %s was kept (now in %s): GKE deletes a removed zone's nodes", n, two[n])
		case z != "us-central1-b" && two[n] != z:
			t.Errorf("node %s of %s is %q after us-central1-b was removed", n, z, two[n])
		}
	}
	np, err := g.GetNodePool(ctx, &containerpb.GetNodePoolRequest{Name: name + "/nodePools/default-pool"})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(np.GetLocations(), ","); got != "us-central1-a,us-central1-c" {
		t.Errorf("the pool's locations = %s", got)
	}

	op, err = g.CreateNodePool(ctx, &containerpb.CreateNodePoolRequest{Parent: name, NodePool: &containerpb.NodePool{Name: "extra", InitialNodeCount: 1}})
	if err != nil {
		t.Fatalf("CreateNodePool: %v", err)
	}
	gkeWait(t, g, project, zone, op)
	if got := zoneCounts(gkeNodesByZone(t, kc, "extra")); got != "us-central1-a:1,us-central1-c:1" {
		t.Errorf("a new pool's nodes by zone = %s", got)
	}
	op, err = r.SetNodePoolSize(ctx, &containerpb.SetNodePoolSizeRequest{Name: name + "/nodePools/default-pool", NodeCount: 2})
	if err != nil {
		t.Fatalf("SetNodePoolSize: %v", err)
	}
	gkeWait(t, g, project, zone, op)
	four := gkeNodesByZone(t, kc, "default-pool")
	if got := zoneCounts(four); got != "us-central1-a:2,us-central1-c:2" {
		t.Errorf("after resizing to two per zone, nodes by zone = %s", got)
	}
	for n, z := range two {
		if four[n] != z {
			t.Errorf("node %s of %s is %q after the resize", n, z, four[n])
		}
	}
}

// gkeNodeTaints is each node of the pool's taints, "key=value:Effect"
// joined by spaces, by node.
func gkeNodeTaints(t *testing.T, kubeconfig, pool string) map[string]string {
	t.Helper()
	out := gkeKubectl(t, kubeconfig, "", "get", "nodes", "-l", "cloud.google.com/gke-nodepool="+pool, "-o",
		`jsonpath={range .items[*]}{.metadata.name}|{range .spec.taints[*]}{.key}={.value}:{.effect} {end}{"\n"}{end}`)
	m := map[string]string{}
	for _, l := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(l), "|"); ok {
			// The node lifecycle's own taints (node.kubernetes.io/not-ready
			// for a moment after the node turns Ready) are not the pool's.
			var own []string
			for _, taint := range strings.Fields(v) {
				if !strings.HasPrefix(taint, "node.kubernetes.io/") {
					own = append(own, taint)
				}
			}
			m[k] = strings.Join(own, " ")
		}
	}
	return m
}

// gkePodPhase is a pod's phase and the node it runs on.
func gkePodPhase(t *testing.T, kubeconfig, pod string) string {
	t.Helper()
	return gkeKubectl(t, kubeconfig, "", "get", "pod", pod, "-o", `jsonpath={.status.phase} {.spec.nodeName}`)
}

// TestGKENodeLabelsAndTaints (#2100), through the official Go client over
// gRPC and REST, as Google's node-taints and update-existing-nodepools
// pages document a node pool's Kubernetes labels and taints: a pool made
// with node_config labels and a NO_SCHEDULE taint has them on its node,
// and a pod with no toleration stays Pending; UpdateNodePool's taints
// (empty) removes the taint, and the pod is scheduled onto the node;
// UpdateNodePool's labels replace the pool's (one removed, one changed,
// one added), and the node carries exactly those; a node added later
// (SetNodePoolSize) registers with them, and with the machine type as
// node.kubernetes.io/instance-type. UpdateNodePool's machine_type
// recreates both nodes (UPGRADE_NODES, surge): new nodes carrying the new
// machine type and the pool's labels. A taint with no effect and a disk
// under 10 GB are INVALID_ARGUMENT.
// covers: google.container.v1.ClusterManager/UpdateNodePool, google.container.v1.ClusterManager/CreateNodePool
func TestGKENodeLabelsAndTaints(t *testing.T) {
	h := New(t)
	g, r := gkeClients(t, h)
	if _, err := exec.LookPath("kubectl"); err != nil {
		t.Skip("kubectl is required")
	}
	ctx := context.Background()
	project := h.Project()
	const zone, cluster = "us-central1-f", "taints-2100"
	parent := "projects/" + project + "/locations/" + zone
	name := parent + "/clusters/" + cluster
	t.Cleanup(func() {
		if op, err := g.DeleteCluster(context.Background(), &containerpb.DeleteClusterRequest{Name: name}); err == nil {
			gkeWait(t, g, project, zone, op)
		}
	})
	op, err := g.CreateCluster(ctx, &containerpb.CreateClusterRequest{Parent: parent, Cluster: &containerpb.Cluster{
		Name: cluster, MaintenancePolicy: gkeNoAutoUpgrade(),
		MasterAuth: &containerpb.MasterAuth{ClientCertificateConfig: &containerpb.ClientCertificateConfig{IssueClientCertificate: true}},
		NodePools:  []*containerpb.NodePool{{Name: "default-pool", InitialNodeCount: 1}}}})
	if err != nil {
		t.Fatal(err)
	}
	gkeWait(t, g, project, zone, op)
	c, err := g.GetCluster(ctx, &containerpb.GetClusterRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	kc := gkeKubeconfig(t, c)
	_, err = g.CreateNodePool(ctx, &containerpb.CreateNodePoolRequest{Parent: name, NodePool: &containerpb.NodePool{Name: "bad", InitialNodeCount: 1,
		Config: &containerpb.NodeConfig{Taints: []*containerpb.NodeTaint{{Key: "dedicated", Value: "x"}}}}})
	wantPrivateCACode(t, "a taint with no effect", err, codes.InvalidArgument)
	op, err = g.CreateNodePool(ctx, &containerpb.CreateNodePoolRequest{Parent: name, NodePool: &containerpb.NodePool{Name: "tainted", InitialNodeCount: 1,
		Config: &containerpb.NodeConfig{Labels: map[string]string{"team": "a", "example.com/tier": "web"},
			Taints: []*containerpb.NodeTaint{{Key: "dedicated", Value: "batch", Effect: containerpb.NodeTaint_NO_SCHEDULE}}}}})
	if err != nil {
		t.Fatalf("CreateNodePool: %v", err)
	}
	gkeWait(t, g, project, zone, op)
	nodes := gkeNodes(t, kc, "tainted")
	if len(nodes) != 1 {
		t.Fatalf("tainted pool nodes = %v", nodes)
	}
	node := nodes[0]
	if got := gkeNodeTaints(t, kc, "tainted")[node]; got != "dedicated=batch:NoSchedule" {
		t.Errorf("the node's taints = %q", got)
	}
	if got := gkeKubectl(t, kc, "", "get", "node", node, "-o", `jsonpath={.metadata.labels.team} {.metadata.labels.example\.com/tier}`); got != "a web" {
		t.Errorf("the node's labels team, example.com/tier = %q", got)
	}
	// Default-pool's node is cordoned, so the tainted node is the only one
	// a pod could go to.
	for _, n := range gkeNodes(t, kc, "default-pool") {
		gkeKubectl(t, kc, "", "cordon", n)
	}
	gkeKubectl(t, kc, "", "run", "untolerated", "--image=registry.k8s.io/pause:3.10", "--restart=Never",
		`--overrides={"spec":{"nodeSelector":{"team":"a"}}}`)
	t.Cleanup(func() {
		_, _ = exec.Command("kubectl", "--kubeconfig", kc, "delete", "pod", "untolerated", "--ignore-not-found").CombinedOutput()
	})
	if !gkeEventually(20*time.Second, func() bool { return strings.HasPrefix(gkePodPhase(t, kc, "untolerated"), "Pending") }) {
		t.Fatalf("the pod is %q", gkePodPhase(t, kc, "untolerated"))
	}
	if gkeEventually(10*time.Second, func() bool {
		return strings.TrimSpace(strings.TrimPrefix(gkePodPhase(t, kc, "untolerated"), "Pending")) != ""
	}) {
		t.Fatalf("a pod with no toleration was scheduled onto the tainted node: %q", gkePodPhase(t, kc, "untolerated"))
	}

	pool := name + "/nodePools/tainted"
	op, err = r.UpdateNodePool(ctx, &containerpb.UpdateNodePoolRequest{Name: pool, Taints: &containerpb.NodeTaints{}})
	if err != nil {
		t.Fatalf("UpdateNodePool (no taints): %v", err)
	}
	if op.GetOperationType() != containerpb.Operation_UPGRADE_NODES {
		t.Errorf("the update's operation type = %s", op.GetOperationType())
	}
	gkeWait(t, g, project, zone, op)
	if got := gkeNodeTaints(t, kc, "tainted")[node]; got != "" {
		t.Errorf("taints after removing them = %q", got)
	}
	if !gkeEventually(2*time.Minute, func() bool { return gkePodPhase(t, kc, "untolerated") == "Running "+node }) {
		t.Errorf("after the taint was removed the pod is %q, want Running on %s", gkePodPhase(t, kc, "untolerated"), node)
	}

	op, err = g.UpdateNodePool(ctx, &containerpb.UpdateNodePoolRequest{Name: pool,
		Labels: &containerpb.NodeLabels{Labels: map[string]string{"team": "b", "env": "test"}}})
	if err != nil {
		t.Fatalf("UpdateNodePool (labels): %v", err)
	}
	gkeWait(t, g, project, zone, op)
	labels := `jsonpath={.metadata.labels.team}/{.metadata.labels.env}/{.metadata.labels.example\.com/tier}`
	if got := gkeKubectl(t, kc, "", "get", "node", node, "-o", labels); got != "b/test/" {
		t.Errorf("the node's team/env/tier after the update = %q, want b/test/", got)
	}
	np, err := g.GetNodePool(ctx, &containerpb.GetNodePoolRequest{Name: pool})
	if err != nil {
		t.Fatal(err)
	}
	if l := np.GetConfig().GetLabels(); len(l) != 2 || l["team"] != "b" || l["env"] != "test" || len(np.GetConfig().GetTaints()) != 0 {
		t.Errorf("the pool's config after the updates: labels %v taints %v", l, np.GetConfig().GetTaints())
	}
	op, err = g.SetNodePoolSize(ctx, &containerpb.SetNodePoolSizeRequest{Name: pool, NodeCount: 2})
	if err != nil {
		t.Fatalf("SetNodePoolSize: %v", err)
	}
	gkeWait(t, g, project, zone, op)
	resized := gkeNodes(t, kc, "tainted")
	for _, n := range resized {
		if got := gkeKubectl(t, kc, "", "get", "node", n, "-o", labels); got != "b/test/" {
			t.Errorf("node %s's team/env/tier = %q, want b/test/", n, got)
		}
	}
	itype := `jsonpath={.metadata.labels.node\.kubernetes\.io/instance-type}`
	if got := gkeKubectl(t, kc, "", "get", "node", resized[0], "-o", itype); got != "e2-medium" {
		t.Errorf("a node's instance type = %q, want the pool's e2-medium", got)
	}

	// A machine type change recreates the nodes with the upgrade strategy.
	op, err = r.UpdateNodePool(ctx, &containerpb.UpdateNodePoolRequest{Name: pool, MachineType: "e2-standard-2"})
	if err != nil {
		t.Fatalf("UpdateNodePool (machine type): %v", err)
	}
	if op.GetOperationType() != containerpb.Operation_UPGRADE_NODES {
		t.Errorf("the machine type change's operation type = %s", op.GetOperationType())
	}
	gkeWait(t, g, project, zone, op)
	recreated := gkeNodes(t, kc, "tainted")
	if len(recreated) != 2 {
		t.Fatalf("nodes after the machine type change = %v", recreated)
	}
	for _, n := range recreated {
		for _, o := range resized {
			if n == o {
				t.Errorf("node %s was kept through the machine type change", n)
			}
		}
		if got := gkeKubectl(t, kc, "", "get", "node", n, "-o", itype+`/{.metadata.labels.team}`); got != "e2-standard-2/b" {
			t.Errorf("recreated node %s's instance type/team = %q, want e2-standard-2/b", n, got)
		}
	}
	np, _ = g.GetNodePool(ctx, &containerpb.GetNodePoolRequest{Name: pool})
	if np.GetConfig().GetMachineType() != "e2-standard-2" || np.GetStatus() != containerpb.NodePool_RUNNING {
		t.Errorf("the pool after the machine type change: %s %s", np.GetConfig().GetMachineType(), np.GetStatus())
	}
	_, err = g.UpdateNodePool(ctx, &containerpb.UpdateNodePoolRequest{Name: pool, DiskSizeGb: 5})
	wantPrivateCACode(t, "a 5 GB disk", err, codes.InvalidArgument)
}

// TestGcloudContainerNodePoolsLabelsTaintsLocations (#2100): gcloud's own
// `node-pools update` flags, as its reference gives them, on a one-node
// pool of a zonal cluster: --node-labels and --node-taints replace the
// pool's Kubernetes labels and taints (the node carries them, describe
// reads them back), --node-taints="" clears them, --node-locations
// gives the pool a node in each zone named, and --machine-type recreates
// the nodes with the new machine type.
func TestGcloudContainerNodePoolsLabelsTaintsLocations(t *testing.T) {
	h := New(t)
	g, _ := gkeClients(t, h)
	shardEndpoint(h, "CLOUDBURROW_TEST_GKE_SERVICEUSAGE", EnvServiceUsage)
	gcloud := gcloudBinary(t)
	cli := os.Getenv(EnvCLI)
	if cli == "" {
		t.Skipf("%s is not set", EnvCLI)
	}
	if _, err := exec.LookPath("kubectl"); err != nil {
		t.Skip("kubectl is required")
	}
	const zone, cluster = "us-central1-b", "gc-taints-2100"
	project := h.Project()
	gdir := t.TempDir()
	cfgName := gcloudSetup(t, cli, gdir)
	gc := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(gcloud, append(args, "--project", project, "--quiet")...)
		cmd.Env = append(gcloudTargetEnv(noGoogleEgress()), "CLOUDSDK_CONFIG="+gdir, "CLOUDSDK_ACTIVE_CONFIG_NAME="+cfgName)
		b, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("gcloud %v: %v\n%s", args, err, b)
		}
		return strings.TrimSpace(string(b))
	}
	full := "projects/" + project + "/locations/" + zone + "/clusters/" + cluster
	t.Cleanup(func() {
		if op, err := g.DeleteCluster(context.Background(), &containerpb.DeleteClusterRequest{Name: full}); err == nil {
			gkeWait(t, g, project, zone, op)
		}
	})
	gc("container", "clusters", "create", cluster, "--zone", zone, "--num-nodes", "1", "--issue-client-certificate")
	c, err := g.GetCluster(context.Background(), &containerpb.GetClusterRequest{Name: full})
	if err != nil {
		t.Fatal(err)
	}
	kc := gkeKubeconfig(t, c)
	upd := func(flags ...string) {
		t.Helper()
		gc(append([]string{"container", "node-pools", "update", "default-pool", "--cluster", cluster, "--zone", zone}, flags...)...)
	}
	upd("--node-labels=team=gc,env=dev")
	upd("--node-taints=dedicated=gc:NoExecute")
	node := gkeNodes(t, kc, "default-pool")[0]
	if got := gkeKubectl(t, kc, "", "get", "node", node, "-o", `jsonpath={.metadata.labels.team}/{.metadata.labels.env}`); got != "gc/dev" {
		t.Errorf("node labels after --node-labels = %q", got)
	}
	if got := gkeNodeTaints(t, kc, "default-pool")[node]; got != "dedicated=gc:NoExecute" {
		t.Errorf("node taints after --node-taints = %q", got)
	}
	desc := gc("container", "node-pools", "describe", "default-pool", "--cluster", cluster, "--zone", zone,
		"--format=value(config.labels.team,config.taints[0].key,config.taints[0].effect)")
	if strings.Join(strings.Fields(desc), " ") != "gc dedicated NO_EXECUTE" {
		t.Errorf("node-pools describe = %q", desc)
	}
	upd("--node-taints=")
	if got := gkeNodeTaints(t, kc, "default-pool")[node]; got != "" {
		t.Errorf("node taints after --node-taints= = %q", got)
	}
	upd("--node-locations", zone+",us-central1-c")
	if got := zoneCounts(gkeNodesByZone(t, kc, "default-pool")); got != zone+":1,us-central1-c:1" {
		t.Errorf("nodes by zone after --node-locations = %s", got)
	}
	if got := gc("container", "node-pools", "describe", "default-pool", "--cluster", cluster, "--zone", zone, "--format=value(locations)"); !strings.Contains(got, "us-central1-c") {
		t.Errorf("the pool's locations = %q", got)
	}
	upd("--machine-type=e2-standard-2")
	for _, n := range gkeNodes(t, kc, "default-pool") {
		if got := gkeKubectl(t, kc, "", "get", "node", n, "-o", `jsonpath={.metadata.labels.node\.kubernetes\.io/instance-type}`); got != "e2-standard-2" {
			t.Errorf("node %s's instance type after --machine-type = %q", n, got)
		}
	}
	if got := gc("container", "node-pools", "describe", "default-pool", "--cluster", cluster, "--zone", zone, "--format=value(config.machineType)"); got != "e2-standard-2" {
		t.Errorf("the pool's machine type = %q", got)
	}
}
