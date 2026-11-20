package run

import (
	"context"
	"errors"
	"testing"
	"time"

	arpb "cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/cloudburrow/cloudburrow/internal/registry"
)

type fakeSaver struct {
	images map[string]v1.Image
	asked  []string
}

func (f *fakeSaver) Save(_ context.Context, local string) (v1.Image, func(), error) {
	f.asked = append(f.asked, local)
	img, ok := f.images[local]
	if !ok {
		return nil, nil, errors.New("No such image: " + local)
	}
	return img, func() {}, nil
}

// ExportImage (#1940) copies an image this registry holds to another
// repository under a new name and tag, pushes a local Docker image read
// back with the saver, defaults the tag to latest, and refuses gcr.io,
// a missing repository, a non-DOCKER one and an image found nowhere,
// without reaching any other registry.
func TestExportImage(t *testing.T) {
	st, err := registry.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	UseRegistry(st)
	t.Cleanup(func() { UseRegistry(nil) })
	s := NewServer(&Knative{}, "t", time.Minute)
	a := s.ArtifactRegistry()
	ctx := context.Background()
	parent := "projects/export-unit/locations/us-central1"
	for id, f := range map[string]arpb.Repository_Format{"src": arpb.Repository_DOCKER, "dst": arpb.Repository_DOCKER, "py": arpb.Repository_PYTHON} {
		if _, err := a.CreateRepository(ctx, &arpb.CreateRepositoryRequest{Parent: parent, RepositoryId: id,
			Repository: &arpb.Repository{Format: f}}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { RemoveRepository(parent + "/repositories/" + id) })
	}
	img, _ := random.Image(256, 2)
	srcRepo := registry.Repo{Location: "us-central1", Project: "export-unit", Repository: "src"}
	want, err := st.WriteImage(srcRepo, "server", "v1", img)
	if err != nil {
		t.Fatal(err)
	}
	host := "us-central1-docker.pkg.dev/export-unit/"
	saver := &fakeSaver{images: map[string]v1.Image{}}

	out, err := ExportImage(ctx, saver, host+"src/server:v1", host+"dst/exported/model:2")
	if err != nil || out != host+"dst/exported/model:2" {
		t.Fatalf("ExportImage from the registry = %q, %v", out, err)
	}
	dstRepo := registry.Repo{Location: "us-central1", Project: "export-unit", Repository: "dst"}
	if d, _, err := st.Resolve(dstRepo, "exported/model", "2"); err != nil || d != want {
		t.Errorf("exported tag = %s, %v; want %s", d, err, want)
	}
	if out, err := ExportImage(ctx, saver, host+"src/server@"+want, host+"dst/bydigest"); err != nil || out != host+"dst/bydigest:latest" {
		t.Errorf("ExportImage by digest, no tag = %q, %v", out, err)
	}
	if len(saver.asked) != 0 {
		t.Errorf("an image the registry holds was read from Docker: %v", saver.asked)
	}
	if !HeldImage(host+"src/server:v1") || HeldImage(host+"src/server:v9") || HeldImage("dev.local/x:1") {
		t.Error("HeldImage disagrees with the store")
	}

	local, _ := random.Image(128, 1)
	saver.images["dev.local/mine:1"] = local
	if _, err := ExportImage(ctx, saver, "dev.local/mine:1", host+"dst/mine:1"); err != nil {
		t.Fatalf("ExportImage of a local image: %v", err)
	}
	ld, _ := local.Digest()
	if d, _, err := st.Resolve(dstRepo, "mine", "1"); err != nil || d != ld.String() {
		t.Errorf("pushed local image = %s, %v; want %s", d, err, ld)
	}

	for _, c := range []struct {
		src, dst string
		code     codes.Code
	}{
		{host + "src/server:v1", "gcr.io/export-unit/model:1", codes.Unimplemented},
		{host + "src/server:v1", "docker.io/library/model:1", codes.InvalidArgument},
		{host + "src/server:v1", host + "nope/model:1", codes.NotFound},
		{host + "src/server:v1", host + "py/model:1", codes.FailedPrecondition},
		{"us-docker.pkg.dev/vertex-ai/prediction/sklearn-cpu.1-6:latest", host + "dst/m:1", codes.FailedPrecondition},
	} {
		if _, err := ExportImage(ctx, saver, c.src, c.dst); status.Code(err) != c.code {
			t.Errorf("ExportImage(%s, %s) = %v; want %v", c.src, c.dst, err, c.code)
		}
	}
}
