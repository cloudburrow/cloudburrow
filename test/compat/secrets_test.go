//go:build compat

package compat

import (
	"fmt"
	"strings"
	"testing"

	secretmanager "cloud.google.com/go/secretmanager/apiv1"
	secretmanagerpb "cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// EnvSecrets points at the local Secret Manager endpoint.
const EnvSecrets = "CLOUDBURROW_TEST_SECRETS"

// secretsClient returns the official client pointed at the local adapter.
//
// Secret Manager has no emulator environment variable in any official client,
// so the endpoint and insecure credentials are explicit.
func secretsClient(t *testing.T, h *Harness) *secretmanager.Client {
	t.Helper()
	endpoint := h.Endpoint(EnvSecrets)
	c, err := secretmanager.NewClient(h.Context(),
		option.WithEndpoint(endpoint),
		option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())),
	)
	if err != nil {
		t.Fatalf("secretmanager.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func secretsParent(h *Harness) string { return "projects/" + h.Project() }

// TestSecretLifecycle drives create, read, list, update and delete through
// the official SDK.
func TestSecretLifecycle(t *testing.T) {
	h := New(t)
	c := secretsClient(t, h)
	ctx := h.Context()
	id := "compat-secret"
	name := secretsParent(h) + "/secrets/" + id

	created, err := c.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{
		Parent:   secretsParent(h),
		SecretId: id,
		Secret: &secretmanagerpb.Secret{
			Replication: &secretmanagerpb.Replication{
				Replication: &secretmanagerpb.Replication_Automatic_{
					Automatic: &secretmanagerpb.Replication_Automatic{},
				},
			},
			Labels: map[string]string{"env": "dev"},
		},
	})
	if err != nil {
		t.Fatalf("CreateSecret: %v", err)
	}
	t.Cleanup(func() {
		_ = c.DeleteSecret(h.Context(), &secretmanagerpb.DeleteSecretRequest{Name: name})
	})
	if created.GetName() != name {
		t.Errorf("name = %q, want %q", created.GetName(), name)
	}
	if created.GetCreateTime() == nil {
		t.Error("no create time was reported")
	}

	got, err := c.GetSecret(ctx, &secretmanagerpb.GetSecretRequest{Name: name})
	if err != nil {
		t.Fatalf("GetSecret: %v", err)
	}
	if got.GetLabels()["env"] != "dev" {
		t.Errorf("labels = %v", got.GetLabels())
	}

	it := c.ListSecrets(ctx, &secretmanagerpb.ListSecretsRequest{Parent: secretsParent(h)})
	found := false
	for {
		s, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListSecrets: %v", err)
		}
		if s.GetName() == name {
			found = true
		}
	}
	if !found {
		t.Error("ListSecrets omitted the secret just created")
	}

	if err := c.DeleteSecret(ctx, &secretmanagerpb.DeleteSecretRequest{Name: name}); err != nil {
		t.Fatalf("DeleteSecret: %v", err)
	}
	_, err = c.GetSecret(ctx, &secretmanagerpb.GetSecretRequest{Name: name})
	if status.Code(err) != codes.NotFound {
		t.Errorf("after deletion GetSecret returned %v, want NotFound", err)
	}
}

// TestSecretVersionLifecycle is the operation applications actually depend
// on: write a payload, read it back, and control its state.
func TestSecretVersionLifecycle(t *testing.T) {
	h := New(t)
	c := secretsClient(t, h)
	ctx := h.Context()
	id := "compat-versions"
	name := secretsParent(h) + "/secrets/" + id

	if _, err := c.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{
		Parent:   secretsParent(h),
		SecretId: id,
		Secret: &secretmanagerpb.Secret{Replication: &secretmanagerpb.Replication{
			Replication: &secretmanagerpb.Replication_Automatic_{
				Automatic: &secretmanagerpb.Replication_Automatic{},
			},
		}},
	}); err != nil {
		t.Fatalf("CreateSecret: %v", err)
	}
	t.Cleanup(func() {
		_ = c.DeleteSecret(h.Context(), &secretmanagerpb.DeleteSecretRequest{Name: name})
	})

	for i, payload := range []string{"first", "second"} {
		v, err := c.AddSecretVersion(ctx, &secretmanagerpb.AddSecretVersionRequest{
			Parent:  name,
			Payload: &secretmanagerpb.SecretPayload{Data: []byte(payload)},
		})
		if err != nil {
			t.Fatalf("AddSecretVersion(%s): %v", payload, err)
		}
		want := fmt.Sprintf("%s/versions/%d", name, i+1)
		if v.GetName() != want {
			t.Errorf("version name = %q, want %q", v.GetName(), want)
		}
	}

	// "latest" is the most recently created version, per the contract.
	accessed, err := c.AccessSecretVersion(ctx, &secretmanagerpb.AccessSecretVersionRequest{
		Name: name + "/versions/latest",
	})
	if err != nil {
		t.Fatalf("AccessSecretVersion(latest): %v", err)
	}
	if string(accessed.GetPayload().GetData()) != "second" {
		t.Errorf("latest = %q, want the most recently created version", accessed.GetPayload().GetData())
	}
	if !strings.HasSuffix(accessed.GetName(), "/versions/2") {
		t.Errorf("access returned %q, want the concrete version name", accessed.GetName())
	}

	// An earlier version stays readable by number.
	first, err := c.AccessSecretVersion(ctx, &secretmanagerpb.AccessSecretVersionRequest{
		Name: name + "/versions/1",
	})
	if err != nil {
		t.Fatalf("AccessSecretVersion(1): %v", err)
	}
	if string(first.GetPayload().GetData()) != "first" {
		t.Errorf("version 1 = %q", first.GetPayload().GetData())
	}

	// Disable, and the payload must become inaccessible without the version
	// disappearing.
	if _, err := c.DisableSecretVersion(ctx, &secretmanagerpb.DisableSecretVersionRequest{
		Name: name + "/versions/1",
	}); err != nil {
		t.Fatalf("DisableSecretVersion: %v", err)
	}
	_, err = c.AccessSecretVersion(ctx, &secretmanagerpb.AccessSecretVersionRequest{
		Name: name + "/versions/1",
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Errorf("accessing a disabled version returned %v, want FailedPrecondition", err)
	}
	meta, err := c.GetSecretVersion(ctx, &secretmanagerpb.GetSecretVersionRequest{
		Name: name + "/versions/1",
	})
	if err != nil {
		t.Fatalf("GetSecretVersion on a disabled version: %v", err)
	}
	if meta.GetState() != secretmanagerpb.SecretVersion_DISABLED {
		t.Errorf("state = %v, want DISABLED", meta.GetState())
	}

	if _, err := c.EnableSecretVersion(ctx, &secretmanagerpb.EnableSecretVersionRequest{
		Name: name + "/versions/1",
	}); err != nil {
		t.Fatalf("EnableSecretVersion: %v", err)
	}
	if _, err := c.AccessSecretVersion(ctx, &secretmanagerpb.AccessSecretVersionRequest{
		Name: name + "/versions/1",
	}); err != nil {
		t.Errorf("a re-enabled version is inaccessible: %v", err)
	}

	// Destroy is terminal.
	destroyed, err := c.DestroySecretVersion(ctx, &secretmanagerpb.DestroySecretVersionRequest{
		Name: name + "/versions/1",
	})
	if err != nil {
		t.Fatalf("DestroySecretVersion: %v", err)
	}
	if destroyed.GetState() != secretmanagerpb.SecretVersion_DESTROYED {
		t.Errorf("state = %v, want DESTROYED", destroyed.GetState())
	}
	if destroyed.GetDestroyTime() == nil {
		t.Error("no destroy time was reported")
	}
	_, err = c.EnableSecretVersion(ctx, &secretmanagerpb.EnableSecretVersionRequest{
		Name: name + "/versions/1",
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Errorf("re-enabling a destroyed version returned %v, want FailedPrecondition", err)
	}

	// Every version must appear in a listing, including the destroyed one.
	vit := c.ListSecretVersions(ctx, &secretmanagerpb.ListSecretVersionsRequest{Parent: name})
	count := 0
	for {
		if _, err := vit.Next(); err == iterator.Done {
			break
		} else if err != nil {
			t.Fatalf("ListSecretVersions: %v", err)
		}
		count++
	}
	if count != 2 {
		t.Errorf("ListSecretVersions returned %d versions, want 2", count)
	}
}

func TestSecretErrorsUseGoogleCodes(t *testing.T) {
	h := New(t)
	c := secretsClient(t, h)
	ctx := h.Context()
	id := "compat-errors"
	name := secretsParent(h) + "/secrets/" + id

	_, err := c.GetSecret(ctx, &secretmanagerpb.GetSecretRequest{Name: name})
	if status.Code(err) != codes.NotFound {
		t.Errorf("missing secret returned %v, want NotFound", err)
	}

	create := &secretmanagerpb.CreateSecretRequest{
		Parent:   secretsParent(h),
		SecretId: id,
		Secret: &secretmanagerpb.Secret{Replication: &secretmanagerpb.Replication{
			Replication: &secretmanagerpb.Replication_Automatic_{
				Automatic: &secretmanagerpb.Replication_Automatic{},
			},
		}},
	}
	if _, err := c.CreateSecret(ctx, create); err != nil {
		t.Fatalf("CreateSecret: %v", err)
	}
	t.Cleanup(func() {
		_ = c.DeleteSecret(h.Context(), &secretmanagerpb.DeleteSecretRequest{Name: name})
	})

	if _, err := c.CreateSecret(ctx, create); status.Code(err) != codes.AlreadyExists {
		t.Errorf("duplicate secret returned %v, want AlreadyExists", err)
	}

	_, err = c.AddSecretVersion(ctx, &secretmanagerpb.AddSecretVersionRequest{
		Parent:  name,
		Payload: &secretmanagerpb.SecretPayload{Data: nil},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("empty payload returned %v, want InvalidArgument", err)
	}

	_, err = c.AccessSecretVersion(ctx, &secretmanagerpb.AccessSecretVersionRequest{
		Name: name + "/versions/latest",
	})
	if status.Code(err) != codes.NotFound {
		t.Errorf("latest on a secret with no versions returned %v, want NotFound", err)
	}
}
