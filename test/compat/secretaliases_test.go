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

// TestSecretManagerVersionAliases (#1850): through the official client,
// UpdateSecret with the version_aliases mask sets a secret's aliases,
// GetSecret returns them, GetSecretVersion and AccessSecretVersion reach a
// version by its alias, and an alias breaking the documented rules (a
// letter first, never latest or NEW) is INVALID_ARGUMENT.
//
// covers: google.cloud.secretmanager.v1.SecretManagerService/UpdateSecret, google.cloud.secretmanager.v1.SecretManagerService/GetSecret, google.cloud.secretmanager.v1.SecretManagerService/GetSecretVersion, google.cloud.secretmanager.v1.SecretManagerService/AccessSecretVersion
func TestSecretManagerVersionAliases(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	sm := secretsClient(t, h)
	id := "aliases-" + h.Project()
	name := secretsParent(h) + "/secrets/" + id
	t.Cleanup(func() { _ = sm.DeleteSecret(context.Background(), &secretmanagerpb.DeleteSecretRequest{Name: name}) })
	if _, err := sm.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{Parent: secretsParent(h), SecretId: id,
		Secret: &secretmanagerpb.Secret{Replication: autoReplication()}}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"first", "second"} {
		if _, err := sm.AddSecretVersion(ctx, &secretmanagerpb.AddSecretVersionRequest{Parent: name,
			Payload: &secretmanagerpb.SecretPayload{Data: []byte(p)}}); err != nil {
			t.Fatal(err)
		}
	}
	update := func(aliases map[string]int64) (*secretmanagerpb.Secret, error) {
		return sm.UpdateSecret(ctx, &secretmanagerpb.UpdateSecretRequest{
			Secret:     &secretmanagerpb.Secret{Name: name, VersionAliases: aliases},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"version_aliases"}},
		})
	}
	if _, err := update(map[string]int64{"stable": 1, "canary": 2}); err != nil {
		t.Fatalf("UpdateSecret version_aliases: %v", err)
	}
	got, err := sm.GetSecret(ctx, &secretmanagerpb.GetSecretRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	if a := got.GetVersionAliases(); a["stable"] != 1 || a["canary"] != 2 || len(a) != 2 {
		t.Errorf("GetSecret version_aliases = %v", a)
	}
	acc, err := sm.AccessSecretVersion(ctx, &secretmanagerpb.AccessSecretVersionRequest{Name: name + "/versions/stable"})
	if err != nil {
		t.Fatalf("AccessSecretVersion by alias: %v", err)
	}
	if string(acc.GetPayload().GetData()) != "first" || acc.GetName() != name+"/versions/1" {
		t.Errorf("AccessSecretVersion(stable) = %s %q", acc.GetName(), acc.GetPayload().GetData())
	}
	v, err := sm.GetSecretVersion(ctx, &secretmanagerpb.GetSecretVersionRequest{Name: name + "/versions/canary"})
	if err != nil || v.GetName() != name+"/versions/2" {
		t.Errorf("GetSecretVersion(canary) = %v, %v", v.GetName(), err)
	}
	for _, bad := range []map[string]int64{{"1st": 1}, {"latest": 1}, {"NEW": 2}} {
		if _, err := update(bad); status.Code(err) != codes.InvalidArgument {
			t.Errorf("UpdateSecret aliases %v = %v; want INVALID_ARGUMENT", bad, err)
		}
	}
}
