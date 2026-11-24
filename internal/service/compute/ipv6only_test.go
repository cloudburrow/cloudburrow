package compute

import (
	"net/http"
	"strings"
	"testing"
)

// TestIPv6OnlySubnetsAndInstances (#2140): an IPv6-only subnet (its /64,
// no IPv4 range, gateway or subnet route), its refusals (an IPv4 range,
// secondary ranges, expansion, stack type changes, auto mode networks),
// IPv6-only VMs on IPv6-only and dual-stack subnets (no networkIP, the
// Debian/Ubuntu rule), and the guests' configuration: no addr, a MAC of
// the IPv6 address, peer6 with the pod, DNS to the IPv6 address.
func TestIPv6OnlySubnetsAndInstances(t *testing.T) {
	s, _, call := parityFixture(t)
	call("POST", "global/networks", nil, obj{"name": "v6", "autoCreateSubnetworks": false, "enableUlaInternalIpv6": true})
	callErr(t, s, 400, "POST", "regions/us-central1/subnetworks", obj{"name": "x", "network": "global/networks/default",
		"stackType": "IPV6_ONLY", "ipv6AccessType": "EXTERNAL"})
	callErr(t, s, 400, "POST", "regions/us-central1/subnetworks", obj{"name": "x", "network": "global/networks/v6", "stackType": "IPV6_ONLY"})
	callErr(t, s, 400, "POST", "regions/us-central1/subnetworks", obj{"name": "x", "network": "global/networks/v6", "stackType": "IPV6_ONLY",
		"ipv6AccessType": "INTERNAL", "secondaryIpRanges": []any{map[string]any{"rangeName": "a", "ipCidrRange": "10.1.0.0/24"}}})
	call("POST", "regions/us-central1/subnetworks", nil, obj{"name": "only6", "network": "global/networks/v6", "stackType": "IPV6_ONLY", "ipv6AccessType": "INTERNAL"})
	call("POST", "regions/us-central1/subnetworks", nil, obj{"name": "ext6", "network": "global/networks/v6", "stackType": "IPV6_ONLY", "ipv6AccessType": "EXTERNAL"})
	call("POST", "regions/us-central1/subnetworks", nil, obj{"name": "dual", "network": "global/networks/v6", "ipCidrRange": "10.6.0.0/24",
		"stackType": "IPV4_IPV6", "ipv6AccessType": "INTERNAL"})
	o6 := call("GET", "regions/us-central1/subnetworks/only6", nil, nil)
	if o6["stackType"] != "IPV6_ONLY" || o6["ipv6AccessType"] != "INTERNAL" || !strings.HasSuffix(str(o6, "ipv6CidrRange"), "/64") ||
		o6["internalIpv6Prefix"] != o6["ipv6CidrRange"] || o6["ipCidrRange"] != nil || o6["gatewayAddress"] != nil {
		t.Fatalf("only6 = %v", o6)
	}
	if e6 := call("GET", "regions/us-central1/subnetworks/ext6", nil, nil); !strings.HasPrefix(str(e6, "externalIpv6Prefix"), "2600:190") {
		t.Fatalf("ext6 = %v", e6)
	}
	// No IPv4 subnet route; the /64's route is the generated one.
	for _, r := range listOf(call("GET", "global/routes", nil, nil)["items"]) {
		if m := objOf(r); str(m, "network") == "projects/p2139/global/networks/v6" && str(m, "destRange") == "" {
			t.Fatalf("route without a range: %v", m)
		}
	}
	callErr(t, s, 400, "POST", "regions/us-central1/subnetworks/only6/expandIpCidrRange", obj{"ipCidrRange": "10.9.0.0/16"})
	callErr(t, s, 400, "PATCH", "regions/us-central1/subnetworks/only6", obj{"stackType": "IPV4_IPV6", "fingerprint": o6["fingerprint"]})
	callErr(t, s, 400, "PATCH", "regions/us-central1/subnetworks/only6", obj{"secondaryIpRanges": []any{map[string]any{"rangeName": "a", "ipCidrRange": "10.1.0.0/24"}},
		"fingerprint": o6["fingerprint"]})
	d := call("GET", "regions/us-central1/subnetworks/dual", nil, nil)
	callErr(t, s, 400, "PATCH", "regions/us-central1/subnetworks/dual", obj{"stackType": "IPV6_ONLY", "fingerprint": d["fingerprint"]})

	nic := func(sub, st string) obj {
		return obj{"networkInterfaces": []any{map[string]any{"subnetwork": "regions/us-central1/subnetworks/" + sub, "stackType": st}}}
	}
	// Refusals: dual-stack on an IPv6-only subnet, IPv4 asked for, a
	// non-Debian/Ubuntu image.
	callErr(t, s, 400, "POST", "zones/us-central1-a/instances", vmBody("bad", nic("only6", "IPV4_IPV6")))
	callErr(t, s, 400, "POST", "zones/us-central1-a/instances", vmBody("bad", obj{"networkInterfaces": []any{map[string]any{
		"subnetwork": "regions/us-central1/subnetworks/only6", "stackType": "IPV6_ONLY", "networkIP": "10.6.0.9"}}}))
	callErr(t, s, 400, "POST", "zones/us-central1-a/instances", vmBody("bad", obj{"networkInterfaces": []any{map[string]any{
		"subnetwork": "regions/us-central1/subnetworks/ext6", "stackType": "IPV6_ONLY", "accessConfigs": []any{map[string]any{"type": "ONE_TO_ONE_NAT"}}}}}))
	rocky := vmBody("bad", nic("only6", "IPV6_ONLY"))
	rocky["disks"] = []any{map[string]any{"boot": true, "autoDelete": true, "initializeParams": map[string]any{
		"sourceImage": "projects/rocky-linux-cloud/global/images/family/rocky-linux-9"}}}
	if _, err := s.Call(http.MethodPost, "p2139", "zones/us-central1-a/instances", nil, rocky); ErrorStatus(err) != 400 ||
		!strings.Contains(err.Error(), "Ubuntu and Debian") {
		t.Fatalf("a Rocky Linux IPv6-only VM: %v", err)
	}
	call("POST", "zones/us-central1-a/instances", nil, vmBody("a", nic("only6", "IPV6_ONLY")))
	call("POST", "zones/us-central1-a/instances", nil, vmBody("b", nic("dual", "IPV6_ONLY")))
	call("POST", "zones/us-central1-a/instances", nil, vmBody("c", nic("dual", "IPV4_IPV6")))
	call("POST", "zones/us-central1-a/instances", nil, vmBody("x", nic("ext6", "IPV6_ONLY")))
	na := objOf(listOf(call("GET", "zones/us-central1-a/instances/a", nil, nil)["networkInterfaces"])[0])
	nb := objOf(listOf(call("GET", "zones/us-central1-a/instances/b", nil, nil)["networkInterfaces"])[0])
	nc := objOf(listOf(call("GET", "zones/us-central1-a/instances/c", nil, nil)["networkInterfaces"])[0])
	nx := objOf(listOf(call("GET", "zones/us-central1-a/instances/x", nil, nil)["networkInterfaces"])[0])
	if na["stackType"] != "IPV6_ONLY" || na["networkIP"] != nil || na["internalIpv6PrefixLength"] != 96.0 ||
		!strings.HasPrefix(str(na, "ipv6Address"), strings.TrimSuffix(str(o6, "ipv6CidrRange"), "::/64")) {
		t.Fatalf("a's interface = %v", na)
	}
	if nb["stackType"] != "IPV6_ONLY" || nb["networkIP"] != nil || str(nb, "ipv6Address") == "" {
		t.Fatalf("b's interface = %v", nb)
	}
	if acs := listOf(nx["ipv6AccessConfigs"]); len(acs) != 1 || nx["networkIP"] != nil || str(objOf(acs[0]), "type") != "DIRECT_IPV6" {
		t.Fatalf("x's interface = %v", nx)
	}
	// The metadata server's interface keys.
	s.mu.Lock()
	inst := s.res["projects/p2139/zones/us-central1-a/instances/a"]
	root := s.guestTreeLocked(inst, "p2139")
	if v, _, ok := guestBody(root, "instance/network-interfaces/0/ipv6s", nil); !ok || v != str(na, "ipv6Address")+"\n" {
		t.Errorf("ipv6s = %q, %v", v, ok)
	}
	if v, _, _ := guestBody(root, "instance/network-interfaces/0/mac", nil); !strings.HasPrefix(v, "42:02:") {
		t.Errorf("mac = %q", v)
	}
	for _, n := range []string{"b", "c"} {
		s.res["projects/p2139/zones/us-central1-a/instances/"+n]["status"] = stRunning
	}
	s.vpcConfigLocked(s.res["projects/p2139/zones/us-central1-a/instances/b"], "10.244.0.11")
	s.vpcConfigLocked(s.res["projects/p2139/zones/us-central1-a/instances/c"], "10.244.0.12")
	conf, ok := s.vpcConfigLocked(inst, "10.244.0.10")
	confC, _ := s.vpcConfigLocked(s.res["projects/p2139/zones/us-central1-a/instances/c"], "10.244.0.12")
	s.mu.Unlock()
	macB := vmNet{ip6: str(nb, "ipv6Address")}.mac()
	if !ok || strings.Contains(conf, "\naddr ") || strings.Contains(conf, "\nroute ") || strings.Contains(conf, "\npeer ") ||
		!strings.Contains(conf, "addr6 "+str(na, "ipv6Address")+"\n") ||
		!strings.Contains(conf, "route6 "+str(o6, "ipv6CidrRange")+"\n") || !strings.Contains(conf, "route6 "+str(d, "ipv6CidrRange")+"\n") ||
		!strings.Contains(conf, "peer6 "+str(nb, "ipv6Address")+" "+macB+" 10.244.0.11\n") ||
		!strings.Contains(conf, "peer6 "+str(nc, "ipv6Address")+" "+macOf(str(nc, "networkIP"))+" 10.244.0.12\n") ||
		!strings.Contains(conf, "host "+str(nb, "ipv6Address")+" b.us-central1-a.c.p2139.internal b\n") {
		t.Fatalf("a's configuration:\n%s", conf)
	}
	// The dual-stack VM reaches the IPv6-only ones over IPv6 only.
	if !strings.Contains(confC, "peer6 "+str(nb, "ipv6Address")+" "+macB+" 10.244.0.11\n") || strings.Contains(confC, "peer  ") ||
		!strings.Contains(confC, "addr "+str(nc, "networkIP")+"\n") {
		t.Fatalf("c's configuration:\n%s", confC)
	}
	// IPv4 addresses are not of an IPv6-only subnet.
	callErr(t, s, 400, "POST", "regions/us-central1/addresses", obj{"name": "v4", "addressType": "INTERNAL",
		"subnetwork": "regions/us-central1/subnetworks/only6"})
	call("POST", "regions/us-central1/addresses", nil, obj{"name": "v6", "addressType": "INTERNAL", "ipVersion": "IPV6",
		"subnetwork": "regions/us-central1/subnetworks/only6"})
}

// TestGlobalIPv6AddressesAndForwardingRules (#2140): a global external
// IPv6 address (a /64 of Google's pool, Premium, none given, none
// internal), a global external Application Load Balancer's forwarding
// rule on it (ipVersion IPV6, the address IN_USE) carrying traffic, an
// ephemeral IPv6 rule, and the refusals.
func TestGlobalIPv6AddressesAndForwardingRules(t *testing.T) {
	s, _, base := lbFixture(t)
	lbCall(t, s, "POST", "global/addresses", obj{"name": "v6", "ipVersion": "IPV6"})
	a := lbCall(t, s, "GET", "global/addresses/v6", nil)
	if a["ipVersion"] != "IPV6" || a["addressType"] != "EXTERNAL" || a["networkTier"] != "PREMIUM" || a["status"] != "RESERVED" ||
		!strings.HasPrefix(str(a, "address"), "2600:1901:") || !strings.HasSuffix(str(a, "address"), "::") {
		t.Fatalf("address = %v", a)
	}
	for _, b := range []obj{
		{"name": "x", "ipVersion": "IPV6", "addressType": "INTERNAL"},
		{"name": "x", "ipVersion": "IPV6", "address": "2600:1901::1"},
		{"name": "x", "ipVersion": "IPV6", "networkTier": "STANDARD"},
	} {
		if _, err := vmCall(t, s, "POST", "global/addresses", b); ErrorStatus(err) != 400 {
			t.Fatalf("POST %v: %v", b, err)
		}
	}
	lbCall(t, s, "POST", "global/forwardingRules", obj{"name": "lb6", "target": "global/targetHttpProxies/lb",
		"portRange": "8080", "loadBalancingScheme": "EXTERNAL_MANAGED", "IPAddress": "global/addresses/v6"})
	r := lbCall(t, s, "GET", "global/forwardingRules/lb6", nil)
	if r["ipVersion"] != "IPV6" || r["IPAddress"] != a["address"] || str(r, "cloudburrowAddress") == "" {
		t.Fatalf("rule = %v", r)
	}
	if a := lbCall(t, s, "GET", "global/addresses/v6", nil); a["status"] != "IN_USE" {
		t.Fatalf("address after the rule = %v", a)
	}
	c := &http.Client{}
	waitFor(t, "the IPv6 rule to answer", func() bool {
		code, body, _ := get(t, c, "http://"+str(r, "cloudburrowAddress")+"/web/x")
		return code == 200 && strings.Contains(body, "/web/x")
	})
	_ = base
	// An ephemeral IPv6 rule; an IPv4 address with ipVersion IPV6; an
	// internal IPv6 rule.
	lbCall(t, s, "POST", "global/forwardingRules", obj{"name": "eph6", "target": "global/targetHttpProxies/lb",
		"portRange": "8081", "loadBalancingScheme": "EXTERNAL_MANAGED", "ipVersion": "IPV6"})
	e := lbCall(t, s, "GET", "global/forwardingRules/eph6", nil)
	if e["ipVersion"] != "IPV6" || !strings.HasPrefix(str(e, "IPAddress"), "2600:1901:") || e["IPAddress"] == r["IPAddress"] {
		t.Fatalf("ephemeral rule = %v", e)
	}
	lbCall(t, s, "POST", "global/addresses", obj{"name": "v4"})
	if _, err := vmCall(t, s, "POST", "global/forwardingRules", obj{"name": "bad", "target": "global/targetHttpProxies/lb", "portRange": "8082",
		"loadBalancingScheme": "EXTERNAL_MANAGED", "ipVersion": "IPV6", "IPAddress": "global/addresses/v4"}); ErrorStatus(err) != 400 {
		t.Fatalf("an IPv4 address on an IPv6 rule: %v", err)
	}
	if _, err := vmCall(t, s, "POST", "global/forwardingRules", obj{"name": "bad", "target": "global/targetHttpProxies/lb", "portRange": "8082",
		"loadBalancingScheme": "EXTERNAL_MANAGED", "ipVersion": "IPV5"}); ErrorStatus(err) != 400 {
		t.Fatalf("ipVersion IPV5: %v", err)
	}
	lbCall(t, s, "DELETE", "global/forwardingRules/lb6", nil)
	if a := lbCall(t, s, "GET", "global/addresses/v6", nil); a["status"] != "RESERVED" {
		t.Fatalf("address after deleting the rule = %v", a)
	}
}
