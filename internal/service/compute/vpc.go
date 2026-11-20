package compute

import (
	"fmt"
	"hash/fnv"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// VM networking (#1968). Each VPC network is one VXLAN segment, its VNI
// derived from the network's ID, built inside the VMs' own network
// namespaces: every VM's guest makes a vxlan device (vpc0) holding its
// internal IP, with an on-link route for each of its network's subnet
// ranges, and a static neighbour and forwarding entry for each peer VM on
// the same network, whose VXLAN packets travel over the pods' own network
// to the peer's pod IP. A VM on another network has another VNI and no
// forwarding entry, so its packets never arrive.
//
// Each VM's guest agent polls GuestPrefix TOKEN/cloudburrow/vpc every two
// seconds, reporting its pod IP, and applies what it reads: the segment,
// its peers, the /etc/hosts lines of the network's internal DNS names
// (INSTANCE.ZONE.c.PROJECT.internal) and the nftables ruleset of the VPC
// firewall rules that apply to it, compiled here.

// vpcSuffix is the guest path the network configuration is read from.
const vpcSuffix = "cloudburrow/vpc"

// podFresh is how long a reported pod IP is used without a new report.
const podFresh = 30 * time.Second

// ClusterCIDRs are the kind cluster's pod and Service ranges (kind's
// defaults, which CloudBurrow's cluster keeps): traffic a VM sends there
// over its pod interface (the metadata server, the cluster DNS, the VXLAN
// underlay) is exempt from egress rules, as Google always allows the
// metadata server.
var ClusterCIDRs = []string{"10.244.0.0/16", "10.96.0.0/12"}

type podSeen struct {
	ip   string
	seen time.Time
}

// vniOf is a network's VXLAN network identifier: 24 bits, never 0.
func vniOf(networkID string) uint32 {
	h := fnv.New32a()
	h.Write([]byte(networkID))
	v := h.Sum32() & 0xffffff
	if v == 0 {
		v = 1
	}
	return v
}

// macOf is the MAC address a VM's vpc0 device takes: 42:01 and its internal
// IP, as Compute Engine's guests see 42:01 addresses.
func macOf(ip string) string {
	v4 := net.ParseIP(ip).To4()
	if v4 == nil {
		return ""
	}
	return fmt.Sprintf("42:01:%02x:%02x:%02x:%02x", v4[0], v4[1], v4[2], v4[3])
}

// vmNet is one instance's place on its network.
type vmNet struct {
	rel, name, zone, project string
	network, ip, sa          string
	tags                     []string
	running                  bool
}

func vmNetOf(rel string, o obj) (vmNet, bool) {
	nics, _ := o["networkInterfaces"].([]any)
	if len(nics) == 0 {
		return vmNet{}, false
	}
	nic, _ := nics[0].(map[string]any)
	v := vmNet{rel: rel, name: str(o, "name"), zone: lastSeg(str(o, "zone")), project: strings.Split(rel, "/")[1],
		network: str(nic, "network"), ip: str(nic, "networkIP"), running: str(o, "status") == stRunning}
	if t, ok := o["tags"].(map[string]any); ok {
		v.tags = strsAny(t["items"])
	}
	if sas, _ := o["serviceAccounts"].([]any); len(sas) > 0 {
		sa, _ := sas[0].(map[string]any)
		v.sa = str(sa, "email")
	}
	return v, v.network != "" && v.ip != ""
}

func (v vmNet) fqdn() string { return v.name + "." + v.zone + ".c." + v.project + ".internal" }

// serveVPC answers a guest's poll for its network configuration.
func (s *Server) serveVPC(w http.ResponseWriter, r *http.Request, inst obj) {
	s.mu.Lock()
	text, ok := s.vpcConfigLocked(inst, r.URL.Query().Get("pod"))
	s.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Metadata-Flavor", "Google")
	w.Header().Set("Content-Type", "text/plain")
	_, _ = w.Write([]byte(text))
}

// vpcConfigLocked is the configuration text of inst's guest, recording its
// reported pod IP.
func (s *Server) vpcConfigLocked(inst obj, pod string) (string, bool) {
	rel := str(inst, "selfLink")
	self, ok := vmNetOf(rel, inst)
	if !ok {
		return "", false
	}
	if s.pods == nil {
		s.pods = map[string]podSeen{}
	}
	if ip := net.ParseIP(pod).To4(); ip != nil {
		s.pods[rel] = podSeen{ip: ip.String(), seen: s.opts.Now()}
	}
	nw, ok := s.res[self.network]
	if !ok {
		return "", false
	}
	var members []vmNet
	for k, o := range s.res {
		if !strings.Contains(k, "/instances/") {
			continue
		}
		if v, ok := vmNetOf(k, o); ok && v.network == self.network {
			members = append(members, v)
		}
	}
	sort.Slice(members, func(i, j int) bool { return members[i].rel < members[j].rel })
	var subnets []string
	for k, o := range s.res {
		if strings.Contains(k, "/subnetworks/") && str(o, "network") == self.network {
			if c := str(o, "ipCidrRange"); c != "" {
				subnets = append(subnets, c)
			}
		}
	}
	sort.Strings(subnets)
	mtu := 1460
	if v, ok := nw["mtu"].(float64); ok && v > 0 {
		mtu = int(v)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "vni %d\nmtu %d\naddr %s\nmac %s\n", vniOf(str(nw, "id")), mtu, self.ip, macOf(self.ip))
	for _, c := range subnets {
		fmt.Fprintf(&b, "route %s\n", c)
	}
	var peerPods []string
	now := s.opts.Now()
	for _, m := range members {
		fmt.Fprintf(&b, "host %s %s %s\n", m.ip, m.fqdn(), m.name)
		if m.rel == rel || !m.running {
			continue
		}
		if p, ok := s.pods[m.rel]; ok && now.Sub(p.seen) < podFresh {
			fmt.Fprintf(&b, "peer %s %s %s\n", m.ip, macOf(m.ip), p.ip)
			peerPods = append(peerPods, p.ip)
		}
	}
	b.WriteString("nft\n")
	b.WriteString(firewallNFT(self, members, s.firewallsOfLocked(self.network), peerPods))
	return b.String(), true
}

// firewallsOfLocked is the network's enabled firewall rules.
func (s *Server) firewallsOfLocked(network string) []obj {
	var out []obj
	for k, o := range s.res {
		if strings.Contains(k, "/global/firewalls/") && str(o, "network") == network && o["disabled"] != true {
			out = append(out, o)
		}
	}
	return out
}

// fwRule is a firewall rule as the compiler orders it.
type fwRule struct {
	o    obj
	prio int
	deny bool
}

// appliesTo is whether the rule's targets include v: every instance when
// it names no target tags or service accounts.
func appliesTo(o obj, v vmNet) bool {
	tt, ts := strsAny(o["targetTags"]), strsAny(o["targetServiceAccounts"])
	if len(tt) == 0 && len(ts) == 0 {
		return true
	}
	for _, t := range tt {
		if hasStr(v.tags, t) {
			return true
		}
	}
	return v.sa != "" && hasStr(ts, v.sa)
}

// nftSet is a list of addresses or ports as an nftables anonymous set.
func nftSet(l []string) string { return "{ " + strings.Join(l, ", ") + " }" }

// protoMatch is an allowed or denied entry as an nftables match: "" for
// all protocols.
func protoMatch(e map[string]any) string {
	p := strings.ToLower(str(e, "IPProtocol"))
	num := map[string]string{"tcp": "tcp", "udp": "udp", "icmp": "icmp", "esp": "esp", "ah": "ah", "sctp": "sctp", "ipip": "4",
		"6": "tcp", "17": "udp", "1": "icmp", "132": "sctp"}
	if p == "all" {
		return ""
	}
	name, ok := num[p]
	if !ok {
		if n, err := strconv.Atoi(p); err == nil {
			name = strconv.Itoa(n)
		} else {
			return "meta l4proto 255"
		}
	}
	m := "meta l4proto " + name
	if ports := strsAny(e["ports"]); len(ports) > 0 {
		m += " th dport " + nftSet(ports)
	}
	return m
}

// firewallNFT compiles the rules that apply to self into an nftables
// ruleset, as https://docs.cloud.google.com/firewall/docs/firewalls
// evaluates them: stateful (replies of an allowed connection pass), the
// rule of the lowest priority number deciding, a deny winning over an
// allow of the same priority, an ingress rule's sources the union of its
// sourceRanges and the primary internal IPs of the instances its source
// tags or service accounts select, and the implied rules at the end: deny
// every other ingress, allow every other egress. Ingress is filtered on
// vpc0, the VPC interface; the pod interface accepts only the VXLAN
// underlay from the network's peers (so a VM cannot be reached around its
// firewall at its pod IP). Egress is filtered on vpc0 and on the pod
// interface, except to the cluster's own ranges.
func firewallNFT(self vmNet, members []vmNet, rules []obj, peerPods []string) string {
	var in, eg []fwRule
	for _, o := range rules {
		if !appliesTo(o, self) {
			continue
		}
		p, _ := o["priority"].(float64)
		r := fwRule{o: o, prio: int(p), deny: len(sliceOf(o["denied"])) > 0}
		if str(o, "direction") == "EGRESS" {
			eg = append(eg, r)
		} else {
			in = append(in, r)
		}
	}
	order := func(l []fwRule) {
		sort.SliceStable(l, func(i, j int) bool {
			if l[i].prio != l[j].prio {
				return l[i].prio < l[j].prio
			}
			if l[i].deny != l[j].deny {
				return l[i].deny
			}
			return str(l[i].o, "name") < str(l[j].o, "name")
		})
	}
	order(in)
	order(eg)
	var b strings.Builder
	b.WriteString("table inet cloudburrow {\n")
	b.WriteString("  chain ingress {\n    type filter hook input priority filter; policy accept;\n")
	b.WriteString("    iifname \"lo\" accept\n    ct state established,related accept\n    ct state invalid drop\n")
	if len(peerPods) > 0 {
		b.WriteString("    iifname != \"vpc0\" ip saddr " + nftSet(peerPods) + " udp dport 4789 accept\n")
	}
	b.WriteString("    iifname != \"vpc0\" drop\n")
	for _, r := range in {
		src := strsAny(r.o["sourceRanges"])
		tags, sas := strsAny(r.o["sourceTags"]), strsAny(r.o["sourceServiceAccounts"])
		for _, m := range members {
			if (len(tags) > 0 && anyIn(m.tags, tags)) || (len(sas) > 0 && m.sa != "" && hasStr(sas, m.sa)) {
				src = append(src, m.ip)
			}
		}
		if len(src) == 0 {
			continue // source tags no instance has: matches nothing
		}
		match := "ip saddr " + nftSet(src)
		if dst := strsAny(r.o["destinationRanges"]); len(dst) > 0 {
			match += " ip daddr " + nftSet(dst)
		}
		writeEntries(&b, r, match)
	}
	b.WriteString("    counter drop comment \"implied deny ingress\"\n  }\n")
	b.WriteString("  chain egress {\n    type filter hook output priority filter; policy accept;\n")
	b.WriteString("    oifname \"lo\" accept\n    ct state established,related accept\n")
	b.WriteString("    oifname != \"vpc0\" ip daddr " + nftSet(append([]string{"169.254.169.254"}, ClusterCIDRs...)) + " accept\n")
	for _, r := range eg {
		dst := strsAny(r.o["destinationRanges"])
		if len(dst) == 0 {
			dst = []string{"0.0.0.0/0"}
		}
		match := "ip daddr " + nftSet(dst)
		if src := strsAny(r.o["sourceRanges"]); len(src) > 0 {
			match += " ip saddr " + nftSet(src)
		}
		writeEntries(&b, r, match)
	}
	b.WriteString("    accept comment \"implied allow egress\"\n  }\n}\n")
	return b.String()
}

func writeEntries(b *strings.Builder, r fwRule, match string) {
	entries, verdict := sliceOf(r.o["allowed"]), "accept"
	if r.deny {
		entries, verdict = sliceOf(r.o["denied"]), "drop"
	}
	for _, e := range entries {
		em, _ := e.(map[string]any)
		line := match
		if pm := protoMatch(em); pm != "" {
			line += " " + pm
		}
		fmt.Fprintf(b, "    %s %s comment %q\n", line, verdict, str(r.o, "name")+" priority "+strconv.Itoa(r.prio))
	}
}

func sliceOf(v any) []any {
	l, _ := v.([]any)
	return l
}

func anyIn(have, want []string) bool {
	for _, w := range want {
		if hasStr(have, w) {
			return true
		}
	}
	return false
}
