//go:build compat

package compat

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	arpb "cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	runpb "cloud.google.com/go/run/apiv2/runpb"
	"github.com/google/go-containerregistry/pkg/crane"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Artifact Registry remote repositories whose upstream is another
// Artifact Registry repository of the instance (#1546):
// remote_repository_config.common_repository.uri, as Google documents.

// remoteOver is a remote repository of a format over upstream.
func remoteOver(format arpb.Repository_Format, upstream string) *arpb.Repository {
	return &arpb.Repository{Format: format, Mode: arpb.Repository_REMOTE_REPOSITORY,
		ModeConfig: &arpb.Repository_RemoteRepositoryConfig{RemoteRepositoryConfig: &arpb.RemoteRepositoryConfig{
			Description: "pull-through",
			RemoteSource: &arpb.RemoteRepositoryConfig_CommonRepository{
				CommonRepository: &arpb.RemoteRepositoryConfig_CommonRemoteRepository{Uri: upstream}}}}}
}

// covers: google.devtools.artifactregistry.v1.ArtifactRegistry/CreateRepository, google.devtools.artifactregistry.v1.ArtifactRegistry/GetRepository, google.devtools.artifactregistry.v1.ArtifactRegistry/ListDockerImages, google.cloud.run.v2.Services/CreateService
//
// TestArtifactRegistryRemoteDockerRepository (#1546): crane pushes an
// image to a standard DOCKER repository; a remote DOCKER repository over
// it, created with the official client, answers crane's pull by tag and
// digest from the upstream, then lists the image as its own; a push to it
// is refused as read-only; a Docker Hub upstream is refused naming egress;
// and a Cloud Run service deployed from the remote repository's URI has
// the cluster pull the image through it.
func TestArtifactRegistryRemoteDockerRepository(t *testing.T) {
	h := New(t)
	ar, ctx := arClient(t, h)
	parent := runParent(h)
	up := formatRepo(t, ctx, ar, parent, "docker-upstream", &arpb.Repository{Format: arpb.Repository_DOCKER})
	_, port, _ := net.SplitHostPort(h.Endpoint(EnvRun))
	host := "us-central1-docker.pkg.localhost:" + port + "/" + h.Project()
	opts := registryOptions(h)
	img := fixtureImage(t, "remote")
	if err := crane.Push(img, host+"/docker-upstream/app:v1", opts...); err != nil {
		t.Fatalf("crane push: %v", err)
	}
	want, _ := img.Digest()

	_, err := ar.CreateRepository(ctx, &arpb.CreateRepositoryRequest{Parent: parent, RepositoryId: "docker-hub",
		Repository: &arpb.Repository{Format: arpb.Repository_DOCKER, Mode: arpb.Repository_REMOTE_REPOSITORY,
			ModeConfig: &arpb.Repository_RemoteRepositoryConfig{RemoteRepositoryConfig: &arpb.RemoteRepositoryConfig{
				RemoteSource: &arpb.RemoteRepositoryConfig_DockerRepository_{DockerRepository: &arpb.RemoteRepositoryConfig_DockerRepository{
					Upstream: &arpb.RemoteRepositoryConfig_DockerRepository_PublicRepository_{
						PublicRepository: arpb.RemoteRepositoryConfig_DockerRepository_DOCKER_HUB}}}}}}})
	if status.Code(err) != codes.Unimplemented || !strings.Contains(err.Error(), "egress is off") {
		t.Errorf("a remote repository over Docker Hub = %v; want UNIMPLEMENTED naming egress", err)
	}

	remote := formatRepo(t, ctx, ar, parent, "docker-remote", remoteOver(arpb.Repository_DOCKER, up.GetName()))
	if got, err := ar.GetRepository(ctx, &arpb.GetRepositoryRequest{Name: remote.GetName()}); err != nil ||
		got.GetMode() != arpb.Repository_REMOTE_REPOSITORY || got.GetRemoteRepositoryConfig().GetCommonRepository().GetUri() != up.GetName() {
		t.Fatalf("GetRepository of the remote repository = %v, %v", got, err)
	}
	if imgs := collectAll(t, ar.ListDockerImages(ctx, &arpb.ListDockerImagesRequest{Parent: remote.GetName()}).Next); len(imgs) != 0 {
		t.Errorf("the remote repository lists %v before any pull", imgs)
	}
	if d, err := crane.Digest(host+"/docker-remote/app:v1", opts...); err != nil || d != want.String() {
		t.Fatalf("crane digest through the remote repository = %q, %v; want %s", d, err, want)
	}
	if _, err := crane.Pull(host+"/docker-remote/app@"+want.String(), opts...); err != nil {
		t.Fatalf("crane pull through the remote repository: %v", err)
	}
	imgs := collectAll(t, ar.ListDockerImages(ctx, &arpb.ListDockerImagesRequest{Parent: remote.GetName()}).Next)
	if len(imgs) != 1 || !strings.HasSuffix(imgs[0].GetUri(), "/docker-remote/app@"+want.String()) {
		t.Errorf("ListDockerImages of the remote repository after the pull = %v", imgs)
	}
	if err := crane.Push(img, host+"/docker-remote/app:pushed", opts...); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Errorf("crane push to the remote repository = %v; want it refused as read-only", err)
	}

	if os.Getenv("CLOUDBURROW_TEST_SKIP_RUN_PULL") != "" {
		return
	}
	c := runClient(t, h)
	id := "remote-pull"
	name := parent + "/services/" + id
	t.Cleanup(func() { _, _ = c.DeleteService(context.Background(), &runpb.DeleteServiceRequest{Name: name}) })
	image := "us-central1-docker.pkg.dev/" + h.Project() + "/docker-remote/app:v1"
	sop, err := c.CreateService(ctx, &runpb.CreateServiceRequest{Parent: parent, ServiceId: id, Service: &runpb.Service{
		Template: &runpb.RevisionTemplate{Containers: []*runpb.Container{{Image: image}}}}})
	if err != nil {
		t.Fatalf("CreateService: %v", err)
	}
	svc, err := sop.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateService.Wait: %v", err)
	}
	if code, body := functionRequest(t, ingress(t), hostOf(t, svc.GetUri()), "", nil); code != http.StatusOK ||
		body != "built from source by SubmitBuild" {
		t.Errorf("the service pulled through the remote repository answered %d %q", code, body)
	}
}

// covers: google.devtools.artifactregistry.v1.ArtifactRegistry/CreateRepository, google.devtools.artifactregistry.v1.ArtifactRegistry/ListFiles, google.devtools.artifactregistry.v1.ArtifactRegistry/ListPythonPackages, google.devtools.artifactregistry.v1.ArtifactRegistry/ListNpmPackages
//
// TestArtifactRegistryRemotePackageRepositories (#1546): a wheel uploaded
// to a standard PYTHON repository is installed by pip (when on PATH, else
// read over HTTP) through a remote PYTHON repository over it, which then
// lists the wheel as its own file and Python package; a virtual repository
// over the remote one serves it too; uploads to the remote repository are
// refused. An npm tarball and a Maven jar are read through remote NPM and
// MAVEN repositories the same way.
func TestArtifactRegistryRemotePackageRepositories(t *testing.T) {
	h := New(t)
	ar, ctx := arClient(t, h)
	parent := runParent(h)
	py := formatRepo(t, ctx, ar, parent, "py-upstream", &arpb.Repository{Format: arpb.Repository_PYTHON})
	file, wheel := pyWheel(t, "0.2.0")
	if code, b := twineUpload(t, pkgBase(h, "python", h.Project(), "py-upstream"), "demo-pkg", "0.2.0", file, wheel); code != http.StatusOK {
		t.Fatalf("twine upload = %d %s", code, b)
	}
	remote := formatRepo(t, ctx, ar, parent, "py-remote", remoteOver(arpb.Repository_PYTHON, py.GetName()))
	rbase := pkgBase(h, "python", h.Project(), "py-remote")
	if pip, err := exec.LookPath("pip3"); err == nil {
		target := t.TempDir()
		cmd := exec.Command(pip, "install", "--disable-pip-version-check", "--no-cache-dir", "--no-deps", "--no-input",
			"--target", target, "--index-url", rbase+"/simple/", "demo-pkg==0.2.0")
		cmd.Env = append(os.Environ(), "PIP_CONFIG_FILE="+os.DevNull)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("pip install through the remote repository: %v\n%s", err, out)
		}
		if b, err := os.ReadFile(filepath.Join(target, "demo_pkg", "__init__.py")); err != nil || !strings.Contains(string(b), "0.2.0") {
			t.Errorf("pip installed %q, %v", b, err)
		}
	} else {
		t.Log("pip3 is not on PATH: the wheel is read over HTTP only")
		mustHTTP(t, "GET", rbase+"/demo-pkg/"+file, "", nil)
	}
	files := collectAll(t, ar.ListFiles(ctx, &arpb.ListFilesRequest{Parent: remote.GetName()}).Next)
	if len(files) != 1 || !strings.Contains(files[0].GetName(), file) || files[0].GetSizeBytes() != int64(len(wheel)) {
		t.Errorf("ListFiles of the remote repository after the install = %v", files)
	}
	if pps := collectAll(t, ar.ListPythonPackages(ctx, &arpb.ListPythonPackagesRequest{Parent: remote.GetName()}).Next); len(pps) != 1 {
		t.Errorf("ListPythonPackages of the remote repository = %v", pps)
	}
	if code, _ := twineUpload(t, rbase, "other", "1.0", "other-1.0-py3-none-any.whl", wheel); code != http.StatusMethodNotAllowed {
		t.Errorf("an upload to the remote repository = %d; want 405", code)
	}
	formatRepo(t, ctx, ar, parent, "py-virtual-remote", &arpb.Repository{Format: arpb.Repository_PYTHON, Mode: arpb.Repository_VIRTUAL_REPOSITORY,
		ModeConfig: &arpb.Repository_VirtualRepositoryConfig{VirtualRepositoryConfig: &arpb.VirtualRepositoryConfig{
			UpstreamPolicies: []*arpb.UpstreamPolicy{{Id: "remote", Repository: remote.GetName(), Priority: 1}}}}})
	if links := string(mustHTTP(t, "GET", pkgBase(h, "python", h.Project(), "py-virtual-remote")+"/simple/demo-pkg/", "", nil)); !strings.Contains(links, file) {
		t.Errorf("the virtual repository over the remote one:\n%s", links)
	}

	npmUp := formatRepo(t, ctx, ar, parent, "npm-upstream", &arpb.Repository{Format: arpb.Repository_NPM})
	tgz := npmTarball(t, "remote-demo", "1.0.0")
	mustHTTP(t, "PUT", pkgBase(h, "npm", h.Project(), "npm-upstream")+"/remote-demo", "application/json", npmPublishBody("remote-demo", "1.0.0", tgz))
	npmRemote := formatRepo(t, ctx, ar, parent, "npm-remote", remoteOver(arpb.Repository_NPM, npmUp.GetName()))
	nbase := pkgBase(h, "npm", h.Project(), "npm-remote")
	if npm, err := exec.LookPath("npm"); err == nil {
		dir := t.TempDir()
		_ = os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"consumer","version":"1.0.0"}`), 0o644)
		cmd := exec.Command(npm, "install", "--no-audit", "--no-fund", "--registry", nbase+"/", "remote-demo@1.0.0")
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "npm_config_cache="+filepath.Join(dir, ".npm"), "npm_config_userconfig="+os.DevNull)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("npm install through the remote repository: %v\n%s", err, out)
		}
		if _, err := os.Stat(filepath.Join(dir, "node_modules", "remote-demo", "index.js")); err != nil {
			t.Errorf("npm install left no package: %v", err)
		}
	} else {
		t.Log("npm is not on PATH: the tarball is read over HTTP only")
		if b := mustHTTP(t, "GET", nbase+"/remote-demo/-/remote-demo-1.0.0.tgz", "", nil); len(b) != len(tgz) {
			t.Errorf("the tarball through the remote repository is %d bytes; want %d", len(b), len(tgz))
		}
	}
	if nps := collectAll(t, ar.ListNpmPackages(ctx, &arpb.ListNpmPackagesRequest{Parent: npmRemote.GetName()}).Next); len(nps) != 1 {
		t.Errorf("ListNpmPackages of the remote repository after the install = %v", nps)
	}

	formatRepo(t, ctx, ar, parent, "mvn-upstream", &arpb.Repository{Format: arpb.Repository_MAVEN})
	jar := "com/example/lib/1.0/lib-1.0.jar"
	mustHTTP(t, "PUT", pkgBase(h, "maven", h.Project(), "mvn-upstream")+"/"+jar, "", []byte("jar bytes"))
	mvRemote := formatRepo(t, ctx, ar, parent, "mvn-remote", remoteOver(arpb.Repository_MAVEN, "https://us-central1-maven.pkg.dev/"+h.Project()+"/mvn-upstream"))
	if b := string(mustHTTP(t, "GET", pkgBase(h, "maven", h.Project(), "mvn-remote")+"/"+jar, "", nil)); b != "jar bytes" {
		t.Errorf("the jar through the remote repository = %q", b)
	}
	if files := collectAll(t, ar.ListFiles(ctx, &arpb.ListFilesRequest{Parent: mvRemote.GetName()}).Next); len(files) != 1 {
		t.Errorf("ListFiles of the remote MAVEN repository = %v", files)
	}
}

// covers: google.devtools.artifactregistry.v1.ArtifactRegistry/CreateRepository, google.devtools.artifactregistry.v1.ArtifactRegistry/GetRepository
//
// TestGcloudArtifactsRemoteRepository (#1546): `gcloud artifacts
// repositories create --mode=remote-repository --remote-python-repo=` with
// another repository's resource path, through the configuration `cloudburrow
// gcloud-setup` writes, egress blocked, makes a remote repository whose
// common_repository names it.
func TestGcloudArtifactsRemoteRepository(t *testing.T) {
	h := New(t)
	ar, ctx := arClient(t, h)
	parent := runParent(h)
	up := formatRepo(t, ctx, ar, parent, "gcloud-py-up", &arpb.Repository{Format: arpb.Repository_PYTHON})
	g := newGcloudSession(t, h)
	name := parent + "/repositories/gcloud-py-remote"
	t.Cleanup(func() {
		if op, err := ar.DeleteRepository(context.Background(), &arpb.DeleteRepositoryRequest{Name: name}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	g.must("artifacts", "repositories", "create", "gcloud-py-remote", "--repository-format=python", "--location=us-central1",
		"--project="+h.Project(), "--mode=remote-repository", "--remote-python-repo="+up.GetName(), "--quiet")
	r, err := ar.GetRepository(ctx, &arpb.GetRepositoryRequest{Name: name})
	if err != nil || r.GetMode() != arpb.Repository_REMOTE_REPOSITORY || r.GetRemoteRepositoryConfig().GetCommonRepository().GetUri() == "" {
		t.Fatalf("the repository gcloud created = %v, %v", r, err)
	}
	out := g.must("artifacts", "repositories", "describe", "gcloud-py-remote", "--location=us-central1", "--project="+h.Project(), "--format=value(mode)")
	if !strings.Contains(out, "REMOTE_REPOSITORY") {
		t.Errorf("gcloud artifacts repositories describe = %q", out)
	}
}

// covers: google.devtools.artifactregistry.v1.ArtifactRegistry/CreateRepository, google.devtools.artifactregistry.v1.ArtifactRegistry/GetRepository, google.devtools.artifactregistry.v1.ArtifactRegistry/DeleteRepository
//
// TestTerraformArtifactRegistryRemoteRepository (#1546): a
// google_artifact_registry_repository with remote_repository_config
// common_repository over a standard DOCKER one applies through `cloudburrow
// terraform` with egress blocked; the official client reads it back; the
// plan is clean; and destroy removes it.
func TestTerraformArtifactRegistryRemoteRepository(t *testing.T) {
	h := New(t)
	m := newTFModule(t)
	_ = runClient(t, h) // skips without CLOUDBURROW_TEST_RUN
	ar, ctx := arClient(t, h)
	suffix := strings.TrimPrefix(h.Project(), "cb-test-")
	m.write(fmt.Sprintf(`
resource "google_artifact_registry_repository" "upstream" {
  project       = %[1]q
  location      = "us-central1"
  repository_id = "tf-dock-up-%[2]s"
  format        = "DOCKER"
}

resource "google_artifact_registry_repository" "remote" {
  project       = %[1]q
  location      = "us-central1"
  repository_id = "tf-dock-remote-%[2]s"
  format        = "DOCKER"
  mode          = "REMOTE_REPOSITORY"
  remote_repository_config {
    description = "pull-through"
    common_repository {
      uri = google_artifact_registry_repository.upstream.id
    }
  }
}
`, h.Project(), suffix))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	parent := runParent(h)
	r, err := ar.GetRepository(ctx, &arpb.GetRepositoryRequest{Name: parent + "/repositories/tf-dock-remote-" + suffix})
	if err != nil || r.GetMode() != arpb.Repository_REMOTE_REPOSITORY ||
		r.GetRemoteRepositoryConfig().GetCommonRepository().GetUri() != parent+"/repositories/tf-dock-up-"+suffix {
		t.Fatalf("the remote repository = %v, %v", r, err)
	}
	m.planClean()
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := ar.GetRepository(ctx, &arpb.GetRepositoryRequest{Name: r.GetName()}); status.Code(err) != codes.NotFound {
		t.Errorf("after destroy, GetRepository = %v; want NOT_FOUND", err)
	}
}
