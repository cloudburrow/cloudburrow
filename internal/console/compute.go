package console

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

// Where the instance's compute runs (#2209): this machine's Docker, or a
// remote host reached over SSH. The Instance page shows it, with the
// host's size and the connection's state, and offers Reconnect, which
// like every other action there needs the admin token.

// ComputeSource reports the instance's compute placement and reconnects
// its SSH connection. cmd/cloudburrow implements it over
// internal/remotecompute.
type ComputeSource interface {
	// Compute is the placement as JSON: location ("local" or "ssh"),
	// target, host size, connected, reconnects and recent events.
	Compute() any
	// Reconnect drops the SSH connection and waits for a new one.
	Reconnect(ctx context.Context) error
}

// SetCompute attaches the compute source; without one the page says the
// compute is this machine's Docker.
func (s *Server) SetCompute(src ComputeSource) { s.computeSrc = src }

// handleCompute is GET /api/instance/compute.
func (s *Server) handleCompute(w http.ResponseWriter, r *http.Request) {
	var v any = map[string]any{"location": "local", "connected": true}
	if s.computeSrc != nil {
		v = s.computeSrc.Compute()
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// handleComputeReconnect is POST /api/instance/compute/reconnect.
func (s *Server) handleComputeReconnect(w http.ResponseWriter, r *http.Request) {
	if !s.instanceWired(w) || !s.instanceAuthorized(w, r) {
		return
	}
	if s.computeSrc == nil {
		writeJSON(w, http.StatusPreconditionFailed, map[string]string{"error": "this instance's compute is this machine's Docker; there is no connection to reconnect"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	done := s.instanceOperation("Reconnect compute host", "compute", "")
	if err := s.computeSrc.Reconnect(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": userMessage(err), "operation": done(err, "")})
		return
	}
	done(nil, "reconnected")
	s.handleCompute(w, r)
}
