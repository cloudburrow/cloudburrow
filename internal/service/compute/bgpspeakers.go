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

// Cloud Router's BGP sessions with router appliances (#2140,
// network-connectivity/docs/router-appliance/overview). A router
// interface on a subnet (checkInterfaces) is one BGP speaker: GoBGP's
// gobgpd, in a pod on the network's VXLAN segment at the interface's
// privateIpAddress, as a VM's guest (vpc.go) or a Cloud Run gateway
// (gateways.go) is. The VMs see it as one more member of their network: a
// peer with a forwarding entry and a source their firewall rules decide on,
// so the appliance's rules must allow TCP 179 from the interface's address,
// as Google's documentation says they must.
//
// The speakers are derived here from the routers, whole, each time they
// are asked for (BGPSpeakers): an interface with BGP peers on a router
// with an ASN. Each one's gobgpd configuration (gobgpdConfig) peers with
// the interface's peers at their peerIpAddress and peerAsn, with the
// router's keepaliveInterval (hold time three times it) and MD5 key, and
// exports to each peer exactly what getRouterStatus says it is advertised
// (advertisedFor), with the peer's advertisedRoutePriority as the MED, as
// Google sets it. The Run side (cmd/cloudburrow/computebgp.go) runs the
// pods, puts those prefixes in each gobgpd's RIB and reports what the
// sessions are and what each peer advertised (SetBGPState).
//
// What the peers advertise becomes the network's dynamic routes: each VM
// of the network in the router's region (every region when the network's
// routing mode is GLOBAL) is given a `routevia PREFIX NEXTHOP` line, the
// best route to each prefix by priority (the MED the peer sent), its next
// hop the appliance, which must be running and forward IP (canIpForward),
// as Google drops what a VM without it would be sent. Prefixes that equal
// or fall within one of the network's subnet ranges are not programmed:
// subnet routes cannot be overridden by dynamic routes.

// BGPSpeaker is one router interface's BGP speaker.
type BGPSpeaker struct {
	// Key names it: the router's relative name, "/", the interface's name.
	Key string
	// Router, Network and Subnetwork are relative names; Region the
	// router's; Interface the interface's name.
	Router, Interface, Network, Subnetwork, Region string
	// IP is the interface's privateIpAddress, where the speaker is.
	IP string
	// Token is what its pod polls under (GuestPrefix TOKEN/cloudburrow/vpc).
	Token string
	// ASN is the router's.
	ASN uint32
	// Config is its gobgpd configuration (TOML).
	Config string
	// Advertise are the prefixes its RIB holds, its next hop IP; the
	// configuration's export policy decides which peer is sent which.
	Advertise []string
	// Peers are the peers' addresses, by peer name.
	Peers map[string]string
}

// BGPRoute is a route a peer advertised.
type BGPRoute struct {
	Prefix, NextHop string
	// MED is the multi-exit discriminator, -1 when the peer sent none.
	MED    int64
	ASPath []uint32
}

// BGPNeighbor is one session as gobgpd reports it.
type BGPNeighbor struct {
	Address string
	// State is the BGP FSM state, 1 (Idle) to 6 (Established).
	State   int
	Up      time.Time // when the session came up, if Established
	Learned []BGPRoute
	// MD5Failed is a session gobgpd could not set its MD5 key on: the
	// kernel has no TCP MD5 signatures (Docker Desktop's has none).
	MD5Failed bool
}

// BGPState is what a speaker's gobgpd reported last.
type BGPState struct {
	Neighbors []BGPNeighbor
	Seen      time.Time
}

func bgpToken(key string) string {
	h := sha256.Sum256([]byte("speaker:" + key))
	return "bgp" + hex.EncodeToString(h[:12])
}

// bgpRel is the relative name a speaker's vmNet and pod entry use; it is
// not a resource and no list shows it.
func bgpRel(key string) string { return "cloudburrow/bgp/" + bgpToken(key) }

// bgpKeepalive is a router's keepaliveInterval: 20 seconds unless set.
func bgpKeepalive(r obj) int {
	if b, ok := r["bgp"].(map[string]any); ok {
		if v, ok := b["keepaliveInterval"].(float64); ok && v > 0 {
			return int(v)
		}
	}
	return 20
}

// BGPSpeakers are the routers' speakers, sorted by key.
func (s *Server) BGPSpeakers() []BGPSpeaker {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.speakersLocked()
}

func (s *Server) speakersLocked() []BGPSpeaker {
	var out []BGPSpeaker
	for k, r := range s.res {
		if !strings.Contains(k, "/routers/") || strings.Count(k, "/") != 5 {
			continue
		}
		b, _ := r["bgp"].(map[string]any)
		asn, _ := b["asn"].(float64)
		if asn == 0 {
			continue
		}
		keys := map[string]string{}
		for _, e := range sliceOf(r["md5AuthenticationKeys"]) {
			keys[str(objOf(e), "name")] = str(objOf(e), "_key")
		}
		adv := s.advertisedLocked(r)
		for _, ie := range sliceOf(r["interfaces"]) {
			iface := objOf(ie)
			ip := str(iface, "privateIpAddress")
			if str(iface, "subnetwork") == "" || ip == "" {
				continue
			}
			var peers []obj
			for _, pe := range sliceOf(r["bgpPeers"]) {
				if p := objOf(pe); str(p, "interfaceName") == str(iface, "name") && str(p, "peerIpAddress") != "" {
					peers = append(peers, p)
				}
			}
			if len(peers) == 0 {
				continue
			}
			key := k + "/" + str(iface, "name")
			sp := BGPSpeaker{Key: key, Router: k, Interface: str(iface, "name"), Network: str(r, "network"),
				Subnetwork: str(iface, "subnetwork"), Region: lastSeg(str(r, "region")), IP: ip, Token: bgpToken(key),
				ASN: uint32(asn), Peers: map[string]string{}}
			sp.Config, sp.Advertise = gobgpdConfig(sp, r, peers, keys, adv)
			for _, p := range peers {
				sp.Peers[str(p, "name")] = str(p, "peerIpAddress")
			}
			out = append(out, sp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// gobgpdConfig is sp's gobgpd configuration and the prefixes its RIB is
// given: a neighbor per enabled peer, and a global export policy with a
// statement per peer that accepts, for that peer only, what it is
// advertised, setting its MED; everything else is rejected.
func gobgpdConfig(sp BGPSpeaker, r obj, peers []obj, keys map[string]string, subnets []string) (string, []string) {
	var b strings.Builder
	q := strconv.Quote
	ka := bgpKeepalive(r)
	fmt.Fprintf(&b, "[global.config]\n  as = %d\n  router-id = %s\n", sp.ASN, q(sp.IP))
	b.WriteString("[global.apply-policy.config]\n  export-policy-list = [\"advertise\"]\n  default-export-policy = \"reject-route\"\n")
	all := map[string]bool{}
	var sets, stmts strings.Builder
	for i, p := range peers {
		if str(p, "enable") == "FALSE" {
			continue
		}
		pa, _ := p["peerAsn"].(float64)
		fmt.Fprintf(&b, "[[neighbors]]\n  [neighbors.config]\n    neighbor-address = %s\n    peer-as = %d\n    description = %s\n",
			q(str(p, "peerIpAddress")), uint32(pa), q(str(p, "name")))
		if k := keys[str(p, "md5AuthenticationKeyName")]; k != "" {
			fmt.Fprintf(&b, "    auth-password = %s\n", q(k))
		}
		fmt.Fprintf(&b, "  [neighbors.timers.config]\n    keepalive-interval = %d\n    hold-time = %d\n    connect-retry = 5\n", ka, 3*ka)
		fmt.Fprintf(&b, "  [neighbors.transport.config]\n    local-address = %s\n", q(sp.IP))
		prefixes := advertisedFor(p, r, subnets)
		if len(prefixes) == 0 {
			continue
		}
		name := "peer" + strconv.Itoa(i)
		fmt.Fprintf(&sets, "[[defined-sets.neighbor-sets]]\n  neighbor-set-name = %s\n  neighbor-info-list = [%s]\n", q(name), q(str(p, "peerIpAddress")+"/32"))
		fmt.Fprintf(&sets, "[[defined-sets.prefix-sets]]\n  prefix-set-name = %s\n", q(name))
		for _, c := range prefixes {
			_, n, err := net.ParseCIDR(c)
			if err != nil || n.IP.To4() == nil {
				continue
			}
			ones, _ := n.Mask.Size()
			fmt.Fprintf(&sets, "  [[defined-sets.prefix-sets.prefix-list]]\n    ip-prefix = %s\n    masklength-range = \"%d..%d\"\n", q(n.String()), ones, ones)
			all[n.String()] = true
		}
		med := bgpDefaultPrio
		if v, ok := p["advertisedRoutePriority"].(float64); ok {
			med = int(v)
		}
		fmt.Fprintf(&stmts, "  [[policy-definitions.statements]]\n    name = %s\n", q(name))
		fmt.Fprintf(&stmts, "    [policy-definitions.statements.conditions.match-neighbor-set]\n      neighbor-set = %s\n", q(name))
		fmt.Fprintf(&stmts, "    [policy-definitions.statements.conditions.match-prefix-set]\n      prefix-set = %s\n", q(name))
		fmt.Fprintf(&stmts, "    [policy-definitions.statements.actions]\n      route-disposition = \"accept-route\"\n")
		fmt.Fprintf(&stmts, "    [policy-definitions.statements.actions.bgp-actions]\n      set-med = \"%d\"\n", med)
	}
	b.WriteString(sets.String())
	b.WriteString("[[policy-definitions]]\n  name = \"advertise\"\n")
	b.WriteString(stmts.String())
	var adv []string
	for c := range all {
		adv = append(adv, c)
	}
	sort.Strings(adv)
	return b.String(), adv
}

// SetBGPState records what the speaker key's gobgpd reported.
func (s *Server) SetBGPState(key string, st BGPState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.bgpStates == nil {
		s.bgpStates = map[string]BGPState{}
	}
	if st.Seen.IsZero() {
		st.Seen = s.opts.Now()
	}
	s.bgpStates[key] = st
}

// bgpNeighborLocked is the session of speaker key with peer address, if
// the speaker reported one lately.
func (s *Server) bgpNeighborLocked(key, address string) (BGPNeighbor, bool) {
	st, ok := s.bgpStates[key]
	if !ok || s.opts.Now().Sub(st.Seen) > podFresh {
		return BGPNeighbor{}, false
	}
	for _, n := range st.Neighbors {
		if n.Address == address {
			return n, true
		}
	}
	return BGPNeighbor{}, false
}

// learnedRoute is a learned route as the network uses it.
type learnedRoute struct {
	BGPRoute
	router, region, peer string
	priority             int64
	up                   time.Time
}

// learnedLocked are the routes network's routers' established sessions
// learned, the network's own subnet ranges (and what they hold) left out.
func (s *Server) learnedLocked(network string) []learnedRoute {
	var subnets []*net.IPNet
	for k, o := range s.res {
		if strings.Contains(k, "/subnetworks/") && str(o, "network") == network {
			if _, n, err := net.ParseCIDR(str(o, "ipCidrRange")); err == nil {
				subnets = append(subnets, n)
			}
		}
	}
	var out []learnedRoute
	for _, sp := range s.speakersLocked() {
		if sp.Network != network {
			continue
		}
		r := s.res[sp.Router]
		for _, pe := range sliceOf(r["bgpPeers"]) {
			p := objOf(pe)
			if str(p, "interfaceName") != sp.Interface || str(p, "enable") == "FALSE" {
				continue
			}
			n, ok := s.bgpNeighborLocked(sp.Key, str(p, "peerIpAddress"))
			if !ok || n.State != 6 {
				continue
			}
			for _, lr := range n.Learned {
				_, pn, err := net.ParseCIDR(lr.Prefix)
				if err != nil || pn.IP.To4() == nil {
					continue
				}
				inSubnet := false
				ones, _ := pn.Mask.Size()
				for _, sn := range subnets {
					if so, _ := sn.Mask.Size(); so <= ones && sn.Contains(pn.IP) {
						inSubnet = true
					}
				}
				if inSubnet {
					continue
				}
				prio := lr.MED
				if prio < 0 {
					prio = 0
				}
				out = append(out, learnedRoute{BGPRoute: lr, router: sp.Router, region: sp.Region, peer: str(p, "name"),
					priority: prio, up: n.Up})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Prefix != b.Prefix {
			return a.Prefix < b.Prefix
		}
		if a.priority != b.priority {
			return a.priority < b.priority
		}
		return a.NextHop < b.NextHop
	})
	return out
}

// bestLearnedLocked are network's best dynamic route to each prefix for
// region: routes learned in the region, or in any region when the
// network's routing mode is GLOBAL; the lowest priority wins, then the
// lowest next hop.
func (s *Server) bestLearnedLocked(network, region string) []learnedRoute {
	global := false
	if rc, ok := s.res[network]["routingConfig"].(map[string]any); ok && str(rc, "routingMode") == "GLOBAL" {
		global = true
	}
	seen := map[string]bool{}
	var out []learnedRoute
	for _, lr := range s.learnedLocked(network) {
		if (!global && lr.region != region) || seen[lr.Prefix] {
			continue
		}
		seen[lr.Prefix] = true
		out = append(out, lr)
	}
	return out
}

// routeViaLocked are self's dynamic routes, as `routevia PREFIX NEXTHOP`
// lines: those whose next hop is a running VM of the network that forwards
// IP, other than self.
func (s *Server) routeViaLocked(self vmNet) string {
	if self.gateway || self.speaker {
		return ""
	}
	var b strings.Builder
	for _, lr := range s.bestLearnedLocked(self.network, regionOfZone(self.zone)) {
		if lr.NextHop == self.ip || !s.forwarderLocked(self.network, lr.NextHop) {
			continue
		}
		fmt.Fprintf(&b, "routevia %s %s\n", lr.Prefix, lr.NextHop)
	}
	return b.String()
}

// forwarderLocked is whether ip is a running VM's primary address on
// network with canIpForward.
func (s *Server) forwarderLocked(network, ip string) bool {
	for k, o := range s.res {
		if !strings.Contains(k, "/instances/") || o["canIpForward"] != true {
			continue
		}
		if v, ok := vmNetOf(k, o); ok && v.network == network && v.ip == ip && v.running {
			return true
		}
	}
	return false
}

// speakerVMNetsLocked are the speakers as members of their networks.
func (s *Server) speakerVMNetsLocked() []vmNet {
	var out []vmNet
	for _, sp := range s.speakersLocked() {
		out = append(out, vmNet{rel: bgpRel(sp.Key), name: "cloud-router", network: sp.Network, ip: sp.IP,
			running: true, project: strings.Split(sp.Network, "/")[1], zone: sp.Region + "-a", speaker: true})
	}
	return out
}

// speakerByTokenLocked is the speaker that polls under token.
func (s *Server) speakerByTokenLocked(token string) (BGPSpeaker, bool) {
	if !strings.HasPrefix(token, "bgp") {
		return BGPSpeaker{}, false
	}
	for _, sp := range s.speakersLocked() {
		if sp.Token == token {
			return sp, true
		}
	}
	return BGPSpeaker{}, false
}

// serveSpeakerVPC answers a speaker's poll: its segment and peers as a
// VM's guest gets them, and its ruleset (speakerNFT).
func (s *Server) serveSpeakerVPC(w http.ResponseWriter, r *http.Request, sp BGPSpeaker) {
	s.mu.Lock()
	inst := obj{"selfLink": bgpRel(sp.Key), "name": "cloud-router", "zone": sp.Region + "-a", "status": stRunning, "_speaker": sp.Key,
		"networkInterfaces": []any{obj{"network": sp.Network, "subnetwork": sp.Subnetwork, "networkIP": sp.IP, "accessConfigs": []any{}}}}
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

// speakerNFT is a speaker's table: from the network it takes BGP (TCP
// 179) and replies only; gobgpd's API is on loopback.
func speakerNFT(peerPods []string) string {
	var b strings.Builder
	b.WriteString("table inet cloudburrow {\n")
	b.WriteString("  chain ingress {\n    type filter hook input priority filter; policy accept;\n")
	b.WriteString("    iifname \"lo\" accept\n    ct state established,related accept\n    ct state invalid drop\n")
	if len(peerPods) > 0 {
		b.WriteString("    iifname != \"vpc0\" ip saddr " + nftSet(peerPods) + " udp dport 4789 accept\n")
	}
	b.WriteString("    iifname \"vpc0\" tcp dport 179 accept comment \"BGP\"\n")
	b.WriteString("    iifname \"vpc0\" icmp type echo-request accept\n")
	b.WriteString("    counter drop comment \"a Cloud Router interface takes BGP only\"\n  }\n")
	b.WriteString("  chain forward {\n    type filter hook forward priority filter; policy drop;\n  }\n}\n")
	return b.String()
}

// peerStatusLocked fills ps, a getRouterStatus bgpPeerStatus, from the
// session of r's peer m, and reports whether one is known.
func (s *Server) peerStatusLocked(rel string, r obj, m map[string]any, ps obj) bool {
	key := rel + "/" + str(m, "interfaceName")
	n, ok := s.bgpNeighborLocked(key, str(m, "peerIpAddress"))
	if !ok {
		return false
	}
	names := map[int]string{1: "Idle", 2: "Connect", 3: "Active", 4: "OpenSent", 5: "OpenConfirm", 6: "Established"}
	ps["state"] = names[n.State]
	if ps["state"] == "" {
		ps["state"] = "Idle"
	}
	if n.State != 6 {
		if n.MD5Failed {
			ps["statusReason"] = "MD5_AUTH_INTERNAL_PROBLEM"
		}
		return true
	}
	ps["status"] = "UP"
	up := s.opts.Now().Sub(n.Up)
	if up < 0 || n.Up.IsZero() {
		up = 0
	}
	ps["uptime"] = bgpUptime(up)
	ps["uptimeSeconds"] = strconv.FormatInt(int64(up/time.Second), 10)
	ps["numLearnedRoutes"] = float64(len(n.Learned))
	return true
}

// bgpUptime is an uptime as getRouterStatus words it.
func bgpUptime(d time.Duration) string {
	sec := int64(d / time.Second)
	parts := []string{}
	for _, u := range []struct {
		n    int64
		name string
	}{{86400, "days"}, {3600, "hours"}, {60, "minutes"}} {
		if v := sec / u.n; v > 0 || len(parts) > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", v, u.name))
		}
		sec %= u.n
	}
	parts = append(parts, fmt.Sprintf("%d seconds", sec))
	return strings.Join(parts, ", ")
}

// routeObj is a learned route as getRouterStatus lists it.
func (s *Server) routeObj(network string, lr learnedRoute) obj {
	o := obj{"kind": "compute#route", "network": network, "destRange": lr.Prefix, "nextHopIp": lr.NextHop,
		"priority": float64(lr.priority), "routeType": "BGP"}
	if len(lr.ASPath) > 0 {
		var l []any
		for _, a := range lr.ASPath {
			l = append(l, float64(a))
		}
		o["asPaths"] = []any{obj{"pathSegmentType": "AS_SEQUENCE", "asLists": l}}
	}
	if !lr.up.IsZero() {
		o["creationTimestamp"] = lr.up.Format("2006-01-02T15:04:05.000-07:00")
	}
	return o
}

// routerBestRoutesLocked are getRouterStatus' bestRoutes (the network's
// best dynamic routes for the router's region) and bestRoutesForRouter
// (the best of those this router learned).
func (s *Server) routerBestRoutesLocked(rel string, r obj) (best, mine []any) {
	network, region := str(r, "network"), lastSeg(str(r, "region"))
	best, mine = []any{}, []any{}
	for _, lr := range s.bestLearnedLocked(network, region) {
		o := s.routeObj(network, lr)
		best = append(best, o)
		if lr.router == rel {
			mine = append(mine, o)
		}
	}
	return best, mine
}
