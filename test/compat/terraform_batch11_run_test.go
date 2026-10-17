//go:build compat

package compat

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	arpb "cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	"cloud.google.com/go/iam/apiv1/iampb"
	"cloud.google.com/go/securesourcemanager/apiv1/securesourcemanagerpb"
	arapi "google.golang.org/api/artifactregistry/v1"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// The run shard's part of #1304's eleventh batch: Artifact Registry's
// project config and repository listing, and Secure Source Manager's
// authoritative IAM resources, both served on the Run port.

// TestTerraformArtifactRegistryProjectConfigAndRepositories (#1304): a
// google_artifact_registry_project_config with platform logs ENABLED at
// INFO applies, and Google's generated artifactregistry/v1 client (the Go
// apiv1 client has no projectConfig) reads it back; the plan is clean;
// DISABLED at ERROR applies in place; the plan is clean again. The data
// source google_artifact_registry_repositories, with a name_filter, lists
// the module's two repositories as the official client does, and a third
// added by the change on the next apply. Destroy removes the repositories;
// the project config, which Google keeps, stays as the module left it.
// covers: google.devtools.artifactregistry.v1.ArtifactRegistry/ListRepositories, google.devtools.artifactregistry.v1.ArtifactRegistry/CreateRepository, google.devtools.artifactregistry.v1.ArtifactRegistry/DeleteRepository
func TestTerraformArtifactRegistryProjectConfigAndRepositories(t *testing.T) {
	h := New(t)
	endpoint := h.Endpoint(EnvRun) // skips without CLOUDBURROW_TEST_RUN
	m := newTFModule(t)
	ctx := h.Context()
	svc, err := arapi.NewService(ctx, option.WithEndpoint("http://"+endpoint+"/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	locs := svc.Projects.Locations
	p := h.Project()
	cfgName := "projects/" + p + "/locations/us-west1/projectConfig"
	t.Cleanup(func() {
		_, _ = locs.UpdateProjectConfig(cfgName, &arapi.ProjectConfig{Name: cfgName}).Context(h.Context()).Do()
	})
	module := func(state, severity string, third bool) string {
		extra := ""
		deps := "google_artifact_registry_repository.a, google_artifact_registry_repository.b"
		if third {
			extra = fmt.Sprintf(`
resource "google_artifact_registry_repository" "c" {
  project       = %q
  location      = "us-west1"
  repository_id = "tfds11-c"
  format        = "NPM"
}
`, p)
			deps += ", google_artifact_registry_repository.c"
		}
		return fmt.Sprintf(`
resource "google_artifact_registry_project_config" "c" {
  project  = %[1]q
  location = "us-west1"
  platform_logs_config {
    logging_state  = %[2]q
    severity_level = %[3]q
  }
}
resource "google_artifact_registry_repository" "a" {
  project       = %[1]q
  location      = "us-west1"
  repository_id = "tfds11-a"
  format        = "DOCKER"
  description   = "first"
}
resource "google_artifact_registry_repository" "b" {
  project       = %[1]q
  location      = "us-west1"
  repository_id = "tfds11-b"
  format        = "PYTHON"
}
resource "google_artifact_registry_repository" "other" {
  project       = %[1]q
  location      = "us-west1"
  repository_id = "tfother11"
  format        = "GENERIC"
}
%[4]s
data "google_artifact_registry_repositories" "r" {
  project     = %[1]q
  location    = "us-west1"
  name_filter = "tfds11-*"
  depends_on  = [%[5]s, google_artifact_registry_repository.other]
}
output "repositories" {
  value = join(",", sort([for r in data.google_artifact_registry_repositories.r.repositories : "${r.repository_id}=${r.format}=${r.description}"]))
}
`, p, state, severity, extra, deps)
	}
	check := func(when, state, severity, wantRepos string) {
		t.Helper()
		c, err := locs.GetProjectConfig(cfgName).Context(ctx).Do()
		if err != nil {
			t.Fatalf("%s: GetProjectConfig: %v", when, err)
		}
		if c.PlatformLogsConfig == nil || c.PlatformLogsConfig.LoggingState != state || c.PlatformLogsConfig.SeverityLevel != severity {
			t.Errorf("%s: the project config = %+v (%+v); want %s at %s", when, c, c.PlatformLogsConfig, state, severity)
		}
		list, err := locs.Repositories.List("projects/" + p + "/locations/us-west1").Context(ctx).Do()
		if err != nil {
			t.Fatalf("%s: Repositories.List: %v", when, err)
		}
		var listed []string
		for _, r := range list.Repositories {
			id := r.Name[strings.LastIndex(r.Name, "/")+1:]
			if strings.HasPrefix(id, "tfds11-") {
				listed = append(listed, id+"="+r.Format+"="+r.Description)
			}
		}
		got := strings.TrimSpace(m.must("output", "-raw", "repositories"))
		if got != wantRepos || got != strings.Join(sortedCopy(listed), ",") {
			t.Errorf("%s: google_artifact_registry_repositories = %q; the official client lists %v; want %q", when, got, listed, wantRepos)
		}
	}
	m.write(module("ENABLED", "INFO", false))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	check("after apply", "ENABLED", "INFO", "tfds11-a=DOCKER=first,tfds11-b=PYTHON=")
	m.planClean()

	m.write(module("DISABLED", "ERROR", true))
	out := m.must("apply", "-auto-approve", "-input=false", "-no-color")
	if !strings.Contains(out, "1 added, 1 changed, 0 destroyed") {
		t.Errorf("the change was not applied in place:\n%s", lastLines(out, 20))
	}
	check("after the change", "DISABLED", "ERROR", "tfds11-a=DOCKER=first,tfds11-b=PYTHON=,tfds11-c=NPM=")
	m.planClean()

	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	for _, id := range []string{"tfds11-a", "tfds11-b", "tfds11-c", "tfother11"} {
		if _, err := locs.Repositories.Get("projects/" + p + "/locations/us-west1/repositories/" + id).Context(ctx).Do(); httpCode(err) != 404 {
			t.Errorf("repository %s after destroy: %v; want 404", id, err)
		}
	}
}

// TestTerraformSecureSourceManagerIAM (#1304):
// google_secure_source_manager_instance_iam_member, _binding and _policy on
// an instance, and google_secure_source_manager_repository_iam_member,
// _binding and _policy on a repository in it, through
// TestTerraformBigQueryIAM's phases (member and binding, none, policy,
// none), each read back with the official client (GetIamPolicy on the
// instance, GetIamPolicyRepo on the repository) and planned clean; the data
// sources google_secure_source_manager_instance_iam_policy and
// _repository_iam_policy read the last policy phase; destroy deletes both.
// Stored, never enforced (ADR-0006).
// covers: google.cloud.securesourcemanager.v1.SecureSourceManager/GetIamPolicyRepo, google.cloud.securesourcemanager.v1.SecureSourceManager/SetIamPolicyRepo, google.cloud.securesourcemanager.v1.SecureSourceManager/CreateInstance, google.cloud.securesourcemanager.v1.SecureSourceManager/DeleteInstance, google.cloud.securesourcemanager.v1.SecureSourceManager/CreateRepository, google.cloud.securesourcemanager.v1.SecureSourceManager/DeleteRepository
func TestTerraformSecureSourceManagerIAM(t *testing.T) {
	h := New(t)
	_ = runClient(t, h) // skips without CLOUDBURROW_TEST_RUN
	m := newTFModule(t)
	ctx := h.Context()
	c := ssmClient(t, h)
	suffix := strings.TrimPrefix(h.Project(), "cb-test-")
	inst, repo := runParent(h)+"/instances/tfiam-"+suffix, runParent(h)+"/repositories/tfriam-"+suffix
	base := fmt.Sprintf(`
resource "google_secure_source_manager_instance" "i" {
  project         = %[1]q
  location        = "us-central1"
  instance_id     = "tfiam-%[2]s"
  deletion_policy = "DELETE"
}
resource "google_secure_source_manager_repository" "r" {
  project         = %[1]q
  location        = "us-central1"
  repository_id   = "tfriam-%[2]s"
  instance        = google_secure_source_manager_instance.i.name
  deletion_policy = "DELETE"
}
`, h.Project(), suffix)
	read := func(name string, repoPolicy bool) func() map[string][]string {
		return func() map[string][]string {
			t.Helper()
			var pol *iampb.Policy
			var err error
			if repoPolicy {
				pol, err = c.GetIamPolicyRepo(ctx, &iampb.GetIamPolicyRequest{Resource: name})
			} else {
				pol, err = c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: name})
			}
			if err != nil {
				t.Fatalf("GetIamPolicy on %s: %v", name, err)
			}
			return policyMap(pol)
		}
	}
	targets := []tfIAMTarget{
		{kind: "google_secure_source_manager_instance", label: "i",
			ref: map[string]string{"project": "google_secure_source_manager_instance.i.project",
				"location": "google_secure_source_manager_instance.i.location", "instance_id": "google_secure_source_manager_instance.i.instance_id"},
			read: read(inst, false)},
		{kind: "google_secure_source_manager_repository", label: "r",
			ref: map[string]string{"project": "google_secure_source_manager_repository.r.project",
				"location": "google_secure_source_manager_repository.r.location", "repository_id": "google_secure_source_manager_repository.r.repository_id"},
			read: read(repo, true)},
	}
	roles := [2]string{"roles/securesourcemanager.instanceAccessor", "roles/securesourcemanager.repoReader"}
	runTFIAMPhases(t, m, base, roles, targets)

	// The IAM policy data sources, over a policy phase applied again.
	m.write(base + tfIAMResources(tfIAMPolicy, roles, targets) + `
data "google_secure_source_manager_instance_iam_policy" "i" {
  project     = google_secure_source_manager_instance.i.project
  location    = google_secure_source_manager_instance.i.location
  instance_id = google_secure_source_manager_instance.i.instance_id
  depends_on  = [google_secure_source_manager_instance_iam_policy.i]
}
data "google_secure_source_manager_repository_iam_policy" "r" {
  project       = google_secure_source_manager_repository.r.project
  location      = google_secure_source_manager_repository.r.location
  repository_id = google_secure_source_manager_repository.r.repository_id
  depends_on    = [google_secure_source_manager_repository_iam_policy.r]
}
output "outputs" {
  value = {
    instance   = data.google_secure_source_manager_instance_iam_policy.i.policy_data
    repository = data.google_secure_source_manager_repository_iam_policy.r.policy_data
  }
}
`)
	m.apply()
	var got map[string]string
	if err := json.Unmarshal([]byte(m.must("output", "-json", "outputs")), &got); err != nil {
		t.Fatalf("the outputs: %v", err)
	}
	for k, tg := range map[string]tfIAMTarget{"instance": targets[0], "repository": targets[1]} {
		if fromTF, want := tfPolicyBindings(t, got[k]), tg.read(); fmt.Sprint(fromTF) != fmt.Sprint(want) || len(want) != 2 {
			t.Errorf("the %s IAM policy data source = %v; the official client reads %v", k, fromTF, want)
		}
	}
	m.planClean()
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := c.GetInstance(ctx, &securesourcemanagerpb.GetInstanceRequest{Name: inst}); status.Code(err) != codes.NotFound {
		t.Errorf("GetInstance after destroy: %v; want NotFound", err)
	}
}

// TestTerraformArtifactRegistryPackageDataSources (#1304): over a GENERIC
// repository the module makes, holding two versions of a package uploaded
// with the API's media upload and a tag made with the official client, the
// data sources google_artifact_registry_packages, _package, _versions,
// _version, _tags, _tag and _file (which downloads the file) match what the
// official client reads. The change relabels the repository in place, and
// the tag moved to the other version with the official client is read on
// the next apply; the plan is clean after each. Destroy deletes the
// repository.
// covers: google.devtools.artifactregistry.v1.ArtifactRegistry/ListPackages, google.devtools.artifactregistry.v1.ArtifactRegistry/GetPackage, google.devtools.artifactregistry.v1.ArtifactRegistry/ListVersions, google.devtools.artifactregistry.v1.ArtifactRegistry/GetVersion, google.devtools.artifactregistry.v1.ArtifactRegistry/ListTags, google.devtools.artifactregistry.v1.ArtifactRegistry/GetTag, google.devtools.artifactregistry.v1.ArtifactRegistry/GetFile, google.devtools.artifactregistry.v1.ArtifactRegistry/UpdateRepository
func TestTerraformArtifactRegistryPackageDataSources(t *testing.T) {
	h := New(t)
	ar, ctx := arClient(t, h) // asks for the Run endpoint first
	m := newTFModule(t)
	p := h.Project()
	repoName := runParent(h) + "/repositories/tfds11-generic"
	pkg := repoName + "/packages/tool"
	out := filepath.Join(t.TempDir(), "tool.tar.gz")
	module := func(team string, data bool) string {
		ds := ""
		if data {
			ds = fmt.Sprintf(`
data "google_artifact_registry_packages" "ps" {
  project       = %[1]q
  location      = "us-central1"
  repository_id = google_artifact_registry_repository.g.repository_id
}
data "google_artifact_registry_package" "p" {
  project       = %[1]q
  location      = "us-central1"
  repository_id = google_artifact_registry_repository.g.repository_id
  name          = "tool"
}
data "google_artifact_registry_versions" "vs" {
  project       = %[1]q
  location      = "us-central1"
  repository_id = google_artifact_registry_repository.g.repository_id
  package_name  = "tool"
}
data "google_artifact_registry_version" "v" {
  project       = %[1]q
  location      = "us-central1"
  repository_id = google_artifact_registry_repository.g.repository_id
  package_name  = "tool"
  version_name  = "1.0.0"
}
data "google_artifact_registry_tags" "ts" {
  project       = %[1]q
  location      = "us-central1"
  repository_id = google_artifact_registry_repository.g.repository_id
  package_name  = "tool"
}
data "google_artifact_registry_tag" "t" {
  project       = %[1]q
  location      = "us-central1"
  repository_id = google_artifact_registry_repository.g.repository_id
  package_name  = "tool"
  tag_name      = "current"
}
data "google_artifact_registry_file" "f" {
  project       = %[1]q
  location      = "us-central1"
  repository_id = google_artifact_registry_repository.g.repository_id
  file_id       = "tool:1.0.0:tool.tar.gz"
  output_path   = %[2]q
  overwrite     = true
}
output "outputs" {
  value = {
    packages = join(",", [for x in data.google_artifact_registry_packages.ps.packages : x.name])
    package  = data.google_artifact_registry_package.p.name
    versions = join(",", sort([for x in data.google_artifact_registry_versions.vs.versions : x.name]))
    version  = data.google_artifact_registry_version.v.name
    tags     = join(",", [for x in data.google_artifact_registry_tags.ts.tags : "${x.name}=${x.version}"])
    tag      = data.google_artifact_registry_tag.t.version
    file     = data.google_artifact_registry_file.f.name
    size     = tostring(data.google_artifact_registry_file.f.size_bytes)
  }
}
`, p, out)
		}
		return fmt.Sprintf(`
resource "google_artifact_registry_repository" "g" {
  project       = %q
  location      = "us-central1"
  repository_id = "tfds11-generic"
  format        = "GENERIC"
  labels        = { team = %q }
}
%s`, p, team, ds)
	}
	m.write(module("burrow", false))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	for _, v := range []string{"1.0.0", "1.1.0"} {
		if code, b := genericUpload(t, h, repoName, "tool", v, "tool.tar.gz", []byte("tool "+v)); code != http.StatusOK {
			t.Fatalf("upload %s = %d %s", v, code, b)
		}
	}
	if _, err := ar.CreateTag(ctx, &arpb.CreateTagRequest{Parent: pkg, TagId: "current", Tag: &arpb.Tag{Version: pkg + "/versions/1.0.0"}}); err != nil {
		t.Fatalf("CreateTag: %v", err)
	}
	check := func(when, team, tagged string) {
		t.Helper()
		var got map[string]string
		if err := json.Unmarshal([]byte(m.must("output", "-json", "outputs")), &got); err != nil {
			t.Fatalf("%s, the outputs: %v", when, err)
		}
		var pkgs, versions, tags []string
		for _, x := range collectAll(t, ar.ListPackages(ctx, &arpb.ListPackagesRequest{Parent: repoName}).Next) {
			pkgs = append(pkgs, x.GetName())
		}
		for _, x := range collectAll(t, ar.ListVersions(ctx, &arpb.ListVersionsRequest{Parent: pkg}).Next) {
			versions = append(versions, x.GetName())
		}
		for _, x := range collectAll(t, ar.ListTags(ctx, &arpb.ListTagsRequest{Parent: pkg}).Next) {
			tags = append(tags, x.GetName()+"="+x.GetVersion())
		}
		pk, err := ar.GetPackage(ctx, &arpb.GetPackageRequest{Name: pkg})
		if err != nil {
			t.Fatalf("%s: GetPackage: %v", when, err)
		}
		ver, err := ar.GetVersion(ctx, &arpb.GetVersionRequest{Name: pkg + "/versions/1.0.0"})
		if err != nil {
			t.Fatalf("%s: GetVersion: %v", when, err)
		}
		tag, err := ar.GetTag(ctx, &arpb.GetTagRequest{Name: pkg + "/tags/current"})
		if err != nil {
			t.Fatalf("%s: GetTag: %v", when, err)
		}
		f, err := ar.GetFile(ctx, &arpb.GetFileRequest{Name: repoName + "/files/tool:1.0.0:tool.tar.gz"})
		if err != nil {
			t.Fatalf("%s: GetFile: %v", when, err)
		}
		want := map[string]string{
			"packages": strings.Join(pkgs, ","), "package": pk.GetName(),
			"versions": strings.Join(sortedCopy(versions), ","), "version": ver.GetName(),
			"tags": strings.Join(tags, ","), "tag": tag.GetVersion(),
			"file": f.GetName(), "size": fmt.Sprint(f.GetSizeBytes()),
		}
		if fmt.Sprint(got) != fmt.Sprint(want) || got["tag"] != pkg+"/versions/"+tagged || got["packages"] != pkg {
			t.Errorf("%s: the data sources = %v; the official client reads %v", when, got, want)
		}
		if b, err := os.ReadFile(out); err != nil || string(b) != "tool 1.0.0" {
			t.Errorf("%s: google_artifact_registry_file wrote %q, %v; want the file's content", when, b, err)
		}
		r, err := ar.GetRepository(ctx, &arpb.GetRepositoryRequest{Name: repoName})
		if err != nil || r.GetLabels()["team"] != team {
			t.Errorf("%s: the repository = %v, %v; want label team=%s", when, r, err, team)
		}
	}
	m.write(module("burrow", true))
	m.apply()
	check("after apply", "burrow", "1.0.0")
	m.planClean()

	if _, err := ar.UpdateTag(ctx, &arpb.UpdateTagRequest{Tag: &arpb.Tag{Name: pkg + "/tags/current", Version: pkg + "/versions/1.1.0"}}); err != nil {
		t.Fatalf("UpdateTag: %v", err)
	}
	m.write(module("warren", true))
	applied := m.must("apply", "-auto-approve", "-input=false", "-no-color")
	if !strings.Contains(applied, "0 added, 1 changed, 0 destroyed") {
		t.Errorf("the change was not applied in place:\n%s", lastLines(applied, 20))
	}
	check("after the change", "warren", "1.1.0")
	m.planClean()

	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := ar.GetRepository(ctx, &arpb.GetRepositoryRequest{Name: repoName}); status.Code(err) != codes.NotFound {
		t.Errorf("GetRepository after destroy: %v; want NotFound", err)
	}
}
