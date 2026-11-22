//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/container/apiv1/containerpb"
	vlogging "cloud.google.com/go/logging/apiv2"
	"cloud.google.com/go/logging/apiv2/loggingpb"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/cloudburrow/cloudburrow/internal/service/gke"
)

// Kubernetes Engine's logs across a CLI restart (#2100), the pattern of
// #418: TestGKELogsRestartSetup runs just before `stop`, and
// TestGKELogsAcrossRestart after each `up`. The logging agent runs in the
// cluster, as GKE's does, so a pod's lines written while the CLI is down
// are shipped once it is back, none lost and none twice. Cloud Logging's
// entries are held in memory (docs/install.md), so what was shipped before
// the stop is gone after it: the entries after the restart must be every
// line from the first one the agent held back while the CLI was down.
const (
	envGKELogsProbe  = "CLOUDBURROW_TEST_GKE_LOGS_PROBE"
	envGKELogsExpect = "CLOUDBURROW_TEST_GKE_LOGS_EXPECT"
)

const gkeLogsRestartZone, gkeLogsRestartCluster = "us-central1-c", "logs-restart-2100"

type gkeLogsProbe struct {
	Cluster    string `json:"cluster"`
	Kubeconfig string `json:"kubeconfig"`
	Tick       string `json:"tick"`
	// Done is when the setup ended, just before CI's stop.
	Done time.Time `json:"done"`
}

// gkeTick is one tick line's entries: how many, when the pod wrote it,
// and when Cloud Logging first received it.
type gkeTick struct {
	n                 int
	written, received time.Time
}

// gkeTicks are the tick lines in Cloud Logging, by number.
func gkeTicks(ctx context.Context, t *testing.T, c *vlogging.Client, project, tick string) map[int]*gkeTick {
	t.Helper()
	it := c.ListLogEntries(ctx, &loggingpb.ListLogEntriesRequest{ResourceNames: []string{"projects/" + project}, PageSize: 1000,
		Filter: fmt.Sprintf(`resource.type="k8s_container" AND resource.labels.cluster_name=%q AND resource.labels.pod_name="ticker" AND textPayload:%q`,
			gkeLogsRestartCluster, tick+"-")})
	out := map[int]*gkeTick{}
	for {
		e, err := it.Next()
		if err == iterator.Done {
			return out
		}
		if err != nil {
			t.Fatalf("ListLogEntries: %v", err)
		}
		n, err := strconv.Atoi(strings.TrimPrefix(e.GetTextPayload(), tick+"-"))
		if err != nil {
			continue
		}
		g := out[n]
		if g == nil {
			g = &gkeTick{written: e.GetTimestamp().AsTime(), received: e.GetReceiveTimestamp().AsTime()}
			out[n] = g
		}
		g.n++
	}
}

func gkeLoggingClient(t *testing.T, h *Harness) *vlogging.Client {
	t.Helper()
	c, err := vlogging.NewClient(context.Background(), loggingClientOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// gkeLastTick is the number of the last tick the pod wrote.
func gkeLastTick(t *testing.T, kubeconfig, tick string) int {
	t.Helper()
	last := gkeKubectl(t, kubeconfig, "", "logs", "ticker", "--tail=1")
	n, err := strconv.Atoi(strings.TrimPrefix(last, tick+"-"))
	if err != nil {
		t.Fatalf("the ticker's last line %q", last)
	}
	return n
}

// TestGKELogsRestartSetup makes a cluster whose pod writes a numbered line
// every half second, waits for its lines in Cloud Logging and for the
// fluentbit-gke DaemonSet that ships them, and leaves both for
// TestGKELogsAcrossRestart, writing what it needs to envGKELogsProbe.
func TestGKELogsRestartSetup(t *testing.T) {
	path := os.Getenv(envGKELogsProbe)
	if path == "" {
		t.Skipf("%s is not set: CI runs this alone, just before stop", envGKELogsProbe)
	}
	h := New(t)
	lc := gkeLoggingClient(t, h)
	g, _ := gkeClients(t, h)
	if _, err := exec.LookPath("kubectl"); err != nil {
		t.Skip("kubectl is required")
	}
	instance := strings.TrimSpace(os.Getenv("CLOUDBURROW_TEST_CLUSTER"))
	if instance == "" {
		t.Skip("CLOUDBURROW_TEST_CLUSTER is not set: the probe is loaded into the cluster by kind")
	}
	ctx := context.Background()
	project := h.Project()
	name := "projects/" + project + "/locations/" + gkeLogsRestartZone + "/clusters/" + gkeLogsRestartCluster
	op, err := g.CreateCluster(ctx, &containerpb.CreateClusterRequest{Parent: "projects/" + project + "/locations/" + gkeLogsRestartZone, Cluster: &containerpb.Cluster{
		Name: gkeLogsRestartCluster, MaintenancePolicy: gkeNoAutoUpgrade(),
		MasterAuth: &containerpb.MasterAuth{ClientCertificateConfig: &containerpb.ClientCertificateConfig{IssueClientCertificate: true}},
		NodePools:  []*containerpb.NodePool{{Name: "default-pool", InitialNodeCount: 1}}}})
	if err != nil {
		t.Fatal(err)
	}
	ok := false
	t.Cleanup(func() {
		if ok {
			return
		}
		if op, err := g.DeleteCluster(context.Background(), &containerpb.DeleteClusterRequest{Name: name}); err == nil {
			gkeWait(t, g, project, gkeLogsRestartZone, op)
		}
	})
	gkeWait(t, g, project, gkeLogsRestartZone, op)
	c, err := g.GetCluster(ctx, &containerpb.GetClusterRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	kc := gkeKubeconfig(t, c)
	loadEventProbe(ctx, t)
	if b, err := exec.CommandContext(ctx, "kind", "load", "docker-image", eventProbeImage, "--name", gke.KindName(instance, name)).CombinedOutput(); err != nil {
		t.Fatalf("load the probe into the GKE cluster: %v\n%s", err, b)
	}
	tick := fmt.Sprint("tick-", time.Now().UnixNano())
	gkeKubectl(t, kc, fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata: {name: ticker, namespace: default, labels: {app: logs-restart-2100}}
spec:
  terminationGracePeriodSeconds: 1
  containers:
  - name: probe
    image: %s
    imagePullPolicy: Never
    env: [{name: PROBE_TICK, value: %q}]
`, eventProbeImage, tick), "apply", "-f", "-")
	gkeKubectl(t, kc, "", "wait", "--for=condition=Ready", "pod/ticker", "--timeout=180s")
	if !gkeEventually(90*time.Second, func() bool { return len(gkeTicks(ctx, t, lc, project, tick)) >= 3 }) {
		t.Fatal("the ticker's lines are not in Cloud Logging")
	}
	// The agent is the cluster's own: GKE's DaemonSet, one ready pod a node.
	ds := gkeKubectl(t, kc, "", "get", "daemonset", "-n", "kube-system", "fluentbit-gke", "-o",
		"jsonpath={.status.desiredNumberScheduled} {.status.numberReady}")
	if f := strings.Fields(ds); len(f) != 2 || f[0] != f[1] || f[0] == "0" {
		t.Fatalf("kube-system/fluentbit-gke desired and ready = %q", ds)
	}
	kcPath := filepath.Join(filepath.Dir(path), "gke-logs-kubeconfig")
	b, err := os.ReadFile(kc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(kcPath, b, 0o600); err != nil {
		t.Fatal(err)
	}
	pb, _ := json.Marshal(gkeLogsProbe{Cluster: name, Kubeconfig: kcPath, Tick: tick, Done: time.Now()})
	if err := os.WriteFile(path, pb, 0o600); err != nil {
		t.Fatal(err)
	}
	ok = true
}

// TestGKELogsAcrossRestart: after stop and up in persistent mode, the
// lines the ticker wrote while the CLI was stopped arrive once it is back,
// each once, and so does every line after them: the entries are one run
// of consecutive ticks, up to the newest the pod has written, whose first
// was written before the restart, within the stop that followed the setup,
// and received only after it. The cluster is then deleted. In ephemeral
// mode the cluster is unknown.
func TestGKELogsAcrossRestart(t *testing.T) {
	expect := os.Getenv(envGKELogsExpect)
	if expect == "" {
		t.Skipf("%s is not set: this runs only after CI restarts the instance", envGKELogsExpect)
	}
	b, err := os.ReadFile(os.Getenv(envGKELogsProbe))
	if err != nil {
		t.Fatalf("the probe TestGKELogsRestartSetup left: %v", err)
	}
	var p gkeLogsProbe
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatal(err)
	}
	h := New(t)
	lc := gkeLoggingClient(t, h)
	g, _ := gkeClients(t, h)
	ctx := context.Background()
	// The setup's project, which the harness may not name again.
	project := strings.Split(p.Cluster, "/")[1]
	switch expect {
	case "absent":
		if _, err := g.GetCluster(ctx, &containerpb.GetClusterRequest{Name: p.Cluster}); status.Code(err) != codes.NotFound {
			t.Fatalf("ephemeral mode after stop/up: GetCluster = %v; want NotFound", err)
		}
		return
	case "present":
	default:
		t.Fatalf("%s must be present or absent, not %q", envGKELogsExpect, expect)
	}
	t.Cleanup(func() {
		if op, err := g.DeleteCluster(context.Background(), &containerpb.DeleteClusterRequest{Name: p.Cluster}); err == nil {
			gkeWait(t, g, project, gkeLogsRestartZone, op)
		}
	})
	last := gkeLastTick(t, p.Kubeconfig, p.Tick)
	var got map[int]*gkeTick
	if !gkeEventually(120*time.Second, func() bool { got = gkeTicks(ctx, t, lc, project, p.Tick); return got[last] != nil }) {
		t.Fatalf("tick %d, written after the restart, is not in Cloud Logging (%d ticks are)", last, len(got))
	}
	first := last
	for n := range got {
		first = min(first, n)
	}
	f := got[first]
	t.Logf("ticks %d to %d after the restart; tick %d written %s, received %s", first, last, first, f.written.Format(time.RFC3339Nano), f.received.Format(time.RFC3339Nano))
	// The first was held back while the CLI was down: written well before
	// it was received, and before the setup's stop could have ended.
	if f.received.Sub(f.written) < 5*time.Second {
		t.Errorf("tick %d was received %s after it was written: no line held while the CLI was stopped arrived", first, f.received.Sub(f.written))
	}
	if f.written.After(p.Done.Add(2 * time.Minute)) {
		t.Errorf("the first tick after the restart, %d, was written %s, over two minutes after the setup ended (%s): the lines of the stop were lost",
			first, f.written.Format(time.RFC3339Nano), p.Done.Format(time.RFC3339Nano))
	}
	var missing, twice []int
	for n := first; n <= last; n++ {
		switch {
		case got[n] == nil:
			missing = append(missing, n)
		case got[n].n > 1:
			twice = append(twice, n)
		}
	}
	if len(missing) > 0 {
		t.Errorf("%d of ticks %d to %d are not in Cloud Logging: %v", len(missing), first, last, missing)
	}
	if len(twice) > 0 {
		t.Errorf("lines shipped more than once: %v", twice)
	}
}
