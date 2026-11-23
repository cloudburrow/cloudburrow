package iap

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/iap/apiv1/iappb"
	"github.com/gorilla/websocket"
)

// IAP TCP forwarding (#2160), as https://docs.cloud.google.com/iap/docs/tcp-forwarding-overview
// and .../iap/docs/using-tcp-forwarding describe it (read 2026-11-22),
// with the relay protocol gcloud speaks to Google's tunnel endpoint
// (wss://tunnel.cloudproxy.app/v4/connect, the
// relay.tunnel.cloudproxy.app WebSocket subprotocol, in gcloud's
// googlecloudsdk/api_lib/compute/iap_tunnel_websocket_utils.py): a tunnel
// to an instance's port (project, zone, instance, interface, port) needs
// iap.tunnelInstances.accessViaIAP (IAP-secured Tunnel User,
// roles/iap.tunnelResourceAccessor) on projects/P/iap_tunnel/zones/Z/
// instances/I or above it; one to a host (project, region, network, host,
// port, and optionally group) iap.tunnelDestGroups.accessViaIAP on a
// destination group of the region that lists the host. Conditions read
// destination.ip and destination.port. The connection reaches the instance
// as Google's comes from 35.235.240.0/20, so the network's firewall
// rules must allow that range on the port.
//
// Frames are binary WebSocket messages: a 2-byte big-endian tag, then
// CONNECT_SUCCESS_SID (0x0001: a 4-byte length and the session ID),
// RECONNECT_SUCCESS_ACK (0x0002: the 8-byte count of bytes received from
// the client), DATA (0x0004: a 4-byte length and at most 16 KiB) or ACK
// (0x0007: the 8-byte count of bytes received). A refusal closes the
// WebSocket with IAP's codes (iap/docs/faq): 4003 (failed to connect to
// backend), 4033 (not authorized), 4047 (instance not found or not
// running); a session whose WebSocket breaks waits a minute for
// /v4/reconnect?sid=S&ack=N.

// Tunnel close codes, from IAP's FAQ.
const (
	TunnelInvalidArgument   = 4000
	TunnelFailedToConnect   = 4003
	TunnelNotAuthorized     = 4033
	TunnelInstanceNotFound  = 4047
	TunnelDestinationClosed = 4010
	tunnelSubprotocol       = "relay.tunnel.cloudproxy.app"
	tunnelMaxData           = 16384
	tunnelTagConnectSID     = 0x0001
	tunnelTagReconnectAck   = 0x0002
	tunnelTagData           = 0x0004
	tunnelTagAck            = 0x0007
)

// TunnelPaths are the paths gcloud's --iap-tunnel-url-override reaches.
var TunnelPaths = []string{"/v4/connect", "/v4/reconnect"}

// TunnelOrigin is the Origin gcloud sends on the tunnel's WebSocket
// (TUNNEL_CLOUDPROXY_ORIGIN in iap_tunnel_websocket_helper.py).
const TunnelOrigin = "bot:iap-tunneler"

// ErrTunnelNotFound and ErrTunnelStopped are a destination's lookups that
// close the tunnel with 4047.
var (
	ErrTunnelNotFound = errors.New("Failed to lookup instance")
	ErrTunnelStopped  = errors.New("Instance is not running")
)

// TunnelDest is a tunnel's destination, resolved.
type TunnelDest struct {
	// IP is its internal address, which conditions read as
	// destination.ip.
	IP string
	// Handle is the resolver's own reference, passed back to Dial.
	Handle any
}

// Tunnel serves IAP TCP forwarding's relay.
type Tunnel struct {
	// IAP is the API whose destination groups and policies apply.
	IAP *Server
	// Caller is the account of a bearer token, ok false for a token this
	// instance did not issue.
	Caller func(bearer string) (email string, ok bool)
	// Allowed checks permission on resource (with the IAP levels above
	// it) for email, with the destination's address and port.
	Allowed func(ctx context.Context, permission, resource, email, ip string, port int) bool
	// Instance resolves an instance's interface (nic0 by default).
	Instance func(ctx context.Context, project, zone, instance, iface string) (TunnelDest, error)
	// Host resolves a host (an address or name) in a network of a region.
	Host func(ctx context.Context, project, region, network, host string) (TunnelDest, error)
	// Dial opens the connection to a destination's port; an error closes
	// the tunnel with 4003.
	Dial func(ctx context.Context, d TunnelDest, port int) (net.Conn, error)

	mu       sync.Mutex
	sessions map[string]*tunnelSession
}

var tunnelUpgrader = websocket.Upgrader{
	Subprotocols:    []string{tunnelSubprotocol},
	ReadBufferSize:  32 << 10,
	WriteBufferSize: 32 << 10,
	// A tunnel is authorized by its bearer token, which a browser's
	// WebSocket cannot send, never by its origin.
	CheckOrigin: func(*http.Request) bool { return true },
}

// ServeHTTP serves /v4/connect and /v4/reconnect.
func (t *Tunnel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !websocket.IsWebSocketUpgrade(r) {
		http.Error(w, "IAP TCP forwarding needs a WebSocket", http.StatusBadRequest)
		return
	}
	ws, err := tunnelUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	email, ok := "", false
	if v := r.Header.Get("Authorization"); len(v) > 7 && strings.EqualFold(v[:7], "bearer ") && t.Caller != nil {
		email, ok = t.Caller(strings.TrimSpace(v[7:]))
	}
	if !ok {
		tunnelClose(ws, TunnelNotAuthorized, "not authorized")
		return
	}
	q := r.URL.Query()
	if strings.HasSuffix(r.URL.Path, "/reconnect") {
		t.reconnect(r.Context(), ws, email, q.Get("sid"), q.Get("ack"))
		return
	}
	port, err := strconv.Atoi(q.Get("port"))
	project := q.Get("project")
	if err != nil || port < 1 || port > 65535 || project == "" {
		tunnelClose(ws, TunnelInvalidArgument, "invalid argument")
		return
	}
	ctx := context.WithoutCancel(r.Context())
	var dest TunnelDest
	if host := q.Get("host"); host != "" {
		dest, ok = t.authorizeHost(ctx, ws, email, project, q.Get("region"), q.Get("network"), host, q.Get("group"), port)
	} else {
		dest, ok = t.authorizeInstance(ctx, ws, email, project, q.Get("zone"), q.Get("instance"), q.Get("interface"), port)
	}
	if !ok {
		return
	}
	dctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	conn, err := t.Dial(dctx, dest, port)
	cancel()
	if err != nil {
		tunnelClose(ws, TunnelFailedToConnect, "failed to connect to backend")
		return
	}
	sid := make([]byte, 18)
	_, _ = rand.Read(sid)
	ss := &tunnelSession{t: t, id: base64.RawURLEncoding.EncodeToString(sid), email: email, backend: conn}
	ss.cond = sync.NewCond(&ss.mu)
	t.mu.Lock()
	if t.sessions == nil {
		t.sessions = map[string]*tunnelSession{}
	}
	t.sessions[ss.id] = ss
	t.mu.Unlock()
	f := make([]byte, 6+len(ss.id))
	binary.BigEndian.PutUint16(f, tunnelTagConnectSID)
	binary.BigEndian.PutUint32(f[2:], uint32(len(ss.id)))
	copy(f[6:], ss.id)
	ss.mu.Lock()
	ss.ws = ws
	err = ws.WriteMessage(websocket.BinaryMessage, f)
	ss.mu.Unlock()
	if err != nil {
		ss.end()
		return
	}
	go ss.pump()
	ss.read(ws)
}

func tunnelClose(ws *websocket.Conn, code int, msg string) {
	_ = ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, msg), time.Now().Add(5*time.Second))
	_ = ws.Close()
}

func (t *Tunnel) authorizeInstance(ctx context.Context, ws *websocket.Conn, email, project, zone, instance, iface string, port int) (TunnelDest, bool) {
	if zone == "" || instance == "" {
		tunnelClose(ws, TunnelInvalidArgument, "invalid argument")
		return TunnelDest{}, false
	}
	if iface == "" {
		iface = "nic0"
	}
	res := "projects/" + project + "/iap_tunnel/zones/" + zone + "/instances/" + instance
	if t.IAP != nil {
		res = t.IAP.norm(res)
	}
	dest, err := t.Instance(ctx, project, zone, instance, iface)
	if !t.Allowed(ctx, "iap.tunnelInstances.accessViaIAP", res, email, dest.IP, port) {
		tunnelClose(ws, TunnelNotAuthorized, "not authorized")
		return TunnelDest{}, false
	}
	switch {
	case errors.Is(err, ErrTunnelNotFound), errors.Is(err, ErrTunnelStopped):
		tunnelClose(ws, TunnelInstanceNotFound, err.Error())
		return TunnelDest{}, false
	case err != nil:
		tunnelClose(ws, TunnelFailedToConnect, "failed to connect to backend")
		return TunnelDest{}, false
	}
	return dest, true
}

// GroupMatches is whether a destination group lists host: an address in
// one of its CIDR ranges, or a name its FQDNs match (a leading "*."
// matching any subdomain).
func GroupMatches(g *iappb.TunnelDestGroup, host string) bool {
	if ip := net.ParseIP(host); ip != nil {
		for _, c := range g.GetCidrs() {
			if _, n, err := net.ParseCIDR(c); err == nil && n.Contains(ip) {
				return true
			}
		}
		return false
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, f := range g.GetFqdns() {
		f = strings.ToLower(f)
		if f == host || (strings.HasPrefix(f, "*.") && strings.HasSuffix(host, f[1:])) {
			return true
		}
	}
	return false
}

func (t *Tunnel) authorizeHost(ctx context.Context, ws *websocket.Conn, email, project, region, network, host, group string, port int) (TunnelDest, bool) {
	if region == "" || network == "" || t.IAP == nil || t.Host == nil {
		tunnelClose(ws, TunnelInvalidArgument, "invalid argument")
		return TunnelDest{}, false
	}
	project = t.IAP.norm("projects/" + project)[len("projects/"):]
	dest, err := t.Host(ctx, project, region, network, host)
	prefix := "projects/" + project + "/iap_tunnel/locations/" + region + "/destGroups/"
	t.IAP.mu.Lock()
	var groups []*iappb.TunnelDestGroup
	for name, g := range t.IAP.groups {
		if strings.HasPrefix(name, prefix) && (group == "" || name == prefix+group) {
			groups = append(groups, g)
		}
	}
	t.IAP.mu.Unlock()
	allowed := false
	for _, g := range groups {
		if (GroupMatches(g, host) || (dest.IP != "" && GroupMatches(g, dest.IP))) &&
			t.Allowed(ctx, "iap.tunnelDestGroups.accessViaIAP", g.GetName(), email, dest.IP, port) {
			allowed = true
			break
		}
	}
	if !allowed {
		tunnelClose(ws, TunnelNotAuthorized, "not authorized")
		return TunnelDest{}, false
	}
	if err != nil {
		tunnelClose(ws, TunnelFailedToConnect, "failed to connect to backend")
		return TunnelDest{}, false
	}
	return dest, true
}

func (t *Tunnel) reconnect(ctx context.Context, ws *websocket.Conn, email, sid, ack string) {
	t.mu.Lock()
	ss := t.sessions[sid]
	t.mu.Unlock()
	n, err := strconv.ParseUint(ack, 10, 64)
	if ss == nil || err != nil || !strings.EqualFold(ss.email, email) {
		tunnelClose(ws, TunnelNotAuthorized, "not authorized")
		return
	}
	_ = ctx
	ss.mu.Lock()
	if ss.done || n < ss.acked || n > ss.sent {
		ss.mu.Unlock()
		tunnelClose(ws, TunnelInvalidArgument, "invalid argument")
		return
	}
	if old := ss.ws; old != nil {
		_ = old.Close()
	}
	ss.trimLocked(n)
	f := make([]byte, 10)
	binary.BigEndian.PutUint16(f, tunnelTagReconnectAck)
	binary.BigEndian.PutUint64(f[2:], ss.received)
	err = ws.WriteMessage(websocket.BinaryMessage, f)
	for off := 0; err == nil && off < len(ss.unacked); off += tunnelMaxData {
		end := min(off+tunnelMaxData, len(ss.unacked))
		err = ws.WriteMessage(websocket.BinaryMessage, dataFrame(ss.unacked[off:end]))
	}
	ss.ws = ws
	ss.gen++
	ss.cond.Broadcast()
	ss.mu.Unlock()
	if err != nil {
		ss.detach(ws)
		return
	}
	ss.read(ws)
}

func dataFrame(b []byte) []byte {
	f := make([]byte, 6+len(b))
	binary.BigEndian.PutUint16(f, tunnelTagData)
	binary.BigEndian.PutUint32(f[2:], uint32(len(b)))
	copy(f[6:], b)
	return f
}

// tunnelSession is one tunnel: its connection to the destination and the
// bytes each way, which outlive a broken WebSocket for a minute.
type tunnelSession struct {
	t       *Tunnel
	id      string
	email   string
	backend net.Conn

	mu   sync.Mutex
	cond *sync.Cond
	ws   *websocket.Conn
	gen  int
	// sent counts the bytes sent to the client, acked those it confirmed;
	// unacked are the bytes between, kept to resend after a reconnect.
	sent, acked uint64
	unacked     []byte
	// received counts the bytes the client sent.
	received uint64
	done     bool
	timer    *time.Timer
}

const tunnelMaxUnacked = 4 << 20

func (ss *tunnelSession) trimLocked(ack uint64) {
	if ack <= ss.acked {
		return
	}
	drop := ack - ss.acked
	if drop > uint64(len(ss.unacked)) {
		drop = uint64(len(ss.unacked))
	}
	ss.unacked = ss.unacked[drop:]
	ss.acked = ack
	ss.cond.Broadcast()
}

// pump sends the destination's bytes to the client.
func (ss *tunnelSession) pump() {
	buf := make([]byte, tunnelMaxData)
	for {
		n, err := ss.backend.Read(buf)
		if n > 0 {
			ss.mu.Lock()
			for !ss.done && (ss.ws == nil || len(ss.unacked) > tunnelMaxUnacked) {
				ss.cond.Wait()
			}
			if ss.done {
				ss.mu.Unlock()
				return
			}
			ss.unacked = append(ss.unacked, buf[:n]...)
			ss.sent += uint64(n)
			ws := ss.ws
			werr := ws.WriteMessage(websocket.BinaryMessage, dataFrame(buf[:n]))
			ss.mu.Unlock()
			if werr != nil {
				ss.detach(ws)
			}
		}
		if err != nil {
			// The destination closed: so does the tunnel.
			ss.mu.Lock()
			for !ss.done && ss.ws == nil {
				ss.cond.Wait()
			}
			if ws := ss.ws; ws != nil && !ss.done {
				_ = ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(5*time.Second))
			}
			ss.mu.Unlock()
			ss.end()
			return
		}
	}
}

// read relays the client's frames on ws until it closes or breaks.
func (ss *tunnelSession) read(ws *websocket.Conn) {
	for {
		typ, b, err := ws.ReadMessage()
		if err != nil {
			var ce *websocket.CloseError
			if errors.As(err, &ce) {
				ss.end()
				return
			}
			ss.detach(ws)
			return
		}
		if typ != websocket.BinaryMessage || len(b) < 2 {
			continue
		}
		switch binary.BigEndian.Uint16(b) {
		case tunnelTagData:
			if len(b) < 6 {
				continue
			}
			n := int(binary.BigEndian.Uint32(b[2:]))
			if n > len(b)-6 {
				ss.end()
				return
			}
			if _, err := ss.backend.Write(b[6 : 6+n]); err != nil {
				ss.mu.Lock()
				if ss.ws == ws {
					_ = ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(TunnelDestinationClosed, "destination read failed"), time.Now().Add(5*time.Second))
				}
				ss.mu.Unlock()
				ss.end()
				return
			}
			ss.mu.Lock()
			ss.received += uint64(n)
			f := make([]byte, 10)
			binary.BigEndian.PutUint16(f, tunnelTagAck)
			binary.BigEndian.PutUint64(f[2:], ss.received)
			var werr error
			if ss.ws == ws {
				werr = ws.WriteMessage(websocket.BinaryMessage, f)
			}
			ss.mu.Unlock()
			if werr != nil {
				ss.detach(ws)
				return
			}
		case tunnelTagAck:
			if len(b) < 10 {
				continue
			}
			ss.mu.Lock()
			ss.trimLocked(binary.BigEndian.Uint64(b[2:]))
			ss.mu.Unlock()
		}
	}
}

// detach forgets a broken WebSocket; the session ends unless the client
// reconnects within a minute.
func (ss *tunnelSession) detach(ws *websocket.Conn) {
	_ = ws.Close()
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if ss.ws != ws || ss.done {
		return
	}
	ss.ws = nil
	gen := ss.gen
	ss.timer = time.AfterFunc(time.Minute, func() {
		ss.mu.Lock()
		stale := ss.ws == nil && ss.gen == gen
		ss.mu.Unlock()
		if stale {
			ss.end()
		}
	})
}

// end closes the session.
func (ss *tunnelSession) end() {
	ss.mu.Lock()
	if ss.done {
		ss.mu.Unlock()
		return
	}
	ss.done = true
	ws := ss.ws
	ss.ws = nil
	ss.cond.Broadcast()
	ss.mu.Unlock()
	_ = ss.backend.Close()
	if ws != nil {
		_ = ws.Close()
	}
	ss.t.mu.Lock()
	delete(ss.t.sessions, ss.id)
	ss.t.mu.Unlock()
}
