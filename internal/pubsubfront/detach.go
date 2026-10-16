package pubsubfront

// DetachSubscription (#1406). The emulator refuses it, UNIMPLEMENTED
// "Method google.pubsub.v1.Publisher/DetachSubscription is unimplemented",
// so the front carries it out from calls the emulator serves, as it does
// labels updates (#949).
//
// Google documents a detach as: "Detaches a subscription from this topic.
// All messages retained in the subscription are dropped. Subsequent Pull and
// StreamingPull requests will return FAILED_PRECONDITION. If the
// subscription is a push subscription, pushes to the endpoint will stop."
// (google/pubsub/v1/pubsub.proto, Publisher.DetachSubscription), and
// Subscription.detached as "Indicates whether the subscription is detached
// from its topic. Detached subscriptions don't receive messages from their
// topic and don't retain any backlog." A detached subscription is still a
// subscription: GetSubscription and ListSubscriptions return it, with its
// topic "_deleted-topic_" (https://cloud.google.com/pubsub/docs/detach-subscriptions),
// and the topic's ListTopicSubscriptions no longer names it.
//
// The front reads the subscription as it then is (with the policy, labels
// and push endpoint the front keeps for it), deletes it from the emulator,
// which drops its backlog, stops its pushes and takes it off the topic's
// list, so nothing published afterwards reaches it, and keeps the
// configuration itself, detached. From then on:
//   - GetSubscription answers the kept configuration, ListSubscriptions of
//     its project names it after the emulator's last page, and
//     DeleteSubscription forgets it;
//   - Pull and StreamingPull fail FAILED_PRECONDITION, as documented;
//   - CreateSubscription of the name fails ALREADY_EXISTS, as the name is
//     still taken;
//   - a detach of it again succeeds and changes nothing;
//   - the other calls are answered from what Google documents of a
//     detached subscription, that it is still a subscription, that it
//     "don't retain any backlog" and that it can't be reattached
//     (detachedcalls.go, #1446): UpdateSubscription and ModifyPushConfig
//     change the kept configuration; Acknowledge and ModifyAckDeadline
//     succeed and change nothing, as there is no outstanding message (with
//     exactly-once delivery each ack ID is reported invalid, as Google
//     reports an ack ID it no longer holds); a Seek to a time succeeds and
//     changes nothing, a Seek to a snapshot and CreateSnapshot fail
//     FAILED_PRECONDITION, as both need the subscription's topic.
// A detached subscription never expires in the front: it has no activity to
// measure. With a state file (state.go) it survives a restart of the front.

import (
	"context"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// deletedTopic is the topic Pub/Sub names for a detached subscription.
const deletedTopic = "_deleted-topic_"

// errDetachedPull is the refusal of a pull from a detached subscription.
func errDetachedPull(name string) error {
	return apierror.FailedPrecondition("The subscription %s is detached from its topic: "+
		"pulls from a detached subscription return FAILED_PRECONDITION", name)
}

// errDetachedTopic is the refusal of a call that needs the subscription's
// topic, which a detached subscription no longer has.
func errDetachedTopic(method, name string) error {
	return apierror.FailedPrecondition("%s needs the subscription's topic, and the subscription %s is detached from it", method, name)
}

// detachedSub is the kept configuration of a detached subscription, a copy,
// and whether name is one.
func (f *Front) detachedSub(name string) (*pubsubpb.Subscription, bool) {
	if name == "" {
		return nil, false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.detached[name]
	if !ok {
		return nil, false
	}
	return proto.Clone(s).(*pubsubpb.Subscription), true
}

// detachedIn is every detached subscription of project ("projects/p"), by
// name.
func (f *Front) detachedIn(project string) []*pubsubpb.Subscription {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*pubsubpb.Subscription
	for name, s := range f.detached {
		if strings.HasPrefix(name, project+"/subscriptions/") {
			out = append(out, proto.Clone(s).(*pubsubpb.Subscription))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GetName() < out[j].GetName() })
	return out
}

// DetachSubscription detaches a subscription from its topic, as Google
// documents it.
func (f *Front) DetachSubscription(ctx context.Context, req *pubsubpb.DetachSubscriptionRequest) (*pubsubpb.DetachSubscriptionResponse, error) {
	name := req.GetSubscription()
	if name == "" {
		return nil, apierror.InvalidArgument("subscription is required")
	}
	if _, ok := f.detachedSub(name); ok {
		return &pubsubpb.DetachSubscriptionResponse{}, nil
	}
	sub, err := f.subscription(ctx, name)
	if err != nil {
		return nil, err
	}
	f.fromRelay(sub)
	f.withPolicy(sub)
	f.withLabels(sub.GetName(), &sub.Labels)
	sub.Topic = deletedTopic
	sub.Detached = true
	if _, err := f.admin.DeleteSubscription(ctx, &pubsubpb.DeleteSubscriptionRequest{Subscription: name}); err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.detached[name] = sub
	// Its activity, policy and labels are in the kept configuration now;
	// the IAM policy stays, as the subscription does.
	delete(f.subs, name)
	delete(f.policies, name)
	delete(f.labels, name)
	f.mu.Unlock()
	f.persist()
	return &pubsubpb.DetachSubscriptionResponse{}, nil
}

// forgetDetached drops a detached subscription; it reports whether name was
// one.
func (f *Front) forgetDetached(name string) bool {
	f.mu.Lock()
	_, ok := f.detached[name]
	delete(f.detached, name)
	f.mu.Unlock()
	if ok {
		f.forget(name)
		f.persist()
	}
	return ok
}

// detachedCall answers a Subscriber call on a detached subscription, the
// whole call, when the call names one: handled reports whether it did.
func (f *Front) detachedCall(method string, fr frame) (resp proto.Message, handled bool, err error) {
	m := method[len(subscriber):]
	var name string
	switch m {
	case "CreateSubscription":
		var s pubsubpb.Subscription
		if proto.Unmarshal(fr, &s) == nil {
			name = s.GetName()
		}
	case "UpdateSubscription":
		var r pubsubpb.UpdateSubscriptionRequest
		if proto.Unmarshal(fr, &r) == nil {
			name = r.GetSubscription().GetName()
		}
	case "StreamingPull":
		var r pubsubpb.StreamingPullRequest
		if proto.Unmarshal(fr, &r) == nil {
			name = r.GetSubscription()
		}
	default:
		name = subscriptionOf(m, fr)
	}
	s, ok := f.detachedSub(name)
	if !ok {
		return nil, false, nil
	}
	switch m {
	case "GetSubscription":
		return s, true, nil
	case "DeleteSubscription":
		f.forgetDetached(name)
		return &emptypb.Empty{}, true, nil
	case "Pull", "StreamingPull":
		return nil, true, errDetachedPull(name)
	case "CreateSubscription":
		return nil, true, apierror.AlreadyExists("Subscription already exists: %s", name)
	}
	resp, err = f.detachedOther(m, fr, s)
	return resp, true, err
}

// handleDetach serves Publisher/DetachSubscription over gRPC.
func (f *Front) handleDetach(ctx context.Context, ss grpc.ServerStream) error {
	var in frame
	if err := ss.RecvMsg(&in); err != nil {
		return err
	}
	f.sawProject(in)
	var req pubsubpb.DetachSubscriptionRequest
	if err := proto.Unmarshal(in, &req); err != nil {
		return apierror.InvalidArgument("the request is not a google.pubsub.v1.DetachSubscriptionRequest: %v", err)
	}
	resp, err := f.DetachSubscription(ctx, &req)
	if err != nil {
		return err
	}
	return sendProto(ss, resp)
}

// sendProto answers a unary gRPC call the front serves itself.
func sendProto(ss grpc.ServerStream, m proto.Message) error {
	b, err := proto.Marshal(m)
	if err != nil {
		return apierror.Internal(err, "encode the answer")
	}
	out := frame(b)
	return ss.SendMsg(&out)
}

// withDetached is a ListSubscriptions page with the project's detached
// subscriptions after the emulator's last page.
func (f *Front) withDetached(project string, l *pubsubpb.ListSubscriptionsResponse) bool {
	if l.GetNextPageToken() != "" || project == "" {
		return false
	}
	d := f.detachedIn(project)
	l.Subscriptions = append(l.Subscriptions, d...)
	return len(d) > 0
}

// writeProtoJSON answers a REST call with m.
func writeProtoJSON(w http.ResponseWriter, m proto.Message) {
	b, err := protojson.Marshal(m)
	if err != nil {
		writeRESTError(w, apierror.Internal(err, "encode the answer"))
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	_, _ = w.Write(b)
}

// serveRESTDetached answers a REST call on a subscription when it is, or
// makes it, detached; it reports whether it answered. verb is the custom
// verb after the colon, "" for none.
func (f *Front) serveRESTDetached(w http.ResponseWriter, r *http.Request, name, verb string) bool {
	if verb == "detach" && r.Method == http.MethodPost {
		resp, err := f.DetachSubscription(r.Context(), &pubsubpb.DetachSubscriptionRequest{Subscription: name})
		if err != nil {
			writeRESTError(w, err)
			return true
		}
		writeProtoJSON(w, resp)
		return true
	}
	s, ok := f.detachedSub(name)
	if !ok {
		return false
	}
	switch {
	case verb == "" && r.Method == http.MethodGet:
		writeProtoJSON(w, s)
	case verb == "" && r.Method == http.MethodDelete:
		f.forgetDetached(name)
		w.Header().Set("Content-Type", "application/json; charset=UTF-8")
		_, _ = w.Write([]byte("{}"))
	case verb == "" && r.Method == http.MethodPut:
		writeRESTError(w, apierror.AlreadyExists("Subscription already exists: %s", name))
	case verb == "pull":
		writeRESTError(w, errDetachedPull(name))
	default:
		body, err := readBody(r)
		if err != nil {
			writeRESTError(w, err)
			return true
		}
		req, err := restDetachedRequest(r, name, verb, body)
		if err == nil && req == nil {
			err = apierror.NotFound("%s %s is not a Pub/Sub method", r.Method, r.URL.Path)
		}
		if err != nil {
			writeRESTError(w, err)
			return true
		}
		resp, err := f.onDetached(r.Context(), req, s)
		if err != nil {
			writeRESTError(w, err)
			return true
		}
		writeProtoJSON(w, resp)
	}
	return true
}

// restListSubscriptions answers GET /v1/projects/{p}/subscriptions when the
// project has a detached subscription, which the emulator's list no longer
// holds: the emulator's page is read over gRPC, rewritten as every list is,
// and the detached subscriptions follow its last page. It reports whether it
// answered.
func (f *Front) restListSubscriptions(w http.ResponseWriter, r *http.Request) bool {
	rest, found := strings.CutPrefix(r.URL.Path, "/v1/projects/")
	if !found || r.Method != http.MethodGet {
		return false
	}
	p, c, _ := strings.Cut(rest, "/")
	if p == "" || c != "subscriptions" {
		return false
	}
	project := "projects/" + p
	if len(f.detachedIn(project)) == 0 {
		return false
	}
	req := &pubsubpb.ListSubscriptionsRequest{Project: project, PageToken: r.URL.Query().Get("pageToken")}
	if n := r.URL.Query().Get("pageSize"); n != "" {
		size, err := strconv.ParseInt(n, 10, 32)
		if err != nil {
			writeRESTError(w, apierror.InvalidArgument("pageSize %q is not a number", n))
			return true
		}
		req.PageSize = int32(size)
	}
	l, err := pubsubpb.NewSubscriberClient(f.upstream).ListSubscriptions(r.Context(), req)
	if err != nil {
		writeRESTError(w, err)
		return true
	}
	for _, s := range l.GetSubscriptions() {
		f.fromRelay(s)
		f.withPolicy(s)
		f.withLabels(s.GetName(), &s.Labels)
	}
	f.withDetached(project, l)
	writeProtoJSON(w, l)
	return true
}
