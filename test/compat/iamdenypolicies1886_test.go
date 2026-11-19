//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	iam "cloud.google.com/go/iam/apiv2"
	iamv2 "cloud.google.com/go/iam/apiv2/iampb"
	rmpb "cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"
	secretmanager "cloud.google.com/go/secretmanager/apiv1"
	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/api/option"
	pt "google.golang.org/api/policytroubleshooter/v3"
	"google.golang.org/genproto/googleapis/type/expr"
)

// denyParent is the v2 parent of resource's deny policies.
func denyParent(resource string) string {
	return "policies/" + url.PathEscape("cloudresourcemanager.googleapis.com/"+resource) + "/denypolicies"
}

func denyRuleOf(principals, perms []string, mod func(*iamv2.DenyRule)) []*iamv2.PolicyRule {
	r := &iamv2.DenyRule{DeniedPrincipals: principals, DeniedPermissions: perms}
	if mod != nil {
		mod(r)
	}
	return []*iamv2.PolicyRule{{Kind: &iamv2.PolicyRule_DenyRule{DenyRule: r}}}
}

// covers: google.iam.v2.Policies/CreatePolicy, google.iam.v2.Policies/GetPolicy, google.iam.v2.Policies/UpdatePolicy, google.iam.v2.Policies/DeletePolicy, google.iam.v2.Policies/ListPolicies, google.cloud.secretmanager.v1.SecretManagerService/AccessSecretVersion
//
// TestIAMEnforcementDenyPolicies (#1886), with the official
// cloud.google.com/go/iam/apiv2 client over gRPC and Secret Manager's: a
// service account holding Secret Accessor on its project reads the
// payload; a deny policy on the project's folder (inherited) denying it
// secretmanager.googleapis.com/versions.access refuses it with Secret
// Manager's PERMISSION_DENIED, and Policy Troubleshooter v3 says
// CANNOT_ACCESS with the folder's policy DENY_ACCESS_STATE_DENIED; a rule
// denying everyone with the account as an exception principal lets it
// through and refuses the instance's own account; a denialCondition on a
// tag denies only once the tag is bound to the project; deleting the
// policies restores access.
func TestIAMEnforcementDenyPolicies(t *testing.T) {
	h := New(t)
	endpoint := h.Endpoint(EnvSecrets)
	rmEndpoint := h.Endpoint(EnvResourceManager)
	h.Endpoint(EnvMetadata)
	requireIAMEnforcement(t)
	ctx := h.Context()
	fc, _, pc := foldersClientsAt(t, h, rmOptionsAt(rmEndpoint))
	fop, err := fc.CreateFolder(ctx, &rmpb.CreateFolderRequest{Folder: &rmpb.Folder{Parent: localOrg, DisplayName: folderLabel(h, "dny-")}})
	if err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}
	folder, err := fop.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}
	project := v1ID(h, "dny-")
	op, err := pc.CreateProject(ctx, &rmpb.CreateProjectRequest{Project: &rmpb.Project{ProjectId: project, Parent: folder.GetName()}})
	if err == nil {
		_, err = waitLRO(ctx, op)
	}
	if err != nil {
		t.Fatalf("CreateProject %s: %v", project, err)
	}
	t.Cleanup(func() {
		_, _ = pc.DeleteProject(context.Background(), &rmpb.DeleteProjectRequest{Name: "projects/" + project})
		_, _ = fc.DeleteFolder(context.Background(), &rmpb.DeleteFolderRequest{Name: folder.GetName()})
	})
	owner, err := secretmanager.NewClient(ctx, grpcAs(endpoint, "")...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Close() })
	secret := "projects/" + project + "/secrets/denied"
	if _, err := owner.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{Parent: "projects/" + project, SecretId: "denied",
		Secret: &secretmanagerpb.Secret{Replication: &secretmanagerpb.Replication{Replication: &secretmanagerpb.Replication_Automatic_{
			Automatic: &secretmanagerpb.Replication_Automatic{}}}}}); err != nil {
		t.Fatalf("CreateSecret: %v", err)
	}
	if _, err := owner.AddSecretVersion(ctx, &secretmanagerpb.AddSecretVersionRequest{Parent: secret,
		Payload: &secretmanagerpb.SecretPayload{Data: []byte("x")}}); err != nil {
		t.Fatalf("AddSecretVersion: %v", err)
	}
	p := newIAMPrincipal(t, h, project, "dny", "roles/secretmanager.secretAccessor")
	as, err := secretmanager.NewClient(ctx, grpcAs(endpoint, p.token)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = as.Close() })
	version := secret + "/versions/1"
	accessAs := func(c *secretmanager.Client) func() error {
		return func() error {
			_, err := c.AccessSecretVersion(ctx, &secretmanagerpb.AccessSecretVersionRequest{Name: version})
			return err
		}
	}
	access := accessAs(as)
	deniedMsg := "Permission 'secretmanager.versions.access' denied for resource '" + version + "' (or it may not exist)."
	waitGranted(t, "AccessSecretVersion with Secret Accessor", access)

	pol, err := iam.NewPoliciesClient(ctx, rmOptionsAt(rmEndpoint)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pol.Close() })
	saID := "principal://iam.googleapis.com/projects/-/serviceAccounts/" + p.email
	create := func(resource, id string, rules []*iamv2.PolicyRule) *iamv2.Policy {
		t.Helper()
		op, err := pol.CreatePolicy(ctx, &iamv2.CreatePolicyRequest{Parent: denyParent(resource), PolicyId: id,
			Policy: &iamv2.Policy{DisplayName: "deny " + id, Rules: rules}})
		if err != nil {
			t.Fatalf("CreatePolicy %s on %s: %v", id, resource, err)
		}
		got, err := op.Wait(ctx)
		if err != nil {
			t.Fatalf("CreatePolicy %s: %v", id, err)
		}
		t.Cleanup(func() {
			if op, err := pol.DeletePolicy(context.Background(), &iamv2.DeletePolicyRequest{Name: got.GetName()}); err == nil {
				_, _ = op.Wait(context.Background())
			}
		})
		return got
	}
	remove := func(p *iamv2.Policy) {
		t.Helper()
		op, err := pol.DeletePolicy(ctx, &iamv2.DeletePolicyRequest{Name: p.GetName(), Etag: p.GetEtag()})
		if err == nil {
			_, err = op.Wait(ctx)
		}
		if err != nil {
			t.Fatalf("DeletePolicy %s: %v", p.GetName(), err)
		}
	}

	// Inherited from the folder.
	onFolder := create(folder.GetName(), "no-secret-reads", denyRuleOf([]string{saID}, []string{"secretmanager.googleapis.com/versions.access"}, nil))
	if !strings.HasPrefix(onFolder.GetName(), "policies/cloudresourcemanager.googleapis.com%2F"+strings.ReplaceAll(folder.GetName(), "/", "%2F")+"/denypolicies/") {
		t.Errorf("policy name = %s", onFolder.GetName())
	}
	wantDenied(t, "AccessSecretVersion denied on the folder", access(), deniedMsg, "secretmanager.versions.access", true)
	base := "http://" + strings.TrimPrefix(rmEndpoint, "http://") + "/"
	pts, err := pt.NewService(ctx, option.WithEndpoint(base), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	resp, err := pts.Iam.Troubleshoot(&pt.GoogleCloudPolicytroubleshooterIamV3TroubleshootIamPolicyRequest{
		AccessTuple: &pt.GoogleCloudPolicytroubleshooterIamV3AccessTuple{Principal: p.email,
			FullResourceName: "//secretmanager.googleapis.com/" + secret, Permission: "secretmanager.versions.access"}}).Context(ctx).Do()
	if err != nil {
		t.Fatalf("iam:troubleshoot: %v", err)
	}
	de := resp.DenyPolicyExplanation
	if resp.OverallAccessState != "CANNOT_ACCESS" || resp.AllowPolicyExplanation.AllowAccessState != "ALLOW_ACCESS_STATE_GRANTED" ||
		de == nil || de.DenyAccessState != "DENY_ACCESS_STATE_DENIED" || !de.PermissionDeniable {
		t.Fatalf("iam:troubleshoot = %s, allow %s, deny %+v", resp.OverallAccessState, resp.AllowPolicyExplanation.AllowAccessState, de)
	}
	foundFolder := false
	for _, r := range de.ExplainedResources {
		if r.FullResourceName == "//cloudresourcemanager.googleapis.com/"+folder.GetName() && r.DenyAccessState == "DENY_ACCESS_STATE_DENIED" {
			for _, ep := range r.ExplainedPolicies {
				for _, re := range ep.RuleExplanations {
					foundFolder = foundFolder || re.DeniedPrincipals[saID].Membership == "MEMBERSHIP_MATCHED" &&
						re.DeniedPermissions["secretmanager.googleapis.com/versions.access"].PermissionMatchingState == "PERMISSION_PATTERN_MATCHED"
				}
			}
		}
	}
	if !foundFolder {
		t.Errorf("iam:troubleshoot did not explain the folder's deny rule: %+v", de.ExplainedResources)
	}
	remove(onFolder)
	waitGranted(t, "AccessSecretVersion after the folder's policy is deleted", access)

	// Everyone denied but the account, on the project by its ID.
	everyone := create("projects/"+project, "everyone-but", denyRuleOf([]string{"principalSet://goog/public:all"},
		[]string{"secretmanager.googleapis.com/versions.*"}, func(r *iamv2.DenyRule) { r.ExceptionPrincipals = []string{saID} }))
	if err := access(); err != nil {
		t.Errorf("AccessSecretVersion as the exception principal: %v", err)
	}
	wantDenied(t, "AccessSecretVersion as the instance's own account, denied to everyone", accessAs(owner)(), deniedMsg, "secretmanager.versions.access", true)
	// Get and list read it back; list without rules.
	got, err := pol.GetPolicy(ctx, &iamv2.GetPolicyRequest{Name: everyone.GetName()})
	if err != nil || len(got.GetRules()) != 1 || got.GetKind() != "DenyPolicy" {
		t.Fatalf("GetPolicy = %v, %v", got, err)
	}
	it := pol.ListPolicies(ctx, &iamv2.ListPoliciesRequest{Parent: denyParent("projects/" + project)})
	listed, err := it.Next()
	if err != nil || listed.GetName() != everyone.GetName() || len(listed.GetRules()) != 0 {
		t.Fatalf("ListPolicies = %v, %v", listed, err)
	}
	// Update it to a tag condition: not denied until the tag is bound.
	tags := newTagClients(t, h)
	kop, err := tags.keys.CreateTagKey(ctx, &rmpb.CreateTagKeyRequest{TagKey: &rmpb.TagKey{Parent: localOrg, ShortName: folderLabel(h, "dny")}})
	if err != nil {
		t.Fatal(err)
	}
	key, err := kop.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	vop, err := tags.values.CreateTagValue(ctx, &rmpb.CreateTagValueRequest{TagValue: &rmpb.TagValue{Parent: key.GetName(), ShortName: "locked"}})
	if err != nil {
		t.Fatal(err)
	}
	value, err := vop.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got.Rules = denyRuleOf([]string{saID}, []string{"secretmanager.googleapis.com/versions.access"}, func(r *iamv2.DenyRule) {
		r.DenialCondition = &expr.Expr{Title: "locked", Expression: `resource.matchTag("` + key.GetNamespacedName() + `", "locked")`}
	})
	uop, err := pol.UpdatePolicy(ctx, &iamv2.UpdatePolicyRequest{Policy: got})
	if err != nil {
		t.Fatalf("UpdatePolicy: %v", err)
	}
	updated, err := uop.Wait(ctx)
	if err != nil {
		t.Fatalf("UpdatePolicy: %v", err)
	}
	waitGranted(t, "AccessSecretVersion before the tag is bound", access)
	if err := accessAs(owner)(); err != nil {
		t.Errorf("AccessSecretVersion as the instance's account once the rule names only the account: %v", err)
	}
	bop, err := tags.bindings.CreateTagBinding(ctx, &rmpb.CreateTagBindingRequest{TagBinding: &rmpb.TagBinding{
		Parent: "//cloudresourcemanager.googleapis.com/projects/" + project, TagValue: value.GetName()}})
	if err == nil {
		_, err = bop.Wait(ctx)
	}
	if err != nil {
		t.Fatalf("CreateTagBinding: %v", err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = tags.bindings.DeleteTagBinding(bg, &rmpb.DeleteTagBindingRequest{Name: "tagBindings/" +
			url.PathEscape("//cloudresourcemanager.googleapis.com/projects/"+project) + "/" + value.GetName()})
		if op, err := tags.values.DeleteTagValue(bg, &rmpb.DeleteTagValueRequest{Name: value.GetName()}); err == nil {
			_, _ = op.Wait(bg)
		}
		if op, err := tags.keys.DeleteTagKey(bg, &rmpb.DeleteTagKeyRequest{Name: key.GetName()}); err == nil {
			_, _ = op.Wait(bg)
		}
	})
	wantDenied(t, "AccessSecretVersion once the project carries the tag", access(), deniedMsg, "secretmanager.versions.access", true)
	remove(updated)
	waitGranted(t, "AccessSecretVersion after the project's policy is deleted", access)
}

// TestGcloudIAMPolicies (#1886): gcloud iam policies create, get, list,
// update and delete a deny policy on a project, through gcloud-setup's iam
// override alone, as Google's "Deny access" page runs them.
func TestGcloudIAMPolicies(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvResourceManager)
	h.Endpoint(EnvMetadata)
	g := newGcloudSession(t, h)
	project := iamProject(t, h, "dnyg-")
	attach := "cloudresourcemanager.googleapis.com/projects/" + project
	dir := t.TempDir()
	file := filepath.Join(dir, "policy.json")
	write := func(display string) {
		t.Helper()
		body := `{"displayName": "` + display + `", "rules": [{"denyRule": {"deniedPrincipals": ["principalSet://goog/public:all"], ` +
			`"deniedPermissions": ["cloudresourcemanager.googleapis.com/projects.delete"]}}]}`
		if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("My deny policy.")
	g.must("iam", "policies", "create", "my-policy", "--attachment-point="+attach, "--kind=denypolicies", "--policy-file="+file)
	t.Cleanup(func() {
		_, _ = g.run(nil, "iam", "policies", "delete", "my-policy", "--attachment-point="+attach, "--kind=denypolicies")
	})
	var got struct {
		Name, Kind, DisplayName, Etag string
		Rules                         []struct {
			DenyRule struct{ DeniedPermissions []string } `json:"denyRule"`
		}
	}
	out := g.must("iam", "policies", "get", "my-policy", "--attachment-point="+attach, "--kind=denypolicies", "--format=json")
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("get: %v\n%s", err, out)
	}
	if got.Kind != "DenyPolicy" || got.DisplayName != "My deny policy." || len(got.Rules) != 1 ||
		got.Rules[0].DenyRule.DeniedPermissions[0] != "cloudresourcemanager.googleapis.com/projects.delete" ||
		!strings.HasSuffix(got.Name, "/denypolicies/my-policy") {
		t.Fatalf("get = %+v", got)
	}
	out = g.must("iam", "policies", "list", "--attachment-point="+attach, "--kind=denypolicies", "--format=json")
	if !strings.Contains(out, "/denypolicies/my-policy") {
		t.Errorf("list = %q", out)
	}
	write("Renamed.")
	g.must("iam", "policies", "update", "my-policy", "--attachment-point="+attach, "--kind=denypolicies", "--policy-file="+file)
	out = g.must("iam", "policies", "get", "my-policy", "--attachment-point="+attach, "--kind=denypolicies", "--format=value(displayName)")
	if strings.TrimSpace(out) != "Renamed." {
		t.Errorf("after update, displayName = %q", out)
	}
	g.must("iam", "policies", "delete", "my-policy", "--attachment-point="+attach, "--kind=denypolicies")
	if out, err := g.run(nil, "iam", "policies", "get", "my-policy", "--attachment-point="+attach, "--kind=denypolicies"); err == nil {
		t.Errorf("get after delete succeeded: %s", out)
	}
}
