package kms

import (
	"context"
	"net"
	"testing"

	kms "cloud.google.com/go/kms/apiv1"
	"cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/cloudburrow/cloudburrow/internal/store"
)

const loc = "projects/demo-project/locations/global"

func client(t *testing.T) *kms.KeyManagementClient {
	t.Helper()
	return clientFor(t, store.NewMemory())
}

// clientFor serves a Server over db to the official client.
func clientFor(t *testing.T, db store.Store) *kms.KeyManagementClient {
	t.Helper()
	return clientOf(t, NewServer(db))
}

// clientOf serves srv to the official client.
func clientOf(t *testing.T, srv *Server) *kms.KeyManagementClient {
	t.Helper()
	g := grpc.NewServer()
	srv.Register(g)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = g.Serve(ln) }()
	t.Cleanup(g.Stop)
	c, err := kms.NewKeyManagementClient(context.Background(), option.WithEndpoint(ln.Addr().String()),
		option.WithoutAuthentication(), option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestKeyRingsKeysAndVersions(t *testing.T) {
	c := client(t)
	ctx := context.Background()
	ring, err := c.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: loc, KeyRingId: "ring"})
	if err != nil || ring.GetName() != loc+"/keyRings/ring" {
		t.Fatalf("CreateKeyRing = %v, %v", ring, err)
	}
	if _, err := c.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: loc, KeyRingId: "ring"}); status.Code(err) != codes.AlreadyExists {
		t.Errorf("duplicate key ring = %v", err)
	}
	if g, err := c.GetKeyRing(ctx, &kmspb.GetKeyRingRequest{Name: ring.GetName()}); err != nil || g.GetName() != ring.GetName() {
		t.Errorf("GetKeyRing = %v, %v", g, err)
	}
	rit := c.ListKeyRings(ctx, &kmspb.ListKeyRingsRequest{Parent: loc})
	if r, err := rit.Next(); err != nil || r.GetName() != ring.GetName() {
		t.Errorf("ListKeyRings = %v, %v", r, err)
	}

	key, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "k",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT, Labels: map[string]string{"app": "x"}}})
	if err != nil {
		t.Fatalf("CreateCryptoKey: %v", err)
	}
	if key.GetPrimary().GetName() != key.GetName()+"/cryptoKeyVersions/1" ||
		key.GetPrimary().GetAlgorithm() != kmspb.CryptoKeyVersion_GOOGLE_SYMMETRIC_ENCRYPTION {
		t.Errorf("new key's primary = %v", key.GetPrimary())
	}
	v2, err := c.CreateCryptoKeyVersion(ctx, &kmspb.CreateCryptoKeyVersionRequest{Parent: key.GetName()})
	if err != nil || v2.GetName() != key.GetName()+"/cryptoKeyVersions/2" || v2.GetState() != kmspb.CryptoKeyVersion_ENABLED {
		t.Fatalf("CreateCryptoKeyVersion = %v, %v", v2, err)
	}
	if g, _ := c.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: key.GetName()}); g.GetPrimary().GetName() != key.GetName()+"/cryptoKeyVersions/1" {
		t.Errorf("a new version became primary: %v", g.GetPrimary())
	}
	updated, err := c.UpdateCryptoKeyPrimaryVersion(ctx, &kmspb.UpdateCryptoKeyPrimaryVersionRequest{Name: key.GetName(), CryptoKeyVersionId: "2"})
	if err != nil || updated.GetPrimary().GetName() != v2.GetName() {
		t.Errorf("UpdateCryptoKeyPrimaryVersion = %v, %v", updated, err)
	}
	var names []string
	vit := c.ListCryptoKeyVersions(ctx, &kmspb.ListCryptoKeyVersionsRequest{Parent: key.GetName()})
	for {
		v, err := vit.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, v.GetName()[len(key.GetName())+1:])
	}
	if len(names) != 2 || names[0] != "cryptoKeyVersions/1" || names[1] != "cryptoKeyVersions/2" {
		t.Errorf("ListCryptoKeyVersions = %v", names)
	}
	kit := c.ListCryptoKeys(ctx, &kmspb.ListCryptoKeysRequest{Parent: ring.GetName()})
	if k, err := kit.Next(); err != nil || k.GetLabels()["app"] != "x" {
		t.Errorf("ListCryptoKeys = %v, %v", k, err)
	}
}

func TestUnsupportedKMSIsUnimplemented(t *testing.T) {
	c := client(t)
	ctx := context.Background()
	ring, _ := c.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: loc, KeyRingId: "r2"})
	for purpose, alg := range map[kmspb.CryptoKey_CryptoKeyPurpose]kmspb.CryptoKeyVersion_CryptoKeyVersionAlgorithm{
		kmspb.CryptoKey_ASYMMETRIC_SIGN:    kmspb.CryptoKeyVersion_EC_SIGN_P256_SHA256,
		kmspb.CryptoKey_ASYMMETRIC_DECRYPT: kmspb.CryptoKeyVersion_RSA_DECRYPT_OAEP_2048_SHA256,
		kmspb.CryptoKey_MAC:                kmspb.CryptoKeyVersion_HMAC_SHA256,
	} {
		_, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "a",
			CryptoKey: &kmspb.CryptoKey{Purpose: purpose, VersionTemplate: &kmspb.CryptoKeyVersionTemplate{Algorithm: alg}}})
		if status.Code(err) != codes.Unimplemented {
			t.Errorf("purpose %s = %v, want Unimplemented", purpose, err)
		}
	}
	if _, err := c.CreateImportJob(ctx, &kmspb.CreateImportJobRequest{Parent: ring.GetName(), ImportJobId: "j"}); status.Code(err) != codes.Unimplemented {
		t.Errorf("CreateImportJob = %v", err)
	}
	if _, err := c.Encrypt(ctx, &kmspb.EncryptRequest{Name: ring.GetName() + "/cryptoKeys/k", Plaintext: []byte("x")}); status.Code(err) != codes.Unimplemented {
		t.Errorf("Encrypt = %v", err)
	}
	if _, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: loc + "/keyRings/absent", CryptoKeyId: "k",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}}); status.Code(err) != codes.NotFound {
		t.Errorf("key in a missing ring = %v", err)
	}
}
