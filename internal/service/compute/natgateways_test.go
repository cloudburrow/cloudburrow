package compute

import (
	"strings"
	"testing"
)

// TestNATGatewaysTranslateTheirVMs (#2140): a public Cloud NAT gateway that
// translates VMs is run as a pod at the subnet's gateway address; each VM
// without an external IP is given minPortsPerVm ports of a NAT IP from
// 1024, the same that getNatMappingInfo reports, which the pod's table
// translates it to; once the pod has polled, the VM routes the internet
// to it and keeps private ranges on its pod interface; a VM with an
// external IP is not translated. AUTO_ONLY gains addresses as its VMs need
// them; MANUAL_ONLY's VMs past its addresses get none, are dropped and
// noted, and minExtraNatIpsNeeded and getNatIpInfo say so.
func TestNATGatewaysTranslateTheirVMs(t *testing.T) {
	s, _ := newTestServer(t, Options{VMs: fakeVMs{}})
	const p, region, zone = "nat-p", "us-central1", "us-central1-a"
	must := func(method, path string, body obj) obj {
		t.Helper()
		out, err := s.Call(method, p, path, nil, body)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		return out
	}
	vm := func(name string, external bool) obj {
		nic := obj{"network": "global/networks/default"}
		if external {
			nic["accessConfigs"] = []any{obj{"name": "External NAT", "type": "ONE_TO_ONE_NAT"}}
		}
		must("POST", "zones/"+zone+"/instances", obj{"name": name, "machineType": "zones/" + zone + "/machineTypes/e2-small",
			"disks":             []any{obj{"boot": true, "initializeParams": obj{"sourceImage": "projects/debian-cloud/global/images/family/debian-12"}}},
			"networkInterfaces": []any{nic}})
		// Running now, not when the fake runtime gets to it.
		s.mu.Lock()
		defer s.mu.Unlock()
		inst := s.res["projects/"+p+"/zones/"+zone+"/instances/"+name]
		inst["status"] = stRunning
		return inst
	}
	a, b, ext := vm("a", false), vm("b", false), vm("ext", true)
	conf := func(inst obj, pod string) string {
		s.mu.Lock()
		defer s.mu.Unlock()
		c, _ := s.vpcConfigLocked(inst, pod)
		return c
	}
	conf(a, "10.244.0.11")
	conf(b, "10.244.0.12")
	conf(ext, "10.244.0.13")
	if gws := s.NATGateways(); len(gws) != 0 {
		t.Fatalf("gateways without a router: %v", gws)
	}
	must("POST", "regions/"+region+"/routers", obj{"name": "r", "network": "global/networks/default",
		"nats": []any{obj{"name": "nat", "natIpAllocateOption": "AUTO_ONLY", "sourceSubnetworkIpRangesToNat": "ALL_SUBNETWORKS_ALL_IP_RANGES",
			"minPortsPerVm": 64.0, "udpIdleTimeoutSec": 45.0, "logConfig": obj{"enable": true, "filter": "ALL"}}}})
	gws := s.NATGateways()
	if len(gws) != 1 {
		t.Fatalf("gateways: %+v", gws)
	}
	g := gws[0]
	sub := s.res["projects/"+p+"/regions/"+region+"/subnetworks/default"]
	if g.IP != str(sub, "gatewayAddress") || g.Pod != "" || !g.Log || g.LogFilter != "ALL" || len(g.NatIPs) != 1 || len(g.VMs) != 2 {
		t.Fatalf("gateway: %+v", g)
	}
	ipA, ipB := str(sliceOf(a["networkInterfaces"])[0].(map[string]any), "networkIP"), str(sliceOf(b["networkInterfaces"])[0].(map[string]any), "networkIP")
	if v := g.VMs[0]; v.IP != ipA || v.NatIP != g.NatIPs[0] || v.PortLo != 1024 || v.PortHi != 1087 {
		t.Fatalf("a's ports: %+v", v)
	}
	if v := g.VMs[1]; v.IP != ipB || v.PortLo != 1088 || v.PortHi != 1151 {
		t.Fatalf("b's ports: %+v", v)
	}
	mi := must("GET", "regions/"+region+"/routers/r/getNatMappingInfo", nil)
	if r := sliceOf(objOf(sliceOf(objOf(sliceOf(mi["result"])[1])["interfaceNatMappings"])[0])["natIpPortRanges"]); len(r) != 1 || r[0] != g.NatIPs[0]+":1088-1151" {
		t.Fatalf("getNatMappingInfo and the pod disagree: %v", mi)
	}
	// No pod yet: the VMs keep their own route.
	if c := conf(a, "10.244.0.11"); strings.Contains(c, "\nnat ") {
		t.Fatalf("routed to a pod that has not polled:\n%s", c)
	}
	// The pod polls.
	s.mu.Lock()
	pc, ok := s.vpcConfigLocked(obj{"selfLink": natRel(g.Key), "name": "nat", "zone": zone, "status": stRunning, "_natGateway": g.Key,
		"networkInterfaces": []any{obj{"network": g.Network, "networkIP": g.IP, "accessConfigs": []any{}}}}, "10.244.0.20")
	s.mu.Unlock()
	if !ok {
		t.Fatal("no pod configuration")
	}
	for _, want := range []string{"addr " + g.IP + "\n", "forward\n", "sysctl nf_conntrack_udp_timeout 45\n", "sysctl nf_conntrack_tcp_timeout_established 1200\n",
		"peer " + ipA + " " + macOf(ipA) + " 10.244.0.11\n",
		`oifname "eth0" ip saddr ` + ipA + " meta l4proto { tcp, udp } snat ip to " + g.NatIPs[0] + ":1024-1087",
		`oifname "eth0" ip saddr ` + ipB + " meta l4proto { tcp, udp } snat ip to " + g.NatIPs[0] + ":1088-1151",
		`iifname "vpc0" oifname "eth0" ip saddr { ` + ipA + ", " + ipB + ` } accept`} {
		if !strings.Contains(pc, want) {
			t.Fatalf("the pod's configuration lacks %q:\n%s", want, pc)
		}
	}
	if strings.Contains(pc, "chain services") || strings.Contains(pc, "ip saddr 10.244.0.13") {
		t.Fatalf("the pod translates what it should not:\n%s", pc)
	}
	if gws := s.NATGateways(); gws[0].Pod != "10.244.0.20" {
		t.Fatalf("pod not recorded: %+v", gws[0])
	}
	c := conf(a, "10.244.0.11")
	for _, want := range []string{"nat " + g.IP + "\n", "natkeep 10.0.0.0/8\n", "natkeep 169.254.0.0/16\n", "natkeep 10.244.0.0/16\n",
		"peer " + g.IP + " " + macOf(g.IP) + " 10.244.0.20\n"} {
		if !strings.Contains(c, want) {
			t.Fatalf("a's configuration lacks %q:\n%s", want, c)
		}
	}
	if c := conf(ext, "10.244.0.13"); strings.Contains(c, "\nnat ") {
		t.Fatalf("a VM with an external IP is translated:\n%s", c)
	}

	// AUTO_ONLY at 32768 ports per VM: one address per VM.
	r := must("GET", "regions/"+region+"/routers/r", nil)
	n := objOf(sliceOf(r["nats"])[0])
	n["minPortsPerVm"] = 32768.0
	must("PATCH", "regions/"+region+"/routers/r", obj{"nats": []any{n}, "fingerprint": r["fingerprint"]})
	if g := s.NATGateways()[0]; len(g.NatIPs) != 2 || g.VMs[1].NatIP != g.NatIPs[1] || g.VMs[1].PortLo != 1024 || g.VMs[1].PortHi != 33791 {
		t.Fatalf("AUTO_ONLY did not grow: %+v", g)
	}
	st := must("GET", "regions/"+region+"/routers/r/getRouterStatus", nil)
	if ns := objOf(sliceOf(objOf(st["result"])["natStatus"])[0]); len(sliceOf(ns["autoAllocatedNatIps"])) != 2 || ns["minExtraNatIpsNeeded"] != 0.0 {
		t.Fatalf("status: %v", ns)
	}

	// MANUAL_ONLY with one address: b gets no ports.
	must("POST", "regions/"+region+"/addresses", obj{"name": "nat-ip"})
	r = must("GET", "regions/"+region+"/routers/r", nil)
	n = objOf(sliceOf(r["nats"])[0])
	delete(n, "_autoIps")
	n["natIpAllocateOption"], n["natIps"] = "MANUAL_ONLY", []any{"regions/" + region + "/addresses/nat-ip"}
	must("PATCH", "regions/"+region+"/routers/r", obj{"nats": []any{n}, "fingerprint": r["fingerprint"]})
	g = s.NATGateways()[0]
	if len(g.NatIPs) != 1 || g.VMs[0].NatIP == "" || g.VMs[1].NatIP != "" {
		t.Fatalf("MANUAL_ONLY: %+v", g)
	}
	st = must("GET", "regions/"+region+"/routers/r/getRouterStatus", nil)
	if ns := objOf(sliceOf(objOf(st["result"])["natStatus"])[0]); ns["minExtraNatIpsNeeded"] != 1.0 {
		t.Fatalf("minExtraNatIpsNeeded: %v", ns)
	}
	info := must("GET", "regions/"+region+"/routers/r/getNatIpInfo", nil)
	if m := objOf(sliceOf(objOf(sliceOf(info["result"])[0])["natIpInfoMappings"])[0]); m["usage"] != "IN_USE" || m["origin"] != "MANUAL" {
		t.Fatalf("getNatIpInfo: %v", info)
	}
	if nft := natNFT(g, nil); !strings.Contains(nft, "ip saddr { "+ipB+" } meta l4proto { tcp, udp } update @dropped") ||
		strings.Contains(nft, "ip saddr "+ipB+" meta l4proto { tcp, udp } snat") {
		t.Fatalf("b is not dropped:\n%s", nft)
	}
}
