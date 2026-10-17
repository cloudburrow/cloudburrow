package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"cloud.google.com/go/logging/apiv2/loggingpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/logging"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// sinksConsole serves the Log router and Resource hierarchy screens over an
// in-process Logging service that knows a Resource Manager registry, as up
// wires them, with one folder.
func sinksConsole(t *testing.T) (*httptest.Server, *loggingService, hierarchyProvider, string) {
	t.Helper()
	reg := resourcemanager.New(store.NewMemory())
	st := logging.NewStore(100)
	svc := &loggingService{store: st, api: logging.NewServer(st), admin: logging.NewAdmin(store.NewMemory()), hierarchy: reg}
	svc.api.UseAdmin(svc.admin)
	svc.admin.UseHierarchy(loggingHierarchy{reg})
	screen := loggingAdminScreen{svc: svc}
	h := hierarchyProvider{registry: reg, sinks: &screen}
	id, err := h.Create(context.Background(), "", map[string]string{"displayName": "Engineering", "parent": resourcemanager.OrganizationName})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(console.New("127.0.0.1:0", nil, loggingSinksProvider{screen}, h).Handler())
	t.Cleanup(srv.Close)
	return srv, svc, h, "folders/" + id
}

// TestConsoleFolderAndOrganizationSinks (#1572): the Log router's Parent
// picker offers the organization and its folders; picked, the screen lists,
// creates, edits and deletes that parent's sinks through ConfigServiceV2,
// with include_children, intercept_children, the sink service and the
// exclusions, which GetSink reads back. intercept_children's documented
// rules are the API's refusals, shown on the form. The folder's and
// organization's pages list their sinks, each linking to its Log router
// page, and link to the Log router screen.
func TestConsoleFolderAndOrganizationSinks(t *testing.T) {
	srv, svc, h, folder := sinksConsole(t)
	ctx := context.Background()
	const project = "proj-1"
	p := loggingSinksProvider{loggingAdminScreen{svc: svc}}

	plain, err := p.List(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	opts := fmt.Sprint(plain.Scope.Options)
	if plain.Scope.Selected != "projects/"+project || !strings.Contains(opts, folder) || !strings.Contains(opts, resourcemanager.OrganizationName) {
		t.Fatalf("the picker is %+v; want the project selected, the organization and %s offered", plain.Scope, folder)
	}
	if strings.Contains(fmt.Sprint(plain.Columns), "Intercept children") {
		t.Errorf("a project's sinks have an Intercept children column: %v", plain.Columns)
	}

	sinks := srv.URL + "/api/resources/logging-sinks?project=" + project + "&parent=" + url.QueryEscape(folder)
	base := map[string]string{"name": "central", "service": otherProject, "destination": project,
		"filter": "severity>=ERROR", "include_children": "true", "intercept_children": "true",
		"exclusions": `{"no-debug":"severity=DEBUG"}`}
	with := func(k, v string) map[string]string {
		m := map[string]string{}
		for a, b := range base {
			m[a] = b
		}
		m[k] = v
		return m
	}
	// The documented rules, as the API words them, on the form.
	for _, c := range []struct {
		values map[string]string
		want   string
	}{
		{with("include_children", "false"), "include_children"},
		{with("service", "Cloud Storage bucket"), "Cloud project"},
	} {
		if code, out := consolePost(t, sinks, c.values); code == http.StatusOK || !strings.Contains(out, c.want) {
			t.Errorf("create %v = %d %s; want a refusal naming %q", c.values, code, out, c.want)
		}
	}
	projectSinks := srv.URL + "/api/resources/logging-sinks?project=" + project
	if code, out := consolePost(t, projectSinks, base); code == http.StatusOK || !strings.Contains(out, "organizations and folders") {
		t.Errorf("an intercepting project sink = %d %s", code, out)
	}

	if code, out := consolePost(t, sinks, base); code != http.StatusOK {
		t.Fatalf("create = %d %s", code, out)
	}
	name := folder + "/sinks/central"
	sk, err := svc.admin.ConfigServer().GetSink(ctx, &loggingpb.GetSinkRequest{SinkName: name})
	if err != nil {
		t.Fatal(err)
	}
	if sk.GetDestination() != "logging.googleapis.com/projects/"+project || !sk.GetIncludeChildren() || !logging.SinkIntercepts(sk) ||
		len(sk.GetExclusions()) != 1 || sk.GetExclusions()[0].GetFilter() != "severity=DEBUG" {
		t.Fatalf("the API reads %v (intercept %v)", sk, logging.SinkIntercepts(sk))
	}

	fctx := console.WithScopeValue(ctx, bucketScopeKey, folder)
	l, err := p.List(fctx, project)
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Items) != 1 || l.Items[0].Name != name || l.Items[0].Fields["Intercept children"] != "Yes" || l.Items[0].Fields["Include children"] != "Yes" {
		t.Fatalf("the folder's sinks are %+v", l.Items)
	}
	var d console.Detail
	getJSON200(t, srv.URL+"/api/detail/logging-sinks?project="+project+"&parent="+url.QueryEscape(folder)+"&name="+url.QueryEscape(name), &d)
	text := loggingDetailText(d)
	if d.Edit == nil || !strings.Contains(text, `"Intercept children","value":"Yes"`) || !strings.Contains(text, "no-debug") {
		t.Fatalf("the sink page = %s", text)
	}

	// Edit: intercepting is turned off, an exclusion added, the destination
	// moved to a log bucket.
	edit := with("intercept_children", "false")
	edit["service"] = "Cloud Logging bucket"
	edit["destination"] = "projects/" + project + "/locations/global/buckets/_Default"
	edit["exclusions"] = `{"no-debug":"severity=DEBUG","no-info":"severity=INFO"}`
	if code, out := consolePatch(t, sinks, []string{name}, edit); code != http.StatusOK {
		t.Fatalf("edit = %d %s", code, out)
	}
	sk, _ = svc.admin.ConfigServer().GetSink(ctx, &loggingpb.GetSinkRequest{SinkName: name})
	if logging.SinkIntercepts(sk) || sk.GetDestination() != "logging.googleapis.com/"+edit["destination"] || len(sk.GetExclusions()) != 2 {
		t.Fatalf("after edit the API reads %v (intercept %v)", sk, logging.SinkIntercepts(sk))
	}
	// Turning it back on with a bucket destination is refused on the form.
	if code, out := consolePatch(t, sinks, []string{name}, with("destination", "logging.googleapis.com/"+edit["destination"])); code == http.StatusOK || !strings.Contains(out, "Cloud project") {
		t.Errorf("intercepting into a bucket = %d %s", code, out)
	}

	// The folder's page lists it, linking to its Log router page.
	fd, err := h.Detail(ctx, "", []string{strings.TrimPrefix(folder, "folders/")})
	if err != nil {
		t.Fatal(err)
	}
	var tab *console.Section
	for i := range fd.Sections {
		if fd.Sections[i].ID == "log-router" {
			tab = &fd.Sections[i]
		}
	}
	if tab == nil || len(tab.Listing.Items) != 1 || tab.Listing.Items[0].Link != sinkLink(name, folder) {
		t.Fatalf("the folder's Log router tab is %+v", tab)
	}
	if !strings.Contains(loggingDetailText(fd), url.QueryEscape(folder)) {
		t.Errorf("the folder's page does not link to its Log router: %s", loggingDetailText(fd))
	}

	// The organization's sink, from its page.
	org := resourcemanager.OrganizationName
	octx := console.WithScopeValue(ctx, bucketScopeKey, org)
	if _, err := p.Create(octx, project, map[string]string{"name": "audit", "service": "Pub/Sub topic",
		"destination": "projects/" + project + "/topics/audit", "include_children": "true"}); err != nil {
		t.Fatal(err)
	}
	od, _ := h.Detail(ctx, "", []string{organizationSegment})
	if !strings.Contains(loggingDetailText(od), org+"/sinks/audit") || !strings.Contains(loggingDetailText(od), sinkListLink(org)) {
		t.Errorf("the organization's page = %s", loggingDetailText(od))
	}

	// Another parent's sink is not reachable from this one.
	if err := p.Delete(octx, project, name); err == nil {
		t.Error("deleting the folder's sink from the organization's screen succeeded")
	}
	if code, out := consoleDelete(t, srv, "logging-sinks", project, name); code == http.StatusOK {
		t.Errorf("deleting the folder's sink from the project's screen = %d %s", code, out)
	}
	if err := p.Delete(fctx, project, name); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.admin.ConfigServer().GetSink(ctx, &loggingpb.GetSinkRequest{SinkName: name}); err == nil {
		t.Error("the deleted sink is still there")
	}
}

// TestSinkDestinationFromService: the sink service's prefix is added to what
// follows it, and a whole destination is taken as it is.
func TestSinkDestinationFromService(t *testing.T) {
	for _, c := range []struct{ service, dest, want string }{
		{"Cloud Storage bucket", "gs://logs/", "storage.googleapis.com/logs"},
		{otherProject, "p-2", "logging.googleapis.com/projects/p-2"},
		{"BigQuery dataset", "projects/p/datasets/d", "bigquery.googleapis.com/projects/p/datasets/d"},
		{"Pub/Sub topic", "pubsub.googleapis.com/projects/p/topics/t", "pubsub.googleapis.com/projects/p/topics/t"},
		{"", "projects/p/locations/global/buckets/b", "logging.googleapis.com/projects/p/locations/global/buckets/b"},
	} {
		got, err := sinkDestination(c.service, c.dest)
		if err != nil || got != c.want {
			t.Errorf("sinkDestination(%q, %q) = %q, %v; want %q", c.service, c.dest, got, err, c.want)
		}
		if c.service != "" && sinkServiceOf(got) != c.service {
			t.Errorf("sinkServiceOf(%q) = %q; want %q", got, sinkServiceOf(got), c.service)
		}
	}
	if _, err := sinkDestination("FTP", "x"); err == nil {
		t.Error("an unknown service was accepted")
	}
}
