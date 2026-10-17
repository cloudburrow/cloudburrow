package bigtablefront

import (
	"context"
	"net/http"
	"strings"

	"google.golang.org/genproto/googleapis/api/annotations"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	// The admin protos, so their descriptors are registered.
	_ "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	_ "cloud.google.com/go/longrunning/autogen/longrunningpb"

	"github.com/cloudburrow/cloudburrow/internal/calllog"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// The admin REST API (#1440): bigtableadmin.googleapis.com/v2 in JSON, on
// the front's port beside gRPC, as Google serves it beside the gRPC API.
// Terraform's google_bigtable_app_profile and google_bigtable_instance_iam_*,
// and gcloud bigtable, call it rather than gRPC.
//
// It is the shared transcoder (internal/transport/rest) over the same
// methods gRPC reaches: the instance admin and operations methods the front
// serves itself, the Refused ones answered UNIMPLEMENTED, and every other
// table admin method sent to the emulator, so REST has no behaviour of its
// own. A bound method neither the front nor the emulator implements answers
// 501; a path Google does not bind answers 404.

// restPrefix is the path the REST API is served under.
const restPrefix = "/v2/"

// restMixins are the bindings bigtableadmin_v2.yaml adds (googleapis
// google/bigtable/admin/v2/bigtableadmin_v2.yaml, http rules), which no
// adminpb descriptor carries. ListOperations comes before GetOperation,
// whose `**` would match its path too.
var restMixins = []*annotations.HttpRule{
	{Selector: "google.longrunning.Operations.ListOperations", Pattern: &annotations.HttpRule_Get{Get: "/v2/{name=operations/projects/**}/operations"}},
	{Selector: "google.longrunning.Operations.CancelOperation", Pattern: &annotations.HttpRule_Post{Post: "/v2/{name=operations/**}:cancel"}},
	{Selector: "google.longrunning.Operations.DeleteOperation", Pattern: &annotations.HttpRule_Delete{Delete: "/v2/{name=operations/**}"}},
	{Selector: "google.longrunning.Operations.GetOperation", Pattern: &annotations.HttpRule_Get{Get: "/v2/{name=operations/**}"}},
	// Not served by the front: 501, which says the API has the method.
	{Selector: "google.cloud.location.Locations.ListLocations", Pattern: &annotations.HttpRule_Get{Get: "/v2/{name=projects/*}/locations"}},
}

// restServices are the services the REST API transcodes to.
var restServices = []string{
	"google.bigtable.admin.v2.BigtableInstanceAdmin",
	"google.bigtable.admin.v2.BigtableTableAdmin",
	"google.longrunning.Operations",
}

// restHandler is the admin REST API, each call in the front's call log.
func (f *Front) restHandler() http.Handler {
	t := &rest.Transcoder{Packages: []string{"google.bigtable.admin.v2"}, Mixins: restMixins, SnakeCaseMasks: true}
	for _, name := range restServices {
		d, err := protoregistry.GlobalFiles.FindDescriptorByName(protoreflect.FullName(name))
		if err != nil {
			panic("bigtablefront: " + err.Error())
		}
		t.RegisterService(f.restService(d.(protoreflect.ServiceDescriptor)), nil)
	}
	return calllog.REST(&f.calls, t)
}

// restService is a service's unary methods as a grpc.ServiceDesc whose
// handlers call f.call, for the transcoder.
func (f *Front) restService(sd protoreflect.ServiceDescriptor) *grpc.ServiceDesc {
	desc := &grpc.ServiceDesc{ServiceName: string(sd.FullName())}
	for i := 0; i < sd.Methods().Len(); i++ {
		md := sd.Methods().Get(i)
		if md.IsStreamingClient() || md.IsStreamingServer() {
			continue
		}
		full := "/" + string(sd.FullName()) + "/" + string(md.Name())
		in, out := md.Input(), md.Output()
		desc.Methods = append(desc.Methods, grpc.MethodDesc{
			MethodName: string(md.Name()),
			Handler: func(_ any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
				req, err := newMessage(in)
				if err != nil {
					return nil, err
				}
				if err := dec(req); err != nil {
					return nil, err
				}
				return f.call(ctx, full, req, out)
			},
		})
	}
	return desc
}

func newMessage(d protoreflect.MessageDescriptor) (proto.Message, error) {
	mt, err := protoregistry.GlobalTypes.FindMessageByName(d.FullName())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "the message type %s: %v", d.FullName(), err)
	}
	return mt.New().Interface(), nil
}

// call answers one decoded unary call as the gRPC front does (handle):
// served by the front, refused, or sent to the emulator.
func (f *Front) call(ctx context.Context, full string, req proto.Message, out protoreflect.MessageDescriptor) (proto.Message, error) {
	if m, ok := f.served[full]; ok {
		b, err := proto.Marshal(req)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "encode the request: %v", err)
		}
		return m(ctx, b)
	}
	if strings.HasPrefix(full, instanceAdmin) || strings.HasPrefix(full, operations) {
		f.logf("bigtable front: refused %s over REST, which neither the emulator nor the front implements", full)
		return nil, status.Errorf(codes.Unimplemented, "the emulator does not currently support %s", full[strings.LastIndexByte(full, '/')+1:])
	}
	if msg, refused := Refused[full]; refused {
		f.logf("bigtable front: refused %s over REST, which the emulator does not implement", full)
		return nil, status.Error(codes.Unimplemented, msg)
	}
	if b, err := proto.Marshal(req); err == nil {
		f.sawIf(f.seen.seeFrame(b))
	}
	resp, err := newMessage(out)
	if err != nil {
		return nil, err
	}
	if err := f.upstream.Invoke(ctx, full, req, resp, grpc.ForceCodec(rawCodec{})); err != nil {
		return nil, err
	}
	return resp, nil
}
