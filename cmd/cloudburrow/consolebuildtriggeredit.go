package main

// A Cloud Build trigger's page: what GetBuildTrigger reads of it, with
// Edit trigger (UpdateBuildTrigger) and Disable / Enable, which had no
// console surface (the console parity audit).

import (
	"context"
	"fmt"
	"strings"

	cloudbuild "cloud.google.com/go/cloudbuild/apiv1/v2"
	cbpb "cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// get reads a trigger through GetBuildTrigger.
func (p buildTriggersProvider) get(ctx context.Context, project, name string) (*cbpb.BuildTrigger, error) {
	t, err := p.find(project, name)
	if err != nil {
		return nil, err
	}
	var got *cbpb.BuildTrigger
	err = p.b.withClient(ctx, func(c *cloudbuild.Client) error {
		var e error
		got, e = c.GetBuildTrigger(ctx, &cbpb.GetBuildTriggerRequest{Name: t.GetResourceName(), ProjectId: p.project(project), TriggerId: t.GetId()})
		return e
	})
	return got, err
}

// triggerStep is the trigger's one step's image and sh -c command, as
// Create trigger makes it; ok is false for a build of another shape.
func triggerStep(t *cbpb.BuildTrigger) (image, command string, ok bool) {
	steps := t.GetBuild().GetSteps()
	if len(steps) != 1 {
		return "", "", false
	}
	s := steps[0]
	if s.GetEntrypoint() != "sh" || len(s.GetArgs()) != 2 || s.GetArgs()[0] != "-c" {
		return s.GetName(), "", false
	}
	return s.GetName(), s.GetArgs()[1], true
}

// Detail implements console.Driller: a trigger's page.
func (p buildTriggersProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) > 1 {
		return console.DeeperThan(1, path), nil
	}
	t, err := p.get(ctx, project, path[0])
	if err != nil {
		return console.Detail{Unavailable: consoleErr(err)}, nil
	}
	st, event := "Enabled", "Manual invocation"
	if t.GetDisabled() {
		st = "Disabled"
	}
	if t.GetWebhookConfig() != nil {
		event = "Webhook event"
	}
	image, command, _ := triggerStep(t)
	props := []console.Property{{Label: "Resource name", Value: t.GetResourceName()}, {Label: "Status", Value: st},
		{Label: "Event", Value: event}, {Label: "Description", Value: t.GetDescription()},
		{Label: "Step image", Value: image}, {Label: "Command", Value: command},
		{Label: "Worker pool", Value: t.GetBuild().GetOptions().GetPool().GetName()},
		{Label: "Approval", Value: approvalLabel(t.GetResourceName())},
		{Label: "Webhook secret", Value: t.GetWebhookConfig().GetSecret()},
		{Label: "Created", Value: stampAge(t.GetCreateTime())}}
	var shown []console.Property
	for _, pr := range props {
		if pr.Value != "" && pr.Value != "-" {
			shown = append(shown, pr)
		}
	}
	return console.Detail{Summary: shown}, nil
}

// DetailActions implements console.PathActor: Edit trigger, prefilled
// from the trigger, Disable or Enable, and Run on an enabled manual one.
func (p buildTriggersProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if len(path) != 1 {
		return nil
	}
	t, err := p.get(ctx, project, path[0])
	if err != nil {
		return nil
	}
	var acts []console.Action
	if image, command, ok := triggerStep(t); ok {
		acts = append(acts, console.Action{ID: "edit-trigger", Label: "Edit trigger", Fields: []console.Field{
			{Name: "description", Label: "Description", Type: "text", Default: t.GetDescription()},
			{Name: "image", Label: "Step image", Type: "text", Required: true, Default: image},
			{Name: "command", Label: "Command", Type: "text", Required: true, Default: command,
				Help: "run with sh -c; $TRIGGER_NAME, $BUILD_ID and _NAME substitutions apply"},
			{Name: "pool", Label: "Worker pool", Type: "text", Default: t.GetBuild().GetOptions().GetPool().GetName(),
				Help: "optional: projects/PROJECT/locations/REGION/workerPools/POOL; empty runs on the default pool"}}})
	}
	if t.GetDisabled() {
		acts = append(acts, console.Action{ID: "enable-trigger", Label: "Enable"})
	} else {
		acts = append(acts, console.Action{ID: "disable-trigger", Label: "Disable",
			Confirm: "A disabled trigger starts no build, from Run or from its webhook, until it is enabled."})
		if t.GetWebhookConfig() == nil {
			acts = append(acts, console.Action{ID: "run", Label: "Run", Confirm: "a build runs from the trigger's build config"})
		}
	}
	return acts
}

// ActAt implements console.PathActor.
func (p buildTriggersProvider) ActAt(ctx context.Context, project string, path []string, action string, v map[string]string) error {
	if len(path) != 1 {
		return fmt.Errorf("unknown action %q", action)
	}
	if action == "run" {
		return p.Act(ctx, project, path[0], action)
	}
	t, err := p.get(ctx, project, path[0])
	if err != nil {
		return err
	}
	next := &cbpb.BuildTrigger{ResourceName: t.GetResourceName()}
	var mask []string
	switch action {
	case "enable-trigger", "disable-trigger":
		next.Disabled, mask = action == "disable-trigger", []string{"disabled"}
	case "edit-trigger":
		image, command := strings.TrimSpace(v["image"]), v["command"]
		if image == "" || strings.TrimSpace(command) == "" {
			return fmt.Errorf("a step image and a command are required")
		}
		b := &cbpb.Build{Steps: []*cbpb.BuildStep{{Name: image, Entrypoint: "sh", Args: []string{"-c", command}}}}
		if old := t.GetBuild(); old != nil {
			b.Substitutions, b.Timeout = old.GetSubstitutions(), old.GetTimeout()
		}
		if pool := strings.TrimSpace(v["pool"]); pool != "" {
			b.Options = &cbpb.BuildOptions{Pool: &cbpb.BuildOptions_PoolOption{Name: pool}}
		}
		next.Description, next.BuildTemplate = strings.TrimSpace(v["description"]), &cbpb.BuildTrigger_Build{Build: b}
		mask = []string{"description", "build"}
	default:
		return fmt.Errorf("unknown action %q", action)
	}
	return p.b.withClient(ctx, func(c *cloudbuild.Client) error {
		_, err := c.UpdateBuildTrigger(ctx, &cbpb.UpdateBuildTriggerRequest{ProjectId: p.project(project), TriggerId: t.GetId(),
			Trigger: next, UpdateMask: &fieldmaskpb.FieldMask{Paths: mask}})
		return err
	})
}
