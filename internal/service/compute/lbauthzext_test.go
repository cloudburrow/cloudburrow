package compute

import (
	"context"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	corepb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	authpb "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
	extprocpb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	typepb "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
)

// funcNetSec decides each request with a function.
type funcNetSec struct {
	f func(AuthzRequest) AuthzVerdict
}

func (funcNetSec) PolicyExists(string) bool                      { return true }
func (funcNetSec) ServerTLS(string) (ServerTLSPolicy, bool)      { return ServerTLSPolicy{}, false }
func (n funcNetSec) Authorize(r AuthzRequest) AuthzVerdict       { return n.f(r) }
func (m mapExts) AuthzExtension(n string) (AuthzExtension, bool) { e, ok := m[n]; return e, ok }

type mapExts map[string]AuthzExtension

// testAuthz is an ext_authz server: /deny is denied with 401 and a
// header, anything else allowed with x-user set and x-drop removed.
type testAuthz struct {
	authpb.UnimplementedAuthorizationServer
	seen chan *authpb.CheckRequest
}

func (a *testAuthz) Check(_ context.Context, in *authpb.CheckRequest) (*authpb.CheckResponse, error) {
	select {
	case a.seen <- in:
	default:
	}
	h := in.GetAttributes().GetRequest().GetHttp()
	switch {
	case strings.HasPrefix(h.GetPath(), "/deny"):
		return &authpb.CheckResponse{Status: &status.Status{Code: int32(codes.PermissionDenied)},
			HttpResponse: &authpb.CheckResponse_DeniedResponse{DeniedResponse: &authpb.DeniedHttpResponse{
				Status:  &typepb.HttpStatus{Code: typepb.StatusCode_Unauthorized},
				Headers: []*corepb.HeaderValueOption{{Header: &corepb.HeaderValue{Key: "x-why", Value: "no"}}}, Body: "denied by authz"}}}, nil
	case strings.HasPrefix(h.GetPath(), "/badheader"):
		return &authpb.CheckResponse{Status: &status.Status{}, HttpResponse: &authpb.CheckResponse_OkResponse{OkResponse: &authpb.OkHttpResponse{
			Headers: []*corepb.HeaderValueOption{{Header: &corepb.HeaderValue{Key: "host", Value: "evil"}}}}}}, nil
	case strings.HasPrefix(h.GetPath(), "/slow"):
		time.Sleep(300 * time.Millisecond)
	}
	return &authpb.CheckResponse{Status: &status.Status{}, HttpResponse: &authpb.CheckResponse_OkResponse{OkResponse: &authpb.OkHttpResponse{
		Headers:              []*corepb.HeaderValueOption{{Header: &corepb.HeaderValue{Key: "x-user", RawValue: []byte("alice")}}},
		HeadersToRemove:      []string{"x-drop"},
		ResponseHeadersToAdd: []*corepb.HeaderValueOption{{Header: &corepb.HeaderValue{Key: "x-authz", Value: "ok"}}}}}}, nil
}

// testProc is an ext_proc server: a request whose :path is /deny gets an
// immediate 403; one whose body says "bad" too; others get x-proc.
type testProc struct {
	extprocpb.UnimplementedExternalProcessorServer
}

func (testProc) Process(st extprocpb.ExternalProcessor_ProcessServer) error {
	for {
		in, err := st.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		deny := func() error {
			return st.Send(&extprocpb.ProcessingResponse{Response: &extprocpb.ProcessingResponse_ImmediateResponse{
				ImmediateResponse: &extprocpb.ImmediateResponse{Status: &typepb.HttpStatus{Code: typepb.StatusCode_Forbidden}, Body: []byte("proc says no")}}})
		}
		if rh := in.GetRequestHeaders(); rh != nil {
			path := ""
			for _, h := range rh.GetHeaders().GetHeaders() {
				if h.GetKey() == ":path" {
					path = string(h.GetRawValue())
				}
			}
			if strings.HasPrefix(path, "/deny") {
				return deny()
			}
			if err := st.Send(&extprocpb.ProcessingResponse{Response: &extprocpb.ProcessingResponse_RequestHeaders{RequestHeaders: &extprocpb.HeadersResponse{
				Response: &extprocpb.CommonResponse{HeaderMutation: &extprocpb.HeaderMutation{SetHeaders: []*corepb.HeaderValueOption{
					{Header: &corepb.HeaderValue{Key: "x-proc", RawValue: []byte("yes")}}}}}}}}); err != nil {
				return err
			}
		}
		if rb := in.GetRequestBody(); rb != nil {
			if strings.Contains(string(rb.GetBody()), "bad") {
				return deny()
			}
			if err := st.Send(&extprocpb.ProcessingResponse{Response: &extprocpb.ProcessingResponse_RequestBody{RequestBody: &extprocpb.BodyResponse{}}}); err != nil {
				return err
			}
		}
	}
}

func serveGRPC(t *testing.T, reg func(*grpc.Server)) (string, int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	g := grpc.NewServer()
	reg(g)
	go func() { _ = g.Serve(ln) }()
	t.Cleanup(g.Stop)
	host, port, _ := net.SplitHostPort(ln.Addr().String())
	p, _ := strconv.Atoi(port)
	return host, p
}

// TestAuthzExtensionsThroughTheProxy: a CUSTOM policy's ext_authz and
// ext_proc extensions decide real requests through the load balancer.
func TestAuthzExtensionsThroughTheProxy(t *testing.T) {
	authz := &testAuthz{seen: make(chan *authpb.CheckRequest, 16)}
	ah, ap := serveGRPC(t, func(g *grpc.Server) { authpb.RegisterAuthorizationServer(g, authz) })
	ph, pp := serveGRPC(t, func(g *grpc.Server) { extprocpb.RegisterExternalProcessorServer(g, testProc{}) })
	exts := mapExts{
		"projects/p/locations/global/authzExtensions/a": {Name: "projects/p/locations/global/authzExtensions/a", Service: "projects/p/global/backendServices/authz",
			WireFormat: "EXT_AUTHZ_GRPC", Timeout: 200 * time.Millisecond, ForwardHeaders: []string{"x-team"}, Metadata: map[string]any{"rule": "{forwarding_rule_id}"}},
		"projects/p/locations/global/authzExtensions/open": {Name: "open", Service: "projects/p/global/backendServices/authz",
			WireFormat: "EXT_AUTHZ_GRPC", Timeout: 50 * time.Millisecond, FailOpen: true},
		"projects/p/locations/global/authzExtensions/p": {Name: "p", Service: "projects/p/global/backendServices/proc", Timeout: time.Second},
	}
	ns := funcNetSec{f: func(r AuthzRequest) AuthzVerdict {
		v := AuthzVerdict{Allowed: true}
		switch {
		case strings.HasPrefix(r.Path, "/proc"):
			v.Custom = []AuthzCustom{{Extensions: []string{"projects/p/locations/global/authzExtensions/p"}}}
		case strings.HasPrefix(r.Path, "/content"):
			v.Content = []AuthzCustom{{Extensions: []string{"projects/p/locations/global/authzExtensions/p"}}}
		case strings.HasPrefix(r.Path, "/open"):
			v.Custom = []AuthzCustom{{Extensions: []string{"projects/p/locations/global/authzExtensions/open"}}}
		case strings.HasPrefix(r.Path, "/public"), r.Path == "/healthz":
		default:
			v.Custom = []AuthzCustom{{Extensions: []string{"projects/p/locations/global/authzExtensions/a"}}}
		}
		return v
	}}
	origin := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "user="+r.Header.Get("X-User")+" drop="+r.Header.Get("X-Drop")+" proc="+r.Header.Get("X-Proc"))
	})
	s, _, base := originFixture(t, Options{NetSec: ns, AuthzExt: exts}, origin)
	z := "zones/us-central1-a/"
	lbCall(t, s, "POST", "global/healthChecks", obj{"name": "tcp", "type": "TCP",
		"tcpHealthCheck": obj{"portSpecification": "USE_SERVING_PORT"}, "checkIntervalSec": 1, "timeoutSec": 1, "healthyThreshold": 1})
	for name, ep := range map[string][2]any{"authz": {ah, ap}, "proc": {ph, pp}} {
		lbCall(t, s, "POST", z+"networkEndpointGroups", obj{"name": name, "networkEndpointType": "NON_GCP_PRIVATE_IP_PORT",
			"network": "global/networks/default", "defaultPort": 80})
		lbCall(t, s, "POST", z+"networkEndpointGroups/"+name+"/attachNetworkEndpoints", obj{"networkEndpoints": []any{
			obj{"ipAddress": ep[0], "port": ep[1]}}})
		lbCall(t, s, "POST", "global/backendServices", obj{"name": name, "protocol": "H2C", "loadBalancingScheme": "EXTERNAL_MANAGED",
			"healthChecks": []any{"global/healthChecks/tcp"},
			"backends":     []any{obj{"group": z + "networkEndpointGroups/" + name, "balancingMode": "RATE", "maxRatePerEndpoint": 100}}})
	}
	do := func(method, path, body string, hdr map[string]string) (int, string, http.Header) {
		t.Helper()
		req, _ := http.NewRequest(method, base+path, strings.NewReader(body))
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b), resp.Header
	}
	var code int
	var body string
	var h http.Header
	waitFor(t, "the extensions' backends to be healthy", func() bool {
		code, _, _ = do("GET", "/proc", "", nil)
		return code == 200
	})
	code, body, h = do("GET", "/app?q=1", "", map[string]string{"X-Drop": "1", "X-Team": "blue", "X-Secret": "s"})
	if code != 200 || body != "user=alice drop= proc=" || h.Get("X-Authz") != "ok" {
		t.Fatalf("ext_authz allow: %d %q %v", code, body, h)
	}
	in := <-authz.seen
	hh := in.GetAttributes().GetRequest().GetHttp()
	if hh.GetPath() != "/app?q=1" || hh.GetHeaders()["x-team"] != "blue" || hh.GetHeaders()["x-secret"] != "" ||
		in.GetAttributes().GetMetadataContext().GetFilterMetadata()["com.google.authz_extension.projects/p/locations/global/authzExtensions/a"].GetFields()["rule"].GetStringValue() != "projects/p/global/forwardingRules/lb" {
		t.Errorf("CheckRequest: %v", in)
	}
	if code, body, h := do("GET", "/deny", "", nil); code != 401 || body != "denied by authz" || h.Get("X-Why") != "no" {
		t.Errorf("ext_authz deny: %d %q", code, body)
	}
	if code, _, _ := do("GET", "/badheader", "", nil); code != 500 {
		t.Errorf("a disallowed header change: %d", code)
	}
	if code, _, _ := do("GET", "/slow", "", nil); code != 500 {
		t.Errorf("a timeout, fail closed: %d", code)
	}
	if code, _, _ := do("GET", "/open/slow", "", nil); code != 200 {
		t.Errorf("a timeout, failOpen: %d", code)
	}
	if code, body, _ := do("GET", "/proc", "", nil); code != 200 || body != "user= drop= proc=yes" {
		t.Errorf("ext_proc allow: %d %q", code, body)
	}
	if code, body, _ := do("POST", "/content", "fine", nil); code != 200 || body != "user= drop= proc=yes" {
		t.Errorf("content authz allow: %d %q", code, body)
	}
	if code, body, _ := do("POST", "/content", "bad body", nil); code != 403 || body != "proc says no" {
		t.Errorf("content authz deny: %d %q", code, body)
	}
	if code, _, _ := do("GET", "/public", "", nil); code != 200 {
		t.Errorf("no CUSTOM policy: %d", code)
	}
}

// TestSourceIdentity: a request from an instance's internal address
// carries its service account and tags.
func TestSourceIdentity(t *testing.T) {
	s, err := New(Options{VMs: fakeVMs{}, SourceTags: func(full string) []int64 {
		if strings.HasPrefix(full, "//compute.googleapis.com/projects/p/zones/us-central1-a/instances/") {
			return []int64{42}
		}
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.res["projects/p/zones/us-central1-a/instances/vm"] = obj{"kind": "compute#instance", "id": "7",
		"networkInterfaces": []any{obj{"networkIP": "10.128.0.9"}}, "serviceAccounts": []any{obj{"email": "sa@p.iam.gserviceaccount.com"}}}
	s.mu.Unlock()
	r := AuthzRequest{SourceIP: "10.128.0.9"}
	s.sourceIdentity(&r)
	if r.SourceVM != "projects/p/zones/us-central1-a/instances/vm" || r.SourceServiceAccount != "sa@p.iam.gserviceaccount.com" || len(r.SourceTagValues) != 1 || r.SourceTagValues[0] != 42 {
		t.Errorf("identity: %+v", r)
	}
	r = AuthzRequest{SourceIP: "127.0.0.1"}
	s.sourceIdentity(&r)
	if r.SourceVM != "" {
		t.Errorf("loopback: %+v", r)
	}
}

// ruleIAP allows dev@example.com on the forwarding rule's IAP resource,
// with client-1 as a programmatic client.
type ruleIAP struct{ fakeIAP }

func (ruleIAP) Allowed(_ context.Context, res, email string, _ IAPRequest) bool {
	return res == "projects/p/iap_web/forwarding_rule/services/lb" && email == "dev@example.com"
}
func (ruleIAP) Settings(res string) map[string]any {
	return map[string]any{"accessSettings": map[string]any{"oauthSettings": map[string]any{"programmaticClients": []any{"client-1"}}}}
}

// TestCustomIAPThroughTheProxy: a CUSTOM policy with Cloud IAP applies
// IAP on the forwarding rule, without IAP on the backend service.
func TestCustomIAPThroughTheProxy(t *testing.T) {
	ns := funcNetSec{f: func(r AuthzRequest) AuthzVerdict {
		if r.Path == "/healthz" {
			return AuthzVerdict{Allowed: true}
		}
		return AuthzVerdict{Allowed: true, Custom: []AuthzCustom{{IAP: true}}}
	}}
	var email string
	origin := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		email = r.Header.Get("X-Goog-Authenticated-User-Email")
		_, _ = io.WriteString(w, "ok")
	})
	_, _, base := originFixture(t, Options{NetSec: ns, IAP: ruleIAP{}}, origin)
	try := func(tok string) int {
		req, _ := http.NewRequest("GET", base+"/", nil)
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if c := try(""); c != 401 {
		t.Errorf("no token: %d", c)
	}
	if c := try("tok:eve@example.com:client-1"); c != 403 {
		t.Errorf("no access: %d", c)
	}
	if c := try("tok:dev@example.com:client-1"); c != 200 || email != "accounts.google.com:dev@example.com" {
		t.Errorf("allowed: %d %q", c, email)
	}
}
