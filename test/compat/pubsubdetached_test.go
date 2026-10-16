//go:build compat

package compat

import (
	"context"
	"strings"
	"testing"
	"time"

	pubsubpb "cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"github.com/googleapis/gax-go/v2/apierror"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// TestPubSubDetachedSubscriptionCalls (#1446): every Subscriber call on a
// detached subscription, through the official generated clients, answered
// as Google documents a detached subscription (internal/pubsubfront,
// detachedcalls.go). UpdateSubscription and ModifyPushConfig change its
// configuration, read back by GetSubscription, with its topic immutable and
// "detached" not clearable; Acknowledge and ModifyAckDeadline succeed, as
// it has no outstanding message; a Seek to a time succeeds; a Seek to a
// snapshot of its old topic and a CreateSnapshot of it fail
// FAILED_PRECONDITION, a Seek to an absent snapshot NOT_FOUND. With
// exactly-once delivery each ack ID is reported
// PERMANENT_FAILURE_INVALID_ACK_ID in the ErrorInfo the client reads.
// covers: google.pubsub.v1.Subscriber/UpdateSubscription, google.pubsub.v1.Subscriber/ModifyPushConfig, google.pubsub.v1.Subscriber/Acknowledge, google.pubsub.v1.Subscriber/ModifyAckDeadline
// covers: google.pubsub.v1.Subscriber/Seek, google.pubsub.v1.Subscriber/CreateSnapshot, google.pubsub.v1.Subscriber/GetSubscription, google.pubsub.v1.Publisher/DetachSubscription
func TestPubSubDetachedSubscriptionCalls(t *testing.T) {
	h := New(t)
	c := pubsubClient(t, h)
	ctx := h.Context()
	admin := c.SubscriptionAdminClient
	project := "projects/" + h.Project()

	tn := topic(t, h, c, "detached-calls-topic")
	sn := subscription(t, h, c, "detached-calls-sub", tn)
	sibling := subscription(t, h, c, "detached-calls-sibling", tn)
	snap := project + "/snapshots/detached-calls-snap"
	if _, err := admin.CreateSnapshot(ctx, &pubsubpb.CreateSnapshotRequest{Name: snap, Subscription: sibling}); err != nil {
		t.Fatalf("CreateSnapshot of the sibling: %v", err)
	}
	t.Cleanup(func() {
		_ = admin.DeleteSnapshot(context.Background(), &pubsubpb.DeleteSnapshotRequest{Snapshot: snap})
	})
	if _, err := c.TopicAdminClient.DetachSubscription(ctx, &pubsubpb.DetachSubscriptionRequest{Subscription: sn}); err != nil {
		t.Fatalf("DetachSubscription: %v", err)
	}

	got, err := admin.UpdateSubscription(ctx, &pubsubpb.UpdateSubscriptionRequest{
		Subscription: &pubsubpb.Subscription{Name: sn, AckDeadlineSeconds: 42, Labels: map[string]string{"env": "test"},
			RetryPolicy: &pubsubpb.RetryPolicy{MinimumBackoff: durationpb.New(20 * time.Second), MaximumBackoff: durationpb.New(60 * time.Second)}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"ack_deadline_seconds", "labels", "retry_policy"}}})
	if err != nil {
		t.Fatalf("UpdateSubscription of the detached subscription: %v", err)
	}
	if got.GetAckDeadlineSeconds() != 42 || got.GetLabels()["env"] != "test" || !got.GetDetached() || got.GetTopic() != "_deleted-topic_" ||
		got.GetRetryPolicy().GetMaximumBackoff().AsDuration() != 60*time.Second {
		t.Errorf("UpdateSubscription answered %v", got)
	}
	if read, err := admin.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: sn}); err != nil ||
		read.GetAckDeadlineSeconds() != 42 || read.GetLabels()["env"] != "test" {
		t.Errorf("GetSubscription after the update = %v, %v", read, err)
	}
	for path, want := range map[string]codes.Code{"topic": codes.InvalidArgument, "detached": codes.FailedPrecondition, "no_such_field": codes.InvalidArgument} {
		_, err := admin.UpdateSubscription(ctx, &pubsubpb.UpdateSubscriptionRequest{
			Subscription: &pubsubpb.Subscription{Name: sn, Topic: tn},
			UpdateMask:   &fieldmaskpb.FieldMask{Paths: []string{path}}})
		if status.Code(err) != want {
			t.Errorf("UpdateSubscription(%s) = %v, want %s", path, err, want)
		}
	}
	if err := admin.ModifyPushConfig(ctx, &pubsubpb.ModifyPushConfigRequest{Subscription: sn,
		PushConfig: &pubsubpb.PushConfig{PushEndpoint: "https://push.example.com/burrow"}}); err != nil {
		t.Fatalf("ModifyPushConfig of the detached subscription: %v", err)
	}
	if read, err := admin.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: sn}); err != nil ||
		read.GetPushConfig().GetPushEndpoint() != "https://push.example.com/burrow" {
		t.Errorf("GetSubscription after ModifyPushConfig = %v, %v", read, err)
	}
	if err := admin.Acknowledge(ctx, &pubsubpb.AcknowledgeRequest{Subscription: sn, AckIds: []string{"some-ack-id"}}); err != nil {
		t.Errorf("Acknowledge on the detached subscription = %v, want success", err)
	}
	if err := admin.ModifyAckDeadline(ctx, &pubsubpb.ModifyAckDeadlineRequest{Subscription: sn,
		AckIds: []string{"some-ack-id"}, AckDeadlineSeconds: 30}); err != nil {
		t.Errorf("ModifyAckDeadline on the detached subscription = %v, want success", err)
	}
	if _, err := admin.Seek(ctx, &pubsubpb.SeekRequest{Subscription: sn,
		Target: &pubsubpb.SeekRequest_Time{Time: timestamppb.Now()}}); err != nil {
		t.Errorf("Seek to a time = %v, want success", err)
	}
	_, err = admin.Seek(ctx, &pubsubpb.SeekRequest{Subscription: sn, Target: &pubsubpb.SeekRequest_Snapshot{Snapshot: snap}})
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "detached") {
		t.Errorf("Seek to a snapshot of its old topic = %v, want FAILED_PRECONDITION naming the detach", err)
	}
	if _, err := admin.Seek(ctx, &pubsubpb.SeekRequest{Subscription: sn,
		Target: &pubsubpb.SeekRequest_Snapshot{Snapshot: project + "/snapshots/absent-snap"}}); status.Code(err) != codes.NotFound {
		t.Errorf("Seek to an absent snapshot = %v, want NOT_FOUND", err)
	}
	_, err = admin.CreateSnapshot(ctx, &pubsubpb.CreateSnapshotRequest{Name: project + "/snapshots/detached-calls-snap2", Subscription: sn})
	if status.Code(err) != codes.FailedPrecondition {
		t.Errorf("CreateSnapshot of the detached subscription = %v, want FAILED_PRECONDITION", err)
	}

	// Exactly-once delivery: each ack ID is reported invalid.
	eod := project + "/subscriptions/detached-calls-eod"
	if _, err := admin.CreateSubscription(ctx, &pubsubpb.Subscription{Name: eod, Topic: tn, EnableExactlyOnceDelivery: true}); err != nil {
		t.Fatalf("CreateSubscription(exactly-once): %v", err)
	}
	t.Cleanup(func() {
		_ = admin.DeleteSubscription(context.Background(), &pubsubpb.DeleteSubscriptionRequest{Subscription: eod})
	})
	if _, err := c.TopicAdminClient.DetachSubscription(ctx, &pubsubpb.DetachSubscriptionRequest{Subscription: eod}); err != nil {
		t.Fatalf("DetachSubscription(exactly-once): %v", err)
	}
	err = admin.Acknowledge(ctx, &pubsubpb.AcknowledgeRequest{Subscription: eod, AckIds: []string{"a1"}})
	ae, ok := apierror.FromError(err)
	if !ok || ae.GRPCStatus().Code() != codes.InvalidArgument || ae.Reason() != "EXACTLY_ONCE_ACKID_FAILURE" ||
		ae.Metadata()["a1"] != "PERMANENT_FAILURE_INVALID_ACK_ID" {
		t.Errorf("Acknowledge on a detached exactly-once subscription = %v, want each ack ID PERMANENT_FAILURE_INVALID_ACK_ID", err)
	}
}
