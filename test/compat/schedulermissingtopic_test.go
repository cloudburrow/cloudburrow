//go:build compat

package compat

import (
	"context"
	"testing"
	"time"

	pubsubpb "cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"cloud.google.com/go/scheduler/apiv1/schedulerpb"
	"google.golang.org/grpc/codes"
)

// TestSchedulerPubSubJobToAMissingTopicFails (#1576): a Pub/Sub job whose
// topic does not exist on the instance's Pub/Sub fails its attempt: after
// RunJob, the official client's GetJob reads a last attempt time and a
// status that is not OK; and no topic is created.
// covers: google.cloud.scheduler.v1.CloudScheduler/RunJob, google.cloud.scheduler.v1.CloudScheduler/GetJob
func TestSchedulerPubSubJobToAMissingTopicFails(t *testing.T) {
	h := New(t)
	ps := pubsubClient(t, h)
	c := schedulerClient(t, h)
	ctx := h.Context()
	parent := "projects/" + h.Project() + "/locations/us-central1"
	missing := "projects/" + h.Project() + "/topics/never-created"

	j, err := c.CreateJob(ctx, &schedulerpb.CreateJobRequest{Parent: parent, Job: &schedulerpb.Job{
		Name: parent + "/jobs/missing-topic", Schedule: "0 0 1 1 *",
		Target: &schedulerpb.Job_PubsubTarget{PubsubTarget: &schedulerpb.PubsubTarget{TopicName: missing, Data: []byte("tick")}}}})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	t.Cleanup(func() { _ = c.DeleteJob(context.Background(), &schedulerpb.DeleteJobRequest{Name: j.GetName()}) })
	if _, err := c.RunJob(ctx, &schedulerpb.RunJobRequest{Name: j.GetName()}); err != nil {
		t.Fatalf("RunJob: %v", err)
	}
	var got *schedulerpb.Job
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		got, err = c.GetJob(ctx, &schedulerpb.GetJobRequest{Name: j.GetName()})
		if err != nil {
			t.Fatalf("GetJob: %v", err)
		}
		if got.GetLastAttemptTime() != nil && got.GetStatus() != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("GetJob after RunJob = %v; want a last attempt and its status", got)
		}
	}
	if codes.Code(got.GetStatus().GetCode()) == codes.OK {
		t.Errorf("the attempt to a missing topic reads status %v; want it failed", got.GetStatus())
	}
	if _, err := ps.TopicAdminClient.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: missing}); err == nil {
		t.Errorf("the attempt created %s", missing)
	}
	t.Logf("status: %v", got.GetStatus())
}
