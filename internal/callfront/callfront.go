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
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/calllog"
	"github.com/cloudburrow/cloudburrow/internal/firestoreadmin"
	"github.com/cloudburrow/cloudburrow/internal/frontready"
	"github.com/cloudburrow/cloudburrow/internal/hostguard"
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

// Watcher is an intercept that is shown the resource each call through the
// front names, its own or the emulator's (#1550): the Firestore Admin API
// keeps the databases documents calls name, which the emulator cannot list.
type Watcher interface {
	SawResource(resource string)
}

// Peeker is an intercept that is shown each request it does not claim
// before the emulator is, and may read its body, putting it back (#1550):
// a Datastore REST call names its database in its body alone.
type Peeker interface {
	SawRequest(r *http.Request)
}

// Stopper is an intercept with a gRPC server of its own, served through
// ServeHTTP, whose calls' handlers can outlive the front's HTTP servers'
// Shutdown (#1804): Stop waits for them, until ctx ends.
type Stopper interface {
	Stop(ctx context.Context) error
}

// Stop stops each of the intercepts that is a Stopper.
func (is Intercepts) Stop(ctx context.Context) error {
	var errs []error
	for _, i := range is {
		if s, ok := i.(Stopper); ok {
			errs = append(errs, s.Stop(ctx))
		}
	}
	return errors.Join(errs...)
}

// SawRequest shows r to each of the intercepts that is a Peeker.
func (is Intercepts) SawRequest(r *http.Request) {
	for _, i := range is {
		if p, ok := i.(Peeker); ok {
			p.SawRequest(r)
		}
	}
}

// SawResource shows resource to each of the intercepts that is a Watcher.
func (is Intercepts) SawResource(resource string) {
	for _, i := range is {
		if w, ok := i.(Watcher); ok {
			w.SawResource(resource)
		}
	}
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
		peeker, _ := icpt.(Peeker)
		next = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if icpt.Claims(r) {
				icpt.ServeHTTP(w, r)
				return
			}
			if peeker != nil {
				peeker.SawRequest(r)
			}
			pass.ServeHTTP(w, r)
		})
	}
	watcher, _ := icpt.(Watcher)
	proxy := netfwd.Observed(next, func(c netfwd.Call) {
		if watcher != nil && c.Resource != "" {
			watcher.SawResource(c.Resource)
		}
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
//
// Every request passes the Host and Origin checks first (#1587): the
// host's tunnels to Firestore and Datastore check them, but Spanner's gRPC
// port is tunnelled raw, and a pod comes to the front directly. service,
// the cluster Service the front serves, is the name pods dial it by, which
// the Host check then passes too (hostguard.WrapService); none is Wrap.
func Server(h http.Handler, logf func(string, ...any), service ...string) *http.Server {
	name := ""
	if len(service) > 0 {
		name = service[0]
	}
	var protocols http.Protocols
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	return &http.Server{
		Handler:   hostguard.WrapService(h, name),
		Protocols: &protocols,
		// The emulators set no stream limit; Go's default of 250 could
		// queue a client's new calls behind its long-lived streams.
		HTTP2:             &http.HTTP2Config{MaxConcurrentStreams: 1 << 16},
		ReadHeaderTimeout: 30 * time.Second,
		ErrorLog:          log.New(logWriter(logf), "", 0),
	}
}

// serveAll serves each of servers on its listener until ctx ends, then shuts
// them down and stops the intercepts, and returns once both are done.
func serveAll(ctx context.Context, servers []*http.Server, listeners []net.Listener, icpts Intercepts) error {
	shut := make(chan struct{})
	go func() {
		defer close(shut)
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for _, srv := range servers {
			_ = srv.Shutdown(sctx)
		}
		// Shutdown waits for ServeHTTP, which can return before a gRPC
		// call an intercept answered has ended (#1804).
		_ = icpts.Stop(sctx)
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
			sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = icpts.Stop(sctx)
			return err
		}
	}
	// Serve returns ErrServerClosed as soon as Shutdown begins, not once
	// it is done.
	<-shut
	return nil
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
	storage := fs.String("storage", "", "the instance's Cloud Storage base URL, for Firestore's and Datastore's exports and imports")
	// The query RPCs the emulator does not serve, answered by the front
	// from calls it does (#1405).
	// The Spanner instance updates the emulator refuses, answered by the
	// front over both ports (#1461).
	spannerInstances := fs.Bool("spanner-instances", false, "answer the Spanner instance updates the emulator does not serve")
	// Where the Firestore Admin API and the Spanner front keep their
	// records and data copies (#1514): the pod's emptyDir.
	documentsDir := fs.String("firestore-documents", "", "directory the Firestore emulator's documents are exported to and imported from (#1615), shared with the emulator's container")
	documentsFresh := fs.Bool("firestore-documents-fresh", false, "discard the exports an earlier front made (ephemeral mode)")
	exportInterval := fs.Duration("firestore-export-interval", 30*time.Second, "how often the Firestore emulator's documents are exported while calls write")
	// Firestore with MongoDB compatibility (#1703): the MongoDB wire
	// protocol for the Admin API's Enterprise edition databases.
	mongoListen := fs.String("mongodb-listen", "", "address the front serves Firestore with MongoDB compatibility's wire protocol on (needs --firestore-admin)")
	stateDir := fs.String("state-dir", "", "directory the Firestore Admin API or the Spanner front keeps its state in")
	// The named Datastore databases calls name, which the emulator cannot
	// list, kept for the project's purge (#1550).
	datastoreDBs := fs.Bool("datastore-databases", false, "record the named Datastore databases calls name, for a project's purge")
	// The cluster Service the front serves, which pods dial it by (#1587).
	service := fs.String("service", "", "the cluster Service the front serves, such as firestore: pods dial it by that name, which the Host check passes")
	// Datastore mode databases and customer-managed keys (#1880, #1881):
	// the Firestore front's Admin API is their record, the Datastore
	// front serves their entities, and the host's Cloud KMS their keys.
	firestoreAdminAddr := fs.String("firestore-admin-addr", "", "the Firestore front (host:port), whose Admin API says which databases the Datastore API serves (with --datastore-databases)")
	datastoreAddr := fs.String("datastore", "", "the Datastore front (host:port), which clears a deleted Datastore mode database's entities (with --firestore-admin)")
	kmsAddr := fs.String("kms", "", "the instance's Cloud KMS (host:port, gRPC), which customer-managed keys are checked against (with --firestore-admin)")
	queries := fs.String("queries", "", "answer the query RPCs the emulator does not serve: datastore (RunAggregationQuery) or firestore (PartitionQuery)")
	if err := fs.Parse(args); err != nil {
		return ErrUsage
	}
	if fs.NArg() > 0 || *listen == "" || *upstream == "" || (*restListen == "") != (*restUpstream == "") {
		fmt.Fprintln(stderr, "call-front: --listen and --upstream are required, --rest-listen and --rest-upstream go together, and nothing else")
		return ErrUsage
	}
	if *mongoListen != "" && !*firestoreAdmin {
		fmt.Fprintln(stderr, "call-front: --mongodb-listen needs --firestore-admin")
		return ErrUsage
	}
	if (*datastoreAddr != "" || *kmsAddr != "") && !*firestoreAdmin {
		fmt.Fprintln(stderr, "call-front: --datastore and --kms need --firestore-admin")
		return ErrUsage
	}
	if *firestoreAdminAddr != "" && !*datastoreDBs {
		fmt.Fprintln(stderr, "call-front: --firestore-admin-addr needs --datastore-databases")
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
	var docs *firestoreadmin.Documents
	var err error
	if *datastoreDBs && *firestoreAdminAddr != "" {
		// The gate goes first: a refused call is answered before any
		// other intercept or the emulator sees it.
		icpts = append(icpts, newDatastoreGate(*firestoreAdminAddr))
	}
	var dbs *datastoreDatabases
	if *datastoreDBs {
		dbs = newDatastoreDatabases(*upstream, logger.Printf)
	}
	switch {
	case *queries == "datastore" || (*datastoreDBs && *queries == ""):
		qd := queryfront.NewDatastore(*upstream)
		if dbs != nil {
			// Each named database in an emulator project of its own
			// (#1885), as the front's own calls keep it (dbs.client).
			qd.SeparateDatabases(dbs.SawResource)
			// The entities named databases kept before are moved
			// to their emulator projects once (#1885); calls naming
			// a named database wait for the move.
			moved := make(chan struct{})
			go dbs.MigrateDatabases(ctx, moved)
			qd.HoldUntil(moved)
		}
		icpts = append(icpts, qd)
	case *queries == "firestore":
		icpts = append(icpts, queryfront.NewFirestore(*upstream))
	}
	if dbs != nil {
		if len(icpts) > 0 {
			if g, ok := icpts[0].(*datastoreGate); ok {
				dbs.gate = g
			}
		}
		// The Datastore Admin API (#1884): exports and imports through
		// the instance's Cloud Storage, and indexes.
		icpts = append(icpts, dbs, newDatastoreAdmin(dbs, *storage))
	}
	if *firestoreAdmin {
		fa := firestoreadmin.New(*upstream)
		fa.SetStorage(*storage)
		fa.SetDatastore(*datastoreAddr)
		if err := fa.SetKMS(*kmsAddr); err != nil {
			return fmt.Errorf("Cloud KMS at %s: %w", *kmsAddr, err)
		}
		if *stateDir != "" {
			if err := fa.KeepState(ctx, *stateDir, logger.Printf); err != nil {
				return fmt.Errorf("the Firestore Admin API's state: %w", err)
			}
		}
		if *documentsDir != "" {
			if *exportInterval <= 0 {
				fmt.Fprintln(stderr, "call-front: --firestore-export-interval must be positive")
				return ErrUsage
			}
			if docs, err = fa.KeepDocuments(ctx, *documentsDir, *exportInterval, *documentsFresh, logger.Printf); err != nil {
				return fmt.Errorf("the Firestore emulator's documents: %w", err)
			}
		}
		fa.Start(ctx)
		// The emulator runs as an Enterprise edition one (#1703); the
		// guard keeps a Standard database's writes to Standard's values.
		// The access gate goes first (#1880, #1881): a Datastore mode
		// database's, or an inaccessible key's, data calls are refused
		// before anything else reads them.
		icpts = append(icpts, fa.AccessGate(), fa, fa.EditionGuard())
		if *mongoListen != "" {
			// TLS only, as Google requires (#1738): the certificate the
			// instance issued, in the front's environment.
			tlsCfg, err := firestoreadmin.MongoTLSConfig(os.Getenv(firestoreadmin.MongoTLSCertEnv), os.Getenv(firestoreadmin.MongoTLSKeyEnv))
			if err != nil {
				return fmt.Errorf("Firestore with MongoDB compatibility's TLS certificate (%s, %s): %w",
					firestoreadmin.MongoTLSCertEnv, firestoreadmin.MongoTLSKeyEnv, err)
			}
			ml, err := net.Listen("tcp", *mongoListen)
			if err != nil {
				return fmt.Errorf("listen on %s: %w", *mongoListen, err)
			}
			ml = tls.NewListener(ml, tlsCfg)
			// Its documents calls go through this front's own port, so
			// they are logged, gated and exported as every other call.
			_, port, _ := net.SplitHostPort(*listen)
			ms := fa.NewMongoServer(net.JoinHostPort("127.0.0.1", port), logger.Printf)
			go func() {
				if err := ms.Serve(ctx, ml); err != nil {
					logger.Printf("firestore mongodb: %v", err)
				}
			}()
			logger.Printf("call front: serving Firestore with MongoDB compatibility on %s", ml.Addr())
		}
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
		var h http.Handler = HandlerWith(p[1], ring, logger.Printf, icpt)
		if docs != nil {
			h = docs.Wrap(h)
		}
		servers = append(servers, Server(h, logger.Printf, *service))
		logger.Printf("call front: serving %s for the emulator at %s", l.Addr(), p[1])
	}
	if err := serveAll(ctx, servers, listeners, icpts); err != nil {
		return err
	}
	// The documents' last export, which the emulator's container waits
	// for before it stops the emulator (#1615).
	if docs != nil {
		<-docs.Done()
	}
	return nil
}
