package main

import (
	"context"
	"strings"
	"testing"
	"time"

	pubsub "cloud.google.com/go/pubsub/v2"
	pubsubpb "cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"cloud.google.com/go/pubsub/v2/pstest"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// pubsubEditFixture is a pstest server, the official client of it and its
// address, for the console's providers over it.
func pubsubEditFixture(t *testing.T, project string, opts ...pstest.ServerReactorOption) (string, *pubsub.Client) {
	t.Helper()
	fake := pstest.NewServer(opts...)
	t.Cleanup(func() { _ = fake.Close() })
	c, err := pubsub.NewClient(context.Background(), project,
		option.WithEndpoint(fake.Addr),
		option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return fake.Addr, c
}

// pubsubSubmitted is what the browser submits for an edit form: every
// field's default except the immutable ones, which it does not send.
func pubsubSubmitted(form *console.EditForm) map[string]string {
	out := map[string]string{}
	for _, f := range form.Fields {
		if !f.Immutable {
			out[f.Name] = f.Default
		}
	}
	return out
}

func pubsubEditOf(t *testing.T, p console.Driller, project, name string) *console.EditForm {
	t.Helper()
	d, err := p.Detail(context.Background(), project, []string{name})
	if err != nil || d.Edit == nil {
		t.Fatalf("the page of %s offers no edit form: %v %+v", name, err, d)
	}
	return d.Edit
}

func fieldNamed(form *console.EditForm, name string) (console.Field, bool) {
	for _, f := range form.Fields {
		if f.Name == name {
			return f, true
		}
	}
	return console.Field{}, false
}

// A topic's page carries Edit topic, prefilled from the topic, with the
// retention the one field sent and labels named in the note with the
// emulator's refusal rather than offered. Saving a retention is what the
// official client's GetTopic reads; an emptied one, which the emulator would
// save as 31 days, is refused; and a refusal from UpdateTopic is returned as
// it came (#786).
func TestPubSubEditTopicThroughUpdateTopic(t *testing.T) {
	ctx := context.Background()
	const project = "edit-proj"
	addr, c := pubsubEditFixture(t, project)
	p := pubsubProvider{endpoint: addr}
	name := "projects/" + project + "/topics/orders"
	if _, err := c.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: name, Labels: map[string]string{"team": "a"}}); err != nil {
		t.Fatal(err)
	}

	form := pubsubEditOf(t, p, project, name)
	if form.Label != "Edit topic" || !strings.Contains(form.Note, pubsubTopicLabelsRefusal) {
		t.Errorf("edit form %q, note %q; want Edit topic with the emulator's labels refusal", form.Label, form.Note)
	}
	if _, ok := fieldNamed(form, "labels"); ok {
		t.Error("the topic form offers labels, which the emulator's UpdateTopic refuses")
	}
	if f, _ := fieldNamed(form, "name"); !f.Immutable || f.Default != "orders" {
		t.Errorf("name field = %+v; want orders, immutable", f)
	}
	values := pubsubSubmitted(form)
	if len(values) != 1 || values["messageRetention"] != "" {
		t.Fatalf("a topic with no retention submits %v; want only an empty retention", values)
	}
	if f, _ := fieldNamed(form, "messageRetention"); f.Required {
		t.Error("the retention of a topic that has none is required")
	}
	if err := p.Edit(ctx, project, []string{name}, values); err != nil {
		t.Fatalf("saving the form unchanged: %v", err)
	}
	if got, _ := c.TopicAdminClient.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: name}); got.GetMessageRetentionDuration() != nil {
		t.Errorf("saving unchanged gave the topic a retention: %v", got.GetMessageRetentionDuration())
	}

	values["messageRetention"] = "2d"
	if err := p.Edit(ctx, project, []string{name}, values); err != nil {
		t.Fatalf("Edit: %v", err)
	}
	got, err := c.TopicAdminClient.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: name})
	if err != nil || got.GetMessageRetentionDuration().AsDuration() != 48*time.Hour || got.GetLabels()["team"] != "a" {
		t.Fatalf("GetTopic after the edit = %v (%v); want 48h retention and the label kept", got, err)
	}
	d, _ := p.Detail(ctx, project, []string{name})
	props := map[string]string{}
	for _, pr := range d.Summary {
		props[pr.Label] = pr.Value
	}
	if props["Message retention"] != "2d" || props["Labels"] != "team=a" {
		t.Errorf("the topic's summary reads %v; want its retention and labels", props)
	}
	form = pubsubEditOf(t, p, project, name)
	if f, _ := fieldNamed(form, "messageRetention"); f.Default != "2d" || !f.Required {
		t.Errorf("after saving, retention is prefilled %+v; want 2d, required", f)
	}

	// Refused before UpdateTopic, changing nothing.
	for what, v := range map[string]map[string]string{
		"an emptied retention": {"messageRetention": ""},
		"a bad duration":       {"messageRetention": "soon"},
		"a rename":             {"messageRetention": "2d", "name": "other"},
	} {
		if err := p.Edit(ctx, project, []string{name}, v); err == nil {
			t.Errorf("an edit with %s was accepted", what)
		} else if what == "an emptied retention" && err.Error() != pubsubRetentionUnclearable {
			t.Errorf("an emptied retention was refused with %v", err)
		}
	}
	if err := p.Edit(ctx, "other-proj", []string{name}, values); err == nil {
		t.Error("an edit of another project's topic was accepted")
	}
	if err := p.Edit(ctx, project, []string{name, "projects/" + project + "/subscriptions/s"}, values); err == nil {
		t.Error("an edit of a topic page's subscription row was accepted")
	}
	if got, _ := c.TopicAdminClient.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: name}); got.GetMessageRetentionDuration().AsDuration() != 48*time.Hour {
		t.Errorf("after refused edits the retention is %v", got.GetMessageRetentionDuration())
	}

	// What UpdateTopic refuses is returned with its code and message.
	const refusal = "message_retention_duration out of bounds"
	addr2, c2 := pubsubEditFixture(t, project, pstest.WithErrorInjection("UpdateTopic", codes.InvalidArgument, refusal))
	if _, err := c2.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: name}); err != nil {
		t.Fatal(err)
	}
	err = pubsubProvider{endpoint: addr2}.Edit(ctx, project, []string{name}, map[string]string{"messageRetention": "5m"})
	if st, _ := status.FromError(err); st.Code() != codes.InvalidArgument || st.Message() != refusal {
		t.Errorf("a refused UpdateTopic = %v; want its INVALID_ARGUMENT %q", err, refusal)
	}
}

// A subscription's page carries Edit subscription, prefilled from it. Saving
// it sends one UpdateSubscription naming only what changed, and the official
// client reads back the ack deadline, retention, push endpoint and
// attributes, retry policy and dead-letter policy; emptying the endpoint
// switches it back to pull, and emptying the policies removes them. The
// topic, filter, expiration, ordering and exactly-once are shown and never
// sent, labels are not on the form, and a subscription saved unchanged is not
// written (#786).
func TestPubSubEditSubscriptionThroughUpdateSubscription(t *testing.T) {
	ctx := context.Background()
	const project = "edit-proj"
	addr, c := pubsubEditFixture(t, project)
	p := pubsubSubscriptionsProvider{endpoint: addr}
	topic := "projects/" + project + "/topics/orders"
	dead := "projects/" + project + "/topics/orders-dead"
	for _, n := range []string{topic, dead} {
		if _, err := c.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: n}); err != nil {
			t.Fatal(err)
		}
	}
	name := "projects/" + project + "/subscriptions/orders-sub"
	if _, err := c.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{
		Name: name, Topic: topic, AckDeadlineSeconds: 10, Filter: `attributes.kind = "a"`,
		EnableMessageOrdering: true, Labels: map[string]string{"team": "a"},
		MessageRetentionDuration: durationpb.New(7 * 24 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	form := pubsubEditOf(t, p, project, name)
	if form.Label != "Edit subscription" || !strings.Contains(form.Note, pubsubSubscriptionLabelsRefusal) {
		t.Errorf("edit form %q, note %q; want Edit subscription with the emulator's labels refusal", form.Label, form.Note)
	}
	immutable := map[string]bool{"name": true, "topic": true, "filter": true, "expiration": true, "messageOrdering": true, "exactlyOnce": true}
	for _, f := range form.Fields {
		if f.Name == "labels" {
			t.Error("the subscription form offers labels, which the emulator's UpdateSubscription refuses")
		}
		if immutable[f.Name] != f.Immutable {
			t.Errorf("field %s immutable = %v", f.Name, f.Immutable)
		}
	}
	if f, _ := fieldNamed(form, "filter"); f.Default != `attributes.kind = "a"` || !strings.Contains(f.Help, pubsubFilterRefusal) {
		t.Errorf("filter field = %+v", f)
	}
	values := pubsubSubmitted(form)
	want := map[string]string{
		"pushEndpoint": "", "pushAttributes": "", "ackDeadline": "10", "messageRetention": "7d", "retainAcked": "false",
		"minBackoff": "", "maxBackoff": "", "deadLetterTopic": "", "maxDeliveryAttempts": "5",
	}
	if len(values) != len(want) {
		t.Errorf("the form submits %v; want exactly %v", values, want)
	}
	for k, v := range want {
		if values[k] != v {
			t.Errorf("%s prefilled as %q, want %q", k, values[k], v)
		}
	}

	values["ackDeadline"] = "30"
	values["messageRetention"] = "2d"
	values["retainAcked"] = "true"
	values["pushEndpoint"] = "http://127.0.0.1:9/push"
	values["pushAttributes"] = `{"x-goog-version":"v1"}`
	values["minBackoff"] = "5s"
	values["maxBackoff"] = "1m"
	values["deadLetterTopic"] = dead
	values["maxDeliveryAttempts"] = "7"
	if err := p.Edit(ctx, project, []string{name}, values); err != nil {
		t.Fatalf("Edit: %v", err)
	}
	s, err := c.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: name})
	if err != nil {
		t.Fatal(err)
	}
	if s.GetAckDeadlineSeconds() != 30 || s.GetMessageRetentionDuration().AsDuration() != 48*time.Hour || !s.GetRetainAckedMessages() ||
		s.GetPushConfig().GetPushEndpoint() != "http://127.0.0.1:9/push" || s.GetPushConfig().GetAttributes()["x-goog-version"] != "v1" ||
		s.GetRetryPolicy().GetMinimumBackoff().AsDuration() != 5*time.Second || s.GetRetryPolicy().GetMaximumBackoff().AsDuration() != time.Minute ||
		s.GetDeadLetterPolicy().GetDeadLetterTopic() != dead || s.GetDeadLetterPolicy().GetMaxDeliveryAttempts() != 7 {
		t.Errorf("GetSubscription after the edit reads %v", s)
	}
	if s.GetLabels()["team"] != "a" || s.GetFilter() != `attributes.kind = "a"` || !s.GetEnableMessageOrdering() {
		t.Errorf("the edit changed what the form does not send: %v", s)
	}
	if got := pubsubSubmitted(pubsubEditOf(t, p, project, name)); got["pushEndpoint"] != "http://127.0.0.1:9/push" ||
		got["minBackoff"] != "5s" || got["maxBackoff"] != "1m0s" || got["messageRetention"] != "2d" {
		t.Errorf("the form after saving is prefilled with %v", got)
	}

	// Back to pull, and no retry or dead-letter policy.
	values["pushEndpoint"], values["pushAttributes"] = "", ""
	values["minBackoff"], values["maxBackoff"] = "", ""
	values["deadLetterTopic"] = ""
	if err := p.Edit(ctx, project, []string{name}, values); err != nil {
		t.Fatalf("Edit back to pull: %v", err)
	}
	s, _ = c.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: name})
	if s.GetPushConfig().GetPushEndpoint() != "" || len(s.GetPushConfig().GetAttributes()) != 0 ||
		s.GetRetryPolicy() != nil || s.GetDeadLetterPolicy() != nil || s.GetAckDeadlineSeconds() != 30 {
		t.Errorf("GetSubscription after switching back to pull reads %v", s)
	}

	// Refused before UpdateSubscription, changing nothing.
	for what, v := range map[string]map[string]string{
		"attributes with no endpoint": {"pushAttributes": `{"x-goog-version":"v1"}`},
		"a deadline not a number":     {"ackDeadline": "soon"},
		"a bad retention":             {"messageRetention": "a week"},
		"no retention":                {"messageRetention": ""},
		"a bad backoff":               {"minBackoff": "fast"},
		"attempts not a number":       {"deadLetterTopic": dead, "maxDeliveryAttempts": "many"},
		"a changed topic":             {"topic": dead},
		"a changed filter":            {"filter": ""},
		"a rename":                    {"name": "other"},
	} {
		bad := map[string]string{}
		for k, x := range values {
			bad[k] = x
		}
		for k, x := range v {
			bad[k] = x
		}
		if err := p.Edit(ctx, project, []string{name}, bad); err == nil {
			t.Errorf("an edit with %s was accepted", what)
		}
	}
	if err := p.Edit(ctx, "other-proj", []string{name}, values); err == nil {
		t.Error("an edit of another project's subscription was accepted")
	}
	if s, _ := c.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: name}); s.GetAckDeadlineSeconds() != 30 ||
		len(s.GetPushConfig().GetAttributes()) != 0 {
		t.Errorf("after refused edits GetSubscription reads %v", s)
	}
	if err := c.SubscriptionAdminClient.DeleteSubscription(ctx, &pubsubpb.DeleteSubscriptionRequest{Subscription: name}); err != nil {
		t.Fatal(err)
	}
	if err := p.Edit(ctx, project, []string{name}, values); status.Code(err) != codes.NotFound {
		t.Errorf("editing a deleted subscription = %v; want NOT_FOUND", err)
	}
}

// What UpdateSubscription refuses is returned with its code and message, and
// a subscription saved unchanged makes no call: with every UpdateSubscription
// refused, the unchanged save still succeeds. Saving a new push endpoint
// keeps the payload wrapper the form does not show (#786).
func TestPubSubEditSubscriptionSendsOnlyChangesAndKeepsWhatItDoesNotShow(t *testing.T) {
	ctx := context.Background()
	const project = "edit-proj"
	const refusal = "ack_deadline_secs out of bounds"
	addr, c := pubsubEditFixture(t, project, pstest.WithErrorInjection("UpdateSubscription", codes.InvalidArgument, refusal))
	p := pubsubSubscriptionsProvider{endpoint: addr}
	topic := "projects/" + project + "/topics/t1"
	if _, err := c.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: topic}); err != nil {
		t.Fatal(err)
	}
	name := "projects/" + project + "/subscriptions/s1"
	if _, err := c.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{Name: name, Topic: topic,
		PushConfig: &pubsubpb.PushConfig{PushEndpoint: "http://127.0.0.1:9/a",
			Wrapper: &pubsubpb.PushConfig_NoWrapper_{NoWrapper: &pubsubpb.PushConfig_NoWrapper{WriteMetadata: true}}}}); err != nil {
		t.Fatal(err)
	}
	values := pubsubSubmitted(pubsubEditOf(t, p, project, name))
	if err := p.Edit(ctx, project, []string{name}, values); err != nil {
		t.Errorf("saving unchanged called UpdateSubscription: %v", err)
	}
	values["ackDeadline"] = "601"
	err := p.Edit(ctx, project, []string{name}, values)
	if st, _ := status.FromError(err); st.Code() != codes.InvalidArgument || st.Message() != refusal {
		t.Errorf("a refused UpdateSubscription = %v; want its INVALID_ARGUMENT %q", err, refusal)
	}

	addr2, c2 := pubsubEditFixture(t, project)
	p2 := pubsubSubscriptionsProvider{endpoint: addr2}
	if _, err := c2.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: topic}); err != nil {
		t.Fatal(err)
	}
	if _, err := c2.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{Name: name, Topic: topic,
		PushConfig: &pubsubpb.PushConfig{PushEndpoint: "http://127.0.0.1:9/a",
			Wrapper: &pubsubpb.PushConfig_NoWrapper_{NoWrapper: &pubsubpb.PushConfig_NoWrapper{WriteMetadata: true}}}}); err != nil {
		t.Fatal(err)
	}
	values = pubsubSubmitted(pubsubEditOf(t, p2, project, name))
	values["pushEndpoint"] = "http://127.0.0.1:9/b"
	if err := p2.Edit(ctx, project, []string{name}, values); err != nil {
		t.Fatal(err)
	}
	s, _ := c2.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: name})
	if s.GetPushConfig().GetPushEndpoint() != "http://127.0.0.1:9/b" || !s.GetPushConfig().GetNoWrapper().GetWriteMetadata() {
		t.Errorf("after a new endpoint the push config is %v; want the endpoint changed and the wrapper kept", s.GetPushConfig())
	}
}

// A subscription that delivers to BigQuery is not offered the push fields,
// which would replace its delivery, and the note says so; durations read back
// in the form they are written (#786).
func TestPubSubEditFormLeavesOtherDeliveriesAndReadsDurations(t *testing.T) {
	form := subscriptionEditForm(&pubsubpb.Subscription{Name: "projects/p/subscriptions/s", Topic: "projects/p/topics/t",
		BigqueryConfig: &pubsubpb.BigQueryConfig{Table: "p.d.t"}})
	for _, f := range form.Fields {
		if f.Name == "pushEndpoint" || f.Name == "pushAttributes" {
			t.Errorf("a BigQuery subscription's form offers %s", f.Name)
		}
	}
	if !strings.Contains(form.Note, "BigQuery") {
		t.Errorf("the note does not say the BigQuery delivery is left as it is: %q", form.Note)
	}
	for in, want := range map[string]time.Duration{"7d": 7 * 24 * time.Hour, "36h": 36 * time.Hour, "10m": 10 * time.Minute, "90s": 90 * time.Second} {
		d, err := parsePubSubDuration("x", in)
		if err != nil || d.AsDuration() != want {
			t.Errorf("parse %q = %v (%v), want %v", in, d, err, want)
		}
	}
	for _, bad := range []string{"-1s", "1w", "d", "7.5d"} {
		if _, err := parsePubSubDuration("x", bad); err == nil {
			t.Errorf("parse %q was accepted", bad)
		}
	}
	for d, want := range map[time.Duration]string{7 * 24 * time.Hour: "7d", 36 * time.Hour: "36h0m0s", 10 * time.Second: "10s"} {
		if got := formatPubSubDuration(durationpb.New(d)); got != want {
			t.Errorf("format %v = %q, want %q", d, got, want)
		}
	}
}
