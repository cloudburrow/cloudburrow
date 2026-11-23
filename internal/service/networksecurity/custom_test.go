package networksecurity

import (
	"context"
	"net/http"
	"testing"

	nspb "cloud.google.com/go/networksecurity/apiv1/networksecuritypb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/cloudburrow/cloudburrow/internal/service/compute"
)

func TestCustomAuthzPolicies(t *testing.T) {
	s, _ := New(Options{})
	const r = "projects/p/regions/us-central1"
	const loc = "projects/p/locations/us-central1"
	rule := r + "/forwardingRules/fr"
	ext := loc + "/authzExtensions/ext"
	s.UseHooks(Hooks{
		ForwardingRule: func(rel string) (string, bool) {
			if rel == "projects/p/global/forwardingRules/x" {
				return "EXTERNAL_MANAGED", true
			}
			return "INTERNAL_MANAGED", rel == rule
		},
		AuthzExtension: func(rel string) (string, bool) { return "INTERNAL_MANAGED", rel == ext },
	})
	ctx := context.Background()
	target := &nspb.AuthzPolicy_Target{LoadBalancingScheme: nspb.AuthzPolicy_INTERNAL_MANAGED, Resources: []string{rule}}
	create := func(id string, p *nspb.AuthzPolicy) error {
		_, err := s.CreateAuthzPolicy(ctx, &nspb.CreateAuthzPolicyRequest{Parent: loc, AuthzPolicyId: id, AuthzPolicy: p})
		return err
	}
	custom := func(cp *nspb.AuthzPolicy_CustomProvider) *nspb.AuthzPolicy {
		return &nspb.AuthzPolicy{Target: target, Action: nspb.AuthzPolicy_CUSTOM, CustomProvider: cp}
	}
	for name, p := range map[string]*nspb.AuthzPolicy{
		"no provider": custom(nil),
		"both": custom(&nspb.AuthzPolicy_CustomProvider{CloudIap: &nspb.AuthzPolicy_CustomProvider_CloudIap{},
			AuthzExtension: &nspb.AuthzPolicy_CustomProvider_AuthzExtension{Resources: []string{ext}}}),
		"two extensions": custom(&nspb.AuthzPolicy_CustomProvider{AuthzExtension: &nspb.AuthzPolicy_CustomProvider_AuthzExtension{Resources: []string{ext, ext}}}),
		"other location": custom(&nspb.AuthzPolicy_CustomProvider{AuthzExtension: &nspb.AuthzPolicy_CustomProvider_AuthzExtension{
			Resources: []string{"projects/p/locations/us-east1/authzExtensions/ext"}}}),
		"content allow": {Target: target, Action: nspb.AuthzPolicy_ALLOW, PolicyProfile: nspb.AuthzPolicy_CONTENT_AUTHZ,
			HttpRules: []*nspb.AuthzPolicy_AuthzRule{{When: "true"}}},
	} {
		if err := create("bad", p); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := create("missing", custom(&nspb.AuthzPolicy_CustomProvider{AuthzExtension: &nspb.AuthzPolicy_CustomProvider_AuthzExtension{
		Resources: []string{loc + "/authzExtensions/none"}}})); status.Code(err) != codes.NotFound {
		t.Errorf("a missing extension: %v", err)
	}
	// An extension for /api from 10/8, then IAP for everything.
	pe := custom(&nspb.AuthzPolicy_CustomProvider{AuthzExtension: &nspb.AuthzPolicy_CustomProvider_AuthzExtension{
		Resources: []string{"https://networkservices.googleapis.com/v1/" + ext}}})
	pe.HttpRules = []*nspb.AuthzPolicy_AuthzRule{{To: &nspb.AuthzPolicy_AuthzRule_To{Operations: []*nspb.AuthzPolicy_AuthzRule_To_RequestOperation{{
		Paths: []*nspb.AuthzPolicy_AuthzRule_StringMatch{{MatchPattern: &nspb.AuthzPolicy_AuthzRule_StringMatch_Prefix{Prefix: "/api"}}}}}}}}
	if err := create("a-ext", pe); err != nil {
		t.Fatal(err)
	}
	if err := create("b-iap", custom(&nspb.AuthzPolicy_CustomProvider{CloudIap: &nspb.AuthzPolicy_CustomProvider_CloudIap{}})); err != nil {
		t.Fatal(err)
	}
	req := compute.AuthzRequest{Rule: rule, Path: "/api/x", Method: "GET", Header: http.Header{}}
	v := s.Authorize(req)
	if !v.Allowed || len(v.Custom) != 2 || v.Custom[0].Extensions[0] != ext || !v.Custom[1].IAP {
		t.Errorf("/api: %+v", v)
	}
	req.Path = "/web"
	if v := s.Authorize(req); len(v.Custom) != 1 || !v.Custom[0].IAP {
		t.Errorf("/web: %+v", v)
	}
	// Source resources: a service account and a tag value set.
	src := &nspb.AuthzPolicy{Target: target, Action: nspb.AuthzPolicy_ALLOW, HttpRules: []*nspb.AuthzPolicy_AuthzRule{{
		From: &nspb.AuthzPolicy_AuthzRule_From{Sources: []*nspb.AuthzPolicy_AuthzRule_From_RequestSource{{
			Resources: []*nspb.AuthzPolicy_AuthzRule_RequestResource{
				{IamServiceAccount: &nspb.AuthzPolicy_AuthzRule_StringMatch{MatchPattern: &nspb.AuthzPolicy_AuthzRule_StringMatch_Exact{Exact: "web@p.iam.gserviceaccount.com"}}},
				{TagValueIdSet: &nspb.AuthzPolicy_AuthzRule_RequestResource_TagValueIdSet{Ids: []int64{11, 12}}},
			}}}}}}}
	if err := create("src", src); err != nil {
		t.Fatal(err)
	}
	if v := s.Authorize(req); v.Allowed {
		t.Error("no source VM: allowed")
	}
	req.SourceVM, req.SourceServiceAccount = "projects/p/zones/us-central1-a/instances/vm", "web@p.iam.gserviceaccount.com"
	if v := s.Authorize(req); !v.Allowed {
		t.Error("the service account: denied")
	}
	req.SourceServiceAccount, req.SourceTagValues = "other@p.iam.gserviceaccount.com", []int64{11}
	if v := s.Authorize(req); v.Allowed {
		t.Error("one tag of two: allowed")
	}
	req.SourceTagValues = []int64{12, 11, 3}
	if v := s.Authorize(req); !v.Allowed {
		t.Error("both tags: denied")
	}
	// Not for external load balancers, and at most 5 IDs.
	ext2 := &nspb.AuthzPolicy_Target{LoadBalancingScheme: nspb.AuthzPolicy_EXTERNAL_MANAGED, Resources: []string{"projects/p/global/forwardingRules/x"}}
	bad := &nspb.AuthzPolicy{Target: ext2, Action: nspb.AuthzPolicy_ALLOW, HttpRules: src.HttpRules}
	if _, err := s.CreateAuthzPolicy(ctx, &nspb.CreateAuthzPolicyRequest{Parent: g, AuthzPolicyId: "x", AuthzPolicy: bad}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("resources on an external load balancer: %v", err)
	}
	six := &nspb.AuthzPolicy{Target: target, Action: nspb.AuthzPolicy_ALLOW, HttpRules: []*nspb.AuthzPolicy_AuthzRule{{
		From: &nspb.AuthzPolicy_AuthzRule_From{Sources: []*nspb.AuthzPolicy_AuthzRule_From_RequestSource{{
			Resources: []*nspb.AuthzPolicy_AuthzRule_RequestResource{{TagValueIdSet: &nspb.AuthzPolicy_AuthzRule_RequestResource_TagValueIdSet{Ids: []int64{1, 2, 3, 4, 5, 6}}}}}}}}}}
	if err := create("six", six); status.Code(err) != codes.InvalidArgument {
		t.Errorf("six tag IDs: %v", err)
	}
	// A CONTENT_AUTHZ extension policy is asked last.
	ce := custom(&nspb.AuthzPolicy_CustomProvider{AuthzExtension: &nspb.AuthzPolicy_CustomProvider_AuthzExtension{Resources: []string{ext}}})
	ce.PolicyProfile = nspb.AuthzPolicy_CONTENT_AUTHZ
	if err := create("content", ce); err != nil {
		t.Fatal(err)
	}
	if v := s.Authorize(req); len(v.Content) != 1 || len(v.Custom) != 1 {
		t.Errorf("content: %+v", v)
	}
}
