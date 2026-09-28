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
	// events are the successive answers to `get events`, like pods.
	events []string
	// readErrs fails that many reads of the pod after the first.
	readErrs int
	podReads int
	term     []string
}

func (f *fakeKube) Run(_ context.Context, stdin string, args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	call := strings.Join(args, " ")
	f.calls = append(f.calls, call)
	if stdin != "" {
		f.stdin = append(f.stdin, stdin)
	}
	if strings.Contains(call, " get events ") {
		if len(f.events) == 0 {
			return `{"items":[]}`, nil
		}
		ev := f.events[0]
		if len(f.events) > 1 {
			f.events = f.events[1:]
		}
		return ev, nil
	}
	if strings.Contains(call, " get pod "+PodName) {
		f.podReads++
		if f.readErrs > 0 && f.podReads > 1 {
			f.readErrs--
			return "", errors.New("Unable to connect to the server: net/http: TLS handshake timeout")
		}
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
		"metadata": map[string]any{"uid": "uid-1", "annotations": map[string]string{specAnnotation: hash}},
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
	if !strings.Contains(strings.Join(progress, "\n"), "Starting the terminal container") {
		t.Errorf("progress = %q; want the container's start reported", progress)
	}
}

// A pod that cannot pull its image is reported with the cluster's reason
// and the pull's own error from the kubelet's events, not waited on.
func TestPrepareReportsAnImagePullFailure(t *testing.T) {
	t.Parallel()
	kube := &fakeKube{}
	m := manager(kube, nil)
	kube.pods = []string{pod(t, m, "ImagePullBackOff", false)}
	pullErr := `Failed to pull image "gcr.io/x": dial tcp: lookup gcr.io: no such host`
	kube.events = []string{events(t,
		event("Pulling", `Pulling image "gcr.io/x"`, "uid-1", t0),
		event("Failed", pullErr, "uid-1", t0.Add(time.Minute)),
		event("BackOff", `Back-off pulling image "gcr.io/x"`, "uid-1", t0.Add(2*time.Minute)))}
	err := m.Prepare(context.Background(), nil)
	reason, ok := IsUnavailable(err)
	for _, want := range []string{"ImagePullBackOff", "detail of ImagePullBackOff", pullErr, "cloudburrow prefetch"} {
		if !ok || !strings.Contains(reason, want) {
			t.Errorf("err = %v; want an unavailable reason naming %q", err, want)
		}
	}
}

var t0 = time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)

// clock is a fake Now that moves on by step at every read.
func clock(start time.Time, step time.Duration) func() time.Time {
	var mu sync.Mutex
	now := start
	return func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		now = now.Add(step)
		return now
	}
}

func event(reason, message, uid string, at time.Time) map[string]any {
	return map[string]any{"reason": reason, "message": message,
		"involvedObject": map[string]any{"kind": "Pod", "name": PodName, "uid": uid},
		"firstTimestamp": at.Format(time.RFC3339), "lastTimestamp": at.Format(time.RFC3339)}
}

func events(t *testing.T, items ...map[string]any) string {
	t.Helper()
	if items == nil {
		items = []map[string]any{}
	}
	b, err := json.Marshal(map[string]any{"items": items})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A first use that pulls the image for longer than any budget (the pull
// the issue saw took about a quarter of an hour) is reported as progress,
// with the time since the kubelet started it, until the image is pulled
// and the pod is ready; then with the size the kubelet reports (#824).
func TestPrepareWaitsThroughALongPull(t *testing.T) {
	t.Parallel()
	kube := &fakeKube{}
	m := manager(kube, nil)
	m.cfg.Stall = time.Minute
	// Each read of the clock is a minute on: thirty polls are half an hour.
	m.cfg.Now = clock(t0, time.Minute)
	pulling := event("Pulling", `Pulling image "`+Image+`"`, "uid-1", t0)
	// The first answer is Prepare's read before the wait.
	pods := []string{pod(t, m, "ContainerCreating", false)}
	var evs []string
	for i := 0; i < 30; i++ {
		pods = append(pods, pod(t, m, "ContainerCreating", false))
		evs = append(evs, events(t, event("Scheduled", "assigned", "uid-1", t0), pulling))
	}
	pods = append(pods, pod(t, m, "ContainerCreating", false), pod(t, m, "", true))
	evs = append(evs, events(t, pulling, event("Pulled", `Successfully pulled image "`+Image+`" in 31m0s (31m0s including waiting). Image size: 1083181916 bytes.`, "uid-1", t0.Add(31*time.Minute))))
	kube.pods, kube.events = pods, evs

	var progress []string
	if err := m.Prepare(context.Background(), func(s string) { progress = append(progress, s) }); err != nil {
		t.Fatalf("a pull under way was reported as a failure: %v", err)
	}
	all := strings.Join(progress, "\n")
	for _, want := range []string{"Pulling the terminal image", "still pulling, 20m0s so far", "no byte count",
		"Pulled the terminal image (1083 MB, as the kubelet reports it)"} {
		if !strings.Contains(all, want) {
			t.Errorf("progress lacks %q:\n%s", want, all)
		}
	}
}

// A pull that fails after a long wait is reported with its reason: waiting
// through a pull does not hide its failure.
func TestPrepareReportsAPullThatFailsAfterAWait(t *testing.T) {
	t.Parallel()
	kube := &fakeKube{}
	m := manager(kube, nil)
	m.cfg.Now = clock(t0, time.Minute)
	pulling := event("Pulling", `Pulling image "`+Image+`"`, "uid-1", t0)
	failed := event("Failed", `Failed to pull image: failed to pull and unpack image: context deadline exceeded`, "uid-1", t0.Add(12*time.Minute))
	kube.pods = []string{pod(t, m, "ContainerCreating", false), pod(t, m, "ContainerCreating", false), pod(t, m, "ErrImagePull", false)}
	kube.events = []string{events(t, pulling), events(t, pulling, failed)}
	var progress []string
	err := m.Prepare(context.Background(), func(s string) { progress = append(progress, s) })
	reason, ok := IsUnavailable(err)
	if !ok || !strings.Contains(reason, "ErrImagePull") || !strings.Contains(reason, "context deadline exceeded") {
		t.Errorf("err = %v; want ErrImagePull with the pull's error", err)
	}
	if !strings.Contains(strings.Join(progress, "\n"), "still pulling") {
		t.Errorf("progress = %q; want the pull reported before it failed", progress)
	}
}

// The other real failures end the wait with their reason: a pod that
// cannot be scheduled, and one deleted while it starts.
func TestPrepareReportsUnschedulableAndDeleted(t *testing.T) {
	t.Parallel()
	kube := &fakeKube{}
	m := manager(kube, nil)
	_, hash := m.manifest()
	unschedulable, _ := json.Marshal(map[string]any{
		"metadata": map[string]any{"uid": "uid-1", "annotations": map[string]string{specAnnotation: hash}},
		"status": map[string]any{"phase": "Pending", "conditions": []any{map[string]any{
			"type": "PodScheduled", "status": "False", "reason": "Unschedulable",
			"message": "0/1 nodes are available: 1 Insufficient memory."}}},
	})
	kube.pods = []string{string(unschedulable)}
	reason, ok := IsUnavailable(m.Prepare(context.Background(), nil))
	if !ok || !strings.Contains(reason, "Unschedulable") || !strings.Contains(reason, "Insufficient memory") {
		t.Errorf("unschedulable: reason = %q", reason)
	}

	kube2 := &fakeKube{}
	m2 := manager(kube2, nil)
	kube2.pods = []string{pod(t, m2, "ContainerCreating", false), ""}
	kube2.events = []string{events(t, event("Pulling", "Pulling image", "uid-1", t0))}
	reason, ok = IsUnavailable(m2.Prepare(context.Background(), nil))
	if !ok || !strings.Contains(reason, "deleted") {
		t.Errorf("deleted: reason = %q", reason)
	}
}

// A pod that shows no change while nothing is pulled is given up on after
// Stall, saying what it was waiting for; a failed read of it or two is not
// a failure.
func TestPrepareGivesUpOnAStalledPodOnly(t *testing.T) {
	t.Parallel()
	kube := &fakeKube{readErrs: readRetries - 1}
	m := manager(kube, nil)
	m.cfg.Stall = 5 * time.Minute
	m.cfg.Now = clock(t0, time.Minute)
	_, hash := m.manifest()
	pending, _ := json.Marshal(map[string]any{
		"metadata": map[string]any{"uid": "uid-1", "annotations": map[string]string{specAnnotation: hash}},
		"status":   map[string]any{"phase": "Pending"},
	})
	kube.pods = []string{string(pending)}
	reason, ok := IsUnavailable(m.Prepare(context.Background(), nil))
	if !ok || !strings.Contains(reason, "no progress for 5m0s") || !strings.Contains(reason, "scheduled") {
		t.Errorf("reason = %q, %v; want a stall naming the wait", reason, ok)
	}
}

// Events of an earlier pod of the same name are not this pod's pull, and a
// pull that finished is not under way.
func TestParsePull(t *testing.T) {
	t.Parallel()
	st, ok := parsePull(events(t,
		event("Pulled", `Successfully pulled image "x" in 1s. Image size: 5 bytes.`, "old", t0.Add(time.Hour)),
		event("Pulling", `Pulling image "x"`, "uid-1", t0)), "uid-1")
	if !ok || !st.Active || st.Pulled || !st.Since.Equal(t0) {
		t.Errorf("an earlier pod's Pulled counted: %+v", st)
	}
	st, _ = parsePull(events(t,
		event("Pulling", `Pulling image "x"`, "uid-1", t0),
		event("Pulled", `Container image "x" already present on machine`, "uid-1", t0.Add(time.Second))), "uid-1")
	if st.Active || !st.Pulled || !st.Present || st.Bytes != 0 {
		t.Errorf("an image already present: %+v", st)
	}
	if _, ok := parsePull("not json", "uid-1"); ok {
		t.Error("unreadable events were parsed")
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
