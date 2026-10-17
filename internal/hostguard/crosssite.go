package hostguard

import (
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
)

// The Host check stops a rebound page; it does not stop an ordinary
// cross-site one (origin.go). A page on https://evil.example can still send
// fetch("http://127.0.0.1:9002/v2/projects/p/locations/l/queues", {method:
// "POST", mode: "no-cors", body: "{...}"}): a simple request, which the
// browser sends without a preflight. The page cannot read the answer, but
// the call runs, and CloudBurrow checks no credentials, so it creates the
// queue (#1586, measured). Two upstream emulators behind the tunnels,
// Pub/Sub and Firestore, are worse: they echo any Origin with
// Access-Control-Allow-Credentials, so the page can read the answers too.
//
// What marks such a request is its Origin, which a page cannot forge: a
// browser sends it on every cross-origin request except a GET or HEAD it
// makes without CORS (an image, a script, a link), and on every POST, PUT,
// PATCH and DELETE. So every listener that Wrap guards also checks Origin
// (CrossSiteAllowed), and refuses a request whose Origin is not:
//
//   - a loopback origin (LoopbackOrigin): a page served from this machine,
//     such as a local web app under development;
//   - the listener's own origin, the scheme and Host the request came to,
//     which the Host check has already vouched for: a page it served itself,
//     as the console serves its own;
//   - on the allowlist set with AllowOrigins, from `up --cors-allow-origin`.
//
// A request with no Origin is a client that is not a browser (the SDKs,
// gcloud, Terraform, curl send none), or a browser's no-cors GET or HEAD,
// whose answer the page cannot read. An unsafe method with no Origin but
// with Sec-Fetch-Site naming another site is refused too, in case a browser
// ever omits Origin where fetch metadata is sent.

var allowedOrigins atomic.Pointer[map[string]bool]

// AllowOrigins sets the origins, beyond loopback ones and a listener's own,
// that every guarded listener in this process serves. Each is one exact
// origin as ParseOrigin accepts it. It replaces the previous set.
func AllowOrigins(origins []string) error {
	set, err := originSet(origins)
	if err != nil {
		return err
	}
	allowedOrigins.Store(&set)
	return nil
}

func validateOrigins(origins []string) error {
	_, err := originSet(origins)
	return err
}

func originSet(origins []string) (map[string]bool, error) {
	set := make(map[string]bool, len(origins))
	for _, o := range origins {
		norm, err := ParseOrigin(o)
		if err != nil {
			return nil, err
		}
		set[norm] = true
	}
	return set, nil
}

// OriginAllowed reports whether a browser request from origin, addressed to
// host over scheme ("http" or "https"), may be served.
func OriginAllowed(origin, scheme, host string) bool {
	if LoopbackOrigin(origin) {
		return true
	}
	o, err := ParseOrigin(origin)
	if err != nil {
		return false
	}
	if set := allowedOrigins.Load(); set != nil && (*set)[o] {
		return true
	}
	if host == "" {
		return false
	}
	self, err := ParseOrigin(scheme + "://" + host)
	return err == nil && self == o
}

// CrossSiteAllowed reports whether r passes the Origin check described
// above. It does not look at Host; Wrap checks that first.
func CrossSiteAllowed(r *http.Request) bool {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		return OriginAllowed(origin, scheme, r.Host)
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	switch r.Header.Get("Sec-Fetch-Site") {
	case "cross-site", "same-site":
		return false
	}
	return true
}

// RefuseOrigin answers a cross-site request with 403 and no CORS headers,
// so a browser neither sends the request a preflight was for nor lets the
// page read the answer.
func RefuseOrigin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusForbidden)
	fmt.Fprint(w, OriginRefusalMessage(r)+"\n")
}

// OriginRefusalMessage is the one-line reason r is refused.
func OriginRefusalMessage(r *http.Request) string {
	from := r.Header.Get("Origin")
	if from == "" {
		from = "Sec-Fetch-Site: " + strings.TrimSpace(r.Header.Get("Sec-Fetch-Site"))
	}
	return fmt.Sprintf("cloudburrow: refused a cross-site browser request (%s): CloudBurrow checks no "+
		"credentials, so it serves browser requests only from loopback origins (localhost, 127.0.0.1, "+
		"[::1]), its own origin, and those named with `cloudburrow up --cors-allow-origin` (ADR-0004, #1586)",
		from)
}
