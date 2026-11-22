package gke

import (
	"context"
	"fmt"
	"regexp"
	"sort"

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
	zones := append([]string(nil), np.Locations...)
	created := proto.Clone(np).(*containerpb.NodePool)
	return s.startOpLocked(p, containerpb.Operation_CREATE_NODE_POOL, np.SelfLink, e, func(ctx context.Context) error {
		if err := s.placePool(ctx, kind, name, zones); err != nil {
			return err
		}
		if err := s.configurePoolOf(ctx, kind, created); err != nil {
			return err
		}
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
// its upgrade settings and resource labels, and its zones, Kubernetes
// labels and taints (#2100), which act on its nodes; machine_type,
// disk_size_gb, disk_type and kubelet_config recreate the nodes.
func (s *Server) UpdateNodePool(_ context.Context, req *containerpb.UpdateNodePoolRequest) (*containerpb.Operation, error) {
	p, err := parseName(req.GetName(), req.GetProjectId(), req.GetZone(), req.GetClusterId(), req.GetNodePoolId(), "", "pool")
	if err != nil {
		return nil, err
	}
	kubelet, err := kubeletSettings("kubelet_config", req.GetKubeletConfig())
	if err != nil {
		return nil, err
	}
	if m := req.GetMachineType(); m != "" && !machineTypeRE.MatchString(m) {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid machine type %q.", m)
	}
	if d := req.GetDiskType(); d != "" && !diskTypes[d] {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid disk type %q.", d)
	}
	if d := req.GetDiskSizeGb(); d != 0 && d < minDiskSizeGb {
		return nil, status.Errorf(codes.InvalidArgument, "disk_size_gb %d is too small: the smallest allowed disk size is %dGB.", d, minDiskSizeGb)
	}
	var taints []NodeTaint
	if req.GetLabels() != nil {
		if err := checkNodeLabels("labels", req.GetLabels().GetLabels()); err != nil {
			return nil, err
		}
	}
	if req.GetTaints() != nil {
		if taints, err = nodeTaints("taints", req.GetTaints().GetTaints()); err != nil {
			return nil, err
		}
	}
	// The reference marks node_version and image_type required, but GKE
	// takes an update without them, keeping the pool's: gcloud's
	// `node-pools update --enable-blue-green-upgrade` (and its other
	// setting flags) sends neither.
	if req.GetImageType() != "" && !validImageType(req.GetImageType()) {
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
	nv := req.GetNodeVersion()
	if nv == "" {
		nv = np.Version
	}
	sv, err := s.resolveNodeVersion(e.cluster, nv)
	if err != nil {
		return nil, err
	}
	imageType := req.GetImageType()
	if imageType == "" {
		imageType = np.GetConfig().GetImageType()
	}
	if us := req.GetUpgradeSettings(); us != nil {
		// The settings apply to this upgrade too.
		np.UpgradeSettings = proto.Clone(us).(*containerpb.NodePool_UpgradeSettings)
		if np.UpgradeSettings.Strategy == nil {
			st := containerpb.NodePoolUpdateStrategy_SURGE
			np.UpgradeSettings.Strategy = &st
		}
	}
	nodes := len(req.GetLocations()) > 0 || req.GetLabels() != nil || req.GetTaints() != nil
	if sv.GKE != np.Version && nodes {
		// UNVERIFIED: gcloud sends a version and these in separate
		// requests; docs re-read 2026-11-22 (how-to/update-existing-nodepools)
		// give no rule for both at once.
		return nil, status.Error(codes.InvalidArgument, "Update a node pool's version separately from its locations, labels and taints.")
	}
	machine := req.GetMachineType() != "" && req.GetMachineType() != np.GetConfig().GetMachineType() ||
		req.GetDiskSizeGb() != 0 && req.GetDiskSizeGb() != int64(np.GetConfig().GetDiskSizeGb()) ||
		req.GetDiskType() != "" && req.GetDiskType() != np.GetConfig().GetDiskType() ||
		req.GetKubeletConfig() != nil && !proto.Equal(req.GetKubeletConfig(), np.GetConfig().GetKubeletConfig())
	if machine && (nodes || sv.GKE != np.Version) {
		// UNVERIFIED, as above (re-read 2026-11-22: how-to/update-existing-nodepools).
		return nil, status.Error(codes.InvalidArgument, "Update a node pool's machine type, disk and kubelet configuration separately from its version, locations, labels and taints.")
	}
	if machine {
		return s.recreatePoolLocked(p, e, np, req, sv, imageType, kubelet)
	}
	if nodes {
		return s.updatePoolNodesLocked(p, e, np, req, taints, imageType)
	}
	if sv.GKE != np.Version {
		if err := checkNodeUpgrade(e.cluster, sv); err != nil {
			return nil, err
		}
		up, err := s.upgrader()
		if err != nil {
			return nil, err
		}
		np.Config.ImageType = imageType
		return s.startPoolUpgradeLocked(p, e, np, sv, up, false), nil
	}
	np.Config.ImageType = imageType
	if rl := req.GetResourceLabels(); rl != nil {
		np.Config.ResourceLabels = rl.GetLabels()
	}
	return s.doneOpLocked(p, containerpb.Operation_UPGRADE_NODES, np.SelfLink), nil
}

// machineTypeRE is a Compute Engine machine type's form: a series and a
// shape (e2-medium, n2-standard-4, e2-custom-4-8192).
var machineTypeRE = regexp.MustCompile(`^[a-z][a-z0-9]*-[a-z0-9]+(-[a-z0-9]+)*$`)

// diskTypes are the node boot disk types (re-read 2026-11-22: NodeConfig.disk_type,
// "Type of the disk attached to each node (e.g. 'pd-standard', 'pd-ssd' or
// 'pd-balanced')", and how-to/custom-boot-disks' Hyperdisk Balanced).
var diskTypes = map[string]bool{"pd-standard": true, "pd-ssd": true, "pd-balanced": true, "pd-extreme": true, "hyperdisk-balanced": true}

// minDiskSizeGb is the smallest node disk: "The smallest allowed disk size
// is 10GB" (NodeConfig.disk_size_gb, REST reference).
const minDiskSizeGb = 10

// recreatePoolLocked changes a pool's machine type, disk size or type, or
// kubelet configuration (how-to/node-system-config: "This change requires
// re-creating the nodes"): "GKE
// recreates nodes using the node pool's upgrade strategy" (re-read
// 2026-11-22: how-to/update-existing-nodepools, Vertically scale by
// changing the node machine attributes), an UPGRADE_NODES operation that
// replaces every node, surge or blue-green, through the same steps as an
// upgrade. Locally a node has this machine's resources whatever its
// machine type; the new nodes carry it as node.kubernetes.io/instance-type.
func (s *Server) recreatePoolLocked(p parsed, e *entry, np *containerpb.NodePool, req *containerpb.UpdateNodePoolRequest,
	sv Version, imageType string, kubelet map[string]any) (*containerpb.Operation, error) {
	up, err := s.upgrader()
	if err != nil {
		return nil, err
	}
	if err := s.checkDisk(); err != nil {
		return nil, err
	}
	if m := req.GetMachineType(); m != "" {
		np.Config.MachineType = m
	}
	if d := req.GetDiskSizeGb(); d != 0 {
		np.Config.DiskSizeGb = int32(d)
	}
	if d := req.GetDiskType(); d != "" {
		np.Config.DiskType = d
	}
	if kc := req.GetKubeletConfig(); kc != nil {
		np.Config.KubeletConfig = kc
	} else if np.GetConfig().GetKubeletConfig() != nil {
		kubelet, _ = kubeletSettings("kubelet_config", np.GetConfig().GetKubeletConfig())
	}
	np.Config.ImageType = imageType
	if rl := req.GetResourceLabels(); rl != nil {
		np.Config.ResourceLabels = rl.GetLabels()
	}
	taints, _ := nodeTaints("taints", np.GetConfig().GetTaints())
	labels := nodeLabels(np.GetConfig())
	kind, pool := e.kind, np.Name
	return s.startPoolReplaceLocked(p, e, np, sv, up, false, func(ctx context.Context) error {
		// Recorded for the new nodes only: the old ones keep their
		// machine type and kubelet until they are replaced.
		if kubelet != nil {
			if err := s.configureKubelet(ctx, kind, pool, kubelet); err != nil {
				return err
			}
		}
		return s.configurePool(ctx, kind, pool, labels, taints, false)
	}), nil
}

// updatePoolNodesLocked moves a pool's nodes to its new zones
// (locations: "changing node pool locations will result in nodes being
// added and/or removed", REST reference, NodePool) and sets their
// Kubernetes labels and taints, in an UPGRADE_NODES operation ("includes
// most forms of updates to node pools", Operation.Type); s.mu is held.
func (s *Server) updatePoolNodesLocked(p parsed, e *entry, np *containerpb.NodePool, req *containerpb.UpdateNodePoolRequest,
	taints []NodeTaint, imageType string) (*containerpb.Operation, error) {
	c := e.cluster
	zones := append([]string(nil), np.Locations...)
	if locs := req.GetLocations(); len(locs) > 0 {
		zones = append([]string(nil), locs...)
		if err := checkPoolZones(regionOf(c.Location), zones); err != nil {
			return nil, err
		}
		sort.Strings(zones)
	}
	want, have := poolNodes(np, len(zones)), poolNodes(np, len(np.Locations))
	if total := clusterNodes(c) - have + want; total > MaxNodesPerCluster {
		return nil, status.Errorf(codes.ResourceExhausted, "The cluster would have %d nodes; a local cluster may have at most %d.", total, MaxNodesPerCluster)
	}
	if want > have {
		if err := s.checkDisk(); err != nil {
			return nil, err
		}
	}
	labels := np.GetConfig().GetLabels()
	if req.GetLabels() != nil {
		labels = req.GetLabels().GetLabels()
	}
	pbTaints := np.GetConfig().GetTaints()
	if req.GetTaints() != nil {
		pbTaints = req.GetTaints().GetTaints()
	} else {
		taints, _ = nodeTaints("taints", pbTaints)
	}
	moved := len(req.GetLocations()) > 0
	cfg := proto.Clone(np.GetConfig()).(*containerpb.NodeConfig)
	cfg.Labels = labels
	np.Status = containerpb.NodePool_RECONCILING
	c.Status = containerpb.Cluster_RECONCILING
	kind, image, name := e.kind, s.NodeImage(np.Version), np.Name
	return s.startOpLocked(p, containerpb.Operation_UPGRADE_NODES, np.SelfLink, e, func(ctx context.Context) error {
		if err := s.configurePool(ctx, kind, name, nodeLabels(cfg), taints, true); err != nil {
			return err
		}
		if !moved {
			return nil
		}
		if err := s.placePool(ctx, kind, name, zones); err != nil {
			return err
		}
		if err := s.backend.Resize(ctx, kind, image, name, want); err != nil {
			return status.Errorf(codes.Internal, "Resizing node pool %q failed: %v", name, err)
		}
		return s.placePool(ctx, kind, name, zones)
	}, func(err error) {
		c.Status = containerpb.Cluster_RUNNING
		if err != nil {
			np.Status = containerpb.NodePool_RUNNING_WITH_ERROR
			np.StatusMessage = status.Convert(err).Message()
			return
		}
		np.Status = containerpb.NodePool_RUNNING
		np.StatusMessage = ""
		np.Locations = zones
		np.Config.ImageType = imageType
		np.Config.Labels = labels
		np.Config.Taints = pbTaints
		if rl := req.GetResourceLabels(); rl != nil {
			np.Config.ResourceLabels = rl.GetLabels()
		}
		setInstanceGroups(p.project, c.Name, np)
		c.CurrentNodeCount = int32(clusterNodes(c))
	}), nil
}

// String is for tests.
func (p parsed) String() string {
	return fmt.Sprintf("%s/%s/%s/%s/%s", p.project, p.location, p.cluster, p.pool, p.op)
}
