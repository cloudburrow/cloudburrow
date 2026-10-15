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

	"github.com/cloudburrow/cloudburrow/internal/hostguard"
)

// Listen makes the server bindable and stoppable by the lifecycle coordinator,
// so it is owned like every other service rather than being a goroutine nobody
// tracks.
type listener struct {
	addr string

	mu   sync.Mutex
	ln   net.Listener
	srv  *http.Server
	done chan struct{}
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
	srv := &http.Server{Handler: s.mixedHandler(), ReadHeaderTimeout: 10 * time.Second}
	done := make(chan struct{})

	s.lis.mu.Lock()
	s.lis.ln, s.lis.srv, s.lis.done = ln, srv, done
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
func (s *Server) mixedHandler() http.Handler {
	g := s.newGRPCServer()
	jsonAPI := s.Handler()
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
	srv, done := s.lis.srv, s.lis.done
	s.lis.srv = nil
	s.lis.mu.Unlock()

	if srv == nil {
		return nil
	}
	err := srv.Shutdown(ctx)
	if err != nil {
		_ = srv.Close()
	}
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
		}
	}
	return err
}
