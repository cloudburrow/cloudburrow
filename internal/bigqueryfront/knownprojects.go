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
// An ACTIVE answer is kept for activeProjectTTL, and the project asked
// again after it: once the engine has the project, the header no longer
// matters, but a project shut down since is not served (#1767). Any other
// answer is asked again after unknownProjectTTL, and a Resource Manager that did not answer after
// unreachableTTL. Each ask is an attempt bounded by projectAskTimeout and,
// when it fails, one more bounded by twice that, so a Resource Manager
// that is down slows no request for long; the
// instance's own project, which the engine has from the start, is served
// all the same.

// KnownProjectHeader is bigquery-emulator patch 0091's header.
const KnownProjectHeader = "X-Cloudburrow-Known-Project"

// unknownProjectTTL is how long an answer other than ACTIVE is kept, and
// unreachableTTL how long Resource Manager's silence is.
const (
	activeProjectTTL  = 2 * time.Second
	unknownProjectTTL = 2 * time.Second
	unreachableTTL    = 10 * time.Second
)

// projectAskTimeout bounds one projects.get of Resource Manager.
var projectAskTimeout = 2 * time.Second

// projectOfPath reads the project a BigQuery REST path names.
var projectOfPath = regexp.MustCompile(`^(?:/upload)?(?:/bigquery/v2)?/projects/([^/]+)(?:/|$)`)

// knownProjects is the projects Resource Manager has ACTIVE.
type knownProjects struct {
	rm      string
	numbers *projectNumbers
	mu      sync.Mutex
	active  map[string]bool
	// fresh is when each ACTIVE answer is asked again.
	fresh   map[string]time.Time
	unknown map[string]time.Time
	// shutDown is the projects Resource Manager last answered
	// DELETE_REQUESTED of (#1767, refuseShutDown).
	shutDown map[string]bool
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
	if k.active[project] && time.Now().Before(k.fresh[project]) {
		k.mu.Unlock()
		return true
	}
	if t, ok := k.unknown[project]; ok && time.Now().Before(t) {
		k.mu.Unlock()
		return false
	}
	k.mu.Unlock()
	u := strings.TrimRight(k.rm, "/") + "/v3/projects/" + url.PathEscape(project)
	actx, cancel := context.WithTimeout(ctx, projectAskTimeout)
	status, body, err := serviceAttempt(actx, http.MethodGet, u, "", nil)
	cancel()
	if err != nil && ctx.Err() == nil {
		// Once more (#1767): the front's first connection to Resource
		// Manager after the pod starts was measured taking over
		// projectAskTimeout, and a project asked then was answered 404.
		actx, cancel = context.WithTimeout(ctx, 2*projectAskTimeout)
		status, body, err = serviceAttempt(actx, http.MethodGet, u, "", nil)
		cancel()
	}
	var p struct {
		State string `json:"state"`
	}
	read := err == nil && status == http.StatusOK && json.Unmarshal(body, &p) == nil
	active := read && p.State == "ACTIVE"
	ttl := unknownProjectTTL
	if err != nil || status/100 == 5 {
		k.mu.Lock()
		was := k.active[project]
		if was {
			// Resource Manager is silent: an ACTIVE project stays served.
			if k.fresh == nil {
				k.fresh = map[string]time.Time{}
			}
			k.fresh[project] = time.Now().Add(unreachableTTL)
		}
		k.mu.Unlock()
		if was {
			return true
		}
		logFront("bigquery front: Resource Manager projects.get of %s: %s; asked again in %s", project, describeAttempt(status, err), unreachableTTL)
		ttl = unreachableTTL
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.shutDown == nil {
		k.shutDown = map[string]bool{}
	}
	if read {
		k.shutDown[project] = p.State == "DELETE_REQUESTED"
	} else if status == http.StatusNotFound || status == http.StatusForbidden {
		delete(k.shutDown, project) // purged, or not the instance's
	}
	if active {
		if k.active == nil {
			k.active = map[string]bool{}
		}
		if k.fresh == nil {
			k.fresh = map[string]time.Time{}
		}
		k.active[project] = true
		k.fresh[project] = time.Now().Add(activeProjectTTL)
		delete(k.unknown, project)
	} else {
		if status != 0 && status/100 != 5 {
			delete(k.active, project)
		}
		if k.unknown == nil {
			k.unknown = map[string]time.Time{}
		}
		k.unknown[project] = time.Now().Add(ttl)
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
		p := pathProject(r)
		k.isActive(r.Context(), p)
		if k.refuseShutDown(w, p) || k.refuseShutDownJob(w, r, p) { // #1767, #1774 (shutdownjobs.go)
			return
		}
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

// refuseShutDown answers a request of a project Resource Manager has in
// DELETE_REQUESTED, which BigQuery has served before (#1767), reporting
// true. A project shut down "is not usable" until it is restored, and its
// "resources ... are shut down" (https://cloud.google.com/resource-manager/docs/creating-managing-projects#shutting_down_projects);
// what BigQuery answers a request of one is not documented: 403
// accessDenied "Access Denied: Project P: the project has been deleted
// (DELETE_REQUESTED)" is CloudBurrow's.
// UNVERIFIED wording and code; docs re-read 2026-11-17: https://cloud.google.com/resource-manager/docs/creating-managing-projects#shutting_down_projects
func (k *knownProjects) refuseShutDown(w http.ResponseWriter, project string) bool {
	if k == nil || project == "" {
		return false
	}
	k.mu.Lock()
	down := k.shutDown[project]
	k.mu.Unlock()
	if !down {
		return false
	}
	writeError(w, http.StatusForbidden, "accessDenied", "Access Denied: Project "+project+": the project has been "+
		"deleted (DELETE_REQUESTED); it is served again once it is restored.")
	return true
}
