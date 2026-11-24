package dataproc

// Dataproc on GKE (#2162): virtual clusters
// (https://docs.cloud.google.com/dataproc/docs/guides/dpgke/dataproc-gke-overview
// and the VirtualClusterConfig of google/cloud/dataproc/v1/clusters.proto),
// a cluster whose Spark runs in a namespace of a Kubernetes Engine
// cluster of the instance: each job's driver is a pod there, its
// executors pods it starts (Spark on Kubernetes), on the node pools the
// cluster's node pool targets give each role.

import (
	"context"
	"regexp"
	"sort"
	"strings"

	dataprocpb "cloud.google.com/go/dataproc/v2/apiv1/dataprocpb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// VirtualEngine starts virtual clusters on the instance's Kubernetes
// Engine clusters.
type VirtualEngine interface {
	StartVirtual(ctx context.Context, spec VirtualSpec) (Nodes, error)
}

// VirtualSpec is a virtual cluster's engine spec.
type VirtualSpec struct {
	// Name and UUID are the Dataproc cluster's; Target its GKE cluster
	// (projects/P/locations/L/clusters/C); Namespace its Kubernetes
	// namespace.
	Name, UUID, Target, Namespace string
	// Spark is the Spark component version (3.5-dataproc-N), resolved.
	Spark string
	// Pools are the node pool targets: a pool's full name, its roles
	// (DEFAULT, CONTROLLER, SPARK_DRIVER, SPARK_EXECUTOR), and the
	// configuration a pool Dataproc creates is made with (nil: it must
	// exist).
	Pools []VirtualPool
	// Properties are the kubernetesSoftwareConfig's properties, "spark:"
	// ones with their prefix dropped: the jobs' Spark defaults.
	Properties map[string]string
}

// VirtualPool is one node pool target.
type VirtualPool struct {
	Name   string
	ID     string
	Roles  []string
	Config *dataprocpb.GkeNodePoolConfig
}

// PoolFor is the node pool a role's pods run on: the pool with that role,
// or the DEFAULT pool.
func (v VirtualSpec) PoolFor(role string) string {
	def := ""
	for _, p := range v.Pools {
		for _, r := range p.Roles {
			if r == role {
				return p.ID
			}
			if r == "DEFAULT" && def == "" {
				def = p.ID
			}
		}
	}
	return def
}

var (
	gkeTargetRE = regexp.MustCompile(`^projects/([^/]+)/locations/([^/]+)/clusters/([^/]+)$`)
	gkePoolRE   = regexp.MustCompile(`^(projects/[^/]+/locations/[^/]+/clusters/[^/]+)/nodePools/([a-z]([-a-z0-9]{0,38}[a-z0-9])?)$`)
	namespaceRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)
)

// sparkOnGKEVersions are the Spark component versions of Dataproc on GKE
// (the versions page, re-read 2026-11-22:
// https://docs.cloud.google.com/dataproc/docs/guides/dpgke/dataproc-gke-versions),
// and the aliases that resolve to the newest.
var sparkOnGKEVersions = []string{"3.5-dataproc-28", "3.5-dataproc-22", "3.5-dataproc-17"}

var sparkOnGKEAliases = map[string]bool{"3": true, "3.5": true, "dataproc-2.2": true, "latest": true}

// virtual is whether c is a Dataproc on GKE cluster.
func virtual(c *dataprocpb.Cluster) bool { return c.GetVirtualClusterConfig() != nil }

// configBucket is the bucket a cluster's jobs' control files and driver
// output are written to: its config bucket, or a virtual cluster's
// staging bucket.
func configBucket(c *dataprocpb.Cluster) string {
	if v := c.GetVirtualClusterConfig(); v != nil {
		return v.GetStagingBucket()
	}
	return c.GetConfig().GetConfigBucket()
}

// checkVirtual checks a virtual cluster to create and fills in what
// Dataproc sets by default.
func (s *Server) checkVirtual(project, region string, c *dataprocpb.Cluster) error {
	v := c.GetVirtualClusterConfig()
	k := v.GetKubernetesClusterConfig()
	if k == nil {
		return apierror.InvalidArgument("virtual_cluster_config.kubernetes_cluster_config is required")
	}
	g := k.GetGkeClusterConfig()
	m := gkeTargetRE.FindStringSubmatch(g.GetGkeClusterTarget())
	if m == nil {
		return apierror.InvalidArgument("virtual_cluster_config.kubernetes_cluster_config.gke_cluster_config.gke_cluster_target %q must be projects/{project}/locations/{location}/clusters/{cluster_id}", g.GetGkeClusterTarget())
	}
	if m[1] != project {
		return apierror.InvalidArgument("The GKE cluster %s must be in project %s", g.GetGkeClusterTarget(), project)
	}
	if k.KubernetesNamespace == "" {
		// "If not specified, the name of the Dataproc Cluster is used."
		k.KubernetesNamespace = c.GetClusterName()
	}
	if !namespaceRE.MatchString(k.KubernetesNamespace) {
		return apierror.InvalidArgument("kubernetes_namespace %q must be a Kubernetes namespace name: up to 63 lowercase letters, digits and hyphens", k.KubernetesNamespace)
	}
	defaults := 0
	seen := map[string]bool{}
	for i, t := range g.GetNodePoolTarget() {
		pm := gkePoolRE.FindStringSubmatch(t.GetNodePool())
		if pm == nil {
			return apierror.InvalidArgument("node_pool_target[%d].node_pool %q must be projects/{project}/locations/{location}/clusters/{cluster}/nodePools/{node_pool}", i, t.GetNodePool())
		}
		if pm[1] != g.GetGkeClusterTarget() {
			return apierror.InvalidArgument("node_pool_target[%d].node_pool %s is not a node pool of the GKE cluster %s", i, t.GetNodePool(), g.GetGkeClusterTarget())
		}
		if seen[t.GetNodePool()] {
			return apierror.InvalidArgument("node pool %s is targeted more than once", t.GetNodePool())
		}
		seen[t.GetNodePool()] = true
		if len(t.GetRoles()) == 0 {
			return apierror.InvalidArgument("node_pool_target[%d].roles must name at least one role", i)
		}
		for _, r := range t.GetRoles() {
			if r == dataprocpb.GkeNodePoolTarget_ROLE_UNSPECIFIED {
				return apierror.InvalidArgument("node_pool_target[%d].roles: ROLE_UNSPECIFIED is not a role", i)
			}
			if r == dataprocpb.GkeNodePoolTarget_DEFAULT {
				defaults++
			}
		}
	}
	// "At least one node pool must have the DEFAULT role."
	if defaults != 1 {
		return apierror.InvalidArgument("Exactly one node pool target must have the DEFAULT role; %d do", defaults)
	}
	sw := k.GetKubernetesSoftwareConfig()
	if sw == nil {
		k.KubernetesSoftwareConfig = &dataprocpb.KubernetesSoftwareConfig{}
		sw = k.KubernetesSoftwareConfig
	}
	for comp := range sw.GetComponentVersion() {
		if comp != "SPARK" {
			return apierror.InvalidArgument("kubernetes_software_config.component_version: %q is not a component of Dataproc on GKE; SPARK is", comp)
		}
	}
	ver := sw.GetComponentVersion()["SPARK"]
	if ver == "" {
		return apierror.InvalidArgument("kubernetes_software_config.component_version must give the SPARK version, one of %s", strings.Join(sparkOnGKEVersions, ", "))
	}
	if !sparkOnGKEAliases[ver] {
		ok := false
		for _, x := range sparkOnGKEVersions {
			ok = ok || x == ver
		}
		if !ok {
			return apierror.InvalidArgument("Spark version %q is not supported on Dataproc on GKE: the versions are %s", ver, strings.Join(sparkOnGKEVersions, ", "))
		}
	}
	for key := range sw.GetProperties() {
		prefix, _, ok := strings.Cut(key, ":")
		if !ok || prefix != "spark" && prefix != "dataproc" {
			return apierror.InvalidArgument("kubernetes_software_config.properties: %q must be prefix:property, prefix spark or dataproc", key)
		}
	}
	if h := v.GetAuxiliaryServicesConfig().GetSparkHistoryServerConfig().GetDataprocCluster(); h != "" && !clusterRE.MatchString(h) {
		return apierror.InvalidArgument("spark_history_server_config.dataproc_cluster %q must be projects/{project}/regions/{region}/clusters/{cluster}", h)
	}
	if v.StagingBucket == "" {
		v.StagingBucket = s.StagingBucket(project, region)
	}
	return nil
}

// virtualSpec is the engine's spec of the virtual cluster c.
func virtualSpec(c *dataprocpb.Cluster) VirtualSpec {
	k := c.GetVirtualClusterConfig().GetKubernetesClusterConfig()
	g := k.GetGkeClusterConfig()
	ver := k.GetKubernetesSoftwareConfig().GetComponentVersion()["SPARK"]
	if sparkOnGKEAliases[ver] {
		ver = sparkOnGKEVersions[0]
	}
	props := map[string]string{}
	for key, val := range k.GetKubernetesSoftwareConfig().GetProperties() {
		if rest, ok := strings.CutPrefix(key, "spark:"); ok {
			props[rest] = val
		}
	}
	spec := VirtualSpec{Name: c.GetClusterName(), UUID: c.GetClusterUuid(), Target: g.GetGkeClusterTarget(),
		Namespace: k.GetKubernetesNamespace(), Spark: ver, Properties: props}
	for _, t := range g.GetNodePoolTarget() {
		pm := gkePoolRE.FindStringSubmatch(t.GetNodePool())
		p := VirtualPool{Name: t.GetNodePool(), Config: t.GetNodePoolConfig()}
		if pm != nil {
			p.ID = pm[2]
		}
		for _, r := range t.GetRoles() {
			p.Roles = append(p.Roles, r.String())
		}
		sort.Strings(p.Roles)
		spec.Pools = append(spec.Pools, p)
	}
	return spec
}

// virtualJob is whether a virtual cluster runs j: "Dataproc on GKE
// supports Spark, PySpark, SparkR, and Spark SQL jobs."
func virtualJob(j *dataprocpb.Job) bool {
	return j.GetSparkJob() != nil || j.GetPysparkJob() != nil || j.GetSparkRJob() != nil || j.GetSparkSqlJob() != nil
}
