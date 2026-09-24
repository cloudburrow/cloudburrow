//go:build compat

package compat

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// EnvMemorystore is the host RESP endpoint of the memorystore service.
const EnvMemorystore = "CLOUDBURROW_TEST_MEMORYSTORE"

// envMemorystoreExpect selects TestMemorystoreAcrossRestart's expectation.
const envMemorystoreExpect = "CLOUDBURROW_TEST_MEMORYSTORE_EXPECT"

// restartProbeKey is written by TestMemorystoreDataPlane and read back by
// TestMemorystoreAcrossRestart after CI stops and starts the instance.
const restartProbeKey = "cloudburrow:restart-probe"

func redisClient(t *testing.T, h *Harness) *redis.Client {
	t.Helper()
	c := redis.NewClient(&redis.Options{Addr: h.Endpoint(EnvMemorystore)})
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// TestMemorystoreDataPlane.
//
// `--services memorystore` (#296) with an ordinary Go Redis client and no
// CloudBurrow code: SET/GET, MULTI/EXEC, PUBLISH/SUBSCRIBE and a Lua EVAL
// against the host endpoint, then PING from a pod against the in-cluster
// Service name. This is a real Valkey server; the Memorystore admin API is
// not served and nothing here touches it.
func TestMemorystoreDataPlane(t *testing.T) {
	h := New(t)
	c := redisClient(t, h)
	ctx := h.Context()
	prefix := "compat:" + h.Project() + ":"

	if err := c.Set(ctx, prefix+"k", "v", 0).Err(); err != nil {
		t.Fatalf("SET: %v", err)
	}
	if got, err := c.Get(ctx, prefix+"k").Result(); err != nil || got != "v" {
		t.Fatalf("GET = %q, %v", got, err)
	}

	// MULTI/EXEC: both commands applied atomically, results in order.
	var incr *redis.IntCmd
	var get *redis.StringCmd
	if _, err := c.TxPipelined(ctx, func(p redis.Pipeliner) error {
		incr = p.Incr(ctx, prefix+"counter")
		get = p.Get(ctx, prefix+"k")
		return nil
	}); err != nil {
		t.Fatalf("MULTI/EXEC: %v", err)
	}
	if incr.Val() != 1 || get.Val() != "v" {
		t.Errorf("MULTI/EXEC results = %d, %q; want 1, \"v\"", incr.Val(), get.Val())
	}

	// PUBLISH/SUBSCRIBE.
	sub := c.Subscribe(ctx, prefix+"events")
	defer sub.Close()
	if _, err := sub.Receive(ctx); err != nil {
		t.Fatalf("SUBSCRIBE: %v", err)
	}
	if n, err := c.Publish(ctx, prefix+"events", "hello").Result(); err != nil || n != 1 {
		t.Fatalf("PUBLISH reached %d subscribers (%v), want 1", n, err)
	}
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	msg, err := sub.ReceiveMessage(rctx)
	cancel()
	if err != nil || msg.Payload != "hello" {
		t.Fatalf("received %v (%v), want \"hello\"", msg, err)
	}

	// Lua EVAL, reading and writing keys server-side.
	script := `redis.call('SET', KEYS[1], ARGV[1]); return redis.call('STRLEN', KEYS[1])`
	if n, err := c.Eval(ctx, script, []string{prefix + "lua"}, "four").Int(); err != nil || n != 4 {
		t.Fatalf("EVAL = %d, %v; want 4", n, err)
	}
	if got, _ := c.Get(ctx, prefix+"lua").Result(); got != "four" {
		t.Errorf("EVAL's SET left %q", got)
	}

	// Inside the cluster, by Service name.
	if cli := os.Getenv(EnvCLI); cli != "" {
		dir := instanceDirFrom(t, strings.Fields(os.Getenv(EnvCLIArgs)))
		pctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
		defer cancel()
		out, err := exec.CommandContext(pctx, "kubectl", "--kubeconfig", filepath.Join(dir, "kubeconfig"),
			"-n", "cloudburrow", "run", "memorystore-ping", "--rm", "-i", "--restart=Never", "--quiet",
			"--image="+memorystoreImage(t),
			"--", "valkey-cli", "-h", "memorystore.cloudburrow.svc.cluster.local", "-p", "6379", "GET", prefix+"k").CombinedOutput()
		if err != nil || strings.TrimSpace(string(out)) != "v" {
			t.Errorf("in-cluster GET via memorystore.cloudburrow.svc.cluster.local = %q (%v), want \"v\"", out, err)
		}
	} else {
		t.Logf("%s is not set; the in-cluster address was not exercised", EnvCLI)
	}

	// Left for TestMemorystoreAcrossRestart, which CI runs after stop/up.
	if err := c.Set(ctx, restartProbeKey, "written-before-stop", 0).Err(); err != nil {
		t.Fatal(err)
	}
}

// TestMemorystoreAcrossRestart measures durability. CI runs it twice after
// the suite: once after stop and up in persistent mode, expecting the probe
// key, and once after up in --mode ephemeral, expecting none.
func TestMemorystoreAcrossRestart(t *testing.T) {
	expect := os.Getenv(envMemorystoreExpect)
	if expect == "" {
		t.Skipf("%s is not set: this runs only after CI restarts the instance", envMemorystoreExpect)
	}
	h := New(t)
	c := redisClient(t, h)
	got, err := c.Get(h.Context(), restartProbeKey).Result()
	switch expect {
	case "present":
		if err != nil || got != "written-before-stop" {
			t.Fatalf("persistent mode after stop/up: GET = %q, %v; want the key written before stop", got, err)
		}
	case "absent":
		if err != redis.Nil {
			t.Fatalf("ephemeral mode after stop/up: GET = %q, %v; want no key", got, err)
		}
	default:
		t.Fatalf("%s must be present or absent, not %q", envMemorystoreExpect, expect)
	}
}

// memorystoreImage is the image the backend runs, so the probe pod needs no
// second pull.
func memorystoreImage(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "internal", "components", "optional.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.Contains(line, "MemorystoreImage =") {
			return strings.Trim(strings.TrimSpace(strings.SplitN(line, "=", 2)[1]), `"`)
		}
	}
	t.Fatal("MemorystoreImage not found")
	return ""
}
