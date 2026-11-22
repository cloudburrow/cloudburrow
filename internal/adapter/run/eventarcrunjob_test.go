package run

import (
	"context"
	"strings"
	"testing"
	"time"

	eventarcpb "cloud.google.com/go/eventarc/apiv1/eventarcpb"
	publishingpb "cloud.google.com/go/eventarc/publishing/apiv1/publishingpb"
	runpb "cloud.google.com/go/run/apiv2/runpb"
)

// A pipeline's Cloud Run job (via HTTP) destination (#2098): an HTTP
// endpoint on the Cloud Run Admin API's jobs.run, served in process. A
// message the enrollment matches runs the job with the RunJobRequest the
// message binding builds (its container's arguments overridden); without an
// OAuth token, as on Google, the call is refused and no execution starts.
func TestPipelineRunsACloudRunJob(t *testing.T) {
	js, _, _, kube := jobsServer(fakeSecrets{})
	ctx := context.Background()
	job := jobParent + "/jobs/on-order"
	if _, err := js.CreateJob(ctx, &runpb.CreateJobRequest{Parent: jobParent, JobId: "on-order", Job: sampleJob()}); err != nil {
		t.Fatal(err)
	}
	if got := EventarcRunJobOf(EventarcRunJobURI(job)); got != job {
		t.Fatalf("EventarcRunJobOf(%s) = %q", EventarcRunJobURI(job), got)
	}
	if EventarcRunJobOf("https://run.googleapis.com/v2/"+job) != "" || EventarcRunJobOf("https://example.com/v2/"+job+":run") != "" {
		t.Error("a URI that is not jobs.run was taken for it")
	}
	f := (&FunctionsServer{s: js.s}).WithEvents(Events{Transport: &fakePublisher{}})
	bus := jobParent + "/messageBuses/orders"
	eaPut(t, eaAdv.buses, &eventarcpb.MessageBus{Name: bus})
	oauth := &eventarcpb.Pipeline_Destination_AuthenticationConfig{AuthenticationMethodDescriptor: &eventarcpb.Pipeline_Destination_AuthenticationConfig_OauthToken{
		OauthToken: &eventarcpb.Pipeline_Destination_AuthenticationConfig_OAuthToken{ServiceAccount: "runner@demo-project.iam.gserviceaccount.com"}}}
	pipeline := func(id string, auth *eventarcpb.Pipeline_Destination_AuthenticationConfig) string {
		p := &eventarcpb.Pipeline{Name: jobParent + "/pipelines/" + id, RetryPolicy: &eventarcpb.Pipeline_RetryPolicy{MaxAttempts: 1},
			Destinations: []*eventarcpb.Pipeline_Destination{{AuthenticationConfig: auth,
				DestinationDescriptor: &eventarcpb.Pipeline_Destination_HttpEndpoint_{HttpEndpoint: &eventarcpb.Pipeline_Destination_HttpEndpoint{
					Uri:                    EventarcRunJobURI(job),
					MessageBindingTemplate: `{"body": {"overrides": {"containerOverrides": [{"args": ["order-" + message.id]}]}}}`}}}}}
		if err := eaCheckPipeline(f.Eventarc())(p); err != nil {
			t.Fatalf("check the pipeline: %v", err)
		}
		eaPut(t, eaAdv.pipelines, p)
		return p.Name
	}
	eaPut(t, eaAdv.enrollments, &eventarcpb.Enrollment{Name: jobParent + "/enrollments/run", MessageBus: bus,
		Destination: pipeline("run", oauth), CelMatch: `message.type == "order.created"`})
	eaPut(t, eaAdv.enrollments, &eventarcpb.Enrollment{Name: jobParent + "/enrollments/noauth", MessageBus: bus,
		Destination: pipeline("noauth", nil), CelMatch: `message.type == "order.unsigned"`})
	publish := func(typ, id string) {
		t.Helper()
		if _, err := f.EventarcPublisher().Publish(ctx, &publishingpb.PublishRequest{MessageBus: bus, Format: &publishingpb.PublishRequest_JsonMessage{
			JsonMessage: `{"specversion":"1.0","type":"` + typ + `","source":"//shop","id":"` + id + `"}`}}); err != nil {
			t.Fatal(err)
		}
	}
	batchJobs := func() []string {
		kube.mu.Lock()
		defer kube.mu.Unlock()
		var out []string
		for _, j := range kube.jobs {
			out = append(out, j.manifest)
		}
		return out
	}

	publish("order.unsigned", "1")
	time.Sleep(500 * time.Millisecond)
	if n := len(batchJobs()); n != 0 {
		t.Fatalf("a delivery without an OAuth token ran the job (%d executions)", n)
	}
	publish("order.created", "7")
	deadline := time.Now().Add(5 * time.Second)
	for len(batchJobs()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the message never ran the job")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if m := batchJobs()[0]; !strings.Contains(m, "order-7") {
		t.Errorf("the execution does not carry the binding's argument: %.1500s", m)
	}
}
