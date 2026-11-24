package console

import (
	"net/http"
	"time"
)

// Moving an instance onto a VM (#2209): Compute capacity's Move. The move
// stops this instance's `up`, which serves this console, so it runs in a
// process of its own (`cloudburrow cloud move`), which writes its progress
// to the instance's state; the page follows it across the restart.

// MoveStep is one line of a move's progress.
type MoveStep struct {
	Time    time.Time `json:"time"`
	Step    string    `json:"step"`
	Message string    `json:"message"`
}

// MoveStatus is a move's progress, as `cloudburrow cloud move
// --progress-file` writes it.
type MoveStatus struct {
	Instance string     `json:"instance"`
	To       string     `json:"to"`
	Started  time.Time  `json:"started"`
	Steps    []MoveStep `json:"steps"`
	Done     bool       `json:"done"`
	Error    string     `json:"error,omitempty"`
	// RolledBack is a failed move whose rollback ran: the instance runs on
	// this machine as before.
	RolledBack bool `json:"rolled_back,omitempty"`
}

// ComputeMover starts a move and reads its progress.
type ComputeMover interface {
	// StartMove starts moving instance onto the VM vm, in a process of its
	// own.
	StartMove(instance, vm string) error
	// MoveStatus is instance's last move, if it has one.
	MoveStatus(instance string) (MoveStatus, bool)
}

// SetMover attaches Move.
func (s *Server) SetMover(m ComputeMover) { s.mover = m }

func (s *Server) handleComputeMove(w http.ResponseWriter, r *http.Request) {
	if !s.computeReady(w, r) {
		return
	}
	if s.mover == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Move is not available on this console"})
		return
	}
	var in struct {
		Instance string `json:"instance"`
	}
	if !readJSONBody(w, r, &in) {
		return
	}
	vm := r.PathValue("instance")
	if in.Instance == "" {
		in.Instance = s.cloudInstance
	}
	if _, err := s.cloud.Machine(vm); err != nil {
		computeError(w, err)
		return
	}
	err := s.mover.StartMove(in.Instance, vm)
	s.computeLog("Move instance to compute VM", vm, err)
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	st, _ := s.mover.MoveStatus(in.Instance)
	writeJSON(w, http.StatusAccepted, st)
}

func (s *Server) handleComputeMoveStatus(w http.ResponseWriter, r *http.Request) {
	if !s.computeReady(w, r) {
		return
	}
	if s.mover == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Move is not available on this console"})
		return
	}
	st, ok := s.mover.MoveStatus(r.PathValue("instance"))
	if !ok {
		// No move is an answer, not an error: the page asks on each load.
		writeJSON(w, http.StatusOK, map[string]any{"instance": r.PathValue("instance"), "none": true, "steps": []MoveStep{}})
		return
	}
	writeJSON(w, http.StatusOK, st)
}
