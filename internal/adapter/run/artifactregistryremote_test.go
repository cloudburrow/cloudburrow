package run

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	arpb "cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	"github.com/cloudburrow/cloudburrow/internal/registry"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestUpstreamName(t *testing.T) {
	for in, want := range map[string]string{
		"projects/proj-a/locations/us-east1/repositories/r":                                            "projects/proj-a/locations/us-east1/repositories/r",
		"https://artifactregistry.googleapis.com/v1/projects/proj-a/locations/us-east1/repositories/r": "projects/proj-a/locations/us-east1/repositories/r",
		"https://us-east1-docker.pkg.dev/proj-a/r":                                                     "projects/proj-a/locations/us-east1/repositories/r",
		"https://registry-1.docker.io":                                                                 "", "https://pypi.org/simple": "", "projects/p": "",
	} {
		got, ok := upstreamName(in)
		if got != want || ok != (want != "") {
			t.Errorf("upstreamName(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
}

// A remote repository over one of this instance's (#1546) is created; a
// public or outside upstream is refused naming egress; a file read through
// the remote repository is then its own, and uploads to it are refused.
func TestRemoteRepositoryPullThrough(t *testing.T) {
	st, err := registry.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	UseRegistry(st)
	t.Cleanup(func() { UseRegistry(nil) })
	s := NewServer(&Knative{}, "t", time.Minute)
	a := s.ArtifactRegistry()
	ctx := context.Background()
	parent := "projects/remote-unit/locations/us-central1"
	create := func(id string, r *arpb.Repository) error {
		_, err := a.CreateRepository(ctx, &arpb.CreateRepositoryRequest{Parent: parent, RepositoryId: id, Repository: r})
		t.Cleanup(func() { RemoveRepository(parent + "/repositories/" + id) })
		return err
	}
	remoteOf := func(uri string) *arpb.Repository {
		return &arpb.Repository{Format: arpb.Repository_NPM, Mode: arpb.Repository_REMOTE_REPOSITORY,
			ModeConfig: &arpb.Repository_RemoteRepositoryConfig{RemoteRepositoryConfig: &arpb.RemoteRepositoryConfig{
				RemoteSource: &arpb.RemoteRepositoryConfig_CommonRepository{CommonRepository: &arpb.RemoteRepositoryConfig_CommonRemoteRepository{Uri: uri}}}}}
	}
	if err := create("up", &arpb.Repository{Format: arpb.Repository_NPM}); err != nil {
		t.Fatal(err)
	}
	if err := create("pub", remoteOf("https://registry.npmjs.org")); status.Code(err) != codes.Unimplemented || !strings.Contains(err.Error(), "egress is off") {
		t.Errorf("a public upstream = %v; want UNIMPLEMENTED naming egress", err)
	}
	if err := create("missing", remoteOf(parent+"/repositories/nope")); status.Code(err) != codes.InvalidArgument {
		t.Errorf("an upstream the instance lacks = %v; want INVALID_ARGUMENT", err)
	}
	if err := create("cache", remoteOf(parent+"/repositories/up")); err != nil {
		t.Fatalf("a remote repository over up: %v", err)
	}
	if _, err := addFile(parent+"/repositories/up", npmTarballID("left-pad", "1.0.0"), "left-pad", "1.0.0", []byte("tgz"), nil); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(RegistryHandlerOr(http.NotFoundHandler()))
	t.Cleanup(srv.Close)
	base := srv.URL + "/us-central1-npm.pkg.dev/remote-unit/cache"
	resp, err := http.Get(base + "/left-pad")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("packument through the remote repository = %v, %v", resp, err)
	}
	resp.Body.Close()
	if len(repoFilesOf(parent+"/repositories/cache")) != 0 {
		t.Error("the packument alone cached a file")
	}
	resp, err = http.Get(base + "/left-pad/-/left-pad-1.0.0.tgz")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(b) != "tgz" {
		t.Fatalf("tarball through the remote repository = %d %q", resp.StatusCode, b)
	}
	if f, ok := fileByID(parent+"/repositories/cache", npmTarballID("left-pad", "1.0.0")); !ok || f.Package != "left-pad" {
		t.Errorf("the remote repository did not keep the tarball it pulled: %+v", repoFilesOf(parent+"/repositories/cache"))
	}
	req, _ := http.NewRequest(http.MethodPut, base+"/other", strings.NewReader("{}"))
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("a publish to the remote repository = %v, %v; want 405", resp, err)
	}
	if err := create("virt", &arpb.Repository{Format: arpb.Repository_NPM, Mode: arpb.Repository_VIRTUAL_REPOSITORY,
		ModeConfig: &arpb.Repository_VirtualRepositoryConfig{VirtualRepositoryConfig: &arpb.VirtualRepositoryConfig{
			UpstreamPolicies: []*arpb.UpstreamPolicy{{Id: "cache", Repository: parent + "/repositories/cache", Priority: 1}}}}}); err != nil {
		t.Fatalf("a virtual repository over the remote one: %v", err)
	}
}

// gcloud's enableIngestionAttestation=false and Terraform's noCache on a
// REMOTE_REPOSITORY, which the published proto lacks, are dropped; an
// attestation or a private network's upstream is refused.
func TestTakeRemoteConfigExtras(t *testing.T) {
	path := "/v1/projects/proj-a/locations/us-central1/repositories?repositoryId=r"
	for body, want := range map[string]string{
		`{"mode":"REMOTE_REPOSITORY","remoteRepositoryConfig":{"enableIngestionAttestation":false,"noCache":{},"commonRepository":{"uri":"x"}}}`: `{"mode":"REMOTE_REPOSITORY","remoteRepositoryConfig":{"commonRepository":{"uri":"x"}}}`,
		`{"mode":"REMOTE_REPOSITORY","remoteRepositoryConfig":{"enableIngestionAttestation":true}}`:                                              "",
		`{"mode":"REMOTE_REPOSITORY","remoteRepositoryConfig":{"serviceDirectoryConfig":{"service":"s"}}}`:                                       "",
		`{"format":"DOCKER"}`: `{"format":"DOCKER"}`,
	} {
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		got, err := takeRemoteConfigExtras(r)
		if want == "" {
			if status.Code(err) != codes.Unimplemented {
				t.Errorf("%s: %v; want UNIMPLEMENTED", body, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: %v", body, err)
		}
		b, _ := io.ReadAll(got.Body)
		if string(b) != want {
			t.Errorf("%s became %s; want %s", body, b, want)
		}
	}
}
