//go:build compat

package compat

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	pubsubpb "cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// consolePubSubEditForm reads a topic's or subscription's page from the
// console and returns its edit form's note, the help of each field it shows
// disabled, and the values it would submit: every field's default except the
// immutable ones, which the browser does not send.
func consolePubSubEditForm(t *testing.T, addr, service, project, name, label string) (string, map[string]string, map[string]string) {
	t.Helper()
	code, body := consoleDo(t, addr, http.MethodGet, "/api/detail/"+service+"?project="+project+"&name="+url.QueryEscape(name), "")
	if code != http.StatusOK {
		t.Fatalf("console detail of %s = %d: %s", name, code, body)
	}
	var detail struct {
		Unavailable string
		Edit        *struct {
			Label, Note string
			Fields      []struct {
				Name, Default, Help string
				Immutable           bool
			}
		}
	}
	if err := json.Unmarshal([]byte(body), &detail); err != nil {
		t.Fatalf("decode detail: %v: %s", err, body)
	}
	if detail.Edit == nil || detail.Edit.Label != label {
		t.Fatalf("the page of %s offers no %s (unavailable: %q): %s", name, label, detail.Unavailable, body)
	}
	values, fixed := map[string]string{}, map[string]string{}
	for _, f := range detail.Edit.Fields {
		if f.Name == "labels" {
			t.Errorf("%s offers labels, which the emulator refuses", label)
		}
		if f.Immutable {
			fixed[f.Name] = f.Help
		} else {
			values[f.Name] = f.Default
		}
	}
	return detail.Edit.Note, fixed, values
}

// TestConsolePubSubEditTopicAndSubscription (#786): the console's Edit topic
// changes a topic's retention and its Edit subscription a subscription's ack
// deadline, retention, retry and dead-letter policies, and switches it from
// pull to push and back; the official client reads back each change. A value
// the emulator refuses is refused on the form with exactly the message the
// official client's own call receives, and changes nothing. Labels, which the
// emulator's UpdateTopic and UpdateSubscription refuse, are not on either form;
// each form's note quotes that refusal, as the subscription form's disabled
// filter and expiration quote theirs, and this test asserts each is still
// what the official client receives. Exactly-once delivery is turned on and
// off from the form, and refused with a push endpoint (#880).
func TestConsolePubSubEditTopicAndSubscription(t *testing.T) {
	h := New(t)
	addr := consoleAddr(t, h)
	ps := pubsubClient(t, h)
	ctx := h.Context()
	project := h.Project()
	tp := topic(t, h, ps, "console-edit")
	dead := topic(t, h, ps, "console-edit-dead")
	sub := subscription(t, h, ps, "console-edit-sub", tp)

	edit := func(service, name string, v map[string]string) (int, string) {
		body, _ := json.Marshal(map[string]any{"Path": []string{name}, "Values": v})
		return consoleDo(t, addr, http.MethodPatch, "/api/resources/"+service+"?project="+project, string(body))
	}
	// sameRefusal asserts the console refused with the official client's own
	// status for the same request.
	sameRefusal := func(what string, code int, body string, sdkErr error) {
		t.Helper()
		st, _ := status.FromError(sdkErr)
		if sdkErr == nil || st.Message() == "" {
			t.Fatalf("the official client's %s = %v; want it refused", what, sdkErr)
		}
		var refusal struct{ Error string }
		_ = json.Unmarshal([]byte(body), &refusal)
		if want := st.Code().String() + ": " + st.Message(); code != http.StatusBadRequest || refusal.Error != want {
			t.Errorf("console %s = %d %s; want 400 with the official client's own %q", what, code, body, want)
		}
	}

	// --- topic
	note, _, values := consolePubSubEditForm(t, addr, "pubsub", project, tp, "Edit topic")
	_, labelsErr := ps.TopicAdminClient.UpdateTopic(ctx, &pubsubpb.UpdateTopicRequest{
		Topic:      &pubsubpb.Topic{Name: tp, Labels: map[string]string{"env": "dev"}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"labels"}}})
	if st, _ := status.FromError(labelsErr); labelsErr == nil || !strings.Contains(note, st.Message()) {
		t.Errorf("UpdateTopic(labels) = %v; the Edit topic note must quote the emulator's refusal: %q", labelsErr, note)
	}
	values["messageRetention"] = "2d"
	if code, body := edit("pubsub", tp, values); code != http.StatusOK {
		t.Fatalf("console topic edit = %d: %s", code, body)
	}
	got, err := ps.TopicAdminClient.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: tp})
	if err != nil || got.GetMessageRetentionDuration().AsDuration() != 48*time.Hour {
		t.Errorf("GetTopic after the console edit = %v (%v); want 48h retention", got, err)
	}
	_, sdkErr := ps.TopicAdminClient.UpdateTopic(ctx, &pubsubpb.UpdateTopicRequest{
		Topic:      &pubsubpb.Topic{Name: tp, MessageRetentionDuration: durationpb.New(5 * time.Minute)},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"message_retention_duration"}}})
	values["messageRetention"] = "5m"
	code, body := edit("pubsub", tp, values)
	sameRefusal("topic retention of 5m", code, body, sdkErr)
	if got, _ := ps.TopicAdminClient.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: tp}); got.GetMessageRetentionDuration().AsDuration() != 48*time.Hour {
		t.Errorf("after a refused edit the topic's retention is %v, want 48h", got.GetMessageRetentionDuration())
	}
	// An emptied retention is refused by the console: the emulator's own
	// UpdateTopic with the mask and no value sets 31 days rather than
	// removing it, which the official client then reads.
	values["messageRetention"] = ""
	if code, body := edit("pubsub", tp, values); code != http.StatusBadRequest || !strings.Contains(body, "31 days") {
		t.Errorf("console edit emptying the topic's retention = %d %s; want 400 saying the emulator would save 31 days", code, body)
	}
	if _, err := ps.TopicAdminClient.UpdateTopic(ctx, &pubsubpb.UpdateTopicRequest{
		Topic: &pubsubpb.Topic{Name: tp}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"message_retention_duration"}}}); err != nil {
		t.Fatalf("UpdateTopic clearing the retention: %v", err)
	}
	if got, _ := ps.TopicAdminClient.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: tp}); got.GetMessageRetentionDuration().AsDuration() != 31*24*time.Hour {
		t.Errorf("after UpdateTopic cleared the retention the client reads %v; the console's refusal says 31 days", got.GetMessageRetentionDuration())
	}

	// --- subscription
	note, fixed, values := consolePubSubEditForm(t, addr, "pubsub-subscriptions", project, sub, "Edit subscription")
	_, labelsErr = ps.SubscriptionAdminClient.UpdateSubscription(ctx, &pubsubpb.UpdateSubscriptionRequest{
		Subscription: &pubsubpb.Subscription{Name: sub, Labels: map[string]string{"env": "dev"}},
		UpdateMask:   &fieldmaskpb.FieldMask{Paths: []string{"labels"}}})
	if st, _ := status.FromError(labelsErr); labelsErr == nil || !strings.Contains(note, st.Message()) {
		t.Errorf("UpdateSubscription(labels) = %v; the Edit subscription note must quote the emulator's refusal: %q", labelsErr, note)
	}
	// The filter and the expiration are shown disabled, each with the
	// emulator's own refusal of a change to it.
	for field, path := range map[string]string{"filter": "filter", "expiration": "expiration_policy"} {
		_, err := ps.SubscriptionAdminClient.UpdateSubscription(ctx, &pubsubpb.UpdateSubscriptionRequest{
			Subscription: &pubsubpb.Subscription{Name: sub, Filter: `attributes.k = "v"`,
				ExpirationPolicy: &pubsubpb.ExpirationPolicy{Ttl: durationpb.New(48 * time.Hour)}},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{path}}})
		help, shown := fixed[field]
		if st, _ := status.FromError(err); err == nil || !shown || !strings.Contains(help, st.Message()) {
			t.Errorf("UpdateSubscription(%s) = %v; the form must show %s disabled with that refusal, not %q", path, err, field, help)
		}
	}
	if values["pushEndpoint"] != "" || values["ackDeadline"] != "10" {
		t.Fatalf("the edit form is not prefilled from the pull subscription: %v", values)
	}
	// Nothing is published, so the endpoint is never called.
	const endpoint = "http://127.0.0.1:9/console-edit"
	values["ackDeadline"] = "30"
	values["messageRetention"] = "2d"
	values["retainAcked"] = "true"
	values["pushEndpoint"] = endpoint
	values["pushAttributes"] = `{"x-goog-version":"v1"}`
	values["minBackoff"] = "5s"
	values["maxBackoff"] = "1m"
	values["deadLetterTopic"] = dead
	values["maxDeliveryAttempts"] = "7"
	if code, body := edit("pubsub-subscriptions", sub, values); code != http.StatusOK {
		t.Fatalf("console subscription edit = %d: %s", code, body)
	}
	s, err := ps.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: sub})
	if err != nil {
		t.Fatalf("GetSubscription: %v", err)
	}
	if s.GetAckDeadlineSeconds() != 30 || s.GetMessageRetentionDuration().AsDuration() != 48*time.Hour || !s.GetRetainAckedMessages() ||
		s.GetRetryPolicy().GetMinimumBackoff().AsDuration() != 5*time.Second || s.GetRetryPolicy().GetMaximumBackoff().AsDuration() != time.Minute ||
		s.GetDeadLetterPolicy().GetDeadLetterTopic() != dead || s.GetDeadLetterPolicy().GetMaxDeliveryAttempts() != 7 {
		t.Errorf("GetSubscription after the console edit reads %v", s)
	}
	if s.GetPushConfig().GetPushEndpoint() != endpoint || s.GetPushConfig().GetAttributes()["x-goog-version"] != "v1" {
		t.Errorf("after switching to push from the console the client reads push config %v; want %s", s.GetPushConfig(), endpoint)
	}

	_, sdkErr = ps.SubscriptionAdminClient.UpdateSubscription(ctx, &pubsubpb.UpdateSubscriptionRequest{
		Subscription: &pubsubpb.Subscription{Name: sub, AckDeadlineSeconds: 601},
		UpdateMask:   &fieldmaskpb.FieldMask{Paths: []string{"ack_deadline_seconds"}}})
	bad := map[string]string{}
	for k, v := range values {
		bad[k] = v
	}
	bad["ackDeadline"] = "601"
	code, body = edit("pubsub-subscriptions", sub, bad)
	sameRefusal("ack deadline of 601", code, body, sdkErr)
	if s, _ := ps.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: sub}); s.GetAckDeadlineSeconds() != 30 {
		t.Errorf("after a refused edit the ack deadline is %d, want 30", s.GetAckDeadlineSeconds())
	}

	// Back to pull, with no retry or dead-letter policy.
	values["pushEndpoint"], values["pushAttributes"] = "", ""
	values["minBackoff"], values["maxBackoff"], values["deadLetterTopic"] = "", "", ""
	if code, body := edit("pubsub-subscriptions", sub, values); code != http.StatusOK {
		t.Fatalf("console edit back to pull = %d: %s", code, body)
	}
	s, err = ps.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: sub})
	if err != nil || s.GetPushConfig().GetPushEndpoint() != "" || s.GetRetryPolicy() != nil || s.GetDeadLetterPolicy() != nil {
		t.Errorf("GetSubscription after switching back to pull = %v (%v)", s, err)
	}

	// Exactly-once delivery (#880): on, refused with a push endpoint, off.
	values["exactlyOnce"] = "true"
	if code, body := edit("pubsub-subscriptions", sub, values); code != http.StatusOK {
		t.Fatalf("console edit to exactly-once = %d: %s", code, body)
	}
	if s, err := ps.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: sub}); err != nil ||
		!s.GetEnableExactlyOnceDelivery() {
		t.Errorf("after turning exactly-once on from the console the client reads %v (%v)", s.GetEnableExactlyOnceDelivery(), err)
	}
	withPush := map[string]string{}
	for k, v := range values {
		withPush[k] = v
	}
	withPush["pushEndpoint"] = endpoint
	if code, body := edit("pubsub-subscriptions", sub, withPush); code == http.StatusOK || !strings.Contains(body, "pull subscriptions only") {
		t.Errorf("console edit to exactly-once with a push endpoint = %d: %s; want the refusal", code, body)
	}
	values["exactlyOnce"] = "false"
	if code, body := edit("pubsub-subscriptions", sub, values); code != http.StatusOK {
		t.Fatalf("console edit exactly-once off = %d: %s", code, body)
	}
	if s, err := ps.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: sub}); err != nil ||
		s.GetEnableExactlyOnceDelivery() || s.GetPushConfig().GetPushEndpoint() != "" {
		t.Errorf("after turning exactly-once off from the console the client reads %v (%v)", s, err)
	}
}
