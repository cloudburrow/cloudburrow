package main

// Pub/Sub pages as Google's console draws them (#1847): the list columns,
// Create topic and Create subscription as Google's pages with its fields,
// order and defaults, the topic page's Subscriptions, Snapshots, Metrics,
// Details, Messages and Ingestion tabs, and the subscription page's Metrics,
// Details, Messages and Snapshots tabs, with Pull, then Ack on each message,
// and Replay messages.
//
// Every call is still the official client's against the emulator (or
// CloudBurrow's front before it), as before: what changed is where each
// control is and what it is called. The actions the pages offered before
// (Pull and ack, Pull without ack, Seek to snapshot, Seek to time, the
// topic page's Create subscription form) are still served by the action
// route, so scripts and older pages keep working; the pages draw Google's.
//
// Sources, Google's public docs only:
// https://docs.cloud.google.com/pubsub/docs/create-topic,
// https://docs.cloud.google.com/pubsub/docs/create-subscription,
// https://docs.cloud.google.com/pubsub/docs/quickstart-console,
// https://docs.cloud.google.com/pubsub/docs/replay-message.

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	vkit "cloud.google.com/go/pubsub/v2/apiv1"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

const (
	// actPull is the Messages tab's Pull: messages are shown and left
	// outstanding, each with an Ack, as Google's tab does.
	actPull = "pull"
	// actAckMessage acknowledges one pulled message by its ack ID.
	actAckMessage = "ack"
	// actReplayMessages is Google's Replay messages: a Seek to a previous
	// time point or to a snapshot.
	actReplayMessages = "replay-messages"

	replayToTime     = "To a previous time point"
	replayToSnapshot = "To a snapshot"

	deliveryPull = "Pull"
	deliveryPush = "Push"

	retryImmediately = "Retry immediately"
	retryBackoff     = "Retry after exponential backoff delay"

	expireAfterDays = "Expire after this many days of inactivity"
	expireNever     = "Never expire"

	encodingJSON   = "JSON"
	encodingBinary = "Binary"
)

// pubsubNotLocal is what a Metrics tab says: Google's charts read Cloud
// Monitoring, which CloudBurrow does not emulate.
const pubsubNotLocal = "Not available locally: Pub/Sub's metrics are Cloud Monitoring time series, which " +
	"CloudBurrow does not emulate. Each call to this instance's Pub/Sub is in Logs Explorer and on the " +
	"console's Requests page."

// pubsubIngestionNote is the topic Ingestion tab's note.
const pubsubIngestionNote = "Not available locally: an import topic ingests from Amazon Kinesis Data Streams, " +
	"Cloud Storage, Azure Event Hubs, Amazon MSK or Confluent Cloud, and CloudBurrow runs no ingestion. " +
	"Create topic does not offer Enable ingestion for that reason; publish to the topic instead."

// pubsubPullHelp is the Messages tab's note on a subscription page.
const pubsubPullHelp = "Click Pull to view messages. A pulled message is not acknowledged until you click its Ack: " +
	"the others are delivered again once their acknowledgement deadline passes. Pull is for looking; at most " +
	"10 messages are shown at a time."

// --- lists ------------------------------------------------------------------

// topicListColumns are the Topics list's columns, after the topic name.
var topicListColumns = []string{"Topic ID", "Encryption key", "Retention", "Schema", "Labels"}

// topicRow is one topic on the Topics list, with its row menu.
func topicRow(project string, t *pubsubpb.Topic) console.Resource {
	retention := "—"
	if d := t.GetMessageRetentionDuration(); d != nil {
		retention = formatPubSubDuration(d)
	}
	schema := "—"
	if s := t.GetSchemaSettings().GetSchema(); s != "" {
		schema = lastSegment(s)
	}
	return console.Resource{
		Name: t.GetName(),
		Fields: map[string]string{
			"Topic ID":       lastSegment(t.GetName()),
			"Encryption key": topicEncryption(t),
			"Retention":      retention,
			"Schema":         schema,
			"Labels":         orDash(formatAttributes(t.GetLabels())),
		},
		Actions: []console.Action{createSubscriptionLink(project, t.GetName())},
	}
}

// topicEncryption names the key a topic's messages are encrypted with.
func topicEncryption(t *pubsubpb.Topic) string {
	if k := t.GetKmsKeyName(); k != "" {
		return k
	}
	return "Google-managed"
}

// createSubscriptionLink is a topic's Create subscription: Google's
// Create subscription page with the topic filled in.
func createSubscriptionLink(project, topic string) console.Action {
	q := url.Values{"topic": {topic}}
	if project != "" {
		q.Set("project", project)
	}
	return console.Action{ID: "create-subscription-page", Label: "Create subscription",
		Href: "/pubsub/subscriptions/create?" + q.Encode()}
}

// subscriptionListColumns are the Subscriptions list's columns.
var subscriptionListColumns = []string{"Subscription ID", "Delivery type", "Topic name", "Ack deadline",
	"Retention", "Message ordering", "Exactly once delivery", "Expiration", "Dead lettering"}

func subscriptionRow(s *pubsubpb.Subscription) console.Resource {
	dl := "Disabled"
	if s.GetDeadLetterPolicy().GetDeadLetterTopic() != "" {
		dl = "Enabled"
	}
	return console.Resource{Name: s.GetName(), Fields: map[string]string{
		"Subscription ID":       lastSegment(s.GetName()),
		"Delivery type":         deliveryType(s),
		"Topic name":            subscriptionTopic(s),
		"Ack deadline":          fmt.Sprintf("%d seconds", s.GetAckDeadlineSeconds()),
		"Retention":             durationWords(s.GetMessageRetentionDuration()),
		"Message ordering":      enabledDisabled(s.GetEnableMessageOrdering()),
		"Exactly once delivery": enabledDisabled(s.GetEnableExactlyOnceDelivery()),
		"Expiration":            expirationWords(s.GetExpirationPolicy()),
		"Dead lettering":        dl,
	}}
}

func enabledDisabled(v bool) string {
	if v {
		return "Enabled"
	}
	return "Disabled"
}

// durationWords is a duration as Google's lists show one: "7 days".
func durationWords(d interface{ AsDuration() time.Duration }) string {
	if d == nil {
		return "—"
	}
	v := d.AsDuration()
	switch {
	case v == 0:
		return "—"
	case v%(24*time.Hour) == 0:
		return plural(int64(v/(24*time.Hour)), "day")
	case v%time.Hour == 0:
		return plural(int64(v/time.Hour), "hour")
	case v%time.Minute == 0:
		return plural(int64(v/time.Minute), "minute")
	}
	return v.String()
}

func plural(n int64, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return strconv.FormatInt(n, 10) + " " + unit + "s"
}

// expirationWords is a subscription's expiration period: never, or how long
// it may be inactive. A subscription with no policy is given Google's 31
// days by the front.
func expirationWords(p *pubsubpb.ExpirationPolicy) string {
	switch {
	case p == nil:
		return "31 days"
	case p.GetTtl() == nil:
		return "Never"
	}
	return durationWords(p.GetTtl())
}

// --- Create topic -------------------------------------------------------

// topicCreatePageFields are Google's Create topic, in its order: Topic ID,
// Add a default subscription, Use a schema, Enable message retention. Enable
// ingestion, the exports and Cloud KMS key are not offered: CloudBurrow runs
// no ingestion, no export subscriptions write anywhere, and the emulator
// encrypts nothing with a key (docs/console-parity.md, Pub/Sub).
func topicCreatePageFields() []console.Field {
	return []console.Field{
		{Name: "name", Label: "Topic ID", Type: "text", Required: true,
			Help:    "3-255 characters, starting with a letter.",
			Pattern: `^[A-Za-z][A-Za-z0-9._~%+\-]{2,254}$`},
		{Name: "defaultSubscription", Label: "Add a default subscription", Type: "checkbox", Default: "true",
			Help: "Creates a pull subscription named after the topic, TOPIC-sub, with the default settings."},
		{Name: "useSchema", Label: "Use a schema", Type: "checkbox", Default: "false",
			Help: "A message published to the topic that does not conform to the schema is refused."},
		{Name: "schema", Label: "Select a Pub/Sub schema", Type: "text", ShowWhen: "useSchema", Required: true,
			Help: "A schema ID of this project, or projects/{project}/schemas/{schema}, that exists."},
		{Name: "schemaEncoding", Label: "Message encoding", Type: "radio", ShowWhen: "useSchema",
			Options: []string{encodingJSON, encodingBinary}, Default: encodingJSON},
		{Name: "enableRetention", Label: "Enable message retention", Type: "checkbox", Default: "false",
			Help: "The topic keeps every message published to it, acknowledged or not, so a subscription can " +
				"seek back to it: from 10 minutes to 31 days."},
		{Name: "retentionDays", Label: "Days", Type: "number", Default: "7", ShowWhen: "enableRetention"},
		{Name: "retentionHours", Label: "Hours", Type: "number", Default: "0", ShowWhen: "enableRetention"},
		{Name: "retentionMinutes", Label: "Minutes", Type: "number", Default: "0", ShowWhen: "enableRetention"},
		pubsubLabelsField("", "", pubsubCreateLabelsHelp),
	}
}

// topicPageValues turns Create topic's page fields into the form's values
// topicFromForm reads: a schema only while Use a schema is checked, and a
// retention from days, hours and minutes only while Enable message
// retention is.
func topicPageValues(values map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(values))
	for k, v := range values {
		out[k] = v
	}
	if v, ok := values["useSchema"]; ok && v != "true" {
		delete(out, "schema")
		delete(out, "schemaEncoding")
	}
	switch out["schemaEncoding"] {
	case encodingBinary:
		out["schemaEncoding"] = "BINARY"
	case encodingJSON:
		out["schemaEncoding"] = "JSON"
	}
	if v, ok := values["enableRetention"]; ok {
		if v != "true" {
			return out, nil
		}
		var total time.Duration
		for _, part := range []struct {
			key, label string
			unit       time.Duration
		}{{"retentionDays", "Days", 24 * time.Hour}, {"retentionHours", "Hours", time.Hour}, {"retentionMinutes", "Minutes", time.Minute}} {
			raw := strings.TrimSpace(values[part.key])
			if raw == "" {
				continue
			}
			n, err := strconv.ParseUint(raw, 10, 16)
			if err != nil {
				return nil, fmt.Errorf("message retention: %s %q is not a whole number", part.label, raw)
			}
			total += time.Duration(n) * part.unit
		}
		if total == 0 {
			return nil, errors.New("message retention: give a period from 10 minutes to 31 days, or clear Enable message retention")
		}
		out["messageRetention"] = total.String()
	}
	return out, nil
}

// --- Create subscription ------------------------------------------------

// subscriptionPageFields are Google's Create subscription, in its order:
// Subscription ID, Pub/Sub topic, Delivery type, Filter, Retry policy,
// Dead lettering, Delivery properties, Acknowledgement deadline, Lifetime
// options, Expiration period, then the labels. withTopic is false on a
// topic's own page, which names it.
func subscriptionPageFields(withTopic bool) []console.Field {
	const (
		filterSec    = "Filter"
		retrySec     = "Retry policy"
		deadSec      = "Dead lettering"
		propsSec     = "Delivery properties"
		ackSec       = "Acknowledgement deadline"
		lifetimeSec  = "Lifetime options"
		expirySec    = "Expiration period"
		labelSection = "Labels"
	)
	fields := []console.Field{
		{Name: "name", Label: "Subscription ID", Type: "text", Required: true,
			Help:    "3-255 characters, starting with a letter.",
			Pattern: `^[A-Za-z][A-Za-z0-9._~%+\-]{2,254}$`},
	}
	if withTopic {
		fields = append(fields, console.Field{Name: "topic", Label: "Pub/Sub topic", Type: "text", Required: true,
			Help: "The topic to subscribe to: its ID in this project, or projects/{project}/topics/{topic}."})
	}
	return append(fields,
		console.Field{Name: "deliveryType", Label: "Delivery type", Type: "radio",
			Options: []string{deliveryPull, deliveryPush}, Default: deliveryPull,
			Help: "Pull: subscribers ask for messages. Push: Pub/Sub sends each message to an endpoint as an HTTP POST. " +
				"Write to BigQuery and Write to Cloud Storage are not offered: this instance's Pub/Sub writes to neither."},
		console.Field{Name: "pushEndpoint", Label: "Endpoint URL", Type: "url", ShowWhen: "deliveryType=" + deliveryPush,
			Required: true, Help: "The URL each message is POSTed to."},
		console.Field{Name: "pushAttributes", Label: "Push attributes", Type: "map", ShowWhen: "deliveryType=" + deliveryPush,
			Help: "Optional. One key=value per line, such as x-goog-version=v1."},
		console.Field{Name: "filter", Label: "Subscription filter", Type: "text", Section: filterSec,
			Help: `Optional. Only messages whose attributes match are delivered, such as attributes.env = "prod"; ` +
				"the others are acknowledged for you. It cannot be changed once the subscription exists."},
		console.Field{Name: "retryPolicy", Label: "Retry policy", Type: "radio", Section: retrySec,
			Options: []string{retryImmediately, retryBackoff}, Default: retryImmediately},
		console.Field{Name: "minBackoff", Label: "Minimum backoff (seconds)", Type: "number", Default: "10", Section: retrySec,
			ShowWhen: "retryPolicy=" + retryBackoff, Help: "0 to 600."},
		console.Field{Name: "maxBackoff", Label: "Maximum backoff (seconds)", Type: "number", Default: "600", Section: retrySec,
			ShowWhen: "retryPolicy=" + retryBackoff, Help: "0 to 600, and no less than the minimum."},
		console.Field{Name: "deadLettering", Label: "Enable dead lettering", Type: "checkbox", Default: "false", Section: deadSec,
			Help: "A message that is not acknowledged after the maximum delivery attempts is published to the dead-letter topic."},
		console.Field{Name: "deadLetterTopic", Label: "Dead letter topic", Type: "text", Section: deadSec, ShowWhen: "deadLettering",
			Required: true, Help: "A topic that exists: its ID in this project, or projects/{project}/topics/{topic}."},
		console.Field{Name: "maxDeliveryAttempts", Label: "Maximum delivery attempts", Type: "number", Default: "5",
			Section: deadSec, ShowWhen: "deadLettering", Help: "5 to 100."},
		console.Field{Name: "exactlyOnce", Label: "Enable exactly once delivery", Type: "checkbox", Default: "false",
			Section: propsSec, Help: pubsubExactlyOnceHelp},
		console.Field{Name: "messageOrdering", Label: "Order messages with an ordering key", Type: "checkbox", Default: "false",
			Section: propsSec, Help: "Messages with the same ordering key are delivered in the order they were published. " +
				"It cannot be changed once the subscription exists."},
		console.Field{Name: "ackDeadline", Label: "Acknowledgement deadline (seconds)", Type: "number", Default: "10",
			Section: ackSec, Help: "10 to 600: how long a subscriber has to acknowledge a message before it is delivered again."},
		console.Field{Name: "messageRetention", Label: "Message retention duration", Type: "text", Default: "7d",
			Section: lifetimeSec, Help: "How long an unacknowledged message is kept, 10m to 31d. " + pubsubDurationHelp},
		console.Field{Name: "retainAcked", Label: "Retain acknowledged messages", Type: "checkbox", Default: "false",
			Section: lifetimeSec, Help: "Keeps acknowledged messages for the retention duration too, so Replay messages can replay them."},
		console.Field{Name: "expirationMode", Label: "Expiration period", Type: "radio", Section: expirySec,
			Options: []string{expireAfterDays, expireNever}, Default: expireAfterDays,
			Help: "Any call naming the subscription is activity, an open streaming pull keeps it active, and so " +
				"does a successful push."},
		console.Field{Name: "expirationDays", Label: "Days", Type: "number", Default: "31", Section: expirySec,
			ShowWhen: "expirationMode=" + expireAfterDays,
			Help: "At least 1, and no shorter than the message retention duration."},
		pubsubLabelsField("", labelSection, pubsubCreateLabelsHelp),
	)
}

// subscriptionPageValues turns Create subscription's page fields into the
// values subscriptionFromForm reads. A field its choice hides is dropped,
// so a push endpoint typed and then switched back to Pull is not sent. A
// form without the page's fields (the older action form) passes unchanged.
func subscriptionPageValues(project string, values map[string]string) map[string]string {
	out := make(map[string]string, len(values))
	for k, v := range values {
		out[k] = v
	}
	if v, ok := values["deliveryType"]; ok && v != deliveryPush {
		delete(out, "pushEndpoint")
		delete(out, "pushAttributes")
	}
	if v, ok := values["retryPolicy"]; ok {
		if v != retryBackoff {
			delete(out, "minBackoff")
			delete(out, "maxBackoff")
		} else {
			for _, k := range []string{"minBackoff", "maxBackoff"} {
				if n := strings.TrimSpace(out[k]); n != "" && strings.Trim(n, "0123456789") == "" {
					out[k] = n + "s"
				}
			}
		}
	}
	if v, ok := values["deadLettering"]; ok && v != "true" {
		delete(out, "deadLetterTopic")
	}
	if dl := strings.TrimSpace(out["deadLetterTopic"]); dl != "" && !strings.Contains(dl, "/") {
		out["deadLetterTopic"] = "projects/" + project + "/topics/" + dl
	}
	if v, ok := values["expirationMode"]; ok {
		if v == expireNever {
			out["expiration"] = "never"
		} else if d := strings.TrimSpace(values["expirationDays"]); d != "" {
			out["expiration"] = d + "d"
		}
	}
	return out
}

// subscriptionTopicName is the Pub/Sub topic field as a topic name.
func subscriptionTopicName(project, raw string) (string, error) {
	t := strings.TrimSpace(raw)
	switch {
	case t == "":
		return "", errors.New("Pub/Sub topic is required")
	case !strings.Contains(t, "/"):
		return "projects/" + project + "/topics/" + t, nil
	case strings.HasPrefix(t, "projects/") && strings.Contains(t, "/topics/"):
		return t, nil
	}
	return "", fmt.Errorf("Pub/Sub topic: %q is neither a topic ID nor projects/{project}/topics/{topic}", t)
}

// CreateForm implements console.Creator for the Subscriptions page:
// Google's Create subscription.
func (pubsubSubscriptionsProvider) CreateForm() (string, []console.Field) {
	return "Create subscription", subscriptionPageFields(true)
}

func (pubsubSubscriptionsProvider) CreateOnPage() bool  { return true }
func (pubsubSubscriptionsProvider) CreateTitle() string { return "Create subscription" }

// Create makes the subscription through CreateSubscription, as the topic
// page's form does.
func (p pubsubSubscriptionsProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	if project == "" {
		return "", errors.New("choose a project before creating a subscription")
	}
	topic, err := subscriptionTopicName(project, values["topic"])
	if err != nil {
		return "", err
	}
	sub, err := subscriptionFromForm(project, topic, subscriptionPageValues(project, values))
	if err != nil {
		return "", err
	}
	c, err := p.topics().client(ctx, project)
	if err != nil {
		return "", err
	}
	defer func() { _ = c.Close() }()
	if err := createSubscription(ctx, c.SubscriptionAdminClient, sub); err != nil {
		return "", err
	}
	return sub.GetName(), nil
}

// --- Create snapshot on the Snapshots page -----------------------------

// CreateForm implements console.Creator for the Snapshots page: Google's
// Create snapshot, an ID and the subscription it is taken of.
func (pubsubSnapshotsProvider) CreateForm() (string, []console.Field) {
	return "Create snapshot", []console.Field{
		{Name: "subscription", Label: "Pub/Sub subscription", Type: "text", Required: true,
			Help: "The subscription to take the snapshot of: its ID in this project, or projects/{project}/subscriptions/{subscription}."},
		{Name: "name", Label: "Snapshot ID", Type: "text", Required: true, Pattern: snapshotIDPattern,
			Help: "3-255 characters, starting with a letter. The snapshot keeps the subscription's unacknowledged " +
				"messages, and every message published to its topic after it. " + pubsubSnapshotLabelsDropped},
	}
}

func (pubsubSnapshotsProvider) CreateTitle() string { return "Create snapshot" }

// Create takes the snapshot through CreateSnapshot.
func (p pubsubSnapshotsProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	if project == "" {
		return "", errors.New("choose a project before creating a snapshot")
	}
	sub := strings.TrimSpace(values["subscription"])
	if !strings.Contains(sub, "/") && sub != "" {
		sub = "projects/" + project + "/subscriptions/" + sub
	}
	if !strings.HasPrefix(sub, "projects/"+project+"/subscriptions/") {
		return "", fmt.Errorf("Pub/Sub subscription: %q is not a subscription of project %s", values["subscription"], project)
	}
	id := strings.TrimSpace(values["name"])
	if id == "" {
		return "", errors.New("snapshot ID is required")
	}
	c, err := p.topics().client(ctx, project)
	if err != nil {
		return "", err
	}
	defer func() { _ = c.Close() }()
	s, err := c.SubscriptionAdminClient.CreateSnapshot(ctx, &pubsubpb.CreateSnapshotRequest{
		Name: "projects/" + project + "/snapshots/" + id, Subscription: sub,
	})
	if err != nil {
		return "", err
	}
	return s.GetName(), nil
}

// --- Messages: Pull, then Ack ------------------------------------------

// ackAction is one pulled message's Ack, carrying its ack ID (and, on a
// topic page, the subscription it was pulled from).
func ackAction(ackID, subscription string) console.Action {
	fields := []console.Field{{Name: "ackId", Type: "hidden", Default: ackID}}
	if subscription != "" {
		fields = append(fields, console.Field{Name: "subscription", Type: "hidden", Default: subscription})
	}
	return console.Action{ID: actAckMessage, Label: "Ack", Fields: fields}
}

// pullToView pulls up to max messages from a pull subscription and leaves
// them outstanding: each row has an Ack. onTopic names the subscription in
// each Ack, which the topic page's path does not.
func pullToView(ctx context.Context, sc *vkit.SubscriptionAdminClient, sub *pubsubpb.Subscription, max int, onTopic bool) (*console.Listing, error) {
	name := sub.GetName()
	if sub.GetPushConfig().GetPushEndpoint() != "" {
		return nil, fmt.Errorf("%s is a push subscription; its messages are delivered to %s, not pulled",
			lastSegment(name), sub.GetPushConfig().GetPushEndpoint())
	}
	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	resp, err := sc.Pull(pctx, &pubsubpb.PullRequest{Subscription: name, MaxMessages: int32(max), ReturnImmediately: true})
	if err != nil && !isDeadline(err) {
		return nil, err
	}
	out := &console.Listing{
		Columns:    []string{"Publish time", "Message body", "Attributes", "Ordering key", "Delivery attempt"},
		NameColumn: "Message ID", Noun: "messages",
	}
	ackSub := ""
	if onTopic {
		ackSub = lastSegment(name)
	}
	for _, rm := range resp.GetReceivedMessages() {
		m := rm.GetMessage()
		attempt := "—"
		if n := rm.GetDeliveryAttempt(); n > 0 {
			attempt = strconv.Itoa(int(n))
		}
		out.Items = append(out.Items, console.Resource{Name: m.GetMessageId(), Fields: map[string]string{
			"Publish time":     m.GetPublishTime().AsTime().UTC().Format(time.RFC3339),
			"Message body":     string(m.GetData()),
			"Attributes":       formatAttributes(m.GetAttributes()),
			"Ordering key":     m.GetOrderingKey(),
			"Delivery attempt": attempt,
		}, Actions: []console.Action{ackAction(rm.GetAckId(), ackSub)}})
	}
	out.Total = len(out.Items)
	if out.Total == 0 {
		out.Note = "No messages were waiting on " + lastSegment(name) + ". A low-volume subscription can answer " +
			"none: click Pull again."
	} else {
		out.Note = fmt.Sprintf("%d not acknowledged yet: Ack each one you are done with; the others are delivered "+
			"again after the acknowledgement deadline, %d seconds.", out.Total, sub.GetAckDeadlineSeconds())
	}
	return out, nil
}

// ackMessage acknowledges one message by its ack ID.
func ackMessage(ctx context.Context, sc *vkit.SubscriptionAdminClient, sub, ackID string) error {
	if strings.TrimSpace(ackID) == "" {
		return errors.New("the message has no ack ID: pull it again")
	}
	return sc.Acknowledge(ctx, &pubsubpb.AcknowledgeRequest{Subscription: sub, AckIds: []string{ackID}})
}

// --- the subscription page ----------------------------------------------

// subscriptionTabActions are the actions drawn inside the subscription
// page's tabs; the page header draws the rest.
var subscriptionTabActions = map[string]bool{actPull: true, actAckMessage: true, actSeekSnapshot: true, actSeekTime: true}

// replayAction is Google's Replay messages: one dialog, Seek to a previous
// time point or to a snapshot.
func replayAction(retainsAcked bool, snapshots []string) console.Action {
	retained := "This subscription does not retain acknowledged messages, so seeking back replays only the " +
		"unacknowledged ones: check Retain acknowledged messages with Edit to replay acknowledged ones too."
	if retainsAcked {
		retained = "This subscription retains acknowledged messages, so seeking back replays them too."
	}
	options := []string{replayToTime}
	snapHelp := "This topic has no snapshots: create one with Create snapshot."
	def := ""
	if len(snapshots) > 0 {
		options = append(options, replayToSnapshot)
		snapHelp = "One of this topic's snapshots: " + strings.Join(snapshots, ", ") + "."
		def = snapshots[0]
	}
	fields := []console.Field{
		{Name: "seek", Label: "Seek", Type: "radio", Options: options, Default: replayToTime},
		{Name: "time", Label: "Time point (RFC 3339)", Type: "text", Required: true, ShowWhen: "seek=" + replayToTime,
			Default: time.Now().UTC().Truncate(time.Second).Format(time.RFC3339),
			Help: "Such as 2026-01-02T15:04:05Z. Messages published before it are marked acknowledged, and those " +
				"after it that the subscription still retains are delivered again. " + retained},
	}
	if len(snapshots) > 0 {
		fields = append(fields, console.Field{Name: "snapshot", Label: "Snapshot", Type: "select", Options: snapshots,
			Default: def, Required: true, ShowWhen: "seek=" + replayToSnapshot, Help: snapHelp})
	}
	return console.Action{ID: actReplayMessages, Label: "Replay messages",
		Confirm: replayConfirm, Fields: fields}
}

// replayConfirm is what Replay messages puts at risk.
const replayConfirm = "Seek changes which messages this subscription delivers: to a time point, messages published " +
	"before it are marked acknowledged and those after it that are retained are redelivered; to a snapshot, " +
	"what the snapshot held unacknowledged, and everything published after it, is redelivered."

// replayAsSeek turns Replay messages' values into the Seek action they ask
// for.
func replayAsSeek(values map[string]string) (string, map[string]string, error) {
	switch values["seek"] {
	case replayToSnapshot:
		return actSeekSnapshot, map[string]string{"snapshot": values["snapshot"]}, nil
	case replayToTime, "":
		return actSeekTime, map[string]string{"time": values["time"]}, nil
	}
	return "", nil, fmt.Errorf("seek: %q is neither %q nor %q", values["seek"], replayToTime, replayToSnapshot)
}

// subscriptionTabs are the subscription page's tabs in Google's order:
// Metrics, Details, Messages, Snapshots, then Permissions.
func subscriptionTabs(s *pubsubpb.Subscription, activity subscriptionActivity, snapshots console.Section, pull []console.Action) []console.Section {
	details := console.Section{ID: "details", Label: "Details", Kind: console.KindProperties}
	details.Groups = append(details.Groups, console.PropertyGroup{Heading: "Overview", Properties: []console.Property{
		{Label: "Subscription name", Value: s.GetName()},
		{Label: "Topic name", Value: subscriptionTopic(s)},
		{Label: "Delivery type", Value: deliveryType(s)},
		{Label: "Acknowledgement deadline", Value: plural(int64(s.GetAckDeadlineSeconds()), "second")},
	}})
	for _, sec := range []console.Section{subscriptionDeliverySection(s), subscriptionDeadLetterSection(s), subscriptionConfigSection(s, activity)} {
		details.Groups = append(details.Groups, sec.Groups...)
		if sec.Note != "" {
			details.Note = strings.TrimSpace(details.Note + " " + sec.Note)
		}
	}

	messages := console.Section{ID: "messages", Label: "Messages", Kind: console.KindProperties, Actions: pull,
		Note: pubsubPullHelp, Groups: []console.PropertyGroup{{Heading: "Pull", Properties: []console.Property{
			{Label: "Delivery type", Value: deliveryType(s)},
			{Label: "Acknowledgement deadline", Value: plural(int64(s.GetAckDeadlineSeconds()), "second")},
		}}}}
	if len(pull) == 0 {
		messages.Note = "Messages are pulled from a pull subscription. This one's are delivered by " + deliveryType(s) +
			", so there is nothing to pull here."
	}
	return []console.Section{
		metricsSection(),
		details,
		messages,
		snapshots,
		{ID: "permissions", Kind: console.KindPermissions},
	}
}

// metricsSection is a Metrics tab: not available locally.
func metricsSection() console.Section {
	return console.Section{ID: "metrics", Label: "Metrics", Kind: console.KindProperties, Note: pubsubNotLocal,
		Groups: []console.PropertyGroup{{Heading: "Metrics", Properties: []console.Property{
			{Label: "Cloud Monitoring", Value: "Not available locally"},
		}}}}
}

// subscriptionSnapshotsSection is the subscription page's Snapshots tab:
// its topic's snapshots, which it can seek to. Create snapshot is beside
// the page's title, as Google's is.
func subscriptionSnapshotsSection(ctx context.Context, tc *vkit.TopicAdminClient, project, topic string) console.Section {
	if topic == deletedTopic {
		return console.Section{ID: "snapshots", Label: "Snapshots", Listing: console.Listing{
			Columns: []string{}, NameColumn: "Snapshot", Noun: "snapshots",
			Note: "This subscription's topic was deleted, so it has no snapshots to seek to.",
		}}
	}
	sec := topicSnapshotsSection(ctx, tc, project, topic)
	if sec.Listing.Total == 0 && sec.Listing.Unavailable == "" {
		sec.Listing.Note = "This subscription's topic has no snapshots. Create snapshot keeps this subscription's " +
			"unacknowledged messages, so Replay messages can return to them."
	}
	return sec
}

// headerActions are the actions not drawn inside a tab.
func headerActions(all []console.Action, inTab map[string]bool) []console.Action {
	out := []console.Action{}
	for _, a := range all {
		if !inTab[a.ID] {
			out = append(out, a)
		}
	}
	return out
}

func actionsWith(all []console.Action, ids ...string) []console.Action {
	var out []console.Action
	for _, id := range ids {
		for _, a := range all {
			if a.ID == id {
				out = append(out, a)
			}
		}
	}
	return out
}

// --- the topic page -------------------------------------------------------

// topicTabActions are the actions drawn inside the topic page's tabs.
var topicTabActions = map[string]bool{actPublish: true, actPull: true, actPullAck: true, actPullNoAck: true,
	actAckMessage: true, actCreateSubscription: true}

// topicDetailsSection is the topic page's Details tab.
func topicDetailsSection(t *pubsubpb.Topic, subscriptions int) console.Section {
	props := []console.Property{
		{Label: "Topic name", Value: t.GetName()},
		{Label: "Topic ID", Value: lastSegment(t.GetName())},
		{Label: "Encryption key", Value: topicEncryption(t)},
		{Label: "Message retention duration", Value: orDash(durationWords(t.GetMessageRetentionDuration()))},
		{Label: "Subscriptions", Value: strconv.Itoa(subscriptions)},
	}
	if s := t.GetSchemaSettings(); s != nil && s.GetSchema() != "" {
		schema := s.GetSchema()
		if schema == deletedSchema {
			schema += " (the schema was deleted)"
		}
		props = append(props, console.Property{Label: "Schema name", Value: schema})
		if e := s.GetEncoding(); e != pubsubpb.Encoding_ENCODING_UNSPECIFIED {
			props = append(props, console.Property{Label: "Message encoding", Value: e.String()})
		}
	}
	groups := []console.PropertyGroup{{Heading: "Topic", Properties: props}}
	if len(t.GetLabels()) > 0 {
		groups = append(groups, console.PropertyGroup{Heading: "Labels", Properties: sortedPairs(t.GetLabels())})
	}
	return console.Section{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: groups}
}

// topicMessagesSection is the topic page's Messages tab: Publish message,
// and Pull from one of its subscriptions.
func topicMessagesSection(actions []console.Action) console.Section {
	return console.Section{ID: "messages", Label: "Messages", Kind: console.KindProperties,
		Actions: actionsWith(actions, actPublish, actPull, actPullAck, actPullNoAck),
		Note: "Publish message sends a message to this topic. Pull shows messages from one of its pull " +
			"subscriptions, each with an Ack; Pull and ack acknowledges every message it shows, and Pull without " +
			"ack makes them available again at once.",
		Groups: []console.PropertyGroup{{Heading: "Messages", Properties: []console.Property{
			{Label: "Publish", Value: "Publish message, with a body, attributes and an ordering key"},
		}}}}
}

// topicIngestionSection is the topic page's Ingestion tab.
func topicIngestionSection(t *pubsubpb.Topic) console.Section {
	source := "None: a standard topic"
	if t != nil && t.GetIngestionDataSourceSettings() != nil {
		source = "Configured, and not run locally"
	}
	return console.Section{ID: "ingestion", Label: "Ingestion", Kind: console.KindProperties, Note: pubsubIngestionNote,
		Groups: []console.PropertyGroup{{Heading: "Ingestion", Properties: []console.Property{
			{Label: "Ingestion source", Value: source},
		}}}}
}

// topicPullAction is the topic Messages tab's Pull.
func topicPullAction(def string) console.Action {
	return console.Action{ID: actPull, Label: "Pull", Fields: []console.Field{{
		Name: "subscription", Label: "Pub/Sub subscription", Type: "text", Required: true, Default: def,
		Help: "A pull subscription of this topic. Messages are shown, each with an Ack, and not acknowledged until you click it.",
	}}}
}

// pullOnTopic is the topic Messages tab's Pull, from the subscription it
// names, which must be this topic's.
func pullOnTopic(ctx context.Context, sc *vkit.SubscriptionAdminClient, project, topic, id string) (*console.Listing, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, errors.New("Pub/Sub subscription is required")
	}
	sub, err := sc.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: "projects/" + project + "/subscriptions/" + id})
	if err != nil {
		return nil, err
	}
	if sub.GetTopic() != topic {
		return nil, fmt.Errorf("%s is not a subscription of %s", id, topic)
	}
	return pullToView(ctx, sc, sub, 10, true)
}

// ackOnTopic acknowledges a message pulled on the topic page.
func ackOnTopic(ctx context.Context, sc *vkit.SubscriptionAdminClient, project, topic string, values map[string]string) error {
	id := strings.TrimSpace(values["subscription"])
	sub, err := sc.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: "projects/" + project + "/subscriptions/" + id})
	if err != nil {
		return err
	}
	if sub.GetTopic() != topic {
		return fmt.Errorf("%s is not a subscription of %s", id, topic)
	}
	return ackMessage(ctx, sc, sub.GetName(), values["ackId"])
}

var (
	_ console.CreateTitler = pubsubSubscriptionsProvider{}
	_ console.PageCreator  = pubsubSubscriptionsProvider{}
	_ console.CreateTitler = pubsubSnapshotsProvider{}
)

// pullMessages is the subscription Messages tab's Pull.
func (p pubsubSubscriptionsProvider) pullMessages(ctx context.Context, project string, path []string) (*console.Listing, error) {
	if len(path) != 1 || !strings.HasPrefix(path[0], "projects/"+project+"/subscriptions/") {
		return nil, fmt.Errorf("Pull applies to a subscription of project %s", project)
	}
	c, err := p.topics().client(ctx, project)
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.Close() }()
	sub, err := c.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: path[0]})
	if err != nil {
		return nil, err
	}
	return pullToView(ctx, c.SubscriptionAdminClient, sub, 10, false)
}
