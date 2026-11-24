package networksecurity

import (
	"context"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"

	nsclient "cloud.google.com/go/networksecurity/apiv1"
	nspb "cloud.google.com/go/networksecurity/apiv1/networksecuritypb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/service/compute"
)

const g = "projects/p/locations/global"

func code(err error) codes.Code { return status.Code(err) }

func TestServerTLSPolicyChecks(t *testing.T) {
	s, _ := New(Options{})
	s.UseHooks(Hooks{TrustConfigExists: func(n string) bool { return n == g+"/trustConfigs/tc" }})
	ctx := context.Background()
	mk := func(id string, p *nspb.ServerTlsPolicy) error {
		_, err := s.CreateServerTlsPolicy(ctx, &nspb.CreateServerTlsPolicyRequest{Parent: g, ServerTlsPolicyId: id, ServerTlsPolicy: p})
		return err
	}
	reject := nspb.ServerTlsPolicy_MTLSPolicy_REJECT_INVALID
	if err := mk("ok", &nspb.ServerTlsPolicy{MtlsPolicy: &nspb.ServerTlsPolicy_MTLSPolicy{ClientValidationMode: reject, ClientValidationTrustConfig: g + "/trustConfigs/tc"}}); err != nil {
		t.Fatal(err)
	}
	for name, p := range map[string]*nspb.ServerTlsPolicy{
		"missing trust config": {MtlsPolicy: &nspb.ServerTlsPolicy_MTLSPolicy{ClientValidationMode: reject, ClientValidationTrustConfig: g + "/trustConfigs/nope"}},
		"reject without trust": {MtlsPolicy: &nspb.ServerTlsPolicy_MTLSPolicy{ClientValidationMode: reject}},
		"regional trust":       {MtlsPolicy: &nspb.ServerTlsPolicy_MTLSPolicy{ClientValidationMode: reject, ClientValidationTrustConfig: "projects/p/locations/us-central1/trustConfigs/tc"}},
		"allow open with alb":  {AllowOpen: true, MtlsPolicy: &nspb.ServerTlsPolicy_MTLSPolicy{ClientValidationMode: reject, ClientValidationTrustConfig: g + "/trustConfigs/tc"}},
	} {
		if err := mk("x", p); code(err) != codes.InvalidArgument && code(err) != codes.NotFound {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := mk("ok", &nspb.ServerTlsPolicy{AllowOpen: true}); code(err) != codes.AlreadyExists {
		t.Errorf("duplicate: %v", err)
	}
	if err := mk("1bad", &nspb.ServerTlsPolicy{AllowOpen: true}); code(err) != codes.InvalidArgument {
		t.Errorf("bad id: %v", err)
	}
	if u := s.TrustConfigUsers(g + "/trustConfigs/tc"); len(u) != 1 {
		t.Errorf("TrustConfigUsers = %v", u)
	}
	s.UseHooks(Hooks{Users: func(string) []string { return []string{"projects/p/global/targetHttpsProxies/x"} }})
	if _, err := s.DeleteServerTlsPolicy(ctx, &nspb.DeleteServerTlsPolicyRequest{Name: g + "/serverTlsPolicies/ok"}); code(err) != codes.FailedPrecondition {
		t.Errorf("delete in use: %v", err)
	}
}

func TestAuthzPolicyEnforcement(t *testing.T) {
	s, _ := New(Options{})
	rule := "projects/p/global/forwardingRules/fr"
	s.UseHooks(Hooks{ForwardingRule: func(rel string) (string, bool) { return "EXTERNAL_MANAGED", rel == rule }})
	ctx := context.Background()
	target := &nspb.AuthzPolicy_Target{LoadBalancingScheme: nspb.AuthzPolicy_EXTERNAL_MANAGED,
		Resources: []string{"https://www.googleapis.com/compute/v1/" + rule}}
	deny := &nspb.AuthzPolicy{Target: target, Action: nspb.AuthzPolicy_DENY, HttpRules: []*nspb.AuthzPolicy_AuthzRule{{
		To: &nspb.AuthzPolicy_AuthzRule_To{Operations: []*nspb.AuthzPolicy_AuthzRule_To_RequestOperation{{
			Paths: []*nspb.AuthzPolicy_AuthzRule_StringMatch{{MatchPattern: &nspb.AuthzPolicy_AuthzRule_StringMatch_Prefix{Prefix: "/admin"}}}}}}}}}
	if _, err := s.CreateAuthzPolicy(ctx, &nspb.CreateAuthzPolicyRequest{Parent: g, AuthzPolicyId: "deny", AuthzPolicy: deny}); err != nil {
		t.Fatal(err)
	}
	req := compute.AuthzRequest{Rule: rule, Host: "a.example.com", Path: "/admin/x", Method: "GET", SourceIP: "127.0.0.1", Header: http.Header{}}
	if s.Authorize(req).Allowed {
		t.Error("DENY /admin let through")
	}
	req.Path = "/public"
	if !s.Authorize(req).Allowed {
		t.Error("no ALLOW policy: denied")
	}
	allow := &nspb.AuthzPolicy{Target: target, Action: nspb.AuthzPolicy_ALLOW, HttpRules: []*nspb.AuthzPolicy_AuthzRule{{
		From: &nspb.AuthzPolicy_AuthzRule_From{Sources: []*nspb.AuthzPolicy_AuthzRule_From_RequestSource{{
			IpBlocks: []*nspb.AuthzPolicy_AuthzRule_IpBlock{{Prefix: "10.0.0.0", Length: 8}}}}},
		When: `request.headers["x-team"] == "blue"`}}}
	if _, err := s.CreateAuthzPolicy(ctx, &nspb.CreateAuthzPolicyRequest{Parent: g, AuthzPolicyId: "allow", AuthzPolicy: allow}); err != nil {
		t.Fatal(err)
	}
	if s.Authorize(req).Allowed {
		t.Error("ALLOW from 10/8 let 127.0.0.1 through")
	}
	req.SourceIP = "10.1.2.3"
	if s.Authorize(req).Allowed {
		t.Error("when not met: let through")
	}
	req.Header.Set("X-Team", "blue")
	if !s.Authorize(req).Allowed {
		t.Error("ALLOW matched: denied")
	}
	// The documented refusals.
	bad := &nspb.AuthzPolicy{Target: target, Action: nspb.AuthzPolicy_ALLOW, HttpRules: []*nspb.AuthzPolicy_AuthzRule{{
		From: &nspb.AuthzPolicy_AuthzRule_From{Sources: []*nspb.AuthzPolicy_AuthzRule_From_RequestSource{{
			Principals: []*nspb.AuthzPolicy_AuthzRule_Principal{{Principal: &nspb.AuthzPolicy_AuthzRule_StringMatch{MatchPattern: &nspb.AuthzPolicy_AuthzRule_StringMatch_Exact{Exact: "spiffe://x"}}}}}}}}}}
	if _, err := s.CreateAuthzPolicy(ctx, &nspb.CreateAuthzPolicyRequest{Parent: g, AuthzPolicyId: "principals", AuthzPolicy: bad}); code(err) != codes.InvalidArgument {
		t.Errorf("principals on a global external ALB: %v", err)
	}
	if _, err := s.CreateAuthzPolicy(ctx, &nspb.CreateAuthzPolicyRequest{Parent: g, AuthzPolicyId: "norules",
		AuthzPolicy: &nspb.AuthzPolicy{Target: target, Action: nspb.AuthzPolicy_ALLOW}}); code(err) != codes.InvalidArgument {
		t.Errorf("ALLOW without rules: %v", err)
	}
	other := &nspb.AuthzPolicy_Target{LoadBalancingScheme: nspb.AuthzPolicy_EXTERNAL_MANAGED, Resources: []string{"projects/p/global/forwardingRules/none"}}
	if _, err := s.CreateAuthzPolicy(ctx, &nspb.CreateAuthzPolicyRequest{Parent: g, AuthzPolicyId: "none",
		AuthzPolicy: &nspb.AuthzPolicy{Target: other, Action: nspb.AuthzPolicy_DENY, HttpRules: deny.HttpRules}}); code(err) != codes.NotFound {
		t.Errorf("missing forwarding rule: %v", err)
	}
	if _, err := s.CreateAuthzPolicy(ctx, &nspb.CreateAuthzPolicyRequest{Parent: g, AuthzPolicyId: "badwhen",
		AuthzPolicy: &nspb.AuthzPolicy{Target: target, Action: nspb.AuthzPolicy_DENY, HttpRules: []*nspb.AuthzPolicy_AuthzRule{{When: "request.("}}}}); code(err) != codes.InvalidArgument {
		t.Errorf("bad when: %v", err)
	}
}

func TestPrincipalNeedsVerifiedCert(t *testing.T) {
	u, _ := url.Parse("spiffe://example.org/ns/a")
	leaf := &x509.Certificate{URIs: []*url.URL{u}}
	p := &nspb.AuthzPolicy_AuthzRule_Principal{Principal: &nspb.AuthzPolicy_AuthzRule_StringMatch{MatchPattern: &nspb.AuthzPolicy_AuthzRule_StringMatch_Exact{Exact: u.String()}}}
	if principalMatches(p, compute.AuthzRequest{Cert: compute.ClientCert{Present: true, Leaf: leaf}}) {
		t.Error("an unverified certificate matched")
	}
	if !principalMatches(p, compute.AuthzRequest{Cert: compute.ClientCert{Present: true, Verified: true, Leaf: leaf}}) {
		t.Error("a verified certificate did not match")
	}
}

// TestOfficialRESTClient: the official networksecurity/apiv1 REST client
// against the JSON API, its operations' waits included, and the state
// restored.
func TestOfficialRESTClient(t *testing.T) {
	state := filepath.Join(t.TempDir(), "ns.json")
	s, _ := New(Options{State: state})
	srv := httptest.NewServer(NewRESTHandler(s))
	defer srv.Close()
	ctx := context.Background()
	c, err := nsclient.NewRESTClient(ctx, option.WithEndpoint(srv.URL), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	op, err := c.CreateServerTlsPolicy(ctx, &nspb.CreateServerTlsPolicyRequest{Parent: g, ServerTlsPolicyId: "stp",
		ServerTlsPolicy: &nspb.ServerTlsPolicy{Description: "d", MtlsPolicy: &nspb.ServerTlsPolicy_MTLSPolicy{
			ClientValidationMode: nspb.ServerTlsPolicy_MTLSPolicy_ALLOW_INVALID_OR_MISSING_CLIENT_CERT}}})
	if err != nil {
		t.Fatal(err)
	}
	p, err := op.Wait(ctx)
	if err != nil || p.GetName() != g+"/serverTlsPolicies/stp" || p.GetCreateTime() == nil {
		t.Fatalf("wait: %v %v", p, err)
	}
	uop, err := c.UpdateServerTlsPolicy(ctx, &nspb.UpdateServerTlsPolicyRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"description"}},
		ServerTlsPolicy: &nspb.ServerTlsPolicy{Name: p.GetName(), Description: "e"}})
	if err != nil {
		t.Fatal(err)
	}
	if p, err = uop.Wait(ctx); err != nil || p.GetDescription() != "e" || p.GetMtlsPolicy() == nil {
		t.Fatalf("update: %v %v", p, err)
	}
	cop, err := c.CreateClientTlsPolicy(ctx, &nspb.CreateClientTlsPolicyRequest{Parent: g, ClientTlsPolicyId: "ctp",
		ClientTlsPolicy: &nspb.ClientTlsPolicy{Sni: "backend.example.com"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cop.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	aop, err := c.CreateAuthorizationPolicy(ctx, &nspb.CreateAuthorizationPolicyRequest{Parent: g, AuthorizationPolicyId: "ap",
		AuthorizationPolicy: &nspb.AuthorizationPolicy{Action: nspb.AuthorizationPolicy_ALLOW, Rules: []*nspb.AuthorizationPolicy_Rule{{
			Destinations: []*nspb.AuthorizationPolicy_Rule_Destination{{Hosts: []string{"*"}, Ports: []uint32{443}}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := aop.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateAuthorizationPolicy(ctx, &nspb.CreateAuthorizationPolicyRequest{Parent: g, AuthorizationPolicyId: "bad",
		AuthorizationPolicy: &nspb.AuthorizationPolicy{Action: nspb.AuthorizationPolicy_ALLOW, Rules: []*nspb.AuthorizationPolicy_Rule{{
			Destinations: []*nspb.AuthorizationPolicy_Rule_Destination{{Hosts: []string{"*"}}}}}}}); err == nil {
		t.Error("a destination without ports was accepted")
	}
	it := c.ListServerTlsPolicies(ctx, &nspb.ListServerTlsPoliciesRequest{Parent: "projects/p/locations/-"})
	n := 0
	for {
		_, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		n++
	}
	if n != 1 {
		t.Errorf("listed %d server TLS policies", n)
	}
	r, _ := New(Options{State: state})
	if _, ok := r.ServerTLSPolicy(g + "/serverTlsPolicies/stp"); !ok || !r.Exists(g+"/authorizationPolicies/ap") {
		t.Error("state not restored")
	}
	dop, err := c.DeleteClientTlsPolicy(ctx, &nspb.DeleteClientTlsPolicyRequest{Name: g + "/clientTlsPolicies/ctp"})
	if err != nil || dop.Wait(ctx) != nil {
		t.Fatalf("delete: %v", err)
	}
	if !s.Handles(httptest.NewRequest("GET", "/v1/"+g+"/serverTlsPolicies", nil)) || s.Handles(httptest.NewRequest("GET", "/v1/"+g+"/certificates", nil)) {
		t.Error("Handles")
	}
}
