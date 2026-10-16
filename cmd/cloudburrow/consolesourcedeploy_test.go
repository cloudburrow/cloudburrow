package main

import (
	"context"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	cbpb "cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

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
	// UpdateRepository (#1304) changes the description and labels the page
	// shows; another path is refused; the page's Delete removes it.
	repo := "projects/console-sd/locations/us-central1/repositories/console-repo"
	if _, err := s.ArtifactRegistry().UpdateRepository(ctx, &arpb.UpdateRepositoryRequest{
		Repository: &arpb.Repository{Name: repo, Description: "changed", Labels: map[string]string{"env": "x"}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"description", "labels"}}}); err != nil {
		t.Fatal(err)
	}
	if repos, _ = (artifactRegistryProvider{defaultProject: "console-sd"}).List(ctx, ""); len(repos.Items) != 1 || repos.Items[0].Fields["Description"] != "changed" {
		t.Errorf("after the update, repositories = %+v", repos.Items)
	}
	if _, err := s.ArtifactRegistry().UpdateRepository(ctx, &arpb.UpdateRepositoryRequest{
		Repository: &arpb.Repository{Name: repo, KmsKeyName: "k"},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"kms_key_name"}}}); status.Code(err) != codes.Unimplemented {
		t.Errorf("an update of kms_key_name = %v; want Unimplemented", err)
	}
	p := artifactRegistryProvider{defaultProject: "console-sd"}
	if err := p.Delete(ctx, "", "console-repo"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ArtifactRegistry().GetRepository(ctx, &arpb.GetRepositoryRequest{Name: repo}); status.Code(err) != codes.NotFound {
		t.Errorf("GetRepository after the console's Delete = %v; want NotFound", err)
	}
	if err := p.Delete(ctx, "", "console-repo"); err == nil {
		t.Error("deleting a deleted repository succeeded")
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

	// A CreateBuild build's page shows each step's status and the log.
	op, err := s.Builds(stubSource{}, stubBuilder{}).CloudBuild().CreateBuild(ctx, &cbpb.CreateBuildRequest{
		Parent: "projects/console-sd/locations/us-central1", Build: &cbpb.Build{
			Source: &cbpb.Source{Source: &cbpb.Source_StorageSource{StorageSource: &cbpb.StorageSource{Bucket: "b", Object: "o.tgz"}}},
			Steps: []*cbpb.BuildStep{{Id: "img", Name: "gcr.io/cloud-builders/docker", Args: []string{"build", "-t", image + ":v2", "."}},
				{Name: "gcr.io/cloud-builders/docker", Args: []string{"push", image + ":v2"}}}, Images: []string{image + ":v2"}}})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 200; i++ {
		if o, _ := s.Operation(op.GetName()); o.GetDone() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	id := op.GetName()[strings.LastIndex(op.GetName(), "/")+1:]
	d, err := runBuildsProvider{}.Detail(ctx, "console-sd", []string{id})
	if err != nil || len(d.Sections) != 2 || len(d.Sections[0].Listing.Items) != 2 ||
		d.Sections[0].Listing.Items[0].Status != "Succeeded" || d.Sections[0].Listing.Items[0].Name != "#0 img" ||
		!strings.Contains(d.Sections[1].Text, "Starting Step #0") || !strings.Contains(d.Sections[1].Text, "DONE") {
		t.Errorf("build detail = %+v, %v", d, err)
	}
}

// blockingBuilder builds until its context ends.
type blockingBuilder struct{}

func (blockingBuilder) Build(ctx context.Context, _ runadapter.BuildSpec) error {
	<-ctx.Done()
	return ctx.Err()
}

// TestConsoleRunBuildAndCancel (#1347): Run build on the Build history page
// creates a build through the official Cloud Build client on the Run port,
// and Cancel on its page cancels it; the filter box narrows the history and
// Retry reruns a finished build (#1382).
func TestConsoleRunBuildAndCancel(t *testing.T) {
	ctx := context.Background()
	s := runadapter.NewServer(&runadapter.Knative{}, "t", time.Minute)
	g := grpc.NewServer()
	s.Builds(stubSource{}, blockingBuilder{}).CloudBuild().Register(g)
	runadapter.NewOperationsServer(s).Register(g)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = g.Serve(ln) }()
	t.Cleanup(g.Stop)
	p := runBuildsProvider{defaultProject: "console-cb", runAddr: func() string { return ln.Addr().String() }}
	id, err := p.Create(ctx, "", map[string]string{"source": "gs://b/src.tgz", "image": "gcr.io/console-cb/img:v1", "region": "us-central1"})
	if err != nil || id == "" {
		t.Fatalf("Create = %q, %v", id, err)
	}
	acts := p.DetailActions(ctx, "", []string{id})
	if len(acts) != 1 || acts[0].ID != "cancel" {
		t.Fatalf("a running build's actions = %+v", acts)
	}
	if err := p.ActAt(ctx, "", []string{id}, "cancel", nil); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	d, _ := p.Detail(ctx, "", []string{id})
	if acts := p.DetailActions(ctx, "", []string{id}); len(d.Summary) == 0 || d.Summary[0].Value != "CANCELLED" ||
		len(acts) != 1 || acts[0].ID != "retry" {
		t.Errorf("after Cancel: %+v, actions %+v", d.Summary, acts)
	}
	// The filter box narrows the history with ListBuilds' filter (#1382),
	// and Retry reruns the build as a new one.
	if l, _ := p.ListFiltered(ctx, "", `status="CANCELLED"`); len(l.Items) != 1 || l.Items[0].Name != id || l.ServerFilter == nil {
		t.Errorf("filtered = %+v", l)
	}
	if l, _ := p.ListFiltered(ctx, "", `status=`); l.ServerFilter == nil || l.ServerFilter.Error == "" {
		t.Errorf("a bad filter = %+v", l.ServerFilter)
	}
	if err := p.ActAt(ctx, "", []string{id}, "retry", nil); err != nil {
		t.Fatalf("Retry: %v", err)
	}
	if l, _ := p.ListFiltered(ctx, "", `status="QUEUED" OR status="WORKING"`); len(l.Items) != 1 || l.Items[0].Name == id {
		t.Errorf("after Retry, running = %+v", l.Items)
	} else {
		_ = p.ActAt(ctx, "", []string{l.Items[0].Name}, "cancel", nil)
	}
	if _, err := p.Create(ctx, "", map[string]string{"source": "b/o", "image": "x"}); err == nil {
		t.Errorf("a source without gs:// was accepted")
	}
}

// TestConsoleBuildHistorySaysWhetherStepsAreOn (#1382): the build history
// says arbitrary steps are off, naming the flag and the Docker socket, or
// on and loopback only.
func TestConsoleBuildHistorySaysWhetherStepsAreOn(t *testing.T) {
	off, err := runBuildsProvider{}.List(context.Background(), "console-steps")
	if err != nil || !strings.Contains(off.Note, "are off") || !strings.Contains(off.Note, "--cloudbuild-host-steps") ||
		!strings.Contains(off.Note, "Docker socket") {
		t.Errorf("the note without the flag = %q, %v", off.Note, err)
	}
	on, err := runBuildsProvider{hostSteps: true}.List(context.Background(), "console-steps")
	if err != nil || !strings.Contains(on.Note, "are on") || !strings.Contains(on.Note, "loopback") {
		t.Errorf("the note with the flag = %q, %v", on.Note, err)
	}
}
