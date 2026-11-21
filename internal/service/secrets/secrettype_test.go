package secrets

import (
	"context"
	"testing"

	secretmanagerpb "cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// TestSecretTypeIsSetAtCreationAndImmutable covers secret_type (#1850):
// CreateSecret keeps it, GetSecret returns it, a secret made without one
// has none, an undefined number is refused, and an UpdateSecret whose mask
// names it is refused, the field being immutable.
func TestSecretTypeIsSetAtCreationAndImmutable(t *testing.T) {
	g, _ := newGRPC(t)
	ctx := context.Background()
	made, err := g.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{
		Parent: "projects/demo", SecretId: "typed",
		Secret: &secretmanagerpb.Secret{SecretType: secretmanagerpb.Secret_ACCESS_KEY,
			Replication: &secretmanagerpb.Replication{Replication: &secretmanagerpb.Replication_Automatic_{Automatic: &secretmanagerpb.Replication_Automatic{}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if made.GetSecretType() != secretmanagerpb.Secret_ACCESS_KEY {
		t.Fatalf("created type = %v", made.GetSecretType())
	}
	got, err := g.GetSecret(ctx, &secretmanagerpb.GetSecretRequest{Name: "projects/demo/secrets/typed"})
	if err != nil || got.GetSecretType() != secretmanagerpb.Secret_ACCESS_KEY {
		t.Fatalf("get type = %v, %v", got.GetSecretType(), err)
	}
	plain, err := g.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{
		Parent: "projects/demo", SecretId: "plain", Secret: &secretmanagerpb.Secret{}})
	if err != nil || plain.GetSecretType() != secretmanagerpb.Secret_SECRET_TYPE_UNSPECIFIED {
		t.Fatalf("untyped = %v, %v", plain.GetSecretType(), err)
	}
	_, err = g.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{
		Parent: "projects/demo", SecretId: "bad", Secret: &secretmanagerpb.Secret{SecretType: 7}})
	wantGRPCCode(t, err, codes.InvalidArgument)
	_, err = g.UpdateSecret(ctx, &secretmanagerpb.UpdateSecretRequest{
		Secret:     &secretmanagerpb.Secret{Name: "projects/demo/secrets/typed", SecretType: secretmanagerpb.Secret_CERTIFICATE},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"secret_type"}},
	})
	wantGRPCCode(t, err, codes.InvalidArgument)
	if err := CheckSecretType("NOPE"); err == nil {
		t.Fatal("CheckSecretType accepted NOPE")
	}
}
