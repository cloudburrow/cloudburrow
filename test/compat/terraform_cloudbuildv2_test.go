//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"testing"

	cloudbuildv2 "cloud.google.com/go/cloudbuild/apiv2"
	cb2pb "cloud.google.com/go/cloudbuild/apiv2/cloudbuildpb"
	ssmpb "cloud.google.com/go/securesourcemanager/apiv1/securesourcemanagerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestTerraformCloudBuildV2 (#2197): a google_cloudbuildv2_connection to
// a GitLab host at this instance's git server, its tokens and webhook
// secret in Secret Manager, and a google_cloudbuildv2_repository linking
// one of the server's Secure Source Manager repositories, apply through
// `cloudburrow terraform` with egress blocked (it sets
// cloudbuildv2_custom_endpoint to the Run port). The official client reads
// both back, installed; the plan is clean; new annotations and disabled
// apply in place; the plan is clean again; destroy deletes the repository,
// then the connection.
// covers: google.devtools.cloudbuild.v2.RepositoryManager/CreateConnection, google.devtools.cloudbuild.v2.RepositoryManager/GetConnection, google.devtools.cloudbuild.v2.RepositoryManager/UpdateConnection, google.devtools.cloudbuild.v2.RepositoryManager/DeleteConnection, google.devtools.cloudbuild.v2.RepositoryManager/CreateRepository, google.devtools.cloudbuild.v2.RepositoryManager/GetRepository, google.devtools.cloudbuild.v2.RepositoryManager/DeleteRepository
func TestTerraformCloudBuildV2(t *testing.T) {
	h := New(t)
	_ = runClient(t, h) // skips without CLOUDBURROW_TEST_RUN
	m := newTFModule(t)
	ctx := h.Context()
	ssm := ssmClient(t, h)
	rm, err := cloudbuildv2.NewRepositoryManagerClient(ctx, runClientOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rm.Close() })
	parent := runParent(h)
	project := h.Project()
	suffix := strings.TrimPrefix(project, "cb-test-")

	iop, err := ssm.CreateInstance(ctx, &ssmpb.CreateInstanceRequest{Parent: parent, InstanceId: "tfcb2-" + suffix})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	inst, err := iop.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	t.Cleanup(func() {
		if op, err := ssm.DeleteInstance(context.Background(), &ssmpb.DeleteInstanceRequest{Name: inst.GetName(), Force: true}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	rop, err := ssm.CreateRepository(ctx, &ssmpb.CreateRepositoryRequest{Parent: parent, RepositoryId: "tfcb2-" + suffix,
		Repository: &ssmpb.Repository{Instance: inst.GetName()}})
	if err != nil {
		t.Fatalf("CreateRepository: %v", err)
	}
	repo, err := rop.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateRepository: %v", err)
	}
	remote := repo.GetUris().GetGitHttps()
	u, err := url.Parse(remote)
	if err != nil || u.Host == "" {
		t.Fatalf("the repository's git URL %q: %v", remote, err)
	}
	host := u.Scheme + "://" + u.Host
	secret := func(value string) string {
		return "projects/" + project + "/secrets/" + buildSecret(ctx, t, h, project, value) + "/versions/1"
	}
	token, readToken, hook := secret("glpat-api"), secret("glpat-read"), secret("webhook-secret")
	connID := "tfcb2-" + suffix
	connName := parent + "/connections/" + connID
	repoName := connName + "/repositories/app"
	t.Cleanup(func() {
		if op, err := rm.DeleteConnection(context.Background(), &cb2pb.DeleteConnectionRequest{Name: connName}); err == nil {
			_ = op.Wait(context.Background())
		}
	})

	module := func(note string, disabled bool) string {
		return fmt.Sprintf(`
resource "google_cloudbuildv2_connection" "c" {
  project     = %[1]q
  location    = "us-central1"
  name        = %[2]q
  annotations = { note = %[3]q }
  disabled    = %[4]t
  gitlab_config {
    host_uri                      = %[5]q
    webhook_secret_secret_version = %[6]q
    read_authorizer_credential {
      user_token_secret_version = %[7]q
    }
    authorizer_credential {
      user_token_secret_version = %[8]q
    }
  }
}

resource "google_cloudbuildv2_repository" "r" {
  project           = %[1]q
  location          = "us-central1"
  name              = "app"
  parent_connection = google_cloudbuildv2_connection.c.name
  remote_uri        = %[9]q
  annotations       = { repo = "app" }
}

output "read" {
  value = {
    stage      = google_cloudbuildv2_connection.c.installation_state[0].stage
    repository = google_cloudbuildv2_repository.r.id
    remote     = google_cloudbuildv2_repository.r.remote_uri
  }
}
`, project, connID, note, disabled, host, hook, readToken, token, remote)
	}
	check := func(when, note string, disabled bool) {
		t.Helper()
		c, err := rm.GetConnection(ctx, &cb2pb.GetConnectionRequest{Name: connName})
		if err != nil || c.GetAnnotations()["note"] != note || c.GetDisabled() != disabled || c.GetGitlabConfig().GetHostUri() != host ||
			c.GetInstallationState().GetStage() != cb2pb.InstallationState_COMPLETE {
			t.Errorf("%s, the connection = %v, %v; want note=%s disabled=%t, installed", when, c, err, note, disabled)
		}
		r, err := rm.GetRepository(ctx, &cb2pb.GetRepositoryRequest{Name: repoName})
		if err != nil || r.GetRemoteUri() != remote || r.GetAnnotations()["repo"] != "app" {
			t.Errorf("%s, the repository = %v, %v", when, r, err)
		}
		m.must("refresh", "-input=false", "-no-color")
		var got map[string]string
		if err := json.Unmarshal([]byte(m.must("output", "-json", "read")), &got); err != nil {
			t.Fatalf("%s, the outputs: %v", when, err)
		}
		if got["stage"] != "COMPLETE" || got["repository"] != repoName || got["remote"] != remote {
			t.Errorf("%s, Terraform reads %v; want COMPLETE, %s, %s", when, got, repoName, remote)
		}
	}

	m.write(module("first", false))
	m.must("init", "-input=false", "-no-color")
	out := m.must("apply", "-auto-approve", "-input=false", "-no-color")
	if !strings.Contains(out, "2 added, 0 changed, 0 destroyed") {
		t.Errorf("the apply was not as planned:\n%s", lastLines(out, 30))
	}
	check("after apply", "first", false)
	m.planClean()

	m.write(module("second", true))
	out = m.must("apply", "-auto-approve", "-input=false", "-no-color")
	if !strings.Contains(out, "0 added, 1 changed, 0 destroyed") {
		t.Errorf("the change was not applied in place:\n%s", lastLines(out, 30))
	}
	check("after the change", "second", true)
	m.planClean()

	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := rm.GetRepository(ctx, &cb2pb.GetRepositoryRequest{Name: repoName}); status.Code(err) != codes.NotFound {
		t.Errorf("GetRepository after destroy: %v; want NotFound", err)
	}
	if _, err := rm.GetConnection(ctx, &cb2pb.GetConnectionRequest{Name: connName}); status.Code(err) != codes.NotFound {
		t.Errorf("GetConnection after destroy: %v; want NotFound", err)
	}
}
