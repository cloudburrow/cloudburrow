package pubsubfront

import (
	"context"
	"testing"
	"time"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// updatePolicy is UpdateSubscription of the expiration policy and whatever
// else is given, through the front.
func (fx *fixture) updatePolicy(t *testing.T, s *pubsubpb.Subscription, paths ...string) (*pubsubpb.Subscription, error) {
	t.Helper()
	return fx.client.SubscriptionAdminClient.UpdateSubscription(context.Background(), &pubsubpb.UpdateSubscriptionRequest{
		Subscription: s, UpdateMask: &fieldmaskpb.FieldMask{Paths: append([]string{"expiration_policy"}, paths...)}})
}

func ttlOf(s *pubsubpb.Subscription) time.Duration {
	return s.GetExpirationPolicy().GetTtl().AsDuration()
}

// An update of the expiration policy (#891) is applied by the front: it
// reads back through the front, from Get, List and the update itself, while
// the backend is never sent the field and keeps its own. Google's rules are
// applied to the new ttl, and the rest of a mixed update still reaches the
// backend.
func TestUpdateSetsTheExpirationPolicy(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	topic := fx.topic(t, "t")
	name := subName("s")
	if _, err := fx.create(t, &pubsubpb.Subscription{Name: name, Topic: topic,
		ExpirationPolicy: &pubsubpb.ExpirationPolicy{Ttl: day(2)}, MessageRetentionDuration: day(1)}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		why string
		s   *pubsubpb.Subscription
		and []string
	}{
		{"a ttl under a day", &pubsubpb.Subscription{Name: name, ExpirationPolicy: &pubsubpb.ExpirationPolicy{Ttl: durationpb.New(12 * time.Hour)}}, nil},
		{"a ttl under the retention given with it", &pubsubpb.Subscription{Name: name,
			ExpirationPolicy: &pubsubpb.ExpirationPolicy{Ttl: day(2)}, MessageRetentionDuration: day(3)}, []string{"message_retention_duration"}},
		{"a negative ttl", &pubsubpb.Subscription{Name: name, ExpirationPolicy: &pubsubpb.ExpirationPolicy{Ttl: durationpb.New(-time.Hour)}}, nil},
	} {
		if _, err := fx.updatePolicy(t, c.s, c.and...); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s = %v, want INVALID_ARGUMENT", c.why, err)
		}
	}
	if _, err := fx.updatePolicy(t, &pubsubpb.Subscription{Name: subName("missing"),
		ExpirationPolicy: &pubsubpb.ExpirationPolicy{Ttl: day(3)}}); status.Code(err) != codes.NotFound {
		t.Errorf("updating a subscription that does not exist = %v, want NOT_FOUND", err)
	}

	got, err := fx.updatePolicy(t, &pubsubpb.Subscription{Name: name, ExpirationPolicy: &pubsubpb.ExpirationPolicy{Ttl: day(3)}})
	if err != nil || ttlOf(got) != 72*time.Hour {
		t.Fatalf("UpdateSubscription = %v, %v; want a 3-day ttl", got.GetExpirationPolicy(), err)
	}
	read, err := fx.client.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: name})
	if err != nil || ttlOf(read) != 72*time.Hour {
		t.Errorf("GetSubscription = %v, %v; want the updated 3-day ttl", read.GetExpirationPolicy(), err)
	}
	it := fx.client.SubscriptionAdminClient.ListSubscriptions(ctx, &pubsubpb.ListSubscriptionsRequest{Project: "projects/" + project})
	if l, err := it.Next(); err != nil || ttlOf(l) != 72*time.Hour {
		t.Errorf("ListSubscriptions = %v, %v; want the updated 3-day ttl", l.GetExpirationPolicy(), err)
	}
	past, err := fx.upstream.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: name})
	if err != nil || ttlOf(past) != 48*time.Hour {
		t.Errorf("the backend reads %v, %v; want its own 2 days, never sent the update", past.GetExpirationPolicy(), err)
	}

	// Mixed: the ack deadline reaches the backend, the policy stays here.
	got, err = fx.updatePolicy(t, &pubsubpb.Subscription{Name: name, AckDeadlineSeconds: 42,
		ExpirationPolicy: &pubsubpb.ExpirationPolicy{}}, "ack_deadline_seconds")
	if err != nil || got.GetAckDeadlineSeconds() != 42 || got.GetExpirationPolicy() == nil || got.GetExpirationPolicy().GetTtl() != nil {
		t.Fatalf("a mixed update = %v, %v; want a 42s deadline and a policy without a ttl", got, err)
	}
	past, _ = fx.upstream.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: name})
	if past.GetAckDeadlineSeconds() != 42 || ttlOf(past) != 48*time.Hour {
		t.Errorf("the backend reads deadline %d, policy %v; want 42 and its own 2 days", past.GetAckDeadlineSeconds(), past.GetExpirationPolicy())
	}

	// The retention is checked against the updated ttl, not the backend's.
	if _, err := fx.updatePolicy(t, &pubsubpb.Subscription{Name: name, ExpirationPolicy: &pubsubpb.ExpirationPolicy{Ttl: day(2)}}); err != nil {
		t.Fatal(err)
	}
	_, err = fx.client.SubscriptionAdminClient.UpdateSubscription(ctx, &pubsubpb.UpdateSubscriptionRequest{
		Subscription: &pubsubpb.Subscription{Name: name, MessageRetentionDuration: day(2.5)},
		UpdateMask:   &fieldmaskpb.FieldMask{Paths: []string{"message_retention_duration"}}})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("a retention above the updated ttl = %v, want INVALID_ARGUMENT", err)
	}

	// No policy with the path is Google's default.
	if got, err := fx.updatePolicy(t, &pubsubpb.Subscription{Name: name}); err != nil || ttlOf(got) != DefaultTTL {
		t.Errorf("an update naming the field with no policy = %v, %v; want the 31-day default", got.GetExpirationPolicy(), err)
	}
}

// The updated policy is the one enforced: a subscription updated to never
// expire outlives its created ttl, and one updated to a day expires a day
// later. A subscription created again under the name reads its own policy,
// not the one an update gave the old one.
func TestUpdatedExpirationIsEnforced(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	topic := fx.topic(t, "t")
	oneDay := &pubsubpb.ExpirationPolicy{Ttl: day(1)}
	for _, id := range []string{"kept", "shortened"} {
		ttl := oneDay
		if id == "shortened" {
			ttl = &pubsubpb.ExpirationPolicy{}
		}
		if _, err := fx.create(t, &pubsubpb.Subscription{Name: subName(id), Topic: topic, ExpirationPolicy: ttl,
			MessageRetentionDuration: day(1)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := fx.updatePolicy(t, &pubsubpb.Subscription{Name: subName("kept"), ExpirationPolicy: &pubsubpb.ExpirationPolicy{}}); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.updatePolicy(t, &pubsubpb.Subscription{Name: subName("shortened"), ExpirationPolicy: oneDay}); err != nil {
		t.Fatal(err)
	}
	fx.advance(t, 25*time.Hour)
	got := fx.listed(t)
	if !got[subName("kept")] || got[subName("shortened")] {
		t.Errorf("25h after the updates, listed %v: want kept (never expires) and not shortened (1 day)", got)
	}

	if err := fx.client.SubscriptionAdminClient.DeleteSubscription(ctx, &pubsubpb.DeleteSubscriptionRequest{Subscription: subName("kept")}); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.create(t, &pubsubpb.Subscription{Name: subName("kept"), Topic: topic}); err != nil {
		t.Fatal(err)
	}
	s, err := fx.client.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: subName("kept")})
	if err != nil || !proto.Equal(s.GetExpirationPolicy(), &pubsubpb.ExpirationPolicy{Ttl: durationpb.New(DefaultTTL)}) {
		t.Errorf("created again, it reads %v, %v; want the 31-day default it was created with", s.GetExpirationPolicy(), err)
	}
}
