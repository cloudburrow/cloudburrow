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
//     way. An IPv4-only subnet is made dual-stack by patch. An IPv6-only
//     subnet (IPV6_ONLY) has the /64 and no IPv4 range, secondary range or
//     gateway; no stack type changes to or from IPV6_ONLY, and neither
//     kind of IPv6 subnet is allowed on an auto mode network;
//   - a VM's network interface of stackType IPV4_IPV6, on a dual-stack
//     subnet, gets a /96 of the subnet's /64: ipv6Address (and
//     internalIpv6PrefixLength 96) on an INTERNAL subnet, an
//     ipv6AccessConfigs entry (DIRECT_IPV6, externalIpv6, prefix length 96)
//     on an EXTERNAL one; an IPV6_ONLY interface, on an IPv6-only or a
//     dual-stack subnet, has the IPv6 address only (no networkIP, no IPv4
//     accessConfigs), and its guest's vpc0 holds no IPv4 address;
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
	// globalV6Pool is where global external IPv6 addresses (global load
	// balancers') are taken from, a /64 each: a /32 of Google's
	// 2600:1900::/28 that no subnet's /64 is chosen from (subnetV6Locked
	// skips it).
	globalV6Pool = mustCIDR("2600:1901::/32")
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
		case strings.Contains(k, "/addresses/") && str(o, "ipVersion") == "IPV6":
			used[str(o, "address")] = true
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
			if n := v6Block(externalPool, (start+i)&(1<<36-1), 64); !used[n.String()] && !globalV6Pool.Contains(n.IP) {
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
	case "IPV4_IPV6", "IPV6_ONLY":
	default:
		return invalid("Invalid value for field '%s.stackType': '%s'. Must be one of [IPV4_ONLY, IPV4_IPV6, IPV6_ONLY].", field, st)
	}
	if auto, _ := netObj["autoCreateSubnetworks"].(bool); auto {
		// "Subnets with IPv6 address ranges aren't supported on auto mode
		// VPC networks or legacy networks" (vpc/docs/subnets).
		return invalid("Invalid value for field '%s.stackType': '%s'. Subnetworks with IPv6 address ranges aren't supported on auto mode VPC networks; convert the network '%s' to custom mode first.", field, st, str(netObj, "name"))
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
	case "IPV4_IPV6", "IPV6_ONLY":
	default:
		return invalid("Invalid value for field '%s.stackType': '%s'.", field, st)
	}
	switch sst := str(sub, "stackType"); {
	case sst == "IPV6_ONLY" && st == "IPV4_IPV6":
		return invalid("Invalid value for field '%s.stackType': 'IPV4_IPV6'. The subnetwork '%s' is IPv6-only: it takes IPv6-only interfaces.", field, str(sub, "name"))
	case sst != "IPV4_IPV6" && sst != "IPV6_ONLY":
		return invalid("Invalid value for field '%s.stackType': '%s'. The subnetwork '%s' does not have IPv6 enabled: its stack type is IPV4_ONLY.", field, st, str(sub, "name"))
	}
	_, base, err := net.ParseCIDR(str(sub, "ipv6CidrRange"))
	if err != nil {
		return invalid("Invalid value for field '%s.subnetwork': the subnetwork has no IPv6 range.", field)
	}
	// An address asked for: a reserved static IPv6 address of the subnet
	// (taken), or a free /96 of its range (ephemeral).
	want, wantField := str(e, "ipv6Address"), field+".ipv6Address"
	if acs := listOf(e["ipv6AccessConfigs"]); str(sub, "ipv6AccessType") == "EXTERNAL" && len(acs) > 0 {
		ac, _ := acs[0].(map[string]any)
		want, wantField = str(ac, "externalIpv6"), field+".ipv6AccessConfigs[0].externalIpv6"
	} else if str(sub, "ipv6AccessType") == "EXTERNAL" {
		want = ""
	}
	addr, static, err := s.wantV6Locked(sub, base, want, wantField)
	if err != nil {
		return err
	}
	if static != "" {
		nic["_staticV6"] = static
	}
	nic["stackType"], nic["ipv6AccessType"] = st, str(sub, "ipv6AccessType")
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
	if want == "IPV6_ONLY" || str(cur, "stackType") == "IPV6_ONLY" {
		// "You can't change a dual-stack or IPv4-only subnet to IPv6-only.
		// Conversely, you can't change an IPv6-only subnet to IPv4-only or
		// dual-stack" (vpc/docs/subnets, read 2026-11-22).
		return invalid("Invalid value for field 'resource.stackType': '%s'. The stack type of subnetwork '%s' (%s) cannot be changed to or from IPV6_ONLY.", want, str(cur, "name"), str(cur, "stackType"))
	}
	if want == "IPV4_ONLY" {
		for k, o := range s.res {
			if !strings.Contains(k, "/instances/") {
				continue
			}
			for _, e := range listOf(o["networkInterfaces"]) {
				nic, _ := e.(map[string]any)
				if str(nic, "subnetwork") == rel && (str(nic, "stackType") == "IPV4_IPV6" || str(nic, "stackType") == "IPV6_ONLY") {
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

// freeV6Locked is the first free /96 of a subnet's /64.
func (s *Server) freeV6Locked(sub obj, base *net.IPNet) (string, error) {
	used := s.usedV6Locked()
	for i := uint64(1); i < 1<<32; i++ {
		if a := v6Add(base.IP, i, 32).String(); !used[a] {
			return a, nil
		}
	}
	return "", errf(http.StatusBadRequest, "ipSpaceExhausted", "IPv6 space of '%s' is exhausted.", str(sub, "name"))
}

// wantV6Locked is the IPv6 address an interface gets on sub: want when
// given (a RESERVED static address of the subnet, whose name it returns, or
// a free /96 of its range), else the first free /96.
func (s *Server) wantV6Locked(sub obj, base *net.IPNet, want, field string) (string, string, error) {
	if want == "" {
		a, err := s.freeV6Locked(sub, base)
		return a, "", err
	}
	ip := net.ParseIP(want)
	if ip == nil || ip.To4() != nil {
		return "", "", invalid("Invalid value for field '%s': '%s'. Must be a valid IPv6 address.", field, want)
	}
	want = ip.String()
	for k, a := range s.res {
		if !strings.Contains(k, "/addresses/") || str(a, "ipVersion") != "IPV6" || str(a, "address") != want {
			continue
		}
		if str(a, "subnetwork") != str(sub, "selfLink") {
			return "", "", invalid("Invalid value for field '%s': '%s'. The address '%s' is not of subnetwork '%s'.", field, want, str(a, "name"), str(sub, "name"))
		}
		if str(a, "status") != "RESERVED" {
			return "", "", invalid("Invalid value for field '%s': '%s'. The IP address '%s' is already being used by another resource.", field, want, want)
		}
		return want, k, nil
	}
	if !base.Contains(ip) {
		return "", "", invalid("Invalid value for field '%s': '%s'. The address is not within the IPv6 range of subnetwork '%s' (%s).", field, want, str(sub, "name"), base)
	}
	if s.usedV6Locked()[want] {
		return "", "", invalid("Invalid value for field '%s': '%s'. The IP address '%s' is already being used by another resource.", field, want, want)
	}
	return want, "", nil
}

// regionalIPv6Locked fills o, a new static regional IPv6 address
// (vpc/docs/reserve-static-internal-ip-address#reserve_a_static_internal_ipv6_address,
// compute/docs/ip-addresses/configure-ipv6-address, read 2026-11-22): of a
// dual-stack subnet whose IPv6 access type is the address type's
// (INTERNAL, or EXTERNAL in the Premium tier), a /96 of its range, given
// or chosen.
func (s *Server) regionalIPv6Locked(project, region, typ string, b, o obj) error {
	sub := Rel(str(b, "subnetwork"), project)
	if sub == "" {
		return required("resource.subnetwork")
	}
	if !strings.Contains(sub, "/") {
		sub = subnetRel(project, region, sub)
	}
	so, ok := s.res[sub]
	if !ok {
		return notFound(sub)
	}
	if lastSeg(str(so, "region")) != region {
		return invalid("Invalid value for field 'resource.subnetwork': '%s'. The subnetwork must be in region '%s'.", sub, region)
	}
	if (str(so, "stackType") != "IPV4_IPV6" && str(so, "stackType") != "IPV6_ONLY") || str(so, "ipv6AccessType") != typ {
		return invalid("Invalid value for field 'resource.subnetwork': '%s'. An %s IPv6 address needs a dual-stack subnetwork whose IPv6 access type is %s.", sub, typ, typ)
	}
	if v, ok := b["prefixLength"].(float64); ok && v != 0 && v != 96 {
		return invalid("Invalid value for field 'resource.prefixLength': '%v'. An IPv6 address of a VM is a /96.", v)
	}
	if p := str(b, "purpose"); p != "" && p != "GCE_ENDPOINT" {
		return unimplemented("purpose %s for an IPv6 address", p)
	}
	if t := str(b, "networkTier"); typ == "EXTERNAL" && t != "" && t != "PREMIUM" {
		return invalid("Invalid value for field 'resource.networkTier': '%s'. External IPv6 addresses are in the Premium tier.", t)
	}
	_, base, err := net.ParseCIDR(str(so, "ipv6CidrRange"))
	if err != nil {
		return invalid("Invalid value for field 'resource.subnetwork': the subnetwork has no IPv6 range.")
	}
	addr, static, err := s.wantV6Locked(so, base, str(b, "address"), "resource.address")
	if err != nil {
		return err
	}
	if static != "" {
		return invalid("Invalid value for field 'resource.address': '%s'. The IP address is already reserved.", addr)
	}
	o["address"], o["ipVersion"], o["prefixLength"] = addr, "IPV6", 96.0
	o["subnetwork"], o["network"], o["ipv6EndpointType"] = sub, str(so, "network"), "VM"
	if typ == "INTERNAL" {
		o["purpose"] = "GCE_ENDPOINT"
	} else {
		o["networkTier"] = "PREMIUM"
	}
	return nil
}

// globalIPv6Locked fills o, a new global external IPv6 address
// (vpc/docs/reserve-static-external-ip-address, read 2026-11-22: "--global
// --ip-version=IPV6", a /64 of Google's pool, for global load balancers
// only): a free /64 of globalV6Pool, its first address the address. No
// address can be given and it is in the Premium tier; there are no global
// internal IPv6 addresses.
func (s *Server) globalIPv6Locked(typ string, b, o obj) error {
	if typ != "EXTERNAL" {
		return invalid("Invalid value for field 'resource.addressType': '%s'. A global IPv6 address is external.", typ)
	}
	if a := str(b, "address"); a != "" {
		return invalid("Invalid value for field 'resource.address': '%s'. A global IPv6 address is chosen from Google's pool and cannot be given.", a)
	}
	if t := str(b, "networkTier"); t != "" && t != "PREMIUM" {
		return invalid("Invalid value for field 'resource.networkTier': '%s'. A global address must be in the Premium network tier.", t)
	}
	if p := str(b, "purpose"); p != "" {
		return invalid("Invalid value for field 'resource.purpose': '%s'. Purpose is set only for internal addresses.", p)
	}
	a, err := s.freeGlobalV6Locked(str(o, "selfLink"))
	if err != nil {
		return err
	}
	o["address"], o["ipVersion"], o["networkTier"] = a, "IPV6", "PREMIUM"
	return nil
}

// freeGlobalV6Locked is the first address of a free /64 of globalV6Pool,
// starting at seed's hash.
func (s *Server) freeGlobalV6Locked(seed string) (string, error) {
	used := s.usedV6Locked()
	for k, o := range s.res {
		if strings.Contains(k, "/forwardingRules/") && str(o, "ipVersion") == "IPV6" {
			used[str(o, "IPAddress")] = true
		}
	}
	start := hashIndex(seed, 32)
	for i := uint64(0); i < 1<<20; i++ {
		if n := v6Block(globalV6Pool, (start+i)&(1<<32-1), 64); !used[n.IP.String()] {
			return n.IP.String(), nil
		}
	}
	return "", errf(http.StatusBadRequest, "ipSpaceExhausted", "No free global IPv6 address.")
}
