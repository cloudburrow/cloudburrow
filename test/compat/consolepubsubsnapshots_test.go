//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/pubsub/v2"
	pubsubpb "cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// consoleSubscriptionActions reads a subscription's page from the console
// and returns its actions by id.
func consoleSubscriptionActions(t *testing.T, addr, project, sub string) map[string]consoleAction {
	t.Helper()
	code, body := consoleDo(t, addr, http.MethodGet,
		"/api/detail/pubsub-subscriptions?project="+project+"&name="+url.QueryEscape(sub), "")
	if code != http.StatusOK {
		t.Fatalf("console detail of %s = %d: %s", sub, code, body)
	}
	var d struct{ Actions []consoleAction }
	if err := json.Unmarshal([]byte(body), &d); err != nil {
		t.Fatalf("decode detail: %v: %s", err, body)
	}
	out := map[string]consoleAction{}
	for _, a := range d.Actions {
		out[a.ID] = a
	}
	return out
}

type consoleAction struct {
	ID, Label, Confirm string
	Fields             []struct{ Name, Default, Help string }
}

// pullAll pulls and acknowledges what the subscription delivers until want
// messages have arrived or the deadline passes, and returns their bodies.
// The emulator is another process, so its delivery after a Seek is awaited
// with a bound rather than assumed.
func pullAll(t *testing.T, ctx context.Context, c *pubsub.Client, sub string, want int, within time.Duration) []string {
	t.Helper()
	var got []string
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) && len(got) < want {
		pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		resp, err := c.SubscriptionAdminClient.Pull(pctx, &pubsubpb.PullRequest{
			Subscription: sub, MaxMessages: 10, ReturnImmediately: true})
		cancel()
		if err != nil && status.Code(err) != codes.DeadlineExceeded && pctx.Err() == nil {
			t.Fatalf("SDK Pull: %v", err)
		}
		var ids []string
		for _, m := range resp.GetReceivedMessages() {
			got = append(got, string(m.GetMessage().GetData()))
			ids = append(ids, m.GetAckId())
		}
		if len(ids) > 0 {
			if err := c.SubscriptionAdminClient.Acknowledge(ctx, &pubsubpb.AcknowledgeRequest{Subscription: sub, AckIds: ids}); err != nil {
				t.Fatalf("SDK Acknowledge: %v", err)
			}
			continue
		}
		time.Sleep(200 * time.Millisecond)
	}
	slices.Sort(got)
	return got
}

// TestConsolePubSubSnapshotsAndSeek (#787): a snapshot created on a
// subscription's page is listed by the official client's ListSnapshots and
// on the console's Snapshots screen and topic page; after the messages are
// acknowledged, the console's Seek to that snapshot makes the official
// client's pull receive them again; a console Seek to a time after them
// acknowledges them; and a snapshot deleted from the console is NOT_FOUND
// through the client. What the emulator refuses is refused with exactly the
// message its own client receives. Create snapshot asks for no labels, and
// the snapshot page offers no edit: this test asserts that the emulator still
// drops a snapshot's labels and still answers UpdateSnapshot UNIMPLEMENTED,
// with the message the page quotes, so an emulator that starts keeping them
// fails here and they are then offered.
func TestConsolePubSubSnapshotsAndSeek(t *testing.T) {
	h := New(t)
	addr := consoleAddr(t, h)
	ps := pubsubClient(t, h)
	ctx := h.Context()
	project := h.Project()
	tp := topic(t, h, ps, "console-snap")
	other := topic(t, h, ps, "console-snap-other")
	sub := subscription(t, h, ps, "console-snap-sub", tp)
	otherSub := subscription(t, h, ps, "console-snap-other-sub", other)
	snap := "projects/" + project + "/snapshots/console-snap"
	t.Cleanup(func() {
		_ = ps.SubscriptionAdminClient.DeleteSnapshot(context.Background(), &pubsubpb.DeleteSnapshotRequest{Snapshot: snap})
	})

	act := func(action string, values map[string]string) (int, string) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"Path": []string{sub}, "Action": action, "Values": values})
		return consoleDo(t, addr, http.MethodPost, "/api/actions/pubsub-subscriptions?project="+project, string(body))
	}
	publish := func(data string) {
		t.Helper()
		if _, err := ps.TopicAdminClient.Publish(ctx, &pubsubpb.PublishRequest{Topic: tp,
			Messages: []*pubsubpb.PubsubMessage{{Data: []byte(data)}}}); err != nil {
			t.Fatalf("SDK Publish: %v", err)
		}
	}

	// What the form leaves out, still what the emulator does: labels given to
	// CreateSnapshot are not kept.
	labelled := "projects/" + project + "/snapshots/console-snap-labels"
	if _, err := ps.SubscriptionAdminClient.CreateSnapshot(ctx, &pubsubpb.CreateSnapshotRequest{
		Name: labelled, Subscription: sub, Labels: map[string]string{"env": "dev"}}); err != nil {
		t.Fatalf("CreateSnapshot with labels: %v", err)
	}
	if got, err := ps.SubscriptionAdminClient.GetSnapshot(ctx, &pubsubpb.GetSnapshotRequest{Snapshot: labelled}); err != nil || len(got.GetLabels()) != 0 {
		t.Errorf("GetSnapshot of a snapshot created with labels = %v (%v); the Create snapshot form says the emulator drops them", got, err)
	}
	_, updErr := ps.SubscriptionAdminClient.UpdateSnapshot(ctx, &pubsubpb.UpdateSnapshotRequest{
		Snapshot: &pubsubpb.Snapshot{Name: labelled, Labels: map[string]string{"env": "dev"}}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"labels"}}})
	_ = ps.SubscriptionAdminClient.DeleteSnapshot(ctx, &pubsubpb.DeleteSnapshotRequest{Snapshot: labelled})

	acts := consoleSubscriptionActions(t, addr, project, sub)
	create, ok := acts["create-snapshot"]
	if !ok || len(create.Fields) != 1 || !strings.Contains(create.Fields[0].Help, "does not keep them") {
		t.Fatalf("the subscription page's Create snapshot = %+v; want only an ID, saying labels are not kept", acts)
	}
	if _, ok := acts["seek-snapshot"]; ok {
		t.Error("Seek to snapshot is offered before the topic has a snapshot")
	}
	if a := acts["seek-time"]; a.Confirm == "" {
		t.Errorf("Seek to time = %+v; want a confirmation", a)
	}

	// m1 is waiting when the snapshot is taken.
	publish("m1")
	if code, body := act("create-snapshot", map[string]string{"name": "console-snap"}); code != http.StatusOK {
		t.Fatalf("console Create snapshot = %d: %s", code, body)
	}
	var listed []string
	it := ps.SubscriptionAdminClient.ListSnapshots(ctx, &pubsubpb.ListSnapshotsRequest{Project: "projects/" + project})
	for _, s := range drain(t, "ListSnapshots", it.Next) {
		listed = append(listed, s.GetName())
	}
	if !contains(listed, snap) {
		t.Fatalf("the official client's ListSnapshots = %v; want the console's %s", listed, snap)
	}
	if got, err := ps.SubscriptionAdminClient.GetSnapshot(ctx, &pubsubpb.GetSnapshotRequest{Snapshot: snap}); err != nil || got.GetTopic() != tp {
		t.Errorf("GetSnapshot = %v (%v); want the topic %s", got, err, tp)
	}
	code, body := consoleDo(t, addr, http.MethodGet, "/api/resources/pubsub-snapshots?project="+project, "")
	if code != http.StatusOK || !strings.Contains(body, snap) {
		t.Errorf("the console's Snapshots screen = %d %s; want %s", code, body, snap)
	}
	code, body = consoleDo(t, addr, http.MethodGet, "/api/detail/pubsub?project="+project+"&name="+url.QueryEscape(tp), "")
	if code != http.StatusOK || !strings.Contains(body, `"id":"snapshots"`) || !strings.Contains(body, snap) {
		t.Errorf("the topic page = %d %s; want a Snapshots section listing %s", code, body, snap)
	}
	st, _ := status.FromError(updErr)
	code, body = consoleDo(t, addr, http.MethodGet, "/api/detail/pubsub-snapshots?project="+project+"&name="+url.QueryEscape(snap), "")
	if updErr == nil || st.Code() != codes.Unimplemented || code != http.StatusOK || !strings.Contains(body, st.Message()) || strings.Contains(body, `"edit"`) {
		t.Errorf("UpdateSnapshot = %v; the snapshot page (%d) must offer no edit and quote that refusal: %s", updErr, code, body)
	}

	// m1 and m2 are acknowledged; the subscription then has nothing.
	publish("m2")
	if got := pullAll(t, ctx, ps, sub, 2, 10*time.Second); !slices.Equal(got, []string{"m1", "m2"}) {
		t.Fatalf("before the seek the SDK pulled %v, want m1 and m2", got)
	}
	if got := pullAll(t, ctx, ps, sub, 1, 2*time.Second); len(got) != 0 {
		t.Fatalf("after acknowledging, the SDK still pulled %v", got)
	}

	// Seek to the snapshot, from the console: what it held unacknowledged
	// (m1) and what was published after it (m2) are redelivered.
	a, ok := consoleSubscriptionActions(t, addr, project, sub)["seek-snapshot"]
	if !ok || a.Confirm == "" || a.Fields[0].Default != "console-snap" {
		t.Fatalf("with a snapshot the page offers Seek to snapshot = %+v; want it prefilled and confirmed", a)
	}
	if code, body := act("seek-snapshot", map[string]string{"snapshot": "console-snap"}); code != http.StatusOK {
		t.Fatalf("console Seek to snapshot = %d: %s", code, body)
	}
	if got := pullAll(t, ctx, ps, sub, 2, 15*time.Second); !slices.Equal(got, []string{"m1", "m2"}) {
		t.Errorf("after the console's Seek to the snapshot the SDK pulled %v, want m1 and m2 again", got)
	}

	// Seek to a time after every message, from the console: they are marked
	// acknowledged, so a seek back to the snapshot is what replays them.
	publish("m3")
	at := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	if code, body := act("seek-time", map[string]string{"time": at}); code != http.StatusOK {
		t.Fatalf("console Seek to time = %d: %s", code, body)
	}
	if got := pullAll(t, ctx, ps, sub, 1, 3*time.Second); len(got) != 0 {
		t.Errorf("after a console Seek to a later time the SDK pulled %v, want nothing", got)
	}

	// A snapshot of another topic: the console is refused with the client's
	// own FAILED_PRECONDITION.
	otherSnap := snapshot(t, h, ps, "console-snap-other", otherSub)
	_, sdkErr := ps.SubscriptionAdminClient.Seek(ctx, &pubsubpb.SeekRequest{Subscription: sub,
		Target: &pubsubpb.SeekRequest_Snapshot{Snapshot: otherSnap}})
	sst, _ := status.FromError(sdkErr)
	code, body = act("seek-snapshot", map[string]string{"snapshot": "console-snap-other"})
	var refusal struct{ Error string }
	_ = json.Unmarshal([]byte(body), &refusal)
	if want := sst.Code().String() + ": " + sst.Message(); sdkErr == nil || code != http.StatusBadRequest || refusal.Error != want {
		t.Errorf("console Seek to another topic's snapshot = %d %s; want 400 with the client's own %q", code, body, want)
	}

	// Delete from the Snapshots screen: NOT_FOUND through the client.
	req := "/api/resources/pubsub-snapshots?project=" + project + "&name=" + url.QueryEscape(snap)
	if code, body := consoleDo(t, addr, http.MethodDelete, req, ""); code != http.StatusOK {
		t.Fatalf("console delete of %s = %d: %s", snap, code, body)
	}
	if _, err := ps.SubscriptionAdminClient.GetSnapshot(ctx, &pubsubpb.GetSnapshotRequest{Snapshot: snap}); status.Code(err) != codes.NotFound {
		t.Errorf("after the console's delete GetSnapshot = %v, want NOT_FOUND", err)
	}
}
