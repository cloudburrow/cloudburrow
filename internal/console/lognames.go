package console

// The Logs Explorer's Log name picker (#1554), as Google's Logs Explorer
// lists the scope's log names in its Log name filter: Cloud Logging's
// LoggingServiceV2/ListLogs of the project, folder or organization the
// Explorer is scoped to. Picking one filters the entries to that log, whose
// full name is a Cloud Logging entry's resource.

import (
	"context"
	"net/http"
	"strings"
	"time"
)

// LogNameLister is Cloud Logging's ListLogs, called in process on the server
// the gRPC and JSON transports serve, with the instance's fault rules
// applied first.
type LogNameLister interface {
	// ListLogs returns parent's log names, projects/P/logs/ID and the like,
	// every page of them.
	ListLogs(ctx context.Context, parent string) ([]string, error)
}

// SetLogNameLister attaches Cloud Logging's ListLogs. Nil, the default,
// offers no Log name picker.
func (s *Server) SetLogNameLister(l LogNameLister) { s.logNames = l }

// handleLogNames is GET /api/logs/names?parent=: parent's log names.
func (s *Server) handleLogNames(w http.ResponseWriter, r *http.Request) {
	if s.logNames == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "Cloud Logging is not enabled on this instance, so it lists no log names"})
		return
	}
	parent := strings.TrimSuffix(strings.TrimSpace(r.URL.Query().Get("parent")), "/")
	if parent == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "parent is required: projects/P, folders/F, organizations/O or billingAccounts/B"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	names, err := s.logNames.ListLogs(ctx, parent)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": userMessage(err)})
		return
	}
	if names == nil {
		names = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"parent": parent, "logNames": names})
}
