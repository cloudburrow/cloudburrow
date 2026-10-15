//go:build compat

package compat

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	artifactregistry "cloud.google.com/go/artifactregistry/apiv1"
	arpb "cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	cloudbuild "cloud.google.com/go/cloudbuild/apiv1/v2"
	cbpb "cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// sourceBuildDir writes testdata/sourcebuild's server, built here for the
// kind nodes' architecture, and a Dockerfile that runs it into a fresh
// directory: the source `gcloud run deploy --source` uploads.
func sourceBuildDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	build := exec.Command("go", "build", "-o", filepath.Join(dir, "server"), ".")
	build.Dir = filepath.Join(moduleRoot(t), "testdata", "sourcebuild")
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+runtime.GOARCH, "GOWORK=off")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the fixture server: %v\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"),
		[]byte("FROM scratch\nCOPY server /server\nENTRYPOINT [\"/server\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// covers: run.namespaces.services.create, run.namespaces.services.get
//
// TestGcloudRunDeploySource (#1347): `gcloud run deploy --source` of a
// Dockerfile source, through the configuration `cloudburrow gcloud-setup`
// writes, egress blocked. gcloud uploads the source to Cloud Storage,
// creates the cloud-run-source-deploy Artifact Registry repository, builds
// through Cloud Build and deploys the image; the service then answers
// through the instance's ingress.
func TestGcloudRunDeploySource(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvRun)
	runShardEndpoint(h, "CLOUDBURROW_TEST_RUN_STORAGE", EnvStorage)
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is required: the build runs with the host's Docker")
	}
	g := newGcloudSession(t, h)
	id := "gcloud-source"
	region := "--region=us-central1"
	t.Cleanup(func() { _, _ = g.run(nil, "run", "services", "delete", id, region, "--quiet") })
	src := sourceBuildDir(t)
	out, err := g.run(nil, "run", "deploy", id, "--source="+src, region, "--no-allow-unauthenticated", "--quiet")
	t.Logf("gcloud run deploy --source:\n%s", out)
	if err != nil {
		t.Fatalf("gcloud run deploy --source: %v", err)
	}
	url := strings.TrimSpace(g.must("run", "services", "describe", id, region, "--format=value(status.url)"))
	if code, got := functionRequest(t, ingress(t), hostOf(t, url), "", nil); code != http.StatusOK ||
		got != "built from source by SubmitBuild" {
		t.Errorf("the deployed service answered %d %q", code, got)
	}
}

// covers: google.devtools.artifactregistry.v1.ArtifactRegistry/CreateRepository, google.devtools.artifactregistry.v1.ArtifactRegistry/GetRepository, google.devtools.artifactregistry.v1.ArtifactRegistry/ListRepositories, google.devtools.cloudbuild.v1.CloudBuild/GetBuild, google.devtools.cloudbuild.v1.CloudBuild/ListBuilds, google.devtools.cloudbuild.v1.CloudBuild/GetDefaultServiceAccount
//
// TestSourceDeployArtifactRegistryAndCloudBuild (#1347): the official
// Artifact Registry and Cloud Build clients, over gRPC on the Run port.
// A DOCKER repository is created (its operation done with it), read, listed
// and refused a second time; another format is UNIMPLEMENTED. A SubmitBuild
// of the sourcebuild fixture is a Cloud Build build: its operation is
// projects/*/locations/*/operations/{build id}, polled to completion, and
// GetBuild and ListBuilds read it back SUCCESS with its steps, its image's
// digest and its logUrl; a service deployed with image@digest runs it.
func TestSourceDeployArtifactRegistryAndCloudBuild(t *testing.T) {
	h := New(t)
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is required: SubmitBuild builds with the host's Docker")
	}
	ctx, cancel := context.WithTimeout(h.Context(), 8*time.Minute)
	t.Cleanup(cancel)
	parent := runParent(h)

	ar, err := artifactregistry.NewClient(ctx, runClientOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ar.Close() })
	repoID := "sdk-repo"
	op, err := ar.CreateRepository(ctx, &arpb.CreateRepositoryRequest{Parent: parent, RepositoryId: repoID,
		Repository: &arpb.Repository{Format: arpb.Repository_DOCKER, Description: "sdk"}})
	if err != nil {
		t.Fatalf("CreateRepository: %v", err)
	}
	repo, err := op.Wait(ctx)
	if err != nil || repo.GetName() != parent+"/repositories/"+repoID || repo.GetDescription() != "sdk" {
		t.Fatalf("CreateRepository.Wait = %v, %v", repo, err)
	}
	if got, err := ar.GetRepository(ctx, &arpb.GetRepositoryRequest{Name: repo.GetName()}); err != nil ||
		got.GetFormat() != arpb.Repository_DOCKER || got.GetRegistryUri() != "us-central1-docker.pkg.dev/"+h.Project()+"/"+repoID {
		t.Errorf("GetRepository = %v, %v", got, err)
	}
	found := false
	it := ar.ListRepositories(ctx, &arpb.ListRepositoriesRequest{Parent: parent})
	for {
		r, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListRepositories: %v", err)
		}
		found = found || r.GetName() == repo.GetName()
	}
	if !found {
		t.Errorf("ListRepositories did not list %s", repo.GetName())
	}
	if _, err := ar.CreateRepository(ctx, &arpb.CreateRepositoryRequest{Parent: parent, RepositoryId: repoID,
		Repository: &arpb.Repository{Format: arpb.Repository_DOCKER}}); status.Code(err) != codes.AlreadyExists {
		t.Errorf("a second CreateRepository = %v; want ALREADY_EXISTS", err)
	}
	if _, err := ar.CreateRepository(ctx, &arpb.CreateRepositoryRequest{Parent: parent, RepositoryId: "sdk-npm",
		Repository: &arpb.Repository{Format: arpb.Repository_NPM}}); status.Code(err) != codes.Unimplemented {
		t.Errorf("an NPM repository = %v; want UNIMPLEMENTED", err)
	}
	if _, err := ar.GetRepository(ctx, &arpb.GetRepositoryRequest{Name: parent + "/repositories/missing"}); status.Code(err) != codes.NotFound {
		t.Errorf("GetRepository of a missing repository = %v; want NOT_FOUND", err)
	}

	cb, err := cloudbuild.NewClient(ctx, runClientOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cb.Close() })
	sa, err := cb.GetDefaultServiceAccount(ctx, &cbpb.GetDefaultServiceAccountRequest{Name: parent + "/defaultServiceAccount"})
	if err != nil || !strings.HasSuffix(sa.GetServiceAccountEmail(), "@cloudbuild.gserviceaccount.com") {
		t.Errorf("GetDefaultServiceAccount = %v, %v", sa, err)
	}

	bucket := uploadSource(ctx, t, h, "source.tar.gz", sourceBuildArchive(ctx, t))
	image := "us-central1-docker.pkg.dev/" + h.Project() + "/" + repoID + "/sdk-built"
	bc := buildsClient(t, h)
	resp, err := bc.SubmitBuild(ctx, &runpb.SubmitBuildRequest{Parent: parent, ImageUri: image,
		Source:    &runpb.SubmitBuildRequest_StorageSource{StorageSource: &runpb.StorageSource{Bucket: bucket, Object: "source.tar.gz"}},
		BuildType: &runpb.SubmitBuildRequest_DockerBuild_{DockerBuild: &runpb.SubmitBuildRequest_DockerBuild{}}})
	if err != nil {
		t.Fatalf("SubmitBuild: %v", err)
	}
	var md cbpb.BuildOperationMetadata
	if err := resp.GetBuildOperation().GetMetadata().UnmarshalTo(&md); err != nil {
		t.Fatalf("the build operation's metadata is not BuildOperationMetadata: %v", err)
	}
	b := md.GetBuild()
	if want := parent + "/operations/" + b.GetId(); resp.GetBuildOperation().GetName() != want ||
		b.GetName() != parent+"/builds/"+b.GetId() {
		t.Fatalf("operation %s for build %s; want %s", resp.GetBuildOperation().GetName(), b.GetName(), want)
	}
	if done := waitBuild(ctx, t, bc, resp.GetBuildOperation()); done.GetError() != nil {
		t.Fatalf("the build failed: %v", done.GetError())
	}
	got, err := cb.GetBuild(ctx, &cbpb.GetBuildRequest{Name: b.GetName()})
	if err != nil || got.GetStatus() != cbpb.Build_SUCCESS || len(got.GetSteps()) != 1 || got.GetLogUrl() == "" ||
		len(got.GetResults().GetImages()) != 1 || !strings.HasPrefix(got.GetResults().GetImages()[0].GetDigest(), "sha256:") {
		t.Fatalf("GetBuild = %v, %v", got, err)
	}
	if g2, err := cb.GetBuild(ctx, &cbpb.GetBuildRequest{ProjectId: h.Project(), Id: b.GetId()}); err != nil || g2.GetName() != b.GetName() {
		t.Errorf("GetBuild by project and id = %v, %v", g2, err)
	}
	listed := false
	bit := cb.ListBuilds(ctx, &cbpb.ListBuildsRequest{Parent: parent, ProjectId: h.Project()})
	for {
		x, err := bit.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListBuilds: %v", err)
		}
		listed = listed || x.GetId() == b.GetId()
	}
	if !listed {
		t.Errorf("ListBuilds did not list %s", b.GetName())
	}

	// The image by digest, as gcloud deploys it, runs the local build.
	c := runClient(t, h)
	id := "sdk-by-digest"
	name := parent + "/services/" + id
	t.Cleanup(func() { _, _ = c.DeleteService(context.Background(), &runpb.DeleteServiceRequest{Name: name}) })
	sop, err := c.CreateService(ctx, &runpb.CreateServiceRequest{Parent: parent, ServiceId: id, Service: &runpb.Service{
		Template: &runpb.RevisionTemplate{Containers: []*runpb.Container{{Image: image + "@" + got.GetResults().GetImages()[0].GetDigest()}}}}})
	if err != nil {
		t.Fatalf("CreateService: %v", err)
	}
	svc, err := sop.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateService.Wait: %v", err)
	}
	if code, body := functionRequest(t, ingress(t), hostOf(t, svc.GetUri()), "", nil); code != http.StatusOK ||
		body != "built from source by SubmitBuild" {
		t.Errorf("the service answered %d %q", code, body)
	}
}
