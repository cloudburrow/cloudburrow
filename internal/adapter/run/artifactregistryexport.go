package run

import (
	"context"
	"errors"
	"strings"

	arpb "cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/registry"
)

// ExportImage copies the container image src into this instance's
// Artifact Registry as dst, as Vertex AI's ExportModel copies a model's
// serving image to an image_destination (#1940). dst is an Artifact
// Registry path, {location}-docker.pkg.dev/{project}/{repository}/{image}
// with an optional tag ("latest" when it has none), under a standard
// DOCKER repository this instance holds. src is read from that registry
// when it names an image there (by tag or digest, through a remote or
// virtual repository too); otherwise it is read back from the local Docker
// daemon with saver (`docker save`, which never pulls), so a locally built
// image (dev.local/...) is pushed as Cloud Build pushes one. It returns the
// image's full path, with its tag.
func ExportImage(ctx context.Context, saver ImageSaver, src, dst string) (string, error) {
	st := RegistryStore()
	if st == nil {
		return "", apierror.FailedPrecondition("Artifact Registry is not served on this instance, so the model's image cannot be exported")
	}
	host, _, _ := strings.Cut(dst, "/")
	if registry.LocationOf(host) == "" {
		if h := strings.ToLower(host); h == "gcr.io" || strings.HasSuffix(h, ".gcr.io") {
			return "", apierror.Unimplemented("output_config.image_destination %q is a Container Registry path, which this instance "+
				"does not serve: give an Artifact Registry path, {location}-docker.pkg.dev/{project}/{repository}/{image}[:tag]", dst)
		}
		return "", apierror.InvalidArgument("output_config.image_destination.output_uri %q is not a Container Registry or Artifact "+
			"Registry path, such as us-central1-docker.pkg.dev/projectId/repoName/imageName:tag", dst)
	}
	repo, image, tag, ok := ParseImageURL(dst)
	if !ok {
		return "", apierror.InvalidArgument("output_config.image_destination.output_uri %q is not a valid Artifact Registry image path "+
			"({location}-docker.pkg.dev/{project}/{repository}/{image}[:tag])", dst)
	}
	name := "projects/" + repo.Project + "/locations/" + repo.Location + "/repositories/" + repo.Repository
	r, found := repoFormat(name)
	if !found {
		return "", apierror.NotFound("Repository %q not found", name)
	}
	if r.GetFormat() != arpb.Repository_DOCKER {
		return "", apierror.FailedPrecondition("Repository %q is a %s repository; a container image needs a DOCKER one", name, r.GetFormat())
	}
	if r.GetMode() == arpb.Repository_REMOTE_REPOSITORY || r.GetMode() == arpb.Repository_VIRTUAL_REPOSITORY {
		return "", apierror.FailedPrecondition("Repository %q is a %s; images cannot be pushed to it", name, r.GetMode())
	}
	out := host + "/" + repo.Project + "/" + repo.Repository + "/" + image + ":" + tag

	if from, fromImage, ref, ok := heldImage(src); ok {
		if _, err := st.CopyImage(from, fromImage, ref, repo, image, tag); err != nil {
			return "", exportError(src, dst, err)
		}
		return out, nil
	}
	if saver == nil {
		return "", apierror.FailedPrecondition("the model's image %s is not in this instance's Artifact Registry, and no local Docker "+
			"daemon is available to read it from", src)
	}
	img, cleanup, err := saver.Save(ctx, src)
	if err != nil {
		return "", apierror.FailedPrecondition("the model's image %s is neither in this instance's Artifact Registry nor in the local "+
			"Docker daemon (nothing is pulled from another registry): %v", src, err)
	}
	defer cleanup()
	if _, err := st.WriteImage(repo, image, tag, img); err != nil {
		return "", exportError(src, dst, err)
	}
	return out, nil
}

func exportError(src, dst string, err error) error {
	var tagErr *registry.TagImmutableError
	switch {
	case errors.As(err, &tagErr):
		return apierror.FailedPrecondition("exporting %s to %s: %v", src, dst, err)
	case errors.Is(err, registry.ErrKeyUnavailable):
		return apierror.FailedPrecondition("exporting %s to %s: %v", src, dst, err)
	}
	return apierror.Internal(err, "exporting %s to %s: %v", src, dst, err)
}

// heldImage finds the repository, image and reference (a tag or digest)
// of ref when it names an image this instance's registry holds, reading a
// remote repository's upstream and a virtual one's upstreams by priority.
func heldImage(ref string) (registry.Repo, string, string, bool) {
	st := RegistryStore()
	if st == nil {
		return registry.Repo{}, "", "", false
	}
	base, digest, byDigest := strings.Cut(ref, "@")
	repo, image, tag, ok := ParseImageURL(base)
	if !ok {
		return registry.Repo{}, "", "", false
	}
	if byDigest {
		tag = digest
	}
	if _, found := lookupRegistryRepo(repo.Location, repo.Project, repo.Repository); !found {
		return registry.Repo{}, "", "", false
	}
	candidates := []registry.Repo{repo}
	if ups, ok := lookupRegistryVirtual(repo); ok {
		candidates = ups
	} else if up, ok := lookupRegistryUpstream(repo); ok && up != (registry.Repo{}) {
		candidates = append(candidates, up)
	}
	for _, c := range candidates {
		if _, _, err := st.Resolve(c, image, tag); err == nil {
			return c, image, tag, true
		}
	}
	return registry.Repo{}, "", "", false
}

// HeldImage reports whether ref names an image this instance's Artifact
// Registry holds, so a pull of it is answered locally.
func HeldImage(ref string) bool {
	_, _, _, ok := heldImage(ref)
	return ok
}
