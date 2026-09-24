//go:build compat

package compat

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	resourcemanager "cloud.google.com/go/resourcemanager/apiv3"
	rmpb "cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"
	crmv1 "google.golang.org/api/cloudresourcemanager/v1"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

func rmV1(t *testing.T, h *Harness) *crmv1.Service {
	t.Helper()
	c, err := crmv1.NewService(h.Context(), option.WithEndpoint("http://"+h.Endpoint(EnvResourceManager)+"/"),
		option.WithoutAuthentication(), option.WithHTTPClient(http.DefaultClient))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func v1ID(h *Harness, prefix string) string {
	id := prefix + strings.TrimPrefix(h.Project(), "cb-test-")
	if len(id) > 30 {
		id = id[:30]
	}
	return strings.TrimRight(id, "-")
}

// TestResourceManagerV1Projects.
//
// The v1 REST surface (#301) through google.golang.org/api/
// cloudresourcemanager/v1: create, get, list and delete, with the project
// visible through v3 and in the console, and the other v1 methods
// UNIMPLEMENTED.
func TestResourceManagerV1Projects(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	c := rmV1(t, h)
	id := v1ID(h, "v1-")

	op, err := c.Projects.Create(&crmv1.Project{ProjectId: id, Name: "From v1"}).Context(ctx).Do()
	if err != nil {
		t.Fatalf("v1 projects.create: %v", err)
	}
	if !op.Done {
		t.Errorf("v1 create operation not done: %+v", op)
	}
	t.Cleanup(func() { _, _ = c.Projects.Delete(id).Do() })
	if p, err := c.Projects.Get(id).Context(ctx).Do(); err != nil || p.Name != "From v1" || p.LifecycleState != "ACTIVE" {
		t.Errorf("v1 projects.get = %+v, %v", p, err)
	}
	list, err := c.Projects.List().Filter("id:" + id).Context(ctx).Do()
	if err != nil || len(list.Projects) != 1 {
		t.Errorf("v1 projects.list(id:%s) = %+v, %v", id, list, err)
	}

	v3, err := resourcemanager.NewProjectsClient(ctx, rmOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	defer v3.Close()
	if p, err := v3.GetProject(ctx, &rmpb.GetProjectRequest{Name: "projects/" + id}); err != nil || p.GetDisplayName() != "From v1" {
		t.Errorf("v3 GetProject of the v1-created project = %v, %v", p, err)
	}
	if !contains(consoleProjects(t, consoleAddr(t, h), h.Project()), id) {
		t.Errorf("the v1-created project %s is not in the console", id)
	}

	var ge *googleapi.Error
	if _, err := c.Projects.GetIamPolicy(id, &crmv1.GetIamPolicyRequest{}).Context(ctx).Do(); !errors.As(err, &ge) || ge.Code != http.StatusNotImplemented {
		t.Errorf("v1 getIamPolicy = %v, want 501 UNIMPLEMENTED", err)
	}

	if _, err := c.Projects.Delete(id).Context(ctx).Do(); err != nil {
		t.Fatalf("v1 projects.delete: %v", err)
	}
	if _, err := c.Projects.Get(id).Context(ctx).Do(); !errors.As(err, &ge) || ge.Code != http.StatusNotFound {
		t.Errorf("v1 get after delete = %v, want 404", err)
	}
}

// TestGcloudProjectsList: `gcloud projects list` through the override
// `cloudburrow env` exports. Skipped where gcloud is not installed.
func TestGcloudProjectsList(t *testing.T) {
	h := New(t)
	gcloud, err := exec.LookPath("gcloud")
	if err != nil {
		t.Skip("gcloud is not on PATH")
	}
	endpoint := h.Endpoint(EnvResourceManager)
	c := rmV1(t, h)
	id := v1ID(h, "gc-")
	if _, err := c.Projects.Create(&crmv1.Project{ProjectId: id}).Context(h.Context()).Do(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = c.Projects.Delete(id).Do() })

	cfgDir := t.TempDir()
	token := filepath.Join(cfgDir, "token")
	if err := os.WriteFile(token, []byte("cloudburrow-local-not-a-credential"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(gcloud, "projects", "list", "--format=value(projectId)", "--access-token-file="+token)
	cmd.Env = append(os.Environ(),
		"CLOUDSDK_CONFIG="+cfgDir,
		"CLOUDSDK_API_ENDPOINT_OVERRIDES_CLOUDRESOURCEMANAGER=http://"+endpoint+"/",
		"CLOUDSDK_CORE_DISABLE_USAGE_REPORTING=true",
		"CLOUDSDK_COMPONENT_MANAGER_DISABLE_UPDATE_CHECK=true",
		"CLOUDSDK_CORE_DISABLE_PROMPTS=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("gcloud projects list: %v\n%s", err, out)
	}
	t.Logf("gcloud projects list:\n%s", out)
	if !strings.Contains(string(out), id) {
		t.Errorf("gcloud projects list does not show %s:\n%s", id, out)
	}
}

// TestTerraformGoogleProject: google_project created and destroyed through
// `cloudburrow terraform`, which points the provider's Resource Manager and
// Cloud Billing endpoints here. Skipped where terraform is not installed.
func TestTerraformGoogleProject(t *testing.T) {
	h := New(t)
	if _, err := exec.LookPath("terraform"); err != nil {
		t.Skip("terraform is not on PATH")
	}
	cli := os.Getenv(EnvCLI)
	if cli == "" {
		t.Skipf("%s is not set", EnvCLI)
	}
	flags := strings.Fields(os.Getenv(EnvCLIArgs))
	id := v1ID(h, "tf-")
	dir := t.TempDir()
	module := fmt.Sprintf(`terraform {
  required_providers {
    google = { source = "hashicorp/google", version = "~> 8.0" }
  }
}
resource "google_project" "p" {
  project_id          = %q
  name                = "Terraform made"
  deletion_policy     = "DELETE"
  auto_create_network = true
}
`, id)
	if err := os.WriteFile(filepath.Join(dir, "main.tf"), []byte(module), 0o644); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command(cli, append(append(append([]string{"terraform"}, flags...), "--"), args...)...)
		cmd.Dir = dir
		b, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("cloudburrow terraform %s: %v\n%s", strings.Join(args, " "), err, b)
		}
		t.Logf("terraform %s:\n%s", args[0], lastLines(string(b), 6))
	}
	c := rmV1(t, h)
	run("init", "-input=false", "-no-color")
	t.Cleanup(func() { _, _ = c.Projects.Delete(id).Do() })
	run("apply", "-auto-approve", "-input=false", "-no-color")
	if p, err := c.Projects.Get(id).Context(h.Context()).Do(); err != nil || p.Name != "Terraform made" {
		t.Fatalf("the applied project is not in CloudBurrow: %+v, %v", p, err)
	}
	run("destroy", "-auto-approve", "-input=false", "-no-color")
	var ge *googleapi.Error
	if _, err := c.Projects.Get(id).Context(h.Context()).Do(); !errors.As(err, &ge) || ge.Code != http.StatusNotFound {
		t.Errorf("the project survived destroy: %v", err)
	}
}
