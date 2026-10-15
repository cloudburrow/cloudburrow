package console

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/cloudburrow/cloudburrow/internal/localai"
)

// LocalModels downloads, loads, unloads and deletes local AI models for the
// Local models page (#1306). *localai.Manager implements it.
type LocalModels interface {
	States() []localai.ModelState
	Download(id string, confirmBytes int64) error
	Cancel(id string) error
	Load(id string) error
	Unload(id string) error
	Delete(id string) error
}

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
// one model. A download's body is {"confirmBytes": N}, the size the user was
// shown and confirmed; anything else is refused before a byte is fetched.
func (s *Server) handleLocalModelAction(w http.ResponseWriter, r *http.Request) {
	if s.localModels == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "local model management is not available"})
		return
	}
	id := r.PathValue("id")
	var err error
	switch r.PathValue("action") {
	case "download":
		var body struct {
			ConfirmBytes int64 `json:"confirmBytes"`
		}
		b, _ := io.ReadAll(io.LimitReader(r.Body, 4096))
		if jerr := json.Unmarshal(b, &body); jerr != nil || body.ConfirmBytes <= 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "a download needs {\"confirmBytes\": N}, the size you confirmed"})
			return
		}
		err = s.localModels.Download(id, body.ConfirmBytes)
	case "cancel":
		err = s.localModels.Cancel(id)
	case "load":
		err = s.localModels.Load(id)
	case "unload":
		err = s.localModels.Unload(id)
	case "delete":
		err = s.localModels.Delete(id)
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown action " + r.PathValue("action")})
		return
	}
	if err != nil {
		writeJSON(w, localModelStatus(err), map[string]string{"error": err.Error()})
		return
	}
	for _, st := range s.localModels.States() {
		if st.ID == id {
			writeJSON(w, http.StatusOK, st)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": id})
}

func localModelStatus(err error) int {
	switch {
	case errors.Is(err, localai.ErrUnknownModel):
		return http.StatusNotFound
	case errors.Is(err, localai.ErrBusy):
		return http.StatusConflict
	case errors.Is(err, localai.ErrNotPinned), errors.Is(err, localai.ErrGated),
		errors.Is(err, localai.ErrNotLoadable), errors.Is(err, localai.ErrNotCached),
		errors.Is(err, localai.ErrChecksumMismatch):
		return http.StatusPreconditionFailed
	case errors.Is(err, localai.ErrSizeNotConfirmed):
		return http.StatusBadRequest
	}
	return http.StatusInternalServerError
}
