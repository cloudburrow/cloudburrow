package pubsubfront

import (
	"context"
	"net"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/pubsub/v2"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// DetachSubscription, which the emulator refuses, is carried out by the
// front: the subscription reads back detached, on "_deleted-topic_", with
// the labels it had; it is gone from the emulator and its topic's list;
// pulls fail FAILED_PRECONDITION; nothing published afterwards reaches it;
// it is still listed and its name still taken, until it is deleted.
func TestDetachSubscription(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	topic := fx.topic(t, "t")
	sub := subName("s")
	if _, err := fx.create(t, &pubsubpb.Subscription{Name: sub, Topic: topic, Labels: map[string]string{"k": "v"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.client.Publisher(topic).Publish(ctx, &pubsub.Message{Data: []byte("before")}).Get(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.client.TopicAdminClient.DetachSubscription(ctx, &pubsubpb.DetachSubscriptionRequest{Subscription: sub}); err != nil {
		t.Fatalf("DetachSubscription: %v", err)
	}
	got, err := fx.client.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: sub})
	if err != nil {
		t.Fatalf("GetSubscription after detach: %v", err)
	}
	if !got.GetDetached() || got.GetTopic() != deletedTopic || got.GetLabels()["k"] != "v" {
		t.Errorf("detached subscription reads %v, want detached on %s with its labels", got, deletedTopic)
	}
	if _, err := fx.upstream.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: sub}); status.Code(err) != codes.NotFound {
		t.Errorf("the emulator still holds the detached subscription: %v", err)
	}
	it := fx.client.TopicAdminClient.ListTopicSubscriptions(ctx, &pubsubpb.ListTopicSubscriptionsRequest{Topic: topic})
	if n, err := it.Next(); err == nil {
		t.Errorf("ListTopicSubscriptions still names %s", n)
	}
	if !fx.listed(t)[sub] {
		t.Error("ListSubscriptions no longer names the detached subscription")
	}
	if _, err := fx.client.Publisher(topic).Publish(ctx, &pubsub.Message{Data: []byte("after")}).Get(ctx); err != nil {
		t.Fatal(err)
	}
	_, err = fx.client.SubscriptionAdminClient.Pull(ctx, &pubsubpb.PullRequest{Subscription: sub, MaxMessages: 10})
	if status.Code(err) != codes.FailedPrecondition {
		t.Errorf("Pull of a detached subscription = %v, want FAILED_PRECONDITION", err)
	}
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := fx.client.Subscriber(sub).Receive(rctx, func(context.Context, *pubsub.Message) {}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("StreamingPull of a detached subscription = %v, want FAILED_PRECONDITION", err)
	}
	if _, err := fx.create(t, &pubsubpb.Subscription{Name: sub, Topic: topic}); status.Code(err) != codes.AlreadyExists {
		t.Errorf("CreateSubscription of a detached name = %v, want ALREADY_EXISTS", err)
	}
	if _, err := fx.client.SubscriptionAdminClient.Seek(ctx, &pubsubpb.SeekRequest{Subscription: sub,
		Target: &pubsubpb.SeekRequest_Time{Time: timestamppb.Now()}}); err != nil {
		t.Errorf("Seek to a time of a detached subscription = %v, want success (#1446)", err)
	}
	if _, err := fx.client.TopicAdminClient.DetachSubscription(ctx, &pubsubpb.DetachSubscriptionRequest{Subscription: sub}); err != nil {
		t.Errorf("a second DetachSubscription = %v, want success", err)
	}
	if err := fx.client.SubscriptionAdminClient.DeleteSubscription(ctx, &pubsubpb.DeleteSubscriptionRequest{Subscription: sub}); err != nil {
		t.Fatalf("DeleteSubscription of a detached subscription: %v", err)
	}
	if _, err := fx.client.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: sub}); status.Code(err) != codes.NotFound {
		t.Errorf("GetSubscription after delete = %v, want NOT_FOUND", err)
	}
	// The name is free again, and a new subscription gets only what is
	// published from now on.
	if _, err := fx.create(t, &pubsubpb.Subscription{Name: sub, Topic: topic}); err != nil {
		t.Fatalf("CreateSubscription after delete: %v", err)
	}
	if _, err := fx.client.TopicAdminClient.DetachSubscription(ctx, &pubsubpb.DetachSubscriptionRequest{Subscription: subName("absent")}); status.Code(err) != codes.NotFound {
		t.Errorf("DetachSubscription of an absent subscription = %v, want NOT_FOUND", err)
	}
}

// Over REST: POST :detach detaches, GET reads it detached, the project's
// list names it, :pull fails FAILED_PRECONDITION and DELETE forgets it.
func TestRESTDetachSubscription(t *testing.T) {
	up := newRESTUpstream(t)
	fx := newFixtureREST(t, up.addr())
	topic := fx.topic(t, "t")
	sub := subName("s")
	if _, err := fx.create(t, &pubsubpb.Subscription{Name: sub, Topic: topic}); err != nil {
		t.Fatal(err)
	}
	if code, body := fx.rest(t, http.MethodPost, "/v1/"+sub+":detach", "{}"); code != http.StatusOK {
		t.Fatalf("POST :detach = %d %v", code, body)
	}
	code, body := fx.rest(t, http.MethodGet, "/v1/"+sub, "")
	if code != http.StatusOK || body["detached"] != true || body["topic"] != deletedTopic {
		t.Errorf("GET after detach = %d %v", code, body)
	}
	code, body = fx.rest(t, http.MethodGet, "/v1/projects/"+project+"/subscriptions", "")
	subs, _ := body["subscriptions"].([]any)
	if code != http.StatusOK || len(subs) != 1 {
		t.Errorf("GET the list = %d %v, want the detached subscription", code, body)
	}
	if code, body := fx.rest(t, http.MethodPost, "/v1/"+sub+":pull", `{"maxMessages":1}`); code != http.StatusBadRequest ||
		body["error"].(map[string]any)["status"] != "FAILED_PRECONDITION" {
		t.Errorf(":pull of a detached subscription = %d %v", code, body)
	}
	if code, _ := fx.rest(t, http.MethodDelete, "/v1/"+sub, ""); code != http.StatusOK {
		t.Errorf("DELETE = %d", code)
	}
	if _, ok := fx.front.detachedSub(sub); ok {
		t.Error("DELETE kept the detached subscription")
	}
	for _, c := range up.calls() {
		t.Errorf("the emulator's REST API saw %s %s", c.method, c.path)
	}
}

// A detached subscription and a snapshot update survive a restart of the
// front with a state file.
func TestDetachedAndSnapshotsAreKeptInTheStateFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	f, err := New("127.0.0.1:1", func(string, ...any) {})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.KeepState(path); err != nil {
		t.Fatal(err)
	}
	sub := subName("d")
	at := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	f.mu.Lock()
	f.detached[sub] = &pubsubpb.Subscription{Name: sub, Topic: deletedTopic, Detached: true}
	f.snapshots["projects/p/snapshots/n"] = keptSnapshot{Labels: map[string]string{}, HasLabels: true, ExpireTime: at}
	f.mu.Unlock()
	f.persist()

	g, err := New("127.0.0.1:1", func(string, ...any) {})
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	if err := g.KeepState(path); err != nil {
		t.Fatal(err)
	}
	if d, ok := g.detachedSub(sub); !ok || !d.GetDetached() || d.GetTopic() != deletedTopic {
		t.Errorf("restored detached = %v, %v", d, ok)
	}
	if k := g.snapshots["projects/p/snapshots/n"]; !k.HasLabels || !k.ExpireTime.Equal(at) {
		t.Errorf("restored snapshot = %+v", k)
	}
}

// snapshotUpstream stands in for the emulator's snapshots, which the
// in-memory Pub/Sub does not serve; it refuses UpdateSnapshot as the
// emulator does.
type snapshotUpstream struct {
	pubsubpb.UnimplementedSubscriberServer
	mu    sync.Mutex
	snaps map[string]*pubsubpb.Snapshot
}

func (u *snapshotUpstream) CreateSnapshot(_ context.Context, r *pubsubpb.CreateSnapshotRequest) (*pubsubpb.Snapshot, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	s := &pubsubpb.Snapshot{Name: r.GetName(), Topic: "projects/" + project + "/topics/t",
		ExpireTime: timestamppb.New(time.Now().Add(7 * 24 * time.Hour)), Labels: r.GetLabels()}
	u.snaps[r.GetName()] = s
	return proto.Clone(s).(*pubsubpb.Snapshot), nil
}

func (u *snapshotUpstream) GetSnapshot(_ context.Context, r *pubsubpb.GetSnapshotRequest) (*pubsubpb.Snapshot, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	s, ok := u.snaps[r.GetSnapshot()]
	if !ok {
		return nil, status.Error(codes.NotFound, "Snapshot does not exist")
	}
	return proto.Clone(s).(*pubsubpb.Snapshot), nil
}

func (u *snapshotUpstream) ListSnapshots(context.Context, *pubsubpb.ListSnapshotsRequest) (*pubsubpb.ListSnapshotsResponse, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := &pubsubpb.ListSnapshotsResponse{}
	for _, s := range u.snaps {
		out.Snapshots = append(out.Snapshots, proto.Clone(s).(*pubsubpb.Snapshot))
	}
	return out, nil
}

func (u *snapshotUpstream) DeleteSnapshot(_ context.Context, r *pubsubpb.DeleteSnapshotRequest) (*emptypb.Empty, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if _, ok := u.snaps[r.GetSnapshot()]; !ok {
		return nil, status.Error(codes.NotFound, "Snapshot does not exist")
	}
	delete(u.snaps, r.GetSnapshot())
	return &emptypb.Empty{}, nil
}

// UpdateSnapshot, which the emulator refuses, is applied by the front:
// labels and an earlier expire_time read back on GetSnapshot, ListSnapshots
// and over REST, the emulator's own copy is untouched, a mask naming
// anything else and an expiry in the past or past the snapshot's are
// refused, and the snapshot is deleted when its kept expiry passes.
func TestUpdateSnapshot(t *testing.T) {
	up := &snapshotUpstream{snaps: map[string]*pubsubpb.Snapshot{}}
	srv := grpc.NewServer()
	pubsubpb.RegisterSubscriberServer(srv, up)
	ul, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ul) }()
	t.Cleanup(srv.Stop)
	f, err := New(ul.Addr().String(), func(string, ...any) {})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	sctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = f.Serve(sctx, l, time.Hour) }()
	t.Cleanup(func() { cancel(); <-done })
	conn, err := grpc.NewClient(l.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	fx := &fixture{front: f, addr: l.Addr().String(), conn: conn}
	c := pubsubpb.NewSubscriberClient(conn)
	ctx := context.Background()

	snap := "projects/" + project + "/snapshots/n"
	created, err := c.CreateSnapshot(ctx, &pubsubpb.CreateSnapshotRequest{Name: snap, Subscription: subName("s")})
	if err != nil {
		t.Fatal(err)
	}
	update := func(s *pubsubpb.Snapshot, paths ...string) (*pubsubpb.Snapshot, error) {
		s.Name = snap
		return c.UpdateSnapshot(ctx, &pubsubpb.UpdateSnapshotRequest{Snapshot: s, UpdateMask: &fieldmaskpb.FieldMask{Paths: paths}})
	}
	expire := f.clock.Now().Add(2 * time.Hour).Truncate(time.Second)
	got, err := update(&pubsubpb.Snapshot{Labels: map[string]string{"k": "v"}, ExpireTime: timestamppb.New(expire)}, "labels", "expireTime")
	if err != nil {
		t.Fatalf("UpdateSnapshot: %v", err)
	}
	if got.GetLabels()["k"] != "v" || !got.GetExpireTime().AsTime().Equal(expire) {
		t.Errorf("UpdateSnapshot answered %v", got)
	}
	read, err := c.GetSnapshot(ctx, &pubsubpb.GetSnapshotRequest{Snapshot: snap})
	if err != nil || read.GetLabels()["k"] != "v" || !read.GetExpireTime().AsTime().Equal(expire) {
		t.Errorf("GetSnapshot after the update = %v, %v", read, err)
	}
	list, err := c.ListSnapshots(ctx, &pubsubpb.ListSnapshotsRequest{Project: "projects/" + project})
	if err != nil || len(list.GetSnapshots()) != 1 || list.GetSnapshots()[0].GetLabels()["k"] != "v" {
		t.Errorf("ListSnapshots after the update = %v, %v", list, err)
	}
	if code, body := fx.rest(t, http.MethodGet, "/v1/"+snap, ""); code != http.StatusOK || body["labels"].(map[string]any)["k"] != "v" {
		t.Errorf("REST GET after the update = %d %v", code, body)
	}
	if code, body := fx.rest(t, http.MethodPatch, "/v1/"+snap, `{"snapshot":{"labels":{"k":"w"}},"updateMask":"labels"}`); code != http.StatusOK ||
		body["labels"].(map[string]any)["k"] != "w" {
		t.Errorf("REST PATCH = %d %v", code, body)
	}
	if code, body := fx.rest(t, http.MethodGet, "/v1/projects/"+project+"/snapshots", ""); code != http.StatusOK ||
		body["snapshots"].([]any)[0].(map[string]any)["labels"].(map[string]any)["k"] != "w" {
		t.Errorf("REST list = %d %v", code, body)
	}
	if raw := up.snaps[snap]; raw.GetLabels()["k"] != "" {
		t.Errorf("the emulator's snapshot = %v; the front should keep the update", raw)
	}
	for _, tc := range []struct {
		what  string
		s     *pubsubpb.Snapshot
		paths []string
	}{
		{"topic", &pubsubpb.Snapshot{Topic: "projects/" + project + "/topics/u"}, []string{"topic"}},
		{"no mask", &pubsubpb.Snapshot{}, nil},
		{"past", &pubsubpb.Snapshot{ExpireTime: timestamppb.New(time.Now().Add(-time.Hour))}, []string{"expire_time"}},
		{"after its expiry", &pubsubpb.Snapshot{ExpireTime: timestamppb.New(created.GetExpireTime().AsTime().Add(time.Hour))}, []string{"expire_time"}},
		{"bad labels", &pubsubpb.Snapshot{Labels: map[string]string{"UPPER": "x"}}, []string{"labels"}},
	} {
		if _, err := update(tc.s, tc.paths...); status.Code(err) != codes.InvalidArgument {
			t.Errorf("UpdateSnapshot(%s) = %v, want INVALID_ARGUMENT", tc.what, err)
		}
	}
	fx.advance(t, 3*time.Hour)
	if _, err := c.GetSnapshot(ctx, &pubsubpb.GetSnapshotRequest{Snapshot: snap}); status.Code(err) != codes.NotFound {
		t.Errorf("the snapshot after its kept expiry = %v, want it deleted", err)
	}
	if _, ok := f.snapshots[snap]; ok {
		t.Error("the front still keeps the expired snapshot's update")
	}
}
