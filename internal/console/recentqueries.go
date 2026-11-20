package console

// The Query library's Recent queries (#2031), as Google keeps them: per
// user, through Cloud Logging's projects.locations.recentQueries.list, the
// Logs Explorer and Log Analytics recording each query they run. The
// console serves its pages as the instance's own account, so these are
// that account's recent queries, which a client with the instance's
// credentials lists too.

import (
	"encoding/json"
	"net/http"
	"strings"
)

// RecentQuery is one of the user's recent queries.
type RecentQuery struct {
	Name        string `json:"name"`
	Query       string `json:"query"`
	SQL         bool   `json:"sql,omitempty"`
	LastRunTime string `json:"lastRunTime"`
}

// RecentQueries records and lists the user's recent queries in a project.
type RecentQueries interface {
	// Record notes that the user ran query (Log Analytics SQL when sql).
	Record(project, query string, sql bool) error
	// List is recentQueries.list of the project's global location, most
	// recent first, of one kind.
	List(project string, sql bool) ([]RecentQuery, error)
}

// SetRecentQueries attaches Cloud Logging's recent queries. Nil, the
// default, keeps none.
func (s *Server) SetRecentQueries(r RecentQueries) { s.recentQueries = r }

// handleRecentQueries is GET /api/logs/recent-queries?project=&type=sql|logging,
// the user's recent queries of that kind, and POST with {query, sql}, which
// records one.
func (s *Server) handleRecentQueries(w http.ResponseWriter, r *http.Request) {
	if s.recentQueries == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "Cloud Logging is not enabled on this instance, so it keeps no recent queries"})
		return
	}
	project := strings.TrimSpace(r.URL.Query().Get("project"))
	if project == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "project is required"})
		return
	}
	if r.Method == http.MethodPost {
		var req struct {
			Query string `json:"query"`
			SQL   bool   `json:"sql"`
		}
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if err := s.recentQueries.Record(project, req.Query, req.SQL); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": userMessage(err)})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{})
		return
	}
	list, err := s.recentQueries.List(project, r.URL.Query().Get("type") == "sql")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": userMessage(err)})
		return
	}
	if list == nil {
		list = []RecentQuery{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"recentQueries": list})
}
