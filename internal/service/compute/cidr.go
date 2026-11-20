package compute

import (
	"encoding/binary"
	"net"
	"regexp"
)

// Subnet IPv4 ranges, as https://docs.cloud.google.com/vpc/docs/subnets
// (re-read 2026-11-19) documents them: a valid CIDR block from /4 to /29,
// not in, containing or overlapping a prohibited range, and not spanning
// two of the valid ranges (or a valid range and the public addresses
// around it).

var (
	// validRanges are the documented private and special ranges a subnet
	// may use; any other range is a privately used public range.
	validRanges = cidrs("10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10", "192.0.0.0/24",
		"192.0.2.0/24", "198.51.100.0/24", "203.0.113.0/24", "192.88.99.0/24", "198.18.0.0/15", "240.0.0.0/4")
	// prohibited are the ranges a subnet range may not match, be within
	// or contain.
	prohibited = cidrs("0.0.0.0/8", "127.0.0.0/8", "169.254.0.0/16", "224.0.0.0/4", "255.255.255.255/32",
		"199.36.153.4/30", "199.36.153.8/30")
	// autoBlock holds every auto mode subnet; unused parts are reserved.
	autoBlock = cidrs("10.128.0.0/9")[0]
)

func cidrs(ss ...string) []*net.IPNet {
	out := make([]*net.IPNet, len(ss))
	for i, s := range ss {
		_, n, err := net.ParseCIDR(s)
		if err != nil {
			panic(err)
		}
		out[i] = n
	}
	return out
}

// Overlaps reports whether a and b share an address.
func Overlaps(a, b *net.IPNet) bool { return a.Contains(b.IP) || b.Contains(a.IP) }

// ParseCIDR parses an IPv4 CIDR block whose address is its network's.
func ParseCIDR(s string) (*net.IPNet, bool) {
	ip, n, err := net.ParseCIDR(s)
	if err != nil || ip.To4() == nil || !ip.Equal(n.IP) {
		return nil, false
	}
	return n, true
}

// checkSubnetRange checks a primary (secondary false) or secondary range
// of field.
func checkSubnetRange(field, s string) (*net.IPNet, error) {
	n, ok := ParseCIDR(s)
	if !ok {
		return nil, invalid("Invalid value for field '%s': '%s'. Invalid IPCidrRange: %s is not a valid CIDR block.", field, s, s)
	}
	ones, _ := n.Mask.Size()
	if ones < 4 || ones > 29 {
		return nil, invalid("Invalid value for field '%s': '%s'. Invalid IPCidrRange: %s. The prefix length must be between /4 and /29.", field, s, s)
	}
	for _, p := range prohibited {
		if Overlaps(p, n) {
			return nil, invalid("Invalid value for field '%s': '%s'. Invalid IPCidrRange: %s conflicts with the restricted range %s.", field, s, s, p)
		}
	}
	// It must not span a valid range and what is outside it.
	for _, v := range validRanges {
		if Overlaps(v, n) && !contains(v, n) {
			return nil, invalid("Invalid value for field '%s': '%s'. Invalid IPCidrRange: %s spans more than one IPv4 range (%s and the addresses around it); use a range within one.", field, s, s, v)
		}
	}
	return n, nil
}

// contains reports whether outer holds every address of inner.
func contains(outer, inner *net.IPNet) bool {
	oo, _ := outer.Mask.Size()
	io, _ := inner.Mask.Size()
	return oo <= io && outer.Contains(inner.IP)
}

func ip4(ip net.IP) uint32 { return binary.BigEndian.Uint32(ip.To4()) }

func fromU32(v uint32) net.IP {
	b := make(net.IP, 4)
	binary.BigEndian.PutUint32(b, v)
	return b
}

// gateway is a primary range's default gateway, its second address.
func gateway(n *net.IPNet) string { return fromU32(ip4(n.IP) + 1).String() }

// reservedAddr reports whether ip is one of the four addresses Google
// keeps in a primary range: the network, the gateway, the second-to-last
// and the broadcast address.
func reservedAddr(n *net.IPNet, ip net.IP) bool {
	ones, _ := n.Mask.Size()
	lo := ip4(n.IP)
	hi := lo + (uint32(1) << (32 - ones)) - 1
	v := ip4(ip)
	return v == lo || v == lo+1 || v == hi-1 || v == hi
}

// rangeSize is the number of addresses in n.
func rangeSize(n *net.IPNet) uint32 {
	ones, _ := n.Mask.Size()
	return uint32(1) << (32 - ones)
}

// nameRE is Compute's resource name rule (RFC 1035).
var nameRE = regexp.MustCompile(`^[a-z](?:[-a-z0-9]{0,61}[a-z0-9])?$`)

const nameRule = `(?:[a-z](?:[-a-z0-9]{0,61}[a-z0-9])?)`

// checkName refuses a name outside the rule, in Google's words.
func checkName(field, name string) error {
	if name == "" {
		return invalid("Invalid value for field '%s': ''. Must be a match of regex '%s'", field, nameRule)
	}
	if !nameRE.MatchString(name) {
		return invalid("Invalid value for field '%s': '%s'. Must be a match of regex '%s'", field, name, nameRule)
	}
	return nil
}

// IsValidName reports whether name follows Compute's resource name rule.
func IsValidName(name string) bool { return nameRE.MatchString(name) }

func required(field string) error {
	return invalid("Required field '%s' not specified", field)
}
