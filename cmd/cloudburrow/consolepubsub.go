package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	vkit "cloud.google.com/go/pubsub/v2/apiv1"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// Pub/Sub actions on a topic page (#294): create a subscription, publish, and
// pull. Every one is a call the official client makes against the emulator,
// the same call an application would make, so a subscription created here or
// a message published here is indistinguishable from one an SDK produced.

const (
	actCreateSubscription = "create-subscription"
	actPublish            = "publish"
	actPullAck            = "pull-ack"
	actPullNoAck          = "pull-no-ack"

	// maxConsolePull bounds one pull. A console pull is for looking, and a
	// page of a thousand messages is one nobody reads.
	maxConsolePull = 100
)

// pullNoAckHelp is the copy for "Pull without ack". Pub/Sub has no peek: a
// pulled message is delivered, and not acknowledging it means it comes back.
// Calling this "view" or "peek" would promise a read with no effect.
const pullNoAckHelp = "Not a peek: Pub/Sub has none. Pulled messages are not acknowledged — " +
	"their ack deadline is set to 0, so they are redelivered at once and their " +
	"delivery attempts go up."

func (p pubsubProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if len(path) != 1 || project == "" {
		return nil
	}
	subField := console.Field{
		Name: "subscription", Label: "Subscription ID", Type: "text", Required: true,
		Help: "A pull subscription of this topic.",
	}
	if subs, err := p.topicSubscriptions(ctx, project, path[0]); err == nil && len(subs) > 0 {
		subField.Default = lastSegment(subs[0])
	}
	maxField := console.Field{
		Name: "max", Label: "Maximum messages", Type: "number", Default: "10",
		Help: fmt.Sprintf("1 to %d.", maxConsolePull),
	}
	noAckSub := subField
	noAckSub.Help = pullNoAckHelp
	return []console.Action{
		{ID: actCreateSubscription, Label: "Create subscription", Fields: []console.Field{
			{
				Name: "name", Label: "Subscription ID", Type: "text", Required: true,
				Help:    "3-255 characters, starting with a letter.",
				Pattern: `^[A-Za-z][A-Za-z0-9._~%+\-]{2,254}$`,
			},
			{
				Name: "pushEndpoint", Label: "Push endpoint", Type: "url",
				Help: "Leave empty for a pull subscription. A push endpoint receives each message as an HTTP POST.",
			},
			{
				Name: "ackDeadline", Label: "Acknowledgement deadline (seconds)", Type: "number", Default: "10",
				Help: "10 to 600.",
			},
		}},
		{ID: actPublish, Label: "Publish message", Fields: []console.Field{
			{Name: "data", Label: "Message body", Type: "textarea", Required: true},
			{
				Name: "attributes", Label: "Attributes", Type: "map",
				Help: "One key=value per line.",
			},
			{
				Name: "orderingKey", Label: "Ordering key", Type: "text",
				Help: "Delivered in order only to subscriptions with message ordering enabled.",
			},
		}},
		{ID: actPullAck, Label: "Pull and ack", Fields: []console.Field{subField, maxField}},
		{ID: actPullNoAck, Label: "Pull without ack", Fields: []console.Field{noAckSub, maxField}},
	}
}

func (p pubsubProvider) ActAt(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	_, err := p.ActAtResult(ctx, project, path, action, values)
	return err
}

func (p pubsubProvider) ActAtResult(ctx context.Context, project string, path []string, action string, values map[string]string) (*console.Listing, error) {
	if len(path) != 1 {
		return nil, fmt.Errorf("Pub/Sub actions apply to a topic")
	}
	topic := path[0]
	if !strings.HasPrefix(topic, "projects/"+project+"/topics/") {
		return nil, fmt.Errorf("%s is not a topic of project %s", topic, project)
	}
	c, err := p.client(ctx, project)
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.Close() }()

	switch action {
	case actCreateSubscription:
		id := strings.TrimSpace(values["name"])
		if id == "" {
			return nil, fmt.Errorf("subscription ID is required")
		}
		sub := &pubsubpb.Subscription{
			Name:  fmt.Sprintf("projects/%s/subscriptions/%s", project, id),
			Topic: topic,
		}
		if v := strings.TrimSpace(values["ackDeadline"]); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 10 || n > 600 {
				return nil, fmt.Errorf("the acknowledgement deadline must be 10 to 600 seconds")
			}
			sub.AckDeadlineSeconds = int32(n)
		}
		if ep := strings.TrimSpace(values["pushEndpoint"]); ep != "" {
			sub.PushConfig = &pubsubpb.PushConfig{PushEndpoint: ep}
		}
		_, err := c.SubscriptionAdminClient.CreateSubscription(ctx, sub)
		return nil, err

	case actPublish:
		attrs, err := console.ParseMap(values["attributes"])
		if err != nil {
			return nil, fmt.Errorf("attributes: %w", err)
		}
		msg := &pubsubpb.PubsubMessage{
			Data:        []byte(values["data"]),
			Attributes:  attrs,
			OrderingKey: strings.TrimSpace(values["orderingKey"]),
		}
		if len(msg.Data) == 0 && len(attrs) == 0 {
			return nil, fmt.Errorf("a message needs a body or at least one attribute")
		}
		res, err := c.TopicAdminClient.Publish(ctx, &pubsubpb.PublishRequest{Topic: topic, Messages: []*pubsubpb.PubsubMessage{msg}})
		if err != nil {
			return nil, err
		}
		return &console.Listing{
			Columns: []string{}, NameColumn: "Message ID", Noun: "messages",
			Items: []console.Resource{{Name: strings.Join(res.GetMessageIds(), ", ")}}, Total: 1,
		}, nil

	case actPullAck, actPullNoAck:
		return p.pull(ctx, c.SubscriptionAdminClient, project, topic, values, action == actPullAck)
	default:
		return nil, fmt.Errorf("unknown action %q", action)
	}
}

func (p pubsubProvider) pull(ctx context.Context, sc *vkit.SubscriptionAdminClient, project, topic string, values map[string]string, ack bool) (*console.Listing, error) {
	id := strings.TrimSpace(values["subscription"])
	if id == "" {
		return nil, fmt.Errorf("subscription ID is required")
	}
	name := fmt.Sprintf("projects/%s/subscriptions/%s", project, id)
	sub, err := sc.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: name})
	if err != nil {
		return nil, err
	}
	// Only this topic's subscriptions: the action is on the topic's page, and
	// pulling from an unrelated subscription through it would be an operation
	// the page never offered.
	if sub.GetTopic() != topic {
		return nil, fmt.Errorf("%s is not a subscription of %s", id, topic)
	}
	if sub.GetPushConfig().GetPushEndpoint() != "" {
		return nil, fmt.Errorf("%s is a push subscription; its messages are delivered to %s, not pulled",
			id, sub.GetPushConfig().GetPushEndpoint())
	}
	max := 10
	if v := strings.TrimSpace(values["max"]); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxConsolePull {
			return nil, fmt.Errorf("maximum messages must be 1 to %d", maxConsolePull)
		}
		max = n
	}

	// A bounded wait. A pull on an empty subscription is allowed to wait for
	// a message to arrive, which on a console button is a spinner that never
	// stops; an empty answer after a few seconds is the honest one.
	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	// ReturnImmediately is deprecated in the API, and is still what makes an
	// empty pull answer at once on the emulator; the timeout covers the rest.
	resp, err := sc.Pull(pctx, &pubsubpb.PullRequest{Subscription: name, MaxMessages: int32(max), ReturnImmediately: true})
	if err != nil && !isDeadline(err) {
		return nil, err
	}

	out := &console.Listing{
		Columns:    []string{"Body", "Attributes", "Ordering key", "Published", "Delivery attempt"},
		NameColumn: "Message ID", Noun: "messages",
	}
	var ackIDs []string
	for _, rm := range resp.GetReceivedMessages() {
		m := rm.GetMessage()
		ackIDs = append(ackIDs, rm.GetAckId())
		attempt := "—"
		// Pub/Sub counts delivery attempts only for a subscription with a
		// dead-letter policy; elsewhere the field is zero, and zero would
		// read as "never delivered".
		if n := rm.GetDeliveryAttempt(); n > 0 {
			attempt = strconv.Itoa(int(n))
		}
		out.Items = append(out.Items, console.Resource{Name: m.GetMessageId(), Fields: map[string]string{
			"Body":             string(m.GetData()),
			"Attributes":       formatAttributes(m.GetAttributes()),
			"Ordering key":     m.GetOrderingKey(),
			"Published":        m.GetPublishTime().AsTime().UTC().Format(time.RFC3339),
			"Delivery attempt": attempt,
		}})
	}
	out.Total = len(out.Items)
	if len(ackIDs) > 0 {
		if ack {
			err = sc.Acknowledge(ctx, &pubsubpb.AcknowledgeRequest{Subscription: name, AckIds: ackIDs})
		} else {
			err = sc.ModifyAckDeadline(ctx, &pubsubpb.ModifyAckDeadlineRequest{Subscription: name, AckIds: ackIDs, AckDeadlineSeconds: 0})
		}
		if err != nil {
			// The messages were pulled and are shown; what failed is what
			// happens to them next, so say which.
			verb := "acknowledged"
			if !ack {
				verb = "released for redelivery"
			}
			return nil, fmt.Errorf("pulled %d messages, but they could not be %s (they return when their ack deadline passes): %w",
				len(ackIDs), verb, err)
		}
	}
	switch {
	case len(out.Items) == 0:
		out.Note = "No messages were waiting on " + id + "."
	case ack:
		out.Note = fmt.Sprintf("%d acknowledged: they will not be delivered again.", len(out.Items))
	default:
		out.Note = fmt.Sprintf("%d not acknowledged: they are redelivered, and their delivery attempts go up.", len(out.Items))
	}
	return out, nil
}

// topicSubscriptions lists the subscriptions attached to a topic.
func (p pubsubProvider) topicSubscriptions(ctx context.Context, project, topic string) ([]string, error) {
	c, err := p.client(ctx, project)
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.Close() }()
	var out []string
	it := c.TopicAdminClient.ListTopicSubscriptions(ctx, &pubsubpb.ListTopicSubscriptionsRequest{Topic: topic})
	for {
		name, err := it.Next()
		if errors.Is(err, iterator.Done) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		out = append(out, name)
	}
}

func formatAttributes(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+m[k])
	}
	return strings.Join(parts, ", ")
}

func isDeadline(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) || status.Code(err) == codes.DeadlineExceeded
}
