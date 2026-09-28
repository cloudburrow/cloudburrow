package main

// Pub/Sub: the options Create topic and Create subscription set (#852).
//
// Each option here was measured against the pinned emulator through the
// official client before it was offered (TestConsolePubSubCreateOptions):
// a topic's message retention and its schema, which the emulator enforces on
// publish; a subscription's retention, retained acknowledged messages,
// message ordering, filter (only matching messages are delivered), retry
// policy and dead-letter policy, each read back as it was created.
//
// Expiration and exactly-once delivery are offered since #873. The emulator
// stores an expiration policy and never acts on it, so CloudBurrow's front
// before it (internal/pubsubfront) enforces it: a subscription idle for its
// ttl is deleted, and a ttl Google refuses is refused
// (TestPubSubSubscriptionExpiresWhenIdle,
// TestPubSubRefusesAnExpirationGoogleRefuses). Exactly-once delivery the
// emulator implements itself: an acknowledged message is not resent, and an
// expired ack ID is refused (TestPubSubExactlyOnceDelivery).

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	vkit "cloud.google.com/go/pubsub/v2/apiv1"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	gax "github.com/googleapis/gax-go/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// pubsubExpirationHelp explains the expiration field, the same rules the
// front enforces (internal/pubsubfront).
const pubsubExpirationHelp = "How long the subscription may be inactive before it is deleted: at least 1d, and no " +
	"shorter than the message retention duration. Empty: Google's default, 31d. never: it never expires. Any call " +
	"naming the subscription is activity, an open streaming pull keeps it active, and so does a successful push " +
	"(an answer of 102, 200, 201, 202 or 204). " + pubsubDurationHelp

// pubsubExactlyOnceHelp explains the exactly-once field.
const pubsubExactlyOnceHelp = "A message is not resent while its acknowledgement deadline holds, an acknowledged " +
	"message is not resent, and an acknowledgement with an expired ack ID is refused, so a subscriber knows whether " +
	"it succeeded. Pull subscriptions only: Google does not offer it with a push endpoint."

// pubsubExactlyOncePush is the refusal of exactly-once with a push
// endpoint. The emulator accepts the pair; Google's exactly-once page says
// "Push and export subscriptions don't support exactly-once delivery."
const pubsubExactlyOncePush = "exactly-once delivery is for pull subscriptions only: Google's push and export " +
	"subscriptions do not support it. Leave the push endpoint empty, or clear exactly-once delivery"

// pubsubFilterRefusedAtCreate prefixes the emulator's answer to a filter it
// cannot parse, which is UNKNOWN "Application error processing RPC" and names
// neither the field nor the fault.
const pubsubFilterRefusedAtCreate = "the emulator refused the filter; check it against the Pub/Sub filter syntax, " +
	`such as attributes.env = "prod" or hasPrefix(attributes.name, "a")`

// topicCreateFields are Create topic's fields after the Topic ID: its
// retention and its schema.
func topicCreateFields() []console.Field {
	return []console.Field{
		{Name: "messageRetention", Label: "Message retention duration", Type: "text", Section: "Message retention",
			Help: "Optional. How long the topic keeps every message published to it, acknowledged or not, " +
				"from 10m to 31d. " + pubsubDurationHelp + " Empty: the topic keeps none."},
		{Name: "schema", Label: "Schema", Type: "text", Section: "Schema",
			Help: "Optional. A schema ID of this project, or projects/{project}/schemas/{schema}, that exists. " +
				"A message published to the topic that does not conform to it is refused."},
		{Name: "schemaEncoding", Label: "Message encoding", Type: "select", Section: "Schema",
			Options: []string{"JSON", "BINARY"}, Default: "JSON",
			Help: "How a message's data encodes the schema, with a schema only."},
	}
}

// topicFromForm is the topic Create topic describes.
func topicFromForm(project, name string, values map[string]string) (*pubsubpb.Topic, error) {
	t := &pubsubpb.Topic{Name: name}
	retention, err := parsePubSubDuration("Message retention duration", values["messageRetention"])
	if err != nil {
		return nil, err
	}
	t.MessageRetentionDuration = retention
	if schema := strings.TrimSpace(values["schema"]); schema != "" {
		if !strings.HasPrefix(schema, "projects/") {
			schema = "projects/" + project + "/schemas/" + schema
		}
		enc := pubsubpb.Encoding_JSON
		switch strings.TrimSpace(values["schemaEncoding"]) {
		case "", "JSON":
		case "BINARY":
			enc = pubsubpb.Encoding_BINARY
		default:
			return nil, fmt.Errorf("message encoding: %q is neither JSON nor BINARY", values["schemaEncoding"])
		}
		t.SchemaSettings = &pubsubpb.SchemaSettings{Schema: schema, Encoding: enc}
	}
	return t, nil
}

// subscriptionCreateFields is Create subscription on a topic's page: what
// the emulator's CreateSubscription keeps and acts on, under the same
// headings as Edit subscription.
func subscriptionCreateFields() []console.Field {
	const delivery, retries, deadLetter = "Delivery", "Retry policy", "Dead lettering"
	return []console.Field{
		{Name: "name", Label: "Subscription ID", Type: "text", Required: true,
			Help:    "3-255 characters, starting with a letter.",
			Pattern: `^[A-Za-z][A-Za-z0-9._~%+\-]{2,254}$`},
		{Name: "pushEndpoint", Label: "Push endpoint", Type: "url", Section: delivery,
			Help: "Leave empty for a pull subscription. A push endpoint receives each message as an HTTP POST."},
		{Name: "ackDeadline", Label: "Acknowledgement deadline (seconds)", Type: "number", Default: "10", Section: delivery,
			Help: "10 to 600."},
		{Name: "messageRetention", Label: "Message retention duration", Type: "text", Default: "7d", Section: delivery,
			Help: "How long an unacknowledged message is kept, 10m to 31d. " + pubsubDurationHelp},
		{Name: "retainAcked", Label: "Retain acknowledged messages", Type: "checkbox", Default: "false", Section: delivery,
			Help: "Keeps acknowledged messages for the retention duration too, so a seek can replay them."},
		{Name: "messageOrdering", Label: "Message ordering", Type: "checkbox", Default: "false", Section: delivery,
			Help: "Messages with the same ordering key are delivered in the order they were published. " +
				"It cannot be changed once the subscription exists."},
		{Name: "filter", Label: "Subscription filter", Type: "text", Section: delivery,
			Help: `Optional. Only messages whose attributes match are delivered, such as attributes.env = "prod"; ` +
				"the others are acknowledged for you. It cannot be changed once the subscription exists."},
		{Name: "exactlyOnce", Label: "Exactly-once delivery", Type: "checkbox", Default: "false", Section: delivery,
			Help: pubsubExactlyOnceHelp},
		{Name: "expiration", Label: "Expiration period", Type: "text", Default: "31d", Section: delivery,
			Help: pubsubExpirationHelp},
		{Name: "minBackoff", Label: "Minimum backoff", Type: "text", Section: retries,
			Help: "0s to 600s. Both empty: no retry policy, and a message that is not acknowledged is redelivered at once. " +
				"One left empty takes its default, 10s minimum or 600s maximum."},
		{Name: "maxBackoff", Label: "Maximum backoff", Type: "text", Section: retries,
			Help: "0s to 600s, and no less than the minimum. " + pubsubDurationHelp},
		{Name: "deadLetterTopic", Label: "Dead-letter topic", Type: "text", Section: deadLetter,
			Help: "Optional. projects/{project}/topics/{topic}, a topic that exists. Empty: no dead-letter policy."},
		{Name: "maxDeliveryAttempts", Label: "Maximum delivery attempts", Type: "number", Default: "5", Section: deadLetter,
			Help: "5 to 100. After this many delivery attempts a message is published to the dead-letter topic."},
	}
}

// subscriptionFromForm is the subscription Create subscription describes. A
// range is left to CreateSubscription, whose refusal names it; what is
// checked here is only what it could not be sent as.
func subscriptionFromForm(project, topic string, values map[string]string) (*pubsubpb.Subscription, error) {
	id := strings.TrimSpace(values["name"])
	if id == "" {
		return nil, errors.New("subscription ID is required")
	}
	sub := &pubsubpb.Subscription{
		Name:                      fmt.Sprintf("projects/%s/subscriptions/%s", project, id),
		Topic:                     topic,
		RetainAckedMessages:       values["retainAcked"] == "true",
		EnableMessageOrdering:     values["messageOrdering"] == "true",
		EnableExactlyOnceDelivery: values["exactlyOnce"] == "true",
		Filter:                    strings.TrimSpace(values["filter"]),
	}
	var errs []error
	if v := strings.TrimSpace(values["ackDeadline"]); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 10 || n > 600 {
			errs = append(errs, errors.New("the acknowledgement deadline must be 10 to 600 seconds"))
		}
		sub.AckDeadlineSeconds = int32(n)
	}
	if ep := strings.TrimSpace(values["pushEndpoint"]); ep != "" {
		sub.PushConfig = &pubsubpb.PushConfig{PushEndpoint: ep}
		if sub.EnableExactlyOnceDelivery {
			errs = append(errs, errors.New(pubsubExactlyOncePush))
		}
	}
	switch v := strings.TrimSpace(values["expiration"]); strings.ToLower(v) {
	case "":
		// Google's default, which the front gives a subscription with none.
	case "never":
		sub.ExpirationPolicy = &pubsubpb.ExpirationPolicy{}
	default:
		ttl, err := parsePubSubDuration("Expiration period", v)
		switch {
		case err != nil:
			errs = append(errs, err)
		case ttl.AsDuration() == 0:
			errs = append(errs, errors.New("expiration period: 0 is not a period; give one of at least 1d, or never"))
		default:
			sub.ExpirationPolicy = &pubsubpb.ExpirationPolicy{Ttl: ttl}
		}
	}
	retention, err := parsePubSubDuration("Message retention duration", values["messageRetention"])
	if err != nil {
		errs = append(errs, err)
	}
	sub.MessageRetentionDuration = retention

	minB, errMin := parsePubSubDuration("Minimum backoff", values["minBackoff"])
	maxB, errMax := parsePubSubDuration("Maximum backoff", values["maxBackoff"])
	if errMin != nil || errMax != nil {
		errs = append(errs, errors.Join(errMin, errMax))
	} else if minB != nil || maxB != nil {
		sub.RetryPolicy = &pubsubpb.RetryPolicy{MinimumBackoff: minB, MaximumBackoff: maxB}
	}

	if dl := strings.TrimSpace(values["deadLetterTopic"]); dl != "" {
		n, err := strconv.ParseInt(strings.TrimSpace(values["maxDeliveryAttempts"]), 10, 32)
		if err != nil {
			errs = append(errs, fmt.Errorf("maximum delivery attempts: %q is not a whole number", values["maxDeliveryAttempts"]))
		}
		sub.DeadLetterPolicy = &pubsubpb.DeadLetterPolicy{DeadLetterTopic: dl, MaxDeliveryAttempts: int32(n)}
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return sub, nil
}

// createSubscription sends CreateSubscription once. The official client
// retries UNKNOWN, which is how the emulator answers a filter it cannot
// parse, so with its default settings a malformed filter is retried until
// the request's deadline and the form waits for a minute to be told nothing.
func createSubscription(ctx context.Context, sc *vkit.SubscriptionAdminClient, sub *pubsubpb.Subscription) error {
	_, err := sc.CreateSubscription(ctx, sub, gax.WithRetry(func() gax.Retryer { return nil }))
	if err != nil && sub.GetFilter() != "" && status.Code(err) == codes.Unknown {
		return fmt.Errorf("%s: %w", pubsubFilterRefusedAtCreate, err)
	}
	return err
}
