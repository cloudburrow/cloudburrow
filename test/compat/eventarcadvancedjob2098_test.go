//go:build compat

package compat

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	eventarc "cloud.google.com/go/eventarc/apiv1"
	eventarcpb "cloud.google.com/go/eventarc/apiv1/eventarcpb"
	publishing "cloud.google.com/go/eventarc/publishing/apiv1"
	publishingpb "cloud.google.com/go/eventarc/publishing/apiv1/publishingpb"
	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// covers: google.cloud.eventarc.v1.Eventarc/CreatePipeline, google.cloud.eventarc.v1.Eventarc/CreateEnrollment, google.cloud.eventarc.publishing.v1.Publisher/Publish
//
// TestEventarcAdvancedPipelineRunsAJob (#2098), in the run shard, with the
// official Eventarc, Eventarc Publishing and Cloud Run clients: Google's
// console's Cloud Run job (via HTTP) destination is an HTTP endpoint on the
// Cloud Run Admin API's jobs.run with an OAuth token and a message binding
// that builds the RunJobRequest. A message the enrollment matches, published
// to the bus, starts one execution of the job, its container's arguments
// the binding's overrides; this instance serves the call, nothing reaches
// Google. An OAuth token on any other endpoint is UNIMPLEMENTED.
func TestEventarcAdvancedPipelineRunsAJob(t *testing.T) {
	h := New(t)
	shardEndpoint(h, "CLOUDBURROW_TEST_RUN_PUBSUB", EnvPubSub)
	jc, xc := runJobClients(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	ec, err := eventarc.NewClient(ctx, runClientOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ec.Close() })
	pc, err := publishing.NewPublisherClient(ctx, runClientOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	parent := runParent(h)
	n := time.Now().UnixNano() % 1e6
	jobID := fmt.Sprintf("on-order-%d", n)
	job := parent + "/jobs/" + jobID
	jop, err := jc.CreateJob(ctx, &runpb.CreateJobRequest{Parent: parent, JobId: jobID, Job: shellJob(`echo "order $1"`, 1, nil)})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	t.Cleanup(func() { _, _ = jc.DeleteJob(context.Background(), &runpb.DeleteJobRequest{Name: job}) })
	if _, err := waitLRO(ctx, jop); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	busOp, err := ec.CreateMessageBus(ctx, &eventarcpb.CreateMessageBusRequest{Parent: parent, MessageBusId: fmt.Sprintf("jbus-%d", n),
		MessageBus: &eventarcpb.MessageBus{}})
	if err != nil {
		t.Fatalf("CreateMessageBus: %v", err)
	}
	bus, err := busOp.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if op, err := ec.DeleteMessageBus(context.Background(), &eventarcpb.DeleteMessageBusRequest{Name: bus.GetName()}); err == nil {
			_, _ = op.Wait(context.Background())
		}
	})
	sa := "runner@" + h.Project() + ".iam.gserviceaccount.com"
	oauth := &eventarcpb.Pipeline_Destination_AuthenticationConfig{AuthenticationMethodDescriptor: &eventarcpb.Pipeline_Destination_AuthenticationConfig_OauthToken{
		OauthToken: &eventarcpb.Pipeline_Destination_AuthenticationConfig_OAuthToken{ServiceAccount: sa}}}
	dest := func(uri string) []*eventarcpb.Pipeline_Destination {
		return []*eventarcpb.Pipeline_Destination{{AuthenticationConfig: oauth,
			DestinationDescriptor: &eventarcpb.Pipeline_Destination_HttpEndpoint_{HttpEndpoint: &eventarcpb.Pipeline_Destination_HttpEndpoint{
				Uri:                    uri,
				MessageBindingTemplate: `{"body": {"overrides": {"containerOverrides": [{"args": ["sh", "order-" + message.id]}]}}}`}}}}
	}
	if _, err := ec.CreatePipeline(ctx, &eventarcpb.CreatePipelineRequest{Parent: parent, PipelineId: fmt.Sprintf("jbad-%d", n),
		Pipeline: &eventarcpb.Pipeline{Destinations: dest("https://example.com/hook")}}); status.Code(err) != codes.Unimplemented {
		t.Errorf("an OAuth token to another endpoint = %v; want UNIMPLEMENTED", err)
	}
	plOp, err := ec.CreatePipeline(ctx, &eventarcpb.CreatePipelineRequest{Parent: parent, PipelineId: fmt.Sprintf("tojob-%d", n),
		Pipeline: &eventarcpb.Pipeline{Destinations: dest("https://run.googleapis.com/v2/" + job + ":run")}})
	if err != nil {
		t.Fatalf("CreatePipeline: %v", err)
	}
	pl, err := plOp.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if op, err := ec.DeletePipeline(context.Background(), &eventarcpb.DeletePipelineRequest{Name: pl.GetName()}); err == nil {
			_, _ = op.Wait(context.Background())
		}
	})
	enOp, err := ec.CreateEnrollment(ctx, &eventarcpb.CreateEnrollmentRequest{Parent: parent, EnrollmentId: fmt.Sprintf("jen-%d", n),
		Enrollment: &eventarcpb.Enrollment{CelMatch: `message.type == "com.example.order.created"`, MessageBus: bus.GetName(), Destination: pl.GetName()}})
	if err != nil {
		t.Fatalf("CreateEnrollment: %v", err)
	}
	en, err := enOp.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if op, err := ec.DeleteEnrollment(context.Background(), &eventarcpb.DeleteEnrollmentRequest{Name: en.GetName()}); err == nil {
			_, _ = op.Wait(context.Background())
		}
	})

	executions := func() []*runpb.Execution {
		var out []*runpb.Execution
		for it := xc.ListExecutions(ctx, &runpb.ListExecutionsRequest{Parent: job}); ; {
			x, err := it.Next()
			if err == iterator.Done {
				return out
			}
			if err != nil {
				t.Fatalf("ListExecutions: %v", err)
			}
			out = append(out, x)
		}
	}
	for _, ev := range []string{`{"specversion":"1.0","type":"com.example.order.deleted","source":"//shop","id":"1"}`,
		`{"specversion":"1.0","type":"com.example.order.created","source":"//shop","id":"42"}`} {
		if _, err := pc.Publish(ctx, &publishingpb.PublishRequest{MessageBus: bus.GetName(),
			Format: &publishingpb.PublishRequest_JsonMessage{JsonMessage: ev}}); err != nil {
			t.Fatalf("Publish: %v", err)
		}
	}
	deadline := time.Now().Add(time.Minute)
	for len(executions()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the matched message never ran the job")
		}
		time.Sleep(time.Second)
	}
	time.Sleep(2 * time.Second)
	xs := executions()
	if len(xs) != 1 {
		t.Fatalf("%d executions; want one, for the matched message only", len(xs))
	}
	if got := strings.Join(xs[0].GetTemplate().GetContainers()[0].GetArgs(), " "); !strings.Contains(got, "order-42") {
		t.Errorf("the execution's arguments %q; want the binding's order-42", got)
	}
}
