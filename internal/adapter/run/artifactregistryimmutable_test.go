package run

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	arpb "cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/registry"
)

// A DOCKER repository's docker_config.immutable_tags is accepted at
// create, changed by UpdateRepository and enforced: DeleteTag and moving
// a tag are FAILED_PRECONDITION while it is on. docker_config on another
// format is refused. kms_key_name is a CryptoKey in the repository's
// location, stored and read back, and cannot change.
func TestRepositoryImmutableTagsAndKMSKey(t *testing.T) {
	st, err := registry.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	UseRegistry(st)
	t.Cleanup(func() { UseRegistry(nil) })
	useFakeKMS(t)
	s := NewServer(&Knative{}, "t", time.Minute)
	a := s.ArtifactRegistry()
	ctx := context.Background()
	parent := "projects/immutable-unit/locations/us-central1"
	name := parent + "/repositories/imm"
	create := func(id string, r *arpb.Repository) error {
		_, err := a.CreateRepository(ctx, &arpb.CreateRepositoryRequest{Parent: parent, RepositoryId: id, Repository: r})
		t.Cleanup(func() { RemoveRepository(parent + "/repositories/" + id) })
		return err
	}
	immutable := func(on bool) *arpb.Repository_DockerConfig {
		return &arpb.Repository_DockerConfig{DockerConfig: &arpb.Repository_DockerRepositoryConfig{ImmutableTags: on}}
	}
	if err := create("npm", &arpb.Repository{Format: arpb.Repository_NPM, FormatConfig: immutable(true)}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("docker_config on an NPM repository = %v; want INVALID_ARGUMENT", err)
	}
	for key, want := range map[string]codes.Code{
		"k": codes.InvalidArgument,
		"projects/immutable-unit/locations/us-east1/keyRings/r/cryptoKeys/k": codes.InvalidArgument,
	} {
		if err := create("bad-key", &arpb.Repository{Format: arpb.Repository_DOCKER, KmsKeyName: key}); status.Code(err) != want {
			t.Errorf("kms_key_name %q = %v; want %v", key, err, want)
		}
	}
	key := "projects/immutable-unit/locations/us-central1/keyRings/r/cryptoKeys/k"
	if err := create("imm", &arpb.Repository{Format: arpb.Repository_DOCKER, FormatConfig: immutable(true), KmsKeyName: key}); err != nil {
		t.Fatal(err)
	}
	r, err := a.GetRepository(ctx, &arpb.GetRepositoryRequest{Name: name})
	if err != nil || !r.GetDockerConfig().GetImmutableTags() || r.GetKmsKeyName() != key {
		t.Fatalf("GetRepository = %v, %v; want immutable tags and the key", r, err)
	}
	if _, err := a.UpdateRepository(ctx, &arpb.UpdateRepositoryRequest{Repository: &arpb.Repository{Name: name, KmsKeyName: ""},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"kms_key_name"}}}); status.Code(err) != codes.Unimplemented {
		t.Errorf("UpdateRepository of kms_key_name = %v; want UNIMPLEMENTED", err)
	}

	srv := httptest.NewServer(RegistryHandler())
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "http://")
	img1, _ := random.Image(128, 1)
	img2, _ := random.Image(128, 1)
	if err := crane.Push(img1, host+"/immutable-unit/imm/app:v1"); err != nil {
		t.Fatal(err)
	}
	if err := crane.Push(img2, host+"/immutable-unit/imm/app:v2"); err != nil {
		t.Fatal(err)
	}
	d2, _ := img2.Digest()
	pkg := name + "/packages/app"
	if _, err := a.DeleteTag(ctx, &arpb.DeleteTagRequest{Name: pkg + "/tags/v1"}); status.Code(err) != codes.FailedPrecondition ||
		!strings.Contains(err.Error(), "immutable") {
		t.Errorf("DeleteTag with immutable tags = %v; want FAILED_PRECONDITION", err)
	}
	if _, err := a.UpdateTag(ctx, &arpb.UpdateTagRequest{Tag: &arpb.Tag{Name: pkg + "/tags/v1", Version: pkg + "/versions/" + d2.String()}}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("UpdateTag moving v1 with immutable tags = %v; want FAILED_PRECONDITION", err)
	}
	if _, err := a.UpdateRepository(ctx, &arpb.UpdateRepositoryRequest{Repository: &arpb.Repository{Name: name, FormatConfig: immutable(false)},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"docker_config.immutable_tags"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.DeleteTag(ctx, &arpb.DeleteTagRequest{Name: pkg + "/tags/v1"}); err != nil {
		t.Errorf("DeleteTag with immutable tags off: %v", err)
	}
}
