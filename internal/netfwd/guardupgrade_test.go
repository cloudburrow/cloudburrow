package netfwd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

// startUpgradingUpstream serves HTTP/1.1 and takes an h2c upgrade, as the
// Pub/Sub front does: 101, then HTTP/2 with the request as stream 1. Every
// answer names the protocol its request arrived in. settings receives each
// upgrade request's HTTP2-Settings.
func startUpgradingUpstream(t *testing.T) (addr string, settings chan string, upgrades *atomic.Int32) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	settings = make(chan string, 4)
	upgrades = &atomic.Int32{}
	answer := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "HTTP/%d %s", r.ProtoMajor, r.URL.Path)
	})
	h2 := &http2.Server{}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v := r.Header["Http2-Settings"]
		if r.Header.Get("Upgrade") != "h2c" || len(v) != 1 || !strings.Contains(r.Header.Get("Connection"), "HTTP2-Settings") {
			answer(w, r)
			return
		}
		settings <- v[0]
		b, err := base64.RawURLEncoding.DecodeString(v[0])
		if err != nil {
			answer(w, r)
			return
		}
		c, rw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer c.Close()
		upgrades.Add(1)
		_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: h2c\r\n\r\n")
		_ = rw.Flush()
		r.Header.Del("Upgrade")
		r.Header.Del("Connection")
		r.Header.Del("Http2-Settings")
		h2.ServeConn(&bufConn{Conn: c, r: rw.Reader}, &http2.ServeConnOpts{Context: context.Background(),
			Handler: answer, UpgradeRequest: r, Settings: b})
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return ln.Addr().String(), settings, upgrades
}

type bufConn struct {
	net.Conn
	r io.Reader
}

func (c *bufConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// upgradeRequest is what `curl --http2` sends on an http:// URL.
func upgradeRequest(host, path, settings string) string {
	return "GET " + path + " HTTP/1.1\r\nHost: " + host + "\r\nConnection: Upgrade, HTTP2-Settings\r\n" +
		"Upgrade: h2c\r\nHTTP2-Settings: " + settings + "\r\n\r\n"
}

// #964: an HTTP/1.1 request asking to switch to h2c reaches the port whole,
// HTTP2-Settings included, and the port's 101 and the HTTP/2 connection
// after it pass through the guard: stream 1 is the upgraded request, and a
// second request on the connection is answered over HTTP/2 too.
func TestGuardPassesAnH2CUpgrade(t *testing.T) {
	t.Parallel()
	up, settings, _ := startUpgradingUpstream(t)
	addr := startTestGuard(t, up)
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	// SETTINGS_MAX_CONCURRENT_STREAMS = 100, base64url.
	const sent = "AAMAAABk"
	if _, err := io.WriteString(c, upgradeRequest("127.0.0.1", "/v1/first", sent)); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols || !strings.EqualFold(resp.Header.Get("Upgrade"), "h2c") {
		t.Fatalf("the upgrade through the guard = %d %v, want 101 to h2c", resp.StatusCode, resp.Header)
	}
	if got := <-settings; got != sent {
		t.Errorf("the port was sent HTTP2-Settings %q, want %q", got, sent)
	}
	if _, err := io.WriteString(c, "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	fr := http2.NewFramer(c, br)
	if err := fr.WriteSettings(); err != nil {
		t.Fatal(err)
	}
	var hb bytes.Buffer
	enc := hpack.NewEncoder(&hb)
	for _, kv := range [][2]string{{":method", "GET"}, {":scheme", "http"}, {":path", "/v1/second"}, {":authority", "127.0.0.1"}} {
		_ = enc.WriteField(hpack.HeaderField{Name: kv[0], Value: kv[1]})
	}
	if err := fr.WriteHeaders(http2.HeadersFrameParam{StreamID: 3, BlockFragment: hb.Bytes(), EndHeaders: true, EndStream: true}); err != nil {
		t.Fatal(err)
	}
	body := map[uint32]string{}
	for done := 0; done < 2; {
		f, err := fr.ReadFrame()
		if err != nil {
			t.Fatalf("read the upgraded connection: %v (bodies %v)", err, body)
		}
		switch f := f.(type) {
		case *http2.SettingsFrame:
			if !f.IsAck() {
				_ = fr.WriteSettingsAck()
			}
		case *http2.DataFrame:
			body[f.StreamID] += string(f.Data())
			if f.StreamEnded() {
				done++
			}
		}
	}
	if body[1] != "HTTP/2 /v1/first" || body[3] != "HTTP/2 /v1/second" {
		t.Errorf("the upgraded connection answered %v; want both requests over HTTP/2", body)
	}
}

// An upgrade the port does not take is answered over HTTP/1.1, as before;
// one naming a foreign Host is refused like any HTTP/1.1 request, and never
// reaches the port.
func TestGuardChecksAnH2CUpgradeAndLeavesItToThePort(t *testing.T) {
	t.Parallel()
	emu := startFakeEmulator(t)
	addr := startTestGuard(t, emu.addr)
	send := func(host string) *http.Response {
		t.Helper()
		c, err := net.DialTimeout("tcp", addr, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		_ = c.SetDeadline(time.Now().Add(10 * time.Second))
		if _, err := io.WriteString(c, upgradeRequest(host, "/v1/projects/p/topics", "")); err != nil {
			t.Fatal(err)
		}
		resp, err := http.ReadResponse(bufio.NewReader(c), nil)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp
	}
	if resp := send("attacker.example:8085"); resp.StatusCode != http.StatusMisdirectedRequest {
		t.Errorf("an upgrade with a foreign Host = %d, want 421", resp.StatusCode)
	}
	if n := emu.restHits.Load(); n != 0 {
		t.Errorf("the refused upgrade reached the port (%d hits)", n)
	}
	if resp := send("127.0.0.1"); resp.StatusCode != http.StatusOK || resp.ProtoMajor != 1 {
		t.Errorf("an upgrade the port does not take = HTTP/%d %d, want HTTP/1.1 200", resp.ProtoMajor, resp.StatusCode)
	}
	if n := emu.restHits.Load(); n != 1 {
		t.Errorf("the port answered %d requests, want 1", n)
	}
}
