package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	gcs "cloud.google.com/go/storage"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	grpcstatus "google.golang.org/grpc/status"

	pb "github.com/cloudburrow/cloudburrow/internal/service/storage/storagev2pb"
)

// grpcFixture serves s on loopback, HTTP/1.1 and h2c on one port, as
// storage-server does, and returns its address.
func grpcFixture(t *testing.T, s *Server) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var p http.Protocols
	p.SetHTTP1(true)
	p.SetUnencryptedHTTP2(true)
	hs := &http.Server{Handler: s, Protocols: &p, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = hs.Serve(ln) }()
	t.Cleanup(func() { _ = hs.Close() })
	return ln.Addr().String()
}

func grpcClients(t *testing.T) (grpcc, jsonc *gcs.Client, addr string) {
	t.Helper()
	s, err := NewServer(Options{})
	if err != nil {
		t.Fatal(err)
	}
	addr = grpcFixture(t, s)
	ctx := context.Background()
	grpcc, err = gcs.NewGRPCClient(ctx, option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = grpcc.Close() })
	jsonc, err = gcs.NewClient(ctx, option.WithEndpoint("http://"+addr+"/storage/v1/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = jsonc.Close() })
	return grpcc, jsonc, addr
}

// TestGRPCObjectsAgreeWithJSON writes and reads through the official Go
// client's gRPC transport, and reads each result back over JSON.
func TestGRPCObjectsAgreeWithJSON(t *testing.T) {
	g, j, _ := grpcClients(t)
	ctx := context.Background()
	bh := g.Bucket("grpc-bucket")
	if err := bh.Create(ctx, "p", &gcs.BucketAttrs{Labels: map[string]string{"k": "v"}, VersioningEnabled: true}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	ja, err := j.Bucket("grpc-bucket").Attrs(ctx)
	if err != nil || ja.Labels["k"] != "v" || !ja.VersioningEnabled {
		t.Fatalf("JSON Attrs = %+v, %v", ja, err)
	}
	// Bigger than one 2 MiB message and one 256 KiB unit, in small chunks,
	// so the write is persisted over several resumable chunks.
	payload := bytes.Repeat([]byte("0123456789abcdef"), 400_000)
	w := bh.Object("dir/big").NewWriter(ctx)
	w.ChunkSize = 1 << 20
	w.ContentType = "text/plain"
	w.Metadata = map[string]string{"a": "b"}
	if _, err := w.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	attrs := w.Attrs()
	jo, err := j.Bucket("grpc-bucket").Object("dir/big").Attrs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if jo.Generation != attrs.Generation || jo.CRC32C != attrs.CRC32C || !bytes.Equal(jo.MD5, attrs.MD5) || jo.Size != int64(len(payload)) || jo.Metadata["a"] != "b" || jo.ContentType != "text/plain" {
		t.Fatalf("JSON attrs %+v differ from gRPC attrs %+v", jo, attrs)
	}
	// A JSON write read over gRPC, whole and ranged.
	jw := j.Bucket("grpc-bucket").Object("dir/small").NewWriter(ctx)
	_, _ = jw.Write([]byte("hello, grpc"))
	if err := jw.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := bh.Object("dir/small").NewReader(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(r)
	_ = r.Close()
	if string(got) != "hello, grpc" {
		t.Fatalf("read %q", got)
	}
	r, err = bh.Object("dir/small").NewRangeReader(ctx, 7, 4)
	if err != nil {
		t.Fatal(err)
	}
	got, _ = io.ReadAll(r)
	_ = r.Close()
	if string(got) != "grpc" {
		t.Fatalf("ranged read %q", got)
	}
	r, err = bh.Object("dir/big").NewReader(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got, _ = io.ReadAll(r)
	_ = r.Close()
	if !bytes.Equal(got, payload) {
		t.Fatalf("big read: %d bytes, want %d", len(got), len(payload))
	}
	// Preconditions: a stale generation is refused.
	w = bh.Object("dir/small").If(gcs.Conditions{GenerationMatch: 1}).NewWriter(ctx)
	_, _ = w.Write([]byte("x"))
	if err := w.Close(); grpcstatus.Code(err) != codes.FailedPrecondition {
		t.Fatalf("stale write = %v, want FAILED_PRECONDITION", err)
	}
	// A wrong CRC32C is refused, and leaves nothing.
	w = bh.Object("dir/bad").NewWriter(ctx)
	w.SendCRC32C, w.CRC32C = true, 1234
	_, _ = w.Write([]byte("checksummed"))
	if err := w.Close(); err == nil {
		t.Fatal("a wrong crc32c was accepted")
	}
	if _, err := j.Bucket("grpc-bucket").Object("dir/bad").Attrs(ctx); !errors.Is(err, gcs.ErrObjectNotExist) {
		t.Fatalf("dir/bad after a refused write: %v", err)
	}
	// List with prefix and delimiter.
	it := bh.Objects(ctx, &gcs.Query{Prefix: "", Delimiter: "/"})
	var names []string
	for {
		a, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, a.Name+a.Prefix)
	}
	if strings.Join(names, ",") != "dir/" {
		t.Fatalf("list = %v", names)
	}
	// Update, compose, copy (rewrite), delete.
	if _, err := bh.Object("dir/small").Update(ctx, gcs.ObjectAttrsToUpdate{ContentType: "text/x"}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if jo, _ := j.Bucket("grpc-bucket").Object("dir/small").Attrs(ctx); jo.ContentType != "text/x" {
		t.Fatalf("JSON sees content type %q", jo.ContentType)
	}
	if _, err := bh.Object("composed").ComposerFrom(bh.Object("dir/small"), bh.Object("dir/small")).Run(ctx); err != nil {
		t.Fatalf("Compose: %v", err)
	}
	if _, err := bh.Object("copied").CopierFrom(bh.Object("dir/big")).Run(ctx); err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if jo, _ := j.Bucket("grpc-bucket").Object("copied").Attrs(ctx); jo == nil || jo.CRC32C != attrs.CRC32C {
		t.Fatalf("copied = %+v", jo)
	}
	if err := bh.Object("composed").Delete(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := j.Bucket("grpc-bucket").Object("composed").Attrs(ctx); !errors.Is(err, gcs.ErrObjectNotExist) {
		t.Fatalf("composed after delete: %v", err)
	}
	if _, err := bh.Object("composed").Attrs(ctx); !errors.Is(err, gcs.ErrObjectNotExist) {
		t.Fatalf("gRPC Attrs after delete = %v", err)
	}
}

// TestGRPCRegistryMatchesTheServer calls every google.storage.v2.Storage
// method with an empty request, and holds tools/coverage/registry.json to
// what the server answers: UNIMPLEMENTED exactly where it says so.
func TestGRPCRegistryMatchesTheServer(t *testing.T) {
	raw, err := os.ReadFile("../../../tools/coverage/registry.json")
	if err != nil {
		t.Fatal(err)
	}
	var reg map[string]string
	if err := json.Unmarshal(raw, &reg); err != nil {
		t.Fatal(err)
	}
	s, err := NewServer(Options{})
	if err != nil {
		t.Fatal(err)
	}
	conn, err := grpc.NewClient(grpcFixture(t, s), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	for _, full := range GRPCMethods() {
		want, ok := reg[full]
		if !ok {
			t.Errorf("%s has no registry entry", full)
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		code := emptyCall(ctx, conn, full)
		cancel()
		if (want == "unimplemented") != (code == codes.Unimplemented) {
			t.Errorf("%s is %s in the registry, but the server answered %v", full, want, code)
		}
	}
}

// emptyCall invokes full with an empty message and returns its code.
func emptyCall(ctx context.Context, conn *grpc.ClientConn, full string) codes.Code {
	desc := &grpc.StreamDesc{ClientStreams: true, ServerStreams: true}
	stream, err := conn.NewStream(ctx, desc, "/"+full)
	if err != nil {
		return grpcstatus.Code(err)
	}
	_ = stream.SendMsg(&pb.Object{})
	_ = stream.CloseSend()
	var out pb.Object
	for {
		if err := stream.RecvMsg(&out); err != nil {
			if errors.Is(err, io.EOF) {
				return codes.OK
			}
			return grpcstatus.Code(err)
		}
	}
}

// TestGRPCWriteObjectResumes covers the methods the Go client calls only
// when a stream breaks, or never: a WriteObject stream that ends unfinished
// leaves its whole 256 KiB units persisted, QueryWriteStatus reports them,
// a second WriteObject from that offset finishes the object with its
// checksums verified, and CancelResumableWrite ends a session.
func TestGRPCWriteObjectResumes(t *testing.T) {
	g, j, addr := grpcClients(t)
	ctx := context.Background()
	if err := g.Bucket("resume").Create(ctx, "p", nil); err != nil {
		t.Fatal(err)
	}
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	c := pb.NewStorageClient(conn)
	start, err := c.StartResumableWrite(ctx, &pb.StartResumableWriteRequest{WriteObjectSpec: &pb.WriteObjectSpec{
		Resource: &pb.Object{Bucket: "projects/_/buckets/resume", Name: "obj", ContentType: "application/x-test"}}})
	if err != nil {
		t.Fatal(err)
	}
	data := bytes.Repeat([]byte{7}, chunkUnit+1000)
	ws, err := c.WriteObject(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_ = ws.Send(&pb.WriteObjectRequest{FirstMessage: &pb.WriteObjectRequest_UploadId{UploadId: start.GetUploadId()},
		Data: &pb.WriteObjectRequest_ChecksummedData{ChecksummedData: checksummed(data[:chunkUnit+500])}})
	resp, err := ws.CloseAndRecv()
	if err != nil || resp.GetPersistedSize() != chunkUnit {
		t.Fatalf("unfinished WriteObject = %v, %v; want %d persisted", resp, err, chunkUnit)
	}
	q, err := c.QueryWriteStatus(ctx, &pb.QueryWriteStatusRequest{UploadId: start.GetUploadId()})
	if err != nil || q.GetPersistedSize() != chunkUnit {
		t.Fatalf("QueryWriteStatus = %v, %v", q, err)
	}
	ws, _ = c.WriteObject(ctx)
	wrong := uint32(1)
	_ = ws.Send(&pb.WriteObjectRequest{FirstMessage: &pb.WriteObjectRequest_UploadId{UploadId: start.GetUploadId()}, WriteOffset: chunkUnit,
		Data:        &pb.WriteObjectRequest_ChecksummedData{ChecksummedData: checksummed(data[chunkUnit:])},
		FinishWrite: true, ObjectChecksums: &pb.ObjectChecksums{Crc32C: &wrong}})
	if _, err := ws.CloseAndRecv(); grpcstatus.Code(err) != codes.InvalidArgument {
		t.Fatalf("a finish with a wrong crc32c = %v, want INVALID_ARGUMENT", err)
	}
	crc := crc32Of(data)
	ws, _ = c.WriteObject(ctx)
	_ = ws.Send(&pb.WriteObjectRequest{FirstMessage: &pb.WriteObjectRequest_UploadId{UploadId: start.GetUploadId()}, WriteOffset: chunkUnit,
		Data:        &pb.WriteObjectRequest_ChecksummedData{ChecksummedData: checksummed(data[chunkUnit:])},
		FinishWrite: true, ObjectChecksums: &pb.ObjectChecksums{Crc32C: &crc}})
	resp, err = ws.CloseAndRecv()
	if err != nil || resp.GetResource().GetSize() != int64(len(data)) {
		t.Fatalf("finishing WriteObject = %v, %v", resp, err)
	}
	if a, err := j.Bucket("resume").Object("obj").Attrs(ctx); err != nil || a.CRC32C != crc || a.ContentType != "application/x-test" {
		t.Fatalf("JSON attrs = %+v, %v", a, err)
	}
	q, err = c.QueryWriteStatus(ctx, &pb.QueryWriteStatusRequest{UploadId: start.GetUploadId()})
	if err != nil || q.GetResource().GetGeneration() != resp.GetResource().GetGeneration() {
		t.Fatalf("QueryWriteStatus after the finish = %v, %v", q, err)
	}
	other, _ := c.StartResumableWrite(ctx, &pb.StartResumableWriteRequest{WriteObjectSpec: &pb.WriteObjectSpec{
		Resource: &pb.Object{Bucket: "projects/_/buckets/resume", Name: "cancelled"}}})
	if _, err := c.CancelResumableWrite(ctx, &pb.CancelResumableWriteRequest{UploadId: other.GetUploadId()}); err != nil {
		t.Fatalf("CancelResumableWrite: %v", err)
	}
	if _, err := c.QueryWriteStatus(ctx, &pb.QueryWriteStatusRequest{UploadId: other.GetUploadId()}); grpcstatus.Code(err) != codes.NotFound {
		t.Fatalf("QueryWriteStatus after cancel = %v, want NOT_FOUND", err)
	}
}

func crc32Of(b []byte) uint32 { return checksummed(b).GetCrc32C() }
