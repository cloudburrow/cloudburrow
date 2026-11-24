package compute

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Cloud NAT's address translation (#2140). Each public Cloud NAT gateway
// that translates at least one VM is carried by a pod of its own on its
// network's VXLAN segment, like a VM's guest (vpc.go), which:
//
//   - holds the gateway address of the first subnet it translates (the
//     range's second address, which Google reserves and never gives a VM),
//     and each translated VM routes 0.0.0.0/1 and 128.0.0.0/1 to its own
//     subnet's gateway address, which resolves to the pod: the VM's
//     "default internet gateway" next hop is the NAT gateway, as on Google.
//     Private ranges, the cluster's and the metadata server stay on the
//     VM's pod interface;
//   - translates each VM's TCP and UDP source to the NAT IP and the static
//     port range getNatMappingInfo gives it (minPortsPerVm ports from
//     1024, natAllocLocked), and its ICMP to the NAT IP;
//   - applies the gateway's idle and TIME_WAIT timeouts to its conntrack
//     table;
//   - drops what a VM with no ports (no NAT IP left for it) sends, noting
//     each dropped connection in an nftables set for the DROPPED log.
//
// The kind nodes route each NAT IP to the pod (cmd/cloudburrow/computenat.go)
// so replies come back to it; the node masquerades what leaves the
// cluster, so a destination sees this machine's address, not the NAT IP.
// The same loop reads each pod's conntrack table and the dropped set and
// writes compute.googleapis.com/nat_flows entries per the gateway's
// logConfig.
//
// The gateways are computed from the routers whenever asked (natGatewaysLocked),
// keyed by router and name; nothing is kept but the pods' last reported
// IPs, under natRel.

// NATGateway is a Cloud NAT gateway as its pod runs it.
type NATGateway struct {
	// Key is "projects/P/regions/R/routers/ROUTER/nats/NAT".
	Key                                     string
	Project, Region, Router, RouterID, Name string
	// Network is the router's network's relative name.
	Network string
	// IP is the pod's address on the network; Token is what it polls under
	// (GuestPrefix TOKEN/cloudburrow/vpc); Pod is its pod IP once it has
	// polled.
	IP, Token, Pod string
	// NatIPs are the gateway's active NAT IPs.
	NatIPs []string
	// Log is logConfig.enable; LogFilter is logConfig.filter.
	Log       bool
	LogFilter string
	// VMs are the VMs it translates.
	VMs []NATVM
	// EIM is enableEndpointIndependentMapping; Dynamic is
	// enableDynamicPortAllocation.
	EIM, Dynamic bool
}

// NATVM is a VM a gateway translates and the ports it was given; NatIP is
// "" when no NAT IP has ports left for it.
type NATVM struct {
	Name, Zone, Subnetwork, IP string
	NatIP                      string
	PortLo, PortHi             int
}

func natGatewayKey(router, name string) string { return router + "/nats/" + name }

// natRel is the relative name a NAT gateway's pod entry and vmNet use; it
// is not a resource and no list shows it.
func natRel(key string) string { return "cloudburrow/natgateways/" + natToken(key) }

func natToken(key string) string {
	h := sha256.Sum256([]byte("nat:" + key))
	return "nt" + hex.EncodeToString(h[:12])
}

// natSlot is one VM's static allocation: ports lo to hi of ip, or none.
type natSlot struct {
	vm     natVM
	ip     string
	lo, hi int
}

// natAllocLocked is gateway m's port allocation. With static allocation
// each VM it translates, in order, is given minPortsPerVm ports of a NAT
// IP from port 1024, natPortsPerIP/minPortsPerVm VMs per address. With
// dynamic port allocation each VM is given its current number of ports
// (natDynPortsOf: minPortsPerVm, doubled as it nears exhausting them, up
// to maxPortsPerVm, and halved again once it uses few), a block aligned
// to its size, first fit over the gateway's addresses in order. An
// AUTO_ONLY gateway is given more addresses as its VMs need them, as
// Google allocates them; a MANUAL_ONLY gateway's VMs past its addresses
// get none (minExtraNatIpsNeeded).
func (s *Server) natAllocLocked(r obj, m map[string]any) (ips []string, slots []natSlot) {
	ips, _ = s.natIPsOf(m)
	vms := s.natVMsLocked(r, m)
	if m["enableDynamicPortAllocation"] == true {
		return s.natDynAllocLocked(m, ips, vms)
	}
	ports := natMinPorts(m)
	per := natPortsPerIP / ports
	if per < 1 {
		per = 1
	}
	if str(m, "natIpAllocateOption") == natAutoOnly {
		for need := (len(vms) + per - 1) / per; len(ips) < need; {
			ip := s.freeAutoNatIPLocked()
			if ip == "" {
				break
			}
			m["_autoIps"] = append(sliceOf(m["_autoIps"]), ip)
			ips = append(ips, ip)
		}
	}
	for i, vm := range vms {
		sl := natSlot{vm: vm}
		if i/per < len(ips) {
			sl.ip = ips[i/per]
			sl.lo = 1024 + (i%per)*ports
			sl.hi = sl.lo + ports - 1
		}
		slots = append(slots, sl)
	}
	return ips, slots
}

// natDynAllocLocked is natAllocLocked with dynamic port allocation.
func (s *Server) natDynAllocLocked(m map[string]any, ips []string, vms []natVM) ([]string, []natSlot) {
	type block struct{ lo, hi int }
	used := map[string][]block{}
	free := func(ip string, lo, hi int) bool {
		for _, b := range used[ip] {
			if lo <= b.hi && b.lo <= hi {
				return false
			}
		}
		return true
	}
	// A VM keeps the block it was given (_dynAt, "NATIP:LO-HI") while it
	// is still free and aligned to its size, so another VM's growth or
	// shrinking never moves it: those whose size is unchanged first, then
	// those that grew or shrank in place where they can; the others are
	// placed first fit.
	at := mapOf(m["_dynAt"])
	slots := make([]natSlot, len(vms))
	for i, vm := range vms {
		slots[i].vm = vm
	}
	for _, same := range []bool{true, false} {
		for i, vm := range vms {
			ip, rng, ok := strings.Cut(str(at, vm.ip), ":")
			a, b, _ := strings.Cut(rng, "-")
			n, err1 := strconv.Atoi(a)
			h, err2 := strconv.Atoi(b)
			size := natDynPortsOf(m, vm.ip)
			if slots[i].ip != "" || !ok || err1 != nil || err2 != nil || (h-n+1 == size) != same ||
				(n-1024)%size != 0 || n-1024+size > natPortsPerIP || !hasStr(ips, ip) || !free(ip, n, n+size-1) {
				continue
			}
			slots[i].ip, slots[i].lo, slots[i].hi = ip, n, n+size-1
			used[ip] = append(used[ip], block{n, n + size - 1})
		}
	}
	for k := range slots {
		sl := &slots[k]
		if sl.ip != "" {
			continue
		}
		size := natDynPortsOf(m, sl.vm.ip)
		for i := 0; sl.ip == ""; i++ {
			if i == len(ips) {
				if str(m, "natIpAllocateOption") != natAutoOnly {
					break
				}
				ip := s.freeAutoNatIPLocked()
				if ip == "" {
					break
				}
				m["_autoIps"] = append(sliceOf(m["_autoIps"]), ip)
				ips = append(ips, ip)
			}
			for off := 0; off+size <= natPortsPerIP; off += size {
				if lo, hi := 1024+off, 1024+off+size-1; free(ips[i], lo, hi) {
					sl.ip, sl.lo, sl.hi = ips[i], lo, hi
					used[ips[i]] = append(used[ips[i]], block{lo, hi})
					break
				}
			}
		}
	}
	next := map[string]any{}
	for _, sl := range slots {
		if sl.ip != "" && sl.vm.ip != "" {
			next[sl.vm.ip] = fmt.Sprintf("%s:%d-%d", sl.ip, sl.lo, sl.hi)
		}
	}
	if len(next) == 0 {
		delete(m, "_dynAt")
	} else {
		m["_dynAt"] = next
	}
	return ips, slots
}

// natMaxPorts is a dynamic gateway's maxPortsPerVm (Google's default
// 65536), as one block of an address: at most its largest power of 2.
func natMaxPorts(m map[string]any) int {
	max := natDefaultMaxDyn
	if v, ok := m["maxPortsPerVm"].(float64); ok && v > 0 {
		max = int(v)
	}
	if max > natMaxBlock {
		max = natMaxBlock
	}
	return max
}

// natMaxBlock is the largest power of 2 of an address's 64512 ports.
const natMaxBlock = 32768

// natDynPortsOf is how many ports a dynamic gateway's VM at ip has now.
func natDynPortsOf(m map[string]any, ip string) int {
	n := natMinPorts(m)
	if v, ok := mapOf(m["_dynPorts"])[ip].(float64); ok && int(v) > n {
		n = int(v)
	}
	if max := natMaxPorts(m); n > max {
		n = max
	}
	return n
}

// natShrinkAfter is how long a VM uses at most a quarter of its dynamic
// ports before they are halved, and natGrowAt the share of them in use
// (7/8) at which they are doubled: "If a VM is close to exhausting all
// the ports that are allocated to it, the number of ports assigned to the
// VM is doubled", and "when the port usage significantly decreases, the
// ports are deallocated" (nat/docs/ports-and-addresses).
const (
	natShrinkAfter = 30 * time.Second
	natGrowNum     = 7
	natGrowDen     = 8
)

// SetNATPortUsage is what the gateway key's pod reports: how many NAT
// ports each VM, by internal IP, has in use (its connections' distinct NAT
// ports, the larger of TCP's and UDP's). A gateway with dynamic port
// allocation doubles the ports of a VM close to exhausting them, up to
// maxPortsPerVm, and halves those of a VM that has used at most a quarter
// of them for natShrinkAfter, down to minPortsPerVm. It reports whether
// an allocation changed.
func (s *Server) SetNATPortUsage(key string, inUse map[string]int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	router, name, _ := strings.Cut(key, "/nats/")
	r, ok := s.res[router]
	if !ok {
		return false
	}
	var m map[string]any
	for _, e := range sliceOf(r["nats"]) {
		if nm, _ := e.(map[string]any); str(nm, "name") == name {
			m = nm
		}
	}
	if m == nil || m["enableDynamicPortAllocation"] != true {
		return false
	}
	if s.natLow == nil {
		s.natLow = map[string]time.Time{}
	}
	dyn := mapOf(m["_dynPorts"])
	if dyn == nil {
		dyn = map[string]any{}
	}
	now := s.opts.Now()
	min, max := natMinPorts(m), natMaxPorts(m)
	changed := false
	live := map[string]bool{}
	for _, vm := range s.natVMsLocked(r, m) {
		if vm.ip == "" {
			continue
		}
		live[vm.ip] = true
		have := natDynPortsOf(m, vm.ip)
		used, lowKey := inUse[vm.ip], key+"|"+vm.ip
		next := have
		switch {
		case used*natGrowDen >= have*natGrowNum && have < max:
			next = have * 2
			delete(s.natLow, lowKey)
		case used*4 <= have && have > min:
			since, ok := s.natLow[lowKey]
			if !ok {
				s.natLow[lowKey] = now
			} else if now.Sub(since) >= natShrinkAfter {
				next = have / 2
				s.natLow[lowKey] = now
			}
		default:
			delete(s.natLow, lowKey)
		}
		if next != have {
			changed = true
			if next <= min {
				delete(dyn, vm.ip)
			} else {
				dyn[vm.ip] = float64(next)
			}
		}
	}
	for ip := range dyn {
		if !live[ip] {
			delete(dyn, ip)
			changed = true
		}
	}
	if !changed {
		return false
	}
	if len(dyn) == 0 {
		delete(m, "_dynPorts")
	} else {
		m["_dynPorts"] = dyn
	}
	_ = s.saveLocked()
	return true
}

// natNeed is how many NAT IPs m's VMs need.
func natNeed(m map[string]any, vms int) int {
	per := natPortsPerIP / natMinPorts(m)
	if per < 1 {
		per = 1
	}
	return (vms + per - 1) / per
}

// freeAutoNatIPLocked is an address of the local pool that no address
// resource, access config or AUTO_ONLY gateway holds.
func (s *Server) freeAutoNatIPLocked() string {
	used := s.externalInUseLocked()
	for ip := range s.natAutoIPsLocked() {
		used[ip] = true
	}
	lo := ip4(regionalPool.IP)
	for v := lo + 1; v < lo+rangeSize(regionalPool)-1; v++ {
		if ip := fromU32(v).String(); !used[ip] {
			return ip
		}
	}
	return ""
}

// natGatewaysLocked are the public gateways that translate at least one
// VM, sorted by key.
func (s *Server) natGatewaysLocked() []NATGateway {
	var out []NATGateway
	for k, r := range s.res {
		if !strings.Contains(k, "/routers/") {
			continue
		}
		for _, e := range sliceOf(r["nats"]) {
			m, _ := e.(map[string]any)
			if natTypeOf(m) != natTypePublic {
				continue
			}
			ips, slots := s.natAllocLocked(r, m)
			if len(slots) == 0 {
				continue
			}
			key := natGatewayKey(k, str(m, "name"))
			g := NATGateway{Key: key, Project: strings.Split(k, "/")[1], Region: lastSeg(str(r, "region")), Router: str(r, "name"),
				RouterID: str(r, "id"), Name: str(m, "name"), Network: str(r, "network"), Token: natToken(key), NatIPs: ips,
				EIM: m["enableEndpointIndependentMapping"] == true, Dynamic: m["enableDynamicPortAllocation"] == true}
			if lc, ok := m["logConfig"].(map[string]any); ok {
				g.Log, g.LogFilter = lc["enable"] == true, str(lc, "filter")
			}
			for _, sl := range slots {
				g.VMs = append(g.VMs, NATVM{Name: sl.vm.name, Zone: sl.vm.zone, Subnetwork: sl.vm.subnet, IP: sl.vm.ip,
					NatIP: sl.ip, PortLo: sl.lo, PortHi: sl.hi})
				if g.IP == "" {
					g.IP = subnetGatewayOf(s.res[sl.vm.subnet])
				}
			}
			if g.IP == "" {
				continue
			}
			if p, ok := s.pods[natRel(key)]; ok && s.opts.Now().Sub(p.seen) < podFresh {
				g.Pod = p.ip
			}
			out = append(out, g)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// subnetGatewayOf is a subnet's gateway address: its primary range's
// second address.
func subnetGatewayOf(sub obj) string {
	if g := str(sub, "gatewayAddress"); g != "" {
		return g
	}
	_, n, err := net.ParseCIDR(str(sub, "ipCidrRange"))
	if err != nil || n.IP.To4() == nil {
		return ""
	}
	return gateway(n)
}

// NATGateways are the NAT gateways to run, with their pods' IPs as last
// polled. AUTO_ONLY addresses given since the last call are saved.
func (s *Server) NATGateways() []NATGateway {
	s.mu.Lock()
	defer s.mu.Unlock()
	before := fmt.Sprint(s.natAutoIPsLocked(), s.natDynAtLocked())
	out := s.natGatewaysLocked()
	if fmt.Sprint(s.natAutoIPsLocked(), s.natDynAtLocked()) != before {
		_ = s.saveLocked()
	}
	return out
}

// natGatewayByTokenLocked is the gateway that polls under token.
func (s *Server) natGatewayByTokenLocked(token string) (NATGateway, bool) {
	if !strings.HasPrefix(token, "nt") {
		return NATGateway{}, false
	}
	for _, g := range s.natGatewaysLocked() {
		if g.Token == token {
			return g, true
		}
	}
	return NATGateway{}, false
}

// natVMNetsLocked are the gateways' pods as members of their networks.
func (s *Server) natVMNetsLocked() []vmNet {
	var out []vmNet
	for _, g := range s.natGatewaysLocked() {
		out = append(out, vmNet{rel: natRel(g.Key), name: "nat", network: g.Network, ip: g.IP, running: true,
			project: g.Project, zone: g.Region + "-a", gateway: true})
	}
	return out
}

// serveNATVPC answers a NAT gateway pod's poll: its segment and peers as a
// VM's guest gets them, and its ruleset (natNFT).
func (s *Server) serveNATVPC(w http.ResponseWriter, r *http.Request, g NATGateway) {
	s.mu.Lock()
	inst := obj{"selfLink": natRel(g.Key), "name": "nat", "zone": g.Region + "-a", "status": stRunning, "_natGateway": g.Key,
		"networkInterfaces": []any{obj{"network": g.Network, "networkIP": g.IP, "accessConfigs": []any{}}}}
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

// natPodConfigLocked is the NAT gateway pod key's part of its
// configuration: it forwards, its conntrack timeouts, and its table.
func (s *Server) natPodConfigLocked(key string, peerPods []string) (lines, table string) {
	var g NATGateway
	var m map[string]any
	for _, x := range s.natGatewaysLocked() {
		if x.Key == key {
			g = x
		}
	}
	router, name, _ := strings.Cut(key, "/nats/")
	for _, e := range sliceOf(s.res[router]["nats"]) {
		if nm, _ := e.(map[string]any); str(nm, "name") == name {
			m = nm
		}
	}
	var b strings.Builder
	b.WriteString("forward\n")
	for _, t := range natSysctls(m) {
		fmt.Fprintf(&b, "sysctl %s %d\n", t.name, t.sec)
	}
	return b.String(), natNFT(g, peerPods)
}

type natSysctl struct {
	name string
	sec  int
}

// natSysctls are the pod's conntrack timeouts: the gateway's (or Google's
// defaults, NATTimeoutDefaults) for UDP, ICMP, established TCP, transitory
// TCP (opening and closing states) and TIME_WAIT.
func natSysctls(m map[string]any) []natSysctl {
	sec := func(k string) int {
		if v, ok := m[k].(float64); ok && v > 0 {
			return int(v)
		}
		return NATTimeoutDefaults[k]
	}
	var out []natSysctl
	add := func(k string, names ...string) {
		for _, n := range names {
			out = append(out, natSysctl{"nf_conntrack_" + n, sec(k)})
		}
	}
	add("udpIdleTimeoutSec", "udp_timeout", "udp_timeout_stream")
	add("icmpIdleTimeoutSec", "icmp_timeout")
	add("tcpEstablishedIdleTimeoutSec", "tcp_timeout_established")
	add("tcpTransitoryIdleTimeoutSec", "tcp_timeout_syn_sent", "tcp_timeout_syn_recv", "tcp_timeout_fin_wait",
		"tcp_timeout_close_wait", "tcp_timeout_last_ack", "tcp_timeout_close")
	add("tcpTimeWaitTimeoutSec", "tcp_timeout_time_wait")
	return out
}

// NATDroppedSet is the nftables set of a NAT gateway pod's table that
// notes each connection dropped for want of ports: source IP, source
// port, destination IP, destination port, protocol.
const NATDroppedSet = "dropped"

// natNFT is a NAT gateway pod's table: it takes nothing new itself
// (replies and the segment only); it forwards what its VMs send onto its
// pod interface, translated to each VM's NAT IP and ports, and drops (and
// notes) what a VM with no ports sends.
func natNFT(g NATGateway, peerPods []string) string {
	var b strings.Builder
	b.WriteString("table inet cloudburrow {\n")
	fmt.Fprintf(&b, "  set %s {\n    type ipv4_addr . inet_service . ipv4_addr . inet_service . inet_proto\n    flags dynamic,timeout\n    timeout 2m\n  }\n", NATDroppedSet)
	b.WriteString("  chain ingress {\n    type filter hook input priority filter; policy accept;\n")
	b.WriteString("    iifname \"lo\" accept\n    ct state established,related accept\n    ct state invalid drop\n")
	if len(peerPods) > 0 {
		b.WriteString("    iifname != \"vpc0\" ip saddr " + nftSet(peerPods) + " udp dport 4789 accept\n")
	}
	b.WriteString("    counter drop comment \"a NAT gateway takes no connection\"\n  }\n")
	var ok, none []string
	for _, v := range g.VMs {
		if v.NatIP != "" {
			ok = append(ok, v.IP)
		} else {
			none = append(none, v.IP)
		}
	}
	b.WriteString("  chain forward {\n    type filter hook forward priority filter; policy drop;\n")
	b.WriteString("    ct state established,related accept\n")
	if len(ok) > 0 {
		b.WriteString("    iifname \"vpc0\" oifname \"eth0\" ip saddr " + nftSet(ok) + " accept comment \"translated\"\n")
	}
	if len(none) > 0 {
		fmt.Fprintf(&b, "    iifname \"vpc0\" oifname \"eth0\" ip saddr %s meta l4proto { tcp, udp } update @%s { ip saddr . th sport . ip daddr . th dport . meta l4proto } counter drop comment \"no NAT ports\"\n", nftSet(none), NATDroppedSet)
	}
	b.WriteString("    counter drop\n  }\n")
	b.WriteString("  chain postrouting {\n    type nat hook postrouting priority srcnat; policy accept;\n")
	for _, v := range g.VMs {
		if v.NatIP == "" {
			continue
		}
		if g.EIM {
			// Endpoint-Independent Mapping: an internal IP:port is mapped to
			// one NAT port whatever its destination, a hash of the pair into
			// the VM's range; a second internal port whose NAT port another
			// connection to the same destination holds is dropped, the
			// endpoint-independent conflict of nat/docs/ports-and-addresses.
			// (nftables types a hash as an integer: a map turns it into
			// the port.)
			n := v.PortHi - v.PortLo + 1
			fmt.Fprintf(&b, "    oifname \"eth0\" ip saddr %s meta l4proto { tcp, udp } snat ip to %s : jhash ip saddr . th sport mod %d map { ", v.IP, v.NatIP, n)
			for i := 0; i < n; i++ {
				if i > 0 {
					b.WriteString(", ")
				}
				fmt.Fprintf(&b, "%d : %d", i, v.PortLo+i)
			}
			fmt.Fprintf(&b, " } comment %q\n", v.Name)
		} else {
			// Without it the mapping is per connection: "random" keeps
			// Linux from reusing an internal IP:port's earlier mapping for
			// a new destination (find_appropriate_src), which would be
			// endpoint-independent.
			fmt.Fprintf(&b, "    oifname \"eth0\" ip saddr %s meta l4proto { tcp, udp } snat ip to %s:%d-%d random comment %q\n", v.IP, v.NatIP, v.PortLo, v.PortHi, v.Name)
		}
		fmt.Fprintf(&b, "    oifname \"eth0\" ip saddr %s snat ip to %s\n", v.IP, v.NatIP)
	}
	b.WriteString("  }\n}\n")
	return b.String()
}

// natKeepRanges are what a translated VM still reaches over its pod
// interface: private ranges (this machine's networks, where CloudBurrow's
// APIs are), the cluster's and the metadata server's link-local range.
func natKeepRanges() []string {
	out := append([]string{"169.254.0.0/16"}, PrivateRanges...)
	return append(out, ClusterCIDRs...)
}

// natVMConfigLocked is a VM's part of its configuration when a NAT
// gateway with a running pod translates it: the pod as the neighbour at
// its subnet's gateway address, the internet routed there, and what stays
// on the pod interface. "" otherwise.
func (s *Server) natVMConfigLocked(inst obj, self vmNet) string {
	nics := sliceOf(inst["networkInterfaces"])
	if len(nics) == 0 || self.ip == "" {
		return ""
	}
	nic, _ := nics[0].(map[string]any)
	if len(sliceOf(nic["accessConfigs"])) > 0 {
		return ""
	}
	sub := str(nic, "subnetwork")
	for _, g := range s.natGatewaysLocked() {
		if g.Pod == "" {
			continue
		}
		for _, v := range g.VMs {
			if v.IP != self.ip || v.Subnetwork != sub || g.Network != self.network {
				continue
			}
			gw := subnetGatewayOf(s.res[sub])
			var b strings.Builder
			if gw != g.IP {
				// The pod answers at every subnet's gateway address: its MAC
				// is its own address's.
				fmt.Fprintf(&b, "peer %s %s %s\n", gw, macOf(g.IP), g.Pod)
			}
			fmt.Fprintf(&b, "nat %s\n", gw)
			for _, c := range natKeepRanges() {
				fmt.Fprintf(&b, "natkeep %s\n", c)
			}
			return b.String()
		}
	}
	return ""
}

// natUsage are the NAT IPs some VM was given ports of.
func natUsage(slots []natSlot) map[string]bool {
	used := map[string]bool{}
	for _, sl := range slots {
		if sl.ip != "" {
			used[sl.ip] = true
		}
	}
	return used
}

// mapOf is v as a JSON object, or nil.
func mapOf(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// natDynAtLocked are the dynamic gateways' blocks (_dynAt), by gateway
// key, as fmt prints them in key order.
func (s *Server) natDynAtLocked() map[string]any {
	out := map[string]any{}
	for k, r := range s.res {
		if !strings.Contains(k, "/routers/") {
			continue
		}
		for _, e := range sliceOf(r["nats"]) {
			if m, _ := e.(map[string]any); m["_dynAt"] != nil {
				out[natGatewayKey(k, str(m, "name"))] = fmt.Sprint(m["_dynAt"])
			}
		}
	}
	return out
}
