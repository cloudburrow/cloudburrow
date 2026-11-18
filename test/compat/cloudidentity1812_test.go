//go:build compat

package compat

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	"golang.org/x/oauth2"
	ci "google.golang.org/api/cloudidentity/v1"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
)

// localCustomer is the instance's organization's Cloud Identity customer,
// and localDomain its domain.
const (
	localCustomer = "customers/C0cloudburrow"
	localDomain   = "cloudburrow.local"
)

// cloudIdentity is the official discovery client
// (google.golang.org/api/cloudidentity/v1) at the Resource Manager
// endpoint, which serves Cloud Identity's groups (#1812).
func cloudIdentity(t *testing.T, h *Harness, rm string) *ci.Service {
	t.Helper()
	if rm == "" {
		t.Skip("no Resource Manager endpoint")
	}
	svc, err := ci.NewService(h.Context(), option.WithEndpoint("http://"+strings.TrimPrefix(rm, "http://")+"/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func apiCode(err error) int {
	var g *googleapi.Error
	if errors.As(err, &g) {
		return g.Code
	}
	return 0
}

// covers: cloudidentity.groups.create, cloudidentity.groups.get, cloudidentity.groups.list, cloudidentity.groups.lookup, cloudidentity.groups.patch, cloudidentity.groups.delete, cloudidentity.groups.memberships.create, cloudidentity.groups.memberships.get, cloudidentity.groups.memberships.list, cloudidentity.groups.memberships.lookup, cloudidentity.groups.memberships.delete, cloudidentity.groups.memberships.checkTransitiveMembership, cloudidentity.groups.memberships.searchTransitiveGroups
//
// TestCloudIdentityGroups1812 (#1812), with the official
// google.golang.org/api/cloudidentity/v1 client: Google Groups under the
// instance's customer are created (a done Operation with the group),
// read, listed, looked up by email and patched; a group already taken is
// 409, one outside the customer's domain 400, a missing one 403 (Cloud
// Identity does not tell it from one the caller may not see).
// Memberships (a service account, a nested group) are created, read,
// listed and looked up; checkTransitiveMembership and
// searchTransitiveGroups follow nesting (DIRECT and INDIRECT); deleting a
// membership and the groups undoes them.
func TestCloudIdentityGroups1812(t *testing.T) {
	h := New(t)
	svc := cloudIdentity(t, h, h.Endpoint(EnvResourceManager))
	ctx := h.Context()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano()%1e9)
	outer, inner := groupFixture(t, svc, "ci-outer-"+suffix, "ci-inner-"+suffix)

	g, err := svc.Groups.Get(outer.Name).Context(ctx).Do()
	if err != nil || g.GroupKey.Id != "ci-outer-"+suffix+"@"+localDomain || g.Parent != localCustomer || g.DisplayName != "Outer" ||
		g.CreateTime == "" {
		t.Errorf("groups.get = %+v, %v", g, err)
	}
	look, err := svc.Groups.Lookup().GroupKeyId("CI-Inner-" + suffix + "@" + localDomain).Context(ctx).Do()
	if err != nil || look.Name != inner.Name {
		t.Errorf("groups.lookup = %+v, %v; want %s", look, err, inner.Name)
	}
	var found int
	if err := svc.Groups.List().Parent(localCustomer).View("FULL").PageSize(1).Pages(ctx, func(l *ci.ListGroupsResponse) error {
		for _, x := range l.Groups {
			if x.Name == outer.Name || x.Name == inner.Name {
				found++
			}
		}
		return nil
	}); err != nil || found != 2 {
		t.Errorf("groups.list paged by one: found %d of 2, %v", found, err)
	}
	pop, err := svc.Groups.Patch(outer.Name, &ci.Group{Description: "the outer group"}).UpdateMask("description").Context(ctx).Do()
	if err != nil || !pop.Done {
		t.Errorf("groups.patch = %+v, %v", pop, err)
	} else if g, _ := svc.Groups.Get(outer.Name).Context(ctx).Do(); g == nil || g.Description != "the outer group" {
		t.Errorf("groups.get after patch = %+v", g)
	}

	for _, tc := range []struct {
		what  string
		group *ci.Group
		code  int
	}{
		{"a taken email", &ci.Group{Parent: localCustomer, GroupKey: &ci.EntityKey{Id: "ci-outer-" + suffix + "@" + localDomain},
			Labels: map[string]string{"cloudidentity.googleapis.com/groups.discussion_forum": ""}}, http.StatusConflict},
		{"another domain", &ci.Group{Parent: localCustomer, GroupKey: &ci.EntityKey{Id: "x-" + suffix + "@example.com"},
			Labels: map[string]string{"cloudidentity.googleapis.com/groups.discussion_forum": ""}}, http.StatusBadRequest},
		{"no Google Group label", &ci.Group{Parent: localCustomer, GroupKey: &ci.EntityKey{Id: "y-" + suffix + "@" + localDomain},
			Labels: map[string]string{}}, http.StatusBadRequest},
		{"another customer", &ci.Group{Parent: "customers/C0other", GroupKey: &ci.EntityKey{Id: "z-" + suffix + "@" + localDomain},
			Labels: map[string]string{"cloudidentity.googleapis.com/groups.discussion_forum": ""}}, http.StatusForbidden},
	} {
		if _, err := svc.Groups.Create(tc.group).InitialGroupConfig("EMPTY").Context(ctx).Do(); apiCode(err) != tc.code {
			t.Errorf("groups.create with %s = %v, want %d", tc.what, err, tc.code)
		}
	}
	if _, err := svc.Groups.Get("groups/0nosuchgroup").Context(ctx).Do(); apiCode(err) != http.StatusForbidden {
		t.Errorf("groups.get of a missing group = %v, want 403", err)
	}
	if _, err := svc.Groups.Lookup().GroupKeyId("nobody-" + suffix + "@" + localDomain).Context(ctx).Do(); apiCode(err) != http.StatusForbidden {
		t.Errorf("groups.lookup of a missing group = %v, want 403", err)
	}

	sa := "ci-member-" + suffix + "@example-project.iam.gserviceaccount.com"
	mem := addMember(t, svc, inner.Name, sa)
	if _, err := svc.Groups.Memberships.Create(inner.Name, &ci.Membership{PreferredMemberKey: &ci.EntityKey{Id: sa}}).Context(ctx).Do(); apiCode(err) != http.StatusConflict {
		t.Errorf("a second membership of %s = %v, want 409", sa, err)
	}
	if _, err := svc.Groups.Memberships.Create(inner.Name, &ci.Membership{PreferredMemberKey: &ci.EntityKey{Id: "a-" + suffix + "@" + localDomain},
		Roles: []*ci.MembershipRole{{Name: "MEMBER"}, {Name: "MEMBER"}}}).Context(ctx).Do(); apiCode(err) != http.StatusBadRequest {
		t.Errorf("a membership with MEMBER twice = %v, want 400", err)
	}
	nested := addMember(t, svc, outer.Name, inner.GroupKey.Id)
	m, err := svc.Groups.Memberships.Get(mem.Name).Context(ctx).Do()
	if err != nil || m.Type != "SERVICE_ACCOUNT" || len(m.Roles) != 1 || m.Roles[0].Name != "MEMBER" {
		t.Errorf("memberships.get = %+v, %v; want a SERVICE_ACCOUNT MEMBER", m, err)
	}
	if n, err := svc.Groups.Memberships.Get(nested.Name).Context(ctx).Do(); err != nil || n.Type != "GROUP" {
		t.Errorf("memberships.get of the nested group = %+v, %v; want GROUP", n, err)
	}
	ml, err := svc.Groups.Memberships.List(outer.Name).View("FULL").Context(ctx).Do()
	if err != nil || len(ml.Memberships) != 1 || ml.Memberships[0].PreferredMemberKey.Id != inner.GroupKey.Id {
		t.Errorf("memberships.list of the outer group = %+v, %v", ml, err)
	}
	ml2, err := svc.Groups.Memberships.Lookup(inner.Name).MemberKeyId(sa).Context(ctx).Do()
	if err != nil || ml2.Name != mem.Name {
		t.Errorf("memberships.lookup = %+v, %v; want %s", ml2, err, mem.Name)
	}

	check := func(group, member string) bool {
		t.Helper()
		r, err := svc.Groups.Memberships.CheckTransitiveMembership(group).Query(fmt.Sprintf("member_key_id == '%s'", member)).Context(ctx).Do()
		if err != nil {
			t.Fatalf("checkTransitiveMembership of %s in %s: %v", member, group, err)
		}
		return r.HasMembership
	}
	if !check(outer.Name, sa) || !check(inner.Name, sa) || check(inner.Name, "stranger-"+suffix+"@"+localDomain) {
		t.Errorf("checkTransitiveMembership: want %s in both groups (the outer through the inner), and no stranger", sa)
	}
	rel := map[string]string{}
	if err := svc.Groups.Memberships.SearchTransitiveGroups("groups/-").Query(fmt.Sprintf(
		"member_key_id == '%s' && 'cloudidentity.googleapis.com/groups.discussion_forum' in labels", sa)).Pages(ctx,
		func(r *ci.SearchTransitiveGroupsResponse) error {
			for _, x := range r.Memberships {
				rel[x.Group] = x.RelationType
			}
			return nil
		}); err != nil {
		t.Fatalf("searchTransitiveGroups: %v", err)
	}
	if rel[inner.Name] != "DIRECT" || rel[outer.Name] != "INDIRECT" {
		t.Errorf("searchTransitiveGroups = %v; want %s DIRECT and %s INDIRECT", rel, inner.Name, outer.Name)
	}
	if _, err := svc.Groups.Memberships.SearchTransitiveGroups("groups/-").Query(fmt.Sprintf("member_key_id == '%s'", sa)).Context(ctx).Do(); apiCode(err) != http.StatusBadRequest {
		t.Errorf("searchTransitiveGroups without a label = %v, want 400", err)
	}

	if _, err := svc.Groups.Memberships.Delete(nested.Name).Context(ctx).Do(); err != nil {
		t.Fatalf("memberships.delete: %v", err)
	}
	if check(outer.Name, sa) {
		t.Errorf("checkTransitiveMembership after the nesting was deleted: still a member")
	}
}

// groupFixture creates two Google Groups of the local domain, deleted
// when the test ends.
func groupFixture(t *testing.T, svc *ci.Service, outerID, innerID string) (*ci.Group, *ci.Group) {
	t.Helper()
	var out []*ci.Group
	for i, id := range []string{outerID, innerID} {
		op, err := svc.Groups.Create(&ci.Group{Parent: localCustomer, GroupKey: &ci.EntityKey{Id: id + "@" + localDomain},
			DisplayName: []string{"Outer", "Inner"}[i],
			Labels:      map[string]string{"cloudidentity.googleapis.com/groups.discussion_forum": ""}}).InitialGroupConfig("EMPTY").Do()
		if err != nil || !op.Done {
			t.Fatalf("groups.create %s = %+v, %v", id, op, err)
		}
		name, err := svc.Groups.Lookup().GroupKeyId(id + "@" + localDomain).Do()
		if err != nil {
			t.Fatalf("groups.lookup %s: %v", id, err)
		}
		g, err := svc.Groups.Get(name.Name).Do()
		if err != nil {
			t.Fatalf("groups.get %s: %v", name.Name, err)
		}
		t.Cleanup(func() { _, _ = svc.Groups.Delete(g.Name).Context(context.Background()).Do() })
		out = append(out, g)
	}
	return out[0], out[1]
}

// addMember makes email a MEMBER of group, and returns the membership.
func addMember(t *testing.T, svc *ci.Service, group, email string) *ci.Membership {
	t.Helper()
	op, err := svc.Groups.Memberships.Create(group, &ci.Membership{PreferredMemberKey: &ci.EntityKey{Id: email},
		Roles: []*ci.MembershipRole{{Name: "MEMBER"}}}).Do()
	if err != nil || !op.Done {
		t.Fatalf("memberships.create %s in %s = %+v, %v", email, group, op, err)
	}
	l, err := svc.Groups.Memberships.Lookup(group).MemberKeyId(email).Do()
	if err != nil {
		t.Fatalf("memberships.lookup %s in %s: %v", email, group, err)
	}
	m, err := svc.Groups.Memberships.Get(l.Name).Do()
	if err != nil {
		t.Fatalf("memberships.get %s: %v", l.Name, err)
	}
	return m
}

// covers: bigquery.datasets.patch, bigquery.tabledata.list
//
// TestBigQueryGroupMembers1812 (#1812): with a token the instance minted
// for a service account that is a member of a Cloud Identity group,
// itself a member of another (the official cloudidentity/v1 and
// bigquery clients): a dataset access entry groupByEmail of the outer
// group lets it read the dataset's table, a project binding of
// group:inner grants on the project, and removing the nesting takes the
// dataset's grant away again.
func TestBigQueryGroupMembers1812(t *testing.T) {
	h := New(t)
	endpoint := h.Endpoint(EnvBigQuery)
	if endpoint == "" || h.Endpoint(EnvMetadata) == "" {
		t.Skip("no BigQuery or metadata endpoint")
	}
	rmEndpoint := shardEndpoint(h, "CLOUDBURROW_TEST_BIGQUERY_RESOURCEMANAGER", EnvResourceManager)
	svc := cloudIdentity(t, h, rmEndpoint)
	home, project := bigqueryClient(t, h)
	ctx := h.Context()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano()%1e9)
	outer, inner := groupFixture(t, svc, "bq-outer-"+suffix, "bq-inner-"+suffix)

	ds := "grp" + suffix
	if err := home.Dataset(ds).Create(ctx, &bigquery.DatasetMetadata{Location: "US"}); err != nil {
		t.Fatalf("datasets.insert: %v", err)
	}
	t.Cleanup(func() { _ = home.Dataset(ds).DeleteWithContents(context.Background()) })
	type row struct {
		X int64 `bigquery:"x"`
	}
	for _, tb := range []string{"t"} {
		if err := home.Dataset(ds).Table(tb).Create(ctx, &bigquery.TableMetadata{Schema: bigquery.Schema{{Name: "x", Type: bigquery.IntegerFieldType}}}); err != nil {
			t.Fatalf("tables.insert: %v", err)
		}
		if err := home.Dataset(ds).Table(tb).Inserter().Put(ctx, []row{{X: 7}}); err != nil {
			t.Fatalf("insertAll: %v", err)
		}
	}

	p := newIAMPrincipal(t, h, project, "bqgrp")
	nested := addMember(t, svc, outer.Name, inner.GroupKey.Id)
	addMember(t, svc, inner.Name, p.email)

	tc, err := bigquery.NewClient(ctx, project, option.WithEndpoint(endpoint),
		option.WithTokenSource(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: p.token, TokenType: "Bearer"})))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tc.Close() })
	read := func() error {
		it := tc.Dataset(ds).Table("t").Read(ctx)
		for {
			var r []bigquery.Value
			err := it.Next(&r)
			if errors.Is(err, iterator.Done) {
				return nil
			}
			if err != nil {
				return err
			}
		}
	}
	waitFor := func(what string, want bool) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for {
			err := read()
			if (err == nil) == want {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: tabledata.list = %v, want allowed=%v", what, err, want)
			}
			time.Sleep(500 * time.Millisecond)
		}
	}
	waitFor("before any grant", false)

	md, err := home.Dataset(ds).Metadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := home.Dataset(ds).Update(ctx, bigquery.DatasetMetadataToUpdate{Access: append(md.Access,
		&bigquery.AccessEntry{Role: bigquery.ReaderRole, EntityType: bigquery.GroupEmailEntity, Entity: outer.GroupKey.Id})}, md.ETag); err != nil {
		t.Fatalf("datasets.patch groupByEmail: %v", err)
	}
	waitFor("a READER entry of the outer group, the account in it through the inner group", true)

	if _, err := svc.Groups.Memberships.Delete(nested.Name).Do(); err != nil {
		t.Fatalf("memberships.delete: %v", err)
	}
	waitFor("after the inner group left the outer", false)

	// A project binding of group:inner, which the account is a direct
	// member of.
	change := projectRoles(t, h, project, "group:"+inner.GroupKey.Id)
	change("roles/bigquery.dataViewer", true)
	t.Cleanup(func() { change("roles/bigquery.dataViewer", false) })
	waitFor("roles/bigquery.dataViewer granted to group:"+inner.GroupKey.Id, true)
}
