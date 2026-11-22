package apphub

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	apphubpb "cloud.google.com/go/apphub/apiv1/apphubpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

const runURI = "//run.googleapis.com/projects/svc-project-1/locations/us-central1/services/hello"

func testServer(t *testing.T, state string, live map[string]bool) *Server {
	t.Helper()
	s, err := NewServer(state, Options{Discover: func(_ context.Context, project string) ([]Resource, error) {
		var out []Resource
		if project == "svc-project-1" && live[runURI] {
			out = append(out, Resource{URI: runURI, Project: project, Location: "us-central1"})
		}
		if project == "svc-project-1" {
			out = append(out, Resource{URI: "//container.googleapis.com/projects/svc-project-1/locations/us-central1/clusters/c1",
				Project: project, Location: "us-central1", Shared: true})
		}
		return out, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func code(err error) codes.Code { return status.Code(err) }

func TestAttachDiscoverRegister(t *testing.T) {
	ctx := context.Background()
	live := map[string]bool{runURI: true}
	state := filepath.Join(t.TempDir(), "apphub.json")
	s := testServer(t, state, live)
	host := "projects/host-project-1/locations/global"
	if _, err := s.CreateServiceProjectAttachment(ctx, &apphubpb.CreateServiceProjectAttachmentRequest{Parent: host,
		ServiceProjectAttachmentId: "svc-project-1", ServiceProjectAttachment: &apphubpb.ServiceProjectAttachment{ServiceProject: "projects/svc-project-1"}}); err != nil {
		t.Fatal(err)
	}
	// One host at a time.
	_, err := s.CreateServiceProjectAttachment(ctx, &apphubpb.CreateServiceProjectAttachmentRequest{Parent: "projects/host-project-2/locations/global",
		ServiceProjectAttachmentId: "svc-project-1", ServiceProjectAttachment: &apphubpb.ServiceProjectAttachment{ServiceProject: "projects/svc-project-1"}})
	if code(err) != codes.AlreadyExists {
		t.Fatalf("a second host: %v", err)
	}
	lk, err := s.LookupServiceProjectAttachment(ctx, &apphubpb.LookupServiceProjectAttachmentRequest{Name: "projects/svc-project-1/locations/global"})
	if err != nil || !strings.HasPrefix(lk.GetServiceProjectAttachment().GetServiceProject(), "projects/") || lk.GetServiceProjectAttachment().GetServiceProject() == "projects/svc-project-1" {
		t.Fatalf("lookup: %v, %v", lk, err)
	}
	if lk, _ := s.LookupServiceProjectAttachment(ctx, &apphubpb.LookupServiceProjectAttachmentRequest{Name: "projects/other-project/locations/global"}); lk.GetServiceProjectAttachment() != nil {
		t.Fatalf("an unattached project's lookup: %v", lk)
	}
	ds, err := s.LookupDiscoveredService(ctx, &apphubpb.LookupDiscoveredServiceRequest{Parent: "projects/host-project-1/locations/us-central1", Uri: runURI})
	if err != nil || ds.GetDiscoveredService() == nil {
		t.Fatalf("lookup discovered: %v, %v", ds, err)
	}
	if l, _ := s.ListDiscoveredServices(ctx, &apphubpb.ListDiscoveredServicesRequest{Parent: "projects/host-project-1/locations/us-central1"}); len(l.GetDiscoveredServices()) != 2 {
		t.Fatalf("list discovered: %v", l)
	}
	// A regional application in another region cannot register it.
	for _, a := range []struct {
		loc, id string
		scope   apphubpb.Scope_Type
	}{{"us-east1", "east", apphubpb.Scope_REGIONAL}, {"global", "glob", apphubpb.Scope_GLOBAL}} {
		if _, err := s.CreateApplication(ctx, &apphubpb.CreateApplicationRequest{Parent: "projects/host-project-1/locations/" + a.loc, ApplicationId: a.id,
			Application: &apphubpb.Application{Scope: &apphubpb.Scope{Type: a.scope}, Attributes: &apphubpb.Attributes{
				Criticality: &apphubpb.Criticality{Type: apphubpb.Criticality_HIGH}, Environment: &apphubpb.Environment{Type: apphubpb.Environment_PRODUCTION}}}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.CreateApplication(ctx, &apphubpb.CreateApplicationRequest{Parent: "projects/host-project-1/locations/us-east1", ApplicationId: "bad",
		Application: &apphubpb.Application{Scope: &apphubpb.Scope{Type: apphubpb.Scope_GLOBAL}}}); code(err) != codes.InvalidArgument {
		t.Fatalf("a GLOBAL application in a region: %v", err)
	}
	disc := ds.GetDiscoveredService().GetName()
	_, err = s.CreateService(ctx, &apphubpb.CreateServiceRequest{Parent: "projects/host-project-1/locations/us-east1/applications/east", ServiceId: "hello",
		Service: &apphubpb.Service{DiscoveredService: disc}})
	if code(err) != codes.InvalidArgument {
		t.Fatalf("a us-central1 service in a us-east1 application: %v", err)
	}
	app := "projects/host-project-1/locations/global/applications/glob"
	if _, err := s.CreateService(ctx, &apphubpb.CreateServiceRequest{Parent: app, ServiceId: "hello",
		Service: &apphubpb.Service{DiscoveredService: disc, Attributes: &apphubpb.Attributes{Environment: &apphubpb.Environment{Type: apphubpb.Environment_STAGING}}}}); err != nil {
		t.Fatal(err)
	}
	refs := s.RefsFor(runURI)
	if len(refs) != 1 || refs[0].ServiceID != "hello" || refs[0].Environment != "STAGING" || refs[0].Labels()["apphub_application_id"] != "glob" {
		t.Fatalf("RefsFor: %+v", refs)
	}
	if _, err := s.UpdateService(ctx, &apphubpb.UpdateServiceRequest{Service: &apphubpb.Service{Name: app + "/services/hello", DisplayName: "Hello"},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"display_name"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteApplication(ctx, &apphubpb.DeleteApplicationRequest{Name: app}); code(err) != codes.FailedPrecondition {
		t.Fatalf("deleting an application with a service: %v", err)
	}
	// The state survives a restart; the resource's deletion detaches it.
	live[runURI] = false
	s2 := testServer(t, state, live)
	got, err := s2.GetService(ctx, &apphubpb.GetServiceRequest{Name: app + "/services/hello"})
	if err != nil || got.GetState() != apphubpb.Service_DETACHED || got.GetDisplayName() != "Hello" {
		t.Fatalf("after the restart and the deletion: %v, %v", got, err)
	}
	if _, err := s2.DetachServiceProjectAttachment(ctx, &apphubpb.DetachServiceProjectAttachmentRequest{Name: "projects/svc-project-1/locations/global"}); err != nil {
		t.Fatal(err)
	}
	if l, _ := s2.ListDiscoveredServices(ctx, &apphubpb.ListDiscoveredServicesRequest{Parent: "projects/host-project-1/locations/us-central1"}); len(l.GetDiscoveredServices()) != 0 {
		t.Fatalf("discovered after the detach: %v", l)
	}
}

func TestBoundaryAndSchemas(t *testing.T) {
	ctx := context.Background()
	s := testServer(t, "", nil)
	name := "projects/host-project-1/locations/global/boundary"
	b, err := s.GetBoundary(ctx, &apphubpb.GetBoundaryRequest{Name: name})
	if err != nil || b.GetType() != apphubpb.Boundary_MANUAL {
		t.Fatalf("boundary: %v, %v", b, err)
	}
	op, err := s.UpdateBoundary(ctx, &apphubpb.UpdateBoundaryRequest{Boundary: &apphubpb.Boundary{Name: name, Scope: &apphubpb.Boundary_CrmNode{CrmNode: "projects/svc-project-1"}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"crm_node"}}})
	if err != nil || !op.GetDone() {
		t.Fatal(op, err)
	}
	if l, _ := s.ListDiscoveredServices(ctx, &apphubpb.ListDiscoveredServicesRequest{Parent: "projects/host-project-1/locations/us-central1"}); len(l.GetDiscoveredServices()) != 1 {
		t.Fatalf("discovered through the boundary's node: %v", l)
	}
	l, err := s.ListExtendedMetadataSchemas(ctx, &apphubpb.ListExtendedMetadataSchemasRequest{Parent: "projects/host-project-1/locations/global"})
	if err != nil || len(l.GetExtendedMetadataSchemas()) != 3 {
		t.Fatal(l, err)
	}
	g, err := s.GetExtendedMetadataSchema(ctx, &apphubpb.GetExtendedMetadataSchemaRequest{Name: "projects/host-project-1/locations/global/extendedMetadataSchemas/apphub.googleapis.com/McpServer"})
	if err != nil || !strings.Contains(g.GetJsonSchema(), "interfaces") {
		t.Fatal(g, err)
	}
}

func TestRESTRoutes(t *testing.T) {
	s := testServer(t, "", nil)
	srv := httptest.NewServer(NewRESTHandler(s))
	defer srv.Close()
	for _, p := range []string{"/v1/projects/host-project-1/locations", "/v1/projects/host-project-1/locations/global/applications",
		"/v1/projects/host-project-1/locations/global:lookupServiceProjectAttachment"} {
		resp, err := http.Get(srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Errorf("GET %s: %d", p, resp.StatusCode)
		}
	}
}

// TestMultiRegionalApplications (#2110): a MULTI_REGIONAL application lives
// in us or eu and registers its multi-region's regions' resources only;
// the JSON API names the scope MULTI_REGIONAL both ways.
func TestMultiRegionalApplications(t *testing.T) {
	ctx := context.Background()
	s := testServer(t, "", map[string]bool{runURI: true})
	if _, err := s.CreateServiceProjectAttachment(ctx, &apphubpb.CreateServiceProjectAttachmentRequest{Parent: "projects/host-project-1/locations/global",
		ServiceProjectAttachmentId: "svc-project-1", ServiceProjectAttachment: &apphubpb.ServiceProjectAttachment{ServiceProject: "projects/svc-project-1"}}); err != nil {
		t.Fatal(err)
	}
	mk := func(loc, id string, scope apphubpb.Scope_Type) error {
		_, err := s.CreateApplication(ctx, &apphubpb.CreateApplicationRequest{Parent: "projects/host-project-1/locations/" + loc, ApplicationId: id,
			Application: &apphubpb.Application{Scope: &apphubpb.Scope{Type: scope}}})
		return err
	}
	for _, c := range []struct {
		loc   string
		scope apphubpb.Scope_Type
	}{{"global", ScopeMultiRegional}, {"us-central1", ScopeMultiRegional}, {"us", apphubpb.Scope_REGIONAL}, {"us", apphubpb.Scope_GLOBAL}} {
		if err := mk(c.loc, "bad", c.scope); code(err) != codes.InvalidArgument {
			t.Errorf("%v in %s: %v, want INVALID_ARGUMENT", c.scope, c.loc, err)
		}
	}
	if err := mk("us", "multi", ScopeMultiRegional); err != nil {
		t.Fatal(err)
	}
	if err := mk("eu", "multi-eu", ScopeMultiRegional); err != nil {
		t.Fatal(err)
	}
	ds, err := s.LookupDiscoveredService(ctx, &apphubpb.LookupDiscoveredServiceRequest{Parent: "projects/host-project-1/locations/us-central1", Uri: runURI})
	if err != nil || ds.GetDiscoveredService() == nil {
		t.Fatal(ds, err)
	}
	reg := func(app string) error {
		_, err := s.CreateService(ctx, &apphubpb.CreateServiceRequest{Parent: app, ServiceId: "hello",
			Service: &apphubpb.Service{DiscoveredService: ds.GetDiscoveredService().GetName()}})
		return err
	}
	if err := reg("projects/host-project-1/locations/eu/applications/multi-eu"); code(err) != codes.InvalidArgument {
		t.Errorf("a us-central1 service in an eu application: %v, want INVALID_ARGUMENT", err)
	}
	if err := reg("projects/host-project-1/locations/us/applications/multi"); err != nil {
		t.Errorf("a us-central1 service in a us application: %v", err)
	}
	for _, c := range []struct{ multi, loc string }{{"us", "us-east4"}, {"eu", "europe-west1"}, {"eu", "europe-west2"}, {"us", "europe-west1"}} {
		want := !(c.loc == "europe-west2" || c.multi == "us" && c.loc == "europe-west1")
		if got := InMultiRegion(c.multi, c.loc); got != want {
			t.Errorf("InMultiRegion(%s, %s) = %v", c.multi, c.loc, got)
		}
	}

	srv := httptest.NewServer(NewRESTHandler(s))
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/v1/projects/host-project-1/locations/eu/applications?applicationId=rest-multi", "application/json",
		strings.NewReader(`{"scope":{"type":"MULTI_REGIONAL"},"displayName":"<rest>"}`))
	if err != nil {
		t.Fatal(err)
	}
	body := readAll(t, resp)
	if resp.StatusCode != 200 || !strings.Contains(body, `"MULTI_REGIONAL"`) {
		t.Fatalf("create over JSON: %d %s", resp.StatusCode, body)
	}
	resp, err = http.Get(srv.URL + "/v1/projects/host-project-1/locations/eu/applications/rest-multi")
	if err != nil {
		t.Fatal(err)
	}
	if body := readAll(t, resp); !strings.Contains(body, `"scope":{"type":"MULTI_REGIONAL"}`) || !strings.Contains(body, "<rest>") {
		t.Fatalf("get over JSON: %s", body)
	}
}

func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	var b strings.Builder
	if _, err := io.Copy(&b, resp.Body); err != nil {
		t.Fatal(err)
	}
	return b.String()
}
