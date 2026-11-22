package iap

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"cloud.google.com/go/iam/apiv1/iampb"
	"cloud.google.com/go/iap/apiv1/iappb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

const svc = "projects/p/iap_web/compute/services/web"

func TestPoliciesSettingsAndLevels(t *testing.T) {
	ctx := context.Background()
	state := filepath.Join(t.TempDir(), "iap.json")
	s, err := New(Options{State: state, BackendService: func(rel string) bool { return rel == "projects/p/global/backendServices/web" },
		Normalize: func(n string) string {
			return strings.Replace(strings.Replace(n, "projects/123/", "projects/p/", 1), "/services/999", "/services/web", 1)
		}})
	if err != nil {
		t.Fatal(err)
	}
	if got := Levels(svc); !reflect.DeepEqual(got, []string{svc, "projects/p/iap_web/compute", "projects/p/iap_web"}) {
		t.Errorf("Levels: %v", got)
	}
	pol, err := s.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: "projects/123/iap_web/compute/services/999", Policy: &iampb.Policy{
		Bindings: []*iampb.Binding{{Role: "roles/iap.httpsResourceAccessor", Members: []string{"user:a@example.com"}}}}})
	if err != nil || len(pol.GetBindings()) != 1 {
		t.Fatalf("set: %v %v", pol, err)
	}
	if p := s.Policy(svc); p == nil || p.Bindings[0].Members[0] != "user:a@example.com" {
		t.Errorf("Policy by name: %v", p)
	}
	if _, err := s.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: "projects/p/iap_web/compute/services/none"}); status.Code(err) != codes.NotFound {
		t.Errorf("missing backend service: %v", err)
	}
	if _, err := s.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: "projects/p/buckets/x"}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("not an IAP resource: %v", err)
	}
	// Settings: the top level's CORS, the service's own login hint, merged.
	if _, err := s.UpdateIapSettings(ctx, &iappb.UpdateIapSettingsRequest{IapSettings: &iappb.IapSettings{Name: "projects/p/iap_web",
		AccessSettings: &iappb.AccessSettings{CorsSettings: &iappb.CorsSettings{AllowHttpOptions: wrapperspb.Bool(true)}}}}); err != nil {
		t.Fatal(err)
	}
	st, err := s.UpdateIapSettings(ctx, &iappb.UpdateIapSettingsRequest{IapSettings: &iappb.IapSettings{Name: svc,
		AccessSettings: &iappb.AccessSettings{OauthSettings: &iappb.OAuthSettings{LoginHint: wrapperspb.String("a@example.com")},
			CorsSettings: &iappb.CorsSettings{AllowHttpOptions: wrapperspb.Bool(false)}}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"access_settings.oauth_settings.login_hint"}}})
	if err != nil || st.GetAccessSettings().GetCorsSettings() != nil {
		t.Fatalf("masked update: %v %v", st, err)
	}
	m := s.Settings(svc)
	as, _ := m["accessSettings"].(map[string]any)
	if as == nil || as["corsSettings"] == nil || as["oauthSettings"] == nil {
		t.Errorf("merged settings: %v", m)
	}
	if _, err := s.UpdateIapSettings(ctx, &iappb.UpdateIapSettingsRequest{IapSettings: &iappb.IapSettings{Name: svc,
		ApplicationSettings: &iappb.ApplicationSettings{AccessDeniedPageSettings: &iappb.AccessDeniedPageSettings{AccessDeniedPageUri: wrapperspb.String("http://x")}}}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("http access-denied page: %v", err)
	}
	if _, err := s.ValidateIapAttributeExpression(ctx, &iappb.ValidateIapAttributeExpressionRequest{Name: svc,
		Expression: `attributes.saml_attributes.filter(attribute, attribute.name in ["test1", "test2"])`}); err != nil {
		t.Errorf("valid expression: %v", err)
	}
	if _, err := s.ValidateIapAttributeExpression(ctx, &iappb.ValidateIapAttributeExpressionRequest{Name: svc, Expression: `attributes.saml_attributes.filter(`}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("unbalanced expression: %v", err)
	}
	// Destination groups.
	parent := "projects/p/iap_tunnel/locations/us-central1"
	if _, err := s.CreateTunnelDestGroup(ctx, &iappb.CreateTunnelDestGroupRequest{Parent: parent, TunnelDestGroupId: "g",
		TunnelDestGroup: &iappb.TunnelDestGroup{Cidrs: []string{"10.0.0.0/24"}, Fqdns: []string{"db.example.com"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTunnelDestGroup(ctx, &iappb.CreateTunnelDestGroupRequest{Parent: parent, TunnelDestGroupId: "bad",
		TunnelDestGroup: &iappb.TunnelDestGroup{Cidrs: []string{"10.0.0.1/24"}}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("bad CIDR: %v", err)
	}
	// The deprecated OAuth Admin API.
	if _, err := s.CreateBrand(ctx, &iappb.CreateBrandRequest{Parent: "projects/p"}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("CreateBrand: %v", err)
	}
	if l, err := s.ListBrands(ctx, &iappb.ListBrandsRequest{Parent: "projects/p"}); err != nil || len(l.GetBrands()) != 0 {
		t.Errorf("ListBrands: %v %v", l, err)
	}
	// Restored.
	s2, err := New(Options{State: state})
	if err != nil || s2.Policy(svc) == nil || s2.Settings(svc) == nil {
		t.Errorf("restore: %v", err)
	}
	if err := s2.ResetProject("p"); err != nil || s2.Policy(svc) != nil {
		t.Errorf("reset: %v", err)
	}
}

func TestRESTRoutes(t *testing.T) {
	s, _ := New(Options{})
	srv := httptest.NewServer(NewRESTHandler(s))
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/v1/"+svc+":getIamPolicy", "application/json", strings.NewReader(`{}`))
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("getIamPolicy: %v %v", resp, err)
	}
	resp.Body.Close()
	resp, _ = http.Get(srv.URL + "/v1/" + svc + ":iapSettings")
	if resp.StatusCode != 200 {
		t.Errorf("iapSettings: %d", resp.StatusCode)
	}
	resp.Body.Close()
	for path, want := range map[string]bool{"/v1/" + svc + ":getIamPolicy": true, "/v1/projects/p/iap_web:iapSettings": true,
		"/v1/projects/p/brands": true, "/v1/projects/p/iap_tunnel/locations/us-central1/destGroups": true,
		"/v1/projects/p/locations/global/certificates": false, "/compute/v1/projects/p": false} {
		if Handles(path) != want {
			t.Errorf("Handles(%s) != %v", path, want)
		}
	}
}
