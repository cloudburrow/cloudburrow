package dataproc

// Node groups (#2162): a cluster's auxiliary node groups, as the driver
// node groups page documents them
// (https://docs.cloud.google.com/dataproc/docs/guides/node-groups/dataproc-driver-node-groups,
// read 2026-11-22), and projects.regions.clusters.nodeGroups (create,
// resize, get). The one role is DRIVER: its nodes run the jobs' drivers,
// and a cluster with one needs each Spark job's driver_scheduling_config.
// "Clusters with node groups cannot be stopped, restarted, or autoscaled",
// and "node groups are not supported in workflow templates".

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	dataprocpb "cloud.google.com/go/dataproc/v2/apiv1/dataprocpb"
	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// nodeGroupIDRE: "The node group ID must contain only letters (a-z, A-Z),
// numbers (0-9), underscores (_), and hyphens (-). Cannot begin or end
// with underscore or hyphen. Must consist of from 3 to 33 characters."
var nodeGroupIDRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{1,31}[A-Za-z0-9]$`)

var nodeGroupRE = regexp.MustCompile(`^projects/([^/]+)/regions/([^/]+)/clusters/([^/]+)/nodeGroups/([^/]+)$`)

// nodeGroupName is a node group's resource name.
func nodeGroupName(project, region, cluster, id string) string {
	return clusterKey(project, region, cluster) + "/nodeGroups/" + id
}

// checkNodeGroup checks a node group to create, and fills in its
// defaults: its name, and its config's instances (CLUSTER-np-XXXX, four
// letters and digits, the page's example's form), machine type (n1-standard-4) and preemptibility.
func checkNodeGroup(project, region, cluster, id string, ng *dataprocpb.NodeGroup) error {
	if ng == nil {
		return apierror.InvalidArgument("node_group is required")
	}
	if !nodeGroupIDRE.MatchString(id) {
		return apierror.InvalidArgument("Node group ID %q must contain only letters, numbers, underscores and hyphens, not begin or end with an underscore or hyphen, and be 3 to 33 characters", id)
	}
	if len(ng.GetRoles()) != 1 || ng.GetRoles()[0] != dataprocpb.NodeGroup_DRIVER {
		return apierror.InvalidArgument("Node group %s must have the one role DRIVER", id)
	}
	if ng.NodeGroupConfig == nil {
		ng.NodeGroupConfig = &dataprocpb.InstanceGroupConfig{}
	}
	cfg := ng.NodeGroupConfig
	if cfg.GetNumInstances() < 0 {
		return apierror.InvalidArgument("node_group_config.num_instances must not be negative")
	}
	if cfg.MachineTypeUri == "" {
		cfg.MachineTypeUri = "https://www.googleapis.com/compute/v1/projects/" + project + "/zones/" + region + "-a/machineTypes/n1-standard-4"
	}
	if cfg.Preemptibility == dataprocpb.InstanceGroupConfig_PREEMPTIBILITY_UNSPECIFIED {
		cfg.Preemptibility = dataprocpb.InstanceGroupConfig_NON_PREEMPTIBLE
	}
	if cfg.Preemptibility != dataprocpb.InstanceGroupConfig_NON_PREEMPTIBLE {
		return apierror.InvalidArgument("Node group %s: driver node groups are non-preemptible", id)
	}
	resizeInstances(cluster, cfg, int(cfg.GetNumInstances()))
	ng.Name = nodeGroupName(project, region, cluster, id)
	return nil
}

// resizeInstances sets a node group config's instances to n: its first
// n names kept, new ones added.
func resizeInstances(cluster string, cfg *dataprocpb.InstanceGroupConfig, n int) {
	names := cfg.GetInstanceNames()
	if len(names) > n {
		names = names[:n]
	}
	for len(names) < n {
		names = append(names, cluster+"-np-"+randomSuffix(4))
	}
	cfg.InstanceNames, cfg.NumInstances = names, int32(n)
}

// checkAuxiliaryGroups checks a cluster's auxiliary node groups.
func checkAuxiliaryGroups(project, region string, c *dataprocpb.Cluster) error {
	groups := c.GetConfig().GetAuxiliaryNodeGroups()
	if len(groups) == 0 {
		return nil
	}
	if c.GetConfig().GetAutoscalingConfig().GetPolicyUri() != "" {
		return apierror.InvalidArgument("Clusters with node groups cannot be autoscaled")
	}
	seen := map[string]bool{}
	for _, g := range groups {
		if g.NodeGroupId == "" {
			g.NodeGroupId = "np-" + randomSuffix(4)
		}
		if seen[g.NodeGroupId] {
			return apierror.InvalidArgument("Node group ID %q must be unique within the cluster", g.NodeGroupId)
		}
		seen[g.NodeGroupId] = true
		if err := checkNodeGroup(project, region, c.GetClusterName(), g.NodeGroupId, g.NodeGroup); err != nil {
			return err
		}
	}
	return nil
}

// driverGroup is a cluster's DRIVER node group, or nil.
func driverGroup(c *dataprocpb.Cluster) *dataprocpb.AuxiliaryNodeGroup {
	for _, g := range c.GetConfig().GetAuxiliaryNodeGroups() {
		for _, r := range g.GetNodeGroup().GetRoles() {
			if r == dataprocpb.NodeGroup_DRIVER {
				return g
			}
		}
	}
	return nil
}

// checkDriverScheduling checks a job's driver_scheduling_config against
// its cluster, with the page's errors.
func checkDriverScheduling(j *dataprocpb.Job, c *dataprocpb.Cluster) error {
	ds := j.GetDriverSchedulingConfig()
	if ds != nil && (ds.GetMemoryMb() <= 0 || ds.GetVcores() <= 0) {
		return apierror.InvalidArgument("Positive values are required for all driver scheduling config values.")
	}
	if ds == nil && driverGroup(c) != nil {
		kind := strings.ToUpper(JobType(j))
		return apierror.InvalidArgument("Cluster %s requires driver scheduling config to run %s job because it contains a node pool with role DRIVER.", c.ClusterName, kind)
	}
	return nil
}

// placeDriverLocked is the driver node a job's driver runs on: its
// cluster's driver node group's nodes in turn, or "" for none.
func (s *Server) placeDriverLocked(c *dataprocpb.Cluster) string {
	names := driverGroup(c).GetNodeGroup().GetNodeGroupConfig().GetInstanceNames()
	if len(names) == 0 {
		return ""
	}
	if s.driverNext == nil {
		s.driverNext = map[string]int{}
	}
	n := s.driverNext[c.ClusterUuid]
	s.driverNext[c.ClusterUuid] = n + 1
	return names[n%len(names)]
}

func (s *Server) nodeGroupLocked(name string) (*dataprocpb.Cluster, *dataprocpb.AuxiliaryNodeGroup, error) {
	m := nodeGroupRE.FindStringSubmatch(name)
	if m == nil {
		return nil, nil, apierror.InvalidArgument("Invalid node group name %q: projects/PROJECT/regions/REGION/clusters/CLUSTER/nodeGroups/NODE_GROUP_ID", name)
	}
	c, err := s.clusterLocked(m[1], m[2], m[3], "")
	if err != nil {
		return nil, nil, err
	}
	for _, g := range c.GetConfig().GetAuxiliaryNodeGroups() {
		if g.GetNodeGroupId() == m[4] {
			return c, g, nil
		}
	}
	return nil, nil, apierror.NotFound("Not found: Node group %s", name)
}

func (s *Server) GetNodeGroup(_ context.Context, req *dataprocpb.GetNodeGroupRequest) (*dataprocpb.NodeGroup, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, g, err := s.nodeGroupLocked(req.GetName())
	if err != nil {
		return nil, err
	}
	return clone(g.GetNodeGroup()), nil
}

func (s *Server) newNodeGroupOpLocked(project, region string, c *dataprocpb.Cluster, id string, kind dataprocpb.NodeGroupOperationMetadata_NodeGroupOperationType, desc string, labels map[string]string) *longrunningpb.Operation {
	md := &dataprocpb.NodeGroupOperationMetadata{NodeGroupId: id, ClusterUuid: c.ClusterUuid, OperationType: kind, Description: desc, Labels: labels,
		Status: &dataprocpb.ClusterOperationStatus{State: dataprocpb.ClusterOperationStatus_RUNNING, InnerState: "RUNNING", StateStartTime: timestamppb.New(s.now())}}
	return s.newOpLocked(opName(project, region), md)
}

// applyNodeGroup starts or resizes a node group's nodes on the cluster's
// engine, then ends the operation opn with the group.
func (s *Server) applyNodeGroup(clusterName, id, opn string) {
	defer s.wg.Done()
	s.mu.Lock()
	c := s.clusters[clusterName]
	nodes := s.nodes[clusterName]
	var g *dataprocpb.AuxiliaryNodeGroup
	if c != nil {
		for _, x := range c.GetConfig().GetAuxiliaryNodeGroups() {
			if x.GetNodeGroupId() == id {
				g = x
			}
		}
	}
	s.mu.Unlock()
	var err error
	if c == nil || g == nil {
		err = status.Error(codes.NotFound, "the node group's cluster was deleted")
	} else if nodes != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		err = nodes.SetNodeGroup(ctx, id, g.GetNodeGroup().GetNodeGroupConfig().GetInstanceNames())
		cancel()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.finishOpLocked(opn, nil, status.Error(codes.Unknown, "Node group "+id+": "+err.Error()))
	} else {
		s.finishOpLocked(opn, clone(g.GetNodeGroup()), nil)
	}
	s.save()
}

func (s *Server) CreateNodeGroup(_ context.Context, req *dataprocpb.CreateNodeGroupRequest) (*longrunningpb.Operation, error) {
	m := clusterRE.FindStringSubmatch(req.GetParent())
	if m == nil {
		return nil, apierror.InvalidArgument("Invalid parent %q: projects/PROJECT/regions/REGION/clusters/CLUSTER", req.GetParent())
	}
	if r := req.GetRequestId(); r != "" && !requestIDRE.MatchString(r) {
		return nil, apierror.InvalidArgument("request_id %q must be at most 40 letters, digits, underscores and hyphens", r)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.clusterLocked(m[1], m[2], m[3], "")
	if err != nil {
		return nil, err
	}
	key := m[1] + "/" + m[2] + "|nodegroup|" + req.GetRequestId()
	if req.GetRequestId() != "" {
		if op, ok := s.ops[s.requests[key]]; ok {
			return clone(op), nil
		}
	}
	if c.GetStatus().GetState() != dataprocpb.ClusterStatus_RUNNING {
		return nil, apierror.FailedPrecondition("Cluster %s is in state %s: node groups are created on a RUNNING cluster", c.ClusterName, c.GetStatus().GetState())
	}
	id := req.GetNodeGroupId()
	if id == "" {
		id = "np-" + randomSuffix(4)
	}
	ng := clone(req.GetNodeGroup())
	if err := checkNodeGroup(m[1], m[2], c.ClusterName, id, ng); err != nil {
		return nil, err
	}
	for _, g := range c.GetConfig().GetAuxiliaryNodeGroups() {
		if g.GetNodeGroupId() == id {
			return nil, apierror.AlreadyExists("Already exists: Node group %s", ng.Name)
		}
	}
	if c.GetConfig().GetAutoscalingConfig().GetPolicyUri() != "" {
		return nil, apierror.FailedPrecondition("Cluster %s is autoscaled: clusters with node groups cannot be autoscaled", c.ClusterName)
	}
	c.Config.AuxiliaryNodeGroups = append(c.Config.AuxiliaryNodeGroups, &dataprocpb.AuxiliaryNodeGroup{NodeGroup: ng, NodeGroupId: id})
	op := s.newNodeGroupOpLocked(m[1], m[2], c, id, dataprocpb.NodeGroupOperationMetadata_CREATE,
		fmt.Sprintf("Add node group %s with %d nodes", id, ng.GetNodeGroupConfig().GetNumInstances()), ng.GetLabels())
	if req.GetRequestId() != "" {
		s.requests[key] = op.Name
	}
	s.save()
	s.wg.Add(1)
	go s.applyNodeGroup(clusterKey(m[1], m[2], m[3]), id, op.Name)
	return clone(op), nil
}

func (s *Server) ResizeNodeGroup(_ context.Context, req *dataprocpb.ResizeNodeGroupRequest) (*longrunningpb.Operation, error) {
	if req.GetSize() < 0 {
		return nil, apierror.InvalidArgument("size must not be negative")
	}
	if r := req.GetRequestId(); r != "" && !requestIDRE.MatchString(r) {
		return nil, apierror.InvalidArgument("request_id %q must be at most 40 letters, digits, underscores and hyphens", r)
	}
	if d := req.GetGracefulDecommissionTimeout(); d != nil && (d.AsDuration() < 0 || d.AsDuration() > 24*time.Hour) {
		return nil, apierror.InvalidArgument("graceful_decommission_timeout must be between 0 and 1 day")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, g, err := s.nodeGroupLocked(req.GetName())
	if err != nil {
		return nil, err
	}
	m := nodeGroupRE.FindStringSubmatch(req.GetName())
	key := m[1] + "/" + m[2] + "|nodegroup-resize|" + req.GetRequestId()
	if req.GetRequestId() != "" {
		if op, ok := s.ops[s.requests[key]]; ok {
			return clone(op), nil
		}
	}
	if c.GetStatus().GetState() != dataprocpb.ClusterStatus_RUNNING {
		return nil, apierror.FailedPrecondition("Cluster %s is in state %s: node groups are resized on a RUNNING cluster", c.ClusterName, c.GetStatus().GetState())
	}
	cfg := g.GetNodeGroup().GetNodeGroupConfig()
	from := cfg.GetNumInstances()
	resizeInstances(c.ClusterName, cfg, int(req.GetSize()))
	op := s.newNodeGroupOpLocked(m[1], m[2], c, g.GetNodeGroupId(), dataprocpb.NodeGroupOperationMetadata_RESIZE,
		fmt.Sprintf("Resize node group %s from %d to %d nodes", g.GetNodeGroupId(), from, req.GetSize()), g.GetNodeGroup().GetLabels())
	if req.GetRequestId() != "" {
		s.requests[key] = op.Name
	}
	s.save()
	s.wg.Add(1)
	go s.applyNodeGroup(clusterKey(m[1], m[2], m[3]), g.GetNodeGroupId(), op.Name)
	return clone(op), nil
}
