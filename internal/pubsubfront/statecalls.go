package pubsubfront

import (
	"net/http"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// StateHeader is the gRPC metadata key CloudBurrow's own `state save`,
// `state load` and `state reset` set on the calls they make through the
// front. The disabled-API gate (#1914) lets those calls through: a
// project whose Pub/Sub API is disabled, as unlinking its billing account
// does (#2109) or `gcloud services disable` does, still holds its topics
// and subscriptions, and a state save must capture them, a load replace them
// and a reset clear them, rather than fail the whole archive on that one
// project. A client of the API never sends it; every other check, and
// every call without it, is unchanged.
const StateHeader = "x-cloudburrow-state"

// stateCallHTTP is whether r is one of those calls, as the HTTP gate sees
// it (gRPC metadata arrives as headers).
func stateCallHTTP(r *http.Request) bool { return r.Header.Get(StateHeader) != "" }

// skipForStateCalls runs gate except for the state calls, which go
// straight to the handler.
func skipForStateCalls(gate grpc.StreamServerInterceptor) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if md, ok := metadata.FromIncomingContext(ss.Context()); ok && len(md.Get(StateHeader)) > 0 {
			return handler(srv, ss)
		}
		return gate(srv, ss, info, handler)
	}
}
