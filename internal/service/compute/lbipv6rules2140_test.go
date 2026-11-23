package compute

import (
	"net"
	"strings"
	"testing"
)

// TestRegionalAndInternalIPv6ForwardingRules2140 (#2140): an internal
// passthrough Network Load Balancer's rule with ipVersion IPV6 is given a
// /96 of its dual-stack subnet's internal range (or the reserved internal
// IPv6 address named), and a backend service-based external passthrough
// one a /96 of an EXTERNAL subnet's range, in the Premium Tier; each is
// served on its listener; a subnet of the wrong IPv6 access type, a
// missing subnetwork and a target pool are refused. A dual-stack VM of the
// network reaches the internal rule's address through relay6, with the
// address on lo subject to its egress rules; an IPv4-only VM does not.
func TestRegionalAndInternalIPv6ForwardingRules2140(t *testing.T) {
	s, _ := newTestServer(t, Options{VMs: fakeVMs{}, GuestReach: func(a string) string {
		_, port, _ := strings.Cut(a, ":")
		return "192.168.65.254:" + port
	}})
	const p, region, zone = "v6rules", "us-central1", "us-central1-a"
	must := func(method, path string, body obj) obj {
		t.Helper()
		out, err := s.Call(method, p, path, nil, body)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		return out
	}
	fails := func(code int, path string, body obj) {
		t.Helper()
		if _, err := s.Call("POST", p, path, nil, body); ErrorStatus(err) != code {
			t.Fatalf("POST %s %v: %v, want %d", path, body, err, code)
		}
	}
	r := "regions/" + region + "/"
	must("POST", "global/networks", obj{"name": "v6", "autoCreateSubnetworks": false, "enableUlaInternalIpv6": true})
	must("POST", r+"subnetworks", obj{"name": "in6", "network": "global/networks/v6", "ipCidrRange": "10.6.0.0/24", "stackType": "IPV4_IPV6", "ipv6AccessType": "INTERNAL"})
	must("POST", r+"subnetworks", obj{"name": "ex6", "network": "global/networks/v6", "ipCidrRange": "10.7.0.0/24", "stackType": "IPV4_IPV6", "ipv6AccessType": "EXTERNAL"})
	must("POST", r+"healthChecks", obj{"name": "hc", "type": "TCP", "tcpHealthCheck": obj{"port": 80}})
	must("POST", r+"backendServices", obj{"name": "ilb", "protocol": "TCP", "loadBalancingScheme": "INTERNAL", "network": "global/networks/v6",
		"healthChecks": []any{"projects/" + p + "/" + r + "healthChecks/hc"}})
	must("POST", r+"backendServices", obj{"name": "xlb", "protocol": "TCP", "loadBalancingScheme": "EXTERNAL",
		"healthChecks": []any{"projects/" + p + "/" + r + "healthChecks/hc"}})
	ilbBody := func(name, sub string) obj {
		return obj{"name": name, "loadBalancingScheme": "INTERNAL", "IPProtocol": "TCP", "ports": []any{"80"}, "ipVersion": "IPV6",
			"network": "global/networks/v6", "subnetwork": sub, "backendService": "projects/" + p + "/" + r + "backendServices/ilb"}
	}
	fails(400, r+"forwardingRules", ilbBody("bad", r+"subnetworks/ex6"))
	fails(400, r+"forwardingRules", ilbBody("bad", ""))
	must("POST", r+"forwardingRules", ilbBody("ilb6", r+"subnetworks/in6"))
	ilb := must("GET", r+"forwardingRules/ilb6", nil)
	in6 := must("GET", r+"subnetworks/in6", nil)
	_, base, _ := net.ParseCIDR(str(in6, "ipv6CidrRange"))
	a, ok := strings.CutSuffix(str(ilb, "IPAddress"), "/96")
	if !ok || !base.Contains(net.ParseIP(a)) || ilb["ipVersion"] != "IPV6" || !strings.HasSuffix(str(ilb, "subnetwork"), "/subnetworks/in6") || len(strs(ilb, "cloudburrowAddresses")) != 1 {
		t.Fatalf("the internal IPv6 rule: %v", ilb)
	}
	// A reserved internal IPv6 address, by name.
	must("POST", r+"addresses", obj{"name": "v6vip", "addressType": "INTERNAL", "ipVersion": "IPV6", "subnetwork": r + "subnetworks/in6"})
	vip := must("GET", r+"addresses/v6vip", nil)
	b := ilbBody("ilb6b", "")
	b["IPAddress"], b["ports"] = "v6vip", []any{"8080"}
	must("POST", r+"forwardingRules", b)
	if rb := must("GET", r+"forwardingRules/ilb6b", nil); rb["IPAddress"] != str(vip, "address")+"/96" {
		t.Fatalf("the reserved address: %v", rb)
	}
	if v := must("GET", r+"addresses/v6vip", nil); v["status"] != "IN_USE" {
		t.Fatalf("the address not in use: %v", v)
	}
	// External, backend service-based, Premium; a target pool is IPv4 only.
	xb := obj{"name": "xlb6", "loadBalancingScheme": "EXTERNAL", "IPProtocol": "TCP", "portRange": "80", "ipVersion": "IPV6",
		"subnetwork": r + "subnetworks/ex6", "backendService": "projects/" + p + "/" + r + "backendServices/xlb"}
	xb["networkTier"] = "STANDARD"
	fails(400, r+"forwardingRules", xb)
	delete(xb, "networkTier")
	must("POST", r+"forwardingRules", xb)
	x := must("GET", r+"forwardingRules/xlb6", nil)
	ex6 := must("GET", r+"subnetworks/ex6", nil)
	_, xbase, _ := net.ParseCIDR(str(ex6, "ipv6CidrRange"))
	if xa, ok := strings.CutSuffix(str(x, "IPAddress"), "/96"); !ok || !xbase.Contains(net.ParseIP(xa)) || x["networkTier"] != "PREMIUM" {
		t.Fatalf("the external IPv6 rule: %v", x)
	}
	must("POST", r+"targetPools", obj{"name": "tp"})
	fails(400, r+"forwardingRules", obj{"name": "tp6", "IPProtocol": "TCP", "portRange": "80", "ipVersion": "IPV6",
		"subnetwork": r + "subnetworks/ex6", "target": "projects/" + p + "/" + r + "targetPools/tp"})

	// The VMs' data plane.
	vm := func(name, stack string) obj {
		must("POST", "zones/"+zone+"/instances", obj{"name": name, "machineType": "zones/" + zone + "/machineTypes/e2-small",
			"disks":             []any{obj{"boot": true, "initializeParams": obj{"sourceImage": "projects/debian-cloud/global/images/family/debian-12"}}},
			"networkInterfaces": []any{obj{"subnetwork": r + "subnetworks/in6", "stackType": stack}}})
		return s.res["projects/"+p+"/zones/"+zone+"/instances/"+name]
	}
	conf := func(inst obj) string {
		s.mu.Lock()
		defer s.mu.Unlock()
		c, _ := s.vpcConfigLocked(inst, "10.244.0.9")
		return c
	}
	listener := strings.TrimPrefix(strs(ilb, "cloudburrowAddresses")[0], "80=127.0.0.1:")
	dual, v4 := vm("dual", "IPV4_IPV6"), vm("v4", "IPV4_ONLY")
	c := conf(dual)
	for _, want := range []string{"relay6 " + a + " 80 192.168.65.254:" + listener + "\n", `oifname "lo" ip6 daddr != { ` + str(vip, "address") + ", " + a + " } accept",
		"relay6 " + str(vip, "address") + " 8080 "} {
		if !strings.Contains(c, want) && !strings.Contains(c, strings.Replace(want, str(vip, "address")+", "+a, a+", "+str(vip, "address"), 1)) {
			t.Fatalf("the dual-stack VM lacks %q:\n%s", want, c)
		}
	}
	if strings.Contains(c, "dnat ip to") && strings.Contains(c, "ip daddr "+a) {
		t.Fatalf("an IPv6 address in the IPv4 rewrite:\n%s", c)
	}
	if c := conf(v4); strings.Contains(c, "relay6 ") {
		t.Fatalf("an IPv4-only VM relays IPv6:\n%s", c)
	}
	must("POST", "global/firewalls", obj{"name": "no-ilb6", "network": "global/networks/v6", "direction": "EGRESS",
		"destinationRanges": []any{a + "/128"}, "denied": []any{obj{"IPProtocol": "tcp"}}, "priority": 100})
	if c := conf(dual); !strings.Contains(c, "ct original ip6 daddr { "+a+"/128 } meta l4proto tcp drop") {
		t.Fatalf("egress deny of the IPv6 rule:\n%s", c)
	}
}
