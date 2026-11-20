package pubsubfront

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/orgpolicy"
)

// Custom constraints (#1891) on pubsub.googleapis.com/Topic and
// pubsub.googleapis.com/Subscription, CREATE and UPDATE, over gRPC and
// REST ("Use custom organization policies", Pub/Sub, read 2026-11-19):
// the condition reads the resource by its REST field names
// (resource.messageRetentionDuration, resource.ackDeadlineSeconds); an
// update is checked on the resource as its mask would leave it. A
// violation is FAILED_PRECONDITION "Operation denied by custom org
// policies: [...]".

// asResource is m as custom constraints read it: its REST JSON.
func asResource(m proto.Message) any {
	raw, err := protojson.Marshal(m)
	if err != nil {
		return map[string]any{}
	}
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out
}

func checkCustom(ctx context.Context, name, resourceType, method string, m proto.Message) error {
	project := orgpolicy.ProjectOf(name)
	if project == "" || !orgpolicy.SourceSet() {
		return nil
	}
	err := orgpolicy.CheckCustom(ctx, project, resourceType, method, asResource(m))
	var v *orgpolicy.CustomViolation
	if errors.As(err, &v) {
		return status.Error(codes.FailedPrecondition, v.Error())
	}
	return nil
}

// applyMask copies the fields paths name (by their first segment, proto
// or JSON name) from req into cur; an empty mask copies every field set.
func applyMask(cur, req proto.Message, paths []string) {
	c, r := cur.ProtoReflect(), req.ProtoReflect()
	fields := r.Descriptor().Fields()
	if len(paths) == 0 {
		r.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
			c.Set(fd, v)
			return true
		})
		return
	}
	for _, p := range paths {
		top, _, _ := strings.Cut(p, ".")
		fd := fields.ByName(protoreflect.Name(top))
		if fd == nil {
			fd = fields.ByJSONName(top)
		}
		if fd == nil {
			continue
		}
		if r.Has(fd) {
			c.Set(fd, r.Get(fd))
		} else {
			c.Clear(fd)
		}
	}
}

// customUpdateTopic checks an UpdateTopic on the topic as it would be.
func (f *Front) customUpdateTopic(ctx context.Context, t *pubsubpb.Topic, paths []string) error {
	if t == nil || !orgpolicy.SourceSet() {
		return nil
	}
	cur, err := f.topics.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: t.GetName()})
	if err != nil {
		return nil // the update answers it
	}
	next := proto.Clone(cur).(*pubsubpb.Topic)
	applyMask(next, t, paths)
	return checkCustom(ctx, t.GetName(), orgpolicy.TypeTopic, orgpolicy.MethodUpdate, next)
}

// customUpdateSubscription checks an UpdateSubscription likewise.
func (f *Front) customUpdateSubscription(ctx context.Context, s *pubsubpb.Subscription, paths []string) error {
	if s == nil || !orgpolicy.SourceSet() {
		return nil
	}
	cur, err := f.admin.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: s.GetName()})
	if err != nil {
		return nil
	}
	next := proto.Clone(cur).(*pubsubpb.Subscription)
	applyMask(next, s, paths)
	return checkCustom(ctx, s.GetName(), orgpolicy.TypeSubscription, orgpolicy.MethodUpdate, next)
}

// customGRPCUpdate checks an UpdateTopic or UpdateSubscription frame.
func (f *Front) customGRPCUpdate(ctx context.Context, method string, in frame) error {
	if method == publisher+"UpdateTopic" {
		var req pubsubpb.UpdateTopicRequest
		if proto.Unmarshal(in, &req) != nil {
			return nil
		}
		return f.customUpdateTopic(ctx, req.GetTopic(), req.GetUpdateMask().GetPaths())
	}
	var req pubsubpb.UpdateSubscriptionRequest
	if proto.Unmarshal(in, &req) != nil {
		return nil
	}
	return f.customUpdateSubscription(ctx, req.GetSubscription(), req.GetUpdateMask().GetPaths())
}

// subscriptionPath is the subscription a REST path names,
// /v1/projects/{p}/subscriptions/{s}, with no verb.
func subscriptionPath(path string) (string, bool) {
	rest, found := strings.CutPrefix(path, "/v1/")
	if !found {
		return "", false
	}
	parts := strings.Split(rest, "/")
	if len(parts) != 4 || parts[0] != "projects" || parts[1] == "" || parts[2] != "subscriptions" || parts[3] == "" || strings.Contains(parts[3], ":") {
		return "", false
	}
	return "projects/" + parts[1] + "/subscriptions/" + parts[3], true
}

func restMask(r *http.Request, paths []string) []string {
	if len(paths) == 0 && r.URL.Query().Get("updateMask") != "" {
		return strings.Split(r.URL.Query().Get("updateMask"), ",")
	}
	return paths
}

// customREST checks a REST create (PUT) or update (PATCH) of a topic or
// subscription.
func (f *Front) customREST(r *http.Request) error {
	if (r.Method != http.MethodPut && r.Method != http.MethodPatch) || !orgpolicy.SourceSet() {
		return nil
	}
	topic, verb, isTopic := topicPath(r.URL.Path)
	sub, isSub := subscriptionPath(r.URL.Path)
	if (!isTopic || verb != "") && !isSub {
		return nil
	}
	body, err := readBody(r)
	if err != nil {
		return err
	}
	switch {
	case isTopic && r.Method == http.MethodPut:
		t := &pubsubpb.Topic{}
		if restJSON.Unmarshal(body, t) != nil {
			return nil
		}
		t.Name = topic
		return checkCustom(r.Context(), topic, orgpolicy.TypeTopic, orgpolicy.MethodCreate, t)
	case isTopic:
		var req pubsubpb.UpdateTopicRequest
		if restJSON.Unmarshal(body, &req) != nil || req.GetTopic() == nil {
			return nil
		}
		req.Topic.Name = topic
		return f.customUpdateTopic(r.Context(), req.GetTopic(), restMask(r, req.GetUpdateMask().GetPaths()))
	case r.Method == http.MethodPut:
		s := &pubsubpb.Subscription{}
		if restJSON.Unmarshal(body, s) != nil {
			return nil
		}
		s.Name = sub
		return checkCustom(r.Context(), sub, orgpolicy.TypeSubscription, orgpolicy.MethodCreate, s)
	default:
		var req pubsubpb.UpdateSubscriptionRequest
		if restJSON.Unmarshal(body, &req) != nil || req.GetSubscription() == nil {
			return nil
		}
		req.Subscription.Name = sub
		return f.customUpdateSubscription(r.Context(), req.GetSubscription(), restMask(r, req.GetUpdateMask().GetPaths()))
	}
}

// Custom constraints on pubsub.googleapis.com/Schema and
// pubsub.googleapis.com/Snapshot (#1895), whose documented fields are
// resource.definition, .name and .type, and resource.expireTime, .name
// and .topic (internal/orgpolicy/fields_pubsub.go): CreateSchema is a
// CREATE, CommitSchema (a new revision of the definition) an UPDATE, and
// so is RollbackSchema (#1902), which "creates a new schema revision that
// is a copy of the provided revision_id" (schema.proto): it is checked on
// that revision's type and definition;
// CreateSnapshot a CREATE, UpdateSnapshot an UPDATE.

// customSchema checks a schema as CreateSchema or CommitSchema would
// leave it.
func customSchema(ctx context.Context, name string, s *pubsubpb.Schema, method string) error {
	if !orgpolicy.SourceSet() || name == "" {
		return nil
	}
	res := &pubsubpb.Schema{Name: name, Type: s.GetType(), Definition: s.GetDefinition()}
	return checkCustom(ctx, name, orgpolicy.TypeSchema, method, res)
}

// snapshotLifetime is a new snapshot's longest life: "A newly-created
// snapshot expires no later than 7 days from the time of its creation"
// (google/pubsub/v1/pubsub.proto, Snapshot.expire_time).
const snapshotLifetime = 7 * 24 * time.Hour

// customCreateSnapshot checks the snapshot a CreateSnapshot would make:
// its name, its subscription's topic, and its expiry, on the wall clock,
// as the emulator gives it.
//
// Google computes the expiry as "7 days - (age of oldest unacked message
// in the subscription)" (pubsub.proto, Snapshot.expire_time). The front
// checks 7 days from now, the expiry exactly for a subscription with no
// backlog and the latest one for any other, because it cannot learn the
// oldest unacked message's age (#1902): the emulator reports no backlog
// or message age (no oldest_unacked_message_age, no publish time without
// delivering the message); a Pull to look would lease the messages, count
// a delivery attempt and reorder them (restore.go says the same of
// reading a backlog); and the front does not see every ack, since a
// StreamingPull's acks travel inside a stream it reads only the first
// frame of, and a lapsed ack deadline redelivers inside the emulator. Nor
// can the check run on the emulator's answer: the emulator holds the
// snapshot as soon as it answers, and every client could read or seek to
// it before a refusal took it away. A condition on resource.expireTime
// is therefore read on the latest expiry the snapshot may have.
func (f *Front) customCreateSnapshot(ctx context.Context, req *pubsubpb.CreateSnapshotRequest) error {
	if !orgpolicy.SourceSet() || req.GetName() == "" {
		return nil
	}
	res := &pubsubpb.Snapshot{Name: req.GetName(), ExpireTime: timestamppb.New(f.clock.Wall().Add(snapshotLifetime))}
	if sub, err := f.admin.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: req.GetSubscription()}); err == nil {
		res.Topic = sub.GetTopic()
	}
	return checkCustom(ctx, req.GetName(), orgpolicy.TypeSnapshot, orgpolicy.MethodCreate, res)
}

// customRollbackSchema checks a RollbackSchema on the schema it would
// leave: name, with the type and definition of its revision revisionID,
// read from the front's Protocol Buffer schemas or the emulator's AVRO
// ones. A revision that cannot be read is left to the call to answer
// (NOT_FOUND, or INVALID_ARGUMENT for an empty revision_id).
func (f *Front) customRollbackSchema(ctx context.Context, name, revisionID string) error {
	if !orgpolicy.SourceSet() || name == "" || revisionID == "" {
		return nil
	}
	var rev *pubsubpb.Schema
	if revs, ok := f.keptSchema(name); ok {
		if rev, ok = revision(revs, revisionID); !ok {
			return nil
		}
	} else if f.upstream != nil {
		s, err := pubsubpb.NewSchemaServiceClient(f.upstream).GetSchema(ctx,
			&pubsubpb.GetSchemaRequest{Name: name + "@" + revisionID, View: pubsubpb.SchemaView_FULL})
		if err != nil {
			return nil
		}
		rev = s
	}
	if rev == nil {
		return nil
	}
	return customSchema(ctx, name, rev, orgpolicy.MethodUpdate)
}

// customFirstFrame checks a CreateSchema, CommitSchema, RollbackSchema or CreateSnapshot
// over gRPC, answering a stream that replays its first frame.
func (f *Front) customFirstFrame(ctx context.Context, ss grpc.ServerStream, method string) (grpc.ServerStream, error) {
	if !orgpolicy.SourceSet() {
		return ss, nil
	}
	switch method {
	case schemaService + "CreateSchema", schemaService + "CommitSchema", schemaService + "RollbackSchema", subscriber + "CreateSnapshot":
	default:
		return ss, nil
	}
	var first frame
	if err := ss.RecvMsg(&first); err != nil {
		return nil, err
	}
	var err error
	switch method {
	case schemaService + "CreateSchema":
		var req pubsubpb.CreateSchemaRequest
		if proto.Unmarshal(first, &req) == nil && req.GetSchemaId() != "" {
			err = customSchema(ctx, req.GetParent()+"/schemas/"+req.GetSchemaId(), req.GetSchema(), orgpolicy.MethodCreate)
		}
	case schemaService + "CommitSchema":
		var req pubsubpb.CommitSchemaRequest
		if proto.Unmarshal(first, &req) == nil {
			err = customSchema(ctx, req.GetName(), req.GetSchema(), orgpolicy.MethodUpdate)
		}
	case schemaService + "RollbackSchema":
		var req pubsubpb.RollbackSchemaRequest
		if proto.Unmarshal(first, &req) == nil {
			err = f.customRollbackSchema(ctx, req.GetName(), req.GetRevisionId())
		}
	default:
		var req pubsubpb.CreateSnapshotRequest
		if proto.Unmarshal(first, &req) == nil {
			err = f.customCreateSnapshot(ctx, &req)
		}
	}
	if err != nil {
		return nil, err
	}
	return &replayStream{ServerStream: ss, first: first}, nil
}

// customRESTSchema checks a REST CreateSchema (POST
// /v1/projects/{p}/schemas?schemaId=), CommitSchema (POST
// /v1/{name}:commit) or RollbackSchema (POST /v1/{name}:rollback).
func (f *Front) customRESTSchema(r *http.Request, parent, name, verb string) error {
	if r.Method != http.MethodPost || !orgpolicy.SourceSet() {
		return nil
	}
	switch {
	case name == "" && verb == "":
		id := r.URL.Query().Get("schemaId")
		if id == "" {
			return nil
		}
		body, err := readBody(r)
		if err != nil {
			return err
		}
		s := &pubsubpb.Schema{}
		if restJSON.Unmarshal(body, s) != nil {
			return nil
		}
		return customSchema(r.Context(), parent+"/schemas/"+id, s, orgpolicy.MethodCreate)
	case name != "" && verb == "commit":
		body, err := readBody(r)
		if err != nil {
			return err
		}
		var req pubsubpb.CommitSchemaRequest
		if restJSON.Unmarshal(body, &req) != nil {
			return nil
		}
		return customSchema(r.Context(), name, req.GetSchema(), orgpolicy.MethodUpdate)
	case name != "" && verb == "rollback":
		body, err := readBody(r)
		if err != nil {
			return err
		}
		var req pubsubpb.RollbackSchemaRequest
		if restJSON.Unmarshal(body, &req) != nil {
			return nil
		}
		return f.customRollbackSchema(r.Context(), name, req.GetRevisionId())
	}
	return nil
}

// customRESTSnapshot checks a REST CreateSnapshot (PUT
// /v1/projects/{p}/snapshots/{n}, the body {subscription}).
func (f *Front) customRESTSnapshot(r *http.Request, name string) error {
	if r.Method != http.MethodPut || !orgpolicy.SourceSet() {
		return nil
	}
	body, err := readBody(r)
	if err != nil {
		return err
	}
	var req pubsubpb.CreateSnapshotRequest
	if restJSON.Unmarshal(body, &req) != nil {
		return nil
	}
	req.Name = name
	return f.customCreateSnapshot(r.Context(), &req)
}

// customUpdateSnapshot checks an UpdateSnapshot on the snapshot as it
// would be: its expiry the one the update sets.
func customUpdateSnapshot(ctx context.Context, cur *pubsubpb.Snapshot, expire *time.Time) error {
	if !orgpolicy.SourceSet() {
		return nil
	}
	res := &pubsubpb.Snapshot{Name: cur.GetName(), Topic: cur.GetTopic(), ExpireTime: cur.GetExpireTime()}
	if expire != nil {
		res.ExpireTime = timestamppb.New(*expire)
	}
	return checkCustom(ctx, cur.GetName(), orgpolicy.TypeSnapshot, orgpolicy.MethodUpdate, res)
}
