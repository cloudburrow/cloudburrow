package redisadmin

import (
	"net"
	"strconv"

	redispb "cloud.google.com/go/redis/apiv1/redispb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// Networks are the Compute Engine VPC networks (#1936) a Memorystore
// resource's network fields are checked against, as Google checks them,
// when the instance serves Compute Engine; without it (nil) any network
// is accepted, as before. internal/service/compute's Server is one.
type Networks interface {
	// HasNetwork reports whether the network ref exists, in project
	// when ref is a bare name.
	HasNetwork(ref, project string) bool
	// Conflict describes the subnet or allocated range of network (a
	// relative name) that n overlaps, or "".
	Conflict(network string, n *net.IPNet) string
	// AllocatedRange is the range of a VPC_PEERING global address name
	// allocated on network.
	AllocatedRange(network, name string) (*net.IPNet, bool)
}

// SetNetworks makes the API check networks against nets(), read at each
// call; nets() nil checks nothing.
func (s *Server) SetNetworks(nets func() Networks) { s.networks = nets }

func (s *Server) vpc() Networks {
	if s == nil || s.networks == nil {
		return nil
	}
	return s.networks()
}

// checkInstanceVPC checks a Memorystore for Redis instance's
// authorized_network and reserved_ip_range against the network
// (https://cloud.google.com/memorystore/docs/redis/reference/rest/v1/projects.locations.instances,
// re-read 2026-11-19): the network must exist, and a CIDR range "must be
// unique and non-overlapping with existing subnets in an authorized
// network"; with PRIVATE_SERVICE_ACCESS a range name is an allocated range
// of the network. A range chosen for the instance is moved off a subnet.
// The error texts are UNVERIFIED.
func (s *Server) checkInstanceVPC(in *redispb.Instance, r *record) error {
	nets := s.vpc()
	if nets == nil {
		return nil
	}
	if !nets.HasNetwork(r.AuthorizedNetwork, "") {
		return apierror.InvalidArgument("authorized_network %q does not exist: create the network first (Compute Engine networks.insert)", r.AuthorizedNetwork)
	}
	if r.ConnectMode == redispb.Instance_PRIVATE_SERVICE_ACCESS.String() && rangeNameRE.MatchString(r.ReservedIPRange) {
		if _, ok := nets.AllocatedRange(r.AuthorizedNetwork, r.ReservedIPRange); !ok {
			return apierror.InvalidArgument("reserved_ip_range %q is not an IP range allocated for private services access on network %s", r.ReservedIPRange, r.AuthorizedNetwork)
		}
		return nil
	}
	_, n, err := net.ParseCIDR(r.ReservedIPRange)
	if err != nil {
		return nil
	}
	c := nets.Conflict(r.AuthorizedNetwork, n)
	if c == "" {
		return nil
	}
	if in.GetReservedIpRange() != "" {
		return apierror.InvalidArgument("reserved_ip_range %q overlaps %s of network %s: the range must be unique and non-overlapping with existing subnets in an authorized network", r.ReservedIPRange, c, r.AuthorizedNetwork)
	}
	// Chosen here: as Google picks an unused block, take the next one free.
	ones, _ := n.Mask.Size()
	for i := 1; i < 4096; i++ {
		cand := defaultRange(r.Name+"#"+strconv.Itoa(i), ones)
		if _, cn, err := net.ParseCIDR(cand); err == nil && nets.Conflict(r.AuthorizedNetwork, cn) == "" {
			r.ReservedIPRange = cand
			return nil
		}
	}
	return apierror.FailedPrecondition("no free /%d range on network %s for the instance", ones, r.AuthorizedNetwork)
}

// checkPSCNetworks refuses a PSC network (projects/P/global/networks/N)
// that does not exist, as Google refuses it.
func (s *Server) checkPSCNetworks(field string, networks ...string) error {
	nets := s.vpc()
	if nets == nil {
		return nil
	}
	for _, n := range networks {
		if !nets.HasNetwork(n, "") {
			return apierror.InvalidArgument("%s %q does not exist: create the network first (Compute Engine networks.insert)", field, n)
		}
	}
	return nil
}
