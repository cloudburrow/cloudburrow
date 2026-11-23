package compute

import (
	"strings"
	"testing"
	"time"
)

// TestNATDynamicPortsAndEndpointIndependentMapping2140 (#2140): a gateway
// with dynamic port allocation gives each VM minPortsPerVm ports, doubles
// them when the VM nears exhausting them (up to maxPortsPerVm), and halves
// them once it has used few for natShrinkAfter; getNatMappingInfo and the
// pod's table follow, and blocks never overlap. A gateway with
// Endpoint-Independent Mapping maps each internal IP:port to one NAT port
// of the VM's range whatever the destination.
func TestNATDynamicPortsAndEndpointIndependentMapping2140(t *testing.T) {
	now := time.Date(2026, 11, 22, 8, 30, 0, 0, time.UTC)
	s, err := New(Options{VMs: fakeVMs{free: 1 << 40}, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	const p, region, zone = "nat-dyn", "us-central1", "us-central1-a"
	must := func(method, path string, body obj) obj {
		t.Helper()
		out, err := s.Call(method, p, path, nil, body)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		return out
	}
	ip := map[string]string{}
	for _, name := range []string{"a", "b"} {
		must("POST", "zones/"+zone+"/instances", obj{"name": name, "machineType": "zones/" + zone + "/machineTypes/e2-small",
			"disks":             []any{obj{"boot": true, "initializeParams": obj{"sourceImage": "projects/debian-cloud/global/images/family/debian-12"}}},
			"networkInterfaces": []any{obj{"network": "global/networks/default"}}})
		s.mu.Lock()
		inst := s.res["projects/"+p+"/zones/"+zone+"/instances/"+name]
		inst["status"] = stRunning
		ip[name] = str(objOf(sliceOf(inst["networkInterfaces"])[0]), "networkIP")
		s.mu.Unlock()
	}
	must("POST", "regions/"+region+"/routers", obj{"name": "r", "network": "global/networks/default",
		"nats": []any{obj{"name": "nat", "natIpAllocateOption": "AUTO_ONLY", "sourceSubnetworkIpRangesToNat": "ALL_SUBNETWORKS_ALL_IP_RANGES",
			"enableDynamicPortAllocation": true, "minPortsPerVm": 32.0, "maxPortsPerVm": 128.0}}})
	ports := func() map[string][2]int {
		t.Helper()
		g := s.NATGateways()[0]
		if !g.Dynamic || g.EIM {
			t.Fatalf("gateway: %+v", g)
		}
		out := map[string][2]int{}
		for _, v := range g.VMs {
			out[v.IP] = [2]int{v.PortLo, v.PortHi}
		}
		// getNatMappingInfo agrees.
		mi := must("GET", "regions/"+region+"/routers/r/getNatMappingInfo", nil)
		for _, e := range sliceOf(mi["result"]) {
			m := objOf(sliceOf(objOf(e)["interfaceNatMappings"])[0])
			pr := out[str(m, "sourceVirtualIp")]
			if m["numTotalNatPorts"] != float64(pr[1]-pr[0]+1) || !strings.HasSuffix(sliceOf(m["natIpPortRanges"])[0].(string), ":"+itoa(pr[0])+"-"+itoa(pr[1])) {
				t.Fatalf("getNatMappingInfo %v, the pod %v", m, pr)
			}
		}
		return out
	}
	key := s.NATGateways()[0].Key
	if pr := ports(); pr[ip["a"]] != [2]int{1024, 1055} || pr[ip["b"]] != [2]int{1056, 1087} {
		t.Fatalf("initial: %v", pr)
	}
	// a nears exhausting its 32: 64, then 128, never past maxPortsPerVm.
	if !s.SetNATPortUsage(key, map[string]int{ip["a"]: 30}) {
		t.Fatal("no growth")
	}
	if pr := ports(); pr[ip["a"]][1]-pr[ip["a"]][0] != 63 || pr[ip["b"]] != [2]int{1056, 1087} {
		t.Fatalf("after doubling: %v", pr)
	}
	s.SetNATPortUsage(key, map[string]int{ip["a"]: 64})
	s.SetNATPortUsage(key, map[string]int{ip["a"]: 128})
	pr := ports()
	if pr[ip["a"]][1]-pr[ip["a"]][0] != 127 {
		t.Fatalf("at the maximum: %v", pr)
	}
	if a, b := pr[ip["a"]], pr[ip["b"]]; a[0] <= b[1] && b[0] <= a[1] {
		t.Fatalf("overlapping blocks: %v", pr)
	}
	// Low usage: halved only after natShrinkAfter, down to the minimum.
	s.SetNATPortUsage(key, map[string]int{ip["a"]: 2})
	if pr := ports(); pr[ip["a"]][1]-pr[ip["a"]][0] != 127 {
		t.Fatalf("shrunk at once: %v", pr)
	}
	now = now.Add(natShrinkAfter)
	s.SetNATPortUsage(key, map[string]int{ip["a"]: 2})
	if pr := ports(); pr[ip["a"]][1]-pr[ip["a"]][0] != 63 {
		t.Fatalf("not halved: %v", pr)
	}
	for i := 0; i < 4; i++ {
		now = now.Add(natShrinkAfter)
		s.SetNATPortUsage(key, map[string]int{})
	}
	if pr := ports(); pr[ip["a"]][1]-pr[ip["a"]][0] != 31 {
		t.Fatalf("below the minimum: %v", pr)
	}

	// Endpoint-Independent Mapping (static allocation only).
	r := must("GET", "regions/"+region+"/routers/r", nil)
	n := objOf(sliceOf(r["nats"])[0])
	delete(n, "maxPortsPerVm")
	n["enableDynamicPortAllocation"], n["enableEndpointIndependentMapping"], n["minPortsPerVm"] = false, true, 64.0
	must("PATCH", "regions/"+region+"/routers/r", obj{"nats": []any{n}, "fingerprint": r["fingerprint"]})
	g := s.NATGateways()[0]
	if !g.EIM || g.Dynamic {
		t.Fatalf("gateway: %+v", g)
	}
	nft := natNFT(g, nil)
	want := `ip saddr ` + g.VMs[0].IP + " meta l4proto { tcp, udp } snat ip to " + g.VMs[0].NatIP + " : jhash ip saddr . th sport mod 64 offset 1024"
	if !strings.Contains(nft, want) {
		t.Fatalf("no endpoint-independent mapping %q:\n%s", want, nft)
	}
	if s.SetNATPortUsage(g.Key, map[string]int{ip["a"]: 64}) {
		t.Fatal("a static gateway's ports changed")
	}
}
