package dataproc

import (
	"context"
	"net/http"
	"strings"
	"time"

	dataprocpb "cloud.google.com/go/dataproc/v2/apiv1/dataprocpb"
	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/genproto/googleapis/api/annotations"
	locationpb "google.golang.org/genproto/googleapis/cloud/location"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/aipfilter"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/locations"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// Register serves the three controllers and the Operations and Locations
// mixins on g, a gRPC server or the JSON transcoder.
func (s *Server) Register(g grpc.ServiceRegistrar) {
	dataprocpb.RegisterBatchControllerServer(g, s)
	dataprocpb.RegisterSessionControllerServer(g, s)
	dataprocpb.RegisterSessionTemplateControllerServer(g, s)
	longrunningpb.RegisterOperationsServer(g, &operations{s: s})
	locations.Register(g, &locationsServer{})
}

// newOpLocked records a running operation of metadata.
func (s *Server) newOpLocked(name string, metadata proto.Message) *longrunningpb.Operation {
	md, _ := anypb.New(metadata)
	op := &longrunningpb.Operation{Name: name, Metadata: md}
	s.ops[name] = op
	return op
}

// finishOpLocked ends the operation name with response or err, and its
// metadata's done time.
func (s *Server) finishOpLocked(name string, response proto.Message, err error) {
	op := s.ops[name]
	if op == nil || op.Done {
		return
	}
	op.Done = true
	if err != nil {
		st, _ := status.FromError(apierror.Wrap(err))
		op.Result = &longrunningpb.Operation_Error{Error: st.Proto()}
	} else if response != nil {
		a, _ := anypb.New(response)
		op.Result = &longrunningpb.Operation_Response{Response: a}
	}
	done := timestamppb.New(s.now())
	if op.Metadata != nil {
		if m, e := op.Metadata.UnmarshalNew(); e == nil {
			switch md := m.(type) {
			case *dataprocpb.BatchOperationMetadata:
				md.DoneTime = done
			case *dataprocpb.SessionOperationMetadata:
				md.DoneTime = done
			}
			op.Metadata, _ = anypb.New(m)
		}
	}
}

// operations is google.longrunning.Operations over the server's
// operations: a batch's create, a session's create, terminate and delete.
type operations struct {
	longrunningpb.UnimplementedOperationsServer
	s *Server
}

func (o *operations) GetOperation(_ context.Context, req *longrunningpb.GetOperationRequest) (*longrunningpb.Operation, error) {
	o.s.mu.Lock()
	defer o.s.mu.Unlock()
	op, err := o.s.opLocked(req.GetName())
	if err != nil {
		return nil, err
	}
	return clone(op), nil
}

func (s *Server) opLocked(name string) (*longrunningpb.Operation, error) {
	m := opRE.FindStringSubmatch(name)
	if m == nil {
		return nil, apierror.InvalidArgument("name %q must be projects/{project}/regions/{region}/operations/{operation}", name)
	}
	// An operation is named under regions/; locations/ names it too.
	canonical := "projects/" + m[1] + "/regions/" + m[3] + "/operations/" + m[4]
	op, ok := s.ops[canonical]
	if !ok {
		return nil, apierror.NotFound("Operation %s not found", name)
	}
	return op, nil
}

// WaitOperation waits for the operation's end, or the request's timeout
// (Operations' default, one minute when none is given).
func (o *operations) WaitOperation(ctx context.Context, req *longrunningpb.WaitOperationRequest) (*longrunningpb.Operation, error) {
	timeout := time.Minute
	if t := req.GetTimeout(); t != nil {
		timeout = t.AsDuration()
	}
	deadline := time.Now().Add(timeout)
	for {
		op, err := o.GetOperation(ctx, &longrunningpb.GetOperationRequest{Name: req.GetName()})
		if err != nil || op.Done || time.Now().After(deadline) {
			return op, err
		}
		select {
		case <-ctx.Done():
			return op, nil
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func (o *operations) ListOperations(_ context.Context, req *longrunningpb.ListOperationsRequest) (*longrunningpb.ListOperationsResponse, error) {
	name := strings.Replace(req.GetName(), "/locations/", "/regions/", 1)
	name = strings.TrimSuffix(name, "/operations")
	parts := strings.Split(name, "/")
	if len(parts) != 4 || parts[0] != "projects" || parts[2] != "regions" {
		return nil, apierror.InvalidArgument("name %q must be projects/{project}/regions/{region}/operations", req.GetName())
	}
	o.s.mu.Lock()
	var all []*longrunningpb.Operation
	for _, k := range sortedKeys(o.s.ops) {
		if strings.HasPrefix(k, name+"/operations/") {
			all = append(all, clone(o.s.ops[k]))
		}
	}
	o.s.mu.Unlock()
	out, next, err := aipfilter.ListPage("dataproc-ops:"+name, req.GetFilter(), "", all, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, err
	}
	return &longrunningpb.ListOperationsResponse{Operations: out, NextPageToken: next}, nil
}

func (o *operations) DeleteOperation(_ context.Context, req *longrunningpb.DeleteOperationRequest) (*emptypb.Empty, error) {
	o.s.mu.Lock()
	defer o.s.mu.Unlock()
	op, err := o.s.opLocked(req.GetName())
	if err != nil {
		return nil, err
	}
	delete(o.s.ops, op.Name)
	o.s.save()
	return &emptypb.Empty{}, nil
}

// CancelOperation cancels a batch: "To cancel a batch, call
// operations.cancel on the batch's operation" (batches' reference). The
// batch goes CANCELLING, then CANCELLED once its Spark application is
// stopped. A done operation is left as it is; a session's operations
// cannot be cancelled.
func (o *operations) CancelOperation(_ context.Context, req *longrunningpb.CancelOperationRequest) (*emptypb.Empty, error) {
	s := o.s
	s.mu.Lock()
	defer s.mu.Unlock()
	op, err := s.opLocked(req.GetName())
	if err != nil {
		return nil, err
	}
	if op.Done {
		return nil, apierror.FailedPrecondition("Operation %s is already done", op.Name)
	}
	var batch *dataprocpb.Batch
	for _, b := range s.batches {
		if b.Operation == op.Name {
			batch = b
		}
	}
	if batch == nil {
		return nil, status.Errorf(codes.FailedPrecondition, "Operation %s cannot be cancelled", op.Name)
	}
	if batch.State == dataprocpb.Batch_PENDING || batch.State == dataprocpb.Batch_RUNNING {
		s.setBatchStateLocked(batch, dataprocpb.Batch_CANCELLING, "")
		if c := s.cancels[batch.Name]; c != nil {
			c()
		}
		s.save()
	}
	return &emptypb.Empty{}, nil
}

// ---- google.cloud.location.Locations ----

type locationsServer struct {
	locationpb.UnimplementedLocationsServer
}

func (l *locationsServer) ListLocations(_ context.Context, req *locationpb.ListLocationsRequest) (*locationpb.ListLocationsResponse, error) {
	if !projectRE.MatchString(req.GetName()) {
		return nil, apierror.InvalidArgument("name %q must be projects/{project}", req.GetName())
	}
	mk := func(id string) *locationpb.Location {
		return &locationpb.Location{Name: req.GetName() + "/locations/" + id, LocationId: id, DisplayName: id}
	}
	ids, err := aipfilter.LocationIDs(req.GetFilter(), Locations(), func(id string) (*locationpb.Location, error) { return mk(id), nil })
	if err != nil {
		return nil, err
	}
	var all []*locationpb.Location
	for _, id := range ids {
		all = append(all, mk(id))
	}
	out, next, err := aipfilter.Page("dataproc-locations:"+req.GetName(), all, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, err
	}
	return &locationpb.ListLocationsResponse{Locations: out, NextPageToken: next}, nil
}

func (l *locationsServer) GetLocation(_ context.Context, req *locationpb.GetLocationRequest) (*locationpb.Location, error) {
	m := parentRE.FindStringSubmatch(req.GetName())
	if m == nil {
		return nil, apierror.InvalidArgument("name %q must be projects/{project}/locations/{location}", req.GetName())
	}
	if !validLocation(m[2]) {
		return nil, apierror.NotFound("Location %s not found", req.GetName())
	}
	return &locationpb.Location{Name: req.GetName(), LocationId: m[2], DisplayName: m[2]}, nil
}

// ---- JSON API ----

// mixins are the bindings dataproc_v1.yaml adds: Operations, under
// regions/ (Dataproc's operations' names) and locations/, and Locations.
var mixins = []*annotations.HttpRule{
	{Selector: "google.cloud.location.Locations.GetLocation",
		Pattern: &annotations.HttpRule_Get{Get: "/v1/{name=projects/*/locations/*}"}},
	{Selector: "google.cloud.location.Locations.ListLocations",
		Pattern: &annotations.HttpRule_Get{Get: "/v1/{name=projects/*}/locations"}},
	{Selector: "google.longrunning.Operations.CancelOperation",
		Pattern: &annotations.HttpRule_Post{Post: "/v1/{name=projects/*/regions/*/operations/*}:cancel"},
		AdditionalBindings: []*annotations.HttpRule{
			{Pattern: &annotations.HttpRule_Post{Post: "/v1/{name=projects/*/locations/*/operations/*}:cancel"}}}},
	{Selector: "google.longrunning.Operations.DeleteOperation",
		Pattern: &annotations.HttpRule_Delete{Delete: "/v1/{name=projects/*/regions/*/operations/*}"},
		AdditionalBindings: []*annotations.HttpRule{
			{Pattern: &annotations.HttpRule_Delete{Delete: "/v1/{name=projects/*/locations/*/operations/*}"}}}},
	{Selector: "google.longrunning.Operations.GetOperation",
		Pattern: &annotations.HttpRule_Get{Get: "/v1/{name=projects/*/regions/*/operations/*}"},
		AdditionalBindings: []*annotations.HttpRule{
			{Pattern: &annotations.HttpRule_Get{Get: "/v1/{name=projects/*/locations/*/operations/*}"}}}},
	{Selector: "google.longrunning.Operations.ListOperations",
		Pattern: &annotations.HttpRule_Get{Get: "/v1/{name=projects/*/regions/*/operations}"},
		AdditionalBindings: []*annotations.HttpRule{
			{Pattern: &annotations.HttpRule_Get{Get: "/v1/{name=projects/*/locations/*/operations}"}}}},
}

// Mixins are the JSON API's Operations and Locations bindings.
func Mixins() []*annotations.HttpRule { return mixins }

// Transcoder is the JSON API's configuration, with nothing registered.
func Transcoder() *rest.Transcoder {
	return &rest.Transcoder{Packages: []string{"google.cloud.dataproc.v1"}, Mixins: mixins, SnakeCaseMasks: true}
}

// NewRESTHandler is the JSON API over s, with the Spark Connect bridge's
// WebSocket path beside it.
func NewRESTHandler(s *Server) http.Handler {
	t := Transcoder()
	s.Register(t)
	return t
}

// CancelBatch cancels a batch through its operation, as the console's
// Cancel does (operations.cancel on Batch.operation).
func (s *Server) CancelBatch(ctx context.Context, name string) error {
	s.mu.Lock()
	b := s.batches[name]
	op := ""
	if b != nil {
		op = b.GetOperation()
	}
	s.mu.Unlock()
	if b == nil {
		return status.Errorf(codes.NotFound, "Batch %s not found", name)
	}
	_, err := (&operations{s: s}).CancelOperation(ctx, &longrunningpb.CancelOperationRequest{Name: op})
	return err
}
