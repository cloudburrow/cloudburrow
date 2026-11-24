//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/apphub/apiv1/apphubpb"
	computeapi "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	"google.golang.org/protobuf/proto"
)

// lbWebScript serves "VM-<hostname>" on port 80 with netcat-openbsd, one
// connection at a time, which the debian-12 guest image has.
const lbWebScript = `#!/bin/bash
cat > /usr/local/bin/lbweb <<'EOF'
#!/bin/bash
body="VM-$(hostname)"
while true; do printf 'HTTP/1.1 200 OK\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s' ${#body} "$body" | nc -lN 80 >/dev/null 2>&1; done
EOF
chmod +x /usr/local/bin/lbweb
systemd-run --unit=lbweb /usr/local/bin/lbweb
`

// covers: compute.healthChecks.insert, compute.healthChecks.get, compute.healthChecks.delete, compute.backendServices.insert, compute.backendServices.get, compute.backendServices.getHealth, compute.backendServices.delete, compute.regionNetworkEndpointGroups.insert, compute.regionNetworkEndpointGroups.get, compute.regionNetworkEndpointGroups.delete, compute.urlMaps.insert, compute.urlMaps.get, compute.urlMaps.validate, compute.urlMaps.delete, compute.targetHttpProxies.insert, compute.targetHttpProxies.get, compute.targetHttpProxies.delete, compute.globalForwardingRules.insert, compute.globalForwardingRules.get, compute.globalForwardingRules.delete
//
// TestComputeExternalHTTPLoadBalancerThroughTheGoClient (#2153), the
// official compute/apiv1 REST clients, as
// load-balancing/docs/https/setup-global-ext-https-compute and
// .../setup-global-ext-https-serverless set one up: a zonal managed
// instance group of two debian-12 VMs serving port 80 (named port http),
// a firewall rule for Google's health-check ranges, an HTTP health check,
// a backend service of the group and one of a serverless network
// endpoint group of a Cloud Run service, a URL map sending /api/* to Cloud
// Run and the rest to the VMs (validated first), a target HTTP proxy and
// a global forwarding rule on port 80. The rule's address (127.0.0.1) and
// local port take real requests, routed by path; the backend service
// reports both VMs HEALTHY; deletion of an in-use URL map is refused.
// Then App Hub discovers the forwarding rule and backend services as
// services and the instance group as a workload, at supported-resources'
// URIs.
func TestComputeExternalHTTPLoadBalancerThroughTheGoClient(t *testing.T) {
	h := New(t)
	computeAddr := shardEndpoint(h, "CLOUDBURROW_TEST_RUN_COMPUTE", EnvCompute)
	opts := computeOpts(t, h)
	h.Endpoint(EnvRun) // skips without Cloud Run
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	t.Cleanup(cancel)
	project, zone, region := h.Project(), "us-central1-a", "us-central1"
	sfx := time.Now().UnixNano() % 1e6
	n := func(s string) string { return fmt.Sprintf("lb-%s-%d", s, sfx) }

	// The Cloud Run service behind /api/*.
	runSvc, _ := deployAppHubResources(t, h, project, n("api"))
	runName := runSvc[strings.LastIndex(runSvc, "/")+1:]

	tpls, err := computeapi.NewInstanceTemplatesRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	igms, _ := computeapi.NewInstanceGroupManagersRESTClient(ctx, opts...)
	fws, _ := computeapi.NewFirewallsRESTClient(ctx, opts...)
	hcs, _ := computeapi.NewHealthChecksRESTClient(ctx, opts...)
	bss, _ := computeapi.NewBackendServicesRESTClient(ctx, opts...)
	negs, _ := computeapi.NewRegionNetworkEndpointGroupsRESTClient(ctx, opts...)
	ums, _ := computeapi.NewUrlMapsRESTClient(ctx, opts...)
	thps, _ := computeapi.NewTargetHttpProxiesRESTClient(ctx, opts...)
	gfrs, _ := computeapi.NewGlobalForwardingRulesRESTClient(ctx, opts...)
	do := func(what string, op *computeapi.Operation, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if err := op.Wait(ctx); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	later := func(f func(c context.Context) (*computeapi.Operation, error)) {
		t.Cleanup(func() {
			c := context.Background()
			if op, err := f(c); err == nil {
				_ = op.Wait(c)
			}
		})
	}

	groupTemplate(t, ctx, tpls, project, n("tpl"), lbWebScript)
	op, err := igms.Insert(ctx, &computepb.InsertInstanceGroupManagerRequest{Project: project, Zone: zone,
		InstanceGroupManagerResource: &computepb.InstanceGroupManager{Name: proto.String(n("web")), BaseInstanceName: proto.String(n("web")),
			InstanceTemplate: proto.String("global/instanceTemplates/" + n("tpl")), TargetSize: proto.Int32(2),
			NamedPorts: []*computepb.NamedPort{{Name: proto.String("http"), Port: proto.Int32(80)}}}})
	do("the managed instance group", op, err)
	later(func(c context.Context) (*computeapi.Operation, error) {
		return igms.Delete(c, &computepb.DeleteInstanceGroupManagerRequest{Project: project, Zone: zone, InstanceGroupManager: n("web")})
	})
	op, err = fws.Insert(ctx, &computepb.InsertFirewallRequest{Project: project, FirewallResource: &computepb.Firewall{Name: proto.String(n("hc")),
		Network: proto.String("global/networks/default"), SourceRanges: []string{"35.191.0.0/16", "130.211.0.0/22"},
		Allowed: []*computepb.Allowed{{IPProtocol: proto.String("tcp"), Ports: []string{"80"}}}}})
	do("the health-check firewall rule", op, err)
	later(func(c context.Context) (*computeapi.Operation, error) {
		return fws.Delete(c, &computepb.DeleteFirewallRequest{Project: project, Firewall: n("hc")})
	})
	op, err = hcs.Insert(ctx, &computepb.InsertHealthCheckRequest{Project: project, HealthCheckResource: &computepb.HealthCheck{
		Name: proto.String(n("hc")), Type: proto.String("HTTP"), CheckIntervalSec: proto.Int32(2), TimeoutSec: proto.Int32(2),
		HttpHealthCheck: &computepb.HTTPHealthCheck{PortSpecification: proto.String("USE_SERVING_PORT"), RequestPath: proto.String("/")}}})
	do("the health check", op, err)
	later(func(c context.Context) (*computeapi.Operation, error) {
		return hcs.Delete(c, &computepb.DeleteHealthCheckRequest{Project: project, HealthCheck: n("hc")})
	})
	op, err = bss.Insert(ctx, &computepb.InsertBackendServiceRequest{Project: project, BackendServiceResource: &computepb.BackendService{
		Name: proto.String(n("web")), Protocol: proto.String("HTTP"), PortName: proto.String("http"),
		LoadBalancingScheme: proto.String("EXTERNAL_MANAGED"), HealthChecks: []string{"global/healthChecks/" + n("hc")},
		Backends: []*computepb.Backend{{Group: proto.String("zones/" + zone + "/instanceGroups/" + n("web")), BalancingMode: proto.String("UTILIZATION")}}}})
	do("the VMs' backend service", op, err)
	later(func(c context.Context) (*computeapi.Operation, error) {
		return bss.Delete(c, &computepb.DeleteBackendServiceRequest{Project: project, BackendService: n("web")})
	})
	op, err = negs.Insert(ctx, &computepb.InsertRegionNetworkEndpointGroupRequest{Project: project, Region: region,
		NetworkEndpointGroupResource: &computepb.NetworkEndpointGroup{Name: proto.String(n("api")), NetworkEndpointType: proto.String("SERVERLESS"),
			CloudRun: &computepb.NetworkEndpointGroupCloudRun{Service: proto.String(runName)}}})
	do("the serverless network endpoint group", op, err)
	later(func(c context.Context) (*computeapi.Operation, error) {
		return negs.Delete(c, &computepb.DeleteRegionNetworkEndpointGroupRequest{Project: project, Region: region, NetworkEndpointGroup: n("api")})
	})
	op, err = bss.Insert(ctx, &computepb.InsertBackendServiceRequest{Project: project, BackendServiceResource: &computepb.BackendService{
		Name: proto.String(n("api")), LoadBalancingScheme: proto.String("EXTERNAL_MANAGED"),
		Backends: []*computepb.Backend{{Group: proto.String("regions/" + region + "/networkEndpointGroups/" + n("api"))}}}})
	do("Cloud Run's backend service", op, err)
	later(func(c context.Context) (*computeapi.Operation, error) {
		return bss.Delete(c, &computepb.DeleteBackendServiceRequest{Project: project, BackendService: n("api")})
	})
	um := &computepb.UrlMap{Name: proto.String(n("map")), DefaultService: proto.String("global/backendServices/" + n("web")),
		HostRules: []*computepb.HostRule{{Hosts: []string{"*"}, PathMatcher: proto.String("paths")}},
		PathMatchers: []*computepb.PathMatcher{{Name: proto.String("paths"), DefaultService: proto.String("global/backendServices/" + n("web")),
			PathRules: []*computepb.PathRule{{Paths: []string{"/api", "/api/*"}, Service: proto.String("global/backendServices/" + n("api"))}}}},
		Tests: []*computepb.UrlMapTest{{Host: proto.String("example.com"), Path: proto.String("/api/x"),
			Service: proto.String("global/backendServices/" + n("api"))}}}
	v, err := ums.Validate(ctx, &computepb.ValidateUrlMapRequest{Project: project, UrlMap: n("map"),
		UrlMapsValidateRequestResource: &computepb.UrlMapsValidateRequest{Resource: um}})
	if err != nil || !v.GetResult().GetLoadSucceeded() || !v.GetResult().GetTestPassed() {
		t.Fatalf("validate: %v %v", v, err)
	}
	op, err = ums.Insert(ctx, &computepb.InsertUrlMapRequest{Project: project, UrlMapResource: um})
	do("the URL map", op, err)
	later(func(c context.Context) (*computeapi.Operation, error) {
		return ums.Delete(c, &computepb.DeleteUrlMapRequest{Project: project, UrlMap: n("map")})
	})
	op, err = thps.Insert(ctx, &computepb.InsertTargetHttpProxyRequest{Project: project, TargetHttpProxyResource: &computepb.TargetHttpProxy{
		Name: proto.String(n("proxy")), UrlMap: proto.String("global/urlMaps/" + n("map"))}})
	do("the target HTTP proxy", op, err)
	later(func(c context.Context) (*computeapi.Operation, error) {
		return thps.Delete(c, &computepb.DeleteTargetHttpProxyRequest{Project: project, TargetHttpProxy: n("proxy")})
	})
	op, err = gfrs.Insert(ctx, &computepb.InsertGlobalForwardingRuleRequest{Project: project, ForwardingRuleResource: &computepb.ForwardingRule{
		Name: proto.String(n("rule")), Target: proto.String("global/targetHttpProxies/" + n("proxy")), PortRange: proto.String("80"),
		LoadBalancingScheme: proto.String("EXTERNAL_MANAGED"), IPProtocol: proto.String("TCP")}})
	do("the forwarding rule", op, err)
	later(func(c context.Context) (*computeapi.Operation, error) {
		return gfrs.Delete(c, &computepb.DeleteGlobalForwardingRuleRequest{Project: project, ForwardingRule: n("rule")})
	})
	rule, err := gfrs.Get(ctx, &computepb.GetGlobalForwardingRuleRequest{Project: project, ForwardingRule: n("rule")})
	if err != nil || rule.GetIPAddress() != "127.0.0.1" || rule.GetPortRange() != "80-80" {
		t.Fatalf("the forwarding rule: %v %v", rule, err)
	}
	if _, err := ums.Delete(ctx, &computepb.DeleteUrlMapRequest{Project: project, UrlMap: n("map")}); computeCode(err) != http.StatusBadRequest {
		t.Errorf("deleting the URL map in use: %v", err)
	}
	// The rule's local port, from its JSON (cloudburrowAddress, which the
	// client's proto does not carry).
	resp, err := http.Get("http://" + computeAddr + "/compute/v1/projects/" + project + "/global/forwardingRules/" + n("rule"))
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&raw)
	resp.Body.Close()
	addr, _ := raw["cloudburrowAddress"].(string)
	if !strings.HasPrefix(addr, "127.0.0.1:") {
		t.Fatalf("the rule's address: %v", raw)
	}
	waitFor(t, "both VMs HEALTHY", 20*time.Minute, func() bool {
		hh, err := bss.GetHealth(ctx, &computepb.GetHealthBackendServiceRequest{Project: project, BackendService: n("web"),
			ResourceGroupReferenceResource: &computepb.ResourceGroupReference{Group: proto.String("zones/" + zone + "/instanceGroups/" + n("web"))}})
		if err != nil || len(hh.GetHealthStatus()) != 2 {
			return false
		}
		for _, s := range hh.GetHealthStatus() {
			if s.GetHealthState() != "HEALTHY" {
				return false
			}
		}
		return true
	})
	fetch := func(path string) (int, string) {
		resp, err := http.Get("http://" + addr + path)
		if err != nil {
			return 0, err.Error()
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	vms := map[string]bool{}
	waitFor(t, "the VMs to answer /", 3*time.Minute, func() bool {
		code, body := fetch("/")
		if code == 200 && strings.HasPrefix(body, "VM-") {
			vms[body] = true
		}
		return len(vms) == 2
	})
	waitFor(t, "Cloud Run to answer /api/hello", 3*time.Minute, func() bool {
		code, body := fetch("/api/hello")
		return code == 200 && strings.Contains(body, "Hello")
	})

	t.Run("AppHubDiscovers", func(t *testing.T) {
		h := h.For(t)
		c := appHubClients(t, h)["grpc"] // skips without App Hub
		detachAppHubProject(t, h, c, project)
		host := "projects/" + project + "/locations/"
		aop, err := c.CreateServiceProjectAttachment(ctx, &apphubpb.CreateServiceProjectAttachmentRequest{Parent: host + "global",
			ServiceProjectAttachmentId: project, ServiceProjectAttachment: &apphubpb.ServiceProjectAttachment{ServiceProject: "projects/" + project}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := aop.Wait(ctx); err != nil {
			t.Fatal(err)
		}
		cp := "//compute.googleapis.com/projects/" + project + "/"
		for _, uri := range []string{cp + "global/forwardingRules/" + n("rule"), cp + "global/backendServices/" + n("web"),
			cp + "global/backendServices/" + n("api")} {
			ds, err := c.LookupDiscoveredService(ctx, &apphubpb.LookupDiscoveredServiceRequest{Parent: host + "global", Uri: uri})
			if err != nil || ds.GetDiscoveredService() == nil {
				t.Errorf("LookupDiscoveredService %s: %v, %v", uri, ds, err)
			}
		}
		uri := cp + "zones/" + zone + "/instanceGroups/" + n("web")
		dw, err := c.LookupDiscoveredWorkload(ctx, &apphubpb.LookupDiscoveredWorkloadRequest{Parent: host + region, Uri: uri})
		if err != nil || dw.GetDiscoveredWorkload() == nil {
			t.Errorf("LookupDiscoveredWorkload %s: %v, %v", uri, dw, err)
		}
	})
}
