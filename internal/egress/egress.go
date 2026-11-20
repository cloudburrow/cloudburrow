// Package egress is the local-only guard every outbound dispatcher sends
// through (#2015): Cloud Scheduler HTTP jobs, Cloud Tasks HTTP targets,
// Pub/Sub push, Eventarc HTTP destinations, Workflows http.* steps,
// Monitoring webhooks and uptime checks, Cloud Storage notification
// channels and Secure Source Manager hooks.
//
// A dispatcher sends a request to a URL a user stored. On GCP such a URL may
// name a Google API (a workflow's Cloud Scheduler trigger posts to
// workflowexecutions.googleapis.com). Here a Google API host is never
// dialled: a *.googleapis.com URL is sent to this instance's own endpoint of
// the API it names when the instance serves it, and is refused locally when
// it does not; every other Google host (*.google.com, *.run.app,
// *.cloudfunctions.net, *.appspot.com, the metadata server ...) is refused.
// The decision is made on the URL, before any DNS lookup, proxy or dial, and
// again on every redirect, because Transport makes it in RoundTrip. A URL off
// the machine that is not Google's stays the user's choice, as on GCP,
// unless the dispatcher is stricter.
package egress

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
)

// googleDomains are the domains whose hosts are Google's: none is ever
// dialled. googleapis.com hosts are mapped to this instance first.
var googleDomains = []string{
	"googleapis.com", "google.com", "googleusercontent.com", "gstatic.com", "googleprod.com",
	"run.app", "cloudfunctions.net", "appspot.com", "sourcemanager.dev",
	"google.internal", "metadata.goog", "gcr.io", "pkg.dev",
}

// metadataIP is the GCE metadata server's address.
const metadataIP = "169.254.169.254"

// GoogleHost reports whether host is one of Google's, which no dispatcher
// dials.
func GoogleHost(host string) bool {
	h := normalize(host)
	if h == metadataIP || h == "fd00:ec2::254" {
		return true
	}
	for _, d := range googleDomains {
		if h == d || strings.HasSuffix(h, "."+d) {
			return true
		}
	}
	return false
}

func normalize(host string) string {
	return strings.ToLower(strings.TrimSuffix(strings.Trim(host, "[]"), "."))
}

// LocalAPIs maps an API, by the first label of its googleapis.com host
// ("storage", "pubsub", "workflowexecutions" ...), to this instance's
// "http://host:port" serving it, or "" when it is not served.
type LocalAPIs func(api string) string

var local atomic.Pointer[LocalAPIs]

// SetLocal sets the process's map of served APIs, which Resolve and
// Transport use. The instance sets it when it starts; until then, and in a
// process that never sets it (a front in a pod), every Google API host is
// refused.
func SetLocal(f LocalAPIs) {
	if f == nil {
		local.Store(nil)
		return
	}
	local.Store(&f)
}

// Local is the process's map, set by SetLocal; nil when unset.
func Local() LocalAPIs {
	if p := local.Load(); p != nil {
		return *p
	}
	return nil
}

// RefusedError is a request CloudBurrow refused to send because its host is
// Google's.
type RefusedError struct {
	Host string
	// API is the googleapis.com API named, "" for another Google host.
	API string
}

func (e *RefusedError) Error() string {
	if e.API != "" {
		return fmt.Sprintf("refused: %s.googleapis.com is not served by this CloudBurrow instance, which sends no request to Google (start it with the service in --services)", e.API)
	}
	return fmt.Sprintf("refused: %s is Google's, and CloudBurrow sends no request to Google; send to the resource's local URL instead", e.Host)
}

// API is the API a googleapis.com host and path name, given the served map:
// the host's first label, a regional prefix dropped when only the
// unprefixed API is served (us-central1-run → run), and www.googleapis.com's
// /storage/ and /upload/storage/ paths are storage. ok is false for a host
// that is not under googleapis.com.
func API(host, path string, served LocalAPIs) (api string, ok bool) {
	h := normalize(host)
	rest, ok := strings.CutSuffix(h, ".googleapis.com")
	if !ok {
		return "", false
	}
	api, _, _ = strings.Cut(rest, ".")
	if api == "www" && (strings.HasPrefix(path, "/storage/") || strings.HasPrefix(path, "/upload/storage/")) {
		return "storage", true
	}
	if i := strings.LastIndex(api, "-"); i >= 0 && served != nil && served(api) == "" && served(api[i+1:]) != "" {
		api = api[i+1:]
	}
	return api, true
}

// ResolveURL is where u is sent: u itself when its host is not Google's, a
// copy at this instance's endpoint of a served googleapis.com API, or a
// *RefusedError. served is the map; nil serves nothing.
func ResolveURL(u *url.URL, served LocalAPIs) (*url.URL, error) {
	host := u.Hostname()
	if !GoogleHost(host) {
		return u, nil
	}
	api, isAPI := API(host, u.Path, served)
	if !isAPI {
		return nil, &RefusedError{Host: normalize(host)}
	}
	base := ""
	if served != nil {
		base = served(api)
	}
	if base == "" {
		return nil, &RefusedError{Host: normalize(host), API: api}
	}
	b, err := url.Parse(base)
	if err != nil || b.Host == "" {
		return nil, fmt.Errorf("bad local endpoint %q for %s: %v", base, api, err)
	}
	out := *u
	out.Scheme, out.Host = b.Scheme, b.Host
	return &out, nil
}

// ResolveWith is ResolveURL on a raw URL. A URL that does not parse is
// returned as it is, for the caller's request to reject.
func ResolveWith(raw string, served LocalAPIs) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return raw, nil
	}
	out, err := ResolveURL(u, served)
	if err != nil {
		return "", err
	}
	if out == u {
		return raw, nil
	}
	return out.String(), nil
}

// Resolve is ResolveWith the process's map (SetLocal).
func Resolve(raw string) (string, error) { return ResolveWith(raw, Local()) }

// RoundTripper resolves every request before Next sends it.
type RoundTripper struct {
	// Next sends the resolved request; http.DefaultTransport when nil.
	Next http.RoundTripper
	// Served is the map to resolve with; the process's (SetLocal) when nil.
	Served LocalAPIs
}

// RoundTrip refuses a request to a Google host, sends one to a served
// googleapis.com API to this instance, and passes every other one on.
func (t *RoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	served := t.Served
	if served == nil {
		served = Local()
	}
	u, err := ResolveURL(req.URL, served)
	if err != nil {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, err
	}
	if u != req.URL {
		r := req.Clone(req.Context())
		r.URL = u
		// The Host header follows the URL: the local endpoint's Host check
		// accepts its own address, not googleapis.com.
		r.Host = ""
		req = r
	}
	next := t.Next
	if next == nil {
		next = http.DefaultTransport
	}
	return next.RoundTrip(req)
}

// Transport wraps next (http.DefaultTransport when nil) in the guard.
func Transport(next http.RoundTripper) http.RoundTripper {
	if g, ok := next.(*RoundTripper); ok {
		return g
	}
	return &RoundTripper{Next: next}
}

// Client is a copy of c (a new client when nil) whose transport is behind
// the guard; c itself, which may be shared (http.DefaultClient), is left.
func Client(c *http.Client) *http.Client {
	out := &http.Client{}
	if c != nil {
		*out = *c
	}
	out.Transport = Transport(out.Transport)
	return out
}

// Guarded reports whether c sends through the guard.
func Guarded(c *http.Client) bool {
	_, ok := c.Transport.(*RoundTripper)
	return ok
}

// SetNext replaces what the guard on c sends with, returning the previous
// one; tests put a loopback-only dialer beneath a dispatcher's own client.
// It panics when c is not guarded.
func SetNext(c *http.Client, next http.RoundTripper) http.RoundTripper {
	g := c.Transport.(*RoundTripper)
	prev := g.Next
	g.Next = next
	return prev
}

// IsLoopbackHost reports whether host is a loopback address or a name that
// is one by definition (localhost and names under .localhost).
func IsLoopbackHost(host string) bool {
	h := normalize(host)
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}
