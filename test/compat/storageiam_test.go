//go:build compat

package compat

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"cloud.google.com/go/storage"
)

// Bucket IAM (#504), stored and not enforced under ADR-0006 as amended for
// Cloud Storage.

func iamBucket(t *testing.T, h *Harness, c *storage.Client) *storage.BucketHandle {
	t.Helper()
	bh := c.Bucket(h.Project() + "-iam")
	if err := bh.Create(h.Context(), h.Project(), nil); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	t.Cleanup(func() { _ = bh.Delete(context.Background()) })
	return bh
}

// TestStorageBucketIAMRoundTrip: a policy set through the official client
// reads back with its binding, and testIamPermissions returns every
// requested permission.
// covers: storage.buckets.getIamPolicy, storage.buckets.setIamPolicy, storage.buckets.testIamPermissions
func TestStorageBucketIAMRoundTrip(t *testing.T) {
	h := New(t)
	c := storageClient(t, h)
	ctx := h.Context()
	bh := iamBucket(t, h, c)
	p, err := bh.IAM().Policy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	p.Add("user:dev@example.com", "roles/storage.objectViewer")
	if err := bh.IAM().SetPolicy(ctx, p); err != nil {
		t.Fatal(err)
	}
	got, err := bh.IAM().Policy(ctx)
	if err != nil || !got.HasRole("user:dev@example.com", "roles/storage.objectViewer") {
		t.Fatalf("policy after set = %v, %v", got, err)
	}
	perms, err := bh.IAM().TestPermissions(ctx, []string{"storage.objects.get", "storage.objects.delete"})
	if err != nil || len(perms) != 2 {
		t.Errorf("testIamPermissions = %v, %v; want every requested permission (nothing is enforced)", perms, err)
	}
}

// TestStorageBucketIAMStaleEtag: a set carrying a stale etag is refused, so
// read-modify-write loops behave as against Google.
//
// unverified: storage.buckets.setIamPolicy 412: a stale etag (the JSON API's precondition code; no page states it for this method)
// covers: storage.buckets.setIamPolicy
func TestStorageBucketIAMStaleEtag(t *testing.T) {
	h := New(t)
	c := storageClient(t, h)
	ctx := h.Context()
	bh := iamBucket(t, h, c)
	stale, err := bh.IAM().Policy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	fresh, _ := bh.IAM().Policy(ctx)
	fresh.Add("user:a@example.com", "roles/storage.objectViewer")
	if err := bh.IAM().SetPolicy(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	stale.Add("user:b@example.com", "roles/storage.objectViewer")
	if code, _ := apiError(bh.IAM().SetPolicy(ctx, stale)); code != http.StatusPreconditionFailed {
		t.Errorf("a set with a stale etag = %d; want 412", code)
	}
}

// TestStorageBucketIAMConditionUnimplemented: a conditional binding is 501
// naming the condition, never stored and ignored (ADR-0006 rule 3).
func TestStorageBucketIAMConditionUnimplemented(t *testing.T) {
	h := New(t)
	c := storageClient(t, h)
	bh := iamBucket(t, h, c)
	code, body := rawStorage(t, h, "PUT", "/storage/v1/b/"+bh.BucketName()+"/iam",
		`{"version":3,"bindings":[{"role":"roles/storage.objectViewer","members":["user:a@example.com"],"condition":{"title":"t","expression":"true"}}]}`)
	if code != http.StatusNotImplemented || !strings.Contains(body, "condition") {
		t.Errorf("a conditional binding = %d %s; want 501 naming the condition", code, body)
	}
}

// TestStorageACLNotImplemented: bucket, object and default object ACLs are
// 501 naming the method, never an empty list.
func TestStorageACLNotImplemented(t *testing.T) {
	h := New(t)
	c := storageClient(t, h)
	bh := iamBucket(t, h, c)
	for path, method := range map[string]string{
		"/acl":              "storage.bucketAccessControls.list",
		"/defaultObjectAcl": "storage.defaultObjectAccessControls.list",
		"/o/x/acl":          "storage.objectAccessControls.list",
	} {
		code, body := rawStorage(t, h, "GET", "/storage/v1/b/"+bh.BucketName()+path, "")
		if code != http.StatusNotImplemented || !strings.Contains(body, method) {
			t.Errorf("GET %s = %d %s; want 501 naming %s", path, code, body, method)
		}
	}
}
