//go:build compat

package compat

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"testing"

	vlogging "cloud.google.com/go/logging/apiv2"
	"cloud.google.com/go/logging/apiv2/loggingpb"
	logging "google.golang.org/api/logging/v2"
	observability "google.golang.org/api/observability/v1"
	"google.golang.org/api/option"
	mrpb "google.golang.org/genproto/googleapis/api/monitoredres"
)

// observabilityService is Google's generated Observability v1 JSON client
// (google.golang.org/api/observability/v1; Google publishes no other Go
// client for it), on the Logging port, where CloudBurrow serves its scopes.
func observabilityService(t *testing.T, h *Harness) *observability.Service {
	t.Helper()
	svc, err := observability.NewService(h.Context(), option.WithEndpoint("http://"+h.Endpoint(EnvLogging)+"/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatalf("observability.NewService: %v", err)
	}
	return svc
}

// covers: google.logging.v2.LoggingServiceV2/WriteLogEntries
//
// TestObservabilityDefaultLogScope (#2020), through Google's generated
// Observability v1 and Logging v2 JSON clients: a project's _Default scope
// starts on its own _Default log scope with no updateTime; a log scope of
// two projects is set as the default through scopes.patch and read back
// through scopes.get; and entries.list over that scope's resource names,
// as the Logs Explorer searches a log scope, returns the entries of both
// projects. A scope other than _Default is NOT_FOUND, another location, a
// log scope of another project, an unknown mask path and a scope of six
// projects are INVALID_ARGUMENT, a missing log scope is NOT_FOUND, and
// deleting the default log scope makes _Default the default again.
func TestObservabilityDefaultLogScope(t *testing.T) {
	h := New(t)
	obs := observabilityService(t, h)
	lg := loggingJSONService(t, h)
	ctx := h.Context()
	pa, pb := h.Project(), h.Project()+"-obs2020"
	scopes := obs.Projects.Locations.Scopes
	name := "projects/" + pa + "/locations/global/scopes/_Default"

	sc, err := scopes.Get(name).Context(ctx).Do()
	if err != nil {
		t.Fatalf("scopes.get: %v", err)
	}
	if sc.Name != name || sc.LogScope != "//logging.googleapis.com/projects/"+pa+"/locations/global/logScopes/_Default" || sc.UpdateTime != "" {
		t.Errorf("the initial _Default scope = %+v", sc)
	}

	// Entries in two projects, through the official gRPC client.
	wc, err := vlogging.NewClient(ctx, loggingClientOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = wc.Close() })
	for _, p := range []string{pa, pb} {
		if _, err := wc.WriteLogEntries(ctx, &loggingpb.WriteLogEntriesRequest{
			LogName:  "projects/" + p + "/logs/obs2020",
			Resource: &mrpb.MonitoredResource{Type: "global"},
			Entries:  []*loggingpb.LogEntry{{Payload: &loggingpb.LogEntry_TextPayload{TextPayload: "from " + p}}},
		}); err != nil {
			t.Fatalf("WriteLogEntries %s: %v", p, err)
		}
	}

	ls := lg.Projects.Locations.LogScopes
	parent := "projects/" + pa + "/locations/global"
	scope, err := ls.Create(parent, &logging.LogScope{ResourceNames: []string{"projects/" + pa, "projects/" + pb},
		Description: "two projects"}).LogScopeId("two").Context(ctx).Do()
	if err != nil {
		t.Fatalf("logScopes.create: %v", err)
	}
	t.Cleanup(func() { _, _ = ls.Delete(scope.Name).Context(context.Background()).Do() })
	six := []string{}
	for _, s := range []string{"a", "b", "c", "d", "e", "f"} {
		six = append(six, "projects/"+pa+"-"+s)
	}
	if _, err := ls.Create(parent, &logging.LogScope{ResourceNames: six}).LogScopeId("six").Context(ctx).Do(); httpCode(err) != http.StatusBadRequest {
		t.Errorf("a log scope of six projects: %v; want 400", err)
	}

	full := "//logging.googleapis.com/" + scope.Name
	got, err := scopes.Patch(name, &observability.Scope{LogScope: full}).UpdateMask("logScope").Context(ctx).Do()
	if err != nil {
		t.Fatalf("scopes.patch: %v", err)
	}
	if got.LogScope != full || got.UpdateTime == "" || got.TraceScope == "" {
		t.Errorf("scopes.patch = %+v", got)
	}
	sc, err = scopes.Get(name).Context(ctx).Do()
	if err != nil || sc.LogScope != full {
		t.Fatalf("scopes.get after patch = %+v, %v", sc, err)
	}

	// The Explorer's read of the default log scope: its resource names.
	def, err := ls.Get(strings.TrimPrefix(sc.LogScope, "//logging.googleapis.com/")).Context(ctx).Do()
	if err != nil {
		t.Fatalf("logScopes.get: %v", err)
	}
	resp, err := lg.Entries.List(&logging.ListLogEntriesRequest{ResourceNames: def.ResourceNames,
		PageSize: 1000}).Context(ctx).Do()
	if err != nil {
		t.Fatalf("entries.list: %v", err)
	}
	var texts []string
	for _, e := range resp.Entries {
		if strings.HasSuffix(e.LogName, "/logs/obs2020") {
			texts = append(texts, e.TextPayload)
		}
	}
	sort.Strings(texts)
	if strings.Join(texts, "|") != "from "+pa+"|from "+pb {
		t.Errorf("entries.list over the scope = %q; want an entry from each project", texts)
	}

	// Refusals.
	if _, err := scopes.Get("projects/" + pa + "/locations/global/scopes/other").Context(ctx).Do(); httpCode(err) != http.StatusNotFound {
		t.Errorf("scopes.get other: %v; want 404", err)
	}
	if _, err := scopes.Get("projects/" + pa + "/locations/us-central1/scopes/_Default").Context(ctx).Do(); httpCode(err) != http.StatusBadRequest {
		t.Errorf("scopes.get in us-central1: %v; want 400", err)
	}
	for what, c := range map[string]struct {
		scope *observability.Scope
		mask  string
		want  int
	}{
		"another project's log scope": {&observability.Scope{LogScope: "//logging.googleapis.com/projects/" + pb + "/locations/global/logScopes/two"}, "logScope", http.StatusBadRequest},
		"a missing log scope":         {&observability.Scope{LogScope: "//logging.googleapis.com/" + parent + "/logScopes/none"}, "logScope", http.StatusNotFound},
		"an unqualified log scope":    {&observability.Scope{LogScope: scope.Name}, "logScope", http.StatusBadRequest},
		"an unknown mask path":        {&observability.Scope{LogScope: full}, "name", http.StatusBadRequest},
	} {
		if _, err := scopes.Patch(name, c.scope).UpdateMask(c.mask).Context(ctx).Do(); httpCode(err) != c.want {
			t.Errorf("scopes.patch with %s: %v; want %d", what, err, c.want)
		}
	}

	if _, err := ls.Delete(scope.Name).Context(ctx).Do(); err != nil {
		t.Fatalf("logScopes.delete: %v", err)
	}
	sc, err = scopes.Get(name).Context(ctx).Do()
	if err != nil || !strings.HasSuffix(sc.LogScope, "/logScopes/_Default") {
		t.Errorf("after deleting the default log scope, scopes.get = %+v, %v", sc, err)
	}
}

// TestGcloudObservabilityScopes (#2020): gcloud observability scopes
// update --log-scope sets a project's default log scope and describe
// reads it back, through the observability override gcloud-setup writes.
func TestGcloudObservabilityScopes(t *testing.T) {
	h := New(t)
	lg := loggingJSONService(t, h) // asks for the Logging endpoint first
	g := newGcloudSession(t, h)
	ctx := h.Context()
	p := "--project=" + h.Project()
	parent := "projects/" + h.Project() + "/locations/global"
	ls := lg.Projects.Locations.LogScopes
	scope, err := ls.Create(parent, &logging.LogScope{ResourceNames: []string{"projects/" + h.Project()}}).LogScopeId("gcloud2020").Context(ctx).Do()
	if err != nil {
		t.Fatalf("logScopes.create: %v", err)
	}
	t.Cleanup(func() { _, _ = ls.Delete(scope.Name).Context(context.Background()).Do() })

	if got := g.must("observability", "scopes", "describe", "_Default", "--location=global", p, "--format=value(logScope)"); strings.TrimSpace(got) !=
		"//logging.googleapis.com/"+parent+"/logScopes/_Default" {
		t.Errorf("scopes describe = %q", got)
	}
	g.must("observability", "scopes", "update", "_Default", "--location=global", p, "--log-scope=//logging.googleapis.com/"+scope.Name)
	if got := g.must("observability", "scopes", "describe", "_Default", "--location=global", p, "--format=value(logScope)"); strings.TrimSpace(got) !=
		"//logging.googleapis.com/"+scope.Name {
		t.Errorf("scopes describe after update = %q", got)
	}
}
