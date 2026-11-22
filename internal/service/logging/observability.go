package logging

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/store"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// The Observability API's scopes (#2020): observability.googleapis.com v1
// projects.locations.scopes get and patch, as its discovery document
// describes them, served beside Cloud Logging on its port, since a scope's
// logScope is the log scope (logging.googleapis.com/LogScope) the Logs
// Explorer opens on (docs.cloud.google.com/logging/docs/log-scope/
// create-and-manage, re-read 2026-11-20). Google publishes no Go client
// for it but the generated google.golang.org/api/observability/v1.
//
// A project has one scope, _Default, in the global location. Until it is
// patched its logScope is the project's own _Default log scope, its
// traceScope the project's _Default trace scope, and its updateTime unset,
// as the reference says ("initially unset").
//
// Choices Google does not document (UNVERIFIED): a scope other than
// _Default is NOT_FOUND; a location other than global, a logScope that is
// not //logging.googleapis.com/projects/{project}/locations/global/logScopes/{id}
// of the same project, or a traceScope that is not
// projects/{project}/locations/global/traceScopes/{id}, is INVALID_ARGUMENT;
// a logScope that names a log scope the project does not hold is
// NOT_FOUND; and an updateMask path other than logScope or traceScope is
// INVALID_ARGUMENT. A traceScope must name a trace scope the project holds
// (tracescopes.go, #2108), else NOT_FOUND.

// ObservabilityScope is an Observability API Scope resource.
type ObservabilityScope struct {
	Name       string `json:"name,omitempty"`
	LogScope   string `json:"logScope,omitempty"`
	TraceScope string `json:"traceScope,omitempty"`
	UpdateTime string `json:"updateTime,omitempty"`
}

// DefaultScopeID is the one Observability scope a project has.
const DefaultScopeID = "_Default"

// LogScopeServicePrefix is the service prefix of a scope's logScope, a
// full resource name.
const LogScopeServicePrefix = "//logging.googleapis.com/"

var (
	obsLogScopeRe   = regexp.MustCompile(`^//logging\.googleapis\.com/projects/([^/]+)/locations/global/logScopes/([A-Za-z0-9_\-]+)$`)
	obsTraceScopeRe = regexp.MustCompile(`^projects/([^/]+)/locations/global/traceScopes/([A-Za-z0-9_\-]+)$`)
)

func obsScopeKey(project string) string {
	return "observability/scopes/" + hex.EncodeToString([]byte(project)) + "/" + DefaultScopeID
}

func defaultObservabilityScope(project string) ObservabilityScope {
	return ObservabilityScope{
		Name:       "projects/" + project + "/locations/global/scopes/" + DefaultScopeID,
		LogScope:   LogScopeServicePrefix + "projects/" + project + "/locations/global/logScopes/" + DefaultScopeID,
		TraceScope: "projects/" + project + "/locations/global/traceScopes/" + DefaultScopeID,
	}
}

// ServeObservability serves an Observability API request and reports true,
// or reports false for any other path.
func (a *Admin) ServeObservability(w http.ResponseWriter, r *http.Request) bool {
	// Settings, operations, locations, buckets, datasets, views and links
	// (obsresources.go, #2117).
	if a.serveObsResources(w, r) {
		return true
	}
	path, ok := strings.CutPrefix(r.URL.Path, "/v1/projects/")
	if !ok {
		return false
	}
	parts := strings.Split(path, "/")
	if len(parts) < 3 || parts[1] != "locations" {
		return false
	}
	if len(parts) >= 4 && parts[3] == "traceScopes" && len(parts) <= 5 {
		id := ""
		if len(parts) == 5 {
			id = parts[4]
		}
		if err := a.serveTraceScopes(w, r, parts[0], parts[2], id); err != nil {
			rest.WriteError(w, err)
		}
		return true
	}
	if len(parts) != 5 || parts[3] != "scopes" {
		rest.WriteError(w, apierror.NotFound("no Observability API method answers %s %s", r.Method, r.URL.Path))
		return true
	}
	if err := a.serveObservability(w, r, parts[0], parts[2], parts[4]); err != nil {
		rest.WriteError(w, err)
	}
	return true
}

func (a *Admin) serveObservability(w http.ResponseWriter, r *http.Request, project, loc, id string) error {
	if project == "" {
		return apierror.InvalidArgument("name must be projects/{project}/locations/global/scopes/_Default")
	}
	if loc != "global" {
		return apierror.InvalidArgument("a scope's location must be global, not %q", loc)
	}
	if id != DefaultScopeID {
		return apierror.NotFound("projects/%s/locations/global/scopes/%s does not exist: a project's one scope is _Default", project, id)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	switch r.Method {
	case http.MethodGet:
		s, err := a.observabilityScope(project)
		if err != nil {
			return err
		}
		return rest.WriteJSON(w, http.StatusOK, s)
	case http.MethodPatch:
		s, err := a.patchObservabilityScope(r, project)
		if err != nil {
			return err
		}
		return rest.WriteJSON(w, http.StatusOK, s)
	}
	return apierror.NotFound("no scopes method answers %s %s", r.Method, r.URL.Path)
}

// observabilityScope is the project's scope; the caller holds a.mu.
func (a *Admin) observabilityScope(project string) (ObservabilityScope, error) {
	raw, err := a.db.Get(obsScopeKey(project))
	if errors.Is(err, store.ErrNotFound) {
		return defaultObservabilityScope(project), nil
	}
	if err != nil {
		return ObservabilityScope{}, apierror.Internal(err, "reading the scope")
	}
	var s ObservabilityScope
	if err := json.Unmarshal(raw, &s); err != nil {
		return ObservabilityScope{}, apierror.Internal(err, "decoding the scope")
	}
	return s, nil
}

func (a *Admin) patchObservabilityScope(r *http.Request, project string) (ObservabilityScope, error) {
	cur, err := a.observabilityScope(project)
	if err != nil {
		return ObservabilityScope{}, err
	}
	var body ObservabilityScope
	if err := rest.DecodeJSON(r, &body); err != nil {
		return ObservabilityScope{}, err
	}
	var paths []string
	for _, f := range strings.Split(r.URL.Query().Get("updateMask"), ",") {
		if f = strings.TrimSpace(f); f != "" {
			paths = append(paths, f)
		}
	}
	if len(paths) == 0 {
		// No mask: every field present in the request is overwritten.
		if body.LogScope != "" {
			paths = append(paths, "logScope")
		}
		if body.TraceScope != "" {
			paths = append(paths, "traceScope")
		}
	}
	for _, f := range paths {
		switch f {
		case "logScope", "log_scope":
			if err := a.checkObsLogScope(project, body.LogScope); err != nil {
				return ObservabilityScope{}, err
			}
			cur.LogScope = body.LogScope
		case "traceScope", "trace_scope":
			m := obsTraceScopeRe.FindStringSubmatch(body.TraceScope)
			if m == nil || m[1] != project {
				return ObservabilityScope{}, apierror.InvalidArgument("traceScope %q is not projects/%s/locations/global/traceScopes/{trace_scope}", body.TraceScope, project)
			}
			if _, err := a.traceScope(project, m[2]); err != nil {
				return ObservabilityScope{}, err // NOT_FOUND: a trace scope the project does not hold (tracescopes.go)
			}
			cur.TraceScope = body.TraceScope
		default:
			return ObservabilityScope{}, apierror.InvalidArgument("updateMask path %q is not a field that can be updated", f)
		}
	}
	cur.UpdateTime = a.now().UTC().Format(time.RFC3339Nano)
	raw, err := json.Marshal(cur)
	if err != nil {
		return ObservabilityScope{}, apierror.Internal(err, "encoding the scope")
	}
	if err := a.db.Put(obsScopeKey(project), raw); err != nil {
		return ObservabilityScope{}, apierror.Internal(err, "writing the scope")
	}
	return cur, nil
}

// checkObsLogScope checks a scope's logScope names a log scope the project
// holds, or its own _Default.
func (a *Admin) checkObsLogScope(project, name string) error {
	m := obsLogScopeRe.FindStringSubmatch(name)
	if m == nil || m[1] != project {
		return apierror.InvalidArgument("logScope %q is not //logging.googleapis.com/projects/%s/locations/global/logScopes/{log_scope}", name, project)
	}
	if m[2] == DefaultScopeID {
		return nil
	}
	if _, err := a.db.Get(collLogScopes.key(projectParent(project), "global", m[2])); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return apierror.NotFound("log scope %s does not exist", strings.TrimPrefix(name, LogScopeServicePrefix))
		}
		return apierror.Internal(err, "reading the log scope")
	}
	return nil
}

// CallObservability calls the Observability scopes JSON API in process, for
// the console, and returns the status and body a client would get.
func (a *Admin) CallObservability(method, path string, body []byte) (int, []byte) {
	r := httptest.NewRequest(method, path, bytes.NewReader(body))
	if len(body) > 0 {
		r.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	if !a.ServeObservability(w, r) {
		return http.StatusNotFound, []byte(`{"error":{"message":"not an Observability scopes path"}}`)
	}
	return w.Code, w.Body.Bytes()
}

// forgetDefaultLogScope makes the project's own _Default its default log
// scope again when the deleted log scope id was its default. The caller
// holds a.mu. UNVERIFIED (docs re-read 2026-11-20, still unanswered:
// https://docs.cloud.google.com/logging/docs/log-scope/create-and-manage):
// Google does not say what a deleted default leaves.
func (a *Admin) forgetDefaultLogScope(project, id string) error {
	s, err := a.observabilityScope(project)
	if err != nil {
		return err
	}
	if s.LogScope != LogScopeServicePrefix+"projects/"+project+"/locations/global/logScopes/"+id {
		return nil
	}
	s.LogScope = defaultObservabilityScope(project).LogScope
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
