package main

// Cloud Run: a service's Triggers tab (#1852). Google's Add trigger makes an
// Eventarc trigger; this instance serves no eventarc.googleapis.com API, so
// the Pub/Sub trigger is what Eventarc builds it on: a push subscription on
// the chosen topic that delivers to the service. The tab lists the push
// subscriptions that deliver to the service, with Add trigger and Delete
// trigger, through the official Pub/Sub client on the instance's Pub/Sub.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/api/iterator"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/k8s"
)

const (
	runAddTrigger    = "add-trigger"
	runDeleteTrigger = "delete-trigger"
	// runPubSubEvent is the one event type Google's Pub/Sub trigger names.
	runPubSubEvent = "google.cloud.pubsub.topic.v1.messagePublished"
)

// runPushEndpoint is where a trigger pushes: the service's cluster-local
// address, which the Pub/Sub emulator in the cluster reaches.
func (p runProvider) runPushEndpoint(svc *ksvcStatus) string {
	return "http://" + svc.KName + "." + p.namespace + ".svc.cluster.local"
}

// runTriggersNote says what a trigger is here.
const runTriggersNote = "A Pub/Sub trigger is a push subscription on the topic that delivers each message to " +
	"the service, as Eventarc's Pub/Sub transport does. No Eventarc API is served locally, so the service " +
	"receives Pub/Sub's push request (the message in a JSON envelope), not a CloudEvent, and only Pub/Sub " +
	"triggers are offered."

// triggersSection is the Triggers tab.
func (p runProvider) triggersSection(ctx context.Context, project string, svc *ksvcStatus) console.Section {
	if p.pubsubAddr == "" {
		return runNotLocal("triggers", "Triggers", "Add trigger makes a Pub/Sub trigger, and this instance "+
			"does not serve Pub/Sub: start it with pubsub in --services.")
	}
	sec := console.Section{ID: "triggers", Label: "Triggers", Note: runTriggersNote, Listing: console.Listing{
		Columns: []string{"Event type", "Topic", "Push endpoint"}, NameColumn: "Trigger name", Noun: "triggers",
		Items: []console.Resource{},
	}}
	subs, err := p.runTriggers(ctx, project, svc)
	if err != nil {
		sec.Listing.Unavailable = "cannot read Pub/Sub subscriptions: " + err.Error()
		return sec
	}
	for _, s := range subs {
		sec.Listing.Items = append(sec.Listing.Items, console.Resource{Name: lastSegment(s.GetName()), Fields: map[string]string{
			"Event type": runPubSubEvent, "Topic": lastSegment(s.GetTopic()),
			"Push endpoint": s.GetPushConfig().GetPushEndpoint(),
		}})
	}
	sec.Listing.Total = len(sec.Listing.Items)
	sec.Actions = p.triggerActions(ctx, project, svc)
	return sec
}

// runTriggers are the project's push subscriptions delivering to svc.
func (p runProvider) runTriggers(ctx context.Context, project string, svc *ksvcStatus) ([]*pubsubpb.Subscription, error) {
	project = p.projectOr(project)
	c, err := pubsubProvider{endpoint: p.pubsubAddr}.client(ctx, project)
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.Close() }()
	endpoint := p.runPushEndpoint(svc)
	var out []*pubsubpb.Subscription
	it := c.SubscriptionAdminClient.ListSubscriptions(ctx, &pubsubpb.ListSubscriptionsRequest{Project: "projects/" + project})
	for {
		s, err := it.Next()
		if errors.Is(err, iterator.Done) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		if e := s.GetPushConfig().GetPushEndpoint(); e == endpoint || strings.HasPrefix(e, endpoint+"/") ||
			(svc.Status.URL != "" && strings.HasPrefix(e, svc.Status.URL)) {
			out = append(out, s)
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
		out = append(out, lastSegment(t.GetName()))
	}
	slices.Sort(out)
	return out
}

// triggerActions are Google's Pub/Sub trigger dialog and Delete trigger, on
// a service CloudBurrow created.
func (p runProvider) triggerActions(ctx context.Context, project string, svc *ksvcStatus) []console.Action {
	if p.pubsubAddr == "" || svc == nil || svc.Metadata.Labels[k8s.OwnedLabel] != k8s.OwnedValue {
		return nil
	}
	topic := console.Field{Name: "topic", Label: "Select a Cloud Pub/Sub topic", Type: "select", Required: true,
		Options: p.runTopics(ctx, project), Help: "Each message published to it is pushed to the service."}
	if len(topic.Options) == 0 {
		topic.Type, topic.Help = "text", "A topic ID of this project; there is none yet: create one on the Pub/Sub page."
	}
	return []console.Action{
		{ID: runAddTrigger, Label: "Add trigger", Submit: "Save trigger", Fields: []console.Field{
			{Name: "name", Label: "Trigger name", Type: "text", Required: true,
				Pattern: `^[a-zA-Z][a-zA-Z0-9\-_.~+%]{2,254}$`,
				Help:    "The push subscription's ID: 3 to 255 characters, starting with a letter."},
			{Name: "eventType", Label: "Event type", Type: "select", Options: []string{runPubSubEvent}, Default: runPubSubEvent,
				Help: "Event provider Cloud Pub/Sub, the only one offered locally."},
			topic,
			{Name: "region", Label: "Region", Type: "select", Options: []string{p.location()}, Default: p.location()},
			{Name: "serviceAccount", Label: "Service account", Type: "text",
				Help: "Optional. The account the push authenticates as, with an OIDC token for the service's URL."},
		}},
		{ID: runDeleteTrigger, Label: "Delete trigger", Destructive: true, Fields: []console.Field{
			{Name: "name", Label: "Trigger name", Type: "text", Required: true, Help: "A trigger on this tab."},
		}},
	}
}

// actOnTriggers performs Add trigger and Delete trigger.
func (p runProvider) actOnTriggers(ctx context.Context, project, service, action string, values map[string]string) error {
	if p.pubsubAddr == "" {
		return errors.New("this instance does not serve Pub/Sub, which a trigger delivers through")
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
	c, err := pubsubProvider{endpoint: p.pubsubAddr}.client(ctx, project)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	name := "projects/" + project + "/subscriptions/" + id
	if action == runDeleteTrigger {
		subs, err := p.runTriggers(ctx, project, svc)
		if err != nil {
			return err
		}
		if !slices.ContainsFunc(subs, func(s *pubsubpb.Subscription) bool { return s.GetName() == name }) {
			return fmt.Errorf("%s is not a trigger of service %s", id, service)
		}
		return c.SubscriptionAdminClient.DeleteSubscription(ctx, &pubsubpb.DeleteSubscriptionRequest{Subscription: name})
	}
	if t := strings.TrimSpace(values["eventType"]); t != "" && t != runPubSubEvent {
		return fmt.Errorf("event type %q is not offered locally: only %s", t, runPubSubEvent)
	}
	if r := strings.TrimSpace(values["region"]); r != "" && r != p.location() {
		return fmt.Errorf("region %q is not served: this instance's Cloud Run location is %s", r, p.location())
	}
	topic := strings.TrimSpace(values["topic"])
	if topic == "" {
		return errors.New("choose a Cloud Pub/Sub topic")
	}
	if !strings.HasPrefix(topic, "projects/") {
		topic = "projects/" + project + "/topics/" + topic
	}
	push := &pubsubpb.PushConfig{PushEndpoint: p.runPushEndpoint(svc)}
	if sa := strings.TrimSpace(values["serviceAccount"]); sa != "" {
		push.AuthenticationMethod = &pubsubpb.PushConfig_OidcToken_{OidcToken: &pubsubpb.PushConfig_OidcToken{
			ServiceAccountEmail: sa, Audience: svc.Status.URL}}
	}
	return createSubscription(ctx, c.SubscriptionAdminClient, &pubsubpb.Subscription{
		Name: name, Topic: topic, PushConfig: push, AckDeadlineSeconds: 600})
}
