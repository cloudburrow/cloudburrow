//go:build compat

package compat

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
)

// TestGcloudPubSubUpdateExpiration (#908): `gcloud pubsub subscriptions
// update --expiration-period`, a REST PATCH of expirationPolicy, is applied
// by CloudBurrow's front as the gRPC update is (#891), where the emulator
// alone refuses it: 12h is refused with Google's rule; 3d and never are
// applied, and `describe`, `list`, plain REST GET and the gRPC client read
// them; and they are enforced: with a 1-day ttl raised to 3 days, the
// subscription is kept 25 hours on and gone once idle for 73, and one set to
// never expire is kept. A plain REST PATCH does the same.
// covers: google.pubsub.v1.Subscriber/UpdateSubscription, google.pubsub.v1.Subscriber/GetSubscription, google.pubsub.v1.Subscriber/ListSubscriptions
func TestGcloudPubSubUpdateExpiration(t *testing.T) {
	h := New(t)
	c := pubsubClient(t, h)
	g := newGcloudSession(t, h)
	ctx := h.Context()
	p := "--project=" + h.Project()
	topicID := "gcloud-exp-topic"
	name := func(id string) string { return "projects/" + h.Project() + "/subscriptions/gcloud-exp-" + id }
	ids := []string{"three", "never", "rest"}
	t.Cleanup(func() {
		for _, id := range ids {
			_ = c.SubscriptionAdminClient.DeleteSubscription(context.Background(), &pubsubpb.DeleteSubscriptionRequest{Subscription: name(id)})
		}
		_ = c.TopicAdminClient.DeleteTopic(context.Background(), &pubsubpb.DeleteTopicRequest{Topic: "projects/" + h.Project() + "/topics/" + topicID})
	})
	g.must("pubsub", "topics", "create", topicID, p)
	for _, id := range ids {
		g.must("pubsub", "subscriptions", "create", "gcloud-exp-"+id, p, "--topic="+topicID,
			"--expiration-period=1d", "--message-retention-duration=1d")
	}
	ttl := func(id string) string {
		return strings.TrimSpace(g.must("pubsub", "subscriptions", "describe", "gcloud-exp-"+id, p, "--format=value(expirationPolicy.ttl)"))
	}

	out, err := g.run(nil, "pubsub", "subscriptions", "update", "gcloud-exp-three", p, "--expiration-period=12h")
	if err == nil || !strings.Contains(out, "at least 1 day") {
		t.Errorf("update --expiration-period=12h = %v:\n%s\nwant the refusal", err, out)
	}
	if got := ttl("three"); got != "86400s" {
		t.Errorf("after the refused update, the ttl describes as %q, want 86400s", got)
	}
	g.must("pubsub", "subscriptions", "update", "gcloud-exp-three", p, "--expiration-period=3d")
	g.must("pubsub", "subscriptions", "update", "gcloud-exp-never", p, "--expiration-period=never")
	code, body := pubsubREST(t, h, http.MethodPatch, "/v1/"+name("rest"),
		`{"subscription":{"expirationPolicy":{"ttl":"259200s"},"ackDeadlineSeconds":20},"updateMask":"expirationPolicy,ackDeadlineSeconds"}`)
	if pol, _ := body["expirationPolicy"].(map[string]any); code != http.StatusOK || pol["ttl"] != "259200s" || body["ackDeadlineSeconds"] != float64(20) {
		t.Errorf("REST PATCH = %d %v, want a 3-day ttl and a 20s deadline", code, body)
	}

	for id, want := range map[string]string{"three": "259200s", "never": "", "rest": "259200s"} {
		if got := ttl(id); got != want {
			t.Errorf("gcloud describes %s's ttl as %q, want %q", id, got, want)
		}
		s, err := c.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: name(id)})
		wantTTL := map[string]time.Duration{"three": 72 * time.Hour, "rest": 72 * time.Hour}[id]
		if err != nil || s.GetExpirationPolicy() == nil || s.GetExpirationPolicy().GetTtl().AsDuration() != wantTTL {
			t.Errorf("over gRPC, %s reads %v, %v", id, s.GetExpirationPolicy(), err)
		}
		_, body := pubsubREST(t, h, http.MethodGet, "/v1/"+name(id), "")
		pol, _ := body["expirationPolicy"].(map[string]any)
		if got, _ := pol["ttl"].(string); pol == nil || got != want {
			t.Errorf("REST GET of %s = %v, want ttl %q", id, body, want)
		}
	}
	listed := g.must("pubsub", "subscriptions", "list", p, "--format=value(name,expirationPolicy.ttl)")
	for _, line := range strings.Split(strings.TrimSpace(listed), "\n") {
		if strings.Contains(line, "gcloud-exp-three") && !strings.Contains(line, "259200s") {
			t.Errorf("gcloud lists %q, want the 3-day ttl", line)
		}
	}

	advancePubSubClock(t, h, 25*time.Hour)
	got := listedSubscriptions(t, h, c)
	for _, id := range ids {
		if !got[name(id)] {
			t.Errorf("25h on, %s (updated past its 1-day ttl) is gone", id)
		}
	}
	advancePubSubClock(t, h, 48*time.Hour)
	got = listedSubscriptions(t, h, c)
	for id, want := range map[string]bool{"three": false, "rest": false, "never": true} {
		if got[name(id)] != want {
			t.Errorf("73h on, %s is listed = %v, want %v", id, got[name(id)], want)
		}
	}
}
