package spannerfront

import (
	"context"
	"regexp"
	"strings"
	"unicode/utf8"

	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"cloud.google.com/go/spanner/admin/instance/apiv1/instancepb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/locations"
)

// Instance configurations, instance moves and partition updates (#1498).
//
// The emulator has one configuration, emulator-config, and creates an
// instance in any configuration named. The front lists and gets Google's
// managed configurations beside it: one regional configuration a region
// (regional-us-central1, ...) and the multi-region nam3 and eur3, with
// the replicas Google documents. It keeps the custom (user-managed)
// configurations made from them, as Google documents them: the id starts
// custom-, the base is a Google-managed configuration, and the replicas
// are the base's with read-only replicas added. A configuration is never
// more than a name to the emulator, which serves every instance the same,
// so MoveInstance changes an instance's configuration as the front lays
// over GetInstance, and the move is done at once.

var configNameRe = regexp.MustCompile(`^(projects/[^/]+)/instanceConfigs/([^/]+)$`)

const emulatorConfig = "emulator-config"

// multiRegions are the multi-region configurations Google documents, with
// their replicas: read-write in two regions, a witness in a third.
var multiRegions = map[string]struct {
	name     string
	rw       []string
	witness  string
	leader   string
	optional []string
}{
	"nam3": {"North America (Northern Virginia/South Carolina)", []string{"us-east4", "us-east1"}, "us-central1", "us-east4", nil},
	"eur3": {"Europe (Belgium/Netherlands)", []string{"europe-west1", "europe-west4"}, "europe-north1", "europe-west1", nil},
}

// managedConfig is the Google-managed configuration id of project, or nil.
func managedConfig(project, id string) *instancepb.InstanceConfig {
	name := project + "/instanceConfigs/" + id
	if region, ok := strings.CutPrefix(id, "regional-"); ok {
		for _, r := range locations.Regions {
			if r != region {
				continue
			}
			c := &instancepb.InstanceConfig{Name: name, DisplayName: region, ConfigType: instancepb.InstanceConfig_GOOGLE_MANAGED,
				LeaderOptions: []string{region}, State: instancepb.InstanceConfig_READY,
				FreeInstanceAvailability: instancepb.InstanceConfig_AVAILABLE, QuorumType: instancepb.InstanceConfig_REGION}
			for i := 0; i < 3; i++ {
				c.Replicas = append(c.Replicas, &instancepb.ReplicaInfo{Location: region, Type: instancepb.ReplicaInfo_READ_WRITE, DefaultLeaderLocation: i == 0})
			}
			for _, other := range locations.Regions {
				if other != region {
					c.OptionalReplicas = append(c.OptionalReplicas, &instancepb.ReplicaInfo{Location: other, Type: instancepb.ReplicaInfo_READ_ONLY})
				}
			}
			return c
		}
		return nil
	}
	m, ok := multiRegions[id]
	if !ok {
		return nil
	}
	c := &instancepb.InstanceConfig{Name: name, DisplayName: m.name, ConfigType: instancepb.InstanceConfig_GOOGLE_MANAGED,
		LeaderOptions: m.rw, State: instancepb.InstanceConfig_READY,
		FreeInstanceAvailability: instancepb.InstanceConfig_UNSUPPORTED, QuorumType: instancepb.InstanceConfig_MULTI_REGION}
	for _, r := range m.rw {
		for i := 0; i < 2; i++ {
			c.Replicas = append(c.Replicas, &instancepb.ReplicaInfo{Location: r, Type: instancepb.ReplicaInfo_READ_WRITE, DefaultLeaderLocation: r == m.leader && i == 0})
		}
	}
	c.Replicas = append(c.Replicas, &instancepb.ReplicaInfo{Location: m.witness, Type: instancepb.ReplicaInfo_WITNESS})
	for _, r := range locations.Regions {
		c.OptionalReplicas = append(c.OptionalReplicas, &instancepb.ReplicaInfo{Location: r, Type: instancepb.ReplicaInfo_READ_ONLY})
	}
	return c
}

func managedConfigs(project string) []*instancepb.InstanceConfig {
	var out []*instancepb.InstanceConfig
	for _, r := range locations.Regions {
		out = append(out, managedConfig(project, "regional-"+r))
	}
	for id := range multiRegions {
		out = append(out, managedConfig(project, id))
	}
	return out
}

func (s *Instances) GetInstanceConfig(ctx context.Context, req *instancepb.GetInstanceConfigRequest) (*instancepb.InstanceConfig, error) {
	m := configNameRe.FindStringSubmatch(req.GetName())
	if m == nil {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid GetInstanceConfig request: name %q is not projects/{project}/instanceConfigs/{config}.", req.GetName())
	}
	if c := managedConfig(m[1], m[2]); c != nil {
		return c, nil
	}
	s.mu.Lock()
	c, ok := s.configs[req.GetName()]
	s.mu.Unlock()
	if ok {
		return proto.Clone(c).(*instancepb.InstanceConfig), nil
	}
	return s.emu.GetInstanceConfig(ctx, req)
}

func (s *Instances) ListInstanceConfigs(ctx context.Context, req *instancepb.ListInstanceConfigsRequest) (*instancepb.ListInstanceConfigsResponse, error) {
	byName := map[string]*instancepb.InstanceConfig{}
	emu, err := s.emu.ListInstanceConfigs(ctx, &instancepb.ListInstanceConfigsRequest{Parent: req.GetParent()})
	if err != nil {
		return nil, err
	}
	for _, c := range emu.GetInstanceConfigs() {
		byName[c.GetName()] = c
	}
	for _, c := range managedConfigs(req.GetParent()) {
		byName[c.GetName()] = c
	}
	s.mu.Lock()
	for name, c := range s.configs {
		if strings.HasPrefix(name, req.GetParent()+"/") {
			byName[name] = proto.Clone(c).(*instancepb.InstanceConfig)
		}
	}
	s.mu.Unlock()
	page, next, err := pageOf("configs:"+req.GetParent(), byName, req.GetPageToken(), req.GetPageSize())
	if err != nil {
		return nil, err
	}
	resp := &instancepb.ListInstanceConfigsResponse{NextPageToken: next}
	for _, n := range page {
		resp.InstanceConfigs = append(resp.InstanceConfigs, byName[n])
	}
	return resp, nil
}

var customIDRe = regexp.MustCompile(`^custom-[a-z0-9-]{1,57}$`)

func replicaKey(r *instancepb.ReplicaInfo) string {
	return r.GetLocation() + "/" + r.GetType().String()
}

func (s *Instances) CreateInstanceConfig(ctx context.Context, req *instancepb.CreateInstanceConfigRequest) (*longrunningpb.Operation, error) {
	const method = "CreateInstanceConfig"
	if !regexp.MustCompile(`^projects/[^/]+$`).MatchString(req.GetParent()) {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid %s request: parent %q is not projects/{project}.", method, req.GetParent())
	}
	if !customIDRe.MatchString(req.GetInstanceConfigId()) {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid %s request: instance_config_id %q must start with custom- and be 8 to 64 lowercase letters, digits and hyphens.", method, req.GetInstanceConfigId())
	}
	in := proto.Clone(req.GetInstanceConfig()).(*instancepb.InstanceConfig)
	if in == nil {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid %s request: instance_config is required.", method)
	}
	if n := utf8.RuneCountInString(in.GetDisplayName()); n < 1 || n > 30 {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid %s request: display_name must be 1 to 30 characters long.", method)
	}
	bm := configNameRe.FindStringSubmatch(in.GetBaseConfig())
	if bm == nil || bm[1] != req.GetParent() {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid %s request: base_config %q must be a Google-managed configuration of %s.", method, in.GetBaseConfig(), req.GetParent())
	}
	base := managedConfig(bm[1], bm[2])
	if base == nil {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid %s request: base_config %q is not a Google-managed configuration.", method, in.GetBaseConfig())
	}
	// The replicas are the base's, with read-only replicas added.
	need := map[string]int{}
	for _, r := range base.GetReplicas() {
		need[replicaKey(r)]++
	}
	for _, r := range in.GetReplicas() {
		if need[replicaKey(r)] > 0 {
			need[replicaKey(r)]--
			continue
		}
		if r.GetType() != instancepb.ReplicaInfo_READ_ONLY {
			return nil, status.Errorf(codes.InvalidArgument, "Invalid %s request: replica %s %s is not one of the base configuration's: an added replica must be READ_ONLY.", method, r.GetLocation(), r.GetType())
		}
		known := false
		for _, o := range base.GetOptionalReplicas() {
			known = known || o.GetLocation() == r.GetLocation()
		}
		if !known {
			return nil, status.Errorf(codes.InvalidArgument, "Invalid %s request: %s is not an optional replica location of %s.", method, r.GetLocation(), in.GetBaseConfig())
		}
	}
	for k, n := range need {
		if n > 0 {
			return nil, status.Errorf(codes.InvalidArgument, "Invalid %s request: replicas must include every replica of the base configuration; %s is missing.", method, k)
		}
	}
	name := req.GetParent() + "/instanceConfigs/" + req.GetInstanceConfigId()
	in.Name, in.ConfigType, in.State, in.Reconciling = name, instancepb.InstanceConfig_USER_MANAGED, instancepb.InstanceConfig_READY, false
	in.LeaderOptions, in.OptionalReplicas, in.QuorumType = base.GetLeaderOptions(), nil, base.GetQuorumType()
	in.FreeInstanceAvailability = instancepb.InstanceConfig_UNSUPPORTED
	in.Etag = opID()
	start := s.now()
	if !req.GetValidateOnly() {
		s.mu.Lock()
		if _, ok := s.configs[name]; ok {
			s.mu.Unlock()
			return nil, status.Errorf(codes.AlreadyExists, "Instance config already exists: %s", name)
		}
		s.configs[name] = proto.Clone(in).(*instancepb.InstanceConfig)
		s.mu.Unlock()
	}
	meta := &instancepb.CreateInstanceConfigMetadata{InstanceConfig: in,
		Progress: &instancepb.OperationProgress{ProgressPercent: 100, StartTime: timestamppb.New(start), EndTime: timestamppb.New(s.now())}}
	return s.doneOp(name, meta, in)
}

// customConfig is the custom configuration named name, for method, with
// etag checked.
func (s *Instances) customConfig(method, name, etag string) (*instancepb.InstanceConfig, error) {
	m := configNameRe.FindStringSubmatch(name)
	if m == nil {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid %s request: name %q is not projects/{project}/instanceConfigs/{config}.", method, name)
	}
	if managedConfig(m[1], m[2]) != nil || m[2] == emulatorConfig {
		return nil, status.Errorf(codes.FailedPrecondition, "%s: %s is a Google-managed configuration: only a user-managed configuration can be changed.", method, name)
	}
	c, ok := s.configs[name]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "Instance config not found: %s", name)
	}
	if etag != "" && etag != c.GetEtag() {
		return nil, status.Errorf(codes.Aborted, "%s: the etag %q does not match the configuration's: it changed since it was read.", method, etag)
	}
	return c, nil
}

func (s *Instances) UpdateInstanceConfig(ctx context.Context, req *instancepb.UpdateInstanceConfigRequest) (*longrunningpb.Operation, error) {
	const method = "UpdateInstanceConfig"
	in := req.GetInstanceConfig()
	paths := req.GetUpdateMask().GetPaths()
	if len(paths) == 0 {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid %s request: update_mask must name display_name or labels.", method)
	}
	start := s.now()
	s.mu.Lock()
	cur, err := s.customConfig(method, in.GetName(), in.GetEtag())
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	next := proto.Clone(cur).(*instancepb.InstanceConfig)
	for _, p := range paths {
		switch snake(p) {
		case "display_name":
			if n := utf8.RuneCountInString(in.GetDisplayName()); n < 1 || n > 30 {
				s.mu.Unlock()
				return nil, status.Errorf(codes.InvalidArgument, "Invalid %s request: display_name must be 1 to 30 characters long.", method)
			}
			next.DisplayName = in.GetDisplayName()
		case "labels":
			next.Labels = in.GetLabels()
		default:
			s.mu.Unlock()
			return nil, status.Errorf(codes.InvalidArgument, "Invalid %s request: field_mask path %q cannot be updated: display_name and labels can.", method, p)
		}
	}
	next.Etag = opID()
	if !req.GetValidateOnly() {
		s.configs[in.GetName()] = next
	}
	s.mu.Unlock()
	meta := &instancepb.UpdateInstanceConfigMetadata{InstanceConfig: next,
		Progress: &instancepb.OperationProgress{ProgressPercent: 100, StartTime: timestamppb.New(start), EndTime: timestamppb.New(s.now())}}
	return s.doneOp(in.GetName(), meta, next)
}

func (s *Instances) DeleteInstanceConfig(ctx context.Context, req *instancepb.DeleteInstanceConfigRequest) (*emptypb.Empty, error) {
	const method = "DeleteInstanceConfig"
	s.mu.Lock()
	_, err := s.customConfig(method, req.GetName(), req.GetEtag())
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	project := configNameRe.FindStringSubmatch(req.GetName())[1]
	insts, err := s.ListInstances(ctx, &instancepb.ListInstancesRequest{Parent: project})
	if err != nil {
		return nil, err
	}
	for _, i := range insts.GetInstances() {
		if i.GetConfig() == req.GetName() {
			return nil, status.Errorf(codes.FailedPrecondition, "%s: instance %s uses %s: move or delete it first.", method, i.GetName(), req.GetName())
		}
	}
	if !req.GetValidateOnly() {
		s.mu.Lock()
		delete(s.configs, req.GetName())
		s.mu.Unlock()
	}
	return &emptypb.Empty{}, nil
}

func (s *Instances) ListInstanceConfigOperations(ctx context.Context, req *instancepb.ListInstanceConfigOperationsRequest) (*instancepb.ListInstanceConfigOperationsResponse, error) {
	ops, next, err := s.listOps(ctx, "configOps:"+req.GetParent(), opPrefix(req.GetParent(), "instanceConfigs"), req.GetFilter(), req.GetPageToken(), req.GetPageSize())
	if err != nil {
		return nil, err
	}
	return &instancepb.ListInstanceConfigOperationsResponse{Operations: ops, NextPageToken: next}, nil
}

// MoveInstance moves an instance to another configuration: the
// configuration the front lays over GetInstance from then on.
func (s *Instances) MoveInstance(ctx context.Context, req *instancepb.MoveInstanceRequest) (*longrunningpb.Operation, error) {
	if !instanceNameRe.MatchString(req.GetName()) {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid MoveInstance request: name %q is not projects/{project}/instances/{instance}.", req.GetName())
	}
	if _, err := s.GetInstanceConfig(ctx, &instancepb.GetInstanceConfigRequest{Name: req.GetTargetConfig()}); err != nil {
		return nil, err
	}
	cur, err := s.GetInstance(ctx, &instancepb.GetInstanceRequest{Name: req.GetName()})
	if err != nil {
		return nil, err
	}
	if cur.GetConfig() == req.GetTargetConfig() {
		return nil, status.Errorf(codes.FailedPrecondition, "MoveInstance: %s is already in %s.", req.GetName(), req.GetTargetConfig())
	}
	start := s.now()
	s.mu.Lock()
	c, ok := s.changes[req.GetName()]
	if !ok {
		c = &change{inst: &instancepb.Instance{}, paths: map[string]bool{}}
		s.changes[req.GetName()] = c
	}
	c.inst.Config, c.paths["config"], c.at = req.GetTargetConfig(), true, s.now()
	s.mu.Unlock()
	meta := &instancepb.MoveInstanceMetadata{TargetConfig: req.GetTargetConfig(),
		Progress: &instancepb.OperationProgress{ProgressPercent: 100, StartTime: timestamppb.New(start), EndTime: timestamppb.New(s.now())}}
	return s.doneOp(req.GetName(), meta, &instancepb.MoveInstanceResponse{})
}

// partitionChange is what UpdateInstancePartition set on a partition.
type partitionChange struct {
	part  *instancepb.InstancePartition
	paths map[string]bool
}

func (s *Instances) overlayPartition(p *instancepb.InstancePartition) {
	c, ok := s.partitions[p.GetName()]
	if !ok {
		return
	}
	if c.paths["display_name"] {
		p.DisplayName = c.part.GetDisplayName()
	}
	if c.paths["compute_capacity"] {
		p.ComputeCapacity = c.part.ComputeCapacity
	}
	p.UpdateTime = c.part.GetUpdateTime()
}

func (s *Instances) GetInstancePartition(ctx context.Context, req *instancepb.GetInstancePartitionRequest) (*instancepb.InstancePartition, error) {
	p, err := s.emu.GetInstancePartition(ctx, req)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.overlayPartition(p)
	return p, nil
}

func (s *Instances) ListInstancePartitions(ctx context.Context, req *instancepb.ListInstancePartitionsRequest) (*instancepb.ListInstancePartitionsResponse, error) {
	resp, err := s.emu.ListInstancePartitions(ctx, req)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range resp.GetInstancePartitions() {
		s.overlayPartition(p)
	}
	return resp, nil
}

func (s *Instances) DeleteInstancePartition(ctx context.Context, req *instancepb.DeleteInstancePartitionRequest) (*emptypb.Empty, error) {
	resp, err := s.emu.DeleteInstancePartition(ctx, req)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	delete(s.partitions, req.GetName())
	s.mu.Unlock()
	return resp, nil
}

// UpdateInstancePartition changes a partition's display name and compute
// capacity, as UpdateInstance does an instance's.
func (s *Instances) UpdateInstancePartition(ctx context.Context, req *instancepb.UpdateInstancePartitionRequest) (*longrunningpb.Operation, error) {
	const method = "UpdateInstancePartition"
	in := req.GetInstancePartition()
	paths := req.GetFieldMask().GetPaths()
	if len(paths) == 0 {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid %s request: field_mask must name at least one field.", method)
	}
	cur, err := s.GetInstancePartition(ctx, &instancepb.GetInstancePartitionRequest{Name: in.GetName()})
	if err != nil {
		return nil, err
	}
	if e := in.GetEtag(); e != "" && cur.GetEtag() != "" && e != cur.GetEtag() {
		return nil, status.Errorf(codes.Aborted, "%s: the etag %q does not match the partition's: it changed since it was read.", method, e)
	}
	set := map[string]bool{}
	for _, p := range paths {
		switch p = snake(p); p {
		case "display_name":
			if n := utf8.RuneCountInString(in.GetDisplayName()); n < 4 || n > 30 {
				return nil, status.Errorf(codes.InvalidArgument, "Invalid %s request: display_name must be 4 to 30 characters long.", method)
			}
			set[p] = true
		case "node_count", "processing_units":
			set["compute_capacity"] = true
		case "autoscaling_config":
			if in.GetAutoscalingConfig() == nil {
				continue
			}
			return nil, status.Errorf(codes.Unimplemented, "%s of autoscaling_config is not served here: the emulator does not autoscale: set node_count or processing_units", method)
		default:
			return nil, status.Errorf(codes.InvalidArgument, "Invalid %s request: field_mask path %q is not a field of InstancePartition that can be updated.", method, p)
		}
	}
	if set["compute_capacity"] {
		var pu int32
		switch c := in.ComputeCapacity.(type) {
		case *instancepb.InstancePartition_NodeCount:
			pu = c.NodeCount * 1000
		case *instancepb.InstancePartition_ProcessingUnits:
			pu = c.ProcessingUnits
		}
		if pu <= 0 || (pu < 1000 && pu%100 != 0) || (pu >= 1000 && pu%1000 != 0) {
			return nil, status.Errorf(codes.InvalidArgument, "Invalid %s request: processing_units must be a positive multiple of 100 below 1000, and a multiple of 1000 from 1000.", method)
		}
	}
	start := s.now()
	s.mu.Lock()
	c, ok := s.partitions[in.GetName()]
	if !ok {
		c = &partitionChange{part: &instancepb.InstancePartition{}, paths: map[string]bool{}}
		s.partitions[in.GetName()] = c
	}
	if set["display_name"] {
		c.part.DisplayName = in.GetDisplayName()
	}
	if set["compute_capacity"] {
		c.part.ComputeCapacity = in.ComputeCapacity
	}
	for p := range set {
		c.paths[p] = true
	}
	c.part.UpdateTime = timestamppb.New(s.now())
	s.overlayPartition(cur)
	s.mu.Unlock()
	meta := &instancepb.UpdateInstancePartitionMetadata{InstancePartition: cur, StartTime: timestamppb.New(start), EndTime: timestamppb.New(s.now())}
	return s.doneOp(in.GetName(), meta, cur)
}
