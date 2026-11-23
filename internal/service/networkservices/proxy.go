package networkservices

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	nsvpb "cloud.google.com/go/networkservices/apiv1/networkservicespb"

	"github.com/cloudburrow/cloudburrow/internal/egress"
)

// Secure Web Proxy's data plane (#2160): each SECURE_WEB_GATEWAY gateway
// is an explicit forward proxy (routingMode EXPLICIT_ROUTING_MODE), as
// https://docs.cloud.google.com/secure-web-proxy/docs/overview,
// .../quickstart, .../policies-and-rules-overview and .../configure-rules
// describe it (read 2026-11-22):
//
//   - each of its ports is listened on at the host (Options.Bind), on a
//     port of its own, given in the gateway's JSON as cloudburrowAddress
//     and cloudburrowAddresses (PORT=HOST:PORT), kept across restarts, as
//     a forwarding rule's are; a client connects to it over plain HTTP, or
//     over TLS when the gateway has certificateUrls;
//   - an HTTP CONNECT tunnels to its target, and an absolute-URI request
//     (GET http://host/path) is forwarded, both after the gateway security
//     policy's rules: "evaluates rules based on priority, with the lowest
//     number checked first", the first rule whose sessionMatcher (and
//     applicationMatcher, when it has one) matches decides (basicProfile
//     ALLOW or DENY), and with none "gateway security policies are
//     deny-all by default"; a denied request gets 403 Forbidden;
//   - for a CONNECT, host() is the CONNECT target and the request
//     attributes are not known, so a rule with an applicationMatcher
//     matches a tunnel only with TLS inspection (tlsInspectionEnabled and
//     the policy's tlsInspectionPolicy, tlsinspect.go), which decrypts it
//     and matches each request inside it;
//   - the proxy dials out through CloudBurrow's egress guard: a request
//     to a googleapis.com API served here goes to this instance, and no
//     connection is ever opened to Google (502 Bad Gateway instead).

// GatewayRule is a gateway security policy rule as the proxy applies it.
type GatewayRule struct {
	Name          string
	Priority      int32
	Enabled       bool
	Allow         bool
	TLSInspection bool
	Session       string
	Application   string
}

// Policies is Network Security as the proxy reads it.
type Policies interface {
	// GatewayRules are a gateway security policy's rules
	// (projects/P/locations/L/gatewaySecurityPolicies/X); ok is false when
	// there is no such policy.
	GatewayRules(policy string) (rules []GatewayRule, ok bool)
	// URLList is a URL list's values (projects/P/locations/L/urlLists/X).
	URLList(name string) (values []string, ok bool)
}

// Source is what the proxy knows of a client by its address: the
// service accounts and secure tags (tagValues/ID) of the VM instance
// that has it.
type Source func(ip string) (serviceAccounts, tags []string)

// Certificates are Certificate Manager certificates, by name, that a
// gateway's TLS port presents.
type Certificates func(names []string) []tls.Certificate

// UsePolicies, UseSource and UseCertificates set what the proxy reads.
func (s *Server) UsePolicies(p Policies) {
	s.proxy.mu.Lock()
	s.proxy.policies = p
	s.proxy.mu.Unlock()
}

func (s *Server) UseSource(f Source) {
	s.proxy.mu.Lock()
	s.proxy.source = f
	s.proxy.mu.Unlock()
}

func (s *Server) UseCertificates(f Certificates) {
	s.proxy.mu.Lock()
	s.proxy.certs = f
	s.proxy.mu.Unlock()
}

// UseTransport sets what plain HTTP requests are forwarded with, and
// UseDialer what tunnels dial; both are behind the egress guard by
// default. Tests put a loopback-only dialer here.
func (s *Server) UseTransport(rt http.RoundTripper, dial func(ctx context.Context, network, addr string) (net.Conn, error)) {
	s.proxy.mu.Lock()
	if rt != nil {
		s.proxy.transport = rt
	}
	if dial != nil {
		s.proxy.dial = dial
	}
	s.proxy.mu.Unlock()
}

type proxyPlane struct {
	s         *Server
	mu        sync.Mutex
	listeners map[string][]*swpListener // by gateway
	prev      map[string]string         // "gateway|port" -> host address
	policies  Policies
	source    Source
	certs     Certificates
	transport http.RoundTripper
	dial      func(ctx context.Context, network, addr string) (net.Conn, error)
	started   bool
	m         matchers
	issuer    LeafIssuer     // TLS inspection's CA pool issuer (tlsinspect.go)
	roots     *x509.CertPool // an inspected server's CA set; nil: the system's
	leaves    leafCache
}

type swpListener struct {
	gateway string
	port    int32
	addr    string
	ln      net.Listener
	srv     *http.Server
}

func newProxyPlane(s *Server) *proxyPlane {
	return &proxyPlane{s: s, listeners: map[string][]*swpListener{}, prev: map[string]string{},
		transport: egress.Transport(&http.Transport{Proxy: nil, MaxIdleConnsPerHost: 8, IdleConnTimeout: 300 * time.Second}),
		dial:      (&net.Dialer{Timeout: 30 * time.Second}).DialContext}
}

func (p *proxyPlane) addrsSnapshot() map[string]string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := map[string]string{}
	for k, v := range p.prev {
		out[k] = v
	}
	for _, ls := range p.listeners {
		for _, l := range ls {
			out[l.gateway+"|"+strconv.Itoa(int(l.port))] = l.addr
		}
	}
	return out
}

// Start opens the gateways' listeners; before it, records are kept but
// nothing listens.
func (s *Server) Start() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.proxy.mu.Lock()
	s.proxy.started = true
	s.proxy.mu.Unlock()
	s.changedLocked()
}

// Stop closes every listener.
func (s *Server) Stop() {
	p := s.proxy
	p.mu.Lock()
	defer p.mu.Unlock()
	p.started = false
	for g, ls := range p.listeners {
		for _, l := range ls {
			l.close()
		}
		delete(p.listeners, g)
	}
}

func (l *swpListener) close() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = l.srv.Shutdown(ctx)
	_ = l.ln.Close()
}

// gatewayPorts are the ports a gateway listens on: its ports, or, with
// allPorts, 80 and 443 (an explicit proxy's clients name one port; the
// rest of the range is not listened on here, docs/gaps.md).
func gatewayPorts(g *nsvpb.Gateway) []int32 {
	if g.GetAllPorts() {
		return []int32{80, 443}
	}
	return g.GetPorts()
}

// changedLocked opens the listeners of new SECURE_WEB_GATEWAY gateways and
// closes those of deleted ones; s.mu is held.
func (s *Server) changedLocked() {
	p := s.proxy
	p.mu.Lock()
	if !p.started {
		p.mu.Unlock()
		return
	}
	want := map[string]*nsvpb.Gateway{}
	for k, m := range s.res {
		if g, ok := m.(*nsvpb.Gateway); ok && g.GetType() == nsvpb.Gateway_SECURE_WEB_GATEWAY {
			want[k] = g
		}
	}
	for name, ls := range p.listeners {
		if _, ok := want[name]; !ok {
			for _, l := range ls {
				l.close()
				delete(p.prev, name+"|"+strconv.Itoa(int(l.port)))
			}
			delete(p.listeners, name)
		}
	}
	changed := false
	for name, g := range want {
		if _, ok := p.listeners[name]; ok {
			continue
		}
		for _, port := range gatewayPorts(g) {
			key := name + "|" + strconv.Itoa(int(port))
			var ln net.Listener
			var err error
			if a := p.prev[key]; a != "" {
				ln, err = net.Listen("tcp", a)
			}
			if ln == nil || err != nil {
				ln, err = net.Listen("tcp", net.JoinHostPort(s.opts.Bind, "0"))
			}
			if err != nil {
				continue
			}
			l := &swpListener{gateway: name, port: port, addr: ln.Addr().String(), ln: ln}
			l.srv = &http.Server{Handler: &swpHandler{p: p, gateway: name, port: port}, ReadHeaderTimeout: 30 * time.Second,
				IdleTimeout: 300 * time.Second}
			go func() { _ = l.srv.Serve(&sniffListener{Listener: ln, p: p, gateway: name}) }()
			p.listeners[name] = append(p.listeners[name], l)
			p.prev[key] = l.addr
			changed = true
		}
	}
	p.mu.Unlock()
	if changed {
		_ = s.saveLocked()
	}
}

// GatewayAddresses are a gateway's listeners, PORT=HOST:PORT, in port
// order; nil when it listens on nothing.
func (s *Server) GatewayAddresses(name string) []string {
	p := s.proxy
	p.mu.Lock()
	defer p.mu.Unlock()
	ls := append([]*swpListener(nil), p.listeners[name]...)
	sort.Slice(ls, func(i, j int) bool { return ls[i].port < ls[j].port })
	var out []string
	for _, l := range ls {
		out = append(out, strconv.Itoa(int(l.port))+"="+l.addr)
	}
	return out
}

// GatewayAddress is the host address of a gateway's port, "" when none.
func (s *Server) GatewayAddress(name string, port int32) string {
	for _, a := range s.GatewayAddresses(name) {
		if strings.HasPrefix(a, strconv.Itoa(int(port))+"=") {
			return a[strings.Index(a, "=")+1:]
		}
	}
	return ""
}

// sniffListener hands each connection to the HTTP server as it is, or,
// when it opens with a TLS handshake and the gateway has certificates,
// as a TLS connection: a client may speak HTTPS to the proxy itself.
type sniffListener struct {
	net.Listener
	p       *proxyPlane
	gateway string
}

func (l *sniffListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &sniffConn{Conn: c, l: l}, nil
}

// sniffConn decides on its first Read whether it is TLS.
type sniffConn struct {
	net.Conn
	l    *sniffListener
	once sync.Once
	r    io.Reader
	tc   *tls.Conn
}

func (c *sniffConn) init() {
	br := bufio.NewReader(c.Conn)
	_ = c.Conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	b, err := br.Peek(1)
	_ = c.Conn.SetReadDeadline(time.Time{})
	pre := &prefixConn{Conn: c.Conn, r: br}
	if err == nil && b[0] == 0x16 {
		if certs := c.l.p.certificates(c.l.gateway); len(certs) > 0 {
			c.tc = tls.Server(pre, &tls.Config{Certificates: certs, NextProtos: []string{"http/1.1"}})
			c.r = c.tc
			return
		}
	}
	c.r = pre
}

func (c *sniffConn) Read(b []byte) (int, error) {
	c.once.Do(c.init)
	return c.r.Read(b)
}

func (c *sniffConn) Write(b []byte) (int, error) {
	c.once.Do(c.init)
	if c.tc != nil {
		return c.tc.Write(b)
	}
	return c.Conn.Write(b)
}

type prefixConn struct {
	net.Conn
	r io.Reader
}

func (c *prefixConn) Read(b []byte) (int, error) { return c.r.Read(b) }

func (p *proxyPlane) certificates(gateway string) []tls.Certificate {
	p.s.mu.Lock()
	g, _ := p.s.res[gateway].(*nsvpb.Gateway)
	var names []string
	if g != nil {
		for _, c := range g.GetCertificateUrls() {
			names = append(names, relRef(c))
		}
	}
	p.s.mu.Unlock()
	p.mu.Lock()
	f := p.certs
	p.mu.Unlock()
	if f == nil || len(names) == 0 {
		return nil
	}
	return f(names)
}

// swpHandler is one gateway port's proxy.
type swpHandler struct {
	p       *proxyPlane
	gateway string
	port    int32
}

// hopHeaders are removed from a forwarded request and its response.
var hopHeaders = []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
	"Te", "Trailer", "Transfer-Encoding", "Upgrade"}

func (h *swpHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	srcIP, srcPort := splitAddr(r.RemoteAddr)
	if r.Method == http.MethodConnect {
		host, port := splitHostPort(r.Host, 443)
		in := matchInput{host: host, destPort: port, srcIP: srcIP, srcPort: srcPort}
		d := h.p.decideTunnel(h.gateway, in)
		if !d.allow {
			deny(w)
			return
		}
		if d.inspect {
			h.inspect(w, r, d, host, port, srcIP, srcPort)
			return
		}
		h.tunnel(w, r, net.JoinHostPort(host, strconv.Itoa(port)))
		return
	}
	// An explicit proxy's client sends the absolute URI. An origin-form
	// request is what a web page's fetch to the proxy's own address sends
	// (any Host, by DNS rebinding): it is refused, so no page reaches
	// through the proxy (ADR-0004, #1586); a page cannot send CONNECT.
	if !r.URL.IsAbs() || r.URL.Host == "" {
		http.Error(w, "Bad Request: a request through a secure web proxy names its absolute URI", http.StatusBadRequest)
		return
	}
	u := *r.URL
	def := 80
	if u.Scheme == "https" {
		def = 443
	}
	host, port := splitHostPort(u.Host, def)
	if host == "" {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	in := matchInput{host: host, destPort: port, srcIP: srcIP, srcPort: srcPort, req: r, scheme: u.Scheme}
	if !h.p.allowed(h.gateway, in) {
		deny(w)
		return
	}
	out := r.Clone(r.Context())
	out.RequestURI = ""
	out.URL = &u
	out.Host = r.Host
	if out.Host == "" {
		out.Host = u.Host
	}
	for _, k := range hopHeaders {
		out.Header.Del(k)
	}
	h.p.mu.Lock()
	rt := h.p.transport
	h.p.mu.Unlock()
	resp, err := rt.RoundTrip(out)
	if err != nil {
		badGateway(w, err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	for _, k := range hopHeaders {
		resp.Header.Del(k)
	}
	for k, v := range resp.Header {
		w.Header()[k] = v
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// tunnel answers 200 and splices the client with target.
func (h *swpHandler) tunnel(w http.ResponseWriter, r *http.Request, target string) {
	host, _, _ := net.SplitHostPort(target)
	if egress.GoogleHost(host) {
		badGateway(w, &egress.RefusedError{Host: host})
		return
	}
	h.p.mu.Lock()
	dial := h.p.dial
	h.p.mu.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	up, err := dial(ctx, "tcp", target)
	cancel()
	if err != nil {
		badGateway(w, err)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		_ = up.Close()
		http.Error(w, "the proxy cannot tunnel over this connection", http.StatusInternalServerError)
		return
	}
	c, buf, err := hj.Hijack()
	if err != nil {
		_ = up.Close()
		return
	}
	_, _ = io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\n")
	if n := buf.Reader.Buffered(); n > 0 {
		b, _ := buf.Reader.Peek(n)
		_, _ = up.Write(b)
	}
	done := make(chan struct{}, 2)
	idle := 300 * time.Second // "Stream idle timeout: 300 seconds"
	cp := func(dst, src net.Conn) {
		b := make([]byte, 32*1024)
		for {
			_ = src.SetReadDeadline(time.Now().Add(idle))
			n, err := src.Read(b)
			if n > 0 {
				if _, werr := dst.Write(b[:n]); werr != nil {
					break
				}
			}
			if err != nil {
				break
			}
		}
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
		done <- struct{}{}
	}
	go cp(up, c)
	go cp(c, up)
	<-done
	<-done
	_ = c.Close()
	_ = up.Close()
}

// deny answers a request the policy does not allow.
func deny(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusForbidden)
	_, _ = io.WriteString(w, "Forbidden\n")
}

func badGateway(w http.ResponseWriter, err error) {
	var ref *egress.RefusedError
	msg := "upstream connect error"
	if errors.As(err, &ref) {
		msg = ref.Error()
	}
	http.Error(w, msg, http.StatusBadGateway)
}

func splitAddr(a string) (string, int) {
	h, p, err := net.SplitHostPort(a)
	if err != nil {
		return a, 0
	}
	n, _ := strconv.Atoi(p)
	return h, n
}

func splitHostPort(hp string, def int) (string, int) {
	h, p, err := net.SplitHostPort(hp)
	if err != nil {
		return strings.Trim(hp, "[]"), def
	}
	n, err := strconv.Atoi(p)
	if err != nil {
		return h, def
	}
	return h, n
}

// matchInput is what the matchers read of one connection or request.
type matchInput struct {
	host             string
	destPort         int
	srcIP            string
	srcPort          int
	req              *http.Request // nil for a CONNECT
	scheme           string
	serviceAccounts  []string
	tags             []string
	identityResolved bool
}

// allowed applies a gateway's policy to in.
func (p *proxyPlane) allowed(gateway string, in matchInput) bool {
	p.s.mu.Lock()
	g, _ := p.s.res[gateway].(*nsvpb.Gateway)
	policy := ""
	if g != nil {
		policy = relRef(g.GetGatewaySecurityPolicy())
	}
	p.s.mu.Unlock()
	p.mu.Lock()
	pols, src := p.policies, p.source
	p.mu.Unlock()
	if policy == "" || pols == nil {
		return false
	}
	rules, ok := pols.GatewayRules(policy)
	if !ok {
		return false
	}
	sort.SliceStable(rules, func(i, j int) bool { return rules[i].Priority < rules[j].Priority })
	if src != nil {
		in.serviceAccounts, in.tags = src(in.srcIP)
	}
	for _, r := range rules {
		if !r.Enabled {
			continue
		}
		if p.ruleMatches(pols, r, in) {
			return r.Allow
		}
	}
	return false
}

// sessionMatches is whether a rule's sessionMatcher matches in.
func (p *proxyPlane) sessionMatches(pols Policies, r GatewayRule, in matchInput) bool {
	return p.eval(pols, r.Session, false, sessionVars(in))
}

func sessionVars(in matchInput) map[string]any {
	return map[string]any{
		"source":      map[string]any{"ip": in.srcIP, "port": int64(in.srcPort)},
		"destination": map[string]any{"port": int64(in.destPort)},
		varHost:       strings.ToLower(in.host),
		varTags:       nonNil(in.tags),
		varSAs:        nonNil(in.serviceAccounts),
	}
}

func (p *proxyPlane) ruleMatches(pols Policies, r GatewayRule, in matchInput) bool {
	if r.Application != "" && in.req == nil {
		return false
	}
	vars := sessionVars(in)
	if !p.eval(pols, r.Session, false, vars) {
		return false
	}
	if r.Application == "" {
		return true
	}
	req := in.req
	headers := map[string]any{}
	for k, v := range req.Header {
		headers[strings.ToLower(k)] = strings.Join(v, ",")
	}
	headers["host"] = req.Host
	vars["request"] = map[string]any{"headers": headers, "method": req.Method, "host": req.Host, "path": req.URL.Path,
		"query": req.URL.RawQuery, "scheme": strings.ToLower(in.scheme)}
	vars[varURL] = strings.ToLower(in.host) + req.URL.Path
	vars[varUA] = req.Header.Get("User-Agent")
	return p.eval(pols, r.Application, true, vars)
}

func nonNil(l []string) []string {
	if l == nil {
		return []string{}
	}
	return l
}

func (p *proxyPlane) eval(_ Policies, expr string, application bool, vars map[string]any) bool {
	if strings.TrimSpace(expr) == "" {
		return !application
	}
	prg, err := p.m.compile(p.urlList, expr, application)
	if err != nil {
		return false
	}
	out, _, err := prg.Eval(vars)
	if err != nil {
		return false
	}
	b, ok := out.Value().(bool)
	return ok && b
}

// urlList reads a URL list through the current policies.
func (p *proxyPlane) urlList(name string) ([]string, bool) {
	p.mu.Lock()
	pols := p.policies
	p.mu.Unlock()
	if pols == nil {
		return nil, false
	}
	return pols.URLList(name)
}
