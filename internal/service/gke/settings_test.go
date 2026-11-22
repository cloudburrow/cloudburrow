package gke

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/container/apiv1/containerpb"
	"google.golang.org/grpc/codes"
)

// ctlFake is a Controller over upFake: its nodes are the pools', its pods
// what a test sets.
type ctlFake struct {
	*upFake
	cmu      sync.Mutex
	abac     []bool
	netpol   []bool
	repaired []string
	pods     []map[string]any
	notReady map[string]time.Time
}

func (f *ctlFake) Get(_ context.Context, _, path string) ([]byte, error) {
	f.cmu.Lock()
	defer f.cmu.Unlock()
	switch path {
	case "/api/v1/nodes":
		var items []map[string]any
		f.umu.Lock()
		for pool, ns := range f.pools {
			for _, n := range ns {
				cond := map[string]any{"type": "Ready", "status": "True", "lastTransitionTime": "2026-01-01T00:00:00Z"}
				if t, ok := f.notReady[n.Name]; ok {
					cond = map[string]any{"type": "Ready", "status": "Unknown", "lastTransitionTime": t.Format(time.RFC3339)}
				}
				items = append(items, map[string]any{
					"metadata": map[string]any{"name": n.Name, "labels": map[string]string{"cloud.google.com/gke-nodepool": pool}},
					"status":   map[string]any{"allocatable": map[string]string{"cpu": "4", "memory": "8Gi"}, "conditions": []any{cond}}})
			}
		}
		f.umu.Unlock()
		return json.Marshal(map[string]any{"items": items})
	case "/api/v1/pods":
		return json.Marshal(map[string]any{"items": f.pods})
	case "/openid/v1/jwks":
		return []byte(`{"keys":[{"use":"sig","kty":"RSA","kid":"k1","alg":"RS256","n":"AQAB-n","e":"AQAB"}]}`), nil
	}
	return nil, nil
}

func (f *ctlFake) SetLegacyAbac(_ context.Context, _ string, on bool) error {
	f.cmu.Lock()
	defer f.cmu.Unlock()
	f.abac = append(f.abac, on)
	return nil
}

func (f *ctlFake) SetNetworkPolicy(_ context.Context, _ string, on bool) error {
	f.cmu.Lock()
	defer f.cmu.Unlock()
	f.netpol = append(f.netpol, on)
	return nil
}

func (f *ctlFake) RepairNode(_ context.Context, _, _, _, name string) error {
	f.cmu.Lock()
	defer f.cmu.Unlock()
	f.repaired = append(f.repaired, name)
	delete(f.notReady, name)
	return nil
}

func (f *ctlFake) setPods(p ...map[string]any) {
	f.cmu.Lock()
	f.pods = p
	f.cmu.Unlock()
}

type subnets struct{}

func (subnets) HasNetwork(string, string) bool { return true }
func (subnets) Subnetwork(string, string, string) (string, bool) {
	return "projects/p/global/networks/default", true
}
func (subnets) UsableSubnetworks(project string) []UsableSubnet {
	return []UsableSubnet{
		{Subnetwork: "projects/" + project + "/regions/us-central1/subnetworks/default", Network: "projects/" + project + "/global/networks/default",
			IPCIDRRange: "10.128.0.0/20", Secondary: [][2]string{{"pods", "10.4.0.0/14"}}},
		{Subnetwork: "projects/" + project + "/regions/us-east1/subnetworks/default", Network: "projects/" + project + "/global/networks/default", IPCIDRRange: "10.142.0.0/20"},
	}
}

func newCtlServer(t *testing.T, now *time.Time) (*Server, *ctlFake) {
	t.Helper()
	f := &ctlFake{upFake: newUpFake(), notReady: map[string]time.Time{}}
	var mu sync.Mutex
	s, err := NewServer(f, Options{Instance: "cloudburrow-t", AutoUpgradeInterval: -1, AutoscaleInterval: -1,
		Now:      func() time.Time { mu.Lock(); defer mu.Unlock(); return *now },
		Networks: func() Networks { return subnets{} },
		Versions: []Version{{GKE: newV, Image: newImg}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s, f
}

func mustDone(t *testing.T, s *Server, loc string, op *containerpb.Operation, err error) *containerpb.Operation {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	got := wait(t, s, "p", loc, op)
	if got.Error != nil {
		t.Fatal(got.Error)
	}
	return got
}

func TestLegacyAbacNetworkPolicyAndAddons(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	s, f := newCtlServer(t, &now)
	ctx := context.Background()
	const loc = "us-central1-a"
	op, err := s.CreateCluster(ctx, &containerpb.CreateClusterRequest{Parent: "projects/p/locations/" + loc,
		Cluster: &containerpb.Cluster{Name: "c1", InitialNodeCount: 1}})
	mustDone(t, s, loc, op, err)
	name := "projects/p/locations/" + loc + "/clusters/c1"
	if len(f.netpol) != 1 || f.netpol[0] {
		t.Fatalf("a new Standard cluster's enforcement = %v, want off", f.netpol)
	}
	c, _ := s.GetCluster(ctx, &containerpb.GetClusterRequest{Name: name})
	if !c.GetAddonsConfig().GetNetworkPolicyConfig().GetDisabled() {
		t.Errorf("the network policy add-on of a new cluster = %v, want disabled", c.GetAddonsConfig())
	}

	_, err = s.SetNetworkPolicy(ctx, &containerpb.SetNetworkPolicyRequest{Name: name, NetworkPolicy: &containerpb.NetworkPolicy{Enabled: true}})
	wantCode(t, "network policy without its add-on", err, codes.FailedPrecondition)
	_, err = s.SetAddonsConfig(ctx, &containerpb.SetAddonsConfigRequest{Name: name, AddonsConfig: &containerpb.AddonsConfig{
		HttpLoadBalancing: &containerpb.HttpLoadBalancing{Disabled: true}}})
	wantCode(t, "another add-on", err, codes.Unimplemented)
	op, err = s.SetAddonsConfig(ctx, &containerpb.SetAddonsConfigRequest{Name: name, AddonsConfig: &containerpb.AddonsConfig{
		NetworkPolicyConfig: &containerpb.NetworkPolicyConfig{}}})
	mustDone(t, s, loc, op, err)
	op, err = s.SetNetworkPolicy(ctx, &containerpb.SetNetworkPolicyRequest{Name: name, NetworkPolicy: &containerpb.NetworkPolicy{Enabled: true}})
	if got := mustDone(t, s, loc, op, err); got.OperationType != containerpb.Operation_SET_NETWORK_POLICY {
		t.Errorf("operation type %v", got.OperationType)
	}
	c, _ = s.GetCluster(ctx, &containerpb.GetClusterRequest{Name: name})
	if !c.GetNetworkPolicy().GetEnabled() || c.GetNetworkPolicy().GetProvider() != containerpb.NetworkPolicy_CALICO || !f.netpol[len(f.netpol)-1] {
		t.Errorf("network policy = %v, enforcement calls %v", c.GetNetworkPolicy(), f.netpol)
	}
	_, err = s.SetAddonsConfig(ctx, &containerpb.SetAddonsConfigRequest{Name: name, AddonsConfig: &containerpb.AddonsConfig{
		NetworkPolicyConfig: &containerpb.NetworkPolicyConfig{Disabled: true}}})
	wantCode(t, "the add-on while enforcing", err, codes.FailedPrecondition)

	op, err = s.SetLegacyAbac(ctx, &containerpb.SetLegacyAbacRequest{Name: name, Enabled: true})
	mustDone(t, s, loc, op, err)
	c, _ = s.GetCluster(ctx, &containerpb.GetClusterRequest{Name: name})
	if !c.GetLegacyAbac().GetEnabled() || len(f.abac) != 1 || !f.abac[0] {
		t.Errorf("legacy ABAC = %v, calls %v", c.GetLegacyAbac(), f.abac)
	}

	keys, err := s.GetJSONWebKeys(ctx, &containerpb.GetJSONWebKeysRequest{Parent: name})
	if err != nil || len(keys.GetKeys()) != 1 || keys.Keys[0].Kid != "k1" || keys.Keys[0].Alg != "RS256" {
		t.Errorf("GetJSONWebKeys = %v, %v", keys, err)
	}

	f.setPods(map[string]any{"metadata": map[string]any{"name": "priv", "namespace": "default"},
		"spec": map[string]any{"hostNetwork": true, "containers": []any{map[string]any{"name": "c", "securityContext": map[string]any{"privileged": true}}}}},
		map[string]any{"metadata": map[string]any{"name": "sys", "namespace": "kube-system"},
			"spec": map[string]any{"containers": []any{map[string]any{"name": "c", "securityContext": map[string]any{"privileged": true}}}}})
	compat, err := s.CheckAutopilotCompatibility(ctx, &containerpb.CheckAutopilotCompatibilityRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, i := range compat.GetIssues() {
		got = append(got, i.GetConstraintType()+"="+strings.Join(i.GetSubjects(), ","))
	}
	if strings.Join(got, ";") != "autogke-disallow-privilege=default/priv;autogke-disallow-hostnamespaces=default/priv" {
		t.Errorf("issues = %v", got)
	}

	l, err := s.ListUsableSubnetworks(ctx, &containerpb.ListUsableSubnetworksRequest{Parent: "projects/p", PageSize: 1})
	if err != nil || len(l.GetSubnetworks()) != 1 || l.GetNextPageToken() == "" ||
		l.Subnetworks[0].GetSecondaryIpRanges()[0].GetStatus() != containerpb.UsableSubnetworkSecondaryRange_UNUSED {
		t.Fatalf("ListUsableSubnetworks = %v, %v", l, err)
	}
	l, err = s.ListUsableSubnetworks(ctx, &containerpb.ListUsableSubnetworksRequest{Parent: "projects/p", PageToken: l.GetNextPageToken()})
	if err != nil || len(l.GetSubnetworks()) != 1 || l.GetNextPageToken() != "" || !strings.Contains(l.Subnetworks[0].Subnetwork, "us-east1") {
		t.Fatalf("ListUsableSubnetworks page 2 = %v, %v", l, err)
	}
	_, err = s.ListUsableSubnetworks(ctx, &containerpb.ListUsableSubnetworksRequest{Parent: "projects/p", Filter: "name=x"})
	wantCode(t, "another filter", err, codes.InvalidArgument)

	// An Autopilot cluster refuses ABAC at creation.
	_, err = s.CreateCluster(ctx, &containerpb.CreateClusterRequest{Parent: "projects/p/locations/us-central1",
		Cluster: &containerpb.Cluster{Name: "ap", Autopilot: &containerpb.Autopilot{Enabled: true}, LegacyAbac: &containerpb.LegacyAbac{Enabled: true}}})
	wantCode(t, "Autopilot with ABAC", err, codes.InvalidArgument)
}

func TestAutoscalerAndAutoRepair(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	s, f := newCtlServer(t, &now)
	ctx := context.Background()
	const loc = "us-central1-a"
	op, err := s.CreateCluster(ctx, &containerpb.CreateClusterRequest{Parent: "projects/p/locations/" + loc,
		Cluster: &containerpb.Cluster{Name: "c1", InitialNodeCount: 1}})
	mustDone(t, s, loc, op, err)
	pool := "projects/p/locations/" + loc + "/clusters/c1/nodePools/default-pool"

	_, err = s.SetNodePoolAutoscaling(ctx, &containerpb.SetNodePoolAutoscalingRequest{Name: pool,
		Autoscaling: &containerpb.NodePoolAutoscaling{Enabled: true, MinNodeCount: 3, MaxNodeCount: 2}})
	wantCode(t, "min above max", err, codes.InvalidArgument)
	_, err = s.SetNodePoolAutoscaling(ctx, &containerpb.SetNodePoolAutoscalingRequest{Name: pool,
		Autoscaling: &containerpb.NodePoolAutoscaling{Enabled: true, MaxNodeCount: 50}})
	wantCode(t, "past the local cluster", err, codes.ResourceExhausted)
	op, err = s.SetNodePoolAutoscaling(ctx, &containerpb.SetNodePoolAutoscalingRequest{Name: pool,
		Autoscaling: &containerpb.NodePoolAutoscaling{Enabled: true, MinNodeCount: 1, MaxNodeCount: 2}})
	mustDone(t, s, loc, op, err)

	pending := map[string]any{"metadata": map[string]any{"name": "web", "namespace": "default"},
		"spec":   map[string]any{"containers": []any{map[string]any{"resources": map[string]any{"requests": map[string]string{"cpu": "3"}}}}},
		"status": map[string]any{"phase": "Pending", "conditions": []any{map[string]any{"type": "PodScheduled", "status": "False", "reason": "Unschedulable"}}}}
	tooBig := map[string]any{"metadata": map[string]any{"name": "huge", "namespace": "default"},
		"spec":   map[string]any{"containers": []any{map[string]any{"resources": map[string]any{"requests": map[string]string{"cpu": "64"}}}}},
		"status": pending["status"]}
	f.setPods(tooBig)
	s.Autoscale(ctx)
	if n := len(f.pools["default-pool"]); n != 1 {
		t.Fatalf("a pod no node could hold grew the pool to %d", n)
	}
	f.setPods(pending)
	s.Autoscale(ctx)
	np, _ := s.GetNodePool(ctx, &containerpb.GetNodePoolRequest{Name: pool})
	if n := len(f.pools["default-pool"]); n != 2 || np.GetInitialNodeCount() != 2 {
		t.Fatalf("after a pending pod the pool has %d nodes (record %d), want 2", n, np.GetInitialNodeCount())
	}
	now = now.Add(time.Minute)
	s.Autoscale(ctx)
	if n := len(f.pools["default-pool"]); n != 2 {
		t.Fatalf("the pool grew past its maximum: %d", n)
	}

	// The pod runs on the second node; the first is empty and goes after
	// ten minutes, the pool never below its minimum.
	running := map[string]any{"metadata": map[string]any{"name": "web", "namespace": "default"},
		"spec": map[string]any{"nodeName": f.pools["default-pool"][1].Name}, "status": map[string]any{"phase": "Running"}}
	f.setPods(running)
	s.Autoscale(ctx)
	now = now.Add(5 * time.Minute)
	s.Autoscale(ctx)
	if n := len(f.pools["default-pool"]); n != 2 {
		t.Fatalf("a node empty for 5 minutes was removed: %d nodes", n)
	}
	now = now.Add(6 * time.Minute)
	s.Autoscale(ctx)
	np, _ = s.GetNodePool(ctx, &containerpb.GetNodePoolRequest{Name: pool})
	if n := len(f.pools["default-pool"]); n != 1 || np.GetInitialNodeCount() != 1 || !strings.Contains(f.events(), "drain n1") {
		t.Fatalf("after ten empty minutes: %d nodes (record %d), events %s", n, np.GetInitialNodeCount(), f.events())
	}
	f.setPods()
	now = now.Add(20 * time.Minute)
	s.Autoscale(ctx)
	if n := len(f.pools["default-pool"]); n != 1 {
		t.Fatalf("the pool went below its minimum: %d", n)
	}

	// Auto-repair: a node NotReady for ten minutes is re-created.
	node := f.pools["default-pool"][0].Name
	f.cmu.Lock()
	f.notReady[node] = now.Add(-3 * time.Minute)
	f.cmu.Unlock()
	if ops := s.Autoscale(ctx); len(ops) != 0 {
		t.Fatalf("a node NotReady for 3 minutes was repaired: %v", ops)
	}
	// The Kubernetes Engine clock moved ahead (the maintenance windows'
	// AdvanceClock) does not age the kubelet's real-time lastTransitionTime.
	if _, err := s.AdvanceClock(time.Hour); err != nil {
		t.Fatal(err)
	}
	if ops := s.Autoscale(ctx); len(ops) != 0 {
		t.Fatalf("a node NotReady for 3 minutes was repaired once the clock moved an hour ahead: %v", ops)
	}
	now = now.Add(8 * time.Minute)
	ops := s.Autoscale(ctx)
	if len(ops) != 1 || ops[0].OperationType != containerpb.Operation_AUTO_REPAIR_NODES {
		t.Fatalf("repair operations = %v", ops)
	}
	wait(t, s, "p", loc, ops[0])
	if strings.Join(f.repaired, ",") != node {
		t.Errorf("repaired %v, want %s", f.repaired, node)
	}

	// Management: auto-repair off stops repairs; auto-upgrade stays on a
	// release channel.
	_, err = s.SetNodePoolManagement(ctx, &containerpb.SetNodePoolManagementRequest{Name: pool, Management: &containerpb.NodeManagement{AutoRepair: true}})
	wantCode(t, "auto-upgrade off on a release channel", err, codes.InvalidArgument)
	op, err = s.SetNodePoolManagement(ctx, &containerpb.SetNodePoolManagementRequest{Name: pool, Management: &containerpb.NodeManagement{AutoUpgrade: true}})
	mustDone(t, s, loc, op, err)
	f.cmu.Lock()
	f.notReady[node] = now.Add(-time.Hour)
	f.cmu.Unlock()
	if ops := s.Autoscale(ctx); len(ops) != 0 {
		t.Fatalf("a pool without auto-repair was repaired: %v", ops)
	}
}
