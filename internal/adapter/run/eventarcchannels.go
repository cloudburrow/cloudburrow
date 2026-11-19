package run

import (
	"context"

	eventarcpb "cloud.google.com/go/eventarc/apiv1/eventarcpb"
	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// Channels and channel connections (#1877) are refused with the reason: a
// channel's events come from a third-party provider, which publishes them
// from its own infrastructure through Eventarc's publishing API once the
// channel is activated with the provider, and no provider can reach this
// instance.
const channelReason = "Eventarc channels are not supported: a channel receives a third-party provider's events, which " +
	"the provider publishes from its own infrastructure once the channel is activated with it, and no provider can reach " +
	"this local instance; Google sources' direct events and Cloud Audit Logs events are served"

func (e *EventarcServer) GetChannel(context.Context, *eventarcpb.GetChannelRequest) (*eventarcpb.Channel, error) {
	return nil, apierror.Unimplemented(channelReason)
}

func (e *EventarcServer) ListChannels(context.Context, *eventarcpb.ListChannelsRequest) (*eventarcpb.ListChannelsResponse, error) {
	return nil, apierror.Unimplemented(channelReason)
}

func (e *EventarcServer) CreateChannel(context.Context, *eventarcpb.CreateChannelRequest) (*longrunningpb.Operation, error) {
	return nil, apierror.Unimplemented(channelReason)
}

func (e *EventarcServer) UpdateChannel(context.Context, *eventarcpb.UpdateChannelRequest) (*longrunningpb.Operation, error) {
	return nil, apierror.Unimplemented(channelReason)
}

func (e *EventarcServer) DeleteChannel(context.Context, *eventarcpb.DeleteChannelRequest) (*longrunningpb.Operation, error) {
	return nil, apierror.Unimplemented(channelReason)
}

func (e *EventarcServer) GetChannelConnection(context.Context, *eventarcpb.GetChannelConnectionRequest) (*eventarcpb.ChannelConnection, error) {
	return nil, apierror.Unimplemented(channelReason)
}

func (e *EventarcServer) ListChannelConnections(context.Context, *eventarcpb.ListChannelConnectionsRequest) (*eventarcpb.ListChannelConnectionsResponse, error) {
	return nil, apierror.Unimplemented(channelReason)
}

func (e *EventarcServer) CreateChannelConnection(context.Context, *eventarcpb.CreateChannelConnectionRequest) (*longrunningpb.Operation, error) {
	return nil, apierror.Unimplemented(channelReason)
}

func (e *EventarcServer) DeleteChannelConnection(context.Context, *eventarcpb.DeleteChannelConnectionRequest) (*longrunningpb.Operation, error) {
	return nil, apierror.Unimplemented(channelReason)
}
