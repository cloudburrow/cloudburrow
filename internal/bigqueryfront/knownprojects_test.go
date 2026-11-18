package bigqueryfront

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/resource"
)

// TestKnownProjectsMarkTheEngine (#1764): a request of a project Resource
// Manager has ACTIVE reaches the engine with KnownProjectHeader, one of a
// project it does not have (or has not ACTIVE) without, and a client's own
// header never reaches it. Resource Manager is asked once for an ACTIVE
// project.
func TestKnownProjectsMarkTheEngine(t *testing.T) {
	var asked atomic.Int32
	rm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		switch r.URL.Path {
		case "/v3/projects/other":
			_, _ = w.Write([]byte(`{"projectId":"other","state":"ACTIVE"}`))
		case "/v3/projects/gone":
			_, _ = w.Write([]byte(`{"projectId":"gone","state":"DELETE_REQUESTED"}`))
		default:
			http.Error(w, `{"error":{"code":404}}`, http.StatusNotFound)
		}
	}))
	defer rm.Close()
	var marked atomic.Value
	engine := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		marked.Store(r.Header.Get(KnownProjectHeader))
		_, _ = w.Write([]byte(`{}`))
	})
	k := &knownProjects{rm: rm.URL}
	h := k.client(k.engine(engine))
	for _, c := range []struct {
		path   string
		client bool
		want   string
	}{
		{"/bigquery/v2/projects/other/datasets", false, "1"},
		{"/bigquery/v2/projects/other/datasets", false, "1"},
		{"/upload/bigquery/v2/projects/other/jobs", false, "1"},
		{"/bigquery/v2/projects/gone/datasets", false, ""},
		{"/bigquery/v2/projects/nowhere/datasets", true, ""},
	} {
		req := httptest.NewRequest(http.MethodGet, c.path, nil)
		if c.client {
			req.Header.Set(KnownProjectHeader, "1")
		}
		h.ServeHTTP(httptest.NewRecorder(), req)
		if got, _ := marked.Load().(string); got != c.want {
			t.Errorf("%s (client header %v): the engine's header %q, want %q", c.path, c.client, got, c.want)
		}
	}
	if n := asked.Load(); n != 3 {
		t.Errorf("Resource Manager was asked %d times, want 3 (other once, gone, nowhere)", n)
	}
	// With no Resource Manager, nothing is marked.
	none := &knownProjects{}
	req := httptest.NewRequest(http.MethodGet, "/bigquery/v2/projects/other/datasets", nil)
	req.Header.Set(KnownProjectHeader, "1")
	none.client(none.engine(engine)).ServeHTTP(httptest.NewRecorder(), req)
	if got, _ := marked.Load().(string); got != "" {
		t.Errorf("with no Resource Manager: the engine's header %q, want none", got)
	}
}

// TestProjectsListIsResourceManagers (#1764): projects.list is every
// ACTIVE project of Resource Manager, across its pages, by ID, with the
// project's number; without a Resource Manager the engine answers.
func TestProjectsListIsResourceManagers(t *testing.T) {
	rm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v3/projects:search" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("pageToken") == "" {
			_, _ = w.Write([]byte(`{"projects":[{"projectId":"zeta","state":"ACTIVE"},{"projectId":"gone","state":"DELETE_REQUESTED"}],"nextPageToken":"2"}`))
			return
		}
		_, _ = w.Write([]byte(`{"projects":[{"projectId":"alpha","state":"ACTIVE","displayName":"Alpha"}]}`))
	}))
	defer rm.Close()
	engine := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"projects":[{"id":"engine"}]}`))
	})
	k := &knownProjects{rm: rm.URL}
	rec := httptest.NewRecorder()
	k.client(engine).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/bigquery/v2/projects", nil))
	var got struct {
		Projects []struct {
			ID               string `json:"id"`
			NumericID        string `json:"numericId"`
			FriendlyName     string `json:"friendlyName"`
			ProjectReference struct {
				ProjectID string `json:"projectId"`
			} `json:"projectReference"`
		} `json:"projects"`
		TotalItems int `json:"totalItems"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if len(got.Projects) != 2 || got.Projects[0].ID != "alpha" || got.Projects[1].ID != "zeta" || got.TotalItems != 2 {
		t.Fatalf("projects.list = %s, want alpha and zeta", rec.Body)
	}
	a := got.Projects[0]
	if a.FriendlyName != "Alpha" || a.ProjectReference.ProjectID != "alpha" || a.NumericID != strconv.FormatInt(resource.ProjectNumber("alpha"), 10) {
		t.Errorf("alpha = %+v", a)
	}
	if !k.known("zeta") {
		t.Error("a listed project is not marked known")
	}
	rec = httptest.NewRecorder()
	(&knownProjects{}).client(engine).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/bigquery/v2/projects", nil))
	if !strings.Contains(rec.Body.String(), `"engine"`) {
		t.Errorf("with no Resource Manager: %s, want the engine's answer", rec.Body)
	}
}

// TestUnreachableResourceManagerIsAskedOnce (#1764): a Resource Manager
// that does not answer is asked once per unreachableTTL, not per request.
func TestUnreachableResourceManagerIsAskedOnce(t *testing.T) {
	var asked atomic.Int32
	rm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer rm.Close()
	k := &knownProjects{rm: rm.URL}
	for i := 0; i < 3; i++ {
		if k.isActive(context.Background(), "p") {
			t.Fatal("a failed ask made p ACTIVE")
		}
	}
	if n := asked.Load(); n != 1 {
		t.Errorf("Resource Manager was asked %d times, want 1", n)
	}
}
