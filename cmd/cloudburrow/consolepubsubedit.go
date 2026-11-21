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
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/pubsubfront"
)

const pubsubFilterRefusal = "Updating the filter field is currently unsupported in the Pub/Sub Emulator."

// pubsubLabelRules is Google's rules on labels, which CloudBurrow's front
// enforces (#962, internal/pubsubfront/labelrules.go) and the forms check.
const pubsubLabelRules = "Up to 64 labels. A key is 1 to 63 lowercase letters, international characters, " +
	"digits, _ and -, starting with a lowercase letter or an international character; a value is 0 to 63 of " +
	"the same characters."

// pubsubLabelsHelp is the labels field's help on the edit forms. The
// emulator refuses a labels update; CloudBurrow's front applies it (#949).
const pubsubLabelsHelp = "One key=value per line, such as env=dev. Empty: no labels. " + pubsubLabelRules +
	" Applied by CloudBurrow's Pub/Sub front, since the emulator refuses a labels update."

// labelsField is the labels field of the edit forms, prefilled with l.
func labelsField(l map[string]string, section string) console.Field {
	return pubsubLabelsField(console.FormatMap(l), section, pubsubLabelsHelp)
}

// pubsubLabelsField is a labels field holding the browser to Google's
// rules, as the front holds the API to them.
func pubsubLabelsField(def, section, help string) console.Field {
	return console.Field{Name: "labels", Label: "Labels", Type: "map", Default: def, Section: section, Help: help,
		KeyPattern: pubsubfront.LabelKeyPattern, ValuePattern: pubsubfront.LabelValuePattern,
		KeyHelp: "A key is 1 to 63 lowercase letters, international characters, digits, _ and -, " +
			"starting with a lowercase letter or an international character.",
		ValueHelp:  "A value is 0 to 63 lowercase letters, international characters, digits, _ and -.",
		MaxEntries: pubsubfront.MaxLabels}
}

// formLabels reads a labels field, refused as the front refuses it, with
// its message. ok is false when the form did not send it.
func formLabels(values map[string]string) (l map[string]string, ok bool, err error) {
	v, ok := values["labels"]
	if !ok {
		return nil, false, nil
	}
	l, err = console.ParseMap(v)
	if err != nil {
		return nil, false, fmt.Errorf("labels: %w", err)
	}
	if err := pubsubfront.CheckLabels(l); err != nil {
		return nil, false, errors.New(status.Convert(err).Message())
	}
	return l, true, nil
}

// changedLabels reads the labels field and reports whether it changes cur;
// a form posted without it (an older page) changes nothing.
func changedLabels(values map[string]string, cur map[string]string) (map[string]string, bool, error) {
	l, ok, err := formLabels(values)
	if !ok || err != nil {
		return nil, false, err
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

// topicEncoding is a topic's message encoding, JSON when it has none.
func topicEncoding(t *pubsubpb.Topic) string {
	if t.GetSchemaSettings().GetEncoding() == pubsubpb.Encoding_BINARY {
		return "BINARY"
	}
	return "JSON"
}

// changedSchemaSettings is the binding the form asks for, and whether it
// differs from the topic's; nil is no schema. A schema and encoding left as
// they are keep the binding's revision range.
func changedSchemaSettings(project string, values map[string]string, cur *pubsubpb.Topic) (*pubsubpb.SchemaSettings, bool, error) {
	raw, sent := values["schema"]
	if !sent {
		return nil, false, nil
	}
	schema := strings.TrimSpace(raw)
	if schema != "" && !strings.HasPrefix(schema, "projects/") && schema != "_deleted-schema_" {
		schema = "projects/" + project + "/schemas/" + schema
	}
	enc := pubsubpb.Encoding_JSON
	switch strings.TrimSpace(values["schemaEncoding"]) {
	case "", "JSON":
	case "BINARY":
		enc = pubsubpb.Encoding_BINARY
	default:
		return nil, false, fmt.Errorf("message encoding: %q is neither JSON nor BINARY", values["schemaEncoding"])
	}
	old := cur.GetSchemaSettings()
	if schema == "" {
		return nil, old != nil, nil
	}
	if old != nil && old.GetSchema() == schema && (old.GetEncoding() == enc || values["schemaEncoding"] == "") {
		return nil, false, nil
	}
	return &pubsubpb.SchemaSettings{Schema: schema, Encoding: enc}, true, nil
}

// Edit implements console.Editor for a topic: UpdateTopic with a mask naming
// the retention, the labels and the schema settings (#1446) that changed. A subscription row of the topic page, at
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
	// Google's Edit topic page (#1847); a request with the older dialog's
	// fields is read as it was.
	if _, ok := values["enableRetention"]; ok {
		if values, err = topicEditValues(values); err != nil {
			return err
		}
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
	ss, schemaChanged, err := changedSchemaSettings(project, values, cur)
	if err != nil {
		return err
	}
	if schemaChanged {
		upd.SchemaSettings = ss
		paths = append(paths, "schema_settings")
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

// pushOrPull is whether a subscription's delivery is its push config. A
// Bigtable subscription keeps its delivery as it is; a BigQuery or Cloud
// Storage one is changed on Edit subscription's page (#2051).
func pushOrPull(s *pubsubpb.Subscription) bool {
	d := deliveryType(s)
	return d == "Push" || d == "Pull"
}

// exportUpdates are the export configs an edit changes, by update mask
// path: each one the chosen delivery sets or clears that differs from the
// subscription's, read from Create subscription's fields.
func exportUpdates(cur *pubsubpb.Subscription, delivery string, values map[string]string) (*pubsubpb.Subscription, []string, error) {
	want := &pubsubpb.Subscription{}
	form := map[string]string{}
	for k, v := range values {
		form[k] = v
	}
	form["deliveryType"] = delivery
	if err := exportFromForm(want, form); err != nil {
		return nil, nil, err
	}
	if delivery == deliveryBigQuery && want.GetBigqueryConfig().GetTable() == "" {
		return nil, nil, errors.New("table is required to write to BigQuery")
	}
	if delivery == deliveryStorage && want.GetCloudStorageConfig().GetBucket() == "" {
		return nil, nil, errors.New("Cloud Storage bucket is required to write to Cloud Storage")
	}
	var paths []string
	bq := proto.Clone(cur).(*pubsubpb.Subscription).GetBigqueryConfig()
	if bq != nil {
		bq.State = 0
	}
	if !proto.Equal(bq, want.GetBigqueryConfig()) {
		paths = append(paths, "bigquery_config")
	}
	cs := proto.Clone(cur).(*pubsubpb.Subscription).GetCloudStorageConfig()
	if cs != nil {
		cs.State = 0
		if w := want.GetCloudStorageConfig(); w != nil {
			if cs.MaxDuration != nil && w.MaxDuration == nil {
				w.MaxDuration = cs.MaxDuration
			}
			// No output format is text, which the form shows.
			if cs.OutputFormat == nil && w.GetTextConfig() != nil {
				w.OutputFormat = nil
			}
		}
	}
	if !proto.Equal(cs, want.GetCloudStorageConfig()) {
		paths = append(paths, "cloud_storage_config")
	}
	return want, paths, nil
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
	// Google's Edit subscription page (#1847); a request with the older
	// dialog's fields is read as it was.
	if _, ok := values["deliveryType"]; ok {
		if values, err = subscriptionEditValues(project, cur, values); err != nil {
			return err
		}
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

	// Edit subscription's page chose a delivery type (#2051): the export
	// configs it sets or clears.
	delivery, onPage := values[editDeliveryKey]
	if onPage && editDelivery(cur) != "" {
		want, exportPaths, err := exportUpdates(cur, delivery, values)
		if err != nil {
			errs = append(errs, err)
		} else {
			upd.BigqueryConfig, upd.CloudStorageConfig = want.GetBigqueryConfig(), want.GetCloudStorageConfig()
			paths = append(paths, exportPaths...)
		}
	}
	if pushOrPull(cur) || (onPage && editDelivery(cur) != "") {
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
	resultExport := !pushOrPull(cur)
	if pushOrPull(cur) || (onPage && editDelivery(cur) != "") {
		resultPush = strings.TrimSpace(values["pushEndpoint"])
	}
	if onPage && editDelivery(cur) != "" {
		resultExport = delivery == deliveryBigQuery || delivery == deliveryStorage
	}
	if exactlyOnce && (resultPush != "" || resultExport) {
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
