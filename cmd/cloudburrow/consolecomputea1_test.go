package main

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// TestConsoleComputeA1 (#2139, #2140): Edit labels on a custom image and
// a machine image; the VM Edit's provisioning model, refused while the VM
// runs and changed once stopped; Create a disk's Regional location, the
// regional disk's row, page and attach; dual-stack subnets, network and
// VM in the forms and lists; IPv6 firewall ranges; Reserve external
// static IP address's Attached to.
func TestConsoleComputeA1(t *testing.T) {
	ctx := context.Background()
	s := computeVMConsoleFixture(t)
	const project = "a1"
	vms := computeInstancesProvider{s}
	if _, err := vms.Create(ctx, project, map[string]string{"name": "app", "zone": "us-central1-a", "machineType": "e2-small"}); err != nil {
		t.Fatal(err)
	}
	settle(t, s, project)

	// Labels on images and machine images.
	images, mis := computeImagesProvider{s}, computeMachineImagesProvider{s}
	if _, err := images.Create(ctx, project, map[string]string{"name": "img", "source": "Disk", "disk": "us-central1-a/app", "force": "true"}); err != nil {
		t.Fatal(err)
	}
	if _, err := mis.Create(ctx, project, map[string]string{"name": "mi", "source": "us-central1-a/app"}); err != nil {
		t.Fatal(err)
	}
	settleGlobal(t, s, project)
	if got := vpcActionIDs(images.DetailActions(ctx, project, []string{"img"})); got != "labels,deprecate,delete" {
		t.Fatalf("image actions = %s", got)
	}
	if got := vpcActionIDs(mis.DetailActions(ctx, project, []string{"mi"})); got != "labels,createvm" {
		t.Fatalf("machine image actions = %s", got)
	}
	if err := images.ActAt(ctx, project, []string{"img"}, "labels", map[string]string{"labels": `{"env":"prod"}`}); err != nil {
		t.Fatal(err)
	}
	if err := mis.ActAt(ctx, project, []string{"mi"}, "labels", map[string]string{"labels": `{"env":"dev","team":"a"}`}); err != nil {
		t.Fatal(err)
	}
	if err := mis.ActAt(ctx, project, []string{"mi"}, "labels", map[string]string{"labels": `{"Env":"dev"}`}); err == nil {
		t.Error("an uppercase label key was accepted")
	}
	settleGlobal(t, s, project)
	im, _ := s.call("GET", project, "global/images/img", nil, nil)
	mi, _ := s.call("GET", project, "global/machineImages/mi", nil, nil)
	if vmLabelsText(im) != "env: prod" || !strings.Contains(vmLabelsText(mi), "team: a") {
		t.Fatalf("labels: image %v, machine image %v", im["labels"], mi["labels"])
	}

	// The VM provisioning model, after create.
	d, _ := vms.Detail(ctx, project, []string{"us-central1-a", "app"})
	pm := vpcField(d.Edit.Fields, "provisioningModel")
	if pm == nil || pm.Default != "Standard" || pm.UnavailableOptions["Spot"] == "" || !strings.Contains(c2139Props(d), "VM provisioning model=Standard") {
		t.Fatalf("a running VM's provisioning model = %+v; %s", pm, c2139Props(d))
	}
	if err := vms.Edit(ctx, project, []string{"us-central1-a", "app"}, map[string]string{"provisioningModel": "Spot"}); err == nil {
		t.Error("a running VM became Spot")
	}
	if err := vms.ActAt(ctx, project, []string{"us-central1-a", "app"}, "stop", map[string]string{}); err != nil {
		t.Fatal(err)
	}
	settle(t, s, project)
	d, _ = vms.Detail(ctx, project, []string{"us-central1-a", "app"})
	if pm := vpcField(d.Edit.Fields, "provisioningModel"); len(pm.UnavailableOptions) != 0 {
		t.Fatalf("a stopped VM's provisioning model = %+v", pm)
	}
	if err := vms.Edit(ctx, project, []string{"us-central1-a", "app"}, map[string]string{"provisioningModel": "Spot", "terminationAction": "Delete"}); err != nil {
		t.Fatal(err)
	}
	d, _ = vms.Detail(ctx, project, []string{"us-central1-a", "app"})
	if p := c2139Props(d); !strings.Contains(p, "VM provisioning model=Spot") || !strings.Contains(p, "On VM termination=Delete") {
		t.Fatalf("after Edit: %s", p)
	}

	// Regional disks.
	disks := computeDisksProvider{s}
	if _, err := disks.Create(ctx, project, map[string]string{"name": "ha", "location": "Regional", "zone": "us-central1-a", "replicaZone": "us-central1-b",
		"sizeGb": "20", "type": "pd-ssd"}); err != nil {
		t.Fatal(err)
	}
	if _, err := disks.Create(ctx, project, map[string]string{"name": "bad", "location": "Regional", "zone": "us-central1-a", "replicaZone": "us-east1-b"}); err == nil {
		t.Error("a regional disk with a replica zone of another region was made")
	}
	settleRegion(t, s, project, "us-central1")
	dl, _ := disks.List(ctx, project)
	r := vpcRow(dl, "ha")
	if r == nil || r.Fields["Zone"] != "us-central1-a, us-central1-b" || strings.Join(r.Target, "/") != "us-central1/ha" || r.Fields["Type"] != "pd-ssd" {
		t.Fatalf("the regional disk's row = %+v", r)
	}
	dd, _ := disks.Detail(ctx, project, r.Target)
	if !strings.Contains(c2139Props(dd), "Replica zones=us-central1: us-central1-a, us-central1-b") {
		t.Fatalf("the regional disk's page = %s", c2139Props(dd))
	}
	if err := disks.ActAt(ctx, project, r.Target, "labels", map[string]string{"labels": `{"tier":"ha"}`}); err != nil {
		t.Fatal(err)
	}
	var attach []string
	for _, a := range vms.instanceActions(mustInstance(t, vms, project, "app"), true) {
		if a.ID == "attachdisk" && len(a.Fields) > 0 {
			attach = a.Fields[0].Options
		}
	}
	if !strings.Contains(strings.Join(attach, ","), "regions/us-central1/disks/ha") {
		t.Fatalf("attach options = %v", attach)
	}
	if err := vms.ActAt(ctx, project, []string{"us-central1-a", "app"}, "attachdisk", map[string]string{"disk": "regions/us-central1/disks/ha"}); err != nil {
		t.Fatal(err)
	}
	settle(t, s, project)
	dl, _ = disks.List(ctx, project)
	if r := vpcRow(dl, "ha"); r.Fields["In use by"] != "app" {
		t.Fatalf("ha after attach = %+v", r)
	}

	// IPv6: a ULA network, a dual-stack subnet, a dual-stack VM.
	nets := computeNetworksProvider{s}
	if _, err := nets.Create(ctx, project, map[string]string{"name": "six", "subnetMode": "Custom", "autoMtu": "true", "ulaIpv6": "true",
		"ulaAllocation": "Automatically", "subnetName": "six-a", "subnetRegion": "us-central1", "subnetRange": "10.60.0.0/24",
		"subnetStackType": "IPV4_IPV6", "subnetIpv6AccessType": "Internal", "routingMode": "Regional"}); err != nil {
		t.Fatal(err)
	}
	subs := computeSubnetsProvider{s}
	sl, _ := subs.List(ctx, project)
	sr := vpcRow(sl, "six-a")
	if sr == nil || sr.Fields["IP stack type"] != "IPv4 and IPv6 (dual-stack)" || !strings.HasPrefix(sr.Fields["Internal IPv6 ranges"], "fd2") ||
		sr.Fields["External IPv6 ranges"] != "—" {
		t.Fatalf("six-a's row = %+v", sr)
	}
	// Edit subnet: an IPv4 subnet made dual-stack, its access type then fixed.
	if _, err := s.call("POST", project, "regions/us-central1/subnetworks", nil, map[string]any{"name": "six-b", "network": "global/networks/six",
		"ipCidrRange": "10.61.0.0/24"}); err != nil {
		t.Fatal(err)
	}
	sub := []string{"six", "subnets", "us-central1", "six-b"}
	sd, _ := nets.Detail(ctx, project, sub)
	if f := vpcField(sd.Edit.Fields, "stackType"); f == nil || f.Immutable || f.Default != "IPV4_ONLY" {
		t.Fatalf("Edit subnet's IP stack type = %+v", f)
	}
	if err := nets.Edit(ctx, project, sub, map[string]string{"stackType": "IPV4_IPV6", "ipv6AccessType": "Internal", "ipCidrRange": "10.61.0.0/24"}); err != nil {
		t.Fatal(err)
	}
	sd, _ = nets.Detail(ctx, project, sub)
	if f := vpcField(sd.Edit.Fields, "ipv6AccessType"); f.Default != "Internal" || !f.Immutable || vpcField(sd.Edit.Fields, "stackType").Default != "IPV4_IPV6" {
		t.Fatalf("six-b after Edit = %+v", sd.Edit.Fields)
	}
	if f := vpcField(nets.createFieldsForTest(), "subnetStackType"); len(f.UnavailableOptions) != 0 {
		t.Errorf("a stack type is still drawn unavailable: %v", f.UnavailableOptions)
	}
	// IPv6 (single-stack): a subnet with no IPv4 range ("Add an IPv6-only
	// subnet"), whose Edit keeps its stack type, and an IPv6-only VM on it.
	if f := vpcField(nets.createFieldsForTest(), "subnetRange"); f.ShowWhen != "subnetStackType=IPV4_ONLY|IPV4_IPV6" {
		t.Errorf("IPv4 range shows for IPv6-only: %+v", f)
	}
	if f := vpcField(nets.createFieldsForTest(), "subnetIpv6AccessType"); f.ShowWhen != "subnetStackType=IPV4_IPV6|IPV6_ONLY" {
		t.Errorf("IPv6 access type = %+v", f)
	}
	if err := nets.ActAt(ctx, project, []string{"six"}, "addsubnet", map[string]string{"name": "six-c", "region": "us-central1",
		"stackType": "IPV6_ONLY", "ipv6AccessType": "Internal", "ipCidrRange": "", "secondaryRanges": "", "privateGoogleAccess": "Off", "flowLogs": "Off"}); err != nil {
		t.Fatal(err)
	}
	c6, err := s.call("GET", project, "regions/us-central1/subnetworks/six-c", nil, nil)
	if err != nil || vpcStr(c6, "stackType") != "IPV6_ONLY" || vpcStr(c6, "ipCidrRange") != "" || vpcStr(c6, "ipv6AccessType") != "INTERNAL" {
		t.Fatalf("six-c = %v, %v", c6, err)
	}
	cd, _ := nets.Detail(ctx, project, []string{"six", "subnets", "us-central1", "six-c"})
	if f := vpcField(cd.Edit.Fields, "stackType"); f == nil || !f.Immutable || f.Default != "IPV6_ONLY" || vpcField(cd.Edit.Fields, "ipCidrRange") != nil {
		t.Fatalf("Edit of an IPv6-only subnet = %+v", cd.Edit.Fields)
	}
	if f := vpcField(sd.Edit.Fields, "stackType"); slices.Contains(f.Options, "IPV6_ONLY") {
		t.Errorf("a dual-stack subnet's Edit offers IPv6-only: %v", f.Options)
	}
	if err := nets.Edit(ctx, project, sub, map[string]string{"stackType": "IPV6_ONLY", "ipCidrRange": "10.61.0.0/24"}); err == nil {
		t.Error("a dual-stack subnet was made IPv6-only")
	}
	if f := vpcField(vms.CreateFormFor(ctx, project), "stackType"); !slices.Contains(f.Options, "IPV6_ONLY") ||
		vpcField(vms.CreateFormFor(ctx, project), "internalIP").ShowWhen == "" {
		t.Errorf("Create an instance's IP stack type = %+v", f)
	}
	if _, err := vms.Create(ctx, project, map[string]string{"name": "v6only", "zone": "us-central1-a", "machineType": "e2-small",
		"network": "six", "subnetwork": "six-c", "stackType": "IPV6_ONLY", "externalIP": "Ephemeral", "internalIP": ""}); err != nil {
		t.Fatal(err)
	}
	settle(t, s, project)
	v6, _ := s.call("GET", project, "zones/us-central1-a/instances/v6only", nil, nil)
	if n := vmMaps(v6, "networkInterfaces"); len(n) != 1 || vpcStr(n[0], "stackType") != "IPV6_ONLY" || vpcStr(n[0], "networkIP") != "" ||
		len(vmMaps(n[0], "accessConfigs")) != 0 {
		t.Fatalf("v6only = %v", v6)
	}
	if _, err := vms.Create(ctx, project, map[string]string{"name": "v6vm", "zone": "us-central1-a", "machineType": "e2-small",
		"network": "six", "subnetwork": "six-a", "stackType": "IPV4_IPV6"}); err != nil {
		t.Fatal(err)
	}
	settle(t, s, project)
	vd, _ := vms.Detail(ctx, project, []string{"us-central1-a", "v6vm"})
	var nicRow map[string]string
	for _, sec := range vd.Sections {
		if sec.ID == "networking" && len(sec.Listing.Items) > 0 {
			nicRow = sec.Listing.Items[0].Fields
		}
	}
	if nicRow["IP stack type"] != "IPv4 and IPv6 (dual-stack)" || !strings.HasPrefix(nicRow["Internal IPv6"], "fd2") {
		t.Fatalf("v6vm's network interface = %v", nicRow)
	}
	// An IPv6 firewall rule.
	fwp := computeFirewallsProvider{s}
	if _, err := fwp.Create(ctx, project, map[string]string{"name": "six-in", "network": "six", "priority": "1000", "direction": "Ingress",
		"action": "Allow", "targets": "All instances in the network", "sourceFilter": "IPv6 ranges", "sourceRanges6": "::/0", "protocolsMode": "Allow all"}); err != nil {
		t.Fatal(err)
	}
	fw, _ := s.call("GET", project, "global/firewalls/six-in", nil, nil)
	if strings.Join(vpcStrs(fw, "sourceRanges"), ",") != "::/0" {
		t.Fatalf("six-in = %v", fw)
	}

	// Reserve external static IP address, attached to app.
	addrs := computeAddressesProvider{s}
	var reserve []string
	for _, a := range addrs.ListActions(ctx, project) {
		if a.ID == "reserve-external" {
			reserve = vpcField(a.Fields, "attachTo").Options
		}
	}
	if strings.Join(reserve, ",") != "None,us-central1-a/app,us-central1-a/v6vm" {
		t.Fatalf("Attached to = %v", reserve)
	}
	if err := addrs.ActAt(ctx, project, nil, "reserve-external", map[string]string{"name": "app-ip", "ipType": "Regional", "region": "us-central1",
		"networkTier": "Premium", "attachTo": "us-central1-a/app"}); err != nil {
		t.Fatal(err)
	}
	a, _ := s.call("GET", project, "regions/us-central1/addresses/app-ip", nil, nil)
	if vpcStr(a, "status") != "IN_USE" || vmExternalIP(mustInstance(t, vms, project, "app")) != vpcStr(a, "address") {
		t.Fatalf("app-ip = %v", a)
	}
	el, _ := addrs.List(ctx, project)
	if r := vpcRow(el, "app-ip"); r == nil || r.Fields["In use by"] != "app" {
		t.Fatalf("app-ip's row = %+v", r)
	}

	// Create an instance with a reserved static external address.
	if _, err := s.call("POST", project, "regions/us-central1/addresses", nil, map[string]any{"name": "free-ip"}); err != nil {
		t.Fatal(err)
	}
	free, _ := s.call("GET", project, "regions/us-central1/addresses/free-ip", nil, nil)
	ext := vpcField(vms.CreateFormFor(ctx, project), "externalIP")
	if strings.Join(ext.Options, ",") != "Ephemeral,None,us-central1/free-ip" || !strings.Contains(ext.OptionLabels["us-central1/free-ip"], vpcStr(free, "address")) {
		t.Fatalf("External IPv4 address = %v %v", ext.Options, ext.OptionLabels)
	}
	if _, err := vms.Create(ctx, project, map[string]string{"name": "fixed", "zone": "us-central1-a", "machineType": "e2-small", "externalIP": "us-central1/free-ip"}); err != nil {
		t.Fatal(err)
	}
	settle(t, s, project)
	if got := vmExternalIP(mustInstance(t, vms, project, "fixed")); got != vpcStr(free, "address") {
		t.Fatalf("fixed's external IP = %s, want %s", got, vpcStr(free, "address"))
	}

	// Reserve internal static IP address, IPv6, on the dual-stack subnet.
	for _, a := range addrs.ListActions(ctx, project) {
		if f := vpcField(a.Fields, "ipVersion"); a.ID == "reserve-internal" && len(f.UnavailableOptions) != 0 {
			t.Fatalf("IP version = %+v", f)
		}
	}
	if err := addrs.ActAt(ctx, project, nil, "reserve-internal", map[string]string{"name": "six-ip", "ipVersion": "IPv6", "network": "six",
		"region": "us-central1", "subnetwork": "six-a", "assign": "Assign automatically", "purpose": "Non-shared"}); err != nil {
		t.Fatal(err)
	}
	// Create route, IPv6, to the default internet gateway.
	routes := computeRoutesProvider{s}
	if f := vpcField(routes.CreateFormFor(ctx, project), "ipVersion"); len(f.UnavailableOptions) != 0 {
		t.Fatalf("Create route's IP version = %+v", f)
	}
	if _, err := routes.Create(ctx, project, map[string]string{"name": "six-out", "network": "six", "ipVersion": "IPv6", "destRange6": "2001:db8::/32",
		"priority": "1000", "nextHop": "Default internet gateway"}); err != nil {
		t.Fatal(err)
	}
	if r, err := s.call("GET", project, "global/routes/six-out", nil, nil); err != nil || vpcStr(r, "destRange") != "2001:db8::/32" {
		t.Fatalf("six-out = %v %v", r, err)
	}
	il, _ := computeInternalAddressesProvider{addrs}.List(ctx, project)
	if r := vpcRow(il, "six-ip"); r == nil || r.Fields["Version"] != "IPv6" || !strings.HasPrefix(r.Fields["IP address"], "fd2") || !strings.HasSuffix(r.Fields["IP address"], "/96") {
		t.Fatalf("six-ip's row = %+v", r)
	}
}

func mustInstance(t *testing.T, vms computeInstancesProvider, project, name string) map[string]any {
	t.Helper()
	o, err := vms.instance(project, []string{"us-central1-a", name})
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func (p computeNetworksProvider) createFieldsForTest() []console.Field {
	_, f := p.CreateForm()
	return f
}
