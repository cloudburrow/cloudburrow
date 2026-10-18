// Package debugpprof serves the Go runtime's profiles (net/http/pprof) when
// CLOUDBURROW_DEBUG_PPROF names a loopback address, for a soak or a leak hunt
// (#1609). It is off by default: with the variable unset nothing listens.
//
// The address must be loopback, so the profiles never leave the machine, or,
// in a pod, the pod: read them there with `kubectl port-forward`, which
// reaches a pod's loopback.
package debugpprof

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/pprof"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/hostguard"
)

// EnvVar is the variable that turns the profiles on, such as
// CLOUDBURROW_DEBUG_PPROF=127.0.0.1:6060.
const EnvVar = "CLOUDBURROW_DEBUG_PPROF"

// profiles serves the profiles under /debug/pprof/.
func profiles() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	return mux
}

// Start listens on the address getenv(EnvVar) names and serves the
// profiles there in the background, behind the Host check every other
// listener has. It returns "" and nil when the variable is unset,
// and an error when the address is not loopback or cannot be listened on.
func Start(getenv func(string) string, log io.Writer) (string, error) {
	addr := getenv(EnvVar)
	if addr == "" {
		return "", nil
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("%s=%q: %w", EnvVar, addr, err)
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return "", fmt.Errorf("%s=%q: the address must be a loopback IP, such as 127.0.0.1:6060", EnvVar, addr)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return "", fmt.Errorf("%s: %w", EnvVar, err)
	}
	srv := &http.Server{Handler: hostguard.Wrap(profiles()), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	if log != nil {
		fmt.Fprintf(log, "debug: serving pprof on http://%s/debug/pprof/ (%s)\n", ln.Addr(), EnvVar)
	}
	return ln.Addr().String(), nil
}
