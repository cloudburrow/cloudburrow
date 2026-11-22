package dataproc

// Autoscaling policies (#2162): projects.regions.autoscalingPolicies and
// projects.locations.autoscalingPolicies, as autoscaling_policies.proto
// documents their fields and bounds, and the autoscaler that applies a
// cluster's policy.

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"regexp"
	"strings"
	"time"

	dataprocpb "cloud.google.com/go/dataproc/v2/apiv1/dataprocpb"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/cloudburrow/cloudburrow/internal/aipfilter"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

func anyOf(m proto.Message) (*anypb.Any, error) { return anypb.New(m) }

// policyIDRE: "The id must contain only letters (a-z, A-Z), numbers
// (0-9), underscores (_), and hyphens (-). Cannot begin or end with
// underscore or hyphen. Must consist of between 3 and 50 characters."
// Workflow templates' IDs and steps' IDs follow the same rule.
var policyIDRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{1,48}[A-Za-z0-9]$`)

var policyRE = regexp.MustCompile(`^projects/([^/]+)/(regions|locations)/([^/]+)/autoscalingPolicies/([^/]+)$`)

// policyKeyOf is a policy's key whichever collection names it, or a
// policy_uri (a full URL, or projects/...): its regions/ name.
func policyKeyOf(name string) string {
	if i := strings.Index(name, "projects/"); i >= 0 {
		name = name[i:]
	}
	m := policyRE.FindStringSubmatch(name)
	if m == nil {
		return name
	}
	return "projects/" + m[1] + "/regions/" + m[3] + "/autoscalingPolicies/" + m[4]
}

func (s *Server) policyLocked(key string) (*dataprocpb.AutoscalingPolicy, error) {
	if !policyRE.MatchString(key) {
		return nil, apierror.InvalidArgument("name %q must be projects/{project}/regions/{region}/autoscalingPolicies/{policy}", key)
	}
	p, ok := s.policies[key]
	if !ok {
		return nil, apierror.NotFound("Not found: Autoscaling policy %s", key)
	}
	return p, nil
}

func checkPolicy(p *dataprocpb.AutoscalingPolicy) error {
	if !policyIDRE.MatchString(p.GetId()) {
		return apierror.InvalidArgument("Autoscaling policy id %q must be 3 to 50 letters, numbers, underscores and hyphens, not beginning or ending with an underscore or hyphen", p.GetId())
	}
	alg := p.GetBasicAlgorithm()
	if alg == nil {
		return apierror.InvalidArgument("basic_algorithm is required")
	}
	if cd := alg.GetCooldownPeriod(); cd != nil && (cd.AsDuration() < 2*time.Minute || cd.AsDuration() > 24*time.Hour) {
		return apierror.InvalidArgument("basic_algorithm.cooldown_period must be between 2m and 1d")
	}
	y := alg.GetYarnConfig()
	if y == nil {
		return apierror.InvalidArgument("basic_algorithm.yarn_config is required")
	}
	if y.GetGracefulDecommissionTimeout() == nil {
		return apierror.InvalidArgument("basic_algorithm.yarn_config.graceful_decommission_timeout is required")
	}
	if d := y.GetGracefulDecommissionTimeout().AsDuration(); d < 0 || d > 24*time.Hour {
		return apierror.InvalidArgument("basic_algorithm.yarn_config.graceful_decommission_timeout must be between 0s and 1d")
	}
	for _, f := range []struct {
		name string
		v    float64
	}{{"scale_up_factor", y.GetScaleUpFactor()}, {"scale_down_factor", y.GetScaleDownFactor()},
		{"scale_up_min_worker_fraction", y.GetScaleUpMinWorkerFraction()}, {"scale_down_min_worker_fraction", y.GetScaleDownMinWorkerFraction()}} {
		if f.v < 0 || f.v > 1 {
			return apierror.InvalidArgument("basic_algorithm.yarn_config.%s must be between 0.0 and 1.0", f.name)
		}
	}
	w := p.GetWorkerConfig()
	if w == nil {
		return apierror.InvalidArgument("worker_config is required")
	}
	if w.GetMinInstances() == 0 {
		w.MinInstances = 2 // "Default: 2."
	}
	if w.GetMinInstances() < 2 {
		return apierror.InvalidArgument("worker_config.min_instances must be at least 2")
	}
	if w.GetMaxInstances() < w.GetMinInstances() {
		return apierror.InvalidArgument("worker_config.max_instances must be at least worker_config.min_instances (%d)", w.GetMinInstances())
	}
	if sw := p.GetSecondaryWorkerConfig(); sw != nil {
		if sw.GetMinInstances() < 0 || sw.GetMaxInstances() < sw.GetMinInstances() {
			return apierror.InvalidArgument("secondary_worker_config.max_instances must be at least secondary_worker_config.min_instances")
		}
	}
	if len(p.GetLabels()) > maxLabels {
		return apierror.InvalidArgument("No more than %d labels can be associated with an autoscaling policy", maxLabels)
	}
	return nil
}

func (s *Server) CreateAutoscalingPolicy(_ context.Context, req *dataprocpb.CreateAutoscalingPolicyRequest) (*dataprocpb.AutoscalingPolicy, error) {
	m := regionRE.FindStringSubmatch(req.GetParent())
	if m == nil {
		return nil, apierror.InvalidArgument("parent %q must be projects/{project}/regions/{region} or projects/{project}/locations/{location}", req.GetParent())
	}
	if !validLocation(m[3]) {
		return nil, apierror.InvalidArgument("Region %q is not a Dataproc region", m[3])
	}
	if req.GetPolicy() == nil {
		return nil, apierror.InvalidArgument("policy is required")
	}
	p := clone(req.GetPolicy())
	if err := checkPolicy(p); err != nil {
		return nil, err
	}
	p.Name = req.GetParent() + "/autoscalingPolicies/" + p.GetId()
	key := policyKeyOf(p.Name)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.policies[key]; ok {
		return nil, apierror.AlreadyExists("Already exists: Autoscaling policy %s", p.Name)
	}
	s.policies[key] = p
	s.save()
	return clone(p), nil
}

// UpdateAutoscalingPolicy replaces the policy: "Disabled check for
// update_mask, because all updates will be full replacements."
func (s *Server) UpdateAutoscalingPolicy(_ context.Context, req *dataprocpb.UpdateAutoscalingPolicyRequest) (*dataprocpb.AutoscalingPolicy, error) {
	if req.GetPolicy() == nil {
		return nil, apierror.InvalidArgument("policy is required")
	}
	key := policyKeyOf(req.GetPolicy().GetName())
	p := clone(req.GetPolicy())
	if p.Id == "" {
		p.Id = lastSegment(key)
	}
	if err := checkPolicy(p); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, err := s.policyLocked(key)
	if err != nil {
		return nil, err
	}
	if p.Id != cur.Id {
		return nil, apierror.InvalidArgument("policy.id %q must be the policy's own, %q", p.Id, cur.Id)
	}
	p.Name = cur.Name
	s.policies[key] = p
	s.save()
	return clone(p), nil
}

func (s *Server) GetAutoscalingPolicy(_ context.Context, req *dataprocpb.GetAutoscalingPolicyRequest) (*dataprocpb.AutoscalingPolicy, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.policyLocked(policyKeyOf(req.GetName()))
	if err != nil {
		return nil, err
	}
	return clone(p), nil
}

func (s *Server) ListAutoscalingPolicies(_ context.Context, req *dataprocpb.ListAutoscalingPoliciesRequest) (*dataprocpb.ListAutoscalingPoliciesResponse, error) {
	m := regionRE.FindStringSubmatch(req.GetParent())
	if m == nil {
		return nil, apierror.InvalidArgument("parent %q must be projects/{project}/regions/{region} or projects/{project}/locations/{location}", req.GetParent())
	}
	// "Maximum page_size is 1000." Default 100.
	if req.GetPageSize() > 1000 {
		return nil, apierror.InvalidArgument("page_size must be at most 1000")
	}
	size := int(req.GetPageSize())
	if size == 0 {
		size = 100
	}
	prefix := "projects/" + m[1] + "/regions/" + m[3] + "/autoscalingPolicies/"
	s.mu.Lock()
	var all []*dataprocpb.AutoscalingPolicy
	for _, k := range sortedKeys(s.policies) {
		if strings.HasPrefix(k, prefix) {
			all = append(all, clone(s.policies[k]))
		}
	}
	s.mu.Unlock()
	out, next, err := aipfilter.Page("dataproc-policies:"+prefix, all, req.GetPageToken(), size)
	if err != nil {
		return nil, err
	}
	return &dataprocpb.ListAutoscalingPoliciesResponse{Policies: out, NextPageToken: next}, nil
}

// DeleteAutoscalingPolicy: "Deletes an autoscaling policy. It is an
// error to delete an autoscaling policy that is in use by one or more
// clusters."
func (s *Server) DeleteAutoscalingPolicy(_ context.Context, req *dataprocpb.DeleteAutoscalingPolicyRequest) (*emptypb.Empty, error) {
	key := policyKeyOf(req.GetName())
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.policyLocked(key); err != nil {
		return nil, err
	}
	for _, c := range s.clusters {
		if p := c.GetConfig().GetAutoscalingConfig().GetPolicyUri(); p != "" && policyKeyOf(p) == key {
			return nil, apierror.FailedPrecondition("Autoscaling policy %s is in use by cluster %s", req.GetName(), c.ClusterName)
		}
	}
	delete(s.policies, key)
	s.save()
	return &emptypb.Empty{}, nil
}

// policyOfLocked is the cluster's autoscaling policy, or nil.
func (s *Server) policyOfLocked(c *dataprocpb.Cluster) *dataprocpb.AutoscalingPolicy {
	uri := c.GetConfig().GetAutoscalingConfig().GetPolicyUri()
	if uri == "" {
		return nil
	}
	p, ok := s.policies[policyKeyOf(uri)]
	if !ok {
		return nil
	}
	return clone(p)
}

// cooldown is the policy's cooldown period: "Default: 2m."
func cooldown(p *dataprocpb.AutoscalingPolicy) time.Duration {
	if cd := p.GetBasicAlgorithm().GetCooldownPeriod(); cd != nil {
		return cd.AsDuration()
	}
	return 2 * time.Minute
}

// masterState is the standalone master's /json/: its cores and the
// active applications', which stand in for YARN's memory metrics.
type masterState struct {
	Cores      int `json:"cores"`
	CoresUsed  int `json:"coresused"`
	ActiveApps []struct {
		State string `json:"state"`
		Cores int    `json:"cores"`
	} `json:"activeapps"`
}

// recommend is the autoscaler's worker count for a cluster of current
// workers. Dataproc's algorithm works from YARN's pending and available
// memory; a local cluster has Spark's standalone scheduler, so its
// pending capacity is the active applications' cores beyond the
// cluster's (an application WAITING for executors), its available
// capacity the cores no application holds. The scale-up and scale-down
// factors, the minimum worker fractions and the bounds apply as the
// policy documents them.
func recommend(p *dataprocpb.AutoscalingPolicy, nodes Nodes, current int, busy bool) int {
	if nodes == nil || current == 0 {
		return current
	}
	y := p.GetBasicAlgorithm().GetYarnConfig()
	lo := int(p.GetWorkerConfig().GetMinInstances() + p.GetSecondaryWorkerConfig().GetMinInstances())
	hi := int(p.GetWorkerConfig().GetMaxInstances() + p.GetSecondaryWorkerConfig().GetMaxInstances())
	var st masterState
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if addr := nodes.Web(ctx)["master"]; addr != "" {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/json/", nil)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			_ = json.NewDecoder(resp.Body).Decode(&st)
			resp.Body.Close()
		}
	}
	perWorker := 2.0
	if current > 0 && st.Cores > 0 {
		perWorker = float64(st.Cores) / float64(current)
	}
	pending := 0.0
	for _, a := range st.ActiveApps {
		if a.State == "WAITING" {
			pending += perWorker
		}
	}
	if st.Cores > 0 && st.CoresUsed >= st.Cores && len(st.ActiveApps) > 0 {
		pending += perWorker
	}
	available := float64(st.Cores - st.CoresUsed)
	delta := 0
	switch {
	case pending > 0:
		delta = int(math.Ceil(pending * y.GetScaleUpFactor() / perWorker))
		if float64(delta) < y.GetScaleUpMinWorkerFraction()*float64(current) {
			delta = 0
		}
	case !busy && available > 0:
		delta = -int(math.Floor(available * y.GetScaleDownFactor() / perWorker))
		if float64(-delta) < y.GetScaleDownMinWorkerFraction()*float64(current) {
			delta = 0
		}
	}
	want := current + delta
	if want < lo {
		want = lo
	}
	if want > hi {
		want = hi
	}
	return want
}

// autoscale resizes the cluster to want workers, primary workers first
// up to the policy's maximum, the rest secondary, as an UPDATE operation.
func (s *Server) autoscale(name, uuid string, p *dataprocpb.AutoscalingPolicy, want int) {
	s.mu.Lock()
	c := s.clusters[name]
	if c == nil || c.ClusterUuid != uuid || c.GetStatus().GetState() != dataprocpb.ClusterStatus_RUNNING {
		s.mu.Unlock()
		return
	}
	primary := want
	if max := int(p.GetWorkerConfig().GetMaxInstances()); primary > max {
		primary = max
	}
	secondary := want - primary
	project, region := c.GetProjectId(), regionOfCluster(c)
	s.mu.Unlock()
	s.logf("dataproc: autoscaling cluster %s to %d primary and %d secondary workers", name, primary, secondary)
	cfg := &dataprocpb.ClusterConfig{WorkerConfig: &dataprocpb.InstanceGroupConfig{NumInstances: int32(primary)},
		SecondaryWorkerConfig: &dataprocpb.InstanceGroupConfig{NumInstances: int32(secondary)}}
	paths := []string{"config.worker_config.num_instances", "config.secondary_worker_config.num_instances"}
	if _, err := s.UpdateCluster(context.Background(), &dataprocpb.UpdateClusterRequest{ProjectId: project, Region: region, ClusterName: lastSegment(name),
		Cluster: &dataprocpb.Cluster{Config: cfg}, UpdateMask: fieldMask(paths...)}); err != nil {
		s.logf("dataproc: autoscaling %s: %v", name, err)
	}
}
