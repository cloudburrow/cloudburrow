// Package callfront is the call front the Spanner, Firestore and Datastore
// pods run beside Google's emulators (#1288), as `cloudburrow-storage
// call-front`.
//
// Those emulators had no front: Spanner's port is a raw tunnel, and the
// host tunnel's guard saw Firestore's and Datastore's calls from the host
// only (#1267), not a pod's, which reach the emulator's Service directly.
// The call front serves the Service's port and passes every request to the
// emulator on the pod's loopback unchanged, in the protocol it arrived in:
// gRPC over h2c, with its streams and trailers, and REST over HTTP/1.1
// (netfwd.PassThrough, the guard's own proxy without its Host check). The
// bytes pass unread. It keeps each call's method, the resource its routing
// headers or REST path name, its gRPC code or HTTP status, its start,
// duration and traceparent in a call log (internal/calllog), never a
// payload, which the CLI reads through the tunnel as it reads the Pub/Sub
// and Bigtable fronts' (#1266, #1267). Every client is seen, the host's and
// a pod's alike.
//
// It also answers the readiness path the backend's container is probed
// through (internal/frontready), since the emulator listens on the pod's
// loopback alone.
package callfront

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/calllog"
	"github.com/cloudburrow/cloudburrow/internal/frontready"
	"github.com/cloudburrow/cloudburrow/internal/netfwd"
)

// ErrUsage means the arguments were refused; the reason has been written.
var ErrUsage = errors.New("usage")

// Handler is the front for the emulator at upstream, recording into ring.
func Handler(upstream string, ring *calllog.Ring, logf func(string, ...any)) http.Handler {
	proxy := netfwd.Observed(netfwd.PassThrough(upstream, logf), func(c netfwd.Call) {
		transport := "http"
		if c.GRPC {
			transport = "grpc"
		}
		ring.Add(calllog.Call{Method: c.Method, Transport: transport, Code: c.Code, Resource: c.Resource,
			Time: c.Time, Duration: c.Duration, TraceParent: c.TraceParent})
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == calllog.Path && r.Method == http.MethodGet && !strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc"):
			ring.ServeHTTP(w, r)
		case r.URL.Path == frontready.Path:
			frontready.Serve(w, upstream)
		default:
			proxy.ServeHTTP(w, r)
		}
	})
}

// Server serves h on both protocols an emulator port carries: HTTP/1.1 and
// cleartext HTTP/2 with prior knowledge, which is how the official clients
// speak gRPC to an emulator.
func Server(h http.Handler, logf func(string, ...any)) *http.Server {
	var protocols http.Protocols
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	return &http.Server{
		Handler:   h,
		Protocols: &protocols,
		// The emulators set no stream limit; Go's default of 250 could
		// queue a client's new calls behind its long-lived streams.
		HTTP2:             &http.HTTP2Config{MaxConcurrentStreams: 1 << 16},
		ReadHeaderTimeout: 30 * time.Second,
		ErrorLog:          log.New(logWriter(logf), "", 0),
	}
}

type logWriter func(string, ...any)

func (l logWriter) Write(p []byte) (int, error) {
	l("call front: %s", strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// Run serves the front with the flags of `cloudburrow-storage call-front`
// until ctx ends.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("call-front", flag.ContinueOnError)
	fs.SetOutput(stderr)
	listen := fs.String("listen", "", "address the front serves the emulator's port on")
	upstream := fs.String("upstream", "", "the emulator's address, on the pod's loopback")
	if err := fs.Parse(args); err != nil {
		return ErrUsage
	}
	if fs.NArg() > 0 || *listen == "" || *upstream == "" {
		fmt.Fprintln(stderr, "call-front: --listen and --upstream are required, and nothing else")
		return ErrUsage
	}
	logger := log.New(stdout, "", log.LstdFlags|log.LUTC)
	l, err := net.Listen("tcp", *listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", *listen, err)
	}
	srv := Server(Handler(*upstream, &calllog.Ring{}, logger.Printf), logger.Printf)
	logger.Printf("call front: serving %s for the emulator at %s", l.Addr(), *upstream)
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	if err := srv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
