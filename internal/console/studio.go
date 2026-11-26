package console

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// Studio is a provider with a workspace screen of its own (#1861), such as
// BigQuery Studio: an explorer tree, a query editor whose runs report a job,
// and the job histories. Its calls do not fit the list, detail and query
// routes, which answer one listing each, so the screen sends them to
// POST /api/studio/{service}?call=<name> with a JSON body the provider reads.
//
// Every call is recorded in the operations ledger as "studio <call>", and,
// like a query, without its body: a statement is the user's text.
type Studio interface {
	// StudioCall answers one call. An unknown call is an error.
	StudioCall(ctx context.Context, project, call string, body json.RawMessage) (any, error)
}

// StudioTimeouter is a Studio whose call may run longer than a read: Cloud
// SQL Studio's run, which Google cancels after five minutes (#2208).
type StudioTimeouter interface {
	StudioTimeout(call string) time.Duration
}

func (s *Server) handleStudio(w http.ResponseWriter, r *http.Request) {
	p, ok := s.providers[r.PathValue("service")]
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such service"})
		return
	}
	studio, ok := p.(Studio)
	if !ok {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": p.Title() + " has no studio"})
		return
	}
	call := strings.TrimSpace(r.URL.Query().Get("call"))
	if call == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "a call is required"})
		return
	}
	var body json.RawMessage
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<17)).Decode(&body); err != nil {
		body = json.RawMessage("{}")
	}
	ctx, cancel := context.WithTimeout(r.Context(), readBudget)
	if t, ok := p.(StudioTimeouter); ok {
		if d := t.StudioTimeout(call); d > 0 {
			cancel()
			ctx, cancel = context.WithTimeout(r.Context(), d)
		}
	}
	defer cancel()
	project := r.URL.Query().Get("project")
	opID := s.logs.StartOperation("studio "+call, p.Title(), project)
	out, err := studio.StudioCall(ctx, project, call, body)
	if err != nil {
		s.logs.FinishOperation(opID, OperationFailed, userMessage(err))
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": userMessage(err), "operation": opID})
		return
	}
	s.logs.FinishOperation(opID, OperationSucceeded, "")
	writeJSON(w, http.StatusOK, out)
}
