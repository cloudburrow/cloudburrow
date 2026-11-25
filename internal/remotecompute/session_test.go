package remotecompute

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeTransport is a connection the test drops and refuses on cue. It
// records each master's initial forwards and every control command, so a
// test sees what a reconnect replayed.
type fakeTransport struct {
	mu        sync.Mutex
	masters   [][]string
	controls  []string
	current   chan error
	refuse    int // the next refuse Master calls fail
	allocated int
	installed bool
}

func (f *fakeTransport) Authorize(context.Context) (bool, error) { return f.installed, nil }

func (f *fakeTransport) Master(_ context.Context, initial []string) (<-chan error, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.refuse > 0 {
		f.refuse--
		return nil, errors.New("connection refused")
	}
	f.masters = append(f.masters, append([]string(nil), initial...))
	f.current = make(chan error, 1)
	return f.current, nil
}

func (f *fakeTransport) Control(_ context.Context, op string, args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.controls = append(f.controls, op+" "+strings.Join(args, " "))
	switch op {
	case "exit":
		if f.current != nil {
			f.current <- nil
			close(f.current)
			f.current = nil
		}
	case "forward":
		if len(args) == 2 && args[0] == "-R" && strings.Contains(args[1], ":0:") {
			f.allocated++
			return fmt.Sprintf("%d\n", 40000+f.allocated), nil
		}
	}
	return "", nil
}

func (f *fakeTransport) Run(context.Context, string) (string, error) { return "", nil }

// drop ends the current connection as a network failure would.
func (f *fakeTransport) drop() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.current != nil {
		f.current <- errors.New("broken pipe")
		close(f.current)
		f.current = nil
	}
}

func (f *fakeTransport) snapshot() (masters [][]string, controls []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]string(nil), f.masters...), append([]string(nil), f.controls...)
}

func connectFake(t *testing.T, f *fakeTransport) (*Session, *[]Event) {
	t.Helper()
	var mu sync.Mutex
	events := &[]Event{}
	s, err := Connect(context.Background(), Options{
		Target: Target{User: "dev", Host: "box.example", Port: 2222}, StateDir: t.TempDir(), Instance: "t",
		SkipPrerequisites: true, transport: f, backoff: func(int) time.Duration { return time.Millisecond },
		Progress: func(e Event) { mu.Lock(); *events = append(*events, e); mu.Unlock() },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, events
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestConnectForwardsTheDockerSocket(t *testing.T) {
	f := &fakeTransport{}
	s, events := connectFake(t, f)
	masters, _ := f.snapshot()
	if len(masters) != 1 || masters[0][0] != "-L" || !strings.HasSuffix(masters[0][1], "docker.sock:/var/run/docker.sock") {
		t.Fatalf("the first connection carried %v, want the Docker socket forward", masters)
	}
	if !strings.HasPrefix(s.DockerHost(), "unix://") || !strings.HasSuffix(s.DockerHost(), "/docker.sock") {
		t.Fatalf("DockerHost = %q", s.DockerHost())
	}
	// sun_path is 104 bytes on macOS.
	if n := len(strings.TrimPrefix(s.DockerHost(), "unix://")); n > 100 {
		t.Fatalf("the Docker socket path is %d bytes, too long for a Unix socket", n)
	}
	var stages []Stage
	for _, e := range *events {
		stages = append(stages, e.Stage)
	}
	if got := fmt.Sprint(stages); got != "[key connecting connected]" {
		t.Fatalf("progress stages = %s", got)
	}
	if st := s.Status(); !st.Connected || st.Target != "ssh://dev@box.example:2222" || st.Reconnects != 0 {
		t.Fatalf("status = %+v", st)
	}
}

func TestSupervisorReconnectsAndReplaysEveryForward(t *testing.T) {
	f := &fakeTransport{}
	s, events := connectFake(t, f)
	ctx := context.Background()
	if _, err := s.Forward(ctx, "127.0.0.1:6443", "127.0.0.1:6443"); err != nil {
		t.Fatal(err)
	}
	bound, _, err := s.Reverse(ctx, "172.18.0.1:0", "127.0.0.1:9001")
	if err != nil {
		t.Fatal(err)
	}
	if bound != "172.18.0.1:40001" {
		t.Fatalf("Reverse bound %q, want the port ssh allocated", bound)
	}
	// Two failed attempts, then the host answers again.
	f.mu.Lock()
	f.refuse = 2
	f.mu.Unlock()
	f.drop()
	waitFor(t, "the reconnect", func() bool { st := s.Status(); return st.Connected && st.Reconnects == 1 })

	masters, controls := f.snapshot()
	if len(masters) != 2 {
		t.Fatalf("%d connections, want 2", len(masters))
	}
	replayed := strings.Join(controls, "\n")
	// The local forward twice; the reverse one first at port 0, then, on
	// the new connection, at the port the server chose, which the
	// containers already hold.
	for want, n := range map[string]int{"forward -L 127.0.0.1:6443:127.0.0.1:6443": 2,
		"forward -R 172.18.0.1:0:127.0.0.1:9001": 1, "forward -R 172.18.0.1:40001:127.0.0.1:9001": 1} {
		if strings.Count(replayed, want) != n {
			t.Errorf("%q was sent %d times, want %d:\n%s", want, strings.Count(replayed, want), n, replayed)
		}
	}
	var stages []string
	for _, e := range *events {
		stages = append(stages, string(e.Stage))
	}
	got := strings.Join(stages, " ")
	if !strings.Contains(got, "disconnected reconnecting reconnecting reconnecting connected") {
		t.Fatalf("stages = %s, want a disconnect, three attempts and a reconnect", got)
	}
}

func TestCancelledForwardIsNotReplayed(t *testing.T) {
	f := &fakeTransport{}
	s, _ := connectFake(t, f)
	cancel, err := s.Forward(context.Background(), "127.0.0.1:8080", "127.0.0.1:8080")
	if err != nil {
		t.Fatal(err)
	}
	if err := cancel(); err != nil {
		t.Fatal(err)
	}
	f.drop()
	waitFor(t, "the reconnect", func() bool { return s.Status().Reconnects == 1 && s.Status().Connected })
	_, controls := f.snapshot()
	n := 0
	for _, c := range controls {
		if strings.HasPrefix(c, "forward -L 127.0.0.1:8080") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("the cancelled forward was sent %d times, want once: %v", n, controls)
	}
}

func TestReconnectOnRequest(t *testing.T) {
	f := &fakeTransport{}
	s, _ := connectFake(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Reconnect(ctx); err != nil {
		t.Fatal(err)
	}
	if st := s.Status(); !st.Connected || st.Reconnects != 1 {
		t.Fatalf("after Reconnect: %+v", st)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Reconnect(ctx); !errors.Is(err, ErrClosed) {
		t.Fatalf("Reconnect after Close = %v, want ErrClosed", err)
	}
	if _, err := s.Forward(ctx, "127.0.0.1:1", "127.0.0.1:1"); !errors.Is(err, ErrClosed) {
		t.Fatalf("Forward after Close = %v, want ErrClosed", err)
	}
}

func TestCloseStopsReconnecting(t *testing.T) {
	f := &fakeTransport{}
	s, _ := connectFake(t, f)
	f.mu.Lock()
	f.refuse = 1 << 30
	f.mu.Unlock()
	f.drop()
	waitFor(t, "the disconnect", func() bool { return !s.Status().Connected })
	done := make(chan error)
	go func() { done <- s.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not stop the reconnect loop")
	}
	if _, err := os.Stat(filepath.Dir(strings.TrimPrefix(s.DockerHost(), "unix://"))); !os.IsNotExist(err) {
		t.Fatalf("the socket directory outlived Close: %v", err)
	}
}

func TestEnsureKeyIsOwnerOnlyAndStable(t *testing.T) {
	dir := t.TempDir()
	tg := Target{User: "dev", Host: "box"}
	k1, err := EnsureKey(dir, tg, "cloudburrow-t")
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(k1.Path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("key mode %v, want 0600", info.Mode().Perm())
	}
	if !strings.HasPrefix(k1.Public, "ssh-ed25519 ") || !strings.HasSuffix(k1.Public, " cloudburrow-t") {
		t.Fatalf("public line %q", k1.Public)
	}
	if !strings.HasPrefix(k1.Path, filepath.Join(dir, "remote")) {
		t.Fatalf("key at %s, not under the instance's state", k1.Path)
	}
	k2, err := EnsureKey(dir, tg, "cloudburrow-t")
	if err != nil || k2.Public != k1.Public {
		t.Fatalf("second EnsureKey made another key (%v)", err)
	}
	other, err := EnsureKey(dir, Target{User: "dev", Host: "other"}, "cloudburrow-t")
	if err != nil || other.Public == k1.Public {
		t.Fatalf("another host shares the key (%v)", err)
	}
	if err := os.Chmod(k1.Path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureKey(dir, tg, "cloudburrow-t"); err == nil || !strings.Contains(err.Error(), "readable by others") {
		t.Fatalf("a world-readable key was accepted: %v", err)
	}
}

func TestParseTarget(t *testing.T) {
	for in, want := range map[string]Target{
		"ssh://box":                  {Host: "box"},
		"ssh://dev@box.example":      {User: "dev", Host: "box.example"},
		"ssh://dev@10.0.0.5:2222":    {User: "dev", Host: "10.0.0.5", Port: 2222},
		"ssh://dev@[2001:db8::1]:22": {User: "dev", Host: "2001:db8::1", Port: 22},
	} {
		got, err := ParseTarget(in)
		if err != nil || got != want {
			t.Errorf("ParseTarget(%q) = %+v, %v; want %+v", in, got, err, want)
		}
		if got.String() != in {
			t.Errorf("%+v.String() = %q, want %q", got, got.String(), in)
		}
	}
	for _, bad := range []string{"", "box", "tcp://box:2375", "ssh://", "ssh://dev:pw@box", "ssh://box/path",
		"ssh://-oProxyCommand=x", "ssh://dev@box:0", "ssh://dev@box:70000", "ssh://a;b@box"} {
		if _, err := ParseTarget(bad); err == nil {
			t.Errorf("ParseTarget(%q) accepted", bad)
		}
	}
}

func TestPublishedPorts(t *testing.T) {
	out := "127.0.0.1:41235->6443/tcp\n" +
		"127.0.0.1:23080->31080/tcp, 0.0.0.0:8000-8002->80-82/tcp, :::8000-8002->80-82/tcp\n" +
		"10.0.0.5:9999->99/tcp, 127.0.0.1:5353->53/udp\n" +
		"\n" +
		"5432/tcp\n"
	if got := fmt.Sprint(PublishedPorts(out)); got != "[8000 8001 8002 23080 41235]" {
		t.Fatalf("PublishedPorts = %s", got)
	}
}

func TestMirrorFollowsPublishedPorts(t *testing.T) {
	f := &fakeTransport{}
	s, _ := connectFake(t, f)
	var mu sync.Mutex
	published := []int{41235}
	m := s.NewMirror()
	m.List = func(context.Context) ([]int, error) {
		mu.Lock()
		defer mu.Unlock()
		return append([]int(nil), published...), nil
	}
	ctx := context.Background()
	if err := m.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	published = []int{23080}
	mu.Unlock()
	if err := m.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(m.Forwarded()); got != "[23080]" {
		t.Fatalf("forwarded %s, want [23080]", got)
	}
	_, controls := f.snapshot()
	joined := strings.Join(controls, "\n")
	for _, want := range []string{"forward -L 127.0.0.1:41235:127.0.0.1:41235", "cancel -L 127.0.0.1:41235:127.0.0.1:41235",
		"forward -L 127.0.0.1:23080:127.0.0.1:23080"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in:\n%s", want, joined)
		}
	}
	m.Stop()
	if len(m.Forwarded()) != 0 {
		t.Fatal("Stop left forwards")
	}
}

func TestRecordRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if _, ok, err := LoadRecord(dir); ok || err != nil {
		t.Fatalf("an instance with no record: ok=%v err=%v", ok, err)
	}
	r := Record{Target: Target{User: "dev", Host: "box", Port: 2222}, Created: time.Unix(1, 0).UTC(), Host: HostInfo{CPUs: 4}}
	if err := SaveRecord(dir, r); err != nil {
		t.Fatal(err)
	}
	got, ok, err := LoadRecord(dir)
	if err != nil || !ok || got.Target != r.Target || got.Host.CPUs != 4 {
		t.Fatalf("LoadRecord = %+v %v %v", got, ok, err)
	}
	if err := RemoveRecord(dir); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := LoadRecord(dir); ok {
		t.Fatal("the record outlived RemoveRecord")
	}
}
