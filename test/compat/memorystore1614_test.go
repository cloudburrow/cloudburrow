//go:build compat

package compat

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	memorystorepb "cloud.google.com/go/memorystore/apiv1/memorystorepb"
	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"

	"github.com/cloudburrow/cloudburrow/internal/service/redisadmin"
)

// valkeyFailoverWithin is how soon writes resume on the primary endpoint
// after the primary fails: Google documents that "Automatic failovers can
// take time on the order of tens of seconds".
const valkeyFailoverWithin = 60 * time.Second

// valkeyRunID is INFO server's run_id and INFO replication's role of the
// server a new connection to addr reaches.
func valkeyRunID(ctx context.Context, addr string) (string, string, error) {
	c := redis.NewClient(&redis.Options{Addr: addr, MaxRetries: -1, DialTimeout: 2 * time.Second, ReadTimeout: 2 * time.Second, PoolSize: 1})
	defer c.Close()
	info, err := c.Info(ctx, "server", "replication").Result()
	if err != nil {
		return "", "", err
	}
	field := func(k string) string {
		for _, line := range strings.Split(info, "\n") {
			if key, v, ok := strings.Cut(strings.TrimSpace(line), ":"); ok && key == k {
				return v
			}
		}
		return ""
	}
	return field("run_id"), field("role"), nil
}

// covers: google.cloud.memorystore.v1.Memorystore/CreateInstance, google.cloud.memorystore.v1.Memorystore/GetInstance
//
// TestMemorystoreValkeyReaderAndFailover (#1614), with cloud.google.com/
// go/memorystore/apiv1 and go-redis: a CLUSTER_DISABLED instance of two
// replicas, its endpoints asked as endpoints. Keys written on the primary
// endpoint are read on the reader endpoint, whose connections reach both
// replicas, and it refuses writes. The primary's pod is deleted (kubectl,
// this instance's own pod only, found by its role label): writes resume
// on the same primary endpoint within the tens of seconds Google
// documents, reaching a promoted replica with the keys written before;
// the instance's endpoints still name the same primary and reader
// addresses, and the in-cluster primary Service selects the new primary's
// pod.
func TestMemorystoreValkeyReaderAndFailover(t *testing.T) {
	h := New(t)
	c, project := valkeyAdminClient(t, h)
	kubeconfig := strings.TrimSpace(os.Getenv(envKubeconfig))
	if kubeconfig == "" {
		t.Skipf("%s is required to delete the primary's pod", envKubeconfig)
	}
	if _, err := exec.LookPath("kubectl"); err != nil {
		t.Skip("kubectl is required to delete the primary's pod")
	}
	ctx := memorystoreWait(t)
	parent := "projects/" + project + "/locations/us-central1"
	id := fmt.Sprintf("compat-vf-%d", time.Now().UnixNano()%1e8)
	name := parent + "/instances/" + id
	network := "projects/" + project + "/global/networks/default"
	op, err := c.CreateInstance(ctx, &memorystorepb.CreateInstanceRequest{Parent: parent, InstanceId: id, Instance: &memorystorepb.Instance{
		Mode: memorystorepb.Instance_CLUSTER_DISABLED, ReplicaCount: proto.Int32(2), NodeType: memorystorepb.Instance_SHARED_CORE_NANO,
		Endpoints: []*memorystorepb.Instance_InstanceEndpoint{{Connections: []*memorystorepb.Instance_ConnectionDetail{{
			Connection: &memorystorepb.Instance_ConnectionDetail_PscAutoConnection{PscAutoConnection: &memorystorepb.PscAutoConnection{
				ProjectId: project, Network: network}}}}}},
	}})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	t.Cleanup(func() {
		if op, err := c.DeleteInstance(context.Background(), &memorystorepb.DeleteInstanceRequest{Name: name}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	inst, err := op.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateInstance operation: %v", err)
	}
	primaryAddr := valkeyEndpoint(t, inst, memorystorepb.ConnectionType_CONNECTION_TYPE_PRIMARY)
	readerAddr := valkeyEndpoint(t, inst, memorystorepb.ConnectionType_CONNECTION_TYPE_READER)
	primary := redis.NewClient(&redis.Options{Addr: primaryAddr})
	defer primary.Close()
	for i := 0; i < 20; i++ {
		if err := primary.Set(ctx, fmt.Sprintf("k%d", i), "before", 0).Err(); err != nil {
			t.Fatalf("SET on the primary endpoint: %v", err)
		}
	}
	eventuallyTrue(t, "both replicas have the keys", func() bool {
		n, err := primary.Do(ctx, "WAIT", 2, 1000).Int()
		return err == nil && n >= 2
	})
	reader := redis.NewClient(&redis.Options{Addr: readerAddr})
	defer reader.Close()
	if v, err := reader.Get(ctx, "k7").Result(); err != nil || v != "before" {
		t.Fatalf("GET on the reader endpoint = %q, %v; want before", v, err)
	}
	if err := reader.Set(ctx, "x", "y", 0).Err(); err == nil || !strings.Contains(err.Error(), "READONLY") {
		t.Errorf("SET on the reader endpoint = %v, want READONLY", err)
	}
	oldPrimary, role, err := valkeyRunID(ctx, primaryAddr)
	if err != nil || role != "master" {
		t.Fatalf("the primary endpoint reaches %s (%s), %v", oldPrimary, role, err)
	}
	replicas := map[string]bool{}
	for i := 0; i < 8; i++ {
		id, role, err := valkeyRunID(ctx, readerAddr)
		if err != nil || role != "slave" {
			t.Fatalf("the reader endpoint reaches %s (%s), %v", id, role, err)
		}
		replicas[id] = true
	}
	if len(replicas) != 2 {
		t.Errorf("the reader endpoint's connections reached %d replicas, want both", len(replicas))
	}

	// The primary's pod, by its role label, in this instance's cluster.
	base := redisadmin.ValkeyServerBase(name)
	sel := "cloudburrow.dev/memorystore-of=" + base + ",cloudburrow.dev/memorystore-role=primary"
	kubectl := func(args ...string) (string, error) {
		out, err := exec.CommandContext(ctx, "kubectl", append([]string{"--kubeconfig", kubeconfig}, args...)...).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	var ns, app string
	eventuallyTrue(t, "the primary's pod is labelled", func() bool {
		out, err := kubectl("get", "pods", "-A", "-l", sel, "-o", "jsonpath={.items[*].metadata.namespace} {.items[*].metadata.labels.app}")
		f := strings.Fields(out)
		if err != nil || len(f) != 2 {
			return false
		}
		ns, app = f[0], f[1]
		return true
	})
	if out, err := kubectl("-n", ns, "delete", "pod", "-l", "app="+app, "--wait=false"); err != nil {
		t.Fatalf("kubectl delete pod: %v: %s", err, out)
	}
	deleted := time.Now()

	var newPrimary string
	for {
		if time.Since(deleted) > valkeyFailoverWithin {
			t.Fatalf("writes did not resume on the primary endpoint within %v of deleting the primary's pod", valkeyFailoverWithin)
		}
		id, role, err := valkeyRunID(ctx, primaryAddr)
		if err == nil && role == "master" && id != oldPrimary {
			w := redis.NewClient(&redis.Options{Addr: primaryAddr, MaxRetries: -1, DialTimeout: 2 * time.Second, ReadTimeout: 2 * time.Second})
			err = w.Set(ctx, "k-after", "after", 0).Err()
			w.Close()
			if err == nil {
				newPrimary = id
				break
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Logf("writes resumed on the primary endpoint %v after the primary's pod was deleted", time.Since(deleted).Round(100*time.Millisecond))
	if !replicas[newPrimary] {
		t.Errorf("the new primary %s is not one of the replicas %v", newPrimary, replicas)
	}
	next := redis.NewClient(&redis.Options{Addr: primaryAddr})
	defer next.Close()
	if v, err := next.Get(ctx, "k7").Result(); err != nil || v != "before" {
		t.Errorf("GET k7 on the primary endpoint after the failover = %q, %v; want before", v, err)
	}
	after, err := c.GetInstance(ctx, &memorystorepb.GetInstanceRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	if got := valkeyEndpoint(t, after, memorystorepb.ConnectionType_CONNECTION_TYPE_PRIMARY); got != primaryAddr {
		t.Errorf("the primary endpoint moved: %s, was %s", got, primaryAddr)
	}
	if got := valkeyEndpoint(t, after, memorystorepb.ConnectionType_CONNECTION_TYPE_READER); got != readerAddr {
		t.Errorf("the reader endpoint moved: %s, was %s", got, readerAddr)
	}
	if after.GetState() != memorystorepb.Instance_ACTIVE {
		t.Errorf("state %v after the failover, want ACTIVE", after.GetState())
	}
	// In the cluster, the primary Service selects the promoted node's pod.
	for end := time.Now().Add(time.Minute); ; time.Sleep(time.Second) {
		apps, aerr := kubectl("-n", ns, "get", "pods", "-l", sel, "-o", "jsonpath={.items[*].metadata.labels.app}")
		ips, err := kubectl("-n", ns, "get", "endpointslices", "-l", "kubernetes.io/service-name="+base+"-primary",
			"-o", "jsonpath={.items[*].endpoints[*].addresses[*]}")
		podIP, perr := kubectl("-n", ns, "get", "pods", "-l", "app="+apps, "-o", "jsonpath={.items[0].status.podIP}")
		if aerr == nil && err == nil && perr == nil && len(strings.Fields(apps)) == 1 && apps != app && ips != "" && ips == podIP {
			break
		}
		if time.Now().After(end) {
			t.Fatalf("the primary Service does not select the new primary's pod within a minute: pods %q (%v), endpoints %q (%v), the pod's IP %q (%v)",
				apps, aerr, ips, err, podIP, perr)
		}
	}
	// The old primary's node comes back a replica of the new primary.
	eventuallyTrue(t, "the old primary's node replicates the new primary", func() bool {
		n, err := next.Do(ctx, "WAIT", 2, 1000).Int()
		return err == nil && n >= 2
	})
}
