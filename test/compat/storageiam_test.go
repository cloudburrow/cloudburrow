//go:build compat

package compat

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"cloud.google.com/go/iam"
	"cloud.google.com/go/iam/apiv1/iampb"
	"cloud.google.com/go/storage"
	exprpb "google.golang.org/genproto/googleapis/type/expr"
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
// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/storage/docs/json_api/v1/buckets/setIamPolicy
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

// TestStorageBucketIAMConditions (#1745), through the official client's
// version 3 IAM handle: a conditional binding is refused on a bucket
// without uniform bucket-level access ("To use conditions in the allow
// policy for a Cloud Storage bucket, you must enable uniform bucket-level
// access"), and stored and read back with its condition on one with it; a
// version 1 read renders the role ROLE_withcond_HASH without the
// condition; uniform bucket-level access cannot then be disabled.
//
// unverified: storage.buckets.setIamPolicy 400: the refusals' text (Google does not publish it)
// Docs re-read 2026-11-17, still unanswered: https://docs.cloud.google.com/storage/docs/access-control/using-iam-permissions
// covers: storage.buckets.getIamPolicy, storage.buckets.setIamPolicy
func TestStorageBucketIAMConditions(t *testing.T) {
	h := New(t)
	c := storageClient(t, h)
	ctx := h.Context()
	bh := iamBucket(t, h, c)
	expr := `resource.name.startsWith("projects/_/buckets/` + bh.BucketName() + `/objects/public/")`
	policy := func() *iam.Policy3 {
		return &iam.Policy3{Bindings: []*iampb.Binding{{Role: "roles/storage.objectViewer", Members: []string{"user:a@example.com"},
			Condition: &exprpb.Expr{Title: "public objects", Expression: expr}}}}
	}
	if code, _ := apiError(bh.IAM().V3().SetPolicy(ctx, policy())); code != http.StatusBadRequest {
		t.Errorf("a conditional binding without uniform bucket-level access = %d; want 400", code)
	}
	if _, err := bh.Update(ctx, storage.BucketAttrsToUpdate{UniformBucketLevelAccess: &storage.UniformBucketLevelAccess{Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	if err := bh.IAM().V3().SetPolicy(ctx, policy()); err != nil {
		t.Fatalf("a conditional binding with uniform bucket-level access: %v", err)
	}
	got, err := bh.IAM().V3().Policy(ctx)
	if err != nil || len(got.Bindings) != 1 || got.Bindings[0].GetCondition().GetExpression() != expr || got.Bindings[0].GetCondition().GetTitle() != "public objects" {
		t.Fatalf("version 3 read = %v, %v", got, err)
	}
	v1, err := bh.IAM().Policy(ctx)
	if err != nil || len(v1.Roles()) != 1 || !strings.HasPrefix(string(v1.Roles()[0]), "roles/storage.objectViewer_withcond_") {
		t.Errorf("version 1 read = %v, %v; want the ROLE_withcond_HASH rendering", v1, err)
	}
	if _, err := bh.Update(ctx, storage.BucketAttrsToUpdate{UniformBucketLevelAccess: &storage.UniformBucketLevelAccess{Enabled: false}}); err == nil {
		t.Error("uniform bucket-level access was disabled under a conditional binding")
	}
}
