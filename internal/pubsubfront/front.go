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
//     retention above the ttl;
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
// A push subscription never expires here: the emulator makes the pushes
// itself, and the front cannot see whether they succeed, so it cannot tell an
// idle push subscription from a busy one.
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

	mu   sync.Mutex
	subs map[string]*subState
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
		subs: map[string]*subState{}}, nil
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
		ttl, expires := subscriptionTTL(sub)
		if !expires || sub.GetPushConfig().GetPushEndpoint() != "" || now.Sub(c.last) < ttl {
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
	if method == ClockMethod {
		return f.handleClock(ss)
	}
	ctx, cancel := context.WithCancel(ss.Context())
	defer cancel()
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		ctx = metadata.NewOutgoingContext(ctx, forwardable(md))
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
	go func() { respErr <- pumpResponses(ss, cs) }()
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
// message and the trailer, and returns its status (nil for OK).
func pumpResponses(ss grpc.ServerStream, cs grpc.ClientStream) error {
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
	if len(o.method) <= len(subscriber) || o.method[:len(subscriber)] != subscriber {
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
		o.sub = s.GetName()
		if s.ExpirationPolicy == nil {
			s.ExpirationPolicy = &pubsubpb.ExpirationPolicy{Ttl: durationpb.New(DefaultTTL)}
			b, err := proto.Marshal(&s)
			if err != nil {
				return nil, status.Errorf(codes.Internal, "re-encode the subscription: %v", err)
			}
			return b, nil
		}
	case "UpdateSubscription":
		var r pubsubpb.UpdateSubscriptionRequest
		if err := proto.Unmarshal(fr, &r); err != nil {
			return fr, nil
		}
		o.sub = r.GetSubscription().GetName()
		if err := o.checkUpdate(&r); err != nil {
			return nil, err
		}
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
	}
	if o.method[len(subscriber):] != "CreateSubscription" {
		o.f.touch(o.sub)
	}
	return fr, nil
}

// checkUpdate refuses a retention above the subscription's ttl.
func (o *observed) checkUpdate(r *pubsubpb.UpdateSubscriptionRequest) error {
	raises := false
	for _, p := range r.GetUpdateMask().GetPaths() {
		if p == "message_retention_duration" || p == "messageRetentionDuration" {
			raises = true
		}
	}
	if !raises || o.sub == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cur, err := o.f.admin.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: o.sub})
	if err != nil {
		return nil // the emulator answers the update itself
	}
	return checkPolicy(cur.GetExpirationPolicy(), r.GetSubscription().GetMessageRetentionDuration())
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
