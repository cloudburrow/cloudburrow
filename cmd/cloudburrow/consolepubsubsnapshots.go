package main

// Pub/Sub snapshots and Seek (#787).
//
// Snapshots and Seek are verified with the official client, and the console
// had no way to reach them: no screen listed a snapshot, and a subscription
// could not be replayed. This adds a Snapshots screen (list, detail, delete),
// Create snapshot and Seek on a subscription's page, and a Snapshots section
// on a topic's page. Every call is the official pubsub/v2 admin client's
// against the emulator, the call an application makes, so a refusal is the
// emulator's own and is shown as it came.
//
// What the pinned emulator does, measured against it and asserted by
// TestConsolePubSubSnapshotsAndSeek:
//   - CreateSnapshot answers with the snapshot and an expiry, and does not
//     keep labels: a snapshot created with labels reads back with none. The
//     form therefore asks for no labels and its help says why.
//   - UpdateSnapshot is UNIMPLEMENTED (TestPubSubUpdateSnapshotIsRefused), so
//     a snapshot page offers no edit and says so.
//   - Seek to a snapshot marks unacknowledged the messages the snapshot held
//     unacknowledged and every message published after it, so they are
//     redelivered; a snapshot of another topic is refused FAILED_PRECONDITION.
//   - Seek to a time after every message acknowledges them all. Seeking back
//     in time redelivers what the subscription still retains from after it,
//     which for acknowledged messages means only with retain_acked_messages
//     on: the API's contract, seen against the emulator while this was built
//     and not asserted by a test, so the form's help says it and claims no
//     more.

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	vkit "cloud.google.com/go/pubsub/v2/apiv1"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/api/iterator"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

const (
	actCreateSnapshot = "create-snapshot"
	actSeekSnapshot   = "seek-snapshot"
	actSeekTime       = "seek-time"
)

// pubsubSnapshotEditRefusal is the emulator's answer to UpdateSnapshot, word
// for word, as TestPubSubUpdateSnapshotIsRefused asserts it.
const pubsubSnapshotEditRefusal = "Method google.pubsub.v1.Subscriber/UpdateSnapshot is unimplemented"

// pubsubSnapshotLabelsDropped is why Create snapshot asks for no labels.
const pubsubSnapshotLabelsDropped = "Labels are not asked for: this emulator's CreateSnapshot accepts them " +
	"and does not keep them, so a snapshot created with labels reads back with none."

// snapshotIDPattern is the resource ID rule Pub/Sub applies to a snapshot
// name, as it does to topics and subscriptions: 3-255 characters, starting
// with a letter.
const snapshotIDPattern = `^[A-Za-z][A-Za-z0-9._~%+\-]{2,254}$`

// pubsubSnapshotsProvider is the Pub/Sub Snapshots screen (#787).
type pubsubSnapshotsProvider struct{ endpoint string }

func (pubsubSnapshotsProvider) ID() string    { return "pubsub-snapshots" }
func (pubsubSnapshotsProvider) Title() string { return "Pub/Sub snapshots" }

func (p pubsubSnapshotsProvider) topics() pubsubProvider { return pubsubProvider(p) }

// snapshotExpiry is the expiry column: when the emulator will drop the
// snapshot.
func snapshotExpiry(s *pubsubpb.Snapshot) string {
	if s.GetExpireTime() == nil {
		return "—"
	}
	return s.GetExpireTime().AsTime().UTC().Format(time.RFC3339)
}

// snapshotLink is a snapshot's page, for a row on another screen.
func snapshotLink(project, name string) string {
	return "/pubsub/snapshots/" + url.PathEscape(name) + "?project=" + url.QueryEscape(project)
}

func (p pubsubSnapshotsProvider) List(ctx context.Context, project string) (console.Listing, error) {
	out := console.Listing{
		Columns:    []string{"Topic", "Expires"},
		NameColumn: "Snapshot",
		Noun:       "snapshots",
	}
	if project == "" {
		out.Prompt = "Pub/Sub lists snapshots per project. Choose one in the toolbar."
		return out, nil
	}
	c, err := p.topics().client(ctx, project)
	if err != nil {
		return console.Listing{}, fmt.Errorf("connect to Pub/Sub: %w", err)
	}
	defer func() { _ = c.Close() }()

	it := c.SubscriptionAdminClient.ListSnapshots(ctx, &pubsubpb.ListSnapshotsRequest{Project: "projects/" + project})
	for {
		s, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return console.Listing{}, fmt.Errorf("list snapshots: %w", err)
		}
		out.Items = append(out.Items, console.Resource{Name: s.GetName(), Fields: map[string]string{
			"Topic":   s.GetTopic(),
			"Expires": snapshotExpiry(s),
		}})
	}
	out.Total = len(out.Items)
	if out.Total == 0 {
		out.Note = "A snapshot is created from a subscription's page, with Create snapshot."
	}
	return out, nil
}

// Detail implements console.Driller for one snapshot.
func (p pubsubSnapshotsProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) > 1 {
		return console.DeeperThan(1, path), nil
	}
	if project == "" {
		return console.Detail{Prompt: "Choose a project in the toolbar."}, nil
	}
	name := path[0]
	if !strings.HasPrefix(name, "projects/"+project+"/snapshots/") {
		return console.Detail{Unavailable: fmt.Sprintf("%s is not a snapshot of project %s", name, project)}, nil
	}
	c, err := p.topics().client(ctx, project)
	if err != nil {
		return console.Detail{Unavailable: "cannot reach Pub/Sub: " + err.Error()}, nil
	}
	defer func() { _ = c.Close() }()

	s, err := c.SubscriptionAdminClient.GetSnapshot(ctx, &pubsubpb.GetSnapshotRequest{Snapshot: name})
	if err != nil {
		return console.Detail{}, err
	}

	// The subscriptions that can seek to it: Seek requires the
	// subscription's topic to be the snapshot's.
	subs := console.Listing{
		Columns: []string{}, NameColumn: "Subscription", Noun: "subscriptions",
	}
	if t := s.GetTopic(); t != "" && t != deletedTopic {
		it := c.TopicAdminClient.ListTopicSubscriptions(ctx, &pubsubpb.ListTopicSubscriptionsRequest{Topic: t})
		for {
			n, err := it.Next()
			if errors.Is(err, iterator.Done) {
				break
			}
			if err != nil {
				subs.Unavailable = "listing the topic's subscriptions: " + err.Error()
				break
			}
			subs.Items = append(subs.Items, console.Resource{
				Name: n, Link: "/pubsub/subscriptions/" + url.PathEscape(n) + "?project=" + url.QueryEscape(project),
			})
		}
	}
	subs.Total = len(subs.Items)
	subs.Note = "Seek to this snapshot from a subscription's page. Only subscriptions of the snapshot's topic can seek to it."

	config := []console.Property{
		{Label: "Edit", Value: "Not offered: this emulator's UpdateSnapshot answers UNIMPLEMENTED (\"" +
			pubsubSnapshotEditRefusal + "\"), so a snapshot's labels and expiry cannot be changed."},
	}
	groups := []console.PropertyGroup{{Heading: "Snapshot", Properties: config}}
	if len(s.GetLabels()) > 0 {
		groups = append(groups, console.PropertyGroup{Heading: "Labels", Properties: sortedPairs(s.GetLabels())})
	}

	return console.Detail{
		Summary: []console.Property{
			{Label: "Snapshot", Value: s.GetName()},
			{Label: "Topic", Value: orDash(s.GetTopic())},
			{Label: "Expires", Value: snapshotExpiry(s)},
		},
		Sections: []console.Section{
			{ID: "subscriptions", Label: "Subscriptions", Listing: subs},
			{ID: "configuration", Label: "Configuration", Kind: console.KindProperties, Groups: groups},
		},
	}, nil
}

// Delete implements console.Deleter through the official client's
// DeleteSnapshot. A snapshot of another project is refused before any call.
func (p pubsubSnapshotsProvider) Delete(ctx context.Context, project, name string) error {
	if !strings.HasPrefix(name, "projects/"+project+"/snapshots/") {
		return fmt.Errorf("%s is not a snapshot of project %s", name, project)
	}
	c, err := p.topics().client(ctx, project)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	return c.SubscriptionAdminClient.DeleteSnapshot(ctx, &pubsubpb.DeleteSnapshotRequest{Snapshot: name})
}

// topicSnapshots lists a topic's snapshots through ListTopicSnapshots.
func topicSnapshots(ctx context.Context, tc *vkit.TopicAdminClient, topic string) ([]string, error) {
	var out []string
	it := tc.ListTopicSnapshots(ctx, &pubsubpb.ListTopicSnapshotsRequest{Topic: topic})
	for {
		n, err := it.Next()
		if errors.Is(err, iterator.Done) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
}

// topicSnapshotsSection is the Snapshots section of a topic's page, from
// ListTopicSnapshots; each row opens the snapshot's page.
func topicSnapshotsSection(ctx context.Context, tc *vkit.TopicAdminClient, project, topic string) console.Section {
	l := console.Listing{Columns: []string{}, NameColumn: "Snapshot", Noun: "snapshots"}
	names, err := topicSnapshots(ctx, tc, topic)
	if err != nil {
		l.Unavailable = "listing snapshots: " + err.Error()
	}
	for _, n := range names {
		l.Items = append(l.Items, console.Resource{Name: n, Link: snapshotLink(project, n)})
	}
	l.Total = len(l.Items)
	if l.Total == 0 && l.Unavailable == "" {
		l.Note = "This topic has no snapshots. A snapshot is created from one of its subscriptions' pages."
	}
	return console.Section{ID: "snapshots", Label: "Snapshots", Listing: l}
}

// seekSnapshotConfirm and seekTimeConfirm are what a Seek puts at risk,
// shown in the confirmation that asks for the subscription's name back.
const (
	seekSnapshotConfirm = "Seek changes which messages this subscription delivers: the messages the snapshot " +
		"held unacknowledged, and every message published after it, are redelivered, and every other message " +
		"is marked acknowledged."
	seekTimeConfirm = "Seek changes which messages this subscription delivers: messages published before the " +
		"time are marked acknowledged, and those published after it that the subscription still retains are " +
		"redelivered."
)

// DetailActions implements console.PathActor for a subscription's page:
// Create snapshot, Seek to one of its topic's snapshots or to a time, and
// Advance clock (CloudBurrow extension, #1040; consolepubsubclock.go).
//
// Only Advance clock is offered on a subscription whose topic was deleted or
// that was detached, which no longer receives its topic's messages: a
// snapshot of it holds nothing, and it has nothing to replay.
func (p pubsubSubscriptionsProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if project == "" || len(path) != 1 || !strings.HasPrefix(path[0], "projects/"+project+"/subscriptions/") {
		return nil
	}
	c, err := p.topics().client(ctx, project)
	if err != nil {
		return nil
	}
	defer func() { _ = c.Close() }()
	s, err := c.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: path[0]})
	if err != nil {
		return nil
	}
	if s.GetTopic() == deletedTopic || s.GetDetached() {
		return []console.Action{advanceClockAction()}
	}

	actions := []console.Action{{ID: actCreateSnapshot, Label: "Create snapshot", Fields: []console.Field{{
		Name: "name", Label: "Snapshot ID", Type: "text", Required: true, Pattern: snapshotIDPattern,
		Help: "3-255 characters, starting with a letter. The snapshot keeps this subscription's " +
			"unacknowledged messages, and every message published to its topic after it, so a seek can replay them. " +
			pubsubSnapshotLabelsDropped,
	}}}}

	// Seek to a snapshot only when the topic has one: a Seek to a snapshot of
	// another topic is refused.
	if snaps, err := topicSnapshots(ctx, c.TopicAdminClient, s.GetTopic()); err == nil && len(snaps) > 0 {
		ids := make([]string, len(snaps))
		for i, n := range snaps {
			ids[i] = lastSegment(n)
		}
		actions = append(actions, console.Action{
			ID: actSeekSnapshot, Label: "Seek to snapshot", Confirm: seekSnapshotConfirm,
			Fields: []console.Field{{
				Name: "snapshot", Label: "Snapshot ID", Type: "text", Required: true, Default: ids[0],
				Help: "One of this topic's snapshots: " + strings.Join(ids, ", ") + ". " +
					"Changes which messages are redelivered: what the snapshot held unacknowledged, and everything published after it.",
			}},
		})
	}

	retained := "This subscription does not retain acknowledged messages, so seeking back replays only the unacknowledged ones."
	if s.GetRetainAckedMessages() {
		retained = "This subscription retains acknowledged messages, so seeking back replays them too."
	}
	actions = append(actions, console.Action{
		ID: actSeekTime, Label: "Seek to time", Confirm: seekTimeConfirm,
		Fields: []console.Field{{
			Name: "time", Label: "Time (RFC 3339)", Type: "text", Required: true,
			Default: time.Now().UTC().Truncate(time.Second).Format(time.RFC3339),
			Help: "Such as 2026-01-02T15:04:05Z. Changes which messages are redelivered: those published before it are " +
				"marked acknowledged, and those after it are redelivered. A time in the future acknowledges every message. " +
				retained,
		}},
	})
	return append(actions, advanceClockAction())
}

// ActAt implements console.PathActor.
func (p pubsubSubscriptionsProvider) ActAt(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	if len(path) != 1 {
		return errors.New("Pub/Sub subscription actions apply to one subscription")
	}
	sub := path[0]
	if !strings.HasPrefix(sub, "projects/"+project+"/subscriptions/") {
		return fmt.Errorf("%s is not a subscription of project %s", sub, project)
	}
	c, err := p.topics().client(ctx, project)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()

	switch action {
	case actCreateSnapshot:
		id := strings.TrimSpace(values["name"])
		if id == "" {
			return errors.New("snapshot ID is required")
		}
		_, err := c.SubscriptionAdminClient.CreateSnapshot(ctx, &pubsubpb.CreateSnapshotRequest{
			Name: "projects/" + project + "/snapshots/" + id, Subscription: sub,
		})
		return err

	case actSeekSnapshot:
		snap := strings.TrimSpace(values["snapshot"])
		if snap == "" {
			return errors.New("snapshot ID is required")
		}
		if !strings.Contains(snap, "/") {
			snap = "projects/" + project + "/snapshots/" + snap
		} else if !strings.HasPrefix(snap, "projects/"+project+"/snapshots/") {
			return fmt.Errorf("%s is not a snapshot of project %s", snap, project)
		}
		_, err := c.SubscriptionAdminClient.Seek(ctx, &pubsubpb.SeekRequest{
			Subscription: sub, Target: &pubsubpb.SeekRequest_Snapshot{Snapshot: snap},
		})
		return err

	case actSeekTime:
		raw := strings.TrimSpace(values["time"])
		at, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			return fmt.Errorf("time: %q is not an RFC 3339 time such as 2026-01-02T15:04:05Z", raw)
		}
		_, err = c.SubscriptionAdminClient.Seek(ctx, &pubsubpb.SeekRequest{
			Subscription: sub, Target: &pubsubpb.SeekRequest_Time{Time: timestamppb.New(at)},
		})
		return err

	default:
		return fmt.Errorf("unknown action %q on a subscription", action)
	}
}

var (
	_ console.Driller   = pubsubSnapshotsProvider{}
	_ console.Deleter   = pubsubSnapshotsProvider{}
	_ console.PathActor = pubsubSubscriptionsProvider{}
)
