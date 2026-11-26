package ssmtunnel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/xtaci/smux"
)

// ClientVersion is the plugin version this client speaks as: the
// session-manager-plugin release whose protocol it implements.
const ClientVersion = "1.2.707.0"

// Session is a StartSession answer.
type Session struct {
	SessionID  string
	StreamURL  string
	TokenValue string
}

// Starter starts and ends the instance's port-forwarding sessions: the
// AWS SDK's ssm StartSession (document AWS-StartPortForwardingSession) and
// TerminateSession, and the SigV4 header a stream URL that is not presigned
// needs (service ssmmessages).
type Starter interface {
	Start(ctx context.Context) (Session, error)
	Terminate(ctx context.Context, sessionID string) error
	Header(ctx context.Context, streamURL string) (http.Header, error)
}

// Tunnel listens on 127.0.0.1 and carries each connection to the
// instance's port through Session Manager.
type Tunnel struct {
	ln      net.Listener
	starter Starter
	logf    func(string, ...any)
	dialer  *websocket.Dialer

	mu      sync.Mutex
	cur     *channel
	closed  bool
	opened  int
	lastErr error
	wg      sync.WaitGroup
}

// Options configure Listen.
type Options struct {
	// Logf reports sessions opening and failing; nil drops them.
	Logf func(format string, args ...any)
	// Dialer opens the WebSocket; nil is websocket.DefaultDialer.
	Dialer *websocket.Dialer
}

// channel is one open session.
type channel struct {
	id    string
	e     *endpoint
	mux   *smux.Session // nil for an agent that does not multiplex
	agent string
	basic bool
}

// Listen starts the listener. Sessions open on the first connection and
// again whenever the last one has ended.
func Listen(starter Starter, o Options) (*Tunnel, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	t := &Tunnel{ln: ln, starter: starter, logf: o.Logf, dialer: o.Dialer}
	if t.logf == nil {
		t.logf = func(string, ...any) {}
	}
	go t.accept()
	return t, nil
}

// Addr is the listener's host:port.
func (t *Tunnel) Addr() string { return t.ln.Addr().String() }

// Port is the listener's port.
func (t *Tunnel) Port() int { return t.ln.Addr().(*net.TCPAddr).Port }

// Sessions is how many sessions the tunnel has opened.
func (t *Tunnel) Sessions() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.opened
}

// LastError is the last session's failure.
func (t *Tunnel) LastError() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.lastErr
}

// Close stops listening and ends the session.
func (t *Tunnel) Close() error {
	t.mu.Lock()
	t.closed = true
	cur := t.cur
	t.cur = nil
	t.mu.Unlock()
	err := t.ln.Close()
	if cur != nil {
		t.end(cur)
	}
	return err
}

func (t *Tunnel) end(c *channel) {
	if c.e.Err() == nil {
		_ = c.e.Send(PayloadFlag, FlagPayload(FlagTerminateSession))
	}
	if c.mux != nil {
		_ = c.mux.Close()
	}
	c.e.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = t.starter.Terminate(ctx, c.id)
}

func (t *Tunnel) accept() {
	for {
		conn, err := t.ln.Accept()
		if err != nil {
			return
		}
		go t.serve(conn)
	}
}

func (t *Tunnel) serve(conn net.Conn) {
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	stream, err := t.stream(ctx)
	cancel()
	if err != nil {
		t.mu.Lock()
		t.lastErr = err
		t.mu.Unlock()
		t.logf("ssm: %v", err)
		return
	}
	defer stream.Close()
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(stream, conn); done <- struct{}{} }()
	go func() { _, _ = io.Copy(conn, stream); done <- struct{}{} }()
	<-done
}

// stream is a new connection to the port: a smux stream on the current
// session, opening the session first if there is none; or, for an agent
// that does not multiplex, a session of its own.
func (t *Tunnel) stream(ctx context.Context) (io.ReadWriteCloser, error) {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil, net.ErrClosed
	}
	cur := t.cur
	if cur != nil && (cur.e.Err() != nil || cur.basic || cur.mux.IsClosed()) {
		if cur.basic {
			cur = nil
		} else {
			t.cur, cur = nil, nil
		}
	}
	if cur == nil {
		c, err := t.open(ctx)
		if err != nil {
			t.mu.Unlock()
			return nil, err
		}
		cur = c
		if !c.basic {
			t.cur = c
		}
	}
	t.mu.Unlock()
	if cur.basic {
		return &basicStream{t: t, c: cur, p: &pipe{e: cur.e, onFlag: t.flag}}, nil
	}
	s, err := cur.mux.OpenStream()
	if err != nil {
		return nil, fmt.Errorf("open a connection on the Session Manager session %s: %w", cur.id, err)
	}
	return s, nil
}

type basicStream struct {
	t *Tunnel
	c *channel
	p *pipe
}

func (b *basicStream) Read(p []byte) (int, error)  { return b.p.Read(p) }
func (b *basicStream) Write(p []byte) (int, error) { return b.p.Write(p) }
func (b *basicStream) Close() error {
	_ = b.c.e.Send(PayloadFlag, FlagPayload(FlagDisconnectToPort))
	b.t.end(b.c)
	return nil
}

func (t *Tunnel) flag(f Flag) {
	if f == FlagConnectToPortError {
		t.mu.Lock()
		t.lastErr = errors.New("the SSM agent on the VM could not connect to its SSH port (see the agent's log on the VM)")
		t.mu.Unlock()
	}
}

// open starts a session and runs its handshake. t.mu is held.
func (t *Tunnel) open(ctx context.Context) (*channel, error) {
	s, err := t.starter.Start(ctx)
	if err != nil {
		return nil, err
	}
	c, err := openChannel(ctx, s, t.starter, t.dialer)
	if err != nil {
		tctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		_ = t.starter.Terminate(tctx, s.SessionID)
		cancel()
		return nil, err
	}
	t.opened++
	t.logf("ssm: session %s open (agent %s)", s.SessionID, c.agent)
	return c, nil
}

// openChannel opens a started session's data channel: the WebSocket, the token,
// and the handshake, then smux when the agent multiplexes.
func openChannel(ctx context.Context, s Session, starter Starter, d *websocket.Dialer) (*channel, error) {
	if s.StreamURL == "" || s.TokenValue == "" {
		return nil, errors.New("StartSession returned no stream URL or token")
	}
	u, err := url.Parse(s.StreamURL)
	if err != nil || (u.Scheme != "wss" && u.Scheme != "ws") {
		return nil, fmt.Errorf("StartSession returned a stream URL that is not a WebSocket URL: %q", s.StreamURL)
	}
	var header http.Header
	if u.Query().Get("X-Amz-Signature") == "" && starter != nil {
		if header, err = starter.Header(ctx, s.StreamURL); err != nil {
			return nil, fmt.Errorf("sign the Session Manager stream URL: %w", err)
		}
	}
	if d == nil {
		d = websocket.DefaultDialer
	}
	ws, resp, err := d.DialContext(ctx, s.StreamURL, header)
	if err != nil {
		if resp != nil {
			return nil, fmt.Errorf("open the Session Manager data channel: %s: %w", resp.Status, err)
		}
		return nil, fmt.Errorf("open the Session Manager data channel: %w", err)
	}
	open, _ := json.Marshal(OpenDataChannel{MessageSchemaVersion: "1.0", RequestID: UUIDString(NewID()), TokenValue: s.TokenValue,
		ClientID: UUIDString(NewID()), ClientVersion: ClientVersion})
	if err := ws.WriteMessage(websocket.TextMessage, open); err != nil {
		_ = ws.Close()
		return nil, fmt.Errorf("send the session token: %w", err)
	}
	e := newEndpoint(ws, true)
	e.start()
	c, err := clientHandshake(e)
	if err != nil {
		e.Close()
		return nil, err
	}
	c.id = s.SessionID
	return c, nil
}

func clientHandshake(e *endpoint) (*channel, error) {
	c := &channel{e: e}
	deadline := 30 * time.Second
	for {
		m, err := e.next(deadline)
		if err != nil {
			return nil, fmt.Errorf("the Session Manager handshake: %w", err)
		}
		switch m.PayloadType {
		case PayloadHandshakeRequest:
			var req HandshakeRequest
			if err := json.Unmarshal(m.Payload, &req); err != nil {
				return nil, fmt.Errorf("the agent's handshake request: %w", err)
			}
			c.agent = req.AgentVersion
			resp := HandshakeResponse{ClientVersion: ClientVersion, ProcessedClientActions: []ProcessedAction{}, Errors: []string{}}
			for _, a := range req.RequestedClientActions {
				pa := ProcessedAction{ActionType: a.ActionType}
				switch a.ActionType {
				case "SessionType":
					var p SessionTypeParameters
					if err := json.Unmarshal(a.ActionParameters, &p); err != nil || p.SessionType != "Port" {
						pa.ActionStatus, pa.Error = ActionFailed, fmt.Sprintf("CloudBurrow opens only Port sessions, not %q", p.SessionType)
					} else {
						pa.ActionStatus = ActionSuccess
						if typ, _ := p.Properties["type"].(string); typ != "" && typ != "LocalPortForwarding" {
							pa.ActionStatus, pa.Error = ActionFailed, "CloudBurrow forwards a local port, not "+typ
						}
					}
				case "KMSEncryption":
					pa.ActionStatus = ActionFailed
					pa.Error = "the account's Session Manager preferences ask for KMS encryption of session data, which CloudBurrow does not do yet"
				default:
					pa.ActionStatus, pa.Error = ActionUnsupported, "Unsupported action "+a.ActionType
				}
				if pa.Error != "" {
					resp.Errors = append(resp.Errors, pa.Error)
				}
				resp.ProcessedClientActions = append(resp.ProcessedClientActions, pa)
			}
			b, _ := json.Marshal(resp)
			if err := e.Send(PayloadHandshakeResponse, b); err != nil {
				return nil, err
			}
			if len(resp.Errors) > 0 {
				return nil, errors.New("the Session Manager handshake: " + strings.Join(resp.Errors, "; "))
			}
		case PayloadHandshakeComplete:
			if VersionAfter(c.agent, MuxAfterAgentVersion) {
				cfg := smux.DefaultConfig()
				if VersionAfter(c.agent, KeepAliveOffAfterAgentVersion) {
					cfg.KeepAliveDisabled = true
				}
				mux, err := smux.Client(&pipe{e: e}, cfg)
				if err != nil {
					return nil, err
				}
				c.mux = mux
			} else {
				c.basic = true
			}
			return c, nil
		case PayloadOutput, PayloadFlag:
			return nil, errors.New("the Session Manager agent sent data before completing the handshake")
		}
	}
}
