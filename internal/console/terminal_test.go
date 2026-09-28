package console

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/websocket"
)

// fakeTerminal is a Terminal whose shell echoes what is typed, so the
// bridge can be tested without a cluster.
type fakeTerminal struct {
	prepareErr error
	mu         sync.Mutex
	opened     []*fakeShell
}

func (f *fakeTerminal) Prepare(_ context.Context, progress func(string)) error {
	progress("Creating the terminal pod")
	return f.prepareErr
}

func (f *fakeTerminal) Open(project string, cols, rows uint16) (TerminalSession, error) {
	r, w := io.Pipe()
	sh := &fakeShell{project: project, cols: cols, rows: rows, out: r, in: w}
	f.mu.Lock()
	f.opened = append(f.opened, sh)
	f.mu.Unlock()
	go func() { _, _ = io.WriteString(w, "cloudburrow ("+project+")$ ") }()
	return sh, nil
}

func (f *fakeTerminal) shells() []*fakeShell {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*fakeShell(nil), f.opened...)
}

type fakeShell struct {
	mu         sync.Mutex
	project    string
	cols, rows uint16
	projects   []string
	out        *io.PipeReader
	in         *io.PipeWriter
	closed     bool
}

func (s *fakeShell) Read(b []byte) (int, error) { return s.out.Read(b) }

// Write echoes each line back as the shell's output; "exit" ends the shell.
func (s *fakeShell) Write(b []byte) (int, error) {
	if strings.TrimSpace(string(b)) == "exit" {
		_ = s.in.Close()
		return len(b), nil
	}
	go func() { _, _ = io.WriteString(s.in, "echo:"+string(b)) }()
	return len(b), nil
}

func (s *fakeShell) Resize(cols, rows uint16) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cols, s.rows = cols, rows
	return nil
}

func (s *fakeShell) SetProject(_ context.Context, project string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.projects = append(s.projects, project)
	return nil
}

func (s *fakeShell) Close() error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	_ = s.in.Close()
	return nil
}

func (s *fakeShell) size() (uint16, uint16) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cols, s.rows
}

func serveTerminal(t *testing.T, term Terminal) *httptest.Server {
	t.Helper()
	s := New("127.0.0.1:0", nil)
	if term != nil {
		s.SetTerminal(term)
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(func() {
		srv.Close()
		_ = s.Stop(context.Background())
	})
	return srv
}

// dial opens the terminal socket as the console's own page would.
func dial(t *testing.T, srv *httptest.Server, query string) *websocket.Conn {
	t.Helper()
	cfg, err := websocket.NewConfig("ws"+strings.TrimPrefix(srv.URL, "http")+"/api/terminal/socket?"+query, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	ws, err := websocket.DialConfig(cfg)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	_ = ws.SetDeadline(time.Now().Add(10 * time.Second))
	return ws
}

// next reads frames until one satisfies want, returning the text or bytes
// read on the way.
func next(t *testing.T, ws *websocket.Conn, want func(f frame) bool) frame {
	t.Helper()
	for {
		var f frame
		if err := frameCodec.Receive(ws, &f); err != nil {
			t.Fatalf("receive: %v", err)
		}
		if want(f) {
			return f
		}
	}
}

func control(typ string) func(frame) bool {
	return func(f frame) bool {
		var m terminalMessage
		return f.text && json.Unmarshal(f.data, &m) == nil && m.Type == typ
	}
}

func output(s string) func(frame) bool {
	return func(f frame) bool { return !f.text && strings.Contains(string(f.data), s) }
}

func message(t *testing.T, f frame) terminalMessage {
	t.Helper()
	var m terminalMessage
	if err := json.Unmarshal(f.data, &m); err != nil {
		t.Fatalf("control frame %q: %v", f.data, err)
	}
	return m
}

func upgrade(t *testing.T, srv *httptest.Server, headers map[string]string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/api/terminal/socket", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	for k, v := range headers {
		if k == "Host" {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusSwitchingProtocols {
		// The body is the connection now; there is nothing to read to EOF.
		return resp.StatusCode, ""
	}
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// The terminal is a shell, so its upgrade is refused to anything but the
// console's own page: a foreign Origin, a missing one, a page on another
// port of this machine, a cross-site fetch, and a rebound Host (#676).
func TestTerminalUpgradeIsRefusedToAnyOtherOriginOrHost(t *testing.T) {
	t.Parallel()
	term := &fakeTerminal{}
	srv := serveTerminal(t, term)
	host := strings.TrimPrefix(srv.URL, "http://")
	for _, c := range []struct {
		name    string
		headers map[string]string
		want    int
	}{
		{"a foreign origin", map[string]string{"Origin": "http://evil.example"}, http.StatusForbidden},
		{"no origin", map[string]string{}, http.StatusForbidden},
		{"another port on loopback", map[string]string{"Origin": "http://127.0.0.1:1"}, http.StatusForbidden},
		{"https spelling of the origin", map[string]string{"Origin": "https://" + host}, http.StatusForbidden},
		{"a null origin", map[string]string{"Origin": "null"}, http.StatusForbidden},
		{"a cross-site fetch", map[string]string{"Origin": srv.URL, "Sec-Fetch-Site": "cross-site"}, http.StatusForbidden},
		{"a rebound host with a matching origin", map[string]string{
			"Host": "attacker.example:9090", "Origin": "http://attacker.example:9090", "Sec-Fetch-Site": "same-origin"},
			http.StatusMisdirectedRequest},
	} {
		code, body := upgrade(t, srv, c.headers)
		if code != c.want {
			t.Errorf("%s: %d %.200q, want %d", c.name, code, body, c.want)
		}
	}
	if n := len(term.shells()); n != 0 {
		t.Errorf("a refused upgrade opened %d shells", n)
	}
	// The same request from the console's own origin is upgraded.
	if code, _ := upgrade(t, srv, map[string]string{"Origin": srv.URL}); code != http.StatusSwitchingProtocols {
		t.Errorf("the console's own origin = %d, want 101", code)
	}
}

// A connection from off this machine is refused even with the right Origin
// and Host, which --allow-remote would otherwise let through.
func TestTerminalIsRefusedToARemotePeer(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:9090/api/terminal/socket", nil)
	r.Header.Set("Origin", "http://127.0.0.1:9090")
	r.RemoteAddr = "192.168.1.20:51000"
	if why := terminalRefusal(r); !strings.Contains(why, "only to this machine") {
		t.Errorf("a LAN peer: %q, want a refusal", why)
	}
	r.RemoteAddr = "127.0.0.1:51000"
	if why := terminalRefusal(r); why != "" {
		t.Errorf("a loopback peer was refused: %q", why)
	}
}

// Keystrokes reach the shell, its output reaches the browser, and the
// drawer's size and the toolbar's project reach the shell.
func TestTerminalBridgesKeystrokesOutputSizeAndProject(t *testing.T) {
	t.Parallel()
	term := &fakeTerminal{}
	srv := serveTerminal(t, term)
	ws := dial(t, srv, "project=proj-one&cols=132&rows=41")

	if m := message(t, next(t, ws, control("status"))); m.Message != "Creating the terminal pod" {
		t.Errorf("status = %q", m.Message)
	}
	sess := message(t, next(t, ws, control("session")))
	if sess.ID == "" || sess.Project != "proj-one" || sess.Resumed {
		t.Errorf("session = %+v; want a new session in proj-one", sess)
	}
	next(t, ws, output("cloudburrow (proj-one)$ "))
	shells := term.shells()
	if len(shells) != 1 {
		t.Fatalf("opened %d shells, want 1", len(shells))
	}
	if c, r := shells[0].size(); c != 132 || r != 41 {
		t.Errorf("opened at %dx%d, want 132x41", c, r)
	}

	if err := frameCodec.Send(ws, []byte("gcloud storage ls\n")); err != nil {
		t.Fatal(err)
	}
	next(t, ws, output("echo:gcloud storage ls"))

	if err := sendControl(ws, terminalMessage{Type: "resize", Cols: 90, Rows: 20}); err != nil {
		t.Fatal(err)
	}
	if err := sendControl(ws, terminalMessage{Type: "project", Project: "proj-two"}); err != nil {
		t.Fatal(err)
	}
	if m := message(t, next(t, ws, control("project"))); m.Project != "proj-two" || !strings.Contains(m.Message, "switched to proj-two") {
		t.Errorf("project reply = %+v", m)
	}
	if c, r := shells[0].size(); c != 90 || r != 20 {
		t.Errorf("resized to %dx%d, want 90x20", c, r)
	}
	shells[0].mu.Lock()
	projects := shells[0].projects
	shells[0].mu.Unlock()
	if len(projects) != 1 || projects[0] != "proj-two" {
		t.Errorf("the shell was switched to %v, want [proj-two]", projects)
	}
}

// Closing the drawer or reloading detaches rather than ends: reattaching by
// id returns to the same shell, with its recent output replayed.
func TestTerminalReattachKeepsTheSession(t *testing.T) {
	t.Parallel()
	term := &fakeTerminal{}
	srv := serveTerminal(t, term)
	ws := dial(t, srv, "project=proj-one")
	id := message(t, next(t, ws, control("session"))).ID
	if err := frameCodec.Send(ws, []byte("touch kept\n")); err != nil {
		t.Fatal(err)
	}
	next(t, ws, output("echo:touch kept"))
	_ = ws.Close()

	again := dial(t, srv, "project=proj-one&session="+id)
	m := message(t, next(t, again, control("session")))
	if m.ID != id || !m.Resumed {
		t.Errorf("reattach = %+v; want session %s resumed", m, id)
	}
	next(t, again, output("echo:touch kept"))
	if n := len(term.shells()); n != 1 {
		t.Errorf("reattaching opened %d shells, want the one", n)
	}
}

// A shell that ends says so, and its id no longer attaches.
func TestTerminalShellExitIsReported(t *testing.T) {
	t.Parallel()
	term := &fakeTerminal{}
	srv := serveTerminal(t, term)
	ws := dial(t, srv, "")
	id := message(t, next(t, ws, control("session"))).ID
	if err := frameCodec.Send(ws, []byte("exit\n")); err != nil {
		t.Fatal(err)
	}
	if m := message(t, next(t, ws, control("exit"))); m.Message != "the shell has ended" {
		t.Errorf("exit = %q", m.Message)
	}
	again := dial(t, srv, "session="+id)
	if m := message(t, next(t, again, control("session"))); m.ID == id {
		t.Error("an ended session was reattached")
	}
}

// When there is no terminal, or its pod cannot start, the drawer is told
// why instead of being left with a blank terminal.
func TestTerminalUnavailableSaysWhy(t *testing.T) {
	t.Parallel()
	none := serveTerminal(t, nil)
	if m := message(t, next(t, dial(t, none, ""), control("unavailable"))); m.Message != noTerminal {
		t.Errorf("no terminal: %q", m.Message)
	}
	code, body := get(t, none, "/api/terminal", nil)
	if code != http.StatusOK || !strings.Contains(body, `"available":false`) || !strings.Contains(body, "no cluster terminal") {
		t.Errorf("GET /api/terminal = %d %s", code, body)
	}

	broken := serveTerminal(t, &fakeTerminal{prepareErr: errors.New("the terminal pod cannot start (ImagePullBackOff)")})
	m := message(t, next(t, dial(t, broken, ""), control("unavailable")))
	if !strings.Contains(m.Message, "ImagePullBackOff") {
		t.Errorf("a pod that cannot start: %q", m.Message)
	}
	if code, body := get(t, broken, "/api/terminal", nil); !strings.Contains(body, `"available":true`) {
		t.Errorf("GET /api/terminal with a terminal = %d %s", code, body)
	}
}
