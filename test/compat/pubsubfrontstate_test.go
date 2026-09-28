//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// pubsubPod is what the test reads of the Pub/Sub pod: its node, and each
// container's ID, restarts and readiness.
type pubsubPod struct {
	name, node string
	containers map[string]podContainer
}

type podContainer struct {
	id       string
	restarts int
	ready    bool
}

func readPubSubPod(t *testing.T, ctx context.Context, kc func(context.Context, ...string) ([]byte, error)) pubsubPod {
	t.Helper()
	out, err := kc(ctx, "get", "pod", "-l", "app=pubsub", "-o", "json")
	if err != nil {
		t.Fatalf("kubectl get pod -l app=pubsub: %v\n%s", err, out)
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name              string  `json:"name"`
				DeletionTimestamp *string `json:"deletionTimestamp"`
			} `json:"metadata"`
			Spec struct {
				NodeName string `json:"nodeName"`
			} `json:"spec"`
			Status struct {
				ContainerStatuses []struct {
					Name         string `json:"name"`
					ContainerID  string `json:"containerID"`
					RestartCount int    `json:"restartCount"`
					Ready        bool   `json:"ready"`
				} `json:"containerStatuses"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		t.Fatalf("read the pod list: %v", err)
	}
	for _, it := range list.Items {
		if it.Metadata.DeletionTimestamp != nil {
			continue
		}
		p := pubsubPod{name: it.Metadata.Name, node: it.Spec.NodeName, containers: map[string]podContainer{}}
		for _, c := range it.Status.ContainerStatuses {
			_, id, _ := strings.Cut(c.ContainerID, "://")
			p.containers[c.Name] = podContainer{id: id, restarts: c.RestartCount, ready: c.Ready}
		}
		return p
	}
	t.Fatalf("no running Pub/Sub pod:\n%s", out)
	return pubsubPod{}
}

// restartPubSubFront stops the front's container alone, as a crash would,
// through the container runtime of the kind node the pod runs on (the
// front's image has no shell to be told to exit), and waits for the kubelet
// to start it again. The emulator's container is not touched.
func restartPubSubFront(t *testing.T) {
	t.Helper()
	kubeconfig := strings.TrimSpace(os.Getenv(envKubeconfig))
	if kubeconfig == "" {
		t.Skipf("%s is not set; the front's container is restarted through the cluster", envKubeconfig)
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is not on PATH; the front's container is stopped through the kind node")
	}
	ns := strings.TrimSpace(os.Getenv("CLOUDBURROW_TEST_NAMESPACE"))
	if ns == "" {
		ns = "cloudburrow"
	}
	kc := func(ctx context.Context, args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, "kubectl", append([]string{"--kubeconfig", kubeconfig, "-n", ns}, args...)...).CombinedOutput()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	before := readPubSubPod(t, ctx, kc)
	front, emulator := before.containers["front"], before.containers["pubsub"]
	if front.id == "" || emulator.id == "" {
		t.Fatalf("the Pub/Sub pod %s has containers %v; want front and pubsub", before.name, before.containers)
	}
	if out, err := exec.CommandContext(ctx, "docker", "exec", before.node, "crictl", "stop", front.id).CombinedOutput(); err != nil {
		t.Fatalf("crictl stop the front on %s: %v\n%s", before.node, err, out)
	}
	for {
		now := readPubSubPod(t, ctx, kc)
		f, e := now.containers["front"], now.containers["pubsub"]
		if now.name != before.name {
			t.Fatalf("the pod was replaced (%s, then %s); only the front's container was to restart", before.name, now.name)
		}
		if e.restarts != emulator.restarts || e.id != emulator.id {
			t.Fatalf("the emulator's container restarted too: %+v, then %+v", emulator, e)
		}
		if f.restarts > front.restarts && f.ready {
			t.Logf("the front restarted alone: container %s, restarts %d", f.id, f.restarts)
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("the front did not come back ready: %+v", f)
		case <-time.After(time.Second):
		}
	}
}

// TestPubSubFrontRestartKeepsItsState (#898): the front keeps what the
// emulator does not store on the pod's emptyDir, so a restart of the front's
// container alone, the emulator still running, loses none of it. Through
// the official client: a subscription whose 1-day ttl was updated to 2 days
// still reads 2 days after the restart, and is kept 25 hours on and gone once
// idle for 2 days; one created with a 1-day ttl and left alone since before
// the restart is gone 25 hours on, where a front that forgot it would never
// expire it; the clock has not gone back.
// covers: google.pubsub.v1.Subscriber/UpdateSubscription, google.pubsub.v1.Subscriber/GetSubscription, google.pubsub.v1.Subscriber/ListSubscriptions
func TestPubSubFrontRestartKeepsItsState(t *testing.T) {
	h := New(t)
	c := pubsubClient(t, h)
	ctx := h.Context()
	topicName := topic(t, h, c, "front-restart-topic")
	name := func(id string) string {
		return fmt.Sprintf("projects/%s/subscriptions/front-restart-%s", h.Project(), id)
	}
	for _, id := range []string{"updated", "idle"} {
		n := name(id)
		if _, err := c.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{Name: n, Topic: topicName,
			ExpirationPolicy: &pubsubpb.ExpirationPolicy{Ttl: days(1)}, MessageRetentionDuration: days(1)}); err != nil {
			t.Fatalf("CreateSubscription %s: %v", n, err)
		}
		t.Cleanup(func() {
			_ = c.SubscriptionAdminClient.DeleteSubscription(context.Background(), &pubsubpb.DeleteSubscriptionRequest{Subscription: n})
		})
	}
	if _, err := c.SubscriptionAdminClient.UpdateSubscription(ctx, &pubsubpb.UpdateSubscriptionRequest{
		Subscription: &pubsubpb.Subscription{Name: name("updated"), ExpirationPolicy: &pubsubpb.ExpirationPolicy{Ttl: days(2)}},
		UpdateMask:   &fieldmaskpb.FieldMask{Paths: []string{"expiration_policy"}}}); err != nil {
		t.Fatalf("UpdateSubscription: %v", err)
	}
	before := time.Now()
	advancePubSubClock(t, h, 20*time.Hour)

	restartPubSubFront(t)

	after := pubsubClient(t, h)
	var s *pubsubpb.Subscription
	var err error
	for deadline := time.Now().Add(time.Minute); ; {
		// The tunnel's connection to the old container is gone; the first
		// calls may be refused while it reconnects.
		s, err = after.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: name("updated")})
		if status.Code(err) != codes.Unavailable || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Second)
	}
	if err != nil || s.GetExpirationPolicy().GetTtl().AsDuration() != 48*time.Hour {
		t.Fatalf("after the front restarted, the updated subscription reads %v, %v; want its 2-day ttl, not the emulator's 1 day",
			s.GetExpirationPolicy(), err)
	}
	if !listedSubscriptions(t, h, after)[name("idle")] {
		t.Fatal("the untouched subscription is gone right after the restart")
	}
	// The clock kept its 20 hours: 5 more is 25 for the untouched one.
	advancePubSubClock(t, h, 5*time.Hour)
	got := listedSubscriptions(t, h, after)
	if got[name("idle")] || !got[name("updated")] {
		t.Errorf("25h on, across the restart, idle listed = %v (want false: a 1-day ttl), updated listed = %v (want true: 2 days)",
			got[name("idle")], got[name("updated")])
	}
	// The read after the restart, at 20h, was activity: 2 days idle is 68h.
	advancePubSubClock(t, h, 44*time.Hour)
	if listedSubscriptions(t, h, after)[name("updated")] {
		t.Error("69h on, idle for 49h, the subscription updated to a 2-day ttl is still listed")
	}
	t.Logf("restart and checks took %s", time.Since(before).Round(time.Second))
}

// TestPubSubSeedExpirationAndRetention (#899): a seed file's subscription
// takes an expirationPolicy, a messageRetentionDuration and
// retainAckedMessages, which the official client reads back; {} is a policy
// without a ttl. A ttl Google refuses (12h, or 1 day under the 7-day default
// retention) is a 400 before anything is seeded. The seeded ttl is enforced
// through the front's clock: the 1-day one is gone 25 hours on, and the one
// that never expires is kept.
// covers: google.pubsub.v1.Subscriber/CreateSubscription, google.pubsub.v1.Subscriber/GetSubscription
func TestPubSubSeedExpirationAndRetention(t *testing.T) {
	h := New(t)
	control := h.Endpoint(EnvControl)
	project := h.Project()
	c := pubsubClient(t, h)
	ctx := h.Context()
	t.Cleanup(func() { adminReset(t, control, "service=pubsub&project="+project) })
	sub := func(id string) string { return "projects/" + project + "/subscriptions/seed-exp-" + id }
	topicName := "projects/" + project + "/topics/seed-exp"

	for why, fields := range map[string]string{
		"a ttl of 12h": `"expirationPolicy": {"ttl": "43200s"}, "messageRetentionDuration": "3600s"`,
		"a 1-day ttl under the default 7-day retention": `"expirationPolicy": {"ttl": "86400s"}`,
	} {
		doc := fmt.Sprintf(`{"components": {"pubsub": {"topics": [{"name": %q}],
			"subscriptions": [{"name": %q, "topic": %q, %s}]}}}`, topicName, sub("refused"), topicName, fields)
		if code, body := adminSeed(t, control, doc); code != http.StatusBadRequest || !strings.Contains(body, "expirationPolicy") {
			t.Errorf("%s: seed = %d %s; want 400 naming expirationPolicy", why, code, body)
		}
	}
	if _, err := c.TopicAdminClient.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: topicName}); status.Code(err) != codes.NotFound {
		t.Errorf("a refused seed created its topic: %v", err)
	}

	doc := fmt.Sprintf(`{"components": {"pubsub": {"topics": [{"name": %[1]q}], "subscriptions": [
		{"name": %[2]q, "topic": %[1]q, "expirationPolicy": {"ttl": "86400s"}, "messageRetentionDuration": "3600s", "retainAckedMessages": true},
		{"name": %[3]q, "topic": %[1]q, "expirationPolicy": {}}]}}}`, topicName, sub("day"), sub("never"))
	if code, body := adminSeed(t, control, doc); code != http.StatusOK {
		t.Fatalf("seed: %d %s", code, body)
	}
	day, err := c.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: sub("day")})
	if err != nil || day.GetExpirationPolicy().GetTtl().AsDuration() != 24*time.Hour ||
		day.GetMessageRetentionDuration().AsDuration() != time.Hour || !day.GetRetainAckedMessages() {
		t.Fatalf("the seeded subscription reads %v, %v; want a 1-day ttl, a 1-hour retention and acked messages kept", day, err)
	}
	never, err := c.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: sub("never")})
	if err != nil || never.GetExpirationPolicy() == nil || never.GetExpirationPolicy().GetTtl() != nil {
		t.Fatalf("the never-expiring subscription reads %v, %v", never.GetExpirationPolicy(), err)
	}

	advancePubSubClock(t, h, 25*time.Hour)
	got := listedSubscriptions(t, h, c)
	if got[sub("day")] || !got[sub("never")] {
		t.Errorf("25h after seeding, day listed = %v (want false), never listed = %v (want true)", got[sub("day")], got[sub("never")])
	}
}

// TestTerraformPubSubPushAndExpiration (#928): a google_pubsub_subscription
// with push_config.push_endpoint and expiration_policy.ttl applies through
// `cloudburrow terraform` and CloudBurrow's Pub/Sub front, plans clean (the
// provider reads back the real endpoint, never the push relay's, and the
// ttl). A change to expiration_policy.ttl is applied by the front: the
// provider's PATCH names ?updateMask=expirationPolicy,bigqueryConfig in the
// URL, and the front reads it, keeps the policy and leaves out the
// bigqueryConfig that sets nothing, so the emulator is sent nothing. A change
// to push_endpoint reaches the emulator as the relay's, and reads back as
// given. Each change plans clean after, and the official gRPC client reads
// each step.
// covers: google.pubsub.v1.Subscriber/CreateSubscription, google.pubsub.v1.Subscriber/GetSubscription, google.pubsub.v1.Subscriber/UpdateSubscription, google.pubsub.v1.Subscriber/DeleteSubscription
func TestTerraformPubSubPushAndExpiration(t *testing.T) {
	h := New(t)
	m := newTFModule(t)
	ps := pubsubClient(t, h)
	ctx := h.Context()
	project := h.Project()
	sub := "projects/" + project + "/subscriptions/tf-push-exp"
	const endpoint, moved = "http://tf-push.example.test/push", "http://tf-push.example.test/moved"

	module := func(endpoint, ttl string) string {
		return fmt.Sprintf(`
resource "google_pubsub_topic" "t" {
  project = %[1]q
  name    = "tf-push-exp"
}
resource "google_pubsub_subscription" "s" {
  project                    = %[1]q
  name                       = "tf-push-exp"
  topic                      = google_pubsub_topic.t.id
  message_retention_duration = "86400s"
  push_config {
    push_endpoint = %[2]q
  }
  expiration_policy {
    ttl = %[3]q
  }
}
`, project, endpoint, ttl)
	}
	check := func(when, endpoint string, ttl time.Duration) {
		t.Helper()
		s, err := ps.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: sub})
		if err != nil {
			t.Fatalf("%s, GetSubscription: %v", when, err)
		}
		if s.GetPushConfig().GetPushEndpoint() != endpoint || s.GetExpirationPolicy().GetTtl().AsDuration() != ttl ||
			s.GetMessageRetentionDuration().AsDuration() != 24*time.Hour {
			t.Errorf("%s, the subscription reads endpoint %q, policy %v, retention %v; want %q and %s",
				when, s.GetPushConfig().GetPushEndpoint(), s.GetExpirationPolicy(), s.GetMessageRetentionDuration(), endpoint, ttl)
		}
	}

	m.write(module(endpoint, "172800s"))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	check("after apply", endpoint, 48*time.Hour)
	m.planClean()
	state := m.must("state", "show", "-no-color", "google_pubsub_subscription.s")
	// The relay's endpoint is on the pod's loopback, port 8087.
	if !strings.Contains(state, endpoint) || strings.Contains(state, "127.0.0.1:8087") {
		t.Errorf("terraform's state names another endpoint than %q:\n%s", endpoint, state)
	}

	m.write(module(endpoint, "259200s"))
	if out, err := m.tf("apply", "-auto-approve", "-input=false", "-no-color"); err != nil {
		t.Fatalf("a change to expiration_policy.ttl failed: %v\n%s", err, lastLines(out, 30))
	}
	check("after the ttl change", endpoint, 72*time.Hour)
	m.planClean()

	// A new endpoint goes through the relay too, and reads back as given.
	m.write(module(moved, "259200s"))
	if out, err := m.tf("apply", "-auto-approve", "-input=false", "-no-color"); err != nil {
		t.Fatalf("a change to push_config.push_endpoint failed: %v\n%s", err, lastLines(out, 30))
	}
	check("after the endpoint change", moved, 72*time.Hour)
	m.planClean()

	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := ps.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: sub}); status.Code(err) != codes.NotFound {
		t.Errorf("GetSubscription after destroy: %v; want NotFound", err)
	}
}
