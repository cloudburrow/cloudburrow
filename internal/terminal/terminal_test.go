package terminal

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/k8s"
)

// fakeKube answers the kubectl calls Prepare, Open and SetProject make,
// from a scripted sequence of pod states, and records every call.
type fakeKube struct {
	mu    sync.Mutex
	calls []string
	stdin []string
	// pods are the successive answers to `get pod`; "" is NotFound. The
	// last one repeats.
	pods []string
	term []string
}

func (f *fakeKube) Run(_ context.Context, stdin string, args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	call := strings.Join(args, " ")
	f.calls = append(f.calls, call)
	if stdin != "" {
		f.stdin = append(f.stdin, stdin)
	}
	if strings.Contains(call, " get pod "+PodName) {
		pod := f.pods[0]
		if len(f.pods) > 1 {
			f.pods = f.pods[1:]
		}
		if pod == "" {
			return "", errors.New(`Error from server (NotFound): pods "cloudburrow-terminal" not found`)
		}
		return pod, nil
	}
	return "", nil
}

func (f *fakeKube) StartTerminal(_, _ uint16, args ...string) (k8s.TTY, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.term = args
	return nil, nil
}

func (f *fakeKube) ran(s string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if strings.Contains(c, s) {
			return true
		}
	}
	return false
}

func pod(t *testing.T, m *Manager, waiting string, ready bool) string {
	t.Helper()
	_, hash := m.manifest()
	status := map[string]any{"ready": ready, "state": map[string]any{}}
	if waiting != "" {
		status["state"] = map[string]any{"waiting": map[string]any{"reason": waiting, "message": "detail of " + waiting}}
	}
	b, err := json.Marshal(map[string]any{
		"metadata": map[string]any{"annotations": map[string]string{specAnnotation: hash}},
		"status":   map[string]any{"phase": "Pending", "containerStatuses": []any{status}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func manager(kube *fakeKube, env map[string]string) *Manager {
	runner := func(ns string) *k8s.Runner { return k8s.NewWith(kube, "/kc", "", ns) }
	return New(Config{
		Kube: runner("cloudburrow"), KubeIn: runner, Instance: "demo-x",
		ViewNamespaces: []string{"cloudburrow", "default"},
		Env:            func() map[string]string { return env },
		Poll:           time.Millisecond,
	})
}

// The pod is the pinned image, with the environment it was given and
// nothing of the host's: no host path, no mounted credential, no
// credential variable.
func TestThePodIsPinnedAndCarriesNoHostCredential(t *testing.T) {
	t.Parallel()
	m := manager(&fakeKube{}, map[string]string{
		"CLOUDSDK_API_ENDPOINT_OVERRIDES_STORAGE": "http://storage.cloudburrow.svc.cluster.local:4443/storage/v1/",
		"CLOUDSDK_AUTH_DISABLE_CREDENTIALS":       "true",
	})
	manifest, _ := m.manifest()
	if !strings.Contains(Image, "@sha256:") || !strings.Contains(manifest, Image) {
		t.Errorf("the pod's image is not the digest-pinned Image:\n%s", manifest)
	}
	for _, want := range []string{
		`"name":"CLOUDSDK_API_ENDPOINT_OVERRIDES_STORAGE","value":"http://storage.cloudburrow.svc.cluster.local:4443/storage/v1/"`,
		`"name":"CLOUDSDK_AUTH_DISABLE_CREDENTIALS","value":"true"`,
		`"serviceAccountName":"cloudburrow-terminal"`,
		`"cloudburrow.dev/owned":"true"`, `"cloudburrow.dev/instance":"demo-x"`,
	} {
		if !strings.Contains(manifest, want) {
			t.Errorf("the manifest lacks %s", want)
		}
	}
	for _, never := range []string{"hostPath", "volumes", "GOOGLE_APPLICATION_CREDENTIALS", "privileged", "hostNetwork"} {
		if strings.Contains(manifest, never) {
			t.Errorf("the manifest carries %s", never)
		}
	}
}

// A missing pod is created with read-only access to the instance's and the
// workloads' namespaces, and Prepare reports the image pull while it waits.
func TestPrepareCreatesThePodAndReportsTheWait(t *testing.T) {
	t.Parallel()
	kube := &fakeKube{}
	m := manager(kube, nil)
	kube.pods = []string{"", pod(t, m, "ContainerCreating", false), pod(t, m, "", true)}
	var progress []string
	if err := m.Prepare(context.Background(), func(s string) { progress = append(progress, s) }); err != nil {
		t.Fatal(err)
	}
	if !kube.ran("-n cloudburrow apply -f -") {
		t.Errorf("the pod was not applied: %v", kube.calls)
	}
	var bound []string
	for _, in := range kube.stdin {
		if strings.Contains(in, `"kind":"RoleBinding"`) {
			if !strings.Contains(in, `"name":"view"`) {
				t.Errorf("a RoleBinding grants more than view: %s", in)
			}
			var rb struct {
				Metadata struct{ Namespace string } `json:"metadata"`
			}
			_ = json.Unmarshal([]byte(in), &rb)
			bound = append(bound, rb.Metadata.Namespace)
		}
	}
	if strings.Join(bound, ",") != "cloudburrow,default" {
		t.Errorf("view bound in %v, want cloudburrow and default", bound)
	}
	if !strings.Contains(strings.Join(progress, "\n"), "Pulling the terminal image") {
		t.Errorf("progress = %q; want the image pull reported", progress)
	}
}

// A pod that cannot pull its image is reported with the cluster's reason,
// not waited on until the budget runs out.
func TestPrepareReportsAnImagePullFailure(t *testing.T) {
	t.Parallel()
	kube := &fakeKube{}
	m := manager(kube, nil)
	kube.pods = []string{pod(t, m, "ImagePullBackOff", false)}
	err := m.Prepare(context.Background(), nil)
	reason, ok := IsUnavailable(err)
	if !ok || !strings.Contains(reason, "ImagePullBackOff") || !strings.Contains(reason, "detail of ImagePullBackOff") {
		t.Errorf("err = %v; want an unavailable reason naming ImagePullBackOff", err)
	}
}

// A pod made for other endpoints is replaced: its environment cannot be
// changed in place, and a shell in it would reach the wrong addresses.
func TestPrepareReplacesAPodMadeForAnotherConfiguration(t *testing.T) {
	t.Parallel()
	kube := &fakeKube{}
	m := manager(kube, map[string]string{"STORAGE_EMULATOR_HOST": "http://new:1"})
	old := strings.Replace(pod(t, m, "", true), `"cloudburrow.dev/terminal-spec":"`, `"cloudburrow.dev/terminal-spec":"x`, 1)
	kube.pods = []string{old, pod(t, m, "", true)}
	if err := m.Prepare(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if !kube.ran("delete pod "+PodName) || !kube.ran("apply -f -") {
		t.Errorf("the stale pod was not replaced: %v", kube.calls)
	}
}

// A shell is an interactive bash with the session's project, and a project
// switch is written through its own exec with the project as an argument.
func TestOpenAndSetProject(t *testing.T) {
	t.Parallel()
	kube := &fakeKube{}
	m := manager(kube, nil)
	s, err := m.Open("proj-one", 100, 30)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(kube.term, " ")
	for _, want := range []string{
		"-n cloudburrow exec -i -t " + PodName + " -c shell -- env CLOUDBURROW_SESSION=",
		"CLOUDSDK_CORE_PROJECT=proj-one", "bash --rcfile " + rcPath + " -i",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("exec %q lacks %q", got, want)
		}
	}
	if err := s.SetProject(context.Background(), "proj-two"); err != nil {
		t.Fatal(err)
	}
	if !kube.ran(`sh -c printf '%s' "$1" > "` + stateDir + `/project-$2" sh proj-two ` + s.id) {
		t.Errorf("the project was not written by argument: %v", kube.calls)
	}
	for _, bad := range []string{"p; rm -rf /", "$(id)", "UPPER"} {
		if _, err := m.Open(bad, 80, 24); err == nil {
			t.Errorf("Open(%q) was accepted", bad)
		}
		if err := s.SetProject(context.Background(), bad); err == nil {
			t.Errorf("SetProject(%q) was accepted", bad)
		}
	}
}

// "All projects" in the toolbar is a shell with no project.
func TestOpenWithNoProject(t *testing.T) {
	t.Parallel()
	kube := &fakeKube{}
	if _, err := manager(kube, nil).Open("", 80, 24); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(kube.term, " "), "CLOUDSDK_CORE_PROJECT") {
		t.Errorf("a shell with no project sets one: %v", kube.term)
	}
}
