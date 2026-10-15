package storage

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"cloud.google.com/go/iam/apiv1/iampb"
	gcs "cloud.google.com/go/storage"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	grpcstatus "google.golang.org/grpc/status"

	pb "github.com/cloudburrow/cloudburrow/internal/service/storage/storagev2pb"
)

func hasGRPCRule(rules []gcs.ACLRule, entity gcs.ACLEntity, role gcs.ACLRole) bool {
	for _, r := range rules {
		if r.Entity == entity && r.Role == role {
			return true
		}
	}
	return false
}

// TestGRPCACLFieldsAgreeWithJSON (#1345): Bucket.acl, default_object_acl
// and Object.acl over the official Go client's gRPC transport, which reads
// them with GetBucket or GetObject and writes them with an update_mask of
// acl or default_object_acl, are the JSON API's ACLs (#1233).
func TestGRPCACLFieldsAgreeWithJSON(t *testing.T) {
	g, j, _ := grpcClients(t)
	ctx := context.Background()
	bh := g.Bucket("acl-bucket")
	if err := bh.Create(ctx, "p", &gcs.BucketAttrs{ACL: []gcs.ACLRule{{Entity: "user-a@example.com", Role: gcs.RoleOwner}}}); err != nil {
		t.Fatalf("Create with an ACL: %v", err)
	}
	if rules, err := j.Bucket("acl-bucket").ACL().List(ctx); err != nil || !hasGRPCRule(rules, "user-a@example.com", gcs.RoleOwner) {
		t.Fatalf("JSON bucket ACL after a gRPC create = %+v, %v", rules, err)
	}
	if err := bh.ACL().Set(ctx, "user-b@example.com", gcs.RoleReader); err != nil {
		t.Fatalf("gRPC bucket ACL Set: %v", err)
	}
	if rules, err := j.Bucket("acl-bucket").ACL().List(ctx); err != nil || !hasGRPCRule(rules, "user-b@example.com", gcs.RoleReader) {
		t.Fatalf("JSON bucket ACL = %+v, %v", rules, err)
	}
	if err := j.Bucket("acl-bucket").ACL().Set(ctx, "user-c@example.com", gcs.RoleWriter); err != nil {
		t.Fatal(err)
	}
	if rules, err := bh.ACL().List(ctx); err != nil || !hasGRPCRule(rules, "user-c@example.com", gcs.RoleWriter) {
		t.Fatalf("gRPC bucket ACL = %+v, %v", rules, err)
	}
	if err := bh.ACL().Delete(ctx, "user-b@example.com"); err != nil {
		t.Fatalf("gRPC bucket ACL Delete: %v", err)
	}
	if rules, _ := j.Bucket("acl-bucket").ACL().List(ctx); hasGRPCRule(rules, "user-b@example.com", gcs.RoleReader) {
		t.Errorf("JSON bucket ACL still has the deleted entry: %+v", rules)
	}

	if err := bh.DefaultObjectACL().Set(ctx, gcs.AllUsers, gcs.RoleReader); err != nil {
		t.Fatalf("gRPC default object ACL Set: %v", err)
	}
	if rules, err := j.Bucket("acl-bucket").DefaultObjectACL().List(ctx); err != nil || !hasGRPCRule(rules, gcs.AllUsers, gcs.RoleReader) {
		t.Fatalf("JSON default object ACL = %+v, %v", rules, err)
	}

	w := bh.Object("o").NewWriter(ctx)
	w.ACL = []gcs.ACLRule{{Entity: gcs.AllAuthenticatedUsers, Role: gcs.RoleReader}, {Entity: "user-a@example.com", Role: gcs.RoleOwner}}
	if _, err := io.WriteString(w, "x"); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("write with an ACL: %v", err)
	}
	if rules, err := j.Bucket("acl-bucket").Object("o").ACL().List(ctx); err != nil || !hasGRPCRule(rules, gcs.AllAuthenticatedUsers, gcs.RoleReader) || hasGRPCRule(rules, gcs.AllUsers, gcs.RoleReader) {
		t.Fatalf("JSON object ACL after a gRPC write = %+v, %v", rules, err)
	}
	if err := bh.Object("o").ACL().Set(ctx, "user-d@example.com", gcs.RoleReader); err != nil {
		t.Fatalf("gRPC object ACL Set: %v", err)
	}
	if rules, err := j.Bucket("acl-bucket").Object("o").ACL().List(ctx); err != nil || !hasGRPCRule(rules, "user-d@example.com", gcs.RoleReader) {
		t.Fatalf("JSON object ACL = %+v, %v", rules, err)
	}
	if rules, err := bh.Object("o").ACL().List(ctx); err != nil || !hasGRPCRule(rules, "user-d@example.com", gcs.RoleReader) {
		t.Fatalf("gRPC object ACL = %+v, %v", rules, err)
	}
	if err := bh.Object("o").ACL().Set(ctx, "user-d@example.com", gcs.RoleWriter); grpcstatus.Code(err) != codes.InvalidArgument {
		t.Errorf("an object WRITER entry = %v, want INVALID_ARGUMENT as the JSON API refuses it", err)
	}

	ub := g.Bucket("acl-uniform")
	if err := ub.Create(ctx, "p", &gcs.BucketAttrs{UniformBucketLevelAccess: gcs.UniformBucketLevelAccess{Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	if err := ub.ACL().Set(ctx, "user-b@example.com", gcs.RoleReader); err == nil {
		t.Errorf("an ACL on a uniform-access bucket was accepted")
	}
}

// TestGRPCManagedFolderIAM (#1345): GetIamPolicy, SetIamPolicy and
// TestIamPermissions on a managed folder's resource name are the JSON API's
// managedFolders IAM methods. The Go client has no managed-folder IAM, so
// this calls the generated stub.
func TestGRPCManagedFolderIAM(t *testing.T) {
	g, _, addr := grpcClients(t)
	ctx := context.Background()
	if err := g.Bucket("mf-bucket").Create(ctx, "p", &gcs.BucketAttrs{UniformBucketLevelAccess: gcs.UniformBucketLevelAccess{Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post("http://"+addr+"/storage/v1/b/mf-bucket/managedFolders", "application/json", strings.NewReader(`{"name":"team/sub/"}`))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("insert the managed folder: %v %v", resp, err)
	}
	resp.Body.Close()
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	c := pb.NewStorageClient(conn)
	res := "projects/_/buckets/mf-bucket/managedFolders/team/sub/"
	p, err := c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: res})
	if err != nil {
		t.Fatalf("GetIamPolicy: %v", err)
	}
	p.Bindings = append(p.Bindings, &iampb.Binding{Role: "roles/storage.objectViewer", Members: []string{"user:dev@example.com"}})
	if _, err := c.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: res, Policy: p}); err != nil {
		t.Fatalf("SetIamPolicy: %v", err)
	}
	if _, err := c.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: res, Policy: p}); grpcstatus.Code(err) != codes.FailedPrecondition {
		t.Errorf("SetIamPolicy with a stale etag = %v, want FAILED_PRECONDITION", err)
	}
	resp, err = http.Get("http://" + addr + "/storage/v1/b/mf-bucket/managedFolders/team%2Fsub%2F/iam")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !bytes.Contains(body, []byte("user:dev@example.com")) {
		t.Errorf("the JSON API's managed folder policy = %d %s", resp.StatusCode, body)
	}
	if bp, err := g.Bucket("mf-bucket").IAM().Policy(ctx); err != nil || bp.HasRole("user:dev@example.com", "roles/storage.objectViewer") {
		t.Errorf("the bucket's policy took the managed folder's binding: %v", err)
	}
	perms, err := c.TestIamPermissions(ctx, &iampb.TestIamPermissionsRequest{Resource: res, Permissions: []string{"storage.objects.get"}})
	if err != nil || len(perms.GetPermissions()) != 1 {
		t.Errorf("TestIamPermissions = %v, %v", perms, err)
	}
	if _, err := c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: "projects/_/buckets/mf-bucket/managedFolders/absent/"}); grpcstatus.Code(err) != codes.NotFound {
		t.Errorf("GetIamPolicy of an absent managed folder = %v, want NOT_FOUND", err)
	}
}
