package main

// Artifact Registry repositories and Cloud Build's build history (#1347):
// what `gcloud run deploy --source` creates. Both read the stores the
// adapter's ArtifactRegistry and CloudBuild APIs serve, in this process.

import (
	"context"
	"fmt"
	"strings"
	"time"

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

// Detail is one build's page: its steps with their status and timing, and
// its log.
func (p runBuildsProvider) Detail(_ context.Context, project string, path []string) (console.Detail, error) {
	if len(path) > 1 {
		return console.DeeperThan(1, path), nil
	}
	if project == "" {
		project = p.defaultProject
	}
	var b *cbpb.Build
	for _, x := range runadapter.CloudBuilds(project, "") {
		if x.GetId() == path[0] {
			b = x
		}
	}
	if b == nil {
		return console.Detail{Unavailable: "build " + path[0] + " not found"}, nil
	}
	summary := []console.Property{{Label: "Status", Value: b.GetStatus().String()}, {Label: "Name", Value: b.GetName()}}
	add := func(label, v string) {
		if v != "" {
			summary = append(summary, console.Property{Label: label, Value: v})
		}
	}
	if s := b.GetSource().GetStorageSource(); s != nil {
		add("Source", "gs://"+s.GetBucket()+"/"+s.GetObject())
	}
	add("Images", strings.Join(b.GetImages(), ", "))
	add("Logs bucket", b.GetLogsBucket())
	add("Status detail", b.GetStatusDetail())
	if b.GetStartTime() != nil && b.GetFinishTime() != nil {
		add("Duration", b.GetFinishTime().AsTime().Sub(b.GetStartTime().AsTime()).Round(time.Millisecond).String())
	}
	steps := console.Listing{Columns: []string{"Name", "Args", "Duration"}, NameColumn: "Step", Noun: "steps", AlwaysStatus: true}
	for i, st := range b.GetSteps() {
		dur := "-"
		if t := st.GetTiming(); t.GetStartTime() != nil && t.GetEndTime() != nil {
			dur = t.GetEndTime().AsTime().Sub(t.GetStartTime().AsTime()).Round(time.Millisecond).String()
		}
		label := fmt.Sprintf("#%d", i)
		if st.GetId() != "" {
			label += " " + st.GetId()
		}
		steps.Items = append(steps.Items, console.Resource{Name: label, Status: buildStatus(&cbpb.Build{Status: st.GetStatus()}),
			Fields: map[string]string{"Name": st.GetName(), "Args": strings.Join(st.GetArgs(), " "), "Duration": dur}})
	}
	steps.Total = len(steps.Items)
	log, ok := runadapter.BuildLog(b.GetName())
	if !ok {
		log = b.GetStatusDetail()
	}
	if log == "" {
		log = "(no log yet)"
	}
	return console.Detail{Summary: summary, Sections: []console.Section{
		{ID: "steps", Label: "Steps", Listing: steps},
		{ID: "log", Label: "Build log", Kind: console.KindText, Text: log},
	}}, nil
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
	_ console.Driller  = runBuildsProvider{}
)
