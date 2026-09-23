package secrets

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
	"google.golang.org/grpc"

	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// Server serves Secret Manager over gRPC and JSON on one port.
//
// Google's endpoint answers both on `secretmanager.googleapis.com:443`, and a
// developer who points a REST client and a gRPC client at different ports
// would find CloudBurrow's shape does not match the service it emulates.
// Both surfaces share one Store, so there is one implementation of the
// contract rather than two that can drift.
//
// gRPC is served through grpc.Server.ServeHTTP rather than its own listener.
// That path is documented upstream as lower-performance than grpc.Serve; for
// a local development endpoint, matching the real service's shape is worth
// more than throughput.
type Server struct {
	addr  string
	store *Store

	mu   sync.Mutex
	ln   net.Listener
	srv  *http.Server
	grpc *grpc.Server
	done chan struct{}
}

// NewServer returns a Secret Manager server bound to addr.
func NewServer(addr string, store *Store) *Server {
	return &Server{addr: addr, store: store}
}

func (s *Server) Name() string { return "secretmanager" }

// Addr returns the resolved listen address, or "" before Start.
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

// handler routes a request to whichever surface it belongs to.
func (s *Server) handler(grpcSrv *grpc.Server) http.Handler {
	router := rest.NewRouter()
	NewRESTServer(s.store).Routes(router)

	both := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A gRPC request is HTTP/2 with an application/grpc content type.
		// Checking both matters: an HTTP/2 JSON client must still reach the
		// REST router.
		if r.ProtoMajor == 2 && strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") {
			grpcSrv.ServeHTTP(w, r)
			return
		}
		router.ServeHTTP(w, r)
	})

	// h2c allows prior-knowledge HTTP/2 without TLS, which is what a gRPC
	// client using insecure credentials speaks.
	return h2c.NewHandler(both, &http2.Server{})
}

// Start binds the listener and begins serving. It does not block.
func (s *Server) Start(ctx context.Context) error {
	grpcSrv := grpc.NewServer()
	NewGRPCServer(s.store).Register(grpcSrv)

	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", s.addr)
	if err != nil {
		return fmt.Errorf("bind Secret Manager address %s: %w", s.addr, err)
	}

	srv := &http.Server{Handler: s.handler(grpcSrv), ReadHeaderTimeout: 10 * time.Second}
	done := make(chan struct{})

	s.mu.Lock()
	s.ln, s.srv, s.grpc, s.done = ln, srv, grpcSrv, done
	s.mu.Unlock()

	go func() {
		defer close(done)
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			_ = err
		}
	}()
	return nil
}

// Stop shuts the server down within ctx's deadline.
func (s *Server) Stop(ctx context.Context) error {
	s.mu.Lock()
	srv, grpcSrv, done := s.srv, s.grpc, s.done
	s.srv = nil
	s.mu.Unlock()

	if srv == nil {
		return nil
	}
	err := srv.Shutdown(ctx)
	if err != nil {
		_ = srv.Close()
	}
	// The gRPC server holds no listener of its own here, but it does hold
	// in-flight streams; stopping it releases them.
	if grpcSrv != nil {
		grpcSrv.Stop()
	}
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
		}
	}
	return err
}
