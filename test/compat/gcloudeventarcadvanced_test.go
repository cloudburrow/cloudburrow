//go:build compat

package compat

import (
	"context"
	"strings"
	"testing"
	"time"

	eventarc "cloud.google.com/go/eventarc/apiv1"
	eventarcpb "cloud.google.com/go/eventarc/apiv1/eventarcpb"
	"cloud.google.com/go/pubsub/v2"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestGcloudEventarcAdvanced (#1947), gcloud through the harness, as
// Google's "Publish and receive events by creating a bus and enrollment
// (gcloud CLI)" quickstart documents it: eventarc message-buses create
// --logging-config, pipelines create --destinations=pubsub_topic= with
// --input-payload-format-json and a transformation, enrollments create
// --cel-match, google-api-sources create, message-buses describe, list,
// list-enrollments and update; message-buses publish (--json-message and
// --event-*) delivers the matching events to the topic, which Pub/Sub
// reads; channels create and publish; and every delete.
func TestGcloudEventarcAdvanced(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvRun)
	pubsubAddr := runShardEndpoint(h, "CLOUDBURROW_TEST_RUN_PUBSUB", EnvPubSub)
	g := newGcloudSession(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	ec, err := eventarc.NewClient(ctx, runClientOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ec.Close() })
	t.Setenv("PUBSUB_EMULATOR_HOST", pubsubAddr)
	pc, err := pubsub.NewClient(ctx, g.project)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	parent := "projects/" + g.project + "/locations/us-central1"
	tn := "projects/" + g.project + "/topics/gcloud-adv"
	if _, err := pc.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: tn}); err != nil {
		t.Fatalf("CreateTopic: %v", err)
	}
	sub := "projects/" + g.project + "/subscriptions/gcloud-adv"
	if _, err := pc.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{Name: sub, Topic: tn}); err != nil {
		t.Fatalf("CreateSubscription: %v", err)
	}
	t.Cleanup(func() {
		_ = pc.SubscriptionAdminClient.DeleteSubscription(context.Background(), &pubsubpb.DeleteSubscriptionRequest{Subscription: sub})
		_ = pc.TopicAdminClient.DeleteTopic(context.Background(), &pubsubpb.DeleteTopicRequest{Topic: tn})
	})
	loc := "--location=us-central1"
	cleanup := func(args ...string) {
		t.Cleanup(func() { _, _ = g.run(nil, append(args, loc, "--quiet")...) })
	}

	g.must("eventarc", "message-buses", "create", "gcloud-bus", loc, "--logging-config=INFO")
	cleanup("eventarc", "message-buses", "delete", "gcloud-bus")
	g.must("eventarc", "pipelines", "create", "gcloud-pipe", loc, "--destinations=pubsub_topic=gcloud-adv",
		"--input-payload-format-json=", `--mediations=transformation_template=message.removeFields(["data.secret"])`,
		"--max-retry-attempts=3")
	cleanup("eventarc", "pipelines", "delete", "gcloud-pipe")
	g.must("eventarc", "enrollments", "create", "gcloud-enr", loc, "--message-bus=gcloud-bus", "--destination-pipeline=gcloud-pipe",
		`--cel-match=message.type == "com.example.gcloud.v1"`)
	cleanup("eventarc", "enrollments", "delete", "gcloud-enr")
	g.must("eventarc", "google-api-sources", "create", "gcloud-src", loc, "--destination-message-bus=gcloud-bus")
	cleanup("eventarc", "google-api-sources", "delete", "gcloud-src")

	if out := g.must("eventarc", "message-buses", "describe", "gcloud-bus", loc, "--format=value(loggingConfig.logSeverity)"); lastLine(out) != "INFO" {
		t.Errorf("message-buses describe = %q", out)
	}
	if out := g.must("eventarc", "message-buses", "list", loc, "--format=value(name)"); !strings.Contains(out, "gcloud-bus") {
		t.Errorf("message-buses list:\n%s", out)
	}
	if out := g.must("eventarc", "message-buses", "list-enrollments", "gcloud-bus", loc); !strings.Contains(out, "gcloud-enr") {
		t.Errorf("message-buses list-enrollments:\n%s", out)
	}
	g.must("eventarc", "message-buses", "update", "gcloud-bus", loc, "--logging-config=DEBUG")
	if b, err := ec.GetMessageBus(ctx, &eventarcpb.GetMessageBusRequest{Name: parent + "/messageBuses/gcloud-bus"}); err != nil ||
		b.GetLoggingConfig().GetLogSeverity() != eventarcpb.LoggingConfig_DEBUG {
		t.Errorf("after update the bus = %v, %v", b, err)
	}
	if p, err := ec.GetPipeline(ctx, &eventarcpb.GetPipelineRequest{Name: parent + "/pipelines/gcloud-pipe"}); err != nil ||
		p.GetDestinations()[0].GetTopic() != tn || p.GetInputPayloadFormat().GetJson() == nil || p.GetRetryPolicy().GetMaxAttempts() != 3 {
		t.Errorf("the pipeline = %v, %v", p, err)
	}
	if out := g.must("eventarc", "enrollments", "describe", "gcloud-enr", loc, "--format=value(celMatch)"); !strings.Contains(out, "com.example.gcloud.v1") {
		t.Errorf("enrollments describe = %q", out)
	}
	if out := g.must("eventarc", "pipelines", "list", loc, "--format=value(name)"); !strings.Contains(out, "gcloud-pipe") {
		t.Errorf("pipelines list:\n%s", out)
	}
	if out := g.must("eventarc", "google-api-sources", "describe", "gcloud-src", loc, "--format=value(destination)"); !strings.HasSuffix(lastLine(out), "/messageBuses/gcloud-bus") {
		t.Errorf("google-api-sources describe = %q", out)
	}

	g.must("eventarc", "message-buses", "publish", "gcloud-bus", loc,
		`--json-message={"specversion":"1.0","id":"gcloud-1","source":"//gcloud","type":"com.example.gcloud.v1","datacontenttype":"application/json","data":{"n":1,"secret":"s"}}`)
	g.must("eventarc", "message-buses", "publish", "gcloud-bus", loc, "--event-id=gcloud-2", "--event-source=//gcloud",
		"--event-type=com.example.gcloud.v1", `--event-data={"n":2,"secret":"s"}`, "--event-attributes=datacontenttype=application/json")
	g.must("eventarc", "message-buses", "publish", "gcloud-bus", loc, "--event-id=gcloud-3", "--event-source=//gcloud",
		"--event-type=com.example.other.v1", `--event-data={"n":3}`)
	got := map[string]string{}
	deadline := time.Now().Add(60 * time.Second)
	for len(got) < 2 && time.Now().Before(deadline) {
		resp, err := pc.SubscriptionAdminClient.Pull(ctx, &pubsubpb.PullRequest{Subscription: sub, MaxMessages: 10})
		if err != nil {
			t.Fatalf("Pull: %v", err)
		}
		for _, rm := range resp.GetReceivedMessages() {
			got[rm.GetMessage().GetAttributes()["ce-id"]] = string(rm.GetMessage().GetData())
			_ = pc.SubscriptionAdminClient.Acknowledge(ctx, &pubsubpb.AcknowledgeRequest{Subscription: sub, AckIds: []string{rm.GetAckId()}})
		}
	}
	for _, id := range []string{"gcloud-1", "gcloud-2"} {
		if !strings.Contains(got[id], `"n":`) || strings.Contains(got[id], "secret") {
			t.Errorf("%s on the topic = %q (all %v)", id, got[id], got)
		}
	}
	if _, ok := got["gcloud-3"]; ok {
		t.Error("gcloud-3, of another type, reached the topic")
	}

	// A channel, and a publish to it.
	g.must("eventarc", "channels", "create", "gcloud-chan", loc)
	cleanup("eventarc", "channels", "delete", "gcloud-chan")
	if out := g.must("eventarc", "channels", "describe", "gcloud-chan", loc, "--format=value(state)"); lastLine(out) != "ACTIVE" {
		t.Errorf("channels describe = %q", out)
	}
	g.must("eventarc", "channels", "publish", "gcloud-chan", loc, "--event-id=gcloud-c1", "--event-source=//gcloud",
		"--event-type=com.example.gcloud.v1", `--event-data={"n":1}`)

	for _, del := range [][]string{{"enrollments", "gcloud-enr"}, {"google-api-sources", "gcloud-src"}, {"pipelines", "gcloud-pipe"},
		{"message-buses", "gcloud-bus"}, {"channels", "gcloud-chan"}} {
		g.must("eventarc", del[0], "delete", del[1], loc, "--quiet")
	}
	if _, err := ec.GetMessageBus(ctx, &eventarcpb.GetMessageBusRequest{Name: parent + "/messageBuses/gcloud-bus"}); status.Code(err) != codes.NotFound {
		t.Errorf("the bus after delete: %v", err)
	}
}
