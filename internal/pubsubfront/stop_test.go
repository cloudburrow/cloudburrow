package pubsubfront

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"cloud.google.com/go/pubsub/v2/pstest"

	"github.com/cloudburrow/cloudburrow/internal/frontready"
	"github.com/cloudburrow/cloudburrow/internal/grpchttp/grpchttptest"
)

// TestServeWaitsForCallEnd: once ctx ends, Serve returns only after every
// call has ended for the gRPC server's stats handlers, on a connection gRPC
// opened, which grpc-go's own transport serves, and on one a REST request
// opened, whose gRPC calls go through ServeHTTP (#1804). Either ends a call
// on a goroutine of its own after the answer is written; the state the
// call changed is flushed after it.
func TestServeWaitsForCallEnd(t *testing.T) {
	for _, restFirst := range []bool{false, true} {
		fake := pstest.NewServer()
		f, err := New(fake.Addr, t.Logf)
		if err != nil {
			t.Fatal(err)
		}
		var h grpchttptest.SlowEnd
		f.grpcOptions = append(f.grpcOptions, h.Option())
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		served := make(chan error, 1)
		go func() { served <- f.Serve(ctx, l, time.Hour) }()
		const method = "/google.pubsub.v1.Publisher/GetTopic"
		if restFirst {
			// One HTTP/2 connection, REST first, then gRPC.
			c := h2cClient()
			resp, err := c.Get("http://" + l.Addr().String() + frontready.Path)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			req, _ := http.NewRequest(http.MethodPost, "http://"+l.Addr().String()+method, bytes.NewReader([]byte{0, 0, 0, 0, 0}))
			req.Header.Set("Content-Type", "application/grpc")
			req.Header.Set("Te", "trailers")
			resp, err = c.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		} else if err := grpchttptest.Call(context.Background(), l.Addr().String(), method); err != nil {
			t.Fatal(err)
		}
		cancel()
		if err := <-served; err != nil {
			t.Fatal(err)
		}
		if !h.Ended() {
			t.Errorf("REST first %v: Serve returned before the call's stats End (%d calls)", restFirst, h.Calls())
		}
		_ = f.Close()
		_ = fake.Close()
	}
}
