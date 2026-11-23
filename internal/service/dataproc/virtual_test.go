package dataproc

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	dataprocpb "cloud.google.com/go/dataproc/v2/apiv1/dataprocpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/cloudburrow/cloudburrow/internal/sparkproc"
)

type fakeVirtual struct {
	mu    sync.Mutex
	specs []VirtualSpec
	jobs  []sparkproc.Job
}

func (f *fakeVirtual) StartVirtual(_ context.Context, spec VirtualSpec) (Nodes, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.specs = append(f.specs, spec)
	return &fakeNodes{}, nil
}

func virtualCluster(name, target string, roles []dataprocpb.GkeNodePoolTarget_Role, spark string) *dataprocpb.Cluster {
	return &dataprocpb.Cluster{ClusterName: name, VirtualClusterConfig: &dataprocpb.VirtualClusterConfig{
		InfrastructureConfig: &dataprocpb.VirtualClusterConfig_KubernetesClusterConfig{KubernetesClusterConfig: &dataprocpb.KubernetesClusterConfig{
			Config: &dataprocpb.KubernetesClusterConfig_GkeClusterConfig{GkeClusterConfig: &dataprocpb.GkeClusterConfig{GkeClusterTarget: target,
				NodePoolTarget: []*dataprocpb.GkeNodePoolTarget{{NodePool: target + "/nodePools/default-pool", Roles: roles}}}},
			KubernetesSoftwareConfig: &dataprocpb.KubernetesSoftwareConfig{ComponentVersion: map[string]string{"SPARK": spark}}}}}}
}

// TestVirtualClusters: a Dataproc on GKE cluster's checks and defaults,
// its spec, and the jobs and calls it refuses.
func TestVirtualClusters(t *testing.T) {
	s, _, _ := newClusterServer(t)
	fv := &fakeVirtual{}
	s.opts.Virtual = fv
	ctx := context.Background()
	const target = "projects/p/locations/us-central1-a/clusters/g"
	def := []dataprocpb.GkeNodePoolTarget_Role{dataprocpb.GkeNodePoolTarget_DEFAULT}
	for what, c := range map[string]*dataprocpb.Cluster{
		"a bad target":    virtualCluster("v", "clusters/g", def, "3.5-dataproc-28"),
		"another project": virtualCluster("v", "projects/q/locations/us-central1-a/clusters/g", def, "3.5-dataproc-28"),
		"no DEFAULT pool": virtualCluster("v", target, []dataprocpb.GkeNodePoolTarget_Role{dataprocpb.GkeNodePoolTarget_SPARK_EXECUTOR}, "3.5-dataproc-28"),
		"no Spark":        virtualCluster("v", target, def, ""),
		"an old Spark":    virtualCluster("v", target, def, "3.1-dataproc-7"),
	} {
		if _, err := s.CreateCluster(ctx, &dataprocpb.CreateClusterRequest{ProjectId: "p", Region: "us-central1", Cluster: c}); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: %v", what, err)
		}
	}
	both := virtualCluster("v", target, def, "latest")
	both.Config = &dataprocpb.ClusterConfig{}
	if _, err := s.CreateCluster(ctx, &dataprocpb.CreateClusterRequest{ProjectId: "p", Region: "us-central1", Cluster: both}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("config and virtual_cluster_config: %v", err)
	}
	op, err := s.CreateCluster(ctx, &dataprocpb.CreateClusterRequest{ProjectId: "p", Region: "us-central1", Cluster: virtualCluster("v", target, def, "latest")})
	if err != nil {
		t.Fatal(err)
	}
	if done := waitOp(t, s, op.Name); done.GetError() != nil {
		t.Fatalf("create: %v", done.GetError())
	}
	c, err := s.GetCluster(ctx, &dataprocpb.GetClusterRequest{ProjectId: "p", Region: "us-central1", ClusterName: "v"})
	if err != nil {
		t.Fatal(err)
	}
	k := c.GetVirtualClusterConfig().GetKubernetesClusterConfig()
	if c.GetConfig() != nil || k.GetKubernetesNamespace() != "v" || c.GetVirtualClusterConfig().GetStagingBucket() == "" ||
		c.GetStatus().GetState() != dataprocpb.ClusterStatus_RUNNING {
		t.Errorf("the virtual cluster: %v", c)
	}
	if len(fv.specs) != 1 || fv.specs[0].Spark != "3.5-dataproc-28" || fv.specs[0].Namespace != "v" || fv.specs[0].Target != target ||
		fv.specs[0].PoolFor("SPARK_DRIVER") != "default-pool" {
		t.Errorf("the spec: %+v", fv.specs)
	}
	place := &dataprocpb.JobPlacement{ClusterName: "v"}
	if _, err := s.SubmitJob(ctx, &dataprocpb.SubmitJobRequest{ProjectId: "p", Region: "us-central1", Job: &dataprocpb.Job{Placement: place,
		TypeJob: &dataprocpb.Job_HiveJob{HiveJob: &dataprocpb.HiveJob{Queries: &dataprocpb.HiveJob_QueryList{QueryList: &dataprocpb.QueryList{Queries: []string{"SHOW TABLES"}}}}}}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a Hive job: %v", err)
	}
	j, err := s.SubmitJob(ctx, &dataprocpb.SubmitJobRequest{ProjectId: "p", Region: "us-central1", Job: &dataprocpb.Job{Placement: place,
		TypeJob: &dataprocpb.Job_PysparkJob{PysparkJob: &dataprocpb.PySparkJob{MainPythonFileUri: "gs://b/pi.py"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if want := "gs://" + c.GetVirtualClusterConfig().GetStagingBucket() + "/google-cloud-dataproc-metainfo/"; len(j.GetDriverOutputResourceUri()) < len(want) || j.GetDriverOutputResourceUri()[:len(want)] != want {
		t.Errorf("driver output %s", j.GetDriverOutputResourceUri())
	}
	if _, err := s.StopCluster(ctx, &dataprocpb.StopClusterRequest{ProjectId: "p", Region: "us-central1", ClusterName: "v"}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("stop: %v", err)
	}
}

// yarnNodes are Nodes whose ResourceManager is addr.
type yarnNodes struct {
	fakeNodes
	addr string
}

func (n *yarnNodes) Web(context.Context) map[string]string { return map[string]string{"yarn": n.addr} }

// TestRecommendFromYarnMemory: on a cluster with YARN the autoscaler
// works from the root queue's pending and available memory, as
// Dataproc's algorithm does.
func TestRecommendFromYarnMemory(t *testing.T) {
	var pending, available, allocated float64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/jmx" || r.URL.Query().Get("qry") != "Hadoop:service=ResourceManager,name=QueueMetrics,q0=root" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintf(w, `{"beans":[{"PendingMB":%v,"AvailableMB":%v,"AllocatedMB":%v}]}`, pending, available, allocated)
	}))
	defer srv.Close()
	n := &yarnNodes{addr: strings.TrimPrefix(srv.URL, "http://")}
	p := &dataprocpb.AutoscalingPolicy{
		WorkerConfig: &dataprocpb.InstanceGroupAutoscalingPolicyConfig{MinInstances: 2, MaxInstances: 10},
		Algorithm: &dataprocpb.AutoscalingPolicy_BasicAlgorithm{BasicAlgorithm: &dataprocpb.BasicAutoscalingAlgorithm{
			Config: &dataprocpb.BasicAutoscalingAlgorithm_YarnConfig{YarnConfig: &dataprocpb.BasicYarnAutoscalingConfig{ScaleUpFactor: 1, ScaleDownFactor: 1}}}}}
	// 2 workers of 4096 MB each, all allocated, 8192 MB pending: 2 more.
	pending, available, allocated = 8192, 0, 8192
	if got := recommend(p, n, 2, true); got != 4 {
		t.Errorf("scale up: %d, want 4", got)
	}
	// 4 workers, 8192 MB available and nothing running: down to 2.
	pending, available, allocated = 0, 16384, 0
	if got := recommend(p, n, 4, false); got != 2 {
		t.Errorf("scale down: %d, want 2", got)
	}
}
