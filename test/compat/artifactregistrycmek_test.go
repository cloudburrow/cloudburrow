//go:build compat

package compat

import (
	"strings"
	"testing"

	"cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	kms "cloud.google.com/go/kms/apiv1"
	"cloud.google.com/go/kms/apiv1/kmspb"
	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/uuid"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// covers: google.devtools.artifactregistry.v1.ArtifactRegistry/CreateRepository, google.devtools.artifactregistry.v1.ArtifactRegistry/ListVersions
//
// TestArtifactRegistryCMEK (#1897): a DOCKER repository created with the
// official client and kms_key_name, a key the official Cloud KMS client
// made in the repository's location, encrypts what crane pushes: it pulls
// back while the key version is enabled; with the version DISABLED a pull
// and a push are refused (the registry's DENIED) while ListVersions still
// lists the version, as metadata stays readable; enabled again, the image pulls; with the version
// scheduled for destruction it is refused again.
func TestArtifactRegistryCMEK(t *testing.T) {
	h := New(t)
	ar, ctx := arClient(t, h)
	kc, err := kms.NewKeyManagementClient(ctx, option.WithEndpoint(runShardEndpoint(h, "CLOUDBURROW_TEST_RUN_KMS", EnvKMS)),
		option.WithoutAuthentication(), option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = kc.Close() })
	parent := runParent(h)
	ring := "ar" + strings.ReplaceAll(uuid.NewString(), "-", "")[:10]
	if _, err := kc.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: parent, KeyRingId: ring}); err != nil {
		t.Fatalf("CreateKeyRing: %v", err)
	}
	key, err := kc.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: parent + "/keyRings/" + ring, CryptoKeyId: "repo",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}})
	if err != nil {
		t.Fatalf("CreateCryptoKey: %v", err)
	}
	version := key.GetName() + "/cryptoKeyVersions/1"
	repo := formatRepo(t, ctx, ar, parent, "cmek-docker", &artifactregistrypb.Repository{Format: artifactregistrypb.Repository_DOCKER,
		KmsKeyName: key.GetName()})
	if repo.GetKmsKeyName() != key.GetName() {
		t.Fatalf("the repository's kms_key_name is %q; want %q", repo.GetKmsKeyName(), key.GetName())
	}
	port := h.Endpoint(EnvRun)[strings.LastIndex(h.Endpoint(EnvRun), ":")+1:]
	ref := "us-central1-docker.pkg.localhost:" + port + "/" + h.Project() + "/cmek-docker/app"
	opts := registryOptions(h)
	img, err := random.Image(1024, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := crane.Push(img, ref+":v1", opts...); err != nil {
		t.Fatalf("push: %v", err)
	}
	digest, err := img.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := crane.Pull(ref+":v1", opts...); err != nil {
		t.Fatalf("pull: %v", err)
	}
	setState := func(s kmspb.CryptoKeyVersion_CryptoKeyVersionState) {
		t.Helper()
		if _, err := kc.UpdateCryptoKeyVersion(ctx, &kmspb.UpdateCryptoKeyVersionRequest{
			CryptoKeyVersion: &kmspb.CryptoKeyVersion{Name: version, State: s},
			UpdateMask:       &fieldmaskpb.FieldMask{Paths: []string{"state"}}}); err != nil {
			t.Fatalf("UpdateCryptoKeyVersion %v: %v", s, err)
		}
	}
	refused := func(when string) {
		t.Helper()
		if _, err := crane.Pull(ref+":v1", opts...); err == nil || !strings.Contains(err.Error(), "DENIED") {
			t.Errorf("pull %s = %v; want DENIED", when, err)
		}
		other, _ := random.Image(256, 1)
		if err := crane.Push(other, ref+":v2", opts...); err == nil {
			t.Errorf("push %s succeeded", when)
		}
		// Metadata stays readable: "Users with the Reader or Viewer role
		// can still see metadata such as artifact names, versions, and
		// tags".
		v, err := ar.ListVersions(ctx, &artifactregistrypb.ListVersionsRequest{Parent: repo.GetName() + "/packages/app"}).Next()
		if err != nil || !strings.HasSuffix(v.GetName(), digest.String()) {
			t.Errorf("ListVersions %s = %v, %v; want the pushed version", when, v, err)
		}
	}
	setState(kmspb.CryptoKeyVersion_DISABLED)
	refused("with the key version disabled")
	setState(kmspb.CryptoKeyVersion_ENABLED)
	if _, err := crane.Pull(ref+":v1", opts...); err != nil {
		t.Errorf("pull with the key version enabled again: %v", err)
	}
	if _, err := kc.DestroyCryptoKeyVersion(ctx, &kmspb.DestroyCryptoKeyVersionRequest{Name: version}); err != nil {
		t.Fatalf("DestroyCryptoKeyVersion: %v", err)
	}
	refused("with the key version scheduled for destruction")
}
