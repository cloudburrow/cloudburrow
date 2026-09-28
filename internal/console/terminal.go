package console

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/websocket"
)

// Terminal is the Cloud Shell-style terminal behind the top bar's drawer
// (#781): a shell in a pod in the instance's cluster, never on this machine.
//
// Declared here, narrowly, so the console knows nothing of pods: the wiring
// hands it an implementation (internal/terminal) that does.
type Terminal interface {
	// Prepare makes the shell's pod ready, reporting what it is waiting for
	// through progress. It decides for itself how long to wait; ctx ends
	// when the browser goes away. An error is shown in the drawer as it
	// stands, with Try again.
	Prepare(ctx context.Context, progress func(string)) error
	// Open starts a shell scoped to project (none when it is empty) on a
	// terminal of cols by rows.
	Open(project string, cols, rows uint16) (TerminalSession, error)
}

// TerminalSession is one open shell.
type TerminalSession interface {
	// Read returns what the shell wrote to its terminal; an error, io.EOF
	// included, means the shell has ended.
	io.ReadWriteCloser
	Resize(cols, rows uint16) error
	// SetProject makes the shell follow a newly selected project.
	SetProject(ctx context.Context, project string) error
}

// SetTerminal installs the terminal. Nil, the default, leaves the drawer
// saying the instance has none.
func (s *Server) SetTerminal(t Terminal) {
	s.termMu.Lock()
	defer s.termMu.Unlock()
	s.terminal = t
}

const (
	// terminalIdle is how long a shell nobody is attached to is kept, so a
	// closed drawer or a reload returns to the same shell.
	terminalIdle = 15 * time.Minute
	// terminalBacklog is how much recent output a reattach replays.
	terminalBacklog = 256 << 10
	// terminalMaxSessions bounds the shells one console keeps: the drawer's
	// tabs (#834), across every browser tab showing this console.
	terminalMaxSessions = 8
	// terminalMaxFrame bounds one message from the browser.
	terminalMaxFrame = 64 << 10
)

// noTerminal is the drawer's reason when no terminal is installed.
const noTerminal = "this instance has no cluster terminal: the console was started without a cluster to run it in"

// terminalMessage is a control message, sent as a WebSocket text frame.
// Terminal bytes go as binary frames in both directions.
type terminalMessage struct {
	// Type is one of:
	//   from the browser: "resize" (Cols, Rows), "project" (Project),
	//   "close" (end this shell now, when its drawer tab is closed);
	//   from the server: "status" (Message), "session" (ID, Project,
	//   Resumed), "project" (Project, Message), "unavailable" (Message),
	//   "exit" (Message), "detached" (Message).
	Type    string `json:"type"`
	Message string `json:"message,omitempty"`
	ID      string `json:"id,omitempty"`
	Project string `json:"project,omitempty"`
	Resumed bool   `json:"resumed,omitempty"`
	Cols    uint16 `json:"cols,omitempty"`
	Rows    uint16 `json:"rows,omitempty"`
}

// handleTerminalStatus answers whether the drawer can open a terminal, so it
// can say why not before trying.
func (s *Server) handleTerminalStatus(w http.ResponseWriter, r *http.Request) {
	s.termMu.Lock()
	t := s.terminal
	s.termMu.Unlock()
	if t == nil {
		writeJSON(w, http.StatusOK, map[string]any{"available": false, "reason": noTerminal})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"available": true, "maxSessions": terminalMaxSessions})
}

// terminalRefusal is why an upgrade is refused, or "" when it may proceed.
//
// The terminal is a shell, so the bar is higher than the rest of the API's.
// Host has already passed hostguard, and sameOriginOnly has refused a
// cross-site Sec-Fetch-Site, by the time this runs. Beyond those, a browser
// always sends Origin on a WebSocket upgrade, so one without it is refused
// rather than taken for curl; it must name exactly this console; and the
// peer must be on this machine, even when --allow-remote has bound the
// console somewhere a LAN can reach.
func terminalRefusal(r *http.Request) string {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return "a terminal upgrade must carry Origin"
	}
	if !strings.EqualFold(origin, "http://"+r.Host) {
		return "a terminal upgrade from origin " + strconv.Quote(origin) + " is refused: only this console's own page may open it"
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return "the terminal is served only to this machine"
	}
	return ""
}

func (s *Server) handleTerminalSocket(w http.ResponseWriter, r *http.Request) {
	if why := terminalRefusal(r); why != "" {
		http.Error(w, why, http.StatusForbidden)
		return
	}
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		http.Error(w, "this endpoint is a WebSocket", http.StatusBadRequest)
		return
	}
	websocket.Server{
		// The Origin check above is the handshake's; the library's own
		// would accept any well-formed origin.
		Handshake: func(*websocket.Config, *http.Request) error { return nil },
		Handler: func(ws *websocket.Conn) {
			ws.MaxPayloadBytes = terminalMaxFrame
			s.bridgeTerminal(ws, r)
		},
	}.ServeHTTP(w, r)
}

// frameCodec reads a frame and whether it was text, which is what separates
// a control message from typed bytes.
var frameCodec = websocket.Codec{
	Marshal: func(v any) ([]byte, byte, error) {
		switch b := v.(type) {
		case []byte:
			return b, websocket.BinaryFrame, nil
		default:
			out, err := json.Marshal(v)
			return out, websocket.TextFrame, err
		}
	},
	Unmarshal: func(data []byte, payloadType byte, v any) error {
		f := v.(*frame)
		f.text = payloadType == websocket.TextFrame
		f.data = append([]byte(nil), data...)
		return nil
	},
}

type frame struct {
	text bool
	data []byte
}

func sendControl(ws *websocket.Conn, m terminalMessage) error { return frameCodec.Send(ws, m) }

// terminalCapMessage is what a drawer tab is told when the console already
// holds as many shells as it keeps.
var terminalCapMessage = "this console already has " + strconv.Itoa(terminalMaxSessions) +
	" terminal tabs open, the most it keeps; close one to open another"

// bridgeTerminal is one browser connection: it attaches to the session the
// browser names, if it is still running, or opens a new one.
//
// Each drawer tab (#834) is its own connection and its own session. What a
// session id lets a page do is what it did when the drawer had one session
// (#781): the id is 128 random bits, sent only on the connection that opened
// the shell and on later attaches by that id, and listed by no endpoint, so
// a page attaches to a shell only if it was given the id, and every
// connection, new or attaching, passes terminalRefusal first. An id that
// names no running shell opens a new one; it never attaches to another.
func (s *Server) bridgeTerminal(ws *websocket.Conn, r *http.Request) {
	defer ws.Close()
	q := r.URL.Query()
	project := q.Get("project")
	cols, rows := dimension(q.Get("cols"), 80), dimension(q.Get("rows"), 24)

	// Frames are read from the start, so a browser that goes away while the
	// pod is still starting cancels the wait.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	frames := make(chan frame, 64)
	go func() {
		defer cancel()
		defer close(frames)
		for {
			var f frame
			if err := frameCodec.Receive(ws, &f); err != nil {
				return
			}
			select {
			case frames <- f:
			case <-ctx.Done():
				return
			}
		}
	}()

	sess := s.sessions().find(q.Get("session"))
	if sess == nil {
		s.termMu.Lock()
		t := s.terminal
		s.termMu.Unlock()
		if t == nil {
			_ = sendControl(ws, terminalMessage{Type: "unavailable", Message: noTerminal})
			return
		}
		// The slot is held while the pod is prepared, so tabs opened at once
		// cannot together pass the cap.
		if !s.sessions().reserve() {
			_ = sendControl(ws, terminalMessage{Type: "unavailable", Message: terminalCapMessage})
			return
		}
		// No budget of the console's own: the first use pulls an image of
		// about 1 GB, which took longer than the quarter of an hour this
		// once allowed (#824). Prepare waits while the pull is under way and
		// gives up on a real failure; the browser going away cancels ctx.
		err := t.Prepare(ctx, func(msg string) { _ = sendControl(ws, terminalMessage{Type: "status", Message: msg}) })
		if err != nil {
			s.sessions().release()
			_ = sendControl(ws, terminalMessage{Type: "unavailable", Message: userMessage(err)})
			return
		}
		opened, err := t.Open(project, cols, rows)
		if err != nil {
			s.sessions().release()
			_ = sendControl(ws, terminalMessage{Type: "unavailable", Message: userMessage(err)})
			return
		}
		sess = s.sessions().add(opened, project)
	}

	if !sess.attach(ws) {
		_ = sendControl(ws, terminalMessage{Type: "exit", Message: "the shell has ended"})
		return
	}
	defer sess.detach(ws)

	for {
		var f frame
		var ok bool
		select {
		case f, ok = <-frames:
		case <-sess.done:
			return
		}
		if !ok {
			return
		}
		if !f.text {
			if _, err := sess.sess.Write(f.data); err != nil {
				return
			}
			continue
		}
		var m terminalMessage
		if json.Unmarshal(f.data, &m) != nil {
			continue
		}
		switch m.Type {
		case "resize":
			if m.Cols > 0 && m.Rows > 0 {
				_ = sess.sess.Resize(m.Cols, m.Rows)
			}
		case "project":
			sess.setProject(ctx, ws, m.Project)
		case "close":
			// Closing a drawer tab ends its shell now, rather than after the
			// idle timeout; the other tabs' shells are untouched.
			sess.end("the tab was closed")
			return
		}
	}
}

// dimension parses a terminal size, falling back to def for anything
// missing or implausible.
func dimension(v string, def uint16) uint16 {
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > 1000 {
		return def
	}
	return uint16(n)
}

// terminalSessions are the shells this console holds open.
type terminalSessions struct {
	mu   sync.Mutex
	byID map[string]*terminalSession
	// pending counts shells being opened, which reserve holds a place for.
	pending int
}

func (s *Server) sessions() *terminalSessions {
	s.termMu.Lock()
	defer s.termMu.Unlock()
	if s.termSessions == nil {
		s.termSessions = &terminalSessions{byID: map[string]*terminalSession{}}
	}
	return s.termSessions
}

func (ts *terminalSessions) find(id string) *terminalSession {
	if id == "" {
		return nil
	}
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.byID[id]
}

// reserve holds a place for a shell about to be opened, and reports false
// when the console already keeps terminalMaxSessions. A reservation ends
// with add or release.
func (ts *terminalSessions) reserve() bool {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if len(ts.byID)+ts.pending >= terminalMaxSessions {
		return false
	}
	ts.pending++
	return true
}

func (ts *terminalSessions) release() {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if ts.pending > 0 {
		ts.pending--
	}
}

// add keeps a newly opened shell, in the place reserve held for it.
func (ts *terminalSessions) add(sess TerminalSession, project string) *terminalSession {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	t := &terminalSession{id: hex.EncodeToString(b), sess: sess, project: project,
		done: make(chan struct{}), owner: ts}
	ts.mu.Lock()
	ts.byID[t.id] = t
	if ts.pending > 0 {
		ts.pending--
	}
	ts.mu.Unlock()
	go t.pump()
	return t
}

func (ts *terminalSessions) remove(id string) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	delete(ts.byID, id)
}

// closeAll ends every shell, when the console stops.
func (ts *terminalSessions) closeAll() {
	ts.mu.Lock()
	all := make([]*terminalSession, 0, len(ts.byID))
	for _, t := range ts.byID {
		all = append(all, t)
	}
	ts.mu.Unlock()
	for _, t := range all {
		t.end("the console stopped")
	}
}

// terminalSession is one shell, which outlives the connections to it: a
// reload or a closed drawer detaches, and reattaching replays the recent
// output.
type terminalSession struct {
	id    string
	sess  TerminalSession
	owner *terminalSessions
	done  chan struct{}

	mu      sync.Mutex
	project string
	backlog []byte
	conn    *websocket.Conn
	idle    *time.Timer
	ended   bool
}

// pump copies the shell's output to whoever is attached, and keeps the
// recent part of it for the next attach.
func (t *terminalSession) pump() {
	buf := make([]byte, 32<<10)
	for {
		n, err := t.sess.Read(buf)
		if n > 0 {
			chunk := append([]byte(nil), buf[:n]...)
			t.mu.Lock()
			t.backlog = append(t.backlog, chunk...)
			if over := len(t.backlog) - terminalBacklog; over > 0 {
				t.backlog = append([]byte(nil), t.backlog[over:]...)
			}
			if t.conn != nil {
				_ = frameCodec.Send(t.conn, chunk)
			}
			t.mu.Unlock()
		}
		if err != nil {
			reason := "the shell has ended"
			if !errors.Is(err, io.EOF) {
				reason = "the connection to the terminal pod was lost; open the terminal again for a new shell"
			}
			t.end(reason)
			return
		}
	}
}

// end closes the shell and tells whoever is attached why.
func (t *terminalSession) end(reason string) {
	t.mu.Lock()
	if t.ended {
		t.mu.Unlock()
		return
	}
	t.ended = true
	if t.idle != nil {
		t.idle.Stop()
	}
	conn := t.conn
	t.conn = nil
	t.mu.Unlock()
	t.owner.remove(t.id)
	_ = t.sess.Close()
	if conn != nil {
		_ = sendControl(conn, terminalMessage{Type: "exit", Message: reason})
		_ = conn.Close()
	}
	close(t.done)
}

// attach makes ws the session's connection, taking it from any other tab,
// and replays the recent output. It reports false for an ended session.
func (t *terminalSession) attach(ws *websocket.Conn) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.ended {
		return false
	}
	if t.idle != nil {
		t.idle.Stop()
		t.idle = nil
	}
	resumed := len(t.backlog) > 0 || t.conn != nil
	if old := t.conn; old != nil {
		_ = sendControl(old, terminalMessage{Type: "detached", Message: "this terminal was opened in another tab"})
		_ = old.Close()
	}
	t.conn = ws
	_ = sendControl(ws, terminalMessage{Type: "session", ID: t.id, Project: t.project, Resumed: resumed})
	if len(t.backlog) > 0 {
		_ = frameCodec.Send(ws, append([]byte(nil), t.backlog...))
	}
	return true
}

// detach lets go of ws, if it is still the session's connection, and ends
// the shell if nobody reattaches within terminalIdle.
func (t *terminalSession) detach(ws *websocket.Conn) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.conn != ws || t.ended {
		return
	}
	t.conn = nil
	t.idle = time.AfterFunc(terminalIdle, func() {
		t.end("the terminal was left detached for " + terminalIdle.String())
	})
}

// setProject switches the shell's project and says so in the terminal.
func (t *terminalSession) setProject(ctx context.Context, ws *websocket.Conn, project string) {
	sctx, cancel := context.WithTimeout(ctx, readBudget)
	defer cancel()
	if err := t.sess.SetProject(sctx, project); err != nil {
		_ = sendControl(ws, terminalMessage{Type: "project", Project: t.currentProject(),
			Message: "the terminal could not switch project: " + userMessage(err)})
		return
	}
	t.mu.Lock()
	t.project = project
	t.mu.Unlock()
	shown := project
	if shown == "" {
		shown = "no project"
	}
	_ = sendControl(ws, terminalMessage{Type: "project", Project: project,
		Message: "switched to " + shown + "; CLOUDSDK_CORE_PROJECT and the prompt follow from the next prompt"})
}

func (t *terminalSession) currentProject() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.project
}
