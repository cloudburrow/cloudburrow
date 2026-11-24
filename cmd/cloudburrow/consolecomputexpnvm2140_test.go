package main

import (
	"context"
	"strings"
	"testing"
)

// TestConsoleCreateInstanceInServiceProject2140: in a Shared VPC service
// project, Create an instance's Networking offers "Networks in this
// project" or "Networks shared with me (from host project: H)" with the
// host's subnets; an instance made on a shared subnet has the host's
// network and subnet on its interface; a subnet of another region is
// refused; a plain project's form has no such choice.
func TestConsoleCreateInstanceInServiceProject2140(t *testing.T) {
	ctx := context.Background()
	s := computeVMConsoleFixture(t)
	vms, xpn := computeInstancesProvider{s}, computeSharedVPCProvider{s}
	if f := vpcField(vms.CreateFormFor(ctx, "xvh-plain"), "networkSource"); f != nil {
		t.Fatalf("a plain project's form has %+v", f)
	}
	if err := xpn.ActAt(ctx, "xvh", nil, "setup", map[string]string{"sharing": "all", "projects": "xvs"}); err != nil {
		t.Fatal(err)
	}
	fields := vms.CreateFormFor(ctx, "xvs")
	names := vpcFieldNames(fields)
	if !strings.Contains(names, "networkSource,network,subnetwork,sharedSubnetwork") {
		t.Fatalf("service project's Networking fields = %s", names)
	}
	src := vpcField(fields, "networkSource")
	if src.OptionLabels[vmNetShared] != "Networks shared with me (from host project: xvh)" || src.OptionLabels[vmNetLocal] != "Networks in this project" {
		t.Fatalf("Network interfaces = %+v", src)
	}
	shared := vpcField(fields, "sharedSubnetwork")
	if shared.VisibleWhen["networkSource"] != vmNetShared || !strings.Contains(strings.Join(shared.Options, ","), "us-central1/default") {
		t.Fatalf("Shared subnetwork = %+v", shared)
	}
	if vpcField(fields, "network").VisibleWhen["networkSource"] != vmNetLocal {
		t.Fatal("the project's own Network shows for shared networks too")
	}
	base := map[string]string{"zone": "us-central1-a", "machineType": "e2-small", "image": "debian-cloud/debian-12", "bootDiskSize": "10",
		"network": "default", "externalIP": "None", "networkSource": vmNetShared}
	vals := func(name, sub string) map[string]string {
		m := map[string]string{"name": name, "sharedSubnetwork": sub}
		for k, v := range base {
			m[k] = v
		}
		return m
	}
	if _, err := vms.Create(ctx, "xvs", vals("wrong-region", "europe-west1/default")); err == nil || !strings.Contains(err.Error(), "not the instance's region") {
		t.Fatalf("a subnet of another region: %v", err)
	}
	if _, err := vms.Create(ctx, "xvs", vals("on-host", "us-central1/default")); err != nil {
		t.Fatal(err)
	}
	settle(t, s, "xvs")
	o, err := s.call("GET", "xvs", "zones/us-central1-a/instances/on-host", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	nic := vmMaps(o, "networkInterfaces")[0]
	if !strings.HasSuffix(vpcStr(nic, "network"), "projects/xvh/global/networks/default") ||
		!strings.HasSuffix(vpcStr(nic, "subnetwork"), "projects/xvh/regions/us-central1/subnetworks/default") {
		t.Fatalf("the instance's interface = %v", nic)
	}
}
