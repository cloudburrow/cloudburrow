package components

import (
	"slices"
	"strings"
	"testing"
)

// TestMemorystoreServerTLSFront (#1768): a Redis before 6.0 with
// in-transit encryption listens on the pod's loopback alone, with no TLS
// flags of its own, and its front serves TLS on 6378 and relays the
// instance's replication.
func TestMemorystoreServerTLSFront(t *testing.T) {
	b := MemorystoreServerOf(MemorystoreSettings{Name: "redis-x-n1", MaxMemory: 1 << 30, Image: MemorystoreRedis50Image,
		TLSCert: "cert", TLSKey: "key", TLSCA: "ca", TLSFront: true, FrontImage: "dev.local/storage:1", Instance: "redis-x", Namespace: "ns"})
	if b.Port != MemorystoreTLSPort || b.Front == nil || b.Front.UpstreamPort != MemorystorePort || b.Front.Image != "dev.local/storage:1" {
		t.Fatalf("backend %+v, front %+v", b, b.Front)
	}
	args := strings.Join(b.Args, " ")
	if !strings.Contains(args, "--port 6379 --bind 127.0.0.1") || strings.Contains(args, "tls") {
		t.Errorf("server args: %s", args)
	}
	if !slices.Equal(b.Front.Args, []string{"redis-tls-front", "--listen", "0.0.0.0:6378", "--upstream", "127.0.0.1:6379", "--instance", "redis-x", "--namespace", "ns"}) {
		t.Errorf("front args: %q", b.Front.Args)
	}
	if b.Front.Env["MEMORYSTORE_TLS_CERT"] != "cert" || b.Front.Env["MEMORYSTORE_TLS_KEY"] != "key" || b.Front.Env["MEMORYSTORE_TLS_CA"] != "ca" || b.Env != nil {
		t.Errorf("front env %v, server env %v", b.Front.Env, b.Env)
	}
}
