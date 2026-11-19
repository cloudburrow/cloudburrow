package redisadmin

import (
	"crypto/sha256"
	"fmt"
	"net"
	"regexp"

	redispb "cloud.google.com/go/redis/apiv1/redispb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// A Memorystore for Redis instance's network (#1906): authorized_network,
// connect_mode and reserved_ip_range are kept, checked and returned as
// Google documents them
// (https://docs.cloud.google.com/memorystore/docs/redis/networking,
// re-read 2026-11-19):
//
//   - authorized_network, "the full name of the Google Compute Engine
//     network to which the instance is connected", defaults to the
//     project's default network; a bare network name is taken as one of
//     the instance's project;
//   - connect_mode defaults to DIRECT_PEERING ("by default, new instances
//     are created using the direct peering connection mode"), or is
//     PRIVATE_SERVICE_ACCESS; "you cannot switch the connection mode of an
//     existing instance", and neither the network nor the range is an
//     updatable field;
//   - reserved_ip_range, with DIRECT_PEERING, is an IPv4 CIDR range of
//     internal addresses, at least a /29 ("the minimum required block size
//     is /29 for instances without read replicas", /28 with read
//     replicas); with PRIVATE_SERVICE_ACCESS it is the name of an
//     allocated range, or a CIDR range of at least /28 with read
//     replicas. Unset, a /29 (/28 with read replicas) is chosen, as Google
//     chooses an unused block, in 10.0.0.0/8, the same for the same
//     instance name.
//
// There is no VPC here: the network and range are not where the server is.
// Every local client reaches the instance at its host and port, whatever
// the network it is authorized on, and host is the host's address, not one
// of the range. A network that does not exist is not refused, as Compute
// Engine is not served to look it up.

var (
	networkRE    = regexp.MustCompile(`^projects/([^/]+)/global/networks/([a-z]([-a-z0-9]{0,61}[a-z0-9])?)$`)
	networkIDRE  = regexp.MustCompile(`^[a-z]([-a-z0-9]{0,61}[a-z0-9])?$`)
	rangeNameRE  = regexp.MustCompile(`^[a-z]([-a-z0-9]{0,61}[a-z0-9])?$`)
	shortNetRE   = regexp.MustCompile(`^global/networks/([a-z]([-a-z0-9]{0,61}[a-z0-9])?)$`)
	privateBlock = []*net.IPNet{mustCIDR("10.0.0.0/8"), mustCIDR("172.16.0.0/12"), mustCIDR("192.168.0.0/16"),
		mustCIDR("100.64.0.0/10"), mustCIDR("192.0.0.0/24"), mustCIDR("192.0.2.0/24"), mustCIDR("198.18.0.0/15"),
		mustCIDR("198.51.100.0/24"), mustCIDR("203.0.113.0/24"), mustCIDR("240.0.0.0/4")}
)

func mustCIDR(s string) *net.IPNet {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		panic(err)
	}
	return n
}

// instanceNetwork is what CreateInstance keeps of in's network, for the
// instance name in project.
func instanceNetwork(in *redispb.Instance, name, project string) (network, mode, ipRange string, err error) {
	network = in.GetAuthorizedNetwork()
	switch {
	case network == "":
		network = "projects/" + project + "/global/networks/default"
	case networkIDRE.MatchString(network):
		network = "projects/" + project + "/global/networks/" + network
	case shortNetRE.MatchString(network):
		network = "projects/" + project + "/" + network
	case !networkRE.MatchString(network):
		return "", "", "", apierror.InvalidArgument("authorized_network %q must be the full name of a network: projects/{project}/global/networks/{network}", in.GetAuthorizedNetwork())
	}
	cm := in.GetConnectMode()
	switch cm {
	case redispb.Instance_CONNECT_MODE_UNSPECIFIED:
		cm = redispb.Instance_DIRECT_PEERING
	case redispb.Instance_DIRECT_PEERING, redispb.Instance_PRIVATE_SERVICE_ACCESS:
	default:
		return "", "", "", apierror.InvalidArgument("connect_mode %v is not DIRECT_PEERING or PRIVATE_SERVICE_ACCESS", cm)
	}
	minBlock := 29
	if in.GetReadReplicasMode() == redispb.Instance_READ_REPLICAS_ENABLED {
		minBlock = 28
	}
	ipRange = in.GetReservedIpRange()
	switch {
	case ipRange == "":
		ipRange = defaultRange(name, minBlock)
	case cm == redispb.Instance_PRIVATE_SERVICE_ACCESS && rangeNameRE.MatchString(ipRange):
		// The name of an allocated range of the private services access
		// connection.
	default:
		if err := checkRange(ipRange, minBlock); err != nil {
			return "", "", "", err
		}
	}
	return network, cm.String(), ipRange, nil
}

// checkRange checks a CIDR reserved_ip_range of at least a /minBlock.
func checkRange(s string, minBlock int) error {
	ip, n, err := net.ParseCIDR(s)
	if err != nil || ip.To4() == nil {
		return apierror.InvalidArgument("reserved_ip_range %q must be an IPv4 range in CIDR notation, such as 10.0.0.0/29", s)
	}
	if !ip.Equal(n.IP) {
		return apierror.InvalidArgument("reserved_ip_range %q is not the start of its range: %s", s, n)
	}
	if ones, _ := n.Mask.Size(); ones > minBlock {
		return apierror.InvalidArgument("reserved_ip_range %q is too small: the minimum required block size is /%d", s, minBlock)
	}
	for _, p := range privateBlock {
		if p.Contains(n.IP) {
			return nil
		}
	}
	return apierror.InvalidArgument("reserved_ip_range %q is not a range of internal addresses: Memorystore for Redis supports RFC 1918 and some non-RFC 1918 private ranges, not public ones", s)
}

// defaultRange is the /block chosen for an instance with no
// reserved_ip_range: in 10.0.0.0/8, from a hash of its name.
func defaultRange(name string, block int) string {
	sum := sha256.Sum256([]byte(name))
	size := uint32(1) << (32 - block)
	off := (uint32(sum[0])<<16 | uint32(sum[1])<<8 | uint32(sum[2])) &^ (size - 1)
	return fmt.Sprintf("10.%d.%d.%d/%d", byte(off>>16), byte(off>>8), byte(off), block)
}

// renderNetwork sets inst's network fields from r's.
func (r *record) renderNetwork(inst *redispb.Instance) {
	m := instanceRE.FindStringSubmatch(r.Name)
	inst.AuthorizedNetwork = r.AuthorizedNetwork
	if inst.AuthorizedNetwork == "" && m != nil {
		inst.AuthorizedNetwork = "projects/" + m[1] + "/global/networks/default"
	}
	inst.ConnectMode = redispb.Instance_DIRECT_PEERING
	if v, ok := redispb.Instance_ConnectMode_value[r.ConnectMode]; ok && v != 0 {
		inst.ConnectMode = redispb.Instance_ConnectMode(v)
	}
	inst.ReservedIpRange = r.ReservedIPRange
	if inst.ReservedIpRange == "" {
		block := 29
		if r.ReadReplicas {
			block = 28
		}
		inst.ReservedIpRange = defaultRange(r.Name, block)
	}
}
