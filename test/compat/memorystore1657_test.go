//go:build compat

package compat

import (
	"encoding/json"
	"fmt"
	"net/http"
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

// valkeyFailovers are the automatic failovers of instance name recorded
// in /admin/events since since.
func valkeyFailovers(t *testing.T, control, name string, since time.Time) []adminEvent {
	t.Helper()
	url := fmt.Sprintf("http://%s/admin/events?service=memorystore&kind=failover&limit=1000&since=%s",
		control, since.UTC().Format(time.RFC3339Nano))
	code, raw := adminDo(t, http.MethodGet, url, "", nil)
	if code != http.StatusOK {
		t.Fatalf("GET %s: %d %s", url, code, raw)
	}
	var body struct{ Events []adminEvent }
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("decode events: %v", err)
	}
	var mine []adminEvent
	for _, e := range body.Events {
		if e.Target == name {
			mine = append(mine, e)
		}
	}
	return mine
}

// covers: google.cloud.memorystore.v1.Memorystore/CreateInstance, google.cloud.memorystore.v1.Memorystore/GetInstance
//
// TestMemorystoreValkeyAutomaticFailover (#1657), with cloud.google.com/
// go/memorystore/apiv1 and go-redis: a CLUSTER_DISABLED instance of one
// replica, written to all along on its primary endpoint. The primary's
// pod is deleted (kubectl, this instance's own pod only, by its role
// label): writes resume on the same endpoint, at the promoted replica,
// within the tens of seconds Google documents. The old primary's node
// rejoins, empty, as a replica of the promoted one and syncs, and the
// writes go on past the instance's settling: the primary endpoint reaches
// the promoted node throughout, /admin/events records exactly one
// failover, from the old primary's node to the replica's, and every write
// acknowledged after the failover, and every one before it the replica
// had, is read back.
func TestMemorystoreValkeyAutomaticFailover(t *testing.T) {
	h := New(t)
	c, project := valkeyAdminClient(t, h)
	control := h.Endpoint(EnvControl)
	kubeconfig := strings.TrimSpace(os.Getenv(envKubeconfig))
	if kubeconfig == "" {
		t.Skipf("%s is required to delete the primary's pod", envKubeconfig)
	}
	if _, err := exec.LookPath("kubectl"); err != nil {
		t.Skip("kubectl is required to delete the primary's pod")
	}
	ctx := memorystoreWait(t)
	inst := valkeyDisabled(t, ctx, c, project, fmt.Sprintf("compat-afo-%d", time.Now().UnixNano()%1e8), func(in *memorystorepb.Instance) {
		in.ReplicaCount = proto.Int32(1)
	})
	name := inst.GetName()
	primaryAddr := valkeyEndpoint(t, inst, memorystorepb.ConnectionType_CONNECTION_TYPE_PRIMARY)
	primary := valkeyTLSPrimary(t, inst, nil)
	for i := 0; i < 20; i++ {
		if err := primary.Set(ctx, fmt.Sprintf("before-%d", i), "1", 0).Err(); err != nil {
			t.Fatalf("SET on the primary endpoint: %v", err)
		}
	}
	eventuallyTrue(t, "the replica has the keys", func() bool {
		n, err := primary.Do(ctx, "WAIT", 1, 1000).Int()
		return err == nil && n >= 1
	})
	oldPrimary, role, err := valkeyRunID(ctx, primaryAddr, nil)
	if err != nil || role != "master" {
		t.Fatalf("the primary endpoint reaches %s (%s), %v", oldPrimary, role, err)
	}

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
	since := time.Now()
	if out, err := kubectl("-n", ns, "delete", "pod", "-l", "app="+app, "--wait=false"); err != nil {
		t.Fatalf("kubectl delete pod: %v: %s", err, out)
	}
	deleted := time.Now()

	// Writes resume at the promoted replica.
	w := redis.NewClient(&redis.Options{Addr: primaryAddr, MaxRetries: -1, DialTimeout: 2 * time.Second, ReadTimeout: 2 * time.Second, WriteTimeout: 2 * time.Second, PoolSize: 1})
	defer w.Close()
	var newPrimary string
	for {
		if time.Since(deleted) > valkeyFailoverWithin {
			t.Fatalf("writes did not resume on the primary endpoint within %v of deleting the primary's pod", valkeyFailoverWithin)
		}
		id, role, err := valkeyRunID(ctx, primaryAddr, nil)
		if err == nil && role == "master" && id != oldPrimary && w.Set(ctx, "resumed", "1", 0).Err() == nil {
			newPrimary = id
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Logf("writes resumed on the primary endpoint %v after the primary's pod was deleted", time.Since(deleted).Round(100*time.Millisecond))

	// Write on: through the old node's rejoining and syncing, and the
	// settling after it. Each acknowledged write must survive.
	var acked []string
	rejoined := time.Time{}
	settle := 25 * time.Second
	for i := 0; ; i++ {
		if time.Since(deleted) > 4*valkeyFailoverWithin {
			t.Fatalf("the old primary's node did not rejoin and sync within %v (acknowledged %d writes)", 4*valkeyFailoverWithin, len(acked))
		}
		if !rejoined.IsZero() && time.Since(rejoined) > settle {
			break
		}
		k := fmt.Sprintf("after-%d", i)
		if err := w.Set(ctx, k, "1", 0).Err(); err == nil {
			acked = append(acked, k)
		}
		if i%10 == 0 {
			id, role, err := valkeyRunID(ctx, primaryAddr, nil)
			if err == nil && (role != "master" || id != newPrimary) {
				t.Fatalf("the primary endpoint reaches %s (%s) after %d writes, not the promoted node %s: a second failover", id, role, len(acked), newPrimary)
			}
			if rejoined.IsZero() {
				if n, err := w.Do(ctx, "WAIT", 1, 100).Int(); err == nil && n >= 1 {
					rejoined = time.Now()
					t.Logf("the old primary's node replicates the promoted one %v after its pod was deleted", rejoined.Sub(deleted).Round(100*time.Millisecond))
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(acked) == 0 {
		t.Fatal("no write was acknowledged after the failover")
	}

	fo := valkeyFailovers(t, control, name, since)
	if len(fo) != 1 {
		t.Fatalf("/admin/events records %d automatic failovers of %s after one pod deletion, want 1: %+v", len(fo), name, fo)
	}
	if from, to := fo[0].Detail["from"], fo[0].Detail["to"]; from == to || !strings.HasPrefix(from, base+"-") || !strings.HasPrefix(to, base+"-") {
		t.Errorf("the failover went from %q to %q, want from one of %s's nodes to the other", from, to, name)
	}

	r := redis.NewClient(&redis.Options{Addr: primaryAddr})
	defer r.Close()
	for _, k := range append(append([]string{"resumed"}, acked...), "before-0", "before-19") {
		if v, err := r.Get(ctx, k).Result(); err != nil || v != "1" {
			t.Fatalf("GET %s = %q, %v after the failover: an acknowledged write was lost (of %d)", k, v, err, len(acked))
		}
	}
	t.Logf("%d writes acknowledged after the failover, all read back", len(acked))
}
