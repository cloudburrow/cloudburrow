package pubsubfront

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/pubsub/v2"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"cloud.google.com/go/pubsub/v2/pstest"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// frontRun is one run of a front that keeps its state in a file, before a
// backend that outlives it, as the emulator outlives a restart of the
// front's container.
type frontRun struct {
	front  *Front
	conn   *grpc.ClientConn
	client *pubsub.Client
	stop   func()
}

func runFront(t *testing.T, upstream, statePath string) *frontRun {
	t.Helper()
	f, err := New(upstream, func(string, ...any) {})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.KeepState(statePath); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = f.Serve(ctx, l, time.Hour) }()
	conn, err := grpc.NewClient(l.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	c, err := pubsub.NewClient(context.Background(), project, option.WithGRPCConn(conn))
	if err != nil {
		t.Fatal(err)
	}
	r := &frontRun{front: f, conn: conn, client: c}
	stopped := false
	r.stop = func() {
		if stopped {
			return
		}
		stopped = true
		_ = c.Close()
		cancel()
		<-done
		_ = f.Close()
	}
	t.Cleanup(r.stop)
	return r
}

func (r *frontRun) advance(t *testing.T, d time.Duration) time.Time {
	t.Helper()
	var now timestamppb.Timestamp
	if err := r.conn.Invoke(context.Background(), ClockMethod, durationpb.New(d), &now); err != nil {
		t.Fatalf("advance %s: %v", d, err)
	}
	return now.AsTime()
}

func (r *frontRun) listed(t *testing.T) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	it := r.client.SubscriptionAdminClient.ListSubscriptions(context.Background(),
		&pubsubpb.ListSubscriptionsRequest{Project: "projects/" + project})
	for s, err := it.Next(); err == nil; s, err = it.Next() {
		out[s.GetName()] = true
	}
	return out
}

// #898: what the front keeps and the emulator does not survives a restart
// of the front alone. After the restart an updated policy still reads back
// and is enforced, the clock has not gone back, a subscription idle since
// before the restart still expires on time, and the projects are listed.
func TestStateSurvivesARestartOfTheFront(t *testing.T) {
	fake := pstest.NewServer()
	t.Cleanup(func() { _ = fake.Close() })
	path := filepath.Join(t.TempDir(), "state.json")
	ctx := context.Background()

	first := runFront(t, fake.Addr, path)
	topic := "projects/" + project + "/topics/t"
	if _, err := first.client.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: topic}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"updated", "idle", "busy"} {
		if _, err := first.client.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{Name: subName(id), Topic: topic,
			ExpirationPolicy: &pubsubpb.ExpirationPolicy{Ttl: day(1)}, MessageRetentionDuration: day(1)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := first.client.SubscriptionAdminClient.UpdateSubscription(ctx, &pubsubpb.UpdateSubscriptionRequest{
		Subscription: &pubsubpb.Subscription{Name: subName("updated"), ExpirationPolicy: &pubsubpb.ExpirationPolicy{Ttl: day(3)}},
		UpdateMask:   fieldMask("expiration_policy")}); err != nil {
		t.Fatal(err)
	}
	// Written before the update was answered, not at the next flush.
	b, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(b), `"`+subName("updated")+`":{"ttl":"259200s"}`) {
		t.Fatalf("the state file after the update = %s, %v; want the 3-day policy in it", b, err)
	}
	before := first.advance(t, 20*time.Hour)
	// An acknowledge is activity (a GetSubscription is not, #1039).
	_ = first.client.SubscriptionAdminClient.Acknowledge(ctx, &pubsubpb.AcknowledgeRequest{Subscription: subName("busy"), AckIds: []string{"x"}})
	first.stop()

	second := runFront(t, fake.Addr, path)
	if now := second.advance(t, 0); now.Before(before) {
		t.Errorf("the clock after the restart reads %s, before %s", now, before)
	}
	s, err := second.client.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: subName("updated")})
	if err != nil || ttlOf(s) != 72*time.Hour {
		t.Fatalf("after the restart, the updated subscription reads %v, %v; want its 3-day ttl", s.GetExpirationPolicy(), err)
	}
	_ = second.client.SubscriptionAdminClient.Acknowledge(ctx, &pubsubpb.AcknowledgeRequest{Subscription: subName("updated"), AckIds: []string{"x"}})
	second.advance(t, 5*time.Hour)
	got := second.listed(t)
	if got[subName("idle")] || !got[subName("updated")] || !got[subName("busy")] {
		t.Errorf("25h on, across the restart, listed %v; want idle (a 1-day ttl, untouched) gone and the others kept", got)
	}
	// The acknowledge after the restart, at 20h, was activity on it: three days
	// idle is 92h.
	second.advance(t, 66*time.Hour)
	if got := second.listed(t); !got[subName("updated")] {
		t.Errorf("91h on, idle 71h, the updated subscription (3-day ttl) is gone")
	}
	second.advance(t, 2*time.Hour)
	if got := second.listed(t); got[subName("updated")] {
		t.Errorf("93h on, idle 73h, the updated subscription (3-day ttl) is still listed")
	}
	var l structpb.ListValue
	if err := second.conn.Invoke(ctx, ProjectsMethod, &emptypb.Empty{}, &l); err != nil || len(l.GetValues()) != 1 ||
		l.GetValues()[0].GetStringValue() != project {
		t.Errorf("projects after the restart = %v, %v; want %s", l.GetValues(), err, project)
	}
}

// A deleted subscription's kept policy is dropped from the file at once, so
// a subscription created again after a restart reads its own.
func TestStateDropsADeletedSubscriptionsPolicy(t *testing.T) {
	fake := pstest.NewServer()
	t.Cleanup(func() { _ = fake.Close() })
	path := filepath.Join(t.TempDir(), "state.json")
	ctx := context.Background()
	r := runFront(t, fake.Addr, path)
	topic := "projects/" + project + "/topics/t"
	if _, err := r.client.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: topic}); err != nil {
		t.Fatal(err)
	}
	sub := &pubsubpb.Subscription{Name: subName("s"), Topic: topic, ExpirationPolicy: &pubsubpb.ExpirationPolicy{Ttl: day(1)},
		MessageRetentionDuration: day(1)}
	if _, err := r.client.SubscriptionAdminClient.CreateSubscription(ctx, sub); err != nil {
		t.Fatal(err)
	}
	if _, err := r.client.SubscriptionAdminClient.UpdateSubscription(ctx, &pubsubpb.UpdateSubscriptionRequest{
		Subscription: &pubsubpb.Subscription{Name: subName("s"), ExpirationPolicy: &pubsubpb.ExpirationPolicy{}},
		UpdateMask:   fieldMask("expiration_policy")}); err != nil {
		t.Fatal(err)
	}
	if err := r.client.SubscriptionAdminClient.DeleteSubscription(ctx, &pubsubpb.DeleteSubscriptionRequest{Subscription: subName("s")}); err != nil {
		t.Fatal(err)
	}
	var saved savedState
	b, _ := os.ReadFile(path)
	if err := json.Unmarshal(b, &saved); err != nil || len(saved.Policies) != 0 {
		t.Errorf("the state file after the delete = %s, %v; want no policy", b, err)
	}
}

// KeepState fails on a path it cannot write, and starts afresh, replacing
// it, from a file it cannot read as state.
func TestKeepStateRefusesAnUnwritablePathAndReplacesAnUnreadableFile(t *testing.T) {
	f, err := New("127.0.0.1:1", func(string, ...any) {})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.KeepState(filepath.Join(t.TempDir(), "missing", "state.json")); err == nil {
		t.Error("KeepState in a directory that does not exist succeeded")
	}

	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	var logged []string
	g, err := New("127.0.0.1:1", func(format string, _ ...any) { logged = append(logged, format) })
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	if err := g.KeepState(path); err != nil {
		t.Fatalf("KeepState over an unreadable file = %v", err)
	}
	var saved savedState
	b, _ := os.ReadFile(path)
	if err := json.Unmarshal(b, &saved); err != nil {
		t.Errorf("the file was not replaced with state: %s", b)
	}
	if len(logged) == 0 || !strings.Contains(logged[0], "unreadable") {
		t.Errorf("logged %q; want the unreadable file named", logged)
	}
}

// A restored clock offset only moves the clock forward.
func TestRestoreNeverMovesTheClockBack(t *testing.T) {
	f, err := New("127.0.0.1:1", func(string, ...any) {})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	f.clock.Advance(48 * time.Hour)
	if err := f.restore([]byte(`{"clockOffsetNanos": 3600000000000}`)); err != nil {
		t.Fatal(err)
	}
	if got := f.clock.Offset(); got != 48*time.Hour {
		t.Errorf("offset after restoring 1h over 48h = %s", got)
	}
	if err := f.restore([]byte(`{"clockOffsetNanos": -1}`)); err == nil {
		t.Error("a negative offset was restored")
	}
	if err := f.restore([]byte(`{"policies": {"x": {"ttl": "forever"}}}`)); err == nil {
		t.Error("an unreadable policy was restored")
	}
}

func fieldMask(paths ...string) *fieldmaskpb.FieldMask { return &fieldmaskpb.FieldMask{Paths: paths} }
