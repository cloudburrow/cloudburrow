//go:build compat

package compat

import (
	"context"
	"net/http"
	"strings"
	"testing"

	secretmanager "cloud.google.com/go/secretmanager/apiv1"
	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// userManaged is a user-managed replication policy in the given locations.
func userManaged(locations ...string) *secretmanagerpb.Replication {
	um := &secretmanagerpb.Replication_UserManaged{}
	for _, l := range locations {
		um.Replicas = append(um.Replicas, &secretmanagerpb.Replication_UserManaged_Replica{Location: l})
	}
	return &secretmanagerpb.Replication{Replication: &secretmanagerpb.Replication_UserManaged_{UserManaged: um}}
}

// secretsRESTClient is the official client over its REST transport, which
// Secret Manager serves on the same port as gRPC.
func secretsRESTClient(t *testing.T, h *Harness) *secretmanager.Client {
	t.Helper()
	c, err := secretmanager.NewRESTClient(h.Context(),
		option.WithEndpoint("http://"+h.Endpoint(EnvSecrets)),
		option.WithoutAuthentication(),
	)
	if err != nil {
		t.Fatalf("secretmanager.NewRESTClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// TestSecretManagerUserManagedReplicasRoundTrip (#857): a secret created
// with user-managed replication in two locations reads back with both, in
// order, from CreateSecret, GetSecret and ListSecrets over gRPC and from
// GetSecret over REST. Replication is immutable (Secret.replication is
// IMMUTABLE): an UpdateSecret whose mask names it with a different policy is
// INVALID_ARGUMENT over both transports and changes nothing, and one that
// sends the policy unchanged is accepted, as AIP-203 specifies. A policy
// with customer-managed encryption is UNIMPLEMENTED naming the field.
// covers: google.cloud.secretmanager.v1.SecretManagerService/CreateSecret, google.cloud.secretmanager.v1.SecretManagerService/GetSecret, google.cloud.secretmanager.v1.SecretManagerService/ListSecrets, google.cloud.secretmanager.v1.SecretManagerService/UpdateSecret
func TestSecretManagerUserManagedReplicasRoundTrip(t *testing.T) {
	h := New(t)
	c := secretsClient(t, h)
	rc := secretsRESTClient(t, h)
	ctx := h.Context()
	want := userManaged("us-east1", "europe-west1")
	id := "user-managed-replicas"
	name := secretsParent(h) + "/secrets/" + id
	created, err := c.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{Parent: secretsParent(h), SecretId: id,
		Secret: &secretmanagerpb.Secret{Replication: want}})
	if err != nil {
		t.Fatalf("CreateSecret: %v", err)
	}
	t.Cleanup(func() { _ = c.DeleteSecret(context.Background(), &secretmanagerpb.DeleteSecretRequest{Name: name}) })
	if !proto.Equal(created.GetReplication(), want) {
		t.Errorf("CreateSecret returned replication %v; want %v", created.GetReplication(), want)
	}
	for transport, client := range map[string]*secretmanager.Client{"gRPC": c, "REST": rc} {
		got, err := client.GetSecret(ctx, &secretmanagerpb.GetSecretRequest{Name: name})
		if err != nil {
			t.Fatalf("GetSecret over %s: %v", transport, err)
		}
		if !proto.Equal(got.GetReplication(), want) {
			t.Errorf("GetSecret over %s: replication %v; want %v", transport, got.GetReplication(), want)
		}
	}
	listed := false
	it := c.ListSecrets(ctx, &secretmanagerpb.ListSecretsRequest{Parent: secretsParent(h)})
	for {
		s, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListSecrets: %v", err)
		}
		if s.GetName() == name {
			listed = true
			if !proto.Equal(s.GetReplication(), want) {
				t.Errorf("ListSecrets: replication %v; want %v", s.GetReplication(), want)
			}
		}
	}
	if !listed {
		t.Errorf("ListSecrets does not list %s", name)
	}

	auto := &secretmanagerpb.Replication{Replication: &secretmanagerpb.Replication_Automatic_{Automatic: &secretmanagerpb.Replication_Automatic{}}}
	for transport, client := range map[string]*secretmanager.Client{"gRPC": c, "REST": rc} {
		for change, repl := range map[string]*secretmanagerpb.Replication{
			"to automatic":          auto,
			"to another location":   userManaged("us-central1"),
			"to reordered replicas": userManaged("europe-west1", "us-east1"),
		} {
			_, err := client.UpdateSecret(ctx, &secretmanagerpb.UpdateSecretRequest{
				Secret:     &secretmanagerpb.Secret{Name: name, Replication: repl, Labels: map[string]string{"changed": "yes"}},
				UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"labels", "replication"}},
			})
			if status.Code(err) != codes.InvalidArgument {
				t.Errorf("UpdateSecret over %s changing replication %s = %v; want InvalidArgument", transport, change, err)
			}
		}
	}
	got, err := c.GetSecret(ctx, &secretmanagerpb.GetSecretRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(got.GetReplication(), want) || len(got.GetLabels()) != 0 {
		t.Errorf("after refused updates the secret has replication %v and labels %v", got.GetReplication(), got.GetLabels())
	}

	// Customer-managed encryption is not implemented: a policy carrying it
	// is refused naming the field, not stored without it.
	cmek := &secretmanagerpb.CustomerManagedEncryption{KmsKeyName: "projects/" + h.Project() + "/locations/us-east1/keyRings/r/cryptoKeys/k"}
	for field, repl := range map[string]*secretmanagerpb.Replication{
		"replication.automatic.customer_managed_encryption": {Replication: &secretmanagerpb.Replication_Automatic_{
			Automatic: &secretmanagerpb.Replication_Automatic{CustomerManagedEncryption: cmek}}},
		"replication.user_managed.replicas[].customer_managed_encryption": {Replication: &secretmanagerpb.Replication_UserManaged_{
			UserManaged: &secretmanagerpb.Replication_UserManaged{Replicas: []*secretmanagerpb.Replication_UserManaged_Replica{
				{Location: "us-east1", CustomerManagedEncryption: cmek}}}}},
	} {
		_, err := c.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{Parent: secretsParent(h), SecretId: "cmek-refused",
			Secret: &secretmanagerpb.Secret{Replication: repl}})
		if status.Code(err) != codes.Unimplemented || !strings.Contains(err.Error(), field) {
			_ = c.DeleteSecret(context.Background(), &secretmanagerpb.DeleteSecretRequest{Name: secretsParent(h) + "/secrets/cmek-refused"})
			t.Errorf("CreateSecret with %s = %v; want Unimplemented naming it", field, err)
		}
	}

	updated, err := c.UpdateSecret(ctx, &secretmanagerpb.UpdateSecretRequest{
		Secret:     &secretmanagerpb.Secret{Name: name, Replication: want, Labels: map[string]string{"team": "blue"}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"labels", "replication"}},
	})
	if err != nil {
		t.Fatalf("UpdateSecret with replication unchanged: %v", err)
	}
	if !proto.Equal(updated.GetReplication(), want) || updated.GetLabels()["team"] != "blue" {
		t.Errorf("UpdateSecret with replication unchanged returned replication %v, labels %v", updated.GetReplication(), updated.GetLabels())
	}
}

// TestConsoleSecretReplicationPolicy (#857): Create secret on the console
// with the User-managed policy and two locations makes a secret the
// official client reads back with both replicas, and one with Automatic
// reads back automatic. User-managed with no location, or a location with
// Automatic, is refused with a 400 and leaves no secret behind.
func TestConsoleSecretReplicationPolicy(t *testing.T) {
	h := New(t)
	addr := consoleAddr(t, h)
	c := secretsClient(t, h)
	ctx := h.Context()
	project := h.Project()
	for id, values := range map[string]map[string]string{
		"console-user-managed": {"replication": "User-managed", "locations": "us-east1, europe-west1"},
		"console-automatic":    {"replication": "Automatic"},
	} {
		name := secretsParent(h) + "/secrets/" + id
		t.Cleanup(func() { _ = c.DeleteSecret(context.Background(), &secretmanagerpb.DeleteSecretRequest{Name: name}) })
		values["secretId"], values["payload"] = id, "value"
		if code, body := consoleCreate(t, addr, "secrets", project, values); code != http.StatusOK {
			t.Fatalf("console create %s = %d: %s", id, code, body)
		}
	}
	um, err := c.GetSecret(ctx, &secretmanagerpb.GetSecretRequest{Name: secretsParent(h) + "/secrets/console-user-managed"})
	if err != nil {
		t.Fatal(err)
	}
	if want := userManaged("us-east1", "europe-west1"); !proto.Equal(um.GetReplication(), want) {
		t.Errorf("the console's user-managed secret reads replication %v; want %v", um.GetReplication(), want)
	}
	auto, err := c.GetSecret(ctx, &secretmanagerpb.GetSecretRequest{Name: secretsParent(h) + "/secrets/console-automatic"})
	if err != nil {
		t.Fatal(err)
	}
	if auto.GetReplication().GetAutomatic() == nil {
		t.Errorf("the console's automatic secret reads replication %v", auto.GetReplication())
	}

	for why, values := range map[string]map[string]string{
		"user-managed without a location": {"replication": "User-managed"},
		"automatic with a location":       {"replication": "Automatic", "locations": "us-east1"},
	} {
		values["secretId"], values["payload"] = "console-refused", "value"
		if code, body := consoleCreate(t, addr, "secrets", project, values); code != http.StatusBadRequest {
			t.Errorf("console create with %s = %d %s; want 400", why, code, body)
		}
		name := secretsParent(h) + "/secrets/console-refused"
		if _, err := c.GetSecret(ctx, &secretmanagerpb.GetSecretRequest{Name: name}); status.Code(err) != codes.NotFound {
			_ = c.DeleteSecret(context.Background(), &secretmanagerpb.DeleteSecretRequest{Name: name})
			t.Errorf("a refused console create with %s left a secret (%v)", why, err)
		}
	}
}
