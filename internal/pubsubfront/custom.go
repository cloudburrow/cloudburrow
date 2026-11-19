package pubsubfront

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

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
