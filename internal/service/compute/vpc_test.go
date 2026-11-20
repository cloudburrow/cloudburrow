package compute

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

func fw(name, dir string, prio float64, deny bool, extra obj) obj {
	o := obj{"name": name, "direction": dir, "priority": prio, "network": "projects/p/global/networks/n"}
	e := []any{map[string]any{"IPProtocol": "tcp", "ports": []any{"8080"}}}
	if deny {
		o["denied"] = e
	} else {
		o["allowed"] = e
	}
	for k, v := range extra {
		o[k] = v
	}
	return o
}

func TestFirewallNFTOrdersAndSelects(t *testing.T) {
	self := vmNet{rel: "projects/p/zones/z/instances/a", ip: "10.0.0.2", tags: []string{"web"}, sa: "sa@p.iam.gserviceaccount.com"}
	peer := vmNet{rel: "projects/p/zones/z/instances/b", ip: "10.0.0.3", tags: []string{"client"}}
	rules := []obj{
		fw("allow-low", "INGRESS", 2000, false, obj{"sourceRanges": []any{"10.0.0.0/8"}}),
		fw("deny-high", "INGRESS", 1000, true, obj{"sourceRanges": []any{"10.0.0.0/8"}}),
		fw("allow-same", "INGRESS", 1000, false, obj{"sourceRanges": []any{"10.0.0.0/8"}}),
		fw("tags", "INGRESS", 500, false, obj{"sourceTags": []any{"client"}, "targetTags": []any{"web"}}),
		fw("other-target", "INGRESS", 100, true, obj{"sourceRanges": []any{"0.0.0.0/0"}, "targetTags": []any{"db"}}),
		fw("sa-target", "INGRESS", 600, false, obj{"sourceRanges": []any{"1.2.3.4"}, "targetServiceAccounts": []any{"sa@p.iam.gserviceaccount.com"}}),
		fw("no-source-match", "INGRESS", 700, false, obj{"sourceTags": []any{"nobody"}}),
		fw("egress-deny", "EGRESS", 1000, true, obj{"destinationRanges": []any{"10.0.0.3/32"}}),
	}
	out := firewallNFT(self, []vmNet{self, peer}, rules, []string{"10.244.1.5"})
	idx := func(s string) int {
		i := strings.Index(out, s)
		if i < 0 {
			t.Fatalf("%q missing from:\n%s", s, out)
		}
		return i
	}
	if strings.Contains(out, "other-target") || strings.Contains(out, "no-source-match") {
		t.Fatalf("a rule that does not apply was compiled:\n%s", out)
	}
	tags := idx(`ip saddr { 10.0.0.3 } meta l4proto tcp th dport { 8080 } accept comment "tags priority 500"`)
	sa := idx(`"sa-target priority 600"`)
	deny := idx(`drop comment "deny-high priority 1000"`)
	same := idx(`accept comment "allow-same priority 1000"`)
	low := idx(`"allow-low priority 2000"`)
	implied := idx(`implied deny ingress`)
	if !(tags < sa && sa < deny && deny < same && same < low && low < implied) {
		t.Fatalf("rules out of order (priority, then deny first):\n%s", out)
	}
	idx(`ip saddr { 10.244.1.5 } udp dport 4789 accept`)
	if e := idx(`ip daddr { 10.0.0.3/32 } meta l4proto tcp th dport { 8080 } drop comment "egress-deny priority 1000"`); e < idx("chain egress") || e > idx("implied allow egress") {
		t.Fatalf("egress rule misplaced:\n%s", out)
	}
}

func TestProtoMatch(t *testing.T) {
	for _, c := range []struct {
		in   map[string]any
		want string
	}{
		{map[string]any{"IPProtocol": "all"}, ""},
		{map[string]any{"IPProtocol": "icmp"}, "meta l4proto icmp"},
		{map[string]any{"IPProtocol": "udp", "ports": []any{"53", "1000-2000"}}, "meta l4proto udp th dport { 53, 1000-2000 }"},
		{map[string]any{"IPProtocol": "47"}, "meta l4proto 47"},
		{map[string]any{"IPProtocol": "6", "ports": []any{"22"}}, "meta l4proto tcp th dport { 22 }"},
	} {
		if got := protoMatch(c.in); got != c.want {
			t.Errorf("%v: %q, want %q", c.in, got, c.want)
		}
	}
}

func TestVPCConfigPeersAndHosts(t *testing.T) {
	now := time.Unix(1e9, 0)
	s := &Server{opts: Options{Now: func() time.Time { return now }}, res: map[string]obj{}}
	nw := "projects/p/global/networks/n"
	s.res[nw] = obj{"id": "123", "mtu": 1460.0}
	s.res["projects/p/global/networks/other"] = obj{"id": "456"}
	s.res["projects/p/regions/r/subnetworks/s"] = obj{"network": nw, "ipCidrRange": "10.0.0.0/24"}
	inst := func(name, network, ip string) obj {
		rel := "projects/p/zones/us-central1-a/instances/" + name
		o := obj{"name": name, "selfLink": rel, "zone": "projects/p/zones/us-central1-a", "status": "RUNNING",
			"networkInterfaces": []any{map[string]any{"network": network, "networkIP": ip}}}
		s.res[rel] = o
		return o
	}
	a, b := inst("a", nw, "10.0.0.2"), inst("b", nw, "10.0.0.3")
	c := inst("c", "projects/p/global/networks/other", "10.1.0.2")
	s.vpcConfigLocked(b, "10.244.0.8")
	s.vpcConfigLocked(c, "10.244.0.9")
	out, ok := s.vpcConfigLocked(a, "10.244.0.7")
	if !ok {
		t.Fatal("no configuration")
	}
	for _, want := range []string{"vni " + strconv.FormatUint(uint64(vniOf("123")), 10), "addr 10.0.0.2", "mac 42:01:0a:00:00:02", "route 10.0.0.0/24",
		"peer 10.0.0.3 42:01:0a:00:00:03 10.244.0.8", "host 10.0.0.3 b.us-central1-a.c.p.internal b", "\nnft\ntable inet cloudburrow"} {
		if !strings.Contains(out, want) {
			t.Errorf("%q missing:\n%s", want, out)
		}
	}
	if strings.Contains(out, "10.244.0.9") || strings.Contains(out, "c.us-central1-a") {
		t.Errorf("an instance of another network is a peer:\n%s", out)
	}
	now = now.Add(time.Minute)
	if out, _ := s.vpcConfigLocked(a, ""); strings.Contains(out, "peer ") {
		t.Errorf("a peer that stopped reporting is still a peer:\n%s", out)
	}
}
