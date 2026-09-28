package pubsubfront

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/pubsub/v2"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"cloud.google.com/go/pubsub/v2/pstest"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const project = "front-test"

// fixture is a front before an in-memory Pub/Sub, with an official client
// that reaches it only through the front.
type fixture struct {
	front  *Front
	addr   string
	client *pubsub.Client
	conn   *grpc.ClientConn
	// upstream reaches the in-memory Pub/Sub directly, past the front.
	upstream *pubsub.Client
	logs     []string
	mu       sync.Mutex
}

func newFixture(t *testing.T) *fixture { return newFixtureWith(t, false) }

// newFixtureWith is a fixture whose front relays pushes when relay is set.
func newFixtureWith(t *testing.T, relay bool) *fixture {
	t.Helper()
	fake := pstest.NewServer()
	t.Cleanup(func() { _ = fake.Close() })
	fx := &fixture{}
	f, err := New(fake.Addr, func(format string, args ...any) {
		fx.mu.Lock()
		defer fx.mu.Unlock()
		fx.logs = append(fx.logs, format)
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if relay {
		rl, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		f.RelayPushes(ctx, rl)
	}
	done := make(chan struct{})
	go func() { defer close(done); _ = f.Serve(ctx, l, time.Hour) }()
	t.Cleanup(func() { cancel(); <-done })
	fx.front, fx.addr = f, l.Addr().String()

	conn, err := grpc.NewClient(fx.addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	fx.conn = conn
	c, err := pubsub.NewClient(context.Background(), project, option.WithGRPCConn(conn))
	if err != nil {
		t.Fatal(err)
	}
	fx.client = c
	up, err := pubsub.NewClient(context.Background(), project, option.WithEndpoint(fake.Addr),
		option.WithoutAuthentication(), option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = up.Close() })
	fx.upstream = up
	return fx
}

func (fx *fixture) topic(t *testing.T, id string) string {
	t.Helper()
	name := "projects/" + project + "/topics/" + id
	if _, err := fx.client.TopicAdminClient.CreateTopic(context.Background(), &pubsubpb.Topic{Name: name}); err != nil {
		t.Fatal(err)
	}
	return name
}

func (fx *fixture) create(t *testing.T, s *pubsubpb.Subscription) (*pubsubpb.Subscription, error) {
	t.Helper()
	return fx.client.SubscriptionAdminClient.CreateSubscription(context.Background(), s)
}

// advance calls ClockMethod the way a test outside the process does.
func (fx *fixture) advance(t *testing.T, d time.Duration) time.Time {
	t.Helper()
	var now timestamppb.Timestamp
	if err := fx.conn.Invoke(context.Background(), ClockMethod, durationpb.New(d), &now); err != nil {
		t.Fatalf("advance %s: %v", d, err)
	}
	return now.AsTime()
}

// listed is the project's subscriptions; listing is not activity on any.
func (fx *fixture) listed(t *testing.T) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	it := fx.client.SubscriptionAdminClient.ListSubscriptions(context.Background(),
		&pubsubpb.ListSubscriptionsRequest{Project: "projects/" + project})
	for {
		s, err := it.Next()
		if err != nil {
			break
		}
		out[s.GetName()] = true
	}
	return out
}

func subName(id string) string { return "projects/" + project + "/subscriptions/" + id }

func day(n float64) *durationpb.Duration {
	return durationpb.New(time.Duration(n * float64(24*time.Hour)))
}

// Every call passes through: a message published through the front is
// pulled and acknowledged through it.
func TestForwardsPublishAndPull(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	topic := fx.topic(t, "t")
	if _, err := fx.create(t, &pubsubpb.Subscription{Name: subName("s"), Topic: topic}); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.client.Publisher(topic).Publish(ctx, &pubsub.Message{Data: []byte("hi"),
		Attributes: map[string]string{"k": "v"}}).Get(ctx); err != nil {
		t.Fatal(err)
	}
	resp, err := fx.client.SubscriptionAdminClient.Pull(ctx, &pubsubpb.PullRequest{Subscription: subName("s"), MaxMessages: 1})
	if err != nil || len(resp.GetReceivedMessages()) != 1 {
		t.Fatalf("pull = %v, %v", resp, err)
	}
	m := resp.GetReceivedMessages()[0]
	if string(m.GetMessage().GetData()) != "hi" || m.GetMessage().GetAttributes()["k"] != "v" {
		t.Errorf("pulled %v", m)
	}
	if err := fx.client.SubscriptionAdminClient.Acknowledge(ctx, &pubsubpb.AcknowledgeRequest{
		Subscription: subName("s"), AckIds: []string{m.GetAckId()}}); err != nil {
		t.Errorf("ack: %v", err)
	}
	if _, err := fx.client.SubscriptionAdminClient.GetSubscription(ctx,
		&pubsubpb.GetSubscriptionRequest{Subscription: subName("absent")}); status.Code(err) != codes.NotFound {
		t.Errorf("an absent subscription = %v, want the upstream's NOT_FOUND", err)
	}
}

// A policy Google refuses is refused before it reaches the emulator; one it
// accepts is created as given, and no policy is Google's 31-day default.
func TestCreateChecksTheExpirationPolicy(t *testing.T) {
	fx := newFixture(t)
	topic := fx.topic(t, "t")
	for _, c := range []struct {
		id        string
		policy    *pubsubpb.ExpirationPolicy
		retention *durationpb.Duration
		ok        bool
	}{
		{"below-a-day", &pubsubpb.ExpirationPolicy{Ttl: durationpb.New(23 * time.Hour)}, durationpb.New(time.Hour), false},
		{"negative", &pubsubpb.ExpirationPolicy{Ttl: durationpb.New(-time.Hour)}, nil, false},
		{"below-default-retention", &pubsubpb.ExpirationPolicy{Ttl: day(1)}, nil, false},
		{"below-retention", &pubsubpb.ExpirationPolicy{Ttl: day(2)}, day(3), false},
		{"one-day", &pubsubpb.ExpirationPolicy{Ttl: day(1)}, day(1), true},
		{"equal-default-retention", &pubsubpb.ExpirationPolicy{Ttl: day(7)}, nil, true},
		{"never", &pubsubpb.ExpirationPolicy{}, nil, true},
		{"default", nil, nil, true},
	} {
		got, err := fx.create(t, &pubsubpb.Subscription{Name: subName(c.id), Topic: topic,
			ExpirationPolicy: c.policy, MessageRetentionDuration: c.retention})
		if !c.ok {
			if status.Code(err) != codes.InvalidArgument {
				t.Errorf("%s: %v, want INVALID_ARGUMENT", c.id, err)
			}
			if fx.listed(t)[subName(c.id)] {
				t.Errorf("%s: refused, and created anyway", c.id)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", c.id, err)
			continue
		}
		want := c.policy
		if want == nil {
			want = &pubsubpb.ExpirationPolicy{Ttl: durationpb.New(DefaultTTL)}
		}
		if got.GetExpirationPolicy().GetTtl().AsDuration() != want.GetTtl().AsDuration() || got.GetExpirationPolicy() == nil {
			t.Errorf("%s: created with %v, want %v", c.id, got.GetExpirationPolicy(), want)
		}
	}
}

// An update that would make the retention longer than the ttl is refused.
func TestUpdateKeepsRetentionWithinTheTTL(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	topic := fx.topic(t, "t")
	if _, err := fx.create(t, &pubsubpb.Subscription{Name: subName("s"), Topic: topic,
		ExpirationPolicy: &pubsubpb.ExpirationPolicy{Ttl: day(2)}, MessageRetentionDuration: day(1)}); err != nil {
		t.Fatal(err)
	}
	update := func(d *durationpb.Duration) error {
		_, err := fx.client.SubscriptionAdminClient.UpdateSubscription(ctx, &pubsubpb.UpdateSubscriptionRequest{
			Subscription: &pubsubpb.Subscription{Name: subName("s"), MessageRetentionDuration: d},
			UpdateMask:   &fieldmaskpb.FieldMask{Paths: []string{"message_retention_duration"}}})
		return err
	}
	if err := update(day(3)); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a retention above the ttl = %v, want INVALID_ARGUMENT", err)
	}
	if err := update(day(2)); err != nil {
		t.Errorf("a retention equal to the ttl = %v", err)
	}
}

// A subscription idle for its ttl is deleted, and activity or an open
// streaming pull restarts or holds its clock; a push subscription and one
// that never expires are kept.
func TestIdleSubscriptionsExpire(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	topic := fx.topic(t, "t")
	oneDay := &pubsubpb.ExpirationPolicy{Ttl: day(1)}
	for _, s := range []*pubsubpb.Subscription{
		{Name: subName("idle"), Topic: topic, ExpirationPolicy: oneDay, MessageRetentionDuration: day(1)},
		{Name: subName("pulled"), Topic: topic, ExpirationPolicy: oneDay, MessageRetentionDuration: day(1)},
		{Name: subName("streaming"), Topic: topic, ExpirationPolicy: oneDay, MessageRetentionDuration: day(1)},
		{Name: subName("never"), Topic: topic, ExpirationPolicy: &pubsubpb.ExpirationPolicy{}},
		{Name: subName("default"), Topic: topic},
		{Name: subName("push"), Topic: topic, ExpirationPolicy: oneDay, MessageRetentionDuration: day(1),
			PushConfig: &pubsubpb.PushConfig{PushEndpoint: "http://127.0.0.1:1/push"}},
	} {
		if _, err := fx.create(t, s); err != nil {
			t.Fatal(err)
		}
	}
	// A streaming pull, open until the end.
	rctx, stop := context.WithCancel(ctx)
	received := make(chan struct{}, 1)
	recvDone := make(chan struct{})
	go func() {
		defer close(recvDone)
		_ = fx.client.Subscriber(subName("streaming")).Receive(rctx, func(_ context.Context, m *pubsub.Message) {
			m.Ack()
			select {
			case received <- struct{}{}:
			default:
			}
		})
	}()
	t.Cleanup(func() { stop(); <-recvDone })
	if _, err := fx.client.Publisher(topic).Publish(ctx, &pubsub.Message{Data: []byte("x")}).Get(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-received:
	case <-time.After(10 * time.Second):
		t.Fatal("the streaming pull received nothing")
	}

	fx.advance(t, 20*time.Hour)
	if resp, err := fx.client.SubscriptionAdminClient.Pull(ctx, &pubsubpb.PullRequest{Subscription: subName("pulled"),
		MaxMessages: 1}); err != nil || len(resp.GetReceivedMessages()) != 1 {
		t.Fatalf("pull = %v, %v", resp, err)
	}
	fx.advance(t, 5*time.Hour) // 25h: idle for more than a day
	got := fx.listed(t)
	for name, want := range map[string]bool{"idle": false, "pulled": true, "streaming": true, "never": true, "default": true, "push": true} {
		if got[subName(name)] != want {
			t.Errorf("after 25h, %s listed = %v, want %v", name, got[subName(name)], want)
		}
	}
	if _, err := fx.client.SubscriptionAdminClient.GetSubscription(ctx,
		&pubsubpb.GetSubscriptionRequest{Subscription: subName("idle")}); status.Code(err) != codes.NotFound {
		t.Errorf("the expired subscription reads %v, want NOT_FOUND", err)
	}

	fx.advance(t, 20*time.Hour) // 45h: pulled has been idle 25h
	got = fx.listed(t)
	for name, want := range map[string]bool{"pulled": false, "streaming": true, "never": true, "default": true, "push": true} {
		if got[subName(name)] != want {
			t.Errorf("after 45h, %s listed = %v, want %v", name, got[subName(name)], want)
		}
	}
	// The default is 31 days of idleness.
	fx.advance(t, 30*24*time.Hour)
	got = fx.listed(t)
	if !got[subName("never")] || got[subName("default")] || !got[subName("streaming")] {
		t.Errorf("after 31 days and 21 hours, listed %v: want never and streaming kept, default expired", got)
	}
}

// The clock only moves forward, and a zero advance reads it.
func TestClockRefusesGoingBack(t *testing.T) {
	fx := newFixture(t)
	var now timestamppb.Timestamp
	err := fx.conn.Invoke(context.Background(), ClockMethod, durationpb.New(-time.Second), &now)
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("a negative advance = %v, want INVALID_ARGUMENT", err)
	}
	before := fx.advance(t, 0)
	after := fx.advance(t, 48*time.Hour)
	if d := after.Sub(before); d < 48*time.Hour || d > 49*time.Hour {
		t.Errorf("advancing 48h moved the clock %s", d)
	}
}
