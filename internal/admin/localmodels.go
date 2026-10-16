package admin

import (
	"io"
	"net/http"

	"github.com/cloudburrow/cloudburrow/internal/localai"
)

// SetLocalModels makes /admin/local-models manage m (#1396): the local AI
// models' state, and download, cancel, load, unload and delete, for a script
// or the CLI with the admin token, as the console's Local models page offers
// them. The two drive one manager, so a download started on either is seen
// on both. nil leaves the routes answering 404.
func (a *API) SetLocalModels(m localai.Actions) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.localModels = m
}

func (a *API) models() localai.Actions {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.localModels
}

func (a *API) handleLocalModels(w http.ResponseWriter, _ *http.Request) {
	m := a.models()
	if m == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "local model management is not available on this instance"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": m.States(), "actions": localai.ActionNames})
}

// handleLocalModelAction is POST /admin/local-models/{id}/{action}. A
// download needs {"confirmBytes": N}, the model's exact pinned size, and
// verifies every file against its pinned SHA-256 before keeping it.
func (a *API) handleLocalModelAction(w http.ResponseWriter, r *http.Request) {
	m := a.models()
	if m == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "local model management is not available on this instance"})
		return
	}
	b, _ := io.ReadAll(io.LimitReader(r.Body, 4096))
	code, body := localai.Act(m, r.PathValue("id"), r.PathValue("action"), b)
	writeJSON(w, code, body)
}
