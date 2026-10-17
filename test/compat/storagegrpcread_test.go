//go:build compat

package compat

import (
	"bytes"
	"errors"
	"hash/crc32"
	"io"
	"testing"

	"cloud.google.com/go/storage"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	pb "github.com/cloudburrow/cloudburrow/internal/service/storage/storagev2pb"
)

// TestStorageGRPCReadObjectMessagesHandleAndRange (#1576): what
// TestStorageGRPCObjects does not assert about a gRPC read. Each
// ReadObject message of a read over several messages carries its own
// CRC32C of its own bytes (ChecksummedData.crc32c, which the official
// client checks message by message); a read_offset past the end is
// OUT_OF_RANGE, on ReadObject and through the official client; and the
// read_handle a BidiReadObject returns, given back by the official client
// with no generation, reads the generation it was issued for after the
// object was overwritten.
// covers: google.storage.v2.Storage/ReadObject, google.storage.v2.Storage/BidiReadObject
func TestStorageGRPCReadObjectMessagesHandleAndRange(t *testing.T) {
	h := New(t)
	g := storageGRPCClient(t, h, false)
	gb := storageGRPCClient(t, h, true)
	j := storageClient(t, h)
	ctx := h.Context()
	jb := bucket(t, h, j)
	if _, err := jb.Update(ctx, storage.BucketAttrsToUpdate{VersioningEnabled: true}); err != nil {
		t.Fatal(err)
	}

	payload := bytes.Repeat([]byte("0123456789abcdef"), 5<<20/16+3) // over 5 MiB: three messages
	w := jb.Object("msgs.bin").NewWriter(ctx)
	if _, err := w.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	first := w.Attrs()

	conn, err := grpc.NewClient(stripScheme(h.Endpoint(EnvStorage)), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	raw := pb.NewStorageClient(conn)
	bucketName := "projects/_/buckets/" + jb.BucketName()

	stream, err := raw.ReadObject(ctx, &pb.ReadObjectRequest{Bucket: bucketName, Object: "msgs.bin"})
	if err != nil {
		t.Fatal(err)
	}
	var got []byte
	msgs := 0
	for {
		m, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("ReadObject Recv: %v", err)
		}
		msgs++
		cd := m.GetChecksummedData()
		if cd.Crc32C == nil {
			t.Fatalf("message %d has no crc32c", msgs)
		}
		if want := crc32.Checksum(cd.GetContent(), crc32.MakeTable(crc32.Castagnoli)); cd.GetCrc32C() != want {
			t.Errorf("message %d crc32c = %d, want %d of its %d bytes", msgs, cd.GetCrc32C(), want, len(cd.GetContent()))
		}
		if len(cd.GetContent()) > 2<<20 {
			t.Errorf("message %d carries %d bytes, over 2 MiB", msgs, len(cd.GetContent()))
		}
		got = append(got, cd.GetContent()...)
	}
	if msgs < 3 || !bytes.Equal(got, payload) {
		t.Fatalf("ReadObject: %d messages, %d bytes; want at least 3 and the %d bytes written", msgs, len(got), len(payload))
	}
	// The official client reads it whole through the same per-message
	// checks, on both read methods.
	for _, c := range []*storage.Client{g, gb} {
		r, err := c.Bucket(jb.BucketName()).Object("msgs.bin").NewReader(ctx)
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(r)
		_ = r.Close()
		if err != nil || !bytes.Equal(b, payload) {
			t.Fatalf("official gRPC read: %d bytes, %v", len(b), err)
		}
	}

	// Past the end: OUT_OF_RANGE.
	size := int64(len(payload))
	stream, err = raw.ReadObject(ctx, &pb.ReadObjectRequest{Bucket: bucketName, Object: "msgs.bin", ReadOffset: size + 1})
	if err == nil {
		_, err = stream.Recv()
	}
	if status.Code(err) != codes.OutOfRange {
		t.Errorf("ReadObject at offset %d of %d = %v, want OUT_OF_RANGE", size+1, size, err)
	}
	if r, err := g.Bucket(jb.BucketName()).Object("msgs.bin").NewRangeReader(ctx, size+1, -1); status.Code(err) != codes.OutOfRange {
		if r != nil {
			_ = r.Close()
		}
		t.Errorf("official NewRangeReader at offset %d of %d = %v, want OUT_OF_RANGE", size+1, size, err)
	}

	// read_handle: issued by BidiReadObject, it pins the generation.
	r, err := gb.Bucket(jb.BucketName()).Object("msgs.bin").NewRangeReader(ctx, 0, 16)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(r)
	handle := r.ReadHandle()
	_ = r.Close()
	if len(handle) == 0 {
		t.Fatal("BidiReadObject returned no read_handle")
	}
	w = jb.Object("msgs.bin").NewWriter(ctx)
	_, _ = w.Write([]byte("the second generation"))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	r, err = gb.Bucket(jb.BucketName()).Object("msgs.bin").ReadHandle(handle).NewRangeReader(ctx, 0, 16)
	if err != nil {
		t.Fatalf("read by handle: %v", err)
	}
	b, err := io.ReadAll(r)
	_ = r.Close()
	if err != nil || string(b) != "0123456789abcdef" || r.Attrs.Generation != first.Generation {
		t.Errorf("read by the first generation's handle = %q gen %d, %v; want %q gen %d", b, r.Attrs.Generation, err, "0123456789abcdef", first.Generation)
	}
	r, err = gb.Bucket(jb.BucketName()).Object("msgs.bin").NewReader(ctx)
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(r)
	_ = r.Close()
	if string(b) != "the second generation" {
		t.Errorf("read with no handle = %q, want the live generation", b)
	}
}
