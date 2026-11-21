//go:build compat

package compat

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	eventarc "cloud.google.com/go/eventarc/apiv1"
	"cloud.google.com/go/eventarc/apiv1/eventarcpb"
	admin "cloud.google.com/go/iam/admin/apiv1"
	"cloud.google.com/go/iam/admin/apiv1/adminpb"
	"cloud.google.com/go/iam/apiv1/iampb"
	iamapi "cloud.google.com/go/iam/apiv2"
	iamv2 "cloud.google.com/go/iam/apiv2/iampb"
	resourcemanager "cloud.google.com/go/resourcemanager/apiv3"
	rmpb "cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"
	"golang.org/x/oauth2"
	crmv1 "google.golang.org/api/cloudresourcemanager/v1"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
)

// rmDenied is Resource Manager's refusal of permission on resource.
func rmDenied(permission, resource string) string {
	return "Permission '" + permission + "' denied on resource '//cloudresourcemanager.googleapis.com/" + resource + "' (or it may not exist)."
}

// iamDenied is the IAM API's refusal of permission.
func iamDenied(permission string) string {
	return "Permission '" + permission + "' denied on resource (or it may not exist)."
}

// searchFinds is whether SearchProjects as c finds project.
func searchFinds(ctx context.Context, c *resourcemanager.ProjectsClient, project string) (bool, error) {
	it := c.SearchProjects(ctx, &rmpb.SearchProjectsRequest{Query: "id:" + project})
	for {
		p, err := it.Next()
		if errors.Is(err, iterator.Done) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if p.GetProjectId() == project {
			return true, nil
		}
	}
}

// covers: google.cloud.resourcemanager.v3.Projects/GetProject, google.cloud.resourcemanager.v3.Projects/GetIamPolicy, google.cloud.resourcemanager.v3.Projects/SetIamPolicy, google.cloud.resourcemanager.v3.Projects/SearchProjects, google.cloud.resourcemanager.v3.TagKeys/CreateTagKey, google.cloud.resourcemanager.v3.TagKeys/GetTagKey, google.cloud.resourcemanager.v3.TagKeys/DeleteTagKey, cloudresourcemanager.projects.get, cloudresourcemanager.projects.list
//
// TestIAMEnforcementResourceManager (#1879), with the official Resource
// Manager v3 client over gRPC and the v1 REST client, with a minted token:
// without a role, GetProject (resourcemanager.projects.get) and
// GetIamPolicy (resourcemanager.projects.getIamPolicy) are refused, a
// search does not find the project, and v1's projects.get is a 403 and its
// list leaves the project out; Browser on the project lets them through but
// not SetIamPolicy, which Project IAM Admin grants. A tag key under the
// project is refused until Tag Viewer is granted on the project, its
// parent. The instance's own account is never refused.
func TestIAMEnforcementResourceManager(t *testing.T) {
	h := New(t)
	rmEndpoint := h.Endpoint(EnvResourceManager)
	h.Endpoint(EnvMetadata)
	requireIAMEnforcement(t)
	ctx := h.Context()
	project := iamProject(t, h, "iorm11-")
	name := "projects/" + project
	p := newIAMPrincipal(t, h, project, "iorm11")
	owner := rmProjects(t, h)
	as, err := resourcemanager.NewProjectsClient(ctx, grpcAs(rmEndpoint, p.token)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = as.Close() })
	v1, err := crmv1.NewService(ctx, option.WithEndpoint("http://"+stripScheme(rmEndpoint)+"/"),
		option.WithHTTPClient(&http.Client{Transport: &oauth2.Transport{Source: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: p.token})}}))
	if err != nil {
		t.Fatal(err)
	}
	get := func() error { _, err := as.GetProject(ctx, &rmpb.GetProjectRequest{Name: name}); return err }
	getPolicy := func() error {
		_, err := as.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: name})
		return err
	}
	v1Get := func() error { _, err := v1.Projects.Get(project).Context(ctx).Do(); return err }
	v1Lists := func() bool {
		l, err := v1.Projects.List().Filter("id:" + project).Context(ctx).Do()
		if err != nil {
			t.Fatalf("v1 projects.list: %v", err)
		}
		for _, x := range l.Projects {
			if x.ProjectId == project {
				return true
			}
		}
		return false
	}
	if _, err := owner.GetProject(ctx, &rmpb.GetProjectRequest{Name: name}); err != nil {
		t.Fatalf("the instance's own account: GetProject = %v", err)
	}
	wantDenied(t, "GetProject without a role", get(), rmDenied("resourcemanager.projects.get", name), "resourcemanager.projects.get", true)
	wantDenied(t, "GetIamPolicy without a role", getPolicy(), rmDenied("resourcemanager.projects.getIamPolicy", name),
		"resourcemanager.projects.getIamPolicy", true)
	if found, err := searchFinds(ctx, as, project); err != nil || found {
		t.Errorf("SearchProjects without a role = %v, %v; want the project left out", found, err)
	}
	var gerr *googleapi.Error
	if err := v1Get(); !errors.As(err, &gerr) || gerr.Code != http.StatusForbidden {
		t.Errorf("v1 projects.get without a role = %v; want 403", err)
	}
	if v1Lists() {
		t.Error("v1 projects.list without a role lists the project")
	}

	p.grant("roles/browser")
	t.Cleanup(func() { p.revoke("roles/browser") })
	waitGranted(t, "GetProject as a Browser", get)
	if err := getPolicy(); err != nil {
		t.Errorf("GetIamPolicy as a Browser: %v", err)
	}
	if found, err := searchFinds(ctx, as, project); err != nil || !found {
		t.Errorf("SearchProjects as a Browser = %v, %v; want the project", found, err)
	}
	if err := v1Get(); err != nil {
		t.Errorf("v1 projects.get as a Browser: %v", err)
	}
	if !v1Lists() {
		t.Error("v1 projects.list as a Browser leaves the project out")
	}
	pol, err := owner.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: name})
	if err != nil {
		t.Fatal(err)
	}
	_, err = as.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: name, Policy: pol})
	wantDenied(t, "SetIamPolicy as a Browser", err, rmDenied("resourcemanager.projects.setIamPolicy", name),
		"resourcemanager.projects.setIamPolicy", true)
	p.grant("roles/resourcemanager.projectIamAdmin")
	t.Cleanup(func() { p.revoke("roles/resourcemanager.projectIamAdmin") })
	waitGranted(t, "SetIamPolicy as a Project IAM Admin", func() error {
		cur, err := owner.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: name})
		if err != nil {
			return err
		}
		_, err = as.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: name, Policy: cur})
		return err
	})

	// A tag key's parent's ancestry applies to it.
	keysOwner, err := resourcemanager.NewTagKeysClient(ctx, rmOptionsAt(rmEndpoint)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = keysOwner.Close() })
	op, err := keysOwner.CreateTagKey(ctx, &rmpb.CreateTagKeyRequest{TagKey: &rmpb.TagKey{Parent: name, ShortName: "env11"}})
	if err != nil {
		t.Fatalf("CreateTagKey as the instance's account: %v", err)
	}
	key, err := op.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if op, err := keysOwner.DeleteTagKey(context.Background(), &rmpb.DeleteTagKeyRequest{Name: key.GetName()}); err == nil {
			_, _ = op.Wait(context.Background())
		}
	})
	keysAs, err := resourcemanager.NewTagKeysClient(ctx, grpcAs(rmEndpoint, p.token)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = keysAs.Close() })
	getKey := func() error { _, err := keysAs.GetTagKey(ctx, &rmpb.GetTagKeyRequest{Name: key.GetName()}); return err }
	wantDenied(t, "GetTagKey without Tag Viewer", getKey(), rmDenied("resourcemanager.tagKeys.get", key.GetName()),
		"resourcemanager.tagKeys.get", true)
	p.grant("roles/resourcemanager.tagViewer")
	t.Cleanup(func() { p.revoke("roles/resourcemanager.tagViewer") })
	waitGranted(t, "GetTagKey as a Tag Viewer on the project", getKey)
}

// covers: google.cloud.resourcemanager.v3.Projects/SetIamPolicy, google.cloud.resourcemanager.v3.Projects/GetIamPolicy, google.iam.v2.Policies/CreatePolicy, google.iam.v2.Policies/DeletePolicy
//
// TestIAMEnforcementOwnerRepairs (#1879): the instance's own account, the
// creator of every project, keeps every permission whatever the project's
// policy says, as Google's creator keeps roles/owner: after it sets a policy
// with no binding at all it still reads and sets the policy. A deny
// policy denying it resourcemanager.projects.setIamPolicy refuses it, and,
// since no iam.denypolicies permission can be denied, it deletes that deny
// policy and sets the policy again.
func TestIAMEnforcementOwnerRepairs(t *testing.T) {
	h := New(t)
	rmEndpoint := h.Endpoint(EnvResourceManager)
	h.Endpoint(EnvMetadata)
	requireIAMEnforcement(t)
	ctx := h.Context()
	project := iamProject(t, h, "iorp11-")
	name := "projects/" + project
	owner := rmProjects(t, h)
	me, _ := instanceAccount(t)
	pol, err := owner.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: name})
	if err != nil {
		t.Fatal(err)
	}
	broken, err := owner.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: name, Policy: &iampb.Policy{Etag: pol.GetEtag()}})
	if err != nil {
		t.Fatalf("SetIamPolicy with no binding: %v", err)
	}
	if len(broken.GetBindings()) != 0 {
		t.Fatalf("the policy kept %v", broken.GetBindings())
	}
	fixed, err := owner.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: name})
	if err != nil {
		t.Fatalf("GetIamPolicy after removing every binding: %v", err)
	}
	fixed.Bindings = []*iampb.Binding{{Role: "roles/owner", Members: []string{"serviceAccount:" + me}}}
	if _, err := owner.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: name, Policy: fixed}); err != nil {
		t.Fatalf("SetIamPolicy repairing the policy: %v", err)
	}

	deny, err := iamapi.NewPoliciesClient(ctx, rmOptionsAt(rmEndpoint)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = deny.Close() })
	op, err := deny.CreatePolicy(ctx, &iamv2.CreatePolicyRequest{Parent: denyParent(name), PolicyId: "lockout",
		Policy: &iamv2.Policy{DisplayName: "lock out the owner", Rules: denyRuleOf(
			[]string{"principal://iam.googleapis.com/projects/-/serviceAccounts/" + me},
			[]string{"cloudresourcemanager.googleapis.com/projects.setIamPolicy"}, nil)}})
	if err != nil {
		t.Fatalf("CreatePolicy: %v", err)
	}
	lock, err := op.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if op, err := deny.DeletePolicy(context.Background(), &iamv2.DeletePolicyRequest{Name: lock.GetName()}); err == nil {
			_, _ = op.Wait(context.Background())
		}
	})
	set := func() error {
		cur, err := owner.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: name})
		if err != nil {
			return err
		}
		_, err = owner.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: name, Policy: cur})
		return err
	}
	wantDenied(t, "SetIamPolicy denied to the owner", set(), rmDenied("resourcemanager.projects.setIamPolicy", name),
		"resourcemanager.projects.setIamPolicy", true)
	dop, err := deny.DeletePolicy(ctx, &iamv2.DeletePolicyRequest{Name: lock.GetName()})
	if err == nil {
		_, err = dop.Wait(ctx)
	}
	if err != nil {
		t.Fatalf("DeletePolicy as the denied owner: %v", err)
	}
	waitGranted(t, "SetIamPolicy once the deny policy is gone", set)
}

// covers: google.iam.admin.v1.IAM/ListServiceAccounts, google.iam.admin.v1.IAM/GetServiceAccount, google.iam.admin.v1.IAM/GetIamPolicy, google.iam.admin.v1.IAM/ListServiceAccountKeys, google.iam.admin.v1.IAM/CreateRole, google.iam.admin.v1.IAM/ListRoles, google.iam.admin.v1.IAM/GetRole, google.iam.admin.v1.IAM/DeleteRole, google.iam.v2.Policies/ListPolicies
//
// TestIAMEnforcementIAMAPI (#1879), with the official IAM admin and IAM
// v2 clients over gRPC and a minted token: without a role, listing the
// project's service accounts (iam.serviceAccounts.list), reading another
// account by projects/- (iam.serviceAccounts.get), its policy
// (iam.serviceAccounts.getIamPolicy, on the account) and its keys
// (iam.serviceAccountKeys.list), creating a custom role (iam.roles.create)
// and listing deny policies (iam.denypolicies.list) are refused with the
// IAM API's words; the predefined roles are anyone's to read. Service
// Account Admin on the account's own policy lets the account and its
// policy be read, not the project's list; Role Administrator on the
// project lets a role be made.
func TestIAMEnforcementIAMAPI(t *testing.T) {
	h := New(t)
	rmEndpoint := h.Endpoint(EnvResourceManager)
	h.Endpoint(EnvMetadata)
	requireIAMEnforcement(t)
	ctx := h.Context()
	project := iamProject(t, h, "ioia11-")
	p := newIAMPrincipal(t, h, project, "ioia11")
	target := newServiceAccount(t, h, project, "ioit11")
	as, err := admin.NewIamClient(ctx, grpcAs(rmEndpoint, p.token)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = as.Close() })
	byDash := "projects/-/serviceAccounts/" + target.GetEmail()
	list := func() error {
		_, err := as.ListServiceAccounts(ctx, &adminpb.ListServiceAccountsRequest{Name: "projects/" + project}).Next()
		if errors.Is(err, iterator.Done) {
			return nil
		}
		return err
	}
	get := func() error {
		_, err := as.GetServiceAccount(ctx, &adminpb.GetServiceAccountRequest{Name: byDash})
		return err
	}
	getPolicy := func() error {
		_, err := as.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: target.GetName()})
		return err
	}
	keys := func() error {
		_, err := as.ListServiceAccountKeys(ctx, &adminpb.ListServiceAccountKeysRequest{Name: target.GetName()})
		return err
	}
	createRole := func() error {
		r, err := as.CreateRole(ctx, &adminpb.CreateRoleRequest{Parent: "projects/" + project, RoleId: "enforced11",
			Role: &adminpb.Role{Title: "enforced", IncludedPermissions: []string{"storage.buckets.get"}}})
		if err == nil {
			_, _ = iamAdmin(t, h).DeleteRole(context.Background(), &adminpb.DeleteRoleRequest{Name: r.GetName()})
		}
		return err
	}
	wantDenied(t, "ListServiceAccounts without a role", list(), iamDenied("iam.serviceAccounts.list"), "iam.serviceAccounts.list", true)
	wantDenied(t, "GetServiceAccount without a role", get(), iamDenied("iam.serviceAccounts.get"), "iam.serviceAccounts.get", true)
	wantDenied(t, "GetIamPolicy without a role", getPolicy(), iamDenied("iam.serviceAccounts.getIamPolicy"),
		"iam.serviceAccounts.getIamPolicy", true)
	wantDenied(t, "ListServiceAccountKeys without a role", keys(), iamDenied("iam.serviceAccountKeys.list"),
		"iam.serviceAccountKeys.list", true)
	wantDenied(t, "CreateRole without a role", createRole(), iamDenied("iam.roles.create"), "iam.roles.create", true)
	if _, err := as.GetRole(ctx, &adminpb.GetRoleRequest{Name: "roles/viewer"}); err != nil {
		t.Errorf("GetRole of a predefined role: %v", err)
	}
	if _, err := as.ListRoles(ctx, &adminpb.ListRolesRequest{}); err != nil {
		t.Errorf("ListRoles of the predefined roles: %v", err)
	}
	deny, err := iamapi.NewPoliciesClient(ctx, grpcAs(rmEndpoint, p.token)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = deny.Close() })
	_, err = deny.ListPolicies(ctx, &iamv2.ListPoliciesRequest{Parent: denyParent("projects/" + project)}).Next()
	if err == nil || !strings.Contains(err.Error(), "iam.denypolicies.list") {
		t.Errorf("ListPolicies without a role = %v; want iam.denypolicies.list refused", err)
	}

	grantOn(t, h, target.GetName(), "roles/iam.serviceAccountAdmin", "serviceAccount:"+p.email)
	waitGranted(t, "GetServiceAccount as its Service Account Admin", get)
	if err := getPolicy(); err != nil {
		t.Errorf("GetIamPolicy as its Service Account Admin: %v", err)
	}
	wantDenied(t, "ListServiceAccounts as one account's admin", list(), iamDenied("iam.serviceAccounts.list"), "iam.serviceAccounts.list", true)
	p.grant("roles/iam.roleAdmin")
	t.Cleanup(func() { p.revoke("roles/iam.roleAdmin") })
	waitGranted(t, "CreateRole as a Role Administrator", createRole)
}

// covers: google.cloud.eventarc.v1.Eventarc/ListTriggers, google.cloud.eventarc.v1.Eventarc/ListChannels, google.cloud.eventarc.v1.Eventarc/GetGoogleChannelConfig, google.cloud.eventarc.v1.Eventarc/ListProviders, google.cloud.eventarc.v1.Eventarc/CreateChannel
//
// TestIAMEnforcementEventarc (#1879), with the official Eventarc client
// over gRPC and a minted token: without a role, ListTriggers
// (eventarc.triggers.list), ListChannels, ListProviders and
// GetGoogleChannelConfig are refused on the location; Eventarc Viewer on
// the project lets them through, not CreateChannel
// (eventarc.channels.create). The instance's own account is never refused.
func TestIAMEnforcementEventarc(t *testing.T) {
	h := New(t)
	runEndpoint := h.Endpoint(EnvRun)
	h.Endpoint(EnvResourceManager)
	h.Endpoint(EnvMetadata)
	requireIAMEnforcement(t)
	ctx := h.Context()
	project := iamProject(t, h, "ioea11-")
	p := newIAMPrincipal(t, h, project, "ioea11")
	loc := "projects/" + project + "/locations/us-central1"
	owner, err := eventarc.NewClient(ctx, grpcAs(runEndpoint, "")...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Close() })
	as, err := eventarc.NewClient(ctx, grpcAs(runEndpoint, p.token)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = as.Close() })
	first := func(err error) error {
		if errors.Is(err, iterator.Done) {
			return nil
		}
		return err
	}
	triggers := func(c *eventarc.Client) error {
		_, err := c.ListTriggers(ctx, &eventarcpb.ListTriggersRequest{Parent: loc}).Next()
		return first(err)
	}
	channels := func() error {
		_, err := as.ListChannels(ctx, &eventarcpb.ListChannelsRequest{Parent: loc}).Next()
		return first(err)
	}
	providers := func() error {
		_, err := as.ListProviders(ctx, &eventarcpb.ListProvidersRequest{Parent: loc}).Next()
		return first(err)
	}
	config := func() error {
		_, err := as.GetGoogleChannelConfig(ctx, &eventarcpb.GetGoogleChannelConfigRequest{Name: loc + "/googleChannelConfig"})
		return err
	}
	if err := triggers(owner); err != nil {
		t.Fatalf("the instance's own account: ListTriggers = %v", err)
	}
	denied := func(permission, resource string) string {
		return "Permission '" + permission + "' denied on resource '" + resource + "' (or it may not exist)."
	}
	wantDenied(t, "ListTriggers without a role", triggers(as), denied("eventarc.triggers.list", loc), "eventarc.triggers.list", true)
	wantDenied(t, "ListChannels without a role", channels(), denied("eventarc.channels.list", loc), "eventarc.channels.list", true)
	wantDenied(t, "ListProviders without a role", providers(), denied("eventarc.providers.list", loc), "eventarc.providers.list", true)
	wantDenied(t, "GetGoogleChannelConfig without a role", config(), denied("eventarc.googleChannelConfigs.get", loc+"/googleChannelConfig"),
		"eventarc.googleChannelConfigs.get", true)
	p.grant("roles/eventarc.viewer")
	t.Cleanup(func() { p.revoke("roles/eventarc.viewer") })
	waitGranted(t, "ListTriggers as an Eventarc Viewer", func() error { return triggers(as) })
	for what, call := range map[string]func() error{"ListChannels": channels, "ListProviders": providers, "GetGoogleChannelConfig": config} {
		if err := call(); err != nil {
			t.Errorf("%s as an Eventarc Viewer: %v", what, err)
		}
	}
	_, err = as.CreateChannel(ctx, &eventarcpb.CreateChannelRequest{Parent: loc, ChannelId: "enforced11",
		Channel: &eventarcpb.Channel{Name: loc + "/channels/enforced11"}})
	wantDenied(t, "CreateChannel as an Eventarc Viewer", err, denied("eventarc.channels.create", loc), "eventarc.channels.create", true)
}
