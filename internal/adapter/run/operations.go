package run

import (
	"context"
	"strings"
	"time"

	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/paging"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"
)

// waitPoll is how often WaitOperation reads a pending operation again.
var waitPoll = 100 * time.Millisecond

// OperationsServer serves google.longrunning.Operations for the operations
// this adapter creates.
//
// Without it, CreateService returns an operation name that a client cannot
// poll: the official SDK's op.Wait calls Operations.GetOperation, and an
// unregistered service makes every create appear to hang.
type OperationsServer struct {
	longrunningpb.UnimplementedOperationsServer
	adapter *Server
}

// NewOperationsServer returns the operations service for an adapter.
func NewOperationsServer(a *Server) *OperationsServer { return &OperationsServer{adapter: a} }

// Register adds the operations service to a gRPC server or a JSON transcoder.
func (o *OperationsServer) Register(g grpc.ServiceRegistrar) {
	longrunningpb.RegisterOperationsServer(g, o)
}

// GetOperation returns a tracked operation so a client can poll a create.
func (o *OperationsServer) GetOperation(_ context.Context, req *longrunningpb.GetOperationRequest) (*longrunningpb.Operation, error) {
	return o.adapter.Operation(req.GetName())
}

// DeleteOperation forgets an operation. Deleting an unknown one is not an
// error: the caller's goal is that it no longer exists.
func (o *OperationsServer) DeleteOperation(_ context.Context, _ *longrunningpb.DeleteOperationRequest) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, nil
}

// CancelOperation is not supported: a deployment already in flight cannot be
// recalled here, and pretending otherwise would leave the caller believing the
// rollout stopped.
func (o *OperationsServer) CancelOperation(context.Context, *longrunningpb.CancelOperationRequest) (*emptypb.Empty, error) {
	return nil, apierror.Unimplemented("cancelling an in-flight deployment is not supported")
}

// ListOperations lists the operations under projects/*/locations/*, the
// collection run_v2.yaml binds, in name order and paged. A filter (#1263) is
// applied before paging, so a page holds only matching operations; one it
// cannot evaluate is INVALID_ARGUMENT, never ignored (opfilter.go).
func (o *OperationsServer) ListOperations(_ context.Context, req *longrunningpb.ListOperationsRequest) (*longrunningpb.ListOperationsResponse, error) {
	parent := req.GetName()
	if parent == "" || parent == "operations" {
		// Cloud Functions v1's operations.list (#1428), whose gRPC
		// service is this one.
		return o.adapter.listV1Operations(req)
	}
	if p := strings.Split(parent, "/"); len(p) != 4 || p[0] != "projects" || p[2] != "locations" || p[1] == "" || p[3] == "" {
		return nil, apierror.InvalidArgument("name %q must be projects/{project}/locations/{location}", parent)
	}
	filter, err := parseOpFilter(req.GetFilter())
	if err != nil {
		return nil, err
	}
	byName := map[string]*longrunningpb.Operation{}
	var names []string
	for _, op := range o.adapter.ops.List(parent + "/operations/") {
		p, err := o.adapter.toProtoOperation(op.Name)
		if err != nil {
			continue // forgotten between the listing and now
		}
		if filter != nil && !filter.match(p) {
			continue
		}
		byName[op.Name] = p
		names = append(names, op.Name)
	}
	page, next, err := paging.Page("run-operations:"+parent+"?"+req.GetFilter(), names, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, apierror.InvalidArgument("%v", err)
	}
	resp := &longrunningpb.ListOperationsResponse{NextPageToken: next}
	for _, n := range page {
		resp.Operations = append(resp.Operations, byName[n])
	}
	return resp, nil
}

// WaitOperation returns the operation once it is done, or as it is when the
// request's timeout (or the call's deadline, when none is given) passes, as
// AIP-151 describes: running out of time is not an error.
func (o *OperationsServer) WaitOperation(ctx context.Context, req *longrunningpb.WaitOperationRequest) (*longrunningpb.Operation, error) {
	op, err := o.adapter.toProtoOperation(req.GetName())
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
			return o.adapter.toProtoOperation(req.GetName())
		case <-timeout:
			return o.adapter.toProtoOperation(req.GetName())
		case <-tick.C:
			if op, err = o.adapter.toProtoOperation(req.GetName()); err != nil || op.GetDone() {
				return op, err
			}
		}
	}
}
