package console

import (
	"net/http"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/byoc"
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
	// RolledBack is a failed move whose rollback ran: the instance runs
	// where it ran before.
	RolledBack bool `json:"rolled_back,omitempty"`
	// Kind is "move" (or empty), or "replace": a VM changed for one of
	// another size family, the instance moved onto it.
	Kind string `json:"kind,omitempty"`
	// PID is the process running it.
	PID int `json:"pid,omitempty"`
	// Interrupted is a move that stopped before it was done or undone (its
	// process ended, or it was cut off): it resumes or rolls back.
	Interrupted bool `json:"interrupted,omitempty"`
}

// ComputeMover starts a move and reads its progress.
type ComputeMover interface {
	// StartMove starts moving instance onto the VM vm, or with "local"
	// back to this machine, in a process of its own.
	StartMove(instance, vm string) error
	// ResumeMove continues an interrupted move; RollbackMove undoes it.
	ResumeMove(instance string) error
	RollbackMove(instance string) error
	// StartReplace changes the VM vm, which instance runs on, for a new
	// one of size, moving the instance onto it and deleting vm.
	StartReplace(instance, vm, size string) error
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

// handleComputeMoveBack is Compute capacity's Move back to this machine:
// the instance leaves its VM (or host) for this machine's Docker.
func (s *Server) handleComputeMoveBack(w http.ResponseWriter, r *http.Request) {
	s.moveAction(w, r, "Move instance back to this machine", func(instance string) error { return s.mover.StartMove(instance, "local") })
}

// handleComputeMoveResume continues an interrupted move.
func (s *Server) handleComputeMoveResume(w http.ResponseWriter, r *http.Request) {
	s.moveAction(w, r, "Resume instance move", func(instance string) error { return s.mover.ResumeMove(instance) })
}

// handleComputeMoveRollback undoes an interrupted move.
func (s *Server) handleComputeMoveRollback(w http.ResponseWriter, r *http.Request) {
	s.moveAction(w, r, "Roll back instance move", func(instance string) error { return s.mover.RollbackMove(instance) })
}

func (s *Server) moveAction(w http.ResponseWriter, r *http.Request, kind string, f func(string) error) {
	if !s.computeReady(w, r) {
		return
	}
	if s.mover == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Move is not available on this console"})
		return
	}
	instance := r.PathValue("instance")
	err := f(instance)
	s.computeLog(kind, instance, err)
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	st, _ := s.mover.MoveStatus(instance)
	writeJSON(w, http.StatusAccepted, st)
}

// handleComputeResizePlan is GET /api/compute/machines/{vm}/resize-plan:
// what a size change does and costs, for the page to show before it asks.
func (s *Server) handleComputeResizePlan(w http.ResponseWriter, r *http.Request) {
	if !s.computeReady(w, r) {
		return
	}
	q := r.URL.Query()
	var p byoc.Preset
	var err error
	if q.Get("size") == "" {
		if p, err = byoc.ParsePreset(q.Get("preset")); err != nil {
			badRequest(w, err.Error())
			return
		}
	}
	plan, err := s.cloud.PlanResize(r.Context(), r.PathValue("instance"), p, q.Get("size"))
	if err != nil {
		computeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, plan)
}

// handleComputeReplace is the guided change across size families: a new
// VM, the instance moved onto it, the old one deleted, in a process of its
// own (it stops the up that serves this console).
func (s *Server) handleComputeReplace(w http.ResponseWriter, r *http.Request) {
	if !s.computeReady(w, r) {
		return
	}
	if s.mover == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Changing the VM is not available on this console"})
		return
	}
	var in struct {
		Preset   string `json:"preset"`
		Instance string `json:"instance"`
	}
	if !readJSONBody(w, r, &in) {
		return
	}
	vm := r.PathValue("instance")
	p, err := byoc.ParsePreset(in.Preset)
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	plan, err := s.cloud.PlanResize(r.Context(), vm, p, "")
	if err != nil {
		computeError(w, err)
		return
	}
	if !plan.NewVM {
		badRequest(w, vm+" resizes in place to "+plan.To.Name+": use Resize")
		return
	}
	if in.Instance == "" {
		in.Instance = s.cloudInstance
	}
	err = s.mover.StartReplace(in.Instance, vm, string(p))
	s.computeLog("Replace compute VM", vm, err)
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	st, _ := s.mover.MoveStatus(in.Instance)
	writeJSON(w, http.StatusAccepted, st)
}
