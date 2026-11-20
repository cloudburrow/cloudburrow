//go:build compat

package compat

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	datatransfer "cloud.google.com/go/bigquery/datatransfer/apiv1"
	"cloud.google.com/go/bigquery/datatransfer/apiv1/datatransferpb"
	"cloud.google.com/go/iam/apiv1/iampb"
	resourcemanager "cloud.google.com/go/resourcemanager/apiv3"
	rmpb "cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"
	secretmanager "cloud.google.com/go/secretmanager/apiv1"
	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"cloud.google.com/go/securesourcemanager/apiv1/securesourcemanagerpb"
	"golang.org/x/oauth2"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// covers: google.cloud.bigquery.datatransfer.v1.DataTransferService/ListTransferConfigs, google.cloud.bigquery.datatransfer.v1.DataTransferService/ListDataSources, google.cloud.bigquery.datatransfer.v1.DataTransferService/DeleteTransferConfig, google.cloud.bigquery.datatransfer.v1.DataTransferService/StartManualTransferRuns, google.cloud.bigquery.datatransfer.v1.DataTransferService/EnrollDataSources
//
// TestIAMEnforcementDataTransfer (#1879, round 12), with the official
// BigQuery Data Transfer client over gRPC and REST and a minted token:
// without a role, ListTransferConfigs (bigquery.transfers.get on the
// parent) is refused over both; as a BigQuery User it lists, and
// DeleteTransferConfig and StartManualTransferRuns
// (bigquery.transfers.update) are refused until BigQuery Admin is granted,
// when they reach the service (NOT_FOUND of a config that does not exist).
// EnrollDataSources needs resourcemanager.projects.update, which BigQuery
// Admin lacks. The instance's own account is never refused.
func TestIAMEnforcementDataTransfer(t *testing.T) {
	h := New(t)
	addr := h.Endpoint(EnvDataTransfer)
	h.Endpoint(EnvResourceManager)
	h.Endpoint(EnvMetadata)
	requireIAMEnforcement(t)
	ctx := h.Context()
	project := iamProject(t, h, "iodt12-")
	parent := "projects/" + project + "/locations/us"
	config := parent + "/transferConfigs/absent"
	p := newIAMPrincipal(t, h, project, "iodt12")
	owner, err := datatransfer.NewClient(ctx, grpcAs(addr, "")...)
	if err != nil {
		t.Fatal(err)
	}
	as, err := datatransfer.NewClient(ctx, grpcAs(addr, p.token)...)
	if err != nil {
		t.Fatal(err)
	}
	// The REST client adds /v1 itself: an endpoint with no trailing slash.
	asREST, err := datatransfer.NewRESTClient(ctx, option.WithEndpoint("http://"+stripScheme(addr)),
		option.WithHTTPClient(&http.Client{Transport: &oauth2.Transport{Source: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: p.token})}}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Close(); _ = as.Close(); _ = asREST.Close() })
	list := func(c *datatransfer.Client) error {
		_, err := c.ListTransferConfigs(ctx, &datatransferpb.ListTransferConfigsRequest{Parent: parent}).Next()
		return first(err)
	}
	del := func(c *datatransfer.Client) error {
		return c.DeleteTransferConfig(ctx, &datatransferpb.DeleteTransferConfigRequest{Name: config})
	}
	start := func(c *datatransfer.Client) error {
		_, err := c.StartManualTransferRuns(ctx, &datatransferpb.StartManualTransferRunsRequest{Parent: config})
		return err
	}
	enroll := func(c *datatransfer.Client) error {
		return c.EnrollDataSources(ctx, &datatransferpb.EnrollDataSourcesRequest{Name: "projects/" + project,
			DataSourceIds: []string{"scheduled_query"}})
	}

	if err := list(owner); err != nil {
		t.Fatalf("the instance's own account: ListTransferConfigs = %v", err)
	}
	if err := del(owner); status.Code(err) != codes.NotFound {
		t.Errorf("the instance's own account: DeleteTransferConfig of an absent config = %v; want NOT_FOUND", err)
	}
	if err := enroll(owner); err != nil {
		t.Errorf("the instance's own account: EnrollDataSources = %v", err)
	}
	wantDenied(t, "ListTransferConfigs without a role", list(as), newerDenied("bigquery.transfers.get", parent), "bigquery.transfers.get", true)
	if err := list(asREST); status.Code(err) != codes.PermissionDenied {
		t.Errorf("ListTransferConfigs over REST without a role = %v; want PERMISSION_DENIED", err)
	}

	p.grant("roles/bigquery.user")
	t.Cleanup(func() { p.revoke("roles/bigquery.user") })
	waitGranted(t, "ListTransferConfigs as a BigQuery User", func() error { return list(as) })
	if err := list(asREST); err != nil {
		t.Errorf("ListTransferConfigs over REST as a BigQuery User: %v", err)
	}
	if _, err := as.ListDataSources(ctx, &datatransferpb.ListDataSourcesRequest{Parent: parent}).Next(); first(err) != nil {
		t.Errorf("ListDataSources as a BigQuery User: %v", err)
	}
	wantDenied(t, "DeleteTransferConfig as a BigQuery User", del(as), newerDenied("bigquery.transfers.update", config),
		"bigquery.transfers.update", true)
	wantDenied(t, "StartManualTransferRuns as a BigQuery User", start(as), newerDenied("bigquery.transfers.update", config),
		"bigquery.transfers.update", true)

	p.grant("roles/bigquery.admin")
	t.Cleanup(func() { p.revoke("roles/bigquery.admin") })
	waitGranted(t, "DeleteTransferConfig as a BigQuery Admin", func() error {
		if err := del(as); status.Code(err) != codes.NotFound {
			return err
		}
		return nil
	})
	wantPast(t, "StartManualTransferRuns as a BigQuery Admin", start(as))
	wantDenied(t, "EnrollDataSources as a BigQuery Admin", enroll(as), newerDenied("resourcemanager.projects.update", "projects/"+project),
		"resourcemanager.projects.update", true)
}

// covers: google.cloud.resourcemanager.v3.TagBindings/CreateTagBinding, google.cloud.resourcemanager.v3.TagBindings/ListTagBindings, google.cloud.resourcemanager.v3.TagBindings/DeleteTagBinding, google.cloud.resourcemanager.v3.TagValues/SetIamPolicy
//
// TestIAMEnforcementTagBindingsOnOtherResources (#1879, round 12), with
// the official Resource Manager and Secret Manager clients over gRPC: a
// caller who may use a tag value (Tag User on the value alone) is refused
// a binding on a secret, which needs secretmanager.secrets.createTagBinding
// on the secret, in Secret Manager's words, and listing the secret's
// bindings (secretmanager.secrets.listTagBindings); Tag User on the
// project, which holds both, lets it bind, list and detach. The instance's
// own account is never refused.
func TestIAMEnforcementTagBindingsOnOtherResources(t *testing.T) {
	h := New(t)
	rmEndpoint := h.Endpoint(EnvResourceManager)
	smEndpoint := h.Endpoint(EnvSecrets)
	h.Endpoint(EnvMetadata)
	requireIAMEnforcement(t)
	ctx := h.Context()
	project := iamProject(t, h, "iotb12-")
	name := "projects/" + project
	secret := name + "/secrets/tagged"
	full := "//secretmanager.googleapis.com/" + secret
	sm, err := secretmanager.NewClient(ctx, grpcAs(smEndpoint, "")...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sm.Close() })
	if _, err := sm.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{Parent: name, SecretId: "tagged",
		Secret: &secretmanagerpb.Secret{Replication: &secretmanagerpb.Replication{Replication: &secretmanagerpb.Replication_Automatic_{
			Automatic: &secretmanagerpb.Replication_Automatic{}}}}}); err != nil {
		t.Fatalf("CreateSecret: %v", err)
	}
	keys, err := resourcemanager.NewTagKeysClient(ctx, rmOptionsAt(rmEndpoint)...)
	if err != nil {
		t.Fatal(err)
	}
	values, err := resourcemanager.NewTagValuesClient(ctx, rmOptionsAt(rmEndpoint)...)
	if err != nil {
		t.Fatal(err)
	}
	ownerBindings, err := resourcemanager.NewTagBindingsClient(ctx, rmOptionsAt(rmEndpoint)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = keys.Close(); _ = values.Close(); _ = ownerBindings.Close() })
	kop, err := keys.CreateTagKey(ctx, &rmpb.CreateTagKeyRequest{TagKey: &rmpb.TagKey{Parent: name, ShortName: "tier12"}})
	if err != nil {
		t.Fatal(err)
	}
	key, err := kop.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	vop, err := values.CreateTagValue(ctx, &rmpb.CreateTagValueRequest{TagValue: &rmpb.TagValue{Parent: key.GetName(), ShortName: "gold"}})
	if err != nil {
		t.Fatal(err)
	}
	value, err := vop.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	p := newIAMPrincipal(t, h, project, "iotb12")
	t.Cleanup(func() {
		bg := context.Background()
		if op, err := values.DeleteTagValue(bg, &rmpb.DeleteTagValueRequest{Name: value.GetName()}); err == nil {
			_, _ = op.Wait(bg)
		}
		if op, err := keys.DeleteTagKey(bg, &rmpb.DeleteTagKeyRequest{Name: key.GetName()}); err == nil {
			_, _ = op.Wait(bg)
		}
	})
	// Tag User on the value alone.
	pol, err := values.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: value.GetName()})
	if err != nil {
		t.Fatal(err)
	}
	pol.Bindings = append(pol.Bindings, &iampb.Binding{Role: "roles/resourcemanager.tagUser", Members: []string{"serviceAccount:" + p.email}})
	if _, err := values.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: value.GetName(), Policy: pol}); err != nil {
		t.Fatalf("SetIamPolicy on the tag value: %v", err)
	}
	as, err := resourcemanager.NewTagBindingsClient(ctx, grpcAs(rmEndpoint, p.token)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = as.Close() })
	bind := func(c *resourcemanager.TagBindingsClient) error {
		op, err := c.CreateTagBinding(ctx, &rmpb.CreateTagBindingRequest{TagBinding: &rmpb.TagBinding{Parent: full, TagValue: value.GetName()}})
		if err != nil {
			return err
		}
		_, err = op.Wait(ctx)
		return err
	}
	list := func(c *resourcemanager.TagBindingsClient) error {
		_, err := c.ListTagBindings(ctx, &rmpb.ListTagBindingsRequest{Parent: full}).Next()
		return first(err)
	}
	binding := "tagBindings/" + url.PathEscape(full) + "/" + value.GetName()
	unbind := func(c *resourcemanager.TagBindingsClient) error {
		op, err := c.DeleteTagBinding(ctx, &rmpb.DeleteTagBindingRequest{Name: binding})
		if err != nil {
			return err
		}
		return op.Wait(ctx)
	}
	smDenied := func(permission string) string {
		return "Permission '" + permission + "' denied for resource '" + secret + "' (or it may not exist)."
	}

	wantDenied(t, "CreateTagBinding on a secret as a Tag User of the value", bind(as),
		smDenied("secretmanager.secrets.createTagBinding"), "secretmanager.secrets.createTagBinding", true)
	wantDenied(t, "ListTagBindings of a secret as a Tag User of the value", list(as),
		smDenied("secretmanager.secrets.listTagBindings"), "secretmanager.secrets.listTagBindings", true)
	if err := bind(ownerBindings); err != nil {
		t.Fatalf("the instance's own account: CreateTagBinding on the secret = %v", err)
	}
	wantDenied(t, "DeleteTagBinding of a secret as a Tag User of the value", unbind(as),
		smDenied("secretmanager.secrets.deleteTagBinding"), "secretmanager.secrets.deleteTagBinding", true)
	if err := unbind(ownerBindings); err != nil {
		t.Fatalf("the instance's own account: DeleteTagBinding = %v", err)
	}

	p.grant("roles/resourcemanager.tagUser")
	t.Cleanup(func() { p.revoke("roles/resourcemanager.tagUser") })
	waitGranted(t, "CreateTagBinding on a secret as a Tag User of the project", func() error { return bind(as) })
	if err := list(as); err != nil {
		t.Errorf("ListTagBindings as a Tag User of the project: %v", err)
	}
	if err := unbind(as); err != nil {
		t.Errorf("DeleteTagBinding as a Tag User of the project: %v", err)
	}
}

// covers: google.cloud.securesourcemanager.v1.SecureSourceManager/CreateRepository, google.cloud.securesourcemanager.v1.SecureSourceManager/SetIamPolicyRepo
//
// TestIAMEnforcementSourceManagerGit (#1879, round 12), with git over
// HTTPS and a minted token: a clone (git ls-remote) and a push to a
// repository are refused without a role; Instance Accessor on the project
// and Repo Reader on the repository's own policy let the clone through
// (securesourcemanager.instances.access, securesourcemanager.repositories.fetch)
// but not the push, which Repo Writer grants (.push), as "Access control
// with IAM" asks for those roles to pull and push. The instance's own
// account is never refused.
func TestIAMEnforcementSourceManagerGit(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvRun)
	h.Endpoint(EnvResourceManager)
	h.Endpoint(EnvMetadata)
	requireIAMEnforcement(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required")
	}
	ctx := h.Context()
	project := iamProject(t, h, "iogit12-")
	parent := "projects/" + project + "/locations/us-central1"
	p := newIAMPrincipal(t, h, project, "iogit12")
	owner := ssmClient(t, h)
	iop, err := owner.CreateInstance(ctx, &securesourcemanagerpb.CreateInstanceRequest{Parent: parent, InstanceId: "gi12"})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	inst, err := iop.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if op, err := owner.DeleteInstance(context.Background(), &securesourcemanagerpb.DeleteInstanceRequest{Name: inst.GetName(), Force: true}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	rop, err := owner.CreateRepository(ctx, &securesourcemanagerpb.CreateRepositoryRequest{Parent: parent, RepositoryId: "gr12",
		Repository: &securesourcemanagerpb.Repository{Instance: inst.GetName()}})
	if err != nil {
		t.Fatalf("CreateRepository: %v", err)
	}
	repo, err := rop.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	url := repo.GetUris().GetGitHttps()
	work := t.TempDir()
	git := func(token string, args ...string) error {
		base := []string{"-C", work, "-c", "user.name=t", "-c", "user.email=t@example.com"}
		if token != "" {
			base = append(base, "-c", "http.extraHeader=Authorization: Bearer "+token)
		}
		cmd := exec.Command("git", append(base, args...)...)
		cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
		out, err := cmd.CombinedOutput()
		if err != nil {
			return &gitError{out: strings.TrimSpace(string(out))}
		}
		return nil
	}
	if err := git("", "init", "--quiet", "-b", "main"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := git("", "add", "."); err != nil {
		t.Fatal(err)
	}
	if err := git("", "commit", "--quiet", "-m", "first"); err != nil {
		t.Fatal(err)
	}
	fetch := func() error { return git(p.token, "ls-remote", url) }
	push := func() error { return git(p.token, "push", "--quiet", url, "main") }
	wantRefused := func(what string, err error) {
		t.Helper()
		ge, ok := err.(*gitError)
		if !ok || !strings.Contains(ge.out, "403") {
			t.Fatalf("%s = %v; want a 403", what, err)
		}
	}
	getPol := func(ctx context.Context, r *iampb.GetIamPolicyRequest) (*iampb.Policy, error) {
		return owner.GetIamPolicyRepo(ctx, r)
	}
	setPol := func(ctx context.Context, r *iampb.SetIamPolicyRequest) (*iampb.Policy, error) {
		return owner.SetIamPolicyRepo(ctx, r)
	}
	if err := git("", "ls-remote", url); err != nil {
		t.Fatalf("the instance's own account: ls-remote = %v", err)
	}
	wantRefused("ls-remote without a role", fetch())
	wantRefused("push without a role", push())

	p.grant("roles/securesourcemanager.instanceAccessor")
	t.Cleanup(func() { p.revoke("roles/securesourcemanager.instanceAccessor") })
	wantRefused("ls-remote as an Instance Accessor alone", fetch())
	grantOwn(t, ctx, repo.GetName(), "roles/securesourcemanager.repoReader", "serviceAccount:"+p.email, getPol, setPol)
	waitGranted(t, "ls-remote as the repository's reader", fetch)
	wantRefused("push as the repository's reader", push())
	grantOwn(t, ctx, repo.GetName(), "roles/securesourcemanager.repoWriter", "serviceAccount:"+p.email, getPol, setPol)
	waitGranted(t, "push as the repository's writer", push)
}

// gitError is a failed git command's output.
type gitError struct{ out string }

func (e *gitError) Error() string { return e.out }
