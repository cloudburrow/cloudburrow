package admin

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// Project purges (#1537).
//
// Google purges a deleted project 30 days after DeleteProject and frees its
// ID, and a project created again under it starts empty. POST
// /admin/purge?project= purges one now, in place of the wait, so a test can
// reuse an ID; the project must be pending deletion. A purge, whenever it
// happens, deletes the project's state in every component that can scope a
// reset to one project (PurgeProject), as a project-scoped reset does.

// SetPurger sets what POST /admin/purge calls: the project registry's purge,
// which deletes the record and then calls PurgeProject.
func (a *API) SetPurger(fn func(ctx context.Context, project string) error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.purger = fn
}

// PurgeProject deletes one project's state in every registered component
// that can scope a reset to one project, those named in skip apart (the
// registry, which purges its own). Every one is attempted; the failures are
// returned by component.
func (a *API) PurgeProject(ctx context.Context, project string, skip ...string) map[string]error {
	failed := map[string]error{}
	if project == "" {
		return failed
	}
	skipped := map[string]bool{}
	for _, s := range skip {
		skipped[s] = true
	}
	for _, c := range a.resets {
		pr, ok := c.(ProjectResetter)
		if !ok || skipped[c.Name()] {
			continue
		}
		if err := pr.ResetProject(ctx, project); err != nil {
			failed[c.Name()] = err
		}
	}
	return failed
}

func (a *API) handlePurge(w http.ResponseWriter, r *http.Request) {
	project := strings.TrimSpace(r.URL.Query().Get("project"))
	if project == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "project= is required"})
		return
	}
	a.mu.Lock()
	purge := a.purger
	a.mu.Unlock()
	if purge == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "this instance has no project registry to purge from"})
		return
	}
	if err := purge(r.Context(), project); err != nil {
		code := http.StatusInternalServerError
		var e *apierror.Error
		if errors.As(err, &e) {
			code = e.HTTPStatus()
		}
		writeJSON(w, code, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"purged": project})
}
