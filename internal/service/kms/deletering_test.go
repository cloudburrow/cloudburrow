package kms

import (
	"context"
	"net"
	"testing"
	"time"

	"cloud.google.com/go/kms/apiv1/kmspb"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/cloudburrow/cloudburrow/internal/sched"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// DeleteKeyRing (#1595): a ring with a key or an unexpired import job is
// refused; an empty one is deleted, its expired import jobs with it, and
// its name is held for 5 minutes on the server's clock.
func TestDeleteKeyRing(t *testing.T) {
	ctx := context.Background()
	clock := sched.NewFakeClock(time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC))
	s := NewServerWithClock(store.NewMemory(), clock)
	ring, err := s.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: loc, KeyRingId: "gone"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteKeyRing(ctx, &kmspb.GetKeyRingRequest{Name: loc + "/keyRings/absent"}); status.Code(err) != codes.NotFound {
		t.Errorf("DeleteKeyRing(absent) = %v; want NotFound", err)
	}
	key, err := s.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "k",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}, SkipInitialVersionCreation: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteKeyRing(ctx, &kmspb.GetKeyRingRequest{Name: ring.GetName()}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("DeleteKeyRing with a key = %v; want FailedPrecondition", err)
	}
	op, err := s.DeleteCryptoKey(ctx, &kmspb.DeleteCryptoKeyRequest{Name: key.GetName()})
	if err != nil || !op.GetDone() {
		t.Fatalf("DeleteCryptoKey = %v, %v", op, err)
	}
	if _, err := s.CreateImportJob(ctx, &kmspb.CreateImportJobRequest{Parent: ring.GetName(), ImportJobId: "j", ImportJob: &kmspb.ImportJob{
		ImportMethod: kmspb.ImportJob_RSA_OAEP_3072_SHA1_AES_256, ProtectionLevel: kmspb.ProtectionLevel_SOFTWARE}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteKeyRing(ctx, &kmspb.GetKeyRingRequest{Name: ring.GetName()}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("DeleteKeyRing with an ACTIVE import job = %v; want FailedPrecondition", err)
	}
	clock.Advance(importJobTTL)
	op, err = s.DeleteKeyRing(ctx, &kmspb.GetKeyRingRequest{Name: ring.GetName()})
	if err != nil || !op.GetDone() || op.GetError() != nil {
		t.Fatalf("DeleteKeyRing(empty) = %v, %v; want a done Operation", op, err)
	}
	if _, err := s.GetKeyRing(ctx, &kmspb.GetKeyRingRequest{Name: ring.GetName()}); status.Code(err) != codes.NotFound {
		t.Errorf("GetKeyRing after delete = %v; want NotFound", err)
	}
	if _, err := s.GetImportJob(ctx, &kmspb.GetImportJobRequest{Name: ring.GetName() + "/importJobs/j"}); status.Code(err) != codes.NotFound {
		t.Errorf("GetImportJob after the ring's delete = %v; want NotFound", err)
	}
	if _, err := s.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: loc, KeyRingId: "gone"}); status.Code(err) != codes.AlreadyExists {
		t.Errorf("CreateKeyRing straight after delete = %v; want AlreadyExists", err)
	}
	clock.Advance(ringReuseAfter)
	if _, err := s.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: loc, KeyRingId: "gone"}); err != nil {
		t.Errorf("CreateKeyRing 5 minutes after delete: %v", err)
	}
}

// DeleteKeyRing is served over gRPC at the method Google's would be, with
// the KeyManagementService's other methods still there (#1595).
func TestDeleteKeyRingOverGRPC(t *testing.T) {
	ctx := context.Background()
	s := NewServer(store.NewMemory())
	srv := grpc.NewServer()
	s.Register(srv)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient(ln.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	c := kmspb.NewKeyManagementServiceClient(conn)
	ring, err := c.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: loc, KeyRingId: "grpc"})
	if err != nil {
		t.Fatal(err)
	}
	var op longrunningpb.Operation
	if err := conn.Invoke(ctx, deleteKeyRingMethod, &kmspb.GetKeyRingRequest{}, &op); status.Code(err) != codes.InvalidArgument {
		t.Errorf("DeleteKeyRing with no name = %v; want InvalidArgument", err)
	}
	if err := conn.Invoke(ctx, deleteKeyRingMethod, &kmspb.GetKeyRingRequest{Name: ring.GetName()}, &op); err != nil || !op.GetDone() {
		t.Fatalf("DeleteKeyRing = %v, %v", &op, err)
	}
	if _, err := c.GetKeyRing(ctx, &kmspb.GetKeyRingRequest{Name: ring.GetName()}); status.Code(err) != codes.NotFound {
		t.Errorf("GetKeyRing after DeleteKeyRing = %v; want NotFound", err)
	}
}
