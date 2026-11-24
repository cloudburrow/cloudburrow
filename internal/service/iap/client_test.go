package iap

import (
	"context"
	"net/http/httptest"
	"testing"

	"cloud.google.com/go/iam/apiv1/iampb"
	iapclient "cloud.google.com/go/iap/apiv1"
	"cloud.google.com/go/iap/apiv1/iappb"
	"google.golang.org/api/option"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// TestOfficialRESTClient: the official iap/apiv1 REST clients against the
// JSON API.
func TestOfficialRESTClient(t *testing.T) {
	s, _ := New(Options{})
	srv := httptest.NewServer(NewRESTHandler(s))
	defer srv.Close()
	ctx := context.Background()
	c, err := iapclient.NewIdentityAwareProxyAdminRESTClient(ctx, option.WithEndpoint(srv.URL), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: svc, Policy: &iampb.Policy{
		Bindings: []*iampb.Binding{{Role: "roles/iap.httpsResourceAccessor", Members: []string{"user:a@example.com"}}}}}); err != nil {
		t.Fatal(err)
	}
	if p, err := c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: svc}); err != nil || len(p.GetBindings()) != 1 {
		t.Fatalf("get: %v %v", p, err)
	}
	st, err := c.UpdateIapSettings(ctx, &iappb.UpdateIapSettingsRequest{IapSettings: &iappb.IapSettings{Name: svc,
		AccessSettings: &iappb.AccessSettings{CorsSettings: &iappb.CorsSettings{AllowHttpOptions: wrapperspb.Bool(true)}}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"access_settings.cors_settings.allow_http_options"}}})
	if err != nil || !st.GetAccessSettings().GetCorsSettings().GetAllowHttpOptions().GetValue() {
		t.Fatalf("settings: %v %v", st, err)
	}
	if _, err := c.GetIapSettings(ctx, &iappb.GetIapSettingsRequest{Name: svc}); err != nil {
		t.Fatal(err)
	}
	o, _ := iapclient.NewIdentityAwareProxyOAuthRESTClient(ctx, option.WithEndpoint(srv.URL), option.WithoutAuthentication())
	defer o.Close()
	if _, err := o.ListBrands(ctx, &iappb.ListBrandsRequest{Parent: "projects/p"}); err != nil {
		t.Fatal(err)
	}
}
