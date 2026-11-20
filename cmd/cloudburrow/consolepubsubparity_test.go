package main

import (
	"context"
	"strings"
	"testing"
	"time"

	pubsubpb "cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// pageDefaults is what the browser submits for a create page left as it
// opened: every field's default.
func pageDefaults(fields []console.Field) map[string]string {
	out := map[string]string{}
	for _, f := range fields {
		out[f.Name] = f.Default
	}
	return out
}

// Google's Create topic and Create subscription pages (#1847): their
// fields in Google's order, each revealed field sent only while its choice
// holds, and what they create read back through the official client.
func TestPubSubGooglePagesCreate(t *testing.T) {
	ctx := context.Background()
	const project = "pages"
	addr, c := pubsubEditFixture(t, project)
	topics := pubsubProvider{endpoint: addr}
	subs := pubsubSubscriptionsProvider{endpoint: addr}

	label, fields := topics.CreateForm()
	var names []string
	for _, f := range fields {
		names = append(names, f.Name)
	}
	if label != "Create topic" || topics.CreateTitle() != "Create topic" || !topics.CreateOnPage() {
		t.Errorf("Create topic is %q titled %q", label, topics.CreateTitle())
	}
	if got := strings.Join(names, " "); got != "name defaultSubscription useSchema schema schemaEncoding enableIngestion "+
		"enableRetention retentionDays retentionHours retentionMinutes exportBigQuery exportTable exportStorage exportBucket addTransform encryption kmsKeyName labels" {
		t.Errorf("Create topic's fields are %s; want Google's order", got)
	}
	if topics.CreateLayout() != console.CreateLayoutPage {
		t.Error("Create topic is not one page, as Google's is")
	}

	// Retention typed and then unchecked is not sent; checked, days, hours
	// and minutes are.
	v := pageDefaults(fields)
	v["name"], v["retentionDays"] = "plain", "3"
	plain, err := topics.Create(ctx, project, v)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := c.TopicAdminClient.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: plain}); got.GetMessageRetentionDuration() != nil {
		t.Errorf("an unchecked retention was set: %v", got.GetMessageRetentionDuration())
	}
	v["name"], v["enableRetention"], v["retentionDays"], v["retentionHours"] = "kept", "true", "1", "12"
	kept, err := topics.Create(ctx, project, v)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := c.TopicAdminClient.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: kept}); got.GetMessageRetentionDuration().AsDuration() != 36*time.Hour {
		t.Errorf("retention = %v, want 36h", got.GetMessageRetentionDuration())
	}

	// The Topics list: Google's columns, and a row menu whose Create
	// subscription opens the page with the topic.
	l, err := topics.List(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range l.Items {
		if r.Name == kept {
			if r.Fields["Topic ID"] != "kept" || r.Fields["Retention"] != "36h0m0s" || r.Fields["Encryption key"] != "Google-managed" {
				t.Errorf("row = %v", r.Fields)
			}
			if len(r.Actions) != 1 || !strings.HasPrefix(r.Actions[0].Href, "/pubsub/subscriptions/create?") ||
				!strings.Contains(r.Actions[0].Href, "topic=projects%2Fpages%2Ftopics%2Fkept") {
				t.Errorf("row menu = %+v", r.Actions)
			}
		}
	}

	// Create subscription from the Subscriptions page.
	_, sf := subs.CreateForm()
	names = names[:0]
	for _, f := range sf {
		names = append(names, f.Name)
	}
	if got := strings.Join(names, " "); !strings.HasPrefix(got, "name topic deliveryType bigqueryTable bigquerySchema bigqueryDropUnknown bigqueryWriteMetadata "+
		"bigqueryServiceAccount storageBucket filenamePrefix filenameSuffix filenameDatetimeFormat storageFormat storageUseTopicSchema storageWriteMetadata "+
		"maxDurationMinutes maxBytes maxMessages pushEndpoint pushAttributes filter retryPolicy") {
		t.Errorf("Create subscription's fields are %s; want Google's order", got)
	}
	sv := pageDefaults(sf)
	sv["name"], sv["topic"] = "orders", "kept"
	// Hidden by its choice: a push endpoint with Pull, backoffs with Retry
	// immediately, and a dead-letter topic with dead lettering unchecked.
	sv["pushEndpoint"], sv["deadLetterTopic"] = "https://example.invalid/", "plain"
	name, err := subs.Create(ctx, project, sv)
	if err != nil {
		t.Fatal(err)
	}
	s, err := c.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: name})
	if err != nil {
		t.Fatal(err)
	}
	if s.GetTopic() != kept || s.GetPushConfig().GetPushEndpoint() != "" || s.GetRetryPolicy() != nil ||
		s.GetDeadLetterPolicy() != nil || s.GetAckDeadlineSeconds() != 10 ||
		s.GetMessageRetentionDuration().AsDuration() != 7*24*time.Hour || s.GetExpirationPolicy().GetTtl().AsDuration() != 31*24*time.Hour {
		t.Errorf("the defaults made %+v", s)
	}

	sv["name"], sv["retryPolicy"], sv["minBackoff"], sv["maxBackoff"] = "orders-retry", retryBackoff, "5", "60"
	sv["deadLettering"], sv["maxDeliveryAttempts"], sv["expirationMode"] = "true", "7", expireNever
	name, err = subs.Create(ctx, project, sv)
	if err != nil {
		t.Fatal(err)
	}
	s, _ = c.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: name})
	if s.GetRetryPolicy().GetMinimumBackoff().AsDuration() != 5*time.Second || s.GetRetryPolicy().GetMaximumBackoff().AsDuration() != time.Minute ||
		s.GetDeadLetterPolicy().GetDeadLetterTopic() != plain || s.GetDeadLetterPolicy().GetMaxDeliveryAttempts() != 7 ||
		s.GetExpirationPolicy() == nil || s.GetExpirationPolicy().GetTtl() != nil {
		t.Errorf("the choices made %+v", s)
	}
	if _, err := subs.Create(ctx, project, map[string]string{"name": "x-no-topic"}); err == nil {
		t.Error("a subscription with no topic was created")
	}

	// The Subscriptions list's columns.
	sl, err := subs.List(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range sl.Items {
		if r.Name == name && (r.Fields["Expiration"] != "Never" || r.Fields["Dead lettering"] != "Enabled" || r.Fields["Ack deadline"] != "10 seconds") {
			t.Errorf("subscription row = %v", r.Fields)
		}
	}
}

// The subscription page's tabs, in Google's order, its Messages tab's Pull
// with an Ack on each message, and Replay messages (#1847).
func TestPubSubSubscriptionPagePullAckAndReplay(t *testing.T) {
	ctx := context.Background()
	const project = "pull"
	// The snapshot fake, whose time Seek pstest cannot replay safely (#1878).
	addr, _, c := newSnapshotFake(t, project)
	topics := pubsubProvider{endpoint: addr}
	subs := pubsubSubscriptionsProvider{endpoint: addr}
	topic, err := topics.Create(ctx, project, map[string]string{"name": "t"})
	if err != nil {
		t.Fatal(err)
	}
	// pstest's Pull dereferences a subscription's retention, so it is given
	// one: the page's default, 7 days.
	_, sf := subs.CreateForm()
	sv := pageDefaults(sf)
	sv["name"], sv["topic"] = "t-sub", "t"
	sub, err := subs.Create(ctx, project, sv)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := topics.ActAtResult(ctx, project, []string{topic}, actPublish, map[string]string{"data": "hello"}); err != nil {
		t.Fatal(err)
	}

	d, err := subs.Detail(ctx, project, []string{sub})
	if err != nil {
		t.Fatal(err)
	}
	var tabs []string
	for _, s := range d.Sections {
		tabs = append(tabs, s.ID)
	}
	if got := strings.Join(tabs, " "); got != "metrics details messages snapshots permissions" {
		t.Errorf("tabs = %s", got)
	}
	if acts := d.Sections[2].Actions; len(acts) != 1 || acts[0].ID != actPull {
		t.Errorf("the Messages tab's actions are %+v; want Pull", acts)
	}
	var header []string
	for _, a := range d.Actions {
		header = append(header, a.Label)
	}
	if got := strings.Join(header, ", "); !strings.Contains(got, "Replay messages") || strings.Contains(got, "Pull") ||
		strings.Contains(got, "Seek to") {
		t.Errorf("header actions = %s", got)
	}

	res, err := subs.ActAtResult(ctx, project, []string{sub}, actPull, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Items) != 1 || res.Items[0].Fields["Message body"] != "hello" || len(res.Items[0].Actions) != 1 {
		t.Fatalf("pulled %+v", res.Items)
	}
	ack := res.Items[0].Actions[0]
	if ack.ID != actAckMessage || ack.Fields[0].Name != "ackId" || ack.Fields[0].Default == "" {
		t.Fatalf("the Ack is %+v", ack)
	}
	if _, err := subs.ActAtResult(ctx, project, []string{sub}, actAckMessage, map[string]string{"ackId": ack.Fields[0].Default}); err != nil {
		t.Fatalf("ack: %v", err)
	}

	// The topic page: Google's tabs, Publish message and Pull in Messages,
	// and Create subscription beside the title as a link to its page.
	td, err := topics.Detail(ctx, project, []string{topic})
	if err != nil {
		t.Fatal(err)
	}
	tabs = tabs[:0]
	for _, s := range td.Sections {
		tabs = append(tabs, s.ID)
	}
	if got := strings.Join(tabs, " "); got != "subscriptions snapshots metrics details messages ingestion permissions" {
		t.Errorf("topic tabs = %s", got)
	}
	if len(td.Actions) != 1 || td.Actions[0].Label != "Create subscription" || td.Actions[0].Href == "" {
		t.Errorf("topic header = %+v", td.Actions)
	}
	if _, err := topics.ActAtResult(ctx, project, []string{topic}, actPublish, map[string]string{"data": "again"}); err != nil {
		t.Fatal(err)
	}
	tres, err := topics.ActAtResult(ctx, project, []string{topic}, actPull, map[string]string{"subscription": "t-sub"})
	if err != nil || len(tres.Items) == 0 {
		t.Fatalf("topic pull = %+v, %v", tres, err)
	}
	ta := tres.Items[0].Actions[0]
	vals := map[string]string{}
	for _, f := range ta.Fields {
		vals[f.Name] = f.Default
	}
	if vals["subscription"] != "t-sub" {
		t.Errorf("the topic page's Ack names %q", vals["subscription"])
	}
	if _, err := topics.ActAtResult(ctx, project, []string{topic}, actAckMessage, vals); err != nil {
		t.Errorf("topic ack: %v", err)
	}

	// Last: pstest's Pull fails on a subscription it has seeked.
	// Replay messages to a time point is a Seek.
	if _, err := subs.ActAtResult(ctx, project, []string{sub}, actReplayMessages, map[string]string{
		"seek": replayToTime, "time": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
	}); err != nil {
		t.Errorf("replay to a time point: %v", err)
	}
	if _, err := subs.ActAtResult(ctx, project, []string{sub}, actReplayMessages, map[string]string{"seek": "sideways"}); err == nil {
		t.Error("an unknown seek was accepted")
	}

	_ = c
}

// Create snapshot on the Snapshots page (#1847).
func TestPubSubSnapshotsPageCreate(t *testing.T) {
	ctx := context.Background()
	const project = "snaps"
	addr, _, c := newSnapshotFake(t, project)
	if _, err := (pubsubProvider{endpoint: addr}).Create(ctx, project, map[string]string{"name": "t", "defaultSubscription": "true"}); err != nil {
		t.Fatal(err)
	}
	p := pubsubSnapshotsProvider{endpoint: addr}
	name, err := p.Create(ctx, project, map[string]string{"subscription": "t-sub", "name": "before"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.SubscriptionAdminClient.GetSnapshot(ctx, &pubsubpb.GetSnapshotRequest{Snapshot: name}); err != nil {
		t.Errorf("the snapshot reads %v", err)
	}
	if _, err := p.Create(ctx, project, map[string]string{"subscription": "projects/other/subscriptions/x", "name": "n"}); err == nil {
		t.Error("a snapshot of another project's subscription was taken")
	}
}

// TestPubSubUnavailableChoicesAreRefused (#1847): Google's choices this
// instance cannot make (Enable ingestion, a transform) are drawn disabled
// with a reason, and a request that chooses one anyway is refused with that
// reason, creating nothing. The exports and Cloud KMS key work since #1874.
func TestPubSubUnavailableChoicesAreRefused(t *testing.T) {
	ctx := context.Background()
	const project = "unavailable"
	addr, c := pubsubEditFixture(t, project)
	topics := pubsubProvider{endpoint: addr}
	subs := pubsubSubscriptionsProvider{endpoint: addr}

	_, fields := topics.CreateForm()
	for name, value := range map[string]string{"enableIngestion": "true", "addTransform": "true"} {
		v := pageDefaults(fields)
		v["name"], v[name] = "refused-"+strings.ToLower(name), value
		if _, err := topics.Create(ctx, project, v); err == nil || !strings.Contains(err.Error(), "Not available locally") {
			t.Errorf("Create topic with %s=%s: %v, want the reason it is unavailable", name, value, err)
		}
		if _, err := c.TopicAdminClient.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: "projects/" + project + "/topics/" + v["name"]}); err == nil {
			t.Errorf("Create topic with %s=%s created the topic", name, value)
		}
	}
	_, sf := subs.CreateForm()
	for _, f := range sf {
		if f.Name == "deliveryType" && len(f.UnavailableOptions) != 0 {
			t.Errorf("Delivery type draws a choice unavailable: %+v", f)
		}
	}
	_ = c
}
