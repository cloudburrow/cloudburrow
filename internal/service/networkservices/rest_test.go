package networkservices_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	networksecurity "cloud.google.com/go/networksecurity/apiv1"
	"cloud.google.com/go/networksecurity/apiv1/networksecuritypb"
	nsvc "cloud.google.com/go/networkservices/apiv1"
	"cloud.google.com/go/networkservices/apiv1/networkservicespb"
	"google.golang.org/api/option"

	nsec "github.com/cloudburrow/cloudburrow/internal/service/networksecurity"
	"github.com/cloudburrow/cloudburrow/internal/service/networkservices"
)

// TestSecureWebProxyOverREST drives both APIs with the official Go REST
// clients, as on the Compute Engine port, and sends a request through the
// gateway to a loopback origin.
func TestSecureWebProxyOverREST(t *testing.T) {
	ns, err := nsec.New(nsec.Options{})
	if err != nil {
		t.Fatal(err)
	}
	nsv, err := networkservices.New(networkservices.Options{})
	if err != nil {
		t.Fatal(err)
	}
	nsv.UsePolicies(ns)
	ns.UseGatewayUsers(nsv.GatewaySecurityPolicyUsers)
	nsv.Start()
	defer nsv.Stop()
	nsrest, nsvrest := nsec.NewRESTHandler(ns), networkservices.NewRESTHandler(nsv)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case ns.Handles(r):
			nsrest.ServeHTTP(w, r)
		case nsv.Handles(r):
			nsvrest.ServeHTTP(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	opts := []option.ClientOption{option.WithEndpoint(srv.URL), option.WithoutAuthentication()}
	sc, err := networksecurity.NewRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	gc, err := nsvc.NewRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	parent := "projects/p/locations/us-central1"
	pop, err := sc.CreateGatewaySecurityPolicy(ctx, &networksecuritypb.CreateGatewaySecurityPolicyRequest{Parent: parent, GatewaySecurityPolicyId: "pol",
		GatewaySecurityPolicy: &networksecuritypb.GatewaySecurityPolicy{}})
	if err != nil {
		t.Fatal(err)
	}
	pol, err := pop.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rop, err := sc.CreateGatewaySecurityPolicyRule(ctx, &networksecuritypb.CreateGatewaySecurityPolicyRuleRequest{Parent: pol.GetName(), GatewaySecurityPolicyRuleId: "allow",
		GatewaySecurityPolicyRule: &networksecuritypb.GatewaySecurityPolicyRule{Enabled: true, Priority: 1, SessionMatcher: "host() == '127.0.0.1'",
			ApplicationMatcher: "request.path == '/ok'",
			Profile:            &networksecuritypb.GatewaySecurityPolicyRule_BasicProfile_{BasicProfile: networksecuritypb.GatewaySecurityPolicyRule_ALLOW}}})
	if err != nil {
		t.Fatal(err)
	}
	if r, err := rop.Wait(ctx); err != nil || r.GetBasicProfile() != networksecuritypb.GatewaySecurityPolicyRule_ALLOW {
		t.Fatalf("rule: %v %v", r, err)
	}
	gop, err := gc.CreateGateway(ctx, &networkservicespb.CreateGatewayRequest{Parent: parent, GatewayId: "swp", Gateway: &networkservicespb.Gateway{
		Type: networkservicespb.Gateway_SECURE_WEB_GATEWAY, Ports: []int32{8080}, GatewaySecurityPolicy: pol.GetName(),
		Network: "projects/p/global/networks/n", Subnetwork: "projects/p/regions/us-central1/subnetworks/s"}})
	if err != nil {
		t.Fatal(err)
	}
	gw, err := gop.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sc.DeleteGatewaySecurityPolicy(ctx, &networksecuritypb.DeleteGatewaySecurityPolicyRequest{Name: pol.GetName()}); err == nil {
		t.Error("a policy a gateway names was deleted")
	}
	resp, err := http.Get(srv.URL + "/v1/" + gw.GetName())
	if err != nil {
		t.Fatal(err)
	}
	var raw struct {
		Address   string   `json:"cloudburrowAddress"`
		Addresses []string `json:"cloudburrowAddresses"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&raw)
	_ = resp.Body.Close()
	if raw.Address == "" || len(raw.Addresses) != 1 || !strings.HasPrefix(raw.Addresses[0], "8080=") {
		t.Fatalf("the gateway's listener: %+v", raw)
	}
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "ok") }))
	defer origin.Close()
	pu, _ := url.Parse("http://" + raw.Address)
	c := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(pu)}, Timeout: 10 * time.Second}
	for path, want := range map[string]int{"/ok": 200, "/no": 403} {
		r, err := c.Get(origin.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		_ = r.Body.Close()
		if r.StatusCode != want {
			t.Errorf("%s: %d, want %d", path, r.StatusCode, want)
		}
	}
	// Collections not served answer UNIMPLEMENTED rather than 404.
	if _, err := gc.GetMesh(ctx, &networkservicespb.GetMeshRequest{Name: "projects/p/locations/global/meshes/m"}); err == nil || !strings.Contains(err.Error(), "501") {
		t.Errorf("GetMesh: %v, want 501", err)
	}
}
