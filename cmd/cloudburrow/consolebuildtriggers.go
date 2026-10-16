package main

// Cloud Build's Triggers and Worker pools pages (#1422): create, run and
// delete a trigger, create and delete a pool, through the official client
// on the Run port, as the API does.

import (
	"context"
	"fmt"
	"strings"

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

func (p buildTriggersProvider) List(_ context.Context, project string) (console.Listing, error) {
	out := console.Listing{Columns: []string{"Region", "Event", "Description", "Pool", "Approval", "Age"},
		NameColumn: "Trigger", Noun: "triggers", AlwaysStatus: true,
		Note: "Manual and webhook triggers run an inline build. GitHub, GitLab, Bitbucket and Cloud Source Repositories " +
			"events, and builds read from a repository, need a hosted git service and are refused."}
	for _, t := range runadapter.Triggers(p.project(project), "") {
		region := strings.Split(t.GetResourceName(), "/")[3]
		event, st := "Manual invocation", "Enabled"
		if t.GetWebhookConfig() != nil {
			event = "Webhook event"
		}
		if t.GetDisabled() {
			st = "Disabled"
		}
		out.Items = append(out.Items, console.Resource{Name: t.GetName(), Status: st, Fields: map[string]string{
			"Region": region, "Event": event, "Description": orDash(t.GetDescription()),
			"Pool": orDash(t.GetBuild().GetOptions().GetPool().GetName()), "Approval": approvalLabel(t.GetResourceName()),
			"Age": stampAge(t.GetCreateTime())}})
	}
	out.Total = len(out.Items)
	return out, nil
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

func (buildTriggersProvider) CreateForm() (string, []console.Field) {
	return "Create trigger", []console.Field{
		{Name: "name", Label: "Name", Type: "text", Required: true, Pattern: "^[a-zA-Z0-9][-a-zA-Z0-9_]{0,62}$"},
		{Name: "region", Label: "Region", Type: "text", Default: "global", Help: "global, or a region such as us-central1"},
		{Name: "description", Label: "Description", Type: "text"},
		{Name: "image", Label: "Step image", Type: "text", Required: true, Default: "alpine:3.20",
			Help: "the build's one step; a step other than docker build, docker push or pack runs only with --cloudbuild-host-steps"},
		{Name: "command", Label: "Command", Type: "text", Required: true, Default: "echo triggered by $TRIGGER_NAME",
			Help: "run with sh -c; $TRIGGER_NAME, $BUILD_ID and _NAME substitutions apply"},
		{Name: "pool", Label: "Worker pool", Type: "text", Help: "optional: projects/PROJECT/locations/REGION/workerPools/POOL"},
		{Name: "secret", Label: "Webhook secret", Type: "text",
			Help: "optional: a Secret Manager version, projects/PROJECT/secrets/SECRET/versions/N, which makes a webhook trigger"},
		{Name: "approval", Label: "Require approval", Type: "checkbox",
			Help: "approvalConfig.approvalRequired: each build waits, PENDING, until it is approved on its build page"},
	}
}

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
	b := &cbpb.Build{Steps: []*cbpb.BuildStep{{Name: strings.TrimSpace(v["image"]), Entrypoint: "sh", Args: []string{"-c", v["command"]}}}}
	if pool := strings.TrimSpace(v["pool"]); pool != "" {
		b.Options = &cbpb.BuildOptions{Pool: &cbpb.BuildOptions_PoolOption{Name: pool}}
	}
	t := &cbpb.BuildTrigger{Name: strings.TrimSpace(v["name"]), Description: v["description"], BuildTemplate: &cbpb.BuildTrigger_Build{Build: b}}
	if s := strings.TrimSpace(v["secret"]); s != "" {
		t.WebhookConfig = &cbpb.WebhookConfig{AuthMethod: &cbpb.WebhookConfig_Secret{Secret: s}}
	}
	err := p.b.withClient(ctx, func(c *cloudbuild.Client) error {
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
	if r.Status == "Enabled" && r.Fields["Event"] == "Manual invocation" {
		return []console.Action{{ID: "run", Label: "Run", Confirm: "a build runs from the trigger's build config"}}
	}
	return nil
}

func (p buildTriggersProvider) Act(ctx context.Context, project, name, action string) error {
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
		NameColumn: "Pool", Noun: "worker pools", AlwaysStatus: true,
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
		{Name: "name", Label: "Name", Type: "text", Required: true, Pattern: "^[a-z]([-a-z0-9]{0,61}[a-z0-9])?$"},
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
	_ console.Creator = buildTriggersProvider{}
	_ console.Actor   = buildTriggersProvider{}
	_ console.Deleter = buildTriggersProvider{}
	_ console.Creator = buildWorkerPoolsProvider{}
	_ console.Deleter = buildWorkerPoolsProvider{}
)
