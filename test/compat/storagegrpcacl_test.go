//go:build compat

package compat

import (
	"io"
	"testing"

	"cloud.google.com/go/storage"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func hasACLRule(rules []storage.ACLRule, entity storage.ACLEntity, role storage.ACLRole) bool {
	for _, r := range rules {
		if r.Entity == entity && r.Role == role {
			return true
		}
	}
	return false
}

// TestStorageGRPCACLs (#1345): the acl fields over gRPC with the official
// Go client: Bucket.acl on CreateBucket and through UpdateBucket's acl mask
// (BucketHandle.ACL, which the gRPC client reads with GetBucket),
// default_object_acl, and Object.acl on a write and through UpdateObject's
// acl mask, each read back through the JSON API's ACLs (#1233), and a JSON
// change read back over gRPC. Measured first: every one was refused
// UNIMPLEMENTED.
//
// covers: google.storage.v2.Storage/CreateBucket, google.storage.v2.Storage/GetBucket, google.storage.v2.Storage/UpdateBucket
// covers: google.storage.v2.Storage/BidiWriteObject, google.storage.v2.Storage/GetObject, google.storage.v2.Storage/UpdateObject
func TestStorageGRPCACLs(t *testing.T) {
	h := New(t)
	g := storageGRPCClient(t, h, false)
	j := storageClient(t, h)
	ctx := h.Context()

	name := h.Project() + "-grpc-acl"
	bh := g.Bucket(name)
	if err := bh.Create(ctx, h.Project(), &storage.BucketAttrs{ACL: []storage.ACLRule{{Entity: "user-a@example.com", Role: storage.RoleOwner}}}); err != nil {
		t.Fatalf("Create with an ACL: %v", err)
	}
	t.Cleanup(func() { emptyAndDelete(ctx, j.Bucket(name)) })
	jb := j.Bucket(name)
	if rules, err := jb.ACL().List(ctx); err != nil || !hasACLRule(rules, "user-a@example.com", storage.RoleOwner) {
		t.Fatalf("JSON bucket ACL after a gRPC create = %+v, %v", rules, err)
	}
	if err := bh.ACL().Set(ctx, "user-b@example.com", storage.RoleReader); err != nil {
		t.Fatalf("gRPC bucket ACL Set: %v", err)
	}
	if rules, err := jb.ACL().List(ctx); err != nil || !hasACLRule(rules, "user-b@example.com", storage.RoleReader) {
		t.Fatalf("JSON bucket ACL = %+v, %v", rules, err)
	}
	if err := jb.ACL().Set(ctx, "user-c@example.com", storage.RoleWriter); err != nil {
		t.Fatal(err)
	}
	if rules, err := bh.ACL().List(ctx); err != nil || !hasACLRule(rules, "user-c@example.com", storage.RoleWriter) {
		t.Fatalf("gRPC bucket ACL after a JSON set = %+v, %v", rules, err)
	}
	if err := bh.ACL().Delete(ctx, "user-b@example.com"); err != nil {
		t.Fatalf("gRPC bucket ACL Delete: %v", err)
	}
	if rules, _ := jb.ACL().List(ctx); hasACLRule(rules, "user-b@example.com", storage.RoleReader) {
		t.Errorf("JSON bucket ACL still has the deleted entry: %+v", rules)
	}

	if err := bh.DefaultObjectACL().Set(ctx, storage.AllUsers, storage.RoleReader); err != nil {
		t.Fatalf("gRPC default object ACL Set: %v", err)
	}
	if rules, err := jb.DefaultObjectACL().List(ctx); err != nil || !hasACLRule(rules, storage.AllUsers, storage.RoleReader) {
		t.Fatalf("JSON default object ACL = %+v, %v", rules, err)
	}

	w := bh.Object("o").NewWriter(ctx)
	w.ACL = []storage.ACLRule{{Entity: storage.AllAuthenticatedUsers, Role: storage.RoleReader}, {Entity: "user-a@example.com", Role: storage.RoleOwner}}
	if _, err := io.WriteString(w, "x"); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("write with an ACL: %v", err)
	}
	if rules, err := jb.Object("o").ACL().List(ctx); err != nil || !hasACLRule(rules, storage.AllAuthenticatedUsers, storage.RoleReader) || hasACLRule(rules, storage.AllUsers, storage.RoleReader) {
		t.Fatalf("JSON object ACL after a gRPC write = %+v, %v; want the written ACL, not the default", rules, err)
	}
	if err := bh.Object("o").ACL().Set(ctx, "user-d@example.com", storage.RoleReader); err != nil {
		t.Fatalf("gRPC object ACL Set: %v", err)
	}
	if rules, err := jb.Object("o").ACL().List(ctx); err != nil || !hasACLRule(rules, "user-d@example.com", storage.RoleReader) {
		t.Fatalf("JSON object ACL = %+v, %v", rules, err)
	}
	if rules, err := bh.Object("o").ACL().List(ctx); err != nil || !hasACLRule(rules, "user-d@example.com", storage.RoleReader) {
		t.Fatalf("gRPC object ACL = %+v, %v", rules, err)
	}
	if err := bh.Object("o").ACL().Set(ctx, "user-d@example.com", storage.RoleWriter); status.Code(err) != codes.InvalidArgument {
		t.Errorf("an object WRITER entry = %v, want INVALID_ARGUMENT as the JSON API refuses it", err)
	}
}
