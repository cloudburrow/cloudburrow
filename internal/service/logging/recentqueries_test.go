package logging

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/store"
)

// TestRecentQueries (#2031): a run query is listed for the user who ran it
// and no one else, most recent first, once however often it ran; a type()
// filter keeps one kind; another filter is INVALID_ARGUMENT; a query older
// than 30 days is gone; and list is the resource's only method.
func TestRecentQueries(t *testing.T) {
	SetCallerResolver(func(bearer string) (string, bool) {
		if bearer == "tok-other" {
			return "Other@example.com", true
		}
		return "", false
	}, "Me@example.com")
	t.Cleanup(func() { SetCallerResolver(nil, "") })
	a := NewAdmin(store.NewMemory())
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	a.now = func() time.Time { return now }
	record := func(user, q string, sql bool) {
		t.Helper()
		if err := a.RecordRecentQuery("projects/proj-one", "global", user, q, sql); err != nil {
			t.Fatalf("record %q: %v", q, err)
		}
		now = now.Add(time.Second)
	}
	list := func(path, bearer string) (int, []recentQuery, string) {
		t.Helper()
		r := httptest.NewRequest(http.MethodGet, path, nil)
		if bearer != "" {
			r.Header.Set("Authorization", "Bearer "+bearer)
		}
		w := httptest.NewRecorder()
		if !a.ServeExtra(w, r) {
			t.Fatalf("%s was not served", path)
		}
		var out struct {
			RecentQueries []recentQuery
			NextPageToken string
		}
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out.RecentQueries, out.NextPageToken
	}
	record("", "severity>=ERROR", false)
	record("me@example.com", "SELECT 1", true)
	record("", `resource.type="gce_instance"`, false)
	record("", "severity>=ERROR", false) // again: moves to the top
	record("other@example.com", "theirs", false)

	code, got, _ := list("/v2/projects/proj-one/locations/global/recentQueries", "")
	if code != http.StatusOK || len(got) != 3 {
		t.Fatalf("list = %d %+v, want my 3 queries", code, got)
	}
	if got[0].LoggingQuery == nil || got[0].LoggingQuery.Filter != "severity>=ERROR" || got[2].OpsAnalyticsQuery == nil ||
		got[2].OpsAnalyticsQuery.SQLQueryText != "SELECT 1" {
		t.Errorf("list order = %+v, want severity>=ERROR (run last) first and SELECT 1 last", got)
	}
	if !strings.HasPrefix(got[0].Name, "projects/proj-one/locations/global/recentQueries/") || got[0].LastRunTime != "2026-11-20T12:00:03Z" {
		t.Errorf("the first = %+v", got[0])
	}
	if _, got, _ := list("/v2/projects/proj-one/locations/global/recentQueries", "tok-other"); len(got) != 1 || got[0].LoggingQuery.Filter != "theirs" {
		t.Errorf("another user's list = %+v, want only theirs", got)
	}
	if _, got, _ := list(`/v2/projects/proj-one/locations/-/recentQueries?filter=type(%22OpsAnalytics%22)`, ""); len(got) != 1 || got[0].OpsAnalyticsQuery == nil {
		t.Errorf("type(\"OpsAnalytics\") = %+v", got)
	}
	if _, got, _ := list(`/v2/projects/proj-one/locations/global/recentQueries?filter=type(%22Logging%22)`, ""); len(got) != 2 {
		t.Errorf("type(\"Logging\") = %+v", got)
	}
	if _, got, _ := list("/v2/projects/proj-one/locations/us-central1/recentQueries", ""); len(got) != 0 {
		t.Errorf("another location = %+v, want none", got)
	}
	_, page, next := list("/v2/projects/proj-one/locations/global/recentQueries?pageSize=2", "")
	if len(page) != 2 || next == "" {
		t.Fatalf("pageSize=2 = %d, next %q", len(page), next)
	}
	if _, rest, _ := list("/v2/projects/proj-one/locations/global/recentQueries?pageSize=2&pageToken="+next, ""); len(rest) != 1 || rest[0].OpsAnalyticsQuery == nil {
		t.Errorf("the second page = %+v", rest)
	}
	for path, want := range map[string]int{
		`/v2/projects/proj-one/locations/global/recentQueries?filter=visibility%3DPRIVATE`: http.StatusBadRequest,
		"/v2/projects/proj-one/recentQueries":                                              http.StatusBadRequest,
		"/v2/projects/proj-one/locations/global/recentQueries/x":                           http.StatusNotFound,
	} {
		if code, _, _ := list(path, ""); code != want {
			t.Errorf("GET %s = %d, want %d", path, code, want)
		}
	}
	w := httptest.NewRecorder()
	a.ServeExtra(w, httptest.NewRequest(http.MethodPost, "/v2/projects/proj-one/locations/global/recentQueries", strings.NewReader("{}")))
	if w.Code != http.StatusNotFound {
		t.Errorf("POST = %d, want 404: list is the only method", w.Code)
	}

	// 30 days on, they are gone.
	now = now.Add(31 * 24 * time.Hour)
	if _, got, _ := list("/v2/projects/proj-one/locations/global/recentQueries", ""); len(got) != 0 {
		t.Errorf("after 31 days = %+v, want none", got)
	}
}

// TestRecentQueriesKeepTenThousand: past 10,000 unique queries, the oldest
// goes.
func TestRecentQueriesKeepTenThousand(t *testing.T) {
	a := NewAdmin(store.NewMemory())
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	a.now = func() time.Time { return now }
	for i := 0; i <= maxRecentQueries; i++ {
		a.mu.Lock()
		id := recentQueryID(false, fmt.Sprint("q", i))
		raw, _ := json.Marshal(recentQuery{Name: "projects/proj-one/locations/global/recentQueries/" + id,
			LoggingQuery: &loggingQuery{Filter: fmt.Sprint("q", i)}, LastRunTime: now.Add(time.Duration(i) * time.Millisecond).Format(time.RFC3339Nano)})
		_ = a.db.Put(recentKey(parent{collProjects, "proj-one"}, "u", "global", id), raw)
		a.mu.Unlock()
	}
	now = now.Add(time.Hour)
	if err := a.RecordRecentQuery("projects/proj-one", "global", "u", "newest", false); err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	all, err := a.recentQueries(parent{collProjects, "proj-one"}, "u", "-")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != maxRecentQueries || all[0].q.LoggingQuery.Filter != "newest" || all[len(all)-1].q.LoggingQuery.Filter != "q2" {
		t.Errorf("kept %d, first %q, last %q; want 10000 from newest to q2", len(all), all[0].q.LoggingQuery.Filter, all[len(all)-1].q.LoggingQuery.Filter)
	}
}
