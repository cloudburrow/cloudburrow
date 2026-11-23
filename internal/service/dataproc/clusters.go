package dataproc

// Dataproc clusters (#2162): projects.regions.clusters, as the API
// reference documents them
// (https://docs.cloud.google.com/dataproc/docs/reference/rest/v1/projects.regions.clusters,
// and the field comments of google/cloud/dataproc/v1/clusters.proto). A
// cluster is a Spark standalone cluster in local containers
// (sparkproc.Cluster): its master node runs the standalone Master and the
// Spark History Server, each worker a Worker. Create, get, list, update
// (labels, resizing the primary and secondary workers, the autoscaling
// policy and the scheduled deletion and stop), stop, start, delete and
// diagnose, each a long-running operation with ClusterOperationMetadata.

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	dataprocpb "cloud.google.com/go/dataproc/v2/apiv1/dataprocpb"
	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/aipfilter"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/sparkproc"
)

// Nodes are a running cluster's containers (*sparkproc.Cluster).
type Nodes interface {
	Scale(ctx context.Context, workers int) error
	Workers() int
	Stop(ctx context.Context) error
	Start(ctx context.Context) error
	Delete()
	Submit(ctx context.Context, job sparkproc.Job) (sparkproc.Result, error)
	Web(ctx context.Context) map[string]string
	HistoryDir() string
	Diagnose(ctx context.Context, summary string) ([]byte, error)
	// SetNodeGroup makes a node group's nodes the instances named.
	SetNodeGroup(ctx context.Context, id string, instances []string) error
}

// ClusterEngine starts clusters: an Engine that runs them.
type ClusterEngine interface {
	StartCluster(ctx context.Context, spec sparkproc.ClusterSpec) (Nodes, error)
}

func (d Docker) StartCluster(ctx context.Context, spec sparkproc.ClusterSpec) (Nodes, error) {
	c, err := d.D.StartCluster(ctx, spec)
	if err != nil {
		return nil, err
	}
	return c, nil
}

var (
	regionRE  = regexp.MustCompile(`^projects/([^/]+)/(regions|locations)/([^/]+)$`)
	clusterRE = regexp.MustCompile(`^projects/([^/]+)/regions/([^/]+)/clusters/([^/]+)$`)
	// clusterNameRE: "The name must start with a lowercase letter, and
	// can contain up to 51 lowercase letters, numbers, and hyphens. It
	// cannot end with a hyphen."
	clusterNameRE = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,49}[a-z0-9])?$`)
)

// ClusterImageVersions are the served image versions, as the image
// version list has them (re-read 2026-11-22:
// https://docs.cloud.google.com/dataproc/docs/concepts/versioning/dataproc-version-clusters):
// 2.1 runs Spark 3.3, 2.2 and 2.3 Spark 3.5, 3.0 Spark 4.1. Each one's
// operating systems are its image's suffixes. 1.x and 2.0 are past their
// end of support.
var ClusterImageVersions = map[string][]string{
	"2.1": {"debian11", "ubuntu20", "rocky8"},
	"2.2": {"debian12", "ubuntu22", "rocky9"},
	"2.3": {"debian12", "ubuntu22", "rocky9"},
	"3.0": {"debian13", "ubuntu24", "rocky9"},
}

// DefaultImageVersion is a new cluster's image version when it names
// none: "the latest generally available Debian-based" image (the image
// version list), 3.0-debian13.
const DefaultImageVersion = "3.0-debian13"

// imageVersion is the cluster's MAJOR.MINOR and its full image version
// (MAJOR.MINOR[.SUB]-OS), or an INVALID_ARGUMENT.
func imageVersion(v string) (mm, full string, err error) {
	if v == "" {
		v = DefaultImageVersion
	}
	ver, osName, _ := strings.Cut(v, "-")
	parts := strings.Split(ver, ".")
	if len(parts) < 2 || len(parts) > 3 {
		return "", "", apierror.InvalidArgument("Invalid image version %q", v)
	}
	mm = parts[0] + "." + parts[1]
	oses, ok := ClusterImageVersions[mm]
	if !ok {
		served := make([]string, 0, len(ClusterImageVersions))
		for k := range ClusterImageVersions {
			served = append(served, k)
		}
		sort.Strings(served)
		return "", "", apierror.InvalidArgument("Image version %q is not supported. Supported image versions are: %s", v, strings.Join(served, ", "))
	}
	if osName == "" {
		osName = oses[0]
	}
	found := false
	for _, o := range oses {
		found = found || o == osName
	}
	if !found {
		return "", "", apierror.InvalidArgument("Image version %q: image %s is offered on %s", v, mm, strings.Join(oses, ", "))
	}
	return mm, ver + "-" + osName, nil
}

func clusterKey(project, region, name string) string {
	return "projects/" + project + "/regions/" + region + "/clusters/" + name
}

func checkRegion(project, region string) error {
	if project == "" {
		return apierror.InvalidArgument("project_id is required")
	}
	if region == "" {
		return apierror.InvalidArgument("region is required")
	}
	if !validLocation(region) {
		return apierror.InvalidArgument("Region %q is not a Dataproc region", region)
	}
	return nil
}

func clusterActive(st dataprocpb.ClusterStatus_State) bool {
	return st == dataprocpb.ClusterStatus_CREATING || st == dataprocpb.ClusterStatus_UPDATING || st == dataprocpb.ClusterStatus_RUNNING
}

// sparkProps are the spark: properties of the cluster's softwareConfig,
// their prefix dropped: every job's Spark defaults.
func sparkProps(c *dataprocpb.Cluster) map[string]string {
	out := map[string]string{}
	for k, v := range c.GetConfig().GetSoftwareConfig().GetProperties() {
		if rest, ok := strings.CutPrefix(k, "spark:"); ok {
			out[rest] = v
		}
	}
	return out
}

func singleNode(c *dataprocpb.Cluster) bool {
	return c.GetConfig().GetSoftwareConfig().GetProperties()["dataproc:dataproc.allow.zero.workers"] == "true"
}

// workerCount is the cluster's primary and secondary workers.
func workerCount(c *dataprocpb.Cluster) int {
	return int(c.GetConfig().GetWorkerConfig().GetNumInstances() + c.GetConfig().GetSecondaryWorkerConfig().GetNumInstances())
}

// instanceNames are a group's instances' names, as Dataproc derives them
// "from cluster_name, num_instances, and the instance group".
func instanceNames(cluster, group string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("%s-%s-%d", cluster, group, i)
	}
	if group == "m" && n == 1 {
		out[0] = cluster + "-m"
	}
	return out
}

// checkCluster checks a cluster to create and fills in what Dataproc
// sets by default.
func (s *Server) checkCluster(project, region string, c *dataprocpb.Cluster) error {
	if c.GetProjectId() != "" && c.GetProjectId() != project {
		return apierror.InvalidArgument("cluster.project_id %q must match the request's project %q", c.GetProjectId(), project)
	}
	if !clusterNameRE.MatchString(c.GetClusterName()) {
		return apierror.InvalidArgument("Cluster name %q must start with a lowercase letter, contain up to 51 lowercase letters, numbers and hyphens, and not end with a hyphen", c.GetClusterName())
	}
	if c.GetVirtualClusterConfig() != nil {
		if c.GetConfig() != nil {
			return apierror.InvalidArgument("Exactly one of config and virtual_cluster_config must be specified")
		}
		return status.Error(codes.Unimplemented, "Dataproc on GKE virtual clusters (virtual_cluster_config) are not served by CloudBurrow: create a cluster of Compute Engine instances (config)")
	}
	if len(c.GetLabels()) > maxLabels {
		return apierror.InvalidArgument("No more than %d labels can be associated with a cluster", maxLabels)
	}
	if c.Config == nil {
		c.Config = &dataprocpb.ClusterConfig{}
	}
	cfg := c.Config
	if cfg.SoftwareConfig == nil {
		cfg.SoftwareConfig = &dataprocpb.SoftwareConfig{}
	}
	mm, full, err := imageVersion(cfg.SoftwareConfig.GetImageVersion())
	if err != nil {
		return err
	}
	cfg.SoftwareConfig.ImageVersion = full
	if err := checkComponents(mm, cfg.SoftwareConfig.GetOptionalComponents()); err != nil {
		return err
	}
	// "you can set the dataproc:dataproc.iceberg.version cluster property
	// to 1.10 or 1.6" (the 2.2 and 2.3 release notes).
	if v, ok := cfg.SoftwareConfig.GetProperties()["dataproc:dataproc.iceberg.version"]; ok && v != "1.6" && v != "1.10" {
		return apierror.InvalidArgument("Invalid value %q for property dataproc:dataproc.iceberg.version: the supported Iceberg versions are 1.6 and 1.10", v)
	}
	if err := checkAuxiliaryGroups(project, region, c); err != nil {
		return err
	}
	if cfg.MasterConfig == nil {
		cfg.MasterConfig = &dataprocpb.InstanceGroupConfig{}
	}
	switch n := cfg.MasterConfig.GetNumInstances(); n {
	case 0:
		cfg.MasterConfig.NumInstances = 1
	case 1, 3:
	default:
		return apierror.InvalidArgument("master_config.num_instances must be 1 (a standard cluster) or 3 (a high availability cluster), not %d", n)
	}
	if singleNode(c) {
		if cfg.GetWorkerConfig().GetNumInstances() > 0 || cfg.GetSecondaryWorkerConfig().GetNumInstances() > 0 {
			return apierror.InvalidArgument("A single node cluster (dataproc:dataproc.allow.zero.workers=true) cannot have workers")
		}
		if cfg.MasterConfig.GetNumInstances() == 3 {
			return apierror.InvalidArgument("A single node cluster cannot be a high availability cluster")
		}
		cfg.WorkerConfig = nil
	} else {
		if cfg.WorkerConfig == nil {
			cfg.WorkerConfig = &dataprocpb.InstanceGroupConfig{}
		}
		if cfg.WorkerConfig.NumInstances == 0 {
			cfg.WorkerConfig.NumInstances = 2
		}
		if cfg.WorkerConfig.NumInstances < 2 {
			return apierror.InvalidArgument("Multi node clusters must have at least 2 primary workers; a single node cluster sets dataproc:dataproc.allow.zero.workers=true")
		}
		cfg.WorkerConfig.InstanceNames = instanceNames(c.ClusterName, "w", int(cfg.WorkerConfig.NumInstances))
		if cfg.WorkerConfig.MachineTypeUri == "" {
			cfg.WorkerConfig.MachineTypeUri = defaultMachine(project, region)
		}
		cfg.WorkerConfig.Preemptibility = dataprocpb.InstanceGroupConfig_NON_PREEMPTIBLE
	}
	if sw := cfg.GetSecondaryWorkerConfig(); sw != nil {
		if sw.Preemptibility == dataprocpb.InstanceGroupConfig_PREEMPTIBILITY_UNSPECIFIED {
			sw.Preemptibility = dataprocpb.InstanceGroupConfig_PREEMPTIBLE
		}
		sw.IsPreemptible = sw.Preemptibility != dataprocpb.InstanceGroupConfig_NON_PREEMPTIBLE
		sw.InstanceNames = instanceNames(c.ClusterName, "sw", int(sw.NumInstances))
	}
	cfg.MasterConfig.InstanceNames = instanceNames(c.ClusterName, "m", int(cfg.MasterConfig.NumInstances))
	if cfg.MasterConfig.MachineTypeUri == "" {
		cfg.MasterConfig.MachineTypeUri = defaultMachine(project, region)
	}
	if p := cfg.GetAutoscalingConfig().GetPolicyUri(); p != "" {
		if _, err := s.policyLocked(policyKeyOf(p)); err != nil {
			return apierror.InvalidArgument("Autoscaling policy %s cannot be used: %v", p, err)
		}
	}
	if lc := cfg.GetLifecycleConfig(); lc != nil {
		for _, d := range []struct {
			field string
			v     time.Duration
			set   bool
		}{{"idle_delete_ttl", lc.GetIdleDeleteTtl().AsDuration(), lc.GetIdleDeleteTtl() != nil},
			{"auto_delete_ttl", lc.GetAutoDeleteTtl().AsDuration(), lc.GetAutoDeleteTtl() != nil},
			{"idle_stop_ttl", lc.GetIdleStopTtl().AsDuration(), lc.GetIdleStopTtl() != nil},
			{"auto_stop_ttl", lc.GetAutoStopTtl().AsDuration(), lc.GetAutoStopTtl() != nil}} {
			// "Minimum value is 10 minutes; maximum value is 14 days."
			if d.set && (d.v < 10*time.Minute || d.v > 14*24*time.Hour) {
				return apierror.InvalidArgument("config.lifecycle_config.%s must be between 10 minutes and 14 days", d.field)
			}
		}
	}
	if cfg.GceClusterConfig == nil {
		cfg.GceClusterConfig = &dataprocpb.GceClusterConfig{}
	}
	if cfg.GceClusterConfig.ZoneUri == "" {
		cfg.GceClusterConfig.ZoneUri = "https://www.googleapis.com/compute/v1/projects/" + project + "/zones/" + region + "-a"
	}
	if cfg.GceClusterConfig.NetworkUri == "" && cfg.GceClusterConfig.SubnetworkUri == "" {
		cfg.GceClusterConfig.NetworkUri = "https://www.googleapis.com/compute/v1/projects/" + project + "/global/networks/default"
	}
	return nil
}

// componentImages are the image versions each optional component is
// offered on, as the components overview lists them (read 2026-11-22:
// https://docs.cloud.google.com/dataproc/docs/concepts/components/overview):
// the first image version of each, HBase and Presto not on 2.1 and later.
var componentImages = map[dataprocpb.Component]func(mm string) bool{
	dataprocpb.Component_DELTA:        func(mm string) bool { return mm >= "2.2" },
	dataprocpb.Component_ICEBERG:      func(mm string) bool { return mm >= "2.2" },
	dataprocpb.Component_HUDI:         func(string) bool { return true },
	dataprocpb.Component_DOCKER:       func(string) bool { return true },
	dataprocpb.Component_FLINK:        func(string) bool { return true },
	dataprocpb.Component_HIVE_WEBHCAT: func(string) bool { return true },
	dataprocpb.Component_JUPYTER:      func(string) bool { return true },
	dataprocpb.Component_PIG:          func(string) bool { return true },
	dataprocpb.Component_RANGER:       func(string) bool { return true },
	dataprocpb.Component_SOLR:         func(string) bool { return true },
	dataprocpb.Component_TRINO:        func(mm string) bool { return mm >= "2.1" },
	dataprocpb.Component_ZEPPELIN:     func(string) bool { return true },
	dataprocpb.Component_ZOOKEEPER:    func(string) bool { return true },
	dataprocpb.Component_HBASE:        func(mm string) bool { return mm < "2.1" },
	dataprocpb.Component_PRESTO:       func(mm string) bool { return mm < "2.1" },
	dataprocpb.Component_ANACONDA:     func(mm string) bool { return mm < "2.0" },
}

// servedComponents run as containers on a local cluster
// (internal/sparkproc's Ecosystem).
var servedComponents = map[dataprocpb.Component]bool{
	dataprocpb.Component_FLINK: true, dataprocpb.Component_HIVE_WEBHCAT: true, dataprocpb.Component_JUPYTER: true,
	dataprocpb.Component_PIG: true, dataprocpb.Component_SOLR: true, dataprocpb.Component_TRINO: true,
	dataprocpb.Component_ZOOKEEPER: true, dataprocpb.Component_DELTA: true, dataprocpb.Component_ICEBERG: true,
	dataprocpb.Component_HUDI: true, dataprocpb.Component_ZEPPELIN: true,
}

// unservedComponents are why the other components do not run.
var unservedComponents = map[dataprocpb.Component]string{
	dataprocpb.Component_DOCKER: "a Docker daemon on a node's container needs a privileged container: the official docker:dind and docker:dind-rootless images both " +
		"need --privileged, and a non-privileged runtime for system containers (Sysbox's sysbox-runc) is not one of the machine's Docker runtimes",
	dataprocpb.Component_RANGER: "Apache Ranger publishes no official image",
}

// zeppelin30 is why ZEPPELIN is not served on image 3.0, whose Zeppelin
// is 0.12.0: its official image is linux/amd64 only and 3.3 GB
// compressed, a second one beside 0.10.1's (the machine's disk budget).
const zeppelin30 = "image 3.0's Zeppelin 0.12.0 is not pinned: its official image is linux/amd64 only and 3.3 GB compressed, beside the 3.7 GB of 2.x's Zeppelin 0.10.1"

// icebergREST is why ICEBERG is not served on image 3.0: its Spark's
// default_iceberg catalog is the Hive Metastore's Iceberg REST catalog
// (the Iceberg component page, read 2026-11-23), which the Hive 4.0.1 of
// the pinned apache/hive image does not serve.
const icebergREST = "image 3.0's default_iceberg catalog is the Hive Metastore's Iceberg REST catalog, which the local cluster's Hive 4.0.1 metastore does not serve"

// checkComponents checks a cluster's optional components against its
// image version.
func checkComponents(mm string, comps []dataprocpb.Component) error {
	seen := map[dataprocpb.Component]bool{}
	for _, comp := range comps {
		if comp == dataprocpb.Component_COMPONENT_UNSPECIFIED {
			return apierror.InvalidArgument("optional_components: COMPONENT_UNSPECIFIED is not a component")
		}
		if seen[comp] {
			return apierror.InvalidArgument("optional_components: %s is listed more than once", comp)
		}
		seen[comp] = true
		offered, ok := componentImages[comp]
		if !ok || !offered(mm) {
			return apierror.InvalidArgument("Optional component %s is not supported on image version %s", comp, mm)
		}
	}
	for _, comp := range comps {
		if comp == dataprocpb.Component_ZEPPELIN && mm >= "3.0" {
			return status.Errorf(codes.Unimplemented, "The ZEPPELIN optional component is not served by CloudBurrow on image version %s (%s)", mm, zeppelin30)
		}
		if comp == dataprocpb.Component_ICEBERG && mm >= "3.0" {
			return status.Errorf(codes.Unimplemented, "The ICEBERG optional component is not served by CloudBurrow on image version %s (%s)", mm, icebergREST)
		}
		if !servedComponents[comp] {
			return status.Errorf(codes.Unimplemented, "The %s optional component is not served by CloudBurrow (%s): a local cluster runs Delta Lake, Flink, Hive WebHCat, Hudi, Iceberg, Jupyter, Pig, Solr, Trino, Zeppelin and ZooKeeper", comp, unservedComponents[comp])
		}
	}
	return nil
}

// imageMMOf is a cluster's image version's MAJOR.MINOR.
func imageMMOf(c *dataprocpb.Cluster) string {
	mm, _, _ := imageVersion(c.GetConfig().GetSoftwareConfig().GetImageVersion())
	return mm
}

// hasComponent is whether the cluster enables the optional component
// name (a Component's enum name).
func hasComponent(c *dataprocpb.Cluster, name string) bool {
	for _, comp := range c.GetConfig().GetSoftwareConfig().GetOptionalComponents() {
		if comp.String() == name {
			return true
		}
	}
	return false
}

func defaultMachine(project, region string) string {
	return "https://www.googleapis.com/compute/v1/projects/" + project + "/zones/" + region + "-a/machineTypes/n2-standard-4"
}

// TempBucket is a project's temp bucket in a region, when a cluster names
// none: prefixed dataproc-temp- (the staging and temp buckets page). The
// rest of its name, like the staging bucket's, is
// UNVERIFIED (docs re-read 2026-11-22: https://docs.cloud.google.com/dataproc/docs/concepts/configuring-clusters/staging-bucket).
func (s *Server) TempBucket(project, location string) string {
	return strings.Replace(s.StagingBucket(project, location), "dataproc-staging-", "dataproc-temp-", 1)
}

func (s *Server) newClusterOpLocked(project, region string, c *dataprocpb.Cluster, kind, desc string) *longrunningpb.Operation {
	now := timestamppb.New(s.now())
	md := &dataprocpb.ClusterOperationMetadata{ClusterName: c.GetClusterName(), ClusterUuid: c.GetClusterUuid(), OperationType: kind,
		Description: desc, Labels: c.GetLabels(),
		Status: &dataprocpb.ClusterOperationStatus{State: dataprocpb.ClusterOperationStatus_PENDING, InnerState: "PENDING", StateStartTime: now}}
	return s.newOpLocked(opName(project, region), md)
}

// setClusterStateLocked moves c to st, its status into its history.
func (s *Server) setClusterStateLocked(c *dataprocpb.Cluster, st dataprocpb.ClusterStatus_State, detail string) {
	if c.Status != nil {
		c.StatusHistory = append(c.StatusHistory, c.Status)
	}
	c.Status = &dataprocpb.ClusterStatus{State: st, Detail: detail, StateStartTime: timestamppb.New(s.now())}
}

// runOpLocked marks the cluster operation name RUNNING.
func (s *Server) runOpLocked(name string) {
	op := s.ops[name]
	if op == nil {
		return
	}
	var md dataprocpb.ClusterOperationMetadata
	if op.GetMetadata().UnmarshalTo(&md) != nil {
		return
	}
	md.StatusHistory = append(md.StatusHistory, md.Status)
	md.Status = &dataprocpb.ClusterOperationStatus{State: dataprocpb.ClusterOperationStatus_RUNNING, InnerState: "RUNNING", StateStartTime: timestamppb.New(s.now())}
	op.Metadata, _ = anyOf(&md)
}

func (s *Server) CreateCluster(ctx context.Context, req *dataprocpb.CreateClusterRequest) (*longrunningpb.Operation, error) {
	if err := checkRegion(req.GetProjectId(), req.GetRegion()); err != nil {
		return nil, err
	}
	if req.GetCluster() == nil {
		return nil, apierror.InvalidArgument("cluster is required")
	}
	if r := req.GetRequestId(); r != "" && !requestIDRE.MatchString(r) {
		return nil, apierror.InvalidArgument("request_id %q must be at most 40 letters, digits, underscores and hyphens", r)
	}
	c := clone(req.GetCluster())
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, status.Error(codes.Unavailable, "the instance is stopping")
	}
	key := req.GetProjectId() + "/" + req.GetRegion() + "|cluster|" + req.GetRequestId()
	if req.GetRequestId() != "" {
		if op, ok := s.ops[s.requests[key]]; ok {
			return clone(op), nil
		}
	}
	if err := s.checkCluster(req.GetProjectId(), req.GetRegion(), c); err != nil {
		return nil, err
	}
	name := clusterKey(req.GetProjectId(), req.GetRegion(), c.ClusterName)
	if _, ok := s.clusters[name]; ok {
		return nil, apierror.AlreadyExists("Already exists: Failed to create cluster: Cluster projects/%s/regions/%s/clusters/%s", req.GetProjectId(), req.GetRegion(), c.ClusterName)
	}
	return s.createClusterLocked(ctx, req.GetProjectId(), req.GetRegion(), c, key, req.GetRequestId())
}

// createClusterLocked records a checked cluster and starts it.
func (s *Server) createClusterLocked(_ context.Context, project, region string, c *dataprocpb.Cluster, key, requestID string) (*longrunningpb.Operation, error) {
	name := clusterKey(project, region, c.ClusterName)
	c.ProjectId, c.ClusterUuid, c.StatusHistory = project, newUUID(), nil
	cfg := c.Config
	if cfg.ConfigBucket == "" {
		cfg.ConfigBucket = s.StagingBucket(project, region)
	}
	if cfg.TempBucket == "" {
		cfg.TempBucket = s.TempBucket(project, region)
	}
	if c.Labels == nil {
		c.Labels = map[string]string{}
	}
	// Dataproc's own labels on every cluster.
	c.Labels["goog-dataproc-cluster-name"], c.Labels["goog-dataproc-cluster-uuid"], c.Labels["goog-dataproc-location"] = c.ClusterName, c.ClusterUuid, region
	c.Status = nil
	s.setClusterStateLocked(c, dataprocpb.ClusterStatus_CREATING, "")
	workers := workerCount(c)
	desc := fmt.Sprintf("Create cluster with %d workers", workers)
	if singleNode(c) {
		desc = "Create single node cluster"
	}
	op := s.newClusterOpLocked(project, region, c, "CREATE", desc)
	s.clusters[name] = c
	if requestID != "" {
		s.requests[key] = op.Name
	}
	s.save()
	s.wg.Add(1)
	go s.startCluster(name, op.Name, false)
	return clone(op), nil
}

// clusterSpec is the engine's spec of the cluster.
func clusterSpec(c *dataprocpb.Cluster) sparkproc.ClusterSpec {
	mm, _, _ := imageVersion(c.GetConfig().GetSoftwareConfig().GetImageVersion())
	m := clusterRE.FindStringSubmatch(clusterKey(c.GetProjectId(), regionOfCluster(c), c.GetClusterName()))
	project, region := "", ""
	if m != nil {
		project, region = m[1], m[2]
	}
	ig := c.GetConfig().GetWorkerConfig()
	if workerCount(c) == 0 {
		ig = c.GetConfig().GetMasterConfig()
	}
	cores, mem := machineShape(ig.GetMachineTypeUri())
	var comps []string
	for _, comp := range c.GetConfig().GetSoftwareConfig().GetOptionalComponents() {
		comps = append(comps, comp.String())
	}
	conf := map[string]string{}
	for k, v := range c.GetConfig().GetSoftwareConfig().GetProperties() {
		if p, _, ok := strings.Cut(k, ":"); ok && p != "spark" && p != "dataproc" {
			conf[k] = v
		}
	}
	return sparkproc.ClusterSpec{ID: c.GetClusterUuid(), Name: c.GetClusterName(), Project: project, Location: region,
		ImageVersion: mm, Workers: workerCount(c), WorkerCores: cores, WorkerMemory: mem,
		Properties: sparkProps(c), HistoryBase: gatewayBase(c.GetClusterUuid()) + "sparkhistory",
		GatewayBase: gatewayBase(c.GetClusterUuid()), Components: comps, ConfProperties: conf,
		IcebergVersion: c.GetConfig().GetSoftwareConfig().GetProperties()["dataproc:dataproc.iceberg.version"]}
}

var machineRE = regexp.MustCompile(`(?:^|/)[a-z0-9]+-(standard|highmem|highcpu)-(\d+)$`)

// machineShape is a worker's Spark cores and memory from its machine
// type: its vCPUs, and 4 GB of memory a vCPU for a standard type (8 for
// highmem, 1 for highcpu), as Compute Engine's general-purpose families
// have them; a type of another shape (e2-medium, a custom type) gets
// Spark's 2 cores and 2g.
func machineShape(uri string) (int, string) {
	m := machineRE.FindStringSubmatch(uri)
	if m == nil {
		return 0, ""
	}
	n, err := strconv.Atoi(m[2])
	if err != nil || n <= 0 {
		return 0, ""
	}
	per := map[string]int{"standard": 4, "highmem": 8, "highcpu": 1}[m[1]]
	return n, strconv.Itoa(n*per) + "g"
}

func regionOfCluster(c *dataprocpb.Cluster) string { return c.GetLabels()["goog-dataproc-location"] }

// startCluster starts the cluster's nodes; the operation opName ends when
// they answer (RUNNING) or fail (ERROR). restart starts a cluster the
// CLI's last run left running.
func (s *Server) startCluster(name, opName string, restart bool) {
	defer s.wg.Done()
	s.mu.Lock()
	c := s.clusters[name]
	if c == nil {
		s.mu.Unlock()
		return
	}
	spec := clusterSpec(c)
	s.runOpLocked(opName)
	s.mu.Unlock()
	var nodes Nodes
	var err error
	ce, ok := s.opts.Engine.(ClusterEngine)
	if !ok || s.opts.Engine == nil {
		err = &sparkproc.Unavailable{Msg: "this instance runs no Apache Spark"}
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
		nodes, err = ce.StartCluster(ctx, spec)
		cancel()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c = s.clusters[name]
	if c == nil || c.GetStatus().GetState() == dataprocpb.ClusterStatus_DELETING {
		if nodes != nil {
			go nodes.Delete()
		}
		return
	}
	if err != nil {
		msg := err.Error()
		var u *sparkproc.Unavailable
		if errors.As(err, &u) {
			msg = "Apache Spark is not available on this instance: " + u.Msg
		}
		s.setClusterStateLocked(c, dataprocpb.ClusterStatus_ERROR, msg)
		s.finishOpLocked(opName, nil, status.Error(codes.Unknown, "Cluster "+c.ClusterName+" failed to start: "+msg))
		s.save()
		return
	}
	for _, g := range c.GetConfig().GetAuxiliaryNodeGroups() {
		gctx, gcancel := context.WithTimeout(context.Background(), 10*time.Minute)
		err := nodes.SetNodeGroup(gctx, g.GetNodeGroupId(), g.GetNodeGroup().GetNodeGroupConfig().GetInstanceNames())
		gcancel()
		if err != nil {
			go nodes.Delete()
			s.setClusterStateLocked(c, dataprocpb.ClusterStatus_ERROR, err.Error())
			s.finishOpLocked(opName, nil, status.Error(codes.Unknown, "Cluster "+c.ClusterName+" failed to start: "+err.Error()))
			s.save()
			return
		}
	}
	s.nodes[name] = nodes
	s.fillEndpointsLocked(c)
	s.setClusterStateLocked(c, dataprocpb.ClusterStatus_RUNNING, "")
	s.markIdleLocked(c)
	if opName != "" {
		s.finishOpLocked(opName, clone(c), nil)
	}
	s.save()
	s.watchCluster(name, c.ClusterUuid)
	if restart {
		s.logf("dataproc: cluster %s started again", name)
	}
}

// gatewayBase is the path the cluster's web interfaces are served at
// through the API's port (Component Gateway's base URL, its scheme and
// authority this API's, as the caller reached it).
func gatewayBase(uuid string) string { return "/component-gateway/" + uuid + "/" }

// gatewayPaths are the components' paths under the gateway base, and
// gatewayKeys the Nodes.Web key each path is served from.
var (
	gatewayPaths = map[string]string{"Jupyter": "jupyter/tree", "JupyterLab": "jupyter/lab", "Zeppelin": "zeppelin/",
		"Flink": "flink/", "Trino": "trino/ui/", "Solr": "solr/", "WebHCat": "webhcat/templeton/v1/status"}
	gatewayKeys = map[string]string{"sparkhistory": "history", "jupyter": "jupyter", "zeppelin": "zeppelin", "flink": "flink",
		"trino": "trino", "solr": "solr", "webhcat": "webhcat", "yarn": "yarn", "jobhistory": "jobhistory", "apphistory": "apphistory"}
	// gatewayRewritten are the Hadoop web interfaces, which have no base
	// path setting: their pages' root-relative links and their own
	// absolute URLs (http://CLUSTER-m:8088/...) are rewritten to their
	// gateway paths.
	gatewayRewritten = map[string]string{"yarn": "8088", "jobhistory": "19888", "apphistory": "8188"}
	// gatewayFull are the components served at their full path (their
	// base URL configured to it), the rest with it stripped.
	gatewayFull = map[string]bool{"jupyter": true, "zeppelin": true}
)

// fillEndpointsLocked sets the Component Gateway's endpoints, for a
// cluster that enables HTTP port access: "Spark History Server" (the
// key the Component Gateway page lists in endpointConfig.httpPorts, re-read 2026-11-22:
// https://docs.cloud.google.com/dataproc/docs/concepts/accessing/dataproc-gateways).
func (s *Server) fillEndpointsLocked(c *dataprocpb.Cluster) {
	ec := c.GetConfig().GetEndpointConfig()
	if ec == nil || !ec.GetEnableHttpPortAccess() {
		return
	}
	base := gatewayBase(c.GetClusterUuid())
	// The Hadoop interfaces every cluster runs (YARN, internal/sparkproc).
	ec.HttpPorts = map[string]string{"Spark History Server": base + "sparkhistory/", "YARN ResourceManager": base + "yarn/",
		"MapReduce Job History": base + "jobhistory/", "YARN Application Timeline": base + "apphistory/"}
	// The components' web interfaces. Their keys are UNVERIFIED (docs
	// re-read 2026-11-22: the Component Gateway page lists only the
	// Hadoop, Spark and Tez interfaces' keys).
	for key, comp := range map[string]string{"Jupyter": "JUPYTER", "JupyterLab": "JUPYTER", "Zeppelin": "ZEPPELIN",
		"Flink": "FLINK", "Trino": "TRINO", "Solr": "SOLR", "WebHCat": "HIVE_WEBHCAT"} {
		if hasComponent(c, comp) {
			ec.HttpPorts[key] = base + gatewayPaths[key]
		}
	}
}

// renderCluster is c as a caller at ctx's authority reads it: its
// Component Gateway URLs made absolute.
func renderCluster(ctx context.Context, c *dataprocpb.Cluster) *dataprocpb.Cluster {
	out := clone(c)
	if host := authority(ctx); host != "" {
		for k, v := range out.GetConfig().GetEndpointConfig().GetHttpPorts() {
			if strings.HasPrefix(v, "/") {
				out.Config.EndpointConfig.HttpPorts[k] = "http://" + host + v
			}
		}
	}
	return out
}

func (s *Server) clusterLocked(project, region, name, uuid string) (*dataprocpb.Cluster, error) {
	if err := checkRegion(project, region); err != nil {
		return nil, err
	}
	c, ok := s.clusters[clusterKey(project, region, name)]
	if !ok || (uuid != "" && c.ClusterUuid != uuid) {
		return nil, apierror.NotFound("Not found: Cluster projects/%s/regions/%s/clusters/%s", project, region, name)
	}
	return c, nil
}

func (s *Server) GetCluster(ctx context.Context, req *dataprocpb.GetClusterRequest) (*dataprocpb.Cluster, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.clusterLocked(req.GetProjectId(), req.GetRegion(), req.GetClusterName(), "")
	if err != nil {
		return nil, err
	}
	return renderCluster(ctx, c), nil
}

// clusterFilter matches ListClusters' filter: "field = value [AND [field
// = value]] ...", field status.state, clusterName or labels.[KEY], value
// * for any; ACTIVE is CREATING, UPDATING and RUNNING, INACTIVE DELETING,
// ERROR, STOPPING and STOPPED.
func clusterFilter(filter string) (func(*dataprocpb.Cluster) bool, error) {
	terms, err := andTerms(filter)
	if err != nil {
		return nil, err
	}
	return func(c *dataprocpb.Cluster) bool {
		for _, t := range terms {
			switch {
			case t.field == "status.state":
				st := c.GetStatus().GetState()
				switch strings.ToUpper(t.value) {
				case "*":
				case "ACTIVE":
					if !clusterActive(st) {
						return false
					}
				case "INACTIVE":
					if clusterActive(st) {
						return false
					}
				default:
					if st.String() != strings.ToUpper(t.value) {
						return false
					}
				}
			case t.field == "clusterName" || t.field == "cluster_name":
				if t.value != "*" && c.GetClusterName() != t.value {
					return false
				}
			case strings.HasPrefix(t.field, "labels."):
				v, ok := c.GetLabels()[strings.TrimPrefix(t.field, "labels.")]
				if !ok || (t.value != "*" && v != t.value) {
					return false
				}
			}
		}
		return true
	}, checkTerms(terms, "status.state", "clusterName", "cluster_name")
}

type term struct{ field, op, value string }

var termRE = regexp.MustCompile(`^\s*([A-Za-z_.\-\[\]0-9]+)\s*(<=|>=|!=|=|<|>|:)\s*("[^"]*"|\S+)`)

// andTerms splits a Dataproc list filter into its AND-ed terms.
func andTerms(filter string) ([]term, error) {
	var out []term
	rest := strings.TrimSpace(filter)
	for rest != "" {
		m := termRE.FindStringSubmatchIndex(rest)
		if m == nil {
			return nil, apierror.InvalidArgument("Invalid filter %q: expected field = value", filter)
		}
		t := term{field: rest[m[2]:m[3]], op: rest[m[4]:m[5]], value: strings.Trim(rest[m[6]:m[7]], `"`)}
		out = append(out, t)
		rest = strings.TrimSpace(rest[m[1]:])
		if r, ok := strings.CutPrefix(rest, "AND "); ok {
			rest = strings.TrimSpace(r)
		} else if strings.HasPrefix(rest, "OR ") {
			return nil, apierror.InvalidArgument("Invalid filter %q: only the logical AND operator is supported", filter)
		}
	}
	return out, nil
}

func checkTerms(terms []term, fields ...string) error {
	for _, t := range terms {
		if strings.HasPrefix(t.field, "labels.") {
			continue
		}
		ok := false
		for _, f := range fields {
			ok = ok || f == t.field
		}
		if !ok {
			return apierror.InvalidArgument("Invalid filter field %q", t.field)
		}
	}
	return nil
}

func (s *Server) ListClusters(ctx context.Context, req *dataprocpb.ListClustersRequest) (*dataprocpb.ListClustersResponse, error) {
	if err := checkRegion(req.GetProjectId(), req.GetRegion()); err != nil {
		return nil, err
	}
	match, err := clusterFilter(req.GetFilter())
	if err != nil {
		return nil, err
	}
	prefix := "projects/" + req.GetProjectId() + "/regions/" + req.GetRegion() + "/clusters/"
	s.mu.Lock()
	var all []*dataprocpb.Cluster
	// "Lists all regions/{region}/clusters in a project alphabetically."
	for _, k := range sortedKeys(s.clusters) {
		if strings.HasPrefix(k, prefix) && match(s.clusters[k]) {
			all = append(all, renderCluster(ctx, s.clusters[k]))
		}
	}
	s.mu.Unlock()
	out, next, err := aipfilter.Page("dataproc-clusters:"+prefix+req.GetFilter(), all, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, err
	}
	return &dataprocpb.ListClustersResponse{Clusters: out, NextPageToken: next}, nil
}

// updatable are UpdateCluster's masks, as the reference's table lists
// them, with the scheduled deletion and stop of the cluster's lifecycle.
var updatable = map[string]bool{"labels": true, "config.worker_config.num_instances": true,
	"config.secondary_worker_config.num_instances": true, "config.autoscaling_config.policy_uri": true,
	"config.lifecycle_config.idle_delete_ttl": true, "config.lifecycle_config.auto_delete_time": true,
	"config.lifecycle_config.auto_delete_ttl": true, "config.lifecycle_config.idle_stop_ttl": true,
	"config.lifecycle_config.auto_stop_time": true, "config.lifecycle_config.auto_stop_ttl": true}

func snake(p string) string {
	var b strings.Builder
	for _, r := range p {
		if r >= 'A' && r <= 'Z' {
			b.WriteByte('_')
			r += 'a' - 'A'
		}
		b.WriteRune(r)
	}
	return b.String()
}

func (s *Server) UpdateCluster(ctx context.Context, req *dataprocpb.UpdateClusterRequest) (*longrunningpb.Operation, error) {
	if req.GetCluster() == nil {
		return nil, apierror.InvalidArgument("cluster is required")
	}
	if len(req.GetUpdateMask().GetPaths()) == 0 {
		return nil, apierror.InvalidArgument("update_mask is required")
	}
	if d := req.GetGracefulDecommissionTimeout(); d != nil && (d.AsDuration() < 0 || d.AsDuration() > 24*time.Hour) {
		return nil, apierror.InvalidArgument("graceful_decommission_timeout must be between 0 and 1 day")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.clusterLocked(req.GetProjectId(), req.GetRegion(), req.GetClusterName(), "")
	if err != nil {
		return nil, err
	}
	key := req.GetProjectId() + "/" + req.GetRegion() + "|update|" + req.GetRequestId()
	if req.GetRequestId() != "" {
		if op, ok := s.ops[s.requests[key]]; ok {
			return clone(op), nil
		}
	}
	if c.GetStatus().GetState() != dataprocpb.ClusterStatus_RUNNING {
		return nil, apierror.FailedPrecondition("Cluster %s is in state %s: the cluster must be RUNNING to be updated", c.ClusterName, c.GetStatus().GetState())
	}
	nc := clone(c)
	in := req.GetCluster()
	if nc.Config == nil {
		nc.Config = &dataprocpb.ClusterConfig{}
	}
	resize := false
	for _, p := range req.GetUpdateMask().GetPaths() {
		path := snake(p)
		if !updatable[path] {
			return nil, apierror.InvalidArgument("Invalid update mask path %q: the fields that can be updated are %s", p, strings.Join(sortedKeys(updatable), ", "))
		}
		switch path {
		case "labels":
			labels := map[string]string{}
			for k, v := range in.GetLabels() {
				labels[k] = v
			}
			for _, k := range []string{"goog-dataproc-cluster-name", "goog-dataproc-cluster-uuid", "goog-dataproc-location"} {
				labels[k] = c.Labels[k]
			}
			nc.Labels = labels
		case "config.worker_config.num_instances":
			if singleNode(c) {
				return nil, apierror.InvalidArgument("A single node cluster cannot be resized: it has no workers")
			}
			n := in.GetConfig().GetWorkerConfig().GetNumInstances()
			if n < 2 {
				return nil, apierror.InvalidArgument("Multi node clusters must have at least 2 primary workers")
			}
			nc.Config.WorkerConfig.NumInstances = n
			nc.Config.WorkerConfig.InstanceNames = instanceNames(c.ClusterName, "w", int(n))
			resize = true
		case "config.secondary_worker_config.num_instances":
			if singleNode(c) {
				return nil, apierror.InvalidArgument("A single node cluster cannot have secondary workers")
			}
			n := in.GetConfig().GetSecondaryWorkerConfig().GetNumInstances()
			if n < 0 {
				return nil, apierror.InvalidArgument("config.secondary_worker_config.num_instances must not be negative")
			}
			if nc.Config.SecondaryWorkerConfig == nil {
				nc.Config.SecondaryWorkerConfig = &dataprocpb.InstanceGroupConfig{Preemptibility: dataprocpb.InstanceGroupConfig_PREEMPTIBLE, IsPreemptible: true}
			}
			nc.Config.SecondaryWorkerConfig.NumInstances = n
			nc.Config.SecondaryWorkerConfig.InstanceNames = instanceNames(c.ClusterName, "sw", int(n))
			resize = true
		case "config.autoscaling_config.policy_uri":
			p := in.GetConfig().GetAutoscalingConfig().GetPolicyUri()
			if p == "" {
				nc.Config.AutoscalingConfig = nil
				break
			}
			if _, err := s.policyLocked(policyKeyOf(p)); err != nil {
				return nil, apierror.InvalidArgument("Autoscaling policy %s cannot be used: %v", p, err)
			}
			nc.Config.AutoscalingConfig = &dataprocpb.AutoscalingConfig{PolicyUri: p}
		default:
			if nc.Config.LifecycleConfig == nil {
				nc.Config.LifecycleConfig = &dataprocpb.LifecycleConfig{}
			}
			lc, src := nc.Config.LifecycleConfig, in.GetConfig().GetLifecycleConfig()
			switch path {
			case "config.lifecycle_config.idle_delete_ttl":
				lc.IdleDeleteTtl = src.GetIdleDeleteTtl()
			case "config.lifecycle_config.auto_delete_time":
				lc.Ttl = nil
				if src.GetAutoDeleteTime() != nil {
					lc.Ttl = &dataprocpb.LifecycleConfig_AutoDeleteTime{AutoDeleteTime: src.GetAutoDeleteTime()}
				}
			case "config.lifecycle_config.auto_delete_ttl":
				lc.Ttl = nil
				if src.GetAutoDeleteTtl() != nil {
					lc.Ttl = &dataprocpb.LifecycleConfig_AutoDeleteTtl{AutoDeleteTtl: src.GetAutoDeleteTtl()}
				}
			case "config.lifecycle_config.idle_stop_ttl":
				lc.IdleStopTtl = src.GetIdleStopTtl()
			case "config.lifecycle_config.auto_stop_time":
				lc.StopTtl = nil
				if src.GetAutoStopTime() != nil {
					lc.StopTtl = &dataprocpb.LifecycleConfig_AutoStopTime{AutoStopTime: src.GetAutoStopTime()}
				}
			case "config.lifecycle_config.auto_stop_ttl":
				lc.StopTtl = nil
				if src.GetAutoStopTtl() != nil {
					lc.StopTtl = &dataprocpb.LifecycleConfig_AutoStopTtl{AutoStopTtl: src.GetAutoStopTtl()}
				}
			}
		}
	}
	nc.Status, nc.StatusHistory = c.Status, c.StatusHistory
	s.clusters[clusterKey(req.GetProjectId(), req.GetRegion(), c.ClusterName)] = nc
	c = nc
	s.setClusterStateLocked(c, dataprocpb.ClusterStatus_UPDATING, "")
	op := s.newClusterOpLocked(req.GetProjectId(), req.GetRegion(), c, "UPDATE", fmt.Sprintf("Update cluster %s", c.ClusterName))
	if req.GetRequestId() != "" {
		s.requests[key] = op.Name
	}
	s.save()
	name := clusterKey(req.GetProjectId(), req.GetRegion(), c.ClusterName)
	want := workerCount(c)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		var err error
		s.mu.Lock()
		s.runOpLocked(op.Name)
		nodes := s.nodes[name]
		s.mu.Unlock()
		if resize && nodes != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			err = nodes.Scale(ctx, want)
			cancel()
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		c := s.clusters[name]
		if c == nil {
			return
		}
		if err != nil {
			s.setClusterStateLocked(c, dataprocpb.ClusterStatus_ERROR_DUE_TO_UPDATE, err.Error())
			s.finishOpLocked(op.Name, nil, status.Error(codes.Unknown, "Cluster "+c.ClusterName+" failed to update: "+err.Error()))
		} else {
			s.setClusterStateLocked(c, dataprocpb.ClusterStatus_RUNNING, "")
			s.finishOpLocked(op.Name, clone(c), nil)
		}
		s.save()
	}()
	return clone(op), nil
}

func (s *Server) StopCluster(ctx context.Context, req *dataprocpb.StopClusterRequest) (*longrunningpb.Operation, error) {
	return s.stopStart(req.GetProjectId(), req.GetRegion(), req.GetClusterName(), req.GetClusterUuid(), req.GetRequestId(), true)
}

func (s *Server) StartCluster(ctx context.Context, req *dataprocpb.StartClusterRequest) (*longrunningpb.Operation, error) {
	return s.stopStart(req.GetProjectId(), req.GetRegion(), req.GetClusterName(), req.GetClusterUuid(), req.GetRequestId(), false)
}

func (s *Server) stopStart(project, region, clusterName, uuid, requestID string, stop bool) (*longrunningpb.Operation, error) {
	if requestID != "" && !requestIDRE.MatchString(requestID) {
		return nil, apierror.InvalidArgument("request_id %q must be at most 40 letters, digits, underscores and hyphens", requestID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.clusterLocked(project, region, clusterName, uuid)
	if err != nil {
		return nil, err
	}
	verb := map[bool]string{true: "stop", false: "start"}[stop]
	key := project + "/" + region + "|" + verb + "|" + requestID
	if requestID != "" {
		if op, ok := s.ops[s.requests[key]]; ok {
			return clone(op), nil
		}
	}
	st := c.GetStatus().GetState()
	if stop && len(c.GetConfig().GetAuxiliaryNodeGroups()) > 0 {
		return nil, apierror.FailedPrecondition("Cluster %s has node groups: clusters with node groups cannot be stopped or restarted", c.ClusterName)
	}
	if stop && st != dataprocpb.ClusterStatus_RUNNING {
		return nil, apierror.FailedPrecondition("Cluster %s is in state %s: only a RUNNING cluster can be stopped", c.ClusterName, st)
	}
	if !stop && st != dataprocpb.ClusterStatus_STOPPED {
		return nil, apierror.FailedPrecondition("Cluster %s is in state %s: only a STOPPED cluster can be started", c.ClusterName, st)
	}
	via, end := dataprocpb.ClusterStatus_STOPPING, dataprocpb.ClusterStatus_STOPPED
	if !stop {
		via, end = dataprocpb.ClusterStatus_STARTING, dataprocpb.ClusterStatus_RUNNING
	}
	s.setClusterStateLocked(c, via, "")
	op := s.newClusterOpLocked(project, region, c, strings.ToUpper(verb), strings.ToUpper(verb[:1])+verb[1:]+" cluster "+c.ClusterName)
	if requestID != "" {
		s.requests[key] = op.Name
	}
	s.save()
	name := clusterKey(project, region, c.ClusterName)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.mu.Lock()
		s.runOpLocked(op.Name)
		nodes := s.nodes[name]
		s.mu.Unlock()
		var err error
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		switch {
		case nodes == nil && stop:
		case nodes == nil:
			// A cluster stopped before the CLI's last run ended has no
			// containers: start it afresh.
			s.wg.Add(1)
			cancel()
			s.startCluster(name, op.Name, true)
			return
		case stop:
			err = nodes.Stop(ctx)
		default:
			err = nodes.Start(ctx)
		}
		cancel()
		s.mu.Lock()
		defer s.mu.Unlock()
		c := s.clusters[name]
		if c == nil {
			return
		}
		if err != nil {
			s.setClusterStateLocked(c, dataprocpb.ClusterStatus_ERROR, err.Error())
			s.finishOpLocked(op.Name, nil, status.Error(codes.Unknown, err.Error()))
		} else {
			s.setClusterStateLocked(c, end, "")
			if end == dataprocpb.ClusterStatus_RUNNING {
				s.markIdleLocked(c)
			}
			s.finishOpLocked(op.Name, clone(c), nil)
		}
		s.save()
	}()
	return clone(op), nil
}

func (s *Server) DeleteCluster(ctx context.Context, req *dataprocpb.DeleteClusterRequest) (*longrunningpb.Operation, error) {
	if r := req.GetRequestId(); r != "" && !requestIDRE.MatchString(r) {
		return nil, apierror.InvalidArgument("request_id %q must be at most 40 letters, digits, underscores and hyphens", r)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := req.GetProjectId() + "/" + req.GetRegion() + "|delete|" + req.GetRequestId()
	if req.GetRequestId() != "" {
		if op, ok := s.ops[s.requests[key]]; ok {
			return clone(op), nil
		}
	}
	c, err := s.clusterLocked(req.GetProjectId(), req.GetRegion(), req.GetClusterName(), req.GetClusterUuid())
	if err != nil {
		return nil, err
	}
	if c.GetStatus().GetState() == dataprocpb.ClusterStatus_DELETING {
		return nil, apierror.FailedPrecondition("Cluster %s is already being deleted", c.ClusterName)
	}
	op := s.deleteClusterLocked(req.GetProjectId(), req.GetRegion(), c)
	if req.GetRequestId() != "" {
		s.requests[key] = op.Name
	}
	s.save()
	return clone(op), nil
}

// deleteClusterLocked deletes c: DELETING, its jobs' drivers stopped and
// its nodes removed, then gone.
func (s *Server) deleteClusterLocked(project, region string, c *dataprocpb.Cluster) *longrunningpb.Operation {
	s.setClusterStateLocked(c, dataprocpb.ClusterStatus_DELETING, "")
	op := s.newClusterOpLocked(project, region, c, "DELETE", "Delete cluster "+c.ClusterName)
	name := clusterKey(project, region, c.ClusterName)
	for jn, j := range s.jobs {
		if j.GetPlacement().GetClusterUuid() == c.ClusterUuid && !jobDone(j) {
			if cancel := s.cancels[jn]; cancel != nil {
				cancel()
			}
		}
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.mu.Lock()
		s.runOpLocked(op.Name)
		nodes := s.nodes[name]
		delete(s.nodes, name)
		s.mu.Unlock()
		if nodes != nil {
			nodes.Delete()
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if cur := s.clusters[name]; cur != nil && cur.ClusterUuid == c.ClusterUuid {
			delete(s.clusters, name)
		}
		s.finishOpLocked(op.Name, &emptypb.Empty{}, nil)
		s.save()
	}()
	return op
}

func (s *Server) DiagnoseCluster(ctx context.Context, req *dataprocpb.DiagnoseClusterRequest) (*longrunningpb.Operation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.clusterLocked(req.GetProjectId(), req.GetRegion(), req.GetClusterName(), "")
	if err != nil {
		return nil, err
	}
	if c.GetStatus().GetState() != dataprocpb.ClusterStatus_RUNNING {
		return nil, apierror.FailedPrecondition("Cluster %s is in state %s: only a RUNNING cluster can be diagnosed", c.ClusterName, c.GetStatus().GetState())
	}
	dir := strings.TrimSuffix(req.GetTarballGcsDir(), "/")
	if dir != "" && !strings.HasPrefix(dir, "gs://") {
		return nil, apierror.InvalidArgument("tarball_gcs_dir %q must be a gs:// directory", req.GetTarballGcsDir())
	}
	for _, j := range req.GetJobs() {
		if _, ok := s.jobs[j]; !ok {
			return nil, apierror.InvalidArgument("Job %s not found", j)
		}
	}
	op := s.newClusterOpLocked(req.GetProjectId(), req.GetRegion(), c, "DIAGNOSE", "Diagnose cluster "+c.ClusterName)
	task := newUUID()
	if dir == "" {
		// "a task-specific directory in the cluster's staging bucket"
		dir = "gs://" + c.GetConfig().GetConfigBucket() + "/google-cloud-dataproc-metainfo/" + c.ClusterUuid + "/tasks/" + task
	}
	uri := dir + "/diagnostic.tar.gz"
	name := clusterKey(req.GetProjectId(), req.GetRegion(), c.ClusterName)
	summary := s.diagnoseSummaryLocked(c, req)
	project, region, bucketMade := req.GetProjectId(), req.GetRegion(), dir == "" || strings.HasPrefix(dir, "gs://"+c.GetConfig().GetConfigBucket()+"/")
	s.save()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.mu.Lock()
		s.runOpLocked(op.Name)
		nodes := s.nodes[name]
		s.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		var err error
		var data []byte
		switch {
		case nodes == nil:
			err = errors.New("the cluster's nodes are not running")
		case s.opts.Storage == nil:
			err = errors.New("this instance serves no Cloud Storage to write the diagnostic tarball to")
		default:
			data, err = nodes.Diagnose(ctx, summary)
		}
		if err == nil && bucketMade {
			bucket, _, _ := strings.Cut(strings.TrimPrefix(uri, "gs://"), "/")
			err = s.opts.Storage.EnsureBucket(ctx, project, bucket, region)
		}
		if err == nil {
			err = s.opts.Storage.Write(ctx, uri, data)
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if err != nil {
			s.finishOpLocked(op.Name, nil, status.Error(codes.Unknown, "Diagnose failed: "+err.Error()))
		} else {
			s.finishOpLocked(op.Name, &dataprocpb.DiagnoseClusterResults{OutputUri: uri}, nil)
		}
		s.save()
	}()
	return clone(op), nil
}

func (s *Server) diagnoseSummaryLocked(c *dataprocpb.Cluster, req *dataprocpb.DiagnoseClusterRequest) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Cluster: %s\nCluster UUID: %s\nState: %s since %s\n", c.ClusterName, c.ClusterUuid, c.GetStatus().GetState(),
		c.GetStatus().GetStateStartTime().AsTime().Format(time.RFC3339))
	fmt.Fprintf(&b, "Image version: %s\nMaster: %s\nPrimary workers: %d\nSecondary workers: %d\n", c.GetConfig().GetSoftwareConfig().GetImageVersion(),
		strings.Join(c.GetConfig().GetMasterConfig().GetInstanceNames(), ", "), c.GetConfig().GetWorkerConfig().GetNumInstances(),
		c.GetConfig().GetSecondaryWorkerConfig().GetNumInstances())
	b.WriteString("Jobs:\n")
	only := map[string]bool{}
	for _, j := range req.GetJobs() {
		only[j] = true
	}
	for _, k := range sortedKeys(s.jobs) {
		j := s.jobs[k]
		if j.GetPlacement().GetClusterUuid() != c.ClusterUuid || (len(only) > 0 && !only[k]) {
			continue
		}
		fmt.Fprintf(&b, "  %s: %s %s\n", j.GetReference().GetJobId(), j.GetStatus().GetState(), j.GetStatus().GetDetails())
	}
	return b.String()
}

// markIdleLocked records when the cluster last had no running job, its
// lifecycle's idle start time.
func (s *Server) markIdleLocked(c *dataprocpb.Cluster) {
	if lc := c.GetConfig().GetLifecycleConfig(); lc != nil {
		lc.IdleStartTime = timestamppb.New(s.now())
	}
}

// clusterBusyLocked is whether a job runs on the cluster.
func (s *Server) clusterBusyLocked(uuid string) bool {
	for _, j := range s.jobs {
		if j.GetPlacement().GetClusterUuid() == uuid && !jobDone(j) {
			return true
		}
	}
	return false
}

// watchCluster runs the cluster's scheduled deletion and stop and its
// autoscaling policy, until the cluster is gone.
func (s *Server) watchCluster(name, uuid string) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		tick := time.NewTicker(5 * time.Second)
		defer tick.Stop()
		lastScale := time.Time{}
		for {
			select {
			case <-tick.C:
			case <-s.stop:
				return
			}
			s.mu.Lock()
			c := s.clusters[name]
			if s.closed || c == nil || c.ClusterUuid != uuid || s.nodes[name] == nil {
				s.mu.Unlock()
				return
			}
			if c.GetStatus().GetState() != dataprocpb.ClusterStatus_RUNNING {
				s.mu.Unlock()
				continue
			}
			now := s.now()
			lc := c.GetConfig().GetLifecycleConfig()
			busy := s.clusterBusyLocked(uuid)
			if busy {
				s.markIdleLocked(c)
			}
			idleSince := lc.GetIdleStartTime().AsTime()
			created := c.GetStatus().GetStateStartTime().AsTime()
			if len(c.GetStatusHistory()) > 0 {
				created = c.GetStatusHistory()[0].GetStateStartTime().AsTime()
			}
			project, region := c.GetProjectId(), regionOfCluster(c)
			var del, stop bool
			if lc != nil {
				del = (lc.GetIdleDeleteTtl() != nil && !busy && now.Sub(idleSince) > lc.GetIdleDeleteTtl().AsDuration()) ||
					(lc.GetAutoDeleteTime() != nil && now.After(lc.GetAutoDeleteTime().AsTime())) ||
					(lc.GetAutoDeleteTtl() != nil && now.Sub(created) > lc.GetAutoDeleteTtl().AsDuration())
				stop = (lc.GetIdleStopTtl() != nil && !busy && now.Sub(idleSince) > lc.GetIdleStopTtl().AsDuration()) ||
					(lc.GetAutoStopTime() != nil && now.After(lc.GetAutoStopTime().AsTime())) ||
					(lc.GetAutoStopTtl() != nil && now.Sub(created) > lc.GetAutoStopTtl().AsDuration())
			}
			switch {
			case del:
				s.logf("dataproc: cluster %s reached its scheduled deletion", name)
				s.deleteClusterLocked(project, region, c)
				s.save()
				s.mu.Unlock()
				return
			case stop:
				s.mu.Unlock()
				s.logf("dataproc: cluster %s reached its scheduled stop", name)
				_, _ = s.stopStart(project, region, c.ClusterName, "", "", true)
				continue
			}
			pol, nodes, current := s.policyOfLocked(c), s.nodes[name], workerCount(c)
			snap := clone(c)
			s.mu.Unlock()
			s.syncHistory(snap, nodes)
			if pol != nil && now.Sub(lastScale) >= cooldown(pol) {
				if want := recommend(pol, nodes, current, busy); want != current {
					lastScale = now
					s.autoscale(name, uuid, pol, want)
				}
			}
		}
	}()
}

// atoi32 parses n, or 0.
func atoi32(n string) int32 {
	v, _ := strconv.Atoi(n)
	return int32(v)
}
