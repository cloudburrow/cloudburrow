package dataform

import (
	"context"
	"net/http"
	"regexp"
	"strings"

	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/genproto/googleapis/api/annotations"
	locationpb "google.golang.org/genproto/googleapis/cloud/location"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/cloudburrow/cloudburrow/internal/aipfilter"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/locations"
	"github.com/cloudburrow/cloudburrow/internal/paging"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// Register serves the API, its operations and its locations on g.
func (s *Server) Register(g grpc.ServiceRegistrar) {
	g.RegisterService(s.keyedDesc(), s)
	s.RegisterBeta(g)
	longrunningpb.RegisterOperationsServer(g, &operations{s: s})
	locations.Register(g, &locationsServer{})
}

// ---- google.cloud.location.Locations ----

type locationsServer struct {
	locationpb.UnimplementedLocationsServer
}

var projectRE = regexp.MustCompile(`^projects/[^/]+$`)

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
	page, next, err := paging.Page("locations:"+req.GetName(), ids, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, apierror.InvalidArgument("invalid page_token")
	}
	out := &locationpb.ListLocationsResponse{NextPageToken: next}
	for _, id := range page {
		out.Locations = append(out.Locations, mk(id))
	}
	return out, nil
}

func (l *locationsServer) GetLocation(_ context.Context, req *locationpb.GetLocationRequest) (*locationpb.Location, error) {
	m := locationRE.FindStringSubmatch(req.GetName())
	if m == nil {
		return nil, apierror.InvalidArgument("name %q must be projects/{project}/locations/{location}", req.GetName())
	}
	if !validLocation(m[2]) {
		return nil, apierror.NotFound("Location '%s' was not found", req.GetName())
	}
	return &locationpb.Location{Name: req.GetName(), LocationId: m[2], DisplayName: m[2]}, nil
}

// ---- google.longrunning.Operations ----

// operations serves the operations MoveRepository, MoveFolder, the tree
// deletes and CreateCompilationResultLongRunning return, done.
type operations struct {
	longrunningpb.UnimplementedOperationsServer
	s *Server
}

func (o *operations) GetOperation(_ context.Context, req *longrunningpb.GetOperationRequest) (*longrunningpb.Operation, error) {
	if !operationRE.MatchString(req.GetName()) {
		return nil, apierror.InvalidArgument("name %q must be projects/*/locations/*/operations/*", req.GetName())
	}
	o.s.mu.Lock()
	defer o.s.mu.Unlock()
	op, ok := o.s.ops[req.GetName()]
	if !ok {
		return nil, apierror.NotFound("operation %s not found", req.GetName())
	}
	return clone(op), nil
}

func (o *operations) WaitOperation(ctx context.Context, req *longrunningpb.WaitOperationRequest) (*longrunningpb.Operation, error) {
	return o.GetOperation(ctx, &longrunningpb.GetOperationRequest{Name: req.GetName()})
}

func (o *operations) ListOperations(_ context.Context, req *longrunningpb.ListOperationsRequest) (*longrunningpb.ListOperationsResponse, error) {
	if _, err := checkLocation("name", req.GetName()); err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.GetFilter()) != "" {
		return nil, apierror.InvalidArgument("filter is not supported on Dataform operations")
	}
	o.s.mu.Lock()
	defer o.s.mu.Unlock()
	keys := childrenOf(o.s.ops, req.GetName(), "operations")
	start, end, next, err := page("ops:"+req.GetName(), req.GetPageToken(), req.GetPageSize(), len(keys))
	if err != nil {
		return nil, err
	}
	out := &longrunningpb.ListOperationsResponse{NextPageToken: next}
	for _, k := range keys[start:end] {
		out.Operations = append(out.Operations, clone(o.s.ops[k]))
	}
	return out, nil
}

func (o *operations) DeleteOperation(_ context.Context, req *longrunningpb.DeleteOperationRequest) (*emptypb.Empty, error) {
	o.s.mu.Lock()
	defer o.s.mu.Unlock()
	if _, ok := o.s.ops[req.GetName()]; !ok {
		return nil, apierror.NotFound("operation %s not found", req.GetName())
	}
	delete(o.s.ops, req.GetName())
	return &emptypb.Empty{}, nil
}

// CancelOperation of a done operation does nothing (AIP-151): every
// operation here is done when it is returned.
func (o *operations) CancelOperation(ctx context.Context, req *longrunningpb.CancelOperationRequest) (*emptypb.Empty, error) {
	if _, err := o.GetOperation(ctx, &longrunningpb.GetOperationRequest{Name: req.GetName()}); err != nil {
		return nil, err
	}
	return &emptypb.Empty{}, nil
}

// Mixins are the bindings dataform_v1.yaml and dataform_v1beta1.yaml add:
// Locations and Operations.
func Mixins() []*annotations.HttpRule {
	var out []*annotations.HttpRule
	for _, v := range []string{"/v1", "/v1beta1"} {
		op := v + "/{name=projects/*/locations/*/operations/*}"
		out = append(out,
			&annotations.HttpRule{Selector: "google.cloud.location.Locations.GetLocation", Pattern: &annotations.HttpRule_Get{Get: v + "/{name=projects/*/locations/*}"}},
			&annotations.HttpRule{Selector: "google.cloud.location.Locations.ListLocations", Pattern: &annotations.HttpRule_Get{Get: v + "/{name=projects/*}/locations"}},
			&annotations.HttpRule{Selector: "google.longrunning.Operations.ListOperations", Pattern: &annotations.HttpRule_Get{Get: v + "/{name=projects/*/locations/*}/operations"}},
			&annotations.HttpRule{Selector: "google.longrunning.Operations.GetOperation", Pattern: &annotations.HttpRule_Get{Get: op}},
			&annotations.HttpRule{Selector: "google.longrunning.Operations.DeleteOperation", Pattern: &annotations.HttpRule_Delete{Delete: op}},
			&annotations.HttpRule{Selector: "google.longrunning.Operations.CancelOperation", Pattern: &annotations.HttpRule_Post{Post: op + ":cancel"}, Body: "*"},
		)
	}
	return out
}

// Transcoder is the JSON API's configuration, with nothing registered.
func Transcoder() *rest.Transcoder {
	return &rest.Transcoder{Packages: []string{"google.cloud.dataform.v1", "google.cloud.dataform.v1beta1"}, Mixins: Mixins(), SnakeCaseMasks: true}
}

// NewRESTHandler is the JSON API over s.
func NewRESTHandler(s *Server) http.Handler {
	t := Transcoder()
	s.Register(t)
	return t
}
