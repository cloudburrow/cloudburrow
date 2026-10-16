package main

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	arpb "cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/v1/random"

	runadapter "github.com/cloudburrow/cloudburrow/internal/adapter/run"
	"github.com/cloudburrow/cloudburrow/internal/registry"
)

// TestConsoleArtifactRegistryImagesAndTags (#1383): a repository's page
// lists the images pushed to its registry; an image's page its versions
// and tags; a version's page its files. Add tag, Remove tag and Delete at
// each level change what the registry serves.
func TestConsoleArtifactRegistryImagesAndTags(t *testing.T) {
	ctx := context.Background()
	st, err := registry.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	runadapter.UseRegistry(st)
	t.Cleanup(func() { runadapter.UseRegistry(nil) })
	s := runadapter.NewServer(&runadapter.Knative{}, "t", time.Minute)
	if _, err := s.ArtifactRegistry().CreateRepository(ctx, &arpb.CreateRepositoryRequest{
		Parent: "projects/console-ar/locations/us-central1", RepositoryId: "images",
		Repository: &arpb.Repository{Format: arpb.Repository_DOCKER}}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(runadapter.RegistryHandler())
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "http://")
	img, _ := random.Image(256, 1)
	if err := crane.Push(img, host+"/console-ar/images/web:v1"); err != nil {
		t.Fatal(err)
	}
	digest, _ := img.Digest()
	p := artifactRegistryProvider{defaultProject: "console-ar"}
	d, err := p.Detail(ctx, "", []string{"images"})
	if err != nil || len(d.Sections) != 1 || len(d.Sections[0].Listing.Items) != 1 || d.Sections[0].Listing.Items[0].Name != "web" {
		t.Fatalf("repository page = %+v, %v", d, err)
	}
	d, err = p.Detail(ctx, "", []string{"images", "web"})
	if err != nil || d.Sections[0].Listing.Items[0].Name != digest.String() || d.Sections[1].Listing.Items[0].Name != "v1" {
		t.Fatalf("image page = %+v, %v", d, err)
	}
	vpath := []string{"images", "web", digest.String()}
	d, err = p.Detail(ctx, "", vpath)
	if err != nil || len(d.Sections[0].Listing.Items) != 2 { // config and layer
		t.Fatalf("version page = %+v, %v", d, err)
	}
	if err := p.ActAt(ctx, "", vpath, "add-tag", map[string]string{"tag": "prod"}); err != nil {
		t.Fatal(err)
	}
	if got, err := crane.Digest(host + "/console-ar/images/web:prod"); err != nil || got != digest.String() {
		t.Errorf("the added tag pulls %q, %v", got, err)
	}
	if err := p.ActAt(ctx, "", vpath, "remove-tag", map[string]string{"tag": "prod"}); err != nil {
		t.Fatal(err)
	}
	if err := p.ActAt(ctx, "", vpath, "remove-tag", map[string]string{"tag": "prod"}); err == nil {
		t.Error("removing a removed tag succeeded")
	}
	if err := p.ActAt(ctx, "", vpath, "delete", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := crane.Digest(host + "/console-ar/images/web:v1"); err == nil {
		t.Error("v1 resolves after its version was deleted")
	}
	if err := crane.Push(img, host+"/console-ar/images/web:v1"); err != nil {
		t.Fatal(err)
	}
	if err := p.ActAt(ctx, "", []string{"images", "web"}, "delete", nil); err != nil {
		t.Fatal(err)
	}
	if d, _ := p.Detail(ctx, "", []string{"images"}); len(d.Sections[0].Listing.Items) != 0 {
		t.Errorf("after Delete image, images = %+v", d.Sections[0].Listing.Items)
	}
	if err := p.ActAt(ctx, "", []string{"images"}, "delete", nil); err != nil {
		t.Fatal(err)
	}
}
