//go:build compat

package compat

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"cloud.google.com/go/pubsub/v2"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	secretmanager "cloud.google.com/go/secretmanager/apiv1"
	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"cloud.google.com/go/storage"
	"github.com/googleapis/gax-go/v2/apierror"
	ak "google.golang.org/api/apikeys/v2"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

// newAPIKey creates a key in project with restrictions and returns its
// key string.
func newAPIKey(t *testing.T, h *Harness, c *ak.Service, project, prefix string, r *ak.V2Restrictions) string {
	t.Helper()
	parent := "projects/" + project + "/locations/global"
	id := strings.ToLower(v1ID(h, prefix))
	op, err := c.Projects.Locations.Keys.Create(parent, &ak.V2Key{DisplayName: prefix, Restrictions: r}).KeyId(id).Context(h.Context()).Do()
	if err != nil || !op.Done {
		t.Fatalf("keys.create = %+v, %v", op, err)
	}
	t.Cleanup(func() {
		_, _ = c.Projects.Locations.Keys.Delete(parent + "/keys/" + id).Context(context.Background()).Do()
	})
	ks, err := c.Projects.Locations.Keys.GetKeyString(parent + "/keys/" + id).Context(h.Context()).Do()
	if err != nil {
		t.Fatal(err)
	}
	return ks.KeyString
}

// keyRefusal is nil when err is Google's refusal with reason.
func keyRefusal(err error, reason, service string) error {
	if err == nil {
		return fmt.Errorf("the call succeeded")
	}
	var ae *apierror.APIError
	ok := errors.As(err, &ae)
	if !ok || ae.Reason() == "" {
		ae, ok = apierror.FromError(err)
	}
	if !ok || ae.Reason() != reason || ae.Domain() != "googleapis.com" || ae.Metadata()["service"] != service {
		return fmt.Errorf("want %s for %s: %v", reason, service, err)
	}
	return nil
}

// withKey sends key in x-goog-api-key on every gRPC call, as the clients
// do for option.WithAPIKey over TLS.
func withKey(key string) option.ClientOption {
	return option.WithGRPCDialOption(grpc.WithChainUnaryInterceptor(func(ctx context.Context, method string, req, reply any,
		cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		return invoker(metadata.AppendToOutgoingContext(ctx, "x-goog-api-key", key), method, req, reply, cc, opts...)
	}))
}

// covers: apikeys.projects.locations.keys.create, apikeys.projects.locations.keys.getKeyString
//
// TestAPIKeysEnforced (#1915), with the official clients: a call carrying
// a key that no key has is refused API_KEY_INVALID; a key restricted to
// Cloud Storage (API restrictions) serves Cloud Storage over REST and is
// refused API_KEY_SERVICE_BLOCKED by Secret Manager (in process, gRPC) and
// Pub/Sub (in the cluster, gRPC); a key restricted to websites is refused
// API_KEY_HTTP_REFERRER_BLOCKED for a call from no listed referrer.
func TestAPIKeysEnforced(t *testing.T) {
	h := New(t)
	c := apiKeysClient(t, h)
	h.Endpoint(EnvSecrets)
	h.Endpoint(EnvPubSub)
	storageOnly := newAPIKey(t, h, c, h.Project(), "st-", &ak.V2Restrictions{ApiTargets: []*ak.V2ApiTarget{{Service: "storage.googleapis.com"}}})
	sites := newAPIKey(t, h, c, h.Project(), "ws-", &ak.V2Restrictions{
		BrowserKeyRestrictions: &ak.V2BrowserKeyRestrictions{AllowedReferrers: []string{"https://app.example.com/*"}}})

	// Cloud Storage over REST, the key as option.WithAPIKey sends it.
	sc, err := storage.NewClient(h.Context(), option.WithEndpoint(h.Endpoint(EnvStorage)+"/storage/v1/"), option.WithAPIKey(storageOnly))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sc.Close() })
	it := sc.Buckets(h.Context(), h.Project())
	if _, err := it.Next(); err != nil && err != iterator.Done {
		t.Errorf("buckets.list with a key for Cloud Storage: %v", err)
	}
	bad, err := storage.NewClient(h.Context(), option.WithEndpoint(h.Endpoint(EnvStorage)+"/storage/v1/"), option.WithAPIKey("AIzaNotAKeyCloudBurrowEverMade0000000"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bad.Close() })
	_, err = bad.Buckets(h.Context(), h.Project()).Next()
	if err := keyRefusal(err, "API_KEY_INVALID", "storage.googleapis.com"); err != nil {
		t.Errorf("an unknown key: %v", err)
	}

	// Secret Manager, in process.
	smk := secretsClientWith(t, h, withKey(storageOnly))
	_, err = smk.ListSecrets(h.Context(), &secretmanagerpb.ListSecretsRequest{Parent: secretsParent(h)}).Next()
	if err := keyRefusal(err, "API_KEY_SERVICE_BLOCKED", "secretmanager.googleapis.com"); err != nil {
		t.Errorf("Secret Manager with a Cloud Storage key: %v", err)
	}

	// Pub/Sub, in the cluster.
	t.Setenv("PUBSUB_EMULATOR_HOST", h.Endpoint(EnvPubSub))
	pc, err := pubsub.NewClient(h.Context(), h.Project(), withKey(storageOnly))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	_, err = pc.TopicAdminClient.GetTopic(h.Context(), &pubsubpb.GetTopicRequest{Topic: "projects/" + h.Project() + "/topics/none"})
	if err := keyRefusal(err, "API_KEY_SERVICE_BLOCKED", "pubsub.googleapis.com"); err != nil {
		t.Errorf("Pub/Sub with a Cloud Storage key: %v", err)
	}

	// A websites key from no referrer.
	wc, err := storage.NewClient(h.Context(), option.WithEndpoint(h.Endpoint(EnvStorage)+"/storage/v1/"), option.WithAPIKey(sites))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = wc.Close() })
	_, err = wc.Buckets(h.Context(), h.Project()).Next()
	if err := keyRefusal(err, "API_KEY_HTTP_REFERRER_BLOCKED", "storage.googleapis.com"); err != nil {
		t.Errorf("a websites key with no referrer: %v", err)
	}
}

// secretsClientWith is secretsClient with extra options.
func secretsClientWith(t *testing.T, h *Harness, opts ...option.ClientOption) *secretmanager.Client {
	t.Helper()
	c, err := secretmanager.NewClient(h.Context(), append([]option.ClientOption{option.WithEndpoint(h.Endpoint(EnvSecrets)),
		option.WithoutAuthentication(), option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials()))}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}
