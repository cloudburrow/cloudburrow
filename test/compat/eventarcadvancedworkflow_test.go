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
	"cloud.google.com/go/workflows/executions/apiv1/executionspb"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// covers: google.cloud.eventarc.v1.Eventarc/CreateMessageBus, google.cloud.eventarc.v1.Eventarc/GetMessageBus, google.cloud.eventarc.v1.Eventarc/ListMessageBusEnrollments, google.cloud.eventarc.v1.Eventarc/DeleteMessageBus, google.cloud.eventarc.v1.Eventarc/CreatePipeline, google.cloud.eventarc.v1.Eventarc/GetPipeline, google.cloud.eventarc.v1.Eventarc/DeletePipeline, google.cloud.eventarc.v1.Eventarc/CreateEnrollment, google.cloud.eventarc.v1.Eventarc/ListEnrollments, google.cloud.eventarc.v1.Eventarc/DeleteEnrollment, google.cloud.eventarc.publishing.v1.Publisher/Publish
//
// TestEventarcAdvancedPipelineStartsAWorkflow (#1996), in the run shard,
// with the official Eventarc, Eventarc Publishing and Workflows clients:
// a message bus, a pipeline whose destination is a deployed workflow, and
// an enrollment whose cel_match selects one event type. An event of that
// type published to the bus as json_message starts one execution of the
// workflow with the CloudEvent in JSON as its argument; an event of
// another type starts none. A workflow that does not exist is
// INVALID_ARGUMENT. (Pipelines to topics and buses are
// TestEventarcAdvancedBusToCloudRun's.)
func TestEventarcAdvancedPipelineStartsAWorkflow(t *testing.T) {
	h := New(t)
	w := workflowsClients(t, h)
	shardEndpoint(h, "CLOUDBURROW_TEST_RUN_PUBSUB", EnvPubSub)
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
	wf := w.deploy(t, h, fmt.Sprintf("compat-eaa-%d", n), `main:
  params: [event]
  steps:
    - r:
        return:
          type: ${event.type}
          source: ${event.source}
          id: ${event.id}
          order: ${event.data.order}
`)

	busOp, err := ec.CreateMessageBus(ctx, &eventarcpb.CreateMessageBusRequest{Parent: parent, MessageBusId: fmt.Sprintf("bus-%d", n),
		MessageBus: &eventarcpb.MessageBus{DisplayName: "compat bus"}})
	if err != nil {
		t.Fatalf("CreateMessageBus: %v", err)
	}
	bus, err := busOp.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateMessageBus: %v", err)
	}
	t.Cleanup(func() {
		if op, err := ec.DeleteMessageBus(context.Background(), &eventarcpb.DeleteMessageBusRequest{Name: bus.GetName()}); err == nil {
			_, _ = op.Wait(context.Background())
		}
	})
	if got, err := ec.GetMessageBus(ctx, &eventarcpb.GetMessageBusRequest{Name: bus.GetName()}); err != nil || got.GetDisplayName() != "compat bus" || got.GetUid() == "" {
		t.Errorf("GetMessageBus = %v, %v", got, err)
	}

	if _, err := ec.CreatePipeline(ctx, &eventarcpb.CreatePipelineRequest{Parent: parent, PipelineId: fmt.Sprintf("to-none-%d", n),
		Pipeline: &eventarcpb.Pipeline{Destinations: []*eventarcpb.Pipeline_Destination{{DestinationDescriptor: &eventarcpb.Pipeline_Destination_Workflow{
			Workflow: wf.GetName() + "-missing"}}}}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a pipeline to a missing workflow = %v; want INVALID_ARGUMENT", err)
	}
	plOp, err := ec.CreatePipeline(ctx, &eventarcpb.CreatePipelineRequest{Parent: parent, PipelineId: fmt.Sprintf("to-wf-%d", n),
		Pipeline: &eventarcpb.Pipeline{Destinations: []*eventarcpb.Pipeline_Destination{{DestinationDescriptor: &eventarcpb.Pipeline_Destination_Workflow{
			Workflow: wf.GetName()}}}}})
	if err != nil {
		t.Fatalf("CreatePipeline: %v", err)
	}
	pl, err := plOp.Wait(ctx)
	if err != nil {
		t.Fatalf("CreatePipeline: %v", err)
	}
	t.Cleanup(func() {
		if op, err := ec.DeletePipeline(context.Background(), &eventarcpb.DeletePipelineRequest{Name: pl.GetName()}); err == nil {
			_, _ = op.Wait(context.Background())
		}
	})
	if got, err := ec.GetPipeline(ctx, &eventarcpb.GetPipelineRequest{Name: pl.GetName()}); err != nil || got.GetDestinations()[0].GetWorkflow() != wf.GetName() {
		t.Errorf("GetPipeline = %v, %v", got, err)
	}

	enOp, err := ec.CreateEnrollment(ctx, &eventarcpb.CreateEnrollmentRequest{Parent: parent, EnrollmentId: fmt.Sprintf("orders-%d", n),
		Enrollment: &eventarcpb.Enrollment{CelMatch: `message.type == "com.example.order.created" && message.source.startsWith("//shop")`,
			MessageBus: bus.GetName(), Destination: pl.GetName()}})
	if err != nil {
		t.Fatalf("CreateEnrollment: %v", err)
	}
	en, err := enOp.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateEnrollment: %v", err)
	}
	t.Cleanup(func() {
		if op, err := ec.DeleteEnrollment(context.Background(), &eventarcpb.DeleteEnrollmentRequest{Name: en.GetName()}); err == nil {
			_, _ = op.Wait(context.Background())
		}
	})
	listed := false
	it := ec.ListEnrollments(ctx, &eventarcpb.ListEnrollmentsRequest{Parent: parent})
	for {
		e, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListEnrollments: %v", err)
		}
		listed = listed || e.GetName() == en.GetName()
	}
	if !listed {
		t.Errorf("ListEnrollments does not list %s", en.GetName())
	}
	bit := ec.ListMessageBusEnrollments(ctx, &eventarcpb.ListMessageBusEnrollmentsRequest{Parent: bus.GetName()})
	if got, err := bit.Next(); err != nil || got != en.GetName() {
		t.Errorf("ListMessageBusEnrollments = %q, %v", got, err)
	}

	// Another type first: no execution.
	if _, err := pc.Publish(ctx, &publishingpb.PublishRequest{MessageBus: bus.GetName(), Format: &publishingpb.PublishRequest_JsonMessage{
		JsonMessage: `{"specversion":"1.0","type":"com.example.order.deleted","source":"//shop/eu","id":"evt-0","data":{"order":41}}`}}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if _, err := pc.Publish(ctx, &publishingpb.PublishRequest{MessageBus: bus.GetName(), Format: &publishingpb.PublishRequest_JsonMessage{
		JsonMessage: `{"specversion":"1.0","type":"com.example.order.created","source":"//shop/eu","id":"evt-1","datacontenttype":"application/json","data":{"order":42}}`}}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	var names []string
	deadline := time.Now().Add(2 * time.Minute)
	for len(names) == 0 {
		it := w.ex.ListExecutions(ctx, &executionspb.ListExecutionsRequest{Parent: wf.GetName()})
		for {
			e, err := it.Next()
			if err != nil {
				break
			}
			names = append(names, e.GetName())
		}
		if len(names) == 0 {
			if time.Now().After(deadline) {
				t.Fatal("no execution started for the matching event")
			}
			time.Sleep(300 * time.Millisecond)
		}
	}
	got := w.await(t, h, names[0], time.Minute)
	if got.GetState() != executionspb.Execution_SUCCEEDED || !strings.Contains(got.GetResult(), `"type":"com.example.order.created"`) ||
		!strings.Contains(got.GetResult(), `"id":"evt-1"`) || !strings.Contains(got.GetResult(), `"order":42`) {
		t.Errorf("the execution the event started: %v %s %v", got.GetState(), got.GetResult(), got.GetError())
	}
	// Let any delivery of the non-matching event land before counting.
	time.Sleep(2 * time.Second)
	count := 0
	eit := w.ex.ListExecutions(ctx, &executionspb.ListExecutionsRequest{Parent: wf.GetName()})
	for {
		if _, err := eit.Next(); err != nil {
			break
		}
		count++
	}
	if count != 1 {
		t.Errorf("%d executions; want 1 (the non-matching event must start none)", count)
	}
}
