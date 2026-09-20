package netfwd

import (
	"strings"
	"testing"
)

// In-cluster and host addresses are different things. A workload handed the
// host form cannot connect, which is the failure this separation prevents.
func TestInClusterAddrIsNotTheHostAddr(t *testing.T) {
	t.Parallel()
	tgt := Target{Name: "storage", Namespace: "cloudburrow", ServicePort: 4443}

	if got, want := tgt.InClusterHost(), "storage.cloudburrow.svc.cluster.local"; got != want {
		t.Errorf("InClusterHost() = %q, want %q", got, want)
	}
	if got, want := tgt.InClusterAddr(), "storage.cloudburrow.svc.cluster.local:4443"; got != want {
		t.Errorf("InClusterAddr() = %q, want %q", got, want)
	}
	if strings.Contains(tgt.InClusterAddr(), "127.0.0.1") {
		t.Error("in-cluster address must not be loopback; a pod's loopback is itself")
	}
}

// Before Start there is no host address to report, and saying otherwise would
// hand a caller an endpoint that does not exist.
func TestHostAddrEmptyBeforeStart(t *testing.T) {
	t.Parallel()
	f := New(Target{Name: "pubsub", Namespace: "cloudburrow", ServicePort: 8085}, "/tmp/kc", "")
	if got := f.HostAddr(); got != "" {
		t.Errorf("HostAddr() = %q before Start, want empty", got)
	}
	if f.InClusterAddr() == "" {
		t.Error("InClusterAddr() should be known without starting; it does not depend on a tunnel")
	}
}

// Two instances must get distinct host ports so they can run side by side.
func TestFreePortsAreDistinct(t *testing.T) {
	t.Parallel()
	a, err := freePort("127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	b, err := freePort("127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if a == 0 || b == 0 {
		t.Fatalf("freePort returned 0: %d, %d", a, b)
	}
	if a == b {
		t.Errorf("two reservations returned the same port %d; parallel instances would collide", a)
	}
}

func TestDefaultBindIsLoopback(t *testing.T) {
	t.Parallel()
	f := New(Target{Name: "x", Namespace: "y", ServicePort: 1}, "/tmp/kc", "")
	if f.bindAddr != "127.0.0.1" {
		t.Errorf("default bind = %q, want loopback", f.bindAddr)
	}
}

func TestStopBeforeStartIsSafe(t *testing.T) {
	t.Parallel()
	f := New(Target{Name: "x", Namespace: "y", ServicePort: 1}, "/tmp/kc", "")
	if err := f.Stop(t.Context()); err != nil {
		t.Errorf("Stop() before Start = %v, want nil", err)
	}
}

// The endpoint table must reflect the real, verified per-SDK situation.
func TestEndpointEnvVars(t *testing.T) {
	t.Parallel()
	tests := []struct {
		service   string
		wantVar   string
		wantValue string
	}{
		// Python uses the value verbatim and requires a scheme; Go prepends
		// http:// when absent. The scheme form satisfies both.
		{"storage", "STORAGE_EMULATOR_HOST", "http://127.0.0.1:9001"},
		// Pub/Sub takes a bare host:port.
		{"pubsub", "PUBSUB_EMULATOR_HOST", "127.0.0.1:9001"},
		// No emulator variable exists for these in the official clients.
		{"tasks", "", ""},
		{"run", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.service, func(t *testing.T) {
			t.Parallel()
			e := NewEndpoint(tt.service, "127.0.0.1:9001", "x.y.svc.cluster.local:1")
			if e.EnvVar != tt.wantVar {
				t.Errorf("EnvVar = %q, want %q", e.EnvVar, tt.wantVar)
			}
			if e.EnvValue != tt.wantValue {
				t.Errorf("EnvValue = %q, want %q", e.EnvValue, tt.wantValue)
			}
		})
	}
}

// Services without an environment variable must be called out explicitly, not
// silently omitted — a user would otherwise assume the export list is complete.
func TestPrintEndpointsNamesServicesWithoutEnvVars(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	PrintEndpoints(&b, []Endpoint{
		NewEndpoint("storage", "127.0.0.1:1", "storage.cb.svc.cluster.local:4443"),
		NewEndpoint("tasks", "127.0.0.1:2", "tasks.cb.svc.cluster.local:8080"),
	})
	out := b.String()
	if !strings.Contains(out, "export STORAGE_EMULATOR_HOST=http://127.0.0.1:1") {
		t.Errorf("missing storage export line:\n%s", out)
	}
	if !strings.Contains(out, "no emulator environment variable exists") {
		t.Errorf("tasks must be reported as needing explicit client options:\n%s", out)
	}
	if !strings.Contains(out, "svc.cluster.local") {
		t.Errorf("in-cluster addresses must be shown:\n%s", out)
	}
}
