//go:build compat

package compat

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"cloud.google.com/go/storage"
	"google.golang.org/api/iterator"
)

func adminSeed(t *testing.T, control, doc string) (int, string) {
	t.Helper()
	resp, err := http.Post("http://"+control+"/admin/seed", "application/json", strings.NewReader(doc))
	if err != nil {
		t.Fatalf("POST /admin/seed: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// seedDocument covers Storage, Pub/Sub and Secret Manager in one document,
// named after the test's project so runs cannot collide. skip sets
// ifNotExists on every component.
func seedDocument(project, bucket string, skip bool) string {
	return fmt.Sprintf(`{"components": {
	"storage": {"ifNotExists": %[3]t, "buckets": [{"name": %[2]q, "objects": [
		{"name": "config/app.json", "content": "{\"debug\": true}", "contentType": "application/json", "metadata": {"owner": "seed"}},
		{"name": "bin", "contentBase64": "AAEC/w=="}
	]}]},
	"pubsub": {"ifNotExists": %[3]t,
		"topics": [{"name": "projects/%[1]s/topics/orders"}, {"name": "projects/%[1]s/topics/orders-dlq"}],
		"subscriptions": [{"name": "projects/%[1]s/subscriptions/orders-sub", "topic": "projects/%[1]s/topics/orders",
			"ackDeadlineSeconds": 42, "filter": "attributes.kind = \"paid\"",
			"deadLetterPolicy": {"deadLetterTopic": "projects/%[1]s/topics/orders-dlq", "maxDeliveryAttempts": 7}}]},
	"secretmanager": {"ifNotExists": %[3]t, "secrets": [{"name": "projects/%[1]s/secrets/api-key",
		"labels": {"env": "dev"}, "versions": [{"data": "first"}, {"data": "second"}]}]}
}}`, project, bucket, skip)
}

// TestOneSeedDocumentIsReadBackByTheOfficialSDKs.
//
// Seeding covered Cloud Tasks only (#275). One document now seeds Storage,
// Pub/Sub and Secret Manager, and everything in it must be what the official
// clients read back — bytes, metadata and subscription configuration included.
// A repeat is a conflict, and ifNotExists makes it a no-op that duplicates
// nothing.
func TestOneSeedDocumentIsReadBackByTheOfficialSDKs(t *testing.T) {
	h := New(t)
	control := h.Endpoint(EnvControl)
	project := h.Project()
	bucket := project + "-seed"
	sc := storageClient(t, h)
	t.Cleanup(func() {
		it := sc.Bucket(bucket).Objects(h.Context(), nil)
		for {
			o, err := it.Next()
			if err != nil {
				break
			}
			_ = sc.Bucket(bucket).Object(o.Name).Delete(h.Context())
		}
		_ = sc.Bucket(bucket).Delete(h.Context())
		adminReset(t, control, "service=pubsub,secretmanager&project="+project)
	})

	if code, body := adminSeed(t, control, seedDocument(project, bucket, false)); code != http.StatusOK {
		t.Fatalf("seed: %d %s", code, body)
	}

	// Storage: bucket, object bytes, content type and metadata.
	if _, err := sc.Bucket(bucket).Attrs(h.Context()); err != nil {
		t.Fatalf("bucket attrs: %v", err)
	}
	obj := sc.Bucket(bucket).Object("config/app.json")
	r, err := obj.NewReader(h.Context())
	if err != nil {
		t.Fatalf("read seeded object: %v", err)
	}
	data, _ := io.ReadAll(r)
	_ = r.Close()
	if string(data) != `{"debug": true}` {
		t.Errorf("object bytes = %q", data)
	}
	oattrs, err := obj.Attrs(h.Context())
	if err != nil {
		t.Fatal(err)
	}
	if oattrs.ContentType != "application/json" || oattrs.Metadata["owner"] != "seed" {
		t.Errorf("object attrs: content type %q, metadata %v", oattrs.ContentType, oattrs.Metadata)
	}
	r, err = sc.Bucket(bucket).Object("bin").NewReader(h.Context())
	if err != nil {
		t.Fatal(err)
	}
	data, _ = io.ReadAll(r)
	_ = r.Close()
	if string(data) != "\x00\x01\x02\xff" {
		t.Errorf("base64 object bytes = %v", data)
	}

	// Pub/Sub: the subscription's configuration as the client reads it.
	pc := pubsubClient(t, h)
	sub, err := pc.SubscriptionAdminClient.GetSubscription(h.Context(),
		&pubsubpb.GetSubscriptionRequest{Subscription: "projects/" + project + "/subscriptions/orders-sub"})
	if err != nil {
		t.Fatalf("GetSubscription: %v", err)
	}
	if sub.AckDeadlineSeconds != 42 || sub.Filter != `attributes.kind = "paid"` ||
		sub.GetDeadLetterPolicy().GetDeadLetterTopic() != "projects/"+project+"/topics/orders-dlq" ||
		sub.GetDeadLetterPolicy().GetMaxDeliveryAttempts() != 7 {
		t.Errorf("subscription = %v", sub)
	}

	// Secret Manager: the latest version is the last listed.
	smc := secretsClient(t, h)
	secret := "projects/" + project + "/secrets/api-key"
	v, err := smc.AccessSecretVersion(h.Context(), &secretmanagerpb.AccessSecretVersionRequest{Name: secret + "/versions/latest"})
	if err != nil {
		t.Fatalf("AccessSecretVersion: %v", err)
	}
	if string(v.Payload.Data) != "second" {
		t.Errorf("latest payload = %q", v.Payload.Data)
	}

	// A repeat conflicts; with ifNotExists it succeeds and duplicates nothing.
	if code, body := adminSeed(t, control, seedDocument(project, bucket, false)); code != http.StatusConflict {
		t.Errorf("an identical second seed returned %d %s, want 409", code, body)
	}
	if code, body := adminSeed(t, control, seedDocument(project, bucket, true)); code != http.StatusOK {
		t.Errorf("a repeat with ifNotExists returned %d %s", code, body)
	}
	versions := 0
	it := smc.ListSecretVersions(h.Context(), &secretmanagerpb.ListSecretVersionsRequest{Parent: secret})
	for {
		if _, err := it.Next(); errors.Is(err, iterator.Done) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		versions++
	}
	if versions != 2 {
		t.Errorf("%d secret versions after the repeats, want 2", versions)
	}
	objects := 0
	oit := sc.Bucket(bucket).Objects(h.Context(), &storage.Query{})
	for {
		if _, err := oit.Next(); errors.Is(err, iterator.Done) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		objects++
	}
	if objects != 2 {
		t.Errorf("%d objects after the repeats, want 2", objects)
	}
}

// TestAnInvalidSeedDocumentSeedsNothing.
//
// One bad field anywhere must leave every service untouched: a half-applied
// seed is a state no test asked for.
func TestAnInvalidSeedDocumentSeedsNothing(t *testing.T) {
	h := New(t)
	control := h.Endpoint(EnvControl)
	project := h.Project()
	bucket := project + "-bad"

	// Valid Storage and Secret Manager components, and a Pub/Sub topic with
	// a schema the emulator is not known to honour.
	doc := fmt.Sprintf(`{"components": {
	"storage": {"buckets": [{"name": %[2]q}]},
	"secretmanager": {"secrets": [{"name": "projects/%[1]s/secrets/never"}]},
	"pubsub": {"topics": [{"name": "projects/%[1]s/topics/typed", "schemaSettings": {"schema": "projects/%[1]s/schemas/s"}}]}
}}`, project, bucket)
	code, body := adminSeed(t, control, doc)
	if code != http.StatusBadRequest || !strings.Contains(body, "schemaSettings") {
		t.Fatalf("seed returned %d %s, want 400 naming schemaSettings", code, body)
	}

	if _, err := storageClient(t, h).Bucket(bucket).Attrs(h.Context()); !errors.Is(err, storage.ErrBucketNotExist) {
		t.Errorf("the bucket from a refused seed exists (err %v)", err)
	}
	// A bucket field the seed format does not take is refused by name too,
	// and seeds nothing.
	code, body = adminSeed(t, control, fmt.Sprintf(`{"components": {"storage": {"buckets": [{"name": %q, "versioning": true}]}}}`, bucket))
	if code != http.StatusBadRequest || !strings.Contains(body, "versioning") {
		t.Errorf("a bucket with versioning returned %d %s, want 400 naming versioning", code, body)
	}
	if _, err := storageClient(t, h).Bucket(bucket).Attrs(h.Context()); !errors.Is(err, storage.ErrBucketNotExist) {
		t.Errorf("the bucket from the refused versioning seed exists (err %v)", err)
	}

	if _, err := secretsClient(t, h).GetSecret(h.Context(),
		&secretmanagerpb.GetSecretRequest{Name: "projects/" + project + "/secrets/never"}); err == nil {
		t.Error("the secret from a refused seed exists")
	}
}
