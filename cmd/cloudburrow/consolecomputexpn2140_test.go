package main

import (
	"context"
	"strings"
	"testing"
)

// TestConsoleSharedVPC2140: Set up Shared VPC on a project (host, with a
// service project attached), the host's Attached projects and Subnets in
// host project tabs, Attach projects, the service project's view of the
// host's subnets, Detach and Disable Shared VPC.
func TestConsoleSharedVPC2140(t *testing.T) {
	ctx := context.Background()
	screen := computeVMConsoleFixture(t)
	p := computeSharedVPCProvider{screen}
	if p.ID() != "vpc-sharedvpc" || p.Title() != "Shared VPC" {
		t.Fatalf("screen %s %s", p.ID(), p.Title())
	}
	if got := vpcActionIDs(p.ListActions(ctx, "xh")); got != "setup" {
		t.Fatalf("a plain project's actions = %s", got)
	}
	setup := p.ListActions(ctx, "xh")[0]
	if got := vpcFieldNames(setup.Fields); !strings.HasPrefix(got, "enable,sharing,projects,") || !strings.Contains(got, ",subnet:us-central1/default,") || !strings.HasSuffix(got, ",members") {
		t.Fatalf("Set up Shared VPC fields = %s", got)
	}
	if f := vpcField(setup.Fields, "sharing"); f == nil || len(f.UnavailableOptions) != 0 || f.OptionLabels["individual"] != "Individual subnets (subnet-level permissions)" ||
		f.OptionLabels["all"] != "Share all subnets (project-level permissions)" {
		t.Fatalf("sharing mode = %+v", f)
	}
	if err := p.ActAt(ctx, "xh", nil, "setup", map[string]string{"sharing": "all", "projects": "xs1"}); err != nil {
		t.Fatal(err)
	}
	l, err := p.List(ctx, "xh")
	if err != nil {
		t.Fatal(err)
	}
	if l.ListTab != "attached" || len(l.Tabs) != 2 || l.Tabs[0].Label != "Subnets in host project" || len(l.Tabs[0].Groups) == 0 {
		t.Fatalf("host page tabs = %+v", l.Tabs)
	}
	if r := vpcRow(l, "xs1"); r == nil || r.Fields["Resources on shared subnets"] != "0" {
		t.Fatalf("attached projects = %+v", l.Items)
	}
	if got := vpcActionIDs(p.ListActions(ctx, "xh")); got != "attach,share,disable" {
		t.Fatalf("host actions = %s", got)
	}
	if err := p.ActAt(ctx, "xh", nil, "attach", map[string]string{"projects": "xs2, xs3"}); err != nil {
		t.Fatal(err)
	}
	if err := p.ActAt(ctx, "xh", nil, "attach", map[string]string{"projects": "xh"}); err == nil {
		t.Fatal("the host attached to itself")
	}
	sl, _ := p.List(ctx, "xs2")
	if r := vpcRow(sl, "default"); r == nil || r.Fields["Project"] != "xh" || !strings.Contains(sl.Note, "xh") {
		t.Fatalf("service project's view = %+v %q", sl.Items, sl.Note)
	}
	if err := p.ActAt(ctx, "xh", nil, "disable", nil); err == nil || !strings.Contains(err.Error(), "still attached") {
		t.Fatalf("disable with attached projects: %v", err)
	}
	for _, s := range []string{"xs1", "xs2", "xs3"} {
		if err := p.ActAt(ctx, "xh", []string{s}, "detach", nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.ActAt(ctx, "xh", nil, "disable", nil); err != nil {
		t.Fatal(err)
	}
	if got := vpcActionIDs(p.ListActions(ctx, "xh")); got != "setup" {
		t.Fatalf("after disable = %s", got)
	}
}

// TestConsoleSharedVPCIndividualSubnets2140: Set up Shared VPC with
// Individual subnets (subnet-level permissions) grants Compute Network
// User on the chosen subnet only, through its IAM policy; the host page
// shows each subnet's Compute Network Users; Share subnets adds to them;
// the service project's view lists the host's subnets from listUsable;
// a choice without subnets or principals is refused before anything is
// changed.
func TestConsoleSharedVPCIndividualSubnets2140(t *testing.T) {
	ctx := context.Background()
	screen := computeVMConsoleFixture(t)
	p := computeSharedVPCProvider{screen}
	if err := p.ActAt(ctx, "xi", nil, "setup", map[string]string{"sharing": "individual", "projects": "xj"}); err == nil || !strings.Contains(err.Error(), "Subnets to share") {
		t.Fatalf("no subnet chosen: %v", err)
	}
	if role, _, _ := p.xpnRole("xi"); role != "" {
		t.Fatalf("a refused setup changed the project: %s", role)
	}
	if err := p.ActAt(ctx, "xi", nil, "setup", map[string]string{"sharing": "individual", "projects": "xj", "subnet:us-central1/default": "true"}); err == nil || !strings.Contains(err.Error(), "Compute Network Users") {
		t.Fatalf("no principal: %v", err)
	}
	if err := p.ActAt(ctx, "xi", nil, "setup", map[string]string{"sharing": "individual", "projects": "xj",
		"subnet:us-central1/default": "true", "members": "user:dev@example.com"}); err != nil {
		t.Fatal(err)
	}
	pol, err := p.call("GET", "xi", "regions/us-central1/subnetworks/default/getIamPolicy", nil, nil)
	if err != nil || !strings.Contains(asString(pol), `"roles/compute.networkUser"`) || !strings.Contains(asString(pol), "user:dev@example.com") {
		t.Fatalf("the subnet's policy = %v %v", pol, err)
	}
	other, _ := p.call("GET", "xi", "regions/europe-west1/subnetworks/default/getIamPolicy", nil, nil)
	if strings.Contains(asString(other), "dev@example.com") {
		t.Fatalf("a subnet not chosen was shared: %v", other)
	}
	users := func(region string) string {
		l, err := p.List(ctx, "xi")
		if err != nil {
			t.Fatal(err)
		}
		for _, g := range l.Tabs[0].Groups {
			if g.Heading == "default / default" && g.Properties[0].Value == region {
				for _, pr := range g.Properties {
					if pr.Label == "Compute Network Users on this subnet" {
						return pr.Value
					}
				}
			}
		}
		return "?"
	}
	if got := users("us-central1"); got != "user:dev@example.com" {
		t.Fatalf("us-central1's users = %q", got)
	}
	if got := users("europe-west1"); got != "—" && got != "-" {
		t.Fatalf("europe-west1's users = %q", got)
	}
	if err := p.ActAt(ctx, "xi", nil, "share", map[string]string{"subnet:us-central1/default": "true", "members": "user:dev@example.com, group:ops@example.com"}); err != nil {
		t.Fatal(err)
	}
	if got := users("us-central1"); got != "group:ops@example.com, user:dev@example.com" {
		t.Fatalf("after Share subnets = %q", got)
	}
	sl, _ := p.List(ctx, "xj")
	if r := vpcRow(sl, "default"); r == nil || r.Fields["Project"] != "xi" {
		t.Fatalf("service project's view = %+v", sl.Items)
	}
	for _, it := range sl.Items {
		if it.Fields["Project"] != "xi" {
			t.Fatalf("the service project's own subnet listed as shared: %+v", it)
		}
	}
}
