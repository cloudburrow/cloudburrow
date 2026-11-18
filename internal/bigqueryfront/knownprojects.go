package bigqueryfront

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Every project of the instance's Resource Manager (#1764). "Datasets,
// tables and jobs belong to a project", and a job of one project reads and
// writes the tables of another by naming it, `project.dataset.table`
// (https://cloud.google.com/bigquery/docs/resource-hierarchy). The emulator
// served the project it was started with alone, and answered every other
// 404 "project P is not found", even one Resource Manager has. The front
// now asks Resource Manager (projects.get, through the cloudburrow-host
// Service, --resourcemanager) of each project a request names, and marks
// the engine's request with KnownProjectHeader when Resource Manager has
// it ACTIVE: the emulator adds the project on first use, empty
// (bigquery-emulator patch 0091), and serves it from then on. A project
// Resource Manager does not have keeps the emulator's answer; a client's
// own KnownProjectHeader is removed. With no Resource Manager to ask, the
// instance's project alone is served, as before.
//
// An ACTIVE answer is kept: once the engine has the project, the header
// no longer matters. Any other answer is asked again after
// unknownProjectTTL.

// KnownProjectHeader is bigquery-emulator patch 0091's header.
const KnownProjectHeader = "X-Cloudburrow-Known-Project"

// unknownProjectTTL is how long an answer other than ACTIVE is kept.
const unknownProjectTTL = 2 * time.Second

// projectOfPath reads the project a BigQuery REST path names.
var projectOfPath = regexp.MustCompile(`^(?:/upload)?(?:/bigquery/v2)?/projects/([^/]+)(?:/|$)`)

// knownProjects is the projects Resource Manager has ACTIVE.
type knownProjects struct {
	rm      string
	numbers *projectNumbers
	mu      sync.Mutex
	active  map[string]bool
	unknown map[string]time.Time
}

func pathProject(r *http.Request) string {
	m := projectOfPath.FindStringSubmatch(r.URL.EscapedPath())
	if m == nil {
		return ""
	}
	p, err := url.PathUnescape(m[1])
	if err != nil {
		return ""
	}
	return p
}

// isActive reports whether Resource Manager has project ACTIVE.
func (k *knownProjects) isActive(ctx context.Context, project string) bool {
	if k == nil || k.rm == "" || project == "" {
		return false
	}
	k.mu.Lock()
	if k.active[project] {
		k.mu.Unlock()
		return true
	}
	if t, ok := k.unknown[project]; ok && time.Since(t) < unknownProjectTTL {
		k.mu.Unlock()
		return false
	}
	k.mu.Unlock()
	u := strings.TrimRight(k.rm, "/") + "/v3/projects/" + url.PathEscape(project)
	status, body, err := serviceCall(ctx, "Resource Manager projects.get of "+project, http.MethodGet, u, "", nil)
	var p struct {
		State string `json:"state"`
	}
	active := err == nil && status == http.StatusOK && json.Unmarshal(body, &p) == nil && p.State == "ACTIVE"
	k.mu.Lock()
	defer k.mu.Unlock()
	if active {
		if k.active == nil {
			k.active = map[string]bool{}
		}
		k.active[project] = true
		delete(k.unknown, project)
	} else {
		if k.unknown == nil {
			k.unknown = map[string]time.Time{}
		}
		k.unknown[project] = time.Now()
	}
	return active
}

// known reports whether project was found ACTIVE, without asking.
func (k *knownProjects) known(project string) bool {
	if k == nil {
		return false
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.active[project]
}

// projectsListPath is projects.list's.
var projectsListPath = regexp.MustCompile(`^(?:/bigquery/v2)?/projects/?$`)

// listProjects answers projects.list, "projects to which the user has been
// granted any project role" (https://cloud.google.com/bigquery/docs/reference/rest/v2/projects/list):
// every ACTIVE project of Resource Manager, which an instance grants every
// caller (CloudBurrow authenticates no request), by project ID, with its
// number. The emulator listed the projects it had been used in, numbered
// from 1. false when there is no Resource Manager to ask.
func (k *knownProjects) listProjects(w http.ResponseWriter, r *http.Request) bool {
	if k == nil || k.rm == "" {
		return false
	}
	type project struct {
		ProjectID   string `json:"projectId"`
		State       string `json:"state"`
		DisplayName string `json:"displayName"`
	}
	var all []project
	token := ""
	for {
		u := strings.TrimRight(k.rm, "/") + "/v3/projects:search?pageSize=1000"
		if token != "" {
			u += "&pageToken=" + url.QueryEscape(token)
		}
		status, body, err := serviceCall(r.Context(), "Resource Manager projects.search", http.MethodGet, u, "", nil)
		var page struct {
			Projects      []project `json:"projects"`
			NextPageToken string    `json:"nextPageToken"`
		}
		if err != nil || status != http.StatusOK || json.Unmarshal(body, &page) != nil {
			msg := "cloudburrow: could not list the projects of Resource Manager"
			if err != nil {
				msg += ": " + err.Error()
			} else {
				msg += ": " + errorMessage(body, status)
			}
			writeError(w, http.StatusInternalServerError, "backendError", msg)
			return true
		}
		all = append(all, page.Projects...)
		if token = page.NextPageToken; token == "" {
			break
		}
	}
	type ref struct {
		ProjectID string `json:"projectId"`
	}
	type listed struct {
		Kind             string `json:"kind"`
		ID               string `json:"id"`
		NumericID        string `json:"numericId"`
		ProjectReference ref    `json:"projectReference"`
		FriendlyName     string `json:"friendlyName"`
	}
	out := []listed{}
	k.mu.Lock()
	if k.active == nil {
		k.active = map[string]bool{}
	}
	for _, p := range all {
		if p.State != "ACTIVE" {
			continue
		}
		k.active[p.ProjectID] = true
		name := p.DisplayName
		if name == "" {
			name = p.ProjectID
		}
		out = append(out, listed{Kind: "bigquery#project", ID: p.ProjectID,
			NumericID: strconv.FormatInt(k.numbers.of(p.ProjectID), 10), ProjectReference: ref{p.ProjectID}, FriendlyName: name})
	}
	k.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	writeJSON(w, http.StatusOK, map[string]any{"kind": "bigquery#projectList", "projects": out, "totalItems": len(out)})
	return true
}

// client removes a client's KnownProjectHeader and asks Resource Manager
// of the project the request names, before the front answers it.
func (k *knownProjects) client(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && projectsListPath.MatchString(r.URL.Path) && k.listProjects(w, r) {
			return
		}
		if r.Header.Get(KnownProjectHeader) != "" {
			r = r.Clone(r.Context())
			r.Header.Del(KnownProjectHeader)
		}
		k.isActive(r.Context(), pathProject(r))
		next.ServeHTTP(w, r)
	})
}

// engine marks each request to the engine of a project found ACTIVE: the
// client's, and the ones the front sends of its own.
func (k *knownProjects) engine(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p := pathProject(r); p != "" && k.known(p) && r.Header.Get(KnownProjectHeader) == "" {
			r = r.Clone(r.Context())
			r.Header.Set(KnownProjectHeader, "1")
		}
		next.ServeHTTP(w, r)
	})
}
