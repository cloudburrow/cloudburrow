// Package spannerfront answers the Spanner instance updates the Spanner
// emulator refuses (#1461), in the call front beside it (internal/callfront).
//
// The emulator keeps its instances, but answers UpdateInstance
// UNIMPLEMENTED, so the Terraform provider cannot change a
// google_spanner_instance and gcloud cannot run `spanner instances update`.
// The front serves UpdateInstance itself, over gRPC and over the admin REST
// API, for the fields an emulator instance can carry faithfully: its display
// name, its compute capacity (node_count or processing_units) and its
// labels. The emulator's capacity was never more than a number on the
// instance: it serves every instance the same. The front keeps those
// changes, in memory as the emulator keeps its instances, and lays them over
// the emulator's own answer to GetInstance and ListInstances; DeleteInstance
// passes to the emulator and drops them.
//
// What an emulator instance cannot carry is refused UNIMPLEMENTED, naming
// why: autoscaling (the emulator does not scale), a move to another
// configuration (MoveInstance), the edition and the default backup schedule
// type (the emulator has no editions and takes no backups).
package spannerfront

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"cloud.google.com/go/spanner/admin/instance/apiv1/instancepb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const instanceService = "/google.spanner.admin.instance.v1.InstanceAdmin/"

// claimed are the InstanceAdmin methods the front answers.
var claimed = map[string]bool{"GetInstance": true, "ListInstances": true, "UpdateInstance": true, "DeleteInstance": true}

var instanceNameRe = regexp.MustCompile(`^projects/[^/]+/instances/[^/]+$`)

// Instances answers the instance calls Claims claims, over gRPC and REST.
type Instances struct {
	instancepb.UnimplementedInstanceAdminServer

	emu  instancepb.InstanceAdminClient
	grpc *grpc.Server
	now  func() time.Time

	mu sync.Mutex
	// changes are the fields an update set, per instance name: the
	// instance carries the values, and paths the fields they are.
	changes map[string]*change
}

type change struct {
	inst  *instancepb.Instance
	paths map[string]bool
	at    time.Time
}

// New returns the front for the emulator whose gRPC port is at emulator.
func New(emulator string) (*Instances, error) {
	conn, err := grpc.NewClient(emulator, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, err
	}
	return NewWithClient(instancepb.NewInstanceAdminClient(conn)), nil
}

// NewWithClient is New over an InstanceAdmin client of the emulator.
func NewWithClient(emu instancepb.InstanceAdminClient) *Instances {
	s := &Instances{emu: emu, now: func() time.Time { return time.Now().UTC() }, changes: map[string]*change{}}
	s.grpc = grpc.NewServer()
	instancepb.RegisterInstanceAdminServer(s.grpc, s)
	return s
}

// Claims reports whether r is one of the instance calls the front answers.
func (s *Instances) Claims(r *http.Request) bool {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") {
		m, ok := strings.CutPrefix(r.URL.Path, instanceService)
		return ok && claimed[m]
	}
	_, ok := restCall(r)
	return ok
}

// ServeHTTP answers a call Claims claimed.
func (s *Instances) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") {
		s.grpc.ServeHTTP(w, r)
		return
	}
	s.serveREST(w, r)
}

// overlay lays the changes kept for inst over it. s.mu is held.
func (s *Instances) overlay(inst *instancepb.Instance) {
	c, ok := s.changes[inst.GetName()]
	if !ok {
		return
	}
	apply(inst, c.inst, c.paths)
	inst.UpdateTime = timestamppb.New(c.at)
}

func apply(dst, src *instancepb.Instance, paths map[string]bool) {
	if paths["display_name"] {
		dst.DisplayName = src.DisplayName
	}
	if paths["node_count"] || paths["processing_units"] {
		dst.NodeCount, dst.ProcessingUnits = src.NodeCount, src.ProcessingUnits
	}
	if paths["labels"] {
		dst.Labels = src.Labels
	}
}

func (s *Instances) GetInstance(ctx context.Context, req *instancepb.GetInstanceRequest) (*instancepb.Instance, error) {
	inst, err := s.emu.GetInstance(ctx, req)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.overlay(inst)
	return inst, nil
}

func (s *Instances) ListInstances(ctx context.Context, req *instancepb.ListInstancesRequest) (*instancepb.ListInstancesResponse, error) {
	resp, err := s.emu.ListInstances(ctx, req)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, inst := range resp.Instances {
		s.overlay(inst)
	}
	return resp, nil
}

func (s *Instances) DeleteInstance(ctx context.Context, req *instancepb.DeleteInstanceRequest) (*emptypb.Empty, error) {
	resp, err := s.emu.DeleteInstance(ctx, req)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.changes, req.GetName())
	return resp, nil
}

// refused are the update paths an emulator instance cannot carry.
var refused = map[string]string{
	"config":                       "a move to another instance configuration is MoveInstance, which the emulator does not serve",
	"autoscaling_config":           "the emulator does not autoscale: set node_count or processing_units",
	"edition":                      "the emulator has no editions",
	"default_backup_schedule_type": "the emulator takes no backups",
	"instance_type":                "an instance's type cannot change",
}

// UpdateInstance changes the display name, compute capacity and labels of
// an instance the emulator keeps, as Google does, and returns the done
// operation.
func (s *Instances) UpdateInstance(ctx context.Context, req *instancepb.UpdateInstanceRequest) (*longrunningpb.Operation, error) {
	in := req.GetInstance()
	if in == nil || !instanceNameRe.MatchString(in.GetName()) {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid UpdateInstance request: instance.name %q is not projects/{project}/instances/{instance}.", in.GetName())
	}
	paths := map[string]bool{}
	named := len(req.GetFieldMask().GetPaths()) > 0
	for _, p := range req.GetFieldMask().GetPaths() {
		p = snake(p)
		if why, ok := refused[p]; ok {
			// Clearing what the emulator never has is no change: gcloud
			// clears autoscaling_config when it sets a node count.
			if unset(in, p) {
				continue
			}
			return nil, status.Errorf(codes.Unimplemented, "UpdateInstance of %s is not served here: %s", p, why)
		}
		switch p {
		case "display_name", "node_count", "processing_units", "labels":
			paths[p] = true
		default:
			return nil, status.Errorf(codes.InvalidArgument, "Invalid UpdateInstance request: field_mask path %q is not a field of Instance that can be updated.", p)
		}
	}
	if !named {
		return nil, status.Error(codes.InvalidArgument, "Invalid UpdateInstance request: field_mask must name at least one field.")
	}
	if paths["display_name"] {
		if n := utf8.RuneCountInString(in.GetDisplayName()); n < 4 || n > 30 {
			return nil, status.Error(codes.InvalidArgument, "Invalid UpdateInstance request: display_name must be 4 to 30 characters long.")
		}
	}
	want := proto.Clone(in).(*instancepb.Instance)
	switch {
	case paths["node_count"] && paths["processing_units"]:
		if in.GetProcessingUnits() != in.GetNodeCount()*1000 {
			return nil, status.Error(codes.InvalidArgument, "Invalid UpdateInstance request: node_count and processing_units are both set and do not agree: a node is 1000 processing units.")
		}
	case paths["node_count"]:
		want.ProcessingUnits = in.GetNodeCount() * 1000
	case paths["processing_units"]:
		want.NodeCount = in.GetProcessingUnits() / 1000
	}
	if paths["node_count"] && in.GetNodeCount() <= 0 && !paths["processing_units"] {
		return nil, status.Error(codes.InvalidArgument, "Invalid UpdateInstance request: node_count must be positive.")
	}
	if paths["node_count"] || paths["processing_units"] {
		pu := want.GetProcessingUnits()
		if pu <= 0 || (pu < 1000 && pu%100 != 0) || (pu >= 1000 && pu%1000 != 0) {
			return nil, status.Error(codes.InvalidArgument, "Invalid UpdateInstance request: processing_units must be a positive multiple of 100 below 1000, and a multiple of 1000 from 1000.")
		}
	}

	cur, err := s.emu.GetInstance(ctx, &instancepb.GetInstanceRequest{Name: in.GetName()})
	if err != nil {
		return nil, err
	}
	start := s.now()
	s.mu.Lock()
	c, ok := s.changes[in.GetName()]
	if !ok {
		c = &change{inst: &instancepb.Instance{}, paths: map[string]bool{}}
		s.changes[in.GetName()] = c
	}
	apply(c.inst, want, paths)
	for p := range paths {
		c.paths[p] = true
	}
	c.at = s.now()
	s.overlay(cur)
	s.mu.Unlock()

	meta, err := anypb.New(&instancepb.UpdateInstanceMetadata{Instance: cur, StartTime: timestamppb.New(start), EndTime: timestamppb.New(s.now())})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "operation metadata: %v", err)
	}
	resp, err := anypb.New(cur)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "operation response: %v", err)
	}
	return &longrunningpb.Operation{Name: in.GetName() + "/operations/" + opID(), Metadata: meta, Done: true,
		Result: &longrunningpb.Operation_Response{Response: resp}}, nil
}

// unset reports whether in leaves the refused field p at its zero value,
// which is what an emulator instance has.
func unset(in *instancepb.Instance, p string) bool {
	switch p {
	case "autoscaling_config":
		return in.GetAutoscalingConfig() == nil
	case "edition":
		return in.GetEdition() == instancepb.Instance_EDITION_UNSPECIFIED
	case "default_backup_schedule_type":
		return in.GetDefaultBackupScheduleType() == instancepb.Instance_DEFAULT_BACKUP_SCHEDULE_TYPE_UNSPECIFIED
	}
	return false
}

func opID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "_auto_" + strings.ToLower(base64.RawURLEncoding.EncodeToString(b))
}

// snake turns a REST mask path (camelCase) into the proto field name.
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
