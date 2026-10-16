package console

import (
	"io"
	"net/http"

	"github.com/cloudburrow/cloudburrow/internal/localai"
)

// LocalModels downloads, loads, unloads and deletes local AI models for the
// Local models page (#1306). *localai.Manager implements it; the admin API
// drives the same one (#1396).
type LocalModels = localai.Actions

// LocalModelsNote is shown above the list.
const LocalModelsNote = "Only a model with a pinned revision and pinned SHA-256 digests is downloaded, " +
	"from that revision on Hugging Face, each file verified before it is kept — the same pinned " +
	"source -local-ai-embeddings fetches. Gated and unpinned models are listed and never downloaded. " +
	"Load and Unload change what the running endpoint serves; only the embedding runtime supports them."

// SetLocalModels configures the Local models page; nil leaves it off.
func (s *Server) SetLocalModels(m LocalModels) { s.localModels = m }

func (s *Server) handleLocalModels(w http.ResponseWriter, _ *http.Request) {
	if s.localModels == nil {
		writeJSON(w, http.StatusOK, map[string]any{"configured": false, "models": []any{},
			"note": "Local model management is not available on this instance."})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"configured": true,
		"models": s.localModels.States(), "note": LocalModelsNote})
}

// handleLocalModelAction runs download, cancel, load, unload or delete on
// one model, through localai.Act as the admin API does (#1396). A download's
// body is {"confirmBytes": N}, the size the user was shown and confirmed;
// anything else is refused before a byte is fetched.
func (s *Server) handleLocalModelAction(w http.ResponseWriter, r *http.Request) {
	if s.localModels == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "local model management is not available"})
		return
	}
	b, _ := io.ReadAll(io.LimitReader(r.Body, 4096))
	code, body := localai.Act(s.localModels, r.PathValue("id"), r.PathValue("action"), b)
	writeJSON(w, code, body)
}
