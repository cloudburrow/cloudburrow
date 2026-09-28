// Package frontready is the readiness path a front serves for the backend
// beside it in its pod (#1114).
//
// A backend with a front listens on the pod's loopback only, so that no
// other pod can reach it past the front's checks. Kubernetes probes a
// container at the pod's IP, which a loopback port does not answer, so the
// backend's container is probed through the front instead: the front
// answers Path with 200 while each of its backend's ports accepts a
// connection on loopback, and 503 otherwise.
package frontready

import (
	"net"
	"net/http"
	"time"
)

// Path is the front's path the backend container's readiness probe gets.
// It is not a path of any Google API.
const Path = "/cloudburrow/backend-ready"

// dialTimeout bounds each connection attempt.
const dialTimeout = time.Second

// Ready reports whether each of upstreams (host:port) accepts a TCP
// connection, and the first that does not.
func Ready(upstreams ...string) (bool, string) {
	for _, u := range upstreams {
		c, err := net.DialTimeout("tcp", u, dialTimeout)
		if err != nil {
			return false, u
		}
		_ = c.Close()
	}
	return true, ""
}

// Wrap serves Path, by Ready of upstreams, and passes every other request
// to next.
func Wrap(next http.Handler, upstreams ...string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != Path {
			next.ServeHTTP(w, r)
			return
		}
		Serve(w, upstreams...)
	})
}

// Serve answers a request for Path.
func Serve(w http.ResponseWriter, upstreams ...string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if ok, down := Ready(upstreams...); !ok {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("the backend does not accept connections on " + down + "\n"))
		return
	}
	_, _ = w.Write([]byte("ok\n"))
}
