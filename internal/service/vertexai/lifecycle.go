package vertexai

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

	"github.com/cloudburrow/cloudburrow/internal/grpchttp"
	"github.com/cloudburrow/cloudburrow/internal/hostguard"
)

// Listen makes the server bindable and stoppable by the lifecycle coordinator,
// so it is owned like every other service rather than being a goroutine nobody
// tracks.
type listener struct {
	addr string

	// options are extra gRPC server options (tests: a stats handler; IAM
	// enforcement's interceptors, UseIAM).
	options []grpc.ServerOption
	// wrapJSON wraps the JSON routes (IAM enforcement, #1879); nil none.
	wrapJSON func(http.Handler) http.Handler

	mu   sync.Mutex
	ln   net.Listener
	srv  *http.Server
	grpc *grpc.Server
	done chan struct{}
}

// UseIAM checks every call with IAM enforcement's interceptors and JSON
// wrapper (#1879). It is called before Start.
func (s *Server) UseIAM(unary []grpc.UnaryServerInterceptor, stream []grpc.StreamServerInterceptor, wrapJSON func(http.Handler) http.Handler) {
	s.lis.options = append(s.lis.options, grpc.ChainUnaryInterceptor(unary...), grpc.ChainStreamInterceptor(stream...))
	s.lis.wrapJSON = wrapJSON
}

// Name implements the lifecycle component interface.
func (s *Server) Name() string { return "localai" }

// Addr reports the bound address, empty until Start succeeds.
func (s *Server) Addr() string {
	s.lis.mu.Lock()
	defer s.lis.mu.Unlock()
	if s.lis.ln == nil {
		return ""
	}
	return s.lis.ln.Addr().String()
}

// Start binds the listener and serves. It does not block.
func (s *Server) Start(ctx context.Context) error {
	if s.lis.addr == "" {
		return errors.New("no address configured for the local generation endpoint")
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", s.lis.addr)
	if err != nil {
		return fmt.Errorf("bind local AI address %s: %w", s.lis.addr, err)
	}

	// No write timeout: a generation can legitimately run for minutes, and a
	// deadline here would cut a stream mid-token for no reason the caller
	// could see. The read header timeout still bounds a stalled client.
	g := s.newGRPCServer()
	srv := &http.Server{Handler: s.mixedHandler(g), ReadHeaderTimeout: 10 * time.Second}
	done := make(chan struct{})

	s.lis.mu.Lock()
	s.lis.ln, s.lis.srv, s.lis.grpc, s.lis.done = ln, srv, g, done
	s.lis.mu.Unlock()

	go func() {
		defer close(done)
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			_ = err
		}
	}()
	return nil
}

// mixedHandler serves gRPC and JSON on one port, as the main gRPC port does
// for Secret Manager and KMS: HTTP/2 with an application/grpc content type
// goes to PredictionService, everything else to the JSON routes. h2c accepts
// the prior-knowledge HTTP/2 an insecure gRPC client speaks.
func (s *Server) mixedHandler(g *grpc.Server) http.Handler {
	jsonAPI := s.Handler()
	if s.lis.wrapJSON != nil {
		jsonAPI = s.lis.wrapJSON(jsonAPI)
	}
	grpcAPI := hostguard.Wrap(g)
	both := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor == 2 && strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") {
			grpcAPI.ServeHTTP(w, r)
			return
		}
		jsonAPI.ServeHTTP(w, r)
	})
	return h2c.NewHandler(both, &http2.Server{})
}

// Stop shuts the endpoint down.
func (s *Server) Stop(ctx context.Context) error {
	s.lis.mu.Lock()
	srv, g, done := s.lis.srv, s.lis.grpc, s.lis.done
	s.lis.srv, s.lis.grpc = nil, nil
	s.lis.mu.Unlock()

	if srv == nil {
		return nil
	}
	err := srv.Shutdown(ctx)
	if err != nil {
		_ = srv.Close()
	}
	// h2c hijacks the connection, so Shutdown waits for no gRPC call;
	// stopping g ends them and waits for their handlers, where a call's
	// stats end, until ctx ends (#1804).
	_ = grpchttp.Stop(ctx, g)
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
		}
	}
	return err
}
