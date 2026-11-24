//go:build compat

package compat

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	computeapi "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	"google.golang.org/protobuf/proto"
)

// TestComputeMIGAutohealing2225 (#2225), the official compute/apiv1 REST
// clients, as compute/docs/instance-groups/autohealing-instances-in-migs
// sets it up: a firewall rule for Google's probe ranges, an HTTP health
// check of port 80, and a zonal group of two debian-12 VMs serving port 80
// with autoHealingPolicies (the health check, initialDelaySec 30). Both
// VMs end HEALTHY in listManagedInstances' instanceHealth and the group
// stable. The health check moved to port 81, which nothing serves, the
// VMs go TIMEOUT and the group recreates them by the same names (new ids),
// never more at a time than maxUnavailable (1); moved back, the group is
// stable again. Out-of-range initialDelaySec and two policies are refused.
// covers: compute.instanceGroupManagers.insert, compute.instanceGroupManagers.patch, compute.instanceGroupManagers.get, compute.instanceGroupManagers.listManagedInstances
func TestComputeMIGAutohealing2225(t *testing.T) {
	h := New(t)
	opts := computeOpts(t, h) // skips without CLOUDBURROW_TEST_COMPUTE
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	t.Cleanup(cancel)
	project, zone := h.Project(), "us-central1-a"
	tpls, err := computeapi.NewInstanceTemplatesRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	igms, _ := computeapi.NewInstanceGroupManagersRESTClient(ctx, opts...)
	fws, _ := computeapi.NewFirewallsRESTClient(ctx, opts...)
	hcs, _ := computeapi.NewHealthChecksRESTClient(ctx, opts...)
	sfx := time.Now().UnixNano() % 1e6
	n := func(s string) string { return fmt.Sprintf("ah-%s-%d", s, sfx) }
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
	op, err := fws.Insert(ctx, &computepb.InsertFirewallRequest{Project: project, FirewallResource: &computepb.Firewall{Name: proto.String(n("hc")),
		Network: proto.String("global/networks/default"), SourceRanges: []string{"35.191.0.0/16", "130.211.0.0/22"},
		Allowed: []*computepb.Allowed{{IPProtocol: proto.String("tcp"), Ports: []string{"80", "81"}}}}})
	do("the health-check firewall rule", op, err)
	later(func(c context.Context) (*computeapi.Operation, error) {
		return fws.Delete(c, &computepb.DeleteFirewallRequest{Project: project, Firewall: n("hc")})
	})
	hcOf := func(port int32) *computepb.HealthCheck {
		return &computepb.HealthCheck{Name: proto.String(n("hc")), Type: proto.String("HTTP"), CheckIntervalSec: proto.Int32(2), TimeoutSec: proto.Int32(2),
			HealthyThreshold: proto.Int32(1), UnhealthyThreshold: proto.Int32(2),
			HttpHealthCheck: &computepb.HTTPHealthCheck{Port: proto.Int32(port), RequestPath: proto.String("/")}}
	}
	op, err = hcs.Insert(ctx, &computepb.InsertHealthCheckRequest{Project: project, HealthCheckResource: hcOf(80)})
	do("the health check", op, err)
	later(func(c context.Context) (*computeapi.Operation, error) {
		return hcs.Delete(c, &computepb.DeleteHealthCheckRequest{Project: project, HealthCheck: n("hc")})
	})
	hcLink := "global/healthChecks/" + n("hc")

	// Refusals.
	for _, pols := range [][]*computepb.InstanceGroupManagerAutoHealingPolicy{
		{{HealthCheck: proto.String(hcLink), InitialDelaySec: proto.Int32(3601)}},
		{{HealthCheck: proto.String(hcLink)}, {HealthCheck: proto.String(hcLink)}},
		{{HealthCheck: proto.String("global/healthChecks/" + n("none"))}},
	} {
		_, err := igms.Insert(ctx, &computepb.InsertInstanceGroupManagerRequest{Project: project, Zone: zone,
			InstanceGroupManagerResource: &computepb.InstanceGroupManager{Name: proto.String(n("bad")), InstanceTemplate: proto.String("global/instanceTemplates/" + n("tpl")),
				TargetSize: proto.Int32(0), AutoHealingPolicies: pols}})
		if c := computeCode(err); c != 400 && c != 404 {
			t.Fatalf("autoHealingPolicies %v: %v, want a refusal", pols, err)
		}
	}

	op, err = igms.Insert(ctx, &computepb.InsertInstanceGroupManagerRequest{Project: project, Zone: zone,
		InstanceGroupManagerResource: &computepb.InstanceGroupManager{Name: proto.String(n("web")), BaseInstanceName: proto.String(n("web")),
			InstanceTemplate: proto.String("global/instanceTemplates/" + n("tpl")), TargetSize: proto.Int32(2),
			AutoHealingPolicies: []*computepb.InstanceGroupManagerAutoHealingPolicy{{HealthCheck: proto.String(hcLink), InitialDelaySec: proto.Int32(30)}}}})
	do("the autohealing group", op, err)
	later(func(c context.Context) (*computeapi.Operation, error) {
		return igms.Delete(c, &computepb.DeleteInstanceGroupManagerRequest{Project: project, Zone: zone, InstanceGroupManager: n("web")})
	})
	if _, err := hcs.Delete(ctx, &computepb.DeleteHealthCheckRequest{Project: project, HealthCheck: n("hc")}); computeCode(err) != 400 {
		t.Fatalf("deleting the health check a group autoheals with: %v, want 400", err)
	}
	g, err := igms.Get(ctx, &computepb.GetInstanceGroupManagerRequest{Project: project, Zone: zone, InstanceGroupManager: n("web")})
	if err != nil || len(g.GetAutoHealingPolicies()) != 1 || g.GetAutoHealingPolicies()[0].GetInitialDelaySec() != 30 ||
		!strings.HasSuffix(g.GetAutoHealingPolicies()[0].GetHealthCheck(), "/"+hcLink) {
		t.Fatalf("the group's autoHealingPolicies: %v, %v", g.GetAutoHealingPolicies(), err)
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
	stable := func() bool {
		g, err := igms.Get(ctx, &computepb.GetInstanceGroupManagerRequest{Project: project, Zone: zone, InstanceGroupManager: n("web")})
		return err == nil && g.GetStatus().GetIsStable()
	}
	ids := map[string]uint64{}
	waitFor(t, "both VMs HEALTHY and the group stable", 15*time.Minute, func() bool {
		mis := list()
		if len(mis) != 2 {
			return false
		}
		for _, mi := range mis {
			if health(mi) != "HEALTHY" || mi.GetCurrentAction() != "NONE" {
				return false
			}
			ids[mi.GetName()] = mi.GetId()
		}
		return stable()
	})

	op, err = hcs.Update(ctx, &computepb.UpdateHealthCheckRequest{Project: project, HealthCheck: n("hc"), HealthCheckResource: hcOf(81)})
	do("the health check moved to port 81", op, err)
	waitFor(t, "a VM TIMEOUT", 5*time.Minute, func() bool {
		for _, mi := range list() {
			if health(mi) == "TIMEOUT" {
				return true
			}
		}
		return false
	})
	waitFor(t, "a VM recreated by the same name", 10*time.Minute, func() bool {
		g, err := igms.Get(ctx, &computepb.GetInstanceGroupManagerRequest{Project: project, Zone: zone, InstanceGroupManager: n("web")})
		if err == nil && g.GetCurrentActions().GetRecreating() > 1 {
			t.Fatalf("recreating %d VMs at once, beyond maxUnavailable 1", g.GetCurrentActions().GetRecreating())
		}
		for _, mi := range list() {
			if old, ok := ids[mi.GetName()]; ok && mi.GetId() != 0 && mi.GetId() != old {
				return true
			}
		}
		return false
	})
	op, err = hcs.Update(ctx, &computepb.UpdateHealthCheckRequest{Project: project, HealthCheck: n("hc"), HealthCheckResource: hcOf(80)})
	do("the health check moved back to port 80", op, err)
	waitFor(t, "the group stable again, both VMs HEALTHY", 15*time.Minute, func() bool {
		mis := list()
		if len(mis) != 2 {
			return false
		}
		for _, mi := range mis {
			if _, ok := ids[mi.GetName()]; !ok || health(mi) != "HEALTHY" {
				return false
			}
		}
		return stable()
	})
	// The initial delay patched.
	op, err = igms.Patch(ctx, &computepb.PatchInstanceGroupManagerRequest{Project: project, Zone: zone, InstanceGroupManager: n("web"),
		InstanceGroupManagerResource: &computepb.InstanceGroupManager{
			AutoHealingPolicies: []*computepb.InstanceGroupManagerAutoHealingPolicy{{HealthCheck: proto.String(hcLink), InitialDelaySec: proto.Int32(90)}}}})
	do("patching the initial delay", op, err)
	g, _ = igms.Get(ctx, &computepb.GetInstanceGroupManagerRequest{Project: project, Zone: zone, InstanceGroupManager: n("web")})
	if g.GetAutoHealingPolicies()[0].GetInitialDelaySec() != 90 {
		t.Fatalf("after patch: %v", g.GetAutoHealingPolicies())
	}
}

// TestGcloudComputeMIGAutohealing2225 (#2225): the pinned gcloud through
// gcloud-setup, egress to Google blocked, every command on a served API:
// health-checks create tcp, instance-groups managed create --health-check
// --initial-delay, describe (the policy), update --initial-delay (the
// health check kept), list-instances (instanceHealth), update
// --clear-autohealing, describe (none), delete.
func TestGcloudComputeMIGAutohealing2225(t *testing.T) {
	h := New(t)
	opts := computeOpts(t, h) // skips without CLOUDBURROW_TEST_COMPUTE
	gcloud := gcloudBinary(t)
	cli := os.Getenv(EnvCLI)
	if cli == "" {
		t.Skipf("%s is not set", EnvCLI)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	t.Cleanup(cancel)
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
	tpls, err := computeapi.NewInstanceTemplatesRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	const zone = "us-central1-a"
	sfx := time.Now().UnixNano() % 1e6
	tpl, hc, name := fmt.Sprintf("gah-t-%d", sfx), fmt.Sprintf("gah-hc-%d", sfx), fmt.Sprintf("gah-%d", sfx)
	groupTemplate(t, ctx, tpls, h.Project(), tpl, "")
	t.Cleanup(func() {
		_, _ = run("compute", "instance-groups", "managed", "delete", name, "--zone", zone)
		_, _ = run("compute", "health-checks", "delete", hc)
		c := context.Background()
		if op, err := tpls.Delete(c, &computepb.DeleteInstanceTemplateRequest{Project: h.Project(), InstanceTemplate: tpl}); err == nil {
			_ = op.Wait(c)
		}
	})
	gc("compute", "health-checks", "create", "tcp", hc, "--port", "80")
	gc("compute", "instance-groups", "managed", "create", name, "--zone", zone, "--template", tpl, "--size", "1",
		"--health-check", hc, "--initial-delay", "60")
	out := gc("compute", "instance-groups", "managed", "describe", name, "--zone", zone,
		"--format=value(autoHealingPolicies[0].healthCheck.basename(),autoHealingPolicies[0].initialDelaySec)")
	if f := strings.Fields(out); len(f) != 2 || f[0] != hc || f[1] != "60" {
		t.Fatalf("describe after create: %q", out)
	}
	gc("compute", "instance-groups", "managed", "update", name, "--zone", zone, "--initial-delay", "120")
	out = gc("compute", "instance-groups", "managed", "describe", name, "--zone", zone,
		"--format=value(autoHealingPolicies[0].healthCheck.basename(),autoHealingPolicies[0].initialDelaySec)")
	if f := strings.Fields(out); len(f) != 2 || f[0] != hc || f[1] != "120" {
		t.Fatalf("describe after update --initial-delay: %q", out)
	}
	waitFor(t, "list-instances with instanceHealth", 5*time.Minute, func() bool {
		out, err := run("compute", "instance-groups", "managed", "list-instances", name, "--zone", zone,
			"--format=value(instanceHealth[0].detailedHealthState)")
		return err == nil && strings.TrimSpace(out) != ""
	})
	gc("compute", "instance-groups", "managed", "update", name, "--zone", zone, "--clear-autohealing")
	if out := strings.TrimSpace(gc("compute", "instance-groups", "managed", "describe", name, "--zone", zone, "--format=value(autoHealingPolicies)")); out != "" {
		t.Fatalf("describe after --clear-autohealing: %q", out)
	}
	gc("compute", "instance-groups", "managed", "delete", name, "--zone", zone)
}
