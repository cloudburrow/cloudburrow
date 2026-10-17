//go:build compat

package compat

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"

	vlogging "cloud.google.com/go/logging/apiv2"
	"cloud.google.com/go/logging/apiv2/loggingpb"
	logging "google.golang.org/api/logging/v2"
	"google.golang.org/api/option"
)

// loggingJSONService is Google's generated Logging v2 JSON client, the
// official client of the methods google.logging.v2's protos do not carry
// (saved queries, log scopes) and of the IAMPolicy mixin on log views.
func loggingJSONService(t *testing.T, h *Harness) *logging.Service {
	t.Helper()
	svc, err := logging.NewService(h.Context(), option.WithEndpoint("http://"+h.Endpoint(EnvLogging)+"/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatalf("logging.NewService: %v", err)
	}
	return svc
}

// loggingView makes a view on the project's _Default bucket through the
// official ConfigClient, and removes it when the test ends.
func loggingView(t *testing.T, h *Harness, id string) string {
	t.Helper()
	ctx := h.Context()
	cc, err := vlogging.NewConfigClient(ctx, loggingClientOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	parent := "projects/" + h.Project() + "/locations/global/buckets/_Default"
	v, err := cc.CreateView(ctx, &loggingpb.CreateViewRequest{Parent: parent, ViewId: id,
		View: &loggingpb.LogView{Filter: `LOG_ID("stdout")`}})
	if err != nil {
		t.Fatalf("CreateView: %v", err)
	}
	t.Cleanup(func() { _ = cc.DeleteView(context.Background(), &loggingpb.DeleteViewRequest{Name: v.GetName()}) })
	return v.GetName()
}

// covers: google.logging.v2.ConfigServiceV2/CreateView, google.logging.v2.ConfigServiceV2/DeleteView
//
// TestLoggingViewIAMSavedQueriesAndLogScopes (#1519), through Google's
// generated google.golang.org/api/logging/v2 JSON client: a log view's IAM
// policy is set, read back with its etag, tested (every permission, as
// nothing is enforced) and goes with the view; a missing view's is
// NOT_FOUND. A saved query is created (with and without an ID), read,
// listed, updated by mask, refused without exactly one query and with an
// unknown visibility, and deleted. A log scope is created in global (not
// another location), read, listed, updated, refused without resources, and
// deleted.
func TestLoggingViewIAMSavedQueriesAndLogScopes(t *testing.T) {
	h := New(t)
	svc := loggingJSONService(t, h)
	view := loggingView(t, h, "iam-1519")
	ctx := h.Context()
	p := "projects/" + h.Project()

	views := svc.Projects.Locations.Buckets.Views
	pol, err := views.SetIamPolicy(view, &logging.SetIamPolicyRequest{Policy: &logging.Policy{
		Bindings: []*logging.Binding{{Role: "roles/logging.viewAccessor", Members: []string{"user:v@example.com"}}}}}).Context(ctx).Do()
	if err != nil {
		t.Fatalf("SetIamPolicy: %v", err)
	}
	got, err := views.GetIamPolicy(view, &logging.GetIamPolicyRequest{}).Context(ctx).Do()
	if err != nil || len(got.Bindings) != 1 || got.Bindings[0].Members[0] != "user:v@example.com" || got.Etag != pol.Etag || got.Etag == "" {
		t.Errorf("GetIamPolicy = %+v, %v", got, err)
	}
	perms, err := views.TestIamPermissions(view, &logging.TestIamPermissionsRequest{Permissions: []string{"logging.views.access"}}).Context(ctx).Do()
	if err != nil || len(perms.Permissions) != 1 {
		t.Errorf("TestIamPermissions = %+v, %v", perms, err)
	}
	missing := strings.TrimSuffix(view, "iam-1519") + "no-such-view"
	if _, err := views.GetIamPolicy(missing, &logging.GetIamPolicyRequest{}).Context(ctx).Do(); httpCode(err) != http.StatusNotFound {
		t.Errorf("GetIamPolicy on a missing view = %v; want 404", err)
	}

	sq := svc.Projects.Locations.SavedQueries
	parent := p + "/locations/global"
	q, err := sq.Create(parent, &logging.SavedQuery{DisplayName: "Errors", Visibility: "PRIVATE",
		LoggingQuery: &logging.LoggingQuery{Filter: "severity>=ERROR", SummaryFields: []*logging.SummaryField{{Field: "jsonPayload.msg"}}}}).
		SavedQueryId("errors").Context(ctx).Do()
	if err != nil {
		t.Fatalf("SavedQueries.Create: %v", err)
	}
	if q.Name != parent+"/savedQueries/errors" || q.CreateTime == "" || q.LoggingQuery.SummaryFields[0].Field != "jsonPayload.msg" {
		t.Errorf("created %+v", q)
	}
	if _, err := sq.Create(parent, &logging.SavedQuery{DisplayName: "x", Visibility: "PRIVATE",
		LoggingQuery: &logging.LoggingQuery{Filter: "a"}}).SavedQueryId("errors").Context(ctx).Do(); httpCode(err) != http.StatusConflict {
		t.Errorf("a second create of one ID = %v; want 409", err)
	}
	if _, err := sq.Create(parent, &logging.SavedQuery{DisplayName: "x", Visibility: "PRIVATE"}).Context(ctx).Do(); httpCode(err) != http.StatusBadRequest {
		t.Errorf("a saved query with no query = %v; want 400", err)
	}
	if _, err := sq.Create(parent, &logging.SavedQuery{DisplayName: "x", Visibility: "PUBLIC",
		LoggingQuery: &logging.LoggingQuery{Filter: "a"}}).Context(ctx).Do(); httpCode(err) != http.StatusBadRequest {
		t.Errorf("visibility PUBLIC = %v; want 400", err)
	}
	sql, err := sq.Create(parent, &logging.SavedQuery{DisplayName: "SQL", Visibility: "SHARED",
		OpsAnalyticsQuery: &logging.OpsAnalyticsQuery{SqlQueryText: "SELECT 1"}}).Context(ctx).Do()
	if err != nil || !strings.HasPrefix(sql.Name, parent+"/savedQueries/") {
		t.Fatalf("a saved query with a made ID = %+v, %v", sql, err)
	}
	up, err := sq.Patch(q.Name, &logging.SavedQuery{Description: "all errors", Visibility: "SHARED"}).
		UpdateMask("description,visibility").Context(ctx).Do()
	if err != nil || up.Description != "all errors" || up.Visibility != "SHARED" || up.DisplayName != "Errors" {
		t.Errorf("Patch = %+v, %v", up, err)
	}
	list, err := sq.List(parent).Context(ctx).Do()
	var names []string
	if err == nil {
		for _, s := range list.SavedQueries {
			names = append(names, s.Name)
		}
	}
	sort.Strings(names)
	if err != nil || len(names) != 2 {
		t.Errorf("List = %v, %v; want both", names, err)
	}
	for _, n := range []string{q.Name, sql.Name} {
		if _, err := sq.Delete(n).Context(ctx).Do(); err != nil {
			t.Errorf("Delete %s: %v", n, err)
		}
	}
	if _, err := sq.Get(q.Name).Context(ctx).Do(); httpCode(err) != http.StatusNotFound {
		t.Errorf("Get after delete = %v; want 404", err)
	}

	ls := svc.Projects.Locations.LogScopes
	if _, err := ls.Create(p+"/locations/us-central1", &logging.LogScope{ResourceNames: []string{p}}).LogScopeId("s").Context(ctx).Do(); httpCode(err) != http.StatusBadRequest {
		t.Errorf("a log scope outside global = %v; want 400", err)
	}
	if _, err := ls.Create(parent, &logging.LogScope{}).LogScopeId("s").Context(ctx).Do(); httpCode(err) != http.StatusBadRequest {
		t.Errorf("a log scope with no resources = %v; want 400", err)
	}
	scope, err := ls.Create(parent, &logging.LogScope{ResourceNames: []string{p, view}, Description: "mine"}).LogScopeId("scope-1519").Context(ctx).Do()
	if err != nil {
		t.Fatalf("LogScopes.Create: %v", err)
	}
	if scope.Name != parent+"/logScopes/scope-1519" || len(scope.ResourceNames) != 2 {
		t.Errorf("created %+v", scope)
	}
	scope, err = ls.Patch(scope.Name, &logging.LogScope{ResourceNames: []string{p}}).UpdateMask("resourceNames").Context(ctx).Do()
	if err != nil || len(scope.ResourceNames) != 1 || scope.Description != "mine" {
		t.Errorf("Patch = %+v, %v", scope, err)
	}
	if got, err := ls.Get(scope.Name).Context(ctx).Do(); err != nil || got.ResourceNames[0] != p {
		t.Errorf("Get = %+v, %v", got, err)
	}
	if l, err := ls.List(parent).Context(ctx).Do(); err != nil || len(l.LogScopes) != 1 {
		t.Errorf("List = %+v, %v", l, err)
	}
	if _, err := ls.Delete(scope.Name).Context(ctx).Do(); err != nil {
		t.Errorf("Delete: %v", err)
	}
	if _, err := ls.Get(scope.Name).Context(ctx).Do(); httpCode(err) != http.StatusNotFound {
		t.Errorf("Get after delete = %v; want 404", err)
	}
}

// TestTerraformLogViewIAM (#1519): google_logging_log_view_iam_member and
// _binding, then _policy, on a view the official client made, apply
// through `cloudburrow terraform` (logging_custom_endpoint), read back with
// the generated client, plan clean, change in place, plan clean and are
// removed with the view kept.
func TestTerraformLogViewIAM(t *testing.T) {
	h := New(t)
	svc := loggingJSONService(t, h)
	view := loggingView(t, h, "tf-iam-1519")
	m := newTFModule(t)
	ctx := h.Context()
	p := h.Project()
	common := fmt.Sprintf(`
  parent   = "projects/%s"
  location = "global"
  bucket   = "_Default"
  name     = "tf-iam-1519"`, p)
	read := func(when string) map[string][]string {
		t.Helper()
		pol, err := svc.Projects.Locations.Buckets.Views.GetIamPolicy(view, &logging.GetIamPolicyRequest{}).Context(ctx).Do()
		if err != nil {
			t.Fatalf("%s, GetIamPolicy: %v", when, err)
		}
		out := map[string][]string{}
		for _, b := range pol.Bindings {
			out[b.Role] = append([]string(nil), b.Members...)
			sort.Strings(out[b.Role])
		}
		return out
	}
	members := func(role, member, bindingMembers string) string {
		return fmt.Sprintf(`
resource "google_logging_log_view_iam_member" "m" {%s
  role   = %q
  member = %q
}
resource "google_logging_log_view_iam_binding" "b" {%s
  role    = "roles/logging.viewer"
  members = [%s]
}
`, common, role, member, common, bindingMembers)
	}

	m.write(members("roles/logging.viewAccessor", "user:a@example.com", `"user:b@example.com"`))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	got := read("after apply")
	if strings.Join(got["roles/logging.viewAccessor"], ",") != "user:a@example.com" || strings.Join(got["roles/logging.viewer"], ",") != "user:b@example.com" {
		t.Errorf("after apply, policy = %v", got)
	}
	m.planClean()

	m.write(members("roles/logging.viewAccessor", "user:c@example.com", `"user:b@example.com", "user:d@example.com"`))
	m.apply()
	got = read("after the change")
	if strings.Join(got["roles/logging.viewAccessor"], ",") != "user:c@example.com" || strings.Join(got["roles/logging.viewer"], ",") != "user:b@example.com,user:d@example.com" {
		t.Errorf("after the change, policy = %v", got)
	}
	m.planClean()

	m.write(fmt.Sprintf(`
data "google_iam_policy" "p" {
  binding {
    role    = "roles/logging.viewAccessor"
    members = ["group:g@example.com"]
  }
}
resource "google_logging_log_view_iam_policy" "p" {%s
  policy_data = data.google_iam_policy.p.policy_data
}
`, common))
	m.apply()
	got = read("with the policy")
	if len(got) != 1 || strings.Join(got["roles/logging.viewAccessor"], ",") != "group:g@example.com" {
		t.Errorf("with the policy, policy = %v", got)
	}
	m.planClean()
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := svc.Projects.Locations.Buckets.Views.Get(view).Context(ctx).Do(); err != nil {
		t.Errorf("the view is gone after destroy: %v", err)
	}
}
