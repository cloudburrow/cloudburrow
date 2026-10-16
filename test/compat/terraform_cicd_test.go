//go:build compat

package compat

import (
	"fmt"
	"strings"
	"testing"

	artifactregistry "cloud.google.com/go/artifactregistry/apiv1"
	arpb "cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Artifact Registry, Cloud Build and Cloud Run's Cloud SQL volume through
// `cloudburrow terraform` (#1304), in the run shard. Artifact Registry and
// Cloud Build are served on the Run port (#1347), where the wrapper points
// artifact_registry_custom_endpoint and cloud_build_custom_endpoint.

// TestTerraformArtifactRegistryRepository (#1304): a DOCKER
// google_artifact_registry_repository with a description and a label
// applies through `cloudburrow terraform` with egress blocked; the official
// gRPC client reads it back with its registry URL; the plan is clean; a new
// description and label are applied in place (an UpdateRepository); the
// plan is clean again; and destroy removes it.
// covers: google.devtools.artifactregistry.v1.ArtifactRegistry/CreateRepository, google.devtools.artifactregistry.v1.ArtifactRegistry/GetRepository, google.devtools.artifactregistry.v1.ArtifactRegistry/UpdateRepository, google.devtools.artifactregistry.v1.ArtifactRegistry/DeleteRepository
func TestTerraformArtifactRegistryRepository(t *testing.T) {
	h := New(t)
	m := newTFModule(t)
	_ = runClient(t, h) // skips without CLOUDBURROW_TEST_RUN
	ctx := h.Context()
	ar, err := artifactregistry.NewClient(ctx, runClientOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ar.Close() }()
	id := "tf-repo-" + strings.TrimPrefix(h.Project(), "cb-test-")
	name := runParent(h) + "/repositories/" + id

	module := func(desc, label string) string {
		return fmt.Sprintf(`
resource "google_artifact_registry_repository" "r" {
  project       = %q
  location      = "us-central1"
  repository_id = %q
  format        = "DOCKER"
  description   = %q
  labels        = { env = %q }
}
`, h.Project(), id, desc, label)
	}
	check := func(when, desc, label string) {
		t.Helper()
		r, err := ar.GetRepository(ctx, &arpb.GetRepositoryRequest{Name: name})
		if err != nil {
			t.Fatalf("%s, GetRepository: %v", when, err)
		}
		if r.GetFormat() != arpb.Repository_DOCKER || r.GetDescription() != desc || r.GetLabels()["env"] != label ||
			r.GetRegistryUri() != "us-central1-docker.pkg.dev/"+h.Project()+"/"+id {
			t.Errorf("%s, the repository = %v", when, r)
		}
	}

	m.write(module("from terraform", "local"))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	check("after apply", "from terraform", "local")
	m.planClean()

	m.write(module("changed in place", "changed"))
	out := m.must("apply", "-auto-approve", "-input=false", "-no-color")
	if !strings.Contains(out, "0 added, 1 changed, 0 destroyed") {
		t.Errorf("the change was not applied in place:\n%s", lastLines(out, 20))
	}
	check("after the change", "changed in place", "changed")
	m.planClean()

	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := ar.GetRepository(ctx, &arpb.GetRepositoryRequest{Name: name}); status.Code(err) != codes.NotFound {
		t.Errorf("GetRepository after destroy: %v; want NotFound", err)
	}
}

// TestTerraformCloudBuildTriggerRefused (#1304): Cloud Build is served for
// builds, not triggers. A google_cloudbuild_trigger is refused by this
// instance, 501 UNIMPLEMENTED from the transcoder on the Run port, with
// egress to Google blocked: the provider was pointed here and nothing
// reached Google. Triggers are not built.
func TestTerraformCloudBuildTriggerRefused(t *testing.T) {
	h := New(t)
	m := newTFModule(t)
	_ = runClient(t, h) // skips without CLOUDBURROW_TEST_RUN
	m.write(fmt.Sprintf(`
resource "google_cloudbuild_trigger" "t" {
  project  = %q
  location = "us-central1"
  name     = "tf-trigger"
  source_to_build {
    uri       = "https://example.com/repo.git"
    ref       = "refs/heads/main"
    repo_type = "GITHUB"
  }
  build {
    step {
      name = "gcr.io/cloud-builders/docker"
      args = ["version"]
    }
  }
}
`, h.Project()))
	m.must("init", "-input=false", "-no-color")
	out, err := m.tf("apply", "-auto-approve", "-input=false", "-no-color")
	if err == nil {
		t.Fatalf("a Cloud Build trigger applied:\n%s", lastLines(out, 20))
	}
	// The transcoder's own refusal: Google would answer a create, and
	// egress to it is blocked, so this 501 can only be the instance's.
	if !strings.Contains(out, "Error 501: method CreateBuildTrigger not implemented") {
		t.Errorf("the trigger was not refused 501 by this instance:\n%s", lastLines(out, 30))
	}
}

// TestTerraformRunServiceCloudSQLVolume (#1304, #1346): a
// google_cloud_run_v2_service with a cloud_sql_instance volume on the
// instance's PostgreSQL applies through `cloudburrow terraform` with egress
// blocked; the official client reads the volume back; the service answers
// SELECT 1 through /cloudsql/<connection name>; the plan is clean (the
// Auth Proxy beside it is not read back as a container); a label and an
// environment variable change in place and keep the volume; the plan is
// clean again; and destroy removes it.
// covers: google.cloud.run.v2.Services/CreateService, google.cloud.run.v2.Services/GetService, google.cloud.run.v2.Services/UpdateService, google.cloud.run.v2.Services/DeleteService
func TestTerraformRunServiceCloudSQLVolume(t *testing.T) {
	h := New(t)
	conn := runCloudSQLConn(t, h)
	m := newTFModule(t)
	svcs := runClient(t, h)
	ctx := h.Context()
	id := "tfsql-" + strings.TrimPrefix(h.Project(), "cb-test-")
	name := runParent(h) + "/services/" + id

	module := func(label, value string) string {
		return fmt.Sprintf(`
resource "google_cloud_run_v2_service" "s" {
  project             = %[1]q
  name                = %[2]q
  location            = "us-central1"
  deletion_protection = false
  labels              = { env = %[4]q }
  template {
    volumes {
      name = "cloudsql"
      cloud_sql_instance {
        instances = [%[3]q]
      }
    }
    containers {
      image   = %[6]q
      command = ["sh", "-c"]
      args = [<<-EOT
%[7]s
EOT
      ]
      env {
        name  = "CONN"
        value = %[3]q
      }
      env {
        name  = "TARGET"
        value = %[5]q
      }
      volume_mounts {
        name       = "cloudsql"
        mount_path = "/cloudsql"
      }
    }
  }
}
`, h.Project(), id, conn, label, value, postgresImage,
			// HCL interpolates ${ and %{ in a heredoc; $${ and %%{ are literal.
			strings.NewReplacer("${", "$${", "%{", "%%{").Replace(cloudSQLServeScript))
	}
	check := func(when, label, value string) *runpb.Service {
		t.Helper()
		s, err := svcs.GetService(ctx, &runpb.GetServiceRequest{Name: name})
		if err != nil {
			t.Fatalf("%s, GetService: %v", when, err)
		}
		st := s.GetTemplate()
		v := st.GetVolumes()
		if s.GetLabels()["env"] != label || len(st.GetContainers()) != 1 || len(v) != 1 ||
			len(v[0].GetCloudSqlInstance().GetInstances()) != 1 || v[0].GetCloudSqlInstance().GetInstances()[0] != conn {
			t.Errorf("%s, the service = labels %v, template %v", when, s.GetLabels(), st)
		}
		envOK := false
		for _, e := range st.GetContainers()[0].GetEnv() {
			envOK = envOK || (e.GetName() == "TARGET" && e.GetValue() == value)
		}
		if !envOK {
			t.Errorf("%s, TARGET is not %q: %v", when, value, st.GetContainers()[0].GetEnv())
		}
		return s
	}

	m.write(module("local", "tf"))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	cloudSQLSelectOne(t, check("after apply", "local", "tf").GetUri(), conn)
	m.planClean()

	m.write(module("changed", "tf2"))
	out := m.must("apply", "-auto-approve", "-input=false", "-no-color")
	if !strings.Contains(out, "0 added, 1 changed, 0 destroyed") {
		t.Errorf("the change was not applied in place:\n%s", lastLines(out, 20))
	}
	cloudSQLSelectOne(t, check("after the change", "changed", "tf2").GetUri(), conn)
	m.planClean()

	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := svcs.GetService(ctx, &runpb.GetServiceRequest{Name: name}); status.Code(err) != codes.NotFound {
		t.Errorf("GetService after destroy: %v; want NotFound", err)
	}
}
