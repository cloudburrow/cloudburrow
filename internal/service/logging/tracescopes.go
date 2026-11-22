package logging

// Trace scopes (#2108): observability.googleapis.com v1
// projects.locations.traceScopes create, get, list, patch and delete, as
// its discovery document describes them and
// docs.cloud.google.com/trace/docs/trace-scope/create-and-manage documents
// them (read 2026-11-21):
//
//   - a TraceScope has name, resourceNames ("Names of the projects that are
//     included in this trace scope. projects/[PROJECT_ID]. A trace scope can
//     include a maximum of 20 projects."), description ("The maximum length
//     of the description is 8000 characters."), createTime and updateTime;
//   - every project has _Default, holding the project itself: "You can't
//     modify the trace scope named _Default" and "You can't delete the trace
//     scope named _Default";
//   - a project holds at most 100 trace scopes (the page's limits table);
//   - "The name of a trace scope can't be modified, and it must be unique
//     within the project";
//   - the project's Observability scope (observability.go) names its default
//     trace scope, which must be one the project holds.
//
// Trace scopes decide which projects' traces the Trace Explorer searches:
// the console's Trace Explorer, over the local Cloud Trace (#2118),
// searches every project of the chosen scope.
//
// Choices Google does not document (UNVERIFIED (docs re-read 2026-11-21:
// https://docs.cloud.google.com/trace/docs/trace-scope/create-and-manage)):
// the location must be global (the only one the reference's names show); a
// trace scope ID is 1-100 letters, digits, - and _, not starting with _;
// a resource name that is not projects/{project} of a project the instance
// holds is INVALID_ARGUMENT, an existing ID ALREADY_EXISTS, a 101st scope
// RESOURCE_EXHAUSTED, a change to _Default FAILED_PRECONDITION; and deleting
// the project's default trace scope makes _Default the default again, as a
// deleted default log scope does.

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"google.golang.org/grpc/codes"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/paging"
	"github.com/cloudburrow/cloudburrow/internal/store"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// TraceScope is an Observability API TraceScope resource.
type TraceScope struct {
	Name          string   `json:"name,omitempty"`
	ResourceNames []string `json:"resourceNames,omitempty"`
	Description   string   `json:"description,omitempty"`
	CreateTime    string   `json:"createTime,omitempty"`
	UpdateTime    string   `json:"updateTime,omitempty"`
}

const (
	// MaxTraceScopes is how many trace scopes a project holds.
	MaxTraceScopes = 100
	// MaxTraceScopeProjects is how many projects one trace scope includes.
	MaxTraceScopeProjects = 20
	maxTraceScopeDesc     = 8000
)

var traceScopeIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_\-]{0,99}$`)

func traceScopePrefix(project string) string {
	return "observability/tracescopes/" + hex.EncodeToString([]byte(project)) + "/"
}

func traceScopeName(project, id string) string {
	return "projects/" + project + "/locations/global/traceScopes/" + id
}

func defaultTraceScope(project string) TraceScope {
	epoch := time.Unix(0, 0).UTC().Format(time.RFC3339Nano)
	return TraceScope{Name: traceScopeName(project, DefaultScopeID), ResourceNames: []string{"projects/" + project},
		Description: "Default trace scope", CreateTime: epoch, UpdateTime: epoch}
}

// serveTraceScopes serves projects/{project}/locations/{loc}/traceScopes[/{id}].
func (a *Admin) serveTraceScopes(w http.ResponseWriter, r *http.Request, project, loc, id string) error {
	if loc != "global" {
		return apierror.InvalidArgument("trace scopes are kept in the global location, not %q", loc)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.check(projectParent(project)); err != nil {
		return err
	}
	switch {
	case id == "" && r.Method == http.MethodPost:
		ts, err := a.createTraceScope(r, project)
		if err != nil {
			return err
		}
		return rest.WriteJSON(w, http.StatusOK, ts)
	case id == "" && r.Method == http.MethodGet:
		return a.listTraceScopes(w, r, project)
	case id != "" && r.Method == http.MethodGet:
		ts, err := a.traceScope(project, id)
		if err != nil {
			return err
		}
		return rest.WriteJSON(w, http.StatusOK, ts)
	case id != "" && r.Method == http.MethodPatch:
		ts, err := a.patchTraceScope(r, project, id)
		if err != nil {
			return err
		}
		return rest.WriteJSON(w, http.StatusOK, ts)
	case id != "" && r.Method == http.MethodDelete:
		if err := a.deleteTraceScope(project, id); err != nil {
			return err
		}
		return rest.WriteJSON(w, http.StatusOK, struct{}{})
	}
	return apierror.NotFound("no traceScopes method answers %s %s", r.Method, r.URL.Path)
}

// traceScope reads one; the caller holds a.mu.
func (a *Admin) traceScope(project, id string) (TraceScope, error) {
	if id == DefaultScopeID {
		return defaultTraceScope(project), nil
	}
	raw, err := a.db.Get(traceScopePrefix(project) + hex.EncodeToString([]byte(id)))
	if errors.Is(err, store.ErrNotFound) {
		return TraceScope{}, apierror.NotFound("trace scope %s does not exist", traceScopeName(project, id))
	}
	if err != nil {
		return TraceScope{}, apierror.Internal(err, "reading the trace scope")
	}
	var ts TraceScope
	if err := json.Unmarshal(raw, &ts); err != nil {
		return TraceScope{}, apierror.Internal(err, "decoding the trace scope")
	}
	return ts, nil
}

func (a *Admin) putTraceScope(project, id string, ts TraceScope) error {
	raw, err := json.Marshal(ts)
	if err != nil {
		return apierror.Internal(err, "encoding the trace scope")
	}
	if err := a.db.Put(traceScopePrefix(project)+hex.EncodeToString([]byte(id)), raw); err != nil {
		return apierror.Internal(err, "writing the trace scope")
	}
	return nil
}

// traceScopeIDs are the project's own trace scopes, _Default aside, in ID order.
func (a *Admin) traceScopeIDs(project string) ([]string, error) {
	keys, err := a.db.List(traceScopePrefix(project))
	if err != nil {
		return nil, apierror.Internal(err, "listing trace scopes")
	}
	var ids []string
	for _, k := range keys {
		if raw, err := hex.DecodeString(strings.TrimPrefix(k, traceScopePrefix(project))); err == nil {
			ids = append(ids, string(raw))
		}
	}
	sort.Strings(ids)
	return ids, nil
}

// checkTraceScope checks a trace scope's resourceNames and description.
func (a *Admin) checkTraceScope(ts TraceScope) error {
	if len(ts.ResourceNames) == 0 {
		return apierror.InvalidArgument("resourceNames is required: name at least one projects/[PROJECT_ID]")
	}
	if len(ts.ResourceNames) > MaxTraceScopeProjects {
		return apierror.InvalidArgument("resourceNames: a trace scope can include a maximum of %d projects, not %d", MaxTraceScopeProjects, len(ts.ResourceNames))
	}
	seen := map[string]bool{}
	for _, n := range ts.ResourceNames {
		p, ok := strings.CutPrefix(n, "projects/")
		if !ok || p == "" || strings.Contains(p, "/") {
			return apierror.InvalidArgument("resourceNames: %q is not projects/[PROJECT_ID]", n)
		}
		if seen[n] {
			return apierror.InvalidArgument("resourceNames: %q is listed twice", n)
		}
		seen[n] = true
		if err := a.check(projectParent(p)); err != nil {
			return apierror.InvalidArgument("resourceNames: %s: %s", n, apierror.From(err).Message)
		}
	}
	if len(ts.Description) > maxTraceScopeDesc {
		return apierror.InvalidArgument("description: the maximum length is %d characters, not %d", maxTraceScopeDesc, len(ts.Description))
	}
	return nil
}

func (a *Admin) createTraceScope(r *http.Request, project string) (TraceScope, error) {
	id := r.URL.Query().Get("traceScopeId")
	switch {
	case id == "":
		return TraceScope{}, apierror.InvalidArgument("traceScopeId is required")
	case id == DefaultScopeID:
		return TraceScope{}, apierror.AlreadyExists("trace scope %s already exists", traceScopeName(project, id))
	case !traceScopeIDRe.MatchString(id):
		return TraceScope{}, apierror.InvalidArgument("traceScopeId %q must be 1-100 letters, digits, - and _, not starting with _", id)
	}
	var body TraceScope
	if err := rest.DecodeJSON(r, &body); err != nil {
		return TraceScope{}, err
	}
	if err := a.checkTraceScope(body); err != nil {
		return TraceScope{}, err
	}
	if _, err := a.traceScope(project, id); err == nil {
		return TraceScope{}, apierror.AlreadyExists("trace scope %s already exists", traceScopeName(project, id))
	}
	ids, err := a.traceScopeIDs(project)
	if err != nil {
		return TraceScope{}, err
	}
	if len(ids)+1 >= MaxTraceScopes {
		return TraceScope{}, &apierror.Error{Code: codes.ResourceExhausted, Reason: "rateLimitExceeded", Message: fmt.Sprintf("project %s holds the maximum of %d trace scopes", project, MaxTraceScopes)}
	}
	now := a.now().UTC().Format(time.RFC3339Nano)
	ts := TraceScope{Name: traceScopeName(project, id), ResourceNames: body.ResourceNames, Description: body.Description,
		CreateTime: now, UpdateTime: now}
	return ts, a.putTraceScope(project, id, ts)
}

func (a *Admin) listTraceScopes(w http.ResponseWriter, r *http.Request, project string) error {
	ids, err := a.traceScopeIDs(project)
	if err != nil {
		return err
	}
	ids = append([]string{DefaultScopeID}, ids...)
	size := 0
	if v := r.URL.Query().Get("pageSize"); v != "" {
		if err := json.Unmarshal([]byte(v), &size); err != nil || size < 0 {
			return apierror.InvalidArgument("pageSize %q is not a non-negative number", v)
		}
	}
	page, next, err := paging.Page("observability-tracescopes|"+project, ids, r.URL.Query().Get("pageToken"), size)
	if err != nil {
		return apierror.InvalidArgument("pageToken: %v", err)
	}
	out := struct {
		TraceScopes   []TraceScope `json:"traceScopes"`
		NextPageToken string       `json:"nextPageToken,omitempty"`
	}{TraceScopes: []TraceScope{}, NextPageToken: next}
	for _, id := range page {
		if ts, err := a.traceScope(project, id); err == nil {
			out.TraceScopes = append(out.TraceScopes, ts)
		}
	}
	return rest.WriteJSON(w, http.StatusOK, out)
}

func (a *Admin) patchTraceScope(r *http.Request, project, id string) (TraceScope, error) {
	if id == DefaultScopeID {
		return TraceScope{}, apierror.FailedPrecondition("trace scope %s can't be modified: the _Default trace scope can't be changed", traceScopeName(project, id))
	}
	cur, err := a.traceScope(project, id)
	if err != nil {
		return TraceScope{}, err
	}
	var body TraceScope
	if err := rest.DecodeJSON(r, &body); err != nil {
		return TraceScope{}, err
	}
	var paths []string
	for _, f := range strings.Split(r.URL.Query().Get("updateMask"), ",") {
		if f = strings.TrimSpace(f); f != "" {
			paths = append(paths, f)
		}
	}
	if len(paths) == 0 {
		if body.ResourceNames != nil {
			paths = append(paths, "resourceNames")
		}
		if body.Description != "" {
			paths = append(paths, "description")
		}
	}
	for _, f := range paths {
		switch f {
		case "resourceNames", "resource_names":
			cur.ResourceNames = body.ResourceNames
		case "description":
			cur.Description = body.Description
		default:
			return TraceScope{}, apierror.InvalidArgument("updateMask path %q is not a field that can be updated", f)
		}
	}
	if err := a.checkTraceScope(cur); err != nil {
		return TraceScope{}, err
	}
	cur.UpdateTime = a.now().UTC().Format(time.RFC3339Nano)
	return cur, a.putTraceScope(project, id, cur)
}

func (a *Admin) deleteTraceScope(project, id string) error {
	if id == DefaultScopeID {
		return apierror.FailedPrecondition("trace scope %s can't be deleted: the _Default trace scope can't be deleted", traceScopeName(project, id))
	}
	if _, err := a.traceScope(project, id); err != nil {
		return err
	}
	if err := a.db.Delete(traceScopePrefix(project) + hex.EncodeToString([]byte(id))); err != nil {
		return apierror.Internal(err, "deleting the trace scope")
	}
	s, err := a.observabilityScope(project)
	if err != nil || s.TraceScope != traceScopeName(project, id) {
		return err
	}
	s.TraceScope = defaultObservabilityScope(project).TraceScope
	s.UpdateTime = a.now().UTC().Format(time.RFC3339Nano)
	raw, err := json.Marshal(s)
	if err != nil {
		return apierror.Internal(err, "encoding the scope")
	}
	if err := a.db.Put(obsScopeKey(project), raw); err != nil {
		return apierror.Internal(err, "writing the scope")
	}
	return nil
}
