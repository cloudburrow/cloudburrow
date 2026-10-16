package main

// Artifact Registry repositories and Cloud Build's build history (#1347):
// what `gcloud run deploy --source` creates. Both read the stores the
// adapter's ArtifactRegistry and CloudBuild APIs serve, in this process.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	cloudbuild "cloud.google.com/go/cloudbuild/apiv1/v2"
	cbpb "cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

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

// Delete removes the project's repository of that ID, as DeleteRepository
// does (#1304). An ID in two locations is refused: the page names
// repositories by ID alone.
func (p artifactRegistryProvider) Delete(_ context.Context, project, name string) error {
	if project == "" {
		project = p.defaultProject
	}
	var match []string
	for _, r := range runadapter.Repositories() {
		parts := strings.Split(r.GetName(), "/")
		if len(parts) == 6 && parts[1] == project && parts[5] == name {
			match = append(match, r.GetName())
		}
	}
	switch len(match) {
	case 0:
		return fmt.Errorf("repository %q not found in project %s", name, project)
	case 1:
		if !runadapter.RemoveRepository(match[0]) {
			return fmt.Errorf("repository %q not found in project %s", name, project)
		}
		return nil
	default:
		return fmt.Errorf("repository %q is in %d locations; delete it with gcloud artifacts repositories delete --location", name, len(match))
	}
}

// runBuildsProvider is Cloud Run's build history: every source build,
// newest first, with its status, image and digest.
type runBuildsProvider struct {
	defaultProject string
	// runAddr is the Run port, where Cloud Build is served; the console's
	// Run build and Cancel go through the official client there.
	runAddr func() string
}

// withClient dials Cloud Build on the Run port as an SDK would.
func (p runBuildsProvider) withClient(ctx context.Context, fn func(*cloudbuild.Client) error) error {
	addr := ""
	if p.runAddr != nil {
		addr = p.runAddr()
	}
	if addr == "" {
		return errors.New("the Cloud Run adapter is not running")
	}
	c, err := cloudbuild.NewClient(ctx, option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		return fmt.Errorf("connect to Cloud Build: %w", err)
	}
	defer func() { _ = c.Close() }()
	return fn(c)
}

// CreateForm is Run build: a Dockerfile source in this instance's Cloud
// Storage, built and its image pushed (loaded into the cluster), as
// `gcloud builds submit --tag` does.
func (runBuildsProvider) CreateForm() (string, []console.Field) {
	return "Run build", []console.Field{
		{Name: "source", Label: "Source", Type: "text", Required: true, Pattern: "^gs://[^/]+/.+$",
			Help: "gs://bucket/object: a .tar.gz or .zip in this instance's Cloud Storage, with a Dockerfile at its root"},
		{Name: "image", Label: "Image", Type: "text", Required: true,
			Help: "the image to build and push, e.g. us-central1-docker.pkg.dev/PROJECT/REPO/IMAGE:TAG; a Cloud Run service naming it runs it"},
		{Name: "region", Label: "Region", Type: "text", Default: "global", Help: "global, or a region such as us-central1"},
	}
}

// Create runs CreateBuild with a docker build step and the image, and
// returns the build's ID.
func (p runBuildsProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	if project == "" {
		project = p.defaultProject
	}
	bucket, object, ok := strings.Cut(strings.TrimPrefix(values["source"], "gs://"), "/")
	if !ok || !strings.HasPrefix(values["source"], "gs://") || object == "" {
		return "", fmt.Errorf("source must be gs://bucket/object")
	}
	image, region := strings.TrimSpace(values["image"]), strings.TrimSpace(values["region"])
	if region == "" {
		region = "global"
	}
	var id string
	err := p.withClient(ctx, func(c *cloudbuild.Client) error {
		op, err := c.CreateBuild(ctx, &cbpb.CreateBuildRequest{ProjectId: project,
			Parent: "projects/" + project + "/locations/" + region, Build: &cbpb.Build{
				Source: &cbpb.Source{Source: &cbpb.Source_StorageSource{StorageSource: &cbpb.StorageSource{Bucket: bucket, Object: object}}},
				Steps:  []*cbpb.BuildStep{{Name: "gcr.io/cloud-builders/docker", Args: []string{"build", "-t", image, "."}}},
				Images: []string{image}}})
		if err != nil {
			return err
		}
		md, err := op.Metadata()
		if err != nil {
			return err
		}
		id = md.GetBuild().GetId()
		return nil
	})
	return id, err
}

// DetailActions offers Cancel on a build that has not finished.
func (p runBuildsProvider) DetailActions(_ context.Context, project string, path []string) []console.Action {
	if len(path) != 1 {
		return nil
	}
	if b := p.build(project, path[0]); b != nil && (b.GetStatus() == cbpb.Build_QUEUED || b.GetStatus() == cbpb.Build_WORKING) {
		return []console.Action{{ID: "cancel", Label: "Cancel", Confirm: "the build stops and is CANCELLED"}}
	}
	return nil
}

// ActAt cancels the build through CancelBuild.
func (p runBuildsProvider) ActAt(ctx context.Context, project string, path []string, action string, _ map[string]string) error {
	if action != "cancel" || len(path) != 1 {
		return fmt.Errorf("unknown action %q", action)
	}
	b := p.build(project, path[0])
	if b == nil {
		return fmt.Errorf("build %s not found", path[0])
	}
	return p.withClient(ctx, func(c *cloudbuild.Client) error {
		_, err := c.CancelBuild(ctx, &cbpb.CancelBuildRequest{Name: b.GetName(), ProjectId: b.GetProjectId(), Id: b.GetId()})
		return err
	})
}

// build finds a project's build by ID.
func (p runBuildsProvider) build(project, id string) *cbpb.Build {
	if project == "" {
		project = p.defaultProject
	}
	for _, x := range runadapter.CloudBuilds(project, "") {
		if x.GetId() == id {
			return x
		}
	}
	return nil
}

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
	b := p.build(project, path[0])
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
	_ console.Provider  = artifactRegistryProvider{}
	_ console.Provider  = runBuildsProvider{}
	_ console.Driller   = runBuildsProvider{}
	_ console.Creator   = runBuildsProvider{}
	_ console.PathActor = runBuildsProvider{}
)
