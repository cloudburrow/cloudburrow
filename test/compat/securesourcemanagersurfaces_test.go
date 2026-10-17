//go:build compat

package compat

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"cloud.google.com/go/iam/apiv1/iampb"
	securesourcemanager "cloud.google.com/go/securesourcemanager/apiv1"
	ssmpb "cloud.google.com/go/securesourcemanager/apiv1/securesourcemanagerpb"
	"github.com/google/uuid"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Secure Source Manager's gcloud, Terraform and IAM surfaces (#1527), on
// the Run port, where #1521 serves it.

// ssmClient is the official client over gRPC on the Run port.
func ssmClient(t *testing.T, h *Harness) *securesourcemanager.Client {
	t.Helper()
	c, err := securesourcemanager.NewClient(h.Context(), runClientOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// ssmInstanceAndRepo makes an instance and a repository on it, deleted
// (force) when the test ends.
func ssmInstanceAndRepo(t *testing.T, h *Harness, c *securesourcemanager.Client, suffix string) (*ssmpb.Instance, *ssmpb.Repository) {
	t.Helper()
	ctx := h.Context()
	iop, err := c.CreateInstance(ctx, &ssmpb.CreateInstanceRequest{Parent: runParent(h), InstanceId: "i-" + suffix})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	in, err := iop.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if op, err := c.DeleteInstance(context.Background(), &ssmpb.DeleteInstanceRequest{Name: in.GetName(), Force: true}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	rop, err := c.CreateRepository(ctx, &ssmpb.CreateRepositoryRequest{Parent: runParent(h), RepositoryId: "r-" + suffix,
		Repository: &ssmpb.Repository{Instance: in.GetName()}})
	if err != nil {
		t.Fatalf("CreateRepository: %v", err)
	}
	r, err := rop.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return in, r
}

// covers: google.cloud.securesourcemanager.v1.SecureSourceManager/GetIamPolicyRepo, google.cloud.securesourcemanager.v1.SecureSourceManager/SetIamPolicyRepo, google.cloud.securesourcemanager.v1.SecureSourceManager/TestIamPermissionsRepo
//
// TestSecureSourceManagerIAM (#1527, ADR-0006): a repository's policy, set
// with the official client's SetIamPolicyRepo over gRPC, reads back with
// GetIamPolicyRepo; a stale etag is ABORTED; TestIamPermissionsRepo answers
// every permission asked about; and an instance's policy, through the
// IAMPolicy mixin at the instance's path with the official client over
// REST (the JSON API under /securesourcemanager/), reads back too. Nothing
// is enforced.
func TestSecureSourceManagerIAM(t *testing.T) {
	h := New(t)
	_ = runClient(t, h) // skips without CLOUDBURROW_TEST_RUN
	ctx := h.Context()
	c := ssmClient(t, h)
	in, r := ssmInstanceAndRepo(t, h, c, strings.ReplaceAll(uuid.NewString(), "-", "")[:8])

	member := "user:dev@example.com"
	pol, err := c.GetIamPolicyRepo(ctx, &iampb.GetIamPolicyRequest{Resource: r.GetName()})
	if err != nil {
		t.Fatalf("GetIamPolicyRepo: %v", err)
	}
	stale := pol.GetEtag()
	pol.Bindings = append(pol.Bindings, &iampb.Binding{Role: "roles/securesourcemanager.repoReader", Members: []string{member}})
	set, err := c.SetIamPolicyRepo(ctx, &iampb.SetIamPolicyRequest{Resource: r.GetName(), Policy: pol})
	if err != nil {
		t.Fatalf("SetIamPolicyRepo: %v", err)
	}
	got, err := c.GetIamPolicyRepo(ctx, &iampb.GetIamPolicyRequest{Resource: r.GetName()})
	if err != nil || len(got.GetBindings()) != 1 || got.GetBindings()[0].GetMembers()[0] != member || string(got.GetEtag()) != string(set.GetEtag()) {
		t.Errorf("GetIamPolicyRepo after the set = %v, %v", got, err)
	}
	if _, err := c.SetIamPolicyRepo(ctx, &iampb.SetIamPolicyRequest{Resource: r.GetName(),
		Policy: &iampb.Policy{Etag: stale}}); status.Code(err) != codes.Aborted {
		t.Errorf("SetIamPolicyRepo with a stale etag: %v; want Aborted", err)
	}
	perms := []string{"securesourcemanager.repos.get", "securesourcemanager.repos.push"}
	if tr, err := c.TestIamPermissionsRepo(ctx, &iampb.TestIamPermissionsRequest{Resource: r.GetName(), Permissions: perms}); err != nil ||
		len(tr.GetPermissions()) != 2 {
		t.Errorf("TestIamPermissionsRepo = %v, %v", tr, err)
	}
	if _, err := c.GetIamPolicyRepo(ctx, &iampb.GetIamPolicyRequest{Resource: runParent(h) + "/repositories/missing"}); status.Code(err) != codes.NotFound {
		t.Errorf("GetIamPolicyRepo of a missing repository: %v; want NotFound", err)
	}

	rc, err := securesourcemanager.NewRESTClient(ctx, option.WithEndpoint("http://"+h.Endpoint(EnvRun)+"/securesourcemanager"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rc.Close() })
	ip, err := rc.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: in.GetName()})
	if err != nil {
		t.Fatalf("the instance's GetIamPolicy over REST: %v", err)
	}
	ip.Bindings = []*iampb.Binding{{Role: "roles/securesourcemanager.instanceAccessor", Members: []string{member}}}
	if _, err := rc.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: in.GetName(), Policy: ip}); err != nil {
		t.Fatalf("the instance's SetIamPolicy over REST: %v", err)
	}
	if got, err := rc.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: in.GetName()}); err != nil || len(got.GetBindings()) != 1 ||
		got.GetBindings()[0].GetRole() != "roles/securesourcemanager.instanceAccessor" {
		t.Errorf("the instance's policy = %v, %v", got, err)
	}
}

// TestGcloudSourceManager (#1527): `gcloud source-manager instances create
// --no-async`, describe, list and delete, and `repos create`, describe,
// list, update --description and delete, through the configuration
// `cloudburrow gcloud-setup` writes (its securesourcemanager override, on
// the Run port), egress blocked; the official client reads what gcloud
// made. Both are GA commands.
func TestGcloudSourceManager(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvRun)
	g := newGcloudSession(t, h)
	ctx := h.Context()
	c := ssmClient(t, h)
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	inst, repo := "gi-"+suffix, "gr-"+suffix
	parent := "projects/" + g.project + "/locations/us-central1"
	t.Cleanup(func() {
		if op, err := c.DeleteInstance(context.Background(), &ssmpb.DeleteInstanceRequest{Name: parent + "/instances/" + inst, Force: true}); err == nil {
			_ = op.Wait(context.Background())
		}
	})

	g.must("source-manager", "instances", "create", inst, "--region=us-central1", "--no-async")
	if out := g.must("source-manager", "instances", "describe", inst, "--region=us-central1", "--format=value(state)"); lastLine(out) != "ACTIVE" {
		t.Errorf("instances describe: state = %q", out)
	}
	if out := g.must("source-manager", "instances", "list", "--region=us-central1", "--format=value(name)"); !strings.Contains(out, inst) {
		t.Errorf("instances list does not list the instance:\n%s", out)
	}

	g.must("source-manager", "repos", "create", repo, "--region=us-central1", "--instance="+inst, "--description=from gcloud")
	if out := g.must("source-manager", "repos", "describe", repo, "--region=us-central1", "--format=value(description)"); lastLine(out) != "from gcloud" {
		t.Errorf("repos describe: description = %q", out)
	}
	if out := g.must("source-manager", "repos", "list", "--region=us-central1", "--instance="+inst, "--format=value(name)"); !strings.Contains(out, repo) {
		t.Errorf("repos list does not list the repository:\n%s", out)
	}
	g.must("source-manager", "repos", "update", repo, "--region=us-central1", "--description=changed")
	r, err := c.GetRepository(ctx, &ssmpb.GetRepositoryRequest{Name: parent + "/repositories/" + repo})
	if err != nil || r.GetDescription() != "changed" || r.GetInstance() != parent+"/instances/"+inst {
		t.Errorf("the repository gcloud updated = %v, %v", r, err)
	}
	g.must("source-manager", "repos", "delete", repo, "--region=us-central1")
	if _, err := c.GetRepository(ctx, &ssmpb.GetRepositoryRequest{Name: parent + "/repositories/" + repo}); status.Code(err) != codes.NotFound {
		t.Errorf("GetRepository after repos delete: %v; want NotFound", err)
	}
	g.must("source-manager", "instances", "delete", inst, "--region=us-central1", "--no-async")
	if _, err := c.GetInstance(ctx, &ssmpb.GetInstanceRequest{Name: parent + "/instances/" + inst}); status.Code(err) != codes.NotFound {
		t.Errorf("GetInstance after instances delete: %v; want NotFound", err)
	}
}

// TestTerraformSecureSourceManager (#1527): a
// google_secure_source_manager_instance and a
// google_secure_source_manager_repository on it, with a
// _repository_iam_member and an _instance_iam_member (stored, never
// enforced: ADR-0006), apply through `cloudburrow terraform` with egress
// blocked (it sets secure_source_manager_custom_endpoint to the Run port);
// the official client reads them back; the plan is clean; a new
// description and member are applied; the plan is clean again; and destroy
// removes them.
func TestTerraformSecureSourceManager(t *testing.T) {
	h := New(t)
	m := newTFModule(t)
	_ = runClient(t, h) // skips without CLOUDBURROW_TEST_RUN
	ctx := h.Context()
	c := ssmClient(t, h)
	suffix := strings.TrimPrefix(h.Project(), "cb-test-")
	inst, repo := runParent(h)+"/instances/tfi-"+suffix, runParent(h)+"/repositories/tfr-"+suffix
	module := func(desc, member string) string {
		return fmt.Sprintf(`
resource "google_secure_source_manager_instance" "i" {
  project         = %[1]q
  location        = "us-central1"
  instance_id     = "tfi-%[2]s"
  deletion_policy = "DELETE"
}

resource "google_secure_source_manager_repository" "r" {
  project         = %[1]q
  location        = "us-central1"
  repository_id   = "tfr-%[2]s"
  instance        = google_secure_source_manager_instance.i.name
  description     = %[3]q
  deletion_policy = "DELETE"
}

resource "google_secure_source_manager_repository_iam_member" "m" {
  project       = %[1]q
  location      = "us-central1"
  repository_id = google_secure_source_manager_repository.r.repository_id
  role          = "roles/securesourcemanager.repoReader"
  member        = %[4]q
}

resource "google_secure_source_manager_instance_iam_member" "m" {
  project     = %[1]q
  location    = "us-central1"
  instance_id = google_secure_source_manager_instance.i.instance_id
  role        = "roles/securesourcemanager.instanceAccessor"
  member      = %[4]q
}
`, h.Project(), suffix, desc, member)
	}
	check := func(when, desc, member string) {
		t.Helper()
		in, err := c.GetInstance(ctx, &ssmpb.GetInstanceRequest{Name: inst})
		if err != nil || in.GetState() != ssmpb.Instance_ACTIVE {
			t.Fatalf("%s, the instance = %v, %v", when, in, err)
		}
		r, err := c.GetRepository(ctx, &ssmpb.GetRepositoryRequest{Name: repo})
		if err != nil || r.GetDescription() != desc || r.GetInstance() != inst {
			t.Fatalf("%s, the repository = %v, %v", when, r, err)
		}
		pol, err := c.GetIamPolicyRepo(ctx, &iampb.GetIamPolicyRequest{Resource: repo})
		if err != nil || len(pol.GetBindings()) != 1 || len(pol.GetBindings()[0].GetMembers()) != 1 || pol.GetBindings()[0].GetMembers()[0] != member {
			t.Errorf("%s, the repository's policy = %v, %v", when, pol, err)
		}
	}

	m.write(module("from terraform", "user:a@example.com"))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	check("after apply", "from terraform", "user:a@example.com")
	m.planClean()

	m.write(module("changed", "user:b@example.com"))
	out := m.must("apply", "-auto-approve", "-input=false", "-no-color")
	// The description changes in place (UpdateRepository); an IAM member's
	// member is replaced, as the provider plans it.
	if !strings.Contains(out, "2 added, 1 changed, 2 destroyed") {
		t.Errorf("the change was not applied as planned:\n%s", lastLines(out, 30))
	}
	check("after the change", "changed", "user:b@example.com")
	m.planClean()

	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := c.GetInstance(ctx, &ssmpb.GetInstanceRequest{Name: inst}); status.Code(err) != codes.NotFound {
		t.Errorf("GetInstance after destroy: %v; want NotFound", err)
	}
	if _, err := c.GetRepository(ctx, &ssmpb.GetRepositoryRequest{Name: repo}); status.Code(err) != codes.NotFound {
		t.Errorf("GetRepository after destroy: %v; want NotFound", err)
	}
}
