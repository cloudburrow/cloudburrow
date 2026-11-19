//go:build compat

package compat

import (
	"strings"
	"testing"

	"cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/v1/random"
)

// covers: google.devtools.artifactregistry.v1.ArtifactRegistry/CreateRepository, google.devtools.artifactregistry.v1.ArtifactRegistry/GetRepository
//
// TestArtifactRegistryVirtualDockerRepository (#1897): a virtual DOCKER
// repository, created with the official client over two standard ones
// and a virtual one, reads its upstreams by priority as Google documents
// ("Higher priority values take precedence over lower values"): crane
// pulls app:v1, which both standard upstreams hold, from the higher one,
// and app:low, which only the lower one holds, from it; an upstream that
// is itself virtual is read through. A push to it is refused: it holds
// nothing of its own.
func TestArtifactRegistryVirtualDockerRepository(t *testing.T) {
	h := New(t)
	ar, ctx := arClient(t, h)
	parent := runParent(h)
	docker := &artifactregistrypb.Repository{Format: artifactregistrypb.Repository_DOCKER}
	formatRepo(t, ctx, ar, parent, "vd-high", docker)
	formatRepo(t, ctx, ar, parent, "vd-low", docker)
	formatRepo(t, ctx, ar, parent, "vd-deep", docker)
	port := h.Endpoint(EnvRun)[strings.LastIndex(h.Endpoint(EnvRun), ":")+1:]
	base := "us-central1-docker.pkg.localhost:" + port + "/" + h.Project() + "/"
	opts := registryOptions(h)
	push := func(ref string) string {
		t.Helper()
		img, err := random.Image(256, 1)
		if err != nil {
			t.Fatal(err)
		}
		if err := crane.Push(img, ref, opts...); err != nil {
			t.Fatalf("push %s: %v", ref, err)
		}
		d, _ := img.Digest()
		return d.String()
	}
	high := push(base + "vd-high/app:v1")
	push(base + "vd-low/app:v1")
	low := push(base + "vd-low/app:low")
	deep := push(base + "vd-deep/app:deep")
	virtual := func(id string, ups map[string]int32) {
		t.Helper()
		var pols []*artifactregistrypb.UpstreamPolicy
		for up, p := range ups {
			pols = append(pols, &artifactregistrypb.UpstreamPolicy{Id: up, Repository: parent + "/repositories/" + up, Priority: p})
		}
		formatRepo(t, ctx, ar, parent, id, &artifactregistrypb.Repository{Format: artifactregistrypb.Repository_DOCKER,
			Mode: artifactregistrypb.Repository_VIRTUAL_REPOSITORY, ModeConfig: &artifactregistrypb.Repository_VirtualRepositoryConfig{
				VirtualRepositoryConfig: &artifactregistrypb.VirtualRepositoryConfig{UpstreamPolicies: pols}}})
	}
	virtual("vd-inner", map[string]int32{"vd-deep": 1})
	virtual("vd", map[string]int32{"vd-low": 10, "vd-high": 100, "vd-inner": 1})
	got, err := ar.GetRepository(ctx, &artifactregistrypb.GetRepositoryRequest{Name: parent + "/repositories/vd"})
	if err != nil || got.GetMode() != artifactregistrypb.Repository_VIRTUAL_REPOSITORY || len(got.GetVirtualRepositoryConfig().GetUpstreamPolicies()) != 3 {
		t.Fatalf("GetRepository = %v, %v", got, err)
	}
	for tag, want := range map[string]string{"v1": high, "low": low, "deep": deep} {
		d, err := crane.Digest(base+"vd/app:"+tag, opts...)
		if err != nil || d != want {
			t.Errorf("virtual pull of app:%s = %s, %v; want %s", tag, d, err, want)
		}
		if _, err := crane.Pull(base+"vd/app:"+tag, opts...); err != nil {
			t.Errorf("virtual pull of app:%s with its layers: %v", tag, err)
		}
	}
	img, _ := random.Image(64, 1)
	if err := crane.Push(img, base+"vd/app:new", opts...); err == nil {
		t.Error("a push to the virtual repository succeeded")
	}
}
