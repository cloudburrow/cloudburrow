package apikeys

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/resource"
)

// Request is what a served API knows of a call that carries an API key,
// for Check.
type Request struct {
	// Service is the API's googleapis.com name ("storage.googleapis.com").
	Service string
	// Method is the called method: the full gRPC method
	// ("google.pubsub.v1.Publisher.Publish") or a REST method's ID.
	Method string
	// Referrer is the request's Referer header, "" for none.
	Referrer string
	// IP is the caller's IP address.
	IP string
	// AndroidPackage and AndroidCert are the X-Android-Package and
	// X-Android-Cert headers, IOSBundle the X-Ios-Bundle-Identifier one.
	AndroidPackage string
	AndroidCert    string
	IOSBundle      string
}

// Denial is Google's refusal of a request's key: the HTTP status, the
// Google status, the message, and the google.rpc.ErrorInfo reason (domain
// googleapis.com) with its metadata.
type Denial struct {
	HTTP     int
	Status   string
	Message  string
	Reason   string
	Metadata map[string]string
}

func (d *Denial) Error() string { return d.Message }

// Domain is the ErrorInfo domain of every Denial.
const Domain = "googleapis.com"

// Invalid is API_KEY_INVALID: a key string no live key has (malformed,
// never made, deleted or purged).
func Invalid(service string) *Denial {
	return &Denial{HTTP: http.StatusBadRequest, Status: "INVALID_ARGUMENT",
		Message: "API key not valid. Please pass a valid API key.", Reason: "API_KEY_INVALID",
		Metadata: map[string]string{"service": service}}
}

// toolKeys are the API keys Google's own tools ship with, and send on
// every anonymous call: live keys of Google's projects, not the caller's,
// so Google's front end accepts them, unrestricted, wherever they are
// sent. An instance never holds them in a project of its own.
//
// gsutil's: gslib/gcs_json_api.py adds it as the key= parameter of every
// JSON API call it makes with no credentials ("This API key is not secret
// and is used to identify gsutil during anonymous requests").
//
// They are held by SHA-256, hex, so no key of Google's is written in this
// repository even though each is published: secret scanners flag the
// AIza form, and the instance only ever needs to recognise one.
var toolKeys = map[string]bool{
	"b3f19cc85129501b94306e7892b522f46fa2dcda7da664cc74a0ca9314d7d984": true, // gsutil
}

// isToolKey reports whether keyString is one of toolKeys.
func isToolKey(keyString string) bool {
	sum := sha256.Sum256([]byte(keyString))
	return toolKeys[hex.EncodeToString(sum[:])]
}

// Authorize is the whole decision on a request carrying keyString: the
// key's project, and nil when the key allows the request, else Google's
// Denial. A key one of Google's tools ships (toolKeys) is allowed, with no
// project of the instance's.
func (s *Server) Authorize(keyString string, r Request) (project string, d *Denial) {
	if isToolKey(keyString) {
		return "", nil
	}
	project, k, ok := s.Lookup(keyString)
	if !ok {
		return project, Invalid(r.Service)
	}
	return project, Check(k, r)
}

// Check is k's restrictions applied to r: nil when they allow it, else
// Google's Denial. Application restrictions are checked first, then API
// restrictions.
func Check(k *Key, r Request) *Denial {
	if k == nil || k.Deleted() || k.Purged {
		return Invalid(r.Service)
	}
	md := func() map[string]string {
		return map[string]string{"service": r.Service, "consumer": fmt.Sprintf("projects/%d", resource.ProjectNumber(k.Project))}
	}
	deny := func(reason, msg string) *Denial {
		return &Denial{HTTP: http.StatusForbidden, Status: "PERMISSION_DENIED", Message: msg, Reason: reason, Metadata: md()}
	}
	rs := k.Restrictions
	if rs == nil {
		return nil
	}
	empty := func(s string) string {
		if s == "" {
			return "<empty>"
		}
		return s
	}
	if b := rs.BrowserKeyRestrictions; b != nil && !referrerAllowed(b.AllowedReferrers, r.Referrer) {
		return deny("API_KEY_HTTP_REFERRER_BLOCKED", fmt.Sprintf("Requests from referer %s are blocked.", empty(r.Referrer)))
	}
	if sv := rs.ServerKeyRestrictions; sv != nil && !ipAllowed(sv.AllowedIps, r.IP) {
		return deny("API_KEY_IP_ADDRESS_BLOCKED", fmt.Sprintf("The provided API key has an IP address restriction. The originating IP address of the call (%s) violates this restriction.", r.IP))
	}
	if a := rs.AndroidKeyRestrictions; a != nil && !androidAllowed(a.AllowedApplications, r.AndroidPackage, r.AndroidCert) {
		return deny("API_KEY_ANDROID_APP_BLOCKED", fmt.Sprintf("Requests from this Android client application %s are blocked.", empty(r.AndroidPackage)))
	}
	if i := rs.IosKeyRestrictions; i != nil && !iosAllowed(i.AllowedBundleIds, r.IOSBundle) {
		return deny("API_KEY_IOS_APP_BLOCKED", fmt.Sprintf("Requests from this iOS client application %s are blocked.", empty(r.IOSBundle)))
	}
	if len(rs.APITargets) > 0 && !targetAllowed(rs.APITargets, r.Service, r.Method) {
		return deny("API_KEY_SERVICE_BLOCKED", fmt.Sprintf("Requests to this API %s method %s are blocked.", r.Service, r.Method))
	}
	return nil
}

func targetAllowed(targets []APITarget, service, method string) bool {
	for _, t := range targets {
		if !strings.EqualFold(t.Service, service) {
			continue
		}
		if len(t.Methods) == 0 {
			return true
		}
		short := method
		if i := strings.LastIndexAny(method, "./"); i >= 0 {
			short = method[i+1:]
		}
		for _, m := range t.Methods {
			for _, name := range []string{method, short} {
				if p, wild := strings.CutSuffix(m, "*"); wild {
					if len(name) >= len(p) && strings.EqualFold(name[:len(p)], p) {
						return true
					}
				} else if strings.EqualFold(name, m) {
					return true
				}
			}
		}
	}
	return false
}

func ipAllowed(allowed []string, ip string) bool {
	addr := net.ParseIP(ip)
	if addr == nil {
		return false
	}
	for _, a := range allowed {
		if n, err := parseIPOrCIDR(a); err == nil && n.Contains(addr) {
			return true
		}
	}
	return false
}

func androidAllowed(apps []AndroidApplication, pkg, cert string) bool {
	fp, ok := normalizeSHA1(cert)
	if !ok {
		return false
	}
	for _, a := range apps {
		if a.PackageName == pkg && strings.EqualFold(a.Sha1Fingerprint, fp) {
			return true
		}
	}
	return false
}

func iosAllowed(ids []string, bundle string) bool {
	for _, id := range ids {
		if bundle != "" && id == bundle {
			return true
		}
	}
	return false
}

// referrerPattern is one website restriction, parsed.
type referrerPattern struct {
	scheme string
	// host is the host, lower-cased; a leading "*." matches any
	// subdomain, and "*" any host.
	host string
	port string
	// path is the path, "" for none; a trailing "*" matches any rest.
	path string
}

func parseReferrerPattern(s string) (referrerPattern, error) {
	var p referrerPattern
	if s == "" || strings.ContainsAny(s, " \t\r\n") {
		return p, errors.New("it must be a URL with no spaces")
	}
	rest := s
	if i := strings.Index(rest, "://"); i >= 0 {
		p.scheme = strings.ToLower(rest[:i])
		if p.scheme != "http" && p.scheme != "https" && p.scheme != "*" {
			return p, errors.New("the scheme must be http or https")
		}
		rest = rest[i+3:]
	}
	hostport := rest
	if i := strings.Index(rest, "/"); i >= 0 {
		hostport, p.path = rest[:i], rest[i:]
	}
	host := hostport
	if i := strings.LastIndex(hostport, ":"); i >= 0 && !strings.HasSuffix(hostport, "]") {
		host, p.port = hostport[:i], hostport[i+1:]
		if p.port != "*" {
			if n, err := strconv.Atoi(p.port); err != nil || n <= 0 || n > 65535 {
				return p, errors.New("the port must be a number")
			}
		}
	}
	p.host = strings.ToLower(host)
	if p.host == "" {
		return p, errors.New("a host is required")
	}
	if h := strings.TrimPrefix(p.host, "*."); strings.Contains(h, "*") && p.host != "*" {
		return p, errors.New("* can replace a subdomain or a path, not the middle of a URL")
	}
	if strings.Contains(strings.TrimSuffix(p.path, "*"), "*") {
		return p, errors.New("* can replace a subdomain or a path, not the middle of a URL")
	}
	return p, nil
}

func (p referrerPattern) matches(u *url.URL) bool {
	if p.scheme != "" && p.scheme != "*" && p.scheme != strings.ToLower(u.Scheme) {
		return false
	}
	host := strings.ToLower(u.Hostname())
	switch {
	case p.host == "*":
	case strings.HasPrefix(p.host, "*."):
		if !strings.HasSuffix(host, p.host[1:]) {
			return false
		}
	default:
		if host != p.host {
			return false
		}
	}
	if p.port != "" && p.port != "*" {
		port := u.Port()
		if port == "" {
			port = map[string]string{"http": "80", "https": "443"}[strings.ToLower(u.Scheme)]
		}
		if port != p.port {
			return false
		}
	}
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	if u.RawQuery != "" {
		path += "?" + u.RawQuery
	}
	want := p.path
	if want == "" {
		want = "/"
	}
	if pre, wild := strings.CutSuffix(want, "*"); wild {
		return strings.HasPrefix(path, pre) || path+"/" == pre
	}
	return path == want
}

func referrerAllowed(allowed []string, referrer string) bool {
	if referrer == "" {
		return false
	}
	u, err := url.Parse(referrer)
	if err != nil || u.Host == "" {
		return false
	}
	for _, a := range allowed {
		if p, err := parseReferrerPattern(a); err == nil && p.matches(u) {
			return true
		}
	}
	return false
}
