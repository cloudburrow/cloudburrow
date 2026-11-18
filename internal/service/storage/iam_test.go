package storage

import (
	"bytes"
	"context"
	"encoding/json"
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
// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/storage/docs/json_api/v1/buckets/setIamPolicy
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

// #1745: a conditional binding needs uniform bucket-level access and
// version 3; it is stored, rendered _withcond_ to a version 1 reader, kept
// through a version 1 read-modify-write, and keeps uniform bucket-level
// access from being disabled.
func TestStorageBucketIAMConditionsInProcess(t *testing.T) {
	_, _, h := sdkBucket(t, "iam-cond")
	iam := h.URL + "/storage/v1/b/iam-cond/iam"
	cond := `{"version":3,"bindings":[{"role":"roles/storage.objectViewer","members":["user:a@example.com"],"condition":{"expression":"resource.name.startsWith(\"projects/_/buckets/iam-cond/objects/pub/\")","title":"pub"}}]}`
	if code, body := rawc(t, "PUT", iam, cond); code != http.StatusBadRequest || !strings.Contains(body, "uniformBucketLevelAccess") {
		t.Errorf("a conditional binding without uniform access = %d %s; want 400", code, body)
	}
	if code, body := rawc(t, "PATCH", h.URL+"/storage/v1/b/iam-cond", `{"iamConfiguration":{"uniformBucketLevelAccess":{"enabled":true}}}`); code != 200 {
		t.Fatalf("enable uniform access = %d %s", code, body)
	}
	if code, body := rawc(t, "PUT", iam, strings.Replace(cond, `"version":3`, `"version":1`, 1)); code != http.StatusBadRequest || !strings.Contains(body, "version 3") {
		t.Errorf("a version 1 conditional policy = %d %s; want 400", code, body)
	}
	if code, body := rawc(t, "PUT", iam, strings.Replace(cond, `startsWith`, `contains`, 1)); code != http.StatusBadRequest {
		t.Errorf("a bad expression = %d %s; want 400", code, body)
	}
	if code, body := rawc(t, "PUT", iam, cond); code != 200 || !strings.Contains(body, `"title":"pub"`) || !strings.Contains(body, `"version":3`) {
		t.Fatalf("a conditional binding = %d %s; want it stored", code, body)
	}
	_, v1 := rawc(t, "GET", iam, "")
	if !strings.Contains(v1, "roles/storage.objectViewer_withcond_") || strings.Contains(v1, `"condition"`) || !strings.Contains(v1, `"version":1`) {
		t.Errorf("version 1 read = %s; want the _withcond_ role without the condition", v1)
	}
	if _, v3 := rawc(t, "GET", iam+"?optionsRequestedPolicyVersion=3", ""); !strings.Contains(v3, `"title":"pub"`) {
		t.Errorf("version 3 read = %s", v3)
	}
	// A version 1 read-modify-write keeps the condition.
	if code, body := rawc(t, "PUT", iam, v1); code != 200 || !strings.Contains(body, `"title":"pub"`) {
		t.Errorf("a version 1 write back = %d %s; want the condition kept", code, body)
	}
	if code, body := rawc(t, "PATCH", h.URL+"/storage/v1/b/iam-cond", `{"iamConfiguration":{"uniformBucketLevelAccess":{"enabled":false}}}`); code != http.StatusBadRequest || !strings.Contains(body, "conditional") {
		t.Errorf("disable uniform access with a condition = %d %s; want 400", code, body)
	}
}

// rawc is raw with the answer's JSON compacted.
func rawc(t *testing.T, method, url, body string) (int, string) {
	t.Helper()
	code, out := raw(t, method, url, body)
	var buf bytes.Buffer
	if json.Compact(&buf, []byte(out)) == nil {
		out = buf.String()
	}
	return code, out
}

// Object IAM stays 501 by name; the ACL methods are built (acl_test.go).
func TestStorageObjectIAMNotImplementedInProcess(t *testing.T) {
	_, _, h := sdkBucket(t, "acl-bucket")
	if code, body := raw(t, "GET", h.URL+"/storage/v1/b/acl-bucket/o/x/iam", ""); code != http.StatusNotImplemented || !strings.Contains(body, "storage.objects.getIamPolicy") {
		t.Errorf("GET o/x/iam = %d %s; want 501 naming storage.objects.getIamPolicy", code, body)
	}
}
