//go:build compat

package compat

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	artifactregistry "cloud.google.com/go/artifactregistry/apiv1"
	arpb "cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	runpb "cloud.google.com/go/run/apiv2/runpb"
	"github.com/google/go-containerregistry/pkg/crane"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// fixtureImage is testdata/sourcebuild's server, built for the kind
// nodes' architecture, as a one-layer image whose entrypoint runs it;
// label varies the config so two calls are two images.
func fixtureImage(t *testing.T, label string) v1.Image {
	t.Helper()
	dir := sourceBuildDir(t)
	bin, err := os.ReadFile(filepath.Join(dir, "server"))
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: "server", Mode: 0o755, Size: int64(len(bin)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	_, _ = tw.Write(bin)
	_ = tw.Close()
	layer, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(buf.Bytes())), nil })
	if err != nil {
		t.Fatal(err)
	}
	img, err := mutate.AppendLayers(empty.Image, layer)
	if err != nil {
		t.Fatal(err)
	}
	cf, err := img.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	cf = cf.DeepCopy()
	cf.OS, cf.Architecture = "linux", runtime.GOARCH
	cf.Config.Entrypoint = []string{"/server"}
	cf.Config.Labels = map[string]string{"fixture": label}
	img, err = mutate.ConfigFile(img, cf)
	if err != nil {
		t.Fatal(err)
	}
	return img
}

// registryOptions reach the Run port's registry whatever the host in the
// reference: {region}-docker.pkg.localhost resolves to loopback by RFC 6761,
// but not every resolver applies it, so the dial goes to the Run port's
// address directly, as the Host still names the location.
func registryOptions(h *Harness) []crane.Option {
	addr := h.Endpoint(EnvRun)
	tr := &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}}
	return []crane.Option{crane.Insecure, crane.WithTransport(tr)}
}

// covers: google.devtools.artifactregistry.v1.ArtifactRegistry/ListPackages, google.devtools.artifactregistry.v1.ArtifactRegistry/GetPackage, google.devtools.artifactregistry.v1.ArtifactRegistry/DeletePackage, google.devtools.artifactregistry.v1.ArtifactRegistry/ListVersions, google.devtools.artifactregistry.v1.ArtifactRegistry/GetVersion, google.devtools.artifactregistry.v1.ArtifactRegistry/DeleteVersion, google.devtools.artifactregistry.v1.ArtifactRegistry/ListTags, google.devtools.artifactregistry.v1.ArtifactRegistry/GetTag, google.devtools.artifactregistry.v1.ArtifactRegistry/CreateTag, google.devtools.artifactregistry.v1.ArtifactRegistry/UpdateTag, google.devtools.artifactregistry.v1.ArtifactRegistry/DeleteTag, google.devtools.artifactregistry.v1.ArtifactRegistry/ListDockerImages, google.devtools.artifactregistry.v1.ArtifactRegistry/GetDockerImage, google.devtools.artifactregistry.v1.ArtifactRegistry/ListFiles, google.devtools.artifactregistry.v1.ArtifactRegistry/GetFile, google.devtools.artifactregistry.v1.ArtifactRegistry/UpdateRepository, google.devtools.artifactregistry.v1.ArtifactRegistry/DeleteRepository, google.cloud.run.v2.Services/CreateService
//
// TestArtifactRegistryDockerRegistry (#1383): a DOCKER repository is a
// Docker registry. go-containerregistry (crane) pushes two images to
// us-central1-docker.pkg.localhost:{run port}/{project}/{repository}/app,
// pulls one back by tag and digest, tags it, and reads the tags list and
// catalog. The official Artifact Registry client then reads the image as a
// package, its manifests as versions, its tags, Docker images and files;
// creates, moves and deletes a tag; refuses to delete a tagged version
// without force. A Cloud Run service deployed from the pushed image's
// Artifact Registry URI, which nothing loaded into the cluster, pulls it
// through the nodes' registry mirror and answers. Cleanup policies are
// stored; DeletePackage, then DeleteRepository, remove the content, and
// the registry no longer knows the name.
func TestArtifactRegistryDockerRegistry(t *testing.T) {
	h := New(t)
	ctx, cancel := context.WithTimeout(h.Context(), 8*time.Minute)
	t.Cleanup(cancel)
	parent := runParent(h)
	ar, err := artifactregistry.NewClient(ctx, runClientOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ar.Close() })
	repoID := "registry-repo"
	repoName := parent + "/repositories/" + repoID
	t.Cleanup(func() {
		if op, err := ar.DeleteRepository(context.Background(), &arpb.DeleteRepositoryRequest{Name: repoName}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	op, err := ar.CreateRepository(ctx, &arpb.CreateRepositoryRequest{Parent: parent, RepositoryId: repoID,
		Repository: &arpb.Repository{Format: arpb.Repository_DOCKER}})
	if err != nil {
		t.Fatalf("CreateRepository: %v", err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatalf("CreateRepository.Wait: %v", err)
	}

	_, port, _ := net.SplitHostPort(h.Endpoint(EnvRun))
	local := "us-central1-docker.pkg.localhost:" + port + "/" + h.Project() + "/" + repoID + "/app"
	opts := registryOptions(h)
	img, other := fixtureImage(t, "one"), fixtureImage(t, "two")
	if err := crane.Push(img, local+":v1", opts...); err != nil {
		t.Fatalf("crane push: %v", err)
	}
	if err := crane.Push(other, local+":other", opts...); err != nil {
		t.Fatalf("crane push of a second image: %v", err)
	}
	want, _ := img.Digest()
	if d, err := crane.Digest(local+":v1", opts...); err != nil || d != want.String() {
		t.Fatalf("crane digest = %q, %v; want %s", d, err, want)
	}
	pulled, err := crane.Pull(local+"@"+want.String(), opts...)
	if err != nil {
		t.Fatalf("crane pull by digest: %v", err)
	}
	if d, _ := pulled.Digest(); d != want {
		t.Fatalf("pulled %s; want %s", d, want)
	}
	if err := crane.Tag(local+":v1", "v2", opts...); err != nil {
		t.Fatalf("crane tag: %v", err)
	}
	if tags, err := crane.ListTags(local, opts...); err != nil || strings.Join(tags, ",") != "other,v1,v2" {
		t.Errorf("crane ls = %v, %v", tags, err)
	}
	if cat, err := crane.Catalog("us-central1-docker.pkg.localhost:"+port, opts...); err != nil ||
		!contains(cat, h.Project()+"/"+repoID+"/app") {
		t.Errorf("crane catalog = %v, %v", cat, err)
	}

	pkgName := repoName + "/packages/app"
	pkgs := collectAll(t, ar.ListPackages(ctx, &arpb.ListPackagesRequest{Parent: repoName}).Next)
	if len(pkgs) != 1 || pkgs[0].GetName() != pkgName {
		t.Fatalf("ListPackages = %v", pkgs)
	}
	if p, err := ar.GetPackage(ctx, &arpb.GetPackageRequest{Name: pkgName}); err != nil || p.GetCreateTime() == nil {
		t.Errorf("GetPackage = %v, %v", p, err)
	}
	vName := pkgName + "/versions/" + want.String()
	versions := collectAll(t, ar.ListVersions(ctx, &arpb.ListVersionsRequest{Parent: pkgName, View: arpb.VersionView_FULL}).Next)
	if len(versions) != 2 {
		t.Fatalf("ListVersions = %v", versions)
	}
	v, err := ar.GetVersion(ctx, &arpb.GetVersionRequest{Name: vName, View: arpb.VersionView_FULL})
	if err != nil || len(v.GetRelatedTags()) != 2 {
		t.Fatalf("GetVersion = %v, %v", v, err)
	}
	tags := collectAll(t, ar.ListTags(ctx, &arpb.ListTagsRequest{Parent: pkgName}).Next)
	if len(tags) != 3 || tags[1].GetName() != pkgName+"/tags/v1" || tags[1].GetVersion() != vName {
		t.Fatalf("ListTags = %v", tags)
	}
	if tg, err := ar.GetTag(ctx, &arpb.GetTagRequest{Name: pkgName + "/tags/v2"}); err != nil || tg.GetVersion() != vName {
		t.Errorf("GetTag = %v, %v", tg, err)
	}
	if tg, err := ar.CreateTag(ctx, &arpb.CreateTagRequest{Parent: pkgName, TagId: "v3", Tag: &arpb.Tag{Version: vName}}); err != nil ||
		tg.GetName() != pkgName+"/tags/v3" {
		t.Fatalf("CreateTag = %v, %v", tg, err)
	}
	if d, err := crane.Digest(local+":v3", opts...); err != nil || d != want.String() {
		t.Errorf("the created tag pulls %q, %v", d, err)
	}
	if _, err := ar.CreateTag(ctx, &arpb.CreateTagRequest{Parent: pkgName, TagId: "v3", Tag: &arpb.Tag{Version: vName}}); status.Code(err) != codes.AlreadyExists {
		t.Errorf("a second CreateTag = %v; want ALREADY_EXISTS", err)
	}
	otherDigest, _ := other.Digest()
	otherName := pkgName + "/versions/" + otherDigest.String()
	if tg, err := ar.UpdateTag(ctx, &arpb.UpdateTagRequest{Tag: &arpb.Tag{Name: pkgName + "/tags/v3", Version: otherName},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"version"}}}); err != nil || tg.GetVersion() != otherName {
		t.Fatalf("UpdateTag = %v, %v", tg, err)
	}
	if err := ar.DeleteTag(ctx, &arpb.DeleteTagRequest{Name: pkgName + "/tags/v3"}); err != nil {
		t.Fatalf("DeleteTag: %v", err)
	}
	if _, err := crane.Digest(local+":v3", opts...); err == nil {
		t.Error("the deleted tag still resolves")
	}

	images := collectAll(t, ar.ListDockerImages(ctx, &arpb.ListDockerImagesRequest{Parent: repoName}).Next)
	if len(images) != 2 {
		t.Fatalf("ListDockerImages = %v", images)
	}
	diName := repoName + "/dockerImages/app@" + want.String()
	di, err := ar.GetDockerImage(ctx, &arpb.GetDockerImageRequest{Name: diName})
	if err != nil || di.GetUri() != "us-central1-docker.pkg.dev/"+h.Project()+"/"+repoID+"/app@"+want.String() ||
		strings.Join(di.GetTags(), ",") != "v1,v2" || di.GetImageSizeBytes() == 0 || di.GetMediaType() == "" {
		t.Fatalf("GetDockerImage = %v, %v", di, err)
	}
	files := collectAll(t, ar.ListFiles(ctx, &arpb.ListFilesRequest{Parent: repoName, Filter: `owner="` + vName + `"`}).Next)
	if len(files) != 3 { // the manifest, its config and its layer
		t.Fatalf("ListFiles for %s = %v", vName, files)
	}
	if f, err := ar.GetFile(ctx, &arpb.GetFileRequest{Name: files[0].GetName()}); err != nil || f.GetSizeBytes() == 0 {
		t.Errorf("GetFile = %v, %v", f, err)
	}

	if _, err := ar.DeleteVersion(ctx, &arpb.DeleteVersionRequest{Name: otherName}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("DeleteVersion of a tagged version = %v; want FAILED_PRECONDITION", err)
	}
	dop, err := ar.DeleteVersion(ctx, &arpb.DeleteVersionRequest{Name: otherName, Force: true})
	if err != nil {
		t.Fatalf("DeleteVersion with force: %v", err)
	}
	if err := dop.Wait(ctx); err != nil {
		t.Fatalf("DeleteVersion.Wait: %v", err)
	}
	if _, err := crane.Digest(local+":other", opts...); err == nil {
		t.Error("the force-deleted version's tag still resolves")
	}

	// Cloud Run pulls the pushed image from the registry: nothing loaded it.
	if os.Getenv("CLOUDBURROW_TEST_SKIP_RUN_PULL") == "" {
		c := runClient(t, h)
		id := "registry-pull"
		name := parent + "/services/" + id
		t.Cleanup(func() { _, _ = c.DeleteService(context.Background(), &runpb.DeleteServiceRequest{Name: name}) })
		image := "us-central1-docker.pkg.dev/" + h.Project() + "/" + repoID + "/app:v1"
		sop, err := c.CreateService(ctx, &runpb.CreateServiceRequest{Parent: parent, ServiceId: id, Service: &runpb.Service{
			Template: &runpb.RevisionTemplate{Containers: []*runpb.Container{{Image: image}}}}})
		if err != nil {
			t.Fatalf("CreateService: %v", err)
		}
		svc, err := sop.Wait(ctx)
		if err != nil {
			t.Fatalf("CreateService.Wait: %v", err)
		}
		if got := svc.GetTemplate().GetContainers()[0].GetImage(); got != image {
			t.Errorf("the service reads back image %q; want %q", got, image)
		}
		if code, body := functionRequest(t, ingress(t), hostOf(t, svc.GetUri()), "", nil); code != http.StatusOK ||
			body != "built from source by SubmitBuild" {
			t.Errorf("the service answered %d %q", code, body)
		}
	}

	rep, err := ar.UpdateRepository(ctx, &arpb.UpdateRepositoryRequest{Repository: &arpb.Repository{Name: repoName,
		CleanupPolicyDryRun: true, CleanupPolicies: map[string]*arpb.CleanupPolicy{"keep-recent": {Id: "keep-recent",
			Action: arpb.CleanupPolicy_KEEP, ConditionType: &arpb.CleanupPolicy_MostRecentVersions{
				MostRecentVersions: &arpb.CleanupPolicyMostRecentVersions{KeepCount: ptr(int32(5))}}}}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"cleanup_policies", "cleanup_policy_dry_run"}}})
	if err != nil || !rep.GetCleanupPolicyDryRun() || rep.GetCleanupPolicies()["keep-recent"].GetMostRecentVersions().GetKeepCount() != 5 {
		t.Fatalf("UpdateRepository of cleanup policies = %v, %v", rep, err)
	}
	pop, err := ar.DeletePackage(ctx, &arpb.DeletePackageRequest{Name: pkgName})
	if err != nil {
		t.Fatalf("DeletePackage: %v", err)
	}
	if err := pop.Wait(ctx); err != nil {
		t.Fatalf("DeletePackage.Wait: %v", err)
	}
	if _, err := ar.GetPackage(ctx, &arpb.GetPackageRequest{Name: pkgName}); status.Code(err) != codes.NotFound {
		t.Errorf("GetPackage after DeletePackage = %v; want NOT_FOUND", err)
	}
	if err := crane.Push(img, local+":v1", opts...); err != nil {
		t.Fatalf("crane push again: %v", err)
	}
	rop, err := ar.DeleteRepository(ctx, &arpb.DeleteRepositoryRequest{Name: repoName})
	if err != nil {
		t.Fatalf("DeleteRepository: %v", err)
	}
	if err := rop.Wait(ctx); err != nil {
		t.Fatalf("DeleteRepository.Wait: %v", err)
	}
	if _, err := crane.Digest(local+":v1", opts...); err == nil || !strings.Contains(err.Error(), "NAME_UNKNOWN") {
		t.Errorf("a deleted repository's image = %v; want NAME_UNKNOWN", err)
	}
}

// collect drains an iterator's Next.
func collectAll[T any](t *testing.T, next func() (T, error)) []T {
	t.Helper()
	var out []T
	for {
		x, err := next()
		if err == iterator.Done {
			return out
		}
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		out = append(out, x)
	}
}

// covers: google.devtools.artifactregistry.v1.ArtifactRegistry/ListDockerImages, google.devtools.artifactregistry.v1.ArtifactRegistry/CreateTag, google.devtools.artifactregistry.v1.ArtifactRegistry/GetTag
//
// TestGcloudArtifactsDockerImages (#1383): `gcloud artifacts docker images
// list` and `gcloud artifacts docker tags add`, through the configuration
// `cloudburrow gcloud-setup` writes, egress blocked, over an image crane
// pushed: the list names it with its tags, and the added tag pulls the
// same digest.
func TestGcloudArtifactsDockerImages(t *testing.T) {
	h := New(t)
	ctx, cancel := context.WithTimeout(h.Context(), 5*time.Minute)
	t.Cleanup(cancel)
	parent := runParent(h)
	ar, err := artifactregistry.NewClient(ctx, runClientOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ar.Close() })
	repoID := "gcloud-docker"
	repoName := parent + "/repositories/" + repoID
	t.Cleanup(func() {
		if op, err := ar.DeleteRepository(context.Background(), &arpb.DeleteRepositoryRequest{Name: repoName}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	op, err := ar.CreateRepository(ctx, &arpb.CreateRepositoryRequest{Parent: parent, RepositoryId: repoID,
		Repository: &arpb.Repository{Format: arpb.Repository_DOCKER}})
	if err != nil {
		t.Fatalf("CreateRepository: %v", err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(h.Endpoint(EnvRun))
	local := "us-central1-docker.pkg.localhost:" + port + "/" + h.Project() + "/" + repoID + "/app"
	img := fixtureImage(t, "gcloud")
	if err := crane.Push(img, local+":v1", registryOptions(h)...); err != nil {
		t.Fatalf("crane push: %v", err)
	}
	want, _ := img.Digest()

	g := newGcloudSession(t, h)
	uri := "us-central1-docker.pkg.dev/" + h.Project() + "/" + repoID
	out := g.must("artifacts", "docker", "images", "list", uri, "--include-tags", "--format=value(package,version,tags)")
	if !strings.Contains(out, uri+"/app") || !strings.Contains(out, want.String()) || !strings.Contains(out, "v1") {
		t.Fatalf("gcloud artifacts docker images list:\n%s", out)
	}
	g.must("artifacts", "docker", "tags", "add", uri+"/app:v1", uri+"/app:prod", "--quiet")
	if d, err := crane.Digest(local+":prod", registryOptions(h)...); err != nil || d != want.String() {
		t.Errorf("the tag gcloud added pulls %q, %v; want %s", d, err, want)
	}
}
