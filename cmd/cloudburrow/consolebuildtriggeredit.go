package main

// A Cloud Build trigger's page: what GetBuildTrigger reads of it, with
// Edit trigger (UpdateBuildTrigger) and Disable / Enable, which had no
// console surface (the console parity audit).

import (
	"context"
	"fmt"

	cloudbuild "cloud.google.com/go/cloudbuild/apiv1/v2"
	cbpb "cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	runadapter "github.com/cloudburrow/cloudburrow/internal/adapter/run"
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

// Detail implements console.Driller: a trigger's page, with its Edit form.
func (p buildTriggersProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) > 1 {
		return console.DeeperThan(1, path), nil
	}
	t, err := p.get(ctx, project, path[0])
	if err != nil {
		return console.Detail{Unavailable: consoleErr(err)}, nil
	}
	st := "Enabled"
	if t.GetDisabled() {
		st = "Disabled"
	}
	image, command, _ := triggerStep(t)
	props := []console.Property{{Label: "Resource name", Value: t.GetResourceName()}, {Label: "Status", Value: st},
		{Label: "Event", Value: triggerEvent(t)}, {Label: "Description", Value: t.GetDescription()},
		{Label: "Repository", Value: t.GetRepositoryEventConfig().GetRepository()},
		{Label: "Build configuration", Value: triggerConfigLabel(t)},
		{Label: "Step image", Value: image}, {Label: "Command", Value: command},
		{Label: "Worker pool", Value: t.GetBuild().GetOptions().GetPool().GetName()},
		{Label: "Approval", Value: approvalLabel(t.GetResourceName())},
		{Label: "Webhook secret", Value: t.GetWebhookConfig().GetSecret()},
		{Label: "Substitution variables", Value: console.FormatMap(t.GetSubstitutions())},
		{Label: "Created", Value: stampAge(t.GetCreateTime())}}
	var shown []console.Property
	for _, pr := range props {
		if pr.Value != "" && pr.Value != "-" {
			shown = append(shown, pr)
		}
	}
	return console.Detail{Summary: shown, Edit: &console.EditForm{Label: "Edit", Title: "Edit trigger", Submit: "Save",
		Fields: p.triggerFields(ctx, project, t), Note: "The name and region are the trigger's identity and cannot change."}}, nil
}

// Edit implements console.Editor: the Edit trigger page, the Create
// trigger fields prefilled, saved through UpdateBuildTrigger. The trigger
// keeps its name, region and whether it is disabled.
func (p buildTriggersProvider) Edit(ctx context.Context, project string, path []string, v map[string]string) error {
	if len(path) != 1 {
		return fmt.Errorf("no trigger at %v", path)
	}
	t, err := p.get(ctx, project, path[0])
	if err != nil {
		return err
	}
	vals := map[string]string{}
	for k, x := range v {
		vals[k] = x
	}
	vals["name"] = t.GetName()
	next, err := triggerFromValues(vals)
	if err != nil {
		return err
	}
	next.ResourceName, next.Disabled = t.GetResourceName(), t.GetDisabled()
	return p.b.withClient(ctx, func(c *cloudbuild.Client) error {
		if _, err := c.UpdateBuildTrigger(ctx, &cbpb.UpdateBuildTriggerRequest{ProjectId: p.project(project), TriggerId: t.GetId(), Trigger: next}); err != nil {
			return err
		}
		return runadapter.SetTriggerApproval(t.GetResourceName(), vals["approval"] == "true" || vals["approval"] == "on")
	})
}

// DetailActions implements console.PathActor: Run, and Disable or Enable,
// beside the page's Edit.
func (p buildTriggersProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if len(path) != 1 {
		return nil
	}
	t, err := p.get(ctx, project, path[0])
	if err != nil {
		return nil
	}
	st := "Enabled"
	if t.GetDisabled() {
		st = "Disabled"
	}
	return p.Actions(console.Resource{Name: t.GetName(), Status: st, Fields: map[string]string{"Event": triggerEvent(t)}})
}

// ActAt implements console.PathActor.
func (p buildTriggersProvider) ActAt(ctx context.Context, project string, path []string, action string, v map[string]string) error {
	if len(path) != 1 {
		return fmt.Errorf("unknown action %q", action)
	}
	if action == "run" {
		return p.Act(ctx, project, path[0], action)
	}
	if action != "enable-trigger" && action != "disable-trigger" {
		return fmt.Errorf("unknown action %q", action)
	}
	t, err := p.get(ctx, project, path[0])
	if err != nil {
		return err
	}
	next := &cbpb.BuildTrigger{ResourceName: t.GetResourceName(), Disabled: action == "disable-trigger"}
	return p.b.withClient(ctx, func(c *cloudbuild.Client) error {
		_, err := c.UpdateBuildTrigger(ctx, &cbpb.UpdateBuildTriggerRequest{ProjectId: p.project(project), TriggerId: t.GetId(),
			Trigger: next, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"disabled"}}})
		return err
	})
}
