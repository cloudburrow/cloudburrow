package compute

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"
)

// Cloud Run's way onto a VPC network (#2140): a Serverless VPC Access
// connector, or Direct VPC egress. Either is carried by a gateway: a pod
// on the network's VXLAN segment, like a VM's guest (vpc.go), holding an
// internal IP of the network, that forwards what Cloud Run's pods send to
// the network's ranges and translates its source to that IP, as Google's
// connector instances, which take their addresses from the connector's
// /28 (vpc/docs/serverless-vpc-access, "Connector instances"), and Direct
// VPC egress, which takes addresses of the subnet
// (run/docs/configuring/vpc-direct-vpc), do. The VMs see it as one more
// member of their network: a peer with a forwarding entry, a source their
// firewall rules decide on (Direct VPC egress's network tags among
// sourceTags), and a name nobody resolves.
//
// The gateway list is the Run side's (cmd/cloudburrow/computerunegress.go):
// it is given here whole (SetGateways) each time Cloud Run's pods or
// settings change, keyed by what it carries. A connector's address is the
// second of its range; Direct VPC egress's is an address of the subnet
// picked as a VM's is and kept while the gateway is, so no VM takes it.

// Gateway is one connector's or Direct VPC egress setting's way onto a
// network.
type Gateway struct {
	// Key names it: "connector:projects/P/locations/R/connectors/C" or
	// "direct:projects/P/regions/R/subnetworks/S:tag,tag".
	Key string
	// Network is the network's relative name; Subnetwork, for Direct VPC
	// egress, the subnet its address is of.
	Network, Subnetwork string
	// Range is a connector's ipCidrRange (or its subnet's range).
	Range string
	// Region is the connector's or the subnet's region.
	Region string
	// Tags are Direct VPC egress's network tags.
	Tags []string
	// Connector is whether it is a connector's (its address the second of
	// Range, or of Subnetwork's range for a connector made on a subnet);
	// otherwise Direct VPC egress's, an address of Subnetwork.
	Connector bool
	// AllTraffic is egress ALL_TRAFFIC: what is not the network's also
	// leaves through it, and so needs Cloud NAT to reach the internet.
	AllTraffic bool
}

// GatewayState is a gateway as the Run side runs it: its address on the
// network, the token its pod polls under (GuestPrefix TOKEN/cloudburrow/vpc)
// and its pod's IP once it has polled.
type GatewayState struct {
	Gateway
	IP, Token, Pod string
}

type runGateway struct {
	GatewayState
}

// gwRel is the relative name a gateway's vmNet and pod entry use; it is
// not a resource and no list shows it.
func gwRel(key string) string { return "cloudburrow/gateways/" + gwToken(key) }

func gwToken(key string) string {
	h := sha256.Sum256([]byte("gateway:" + key))
	return "gw" + hex.EncodeToString(h[:12])
}

// SetGateways replaces the gateways with gws and returns each one's state,
// in gws' order. A gateway whose network or subnet does not exist (any
// more) is left out.
func (s *Server) SetGateways(gws []Gateway) []GatewayState {
	s.mu.Lock()
	defer s.mu.Unlock()
	old := s.gateways
	s.gateways = map[string]*runGateway{}
	var out []GatewayState
	for _, g := range gws {
		if sub, ok := s.res[g.Subnetwork]; ok {
			if g.Network == "" {
				g.Network = str(sub, "network")
			}
			if g.Connector && g.Range == "" {
				g.Range = str(sub, "ipCidrRange")
			}
			if g.Region == "" {
				g.Region = lastSeg(str(sub, "region"))
			}
		}
		if _, ok := s.res[g.Network]; !ok {
			continue
		}
		st := GatewayState{Gateway: g, Token: gwToken(g.Key)}
		if o, ok := old[g.Key]; ok && o.Network == g.Network && o.Subnetwork == g.Subnetwork && o.Range == g.Range {
			st.IP = o.IP
		}
		if st.IP == "" {
			st.IP = s.gatewayIPLocked(g)
		}
		if st.IP == "" {
			continue
		}
		if p, ok := s.pods[gwRel(g.Key)]; ok {
			st.Pod = p.ip
		}
		s.gateways[g.Key] = &runGateway{st}
		out = append(out, st)
	}
	return out
}

// Gateways are the gateways' states, with their pods' IPs as last polled.
func (s *Server) Gateways() []GatewayState {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []GatewayState
	for _, g := range s.gateways {
		st := g.GatewayState
		if p, ok := s.pods[gwRel(g.Key)]; ok && s.opts.Now().Sub(p.seen) < podFresh {
			st.Pod = p.ip
		} else {
			st.Pod = ""
		}
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// gatewayIPLocked picks g's address: a connector's range's second address,
// or a free address of Direct VPC egress's subnet.
func (s *Server) gatewayIPLocked(g Gateway) string {
	if g.Connector {
		_, n, err := net.ParseCIDR(g.Range)
		if err != nil || n.IP.To4() == nil {
			return ""
		}
		return v4(ip4(n.IP) + 2)
	}
	sub, ok := s.res[g.Subnetwork]
	if !ok {
		return ""
	}
	_, n, err := net.ParseCIDR(str(sub, "ipCidrRange"))
	if err != nil || n.IP.To4() == nil {
		return ""
	}
	used := s.usedInSubnetLocked(g.Subnetwork)
	for _, o := range s.gateways {
		used[o.IP] = gwRel(o.Key)
	}
	lo := ip4(n.IP)
	// From the top of the range down, away from where VMs are given
	// addresses from the bottom up.
	for v := lo + rangeSize(n) - 3; v >= lo+2; v-- {
		if _, taken := used[v4(v)]; !taken {
			return v4(v)
		}
	}
	return ""
}

func v4(v uint32) string {
	return net.IPv4(byte(v>>24), byte(v>>16), byte(v>>8), byte(v)).String()
}

// gatewayVMNetsLocked are the gateways as members of their networks.
func (s *Server) gatewayVMNetsLocked() []vmNet {
	var out []vmNet
	for _, g := range s.gateways {
		out = append(out, vmNet{rel: gwRel(g.Key), name: "gateway", network: g.Network, ip: g.IP, tags: g.Tags,
			running: true, project: strings.Split(g.Network, "/")[1], zone: g.Region + "-a", gateway: true})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].rel < out[j].rel })
	return out
}

// gatewayRangesLocked are the connectors' ranges on reach's networks that
// no subnet of them holds: their VMs route them on-link to the gateway.
func (s *Server) gatewayRangesLocked(reach map[string]bool) []string {
	var out []string
	for _, g := range s.gateways {
		if g.Connector && g.Subnetwork == "" && reach[g.Network] {
			out = append(out, g.Range)
		}
	}
	sort.Strings(out)
	return out
}

// gatewayByTokenLocked is the gateway that polls under token.
func (s *Server) gatewayByTokenLocked(token string) *runGateway {
	for _, g := range s.gateways {
		if g.Token == token {
			return g
		}
	}
	return nil
}

// serveGatewayVPC answers a gateway's poll: its segment and peers as a
// VM's guest gets them, and its ruleset (gatewayNFT).
func (s *Server) serveGatewayVPC(w http.ResponseWriter, r *http.Request, g *runGateway) {
	s.mu.Lock()
	inst := obj{"selfLink": gwRel(g.Key), "name": "gateway", "zone": g.Region + "-a", "status": stRunning, "_gateway": g.Key,
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

// gatewayNFT is a gateway's table: it takes nothing new from the network
// (replies only), forwards what the cluster's pods send it onto the
// network with its own address as the source, and, for ALL_TRAFFIC,
// forwards the rest only while Cloud NAT covers its subnet (internet).
func gatewayNFT(self vmNet, peerPods []string, internet bool) string {
	var b strings.Builder
	b.WriteString("table inet cloudburrow {\n")
	b.WriteString("  chain ingress {\n    type filter hook input priority filter; policy accept;\n")
	b.WriteString("    iifname \"lo\" accept\n    ct state established,related accept\n    ct state invalid drop\n")
	if len(peerPods) > 0 {
		b.WriteString("    iifname != \"vpc0\" ip saddr " + nftSet(peerPods) + " udp dport 4789 accept\n")
	}
	b.WriteString("    counter drop comment \"a gateway takes no connection\"\n  }\n")
	b.WriteString("  chain forward {\n    type filter hook forward priority filter; policy drop;\n")
	b.WriteString("    ct state established,related accept\n")
	b.WriteString("    iifname \"eth0\" oifname \"vpc0\" accept comment \"Cloud Run to the network\"\n")
	b.WriteString("    iifname \"eth0\" oifname \"eth0\" ct status dnat accept comment \"Cloud Run to an internal load balancer or endpoint\"\n")
	if internet {
		b.WriteString("    iifname \"eth0\" oifname \"eth0\" accept comment \"Cloud Run to the internet through Cloud NAT\"\n")
	}
	b.WriteString("    counter drop\n  }\n")
	b.WriteString("  chain postrouting {\n    type nat hook postrouting priority 100; policy accept;\n")
	fmt.Fprintf(&b, "    oifname \"vpc0\" snat ip to %s comment \"the gateway's address\"\n", self.ip)
	if internet {
		b.WriteString("    iifname \"eth0\" oifname \"eth0\" masquerade\n")
	}
	b.WriteString("  }\n}\n")
	return b.String()
}

// GatewayRanges are the IPv4 ranges the gateway key carries Cloud Run's
// traffic to: its network's and its ACTIVE peers' subnet ranges
// (secondary ranges among them) and connectors' ranges.
func (s *Server) GatewayRanges(key string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.gateways[key]
	if !ok {
		return nil
	}
	reach := map[string]bool{g.Network: true}
	for _, n := range s.peeredNetworksLocked(g.Network) {
		reach[n] = true
	}
	var out []string
	for k, o := range s.res {
		if !strings.Contains(k, "/subnetworks/") || !reach[str(o, "network")] {
			continue
		}
		if c := str(o, "ipCidrRange"); c != "" {
			out = append(out, c)
		}
		for _, e := range sliceOf(o["secondaryIpRanges"]) {
			if c := str(objOf(e), "ipCidrRange"); c != "" {
				out = append(out, c)
			}
		}
	}
	out = append(out, s.gatewayRangesLocked(reach)...)
	sort.Strings(out)
	return out
}
