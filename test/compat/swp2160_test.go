//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	computeapi "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	networksecurity "cloud.google.com/go/networksecurity/apiv1"
	"cloud.google.com/go/networksecurity/apiv1/networksecuritypb"
	networkservices "cloud.google.com/go/networkservices/apiv1"
	"cloud.google.com/go/networkservices/apiv1/networkservicespb"
	"google.golang.org/api/iterator"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// TestSecureWebProxyThroughTheGoClients builds a Secure Web Proxy with
// the official Go clients (a network, a subnetwork and a proxy-only
// subnet; a URL list, a gateway security policy with rules, a TLS
// inspection policy; the gateway; an authorization extension) and sends
// real requests through the proxy, as the quickstart's curl -x does. Every
// target is a server on this machine's loopback: the proxy never reaches
// the internet in this test.
//
// covers: google.cloud.networksecurity.v1.NetworkSecurity/CreateUrlList, google.cloud.networksecurity.v1.NetworkSecurity/GetUrlList, google.cloud.networksecurity.v1.NetworkSecurity/ListUrlLists, google.cloud.networksecurity.v1.NetworkSecurity/UpdateUrlList, google.cloud.networksecurity.v1.NetworkSecurity/DeleteUrlList, google.cloud.networksecurity.v1.NetworkSecurity/CreateGatewaySecurityPolicy, google.cloud.networksecurity.v1.NetworkSecurity/GetGatewaySecurityPolicy, google.cloud.networksecurity.v1.NetworkSecurity/ListGatewaySecurityPolicies, google.cloud.networksecurity.v1.NetworkSecurity/UpdateGatewaySecurityPolicy, google.cloud.networksecurity.v1.NetworkSecurity/DeleteGatewaySecurityPolicy, google.cloud.networksecurity.v1.NetworkSecurity/CreateGatewaySecurityPolicyRule, google.cloud.networksecurity.v1.NetworkSecurity/GetGatewaySecurityPolicyRule, google.cloud.networksecurity.v1.NetworkSecurity/ListGatewaySecurityPolicyRules, google.cloud.networksecurity.v1.NetworkSecurity/UpdateGatewaySecurityPolicyRule, google.cloud.networksecurity.v1.NetworkSecurity/DeleteGatewaySecurityPolicyRule, google.cloud.networksecurity.v1.NetworkSecurity/CreateTlsInspectionPolicy, google.cloud.networksecurity.v1.NetworkSecurity/GetTlsInspectionPolicy, google.cloud.networksecurity.v1.NetworkSecurity/ListTlsInspectionPolicies, google.cloud.networksecurity.v1.NetworkSecurity/UpdateTlsInspectionPolicy, google.cloud.networksecurity.v1.NetworkSecurity/DeleteTlsInspectionPolicy, google.cloud.networkservices.v1.NetworkServices/CreateGateway, google.cloud.networkservices.v1.NetworkServices/GetGateway, google.cloud.networkservices.v1.NetworkServices/ListGateways, google.cloud.networkservices.v1.NetworkServices/UpdateGateway, google.cloud.networkservices.v1.NetworkServices/DeleteGateway, google.cloud.networkservices.v1.DepService/CreateAuthzExtension, google.cloud.networkservices.v1.DepService/GetAuthzExtension, google.cloud.networkservices.v1.DepService/ListAuthzExtensions, google.cloud.networkservices.v1.DepService/UpdateAuthzExtension, google.cloud.networkservices.v1.DepService/DeleteAuthzExtension
func TestSecureWebProxyThroughTheGoClients(t *testing.T) {
	h := New(t)
	computeAddr := h.Endpoint(EnvCompute) // skips without it
	opts := computeOpts(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	project := h.Project()
	sfx := time.Now().UnixNano() % 1e6
	n := func(s string) string { return fmt.Sprintf("swp-%s-%d", s, sfx) }
	const region = "us-west2"
	parent := "projects/" + project + "/locations/" + region

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "origin "+r.URL.Path)
	}))
	defer origin.Close()
	_, port, _ := net.SplitHostPort(origin.Listener.Addr().String())

	// The network, its subnetwork and its proxy-only subnet.
	nets, err := computeapi.NewNetworksRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = nets.Close() }()
	subs, _ := computeapi.NewSubnetworksRESTClient(ctx, opts...)
	defer func() { _ = subs.Close() }()
	wait := func(what string, op *computeapi.Operation, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if err := op.Wait(ctx); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	op, err := nets.Insert(ctx, &computepb.InsertNetworkRequest{Project: project, NetworkResource: &computepb.Network{
		Name: proto.String(n("net")), AutoCreateSubnetworks: proto.Bool(false)}})
	wait("the network", op, err)
	later(t, func(c context.Context) (*computeapi.Operation, error) {
		return nets.Delete(c, &computepb.DeleteNetworkRequest{Project: project, Network: n("net")})
	})
	netRel := "projects/" + project + "/global/networks/" + n("net")
	for _, sn := range []*computepb.Subnetwork{
		{Name: proto.String(n("sub")), IpCidrRange: proto.String("10.77.0.0/24"), Network: proto.String(netRel)},
		{Name: proto.String(n("proxy")), IpCidrRange: proto.String("10.78.0.0/23"), Network: proto.String(netRel),
			Purpose: proto.String("REGIONAL_MANAGED_PROXY"), Role: proto.String("ACTIVE")},
	} {
		op, err := subs.Insert(ctx, &computepb.InsertSubnetworkRequest{Project: project, Region: region, SubnetworkResource: sn})
		wait("the subnetwork "+sn.GetName(), op, err)
		name := sn.GetName()
		later(t, func(c context.Context) (*computeapi.Operation, error) {
			return subs.Delete(c, &computepb.DeleteSubnetworkRequest{Project: project, Region: region, Subnetwork: name})
		})
	}

	ns, err := networksecurity.NewRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ns.Close() }()
	nsv, err := networkservices.NewRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = nsv.Close() }()
	dep, err := networkservices.NewDepRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dep.Close() }()

	// A URL list.
	lop, err := ns.CreateUrlList(ctx, &networksecuritypb.CreateUrlListRequest{Parent: parent, UrlListId: n("list"),
		UrlList: &networksecuritypb.UrlList{Values: []string{"127.0.0.1/allowed/*"}}})
	if err != nil {
		t.Fatal(err)
	}
	list, err := lop.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ns.CreateUrlList(ctx, &networksecuritypb.CreateUrlListRequest{Parent: parent, UrlListId: n("bad"),
		UrlList: &networksecuritypb.UrlList{Values: []string{"localhost"}}}); err == nil {
		t.Error("CreateUrlList accepted an entry without a period")
	}
	if got, err := ns.GetUrlList(ctx, &networksecuritypb.GetUrlListRequest{Name: list.GetName()}); err != nil || got.GetValues()[0] != "127.0.0.1/allowed/*" {
		t.Fatalf("GetUrlList: %v %v", got, err)
	}
	if !swpListed(ns.ListUrlLists(ctx, &networksecuritypb.ListUrlListsRequest{Parent: parent}).Next, list.GetName()) {
		t.Error("ListUrlLists: the list is missing")
	}
	uop, err := ns.UpdateUrlList(ctx, &networksecuritypb.UpdateUrlListRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"values"}},
		UrlList: &networksecuritypb.UrlList{Name: list.GetName(), Values: []string{"127.0.0.1/allowed/*", "*.example.com"}}})
	if err != nil {
		t.Fatal(err)
	}
	if list, err = uop.Wait(ctx); err != nil || len(list.GetValues()) != 2 {
		t.Fatalf("UpdateUrlList: %v %v", list, err)
	}

	// A TLS inspection policy, and the policy that names it.
	top, err := ns.CreateTlsInspectionPolicy(ctx, &networksecuritypb.CreateTlsInspectionPolicyRequest{Parent: parent, TlsInspectionPolicyId: n("tls"),
		TlsInspectionPolicy: &networksecuritypb.TlsInspectionPolicy{CaPool: parent + "/caPools/" + n("pool")}})
	if err != nil {
		t.Fatal(err)
	}
	tip, err := top.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := ns.GetTlsInspectionPolicy(ctx, &networksecuritypb.GetTlsInspectionPolicyRequest{Name: tip.GetName()}); err != nil || got.GetCaPool() != tip.GetCaPool() {
		t.Fatalf("GetTlsInspectionPolicy: %v %v", got, err)
	}
	if !swpListed(ns.ListTlsInspectionPolicies(ctx, &networksecuritypb.ListTlsInspectionPoliciesRequest{Parent: parent}).Next, tip.GetName()) {
		t.Error("ListTlsInspectionPolicies: the policy is missing")
	}
	tuop, err := ns.UpdateTlsInspectionPolicy(ctx, &networksecuritypb.UpdateTlsInspectionPolicyRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"description"}},
		TlsInspectionPolicy: &networksecuritypb.TlsInspectionPolicy{Name: tip.GetName(), Description: "inspect"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tuop.Wait(ctx); err != nil {
		t.Fatal(err)
	}

	pop, err := ns.CreateGatewaySecurityPolicy(ctx, &networksecuritypb.CreateGatewaySecurityPolicyRequest{Parent: parent, GatewaySecurityPolicyId: n("pol"),
		GatewaySecurityPolicy: &networksecuritypb.GatewaySecurityPolicy{Description: "basic Secure Web Proxy policy", TlsInspectionPolicy: tip.GetName()}})
	if err != nil {
		t.Fatal(err)
	}
	pol, err := pop.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := ns.GetGatewaySecurityPolicy(ctx, &networksecuritypb.GetGatewaySecurityPolicyRequest{Name: pol.GetName()}); err != nil || got.GetTlsInspectionPolicy() != tip.GetName() {
		t.Fatalf("GetGatewaySecurityPolicy: %v %v", got, err)
	}
	if !swpListed(ns.ListGatewaySecurityPolicies(ctx, &networksecuritypb.ListGatewaySecurityPoliciesRequest{Parent: parent}).Next, pol.GetName()) {
		t.Error("ListGatewaySecurityPolicies: the policy is missing")
	}
	if _, err := ns.DeleteTlsInspectionPolicy(ctx, &networksecuritypb.DeleteTlsInspectionPolicyRequest{Name: tip.GetName()}); err == nil {
		t.Error("DeleteTlsInspectionPolicy of a policy in use succeeded")
	}

	// The rules: lowest priority first, the first match decides.
	rule := func(id string, prio int32, allow bool, session, app string, tls bool) {
		t.Helper()
		bp := networksecuritypb.GatewaySecurityPolicyRule_ALLOW
		if !allow {
			bp = networksecuritypb.GatewaySecurityPolicyRule_DENY
		}
		rop, err := ns.CreateGatewaySecurityPolicyRule(ctx, &networksecuritypb.CreateGatewaySecurityPolicyRuleRequest{Parent: pol.GetName(),
			GatewaySecurityPolicyRuleId: id, GatewaySecurityPolicyRule: &networksecuritypb.GatewaySecurityPolicyRule{Enabled: true, Priority: prio,
				Profile: &networksecuritypb.GatewaySecurityPolicyRule_BasicProfile_{BasicProfile: bp}, SessionMatcher: session,
				ApplicationMatcher: app, TlsInspectionEnabled: tls}})
		if err != nil {
			t.Fatalf("rule %s: %v", id, err)
		}
		if _, err := rop.Wait(ctx); err != nil {
			t.Fatalf("rule %s: %v", id, err)
		}
	}
	rule("deny-blocked", 1, false, "host() == '127.0.0.1'", "request.path.startsWith('/allowed/blocked')", false)
	rule("allow-list", 2, true, "host() == '127.0.0.1'", "inUrlList(request.url(), '"+list.GetName()+"')", false)
	rule("allow-tunnel", 3, true, "host() == 'localhost' && destination.port == "+port, "", false)
	rule("inspect", 4, true, "host() == 'inspected.example.com'", "request.method == 'GET'", true)
	if _, err := ns.CreateGatewaySecurityPolicyRule(ctx, &networksecuritypb.CreateGatewaySecurityPolicyRuleRequest{Parent: pol.GetName(),
		GatewaySecurityPolicyRuleId: "dup", GatewaySecurityPolicyRule: &networksecuritypb.GatewaySecurityPolicyRule{Enabled: true, Priority: 1,
			Profile: &networksecuritypb.GatewaySecurityPolicyRule_BasicProfile_{BasicProfile: networksecuritypb.GatewaySecurityPolicyRule_ALLOW}, SessionMatcher: "true"}}); err == nil {
		t.Error("a second rule with priority 1 was accepted")
	}
	if _, err := ns.CreateGatewaySecurityPolicyRule(ctx, &networksecuritypb.CreateGatewaySecurityPolicyRuleRequest{Parent: pol.GetName(),
		GatewaySecurityPolicyRuleId: "badcel", GatewaySecurityPolicyRule: &networksecuritypb.GatewaySecurityPolicyRule{Enabled: true, Priority: 50,
			Profile: &networksecuritypb.GatewaySecurityPolicyRule_BasicProfile_{BasicProfile: networksecuritypb.GatewaySecurityPolicyRule_ALLOW}, SessionMatcher: "request.method == 'GET'"}}); err == nil {
		t.Error("a sessionMatcher reading request attributes was accepted")
	}
	ruleName := pol.GetName() + "/rules/inspect"
	if got, err := ns.GetGatewaySecurityPolicyRule(ctx, &networksecuritypb.GetGatewaySecurityPolicyRuleRequest{Name: ruleName}); err != nil || !got.GetTlsInspectionEnabled() {
		t.Fatalf("GetGatewaySecurityPolicyRule: %v %v", got, err)
	}
	var prios []int32
	for it := ns.ListGatewaySecurityPolicyRules(ctx, &networksecuritypb.ListGatewaySecurityPolicyRulesRequest{Parent: pol.GetName()}); ; {
		r, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		prios = append(prios, r.GetPriority())
	}
	if fmt.Sprint(prios) != "[1 2 3 4]" {
		t.Errorf("ListGatewaySecurityPolicyRules: priorities %v", prios)
	}
	ruop, err := ns.UpdateGatewaySecurityPolicyRule(ctx, &networksecuritypb.UpdateGatewaySecurityPolicyRuleRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"enabled"}},
		GatewaySecurityPolicyRule: &networksecuritypb.GatewaySecurityPolicyRule{Name: ruleName, Enabled: false}})
	if err != nil {
		t.Fatal(err)
	}
	if r, err := ruop.Wait(ctx); err != nil || r.GetEnabled() {
		t.Fatalf("UpdateGatewaySecurityPolicyRule: %v %v", r, err)
	}
	gpop, err := ns.UpdateGatewaySecurityPolicy(ctx, &networksecuritypb.UpdateGatewaySecurityPolicyRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"description"}},
		GatewaySecurityPolicy: &networksecuritypb.GatewaySecurityPolicy{Name: pol.GetName(), Description: "updated"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gpop.Wait(ctx); err != nil {
		t.Fatal(err)
	}

	// The gateway, as the quickstart's gateway.yaml.
	gop, err := nsv.CreateGateway(ctx, &networkservicespb.CreateGatewayRequest{Parent: parent, GatewayId: n("swp"),
		Gateway: &networkservicespb.Gateway{Type: networkservicespb.Gateway_SECURE_WEB_GATEWAY, Ports: []int32{443},
			GatewaySecurityPolicy: pol.GetName(), Network: netRel, Subnetwork: "projects/" + project + "/regions/" + region + "/subnetworks/" + n("sub"),
			RoutingMode: networkservicespb.Gateway_EXPLICIT_ROUTING_MODE}})
	if err != nil {
		t.Fatal(err)
	}
	gw, err := gop.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if op, err := nsv.DeleteGateway(context.Background(), &networkservicespb.DeleteGatewayRequest{Name: gw.GetName()}); err == nil {
			_ = op.Wait(context.Background())
		}
	}()
	if len(gw.GetAddresses()) != 1 || !strings.HasPrefix(gw.GetAddresses()[0], "10.77.0.") {
		t.Errorf("the gateway's address %v is not from its subnetwork", gw.GetAddresses())
	}
	if _, err := nsv.CreateGateway(ctx, &networkservicespb.CreateGatewayRequest{Parent: parent, GatewayId: n("six"),
		Gateway: &networkservicespb.Gateway{Type: networkservicespb.Gateway_SECURE_WEB_GATEWAY, Ports: []int32{1, 2, 3, 4, 5, 6},
			Network: netRel, Subnetwork: gw.GetSubnetwork()}}); err == nil {
		t.Error("a gateway with six ports was accepted")
	}
	if _, err := ns.DeleteGatewaySecurityPolicy(ctx, &networksecuritypb.DeleteGatewaySecurityPolicyRequest{Name: pol.GetName()}); err == nil {
		t.Error("DeleteGatewaySecurityPolicy of a policy a gateway names succeeded")
	}
	if !swpListed(nsv.ListGateways(ctx, &networkservicespb.ListGatewaysRequest{Parent: parent}).Next, gw.GetName()) {
		t.Error("ListGateways: the gateway is missing")
	}
	guop, err := nsv.UpdateGateway(ctx, &networkservicespb.UpdateGatewayRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"description"}},
		Gateway: &networkservicespb.Gateway{Name: gw.GetName(), Description: "my swp"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := guop.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := nsv.UpdateGateway(ctx, &networkservicespb.UpdateGatewayRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"ports"}},
		Gateway: &networkservicespb.Gateway{Name: gw.GetName(), Ports: []int32{80}}}); err == nil {
		t.Error("UpdateGateway changed the ports")
	}
	if got, err := nsv.GetGateway(ctx, &networkservicespb.GetGatewayRequest{Name: gw.GetName()}); err != nil || got.GetDescription() != "my swp" {
		t.Fatalf("GetGateway: %v %v", got, err)
	}

	// Where the proxy listens: the gateway's JSON names it.
	resp, err := http.Get("http://" + computeAddr + "/v1/" + gw.GetName())
	if err != nil {
		t.Fatal(err)
	}
	var raw struct {
		Address string `json:"cloudburrowAddress"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&raw)
	_ = resp.Body.Close()
	if raw.Address == "" {
		t.Fatal("the gateway's JSON has no cloudburrowAddress")
	}
	pu, _ := url.Parse("http://" + raw.Address)
	c := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(pu)}, Timeout: 20 * time.Second}
	for path, want := range map[string]int{"/allowed/page": 200, "/allowed/blocked/page": 403, "/other": 403} {
		r, err := c.Get(origin.URL + path)
		if err != nil {
			t.Fatalf("GET %s through the proxy: %v", path, err)
		}
		body, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()
		if r.StatusCode != want || want == 200 && string(body) != "origin "+path {
			t.Errorf("GET %s through the proxy: %d %q, want %d", path, r.StatusCode, body, want)
		}
	}
	// HTTPS-style tunnelling: CONNECT localhost:PORT is allowed by
	// allow-tunnel; CONNECT 127.0.0.1:PORT matches no session-only rule.
	connect := func(target string) string {
		cn, err := net.DialTimeout("tcp", raw.Address, 10*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = cn.Close() }()
		_ = cn.SetDeadline(time.Now().Add(20 * time.Second))
		fmt.Fprintf(cn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
		b := make([]byte, 1024)
		k, _ := cn.Read(b)
		head := string(b[:k])
		if !strings.HasPrefix(head, "HTTP/1.1 200") {
			return head
		}
		fmt.Fprintf(cn, "GET /tunnelled HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n")
		rest, _ := io.ReadAll(cn)
		return head + string(rest)
	}
	if got := connect("localhost:" + port); !strings.Contains(got, "origin /tunnelled") {
		t.Errorf("CONNECT localhost: %q", got)
	}
	if got := connect("127.0.0.1:" + port); !strings.HasPrefix(got, "HTTP/1.1 403") {
		t.Errorf("CONNECT 127.0.0.1: %q, want 403", got)
	}

	// An authorization extension (Service Extensions) on a regional
	// backend service.
	rbs, _ := computeapi.NewRegionBackendServicesRESTClient(ctx, opts...)
	defer func() { _ = rbs.Close() }()
	op, err = rbs.Insert(ctx, &computepb.InsertRegionBackendServiceRequest{Project: project, Region: region, BackendServiceResource: &computepb.BackendService{
		Name: proto.String(n("authz")), Protocol: proto.String("HTTP2"), LoadBalancingScheme: proto.String("INTERNAL_MANAGED")}})
	wait("the callout backend service", op, err)
	later(t, func(c context.Context) (*computeapi.Operation, error) {
		return rbs.Delete(c, &computepb.DeleteRegionBackendServiceRequest{Project: project, Region: region, BackendService: n("authz")})
	})
	eop, err := dep.CreateAuthzExtension(ctx, &networkservicespb.CreateAuthzExtensionRequest{Parent: parent, AuthzExtensionId: n("ext"),
		AuthzExtension: &networkservicespb.AuthzExtension{Service: "https://www.googleapis.com/compute/v1/projects/" + project + "/regions/" + region + "/backendServices/" + n("authz"),
			Authority: "authz.example.com", LoadBalancingScheme: networkservicespb.LoadBalancingScheme_INTERNAL_MANAGED,
			Timeout: durationpb.New(100 * time.Millisecond), FailOpen: true, ForwardHeaders: []string{"Authorization"}}})
	if err != nil {
		t.Fatal(err)
	}
	ext, err := eop.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if ext.GetWireFormat() != networkservicespb.WireFormat_EXT_PROC_GRPC {
		t.Errorf("the extension's wire format %v, want EXT_PROC_GRPC by default", ext.GetWireFormat())
	}
	if _, err := dep.CreateAuthzExtension(ctx, &networkservicespb.CreateAuthzExtensionRequest{Parent: parent, AuthzExtensionId: n("fast"),
		AuthzExtension: &networkservicespb.AuthzExtension{Service: ext.GetService(), Authority: "a", LoadBalancingScheme: networkservicespb.LoadBalancingScheme_INTERNAL_MANAGED,
			Timeout: durationpb.New(time.Millisecond)}}); err == nil {
		t.Error("a 1 ms timeout was accepted")
	}
	if got, err := dep.GetAuthzExtension(ctx, &networkservicespb.GetAuthzExtensionRequest{Name: ext.GetName()}); err != nil || !got.GetFailOpen() {
		t.Fatalf("GetAuthzExtension: %v %v", got, err)
	}
	if !swpListed(dep.ListAuthzExtensions(ctx, &networkservicespb.ListAuthzExtensionsRequest{Parent: parent}).Next, ext.GetName()) {
		t.Error("ListAuthzExtensions: the extension is missing")
	}
	euop, err := dep.UpdateAuthzExtension(ctx, &networkservicespb.UpdateAuthzExtensionRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"fail_open"}},
		AuthzExtension: &networkservicespb.AuthzExtension{Name: ext.GetName(), FailOpen: false}})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := euop.Wait(ctx); err != nil || got.GetFailOpen() {
		t.Fatalf("UpdateAuthzExtension: %v %v", got, err)
	}
	dop, err := dep.DeleteAuthzExtension(ctx, &networkservicespb.DeleteAuthzExtensionRequest{Name: ext.GetName()})
	if err != nil {
		t.Fatal(err)
	}
	if err := dop.Wait(ctx); err != nil {
		t.Fatal(err)
	}

	// Teardown in order: the gateway, then the policy (and its rules),
	// the TLS inspection policy and the URL list.
	gdop, err := nsv.DeleteGateway(ctx, &networkservicespb.DeleteGatewayRequest{Name: gw.GetName()})
	if err != nil {
		t.Fatal(err)
	}
	if err := gdop.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := net.DialTimeout("tcp", raw.Address, 2*time.Second); err == nil {
		t.Error("the deleted gateway still listens")
	}
	rdop, err := ns.DeleteGatewaySecurityPolicyRule(ctx, &networksecuritypb.DeleteGatewaySecurityPolicyRuleRequest{Name: ruleName})
	if err != nil {
		t.Fatal(err)
	}
	if err := rdop.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	pdop, err := ns.DeleteGatewaySecurityPolicy(ctx, &networksecuritypb.DeleteGatewaySecurityPolicyRequest{Name: pol.GetName()})
	if err != nil {
		t.Fatal(err)
	}
	if err := pdop.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	tdop, err := ns.DeleteTlsInspectionPolicy(ctx, &networksecuritypb.DeleteTlsInspectionPolicyRequest{Name: tip.GetName()})
	if err != nil {
		t.Fatal(err)
	}
	if err := tdop.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	ldop, err := ns.DeleteUrlList(ctx, &networksecuritypb.DeleteUrlListRequest{Name: list.GetName()})
	if err != nil {
		t.Fatal(err)
	}
	if err := ldop.Wait(ctx); err != nil {
		t.Fatal(err)
	}
}

// swpListed is whether a listing holds name.
func swpListed[T interface{ GetName() string }](next func() (T, error), name string) bool {
	for {
		x, err := next()
		if err != nil {
			return false
		}
		if x.GetName() == name {
			return true
		}
	}
}
