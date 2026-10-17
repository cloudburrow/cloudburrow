package main

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	arpb "cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	"cloud.google.com/go/iam/apiv1/iampb"
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
	if err != nil || len(d.Sections) != 3 || len(d.Sections[0].Listing.Items) != 1 || d.Sections[0].Listing.Items[0].Name != "web" {
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
	// #1449: annotations, a display name, a rule and the VPC-SC policy.
	if err := p.ActAt(ctx, "", vpath, "annotate", map[string]string{"annotations": "team=core, scan=clean"}); err != nil {
		t.Fatal(err)
	}
	if d, _ := p.Detail(ctx, "", vpath); !strings.Contains(fmt.Sprint(d.Summary), "scan=clean, team=core") {
		t.Errorf("version annotations = %v", d.Summary)
	}
	if err := p.ActAt(ctx, "", []string{"images", "web"}, "display-name", map[string]string{"name": "Web front end"}); err != nil {
		t.Fatal(err)
	}
	if d, _ := p.Detail(ctx, "", []string{"images", "web"}); !strings.Contains(fmt.Sprint(d.Summary), "Web front end") {
		t.Errorf("display name = %v", d.Summary)
	}
	for action, v := range map[string]map[string]string{"add-rule": {"id": "deny-all", "action": "DENY"}, "vpcsc": {"policy": "ALLOW"}} {
		if err := p.ActAt(ctx, "", []string{"images"}, action, v); err != nil {
			t.Fatalf("%s: %v", action, err)
		}
	}
	if d, _ := p.Detail(ctx, "", []string{"images"}); len(d.Sections[1].Listing.Items) != 1 || !strings.Contains(fmt.Sprint(d.Summary), "ALLOW") {
		t.Errorf("rules = %+v, summary %v", d.Sections[1].Listing.Items, d.Summary)
	}
	if err := p.ActAt(ctx, "", []string{"images"}, "delete-rule", map[string]string{"id": "deny-all"}); err != nil {
		t.Fatal(err)
	}
	_ = p.ActAt(ctx, "", []string{"images"}, "vpcsc", map[string]string{"policy": "DENY"})
	// #1519: the location's projectConfig platform logs.
	if err := p.ActAt(ctx, "", []string{"images"}, "platform-logs", map[string]string{"state": "ENABLED", "severity": "WARNING"}); err != nil {
		t.Fatal(err)
	}
	if d, _ := p.Detail(ctx, "", []string{"images"}); !strings.Contains(fmt.Sprint(d.Summary), "ENABLED, severity WARNING") {
		t.Errorf("platform logs = %v", d.Summary)
	}
	_ = p.ActAt(ctx, "", []string{"images"}, "platform-logs", map[string]string{"state": "LOGGING_STATE_UNSPECIFIED", "severity": "SEVERITY_LEVEL_UNSPECIFIED"})
	file := d.Sections[0].Listing.Items[0].Name
	if err := p.ActAt(ctx, "", vpath, "annotate-file", map[string]string{"file": file, "annotations": "scan=ok"}); err != nil {
		t.Fatal(err)
	}
	if d, _ := p.Detail(ctx, "", vpath); d.Sections[0].Listing.Items[0].Fields["Annotations"] != "scan=ok" {
		t.Errorf("file annotations = %+v", d.Sections[0].Listing.Items[0])
	}
	if err := p.ActAt(ctx, "", vpath, "attach", map[string]string{"id": "sbom", "type": "application/spdx", "file": file}); err != nil {
		t.Fatal(err)
	}
	if d, _ := p.Detail(ctx, "", []string{"images"}); len(d.Sections[2].Listing.Items) != 1 {
		t.Errorf("attachments = %+v", d.Sections[2].Listing.Items)
	}
	if err := p.ActAt(ctx, "", []string{"images"}, "delete-attachment", map[string]string{"id": "sbom"}); err != nil {
		t.Fatal(err)
	}
	if err := p.ActAt(ctx, "", []string{"images"}, "pull-percent", map[string]string{"percent": "10"}); err != nil {
		t.Fatal(err)
	}
	if d, _ := p.Detail(ctx, "", []string{"images"}); !strings.Contains(fmt.Sprint(d.Summary), "pull percent 10") {
		t.Errorf("project settings = %v", d.Summary)
	}
	if err := p.ActAt(ctx, "", vpath, "export", map[string]string{"path": "b"}); err == nil || !strings.Contains(err.Error(), "Cloud Storage") {
		t.Errorf("export with no Cloud Storage = %v", err)
	}
	if err := p.ActAt(ctx, "", []string{"images", "web"}, "delete-untagged", nil); err == nil {
		t.Error("delete-untagged with every version tagged succeeded")
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

// TestConsoleArtifactRegistryPermissions (#1439): a repository's page has a
// Permissions tab over its stored policy (#1304), read back through the
// ArtifactRegistry API's GetIamPolicy; an image's page has none.
func TestConsoleArtifactRegistryPermissions(t *testing.T) {
	ctx := context.Background()
	st, err := registry.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	runadapter.UseRegistry(st)
	t.Cleanup(func() { runadapter.UseRegistry(nil) })
	s := runadapter.NewServer(&runadapter.Knative{}, "t", time.Minute)
	ar := s.ArtifactRegistry()
	const name = "projects/console-ar-iam/locations/us-central1/repositories/perm"
	if _, err := ar.CreateRepository(ctx, &arpb.CreateRepositoryRequest{
		Parent: "projects/console-ar-iam/locations/us-central1", RepositoryId: "perm",
		Repository: &arpb.Repository{Format: arpb.Repository_DOCKER}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { runadapter.RemoveRepository(name) })
	p := artifactRegistryProvider{defaultProject: "console-ar-iam"}
	var lastStale error
	c := permissionsCase{service: "artifact-registry", project: "console-ar-iam", path: []string{"perm"},
		readBack: func(t *testing.T) *iampb.Policy {
			pol, err := ar.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: name})
			if err != nil {
				t.Fatal(err)
			}
			return pol
		},
		write: func(t *testing.T, add *iampb.Binding) {
			pol, _ := ar.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: name})
			stale := pol.GetEtag()
			pol.Bindings = append(pol.Bindings, add)
			if _, err := ar.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: name, Policy: pol}); err != nil {
				t.Fatal(err)
			}
			pol.Etag = stale
			_, lastStale = ar.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: name, Policy: pol})
		},
		refusal: func() string { return grpcRefusal(lastStale) },
	}
	exercisePermissions(t, p, c)

	if held, err := p.TestPermissions(ctx, "", []string{"perm"}, []string{"artifactregistry.repositories.get"}); err != nil || len(held) != 1 {
		t.Errorf("TestPermissions = %v, %v", held, err)
	}
	if p.PolicyOn(ctx, []string{"perm", "web"}) != nil {
		t.Error("an image's page has a Permissions tab; only a repository has a policy")
	}
	if _, err := p.GetPolicy(ctx, "", []string{"missing"}); err == nil {
		t.Error("a missing repository's policy was read")
	}
}
