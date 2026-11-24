//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	computeapi "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	"google.golang.org/protobuf/proto"
)

// drainWebScript serves port 80 one connection at a time with
// netcat-openbsd, as lbWebScript does, answering /slow after 12 seconds
// and anything else at once with "VM-<hostname>"; port 81 answers the
// health check.
const drainWebScript = `#!/bin/bash
cat > /usr/local/bin/drainweb <<'EOF'
#!/bin/bash
body="VM-$(hostname)"
while true; do
  coproc NC { nc -lN 80; }
  read -r line <&"${NC[0]}"
  case "$line" in *" /slow "*) sleep 12 ;; esac
  printf 'HTTP/1.1 200 OK\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s' ${#body} "$body" >&"${NC[1]}"
  exec {NC[1]}>&-
  wait
done
EOF
cat > /usr/local/bin/drainhc <<'EOF'
#!/bin/bash
while true; do printf 'HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok' | nc -lN 81 >/dev/null 2>&1; done
EOF
chmod +x /usr/local/bin/drainweb /usr/local/bin/drainhc
systemd-run --unit=drainweb /usr/local/bin/drainweb
systemd-run --unit=drainhc /usr/local/bin/drainhc
`

// TestComputeConnectionDraining2226 (#2226), the official compute/apiv1
// REST clients, as load-balancing/docs/enabling-connection-draining sets
// it up: a global external Application Load Balancer whose backend
// service has connectionDraining.drainingTimeoutSec 45, in front of a
// zonal managed instance group of one debian-12 VM with an autohealing
// health check. A request the VM takes 12 seconds to answer is in flight
// when deleteInstances removes the VM: the group lists it DELETING and
// its instanceHealth DRAINING, still RUNNING; getHealth no longer finds
// it HEALTHY; a new request is refused (503, no healthy upstream); the
// request in flight is answered 200 by the VM; and the group deletes the
// VM once the timeout has passed. drainingTimeoutSec beyond 3600 is
// refused, and patch sets it.
// covers: compute.backendServices.insert, compute.backendServices.patch, compute.backendServices.get, compute.backendServices.getHealth, compute.instanceGroupManagers.deleteInstances, compute.instanceGroupManagers.listManagedInstances
func TestComputeConnectionDraining2226(t *testing.T) {
	h := New(t)
	opts := computeOpts(t, h) // skips without CLOUDBURROW_TEST_COMPUTE
	computeAddr := h.Endpoint(EnvCompute)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	t.Cleanup(cancel)
	project, zone := h.Project(), "us-central1-a"
	sfx := time.Now().UnixNano() % 1e6
	n := func(s string) string { return fmt.Sprintf("cd-%s-%d", s, sfx) }
	tpls, err := computeapi.NewInstanceTemplatesRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	igms, _ := computeapi.NewInstanceGroupManagersRESTClient(ctx, opts...)
	fws, _ := computeapi.NewFirewallsRESTClient(ctx, opts...)
	hcs, _ := computeapi.NewHealthChecksRESTClient(ctx, opts...)
	bss, _ := computeapi.NewBackendServicesRESTClient(ctx, opts...)
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
	groupTemplate(t, ctx, tpls, project, n("tpl"), drainWebScript)
	op, err := fws.Insert(ctx, &computepb.InsertFirewallRequest{Project: project, FirewallResource: &computepb.Firewall{Name: proto.String(n("hc")),
		Network: proto.String("global/networks/default"), SourceRanges: []string{"35.191.0.0/16", "130.211.0.0/22"},
		Allowed: []*computepb.Allowed{{IPProtocol: proto.String("tcp"), Ports: []string{"80", "81"}}}}})
	do("the health-check firewall rule", op, err)
	later(func(c context.Context) (*computeapi.Operation, error) {
		return fws.Delete(c, &computepb.DeleteFirewallRequest{Project: project, Firewall: n("hc")})
	})
	op, err = hcs.Insert(ctx, &computepb.InsertHealthCheckRequest{Project: project, HealthCheckResource: &computepb.HealthCheck{
		Name: proto.String(n("hc")), Type: proto.String("HTTP"), CheckIntervalSec: proto.Int32(2), TimeoutSec: proto.Int32(2),
		HealthyThreshold: proto.Int32(1), UnhealthyThreshold: proto.Int32(3),
		HttpHealthCheck: &computepb.HTTPHealthCheck{Port: proto.Int32(81), RequestPath: proto.String("/")}}})
	do("the health check", op, err)
	later(func(c context.Context) (*computeapi.Operation, error) {
		return hcs.Delete(c, &computepb.DeleteHealthCheckRequest{Project: project, HealthCheck: n("hc")})
	})
	hcLink := "global/healthChecks/" + n("hc")
	op, err = igms.Insert(ctx, &computepb.InsertInstanceGroupManagerRequest{Project: project, Zone: zone,
		InstanceGroupManagerResource: &computepb.InstanceGroupManager{Name: proto.String(n("web")), BaseInstanceName: proto.String(n("web")),
			InstanceTemplate: proto.String("global/instanceTemplates/" + n("tpl")), TargetSize: proto.Int32(1),
			NamedPorts:          []*computepb.NamedPort{{Name: proto.String("http"), Port: proto.Int32(80)}},
			AutoHealingPolicies: []*computepb.InstanceGroupManagerAutoHealingPolicy{{HealthCheck: proto.String(hcLink), InitialDelaySec: proto.Int32(600)}}}})
	do("the managed instance group", op, err)
	later(func(c context.Context) (*computeapi.Operation, error) {
		return igms.Delete(c, &computepb.DeleteInstanceGroupManagerRequest{Project: project, Zone: zone, InstanceGroupManager: n("web")})
	})
	group := "zones/" + zone + "/instanceGroups/" + n("web")

	// drainingTimeoutSec is 0 to 3600.
	if _, err := bss.Insert(ctx, &computepb.InsertBackendServiceRequest{Project: project, BackendServiceResource: &computepb.BackendService{
		Name: proto.String(n("bad")), LoadBalancingScheme: proto.String("EXTERNAL_MANAGED"),
		ConnectionDraining: &computepb.ConnectionDraining{DrainingTimeoutSec: proto.Int32(3601)}}}); computeCode(err) != 400 {
		t.Fatalf("drainingTimeoutSec 3601: %v, want 400", err)
	}
	op, err = bss.Insert(ctx, &computepb.InsertBackendServiceRequest{Project: project, BackendServiceResource: &computepb.BackendService{
		Name: proto.String(n("web")), Protocol: proto.String("HTTP"), PortName: proto.String("http"),
		LoadBalancingScheme: proto.String("EXTERNAL_MANAGED"), HealthChecks: []string{hcLink},
		ConnectionDraining: &computepb.ConnectionDraining{DrainingTimeoutSec: proto.Int32(10)},
		Backends:           []*computepb.Backend{{Group: proto.String(group), BalancingMode: proto.String("UTILIZATION")}}}})
	do("the backend service", op, err)
	later(func(c context.Context) (*computeapi.Operation, error) {
		return bss.Delete(c, &computepb.DeleteBackendServiceRequest{Project: project, BackendService: n("web")})
	})
	op, err = bss.Patch(ctx, &computepb.PatchBackendServiceRequest{Project: project, BackendService: n("web"), BackendServiceResource: &computepb.BackendService{
		ConnectionDraining: &computepb.ConnectionDraining{DrainingTimeoutSec: proto.Int32(45)}}})
	do("patching the draining timeout", op, err)
	if b, err := bss.Get(ctx, &computepb.GetBackendServiceRequest{Project: project, BackendService: n("web")}); err != nil ||
		b.GetConnectionDraining().GetDrainingTimeoutSec() != 45 {
		t.Fatalf("connectionDraining: %v, %v", b.GetConnectionDraining(), err)
	}
	op, err = ums.Insert(ctx, &computepb.InsertUrlMapRequest{Project: project, UrlMapResource: &computepb.UrlMap{Name: proto.String(n("map")),
		DefaultService: proto.String("global/backendServices/" + n("web"))}})
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
	fetch := func(path string) (int, string) {
		resp, err := (&http.Client{Timeout: time.Minute}).Get("http://" + addr + path)
		if err != nil {
			return 0, err.Error()
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	list := func() []*computepb.ManagedInstance {
		var out []*computepb.ManagedInstance
		it := igms.ListManagedInstances(ctx, &computepb.ListManagedInstancesInstanceGroupManagersRequest{Project: project, Zone: zone, InstanceGroupManager: n("web")})
		for {
			mi, err := it.Next()
			if err != nil {
				return out
			}
			out = append(out, mi)
		}
	}
	health := func(mi *computepb.ManagedInstance) string {
		if ih := mi.GetInstanceHealth(); len(ih) > 0 {
			return ih[0].GetDetailedHealthState()
		}
		return ""
	}
	getHealth := func() []*computepb.HealthStatus {
		hh, err := bss.GetHealth(ctx, &computepb.GetHealthBackendServiceRequest{Project: project, BackendService: n("web"),
			ResourceGroupReferenceResource: &computepb.ResourceGroupReference{Group: proto.String(group)}})
		if err != nil {
			return nil
		}
		return hh.GetHealthStatus()
	}
	var vm *computepb.ManagedInstance
	waitFor(t, "the VM HEALTHY", 20*time.Minute, func() bool {
		mis := list()
		if len(mis) != 1 || health(mis[0]) != "HEALTHY" {
			return false
		}
		vm = mis[0]
		hs := getHealth()
		return len(hs) == 1 && hs[0].GetHealthState() == "HEALTHY"
	})
	waitFor(t, "the VM to answer /", 3*time.Minute, func() bool {
		code, body := fetch("/")
		return code == 200 && strings.HasPrefix(body, "VM-")
	})

	// A request in flight when the VM leaves the group. The VM's server
	// takes one connection at a time and listens again a moment after
	// each answer, so the request waits for that first.
	time.Sleep(2 * time.Second)
	type result struct {
		code int
		body string
	}
	slow := make(chan result, 1)
	go func() {
		code, body := fetch("/slow")
		slow <- result{code, body}
	}()
	time.Sleep(3 * time.Second)
	removed := time.Now()
	op, err = igms.DeleteInstances(ctx, &computepb.DeleteInstancesInstanceGroupManagerRequest{Project: project, Zone: zone, InstanceGroupManager: n("web"),
		InstanceGroupManagersDeleteInstancesRequestResource: &computepb.InstanceGroupManagersDeleteInstancesRequest{Instances: []string{vm.GetInstance()}}})
	do("deleteInstances", op, err)
	mis := list()
	if len(mis) != 1 || mis[0].GetCurrentAction() != "DELETING" || health(mis[0]) != "DRAINING" || mis[0].GetInstanceStatus() != "RUNNING" {
		t.Fatalf("the draining VM: %v", mis)
	}
	if hs := getHealth(); len(hs) != 1 || hs[0].GetHealthState() == "HEALTHY" {
		t.Fatalf("getHealth while draining: %v", hs)
	}
	if code, body := fetch("/"); code != http.StatusServiceUnavailable {
		t.Fatalf("a new request while the VM drains: %d %q, want 503", code, body)
	}
	r := <-slow
	if r.code != 200 || !strings.HasPrefix(r.body, "VM-") {
		t.Fatalf("the request in flight: %d %q, want 200 from the VM", r.code, r.body)
	}
	if mis := list(); len(mis) != 1 || health(mis[0]) != "DRAINING" {
		t.Fatalf("the VM after its request finished, within the timeout: %v", mis)
	}
	waitFor(t, "the VM deleted after the timeout", 5*time.Minute, func() bool { return len(list()) == 0 })
	if d := time.Since(removed); d < 45*time.Second {
		t.Fatalf("the VM was deleted %v after it left the group, before the 45s timeout", d)
	}
}

// TestGcloudComputeConnectionDraining2226 (#2226): the pinned gcloud
// through gcloud-setup, egress to Google blocked, every command on a
// served API: backend-services create --connection-draining-timeout,
// describe, update --connection-draining-timeout, a timeout beyond 3600
// refused, delete.
func TestGcloudComputeConnectionDraining2226(t *testing.T) {
	h := New(t)
	_ = computeOpts(t, h) // skips without CLOUDBURROW_TEST_COMPUTE
	gcloud := gcloudBinary(t)
	cli := os.Getenv(EnvCLI)
	if cli == "" {
		t.Skipf("%s is not set", EnvCLI)
	}
	gdir, home := t.TempDir(), t.TempDir()
	cfg := gcloudSetup(t, cli, gdir)
	run := func(args ...string) (string, error) {
		cmd := exec.Command(gcloud, append(args, "--project", h.Project(), "--quiet")...)
		cmd.Env = append(gcloudTargetEnv(noGoogleEgress()), "CLOUDSDK_CONFIG="+gdir, "CLOUDSDK_ACTIVE_CONFIG_NAME="+cfg, "HOME="+home)
		b, err := cmd.CombinedOutput()
		return string(b), err
	}
	gc := func(args ...string) string {
		t.Helper()
		out, err := run(args...)
		if err != nil {
			t.Fatalf("gcloud %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return out
	}
	name := fmt.Sprintf("gcd-%d", time.Now().UnixNano()%1e6)
	t.Cleanup(func() { _, _ = run("compute", "backend-services", "delete", name, "--global") })
	gc("compute", "backend-services", "create", name, "--global", "--protocol", "HTTP", "--load-balancing-scheme", "EXTERNAL_MANAGED",
		"--connection-draining-timeout", "30")
	describe := func() string {
		return strings.TrimSpace(gc("compute", "backend-services", "describe", name, "--global", "--format=value(connectionDraining.drainingTimeoutSec)"))
	}
	if got := describe(); got != "30" {
		t.Fatalf("describe after create: %q", got)
	}
	gc("compute", "backend-services", "update", name, "--global", "--connection-draining-timeout", "300")
	if got := describe(); got != "300" {
		t.Fatalf("describe after update: %q", got)
	}
	if out, err := run("compute", "backend-services", "update", name, "--global", "--connection-draining-timeout", "3601"); err == nil {
		t.Fatalf("a timeout of 3601 was accepted: %s", out)
	}
	if got := describe(); got != "300" {
		t.Fatalf("describe after the refused update: %q", got)
	}
	gc("compute", "backend-services", "delete", name, "--global")
}

// TestTerraformComputeConnectionDraining2226 (#2226): a
// google_compute_backend_service with connection_draining_timeout_sec 60
// (and a google_compute_region_backend_service with 0) applies through
// `cloudburrow terraform` with egress to Google blocked; the official
// client reads both back; the plan is clean; 120 applies in place and the
// plan is clean again; destroy removes them.
// covers: compute.backendServices.insert, compute.backendServices.get, compute.backendServices.patch, compute.backendServices.delete, compute.regionBackendServices.insert, compute.regionBackendServices.get, compute.regionBackendServices.delete
func TestTerraformComputeConnectionDraining2226(t *testing.T) {
	h := New(t)
	opts := computeOpts(t, h) // skips without CLOUDBURROW_TEST_COMPUTE
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	t.Cleanup(cancel)
	bss, err := computeapi.NewBackendServicesRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	rbss, _ := computeapi.NewRegionBackendServicesRESTClient(ctx, opts...)
	m := newTFModule(t)
	const region = "us-central1"
	name := fmt.Sprintf("tfcd-%d", time.Now().UnixNano()%1e6)
	module := func(timeout int) string {
		return fmt.Sprintf(`
resource "google_compute_backend_service" "global" {
  project                         = %[1]q
  name                            = "%[2]s-g"
  protocol                        = "HTTP"
  load_balancing_scheme           = "EXTERNAL_MANAGED"
  connection_draining_timeout_sec = %[4]d
}
resource "google_compute_region_backend_service" "regional" {
  project                         = %[1]q
  name                            = "%[2]s-r"
  region                          = %[3]q
  protocol                        = "TCP"
  load_balancing_scheme           = "INTERNAL"
  connection_draining_timeout_sec = 0
}
`, m.project, name, region, timeout)
	}
	check := func(want int32) {
		t.Helper()
		g, err := bss.Get(ctx, &computepb.GetBackendServiceRequest{Project: m.project, BackendService: name + "-g"})
		if err != nil || g.GetConnectionDraining().GetDrainingTimeoutSec() != want {
			t.Fatalf("the global backend service's connectionDraining: %v, %v, want %d", g.GetConnectionDraining(), err, want)
		}
		r, err := rbss.Get(ctx, &computepb.GetRegionBackendServiceRequest{Project: m.project, Region: region, BackendService: name + "-r"})
		if err != nil || r.GetConnectionDraining() == nil || r.GetConnectionDraining().GetDrainingTimeoutSec() != 0 {
			t.Fatalf("the regional backend service's connectionDraining: %v, %v", r.GetConnectionDraining(), err)
		}
	}
	m.write(module(60))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	check(60)
	m.planClean()
	m.write(module(120))
	m.apply()
	check(120)
	m.planClean()
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := bss.Get(ctx, &computepb.GetBackendServiceRequest{Project: m.project, BackendService: name + "-g"}); computeCode(err) != 404 {
		t.Errorf("the global backend service after destroy: %v", err)
	}
}
