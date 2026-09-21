package run

import (
	"context"

	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"github.com/identity-wael/cloudburrow/internal/apierror"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"
)

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

// Register adds the operations service to a gRPC server.
func (o *OperationsServer) Register(g *grpc.Server) {
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
