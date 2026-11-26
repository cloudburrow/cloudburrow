package datamigration

import (
	"context"
	"net/http"
	"time"

	dmspb "cloud.google.com/go/clouddms/apiv1/clouddmspb"
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

// Register serves the API and its Operations, Locations and IAMPolicy
// mixins on g, a gRPC server or the JSON transcoder.
func (s *Server) Register(g grpc.ServiceRegistrar) {
	dmspb.RegisterDataMigrationServiceServer(g, s)
	longrunningpb.RegisterOperationsServer(g, &operations{s: s})
	locations.Register(g, &locationsServer{})
	iampb.RegisterIAMPolicyServer(g, &iamServer{s: s})
}

func pageStrings(scope string, items []string, token string, size int32) ([]string, string, error) {
	out, next, err := aipfilter.Page(scope, items, token, int(size))
	if err != nil {
		return nil, "", apierror.InvalidArgument("invalid page_token")
	}
	return out, next, nil
}

// ---- google.longrunning.Operations ----

type operations struct {
	longrunningpb.UnimplementedOperationsServer
	s *Server
}

func (o *operations) GetOperation(_ context.Context, req *longrunningpb.GetOperationRequest) (*longrunningpb.Operation, error) {
	if opRE.FindStringSubmatch(req.GetName()) == nil {
		return nil, apierror.InvalidArgument("name %q must be projects/{project}/locations/{location}/operations/{operation}", req.GetName())
	}
	o.s.mu.Lock()
	defer o.s.mu.Unlock()
	op, ok := o.s.ops[req.GetName()]
	if !ok {
		return nil, apierror.NotFound("Operation '%s' was not found", req.GetName())
	}
	return clone(op), nil
}

// WaitOperation waits until the operation is done or the timeout (at most
// a minute when none is given) passes.
func (o *operations) WaitOperation(ctx context.Context, req *longrunningpb.WaitOperationRequest) (*longrunningpb.Operation, error) {
	wait := time.Minute
	if d := req.GetTimeout(); d != nil && d.AsDuration() > 0 {
		wait = d.AsDuration()
	}
	deadline := time.Now().Add(wait)
	for {
		op, err := o.GetOperation(ctx, &longrunningpb.GetOperationRequest{Name: req.GetName()})
		if err != nil || op.GetDone() || time.Now().After(deadline) {
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
	if _, _, err := checkParent("name", req.GetName()); err != nil {
		return nil, err
	}
	o.s.mu.Lock()
	var all []*longrunningpb.Operation
	for _, k := range sortedKeys(o.s.ops, req.GetName()+"/operations/") {
		all = append(all, clone(o.s.ops[k]))
	}
	o.s.mu.Unlock()
	out, next, err := listPage("dms-ops:"+req.GetName(), req.GetFilter(), "", all, req.GetPageToken(), req.GetPageSize())
	if err != nil {
		return nil, err
	}
	return &longrunningpb.ListOperationsResponse{Operations: out, NextPageToken: next}, nil
}

func (o *operations) DeleteOperation(_ context.Context, req *longrunningpb.DeleteOperationRequest) (*emptypb.Empty, error) {
	o.s.mu.Lock()
	defer o.s.mu.Unlock()
	op, ok := o.s.ops[req.GetName()]
	if !ok {
		return nil, apierror.NotFound("Operation '%s' was not found", req.GetName())
	}
	if !op.GetDone() {
		return nil, failedPrecondition("operation %s is not done; it cannot be deleted", req.GetName())
	}
	delete(o.s.ops, req.GetName())
	if err := o.s.save(); err != nil {
		return nil, apierror.Wrap(err)
	}
	return &emptypb.Empty{}, nil
}

// CancelOperation is accepted for a done operation, which it leaves as it
// is; a running one (a destination being created, a job's step against
// the servers) cannot be stopped part-way and is refused (UNVERIFIED).
// Docs re-read 2026-11-25, still unanswered: https://docs.cloud.google.com/database-migration/docs/reference/rest/v1/projects.locations.operations/cancel
func (o *operations) CancelOperation(_ context.Context, req *longrunningpb.CancelOperationRequest) (*emptypb.Empty, error) {
	o.s.mu.Lock()
	defer o.s.mu.Unlock()
	op, ok := o.s.ops[req.GetName()]
	if !ok {
		return nil, apierror.NotFound("Operation '%s' was not found", req.GetName())
	}
	if !op.GetDone() {
		return nil, unimplemented("operations.cancel of a running operation",
			"its step against the destination's server runs to its end; wait for it, then undo it (delete, restart)")
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
	page, next, err := pageStrings("dms-locations:"+req.GetName(), ids, req.GetPageToken(), req.GetPageSize())
	if err != nil {
		return nil, err
	}
	out := &locationpb.ListLocationsResponse{NextPageToken: next}
	for _, id := range page {
		out.Locations = append(out.Locations, mk(id))
	}
	return out, nil
}

func (l *locationsServer) GetLocation(_ context.Context, req *locationpb.GetLocationRequest) (*locationpb.Location, error) {
	m := parentRE.FindStringSubmatch(req.GetName())
	if m == nil {
		return nil, apierror.InvalidArgument("name %q must be projects/{project}/locations/{location}", req.GetName())
	}
	if !validLocation(m[2]) {
		return nil, apierror.NotFound("Location '%s' was not found", req.GetName())
	}
	return &locationpb.Location{Name: req.GetName(), LocationId: m[2], DisplayName: m[2]}, nil
}

// ---- google.iam.v1.IAMPolicy ----

// iamServer keeps a policy on each connection profile, migration job and
// private connection, enforced as the rest of the instance's are
// (ADR-0006).
type iamServer struct {
	iampb.UnimplementedIAMPolicyServer
	s *Server
}

// IAMPolicy is the IAM mixin, for the console and IAM enforcement.
func (s *Server) IAMPolicy() iampb.IAMPolicyServer { return &iamServer{s: s} }

// resource checks a policy's resource exists; the caller holds mu.
func (i *iamServer) resource(name string) error {
	switch {
	case profileRE.MatchString(name):
		_, err := i.s.profileLocked(name)
		return err
	case jobRE.MatchString(name):
		_, err := i.s.jobLocked(name)
		return err
	case privateRE.MatchString(name):
		_, err := i.s.privateLocked(name)
		return err
	}
	return apierror.InvalidArgument("resource %q is not a connection profile, migration job or private connection", name)
}

func (i *iamServer) GetIamPolicy(_ context.Context, req *iampb.GetIamPolicyRequest) (*iampb.Policy, error) {
	i.s.mu.Lock()
	defer i.s.mu.Unlock()
	if err := i.resource(req.GetResource()); err != nil {
		return nil, err
	}
	out, err := iampolicy.Get(i.s.policies[req.GetResource()], req.GetOptions())
	return out, apierror.Wrap(err)
}

func (i *iamServer) SetIamPolicy(_ context.Context, req *iampb.SetIamPolicyRequest) (*iampb.Policy, error) {
	i.s.mu.Lock()
	defer i.s.mu.Unlock()
	if err := i.resource(req.GetResource()); err != nil {
		return nil, err
	}
	next, err := iampolicy.SetConditional(i.s.policies[req.GetResource()], req)
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	i.s.policies[req.GetResource()] = &next
	if err := i.s.save(); err != nil {
		return nil, apierror.Wrap(err)
	}
	return next.Proto(), nil
}

func (i *iamServer) TestIamPermissions(_ context.Context, req *iampb.TestIamPermissionsRequest) (*iampb.TestIamPermissionsResponse, error) {
	i.s.mu.Lock()
	defer i.s.mu.Unlock()
	if err := i.resource(req.GetResource()); err != nil {
		return nil, err
	}
	return iampolicy.Permissions(req), nil
}

// ---- JSON API ----

// mixins are the bindings datamigration_v1.yaml adds (Locations,
// Operations and IAMPolicy on connection profiles, migration jobs, private
// connections, conversion workspaces and migration job objects), and the
// REST reference's methods clouddmspb v1.16.1 does not have
// (migrationJobs.demoteDestination, .fetchSourceObjects and .objects),
// whose selectors name the permission checks (iameval.DataMigration) and
// which ServeJobJSON answers (#2257); the objects' IAM policy methods
// resolve to nothing and so answer 501.
func mixins() []*annotations.HttpRule {
	out := []*annotations.HttpRule{
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
	for _, coll := range []string{"connectionProfiles", "migrationJobs", "privateConnections", "conversionWorkspaces", "migrationJobs/*/objects"} {
		sel := "google.iam.v1.IAMPolicy."
		if coll == "conversionWorkspaces" || coll == "migrationJobs/*/objects" {
			sel = "google.cloud.clouddms.v1.Unserved."
		}
		r := "/v1/{resource=projects/*/locations/*/" + coll + "/*}"
		out = append(out,
			&annotations.HttpRule{Selector: sel + "GetIamPolicy", Pattern: &annotations.HttpRule_Get{Get: r + ":getIamPolicy"}},
			&annotations.HttpRule{Selector: sel + "SetIamPolicy", Pattern: &annotations.HttpRule_Post{Post: r + ":setIamPolicy"}, Body: "*"},
			&annotations.HttpRule{Selector: sel + "TestIamPermissions", Pattern: &annotations.HttpRule_Post{Post: r + ":testIamPermissions"}, Body: "*"})
	}
	const unserved = "google.cloud.clouddms.v1.DataMigrationService."
	out = append(out,
		&annotations.HttpRule{Selector: unserved + "DemoteDestination",
			Pattern: &annotations.HttpRule_Post{Post: "/v1/{name=projects/*/locations/*/migrationJobs/*}:demoteDestination"}, Body: "*"},
		&annotations.HttpRule{Selector: unserved + "FetchSourceObjects",
			Pattern: &annotations.HttpRule_Get{Get: "/v1/{name=projects/*/locations/*/migrationJobs/*}:fetchSourceObjects"}},
		&annotations.HttpRule{Selector: unserved + "GetMigrationJobObject",
			Pattern: &annotations.HttpRule_Get{Get: "/v1/{name=projects/*/locations/*/migrationJobs/*/objects/*}"}},
		&annotations.HttpRule{Selector: unserved + "ListMigrationJobObjects",
			Pattern: &annotations.HttpRule_Get{Get: "/v1/{parent=projects/*/locations/*/migrationJobs/*}/objects"}},
		&annotations.HttpRule{Selector: unserved + "LookupMigrationJobObject",
			Pattern: &annotations.HttpRule_Post{Post: "/v1/{parent=projects/*/locations/*/migrationJobs/*}/objects:lookup"}, Body: "*"})
	return out
}

// Mixins are the JSON API's extra bindings.
func Mixins() []*annotations.HttpRule { return mixins() }

// Transcoder is the JSON API's configuration, with nothing registered.
func Transcoder() *rest.Transcoder {
	return &rest.Transcoder{Packages: []string{"google.cloud.clouddms.v1"}, Mixins: mixins(), SnakeCaseMasks: true, Overlay: overlay}
}

// NewRESTHandler is the JSON API over s, on the gRPC port: the
// transcoder, and ServeJobJSON's methods clouddmspb has no gRPC method for
// (#2257).
func NewRESTHandler(s *Server) http.Handler {
	t := Transcoder()
	s.Register(t)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.ServeJobJSON(w, r) {
			return
		}
		t.ServeHTTP(w, r)
	})
}

// Operation is the operation named name, for the console.
func (s *Server) Operation(name string) (*longrunningpb.Operation, error) {
	return (&operations{s: s}).GetOperation(context.Background(), &longrunningpb.GetOperationRequest{Name: name})
}
