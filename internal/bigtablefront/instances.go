package bigtablefront

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	btapb "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	"cloud.google.com/go/iam/apiv1/iampb"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/iampolicy"
)

// The instance admin API (#1331). The emulator registers
// BigtableInstanceAdmin but answers every method of it UNIMPLEMENTED (bar a
// DeleteInstance of instances it never has), so the front serves it itself:
// instances, clusters, app profiles and instance IAM policies, kept in
// memory as the emulator keeps its tables, and the long-running operations
// Google answers the mutating methods with, done when they are returned.
//
// The emulator scopes tables by their full name, projects/*/instances/*/
// tables/*, so a created instance is a real namespace for the table admin
// and data calls. Calls naming an instance that was never created are still
// passed to the emulator, as they always have been, rather than refused
// NOT_FOUND as Google refuses them: every client of the emulator before
// #1331 (cbt, the SDKs' emulator mode, Terraform's google_bigtable_table)
// names an instance it never created. Deleting an instance deletes its
// tables.

const (
	instanceAdmin = "/google.bigtable.admin.v2.BigtableInstanceAdmin/"
	operations    = "/google.longrunning.Operations/"
)

var (
	projectRe     = regexp.MustCompile(`^projects/([^/]+)$`)
	instanceRe    = regexp.MustCompile(`^projects/([^/]+)/instances/([^/]+)$`)
	clusterRe     = regexp.MustCompile(`^projects/([^/]+)/instances/([^/]+)/clusters/([^/]+)$`)
	appProfileRe  = regexp.MustCompile(`^projects/([^/]+)/instances/([^/]+)/appProfiles/([^/]+)$`)
	locationRe    = regexp.MustCompile(`^projects/[^/]+/locations/([a-z][-a-z0-9]*)$`)
	instanceIDRe  = regexp.MustCompile(`^[a-z][-a-z0-9]{4,31}[a-z0-9]$`)
	clusterIDRe   = regexp.MustCompile(`^[a-z][-a-z0-9]{4,28}[a-z0-9]$`)
	appProfileRe2 = regexp.MustCompile(`^[_a-zA-Z0-9][-_.a-zA-Z0-9]*$`)
)

// instance is one created instance and what belongs to it.
type instance struct {
	pb       *btapb.Instance
	clusters map[string]*btapb.Cluster    // by cluster ID
	profiles map[string]*btapb.AppProfile // by app profile ID
	policy   *iampolicy.Stored
}

// instances is the state of the instance admin API.
type instances struct {
	mu   sync.Mutex
	now  func() time.Time
	byNm map[string]*instance // by instance name
	ops  map[string]*longrunningpb.Operation
	opsQ []string
	// dropTables deletes every table of an instance in the emulator.
	dropTables func(ctx context.Context, instance string) error
}

func newInstances(dropTables func(context.Context, string) error) *instances {
	return &instances{
		now:        func() time.Time { return time.Now().UTC() },
		byNm:       map[string]*instance{},
		ops:        map[string]*longrunningpb.Operation{},
		dropTables: dropTables,
	}
}

// method is one served method: it decodes its request from the frame and
// returns the response to encode.
type method func(ctx context.Context, in []byte) (proto.Message, error)

func unary[Req proto.Message](mk func() Req, fn func(context.Context, Req) (proto.Message, error)) method {
	return func(ctx context.Context, in []byte) (proto.Message, error) {
		req := mk()
		if err := proto.Unmarshal(in, req); err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "cannot decode the request: %v", err)
		}
		return fn(ctx, req)
	}
}

// methods are the instance admin and operations methods the front serves.
func (s *instances) methods() map[string]method {
	return map[string]method{
		instanceAdmin + "CreateInstance":        unary(func() *btapb.CreateInstanceRequest { return &btapb.CreateInstanceRequest{} }, s.createInstance),
		instanceAdmin + "GetInstance":           unary(func() *btapb.GetInstanceRequest { return &btapb.GetInstanceRequest{} }, s.getInstance),
		instanceAdmin + "ListInstances":         unary(func() *btapb.ListInstancesRequest { return &btapb.ListInstancesRequest{} }, s.listInstances),
		instanceAdmin + "UpdateInstance":        unary(func() *btapb.Instance { return &btapb.Instance{} }, s.updateInstance),
		instanceAdmin + "PartialUpdateInstance": unary(func() *btapb.PartialUpdateInstanceRequest { return &btapb.PartialUpdateInstanceRequest{} }, s.partialUpdateInstance),
		instanceAdmin + "DeleteInstance":        unary(func() *btapb.DeleteInstanceRequest { return &btapb.DeleteInstanceRequest{} }, s.deleteInstance),
		instanceAdmin + "CreateCluster":         unary(func() *btapb.CreateClusterRequest { return &btapb.CreateClusterRequest{} }, s.createCluster),
		instanceAdmin + "GetCluster":            unary(func() *btapb.GetClusterRequest { return &btapb.GetClusterRequest{} }, s.getCluster),
		instanceAdmin + "ListClusters":          unary(func() *btapb.ListClustersRequest { return &btapb.ListClustersRequest{} }, s.listClusters),
		instanceAdmin + "UpdateCluster":         unary(func() *btapb.Cluster { return &btapb.Cluster{} }, s.updateCluster),
		instanceAdmin + "PartialUpdateCluster":  unary(func() *btapb.PartialUpdateClusterRequest { return &btapb.PartialUpdateClusterRequest{} }, s.partialUpdateCluster),
		instanceAdmin + "DeleteCluster":         unary(func() *btapb.DeleteClusterRequest { return &btapb.DeleteClusterRequest{} }, s.deleteCluster),
		instanceAdmin + "CreateAppProfile":      unary(func() *btapb.CreateAppProfileRequest { return &btapb.CreateAppProfileRequest{} }, s.createAppProfile),
		instanceAdmin + "GetAppProfile":         unary(func() *btapb.GetAppProfileRequest { return &btapb.GetAppProfileRequest{} }, s.getAppProfile),
		instanceAdmin + "ListAppProfiles":       unary(func() *btapb.ListAppProfilesRequest { return &btapb.ListAppProfilesRequest{} }, s.listAppProfiles),
		instanceAdmin + "UpdateAppProfile":      unary(func() *btapb.UpdateAppProfileRequest { return &btapb.UpdateAppProfileRequest{} }, s.updateAppProfile),
		instanceAdmin + "DeleteAppProfile":      unary(func() *btapb.DeleteAppProfileRequest { return &btapb.DeleteAppProfileRequest{} }, s.deleteAppProfile),
		instanceAdmin + "GetIamPolicy":          unary(func() *iampb.GetIamPolicyRequest { return &iampb.GetIamPolicyRequest{} }, s.getIamPolicy),
		instanceAdmin + "SetIamPolicy":          unary(func() *iampb.SetIamPolicyRequest { return &iampb.SetIamPolicyRequest{} }, s.setIamPolicy),
		instanceAdmin + "TestIamPermissions":    unary(func() *iampb.TestIamPermissionsRequest { return &iampb.TestIamPermissionsRequest{} }, s.testIamPermissions),
		operations + "GetOperation":             unary(func() *longrunningpb.GetOperationRequest { return &longrunningpb.GetOperationRequest{} }, s.getOperation),
		operations + "ListOperations":           unary(func() *longrunningpb.ListOperationsRequest { return &longrunningpb.ListOperationsRequest{} }, s.listOperations),
		operations + "DeleteOperation":          unary(func() *longrunningpb.DeleteOperationRequest { return &longrunningpb.DeleteOperationRequest{} }, s.deleteOperation),
		operations + "CancelOperation":          unary(func() *longrunningpb.CancelOperationRequest { return &longrunningpb.CancelOperationRequest{} }, s.cancelOperation),
		operations + "WaitOperation":            unary(func() *longrunningpb.WaitOperationRequest { return &longrunningpb.WaitOperationRequest{} }, s.waitOperation),
	}
}

// serve answers one unary call to a served method on ss.
func serveUnary(ss grpc.ServerStream, m method) error {
	var in frame
	if err := ss.RecvMsg(&in); err != nil {
		return err
	}
	resp, err := m(ss.Context(), in)
	if err != nil {
		return err
	}
	return ss.SendMsg(resp)
}

func notFound(kind, name string) error {
	return status.Errorf(codes.NotFound, "%s %s not found.", kind, name)
}

// lookup returns the instance name names, or NOT_FOUND. s.mu is held.
func (s *instances) lookup(name string) (*instance, error) {
	if !instanceRe.MatchString(name) {
		return nil, status.Errorf(codes.InvalidArgument, "invalid instance name %q: want projects/{project}/instances/{instance}", name)
	}
	in, ok := s.byNm[name]
	if !ok {
		return nil, notFound("Instance", name)
	}
	return in, nil
}

// done records a finished operation and returns it. s.mu is held.
func (s *instances) done(target string, meta, resp proto.Message) (proto.Message, error) {
	m, err := anypb.New(meta)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "operation metadata: %v", err)
	}
	r, err := anypb.New(resp)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "operation response: %v", err)
	}
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	op := &longrunningpb.Operation{Name: "operations/" + target + "/operations/" + hex.EncodeToString(b),
		Metadata: m, Done: true, Result: &longrunningpb.Operation_Response{Response: r}}
	s.ops[op.Name] = op
	s.opsQ = append(s.opsQ, op.Name)
	if len(s.opsQ) > 1000 {
		delete(s.ops, s.opsQ[0])
		s.opsQ = s.opsQ[1:]
	}
	return proto.Clone(op), nil
}

// --- instances ---

func (s *instances) createInstance(_ context.Context, req *btapb.CreateInstanceRequest) (proto.Message, error) {
	if !projectRe.MatchString(req.GetParent()) {
		return nil, status.Errorf(codes.InvalidArgument, "invalid parent %q: want projects/{project}", req.GetParent())
	}
	id := req.GetInstanceId()
	if !instanceIDRe.MatchString(id) {
		return nil, status.Errorf(codes.InvalidArgument, "instance_id %q must be 6 to 33 characters: a lowercase letter, then lowercase letters, digits and hyphens, not ending in a hyphen", id)
	}
	ipb := req.GetInstance()
	if ipb == nil {
		return nil, status.Error(codes.InvalidArgument, "instance is required")
	}
	if n := len(ipb.GetDisplayName()); n < 4 || n > 30 {
		return nil, status.Errorf(codes.InvalidArgument, "instance.display_name must be 4 to 30 characters, is %d", n)
	}
	if len(req.GetClusters()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "clusters: at least one cluster is required")
	}
	name := req.GetParent() + "/instances/" + id
	now := s.now()
	in := &instance{clusters: map[string]*btapb.Cluster{}, profiles: map[string]*btapb.AppProfile{}}
	ids := make([]string, 0, len(req.GetClusters()))
	for cid := range req.GetClusters() {
		ids = append(ids, cid)
	}
	sort.Strings(ids)
	for _, cid := range ids {
		c, err := newCluster(name, cid, req.GetClusters()[cid], ipb.GetType())
		if err != nil {
			return nil, err
		}
		in.clusters[cid] = c
	}
	in.pb = &btapb.Instance{Name: name, DisplayName: ipb.GetDisplayName(), State: btapb.Instance_READY,
		Type: ipb.GetType(), Edition: ipb.GetEdition(), Labels: ipb.GetLabels(), CreateTime: timestamppb.New(now),
		SatisfiesPzs: proto.Bool(false), SatisfiesPzi: proto.Bool(false)}
	if in.pb.Type == btapb.Instance_TYPE_UNSPECIFIED {
		in.pb.Type = btapb.Instance_PRODUCTION
	}
	// Every instance is created with a default app profile, routing to its
	// one cluster or, with several, to any.
	def := &btapb.AppProfile{Name: name + "/appProfiles/default", Etag: newEtag(), Description: "Default application profile"}
	if len(ids) == 1 {
		def.RoutingPolicy = &btapb.AppProfile_SingleClusterRouting_{SingleClusterRouting: &btapb.AppProfile_SingleClusterRouting{ClusterId: ids[0]}}
	} else {
		def.RoutingPolicy = &btapb.AppProfile_MultiClusterRoutingUseAny_{MultiClusterRoutingUseAny: &btapb.AppProfile_MultiClusterRoutingUseAny{}}
	}
	in.profiles["default"] = def

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.byNm[name]; ok {
		return nil, status.Errorf(codes.AlreadyExists, "Instance %s already exists.", name)
	}
	s.byNm[name] = in
	return s.done(name, &btapb.CreateInstanceMetadata{OriginalRequest: req, RequestTime: timestamppb.New(now),
		FinishTime: timestamppb.New(s.now())}, proto.Clone(in.pb))
}

// newCluster validates a cluster of the instance and returns it as stored.
func newCluster(instance, id string, c *btapb.Cluster, typ btapb.Instance_Type) (*btapb.Cluster, error) {
	if !clusterIDRe.MatchString(id) {
		return nil, status.Errorf(codes.InvalidArgument, "cluster_id %q must be 6 to 30 characters: a lowercase letter, then lowercase letters, digits and hyphens, not ending in a hyphen", id)
	}
	if c == nil {
		return nil, status.Errorf(codes.InvalidArgument, "cluster %s is required", id)
	}
	if !locationRe.MatchString(c.GetLocation()) {
		return nil, status.Errorf(codes.InvalidArgument, "cluster %s: location %q must be projects/{project}/locations/{zone}", id, c.GetLocation())
	}
	out := proto.Clone(c).(*btapb.Cluster)
	out.Name = instance + "/clusters/" + id
	out.State = btapb.Cluster_READY
	if out.DefaultStorageType == btapb.StorageType_STORAGE_TYPE_UNSPECIFIED {
		out.DefaultStorageType = btapb.StorageType_SSD
	}
	if out.NodeScalingFactor == btapb.Cluster_NODE_SCALING_FACTOR_UNSPECIFIED {
		out.NodeScalingFactor = btapb.Cluster_NODE_SCALING_FACTOR_1X
	}
	if err := checkNodes(out, typ); err != nil {
		return nil, err
	}
	return out, nil
}

// checkNodes refuses a production cluster with neither nodes nor autoscaling,
// and sets an autoscaled cluster's nodes to its minimum.
func checkNodes(c *btapb.Cluster, typ btapb.Instance_Type) error {
	if a := c.GetClusterConfig().GetClusterAutoscalingConfig(); a != nil {
		l := a.GetAutoscalingLimits()
		if l.GetMinServeNodes() < 1 || l.GetMaxServeNodes() < l.GetMinServeNodes() {
			return status.Errorf(codes.InvalidArgument, "cluster %s: autoscaling limits need 1 <= min_serve_nodes <= max_serve_nodes", c.GetName())
		}
		if t := a.GetAutoscalingTargets().GetCpuUtilizationPercent(); t != 0 && (t < 10 || t > 80) {
			return status.Errorf(codes.InvalidArgument, "cluster %s: cpu_utilization_percent must be 10 to 80", c.GetName())
		}
		if c.ServeNodes < l.GetMinServeNodes() || c.ServeNodes > l.GetMaxServeNodes() {
			c.ServeNodes = l.GetMinServeNodes()
		}
		return nil
	}
	if c.ServeNodes < 0 {
		return status.Errorf(codes.InvalidArgument, "cluster %s: serve_nodes cannot be negative", c.GetName())
	}
	if c.ServeNodes == 0 && typ != btapb.Instance_DEVELOPMENT {
		return status.Errorf(codes.InvalidArgument, "cluster %s: serve_nodes must be at least 1, or the cluster autoscaled", c.GetName())
	}
	return nil
}

func (s *instances) getInstance(_ context.Context, req *btapb.GetInstanceRequest) (proto.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	in, err := s.lookup(req.GetName())
	if err != nil {
		return nil, err
	}
	return proto.Clone(in.pb), nil
}

func (s *instances) listInstances(_ context.Context, req *btapb.ListInstancesRequest) (proto.Message, error) {
	if !projectRe.MatchString(req.GetParent()) {
		return nil, status.Errorf(codes.InvalidArgument, "invalid parent %q: want projects/{project}", req.GetParent())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := &btapb.ListInstancesResponse{}
	for _, n := range s.sortedNames(req.GetParent() + "/instances/") {
		out.Instances = append(out.Instances, proto.Clone(s.byNm[n].pb).(*btapb.Instance))
	}
	return out, nil
}

// sortedNames are the instance names under prefix. s.mu is held.
func (s *instances) sortedNames(prefix string) []string {
	var names []string
	for n := range s.byNm {
		if strings.HasPrefix(n, prefix) {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return names
}

// applyInstance applies the instance fields paths names. A nil paths is
// UpdateInstance's: display name, type and labels.
func applyInstance(dst, src *btapb.Instance, paths []string) error {
	if paths == nil {
		paths = []string{"display_name", "type", "labels"}
	}
	for _, p := range paths {
		switch p {
		case "display_name", "displayName":
			if n := len(src.GetDisplayName()); n < 4 || n > 30 {
				return status.Errorf(codes.InvalidArgument, "instance.display_name must be 4 to 30 characters, is %d", n)
			}
			dst.DisplayName = src.GetDisplayName()
		case "type":
			t := src.GetType()
			if t == btapb.Instance_TYPE_UNSPECIFIED {
				t = dst.Type
			}
			if dst.Type == btapb.Instance_PRODUCTION && t == btapb.Instance_DEVELOPMENT {
				return status.Error(codes.InvalidArgument, "a production instance cannot be changed to development")
			}
			dst.Type = t
		case "labels":
			dst.Labels = src.GetLabels()
		case "edition":
			if e := src.GetEdition(); e != btapb.Instance_EDITION_UNSPECIFIED {
				dst.Edition = e
			}
		default:
			return status.Errorf(codes.InvalidArgument, "update_mask path %q is not an updatable Instance field", p)
		}
	}
	return nil
}

func (s *instances) updateInstance(_ context.Context, req *btapb.Instance) (proto.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	in, err := s.lookup(req.GetName())
	if err != nil {
		return nil, err
	}
	next := proto.Clone(in.pb).(*btapb.Instance)
	if err := applyInstance(next, req, nil); err != nil {
		return nil, err
	}
	in.pb = next
	return proto.Clone(next), nil
}

func (s *instances) partialUpdateInstance(_ context.Context, req *btapb.PartialUpdateInstanceRequest) (proto.Message, error) {
	paths := req.GetUpdateMask().GetPaths()
	if len(paths) == 0 {
		return nil, status.Error(codes.InvalidArgument, "update_mask is required")
	}
	start := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	in, err := s.lookup(req.GetInstance().GetName())
	if err != nil {
		return nil, err
	}
	next := proto.Clone(in.pb).(*btapb.Instance)
	if err := applyInstance(next, req.GetInstance(), paths); err != nil {
		return nil, err
	}
	in.pb = next
	return s.done(next.Name, &btapb.UpdateInstanceMetadata{OriginalRequest: req, RequestTime: timestamppb.New(start),
		FinishTime: timestamppb.New(s.now())}, proto.Clone(next))
}

func (s *instances) deleteInstance(ctx context.Context, req *btapb.DeleteInstanceRequest) (proto.Message, error) {
	s.mu.Lock()
	if _, err := s.lookup(req.GetName()); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	delete(s.byNm, req.GetName())
	s.mu.Unlock()
	if s.dropTables != nil {
		if err := s.dropTables(ctx, req.GetName()); err != nil {
			return nil, status.Errorf(codes.Internal, "deleting the tables of %s: %v", req.GetName(), err)
		}
	}
	return &emptypb.Empty{}, nil
}

// --- clusters ---

func (s *instances) createCluster(_ context.Context, req *btapb.CreateClusterRequest) (proto.Message, error) {
	start := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	in, err := s.lookup(req.GetParent())
	if err != nil {
		return nil, err
	}
	c, err := newCluster(in.pb.Name, req.GetClusterId(), req.GetCluster(), in.pb.Type)
	if err != nil {
		return nil, err
	}
	if _, ok := in.clusters[req.GetClusterId()]; ok {
		return nil, status.Errorf(codes.AlreadyExists, "Cluster %s already exists.", c.Name)
	}
	for _, o := range in.clusters {
		if o.Location == c.Location {
			return nil, status.Errorf(codes.InvalidArgument, "instance %s already has a cluster in %s", in.pb.Name, c.Location)
		}
	}
	in.clusters[req.GetClusterId()] = c
	return s.done(c.Name, &btapb.CreateClusterMetadata{OriginalRequest: req, RequestTime: timestamppb.New(start),
		FinishTime: timestamppb.New(s.now())}, proto.Clone(c))
}

// cluster returns the cluster name names. s.mu is held.
func (s *instances) cluster(name string) (*instance, *btapb.Cluster, error) {
	m := clusterRe.FindStringSubmatch(name)
	if m == nil {
		return nil, nil, status.Errorf(codes.InvalidArgument, "invalid cluster name %q: want projects/{project}/instances/{instance}/clusters/{cluster}", name)
	}
	in, err := s.lookup("projects/" + m[1] + "/instances/" + m[2])
	if err != nil {
		return nil, nil, err
	}
	c, ok := in.clusters[m[3]]
	if !ok {
		return nil, nil, notFound("Cluster", name)
	}
	return in, c, nil
}

func (s *instances) getCluster(_ context.Context, req *btapb.GetClusterRequest) (proto.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, c, err := s.cluster(req.GetName())
	if err != nil {
		return nil, err
	}
	return proto.Clone(c), nil
}

// parents are the instances a parent names: one, or with "-" every instance
// of the project. s.mu is held.
func (s *instances) parents(parent string) ([]*instance, error) {
	m := instanceRe.FindStringSubmatch(parent)
	if m == nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid parent %q: want projects/{project}/instances/{instance}", parent)
	}
	if m[2] != "-" {
		in, err := s.lookup(parent)
		if err != nil {
			return nil, err
		}
		return []*instance{in}, nil
	}
	var out []*instance
	for _, n := range s.sortedNames("projects/" + m[1] + "/instances/") {
		out = append(out, s.byNm[n])
	}
	return out, nil
}

func (s *instances) listClusters(_ context.Context, req *btapb.ListClustersRequest) (proto.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ins, err := s.parents(req.GetParent())
	if err != nil {
		return nil, err
	}
	out := &btapb.ListClustersResponse{}
	for _, in := range ins {
		for _, id := range sortedKeys(in.clusters) {
			out.Clusters = append(out.Clusters, proto.Clone(in.clusters[id]).(*btapb.Cluster))
		}
	}
	return out, nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// UpdateCluster changes serve_nodes alone, as Google's does.
func (s *instances) updateCluster(_ context.Context, req *btapb.Cluster) (proto.Message, error) {
	start := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	in, c, err := s.cluster(req.GetName())
	if err != nil {
		return nil, err
	}
	next := proto.Clone(c).(*btapb.Cluster)
	if c.GetClusterConfig().GetClusterAutoscalingConfig() != nil {
		return nil, status.Error(codes.FailedPrecondition, "the cluster is autoscaled: use PartialUpdateCluster to change its autoscaling config or turn it off")
	}
	next.ServeNodes = req.GetServeNodes()
	if err := checkNodes(next, in.pb.Type); err != nil {
		return nil, err
	}
	in.clusters[lastSegment(next.Name)] = next
	return s.done(next.Name, &btapb.UpdateClusterMetadata{OriginalRequest: req, RequestTime: timestamppb.New(start),
		FinishTime: timestamppb.New(s.now())}, proto.Clone(next))
}

func (s *instances) partialUpdateCluster(_ context.Context, req *btapb.PartialUpdateClusterRequest) (proto.Message, error) {
	paths := req.GetUpdateMask().GetPaths()
	if len(paths) == 0 {
		return nil, status.Error(codes.InvalidArgument, "update_mask is required")
	}
	start := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	in, c, err := s.cluster(req.GetCluster().GetName())
	if err != nil {
		return nil, err
	}
	next := proto.Clone(c).(*btapb.Cluster)
	src := req.GetCluster()
	for _, p := range paths {
		switch p {
		case "serve_nodes", "serveNodes":
			next.ServeNodes = src.GetServeNodes()
		case "cluster_config.cluster_autoscaling_config", "clusterConfig.clusterAutoscalingConfig":
			if src.GetClusterConfig().GetClusterAutoscalingConfig() == nil {
				next.Config = nil
			} else {
				next.Config = proto.Clone(src).(*btapb.Cluster).Config
			}
		case "cluster_config.cluster_autoscaling_config.autoscaling_limits.min_serve_nodes",
			"cluster_config.cluster_autoscaling_config.autoscaling_limits.max_serve_nodes",
			"cluster_config.cluster_autoscaling_config.autoscaling_targets.cpu_utilization_percent",
			"cluster_config.cluster_autoscaling_config.autoscaling_targets.storage_utilization_gib_per_node":
			if next.GetClusterConfig().GetClusterAutoscalingConfig() == nil {
				return nil, status.Errorf(codes.InvalidArgument, "update_mask path %q: the cluster is not autoscaled", p)
			}
			a, sa := next.GetClusterConfig().GetClusterAutoscalingConfig(), src.GetClusterConfig().GetClusterAutoscalingConfig()
			switch lastSegmentDot(p) {
			case "min_serve_nodes":
				a.AutoscalingLimits.MinServeNodes = sa.GetAutoscalingLimits().GetMinServeNodes()
			case "max_serve_nodes":
				a.AutoscalingLimits.MaxServeNodes = sa.GetAutoscalingLimits().GetMaxServeNodes()
			case "cpu_utilization_percent":
				if a.AutoscalingTargets == nil {
					a.AutoscalingTargets = &btapb.AutoscalingTargets{}
				}
				a.AutoscalingTargets.CpuUtilizationPercent = sa.GetAutoscalingTargets().GetCpuUtilizationPercent()
			case "storage_utilization_gib_per_node":
				if a.AutoscalingTargets == nil {
					a.AutoscalingTargets = &btapb.AutoscalingTargets{}
				}
				a.AutoscalingTargets.StorageUtilizationGibPerNode = sa.GetAutoscalingTargets().GetStorageUtilizationGibPerNode()
			}
		default:
			return nil, status.Errorf(codes.InvalidArgument, "update_mask path %q is not an updatable Cluster field", p)
		}
	}
	if err := checkNodes(next, in.pb.Type); err != nil {
		return nil, err
	}
	in.clusters[lastSegment(next.Name)] = next
	return s.done(next.Name, &btapb.PartialUpdateClusterMetadata{OriginalRequest: req, RequestTime: timestamppb.New(start),
		FinishTime: timestamppb.New(s.now())}, proto.Clone(next))
}

func (s *instances) deleteCluster(_ context.Context, req *btapb.DeleteClusterRequest) (proto.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	in, _, err := s.cluster(req.GetName())
	if err != nil {
		return nil, err
	}
	if len(in.clusters) == 1 {
		return nil, status.Errorf(codes.FailedPrecondition, "cluster %s is the last cluster of instance %s: delete the instance instead", req.GetName(), in.pb.Name)
	}
	id := lastSegment(req.GetName())
	for _, p := range in.profiles {
		if p.GetSingleClusterRouting().GetClusterId() == id {
			return nil, status.Errorf(codes.FailedPrecondition, "app profile %s routes to cluster %s: change or delete it first", p.Name, id)
		}
	}
	delete(in.clusters, id)
	return &emptypb.Empty{}, nil
}

func lastSegment(name string) string { return name[strings.LastIndexByte(name, '/')+1:] }

func lastSegmentDot(p string) string { return p[strings.LastIndexByte(p, '.')+1:] }

// --- app profiles ---

func newEtag() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// checkRouting refuses an app profile without a routing policy, or routing
// to a cluster the instance does not have.
func checkRouting(in *instance, p *btapb.AppProfile) error {
	switch r := p.GetRoutingPolicy().(type) {
	case nil:
		return status.Error(codes.InvalidArgument, "app_profile: a routing policy (multi_cluster_routing_use_any or single_cluster_routing) is required")
	case *btapb.AppProfile_SingleClusterRouting_:
		if _, ok := in.clusters[r.SingleClusterRouting.GetClusterId()]; !ok {
			return status.Errorf(codes.InvalidArgument, "single_cluster_routing.cluster_id %q is not a cluster of %s", r.SingleClusterRouting.GetClusterId(), in.pb.Name)
		}
	case *btapb.AppProfile_MultiClusterRoutingUseAny_:
		for _, id := range r.MultiClusterRoutingUseAny.GetClusterIds() {
			if _, ok := in.clusters[id]; !ok {
				return status.Errorf(codes.InvalidArgument, "multi_cluster_routing_use_any.cluster_ids: %q is not a cluster of %s", id, in.pb.Name)
			}
		}
	}
	return nil
}

func (s *instances) createAppProfile(_ context.Context, req *btapb.CreateAppProfileRequest) (proto.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	in, err := s.lookup(req.GetParent())
	if err != nil {
		return nil, err
	}
	id := req.GetAppProfileId()
	if !appProfileRe2.MatchString(id) || len(id) > 50 {
		return nil, status.Errorf(codes.InvalidArgument, "app_profile_id %q must be 1 to 50 letters, digits, '-', '_' and '.', not starting with '-' or '.'", id)
	}
	if req.GetAppProfile() == nil {
		return nil, status.Error(codes.InvalidArgument, "app_profile is required")
	}
	if _, ok := in.profiles[id]; ok {
		return nil, status.Errorf(codes.AlreadyExists, "AppProfile %s/appProfiles/%s already exists.", in.pb.Name, id)
	}
	p := proto.Clone(req.GetAppProfile()).(*btapb.AppProfile)
	if err := checkRouting(in, p); err != nil {
		return nil, err
	}
	p.Name = in.pb.Name + "/appProfiles/" + id
	p.Etag = newEtag()
	in.profiles[id] = p
	return proto.Clone(p), nil
}

// profile returns the app profile name names. s.mu is held.
func (s *instances) profile(name string) (*instance, *btapb.AppProfile, error) {
	m := appProfileRe.FindStringSubmatch(name)
	if m == nil {
		return nil, nil, status.Errorf(codes.InvalidArgument, "invalid app profile name %q: want projects/{project}/instances/{instance}/appProfiles/{app_profile}", name)
	}
	in, err := s.lookup("projects/" + m[1] + "/instances/" + m[2])
	if err != nil {
		return nil, nil, err
	}
	p, ok := in.profiles[m[3]]
	if !ok {
		return nil, nil, notFound("AppProfile", name)
	}
	return in, p, nil
}

func (s *instances) getAppProfile(_ context.Context, req *btapb.GetAppProfileRequest) (proto.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, p, err := s.profile(req.GetName())
	if err != nil {
		return nil, err
	}
	return proto.Clone(p), nil
}

func (s *instances) listAppProfiles(_ context.Context, req *btapb.ListAppProfilesRequest) (proto.Message, error) {
	if req.GetPageSize() < 0 {
		return nil, status.Error(codes.InvalidArgument, "page_size cannot be negative")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ins, err := s.parents(req.GetParent())
	if err != nil {
		return nil, err
	}
	var all []*btapb.AppProfile
	for _, in := range ins {
		for _, id := range sortedKeys(in.profiles) {
			all = append(all, in.profiles[id])
		}
	}
	start := 0
	if t := req.GetPageToken(); t != "" {
		n, err := strconv.Atoi(t)
		if err != nil || n < 0 || n > len(all) {
			return nil, status.Errorf(codes.InvalidArgument, "invalid page_token %q", t)
		}
		start = n
	}
	end := len(all)
	if ps := int(req.GetPageSize()); ps > 0 && start+ps < end {
		end = start + ps
	}
	out := &btapb.ListAppProfilesResponse{}
	for _, p := range all[start:end] {
		out.AppProfiles = append(out.AppProfiles, proto.Clone(p).(*btapb.AppProfile))
	}
	if end < len(all) {
		out.NextPageToken = strconv.Itoa(end)
	}
	return out, nil
}

func (s *instances) updateAppProfile(_ context.Context, req *btapb.UpdateAppProfileRequest) (proto.Message, error) {
	paths := req.GetUpdateMask().GetPaths()
	if len(paths) == 0 {
		return nil, status.Error(codes.InvalidArgument, "update_mask is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	in, p, err := s.profile(req.GetAppProfile().GetName())
	if err != nil {
		return nil, err
	}
	src := req.GetAppProfile()
	if e := src.GetEtag(); e != "" && e != p.Etag {
		return nil, status.Error(codes.Aborted, "the app profile's etag does not match: read it again and retry")
	}
	next := proto.Clone(p).(*btapb.AppProfile)
	for _, path := range paths {
		switch path {
		case "description":
			next.Description = src.GetDescription()
		case "multi_cluster_routing_use_any", "multiClusterRoutingUseAny":
			if r := src.GetMultiClusterRoutingUseAny(); r != nil {
				next.RoutingPolicy = &btapb.AppProfile_MultiClusterRoutingUseAny_{MultiClusterRoutingUseAny: proto.Clone(r).(*btapb.AppProfile_MultiClusterRoutingUseAny)}
			}
		case "single_cluster_routing", "singleClusterRouting":
			if r := src.GetSingleClusterRouting(); r != nil {
				next.RoutingPolicy = &btapb.AppProfile_SingleClusterRouting_{SingleClusterRouting: proto.Clone(r).(*btapb.AppProfile_SingleClusterRouting)}
			}
		case "standard_isolation", "standardIsolation", "data_boost_isolation_read_only", "dataBoostIsolationReadOnly", "priority":
			next.Isolation = proto.Clone(src).(*btapb.AppProfile).Isolation
		default:
			return nil, status.Errorf(codes.InvalidArgument, "update_mask path %q is not an updatable AppProfile field", path)
		}
	}
	if err := checkRouting(in, next); err != nil {
		return nil, err
	}
	next.Etag = newEtag()
	in.profiles[lastSegment(next.Name)] = next
	return s.done(next.Name, &btapb.UpdateAppProfileMetadata{}, proto.Clone(next))
}

func (s *instances) deleteAppProfile(_ context.Context, req *btapb.DeleteAppProfileRequest) (proto.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	in, _, err := s.profile(req.GetName())
	if err != nil {
		return nil, err
	}
	delete(in.profiles, lastSegment(req.GetName()))
	return &emptypb.Empty{}, nil
}

// --- IAM: stored, not enforced (ADR-0006) ---

func (s *instances) getIamPolicy(_ context.Context, req *iampb.GetIamPolicyRequest) (proto.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	in, err := s.lookup(req.GetResource())
	if err != nil {
		return nil, err
	}
	return iampolicy.Get(in.policy, req.GetOptions())
}

func (s *instances) setIamPolicy(_ context.Context, req *iampb.SetIamPolicyRequest) (proto.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	in, err := s.lookup(req.GetResource())
	if err != nil {
		return nil, err
	}
	p, err := iampolicy.Set(in.policy, req)
	if err != nil {
		return nil, err
	}
	in.policy = &p
	return p.Proto(), nil
}

func (s *instances) testIamPermissions(_ context.Context, req *iampb.TestIamPermissionsRequest) (proto.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.lookup(req.GetResource()); err != nil {
		return nil, err
	}
	return iampolicy.Permissions(req), nil
}

// --- operations ---

func (s *instances) getOperation(_ context.Context, req *longrunningpb.GetOperationRequest) (proto.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	op, ok := s.ops[req.GetName()]
	if !ok {
		return nil, notFound("Operation", req.GetName())
	}
	return proto.Clone(op), nil
}

func (s *instances) waitOperation(ctx context.Context, req *longrunningpb.WaitOperationRequest) (proto.Message, error) {
	return s.getOperation(ctx, &longrunningpb.GetOperationRequest{Name: req.GetName()})
}

// ListOperations lists the operations of the resource "operations/{name}"
// names, newest first.
func (s *instances) listOperations(_ context.Context, req *longrunningpb.ListOperationsRequest) (proto.Message, error) {
	prefix := strings.TrimSuffix(req.GetName(), "/operations")
	if !strings.HasPrefix(prefix, "operations/projects/") {
		return nil, status.Errorf(codes.InvalidArgument, "invalid name %q: want operations/projects/{project}...", req.GetName())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := &longrunningpb.ListOperationsResponse{}
	for i := len(s.opsQ) - 1; i >= 0; i-- {
		if n := s.opsQ[i]; strings.HasPrefix(n, prefix+"/") {
			out.Operations = append(out.Operations, proto.Clone(s.ops[n]).(*longrunningpb.Operation))
		}
	}
	return out, nil
}

func (s *instances) deleteOperation(_ context.Context, req *longrunningpb.DeleteOperationRequest) (proto.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.ops[req.GetName()]; !ok {
		return nil, notFound("Operation", req.GetName())
	}
	delete(s.ops, req.GetName())
	return &emptypb.Empty{}, nil
}

// CancelOperation of a finished operation does nothing, as Google's does.
func (s *instances) cancelOperation(ctx context.Context, req *longrunningpb.CancelOperationRequest) (proto.Message, error) {
	if _, err := s.getOperation(ctx, &longrunningpb.GetOperationRequest{Name: req.GetName()}); err != nil {
		return nil, err
	}
	return &emptypb.Empty{}, nil
}
