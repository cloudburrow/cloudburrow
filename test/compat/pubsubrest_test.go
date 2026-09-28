//go:build compat

package compat

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// The REST half of #880 (#908): gcloud and Terraform reach Pub/Sub over its
// REST API, and get what the gRPC clients get.

// pushBucket makes a bucket on the builtin storage server whose upload
// endpoint, in the cluster, accepts pushes, and returns its name and an
// upload URL for an object in it.
func pushBucket(t *testing.T, h *Harness, suffix string) (bucket string, upload func(object string) string) {
	t.Helper()
	h.Endpoint(EnvStorage)
	sc := storageClient(t, h)
	bucket = h.Project() + "-" + suffix
	if err := sc.Bucket(bucket).Create(h.Context(), h.Project(), nil); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	t.Cleanup(func() {
		it := sc.Bucket(bucket).Objects(context.Background(), nil)
		for o, err := it.Next(); err == nil; o, err = it.Next() {
			_ = sc.Bucket(bucket).Object(o.Name).Delete(context.Background())
		}
		_ = sc.Bucket(bucket).Delete(context.Background())
	})
	return bucket, func(object string) string {
		return "http://storage:4443/upload/storage/v1/b/" + bucket + "/o?uploadType=media&name=" + object
	}
}

// waitForPush waits for a push of data to be stored as object.
func waitForPush(t *testing.T, h *Harness, bucket, object, data string) {
	t.Helper()
	sc := storageClient(t, h)
	want := base64.StdEncoding.EncodeToString([]byte(data))
	for start := time.Now(); time.Since(start) < 60*time.Second; time.Sleep(time.Second) {
		if strings.Contains(storageObjectText(h.Context(), sc, bucket, object), want) {
			return
		}
	}
	t.Fatalf("the push of %q did not reach %s within 60s", data, object)
}

// TestGcloudPubSubPushSubscription (#908): push subscriptions made by the
// real gcloud, which speaks REST, are relayed as the official gRPC client's
// are. Three subscriptions push to the builtin storage server's upload
// endpoint, which accepts every push: one created with --push-endpoint
// (PUT), one created as pull and given the endpoint by `subscriptions
// update --push-endpoint` (PATCH) and one by `modify-push-config`
// (:modifyPushConfig). A fourth pushes to an upload into a bucket that does
// not exist, which refuses every push. With a one-day ttl, the three whose
// pushes succeed are kept 25h on, and the fourth has expired. `describe`,
// `list` and the gRPC client all read the real endpoint, never the relay's.
// covers: google.pubsub.v1.Subscriber/CreateSubscription, google.pubsub.v1.Subscriber/GetSubscription, google.pubsub.v1.Subscriber/ListSubscriptions, google.pubsub.v1.Subscriber/UpdateSubscription, google.pubsub.v1.Subscriber/ModifyPushConfig
func TestGcloudPubSubPushSubscription(t *testing.T) {
	h := New(t)
	c := pubsubClient(t, h)
	g := newGcloudSession(t, h)
	ctx := h.Context()
	p := "--project=" + h.Project()
	bucket, upload := pushBucket(t, h, "gcloud-push")
	topicID := "gcloud-push-topic"
	name := func(id string) string { return "projects/" + h.Project() + "/subscriptions/gcloud-push-" + id }
	endpoints := map[string]string{
		"created": upload("created"), "updated": upload("updated"), "modified": upload("modified"),
		"refused": "http://storage:4443/upload/storage/v1/b/" + h.Project() + "-absent/o?uploadType=media&name=x",
	}
	t.Cleanup(func() {
		for id := range endpoints {
			_ = c.SubscriptionAdminClient.DeleteSubscription(context.Background(), &pubsubpb.DeleteSubscriptionRequest{Subscription: name(id)})
		}
		_ = c.TopicAdminClient.DeleteTopic(context.Background(), &pubsubpb.DeleteTopicRequest{Topic: "projects/" + h.Project() + "/topics/" + topicID})
	})
	g.must("pubsub", "topics", "create", topicID, p)
	oneDay := []string{"--topic=" + topicID, "--expiration-period=1d", "--message-retention-duration=1d"}
	for _, id := range []string{"created", "refused"} {
		g.must(append([]string{"pubsub", "subscriptions", "create", "gcloud-push-" + id, p, "--push-endpoint=" + endpoints[id]}, oneDay...)...)
	}
	for _, id := range []string{"updated", "modified"} {
		g.must(append([]string{"pubsub", "subscriptions", "create", "gcloud-push-" + id, p}, oneDay...)...)
	}
	g.must("pubsub", "subscriptions", "update", "gcloud-push-updated", p, "--push-endpoint="+endpoints["updated"])
	g.must("pubsub", "subscriptions", "modify-push-config", "gcloud-push-modified", p, "--push-endpoint="+endpoints["modified"])

	for id, ep := range endpoints {
		if got := strings.TrimSpace(g.must("pubsub", "subscriptions", "describe", "gcloud-push-"+id, p,
			"--format=value(pushConfig.pushEndpoint)")); got != ep {
			t.Errorf("gcloud describes %s's endpoint as %q, want %q", id, got, ep)
		}
		s, err := c.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: name(id)})
		if err != nil || s.GetPushConfig().GetPushEndpoint() != ep {
			t.Errorf("over gRPC, %s reads %v, %v; want %q", id, s.GetPushConfig(), err, ep)
		}
	}
	listed := g.must("pubsub", "subscriptions", "list", p, "--format=value(pushConfig.pushEndpoint)")
	if strings.Contains(listed, "/push/") || !strings.Contains(listed, endpoints["created"]) {
		t.Errorf("gcloud lists the endpoints as\n%s\nwant the real ones", listed)
	}

	publish := func(data string) {
		t.Helper()
		g.must("pubsub", "topics", "publish", topicID, p, "--message="+data)
		for _, id := range []string{"created", "updated", "modified"} {
			waitForPush(t, h, bucket, id, data)
		}
	}
	publish("first")
	advancePubSubClock(t, h, 20*time.Hour)
	publish("second")
	advancePubSubClock(t, h, 5*time.Hour)
	got := listedSubscriptions(t, h, c)
	for id, want := range map[string]bool{"created": true, "updated": true, "modified": true, "refused": false} {
		if got[name(id)] != want {
			t.Errorf("25h on a one-day ttl, %s is listed = %v, want %v", id, got[name(id)], want)
		}
	}
}

// TestGcloudPubSubRefusesExactlyOnceWithPush (#908): gcloud, over REST, is
// refused what the gRPC client is: `subscriptions create` with
// --enable-exactly-once-delivery and --push-endpoint, `update
// --push-endpoint` of an exactly-once subscription, `update
// --enable-exactly-once-delivery` of a push subscription and
// `modify-push-config` of an exactly-once subscription. Each change nothing.
// covers: google.pubsub.v1.Subscriber/CreateSubscription, google.pubsub.v1.Subscriber/UpdateSubscription, google.pubsub.v1.Subscriber/ModifyPushConfig
func TestGcloudPubSubRefusesExactlyOnceWithPush(t *testing.T) {
	h := New(t)
	c := pubsubClient(t, h)
	g := newGcloudSession(t, h)
	ctx := h.Context()
	p := "--project=" + h.Project()
	topicID := "gcloud-eod-topic"
	name := func(id string) string { return "projects/" + h.Project() + "/subscriptions/gcloud-eod-" + id }
	t.Cleanup(func() {
		for _, id := range []string{"both", "pull", "pushed"} {
			_ = c.SubscriptionAdminClient.DeleteSubscription(context.Background(), &pubsubpb.DeleteSubscriptionRequest{Subscription: name(id)})
		}
		_ = c.TopicAdminClient.DeleteTopic(context.Background(), &pubsubpb.DeleteTopicRequest{Topic: "projects/" + h.Project() + "/topics/" + topicID})
	})
	const push = "--push-endpoint=http://127.0.0.1:9/push"
	g.must("pubsub", "topics", "create", topicID, p)
	refused := func(what string, args ...string) {
		t.Helper()
		out, err := g.run(nil, append([]string{"pubsub", "subscriptions"}, append(args, p)...)...)
		if err == nil || !strings.Contains(out, "exactly-once delivery is supported only for pull subscriptions") {
			t.Errorf("%s = %v:\n%s\nwant the refusal", what, err, out)
		}
	}
	refused("create with exactly-once and a push endpoint", "create", "gcloud-eod-both", "--topic="+topicID,
		"--enable-exactly-once-delivery", push)
	if _, err := c.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: name("both")}); status.Code(err) != codes.NotFound {
		t.Errorf("refused, and then reads %v", err)
	}
	g.must("pubsub", "subscriptions", "create", "gcloud-eod-pull", p, "--topic="+topicID, "--enable-exactly-once-delivery")
	g.must("pubsub", "subscriptions", "create", "gcloud-eod-pushed", p, "--topic="+topicID, push)
	refused("update --push-endpoint of an exactly-once subscription", "update", "gcloud-eod-pull", push)
	refused("modify-push-config of an exactly-once subscription", "modify-push-config", "gcloud-eod-pull", push)
	refused("update --enable-exactly-once-delivery of a push subscription", "update", "gcloud-eod-pushed", "--enable-exactly-once-delivery")
	for id, eod := range map[string]bool{"pull": true, "pushed": false} {
		s, err := c.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: name(id)})
		if err != nil || s.GetEnableExactlyOnceDelivery() != eod || (s.GetPushConfig().GetPushEndpoint() == "") != eod {
			t.Errorf("%s after the refusals: %v, %v", id, s, err)
		}
	}
}

// TestPubSubRESTPushAndExactlyOnce (#908): over plain REST, on the Pub/Sub
// port, a push endpoint given by PUT, PATCH or :modifyPushConfig reads back
// as given from PUT, GET, PATCH and the project's list, and so does one set
// over gRPC; the emulator is never seen to hold anything else. Exactly-once
// with a push endpoint or a BigQuery export is refused 400
// INVALID_ARGUMENT on PUT, PATCH and :modifyPushConfig.
// covers: google.pubsub.v1.Subscriber/CreateSubscription, google.pubsub.v1.Subscriber/GetSubscription, google.pubsub.v1.Subscriber/ListSubscriptions, google.pubsub.v1.Subscriber/UpdateSubscription, google.pubsub.v1.Subscriber/ModifyPushConfig
func TestPubSubRESTPushAndExactlyOnce(t *testing.T) {
	h := New(t)
	c := pubsubClient(t, h)
	ctx := h.Context()
	p := "/v1/projects/" + h.Project()
	topicName := "projects/" + h.Project() + "/topics/rest-push-topic"
	if code, body := pubsubREST(t, h, http.MethodPut, p+"/topics/rest-push-topic", ""); code != http.StatusOK {
		t.Fatalf("PUT topic = %d %v", code, body)
	}
	name := func(id string) string { return "projects/" + h.Project() + "/subscriptions/rest-push-" + id }
	path := func(id string) string { return "/v1/" + name(id) }
	t.Cleanup(func() {
		for _, id := range []string{"put", "patched", "modified", "grpc", "eod", "both"} {
			_ = c.SubscriptionAdminClient.DeleteSubscription(context.Background(), &pubsubpb.DeleteSubscriptionRequest{Subscription: name(id)})
		}
		_ = c.TopicAdminClient.DeleteTopic(context.Background(), &pubsubpb.DeleteTopicRequest{Topic: topicName})
	})
	ep := func(id string) string { return "http://worker.example/push/" + id + "?token=a&b=c" }
	endpointOf := func(body map[string]any) string {
		pc, _ := body["pushConfig"].(map[string]any)
		s, _ := pc["pushEndpoint"].(string)
		return s
	}

	code, body := pubsubREST(t, h, http.MethodPut, path("put"), `{"topic":"`+topicName+`","ackDeadlineSeconds":33,"pushConfig":{"pushEndpoint":"`+ep("put")+`","attributes":{"x-goog-version":"v1"}}}`)
	if code != http.StatusOK || endpointOf(body) != ep("put") || body["ackDeadlineSeconds"] != float64(33) {
		t.Errorf("PUT = %d %v, want the endpoint and every other field as given", code, body)
	}
	for _, id := range []string{"patched", "modified"} {
		if code, body := pubsubREST(t, h, http.MethodPut, path(id), `{"topic":"`+topicName+`"}`); code != http.StatusOK {
			t.Fatalf("PUT %s = %d %v", id, code, body)
		}
	}
	code, body = pubsubREST(t, h, http.MethodPatch, path("patched"), `{"subscription":{"pushConfig":{"pushEndpoint":"`+ep("patched")+`"}},"updateMask":"pushConfig"}`)
	if code != http.StatusOK || endpointOf(body) != ep("patched") {
		t.Errorf("PATCH = %d %v", code, body)
	}
	if code, body := pubsubREST(t, h, http.MethodPost, path("modified")+":modifyPushConfig", `{"pushConfig":{"pushEndpoint":"`+ep("modified")+`"}}`); code != http.StatusOK {
		t.Errorf(":modifyPushConfig = %d %v", code, body)
	}
	if _, err := c.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{Name: name("grpc"), Topic: topicName,
		PushConfig: &pubsubpb.PushConfig{PushEndpoint: ep("grpc")}}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"put", "patched", "modified", "grpc"} {
		if code, body := pubsubREST(t, h, http.MethodGet, path(id), ""); code != http.StatusOK || endpointOf(body) != ep(id) {
			t.Errorf("GET %s = %d %v, want %s", id, code, body, ep(id))
		}
		if s, err := c.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: name(id)}); err != nil ||
			s.GetPushConfig().GetPushEndpoint() != ep(id) {
			t.Errorf("over gRPC, %s reads %v, %v", id, s.GetPushConfig(), err)
		}
	}
	code, body = pubsubREST(t, h, http.MethodGet, p+"/subscriptions?pageSize=100", "")
	subs, _ := body["subscriptions"].([]any)
	seen := 0
	for _, s := range subs {
		m, _ := s.(map[string]any)
		if strings.Contains(endpointOf(m), "/push/") && !strings.HasPrefix(endpointOf(m), "http://worker.example/") {
			t.Errorf("the list shows the relay: %v", m)
		}
		if strings.HasPrefix(endpointOf(m), "http://worker.example/") {
			seen++
		}
	}
	if code != http.StatusOK || seen != 4 {
		t.Errorf("the list = %d, %d real endpoints of 4: %v", code, seen, body)
	}

	// Exactly-once with push or export.
	if code, body := pubsubREST(t, h, http.MethodPut, path("eod"), `{"topic":"`+topicName+`","enableExactlyOnceDelivery":true}`); code != http.StatusOK {
		t.Fatalf("PUT exactly-once = %d %v", code, body)
	}
	for _, r := range []struct{ what, method, path, body string }{
		{"PUT with a push endpoint", http.MethodPut, path("both"), `{"topic":"` + topicName + `","enableExactlyOnceDelivery":true,"pushConfig":{"pushEndpoint":"` + ep("both") + `"}}`},
		{"PUT with a BigQuery export", http.MethodPut, path("both"), `{"topic":"` + topicName + `","enableExactlyOnceDelivery":true,"bigqueryConfig":{"table":"p.d.t"}}`},
		{"PATCH a push endpoint onto exactly-once", http.MethodPatch, path("eod"), `{"subscription":{"pushConfig":{"pushEndpoint":"` + ep("eod") + `"}},"updateMask":"pushConfig"}`},
		{"PATCH exactly-once onto push", http.MethodPatch, path("put"), `{"subscription":{"enableExactlyOnceDelivery":true},"updateMask":"enableExactlyOnceDelivery"}`},
		{":modifyPushConfig of exactly-once", http.MethodPost, path("eod") + ":modifyPushConfig", `{"pushConfig":{"pushEndpoint":"` + ep("eod") + `"}}`},
	} {
		code, body := pubsubREST(t, h, r.method, r.path, r.body)
		if e, _ := body["error"].(map[string]any); code != http.StatusBadRequest || e["status"] != "INVALID_ARGUMENT" {
			t.Errorf("%s = %d %v, want 400 INVALID_ARGUMENT", r.what, code, body)
		}
	}
	if code, _ := pubsubREST(t, h, http.MethodGet, path("both"), ""); code != http.StatusNotFound {
		t.Errorf("a refused subscription reads %d, want 404", code)
	}
	for id, eod := range map[string]bool{"eod": true, "put": false} {
		s, err := c.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: name(id)})
		if err != nil || s.GetEnableExactlyOnceDelivery() != eod || (s.GetPushConfig().GetPushEndpoint() == "") != eod {
			t.Errorf("%s after the refusals: %v, %v", id, s, err)
		}
	}
}

// TestGcloudPubSubStateSave (#908): a project only gcloud has used, over
// REST, is one `cloudburrow state save` captures. gcloud makes a topic, a
// push subscription and a pull subscription in it; after a save, a reset
// and a load, gcloud describes each as it was, the push endpoint the real
// one. Nothing names the project over gRPC before the save.
// covers: google.pubsub.v1.Subscriber/GetSubscription
func TestGcloudPubSubStateSave(t *testing.T) {
	h := New(t)
	// Skips where the shard runs no Pub/Sub: gcloud would otherwise use its
	// default endpoint, pubsub.googleapis.com.
	h.Endpoint(EnvPubSub)
	cli := os.Getenv(EnvCLI)
	if cli == "" {
		t.Skipf("%s is not set", EnvCLI)
	}
	g := newGcloudSession(t, h)
	flags := strings.Fields(os.Getenv(EnvCLIArgs))
	run := func(args ...string) string {
		t.Helper()
		out, err := exec.Command(cli, append(args, flags...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("cloudburrow %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return string(out)
	}
	control := func() string {
		out, _ := exec.Command(cli, append([]string{"status", "--format", "json"}, flags...)...).Output()
		var st struct {
			ControlURL string `json:"control_url"`
		}
		_ = json.Unmarshal(out, &st)
		return strings.TrimPrefix(st.ControlURL, "http://")
	}
	t.Cleanup(func() {
		if ctl := control(); ctl != "" {
			adminReset(t, ctl, "service=pubsub&project="+h.Project())
		}
	})
	p := "--project=" + h.Project()
	const endpoint = "http://worker.example/push?token=abc"
	g.must("pubsub", "topics", "create", "gcloud-state-topic", p)
	g.must("pubsub", "subscriptions", "create", "gcloud-state-push", p, "--topic=gcloud-state-topic",
		"--push-endpoint="+endpoint, "--ack-deadline=30", "--expiration-period=2d", "--message-retention-duration=1d")
	g.must("pubsub", "subscriptions", "create", "gcloud-state-pull", p, "--topic=gcloud-state-topic",
		"--enable-exactly-once-delivery")
	const format = "--format=json(name,topic,pushConfig,ackDeadlineSeconds,expirationPolicy,messageRetentionDuration,enableExactlyOnceDelivery)"
	describe := func(id string) string {
		return g.must("pubsub", "subscriptions", "describe", id, p, format)
	}
	before := map[string]string{"gcloud-state-push": describe("gcloud-state-push"), "gcloud-state-pull": describe("gcloud-state-pull")}
	if !strings.Contains(before["gcloud-state-push"], endpoint) {
		t.Fatalf("gcloud describes the push subscription as %s", before["gcloud-state-push"])
	}

	file := filepath.Join(t.TempDir(), "state.tar.gz")
	if saved := run("state", "save", file); !strings.Contains(saved, "captured:     pubsub") {
		t.Fatalf("Pub/Sub was not captured:\n%s", saved)
	}
	if code, body := adminReset(t, control(), "service=pubsub&project="+h.Project()); code != http.StatusOK {
		t.Fatalf("reset: %d %s", code, body)
	}
	if out, err := g.run(nil, "pubsub", "subscriptions", "describe", "gcloud-state-push", p); err == nil {
		t.Fatalf("the subscription survived the reset; the test would prove nothing:\n%s", out)
	}
	if loaded := run("state", "load", file); !strings.Contains(loaded, "pubsub") {
		t.Fatalf("state load output:\n%s", loaded)
	}
	for id, want := range before {
		if got := describe(id); got != want {
			t.Errorf("after the load, gcloud describes %s as\n%s\nwant\n%s", id, got, want)
		}
	}
	if got := g.must("pubsub", "topics", "list", p, "--format=value(name)"); !strings.Contains(got, "topics/gcloud-state-topic") {
		t.Errorf("after the load, the topics are %s", got)
	}
}
