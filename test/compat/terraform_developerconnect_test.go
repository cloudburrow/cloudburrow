//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	developerconnect "cloud.google.com/go/developerconnect/apiv1"
	dcpb "cloud.google.com/go/developerconnect/apiv1/developerconnectpb"
	ssmpb "cloud.google.com/go/securesourcemanager/apiv1/securesourcemanagerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestTerraformDeveloperConnect (#1304): a Developer Connect connection to
// this instance's Secure Source Manager, made with the official client (the
// google provider 8.4.0 has no secure_source_manager_instance_config block,
// so it cannot create one), is imported as a
// google_developer_connect_connection whose labels and annotations the
// import applies in place, beside a google_developer_connect_git_repository_link
// to one of the instance's repositories, through `cloudburrow terraform` with
// egress blocked (it sets developer_connect_custom_endpoint to the Run port).
// The official client reads both back; the plan is clean; new labels,
// annotations and disabled apply in place; the plan is clean again; and
// destroy deletes the link, then the connection.
// covers: google.cloud.developerconnect.v1.DeveloperConnect/GetConnection, google.cloud.developerconnect.v1.DeveloperConnect/UpdateConnection, google.cloud.developerconnect.v1.DeveloperConnect/DeleteConnection, google.cloud.developerconnect.v1.DeveloperConnect/CreateGitRepositoryLink, google.cloud.developerconnect.v1.DeveloperConnect/GetGitRepositoryLink, google.cloud.developerconnect.v1.DeveloperConnect/DeleteGitRepositoryLink
func TestTerraformDeveloperConnect(t *testing.T) {
	h := New(t)
	_ = runClient(t, h) // skips without CLOUDBURROW_TEST_RUN
	m := newTFModule(t)
	ctx := h.Context()
	ssm := ssmClient(t, h)
	dc, err := developerconnect.NewClient(ctx, runClientOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dc.Close() })
	parent := runParent(h)
	suffix := strings.TrimPrefix(h.Project(), "cb-test-")

	iop, err := ssm.CreateInstance(ctx, &ssmpb.CreateInstanceRequest{Parent: parent, InstanceId: "tfdc-" + suffix})
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
	rop, err := ssm.CreateRepository(ctx, &ssmpb.CreateRepositoryRequest{Parent: parent, RepositoryId: "tfdc-" + suffix,
		Repository: &ssmpb.Repository{Instance: inst.GetName()}})
	if err != nil {
		t.Fatalf("CreateRepository: %v", err)
	}
	repo, err := rop.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateRepository: %v", err)
	}
	t.Cleanup(func() {
		if op, err := ssm.DeleteRepository(context.Background(), &ssmpb.DeleteRepositoryRequest{Name: repo.GetName()}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	connID := "tfdc-" + suffix
	cop, err := dc.CreateConnection(ctx, &dcpb.CreateConnectionRequest{Parent: parent, ConnectionId: connID,
		Connection: &dcpb.Connection{ConnectionConfig: &dcpb.Connection_SecureSourceManagerInstanceConfig{
			SecureSourceManagerInstanceConfig: &dcpb.SecureSourceManagerInstanceConfig{Instance: inst.GetName()}}}})
	if err != nil {
		t.Fatalf("CreateConnection: %v", err)
	}
	conn, err := cop.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateConnection: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		if op, err := dc.DeleteGitRepositoryLink(ctx, &dcpb.DeleteGitRepositoryLinkRequest{Name: conn.GetName() + "/gitRepositoryLinks/tf-link"}); err == nil {
			_ = op.Wait(ctx)
		}
		if op, err := dc.DeleteConnection(ctx, &dcpb.DeleteConnectionRequest{Name: conn.GetName()}); err == nil {
			_ = op.Wait(ctx)
		}
	})
	cloneURI := repo.GetUris().GetGitHttps()
	if cloneURI == "" {
		t.Fatalf("the repository has no git URL: %v", repo)
	}

	module := func(team, note string, disabled bool) string {
		return fmt.Sprintf(`
import {
  to = google_developer_connect_connection.c
  id = %[1]q
}

resource "google_developer_connect_connection" "c" {
  project       = %[2]q
  location      = "us-central1"
  connection_id = %[3]q
  labels        = { team = %[4]q }
  annotations   = { note = %[5]q }
  disabled      = %[6]t
}

resource "google_developer_connect_git_repository_link" "l" {
  project                = %[2]q
  location               = "us-central1"
  parent_connection      = google_developer_connect_connection.c.connection_id
  git_repository_link_id = "tf-link"
  clone_uri              = %[7]q
  labels                 = { repo = "app" }
}

output "read" {
  value = {
    connection = "${google_developer_connect_connection.c.name}|${google_developer_connect_connection.c.uid}"
    link       = "${google_developer_connect_git_repository_link.l.name}|${google_developer_connect_git_repository_link.l.uid}|${google_developer_connect_git_repository_link.l.clone_uri}"
  }
}
`, conn.GetName(), h.Project(), connID, team, note, disabled, cloneURI)
	}
	linkName := conn.GetName() + "/gitRepositoryLinks/tf-link"
	check := func(when, team, note string, disabled bool) {
		t.Helper()
		c, err := dc.GetConnection(ctx, &dcpb.GetConnectionRequest{Name: conn.GetName()})
		if err != nil || c.GetLabels()["team"] != team || c.GetAnnotations()["note"] != note || c.GetDisabled() != disabled ||
			c.GetSecureSourceManagerInstanceConfig().GetInstance() != inst.GetName() {
			t.Errorf("%s, the connection = %v, %v; want team=%s note=%s disabled=%t", when, c, err, team, note, disabled)
		}
		l, err := dc.GetGitRepositoryLink(ctx, &dcpb.GetGitRepositoryLinkRequest{Name: linkName})
		if err != nil || l.GetCloneUri() != cloneURI || l.GetLabels()["repo"] != "app" {
			t.Errorf("%s, the link = %v, %v", when, l, err)
		}
		// What Terraform read is what the client reads.
		m.must("refresh", "-input=false", "-no-color")
		var got map[string]string
		if err := json.Unmarshal([]byte(m.must("output", "-json", "read")), &got); err != nil {
			t.Fatalf("%s, the outputs: %v", when, err)
		}
		want := map[string]string{"connection": c.GetName() + "|" + c.GetUid(),
			"link": l.GetName() + "|" + l.GetUid() + "|" + l.GetCloneUri()}
		for k, v := range want {
			if got[k] != v {
				t.Errorf("%s, Terraform reads the %s as %q; the official client %q", when, k, got[k], v)
			}
		}
	}

	m.write(module("build", "first", false))
	m.must("init", "-input=false", "-no-color")
	out := m.must("apply", "-auto-approve", "-input=false", "-no-color")
	if !strings.Contains(out, "1 imported, 1 added, 1 changed, 0 destroyed") {
		t.Errorf("the import was not applied as planned:\n%s", lastLines(out, 30))
	}
	check("after apply", "build", "first", false)
	m.planClean()

	m.write(module("release", "second", true))
	out = m.must("apply", "-auto-approve", "-input=false", "-no-color")
	if !strings.Contains(out, "0 added, 1 changed, 0 destroyed") {
		t.Errorf("the change was not applied in place:\n%s", lastLines(out, 30))
	}
	check("after the change", "release", "second", true)
	m.planClean()

	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := dc.GetGitRepositoryLink(ctx, &dcpb.GetGitRepositoryLinkRequest{Name: linkName}); status.Code(err) != codes.NotFound {
		t.Errorf("GetGitRepositoryLink after destroy: %v; want NotFound", err)
	}
	if _, err := dc.GetConnection(ctx, &dcpb.GetConnectionRequest{Name: conn.GetName()}); status.Code(err) != codes.NotFound {
		t.Errorf("GetConnection after destroy: %v; want NotFound", err)
	}
}
