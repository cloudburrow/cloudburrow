package privateca

import (
	"context"
	"net/http"
	"sort"
	"strings"

	"cloud.google.com/go/iam/apiv1/iampb"
	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/genproto/googleapis/api/annotations"
	locationpb "google.golang.org/genproto/googleapis/cloud/location"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/cloudburrow/cloudburrow/internal/aipfilter"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/iampolicy"
	"github.com/cloudburrow/cloudburrow/internal/locations"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// ---- google.longrunning.Operations ----

// operations serves the operations every mutation returns, done.
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
	if err := checkLocation("name", req.GetName()); err != nil {
		return nil, err
	}
	o.s.mu.Lock()
	defer o.s.mu.Unlock()
	out, next, err := filteredPage(req.GetFilter(), "", "privateca-ops:"+req.GetName(), sortedKeys(o.s.ops, req.GetName()+"/operations/"), req.GetPageToken(), req.GetPageSize(),
		func(k string) *longrunningpb.Operation { return clone(o.s.ops[k]) })
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
	if err := o.s.save(); err != nil {
		return nil, apierror.Wrap(err)
	}
	return &emptypb.Empty{}, nil
}

// CancelOperation of a done operation does nothing, as AIP-151 has it: every
// operation here is done when it is returned.
func (o *operations) CancelOperation(ctx context.Context, req *longrunningpb.CancelOperationRequest) (*emptypb.Empty, error) {
	if _, err := o.GetOperation(ctx, &longrunningpb.GetOperationRequest{Name: req.GetName()}); err != nil {
		return nil, err
	}
	return &emptypb.Empty{}, nil
}

// ---- google.cloud.location.Locations ----

type locationsServer struct {
	locationpb.UnimplementedLocationsServer
	s *Server
}

func registerLocations(g grpc.ServiceRegistrar, s *Server) {
	locations.Register(g, &locationsServer{s: s})
}

// Regions are the locations listed: Google Cloud's regions. Any
// well-formed location is accepted in a name, and one holding a pool is
// listed too.
var Regions = locations.Regions

func (l *locationsServer) ids(project string) []string {
	seen := map[string]bool{}
	for _, r := range Regions {
		seen[r] = true
	}
	l.s.mu.Lock()
	for k := range l.s.pools {
		if rest, ok := strings.CutPrefix(k, project+"/locations/"); ok {
			id, _, _ := strings.Cut(rest, "/")
			seen[id] = true
		}
	}
	l.s.mu.Unlock()
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func (l *locationsServer) ListLocations(_ context.Context, req *locationpb.ListLocationsRequest) (*locationpb.ListLocationsResponse, error) {
	if !projectRE.MatchString(req.GetName()) {
		return nil, apierror.InvalidArgument("name %q must be projects/{project}", req.GetName())
	}
	mk := func(id string) *locationpb.Location {
		return &locationpb.Location{Name: req.GetName() + "/locations/" + id, LocationId: id, DisplayName: id}
	}
	ids, err := aipfilter.LocationIDs(req.GetFilter(), l.ids(req.GetName()), func(id string) (*locationpb.Location, error) { return mk(id), nil })
	if err != nil {
		return nil, err
	}
	out, next, err := page("privateca-locations:"+req.GetName()+"|"+req.GetFilter(), ids, req.GetPageToken(), req.GetPageSize(), mk)
	if err != nil {
		return nil, err
	}
	return &locationpb.ListLocationsResponse{Locations: out, NextPageToken: next}, nil
}

func (l *locationsServer) GetLocation(_ context.Context, req *locationpb.GetLocationRequest) (*locationpb.Location, error) {
	if err := checkLocation("name", req.GetName()); err != nil {
		return nil, err
	}
	project, id, _ := strings.Cut(req.GetName(), "/locations/")
	for _, x := range l.ids(project) {
		if x == id {
			return &locationpb.Location{Name: req.GetName(), LocationId: id, DisplayName: id}, nil
		}
	}
	return nil, apierror.NotFound("location %s not found", req.GetName())
}

// ---- google.iam.v1.IAMPolicy ----

// iamServer keeps a policy on each CA pool, never enforced (ADR-0006).
// privateca_v1.yaml binds IAM on caPools, certificateTemplates and
// certificateRevocationLists.
type iamServer struct {
	iampb.UnimplementedIAMPolicyServer
	s *Server
}

func registerIAM(g grpc.ServiceRegistrar, s *Server) {
	iampb.RegisterIAMPolicyServer(g, &iamServer{s: s})
}

// IAMPolicy is the IAM mixin, for the console.
func (s *Server) IAMPolicy() iampb.IAMPolicyServer { return &iamServer{s: s} }

// target is where resource's policy is kept: a pool's, a template's or a
// CRL's.
func (i *iamServer) target(resource string) (**iampolicy.Stored, error) {
	switch {
	case strings.Contains(resource, "/certificateTemplates/"):
		t, err := i.s.template(resource)
		if err != nil {
			return nil, err
		}
		return &t.Policy, nil
	case strings.Contains(resource, "/certificateRevocationLists/"):
		if _, err := i.s.crl(resource); err != nil {
			return nil, err
		}
		p := i.s.crlPolicies[resource]
		if p == nil {
			p = new(*iampolicy.Stored)
			i.s.crlPolicies[resource] = p
		}
		return p, nil
	}
	p, err := i.s.pool(resource)
	if err != nil {
		return nil, err
	}
	return &p.Policy, nil
}

func (i *iamServer) GetIamPolicy(_ context.Context, req *iampb.GetIamPolicyRequest) (*iampb.Policy, error) {
	i.s.mu.Lock()
	defer i.s.mu.Unlock()
	p, err := i.target(req.GetResource())
	if err != nil {
		return nil, err
	}
	out, err := iampolicy.Get(*p, req.GetOptions())
	return out, apierror.Wrap(err)
}

func (i *iamServer) SetIamPolicy(_ context.Context, req *iampb.SetIamPolicyRequest) (*iampb.Policy, error) {
	i.s.mu.Lock()
	defer i.s.mu.Unlock()
	p, err := i.target(req.GetResource())
	if err != nil {
		return nil, err
	}
	next, err := iampolicy.SetConditional(*p, req)
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	*p = &next
	if err := i.s.save(); err != nil {
		return nil, apierror.Wrap(err)
	}
	return next.Proto(), nil
}

func (i *iamServer) TestIamPermissions(_ context.Context, req *iampb.TestIamPermissionsRequest) (*iampb.TestIamPermissionsResponse, error) {
	i.s.mu.Lock()
	defer i.s.mu.Unlock()
	if _, err := i.target(req.GetResource()); err != nil {
		return nil, err
	}
	return iampolicy.Permissions(req), nil
}

// ---- JSON API ----

// mixins are the bindings privateca_v1.yaml adds: Locations, Operations
// and IAMPolicy.
var mixins = []*annotations.HttpRule{
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

func init() {
	for _, res := range []string{"caPools/*", "certificateTemplates/*", "caPools/*/certificateAuthorities/*/certificateRevocationLists/*"} {
		base := "/v1/{resource=projects/*/locations/*/" + res + "}"
		mixins = append(mixins,
			&annotations.HttpRule{Selector: "google.iam.v1.IAMPolicy.GetIamPolicy", Pattern: &annotations.HttpRule_Get{Get: base + ":getIamPolicy"}},
			&annotations.HttpRule{Selector: "google.iam.v1.IAMPolicy.SetIamPolicy", Pattern: &annotations.HttpRule_Post{Post: base + ":setIamPolicy"}, Body: "*"},
			&annotations.HttpRule{Selector: "google.iam.v1.IAMPolicy.TestIamPermissions", Pattern: &annotations.HttpRule_Post{Post: base + ":testIamPermissions"}, Body: "*"})
	}
}

// Transcoder is the JSON API's configuration, with nothing registered.
func Transcoder() *rest.Transcoder {
	return &rest.Transcoder{Packages: []string{"google.cloud.security.privateca.v1"}, Mixins: mixins, SnakeCaseMasks: true}
}

// Routes is every bound route.
func Routes() []rest.Route { return Transcoder().Routes() }

// NewRESTHandler is the JSON API over s, on the gRPC port as
// privateca.googleapis.com serves both.
func NewRESTHandler(s *Server) http.Handler {
	t := Transcoder()
	s.Register(t)
	return s.contentHandler(t)
}
