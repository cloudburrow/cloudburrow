//go:build compat

package compat

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	containeranalysis "cloud.google.com/go/containeranalysis/apiv1"
	iam2 "cloud.google.com/go/iam/apiv2"
	iamv2 "cloud.google.com/go/iam/apiv2/iampb"
	iam3 "cloud.google.com/go/iam/apiv3"
	iamv3 "cloud.google.com/go/iam/apiv3/iampb"
	ncpb "cloud.google.com/go/networkconnectivity/apiv1/networkconnectivitypb"
	"cloud.google.com/go/orgpolicy/apiv2/orgpolicypb"
	"cloud.google.com/go/storagebatchoperations/apiv1/storagebatchoperationspb"
	gpb "google.golang.org/genproto/googleapis/grafeas/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Terraform resources of APIs the instance serves whose provider endpoint
// argument `cloudburrow terraform` did not set, so each was refused
// locally (#1578). Each applies through the wrapper with egress blocked, is
// read back with Google's official client, plans clean, changes in place
// where the provider updates and destroys.

// tfID is a lower-case ID unique to this run, at most n characters.
func tfID(prefix string, n int) string {
	id := fmt.Sprintf("%s%d", prefix, time.Now().UnixNano()%1e8)
	if len(id) > n {
		id = id[:n]
	}
	return id
}

// TestTerraformCloudIdentityGroupAndMembership (#1578): a Google Group of
// the instance's customer and a membership of a service account in it,
// through cloud_identity_custom_endpoint; the official cloudidentity/v1
// client reads both, the plan is clean, the group's display name changes
// in place, and destroy removes both.
// covers: cloudidentity.groups.create, cloudidentity.groups.get, cloudidentity.groups.patch, cloudidentity.groups.delete, cloudidentity.groups.memberships.create, cloudidentity.groups.memberships.get, cloudidentity.groups.memberships.delete
func TestTerraformCloudIdentityGroupAndMembership(t *testing.T) {
	h := New(t)
	svc := cloudIdentity(t, h, h.Endpoint(EnvResourceManager))
	m := newTFModule(t)
	ctx := h.Context()
	email := tfID("tf-ci-", 20) + "@" + localDomain
	member := tfID("tf-cim-", 20) + "@example-project.iam.gserviceaccount.com"
	module := func(display string) string {
		return fmt.Sprintf(`
resource "google_cloud_identity_group" "g" {
  display_name         = %q
  description          = "made by Terraform"
  initial_group_config = "EMPTY"
  parent               = %q
  group_key {
    id = %q
  }
  labels = {
    "cloudidentity.googleapis.com/groups.discussion_forum" = ""
  }
}

resource "google_cloud_identity_group_membership" "m" {
  group = google_cloud_identity_group.g.id
  preferred_member_key {
    id = %q
  }
  roles {
    name = "MEMBER"
  }
}

output "group" {
  value = google_cloud_identity_group.g.name
}

output "membership" {
  value = google_cloud_identity_group_membership.m.name
}
`, display, localCustomer, email, member)
	}
	m.write(module("Terraform group"))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	group := m.must("output", "-raw", "group")
	g, err := svc.Groups.Get(group).Context(ctx).Do()
	if err != nil || g.GroupKey.Id != email || g.DisplayName != "Terraform group" || g.Parent != localCustomer {
		t.Fatalf("groups.get %s = %+v, %v", group, g, err)
	}
	mem, err := svc.Groups.Memberships.Get(m.must("output", "-raw", "membership")).Context(ctx).Do()
	if err != nil || mem.PreferredMemberKey.Id != member || len(mem.Roles) != 1 || mem.Roles[0].Name != "MEMBER" {
		t.Fatalf("memberships.get = %+v, %v", mem, err)
	}
	m.planClean()
	m.write(module("Renamed by Terraform"))
	m.apply()
	if g, err := svc.Groups.Get(group).Context(ctx).Do(); err != nil || g.DisplayName != "Renamed by Terraform" || g.Name != group {
		t.Errorf("after the update groups.get = %+v, %v; want the same group renamed", g, err)
	}
	m.planClean()
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := svc.Groups.Get(group).Context(ctx).Do(); apiCode(err) != 403 && apiCode(err) != 404 {
		t.Errorf("groups.get after destroy = %v; want the group gone", err)
	}
}

// TestTerraformOrgPolicyPolicyAndCustomConstraint (#1578): a project's
// boolean policy and an organization custom constraint, through
// org_policy_custom_endpoint; the official orgpolicy/apiv2 client reads
// them, the plan is clean, both change in place, and destroy removes
// them. The policy is on a project the module creates, so no other test
// sees it.
// covers: google.cloud.orgpolicy.v2.OrgPolicy/CreatePolicy, google.cloud.orgpolicy.v2.OrgPolicy/GetPolicy, google.cloud.orgpolicy.v2.OrgPolicy/UpdatePolicy, google.cloud.orgpolicy.v2.OrgPolicy/DeletePolicy, google.cloud.orgpolicy.v2.OrgPolicy/CreateCustomConstraint, google.cloud.orgpolicy.v2.OrgPolicy/GetCustomConstraint, google.cloud.orgpolicy.v2.OrgPolicy/UpdateCustomConstraint, google.cloud.orgpolicy.v2.OrgPolicy/DeleteCustomConstraint
func TestTerraformOrgPolicyPolicyAndCustomConstraint(t *testing.T) {
	h := New(t)
	c := orgPolicyClients(t, h)["grpc"]
	m := newTFModule(t)
	ctx := h.Context()
	project := tfID("tf-op-", 30)
	constraint := "custom.tfDenyAutoUpgrade" + strings.TrimPrefix(tfID("x", 9), "x")
	module := func(enforce, display string) string {
		return fmt.Sprintf(`
resource "google_project" "p" {
  project_id      = %[1]q
  name            = "Org policy"
  deletion_policy = "DELETE"
}

resource "google_org_policy_policy" "p" {
  name   = "projects/${google_project.p.project_id}/policies/compute.disableSerialPortAccess"
  parent = "projects/${google_project.p.project_id}"
  spec {
    rules {
      enforce = %[2]q
    }
  }
}

resource "google_org_policy_custom_constraint" "c" {
  name           = %[3]q
  parent         = %[4]q
  display_name   = %[5]q
  description    = "Node pools keep auto-upgrade on"
  action_type    = "DENY"
  condition      = "resource.management.autoUpgrade == false"
  method_types   = ["CREATE", "UPDATE"]
  resource_types = ["container.googleapis.com/NodePool"]
}
`, project, enforce, constraint, localOrg, display)
	}
	m.write(module("TRUE", "No auto-upgrade off"))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	policy := "projects/" + project + "/policies/compute.disableSerialPortAccess"
	enforced := func() bool {
		t.Helper()
		p, err := c.GetPolicy(ctx, &orgpolicypb.GetPolicyRequest{Name: policy})
		if err != nil || len(p.GetSpec().GetRules()) != 1 {
			t.Fatalf("GetPolicy %s = %v, %v", policy, p, err)
		}
		return p.GetSpec().GetRules()[0].GetEnforce()
	}
	if !enforced() {
		t.Errorf("after apply %s is not enforced", policy)
	}
	ccName := localOrg + "/customConstraints/" + constraint
	cc, err := c.GetCustomConstraint(ctx, &orgpolicypb.GetCustomConstraintRequest{Name: ccName})
	if err != nil || cc.GetActionType() != orgpolicypb.CustomConstraint_DENY || cc.GetDisplayName() != "No auto-upgrade off" || len(cc.GetMethodTypes()) != 2 {
		t.Fatalf("GetCustomConstraint = %v, %v", cc, err)
	}
	m.planClean()
	m.write(module("FALSE", "Auto-upgrade stays on"))
	m.apply()
	if enforced() {
		t.Errorf("after the update %s is still enforced", policy)
	}
	if cc, err := c.GetCustomConstraint(ctx, &orgpolicypb.GetCustomConstraintRequest{Name: ccName}); err != nil || cc.GetDisplayName() != "Auto-upgrade stays on" {
		t.Errorf("after the update GetCustomConstraint = %v, %v", cc, err)
	}
	m.planClean()
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := c.GetCustomConstraint(ctx, &orgpolicypb.GetCustomConstraintRequest{Name: ccName}); status.Code(err) != codes.NotFound {
		t.Errorf("GetCustomConstraint after destroy = %v; want NOT_FOUND", err)
	}
}

// TestTerraformIAMDenyPolicy (#1578): an IAM v2 deny policy on a project
// the module creates, through iam2_custom_endpoint; the official
// iam/apiv2 client reads it, the plan is clean, its rules change in place,
// and destroy removes it.
// covers: google.iam.v2.Policies/CreatePolicy, google.iam.v2.Policies/GetPolicy, google.iam.v2.Policies/UpdatePolicy, google.iam.v2.Policies/DeletePolicy
func TestTerraformIAMDenyPolicy(t *testing.T) {
	h := New(t)
	ep := h.Endpoint(EnvResourceManager)
	pol, err := iam2.NewPoliciesClient(h.Context(), rmOptionsAt(ep)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pol.Close() })
	m := newTFModule(t)
	ctx := h.Context()
	project := tfID("tf-deny-", 30)
	module := func(perm string) string {
		return fmt.Sprintf(`
resource "google_project" "p" {
  project_id      = %q
  name            = "Deny policy"
  deletion_policy = "DELETE"
}

resource "google_iam_deny_policy" "d" {
  parent       = urlencode("cloudresourcemanager.googleapis.com/projects/${google_project.p.project_id}")
  name         = "tf-deny"
  display_name = "No public updates"
  rules {
    description = "public may not"
    deny_rule {
      denied_principals  = ["principalSet://goog/public:all"]
      denied_permissions = [%q]
    }
  }
}
`, project, perm)
	}
	m.write(module("cloudresourcemanager.googleapis.com/projects.update"))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	name := "policies/" + url.PathEscape("cloudresourcemanager.googleapis.com/projects/"+project) + "/denypolicies/tf-deny"
	read := func() []string {
		t.Helper()
		p, err := pol.GetPolicy(ctx, &iamv2.GetPolicyRequest{Name: name})
		if err != nil || len(p.GetRules()) != 1 || p.GetDisplayName() != "No public updates" {
			t.Fatalf("GetPolicy %s = %v, %v", name, p, err)
		}
		return p.GetRules()[0].GetDenyRule().GetDeniedPermissions()
	}
	if got := read(); len(got) != 1 || got[0] != "cloudresourcemanager.googleapis.com/projects.update" {
		t.Errorf("after apply the denied permissions are %v", got)
	}
	m.planClean()
	m.write(module("cloudresourcemanager.googleapis.com/projects.delete"))
	m.apply()
	if got := read(); len(got) != 1 || got[0] != "cloudresourcemanager.googleapis.com/projects.delete" {
		t.Errorf("after the update the denied permissions are %v", got)
	}
	m.planClean()
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := pol.GetPolicy(ctx, &iamv2.GetPolicyRequest{Name: name}); status.Code(err) != codes.NotFound {
		t.Errorf("GetPolicy after destroy = %v; want NOT_FOUND", err)
	}
}

// TestTerraformIAMPrincipalAccessBoundary (#1578): a principal access
// boundary policy of the organization and its binding to the principals
// of a project the module creates, through iam3_custom_endpoint; the
// official iam/apiv3 clients read them, the plan is clean, both change in
// place, and destroy removes them.
// covers: google.iam.v3.PrincipalAccessBoundaryPolicies/CreatePrincipalAccessBoundaryPolicy, google.iam.v3.PrincipalAccessBoundaryPolicies/GetPrincipalAccessBoundaryPolicy, google.iam.v3.PrincipalAccessBoundaryPolicies/UpdatePrincipalAccessBoundaryPolicy, google.iam.v3.PrincipalAccessBoundaryPolicies/DeletePrincipalAccessBoundaryPolicy, google.iam.v3.PolicyBindings/CreatePolicyBinding, google.iam.v3.PolicyBindings/GetPolicyBinding, google.iam.v3.PolicyBindings/UpdatePolicyBinding, google.iam.v3.PolicyBindings/DeletePolicyBinding
func TestTerraformIAMPrincipalAccessBoundary(t *testing.T) {
	h := New(t)
	ep := h.Endpoint(EnvResourceManager)
	ctx := h.Context()
	pc, err := iam3.NewPrincipalAccessBoundaryPoliciesClient(ctx, rmOptionsAt(ep)...)
	if err != nil {
		t.Fatal(err)
	}
	bc, err := iam3.NewPolicyBindingsClient(ctx, rmOptionsAt(ep)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close(); _ = bc.Close() })
	m := newTFModule(t)
	project := tfID("tf-pab-", 30)
	policyID := tfID("tf-pab-", 30)
	org := strings.TrimPrefix(localOrg, "organizations/")
	module := func(display string) string {
		return fmt.Sprintf(`
resource "google_project" "p" {
  project_id      = %[1]q
  name            = "Boundary project"
  deletion_policy = "DELETE"
}

resource "google_iam_principal_access_boundary_policy" "pab" {
  organization                        = %[2]q
  location                            = "global"
  display_name                        = %[4]q
  principal_access_boundary_policy_id = %[3]q
  details {
    rules {
      description = "the project only"
      resources   = ["//cloudresourcemanager.googleapis.com/projects/${google_project.p.project_id}"]
      effect      = "ALLOW"
    }
    enforcement_version = "1"
  }
}

resource "google_iam_projects_policy_binding" "b" {
  project           = google_project.p.project_id
  location          = "global"
  display_name      = %[4]q
  policy_kind       = "PRINCIPAL_ACCESS_BOUNDARY"
  policy_binding_id = "tf-binding"
  policy            = google_iam_principal_access_boundary_policy.pab.id
  target {
    principal_set = "//cloudresourcemanager.googleapis.com/projects/${google_project.p.project_id}"
  }
}
`, project, org, policyID, display)
	}
	m.write(module("Terraform boundary"))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	policy := localOrg + "/locations/global/principalAccessBoundaryPolicies/" + policyID
	binding := "projects/" + project + "/locations/global/policyBindings/tf-binding"
	read := func(display string) {
		t.Helper()
		p, err := pc.GetPrincipalAccessBoundaryPolicy(ctx, &iamv3.GetPrincipalAccessBoundaryPolicyRequest{Name: policy})
		if err != nil || p.GetDisplayName() != display || len(p.GetDetails().GetRules()) != 1 {
			t.Fatalf("GetPrincipalAccessBoundaryPolicy = %v, %v", p, err)
		}
		b, err := bc.GetPolicyBinding(ctx, &iamv3.GetPolicyBindingRequest{Name: binding})
		if err != nil || b.GetPolicy() != policy || b.GetDisplayName() != display || b.GetPolicyKind() != iamv3.PolicyBinding_PRINCIPAL_ACCESS_BOUNDARY {
			t.Fatalf("GetPolicyBinding = %v, %v", b, err)
		}
	}
	read("Terraform boundary")
	m.planClean()
	m.write(module("Renamed boundary"))
	m.apply()
	read("Renamed boundary")
	m.planClean()
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := pc.GetPrincipalAccessBoundaryPolicy(ctx, &iamv3.GetPrincipalAccessBoundaryPolicyRequest{Name: policy}); status.Code(err) != codes.NotFound {
		t.Errorf("GetPrincipalAccessBoundaryPolicy after destroy = %v; want NOT_FOUND", err)
	}
}

// TestTerraformIAMWorkforcePool (#1578): a workforce pool of the
// organization and an OIDC provider in it, through
// iam_workforce_pool_custom_endpoint; the official iam/v1 client reads
// them, the plan is clean, both change in place, and destroy deletes
// them (soft-deleted, as on Google).
// covers: iam.locations.workforcePools.create, iam.locations.workforcePools.get, iam.locations.workforcePools.patch, iam.locations.workforcePools.delete, iam.locations.workforcePools.providers.create, iam.locations.workforcePools.providers.get, iam.locations.workforcePools.providers.patch, iam.locations.workforcePools.providers.delete
func TestTerraformIAMWorkforcePool(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvResourceManager)
	svc := iamREST(t, h)
	m := newTFModule(t)
	ctx := h.Context()
	pool := wfPoolID(h, "tfwf-")
	module := func(display string) string {
		return fmt.Sprintf(`
resource "google_iam_workforce_pool" "p" {
  workforce_pool_id = %[1]q
  parent            = %[2]q
  location          = "global"
  display_name      = %[3]q
  description       = "made by Terraform"
  session_duration  = "7200s"
}

resource "google_iam_workforce_pool_provider" "o" {
  workforce_pool_id   = google_iam_workforce_pool.p.workforce_pool_id
  location            = "global"
  provider_id         = "tf-oidc"
  display_name        = %[3]q
  attribute_mapping   = { "google.subject" = "assertion.sub" }
  attribute_condition = "true"
  oidc {
    issuer_uri = "https://accounts.example.com"
    client_id  = "client-id"
    web_sso_config {
      response_type             = "ID_TOKEN"
      assertion_claims_behavior = "ONLY_ID_TOKEN_CLAIMS"
    }
  }
}
`, pool, localOrg, display)
	}
	m.write(module("Terraform pool"))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	name := "locations/global/workforcePools/" + pool
	read := func(display string) {
		t.Helper()
		p, err := svc.Locations.WorkforcePools.Get(name).Context(ctx).Do()
		if err != nil || p.DisplayName != display || p.SessionDuration != "7200s" || p.Parent != localOrg {
			t.Fatalf("workforcePools.get = %+v, %v", p, err)
		}
		pr, err := svc.Locations.WorkforcePools.Providers.Get(name + "/providers/tf-oidc").Context(ctx).Do()
		if err != nil || pr.DisplayName != display || pr.Oidc == nil || pr.Oidc.IssuerUri != "https://accounts.example.com" {
			t.Fatalf("providers.get = %+v, %v", pr, err)
		}
	}
	read("Terraform pool")
	m.planClean()
	m.write(module("Renamed pool"))
	m.apply()
	read("Renamed pool")
	m.planClean()
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if p, err := svc.Locations.WorkforcePools.Get(name).Context(ctx).Do(); err != nil || p.State != "DELETED" {
		t.Errorf("workforcePools.get after destroy = %+v, %v; want DELETED", p, err)
	}
}

// TestTerraformServiceAccountAccessToken (#1578): the
// google_service_account_access_token, _id_token and _jwt data sources
// call IAM Credentials' generateAccessToken, generateIdToken and signJwt
// for a service account the module creates, through
// iam_credentials_custom_endpoint at the metadata server. That API
// authenticates its caller whatever --iam-enforcement says, as Google's
// does, so `cloudburrow terraform` signs the provider in as the instance's
// owner for such a module rather than as "owner", and, as Google's
// example does, the module grants that owner Service Account Token
// Creator on the account. The ID token's audience
// and the JWT's claims are read back, the plan is clean, and destroy
// removes the account.
// covers: iamcredentials.projects.serviceAccounts.generateAccessToken, iamcredentials.projects.serviceAccounts.generateIdToken, iamcredentials.projects.serviceAccounts.signJwt
func TestTerraformServiceAccountAccessToken(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvMetadata)
	h.Endpoint(EnvResourceManager)
	path := strings.TrimSpace(os.Getenv(EnvCredentials))
	if path == "" {
		t.Skipf("%s is not set", EnvCredentials)
	}
	raw, err := os.ReadFile(path)
	var fixture struct {
		ClientEmail string `json:"client_email"`
	}
	if err != nil || json.Unmarshal(raw, &fixture) != nil || fixture.ClientEmail == "" {
		t.Fatalf("the credentials fixture names no account: %v", err)
	}
	m := newTFModule(t)
	account := tfID("tf-tok-", 28)
	m.write(fmt.Sprintf(`
resource "google_service_account" "sa" {
  account_id = %q
}

# As Google documents: the caller needs Service Account Token Creator on
# the account; the project's owner does not hold it.
resource "google_service_account_iam_member" "creator" {
  service_account_id = google_service_account.sa.name
  role               = "roles/iam.serviceAccountTokenCreator"
  member             = "serviceAccount:%s"
}

data "google_service_account_access_token" "t" {
  target_service_account = google_service_account.sa.email
  depends_on             = [google_service_account_iam_member.creator]
  scopes                 = ["cloud-platform"]
  lifetime               = "300s"
}

data "google_service_account_id_token" "i" {
  target_service_account = google_service_account.sa.email
  depends_on             = [google_service_account_iam_member.creator]
  target_audience        = "https://tf.example.com"
  include_email          = true
}

data "google_service_account_jwt" "j" {
  target_service_account = google_service_account.sa.email
  depends_on             = [google_service_account_iam_member.creator]
  payload                = jsonencode({ sub = "tf", aud = "https://tf.example.com" })
}

output "minted" {
  value = nonsensitive(length(data.google_service_account_access_token.t.access_token) > 20)
}

output "jwt_parts" {
  value = nonsensitive(length(split(".", data.google_service_account_jwt.j.jwt)))
}

output "email" {
  value = google_service_account.sa.email
}
`, account, fixture.ClientEmail))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	if got := m.must("output", "-raw", "minted"); got != "true" {
		t.Errorf("the access token data source minted no token: %q", got)
	}
	email := m.must("output", "-raw", "email")
	var claims string
	// The token is read from the state, not an output: each plan mints a
	// new one, as on Google, so an output of it would never plan clean.
	var state struct {
		Values struct {
			RootModule struct {
				Resources []struct {
					Address string
					Values  map[string]any
				}
			} `json:"root_module"`
		}
	}
	if err := json.Unmarshal([]byte(m.stdout("show", "-json", "-no-color")), &state); err != nil {
		t.Fatalf("show -json: %v", err)
	}
	idToken := ""
	for _, r := range state.Values.RootModule.Resources {
		if r.Address == "data.google_service_account_id_token.i" {
			idToken, _ = r.Values["id_token"].(string)
		}
	}
	if parts := strings.Split(idToken, "."); len(parts) == 3 {
		b, _ := base64.RawURLEncoding.DecodeString(parts[1])
		claims = string(b)
	}
	if !strings.Contains(claims, `"aud":"https://tf.example.com"`) || !strings.Contains(claims, `"email":"`+email+`"`) {
		t.Errorf("the ID token's claims are %s; want the audience and %s", claims, email)
	}
	if got := m.must("output", "-raw", "jwt_parts"); got != "3" {
		t.Errorf("the signed JWT has %s parts; want 3", got)
	}
	m.planClean()
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
}

// TestTerraformStorageBatchOperationsJob (#1578): a batch operations job
// that sets custom metadata on a bucket's objects under a prefix, through
// storage_batch_operations_custom_endpoint, in a project the module gives
// Storage Intelligence, as Google requires of batch operations; the official
// storagebatchoperations client reads it, the object gets the metadata,
// the plan is clean, and destroy deletes the job.
// covers: storagebatchoperations.projects.locations.jobs.create, storagebatchoperations.projects.locations.jobs.get, storagebatchoperations.projects.locations.jobs.delete
func TestTerraformStorageBatchOperationsJob(t *testing.T) {
	h := New(t)
	c := batchClient(t, h)
	sc := storageClient(t, h)
	m := newTFModule(t)
	ctx := h.Context()
	bucket := h.Project() + "-tfbo"
	job := tfID("tf-job-", 40)
	// The bucket and object are the client's: the job changes the object's
	// metadata, which a google_storage_bucket_object would then plan to
	// replace, on Google too.
	putCompanionObject(t, ctx, companionBucket(t, h, sc, bucket, "us-central1"), "bkt/one.txt", "one")
	m.write(fmt.Sprintf(`
resource "google_storage_control_project_intelligence_config" "i" {
  name           = %q
  edition_config = "STANDARD"
}

resource "google_storage_batch_operations_job" "j" {
  job_id = %q
  bucket_list {
    buckets {
      bucket = %q
      prefix_list {
        included_object_prefixes = ["bkt"]
      }
    }
  }
  put_metadata {
    custom_metadata = { "key" = "value" }
  }
  delete_protection = false
  depends_on        = [google_storage_control_project_intelligence_config.i]
}
`, m.project, job, bucket))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	name := "projects/" + m.project + "/locations/global/jobs/" + job
	j, err := c.GetJob(ctx, &storagebatchoperationspb.GetJobRequest{Name: name})
	if err != nil || len(j.GetBucketList().GetBuckets()) != 1 || j.GetPutMetadata().GetCustomMetadata()["key"] != "value" {
		t.Fatalf("GetJob %s = %v, %v", name, j, err)
	}
	deadline := time.Now().Add(2 * time.Minute)
	for {
		attrs, err := sc.Bucket(bucket).Object("bkt/one.txt").Attrs(ctx)
		if err == nil && attrs.Metadata["key"] == "value" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the job never set the object's metadata: %v, %v", attrs, err)
		}
		time.Sleep(time.Second)
	}
	m.planClean()
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := c.GetJob(context.Background(), &storagebatchoperationspb.GetJobRequest{Name: name}); status.Code(err) != codes.NotFound {
		t.Errorf("GetJob after destroy = %v; want NOT_FOUND", err)
	}
}

// TestTerraformContainerAnalysisNoteAndOccurrence (#1578): an attestation
// note and an occurrence of it on an image, through
// container_analysis_custom_endpoint on the Run port; the official
// containeranalysis client reads them, the plan is clean, the note changes
// in place, and destroy removes both.
// covers: grafeas.v1.Grafeas/CreateNote, grafeas.v1.Grafeas/GetNote, grafeas.v1.Grafeas/UpdateNote, grafeas.v1.Grafeas/DeleteNote, grafeas.v1.Grafeas/CreateOccurrence, grafeas.v1.Grafeas/GetOccurrence, grafeas.v1.Grafeas/DeleteOccurrence
func TestTerraformContainerAnalysisNoteAndOccurrence(t *testing.T) {
	h := New(t)
	ca, err := containeranalysis.NewClient(h.Context(), runClientOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ca.Close() })
	g := ca.GetGrafeasClient()
	m := newTFModule(t)
	ctx := h.Context()
	note := tfID("tf-note-", 30)
	module := func(hint string) string {
		return fmt.Sprintf(`
resource "google_container_analysis_note" "n" {
  name              = %q
  short_description = "made by Terraform"
  attestation_authority {
    hint {
      human_readable_name = %q
    }
  }
}

resource "google_container_analysis_occurrence" "o" {
  resource_uri = "https://us-docker.pkg.dev/%s/repo/image@sha256:0000000000000000000000000000000000000000000000000000000000000000"
  note_name    = google_container_analysis_note.n.id
  attestation {
    serialized_payload = base64encode("{\"critical\":{}}")
    signatures {
      public_key_id = "//cloudkms.googleapis.com/v1/projects/p/locations/global/keyRings/r/cryptoKeys/k/cryptoKeyVersions/1"
      signature     = base64encode("signature")
    }
  }
}

output "occurrence" {
  value = google_container_analysis_occurrence.o.name
}
`, note, hint, m.project)
	}
	m.write(module("Terraform attestor"))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	noteName := "projects/" + m.project + "/notes/" + note
	readNote := func(hint string) {
		t.Helper()
		n, err := g.GetNote(ctx, &gpb.GetNoteRequest{Name: noteName})
		if err != nil || n.GetAttestation().GetHint().GetHumanReadableName() != hint {
			t.Fatalf("GetNote = %v, %v", n, err)
		}
	}
	readNote("Terraform attestor")
	occ := m.must("output", "-raw", "occurrence")
	if !strings.HasPrefix(occ, "projects/") {
		occ = "projects/" + m.project + "/occurrences/" + occ
	}
	if o, err := g.GetOccurrence(ctx, &gpb.GetOccurrenceRequest{Name: occ}); err != nil || o.GetNoteName() != noteName {
		t.Fatalf("GetOccurrence %s = %v, %v", occ, o, err)
	}
	m.planClean()
	m.write(module("Renamed attestor"))
	m.apply()
	readNote("Renamed attestor")
	m.planClean()
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := g.GetNote(ctx, &gpb.GetNoteRequest{Name: noteName}); status.Code(err) != codes.NotFound {
		t.Errorf("GetNote after destroy = %v; want NOT_FOUND", err)
	}
}

// TestTerraformNetworkConnectivityServiceConnectionPolicy (#1578): a
// service connection policy on a network and subnetwork the module
// creates, through network_connectivity_custom_endpoint on the Compute
// Engine port; the official networkconnectivity client reads it, the plan
// is clean, its limit changes in place, and destroy removes it.
// covers: google.cloud.networkconnectivity.v1.CrossNetworkAutomationService/CreateServiceConnectionPolicy, google.cloud.networkconnectivity.v1.CrossNetworkAutomationService/GetServiceConnectionPolicy, google.cloud.networkconnectivity.v1.CrossNetworkAutomationService/UpdateServiceConnectionPolicy, google.cloud.networkconnectivity.v1.CrossNetworkAutomationService/DeleteServiceConnectionPolicy
func TestTerraformNetworkConnectivityServiceConnectionPolicy(t *testing.T) {
	h := New(t)
	c := ncClient(t, h)
	m := newTFModule(t)
	ctx := h.Context()
	nw := tfID("tf-scp-", 30)
	module := func(limit int) string {
		return fmt.Sprintf(`
resource "google_compute_network" "n" {
  name                    = %[1]q
  auto_create_subnetworks = false
}

resource "google_compute_subnetwork" "s" {
  name          = %[1]q
  ip_cidr_range = "10.140.0.0/28"
  region        = "us-central1"
  network       = google_compute_network.n.id
}

resource "google_network_connectivity_service_connection_policy" "p" {
  name          = %[1]q
  location      = "us-central1"
  service_class = "tf-class"
  description   = "made by Terraform"
  network       = google_compute_network.n.id
  psc_config {
    subnetworks = [google_compute_subnetwork.s.id]
    limit       = %[2]d
  }
}
`, nw, limit)
	}
	m.write(module(1))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	name := "projects/" + m.project + "/locations/us-central1/serviceConnectionPolicies/" + nw
	read := func() int64 {
		t.Helper()
		p, err := c.GetServiceConnectionPolicy(ctx, &ncpb.GetServiceConnectionPolicyRequest{Name: name})
		if err != nil || p.GetServiceClass() != "tf-class" || len(p.GetPscConfig().GetSubnetworks()) != 1 {
			t.Fatalf("GetServiceConnectionPolicy = %v, %v", p, err)
		}
		return p.GetPscConfig().GetLimit()
	}
	if got := read(); got != 1 {
		t.Errorf("after apply the limit is %d; want 1", got)
	}
	m.planClean()
	m.write(module(2))
	m.apply()
	if got := read(); got != 2 {
		t.Errorf("after the update the limit is %d; want 2", got)
	}
	m.planClean()
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := c.GetServiceConnectionPolicy(ctx, &ncpb.GetServiceConnectionPolicyRequest{Name: name}); status.Code(err) != codes.NotFound {
		t.Errorf("GetServiceConnectionPolicy after destroy = %v; want NOT_FOUND", err)
	}
}
