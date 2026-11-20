package gke

import (
	"sort"
	"strings"

	"cloud.google.com/go/container/apiv1/containerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Networks are the Compute Engine VPC networks and subnets (#1936) a
// cluster's network and subnetwork are checked against, as Google checks
// them, when the instance serves Compute Engine. Without it (nil) any
// network is accepted, as before. internal/service/compute's Server is
// one.
type Networks interface {
	// HasNetwork reports whether the network ref exists, in project when
	// ref is a bare name.
	HasNetwork(ref, project string) bool
	// Subnetwork is the network (projects/P/global/networks/N) of the
	// subnetwork ref, in project and region when ref is a bare name.
	Subnetwork(ref, project, region string) (network string, ok bool)
}

// checkClusterVPC refuses a cluster whose network does not exist, or
// whose subnetwork does not exist in the cluster's region or is not in
// its network
// (https://docs.cloud.google.com/kubernetes-engine/docs/reference/rest/v1/projects.locations.clusters,
// network and subnetwork, re-read 2026-11-19). The wording is UNVERIFIED.
func (s *Server) checkClusterVPC(c *containerpb.Cluster, project, location string) error {
	if s.opts.Networks == nil {
		return nil
	}
	nets := s.opts.Networks()
	if nets == nil {
		return nil
	}
	network := c.GetNetwork()
	if nc := c.GetNetworkConfig().GetNetwork(); nc != "" {
		network = nc
	}
	if network == "" {
		network = "default"
	}
	if !nets.HasNetwork(network, project) {
		return status.Errorf(codes.InvalidArgument, "Network %q does not exist.", network)
	}
	sub := c.GetSubnetwork()
	if nc := c.GetNetworkConfig().GetSubnetwork(); nc != "" {
		sub = nc
	}
	if sub == "" {
		sub = "default"
		if network != "default" && !strings.HasSuffix(network, "/networks/default") {
			// Google picks the network's subnet in the region named as
			// the network, an auto mode network's.
			sub = network[strings.LastIndex(network, "/")+1:]
		}
	}
	got, ok := nets.Subnetwork(sub, project, regionOf(location))
	if !ok {
		return status.Errorf(codes.InvalidArgument, "Subnetwork %q does not exist in region %s.", sub, regionOf(location))
	}
	if !strings.HasSuffix(got, "/networks/"+network[strings.LastIndex(network, "/")+1:]) {
		return status.Errorf(codes.InvalidArgument, "Subnetwork %q is not in network %q.", sub, network)
	}
	return nil
}

// NetworkUsers names the clusters on the network or subnetwork rel
// (projects/P/global/networks/N or projects/P/regions/R/subnetworks/S),
// which Compute Engine refuses to delete while they exist (#1936).
func (s *Server) NetworkUsers(rel string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for name, e := range s.clusters {
		nc := e.cluster.GetNetworkConfig()
		if nc.GetNetwork() == rel || nc.GetSubnetwork() == rel {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}
