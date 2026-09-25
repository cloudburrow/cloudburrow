package storage

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"cloud.google.com/go/iam/apiv1/iampb"
)

// A policy set through the official client reads back, with a new etag,
// and grants or denies nothing (#504, ADR-0006).
func TestStorageBucketIAMRoundTripInProcess(t *testing.T) {
	_, bh, h := sdkBucket(t, "iam-bucket")
	ctx := context.Background()
	p, err := bh.IAM().Policy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Roles()) != 0 {
		t.Errorf("a new bucket's policy = %v; want no bindings", p.Roles())
	}
	p.Add("user:dev@example.com", "roles/storage.objectViewer")
	if err := bh.IAM().SetPolicy(ctx, p); err != nil {
		t.Fatal(err)
	}
	got, err := bh.IAM().Policy(ctx)
	if err != nil || !got.HasRole("user:dev@example.com", "roles/storage.objectViewer") {
		t.Fatalf("after set: %v, %v", got, err)
	}
	perms, err := bh.IAM().TestPermissions(ctx, []string{"storage.objects.get", "storage.buckets.delete"})
	if err != nil || len(perms) != 2 {
		t.Errorf("testIamPermissions = %v, %v; want every requested permission", perms, err)
	}
	if code, _ := raw(t, "GET", h.URL+"/storage/v1/b/no-such-bucket/iam/testPermissions?permissions=storage.objects.get", ""); code != http.StatusNotFound {
		t.Errorf("testIamPermissions on a missing bucket = %d", code)
	}
	// A version 3 policy without conditions is accepted.
	v3, err := bh.IAM().V3().Policy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	v3.Bindings = append(v3.Bindings, &iampb.Binding{Role: "roles/storage.admin", Members: []string{"group:ops@example.com"}})
	if err := bh.IAM().V3().SetPolicy(ctx, v3); err != nil {
		t.Errorf("a version 3 policy without conditions: %v", err)
	}
}

// A set carrying a stale etag is refused.
//
// unverified: storage.buckets.setIamPolicy 412: a stale etag (the JSON API's precondition code; no page states it for this method)
func TestStorageBucketIAMStaleEtagInProcess(t *testing.T) {
	_, bh, _ := sdkBucket(t, "iam-stale")
	ctx := context.Background()
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
	if code, _ := apiCode(bh.IAM().SetPolicy(ctx, stale)); code != http.StatusPreconditionFailed {
		t.Errorf("a stale etag = %d; want 412", code)
	}
}

// A conditional binding is 501 naming the field, never stored and ignored.
func TestStorageBucketIAMConditionUnimplementedInProcess(t *testing.T) {
	_, _, h := sdkBucket(t, "iam-cond")
	code, body := raw(t, "PUT", h.URL+"/storage/v1/b/iam-cond/iam",
		`{"version":3,"bindings":[{"role":"roles/storage.objectViewer","members":["user:a@example.com"],"condition":{"expression":"true","title":"t"}}]}`)
	if code != http.StatusNotImplemented || !strings.Contains(body, "condition") {
		t.Errorf("a conditional binding = %d %s; want 501 naming the condition", code, body)
	}
	if _, body := raw(t, "GET", h.URL+"/storage/v1/b/iam-cond/iam", ""); strings.Contains(body, "objectViewer") {
		t.Errorf("the refused policy was stored: %s", body)
	}
}

// ACL methods stay 501 by name.
func TestStorageACLNotImplementedInProcess(t *testing.T) {
	_, _, h := sdkBucket(t, "acl-bucket")
	for path, method := range map[string]string{
		"/storage/v1/b/acl-bucket/acl":              "storage.bucketAccessControls.list",
		"/storage/v1/b/acl-bucket/defaultObjectAcl": "storage.defaultObjectAccessControls.list",
		"/storage/v1/b/acl-bucket/o/x/acl":          "storage.objectAccessControls.list",
		"/storage/v1/b/acl-bucket/o/x/iam":          "storage.objects.getIamPolicy",
	} {
		if code, body := raw(t, "GET", h.URL+path, ""); code != http.StatusNotImplemented || !strings.Contains(body, method) {
			t.Errorf("GET %s = %d %s; want 501 naming %s", path, code, body, method)
		}
	}
}
