package run

// Channels and channel connections (#1947), as Google's "Publish events
// from custom sources" and "Third-party providers" pages document them. A
// channel is where an application, or a provider, publishes its own
// events through the Publishing API (eventarcpublishing.go), and a
// trigger on the channel receives those of its event type and filters:
//
//   - a channel created with no provider is ACTIVE at once, for an
//     application's custom events;
//   - a channel created for a provider is PENDING, with an activation
//     token; the provider activates it by creating a channel connection
//     (in its own project) with that token, and then publishes to the
//     connection.
//
// Here every provider is local: whoever holds the token creates the
// connection on this instance, with the official client, gcloud or
// Terraform, as the provider would on Google.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	eventarcpb "cloud.google.com/go/eventarc/apiv1/eventarcpb"
	functionspb "cloud.google.com/go/functions/apiv2/functionspb"
	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/protobuf/proto"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

var eaProviderRE = regexp.MustCompile(`^projects/[^/]+/locations/[^/]+/providers/[^/]+$`)

func eaToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func eaCheckChannel(c *eventarcpb.Channel) error {
	if p := c.GetProvider(); p != "" && !eaProviderRE.MatchString(p) {
		return apierror.InvalidArgument("channel.provider %q must be projects/*/locations/*/providers/*", p)
	}
	return eaCheckKey(c.GetCryptoKeyName())
}

func (e *EventarcServer) GetChannel(_ context.Context, req *eventarcpb.GetChannelRequest) (*eventarcpb.Channel, error) {
	return eaGet(eaAdv.channels, req.GetName())
}

func (e *EventarcServer) ListChannels(_ context.Context, req *eventarcpb.ListChannelsRequest) (*eventarcpb.ListChannelsResponse, error) {
	items, next, err := eaList(eaAdv.channels, req.GetParent(), req.GetPageSize(), req.GetPageToken(), "", req.GetOrderBy())
	if err != nil {
		return nil, err
	}
	return &eventarcpb.ListChannelsResponse{Channels: items, NextPageToken: next}, nil
}

// CreateChannel makes a channel: ACTIVE with no provider, PENDING with an
// activation token for a provider.
func (e *EventarcServer) CreateChannel(_ context.Context, req *eventarcpb.CreateChannelRequest) (*longrunningpb.Operation, error) {
	return eaCreate(e, eaAdv.channels, req.GetParent(), req.GetChannelId(), req.GetChannel(), req.GetValidateOnly(), func(c *eventarcpb.Channel) error {
		if err := eaCheckChannel(c); err != nil {
			return err
		}
		c.State = eventarcpb.Channel_ACTIVE
		if c.GetProvider() != "" {
			c.State = eventarcpb.Channel_PENDING
			c.ActivationToken = eaToken()
		}
		return nil
	})
}

func (e *EventarcServer) UpdateChannel(_ context.Context, req *eventarcpb.UpdateChannelRequest) (*longrunningpb.Operation, error) {
	return eaUpdate(e, eaAdv.channels, req.GetChannel(), req.GetUpdateMask(), false, req.GetValidateOnly(), eaCheckChannel)
}

func (e *EventarcServer) DeleteChannel(_ context.Context, req *eventarcpb.DeleteChannelRequest) (*longrunningpb.Operation, error) {
	return eaDelete(e, eaAdv.channels, req.GetName(), "", false, req.GetValidateOnly(), nil)
}

func (e *EventarcServer) GetChannelConnection(_ context.Context, req *eventarcpb.GetChannelConnectionRequest) (*eventarcpb.ChannelConnection, error) {
	return eaGet(eaAdv.conns, req.GetName())
}

func (e *EventarcServer) ListChannelConnections(_ context.Context, req *eventarcpb.ListChannelConnectionsRequest) (*eventarcpb.ListChannelConnectionsResponse, error) {
	items, next, err := eaList(eaAdv.conns, req.GetParent(), req.GetPageSize(), req.GetPageToken(), "", "")
	if err != nil {
		return nil, err
	}
	return &eventarcpb.ListChannelConnectionsResponse{ChannelConnections: items, NextPageToken: next}, nil
}

// CreateChannelConnection connects a provider to a channel; the channel's
// activation token activates a PENDING channel.
func (e *EventarcServer) CreateChannelConnection(_ context.Context, req *eventarcpb.CreateChannelConnectionRequest) (*longrunningpb.Operation, error) {
	token := req.GetChannelConnection().GetActivationToken()
	op, err := eaCreate(e, eaAdv.conns, req.GetParent(), req.GetChannelConnectionId(), req.GetChannelConnection(), false, func(cc *eventarcpb.ChannelConnection) error {
		cc.ActivationToken = token
		ch, err := eaGet(eaAdv.channels, cc.GetChannel())
		if cc.GetChannel() == "" {
			return apierror.InvalidArgument("channel_connection.channel is required")
		}
		if err != nil {
			return apierror.NotFound("the connection's channel %s not found", cc.GetChannel())
		}
		if ch.GetState() == eventarcpb.Channel_PENDING && token != ch.GetActivationToken() {
			return apierror.InvalidArgument("channel_connection.activation_token is not the channel's activation token")
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	eaAdv.mu.Lock()
	if ch, ok := eaAdv.channels.m[req.GetChannelConnection().GetChannel()]; ok && ch.GetState() == eventarcpb.Channel_PENDING {
		ch.State = eventarcpb.Channel_ACTIVE
		ch.ActivationToken = ""
		eaStamp(ch, ch.GetName(), false)
	}
	eaAdv.mu.Unlock()
	return op, nil
}

func (e *EventarcServer) DeleteChannelConnection(_ context.Context, req *eventarcpb.DeleteChannelConnectionRequest) (*longrunningpb.Operation, error) {
	return eaDelete(e, eaAdv.conns, req.GetName(), "", false, false, nil)
}

// eaCheckChannelTrigger checks a trigger on a channel: the channel is in
// its location, and it filters on the event type.
func eaCheckChannelTrigger(t *eventarcpb.Trigger, spec *functionspb.EventTrigger) error {
	if !eaExists(eaAdv.channels, t.GetChannel()) {
		return apierror.InvalidArgument("the trigger's channel %s not found", t.GetChannel())
	}
	if strings.Split(t.GetChannel(), "/")[3] != spec.GetTriggerRegion() {
		return apierror.InvalidArgument("the trigger's channel %s must be in its location", t.GetChannel())
	}
	if spec.GetEventType() == "" {
		return apierror.InvalidArgument("the trigger's event filters must name its event type: attribute \"type\"")
	}
	seen := map[string]bool{}
	for _, f := range spec.GetEventFilters() {
		if seen[f.GetAttribute()] {
			return apierror.InvalidArgument("event filter attribute %q is given more than once", f.GetAttribute())
		}
		seen[f.GetAttribute()] = true
		if f.GetValue() == "" {
			return apierror.InvalidArgument("event filter attribute %q has no value", f.GetAttribute())
		}
		if f.GetOperator() == pathPatternOp {
			if err := validPathPattern(f.GetValue()); err != nil {
				return apierror.InvalidArgument("%v", err)
			}
		}
	}
	return nil
}

// storeChannelTrigger keeps a trigger on a channel: it has no transport
// of its own, since the Publishing API delivers the channel's events.
func (e *EventarcServer) storeChannelTrigger(name string, t *eventarcpb.Trigger, spec *functionspb.EventTrigger) (*longrunningpb.Operation, error) {
	eaStamp(t, name, true)
	t.Etag = eaEtag(t)
	eaState.mu.Lock()
	eaState.triggers[name] = &eaTrigger{t: t, live: &liveTrigger{spec: spec}}
	eaState.mu.Unlock()
	return e.op(name, "create", proto.Clone(t))
}

// eaPublishToChannel delivers events to every trigger on an ACTIVE
// channel whose type and filters they match.
func (f *FunctionsServer) eaPublishToChannel(channel string, events []eaEvent) error {
	ch, err := eaGet(eaAdv.channels, channel)
	if err != nil {
		return err
	}
	if ch.GetState() != eventarcpb.Channel_ACTIVE {
		return apierror.FailedPrecondition("channel %s is %s, not ACTIVE: activate it with a channel connection", channel, ch.GetState())
	}
	for _, ev := range events {
		if err := eaCheckEvent(ev); err != nil {
			return err
		}
	}
	type target struct {
		t    *eventarcpb.Trigger
		spec *functionspb.EventTrigger
	}
	var targets []target
	eaState.mu.Lock()
	for _, n := range sortedKeys(eaState.triggers) {
		x := eaState.triggers[n]
		if x.t.GetChannel() == channel && x.live != nil {
			targets = append(targets, target{proto.Clone(x.t).(*eventarcpb.Trigger), proto.Clone(x.live.spec).(*functionspb.EventTrigger)})
		}
	}
	eaState.mu.Unlock()
	for _, ev := range events {
		for _, tg := range targets {
			if ev.str("type") != tg.spec.GetEventType() || !eaFiltersMatch(tg.spec.GetEventFilters(), func(a string) (string, bool) {
				v, ok := ev.attrs[a]
				return fmt.Sprint(v), ok
			}) {
				continue
			}
			go f.eaDeliverToTrigger(tg.t, tg.spec, ev)
		}
	}
	return nil
}

// eaDeliverToTrigger posts a channel's event to a trigger's destination,
// retrying, as Eventarc's push does, unless the trigger's retry policy
// allows one attempt (UNVERIFIED, re-read 2026-11-19: Google retries a
// channel event for up to 24 hours; here up to 10 attempts, 1 s to 60 s
// apart).
func (f *FunctionsServer) eaDeliverToTrigger(t *eventarcpb.Trigger, spec *functionspb.EventTrigger, ev eaEvent) {
	h := http.Header{}
	for k, v := range ev.attrs {
		if k != "datacontenttype" {
			h.Set("ce-"+k, eaHeaderValue(v))
		}
	}
	if ct := ev.str("datacontenttype"); ct != "" {
		h.Set("Content-Type", ct)
	}
	attempts := 10
	if spec.GetRetryPolicy() == functionspb.EventTrigger_RETRY_POLICY_DO_NOT_RETRY {
		attempts = 1
	}
	eaHooks.mu.Lock()
	unit := eaHooks.retryD
	eaHooks.mu.Unlock()
	delay := time.Second
	for i := 1; ; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		code, err := f.deliverEvent(ctx, t, &CloudEvent{Headers: h.Clone(), Data: ev.data})
		cancel()
		if err == nil && pushSucceeded(code) {
			return
		}
		if i >= attempts {
			slog.Warn("eventarc: a channel event was not delivered", "trigger", t.GetName(), "status", code, "err", err)
			return
		}
		time.Sleep(time.Duration(float64(delay) * float64(unit) / float64(time.Second)))
		if delay *= 2; delay > time.Minute {
			delay = time.Minute
		}
	}
}

// eaChannelOfConnection is the channel a connection publishes to.
func eaChannelOfConnection(name string) (string, error) {
	cc, err := eaGet(eaAdv.conns, name)
	if err != nil {
		return "", err
	}
	if !strings.Contains(cc.GetChannel(), "/channels/") {
		return "", apierror.FailedPrecondition("channel connection %s names no channel", name)
	}
	return cc.GetChannel(), nil
}
