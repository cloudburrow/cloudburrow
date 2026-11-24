package compute

import (
	"fmt"
	"hash/fnv"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The VPC firewall on a VM's relayed connections (#2180). A connection a
// VM makes to a load balancer's, Secure Web Proxy gateway's or Private
// Service Connect endpoint's VPC address is redirected inside the guest
// to its local relay (vmrelay.go), so the guest's own nftables egress
// chain sees it to 127.0.0.1 and cannot filter it. The relay, which knows
// the instance, the address and port it dialled and the protocol, applies
// the instance's egress firewall rules here instead, as
// https://docs.cloud.google.com/firewall/docs/firewalls (re-read
// 2026-11-23) evaluates them: the enabled EGRESS rules of the instance's
// network whose targets include it (every instance, its network tags or
// its service account), whose destinationRanges hold the address (and
// sourceRanges, when set, the instance's internal IP), and one of whose
// entries holds the protocol and port; the rule of the lowest priority
// number decides, a deny winning over an allow of the same priority; with
// none, the implied allow egress rule allows. A denied connection is
// closed at the relay and reaches nothing.
//
// Firewall Rules Logging
// (https://docs.cloud.google.com/firewall/docs/firewall-rules-logging and
// .../vpc-log-format): a deciding rule whose logConfig.enable is true
// writes an entry to Cloud Logging, in the project of the network, named
// compute.googleapis.com%2Ffirewall, of monitored resource gce_subnetwork,
// its jsonPayload the connection, disposition and rule_details, with the
// instance and vpc metadata unless logConfig.metadata is
// EXCLUDE_ALL_METADATA. An allowed connection is logged once; a denied
// one once per 5-tuple every 5 seconds while it is retried. The implied
// rules are never logged. Hierarchical and network firewall policies are
// not served (docs/gaps.md), so only VPC firewall rules apply.

// FirewallLogEntry is one Firewall Rules Logging record.
type FirewallLogEntry struct {
	// Project is the project of the network, where the entry is written.
	Project string
	// Labels are the gce_subnetwork resource's: project_id,
	// subnetwork_id, subnetwork_name and location.
	Labels map[string]string
	// Payload is the entry's jsonPayload.
	Payload map[string]any
	Time    time.Time
}

// FirewallLogName is a project's firewall log.
func FirewallLogName(project string) string {
	return "projects/" + project + "/logs/compute.googleapis.com%2Ffirewall"
}

// fwDeniedEvery is how often a denied 5-tuple is logged again.
const fwDeniedEvery = 5 * time.Second

// egressDecision is the firewall's decision on one relayed connection.
type egressDecision struct {
	allow bool
	// rule is the deciding rule, nil for the implied allow egress.
	rule obj
	// log is the record to write, nil when none is.
	log *FirewallLogEntry
}

// egressFirewallLocked decides a relayed connection from inst (self) to
// dst over proto ("tcp" or "udp"); src is the connection's source (the
// instance's internal IP and its client's port).
func (s *Server) egressFirewallLocked(inst obj, self vmNet, src, dst netip.AddrPort, proto string) egressDecision {
	var rules []fwRule
	for _, o := range s.firewallsOfLocked(self.network) {
		if str(o, "direction") != "EGRESS" || !appliesTo(o, self) {
			continue
		}
		p, _ := o["priority"].(float64)
		rules = append(rules, fwRule{o: o, prio: int(p), deny: len(sliceOf(o["denied"])) > 0})
	}
	sort.SliceStable(rules, func(i, j int) bool {
		if rules[i].prio != rules[j].prio {
			return rules[i].prio < rules[j].prio
		}
		if rules[i].deny != rules[j].deny {
			return rules[i].deny
		}
		return str(rules[i].o, "name") < str(rules[j].o, "name")
	})
	for _, r := range rules {
		if !egressMatches(r, src.Addr(), dst, proto) {
			continue
		}
		d := egressDecision{allow: !r.deny, rule: r.o}
		d.log = s.firewallLogLocked(inst, self, r, src, dst, proto)
		return d
	}
	return egressDecision{allow: true}
}

// egressMatches is whether an egress rule's destinations, sources and
// entries hold a connection.
func egressMatches(r fwRule, src netip.Addr, dst netip.AddrPort, proto string) bool {
	dsts := strsAny(r.o["destinationRanges"])
	if len(dsts) == 0 {
		dsts = []string{"0.0.0.0/0"}
	}
	if !anyRangeHolds(dsts, dst.Addr()) {
		return false
	}
	if srcs := strsAny(r.o["sourceRanges"]); len(srcs) > 0 && !anyRangeHolds(srcs, src) {
		return false
	}
	entries := sliceOf(r.o["allowed"])
	if r.deny {
		entries = sliceOf(r.o["denied"])
	}
	for _, e := range entries {
		em, _ := e.(map[string]any)
		if entryHolds(em, proto, int(dst.Port())) {
			return true
		}
	}
	return false
}

// anyRangeHolds is whether one of the ranges (CIDRs or addresses) holds a.
func anyRangeHolds(ranges []string, a netip.Addr) bool {
	for _, r := range ranges {
		if !strings.Contains(r, "/") {
			if b, err := netip.ParseAddr(r); err == nil && b == a {
				return true
			}
			continue
		}
		if p, err := netip.ParsePrefix(r); err == nil && p.Contains(a) {
			return true
		}
	}
	return false
}

// entryHolds is whether an allowed or denied entry holds proto ("tcp" or
// "udp") on port: its IPProtocol all, the protocol's name or number, and
// its ports (every port when it lists none).
func entryHolds(e map[string]any, proto string, port int) bool {
	p := strings.ToLower(str(e, "IPProtocol"))
	num := map[string]string{"tcp": "6", "udp": "17"}[proto]
	if p != "all" && p != proto && p != num {
		return false
	}
	ports := strsAny(e["ports"])
	if len(ports) == 0 {
		return true
	}
	for _, spec := range ports {
		lo, hi, ok := strings.Cut(spec, "-")
		a, _ := strconv.Atoi(lo)
		b := a
		if ok {
			b, _ = strconv.Atoi(hi)
		}
		if port >= a && port <= b {
			return true
		}
	}
	return false
}

// firewallLogLocked is the record a deciding rule writes, nil when its
// logging is off or a denied 5-tuple was logged under fwDeniedEvery ago.
func (s *Server) firewallLogLocked(inst obj, self vmNet, r fwRule, src, dst netip.AddrPort, proto string) *FirewallLogEntry {
	lc, _ := r.o["logConfig"].(map[string]any)
	if lc == nil || lc["enable"] != true || s.opts.FirewallLog == nil {
		return nil
	}
	now := s.opts.Now()
	if r.deny {
		key := proto + " " + src.String() + " " + dst.String()
		if s.fwDenied == nil {
			s.fwDenied = map[string]time.Time{}
		}
		if t, ok := s.fwDenied[key]; ok && now.Sub(t) < fwDeniedEvery {
			return nil
		}
		for k, t := range s.fwDenied {
			if now.Sub(t) >= fwDeniedEvery {
				delete(s.fwDenied, k)
			}
		}
		s.fwDenied[key] = now
	}
	netProject := strings.Split(self.network, "/")[1]
	protoNum := map[string]int{"tcp": 6, "udp": 17}[proto]
	action, disposition, entries := "ALLOW", "ALLOWED", sliceOf(r.o["allowed"])
	if r.deny {
		action, disposition, entries = "DENY", "DENIED", sliceOf(r.o["denied"])
	}
	var ipPorts []any
	for _, e := range entries {
		em, _ := e.(map[string]any)
		info := map[string]any{"ip_protocol": strings.ToUpper(str(em, "IPProtocol"))}
		if ports := strsAny(em["ports"]); len(ports) > 0 {
			info["port_range"] = anyList(ports)
		}
		ipPorts = append(ipPorts, info)
	}
	details := map[string]any{
		"reference": "network:" + lastSeg(self.network) + "/firewall:" + str(r.o, "name"),
		"priority":  r.prio, "action": action, "direction": "EGRESS", "ip_port_info": ipPorts,
	}
	for k, f := range map[string]string{"destination_range": "destinationRanges", "source_range": "sourceRanges",
		"target_tag": "targetTags", "target_service_account": "targetServiceAccounts"} {
		if l := strsAny(r.o[f]); len(l) > 0 {
			details[k] = anyList(l)
		}
	}
	payload := map[string]any{
		"connection": map[string]any{"src_ip": src.Addr().String(), "src_port": int(src.Port()),
			"dest_ip": dst.Addr().String(), "dest_port": int(dst.Port()), "protocol": protoNum},
		"disposition":  disposition,
		"rule_details": details,
	}
	subRel, region := "", ""
	if nics := sliceOf(inst["networkInterfaces"]); len(nics) > 0 {
		nic, _ := nics[0].(map[string]any)
		subRel = str(nic, "subnetwork")
	}
	if parts := strings.Split(subRel, "/"); len(parts) == 6 {
		region = parts[3]
	} else if i := strings.LastIndex(self.zone, "-"); i > 0 {
		region = self.zone[:i]
	}
	if str(lc, "metadata") != "EXCLUDE_ALL_METADATA" {
		payload["instance"] = map[string]any{"project_id": self.project, "vm_name": self.name, "region": region, "zone": self.zone}
		vpc := map[string]any{"project_id": netProject, "vpc_name": lastSeg(self.network)}
		if subRel != "" {
			vpc["subnetwork_name"] = lastSeg(subRel)
		}
		payload["vpc"] = vpc
	}
	labels := map[string]string{"project_id": netProject, "location": region}
	if sub, ok := s.res[subRel]; ok {
		labels["subnetwork_name"], labels["subnetwork_id"] = lastSeg(subRel), str(sub, "id")
	}
	return &FirewallLogEntry{Project: netProject, Labels: labels, Payload: payload, Time: now}
}

func anyList(l []string) []any {
	out := make([]any, len(l))
	for i, v := range l {
		out[i] = v
	}
	return out
}

// writeFirewallLog hands a record to Cloud Logging, outside the lock.
func (s *Server) writeFirewallLog(e *FirewallLogEntry) {
	if e != nil && s.opts.FirewallLog != nil {
		s.opts.FirewallLog(*e)
	}
}

// pscNATAddressLocked is the address of a Private Service Connect
// endpoint's connection in its service attachment's NAT subnet, which the
// producer's load balancer sees as the connection's source, as Google
// translates a consumer's packets (configure-private-service-connect-
// producer, "NAT subnets"): one address per endpoint, chosen here by the
// endpoint's connection ID among the subnet's usable addresses.
func (s *Server) pscNATAddressLocked(endpoint obj, att obj) (netip.Addr, bool) {
	for _, n := range strs(att, "natSubnets") {
		p, err := netip.ParsePrefix(str(s.res[n], "ipCidrRange"))
		if err != nil || !p.Addr().Is4() || p.Bits() > 29 {
			continue
		}
		size := uint32(1) << (32 - p.Bits())
		h := fnv.New32a()
		h.Write([]byte(fmt.Sprint(endpoint["pscConnectionId"], str(endpoint, "selfLink"))))
		// .0 and .1 and the last two addresses are reserved.
		off := 2 + h.Sum32()%(size-4)
		b := p.Masked().Addr().As4()
		v := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3]) + off
		return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)}), true
	}
	return netip.Addr{}, false
}
