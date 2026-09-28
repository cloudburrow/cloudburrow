package pubsubfront

// Benchmarks of the front's gRPC transport (#950): publish throughput,
// streaming-pull throughput and unary latency through the front, served
//
//   - native: grpc-go's own HTTP/2 transport, grpc.Server.Serve, which the
//     front used before #909 (split.go routed every connection opening with
//     the HTTP/2 preface to it);
//   - servehttp: grpc.Server.ServeHTTP behind net/http's HTTP/2 server, every
//     request routed by its own content type, which #909 moved it to;
//   - front: Front.Serve as it is now.
//
// The emulator behind the front is a stub (benchUpstream) that answers at
// once, so what is measured is the front and its transports, not the
// emulator. Run with:
//
//	go test ./internal/pubsubfront -run '^$' -bench Transport -benchtime 3s -count 5

import (
	"context"
	"fmt"
	"io"
	"net"
	"sort"
	"testing"
	"time"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// benchUpstream is an emulator that answers every call at once:
// Publish with an id per message, GetTopic with the topic, and
// StreamingPull with pullBatch messages of pullSize bytes for as long as the
// stream is open.
type benchUpstream struct {
	pubsubpb.UnimplementedPublisherServer
	pubsubpb.UnimplementedSubscriberServer
	batch *pubsubpb.StreamingPullResponse
}

const (
	pullBatch = 100
	pullSize  = 1 << 10
)

func (u *benchUpstream) Publish(_ context.Context, r *pubsubpb.PublishRequest) (*pubsubpb.PublishResponse, error) {
	ids := make([]string, len(r.GetMessages()))
	for i := range ids {
		ids[i] = "1"
	}
	return &pubsubpb.PublishResponse{MessageIds: ids}, nil
}

func (u *benchUpstream) GetTopic(_ context.Context, r *pubsubpb.GetTopicRequest) (*pubsubpb.Topic, error) {
	return &pubsubpb.Topic{Name: r.GetTopic()}, nil
}

func (u *benchUpstream) StreamingPull(s pubsubpb.Subscriber_StreamingPullServer) error {
	if _, err := s.Recv(); err != nil {
		return err
	}
	go func() {
		// Acks and deadline changes are read and dropped.
		for {
			if _, err := s.Recv(); err != nil {
				return
			}
		}
	}()
	for {
		if err := s.Send(u.batch); err != nil {
			return err
		}
	}
}

func startBenchUpstream(b *testing.B) string {
	b.Helper()
	u := &benchUpstream{batch: &pubsubpb.StreamingPullResponse{}}
	data := make([]byte, pullSize)
	for i := 0; i < pullBatch; i++ {
		u.batch.ReceivedMessages = append(u.batch.ReceivedMessages, &pubsubpb.ReceivedMessage{
			AckId:   fmt.Sprintf("ack-%d", i),
			Message: &pubsubpb.PubsubMessage{Data: data, MessageId: fmt.Sprint(i)},
		})
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	s := grpc.NewServer()
	pubsubpb.RegisterPublisherServer(s, u)
	pubsubpb.RegisterSubscriberServer(s, u)
	go func() { _ = s.Serve(l) }()
	b.Cleanup(s.Stop)
	return l.Addr().String()
}

// startBenchFront serves a front for upstream in mode and returns a client
// connection to it.
func startBenchFront(b *testing.B, mode, upstream string) *grpc.ClientConn {
	b.Helper()
	f, err := New(upstream, func(string, ...any) {})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = f.Close() })
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	switch mode {
	case "native":
		g := f.Server()
		go func() { defer close(done); _ = g.Serve(l) }()
		b.Cleanup(func() { g.Stop(); <-done })
	case "servehttp":
		g := f.Server()
		srv := f.httpServer(g)
		go func() { defer close(done); _ = srv.Serve(l) }()
		b.Cleanup(func() { srv.Close(); g.Stop(); <-done })
	case "front":
		go func() { defer close(done); _ = f.Serve(ctx, l, time.Hour) }()
		b.Cleanup(func() { cancel(); <-done })
	default:
		b.Fatalf("mode %q", mode)
	}
	b.Cleanup(cancel)
	conn, err := grpc.NewClient(l.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = conn.Close() })
	return conn
}

var benchModes = []string{"native", "servehttp", "front"}

// Publish throughput: many publishers at once, each call ten 1 KiB
// messages, as the official client batches them.
func BenchmarkTransportPublish(b *testing.B) {
	up := startBenchUpstream(b)
	for _, mode := range benchModes {
		b.Run(mode, func(b *testing.B) {
			pub := pubsubpb.NewPublisherClient(startBenchFront(b, mode, up))
			req := &pubsubpb.PublishRequest{Topic: "projects/p/topics/t"}
			for i := 0; i < 10; i++ {
				req.Messages = append(req.Messages, &pubsubpb.PubsubMessage{Data: make([]byte, 1<<10)})
			}
			if _, err := pub.Publish(context.Background(), req); err != nil {
				b.Fatal(err)
			}
			b.SetParallelism(4)
			b.ResetTimer()
			start := time.Now()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					if _, err := pub.Publish(context.Background(), req); err != nil {
						b.Error(err)
						return
					}
				}
			})
			b.ReportMetric(float64(b.N*len(req.Messages))/time.Since(start).Seconds(), "msgs/s")
		})
	}
}

// Streaming-pull throughput: one stream, the upstream sending batches of a
// hundred 1 KiB messages as fast as flow control lets it.
func BenchmarkTransportStreamingPull(b *testing.B) {
	up := startBenchUpstream(b)
	for _, mode := range benchModes {
		b.Run(mode, func(b *testing.B) {
			sub := pubsubpb.NewSubscriberClient(startBenchFront(b, mode, up))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			s, err := sub.StreamingPull(ctx)
			if err != nil {
				b.Fatal(err)
			}
			if err := s.Send(&pubsubpb.StreamingPullRequest{Subscription: "projects/p/subscriptions/s", StreamAckDeadlineSeconds: 60}); err != nil {
				b.Fatal(err)
			}
			if _, err := s.Recv(); err != nil {
				b.Fatal(err)
			}
			b.SetBytes(pullBatch * pullSize)
			b.ResetTimer()
			start := time.Now()
			for i := 0; i < b.N; i++ {
				if _, err := s.Recv(); err != nil && err != io.EOF {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(b.N*pullBatch)/time.Since(start).Seconds(), "msgs/s")
		})
	}
}

// Unary latency: one caller, one GetTopic at a time; p50 and p99 are
// reported beside the mean (ns/op).
func BenchmarkTransportUnaryLatency(b *testing.B) {
	up := startBenchUpstream(b)
	for _, mode := range benchModes {
		b.Run(mode, func(b *testing.B) {
			pub := pubsubpb.NewPublisherClient(startBenchFront(b, mode, up))
			req := &pubsubpb.GetTopicRequest{Topic: "projects/p/topics/t"}
			if _, err := pub.GetTopic(context.Background(), req); err != nil {
				b.Fatal(err)
			}
			lat := make([]time.Duration, 0, b.N)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				t0 := time.Now()
				if _, err := pub.GetTopic(context.Background(), req); err != nil {
					b.Fatal(err)
				}
				lat = append(lat, time.Since(t0))
			}
			b.StopTimer()
			sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
			b.ReportMetric(float64(lat[len(lat)/2])/1e3, "p50-µs")
			b.ReportMetric(float64(lat[len(lat)*99/100])/1e3, "p99-µs")
		})
	}
}
