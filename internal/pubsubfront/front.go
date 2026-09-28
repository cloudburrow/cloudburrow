// Package pubsubfront is a gRPC front for Google's Pub/Sub emulator that
// enforces subscription expiration, which the emulator stores and never acts
// on (#873).
//
// It runs beside the emulator in the Pub/Sub pod and owns the Service's port,
// so every client reaches the emulator through it: the host tunnel, workloads
// in the cluster and the storage server's notifications alike. Every call is
// passed through unchanged, as raw frames, except that:
//
//   - CreateSubscription is refused INVALID_ARGUMENT for an expiration policy
//     Google refuses: a ttl under one day, or under the subscription's message
//     retention duration. A subscription created with no policy is given
//     Google's default of 31 days, so that it reads back as it would from
//     Google and the TTL that is enforced is the one a client sees;
//   - UpdateSubscription is refused the same way when it would raise the
//     retention above the ttl, or set a ttl Google refuses;
//   - an UpdateSubscription of expiration_policy, which the emulator
//     refuses ("Updating the expiration_policy field is currently
//     unsupported in the Pub/Sub Emulator"), is applied by the front
//     (#891): the field is taken out of the mask the emulator sees, the new
//     policy is kept in the front, and every subscription read back through
//     the front (GetSubscription, ListSubscriptions, UpdateSubscription) and
//     every sweep reads it in place of the emulator's. A later
//     CreateSubscription or DeleteSubscription of the name drops it. It is
//     lost if the front restarts, as the emulator's resources are;
//   - exactly-once delivery with a push endpoint, or an export to BigQuery,
//     Cloud Storage or Bigtable, is refused INVALID_ARGUMENT on
//     CreateSubscription, UpdateSubscription and ModifyPushConfig (#880),
//     which the emulator accepts and Google does not: "Push and export
//     subscriptions don't support exactly-once delivery"
//     (https://cloud.google.com/pubsub/docs/exactly-once-delivery);
//   - with the push relay on (push.go, #880), a push subscription's endpoint
//     is the relay's, which forwards each push and counts a successful one
//     as activity, and every subscription read back names its real endpoint;
//   - every call naming a subscription is activity on it, and an open
//     StreamingPull keeps it active for as long as it is open;
//   - a subscription idle for its ttl is deleted.
//
// Google's rules, from the API reference (google/pubsub/v1/pubsub.proto,
// Subscription.expiration_policy) and the subscription properties page
// (https://cloud.google.com/pubsub/docs/subscription-properties):
// "A subscription is considered active as long as any connected subscriber
// is successfully consuming messages from the subscription or is issuing
// operations on the subscription. If expiration_policy is not set, a default
// policy with ttl of 31 days will be used. The minimum allowed value for
// expiration_policy.ttl is 1 day. If expiration_policy is set, but
// expiration_policy.ttl is not set, the subscription never expires." And:
// "Examples of subscriber activities include open connections, active pulls,
// or successful pushes. If you specify the expiration period, the value must
// be at least as long as the message retention duration." Neither names a
// maximum, so none is enforced.
//
// Without the push relay a push subscription never expires: the emulator
// makes the pushes itself, and the front cannot see whether they succeed, so
// it cannot tell an idle push subscription from a busy one. `cloudburrow up`
// always runs the relay.
//
// Time is the front's own clock: the wall clock plus an offset that only
// advances. The offset is moved by one CloudBurrow method on the same port,
// ClockMethod, so a test can pass a day in a moment rather than shorten a
// ttl below what Google accepts. It grants nothing a caller could not
// already do: the emulator has no authentication, and anyone who can reach
// the port can delete any subscription.
package pubsubfront

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	// DefaultTTL is the ttl Google gives a subscription created without an
	// expiration policy.
	DefaultTTL = 31 * 24 * time.Hour
	// MinTTL is the least ttl Google accepts.
	MinTTL = 24 * time.Hour
	// defaultRetention is a subscription's message retention when none is
	// given, which is what a ttl is compared with then.
	defaultRetention = 7 * 24 * time.Hour

	// ClockMethod advances the front's clock. Its request is a
	// google.protobuf.Duration, which must not be negative (zero reads the
	// clock), and its response the front's time after the advance, a
	// google.protobuf.Timestamp. Every subscription idle for its ttl by then
	// is deleted before it answers.
	ClockMethod = "/cloudburrow.pubsub.v1.EmulatorClock/Advance"

	subscriber = "/google.pubsub.v1.Subscriber/"
)

// OffsetClock is the wall clock plus an offset that only grows.
type OffsetClock struct {
	mu     sync.Mutex
	offset time.Duration
	// now is the wall clock; nil means time.Now.
	now func() time.Time
}

// Now returns the wall clock plus the offset.
func (c *OffsetClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now
	if c.now != nil {
		now = c.now
	}
	return now().Add(c.offset)
}

// Advance moves the clock forward by d.
func (c *OffsetClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.offset += d
}

// subState is what the front knows of one subscription's activity.
type subState struct {
	// last is when it was last active.
	last time.Time
	// streams counts its open StreamingPull calls; while any is open it is
	// active.
	streams int
}

// Front proxies to the emulator and expires idle subscriptions.
type Front struct {
	upstream *grpc.ClientConn
	admin    subscriptionAdmin
	clock    *OffsetClock
	logf     func(format string, args ...any)

	// pushClient makes the relay's pushes.
	pushClient *http.Client

	mu   sync.Mutex
	subs map[string]*subState
	// relayBase is the push relay's URL prefix, "" while it is off.
	relayBase string
	// projects is every project a call has named (ProjectsMethod).
	projects map[string]bool
	// policies are the expiration policies UpdateSubscription set, which
	// the emulator refuses to store (#891), by subscription name. Each is
	// what the subscription reads back and what a sweep enforces, in place
	// of the emulator's.
	policies map[string]*pubsubpb.ExpirationPolicy
	// sweeping serialises sweeps, so an advance and the ticker never race
	// to delete one subscription.
	sweeping sync.Mutex
}

// subscriptionAdmin is the part of the emulator's Subscriber service a sweep
// uses.
type subscriptionAdmin interface {
	GetSubscription(ctx context.Context, in *pubsubpb.GetSubscriptionRequest, opts ...grpc.CallOption) (*pubsubpb.Subscription, error)
	DeleteSubscription(ctx context.Context, in *pubsubpb.DeleteSubscriptionRequest, opts ...grpc.CallOption) (*emptypb.Empty, error)
}

// New returns a front for the emulator at upstream (host:port). The
// connection is lazy: nothing is dialled until the first call.
func New(upstream string, logf func(format string, args ...any)) (*Front, error) {
	conn, err := grpc.NewClient(upstream,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(math.MaxInt32), grpc.MaxCallSendMsgSize(math.MaxInt32)))
	if err != nil {
		return nil, fmt.Errorf("the emulator at %s: %w", upstream, err)
	}
	if logf == nil {
		logf = log.Printf
	}
	return &Front{upstream: conn, admin: pubsubpb.NewSubscriberClient(conn), clock: &OffsetClock{}, logf: logf,
		pushClient: &http.Client{}, subs: map[string]*subState{}, projects: map[string]bool{},
		policies: map[string]*pubsubpb.ExpirationPolicy{}}, nil
}

// Close releases the connection to the emulator.
func (f *Front) Close() error { return f.upstream.Close() }

// Server returns the gRPC server that serves the front: every method is
// forwarded, with no size limit of its own, since the emulator applies
// Pub/Sub's.
func (f *Front) Server() *grpc.Server {
	return grpc.NewServer(
		grpc.ForceServerCodec(rawCodec{}),
		grpc.UnknownServiceHandler(f.handle),
		grpc.MaxRecvMsgSize(math.MaxInt32), grpc.MaxSendMsgSize(math.MaxInt32),
		// The official clients ping streaming pulls; the server's default
		// policy would answer a ping more often than every five minutes by
		// closing the connection.
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{MinTime: 10 * time.Second, PermitWithoutStream: true}),
	)
}

// Serve serves the front on l, and sweeps every interval, until ctx ends.
func (f *Front) Serve(ctx context.Context, l net.Listener, interval time.Duration) error {
	srv := f.Server()
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				// Not GracefulStop: an open StreamingPull would hold it forever.
				srv.Stop()
				return
			case <-t.C:
				f.Sweep(ctx)
			}
		}
	}()
	if err := srv.Serve(l); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
		return err
	}
	return nil
}

// Advance moves the clock forward by d and sweeps.
func (f *Front) Advance(ctx context.Context, d time.Duration) time.Time {
	f.clock.Advance(d)
	f.Sweep(ctx)
	return f.clock.Now()
}

// touch records activity on a subscription.
func (f *Front) touch(name string) {
	if name == "" {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.subs[name]
	if s == nil {
		s = &subState{}
		f.subs[name] = s
	}
	s.last = f.clock.Now()
}

// stream records a StreamingPull opening (+1) or closing (-1). Both are
// activity.
func (f *Front) stream(name string, delta int) {
	if name == "" {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.subs[name]
	if s == nil {
		s = &subState{}
		f.subs[name] = s
	}
	s.streams += delta
	if s.streams < 0 {
		s.streams = 0
	}
	s.last = f.clock.Now()
}

// forget drops a subscription that was deleted.
func (f *Front) forget(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.subs, name)
	delete(f.policies, name)
}

// setPolicy keeps the expiration policy an update set.
func (f *Front) setPolicy(name string, p *pubsubpb.ExpirationPolicy) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.policies[name] = proto.Clone(p).(*pubsubpb.ExpirationPolicy)
}

// dropPolicy forgets an updated policy: the subscription is gone, or was
// created again with a policy of its own, which the emulator keeps.
func (f *Front) dropPolicy(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.policies, name)
}

// withPolicy puts the policy an update set, if any, in place of the one the
// emulator returned; it reports whether it changed anything.
func (f *Front) withPolicy(s *pubsubpb.Subscription) bool {
	if s == nil {
		return false
	}
	f.mu.Lock()
	p, ok := f.policies[s.GetName()]
	f.mu.Unlock()
	if !ok || proto.Equal(p, s.GetExpirationPolicy()) {
		return false
	}
	s.ExpirationPolicy = proto.Clone(p).(*pubsubpb.ExpirationPolicy)
	return true
}

// Sweep deletes every subscription idle for its ttl. A subscription's policy
// and delivery type are read from the emulator, which is the authority on
// them; the read is not activity.
func (f *Front) Sweep(ctx context.Context) {
	f.sweeping.Lock()
	defer f.sweeping.Unlock()
	type candidate struct {
		name string
		last time.Time
	}
	var idle []candidate
	f.mu.Lock()
	for name, s := range f.subs {
		if s.streams == 0 {
			idle = append(idle, candidate{name, s.last})
		}
	}
	f.mu.Unlock()
	now := f.clock.Now()
	for _, c := range idle {
		// The shortest ttl there is: nothing younger can be due.
		if now.Sub(c.last) < MinTTL {
			continue
		}
		sub, err := f.admin.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: c.name})
		if status.Code(err) == codes.NotFound {
			f.forgetIfIdleSince(c.name, c.last)
			continue
		}
		if err != nil {
			f.logf("pubsub front: read %s: %v", c.name, err)
			continue
		}
		f.withPolicy(sub)
		ttl, expires := subscriptionTTL(sub)
		// The emulator stores the relay's endpoint; without the relay, a
		// push subscription's pushes are not seen.
		unseen := sub.GetPushConfig().GetPushEndpoint() != "" && f.relaying() == ""
		if !expires || unseen || now.Sub(c.last) < ttl {
			continue
		}
		// Activity may have arrived since the list was taken.
		if !f.idleSince(c.name, c.last) {
			continue
		}
		_, err = f.admin.DeleteSubscription(ctx, &pubsubpb.DeleteSubscriptionRequest{Subscription: c.name})
		if err != nil && status.Code(err) != codes.NotFound {
			f.logf("pubsub front: expire %s: %v", c.name, err)
			continue
		}
		f.dropPolicy(c.name)
		f.forgetIfIdleSince(c.name, c.last)
		f.logf("pubsub front: %s expired, idle for %s (ttl %s)", c.name, now.Sub(c.last), ttl)
	}
}

// idleSince reports whether a subscription has had no activity since last
// and has no stream open.
func (f *Front) idleSince(name string, last time.Time) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.subs[name]
	return s != nil && s.streams == 0 && s.last.Equal(last)
}

func (f *Front) forgetIfIdleSince(name string, last time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s := f.subs[name]; s != nil && s.streams == 0 && s.last.Equal(last) {
		delete(f.subs, name)
	}
}

// subscriptionTTL is a subscription's ttl as Google reads its policy: none is
// the 31-day default, and a policy without a ttl never expires.
func subscriptionTTL(s *pubsubpb.Subscription) (time.Duration, bool) {
	p := s.GetExpirationPolicy()
	if p == nil {
		return DefaultTTL, true
	}
	if ttl := p.GetTtl().AsDuration(); ttl > 0 {
		return ttl, true
	}
	return 0, false
}

// updatedPolicy is the policy an update of expiration_policy sets. An update
// that names the field and gives no policy is read as a subscription created
// without one: Google's 31-day default.
func updatedPolicy(p *pubsubpb.ExpirationPolicy) *pubsubpb.ExpirationPolicy {
	if p == nil {
		return &pubsubpb.ExpirationPolicy{Ttl: durationpb.New(DefaultTTL)}
	}
	return proto.Clone(p).(*pubsubpb.ExpirationPolicy)
}

// retentionOf is a subscription's message retention, or the default.
func retentionOf(d *durationpb.Duration) time.Duration {
	if d == nil {
		return defaultRetention
	}
	return d.AsDuration()
}

// checkPolicy refuses an expiration policy Google refuses.
func checkPolicy(p *pubsubpb.ExpirationPolicy, retention *durationpb.Duration) error {
	if p == nil || p.GetTtl() == nil {
		return nil
	}
	ttl := p.GetTtl().AsDuration()
	switch {
	case ttl < 0:
		return status.Errorf(codes.InvalidArgument, "expiration_policy.ttl must not be negative, got %s", ttl)
	case ttl == 0:
		return nil
	case ttl < MinTTL:
		return status.Errorf(codes.InvalidArgument,
			"expiration_policy.ttl must be at least 1 day, got %s", ttl)
	case ttl < retentionOf(retention):
		return status.Errorf(codes.InvalidArgument,
			"expiration_policy.ttl (%s) must be at least as long as message_retention_duration (%s)", ttl, retentionOf(retention))
	}
	return nil
}

// handle forwards one call of any method to the emulator.
func (f *Front) handle(_ any, ss grpc.ServerStream) error {
	method, ok := grpc.MethodFromServerStream(ss)
	if !ok {
		return status.Error(codes.Internal, "no method on the stream")
	}
	switch method {
	case ClockMethod:
		return f.handleClock(ss)
	case ActivityExportMethod:
		return f.handleActivityExport(ss)
	case ActivityImportMethod:
		return f.handleActivityImport(ss)
	case ProjectsMethod:
		return f.handleProjects(ss)
	}
	ctx, cancel := context.WithCancel(ss.Context())
	defer cancel()
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		ctx = metadata.NewOutgoingContext(ctx, forwardable(md))
	}
	if method == subscriber+"UpdateSubscription" {
		return f.handleUpdate(ctx, ss, method)
	}
	cs, err := f.upstream.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true, ClientStreams: true}, method,
		grpc.ForceCodec(rawCodec{}))
	if err != nil {
		return err
	}
	call := &observed{f: f, method: method}
	defer call.finish()

	reqErr := make(chan error, 1)
	go func() { reqErr <- call.pumpRequests(ss, cs) }()
	respErr := make(chan error, 1)
	go func() { respErr <- pumpResponses(ss, cs, call.response) }()
	for {
		select {
		case err := <-reqErr:
			if err != nil {
				cancel()
				<-respErr
				return err
			}
			reqErr = nil
		case err := <-respErr:
			call.done(err)
			return err
		}
	}
}

// handleUpdate serves UpdateSubscription, a unary call, whole: the front
// applies an update of expiration_policy, which the emulator refuses (#891),
// and forwards the rest. An update of that field alone never reaches the
// emulator, and is answered with the subscription as it now reads.
func (f *Front) handleUpdate(ctx context.Context, ss grpc.ServerStream, method string) error {
	var in frame
	if err := ss.RecvMsg(&in); err != nil {
		return err
	}
	call := &observed{f: f, method: method}
	out, err := call.request(in)
	if err != nil {
		return err
	}
	var resp frame
	if call.local {
		s, err := f.admin.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: call.sub})
		if err != nil {
			call.done(err)
			return err
		}
		b, err := proto.Marshal(s)
		if err != nil {
			return status.Errorf(codes.Internal, "encode the subscription: %v", err)
		}
		resp = b
	} else {
		var header, trailer metadata.MD
		err := f.upstream.Invoke(ctx, method, &out, &resp, grpc.ForceCodec(rawCodec{}), grpc.Header(&header), grpc.Trailer(&trailer))
		if len(header) > 0 {
			_ = ss.SendHeader(header)
		}
		ss.SetTrailer(trailer)
		if err != nil {
			call.done(err)
			return err
		}
	}
	if call.policy != nil {
		f.setPolicy(call.sub, call.policy)
	}
	resp = call.response(resp)
	call.done(nil)
	return ss.SendMsg(&resp)
}

// forwardable is the incoming metadata without the pseudo and transport
// headers the client connection sets for itself.
func forwardable(md metadata.MD) metadata.MD {
	out := metadata.MD{}
	for k, v := range md {
		switch {
		case len(k) > 0 && k[0] == ':', k == "content-type", k == "te", k == "user-agent",
			len(k) > 5 && k[:5] == "grpc-":
			continue
		}
		out[k] = v
	}
	return out
}

// pumpResponses copies the emulator's answer to the client: headers, every
// message, each passed through rewrite, and the trailer, and returns its
// status (nil for OK).
func pumpResponses(ss grpc.ServerStream, cs grpc.ClientStream, rewrite func(frame) frame) error {
	sentHeader := false
	for {
		var fr frame
		err := cs.RecvMsg(&fr)
		if !sentHeader {
			if h, herr := cs.Header(); herr == nil && len(h) > 0 {
				_ = ss.SendHeader(h)
			}
			sentHeader = true
		}
		if err != nil {
			ss.SetTrailer(cs.Trailer())
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		fr = rewrite(fr)
		if err := ss.SendMsg(&fr); err != nil {
			return err
		}
	}
}

// observed is one forwarded call, as far as the front acts on it.
type observed struct {
	f      *Front
	method string
	// sub is the subscription the call names, once its first request is read.
	sub string
	// streaming is set once a StreamingPull has been counted as open.
	streaming bool
	// policy is the expiration policy an UpdateSubscription sets, which
	// the front keeps once the rest of the update succeeds; local is set
	// when it is the whole update, which the emulator then never sees.
	policy *pubsubpb.ExpirationPolicy
	local  bool
}

// pumpRequests copies the client's requests to the emulator, checking and
// observing the first.
func (o *observed) pumpRequests(ss grpc.ServerStream, cs grpc.ClientStream) error {
	first := true
	for {
		var fr frame
		if err := ss.RecvMsg(&fr); err != nil {
			if errors.Is(err, io.EOF) {
				return cs.CloseSend()
			}
			return err
		}
		if first {
			first = false
			out, err := o.request(fr)
			if err != nil {
				return err
			}
			fr = out
		}
		if err := cs.SendMsg(&fr); err != nil {
			// The emulator ended the call; its status comes from the
			// response side.
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

// request checks and records a call's first request, and returns what is
// sent to the emulator in its place.
func (o *observed) request(fr frame) (frame, error) {
	o.f.sawProject(fr)
	if !strings.HasPrefix(o.method, subscriber) {
		return fr, nil
	}
	switch o.method[len(subscriber):] {
	case "CreateSubscription":
		var s pubsubpb.Subscription
		if err := proto.Unmarshal(fr, &s); err != nil {
			return fr, nil // the emulator's refusal is the answer
		}
		if err := checkPolicy(s.GetExpirationPolicy(), s.GetMessageRetentionDuration()); err != nil {
			return nil, err
		}
		if err := checkExactlyOnce(&s); err != nil {
			return nil, err
		}
		o.sub = s.GetName()
		changed := o.f.toRelay(&s)
		if s.ExpirationPolicy == nil {
			s.ExpirationPolicy = &pubsubpb.ExpirationPolicy{Ttl: durationpb.New(DefaultTTL)}
			changed = true
		}
		if changed {
			return reencode(&s)
		}
		return fr, nil
	case "UpdateSubscription":
		var r pubsubpb.UpdateSubscriptionRequest
		if err := proto.Unmarshal(fr, &r); err != nil {
			return fr, nil
		}
		o.sub = r.GetSubscription().GetName()
		o.f.touch(o.sub)
		if err := o.checkUpdate(&r); err != nil {
			return nil, err
		}
		changed := false
		if masks(r.GetUpdateMask().GetPaths(), "expiration_policy") {
			// The emulator refuses the path; the front applies it
			// (handleUpdate) and the emulator sees the rest.
			o.policy = updatedPolicy(r.GetSubscription().GetExpirationPolicy())
			var rest []string
			for _, p := range r.GetUpdateMask().GetPaths() {
				if !masks([]string{p}, "expiration_policy") {
					rest = append(rest, p)
				}
			}
			r.UpdateMask.Paths = rest
			o.local = len(rest) == 0
			changed = true
		}
		if masks(r.GetUpdateMask().GetPaths(), "push_config") && o.f.toRelay(r.GetSubscription()) {
			changed = true
		}
		if changed {
			return reencode(&r)
		}
		return fr, nil
	case "ModifyPushConfig":
		var r pubsubpb.ModifyPushConfigRequest
		if err := proto.Unmarshal(fr, &r); err != nil {
			return fr, nil
		}
		o.sub = r.GetSubscription()
		o.f.touch(o.sub)
		if r.GetPushConfig().GetPushEndpoint() == "" {
			return fr, nil
		}
		if cur, err := o.current(); err == nil && cur.GetEnableExactlyOnceDelivery() {
			return nil, errExactlyOncePush()
		}
		if ep := o.f.relayEndpoint(o.sub, r.GetPushConfig().GetPushEndpoint()); ep != r.GetPushConfig().GetPushEndpoint() {
			r.PushConfig.PushEndpoint = ep
			return reencode(&r)
		}
		return fr, nil
	case "StreamingPull":
		var r pubsubpb.StreamingPullRequest
		if err := proto.Unmarshal(fr, &r); err == nil && r.GetSubscription() != "" {
			o.sub = r.GetSubscription()
			o.streaming = true
			o.f.stream(o.sub, +1)
		}
		return fr, nil
	default:
		o.sub = subscriptionOf(o.method[len(subscriber):], fr)
		o.f.touch(o.sub)
		return fr, nil
	}
}

// reencode is m as it is sent on.
func reencode(m proto.Message) (frame, error) {
	b, err := proto.Marshal(m)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "re-encode the request: %v", err)
	}
	return b, nil
}

// masks reports whether an update mask names field, or a field within it.
// The camelCase form is accepted too, as the emulator accepts it.
func masks(paths []string, field string) bool {
	camel := snakeToCamel(field)
	for _, p := range paths {
		for _, f := range []string{field, camel} {
			if p == f || strings.HasPrefix(p, f+".") {
				return true
			}
		}
	}
	return false
}

func snakeToCamel(s string) string {
	parts := strings.Split(s, "_")
	for i := 1; i < len(parts); i++ {
		if parts[i] != "" {
			parts[i] = strings.ToUpper(parts[i][:1]) + parts[i][1:]
		}
	}
	return strings.Join(parts, "")
}

// errExactlyOncePush is the refusal of exactly-once delivery on a
// subscription that is not a pull subscription. Google documents the rule,
// not its message, so the message quotes the rule.
func errExactlyOncePush() error {
	return status.Error(codes.InvalidArgument, "exactly-once delivery is supported only for pull subscriptions: "+
		"push and export subscriptions don't support exactly-once delivery")
}

// checkExactlyOnce refuses exactly-once delivery on a push or export
// subscription.
func checkExactlyOnce(s *pubsubpb.Subscription) error {
	if !s.GetEnableExactlyOnceDelivery() {
		return nil
	}
	if s.GetPushConfig().GetPushEndpoint() != "" || s.GetBigqueryConfig() != nil ||
		s.GetCloudStorageConfig() != nil || s.GetBigtableConfig() != nil {
		return errExactlyOncePush()
	}
	return nil
}

// current reads the call's subscription from the emulator; the read is not
// activity.
func (o *observed) current() (*pubsubpb.Subscription, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return o.f.admin.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: o.sub})
}

// checkUpdate refuses an update whose result Google refuses: a ttl under a
// day, a retention above the subscription's ttl, or exactly-once delivery on
// a push or export subscription. The result is the current subscription,
// with the policy the front keeps for it, with the masked fields replaced.
func (o *observed) checkUpdate(r *pubsubpb.UpdateSubscriptionRequest) error {
	paths := r.GetUpdateMask().GetPaths()
	retention := masks(paths, "message_retention_duration")
	expiration := masks(paths, "expiration_policy")
	eod := masks(paths, "enable_exactly_once_delivery")
	push := masks(paths, "push_config")
	export := masks(paths, "bigquery_config") || masks(paths, "cloud_storage_config") || masks(paths, "bigtable_config")
	if (!retention && !expiration && !eod && !push && !export) || o.sub == "" {
		return nil
	}
	cur, err := o.current()
	if err != nil {
		if expiration {
			// The front answers this part itself, so it cannot leave the
			// answer to the emulator: NOT_FOUND is the emulator's own.
			return err
		}
		return nil // the emulator answers the update itself
	}
	o.f.withPolicy(cur)
	if retention || expiration {
		policy, ret := cur.GetExpirationPolicy(), cur.GetMessageRetentionDuration()
		if expiration {
			policy = updatedPolicy(r.GetSubscription().GetExpirationPolicy())
		}
		if retention {
			ret = r.GetSubscription().GetMessageRetentionDuration()
		}
		if err := checkPolicy(policy, ret); err != nil {
			return err
		}
	}
	next := proto.Clone(cur).(*pubsubpb.Subscription)
	u := r.GetSubscription()
	if eod {
		next.EnableExactlyOnceDelivery = u.GetEnableExactlyOnceDelivery()
	}
	if push {
		next.PushConfig = u.GetPushConfig()
	}
	if masks(paths, "bigquery_config") {
		next.BigqueryConfig = u.GetBigqueryConfig()
	}
	if masks(paths, "cloud_storage_config") {
		next.CloudStorageConfig = u.GetCloudStorageConfig()
	}
	if masks(paths, "bigtable_config") {
		next.BigtableConfig = u.GetBigtableConfig()
	}
	return checkExactlyOnce(next)
}

// response rewrites what the emulator answers before the client sees it:
// every subscription names its real push endpoint, never the relay's, and
// the expiration policy an update set (#891), never the emulator's.
func (o *observed) response(fr frame) frame {
	if !strings.HasPrefix(o.method, subscriber) {
		return fr
	}
	switch m := o.method[len(subscriber):]; m {
	case "CreateSubscription", "GetSubscription", "UpdateSubscription":
		var s pubsubpb.Subscription
		if proto.Unmarshal(fr, &s) != nil {
			return fr
		}
		changed := o.f.fromRelay(&s)
		if m == "CreateSubscription" {
			// A new subscription: its policy is the one it was created
			// with, which the emulator keeps.
			o.f.dropPolicy(s.GetName())
		} else {
			changed = o.f.withPolicy(&s) || changed
		}
		if !changed {
			return fr
		}
		if b, err := proto.Marshal(&s); err == nil {
			return b
		}
	case "ListSubscriptions":
		var l pubsubpb.ListSubscriptionsResponse
		if proto.Unmarshal(fr, &l) != nil {
			return fr
		}
		changed := false
		for _, s := range l.GetSubscriptions() {
			changed = o.f.fromRelay(s) || changed
			changed = o.f.withPolicy(s) || changed
		}
		if !changed {
			return fr
		}
		if b, err := proto.Marshal(&l); err == nil {
			return b
		}
	}
	return fr
}

// done records a call's outcome once the emulator has answered.
func (o *observed) done(err error) {
	if o.sub == "" {
		return
	}
	switch o.method {
	case subscriber + "CreateSubscription":
		if err == nil {
			o.f.touch(o.sub)
		}
	case subscriber + "DeleteSubscription":
		if err == nil {
			o.f.forget(o.sub)
		}
	case subscriber + "StreamingPull":
	default:
		// A long pull is active until it returns.
		o.f.touch(o.sub)
	}
}

// finish closes a StreamingPull's count, however the call ended.
func (o *observed) finish() {
	if o.streaming {
		o.f.stream(o.sub, -1)
	}
}

// subscriptionOf is the subscription a Subscriber request names, or "".
func subscriptionOf(method string, fr frame) string {
	var m interface {
		proto.Message
		GetSubscription() string
	}
	switch method {
	case "GetSubscription":
		m = &pubsubpb.GetSubscriptionRequest{}
	case "DeleteSubscription":
		m = &pubsubpb.DeleteSubscriptionRequest{}
	case "Pull":
		m = &pubsubpb.PullRequest{}
	case "Acknowledge":
		m = &pubsubpb.AcknowledgeRequest{}
	case "ModifyAckDeadline":
		m = &pubsubpb.ModifyAckDeadlineRequest{}
	case "ModifyPushConfig":
		m = &pubsubpb.ModifyPushConfigRequest{}
	case "Seek":
		m = &pubsubpb.SeekRequest{}
	case "CreateSnapshot":
		m = &pubsubpb.CreateSnapshotRequest{}
	default:
		return ""
	}
	if proto.Unmarshal(fr, m) != nil {
		return ""
	}
	return m.GetSubscription()
}

// handleClock serves ClockMethod.
func (f *Front) handleClock(ss grpc.ServerStream) error {
	var fr frame
	if err := ss.RecvMsg(&fr); err != nil {
		return err
	}
	var d durationpb.Duration
	if err := proto.Unmarshal(fr, &d); err != nil {
		return status.Errorf(codes.InvalidArgument, "the request is not a google.protobuf.Duration: %v", err)
	}
	if err := d.CheckValid(); err != nil || d.AsDuration() < 0 {
		return status.Errorf(codes.InvalidArgument, "the clock only moves forward: %s", d.AsDuration())
	}
	now := f.Advance(ss.Context(), d.AsDuration())
	f.logf("pubsub front: clock advanced by %s to %s", d.AsDuration(), now.UTC().Format(time.RFC3339))
	b, err := proto.Marshal(timestamppb.New(now))
	if err != nil {
		return status.Errorf(codes.Internal, "encode the time: %v", err)
	}
	out := frame(b)
	return ss.SendMsg(&out)
}

// frame is one message as it travels: its encoded bytes, never decoded on the
// way through.
type frame []byte

// rawCodec passes frames through unchanged. Its name is "proto", so the
// content type on both sides is what the clients and the emulator expect.
type rawCodec struct{}

func (rawCodec) Marshal(v any) ([]byte, error) {
	switch m := v.(type) {
	case *frame:
		return *m, nil
	case proto.Message:
		return proto.Marshal(m)
	}
	return nil, fmt.Errorf("pubsubfront: cannot encode %T", v)
}

func (rawCodec) Unmarshal(data []byte, v any) error {
	switch m := v.(type) {
	case *frame:
		*m = append((*m)[:0], data...)
		return nil
	case proto.Message:
		return proto.Unmarshal(data, m)
	}
	return fmt.Errorf("pubsubfront: cannot decode into %T", v)
}

func (rawCodec) Name() string { return "proto" }
