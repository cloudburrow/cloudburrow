package redisadmin

import (
	"context"
	"strings"
	"time"

	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	rpcstatus "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/lro"
	"github.com/cloudburrow/cloudburrow/internal/paging"
)

// operation renders a tracked operation as google.longrunning shapes it:
// OperationMetadata while it runs and after, then the Instance (create,
// update) or Empty (delete), or the error.
func (s *Server) operation(name string) (*longrunningpb.Operation, error) {
	if s.created == nil {
		return nil, apierror.NotFound("operation %s not found", name)
	}
	op, err := s.created.ops.Get(name)
	if err != nil {
		return nil, apierror.NotFound("operation %s not found", name)
	}
	out := &longrunningpb.Operation{Name: op.Name, Done: op.Done()}
	if md, ok := op.Metadata.(proto.Message); ok {
		if a, err := anypb.New(md); err == nil {
			out.Metadata = a
		}
	}
	switch op.State {
	case lro.StateFailed:
		e := apierror.From(op.Error)
		out.Result = &longrunningpb.Operation_Error{Error: &rpcstatus.Status{Code: int32(e.Code), Message: e.Message}}
	case lro.StateSucceeded:
		if res, ok := op.Response.(proto.Message); ok {
			if a, err := anypb.New(res); err == nil {
				out.Result = &longrunningpb.Operation_Response{Response: a}
			}
		}
	}
	return out, nil
}

// OperationsServer serves google.longrunning.Operations for the operations
// CreateInstance, UpdateInstance and DeleteInstance return, as
// redis.googleapis.com does: the SDK's op.Wait, gcloud and Terraform poll
// GetOperation.
type OperationsServer struct {
	longrunningpb.UnimplementedOperationsServer
	api *Server
}

// NewOperationsServer returns the operations service of api.
func NewOperationsServer(api *Server) *OperationsServer { return &OperationsServer{api: api} }

// Register adds the service to a gRPC server or to the JSON transcoder.
func (o *OperationsServer) Register(g grpc.ServiceRegistrar) {
	longrunningpb.RegisterOperationsServer(g, o)
}

// waitPoll is how often WaitOperation reads a pending operation again.
var waitPoll = 100 * time.Millisecond

// GetOperation returns an operation.
func (o *OperationsServer) GetOperation(_ context.Context, req *longrunningpb.GetOperationRequest) (*longrunningpb.Operation, error) {
	return o.api.operation(req.GetName())
}

// ListOperations lists the operations of projects/*/locations/*, in name
// order and paged. A filter is not evaluated, so one is refused rather
// than ignored.
func (o *OperationsServer) ListOperations(_ context.Context, req *longrunningpb.ListOperationsRequest) (*longrunningpb.ListOperationsResponse, error) {
	parent := req.GetName()
	if p := strings.Split(parent, "/"); len(p) != 4 || p[0] != "projects" || p[2] != "locations" || p[1] == "" || p[3] == "" {
		return nil, apierror.InvalidArgument("name %q must be projects/{project}/locations/{location}", parent)
	}
	if req.GetFilter() != "" {
		return nil, apierror.Unimplemented("filtering operations is not supported")
	}
	var names []string
	if ops := o.api.Operations(); ops != nil {
		for _, op := range ops.List(parent + "/operations/") {
			names = append(names, op.Name)
		}
	}
	page, next, err := paging.Page("redis-operations:"+parent, names, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, apierror.InvalidArgument("%v", err)
	}
	resp := &longrunningpb.ListOperationsResponse{NextPageToken: next}
	for _, n := range page {
		if op, err := o.api.operation(n); err == nil {
			resp.Operations = append(resp.Operations, op)
		}
	}
	return resp, nil
}

// DeleteOperation is UNIMPLEMENTED, as CancelOperation: the operations are
// kept for as long as CloudBurrow runs.
func (o *OperationsServer) DeleteOperation(context.Context, *longrunningpb.DeleteOperationRequest) (*emptypb.Empty, error) {
	return nil, apierror.Unimplemented("deleting an operation is not supported")
}

// CancelOperation is UNIMPLEMENTED: a server being started is not stopped
// halfway, and saying it was would be false.
func (o *OperationsServer) CancelOperation(context.Context, *longrunningpb.CancelOperationRequest) (*emptypb.Empty, error) {
	return nil, apierror.Unimplemented("cancelling an operation is not supported")
}

// WaitOperation returns the operation once it is done, or as it is when
// the request's timeout or the call's deadline passes (AIP-151).
func (o *OperationsServer) WaitOperation(ctx context.Context, req *longrunningpb.WaitOperationRequest) (*longrunningpb.Operation, error) {
	op, err := o.api.operation(req.GetName())
	if err != nil || op.GetDone() {
		return op, err
	}
	var timeout <-chan time.Time
	if t := req.GetTimeout(); t != nil {
		if err := t.CheckValid(); err != nil || t.AsDuration() < 0 {
			return nil, apierror.InvalidArgument("timeout must be a non-negative duration")
		}
		timer := time.NewTimer(t.AsDuration())
		defer timer.Stop()
		timeout = timer.C
	}
	tick := time.NewTicker(waitPoll)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return o.api.operation(req.GetName())
		case <-timeout:
			return o.api.operation(req.GetName())
		case <-tick.C:
			if op, err = o.api.operation(req.GetName()); err != nil || op.GetDone() {
				return op, err
			}
		}
	}
}
