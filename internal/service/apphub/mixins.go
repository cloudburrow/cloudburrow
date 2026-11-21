package apphub

import (
	"context"
	"embed"
	"net/http"
	"sort"
	"strings"

	apphubpb "cloud.google.com/go/apphub/apiv1/apphubpb"
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

// Register serves App Hub and its Operations, Locations and IAMPolicy
// mixins on g, a gRPC server or the JSON transcoder.
func (s *Server) Register(g grpc.ServiceRegistrar) {
	apphubpb.RegisterAppHubServer(g, s)
	longrunningpb.RegisterOperationsServer(g, &operations{s: s})
	locations.Register(g, &locationsServer{})
	iampb.RegisterIAMPolicyServer(g, &iamServer{s: s})
}

// ---- extended metadata schemas ----

// The schemas Google's "Extended metadata schemas" page defines
// (docs.cloud.google.com/app-hub/docs/metadata-schemas, read 2026-11-21),
// copied from it. It gives no schema version: 1 is UNVERIFIED.
//
//go:embed schemas/*.json
var schemaFiles embed.FS

var schemaKeys = []struct{ key, file string }{
	{"apphub.googleapis.com/AgentProperties", "schemas/agentproperties.json"},
	{"apphub.googleapis.com/McpServer", "schemas/mcpserver.json"},
	{"discoveryengine.googleapis.com/agentregistry/connector", "schemas/agentregistryconnector.json"},
}

func schema(parent, key, file string) *apphubpb.ExtendedMetadataSchema {
	b, _ := schemaFiles.ReadFile(file)
	return &apphubpb.ExtendedMetadataSchema{Name: parent + "/extendedMetadataSchemas/" + key, JsonSchema: string(b), SchemaVersion: 1}
}

func (s *Server) ListExtendedMetadataSchemas(_ context.Context, req *apphubpb.ListExtendedMetadataSchemasRequest) (*apphubpb.ListExtendedMetadataSchemasResponse, error) {
	if _, _, err := checkParent("parent", req.GetParent()); err != nil {
		return nil, err
	}
	var all []*apphubpb.ExtendedMetadataSchema
	for _, k := range schemaKeys {
		all = append(all, schema(req.GetParent(), k.key, k.file))
	}
	out, next, err := listPage("apphub-schemas:"+req.GetParent(), "", "", all, req.GetPageToken(), req.GetPageSize())
	if err != nil {
		return nil, err
	}
	return &apphubpb.ListExtendedMetadataSchemasResponse{ExtendedMetadataSchemas: out, NextPageToken: next}, nil
}

func (s *Server) GetExtendedMetadataSchema(_ context.Context, req *apphubpb.GetExtendedMetadataSchemaRequest) (*apphubpb.ExtendedMetadataSchema, error) {
	m := schemaRE.FindStringSubmatch(req.GetName())
	if m == nil {
		return nil, apierror.InvalidArgument("name %q must be projects/{project}/locations/{location}/extendedMetadataSchemas/{schema}", req.GetName())
	}
	if !validLocation(m[2]) {
		return nil, apierror.InvalidArgument("location %q is not an App Hub location", m[2])
	}
	for _, k := range schemaKeys {
		if k.key == m[3] {
			return schema("projects/"+m[1]+"/locations/"+m[2], k.key, k.file), nil
		}
	}
	return nil, apierror.NotFound("ExtendedMetadataSchema %s not found", req.GetName())
}

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
	if _, _, err := checkParent("name", req.GetName()); err != nil {
		return nil, err
	}
	o.s.mu.Lock()
	var all []*longrunningpb.Operation
	for _, k := range sortedKeys(o.s.ops, req.GetName()+"/operations/") {
		all = append(all, clone(o.s.ops[k]))
	}
	o.s.mu.Unlock()
	out, next, err := listPage("apphub-ops:"+req.GetName(), req.GetFilter(), "", all, req.GetPageToken(), req.GetPageSize())
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

// CancelOperation of a done operation does nothing, as AIP-151 has it:
// every operation here is done when it is returned.
func (o *operations) CancelOperation(ctx context.Context, req *longrunningpb.CancelOperationRequest) (*emptypb.Empty, error) {
	if _, err := o.GetOperation(ctx, &longrunningpb.GetOperationRequest{Name: req.GetName()}); err != nil {
		return nil, err
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
	out, next, err := listPage("apphub-locations:"+req.GetName(), "", "", all, req.GetPageToken(), req.GetPageSize())
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

// ---- google.iam.v1.IAMPolicy ----

// iamServer keeps a policy on each application, enforced as the rest of
// the instance's are (ADR-0006). apphub_v1.yaml binds IAM on applications.
type iamServer struct {
	iampb.UnimplementedIAMPolicyServer
	s *Server
}

// IAMPolicy is the IAM mixin, for the console and IAM enforcement.
func (s *Server) IAMPolicy() iampb.IAMPolicyServer { return &iamServer{s: s} }

func (i *iamServer) GetIamPolicy(_ context.Context, req *iampb.GetIamPolicyRequest) (*iampb.Policy, error) {
	i.s.mu.Lock()
	defer i.s.mu.Unlock()
	if _, err := i.s.app(req.GetResource()); err != nil {
		return nil, err
	}
	out, err := iampolicy.Get(i.s.policies[req.GetResource()], req.GetOptions())
	return out, apierror.Wrap(err)
}

func (i *iamServer) SetIamPolicy(_ context.Context, req *iampb.SetIamPolicyRequest) (*iampb.Policy, error) {
	i.s.mu.Lock()
	defer i.s.mu.Unlock()
	if _, err := i.s.app(req.GetResource()); err != nil {
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
	if _, err := i.s.app(req.GetResource()); err != nil {
		return nil, err
	}
	return iampolicy.Permissions(req), nil
}

// ---- App Hub metadata for Cloud Logging and Monitoring ----

// Ref is the App Hub metadata of a resource registered to an application,
// as Cloud Logging's LogEntry.apphub (AppHub: application {container,
// location, id} and one of service or workload {id, environment_type,
// criticality_type}) and Cloud Monitoring's apphub_* labels describe it
// (logging/docs/reference/v2/rest/v2/LogEntry,
// monitoring/docs/application-monitoring).
type Ref struct {
	// Container is the application's host project, projects/{project}.
	Container string
	// Location and ApplicationID are the application's.
	Location, ApplicationID string
	// ServiceID or WorkloadID is the registration's ID, the other empty.
	ServiceID, WorkloadID string
	// Environment and Criticality are the registration's attribute types
	// (PRODUCTION, MISSION_CRITICAL, ...), or "".
	Environment, Criticality string
}

// RefsFor are the registrations of the resource with uri, ordered by name:
// LogEntry.apphub "may be empty if there is no associated AppHub
// application or multiple associated applications", which is for the
// caller to decide.
func (s *Server) RefsFor(uri string) []Ref {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Ref
	add := func(name string, a *apphubpb.Attributes, workload bool) {
		m := appRE.FindStringSubmatch(parentOf(name, map[bool]string{false: "/services/", true: "/workloads/"}[workload]))
		r := Ref{Container: "projects/" + m[1], Location: m[2], ApplicationID: m[3]}
		id := name[strings.LastIndex(name, "/")+1:]
		if workload {
			r.WorkloadID = id
		} else {
			r.ServiceID = id
		}
		if t := a.GetEnvironment().GetType(); t != apphubpb.Environment_TYPE_UNSPECIFIED {
			r.Environment = t.String()
		}
		if t := a.GetCriticality().GetType(); t != apphubpb.Criticality_TYPE_UNSPECIFIED {
			r.Criticality = t.String()
		}
		out = append(out, r)
	}
	for k, svc := range s.services {
		if svc.GetServiceReference().GetUri() == uri {
			add(k, svc.GetAttributes(), false)
		}
	}
	for k, w := range s.workloads {
		if w.GetWorkloadReference().GetUri() == uri {
			add(k, w.GetAttributes(), true)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Container+out[i].Location+out[i].ApplicationID+out[i].ServiceID+out[i].WorkloadID <
			out[j].Container+out[j].Location+out[j].ApplicationID+out[j].ServiceID+out[j].WorkloadID
	})
	return out
}

// Labels are r's Cloud Monitoring labels, as application-monitoring's
// filters name them (apphub_application_location, apphub_application_id,
// apphub_service_id); the workload's label, by the same pattern, is
// UNVERIFIED (the labels reference was not read).
func (r Ref) Labels() map[string]string {
	out := map[string]string{"apphub_application_location": r.Location, "apphub_application_id": r.ApplicationID}
	if r.ServiceID != "" {
		out["apphub_service_id"] = r.ServiceID
	}
	if r.WorkloadID != "" {
		out["apphub_workload_id"] = r.WorkloadID
	}
	return out
}

// ---- JSON API ----

// mixins are the bindings apphub_v1.yaml adds: Locations, Operations and
// IAMPolicy on applications.
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
	{Selector: "google.iam.v1.IAMPolicy.GetIamPolicy",
		Pattern: &annotations.HttpRule_Get{Get: "/v1/{resource=projects/*/locations/*/applications/*}:getIamPolicy"}},
	{Selector: "google.iam.v1.IAMPolicy.SetIamPolicy",
		Pattern: &annotations.HttpRule_Post{Post: "/v1/{resource=projects/*/locations/*/applications/*}:setIamPolicy"}, Body: "*"},
	{Selector: "google.iam.v1.IAMPolicy.TestIamPermissions",
		Pattern: &annotations.HttpRule_Post{Post: "/v1/{resource=projects/*/locations/*/applications/*}:testIamPermissions"}, Body: "*"},
}

// Mixins are the JSON API's Locations, Operations and IAMPolicy bindings.
func Mixins() []*annotations.HttpRule { return mixins }

// Transcoder is the JSON API's configuration, with nothing registered.
func Transcoder() *rest.Transcoder {
	return &rest.Transcoder{Packages: []string{"google.cloud.apphub.v1"}, Mixins: mixins, SnakeCaseMasks: true}
}

// NewRESTHandler is the JSON API over s, on the gRPC port.
func NewRESTHandler(s *Server) http.Handler {
	t := Transcoder()
	s.Register(t)
	return t
}
