package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	pubsub "cloud.google.com/go/pubsub/v2"
	pubsubpb "cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"cloud.google.com/go/pubsub/v2/pstest"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// snapshotFake is pstest with snapshots. pstest serves topics, subscriptions,
// publish, pull and a Seek to a time, and answers every snapshot RPC and a
// Seek to a snapshot UNIMPLEMENTED; this serves those from a map, as the
// emulator answers them (measured against it, and asserted there by
// TestConsolePubSubSnapshotsAndSeek): a snapshot records its subscription's
// topic and an expiry, a duplicate is ALREADY_EXISTS, an absent one
// NOT_FOUND, and a Seek to a snapshot of another topic FAILED_PRECONDITION.
// A Seek to a snapshot is recorded rather than replayed: what it redelivers
// is the emulator's to show, and the compat test reads it.
type snapshotFake struct {
	*pstest.GServer

	mu    sync.Mutex
	snaps map[string]*pubsubpb.Snapshot
	seeks []*pubsubpb.SeekRequest
}

// newSnapshotFake serves a snapshotFake on a loopback port and returns its
// address, the fake and an official client of it.
func newSnapshotFake(t *testing.T, project string) (string, *snapshotFake, *pubsub.Client) {
	t.Helper()
	ps := pstest.NewServer()
	t.Cleanup(func() { _ = ps.Close() })
	f := &snapshotFake{GServer: &ps.GServer, snaps: map[string]*pubsubpb.Snapshot{}}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gs := grpc.NewServer()
	pubsubpb.RegisterPublisherServer(gs, f)
	pubsubpb.RegisterSubscriberServer(gs, f)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	addr := lis.Addr().String()
	c, err := pubsub.NewClient(context.Background(), project,
		option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return addr, f, c
}

func (f *snapshotFake) CreateSnapshot(ctx context.Context, req *pubsubpb.CreateSnapshotRequest) (*pubsubpb.Snapshot, error) {
	sub, err := f.GServer.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: req.GetSubscription()})
	if err != nil {
		return nil, status.Error(codes.NotFound, "Subscription does not exist")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.snaps[req.GetName()]; ok {
		return nil, status.Error(codes.AlreadyExists, "Snapshot already exists")
	}
	s := &pubsubpb.Snapshot{Name: req.GetName(), Topic: sub.GetTopic(),
		ExpireTime: timestamppb.New(time.Now().Add(7 * 24 * time.Hour))}
	f.snaps[s.Name] = s
	return s, nil
}

func (f *snapshotFake) GetSnapshot(_ context.Context, req *pubsubpb.GetSnapshotRequest) (*pubsubpb.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.snaps[req.GetSnapshot()]; ok {
		return s, nil
	}
	return nil, status.Error(codes.NotFound, "Snapshot does not exist")
}

func (f *snapshotFake) sorted(keep func(*pubsubpb.Snapshot) bool) []*pubsubpb.Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*pubsubpb.Snapshot
	for _, s := range f.snaps {
		if keep(s) {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (f *snapshotFake) ListSnapshots(_ context.Context, req *pubsubpb.ListSnapshotsRequest) (*pubsubpb.ListSnapshotsResponse, error) {
	return &pubsubpb.ListSnapshotsResponse{Snapshots: f.sorted(func(s *pubsubpb.Snapshot) bool {
		return strings.HasPrefix(s.Name, req.GetProject()+"/snapshots/")
	})}, nil
}

func (f *snapshotFake) ListTopicSnapshots(_ context.Context, req *pubsubpb.ListTopicSnapshotsRequest) (*pubsubpb.ListTopicSnapshotsResponse, error) {
	var names []string
	for _, s := range f.sorted(func(s *pubsubpb.Snapshot) bool { return s.Topic == req.GetTopic() }) {
		names = append(names, s.Name)
	}
	return &pubsubpb.ListTopicSnapshotsResponse{Snapshots: names}, nil
}

func (f *snapshotFake) DeleteSnapshot(_ context.Context, req *pubsubpb.DeleteSnapshotRequest) (*emptypb.Empty, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.snaps[req.GetSnapshot()]; !ok {
		return nil, status.Error(codes.NotFound, "Snapshot does not exist")
	}
	delete(f.snaps, req.GetSnapshot())
	return &emptypb.Empty{}, nil
}

func (f *snapshotFake) Seek(ctx context.Context, req *pubsubpb.SeekRequest) (*pubsubpb.SeekResponse, error) {
	name := req.GetSnapshot()
	if name == "" {
		f.mu.Lock()
		f.seeks = append(f.seeks, req)
		f.mu.Unlock()
		return f.GServer.Seek(ctx, req)
	}
	snap, err := f.GetSnapshot(ctx, &pubsubpb.GetSnapshotRequest{Snapshot: name})
	if err != nil {
		return nil, err
	}
	sub, err := f.GServer.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: req.GetSubscription()})
	if err != nil {
		return nil, err
	}
	if sub.GetTopic() != snap.GetTopic() {
		return nil, status.Errorf(codes.FailedPrecondition, "The subscription's topic %s is different from that of the snapshot %s",
			sub.GetTopic(), snap.GetTopic())
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seeks = append(f.seeks, req)
	return &pubsubpb.SeekResponse{}, nil
}

func (f *snapshotFake) lastSeek() *pubsubpb.SeekRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.seeks) == 0 {
		return nil
	}
	return f.seeks[len(f.seeks)-1]
}

// snapshotsConsole serves a console with the three Pub/Sub screens against a
// snapshotFake.
func snapshotsConsole(t *testing.T, project string) (*httptest.Server, *snapshotFake, *pubsub.Client) {
	t.Helper()
	addr, f, c := newSnapshotFake(t, project)
	srv := httptest.NewServer(console.New("127.0.0.1:0", nil,
		pubsubProvider{endpoint: addr}, pubsubSubscriptionsProvider{endpoint: addr},
		pubsubSnapshotsProvider{endpoint: addr}).Handler())
	t.Cleanup(srv.Close)
	return srv, f, c
}

func subscriptionAct(t *testing.T, srv *httptest.Server, project, sub, action string, values map[string]string) (int, string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"Path": []string{sub}, "Action": action, "Values": values})
	resp, err := http.Post(srv.URL+"/api/actions/pubsub-subscriptions?project="+project, "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out)
}

func actionsByID(d console.Detail) map[string]console.Action {
	out := map[string]console.Action{}
	for _, a := range d.Actions {
		out[a.ID] = a
	}
	return out
}

func sdkSnapshots(t *testing.T, c *pubsub.Client, project string) []string {
	t.Helper()
	var out []string
	it := c.SubscriptionAdminClient.ListSnapshots(context.Background(), &pubsubpb.ListSnapshotsRequest{Project: "projects/" + project})
	for {
		s, err := it.Next()
		if err == iterator.Done {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, s.GetName())
	}
}

// A subscription's page offers Create snapshot, and Seek to a time; Seek to
// snapshot once its topic has one. Each goes through the official client: a
// snapshot created from the page is what the client's ListSnapshots returns,
// a Seek names the snapshot or the time given, and each Seek carries the
// confirmation that asks for the subscription's name back. Create snapshot
// asks for no labels, which the emulator drops. What the API refuses — a
// duplicate, a snapshot of another topic — is refused with its own message,
// and a time that is not RFC 3339 is refused before any call (#787).
func TestASubscriptionPageCreatesSnapshotsAndSeeks(t *testing.T) {
	ctx := context.Background()
	const project = "snap-proj"
	srv, f, c := snapshotsConsole(t, project)
	topic := "projects/" + project + "/topics/orders"
	other := "projects/" + project + "/topics/other"
	sub := "projects/" + project + "/subscriptions/orders-sub"
	otherSub := "projects/" + project + "/subscriptions/other-sub"
	for _, tp := range []string{topic, other} {
		if _, err := c.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: tp}); err != nil {
			t.Fatal(err)
		}
	}
	for s, tp := range map[string]string{sub: topic, otherSub: other} {
		if _, err := c.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{Name: s, Topic: tp}); err != nil {
			t.Fatal(err)
		}
	}
	detail := func(name string) console.Detail {
		var d console.Detail
		getJSON200(t, srv.URL+"/api/detail/pubsub-subscriptions?project="+project+"&name="+url.QueryEscape(name), &d)
		return d
	}

	acts := actionsByID(detail(sub))
	if len(acts) != 2 || acts[actCreateSnapshot].Label != "Create snapshot" || acts[actSeekTime].Label != "Seek to time" {
		t.Fatalf("a subscription whose topic has no snapshot offers %+v; want Create snapshot and Seek to time", acts)
	}
	create := acts[actCreateSnapshot]
	if len(create.Fields) != 1 || create.Fields[0].Name != "name" || !strings.Contains(create.Fields[0].Help, pubsubSnapshotLabelsDropped) {
		t.Errorf("Create snapshot fields = %+v; want only the ID, its help saying labels are dropped", create.Fields)
	}
	if create.Confirm != "" || create.Destructive {
		t.Errorf("Create snapshot asks for confirmation: %+v", create)
	}
	seekTime := acts[actSeekTime]
	if seekTime.Confirm != seekTimeConfirm || !strings.Contains(seekTime.Fields[0].Help, "redelivered") ||
		!strings.Contains(seekTime.Fields[0].Help, "does not retain acknowledged messages") {
		t.Errorf("Seek to time = %+v; want the confirmation, and help saying what is redelivered and what is retained", seekTime)
	}
	if _, err := time.Parse(time.RFC3339, seekTime.Fields[0].Default); err != nil {
		t.Errorf("Seek to time is prefilled with %q, not an RFC 3339 time", seekTime.Fields[0].Default)
	}

	if code, out := subscriptionAct(t, srv, project, sub, actCreateSnapshot, map[string]string{"name": "orders-snap"}); code != http.StatusOK {
		t.Fatalf("Create snapshot = %d: %s", code, out)
	}
	snap := "projects/" + project + "/snapshots/orders-snap"
	if got := sdkSnapshots(t, c, project); len(got) != 1 || got[0] != snap {
		t.Fatalf("the official client's ListSnapshots = %v, want [%s]", got, snap)
	}
	if code, out := subscriptionAct(t, srv, project, sub, actCreateSnapshot, map[string]string{"name": "orders-snap"}); code != http.StatusBadRequest ||
		!strings.Contains(out, "Snapshot already exists") {
		t.Errorf("a duplicate Create snapshot = %d %s; want 400 with the API's message", code, out)
	}

	acts = actionsByID(detail(sub))
	seekSnap, ok := acts[actSeekSnapshot]
	if !ok || seekSnap.Confirm != seekSnapshotConfirm || seekSnap.Fields[0].Default != "orders-snap" ||
		!strings.Contains(seekSnap.Fields[0].Help, "orders-snap") {
		t.Fatalf("with a snapshot of its topic the page offers %+v; want Seek to snapshot prefilled with orders-snap and confirmed", acts)
	}
	if code, out := subscriptionAct(t, srv, project, sub, actSeekSnapshot, map[string]string{"snapshot": "orders-snap"}); code != http.StatusOK {
		t.Fatalf("Seek to snapshot = %d: %s", code, out)
	}
	if s := f.lastSeek(); s.GetSubscription() != sub || s.GetSnapshot() != snap {
		t.Errorf("the Seek sent was %v; want %s to %s", s, sub, snap)
	}

	// The other topic's subscription is not offered it, and a request that
	// names it anyway is refused by the API with its own message.
	if _, ok := actionsByID(detail(otherSub))[actSeekSnapshot]; ok {
		t.Error("a subscription of another topic is offered Seek to snapshot")
	}
	if _, err := c.SubscriptionAdminClient.CreateSnapshot(ctx, &pubsubpb.CreateSnapshotRequest{
		Name: "projects/" + project + "/snapshots/other-snap", Subscription: otherSub}); err != nil {
		t.Fatal(err)
	}
	if code, out := subscriptionAct(t, srv, project, sub, actSeekSnapshot, map[string]string{"snapshot": "other-snap"}); code != http.StatusBadRequest ||
		!strings.Contains(out, "FailedPrecondition") || !strings.Contains(out, "is different from that of the snapshot") {
		t.Errorf("a Seek to another topic's snapshot = %d %s; want 400 with the API's FAILED_PRECONDITION", code, out)
	}
	if code, out := subscriptionAct(t, srv, project, sub, actSeekSnapshot, map[string]string{"snapshot": "projects/elsewhere/snapshots/x"}); code != http.StatusBadRequest {
		t.Errorf("a Seek to another project's snapshot = %d %s; want 400", code, out)
	}

	// Seek to a time: a message waiting before it is marked acknowledged.
	if _, err := c.TopicAdminClient.Publish(ctx, &pubsubpb.PublishRequest{Topic: topic,
		Messages: []*pubsubpb.PubsubMessage{{Data: []byte("m1")}}}); err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	if code, out := subscriptionAct(t, srv, project, sub, actSeekTime, map[string]string{"time": at}); code != http.StatusOK {
		t.Fatalf("Seek to time = %d: %s", code, out)
	}
	if s := f.lastSeek(); s.GetSubscription() != sub || s.GetTime().AsTime().UTC().Format(time.RFC3339) != at {
		t.Errorf("the Seek sent was %v; want %s to %s", s, sub, at)
	}
	pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if resp, err := c.SubscriptionAdminClient.Pull(pctx, &pubsubpb.PullRequest{Subscription: sub, MaxMessages: 10, ReturnImmediately: true}); err == nil &&
		len(resp.GetReceivedMessages()) != 0 {
		t.Errorf("after a Seek to a time after it, the message is still delivered: %v", resp.GetReceivedMessages())
	}
	for _, bad := range []string{"tomorrow", "2026-01-02 15:04:05", ""} {
		if code, out := subscriptionAct(t, srv, project, sub, actSeekTime, map[string]string{"time": bad}); code != http.StatusBadRequest ||
			!strings.Contains(out, "RFC 3339") {
			t.Errorf("Seek to time %q = %d %s; want 400 naming RFC 3339", bad, code, out)
		}
	}

	// Another project's subscription is not reached through this one.
	if code, _ := subscriptionAct(t, srv, "elsewhere", sub, actCreateSnapshot, map[string]string{"name": "x-snap"}); code != http.StatusBadRequest {
		t.Errorf("an action on another project's subscription = %d; want 400", code)
	}

	// A subscription whose topic was deleted is offered none.
	if err := c.TopicAdminClient.DeleteTopic(ctx, &pubsubpb.DeleteTopicRequest{Topic: other}); err != nil {
		t.Fatal(err)
	}
	if acts := detail(otherSub).Actions; len(acts) != 0 {
		t.Errorf("a subscription whose topic was deleted offers %+v", acts)
	}
}

// The Snapshots screen lists every snapshot of the project with its topic and
// expiry, opens each one — the subscriptions that can seek to it, and edit
// not offered with the emulator's reason — and deletes it, confirmed on the
// page, so the official client's GetSnapshot answers NOT_FOUND. A topic's
// page lists its snapshots, each opening its page. There is no create form
// on this screen, and another project's snapshot is refused (#787).
func TestTheSnapshotsScreenListsOpensAndDeletes(t *testing.T) {
	ctx := context.Background()
	const project = "snap-proj"
	srv, _, c := snapshotsConsole(t, project)
	topic := "projects/" + project + "/topics/orders"
	sub := "projects/" + project + "/subscriptions/orders-sub"
	snap := "projects/" + project + "/snapshots/orders-snap"
	if _, err := c.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: topic}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{Name: sub, Topic: topic}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SubscriptionAdminClient.CreateSnapshot(ctx, &pubsubpb.CreateSnapshotRequest{Name: snap, Subscription: sub}); err != nil {
		t.Fatal(err)
	}

	var services struct {
		Services []struct {
			ID             string
			Detail, Delete bool
			Create         any
		}
	}
	getJSON200(t, srv.URL+"/api/services", &services)
	advertised := false
	for _, s := range services.Services {
		if s.ID == "pubsub-snapshots" {
			advertised = s.Detail && s.Delete && (s.Create == nil || s.Create == false)
		}
	}
	if !advertised {
		t.Fatalf("pubsub-snapshots is not advertised with detail and delete and no create: %+v", services)
	}

	var listing console.Listing
	getJSON200(t, srv.URL+"/api/resources/pubsub-snapshots?project="+project, &listing)
	if len(listing.Items) != 1 || listing.Items[0].Name != snap || listing.Items[0].Fields["Topic"] != topic {
		t.Fatalf("snapshots listing = %+v, want %s of %s", listing.Items, snap, topic)
	}
	if _, err := time.Parse(time.RFC3339, listing.Items[0].Fields["Expires"]); err != nil {
		t.Errorf("the expiry column reads %q", listing.Items[0].Fields["Expires"])
	}

	var d console.Detail
	getJSON200(t, srv.URL+"/api/detail/pubsub-snapshots?project="+project+"&name="+url.QueryEscape(snap), &d)
	if d.Edit != nil {
		t.Error("a snapshot page offers an edit, which the emulator's UpdateSnapshot refuses")
	}
	props := map[string]string{}
	for _, p := range d.Summary {
		props[p.Label] = p.Value
	}
	if props["Snapshot"] != snap || props["Topic"] != topic {
		t.Errorf("snapshot summary = %v", props)
	}
	var subsListed, editReason bool
	for _, sec := range d.Sections {
		for _, it := range sec.Listing.Items {
			subsListed = subsListed || (it.Name == sub && strings.Contains(it.Link, "/pubsub/subscriptions/"))
		}
		for _, g := range sec.Groups {
			for _, p := range g.Properties {
				editReason = editReason || (p.Label == "Edit" && strings.Contains(p.Value, pubsubSnapshotEditRefusal))
			}
		}
	}
	if !subsListed || !editReason {
		t.Errorf("the snapshot page does not list %s linked, or say why edit is not offered: %+v", sub, d.Sections)
	}

	// The topic page's Snapshots section, from ListTopicSnapshots.
	var td console.Detail
	getJSON200(t, srv.URL+"/api/detail/pubsub?project="+project+"&name="+url.QueryEscape(topic), &td)
	var listed bool
	for _, sec := range td.Sections {
		if sec.ID == "snapshots" {
			listed = len(sec.Listing.Items) == 1 && sec.Listing.Items[0].Name == snap &&
				sec.Listing.Items[0].Link == "/pubsub/snapshots/"+url.PathEscape(snap)+"?project="+project
		}
	}
	if !listed {
		t.Errorf("the topic page does not list its snapshot, linked to its page: %+v", td.Sections)
	}

	if code, out := consoleDelete(t, srv, "pubsub-snapshots", project, "projects/elsewhere/snapshots/orders-snap"); code != http.StatusBadRequest ||
		!strings.Contains(out, "is not a snapshot of project") {
		t.Errorf("deleting another project's snapshot = %d %s", code, out)
	}
	if code, out := consoleDelete(t, srv, "pubsub-snapshots", project, snap); code != http.StatusOK {
		t.Fatalf("console delete = %d: %s", code, out)
	}
	if _, err := c.SubscriptionAdminClient.GetSnapshot(ctx, &pubsubpb.GetSnapshotRequest{Snapshot: snap}); status.Code(err) != codes.NotFound {
		t.Errorf("after a console delete the client's GetSnapshot = %v, want NOT_FOUND", err)
	}
	if code, out := consoleDelete(t, srv, "pubsub-snapshots", project, snap); code != http.StatusBadRequest || !strings.Contains(out, "Snapshot does not exist") {
		t.Errorf("a second delete = %d %s; want 400 with the API's NOT_FOUND message", code, out)
	}
	var none console.Listing
	getJSON200(t, srv.URL+"/api/resources/pubsub-snapshots?project="+project, &none)
	if len(none.Items) != 0 || none.Note == "" {
		t.Errorf("after the delete the screen lists %+v, note %q", none.Items, none.Note)
	}
}
