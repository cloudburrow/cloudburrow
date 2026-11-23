package filestore

import (
	"encoding/binary"
	"net"
	"sort"
	"strconv"
	"strings"

	"cloud.google.com/go/filestore/apiv1/filestorepb"

	"google.golang.org/grpc/codes"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

func exhausted(format string, a ...any) *apierror.Error {
	e := apierror.InvalidArgument(format, a...)
	e.Code, e.Reason = codes.ResourceExhausted, "resourceExhausted"
	return e
}

// An instance's address (#2192), as
// https://docs.cloud.google.com/filestore/docs/creating-instances and the
// v1 reference describe it (read 2026-11-23): the instance takes a block
// of its VPC network, a /29 on the basic tiers and a /26 on the others,
// which "can't overlap with either existing subnets or assigned IP address
// ranges for other Filestore instances" and may not be in 172.17.0.0/16;
// with DIRECT_PEERING it is reserved_ip_range (chosen when unset), with
// PRIVATE_SERVICE_ACCESS a free block of the allocated range it names (or
// of any range allocated on the network), reserved_ip_range then keeping
// the range's name. The instance serves at the block's second host
// address, as Google's Filestore instances answer (10.0.0.2 for
// 10.0.0.0/29). The backend makes that address reach the instance's server
// from the network's VMs and Kubernetes Engine clusters.

// dockerReserved is the range creating-instances says instances cannot
// use.
var dockerReserved = &net.IPNet{IP: net.IPv4(172, 17, 0, 0).To4(), Mask: net.CIDRMask(16, 32)}

// networkRel is ref as projects/P/global/networks/N.
func networkRel(project, ref string) string {
	if strings.HasPrefix(ref, "projects/") {
		return ref
	}
	return "projects/" + project + "/global/networks/" + ref
}

// blockPrefix is the size of an instance's block.
func blockPrefix(t filestorepb.Instance_Tier) int {
	switch {
	case tiers[t].basic:
		return 29
	case t == filestorepb.Instance_HIGH_SCALE_SSD:
		return 24
	}
	return 26
}

func overlaps(a, b *net.IPNet) bool { return a.Contains(b.IP) || b.Contains(a.IP) }

// hostAddress is the block's second host address.
func hostAddress(block *net.IPNet) string {
	v := binary.BigEndian.Uint32(block.IP.To4()) + 2
	b := make(net.IP, 4)
	binary.BigEndian.PutUint32(b, v)
	return b.String()
}

// usedBlocksLocked are the blocks the network's other instances hold.
func (s *Server) usedBlocksLocked(network, except string) []*net.IPNet {
	var out []*net.IPNet
	for name, r := range s.instances {
		if name == except || r.Block == "" || len(r.I.GetNetworks()) == 0 {
			continue
		}
		if networkRel(projectOf(name), r.I.Networks[0].GetNetwork()) != network {
			continue
		}
		if _, b, err := net.ParseCIDR(r.Block); err == nil {
			out = append(out, b)
		}
	}
	return out
}

func projectOf(name string) string {
	p := strings.Split(name, "/")
	if len(p) < 2 {
		return ""
	}
	return p[1]
}

// placeLocked chooses the instance's block and address on its network:
// rec.Block and rec.Addr.
func (s *Server) placeLocked(in *filestorepb.Instance, project string, rec *instRecord) error {
	n := in.Networks[0]
	network := networkRel(project, n.GetNetwork())
	prefix := blockPrefix(in.GetTier())
	psa := n.GetConnectMode() == filestorepb.NetworkConfig_PRIVATE_SERVICE_ACCESS
	used := s.usedBlocksLocked(network, in.GetName())
	var nets Networks
	if s.opts.Networks != nil {
		nets = s.opts.Networks()
	}
	// free reports why block cannot be used, or "".
	free := func(block *net.IPNet) string {
		if overlaps(block, dockerReserved) {
			return "it overlaps 172.17.0.0/16, which Filestore instances cannot use"
		}
		for _, u := range used {
			if overlaps(block, u) {
				return "it overlaps the IP range " + u.String() + " of another Filestore instance on the network"
			}
		}
		if nets != nil {
			if c := nets.Conflict(network, block); c != "" && !(psa && strings.HasPrefix(c, "allocated range")) {
				return "it overlaps the " + c
			}
		}
		return ""
	}
	set := func(block *net.IPNet) {
		rec.Block, rec.Addr = block.String(), hostAddress(block)
	}
	r := n.GetReservedIpRange()
	if psa && !strings.Contains(r, "/") {
		if nets == nil {
			// No Compute Engine to read the allocated ranges from.
			_, block, _ := net.ParseCIDR(defaultRange(in.GetName(), prefix))
			set(block)
			return nil
		}
		ranges := map[string]*net.IPNet{}
		if r != "" {
			if a, ok := nets.AllocatedRange(network, r); ok {
				ranges[r] = a
			} else {
				// UNVERIFIED wording; the reference says only that the
				// range "must be one of the ranges associated with the
				// private service access connection" (re-read
				// 2026-11-23).
				return apierror.InvalidArgument("reserved_ip_range %q is not an IP range allocated for private services access on %s", r, network)
			}
		} else {
			ranges = nets.AllocatedRanges(network)
		}
		if len(ranges) == 0 {
			// UNVERIFIED wording (re-read 2026-11-23:
			// https://docs.cloud.google.com/filestore/docs/creating-instances).
			return apierror.FailedPrecondition("Network %s has no private services access connection: allocate an IP range for it (a global address with purpose VPC_PEERING) before creating an instance with connect mode PRIVATE_SERVICE_ACCESS", network)
		}
		names := make([]string, 0, len(ranges))
		for k := range ranges {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, name := range names {
			a := ranges[name]
			ones, bits := a.Mask.Size()
			if ones > prefix {
				continue
			}
			base := binary.BigEndian.Uint32(a.IP.To4())
			step := uint32(1) << (bits - prefix)
			for i := uint32(0); i < uint32(1)<<(prefix-ones); i++ {
				b := make(net.IP, 4)
				binary.BigEndian.PutUint32(b, base+i*step)
				block := &net.IPNet{IP: b, Mask: net.CIDRMask(prefix, 32)}
				if free(block) == "" {
					n.ReservedIpRange = name
					set(block)
					return nil
				}
			}
		}
		// The VPC page's wording for an exhausted allocated range
		// (configure-private-services-access, read 2026-11-23).
		return exhausted("Couldn't find free blocks in allocated IP ranges on %s.", network)
	}
	if r == "" {
		for i := 0; i < 64; i++ {
			seed := in.GetName()
			if i > 0 {
				seed += "#" + strconv.Itoa(i)
			}
			_, block, _ := net.ParseCIDR(defaultRange(seed, prefix))
			if free(block) == "" {
				n.ReservedIpRange = block.String()
				set(block)
				return nil
			}
		}
		return exhausted("No free /%d range was found in 10.0.0.0/8 on %s: set reserved_ip_range", prefix, network)
	}
	_, block, err := net.ParseCIDR(r)
	if err != nil {
		return apierror.InvalidArgument("reserved_ip_range %q is not a CIDR block", r)
	}
	if why := free(block); why != "" {
		// UNVERIFIED wording (re-read 2026-11-23: the reference's
		// reservedIpRange field).
		return apierror.InvalidArgument("reserved_ip_range %s cannot be used: %s", block, why)
	}
	set(block)
	return nil
}
