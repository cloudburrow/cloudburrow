package networkservices

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/egress"
	"github.com/cloudburrow/cloudburrow/internal/swpmatch"
)

// TLS inspection (#2160), as
// https://docs.cloud.google.com/secure-web-proxy/docs/tls-inspection-overview,
// .../enable-tls-inspection, .../policies-and-rules-overview and the
// TlsInspectionPolicy reference describe it (read 2026-11-22):
//
//   - a CONNECT is decrypted when the first enabled rule (by priority)
//     whose sessionMatcher matches it has tlsInspectionEnabled and the
//     gateway security policy names a TLS inspection policy ("Inspection is
//     limited to traffic matching the rule's sessionMatcher"); a rule with
//     an applicationMatcher and no inspection cannot match a tunnel and is
//     passed over, as without inspection;
//   - the client's TLS is terminated with a leaf for its SNI (else the
//     CONNECT host) issued by the policy's CA pool through Certificate
//     Authority Service, one certificate request per host, as the
//     documented direct leaf provisioning does (LeafIssuer); the leaf is
//     cached until an hour before it expires;
//   - each request inside the tunnel is matched by the rules again, now
//     with its request attributes, so applicationMatcher rules apply
//     (request.path, request.method, request.headers, request.url() ...);
//     the first match decides and a denied request gets 403 Forbidden
//     inside the tunnel, as a plain HTTP request does;
//   - an allowed request goes to the CONNECT target over a second TLS
//     connection ("opens a second TLS connection to the destination and
//     re-encrypts traffic"), whose server certificate is checked against
//     the public CA set; the policy's trustConfig, excludePublicCaSet,
//     minTlsVersion, tlsFeatureProfile and customTlsFeatures are kept but,
//     as the reference says of each, "Secure Web Proxy does not yet honor
//     this field". A server certificate that fails the check gets 502 Bad
//     Gateway (Google documents no status for it).

// LeafIssuer issues an interception certificate from a CA pool
// (projects/P/locations/L/caPools/X) for a PEM certificate signing
// request: the leaf first, then its issuers, as PEM.
type LeafIssuer func(ctx context.Context, pool, csrPEM string, lifetime time.Duration) (chainPEM []string, err error)

// TLSInspections is what the proxy reads of a gateway security policy's
// TLS inspection policy, when Policies also implements it
// (internal/swpmatch).
type TLSInspections = swpmatch.TLSInspections

// leafLifetime is an interception leaf's validity. Google documents none.
const leafLifetime = 24 * time.Hour

// UseLeafIssuer sets the CA pool issuer; without one no tunnel is
// decrypted, and a rule that asks for inspection refuses the tunnel.
func (s *Server) UseLeafIssuer(f LeafIssuer) {
	s.proxy.mu.Lock()
	s.proxy.issuer = f
	s.proxy.mu.Unlock()
}

// UseUpstreamRoots replaces the public CA set an inspected connection's
// server is checked against (nil: the system's roots). Tests put a
// local CA here.
func (s *Server) UseUpstreamRoots(roots *x509.CertPool) {
	s.proxy.mu.Lock()
	s.proxy.roots = roots
	s.proxy.mu.Unlock()
}

type leafCache struct {
	mu sync.Mutex
	m  map[string]*tls.Certificate // by pool|host
}

// tunnelDecision is what the rules decide for a CONNECT before any byte
// of it is read.
type tunnelDecision struct {
	allow   bool
	inspect bool
	pool    string
	policy  string
}

// decideTunnel applies the gateway's rules to a CONNECT.
func (p *proxyPlane) decideTunnel(gateway string, in matchInput) tunnelDecision {
	policy := p.gatewayPolicy(gateway)
	p.mu.Lock()
	pols, src, issuer := p.policies, p.source, p.issuer
	p.mu.Unlock()
	if policy == "" || pols == nil {
		return tunnelDecision{}
	}
	rules, ok := pols.GatewayRules(policy)
	if !ok {
		return tunnelDecision{}
	}
	sort.SliceStable(rules, func(i, j int) bool { return rules[i].Priority < rules[j].Priority })
	if src != nil {
		in.serviceAccounts, in.tags = src(in.srcIP)
	}
	pool := ""
	if ti, ok := pols.(TLSInspections); ok {
		pool, _ = ti.TLSInspectionCAPool(policy)
	}
	for _, r := range rules {
		if !r.Enabled || !p.sessionMatches(pols, r, in) {
			continue
		}
		if r.TLSInspection && pool != "" {
			if r.Application == "" && !r.Allow {
				return tunnelDecision{} // denied whatever its requests
			}
			if issuer == nil {
				return tunnelDecision{}
			}
			return tunnelDecision{allow: true, inspect: true, pool: pool, policy: policy}
		}
		if r.Application != "" {
			continue // needs the request, which only inspection sees
		}
		return tunnelDecision{allow: r.Allow}
	}
	return tunnelDecision{}
}

// gatewayPolicy is a gateway's gateway security policy, "" when none.
func (p *proxyPlane) gatewayPolicy(gateway string) string {
	p.s.mu.Lock()
	defer p.s.mu.Unlock()
	if g, ok := p.s.res[gateway].(interface{ GetGatewaySecurityPolicy() string }); ok && g != nil {
		return relRef(g.GetGatewaySecurityPolicy())
	}
	return ""
}

// leaf is the interception certificate for host from pool.
func (p *proxyPlane) leaf(ctx context.Context, pool, host string) (*tls.Certificate, error) {
	key := pool + "|" + host
	p.leaves.mu.Lock()
	if c := p.leaves.m[key]; c != nil && c.Leaf != nil && time.Until(c.Leaf.NotAfter) > time.Hour {
		p.leaves.mu.Unlock()
		return c, nil
	}
	p.leaves.mu.Unlock()
	p.mu.Lock()
	issuer := p.issuer
	p.mu.Unlock()
	if issuer == nil {
		return nil, errors.New("no Certificate Authority Service to issue interception certificates")
	}
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tpl := &x509.CertificateRequest{Subject: pkix.Name{CommonName: host}}
	if ip := net.ParseIP(host); ip != nil {
		tpl.IPAddresses = []net.IP{ip}
	} else {
		tpl.DNSNames = []string{host}
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, tpl, k)
	if err != nil {
		return nil, err
	}
	chain, err := issuer(ctx, pool, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})), leafLifetime)
	if err != nil {
		return nil, err
	}
	c := &tls.Certificate{PrivateKey: k}
	for _, s := range chain {
		rest := []byte(s)
		for {
			var b *pem.Block
			b, rest = pem.Decode(rest)
			if b == nil {
				break
			}
			if b.Type == "CERTIFICATE" {
				c.Certificate = append(c.Certificate, b.Bytes)
			}
		}
	}
	if len(c.Certificate) == 0 {
		return nil, errors.New("the CA pool issued no certificate")
	}
	if c.Leaf, err = x509.ParseCertificate(c.Certificate[0]); err != nil {
		return nil, err
	}
	p.leaves.mu.Lock()
	if p.leaves.m == nil {
		p.leaves.m = map[string]*tls.Certificate{}
	}
	p.leaves.m[key] = c
	p.leaves.mu.Unlock()
	return c, nil
}

// inspect answers 200 to the CONNECT, terminates the client's TLS with a
// leaf from the policy's CA pool, and serves the requests inside it.
func (h *swpHandler) inspect(w http.ResponseWriter, r *http.Request, d tunnelDecision, host string, port int, srcIP string, srcPort int) {
	if egress.GoogleHost(host) {
		badGateway(w, &egress.RefusedError{Host: host})
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "the proxy cannot tunnel over this connection", http.StatusInternalServerError)
		return
	}
	c, buf, err := hj.Hijack()
	if err != nil {
		return
	}
	defer func() { _ = c.Close() }()
	_, _ = io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\n")
	var raw net.Conn = c
	if n := buf.Reader.Buffered(); n > 0 {
		b, _ := buf.Reader.Peek(n)
		raw = &prefixConn{Conn: c, r: io.MultiReader(strings.NewReader(string(b)), c)}
	}
	cfg := &tls.Config{NextProtos: []string{"http/1.1"}, GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
		name := hello.ServerName
		if name == "" {
			name = host
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return h.p.leaf(ctx, d.pool, strings.ToLower(name))
	}}
	tc := tls.Server(raw, cfg)
	_ = tc.SetDeadline(time.Now().Add(30 * time.Second))
	if err := tc.Handshake(); err != nil {
		return
	}
	_ = tc.SetDeadline(time.Time{})
	sni := tc.ConnectionState().ServerName
	if sni == "" {
		sni = host
	}
	h.p.mu.Lock()
	dial, roots := h.p.dial, h.p.roots
	h.p.mu.Unlock()
	target := net.JoinHostPort(host, strconv.Itoa(port))
	up := &http.Transport{Proxy: nil, ForceAttemptHTTP2: false, MaxIdleConnsPerHost: 2, IdleConnTimeout: 300 * time.Second,
		TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: sni, NextProtos: []string{"http/1.1"}},
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dial(ctx, "tcp", target)
		}}
	defer up.CloseIdleConnections()
	br := bufio.NewReader(tc)
	for {
		_ = tc.SetReadDeadline(time.Now().Add(300 * time.Second)) // "Stream idle timeout: 300 seconds"
		req, err := http.ReadRequest(br)
		if err != nil {
			return
		}
		_ = tc.SetReadDeadline(time.Time{})
		req.URL.Scheme, req.URL.Host = "https", req.Host
		if req.URL.Host == "" {
			req.URL.Host = sni
		}
		in := matchInput{host: host, destPort: port, srcIP: srcIP, srcPort: srcPort, req: req, scheme: "https"}
		var resp *http.Response
		if !h.p.allowed(h.gateway, in) {
			resp = textResponse(req, http.StatusForbidden, "Forbidden\n")
		} else {
			out := req.Clone(req.Context())
			out.RequestURI = ""
			for _, k := range hopHeaders {
				out.Header.Del(k)
			}
			if resp, err = up.RoundTrip(out); err != nil {
				resp = textResponse(req, http.StatusBadGateway, "upstream connect error\n")
			} else {
				for _, k := range hopHeaders {
					resp.Header.Del(k)
				}
			}
		}
		resp.Close = req.Close
		err = resp.Write(tc)
		_ = resp.Body.Close()
		if err != nil || req.Close {
			return
		}
	}
}

func textResponse(req *http.Request, code int, body string) *http.Response {
	return &http.Response{StatusCode: code, Status: fmt.Sprintf("%d %s", code, http.StatusText(code)), ProtoMajor: 1, ProtoMinor: 1,
		Header: http.Header{"Content-Type": {"text/plain"}}, Body: io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)), Request: req}
}
