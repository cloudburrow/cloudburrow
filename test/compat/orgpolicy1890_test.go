//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	admin "cloud.google.com/go/iam/admin/apiv1"
	"cloud.google.com/go/iam/admin/apiv1/adminpb"
	"cloud.google.com/go/iam/apiv1/iampb"
	kmspb "cloud.google.com/go/kms/apiv1/kmspb"
	orgpolicy "cloud.google.com/go/orgpolicy/apiv2"
	"cloud.google.com/go/orgpolicy/apiv2/orgpolicypb"
	resourcemanager "cloud.google.com/go/resourcemanager/apiv3"
	rmpb "cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"
	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"cloud.google.com/go/storage"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/genproto/googleapis/type/expr"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func opValues(allowed ...string) *orgpolicypb.PolicySpec {
	return &orgpolicypb.PolicySpec{Rules: []*orgpolicypb.PolicySpec_PolicyRule{{Kind: &orgpolicypb.PolicySpec_PolicyRule_Values{
		Values: &orgpolicypb.PolicySpec_PolicyRule_StringValues{AllowedValues: allowed}}}}}
}

func opEnforce() *orgpolicypb.PolicySpec {
	return &orgpolicypb.PolicySpec{Rules: []*orgpolicypb.PolicySpec_PolicyRule{{Kind: &orgpolicypb.PolicySpec_PolicyRule_Enforce{Enforce: true}}}}
}

// orgPolicyClients are the official orgpolicy/apiv2 clients over gRPC and
// JSON on the Resource Manager port.
func orgPolicyClients(t *testing.T, h *Harness) map[string]*orgpolicy.Client {
	t.Helper()
	ep := h.Endpoint(EnvResourceManager)
	g, err := orgpolicy.NewClient(h.Context(), rmOptionsAt(ep)...)
	if err != nil {
		t.Fatal(err)
	}
	r, err := orgpolicy.NewRESTClient(h.Context(), option.WithEndpoint("http://"+ep), option.WithoutAuthentication(), option.WithHTTPClient(&http.Client{}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = g.Close(); _ = r.Close() })
	return map[string]*orgpolicy.Client{"grpc": g, "rest": r}
}

// setOrgPolicy sets spec for constraint on parent and removes it at the
// end of the test.
func setOrgPolicy(t *testing.T, h *Harness, c *orgpolicy.Client, parent, constraint string, spec *orgpolicypb.PolicySpec) {
	t.Helper()
	name := parent + "/policies/" + constraint
	if _, err := c.CreatePolicy(h.Context(), &orgpolicypb.CreatePolicyRequest{Parent: parent, Policy: &orgpolicypb.Policy{Name: name, Spec: spec}}); err != nil {
		t.Fatalf("CreatePolicy %s: %v", name, err)
	}
	t.Cleanup(func() { _ = c.DeletePolicy(context.Background(), &orgpolicypb.DeletePolicyRequest{Name: name}) })
}

func opCode(err error) codes.Code {
	var ge *googleapi.Error
	if errors.As(err, &ge) {
		switch ge.Code {
		case 400:
			return codes.InvalidArgument
		case 404:
			return codes.NotFound
		case 409:
			if strings.Contains(ge.Message, "etag") {
				return codes.Aborted
			}
			return codes.AlreadyExists
		case 412:
			return codes.FailedPrecondition
		}
		return codes.Unknown
	}
	return status.Code(err)
}

// covers: google.cloud.orgpolicy.v2.OrgPolicy/CreatePolicy, google.cloud.orgpolicy.v2.OrgPolicy/GetPolicy, google.cloud.orgpolicy.v2.OrgPolicy/UpdatePolicy, google.cloud.orgpolicy.v2.OrgPolicy/DeletePolicy, google.cloud.orgpolicy.v2.OrgPolicy/ListPolicies, google.cloud.orgpolicy.v2.OrgPolicy/GetEffectivePolicy, google.cloud.orgpolicy.v2.OrgPolicy/ListConstraints, google.cloud.orgpolicy.v2.OrgPolicy/CreateCustomConstraint, google.cloud.orgpolicy.v2.OrgPolicy/GetCustomConstraint, google.cloud.orgpolicy.v2.OrgPolicy/UpdateCustomConstraint, google.cloud.orgpolicy.v2.OrgPolicy/ListCustomConstraints, google.cloud.orgpolicy.v2.OrgPolicy/DeleteCustomConstraint
//
// TestOrgPolicyV2 (#1890), through the official
// cloud.google.com/go/orgpolicy/apiv2 client over gRPC and JSON: a policy
// is created (ALREADY_EXISTS a second time), read back named with the
// project's number, updated (ABORTED on a stale etag), listed and deleted
// (NOT_FOUND after); getEffectivePolicy merges a folder's inheriting list
// policy with the organization's, and a reset restores the default; the
// constraints are listed; a custom constraint is created, read, updated,
// listed and deleted.
func TestOrgPolicyV2(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	clients := orgPolicyClients(t, h)
	fc, _, pc := foldersClientsAt(t, h, rmOptionsAt(h.Endpoint(EnvResourceManager)))
	for kind, c := range clients {
		t.Run(kind, func(t *testing.T) {
			folder := orgPolicyFolder(t, h, fc, "opf-"+kind[:1]+"-")
			project := orgPolicyProject(t, h, pc, folder, "op"+kind[:1]+"-")
			name := "projects/" + project + "/policies/compute.requireOsLogin"
			p, err := c.CreatePolicy(ctx, &orgpolicypb.CreatePolicyRequest{Parent: "projects/" + project,
				Policy: &orgpolicypb.Policy{Name: name, Spec: opEnforce()}})
			if err != nil {
				t.Fatalf("CreatePolicy: %v", err)
			}
			if !strings.HasPrefix(p.GetName(), "projects/") || strings.Contains(p.GetName(), project) || p.GetEtag() == "" {
				t.Errorf("CreatePolicy = %v, want the project's number in the name and an etag", p)
			}
			if _, err := c.CreatePolicy(ctx, &orgpolicypb.CreatePolicyRequest{Parent: "projects/" + project, Policy: &orgpolicypb.Policy{Name: name, Spec: opEnforce()}}); opCode(err) != codes.AlreadyExists {
				t.Errorf("CreatePolicy again = %v, want ALREADY_EXISTS", err)
			}
			got, err := c.GetPolicy(ctx, &orgpolicypb.GetPolicyRequest{Name: name})
			if err != nil || !got.GetSpec().GetRules()[0].GetEnforce() {
				t.Fatalf("GetPolicy = %v, %v", got, err)
			}
			if _, err := c.UpdatePolicy(ctx, &orgpolicypb.UpdatePolicyRequest{Policy: &orgpolicypb.Policy{Name: name, Etag: "c3RhbGU=", Spec: opEnforce()}}); opCode(err) != codes.Aborted {
				t.Errorf("UpdatePolicy with a stale etag = %v, want ABORTED", err)
			}
			off := &orgpolicypb.PolicySpec{Rules: []*orgpolicypb.PolicySpec_PolicyRule{{Kind: &orgpolicypb.PolicySpec_PolicyRule_Enforce{Enforce: false}}}}
			if _, err := c.UpdatePolicy(ctx, &orgpolicypb.UpdatePolicyRequest{Policy: &orgpolicypb.Policy{Name: name, Etag: got.GetEtag(), Spec: off}}); err != nil {
				t.Fatalf("UpdatePolicy: %v", err)
			}
			it := c.ListPolicies(ctx, &orgpolicypb.ListPoliciesRequest{Parent: "projects/" + project})
			n := 0
			for {
				_, err := it.Next()
				if errors.Is(err, iterator.Done) {
					break
				}
				if err != nil {
					t.Fatalf("ListPolicies: %v", err)
				}
				n++
			}
			if n != 1 {
				t.Errorf("ListPolicies = %d, want 1", n)
			}
			if err := c.DeletePolicy(ctx, &orgpolicypb.DeletePolicyRequest{Name: name}); err != nil {
				t.Fatalf("DeletePolicy: %v", err)
			}
			if _, err := c.GetPolicy(ctx, &orgpolicypb.GetPolicyRequest{Name: name}); opCode(err) != codes.NotFound {
				t.Errorf("GetPolicy after delete = %v, want NOT_FOUND", err)
			}

			// The folder merges europe-west1 into the organization-wide
			// default (no policy above: its values alone), the project
			// inherits; a reset on the project restores the default.
			setOrgPolicy(t, h, c, folder, "gcp.resourceLocations", &orgpolicypb.PolicySpec{InheritFromParent: true, Rules: opValues("in:us-locations", "europe-west1").Rules})
			eff, err := c.GetEffectivePolicy(ctx, &orgpolicypb.GetEffectivePolicyRequest{Name: "projects/" + project + "/policies/gcp.resourceLocations"})
			if err != nil || len(eff.GetSpec().GetRules()) != 1 || len(eff.GetSpec().GetRules()[0].GetValues().GetAllowedValues()) != 2 {
				t.Fatalf("GetEffectivePolicy = %v, %v; want the folder's values", eff, err)
			}
			setOrgPolicy(t, h, c, "projects/"+project, "gcp.resourceLocations", &orgpolicypb.PolicySpec{Reset_: true})
			eff, err = c.GetEffectivePolicy(ctx, &orgpolicypb.GetEffectivePolicyRequest{Name: "projects/" + project + "/policies/gcp.resourceLocations"})
			if err != nil || len(eff.GetSpec().GetRules()) != 0 {
				t.Fatalf("GetEffectivePolicy under a reset = %v, %v; want the default", eff, err)
			}

			cit := c.ListConstraints(ctx, &orgpolicypb.ListConstraintsRequest{Parent: "projects/" + project})
			seen := map[string]bool{}
			for {
				con, err := cit.Next()
				if errors.Is(err, iterator.Done) {
					break
				}
				if err != nil {
					t.Fatalf("ListConstraints: %v", err)
				}
				seen[con.GetName()[strings.LastIndex(con.GetName(), "/")+1:]] = true
			}
			for _, want := range []string{"gcp.resourceLocations", "iam.allowedPolicyMemberDomains", "storage.uniformBucketLevelAccess", "iam.disableServiceAccountKeyCreation"} {
				if !seen[want] {
					t.Errorf("ListConstraints lacks %s", want)
				}
			}

			cc := &orgpolicypb.CustomConstraint{Name: localOrg + "/customConstraints/custom.cbTest" + strings.ToUpper(kind[:1]),
				ResourceTypes: []string{"container.googleapis.com/NodePool"}, MethodTypes: []orgpolicypb.CustomConstraint_MethodType{orgpolicypb.CustomConstraint_CREATE},
				Condition: "resource.management.autoUpgrade == false", ActionType: orgpolicypb.CustomConstraint_DENY, DisplayName: "Disable auto-upgrade"}
			if _, err := c.CreateCustomConstraint(ctx, &orgpolicypb.CreateCustomConstraintRequest{Parent: localOrg, CustomConstraint: cc}); err != nil {
				t.Fatalf("CreateCustomConstraint: %v", err)
			}
			t.Cleanup(func() {
				_ = c.DeleteCustomConstraint(context.Background(), &orgpolicypb.DeleteCustomConstraintRequest{Name: cc.Name})
			})
			cc.DisplayName = "Renamed"
			if _, err := c.UpdateCustomConstraint(ctx, &orgpolicypb.UpdateCustomConstraintRequest{CustomConstraint: cc}); err != nil {
				t.Fatalf("UpdateCustomConstraint: %v", err)
			}
			if got, err := c.GetCustomConstraint(ctx, &orgpolicypb.GetCustomConstraintRequest{Name: cc.Name}); err != nil || got.GetDisplayName() != "Renamed" {
				t.Fatalf("GetCustomConstraint = %v, %v", got, err)
			}
			found := false
			ccit := c.ListCustomConstraints(ctx, &orgpolicypb.ListCustomConstraintsRequest{Parent: localOrg})
			for {
				got, err := ccit.Next()
				if errors.Is(err, iterator.Done) {
					break
				}
				if err != nil {
					t.Fatalf("ListCustomConstraints: %v", err)
				}
				found = found || got.GetName() == cc.Name
			}
			if !found {
				t.Error("ListCustomConstraints lacks the custom constraint")
			}
			if err := c.DeleteCustomConstraint(ctx, &orgpolicypb.DeleteCustomConstraintRequest{Name: cc.Name}); err != nil {
				t.Fatalf("DeleteCustomConstraint: %v", err)
			}
		})
	}
}

func orgPolicyFolder(t *testing.T, h *Harness, fc *resourcemanager.FoldersClient, prefix string) string {
	t.Helper()
	ctx := h.Context()
	op, err := fc.CreateFolder(ctx, &rmpb.CreateFolderRequest{Folder: &rmpb.Folder{Parent: localOrg, DisplayName: folderLabel(h, prefix)}})
	if err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}
	f, err := op.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}
	t.Cleanup(func() { _, _ = fc.DeleteFolder(context.Background(), &rmpb.DeleteFolderRequest{Name: f.GetName()}) })
	return f.GetName()
}

func orgPolicyProject(t *testing.T, h *Harness, pc *resourcemanager.ProjectsClient, parent, prefix string) string {
	t.Helper()
	ctx := h.Context()
	id := v1ID(h, prefix)
	op, err := pc.CreateProject(ctx, &rmpb.CreateProjectRequest{Project: &rmpb.Project{ProjectId: id, Parent: parent}})
	if err == nil {
		_, err = waitLRO(ctx, op)
	}
	if err != nil {
		t.Fatalf("CreateProject %s: %v", id, err)
	}
	t.Cleanup(func() {
		_, _ = pc.DeleteProject(context.Background(), &rmpb.DeleteProjectRequest{Name: "projects/" + id})
	})
	return id
}

// covers: google.iam.admin.v1.IAM/CreateServiceAccountKey, google.iam.admin.v1.IAM/CreateServiceAccount, google.cloud.resourcemanager.v3.Projects/SetIamPolicy
//
// TestOrgPolicyEnforcementIAM (#1890), whatever --iam-enforcement, with
// the official IAM admin and Resource Manager clients: on a project under
// iam.disableServiceAccountKeyCreation keys.create is FAILED_PRECONDITION
// "Key creation is not allowed on this service account."; under
// iam.disableServiceAccountCreation creating an account is refused; under
// iam.allowedPolicyMemberDomains (the local customer, with an allowAll
// rule conditioned on a tag) granting user:...@example.com or allUsers
// is FAILED_PRECONDITION "One or more users named in the policy do not
// belong to a permitted customer.", a member of the customer's domain is
// granted, and a project without the policy grants anyone.
func TestOrgPolicyEnforcementIAM(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	ep := h.Endpoint(EnvResourceManager)
	c := orgPolicyClients(t, h)["grpc"]
	_, _, pc := foldersClientsAt(t, h, rmOptionsAt(ep))
	project := orgPolicyProject(t, h, pc, localOrg, "ope-")
	free := orgPolicyProject(t, h, pc, localOrg, "opn-")
	adm, err := admin.NewIamClient(ctx, rmOptionsAt(ep)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = adm.Close() })
	sa, err := adm.CreateServiceAccount(ctx, &adminpb.CreateServiceAccountRequest{Name: "projects/" + project, AccountId: "opkeys"})
	if err != nil {
		t.Fatalf("CreateServiceAccount: %v", err)
	}
	setOrgPolicy(t, h, c, "projects/"+project, "iam.disableServiceAccountKeyCreation", opEnforce())
	_, err = adm.CreateServiceAccountKey(ctx, &adminpb.CreateServiceAccountKeyRequest{Name: sa.GetName()})
	if st, _ := status.FromError(err); st.Code() != codes.FailedPrecondition || st.Message() != "Key creation is not allowed on this service account." {
		t.Errorf("CreateServiceAccountKey under the constraint = %v, want FAILED_PRECONDITION", err)
	}
	setOrgPolicy(t, h, c, "projects/"+project, "iam.disableServiceAccountCreation", opEnforce())
	if _, err := adm.CreateServiceAccount(ctx, &adminpb.CreateServiceAccountRequest{Name: "projects/" + project, AccountId: "opsecond"}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("CreateServiceAccount under the constraint = %v, want FAILED_PRECONDITION", err)
	}

	setOrgPolicy(t, h, c, "projects/"+project, "iam.allowedPolicyMemberDomains", &orgpolicypb.PolicySpec{Rules: []*orgpolicypb.PolicySpec_PolicyRule{
		opValues("is:C0cloudburrow").Rules[0],
		{Kind: &orgpolicypb.PolicySpec_PolicyRule_AllowAll{AllowAll: true}, Condition: &expr.Expr{Expression: "resource.matchTag('100000000001/sharing', 'open')"}}}})
	grant := func(p, member string) error {
		cur, err := pc.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: "projects/" + p})
		if err != nil {
			t.Fatalf("GetIamPolicy: %v", err)
		}
		cur.Bindings = append(cur.Bindings, &iampb.Binding{Role: "roles/viewer", Members: []string{member}})
		_, err = pc.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: "projects/" + p, Policy: cur})
		return err
	}
	for _, m := range []string{"user:someone@example.com", "allUsers"} {
		err := grant(project, m)
		if st, _ := status.FromError(err); st.Code() != codes.FailedPrecondition || st.Message() != "One or more users named in the policy do not belong to a permitted customer." {
			t.Errorf("granting %s = %v, want FAILED_PRECONDITION", m, err)
		}
	}
	if err := grant(project, "user:alice@cloudburrow.local"); err != nil {
		t.Errorf("granting a member of the customer = %v", err)
	}
	if err := grant(project, "serviceAccount:"+sa.GetEmail()); err != nil {
		t.Errorf("granting a service account = %v", err)
	}
	if err := grant(free, "user:someone@example.com"); err != nil {
		t.Errorf("granting on a project without the policy = %v", err)
	}
}

// covers: storage.buckets.insert, storage.buckets.patch, storage.buckets.setIamPolicy
//
// TestOrgPolicyEnforcementStorage (#1890), with the official Cloud
// Storage client: under gcp.resourceLocations allowing in:eu-locations a
// US bucket is 412 with the documented message, an EU one is created;
// under storage.uniformBucketLevelAccess a bucket without uniform access
// is 412, and turning it off is 412; under storage.publicAccessPrevention
// an allUsers grant is 412 and a private one goes through. A project
// without policies creates what it likes.
func TestOrgPolicyEnforcementStorage(t *testing.T) {
	h := New(t)
	sc := storageClient(t, h)
	ctx := h.Context()
	c := orgPolicyClients(t, h)["grpc"]
	_, _, pc := foldersClientsAt(t, h, rmOptionsAt(h.Endpoint(EnvResourceManager)))
	project := orgPolicyProject(t, h, pc, localOrg, "ops-")
	parent := "projects/" + project
	setOrgPolicy(t, h, c, parent, "gcp.resourceLocations", opValues("in:eu-locations"))
	setOrgPolicy(t, h, c, parent, "storage.uniformBucketLevelAccess", opEnforce())
	setOrgPolicy(t, h, c, parent, "storage.publicAccessPrevention", opEnforce())
	uniform := storage.BucketAttrs{UniformBucketLevelAccess: storage.UniformBucketLevelAccess{Enabled: true}}
	expect412 := func(what string, err error, msg string) {
		t.Helper()
		var ge *googleapi.Error
		if !errors.As(err, &ge) || ge.Code != 412 || (msg != "" && ge.Message != msg) {
			t.Errorf("%s = %v, want 412 %s", what, err, msg)
		}
	}
	us := uniform
	us.Location = "US"
	expect412("a US bucket", sc.Bucket(project+"-us").Create(ctx, project, &us),
		"Location MULTI_REGION:US violates constraint constraints/gcp.resourceLocations on the resource projects/"+project+".")
	expect412("a bucket without uniform access", sc.Bucket(project+"-acl").Create(ctx, project, &storage.BucketAttrs{Location: "EU"}), "")
	eu := uniform
	eu.Location = "europe-west1"
	b := sc.Bucket(project + "-eu")
	if err := b.Create(ctx, project, &eu); err != nil {
		t.Fatalf("an europe-west1 bucket: %v", err)
	}
	t.Cleanup(func() { _ = b.Delete(context.Background()) })
	_, err := b.Update(ctx, storage.BucketAttrsToUpdate{UniformBucketLevelAccess: &storage.UniformBucketLevelAccess{Enabled: false}})
	expect412("turning uniform access off", err, "")
	pol, err := b.IAM().Policy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	pol.Add("allUsers", "roles/storage.objectViewer")
	expect412("an allUsers grant", b.IAM().SetPolicy(ctx, pol), "")
	pol.Remove("allUsers", "roles/storage.objectViewer")
	pol.Add("user:alice@cloudburrow.local", "roles/storage.objectViewer")
	if err := b.IAM().SetPolicy(ctx, pol); err != nil {
		t.Errorf("a private grant: %v", err)
	}
	free := orgPolicyProject(t, h, pc, localOrg, "opt-")
	fb := sc.Bucket(free + "-us")
	if err := fb.Create(ctx, free, &storage.BucketAttrs{Location: "US"}); err != nil {
		t.Fatalf("a bucket in a project without policies: %v", err)
	}
	t.Cleanup(func() { _ = fb.Delete(context.Background()) })
}

// covers: google.cloud.kms.v1.KeyManagementService/CreateKeyRing
//
// TestOrgPolicyEnforcementKMSLocations (#1890): gcp.resourceLocations
// allowing in:us-locations refuses a Cloud KMS key ring in europe-west1,
// FAILED_PRECONDITION with the documented message, and allows us-east1
// and the global location.
func TestOrgPolicyEnforcementKMSLocations(t *testing.T) {
	h := New(t)
	shardEndpoint(h, envLoggingResourceManager, EnvResourceManager)
	ctx := h.Context()
	kc := kmsClients(t, h)["grpc"]
	c := orgPolicyClients(t, h)["grpc"]
	_, _, pc := foldersClientsAt(t, h, rmOptionsAt(h.Endpoint(EnvResourceManager)))
	project := orgPolicyProject(t, h, pc, localOrg, "opk-")
	setOrgPolicy(t, h, c, "projects/"+project, "gcp.resourceLocations", opValues("in:us-locations"))
	want := "Location REGION:europe-west1 violates constraint constraints/gcp.resourceLocations on the resource projects/" + project + "."
	_, err := kc.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: "projects/" + project + "/locations/europe-west1", KeyRingId: "op"})
	if st, _ := status.FromError(err); st.Code() != codes.FailedPrecondition || st.Message() != want {
		t.Errorf("a europe-west1 key ring = %v, want FAILED_PRECONDITION %q", err, want)
	}
	for _, loc := range []string{"us-east1", "global"} {
		if _, err := kc.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: "projects/" + project + "/locations/" + loc, KeyRingId: "op"}); err != nil {
			t.Errorf("a %s key ring: %v", loc, err)
		}
	}
}

// covers: google.cloud.secretmanager.v1.SecretManagerService/CreateSecret
//
// TestOrgPolicyEnforcementSecretLocations (#1890): gcp.resourceLocations
// allowing in:us-locations refuses a Secret Manager secret with a
// user-managed replica in europe-west1, FAILED_PRECONDITION with the
// documented message, and creates one replicated in the US.
func TestOrgPolicyEnforcementSecretLocations(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	smc := secretsClient(t, h)
	c := orgPolicyClients(t, h)["grpc"]
	_, _, pc := foldersClientsAt(t, h, rmOptionsAt(h.Endpoint(EnvResourceManager)))
	project := orgPolicyProject(t, h, pc, localOrg, "opm-")
	setOrgPolicy(t, h, c, "projects/"+project, "gcp.resourceLocations", opValues("in:us-locations"))
	want := "Location REGION:europe-west1 violates constraint constraints/gcp.resourceLocations on the resource projects/" + project + "."
	repl := func(locs ...string) *secretmanagerpb.Replication {
		var rs []*secretmanagerpb.Replication_UserManaged_Replica
		for _, l := range locs {
			rs = append(rs, &secretmanagerpb.Replication_UserManaged_Replica{Location: l})
		}
		return &secretmanagerpb.Replication{Replication: &secretmanagerpb.Replication_UserManaged_{UserManaged: &secretmanagerpb.Replication_UserManaged{Replicas: rs}}}
	}
	_, err := smc.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{Parent: "projects/" + project, SecretId: "op-eu",
		Secret: &secretmanagerpb.Secret{Replication: repl("us-central1", "europe-west1")}})
	if st, _ := status.FromError(err); st.Code() != codes.FailedPrecondition || st.Message() != want {
		t.Errorf("a secret replicated to europe-west1 = %v, want FAILED_PRECONDITION %q", err, want)
	}
	if _, err := smc.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{Parent: "projects/" + project, SecretId: "op-us",
		Secret: &secretmanagerpb.Secret{Replication: repl("us-central1", "us-east1")}}); err != nil {
		t.Errorf("a secret replicated in the US: %v", err)
	}
}

// TestGcloudOrgPolicies (#1890): gcloud org-policies set-policy, describe
// (and --effective), list and delete, over the v2 API on the Resource
// Manager port.
func TestGcloudOrgPolicies(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvResourceManager)
	h.Endpoint(EnvMetadata)
	g := newGcloudSession(t, h)
	project := iamProject(t, h, "opg-")
	file := filepath.Join(t.TempDir(), "policy.yaml")
	body := "name: projects/" + project + "/policies/gcp.resourceLocations\nspec:\n  rules:\n  - values:\n      allowedValues:\n      - in:us-locations\n"
	if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	g.must("org-policies", "set-policy", file)
	t.Cleanup(func() { _, _ = g.run(nil, "org-policies", "delete", "gcp.resourceLocations", "--project="+project) })
	var got struct {
		Name string
		Spec struct {
			Rules []struct {
				Values struct{ AllowedValues []string } `json:"values"`
			}
		}
	}
	out := g.must("org-policies", "describe", "gcp.resourceLocations", "--project="+project, "--format=json")
	if err := json.Unmarshal([]byte(out), &got); err != nil || len(got.Spec.Rules) != 1 || got.Spec.Rules[0].Values.AllowedValues[0] != "in:us-locations" {
		t.Fatalf("describe = %s (%v)", out, err)
	}
	out = g.must("org-policies", "describe", "gcp.resourceLocations", "--project="+project, "--effective", "--format=json")
	if !strings.Contains(out, "in:us-locations") {
		t.Errorf("describe --effective = %s", out)
	}
	out = g.must("org-policies", "list", "--project="+project, "--format=json")
	if !strings.Contains(out, "gcp.resourceLocations") {
		t.Errorf("list = %s", out)
	}
	g.must("org-policies", "delete", "gcp.resourceLocations", "--project="+project)
	if out, err := g.run(nil, "org-policies", "describe", "gcp.resourceLocations", "--project="+project); err == nil {
		t.Errorf("describe after delete succeeded: %s", out)
	}
}
