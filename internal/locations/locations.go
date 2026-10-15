// Package locations registers the google.cloud.location.Locations mixin a
// service binds (cloudscheduler_v1.yaml, cloudkms_v1.yaml). genproto's
// RegisterLocationsServer takes only a *grpc.Server, and its service
// description is unexported, so the description is written here, for any
// grpc.ServiceRegistrar: a gRPC server, or the JSON transcoder.
package locations

import (
	"context"

	locationpb "google.golang.org/genproto/googleapis/cloud/location"
	"google.golang.org/grpc"
)

// Register adds srv to g as google.cloud.location.Locations.
func Register(g grpc.ServiceRegistrar, srv locationpb.LocationsServer) {
	g.RegisterService(&desc, srv)
}

func unary[Req any, Resp any](method string, call func(locationpb.LocationsServer, context.Context, *Req) (Resp, error)) grpc.MethodDesc {
	return grpc.MethodDesc{
		MethodName: method,
		Handler: func(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
			in := new(Req)
			if err := dec(in); err != nil {
				return nil, err
			}
			l := srv.(locationpb.LocationsServer)
			if interceptor == nil {
				return call(l, ctx, in)
			}
			info := &grpc.UnaryServerInfo{Server: srv, FullMethod: "/google.cloud.location.Locations/" + method}
			return interceptor(ctx, in, info, func(ctx context.Context, req any) (any, error) { return call(l, ctx, req.(*Req)) })
		},
	}
}

var desc = grpc.ServiceDesc{
	ServiceName: "google.cloud.location.Locations",
	HandlerType: (*locationpb.LocationsServer)(nil),
	Methods: []grpc.MethodDesc{
		unary("ListLocations", locationpb.LocationsServer.ListLocations),
		unary("GetLocation", locationpb.LocationsServer.GetLocation),
	},
	Metadata: "google/cloud/location/locations.proto",
}
