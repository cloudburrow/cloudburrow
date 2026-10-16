package redisadmin

import (
	"context"
	"strconv"
	"strings"
	"time"

	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	redispb "cloud.google.com/go/redis/apiv1/redispb"
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

// Operation returns an operation CreateInstance, UpdateInstance or
// DeleteInstance started, as GetOperation does.
func (s *Server) Operation(name string) (*longrunningpb.Operation, error) { return s.operation(name) }

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
	match, err := operationFilter(req.GetFilter())
	if err != nil {
		return nil, err
	}
	var names []string
	if ops := o.api.Operations(); ops != nil {
		for _, op := range ops.List(parent + "/operations/") {
			if rendered, err := o.api.operation(op.Name); err == nil && match(rendered) {
				names = append(names, op.Name)
			}
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

// DeleteOperation forgets an operation (#1386): the work it tracks goes on,
// and GetOperation is NOT_FOUND after.
func (o *OperationsServer) DeleteOperation(_ context.Context, req *longrunningpb.DeleteOperationRequest) (*emptypb.Empty, error) {
	ops := o.api.Operations()
	if ops == nil || ops.Delete(req.GetName()) != nil {
		return nil, apierror.NotFound("operation %s not found", req.GetName())
	}
	return &emptypb.Empty{}, nil
}

// CancelOperation stops an import or export in progress (#1386): its
// operation fails CANCELLED, and the instance is READY with its data as
// the server has it. A done operation is left as it is. Create, update
// and delete are FAILED_PRECONDITION: a server being started or removed is
// not stopped halfway, and saying it was would be false.
func (o *OperationsServer) CancelOperation(_ context.Context, req *longrunningpb.CancelOperationRequest) (*emptypb.Empty, error) {
	op, err := o.api.operation(req.GetName())
	if err != nil {
		return nil, err
	}
	if op.GetDone() {
		return &emptypb.Empty{}, nil
	}
	if !o.api.created.cancel(req.GetName()) {
		return nil, apierror.FailedPrecondition("operation %s cannot be cancelled: only an import or export can", req.GetName())
	}
	return &emptypb.Empty{}, nil
}

// operationFilter is ListOperations' filter (AIP-160), the subset of it
// that names an operation's own fields: comparisons of done, and of
// metadata's verb and target, with = or !=, joined by AND. Another field
// or operator is INVALID_ARGUMENT, not ignored.
func operationFilter(filter string) (func(*longrunningpb.Operation) bool, error) {
	type cond struct {
		field, value string
		neg          bool
	}
	var conds []cond
	if strings.TrimSpace(filter) != "" {
		for _, term := range strings.Split(filter, " AND ") {
			term = strings.TrimSpace(term)
			op := "="
			if strings.Contains(term, "!=") {
				op = "!="
			}
			k, v, ok := strings.Cut(term, op)
			k, v = strings.TrimSpace(k), strings.Trim(strings.TrimSpace(v), `"`)
			if !ok || k == "" {
				return nil, apierror.InvalidArgument("filter %q: each term is field = value, joined by AND", filter)
			}
			switch k {
			case "done":
				if v != "true" && v != "false" {
					return nil, apierror.InvalidArgument("filter %q: done is true or false", filter)
				}
			case "metadata.verb", "metadata.target":
			default:
				return nil, apierror.InvalidArgument("filter %q: field %q is not done, metadata.verb or metadata.target", filter, k)
			}
			conds = append(conds, cond{field: k, value: v, neg: op == "!="})
		}
	}
	return func(op *longrunningpb.Operation) bool {
		md := &redispb.OperationMetadata{}
		_ = op.GetMetadata().UnmarshalTo(md)
		for _, c := range conds {
			var got string
			switch c.field {
			case "done":
				got = strconv.FormatBool(op.GetDone())
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
