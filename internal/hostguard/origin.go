package hostguard

import (
	"fmt"
	"net/netip"
	"net/url"
	"strings"
)

// The Host check stops a rebound page, which arrives under a name the
// attacker controls. It does not stop an ordinary cross-site request: a page
// on https://evil.example calling fetch("http://127.0.0.1:9001/...") sends
// Host: 127.0.0.1:9001, which passes. What marks that request is its Origin.
// A service that answers cross-origin requests (the builtin storage server,
// whose JSON API allows any origin as Google's does) checks the Origin too,
// with LoopbackOrigin and an allowlist of origins parsed by ParseOrigin
// (#677, ADR-0004).

// ParseOrigin checks that s is a web origin, scheme://host[:port] with the
// scheme http or https and nothing after the authority, and returns it as a
// browser serializes it: lower-case, without a trailing slash, and without
// the scheme's default port.
func ParseOrigin(s string) (string, error) {
	raw := strings.TrimSuffix(strings.TrimSpace(s), "/")
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("origin %q: %w", s, err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("origin %q: want http:// or https:// followed by a host, such as https://app.example:8443", s)
	}
	if u.Opaque != "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery {
		return "", fmt.Errorf("origin %q: an origin is scheme://host[:port], with no path, query or credentials", s)
	}
	host := strings.ToLower(u.Hostname())
	if host == "" || strings.Contains(host, "*") {
		return "", fmt.Errorf("origin %q: want one exact host; wildcards are not accepted", s)
	}
	port := u.Port()
	if (scheme == "http" && port == "80") || (scheme == "https" && port == "443") {
		port = ""
	}
	authority := host
	if strings.Contains(host, ":") {
		authority = "[" + host + "]"
	}
	if port != "" {
		authority += ":" + port
	}
	return scheme + "://" + authority, nil
}

// LoopbackOrigin reports whether origin, a request's Origin header, is a
// page served from this machine's loopback: http or https on any port, at a
// loopback address (127.0.0.0/8, [::1]), localhost, or a name under
// .localhost, which browsers resolve to loopback themselves (RFC 6761).
// "null", sent by sandboxed frames and file: pages, is not.
func LoopbackOrigin(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	host := normalize(u.Hostname())
	if a, err := netip.ParseAddr(host); err == nil {
		return a.Unmap().IsLoopback()
	}
	return host == "localhost" || strings.HasSuffix(host, ".localhost")
}
