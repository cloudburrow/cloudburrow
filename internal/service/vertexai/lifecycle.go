package vertexai

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"
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
	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
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
