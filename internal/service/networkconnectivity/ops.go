package networkconnectivity

import (
	"context"
	"net/http"
	"regexp"
	"strings"

	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	ncpb "cloud.google.com/go/networkconnectivity/apiv1/networkconnectivitypb"
	"google.golang.org/genproto/googleapis/api/annotations"
	locationpb "google.golang.org/genproto/googleapis/cloud/location"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/aipfilter"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/locations"
	"github.com/cloudburrow/cloudburrow/internal/paging"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// opLocked records a finished operation on target: verb create, update or
// delete, and response the resource (or Empty for a delete).
func (s *Server) opLocked(parentName, target, verb string, response proto.Message) (*longrunningpb.Operation, error) {
	now := timestamppb.New(s.now())
	md, err := anypb.New(&ncpb.OperationMetadata{CreateTime: now, EndTime: now, Target: target, Verb: verb, ApiVersion: "v1"})
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	if response == nil {
		response = &emptypb.Empty{}
	}
	resp, err := anypb.New(response)
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	op := &longrunningpb.Operation{Name: parentName + "/operations/operation-" + randomHex(8), Metadata: md, Done: true,
		Result: &longrunningpb.Operation_Response{Response: resp}}
	s.ops[op.Name] = op
	if err := s.saveLocked(); err != nil {
		return nil, apierror.Wrap(err)
	}
	return clone(op), nil
}

// operations serves google.longrunning.Operations.
type operations struct {
	longrunningpb.UnimplementedOperationsServer
	s *Server
}

func (o *operations) GetOperation(_ context.Context, req *longrunningpb.GetOperationRequest) (*longrunningpb.Operation, error) {
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
	if !parentRE.MatchString(req.GetName()) {
		return nil, apierror.InvalidArgument("name %q must be projects/{project}/locations/{location}", req.GetName())
	}
	o.s.mu.Lock()
	defer o.s.mu.Unlock()
	keys := sortedKeys(o.s.ops, req.GetName()+"/operations/")
	all := make([]*longrunningpb.Operation, 0, len(keys))
	for _, k := range keys {
		all = append(all, clone(o.s.ops[k]))
	}
	out, next, err := listPage("ops:"+req.GetName(), all, req.GetFilter(), "", req.GetPageToken(), req.GetPageSize(),
		func(op *longrunningpb.Operation) string { return op.GetName() })
	if err != nil {
		return nil, err
	}
	return &longrunningpb.ListOperationsResponse{Operations: out, NextPageToken: next}, nil
}

func (o *operations) DeleteOperation(_ context.Context, req *longrunningpb.DeleteOperationRequest) (*emptypb.Empty, error) {
	o.s.mu.Lock()
	defer o.s.mu.Unlock()
	if _, ok := o.s.ops[req.GetName()]; !ok {
		return nil, apierror.NotFound("operation %s not found", req.GetName())
	}
	delete(o.s.ops, req.GetName())
	return &emptypb.Empty{}, apierror.Wrap(o.s.saveLocked())
}

// CancelOperation of a done operation does nothing (AIP-151): every
// operation here is done when it is returned.
func (o *operations) CancelOperation(ctx context.Context, req *longrunningpb.CancelOperationRequest) (*emptypb.Empty, error) {
	if _, err := o.GetOperation(ctx, &longrunningpb.GetOperationRequest{Name: req.GetName()}); err != nil {
		return nil, err
	}
	return &emptypb.Empty{}, nil
}

// listPage filters (AIP-160), orders (by name; order_by "name desc"
// reverses) and pages items.
func listPage[T proto.Message](scope string, items []T, filter, orderBy, token string, size int32, key func(T) string) ([]T, string, error) {
	items, err := aipfilter.Apply(filter, items)
	if err != nil {
		return nil, "", err
	}
	byKey := map[string]T{}
	keys := make([]string, 0, len(items))
	for _, it := range items {
		k := key(it)
		byKey[k] = it
		keys = append(keys, k)
	}
	ob := strings.TrimSpace(orderBy)
	switch ob {
	case "", "name", "name asc":
	case "name desc":
		for i, j := 0, len(keys)-1; i < j; i, j = i+1, j-1 {
			keys[i], keys[j] = keys[j], keys[i]
		}
	default:
		return nil, "", apierror.InvalidArgument("order_by %q is not supported: order by name, or name desc", orderBy)
	}
	page, next, err := paging.Page(scope+"|"+filter+"|"+ob, keys, token, int(size))
	if err != nil {
		return nil, "", apierror.InvalidArgument("invalid page_token")
	}
	out := make([]T, 0, len(page))
	for _, k := range page {
		out = append(out, byKey[k])
	}
	return out, next, nil
}

// locationsServer serves google.cloud.location.Locations.
type locationsServer struct {
	locationpb.UnimplementedLocationsServer
}

var projectRE = regexp.MustCompile(`^projects/[^/]+$`)

func (locationsServer) ListLocations(_ context.Context, req *locationpb.ListLocationsRequest) (*locationpb.ListLocationsResponse, error) {
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
	page, next, err := paging.Page("nc-locations:"+req.GetName()+"|"+req.GetFilter(), ids, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, apierror.InvalidArgument("invalid page_token")
	}
	out := &locationpb.ListLocationsResponse{NextPageToken: next}
	for _, id := range page {
		out.Locations = append(out.Locations, mk(id))
	}
	return out, nil
}

func (locationsServer) GetLocation(_ context.Context, req *locationpb.GetLocationRequest) (*locationpb.Location, error) {
	m := parentRE.FindStringSubmatch(req.GetName())
	if m == nil {
		return nil, apierror.InvalidArgument("name %q must be projects/{project}/locations/{location}", req.GetName())
	}
	for _, id := range Locations() {
		if id == m[2] {
			return &locationpb.Location{Name: req.GetName(), LocationId: id, DisplayName: id}, nil
		}
	}
	return nil, apierror.NotFound("Location %s was not found", req.GetName())
}

// The services of the API that are not served: every method answers
// UNIMPLEMENTED, over gRPC and JSON.
type (
	hubs struct {
		ncpb.UnimplementedHubServiceServer
	}
	routing struct {
		ncpb.UnimplementedPolicyBasedRoutingServiceServer
	}
	ranges struct {
		ncpb.UnimplementedInternalRangeServiceServer
	}
	dataTransfer struct {
		ncpb.UnimplementedDataTransferServiceServer
	}
)

// Register serves the API, its operations and locations on g.
func (s *Server) Register(g grpc.ServiceRegistrar) {
	longrunningpb.RegisterOperationsServer(g, &operations{s: s})
	locations.Register(g, locationsServer{})
	s.RegisterAPI(g)
}

// RegisterAPI registers the API's own services, without its Locations and
// Operations mixins: CrossNetworkAutomationService, and the others, whose
// every method answers UNIMPLEMENTED.
func (s *Server) RegisterAPI(g grpc.ServiceRegistrar) {
	ncpb.RegisterCrossNetworkAutomationServiceServer(g, s)
	ncpb.RegisterHubServiceServer(g, hubs{})
	ncpb.RegisterPolicyBasedRoutingServiceServer(g, routing{})
	ncpb.RegisterInternalRangeServiceServer(g, ranges{})
	ncpb.RegisterDataTransferServiceServer(g, dataTransfer{})
}

// HasOperation reports whether name is one of this API's operations, for
// a port whose gRPC surface serves other APIs' operations beside them.
func (s *Server) HasOperation(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.ops[name]
	return ok
}

// Operations is the operations server, for a gRPC server that serves
// another API's operations beside these (Compute's port has none).
func (s *Server) Operations() longrunningpb.OperationsServer { return &operations{s: s} }

// Mixins are networkconnectivity_v1.yaml's Locations and Operations
// bindings.
func Mixins() []*annotations.HttpRule {
	return []*annotations.HttpRule{
		{Selector: "google.cloud.location.Locations.GetLocation",
			Pattern: &annotations.HttpRule_Get{Get: "/v1/{name=projects/*/locations/*}"}},
		{Selector: "google.cloud.location.Locations.ListLocations",
			Pattern: &annotations.HttpRule_Get{Get: "/v1/{name=projects/*}/locations"}},
		{Selector: "google.longrunning.Operations.CancelOperation",
			Pattern: &annotations.HttpRule_Post{Post: "/v1/{name=projects/*/locations/*/operations/*}:cancel"}, Body: "*"},
		{Selector: "google.longrunning.Operations.DeleteOperation",
			Pattern: &annotations.HttpRule_Delete{Delete: "/v1/{name=projects/*/locations/*/operations/*}"}},
		{Selector: "google.longrunning.Operations.GetOperation",
			Pattern: &annotations.HttpRule_Get{Get: "/v1/{name=projects/*/locations/*/operations/*}"}},
		{Selector: "google.longrunning.Operations.ListOperations",
			Pattern: &annotations.HttpRule_Get{Get: "/v1/{name=projects/*/locations/*}/operations"}},
	}
}

// NewRESTHandler is the JSON API over s.
func NewRESTHandler(s *Server) http.Handler {
	t := &rest.Transcoder{Packages: []string{"google.cloud.networkconnectivity.v1"}, Mixins: Mixins(), SnakeCaseMasks: true}
	s.Register(t)
	return t
}

// colls are the collections networkconnectivity.googleapis.com's JSON
// API serves under /v1/projects/P/locations/L/.
var colls = map[string]bool{"hubs": true, "spokes": true, "internalRanges": true, "policyBasedRoutes": true,
	"serviceClasses": true, "serviceConnectionMaps": true, "serviceConnectionPolicies": true,
	"serviceConnectionTokens": true, "multicloudDataTransferConfigs": true,
	"multicloudDataTransferSupportedServices": true}

// Handles is whether a request on the Compute Engine port is this API's
// JSON surface: a path under one of its collections or one of its
// operations. Serverless VPC Access, Certificate Manager, Network Security,
// Network Services and IAP serve /v1/projects/P/locations paths on the
// same port (Serverless VPC Access answers the locations), so every other
// /v1/ path is theirs (main run 45371505851: /v1/.../connectors was
// answered here, with no method bound).
func (s *Server) Handles(r *http.Request) bool {
	path := r.URL.Path
	if !strings.HasPrefix(path, "/v1/projects/") {
		return false
	}
	p := strings.Split(strings.TrimPrefix(path, "/v1/"), "/")
	if len(p) < 5 || p[2] != "locations" {
		return false
	}
	c := strings.SplitN(p[4], ":", 2)[0]
	if colls[c] {
		return true
	}
	if c == "operations" && len(p) >= 6 {
		return s.HasOperation(strings.Join(p[:5], "/") + "/" + strings.SplitN(p[5], ":", 2)[0])
	}
	return false
}
