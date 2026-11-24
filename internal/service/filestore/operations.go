package filestore

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"regexp"
	"strconv"
	"strings"
	"time"

	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	commonpb "google.golang.org/genproto/googleapis/cloud/common"
	rpcstatus "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/aipfilter"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// Operations are google.longrunning operations named
// projects/P/locations/L/operations/operation-MILLIS-HEX, as
// file.googleapis.com names them, with google.cloud.common's
// OperationMetadata (verb create, update, delete, restore, revert).

var opNameRE = regexp.MustCompile(`^projects/([^/]+)/locations/([^/]+)/operations/([^/]+)$`)

// startOp records a running operation on target in location (the
// resource's parent location name, projects/P/locations/L) and runs work
// in the background. finish, called with s.mu held, records the outcome
// on the resources and answers the operation's response. s.mu is held.
func (s *Server) startOpLocked(location, target, verb string, work func(ctx context.Context) error, finish func(err error) proto.Message) *longrunningpb.Operation {
	var rnd [6]byte
	_, _ = rand.Read(rnd[:])
	now := s.now()
	name := location + "/operations/operation-" + strconv.FormatInt(now.UnixMilli(), 10) + "-" + hex.EncodeToString(rnd[:])
	md := &commonpb.OperationMetadata{CreateTime: timestamppb.New(now), Target: target, Verb: verb, ApiVersion: "v1"}
	mdAny, _ := anypb.New(md)
	rec := &opRecord{Op: &longrunningpb.Operation{Name: name, Metadata: mdAny}, done: make(chan struct{})}
	s.ops[name] = rec
	s.busy[target] = name
	s.work.Add(1)
	go func() {
		defer s.work.Done()
		ctx, cancel := context.WithTimeout(s.bg, 30*time.Minute)
		err := work(ctx)
		cancel()
		s.mu.Lock()
		defer s.mu.Unlock()
		res := finish(err)
		delete(s.busy, target)
		md.EndTime = timestamppb.New(s.now())
		rec.Op.Metadata, _ = anypb.New(md)
		rec.Op.Done = true
		if err != nil {
			e := apierror.From(err)
			rec.Op.Result = &longrunningpb.Operation_Error{Error: &rpcstatus.Status{Code: int32(e.Code), Message: e.Message}}
		} else {
			if res == nil {
				res = &emptypb.Empty{}
			}
			a, _ := anypb.New(res)
			rec.Op.Result = &longrunningpb.Operation_Response{Response: a}
		}
		_ = s.saveLocked()
		close(rec.done)
	}()
	return proto.Clone(rec.Op).(*longrunningpb.Operation)
}

// busyLocked refuses a call on a resource another operation is changing.
// UNVERIFIED (docs re-read 2026-11-23: <https://docs.cloud.google.com/filestore/docs/reference/rest/v1/projects.locations.instances>) wording.
func (s *Server) busyLocked(names ...string) error {
	for _, n := range names {
		if op, ok := s.busy[n]; ok {
			return apierror.FailedPrecondition("Operation %s is in progress on %s; retry when it completes.", op, n)
		}
	}
	return nil
}

func (s *Server) operation(name string) (*longrunningpb.Operation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.ops[name]
	if !ok {
		return nil, apierror.NotFound("Resource '%s' was not found", name)
	}
	return proto.Clone(rec.Op).(*longrunningpb.Operation), nil
}

// Operation is an operation, as GetOperation answers it.
func (s *Server) Operation(name string) (*longrunningpb.Operation, error) { return s.operation(name) }

// WaitDone waits until the operation name is done or ctx ends.
func (s *Server) WaitDone(ctx context.Context, name string) {
	s.mu.Lock()
	rec, ok := s.ops[name]
	s.mu.Unlock()
	if !ok {
		return
	}
	select {
	case <-rec.done:
	case <-ctx.Done():
	}
}

// OperationsServer serves google.longrunning.Operations for the
// operations the API returns.
type OperationsServer struct {
	longrunningpb.UnimplementedOperationsServer
	api *Server
}

// NewOperationsServer is the operations service of api.
func NewOperationsServer(api *Server) *OperationsServer { return &OperationsServer{api: api} }

// Register adds the service to a gRPC server or to the JSON transcoder.
func (o *OperationsServer) Register(g grpc.ServiceRegistrar) {
	longrunningpb.RegisterOperationsServer(g, o)
}

// GetOperation answers an operation.
func (o *OperationsServer) GetOperation(_ context.Context, req *longrunningpb.GetOperationRequest) (*longrunningpb.Operation, error) {
	if !opNameRE.MatchString(req.GetName()) {
		return nil, apierror.InvalidArgument("name %q must be projects/{project}/locations/{location}/operations/{operation}", req.GetName())
	}
	return o.api.operation(req.GetName())
}

// ListOperations lists a location's operations, newest first; filter
// takes done and the metadata's verb and target.
func (o *OperationsServer) ListOperations(_ context.Context, req *longrunningpb.ListOperationsRequest) (*longrunningpb.ListOperationsResponse, error) {
	m := parentRE.FindStringSubmatch(req.GetName())
	if m == nil {
		return nil, apierror.InvalidArgument("name %q must be projects/{project}/locations/{location}", req.GetName())
	}
	match, err := opFilter(req.GetFilter())
	if err != nil {
		return nil, err
	}
	s := o.api
	s.mu.Lock()
	var ops []*longrunningpb.Operation
	for name, rec := range s.ops {
		if m[2] == "-" {
			if !strings.HasPrefix(name, "projects/"+m[1]+"/locations/") {
				continue
			}
		} else if !strings.HasPrefix(name, req.GetName()+"/operations/") {
			continue
		}
		if match(rec.Op) {
			ops = append(ops, proto.Clone(rec.Op).(*longrunningpb.Operation))
		}
	}
	s.mu.Unlock()
	sortOps(ops)
	page, next, err := aipfilter.Page("operations:"+req.GetName()+"|"+req.GetFilter(), ops, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, err
	}
	return &longrunningpb.ListOperationsResponse{Operations: page, NextPageToken: next}, nil
}

func sortOps(ops []*longrunningpb.Operation) {
	for i := 1; i < len(ops); i++ {
		for j := i; j > 0 && ops[j].GetName() > ops[j-1].GetName(); j-- {
			ops[j], ops[j-1] = ops[j-1], ops[j]
		}
	}
}

func opFilter(filter string) (func(*longrunningpb.Operation) bool, error) {
	type cond struct {
		field, value string
		neg          bool
	}
	var conds []cond
	if strings.TrimSpace(filter) != "" {
		for _, term := range strings.Split(filter, " AND ") {
			op := "="
			if strings.Contains(term, "!=") {
				op = "!="
			}
			k, v, ok := strings.Cut(term, op)
			k, v = strings.TrimSpace(k), strings.Trim(strings.TrimSpace(v), `"`)
			switch {
			case !ok:
				return nil, apierror.InvalidArgument("filter %q: each term is field = value, joined by AND", filter)
			case k == "done" && v != "true" && v != "false":
				return nil, apierror.InvalidArgument("filter %q: done is true or false", filter)
			case k != "done" && k != "metadata.verb" && k != "metadata.target":
				return nil, apierror.InvalidArgument("filter %q: field %q is not done, metadata.verb or metadata.target", filter, k)
			}
			conds = append(conds, cond{k, v, op == "!="})
		}
	}
	return func(op *longrunningpb.Operation) bool {
		md := &commonpb.OperationMetadata{}
		_ = op.GetMetadata().UnmarshalTo(md)
		for _, c := range conds {
			got := strconv.FormatBool(op.GetDone())
			switch c.field {
			case "metadata.verb":
				got = md.GetVerb()
			case "metadata.target":
				got = md.GetTarget()
			}
			if (got == c.value) == c.neg {
				return false
			}
		}
		return true
	}, nil
}

// DeleteOperation forgets a done operation; a running one is
// FAILED_PRECONDITION. UNVERIFIED (docs re-read 2026-11-23: <https://docs.cloud.google.com/filestore/docs/reference/rest/v1/projects.locations.instances>) wording.
func (o *OperationsServer) DeleteOperation(_ context.Context, req *longrunningpb.DeleteOperationRequest) (*emptypb.Empty, error) {
	s := o.api
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.ops[req.GetName()]
	if !ok {
		return nil, apierror.NotFound("Resource '%s' was not found", req.GetName())
	}
	if !rec.Op.GetDone() {
		return nil, apierror.FailedPrecondition("Operation %s is still running", req.GetName())
	}
	delete(s.ops, req.GetName())
	return &emptypb.Empty{}, nil
}

// CancelOperation records the request on the operation's metadata
// (cancel_requested); the work, a copy of files, runs to its end, as
// Google documents cancellation as best effort.
func (o *OperationsServer) CancelOperation(_ context.Context, req *longrunningpb.CancelOperationRequest) (*emptypb.Empty, error) {
	s := o.api
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.ops[req.GetName()]
	if !ok {
		return nil, apierror.NotFound("Resource '%s' was not found", req.GetName())
	}
	if !rec.Op.GetDone() {
		md := &commonpb.OperationMetadata{}
		_ = rec.Op.GetMetadata().UnmarshalTo(md)
		md.CancelRequested = true
		rec.Op.Metadata, _ = anypb.New(md)
	}
	return &emptypb.Empty{}, nil
}

// WaitOperation answers the operation once done, or when the timeout or
// the call's deadline passes.
func (o *OperationsServer) WaitOperation(ctx context.Context, req *longrunningpb.WaitOperationRequest) (*longrunningpb.Operation, error) {
	if _, err := o.api.operation(req.GetName()); err != nil {
		return nil, err
	}
	if t := req.GetTimeout(); t != nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, t.AsDuration())
		defer cancel()
	}
	o.api.WaitDone(ctx, req.GetName())
	return o.api.operation(req.GetName())
}
