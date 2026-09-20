package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"
)

// ReadinessSource reports what actually initialised. *Coordinator implements it.
type ReadinessSource interface {
	Ready() bool
	State() State
	SortedReady() ([]string, map[string]bool)
	Failure() error
}

// ControlServer serves health and readiness on the control port.
//
// It always binds loopback, regardless of the configured bind address: the
// control port is where admin endpoints land (issue #18), and reset destroys
// data, so it must be unreachable from the container network and the LAN. See
// docs/adr/0004.
type ControlServer struct {
	port   int
	source ReadinessSource

	mu   sync.Mutex
	ln   net.Listener
	srv  *http.Server
	done chan struct{}
}

// NewControlServer returns a control server for the given port. A port of 0
// requests an OS-assigned port; read the result from Addr after Start.
func NewControlServer(port int, source ReadinessSource) *ControlServer {
	return &ControlServer{port: port, source: source}
}

func (s *ControlServer) Name() string { return "control" }

// Addr returns the resolved listen address, or "" before Start.
func (s *ControlServer) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

// Port returns the resolved TCP port, or 0 before Start.
func (s *ControlServer) Port() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln == nil {
		return 0
	}
	if tcp, ok := s.ln.Addr().(*net.TCPAddr); ok {
		return tcp.Port
	}
	return 0
}

// Start binds the control listener and begins serving. It does not block.
func (s *ControlServer) Start(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/readyz", s.handleReady)

	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", net.JoinHostPort("127.0.0.1", fmt.Sprint(s.port)))
	if err != nil {
		return fmt.Errorf("bind control port %d: %w", s.port, err)
	}

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	done := make(chan struct{})

	s.mu.Lock()
	s.ln, s.srv, s.done = ln, srv, done
	s.mu.Unlock()

	go func() {
		defer close(done)
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			// Serve errors after Stop are expected; anything else is reported
			// through readiness rather than crashing the process.
			_ = err
		}
	}()
	return nil
}

// Stop gracefully shuts the control server down within ctx's deadline.
func (s *ControlServer) Stop(ctx context.Context) error {
	s.mu.Lock()
	srv, done := s.srv, s.done
	s.srv = nil
	s.mu.Unlock()

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

// handleHealth reports process liveness. It is deliberately not readiness: a
// process that is alive but failed startup answers 200 here and 503 on
// /readyz, which is what lets a caller tell the two apart.
func (s *ControlServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

type readyResponse struct {
	Ready      bool            `json:"ready"`
	State      string          `json:"state"`
	Components map[string]bool `json:"components"`
	Error      string          `json:"error,omitempty"`
}

// handleReady reports actual initialisation, component by component.
//
// It returns 503 unless every component started, so a caller waiting for
// readiness never proceeds against a half-initialised process.
func (s *ControlServer) handleReady(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	names, ready := s.source.SortedReady()
	components := make(map[string]bool, len(names))
	for _, n := range names {
		components[n] = ready[n]
	}

	resp := readyResponse{
		Ready:      s.source.Ready(),
		State:      s.source.State().String(),
		Components: components,
	}
	if err := s.source.Failure(); err != nil {
		resp.Error = err.Error()
	}

	status := http.StatusServiceUnavailable
	if resp.Ready {
		status = http.StatusOK
	}
	writeJSON(w, status, resp)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
