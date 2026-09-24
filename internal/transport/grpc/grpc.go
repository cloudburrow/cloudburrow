// Package grpc implements the gRPC server, interceptors and service
// registration for the APIs CloudBurrow serves itself.
//
// Protobuf service names are globally unique, so all gRPC surfaces share one
// port and dispatch is unambiguous by construction (ADR-0002, carried forward
// by ADR-0005).
package grpc

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/status"
)

// MaxMessageBytes bounds a single message. Unbounded messages let one client
// exhaust the process's memory.
const MaxMessageBytes = 32 << 20 // 32 MiB

// Server hosts CloudBurrow's gRPC surfaces.
type Server struct {
	addr string

	mu      sync.Mutex
	observe Observer
	srv     *grpc.Server
	ln      net.Listener
	done    chan struct{}
}

// New returns a server bound to addr when started.
func New(addr string) *Server { return &Server{addr: addr} }

func (s *Server) Name() string { return "grpc" }

// Register exposes the underlying server so services can register themselves.
//
// It must be called before Start: gRPC does not permit registration on a
// serving server, and allowing it would silently drop the service.
func (s *Server) Register(fn func(*grpc.Server)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln != nil {
		return errors.New("cannot register a service after the server has started")
	}
	if s.srv == nil {
		// The observer runs outermost, so the code it reports is the one the
		// caller receives — after errorInterceptor has turned an internal error
		// into a Google-style status — not the handler's raw error.
		unary := []grpc.UnaryServerInterceptor{}
		var stream []grpc.StreamServerInterceptor
		if s.observe != nil {
			unary = append(unary, UnaryObserver(s.observe))
			stream = append(stream, StreamObserver(s.observe))
		}
		unary = append(unary, errorInterceptor)
		s.srv = grpc.NewServer(
			grpc.MaxRecvMsgSize(MaxMessageBytes),
			grpc.MaxSendMsgSize(MaxMessageBytes),
			grpc.ChainUnaryInterceptor(unary...),
			grpc.ChainStreamInterceptor(stream...),
		)
		// Reflection lets grpcurl and the SDKs introspect the surface, which
		// makes "what does this actually implement?" answerable.
		reflection.Register(s.srv)
	}
	fn(s.srv)
	return nil
}

// errorInterceptor converts internal errors to gRPC statuses.
//
// Without it an unmapped error reaches the client as Unknown, which tells the
// caller nothing about whether to retry.
// Observe sets a function told about every completed call. It must be set
// before Register, because the interceptors are fixed when the server is
// built.
func (s *Server) Observe(o Observer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.observe = o
}

func errorInterceptor(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	resp, err := handler(ctx, req)
	if err == nil {
		return resp, nil
	}
	// Already a status: pass it through untouched.
	if _, ok := status.FromError(err); ok && status.Code(err) != codes.Unknown {
		return nil, err
	}
	e := apierror.From(err)
	return nil, status.Error(e.Code, e.Message)
}

// Addr returns the resolved listen address, or "" before Start.
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

// Start binds the listener and serves. It does not block.
func (s *Server) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.srv == nil {
		// Nothing registered: a server with no services would accept
		// connections and answer every call Unimplemented, which looks like a
		// broken deployment rather than an unconfigured one.
		s.mu.Unlock()
		return errors.New("no services registered")
	}
	srv := s.srv
	s.mu.Unlock()

	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", s.addr)
	if err != nil {
		return fmt.Errorf("bind gRPC %s: %w", s.addr, err)
	}
	done := make(chan struct{})

	s.mu.Lock()
	s.ln, s.done = ln, done
	s.mu.Unlock()

	go func() {
		defer close(done)
		_ = srv.Serve(ln)
	}()
	return nil
}

// Stop gracefully stops the server, falling back to a hard stop at the
// deadline so shutdown stays bounded.
func (s *Server) Stop(ctx context.Context) error {
	s.mu.Lock()
	srv, done := s.srv, s.done
	s.ln = nil
	s.mu.Unlock()

	if srv == nil || done == nil {
		return nil
	}
	stopped := make(chan struct{})
	go func() {
		srv.GracefulStop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-ctx.Done():
		srv.Stop()
	}
	<-done
	return nil
}
