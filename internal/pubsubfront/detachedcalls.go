package pubsubfront

// The calls on a detached subscription other than Get, List, Delete, Pull,
// StreamingPull and CreateSubscription (#1446). The emulator no longer holds
// the subscription, so the front answers them from the configuration it
// keeps (detach.go), by what Google documents of a detached subscription:
//
//   - it is still a subscription: GetSubscription and ListSubscriptions
//     return it (https://cloud.google.com/pubsub/docs/detach-subscriptions),
//     so UpdateSubscription and ModifyPushConfig change its configuration as
//     they change any subscription's, with the checks the front makes of any
//     update (labels, expiration policy against retention, exactly-once
//     delivery with a push endpoint). Its name and topic are immutable, as
//     on any subscription, and "detached" cannot be cleared: "You can't ...
//     reattach the subscription to a topic." Pushes stay stopped ("If the
//     subscription is a push subscription, pushes to the endpoint will
//     stop"), whatever the push endpoint;
//   - it "don't retain any backlog" (Subscription.detached) and "All messages
//     retained in the subscription are dropped" (DetachSubscription): no
//     message is outstanding, so Acknowledge and ModifyAckDeadline, which
//     Google documents as never failing for an ack ID acknowledged or
//     expired ("Acknowledging a message more than once will not result in an
//     error"), succeed and change nothing. With exactly-once delivery Google
//     reports each ack ID it no longer holds instead, as INVALID_ARGUMENT
//     with an ErrorInfo whose metadata maps it to
//     PERMANENT_FAILURE_INVALID_ACK_ID (https://cloud.google.com/pubsub/docs/exactly-once-delivery),
//     which the official clients read, and so does the front;
//   - a Seek to a time marks the messages retained in the subscription
//     before it acknowledged and those after it unacknowledged (Seek): there
//     are none, so it succeeds and changes nothing;
//   - a Seek to a snapshot needs "The snapshot's topic [to] be the same as
//     that of the provided subscription", and a snapshot retains "Any
//     messages published to the subscription's topic following the
//     successful completion of the CreateSnapshot request" (CreateSnapshot):
//     a detached subscription has no topic, so both fail FAILED_PRECONDITION
//     with the reason (a seek to a snapshot that does not exist is NOT_FOUND
//     first, as on any subscription).

import (
	"context"
	"net/http"
	"strings"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// permanentInvalidAckID is Google's verdict on an ack ID it no longer
// holds, reported per ack ID with exactly-once delivery.
const permanentInvalidAckID = "PERMANENT_FAILURE_INVALID_ACK_ID"

// detachedOther answers a Subscriber call m, its request fr, on the
// detached subscription s.
func (f *Front) detachedOther(m string, fr frame, s *pubsubpb.Subscription) (proto.Message, error) {
	var req proto.Message
	switch m {
	case "UpdateSubscription":
		req = &pubsubpb.UpdateSubscriptionRequest{}
	case "ModifyPushConfig":
		req = &pubsubpb.ModifyPushConfigRequest{}
	case "Acknowledge":
		req = &pubsubpb.AcknowledgeRequest{}
	case "ModifyAckDeadline":
		req = &pubsubpb.ModifyAckDeadlineRequest{}
	case "Seek":
		req = &pubsubpb.SeekRequest{}
	case "CreateSnapshot":
		req = &pubsubpb.CreateSnapshotRequest{}
	default:
		return nil, apierror.Unimplemented("method google.pubsub.v1.Subscriber/%s is not implemented", m)
	}
	if err := proto.Unmarshal(fr, req); err != nil {
		return nil, apierror.InvalidArgument("the request is not a google.pubsub.v1.%s: %v",
			req.ProtoReflect().Descriptor().Name(), err)
	}
	return f.onDetached(context.Background(), req, s)
}

// onDetached carries out req on the detached subscription s.
func (f *Front) onDetached(ctx context.Context, req proto.Message, s *pubsubpb.Subscription) (proto.Message, error) {
	name := s.GetName()
	switch r := req.(type) {
	case *pubsubpb.UpdateSubscriptionRequest:
		return f.updateDetached(r, s)
	case *pubsubpb.ModifyPushConfigRequest:
		next := proto.Clone(s).(*pubsubpb.Subscription)
		next.PushConfig = r.GetPushConfig()
		if err := checkExactlyOnce(next); err != nil {
			return nil, err
		}
		f.keepDetached(next)
		return &emptypb.Empty{}, nil
	case *pubsubpb.AcknowledgeRequest:
		if err := invalidAckIDs(s, r.GetAckIds()); err != nil {
			return nil, err
		}
		return &emptypb.Empty{}, nil
	case *pubsubpb.ModifyAckDeadlineRequest:
		if r.GetAckDeadlineSeconds() < 0 || r.GetAckDeadlineSeconds() > 600 {
			return nil, apierror.InvalidArgument("ack_deadline_seconds must be between 0 and 600, got %d", r.GetAckDeadlineSeconds())
		}
		if err := invalidAckIDs(s, r.GetAckIds()); err != nil {
			return nil, err
		}
		return &emptypb.Empty{}, nil
	case *pubsubpb.SeekRequest:
		snap := r.GetSnapshot()
		if snap == "" {
			if r.GetTime() == nil {
				return nil, apierror.InvalidArgument("Seek requires a time or a snapshot")
			}
			return &pubsubpb.SeekResponse{}, nil
		}
		if _, err := f.snapshotAdmin().GetSnapshot(ctx, &pubsubpb.GetSnapshotRequest{Snapshot: snap}); err != nil {
			return nil, err
		}
		return nil, errDetachedTopic("Seek to a snapshot", name)
	case *pubsubpb.CreateSnapshotRequest:
		return nil, errDetachedTopic("CreateSnapshot", name)
	}
	return nil, apierror.Unimplemented("%T is not a Subscriber request", req)
}

// invalidAckIDs is Google's report of ack IDs it no longer holds, on a
// subscription with exactly-once delivery; nil without it.
func invalidAckIDs(s *pubsubpb.Subscription, ids []string) error {
	if !s.GetEnableExactlyOnceDelivery() || len(ids) == 0 {
		return nil
	}
	md := make(map[string]string, len(ids))
	for _, id := range ids {
		md[id] = permanentInvalidAckID
	}
	st := status.New(codes.InvalidArgument, "Some acknowledgement ids in the request were invalid. "+
		"This could be because the acknowledgement ids have expired or the acknowledgement ids were malformed.")
	if d, err := st.WithDetails(&errdetails.ErrorInfo{Reason: "EXACTLY_ONCE_ACKID_FAILURE",
		Domain: "pubsub.googleapis.com", Metadata: md}); err == nil {
		st = d
	}
	return st.Err()
}

// keepDetached replaces a detached subscription's kept configuration.
func (f *Front) keepDetached(s *pubsubpb.Subscription) {
	f.mu.Lock()
	f.detached[s.GetName()] = proto.Clone(s).(*pubsubpb.Subscription)
	f.mu.Unlock()
	f.persist()
}

// updateDetached applies an UpdateSubscription to the detached
// subscription s, and answers it as it now reads.
func (f *Front) updateDetached(r *pubsubpb.UpdateSubscriptionRequest, s *pubsubpb.Subscription) (*pubsubpb.Subscription, error) {
	paths := r.GetUpdateMask().GetPaths()
	if len(paths) == 0 {
		return nil, apierror.InvalidArgument("update_mask must name at least one field")
	}
	next := proto.Clone(s).(*pubsubpb.Subscription)
	src := r.GetSubscription()
	if src == nil {
		src = &pubsubpb.Subscription{}
	}
	for _, p := range paths {
		snake := camelToSnake(p)
		switch top, _, _ := strings.Cut(snake, "."); top {
		case "name", "topic":
			return nil, apierror.InvalidArgument("Invalid update_mask provided in the UpdateSubscriptionRequest: "+
				"the field %s cannot be updated", top)
		case "detached":
			if !src.GetDetached() {
				return nil, apierror.FailedPrecondition("The subscription %s is detached: a detached subscription "+
					"cannot be reattached to a topic", s.GetName())
			}
			continue
		}
		if err := copyPath(next.ProtoReflect(), src.ProtoReflect(), strings.Split(snake, ".")); err != nil {
			return nil, err
		}
	}
	if masks(paths, "labels") {
		if err := CheckLabels(next.GetLabels()); err != nil {
			return nil, err
		}
	}
	if masks(paths, "expiration_policy") || masks(paths, "message_retention_duration") {
		if err := checkPolicy(next.GetExpirationPolicy(), next.GetMessageRetentionDuration()); err != nil {
			return nil, err
		}
	}
	if err := checkExactlyOnce(next); err != nil {
		return nil, err
	}
	next.Name, next.Topic, next.Detached = s.GetName(), deletedTopic, true
	f.keepDetached(next)
	return next, nil
}

// copyPath sets the field path names in dst to its value in src, as a
// field mask's path is applied: a message field along the way is made in
// dst when src has it.
func copyPath(dst, src protoreflect.Message, path []string) error {
	fd := dst.Descriptor().Fields().ByName(protoreflect.Name(path[0]))
	if fd == nil {
		return apierror.InvalidArgument("Invalid update_mask provided in the UpdateSubscriptionRequest: "+
			"%s is not a known Subscription field", path[0])
	}
	if len(path) == 1 {
		if src.Has(fd) {
			dst.Set(fd, src.Get(fd))
		} else {
			dst.Clear(fd)
		}
		return nil
	}
	if fd.Message() == nil || fd.IsList() || fd.IsMap() {
		return apierror.InvalidArgument("Invalid update_mask provided in the UpdateSubscriptionRequest: "+
			"%s has no field %s", path[0], path[1])
	}
	if !src.Has(fd) && !dst.Has(fd) {
		return nil
	}
	return copyPath(dst.Mutable(fd).Message(), src.Get(fd).Message(), path[1:])
}

// restDetachedRequest is the Subscriber request a REST call on the detached
// subscription name makes, by Pub/Sub's HTTP rules; nil when it is none.
func restDetachedRequest(r *http.Request, name, verb string, body []byte) (proto.Message, error) {
	var req proto.Message
	switch {
	case verb == "" && r.Method == http.MethodPatch:
		req = &pubsubpb.UpdateSubscriptionRequest{}
	case r.Method != http.MethodPost:
		return nil, nil
	case verb == "acknowledge":
		req = &pubsubpb.AcknowledgeRequest{}
	case verb == "modifyAckDeadline":
		req = &pubsubpb.ModifyAckDeadlineRequest{}
	case verb == "modifyPushConfig":
		req = &pubsubpb.ModifyPushConfigRequest{}
	case verb == "seek":
		req = &pubsubpb.SeekRequest{}
	default:
		return nil, nil
	}
	if len(strings.TrimSpace(string(body))) > 0 {
		if err := restJSON.Unmarshal(body, req); err != nil {
			return nil, apierror.InvalidArgument("the body is not a %s: %v", req.ProtoReflect().Descriptor().Name(), err)
		}
	}
	if u, ok := req.(*pubsubpb.UpdateSubscriptionRequest); ok {
		if u.UpdateMask == nil && r.URL.Query().Get("updateMask") != "" {
			u.UpdateMask = &fieldmaskpb.FieldMask{Paths: splitMask(r.URL.Query().Get("updateMask"), nil)}
		}
		if u.Subscription == nil {
			u.Subscription = &pubsubpb.Subscription{}
		}
		u.Subscription.Name = name
	}
	return req, nil
}
