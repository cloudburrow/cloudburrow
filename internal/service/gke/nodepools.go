package gke

import (
	"context"
	"fmt"

	"cloud.google.com/go/container/apiv1/containerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func findPool(c *containerpb.Cluster, name string) *containerpb.NodePool {
	for _, np := range c.NodePools {
		if np.Name == name {
			return np
		}
	}
	return nil
}

// autopilotRefusal: Google manages an Autopilot cluster's nodes, so its node
// pools cannot be changed (UNVERIFIED wording (docs re-read 2026-11-19: https://docs.cloud.google.com/kubernetes-engine/docs/reference/rest/v1/projects.locations.clusters)).
func autopilotRefusal(e *entry) error {
	if e.cluster.GetAutopilot().GetEnabled() {
		return status.Error(codes.FailedPrecondition, "This operation is not supported on Autopilot clusters: Google manages their nodes.")
	}
	return nil
}

func (s *Server) poolLocked(p parsed) (*entry, *containerpb.NodePool, error) {
	e, err := s.lookupLocked(p)
	if err != nil {
		return nil, nil, err
	}
	np := findPool(e.cluster, p.pool)
	if np == nil {
		return nil, nil, notFound(resourceName(p.project, p.location, p.cluster) + "/nodePools/" + p.pool)
	}
	return e, np, nil
}

// ListNodePools lists a cluster's node pools.
func (s *Server) ListNodePools(_ context.Context, req *containerpb.ListNodePoolsRequest) (*containerpb.ListNodePoolsResponse, error) {
	p, err := parseName(req.GetParent(), req.GetProjectId(), req.GetZone(), req.GetClusterId(), "", "", "cluster")
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, err := s.lookupLocked(p)
	if err != nil {
		return nil, err
	}
	resp := &containerpb.ListNodePoolsResponse{}
	for _, np := range e.cluster.NodePools {
		resp.NodePools = append(resp.NodePools, proto.Clone(np).(*containerpb.NodePool))
	}
	return resp, nil
}

// GetNodePool returns a node pool.
func (s *Server) GetNodePool(_ context.Context, req *containerpb.GetNodePoolRequest) (*containerpb.NodePool, error) {
	p, err := parseName(req.GetName(), req.GetProjectId(), req.GetZone(), req.GetClusterId(), req.GetNodePoolId(), "", "pool")
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, np, err := s.poolLocked(p)
	if err != nil {
		return nil, err
	}
	return proto.Clone(np).(*containerpb.NodePool), nil
}

// CreateNodePool adds a node pool, and its nodes to the local cluster.
func (s *Server) CreateNodePool(_ context.Context, req *containerpb.CreateNodePoolRequest) (*containerpb.Operation, error) {
	p, err := parseName(req.GetParent(), req.GetProjectId(), req.GetZone(), req.GetClusterId(), "", "", "cluster")
	if err != nil {
		return nil, err
	}
	if req.GetNodePool() == nil {
		return nil, status.Error(codes.InvalidArgument, "Required \"node_pool\" is missing.")
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
	cur, _ := s.resolveVersion("Node version", c.CurrentNodeVersion)
	np, err := s.newPool(req.GetNodePool(), c.Locations, cur, "node_pool.name")
	if err != nil {
		return nil, err
	}
	if findPool(c, np.Name) != nil {
		return nil, status.Errorf(codes.AlreadyExists, "Already exists: %s/nodePools/%s.", resourceName(p.project, p.location, p.cluster), np.Name)
	}
	n := poolNodes(np, len(np.Locations))
	if total := clusterNodes(c) + n; total > MaxNodesPerCluster {
		return nil, status.Errorf(codes.ResourceExhausted, "The cluster would have %d nodes; a local cluster may have at most %d.", total, MaxNodesPerCluster)
	}
	if err := s.checkDisk(); err != nil {
		return nil, err
	}
	np.SelfLink = c.SelfLink + "/nodePools/" + np.Name
	setInstanceGroups(p.project, c.Name, np)
	c.NodePools = append(c.NodePools, np)
	c.Status = containerpb.Cluster_RECONCILING
	kind, image, name := e.kind, s.NodeImage(np.Version), np.Name
	return s.startOpLocked(p, containerpb.Operation_CREATE_NODE_POOL, np.SelfLink, e, func(ctx context.Context) error {
		if err := s.backend.Resize(ctx, kind, image, name, n); err != nil {
			return status.Errorf(codes.Internal, "Adding node pool %q's nodes failed: %v", name, err)
		}
		return nil
	}, func(err error) {
		c.Status = containerpb.Cluster_RUNNING
		if err != nil {
			np.Status = containerpb.NodePool_ERROR
			np.StatusMessage = status.Convert(err).Message()
			return
		}
		np.Status = containerpb.NodePool_RUNNING
		c.CurrentNodeCount = int32(clusterNodes(c))
	}), nil
}

// DeleteNodePool removes a node pool and its nodes.
func (s *Server) DeleteNodePool(_ context.Context, req *containerpb.DeleteNodePoolRequest) (*containerpb.Operation, error) {
	p, err := parseName(req.GetName(), req.GetProjectId(), req.GetZone(), req.GetClusterId(), req.GetNodePoolId(), "", "pool")
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.readyLocked(p); err != nil {
		if _, _, perr := s.poolLocked(p); perr != nil {
			return nil, perr
		}
		return nil, err
	}
	e, np, err := s.poolLocked(p)
	if err != nil {
		return nil, err
	}
	if err := autopilotRefusal(e); err != nil {
		return nil, err
	}
	c := e.cluster
	np.Status = containerpb.NodePool_STOPPING
	c.Status = containerpb.Cluster_RECONCILING
	kind, image, name := e.kind, s.NodeImage(np.Version), np.Name
	return s.startOpLocked(p, containerpb.Operation_DELETE_NODE_POOL, np.SelfLink, e, func(ctx context.Context) error {
		if err := s.backend.Resize(ctx, kind, image, name, 0); err != nil {
			return status.Errorf(codes.Internal, "Removing node pool %q's nodes failed: %v", name, err)
		}
		return nil
	}, func(err error) {
		c.Status = containerpb.Cluster_RUNNING
		if err != nil {
			np.Status = containerpb.NodePool_ERROR
			np.StatusMessage = status.Convert(err).Message()
			return
		}
		for i, x := range c.NodePools {
			if x == np {
				c.NodePools = append(c.NodePools[:i], c.NodePools[i+1:]...)
				break
			}
		}
		c.CurrentNodeCount = int32(clusterNodes(c))
	}), nil
}

// SetNodePoolSize makes a pool have node_count nodes per zone, adding or
// removing the local cluster's nodes.
func (s *Server) SetNodePoolSize(_ context.Context, req *containerpb.SetNodePoolSizeRequest) (*containerpb.Operation, error) {
	p, err := parseName(req.GetName(), req.GetProjectId(), req.GetZone(), req.GetClusterId(), req.GetNodePoolId(), "", "pool")
	if err != nil {
		return nil, err
	}
	if req.GetNodeCount() < 0 {
		return nil, status.Error(codes.InvalidArgument, "node_count must not be negative.")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, _, err := s.poolLocked(p); err != nil {
		return nil, err
	}
	e, err := s.readyLocked(p)
	if err != nil {
		return nil, err
	}
	if err := autopilotRefusal(e); err != nil {
		return nil, err
	}
	c := e.cluster
	np := findPool(c, p.pool)
	zones := len(np.Locations)
	want := int(req.GetNodeCount()) * zones
	have := poolNodes(np, zones)
	if total := clusterNodes(c) - have + want; total > MaxNodesPerCluster {
		return nil, status.Errorf(codes.ResourceExhausted, "The cluster would have %d nodes; a local cluster may have at most %d.", total, MaxNodesPerCluster)
	}
	if want > have {
		if err := s.checkDisk(); err != nil {
			return nil, err
		}
	}
	np.Status = containerpb.NodePool_RECONCILING
	c.Status = containerpb.Cluster_RECONCILING
	kind, image, name := e.kind, s.NodeImage(np.Version), np.Name
	count := req.GetNodeCount()
	return s.startOpLocked(p, containerpb.Operation_SET_NODE_POOL_SIZE, np.SelfLink, e, func(ctx context.Context) error {
		if err := s.backend.Resize(ctx, kind, image, name, want); err != nil {
			return status.Errorf(codes.Internal, "Resizing node pool %q failed: %v", name, err)
		}
		return nil
	}, func(err error) {
		c.Status = containerpb.Cluster_RUNNING
		if err != nil {
			np.Status = containerpb.NodePool_RUNNING_WITH_ERROR
			np.StatusMessage = status.Convert(err).Message()
			return
		}
		np.Status = containerpb.NodePool_RUNNING
		np.InitialNodeCount = count
		c.CurrentNodeCount = int32(clusterNodes(c))
	}), nil
}

// UpdateNodePool changes a pool's version (to one served) and image type,
// and its resource labels; any other field answers UNIMPLEMENTED.
func (s *Server) UpdateNodePool(_ context.Context, req *containerpb.UpdateNodePoolRequest) (*containerpb.Operation, error) {
	p, err := parseName(req.GetName(), req.GetProjectId(), req.GetZone(), req.GetClusterId(), req.GetNodePoolId(), "", "pool")
	if err != nil {
		return nil, err
	}
	other := []string{}
	if len(req.GetLocations()) > 0 {
		other = append(other, "locations")
	}
	if req.GetLabels() != nil {
		other = append(other, "labels")
	}
	if req.GetTaints() != nil {
		other = append(other, "taints")
	}
	if req.GetMachineType() != "" {
		other = append(other, "machine_type")
	}
	if req.GetDiskSizeGb() != 0 {
		other = append(other, "disk_size_gb")
	}
	if req.GetKubeletConfig() != nil {
		other = append(other, "kubelet_config")
	}
	if len(other) > 0 {
		return nil, status.Errorf(codes.Unimplemented, "Not available locally: updating a node pool's %v.", other)
	}
	if req.GetNodeVersion() == "" || req.GetImageType() == "" {
		// Both are required by the API.
		return nil, status.Error(codes.InvalidArgument, "Required \"node_version\" and \"image_type\" must be set.")
	}
	if !validImageType(req.GetImageType()) {
		return nil, status.Errorf(codes.InvalidArgument, "Image type %q is not supported.", req.GetImageType())
	}
	if err := checkUpgradeSettings(req.GetUpgradeSettings()); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, _, err := s.poolLocked(p); err != nil {
		return nil, err
	}
	e, err := s.readyLocked(p)
	if err != nil {
		return nil, err
	}
	if err := autopilotRefusal(e); err != nil {
		return nil, err
	}
	np := findPool(e.cluster, p.pool)
	sv, err := s.resolveNodeVersion(e.cluster, req.GetNodeVersion())
	if err != nil {
		return nil, err
	}
	if us := req.GetUpgradeSettings(); us != nil {
		// The settings apply to this upgrade too.
		np.UpgradeSettings = proto.Clone(us).(*containerpb.NodePool_UpgradeSettings)
		if np.UpgradeSettings.Strategy == nil {
			st := containerpb.NodePoolUpdateStrategy_SURGE
			np.UpgradeSettings.Strategy = &st
		}
	}
	if sv.GKE != np.Version {
		if err := checkNodeUpgrade(e.cluster, sv); err != nil {
			return nil, err
		}
		up, err := s.upgrader()
		if err != nil {
			return nil, err
		}
		np.Config.ImageType = req.GetImageType()
		return s.startPoolUpgradeLocked(p, e, np, sv, up), nil
	}
	np.Config.ImageType = req.GetImageType()
	if rl := req.GetResourceLabels(); rl != nil {
		np.Config.ResourceLabels = rl.GetLabels()
	}
	return s.doneOpLocked(p, containerpb.Operation_UPGRADE_NODES, np.SelfLink), nil
}

// String is for tests.
func (p parsed) String() string {
	return fmt.Sprintf("%s/%s/%s/%s/%s", p.project, p.location, p.cluster, p.pool, p.op)
}
