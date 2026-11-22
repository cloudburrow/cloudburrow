//go:build compat

package compat

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	computeapi "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	"cloud.google.com/go/iam/apiv1/iampb"
	iap "cloud.google.com/go/iap/apiv1"
	"google.golang.org/protobuf/proto"
)

// lb2160 is an external Application Load Balancer on the instance whose
// backend service is a hybrid network endpoint group of an origin this
// test serves on 127.0.0.1, which the load balancer's local proxy
// reaches: no VM is needed.
type lb2160 struct {
	t        *testing.T
	ctx      context.Context
	project  string
	n        func(string) string
	base     string // the forwarding rule's local URL
	hits     *atomic.Int64
	bss      *computeapi.BackendServicesClient
	ums      *computeapi.UrlMapsClient
	compute  string
	original *httptest.Server
}

func (l *lb2160) do(what string, op *computeapi.Operation, err error) {
	l.t.Helper()
	if err != nil {
		l.t.Fatalf("%s: %v", what, err)
	}
	if err := op.Wait(l.ctx); err != nil {
		l.t.Fatalf("%s: %v", what, err)
	}
}

func later(t *testing.T, f func(c context.Context) (*computeapi.Operation, error)) {
	t.Cleanup(func() {
		c := context.Background()
		if op, err := f(c); err == nil {
			_ = op.Wait(c)
		}
	})
}

func newLB2160(t *testing.T, h *Harness, origin http.Handler) *lb2160 {
	t.Helper()
	computeAddr := h.Endpoint(EnvCompute) // skips without it
	opts := computeOpts(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	t.Cleanup(cancel)
	sfx := time.Now().UnixNano() % 1e6
	l := &lb2160{t: t, ctx: ctx, project: h.Project(), n: func(s string) string { return fmt.Sprintf("lb2-%s-%d", s, sfx) },
		hits: &atomic.Int64{}, compute: computeAddr}
	l.original = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			l.hits.Add(1)
		}
		origin.ServeHTTP(w, r)
	}))
	t.Cleanup(l.original.Close)
	host, port, _ := net.SplitHostPort(l.original.Listener.Addr().String())
	p, _ := strconv.Atoi(port)
	zone := "us-central1-a"
	negs, err := computeapi.NewNetworkEndpointGroupsRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	hcs, _ := computeapi.NewHealthChecksRESTClient(ctx, opts...)
	l.bss, _ = computeapi.NewBackendServicesRESTClient(ctx, opts...)
	l.ums, _ = computeapi.NewUrlMapsRESTClient(ctx, opts...)
	thps, _ := computeapi.NewTargetHttpProxiesRESTClient(ctx, opts...)
	gfrs, _ := computeapi.NewGlobalForwardingRulesRESTClient(ctx, opts...)
	op, err := negs.Insert(ctx, &computepb.InsertNetworkEndpointGroupRequest{Project: l.project, Zone: zone,
		NetworkEndpointGroupResource: &computepb.NetworkEndpointGroup{Name: proto.String(l.n("origin")),
			NetworkEndpointType: proto.String("NON_GCP_PRIVATE_IP_PORT"), Network: proto.String("global/networks/default"),
			DefaultPort: proto.Int32(int32(p))}})
	l.do("the hybrid network endpoint group", op, err)
	later(t, func(c context.Context) (*computeapi.Operation, error) {
		return negs.Delete(c, &computepb.DeleteNetworkEndpointGroupRequest{Project: l.project, Zone: zone, NetworkEndpointGroup: l.n("origin")})
	})
	op, err = negs.AttachNetworkEndpoints(ctx, &computepb.AttachNetworkEndpointsNetworkEndpointGroupRequest{Project: l.project, Zone: zone,
		NetworkEndpointGroup: l.n("origin"), NetworkEndpointGroupsAttachEndpointsRequestResource: &computepb.NetworkEndpointGroupsAttachEndpointsRequest{
			NetworkEndpoints: []*computepb.NetworkEndpoint{{IpAddress: proto.String(host), Port: proto.Int32(int32(p))}}}})
	l.do("the origin's endpoint", op, err)
	op, err = hcs.Insert(ctx, &computepb.InsertHealthCheckRequest{Project: l.project, HealthCheckResource: &computepb.HealthCheck{
		Name: proto.String(l.n("hc")), Type: proto.String("HTTP"), CheckIntervalSec: proto.Int32(2), TimeoutSec: proto.Int32(2),
		HttpHealthCheck: &computepb.HTTPHealthCheck{PortSpecification: proto.String("USE_SERVING_PORT"), RequestPath: proto.String("/healthz")}}})
	l.do("the health check", op, err)
	later(t, func(c context.Context) (*computeapi.Operation, error) {
		return hcs.Delete(c, &computepb.DeleteHealthCheckRequest{Project: l.project, HealthCheck: l.n("hc")})
	})
	op, err = l.bss.Insert(ctx, &computepb.InsertBackendServiceRequest{Project: l.project, BackendServiceResource: &computepb.BackendService{
		Name: proto.String(l.n("app")), Protocol: proto.String("HTTP"), LoadBalancingScheme: proto.String("EXTERNAL_MANAGED"),
		HealthChecks: []string{"global/healthChecks/" + l.n("hc")},
		Backends: []*computepb.Backend{{Group: proto.String("zones/" + zone + "/networkEndpointGroups/" + l.n("origin")),
			BalancingMode: proto.String("RATE"), MaxRatePerEndpoint: proto.Float32(100)}}}})
	l.do("the backend service", op, err)
	later(t, func(c context.Context) (*computeapi.Operation, error) {
		return l.bss.Delete(c, &computepb.DeleteBackendServiceRequest{Project: l.project, BackendService: l.n("app")})
	})
	op, err = l.ums.Insert(ctx, &computepb.InsertUrlMapRequest{Project: l.project, UrlMapResource: &computepb.UrlMap{
		Name: proto.String(l.n("map")), DefaultService: proto.String("global/backendServices/" + l.n("app"))}})
	l.do("the URL map", op, err)
	later(t, func(c context.Context) (*computeapi.Operation, error) {
		return l.ums.Delete(c, &computepb.DeleteUrlMapRequest{Project: l.project, UrlMap: l.n("map")})
	})
	op, err = thps.Insert(ctx, &computepb.InsertTargetHttpProxyRequest{Project: l.project, TargetHttpProxyResource: &computepb.TargetHttpProxy{
		Name: proto.String(l.n("proxy")), UrlMap: proto.String("global/urlMaps/" + l.n("map"))}})
	l.do("the target HTTP proxy", op, err)
	later(t, func(c context.Context) (*computeapi.Operation, error) {
		return thps.Delete(c, &computepb.DeleteTargetHttpProxyRequest{Project: l.project, TargetHttpProxy: l.n("proxy")})
	})
	op, err = gfrs.Insert(ctx, &computepb.InsertGlobalForwardingRuleRequest{Project: l.project, ForwardingRuleResource: &computepb.ForwardingRule{
		Name: proto.String(l.n("rule")), Target: proto.String("global/targetHttpProxies/" + l.n("proxy")), PortRange: proto.String("80"),
		LoadBalancingScheme: proto.String("EXTERNAL_MANAGED"), IPProtocol: proto.String("TCP")}})
	l.do("the forwarding rule", op, err)
	later(t, func(c context.Context) (*computeapi.Operation, error) {
		return gfrs.Delete(c, &computepb.DeleteGlobalForwardingRuleRequest{Project: l.project, ForwardingRule: l.n("rule")})
	})
	l.base = "http://" + ruleAddress(t, computeAddr, l.project, "global/forwardingRules/"+l.n("rule"))
	waitFor(t, "the origin to answer through the load balancer", 2*time.Minute, func() bool {
		code, _, _ := l.get("/healthz", nil)
		return code == 200
	})
	return l
}

// ruleAddress is a forwarding rule's local address, from its JSON
// (cloudburrowAddress, which the client's proto does not carry).
func ruleAddress(t *testing.T, computeAddr, project, rel string) string {
	t.Helper()
	resp, err := http.Get("http://" + computeAddr + "/compute/v1/projects/" + project + "/" + rel)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var raw map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&raw)
	addr, _ := raw["cloudburrowAddress"].(string)
	if !strings.HasPrefix(addr, "127.0.0.1:") {
		t.Fatalf("the rule's address: %v", raw)
	}
	return addr
}

// get sends a GET through the load balancer.
func (l *lb2160) get(path string, hdr map[string]string) (int, string, http.Header) {
	req, err := http.NewRequestWithContext(l.ctx, http.MethodGet, l.base+path, nil)
	if err != nil {
		l.t.Fatal(err)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err.Error(), nil
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header
}

// covers: compute.securityPolicies.insert, compute.securityPolicies.get, compute.securityPolicies.list, compute.securityPolicies.addRule, compute.securityPolicies.getRule, compute.securityPolicies.patchRule, compute.securityPolicies.removeRule, compute.securityPolicies.setLabels, compute.securityPolicies.listPreconfiguredExpressionSets, compute.securityPolicies.delete, compute.backendServices.setSecurityPolicy, compute.backendServices.getEffectiveSecurityPolicies, compute.networkEndpointGroups.insert, compute.networkEndpointGroups.attachNetworkEndpoints
//
// TestCloudArmorBlocksRequestsThroughTheGoClient (#2160), the official
// compute/apiv1 REST clients, as armor/docs/configure-security-policies
// sets a backend security policy up: a policy whose rules deny /admin by
// an advanced expression (403), SQL injection by the sqli-v33-stable
// preconfigured WAF rule (403), and throttle /limited to two requests a
// minute per IP (429), attached to the backend service; real requests
// through the load balancer's local port are blocked by each rule and
// pass otherwise; the rule methods, labels, the WAF expression sets and
// the documented refusals (duplicate priority, the default rule's
// removal, an interval Google does not offer, an in-use deletion).
func TestCloudArmorBlocksRequestsThroughTheGoClient(t *testing.T) {
	h := New(t)
	l := newLB2160(t, h, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "origin %s", r.URL.Path)
	}))
	ctx, project, n := l.ctx, l.project, l.n
	sps, err := computeapi.NewSecurityPoliciesRESTClient(ctx, computeOpts(t, h)...)
	if err != nil {
		t.Fatal(err)
	}
	pol := &computepb.SecurityPolicy{Name: proto.String(n("armor")), Description: proto.String("compat"), Rules: []*computepb.SecurityPolicyRule{
		{Priority: proto.Int32(100), Action: proto.String("deny(403)"),
			Match: &computepb.SecurityPolicyRuleMatcher{Expr: &computepb.Expr{Expression: proto.String("request.path.startsWith('/admin')")}}},
		{Priority: proto.Int32(200), Action: proto.String("deny(403)"),
			Match: &computepb.SecurityPolicyRuleMatcher{Expr: &computepb.Expr{Expression: proto.String("evaluatePreconfiguredWaf('sqli-v33-stable', {'sensitivity': 1})")}}},
		{Priority: proto.Int32(300), Action: proto.String("throttle"),
			Match: &computepb.SecurityPolicyRuleMatcher{Expr: &computepb.Expr{Expression: proto.String("request.path == '/limited'")}},
			RateLimitOptions: &computepb.SecurityPolicyRuleRateLimitOptions{ConformAction: proto.String("allow"), ExceedAction: proto.String("deny(429)"),
				EnforceOnKey: proto.String("IP"), RateLimitThreshold: &computepb.SecurityPolicyRuleRateLimitOptionsThreshold{Count: proto.Int32(2), IntervalSec: proto.Int32(60)}}},
	}}
	op, err := sps.Insert(ctx, &computepb.InsertSecurityPolicyRequest{Project: project, SecurityPolicyResource: pol})
	l.do("the security policy", op, err)
	later(t, func(c context.Context) (*computeapi.Operation, error) {
		return sps.Delete(c, &computepb.DeleteSecurityPolicyRequest{Project: project, SecurityPolicy: n("armor")})
	})
	got, err := sps.Get(ctx, &computepb.GetSecurityPolicyRequest{Project: project, SecurityPolicy: n("armor")})
	if err != nil || len(got.GetRules()) != 4 || got.GetRules()[3].GetPriority() != 2147483647 || got.GetType() != "CLOUD_ARMOR" {
		t.Fatalf("the policy (with Google's default rule): %v %v", got, err)
	}
	op, err = l.bss.SetSecurityPolicy(ctx, &computepb.SetSecurityPolicyBackendServiceRequest{Project: project, BackendService: n("app"),
		SecurityPolicyReferenceResource: &computepb.SecurityPolicyReference{SecurityPolicy: proto.String("global/securityPolicies/" + n("armor"))}})
	l.do("setSecurityPolicy", op, err)
	defer func() {
		op, err := l.bss.SetSecurityPolicy(context.Background(), &computepb.SetSecurityPolicyBackendServiceRequest{Project: project, BackendService: n("app"),
			SecurityPolicyReferenceResource: &computepb.SecurityPolicyReference{SecurityPolicy: proto.String("")}})
		if err == nil {
			_ = op.Wait(context.Background())
		}
	}()
	eff, err := l.bss.GetEffectiveSecurityPolicies(ctx, &computepb.GetEffectiveSecurityPoliciesBackendServiceRequest{Project: project, BackendService: n("app")})
	if err != nil {
		t.Fatalf("getEffectiveSecurityPolicies: %v", err)
	}
	_ = eff
	if code, body, _ := l.get("/admin/panel", nil); code != 403 || !strings.Contains(body, "403 Forbidden") {
		t.Errorf("an /admin request: %d %q", code, body)
	}
	if code, _, _ := l.get("/search?q="+url.QueryEscape("x' or '1'='1"), nil); code != 403 {
		t.Errorf("SQL injection: %d", code)
	}
	if code, body, _ := l.get("/hello", nil); code != 200 || body != "origin /hello" {
		t.Errorf("an allowed request: %d %q", code, body)
	}
	var codes []int
	for range 4 {
		code, _, _ := l.get("/limited", nil)
		codes = append(codes, code)
	}
	if fmt.Sprint(codes) != "[200 200 429 429]" {
		t.Errorf("throttled: %v", codes)
	}
	// The rule methods.
	r, err := sps.GetRule(ctx, &computepb.GetRuleSecurityPolicyRequest{Project: project, SecurityPolicy: n("armor"), Priority: proto.Int32(100)})
	if err != nil || r.GetAction() != "deny(403)" {
		t.Fatalf("getRule: %v %v", r, err)
	}
	op, err = sps.PatchRule(ctx, &computepb.PatchRuleSecurityPolicyRequest{Project: project, SecurityPolicy: n("armor"), Priority: proto.Int32(100),
		SecurityPolicyRuleResource: &computepb.SecurityPolicyRule{Action: proto.String("deny(404)")}})
	l.do("patchRule", op, err)
	if code, _, _ := l.get("/admin/panel", nil); code != 404 {
		t.Errorf("after patchRule: %d", code)
	}
	op, err = sps.AddRule(ctx, &computepb.AddRuleSecurityPolicyRequest{Project: project, SecurityPolicy: n("armor"),
		SecurityPolicyRuleResource: &computepb.SecurityPolicyRule{Priority: proto.Int32(50), Action: proto.String("deny(502)"),
			Match: &computepb.SecurityPolicyRuleMatcher{VersionedExpr: proto.String("SRC_IPS_V1"),
				Config: &computepb.SecurityPolicyRuleMatcherConfig{SrcIpRanges: []string{"127.0.0.1/32"}}}}})
	l.do("addRule", op, err)
	if code, _, _ := l.get("/hello", nil); code != 502 {
		t.Errorf("an IP rule: %d", code)
	}
	op, err = sps.RemoveRule(ctx, &computepb.RemoveRuleSecurityPolicyRequest{Project: project, SecurityPolicy: n("armor"), Priority: proto.Int32(50)})
	l.do("removeRule", op, err)
	if code, _, _ := l.get("/hello", nil); code != 200 {
		t.Errorf("after removeRule: %d", code)
	}
	// The documented refusals.
	if _, err := sps.AddRule(ctx, &computepb.AddRuleSecurityPolicyRequest{Project: project, SecurityPolicy: n("armor"),
		SecurityPolicyRuleResource: &computepb.SecurityPolicyRule{Priority: proto.Int32(100), Action: proto.String("allow"),
			Match: &computepb.SecurityPolicyRuleMatcher{VersionedExpr: proto.String("SRC_IPS_V1"),
				Config: &computepb.SecurityPolicyRuleMatcherConfig{SrcIpRanges: []string{"*"}}}}}); computeCode(err) != 400 {
		t.Errorf("a duplicate priority: %v", err)
	}
	if _, err := sps.RemoveRule(ctx, &computepb.RemoveRuleSecurityPolicyRequest{Project: project, SecurityPolicy: n("armor"),
		Priority: proto.Int32(2147483647)}); computeCode(err) != 400 {
		t.Errorf("removing the default rule: %v", err)
	}
	if _, err := sps.AddRule(ctx, &computepb.AddRuleSecurityPolicyRequest{Project: project, SecurityPolicy: n("armor"),
		SecurityPolicyRuleResource: &computepb.SecurityPolicyRule{Priority: proto.Int32(10), Action: proto.String("throttle"),
			Match: &computepb.SecurityPolicyRuleMatcher{VersionedExpr: proto.String("SRC_IPS_V1"), Config: &computepb.SecurityPolicyRuleMatcherConfig{SrcIpRanges: []string{"*"}}},
			RateLimitOptions: &computepb.SecurityPolicyRuleRateLimitOptions{ConformAction: proto.String("allow"), ExceedAction: proto.String("deny(429)"),
				RateLimitThreshold: &computepb.SecurityPolicyRuleRateLimitOptionsThreshold{Count: proto.Int32(2), IntervalSec: proto.Int32(7)}}}}); computeCode(err) != 400 {
		t.Errorf("an interval Google does not offer: %v", err)
	}
	if _, err := sps.Delete(ctx, &computepb.DeleteSecurityPolicyRequest{Project: project, SecurityPolicy: n("armor")}); computeCode(err) != 400 {
		t.Errorf("deleting a policy in use: %v", err)
	}
	got, _ = sps.Get(ctx, &computepb.GetSecurityPolicyRequest{Project: project, SecurityPolicy: n("armor")})
	op, err = sps.SetLabels(ctx, &computepb.SetLabelsSecurityPolicyRequest{Project: project, Resource: n("armor"),
		GlobalSetLabelsRequestResource: &computepb.GlobalSetLabelsRequest{Labels: map[string]string{"env": "compat"}, LabelFingerprint: got.LabelFingerprint}})
	l.do("setLabels", op, err)
	sets, err := sps.ListPreconfiguredExpressionSets(ctx, &computepb.ListPreconfiguredExpressionSetsSecurityPoliciesRequest{Project: project})
	if err != nil || len(sets.GetPreconfiguredExpressionSets().GetWafRules().GetExpressionSets()) == 0 {
		t.Fatalf("listPreconfiguredExpressionSets: %v %v", sets, err)
	}
	found := false
	it := sps.List(ctx, &computepb.ListSecurityPoliciesRequest{Project: project})
	for {
		p, err := it.Next()
		if err != nil {
			break
		}
		if p.GetName() == n("armor") && p.GetLabels()["env"] == "compat" {
			found = true
		}
	}
	if !found {
		t.Error("list: the labelled policy is missing")
	}
}

// covers: compute.backendServices.patch, compute.backendServices.addSignedUrlKey, compute.backendServices.deleteSignedUrlKey, compute.urlMaps.invalidateCache
//
// TestCloudCDNCachesThroughTheGoClient (#2160): Cloud CDN enabled on the
// backend service (CACHE_ALL_STATIC, a 300-second client TTL, a custom
// response header reporting {cdn_cache_status}), as
// cdn/docs/setting-up-cdn-with-ex-backend and .../using-cache-invalidation
// describe: a static response is a MISS (one origin request) then a HIT
// with Age; urlMaps.invalidateCache makes it a MISS again; with a signed
// URL key only a signed URL (HMAC-SHA1, Expires and KeyName) is served.
func TestCloudCDNCachesThroughTheGoClient(t *testing.T) {
	h := New(t)
	l := newLB2160(t, h, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "public, max-age=3600")
		fmt.Fprintf(w, "png %d", time.Now().UnixNano())
	}))
	ctx, project, n := l.ctx, l.project, l.n
	bs, err := l.bss.Get(ctx, &computepb.GetBackendServiceRequest{Project: project, BackendService: n("app")})
	if err != nil {
		t.Fatal(err)
	}
	op, err := l.bss.Patch(ctx, &computepb.PatchBackendServiceRequest{Project: project, BackendService: n("app"), BackendServiceResource: &computepb.BackendService{
		Fingerprint: bs.Fingerprint, EnableCDN: proto.Bool(true), CustomResponseHeaders: []string{"X-Cache-Status: {cdn_cache_status}"},
		CdnPolicy: &computepb.BackendServiceCdnPolicy{CacheMode: proto.String("CACHE_ALL_STATIC"), ClientTtl: proto.Int32(300),
			DefaultTtl: proto.Int32(3600), MaxTtl: proto.Int32(86400)}}})
	l.do("enabling Cloud CDN", op, err)
	bs, _ = l.bss.Get(ctx, &computepb.GetBackendServiceRequest{Project: project, BackendService: n("app")})
	if !bs.GetEnableCDN() || bs.GetCdnPolicy().GetCacheMode() != "CACHE_ALL_STATIC" {
		t.Fatalf("the backend service: %v", bs)
	}
	l.hits.Store(0)
	_, b1, h1 := l.get("/logo.png", nil)
	_, b2, h2 := l.get("/logo.png", nil)
	if h1.Get("X-Cache-Status") != "miss" || h2.Get("X-Cache-Status") != "hit" || b1 != b2 || l.hits.Load() != 1 || h2.Get("Age") == "" {
		t.Fatalf("MISS then HIT: %v / %v, origin requests %d", h1, h2, l.hits.Load())
	}
	op, err = l.ums.InvalidateCache(ctx, &computepb.InvalidateCacheUrlMapRequest{Project: project, UrlMap: n("map"),
		CacheInvalidationRuleResource: &computepb.CacheInvalidationRule{Path: proto.String("/logo.png")}})
	l.do("invalidateCache", op, err)
	if _, b3, h3 := l.get("/logo.png", nil); h3.Get("X-Cache-Status") != "miss" || b3 == b1 {
		t.Fatalf("after invalidation: %v", h3)
	}
	key := make([]byte, 16)
	_, _ = rand.Read(key)
	op, err = l.bss.AddSignedUrlKey(ctx, &computepb.AddSignedUrlKeyBackendServiceRequest{Project: project, BackendService: n("app"),
		SignedUrlKeyResource: &computepb.SignedUrlKey{KeyName: proto.String("compat"), KeyValue: proto.String(base64.URLEncoding.EncodeToString(key))}})
	l.do("addSignedUrlKey", op, err)
	if code, _, _ := l.get("/logo.png", nil); code != 403 {
		t.Errorf("an unsigned request: %d", code)
	}
	u := l.base + "/logo.png?Expires=" + strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10) + "&KeyName=compat"
	mac := hmac.New(sha1.New, key)
	mac.Write([]byte(u))
	signed := strings.TrimPrefix(u, l.base) + "&Signature=" + base64.URLEncoding.EncodeToString(mac.Sum(nil))
	if code, _, _ := l.get(signed, nil); code != 200 {
		t.Errorf("a signed URL: %d", code)
	}
	op, err = l.bss.DeleteSignedUrlKey(ctx, &computepb.DeleteSignedUrlKeyBackendServiceRequest{Project: project, BackendService: n("app"), KeyName: "compat"})
	l.do("deleteSignedUrlKey", op, err)
	if code, _, _ := l.get("/logo.png", nil); code != 200 {
		t.Errorf("after the key is deleted: %d", code)
	}
}

// covers: compute.backendServices.patch
//
// TestIAPGuardsALoadBalancerThroughTheGoClient (#2160): IAP enabled on
// the backend service with an OAuth client (iap/docs/enabling-compute-howto),
// roles/iap.httpsResourceAccessor granted to the instance's service
// account on the service's iap_web resource through the IAP Go client;
// then, through the load balancer's local port, a request without a token
// is refused 401 ("Invalid IAP credentials: empty token"), one with an ID
// token for another audience 401, and one with an ID token for the OAuth
// client (generateIdToken, iap/docs/authentication-howto) reaches the
// origin with x-goog-iap-jwt-assertion and the identity headers; a request
// forging the headers has them replaced.
func TestIAPGuardsALoadBalancerThroughTheGoClient(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvMetadata) // skips without it
	var seen atomic.Value
	l := newLB2160(t, h, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.Store(r.Header.Clone())
		_, _ = io.WriteString(w, "ok")
	}))
	ctx, project, n := l.ctx, l.project, l.n
	account, _ := instanceAccount(t)
	admin, err := iap.NewIdentityAwareProxyAdminRESTClient(ctx, computeOpts(t, h)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	res := "projects/" + project + "/iap_web/compute/services/" + n("app")
	if _, err := admin.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: res, Policy: &iampb.Policy{
		Bindings: []*iampb.Binding{{Role: "roles/iap.httpsResourceAccessor", Members: []string{"serviceAccount:" + account}}}}}); err != nil {
		t.Fatalf("SetIamPolicy: %v", err)
	}
	client := n("client") + ".apps.googleusercontent.com"
	bs, _ := l.bss.Get(ctx, &computepb.GetBackendServiceRequest{Project: project, BackendService: n("app")})
	op, err := l.bss.Patch(ctx, &computepb.PatchBackendServiceRequest{Project: project, BackendService: n("app"), BackendServiceResource: &computepb.BackendService{
		Fingerprint: bs.Fingerprint, Iap: &computepb.BackendServiceIAP{Enabled: proto.Bool(true), Oauth2ClientId: proto.String(client),
			Oauth2ClientSecret: proto.String("compat-secret")}}})
	l.do("enabling IAP", op, err)
	bs, _ = l.bss.Get(ctx, &computepb.GetBackendServiceRequest{Project: project, BackendService: n("app")})
	if !bs.GetIap().GetEnabled() || bs.GetIap().GetOauth2ClientSecretSha256() == "" || bs.GetIap().GetOauth2ClientSecret() != "" {
		t.Fatalf("the backend service's iap: %v", bs.GetIap())
	}
	if code, body, _ := l.get("/", nil); code != 401 || body != "Invalid IAP credentials: empty token" {
		t.Errorf("without a token: %d %q", code, body)
	}
	other := idTokenFor(t, h, account, "https://example.com/other")
	if code, _, _ := l.get("/", map[string]string{"Authorization": "Bearer " + other}); code != 401 {
		t.Errorf("another audience: %d", code)
	}
	tok := idTokenFor(t, h, account, client)
	code, body, _ := l.get("/", map[string]string{"Authorization": "Bearer " + tok, "X-Goog-Authenticated-User-Email": "forged"})
	if code != 200 || body != "ok" {
		t.Fatalf("with a token: %d %q", code, body)
	}
	got := seen.Load().(http.Header)
	if got.Get("X-Goog-Authenticated-User-Email") != "accounts.google.com:"+account || got.Get("X-Goog-Iap-Jwt-Assertion") == "" {
		t.Errorf("the headers the origin saw: %v", got)
	}
	parts := strings.Split(got.Get("X-Goog-Iap-Jwt-Assertion"), ".")
	if len(parts) == 3 {
		raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
		var claims map[string]any
		_ = json.Unmarshal(raw, &claims)
		if claims["iss"] != "https://cloud.google.com/iap" || !strings.HasSuffix(fmt.Sprint(claims["aud"]), "/global/backendServices/"+fmt.Sprint(bs.GetId())) {
			t.Errorf("the assertion's claims: %v", claims)
		}
	}
	// Without the role, 403.
	if _, err := admin.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: res, Policy: &iampb.Policy{}}); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := l.get("/", map[string]string{"Authorization": "Bearer " + tok}); code != 403 {
		t.Errorf("without the role: %d", code)
	}
	op, err = l.bss.Patch(ctx, &computepb.PatchBackendServiceRequest{Project: project, BackendService: n("app"), BackendServiceResource: &computepb.BackendService{
		Iap: &computepb.BackendServiceIAP{Enabled: proto.Bool(false)}}})
	l.do("disabling IAP", op, err)
}
