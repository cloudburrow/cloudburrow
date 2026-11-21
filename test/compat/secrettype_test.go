//go:build compat

package compat

import (
	"context"
	"testing"

	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// TestSecretManagerSecretType (#1850): through the official client,
// CreateSecret keeps secret_type, GetSecret returns it, and
// an UpdateSecret whose mask names it is INVALID_ARGUMENT, the field being
// "Optional. Immutable." (resources.proto).
//
// covers: google.cloud.secretmanager.v1.SecretManagerService/CreateSecret, google.cloud.secretmanager.v1.SecretManagerService/GetSecret, google.cloud.secretmanager.v1.SecretManagerService/UpdateSecret
func TestSecretManagerSecretType(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	sm := secretsClient(t, h)
	id := "typed-" + h.Project()
	name := secretsParent(h) + "/secrets/" + id
	t.Cleanup(func() { _ = sm.DeleteSecret(context.Background(), &secretmanagerpb.DeleteSecretRequest{Name: name}) })
	made, err := sm.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{Parent: secretsParent(h), SecretId: id,
		Secret: &secretmanagerpb.Secret{Replication: autoReplication(), SecretType: secretmanagerpb.Secret_CERTIFICATE}})
	if err != nil {
		t.Fatal(err)
	}
	if made.GetSecretType() != secretmanagerpb.Secret_CERTIFICATE {
		t.Errorf("CreateSecret secret_type = %v", made.GetSecretType())
	}
	got, err := sm.GetSecret(ctx, &secretmanagerpb.GetSecretRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	if got.GetSecretType() != secretmanagerpb.Secret_CERTIFICATE {
		t.Errorf("GetSecret secret_type = %v", got.GetSecretType())
	}
	_, err = sm.UpdateSecret(ctx, &secretmanagerpb.UpdateSecretRequest{
		Secret:     &secretmanagerpb.Secret{Name: name, SecretType: secretmanagerpb.Secret_ACCESS_KEY},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"secret_type"}},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("UpdateSecret secret_type = %v; want INVALID_ARGUMENT", err)
	}
}
