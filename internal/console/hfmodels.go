package console

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/cloudburrow/cloudburrow/internal/hfmodels"
)

// Hugging Face models in Model Garden (#1840): look a model up on
// huggingface.co (its licence, gated status, revision and size), download it
// into the instance's model cache with its progress, and delete it to free
// the space. The token a gated model needs is the HF_TOKEN environment
// variable of the CloudBurrow process; nothing here accepts, stores or
// returns one.

// HFNote is shown above the Hugging Face section.
const HFNote = "Downloads come from huggingface.co at the model's current commit, each LFS file checked " +
	"against its SHA-256, into this instance's model cache. A download is refused when it would leave " +
	"less than 50 GB free. A gated model needs its licence accepted on huggingface.co and HF_TOKEN set in " +
	"the environment CloudBurrow runs in; the console never asks for the token. Serving a downloaded " +
	"model on a local endpoint is not available yet (#1840), so no endpoint is created."

// SetHFModels configures the Hugging Face section; nil leaves it off.
func (s *Server) SetHFModels(m *hfmodels.Manager) { s.hfModels = m }

func (s *Server) handleHFModels(w http.ResponseWriter, _ *http.Request) {
	if s.hfModels == nil {
		writeJSON(w, http.StatusOK, map[string]any{"configured": false, "models": []any{},
			"note": "Hugging Face downloads are not available on this instance."})
		return
	}
	free, floor := s.hfModels.Free()
	writeJSON(w, http.StatusOK, map[string]any{"configured": true, "models": s.hfModels.States(),
		"free": free, "floor": floor, "note": HFNote})
}

func (s *Server) handleHFLookup(w http.ResponseWriter, r *http.Request) {
	if s.hfModels == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Hugging Face downloads are not available"})
		return
	}
	info, err := s.hfModels.Lookup(r.Context(), r.URL.Query().Get("id"))
	if err != nil {
		writeJSON(w, hfStatus(err), map[string]string{"error": err.Error()})
		return
	}
	free, floor := s.hfModels.Free()
	writeJSON(w, http.StatusOK, map[string]any{"model": info, "free": free, "floor": floor})
}

func (s *Server) handleHFAction(w http.ResponseWriter, r *http.Request) {
	if s.hfModels == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Hugging Face downloads are not available"})
		return
	}
	var req struct {
		ID string `json:"id"`
	}
	b, _ := io.ReadAll(io.LimitReader(r.Body, 4096))
	if json.Unmarshal(b, &req) != nil || req.ID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": `want {"id": "{author}/{model}"}`})
		return
	}
	var err error
	switch r.PathValue("action") {
	case "download":
		_, err = s.hfModels.Start(r.Context(), req.ID)
	case "cancel":
		err = s.hfModels.Cancel(req.ID)
	case "delete":
		err = s.hfModels.Delete(req.ID)
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such action"})
		return
	}
	if err != nil {
		writeJSON(w, hfStatus(err), map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": s.hfModels.States()})
}

func hfStatus(err error) int {
	switch {
	case errors.Is(err, hfmodels.ErrBadID):
		return http.StatusBadRequest
	case errors.Is(err, hfmodels.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, hfmodels.ErrGated), errors.Is(err, hfmodels.ErrDiskSpace):
		return http.StatusPreconditionFailed
	case errors.Is(err, hfmodels.ErrBusy):
		return http.StatusConflict
	}
	return http.StatusBadGateway
}
