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

// A topic's page carries Edit topic, prefilled from the topic: its retention
// and, since CloudBurrow's front applies them (#949), its labels, each sent
// only when changed. Saving a retention or labels is what the official
// client's GetTopic reads; an emptied retention, which the emulator would
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
	if form.Label != "Edit" || form.Title != "Edit topic" || form.Submit != "Update" || form.Layout != console.EditLayoutPage ||
		strings.Contains(form.Note, "abels") {
		t.Errorf("edit form %+v; want Google's Edit opening the Edit topic page, ending in Update, its note not naming labels", form)
	}
	if f, ok := fieldNamed(form, "labels"); !ok || f.Type != "map" || f.Default != `{"team":"a"}` || f.Help != pubsubLabelsHelp {
		t.Errorf("labels field = %+v; want a map prefilled with team=a", f)
	}
	if f, _ := fieldNamed(form, "name"); !f.Immutable || f.Default != "orders" {
		t.Errorf("name field = %+v; want orders, immutable", f)
	}
	values := pubsubSubmitted(form)
	if values["enableRetention"] != "false" || values["useSchema"] != "false" || values["labels"] != `{"team":"a"}` ||
		values["retentionDays"] != "7" {
		t.Fatalf("a topic with no retention submits %v; want no retention, no schema and its labels", values)
	}
	if err := p.Edit(ctx, project, []string{name}, values); err != nil {
		t.Fatalf("saving the form unchanged: %v", err)
	}
	if got, _ := c.TopicAdminClient.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: name}); got.GetMessageRetentionDuration() != nil {
		t.Errorf("saving unchanged gave the topic a retention: %v", got.GetMessageRetentionDuration())
	}

	values["enableRetention"], values["retentionDays"] = "true", "2"
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
	if got := pubsubSubmitted(form); got["enableRetention"] != "true" || got["retentionDays"] != "2" ||
		got["retentionHours"] != "0" || got["retentionMinutes"] != "0" {
		t.Errorf("after saving, the retention is prefilled %v; want 2 days", got)
	}
	// Clearing Enable message retention is refused: the emulator would
	// save 31 days.
	cleared := pubsubSubmitted(form)
	cleared["enableRetention"] = "false"
	if err := p.Edit(ctx, project, []string{name}, cleared); err == nil || err.Error() != pubsubRetentionUnclearable {
		t.Errorf("clearing Enable message retention = %v; want %q", err, pubsubRetentionUnclearable)
	}

	// Labels, changed and then removed, with the retention left as it is.
	values["labels"] = `{"team":"b","env":"prod"}`
	if err := p.Edit(ctx, project, []string{name}, values); err != nil {
		t.Fatalf("Edit labels: %v", err)
	}
	got, err = c.TopicAdminClient.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: name})
	if err != nil || got.GetLabels()["team"] != "b" || got.GetLabels()["env"] != "prod" || len(got.GetLabels()) != 2 ||
		got.GetMessageRetentionDuration().AsDuration() != 48*time.Hour {
		t.Errorf("GetTopic after a labels edit = %v (%v); want team=b, env=prod and the retention kept", got, err)
	}
	values["labels"] = ""
	if err := p.Edit(ctx, project, []string{name}, values); err != nil {
		t.Fatalf("Edit labels: %v", err)
	}
	if got, err = c.TopicAdminClient.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: name}); err != nil || len(got.GetLabels()) != 0 {
		t.Errorf("GetTopic after emptying the labels = %v (%v); want none", got.GetLabels(), err)
	}
	if err := p.Edit(ctx, project, []string{name}, map[string]string{"messageRetention": "2d", "labels": "team=a"}); err == nil {
		t.Error("labels that are not a JSON object were accepted")
	}
	values["labels"] = `{"team":"a"}`
	if err := p.Edit(ctx, project, []string{name}, values); err != nil {
		t.Fatalf("Edit labels: %v", err)
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
// switches it back to pull, and emptying the policies removes them.
// Exactly-once delivery is turned on and off (#880), and refused with a push
// endpoint. The expiration period is set and cleared to never (#891). The
// topic, filter and ordering are shown and never sent, labels are edited
// (#949), and a subscription saved unchanged is not written (#786).
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
	if form.Label != "Edit" || form.Title != "Edit subscription" || form.Submit != "Update" ||
		form.Layout != console.EditLayoutPage || strings.Contains(form.Note, "abels") {
		t.Errorf("edit form %+v; want Google's Edit opening the Edit subscription page, ending in Update", form)
	}
	if f, ok := fieldNamed(form, "labels"); !ok || f.Type != "map" || f.Help != pubsubLabelsHelp {
		t.Errorf("labels field = %+v; want a map", f)
	}
	immutable := map[string]bool{"name": true, "topic": true, "filter": true, "messageOrdering": true}
	for _, f := range form.Fields {
		if immutable[f.Name] != f.Immutable {
			t.Errorf("field %s immutable = %v", f.Name, f.Immutable)
		}
	}
	if f, _ := fieldNamed(form, "filter"); f.Default != `attributes.kind = "a"` || !strings.Contains(f.Help, pubsubFilterRefusal) {
		t.Errorf("filter field = %+v", f)
	}
	if f, _ := fieldNamed(form, "deliveryType"); len(f.UnavailableOptions) != 0 || len(f.Options) != 4 {
		t.Errorf("delivery type = %+v; want all four types offered on a pull subscription (#2051)", f)
	}
	values := pubsubSubmitted(form)
	want := map[string]string{
		"deliveryType": deliveryPull, "pushEndpoint": "", "pushAttributes": "", "ackDeadline": "10", "messageRetention": "7d",
		"retainAcked": "false", "retryPolicy": retryImmediately, "minBackoff": "10", "maxBackoff": "600",
		"deadLettering": "false", "deadLetterTopic": "", "maxDeliveryAttempts": "5", "exactlyOnce": "false",
		"expirationMode": expireAfterDays, "expirationDays": "31", "labels": `{"team":"a"}`,
	}
	for k, v := range want {
		if values[k] != v {
			t.Errorf("%s prefilled as %q, want %q", k, values[k], v)
		}
	}
	// Saved unchanged, it changes nothing.
	if err := p.Edit(ctx, project, []string{name}, values); err != nil {
		t.Fatalf("Edit unchanged: %v", err)
	}
	if s, _ := c.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: name}); s.GetExpirationPolicy() != nil ||
		s.GetRetryPolicy() != nil {
		t.Errorf("saving unchanged wrote %v", s)
	}

	values["ackDeadline"] = "30"
	values["messageRetention"] = "2d"
	values["retainAcked"] = "true"
	values["deliveryType"] = deliveryPush
	values["pushEndpoint"] = "http://127.0.0.1:9/push"
	values["pushAttributes"] = `{"x-goog-version":"v1"}`
	values["retryPolicy"], values["minBackoff"], values["maxBackoff"] = retryBackoff, "5", "60"
	values["deadLettering"], values["deadLetterTopic"], values["maxDeliveryAttempts"] = "true", "orders-dead", "7"
	values["expirationDays"] = "3"
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
		s.GetDeadLetterPolicy().GetDeadLetterTopic() != dead || s.GetDeadLetterPolicy().GetMaxDeliveryAttempts() != 7 ||
		s.GetExpirationPolicy().GetTtl().AsDuration() != 72*time.Hour {
		t.Errorf("GetSubscription after the edit reads %v", s)
	}
	if s.GetLabels()["team"] != "a" || s.GetFilter() != `attributes.kind = "a"` || !s.GetEnableMessageOrdering() {
		t.Errorf("the edit changed what the form does not send: %v", s)
	}
	if got := pubsubSubmitted(pubsubEditOf(t, p, project, name)); got["deliveryType"] != deliveryPush ||
		got["pushEndpoint"] != "http://127.0.0.1:9/push" || got["retryPolicy"] != retryBackoff || got["minBackoff"] != "5" ||
		got["maxBackoff"] != "60" || got["messageRetention"] != "2d" || got["expirationDays"] != "3" ||
		got["deadLettering"] != "true" || got["deadLetterTopic"] != dead {
		t.Errorf("the form after saving is prefilled with %v", got)
	}

	// Back to Pull: the endpoint typed is not sent; no retry or dead-letter
	// policy; Never expire.
	values["deliveryType"] = deliveryPull
	values["retryPolicy"] = retryImmediately
	values["deadLettering"] = "false"
	values["expirationMode"] = expireNever
	if err := p.Edit(ctx, project, []string{name}, values); err != nil {
		t.Fatalf("Edit back to pull: %v", err)
	}
	s, _ = c.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: name})
	if s.GetPushConfig().GetPushEndpoint() != "" || len(s.GetPushConfig().GetAttributes()) != 0 ||
		s.GetRetryPolicy() != nil || s.GetDeadLetterPolicy() != nil || s.GetAckDeadlineSeconds() != 30 ||
		s.GetExpirationPolicy() == nil || s.GetExpirationPolicy().GetTtl() != nil {
		t.Errorf("GetSubscription after switching back to pull, never expiring, reads %v", s)
	}
	if got := pubsubSubmitted(pubsubEditOf(t, p, project, name)); got["expirationMode"] != expireNever {
		t.Errorf("a subscription that never expires is prefilled with %q, want %s", got["expirationMode"], expireNever)
	}
	// An export delivery type is offered on Edit (#2051): its fields set
	// bigquery_config, and Pull clears it again; the table is required.
	toExport := map[string]string{}
	for k, x := range values {
		toExport[k] = x
	}
	toExport["deliveryType"] = deliveryBigQuery
	if err := p.Edit(ctx, project, []string{name}, toExport); err == nil || !strings.Contains(err.Error(), "table is required") {
		t.Errorf("Edit to Write to BigQuery without a table = %v; want the refusal", err)
	}
	toExport["bigqueryTable"], toExport["bigqueryWriteMetadata"] = "p.d.t", "true"
	if err := p.Edit(ctx, project, []string{name}, toExport); err != nil {
		t.Fatalf("Edit to Write to BigQuery: %v", err)
	}
	if s, _ := c.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: name}); s.GetBigqueryConfig().GetTable() != "p.d.t" ||
		!s.GetBigqueryConfig().GetWriteMetadata() || s.GetPushConfig().GetPushEndpoint() != "" {
		t.Errorf("after the switch to BigQuery GetSubscription reads %v", s)
	}
	if got := pubsubSubmitted(pubsubEditOf(t, p, project, name)); got["deliveryType"] != deliveryBigQuery ||
		got["bigqueryTable"] != "p.d.t" || got["bigqueryWriteMetadata"] != "true" {
		t.Errorf("a BigQuery subscription's form is prefilled with %v", got)
	}
	if err := p.Edit(ctx, project, []string{name}, values); err != nil {
		t.Fatalf("Edit from BigQuery back to pull: %v", err)
	}
	if s, _ := c.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: name}); s.GetBigqueryConfig() != nil {
		t.Errorf("after the switch back to pull GetSubscription reads %v", s)
	}

	// Exactly-once on, refused with a push endpoint, and off again.
	values["exactlyOnce"] = "true"
	if err := p.Edit(ctx, project, []string{name}, values); err != nil {
		t.Fatalf("Edit to exactly-once: %v", err)
	}
	if s, _ := c.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: name}); !s.GetEnableExactlyOnceDelivery() {
		t.Errorf("after turning exactly-once on GetSubscription reads %v", s)
	}
	withPush := map[string]string{}
	for k, x := range values {
		withPush[k] = x
	}
	withPush["deliveryType"], withPush["pushEndpoint"] = deliveryPush, "http://127.0.0.1:9/push"
	if err := p.Edit(ctx, project, []string{name}, withPush); err == nil || !strings.Contains(err.Error(), "pull subscriptions only") {
		t.Errorf("exactly-once with a push endpoint = %v; want the refusal", err)
	}
	values["exactlyOnce"] = "false"
	if err := p.Edit(ctx, project, []string{name}, values); err != nil {
		t.Fatalf("Edit exactly-once off: %v", err)
	}
	if s, _ := c.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: name}); s.GetEnableExactlyOnceDelivery() ||
		s.GetPushConfig().GetPushEndpoint() != "" {
		t.Errorf("after turning exactly-once off GetSubscription reads %v", s)
	}

	// Labels (#949), changed alone.
	values["labels"] = `{"team":"b","env":"prod"}`
	if err := p.Edit(ctx, project, []string{name}, values); err != nil {
		t.Fatalf("Edit labels: %v", err)
	}
	if s, _ := c.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: name}); len(s.GetLabels()) != 2 ||
		s.GetLabels()["team"] != "b" || s.GetLabels()["env"] != "prod" || s.GetAckDeadlineSeconds() != 30 {
		t.Errorf("after a labels edit GetSubscription reads %v", s)
	}

	// Refused before UpdateSubscription, changing nothing.
	for what, v := range map[string]map[string]string{
		"bad labels":              {"labels": "team=a"},
		"a deadline not a number": {"ackDeadline": "soon"},
		"a bad retention":         {"messageRetention": "a week"},
		"no retention":            {"messageRetention": ""},
		"a bad backoff":           {"retryPolicy": retryBackoff, "minBackoff": "fast"},
		"a bad expiration":        {"expirationMode": expireAfterDays, "expirationDays": "soon"},
		"a zero expiration":       {"expirationMode": expireAfterDays, "expirationDays": "0"},
		"attempts not a number":   {"deadLettering": "true", "deadLetterTopic": dead, "maxDeliveryAttempts": "many"},
		"a changed topic":         {"topic": dead},
		"a changed filter":        {"filter": ""},
		"a rename":                {"name": "other"},
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
	// A BigQuery subscription's delivery can change (#2051), prefilled.
	form := subscriptionEditForm(&pubsubpb.Subscription{Name: "projects/p/subscriptions/s", Topic: "projects/p/topics/t",
		BigqueryConfig: &pubsubpb.BigQueryConfig{Table: "p.d.t", UseTableSchema: true}})
	if f, _ := fieldNamed(form, "deliveryType"); f.Immutable || f.Default != deliveryBigQuery || len(f.UnavailableOptions) != 0 {
		t.Errorf("a BigQuery subscription's delivery type: %+v", f)
	}
	if f, _ := fieldNamed(form, "bigqueryTable"); f.Default != "p.d.t" {
		t.Errorf("the table is prefilled with %q", f.Default)
	}
	if f, _ := fieldNamed(form, "bigquerySchema"); f.Default != schemaTable {
		t.Errorf("the schema configuration is prefilled with %q", f.Default)
	}
	// A Bigtable one keeps its delivery as it is.
	form = subscriptionEditForm(&pubsubpb.Subscription{Name: "projects/p/subscriptions/s", Topic: "projects/p/topics/t",
		BigtableConfig: &pubsubpb.BigtableConfig{Table: "projects/p/instances/i/tables/t"}})
	for _, f := range form.Fields {
		if f.Name == "pushEndpoint" || f.Name == "bigqueryTable" {
			t.Errorf("a Bigtable subscription's form offers %s", f.Name)
		}
	}
	if f, _ := fieldNamed(form, "deliveryType"); !f.Immutable || !strings.Contains(f.Help, "Bigtable") {
		t.Errorf("the delivery type does not say the Bigtable delivery is left as it is: %+v", f)
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
