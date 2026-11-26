package alloydb

import (
	"context"
	"net/http"
	"strings"

	alloydbpb "cloud.google.com/go/alloydb/apiv1/alloydbpb"
	alphapb "cloud.google.com/go/alloydb/apiv1alpha/alloydbpb"
	betapb "cloud.google.com/go/alloydb/apiv1beta/alloydbpb"
	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/genproto/googleapis/api/annotations"
	locationpb "google.golang.org/genproto/googleapis/cloud/location"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/cloudburrow/cloudburrow/internal/aipfilter"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/locations"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// Versions are the API versions served: v1, and v1beta and v1alpha, whose
// methods are v1's (the connectors call v1alpha).
var Versions = []string{"v1", "v1beta", "v1alpha"}

// Register serves AlloyDB v1, v1beta and v1alpha and the Operations and
// Locations mixins on g, a gRPC server or the JSON transcoder.
func (s *Server) Register(g grpc.ServiceRegistrar) {
	alloydbpb.RegisterAlloyDBAdminServer(g, s)
	alloydbpb.RegisterAlloyDBCSQLAdminServer(g, &alloydbpb.UnimplementedAlloyDBCSQLAdminServer{})
	for _, v := range []string{"v1beta", "v1alpha"} {
		g.RegisterService(s.bridge(v), s)
	}
	longrunningpb.RegisterOperationsServer(g, &operations{s: s})
	locations.Register(g, &locationsServer{})
}

// The v1beta and v1alpha packages are imported so their descriptors are
// registered: bridge reads their methods from them.
var _ = []any{alphapb.AlloyDBAdmin_ServiceDesc, betapb.AlloyDBAdmin_ServiceDesc}

// bridge is the AlloyDBAdmin service of version (v1beta or v1alpha), whose
// methods are v1's: a request is re-read as v1's message of the same
// name, and the answer as the version's. The messages share their field
// numbers; a field only the version has is kept as an unknown field, which
// is written back. A long-running operation's messages are named in the
// version, and the operation is recorded as the version's, so polling it
// answers the same names.
func (s *Server) bridge(version string) *grpc.ServiceDesc {
	full := protoreflect.FullName("google.cloud.alloydb." + version + ".AlloyDBAdmin")
	d, err := protoregistry.GlobalFiles.FindDescriptorByName(full)
	if err != nil {
		panic(err)
	}
	sd := d.(protoreflect.ServiceDescriptor)
	out := &grpc.ServiceDesc{ServiceName: string(full), HandlerType: (*any)(nil), Metadata: sd.ParentFile().Path()}
	for _, m := range alloydbpb.AlloyDBAdmin_ServiceDesc.Methods {
		md := sd.Methods().ByName(protoreflect.Name(m.MethodName))
		if md == nil {
			continue
		}
		v1 := m
		in, outT := md.Input().FullName(), md.Output().FullName()
		out.Methods = append(out.Methods, grpc.MethodDesc{MethodName: m.MethodName,
			Handler: func(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
				it, err := protoregistry.GlobalTypes.FindMessageByName(in)
				if err != nil {
					return nil, apierror.Internal(err, "request type")
				}
				req := it.New().Interface()
				if err := dec(req); err != nil {
					return nil, err
				}
				b, err := proto.Marshal(req)
				if err != nil {
					return nil, apierror.Internal(err, "re-encode the request")
				}
				resp, err := v1.Handler(s, ctx, func(x any) error { return proto.Unmarshal(b, x.(proto.Message)) }, interceptor)
				if err != nil {
					return nil, err
				}
				return s.toVersion(version, v1.MethodName, req, resp.(proto.Message), outT)
			}})
	}
	return out
}

// toVersion re-reads resp, a v1 message, as version's message outT.
func (s *Server) toVersion(version, method string, req proto.Message, resp proto.Message, outT protoreflect.FullName) (proto.Message, error) {
	if op, ok := resp.(*longrunningpb.Operation); ok {
		s.mu.Lock()
		if _, known := s.ops[op.GetName()]; known {
			s.versions[op.GetName()] = version
			_ = s.save()
		}
		s.mu.Unlock()
		return renameOp(op, version), nil
	}
	ot, err := protoregistry.GlobalTypes.FindMessageByName(outT)
	if err != nil {
		return nil, apierror.Internal(err, "response type")
	}
	b, err := proto.Marshal(resp)
	if err != nil {
		return nil, apierror.Internal(err, "re-encode the response")
	}
	out := ot.New().Interface()
	if err := proto.Unmarshal(b, out); err != nil {
		return nil, apierror.Internal(err, "re-read the response")
	}
	if method == "GetConnectionInfo" {
		// v1beta and v1alpha's ConnectionInfo has the PSC DNS name.
		name := req.ProtoReflect().Get(req.ProtoReflect().Descriptor().Fields().ByName("parent")).String()
		s.mu.Lock()
		dns := s.instances[name].GetPscInstanceConfig().GetPscDnsName()
		s.mu.Unlock()
		if f := out.ProtoReflect().Descriptor().Fields().ByName("psc_dns_name"); f != nil && dns != "" {
			out.ProtoReflect().Set(f, protoreflect.ValueOfString(dns))
		}
	}
	return out, nil
}

// renameOp names op's messages in version.
func renameOp(op *longrunningpb.Operation, version string) *longrunningpb.Operation {
	if version == "" || version == "v1" {
		return op
	}
	op = clone(op)
	rename := func(a *anypb.Any) {
		if a != nil {
			a.TypeUrl = strings.Replace(a.TypeUrl, "google.cloud.alloydb.v1.", "google.cloud.alloydb."+version+".", 1)
		}
	}
	rename(op.GetMetadata())
	rename(op.GetResponse())
	return op
}

// ---- google.longrunning.Operations ----

type operations struct {
	longrunningpb.UnimplementedOperationsServer
	s *Server
}

func (o *operations) GetOperation(_ context.Context, req *longrunningpb.GetOperationRequest) (*longrunningpb.Operation, error) {
	o.s.mu.Lock()
	defer o.s.mu.Unlock()
	op, ok := o.s.ops[req.GetName()]
	if !ok {
		return nil, apierror.NotFound("Operation %s not found", req.GetName())
	}
	return renameOp(clone(op), o.s.versions[op.GetName()]), nil
}

func (o *operations) WaitOperation(ctx context.Context, req *longrunningpb.WaitOperationRequest) (*longrunningpb.Operation, error) {
	o.s.waitOps(ctx, []string{req.GetName()})
	return o.GetOperation(ctx, &longrunningpb.GetOperationRequest{Name: req.GetName()})
}

func (o *operations) ListOperations(_ context.Context, req *longrunningpb.ListOperationsRequest) (*longrunningpb.ListOperationsResponse, error) {
	if _, _, err := checkParent("name", req.GetName(), true); err != nil {
		return nil, err
	}
	o.s.mu.Lock()
	var all []*longrunningpb.Operation
	for _, k := range sortedKeys(o.s.ops, "") {
		if inLocation(k, req.GetName()) {
			all = append(all, renameOp(clone(o.s.ops[k]), o.s.versions[k]))
		}
	}
	o.s.mu.Unlock()
	out, next, err := listPage("alloydb-ops:"+req.GetName(), req.GetFilter(), "", all, req.GetPageToken(), req.GetPageSize())
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
		return nil, apierror.NotFound("Operation %s not found", req.GetName())
	}
	if !op.GetDone() {
		return nil, apierror.FailedPrecondition("operation %s is still running", req.GetName())
	}
	delete(o.s.ops, req.GetName())
	delete(o.s.versions, req.GetName())
	return &emptypb.Empty{}, apierror.Wrap(o.s.save())
}

// CancelOperation of a running operation is refused: the servers' work is
// not cancelled half-way. UNVERIFIED: which of AlloyDB's operations Google
// cancels.
// (re-read 2026-11-23: docs.cloud.google.com/alloydb/docs/reference/rest.)
func (o *operations) CancelOperation(_ context.Context, req *longrunningpb.CancelOperationRequest) (*emptypb.Empty, error) {
	o.s.mu.Lock()
	defer o.s.mu.Unlock()
	op, ok := o.s.ops[req.GetName()]
	if !ok {
		return nil, apierror.NotFound("Operation %s not found", req.GetName())
	}
	if op.GetDone() {
		return &emptypb.Empty{}, nil
	}
	return nil, apierror.FailedPrecondition("operation %s cannot be cancelled", req.GetName())
}

// ---- google.cloud.location.Locations ----

type locationsServer struct {
	locationpb.UnimplementedLocationsServer
}

func location(project, id string) *locationpb.Location {
	return &locationpb.Location{Name: "projects/" + project + "/locations/" + id, LocationId: id, DisplayName: id}
}

func (l *locationsServer) ListLocations(_ context.Context, req *locationpb.ListLocationsRequest) (*locationpb.ListLocationsResponse, error) {
	m := projectRE.FindStringSubmatch(req.GetName())
	if m == nil {
		return nil, apierror.InvalidArgument("name %q must be projects/{project}", req.GetName())
	}
	ids, err := aipfilter.LocationIDs(req.GetFilter(), Locations(), func(id string) (*locationpb.Location, error) { return location(m[1], id), nil })
	if err != nil {
		return nil, err
	}
	var all []*locationpb.Location
	for _, id := range ids {
		all = append(all, location(m[1], id))
	}
	out, next, err := listPage("alloydb-locations:"+req.GetName(), "", "", all, req.GetPageToken(), req.GetPageSize())
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
	return location(m[1], m[2]), nil
}

// ---- JSON API ----

// mixins are the bindings alloydb_v1.yaml (and its v1beta and v1alpha)
// add: Locations and Operations.
func mixins() []*annotations.HttpRule {
	var out []*annotations.HttpRule
	for _, v := range Versions {
		out = append(out,
			&annotations.HttpRule{Selector: "google.cloud.location.Locations.GetLocation",
				Pattern: &annotations.HttpRule_Get{Get: "/" + v + "/{name=projects/*/locations/*}"}},
			&annotations.HttpRule{Selector: "google.cloud.location.Locations.ListLocations",
				Pattern: &annotations.HttpRule_Get{Get: "/" + v + "/{name=projects/*}/locations"}},
			&annotations.HttpRule{Selector: "google.longrunning.Operations.CancelOperation",
				Pattern: &annotations.HttpRule_Post{Post: "/" + v + "/{name=projects/*/locations/*/operations/*}:cancel"}, Body: "*"},
			&annotations.HttpRule{Selector: "google.longrunning.Operations.DeleteOperation",
				Pattern: &annotations.HttpRule_Delete{Delete: "/" + v + "/{name=projects/*/locations/*/operations/*}"}},
			&annotations.HttpRule{Selector: "google.longrunning.Operations.GetOperation",
				Pattern: &annotations.HttpRule_Get{Get: "/" + v + "/{name=projects/*/locations/*/operations/*}"}},
			&annotations.HttpRule{Selector: "google.longrunning.Operations.ListOperations",
				Pattern: &annotations.HttpRule_Get{Get: "/" + v + "/{name=projects/*/locations/*}/operations"}},
		)
	}
	return out
}

// Transcoder is the JSON API's configuration, with nothing registered.
func Transcoder() *rest.Transcoder {
	return &rest.Transcoder{Packages: []string{"google.cloud.alloydb.v1", "google.cloud.alloydb.v1beta", "google.cloud.alloydb.v1alpha"},
		Mixins: mixins(), SnakeCaseMasks: true, Overlay: alloydbOverlay}
}

// NewRESTHandler is the JSON API over s, on the gRPC port.
func NewRESTHandler(s *Server) http.Handler {
	t := Transcoder()
	s.Register(t)
	// The opt-in's headers (#2207) reach the methods as gRPC metadata does.
	return restHeaders(t)
}
