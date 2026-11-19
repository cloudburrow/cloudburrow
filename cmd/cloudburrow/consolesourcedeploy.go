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
// runAddr is the Run port, where Create repository and Edit repository
// reach the Artifact Registry API as an SDK does.
type artifactRegistryProvider struct {
	defaultProject string
	runAddr        func() string
}

func (artifactRegistryProvider) ID() string    { return "artifact-registry" }
func (artifactRegistryProvider) Title() string { return "Artifact Registry repositories" }

func (p artifactRegistryProvider) List(_ context.Context, project string) (console.Listing, error) {
	out := console.Listing{Columns: []string{"Format", "Location", "URL", "Description", "Age"},
		NameColumn: "Repository", Noun: "repositories", RowsOpenable: true}
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
	// hostSteps is --cloudbuild-host-steps (#1382): whether CreateBuild runs
	// steps other than docker build, docker push and pack. The history says
	// which, and why.
	hostSteps bool
}

// buildStepsNote is what the build history says about arbitrary steps
// (#1382, ADR-0004).
func buildStepsNote(on bool) string {
	if on {
		return "Build steps other than docker build, docker push and pack are on (--cloudbuild-host-steps): each runs as a " +
			"container on the host's Docker with the host's Docker socket mounted, so they run only for builds submitted " +
			"from this machine's loopback, never from a pod."
	}
	return "Build steps other than docker build, docker push and pack are off: each would run as a container on the " +
		"host's Docker with the host's Docker socket mounted, which gives it control of the host, so a build with one is " +
		"refused. Start the instance with --cloudbuild-host-steps to run them, for builds submitted from this machine's loopback only."
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
		{Name: "source", Label: "Source", Type: "text", Required: true, Pattern: "^gs://[^\\/]+/.+$",
			Help: "gs://bucket/object: a .tar.gz or .zip in this instance's Cloud Storage, with a Dockerfile at its root"},
		{Name: "image", Label: "Image", Type: "text", Required: true,
			Help: "the image to build and push, e.g. us-central1-docker.pkg.dev/PROJECT/REPO/IMAGE:TAG; a Cloud Run service naming it runs it"},
		{Name: "region", Label: "Region", Type: "text", Default: "global", Help: "global, or a region such as us-central1"},
		{Name: "substitutions", Label: "Substitutions", Type: "text",
			Help: "optional: _NAME=VALUE pairs, comma separated, which the image may name as ${_NAME}"},
		{Name: "dynamic", Label: "Dynamic substitutions", Type: "checkbox",
			Help: "options.dynamicSubstitutions: substitutions may name others, and bash expansions such as ${_VER#v} apply"},
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
	subs := map[string]string{}
	for _, kv := range strings.Split(values["substitutions"], ",") {
		if kv = strings.TrimSpace(kv); kv == "" {
			continue
		}
		k, v, ok := strings.Cut(kv, "=")
		if !ok || !strings.HasPrefix(k, "_") {
			return "", fmt.Errorf("substitution %q must be _NAME=VALUE", kv)
		}
		subs[k] = v
	}
	var id string
	err := p.withClient(ctx, func(c *cloudbuild.Client) error {
		op, err := c.CreateBuild(ctx, &cbpb.CreateBuildRequest{ProjectId: project,
			Parent: "projects/" + project + "/locations/" + region, Build: &cbpb.Build{
				Source:        &cbpb.Source{Source: &cbpb.Source_StorageSource{StorageSource: &cbpb.StorageSource{Bucket: bucket, Object: object}}},
				Steps:         []*cbpb.BuildStep{{Name: "gcr.io/cloud-builders/docker", Args: []string{"build", "-t", image, "."}}},
				Substitutions: subs, Options: &cbpb.BuildOptions{DynamicSubstitutions: values["dynamic"] == "true" || values["dynamic"] == "on"},
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

// DetailActions offers Cancel on a build that has not finished, and Retry
// on a finished one CreateBuild created (#1382), as Google's build page does.
func (p runBuildsProvider) DetailActions(_ context.Context, project string, path []string) []console.Action {
	if len(path) != 1 {
		return nil
	}
	b := p.build(project, path[0])
	switch {
	case b == nil:
		return nil
	case b.GetStatus() == cbpb.Build_PENDING && b.GetApproval().GetState() == cbpb.BuildApproval_PENDING:
		// Google's Approve and Reject dialogs: an optional message, and a
		// URL under "Show additional options" (#1854).
		decision := []console.Field{
			{Name: "comment", Label: "Message", Type: "textarea", Help: "Optional: recorded on the build's approval."},
			{Name: "url", Label: "URL", Type: "text", Pattern: `^https?://\S+$`, Section: "Show additional options",
				Help: "Optional: a link to more on the decision, such as a review."}}
		return []console.Action{
			{ID: "approve", Label: "Approve", Fields: decision},
			{ID: "reject", Label: "Reject", Fields: decision, Destructive: true},
			{ID: "cancel", Label: "Cancel", Confirm: "the build never runs and is CANCELLED"},
		}
	case b.GetStatus() == cbpb.Build_QUEUED || b.GetStatus() == cbpb.Build_WORKING:
		return []console.Action{{ID: "cancel", Label: "Cancel", Confirm: "the build stops and is CANCELLED"}}
	case runadapter.Retryable(b.GetName()):
		return []console.Action{{ID: "retry", Label: "Rebuild", Confirm: "a new build, with a new ID, runs the same source and steps"}}
	}
	return nil
}

// ActAt cancels the build through CancelBuild, or reruns it through
// RetryBuild.
func (p runBuildsProvider) ActAt(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	if (action != "cancel" && action != "retry" && action != "approve" && action != "reject") || len(path) != 1 {
		return fmt.Errorf("unknown action %q", action)
	}
	b := p.build(project, path[0])
	if b == nil {
		return fmt.Errorf("build %s not found", path[0])
	}
	return p.withClient(ctx, func(c *cloudbuild.Client) error {
		if action == "approve" || action == "reject" {
			// ApproveBuild (#1449), as gcloud builds approve and reject do.
			d := cbpb.ApprovalResult_APPROVED
			if action == "reject" {
				d = cbpb.ApprovalResult_REJECTED
			}
			op, err := c.ApproveBuild(ctx, &cbpb.ApproveBuildRequest{Name: b.GetName(),
				ApprovalResult: &cbpb.ApprovalResult{Decision: d, Comment: values["comment"], Url: strings.TrimSpace(values["url"])}})
			if err != nil {
				return err
			}
			_, err = op.Wait(ctx)
			return err
		}
		if action == "retry" {
			_, err := c.RetryBuild(ctx, &cbpb.RetryBuildRequest{Name: b.GetName(), ProjectId: b.GetProjectId(), Id: b.GetId()})
			return err
		}
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
func (runBuildsProvider) Title() string { return "Cloud Build history" }

func (p runBuildsProvider) List(ctx context.Context, project string) (console.Listing, error) {
	return p.ListFiltered(ctx, project, "")
}

const buildsFilterHelp = `ListBuilds' filter, as "Filtering build results" documents: status, build_id, trigger_id, tags, images, ` +
	`results.images.name, results.images.digest, source.storage_source.bucket/object, create_time, start_time, finish_time; ` +
	`= and != (and > >= < <= for times, RFC 3339), combined with AND, OR, NOT and parentheses`

// ListFiltered is the build history narrowed by a ListBuilds filter
// (#1382), evaluated by the same code as the API. Its columns are
// Google's History columns (#1854), then the region and image.
func (p runBuildsProvider) ListFiltered(_ context.Context, project, filter string) (console.Listing, error) {
	out := console.Listing{Columns: []string{"Source", "Ref", "Commit", "Trigger name", "Created", "Duration", "Region", "Image"},
		NameColumn: "Build", Noun: "builds", AlwaysStatus: true, RowsOpenable: true, Note: buildStepsNote(p.hostSteps),
		ServerFilter: &console.ServerFilter{Value: filter, Placeholder: `status="FAILURE" OR status="TIMEOUT"`, Help: buildsFilterHelp}}
	if project == "" {
		project = p.defaultProject
	}
	builds, err := runadapter.ListBuildsFiltered(project, "", filter)
	if err != nil {
		out.ServerFilter.Error = err.Error()
		return out, nil
	}
	triggers := map[string]string{}
	for _, t := range runadapter.Triggers(project, "") {
		triggers[t.GetId()] = t.GetName()
	}
	for _, b := range builds {
		image := ""
		if len(b.GetImages()) > 0 {
			image = b.GetImages()[0]
		}
		subs := b.GetSubstitutions()
		ref := subs["BRANCH_NAME"]
		if ref == "" {
			ref = subs["TAG_NAME"]
		}
		out.Items = append(out.Items, console.Resource{Name: b.GetId(), Status: buildStatus(b), Fields: map[string]string{
			"Source": orDash(buildSource(b)), "Ref": orDash(ref), "Commit": orDash(subs["SHORT_SHA"]),
			"Trigger name": orDash(triggers[b.GetBuildTriggerId()]), "Created": stampAge(b.GetCreateTime()),
			"Duration": orDash(buildDuration(b)), "Region": buildRegion(b), "Image": orDash(image)}})
	}
	out.Total = len(out.Items)
	return out, nil
}

// buildRegion is a build's location, global for the global form.
func buildRegion(b *cbpb.Build) string {
	if parts := strings.Split(b.GetName(), "/"); len(parts) == 6 {
		return parts[3]
	}
	return "global"
}

// buildSource is a build's Source column: its Cloud Storage archive or its
// repository.
func buildSource(b *cbpb.Build) string {
	if s := b.GetSource().GetStorageSource(); s != nil {
		return "gs://" + s.GetBucket() + "/" + s.GetObject()
	}
	if g := b.GetSource().GetGitSource(); g != nil {
		u := strings.TrimSuffix(g.GetUrl(), ".git")
		return u[strings.LastIndex(u, "/")+1:]
	}
	return ""
}

// buildDuration is how long a build ran, or has run so far.
func buildDuration(b *cbpb.Build) string {
	if b.GetStartTime() == nil {
		return ""
	}
	end := time.Now()
	if b.GetFinishTime() != nil {
		end = b.GetFinishTime().AsTime()
	}
	return end.Sub(b.GetStartTime().AsTime()).Round(time.Second).String()
}

// Detail is one build's page, with Google's views (#1854): Build summary
// (the steps; a step opens its own log), Build log, Execution details and
// Build artifacts.
func (p runBuildsProvider) Detail(_ context.Context, project string, path []string) (console.Detail, error) {
	if len(path) > 2 {
		return console.DeeperThan(2, path), nil
	}
	b := p.build(project, path[0])
	if b == nil {
		return console.Detail{Unavailable: "build " + path[0] + " not found"}, nil
	}
	log, ok := runadapter.BuildLog(b.GetName())
	if !ok {
		log = b.GetStatusDetail()
	}
	if len(path) == 2 {
		return buildStepPage(b, path[1], log), nil
	}
	summary := []console.Property{{Label: "Status", Value: b.GetStatus().String()}, {Label: "Name", Value: b.GetName()}}
	add := func(label, v string) {
		if v != "" {
			summary = append(summary, console.Property{Label: label, Value: v})
		}
	}
	add("Source", buildSource(b))
	add("Images", strings.Join(b.GetImages(), ", "))
	add("Log write error", runadapter.BuildLogError(b.GetName()))
	add("Status detail", b.GetStatusDetail())
	if a := b.GetApproval(); a != nil {
		v := a.GetState().String()
		if c := a.GetResult().GetComment(); c != "" {
			v += ": " + c
		}
		add("Approval", v)
		add("Approval URL", a.GetResult().GetUrl())
	}
	add("Duration", buildDuration(b))
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
			Opens:  []string{b.GetId(), fmt.Sprint(i)},
			Fields: map[string]string{"Name": st.GetName(), "Args": strings.Join(st.GetArgs(), " "), "Duration": dur}})
	}
	steps.Total = len(steps.Items)
	if log == "" {
		log = "(no log yet)"
	}
	return console.Detail{Summary: summary, Sections: []console.Section{
		{ID: "summary", Label: "Build summary", Listing: steps, Note: "Select a step to see its log."},
		{ID: "log", Label: "Build log", Kind: console.KindText, Text: log},
		{ID: "execution", Label: "Execution details", Kind: console.KindProperties, Groups: buildExecution(b)},
		{ID: "artifacts", Label: "Build artifacts", Listing: buildArtifacts(b)},
	}}, nil
}

// buildStepPage is one step's log: the build log's lines that step wrote,
// as Google's Build summary shows a selected step's.
func buildStepPage(b *cbpb.Build, step, log string) console.Detail {
	var i int
	if _, err := fmt.Sscan(step, &i); err != nil || i < 0 || i >= len(b.GetSteps()) || fmt.Sprint(i) != step {
		return console.Detail{Unavailable: "build " + b.GetId() + " has no step " + step}
	}
	prefix := fmt.Sprintf("Step #%d", i)
	var lines []string
	for _, l := range strings.Split(log, "\n") {
		if l == "Starting "+prefix || strings.HasPrefix(l, "Starting "+prefix+" ") ||
			strings.HasPrefix(l, prefix+":") || strings.HasPrefix(l, prefix+" ") || l == "Finished "+prefix ||
			strings.HasPrefix(l, "Finished "+prefix+" ") {
			lines = append(lines, l)
		}
	}
	text := strings.Join(lines, "\n")
	if text == "" {
		text = "(no log for this step yet)"
	}
	st := b.GetSteps()[i]
	title := prefix
	if st.GetId() != "" {
		title += " - " + st.GetId()
	}
	return console.Detail{Title: title, Summary: []console.Property{{Label: "Status", Value: st.GetStatus().String()},
		{Label: "Name", Value: st.GetName()}, {Label: "Args", Value: strings.Join(st.GetArgs(), " ")}},
		Trail:    []console.Crumb{{Label: b.GetId(), Path: []string{b.GetId()}}},
		Sections: []console.Section{{ID: "log", Label: "Step log", Kind: console.KindText, Text: text}}}
}

// buildExecution is the Execution details view: what the build ran with.
func buildExecution(b *cbpb.Build) []console.PropertyGroup {
	o := b.GetOptions()
	run := []console.Property{{Label: "Build ID", Value: b.GetId()}, {Label: "Region", Value: buildRegion(b)},
		{Label: "Trigger", Value: b.GetBuildTriggerId()}, {Label: "Created", Value: stampAge(b.GetCreateTime())},
		{Label: "Duration", Value: buildDuration(b)}, {Label: "Logs bucket", Value: b.GetLogsBucket()},
		{Label: "Service account", Value: b.GetServiceAccount()}, {Label: "Private pool", Value: o.GetPool().GetName()},
		{Label: "Machine type", Value: o.GetMachineType().String()}, {Label: "Logging", Value: o.GetLogging().String()}}
	if b.GetTimeout() != nil {
		run = append(run, console.Property{Label: "Timeout", Value: b.GetTimeout().AsDuration().String()})
	}
	if o.GetDynamicSubstitutions() {
		run = append(run, console.Property{Label: "Dynamic substitutions", Value: "on"})
	}
	groups := []console.PropertyGroup{{Heading: "Execution details", Properties: run}}
	if subs := b.GetSubstitutions(); len(subs) > 0 {
		var ps []console.Property
		for _, k := range sortedKeys(subs) {
			ps = append(ps, console.Property{Label: k, Value: subs[k]})
		}
		groups = append(groups, console.PropertyGroup{Heading: "Substitutions", Properties: ps})
	}
	if env := o.GetEnv(); len(env) > 0 {
		var ps []console.Property
		for _, e := range env {
			k, v, _ := strings.Cut(e, "=")
			ps = append(ps, console.Property{Label: k, Value: v})
		}
		groups = append(groups, console.PropertyGroup{Heading: "Environment variables", Properties: ps})
	}
	return groups
}

// buildArtifacts is the Build artifacts view: the images the build pushed,
// with their digests, and the images it names that it has not pushed yet.
func buildArtifacts(b *cbpb.Build) console.Listing {
	out := console.Listing{Columns: []string{"Type", "Digest"}, NameColumn: "Artifact", Noun: "artifacts"}
	pushed := map[string]bool{}
	for _, img := range b.GetResults().GetImages() {
		pushed[img.GetName()] = true
		out.Items = append(out.Items, console.Resource{Name: img.GetName(), Fields: map[string]string{"Type": "Image", "Digest": orDash(img.GetDigest())}})
	}
	for _, img := range b.GetImages() {
		if !pushed[img] {
			out.Items = append(out.Items, console.Resource{Name: img, Fields: map[string]string{"Type": "Image", "Digest": "-"}})
		}
	}
	if loc := b.GetArtifacts().GetObjects().GetLocation(); loc != "" {
		for _, path := range b.GetArtifacts().GetObjects().GetPaths() {
			out.Items = append(out.Items, console.Resource{Name: strings.TrimSuffix(loc, "/") + "/" + path, Fields: map[string]string{"Type": "Object", "Digest": "-"}})
		}
	}
	out.Total = len(out.Items)
	return out
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
	case cbpb.Build_PENDING:
		return "Awaiting approval"
	}
	return "Queued"
}

var (
	_ console.Provider     = artifactRegistryProvider{}
	_ console.Provider     = runBuildsProvider{}
	_ console.Driller      = runBuildsProvider{}
	_ console.Creator      = runBuildsProvider{}
	_ console.PathActor    = runBuildsProvider{}
	_ console.FilterLister = runBuildsProvider{}
)
