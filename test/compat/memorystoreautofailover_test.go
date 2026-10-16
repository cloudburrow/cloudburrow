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

	redispb "cloud.google.com/go/redis/apiv1/redispb"

	"github.com/cloudburrow/cloudburrow/internal/service/redisadmin"
)

// covers: google.cloud.redis.v1.CloudRedis/CreateInstance, google.cloud.redis.v1.CloudRedis/GetInstance
//
// TestMemorystoreAutomaticFailover (#1412), with cloud.google.com/go/
// redis/apiv1: the primary node's pod of a STANDARD_HA instance is
// deleted (kubectl delete pod, this instance's own pod only). The
// instance fails over on its own, as Memorystore's does: a replica is
// promoted, current_location_id moves to its zone, the instance is READY
// again, and a client at the same host:port reaches the new primary
// (role:master) with the key written before; the old primary's node comes
// back as a replica of it.
func TestMemorystoreAutomaticFailover(t *testing.T) {
	h := New(t)
	c, project := memorystoreAdminClient(t, h)
	kubeconfig := strings.TrimSpace(os.Getenv(envKubeconfig))
	if kubeconfig == "" {
		t.Skip("CLOUDBURROW_TEST_KUBECONFIG is required to delete the primary's pod")
	}
	if _, err := exec.LookPath("kubectl"); err != nil {
		t.Skip("kubectl is required to delete the primary's pod")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	parent := "projects/" + project + "/locations/us-central1"
	id := fmt.Sprintf("auto-%d", time.Now().UnixNano()%1e8)
	name := parent + "/instances/" + id
	op, err := c.CreateInstance(ctx, &redispb.CreateInstanceRequest{Parent: parent, InstanceId: id,
		Instance: &redispb.Instance{Tier: redispb.Instance_STANDARD_HA, MemorySizeGb: 1, AuthEnabled: true, LocationId: "us-central1-a"}})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	t.Cleanup(func() {
		if op, err := c.DeleteInstance(context.Background(), &redispb.DeleteInstanceRequest{Name: name}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	inst, err := op.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateInstance operation: %v", err)
	}
	auth, err := c.GetInstanceAuthString(ctx, &redispb.GetInstanceAuthStringRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	pw := auth.GetAuthString()
	rc := redisAt(inst, pw)
	defer rc.Close()
	if err := rc.Set(ctx, "k", "before", 0).Err(); err != nil {
		t.Fatalf("SET: %v", err)
	}
	eventuallyTrue(t, "the replica has the key", func() bool {
		n, err := rc.Do(ctx, "WAIT", 1, 1000).Int()
		return err == nil && n >= 1
	})

	// The primary's pod: app=<server>-n0, in this instance's cluster.
	app := redisadmin.ServerName(name) + "-n0"
	out, err := exec.CommandContext(ctx, "kubectl", "--kubeconfig", kubeconfig, "delete", "pod", "-A", "-l", "app="+app, "--wait=false").CombinedOutput()
	if err != nil || !strings.Contains(string(out), "deleted") {
		// kubectl delete has no -A with -l in every version: find the namespace.
		ns, nerr := exec.CommandContext(ctx, "kubectl", "--kubeconfig", kubeconfig, "get", "pods", "-A", "-l", "app="+app,
			"-o", "jsonpath={.items[0].metadata.namespace}").Output()
		if nerr != nil || len(ns) == 0 {
			t.Fatalf("find the primary's pod %s: %v %s", app, err, out)
		}
		if out, err := exec.CommandContext(ctx, "kubectl", "--kubeconfig", kubeconfig, "-n", string(ns), "delete", "pod", "-l", "app="+app, "--wait=false").CombinedOutput(); err != nil {
			t.Fatalf("kubectl delete pod: %v: %s", err, out)
		}
	}
	deleted := time.Now()

	var after *redispb.Instance
	for end := time.Now().Add(3 * time.Minute); ; {
		after, err = c.GetInstance(ctx, &redispb.GetInstanceRequest{Name: name})
		if err == nil && after.GetCurrentLocationId() != "us-central1-a" && after.GetState() == redispb.Instance_READY {
			break
		}
		if time.Now().After(end) {
			t.Fatalf("no automatic failover within 3 minutes of deleting the primary's pod: %v, %v", after, err)
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Logf("failed over %v after the pod was deleted", time.Since(deleted).Round(time.Second))
	if after.GetHost() != inst.GetHost() || after.GetPort() != inst.GetPort() {
		t.Errorf("the primary endpoint moved: %s:%d, was %s:%d", after.GetHost(), after.GetPort(), inst.GetHost(), inst.GetPort())
	}
	if after.GetCurrentLocationId() != inst.GetAlternativeLocationId() {
		t.Errorf("current_location_id %s, want the replica's zone %s", after.GetCurrentLocationId(), inst.GetAlternativeLocationId())
	}
	next := redisAt(after, pw)
	defer next.Close()
	if v, err := next.Get(ctx, "k").Result(); err != nil || v != "before" {
		t.Errorf("GET k at the same host:port after the automatic failover = %q, %v; want before", v, err)
	}
	if r := role(ctx, t, next); r != "master" {
		t.Errorf("after the automatic failover host:port reaches a %s", r)
	}
	if err := next.Set(ctx, "k2", "after", 0).Err(); err != nil {
		t.Fatalf("SET on the new primary: %v", err)
	}
	// The old primary's node is back, a replica of the new primary.
	eventuallyTrue(t, "the old primary's node replicates the new primary", func() bool {
		n, err := next.Do(ctx, "WAIT", 1, 1000).Int()
		return err == nil && n >= 1
	})
}
