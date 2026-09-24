//go:build compat

package compat

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"cloud.google.com/go/pubsub/v2"
	pubsubpb "cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	scheduler "cloud.google.com/go/scheduler/apiv1"
	schedulerpb "cloud.google.com/go/scheduler/apiv1/schedulerpb"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// EnvScheduler is the Cloud Scheduler endpoint.
const EnvScheduler = "CLOUDBURROW_TEST_SCHEDULER"

func schedulerClient(t *testing.T, h *Harness) *scheduler.CloudSchedulerClient {
	t.Helper()
	c, err := scheduler.NewCloudSchedulerClient(h.Context(), option.WithEndpoint(h.Endpoint(EnvScheduler)),
		option.WithoutAuthentication(), option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// covers: google.cloud.scheduler.v1.CloudScheduler/CreateJob, google.cloud.scheduler.v1.CloudScheduler/GetJob, google.cloud.scheduler.v1.CloudScheduler/ListJobs, google.cloud.scheduler.v1.CloudScheduler/PauseJob, google.cloud.scheduler.v1.CloudScheduler/ResumeJob, google.cloud.scheduler.v1.CloudScheduler/RunJob, google.cloud.scheduler.v1.CloudScheduler/DeleteJob
//
// TestSchedulerHTTPAndPubSubJobs (#302), with cloud.google.com/go/scheduler/
// apiv1 against the CI instance: an every-minute HTTP job that RunJob
// delivers at once to an httptest server; a Pub/Sub job whose message a
// subscription receives; pause and resume; and App Engine and OIDC targets
// refused as UNIMPLEMENTED.
func TestSchedulerHTTPAndPubSubJobs(t *testing.T) {
	h := New(t)
	c := schedulerClient(t, h)
	ctx := h.Context()
	parent := "projects/" + h.Project() + "/locations/us-central1"

	got := make(chan http.Header, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got <- r.Header.Clone() }))
	defer srv.Close()
	job, err := c.CreateJob(ctx, &schedulerpb.CreateJobRequest{Parent: parent, Job: &schedulerpb.Job{
		Name: parent + "/jobs/http-every-minute", Schedule: "* * * * *", TimeZone: "America/New_York",
		Target: &schedulerpb.Job_HttpTarget{HttpTarget: &schedulerpb.HttpTarget{Uri: srv.URL, HttpMethod: schedulerpb.HttpMethod_POST}}}})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	t.Cleanup(func() { _ = c.DeleteJob(context.Background(), &schedulerpb.DeleteJobRequest{Name: job.GetName()}) })
	if _, err := c.RunJob(ctx, &schedulerpb.RunJobRequest{Name: job.GetName()}); err != nil {
		t.Fatalf("RunJob: %v", err)
	}
	select {
	case hdr := <-got:
		if hdr.Get("X-CloudScheduler") != "true" || hdr.Get("X-CloudScheduler-JobName") != "http-every-minute" {
			t.Errorf("delivery headers = %v", hdr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("RunJob did not deliver to the target")
	}

	if p, err := c.PauseJob(ctx, &schedulerpb.PauseJobRequest{Name: job.GetName()}); err != nil || p.GetState() != schedulerpb.Job_PAUSED {
		t.Errorf("PauseJob = %v, %v", p, err)
	}
	if r, err := c.ResumeJob(ctx, &schedulerpb.ResumeJobRequest{Name: job.GetName()}); err != nil || r.GetState() != schedulerpb.Job_ENABLED {
		t.Errorf("ResumeJob = %v, %v", r, err)
	}
	it := c.ListJobs(ctx, &schedulerpb.ListJobsRequest{Parent: parent})
	if j, err := it.Next(); err != nil || j.GetName() != job.GetName() {
		t.Errorf("ListJobs = %v, %v", j, err)
	}
	if g, err := c.GetJob(ctx, &schedulerpb.GetJobRequest{Name: job.GetName()}); err != nil || g.GetLastAttemptTime() == nil {
		t.Errorf("GetJob after a run = %v, %v; want a last attempt time", g, err)
	}

	// Pub/Sub target: the message reaches a subscription.
	ps := pubsubClient(t, h)
	tp := topic(t, h, ps, "sched-target")
	sub := "projects/" + h.Project() + "/subscriptions/sched-target-sub"
	if _, err := ps.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{Name: sub, Topic: tp}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = ps.SubscriptionAdminClient.DeleteSubscription(context.Background(), &pubsubpb.DeleteSubscriptionRequest{Subscription: sub})
	})
	pj, err := c.CreateJob(ctx, &schedulerpb.CreateJobRequest{Parent: parent, Job: &schedulerpb.Job{
		Name: parent + "/jobs/pubsub-weekly", Schedule: "0 9 * * 1",
		Target: &schedulerpb.Job_PubsubTarget{PubsubTarget: &schedulerpb.PubsubTarget{
			TopicName: tp, Data: []byte("tick"), Attributes: map[string]string{"from": "scheduler"}}}}})
	if err != nil {
		t.Fatalf("CreateJob(pubsub): %v", err)
	}
	t.Cleanup(func() { _ = c.DeleteJob(context.Background(), &schedulerpb.DeleteJobRequest{Name: pj.GetName()}) })
	if _, err := c.RunJob(ctx, &schedulerpb.RunJobRequest{Name: pj.GetName()}); err != nil {
		t.Fatal(err)
	}
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	var msg *pubsub.Message
	_ = ps.Subscriber(sub).Receive(rctx, func(_ context.Context, m *pubsub.Message) {
		m.Ack()
		if msg == nil {
			msg = m
			cancel()
		}
	})
	cancel()
	if msg == nil || string(msg.Data) != "tick" || msg.Attributes["from"] != "scheduler" {
		t.Errorf("the Pub/Sub job's message = %v, want data tick and attribute from=scheduler", msg)
	}

	// Refused, not accepted and ignored.
	if _, err := c.CreateJob(ctx, &schedulerpb.CreateJobRequest{Parent: parent, Job: &schedulerpb.Job{
		Name: parent + "/jobs/app-engine", Schedule: "* * * * *",
		Target: &schedulerpb.Job_AppEngineHttpTarget{AppEngineHttpTarget: &schedulerpb.AppEngineHttpTarget{RelativeUri: "/"}}}}); status.Code(err) != codes.Unimplemented {
		t.Errorf("App Engine target = %v, want Unimplemented", err)
	}
	if _, err := c.CreateJob(ctx, &schedulerpb.CreateJobRequest{Parent: parent, Job: &schedulerpb.Job{
		Name: parent + "/jobs/oidc", Schedule: "* * * * *",
		Target: &schedulerpb.Job_HttpTarget{HttpTarget: &schedulerpb.HttpTarget{Uri: srv.URL,
			AuthorizationHeader: &schedulerpb.HttpTarget_OidcToken{OidcToken: &schedulerpb.OidcToken{ServiceAccountEmail: "x@y"}}}}}}); status.Code(err) != codes.Unimplemented {
		t.Errorf("OIDC token = %v, want Unimplemented", err)
	}
}
