package main

// The Artifact Registry repository's page (#1383): a DOCKER repository's
// images, an image's versions (manifests) and tags, a version's files, and
// Delete at each level, Add tag and Remove tag on a version: what the
// registry the repository serves holds, read from the same store the
// ArtifactRegistry API and the Docker Registry API read.

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	arpb "cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	runadapter "github.com/cloudburrow/cloudburrow/internal/adapter/run"
	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/registry"
)

// repository finds the project's repository of that ID, refusing an ID in
// two locations, as Delete does.
func (p artifactRegistryProvider) repository(project, id string) (*arpb.Repository, registry.Repo, error) {
	if project == "" {
		project = p.defaultProject
	}
	var match []*arpb.Repository
	for _, r := range runadapter.Repositories() {
		parts := strings.Split(r.GetName(), "/")
		if len(parts) == 6 && parts[1] == project && parts[5] == id {
			match = append(match, r)
		}
	}
	switch len(match) {
	case 0:
		return nil, registry.Repo{}, fmt.Errorf("repository %q not found in project %s", id, project)
	case 1:
		parts := strings.Split(match[0].GetName(), "/")
		return match[0], registry.Repo{Location: parts[3], Project: parts[1], Repository: parts[5]}, nil
	}
	return nil, registry.Repo{}, fmt.Errorf("repository %q is in %d locations; use gcloud artifacts with --location", id, len(match))
}

// Detail opens a repository, an image in it, or one of its versions.
func (p artifactRegistryProvider) Detail(_ context.Context, project string, path []string) (console.Detail, error) {
	r, repo, err := p.repository(project, path[0])
	if err != nil {
		return console.Detail{}, err
	}
	st := runadapter.RegistryStore()
	if st == nil {
		return console.Detail{Unavailable: "this instance has no registry store"}, nil
	}
	switch len(path) {
	case 1:
		return p.repositoryDetail(st, r, repo), nil
	case 2:
		return p.imageDetail(st, repo, path[1])
	case 3:
		return p.versionDetail(st, repo, path[1], path[2])
	}
	return console.DeeperThan(3, path), nil
}

func (p artifactRegistryProvider) repositoryDetail(st *registry.Store, r *arpb.Repository, repo registry.Repo) console.Detail {
	images := console.Listing{Columns: []string{"Versions", "Tags", "Updated"}, NameColumn: "Image", Noun: "images",
		RowsOpenable: true}
	for _, im := range st.Images(repo) {
		ms, err := st.Manifests(repo, im)
		if err != nil || len(ms) == 0 {
			continue
		}
		tags := 0
		for _, m := range ms {
			tags += len(m.Tags)
		}
		images.Items = append(images.Items, console.Resource{Name: im, Fields: map[string]string{
			"Versions": strconv.Itoa(len(ms)), "Tags": strconv.Itoa(tags), "Updated": stampAge(timestamppb.New(ms[0].Uploaded))}})
	}
	images.Total = len(images.Items)
	policies := []string{}
	for id := range r.GetCleanupPolicies() {
		policies = append(policies, id)
	}
	sort.Strings(policies)
	cleanup := "none"
	if len(policies) > 0 {
		cleanup = strings.Join(policies, ", ")
		if r.GetCleanupPolicyDryRun() {
			cleanup += " (dry run)"
		}
	}
	return console.Detail{
		Summary: []console.Property{{Label: "Format", Value: r.GetFormat().String()}, {Label: "Location", Value: repo.Location},
			{Label: "URL", Value: r.GetRegistryUri()}, {Label: "Push to", Value: registryPushHint(repo)},
			{Label: "Description", Value: orDash(r.GetDescription())}, {Label: "Cleanup policies", Value: cleanup}},
		Sections: []console.Section{{ID: "images", Label: "Images", Listing: images}},
	}
}

// registryPushHint is where a local docker client pushes the repository's
// images: the Run port under {location}-docker.pkg.localhost.
func registryPushHint(repo registry.Repo) string {
	return repo.Location + "-docker.pkg.localhost:<run port>/" + repo.Project + "/" + repo.Repository + "/IMAGE:TAG"
}

func (p artifactRegistryProvider) imageDetail(st *registry.Store, repo registry.Repo, image string) (console.Detail, error) {
	ms, err := st.Manifests(repo, image)
	if err != nil || len(ms) == 0 {
		return console.Detail{}, fmt.Errorf("image %q not found in repository %s", image, repo.Repository)
	}
	versions := console.Listing{Columns: []string{"Tags", "Size", "Media type", "Uploaded"}, NameColumn: "Digest",
		Noun: "versions", RowsOpenable: true}
	tags := console.Listing{Columns: []string{"Version"}, NameColumn: "Tag", Noun: "tags"}
	for _, m := range ms {
		versions.Items = append(versions.Items, console.Resource{Name: m.Digest, Fields: map[string]string{
			"Tags": orDash(strings.Join(m.Tags, ", ")), "Size": humanBytes(m.ImageSize), "Media type": m.MediaType,
			"Uploaded": stampAge(timestamppb.New(m.Uploaded))}})
		for _, t := range m.Tags {
			tags.Items = append(tags.Items, console.Resource{Name: t, Fields: map[string]string{"Version": shortDigest(m.Digest)}})
		}
	}
	sort.Slice(tags.Items, func(i, j int) bool { return tags.Items[i].Name < tags.Items[j].Name })
	versions.Total, tags.Total = len(versions.Items), len(tags.Items)
	return console.Detail{
		Summary:  []console.Property{{Label: "Image", Value: registry.ImageURL(repo, image)}, {Label: "Versions", Value: strconv.Itoa(len(ms))}},
		Sections: []console.Section{{ID: "versions", Label: "Versions", Listing: versions}, {ID: "tags", Label: "Tags", Listing: tags}},
	}, nil
}

func (p artifactRegistryProvider) versionDetail(st *registry.Store, repo registry.Repo, image, digest string) (console.Detail, error) {
	m, err := st.Manifest(repo, image, digest)
	if err != nil {
		return console.Detail{}, fmt.Errorf("version %s of %s not found", digest, image)
	}
	files := console.Listing{Columns: []string{"Kind", "Media type", "Size"}, NameColumn: "Digest", Noun: "files"}
	add := func(kind string, d registry.Descriptor) {
		files.Items = append(files.Items, console.Resource{Name: d.Digest, Fields: map[string]string{
			"Kind": kind, "Media type": orDash(d.MediaType), "Size": humanBytes(d.Size)}})
	}
	if m.Doc != nil {
		if m.Doc.Config != nil {
			add("config", *m.Doc.Config)
		}
		for _, l := range m.Doc.Layers {
			add("layer", l)
		}
		for _, x := range m.Doc.Manifests {
			kind := "manifest"
			if x.Platform != nil {
				kind = "manifest (" + x.Platform.OS + "/" + x.Platform.Architecture + ")"
			}
			add(kind, x)
		}
	}
	files.Total = len(files.Items)
	return console.Detail{
		Summary: []console.Property{{Label: "Pull by digest", Value: registry.ImageURL(repo, image) + "@" + m.Digest},
			{Label: "Tags", Value: orDash(strings.Join(m.Tags, ", "))}, {Label: "Media type", Value: m.MediaType},
			{Label: "Size", Value: humanBytes(m.ImageSize)}, {Label: "Uploaded", Value: stampAge(timestamppb.New(m.Uploaded))}},
		Sections: []console.Section{{ID: "files", Label: "Files", Listing: files}},
	}, nil
}

// DetailActions are Delete at each level, and Add tag and Remove tag on a
// version.
func (p artifactRegistryProvider) DetailActions(_ context.Context, project string, path []string) []console.Action {
	switch len(path) {
	case 1:
		return []console.Action{{ID: "delete", Label: "Delete repository", Destructive: true, Leaves: true,
			Confirm: "The repository and every image it holds are deleted, as DeleteRepository does."}}
	case 2:
		return []console.Action{{ID: "delete", Label: "Delete image", Destructive: true, Leaves: true,
			Confirm: "Every version and tag of the image is deleted, as DeletePackage does."}}
	case 3:
		return []console.Action{
			{ID: "add-tag", Label: "Add tag", Fields: []console.Field{{Name: "tag", Label: "Tag", Type: "text", Required: true,
				Help: "a new tag, or an existing one to move to this version"}}},
			{ID: "remove-tag", Label: "Remove tag", Fields: []console.Field{{Name: "tag", Label: "Tag", Type: "text", Required: true,
				Help: "one of this version's tags; the version stays"}}},
			{ID: "delete", Label: "Delete version", Destructive: true, Leaves: true,
				Confirm: "The version and its tags are deleted, as DeleteVersion with force does."},
		}
	}
	return nil
}

// ActAt performs one of DetailActions.
func (p artifactRegistryProvider) ActAt(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	if len(path) == 1 && action == "delete" {
		return p.Delete(ctx, project, path[0])
	}
	_, repo, err := p.repository(project, path[0])
	if err != nil {
		return err
	}
	st := runadapter.RegistryStore()
	if st == nil {
		return fmt.Errorf("this instance has no registry store")
	}
	switch {
	case len(path) == 2 && action == "delete":
		return st.DeleteImage(repo, path[1])
	case len(path) == 3 && action == "delete":
		return st.DeleteManifest(repo, path[1], path[2])
	case len(path) == 3 && action == "add-tag":
		tag := strings.TrimSpace(values["tag"])
		if !registry.ValidTag(tag) {
			return fmt.Errorf("%q is not a valid tag", tag)
		}
		return st.SetTag(repo, path[1], tag, path[2])
	case len(path) == 3 && action == "remove-tag":
		tag := strings.TrimSpace(values["tag"])
		tags, err := st.Tags(repo, path[1])
		if err != nil {
			return err
		}
		if tags[tag] != path[2] {
			return fmt.Errorf("%q is not a tag of this version", tag)
		}
		return st.DeleteTag(repo, path[1], tag)
	}
	return fmt.Errorf("unknown action %q", action)
}

var (
	_ console.Driller   = artifactRegistryProvider{}
	_ console.PathActor = artifactRegistryProvider{}
	_ console.Deleter   = artifactRegistryProvider{}
)
