package grpc

import (
	"context"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Call is one completed RPC, as an observer sees it.
//
// It deliberately carries no request or response. The observer exists to
// answer "did my call arrive, and what came back" — a method, a resource name
// and a status code do that — and a record that held payloads would be the one
// place in CloudBurrow a Secret Manager value was kept after the call returned.
// Resource is read from the request's own name or parent field and nothing
// else.
type Call struct {
	// Method is the full gRPC method, /package.Service/Method.
	Method string
	// Resource is the request's name or parent, when it has one.
	Resource string
	Code     codes.Code
	Duration time.Duration
	// Stream reports a streaming call, for which Resource is always empty:
	// the first message has not been read when the call is accepted.
	Stream bool
}

// Observer is told about each completed call.
type Observer func(Call)

// UnaryObserver reports every unary call to o.
func UnaryObserver(o Observer) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		start := time.Now()
		resp, err := handler(ctx, req)
		o(Call{
			Method:   info.FullMethod,
			Resource: ResourceOf(req),
			Code:     status.Code(err),
			Duration: time.Since(start),
		})
		return resp, err
	}
}

// StreamObserver reports every streaming call to o when it ends.
func StreamObserver(o Observer) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		start := time.Now()
		err := handler(srv, ss)
		o(Call{Method: info.FullMethod, Code: status.Code(err), Duration: time.Since(start), Stream: true})
		return err
	}
}

// ResourceOf returns the resource a request addresses, from its name or parent.
//
// Only those two fields, and only as strings. Google's APIs put the resource a
// call acts on in one of them, so that is enough to say what was touched,
// without reaching into anything that could be a payload.
func ResourceOf(req any) string {
	if r, ok := req.(interface{ GetName() string }); ok && r.GetName() != "" {
		return r.GetName()
	}
	if r, ok := req.(interface{ GetParent() string }); ok {
		return r.GetParent()
	}
	return ""
}
