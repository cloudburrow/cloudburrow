package main

import (
	"context"
	"testing"

	"cloud.google.com/go/storage"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

func aclSection(d console.Detail) (console.Section, bool) {
	for _, s := range d.Sections {
		if s.ID == "acl" {
			return s, true
		}
	}
	return console.Section{}, false
}

func aclRowFor(s console.Section, entity, scope string) (console.Resource, bool) {
	for _, r := range s.Listing.Items {
		if r.Name == entity && r.Fields["ACL"] == aclScopeLabels[scope] {
			return r, true
		}
	}
	return console.Resource{}, false
}

// A bucket's and an object's Access control tab (#1233) list their ACLs
// through the official client's ACL handles; Add ACL entry and Remove
// change them, as the client then reads; Edit bucket replaces the default
// object ACL with a predefined one; under uniform bucket-level access there
// is no tab and no Add ACL entry.
func TestStorageConsoleACLs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p, c := newObjectOpsProvider(t)

	d, err := p.Detail(ctx, "p", []string{"ops"})
	if err != nil {
		t.Fatal(err)
	}
	sec, ok := aclSection(d)
	if !ok || sec.Unavailable != "" || len(sec.Listing.Items) != 6 {
		t.Fatalf("bucket Access control tab = %+v, %v; want projectPrivate's three entries twice", sec, ok)
	}
	if !hasAction(d.Actions, "addacl") || !hasAction(p.DetailActions(ctx, "p", []string{"ops"}), "addacl") {
		t.Error("the bucket page offers no Add ACL entry")
	}
	if err := p.ActAt(ctx, "p", []string{"ops"}, "addacl",
		map[string]string{"scope": aclScopeBucket, "entity": "user-dev@example.com", "role": "WRITER"}); err != nil {
		t.Fatal(err)
	}
	if err := p.ActAt(ctx, "p", []string{"ops"}, "addacl",
		map[string]string{"scope": aclScopeDefault, "entity": "allUsers", "role": "READER"}); err != nil {
		t.Fatal(err)
	}
	rules, _ := c.Bucket("ops").ACL().List(ctx)
	if !hasACLRule(rules, "user-dev@example.com", storage.RoleWriter) {
		t.Errorf("bucket ACL after Add = %+v", rules)
	}
	d, _ = p.Detail(ctx, "p", []string{"ops"})
	sec, _ = aclSection(d)
	row, ok := aclRowFor(sec, "user-dev@example.com", aclScopeBucket)
	if !ok || row.Fields["Role"] != "WRITER" || !hasAction(row.Actions, "removeacl") {
		t.Fatalf("no row for the added entry: %+v", sec.Listing.Items)
	}
	if !hasAction(p.DetailActions(ctx, "p", row.Target), "removeacl") {
		t.Errorf("DetailActions(%v) offers no Remove", row.Target)
	}
	if err := p.ActAt(ctx, "p", row.Target, "removeacl", nil); err != nil {
		t.Fatal(err)
	}
	if rules, _ = c.Bucket("ops").ACL().List(ctx); hasACLRule(rules, "user-dev@example.com", storage.RoleWriter) {
		t.Errorf("removed entry still there: %+v", rules)
	}

	// An object takes the default object ACL, and its tab changes its own.
	putObject(t, c, "ops", "a.txt", "x", nil)
	op := objectPath("ops", "a.txt")
	d, err = p.Detail(ctx, "p", op)
	if err != nil {
		t.Fatal(err)
	}
	sec, ok = aclSection(d)
	if _, has := aclRowFor(sec, "allUsers", aclScopeObject); !ok || !has {
		t.Fatalf("object Access control tab = %+v; want the default object ACL's allUsers", sec)
	}
	if !hasAction(p.DetailActions(ctx, "p", op), "addacl") {
		t.Error("the object page offers no Add ACL entry")
	}
	if err := p.ActAt(ctx, "p", op, "addacl",
		map[string]string{"scope": aclScopeObject, "entity": "group-team@example.com", "role": "OWNER"}); err != nil {
		t.Fatal(err)
	}
	if err := p.ActAt(ctx, "p", op, "addacl",
		map[string]string{"scope": aclScopeObject, "entity": "group-team@example.com", "role": "WRITER"}); err == nil {
		t.Error("an object WRITER was accepted")
	}
	d, _ = p.Detail(ctx, "p", op)
	sec, _ = aclSection(d)
	orow, ok := aclRowFor(sec, "allUsers", aclScopeObject)
	if !ok {
		t.Fatal("no allUsers row on the object")
	}
	if err := p.ActAt(ctx, "p", orow.Target, "removeacl", nil); err != nil {
		t.Fatal(err)
	}
	orules, _ := c.Bucket("ops").Object("a.txt").ACL().List(ctx)
	if hasACLRule(orules, "allUsers", storage.RoleReader) || !hasACLRule(orules, "group-team@example.com", storage.RoleOwner) {
		t.Errorf("object ACL = %+v", orules)
	}

	// Edit bucket: a predefined default object ACL.
	d, _ = p.Detail(ctx, "p", []string{"ops"})
	values := bucketEditValues(d.Edit)
	if values["predefinedDefaultObjectAcl"] != aclKeep {
		t.Fatalf("Edit bucket default object ACL = %q; want %q", values["predefinedDefaultObjectAcl"], aclKeep)
	}
	if err := p.Edit(ctx, "p", []string{"ops"}, values); err != nil {
		t.Fatal(err)
	}
	if def, _ := c.Bucket("ops").DefaultObjectACL().List(ctx); !hasACLRule(def, "allUsers", storage.RoleReader) {
		t.Errorf("keep current changed the default object ACL: %+v", def)
	}
	values["predefinedDefaultObjectAcl"] = "private"
	if err := p.Edit(ctx, "p", []string{"ops"}, values); err != nil {
		t.Fatal(err)
	}
	if def, _ := c.Bucket("ops").DefaultObjectACL().List(ctx); len(def) != 1 || hasACLRule(def, "allUsers", storage.RoleReader) {
		t.Errorf("default object ACL after private = %+v", def)
	}

	// Uniform bucket-level access: no tab, no Add, no field.
	if _, err := c.Bucket("other").Update(ctx, storage.BucketAttrsToUpdate{UniformBucketLevelAccess: &storage.UniformBucketLevelAccess{Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	d, _ = p.Detail(ctx, "p", []string{"other"})
	if _, ok := aclSection(d); ok || hasAction(d.Actions, "addacl") {
		t.Error("a bucket with uniform access shows its ACLs")
	}
	if _, ok := bucketEditValues(d.Edit)["predefinedDefaultObjectAcl"]; ok {
		t.Error("Edit bucket offers a default object ACL under uniform access")
	}
	putObject(t, c, "other", "b.txt", "x", nil)
	if d, _ = p.Detail(ctx, "p", objectPath("other", "b.txt")); func() bool { _, ok := aclSection(d); return ok }() {
		t.Error("an object in a bucket with uniform access shows its ACL")
	}
}

func hasACLRule(rules []storage.ACLRule, entity storage.ACLEntity, role storage.ACLRole) bool {
	for _, r := range rules {
		if r.Entity == entity && r.Role == role {
			return true
		}
	}
	return false
}
