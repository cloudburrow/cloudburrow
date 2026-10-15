//go:build compat

package compat

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"cloud.google.com/go/storage"
	"google.golang.org/api/googleapi"
)

// Cloud Storage ACLs (#1233), stored and returned, never enforced, under
// ADR-0006 as amended 2026-10-15.

func aclRule(rules []storage.ACLRule, entity storage.ACLEntity, role storage.ACLRole) bool {
	for _, r := range rules {
		if r.Entity == entity && r.Role == role {
			return true
		}
	}
	return false
}

func aclCode(err error) int {
	var ge *googleapi.Error
	if errors.As(err, &ge) {
		return ge.Code
	}
	return 0
}

// TestStorageBucketACL: the Go client's bucket ACLHandle lists the
// projectPrivate default, by project number, sets, and deletes; Attrs
// returns the ACL.
// covers: storage.bucketAccessControls.list, storage.bucketAccessControls.update, storage.bucketAccessControls.delete, storage.buckets.get
func TestStorageBucketACL(t *testing.T) {
	h := New(t)
	c := storageClient(t, h)
	ctx := h.Context()
	bh := c.Bucket(h.Project() + "-acl")
	if err := bh.Create(ctx, h.Project(), nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bh.Delete(context.Background()) })
	rules, err := bh.ACL().List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 3 {
		t.Errorf("default bucket ACL = %+v; want projectPrivate's three entries", rules)
	}
	for _, r := range rules {
		if !strings.HasPrefix(string(r.Entity), "project-") || r.ProjectTeam == nil || r.ProjectTeam.ProjectNumber == "" ||
			!strings.HasSuffix(string(r.Entity), "-"+r.ProjectTeam.ProjectNumber) {
			t.Errorf("default entry %+v is not project-<team>-<number>", r)
		}
	}
	if err := bh.ACL().Set(ctx, "user-dev@example.com", storage.RoleWriter); err != nil {
		t.Fatal(err)
	}
	attrs, err := bh.Attrs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !aclRule(attrs.ACL, "user-dev@example.com", storage.RoleWriter) {
		t.Errorf("Attrs ACL = %+v; want user-dev@example.com WRITER", attrs.ACL)
	}
	if err := bh.ACL().Delete(ctx, "user-dev@example.com"); err != nil {
		t.Fatal(err)
	}
	if rules, _ = bh.ACL().List(ctx); aclRule(rules, "user-dev@example.com", storage.RoleWriter) {
		t.Errorf("deleted entry still listed: %+v", rules)
	}
}

// TestStorageObjectACL: a new object takes the bucket's default object
// ACL; an object's own ACL is set and deleted through its ACLHandle, and
// WRITER is refused on it.
// covers: storage.defaultObjectAccessControls.update, storage.defaultObjectAccessControls.list, storage.defaultObjectAccessControls.delete, storage.objectAccessControls.update, storage.objectAccessControls.list, storage.objectAccessControls.delete
func TestStorageObjectACL(t *testing.T) {
	h := New(t)
	c := storageClient(t, h)
	ctx := h.Context()
	bh := c.Bucket(h.Project() + "-oacl")
	if err := bh.Create(ctx, h.Project(), nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bh.Object("o.txt").Delete(context.Background()); _ = bh.Delete(context.Background()) })
	if err := bh.DefaultObjectACL().Set(ctx, storage.AllUsers, storage.RoleReader); err != nil {
		t.Fatal(err)
	}
	if def, err := bh.DefaultObjectACL().List(ctx); err != nil || !aclRule(def, storage.AllUsers, storage.RoleReader) {
		t.Fatalf("default object ACL = %+v, %v", def, err)
	}
	if err := writeObject(h, c, bh.BucketName(), "o.txt", "x"); err != nil {
		t.Fatal(err)
	}
	o := bh.Object("o.txt")
	rules, err := o.ACL().List(ctx)
	if err != nil || !aclRule(rules, storage.AllUsers, storage.RoleReader) {
		t.Fatalf("new object ACL = %+v, %v; want the bucket's default", rules, err)
	}
	if err := o.ACL().Set(ctx, "group-team@example.com", storage.RoleOwner); err != nil {
		t.Fatal(err)
	}
	if code := aclCode(o.ACL().Set(ctx, "group-team@example.com", storage.RoleWriter)); code != http.StatusBadRequest {
		t.Errorf("an object WRITER = %d; want 400", code)
	}
	if err := o.ACL().Delete(ctx, storage.AllUsers); err != nil {
		t.Fatal(err)
	}
	rules, _ = o.ACL().List(ctx)
	if aclRule(rules, storage.AllUsers, storage.RoleReader) || !aclRule(rules, "group-team@example.com", storage.RoleOwner) {
		t.Errorf("object ACL = %+v", rules)
	}
	if err := bh.DefaultObjectACL().Delete(ctx, storage.AllUsers); err != nil {
		t.Fatal(err)
	}
}

// TestStoragePredefinedACL: predefinedAcl and predefinedDefaultObjectAcl
// on bucket create and update, and predefinedAcl on an object write.
// covers: storage.buckets.insert, storage.buckets.patch, storage.objects.insert
func TestStoragePredefinedACL(t *testing.T) {
	h := New(t)
	c := storageClient(t, h)
	ctx := h.Context()
	bh := c.Bucket(h.Project() + "-pacl")
	if err := bh.Create(ctx, h.Project(), &storage.BucketAttrs{PredefinedACL: "publicRead", PredefinedDefaultObjectACL: "authenticatedRead"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bh.Object("p.txt").Delete(context.Background()); _ = bh.Delete(context.Background()) })
	attrs, err := bh.Attrs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !aclRule(attrs.ACL, storage.AllUsers, storage.RoleReader) || !aclRule(attrs.DefaultObjectACL, storage.AllAuthenticatedUsers, storage.RoleReader) {
		t.Errorf("ACL %+v, default object ACL %+v; want publicRead and authenticatedRead", attrs.ACL, attrs.DefaultObjectACL)
	}
	if attrs, err = bh.Update(ctx, storage.BucketAttrsToUpdate{PredefinedACL: "private"}); err != nil {
		t.Fatal(err)
	} else if len(attrs.ACL) != 1 {
		t.Errorf("private bucket ACL = %+v; want the project owners alone", attrs.ACL)
	}
	w := bh.Object("p.txt").NewWriter(ctx)
	w.PredefinedACL = "publicRead"
	_, _ = w.Write([]byte("x"))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if !aclRule(w.Attrs().ACL, storage.AllUsers, storage.RoleReader) {
		t.Errorf("publicRead object ACL = %+v", w.Attrs().ACL)
	}
}

// TestStorageACLUniformAccess: under uniform bucket-level access every
// ACL call fails 400, as the uniform-bucket-level-access page says.
// covers: storage.bucketAccessControls.list, storage.defaultObjectAccessControls.list, storage.bucketAccessControls.insert
func TestStorageACLUniformAccess(t *testing.T) {
	h := New(t)
	c := storageClient(t, h)
	ctx := h.Context()
	bh := c.Bucket(h.Project() + "-uacl")
	if err := bh.Create(ctx, h.Project(), &storage.BucketAttrs{UniformBucketLevelAccess: storage.UniformBucketLevelAccess{Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bh.Delete(context.Background()) })
	if _, err := bh.ACL().List(ctx); aclCode(err) != http.StatusBadRequest {
		t.Errorf("bucket ACL list = %v; want 400", err)
	}
	if _, err := bh.DefaultObjectACL().List(ctx); aclCode(err) != http.StatusBadRequest {
		t.Errorf("default object ACL list = %v; want 400", err)
	}
	code, body := rawStorage(t, h, "POST", "/storage/v1/b/"+bh.BucketName()+"/acl", `{"entity":"allUsers","role":"READER"}`)
	if code != http.StatusBadRequest || !strings.Contains(body, "uniform bucket-level access") {
		t.Errorf("bucket ACL insert = %d %s; want 400 naming uniform bucket-level access", code, body)
	}
}

// TestGcloudStorageACL: gcloud storage's ACL flags round-trip.
// covers: storage.buckets.patch, storage.objects.patch
func TestGcloudStorageACL(t *testing.T) {
	h := New(t)
	g := newGcloudSession(t, h)
	c := storageClient(t, h)
	name := h.Project() + "-gacl"
	g.must("storage", "buckets", "create", "gs://"+name)
	t.Cleanup(func() { _, _ = g.run(nil, "storage", "rm", "-r", "gs://"+name) })
	g.must("storage", "buckets", "update", "gs://"+name, "--add-acl-grant=entity=user-dev@example.com,role=WRITER")
	attrs, err := c.Bucket(name).Attrs(h.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !aclRule(attrs.ACL, "user-dev@example.com", storage.RoleWriter) {
		t.Errorf("bucket ACL after --add-acl-grant = %+v", attrs.ACL)
	}
	if err := writeObject(h, c, name, "o.txt", "x"); err != nil {
		t.Fatal(err)
	}
	g.must("storage", "objects", "update", "gs://"+name+"/o.txt", "--predefined-acl=publicRead")
	rules, err := c.Bucket(name).Object("o.txt").ACL().List(h.Context())
	if err != nil || !aclRule(rules, storage.AllUsers, storage.RoleReader) {
		t.Errorf("object ACL after --predefined-acl=publicRead = %+v, %v", rules, err)
	}
	if out := g.must("storage", "objects", "describe", "gs://"+name+"/o.txt", "--format=json(acl)"); !strings.Contains(out, "allUsers") {
		t.Errorf("objects describe does not show the ACL:\n%s", out)
	}
}

// TestStorageACLMethods: get, insert and patch of each ACL resource on the
// JSON API, which the Go client's ACLHandle does not send.
// covers: storage.bucketAccessControls.get, storage.bucketAccessControls.insert, storage.bucketAccessControls.patch, storage.defaultObjectAccessControls.get, storage.defaultObjectAccessControls.insert, storage.defaultObjectAccessControls.patch, storage.objectAccessControls.get, storage.objectAccessControls.insert, storage.objectAccessControls.patch
func TestStorageACLMethods(t *testing.T) {
	h := New(t)
	c := storageClient(t, h)
	ctx := h.Context()
	bh := c.Bucket(h.Project() + "-racl")
	if err := bh.Create(ctx, h.Project(), nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bh.Object("o").Delete(context.Background()); _ = bh.Delete(context.Background()) })
	if err := writeObject(h, c, bh.BucketName(), "o", "x"); err != nil {
		t.Fatal(err)
	}
	base := "/storage/v1/b/" + bh.BucketName()
	for _, res := range []struct{ path, kind string }{
		{base + "/acl", "storage#bucketAccessControl"},
		{base + "/defaultObjectAcl", "storage#objectAccessControl"},
		{base + "/o/o/acl", "storage#objectAccessControl"},
	} {
		code, body := rawStorage(t, h, "POST", res.path, `{"entity":"domain-example.com","role":"READER"}`)
		if code != http.StatusOK || !strings.Contains(body, res.kind) || !strings.Contains(body, `"domain": "example.com"`) {
			t.Errorf("insert %s = %d %s", res.path, code, body)
		}
		if code, body = rawStorage(t, h, "PATCH", res.path+"/domain-example.com", `{"role":"OWNER"}`); code != http.StatusOK || !strings.Contains(body, `"OWNER"`) {
			t.Errorf("patch %s = %d %s", res.path, code, body)
		}
		if code, body = rawStorage(t, h, "GET", res.path+"/domain-example.com", ""); code != http.StatusOK || !strings.Contains(body, `"OWNER"`) {
			t.Errorf("get %s = %d %s", res.path, code, body)
		}
		if code, body = rawStorage(t, h, "GET", res.path+"/allUsers", ""); code != http.StatusNotFound {
			t.Errorf("get of a missing entry at %s = %d %s; want 404", res.path, code, body)
		}
	}
}
