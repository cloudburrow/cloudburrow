package iap

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/iap/apiv1/iappb"
	"github.com/gorilla/websocket"
)

// echoServer answers each line with "echo: LINE".
func echoServer(t *testing.T) (string, int) {
	t.Helper()
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
			go func() {
				defer c.Close()
				sc := bufio.NewScanner(c)
				for sc.Scan() {
					if sc.Text() == "quit" {
						return
					}
					_, _ = c.Write([]byte("echo: " + sc.Text() + "\n"))
				}
			}()
		}
	}()
	return "127.0.0.1", ln.Addr().(*net.TCPAddr).Port
}

type tunnelClient struct {
	t  *testing.T
	ws *websocket.Conn
}

func dialTunnel(t *testing.T, base, path string, q url.Values, token string) (*tunnelClient, error) {
	t.Helper()
	h := http.Header{}
	if token != "" {
		h.Set("Authorization", "Bearer "+token)
	}
	d := websocket.Dialer{Subprotocols: []string{"relay.tunnel.cloudproxy.app"}}
	ws, _, err := d.Dial("ws"+strings.TrimPrefix(base, "http")+path+"?"+q.Encode(), h)
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() { ws.Close() })
	return &tunnelClient{t: t, ws: ws}, nil
}

// frame reads the next frame: its tag and body; a close is an error.
func (c *tunnelClient) frame() (uint16, []byte, error) {
	_ = c.ws.SetReadDeadline(time.Now().Add(10 * time.Second))
	_, b, err := c.ws.ReadMessage()
	if err != nil {
		return 0, nil, err
	}
	return binary.BigEndian.Uint16(b), b[2:], nil
}

func (c *tunnelClient) send(s string) {
	f := make([]byte, 6+len(s))
	binary.BigEndian.PutUint16(f, tunnelTagData)
	binary.BigEndian.PutUint32(f[2:], uint32(len(s)))
	copy(f[6:], s)
	if err := c.ws.WriteMessage(websocket.BinaryMessage, f); err != nil {
		c.t.Fatal(err)
	}
}

// data reads frames until a DATA frame, and is its payload.
func (c *tunnelClient) data() string {
	c.t.Helper()
	for {
		tag, b, err := c.frame()
		if err != nil {
			c.t.Fatalf("reading: %v", err)
		}
		if tag == tunnelTagData {
			n := binary.BigEndian.Uint32(b)
			return string(b[4 : 4+n])
		}
	}
}

func closeCode(err error) int {
	var ce *websocket.CloseError
	if errors.As(err, &ce) {
		return ce.Code
	}
	return 0
}

// TestTunnelRelay (#2160): the relay subprotocol end to end with a Go
// WebSocket client: CONNECT_SUCCESS_SID, DATA both ways with ACKs, a
// reconnect that resends what was not acknowledged, and IAP's close
// codes for a caller without the role, an unknown instance, an
// unreachable port and a host no destination group lists.
func TestTunnelRelay(t *testing.T) {
	ip, port := echoServer(t)
	srv, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.CreateTunnelDestGroup(context.Background(), &iappb.CreateTunnelDestGroupRequest{Parent: "projects/p/iap_tunnel/locations/us-central1",
		TunnelDestGroupId: "lab", TunnelDestGroup: &iappb.TunnelDestGroup{Cidrs: []string{"127.0.0.0/8"}}}); err != nil {
		t.Fatal(err)
	}
	tun := &Tunnel{IAP: srv,
		Caller: func(b string) (string, bool) { return strings.TrimPrefix(b, "tok-"), strings.HasPrefix(b, "tok-") },
		Allowed: func(_ context.Context, perm, res, email, dip string, dport int) bool {
			switch email {
			case "dev@example.com":
				return perm == "iap.tunnelInstances.accessViaIAP" && strings.HasPrefix(res, "projects/p/iap_tunnel/zones/z/instances/") && (dip == ip || dip == "")
			case "ops@example.com":
				return perm == "iap.tunnelDestGroups.accessViaIAP" && res == "projects/p/iap_tunnel/locations/us-central1/destGroups/lab" && dport == port
			}
			return false
		},
		Instance: func(_ context.Context, project, zone, inst, iface string) (TunnelDest, error) {
			switch inst {
			case "vm":
				return TunnelDest{IP: ip}, nil
			case "stopped":
				return TunnelDest{IP: ip}, ErrTunnelStopped
			}
			return TunnelDest{}, ErrTunnelNotFound
		},
		Host: func(_ context.Context, project, region, network, host string) (TunnelDest, error) {
			return TunnelDest{IP: host}, nil
		},
		Dial: func(ctx context.Context, d TunnelDest, p int) (net.Conn, error) {
			var dl net.Dialer
			return dl.DialContext(ctx, "tcp", net.JoinHostPort(d.IP, strconv.Itoa(p)))
		},
	}
	hs := httptest.NewServer(tun)
	t.Cleanup(hs.Close)
	inst := url.Values{"project": {"p"}, "zone": {"z"}, "instance": {"vm"}, "interface": {"nic0"}, "port": {strconv.Itoa(port)}}

	c, err := dialTunnel(t, hs.URL, "/v4/connect", inst, "tok-dev@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if c.ws.Subprotocol() != "relay.tunnel.cloudproxy.app" {
		t.Fatalf("subprotocol %q", c.ws.Subprotocol())
	}
	tag, b, err := c.frame()
	if err != nil || tag != tunnelTagConnectSID {
		t.Fatalf("first frame: %d %v", tag, err)
	}
	sid := string(b[4 : 4+binary.BigEndian.Uint32(b)])
	c.send("hello\n")
	if got := c.data(); got != "echo: hello\n" {
		t.Fatalf("relayed %q", got)
	}
	// Break the WebSocket without acknowledging, send more, and reconnect:
	// the reply is resent, and RECONNECT_SUCCESS_ACK counts our bytes.
	c.send("again\n")
	time.Sleep(200 * time.Millisecond)
	_ = c.ws.UnderlyingConn().Close()
	c2, err := dialTunnel(t, hs.URL, "/v4/reconnect", url.Values{"sid": {sid}, "ack": {"0"}, "zone": {"z"}}, "tok-dev@example.com")
	if err != nil {
		t.Fatal(err)
	}
	tag, b, err = c2.frame()
	if err != nil || tag != tunnelTagReconnectAck || binary.BigEndian.Uint64(b) != uint64(len("hello\nagain\n")) {
		t.Fatalf("reconnect: %d %v %v", tag, b, err)
	}
	got := c2.data()
	for !strings.Contains(got, "echo: again\n") {
		got += c2.data()
	}
	if got != "echo: hello\necho: again\n" {
		t.Fatalf("after reconnect %q", got)
	}
	// Another caller cannot take the session over.
	c3, err := dialTunnel(t, hs.URL, "/v4/reconnect", url.Values{"sid": {sid}, "ack": {"0"}}, "tok-eve@example.com")
	if err == nil {
		if _, _, err := c3.frame(); closeCode(err) != TunnelNotAuthorized {
			t.Fatalf("reconnect by another: %v", err)
		}
	}
	// The destination closing closes the tunnel.
	c2.send("quit\n")
	for {
		_, _, err := c2.frame()
		if err != nil {
			if code := closeCode(err); code != websocket.CloseNormalClosure {
				t.Fatalf("after quit: %v", err)
			}
			break
		}
	}

	refused := func(name string, q url.Values, token string, want int) {
		t.Helper()
		c, err := dialTunnel(t, hs.URL, "/v4/connect", q, token)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if _, _, err := c.frame(); closeCode(err) != want {
			t.Fatalf("%s: %v, want %d", name, err, want)
		}
	}
	with := func(k, v string) url.Values {
		q := url.Values{}
		for a, b := range inst {
			q[a] = b
		}
		q.Set(k, v)
		return q
	}
	refused("no token", inst, "", TunnelNotAuthorized)
	refused("no role", inst, "tok-eve@example.com", TunnelNotAuthorized)
	refused("unknown instance", with("instance", "gone"), "tok-dev@example.com", TunnelInstanceNotFound)
	refused("stopped instance", with("instance", "stopped"), "tok-dev@example.com", TunnelInstanceNotFound)
	refused("bad port", with("port", "x"), "tok-dev@example.com", TunnelInvalidArgument)
	refused("closed port", with("port", "1"), "tok-dev@example.com", TunnelFailedToConnect)

	// A host tunnel through the destination group.
	host := url.Values{"project": {"p"}, "region": {"us-central1"}, "network": {"default"}, "host": {ip}, "port": {strconv.Itoa(port)}, "group": {"lab"}}
	c4, err := dialTunnel(t, hs.URL, "/v4/connect", host, "tok-ops@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if tag, _, err := c4.frame(); err != nil || tag != tunnelTagConnectSID {
		t.Fatalf("host tunnel: %d %v", tag, err)
	}
	c4.send("via group\n")
	if got := c4.data(); got != "echo: via group\n" {
		t.Fatalf("host tunnel relayed %q", got)
	}
	out := url.Values{}
	for a, b := range host {
		out[a] = b
	}
	out.Set("host", "10.9.9.9")
	refused("host outside the group", out, "tok-ops@example.com", TunnelNotAuthorized)
	out.Set("host", ip)
	out.Set("group", "other")
	refused("unknown group", out, "tok-ops@example.com", TunnelNotAuthorized)
}

func TestGroupMatches(t *testing.T) {
	g := &iappb.TunnelDestGroup{Cidrs: []string{"10.0.0.0/24"}, Fqdns: []string{"db.example.com", "*.corp.example"}}
	for h, want := range map[string]bool{"10.0.0.7": true, "10.0.1.7": false, "db.example.com": true, "DB.example.com.": true,
		"a.corp.example": true, "corp.example": false, "x.example.com": false} {
		if GroupMatches(g, h) != want {
			t.Errorf("GroupMatches(%s) = %v", h, !want)
		}
	}
}
