//go:build compat

package compat

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	secretmanager "cloud.google.com/go/secretmanager/apiv1"
	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func autoReplication() *secretmanagerpb.Replication {
	return &secretmanagerpb.Replication{Replication: &secretmanagerpb.Replication_Automatic_{Automatic: &secretmanagerpb.Replication_Automatic{}}}
}

// TestSecretManagerExpiry (#1225): through the official client over gRPC
// and REST, a ttl on CreateSecret is answered as the expire_time it gives,
// an UpdateSecret whose mask names expire_time sets and (with none) clears
// it, and a secret with a short ttl is gone, NOT_FOUND over both
// transports, once it passes.
// covers: google.cloud.secretmanager.v1.SecretManagerService/CreateSecret, google.cloud.secretmanager.v1.SecretManagerService/UpdateSecret, google.cloud.secretmanager.v1.SecretManagerService/GetSecret
func TestSecretManagerExpiry(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	for transport, c := range map[string]*secretmanager.Client{"gRPC": secretsClient(t, h), "REST": secretsRESTClient(t, h)} {
		id := "expiry-" + strings.ToLower(transport)
		name := secretsParent(h) + "/secrets/" + id
		before := time.Now()
		created, err := c.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{Parent: secretsParent(h), SecretId: id,
			Secret: &secretmanagerpb.Secret{Replication: autoReplication(), Expiration: &secretmanagerpb.Secret_Ttl{Ttl: durationpb.New(time.Hour)}}})
		if err != nil {
			t.Fatalf("%s: CreateSecret with a ttl: %v", transport, err)
		}
		t.Cleanup(func() { _ = c.DeleteSecret(context.Background(), &secretmanagerpb.DeleteSecretRequest{Name: name}) })
		exp := created.GetExpireTime().AsTime()
		if created.GetExpireTime() == nil || exp.Before(before.Add(time.Hour-time.Minute)) || exp.After(time.Now().Add(time.Hour+time.Minute)) {
			t.Errorf("%s: expire_time = %v; want about an hour from now", transport, created.GetExpireTime())
		}
		at := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)
		upd, err := c.UpdateSecret(ctx, &secretmanagerpb.UpdateSecretRequest{
			Secret:     &secretmanagerpb.Secret{Name: name, Expiration: &secretmanagerpb.Secret_ExpireTime{ExpireTime: timestamppb.New(at)}},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"expire_time"}}})
		if err != nil || !upd.GetExpireTime().AsTime().Equal(at) {
			t.Errorf("%s: UpdateSecret expire_time = %v, %v; want %s", transport, upd.GetExpireTime(), err, at)
		}
		upd, err = c.UpdateSecret(ctx, &secretmanagerpb.UpdateSecretRequest{
			Secret: &secretmanagerpb.Secret{Name: name}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"expire_time"}}})
		if err != nil || upd.GetExpireTime() != nil {
			t.Errorf("%s: UpdateSecret clearing expire_time = %v, %v; want none", transport, upd.GetExpireTime(), err)
		}
		got, err := c.GetSecret(ctx, &secretmanagerpb.GetSecretRequest{Name: name})
		if err != nil || got.GetExpireTime() != nil {
			t.Errorf("%s: GetSecret after clearing = %v, %v", transport, got.GetExpireTime(), err)
		}

		shortID := "expiry-short-" + strings.ToLower(transport)
		short := secretsParent(h) + "/secrets/" + shortID
		if _, err := c.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{Parent: secretsParent(h), SecretId: shortID,
			Secret: &secretmanagerpb.Secret{Replication: autoReplication(), Expiration: &secretmanagerpb.Secret_Ttl{Ttl: durationpb.New(2 * time.Second)}}}); err != nil {
			t.Fatalf("%s: CreateSecret with a 2s ttl: %v", transport, err)
		}
		t.Cleanup(func() { _ = c.DeleteSecret(context.Background(), &secretmanagerpb.DeleteSecretRequest{Name: short}) })
		if _, err := c.AddSecretVersion(ctx, &secretmanagerpb.AddSecretVersionRequest{Parent: short, Payload: &secretmanagerpb.SecretPayload{Data: []byte("x")}}); err != nil {
			t.Fatalf("%s: AddSecretVersion: %v", transport, err)
		}
		deadline := time.Now().Add(20 * time.Second)
		for {
			_, err := c.GetSecret(ctx, &secretmanagerpb.GetSecretRequest{Name: short})
			if status.Code(err) == codes.NotFound {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: a secret with a 2s ttl still answers %v after 20s", transport, err)
			}
			time.Sleep(250 * time.Millisecond)
		}
	}
}

// TestSecretManagerListFilters (#1228): the filtering page's documented
// grammar through the official client over gRPC and REST, on ListSecrets
// and ListSecretVersions; an undocumented form is UNIMPLEMENTED naming it.
// covers: google.cloud.secretmanager.v1.SecretManagerService/ListSecrets, google.cloud.secretmanager.v1.SecretManagerService/ListSecretVersions
func TestSecretManagerListFilters(t *testing.T) {
	h := New(t)
	grpcClient := secretsClient(t, h)
	ctx := h.Context()
	run := strings.ToLower(strings.ReplaceAll(t.Name(), "/", "-"))
	scope := "labels.compat-run=" + run
	mk := func(id string, labels map[string]string, sec *secretmanagerpb.Secret) {
		t.Helper()
		labels["compat-run"] = run
		sec.Labels = labels
		if sec.Replication == nil {
			sec.Replication = autoReplication()
		}
		if _, err := grpcClient.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{Parent: secretsParent(h), SecretId: id, Secret: sec}); err != nil {
			t.Fatalf("CreateSecret %s: %v", id, err)
		}
		name := secretsParent(h) + "/secrets/" + id
		t.Cleanup(func() {
			_ = grpcClient.DeleteSecret(context.Background(), &secretmanagerpb.DeleteSecretRequest{Name: name})
		})
	}
	mk("filter-app-db", map[string]string{"environment": "production"}, &secretmanagerpb.Secret{})
	mk("filter-app-cache", map[string]string{"environment": "dev"}, &secretmanagerpb.Secret{
		Expiration: &secretmanagerpb.Secret_Ttl{Ttl: durationpb.New(24 * time.Hour)}})
	mk("filter-west", map[string]string{}, &secretmanagerpb.Secret{Replication: userManaged("us-west1")})
	versions := secretsParent(h) + "/secrets/filter-app-db"
	for i := 0; i < 3; i++ {
		if _, err := grpcClient.AddSecretVersion(ctx, &secretmanagerpb.AddSecretVersionRequest{Parent: versions, Payload: &secretmanagerpb.SecretPayload{Data: []byte("x")}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := grpcClient.DisableSecretVersion(ctx, &secretmanagerpb.DisableSecretVersionRequest{Name: versions + "/versions/2"}); err != nil {
		t.Fatal(err)
	}
	if _, err := grpcClient.DestroySecretVersion(ctx, &secretmanagerpb.DestroySecretVersionRequest{Name: versions + "/versions/1"}); err != nil {
		t.Fatal(err)
	}

	secretCases := map[string]string{
		"name:APP":                      "filter-app-cache filter-app-db",
		"labels.environment=production": "filter-app-db",
		"labels.environment:*":          "filter-app-cache filter-app-db",
		"NOT labels.environment:*":      "filter-west",
		"expire_time:*":                 "filter-app-cache",
		"replication.automatic:*":       "filter-app-cache filter-app-db",
		"replication.user_managed.replicas.location:(us-central1 OR us-west1)": "filter-west",
		"name:west OR labels.environment=dev":                                  "filter-app-cache filter-west",
		"create_time>2020-01-01T00:00:00Z":                                     "filter-app-cache filter-app-db filter-west",
		"create_time<2020-01-01":                                               "",
	}
	versionCases := map[string]string{
		"state:(ENABLED OR DISABLED)":                 "2 3",
		"state:DESTROYED AND destroy_time>2020-01-01": "1",
		"state=ENABLED":                               "3",
	}
	for transport, c := range map[string]*secretmanager.Client{"gRPC": grpcClient, "REST": secretsRESTClient(t, h)} {
		for filter, want := range secretCases {
			var got []string
			it := c.ListSecrets(ctx, &secretmanagerpb.ListSecretsRequest{Parent: secretsParent(h), Filter: scope + " AND (" + filter + ")"})
			for {
				s, err := it.Next()
				if err == iterator.Done {
					break
				}
				if err != nil {
					t.Fatalf("%s: ListSecrets %q: %v", transport, filter, err)
				}
				got = append(got, s.GetName()[strings.LastIndex(s.GetName(), "/")+1:])
			}
			sort.Strings(got)
			if strings.Join(got, " ") != want {
				t.Errorf("%s: ListSecrets filter %q = %v; want [%s]", transport, filter, got, want)
			}
		}
		for filter, want := range versionCases {
			var got []string
			it := c.ListSecretVersions(ctx, &secretmanagerpb.ListSecretVersionsRequest{Parent: versions, Filter: filter})
			for {
				v, err := it.Next()
				if err == iterator.Done {
					break
				}
				if err != nil {
					t.Fatalf("%s: ListSecretVersions %q: %v", transport, filter, err)
				}
				got = append(got, v.GetName()[strings.LastIndex(v.GetName(), "/")+1:])
			}
			sort.Strings(got)
			if strings.Join(got, " ") != want {
				t.Errorf("%s: ListSecretVersions filter %q = %v; want [%s]", transport, filter, got, want)
			}
		}
		for _, filter := range []string{`name ~ "filter-.*"`, "owner=me"} {
			_, err := c.ListSecrets(ctx, &secretmanagerpb.ListSecretsRequest{Parent: secretsParent(h), Filter: filter}).Next()
			if status.Code(err) != codes.Unimplemented {
				t.Errorf("%s: ListSecrets with the undocumented filter %q = %v; want Unimplemented", transport, filter, err)
			}
		}
	}
}
