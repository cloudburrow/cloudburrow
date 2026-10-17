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
	"github.com/cloudburrow/cloudburrow/internal/firestoreadmin"
	"github.com/cloudburrow/cloudburrow/internal/frontready"
	"github.com/cloudburrow/cloudburrow/internal/netfwd"
	"github.com/cloudburrow/cloudburrow/internal/queryfront"
	"github.com/cloudburrow/cloudburrow/internal/spannerfront"
)

// ErrUsage means the arguments were refused; the reason has been written.
var ErrUsage = errors.New("usage")

// Intercept answers some calls itself instead of the emulator: the
// Firestore Admin API (internal/firestoreadmin, #1331), the query RPCs
// (internal/queryfront, #1405) and Spanner's instance updates
// (internal/spannerfront, #1461), which the emulators do not serve.
type Intercept interface {
	http.Handler
	Claims(r *http.Request) bool
}

// Intercepts is several intercepts; the first that claims a call answers it.
type Intercepts []Intercept

// Claims reports whether any of the intercepts claims r.
func (is Intercepts) Claims(r *http.Request) bool { return is.claimant(r) != nil }

func (is Intercepts) claimant(r *http.Request) Intercept {
	for _, i := range is {
		if i.Claims(r) {
			return i
		}
	}
	return nil
}

// ServeHTTP answers r through the first intercept that claims it.
func (is Intercepts) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if i := is.claimant(r); i != nil {
		i.ServeHTTP(w, r)
		return
	}
	http.NotFound(w, r)
}

// Handler is the front for the emulator at upstream, recording into ring.
func Handler(upstream string, ring *calllog.Ring, logf func(string, ...any)) http.Handler {
	return HandlerWith(upstream, ring, logf, nil)
}

// HandlerWith is Handler, with the calls icpt claims answered by it rather
// than the emulator, and recorded in the same call log.
func HandlerWith(upstream string, ring *calllog.Ring, logf func(string, ...any), icpt Intercept) http.Handler {
	var next http.Handler = netfwd.PassThrough(upstream, logf)
	if icpt != nil {
		pass := next
		next = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if icpt.Claims(r) {
				icpt.ServeHTTP(w, r)
				return
			}
			pass.ServeHTTP(w, r)
		})
	}
	proxy := netfwd.Observed(next, func(c netfwd.Call) {
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
	// A second port of the same emulator, fronted into the same call log:
	// Spanner's admin REST API, which the Terraform provider uses (#1331).
	restListen := fs.String("rest-listen", "", "address the front serves the emulator's second (REST) port on")
	restUpstream := fs.String("rest-upstream", "", "the emulator's second (REST) address, on the pod's loopback")
	// The Firestore Admin API, served by the front itself (#1331).
	firestoreAdmin := fs.Bool("firestore-admin", false, "serve the Firestore Admin API in front of the Firestore emulator")
	// The instance's Cloud Storage, which Firestore exports are written to
	// and imports read from (#1500).
	storage := fs.String("storage", "", "the instance's Cloud Storage base URL, for Firestore exports and imports")
	// The query RPCs the emulator does not serve, answered by the front
	// from calls it does (#1405).
	// The Spanner instance updates the emulator refuses, answered by the
	// front over both ports (#1461).
	spannerInstances := fs.Bool("spanner-instances", false, "answer the Spanner instance updates the emulator does not serve")
	// Where the Firestore Admin API and the Spanner front keep their
	// records and data copies (#1514): the pod's emptyDir.
	stateDir := fs.String("state-dir", "", "directory the Firestore Admin API or the Spanner front keeps its state in")
	queries := fs.String("queries", "", "answer the query RPCs the emulator does not serve: datastore (RunAggregationQuery) or firestore (PartitionQuery)")
	if err := fs.Parse(args); err != nil {
		return ErrUsage
	}
	if fs.NArg() > 0 || *listen == "" || *upstream == "" || (*restListen == "") != (*restUpstream == "") {
		fmt.Fprintln(stderr, "call-front: --listen and --upstream are required, --rest-listen and --rest-upstream go together, and nothing else")
		return ErrUsage
	}
	if *queries != "" && *queries != "datastore" && *queries != "firestore" {
		fmt.Fprintln(stderr, "call-front: --queries is datastore or firestore")
		return ErrUsage
	}
	logger := log.New(stdout, "", log.LstdFlags|log.LUTC)
	ring := &calllog.Ring{}
	pairs := [][2]string{{*listen, *upstream}}
	if *restListen != "" {
		pairs = append(pairs, [2]string{*restListen, *restUpstream})
	}
	var servers []*http.Server
	var listeners []net.Listener
	var icpts Intercepts
	switch *queries {
	case "datastore":
		icpts = append(icpts, queryfront.NewDatastore(*upstream))
	case "firestore":
		icpts = append(icpts, queryfront.NewFirestore(*upstream))
	}
	if *firestoreAdmin {
		fa := firestoreadmin.New(*upstream)
		fa.SetStorage(*storage)
		if *stateDir != "" {
			if err := fa.KeepState(ctx, *stateDir, logger.Printf); err != nil {
				return fmt.Errorf("the Firestore Admin API's state: %w", err)
			}
		}
		fa.Start(ctx)
		icpts = append(icpts, fa)
	}
	if *spannerInstances {
		si, err := spannerfront.New(*upstream)
		if err != nil {
			return fmt.Errorf("the Spanner instance front: %w", err)
		}
		if *stateDir != "" {
			if err := si.KeepState(ctx, *stateDir, logger.Printf); err != nil {
				return fmt.Errorf("the Spanner front's state: %w", err)
			}
		}
		icpts = append(icpts, si, spannerfront.NewRefusals())
	}
	var icpt Intercept
	if len(icpts) > 0 {
		icpt = icpts
	}
	for _, p := range pairs {
		l, err := net.Listen("tcp", p[0])
		if err != nil {
			for _, l := range listeners {
				_ = l.Close()
			}
			return fmt.Errorf("listen on %s: %w", p[0], err)
		}
		listeners = append(listeners, l)
		// Both ports carry the intercepts: Spanner's REST port is the
		// same API as its gRPC one.
		servers = append(servers, Server(HandlerWith(p[1], ring, logger.Printf, icpt), logger.Printf))
		logger.Printf("call front: serving %s for the emulator at %s", l.Addr(), p[1])
	}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for _, srv := range servers {
			_ = srv.Shutdown(sctx)
		}
	}()
	errs := make(chan error, len(servers))
	for i, srv := range servers {
		go func() { errs <- srv.Serve(listeners[i]) }()
	}
	// One port failing fails the front, so the pod restarts it whole
	// rather than serve one port of two.
	for range servers {
		if err := <-errs; err != nil && !errors.Is(err, http.ErrServerClosed) {
			for _, srv := range servers {
				_ = srv.Close()
			}
			return err
		}
	}
	return nil
}
