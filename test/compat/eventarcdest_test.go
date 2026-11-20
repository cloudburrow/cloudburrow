//go:build compat

package compat

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	eventarc "cloud.google.com/go/eventarc/apiv1"
	eventarcpb "cloud.google.com/go/eventarc/apiv1/eventarcpb"
	"cloud.google.com/go/pubsub/v2"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// covers: google.cloud.eventarc.v1.Eventarc/CreateTrigger, google.cloud.eventarc.v1.Eventarc/DeleteTrigger
//
// TestEventarcDestinations (#1877), with the official Eventarc and Pub/Sub
// clients: a messagePublished trigger whose destination is an internal
// HTTP endpoint (destination.httpEndpoint.uri with the network attachment
// Google requires) delivers a published message to the URI as a
// binary-mode CloudEvent; without the network attachment, or to a
// function that does not exist, CreateTrigger is INVALID_ARGUMENT. A GKE
// destination, a channel and the channel and channel
// connection methods are UNIMPLEMENTED with the reason; a workflow that is
// not deployed is refused (TestEventarcTriggerStartsAWorkflow delivers to
// one that is).
func TestEventarcDestinations(t *testing.T) {
	h := New(t)
	pubsubAddr := runShardEndpoint(h, "CLOUDBURROW_TEST_RUN_PUBSUB", EnvPubSub)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	t.Setenv("PUBSUB_EMULATOR_HOST", pubsubAddr)
	pc, err := pubsub.NewClient(ctx, h.Project())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	ec, err := eventarc.NewClient(ctx, runClientOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ec.Close() })
	parent := runParent(h)
	workflowCode := codes.FailedPrecondition
	if strings.TrimSpace(os.Getenv(EnvWorkflows)) != "" {
		workflowCode = codes.InvalidArgument
	}

	var mu sync.Mutex
	var got []*http.Request
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got, bodies = append(got, r), append(bodies, string(b))
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	typ := []*eventarcpb.EventFilter{{Attribute: "type", Value: "google.cloud.pubsub.topic.v1.messagePublished"}}
	attachment := "projects/" + h.Project() + "/regions/us-central1/networkAttachments/events"
	http1 := &eventarcpb.Destination{Descriptor_: &eventarcpb.Destination_HttpEndpoint{HttpEndpoint: &eventarcpb.HttpEndpoint{Uri: srv.URL + "/route"}},
		NetworkConfig: &eventarcpb.NetworkConfig{NetworkAttachment: attachment}}

	for name, c := range map[string]struct {
		tr   *eventarcpb.Trigger
		code codes.Code
	}{
		"an HTTP endpoint with no network attachment": {&eventarcpb.Trigger{EventFilters: typ, Destination: &eventarcpb.Destination{
			Descriptor_: &eventarcpb.Destination_HttpEndpoint{HttpEndpoint: &eventarcpb.HttpEndpoint{Uri: srv.URL}}}}, codes.InvalidArgument},
		"a missing function": {&eventarcpb.Trigger{EventFilters: typ, Destination: &eventarcpb.Destination{
			Descriptor_: &eventarcpb.Destination_CloudFunction{CloudFunction: parent + "/functions/no-such-function"}}}, codes.InvalidArgument},
		"a GKE service": {&eventarcpb.Trigger{EventFilters: typ, Destination: &eventarcpb.Destination{Descriptor_: &eventarcpb.Destination_Gke{
			Gke: &eventarcpb.GKE{Cluster: "c", Location: "us-central1", Namespace: "default", Service: "s"}}}}, codes.Unimplemented},
		// A workflow that is not deployed (#1986): INVALID_ARGUMENT where
		// the instance serves Workflows, FAILED_PRECONDITION where not.
		"a missing workflow": {&eventarcpb.Trigger{EventFilters: typ, Destination: &eventarcpb.Destination{
			Descriptor_: &eventarcpb.Destination_Workflow{Workflow: parent + "/workflows/no-such-workflow"}}}, workflowCode},
		"a channel": {&eventarcpb.Trigger{EventFilters: typ, Channel: parent + "/channels/c", Destination: http1}, codes.Unimplemented},
	} {
		_, err := ec.CreateTrigger(ctx, &eventarcpb.CreateTriggerRequest{Parent: parent, TriggerId: "bad", Trigger: c.tr})
		if status.Code(err) != c.code {
			t.Errorf("CreateTrigger with %s: %v; want %v", name, err, c.code)
		}
	}
	if _, err := ec.CreateChannel(ctx, &eventarcpb.CreateChannelRequest{Parent: parent, ChannelId: "c",
		Channel: &eventarcpb.Channel{Provider: parent + "/providers/datadog"}}); status.Code(err) != codes.Unimplemented ||
		!strings.Contains(err.Error(), "third-party provider") {
		t.Errorf("CreateChannel: %v; want UNIMPLEMENTED naming why", err)
	}
	if _, err := ec.CreateChannelConnection(ctx, &eventarcpb.CreateChannelConnectionRequest{Parent: parent, ChannelConnectionId: "c",
		ChannelConnection: &eventarcpb.ChannelConnection{Channel: parent + "/channels/c"}}); status.Code(err) != codes.Unimplemented {
		t.Errorf("CreateChannelConnection: %v; want UNIMPLEMENTED", err)
	}

	op, err := ec.CreateTrigger(ctx, &eventarcpb.CreateTriggerRequest{Parent: parent, TriggerId: "to-http-endpoint",
		Trigger: &eventarcpb.Trigger{EventFilters: typ, Destination: http1}})
	if err != nil {
		t.Fatalf("CreateTrigger to an HTTP endpoint: %v", err)
	}
	tr, err := op.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateTrigger to an HTTP endpoint: %v", err)
	}
	t.Cleanup(func() {
		if op, err := ec.DeleteTrigger(context.Background(), &eventarcpb.DeleteTriggerRequest{Name: tr.GetName()}); err == nil {
			_, _ = op.Wait(context.Background())
		}
	})
	if tr.GetDestination().GetHttpEndpoint().GetUri() != srv.URL+"/route" || tr.GetDestination().GetNetworkConfig().GetNetworkAttachment() != attachment {
		t.Errorf("the trigger's destination = %v", tr.GetDestination())
	}
	res, err := pc.TopicAdminClient.Publish(ctx, &pubsubpb.PublishRequest{Topic: tr.GetTransport().GetPubsub().GetTopic(),
		Messages: []*pubsubpb.PubsubMessage{{Data: []byte("to the endpoint")}}})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	id := res.GetMessageIds()[0]
	deadline := time.Now().Add(90 * time.Second)
	for {
		mu.Lock()
		var hit *http.Request
		var body string
		for i, r := range got {
			if r.Header.Get("ce-id") == id {
				hit, body = r, bodies[i]
			}
		}
		mu.Unlock()
		if hit != nil {
			if hit.URL.Path != "/route" || hit.Header.Get("ce-type") != "google.cloud.pubsub.topic.v1.messagePublished" ||
				hit.Header.Get("ce-source") != "//pubsub.googleapis.com/"+tr.GetTransport().GetPubsub().GetTopic() ||
				!strings.Contains(body, `"subscription"`) {
				t.Errorf("the endpoint got %s %v: %s", hit.URL.Path, hit.Header, body)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no CloudEvent with ce-id %s reached the HTTP endpoint", id)
		}
		time.Sleep(time.Second)
	}
}
