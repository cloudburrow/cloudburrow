//go:build compat

package compat

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/dataflow/apiv1beta3/dataflowpb"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
)

// covers: google.dataflow.v1beta3.JobsV1Beta3/CreateJob, google.dataflow.v1beta3.JobsV1Beta3/UpdateJob
//
// TestJavaDataflowPubsub (#2215): a streaming Java pipeline
// (testdata/dataflowjava PubsubCopy) reads a subscription's messages with
// their attributes (PubsubIO.readMessagesWithAttributes) and publishes
// each to a topic (PubsubIO.writeMessages), submitted with the Java SDK's
// DataflowRunner, on the Beam Java 11, 17 and 21 SDK harnesses (the
// job's sdkContainerImage, as a custom container names one). Each job's
// output messages are its input's, upper-cased, with their attributes
// and the Java version they ran on; drained, the job is DRAINED.
func TestJavaDataflowPubsub(t *testing.T) {
	h, _, c := dataflowEnv(t)
	storageAddr := h.Endpoint(EnvStorage)
	pubsubAddr := h.Endpoint(EnvPubSub)
	dfAddr := h.Endpoint(EnvDataflow)
	ps := pubsubClient(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()
	sc := storageClient(t, h)
	project := h.Project()
	bucket := fmt.Sprintf("%s-dataflow-jps-%d", project, time.Now().UnixNano()%1e6)
	dataflowBucket(t, ctx, sc, project, bucket)
	work := buildBeamJava(t, ctx)
	network := beamNetwork(t, ctx, map[int]string{9000: storageAddr, 9001: pubsubAddr, 80: dfAddr})

	for _, java := range []string{"11", "17", "21"} {
		t.Run("Java"+java, func(t *testing.T) {
			suffix := fmt.Sprintf("%s-%d", java, time.Now().UnixNano()%1e6)
			in := fmt.Sprintf("projects/%s/topics/jps-in-%s", project, suffix)
			inSub := fmt.Sprintf("projects/%s/subscriptions/jps-in-%s", project, suffix)
			out := fmt.Sprintf("projects/%s/topics/jps-out-%s", project, suffix)
			outSub := fmt.Sprintf("projects/%s/subscriptions/jps-out-%s", project, suffix)
			for _, tp := range []string{in, out} {
				if _, err := ps.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: tp}); err != nil {
					t.Fatal(err)
				}
			}
			for s, tp := range map[string]string{inSub: in, outSub: out} {
				if _, err := ps.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{Name: s, Topic: tp}); err != nil {
					t.Fatal(err)
				}
			}
			name := "java-pubsub-" + suffix
			args := append([]string{"--inputSubscription=" + inSub, "--outputTopic=" + out,
				"--sdkContainerImage=apache/beam_java" + java + "_sdk:2.78.0"}, javaDataflowArgs(project, bucket, name)...)
			id := javaJobID(t, runBeamJava(t, ctx, network, work, "dev.cloudburrow.dataflow.PubsubCopy", args...))
			j := waitDataflowJob(t, ctx, c, project, id, dataflowpb.JobState_JOB_STATE_RUNNING, dataflowpb.JobState_JOB_STATE_FAILED)
			if j.CurrentState != dataflowpb.JobState_JOB_STATE_RUNNING {
				t.Fatalf("job %s is %s:\n%s", id, j.CurrentState, jobMessages(t, ctx, c, project, id))
			}
			if j.Type != dataflowpb.JobType_JOB_TYPE_STREAMING {
				t.Errorf("job %s is %s; want JOB_TYPE_STREAMING", id, j.Type)
			}
			for _, w := range []string{"apple", "banana"} {
				if _, err := ps.TopicAdminClient.Publish(ctx, &pubsubpb.PublishRequest{Topic: in, Messages: []*pubsubpb.PubsubMessage{
					{Data: []byte(w), Attributes: map[string]string{"fruit": w}}}}); err != nil {
					t.Fatal(err)
				}
			}
			var got []string
			for deadline := time.Now().Add(8 * time.Minute); len(got) < 2; {
				resp, err := ps.SubscriptionAdminClient.Pull(ctx, &pubsubpb.PullRequest{Subscription: outSub, MaxMessages: 10, ReturnImmediately: true})
				if err != nil {
					t.Fatal(err)
				}
				var acks []string
				for _, rm := range resp.ReceivedMessages {
					m := rm.Message
					got = append(got, fmt.Sprintf("%s fruit=%s java=%s", m.Data, m.Attributes["fruit"], m.Attributes["java"]))
					acks = append(acks, rm.AckId)
				}
				if len(acks) > 0 {
					_ = ps.SubscriptionAdminClient.Acknowledge(ctx, &pubsubpb.AcknowledgeRequest{Subscription: outSub, AckIds: acks})
				}
				if len(got) < 2 && time.Now().After(deadline) {
					t.Fatalf("the job published %v:\n%s", got, jobMessages(t, ctx, c, project, id))
				}
				time.Sleep(2 * time.Second)
			}
			sort.Strings(got)
			want := fmt.Sprintf("APPLE fruit=apple java=%s,BANANA fruit=banana java=%s", java, java)
			if s := strings.Join(got, ","); s != want {
				t.Errorf("the job published %s; want %s", s, want)
			}
			if _, err := c.jobs.UpdateJob(ctx, &dataflowpb.UpdateJobRequest{ProjectId: project, Location: "us-central1", JobId: id,
				Job: &dataflowpb.Job{RequestedState: dataflowpb.JobState_JOB_STATE_DRAINED}}); err != nil {
				t.Fatalf("drain: %v", err)
			}
			j = waitDataflowJob(t, ctx, c, project, id, dataflowpb.JobState_JOB_STATE_DRAINED, dataflowpb.JobState_JOB_STATE_FAILED, dataflowpb.JobState_JOB_STATE_CANCELLED)
			if j.CurrentState != dataflowpb.JobState_JOB_STATE_DRAINED {
				t.Fatalf("job %s ended %s:\n%s", id, j.CurrentState, jobMessages(t, ctx, c, project, id))
			}
		})
	}
}
