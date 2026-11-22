package compute

import (
	"crypto/sha256"
	"encoding/binary"
	"math/big"
	"net"
	"net/http"
	"strings"
)

// IPv6 (#2140, https://docs.cloud.google.com/vpc/docs/subnets#ipv6-ranges,
// .../vpc/docs/create-modify-vpc-networks ("Enable ULA internal IPv6"),
// .../vpc/docs/create-modify-vpc-networks#create-dual-stack-subnet,
// .../compute/docs/ip-addresses/configure-ipv6-address and the networks,
// subnetworks and instances REST references, read 2026-11-22):
//
//   - a network's enableUlaInternalIpv6 gives it internalIpv6Range, a /48
//     of fd20::/20 (one given must be a /48 of it, free among networks);
//   - a dual-stack subnet (stackType IPV4_IPV6) has an ipv6AccessType:
//     INTERNAL takes a /64 of its network's ULA /48 (internalIpv6Prefix),
//     which needs enableUlaInternalIpv6; EXTERNAL a /64 of Google's
//     2600:1900::/28 (externalIpv6Prefix); ipv6CidrRange is the /64 either
//     way. An IPv4-only subnet is made dual-stack by patch; IPV6_ONLY
//     subnets are not served;
//   - a VM's network interface of stackType IPV4_IPV6, on a dual-stack
//     subnet, gets a /96 of the subnet's /64: ipv6Address (and
//     internalIpv6PrefixLength 96) on an INTERNAL subnet, an
//     ipv6AccessConfigs entry (DIRECT_IPV6, externalIpv6, prefix length 96)
//     on an EXTERNAL one;
//   - firewall rules take IPv6 ranges, matched against IPv6 traffic only.
//
// The data plane (vpc.go) carries the VMs' IPv6 addresses on the same
// VXLAN segment as their IPv4 ones: each guest's vpc0 holds its address,
// an on-link route per dual-stack /64 of the network and a permanent IPv6
// neighbour entry per peer, so VMs reach each other over IPv6 on the
// network whatever the cluster's own pod network carries (kind's is IPv4
// only); nothing routes an IPv6 address off the network.

var (
	ulaPool      = mustCIDR("fd20::/20")
	externalPool = mustCIDR("2600:1900::/28")
)

func mustCIDR(s string) *net.IPNet {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		panic(err)
	}
	return n
}

// v6Add is ip plus off<<shift.
func v6Add(ip net.IP, off uint64, shift uint) net.IP {
	v := new(big.Int).SetBytes(ip.To16())
	v.Add(v, new(big.Int).Lsh(new(big.Int).SetUint64(off), shift))
	b := v.Bytes()
	out := make(net.IP, 16)
	copy(out[16-len(b):], b)
	return out
}

// v6Block is the index-th block of prefix length bits inside pool.
func v6Block(pool *net.IPNet, index uint64, bits int) *net.IPNet {
	return &net.IPNet{IP: v6Add(pool.IP, index, uint(128-bits)), Mask: net.CIDRMask(bits, 128)}
}

// hashIndex is a stable starting index of size bits for seed.
func hashIndex(seed string, size uint) uint64 {
	h := sha256.Sum256([]byte(seed))
	return binary.BigEndian.Uint64(h[:8]) & (1<<size - 1)
}

// usedV6Locked is every IPv6 range or address in use, by its string.
func (s *Server) usedV6Locked() map[string]bool {
	used := map[string]bool{}
	for k, o := range s.res {
		switch {
		case strings.Contains(k, "/global/networks/"):
			used[str(o, "internalIpv6Range")] = true
		case strings.Contains(k, "/subnetworks/"):
			used[str(o, "ipv6CidrRange")] = true
		case strings.Contains(k, "/instances/"):
			for _, e := range listOf(o["networkInterfaces"]) {
				nic, _ := e.(map[string]any)
				used[str(nic, "ipv6Address")] = true
				for _, a := range listOf(nic["ipv6AccessConfigs"]) {
					ac, _ := a.(map[string]any)
					used[str(ac, "externalIpv6")] = true
				}
			}
		}
	}
	delete(used, "")
	return used
}

// ulaRangeLocked is a network's internal IPv6 /48: want when given (a /48
// of fd20::/20), else a free one chosen from the network's ID.
func (s *Server) ulaRangeLocked(seed, want string) (string, error) {
	used := s.usedV6Locked()
	if want != "" {
		ip, n, err := net.ParseCIDR(want)
		ones, _ := n.Mask.Size()
		if err != nil || ip.To4() != nil || ones != 48 || !ulaPool.Contains(ip) || !ip.Equal(n.IP) {
			return "", invalid("Invalid value for field 'resource.internalIpv6Range': '%s'. Must be a /48 range of fd20::/20.", want)
		}
		if used[n.String()] {
			return "", invalid("Invalid value for field 'resource.internalIpv6Range': '%s'. The range is already in use by another network.", want)
		}
		return n.String(), nil
	}
	start := hashIndex(seed, 28)
	for i := uint64(0); i < 1<<28; i++ {
		n := v6Block(ulaPool, (start+i)&(1<<28-1), 48)
		if !used[n.String()] {
			return n.String(), nil
		}
	}
	return "", errf(http.StatusBadRequest, "ipSpaceExhausted", "No free /48 of fd20::/20.")
}

// subnetV6Locked is a free /64 for a subnet: of the network's ULA /48 for
// INTERNAL, of the external pool for EXTERNAL.
func (s *Server) subnetV6Locked(netObj obj, access, seed string) (string, error) {
	used := s.usedV6Locked()
	switch access {
	case "INTERNAL":
		_, base, err := net.ParseCIDR(str(netObj, "internalIpv6Range"))
		if err != nil {
			return "", invalid("Invalid value for field 'resource.ipv6AccessType': 'INTERNAL'. The network '%s' does not have ULA internal IPv6 enabled (enableUlaInternalIpv6).", str(netObj, "name"))
		}
		for i := uint64(0); i < 1<<16; i++ {
			if n := v6Block(base, i, 64); !used[n.String()] {
				return n.String(), nil
			}
		}
	case "EXTERNAL":
		start := hashIndex(seed, 36)
		for i := uint64(0); i < 1<<20; i++ {
			if n := v6Block(externalPool, (start+i)&(1<<36-1), 64); !used[n.String()] {
				return n.String(), nil
			}
		}
	}
	return "", errf(http.StatusBadRequest, "ipSpaceExhausted", "No free IPv6 /64 for the subnetwork.")
}

// dualStackLocked fills o, a subnet of network netObj, from b's stackType
// and ipv6AccessType.
func (s *Server) dualStackLocked(netObj, b, o obj, field string) error {
	st := str(b, "stackType")
	switch st {
	case "", "IPV4_ONLY":
		if at := str(b, "ipv6AccessType"); at != "" {
			return invalid("Invalid value for field '%s.ipv6AccessType': '%s'. An IPv4-only subnetwork has no IPv6 access type.", field, at)
		}
		return nil
	case "IPV4_IPV6":
	case "IPV6_ONLY":
		return unimplemented("stackType IPV6_ONLY: IPv6-only subnets are not served; use IPV4_IPV6 (dual-stack)")
	default:
		return invalid("Invalid value for field '%s.stackType': '%s'. Must be one of [IPV4_ONLY, IPV4_IPV6, IPV6_ONLY].", field, st)
	}
	at := str(b, "ipv6AccessType")
	if at != "INTERNAL" && at != "EXTERNAL" {
		if at == "" {
			return required(field + ".ipv6AccessType")
		}
		return invalid("Invalid value for field '%s.ipv6AccessType': '%s'. Must be INTERNAL or EXTERNAL.", field, at)
	}
	r, err := s.subnetV6Locked(netObj, at, str(o, "selfLink"))
	if err != nil {
		return err
	}
	o["stackType"], o["ipv6AccessType"], o["ipv6CidrRange"] = st, at, r
	if at == "INTERNAL" {
		o["internalIpv6Prefix"] = r
	} else {
		o["externalIpv6Prefix"] = r
	}
	return nil
}

// nicV6Locked gives nic, on subnet sub, its IPv6 address when the
// interface asks for IPV4_IPV6.
func (s *Server) nicV6Locked(e map[string]any, nic obj, sub obj, field string) error {
	st := str(e, "stackType")
	switch st {
	case "", "IPV4_ONLY":
		return nil
	case "IPV4_IPV6":
	case "IPV6_ONLY":
		return unimplemented("stackType IPV6_ONLY: IPv6-only network interfaces are not served; use IPV4_IPV6")
	default:
		return invalid("Invalid value for field '%s.stackType': '%s'.", field, st)
	}
	if str(sub, "stackType") != "IPV4_IPV6" {
		return invalid("Invalid value for field '%s.stackType': 'IPV4_IPV6'. The subnetwork '%s' does not have IPv6 enabled: its stack type is IPV4_ONLY.", field, str(sub, "name"))
	}
	_, base, err := net.ParseCIDR(str(sub, "ipv6CidrRange"))
	if err != nil {
		return invalid("Invalid value for field '%s.subnetwork': the subnetwork has no IPv6 range.", field)
	}
	used := s.usedV6Locked()
	var addr string
	for i := uint64(1); i < 1<<32; i++ {
		if a := v6Add(base.IP, i, 32).String(); !used[a] {
			addr = a
			break
		}
	}
	if addr == "" {
		return errf(http.StatusBadRequest, "ipSpaceExhausted", "IPv6 space of '%s' is exhausted.", str(sub, "name"))
	}
	nic["stackType"], nic["ipv6AccessType"] = "IPV4_IPV6", str(sub, "ipv6AccessType")
	if str(sub, "ipv6AccessType") == "INTERNAL" {
		nic["ipv6Address"], nic["internalIpv6PrefixLength"] = addr, 96.0
		return nil
	}
	name := "external-ipv6"
	tier := "PREMIUM"
	if acs := listOf(e["ipv6AccessConfigs"]); len(acs) > 0 {
		ac, _ := acs[0].(map[string]any)
		name = vpcOrStr(str(ac, "name"), name)
		if t := str(ac, "networkTier"); t != "" && t != "PREMIUM" {
			return invalid("Invalid value for field '%s.ipv6AccessConfigs[0].networkTier': '%s'. External IPv6 addresses are in the Premium tier.", field, t)
		}
	}
	nic["ipv6AccessConfigs"] = []any{obj{"kind": "compute#accessConfig", "type": "DIRECT_IPV6", "name": name,
		"externalIpv6": addr, "externalIpv6PrefixLength": 96.0, "networkTier": tier}}
	return nil
}

// isV6 is whether a firewall range or address is IPv6.
func isV6(r string) bool { return strings.Contains(r, ":") }

// byFamily keeps the IPv4 (v6 false) or IPv6 entries of l.
func byFamily(l []string, v6 bool) []string {
	var out []string
	for _, r := range l {
		if isV6(r) == v6 {
			out = append(out, r)
		}
	}
	return out
}

// patchUlaLocked turns a network's ULA internal IPv6 on (a /48 given or
// chosen) or off (refused while a subnet takes an internal IPv6 range).
func (s *Server) patchUlaLocked(rel string, cur, next obj, on bool, want string) error {
	if was, _ := cur["enableUlaInternalIpv6"].(bool); was == on {
		return nil
	}
	if !on {
		for k, o := range s.res {
			if strings.Contains(k, "/subnetworks/") && str(o, "network") == rel && str(o, "ipv6AccessType") == "INTERNAL" {
				return errf(http.StatusBadRequest, "resourceInUseByAnotherResource",
					"The network's internal IPv6 range is in use by subnetwork '%s': make it IPv4 only first.", k)
			}
		}
		delete(next, "enableUlaInternalIpv6")
		delete(next, "internalIpv6Range")
		return nil
	}
	r, err := s.ulaRangeLocked(rel, want)
	if err != nil {
		return err
	}
	next["enableUlaInternalIpv6"], next["internalIpv6Range"] = true, r
	return nil
}

// patchStackLocked changes a subnet's stack type (vpc/docs/create-modify-vpc-networks,
// "Change a subnet's stack type"): IPV4_ONLY to IPV4_IPV6 with an
// ipv6AccessType, or back to IPV4_ONLY while no VM holds one of its IPv6
// addresses; a dual-stack subnet's access type is fixed.
func (s *Server) patchStackLocked(rel string, cur, next, b obj) error {
	want := vpcOrStr(str(b, "stackType"), str(cur, "stackType"))
	if want == str(cur, "stackType") {
		if at := str(b, "ipv6AccessType"); at != "" && at != str(cur, "ipv6AccessType") {
			return invalid("Invalid value for field 'resource.ipv6AccessType': '%s'. A subnetwork's IPv6 access type cannot be changed.", at)
		}
		return nil
	}
	if want == "IPV4_ONLY" {
		for k, o := range s.res {
			if !strings.Contains(k, "/instances/") {
				continue
			}
			for _, e := range listOf(o["networkInterfaces"]) {
				nic, _ := e.(map[string]any)
				if str(nic, "subnetwork") == rel && str(nic, "stackType") == "IPV4_IPV6" {
					return errf(http.StatusBadRequest, "resourceInUseByAnotherResource",
						"The subnetwork's IPv6 range is in use by instance '%s'.", k)
				}
			}
		}
		for _, k := range []string{"ipv6AccessType", "ipv6CidrRange", "internalIpv6Prefix", "externalIpv6Prefix"} {
			delete(next, k)
		}
		next["stackType"] = "IPV4_ONLY"
		return nil
	}
	return s.dualStackLocked(s.res[str(cur, "network")], b, next, "resource")
}

// dualStackPeeringLocked is whether network's peering with peer exchanges
// IPv6 routes (stackType IPV4_IPV6).
func (s *Server) dualStackPeeringLocked(network, peer string) bool {
	o, ok := s.res[network]
	if !ok {
		return false
	}
	for _, e := range s.peeringsLocked(o) {
		if m, _ := e.(map[string]any); Rel(str(m, "network"), "") == peer && str(m, "stackType") == "IPV4_IPV6" {
			return true
		}
	}
	return false
}
