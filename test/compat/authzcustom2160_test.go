//go:build compat

package compat

import (
	"context"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	computeapi "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	networksecurity "cloud.google.com/go/networksecurity/apiv1"
	"cloud.google.com/go/networksecurity/apiv1/networksecuritypb"
	networkservices "cloud.google.com/go/networkservices/apiv1"
	"cloud.google.com/go/networkservices/apiv1/networkservicespb"
	corepb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extprocpb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	typepb "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
)

// authzCallout is the test's authorization extension: an ext_proc server
// that denies a request whose path starts with /blocked (403, its own
// body) and tags the others with x-authz-user.
type authzCallout struct {
	extprocpb.UnimplementedExternalProcessorServer
	calls chan string
}

func (a *authzCallout) Process(st extprocpb.ExternalProcessor_ProcessServer) error {
	for {
		in, err := st.Recv()
		if err != nil {
			return nil
		}
		rh := in.GetRequestHeaders()
		if rh == nil {
			continue
		}
		path := ""
		for _, h := range rh.GetHeaders().GetHeaders() {
			if h.GetKey() == ":path" {
				path = string(h.GetRawValue())
			}
		}
		select {
		case a.calls <- path:
		default:
		}
		if strings.HasPrefix(path, "/blocked") {
			return st.Send(&extprocpb.ProcessingResponse{Response: &extprocpb.ProcessingResponse_ImmediateResponse{
				ImmediateResponse: &extprocpb.ImmediateResponse{Status: &typepb.HttpStatus{Code: typepb.StatusCode_Forbidden}, Body: []byte("blocked by the callout")}}})
		}
		if err := st.Send(&extprocpb.ProcessingResponse{Response: &extprocpb.ProcessingResponse_RequestHeaders{RequestHeaders: &extprocpb.HeadersResponse{
			Response: &extprocpb.CommonResponse{HeaderMutation: &extprocpb.HeaderMutation{SetHeaders: []*corepb.HeaderValueOption{
				{Header: &corepb.HeaderValue{Key: "x-authz-user", RawValue: []byte("callout-ok")}}}}}}}}); err != nil {
			return err
		}
	}
}

// covers: google.cloud.networksecurity.v1.NetworkSecurity/CreateAuthzPolicy, google.cloud.networksecurity.v1.NetworkSecurity/DeleteAuthzPolicy, google.cloud.networkservices.v1.DepService/CreateAuthzExtension, google.cloud.networkservices.v1.DepService/DeleteAuthzExtension
//
// TestCustomAuthorizationThroughALoadBalancer (#2160), the official
// compute, networkservices and networksecurity REST clients, as
// load-balancing/docs/auth-policy/auth-policy-overview and
// service-extensions/docs/lb-extensions-overview set it up: a callout
// backend service (H2C, a hybrid NEG on a local ext_proc server), an
// authorization extension on it, and a CUSTOM authorization policy on an
// external Application Load Balancer's forwarding rule that delegates
// /api to it. Real requests through the rule: /api is allowed with the
// callout's header, /api/blocked gets the callout's 403, /other is not
// delegated; then a CUSTOM policy with Cloud IAP answers 401 without a
// token; source resources are refused on an external load balancer.
func TestCustomAuthorizationThroughALoadBalancer(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvCompute)
	l := newLB2160(t, h, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "user="+r.Header.Get("X-Authz-User"))
	}))
	ctx, opts := l.ctx, computeOpts(t, h)

	// The callout's server, its hybrid NEG, health check and backend
	// service.
	callout := &authzCallout{calls: make(chan string, 16)}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	g := grpc.NewServer()
	extprocpb.RegisterExternalProcessorServer(g, callout)
	go func() { _ = g.Serve(ln) }()
	t.Cleanup(g.Stop)
	host, port, _ := net.SplitHostPort(ln.Addr().String())
	p, _ := strconv.Atoi(port)
	zone := "us-central1-a"
	negs, _ := computeapi.NewNetworkEndpointGroupsRESTClient(ctx, opts...)
	op, err := negs.Insert(ctx, &computepb.InsertNetworkEndpointGroupRequest{Project: l.project, Zone: zone,
		NetworkEndpointGroupResource: &computepb.NetworkEndpointGroup{Name: proto.String(l.n("callout")),
			NetworkEndpointType: proto.String("NON_GCP_PRIVATE_IP_PORT"), Network: proto.String("global/networks/default"), DefaultPort: proto.Int32(int32(p))}})
	l.do("the callout's NEG", op, err)
	later(t, func(c context.Context) (*computeapi.Operation, error) {
		return negs.Delete(c, &computepb.DeleteNetworkEndpointGroupRequest{Project: l.project, Zone: zone, NetworkEndpointGroup: l.n("callout")})
	})
	op, err = negs.AttachNetworkEndpoints(ctx, &computepb.AttachNetworkEndpointsNetworkEndpointGroupRequest{Project: l.project, Zone: zone,
		NetworkEndpointGroup: l.n("callout"), NetworkEndpointGroupsAttachEndpointsRequestResource: &computepb.NetworkEndpointGroupsAttachEndpointsRequest{
			NetworkEndpoints: []*computepb.NetworkEndpoint{{IpAddress: proto.String(host), Port: proto.Int32(int32(p))}}}})
	l.do("the callout's endpoint", op, err)
	hcs, _ := computeapi.NewHealthChecksRESTClient(ctx, opts...)
	op, err = hcs.Insert(ctx, &computepb.InsertHealthCheckRequest{Project: l.project, HealthCheckResource: &computepb.HealthCheck{
		Name: proto.String(l.n("tcp")), Type: proto.String("TCP"), CheckIntervalSec: proto.Int32(2), TimeoutSec: proto.Int32(2),
		TcpHealthCheck: &computepb.TCPHealthCheck{PortSpecification: proto.String("USE_SERVING_PORT")}}})
	l.do("the callout's health check", op, err)
	later(t, func(c context.Context) (*computeapi.Operation, error) {
		return hcs.Delete(c, &computepb.DeleteHealthCheckRequest{Project: l.project, HealthCheck: l.n("tcp")})
	})
	op, err = l.bss.Insert(ctx, &computepb.InsertBackendServiceRequest{Project: l.project, BackendServiceResource: &computepb.BackendService{
		Name: proto.String(l.n("callout")), Protocol: proto.String("H2C"), LoadBalancingScheme: proto.String("EXTERNAL_MANAGED"),
		HealthChecks: []string{"global/healthChecks/" + l.n("tcp")},
		Backends: []*computepb.Backend{{Group: proto.String("zones/" + zone + "/networkEndpointGroups/" + l.n("callout")),
			BalancingMode: proto.String("RATE"), MaxRatePerEndpoint: proto.Float32(100)}}}})
	l.do("the callout's backend service", op, err)
	later(t, func(c context.Context) (*computeapi.Operation, error) {
		return l.bss.Delete(c, &computepb.DeleteBackendServiceRequest{Project: l.project, BackendService: l.n("callout")})
	})

	// The authorization extension.
	dep, err := networkservices.NewDepRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dep.Close() })
	loc := "projects/" + l.project + "/locations/global"
	eop, err := dep.CreateAuthzExtension(ctx, &networkservicespb.CreateAuthzExtensionRequest{Parent: loc, AuthzExtensionId: l.n("ext"),
		AuthzExtension: &networkservicespb.AuthzExtension{LoadBalancingScheme: networkservicespb.LoadBalancingScheme_EXTERNAL_MANAGED,
			Authority: "authz.example.com", Timeout: durationpb.New(time.Second),
			Service: "https://www.googleapis.com/compute/v1/projects/" + l.project + "/global/backendServices/" + l.n("callout")}})
	if err != nil {
		t.Fatal(err)
	}
	ext, err := eop.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if op, err := dep.DeleteAuthzExtension(context.Background(), &networkservicespb.DeleteAuthzExtensionRequest{Name: ext.GetName()}); err == nil {
			_ = op.Wait(context.Background())
		}
	})

	// The CUSTOM policy that delegates /api to it.
	ns, err := networksecurity.NewRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ns.Close() })
	ruleURL := "https://www.googleapis.com/compute/v1/projects/" + l.project + "/global/forwardingRules/" + l.n("rule")
	target := &networksecuritypb.AuthzPolicy_Target{LoadBalancingScheme: networksecuritypb.AuthzPolicy_EXTERNAL_MANAGED, Resources: []string{ruleURL}}
	create := func(id string, p *networksecuritypb.AuthzPolicy) (*networksecuritypb.AuthzPolicy, error) {
		op, err := ns.CreateAuthzPolicy(ctx, &networksecuritypb.CreateAuthzPolicyRequest{Parent: loc, AuthzPolicyId: id, AuthzPolicy: p})
		if err != nil {
			return nil, err
		}
		got, err := op.Wait(ctx)
		if err == nil {
			t.Cleanup(func() {
				if op, err := ns.DeleteAuthzPolicy(context.Background(), &networksecuritypb.DeleteAuthzPolicyRequest{Name: got.GetName()}); err == nil {
					_ = op.Wait(context.Background())
				}
			})
		}
		return got, err
	}
	pol, err := create(l.n("custom"), &networksecuritypb.AuthzPolicy{Target: target, Action: networksecuritypb.AuthzPolicy_CUSTOM,
		HttpRules: []*networksecuritypb.AuthzPolicy_AuthzRule{{To: &networksecuritypb.AuthzPolicy_AuthzRule_To{
			Operations: []*networksecuritypb.AuthzPolicy_AuthzRule_To_RequestOperation{{Paths: []*networksecuritypb.AuthzPolicy_AuthzRule_StringMatch{
				{MatchPattern: &networksecuritypb.AuthzPolicy_AuthzRule_StringMatch_Prefix{Prefix: "/api"}}}}}}}},
		CustomProvider: &networksecuritypb.AuthzPolicy_CustomProvider{AuthzExtension: &networksecuritypb.AuthzPolicy_CustomProvider_AuthzExtension{
			Resources: []string{ext.GetName()}}}})
	if err != nil {
		t.Fatalf("the CUSTOM policy: %v", err)
	}
	waitFor(t, "the callout to decide /api", 2*time.Minute, func() bool {
		code, body, _ := l.get("/api/x", nil)
		return code == 200 && body == "user=callout-ok"
	})
	if code, body, _ := l.get("/api/blocked", nil); code != 403 || body != "blocked by the callout" {
		t.Errorf("/api/blocked: %d %q", code, body)
	}
	for len(callout.calls) > 0 {
		<-callout.calls
	}
	if code, body, _ := l.get("/other", nil); code != 200 || body != "user=" || len(callout.calls) != 0 {
		t.Errorf("/other, not delegated: %d %q (%d calls)", code, body, len(callout.calls))
	}
	if op, err := ns.DeleteAuthzPolicy(ctx, &networksecuritypb.DeleteAuthzPolicyRequest{Name: pol.GetName()}); err != nil {
		t.Fatal(err)
	} else if err := op.Wait(ctx); err != nil {
		t.Fatal(err)
	}

	// Cloud IAP as the provider: no token, 401 (or the sign-in's 302).
	if _, err := create(l.n("iap"), &networksecuritypb.AuthzPolicy{Target: target, Action: networksecuritypb.AuthzPolicy_CUSTOM,
		CustomProvider: &networksecuritypb.AuthzPolicy_CustomProvider{CloudIap: &networksecuritypb.AuthzPolicy_CustomProvider_CloudIap{}}}); err != nil {
		t.Fatalf("the Cloud IAP policy: %v", err)
	}
	waitFor(t, "IAP to refuse a request without a token", time.Minute, func() bool {
		code, _, _ := l.get("/api/x", map[string]string{"Accept": "application/json"})
		return code == 401 || code == 302
	})
	if code, body, _ := l.get("/api/x", map[string]string{"Authorization": "Bearer not-a-token"}); code != 401 {
		t.Errorf("a bad token: %d %q", code, body)
	}

	// Source resources are for internal load balancers only.
	if _, err := create(l.n("src"), &networksecuritypb.AuthzPolicy{Target: target, Action: networksecuritypb.AuthzPolicy_ALLOW,
		HttpRules: []*networksecuritypb.AuthzPolicy_AuthzRule{{From: &networksecuritypb.AuthzPolicy_AuthzRule_From{
			Sources: []*networksecuritypb.AuthzPolicy_AuthzRule_From_RequestSource{{Resources: []*networksecuritypb.AuthzPolicy_AuthzRule_RequestResource{{
				IamServiceAccount: &networksecuritypb.AuthzPolicy_AuthzRule_StringMatch{MatchPattern: &networksecuritypb.AuthzPolicy_AuthzRule_StringMatch_Exact{
					Exact: "web@" + l.project + ".iam.gserviceaccount.com"}}}}}}}}}}); computeCode(err) != http.StatusBadRequest {
		t.Errorf("source resources on an external load balancer: %v", err)
	}
}
