//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/pubsub/v2"
	pubsubpb "cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
)

// TestConsolePubSubActions.
//
// The console's Pub/Sub actions (#294), each checked through the official
// SDK: a subscription the console created has the settings it was given, a
// message the console published reaches an SDK subscriber with its data and
// attributes, a console "Pull and ack" leaves nothing for the SDK, and a
// console "Pull without ack" leaves the message for the SDK to receive
// again. Every action is in the operations ledger.
func TestConsolePubSubActions(t *testing.T) {
	h := New(t)
	addr := consoleAddr(t, h)
	ps := pubsubClient(t, h)
	ctx := h.Context()
	project := h.Project()
	tp := topic(t, h, ps, "console-actions")
	subID := "console-actions-sub"
	sub := fmt.Sprintf("projects/%s/subscriptions/%s", project, subID)
	t.Cleanup(func() {
		_ = ps.SubscriptionAdminClient.DeleteSubscription(context.Background(),
			&pubsubpb.DeleteSubscriptionRequest{Subscription: sub})
	})

	act := func(action string, values map[string]string) map[string]any {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"Path": []string{tp}, "Action": action, "Values": values})
		code, out := consoleDo(t, addr, http.MethodPost, "/api/actions/pubsub?project="+project, string(body))
		if code != http.StatusOK {
			t.Fatalf("console %s = %d: %s", action, code, out)
		}
		var res map[string]any
		_ = json.Unmarshal([]byte(out), &res)
		return res
	}
	resultIDs := func(res map[string]any) []string {
		r, _ := res["result"].(map[string]any)
		items, _ := r["items"].([]any)
		var ids []string
		for _, it := range items {
			ids = append(ids, it.(map[string]any)["name"].(string))
		}
		return ids
	}
	publish := func(data string, attrs map[string]string) string {
		t.Helper()
		a, _ := json.Marshal(attrs)
		ids := resultIDs(act("publish", map[string]string{"data": data, "attributes": string(a)}))
		if len(ids) != 1 || ids[0] == "" {
			t.Fatalf("publish returned no message ID: %v", ids)
		}
		return ids[0]
	}
	sdkPull := func() []*pubsubpb.ReceivedMessage {
		t.Helper()
		pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		resp, err := ps.SubscriptionAdminClient.Pull(pctx, &pubsubpb.PullRequest{
			Subscription: sub, MaxMessages: 10, ReturnImmediately: true})
		if err != nil && pctx.Err() == nil {
			t.Fatalf("SDK Pull: %v", err)
		}
		return resp.GetReceivedMessages()
	}

	// Create subscription, then read it back through the SDK.
	act("create-subscription", map[string]string{"name": subID, "ackDeadline": "20"})
	got, err := ps.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: sub})
	if err != nil {
		t.Fatalf("the SDK cannot read the console-created subscription: %v", err)
	}
	if got.GetTopic() != tp || got.GetAckDeadlineSeconds() != 20 || got.GetPushConfig().GetPushEndpoint() != "" {
		t.Errorf("console-created subscription = topic %s, ack %ds, push %q; want %s, 20s, pull",
			got.GetTopic(), got.GetAckDeadlineSeconds(), got.GetPushConfig().GetPushEndpoint(), tp)
	}

	// Publish with attributes; an SDK subscriber receives the same.
	attrs := map[string]string{"origin": "console", "kind": "test"}
	id := publish("hello from the console", attrs)
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	var received *pubsub.Message
	err = ps.Subscriber(sub).Receive(rctx, func(_ context.Context, m *pubsub.Message) {
		m.Ack()
		if received == nil {
			received = m
			cancel()
		}
	})
	cancel()
	if received == nil {
		t.Fatalf("the SDK subscriber received nothing (%v)", err)
	}
	if received.ID != id || string(received.Data) != "hello from the console" ||
		received.Attributes["origin"] != "console" || received.Attributes["kind"] != "test" || len(received.Attributes) != 2 {
		t.Errorf("SDK received id %s data %q attrs %v; want %s, the console's data and %v",
			received.ID, received.Data, received.Attributes, id, attrs)
	}
	// Leave nothing behind for the next step: the ack above is async.
	time.Sleep(time.Second)
	for _, m := range sdkPull() {
		_ = ps.SubscriptionAdminClient.Acknowledge(ctx, &pubsubpb.AcknowledgeRequest{Subscription: sub, AckIds: []string{m.GetAckId()}})
	}

	// Pull and ack: the console gets the message, the SDK then gets nothing.
	id = publish("acked by the console", nil)
	if ids := resultIDs(act("pull-ack", map[string]string{"subscription": subID})); len(ids) != 1 || ids[0] != id {
		t.Fatalf("console pull-ack returned %v, want [%s]", ids, id)
	}
	if ms := sdkPull(); len(ms) != 0 {
		t.Errorf("after a console pull with ack, the SDK pulled %d messages", len(ms))
	}

	// Pull without ack: the console sees it, and the SDK is redelivered it.
	id = publish("left by the console", nil)
	if ids := resultIDs(act("pull-no-ack", map[string]string{"subscription": subID})); len(ids) != 1 || ids[0] != id {
		t.Fatalf("console pull-no-ack returned %v, want [%s]", ids, id)
	}
	ms := sdkPull()
	if len(ms) != 1 || ms[0].GetMessage().GetMessageId() != id {
		t.Fatalf("after a console pull without ack, the SDK pulled %d messages, want the redelivered %s", len(ms), id)
	}
	_ = ps.SubscriptionAdminClient.Acknowledge(ctx, &pubsubpb.AcknowledgeRequest{Subscription: sub, AckIds: []string{ms[0].GetAckId()}})

	// The copy for "Pull without ack" says what it does to the message.
	code, out := consoleDo(t, addr, http.MethodGet,
		"/api/detail/pubsub?project="+project+"&name="+url.QueryEscape(tp), "")
	if code != http.StatusOK {
		t.Fatalf("detail = %d: %s", code, out)
	}
	var detail struct {
		Actions []struct {
			ID     string
			Label  string
			Fields []struct{ Help string }
		}
	}
	_ = json.Unmarshal([]byte(out), &detail)
	copyOK := false
	for _, a := range detail.Actions {
		if a.ID == "pull-no-ack" {
			for _, f := range a.Fields {
				copyOK = copyOK || strings.Contains(f.Help, "delivery attempts")
			}
		}
	}
	if !copyOK {
		t.Errorf("the topic page offers no Pull without ack whose copy mentions delivery attempts: %s", out)
	}

	// Every action is in the ledger, against the topic, succeeded.
	code, out = consoleDo(t, addr, http.MethodGet, "/api/operations?project="+project, "")
	if code != http.StatusOK {
		t.Fatalf("operations = %d: %s", code, out)
	}
	var ops struct {
		Operations []struct{ Kind, Resource, State string }
	}
	_ = json.Unmarshal([]byte(out), &ops)
	for _, kind := range []string{"create-subscription", "publish", "pull-ack", "pull-no-ack"} {
		found := false
		for _, o := range ops.Operations {
			found = found || (o.Kind == kind && o.Resource == tp && o.State == "SUCCEEDED")
		}
		if !found {
			t.Errorf("no succeeded %s on %s in the operations ledger", kind, tp)
		}
	}
	if strings.Contains(out, "hello from the console") || strings.Contains(out, "left by the console") {
		t.Error("the operations ledger holds message data")
	}
}
