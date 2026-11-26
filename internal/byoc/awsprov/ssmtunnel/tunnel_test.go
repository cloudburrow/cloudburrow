package ssmtunnel

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestMessageRoundTripAndLayout(t *testing.T) {
	m := Message{Type: InputStreamData, SchemaVersion: 1, Sequence: 7, Flags: 1, ID: NewID(), PayloadType: PayloadOutput,
		Payload: []byte("hello"), CreatedDate: time.UnixMilli(1700000000123)}
	b := m.Marshal()
	if got := binary.BigEndian.Uint32(b); got != 116 {
		t.Fatalf("HeaderLength = %d, want 116", got)
	}
	if string(b[4:4+len(InputStreamData)]) != InputStreamData || b[4+len(InputStreamData)] != ' ' {
		t.Fatalf("MessageType is not space padded: %q", b[4:36])
	}
	// The UUID's least significant half comes first.
	if !bytes.Equal(b[64:72], m.ID[8:16]) || !bytes.Equal(b[72:80], m.ID[0:8]) {
		t.Fatal("MessageId halves are not in the plugin's order")
	}
	if binary.BigEndian.Uint32(b[116:]) != 5 || string(b[120:]) != "hello" {
		t.Fatal("payload length or payload misplaced")
	}
	got, err := Unmarshal(b)
	if err != nil {
		t.Fatal(err)
	}
	if got.Type != m.Type || got.Sequence != 7 || got.Flags != 1 || got.ID != m.ID || got.PayloadType != PayloadOutput ||
		string(got.Payload) != "hello" || !got.CreatedDate.Equal(m.CreatedDate) {
		t.Fatalf("round trip: %+v", got)
	}
	b[125-1] ^= 0xff // corrupt the payload: the digest no longer matches
	if _, err := Unmarshal(b); !errors.Is(err, ErrMalformed) {
		t.Fatalf("corrupt payload accepted: %v", err)
	}
	if _, err := Unmarshal(b[:50]); !errors.Is(err, ErrMalformed) {
		t.Fatal("short frame accepted")
	}
	if UUIDString([16]byte{0x12, 0x34, 0x56, 0x78, 0x9a, 0xbc, 0xde, 0xf0, 1, 2, 3, 4, 5, 6, 7, 8}) != "12345678-9abc-def0-0102-030405060708" {
		t.Fatal("UUIDString")
	}
}

func TestVersionAfter(t *testing.T) {
	for _, c := range []struct {
		v, base string
		want    bool
	}{{"3.3.1802.0", MuxAfterAgentVersion, true}, {"3.0.196.0", MuxAfterAgentVersion, false}, {"2.3.722.0", MuxAfterAgentVersion, false},
		{"3.1.1511.1", KeepAliveOffAfterAgentVersion, true}, {"junk", "1.0", false}} {
		if got := VersionAfter(c.v, c.base); got != c.want {
			t.Errorf("VersionAfter(%s, %s) = %v", c.v, c.base, got)
		}
	}
}

// echo is a TCP server echoing each connection.
func echo(t *testing.T) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
		}
	}()
	return ln.Addr().String()
}

// fakeService is StartSession, TerminateSession and the stream endpoint.
type fakeService struct {
	t         *testing.T
	srv       *httptest.Server
	target    string
	version   string
	drop      func(Message, int) bool
	started   atomic.Int32
	ended     atomic.Int32
	signed    atomic.Int32
	mu        sync.Mutex
	tokens    map[string]string
	refuseTok bool
}

func newFakeService(t *testing.T, target, version string) *fakeService {
	f := &fakeService{t: t, target: target, version: version, tokens: map[string]string{}}
	up := websocket.Upgrader{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/v1/data-channel/")
		f.mu.Lock()
		tok, ok := f.tokens[id]
		f.mu.Unlock()
		if !ok {
			http.Error(w, "no such session", http.StatusNotFound)
			return
		}
		if strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256") {
			f.signed.Add(1)
		}
		ws, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		if f.refuseTok {
			tok = "other"
		}
		_ = ServeAgent(ws, AgentOptions{Token: tok, Version: f.version, Port: "22", Drop: f.drop,
			Dial: func() (net.Conn, error) { return net.Dial("tcp", f.target) }})
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeService) Start(context.Context) (Session, error) {
	n := f.started.Add(1)
	id := "cloudburrow-test-" + string(rune('a'+n))
	f.mu.Lock()
	f.tokens[id] = "token-" + id
	f.mu.Unlock()
	return Session{SessionID: id, TokenValue: "token-" + id, StreamURL: "ws" + strings.TrimPrefix(f.srv.URL, "http") + "/v1/data-channel/" + id + "?role=publish_subscribe"}, nil
}

func (f *fakeService) Terminate(context.Context, string) error { f.ended.Add(1); return nil }

func (f *fakeService) Header(context.Context, string) (http.Header, error) {
	h := http.Header{}
	h.Set("Authorization", "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20261124/us-east-1/ssmmessages/aws4_request")
	return h, nil
}

func roundTrip(t *testing.T, addr string, payload []byte) {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(60 * time.Second))
	go func() { _, _ = c.Write(payload) }()
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(c, got); err != nil {
		t.Fatalf("read back %d bytes: %v", len(payload), err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("echoed bytes differ")
	}
}

func random(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

func TestTunnelMultiplexesConnectionsOverOneSession(t *testing.T) {
	f := newFakeService(t, echo(t), AgentDefaultVersion)
	tun, err := Listen(f, Options{})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); roundTrip(t, tun.Addr(), random(200<<10)) }()
	}
	wg.Wait()
	roundTrip(t, tun.Addr(), []byte("\n")) // a lone newline is data, not Enter
	if n := f.started.Load(); n != 1 {
		t.Fatalf("%d sessions started for 5 connections; a multiplexing agent needs one", n)
	}
	if f.signed.Load() != 1 {
		t.Fatal("the stream URL was opened without the SigV4 header")
	}
	if err := tun.Close(); err != nil {
		t.Fatal(err)
	}
	if f.ended.Load() != 1 {
		t.Fatal("Close did not terminate the session")
	}
}

func TestTunnelResendsLostFrames(t *testing.T) {
	f := newFakeService(t, echo(t), AgentDefaultVersion)
	var dropped atomic.Int32
	// The agent loses the first send of every third stream message.
	f.drop = func(m Message, attempt int) bool {
		if attempt == 1 && m.Sequence%3 == 1 {
			dropped.Add(1)
			return true
		}
		return false
	}
	tun, err := Listen(f, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer tun.Close()
	roundTrip(t, tun.Addr(), random(64<<10))
	if dropped.Load() == 0 {
		t.Fatal("no frame was dropped; the test proves nothing")
	}
}

func TestTunnelBasicModeForAnOldAgent(t *testing.T) {
	f := newFakeService(t, echo(t), "2.3.800.0")
	tun, err := Listen(f, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer tun.Close()
	roundTrip(t, tun.Addr(), random(10<<10))
	roundTrip(t, tun.Addr(), random(10<<10))
	if n := f.started.Load(); n != 2 {
		t.Fatalf("%d sessions for 2 connections to an agent that does not multiplex; want one each", n)
	}
}

func TestTunnelReopensAfterTheSessionEnds(t *testing.T) {
	f := newFakeService(t, echo(t), AgentDefaultVersion)
	tun, err := Listen(f, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer tun.Close()
	roundTrip(t, tun.Addr(), []byte("one"))
	tun.mu.Lock()
	tun.cur.e.Close() // the session drops (idle timeout, network)
	tun.mu.Unlock()
	roundTrip(t, tun.Addr(), []byte("two"))
	if f.started.Load() != 2 {
		t.Fatal("a dropped session was not replaced")
	}
}

func TestTunnelRefusedTokenIsAnError(t *testing.T) {
	f := newFakeService(t, echo(t), AgentDefaultVersion)
	f.refuseTok = true
	tun, err := Listen(f, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer tun.Close()
	c, err := net.Dial("tcp", tun.Addr())
	if err != nil {
		t.Fatal(err)
	}
	_ = c.SetDeadline(time.Now().Add(40 * time.Second))
	_, rerr := c.Read(make([]byte, 1))
	c.Close()
	if rerr == nil || tun.LastError() == nil {
		t.Fatalf("a refused token gave a working connection (read %v, last %v)", rerr, tun.LastError())
	}
}
