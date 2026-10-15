package main

// Cloud Storage ACLs in the console (#1233): an Access control tab on a
// bucket's page, listing the bucket's ACL and its default object ACL, and on
// an object's page, listing the object's ACL; Add ACL entry on both pages and
// Remove on each row. Edit bucket offers a predefined default object ACL.
// The tab is shown only while uniform bucket-level access is off: with it on,
// every ACL call is refused, as Google refuses it.
//
// Every call goes through the official client's ACL handles,
// BucketHandle.ACL, BucketHandle.DefaultObjectACL and ObjectHandle.ACL, the
// calls an application makes. ACLs are stored and never enforced
// (ADR-0006 as amended 2026-10-15); the tab says so.
//
// A row's actions are at [aclPage, scope, bucket, entity] for a bucket's
// entry or a default object entry, and [aclPage, "object", bucket, object,
// entity] for an object's.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"cloud.google.com/go/storage"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

const aclPage = "_acl"

// The scopes an entry belongs to, as the form and the paths name them.
const (
	aclScopeBucket  = "bucket"
	aclScopeDefault = "default"
	aclScopeObject  = "object"
)

var aclScopeLabels = map[string]string{
	aclScopeBucket:  "Bucket",
	aclScopeDefault: "Default for new objects",
	aclScopeObject:  "Object",
}

// predefinedObjectACLs are the predefinedDefaultObjectAcl values Edit
// bucket offers, as the JSON API spells them.
var predefinedObjectACLs = []string{"authenticatedRead", "bucketOwnerFullControl", "bucketOwnerRead", "private", "projectPrivate", "publicRead"}

const aclKeep = "(keep current)"

const aclNote = "Stored and returned, never enforced: CloudBurrow authenticates no caller, so no ACL " +
	"allows or refuses anything here (ADR-0006, amended 2026-10-15)."

const entityHelp = "user-EMAIL, group-EMAIL, domain-DOMAIN, project-owners|editors|viewers-PROJECT " +
	"(a project ID is stored as its number), allUsers or allAuthenticatedUsers."

// aclAddAction is Add ACL entry. scopes are the ACLs the page can add to;
// an object's has no WRITER.
func aclAddAction(scopes []string) console.Action {
	roles := []string{"READER", "OWNER"}
	if len(scopes) > 0 && scopes[0] == aclScopeBucket {
		roles = append(roles, "WRITER")
	}
	fields := []console.Field{}
	if len(scopes) > 1 {
		opts := append([]string(nil), scopes...)
		fields = append(fields, console.Field{Name: "scope", Label: "ACL", Type: "select", Required: true,
			Options: opts, Default: scopes[0],
			Help: "bucket: the bucket's own ACL. default: the default object ACL, which new objects take."})
	} else {
		fields = append(fields, console.Field{Name: "scope", Type: "hidden", Default: scopes[0]})
	}
	return console.Action{ID: "addacl", Label: "Add ACL entry", Fields: append(fields,
		console.Field{Name: "entity", Label: "Entity", Type: "text", Required: true, Help: entityHelp},
		console.Field{Name: "role", Label: "Role", Type: "select", Required: true, Options: roles, Default: "READER",
			Help: "WRITER is a bucket ACL's only: an object's ACL has OWNER and READER. An entity already listed gets this role."},
	)}
}

func aclRemoveAction() console.Action {
	return console.Action{ID: "removeacl", Label: "Remove", Destructive: true}
}

// aclRow is one entry's row.
func aclRow(scope string, target []string, r storage.ACLRule) console.Resource {
	return console.Resource{
		Name:    string(r.Entity),
		Fields:  map[string]string{"Role": string(r.Role), "ACL": aclScopeLabels[scope]},
		Target:  target,
		Actions: []console.Action{aclRemoveAction()},
	}
}

// bucketACLSection is a bucket's Access control tab, or false under uniform
// bucket-level access.
func (p storageProvider) bucketACLSection(ctx context.Context, b bucketMeta) (console.Section, bool) {
	if b.IAMConfiguration.UniformBucketLevelAccess.Enabled {
		return console.Section{}, false
	}
	sec := console.Section{ID: "acl", Label: "Access control"}
	c, err := p.storageClient(ctx)
	if err != nil {
		sec.Unavailable = err.Error()
		return sec, true
	}
	defer func() { _ = c.Close() }()
	bh := c.Bucket(b.Name)
	list := console.Listing{Columns: []string{"Role", "ACL"}, NameColumn: "Entity", Noun: "ACL entries",
		Items: []console.Resource{}, Note: aclNote}
	for _, part := range []struct {
		scope string
		acl   *storage.ACLHandle
		what  string
	}{{aclScopeBucket, bh.ACL(), "bucketAccessControls.list"}, {aclScopeDefault, bh.DefaultObjectACL(), "defaultObjectAccessControls.list"}} {
		rules, err := part.acl.List(ctx)
		if err != nil {
			sec.Unavailable = part.what + ": " + err.Error()
			return sec, true
		}
		for _, r := range rules {
			list.Items = append(list.Items, aclRow(part.scope, []string{aclPage, part.scope, b.Name, string(r.Entity)}, r))
		}
	}
	list.Total = len(list.Items)
	sec.Listing = list
	return sec, true
}

// objectACLSection is an object's Access control tab, or false under
// uniform bucket-level access.
func (p storageProvider) objectACLSection(ctx context.Context, bucket, name string) (console.Section, bool) {
	b, err := p.readBucket(ctx, bucket)
	if err != nil || b.IAMConfiguration.UniformBucketLevelAccess.Enabled {
		return console.Section{}, false
	}
	sec := console.Section{ID: "acl", Label: "Access control"}
	c, err := p.storageClient(ctx)
	if err != nil {
		sec.Unavailable = err.Error()
		return sec, true
	}
	defer func() { _ = c.Close() }()
	rules, err := c.Bucket(bucket).Object(name).ACL().List(ctx)
	if err != nil {
		sec.Unavailable = "objectAccessControls.list: " + err.Error()
		return sec, true
	}
	list := console.Listing{Columns: []string{"Role", "ACL"}, NameColumn: "Entity", Noun: "ACL entries",
		Items: []console.Resource{}, Note: aclNote}
	for _, r := range rules {
		list.Items = append(list.Items, aclRow(aclScopeObject, []string{aclPage, aclScopeObject, bucket, name, string(r.Entity)}, r))
	}
	list.Total = len(list.Items)
	sec.Listing = list
	return sec, true
}

// bucketACLActions is Add ACL entry on a bucket's page, unless uniform
// access is on.
func bucketACLActions(b bucketMeta) []console.Action {
	if b.IAMConfiguration.UniformBucketLevelAccess.Enabled {
		return nil
	}
	return []console.Action{aclAddAction([]string{aclScopeBucket, aclScopeDefault})}
}

// objectACLActions is Add ACL entry on an object's live page, unless its
// bucket has uniform access.
func (p storageProvider) objectACLActions(ctx context.Context, bucket string) []console.Action {
	b, err := p.readBucket(ctx, bucket)
	if err != nil || b.IAMConfiguration.UniformBucketLevelAccess.Enabled {
		return nil
	}
	return []console.Action{aclAddAction([]string{aclScopeObject})}
}

// aclEntryPath reads a row's address.
func aclEntryPath(path []string) (scope, bucket, object, entity string, ok bool) {
	if len(path) < 4 || path[0] != aclPage {
		return "", "", "", "", false
	}
	switch {
	case (path[1] == aclScopeBucket || path[1] == aclScopeDefault) && len(path) == 4:
		return path[1], path[2], "", path[3], true
	case path[1] == aclScopeObject && len(path) == 5:
		return path[1], path[2], path[3], path[4], true
	}
	return "", "", "", "", false
}

// aclHandle is the ACL a scope names.
func aclHandle(c *storage.Client, scope, bucket, object string) (*storage.ACLHandle, error) {
	switch scope {
	case aclScopeBucket:
		return c.Bucket(bucket).ACL(), nil
	case aclScopeDefault:
		return c.Bucket(bucket).DefaultObjectACL(), nil
	case aclScopeObject:
		return c.Bucket(bucket).Object(object).ACL(), nil
	}
	return nil, fmt.Errorf("ACL %q is not bucket, default or object", scope)
}

// actOnACL performs Add ACL entry and Remove, and reports whether the
// action was one of them.
func (p storageProvider) actOnACL(ctx context.Context, path []string, action string, values map[string]string) (bool, error) {
	var scope, bucket, object, entity string
	switch action {
	case "addacl":
		scope = strings.TrimSpace(values["scope"])
		switch {
		case len(path) == 1 && !strings.HasPrefix(path[0], "_") && (scope == aclScopeBucket || scope == aclScopeDefault):
			bucket = path[0]
		case len(path) == 3 && path[0] == objectPage && scope == aclScopeObject:
			bucket, object = path[1], path[2]
		default:
			return true, errors.New("an ACL entry is added on its bucket's page or its object's page")
		}
		entity = strings.TrimSpace(values["entity"])
		if entity == "" {
			return true, errors.New("name the entity")
		}
	case "removeacl":
		var ok bool
		if scope, bucket, object, entity, ok = aclEntryPath(path); !ok {
			return true, errors.New("remove names one ACL entry")
		}
	default:
		return false, nil
	}
	c, err := p.storageClient(ctx)
	if err != nil {
		return true, err
	}
	defer func() { _ = c.Close() }()
	h, err := aclHandle(c, scope, bucket, object)
	if err != nil {
		return true, err
	}
	if action == "removeacl" {
		return true, h.Delete(ctx, storage.ACLEntity(entity))
	}
	return true, h.Set(ctx, storage.ACLEntity(entity), storage.ACLRole(strings.TrimSpace(values["role"])))
}

// aclEntryActions are the actions at an entry's address, and whether the
// path is one.
func aclEntryActions(path []string) ([]console.Action, bool) {
	if _, _, _, _, ok := aclEntryPath(path); !ok {
		return nil, len(path) > 0 && path[0] == aclPage
	}
	return []console.Action{aclRemoveAction()}, true
}

// defaultObjectACLField is Edit bucket's predefined default object ACL,
// offered while uniform access is off.
func defaultObjectACLField(b bucketMeta) []console.Field {
	if b.IAMConfiguration.UniformBucketLevelAccess.Enabled {
		return nil
	}
	return []console.Field{{Name: "predefinedDefaultObjectAcl", Label: "Default object ACL", Type: "select",
		Section: "Protection", Options: append([]string{aclKeep}, predefinedObjectACLs...), Default: aclKeep,
		Help: "Optional. Replaces the default object ACL, which new objects take, with a predefined one " +
			"(predefinedDefaultObjectAcl); single entries are added and removed on the Access control tab. " +
			"Stored, never enforced."}}
}
