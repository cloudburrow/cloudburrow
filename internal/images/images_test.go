package images

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeRunner struct {
	out   map[string]string
	err   map[string]error
	calls []string
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	cmd := name + " " + strings.Join(args, " ")
	f.calls = append(f.calls, cmd)
	for k, v := range f.err {
		if strings.Contains(cmd, k) {
			return "", v
		}
	}
	for k, v := range f.out {
		if strings.Contains(cmd, k) {
			return v, nil
		}
	}
	return "", nil
}

func (f *fakeRunner) ran(s string) bool {
	for _, c := range f.calls {
		if strings.Contains(c, s) {
			return true
		}
	}
	return false
}

// The whole reason this package exists: a local image must carry a prefix
// Knative will not try to resolve against a registry.
func TestLocalise(t *testing.T) {
	t.Parallel()
	tests := []struct{ in, want string }{
		{"myapp:v1", "dev.local/myapp:v1"},             // bare local name
		{"dev.local/myapp:v1", "dev.local/myapp:v1"},   // already local
		{"ko.local/myapp:v1", "ko.local/myapp:v1"},     // other accepted prefix
		{"kind.local/myapp:v1", "kind.local/myapp:v1"}, // other accepted prefix
		{"gcr.io/proj/app:v1", "gcr.io/proj/app:v1"},   // real registry, leave alone
		{"localhost:5000/app:v1", "localhost:5000/app:v1"},
		{"docker.io/library/nginx:1", "docker.io/library/nginx:1"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()
			if got := Localise(tt.in); got != tt.want {
				t.Errorf("Localise(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// A locally loaded image must never be pulled: no registry can serve it.
func TestPullPolicy(t *testing.T) {
	t.Parallel()
	if got := PullPolicy("dev.local/app:v1"); got != "Never" {
		t.Errorf("PullPolicy(local) = %q, want Never", got)
	}
	if got := PullPolicy("gcr.io/p/app:v1"); got != "IfNotPresent" {
		t.Errorf("PullPolicy(remote) = %q, want IfNotPresent", got)
	}
}

func TestRequireTagged(t *testing.T) {
	t.Parallel()
	ok := []string{"app:v1", "dev.local/app:v1", "gcr.io/p/app:v1", "app@sha256:" + strings.Repeat("a", 64)}
	bad := []string{"app", "dev.local/app", "gcr.io/p/app"}
	for _, r := range ok {
		if err := RequireTagged(r); err != nil {
			t.Errorf("RequireTagged(%q) = %v, want nil", r, err)
		}
	}
	for _, r := range bad {
		if err := RequireTagged(r); err == nil {
			t.Errorf("RequireTagged(%q) = nil, want rejection of a mutable reference", r)
		}
	}
}

// A non-local reference must be refused before loading, since Knative would
// then try to resolve it against a registry that cannot serve it.
func TestLoadRejectsNonLocal(t *testing.T) {
	t.Parallel()
	l := &Loader{ClusterName: "cloudburrow-t", Runner: &fakeRunner{}}
	err := l.Load(context.Background(), "gcr.io/p/app:v1", "")
	if !errors.Is(err, ErrNotLocal) {
		t.Fatalf("Load() = %v, want ErrNotLocal", err)
	}
	if !strings.Contains(err.Error(), LocalPrefix) {
		t.Errorf("error should suggest the local prefix, got: %v", err)
	}
}

func TestLoadRejectsUntagged(t *testing.T) {
	t.Parallel()
	l := &Loader{ClusterName: "cloudburrow-t", Runner: &fakeRunner{}}
	if err := l.Load(context.Background(), "dev.local/app", ""); err == nil {
		t.Fatal("Load() = nil, want rejection of an untagged reference")
	}
}

// An architecture mismatch must be reported as itself, not surface later as an
// opaque ImagePullBackOff or a crash loop.
func TestLoadDetectsArchMismatch(t *testing.T) {
	t.Parallel()
	r := &fakeRunner{out: map[string]string{
		"docker image inspect": "amd64\n",
		"kubectl":              "arm64",
	}}
	l := &Loader{ClusterName: "cloudburrow-t", Runner: r}
	err := l.Load(context.Background(), "dev.local/app:v1", "/tmp/kubeconfig")
	if !errors.Is(err, ErrArchMismatch) {
		t.Fatalf("Load() = %v, want ErrArchMismatch", err)
	}
	for _, want := range []string{"amd64", "arm64", "--platform"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q so the fix is obvious, got: %v", want, err)
		}
	}
	if r.ran("kind load") {
		t.Error("Load() loaded a mismatched image instead of refusing it")
	}
}

func TestLoadSucceeds(t *testing.T) {
	t.Parallel()
	r := &fakeRunner{out: map[string]string{
		"docker image inspect": "arm64\n",
		"kubectl":              "arm64",
	}}
	l := &Loader{ClusterName: "cloudburrow-t", Runner: r}
	if err := l.Load(context.Background(), "dev.local/app:v1", "/tmp/kubeconfig"); err != nil {
		t.Fatalf("Load() = %v", err)
	}
	if !r.ran("kind load docker-image dev.local/app:v1 --name cloudburrow-t") {
		t.Errorf("expected a kind load into the named cluster, got: %v", r.calls)
	}
}

// A missing local image is reported as missing rather than as a load failure.
func TestLoadReportsMissingImage(t *testing.T) {
	t.Parallel()
	r := &fakeRunner{err: map[string]error{"docker image inspect": errors.New("No such image")}}
	l := &Loader{ClusterName: "cloudburrow-t", Runner: r}
	err := l.Load(context.Background(), "dev.local/app:v1", "")
	if !errors.Is(err, ErrNotPresent) {
		t.Fatalf("Load() = %v, want ErrNotPresent", err)
	}
}
