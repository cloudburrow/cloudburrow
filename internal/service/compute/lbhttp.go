package compute

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/http2"

	"github.com/cloudburrow/cloudburrow/internal/egress"
)

// The Application Load Balancers' proxy (#2153): each request through a
// target HTTP or HTTPS proxy is routed by its URL map (lbMatch), with the
// route action (URL rewrite, weighted backend services, timeout, retries,
// fault injection, mirroring, CORS) and every level's header actions, to
// an endpoint of the chosen backend service (lbbalance.go) or to the
// chosen backend bucket's objects, as
// https://docs.cloud.google.com/load-balancing/docs/https and
// .../https/traffic-management-global describe it (read 2026-11-22). The
// proxy adds X-Forwarded-For (the client, then the load balancer's
// address), X-Forwarded-Proto and Via: 1.1 google, as Google's does, and
// the backend service's custom request and response headers.

type lbHTTP struct {
	s    *Server
	rule string
	port int
	tls  bool
}

// lbRequestState travels with one request through the transport.
type lbRequestState struct {
	set      lbBackendSet
	key      string
	cookie   string // the affinity cookie to set, "name=value"
	cookieTT float64
	ep       lbEndpoint
	tried    map[string]bool
	retry    obj
	timeout  time.Duration
	scheme   string
	external bool
}

type lbStateKey struct{}

func (h *lbHTTP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s := h.s
	s.mu.Lock()
	rule := deepCopy(s.res[h.rule]).(obj)
	proxy := s.res[str(rule, "target")]
	mapRel := str(proxy, "urlMap")
	m := deepCopy(s.res[mapRel]).(obj)
	s.mu.Unlock()
	if m == nil {
		lbError(w, http.StatusNotFound, "the target proxy has no URL map")
		return
	}
	scheme := "http"
	if h.tls {
		scheme = "https"
		// Frontend mutual TLS's check, then the authorization policies
		// (#2160, lbmtls.go).
		r = s.withClientCert(r, proxy)
	}
	if !h.authorize(w, r, scheme) {
		return
	}
	host := r.Host
	if s.iapAuthenticate(w, r, scheme) {
		// IAP's sign-in coming back to the app (#2160).
		return
	}
	d := lbMatch(m, host, r.URL.Path, r.Header, r.URL.Query())
	if d.action != nil {
		if fi := objOf(d.action["faultInjectionPolicy"]); fi != nil {
			if dl := objOf(fi["delay"]); dl != nil && rand.Float64()*100 < numOr(dl, "percentage", 0) {
				time.Sleep(durationOf(objOf(dl["fixedDelay"])))
			}
			if ab := objOf(fi["abort"]); ab != nil && rand.Float64()*100 < numOr(ab, "percentage", 0) {
				lbError(w, int(numOr(ab, "httpStatus", 503)), "fault filter abort")
				return
			}
		}
		if cors := objOf(d.action["corsPolicy"]); cors != nil && cors["disabled"] != true && corsHandled(w, r, cors) {
			return
		}
	}
	if d.redirect != nil {
		loc, code := d.redirectURL(scheme, host, r.URL.Path, r.URL.RawQuery)
		applyResponseHeaders(w.Header(), d.headers, "")
		w.Header().Set("Location", loc)
		w.WriteHeader(code)
		return
	}
	svc := d.service
	var weightedHA obj
	if len(d.weighted) > 0 {
		total := 0.0
		for _, wb := range d.weighted {
			total += numOr(wb, "weight", 0)
		}
		pick := rand.Float64() * total
		for _, wb := range d.weighted {
			svc, weightedHA = str(wb, "backendService"), objOf(wb["headerAction"])
			if pick -= numOr(wb, "weight", 0); pick < 0 {
				break
			}
		}
	}
	headers := d.headers
	if weightedHA != nil {
		headers = append(headers, weightedHA)
	}
	newHost, newPath := d.rewritten(host, r.URL.Path)
	s.mu.Lock()
	so := deepCopy(s.res[svc]).(obj)
	s.mu.Unlock()
	if so == nil {
		lbError(w, http.StatusNotFound, "no backend")
		return
	}
	// Cloud Armor's edge security policy, then Cloud CDN's cache, then
	// the backend security policy and IAP on a miss (#2160).
	areq := newArmorReq(r, scheme)
	extra, ok := s.armorCheck(w, areq, str(so, "edgeSecurityPolicy"))
	if !ok {
		return
	}
	headers = append(headers, extra...)
	cw, done := h.cdnFront(w, r, so, mapRel, scheme)
	if done {
		return
	}
	defer cw.finish()
	w = cw
	if str(so, "kind") == "compute#backendBucket" {
		h.serveBucket(w, r, so, newPath, headers)
		return
	}
	extra, ok = s.armorCheck(w, areq, str(so, "securityPolicy"))
	if !ok {
		return
	}
	headers = append(headers, extra...)
	if !s.iapCheck(w, r, so, scheme) {
		return
	}
	s.mu.Lock()
	set, _ := s.backendSetLocked(svc)
	s.mu.Unlock()
	if d.action != nil {
		if mp := objOf(d.action["requestMirrorPolicy"]); mp != nil {
			h.mirror(r, str(mp, "backendService"), newHost, newPath)
		}
	}
	st := &lbRequestState{set: set, tried: map[string]bool{}, scheme: scheme,
		external: strings.HasPrefix(str(rule, "loadBalancingScheme"), "EXTERNAL")}
	st.timeout = time.Duration(numOr(so, "timeoutSec", 30)) * time.Second
	if d.action != nil {
		if t := objOf(d.action["timeout"]); t != nil {
			st.timeout = durationOf(t)
		}
		st.retry = objOf(d.action["retryPolicy"])
	}
	setAffinity(st, so, r)
	clientIP, _, _ := net.SplitHostPort(r.RemoteAddr)
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme, pr.Out.URL.Host = "http", "backend.invalid"
			pr.Out.URL.Path, pr.Out.URL.RawPath = newPath, ""
			pr.Out.Host = newHost
			pr.Out.Header["X-Forwarded-For"] = []string{strings.TrimPrefix(strings.Join(append(r.Header.Values("X-Forwarded-For"), clientIP), ", ")+", "+str(rule, "IPAddress"), ", ")}
			pr.Out.Header.Set("X-Forwarded-Proto", scheme)
			pr.Out.Header.Add("Via", "1.1 google")
			if pr.Out.Header.Get("X-Cloud-Trace-Context") == "" {
				pr.Out.Header.Set("X-Cloud-Trace-Context", randHex(16)+"/"+strconv.FormatUint(rand.Uint64()>>1, 10))
			}
			for _, hdr := range strs(so, "customRequestHeaders") {
				if i := strings.Index(hdr, ":"); i > 0 {
					pr.Out.Header.Add(strings.TrimSpace(hdr[:i]), headerVars(strings.TrimSpace(hdr[i+1:]), r, clientIP))
				}
			}
			applyRequestHeaders(pr.Out.Header, headers, r, clientIP)
			*pr.Out = *pr.Out.WithContext(context.WithValue(pr.Out.Context(), lbStateKey{}, st))
		},
		Transport: &lbTransport{s: s},
		ModifyResponse: func(resp *http.Response) error {
			for _, hdr := range strs(so, "customResponseHeaders") {
				if i := strings.Index(hdr, ":"); i > 0 {
					resp.Header.Add(strings.TrimSpace(hdr[:i]), headerVars(strings.TrimSpace(hdr[i+1:]), r, clientIP))
				}
			}
			applyResponseHeaders(resp.Header, headers, "")
			if st.cookie != "" {
				c := &http.Cookie{Name: st.cookie[:strings.Index(st.cookie, "=")], Value: st.cookie[strings.Index(st.cookie, "=")+1:], Path: "/", HttpOnly: true}
				if st.cookieTT > 0 {
					c.MaxAge = int(st.cookieTT)
				}
				resp.Header.Add("Set-Cookie", c.String())
			}
			resp.Header.Add("Via", "1.1 google")
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			switch {
			case errors.Is(err, errNoHealthy):
				lbError(w, http.StatusServiceUnavailable, "no healthy upstream")
			case errors.Is(err, context.DeadlineExceeded):
				lbError(w, http.StatusGatewayTimeout, "upstream request timeout")
			default:
				lbError(w, http.StatusBadGateway, "upstream connect error or disconnect/reset before headers: "+err.Error())
			}
		},
	}
	ctx, cancel := context.WithTimeout(r.Context(), st.timeout)
	defer cancel()
	rp.ServeHTTP(w, r.WithContext(ctx))
}

var errNoHealthy = errors.New("no healthy upstream")

// lbError writes an error response as Google's front ends answer one.
func lbError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "text/plain")
	w.Header().Set("Via", "1.1 google")
	w.WriteHeader(code)
	_, _ = io.WriteString(w, msg)
}

func durationOf(d obj) time.Duration {
	secs, _ := strconv.ParseInt(fmt.Sprint(d["seconds"]), 10, 64)
	if f, ok := d["seconds"].(float64); ok {
		secs = int64(f)
	}
	return time.Duration(secs)*time.Second + time.Duration(numOr(d, "nanos", 0))
}

// setAffinity sets a request's affinity key and the cookie to set.
func setAffinity(st *lbRequestState, bs obj, r *http.Request) {
	ip, port, _ := net.SplitHostPort(r.RemoteAddr)
	ch := objOf(bs["consistentHash"])
	switch str(bs, "sessionAffinity") {
	case "CLIENT_IP", "CLIENT_IP_PROTO", "CLIENT_IP_NO_DESTINATION":
		st.key = hashKey(ip)
	case "CLIENT_IP_PORT_PROTO":
		st.key = hashKey(ip, port)
	case "GENERATED_COOKIE":
		if c, err := r.Cookie("GCLB"); err == nil && c.Value != "" {
			st.key = c.Value
		} else {
			v := randHex(8)
			st.key, st.cookie, st.cookieTT = v, "GCLB="+v, numOr(bs, "affinityCookieTtlSec", 0)
		}
	case "HEADER_FIELD":
		st.key = hashKey(r.Header.Get(str(ch, "httpHeaderName")))
	case "HTTP_COOKIE", "STRONG_COOKIE_AFFINITY":
		hc := objOf(ch["httpCookie"])
		if str(bs, "sessionAffinity") == "STRONG_COOKIE_AFFINITY" {
			hc = objOf(bs["strongSessionAffinityCookie"])
		}
		name := str(hc, "name")
		if name == "" {
			name = "GCILB"
		}
		if c, err := r.Cookie(name); err == nil && c.Value != "" {
			st.key = c.Value
		} else {
			v := randHex(8)
			st.key, st.cookie = v, name+"="+v
			if ttl := objOf(hc["ttl"]); ttl != nil {
				st.cookieTT = durationOf(ttl).Seconds()
			}
		}
	}
	if st.key == "" && (str(bs, "localityLbPolicy") == "RING_HASH" || str(bs, "localityLbPolicy") == "MAGLEV") {
		st.key = hashKey(ip)
	}
}

// headerVars fills a custom header's variables
// (load-balancing/docs/https/custom-headers, "Variables").
func headerVars(v string, r *http.Request, clientIP string) string {
	repl := strings.NewReplacer("{client_ip_address}", clientIP, "{client_region}", "", "{client_region_subdivision}", "",
		"{client_city}", "", "{client_city_lat_long}", "", "{client_rtt_msec}", "0", "{origin_request_header}", r.Header.Get("Origin"),
		"{tls_sni_hostname}", sniOf(r), "{tls_version}", tlsVersionOf(r), "{tls_cipher_suite}", cipherOf(r))
	if strings.Contains(v, "{client_cert_") {
		v = strings.NewReplacer(mtlsVars(r)...).Replace(v)
	}
	return repl.Replace(v)
}

func sniOf(r *http.Request) string {
	if r.TLS != nil {
		return r.TLS.ServerName
	}
	return ""
}

func tlsVersionOf(r *http.Request) string {
	if r.TLS == nil {
		return ""
	}
	return tls.VersionName(r.TLS.Version)
}

func cipherOf(r *http.Request) string {
	if r.TLS == nil {
		return ""
	}
	return tls.CipherSuiteName(r.TLS.CipherSuite)
}

func applyRequestHeaders(h http.Header, actions []obj, r *http.Request, clientIP string) {
	for _, a := range actions {
		for _, n := range strs(a, "requestHeadersToRemove") {
			h.Del(n)
		}
		for _, e := range listOf(a["requestHeadersToAdd"]) {
			ad := objOf(e)
			v := headerVars(str(ad, "headerValue"), r, clientIP)
			if ad["replace"] == true {
				h.Set(str(ad, "headerName"), v)
			} else {
				h.Add(str(ad, "headerName"), v)
			}
		}
	}
}

func applyResponseHeaders(h http.Header, actions []obj, _ string) {
	for _, a := range actions {
		for _, n := range strs(a, "responseHeadersToRemove") {
			h.Del(n)
		}
		for _, e := range listOf(a["responseHeadersToAdd"]) {
			ad := objOf(e)
			if ad["replace"] == true {
				h.Set(str(ad, "headerName"), str(ad, "headerValue"))
			} else {
				h.Add(str(ad, "headerName"), str(ad, "headerValue"))
			}
		}
	}
}

// corsHandled answers a CORS preflight, or adds the CORS headers to w; it
// reports whether the request is answered.
func corsHandled(w http.ResponseWriter, r *http.Request, c obj) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return false
	}
	ok := false
	for _, o := range strs(c, "allowOrigins") {
		if o == "*" || o == origin {
			ok = true
		}
	}
	for _, re := range strs(c, "allowOriginRegexes") {
		if m, _ := regexpMatch(re, origin); m {
			ok = true
		}
	}
	if !ok {
		return false
	}
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", origin)
	if c["allowCredentials"] == true {
		h.Set("Access-Control-Allow-Credentials", "true")
	}
	if l := strs(c, "exposeHeaders"); len(l) > 0 {
		h.Set("Access-Control-Expose-Headers", strings.Join(l, ","))
	}
	if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
		if l := strs(c, "allowMethods"); len(l) > 0 {
			h.Set("Access-Control-Allow-Methods", strings.Join(l, ","))
		}
		if l := strs(c, "allowHeaders"); len(l) > 0 {
			h.Set("Access-Control-Allow-Headers", strings.Join(l, ","))
		}
		if v, ok := num(c, "maxAge"); ok {
			h.Set("Access-Control-Max-Age", strconv.Itoa(int(v)))
		}
		w.WriteHeader(http.StatusOK)
		return true
	}
	return false
}

// lbTransport sends a request to an endpoint of its backend service,
// trying another on the retry policy's conditions.
type lbTransport struct{ s *Server }

func (t *lbTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	st, _ := req.Context().Value(lbStateKey{}).(*lbRequestState)
	if st == nil {
		return nil, errors.New("no backend")
	}
	var body []byte
	if req.Body != nil && req.Body != http.NoBody {
		b, err := io.ReadAll(io.LimitReader(req.Body, 32<<20))
		if err != nil {
			return nil, err
		}
		body = b
	}
	attempts := 1
	var conds []string
	if st.retry != nil {
		attempts += int(numOr(st.retry, "numRetries", 1))
		conds = strs(st.retry, "retryConditions")
	}
	var lastErr error
	for i := 0; i < attempts; i++ {
		ep, ok := t.s.lb.bal.pick(st.set, st.key, st.tried, false)
		if !ok {
			if lastErr != nil {
				return nil, lastErr
			}
			return nil, errNoHealthy
		}
		st.tried[ep.id] = true
		resp, err := t.send(req, body, st, ep)
		t.s.lb.bal.done(ep)
		if err == nil {
			t.s.lb.bal.recordORCA(ep, resp)
		}
		retry := false
		switch {
		case err != nil:
			retry = hasAny(conds, "connect-failure", "gateway-error", "5xx", "reset", "refused-stream")
		case resp.StatusCode >= 500:
			retry = hasAny(conds, "5xx") || (resp.StatusCode >= 502 && resp.StatusCode <= 504 && hasAny(conds, "gateway-error"))
		case resp.StatusCode == 409 || resp.StatusCode == 404:
			retry = hasAny(conds, "retriable-4xx") && resp.StatusCode == 409
		}
		if !retry || i == attempts-1 {
			if err != nil {
				return nil, err
			}
			return resp, nil
		}
		if resp != nil {
			_ = resp.Body.Close()
		}
		lastErr = err
		if len(st.tried) >= endpointCount(st.set) {
			st.tried = map[string]bool{}
		}
	}
	return nil, lastErr
}

func hasAny(l []string, want ...string) bool {
	for _, w := range want {
		if hasStr(l, w) {
			return true
		}
	}
	return false
}

func endpointCount(set lbBackendSet) int {
	n := 0
	for _, g := range set.groups {
		n += len(g.healthy)
	}
	return n
}

// send sends one attempt to ep.
func (t *lbTransport) send(req *http.Request, body []byte, st *lbRequestState, ep lbEndpoint) (*http.Response, error) {
	out := req.Clone(req.Context())
	if body != nil {
		out.Body, out.ContentLength = io.NopCloser(bytes.NewReader(body)), int64(len(body))
	}
	proto := st.set.protocol
	tlsBackend := proto == "HTTPS" || proto == "HTTP2"
	var rt http.RoundTripper
	switch ep.kind {
	case "serverless":
		ingress, host, err := t.s.serverlessTarget(req.Context(), ep.neg, req.Host, req.URL.Path, st.external)
		if err != nil {
			return nil, err
		}
		out.URL.Scheme, out.URL.Host, out.Host = "http", ingress, host
		rt = plainTransport
	case "internet":
		host := ep.fqdn
		if host == "" {
			host = ep.ip
		}
		out.URL.Host = net.JoinHostPort(host, strconv.Itoa(ep.port))
		out.URL.Scheme = "http"
		if tlsBackend || ep.port == 443 {
			out.URL.Scheme = "https"
		}
		if out.Host == req.Host && ep.fqdn != "" {
			out.Host = ep.fqdn
		}
		// Behind the local-only guard: an internet endpoint never reaches
		// Google (#2015).
		rt = egress.Transport(http.DefaultTransport)
	case "psc":
		addr, err := t.s.pscTargetAddr(ep.neg)
		if err != nil {
			return nil, err
		}
		u, _ := url.Parse(addr)
		if u != nil && u.Scheme != "" {
			out.URL.Scheme, out.URL.Host = u.Scheme, u.Host
		} else {
			out.URL.Scheme, out.URL.Host = "http", addr
		}
		rt = plainTransport
	default:
		addr, err := t.s.dialAddr(req.Context(), ep, ep.port)
		if err != nil {
			return nil, err
		}
		out.URL.Host = addr
		out.URL.Scheme = "http"
		rt = plainTransport
		switch proto {
		case "HTTPS":
			out.URL.Scheme, rt = "https", tlsTransport
		case "HTTP2":
			out.URL.Scheme, rt = "https", h2Transport
		case "H2C", "GRPC":
			rt = h2cTransport
		}
		if tlsBackend {
			// Backend authenticated TLS and mTLS (lbbackendtls.go).
			if bt, ok := t.s.backendTLSTransport(st.set, proto == "HTTP2"); ok {
				rt = bt
			}
		}
	}
	return rt.RoundTrip(out)
}

var (
	plainTransport = &http.Transport{Proxy: nil, MaxIdleConnsPerHost: 16, IdleConnTimeout: 60 * time.Second}
	tlsTransport   = &http.Transport{Proxy: nil, TLSClientConfig: &tls.Config{InsecureSkipVerify: true}} //nolint:gosec // Google does not verify backends' certificates
	h2Transport    = &http.Transport{Proxy: nil, ForceAttemptHTTP2: true,
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true, NextProtos: []string{"h2"}}} //nolint:gosec // as above
	h2cTransport = &http2.Transport{AllowHTTP: true, DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}}
)

// mirror sends a copy of the request to the mirror backend service and
// drops its response, as requestMirrorPolicy does.
func (h *lbHTTP) mirror(r *http.Request, svc, host, path string) {
	h.s.mu.Lock()
	set, ok := h.s.backendSetLocked(svc)
	h.s.mu.Unlock()
	if !ok {
		return
	}
	out := r.Clone(context.Background())
	out.Body, out.ContentLength = http.NoBody, 0
	out.RequestURI, out.Host = "", host+"-shadow"
	out.URL = &url.URL{Scheme: "http", Host: "backend.invalid", Path: path, RawQuery: r.URL.RawQuery}
	st := &lbRequestState{set: set, tried: map[string]bool{}}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		resp, err := (&lbTransport{s: h.s}).RoundTrip(out.WithContext(context.WithValue(ctx, lbStateKey{}, st)))
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
	}()
}

// serveBucket serves a backend bucket's object at path
// (load-balancing/docs/https/ext-load-balancer-backend-buckets): GET and
// HEAD of the bucket's objects, the website configuration's main page for
// a directory and its not-found page for a missing object, and 403 when
// the bucket's objects are not public (allUsers as a viewer), as Google's
// load balancer reads a bucket only that way.
func (h *lbHTTP) serveBucket(w http.ResponseWriter, r *http.Request, bb obj, path string, headers []obj) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		lbError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	base := h.s.localAPI("storage")
	if base == "" {
		lbError(w, http.StatusBadGateway, "Cloud Storage is not served on this instance")
		return
	}
	bucket := str(bb, "bucketName")
	meta, err := h.s.lb.cache.json(r.Context(), base+"/storage/v1/b/"+url.PathEscape(bucket))
	if err != nil {
		writeXMLError(w, http.StatusNotFound, "NoSuchBucket", "The specified bucket does not exist.")
		return
	}
	pol, _ := h.s.lb.cache.json(r.Context(), base+"/storage/v1/b/"+url.PathEscape(bucket)+"/iam")
	if !publicBucket(pol) {
		writeXMLError(w, http.StatusForbidden, "AccessDenied", "Access denied.")
		return
	}
	web := objOf(meta["website"])
	key := strings.TrimPrefix(path, "/")
	if (key == "" || strings.HasSuffix(key, "/")) && str(web, "mainPageSuffix") != "" {
		key += str(web, "mainPageSuffix")
	}
	status := http.StatusOK
	resp, err := h.fetchObject(r, base, bucket, key)
	if err == nil && resp.StatusCode == http.StatusNotFound && str(web, "notFoundPage") != "" {
		_ = resp.Body.Close()
		status = http.StatusNotFound
		resp, err = h.fetchObject(r, base, bucket, str(web, "notFoundPage"))
	}
	if err != nil {
		lbError(w, http.StatusBadGateway, err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		writeXMLError(w, http.StatusNotFound, "NoSuchKey", "The specified key does not exist.")
		return
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		lbError(w, resp.StatusCode, "the bucket's object could not be read")
		return
	}
	if resp.StatusCode == http.StatusPartialContent {
		status = resp.StatusCode
	}
	for _, k := range []string{"Content-Type", "Content-Length", "ETag", "Cache-Control", "Last-Modified", "Content-Range", "Content-Encoding"} {
		if v := resp.Header.Get(k); v != "" {
			w.Header().Set(k, v)
		}
	}
	for _, hdr := range strs(bb, "customResponseHeaders") {
		if i := strings.Index(hdr, ":"); i > 0 {
			w.Header().Add(strings.TrimSpace(hdr[:i]), strings.TrimSpace(hdr[i+1:]))
		}
	}
	applyResponseHeaders(w.Header(), headers, "")
	w.Header().Set("Via", "1.1 google")
	w.WriteHeader(status)
	if r.Method == http.MethodGet {
		_, _ = io.Copy(w, resp.Body)
	}
}

func (h *lbHTTP) fetchObject(r *http.Request, base, bucket, key string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet,
		base+"/storage/v1/b/"+url.PathEscape(bucket)+"/o/"+url.PathEscape(key)+"?alt=media", nil)
	if err != nil {
		return nil, err
	}
	if rg := r.Header.Get("Range"); rg != "" {
		req.Header.Set("Range", rg)
	}
	return plainTransport.RoundTrip(req)
}

func publicBucket(pol obj) bool {
	for _, e := range listOf(pol["bindings"]) {
		b := objOf(e)
		role := str(b, "role")
		if role != "roles/storage.objectViewer" && role != "roles/storage.legacyObjectReader" && role != "roles/storage.admin" &&
			role != "roles/storage.objectAdmin" {
			continue
		}
		if hasStr(strs(b, "members"), "allUsers") {
			return true
		}
	}
	return false
}

func writeXMLError(w http.ResponseWriter, code int, c, msg string) {
	w.Header().Set("Content-Type", "application/xml; charset=UTF-8")
	w.WriteHeader(code)
	_, _ = fmt.Fprintf(w, "<?xml version='1.0' encoding='UTF-8'?><Error><Code>%s</Code><Message>%s</Message></Error>", c, msg)
}

// localAPI is a local API's base URL.
func (s *Server) localAPI(api string) string {
	if s.opts.LB != nil {
		if b := s.opts.LB.LocalAPI(api); b != "" {
			return strings.TrimSuffix(b, "/")
		}
	}
	if f := egress.Local(); f != nil {
		return strings.TrimSuffix(f(api), "/")
	}
	return ""
}

// lbCache keeps what the data plane reads from other APIs for a few
// seconds.
type lbCache struct {
	mu sync.Mutex
	m  map[string]lbCached
}

type lbCached struct {
	at  time.Time
	v   obj
	err error
}

// json GETs a JSON document, cached for 3 seconds.
func (c *lbCache) json(ctx context.Context, u string) (obj, error) {
	c.mu.Lock()
	if c.m == nil {
		c.m = map[string]lbCached{}
	}
	if e, ok := c.m[u]; ok && time.Since(e.at) < 3*time.Second {
		c.mu.Unlock()
		return e.v, e.err
	}
	c.mu.Unlock()
	v, err := getJSON(ctx, u)
	c.mu.Lock()
	c.m[u] = lbCached{at: time.Now(), v: v, err: err}
	c.mu.Unlock()
	return v, err
}

func getJSON(ctx context.Context, u string) (obj, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := plainTransport.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: HTTP %d", u, resp.StatusCode)
	}
	var v obj
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

// serverlessTarget is where a serverless network endpoint group sends a
// request: the cluster's ingress and the Host of the Cloud Run service
// (or function) it names, or its URL mask finds in the request
// (load-balancing/docs/negs/serverless-neg-concepts, "URL masks"). A
// service whose ingress is internal only takes no external load balancer's
// traffic, as Cloud Run's ingress settings say, and answers 404.
func (s *Server) serverlessTarget(ctx context.Context, neg obj, host, path string, external bool) (string, string, error) {
	ingress := ""
	if s.opts.LB != nil {
		ingress = s.opts.LB.Ingress()
	}
	if ingress == "" {
		return "", "", errors.New("no Cloud Run ingress is published on this machine")
	}
	parts := strings.Split(str(neg, "selfLink"), "/")
	project, region := parts[1], parts[3]
	if cf := objOf(neg["cloudFunction"]); cf != nil {
		fn := str(cf, "function")
		if fn == "" {
			fn, _ = urlMaskMatch(str(cf, "urlMask"), host, path, "function")
		}
		base := s.localAPI("cloudfunctions")
		f, err := s.lb.cache.json(ctx, base+"/v2/projects/"+project+"/locations/"+region+"/functions/"+url.PathEscape(fn))
		if err != nil {
			return "", "", fmt.Errorf("function %s: %w", fn, err)
		}
		u, _ := url.Parse(str(objOf(f["serviceConfig"]), "uri"))
		if u == nil || u.Host == "" {
			u, _ = url.Parse(str(f, "url"))
		}
		if u == nil || u.Host == "" {
			return "", "", fmt.Errorf("function %s has no URL yet", fn)
		}
		return ingress, u.Host, nil
	}
	cr := objOf(neg["cloudRun"])
	svc, tag := str(cr, "service"), str(cr, "tag")
	if svc == "" {
		svc, tag = urlMaskMatch(str(cr, "urlMask"), host, path, "service")
	}
	if svc == "" {
		return "", "", errors.New("the request matches no Cloud Run service of the URL mask")
	}
	base := s.localAPI("run")
	o, err := s.lb.cache.json(ctx, base+"/v2/projects/"+project+"/locations/"+region+"/services/"+url.PathEscape(svc))
	if err != nil {
		return "", "", fmt.Errorf("Cloud Run service %s: %w", svc, err)
	}
	if external && str(o, "ingress") == "INGRESS_TRAFFIC_INTERNAL_ONLY" {
		return "", "", errors.New("the Cloud Run service's ingress is internal only")
	}
	uri := str(o, "uri")
	if tag != "" {
		uri = ""
		for _, e := range listOf(o["trafficStatuses"]) {
			if ts := objOf(e); str(ts, "tag") == tag {
				uri = str(ts, "uri")
			}
		}
	}
	u, _ := url.Parse(uri)
	if u == nil || u.Host == "" {
		return "", "", fmt.Errorf("Cloud Run service %s has no URL for tag %q", svc, tag)
	}
	return ingress, u.Host, nil
}

// urlMaskMatch reads <service> (or <function>) and <tag> out of a request
// by a URL mask such as "<service>.example.com" or "example.com/<service>".
func urlMaskMatch(mask, host, path, name string) (string, string) {
	if mask == "" {
		return "", ""
	}
	if h, _, err := splitHostPortLB(host); err == nil {
		host = h
	}
	target := host + path
	if strings.HasPrefix(mask, "/") {
		target = path
	}
	re := "^" + strings.NewReplacer("<"+name+">", "(?P<s>[a-z0-9-]+)", "<tag>", "(?P<t>[a-z0-9-]+)").Replace(regexpQuoteMask(mask))
	m, groups := regexpFind(re, target)
	if m == nil {
		return "", ""
	}
	out := map[string]string{}
	for i, g := range groups {
		if g != "" && i < len(m) {
			out[g] = m[i]
		}
	}
	return out["s"], out["t"]
}

// pscTargetAddr is where a Private Service Connect network endpoint group
// sends: the listener of its service attachment's producer forwarding
// rule, or the local API of a Google API host.
func (s *Server) pscTargetAddr(neg obj) (string, error) {
	t := str(neg, "pscTargetService")
	if strings.HasSuffix(t, ".googleapis.com") {
		return egress.Resolve("https://" + t)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	att := s.res[t]
	if att == nil {
		return "", errors.New("the service attachment is gone")
	}
	if pscStatus(att, strings.Split(str(neg, "selfLink"), "/")[1]) != "ACCEPTED" {
		return "", errors.New("the service attachment has not accepted the connection")
	}
	rule := s.res[Rel(str(att, "targetService"), "")]
	if a := str(rule, "cloudburrowAddress"); a != "" {
		return a, nil
	}
	return "", errors.New("the service attachment's producer forwarding rule is not served on this machine")
}

func regexpMatch(re, s string) (bool, error) { return regexp.MatchString(re, s) }

// regexpFind is the submatches of re in s and the groups' names.
func regexpFind(re, s string) ([]string, []string) {
	r, err := regexp.Compile(re)
	if err != nil {
		return nil, nil
	}
	return r.FindStringSubmatch(s), r.SubexpNames()
}

// regexpQuoteMask quotes a URL mask but its <placeholders>.
func regexpQuoteMask(mask string) string {
	q := regexp.QuoteMeta(mask)
	return q
}
