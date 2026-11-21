package admin

import (
	"encoding/json"
	"net/http"

	"github.com/cloudburrow/cloudburrow/internal/ratequota"
)

// Request quotas (#2063): GET /admin/quotas lists the per-minute request
// quotas this instance enforces and the limits in force; PUT sets one,
// {"metric": "parametermanager.googleapis.com/read_requests", "project":
// "p", "limit": 3}, for one project or, without "project", every project;
// DELETE /admin/quotas?metric=...&project=... restores the default (no
// metric: every metric's, for that project or for every project). A test
// lowers a limit in a project of its own, so no other test's calls are
// refused.

// SetQuotas sets the instance's request quotas; nil serves none.
func (a *API) SetQuotas(l *ratequota.Limiter) { a.quotas = l }

type quotaRule struct {
	Metric  string `json:"metric"`
	Project string `json:"project,omitempty"`
	Limit   *int64 `json:"limit"`
}

func (a *API) quotaRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/quotas", a.authorized(func(w http.ResponseWriter, _ *http.Request) {
		if a.quotas == nil {
			writeJSON(w, http.StatusOK, map[string]any{"quotas": []ratequota.Quota{}})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"quotas": a.quotas.Quotas()})
	}))
	mux.HandleFunc("PUT /admin/quotas", a.authorized(func(w http.ResponseWriter, r *http.Request) {
		if a.quotas == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "this instance enforces no request quotas"})
			return
		}
		var q quotaRule
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&q); err != nil || q.Limit == nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": `body must be {"metric": ..., "project": ..., "limit": N}`})
			return
		}
		if err := a.quotas.Set(q.Metric, q.Project, *q.Limit); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"quotas": a.quotas.Quotas()})
	}))
	mux.HandleFunc("DELETE /admin/quotas", a.authorized(func(w http.ResponseWriter, r *http.Request) {
		if a.quotas != nil {
			a.quotas.Clear(r.URL.Query().Get("metric"), r.URL.Query().Get("project"))
		}
		w.WriteHeader(http.StatusNoContent)
	}))
}
