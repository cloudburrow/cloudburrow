//go:build compat

package compat

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	clusterpb "cloud.google.com/go/redis/cluster/apiv1/clusterpb"
	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"

	"github.com/cloudburrow/cloudburrow/internal/components"
)

// covers: google.cloud.redis.cluster.v1.CloudRedisCluster/CreateCluster, google.cloud.redis.cluster.v1.CloudRedisCluster/DeleteCluster
//
// TestMemorystoreRedisClusterHostAndPodClients (#1614), with cloud.google.
// com/go/redis/cluster/apiv1 and go-redis on the host and valkey-cli in a
// pod: a cluster of an instance bound to 127.0.0.1 serves both kinds of
// cluster client without any address on a network. A go-redis
// ClusterClient on the host, started from the discovery endpoint, is given
// the nodes at [::1] (its tunnels bring its connections in over IPv6) and
// sets and gets keys in every shard; valkey-cli -c in a pod, started from
// a node's Service, is given the nodes' Services' cluster IPs (it connects
// over IPv4), follows MOVED to every shard, reads the host's keys and
// writes keys the host client then reads.
func TestMemorystoreRedisClusterHostAndPodClients(t *testing.T) {
	h := New(t)
	c, project := redisClusterAdminClient(t, h)
	kubeconfig := strings.TrimSpace(os.Getenv(envKubeconfig))
	if kubeconfig == "" {
		t.Skipf("%s is required to run the in-cluster client", envKubeconfig)
	}
	if _, err := exec.LookPath("kubectl"); err != nil {
		t.Skip("kubectl is required to run the in-cluster client")
	}
	kubectl := func(args ...string) (string, error) {
		cmd := exec.Command("kubectl", append([]string{"--kubeconfig", kubeconfig, "-n", "cloudburrow"}, args...)...)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			return string(out), fmt.Errorf("kubectl %s: %v %s", args[0], err, stderr.String())
		}
		return string(out), nil
	}
	ctx := memorystoreWait(t)
	parent := "projects/" + project + "/locations/us-central1"
	id := fmt.Sprintf("compat-ca-%d", time.Now().UnixNano()%1e8)
	name := parent + "/clusters/" + id
	op, err := c.CreateCluster(ctx, &clusterpb.CreateClusterRequest{Parent: parent, ClusterId: id, Cluster: &clusterpb.Cluster{
		ShardCount: proto.Int32(3), ReplicaCount: proto.Int32(0), NodeType: clusterpb.NodeType_REDIS_SHARED_CORE_NANO,
		AuthorizationMode: clusterpb.AuthorizationMode_AUTH_MODE_DISABLED,
		PscConfigs:        []*clusterpb.PscConfig{{Network: "projects/" + project + "/global/networks/default"}},
	}})
	if err != nil {
		t.Fatalf("CreateCluster: %v", err)
	}
	t.Cleanup(func() {
		if op, err := c.DeleteCluster(h.Context(), &clusterpb.DeleteClusterRequest{Name: name}); err == nil {
			_ = waitLRODone(h.Context(), op)
		}
	})
	cl, err := waitLRO(ctx, op)
	if err != nil {
		t.Fatalf("CreateCluster operation: %v", err)
	}
	ep := cl.GetDiscoveryEndpoints()[0]
	if ep.GetAddress() != "127.0.0.1" {
		t.Skipf("the instance is bound to %s: it announces that address to every client", ep.GetAddress())
	}
	port := strconv.Itoa(int(ep.GetPort()))

	// The host client.
	rc := redis.NewClusterClient(&redis.ClusterOptions{Addrs: []string{net.JoinHostPort(ep.GetAddress(), port)}})
	defer rc.Close()
	masters := clusterMasters(t, h, rc)
	if len(masters) != 3 {
		t.Errorf("CLUSTER SLOTS through the host names %d primaries, want 3: %v", len(masters), masters)
	}
	for a := range masters {
		if host, _, _ := net.SplitHostPort(a); host != "::1" {
			t.Errorf("a host client is given the node %s, want one at ::1", a)
		}
	}
	clusterKeys(t, h, rc, "host", 30, true)

	// The in-cluster client, from the first node's Service.
	svcs, err := kubectl("get", "services", "-o", "jsonpath={.items[*].metadata.name}")
	if err != nil {
		t.Fatal(err)
	}
	first := ""
	for _, s := range strings.Fields(svcs) {
		if strings.HasPrefix(s, "redis-c"+id+"-") && strings.HasSuffix(s, "-0") {
			first = s
		}
	}
	if first == "" {
		t.Fatalf("no Service of the cluster's first node among %s", svcs)
	}
	var script strings.Builder
	fmt.Fprintf(&script, "set -e; c() { valkey-cli -c -h %s -p %s \"$@\"; }; ", first, port)
	for i := 0; i < 30; i++ {
		fmt.Fprintf(&script, "echo \"host-%d=$(c GET host-%d)\"; c SET pod-%d %d >/dev/null; ", i, i, i, i)
	}
	fmt.Fprintf(&script, "valkey-cli -h %s -p %s CLUSTER SLOTS", first, port)
	pod := "compat-ca-client-" + strconv.FormatInt(time.Now().UnixNano()%1e8, 10)
	if _, err := kubectl("run", pod, "--image="+components.MemorystoreImage, "--image-pull-policy=IfNotPresent", "--restart=Never",
		"--command", "--", "sh", "-c", script.String()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = kubectl("delete", "pod", pod, "--ignore-not-found", "--wait=false") })
	var logs string
	for deadline := time.Now().Add(3 * time.Minute); ; time.Sleep(2 * time.Second) {
		phase, _ := kubectl("get", "pod", pod, "-o", "jsonpath={.status.phase}")
		if phase == "Succeeded" || phase == "Failed" {
			logs, _ = kubectl("logs", pod)
			if phase == "Failed" {
				t.Fatalf("the in-cluster client failed: %s", logs)
			}
			break
		}
		if time.Now().After(deadline) {
			logs, _ = kubectl("logs", pod)
			t.Fatalf("the in-cluster client did not finish (%s): %s", phase, logs)
		}
	}
	for i := 0; i < 30; i++ {
		if !strings.Contains(logs, fmt.Sprintf("host-%d=%d\n", i, i)) {
			t.Errorf("the in-cluster client did not read host-%d: %s", i, logs)
			break
		}
	}
	ips, err := kubectl("get", "services", "-o", "jsonpath={range .items[*]}{.metadata.name} {.spec.clusterIP}{\"\\n\"}{end}")
	if err != nil {
		t.Fatal(err)
	}
	nodeIPs := 0
	for _, line := range strings.Split(ips, "\n") {
		if f := strings.Fields(line); len(f) == 2 && strings.HasPrefix(f[0], "redis-c"+id+"-") {
			nodeIPs++
			if !strings.Contains(logs, "\n"+f[1]+"\n") {
				t.Errorf("CLUSTER SLOTS in the cluster does not give the node %s at its Service's cluster IP %s:\n%s", f[0], f[1], logs)
			}
		}
	}
	if nodeIPs != 3 || strings.Contains(logs, "127.0.0.1") || strings.Contains(logs, "::1") {
		t.Errorf("CLUSTER SLOTS in the cluster (%d node Services): %s", nodeIPs, logs)
	}
	// The pod's keys, read on the host.
	clusterKeys(t, h, rc, "pod", 30, false)
}
