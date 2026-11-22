package main

// Cloud Run: a service's Triggers tab (#1852, #1877), which is a Cloud Run
// function's too. Google's tab lists the Eventarc triggers whose
// destination is the service, and its Add trigger makes one: here through
// the official Eventarc client on this instance's Eventarc (served on the
// Run port over Pub/Sub, its transport). Add trigger is Google's Eventarc
// trigger pane (#2093), the one Cloud Run functions' Write a function and
// Triggers tab have (consolefunctionseventarc.go): every event provider
// and event type the instance's Eventarc lists through ListProviders, with
// each type's filters, the channels of Custom and Third-party, the region
// and the service account. Delete trigger deletes one of the tab's
// triggers.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	eventarc "cloud.google.com/go/eventarc/apiv1"
	eventarcpb "cloud.google.com/go/eventarc/apiv1/eventarcpb"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/k8s"
)

const (
	runAddTrigger    = "add-trigger"
	runDeleteTrigger = "delete-trigger"
)

// runTriggersNote says what a trigger is here.
const runTriggersNote = "An Eventarc trigger delivers each event to the service as a CloudEvent (binary content " +
	"mode, the ce-* headers), over a Pub/Sub push subscription Eventarc makes. Add trigger offers every event provider " +
	"this instance's Eventarc lists: Cloud Pub/Sub, Cloud Storage, Cloud Firestore (and Datastore mode), Cloud Audit Logs " +
	"of each served service, and the direct events of the services that have them, with Custom and Third-party channels. " +
	"Firebase's providers are not served, so they are not offered."

// eventarcClient is the official Eventarc client on the Run port.
func (p runProvider) eventarcClient(ctx context.Context) (*eventarc.Client, error) {
	if p.runEndpoint() == "" {
		return nil, errors.New("the Cloud Run adapter is not running")
	}
	c, err := eventarc.NewClient(ctx, option.WithEndpoint(p.runEndpoint()), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		return nil, fmt.Errorf("connect to Eventarc: %w", err)
	}
	return c, nil
}

// triggersSection is the Triggers tab.
func (p runProvider) triggersSection(ctx context.Context, project string, svc *ksvcStatus) console.Section {
	if p.pubsubAddr == "" {
		return runNotLocal("triggers", "Triggers", "A trigger is an Eventarc trigger, which this instance's Eventarc "+
			"delivers over Pub/Sub, and this instance does not serve Pub/Sub: start it with pubsub in --services.")
	}
	sec := console.Section{ID: "triggers", Label: "Triggers", Note: runTriggersNote, Listing: console.Listing{
		Columns:    []string{"Event provider", "Event type", "Filters", "Region", "Service URL path", "Transport topic"},
		NameColumn: "Trigger name", Noun: "triggers", Items: []console.Resource{},
	}}
	ts, err := p.runTriggers(ctx, project, svc)
	if err != nil {
		sec.Listing.Unavailable = "cannot read Eventarc triggers: " + err.Error()
		return sec
	}
	cat := p.eventarcCatalog(ctx, project)
	for _, t := range ts {
		typ, filters := "", []string{}
		for _, f := range t.GetEventFilters() {
			if f.GetAttribute() == "type" {
				typ = f.GetValue()
			} else {
				filters = append(filters, f.GetAttribute()+": "+f.GetValue())
			}
		}
		parts := strings.Split(t.GetName(), "/")
		sec.Listing.Items = append(sec.Listing.Items, console.Resource{Name: lastSegment(t.GetName()), Fields: map[string]string{
			"Event provider": eventarcTriggerProvider(cat, t), "Event type": typ, "Filters": strings.Join(filters, ", "),
			"Region": parts[3], "Service URL path": t.GetDestination().GetCloudRun().GetPath(),
			"Transport topic": lastSegment(t.GetTransport().GetPubsub().GetTopic()),
		}})
	}
	sec.Listing.Total = len(sec.Listing.Items)
	sec.Actions = p.triggerActions(ctx, project, svc)
	return sec
}

// runTriggers are the project's Eventarc triggers in the instance's region
// whose destination is svc.
func (p runProvider) runTriggers(ctx context.Context, project string, svc *ksvcStatus) ([]*eventarcpb.Trigger, error) {
	project = p.projectOr(project)
	c, err := p.eventarcClient(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.Close() }()
	var out []*eventarcpb.Trigger
	it := c.ListTriggers(ctx, &eventarcpb.ListTriggersRequest{Parent: "projects/" + project + "/locations/" + p.location()})
	for {
		t, err := it.Next()
		if errors.Is(err, iterator.Done) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		if run := t.GetDestination().GetCloudRun(); run.GetService() == svc.Metadata.Name &&
			(run.GetRegion() == "" || run.GetRegion() == p.location()) {
			out = append(out, t)
		}
	}
}

// runTopics are the project's topic IDs, for Add trigger's topic.
func (p runProvider) runTopics(ctx context.Context, project string) []string {
	c, err := pubsubProvider{endpoint: p.pubsubAddr}.client(ctx, p.projectOr(project))
	if err != nil {
		return nil
	}
	defer func() { _ = c.Close() }()
	var out []string
	it := c.TopicAdminClient.ListTopics(ctx, &pubsubpb.ListTopicsRequest{Project: "projects/" + p.projectOr(project)})
	for {
		t, err := it.Next()
		if err != nil {
			break
		}
		if !strings.HasPrefix(lastSegment(t.GetName()), "eventarc-") {
			out = append(out, lastSegment(t.GetName()))
		}
	}
	slices.Sort(out)
	return out
}

// eventarcCatalog is what Add trigger offers in the instance's region,
// with the project's topics for Cloud Pub/Sub's.
func (p runProvider) eventarcCatalog(ctx context.Context, project string) eventarcPaneCatalog {
	c, err := p.eventarcClient(ctx)
	if err != nil {
		return eventarcPaneCatalog{err: err.Error()}
	}
	defer func() { _ = c.Close() }()
	cat := eventarcCatalogFrom(ctx, c, p.projectOr(project), p.location())
	cat.topics = append([]string{}, p.runTopics(ctx, project)...)
	return cat
}

// triggerActions are Google's Add trigger pane and Delete trigger, on a
// service CloudBurrow created.
func (p runProvider) triggerActions(ctx context.Context, project string, svc *ksvcStatus) []console.Action {
	if p.pubsubAddr == "" || svc == nil || svc.Metadata.Labels[k8s.OwnedLabel] != k8s.OwnedValue {
		return nil
	}
	return []console.Action{
		{ID: runAddTrigger, Label: "Add trigger", Submit: "Save trigger", Fields: eventarcPane(p.eventarcCatalog(ctx, project), p.location())},
		{ID: runDeleteTrigger, Label: "Delete trigger", Destructive: true, Fields: []console.Field{
			{Name: "name", Label: "Trigger name", Type: "text", Required: true, Help: "A trigger on this tab."},
		}},
	}
}

// actOnTriggers performs Add trigger and Delete trigger.
func (p runProvider) actOnTriggers(ctx context.Context, project, service, action string, values map[string]string) error {
	if p.pubsubAddr == "" {
		return errors.New("this instance does not serve Pub/Sub, which Eventarc delivers through")
	}
	project = p.projectOr(project)
	if project == "" {
		return errors.New("choose a project before changing a service's triggers")
	}
	svc, err := p.service(ctx, project, service)
	if err == nil && svc == nil {
		err = fmt.Errorf("service %s not found", service)
	}
	if err != nil {
		return err
	}
	id := strings.TrimSpace(values["name"])
	if id == "" {
		return errors.New("a trigger name is required")
	}
	if r := strings.TrimSpace(values["region"]); r != "" && r != p.location() {
		return fmt.Errorf("region %q is not served: this instance's Cloud Run location is %s", r, p.location())
	}
	c, err := p.eventarcClient(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	parent := "projects/" + project + "/locations/" + p.location()
	if action == runDeleteTrigger {
		ts, err := p.runTriggers(ctx, project, svc)
		if err != nil {
			return err
		}
		if !slices.ContainsFunc(ts, func(t *eventarcpb.Trigger) bool { return t.GetName() == parent+"/triggers/"+id }) {
			return fmt.Errorf("%s is not a trigger of service %s", id, service)
		}
		op, err := c.DeleteTrigger(ctx, &eventarcpb.DeleteTriggerRequest{Name: parent + "/triggers/" + id})
		if err != nil {
			return err
		}
		_, err = op.Wait(ctx)
		return err
	}
	return savePaneTrigger(ctx, c, project, p.location(), service, "", values)
}
