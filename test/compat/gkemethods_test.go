//go:build compat

package compat

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"
	"testing"
	"time"

	container "cloud.google.com/go/container/apiv1"
	"cloud.google.com/go/container/apiv1/containerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/cloudburrow/cloudburrow/internal/service/gke"
)

// gkeTLSGet is a GET of the cluster's API server at path, trusting only
// the CA PEM ca and authenticating with the cluster's client certificate.
func gkeTLSGet(c *containerpb.Cluster, ca []byte, path string) (int, []byte, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return 0, nil, errNoCA
	}
	cert, _ := base64.StdEncoding.DecodeString(c.GetMasterAuth().GetClientCertificate())
	key, _ := base64.StdEncoding.DecodeString(c.GetMasterAuth().GetClientKey())
	pair, err := tls.X509KeyPair(cert, key)
	if err != nil {
		return 0, nil, err
	}
	hc := &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{
		RootCAs: pool, Certificates: []tls.Certificate{pair}}}}
	resp, err := hc.Get("https://" + c.GetEndpoint() + path)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b, nil
}

type gkeErr string

func (e gkeErr) Error() string { return string(e) }

const errNoCA = gkeErr("no CA certificate in the PEM")

func gkeCA(t *testing.T, c *containerpb.Cluster) []byte {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(c.GetMasterAuth().GetClusterCaCertificate())
	if err != nil || len(b) == 0 {
		t.Fatalf("cluster CA: %v", err)
	}
	return b
}

// gkeNodeZones is each node of the pool's zone label.
func gkeNodeZones(t *testing.T, kubeconfig, pool string) []string {
	t.Helper()
	out, err := exec.Command("kubectl", "--kubeconfig", kubeconfig, "get", "nodes", "-l", "cloud.google.com/gke-nodepool="+pool,
		"-o", `jsonpath={range .items[*]}{.metadata.labels.topology\.kubernetes\.io/zone}{"\n"}{end}`).CombinedOutput()
	if err != nil {
		t.Fatalf("kubectl get nodes: %v\n%s", err, out)
	}
	z := strings.Fields(string(out))
	sort.Strings(z)
	return z
}

// TestGKERotationLocationsAndUpgradeInfo (#2100): the official client, over
// gRPC and REST, on a zonal Standard cluster:
//   - FetchClusterUpgradeInfo and FetchNodePoolUpgradeInfo report
//     automatic upgrades paused by the cluster's "no upgrades" exclusion;
//   - SetLocations adds a zone (the pool gets its node count there, and
//     the nodes carry topology.kubernetes.io/zone), refuses zones without
//     the cluster's own or outside its region, and removes the zone again;
//   - StartIPRotation makes a new cluster CA and a new endpoint: during
//     the rotation a client that trusts only the old CA and one that trusts
//     only the new one both reach the API server, at the new endpoint and
//     the old one; CompleteIPRotation drops the old CA and the old
//     endpoint, after which only the new-CA client does, only at the new
//     endpoint, and the old client certificate is refused.
//   - the endpoint rules dropped inside the control plane (as a restarted
//     container loses them) are put back within a minute.
//
// covers: google.container.v1.ClusterManager/FetchClusterUpgradeInfo, google.container.v1.ClusterManager/FetchNodePoolUpgradeInfo, google.container.v1.ClusterManager/SetLocations, google.container.v1.ClusterManager/StartIPRotation, google.container.v1.ClusterManager/CompleteIPRotation
func TestGKERotationLocationsAndUpgradeInfo(t *testing.T) {
	h := New(t)
	g, r := gkeClients(t, h)
	if _, err := exec.LookPath("kubectl"); err != nil {
		t.Skip("kubectl is required")
	}
	ctx := context.Background()
	project := h.Project()
	const zone, cluster = "us-central1-a", "rotate-2100"
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

	// Upgrade information.
	info, err := r.FetchClusterUpgradeInfo(ctx, &containerpb.FetchClusterUpgradeInfoRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	if len(info.GetAutoUpgradeStatus()) != 1 || info.GetAutoUpgradeStatus()[0] != containerpb.ClusterUpgradeInfo_UPGRADE_PAUSED ||
		len(info.GetPausedReason()) != 1 || info.GetPausedReason()[0] != containerpb.ClusterUpgradeInfo_MAINTENANCE_EXCLUSION_NO_UPGRADES {
		t.Errorf("cluster upgrade info under a \"no upgrades\" exclusion = %v", info)
	}
	pinfo, err := g.FetchNodePoolUpgradeInfo(ctx, &containerpb.FetchNodePoolUpgradeInfoRequest{Name: name + "/nodePools/default-pool"})
	if err != nil {
		t.Fatal(err)
	}
	if len(pinfo.GetAutoUpgradeStatus()) != 1 || pinfo.GetAutoUpgradeStatus()[0] != containerpb.NodePoolUpgradeInfo_UPGRADE_PAUSED {
		t.Errorf("node pool upgrade info = %v", pinfo)
	}
	_, err = g.FetchNodePoolUpgradeInfo(ctx, &containerpb.FetchNodePoolUpgradeInfoRequest{Name: name + "/nodePools/nope"})
	wantPrivateCACode(t, "an unknown pool's upgrade info", err, codes.NotFound)

	c, err := g.GetCluster(ctx, &containerpb.GetClusterRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	kc := gkeKubeconfig(t, c)

	// Zones.
	_, err = g.SetLocations(ctx, &containerpb.SetLocationsRequest{Name: name, Locations: []string{"us-central1-b"}})
	wantPrivateCACode(t, "zones without the cluster's own", err, codes.InvalidArgument)
	_, err = g.SetLocations(ctx, &containerpb.SetLocationsRequest{Name: name, Locations: []string{zone, "us-east1-b"}})
	wantPrivateCACode(t, "a zone of another region", err, codes.InvalidArgument)
	op, err = r.SetLocations(ctx, &containerpb.SetLocationsRequest{Name: name, Locations: []string{zone, "us-central1-b"}})
	if err != nil {
		t.Fatal(err)
	}
	gkeWait(t, g, project, zone, op)
	c, _ = g.GetCluster(ctx, &containerpb.GetClusterRequest{Name: name})
	if got := strings.Join(c.GetLocations(), ","); got != "us-central1-a,us-central1-b" {
		t.Errorf("locations = %s", got)
	}
	if got := strings.Join(c.GetNodePools()[0].GetLocations(), ","); got != "us-central1-a,us-central1-b" {
		t.Errorf("the pool's locations = %s", got)
	}
	if got := strings.Join(gkeNodeZones(t, kc, "default-pool"), ","); got != "us-central1-a,us-central1-b" {
		t.Errorf("the pool's nodes' zones = %s, want one node in each zone", got)
	}
	op, err = g.SetLocations(ctx, &containerpb.SetLocationsRequest{Name: name, Locations: []string{zone}})
	if err != nil {
		t.Fatal(err)
	}
	gkeWait(t, g, project, zone, op)
	if got := strings.Join(gkeNodeZones(t, kc, "default-pool"), ","); got != zone {
		t.Errorf("after removing us-central1-b, the nodes' zones = %s", got)
	}

	// Rotation.
	oldCA := gkeCA(t, c)
	op, err = g.StartIPRotation(ctx, &containerpb.StartIPRotationRequest{Name: name, RotateCredentials: true})
	if err != nil {
		t.Fatal(err)
	}
	gkeWait(t, g, project, zone, op)
	_, err = g.StartIPRotation(ctx, &containerpb.StartIPRotationRequest{Name: name})
	wantPrivateCACode(t, "a second rotation", err, codes.FailedPrecondition)
	during, err := g.GetCluster(ctx, &containerpb.GetClusterRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	newCA := gkeCA(t, during)
	if string(newCA) == string(oldCA) {
		t.Fatal("the cluster CA did not change when the rotation started")
	}
	for what, ca := range map[string][]byte{"the old CA": oldCA, "the new CA": newCA} {
		if code, body, err := gkeTLSGet(during, ca, "/api/v1/namespaces/default"); err != nil || code != http.StatusOK {
			t.Errorf("during the rotation, a client trusting only %s: %d %s %v", what, code, body, err)
		}
	}
	// The control plane serves on a new address and on the old one.
	if during.GetEndpoint() == c.GetEndpoint() {
		t.Errorf("the endpoint did not change when the rotation started: %s", c.GetEndpoint())
	}
	atOld := proto.Clone(during).(*containerpb.Cluster)
	atOld.Endpoint = c.GetEndpoint()
	if code, body, err := gkeTLSGet(atOld, newCA, "/api/v1/namespaces/default"); err != nil || code != http.StatusOK {
		t.Errorf("during the rotation, the old endpoint %s: %d %s %v", c.GetEndpoint(), code, body, err)
	}
	// The credentials issued before the rotation are still accepted.
	if code, _, err := gkeTLSGet(c, newCA, "/api/v1/namespaces/default"); err != nil || code != http.StatusOK {
		t.Errorf("during the rotation, the old client certificate: %d %v", code, err)
	}
	op, err = r.CompleteIPRotation(ctx, &containerpb.CompleteIPRotationRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	gkeWait(t, g, project, zone, op)
	_, err = g.CompleteIPRotation(ctx, &containerpb.CompleteIPRotationRequest{Name: name})
	wantPrivateCACode(t, "completing with no rotation", err, codes.FailedPrecondition)
	after, err := g.GetCluster(ctx, &containerpb.GetClusterRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	if string(gkeCA(t, after)) != string(newCA) {
		t.Error("the cluster CA changed again when the rotation completed")
	}
	if code, body, err := gkeTLSGet(after, newCA, "/api/v1/namespaces/default"); err != nil || code != http.StatusOK {
		t.Errorf("after the rotation, a client trusting only the new CA: %d %s %v", code, body, err)
	}
	if after.GetEndpoint() != during.GetEndpoint() {
		t.Errorf("the endpoint after the rotation = %s, want the new one %s", after.GetEndpoint(), during.GetEndpoint())
	}
	atOld = proto.Clone(after).(*containerpb.Cluster)
	atOld.Endpoint = c.GetEndpoint()
	if _, _, err := gkeTLSGet(atOld, newCA, "/api/v1/namespaces/default"); err == nil {
		t.Errorf("after the rotation, the old endpoint %s still serves", c.GetEndpoint())
	}
	if _, _, err := gkeTLSGet(after, oldCA, "/api/v1/namespaces/default"); err == nil {
		t.Error("after the rotation, a client trusting only the old CA still verified the API server")
	}
	oldCert := proto.Clone(c).(*containerpb.Cluster)
	oldCert.Endpoint = after.GetEndpoint()
	// A certificate the API server no longer trusts authenticates no one:
	// the request is anonymous's, which may not read a namespace.
	if code, _, err := gkeTLSGet(oldCert, newCA, "/api/v1/namespaces/default"); err != nil || (code != http.StatusUnauthorized && code != http.StatusForbidden) {
		t.Errorf("after the rotation, the client certificate of the old CA: %d %v, want 401 or 403", code, err)
	}
	// The nodes and the pods trust the new CA: the pool's node is Ready
	// and kube-system's pods run again.
	akc := gkeKubeconfig(t, after)
	if !gkeEventually(3*time.Minute, func() bool {
		out, err := exec.Command("kubectl", "--kubeconfig", akc, "get", "nodes", "-o",
			`jsonpath={range .items[*]}{.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}`).CombinedOutput()
		return err == nil && !strings.Contains(string(out), "False") && !strings.Contains(string(out), "Unknown") && strings.Contains(string(out), "True")
	}) {
		t.Error("the nodes are not Ready after the rotation")
	}
	if !gkeEventually(3*time.Minute, func() bool {
		out, err := exec.Command("kubectl", "--kubeconfig", akc, "-n", "kube-system", "get", "pods", "-l", "k8s-app=kube-dns", "-o",
			`jsonpath={.items[*].status.containerStatuses[*].ready}`).CombinedOutput()
		return err == nil && strings.Contains(string(out), "true") && !strings.Contains(string(out), "false")
	}) {
		out, _ := exec.Command("kubectl", "--kubeconfig", akc, "-n", "kube-system", "describe", "pods", "-l", "k8s-app=kube-dns").CombinedOutput()
		logs, _ := exec.Command("kubectl", "--kubeconfig", akc, "-n", "kube-system", "logs", "-l", "k8s-app=kube-dns", "--tail", "20").CombinedOutput()
		t.Errorf("CoreDNS is not ready after the rotation:\n%s\n%s", out, logs)
	}

	// Last, as it cuts the host off the cluster for a while:
	// the rules that serve the new endpoint and refuse the old one live in
	// the control plane's network namespace, which a restarted container
	// loses: dropped there, they are put back within a minute.
	instance := strings.TrimSpace(os.Getenv("CLOUDBURROW_TEST_CLUSTER"))
	if instance == "" {
		t.Skip("CLOUDBURROW_TEST_CLUSTER is not set: the cluster's control plane container is not known")
	}
	drop := `iptables -t nat -D PREROUTING -p tcp --dport 6444 -j REDIRECT --to-ports 6443 && ` +
		`iptables -S INPUT | grep -e '--dport 6443' | sed 's/^-A /-D /' | while read -r rule; do eval "iptables $rule"; done`
	if out, err := exec.Command("docker", "exec", gke.KindName(instance, name)+"-control-plane", "sh", "-c", drop).CombinedOutput(); err != nil {
		t.Fatalf("drop the control plane's endpoint rules: %v\n%s", err, out)
	}
	if code, _, err := gkeTLSGet(after, newCA, "/api/v1/namespaces/default"); err == nil && code == http.StatusOK {
		t.Error("the new endpoint served with its rule dropped")
	}
	if !gkeEventually(90*time.Second, func() bool {
		_, _, oldErr := gkeTLSGet(atOld, newCA, "/api/v1/namespaces/default")
		code, _, newErr := gkeTLSGet(after, newCA, "/api/v1/namespaces/default")
		return oldErr != nil && newErr == nil && code == http.StatusOK
	}) {
		t.Error("after the control plane lost its rules, the new endpoint does not serve or the old one serves again")
	}
}

// TestGKERollbackSafeUpgrade (#2100): a minor control plane upgrade with
// desired_rollback_safe_upgrade is GKE's two steps: step one moves the
// control plane to the newer version and keeps the previous minor
// version's behaviour (the API server reports the previous minor as its
// emulated version), and FetchClusterUpgradeInfo reports the rollback-safe
// status and the release schedule's end-of-support dates; during the soak
// a node upgrade past the emulated version is refused, and an upgrade to
// the previous version rolls the control plane back (the API server and
// the control plane's kubelet run it again, and the status is gone);
// upgraded again, CompleteControlPlaneUpgrade is step two, after which
// the API server emulates nothing, the status is gone and a downgrade is
// refused. A soak shorter than six
// hours is refused, and so is completing with nothing to complete.
//
// covers: google.container.v1.ClusterManager/CompleteControlPlaneUpgrade
func TestGKERollbackSafeUpgrade(t *testing.T) {
	h := New(t)
	g, r := gkeClients(t, h)
	if _, err := exec.LookPath("kubectl"); err != nil {
		t.Skip("kubectl is required")
	}
	ctx := context.Background()
	project := h.Project()
	const zone, cluster = "us-central1-b", "rollback-safe-2100"
	parent := "projects/" + project + "/locations/" + zone
	name := parent + "/clusters/" + cluster
	older, newer, _ := gkeUpgradeVersions(t, g, parent)
	t.Cleanup(func() {
		if op, err := g.DeleteCluster(context.Background(), &containerpb.DeleteClusterRequest{Name: name}); err == nil {
			gkeWait(t, g, project, zone, op)
		}
	})
	op, err := g.CreateCluster(ctx, &containerpb.CreateClusterRequest{Parent: parent, Cluster: &containerpb.Cluster{
		Name: cluster, InitialClusterVersion: older, MaintenancePolicy: gkeNoAutoUpgrade(),
		MasterAuth: &containerpb.MasterAuth{ClientCertificateConfig: &containerpb.ClientCertificateConfig{IssueClientCertificate: true}},
		NodePools:  []*containerpb.NodePool{{Name: "default-pool", InitialNodeCount: 1}}}})
	if err != nil {
		t.Fatal(err)
	}
	gkeWait(t, g, project, zone, op)
	_, err = g.CompleteControlPlaneUpgrade(ctx, &containerpb.CompleteControlPlaneUpgradeRequest{Name: name})
	wantPrivateCACode(t, "completing with no rollback-safe upgrade", err, codes.FailedPrecondition)
	_, err = g.UpdateCluster(ctx, &containerpb.UpdateClusterRequest{Name: name, Update: &containerpb.ClusterUpdate{
		DesiredMasterVersion: newer, DesiredRollbackSafeUpgrade: &containerpb.RollbackSafeUpgrade{ControlPlaneSoakDuration: durationpb.New(time.Hour)}}})
	wantPrivateCACode(t, "a one-hour soak", err, codes.InvalidArgument)
	op, err = r.UpdateCluster(ctx, &containerpb.UpdateClusterRequest{Name: name, Update: &containerpb.ClusterUpdate{
		DesiredMasterVersion: newer, DesiredRollbackSafeUpgrade: &containerpb.RollbackSafeUpgrade{ControlPlaneSoakDuration: durationpb.New(6 * time.Hour)}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := gkeWaitLong(t, g, project, zone, op, 15*time.Minute); got.GetOperationType() != containerpb.Operation_UPGRADE_MASTER {
		t.Errorf("step one's operation type %v", got.GetOperationType())
	}
	c, err := g.GetCluster(ctx, &containerpb.GetClusterRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	if c.GetCurrentMasterVersion() != newer || c.GetCurrentEmulatedVersion() != older {
		t.Errorf("after step one: master %s, emulated %s; want %s emulating %s", c.GetCurrentMasterVersion(), c.GetCurrentEmulatedVersion(), newer, older)
	}
	kc := gkeKubeconfig(t, c)
	minorOf := func(v string) string { return strings.Split(v, ".")[1] }
	emulation := func() string {
		out, err := exec.Command("kubectl", "--kubeconfig", kc, "get", "--raw", "/version").CombinedOutput()
		if err != nil {
			t.Fatalf("GET /version: %v\n%s", err, out)
		}
		var v struct {
			Minor          string `json:"minor"`
			EmulationMinor string `json:"emulationMinor"`
		}
		_ = json.Unmarshal(out, &v)
		return v.Minor + "/" + v.EmulationMinor
	}
	if got, want := emulation(), minorOf(newer)+"/"+minorOf(older); got != want {
		t.Errorf("during the soak the API server's minor/emulated minor = %s, want %s", got, want)
	}
	info, err := g.FetchClusterUpgradeInfo(ctx, &containerpb.FetchClusterUpgradeInfoRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	rs := info.GetRollbackSafeUpgradeStatus()
	if rs.GetMode() != containerpb.RollbackSafeUpgradeStatus_KCP_MINOR_UPGRADE_ROLLBACK_SAFE_MODE || rs.GetPreviousVersion() != older ||
		rs.GetControlPlaneUpgradeRollbackEndTime().AsTime().Before(time.Now().Add(5*time.Hour)) {
		t.Errorf("rollback-safe status = %v", rs)
	}
	if d := info.GetUpgradeDetails(); len(d) == 0 || d[len(d)-1].GetInitialVersion() != older || d[len(d)-1].GetTargetVersion() != newer ||
		d[len(d)-1].GetState() != containerpb.UpgradeDetails_SUCCEEDED || d[len(d)-1].GetStartType() != containerpb.UpgradeDetails_MANUAL {
		t.Errorf("upgrade details = %v", d)
	}
	// The support dates are the release schedule's for the version's minor.
	if info.GetEndOfStandardSupportTimestamp() == "" || info.GetEndOfExtendedSupportTimestamp() == "" {
		t.Errorf("end of support = %q, %q", info.GetEndOfStandardSupportTimestamp(), info.GetEndOfExtendedSupportTimestamp())
	}
	// During the soak a node pool goes no further than the emulated
	// version, and the control plane back to no version but the exact
	// previous one.
	_, err = g.UpdateCluster(ctx, &containerpb.UpdateClusterRequest{Name: name, Update: &containerpb.ClusterUpdate{
		DesiredNodeVersion: newer, DesiredNodePoolId: "default-pool"}})
	wantPrivateCACode(t, "a node upgrade past the emulated version", err, codes.FailedPrecondition)
	// The rollback: the control plane runs the previous version again.
	op, err = g.UpdateCluster(ctx, &containerpb.UpdateClusterRequest{Name: name, Update: &containerpb.ClusterUpdate{DesiredMasterVersion: older}})
	if err != nil {
		t.Fatal(err)
	}
	if got := gkeWaitLong(t, g, project, zone, op, 10*time.Minute); got.GetOperationType() != containerpb.Operation_UPGRADE_MASTER || got.GetError() != nil {
		t.Fatalf("the rollback: %v %v", got.GetOperationType(), got.GetError())
	}
	c, _ = g.GetCluster(ctx, &containerpb.GetClusterRequest{Name: name})
	if c.GetCurrentMasterVersion() != older || c.GetCurrentEmulatedVersion() != older {
		t.Errorf("after the rollback: master %s, emulated %s; want %s", c.GetCurrentMasterVersion(), c.GetCurrentEmulatedVersion(), older)
	}
	if got := emulation(); got != minorOf(older)+"/" && got != minorOf(older)+"/"+minorOf(older) {
		t.Errorf("after the rollback the API server's minor/emulated minor = %s, want %s", got, minorOf(older))
	}
	cpKubelet := gkeKubectl(t, kc, "", "get", "nodes", "-l", "node-role.kubernetes.io/control-plane", "-o", "jsonpath={.items[0].status.nodeInfo.kubeletVersion}")
	if !strings.HasPrefix(older, strings.TrimPrefix(cpKubelet, "v")) {
		t.Errorf("after the rollback the control plane's kubelet is %s, want %s", cpKubelet, older)
	}
	info, _ = g.FetchClusterUpgradeInfo(ctx, &containerpb.FetchClusterUpgradeInfoRequest{Name: name})
	if info.GetRollbackSafeUpgradeStatus() != nil {
		t.Errorf("rollback-safe status after the rollback = %v", info.GetRollbackSafeUpgradeStatus())
	}
	if d := info.GetUpgradeDetails(); len(d) == 0 || d[len(d)-1].GetInitialVersion() != newer || d[len(d)-1].GetTargetVersion() != older ||
		d[len(d)-1].GetState() != containerpb.UpgradeDetails_SUCCEEDED {
		t.Errorf("upgrade details after the rollback = %v", d)
	}
	// Upgraded again, rollback-safe; then step two.
	op, err = g.UpdateCluster(ctx, &containerpb.UpdateClusterRequest{Name: name, Update: &containerpb.ClusterUpdate{
		DesiredMasterVersion: newer, DesiredRollbackSafeUpgrade: &containerpb.RollbackSafeUpgrade{ControlPlaneSoakDuration: durationpb.New(6 * time.Hour)}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := gkeWaitLong(t, g, project, zone, op, 15*time.Minute); got.GetError() != nil {
		t.Fatalf("the second step one: %v", got.GetError())
	}
	op, err = g.CompleteControlPlaneUpgrade(ctx, &containerpb.CompleteControlPlaneUpgradeRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	gkeWaitLong(t, g, project, zone, op, 6*time.Minute)
	if got, want := emulation(), minorOf(newer)+"/"+minorOf(newer); got != want && got != minorOf(newer)+"/" {
		t.Errorf("after step two the API server's minor/emulated minor = %s, want %s", got, want)
	}
	c, _ = g.GetCluster(ctx, &containerpb.GetClusterRequest{Name: name})
	if c.GetCurrentEmulatedVersion() != newer {
		t.Errorf("after step two the emulated version = %s, want %s", c.GetCurrentEmulatedVersion(), newer)
	}
	info, _ = g.FetchClusterUpgradeInfo(ctx, &containerpb.FetchClusterUpgradeInfoRequest{Name: name})
	if info.GetRollbackSafeUpgradeStatus() != nil {
		t.Errorf("rollback-safe status after step two = %v", info.GetRollbackSafeUpgradeStatus())
	}
	// No way back after step two.
	_, err = g.UpdateCluster(ctx, &containerpb.UpdateClusterRequest{Name: name, Update: &containerpb.ClusterUpdate{DesiredMasterVersion: older}})
	wantPrivateCACode(t, "a downgrade after step two", err, codes.InvalidArgument)
}

// gkeWaitLong is gkeWait with a longer deadline.
func gkeWaitLong(t *testing.T, g *container.ClusterManagerClient, project, location string, op *containerpb.Operation, d time.Duration) *containerpb.Operation {
	t.Helper()
	name := "projects/" + project + "/locations/" + location + "/operations/" + op.GetName()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		got, err := g.GetOperation(context.Background(), &containerpb.GetOperationRequest{Name: name})
		if err != nil {
			t.Fatalf("GetOperation %s: %v", name, err)
		}
		if got.GetStatus() == containerpb.Operation_DONE {
			if got.GetError() != nil {
				t.Fatalf("operation %s (%s) failed: %v", got.GetName(), got.GetOperationType(), got.GetError())
			}
			return got
		}
		time.Sleep(3 * time.Second)
	}
	t.Fatalf("operation %s did not finish in %s", name, d)
	return nil
}

// TestGcloudContainerRotationLocationsAndUpgradeInfo (#2100): gcloud
// container clusters and node-pools get-upgrade-info, clusters update --node-locations, update
// --start-credential-rotation and --complete-credential-rotation, and
// complete-control-plane-upgrade's refusal with nothing to complete.
//
// covers: google.container.v1.ClusterManager/FetchClusterUpgradeInfo, google.container.v1.ClusterManager/FetchNodePoolUpgradeInfo, google.container.v1.ClusterManager/SetLocations, google.container.v1.ClusterManager/StartIPRotation, google.container.v1.ClusterManager/CompleteIPRotation, google.container.v1.ClusterManager/CompleteControlPlaneUpgrade
func TestGcloudContainerRotationLocationsAndUpgradeInfo(t *testing.T) {
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
	run := func(args ...string) (string, error) {
		cmd := exec.Command(gcloud, append(args, "--project", h.Project(), "--quiet")...)
		cmd.Env = append(gcloudTargetEnv(noGoogleEgress()), "CLOUDSDK_CONFIG="+gdir, "CLOUDSDK_ACTIVE_CONFIG_NAME="+cfg,
			"CLOUDSDK_CONTAINER_USE_CLIENT_CERTIFICATE=true", "KUBECONFIG="+kubeconfig)
		b, err := cmd.CombinedOutput()
		return string(b), err
	}
	gc := func(args ...string) string {
		t.Helper()
		out, err := run(args...)
		if err != nil {
			t.Fatalf("gcloud %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return out
	}
	const zone, cluster = "us-central1-c", "gc-rotate-2100"
	full := "projects/" + h.Project() + "/locations/" + zone + "/clusters/" + cluster
	t.Cleanup(func() {
		if op, err := g.DeleteCluster(context.Background(), &containerpb.DeleteClusterRequest{Name: full}); err == nil {
			gkeWait(t, g, h.Project(), zone, op)
		}
	})
	gc("container", "clusters", "create", cluster, "--zone", zone, "--num-nodes", "1", "--issue-client-certificate")
	if out := gc("container", "clusters", "get-upgrade-info", cluster, "--zone", zone, "--format=value(autoUpgradeStatus)"); !strings.Contains(out, "ACTIVE") && !strings.Contains(out, "PAUSED") {
		t.Errorf("get-upgrade-info autoUpgradeStatus = %q", out)
	}
	if out := gc("container", "node-pools", "get-upgrade-info", "default-pool", "--cluster", cluster, "--zone", zone, "--format=value(autoUpgradeStatus)"); !strings.Contains(out, "ACTIVE") && !strings.Contains(out, "PAUSED") {
		t.Errorf("node-pools get-upgrade-info autoUpgradeStatus = %q", out)
	}
	if out, err := run("container", "clusters", "complete-control-plane-upgrade", cluster, "--zone", zone); err == nil {
		t.Errorf("complete-control-plane-upgrade with nothing to complete succeeded: %s", out)
	}

	gc("container", "clusters", "update", cluster, "--zone", zone, "--node-locations", zone+",us-central1-f")
	if got := gc("container", "clusters", "describe", cluster, "--zone", zone, "--format=value(locations)"); !strings.Contains(got, "us-central1-f") {
		t.Errorf("locations after --node-locations = %q", got)
	}
	if got := gc("container", "node-pools", "describe", "default-pool", "--cluster", cluster, "--zone", zone, "--format=value(locations)"); !strings.Contains(got, "us-central1-f") {
		t.Errorf("the pool's locations after --node-locations = %q", got)
	}
	gc("container", "clusters", "update", cluster, "--zone", zone, "--node-locations", zone)

	before := gc("container", "clusters", "describe", cluster, "--zone", zone, "--format=value(masterAuth.clusterCaCertificate)")
	gc("container", "clusters", "update", cluster, "--zone", zone, "--start-credential-rotation")
	gc("container", "clusters", "get-credentials", cluster, "--zone", zone)
	if out, err := exec.Command("kubectl", "--kubeconfig", kubeconfig, "get", "namespace", "default").CombinedOutput(); err != nil {
		t.Errorf("kubectl during the rotation: %v\n%s", err, out)
	}
	gc("container", "clusters", "update", cluster, "--zone", zone, "--complete-credential-rotation")
	after := gc("container", "clusters", "describe", cluster, "--zone", zone, "--format=value(masterAuth.clusterCaCertificate)")
	if strings.TrimSpace(after) == strings.TrimSpace(before) {
		t.Error("the cluster CA is the same after the rotation")
	}
	gc("container", "clusters", "get-credentials", cluster, "--zone", zone)
	if out, err := exec.Command("kubectl", "--kubeconfig", kubeconfig, "get", "namespace", "default").CombinedOutput(); err != nil {
		t.Errorf("kubectl after the rotation: %v\n%s", err, out)
	}
}
