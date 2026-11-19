package main

// Cloud Build's Triggers and Worker pools pages (#1422): create, run and
// delete a trigger, create and delete a pool, through the official client
// on the Run port, as the API does.

import (
	"context"
	"fmt"
	"strings"
	"time"

	cloudbuild "cloud.google.com/go/cloudbuild/apiv1/v2"
	cbpb "cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"

	runadapter "github.com/cloudburrow/cloudburrow/internal/adapter/run"
	"github.com/cloudburrow/cloudburrow/internal/console"
)

// buildTriggersProvider is the Triggers page.
type buildTriggersProvider struct{ b runBuildsProvider }

func (buildTriggersProvider) ID() string    { return "cloud-build-triggers" }
func (buildTriggersProvider) Title() string { return "Cloud Build triggers" }

func (p buildTriggersProvider) project(project string) string {
	if project == "" {
		return p.b.defaultProject
	}
	return project
}

func (p buildTriggersProvider) List(ctx context.Context, project string) (console.Listing, error) {
	out := console.Listing{Columns: []string{"Description", "Region", "Repository", "Event", "Build configuration", "Pool", "Approval", "Age"},
		NameColumn: "Name", Noun: "triggers", AlwaysStatus: true, RowsOpenable: true,
		Note: "Push to a branch and Push new tag triggers run on a Developer Connect link to one of this instance's Secure Source Manager " +
			"repositories; manual, webhook and Pub/Sub triggers run an inline build. A Pub/Sub trigger's builds, which arrive from the " +
			"cluster, run docker build, docker push and pack steps only. GitHub, GitLab and Bitbucket need a hosted git service."}
	for _, t := range runadapter.Triggers(p.project(project), "") {
		region := strings.Split(t.GetResourceName(), "/")[3]
		st := "Enabled"
		if t.GetDisabled() {
			st = "Disabled"
		}
		repo := "-"
		if r := t.GetRepositoryEventConfig().GetRepository(); r != "" {
			repo = r[strings.LastIndex(r, "/")+1:]
		}
		out.Items = append(out.Items, console.Resource{Name: t.GetName(), Status: st, Fields: map[string]string{
			"Region": region, "Event": triggerEvent(t), "Description": orDash(t.GetDescription()), "Repository": repo,
			"Build configuration": triggerConfigLabel(t),
			"Pool":                orDash(t.GetBuild().GetOptions().GetPool().GetName()), "Approval": approvalLabel(t.GetResourceName()),
			"Age": stampAge(t.GetCreateTime())}})
	}
	out.Total = len(out.Items)
	return out, nil
}

// triggerEvent is the Event column, in Google's words.
func triggerEvent(t *cbpb.BuildTrigger) string {
	switch {
	case t.GetPubsubConfig() != nil:
		return "Pub/Sub message: " + t.GetPubsubConfig().GetTopic()
	case t.GetWebhookConfig() != nil:
		return "Webhook event"
	}
	// A trigger on a Developer Connect link (#1852).
	if push := t.GetRepositoryEventConfig().GetPush(); push != nil {
		if push.GetTag() != "" {
			return "Push new tag: " + push.GetTag()
		}
		return "Push to branch: " + push.GetBranch()
	}
	return "Manual invocation"
}

// triggerConfigLabel is the Build configuration column.
func triggerConfigLabel(t *cbpb.BuildTrigger) string {
	switch {
	case t.GetAutodetect():
		return "Autodetected"
	case t.GetFilename() != "":
		return "Cloud Build configuration file: " + t.GetFilename()
	}
	return "Inline"
}

// find resolves a trigger by its name, which may be in any region.
func (p buildTriggersProvider) find(project, name string) (*cbpb.BuildTrigger, error) {
	var match []*cbpb.BuildTrigger
	for _, t := range runadapter.Triggers(p.project(project), "") {
		if t.GetName() == name {
			match = append(match, t)
		}
	}
	switch len(match) {
	case 0:
		return nil, fmt.Errorf("trigger %q not found", name)
	case 1:
		return match[0], nil
	}
	return nil, fmt.Errorf("trigger %q is in %d regions; use gcloud builds triggers --region", name, len(match))
}

// CreateForm is Google's Create trigger page (consolebuildtriggerform.go).
func (p buildTriggersProvider) CreateForm() (string, []console.Field) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return "Create trigger", p.triggerFields(ctx, "", nil)
}

// CreateOnPage, CreateLayout and CreateSubmit draw Create trigger as one
// page under its headings, ending in Create, as Google's is.
func (buildTriggersProvider) CreateOnPage() bool   { return true }
func (buildTriggersProvider) CreateLayout() string { return console.CreateLayoutPage }
func (buildTriggersProvider) CreateSubmit() string { return "Create" }
func (buildTriggersProvider) DeleteLabel() string  { return "Delete" }
func (buildTriggersProvider) RowEditLabel() string { return "Edit" }

// approvalLabel is the Approval column: whether the trigger's builds wait
// for approval (#1449).
func approvalLabel(resourceName string) string {
	if runadapter.TriggerNeedsApproval(resourceName) {
		return "Required"
	}
	return "-"
}

func (p buildTriggersProvider) Create(ctx context.Context, project string, v map[string]string) (string, error) {
	project = p.project(project)
	region := strings.TrimSpace(v["region"])
	if region == "" {
		region = "global"
	}
	t, err := triggerFromValues(v)
	if err != nil {
		return "", err
	}
	err = p.b.withClient(ctx, func(c *cloudbuild.Client) error {
		made, err := c.CreateBuildTrigger(ctx, &cbpb.CreateBuildTriggerRequest{ProjectId: project,
			Parent: "projects/" + project + "/locations/" + region, Trigger: t})
		if err != nil || (v["approval"] != "true" && v["approval"] != "on") {
			return err
		}
		// The Go client's protos have no approvalConfig; the JSON API
		// carries it, and the console sets it as that API does.
		return runadapter.SetTriggerApproval(made.GetResourceName(), true)
	})
	return t.GetName(), err
}

// Actions offers Run on an enabled manual trigger, as Google's Triggers
// page does; a webhook trigger runs from its URL.
func (p buildTriggersProvider) Actions(r console.Resource) []console.Action {
	var acts []console.Action
	ev := r.Fields["Event"]
	if r.Status == "Enabled" && (ev == "Manual invocation" || strings.HasPrefix(ev, "Push ")) {
		acts = append(acts, console.Action{ID: "run", Label: "Run", Confirm: "a build runs from the trigger's build config"})
	}
	// Disable and Enable in the row's menu (#1854), as Google's.
	if r.Status == "Disabled" {
		return append(acts, console.Action{ID: "enable-trigger", Label: "Enable"})
	}
	return append(acts, console.Action{ID: "disable-trigger", Label: "Disable",
		Confirm: "A disabled trigger starts no build, from Run, a push, its topic or its webhook, until it is enabled."})
}

func (p buildTriggersProvider) Act(ctx context.Context, project, name, action string) error {
	if action == "enable-trigger" || action == "disable-trigger" {
		return p.ActAt(ctx, project, []string{name}, action, nil)
	}
	if action != "run" {
		return fmt.Errorf("unknown action %q", action)
	}
	t, err := p.find(project, name)
	if err != nil {
		return err
	}
	return p.b.withClient(ctx, func(c *cloudbuild.Client) error {
		_, err := c.RunBuildTrigger(ctx, &cbpb.RunBuildTriggerRequest{Name: t.GetResourceName(), ProjectId: p.project(project), TriggerId: t.GetId()})
		return err
	})
}

func (p buildTriggersProvider) Delete(ctx context.Context, project, name string) error {
	t, err := p.find(project, name)
	if err != nil {
		return err
	}
	return p.b.withClient(ctx, func(c *cloudbuild.Client) error {
		return c.DeleteBuildTrigger(ctx, &cbpb.DeleteBuildTriggerRequest{Name: t.GetResourceName(), ProjectId: p.project(project), TriggerId: t.GetId()})
	})
}

// buildWorkerPoolsProvider is the Worker pools page.
type buildWorkerPoolsProvider struct{ b runBuildsProvider }

func (buildWorkerPoolsProvider) ID() string    { return "cloud-build-worker-pools" }
func (buildWorkerPoolsProvider) Title() string { return "Cloud Build worker pools" }

func (p buildWorkerPoolsProvider) project(project string) string {
	if project == "" {
		return p.b.defaultProject
	}
	return project
}

func (p buildWorkerPoolsProvider) List(_ context.Context, project string) (console.Listing, error) {
	out := console.Listing{Columns: []string{"Region", "Machine type", "Disk (GB)", "Display name", "Age"},
		NameColumn: "Pool", Noun: "worker pools", AlwaysStatus: true, RowsOpenable: true,
		Note: "A build that names a pool runs on the host's Docker, like every build here; the pool is recorded on the build."}
	for _, w := range runadapter.WorkerPools(p.project(project)) {
		parts := strings.Split(w.GetName(), "/")
		wc := w.GetPrivatePoolV1Config().GetWorkerConfig()
		disk := "-"
		if wc.GetDiskSizeGb() != 0 {
			disk = fmt.Sprint(wc.GetDiskSizeGb())
		}
		st := "Running"
		if w.GetState() != cbpb.WorkerPool_RUNNING {
			st = w.GetState().String()
		}
		out.Items = append(out.Items, console.Resource{Name: parts[5], Status: st, Fields: map[string]string{
			"Region": parts[3], "Machine type": orDash(wc.GetMachineType()), "Disk (GB)": disk,
			"Display name": orDash(w.GetDisplayName()), "Age": stampAge(w.GetCreateTime())}})
	}
	out.Total = len(out.Items)
	return out, nil
}

func (buildWorkerPoolsProvider) CreateForm() (string, []console.Field) {
	return "Create worker pool", []console.Field{
		{Name: "name", Label: "Name", Type: "text", Required: true, Pattern: "^[a-z]([\\-a-z0-9]{0,61}[a-z0-9])?$"},
		{Name: "region", Label: "Region", Type: "text", Required: true, Default: "us-central1"},
		{Name: "machineType", Label: "Machine type", Type: "text", Default: "e2-standard-2"},
	}
}

func (p buildWorkerPoolsProvider) Create(ctx context.Context, project string, v map[string]string) (string, error) {
	project = p.project(project)
	name := strings.TrimSpace(v["name"])
	err := p.b.withClient(ctx, func(c *cloudbuild.Client) error {
		op, err := c.CreateWorkerPool(ctx, &cbpb.CreateWorkerPoolRequest{Parent: "projects/" + project + "/locations/" + strings.TrimSpace(v["region"]),
			WorkerPoolId: name, WorkerPool: &cbpb.WorkerPool{Config: &cbpb.WorkerPool_PrivatePoolV1Config{PrivatePoolV1Config: &cbpb.PrivatePoolV1Config{
				WorkerConfig: &cbpb.PrivatePoolV1Config_WorkerConfig{MachineType: strings.TrimSpace(v["machineType"])}}}}})
		if err != nil {
			return err
		}
		_, err = op.Wait(ctx)
		return err
	})
	return name, err
}

func (p buildWorkerPoolsProvider) Delete(ctx context.Context, project, name string) error {
	var match []string
	for _, w := range runadapter.WorkerPools(p.project(project)) {
		if strings.HasSuffix(w.GetName(), "/workerPools/"+name) {
			match = append(match, w.GetName())
		}
	}
	if len(match) != 1 {
		return fmt.Errorf("worker pool %q is in %d regions; use gcloud builds worker-pools delete --region", name, len(match))
	}
	return p.b.withClient(ctx, func(c *cloudbuild.Client) error {
		op, err := c.DeleteWorkerPool(ctx, &cbpb.DeleteWorkerPoolRequest{Name: match[0]})
		if err != nil {
			return err
		}
		return op.Wait(ctx)
	})
}

var (
	_ console.CreateLayouter  = buildTriggersProvider{}
	_ console.PageCreator     = buildTriggersProvider{}
	_ console.CreateSubmitter = buildTriggersProvider{}
	_ console.Actor           = buildTriggersProvider{}
	_ console.DeleteLabeler   = buildTriggersProvider{}
	_ console.RowEditor       = buildTriggersProvider{}
	_ console.Creator         = buildWorkerPoolsProvider{}
	_ console.Deleter         = buildWorkerPoolsProvider{}
)
