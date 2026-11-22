package compute

import (
	"strings"
	"testing"
)

// callErr is a call that must fail with status code.
func callErr(t *testing.T, s *Server, code int, method, path string, body obj) {
	t.Helper()
	if _, err := s.Call(method, "p2139", path, nil, body); ErrorStatus(err) != code {
		t.Errorf("%s %s: got %v, want %d", method, path, err, code)
	}
}

// TestImageAndMachineImageLabels (#2139): setLabels on a custom image and
// a machine image under their labelFingerprint, and Google's label rules.
func TestImageAndMachineImageLabels(t *testing.T) {
	s, _, call := parityFixture(t)
	call("POST", "zones/us-central1-a/instances", nil, vmBody("lv", nil))
	call("POST", "global/images", nil, obj{"name": "img", "sourceDisk": "zones/us-central1-a/disks/lv", "forceCreate": true})
	call("POST", "global/machineImages", nil, obj{"name": "mi", "sourceInstance": "zones/us-central1-a/instances/lv", "labels": obj{"team": "a"}})
	for _, path := range []string{"global/images/img", "global/machineImages/mi"} {
		o := call("GET", path, nil, nil)
		fp := str(o, "labelFingerprint")
		if fp == "" {
			t.Fatalf("%s has no labelFingerprint: %v", path, o)
		}
		callErr(t, s, 412, "POST", path+"/setLabels", obj{"labels": obj{"env": "dev"}, "labelFingerprint": "stale"})
		callErr(t, s, 400, "POST", path+"/setLabels", obj{"labels": obj{"Env": "dev"}, "labelFingerprint": fp})
		callErr(t, s, 400, "POST", path+"/setLabels", obj{"labels": obj{"env": "Dev!"}, "labelFingerprint": fp})
		call("POST", path+"/setLabels", nil, obj{"labels": obj{"env": "dev", "ñu": "x"}, "labelFingerprint": fp})
		o = call("GET", path, nil, nil)
		if l, _ := o["labels"].(map[string]any); l["env"] != "dev" || str(o, "labelFingerprint") == fp {
			t.Errorf("%s after setLabels = %v", path, o)
		}
	}
	many := obj{}
	for i := 0; i < 65; i++ {
		many["k"+strings.Repeat("a", i%5)+string(rune('a'+i%26))+string(rune('a'+i/26))] = "v"
	}
	o := call("GET", "global/images/img", nil, nil)
	callErr(t, s, 400, "POST", "global/images/img/setLabels", obj{"labels": many, "labelFingerprint": o["labelFingerprint"]})
}

// TestProvisioningModelAfterCreate (#2139): setScheduling changes Standard
// to Spot only while the VM is stopped.
func TestProvisioningModelAfterCreate(t *testing.T) {
	s, _, call := parityFixture(t)
	call("POST", "zones/us-central1-a/instances", nil, vmBody("pm", nil))
	callErr(t, s, 400, "POST", "zones/us-central1-a/instances/pm/setScheduling", obj{"provisioningModel": "SPOT"})
	call("POST", "zones/us-central1-a/instances/pm/stop", nil, nil)
	call("POST", "zones/us-central1-a/instances/pm/setScheduling", nil, obj{"provisioningModel": "SPOT", "instanceTerminationAction": "DELETE",
		"automaticRestart": false, "onHostMaintenance": "TERMINATE"})
	sch := call("GET", "zones/us-central1-a/instances/pm", nil, nil)["scheduling"].(map[string]any)
	if sch["provisioningModel"] != "SPOT" || sch["instanceTerminationAction"] != "DELETE" || sch["automaticRestart"] != false {
		t.Fatalf("scheduling = %v", sch)
	}
	call("POST", "zones/us-central1-a/instances/pm/setScheduling", nil, obj{"provisioningModel": "STANDARD", "automaticRestart": true, "onHostMaintenance": "MIGRATE"})
	sch = call("GET", "zones/us-central1-a/instances/pm", nil, nil)["scheduling"].(map[string]any)
	if sch["provisioningModel"] != "STANDARD" || sch["instanceTerminationAction"] != nil {
		t.Fatalf("back to standard = %v", sch)
	}
}

func natOf(o obj) (string, string) {
	nic := listOf(o["networkInterfaces"])[0].(map[string]any)
	acs := listOf(nic["accessConfigs"])
	if len(acs) == 0 {
		return "", ""
	}
	ac := acs[0].(map[string]any)
	return str(ac, "natIP"), str(ac, "networkTier")
}

var withNat = obj{"networkInterfaces": []any{map[string]any{"network": "global/networks/default",
	"accessConfigs": []any{map[string]any{"name": "External NAT", "type": "ONE_TO_ONE_NAT"}}}}}

// TestExternalAddresses (#2140): distinct ephemeral natIPs, promoting one
// to static, assigning a reserved address at insert and with
// add/deleteAccessConfig, delete refused while IN_USE, and the release
// to RESERVED when the VM goes.
func TestExternalAddresses(t *testing.T) {
	s, _, call := parityFixture(t)
	call("POST", "zones/us-central1-a/instances", nil, vmBody("e1", withNat))
	call("POST", "zones/us-central1-a/instances", nil, vmBody("e2", withNat))
	ip1, tier := natOf(call("GET", "zones/us-central1-a/instances/e1", nil, nil))
	ip2, _ := natOf(call("GET", "zones/us-central1-a/instances/e2", nil, nil))
	if !strings.HasPrefix(ip1, "192.0.2.") || !strings.HasPrefix(ip2, "192.0.2.") || ip1 == ip2 || tier != "PREMIUM" {
		t.Fatalf("ephemeral natIPs %q %q (%s)", ip1, ip2, tier)
	}
	if d := call("GET", "zones/us-central1-a/instances/e1", nil, nil); str(d, "cloudburrowSshAddress") != "127.0.0.1:2222" {
		t.Errorf("the SSH address is not kept apart from the natIP: %v", d["cloudburrowSshAddress"])
	}
	// Promote e1's.
	callErr(t, s, 400, "POST", "regions/us-east1/addresses", obj{"name": "far", "address": ip1})
	callErr(t, s, 400, "POST", "regions/us-central1/addresses", obj{"name": "std", "address": ip1, "networkTier": "STANDARD"})
	call("POST", "regions/us-central1/addresses", nil, obj{"name": "e1-ip", "address": ip1})
	a := call("GET", "regions/us-central1/addresses/e1-ip", nil, nil)
	if a["address"] != ip1 || a["status"] != "IN_USE" || a["addressType"] != "EXTERNAL" || a["networkTier"] != "PREMIUM" ||
		!strings.HasSuffix(strs(a, "users")[0], "/instances/e1") {
		t.Fatalf("promoted = %v", a)
	}
	callErr(t, s, 400, "DELETE", "regions/us-central1/addresses/e1-ip", nil)
	callErr(t, s, 501, "POST", "regions/us-central1/addresses", obj{"name": "byoip", "address": "8.8.8.8"})
	// A reserved address given at insert.
	call("POST", "regions/us-central1/addresses", nil, obj{"name": "fixed"})
	fixed := str(call("GET", "regions/us-central1/addresses/fixed", nil, nil), "address")
	callErr(t, s, 400, "POST", "zones/us-central1-a/instances", vmBody("bad", obj{"networkInterfaces": []any{map[string]any{"network": "global/networks/default",
		"accessConfigs": []any{map[string]any{"natIP": "203.0.113.250"}}}}}))
	call("POST", "zones/us-central1-a/instances", nil, vmBody("e3", obj{"networkInterfaces": []any{map[string]any{"network": "global/networks/default",
		"accessConfigs": []any{map[string]any{"natIP": fixed}}}}}))
	if ip, _ := natOf(call("GET", "zones/us-central1-a/instances/e3", nil, nil)); ip != fixed {
		t.Fatalf("e3's natIP = %s, want %s", ip, fixed)
	}
	if a := call("GET", "regions/us-central1/addresses/fixed", nil, nil); a["status"] != "IN_USE" {
		t.Fatalf("fixed = %v", a)
	}
	callErr(t, s, 400, "POST", "zones/us-central1-a/instances", vmBody("e4", obj{"networkInterfaces": []any{map[string]any{"network": "global/networks/default",
		"accessConfigs": []any{map[string]any{"natIP": fixed}}}}}))
	// deleteAccessConfig releases it; addAccessConfig gives it to e2.
	call("POST", "zones/us-central1-a/instances/e3/deleteAccessConfig", map[string][]string{"networkInterface": {"nic0"}, "accessConfig": {"External NAT"}}, nil)
	if a := call("GET", "regions/us-central1/addresses/fixed", nil, nil); a["status"] != "RESERVED" || a["users"] != nil {
		t.Fatalf("fixed after deleteAccessConfig = %v", a)
	}
	callErr(t, s, 400, "POST", "zones/us-central1-a/instances/e2/addAccessConfig", obj{"natIP": fixed})
	call("POST", "zones/us-central1-a/instances/e2/deleteAccessConfig", map[string][]string{"networkInterface": {"nic0"}, "accessConfig": {"External NAT"}}, nil)
	if _, err := s.Call("POST", "p2139", "zones/us-central1-a/instances/e2/addAccessConfig", map[string][]string{"networkInterface": {"nic0"}}, obj{"natIP": fixed}); err != nil {
		t.Fatal(err)
	}
	if ip, _ := natOf(call("GET", "zones/us-central1-a/instances/e2", nil, nil)); ip != fixed {
		t.Fatalf("e2's natIP = %s", ip)
	}
	// Deleting the VM releases its static address.
	call("DELETE", "zones/us-central1-a/instances/e1", nil, nil)
	if a := call("GET", "regions/us-central1/addresses/e1-ip", nil, nil); a["status"] != "RESERVED" {
		t.Fatalf("e1-ip after its VM's delete = %v", a)
	}
	call("DELETE", "regions/us-central1/addresses/e1-ip", nil, nil)
}

// TestRegionalDisks (#2139): replica zones' rules, types and minimum
// sizes, a VM of either replica zone attaching it, one of another zone
// refused, its snapshot, resize, labels and delete.
func TestRegionalDisks(t *testing.T) {
	s, _, call := parityFixture(t)
	rz := []any{"zones/us-central1-a", "zones/us-central1-b"}
	callErr(t, s, 400, "POST", "regions/us-central1/disks", obj{"name": "one", "replicaZones": []any{"zones/us-central1-a"}})
	callErr(t, s, 400, "POST", "regions/us-central1/disks", obj{"name": "far", "replicaZones": []any{"zones/us-central1-a", "zones/us-east1-b"}})
	callErr(t, s, 400, "POST", "regions/us-central1/disks", obj{"name": "same", "replicaZones": []any{"zones/us-central1-a", "zones/us-central1-a"}})
	callErr(t, s, 400, "POST", "regions/us-central1/disks", obj{"name": "ext", "replicaZones": rz, "type": "regions/us-central1/diskTypes/pd-extreme", "sizeGb": "500"})
	callErr(t, s, 400, "POST", "regions/us-central1/disks", obj{"name": "small", "replicaZones": rz, "type": "pd-standard", "sizeGb": "100"})
	call("POST", "regions/us-central1/disks", nil, obj{"name": "rd", "replicaZones": rz, "sizeGb": "20", "labels": obj{"tier": "ha"}})
	d := call("GET", "regions/us-central1/disks/rd", nil, nil)
	if d["status"] != "READY" || !strings.HasSuffix(str(d, "region"), "/regions/us-central1") || d["zone"] != nil ||
		!strings.HasSuffix(str(d, "type"), "/regions/us-central1/diskTypes/pd-balanced") || len(strs(d, "replicaZones")) != 2 {
		t.Fatalf("regional disk = %v", d)
	}
	if l := call("GET", "regions/us-central1/disks", nil, nil); len(listOf(l["items"])) != 1 {
		t.Fatalf("regionDisks.list = %v", l)
	}
	if agg := call("GET", "aggregated/disks", nil, nil); agg["items"].(map[string]any)["regions/us-central1"] == nil {
		t.Errorf("aggregated disks lack the regional disk: %v", agg)
	}
	if dt := call("GET", "regions/us-central1/diskTypes/pd-standard", nil, nil); dt["validDiskSize"] != "200GB-65536GB" {
		t.Errorf("regional pd-standard = %v", dt)
	}
	// Attach in replica zone b, at insert; another zone refused.
	call("POST", "zones/us-central1-b/instances", nil, vmBody("rb", obj{"machineType": "zones/us-central1-b/machineTypes/e2-small",
		"disks": []any{map[string]any{"boot": true, "autoDelete": true, "initializeParams": map[string]any{"sourceImage": "projects/debian-cloud/global/images/family/debian-12"}},
			map[string]any{"source": "regions/us-central1/disks/rd", "deviceName": "ha"}}}))
	s.mu.Lock()
	spec := s.vmSpecLocked(s.res["projects/p2139/zones/us-central1-b/instances/rb"])
	s.mu.Unlock()
	if len(spec.Disks) != 1 || spec.Disks[0].DeviceName != "ha" || !strings.HasPrefix(spec.Disks[0].Disk.Key, "disk-") {
		t.Fatalf("rb's disks = %+v", spec.Disks)
	}
	if d := call("GET", "regions/us-central1/disks/rd", nil, nil); !strings.HasSuffix(strs(d, "users")[0], "/instances/rb") {
		t.Fatalf("rd's users = %v", d["users"])
	}
	callErr(t, s, 400, "DELETE", "regions/us-central1/disks/rd", nil)
	call("POST", "zones/us-central1-b/instances/rb/detachDisk", map[string][]string{"deviceName": {"ha"}}, nil)
	call("POST", "zones/us-central1-c/instances", nil, vmBody("rc", obj{"machineType": "zones/us-central1-c/machineTypes/e2-small"}))
	callErr(t, s, 400, "POST", "zones/us-central1-c/instances/rc/attachDisk", obj{"source": "regions/us-central1/disks/rd"})
	call("POST", "zones/us-central1-a/instances", nil, vmBody("ra", nil))
	call("POST", "zones/us-central1-a/instances/ra/attachDisk", nil, obj{"source": "projects/p2139/regions/us-central1/disks/rd"})
	call("POST", "zones/us-central1-a/instances/ra/detachDisk", map[string][]string{"deviceName": {"rd"}}, nil)
	// Snapshot, resize, labels, delete.
	call("POST", "regions/us-central1/disks/rd/createSnapshot", nil, obj{"name": "rd-snap"})
	snap := call("GET", "global/snapshots/rd-snap", nil, nil)
	if strs(snap, "storageLocations")[0] != "us-central1" || !strings.HasSuffix(str(snap, "sourceDisk"), "/regions/us-central1/disks/rd") {
		t.Fatalf("snapshot of the regional disk = %v", snap)
	}
	callErr(t, s, 400, "POST", "regions/us-central1/disks/rd/resize", obj{"sizeGb": "10"})
	call("POST", "regions/us-central1/disks/rd/resize", nil, obj{"sizeGb": "30"})
	d = call("GET", "regions/us-central1/disks/rd", nil, nil)
	call("POST", "regions/us-central1/disks/rd/setLabels", nil, obj{"labels": obj{"tier": "gold"}, "labelFingerprint": d["labelFingerprint"]})
	if d := call("GET", "regions/us-central1/disks/rd", nil, nil); d["sizeGb"] != "30" || d["labels"].(map[string]any)["tier"] != "gold" {
		t.Fatalf("after resize and setLabels = %v", d)
	}
	// A regional disk from the snapshot.
	call("POST", "regions/us-central1/disks", nil, obj{"name": "rd2", "replicaZones": rz, "sourceSnapshot": "global/snapshots/rd-snap"})
	call("DELETE", "regions/us-central1/disks/rd", nil, nil)
	callErr(t, s, 404, "GET", "regions/us-central1/disks/rd", nil)
}

// TestIPv6DualStack (#2140): a ULA network, internal and external
// dual-stack subnets, dual-stack VMs' addresses, the refusals, firewall
// rules of IPv6 ranges compiled for IPv6 packets, and the guests' IPv6
// segment configuration.
func TestIPv6DualStack(t *testing.T) {
	s, _, call := parityFixture(t)
	call("POST", "global/networks", nil, obj{"name": "v6", "autoCreateSubnetworks": false, "enableUlaInternalIpv6": true})
	nw := call("GET", "global/networks/v6", nil, nil)
	ula := str(nw, "internalIpv6Range")
	if nw["enableUlaInternalIpv6"] != true || !strings.HasPrefix(ula, "fd2") || !strings.HasSuffix(ula, "/48") {
		t.Fatalf("network = %v", nw)
	}
	callErr(t, s, 400, "POST", "global/networks", obj{"name": "v6b", "autoCreateSubnetworks": false, "enableUlaInternalIpv6": true, "internalIpv6Range": "fd00::/48"})
	callErr(t, s, 400, "POST", "global/networks", obj{"name": "v6c", "autoCreateSubnetworks": false, "enableUlaInternalIpv6": true, "internalIpv6Range": ula})
	call("POST", "global/networks", nil, obj{"name": "v4", "autoCreateSubnetworks": false})
	callErr(t, s, 400, "POST", "regions/us-central1/subnetworks", obj{"name": "x", "network": "global/networks/v4", "ipCidrRange": "10.5.0.0/24",
		"stackType": "IPV4_IPV6", "ipv6AccessType": "INTERNAL"})
	callErr(t, s, 400, "POST", "regions/us-central1/subnetworks", obj{"name": "x", "network": "global/networks/v6", "ipCidrRange": "10.5.0.0/24", "stackType": "IPV4_IPV6"})
	callErr(t, s, 501, "POST", "regions/us-central1/subnetworks", obj{"name": "x", "network": "global/networks/v6", "ipCidrRange": "10.5.0.0/24", "stackType": "IPV6_ONLY"})
	call("POST", "regions/us-central1/subnetworks", nil, obj{"name": "in6", "network": "global/networks/v6", "ipCidrRange": "10.6.0.0/24",
		"stackType": "IPV4_IPV6", "ipv6AccessType": "INTERNAL"})
	call("POST", "regions/us-central1/subnetworks", nil, obj{"name": "ex6", "network": "global/networks/v6", "ipCidrRange": "10.7.0.0/24",
		"stackType": "IPV4_IPV6", "ipv6AccessType": "EXTERNAL"})
	call("POST", "regions/us-central1/subnetworks", nil, obj{"name": "only4", "network": "global/networks/v6", "ipCidrRange": "10.8.0.0/24"})
	in6 := call("GET", "regions/us-central1/subnetworks/in6", nil, nil)
	ex6 := call("GET", "regions/us-central1/subnetworks/ex6", nil, nil)
	ulaPrefix := strings.TrimSuffix(ula, "::/48")
	if in6["stackType"] != "IPV4_IPV6" || !strings.HasPrefix(str(in6, "internalIpv6Prefix"), ulaPrefix) || !strings.HasSuffix(str(in6, "ipv6CidrRange"), "/64") {
		t.Fatalf("in6 = %v (ula %s)", in6, ula)
	}
	if ex6["ipv6AccessType"] != "EXTERNAL" || !strings.HasPrefix(str(ex6, "externalIpv6Prefix"), "2600:190") {
		t.Fatalf("ex6 = %v", ex6)
	}
	// Patch only4 to dual-stack, and back.
	o4 := call("GET", "regions/us-central1/subnetworks/only4", nil, nil)
	call("PATCH", "regions/us-central1/subnetworks/only4", nil, obj{"stackType": "IPV4_IPV6", "ipv6AccessType": "INTERNAL", "fingerprint": o4["fingerprint"]})
	o4 = call("GET", "regions/us-central1/subnetworks/only4", nil, nil)
	if o4["stackType"] != "IPV4_IPV6" || str(o4, "ipv6CidrRange") == "" || o4["ipv6CidrRange"] == in6["ipv6CidrRange"] {
		t.Fatalf("only4 patched = %v", o4)
	}
	nic := func(sub, st string) obj {
		return obj{"networkInterfaces": []any{map[string]any{"subnetwork": "regions/us-central1/subnetworks/" + sub, "stackType": st}}}
	}
	call("POST", "zones/us-central1-a/instances", nil, vmBody("a6", nic("in6", "IPV4_IPV6")))
	call("POST", "zones/us-central1-a/instances", nil, vmBody("b6", nic("in6", "IPV4_IPV6")))
	call("POST", "zones/us-central1-a/instances", nil, vmBody("x6", nic("ex6", "IPV4_IPV6")))
	call("POST", "regions/us-central1/subnetworks", nil, obj{"name": "plain", "network": "global/networks/v4", "ipCidrRange": "10.9.0.0/24"})
	callErr(t, s, 400, "POST", "zones/us-central1-a/instances", vmBody("n6", obj{"networkInterfaces": []any{map[string]any{
		"subnetwork": "regions/us-central1/subnetworks/plain", "stackType": "IPV4_IPV6"}}}))
	a6 := listOf(call("GET", "zones/us-central1-a/instances/a6", nil, nil)["networkInterfaces"])[0].(map[string]any)
	b6 := listOf(call("GET", "zones/us-central1-a/instances/b6", nil, nil)["networkInterfaces"])[0].(map[string]any)
	x6 := listOf(call("GET", "zones/us-central1-a/instances/x6", nil, nil)["networkInterfaces"])[0].(map[string]any)
	if a6["stackType"] != "IPV4_IPV6" || a6["ipv6AccessType"] != "INTERNAL" || a6["internalIpv6PrefixLength"] != 96.0 ||
		!strings.HasPrefix(str(a6, "ipv6Address"), strings.TrimSuffix(str(in6, "ipv6CidrRange"), "::/64")) || a6["ipv6Address"] == b6["ipv6Address"] {
		t.Fatalf("a6 %v, b6 %v", a6, b6)
	}
	acs := listOf(x6["ipv6AccessConfigs"])
	if len(acs) != 1 || acs[0].(map[string]any)["type"] != "DIRECT_IPV6" || acs[0].(map[string]any)["externalIpv6PrefixLength"] != 96.0 ||
		!strings.HasPrefix(str(acs[0].(map[string]any), "externalIpv6"), "2600:190") {
		t.Fatalf("x6 = %v", x6)
	}
	// The subnet cannot go back to IPv4 only while a VM holds an address.
	in6 = call("GET", "regions/us-central1/subnetworks/in6", nil, nil)
	callErr(t, s, 400, "PATCH", "regions/us-central1/subnetworks/in6", obj{"stackType": "IPV4_ONLY", "fingerprint": in6["fingerprint"]})
	callErr(t, s, 400, "PATCH", "global/networks/v6", obj{"enableUlaInternalIpv6": false})

	// Firewall rules of IPv6 ranges, and the guests' configuration.
	call("POST", "global/firewalls", nil, obj{"name": "v6-in", "network": "global/networks/v6", "sourceRanges": []any{str(in6, "ipv6CidrRange"), "10.6.0.0/24"},
		"allowed": []any{map[string]any{"IPProtocol": "tcp", "ports": []any{"8080"}}}})
	s.mu.Lock()
	inst := s.res["projects/p2139/zones/us-central1-a/instances/a6"]
	s.vpcConfigLocked(s.res["projects/p2139/zones/us-central1-a/instances/b6"], "10.244.0.9")
	s.res["projects/p2139/zones/us-central1-a/instances/b6"]["status"] = stRunning
	conf, ok := s.vpcConfigLocked(inst, "10.244.0.8")
	s.mu.Unlock()
	if !ok || !strings.Contains(conf, "addr6 "+str(a6, "ipv6Address")+"\n") || !strings.Contains(conf, "route6 "+str(in6, "ipv6CidrRange")+"\n") ||
		!strings.Contains(conf, "peer6 "+str(b6, "ipv6Address")+" "+macOf(str(b6, "networkIP"))+"\n") {
		t.Fatalf("a6's configuration:\n%s", conf)
	}
	if !strings.Contains(conf, "ip6 saddr { "+str(in6, "ipv6CidrRange")+" } meta l4proto tcp th dport { 8080 } accept") ||
		!strings.Contains(conf, "ip saddr { 10.6.0.0/24 } meta l4proto tcp th dport { 8080 } accept") {
		t.Fatalf("the IPv6 rule is not compiled per family:\n%s", conf)
	}
}

// TestStaticIPv6Addresses (#2140): a static internal IPv6 address of a
// dual-stack subnet and an external one, given to VMs as their interface's
// IPv6 address, IN_USE until the VM goes; the refusals.
func TestStaticIPv6Addresses(t *testing.T) {
	s, _, call := parityFixture(t)
	call("POST", "global/networks", nil, obj{"name": "v6", "autoCreateSubnetworks": false, "enableUlaInternalIpv6": true})
	call("POST", "regions/us-central1/subnetworks", nil, obj{"name": "in6", "network": "global/networks/v6", "ipCidrRange": "10.6.0.0/24",
		"stackType": "IPV4_IPV6", "ipv6AccessType": "INTERNAL"})
	call("POST", "regions/us-central1/subnetworks", nil, obj{"name": "ex6", "network": "global/networks/v6", "ipCidrRange": "10.7.0.0/24",
		"stackType": "IPV4_IPV6", "ipv6AccessType": "EXTERNAL"})
	call("POST", "regions/us-central1/subnetworks", nil, obj{"name": "v4", "network": "global/networks/v6", "ipCidrRange": "10.8.0.0/24"})
	callErr(t, s, 400, "POST", "regions/us-central1/addresses", obj{"name": "x", "addressType": "INTERNAL", "ipVersion": "IPV6", "subnetwork": "v4"})
	callErr(t, s, 400, "POST", "regions/us-central1/addresses", obj{"name": "x", "addressType": "EXTERNAL", "ipVersion": "IPV6", "subnetwork": "in6"})
	callErr(t, s, 501, "POST", "global/addresses", obj{"name": "x", "ipVersion": "IPV6"})
	call("POST", "regions/us-central1/addresses", nil, obj{"name": "i6", "addressType": "INTERNAL", "ipVersion": "IPV6", "subnetwork": "in6"})
	call("POST", "regions/us-central1/addresses", nil, obj{"name": "e6", "addressType": "EXTERNAL", "ipVersion": "IPV6", "subnetwork": "regions/us-central1/subnetworks/ex6"})
	i6 := call("GET", "regions/us-central1/addresses/i6", nil, nil)
	e6 := call("GET", "regions/us-central1/addresses/e6", nil, nil)
	sub := call("GET", "regions/us-central1/subnetworks/in6", nil, nil)
	if i6["ipVersion"] != "IPV6" || i6["prefixLength"] != 96.0 || i6["status"] != "RESERVED" ||
		!strings.HasPrefix(str(i6, "address"), strings.TrimSuffix(str(sub, "ipv6CidrRange"), "::/64")) {
		t.Fatalf("i6 = %v", i6)
	}
	if e6["networkTier"] != "PREMIUM" || !strings.HasPrefix(str(e6, "address"), "2600:190") {
		t.Fatalf("e6 = %v", e6)
	}
	callErr(t, s, 400, "POST", "regions/us-central1/addresses", obj{"name": "dup", "addressType": "INTERNAL", "ipVersion": "IPV6", "subnetwork": "in6", "address": i6["address"]})
	call("POST", "zones/us-central1-a/instances", nil, vmBody("a", obj{"networkInterfaces": []any{map[string]any{
		"subnetwork": "regions/us-central1/subnetworks/in6", "stackType": "IPV4_IPV6", "ipv6Address": i6["address"]}}}))
	call("POST", "zones/us-central1-a/instances", nil, vmBody("b", obj{"networkInterfaces": []any{map[string]any{
		"subnetwork": "regions/us-central1/subnetworks/ex6", "stackType": "IPV4_IPV6",
		"ipv6AccessConfigs": []any{map[string]any{"type": "DIRECT_IPV6", "externalIpv6": e6["address"]}}}}}))
	callErr(t, s, 400, "POST", "zones/us-central1-a/instances", vmBody("c", obj{"networkInterfaces": []any{map[string]any{
		"subnetwork": "regions/us-central1/subnetworks/in6", "stackType": "IPV4_IPV6", "ipv6Address": i6["address"]}}}))
	a := listOf(call("GET", "zones/us-central1-a/instances/a", nil, nil)["networkInterfaces"])[0].(map[string]any)
	b := listOf(call("GET", "zones/us-central1-a/instances/b", nil, nil)["networkInterfaces"])[0].(map[string]any)
	if a["ipv6Address"] != i6["address"] || listOf(b["ipv6AccessConfigs"])[0].(map[string]any)["externalIpv6"] != e6["address"] {
		t.Fatalf("a %v, b %v", a, b)
	}
	for _, n := range []string{"i6", "e6"} {
		if o := call("GET", "regions/us-central1/addresses/"+n, nil, nil); o["status"] != "IN_USE" {
			t.Fatalf("%s = %v", n, o)
		}
	}
	callErr(t, s, 400, "DELETE", "regions/us-central1/addresses/i6", nil)
	call("DELETE", "zones/us-central1-a/instances/a", nil, nil)
	if o := call("GET", "regions/us-central1/addresses/i6", nil, nil); o["status"] != "RESERVED" {
		t.Fatalf("i6 after its VM's delete = %v", o)
	}
}
