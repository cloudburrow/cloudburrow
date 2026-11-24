package compute

import (
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
)

// The VPC's internal services on its data plane (#2140, #2153): a VM's TCP
// connections to an internal load balancer's forwarding rule address, or
// to a Private Service Connect endpoint for a published service, reach the
// load balancer that serves it, as on Google:
//
//   - an internal forwarding rule (INTERNAL or INTERNAL_MANAGED) takes
//     connections from VMs of its network and of networks peered with it,
//     in its region, or in every region with allowGlobalAccess
//     (load-balancing/docs/internal#client_access);
//   - an endpoint takes connections from VMs of its own network, in its
//     region or, with allowPscGlobalAccess, every region, while its
//     connection is ACCEPTED, and carries them to the forwarding rule its
//     service attachment publishes (targetService), on the same port
//     (vpc/docs/private-service-connect, "Endpoints").
//
// The load balancers' listeners are on this machine (lbplane.go), so the
// guest's ruleset rewrites each such address and port to the listener as
// its pod reaches it (Options.GuestReach), in an nftables nat chain on the
// output hook; the egress firewall rules match a connection's original
// destination (ct original), so they apply to the service's own address
// as Google applies them. The listener then balances over the backends as the load
// balancer does. Only TCP is carried, as the data plane carries only TCP;
// the source a backend sees is the load balancer's, not the client VM's
// (docs/gaps.md).

// vpcService is one address and port of the VPC that a VM's connection is
// carried from, and the address that takes it.
type vpcService struct {
	ip, to string
	port   int
	what   string // the forwarding rule, for the ruleset's comment
	// v6 is an IPv6 internal forwarding rule's address (ipVersion IPV6),
	// which the guest's relay6 carries: nftables cannot rewrite an IPv6
	// destination to the IPv4 listener.
	v6 bool
}

// vpcServicesLocked are the internal services self reaches.
func (s *Server) vpcServicesLocked(self vmNet, reach map[string]bool) []vpcService {
	if s.opts.GuestReach == nil || s.lb == nil {
		return nil
	}
	region := regionOfZone(self.zone)
	var out []vpcService
	add := func(ip string, rule obj, port int, what string) {
		ip = strings.TrimSuffix(ip, "/96")
		parsed := net.ParseIP(ip)
		if parsed == nil {
			return
		}
		v6 := parsed.To4() == nil
		for _, e := range strs(rule, "cloudburrowAddresses") {
			k, v, ok := strings.Cut(e, "=")
			if !ok || k != strconv.Itoa(port) {
				continue
			}
			if to := s.opts.GuestReach(v); to != "" {
				out = append(out, vpcService{ip: ip, port: port, to: to, what: what, v6: v6})
			}
		}
	}
	for k, o := range s.res {
		if !strings.Contains(k, "/forwardingRules/") {
			continue
		}
		rregion := lastSeg(str(o, "region"))
		local := rregion == region
		if o["_lb"] == true {
			sch := str(o, "loadBalancingScheme")
			if (sch != "INTERNAL" && sch != "INTERNAL_MANAGED") || !reach[str(o, "network")] {
				continue
			}
			if !local && o["allowGlobalAccess"] != true {
				continue
			}
			for _, p := range rulePorts(o) {
				add(str(o, "IPAddress"), o, p, k)
			}
			continue
		}
		// An endpoint for a published service, of self's own network.
		att := str(o, "target")
		if !strings.Contains(att, "/serviceAttachments/") || str(o, "network") != self.network || str(o, "pscConnectionStatus") != "ACCEPTED" {
			continue
		}
		if !local && o["allowPscGlobalAccess"] != true {
			continue
		}
		a, ok := s.res[att]
		if !ok {
			continue
		}
		prod, ok := s.res[str(a, "targetService")]
		if !ok {
			continue
		}
		for _, p := range rulePorts(prod) {
			add(str(o, "IPAddress"), prod, p, k)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ip != out[j].ip {
			return out[i].ip < out[j].ip
		}
		return out[i].port < out[j].port
	})
	return out
}

// servicesNFT is the nat chain of the guest's table carrying svcs, on hook
// (output for a VM's own connections, prerouting for those a gateway
// forwards).
func servicesNFT(svcs []vpcService, hook string) string {
	if len(svcs) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("  chain services {\n    type nat hook " + hook + " priority -100; policy accept;\n")
	for _, v := range svcs {
		if v.v6 {
			continue // relay6, in the config's relay6 lines
		}
		fmt.Fprintf(&b, "    ip daddr %s tcp dport %d dnat ip to %s comment %q\n", v.ip, v.port, v.to, lastSeg(v.what))
	}
	b.WriteString("  }\n")
	// The source was chosen for vpc0 before the rewrite sent the
	// connection out of eth0: it leaves with the pod's own address, so the
	// replies come back to it.
	b.WriteString("  chain services_source {\n    type nat hook postrouting priority 100; policy accept;\n")
	b.WriteString("    oifname != \"vpc0\" ct status dnat masquerade comment \"carried to a load balancer\"\n  }\n")
	return b.String()
}
