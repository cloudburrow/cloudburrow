package cluster

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSameNodeImage(t *testing.T) {
	t.Parallel()
	const digest = "sha256:099e049362a1526b2db71494e1947aae99bd16290d7c895f2b7ea312e3cbfaed"
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"kindest/node:v1.36.4", "kindest/node:v1.36.4", true},
		// A cluster created before #597 pinned the digest records the tag
		// alone; it is the same image.
		{"kindest/node:v1.36.4", "kindest/node:v1.36.4@" + digest, true},
		{"kindest/node:v1.36.4@" + digest, "kindest/node@" + digest, true},
		{"docker.io/kindest/node:v1.36.4", "kindest/node:v1.36.4", true},
		{"kindest/node:v1.35.0", "kindest/node:v1.36.4@" + digest, false},
		{"kindest/node:v1.36.4@sha256:aaaa", "kindest/node:v1.36.4@" + digest, false},
		{"localhost:5000/node:v1", "localhost:5000/node:v1", true},
		{"localhost:5000/node:v1", "localhost:5000/node:v2", false},
		{"", "kindest/node:v1.36.4", false},
	} {
		if got := SameNodeImage(c.a, c.b); got != c.want {
			t.Errorf("SameNodeImage(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestCompareKnative(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		a, b   string
		cmp    int
		parsed bool
	}{
		{"knative-v1.23.0", "knative-v1.23.0", 0, true},
		{"knative-v1.22.3", "knative-v1.23.0", -1, true},
		{"knative-v1.10.0", "knative-v1.9.0", 1, true},
		{"", "knative-v1.23.0", 0, false},
		{"nightly", "knative-v1.23.0", 0, false},
	} {
		cmp, ok := CompareKnative(c.a, c.b)
		if cmp != c.cmp || ok != c.parsed {
			t.Errorf("CompareKnative(%q, %q) = %d, %v; want %d, %v", c.a, c.b, cmp, ok, c.cmp, c.parsed)
		}
	}
}

// stampCluster is a fake cluster for the stamp: an existing, running
// cluster whose stamp holds annotations (nil for no stamp at all) and whose
// node container runs nodeImage ("" when Docker cannot say).
type stampCluster struct {
	mu          sync.Mutex
	calls       []string
	annotations map[string]string
	nodeImage   string
}

func (f *stampCluster) Run(_ context.Context, name string, args ...string) (string, error) {
	call := name + " " + strings.Join(args, " ")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
	switch {
	case call == "docker info --format {{.ServerVersion}}":
		return "27.0.0", nil
	case call == "kind get clusters":
		return "cloudburrow-test\n", nil
	case strings.HasPrefix(call, "kind get nodes"):
		return "cloudburrow-test-control-plane\n", nil
	case strings.HasPrefix(call, "docker inspect -f {{.State.Running}}"):
		return "true\n", nil
	case strings.HasPrefix(call, "docker inspect -f {{.Config.Image}}"):
		if f.nodeImage == "" {
			return "", errors.New("no such object")
		}
		return f.nodeImage + "\n", nil
	case strings.Contains(call, "get configmap "+StampName+" --ignore-not-found -o json"):
		if f.annotations == nil {
			return "", nil
		}
		var kv []string
		for k, v := range f.annotations {
			kv = append(kv, fmt.Sprintf("%q:%q", k, v))
		}
		return `{"metadata":{"annotations":{` + strings.Join(kv, ",") + `}}}`, nil
	case strings.Contains(call, "get configmap "+StampName+" --ignore-not-found -o name"):
		if f.annotations == nil {
			return "", nil
		}
		return "configmap/" + StampName, nil
	case strings.Contains(call, "create configmap "+StampName):
		f.annotations = map[string]string{}
	case strings.Contains(call, "annotate --overwrite configmap/"+StampName):
		for _, a := range args {
			if k, v, ok := strings.Cut(a, "="); ok && strings.HasPrefix(k, "cloudburrow.dev/") {
				f.annotations[k] = v
			}
		}
	}
	return "", nil
}

func (f *stampCluster) wrote() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if strings.Contains(c, " annotate ") || strings.Contains(c, " create configmap ") {
			return true
		}
	}
	return false
}

const pinned = "kindest/node:v1.36.4@sha256:099e049362a1526b2db71494e1947aae99bd16290d7c895f2b7ea312e3cbfaed"

func stampTestCluster(t *testing.T, f *stampCluster) *Cluster {
	t.Helper()
	c, err := New(Options{Name: "cloudburrow-test", NodeImage: pinned, CLIVersion: "v0.9.0",
		Kubeconfig: filepath.Join(t.TempDir(), "kubeconfig"), Runner: f, LookPath: dockerPresent})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// A cluster this `up` created is stamped with the image it was created
// with and the CLI that created it.
func TestReconcileStampsACreatedCluster(t *testing.T) {
	t.Parallel()
	f := &stampCluster{}
	got, _, err := stampTestCluster(t, f).Reconcile(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if f.annotations[AnnotationNodeImage] != pinned || f.annotations[AnnotationCLIVersion] != "v0.9.0" {
		t.Errorf("stamp = %v", f.annotations)
	}
	if !got.Present || got.NodeImage != pinned || got.CLIVersion != "v0.9.0" {
		t.Errorf("Reconcile returned %+v", got)
	}
	for _, c := range f.calls {
		if strings.Contains(c, "label --overwrite configmap/"+StampName+" cloudburrow.dev/owned=true") {
			return
		}
	}
	t.Errorf("the stamp was not labelled as owned:\n%s", strings.Join(f.calls, "\n"))
}

// An existing cluster stamped with another node image is refused, and the
// message names `cloudburrow delete`, the only way to change it (#601).
// Nothing is written: the stamp keeps saying what the cluster is.
func TestReconcileRefusesAnotherNodeImage(t *testing.T) {
	t.Parallel()
	f := &stampCluster{annotations: map[string]string{AnnotationNodeImage: "kindest/node:v1.35.0", AnnotationCLIVersion: "v0.1.0"}}
	_, _, err := stampTestCluster(t, f).Reconcile(context.Background(), false)
	if !errors.Is(err, ErrNodeImageDrift) {
		t.Fatalf("err = %v, want ErrNodeImageDrift", err)
	}
	for _, want := range []string{"cloudburrow delete", "kindest/node:v1.35.0", pinned} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
	if f.wrote() {
		t.Errorf("a refused cluster's stamp was written:\n%s", strings.Join(f.calls, "\n"))
	}
}

// A matching stamp is kept, and the CLI version brought up to date.
func TestReconcileKeepsAMatchingCluster(t *testing.T) {
	t.Parallel()
	f := &stampCluster{annotations: map[string]string{AnnotationNodeImage: pinned, AnnotationCLIVersion: "v0.1.0",
		AnnotationKnativeVersion: "knative-v1.22.0"}}
	got, note, err := stampTestCluster(t, f).Reconcile(context.Background(), false)
	if err != nil || note != "" {
		t.Fatalf("Reconcile = %v, %q", err, note)
	}
	if f.annotations[AnnotationCLIVersion] != "v0.9.0" || f.annotations[AnnotationNodeImage] != pinned {
		t.Errorf("stamp = %v", f.annotations)
	}
	// Knative is the components' to judge; the cluster reports it as found.
	if got.KnativeVersion != "knative-v1.22.0" {
		t.Errorf("Reconcile returned %+v", got)
	}
}

// A cluster from before the stamp is judged by the image its node container
// runs. The pre-#597 pin, the same tag without a digest, is the same image
// and is accepted and stamped; another image is refused; and one Docker
// cannot report is said to be unknown and left unrecorded, not guessed.
func TestReconcileAnUnstampedCluster(t *testing.T) {
	t.Parallel()
	t.Run("same tag", func(t *testing.T) {
		t.Parallel()
		f := &stampCluster{nodeImage: "kindest/node:v1.36.4"}
		got, note, err := stampTestCluster(t, f).Reconcile(context.Background(), false)
		if err != nil || note != "" {
			t.Fatalf("Reconcile = %v, %q", err, note)
		}
		if f.annotations[AnnotationNodeImage] != "kindest/node:v1.36.4" || got.NodeImage != "kindest/node:v1.36.4" {
			t.Errorf("stamp = %v, returned %+v; want the observed image", f.annotations, got)
		}
	})
	t.Run("another image", func(t *testing.T) {
		t.Parallel()
		f := &stampCluster{nodeImage: "kindest/node:v1.33.1"}
		_, _, err := stampTestCluster(t, f).Reconcile(context.Background(), false)
		if !errors.Is(err, ErrNodeImageDrift) || !strings.Contains(err.Error(), "runs node image kindest/node:v1.33.1") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("unknown", func(t *testing.T) {
		t.Parallel()
		f := &stampCluster{}
		got, note, err := stampTestCluster(t, f).Reconcile(context.Background(), false)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(note, "node image unknown") {
			t.Errorf("note = %q", note)
		}
		if _, ok := f.annotations[AnnotationNodeImage]; ok || got.NodeImage != "" {
			t.Errorf("an unknown image was recorded: %v", f.annotations)
		}
		if f.annotations[AnnotationCLIVersion] != "v0.9.0" {
			t.Errorf("the CLI version was not recorded: %v", f.annotations)
		}
	})
}

// `up` against an existing cluster with another node image fails the
// cluster component, so `up` exits non-zero before anything is installed.
func TestComponentStartRefusesNodeImageDrift(t *testing.T) {
	t.Parallel()
	f := &stampCluster{annotations: map[string]string{AnnotationNodeImage: "kindest/node:v1.35.0"}}
	comp := NewComponent(stampTestCluster(t, f), "", time.Second, io.Discard)
	err := comp.Start(context.Background())
	if !errors.Is(err, ErrNodeImageDrift) || !strings.Contains(err.Error(), "cloudburrow delete") {
		t.Fatalf("Start = %v; want ErrNodeImageDrift naming `cloudburrow delete`", err)
	}
}

func TestReadStampOfAnUnstampedCluster(t *testing.T) {
	t.Parallel()
	s, err := ReadStamp(context.Background(), func(context.Context, ...string) (string, error) { return "", nil })
	if err != nil || s.Present {
		t.Errorf("ReadStamp = %+v, %v; want not present", s, err)
	}
	if _, err := ReadStamp(context.Background(), func(context.Context, ...string) (string, error) {
		return "", errors.New("connection refused")
	}); err == nil {
		t.Error("a failed read was reported as no stamp")
	}
}
