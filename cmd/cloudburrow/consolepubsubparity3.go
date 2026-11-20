package main

// Pub/Sub console parity, round 3 (#1847): the edit flows as Google's
// pages, and the lists' info panel.
//
//   - Edit subscription is a page of its own, laid out as Create
//     subscription is, ending in Update: "In the subscription details page,
//     click Edit. For Delivery type, select the new delivery type. ...
//     Click Update" (https://docs.cloud.google.com/pubsub/docs/manage-subscriptions,
//     read 2026-11-20). Edit topic is drawn the same way, with Create
//     topic's Use a schema and Enable message retention.
//   - The Topics, Subscriptions and Snapshots lists check a row to open the
//     info panel, with its Permissions tab ("select the topics. If the info
//     panel is hidden, click Show info panel. In the Permissions tab, click
//     Add principal", https://docs.cloud.google.com/pubsub/docs/access-control)
//     and its Labels tab ("Select a resource and change to the LABELS tab",
//     https://docs.cloud.google.com/pubsub/docs/labels, both read 2026-11-20).
//     A snapshot has no Permissions tab: Google's console sets no snapshot
//     policy (docs/console-parity.md, Pub/Sub).
//
// Every change is still the official client's UpdateTopic,
// UpdateSubscription or UpdateSnapshot against the emulator, or
// CloudBurrow's front before it, as the dialogs' were.

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// actEditLabels is the info panel's Labels tab, saved with Save.
const actEditLabels = "edit-labels"

// pubsubNoExportOnEdit is why Edit subscription draws the export delivery
// types disabled on a pull or push subscription.
const pubsubNoExportOnEdit = "Not available on Edit locally: CloudBurrow does not yet switch an existing pull or push " +
	"subscription to an export subscription. Create a subscription with this delivery type instead."

// --- the info panel ---------------------------------------------------------

// pubsubLabelsAction is the Labels tab's form, prefilled with labels.
// Linked: it is not a button on the resource's page, whose Edit is
// Google's.
func pubsubLabelsAction(labels map[string]string) console.Action {
	f := labelsField(labels, "")
	f.Help = "One key=value per line, such as env=dev. Empty: no labels. " + pubsubLabelRules
	return console.Action{ID: actEditLabels, Label: "Labels", Submit: "Save", Linked: true, Fields: []console.Field{f}}
}

// pubsubInfoTabs are a list row's info panel tabs: Permissions (a topic's
// or a subscription's), then Labels.
func pubsubInfoTabs(labels map[string]string, noun string, permissions bool) []console.InfoTab {
	var tabs []console.InfoTab
	if permissions {
		tabs = append(tabs, console.InfoTab{ID: "permissions", Label: "Permissions", Permissions: true})
	}
	props := make([]console.Property, 0, len(labels))
	for _, k := range sortedKeys(labels) {
		props = append(props, console.Property{Label: k, Value: labels[k]})
	}
	form := pubsubLabelsAction(labels)
	return append(tabs, console.InfoTab{ID: "labels", Label: "Labels", Properties: props,
		Empty: "This " + noun + " has no labels.", Form: &form})
}

// topicLabelsUpdate is the UpdateTopic that sets a topic's labels from
// the Labels tab, or nil when they are unchanged.
func topicLabelsUpdate(cur *pubsubpb.Topic, values map[string]string) (*pubsubpb.UpdateTopicRequest, error) {
	l, changed, err := changedLabels(withLabels(values), cur.GetLabels())
	if err != nil || !changed {
		return nil, err
	}
	return &pubsubpb.UpdateTopicRequest{Topic: &pubsubpb.Topic{Name: cur.GetName(), Labels: l},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"labels"}}}, nil
}

// subscriptionLabelsUpdate is topicLabelsUpdate's for a subscription.
func subscriptionLabelsUpdate(cur *pubsubpb.Subscription, values map[string]string) (*pubsubpb.UpdateSubscriptionRequest, error) {
	l, changed, err := changedLabels(withLabels(values), cur.GetLabels())
	if err != nil || !changed {
		return nil, err
	}
	return &pubsubpb.UpdateSubscriptionRequest{Subscription: &pubsubpb.Subscription{Name: cur.GetName(), Labels: l},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"labels"}}}, nil
}

// withLabels is values with an empty labels field when the tab sent none:
// a Labels tab saved empty clears them.
func withLabels(values map[string]string) map[string]string {
	if _, ok := values["labels"]; ok {
		return values
	}
	return map[string]string{"labels": ""}
}

// topicSetLabels applies the Labels tab to a topic.
func (p pubsubProvider) topicSetLabels(ctx context.Context, project, topic string, values map[string]string) error {
	c, err := p.client(ctx, project)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	cur, err := c.TopicAdminClient.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: topic})
	if err != nil {
		return err
	}
	req, err := topicLabelsUpdate(cur, values)
	if err != nil || req == nil {
		return err
	}
	_, err = c.TopicAdminClient.UpdateTopic(ctx, req)
	return err
}

// subscriptionSetLabels applies the Labels tab to a subscription.
func (p pubsubSubscriptionsProvider) subscriptionSetLabels(ctx context.Context, project, sub string, values map[string]string) error {
	c, err := p.topics().client(ctx, project)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	cur, err := c.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: sub})
	if err != nil {
		return err
	}
	req, err := subscriptionLabelsUpdate(cur, values)
	if err != nil || req == nil {
		return err
	}
	_, err = c.SubscriptionAdminClient.UpdateSubscription(ctx, req)
	return err
}

// DetailActions implements console.PathActor for a snapshot: the info
// panel's Labels tab, the one action a snapshot row has.
func (p pubsubSnapshotsProvider) DetailActions(_ context.Context, project string, path []string) []console.Action {
	if project == "" || len(path) != 1 || !strings.HasPrefix(path[0], "projects/"+project+"/snapshots/") {
		return nil
	}
	return []console.Action{pubsubLabelsAction(nil)}
}

// ActAt implements console.PathActor for a snapshot: the Labels tab,
// applied as Edit snapshot applies labels.
func (p pubsubSnapshotsProvider) ActAt(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	if action != actEditLabels {
		return fmt.Errorf("%s is not an action on a snapshot", action)
	}
	return p.Edit(ctx, project, path, withLabels(values))
}

// --- Edit subscription --------------------------------------------------

// secondsOf is a duration as whole seconds, for the retry policy's fields.
func secondsOf(d interface{ AsDuration() time.Duration }, def string) string {
	if d == nil {
		return def
	}
	return strconv.FormatInt(int64(d.AsDuration()/time.Second), 10)
}

// expirationDefaults are the Expiration period's choice and days for a
// policy: Google's 31 days for none, Never expire for one without a ttl.
func expirationDefaults(p *pubsubpb.ExpirationPolicy) (mode, days string) {
	switch {
	case p == nil:
		return expireAfterDays, "31"
	case p.GetTtl().AsDuration() <= 0:
		return expireNever, "31"
	}
	d := p.GetTtl().AsDuration()
	return expireAfterDays, strconv.FormatInt(int64((d+24*time.Hour-1)/(24*time.Hour)), 10)
}

// subscriptionEditForm is Google's Edit subscription page, prefilled from
// the subscription, in Create subscription's order: Subscription ID and
// Pub/Sub topic (fixed), Delivery type, Filter (fixed), Retry policy, Dead
// lettering, Delivery properties, Acknowledgement deadline, Lifetime
// options, Expiration period and Labels, then Update.
func subscriptionEditForm(s *pubsubpb.Subscription) *console.EditForm {
	byName := map[string]console.Field{}
	for _, f := range subscriptionPageFields(false) {
		byName[f.Name] = f
	}
	field := func(name, def string) console.Field {
		f := byName[name]
		f.Default = def
		return f
	}
	retry, minB, maxB := retryImmediately, "10", "600"
	if r := s.GetRetryPolicy(); r != nil {
		retry, minB, maxB = retryBackoff, secondsOf(r.GetMinimumBackoff(), "10"), secondsOf(r.GetMaximumBackoff(), "600")
	}
	dl := s.GetDeadLetterPolicy()
	mode, days := expirationDefaults(s.GetExpirationPolicy())

	fields := []console.Field{
		{Name: "name", Label: "Subscription ID", Type: "text", Default: lastSegment(s.GetName()), Immutable: true,
			Help: "A subscription's name cannot be changed."},
		{Name: "topic", Label: "Pub/Sub topic", Type: "text", Default: subscriptionTopic(s), Immutable: true,
			Help: "A subscription's topic cannot be changed: the emulator says the topic field is not mutable."},
	}
	delivery := field("deliveryType", deliveryType(s))
	if pushOrPull(s) {
		delivery.UnavailableOptions = map[string]string{deliveryBigQuery: pubsubNoExportOnEdit, deliveryStorage: pubsubNoExportOnEdit}
		fields = append(fields, delivery,
			field("pushEndpoint", s.GetPushConfig().GetPushEndpoint()),
			field("pushAttributes", console.FormatMap(s.GetPushConfig().GetAttributes())))
	} else {
		delivery.Options = []string{deliveryType(s)}
		delivery.Immutable = true
		delivery.Help = "This subscription delivers to " + deliveryType(s) + ", which this page leaves as it is."
		fields = append(fields, delivery)
	}
	filter := field("filter", s.GetFilter())
	filter.Immutable = true
	filter.Help = "A subscription's filter cannot be changed once it exists. The emulator refuses a change: \"" +
		pubsubFilterRefusal + "\""
	ordering := field("messageOrdering", strconv.FormatBool(s.GetEnableMessageOrdering()))
	ordering.Immutable = true
	ordering.Help = "Set when the subscription is created; it cannot be changed."
	attempts := "5"
	if dl.GetMaxDeliveryAttempts() > 0 {
		attempts = strconv.Itoa(int(dl.GetMaxDeliveryAttempts()))
	}
	fields = append(fields,
		filter,
		field("retryPolicy", retry), field("minBackoff", minB), field("maxBackoff", maxB),
		field("deadLettering", strconv.FormatBool(dl.GetDeadLetterTopic() != "")),
		field("deadLetterTopic", dl.GetDeadLetterTopic()),
		field("maxDeliveryAttempts", attempts),
		field("exactlyOnce", strconv.FormatBool(s.GetEnableExactlyOnceDelivery())),
		ordering,
		field("ackDeadline", strconv.Itoa(int(s.GetAckDeadlineSeconds()))),
		field("messageRetention", formatPubSubDuration(s.GetMessageRetentionDuration())),
		field("retainAcked", strconv.FormatBool(s.GetRetainAckedMessages())),
		field("expirationMode", mode), field("expirationDays", days),
		labelsField(s.GetLabels(), "Labels"),
	)
	for i := range fields {
		if fields[i].Name == "ackDeadline" || fields[i].Name == "messageRetention" {
			fields[i].Required = true
		}
	}
	return &console.EditForm{Label: "Edit", Title: "Edit subscription", Submit: "Update", Layout: console.EditLayoutPage,
		Fields: fields,
		Note: "Saved through UpdateSubscription, with an update mask naming each field that changed; switching between " +
			"Pull and Push is its push_config."}
}

// subscriptionEditValues turns the Edit subscription page's fields into
// the values Edit reads: a hidden choice's fields dropped, Pull an empty
// push endpoint, and an expiration period left as it was when its days
// were not touched (a period of 36h shows as 2 days).
func subscriptionEditValues(project string, cur *pubsubpb.Subscription, values map[string]string) (map[string]string, error) {
	form := subscriptionEditForm(cur)
	if err := refuseUnavailable(values, form.Fields); err != nil {
		return nil, err
	}
	out := subscriptionPageValues(project, values)
	if values["deliveryType"] == deliveryPull {
		out["pushEndpoint"], out["pushAttributes"] = "", ""
	}
	if _, ok := values["retryPolicy"]; ok && values["retryPolicy"] != retryBackoff {
		out["minBackoff"], out["maxBackoff"] = "", ""
	}
	if _, ok := values["deadLettering"]; ok && values["deadLettering"] != "true" {
		out["deadLetterTopic"] = ""
	}
	mode, days := expirationDefaults(cur.GetExpirationPolicy())
	if values["expirationMode"] == mode && (mode == expireNever || strings.TrimSpace(values["expirationDays"]) == days) {
		delete(out, "expiration")
	}
	for _, k := range []string{"deliveryType", "retryPolicy", "deadLettering", "expirationMode", "expirationDays"} {
		delete(out, k)
	}
	return out, nil
}

// --- Edit topic ---------------------------------------------------------

// retentionParts splits a retention into Create topic's days, hours and
// minutes.
func retentionParts(d time.Duration) (days, hours, minutes string) {
	return strconv.Itoa(int(d / (24 * time.Hour))), strconv.Itoa(int(d % (24 * time.Hour) / time.Hour)),
		strconv.Itoa(int(d % time.Hour / time.Minute))
}

// topicEditForm is Edit topic as a page, prefilled from the topic, with
// Create topic's fields: Topic ID (fixed), Use a schema, Enable message
// retention, Encryption (fixed) and Labels, then Update.
func topicEditForm(t *pubsubpb.Topic) *console.EditForm {
	byName := map[string]console.Field{}
	for _, f := range topicCreatePageFields() {
		byName[f.Name] = f
	}
	field := func(name, def string) console.Field {
		f := byName[name]
		f.Default = def
		return f
	}
	schema := t.GetSchemaSettings().GetSchema()
	enc := encodingJSON
	if t.GetSchemaSettings().GetEncoding() == pubsubpb.Encoding_BINARY {
		enc = encodingBinary
	}
	days, hours, minutes := "7", "0", "0"
	retention := field("enableRetention", strconv.FormatBool(t.GetMessageRetentionDuration() != nil))
	if d := t.GetMessageRetentionDuration(); d != nil {
		days, hours, minutes = retentionParts(d.AsDuration())
		retention.Help += " Once set it cannot be cleared here: the emulator sets an emptied retention to 31 days."
	}
	encryption := field("encryption", encryptionGoogle)
	if t.GetKmsKeyName() != "" {
		encryption.Default = encryptionKMS
	}
	encryption.Immutable = true
	encryption.Help = "Not editable on this emulator: its UpdateTopic refuses kms_key_name as \"not a known Topic field\"."
	key := field("kmsKeyName", t.GetKmsKeyName())
	key.Immutable, key.Required = true, false
	return &console.EditForm{Label: "Edit", Title: "Edit topic", Submit: "Update", Layout: console.EditLayoutPage,
		Fields: []console.Field{
			{Name: "name", Label: "Topic ID", Type: "text", Default: lastSegment(t.GetName()), Immutable: true,
				Help: "A topic's name cannot be changed."},
			field("useSchema", strconv.FormatBool(schema != "")),
			field("schema", schema),
			field("schemaEncoding", enc),
			retention,
			field("retentionDays", days), field("retentionHours", hours), field("retentionMinutes", minutes),
			encryption, key,
			labelsField(t.GetLabels(), "Labels"),
		},
		Note: "Saved through UpdateTopic, with an update mask naming each field that changed. The message storage " +
			"policy and the Cloud KMS key cannot be changed on this emulator."}
}

// topicEditValues turns the Edit topic page's fields into the values Edit
// reads: no schema while Use a schema is cleared, and the retention from
// days, hours and minutes while Enable message retention is checked.
func topicEditValues(values map[string]string) (map[string]string, error) {
	out, err := topicPageValues(values)
	if err != nil {
		return nil, err
	}
	if values["useSchema"] != "true" {
		out["schema"], out["schemaEncoding"] = "", ""
	}
	for _, k := range []string{"useSchema", "enableRetention", "retentionDays", "retentionHours", "retentionMinutes", "encryption", "kmsKeyName"} {
		delete(out, k)
	}
	return out, nil
}
