// Package localhost dials names under `.localhost` on loopback without asking
// a resolver (#714).
//
// RFC 6761 §6.3 reserves `.localhost` for loopback, and Cloud Run services are
// named under cloudburrow.localhost, so a Cloud Tasks or Cloud Scheduler
// target such as http://hello.default.cloudburrow.localhost:9080/ is an
// ordinary thing to write. Whether it resolves depends on the host's resolver,
// not on the name: Go's pure resolver, which a CGO_ENABLED=0 release CLI uses
// on Linux, sends the query to the nameserver in /etc/resolv.conf, and glibc
// without systemd-resolved does not resolve it either (docs/networking.md).
// The CLI dials these targets itself, so it answers the name as the RFC says
// and leaves every other name to the resolver.
package localhost

import (
	"context"
	"net"
	"net/http"
	"strings"
	"time"
)

// Loopback is the address a `.localhost` name is dialled on. It is the IPv4
// loopback because that is what the gateway and every CloudBurrow endpoint
// listen on by default (bind_address 127.0.0.1).
const Loopback = "127.0.0.1"

// IsSubdomain reports whether host is a name under `.localhost`, such as
// hello.default.cloudburrow.localhost. Bare "localhost" is not: every resolver
// already answers it from /etc/hosts, which may map it to ::1 as well, and
// that answer is left alone.
func IsSubdomain(host string) bool {
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	return strings.HasSuffix(h, ".localhost") && len(h) > len(".localhost")
}

// Rewrite returns addr with a `.localhost` host replaced by Loopback, and any
// other addr unchanged.
func Rewrite(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil || !IsSubdomain(host) {
		return addr
	}
	return net.JoinHostPort(Loopback, port)
}

// Transport returns a copy of http.DefaultTransport that dials `.localhost`
// names on Loopback. The request, its Host header included, is unchanged, so
// the gateway still routes by the name. Proxy settings still apply.
func Transport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	// The same dialer settings http.DefaultTransport uses.
	d := net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	t.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return d.DialContext(ctx, network, Rewrite(addr))
	}
	return t
}
