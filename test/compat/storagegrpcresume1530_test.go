//go:build compat

package compat

import (
	"bytes"
	"context"
	"io"
	"sync"
	"sync/atomic"
	"testing"

	"cloud.google.com/go/storage"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// brokenWriteStream is the first BidiWriteObject stream of a client, which
// fails UNAVAILABLE after its third message, as a dropped connection does.
type brokenWriteStream struct {
	grpc.ClientStream
	cancel context.CancelFunc
	sent   int
	broken atomic.Bool
}

func (s *brokenWriteStream) SendMsg(m any) error {
	if s.broken.Load() {
		return io.EOF
	}
	s.sent++
	if s.sent > 3 {
		s.broken.Store(true)
		s.cancel()
		return io.EOF
	}
	return s.ClientStream.SendMsg(m)
}

func (s *brokenWriteStream) RecvMsg(m any) error {
	err := s.ClientStream.RecvMsg(m)
	if s.broken.Load() {
		return status.Error(codes.Unavailable, "connection dropped by the test")
	}
	return err
}

// covers: google.storage.v2.Storage/QueryWriteStatus, google.storage.v2.Storage/StartResumableWrite, google.storage.v2.Storage/BidiWriteObject
//
// TestStorageGRPCResumesABrokenWrite (#1530): the official Go client on its
// gRPC transport resumes a resumable write whose stream drops. The first
// BidiWriteObject stream fails UNAVAILABLE after three messages; the client
// (under RetryAlways, as the write has no precondition) asks QueryWriteStatus how much of the upload persisted, resends from
// there on a new stream, and the object read back over the JSON API is the
// whole payload, with its CRC32C.
func TestStorageGRPCResumesABrokenWrite(t *testing.T) {
	h := New(t)
	addr := stripScheme(h.Endpoint(EnvStorage))
	var (
		mu       sync.Mutex
		streams  int
		queries  atomic.Int32
		persists []int64
	)
	streamInt := func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
		if method != "/google.storage.v2.Storage/BidiWriteObject" {
			return streamer(ctx, desc, cc, method, opts...)
		}
		mu.Lock()
		streams++
		first := streams == 1
		mu.Unlock()
		if !first {
			return streamer(ctx, desc, cc, method, opts...)
		}
		sctx, cancel := context.WithCancel(ctx)
		cs, err := streamer(sctx, desc, cc, method, opts...)
		if err != nil {
			cancel()
			return nil, err
		}
		return &brokenWriteStream{ClientStream: cs, cancel: cancel}, nil
	}
	unaryInt := func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		err := invoker(ctx, method, req, reply, cc, opts...)
		if method == "/google.storage.v2.Storage/QueryWriteStatus" {
			queries.Add(1)
			if r, ok := reply.(interface{ GetPersistedSize() int64 }); ok && err == nil {
				mu.Lock()
				persists = append(persists, r.GetPersistedSize())
				mu.Unlock()
			}
		}
		return err
	}
	g, err := storage.NewGRPCClient(h.Context(), option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())),
		option.WithGRPCDialOption(grpc.WithChainStreamInterceptor(streamInt)),
		option.WithGRPCDialOption(grpc.WithChainUnaryInterceptor(unaryInt)))
	if err != nil {
		t.Fatalf("storage.NewGRPCClient: %v", err)
	}
	t.Cleanup(func() { _ = g.Close() })
	j := storageClient(t, h)
	ctx := h.Context()
	jb := bucket(t, h, j)

	payload := bytes.Repeat([]byte("resume-1530-"), 700_000) // 8.4 MB
	// A write with no precondition is not idempotent, so the client retries
	// it only under RetryAlways.
	w := g.Bucket(jb.BucketName()).Object("resumed.bin").Retryer(storage.WithPolicy(storage.RetryAlways)).NewWriter(ctx)
	w.ChunkSize = 1 << 20
	if _, err := w.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close after a dropped stream: %v", err)
	}
	mu.Lock()
	n := streams
	got := append([]int64(nil), persists...)
	mu.Unlock()
	if n < 2 || queries.Load() < 1 {
		t.Fatalf("%d BidiWriteObject streams and %d QueryWriteStatus calls; want the drop resumed", n, queries.Load())
	}
	for _, size := range got {
		if size < 0 || size > int64(len(payload)) || size%(256<<10) != 0 {
			t.Errorf("QueryWriteStatus persisted_size %d; want whole 256 KiB units of the %d bytes", size, len(payload))
		}
	}
	r, err := jb.Object("resumed.bin").NewReader(ctx)
	if err != nil {
		t.Fatal(err)
	}
	back, err := io.ReadAll(r)
	_ = r.Close()
	if err != nil || !bytes.Equal(back, payload) {
		t.Fatalf("read back %d bytes, %v; want the %d written", len(back), err, len(payload))
	}
	if attrs := w.Attrs(); attrs.Size != int64(len(payload)) {
		t.Errorf("the writer's attrs say %d bytes", attrs.Size)
	}
}
