package pubsubfront

// The emulator restarting beneath the front (#1608).
//
// Google's emulator keeps every message a subscription was sent until the
// subscription is deleted (#1603), so under the bounded heap `cloudburrow
// up` gives it, it fills, exits on OutOfMemoryError and its container
// restarts, empty: every topic, subscription, schema and snapshot is gone.
// The front's container goes on, and so does its state file on the pod's
// emptyDir (state.go). So the front keeps the definitions too: every
// topic, subscription (its whole configuration, as the emulator stores it),
// AVRO schema with its revisions, and snapshot the emulator holds, in the
// state file, and when the emulator comes back it creates them again before
// it serves another call. What the front already kept for itself (IAM
// policies, updated expiration policies and labels, detached subscriptions,
// snapshot updates, Protocol Buffer schemas and the topics' bindings to
// them) needs nothing: it never was the emulator's.
//
// The definitions are kept as the emulator holds them, read from it after
// every call that changed one succeeded (refresh), before the call is
// answered, so a client that saw its create succeed finds it after a
// restart; and every sweep interval each project's are read whole again
// (reconcile), which also takes in what a front from before #1608 never
// kept. The reads are serialised (defsMu), so a later read is never
// overwritten by an earlier one, and one that began before the emulator
// ended is dropped (gen).
//
// The front watches the emulator's process as the BigQuery front does
// (internal/bigqueryfront/restart.go, #1016): it holds an idle connection to
// the emulator's port, and the connection closing, or the port refusing,
// is the process ending. From then until the definitions are created again
// every call waits (waitServing), at most restoreWait, so no client sees
// the emptied emulator. When the front itself starts, the emulator may be
// the one it left or a new one (both containers restarted); it is taken as
// new, and the definitions created again, when it holds none of them.
//
// What cannot come back: the messages. A message published before the
// restart and not yet acknowledged is lost with the emulator's memory, and
// so is the backlog a snapshot held: Google's at-least-once delivery
// promises redelivery of an unacknowledged message, and covers nothing of
// an emulator that crashed. A schema's revisions are committed again in
// order, but the emulator gives each a new revision ID and creation time;
// a topic bound to a range of revisions is bound to the new IDs of the same
// revisions. A snapshot is taken again from a subscription to its topic,
// so it holds no backlog; its expiry is kept no later than it was.
//
// Not done (#1608): stopping the growth itself by deleting and creating
// again a subscription whose messages are all acknowledged. It cannot be
// made invisible. The front cannot know a subscription's backlog is empty
// (the emulator reports none, and a Pull to look leases messages, counts a
// delivery attempt and reorders them); deleting it ends every open
// StreamingPull, which the official clients treat as fatal NOT_FOUND, and a
// subscriber normally keeps one open; a message published between the
// delete and the create would never reach it; and it would lose what the
// emulator ties to the subscription: a Seek to a time before the recreate
// (with retain_acked_messages), the backlog snapshots of it hold, ordering
// keys' state and dead-letter delivery attempts.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net"
	"os"
	"sort"
	"strings"
	"time"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/cloudburrow/cloudburrow/internal/frontstate"
)

const (
	// restoreWait is the longest a call waits for the definitions to be
	// created again; past it, it goes on to the emulator as it is.
	restoreWait = 2 * time.Minute
)

// definitions are the resources the emulator holds, as it holds them, by
// name.
type definitions struct {
	topics    map[string]*pubsubpb.Topic
	subs      map[string]*pubsubpb.Subscription
	snapshots map[string]*pubsubpb.Snapshot
	// schemas are each AVRO schema's revisions, oldest first.
	schemas map[string][]*pubsubpb.Schema
}

func newDefinitions() definitions {
	return definitions{topics: map[string]*pubsubpb.Topic{}, subs: map[string]*pubsubpb.Subscription{},
		snapshots: map[string]*pubsubpb.Snapshot{}, schemas: map[string][]*pubsubpb.Schema{}}
}

func (d definitions) empty() bool {
	return len(d.topics) == 0 && len(d.subs) == 0 && len(d.snapshots) == 0 && len(d.schemas) == 0
}

// projects are the projects the definitions name.
func (d definitions) projects() []string {
	seen := map[string]bool{}
	add := func(name string) {
		if p := projectOf(name); p != "" {
			seen[p] = true
		}
	}
	for n := range d.topics {
		add(n)
	}
	for n := range d.subs {
		add(n)
	}
	for n := range d.snapshots {
		add(n)
	}
	for n := range d.schemas {
		add(n)
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// savedDefinitions are the definitions in the state file, each in its wire
// form.
type savedDefinitions struct {
	Topics        map[string][]byte   `json:"topics,omitempty"`
	Subscriptions map[string][]byte   `json:"subscriptions,omitempty"`
	Snapshots     map[string][]byte   `json:"snapshots,omitempty"`
	Schemas       map[string][][]byte `json:"schemas,omitempty"`
}

// saveDefinitionsLocked encodes the definitions; f.mu is held.
func (f *Front) saveDefinitionsLocked() (*savedDefinitions, error) {
	d := f.defs
	if d.empty() {
		return nil, nil
	}
	s := &savedDefinitions{}
	var err error
	if s.Topics, err = frontstate.EncodeMap(d.topics); err != nil {
		return nil, err
	}
	if s.Subscriptions, err = frontstate.EncodeMap(d.subs); err != nil {
		return nil, err
	}
	if s.Snapshots, err = frontstate.EncodeMap(d.snapshots); err != nil {
		return nil, err
	}
	if len(d.schemas) > 0 {
		s.Schemas = make(map[string][][]byte, len(d.schemas))
	}
	for name, revs := range d.schemas {
		for _, r := range revs {
			b, err := proto.Marshal(r)
			if err != nil {
				return nil, err
			}
			s.Schemas[name] = append(s.Schemas[name], b)
		}
	}
	return s, nil
}

// loadDefinitions decodes the definitions a state file holds.
func loadDefinitions(s *savedDefinitions) (definitions, error) {
	d := newDefinitions()
	if s == nil {
		return d, nil
	}
	var err error
	if d.topics, err = frontstate.DecodeMap(s.Topics, func() *pubsubpb.Topic { return &pubsubpb.Topic{} }); err != nil {
		return d, err
	}
	if d.subs, err = frontstate.DecodeMap(s.Subscriptions, func() *pubsubpb.Subscription { return &pubsubpb.Subscription{} }); err != nil {
		return d, err
	}
	if d.snapshots, err = frontstate.DecodeMap(s.Snapshots, func() *pubsubpb.Snapshot { return &pubsubpb.Snapshot{} }); err != nil {
		return d, err
	}
	for name, revs := range s.Schemas {
		for _, b := range revs {
			r := &pubsubpb.Schema{}
			if err := proto.Unmarshal(b, r); err != nil {
				return d, err
			}
			d.schemas[name] = append(d.schemas[name], r)
		}
	}
	return d, nil
}

// waitServing holds a call while the definitions are being created again,
// at most restoreWait.
func (f *Front) waitServing(ctx context.Context) error {
	f.mu.Lock()
	ch := f.restoring
	f.mu.Unlock()
	if ch == nil {
		return nil
	}
	t := time.NewTimer(restoreWait)
	defer t.Stop()
	select {
	case <-ch:
	case <-t.C:
	case <-ctx.Done():
		return status.FromContextError(ctx.Err()).Err()
	}
	return nil
}

// holdCalls makes calls wait until releaseCalls, and drops every read of
// the emulator begun before it.
func (f *Front) holdCalls() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gen++
	if f.restoring == nil {
		f.restoring = make(chan struct{})
	}
}

func (f *Front) releaseCalls() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.restoring != nil {
		close(f.restoring)
		f.restoring = nil
	}
}

// watchEmulator watches the emulator's process until ctx ends: when it is
// first seen, and whenever it comes back after ending, the definitions are
// checked (first) or created again (back), and calls are released.
func (f *Front) watchEmulator(ctx context.Context, dial func() (net.Conn, error), idle, poll time.Duration) {
	seen, down := false, false
	sleep := func(d time.Duration) bool {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return false
		case <-t.C:
			return true
		}
	}
	ended := func() {
		if seen && !down {
			down = true
			f.logf("pubsub front: the emulator's process ended; calls wait until its topics, subscriptions, schemas and snapshots are created again")
			f.holdCalls()
		}
	}
	for ctx.Err() == nil {
		c, err := dial()
		if err != nil {
			ended()
			if !sleep(poll) {
				return
			}
			continue
		}
		switch {
		case !seen:
			f.emulatorFirstSeen(ctx)
		case down:
			down = false
			f.logf("pubsub front: the emulator is back")
			f.restoreDefinitions(ctx)
		}
		f.releaseCalls()
		seen = true
		stop := context.AfterFunc(ctx, func() { _ = c.Close() })
		_ = c.SetReadDeadline(time.Now().Add(idle))
		// A gRPC server may send its HTTP/2 settings unasked: what it sends
		// is read and dropped, and only the connection closing is an end.
		buf := make([]byte, 512)
		for err == nil {
			_, err = c.Read(buf)
		}
		stop()
		_ = c.Close()
		if ctx.Err() != nil {
			return
		}
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			ended()
		}
	}
}

// emulatorFirstSeen decides, when the front starts, whether the emulator
// is the one the definitions describe, and takes in its definitions, or a
// new one holding none of them, and creates them again.
func (f *Front) emulatorFirstSeen(ctx context.Context) {
	f.mu.Lock()
	kept := f.copyDefinitionsLocked()
	f.mu.Unlock()
	if kept.empty() {
		f.reconcileAll(ctx, true)
		return
	}
	held := false
	for _, p := range kept.projects() {
		cur, err := f.readProject(ctx, p)
		if err != nil {
			f.logf("pubsub front: read the emulator's project %s: %v", p, err)
			held = true // never create over an emulator it cannot read
			break
		}
		for name := range kept.topics {
			if cur.topics[name] != nil {
				held = true
			}
		}
		for name := range kept.subs {
			if cur.subs[name] != nil {
				held = true
			}
		}
		for name := range kept.schemas {
			if cur.schemas[name] != nil {
				held = true
			}
		}
	}
	if held {
		f.reconcileAll(ctx, true)
		return
	}
	f.logf("pubsub front: the emulator holds none of the kept definitions; creating them again")
	f.restoreDefinitions(ctx)
}

// copyDefinitionsLocked is a copy of the definitions' maps; f.mu is held.
// The messages are never changed in place, so they are shared.
func (f *Front) copyDefinitionsLocked() definitions {
	d := newDefinitions()
	for k, v := range f.defs.topics {
		d.topics[k] = v
	}
	for k, v := range f.defs.subs {
		d.subs[k] = v
	}
	for k, v := range f.defs.snapshots {
		d.snapshots[k] = v
	}
	for k, v := range f.defs.schemas {
		d.schemas[k] = v
	}
	return d
}

// emulatorCall runs call with a deadline, again while the emulator, just
// started, refuses it UNAVAILABLE.
func emulatorCall(ctx context.Context, call func(context.Context) error) error {
	var err error
	for i := 0; i < 30; i++ {
		c, cancel := context.WithTimeout(ctx, 10*time.Second)
		err = call(c)
		cancel()
		if status.Code(err) != codes.Unavailable && status.Code(err) != codes.DeadlineExceeded {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(time.Second):
		}
	}
	return err
}

// restoreDefinitions creates the kept definitions in an emptied emulator:
// schemas, topics, subscriptions, then snapshots, each after what it names.
// It then reads them back, for the revision IDs and expiries the emulator
// gave them.
func (f *Front) restoreDefinitions(ctx context.Context) {
	f.defsMu.Lock()
	f.mu.Lock()
	d := f.copyDefinitionsLocked()
	f.mu.Unlock()
	pub := pubsubpb.NewPublisherClient(f.upstream)
	sub := pubsubpb.NewSubscriberClient(f.upstream)
	sch := pubsubpb.NewSchemaServiceClient(f.upstream)
	failed := 0
	fail := func(what, name string, err error) {
		failed++
		f.logf("pubsub front: create %s %s again: %v", what, name, err)
	}
	exists := func(err error) bool { return err == nil || status.Code(err) == codes.AlreadyExists }

	// revisions maps each schema's old revision IDs to the new ones.
	revisions := map[string]map[string]string{}
	for _, name := range sortedKeys(d.schemas) {
		revs := d.schemas[name]
		parent, id, ok := strings.Cut(name, "/schemas/")
		if !ok || len(revs) == 0 {
			continue
		}
		ids := map[string]string{}
		var got *pubsubpb.Schema
		if emulatorCall(ctx, func(c context.Context) (err error) {
			_, err = sch.GetSchema(c, &pubsubpb.GetSchemaRequest{Name: name})
			return err
		}) == nil {
			continue // still there: the emulator did not lose it
		}
		err := emulatorCall(ctx, func(c context.Context) (err error) {
			got, err = sch.CreateSchema(c, &pubsubpb.CreateSchemaRequest{Parent: parent, SchemaId: id,
				Schema: &pubsubpb.Schema{Type: revs[0].GetType(), Definition: revs[0].GetDefinition()}})
			return err
		})
		if status.Code(err) == codes.AlreadyExists {
			continue
		}
		if err != nil {
			fail("the schema", name, err)
			continue
		}
		ids[revs[0].GetRevisionId()] = got.GetRevisionId()
		for _, r := range revs[1:] {
			err := emulatorCall(ctx, func(c context.Context) (err error) {
				got, err = sch.CommitSchema(c, &pubsubpb.CommitSchemaRequest{Name: name,
					Schema: &pubsubpb.Schema{Name: name, Type: r.GetType(), Definition: r.GetDefinition()}})
				return err
			})
			if err != nil {
				fail("a revision of the schema", name, err)
				break
			}
			ids[r.GetRevisionId()] = got.GetRevisionId()
		}
		revisions[name] = ids
	}

	for _, name := range sortedKeys(d.topics) {
		t := proto.Clone(d.topics[name]).(*pubsubpb.Topic)
		if ss := t.GetSchemaSettings(); ss != nil {
			ids := revisions[ss.GetSchema()]
			if n, ok := ids[ss.GetFirstRevisionId()]; ok {
				ss.FirstRevisionId = n
			}
			if n, ok := ids[ss.GetLastRevisionId()]; ok {
				ss.LastRevisionId = n
			}
		}
		err := emulatorCall(ctx, func(c context.Context) error { _, err := pub.CreateTopic(c, t); return err })
		if !exists(err) && t.GetSchemaSettings() != nil {
			// Its schema is gone ("_deleted-schema_"), which no create
			// can name.
			t.SchemaSettings = nil
			err = emulatorCall(ctx, func(c context.Context) error { _, err := pub.CreateTopic(c, t); return err })
		}
		if !exists(err) {
			fail("the topic", name, err)
		}
	}

	for _, name := range sortedKeys(d.subs) {
		s := proto.Clone(d.subs[name]).(*pubsubpb.Subscription)
		// Output only.
		s.State, s.TopicMessageRetentionDuration, s.AnalyticsHubSubscriptionInfo = 0, nil, nil
		if s.GetPushConfig().GetPushEndpoint() == "" && s.GetPushConfig().GetAttributes() == nil {
			s.PushConfig = nil // a pull subscription
		}
		var err error
		if s.GetTopic() == deletedTopic {
			err = f.createOrphan(ctx, pub, sub, s)
		} else {
			err = emulatorCall(ctx, func(c context.Context) error { _, err := sub.CreateSubscription(c, s); return err })
		}
		if !exists(err) {
			fail("the subscription", name, err)
		}
	}

	for _, name := range sortedKeys(d.snapshots) {
		if err := f.createSnapshotAgain(ctx, pub, sub, d.snapshots[name], d.subs); !exists(err) {
			fail("the snapshot", name, err)
		}
	}
	f.defsMu.Unlock()

	f.reconcileAll(ctx, true)
	f.logf("pubsub front: created again %d topics, %d subscriptions, %d schemas and %d snapshots; %d failed",
		len(d.topics), len(d.subs), len(d.schemas), len(d.snapshots), failed)
}

// createOrphan creates a subscription whose topic was deleted: on a topic
// of its own, deleted at once, as the emulator then names it. No call is
// served meanwhile, so no client sees that topic.
func (f *Front) createOrphan(ctx context.Context, pub pubsubpb.PublisherClient, sub pubsubpb.SubscriberClient, s *pubsubpb.Subscription) error {
	topic := "projects/" + projectOf(s.GetName()) + "/topics/cloudburrow-restore-" + randomID()
	if err := emulatorCall(ctx, func(c context.Context) error {
		_, err := pub.CreateTopic(c, &pubsubpb.Topic{Name: topic})
		return err
	}); err != nil {
		return err
	}
	s.Topic = topic
	err := emulatorCall(ctx, func(c context.Context) error { _, err := sub.CreateSubscription(c, s); return err })
	if derr := emulatorCall(ctx, func(c context.Context) error {
		_, err := pub.DeleteTopic(c, &pubsubpb.DeleteTopicRequest{Topic: topic})
		return err
	}); err == nil {
		err = derr
	}
	return err
}

// createSnapshotAgain takes a snapshot again, from a subscription to its
// topic, or from one made for it and deleted at once. Its expiry is kept no
// later than the emulator first gave it, as the front keeps an update's
// (snapshots.go).
func (f *Front) createSnapshotAgain(ctx context.Context, pub pubsubpb.PublisherClient, sub pubsubpb.SubscriberClient,
	snap *pubsubpb.Snapshot, subs map[string]*pubsubpb.Subscription) error {
	from := ""
	for _, name := range sortedKeys(subs) {
		if subs[name].GetTopic() == snap.GetTopic() {
			from = name
			break
		}
	}
	temp := from == ""
	if temp {
		from = "projects/" + projectOf(snap.GetName()) + "/subscriptions/cloudburrow-restore-" + randomID()
		if err := emulatorCall(ctx, func(c context.Context) error {
			_, err := sub.CreateSubscription(c, &pubsubpb.Subscription{Name: from, Topic: snap.GetTopic()})
			return err
		}); err != nil {
			return err
		}
	}
	var got *pubsubpb.Snapshot
	err := emulatorCall(ctx, func(c context.Context) (err error) {
		got, err = sub.CreateSnapshot(c, &pubsubpb.CreateSnapshotRequest{Name: snap.GetName(), Subscription: from, Labels: snap.GetLabels()})
		return err
	})
	if temp {
		if derr := emulatorCall(ctx, func(c context.Context) error {
			_, err := sub.DeleteSubscription(c, &pubsubpb.DeleteSubscriptionRequest{Subscription: from})
			return err
		}); err == nil {
			err = derr
		}
	}
	if err == nil && snap.GetExpireTime() != nil && got.GetExpireTime().AsTime().After(snap.GetExpireTime().AsTime()) {
		f.mu.Lock()
		k := f.snapshots[snap.GetName()]
		if k.ExpireTime.IsZero() {
			k.ExpireTime = snap.GetExpireTime().AsTime()
			f.snapshots[snap.GetName()] = k
		}
		f.mu.Unlock()
	}
	return err
}

func randomID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func sortedKeys[T any](m map[string]T) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// readProject reads every definition a project holds in the emulator.
func (f *Front) readProject(ctx context.Context, project string) (definitions, error) {
	d := newDefinitions()
	parent := "projects/" + project
	pub := pubsubpb.NewPublisherClient(f.upstream)
	sub := pubsubpb.NewSubscriberClient(f.upstream)
	sch := pubsubpb.NewSchemaServiceClient(f.upstream)
	c, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for tok := ""; ; {
		r, err := pub.ListTopics(c, &pubsubpb.ListTopicsRequest{Project: parent, PageToken: tok})
		if err != nil {
			return d, err
		}
		for _, t := range r.GetTopics() {
			d.topics[t.GetName()] = t
		}
		if tok = r.GetNextPageToken(); tok == "" {
			break
		}
	}
	for tok := ""; ; {
		r, err := sub.ListSubscriptions(c, &pubsubpb.ListSubscriptionsRequest{Project: parent, PageToken: tok})
		if err != nil {
			return d, err
		}
		for _, s := range r.GetSubscriptions() {
			d.subs[s.GetName()] = s
		}
		if tok = r.GetNextPageToken(); tok == "" {
			break
		}
	}
	for tok := ""; ; {
		r, err := sub.ListSnapshots(c, &pubsubpb.ListSnapshotsRequest{Project: parent, PageToken: tok})
		if status.Code(err) == codes.Unimplemented {
			break // an emulator without snapshots (pstest)
		}
		if err != nil {
			return d, err
		}
		for _, s := range r.GetSnapshots() {
			d.snapshots[s.GetName()] = s
		}
		if tok = r.GetNextPageToken(); tok == "" {
			break
		}
	}
	var names []string
	for tok := ""; ; {
		r, err := sch.ListSchemas(c, &pubsubpb.ListSchemasRequest{Parent: parent, PageToken: tok})
		if status.Code(err) == codes.Unimplemented {
			break
		}
		if err != nil {
			return d, err
		}
		for _, s := range r.GetSchemas() {
			names = append(names, s.GetName())
		}
		if tok = r.GetNextPageToken(); tok == "" {
			break
		}
	}
	for _, name := range names {
		revs, err := schemaRevisions(c, sch, name)
		if status.Code(err) == codes.NotFound {
			continue
		}
		if err != nil {
			return d, err
		}
		d.schemas[name] = revs
	}
	return d, nil
}

// schemaRevisions are a schema's revisions in the emulator, oldest first.
func schemaRevisions(ctx context.Context, sch pubsubpb.SchemaServiceClient, name string) ([]*pubsubpb.Schema, error) {
	var revs []*pubsubpb.Schema
	for tok := ""; ; {
		r, err := sch.ListSchemaRevisions(ctx, &pubsubpb.ListSchemaRevisionsRequest{Name: name,
			View: pubsubpb.SchemaView_FULL, PageToken: tok})
		if err != nil {
			return nil, err
		}
		revs = append(revs, r.GetSchemas()...)
		if tok = r.GetNextPageToken(); tok == "" {
			break
		}
	}
	sort.SliceStable(revs, func(i, j int) bool {
		return revs[i].GetRevisionCreateTime().AsTime().Before(revs[j].GetRevisionCreateTime().AsTime())
	})
	return revs, nil
}

// reconcileAll reads again every project the front knows, and keeps what
// the emulator holds in each as its definitions. held is set while calls
// are held for the watch, which alone reads the emulator then.
func (f *Front) reconcileAll(ctx context.Context, held bool) {
	f.mu.Lock()
	seen := map[string]bool{}
	for p := range f.projects {
		seen[p] = true
	}
	for _, p := range f.defs.projects() {
		seen[p] = true
	}
	f.mu.Unlock()
	changed := false
	for _, p := range sortedKeys(seen) {
		if ctx.Err() != nil {
			return
		}
		changed = f.reconcile(ctx, p, held) || changed
	}
	if changed {
		f.persist()
	}
}

// reconcile keeps what the emulator holds in project as its definitions,
// reporting whether they changed.
func (f *Front) reconcile(ctx context.Context, project string, held bool) bool {
	f.defsMu.Lock()
	defer f.defsMu.Unlock()
	gen, ok := f.readGen()
	if !ok && !held {
		return false
	}
	cur, err := f.readProject(ctx, project)
	if err != nil {
		return false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.gen != gen || (f.restoring != nil && !held) {
		return false
	}
	changed := replaceProject(f.defs.topics, cur.topics, project)
	changed = replaceProject(f.defs.subs, cur.subs, project) || changed
	changed = replaceProject(f.defs.snapshots, cur.snapshots, project) || changed
	changed = replaceSchemas(f.defs.schemas, cur.schemas, project) || changed
	return changed
}

// readGen is the emulator's generation, and whether the emulator may be
// read now: not while the definitions are being created again.
func (f *Front) readGen() (uint64, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gen, f.restoring == nil
}

func replaceProject[M proto.Message](kept, cur map[string]M, project string) bool {
	changed := false
	prefix := "projects/" + project + "/"
	for name, m := range kept {
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		if n, ok := cur[name]; !ok || !proto.Equal(n, m) {
			delete(kept, name)
			changed = true
		}
	}
	for name, m := range cur {
		if _, ok := kept[name]; !ok {
			kept[name] = m
			changed = true
		}
	}
	return changed
}

func replaceSchemas(kept, cur map[string][]*pubsubpb.Schema, project string) bool {
	changed := false
	prefix := "projects/" + project + "/"
	for name, revs := range kept {
		if strings.HasPrefix(name, prefix) && !sameRevisions(revs, cur[name]) {
			delete(kept, name)
			changed = true
		}
	}
	for name, revs := range cur {
		if _, ok := kept[name]; !ok {
			kept[name] = revs
			changed = true
		}
	}
	return changed
}

func sameRevisions(a, b []*pubsubpb.Schema) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !proto.Equal(a[i], b[i]) {
			return false
		}
	}
	return true
}

// Resource kinds a call changes.
const (
	kindTopic    = "topic"
	kindSub      = "subscription"
	kindSnapshot = "snapshot"
	kindSchema   = "schema"
)

// mutationTarget is the resource a gRPC call that changes a definition
// names, from its first request; "" for any other call.
func mutationTarget(method string, fr frame) (kind, name string) {
	pick := func(m proto.Message, get func() string) string {
		if proto.Unmarshal(fr, m) != nil {
			return ""
		}
		return get()
	}
	switch method {
	case publisher + "CreateTopic":
		var t pubsubpb.Topic
		return kindTopic, pick(&t, t.GetName)
	case publisher + "UpdateTopic":
		var r pubsubpb.UpdateTopicRequest
		return kindTopic, pick(&r, func() string { return r.GetTopic().GetName() })
	case publisher + "DeleteTopic":
		var r pubsubpb.DeleteTopicRequest
		return kindTopic, pick(&r, r.GetTopic)
	case publisher + "DetachSubscription":
		var r pubsubpb.DetachSubscriptionRequest
		return kindSub, pick(&r, r.GetSubscription)
	case subscriber + "CreateSubscription":
		var s pubsubpb.Subscription
		return kindSub, pick(&s, s.GetName)
	case subscriber + "UpdateSubscription":
		var r pubsubpb.UpdateSubscriptionRequest
		return kindSub, pick(&r, func() string { return r.GetSubscription().GetName() })
	case subscriber + "DeleteSubscription":
		var r pubsubpb.DeleteSubscriptionRequest
		return kindSub, pick(&r, r.GetSubscription)
	case subscriber + "ModifyPushConfig":
		var r pubsubpb.ModifyPushConfigRequest
		return kindSub, pick(&r, r.GetSubscription)
	case subscriber + "CreateSnapshot":
		var r pubsubpb.CreateSnapshotRequest
		return kindSnapshot, pick(&r, r.GetName)
	case subscriber + "UpdateSnapshot":
		var r pubsubpb.UpdateSnapshotRequest
		return kindSnapshot, pick(&r, func() string { return r.GetSnapshot().GetName() })
	case subscriber + "DeleteSnapshot":
		var r pubsubpb.DeleteSnapshotRequest
		return kindSnapshot, pick(&r, r.GetSnapshot)
	case schemaService + "CreateSchema":
		var r pubsubpb.CreateSchemaRequest
		return kindSchema, pick(&r, func() string {
			if r.GetSchemaId() == "" {
				return ""
			}
			return r.GetParent() + "/schemas/" + r.GetSchemaId()
		})
	case schemaService + "CommitSchema":
		var r pubsubpb.CommitSchemaRequest
		return kindSchema, pick(&r, r.GetName)
	case schemaService + "RollbackSchema":
		var r pubsubpb.RollbackSchemaRequest
		return kindSchema, pick(&r, r.GetName)
	case schemaService + "DeleteSchemaRevision":
		var r pubsubpb.DeleteSchemaRevisionRequest
		return kindSchema, pick(&r, r.GetName)
	case schemaService + "DeleteSchema":
		var r pubsubpb.DeleteSchemaRequest
		return kindSchema, pick(&r, r.GetName)
	}
	return "", ""
}

// restMutationTarget is the resource a REST request that changes a
// definition names; "" for any other request.
func restMutationTarget(method, path, schemaID string) (kind, name string) {
	if method == "GET" || method == "HEAD" {
		return "", ""
	}
	rest, ok := strings.CutPrefix(path, "/v1/")
	if !ok {
		return "", ""
	}
	parts := strings.Split(rest, "/")
	if len(parts) == 3 && parts[0] == "projects" && parts[2] == "schemas" && method == "POST" && schemaID != "" {
		return kindSchema, "projects/" + parts[1] + "/schemas/" + schemaID
	}
	if len(parts) != 4 || parts[0] != "projects" || parts[1] == "" {
		return "", ""
	}
	id, verb, _ := strings.Cut(parts[3], ":")
	if id == "" {
		return "", ""
	}
	name = "projects/" + parts[1] + "/" + parts[2] + "/" + id
	switch {
	case parts[2] == "topics" && verb == "":
		return kindTopic, name
	case parts[2] == "subscriptions" && (verb == "" || verb == "modifyPushConfig" || verb == "detach"):
		return kindSub, name
	case parts[2] == "snapshots" && verb == "":
		return kindSnapshot, name
	case parts[2] == "schemas" && (verb == "" || verb == "commit" || verb == "rollback" || verb == "deleteRevision"):
		return kindSchema, name
	}
	return "", ""
}

// refresh reads again from the emulator the resource a call changed, and
// keeps it, or forgets it when the emulator no longer holds it; a deleted
// topic's subscriptions and a deleted schema's topics are read again too.
// The state file is written before the call is answered.
func (f *Front) refresh(ctx context.Context, kind, name string) {
	if kind == "" || name == "" {
		return
	}
	if i := strings.IndexByte(name, '@'); i >= 0 {
		name = name[:i] // a revision's schema
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	changed := f.refreshOne(ctx, kind, name)
	var also []string
	f.mu.Lock()
	switch kind {
	case kindTopic:
		if f.defs.topics[name] == nil {
			for n, s := range f.defs.subs {
				if s.GetTopic() == name {
					also = append(also, n)
				}
			}
			kind = kindSub
		}
	case kindSchema:
		if f.defs.schemas[name] == nil {
			for n, t := range f.defs.topics {
				if t.GetSchemaSettings().GetSchema() == name {
					also = append(also, n)
				}
			}
			kind = kindTopic
		}
	}
	f.mu.Unlock()
	sort.Strings(also)
	for _, n := range also {
		changed = f.refreshOne(ctx, kind, n) || changed
	}
	if changed {
		f.persist()
	}
}

// refreshOne reads one resource again, reporting whether the definitions
// changed.
func (f *Front) refreshOne(ctx context.Context, kind, name string) bool {
	f.defsMu.Lock()
	defer f.defsMu.Unlock()
	gen, ok := f.readGen()
	if !ok {
		return false
	}
	var (
		got  proto.Message
		revs []*pubsubpb.Schema
		err  error
	)
	switch kind {
	case kindTopic:
		got, err = pubsubpb.NewPublisherClient(f.upstream).GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: name})
	case kindSub:
		got, err = pubsubpb.NewSubscriberClient(f.upstream).GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: name})
	case kindSnapshot:
		got, err = pubsubpb.NewSubscriberClient(f.upstream).GetSnapshot(ctx, &pubsubpb.GetSnapshotRequest{Snapshot: name})
	case kindSchema:
		revs, err = schemaRevisions(ctx, pubsubpb.NewSchemaServiceClient(f.upstream), name)
	default:
		return false
	}
	gone := status.Code(err) == codes.NotFound
	if err != nil && !gone {
		f.logf("pubsub front: read %s %s again: %v", kind, name, err)
		return false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.gen != gen || f.restoring != nil {
		return false
	}
	switch kind {
	case kindTopic:
		return keepOne(f.defs.topics, name, got, gone)
	case kindSub:
		return keepOne(f.defs.subs, name, got, gone)
	case kindSnapshot:
		return keepOne(f.defs.snapshots, name, got, gone)
	}
	if gone || len(revs) == 0 {
		_, had := f.defs.schemas[name]
		delete(f.defs.schemas, name)
		return had
	}
	if sameRevisions(f.defs.schemas[name], revs) {
		return false
	}
	f.defs.schemas[name] = revs
	return true
}

func keepOne[M proto.Message](kept map[string]M, name string, got proto.Message, gone bool) bool {
	old, had := kept[name]
	if gone {
		delete(kept, name)
		return had
	}
	m, ok := got.(M)
	if !ok {
		return false
	}
	if had && proto.Equal(old, m) {
		return false
	}
	kept[name] = m
	return true
}

// firstFrame is a server stream that keeps a copy of the first request it
// reads, for refresh.
type firstFrame struct {
	grpc.ServerStream
	first frame
	read  bool
}

func (s *firstFrame) RecvMsg(m any) error {
	err := s.ServerStream.RecvMsg(m)
	if err == nil && !s.read {
		s.read = true
		if fr, ok := m.(*frame); ok {
			s.first = append(frame(nil), *fr...)
		}
	}
	return err
}
