package localai

import (
	"encoding/json"
	"errors"
	"net/http"
)

// Actions downloads, cancels, loads, unloads and deletes local models.
// *Manager implements it; the console's Local models page and the admin API
// (#1396) both drive one through Act, so the two answer alike.
type Actions interface {
	States() []ModelState
	Download(id string, confirmBytes int64) error
	Cancel(id string) error
	Load(id string) error
	Unload(id string) error
	Delete(id string) error
}

// ActionNames are the actions Act runs.
var ActionNames = []string{"download", "cancel", "load", "unload", "delete"}

// Act runs one action on one model and returns the HTTP status and the JSON
// body to answer with: the model's state after it, or {"error": ...}. A
// download's body is {"confirmBytes": N}, the size the caller was shown and
// confirmed; anything else is refused before a byte is fetched, and the
// download itself verifies every file against its pinned SHA-256.
func Act(m Actions, id, action string, body []byte) (int, any) {
	var err error
	switch action {
	case "download":
		var req struct {
			ConfirmBytes int64 `json:"confirmBytes"`
		}
		if jerr := json.Unmarshal(body, &req); jerr != nil || req.ConfirmBytes <= 0 {
			return http.StatusBadRequest, map[string]string{
				"error": "a download needs {\"confirmBytes\": N}, the size you confirmed"}
		}
		err = m.Download(id, req.ConfirmBytes)
	case "cancel":
		err = m.Cancel(id)
	case "load":
		err = m.Load(id)
	case "unload":
		err = m.Unload(id)
	case "delete":
		err = m.Delete(id)
	default:
		return http.StatusNotFound, map[string]string{"error": "unknown action " + action}
	}
	if err != nil {
		return StatusOf(err), map[string]string{"error": err.Error()}
	}
	for _, st := range m.States() {
		if st.ID == id {
			return http.StatusOK, st
		}
	}
	return http.StatusOK, map[string]string{"id": id}
}

// StatusOf maps a Manager error to its HTTP status.
func StatusOf(err error) int {
	switch {
	case errors.Is(err, ErrUnknownModel):
		return http.StatusNotFound
	case errors.Is(err, ErrBusy):
		return http.StatusConflict
	case errors.Is(err, ErrNotPinned), errors.Is(err, ErrGated),
		errors.Is(err, ErrNotLoadable), errors.Is(err, ErrNotCached),
		errors.Is(err, ErrChecksumMismatch):
		return http.StatusPreconditionFailed
	case errors.Is(err, ErrSizeNotConfirmed):
		return http.StatusBadRequest
	}
	return http.StatusInternalServerError
}
