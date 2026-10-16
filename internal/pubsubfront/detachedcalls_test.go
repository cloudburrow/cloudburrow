package pubsubfront

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"github.com/googleapis/gax-go/v2/apierror"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// detachedFixture is a fixture with the subscription s detached from the
// topic t.
func detachedFixture(t *testing.T, fx *fixture, s *pubsubpb.Subscription) string {
	t.Helper()
	s.Topic = fx.topic(t, "t")
	if _, err := fx.create(t, s); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.client.TopicAdminClient.DetachSubscription(context.Background(),
		&pubsubpb.DetachSubscriptionRequest{Subscription: s.GetName()}); err != nil {
		t.Fatal(err)
	}
	return s.GetName()
}

// The calls on a detached subscription are answered as Google documents
// one (#1446): an update and a push config change its kept configuration,
// acknowledgements change nothing, a seek to a time succeeds, and a seek to
// a snapshot or a snapshot of it fail FAILED_PRECONDITION.
func TestDetachedSubscriptionCalls(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	sub := detachedFixture(t, fx, &pubsubpb.Subscription{Name: subName("s"), Labels: map[string]string{"k": "v"}})
	admin := fx.client.SubscriptionAdminClient

	got, err := admin.UpdateSubscription(ctx, &pubsubpb.UpdateSubscriptionRequest{
		Subscription: &pubsubpb.Subscription{Name: sub, AckDeadlineSeconds: 42, Labels: map[string]string{"a": "b"},
			RetryPolicy: &pubsubpb.RetryPolicy{MinimumBackoff: durationpb.New(20e9)}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"ack_deadline_seconds", "labels", "retry_policy.minimum_backoff"}}})
	if err != nil {
		t.Fatalf("UpdateSubscription of a detached subscription: %v", err)
	}
	if got.GetAckDeadlineSeconds() != 42 || got.GetLabels()["a"] != "b" || got.GetLabels()["k"] != "" ||
		got.GetRetryPolicy().GetMinimumBackoff().AsDuration().Seconds() != 20 || !got.GetDetached() || got.GetTopic() != deletedTopic {
		t.Errorf("UpdateSubscription answered %v", got)
	}
	if read, _ := admin.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: sub}); read.GetAckDeadlineSeconds() != 42 {
		t.Errorf("GetSubscription after the update reads %v", read)
	}
	for paths, want := range map[string]codes.Code{
		"topic":        codes.InvalidArgument,
		"no_such":      codes.InvalidArgument,
		"detached":     codes.FailedPrecondition,
		"labels.bad.x": codes.InvalidArgument,
	} {
		_, err := admin.UpdateSubscription(ctx, &pubsubpb.UpdateSubscriptionRequest{
			Subscription: &pubsubpb.Subscription{Name: sub, Topic: "projects/" + project + "/topics/other"},
			UpdateMask:   &fieldmaskpb.FieldMask{Paths: []string{paths}}})
		if status.Code(err) != want {
			t.Errorf("UpdateSubscription(%s) = %v, want %s", paths, err, want)
		}
	}
	if _, err := admin.UpdateSubscription(ctx, &pubsubpb.UpdateSubscriptionRequest{
		Subscription: &pubsubpb.Subscription{Name: sub, ExpirationPolicy: &pubsubpb.ExpirationPolicy{Ttl: durationpb.New(3600e9)}},
		UpdateMask:   &fieldmaskpb.FieldMask{Paths: []string{"expiration_policy"}}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("UpdateSubscription(a 1h expiration) = %v, want INVALID_ARGUMENT", err)
	}
	if err := admin.ModifyPushConfig(ctx, &pubsubpb.ModifyPushConfigRequest{Subscription: sub,
		PushConfig: &pubsubpb.PushConfig{PushEndpoint: "https://example.com/push"}}); err != nil {
		t.Fatalf("ModifyPushConfig of a detached subscription: %v", err)
	}
	if read, _ := admin.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: sub}); read.GetPushConfig().GetPushEndpoint() != "https://example.com/push" {
		t.Errorf("GetSubscription after ModifyPushConfig reads %v", read)
	}
	if err := admin.Acknowledge(ctx, &pubsubpb.AcknowledgeRequest{Subscription: sub, AckIds: []string{"a1"}}); err != nil {
		t.Errorf("Acknowledge on a detached subscription = %v, want success", err)
	}
	if err := admin.ModifyAckDeadline(ctx, &pubsubpb.ModifyAckDeadlineRequest{Subscription: sub, AckIds: []string{"a1"}, AckDeadlineSeconds: 30}); err != nil {
		t.Errorf("ModifyAckDeadline on a detached subscription = %v, want success", err)
	}
	// A seek to a snapshot is a compat test's: the in-memory Pub/Sub serves no GetSnapshot.
	_, err = admin.CreateSnapshot(ctx, &pubsubpb.CreateSnapshotRequest{Name: "projects/" + project + "/snapshots/n", Subscription: sub})
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "detached") {
		t.Errorf("CreateSnapshot of a detached subscription = %v, want FAILED_PRECONDITION", err)
	}
	// Nothing reached the emulator, which holds no such subscription.
	if _, err := fx.upstream.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: sub}); status.Code(err) != codes.NotFound {
		t.Errorf("the emulator holds the detached subscription: %v", err)
	}
}

// With exactly-once delivery, every ack ID on a detached subscription is
// reported invalid as Google reports one, which the client reads.
func TestDetachedExactlyOnceAcksAreInvalid(t *testing.T) {
	fx := newFixture(t)
	sub := detachedFixture(t, fx, &pubsubpb.Subscription{Name: subName("e"), EnableExactlyOnceDelivery: true})
	err := fx.client.SubscriptionAdminClient.Acknowledge(context.Background(),
		&pubsubpb.AcknowledgeRequest{Subscription: sub, AckIds: []string{"a1", "a2"}})
	ae, ok := apierror.FromError(err)
	if !ok || ae.GRPCStatus().Code() != codes.InvalidArgument || ae.Reason() != "EXACTLY_ONCE_ACKID_FAILURE" ||
		ae.Metadata()["a1"] != permanentInvalidAckID || ae.Metadata()["a2"] != permanentInvalidAckID {
		t.Errorf("Acknowledge = %v, want INVALID_ARGUMENT with each ack ID %s", err, permanentInvalidAckID)
	}
}

// Over REST: PATCH updates the kept configuration, :acknowledge and :seek
// to a time succeed, and a snapshot of it is FAILED_PRECONDITION.
func TestRESTDetachedSubscriptionCalls(t *testing.T) {
	up := newRESTUpstream(t)
	fx := newFixtureREST(t, up.addr())
	sub := detachedFixture(t, fx, &pubsubpb.Subscription{Name: subName("s")})
	code, body := fx.rest(t, http.MethodPatch, "/v1/"+sub,
		`{"subscription":{"ackDeadlineSeconds":33,"labels":{"x":"y"}},"updateMask":"ackDeadlineSeconds,labels"}`)
	if code != http.StatusOK || body["ackDeadlineSeconds"] != float64(33) || body["detached"] != true {
		t.Errorf("PATCH = %d %v", code, body)
	}
	if code, body := fx.rest(t, http.MethodPost, "/v1/"+sub+":acknowledge", `{"ackIds":["a"]}`); code != http.StatusOK {
		t.Errorf(":acknowledge = %d %v", code, body)
	}
	if code, body := fx.rest(t, http.MethodPost, "/v1/"+sub+":seek", `{"time":"`+timestamppb.Now().AsTime().Format("2006-01-02T15:04:05Z")+`"}`); code != http.StatusOK {
		t.Errorf(":seek = %d %v", code, body)
	}
	if code, body := fx.rest(t, http.MethodPut, "/v1/projects/"+project+"/snapshots/n", `{"subscription":"`+sub+`"}`); code != http.StatusBadRequest ||
		body["error"].(map[string]any)["status"] != "FAILED_PRECONDITION" {
		t.Errorf("PUT snapshot = %d %v", code, body)
	}
	for _, c := range up.calls() {
		t.Errorf("the emulator's REST API saw %s %s", c.method, c.path)
	}
}
