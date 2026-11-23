package compute

import (
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net"
	"net/netip"
	"sort"
	"strconv"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/vmrelay"
)

// A VM's connections to load balancers and Secure Web Proxy gateways at
// their VPC addresses (#2160), relayed with the VM's own identity
// (internal/vmrelay). Each guest is told, in its network configuration
// (vpc.go), the relay's port on this machine and every VPC address and
// port it may reach: the forwarding rules of the load balancers its
// network (or, for an external one, any network) has, and the Secure Web
// Proxy gateways of its network (Options.Gateways). Its nftables redirect
// each to a local relay, which opens a connection to the relay's port with
// a PROXY protocol version 2 header holding the instance's internal IP,
// the address it dialled, and its guest token. relayResolve trusts that
// header only when the token is a running instance's and the source is
// that instance's internal IP, and hands the connection to the rule's (or
// gateway's) listener as from that IP, which authorization policies'
// source resources and Secure Web Proxy's source.matchServiceAccount() and
// source.matchTag() map to the instance's service account and secure tags.

// RelayGateways are Secure Web Proxy's gateways as a VM's guest reaches
// them.
type RelayGateways interface {
	// RelayAddresses are the address:port pairs of the gateways on network
	// (projects/P/global/networks/N).
	RelayAddresses(network string) []netip.AddrPort
	// RelayInject hands c to the listener of the gateway on network at
	// dst; false when there is none.
	RelayInject(network string, dst netip.AddrPort, c net.Conn) bool
}

// UseGateways sets Options.Gateways after New, before Start.
func (s *Server) UseGateways(g RelayGateways) { s.opts.Gateways = g }

// RelayAddr is the relay's bound address (host:port), "" when it does not
// listen: the address this machine publishes to the cluster for it.
func (s *Server) RelayAddr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.relay == nil {
		return ""
	}
	return s.relay.Addr()
}

// UseRelayAddress sets how a guest reaches the relay: the in-cluster
// host:port this machine publishes it at, "" when unknown (the guest then
// uses its metadata host and the relay's port).
func (s *Server) UseRelayAddress(f func() string) {
	s.mu.Lock()
	s.relayAt = f
	s.mu.Unlock()
}

// relayLocalBase is the first local port a guest's relays listen on (at
// 127.0.0.1), one per VPC address and port.
const relayLocalBase = 47000

// maxRelays bounds how many VPC addresses and ports one guest relays.
const maxRelays = 256

// relayVIP is one VPC address and port a guest relays.
type relayVIP struct {
	dst   netip.AddrPort
	local int
}

// startRelay opens the relay's listener beside the API's, on a port this
// machine chooses.
func (s *Server) startRelay() {
	if s.opts.VMs == nil || s.relay != nil {
		return
	}
	host := s.opts.RelayBind
	if host == "" {
		host = "127.0.0.1"
	}
	r, err := vmrelay.Listen(net.JoinHostPort(host, "0"), s.relayResolve)
	if err != nil {
		log.Printf("compute: the VM relay cannot listen: %v", err)
		return
	}
	r.Refused = func(err error) { log.Printf("compute: a VM's relayed connection was refused: %v", err) }
	s.mu.Lock()
	s.relay = r
	s.mu.Unlock()
}

func (s *Server) stopRelay() {
	s.mu.Lock()
	r := s.relay
	s.relay = nil
	s.mu.Unlock()
	if r != nil {
		_ = r.Close()
	}
}

// relayVIPsLocked are the VPC addresses and ports self's guest relays,
// sorted, each with its local port.
func (s *Server) relayVIPsLocked(self vmNet, reach map[string]bool) []relayVIP {
	seen := map[netip.AddrPort]bool{}
	var dsts []netip.AddrPort
	add := func(a netip.AddrPort) {
		if !a.Addr().Is4() || a.Addr().IsLoopback() || a.Addr().IsUnspecified() || seen[a] {
			return
		}
		seen[a] = true
		dsts = append(dsts, a)
	}
	for k, o := range s.res {
		if o["_lb"] != true || !strings.Contains(k, "/forwardingRules/") || !s.ruleReachableLocked(o, reach) {
			continue
		}
		kind := s.ruleKindLocked(o)
		if kind == "" || kind == "udp" {
			continue
		}
		ip, err := netip.ParseAddr(str(o, "IPAddress"))
		if err != nil {
			continue
		}
		for _, p := range rulePorts(o) {
			add(netip.AddrPortFrom(ip, uint16(p)))
		}
	}
	if g := s.opts.Gateways; g != nil {
		for n := range reach {
			for _, a := range g.RelayAddresses(n) {
				add(a)
			}
		}
	}
	sort.Slice(dsts, func(i, j int) bool { return dsts[i].Compare(dsts[j]) < 0 })
	if len(dsts) > maxRelays {
		dsts = dsts[:maxRelays]
	}
	out := make([]relayVIP, len(dsts))
	for i, d := range dsts {
		out[i] = relayVIP{dst: d, local: relayLocalBase + i}
	}
	return out
}

// ruleReachableLocked is whether a VM on one of the reach networks reaches
// a load-balancing forwarding rule: an internal one on its own (or a
// peered) network, an external one from any.
func (s *Server) ruleReachableLocked(o obj, reach map[string]bool) bool {
	if !strings.HasPrefix(str(o, "loadBalancingScheme"), "INTERNAL") {
		return true
	}
	n := str(o, "network")
	if n == "" {
		if sub, ok := s.res[str(o, "subnetwork")]; ok {
			n = str(sub, "network")
		}
	}
	return reach[n]
}

// relayConfigLocked is the relay's part of a guest's configuration: the
// relay's port, a "relay LOCAL ADDRESS PORT HEADER" line per VPC address
// and port (HEADER the PROXY header, base64), and the nftables chain that
// redirects each to its local relay.
func (s *Server) relayConfigLocked(self vmNet, token string, reach map[string]bool) (text, nft string) {
	if s.relay == nil || token == "" {
		return "", ""
	}
	src, err := netip.ParseAddr(self.ip)
	if err != nil || !src.Is4() {
		return "", ""
	}
	vips := s.relayVIPsLocked(self, reach)
	var b, n strings.Builder
	port, host := strconv.Itoa(s.relay.Port()), ""
	if s.relayAt != nil {
		if h, p, err := net.SplitHostPort(s.relayAt()); err == nil {
			host, port = h, p
		}
	}
	fmt.Fprintf(&b, "relayport %s\n", port)
	if host != "" {
		fmt.Fprintf(&b, "relayhost %s\n", host)
	}
	for _, v := range vips {
		hdr := vmrelay.Encode(vmrelay.Header{Src: netip.AddrPortFrom(src, 0), Dst: v.dst, Token: token})
		fmt.Fprintf(&b, "relay %d %s %d %s\n", v.local, v.dst.Addr(), v.dst.Port(), base64.StdEncoding.EncodeToString(hdr))
	}
	n.WriteString("  chain relay {\n    type nat hook output priority -100; policy accept;\n")
	for _, v := range vips {
		fmt.Fprintf(&n, "    ip daddr %s tcp dport %d redirect to :%d\n", v.dst.Addr(), v.dst.Port(), v.local)
	}
	n.WriteString("  }\n")
	return b.String(), n.String()
}

// relayResolve authenticates a relayed connection: its token must be a
// running instance's, its source that instance's internal IP, and its
// destination a forwarding rule or gateway the instance's network
// reaches.
func (s *Server) relayResolve(h vmrelay.Header) (netip.AddrPort, func(net.Conn) bool, error) {
	s.mu.Lock()
	var inst obj
	for k, o := range s.res {
		if strings.Contains(k, "/instances/") && str(o, "_guestToken") == h.Token {
			inst = o
			break
		}
	}
	if inst == nil || str(inst, "status") != stRunning {
		s.mu.Unlock()
		return netip.AddrPort{}, nil, errors.New("the guest token is no running instance's")
	}
	self, ok := vmNetOf(str(inst, "selfLink"), inst)
	if !ok || self.ip != h.Src.Addr().String() {
		s.mu.Unlock()
		return netip.AddrPort{}, nil, fmt.Errorf("the source %s is not the instance's internal IP", h.Src.Addr())
	}
	reach := map[string]bool{self.network: true}
	for _, n := range s.peeredNetworksLocked(self.network) {
		reach[n] = true
	}
	var rule string
	for k, o := range s.res {
		if o["_lb"] != true || !strings.Contains(k, "/forwardingRules/") || str(o, "IPAddress") != h.Dst.Addr().String() ||
			!s.ruleReachableLocked(o, reach) {
			continue
		}
		for _, p := range rulePorts(o) {
			if p == int(h.Dst.Port()) {
				rule = k
			}
		}
	}
	s.mu.Unlock()
	if rule != "" {
		p := s.lb
		p.mu.Lock()
		var inj *vmrelay.Listener
		if rl := p.listeners[rule]; rl != nil && rl.kind != "udp" {
			for _, l := range rl.ls {
				if l.rulePort == int(h.Dst.Port()) {
					inj = l.inj
				}
			}
		}
		p.mu.Unlock()
		if inj == nil {
			return netip.AddrPort{}, nil, fmt.Errorf("forwarding rule %s does not listen on port %d", rule, h.Dst.Port())
		}
		return h.Src, inj.Inject, nil
	}
	if g := s.opts.Gateways; g != nil {
		for n := range reach {
			for _, a := range g.RelayAddresses(n) {
				if a == h.Dst {
					nw := n
					return h.Src, func(c net.Conn) bool { return g.RelayInject(nw, h.Dst, c) }, nil
				}
			}
		}
	}
	return netip.AddrPort{}, nil, errors.New("nothing the instance reaches is at " + h.Dst.Addr().String() + ":" + strconv.Itoa(int(h.Dst.Port())))
}
