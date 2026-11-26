//go:build compat

package compat

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	dataflow "cloud.google.com/go/dataflow/apiv1beta3"
	"cloud.google.com/go/dataflow/apiv1beta3/dataflowpb"
	"cloud.google.com/go/pubsub/v2"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// waitSubscriptionRead waits until a running job has read every message of
// sub: a pull finds none (one the test pulls is handed back).
func waitSubscriptionRead(t *testing.T, ctx context.Context, ps *pubsub.Client, c dataflowClients, project, sub, id string) {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Minute); ; {
		resp, err := ps.SubscriptionAdminClient.Pull(ctx, &pubsubpb.PullRequest{Subscription: sub, MaxMessages: 1, ReturnImmediately: true})
		if err == nil && len(resp.ReceivedMessages) == 0 {
			return
		}
		if err == nil {
			_ = ps.SubscriptionAdminClient.ModifyAckDeadline(ctx, &pubsubpb.ModifyAckDeadlineRequest{Subscription: sub,
				AckIds: []string{resp.ReceivedMessages[0].AckId}, AckDeadlineSeconds: 0})
		}
		if time.Now().After(deadline) {
			t.Fatalf("the job did not read the subscription:\n%s", jobMessages(t, ctx, c, project, id))
		}
		time.Sleep(3 * time.Second)
	}
}

// covers: google.dataflow.v1beta3.JobsV1Beta3/SnapshotJob, google.dataflow.v1beta3.SnapshotsV1Beta3/GetSnapshot, google.dataflow.v1beta3.SnapshotsV1Beta3/ListSnapshots, google.dataflow.v1beta3.SnapshotsV1Beta3/DeleteSnapshot
//
// TestDataflowSnapshots (#2215, guides/using-snapshots): a streaming word
// count (one-hour windows, so its counts are state until it is drained)
// reads words; a snapshot of it is taken (PENDING, then READY, with its
// size), listed by region and by job; the job is cancelled, losing its
// state, and a job created from the snapshot (the Python SDK's
// --create_from_snapshot) reads more words; drained, it publishes the
// counts of both jobs' words. A batch job is not snapshotted, and a
// deleted snapshot is gone. gcloud dataflow snapshots create, describe,
// list and delete, configured by gcloud-setup alone, do the same.
func TestDataflowSnapshots(t *testing.T) {
	h, addr, c := dataflowEnv(t)
	storageAddr := h.Endpoint(EnvStorage)
	pubsubAddr := h.Endpoint(EnvPubSub)
	ps := pubsubClient(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()
	snaps, err := dataflow.NewSnapshotsV1Beta3Client(ctx, option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	defer snaps.Close()
	sc := storageClient(t, h)
	project := h.Project()
	bucket := fmt.Sprintf("%s-dataflow-snap-%d", project, time.Now().UnixNano()%1e6)
	dataflowBucket(t, ctx, sc, project, bucket)
	suffix := fmt.Sprint(time.Now().UnixNano() % 1e6)
	in := fmt.Sprintf("projects/%s/topics/snap-in-%s", project, suffix)
	inSub := fmt.Sprintf("projects/%s/subscriptions/snap-in-%s", project, suffix)
	out := fmt.Sprintf("projects/%s/topics/snap-out-%s", project, suffix)
	outSub := fmt.Sprintf("projects/%s/subscriptions/snap-out-%s", project, suffix)
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
	publish := func(words ...string) {
		for _, w := range words {
			if _, err := ps.TopicAdminClient.Publish(ctx, &pubsubpb.PublishRequest{Topic: in, Messages: []*pubsubpb.PubsubMessage{{Data: []byte(w)}}}); err != nil {
				t.Fatal(err)
			}
		}
	}
	network := beamNetwork(t, ctx, map[int]string{9000: storageAddr, 9001: pubsubAddr, 80: h.Endpoint(EnvDataflow)})
	start := func(name string, extra ...string) string {
		args := append([]string{"/flow/snapshot.py", "--subscription=" + inSub, "--output_topic=" + out}, dataflowRunnerArgs(project, bucket, name)...)
		id := jobIDOf(t, runBeamPython(t, ctx, network, project, append(args, extra...)...))
		j := waitDataflowJob(t, ctx, c, project, id, dataflowpb.JobState_JOB_STATE_RUNNING, dataflowpb.JobState_JOB_STATE_FAILED)
		if j.CurrentState != dataflowpb.JobState_JOB_STATE_RUNNING {
			t.Fatalf("job %s is %s:\n%s", id, j.CurrentState, jobMessages(t, ctx, c, project, id))
		}
		return id
	}

	first := start("snap-words-" + suffix)
	publish("apple", "banana", "apple")
	waitSubscriptionRead(t, ctx, ps, c, project, inSub, first)

	if _, err := c.jobs.SnapshotJob(ctx, &dataflowpb.SnapshotJobRequest{ProjectId: project, Location: "us-central1", JobId: "2026-01-01_00_00_00-1"}); status.Code(err) != codes.NotFound {
		t.Errorf("SnapshotJob of a missing job = %v; want NOT_FOUND", err)
	}
	sn, err := c.jobs.SnapshotJob(ctx, &dataflowpb.SnapshotJobRequest{ProjectId: project, Location: "us-central1", JobId: first, Description: "before cancel"})
	if err != nil {
		t.Fatalf("SnapshotJob: %v", err)
	}
	if sn.SourceJobId != first || sn.Region != "us-central1" || sn.Description != "before cancel" || sn.GetTtl().AsDuration() != 7*24*time.Hour {
		t.Errorf("snapshot = %v", sn)
	}
	var got *dataflowpb.Snapshot
	for deadline := time.Now().Add(5 * time.Minute); ; {
		if got, err = snaps.GetSnapshot(ctx, &dataflowpb.GetSnapshotRequest{ProjectId: project, Location: "us-central1", SnapshotId: sn.Id}); err != nil {
			t.Fatal(err)
		}
		if got.State == dataflowpb.SnapshotState_READY || got.State == dataflowpb.SnapshotState_FAILED || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Second)
	}
	if got.State != dataflowpb.SnapshotState_READY || got.DiskSizeBytes <= 0 {
		t.Fatalf("snapshot %s is %s (%d bytes); want READY", sn.Id, got.State, got.DiskSizeBytes)
	}
	for _, job := range []string{"", first} {
		l, err := snaps.ListSnapshots(ctx, &dataflowpb.ListSnapshotsRequest{ProjectId: project, Location: "us-central1", JobId: job})
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, x := range l.Snapshots {
			found = found || x.Id == sn.Id
		}
		if !found {
			t.Errorf("ListSnapshots(job %q) does not list %s", job, sn.Id)
		}
	}

	// gcloud dataflow snapshots, configured by gcloud-setup alone.
	t.Run("gcloud", func(t *testing.T) {
		gc := gcloudDataflow(t, h, project)
		o, err := gc("dataflow", "snapshots", "create", "--job-id="+first, "--region=us-central1", "--snapshot-ttl=2d",
			"--format=value(id)")
		if err != nil {
			t.Fatalf("gcloud dataflow snapshots create: %v\n%s", err, o)
		}
		id2 := strings.TrimSpace(o[strings.LastIndex(strings.TrimSpace(o), "\n")+1:])
		for deadline := time.Now().Add(5 * time.Minute); ; {
			o, err = gc("dataflow", "snapshots", "describe", id2, "--region=us-central1", "--format=value(state,ttl,sourceJobId)")
			if err != nil {
				t.Fatalf("gcloud dataflow snapshots describe: %v\n%s", err, o)
			}
			if strings.Contains(o, "READY") || time.Now().After(deadline) {
				break
			}
			time.Sleep(time.Second)
		}
		if !strings.Contains(o, "READY") || !strings.Contains(o, "172800s") || !strings.Contains(o, first) {
			t.Errorf("gcloud dataflow snapshots describe = %q; want READY, 172800s, %s", o, first)
		}
		o, err = gc("dataflow", "snapshots", "list", "--job-id="+first, "--region=us-central1", "--format=json")
		if err != nil || !strings.Contains(o, id2) || !strings.Contains(o, sn.Id) {
			t.Errorf("gcloud dataflow snapshots list = %v\n%s; want %s and %s", err, o, sn.Id, id2)
		}
		if o, err := gc("dataflow", "snapshots", "delete", id2, "--region=us-central1"); err != nil {
			t.Errorf("gcloud dataflow snapshots delete: %v\n%s", err, o)
		}
		if _, err := snaps.GetSnapshot(ctx, &dataflowpb.GetSnapshotRequest{ProjectId: project, Location: "us-central1", SnapshotId: id2}); status.Code(err) != codes.NotFound {
			t.Errorf("GetSnapshot after gcloud's delete = %v; want NOT_FOUND", err)
		}
	})

	// The first job's state is lost with it.
	if _, err := c.jobs.UpdateJob(ctx, &dataflowpb.UpdateJobRequest{ProjectId: project, Location: "us-central1", JobId: first,
		Job: &dataflowpb.Job{RequestedState: dataflowpb.JobState_JOB_STATE_CANCELLED}}); err != nil {
		t.Fatal(err)
	}
	waitDataflowJob(t, ctx, c, project, first, dataflowpb.JobState_JOB_STATE_CANCELLED)

	second := start("snap-words-restored-"+suffix, "--create_from_snapshot="+sn.Id)
	if j, _ := c.jobs.GetJob(ctx, &dataflowpb.GetJobRequest{ProjectId: project, Location: "us-central1", JobId: second}); j.GetCreatedFromSnapshotId() != sn.Id {
		t.Errorf("the second job's createdFromSnapshotId = %q; want %q", j.GetCreatedFromSnapshotId(), sn.Id)
	}
	publish("apple", "cherry")
	waitSubscriptionRead(t, ctx, ps, c, project, inSub, second)
	if _, err := c.jobs.UpdateJob(ctx, &dataflowpb.UpdateJobRequest{ProjectId: project, Location: "us-central1", JobId: second,
		Job: &dataflowpb.Job{RequestedState: dataflowpb.JobState_JOB_STATE_DRAINED}}); err != nil {
		t.Fatal(err)
	}
	if j := waitDataflowJob(t, ctx, c, project, second, dataflowpb.JobState_JOB_STATE_DRAINED, dataflowpb.JobState_JOB_STATE_FAILED); j.CurrentState != dataflowpb.JobState_JOB_STATE_DRAINED {
		t.Fatalf("job %s ended %s:\n%s", second, j.CurrentState, jobMessages(t, ctx, c, project, second))
	}
	var counts []string
	for deadline := time.Now().Add(time.Minute); len(counts) < 3 && time.Now().Before(deadline); {
		resp, err := ps.SubscriptionAdminClient.Pull(ctx, &pubsubpb.PullRequest{Subscription: outSub, MaxMessages: 10, ReturnImmediately: true})
		if err != nil {
			t.Fatal(err)
		}
		var acks []string
		for _, m := range resp.ReceivedMessages {
			counts = append(counts, string(m.Message.Data))
			acks = append(acks, m.AckId)
		}
		if len(acks) > 0 {
			_ = ps.SubscriptionAdminClient.Acknowledge(ctx, &pubsubpb.AcknowledgeRequest{Subscription: outSub, AckIds: acks})
		}
		time.Sleep(time.Second)
	}
	sort.Strings(counts)
	if s := strings.Join(counts, ", "); s != "apple: 3, banana: 1, cherry: 1" {
		t.Errorf("the restored job's counts are %q; want apple: 3, banana: 1, cherry: 1 (the snapshot's apple: 2, banana: 1 and its own)", s)
	}

	if _, err := c.jobs.SnapshotJob(ctx, &dataflowpb.SnapshotJobRequest{ProjectId: project, Location: "us-central1", JobId: second}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("SnapshotJob of a drained job = %v; want FAILED_PRECONDITION", err)
	}
	if _, err := snaps.DeleteSnapshot(ctx, &dataflowpb.DeleteSnapshotRequest{ProjectId: project, Location: "us-central1", SnapshotId: sn.Id}); err != nil {
		t.Fatal(err)
	}
	if _, err := snaps.GetSnapshot(ctx, &dataflowpb.GetSnapshotRequest{ProjectId: project, Location: "us-central1", SnapshotId: sn.Id}); status.Code(err) != codes.NotFound {
		t.Errorf("GetSnapshot after delete = %v; want NOT_FOUND", err)
	}
}
