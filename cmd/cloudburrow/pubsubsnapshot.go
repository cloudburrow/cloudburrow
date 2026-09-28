package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	pubsub "cloud.google.com/go/pubsub/v2"
	apiv1 "cloud.google.com/go/pubsub/v2/apiv1"
	pubsubpb "cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/cloudburrow/cloudburrow/internal/admin"
	"github.com/cloudburrow/cloudburrow/internal/netfwd"
	"github.com/cloudburrow/cloudburrow/internal/pubsubfront"
)

// pubsubSnapshotter captures Pub/Sub's resources (#880) through the official
// client, the calls a program would make, since Google's emulator has no
// export and keeps nothing across a restart.
//
// Kept, for every project CloudBurrow knows and every project a call through
// the front has named (the emulator serves any project): every schema with every
// revision, in order; every topic as the emulator returns it; every
// subscription with its settings, its real push endpoint (the front's push
// relay is never saved) and how long it had been idle, which is its
// expiration clock; and every snapshot's name and topic.
//
// Not kept: messages, whether published and unacknowledged, retained after
// acknowledgement, or in a snapshot's backlog; and ack IDs and leases. A
// loaded subscription starts empty. A loaded snapshot is taken again from a
// subscription of its topic, so it marks the moment of the load, and its
// expire time is new. A schema revision gets a new revision ID, and a
// topic's schema settings are pointed at the new IDs. A subscription whose
// topic was deleted (its topic reads _deleted-topic_) cannot be created
// again, and is left out. The time between a save and a load does not count
// towards a subscription's expiration.
type pubsubSnapshotter struct {
	tunnel   *netfwd.Forwarder
	projects func() []string
}

const pubsubEntry = "pubsub.json"

// pubsubState is the archive's pubsub.json. Each resource is its protobuf
// JSON form, so every field the emulator keeps is kept.
type pubsubState struct {
	Projects []string `json:"projects"`
	// Schemas holds each schema's revisions, oldest first.
	Schemas       [][]json.RawMessage `json:"schemas"`
	Topics        []json.RawMessage   `json:"topics"`
	Subscriptions []json.RawMessage   `json:"subscriptions"`
	Snapshots     []json.RawMessage   `json:"snapshots"`
	// IdleSeconds is how long each subscription had been idle, by the
	// front's clock.
	IdleSeconds map[string]float64 `json:"idleSeconds,omitempty"`
}

func (p *pubsubSnapshotter) Name() string { return "pubsub" }
func (p *pubsubSnapshotter) Secret() bool { return false }

// clients are the official admin and schema clients, and a plain connection
// for the front's own methods, all through the tunnel.
func (p *pubsubSnapshotter) clients(ctx context.Context) (*pubsub.Client, *apiv1.SchemaClient, *grpc.ClientConn, error) {
	c, err := pubsubAdmin(ctx, p.tunnel, "cloudburrow-state")
	if err != nil {
		return nil, nil, nil, err
	}
	sc, err := apiv1.NewSchemaClient(ctx, option.WithEndpoint(p.tunnel.HostAddr()), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		_ = c.Close()
		return nil, nil, nil, err
	}
	conn, err := grpc.NewClient(p.tunnel.HostAddr(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		_ = c.Close()
		_ = sc.Close()
		return nil, nil, nil, err
	}
	return c, sc, conn, nil
}

// allProjects is every project CloudBurrow knows and every project a call
// through the front has named (pubsubfront.ProjectsMethod): the emulator
// serves any project, and cannot list them. A front without the method
// (UNIMPLEMENTED) adds none.
func (p *pubsubSnapshotter) allProjects(ctx context.Context, conn *grpc.ClientConn) ([]string, error) {
	seen := map[string]bool{}
	for _, pr := range p.projects() {
		seen[pr] = true
	}
	var l structpb.ListValue
	switch err := conn.Invoke(ctx, pubsubfront.ProjectsMethod, &emptypb.Empty{}, &l); {
	case status.Code(err) == codes.Unimplemented:
	case err != nil:
		return nil, fmt.Errorf("list the projects Pub/Sub has served: %w", err)
	default:
		for _, v := range l.GetValues() {
			seen[v.GetStringValue()] = true
		}
	}
	delete(seen, "")
	out := make([]string, 0, len(seen))
	for pr := range seen {
		out = append(out, pr)
	}
	sort.Strings(out)
	return out, nil
}

func marshalAll[M proto.Message](ms []M) ([]json.RawMessage, error) {
	out := make([]json.RawMessage, 0, len(ms))
	for _, m := range ms {
		b, err := protojson.Marshal(m)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, nil
}

// all drains an iterator; an UNIMPLEMENTED answer is an emulator without
// the resource, which has none.
func all[T any](next func() (T, error)) ([]T, error) {
	var out []T
	for {
		v, err := next()
		if errors.Is(err, iterator.Done) || status.Code(err) == codes.Unimplemented {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
}

func (p *pubsubSnapshotter) Export(ctx context.Context, w admin.EntryWriter) error {
	c, sc, conn, err := p.clients(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close(); _ = sc.Close(); _ = conn.Close() }()

	projects, err := p.allProjects(ctx, conn)
	if err != nil {
		return err
	}
	st := pubsubState{Projects: projects, Schemas: [][]json.RawMessage{}}
	var topics []*pubsubpb.Topic
	var subs []*pubsubpb.Subscription
	var snaps []*pubsubpb.Snapshot
	for _, project := range st.Projects {
		parent := "projects/" + project
		schemas, err := all(sc.ListSchemas(ctx, &pubsubpb.ListSchemasRequest{Parent: parent, View: pubsubpb.SchemaView_FULL}).Next)
		if err != nil {
			return fmt.Errorf("list %s's schemas: %w", project, err)
		}
		for _, s := range schemas {
			if !strings.HasPrefix(s.GetName(), parent+"/") {
				continue // a server that lists every project's
			}
			revs, err := all(sc.ListSchemaRevisions(ctx, &pubsubpb.ListSchemaRevisionsRequest{Name: s.GetName(), View: pubsubpb.SchemaView_FULL}).Next)
			if err != nil {
				return fmt.Errorf("list %s's revisions: %w", s.GetName(), err)
			}
			if len(revs) == 0 {
				revs = []*pubsubpb.Schema{s}
			}
			// Oldest first, whichever order the server lists them in.
			sort.SliceStable(revs, func(i, j int) bool {
				return revs[i].GetRevisionCreateTime().AsTime().Before(revs[j].GetRevisionCreateTime().AsTime())
			})
			raw, err := marshalAll(revs)
			if err != nil {
				return err
			}
			st.Schemas = append(st.Schemas, raw)
		}
		t, err := all(c.TopicAdminClient.ListTopics(ctx, &pubsubpb.ListTopicsRequest{Project: parent}).Next)
		if err != nil {
			return fmt.Errorf("list %s's topics: %w", project, err)
		}
		topics = append(topics, t...)
		s, err := all(c.SubscriptionAdminClient.ListSubscriptions(ctx, &pubsubpb.ListSubscriptionsRequest{Project: parent}).Next)
		if err != nil {
			return fmt.Errorf("list %s's subscriptions: %w", project, err)
		}
		subs = append(subs, s...)
		n, err := all(c.SubscriptionAdminClient.ListSnapshots(ctx, &pubsubpb.ListSnapshotsRequest{Project: parent}).Next)
		if err != nil {
			return fmt.Errorf("list %s's snapshots: %w", project, err)
		}
		snaps = append(snaps, n...)
	}
	if st.Topics, err = marshalAll(topics); err != nil {
		return err
	}
	if st.Subscriptions, err = marshalAll(subs); err != nil {
		return err
	}
	if st.Snapshots, err = marshalAll(snaps); err != nil {
		return err
	}

	// The expiration clocks. A front without the method (UNIMPLEMENTED)
	// keeps none, and a load then starts every clock afresh.
	var idle structpb.Struct
	switch err := conn.Invoke(ctx, pubsubfront.ActivityExportMethod, &emptypb.Empty{}, &idle); {
	case status.Code(err) == codes.Unimplemented:
	case err != nil:
		return fmt.Errorf("read the subscriptions' expiration clocks: %w", err)
	default:
		st.IdleSeconds = map[string]float64{}
		for _, s := range subs {
			if v, ok := idle.GetFields()[s.GetName()]; ok {
				st.IdleSeconds[s.GetName()] = v.GetNumberValue()
			}
		}
	}
	b, err := json.MarshalIndent(st, "", " ")
	if err != nil {
		return err
	}
	return w.Add(pubsubEntry, int64(len(b)), bytes.NewReader(b))
}

func unmarshalAll[M proto.Message](raw []json.RawMessage, fresh func() M) ([]M, error) {
	out := make([]M, 0, len(raw))
	for _, r := range raw {
		m := fresh()
		if err := protojson.Unmarshal(r, m); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

// Import replaces every known project's Pub/Sub resources, and those of
// every project the archive names, with the archive's.
func (p *pubsubSnapshotter) Import(ctx context.Context, r admin.EntryReader) error {
	f, err := r.Open(pubsubEntry)
	if err != nil {
		return err
	}
	var st pubsubState
	err = json.NewDecoder(io.LimitReader(f, 1<<30)).Decode(&st)
	_ = f.Close()
	if err != nil {
		return fmt.Errorf("read %s: %w", pubsubEntry, err)
	}
	topics, err := unmarshalAll(st.Topics, func() *pubsubpb.Topic { return &pubsubpb.Topic{} })
	if err != nil {
		return err
	}
	subs, err := unmarshalAll(st.Subscriptions, func() *pubsubpb.Subscription { return &pubsubpb.Subscription{} })
	if err != nil {
		return err
	}
	snaps, err := unmarshalAll(st.Snapshots, func() *pubsubpb.Snapshot { return &pubsubpb.Snapshot{} })
	if err != nil {
		return err
	}

	c, sc, conn, err := p.clients(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close(); _ = sc.Close(); _ = conn.Close() }()

	// Clear: the resetter's subscriptions, snapshots and topics, then the
	// schemas, which topics may name.
	known, err := p.allProjects(ctx, conn)
	if err != nil {
		return err
	}
	projects := map[string]bool{}
	for _, pr := range append(known, st.Projects...) {
		if pr != "" {
			projects[pr] = true
		}
	}
	reset := &pubsubResetter{tunnel: p.tunnel}
	for pr := range projects {
		if err := reset.ResetProject(ctx, pr); err != nil {
			return fmt.Errorf("clear %s: %w", pr, err)
		}
		schemas, err := all(sc.ListSchemas(ctx, &pubsubpb.ListSchemasRequest{Parent: "projects/" + pr}).Next)
		if err != nil {
			return fmt.Errorf("list %s's schemas: %w", pr, err)
		}
		for _, s := range schemas {
			if err := sc.DeleteSchema(ctx, &pubsubpb.DeleteSchemaRequest{Name: s.GetName()}); err != nil && status.Code(err) != codes.NotFound {
				return fmt.Errorf("delete %s: %w", s.GetName(), err)
			}
		}
	}

	// Schemas, revision by revision; each revision's new ID replaces its
	// old one in the topics that name it.
	revisions := map[string]string{}
	for _, raw := range st.Schemas {
		revs, err := unmarshalAll(raw, func() *pubsubpb.Schema { return &pubsubpb.Schema{} })
		if err != nil {
			return err
		}
		for i, rev := range revs {
			name := rev.GetName()
			parent, id, ok := strings.Cut(name, "/schemas/")
			if !ok {
				return fmt.Errorf("schema %q is not projects/{project}/schemas/{schema}", name)
			}
			in := &pubsubpb.Schema{Name: name, Type: rev.GetType(), Definition: rev.GetDefinition()}
			var got *pubsubpb.Schema
			if i == 0 {
				got, err = sc.CreateSchema(ctx, &pubsubpb.CreateSchemaRequest{Parent: parent, SchemaId: id, Schema: in})
			} else {
				got, err = sc.CommitSchema(ctx, &pubsubpb.CommitSchemaRequest{Name: name, Schema: in})
			}
			if err != nil {
				return fmt.Errorf("create %s revision %s: %w", name, rev.GetRevisionId(), err)
			}
			if old := rev.GetRevisionId(); old != "" {
				revisions[name+"@"+old] = got.GetRevisionId()
			}
		}
	}

	// Topics first: a subscription, and a dead-letter policy, name one.
	for _, t := range topics {
		t.State = pubsubpb.Topic_STATE_UNSPECIFIED
		if ss := t.GetSchemaSettings(); ss != nil {
			for _, id := range []*string{&ss.FirstRevisionId, &ss.LastRevisionId} {
				if n, ok := revisions[ss.GetSchema()+"@"+*id]; ok && *id != "" {
					*id = n
				}
			}
		}
		if _, err := c.TopicAdminClient.CreateTopic(ctx, t); err != nil {
			return fmt.Errorf("create %s: %w", t.GetName(), err)
		}
	}
	byTopic := map[string]string{}
	idle := map[string]*structpb.Value{}
	for _, s := range subs {
		if s.GetTopic() == deletedTopic {
			continue
		}
		s.State = pubsubpb.Subscription_STATE_UNSPECIFIED
		s.TopicMessageRetentionDuration = nil
		s.AnalyticsHubSubscriptionInfo = nil
		if s.GetPushConfig().GetPushEndpoint() == "" {
			// A pull subscription: what a server reads back as its empty
			// push config is not sent as one.
			s.PushConfig = nil
		}
		if _, err := c.SubscriptionAdminClient.CreateSubscription(ctx, s); err != nil {
			return fmt.Errorf("create %s: %w", s.GetName(), err)
		}
		if _, ok := byTopic[s.GetTopic()]; !ok {
			byTopic[s.GetTopic()] = s.GetName()
		}
		if v, ok := st.IdleSeconds[s.GetName()]; ok && v >= 0 {
			idle[s.GetName()] = structpb.NewNumberValue(v)
		}
	}

	// A snapshot is taken from a subscription, which it does not name: any
	// subscription of its topic is as good, since every one starts empty.
	for _, n := range snaps {
		from, temp := byTopic[n.GetTopic()], ""
		if from == "" {
			project, _, _ := strings.Cut(strings.TrimPrefix(n.GetName(), "projects/"), "/")
			temp = fmt.Sprintf("projects/%s/subscriptions/cloudburrow-state-%s", project, randomHex(6))
			if _, err := c.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{Name: temp, Topic: n.GetTopic()}); err != nil {
				return fmt.Errorf("a subscription to take %s from: %w", n.GetName(), err)
			}
			from = temp
		}
		_, err := c.SubscriptionAdminClient.CreateSnapshot(ctx, &pubsubpb.CreateSnapshotRequest{
			Name: n.GetName(), Subscription: from, Labels: n.GetLabels()})
		if temp != "" {
			if derr := c.SubscriptionAdminClient.DeleteSubscription(ctx, &pubsubpb.DeleteSubscriptionRequest{Subscription: temp}); derr != nil && err == nil {
				err = derr
			}
		}
		if err != nil {
			return fmt.Errorf("create %s: %w", n.GetName(), err)
		}
	}

	// Last, so nothing above counts as activity: the clocks as they were.
	if len(idle) > 0 {
		err := conn.Invoke(ctx, pubsubfront.ActivityImportMethod, &structpb.Struct{Fields: idle}, &emptypb.Empty{})
		if err != nil && status.Code(err) != codes.Unimplemented {
			return fmt.Errorf("restore the subscriptions' expiration clocks: %w", err)
		}
	}
	return nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
