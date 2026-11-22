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
	if got := vpcFieldNames(setup.Fields); got != "enable,sharing,projects" {
		t.Fatalf("Set up Shared VPC fields = %s", got)
	}
	if f := vpcField(setup.Fields, "sharing"); f == nil || f.UnavailableOptions["individual"] == "" ||
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
	if got := vpcActionIDs(p.ListActions(ctx, "xh")); got != "attach,disable" {
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
