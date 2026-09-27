// Package hostguard refuses HTTP requests addressed to a host CloudBurrow
// does not answer to. It is the DNS-rebinding defence (#676, ADR-0004).
//
// The loopback bind keeps other machines out, but it does not keep out the
// developer's own browser. With DNS rebinding, attacker.example first
// resolves to the attacker and then to 127.0.0.1. The attacker's page then
// talks to a CloudBurrow port as a same-origin peer: the browser sends
// Host: attacker.example:9090, a matching Origin and Sec-Fetch-Site:
// same-origin, and every origin check passes. The one thing the attacker
// cannot change is the name in Host, because the browser always sends the
// name the page was loaded from. So every listener checks that name.
//
// A request is accepted when its Host is:
//
//   - an IP address literal. Rebinding needs a DNS name the attacker
//     controls. A browser only sends an IP literal when the page itself was
//     loaded from that address, and then the origin really is that address.
//     This covers 127.0.0.1 and [::1], a bind address chosen with
//     --allow-remote, the kind network gateway pods reach the Linux relay at
//     (#575), a pod IP a kubelet probe uses and a Service's cluster IP;
//   - localhost or any name under .localhost. Browsers resolve these to
//     loopback themselves (RFC 6761), and no public DNS answers for them;
//   - a name CloudBurrow publishes and no attacker can register:
//     host.docker.internal, and cloudburrow-host, the cluster name pods use
//     for the services this process serves;
//   - a name the listener adds itself, such as the builtin storage server's
//     virtual-hosted bucket names.
//
// A request with no Host at all is also accepted: browsers always send one,
// so its absence means the caller is not a browser.
//
// Cleartext HTTP/2 is not checked. Browsers speak HTTP/2 only over TLS, and
// every CloudBurrow listener is plaintext, so a cleartext HTTP/2 request
// never comes from a browser. This is how gRPC clients reach the ports that
// share gRPC with a JSON API. Their :authority is whatever name they dialed,
// and a check on it could only break them. JSON requests over HTTP/1.1, and
// gRPC-Web, which browsers send over HTTP/1.1, are checked.
package hostguard

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// ClusterHostService is the cluster name pods use for the services the CLI
// serves (#575).
const ClusterHostService = "cloudburrow-host"

// Wrap returns next behind the Host check. extra names more hosts the
// listener answers to: an exact name, or "*.name" for any name below it.
func Wrap(next http.Handler, extra ...string) http.Handler {
	names := make([]string, 0, len(extra))
	for _, e := range extra {
		if e = normalize(e); e != "" {
			names = append(names, e)
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if (r.ProtoMajor >= 2 && r.TLS == nil) || Allowed(r.Host, names...) {
			next.ServeHTTP(w, r)
			return
		}
		Refuse(w, r)
	})
}

// Refuse answers r with 421 Misdirected Request, naming the host it was
// refused for. Wrap calls it; so does a listener that checks Host itself.
func Refuse(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// 421 Misdirected Request: this server does not answer for that
	// authority. It is not cacheable by default, so a rebinding page
	// cannot leave it behind for the real name either.
	w.WriteHeader(http.StatusMisdirectedRequest)
	fmt.Fprint(w, RefusalMessage(r.Host)+"\n")
}

// RefusalMessage is the one-line reason a request for host is refused.
func RefusalMessage(host string) string {
	return fmt.Sprintf("cloudburrow: refused a request for host %q: CloudBurrow answers only to "+
		"an IP address, localhost, or a name it publishes, such as %s.<namespace>.svc.cluster.local "+
		"or host.docker.internal. This is the DNS-rebinding defence (ADR-0004); address the "+
		"server as 127.0.0.1 or localhost.", host, ClusterHostService)
}

// Allowed reports whether a request for hostport, a Host header value,
// passes the check. extra is as for Wrap.
func Allowed(hostport string, extra ...string) bool {
	if hostport == "" {
		return true
	}
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	} else if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = host[1 : len(host)-1]
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return true
	}
	host = normalize(host)
	switch {
	case host == "":
		return false
	case host == "localhost" || strings.HasSuffix(host, ".localhost"):
		return true
	case host == "host.docker.internal":
		return true
	case IsServiceName(host, ClusterHostService):
		return true
	}
	for _, e := range extra {
		e = normalize(e)
		if suffix, ok := strings.CutPrefix(e, "*."); ok {
			if strings.HasSuffix(host, "."+suffix) {
				return true
			}
		} else if host == e {
			return true
		}
	}
	return false
}

// IsServiceName reports whether host is how a pod names the Kubernetes
// Service service: the bare name, <service>.<namespace>.svc, or
// <service>.<namespace>.svc.cluster.local.
//
// <service>.<namespace> is left out on purpose. A namespace can share its
// name with a public top-level domain, and then that name is one an attacker
// can register. The other forms end in a label that public DNS does not
// delegate.
func IsServiceName(host, service string) bool {
	labels := strings.Split(normalize(host), ".")
	if labels[0] != service {
		return false
	}
	switch len(labels) {
	case 1:
		return true
	case 3:
		return labels[1] != "" && labels[2] == "svc"
	case 5:
		return labels[1] != "" && labels[2] == "svc" && labels[3] == "cluster" && labels[4] == "local"
	}
	return false
}

// normalize lower-cases a name and drops a trailing root dot, so
// "LocalHost." and "localhost" are the same host.
func normalize(name string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(name)), ".")
}
