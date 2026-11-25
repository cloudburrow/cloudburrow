package console

// The proxy of Vertex AI Workbench instances' JupyterLab (#2198), as the
// terminal drawer (#781) is a real shell behind the console. Google serves
// an instance's JupyterLab at its proxy_uri,
// HASH-dot-REGION.notebooks.googleusercontent.com, behind Google sign-in;
// here proxy_uri is HASH-dot-REGION.notebooks.localhost:PORT, this
// console's port, and the console serves each such host by proxying it
// whole, WebSockets (kernel channels, terminals) included, to the
// instance's Jupyter Server, adding the server's token, which the browser
// never sees.
//
// A separate host is a separate origin, as Google's googleusercontent.com
// is: JupyterLab's pages and a notebook's outputs cannot call this
// console's API, which refuses any other origin (sameOriginOnly).
//
// The console session: the console's page asks for JupyterLab (Open
// JupyterLab, POST /api/workbench/open, same-origin and from this machine
// alone, as the terminal is), and is answered with a one-time code on the
// instance's proxy host, good for a minute; following it sets a cookie for
// that host alone, which every later request must carry. A request
// without one is refused with a page that says to open JupyterLab from the
// console. Beyond the cookie, as for the terminal, the peer must be on
// this machine, and a request another site starts is refused unless it is
// a top-level navigation (which can read nothing back): a page on another
// instance's proxy host is same-site, so it could otherwise drive this
// one's kernels with the cookie.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"html"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"
)

// JupyterLabs are the Workbench instances' JupyterLabs the console proxies.
type JupyterLabs interface {
	// ProxyURI is the instance's proxy_uri (host:port), or why it has none.
	ProxyURI(ctx context.Context, name string) (string, error)
	// Endpoint is the loopback address and token of the JupyterLab whose
	// proxy host's first label is label, and the instance's name.
	Endpoint(label string) (addr, token, name string, err error)
	// Authorize checks a bearer token's caller may use the JupyterLab
	// whose proxy host's first label is label (a Workbench instance's
	// notebooks.instances.use), nil when it may.
	Authorize(ctx context.Context, label, bearer string) error
}

// JupyterProxyDomain is the domain the instances' proxy hosts are under.
const JupyterProxyDomain = "notebooks.localhost"

const (
	jupyterSessionPath = "/cloudburrow/session"
	jupyterCookie      = "cloudburrow-jupyterlab"
	jupyterCodeTTL     = time.Minute
	jupyterSessionTTL  = 24 * time.Hour
)

type jupyterGrant struct {
	label   string
	expires time.Time
}

type jupyterState struct {
	mu       sync.Mutex
	labs     JupyterLabs
	codes    map[string]jupyterGrant
	sessions map[string]jupyterGrant
}

// SetJupyterLabs serves the Workbench instances' proxy hosts.
func (s *Server) SetJupyterLabs(j JupyterLabs) {
	s.jupyter.mu.Lock()
	s.jupyter.labs = j
	s.jupyter.mu.Unlock()
}

func newSecret() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// jupyterLabel is the proxy host's first label, "" for a host that is not
// one.
func jupyterLabel(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	label, rest, ok := strings.Cut(host, ".")
	if !ok || rest != JupyterProxyDomain || !strings.Contains(label, "-dot-") {
		return ""
	}
	return label
}

func loopbackPeer(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// handleWorkbenchOpen is Open JupyterLab: a one-time link to the
// instance's JupyterLab.
func (s *Server) handleWorkbenchOpen(w http.ResponseWriter, r *http.Request) {
	if !loopbackPeer(r) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "JupyterLab is served only to this machine"})
		return
	}
	s.jupyter.mu.Lock()
	labs := s.jupyter.labs
	s.jupyter.mu.Unlock()
	if labs == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "this console serves no Workbench instances"})
		return
	}
	name := r.URL.Query().Get("name")
	uri, err := labs.ProxyURI(r.Context(), name)
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	label := jupyterLabel(uri)
	if label == "" {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "the instance's proxy_uri " + uri + " is not served by this console"})
		return
	}
	code := newSecret()
	now := time.Now()
	s.jupyter.mu.Lock()
	if s.jupyter.codes == nil {
		s.jupyter.codes = map[string]jupyterGrant{}
	}
	for k, g := range s.jupyter.codes {
		if now.After(g.expires) {
			delete(s.jupyter.codes, k)
		}
	}
	s.jupyter.codes[code] = jupyterGrant{label: label, expires: now.Add(jupyterCodeTTL)}
	s.jupyter.mu.Unlock()
	next := r.URL.Query().Get("next")
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		next = "/lab"
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"url": "http://" + uri + jupyterSessionPath + "?code=" + code + "&next=" + url.QueryEscape(next),
	})
}

func jupyterRefuse(w http.ResponseWriter, status int, title, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`<!doctype html><html lang="en"><head><meta charset="utf-8"><title>` + html.EscapeString(title) +
		`</title></head><body style="font-family:Roboto,Arial,sans-serif;margin:2rem;color:#202124"><main><h1 style="font-size:1.375rem;font-weight:400">` +
		html.EscapeString(title) + `</h1><p>` + html.EscapeString(msg) + `</p></main></body></html>`))
}

// serveJupyter serves an instance's proxy host.
func (s *Server) serveJupyter(w http.ResponseWriter, r *http.Request, label string) {
	if !loopbackPeer(r) {
		jupyterRefuse(w, http.StatusForbidden, "Forbidden", "JupyterLab is served only to this machine.")
		return
	}
	// Another site's page may navigate here, and read nothing back; it may
	// not send anything else.
	switch r.Header.Get("Sec-Fetch-Site") {
	case "same-site", "cross-site":
		if r.Method != http.MethodGet || r.Header.Get("Sec-Fetch-Mode") != "navigate" {
			jupyterRefuse(w, http.StatusForbidden, "Forbidden", "A request from another site to this instance's JupyterLab is refused.")
			return
		}
	}
	if o := r.Header.Get("Origin"); o != "" && !strings.EqualFold(o, "http://"+r.Host) {
		jupyterRefuse(w, http.StatusForbidden, "Forbidden", "A request from origin "+o+" to this instance's JupyterLab is refused.")
		return
	}
	now := time.Now()
	if r.URL.Path == jupyterSessionPath {
		code := r.URL.Query().Get("code")
		s.jupyter.mu.Lock()
		g, ok := s.jupyter.codes[code]
		delete(s.jupyter.codes, code)
		sid := ""
		if ok && g.label == label && now.Before(g.expires) {
			sid = newSecret()
			if s.jupyter.sessions == nil {
				s.jupyter.sessions = map[string]jupyterGrant{}
			}
			for k, v := range s.jupyter.sessions {
				if now.After(v.expires) {
					delete(s.jupyter.sessions, k)
				}
			}
			s.jupyter.sessions[sid] = jupyterGrant{label: label, expires: now.Add(jupyterSessionTTL)}
		}
		s.jupyter.mu.Unlock()
		if sid == "" {
			jupyterRefuse(w, http.StatusUnauthorized, "Open JupyterLab from the console",
				"This link has expired or was already used. Open JupyterLab from the instance's row on the console's Workbench Instances page.")
			return
		}
		http.SetCookie(w, &http.Cookie{Name: jupyterCookie, Value: sid, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode,
			MaxAge: int(jupyterSessionTTL.Seconds())})
		next := r.URL.Query().Get("next")
		if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
			next = "/lab"
		}
		http.Redirect(w, r, next, http.StatusFound)
		return
	}
	c, err := r.Cookie(jupyterCookie)
	s.jupyter.mu.Lock()
	labs := s.jupyter.labs
	ok := false
	if err == nil {
		g, found := s.jupyter.sessions[c.Value]
		ok = found && g.label == label && now.Before(g.expires)
	}
	s.jupyter.mu.Unlock()
	if !ok && labs != nil {
		if tok, found := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); found && strings.TrimSpace(tok) != "" {
			if err := labs.Authorize(r.Context(), label, strings.TrimSpace(tok)); err != nil {
				jupyterRefuse(w, http.StatusForbidden, "Forbidden", err.Error())
				return
			}
			ok = true
		}
	}
	if !ok {
		jupyterRefuse(w, http.StatusUnauthorized, "Open JupyterLab from the console",
			"This instance's JupyterLab is opened from the console: click Open JupyterLab in the instance's row on the Workbench Instances page.")
		return
	}
	if labs == nil {
		jupyterRefuse(w, http.StatusNotFound, "Not found", "This console serves no Workbench instances.")
		return
	}
	addr, token, _, err := labs.Endpoint(label)
	if err != nil {
		jupyterRefuse(w, http.StatusServiceUnavailable, "JupyterLab is not available", err.Error())
		return
	}
	target := &url.URL{Scheme: "http", Host: addr}
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			// The browser's Host, so the server's Origin checks compare
			// the page's origin with it.
			pr.Out.Host = pr.In.Host
			pr.Out.Header.Set("Authorization", "token "+token)
			// The console's session cookie stays here.
			var keep []string
			for _, ck := range pr.In.Cookies() {
				if ck.Name != jupyterCookie {
					keep = append(keep, ck.Name+"="+ck.Value)
				}
			}
			pr.Out.Header.Del("Cookie")
			if len(keep) > 0 {
				pr.Out.Header.Set("Cookie", strings.Join(keep, "; "))
			}
		},
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			jupyterRefuse(w, http.StatusBadGateway, "JupyterLab is not answering", err.Error())
		},
	}
	rp.ServeHTTP(w, r)
}

// withJupyter serves the instances' proxy hosts, and next every other.
func (s *Server) withJupyter(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if label := jupyterLabel(r.Host); label != "" {
			s.serveJupyter(w, r, label)
			return
		}
		next.ServeHTTP(w, r)
	})
}
