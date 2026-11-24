//go:build compat

package compat

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/container/apiv1/containerpb"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
)

// gkeCanI is `kubectl auth can-i` as the default service account.
func gkeCanI(kubeconfig, verb, resource string) string {
	out, _ := exec.Command("kubectl", "--kubeconfig", kubeconfig, "auth", "can-i", verb, resource, "--all-namespaces",
		"--as=system:serviceaccount:default:default", "--as-group=system:serviceaccounts", "--as-group=system:authenticated").CombinedOutput()
	return strings.TrimSpace(string(out))
}

// gkeEventually polls f for up to d.
func gkeEventually(d time.Duration, f func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if f() {
			return true
		}
		time.Sleep(2 * time.Second)
	}
	return f()
}

// TestGKEClusterSettings (#2100): the official client, over gRPC and REST,
// changes a Standard cluster's settings, and each has its effect on the
// local cluster:
//   - network policy: a deny-all NetworkPolicy does not stop the probe
//     while network policy is disabled (a Standard cluster's default);
//     enabling it needs the NetworkPolicy add-on (SetAddonsConfig) first,
//     then stops the probe; disabling it lets the probe serve again;
//   - legacy ABAC: the default service account may list every secret
//     only while it is enabled;
//   - GetJSONWebKeys is the API server's own service account issuer keys;
//   - CheckAutopilotCompatibility names a privileged pod;
//   - autoscaling: a pod no node has room for makes the cluster autoscaler
//     add a node, up to the pool's maximum;
//   - node management: auto-upgrade stays on a release channel, and
//     auto-repair is recorded;
//   - ListUsableSubnetworks lists the project's Compute Engine subnetworks;
//   - SetMasterAuth accepts only clearing the basic-auth username.
//
// covers: google.container.v1.ClusterManager/SetNetworkPolicy, google.container.v1.ClusterManager/SetAddonsConfig, google.container.v1.ClusterManager/SetLegacyAbac, google.container.v1.ClusterManager/GetJSONWebKeys, google.container.v1.ClusterManager/CheckAutopilotCompatibility, google.container.v1.ClusterManager/SetNodePoolAutoscaling, google.container.v1.ClusterManager/SetNodePoolManagement, google.container.v1.ClusterManager/ListUsableSubnetworks, google.container.v1.ClusterManager/SetMasterAuth
func TestGKEClusterSettings(t *testing.T) {
	h := New(t)
	g, r := gkeClients(t, h)
	if _, err := exec.LookPath("kubectl"); err != nil {
		t.Skip("kubectl is required")
	}
	instance := strings.TrimSpace(os.Getenv("CLOUDBURROW_TEST_CLUSTER"))
	if instance == "" {
		t.Skip("CLOUDBURROW_TEST_CLUSTER is not set: the probe is loaded into the cluster by kind")
	}
	ctx := context.Background()
	project := h.Project()
	const zone, cluster = "us-central1-f", "settings-2100"
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
	if c.GetNetworkPolicy().GetEnabled() || !c.GetAddonsConfig().GetNetworkPolicyConfig().GetDisabled() {
		t.Errorf("a new cluster's network policy = %v, add-on %v; want both disabled", c.GetNetworkPolicy(), c.GetAddonsConfig().GetNetworkPolicyConfig())
	}
	kc := gkeKubeconfig(t, c)

	// Network policy.
	gkeDeployProbe(ctx, t, instance, name, kc)
	gkeKubectl(t, kc, `apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: {name: deny-all, namespace: upgrade}
spec:
  podSelector: {}
  policyTypes: [Ingress]
`, "apply", "-f", "-")
	time.Sleep(5 * time.Second)
	if err := gkeProbeServes(kc); err != nil {
		t.Fatalf("a deny-all NetworkPolicy stopped the probe while network policy is disabled: %v", err)
	}
	_, err = g.SetNetworkPolicy(ctx, &containerpb.SetNetworkPolicyRequest{Name: name, NetworkPolicy: &containerpb.NetworkPolicy{Enabled: true}})
	wantPrivateCACode(t, "network policy before its add-on", err, codes.FailedPrecondition)
	op, err = r.SetAddonsConfig(ctx, &containerpb.SetAddonsConfigRequest{Name: name, AddonsConfig: &containerpb.AddonsConfig{
		NetworkPolicyConfig: &containerpb.NetworkPolicyConfig{Disabled: false}}})
	if err != nil {
		t.Fatal(err)
	}
	gkeWait(t, g, project, zone, op)
	_, err = g.SetAddonsConfig(ctx, &containerpb.SetAddonsConfigRequest{Name: name, AddonsConfig: &containerpb.AddonsConfig{
		CloudRunConfig: &containerpb.CloudRunConfig{Disabled: false}}})
	wantPrivateCACode(t, "an add-on with no local counterpart", err, codes.Unimplemented)
	// The probe is asked every second while enforcement is enabled, so
	// the API server's service proxy keeps its connection to it alive
	// across the change (main run 45422957668): GKE re-creates the nodes,
	// so no connection made before outlives it.
	keepAlive := make(chan struct{})
	kept := make(chan struct{})
	go func() {
		defer close(kept)
		for {
			select {
			case <-keepAlive:
				return
			case <-time.After(time.Second):
				_ = gkeProbeServes(kc)
			}
		}
	}()
	op, err = g.SetNetworkPolicy(ctx, &containerpb.SetNetworkPolicyRequest{Name: name, NetworkPolicy: &containerpb.NetworkPolicy{Enabled: true}})
	if err != nil {
		close(keepAlive)
		t.Fatal(err)
	}
	got := gkeWait(t, g, project, zone, op)
	close(keepAlive)
	<-kept
	if got.GetOperationType() != containerpb.Operation_SET_NETWORK_POLICY {
		t.Errorf("operation type %v", got.GetOperationType())
	}
	if !gkeEventually(90*time.Second, func() bool { return gkeProbeServes(kc) != nil }) {
		t.Fatal("the deny-all NetworkPolicy is not enforced with network policy enabled")
	}
	op, err = g.SetNetworkPolicy(ctx, &containerpb.SetNetworkPolicyRequest{Name: name, NetworkPolicy: &containerpb.NetworkPolicy{Enabled: false}})
	if err != nil {
		t.Fatal(err)
	}
	gkeWait(t, g, project, zone, op)
	if !gkeEventually(90*time.Second, func() bool { return gkeProbeServes(kc) == nil }) {
		t.Fatal("the deny-all NetworkPolicy is still enforced with network policy disabled")
	}
	c, _ = g.GetCluster(ctx, &containerpb.GetClusterRequest{Name: name})
	if c.GetNetworkPolicy().GetEnabled() || c.GetAddonsConfig().GetNetworkPolicyConfig().GetDisabled() {
		t.Errorf("network policy = %v, add-on %v", c.GetNetworkPolicy(), c.GetAddonsConfig().GetNetworkPolicyConfig())
	}

	// Legacy ABAC.
	if got := gkeCanI(kc, "list", "secrets"); got != "no" {
		t.Fatalf("before legacy ABAC, can the default service account list secrets? %q", got)
	}
	op, err = g.SetLegacyAbac(ctx, &containerpb.SetLegacyAbacRequest{Name: name, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	gkeWait(t, g, project, zone, op)
	if got := gkeCanI(kc, "list", "secrets"); got != "yes" {
		t.Errorf("with legacy ABAC, can the default service account list secrets? %q", got)
	}
	if c, _ = g.GetCluster(ctx, &containerpb.GetClusterRequest{Name: name}); !c.GetLegacyAbac().GetEnabled() {
		t.Errorf("legacy ABAC = %v", c.GetLegacyAbac())
	}
	op, err = r.SetLegacyAbac(ctx, &containerpb.SetLegacyAbacRequest{Name: name, Enabled: false})
	if err != nil {
		t.Fatal(err)
	}
	gkeWait(t, g, project, zone, op)
	if got := gkeCanI(kc, "list", "secrets"); got != "no" {
		t.Errorf("after legacy ABAC is disabled, can the default service account list secrets? %q", got)
	}

	// GetJSONWebKeys: the API server's issuer keys.
	jwks := gkeKubectl(t, kc, "", "get", "--raw", "/openid/v1/jwks")
	for _, cl := range []struct {
		n string
		f func() (*containerpb.GetJSONWebKeysResponse, error)
	}{{"gRPC", func() (*containerpb.GetJSONWebKeysResponse, error) {
		return g.GetJSONWebKeys(ctx, &containerpb.GetJSONWebKeysRequest{Parent: name})
	}}, {"REST", func() (*containerpb.GetJSONWebKeysResponse, error) {
		return r.GetJSONWebKeys(ctx, &containerpb.GetJSONWebKeysRequest{Parent: name})
	}}} {
		keys, err := cl.f()
		if err != nil {
			t.Fatalf("GetJSONWebKeys over %s: %v", cl.n, err)
		}
		if len(keys.GetKeys()) == 0 || keys.Keys[0].GetKty() != "RSA" || !strings.Contains(jwks, keys.Keys[0].GetKid()) || !strings.Contains(jwks, keys.Keys[0].GetN()) {
			t.Errorf("GetJSONWebKeys over %s = %v; the API server's: %s", cl.n, keys, jwks)
		}
	}

	// CheckAutopilotCompatibility.
	gkeKubectl(t, kc, `apiVersion: v1
kind: Pod
metadata: {name: priv, namespace: upgrade}
spec:
  containers:
  - name: pause
    image: registry.k8s.io/pause:3.10
    imagePullPolicy: Never
    securityContext: {privileged: true}
`, "apply", "-f", "-")
	compat, err := r.CheckAutopilotCompatibility(ctx, &containerpb.CheckAutopilotCompatibilityRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, i := range compat.GetIssues() {
		if i.GetConstraintType() == "autogke-disallow-privilege" && strings.Join(i.GetSubjects(), ",") == "upgrade/priv" &&
			i.GetIncompatibilityType() == containerpb.AutopilotCompatibilityIssue_INCOMPATIBILITY {
			found = true
		}
	}
	if !found || compat.GetSummary() == "" {
		t.Errorf("CheckAutopilotCompatibility = %v, want the privileged pod", compat)
	}
	gkeKubectl(t, kc, "", "-n", "upgrade", "delete", "pod", "priv", "--wait=false")

	// Node management.
	pool := name + "/nodePools/default-pool"
	_, err = g.SetNodePoolManagement(ctx, &containerpb.SetNodePoolManagementRequest{Name: pool, Management: &containerpb.NodeManagement{AutoUpgrade: false, AutoRepair: true}})
	wantPrivateCACode(t, "auto-upgrade off on a release channel", err, codes.InvalidArgument)
	op, err = g.SetNodePoolManagement(ctx, &containerpb.SetNodePoolManagementRequest{Name: pool, Management: &containerpb.NodeManagement{AutoUpgrade: true, AutoRepair: false}})
	if err != nil {
		t.Fatal(err)
	}
	if got := gkeWait(t, g, project, zone, op); got.GetOperationType() != containerpb.Operation_SET_NODE_POOL_MANAGEMENT {
		t.Errorf("operation type %v", got.GetOperationType())
	}
	if np, _ := g.GetNodePool(ctx, &containerpb.GetNodePoolRequest{Name: pool}); np.GetManagement().GetAutoRepair() || !np.GetManagement().GetAutoUpgrade() {
		t.Errorf("management = %v", np.GetManagement())
	}

	// Autoscaling: a pod no node has room for adds a node.
	_, err = g.SetNodePoolAutoscaling(ctx, &containerpb.SetNodePoolAutoscalingRequest{Name: pool,
		Autoscaling: &containerpb.NodePoolAutoscaling{Enabled: true, MinNodeCount: 2, MaxNodeCount: 1}})
	wantPrivateCACode(t, "a minimum above the maximum", err, codes.InvalidArgument)
	op, err = g.SetNodePoolAutoscaling(ctx, &containerpb.SetNodePoolAutoscalingRequest{Name: pool,
		Autoscaling: &containerpb.NodePoolAutoscaling{Enabled: true, MinNodeCount: 1, MaxNodeCount: 2}})
	if err != nil {
		t.Fatal(err)
	}
	gkeWait(t, g, project, zone, op)
	cpu := gkeKubectl(t, kc, "", "get", "nodes", "-l", "cloud.google.com/gke-nodepool=default-pool", "-o", "jsonpath={.items[0].status.allocatable.cpu}")
	cores, err := strconv.Atoi(strings.TrimSuffix(cpu, "m"))
	if err != nil {
		t.Fatalf("allocatable cpu %q", cpu)
	}
	if !strings.HasSuffix(cpu, "m") {
		cores *= 1000
	}
	request := strconv.Itoa(cores*6/10) + "m"
	gkeKubectl(t, kc, `apiVersion: apps/v1
kind: Deployment
metadata: {name: load, namespace: default}
spec:
  replicas: 2
  selector: {matchLabels: {app: load}}
  template:
    metadata: {labels: {app: load}}
    spec:
      containers:
      - name: pause
        image: registry.k8s.io/pause:3.10
        imagePullPolicy: Never
        resources: {requests: {cpu: `+request+`}}
`, "apply", "-f", "-")
	if !gkeEventually(4*time.Minute, func() bool { return len(gkeNodes(t, kc, "default-pool")) == 2 }) {
		t.Fatalf("the autoscaler did not add a node for the pending pod: nodes %v", gkeNodes(t, kc, "default-pool"))
	}
	gkeKubectl(t, kc, "", "rollout", "status", "deployment/load", "--timeout=180s")
	// The pod can run on the new node a poll before the scale-up, which
	// waits for the node to be Ready, records the pool's new size.
	var np *containerpb.NodePool
	if !gkeEventually(30*time.Second, func() bool {
		np, _ = g.GetNodePool(ctx, &containerpb.GetNodePoolRequest{Name: pool})
		return np.GetInitialNodeCount() == 2
	}) || np.GetAutoscaling().GetMaxNodeCount() != 2 || np.GetAutoscaling().GetLocationPolicy() != containerpb.NodePoolAutoscaling_BALANCED {
		t.Errorf("the pool after the scale-up: autoscaling %v, initialNodeCount %d", np.GetAutoscaling(), np.GetInitialNodeCount())
	}
	gkeKubectl(t, kc, "", "scale", "deployment/load", "--replicas=3")
	time.Sleep(30 * time.Second)
	if n := len(gkeNodes(t, kc, "default-pool")); n != 2 {
		t.Errorf("the pool grew past its maximum: %d nodes", n)
	}
	gkeKubectl(t, kc, "", "delete", "deployment", "load")
	op, err = g.SetNodePoolAutoscaling(ctx, &containerpb.SetNodePoolAutoscalingRequest{Name: pool, Autoscaling: &containerpb.NodePoolAutoscaling{Enabled: false}})
	if err != nil {
		t.Fatal(err)
	}
	gkeWait(t, g, project, zone, op)

	// ListUsableSubnetworks.
	for _, cl := range []struct {
		n string
		f func() (int, string, error)
	}{{"gRPC", func() (int, string, error) {
		it := g.ListUsableSubnetworks(ctx, &containerpb.ListUsableSubnetworksRequest{Parent: "projects/" + project})
		n, first := 0, ""
		for {
			s, err := it.Next()
			if err != nil {
				if err == iterator.Done {
					return n, first, nil
				}
				return n, first, err
			}
			if first == "" {
				first = s.GetSubnetwork() + " " + s.GetNetwork() + " " + s.GetIpCidrRange()
			}
			n++
		}
	}}} {
		n, first, err := cl.f()
		if err != nil {
			t.Fatalf("ListUsableSubnetworks over %s: %v", cl.n, err)
		}
		if os.Getenv(EnvCompute) != "" && (n == 0 || !strings.HasPrefix(first, "projects/"+project+"/regions/")) {
			t.Errorf("ListUsableSubnetworks over %s: %d subnetworks, the first %q", cl.n, n, first)
		}
	}
	_, err = r.ListUsableSubnetworks(ctx, &containerpb.ListUsableSubnetworksRequest{Parent: "projects/" + project, Filter: "name=default"}).Next()
	wantPrivateCACode(t, "a filter other than networkProjectId", err, codes.InvalidArgument)

	// SetMasterAuth: basic authentication is gone.
	op, err = g.SetMasterAuth(ctx, &containerpb.SetMasterAuthRequest{Name: name, Action: containerpb.SetMasterAuthRequest_SET_USERNAME, Update: &containerpb.MasterAuth{}})
	if err != nil {
		t.Fatal(err)
	}
	gkeWait(t, g, project, zone, op)
	_, err = g.SetMasterAuth(ctx, &containerpb.SetMasterAuthRequest{Name: name, Action: containerpb.SetMasterAuthRequest_GENERATE_PASSWORD, Update: &containerpb.MasterAuth{}})
	wantPrivateCACode(t, "a basic-auth password", err, codes.InvalidArgument)
}

// TestGcloudContainerClusterSettings (#2100): gcloud, configured by
// `cloudburrow gcloud-setup`, changes a cluster's settings, each with its
// effect: --enable-legacy-authorization lets the default service account
// list every secret and --no-enable-legacy-authorization takes it back;
// --update-addons=NetworkPolicy=ENABLED then --enable-network-policy
// enable network policy; node-pools update sets auto-repair and the
// autoscaling limits; subnets list-usable lists the project's
// subnetworks; check-autopilot-compatibility reports.
// covers: google.container.v1.ClusterManager/SetLegacyAbac, google.container.v1.ClusterManager/SetNetworkPolicy, google.container.v1.ClusterManager/SetNodePoolManagement, google.container.v1.ClusterManager/ListUsableSubnetworks, google.container.v1.ClusterManager/CheckAutopilotCompatibility
func TestGcloudContainerClusterSettings(t *testing.T) {
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
	gdir := t.TempDir()
	cfg := gcloudSetup(t, cli, gdir)
	kubeconfig := t.TempDir() + "/kubeconfig"
	gc := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(gcloud, append(args, "--project", h.Project(), "--quiet")...)
		cmd.Env = append(gcloudTargetEnv(noGoogleEgress()), "CLOUDSDK_CONFIG="+gdir, "CLOUDSDK_ACTIVE_CONFIG_NAME="+cfg,
			"CLOUDSDK_CONTAINER_USE_CLIENT_CERTIFICATE=true", "KUBECONFIG="+kubeconfig)
		b, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("gcloud %s: %v\n%s", strings.Join(args, " "), err, b)
		}
		return string(b)
	}
	const zone, cluster = "us-central1-c", "gc-settings-2100"
	full := "projects/" + h.Project() + "/locations/" + zone + "/clusters/" + cluster
	t.Cleanup(func() {
		if op, err := g.DeleteCluster(context.Background(), &containerpb.DeleteClusterRequest{Name: full}); err == nil {
			gkeWait(t, g, h.Project(), zone, op)
		}
	})
	gc("container", "clusters", "create", cluster, "--zone", zone, "--num-nodes", "1", "--issue-client-certificate")
	gc("container", "clusters", "get-credentials", cluster, "--zone", zone)

	gc("container", "clusters", "update", cluster, "--zone", zone, "--enable-legacy-authorization")
	if got := gkeCanI(kubeconfig, "list", "secrets"); got != "yes" {
		t.Errorf("after --enable-legacy-authorization, can the default service account list secrets? %q", got)
	}
	if got := gc("container", "clusters", "describe", cluster, "--zone", zone, "--format=value(legacyAbac.enabled)"); strings.TrimSpace(got) != "True" {
		t.Errorf("legacyAbac.enabled = %q", got)
	}
	gc("container", "clusters", "update", cluster, "--zone", zone, "--no-enable-legacy-authorization")
	if got := gkeCanI(kubeconfig, "list", "secrets"); got != "no" {
		t.Errorf("after --no-enable-legacy-authorization, can the default service account list secrets? %q", got)
	}

	gc("container", "clusters", "update", cluster, "--zone", zone, "--update-addons=NetworkPolicy=ENABLED")
	gc("container", "clusters", "update", cluster, "--zone", zone, "--enable-network-policy")
	if got := gc("container", "clusters", "describe", cluster, "--zone", zone, "--format=value(networkPolicy.enabled,networkPolicy.provider)"); !strings.Contains(got, "True") || !strings.Contains(got, "CALICO") {
		t.Errorf("networkPolicy = %q", got)
	}

	gc("container", "node-pools", "update", "default-pool", "--cluster", cluster, "--zone", zone, "--no-enable-autorepair")
	if got := gc("container", "node-pools", "describe", "default-pool", "--cluster", cluster, "--zone", zone, "--format=value(management.autoRepair)"); strings.TrimSpace(got) == "True" {
		t.Errorf("management after --no-enable-autorepair = %q", got)
	}
	gc("container", "node-pools", "update", "default-pool", "--cluster", cluster, "--zone", zone, "--enable-autoscaling", "--min-nodes", "1", "--max-nodes", "2")
	if got := gc("container", "node-pools", "describe", "default-pool", "--cluster", cluster, "--zone", zone, "--format=value(autoscaling.enabled,autoscaling.maxNodeCount)"); !strings.Contains(got, "True") || !strings.Contains(got, "2") {
		t.Errorf("autoscaling = %q", got)
	}

	if got := gc("container", "subnets", "list-usable", "--format=json"); os.Getenv(EnvCompute) != "" && !strings.Contains(got, "/subnetworks/") {
		t.Errorf("subnets list-usable:\n%s", got)
	}
	if got := gc("container", "clusters", "check-autopilot-compatibility", cluster, "--zone", zone, "--format=json"); !strings.HasPrefix(strings.TrimSpace(got), "[") {
		t.Errorf("check-autopilot-compatibility:\n%s", got)
	}
}
