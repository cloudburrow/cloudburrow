package main

// Cloud Run: a service's Triggers tab (#1852, #1877). Google's tab lists the
// Eventarc triggers whose destination is the service, and its Add trigger
// makes one: here through the official Eventarc client on this instance's
// Eventarc (served on the Run port over Pub/Sub, its transport), for the
// providers it serves, Cloud Pub/Sub and Cloud Storage. Delete trigger
// deletes one of the tab's triggers.

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
	// runPubSubEvent is Cloud Pub/Sub's event type.
	runPubSubEvent = "google.cloud.pubsub.topic.v1.messagePublished"
)

// runEventTypes are the event types Add trigger offers, with Google's
// provider and event names.
var runEventTypes = []struct{ typ, provider, label string }{
	{runPubSubEvent, "Cloud Pub/Sub", "Cloud Pub/Sub: google.cloud.pubsub.topic.v1.messagePublished"},
	{"google.cloud.storage.object.v1.finalized", "Cloud Storage", "Cloud Storage: google.cloud.storage.object.v1.finalized"},
	{"google.cloud.storage.object.v1.deleted", "Cloud Storage", "Cloud Storage: google.cloud.storage.object.v1.deleted"},
	{"google.cloud.storage.object.v1.archived", "Cloud Storage", "Cloud Storage: google.cloud.storage.object.v1.archived"},
	{"google.cloud.storage.object.v1.metadataUpdated", "Cloud Storage", "Cloud Storage: google.cloud.storage.object.v1.metadataUpdated"},
}

func runEventProvider(typ string) string {
	for _, e := range runEventTypes {
		if e.typ == typ {
			return e.provider
		}
	}
	return ""
}

// runTriggersNote says what a trigger is here.
const runTriggersNote = "An Eventarc trigger delivers each event to the service as a CloudEvent (binary content " +
	"mode, the ce-* headers), over a Pub/Sub push subscription Eventarc makes. This instance's Eventarc serves " +
	"Cloud Pub/Sub's and Cloud Storage's direct events here; Eventarc's Triggers page also makes Cloud Audit Logs and " +
	"Firestore triggers. Firebase events and channels are not served."

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
			"Event provider": runEventProvider(typ), "Event type": typ, "Filters": strings.Join(filters, ", "),
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

// triggerActions are Google's Add trigger dialog and Delete trigger, on a
// service CloudBurrow created.
func (p runProvider) triggerActions(ctx context.Context, project string, svc *ksvcStatus) []console.Action {
	if p.pubsubAddr == "" || svc == nil || svc.Metadata.Labels[k8s.OwnedLabel] != k8s.OwnedValue {
		return nil
	}
	types := make([]string, 0, len(runEventTypes))
	labels := map[string]string{}
	for _, e := range runEventTypes {
		types = append(types, e.typ)
		labels[e.typ] = e.label
	}
	topic := console.Field{Name: "topic", Label: "Select a Cloud Pub/Sub topic", Type: "select",
		Options: append([]string{""}, p.runTopics(ctx, project)...), OptionLabels: map[string]string{"": "Create a topic (Eventarc makes one)"},
		Help: "Cloud Pub/Sub events: each message published to the topic is delivered to the service."}
	return []console.Action{
		{ID: runAddTrigger, Label: "Add trigger", Submit: "Save trigger", Fields: []console.Field{
			{Name: "name", Label: "Trigger name", Type: "text", Required: true, Pattern: `^[a-z][a-z0-9\-]{0,62}$`,
				Help: "1 to 63 lowercase letters, digits and hyphens, starting with a letter."},
			{Name: "eventType", Label: "Event type", Type: "select", Options: types, OptionLabels: labels, Default: runPubSubEvent,
				Help: "Event provider and event: Cloud Pub/Sub and Cloud Storage's direct events are served locally."},
			topic,
			{Name: "bucket", Label: "Bucket", Type: "text", Pattern: `^[a-z0-9][a-z0-9._\-]{1,221}[a-z0-9]$`,
				Help: "Cloud Storage events: the bucket whose objects' events are delivered."},
			{Name: "region", Label: "Region", Type: "select", Options: []string{p.location()}, Default: p.location()},
			{Name: "serviceAccount", Label: "Service account", Type: "text",
				Help: "Optional. The account the delivery authenticates as, with an ID token for the service's URL; " +
					"the default compute account when empty."},
			{Name: "path", Label: "Service URL path", Type: "text", Pattern: `^/.*$`,
				Help: "Optional. The path on the service the events are sent to, beginning with /."},
		}},
		{ID: runDeleteTrigger, Label: "Delete trigger", Destructive: true, Fields: []console.Field{
			{Name: "name", Label: "Trigger name", Type: "text", Required: true, Help: "A trigger on this tab."},
		}},
	}
}

// eventarcTrigger is Add trigger's trigger.
func eventarcTrigger(project, service, region string, values map[string]string) (*eventarcpb.Trigger, error) {
	typ := strings.TrimSpace(values["eventType"])
	if typ == "" {
		typ = runPubSubEvent
	}
	if runEventProvider(typ) == "" {
		return nil, fmt.Errorf("event type %q is not offered locally", typ)
	}
	t := &eventarcpb.Trigger{EventFilters: []*eventarcpb.EventFilter{{Attribute: "type", Value: typ}},
		ServiceAccount: strings.TrimSpace(values["serviceAccount"]),
		Destination: &eventarcpb.Destination{Descriptor_: &eventarcpb.Destination_CloudRun{CloudRun: &eventarcpb.CloudRun{
			Service: service, Region: region, Path: strings.TrimSpace(values["path"])}}}}
	topic, bucket := strings.TrimSpace(values["topic"]), strings.TrimSpace(values["bucket"])
	if typ == runPubSubEvent {
		if bucket != "" {
			return nil, errors.New("a Cloud Pub/Sub trigger takes a topic, not a bucket")
		}
		if topic != "" {
			if !strings.HasPrefix(topic, "projects/") {
				topic = "projects/" + project + "/topics/" + topic
			}
			t.Transport = &eventarcpb.Transport{Intermediary: &eventarcpb.Transport_Pubsub{Pubsub: &eventarcpb.Pubsub{Topic: topic}}}
		}
		return t, nil
	}
	if bucket == "" {
		return nil, errors.New("a Cloud Storage trigger needs its bucket")
	}
	if topic != "" {
		return nil, errors.New("a Cloud Storage trigger takes a bucket, not a topic")
	}
	t.EventFilters = append(t.EventFilters, &eventarcpb.EventFilter{Attribute: "bucket", Value: bucket})
	return t, nil
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
	t, err := eventarcTrigger(project, service, p.location(), values)
	if err != nil {
		return err
	}
	op, err := c.CreateTrigger(ctx, &eventarcpb.CreateTriggerRequest{Parent: parent, TriggerId: id, Trigger: t})
	if err != nil {
		return err
	}
	_, err = op.Wait(ctx)
	return err
}
