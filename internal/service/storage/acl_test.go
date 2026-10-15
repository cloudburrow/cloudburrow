package storage

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"

	gcs "cloud.google.com/go/storage"

	"github.com/cloudburrow/cloudburrow/internal/resource"
)

func hasRule(rules []gcs.ACLRule, entity gcs.ACLEntity, role gcs.ACLRole) bool {
	for _, r := range rules {
		if r.Entity == entity && r.Role == role {
			return true
		}
	}
	return false
}

func projectEntity(team, project string) gcs.ACLEntity {
	return gcs.ACLEntity("project-" + team + "-" + strconv.FormatInt(resource.ProjectNumber(project), 10))
}

// A bucket's ACL through the Go client's ACLHandle (#1233): the
// projectPrivate default, by project number, then set, listed and deleted.
// Nothing is enforced.
func TestStorageBucketACLInProcess(t *testing.T) {
	ctx := context.Background()
	_, bh, _ := sdkBucket(t, "acl-b")
	rules, err := bh.ACL().List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	owners := projectEntity("owners", "demo-project")
	if len(rules) != 3 || !hasRule(rules, owners, gcs.RoleOwner) ||
		!hasRule(rules, projectEntity("editors", "demo-project"), gcs.RoleOwner) ||
		!hasRule(rules, projectEntity("viewers", "demo-project"), gcs.RoleReader) {
		t.Errorf("default bucket ACL = %+v; want projectPrivate", rules)
	}
	for _, r := range rules {
		if r.Entity == owners && (r.ProjectTeam == nil || r.ProjectTeam.Team != "owners") {
			t.Errorf("project entity has no projectTeam: %+v", r)
		}
	}
	if err := bh.ACL().Set(ctx, "user-a@example.com", gcs.RoleWriter); err != nil {
		t.Fatal(err)
	}
	if err := bh.ACL().Set(ctx, gcs.AllUsers, gcs.RoleReader); err != nil {
		t.Fatal(err)
	}
	rules, _ = bh.ACL().List(ctx)
	if !hasRule(rules, "user-a@example.com", gcs.RoleWriter) || !hasRule(rules, gcs.AllUsers, gcs.RoleReader) {
		t.Errorf("after Set: %+v", rules)
	}
	for _, r := range rules {
		if r.Entity == "user-a@example.com" && r.Email != "a@example.com" {
			t.Errorf("user entity email = %q", r.Email)
		}
	}
	attrs, err := bh.Attrs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !hasRule(attrs.ACL, gcs.AllUsers, gcs.RoleReader) || attrs.MetaGeneration != 3 {
		t.Errorf("Attrs ACL = %+v, metageneration %d; want allUsers and metageneration 3", attrs.ACL, attrs.MetaGeneration)
	}
	if err := bh.ACL().Delete(ctx, "user-a@example.com"); err != nil {
		t.Fatal(err)
	}
	if code, _ := apiCode(bh.ACL().Delete(ctx, "user-a@example.com")); code != http.StatusNotFound {
		t.Errorf("deleting a missing entry = %d; want 404", code)
	}
	rules, _ = bh.ACL().List(ctx)
	if hasRule(rules, "user-a@example.com", gcs.RoleWriter) {
		t.Errorf("deleted entry still listed: %+v", rules)
	}
	// A project entity given by ID is stored by number.
	if err := bh.ACL().Set(ctx, "project-viewers-other-project", gcs.RoleReader); err != nil {
		t.Fatal(err)
	}
	rules, _ = bh.ACL().List(ctx)
	if !hasRule(rules, projectEntity("viewers", "other-project"), gcs.RoleReader) {
		t.Errorf("project-viewers-other-project was not stored by number: %+v", rules)
	}
	for _, bad := range []struct {
		entity gcs.ACLEntity
		role   gcs.ACLRole
	}{{"nobody", gcs.RoleReader}, {"user-a@example.com", "ADMIN"}} {
		if code, _ := apiCode(bh.ACL().Set(ctx, bad.entity, bad.role)); code != http.StatusBadRequest {
			t.Errorf("Set(%s, %s) = %d; want 400", bad.entity, bad.role, code)
		}
	}
}

// A bucket's default object ACL is what a new object takes; an object's ACL
// changes on its own, and has no WRITER.
func TestStorageObjectACLInProcess(t *testing.T) {
	ctx := context.Background()
	_, bh, _ := sdkBucket(t, "acl-o")
	if err := bh.DefaultObjectACL().Set(ctx, gcs.AllAuthenticatedUsers, gcs.RoleReader); err != nil {
		t.Fatal(err)
	}
	if code, _ := apiCode(bh.DefaultObjectACL().Set(ctx, gcs.AllUsers, gcs.RoleWriter)); code != http.StatusBadRequest {
		t.Errorf("a default object WRITER = %d; want 400", code)
	}
	o := bh.Object("doc.txt")
	attrs := write(t, o, []byte("hi"), nil)
	if !hasRule(attrs.ACL, gcs.AllAuthenticatedUsers, gcs.RoleReader) || !hasRule(attrs.ACL, projectEntity("owners", "demo-project"), gcs.RoleOwner) {
		t.Errorf("new object ACL = %+v; want the bucket's default object ACL", attrs.ACL)
	}
	if err := o.ACL().Set(ctx, "group-team@example.com", gcs.RoleReader); err != nil {
		t.Fatal(err)
	}
	if code, _ := apiCode(o.ACL().Set(ctx, "group-team@example.com", gcs.RoleWriter)); code != http.StatusBadRequest {
		t.Errorf("an object WRITER = %d; want 400", code)
	}
	if err := o.ACL().Delete(ctx, gcs.AllAuthenticatedUsers); err != nil {
		t.Fatal(err)
	}
	rules, err := o.ACL().List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !hasRule(rules, "group-team@example.com", gcs.RoleReader) || hasRule(rules, gcs.AllAuthenticatedUsers, gcs.RoleReader) {
		t.Errorf("object ACL = %+v", rules)
	}
	// The bucket's default is untouched, and a copy takes it, not its
	// source's ACL.
	cp, err := bh.Object("copy.txt").CopierFrom(o).Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if hasRule(cp.ACL, "group-team@example.com", gcs.RoleReader) || !hasRule(cp.ACL, gcs.AllAuthenticatedUsers, gcs.RoleReader) {
		t.Errorf("copy ACL = %+v; want the bucket's default object ACL", cp.ACL)
	}
	// ACLs in metadata: an object written with one keeps it.
	attrs = write(t, bh.Object("own.txt"), []byte("x"), func(w *gcs.Writer) {
		w.ACL = []gcs.ACLRule{{Entity: "domain-example.com", Role: gcs.RoleReader}}
	})
	if len(attrs.ACL) != 1 || attrs.ACL[0].Entity != "domain-example.com" || attrs.ACL[0].Domain != "example.com" {
		t.Errorf("written ACL = %+v", attrs.ACL)
	}
	// Patching other metadata leaves the ACL.
	up, err := bh.Object("own.txt").Update(ctx, gcs.ObjectAttrsToUpdate{ContentType: "text/plain"})
	if err != nil {
		t.Fatal(err)
	}
	if len(up.ACL) != 1 {
		t.Errorf("ACL after a metadata update = %+v", up.ACL)
	}
}

// predefinedAcl and predefinedDefaultObjectAcl, on create and on update.
func TestStoragePredefinedACLInProcess(t *testing.T) {
	ctx := context.Background()
	c, _ := sdk(t)
	bh := c.Bucket("acl-p")
	if err := bh.Create(ctx, "demo-project", &gcs.BucketAttrs{PredefinedACL: "publicRead", PredefinedDefaultObjectACL: "authenticatedRead"}); err != nil {
		t.Fatal(err)
	}
	attrs, err := bh.Attrs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(attrs.ACL) != 2 || !hasRule(attrs.ACL, gcs.AllUsers, gcs.RoleReader) {
		t.Errorf("publicRead bucket ACL = %+v", attrs.ACL)
	}
	if len(attrs.DefaultObjectACL) != 2 || !hasRule(attrs.DefaultObjectACL, gcs.AllAuthenticatedUsers, gcs.RoleReader) {
		t.Errorf("authenticatedRead default object ACL = %+v", attrs.DefaultObjectACL)
	}
	attrs, err = bh.Update(ctx, gcs.BucketAttrsToUpdate{PredefinedACL: "private"})
	if err != nil {
		t.Fatal(err)
	}
	if len(attrs.ACL) != 1 || !hasRule(attrs.ACL, projectEntity("owners", "demo-project"), gcs.RoleOwner) {
		t.Errorf("private bucket ACL = %+v", attrs.ACL)
	}
	oa := write(t, bh.Object("p.txt"), []byte("x"), func(w *gcs.Writer) { w.PredefinedACL = "publicRead" })
	if len(oa.ACL) != 2 || !hasRule(oa.ACL, gcs.AllUsers, gcs.RoleReader) {
		t.Errorf("publicRead object ACL = %+v", oa.ACL)
	}
	oa, err = bh.Object("p.txt").Update(ctx, gcs.ObjectAttrsToUpdate{PredefinedACL: "private"})
	if err != nil {
		t.Fatal(err)
	}
	if len(oa.ACL) != 1 {
		t.Errorf("private object ACL = %+v", oa.ACL)
	}
	if _, err := bh.Object("p.txt").Update(ctx, gcs.ObjectAttrsToUpdate{PredefinedACL: "bogus"}); err == nil {
		t.Error("predefinedAcl=bogus was accepted")
	}
}

// Under uniform bucket-level access every ACL method fails 400, and a full
// projection carries empty ACLs (uniform-bucket-level-access docs).
func TestStorageACLUniformAccessInProcess(t *testing.T) {
	ctx := context.Background()
	c, h := sdk(t)
	bh := c.Bucket("acl-u")
	if err := bh.Create(ctx, "demo-project", &gcs.BucketAttrs{UniformBucketLevelAccess: gcs.UniformBucketLevelAccess{Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	write(t, bh.Object("x"), []byte("x"), nil)
	if _, err := bh.ACL().List(ctx); err == nil {
		t.Error("bucket ACL list under uniform access succeeded")
	} else if code, _ := apiCode(err); code != http.StatusBadRequest {
		t.Errorf("bucket ACL list = %d; want 400", code)
	}
	if code, _ := apiCode(bh.DefaultObjectACL().Set(ctx, gcs.AllUsers, gcs.RoleReader)); code != http.StatusBadRequest {
		t.Errorf("default object ACL set = %d; want 400", code)
	}
	if code, _ := apiCode(bh.Object("x").ACL().Set(ctx, gcs.AllUsers, gcs.RoleReader)); code != http.StatusBadRequest {
		t.Errorf("object ACL set = %d; want 400", code)
	}
	code, body := raw(t, "GET", h.URL+"/storage/v1/b/acl-u?projection=full&prettyPrint=false", "")
	if code != 200 || !strings.Contains(body, `"acl":[]`) || !strings.Contains(body, `"defaultObjectAcl":[]`) {
		t.Errorf("full projection under uniform access = %d %s; want empty ACLs", code, body)
	}
	if code, body := raw(t, "POST", h.URL+"/storage/v1/b/acl-u/o?uploadType=media&name=y&predefinedAcl=publicRead", "x"); code != 400 {
		t.Errorf("predefinedAcl under uniform access = %d %s; want 400", code, body)
	}
}

// The ACL is returned only under projection=full, and projection is checked.
func TestStorageACLProjectionInProcess(t *testing.T) {
	_, _, h := sdkBucket(t, "acl-j")
	if _, body := raw(t, "GET", h.URL+"/storage/v1/b/acl-j", ""); strings.Contains(body, `"acl"`) {
		t.Errorf("noAcl projection carries acl: %s", body)
	}
	if code, _ := raw(t, "PATCH", h.URL+"/storage/v1/b/acl-j?projection=everything", "{}"); code != 400 {
		t.Errorf("patch with a bad projection = %d; want 400", code)
	}
	code, body := raw(t, "GET", h.URL+"/storage/v1/b/acl-j/acl/allUsers", "")
	if code != 404 {
		t.Errorf("get of a missing entry = %d %s; want 404", code, body)
	}
}
