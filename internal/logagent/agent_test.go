package logagent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/logging/apiv2/loggingpb"
	ltype "google.golang.org/genproto/googleapis/logging/type"
)

type fakeCluster struct {
	mu   sync.Mutex
	cfg  Config
	pods []Pod
}

func (f *fakeCluster) Config(context.Context) (Config, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cfg, nil
}

func (f *fakeCluster) Pods(context.Context) ([]Pod, error) { return f.pods, nil }

// sink records what is written, failing while down.
type sink struct {
	mu      sync.Mutex
	down    bool
	entries []*loggingpb.LogEntry
}

func (s *sink) write(_ context.Context, req *loggingpb.WriteLogEntriesRequest) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.down {
		return errors.New("connection refused")
	}
	s.entries = append(s.entries, req.GetEntries()...)
	return nil
}

func (s *sink) texts() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, e := range s.entries {
		out = append(out, e.GetTextPayload())
	}
	return out
}

var t0 = time.Date(2026, 11, 22, 13, 0, 0, 0, time.UTC)

func criLine(at time.Time, stream, tag, text string) string {
	return at.Format(time.RFC3339Nano) + " " + stream + " " + tag + " " + text + "\n"
}

func appendFile(t *testing.T, path, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
}

func setup(t *testing.T) (root, file string, c *fakeCluster) {
	t.Helper()
	root = t.TempDir()
	dir := filepath.Join(root, "default_web-1_u1", "app")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	c = &fakeCluster{
		cfg: Config{Project: "p", Location: "us-central1-a", Cluster: "c1",
			Categories: map[string]Window{Workload: {On: true}, System: {On: true}}},
		pods: []Pod{{Namespace: "default", Name: "web-1", UID: "u1", Labels: map[string]string{"app": "web"}}},
	}
	return root, filepath.Join(dir, "0.log"), c
}

func newAgent(root string, c Cluster, s *sink, now *time.Time) *Agent {
	return &Agent{Node: "n1", Root: root, Positions: filepath.Join(filepath.Dir(root), filepath.Base(root)+".pos"),
		Cluster: c, Write: s.write, Now: func() time.Time { return *now }}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestAgentShipsOnceAcrossRestartsAndOutages (#2100): every line is
// shipped once, in order, on k8s_container with GKE's labels: across a
// restart of the agent, which reads its positions back, and across an
// outage of Cloud Logging, a CLI restart, whose lines wait and are shipped
// when it is back. A partial line is shipped joined, and only once ended.
func TestAgentShipsOnceAcrossRestartsAndOutages(t *testing.T) {
	root, file, c := setup(t)
	now := t0.Add(time.Hour)
	s := &sink{}
	appendFile(t, file, criLine(t0, "stdout", "F", "one")+criLine(t0.Add(time.Second), "stderr", "F", "two"))
	a := newAgent(root, c, s, &now)
	a.Round(context.Background())
	if got := s.texts(); !equal(got, []string{"one", "two"}) {
		t.Fatalf("first round shipped %q", got)
	}
	e := s.entries[0]
	if e.GetLogName() != "projects/p/logs/stdout" || e.GetResource().GetType() != "k8s_container" ||
		e.GetResource().GetLabels()["pod_name"] != "web-1" || e.GetResource().GetLabels()["container_name"] != "app" ||
		e.GetLabels()["k8s-pod/app"] != "web" || e.GetLabels()["compute.googleapis.com/resource_name"] != "n1" ||
		e.GetSeverity() != ltype.LogSeverity_INFO || s.entries[1].GetSeverity() != ltype.LogSeverity_ERROR {
		t.Errorf("entries = %v", s.entries)
	}

	// Cloud Logging goes away: nothing is lost while it is.
	s.down = true
	appendFile(t, file, criLine(t0.Add(2*time.Second), "stdout", "P", "thr")+criLine(t0.Add(2*time.Second), "stdout", "F", "ee"))
	a.Round(context.Background())
	appendFile(t, file, criLine(t0.Add(3*time.Second), "stdout", "F", "four"))
	a.Round(context.Background())
	// The agent restarts too, from its position file.
	a = newAgent(root, c, s, &now)
	a.Round(context.Background())
	s.down = false
	a.Round(context.Background())
	a.Round(context.Background())
	if got := s.texts(); !equal(got, []string{"one", "two", "three", "four"}) {
		t.Fatalf("after the outage and a restart: %q", got)
	}
	// A record still being written waits for its end.
	appendFile(t, file, t0.Add(4*time.Second).Format(time.RFC3339Nano)+" stdout F fi")
	a.Round(context.Background())
	appendFile(t, file, "ve\n")
	a.Round(context.Background())
	if got := s.texts(); !equal(got, []string{"one", "two", "three", "four", "five"}) {
		t.Fatalf("an unended record: %q", got)
	}
}

// TestAgentShipsByTheConfiguration (#2100): a category off ships nothing
// written after it was turned off, then or once it is back; one turned on
// ships from when it was turned on, so a line written just after the
// change is not lost to the agent's next round; a line written after the
// configuration was read waits for the next round's.
func TestAgentShipsByTheConfiguration(t *testing.T) {
	root, file, c := setup(t)
	now := t0.Add(10 * time.Second)
	s := &sink{}
	a := newAgent(root, c, s, &now)
	appendFile(t, file, criLine(t0.Add(time.Second), "stdout", "F", "before-off"))
	c.cfg.Categories[Workload] = Window{On: false, Since: t0.Add(2 * time.Second)}
	appendFile(t, file, criLine(t0.Add(3*time.Second), "stdout", "F", "while-off"))
	a.Round(context.Background())
	if got := s.texts(); !equal(got, []string{"before-off"}) {
		t.Fatalf("with workloads off since 2s: %q", got)
	}
	c.cfg.Categories[Workload] = Window{On: true, Since: t0.Add(20 * time.Second)}
	appendFile(t, file, criLine(t0.Add(20500*time.Millisecond), "stdout", "F", "just-after-on"))
	appendFile(t, file, criLine(t0.Add(40*time.Second), "stdout", "F", "later"))
	now = t0.Add(30 * time.Second)
	a.Round(context.Background())
	if got := s.texts(); !equal(got, []string{"before-off", "just-after-on"}) {
		t.Fatalf("on again since 20s, read at 30s: %q", got)
	}
	now = t0.Add(50 * time.Second)
	a.Round(context.Background())
	if got := s.texts(); !equal(got, []string{"before-off", "just-after-on", "later"}) {
		t.Fatalf("the next round: %q", got)
	}
}

// TestAgentClassifies (#2100): the control plane's components are shipped
// on k8s_control_plane_component in their own logs, etcd never, and system
// namespaces' containers as system logs.
func TestAgentClassifies(t *testing.T) {
	cfg := Config{Project: "p", Location: "l", Cluster: "c"}
	s, ok := cfg.Classify(Pod{Namespace: "kube-system", Name: "kube-apiserver-x", Labels: map[string]string{"tier": "control-plane", "component": "kube-apiserver"}}, "kube-apiserver", "x")
	if !ok || s.Category != "apiserver" || s.Log != "container.googleapis.com/apiserver" || s.Resource.GetType() != "k8s_control_plane_component" {
		t.Errorf("the API server: %+v", s)
	}
	if _, ok := cfg.Classify(Pod{Namespace: "kube-system", Name: "etcd-x", Labels: map[string]string{"tier": "control-plane", "component": "etcd"}}, "etcd", "x"); ok {
		t.Error("etcd is shipped")
	}
	if s, _ := cfg.Classify(Pod{Namespace: "kube-system", Name: "coredns"}, "coredns", "x"); s.Category != System {
		t.Errorf("coredns: %q", s.Category)
	}
	e := Entry("p", "container.googleapis.com/apiserver", nil, nil, t0, `{"severity":"WARNING","message":"m"}`)
	if e.GetLogName() != "projects/p/logs/container.googleapis.com%2Fapiserver" || e.GetSeverity() != ltype.LogSeverity_WARNING ||
		e.GetJsonPayload().GetFields()["message"].GetStringValue() != "m" {
		t.Errorf("entry = %v", e)
	}
}

// TestConfigNext (#2100): a category turned on or off takes the change's
// time, or now without one, and keeps it until it changes again; one on
// from the cluster's start ships everything.
func TestConfigNext(t *testing.T) {
	c := Config{}.Next(map[string]bool{System: true, Workload: true, "apiserver": false}, time.Time{}, t0)
	if !c.Ships(Workload, time.Unix(1, 0)) || c.Ships("apiserver", t0) {
		t.Fatalf("a new cluster: %+v", c.Categories)
	}
	off := c.Next(map[string]bool{System: false, Workload: false}, t0.Add(time.Minute), t0.Add(2*time.Minute))
	if w := off.Categories[Workload]; w.On || !w.Since.Equal(t0.Add(time.Minute)) {
		t.Fatalf("turned off: %+v", w)
	}
	if off.Any() {
		t.Error("Any with every category off")
	}
	same := off.Next(map[string]bool{System: false, Workload: false}, t0.Add(time.Minute), t0.Add(3*time.Minute))
	if same.Marshal() != off.Marshal() {
		t.Errorf("unchanged: %s, want %s", same.Marshal(), off.Marshal())
	}
	// Back on, by a change this process did not see (zero, or no later than
	// the last): from now.
	on := off.Next(map[string]bool{System: true, Workload: true}, t0.Add(time.Minute), t0.Add(4*time.Minute))
	if w := on.Categories[Workload]; !w.On || !w.Since.Equal(t0.Add(4*time.Minute)) {
		t.Fatalf("back on: %+v", w)
	}
	back, err := ParseConfig(on.Marshal())
	if err != nil || back.Marshal() != on.Marshal() {
		t.Errorf("round trip: %v %v", back, err)
	}
}

// TestAgentNewFileNeverTakesAReusedInodesPosition (#2100): a position left
// by a file deleted while the agent was stopped is not taken by another
// container's file that reuses its inode.
func TestAgentNewFileNeverTakesAReusedInodesPosition(t *testing.T) {
	root, file, c := setup(t)
	now := t0.Add(time.Hour)
	s := &sink{}
	appendFile(t, file, criLine(t0, "stdout", "F", "first"))
	id, _ := fileID(file)
	other := filepath.Join(root, "default_web-2_u2", "app", "0.log")
	if err := os.MkdirAll(filepath.Dir(other), 0o700); err != nil {
		t.Fatal(err)
	}
	appendFile(t, other, criLine(t0, "stdout", "F", "second"))
	a := newAgent(root, c, s, &now)
	// A stale position under the other file's inode but the first's directory.
	_, oid := filepath.Split(func() string { o, _ := fileID(other); return o }())
	a.pos = map[string]int64{filepath.Dir(file) + "#" + oid[strings.LastIndex(oid, "#")+1:]: 1000}
	a.Round(context.Background())
	got := s.texts()
	if len(got) != 2 {
		t.Fatalf("shipped %q, want both lines (positions %v, first file %s)", got, a.pos, id)
	}
}
