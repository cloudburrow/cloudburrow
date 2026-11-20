// Package grpchttp builds and stops a gRPC server that is served through
// grpc.Server.ServeHTTP, on a port net/http (h2c) owns, so that stopping it
// waits for every call to end (#1804).
//
// ServeHTTP runs each call's handler on a goroutine of its own, and returns
// once the answer is written, which can be before that goroutine has run the
// server's stats handlers' End (where tracing ends the call's span, #1159)
// or returned from its interceptors. http.Server.Shutdown waits only for
// ServeHTTP, and grpc.Server.Stop waits for the handler goroutines only when
// the server was built with grpc.WaitForHandlers. A server built by
// NewServer and stopped by Stop has both.
package grpchttp

import (
	"context"

	"google.golang.org/grpc"

	"github.com/cloudburrow/cloudburrow/internal/sysparams"
)

// Options returns opts after grpc.WaitForHandlers(true), for a server served
// through ServeHTTP, and the interceptors that apply the x-goog-fieldmask
// response mask (#1954, internal/sysparams), so every gRPC surface built
// here prunes its answers as Google's do. A server whose codec is raw bytes
// (a proxy to an emulator) passes its messages through unpruned.
func Options(opts ...grpc.ServerOption) []grpc.ServerOption {
	return append([]grpc.ServerOption{
		grpc.WaitForHandlers(true),
		grpc.ChainUnaryInterceptor(sysparams.UnaryServerInterceptor()),
		grpc.ChainStreamInterceptor(sysparams.StreamServerInterceptor()),
	}, opts...)
}

// NewServer returns grpc.NewServer(Options(opts...)...).
func NewServer(opts ...grpc.ServerOption) *grpc.Server {
	return grpc.NewServer(Options(opts...)...)
}

// Stop stops g and waits until every call's handler goroutine has returned
// (which a server from NewServer waits for), or until ctx ends, whichever is
// first; then it returns ctx's error. A nil g is already stopped.
//
// Calls still running are cut off, so a server whose port net/http owns is
// stopped after http.Server.Shutdown has let them finish.
func Stop(ctx context.Context, g *grpc.Server) error {
	if g == nil {
		return nil
	}
	stopped := make(chan struct{})
	go func() { g.Stop(); close(stopped) }()
	select {
	case <-stopped:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
