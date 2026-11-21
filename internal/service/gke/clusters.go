package gke

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"cloud.google.com/go/container/apiv1/containerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/emptypb"
)

type emptyResp = emptypb.Empty

// nameRE is a cluster's or a node pool's name: up to 40 characters,
// lowercase letters, numbers and hyphens, starting with a letter and ending
// with a letter or a number (re-read 2026-11-19:
// https://docs.cloud.google.com/kubernetes-engine/docs/reference/rest/v1/projects.locations.clusters#Cluster).
var nameRE = regexp.MustCompile(`^[a-z]([-a-z0-9]{0,38}[a-z0-9])?$`)

var (
	regionRE = regexp.MustCompile(`^[a-z]+-[a-z]+[0-9]+$`)
	zoneRE   = regexp.MustCompile(`^[a-z]+-[a-z]+[0-9]+-[a-z]$`)
)

func isZone(l string) bool   { return zoneRE.MatchString(l) }
func isRegion(l string) bool { return regionRE.MatchString(l) }

// regionOf is a zone's region, or a region itself.
func regionOf(l string) string {
	if isZone(l) {
		return l[:strings.LastIndex(l, "-")]
	}
	return l
}

// zonesOf is the zones a cluster's nodes run in: its zone, or three of its
// region's, as Google picks by default.
func zonesOf(location string) []string {
	if isZone(location) {
		return []string{location}
	}
	return []string{location + "-a", location + "-b", location + "-c"}
}

// checkName refuses a name outside Google's rule. The wording is
// UNVERIFIED (docs re-read 2026-11-19: https://docs.cloud.google.com/kubernetes-engine/docs/reference/rest/v1/projects.locations.clusters); the rule is the documented one.
func checkName(what, name string) error {
	if !nameRE.MatchString(name) {
		return status.Errorf(codes.InvalidArgument,
			"Invalid value for field %q: %q. Must be 1 to 40 characters, start with a letter, end with a number or a letter, and contain only lowercase letters, numbers and hyphens.",
			what, name)
	}
	return nil
}

func checkLocation(l string) error {
	if !isZone(l) && !isRegion(l) {
		return status.Errorf(codes.InvalidArgument, "Location %q is not a valid zone or region.", l)
	}
	return nil
}

// resolveVersion maps a requested version ("", "latest", "-", a minor such
// as "1.36", a patch "1.36.4" or a full GKE version) to one served.
func (s *Server) resolveVersion(field, v string) (Version, error) {
	if v == "" || v == "latest" || v == "-" {
		return s.opts.Versions[0], nil
	}
	for _, sv := range s.opts.Versions {
		if v == sv.GKE || strings.HasPrefix(sv.GKE, v+".") || strings.HasPrefix(sv.GKE, v+"-") {
			return sv, nil
		}
	}
	// UNVERIFIED wording (docs re-read 2026-11-19: https://docs.cloud.google.com/kubernetes-engine/docs/reference/rest/v1/projects.locations.clusters): Google names the version it refuses.
	return Version{}, status.Errorf(codes.InvalidArgument, "%s %q is unsupported.", field, v)
}

// poolNodes is a pool's nodes across the cluster's zones.
func poolNodes(np *containerpb.NodePool, zones int) int {
	return int(np.GetInitialNodeCount()) * zones
}

func clusterNodes(c *containerpb.Cluster) int {
	n := 0
	for _, np := range c.NodePools {
		n += poolNodes(np, len(np.Locations))
	}
	return n
}

// checkDisk refuses a new cluster or node below the free-space threshold.
func (s *Server) checkDisk() error {
	free, err := s.backend.FreeBytes()
	if err != nil {
		return status.Errorf(codes.Internal, "cannot read the free disk space: %v", err)
	}
	if free < s.opts.MinFreeBytes {
		return status.Errorf(codes.ResourceExhausted,
			"Not enough free disk space on this machine to run another local Kubernetes node: %d GiB free, %d GiB required. Delete clusters or free disk space, then retry.",
			free>>30, s.opts.MinFreeBytes>>30)
	}
	return nil
}

func (s *Server) newPool(np *containerpb.NodePool, zones []string, version Version, nameField string) (*containerpb.NodePool, error) {
	if err := checkName(nameField, np.GetName()); err != nil {
		return nil, err
	}
	if np.GetInitialNodeCount() < 0 {
		return nil, status.Errorf(codes.InvalidArgument, "Node pool %q: initial_node_count must not be negative.", np.GetName())
	}
	if np.GetInitialNodeCount() == 0 && !np.GetAutoscaling().GetEnabled() {
		return nil, status.Errorf(codes.InvalidArgument, "Node pool %q: initial_node_count must be greater than zero.", np.GetName())
	}
	if err := checkAutoscaling(np.GetAutoscaling(), len(zones), 0); err != nil {
		return nil, err
	}
	if len(np.GetLocations()) > 0 {
		return nil, status.Error(codes.Unimplemented, "Choosing a node pool's zones is not available locally.")
	}
	v := version
	if np.GetVersion() != "" {
		var err error
		if v, err = s.resolveVersion("Node version", np.GetVersion()); err != nil {
			return nil, err
		}
	}
	out := proto.Clone(np).(*containerpb.NodePool)
	out.Version = v.GKE
	out.Locations = append([]string(nil), zones...)
	if out.Config == nil {
		out.Config = &containerpb.NodeConfig{}
	}
	if out.Config.MachineType == "" {
		out.Config.MachineType = "e2-medium"
	}
	if out.Config.DiskSizeGb == 0 {
		out.Config.DiskSizeGb = 100
	}
	if out.Config.ImageType == "" {
		out.Config.ImageType = "COS_CONTAINERD"
	}
	if out.Config.DiskType == "" {
		out.Config.DiskType = "pd-balanced"
	}
	if out.Management == nil {
		out.Management = &containerpb.NodeManagement{AutoUpgrade: true, AutoRepair: true}
	}
	if err := checkUpgradeSettings(out.UpgradeSettings); err != nil {
		return nil, err
	}
	if out.UpgradeSettings == nil {
		out.UpgradeSettings = defaultUpgradeSettings()
	}
	if out.Autoscaling != nil {
		out.Autoscaling = normAutoscaling(out.Autoscaling)
	}
	out.Status = containerpb.NodePool_PROVISIONING
	return out, nil
}

// CreateCluster makes a cluster: a local Kubernetes cluster of its own, with
// a node per node pool node.
func (s *Server) CreateCluster(_ context.Context, req *containerpb.CreateClusterRequest) (*containerpb.Operation, error) {
	p, err := parseName(req.GetParent(), req.GetProjectId(), req.GetZone(), "", "", "", "")
	if err != nil {
		return nil, err
	}
	in := req.GetCluster()
	if in == nil {
		return nil, status.Error(codes.InvalidArgument, "Required \"cluster\" is missing.")
	}
	if err := checkName("cluster.name", in.GetName()); err != nil {
		return nil, err
	}
	if err := checkLocation(p.location); err != nil {
		return nil, err
	}
	version, err := s.resolveVersion("Master version", in.GetInitialClusterVersion())
	if err != nil {
		return nil, err
	}
	if len(in.GetLocations()) > 0 {
		return nil, status.Error(codes.Unimplemented, "Choosing a cluster's node zones (cluster.locations) is not available locally.")
	}
	c := proto.Clone(in).(*containerpb.Cluster)
	autopilot := c.GetAutopilot().GetEnabled()
	zones := zonesOf(p.location)
	if autopilot {
		// Autopilot clusters are regional, and Google manages their nodes
		// (re-read 2026-11-19:
		// https://docs.cloud.google.com/kubernetes-engine/docs/concepts/autopilot-overview).
		if !isRegion(p.location) {
			return nil, status.Error(codes.InvalidArgument, "Autopilot clusters must be regional clusters.")
		}
		if len(c.NodePools) > 0 || c.InitialNodeCount != 0 || c.NodeConfig != nil {
			return nil, status.Error(codes.InvalidArgument, "Autopilot clusters do not support node pools, initial_node_count or node_config: Google manages their nodes.")
		}
	} else {
		if len(c.NodePools) > 0 && (c.InitialNodeCount != 0 || c.NodeConfig != nil) {
			return nil, status.Error(codes.InvalidArgument, "Cluster.initial_node_count and Cluster.node_config must not be set when Cluster.node_pools is set.")
		}
		if len(c.NodePools) == 0 {
			if c.InitialNodeCount <= 0 {
				return nil, status.Error(codes.InvalidArgument, "Cluster.initial_node_count must be greater than zero.")
			}
			// The deprecated fields make one pool, default-pool.
			c.NodePools = []*containerpb.NodePool{{Name: "default-pool", InitialNodeCount: c.InitialNodeCount, Config: c.NodeConfig}}
		}
		seen := map[string]bool{}
		for i, np := range c.NodePools {
			out, err := s.newPool(np, zones, version, fmt.Sprintf("cluster.node_pools[%d].name", i))
			if err != nil {
				return nil, err
			}
			if seen[out.Name] {
				return nil, status.Errorf(codes.InvalidArgument, "Node pool %q is listed twice.", out.Name)
			}
			seen[out.Name] = true
			c.NodePools[i] = out
		}
		if n := clusterNodes(c); n > MaxNodesPerCluster {
			return nil, status.Errorf(codes.ResourceExhausted,
				"The cluster would have %d nodes (initial_node_count × %d zones); a local cluster may have at most %d. Use a zonal cluster or fewer nodes.",
				n, len(zones), MaxNodesPerCluster)
		}
	}
	if err := s.checkClusterVPC(c, p.project, p.location); err != nil {
		return nil, err
	}
	if err := checkCreateSettings(c); err != nil {
		return nil, err
	}
	if c.MaintenancePolicy != nil {
		if err := s.checkMaintenancePolicy(c.MaintenancePolicy); err != nil {
			return nil, err
		}
		c.MaintenancePolicy.ResourceVersion = shortHash(in.GetName() + s.stamp())
	}
	if err := s.checkDisk(); err != nil {
		return nil, err
	}

	name := resourceName(p.project, p.location, c.Name)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.clusters[name]; ok {
		return nil, status.Errorf(codes.AlreadyExists, "Already exists: %s.", name)
	}
	if len(s.clusters) >= MaxClusters {
		return nil, status.Errorf(codes.ResourceExhausted,
			"This CloudBurrow instance already runs %d Kubernetes Engine clusters, the most it runs at once. Delete one, then retry.", MaxClusters)
	}
	now := s.stamp()
	c.Location = p.location
	c.Zone = p.location
	c.Locations = zones
	c.SelfLink = s.selfLink(p.project, p.location, "clusters/"+c.Name)
	c.Id = shortHash(name+now) + shortHash(now+name)
	c.CreateTime = now
	c.Status = containerpb.Cluster_PROVISIONING
	c.InitialClusterVersion = version.GKE
	c.CurrentMasterVersion = version.GKE
	c.CurrentNodeVersion = oldestNodeVersion(c)
	if c.Network == "" {
		c.Network = "default"
	}
	if c.Subnetwork == "" {
		c.Subnetwork = "default"
	}
	if c.LoggingService == "" {
		c.LoggingService = "logging.googleapis.com/kubernetes"
	}
	if c.MonitoringService == "" {
		c.MonitoringService = "monitoring.googleapis.com/kubernetes"
	}
	c.ClusterIpv4Cidr = "10.244.0.0/16"
	c.ServicesIpv4Cidr = "10.96.0.0/16"
	if c.ReleaseChannel == nil || c.ReleaseChannel.Channel == containerpb.ReleaseChannel_UNSPECIFIED {
		c.ReleaseChannel = &containerpb.ReleaseChannel{Channel: containerpb.ReleaseChannel_REGULAR}
	}
	issue := c.GetMasterAuth().GetClientCertificateConfig().GetIssueClientCertificate()
	if c.GetMasterAuth().GetUsername() != "" || c.GetMasterAuth().GetPassword() != "" {
		return nil, status.Error(codes.InvalidArgument, "Basic authentication was removed for GKE cluster versions >= 1.19.")
	}
	c.MasterAuth = &containerpb.MasterAuth{ClientCertificateConfig: &containerpb.ClientCertificateConfig{IssueClientCertificate: issue}}
	// initial_node_count is kept as it was given, as Google returns it
	// (Terraform reads it back, and a 0 would replace the cluster); the
	// deprecated node_config is default-pool's.
	c.NodeConfig = nil
	// The network's and subnetwork's full names, which clients read from
	// networkConfig rather than the short network and subnetwork.
	if c.NetworkConfig == nil {
		c.NetworkConfig = &containerpb.NetworkConfig{}
	}
	if c.NetworkConfig.Network == "" {
		c.NetworkConfig.Network = "projects/" + p.project + "/global/networks/" + c.Network
	}
	if c.NetworkConfig.Subnetwork == "" {
		c.NetworkConfig.Subnetwork = "projects/" + p.project + "/regions/" + regionOf(p.location) + "/subnetworks/" + c.Subnetwork
	}
	if c.ResourceLabels == nil {
		c.ResourceLabels = map[string]string{}
	}
	c.LabelFingerprint = fingerprint(c.ResourceLabels)
	for _, np := range c.NodePools {
		np.SelfLink = c.SelfLink + "/nodePools/" + np.Name
		setInstanceGroups(p.project, c.Name, np)
	}
	c.CurrentNodeCount = int32(clusterNodes(c))
	e := &entry{cluster: c, project: p.project, kind: KindName(s.opts.Instance, name)}
	s.clusters[name] = e
	pools := make([]*containerpb.NodePool, len(c.NodePools))
	for i, np := range c.NodePools {
		pools[i] = proto.Clone(np).(*containerpb.NodePool)
	}
	kind := e.kind
	enforce, abac := enforcesNetworkPolicy(c), c.GetLegacyAbac().GetEnabled()
	return s.startOpLocked(p, containerpb.Operation_CREATE_CLUSTER, c.SelfLink, e, func(ctx context.Context) error {
		access, err := s.backend.Create(ctx, kind, version.Image, !autopilot)
		if err != nil {
			return status.Errorf(codes.Internal, "Creating the local cluster failed: %v", err)
		}
		s.mu.Lock()
		e.access = access
		s.mu.Unlock()
		for _, np := range pools {
			// A pool made on an older version than the control plane's
			// (node_pools[].version) runs that version's nodes.
			if err := s.backend.Resize(ctx, kind, s.NodeImage(np.Version), np.Name, poolNodes(np, len(np.Locations))); err != nil {
				return status.Errorf(codes.Internal, "Adding node pool %q's nodes failed: %v", np.Name, err)
			}
		}
		return s.applyCreateSettings(ctx, kind, enforce, abac)
	}, func(err error) {
		if err != nil {
			c.Status = containerpb.Cluster_ERROR
			c.StatusMessage = status.Convert(err).Message()
			for _, np := range c.NodePools {
				np.Status = containerpb.NodePool_ERROR
			}
			return
		}
		c.Status = containerpb.Cluster_RUNNING
		c.Endpoint = e.access.Endpoint
		c.MasterAuth.ClusterCaCertificate = e.access.CACert
		if issue {
			c.MasterAuth.ClientCertificate = e.access.ClientCert
			c.MasterAuth.ClientKey = e.access.ClientKey
		}
		for _, np := range c.NodePools {
			np.Status = containerpb.NodePool_RUNNING
		}
	}), nil
}

// view is the cluster as the API returns it.
func view(e *entry) *containerpb.Cluster {
	return proto.Clone(e.cluster).(*containerpb.Cluster)
}

// GetCluster returns a cluster.
func (s *Server) GetCluster(_ context.Context, req *containerpb.GetClusterRequest) (*containerpb.Cluster, error) {
	p, err := parseName(req.GetName(), req.GetProjectId(), req.GetZone(), req.GetClusterId(), "", "", "cluster")
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, err := s.lookupLocked(p)
	if err != nil {
		return nil, err
	}
	return view(e), nil
}

// ListClusters lists a location's clusters, or every location's for "-".
func (s *Server) ListClusters(_ context.Context, req *containerpb.ListClustersRequest) (*containerpb.ListClustersResponse, error) {
	p, err := parseName(req.GetParent(), req.GetProjectId(), req.GetZone(), "", "", "", "")
	if err != nil {
		return nil, err
	}
	if p.location != "-" {
		if err := checkLocation(p.location); err != nil {
			return nil, err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	resp := &containerpb.ListClustersResponse{}
	for _, e := range s.clusters {
		if e.project == p.project && (p.location == "-" || e.cluster.Location == p.location) {
			resp.Clusters = append(resp.Clusters, view(e))
		}
	}
	sort.Slice(resp.Clusters, func(i, j int) bool { return resp.Clusters[i].Name < resp.Clusters[j].Name })
	return resp, nil
}

// DeleteCluster deletes a cluster and its local cluster.
func (s *Server) DeleteCluster(_ context.Context, req *containerpb.DeleteClusterRequest) (*containerpb.Operation, error) {
	p, err := parseName(req.GetName(), req.GetProjectId(), req.GetZone(), req.GetClusterId(), "", "", "cluster")
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, err := s.lookupLocked(p)
	if err != nil {
		return nil, err
	}
	if e.busy != "" && e.cluster.Status != containerpb.Cluster_PROVISIONING {
		return nil, busyErr(e)
	}
	if e.cluster.Status == containerpb.Cluster_STOPPING {
		return nil, busyErr(e)
	}
	// Deleting a cluster still being made cancels its creation, as Google
	// does.
	if e.busy != "" {
		for _, o := range s.ops {
			if o.op.Name == e.busy && o.cancel != nil {
				o.cancel()
			}
		}
		e.busy = ""
	}
	e.cluster.Status = containerpb.Cluster_STOPPING
	name := resourceName(p.project, p.location, p.cluster)
	kind := e.kind
	return s.startOpLocked(p, containerpb.Operation_DELETE_CLUSTER, e.cluster.SelfLink, e, func(ctx context.Context) error {
		if err := s.backend.Delete(context.WithoutCancel(ctx), kind); err != nil {
			return status.Errorf(codes.Internal, "Deleting the local cluster failed: %v", err)
		}
		return nil
	}, func(err error) {
		if err != nil {
			e.cluster.Status = containerpb.Cluster_ERROR
			e.cluster.StatusMessage = status.Convert(err).Message()
			return
		}
		delete(s.clusters, name)
	}), nil
}

func busyErr(e *entry) error {
	if e.busy == autoscalerBusy {
		return status.Errorf(codes.FailedPrecondition, "Cluster %q is being resized by the cluster autoscaler; retry when it is done.", e.cluster.Name)
	}
	if e.busy != "" {
		return status.Errorf(codes.FailedPrecondition, "Cluster is running incompatible operation %s.", e.busy)
	}
	return status.Errorf(codes.FailedPrecondition, "Cluster %q is not ready: its status is %s.", e.cluster.Name, e.cluster.Status)
}

// readyLocked is the cluster, RUNNING and with no operation running.
func (s *Server) readyLocked(p parsed) (*entry, error) {
	e, err := s.lookupLocked(p)
	if err != nil {
		return nil, err
	}
	if e.busy != "" || (e.cluster.Status != containerpb.Cluster_RUNNING && e.cluster.Status != containerpb.Cluster_DEGRADED) {
		return nil, busyErr(e)
	}
	return e, nil
}

// doneOp records an operation that changed only records: it is DONE when
// returned, as the work is.
func (s *Server) doneOpLocked(p parsed, typ containerpb.Operation_Type, target string) *containerpb.Operation {
	op := s.startOpLocked(p, typ, target, nil, func(context.Context) error { return nil }, nil)
	return op
}

// supportedUpdates are the ClusterUpdate fields UpdateCluster applies; any
// other answers UNIMPLEMENTED, naming it.
var supportedUpdates = map[string]bool{
	"desired_master_version":        true,
	"desired_node_version":          true,
	"desired_node_pool_id":          true,
	"desired_image_type":            true,
	"desired_logging_service":       true,
	"desired_monitoring_service":    true,
	"desired_release_channel":       true,
	"desired_addons_config":         true,
	"desired_node_pool_autoscaling": true,
}

// UpdateCluster applies a ClusterUpdate: versions (to one served), the
// release channel, the logging and monitoring services, the image type.
func (s *Server) UpdateCluster(ctx context.Context, req *containerpb.UpdateClusterRequest) (*containerpb.Operation, error) {
	p, err := parseName(req.GetName(), req.GetProjectId(), req.GetZone(), req.GetClusterId(), "", "", "cluster")
	if err != nil {
		return nil, err
	}
	u := req.GetUpdate()
	if u == nil {
		return nil, status.Error(codes.InvalidArgument, "Required \"update\" is missing.")
	}
	var set []string
	var unsupported []string
	u.ProtoReflect().Range(func(fd protoreflect.FieldDescriptor, _ protoreflect.Value) bool {
		set = append(set, string(fd.Name()))
		if !supportedUpdates[string(fd.Name())] {
			unsupported = append(unsupported, string(fd.Name()))
		}
		return true
	})
	if len(set) == 0 {
		return nil, status.Error(codes.InvalidArgument, "The update is empty: set one desired_* field.")
	}
	if len(unsupported) > 0 {
		sort.Strings(unsupported)
		return nil, status.Errorf(codes.Unimplemented, "Not available locally: updating %s.", strings.Join(unsupported, ", "))
	}
	// The add-ons and a pool's autoscaling, as gcloud's --update-addons
	// and --enable-autoscaling send them (#2100): each its own change.
	if u.GetDesiredAddonsConfig() != nil || u.GetDesiredNodePoolAutoscaling() != nil {
		if u.GetDesiredAddonsConfig() != nil && len(set) == 1 {
			return s.SetAddonsConfig(ctx, &containerpb.SetAddonsConfigRequest{Name: req.GetName(), ProjectId: req.GetProjectId(), Zone: req.GetZone(),
				ClusterId: req.GetClusterId(), AddonsConfig: u.GetDesiredAddonsConfig()})
		}
		if u.GetDesiredNodePoolAutoscaling() != nil && len(set) == 2 && u.GetDesiredNodePoolId() != "" {
			return s.SetNodePoolAutoscaling(ctx, &containerpb.SetNodePoolAutoscalingRequest{
				Name: resourceName(p.project, p.location, p.cluster) + "/nodePools/" + u.GetDesiredNodePoolId(), Autoscaling: u.GetDesiredNodePoolAutoscaling()})
		}
		return nil, status.Error(codes.InvalidArgument, "desired_addons_config, or desired_node_pool_autoscaling with desired_node_pool_id, must be the only change of its update.")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, err := s.readyLocked(p)
	if err != nil {
		return nil, err
	}
	c := e.cluster
	typ := containerpb.Operation_UPDATE_CLUSTER
	if v := u.GetDesiredMasterVersion(); v != "" {
		sv, err := s.resolveVersion("Master version", v)
		if err != nil {
			return nil, err
		}
		typ = containerpb.Operation_UPGRADE_MASTER
		if sv.GKE != c.CurrentMasterVersion {
			// A real upgrade replaces the control plane's version: it is
			// the only change of its operation, as Google runs it.
			if len(set) > 1 {
				return nil, status.Error(codes.InvalidArgument, "desired_master_version must be the only field of an update that upgrades the control plane.")
			}
			if err := checkMasterUpgrade(c, sv); err != nil {
				return nil, err
			}
			up, err := s.upgrader()
			if err != nil {
				return nil, err
			}
			return s.startMasterUpgradeLocked(p, e, sv, up), nil
		}
	}
	if v := u.GetDesiredNodeVersion(); v != "" {
		sv, err := s.resolveNodeVersion(c, v)
		if err != nil {
			return nil, err
		}
		// desired_node_pool_id is required with more than one pool
		// (documented on the field).
		id := u.GetDesiredNodePoolId()
		if id == "" || id == "-" {
			if len(c.NodePools) != 1 {
				return nil, status.Error(codes.InvalidArgument, "desired_node_pool_id is required when the cluster has more than one node pool.")
			}
			id = c.NodePools[0].Name
		}
		np := findPool(c, id)
		if np == nil {
			return nil, notFound(resourceName(p.project, p.location, p.cluster) + "/nodePools/" + id)
		}
		typ = containerpb.Operation_UPGRADE_NODES
		if sv.GKE != np.Version {
			if err := autopilotRefusal(e); err != nil {
				return nil, err
			}
			if err := checkNodeUpgrade(c, sv); err != nil {
				return nil, err
			}
			up, err := s.upgrader()
			if err != nil {
				return nil, err
			}
			pp := p
			pp.pool = np.Name
			return s.startPoolUpgradeLocked(pp, e, np, sv, up, false), nil
		}
	}
	if t := u.GetDesiredImageType(); t != "" {
		if !validImageType(t) {
			return nil, status.Errorf(codes.InvalidArgument, "Image type %q is not supported.", t)
		}
		for _, np := range c.NodePools {
			if id := u.GetDesiredNodePoolId(); id == "" || id == "-" || id == np.Name {
				np.Config.ImageType = t
			}
		}
	}
	if v := u.GetDesiredLoggingService(); v != "" {
		c.LoggingService = v
	}
	if v := u.GetDesiredMonitoringService(); v != "" {
		c.MonitoringService = v
	}
	if rc := u.GetDesiredReleaseChannel(); rc != nil {
		c.ReleaseChannel = proto.Clone(rc).(*containerpb.ReleaseChannel)
	}
	return s.doneOpLocked(p, typ, c.SelfLink), nil
}

// UpdateMaster upgrades the control plane, to a version served.
func (s *Server) UpdateMaster(ctx context.Context, req *containerpb.UpdateMasterRequest) (*containerpb.Operation, error) {
	if req.GetMasterVersion() == "" {
		return nil, status.Error(codes.InvalidArgument, "Required \"master_version\" is missing.")
	}
	return s.UpdateCluster(ctx, &containerpb.UpdateClusterRequest{Name: req.GetName(), ProjectId: req.GetProjectId(), Zone: req.GetZone(),
		ClusterId: req.GetClusterId(), Update: &containerpb.ClusterUpdate{DesiredMasterVersion: req.GetMasterVersion()}})
}

var imageTypes = []string{"COS_CONTAINERD", "UBUNTU_CONTAINERD"}

func validImageType(t string) bool {
	for _, v := range imageTypes {
		if strings.EqualFold(t, v) {
			return true
		}
	}
	return false
}

func fingerprint(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k + "=" + labels[k] + ";")
	}
	return shortHash(b.String())
}

var labelKeyRE = regexp.MustCompile(`^[\p{Ll}\p{Lo}][\p{Ll}\p{Lo}\p{N}_-]{0,62}$`)
var labelValueRE = regexp.MustCompile(`^[\p{Ll}\p{Lo}\p{N}_-]{0,63}$`)

// SetLabels replaces a cluster's resource labels, given the fingerprint of
// the ones it has.
func (s *Server) SetLabels(_ context.Context, req *containerpb.SetLabelsRequest) (*containerpb.Operation, error) {
	p, err := parseName(req.GetName(), req.GetProjectId(), req.GetZone(), req.GetClusterId(), "", "", "cluster")
	if err != nil {
		return nil, err
	}
	if len(req.GetResourceLabels()) > 64 {
		return nil, status.Error(codes.InvalidArgument, "A cluster may have at most 64 labels.")
	}
	for k, v := range req.GetResourceLabels() {
		if !labelKeyRE.MatchString(k) || !labelValueRE.MatchString(v) {
			return nil, status.Errorf(codes.InvalidArgument, "Invalid label %q: %q.", k, v)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, err := s.readyLocked(p)
	if err != nil {
		return nil, err
	}
	if req.GetLabelFingerprint() != e.cluster.LabelFingerprint {
		// UNVERIFIED wording (docs re-read 2026-11-19: https://docs.cloud.google.com/kubernetes-engine/docs/reference/rest/v1/projects.locations.clusters); the fingerprint check is documented.
		return nil, status.Error(codes.FailedPrecondition, "Labels could not be set due to fingerprint mismatch.")
	}
	labels := map[string]string{}
	for k, v := range req.GetResourceLabels() {
		labels[k] = v
	}
	e.cluster.ResourceLabels = labels
	e.cluster.LabelFingerprint = fingerprint(labels)
	return s.doneOpLocked(p, containerpb.Operation_SET_LABELS, e.cluster.SelfLink), nil
}

// SetMasterAuth: basic authentication is gone from every version served
// (GKE 1.19 removed it), so only clearing the username is accepted.
func (s *Server) SetMasterAuth(_ context.Context, req *containerpb.SetMasterAuthRequest) (*containerpb.Operation, error) {
	p, err := parseName(req.GetName(), req.GetProjectId(), req.GetZone(), req.GetClusterId(), "", "", "cluster")
	if err != nil {
		return nil, err
	}
	if req.GetAction() == containerpb.SetMasterAuthRequest_UNKNOWN {
		return nil, status.Error(codes.InvalidArgument, "Required \"action\" is missing.")
	}
	clear := req.GetAction() == containerpb.SetMasterAuthRequest_SET_USERNAME && req.GetUpdate().GetUsername() == ""
	s.mu.Lock()
	defer s.mu.Unlock()
	e, err := s.readyLocked(p)
	if err != nil {
		return nil, err
	}
	if !clear {
		return nil, status.Error(codes.InvalidArgument, "Basic authentication was removed for GKE cluster versions >= 1.19.")
	}
	return s.doneOpLocked(p, containerpb.Operation_SET_MASTER_AUTH, e.cluster.SelfLink), nil
}

// GetServerConfig is the versions served, one per local node image, and
// the release channels, which all offer them.
func (s *Server) GetServerConfig(_ context.Context, req *containerpb.GetServerConfigRequest) (*containerpb.ServerConfig, error) {
	p, err := parseName(req.GetName(), req.GetProjectId(), req.GetZone(), "", "", "", "")
	if err != nil {
		return nil, err
	}
	if err := checkLocation(p.location); err != nil {
		return nil, err
	}
	var versions []string
	for _, v := range s.opts.Versions {
		versions = append(versions, v.GKE)
	}
	def := versions[0]
	cfg := &containerpb.ServerConfig{
		DefaultClusterVersion: def,
		ValidNodeVersions:     versions,
		DefaultImageType:      imageTypes[0],
		ValidImageTypes:       imageTypes,
		ValidMasterVersions:   versions,
	}
	for _, ch := range []containerpb.ReleaseChannel_Channel{containerpb.ReleaseChannel_RAPID, containerpb.ReleaseChannel_REGULAR,
		containerpb.ReleaseChannel_STABLE, containerpb.ReleaseChannel_EXTENDED} {
		cfg.Channels = append(cfg.Channels, &containerpb.ServerConfig_ReleaseChannelConfig{Channel: ch, DefaultVersion: def,
			ValidVersions: versions, UpgradeTargetVersion: def})
	}
	return cfg, nil
}

// NodeImage is the node image a served version runs.
func (s *Server) NodeImage(version string) string {
	for _, v := range s.opts.Versions {
		if v.GKE == version {
			return v.Image
		}
	}
	return s.opts.Versions[0].Image
}

// ClusterAccess is how this process reaches a RUNNING cluster's Kubernetes
// API (projects/P/locations/L/clusters/C), for Eventarc's GKE destinations
// (#1877); false when there is no such cluster or it is not running.
func (s *Server) ClusterAccess(name string) (Access, bool) {
	p, err := parseName(name, "", "", "", "", "", "cluster")
	if err != nil || p.cluster == "" {
		return Access{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, err := s.lookupLocked(p)
	if err != nil || e.cluster.GetStatus() != containerpb.Cluster_RUNNING || e.access.Endpoint == "" {
		return Access{}, false
	}
	return e.access, true
}
