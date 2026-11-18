//go:build compat

package compat

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	credentials "cloud.google.com/go/iam/credentials/apiv1"
	"cloud.google.com/go/iam/credentials/apiv1/credentialspb"
	"golang.org/x/oauth2"
	ci "google.golang.org/api/cloudidentity/v1"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const forumLabel = "cloudidentity.googleapis.com/groups.discussion_forum"

// covers: cloudidentity.groups.search, cloudidentity.groups.memberships.searchDirectGroups, cloudidentity.groups.memberships.searchTransitiveMemberships, cloudidentity.groups.memberships.getMembershipGraph, cloudidentity.groups.memberships.modifyMembershipRoles
//
// TestCloudIdentitySearch1812 (#1812), with the official
// google.golang.org/api/cloudidentity/v1 client: groups.search's query
// (parent, labels, domain_name, group_key and display_name ==,
// startsWith and contains), which must name the parent;
// searchDirectGroups of a member with orderBy; searchTransitiveMemberships
// of the outer group (the nested group DIRECT, its member INDIRECT);
// getMembershipGraph's adjacency list from a member up to the outer group;
// modifyMembershipRoles adding and removing OWNER, refusing to remove
// MEMBER, and updating the MEMBER role's expiry with updateRolesParams.
func TestCloudIdentitySearch1812(t *testing.T) {
	h := New(t)
	svc := cloudIdentity(t, h, h.Endpoint(EnvResourceManager))
	ctx := h.Context()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano()%1e9)
	outer, inner := groupFixture(t, svc, "cs-outer-"+suffix, "cs-inner-"+suffix)

	search := func(q string) map[string]bool {
		t.Helper()
		got := map[string]bool{}
		if err := svc.Groups.Search().Query(q).View("FULL").PageSize(1).Pages(ctx, func(r *ci.SearchGroupsResponse) error {
			for _, g := range r.Groups {
				got[g.Name] = true
			}
			return nil
		}); err != nil {
			t.Fatalf("groups.search %q: %v", q, err)
		}
		return got
	}
	base := "parent == '" + localCustomer + "'"
	for _, tc := range []struct {
		q            string
		outer, inner bool
	}{
		{base + " && '" + forumLabel + "' in labels", true, true},
		{base + " && group_key.startsWith('cs-outer-" + suffix + "')", true, false},
		{base + " && group_key == 'cs-inner-" + suffix + "@" + localDomain + "'", false, true},
		{base + " && group_key.contains('-" + suffix + "@') && display_name == 'Inner'", false, true},
		{base + " && display_name.startsWith('Out') && group_key.contains('" + suffix + "')", true, false},
		{base + " && domain_name == '" + localDomain + "' && group_key.contains('" + suffix + "')", true, true},
		{base + " && domain_name == 'example.com'", false, false},
		{base + " && 'cloudidentity.googleapis.com/groups.security' in labels && group_key.contains('" + suffix + "')", false, false},
	} {
		got := search(tc.q)
		if got[outer.Name] != tc.outer || got[inner.Name] != tc.inner {
			t.Errorf("groups.search %q: outer %v inner %v, want %v %v", tc.q, got[outer.Name], got[inner.Name], tc.outer, tc.inner)
		}
	}
	if _, err := svc.Groups.Search().Query("'" + forumLabel + "' in labels").Context(ctx).Do(); apiCode(err) != http.StatusBadRequest {
		t.Errorf("groups.search without a parent = %v, want 400", err)
	}

	sa := "cs-member-" + suffix + "@example-project.iam.gserviceaccount.com"
	addMember(t, svc, inner.Name, sa)
	nested := addMember(t, svc, outer.Name, inner.GroupKey.Id)

	// searchDirectGroups: the inner group, directly; the outer is not
	// direct.
	var direct []*ci.MembershipRelation
	if err := svc.Groups.Memberships.SearchDirectGroups("groups/-").Query(fmt.Sprintf(
		"member_key_id == '%s' && '%s' in labels", sa, forumLabel)).OrderBy("group_key desc").Pages(ctx,
		func(r *ci.SearchDirectGroupsResponse) error { direct = append(direct, r.Memberships...); return nil }); err != nil {
		t.Fatalf("searchDirectGroups: %v", err)
	}
	if len(direct) != 1 || direct[0].Group != inner.Name || direct[0].GroupKey.Id != inner.GroupKey.Id ||
		!strings.HasPrefix(direct[0].Membership, inner.Name+"/memberships/") || len(direct[0].Roles) != 1 || direct[0].Roles[0].Name != "MEMBER" {
		t.Errorf("searchDirectGroups = %+v; want the inner group's MEMBER membership", direct)
	}
	if _, err := svc.Groups.Memberships.SearchDirectGroups("groups/-").Query("member_key_id == '" + sa + "'").Context(ctx).Do(); apiCode(err) != http.StatusBadRequest {
		t.Errorf("searchDirectGroups without a label = %v, want 400", err)
	}

	tm, err := svc.Groups.Memberships.SearchTransitiveMemberships(outer.Name).Context(ctx).Do()
	if err != nil {
		t.Fatalf("searchTransitiveMemberships: %v", err)
	}
	rel := map[string]string{}
	for _, m := range tm.Memberships {
		if len(m.PreferredMemberKey) == 1 {
			rel[m.PreferredMemberKey[0].Id] = m.RelationType
		}
		if m.PreferredMemberKey[0].Id == inner.GroupKey.Id && m.Member != inner.Name {
			t.Errorf("searchTransitiveMemberships: the nested group's member is %q, want %s", m.Member, inner.Name)
		}
	}
	if len(rel) != 2 || rel[inner.GroupKey.Id] != "DIRECT" || rel[sa] != "INDIRECT" {
		t.Errorf("searchTransitiveMemberships of the outer group = %v; want the inner group DIRECT and %s INDIRECT", rel, sa)
	}

	op, err := svc.Groups.Memberships.GetMembershipGraph(outer.Name).Query(fmt.Sprintf(
		"member_key_id == '%s' && '%s' in labels", sa, forumLabel)).Context(ctx).Do()
	if err != nil || !op.Done {
		t.Fatalf("getMembershipGraph = %+v, %v", op, err)
	}
	var graph ci.GetMembershipGraphResponse
	if err := json.Unmarshal(op.Response, &graph); err != nil {
		t.Fatal(err)
	}
	edges := map[string][]string{}
	for _, al := range graph.AdjacencyList {
		for _, e := range al.Edges {
			edges[al.Group] = append(edges[al.Group], e.PreferredMemberKey.Id)
		}
	}
	if len(graph.Groups) != 2 || len(edges) != 2 || len(edges[outer.Name]) != 1 || edges[outer.Name][0] != inner.GroupKey.Id ||
		len(edges[inner.Name]) != 1 || edges[inner.Name][0] != sa {
		t.Errorf("getMembershipGraph's adjacency list = %v (%d groups); want outer -> inner -> %s", edges, len(graph.Groups), sa)
	}

	mod := func(req *ci.ModifyMembershipRolesRequest) (*ci.Membership, error) {
		r, err := svc.Groups.Memberships.ModifyMembershipRoles(nested.Name, req).Context(ctx).Do()
		if err != nil {
			return nil, err
		}
		return r.Membership, nil
	}
	roles := func(m *ci.Membership) string {
		var out []string
		for _, r := range m.Roles {
			out = append(out, r.Name)
		}
		return strings.Join(out, ",")
	}
	if m, err := mod(&ci.ModifyMembershipRolesRequest{AddRoles: []*ci.MembershipRole{{Name: "OWNER"}}}); err != nil || roles(m) != "OWNER,MEMBER" {
		t.Errorf("modifyMembershipRoles add OWNER = %+v, %v", m, err)
	}
	if _, err := mod(&ci.ModifyMembershipRolesRequest{RemoveRoles: []string{"MEMBER"}}); apiCode(err) != http.StatusBadRequest {
		t.Errorf("modifyMembershipRoles remove MEMBER = %v, want 400", err)
	}
	if m, err := mod(&ci.ModifyMembershipRolesRequest{RemoveRoles: []string{"OWNER"}}); err != nil || roles(m) != "MEMBER" {
		t.Errorf("modifyMembershipRoles remove OWNER = %+v, %v", m, err)
	}
	expire := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)
	upd := &ci.ModifyMembershipRolesRequest{UpdateRolesParams: []*ci.UpdateMembershipRolesParams{{FieldMask: "expiry_detail.expire_time",
		MembershipRole: &ci.MembershipRole{Name: "MEMBER", ExpiryDetail: &ci.ExpiryDetail{ExpireTime: expire.Format(time.RFC3339)}}}}}
	if m, err := mod(upd); err != nil || len(m.Roles) != 1 || m.Roles[0].ExpiryDetail == nil {
		t.Errorf("modifyMembershipRoles updateRolesParams = %+v, %v", m, err)
	} else if got, _ := time.Parse(time.RFC3339Nano, m.Roles[0].ExpiryDetail.ExpireTime); !got.Equal(expire) {
		t.Errorf("the MEMBER role expires %s, want %s", m.Roles[0].ExpiryDetail.ExpireTime, expire)
	}
	upd.AddRoles = []*ci.MembershipRole{{Name: "MANAGER"}}
	if _, err := mod(upd); apiCode(err) != http.StatusBadRequest {
		t.Errorf("modifyMembershipRoles mixing addRoles and updateRolesParams = %v, want 400", err)
	}
}

// covers: cloudidentity.groups.create
//
// TestCloudIdentityInitialOwner1812 (#1812): groups.create with
// initialGroupConfig WITH_INITIAL_OWNER, called with a token the instance
// minted for a service account, makes that account the group's OWNER (and
// MEMBER); EMPTY adds nobody.
func TestCloudIdentityInitialOwner1812(t *testing.T) {
	h := New(t)
	rm := h.Endpoint(EnvResourceManager)
	if rm == "" || h.Endpoint(EnvMetadata) == "" {
		t.Skip("no Resource Manager or metadata endpoint")
	}
	admin := cloudIdentity(t, h, rm)
	p := newIAMPrincipal(t, h, iamProject(t, h, "ci-own-"), "ciown")
	svc, err := ci.NewService(h.Context(), option.WithEndpoint("http://"+strings.TrimPrefix(rm, "http://")+"/"),
		option.WithTokenSource(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: p.token, TokenType: "Bearer"})))
	if err != nil {
		t.Fatal(err)
	}
	ctx := h.Context()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano()%1e9)
	for _, cfg := range []string{"WITH_INITIAL_OWNER", "EMPTY"} {
		email := strings.ToLower(fmt.Sprintf("own-%s-%s@%s", cfg[:5], suffix, localDomain))
		if _, err := svc.Groups.Create(&ci.Group{Parent: localCustomer, GroupKey: &ci.EntityKey{Id: email},
			Labels: map[string]string{forumLabel: ""}}).InitialGroupConfig(cfg).Context(ctx).Do(); err != nil {
			t.Fatalf("groups.create %s: %v", cfg, err)
		}
		name, err := admin.Groups.Lookup().GroupKeyId(email).Context(ctx).Do()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = admin.Groups.Delete(name.Name).Do() })
		ml, err := admin.Groups.Memberships.List(name.Name).Context(ctx).Do()
		if err != nil {
			t.Fatal(err)
		}
		if cfg == "EMPTY" {
			if len(ml.Memberships) != 0 {
				t.Errorf("an EMPTY group has members: %+v", ml.Memberships)
			}
			continue
		}
		if len(ml.Memberships) != 1 || ml.Memberships[0].PreferredMemberKey.Id != p.email || len(ml.Memberships[0].Roles) != 2 ||
			ml.Memberships[0].Roles[0].Name != "OWNER" || ml.Memberships[0].Type != "SERVICE_ACCOUNT" {
			t.Errorf("WITH_INITIAL_OWNER memberships = %+v; want %s as OWNER and MEMBER", ml.Memberships, p.email)
		}
	}
}

// TestIAMCredentialsGroupMember1812 (#1812), with the official
// cloud.google.com/go/iam/credentials/apiv1 and cloudidentity/v1 clients:
// a service account's Token Creator binding to group:outer lets an account
// that is in outer through a nested group mint the service account's
// token, and the nesting's removal takes it away.
func TestIAMCredentialsGroupMember1812(t *testing.T) {
	h := New(t)
	meta := h.Endpoint(EnvMetadata)
	rm := h.Endpoint(EnvResourceManager)
	if meta == "" || rm == "" {
		t.Skip("no metadata or Resource Manager endpoint")
	}
	if !strings.HasPrefix(meta, "http://") {
		meta = "http://" + meta
	}
	svc := cloudIdentity(t, h, rm)
	ctx := h.Context()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano()%1e9)
	outer, inner := groupFixture(t, svc, "ic-outer-"+suffix, "ic-inner-"+suffix)
	project := iamProject(t, h, "ic-grp-")
	p := newIAMPrincipal(t, h, project, "icgrp")
	target := newServiceAccount(t, h, project, "ictgt")
	addMember(t, svc, inner.Name, p.email)
	nested := addMember(t, svc, outer.Name, inner.GroupKey.Id)
	grantOn(t, h, target.GetName(), "roles/iam.serviceAccountTokenCreator", "group:"+outer.GroupKey.Id)

	ic, err := credentials.NewIamCredentialsRESTClient(ctx, option.WithEndpoint(meta),
		option.WithTokenSource(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: p.token, TokenType: "Bearer"})))
	if err != nil {
		t.Fatal(err)
	}
	defer ic.Close()
	mint := func() error {
		_, err := ic.GenerateAccessToken(ctx, &credentialspb.GenerateAccessTokenRequest{
			Name: "projects/-/serviceAccounts/" + target.GetEmail(), Scope: []string{"https://www.googleapis.com/auth/cloud-platform"}})
		return err
	}
	if err := mint(); err != nil {
		t.Fatalf("GenerateAccessToken as a member of group:%s (through %s): %v", outer.GroupKey.Id, inner.GroupKey.Id, err)
	}
	if _, err := svc.Groups.Memberships.Delete(nested.Name).Context(ctx).Do(); err != nil {
		t.Fatal(err)
	}
	if err := mint(); status.Code(err) != codes.PermissionDenied && !strings.Contains(fmt.Sprint(err), "403") {
		t.Errorf("GenerateAccessToken after the nesting was removed = %v, want PERMISSION_DENIED", err)
	}
}

// TestGcloudIdentityGroups (#1812): gcloud identity groups create
// (--customer, which gcloud sends as customerId/{id}), describe, search
// (gcloud's listing of a customer's groups, through groups.search) and
// delete, and memberships add, list and delete, through gcloud-setup's
// cloudidentity override alone, egress blocked.
func TestGcloudIdentityGroups(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvResourceManager)
	g := newGcloudSession(t, h)
	customer := strings.TrimPrefix(localCustomer, "customers/")
	suffix := fmt.Sprintf("%d", time.Now().UnixNano()%1e9)
	email := "gc-" + suffix + "@" + localDomain
	member := "gc-member-" + suffix + "@example-project.iam.gserviceaccount.com"
	g.must("identity", "groups", "create", email, "--customer", customer, "--display-name", "gcloud group",
		"--description", "made by gcloud", "--labels", forumLabel)
	if out := g.must("identity", "groups", "describe", email, "--format=value(groupKey.id,displayName,description)"); !strings.Contains(out, email) ||
		!strings.Contains(out, "gcloud group") || !strings.Contains(out, "made by gcloud") {
		t.Errorf("gcloud identity groups describe = %q", out)
	}
	// search prints groups.search's response, whose groups are a field.
	listed := func() string {
		return g.must("identity", "groups", "search", "--customer", customer, "--labels", forumLabel, "--format=value(groups[].groupKey.id)")
	}
	if out := listed(); !strings.Contains(out, email) {
		t.Errorf("gcloud identity groups search = %q; want %s", out, email)
	}
	g.must("identity", "groups", "memberships", "add", "--group-email", email, "--member-email", member)
	members := func() string {
		return g.must("identity", "groups", "memberships", "list", "--group-email", email, "--format=value(preferredMemberKey.id,roles[].name)")
	}
	if out := members(); !strings.Contains(out, member) || !strings.Contains(out, "MEMBER") {
		t.Errorf("gcloud identity groups memberships list = %q; want %s as MEMBER", out, member)
	}
	g.must("identity", "groups", "memberships", "delete", "--group-email", email, "--member-email", member, "--quiet")
	if out := members(); strings.Contains(out, member) {
		t.Errorf("memberships list after delete = %q", out)
	}
	g.must("identity", "groups", "delete", email, "--quiet")
	if out := listed(); strings.Contains(out, email) {
		t.Errorf("groups search after delete = %q", out)
	}
	if out, err := g.run(nil, "identity", "groups", "describe", email); err == nil {
		t.Errorf("describe of a deleted group succeeded: %s", out)
	}
}
