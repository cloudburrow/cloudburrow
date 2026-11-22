package gke

import (
	"context"
	"sort"

	"cloud.google.com/go/container/apiv1/containerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// SetLocations (#2100) changes the zones a cluster's nodes run in, as GKE
// does (re-read 2026-11-21:
// https://docs.cloud.google.com/kubernetes-engine/docs/how-to/managing-clusters):
// a zonal cluster's zones include its own ("--node-locations must contain
// the cluster's primary zone"), every zone is in the cluster's region,
// and "nodes are immediately added or removed": every node pool has its
// node count in each zone, so each pool is resized to it.
//
// Locally a zone is a label: each node of a pool carries
// topology.kubernetes.io/zone (and topology.kubernetes.io/region), from
// the moment its pool is made. A new node goes to the pool's zone with the
// fewest nodes and keeps it; removing a zone deletes that zone's nodes, as
// GKE does, and the other nodes keep their zones.

// Placer is what a Backend does for zones.
type Placer interface {
	// PlaceNodes labels pool's nodes, and those it gets later, with zones
	// in turn.
	PlaceNodes(ctx context.Context, kind, pool string, zones []string) error
}

// placePool records a new pool's zones before its nodes are made, so each
// is labelled with its zone; a backend with no zones does nothing.
func (s *Server) placePool(ctx context.Context, kind, pool string, zones []string) error {
	placer, ok := s.backend.(Placer)
	if !ok || len(zones) == 0 {
		return nil
	}
	if err := placer.PlaceNodes(ctx, kind, pool, zones); err != nil {
		return status.Errorf(codes.Internal, "Placing node pool %q's nodes failed: %v", pool, err)
	}
	return nil
}

// checkZones refuses zones GKE refuses for a cluster in location.
func checkZones(location string, zones []string) error {
	if len(zones) == 0 {
		return status.Error(codes.InvalidArgument, "Required \"locations\" is missing.")
	}
	seen := map[string]bool{}
	for _, z := range zones {
		if !isZone(z) {
			return status.Errorf(codes.InvalidArgument, "Location %q is not a zone.", z)
		}
		if regionOf(z) != regionOf(location) {
			// UNVERIFIED wording; the rule is documented.
			// Docs re-read 2026-11-21: nothing more is published.
			return status.Errorf(codes.InvalidArgument, "Zone %q is not in the cluster's region %s: all zones must be in the same region as the cluster's control plane.", z, regionOf(location))
		}
		if seen[z] {
			return status.Errorf(codes.InvalidArgument, "Zone %q is listed twice.", z)
		}
		seen[z] = true
	}
	if isZone(location) && !seen[location] {
		return status.Errorf(codes.InvalidArgument, "The locations of a zonal cluster must contain its primary zone %s.", location)
	}
	return nil
}

// checkPoolZones refuses a node pool's own zones (node_pool.locations)
// outside the cluster's region, or listed twice. Unlike the cluster's,
// they need not include its primary zone: the reference sets no such rule
// (re-read 2026-11-22: rest/v1/projects.locations.clusters.nodePools).
func checkPoolZones(region string, zones []string) error {
	seen := map[string]bool{}
	for _, z := range zones {
		if !isZone(z) {
			return status.Errorf(codes.InvalidArgument, "Location %q is not a zone.", z)
		}
		if regionOf(z) != region {
			// UNVERIFIED wording, as checkZones' (re-read 2026-11-22:
			// rest/v1/projects.locations.clusters.nodePools quotes none).
			return status.Errorf(codes.InvalidArgument, "Zone %q is not in the cluster's region %s: all zones must be in the same region as the cluster's control plane.", z, region)
		}
		if seen[z] {
			return status.Errorf(codes.InvalidArgument, "Zone %q is listed twice.", z)
		}
		seen[z] = true
	}
	return nil
}

// SetLocations sets the zones of the cluster's nodes.
func (s *Server) SetLocations(_ context.Context, req *containerpb.SetLocationsRequest) (*containerpb.Operation, error) {
	p, err := parseName(req.GetName(), req.GetProjectId(), req.GetZone(), req.GetClusterId(), "", "", "cluster")
	if err != nil {
		return nil, err
	}
	zones := append([]string(nil), req.GetLocations()...)
	if err := checkZones(p.location, zones); err != nil {
		return nil, err
	}
	sort.Strings(zones)
	placer, ok := s.backend.(Placer)
	if !ok {
		return nil, status.Error(codes.Unimplemented, "Choosing a cluster's zones is not available with this backend.")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, err := s.readyLocked(p)
	if err != nil {
		return nil, err
	}
	if err := autopilotRefusal(e); err != nil {
		return nil, err
	}
	c := e.cluster
	total, grows := 0, false
	for _, np := range c.NodePools {
		// A pool's node count is per zone.
		n := poolNodes(np, len(zones))
		total += n
		grows = grows || n > poolNodes(np, len(np.Locations))
	}
	if total > MaxNodesPerCluster {
		return nil, status.Errorf(codes.ResourceExhausted, "The cluster would have %d nodes; a local cluster may have at most %d.", total, MaxNodesPerCluster)
	}
	if grows {
		if err := s.checkDisk(); err != nil {
			return nil, err
		}
	}
	type pool struct {
		name, image string
		n           int
	}
	var pools []pool
	for _, np := range c.NodePools {
		pools = append(pools, pool{np.Name, s.NodeImage(np.Version), poolNodes(np, len(zones))})
		np.Status = containerpb.NodePool_RECONCILING
	}
	kind := e.kind
	c.Status = containerpb.Cluster_RECONCILING
	return s.startOpLocked(p, containerpb.Operation_UPDATE_CLUSTER, c.SelfLink, e, func(ctx context.Context) error {
		for _, pl := range pools {
			// Placed first, so the nodes a resize adds get their zones.
			if err := placer.PlaceNodes(ctx, kind, pl.name, zones); err != nil {
				return status.Errorf(codes.Internal, "Placing node pool %q's nodes failed: %v", pl.name, err)
			}
			if err := s.backend.Resize(ctx, kind, pl.image, pl.name, pl.n); err != nil {
				return status.Errorf(codes.Internal, "Resizing node pool %q failed: %v", pl.name, err)
			}
			if err := placer.PlaceNodes(ctx, kind, pl.name, zones); err != nil {
				return status.Errorf(codes.Internal, "Placing node pool %q's nodes failed: %v", pl.name, err)
			}
		}
		return nil
	}, func(err error) {
		c.Status = containerpb.Cluster_RUNNING
		for _, np := range c.NodePools {
			np.Status = containerpb.NodePool_RUNNING
		}
		if err != nil {
			c.StatusMessage = status.Convert(err).Message()
			return
		}
		c.StatusMessage = ""
		c.Locations = zones
		for _, np := range c.NodePools {
			np.Locations = append([]string(nil), zones...)
			setInstanceGroups(p.project, c.Name, np)
		}
	}), nil
}
