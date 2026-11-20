package main

// The Triggers section of Workflows' Create workflow and Edit (#2011), as
// Google's has it (https://docs.cloud.google.com/workflows/docs/schedule-workflow,
// https://docs.cloud.google.com/eventarc/standard/docs/workflows/route-trigger-cloud-storage):
// Add new trigger makes an Eventarc trigger whose destination is the
// workflow, or a Cloud Scheduler job that runs it, each through this
// instance's API (Eventarc's official client on the Run port; Cloud
// Scheduler's server in this process). A workflow's page has the Triggers
// tab that lists them.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	eventarcpb "cloud.google.com/go/eventarc/apiv1/eventarcpb"
	"cloud.google.com/go/scheduler/apiv1/schedulerpb"
	"google.golang.org/api/iterator"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/scheduler"
)

const (
	wfTriggersSection = "Triggers"
	wfEventarcField   = "eventarcTriggers"
	wfSchedulerField  = "schedulerTriggers"
)

// wfExecutionsURI is the target Google's Cloud Scheduler trigger posts
// to; the instance's scheduler sends it to its own Workflow Executions.
func wfExecutionsURI(workflow string) string {
	return "https://workflowexecutions.googleapis.com/v1/" + workflow + "/executions"
}

// triggerFields are the Triggers section: one repeatable row per Eventarc
// trigger and per Cloud Scheduler trigger, each with Google's fields in
// its order. A service this instance does not serve has no row to add.
func (p workflowsProvider) triggerFields() []console.Field {
	var out []console.Field
	if p.eventarc != nil {
		et := eaEventTypeField()
		out = append(out, console.Field{Name: wfEventarcField, Label: "Eventarc trigger", Type: console.GroupFieldType,
			Section: wfTriggersSection, AddLabel: "Add new trigger: Eventarc",
			Help: "Each event of the type starts an execution, its CloudEvent the argument. Google sources only; the trigger is in " +
				p.eventarc.run.location() + ", this instance's Eventarc location.",
			Group: []console.Field{
				{Name: "name", Label: "Trigger name", Type: "text", Required: true, Pattern: `[a-z][a-z0-9\-]{0,62}`,
					Help: "1 to 63 lowercase letters, digits and hyphens, starting with a letter"},
				{Name: "eventType", Label: "Event type", Type: "select", Options: et.Options, OptionLabels: et.OptionLabels},
				{Name: "topic", Label: "Cloud Pub/Sub topic", Type: "text"},
				{Name: "bucket", Label: "Bucket", Type: "text"},
				{Name: "database", Label: "Firestore database", Type: "text"},
				{Name: "document", Label: "Document path pattern", Type: "text"},
				{Name: "serviceName", Label: "Audit log service name", Type: "text"},
				{Name: "methodName", Label: "Audit log method name", Type: "text"},
				{Name: "serviceAccount", Label: "Service account", Type: "text"},
			}})
	}
	if p.scheduler != nil {
		levels := map[string]string{}
		for k, v := range workflowsCallLogLabels {
			levels[k] = v
		}
		out = append(out, console.Field{Name: wfSchedulerField, Label: "Cloud Scheduler trigger", Type: console.GroupFieldType,
			Section: wfTriggersSection, AddLabel: "Add new trigger: Cloud Scheduler",
			Help: "A Cloud Scheduler job that runs the workflow on its schedule, authorized as the service account with an OAuth token.",
			Group: []console.Field{
				{Name: "name", Label: "Name", Type: "text", Required: true, Pattern: `[A-Za-z0-9_\-]{1,500}`,
					Help: "letters, numbers, hyphens and underscores"},
				{Name: "region", Label: "Region", Type: "select", Options: schedulerRegions(), Default: "us-central1"},
				{Name: "schedule", Label: "Frequency", Type: "text", Required: true, Default: "*/5 * * * *"},
				{Name: "timeZone", Label: "Timezone", Type: "text", Default: "Etc/UTC"},
				{Name: "argument", Label: "Workflow's argument", Type: "textarea", Default: "{}"},
				{Name: "callLogLevel", Label: "Workflow's call log level", Type: "select", Options: workflowsCallLogLevels, OptionLabels: levels},
				{Name: "serviceAccount", Label: "Service account", Type: "text", Required: true,
					Pattern: `[^@\s]+@[^@\s]+`, Help: "a service account's email"},
			}})
	}
	return out
}

// wfTriggers are the triggers a form's rows describe, checked before the
// workflow is created so a bad row makes nothing.
type wfTriggers struct {
	eventarc  []*eventarcpb.CreateTriggerRequest
	scheduler []*schedulerpb.Job
}

func (p workflowsProvider) triggersFromForm(project, workflow string, values map[string]string) (wfTriggers, error) {
	var out wfTriggers
	rows, err := console.ParseGroup(values[wfEventarcField])
	if err != nil {
		return out, fmt.Errorf("Eventarc triggers: %w", err)
	}
	if len(rows) > 0 && p.eventarc == nil {
		return out, errors.New("Eventarc is not served by this instance")
	}
	for i, r := range rows {
		region := ""
		if p.eventarc != nil {
			region = p.eventarc.run.location()
		}
		vals := map[string]string{"destination": "Workflows", "workflow": workflow, "eventType": r["eventType"],
			"topic": r["topic"], "bucket": r["bucket"], "database": r["database"], "document": r["document"],
			"serviceName": r["serviceName"], "methodName": r["methodName"], "serviceAccount": r["serviceAccount"],
			"resource": "Any resource"}
		t, err := eaFormTrigger(project, region, vals)
		if err != nil {
			return out, fmt.Errorf("Eventarc trigger %d: %w", i+1, err)
		}
		out.eventarc = append(out.eventarc, &eventarcpb.CreateTriggerRequest{Parent: "projects/" + project + "/locations/" + region,
			TriggerId: strings.TrimSpace(r["name"]), Trigger: t})
	}
	rows, err = console.ParseGroup(values[wfSchedulerField])
	if err != nil {
		return out, fmt.Errorf("Cloud Scheduler triggers: %w", err)
	}
	if len(rows) > 0 && p.scheduler == nil {
		return out, errors.New("Cloud Scheduler is not served by this instance")
	}
	for i, r := range rows {
		arg := strings.TrimSpace(r["argument"])
		if arg != "" {
			var v any
			if err := json.Unmarshal([]byte(arg), &v); err != nil {
				return out, fmt.Errorf("Cloud Scheduler trigger %d: the workflow's argument must be JSON: %v", i+1, err)
			}
		}
		body := map[string]string{"argument": arg}
		if lvl := r["callLogLevel"]; lvl != "" && lvl != "CALL_LOG_LEVEL_UNSPECIFIED" {
			body["callLogLevel"] = lvl
		}
		b, _ := json.Marshal(body)
		region := strings.TrimSpace(r["region"])
		if region == "" {
			region = "us-central1"
		}
		tz := strings.TrimSpace(r["timeZone"])
		if tz == "" {
			tz = "Etc/UTC"
		}
		out.scheduler = append(out.scheduler, &schedulerpb.Job{
			Name:        "projects/" + project + "/locations/" + region + "/jobs/" + strings.TrimSpace(r["name"]),
			Description: "Runs " + workflow,
			Schedule:    strings.TrimSpace(r["schedule"]), TimeZone: tz,
			Target: &schedulerpb.Job_HttpTarget{HttpTarget: &schedulerpb.HttpTarget{
				Uri: wfExecutionsURI(workflow), HttpMethod: schedulerpb.HttpMethod_POST, Body: b,
				Headers: map[string]string{"Content-Type": "application/octet-stream", "User-Agent": "Google-Cloud-Scheduler"},
				AuthorizationHeader: &schedulerpb.HttpTarget_OauthToken{OauthToken: &schedulerpb.OAuthToken{
					ServiceAccountEmail: strings.TrimSpace(r["serviceAccount"]), Scope: scheduler.DefaultOAuthScope}},
			}},
		})
	}
	return out, nil
}

// createTriggers makes them, in the form's order.
func (p workflowsProvider) createTriggers(ctx context.Context, project string, ts wfTriggers) error {
	if len(ts.eventarc) > 0 {
		c, err := p.eventarc.run.eventarcClient(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = c.Close() }()
		for _, req := range ts.eventarc {
			op, err := c.CreateTrigger(ctx, req)
			if err != nil {
				return fmt.Errorf("the workflow is deployed; its Eventarc trigger %s was not made: %w", req.GetTriggerId(), err)
			}
			if _, err := op.Wait(ctx); err != nil {
				return fmt.Errorf("the workflow is deployed; its Eventarc trigger %s was not made: %w", req.GetTriggerId(), err)
			}
		}
	}
	for _, job := range ts.scheduler {
		api := p.scheduler.API()
		if api == nil {
			return errors.New("Cloud Scheduler has not started")
		}
		parent := job.GetName()[:strings.Index(job.GetName(), "/jobs/")]
		if _, err := api.CreateJob(ctx, &schedulerpb.CreateJobRequest{Parent: parent, Job: job}); err != nil {
			return fmt.Errorf("the workflow is deployed; its Cloud Scheduler trigger %s was not made: %w", lastSegment(job.GetName()), err)
		}
	}
	return nil
}

// triggersSection is a workflow's Triggers tab: the Eventarc triggers
// whose destination it is, and the Cloud Scheduler jobs that run it.
func (p workflowsProvider) triggersSection(ctx context.Context, project, workflow string) console.Section {
	l := console.Listing{Columns: []string{"Type", "Region", "Event type or frequency", "Service account"}, NameColumn: "Trigger", Noun: "triggers"}
	var problems []string
	if p.eventarc != nil {
		if c, err := p.eventarc.run.eventarcClient(ctx); err != nil {
			problems = append(problems, "Eventarc: "+consoleErr(err))
		} else {
			it := c.ListTriggers(ctx, &eventarcpb.ListTriggersRequest{Parent: "projects/" + project + "/locations/" + p.eventarc.run.location()})
			for {
				t, err := it.Next()
				if errors.Is(err, iterator.Done) {
					break
				}
				if err != nil {
					problems = append(problems, "Eventarc: "+consoleErr(err))
					break
				}
				if t.GetDestination().GetWorkflow() != workflow {
					continue
				}
				typ, _ := eaType(t)
				l.Items = append(l.Items, console.Resource{Name: lastSegment(t.GetName()), Fields: map[string]string{
					"Type": "Eventarc", "Region": eaRegion(t.GetName()), "Event type or frequency": typ,
					"Service account": kmsOrDash(t.GetServiceAccount())}})
			}
			_ = c.Close()
		}
	}
	if p.scheduler != nil && p.scheduler.Store() != nil {
		jobs, err := p.scheduler.Store().List("projects/" + project + "/")
		if err != nil {
			problems = append(problems, "Cloud Scheduler: "+consoleErr(err))
		}
		sort.Slice(jobs, func(i, j int) bool { return jobs[i].Name < jobs[j].Name })
		for _, j := range jobs {
			if j.HTTP == nil || j.HTTP.URI != wfExecutionsURI(workflow) {
				continue
			}
			sa := "—"
			if j.HTTP.OAuthToken != nil {
				sa = j.HTTP.OAuthToken.ServiceAccountEmail
			} else if j.HTTP.OIDCToken != nil {
				sa = j.HTTP.OIDCToken.ServiceAccountEmail
			}
			parts := strings.Split(j.Name, "/")
			l.Items = append(l.Items, console.Resource{Name: parts[len(parts)-1], Fields: map[string]string{
				"Type": "Cloud Scheduler", "Region": parts[3], "Event type or frequency": j.Schedule + " (" + j.TimeZone + ")",
				"Service account": sa}})
		}
	}
	l.Total = len(l.Items)
	sec := console.Section{ID: "triggers", Label: "Triggers", Listing: l}
	if len(problems) > 0 {
		sec.Note = "cannot read every trigger: " + strings.Join(problems, "; ")
	}
	return sec
}
