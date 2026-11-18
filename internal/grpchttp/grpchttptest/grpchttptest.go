// Package grpchttptest checks that a server served through
// grpc.Server.ServeHTTP is stopped only once every call has ended (#1804).
package grpchttptest

import (
	"context"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/stats"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/cloudburrow/cloudburrow/internal/hostguard"
)

// EndDelay is how long SlowEnd's handler takes to see a call end: longer
// than ServeHTTP takes to return once the answer is written.
const EndDelay = 100 * time.Millisecond

// SlowEnd is a stats handler slow to see a call end, as a trace exporter
// can be. Ended reports whether it has seen every call it was told of end.
type SlowEnd struct {
	begun, ended atomic.Int64
}

// Option is the server option that installs h.
func (h *SlowEnd) Option() grpc.ServerOption { return grpc.StatsHandler(h) }

// Calls reports how many calls began.
func (h *SlowEnd) Calls() int64 { return h.begun.Load() }

// Ended reports whether at least one call began and every call that began
// has been seen to end.
func (h *SlowEnd) Ended() bool {
	b := h.begun.Load()
	return b > 0 && h.ended.Load() == b
}

func (h *SlowEnd) TagRPC(ctx context.Context, _ *stats.RPCTagInfo) context.Context   { return ctx }
func (h *SlowEnd) TagConn(ctx context.Context, _ *stats.ConnTagInfo) context.Context { return ctx }
func (h *SlowEnd) HandleConn(context.Context, stats.ConnStats)                       {}

func (h *SlowEnd) HandleRPC(_ context.Context, s stats.RPCStats) {
	switch s.(type) {
	case *stats.Begin:
		h.begun.Add(1)
	case *stats.End:
		time.Sleep(EndDelay)
		h.ended.Add(1)
	}
}

// Call makes one unary call of method (/package.Service/Method) at addr,
// over cleartext HTTP/2, with an empty request: every field unset, which
// any request message decodes. The answer, an error as often as not, is
// not the point: the call is one the server's stats handlers see begin and
// end. It reports whether the call could be made at all.
func Call(ctx context.Context, addr, method string, opts ...grpc.CallOption) error {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.Invoke(ctx, method, &emptypb.Empty{}, &emptypb.Empty{}, opts...)
	return nil
}

// Serve serves h on loopback, HTTP/1.1 and cleartext HTTP/2 on one port,
// behind the Host and Origin checks, as the call front and storage-server
// do, and returns the server and its address. The caller shuts it down.
func Serve(h http.Handler) (*http.Server, string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, "", err
	}
	var p http.Protocols
	p.SetHTTP1(true)
	p.SetUnencryptedHTTP2(true)
	hs := &http.Server{Handler: hostguard.Wrap(h), Protocols: &p, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = hs.Serve(ln) }()
	return hs, ln.Addr().String(), nil
}
