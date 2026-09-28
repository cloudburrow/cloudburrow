package main

// Pub/Sub: Edit topic and Edit subscription (#786).
//
// UpdateTopic and UpdateSubscription are verified with the official client,
// and the console offered no way to call them: a topic's retention and a
// subscription's ack deadline, delivery, retries and dead lettering were
// readable on their pages and fixed. Each edit is one call through the
// official pubsub/v2 admin client against the emulator, the call an SDK
// makes, so what the emulator refuses is refused with its own message.
//
// The forms offer only what the pinned emulator applies, measured against it
// (cloud-pubsub-emulator 0.8.35): UpdateTopic takes message_retention_duration
// and refuses labels, message_storage_policy and kms_key_name as "not a known
// Topic field"; UpdateSubscription takes ack_deadline_seconds,
// message_retention_duration, retain_acked_messages, push_config,
// retry_policy and dead_letter_policy, refuses labels the same way, says
// updating filter and expiration_policy "is currently unsupported in the
// Pub/Sub Emulator", and calls topic, enable_message_ordering and detached
// "not mutable". enable_exactly_once_delivery it updates, in both
// directions, and acts on the change (#880,
// TestPubSubExactlyOnceCanBeChanged), so the form edits it; with a push
// endpoint it is refused, as CloudBurrow's front refuses the pair. The
// expiration the front applies itself, since the front is what enforces it
// (#891, TestPubSubExpirationCanBeUpdated), so the form edits it too, and so
// are both resources' labels, which the front applies in place of the
// emulator's refusal (#949, TestPubSubUpdateLabels). Those
// that cannot change are shown, disabled, or named in the form's note with
// the emulator's own words; none is a field that saves nothing.

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/pubsubfront"
)

const pubsubFilterRefusal = "Updating the filter field is currently unsupported in the Pub/Sub Emulator."

// pubsubLabelsHelp is the labels field's help on both forms. The emulator
// refuses a labels update; CloudBurrow's front applies it (#949).
const pubsubLabelsHelp = "One key=value per line, such as env=dev. Empty: no labels. " +
	"Applied by CloudBurrow's Pub/Sub front, since the emulator refuses a labels update."

// labelsField is the labels field of both forms, prefilled with l.
func labelsField(l map[string]string, section string) console.Field {
	return console.Field{Name: "labels", Label: "Labels", Type: "map", Default: console.FormatMap(l), Section: section,
		Help: pubsubLabelsHelp}
}

// changedLabels reads the labels field and reports whether it changes cur;
// a form posted without it (an older page) changes nothing.
func changedLabels(values map[string]string, cur map[string]string) (map[string]string, bool, error) {
	v, ok := values["labels"]
	if !ok {
		return nil, false, nil
	}
	l, err := console.ParseMap(v)
	if err != nil {
		return nil, false, fmt.Errorf("labels: %w", err)
	}
	return l, len(l) != len(cur) || !maps.Equal(l, cur), nil
}

const pubsubDurationHelp = "A duration such as 7d, 36h, 10m or 30s."

// pubsubRetentionUnclearable is the refusal of an emptied topic retention.
// Measured: the emulator's UpdateTopic with the retention mask and no value
// sets the topic's retention to 31 days rather than removing it, so an empty
// field would save something other than what it shows.
const pubsubRetentionUnclearable = "a topic's message retention cannot be removed on this emulator once set: " +
	"its UpdateTopic sets an empty retention to 31 days. Give a duration from 10m to 31d"

// formatPubSubDuration shows a duration the way the form reads it back:
// whole days as days, anything else in Go's form.
func formatPubSubDuration(d *durationpb.Duration) string {
	if d == nil {
		return ""
	}
	v := d.AsDuration()
	if v > 0 && v%(24*time.Hour) == 0 {
		return strconv.FormatInt(int64(v/(24*time.Hour)), 10) + "d"
	}
	return v.String()
}

// parsePubSubDuration reads a duration field: Go's form, or whole days as
// "7d". Empty is nil, which each caller gives its own meaning.
func parsePubSubDuration(label, raw string) (*durationpb.Duration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	if days, ok := strings.CutSuffix(raw, "d"); ok {
		if n, err := strconv.ParseUint(days, 10, 16); err == nil {
			return durationpb.New(time.Duration(n) * 24 * time.Hour), nil
		}
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 {
		return nil, fmt.Errorf("%s: %q is not a duration; %s", label, raw, pubsubDurationHelp)
	}
	return durationpb.New(d), nil
}

func sameDuration(a, b *durationpb.Duration) bool {
	return (a == nil) == (b == nil) && a.AsDuration() == b.AsDuration()
}

// refuseImmutable refuses a request that changes a field the form shows
// disabled. The browser does not send those; a request that does is asking
// for a change the form never offered.
func refuseImmutable(form *console.EditForm, values map[string]string) error {
	for _, f := range form.Fields {
		if v, ok := values[f.Name]; ok && f.Immutable && strings.TrimSpace(v) != f.Default {
			return fmt.Errorf("%s cannot be changed from the console: %s", f.Label, f.Help)
		}
	}
	return nil
}

// ---------------------------------------------------------------- topics

// topicEditForm is Edit topic, prefilled from the topic: its retention, the
// one field the emulator's UpdateTopic applies.
func topicEditForm(t *pubsubpb.Topic) *console.EditForm {
	retention := console.Field{
		Name: "messageRetention", Label: "Message retention duration", Type: "text",
		Default: formatPubSubDuration(t.GetMessageRetentionDuration()),
		Help: "How long the topic keeps every message published to it, acknowledged or not, from 10m to 31d. " +
			pubsubDurationHelp + " Empty: the topic keeps none.",
	}
	if t.GetMessageRetentionDuration() != nil {
		// Measured: an emptied retention is saved as 31 days.
		retention.Required = true
		retention.Help = "How long the topic keeps every message published to it, acknowledged or not, from 10m to 31d. " +
			pubsubDurationHelp + " Once set it cannot be removed here: the emulator sets an empty retention to 31 days."
	}
	return &console.EditForm{
		Label: "Edit topic",
		Fields: []console.Field{
			{Name: "name", Label: "Topic ID", Type: "text", Default: lastSegment(t.GetName()), Immutable: true,
				Help: "A topic's name cannot be changed."},
			retention,
			labelsField(t.GetLabels(), ""),
		},
		Note: "Saved through UpdateTopic. The schema, the message storage policy and the KMS key cannot be " +
			"changed on this emulator: its UpdateTopic refuses each as \"not a known Topic field\".",
	}
}

// Edit implements console.Editor for a topic: UpdateTopic with a mask naming
// the retention when it changed. A subscription row of the topic page, at
// [topic, subscription], is edited on the subscription's own page.
func (p pubsubProvider) Edit(ctx context.Context, project string, path []string, values map[string]string) error {
	if len(path) != 1 {
		return errors.New("Pub/Sub edits on this screen apply to a topic; a subscription is edited on its own page")
	}
	name := path[0]
	if !strings.HasPrefix(name, "projects/"+project+"/topics/") {
		return fmt.Errorf("%s is not a topic of project %s", name, project)
	}
	c, err := p.client(ctx, project)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()

	cur, err := c.TopicAdminClient.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: name})
	if err != nil {
		return err
	}
	if err := refuseImmutable(topicEditForm(cur), values); err != nil {
		return err
	}
	retention, err := parsePubSubDuration("Message retention duration", values["messageRetention"])
	if err != nil {
		return err
	}
	if retention == nil && cur.GetMessageRetentionDuration() != nil {
		return errors.New(pubsubRetentionUnclearable)
	}
	labels, labelsChanged, err := changedLabels(values, cur.GetLabels())
	if err != nil {
		return err
	}
	upd := &pubsubpb.Topic{Name: name}
	var paths []string
	if !sameDuration(retention, cur.GetMessageRetentionDuration()) {
		upd.MessageRetentionDuration = retention
		paths = append(paths, "message_retention_duration")
	}
	if labelsChanged {
		upd.Labels = labels
		paths = append(paths, "labels")
	}
	if len(paths) == 0 {
		return nil
	}
	_, err = c.TopicAdminClient.UpdateTopic(ctx, &pubsubpb.UpdateTopicRequest{
		Topic: upd, UpdateMask: &fieldmaskpb.FieldMask{Paths: paths},
	})
	return err
}

// ---------------------------------------------------------- subscriptions

// pushOrPull is whether a subscription's delivery is its push config, the
// only delivery the form edits. A BigQuery, Cloud Storage or Bigtable
// subscription keeps its delivery as it is.
func pushOrPull(s *pubsubpb.Subscription) bool {
	d := deliveryType(s)
	return d == "Push" || d == "Pull"
}

// subscriptionEditForm is Edit subscription, prefilled from the
// subscription: what the emulator's UpdateSubscription applies, then what it
// refuses or cannot change, shown disabled with the reason.
func subscriptionEditForm(s *pubsubpb.Subscription) *console.EditForm {
	const delivery, retries, deadLetter, fixed = "Delivery", "Retry policy", "Dead lettering", "Not editable"
	fields := []console.Field{
		{Name: "name", Label: "Subscription ID", Type: "text", Default: lastSegment(s.GetName()), Immutable: true,
			Help: "A subscription's name cannot be changed."},
		{Name: "topic", Label: "Topic", Type: "text", Default: subscriptionTopic(s), Immutable: true,
			Help: "A subscription's topic cannot be changed: the emulator says the topic field is not mutable."},
	}
	if pushOrPull(s) {
		push := s.GetPushConfig()
		fields = append(fields,
			console.Field{Name: "pushEndpoint", Label: "Push endpoint", Type: "url", Default: push.GetPushEndpoint(), Section: delivery,
				Help: "Empty for a pull subscription. An http or https URL receives each message as an HTTP POST: " +
					"giving one switches a pull subscription to push, and emptying it switches back to pull."},
			console.Field{Name: "pushAttributes", Label: "Push attributes", Type: "map", Default: console.FormatMap(push.GetAttributes()),
				Section: delivery, Help: "Optional, with a push endpoint only. One key=value per line, such as x-goog-version=v1."},
		)
	}
	fields = append(fields,
		console.Field{Name: "ackDeadline", Label: "Acknowledgement deadline (seconds)", Type: "number", Required: true,
			Default: strconv.Itoa(int(s.GetAckDeadlineSeconds())), Section: delivery,
			Help: "How long a subscriber has to acknowledge a message before it is redelivered, 10 to 600 seconds."},
		console.Field{Name: "messageRetention", Label: "Message retention duration", Type: "text", Required: true,
			Default: formatPubSubDuration(s.GetMessageRetentionDuration()), Section: delivery,
			Help: "How long an unacknowledged message is kept, 10m to 31d. " + pubsubDurationHelp},
		console.Field{Name: "retainAcked", Label: "Retain acknowledged messages", Type: "checkbox",
			Default: strconv.FormatBool(s.GetRetainAckedMessages()), Section: delivery,
			Help: "Keeps acknowledged messages for the retention duration too, so a seek can replay them."},
		console.Field{Name: "exactlyOnce", Label: "Exactly-once delivery", Type: "checkbox",
			Default: strconv.FormatBool(s.GetEnableExactlyOnceDelivery()), Section: delivery,
			Help: pubsubExactlyOnceHelp},
		console.Field{Name: "expiration", Label: "Expiration period", Type: "text",
			Default: formatPubSubExpiration(s.GetExpirationPolicy()), Section: delivery,
			Help: pubsubExpirationHelp},

		console.Field{Name: "minBackoff", Label: "Minimum backoff", Type: "text",
			Default: formatPubSubDuration(s.GetRetryPolicy().GetMinimumBackoff()), Section: retries,
			Help: "0s to 600s. Both empty: no retry policy, and a message that is not acknowledged is redelivered at once. " +
				"One left empty takes its default, 10s minimum or 600s maximum."},
		console.Field{Name: "maxBackoff", Label: "Maximum backoff", Type: "text",
			Default: formatPubSubDuration(s.GetRetryPolicy().GetMaximumBackoff()), Section: retries,
			Help: "0s to 600s, and no less than the minimum. " + pubsubDurationHelp},

		console.Field{Name: "deadLetterTopic", Label: "Dead-letter topic", Type: "text",
			Default: s.GetDeadLetterPolicy().GetDeadLetterTopic(), Section: deadLetter,
			Help: "projects/{project}/topics/{topic}, a topic that exists. Empty: no dead-letter policy."},
		console.Field{Name: "maxDeliveryAttempts", Label: "Maximum delivery attempts", Type: "number",
			Default: strconv.Itoa(int(max(s.GetDeadLetterPolicy().GetMaxDeliveryAttempts(), 5))), Section: deadLetter,
			Help: "5 to 100. After this many delivery attempts a message is published to the dead-letter topic."},
		labelsField(s.GetLabels(), "Labels"),
	)
	fields = append(fields,
		console.Field{Name: "filter", Label: "Filter", Type: "text", Default: s.GetFilter(), Immutable: true, Section: fixed,
			Help: "The emulator refuses a change: \"" + pubsubFilterRefusal + "\""},
		console.Field{Name: "messageOrdering", Label: "Message ordering", Type: "text",
			Default: yesNo(s.GetEnableMessageOrdering()), Immutable: true, Section: fixed,
			Help: "Set when the subscription is created; the emulator says the field is not mutable."},
	)
	note := "Saved through UpdateSubscription, with an update mask naming each field that changed; switching between " +
		"push and pull is its push_config."
	if !pushOrPull(s) {
		note += " This subscription delivers to " + deliveryType(s) + ", which the form leaves as it is."
	}
	return &console.EditForm{Label: "Edit subscription", Fields: fields, Note: note}
}

// formatPubSubExpiration shows a policy the way the expiration field reads
// it: its ttl, never for a policy without one, and empty for none, which is
// Google's default.
func formatPubSubExpiration(p *pubsubpb.ExpirationPolicy) string {
	switch {
	case p == nil:
		return ""
	case p.GetTtl().AsDuration() <= 0:
		return "never"
	}
	return formatPubSubDuration(p.GetTtl())
}

// Edit implements console.Editor for a subscription: one UpdateSubscription
// whose mask names each field the form changed, so a field left as it was is
// not sent and a subscription saved unchanged is not written at all.
func (p pubsubSubscriptionsProvider) Edit(ctx context.Context, project string, path []string, values map[string]string) error {
	if len(path) != 1 {
		return errors.New("Pub/Sub subscription edits apply to one subscription")
	}
	name := path[0]
	if !strings.HasPrefix(name, "projects/"+project+"/subscriptions/") {
		return fmt.Errorf("%s is not a subscription of project %s", name, project)
	}
	c, err := p.topics().client(ctx, project)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()

	cur, err := c.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: name})
	if err != nil {
		return err
	}
	if err := refuseImmutable(subscriptionEditForm(cur), values); err != nil {
		return err
	}

	upd := &pubsubpb.Subscription{Name: name}
	var paths []string
	var errs []error

	ack, err := strconv.ParseInt(strings.TrimSpace(values["ackDeadline"]), 10, 32)
	if err != nil {
		errs = append(errs, fmt.Errorf("acknowledgement deadline: %q is not a whole number of seconds", values["ackDeadline"]))
	} else if int32(ack) != cur.GetAckDeadlineSeconds() {
		upd.AckDeadlineSeconds = int32(ack)
		paths = append(paths, "ack_deadline_seconds")
	}

	retention, err := parsePubSubDuration("Message retention duration", values["messageRetention"])
	switch {
	case err != nil:
		errs = append(errs, err)
	case retention == nil:
		errs = append(errs, errors.New("message retention duration is required: 10m to 31d"))
	case !sameDuration(retention, cur.GetMessageRetentionDuration()):
		upd.MessageRetentionDuration = retention
		paths = append(paths, "message_retention_duration")
	}

	if retain := values["retainAcked"] == "true"; retain != cur.GetRetainAckedMessages() {
		upd.RetainAckedMessages = retain
		paths = append(paths, "retain_acked_messages")
	}

	// A form posted without the field (an older page) leaves it as it is.
	exactlyOnce := cur.GetEnableExactlyOnceDelivery()
	if v, ok := values["exactlyOnce"]; ok {
		exactlyOnce = v == "true"
	}
	if exactlyOnce != cur.GetEnableExactlyOnceDelivery() {
		upd.EnableExactlyOnceDelivery = exactlyOnce
		paths = append(paths, "enable_exactly_once_delivery")
	}

	// Applied by CloudBurrow's front (#891). Empty is Google's default, as
	// on create; a form posted without the field leaves it as it is.
	if v, ok := values["expiration"]; ok {
		policy, err := parsePubSubExpiration(v)
		switch {
		case err != nil:
			errs = append(errs, err)
		case policy == nil && cur.GetExpirationPolicy() == nil:
		default:
			if policy == nil {
				policy = &pubsubpb.ExpirationPolicy{Ttl: durationpb.New(pubsubfront.DefaultTTL)}
			}
			if !proto.Equal(policy, cur.GetExpirationPolicy()) {
				upd.ExpirationPolicy = policy
				paths = append(paths, "expiration_policy")
			}
		}
	}

	// Applied by CloudBurrow's front (#949).
	if labels, changed, err := changedLabels(values, cur.GetLabels()); err != nil {
		errs = append(errs, err)
	} else if changed {
		upd.Labels = labels
		paths = append(paths, "labels")
	}

	if pushOrPull(cur) {
		endpoint := strings.TrimSpace(values["pushEndpoint"])
		attrs, err := console.ParseMap(values["pushAttributes"])
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("push attributes: %w", err))
		case endpoint == "" && len(attrs) > 0:
			// The emulator stores attributes with no endpoint, on what is
			// then a pull subscription they do nothing for.
			errs = append(errs, errors.New("push attributes need a push endpoint; empty both for a pull subscription"))
		case endpoint != cur.GetPushConfig().GetPushEndpoint() || !maps.Equal(attrs, cur.GetPushConfig().GetAttributes()):
			push := &pubsubpb.PushConfig{}
			if endpoint != "" {
				// What the form does not show (an OIDC token, the payload
				// wrapper) is kept as it is.
				if cur.GetPushConfig() != nil {
					push = proto.Clone(cur.GetPushConfig()).(*pubsubpb.PushConfig)
				}
				push.PushEndpoint, push.Attributes = endpoint, attrs
			}
			upd.PushConfig = push
			paths = append(paths, "push_config")
		}
	}

	minB, errMin := parsePubSubDuration("Minimum backoff", values["minBackoff"])
	maxB, errMax := parsePubSubDuration("Maximum backoff", values["maxBackoff"])
	if errMin != nil || errMax != nil {
		errs = append(errs, errors.Join(errMin, errMax))
	} else {
		var retry *pubsubpb.RetryPolicy
		if minB != nil || maxB != nil {
			retry = &pubsubpb.RetryPolicy{MinimumBackoff: minB, MaximumBackoff: maxB}
		}
		cr := cur.GetRetryPolicy()
		if (retry == nil) != (cr == nil) || (retry != nil &&
			(!sameDuration(minB, cr.GetMinimumBackoff()) || !sameDuration(maxB, cr.GetMaximumBackoff()))) {
			upd.RetryPolicy = retry
			paths = append(paths, "retry_policy")
		}
	}

	var dead *pubsubpb.DeadLetterPolicy
	if topic := strings.TrimSpace(values["deadLetterTopic"]); topic != "" {
		n, err := strconv.ParseInt(strings.TrimSpace(values["maxDeliveryAttempts"]), 10, 32)
		if err != nil {
			errs = append(errs, fmt.Errorf("maximum delivery attempts: %q is not a whole number", values["maxDeliveryAttempts"]))
		}
		dead = &pubsubpb.DeadLetterPolicy{DeadLetterTopic: topic, MaxDeliveryAttempts: int32(n)}
	}
	cd := cur.GetDeadLetterPolicy()
	if (dead == nil) != (cd == nil) || (dead != nil &&
		(dead.GetDeadLetterTopic() != cd.GetDeadLetterTopic() || dead.GetMaxDeliveryAttempts() != cd.GetMaxDeliveryAttempts())) {
		upd.DeadLetterPolicy = dead
		paths = append(paths, "dead_letter_policy")
	}

	// The pair the front refuses, refused here with the form's words.
	resultPush := cur.GetPushConfig().GetPushEndpoint()
	if pushOrPull(cur) {
		resultPush = strings.TrimSpace(values["pushEndpoint"])
	}
	if exactlyOnce && (resultPush != "" || !pushOrPull(cur)) {
		errs = append(errs, errors.New(pubsubExactlyOncePush))
	}

	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	if len(paths) == 0 {
		return nil
	}
	_, err = c.SubscriptionAdminClient.UpdateSubscription(ctx, &pubsubpb.UpdateSubscriptionRequest{
		Subscription: upd, UpdateMask: &fieldmaskpb.FieldMask{Paths: paths},
	})
	return err
}

var (
	_ console.Editor = pubsubProvider{}
	_ console.Editor = pubsubSubscriptionsProvider{}
)
