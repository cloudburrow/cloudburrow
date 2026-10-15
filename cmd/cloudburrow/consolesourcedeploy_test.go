package main

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	arpb "cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	runpb "cloud.google.com/go/run/apiv2/runpb"

	runadapter "github.com/cloudburrow/cloudburrow/internal/adapter/run"
)

type stubSource struct{}

func (stubSource) Fetch(context.Context, string, string, int64) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("src")), nil
}

type stubBuilder struct{}

func (stubBuilder) Build(context.Context, runadapter.BuildSpec) error { return nil }

// TestConsoleSourceDeployScreens (#1347): the Artifact Registry
// Repositories page lists a repository the API created, with its URL, and
// Cloud Run's Build history lists a SubmitBuild build, Succeeded, with its
// image and digest.
func TestConsoleSourceDeployScreens(t *testing.T) {
	ctx := context.Background()
	s := runadapter.NewServer(&runadapter.Knative{}, "t", time.Minute)
	if _, err := s.ArtifactRegistry().CreateRepository(ctx, &arpb.CreateRepositoryRequest{
		Parent: "projects/console-sd/locations/us-central1", RepositoryId: "console-repo",
		Repository: &arpb.Repository{Format: arpb.Repository_DOCKER, Description: "from the API"}}); err != nil {
		t.Fatal(err)
	}
	repos, _ := artifactRegistryProvider{defaultProject: "console-sd"}.List(ctx, "")
	if len(repos.Items) != 1 || repos.Items[0].Name != "console-repo" ||
		repos.Items[0].Fields["URL"] != "us-central1-docker.pkg.dev/console-sd/console-repo" {
		t.Errorf("repositories = %+v", repos.Items)
	}
	image := "us-central1-docker.pkg.dev/console-sd/console-repo/svc"
	resp, err := s.Builds(stubSource{}, stubBuilder{}).SubmitBuild(ctx, &runpb.SubmitBuildRequest{
		Parent: "projects/console-sd/locations/us-central1", ImageUri: image,
		Source:    &runpb.SubmitBuildRequest_StorageSource{StorageSource: &runpb.StorageSource{Bucket: "b", Object: "o.zip"}},
		BuildType: &runpb.SubmitBuildRequest_DockerBuild_{DockerBuild: &runpb.SubmitBuildRequest_DockerBuild{}}})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 200; i++ {
		if op, _ := s.Operation(resp.GetBuildOperation().GetName()); op.GetDone() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	builds, _ := runBuildsProvider{}.List(ctx, "console-sd")
	if len(builds.Items) != 1 || builds.Items[0].Status != "Succeeded" || builds.Items[0].Fields["Image"] != image+":latest" ||
		!strings.HasPrefix(builds.Items[0].Fields["Digest"], "sha256:") || builds.Items[0].Fields["Source"] != "gs://b/o.zip" {
		t.Errorf("builds = %+v", builds.Items)
	}
}
