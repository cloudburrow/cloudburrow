package main

// Artifact Registry repositories and Cloud Build's build history (#1347):
// what `gcloud run deploy --source` creates. Both read the stores the
// adapter's ArtifactRegistry and CloudBuild APIs serve, in this process.

import (
	"context"
	"strings"

	cbpb "cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"

	runadapter "github.com/cloudburrow/cloudburrow/internal/adapter/run"
	"github.com/cloudburrow/cloudburrow/internal/console"
)

// artifactRegistryProvider is the Artifact Registry Repositories page.
type artifactRegistryProvider struct{ defaultProject string }

func (artifactRegistryProvider) ID() string    { return "artifact-registry" }
func (artifactRegistryProvider) Title() string { return "Artifact Registry repositories" }

func (p artifactRegistryProvider) List(_ context.Context, project string) (console.Listing, error) {
	out := console.Listing{Columns: []string{"Format", "Location", "URL", "Description", "Age"},
		NameColumn: "Repository", Noun: "repositories"}
	if project == "" {
		project = p.defaultProject
	}
	for _, r := range runadapter.Repositories() {
		parts := strings.Split(r.GetName(), "/")
		if len(parts) != 6 || parts[1] != project {
			continue
		}
		out.Items = append(out.Items, console.Resource{Name: parts[5], Fields: map[string]string{
			"Format": r.GetFormat().String(), "Location": parts[3], "URL": orDash(r.GetRegistryUri()),
			"Description": orDash(r.GetDescription()), "Age": stampAge(r.GetCreateTime())}})
	}
	out.Total = len(out.Items)
	return out, nil
}

// runBuildsProvider is Cloud Run's build history: every source build,
// newest first, with its status, image and digest.
type runBuildsProvider struct{ defaultProject string }

func (runBuildsProvider) ID() string    { return "run-builds" }
func (runBuildsProvider) Title() string { return "Cloud Run build history" }

func (p runBuildsProvider) List(_ context.Context, project string) (console.Listing, error) {
	out := console.Listing{Columns: []string{"Location", "Image", "Digest", "Source", "Age"},
		NameColumn: "Build", Noun: "builds", AlwaysStatus: true}
	if project == "" {
		project = p.defaultProject
	}
	for _, b := range runadapter.CloudBuilds(project, "") {
		parts := strings.Split(b.GetName(), "/")
		location, image, digest, source := "global", "", "", ""
		if len(parts) == 6 {
			location = parts[3]
		}
		if len(b.GetImages()) > 0 {
			image = b.GetImages()[0]
		}
		if imgs := b.GetResults().GetImages(); len(imgs) > 0 {
			digest = imgs[0].GetDigest()
		}
		if s := b.GetSource().GetStorageSource(); s != nil {
			source = "gs://" + s.GetBucket() + "/" + s.GetObject()
		}
		out.Items = append(out.Items, console.Resource{Name: b.GetId(), Status: buildStatus(b), Fields: map[string]string{
			"Location": location, "Image": orDash(image), "Digest": orDash(digest), "Source": orDash(source),
			"Age": stampAge(b.GetCreateTime())}})
	}
	out.Total = len(out.Items)
	return out, nil
}

// buildStatus is a build's status as the console's Cloud Build page words it.
func buildStatus(b *cbpb.Build) string {
	switch b.GetStatus() {
	case cbpb.Build_SUCCESS:
		return "Succeeded"
	case cbpb.Build_FAILURE, cbpb.Build_INTERNAL_ERROR, cbpb.Build_TIMEOUT:
		return "Failed"
	case cbpb.Build_WORKING:
		return "Running"
	case cbpb.Build_CANCELLED, cbpb.Build_EXPIRED:
		return "Cancelled"
	}
	return "Queued"
}

var (
	_ console.Provider = artifactRegistryProvider{}
	_ console.Provider = runBuildsProvider{}
)
