package cluster

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeRunner records commands and returns scripted results, so the ownership
// and error-mapping logic is tested without Docker or a real cluster.
type fakeRunner struct {
	mu      sync.Mutex
	calls   []string
	results map[string]result
	// fallback is returned for commands with no scripted result.
	fallback result
}

type result struct {
	out string
	err error
}

func newRunner() *fakeRunner {
	return &fakeRunner{results: map[string]result{}}
}

func (f *fakeRunner) script(prefix string, out string, err error) *fakeRunner {
	f.results[prefix] = result{out: out, err: err}
	return f
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	cmd := name + " " + strings.Join(args, " ")
	f.mu.Lock()
	f.calls = append(f.calls, cmd)
	f.mu.Unlock()

	for prefix, r := range f.results {
		if strings.HasPrefix(cmd, prefix) {
			return r.out, r.err
		}
	}
	return f.fallback.out, f.fallback.err
}

func (f *fakeRunner) ran(substr string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if strings.Contains(c, substr) {
			return true
		}
	}
	return false
}

func (f *fakeRunner) callsContaining(substr string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.Contains(c, substr) {
			n++
		}
	}
	return n
}

// newTestCluster builds a Cluster wired to a fake runner and a temp kubeconfig.
func newTestCluster(t *testing.T, r Runner, name string) *Cluster {
	t.Helper()
	c, err := New(Options{
		Name:       name,
		NodeImage:  "kindest/node:v1.36.4",
		Kubeconfig: filepath.Join(t.TempDir(), "kubeconfig"),
		Runner:     r,
	})
	if err != nil {
		t.Fatalf("New() = %v", err)
	}
	return c
}

// The constructor is the chokepoint for ownership: no code path may build a
// Cluster pointed at something CloudBurrow did not create.
func TestNewRejectsUnownedAndUnpinned(t *testing.T) {
	t.Parallel()
	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	tests := []struct {
		name    string
		opts    Options
		wantErr error
		wantMsg string
	}{
		{
			name:    "foreign cluster name",
			opts:    Options{Name: "prod-cluster", NodeImage: "kindest/node:v1.36.4", Kubeconfig: kubeconfig},
			wantErr: ErrNotOwned,
		},
		{
			name:    "empty name",
			opts:    Options{NodeImage: "kindest/node:v1.36.4", Kubeconfig: kubeconfig},
			wantMsg: "name must not be empty",
		},
		{
			name:    "unpinned node image",
			opts:    Options{Name: "cloudburrow-x", NodeImage: "kindest/node", Kubeconfig: kubeconfig},
			wantMsg: "explicit tag or digest",
		},
		{
			name:    "missing kubeconfig path",
			opts:    Options{Name: "cloudburrow-x", NodeImage: "kindest/node:v1.36.4"},
			wantMsg: "kubeconfig path must be set",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := New(tt.opts)
			if err == nil {
				t.Fatal("New() = nil, want error")
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Errorf("New() = %v, want %v", err, tt.wantErr)
			}
			if tt.wantMsg != "" && !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("New() = %v, want containing %q", err, tt.wantMsg)
			}
		})
	}
}

func TestNewAcceptsOwnedAndPinned(t *testing.T) {
	t.Parallel()
	for _, img := range []string{"kindest/node:v1.36.4", "kindest/node@sha256:" + strings.Repeat("a", 64)} {
		if _, err := New(Options{Name: "cloudburrow-dev", NodeImage: img,
			Kubeconfig: filepath.Join(t.TempDir(), "kc")}); err != nil {
			t.Errorf("New(%s) = %v, want nil", img, err)
		}
	}
}

func TestExistsAndStatus(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		clusters   string
		nodes      string
		running    string
		wantExists bool
		wantStatus Status
	}{
		{"absent", "other-cluster\n", "", "", false, StatusAbsent},
		{"running", "cloudburrow-dev\nother\n", "cloudburrow-dev-control-plane\n", "true\n", true, StatusRunning},
		{"stopped", "cloudburrow-dev\n", "cloudburrow-dev-control-plane\n", "false\n", true, StatusStopped},
		{"exact match only", "cloudburrow-dev-extra\n", "", "", false, StatusAbsent},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := newRunner().
				script("kind get clusters", tt.clusters, nil).
				script("kind get nodes", tt.nodes, nil).
				script("docker inspect", tt.running, nil)
			c := newTestCluster(t, r, "cloudburrow-dev")

			gotExists, err := c.Exists(context.Background())
			if err != nil {
				t.Fatalf("Exists() = %v", err)
			}
			if gotExists != tt.wantExists {
				t.Errorf("Exists() = %v, want %v", gotExists, tt.wantExists)
			}
			gotStatus, err := c.Status(context.Background())
			if err != nil {
				t.Fatalf("Status() = %v", err)
			}
			if gotStatus != tt.wantStatus {
				t.Errorf("Status() = %v, want %v", gotStatus, tt.wantStatus)
			}
		})
	}
}

// Repeated `up` must not recreate a running cluster.
func TestCreateIsIdempotent(t *testing.T) {
	t.Parallel()
	r := newRunner().
		script("kind get clusters", "cloudburrow-dev\n", nil).
		script("kind get nodes", "cloudburrow-dev-control-plane\n", nil).
		script("docker inspect", "true\n", nil)
	c := newTestCluster(t, r, "cloudburrow-dev")

	if err := c.Create(context.Background(), ""); err != nil {
		t.Fatalf("Create() = %v", err)
	}
	if r.ran("kind create cluster") {
		t.Error("Create() recreated an already-running cluster")
	}
	if !r.ran("kind export kubeconfig") {
		t.Error("Create() should refresh the kubeconfig for an existing cluster")
	}
}

// A stopped cluster is started rather than recreated, so its volumes survive.
func TestCreateStartsStoppedCluster(t *testing.T) {
	t.Parallel()
	r := newRunner().
		script("kind get clusters", "cloudburrow-dev\n", nil).
		script("kind get nodes", "cloudburrow-dev-control-plane\n", nil).
		script("docker inspect", "false\n", nil)
	c := newTestCluster(t, r, "cloudburrow-dev")

	if err := c.Create(context.Background(), ""); err != nil {
		t.Fatalf("Create() = %v", err)
	}
	if r.ran("kind create cluster") {
		t.Error("Create() recreated a stopped cluster; its state would be lost")
	}
	if !r.ran("docker start") {
		t.Error("Create() should start the stopped nodes")
	}
}

// A failed create must not leave a partial cluster behind.
func TestCreateCleansUpPartialFailure(t *testing.T) {
	t.Parallel()
	boom := errors.New("node image pull failed")
	calls := 0
	r := newRunner().
		script("kind create cluster", "", boom).
		script("kind get nodes", "", nil).
		script("docker inspect", "", nil)
	// The cluster is absent before create, present after the failed attempt.
	r2 := &sequencedRunner{inner: r, onGetClusters: func() string {
		calls++
		if calls == 1 {
			return "" // absent before
		}
		return "cloudburrow-dev\n" // partial cluster left behind
	}}
	c := newTestCluster(t, r2, "cloudburrow-dev")

	err := c.Create(context.Background(), "")
	if err == nil {
		t.Fatal("Create() = nil, want error")
	}
	if !errors.Is(err, boom) {
		t.Errorf("Create() = %v, want wrapping %v", err, boom)
	}
	if !r.ran("kind delete cluster") {
		t.Error("failed Create() must clean up the partial cluster it created")
	}
}

// sequencedRunner lets one command return different results across calls.
type sequencedRunner struct {
	inner         *fakeRunner
	onGetClusters func() string
}

func (s *sequencedRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	cmd := name + " " + strings.Join(args, " ")
	if strings.HasPrefix(cmd, "kind get clusters") {
		s.inner.mu.Lock()
		s.inner.calls = append(s.inner.calls, cmd)
		s.inner.mu.Unlock()
		return s.onGetClusters(), nil
	}
	return s.inner.Run(ctx, name, args...)
}

// Delete is the destructive path, so ownership is re-checked there rather than
// trusting the constructor.
func TestDeleteRefusesUnownedCluster(t *testing.T) {
	t.Parallel()
	r := newRunner().script("kind get clusters", "prod\n", nil)
	// Bypass the constructor to simulate a corrupted or hand-built value.
	c := &Cluster{opts: Options{Name: "prod", NodeImage: "kindest/node:v1.36.4",
		Kubeconfig: filepath.Join(t.TempDir(), "kc")}, runner: r}

	err := c.Delete(context.Background())
	if !errors.Is(err, ErrNotOwned) {
		t.Fatalf("Delete() = %v, want ErrNotOwned", err)
	}
	if r.ran("kind delete cluster") {
		t.Error("Delete() issued a delete for an unowned cluster")
	}
}

func TestDeleteAbsentCluster(t *testing.T) {
	t.Parallel()
	r := newRunner().script("kind get clusters", "", nil)
	c := newTestCluster(t, r, "cloudburrow-dev")
	err := c.Delete(context.Background())
	if !errors.Is(err, ErrAbsent) {
		t.Fatalf("Delete() = %v, want ErrAbsent", err)
	}
}

// Delete removes only our own kubeconfig, never a shared one.
func TestDeleteRemovesOnlyOwnKubeconfig(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ours := filepath.Join(dir, "kubeconfig")
	theirs := filepath.Join(dir, "developer-config")
	for _, p := range []string{ours, theirs} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	r := newRunner().
		script("kind get clusters", "cloudburrow-dev\n", nil).
		script("docker info", "29.0.0\n", nil)
	c, err := New(Options{Name: "cloudburrow-dev", NodeImage: "kindest/node:v1.36.4",
		Kubeconfig: ours, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(context.Background()); err != nil {
		t.Fatalf("Delete() = %v", err)
	}
	if _, err := os.Stat(ours); !os.IsNotExist(err) {
		t.Error("our kubeconfig was not removed")
	}
	if _, err := os.Stat(theirs); err != nil {
		t.Error("Delete() removed a kubeconfig it does not own")
	}
}

// Stop preserves the cluster; only the node containers are stopped.
func TestStopDoesNotDelete(t *testing.T) {
	t.Parallel()
	r := newRunner().
		script("kind get clusters", "cloudburrow-dev\n", nil).
		script("kind get nodes", "cloudburrow-dev-control-plane\n", nil).
		script("docker info", "29.0.0\n", nil)
	c := newTestCluster(t, r, "cloudburrow-dev")

	if err := c.Stop(context.Background()); err != nil {
		t.Fatalf("Stop() = %v", err)
	}
	if !r.ran("docker stop cloudburrow-dev-control-plane") {
		t.Error("Stop() did not stop the node container")
	}
	if r.ran("kind delete cluster") {
		t.Error("Stop() deleted the cluster; stop must preserve it")
	}
}

func TestStopAbsentCluster(t *testing.T) {
	t.Parallel()
	r := newRunner().
		script("kind get clusters", "", nil).
		script("docker info", "29.0.0\n", nil)
	c := newTestCluster(t, r, "cloudburrow-dev")
	if err := c.Stop(context.Background()); !errors.Is(err, ErrAbsent) {
		t.Fatalf("Stop() = %v, want ErrAbsent", err)
	}
}

// Docker problems must be actionable and reported only when Docker is needed.
func TestCheckRuntimeReportsDaemonFailure(t *testing.T) {
	t.Parallel()
	r := newRunner().script("docker info", "", errors.New("Cannot connect to the Docker daemon"))
	c := newTestCluster(t, r, "cloudburrow-dev")
	err := c.CheckRuntime(context.Background())
	if !errors.Is(err, ErrDockerUnavailable) {
		t.Fatalf("CheckRuntime() = %v, want ErrDockerUnavailable", err)
	}
	if !strings.Contains(err.Error(), "start Docker") {
		t.Errorf("error should tell the user what to do, got: %v", err)
	}
}

func TestServerVersion(t *testing.T) {
	t.Parallel()
	t.Run("parses gitVersion", func(t *testing.T) {
		t.Parallel()
		r := newRunner().script("kubectl", `{"clientVersion":{"gitVersion":"v1.99.0"},"serverVersion":{"gitVersion":"v1.36.4"}}`, nil)
		c := newTestCluster(t, r, "cloudburrow-dev")
		got, err := c.ServerVersion(context.Background())
		if err != nil {
			t.Fatalf("ServerVersion() = %v", err)
		}
		if got != "v1.36.4" {
			t.Errorf("ServerVersion() = %q, want v1.36.4 (the server, not the client)", got)
		}
	})

	t.Run("reports a missing server version", func(t *testing.T) {
		t.Parallel()
		r := newRunner().script("kubectl", `{"clientVersion":{"gitVersion":"v1.99.0"}}`, nil)
		c := newTestCluster(t, r, "cloudburrow-dev")
		if _, err := c.ServerVersion(context.Background()); err == nil {
			t.Error("ServerVersion() = nil error when the server did not answer")
		}
	})
}

// WaitReady polls rather than sleeping a fixed amount, and honours its bound.
func TestWaitReady(t *testing.T) {
	t.Parallel()

	t.Run("succeeds once the API answers", func(t *testing.T) {
		t.Parallel()
		attempts := 0
		r := &funcRunner{fn: func(cmd string) (string, error) {
			if strings.Contains(cmd, "readyz") {
				attempts++
				if attempts < 3 {
					return "", errors.New("connection refused")
				}
				return "ok", nil
			}
			return "", nil
		}}
		c := newTestCluster(t, r, "cloudburrow-dev")
		if err := c.WaitReady(context.Background(), 10*time.Second); err != nil {
			t.Fatalf("WaitReady() = %v", err)
		}
		if attempts < 3 {
			t.Errorf("expected repeated polling, got %d attempts", attempts)
		}
	})

	t.Run("reports the last error on timeout", func(t *testing.T) {
		t.Parallel()
		r := &funcRunner{fn: func(string) (string, error) {
			return "", errors.New("connection refused")
		}}
		c := newTestCluster(t, r, "cloudburrow-dev")
		err := c.WaitReady(context.Background(), 200*time.Millisecond)
		if err == nil {
			t.Fatal("WaitReady() = nil, want timeout error")
		}
		if !strings.Contains(err.Error(), "connection refused") {
			t.Errorf("timeout error should carry the underlying cause, got: %v", err)
		}
	})

	t.Run("honours context cancellation", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		r := &funcRunner{fn: func(string) (string, error) { return "", errors.New("nope") }}
		c := newTestCluster(t, r, "cloudburrow-dev")
		if err := c.WaitReady(ctx, time.Minute); !errors.Is(err, context.Canceled) {
			t.Errorf("WaitReady() = %v, want context.Canceled", err)
		}
	})
}

type funcRunner struct {
	fn func(cmd string) (string, error)
}

func (f *funcRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	return f.fn(name + " " + strings.Join(args, " "))
}

// Two instances must operate on separate clusters and kubeconfigs.
func TestTwoInstancesAreIndependent(t *testing.T) {
	t.Parallel()
	r := newRunner().script("kind get clusters", "cloudburrow-a\ncloudburrow-b\n", nil)
	a := newTestCluster(t, r, "cloudburrow-a")
	b := newTestCluster(t, r, "cloudburrow-b")

	if a.Name() == b.Name() || a.KubeconfigPath() == b.KubeconfigPath() {
		t.Fatal("instances share a cluster name or kubeconfig")
	}
	for _, c := range []*Cluster{a, b} {
		ok, err := c.Exists(context.Background())
		if err != nil || !ok {
			t.Errorf("%s Exists() = %v, %v; want true", c.Name(), ok, err)
		}
	}
}

// The developer's kubeconfig is never written to: every kind invocation must
// carry an explicit --kubeconfig.
func TestKubeconfigIsAlwaysExplicit(t *testing.T) {
	t.Parallel()
	r := newRunner().
		script("kind get clusters", "", nil).
		script("docker info", "29.0.0\n", nil)
	c := newTestCluster(t, r, "cloudburrow-dev")
	_ = c.Create(context.Background(), "")

	// Snapshot under the lock, then assert without it: fakeRunner's mutex is
	// not reentrant, and callsContaining takes it too.
	r.mu.Lock()
	snapshot := append([]string(nil), r.calls...)
	r.mu.Unlock()

	for _, cmd := range snapshot {
		if strings.HasPrefix(cmd, "kind create cluster") || strings.HasPrefix(cmd, "kind export kubeconfig") {
			if !strings.Contains(cmd, "--kubeconfig "+c.KubeconfigPath()) {
				t.Errorf("command must pass an explicit --kubeconfig: %s", cmd)
			}
		}
	}
	if n := r.callsContaining("--kubeconfig"); n == 0 {
		t.Errorf("no command passed --kubeconfig; the developer's default file could be modified. calls: %v", snapshot)
	}
}
