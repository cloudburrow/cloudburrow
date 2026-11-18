package bigqueryfront

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestSQLProjects(t *testing.T) {
	for sql, want := range map[string][]string{
		"SELECT * FROM `gone.d.t`":                                          {"gone"},
		"SELECT * FROM gone.d.t JOIN `other-p`.d.u ON TRUE":                 {"gone", "other-p"},
		"SELECT a.b.c FROM d.t AS a":                                        nil,
		"SELECT * FROM `region-us`.INFORMATION_SCHEMA.JOBS":                 {"region-us"},
		"INSERT INTO `p1.d.t` SELECT 1; UPDATE p2.d.t SET x = 1 WHERE TRUE": {"p1", "p2"},
		"SELECT * FROM d.t, `p3.d.u`":                                       {"p3"},
		"CREATE TABLE d.c CLONE `p4.d.t`":                                   {"p4"},
		"SELECT 'FROM x.y.z'":                                               nil,
	} {
		if got := sqlProjects(sql); !reflect.DeepEqual(got, want) {
			t.Errorf("sqlProjects(%q) = %v, want %v", sql, got, want)
		}
	}
}

// TestRefuseShutDownJob: a job of an ACTIVE project naming a table of a
// DELETE_REQUESTED one is 403, nothing reaching the engine (#1774).
func TestRefuseShutDownJob(t *testing.T) {
	rm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v3/projects/home", "/v3/projects/other":
			_, _ = w.Write([]byte(`{"state":"ACTIVE"}`))
		case "/v3/projects/gone":
			_, _ = w.Write([]byte(`{"state":"DELETE_REQUESTED"}`))
		default:
			http.Error(w, `{}`, http.StatusNotFound)
		}
	}))
	defer rm.Close()
	reached := false
	engine := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		_, _ = w.Write([]byte(`{}`))
	})
	k := &knownProjects{rm: rm.URL}
	h := k.client(engine)
	for _, c := range []struct {
		path, body string
		refused    bool
	}{
		{"/bigquery/v2/projects/home/queries", `{"query":"SELECT * FROM ` + "`gone.d.t`" + `"}`, true},
		{"/bigquery/v2/projects/home/jobs", `{"configuration":{"query":{"query":"SELECT 1 FROM other.d.t JOIN gone.d.u ON TRUE"}}}`, true},
		{"/bigquery/v2/projects/home/jobs", `{"configuration":{"copy":{"sourceTable":{"projectId":"gone","datasetId":"d","tableId":"t"},` +
			`"destinationTable":{"datasetId":"d","tableId":"u"}}}}`, true},
		{"/bigquery/v2/projects/home/jobs", `{"configuration":{"load":{"destinationTable":{"projectId":"gone","datasetId":"d","tableId":"t"}}}}`, true},
		{"/bigquery/v2/projects/home/queries", `{"query":"SELECT * FROM other.d.t"}`, false},
		{"/bigquery/v2/projects/home/queries", `{"query":"SELECT 'gone.d.t'"}`, false},
	} {
		reached = false
		req := httptest.NewRequest(http.MethodPost, c.path, strings.NewReader(c.body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if c.refused {
			if rec.Code != http.StatusForbidden || reached || !strings.Contains(rec.Body.String(), "Project gone: the project has been deleted") {
				t.Errorf("%s: HTTP %d %s (engine reached %v), want 403 naming gone", c.body, rec.Code, rec.Body, reached)
			}
		} else if !reached || rec.Code != http.StatusOK {
			t.Errorf("%s: HTTP %d %s (engine reached %v), want served", c.body, rec.Code, rec.Body, reached)
		}
	}
}
