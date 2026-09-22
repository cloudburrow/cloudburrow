package netfwd

import (
	"os"
	"os/exec"
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

// The host address must be known immediately after New, before Start.
//
// This is what lets the storage backend be deployed already knowing the address
// its clients will use. Discovering it only after the tunnel was up would mean
// patching the Deployment, replacing the pod, and breaking that same tunnel.
func TestHostAddrKnownBeforeStart(t *testing.T) {
	t.Parallel()
	f := New(Target{Name: "pubsub", Namespace: "cloudburrow", ServicePort: 8085}, "/tmp/kc", "")
	addr := f.HostAddr()
	if addr == "" {
		t.Fatal("HostAddr() is empty before Start; the backend could not be told where clients will reach it")
	}
	if !strings.HasPrefix(addr, "127.0.0.1:") {
		t.Errorf("HostAddr() = %q, want a loopback address", addr)
	}
	if f.InClusterAddr() == "" {
		t.Error("InClusterAddr() should be known without starting; it does not depend on a tunnel")
	}
}

// An explicitly requested host port is honoured rather than replaced.
func TestExplicitHostPortIsKept(t *testing.T) {
	t.Parallel()
	f := New(Target{Name: "s", Namespace: "n", ServicePort: 1, HostPort: 19999}, "/tmp/kc", "")
	if got, want := f.HostAddr(), "127.0.0.1:19999"; got != want {
		t.Errorf("HostAddr() = %q, want %q", got, want)
	}
}

// Two forwarders must reserve different host ports.
func TestForwardersReserveDistinctPorts(t *testing.T) {
	t.Parallel()
	a := New(Target{Name: "a", Namespace: "n", ServicePort: 1}, "/tmp/kc", "")
	b := New(Target{Name: "b", Namespace: "n", ServicePort: 1}, "/tmp/kc", "")
	if a.HostAddr() == b.HostAddr() {
		t.Errorf("both forwarders reserved %s; parallel instances would collide", a.HostAddr())
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

// Every optional emulator has an official environment variable, and each takes
// a bare host:port. Storage remains the only one needing a scheme.
func TestOptionalServiceEnvVars(t *testing.T) {
	t.Parallel()
	want := map[string]string{
		"firestore": "FIRESTORE_EMULATOR_HOST",
		"datastore": "DATASTORE_EMULATOR_HOST",
		"bigtable":  "BIGTABLE_EMULATOR_HOST",
		"spanner":   "SPANNER_EMULATOR_HOST",
	}
	for service, envVar := range want {
		e := NewEndpoint(service, "127.0.0.1:1234", "svc:1234")
		if e.EnvVar != envVar {
			t.Errorf("%s EnvVar = %q, want %q", service, e.EnvVar, envVar)
		}
		if e.EnvValue != "127.0.0.1:1234" {
			t.Errorf("%s EnvValue = %q, want a bare host:port", service, e.EnvValue)
		}
	}
}

// Cloud SQL's address says what it is not.
//
// Every other endpoint on that list is an emulator of the service it names.
// Cloud SQL is a real PostgreSQL with no management API behind it, because
// Google publishes no Cloud SQL emulator to run — and the endpoint list is
// the one place a reader is guaranteed to be looking.
func TestCloudSQLEndpointStatesItsScope(t *testing.T) {
	t.Parallel()
	var out strings.Builder
	PrintEndpoints(&out, []Endpoint{
		NewEndpoint("cloudsql", "127.0.0.1:5432", "cloudsql.cb.svc.cluster.local:5432"),
		NewEndpoint("pubsub", "127.0.0.1:8085", "pubsub.cb.svc.cluster.local:8085"),
	})

	text := out.String()
	if !strings.Contains(text, "not the Cloud SQL Admin API") {
		t.Errorf("the Cloud SQL endpoint does not say what it is not:\n%s", text)
	}
	// And no other service claims a caveat it does not have.
	if strings.Count(text, "not the Cloud SQL Admin API") != 1 {
		t.Error("the caveat is attached to more than one service")
	}
}

// A tunnel's restart count moves when the tunnel is re-established, and its
// running state reflects now rather than startup.
//
// Readiness latched at startup answers "did this ever work", which is a
// different question from the one a dashboard is asked.
func TestForwarderReportsLiveStateAndRestarts(t *testing.T) {
	f := &Forwarder{target: Target{Name: "probe", Namespace: "cb", ServicePort: 1}}

	if f.Running() {
		t.Error("a forwarder that was never started reports running")
	}
	if got := f.Restarts(); got != 0 {
		t.Errorf("restarts = %d before anything happened", got)
	}

	// A live tunnel: a process, and a done channel that has not fired.
	f.mu.Lock()
	f.cmd = &exec.Cmd{Process: &os.Process{Pid: os.Getpid()}}
	f.done = make(chan struct{})
	f.mu.Unlock()
	if !f.Running() {
		t.Error("a tunnel with a live process reports not running")
	}

	// The pod went away. The supervisor has not re-established it yet, and
	// the console must not report healthy in that window.
	f.mu.Lock()
	close(f.done)
	f.mu.Unlock()
	if f.Running() {
		t.Error("a tunnel whose process has exited still reports running — " +
			"this is the state that made the dashboard answer the wrong question")
	}

	// And a re-establishment is counted, because surviving a restart and
	// never noticing one are different states.
	f.mu.Lock()
	f.restarts++
	f.done = make(chan struct{})
	f.mu.Unlock()
	if got := f.Restarts(); got != 1 {
		t.Errorf("restarts = %d after one re-establishment", got)
	}
	if !f.Running() {
		t.Error("a re-established tunnel reports not running")
	}
}
