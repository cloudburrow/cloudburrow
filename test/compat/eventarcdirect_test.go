//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	eventarcpb "cloud.google.com/go/eventarc/apiv1/eventarcpb"
	functionspb "cloud.google.com/go/functions/apiv2/functionspb"
	"cloud.google.com/go/pubsub/v2"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	redis "cloud.google.com/go/redis/apiv1"
	redispb "cloud.google.com/go/redis/apiv1/redispb"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// directTrigger makes a trigger of one direct resource event type,
// filtered on the resource's ID, delivering to the probe at /direct/TYPE.
func (f *eventarcFixture) directTrigger(t *testing.T, id, typ, attr, resource string) {
	t.Helper()
	f.create(t, id, &eventarcpb.Trigger{Destination: f.toService("/direct/" + typ), EventFilters: []*eventarcpb.EventFilter{
		{Attribute: "type", Value: typ}, {Attribute: attr, Value: resource}}})
}

// directEvent waits for the event of typ the probe got for the resource,
// checks Google's CloudEvent attributes, and decodes its data's payload
// into msg (nil for a deletion that carries none).
func (f *eventarcFixture) directEvent(t *testing.T, typ, service, collection, attr, location, id string, msg proto.Message) {
	t.Helper()
	ev, all := probedEvents(t, f.base, f.host, func(e probedEvent) bool {
		return e.Path == "/direct/"+typ && e.Headers["ce-"+attr] == id
	})
	if ev.Headers == nil {
		t.Errorf("no %s CloudEvent for %s reached the service; it got %d events", typ, id, len(all))
		return
	}
	for k, want := range map[string]string{"ce-type": typ, "ce-specversion": "1.0",
		"ce-source":  "//" + service + "/projects/" + strings.Split(f.parent, "/")[1] + "/locations/" + location,
		"ce-subject": collection + "/" + id, "content-type": "application/json"} {
		if got := ev.Headers[k]; got != want {
			t.Errorf("%s: %s = %q, want %q", typ, k, got, want)
		}
	}
	var data struct {
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal([]byte(ev.Data), &data); err != nil {
		t.Errorf("%s: the data is not JSON: %s", typ, ev.Data)
		return
	}
	if msg == nil {
		if len(data.Payload) != 0 {
			t.Errorf("%s: a deletion's data has a payload: %s", typ, ev.Data)
		}
		return
	}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(data.Payload, msg); err != nil {
		t.Errorf("%s: the payload is not the resource: %v: %s", typ, err, ev.Data)
	}
}

// covers: google.cloud.eventarc.v1.Eventarc/CreateTrigger, google.cloud.eventarc.v1.Eventarc/GetProvider
//
// TestEventarcEventarcAndFunctionsDirectEvents (#1997), with the official
// Eventarc and Cloud Functions clients, as Google's supported-events
// reference lists Eventarc's and Cloud Run functions' direct events and
// the google-cloudevents catalogue documents their data: triggers on
// google.cloud.eventarc.trigger.v1 created, updated and deleted,
// channel.v1 created, updated and deleted, channelConnection.v1 created
// and deleted, and google.cloud.functions.function.v2 created, updated and
// deleted, each filtered on the resource's ID, receive the changes the
// clients make as binary-mode CloudEvents with the resource's extension
// and TriggerEventData, ChannelEventData, ChannelConnectionEventData and
// FunctionEventData in JSON (a function's deletion carries no payload).
// The providers list the types; another resource's changes reach no
// trigger; Speech-to-Text's and Firebase's direct events are UNIMPLEMENTED.
func TestEventarcEventarcAndFunctionsDirectEvents(t *testing.T) {
	h := New(t)
	runShardEndpoint(h, "CLOUDBURROW_TEST_RUN_STORAGE", EnvStorage) // the function's source is a bucket's
	f := newEventarcFixture(t, h, "eventarc-direct-probe")
	fc := functionsClient(t, h)

	for provider, want := range map[string]string{"eventarc.googleapis.com": "google.cloud.eventarc.channelConnection.v1.deleted",
		"cloudfunctions.googleapis.com": "google.cloud.functions.function.v2.updated"} {
		p, err := f.ec.GetProvider(f.ctx, &eventarcpb.GetProviderRequest{Name: f.parent + "/providers/" + provider})
		if err != nil {
			t.Fatalf("GetProvider %s: %v", provider, err)
		}
		var names []string
		for _, et := range p.GetEventTypes() {
			names = append(names, et.GetType())
		}
		if !strings.Contains(strings.Join(names, ","), want) {
			t.Errorf("%s lists %v; want %s", provider, names, want)
		}
	}
	for _, typ := range []string{"google.cloud.speech.phraseSet.v1.created", "google.firebase.remoteconfig.remoteConfig.v1.updated"} {
		_, err := f.ec.CreateTrigger(f.ctx, &eventarcpb.CreateTriggerRequest{Parent: f.parent, TriggerId: "refused",
			Trigger: &eventarcpb.Trigger{EventFilters: []*eventarcpb.EventFilter{{Attribute: "type", Value: typ}}, Destination: f.toService("")}})
		if status.Code(err) != codes.Unimplemented {
			t.Errorf("CreateTrigger of %s = %v; want UNIMPLEMENTED with the reason", typ, err)
		}
	}

	suffix := fmt.Sprint(time.Now().UnixNano() % 1e6)
	target, channel, conn, fn := "ea-target-"+suffix, "ea-chan-"+suffix, "ea-conn-"+suffix, "ea-fn-"+suffix
	for i, typ := range []string{"google.cloud.eventarc.trigger.v1.created", "google.cloud.eventarc.trigger.v1.updated", "google.cloud.eventarc.trigger.v1.deleted"} {
		f.directTrigger(t, fmt.Sprintf("on-trigger-%d", i), typ, "trigger", target)
	}
	for i, typ := range []string{"google.cloud.eventarc.channel.v1.created", "google.cloud.eventarc.channel.v1.updated", "google.cloud.eventarc.channel.v1.deleted"} {
		f.directTrigger(t, fmt.Sprintf("on-channel-%d", i), typ, "channel", channel)
	}
	for i, typ := range []string{"google.cloud.eventarc.channelConnection.v1.created", "google.cloud.eventarc.channelConnection.v1.deleted"} {
		f.directTrigger(t, fmt.Sprintf("on-conn-%d", i), typ, "channelconnection", conn)
	}
	for i, typ := range []string{"google.cloud.functions.function.v2.created", "google.cloud.functions.function.v2.updated", "google.cloud.functions.function.v2.deleted"} {
		f.directTrigger(t, fmt.Sprintf("on-function-%d", i), typ, "function", fn)
	}

	// The Eventarc trigger: create, a labels update, delete.
	tr := f.create(t, target, &eventarcpb.Trigger{Destination: f.toService("/unused"), EventFilters: []*eventarcpb.EventFilter{
		{Attribute: "type", Value: "google.cloud.pubsub.topic.v1.messagePublished"}}})
	other := f.create(t, "ea-other-"+suffix, &eventarcpb.Trigger{Destination: f.toService("/unused"), EventFilters: []*eventarcpb.EventFilter{
		{Attribute: "type", Value: "google.cloud.pubsub.topic.v1.messagePublished"}}})
	time.Sleep(2 * time.Second) // each change is seen on its own read
	uop, err := f.ec.UpdateTrigger(f.ctx, &eventarcpb.UpdateTriggerRequest{Trigger: &eventarcpb.Trigger{Name: tr.GetName(),
		Labels: map[string]string{"team": "events"}}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"labels"}}})
	if err != nil {
		t.Fatalf("UpdateTrigger: %v", err)
	}
	if _, err := uop.Wait(f.ctx); err != nil {
		t.Fatalf("UpdateTrigger: %v", err)
	}
	time.Sleep(2 * time.Second)
	dop, err := f.ec.DeleteTrigger(f.ctx, &eventarcpb.DeleteTriggerRequest{Name: tr.GetName()})
	if err != nil {
		t.Fatalf("DeleteTrigger: %v", err)
	}
	if _, err := dop.Wait(f.ctx); err != nil {
		t.Fatalf("DeleteTrigger: %v", err)
	}

	// The channel: a provider's, PENDING; a connection with its token
	// activates it (an update); then the connection and channel go.
	cop, err := f.ec.CreateChannel(f.ctx, &eventarcpb.CreateChannelRequest{Parent: f.parent, ChannelId: channel,
		Channel: &eventarcpb.Channel{Provider: f.parent + "/providers/acme"}})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	ch, err := cop.Wait(f.ctx)
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	t.Cleanup(func() {
		if op, err := f.ec.DeleteChannel(context.Background(), &eventarcpb.DeleteChannelRequest{Name: ch.GetName()}); err == nil {
			_, _ = op.Wait(context.Background())
		}
	})
	time.Sleep(2 * time.Second)
	ccop, err := f.ec.CreateChannelConnection(f.ctx, &eventarcpb.CreateChannelConnectionRequest{Parent: f.parent, ChannelConnectionId: conn,
		ChannelConnection: &eventarcpb.ChannelConnection{Channel: ch.GetName(), ActivationToken: ch.GetActivationToken()}})
	if err != nil {
		t.Fatalf("CreateChannelConnection: %v", err)
	}
	cc, err := ccop.Wait(f.ctx)
	if err != nil {
		t.Fatalf("CreateChannelConnection: %v", err)
	}
	time.Sleep(2 * time.Second)
	if op, err := f.ec.DeleteChannelConnection(f.ctx, &eventarcpb.DeleteChannelConnectionRequest{Name: cc.GetName()}); err != nil {
		t.Fatalf("DeleteChannelConnection: %v", err)
	} else if _, err := op.Wait(f.ctx); err != nil {
		t.Fatalf("DeleteChannelConnection: %v", err)
	}
	if op, err := f.ec.DeleteChannel(f.ctx, &eventarcpb.DeleteChannelRequest{Name: ch.GetName()}); err != nil {
		t.Fatalf("DeleteChannel: %v", err)
	} else if _, err := op.Wait(f.ctx); err != nil {
		t.Fatalf("DeleteChannel: %v", err)
	}

	// The function: built from a source that is not a zip, so it is
	// FAILED at once; a labels update; delete.
	up, err := fc.GenerateUploadUrl(f.ctx, &functionspb.GenerateUploadUrlRequest{Parent: f.parent})
	if err != nil {
		t.Fatalf("GenerateUploadUrl: %v", err)
	}
	putSource(f.ctx, t, up.GetUploadUrl(), []byte("not a zip"))
	fnName := f.parent + "/functions/" + fn
	t.Cleanup(func() {
		if op, err := fc.DeleteFunction(context.Background(), &functionspb.DeleteFunctionRequest{Name: fnName}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	fop, err := fc.CreateFunction(f.ctx, &functionspb.CreateFunctionRequest{Parent: f.parent, FunctionId: fn, Function: &functionspb.Function{
		BuildConfig: &functionspb.BuildConfig{Runtime: "go123", EntryPoint: "Hello",
			Source: &functionspb.Source{Source: &functionspb.Source_StorageSource{StorageSource: up.GetStorageSource()}}}}})
	if err != nil {
		t.Fatalf("CreateFunction: %v", err)
	}
	_, _ = fop.Wait(f.ctx) // the build fails
	time.Sleep(2 * time.Second)
	fuop, err := fc.UpdateFunction(f.ctx, &functionspb.UpdateFunctionRequest{Function: &functionspb.Function{Name: fnName,
		Labels: map[string]string{"team": "events"}}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"labels"}}})
	if err != nil {
		t.Fatalf("UpdateFunction: %v", err)
	}
	_, _ = fuop.Wait(f.ctx)
	time.Sleep(2 * time.Second)
	fdop, err := fc.DeleteFunction(f.ctx, &functionspb.DeleteFunctionRequest{Name: fnName})
	if err != nil {
		t.Fatalf("DeleteFunction: %v", err)
	}
	if err := fdop.Wait(f.ctx); err != nil {
		t.Fatalf("DeleteFunction: %v", err)
	}

	got := &eventarcpb.Trigger{}
	f.directEvent(t, "google.cloud.eventarc.trigger.v1.created", "eventarc.googleapis.com", "triggers", "trigger", "us-central1", target, got)
	if got.GetName() != tr.GetName() || got.GetDestination().GetCloudRun().GetPath() != "/unused" {
		t.Errorf("TriggerEventData's payload = %v", got)
	}
	got = &eventarcpb.Trigger{}
	f.directEvent(t, "google.cloud.eventarc.trigger.v1.updated", "eventarc.googleapis.com", "triggers", "trigger", "us-central1", target, got)
	if got.GetLabels()["team"] != "events" {
		t.Errorf("the update's payload = %v; want the new labels", got)
	}
	got = &eventarcpb.Trigger{}
	f.directEvent(t, "google.cloud.eventarc.trigger.v1.deleted", "eventarc.googleapis.com", "triggers", "trigger", "us-central1", target, got)
	if got.GetName() != tr.GetName() {
		t.Errorf("the deletion's payload = %v", got)
	}
	gch := &eventarcpb.Channel{}
	f.directEvent(t, "google.cloud.eventarc.channel.v1.created", "eventarc.googleapis.com", "channels", "channel", "us-central1", channel, gch)
	if gch.GetState() != eventarcpb.Channel_PENDING {
		t.Errorf("ChannelEventData's payload on create = %v; want PENDING", gch)
	}
	gch = &eventarcpb.Channel{}
	f.directEvent(t, "google.cloud.eventarc.channel.v1.updated", "eventarc.googleapis.com", "channels", "channel", "us-central1", channel, gch)
	if gch.GetState() != eventarcpb.Channel_ACTIVE {
		t.Errorf("ChannelEventData's payload on activation = %v; want ACTIVE", gch)
	}
	f.directEvent(t, "google.cloud.eventarc.channel.v1.deleted", "eventarc.googleapis.com", "channels", "channel", "us-central1", channel, &eventarcpb.Channel{})
	gcc := &eventarcpb.ChannelConnection{}
	f.directEvent(t, "google.cloud.eventarc.channelConnection.v1.created", "eventarc.googleapis.com", "channelConnections", "channelconnection", "us-central1", conn, gcc)
	if gcc.GetChannel() != ch.GetName() {
		t.Errorf("ChannelConnectionEventData's payload = %v", gcc)
	}
	f.directEvent(t, "google.cloud.eventarc.channelConnection.v1.deleted", "eventarc.googleapis.com", "channelConnections", "channelconnection", "us-central1", conn, &eventarcpb.ChannelConnection{})
	gfn := &functionspb.Function{}
	f.directEvent(t, "google.cloud.functions.function.v2.created", "cloudfunctions.googleapis.com", "functions", "function", "us-central1", fn, gfn)
	if gfn.GetName() != fnName || gfn.GetState() != functionspb.Function_FAILED || gfn.GetBuildConfig().GetRuntime() != "go123" {
		t.Errorf("FunctionEventData's payload = %v", gfn)
	}
	gfn = &functionspb.Function{}
	f.directEvent(t, "google.cloud.functions.function.v2.updated", "cloudfunctions.googleapis.com", "functions", "function", "us-central1", fn, gfn)
	if gfn.GetLabels()["team"] != "events" {
		t.Errorf("the function update's payload = %v", gfn)
	}
	f.directEvent(t, "google.cloud.functions.function.v2.deleted", "cloudfunctions.googleapis.com", "functions", "function", "us-central1", fn, nil)

	_, all := probedEvents(t, f.base, f.host, func(probedEvent) bool { return false })
	otherID := other.GetName()[strings.LastIndex(other.GetName(), "/")+1:]
	for _, e := range all {
		if e.Headers["ce-trigger"] == otherID {
			t.Errorf("another trigger's change reached a filtered trigger: %v", e.Headers)
		}
	}
}

// covers: google.cloud.eventarc.v1.Eventarc/CreateTrigger, google.cloud.eventarc.v1.Eventarc/GetProvider
//
// TestEventarcRedisDirectEvents (#1997), with the official Eventarc and
// Memorystore for Redis clients: triggers on
// google.cloud.redis.instance.v1 created, updated and deleted, filtered on
// the instance's ID, receive its CreateInstance (once READY), a
// display-name UpdateInstance and DeleteInstance as binary-mode
// CloudEvents with the instance extension and InstanceEventData in JSON,
// the Instance as payload except for the deletion, as the
// google-cloudevents catalogue documents it.
func TestEventarcRedisDirectEvents(t *testing.T) {
	h := New(t)
	addr := runShardEndpoint(h, "CLOUDBURROW_TEST_RUN_MEMORYSTORE_ADMIN", EnvMemorystoreAdmin)
	f := newEventarcFixture(t, h, "eventarc-redis-probe")
	c, err := redis.NewCloudRedisClient(f.ctx, option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	p, err := f.ec.GetProvider(f.ctx, &eventarcpb.GetProviderRequest{Name: f.parent + "/providers/redis.googleapis.com"})
	if err != nil {
		t.Fatalf("GetProvider: %v", err)
	}
	var names []string
	for _, et := range p.GetEventTypes() {
		names = append(names, et.GetType())
	}
	if !strings.Contains(strings.Join(names, ","), "google.cloud.redis.instance.v1.created") {
		t.Errorf("redis.googleapis.com lists %v", names)
	}

	id := fmt.Sprintf("ea-redis-%d", time.Now().UnixNano()%1e6)
	for i, kind := range []string{"created", "updated", "deleted"} {
		f.directTrigger(t, fmt.Sprintf("on-redis-%d", i), "google.cloud.redis.instance.v1."+kind, "instance", id)
	}
	name := f.parent + "/instances/" + id
	t.Cleanup(func() {
		if op, err := c.DeleteInstance(context.Background(), &redispb.DeleteInstanceRequest{Name: name}); err == nil {
			_ = waitLRODone(context.Background(), op)
		}
	})
	op, err := c.CreateInstance(f.ctx, &redispb.CreateInstanceRequest{Parent: f.parent, InstanceId: id,
		Instance: &redispb.Instance{Tier: redispb.Instance_BASIC, MemorySizeGb: 1, DisplayName: "first"}})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	if _, err := waitLRO(f.ctx, op); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	time.Sleep(3 * time.Second)
	uop, err := c.UpdateInstance(f.ctx, &redispb.UpdateInstanceRequest{Instance: &redispb.Instance{Name: name, DisplayName: "second"},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"display_name"}}})
	if err != nil {
		t.Fatalf("UpdateInstance: %v", err)
	}
	if _, err := waitLRO(f.ctx, uop); err != nil {
		t.Fatalf("UpdateInstance: %v", err)
	}
	time.Sleep(3 * time.Second)
	dop, err := c.DeleteInstance(f.ctx, &redispb.DeleteInstanceRequest{Name: name})
	if err != nil {
		t.Fatalf("DeleteInstance: %v", err)
	}
	if err := waitLRODone(f.ctx, dop); err != nil {
		t.Fatalf("DeleteInstance: %v", err)
	}

	got := &redispb.Instance{}
	f.directEvent(t, "google.cloud.redis.instance.v1.created", "redis.googleapis.com", "instances", "instance", "us-central1", id, got)
	if got.GetName() != name || got.GetState() != redispb.Instance_READY || got.GetDisplayName() != "first" || got.GetHost() == "" {
		t.Errorf("InstanceEventData's payload on create = %v; want the READY instance", got)
	}
	got = &redispb.Instance{}
	f.directEvent(t, "google.cloud.redis.instance.v1.updated", "redis.googleapis.com", "instances", "instance", "us-central1", id, got)
	if got.GetDisplayName() != "second" {
		t.Errorf("the update's payload = %v; want display name second", got)
	}
	f.directEvent(t, "google.cloud.redis.instance.v1.deleted", "redis.googleapis.com", "instances", "instance", "us-central1", id, nil)
}

// covers: google.cloud.eventarc.v1.Eventarc/CreateTrigger
//
// TestEventarcDeadLetterTopic (#1997), with the official Eventarc and
// Pub/Sub clients, as Google's "Retry events" page documents it: a dead
// letter topic is set on the trigger's Pub/Sub subscription (Eventarc has
// no field of its own), with a maximum of 5 delivery attempts, and a
// short retry backoff. An event whose destination answers 500 every time
// is delivered 5 times and then lands on the dead-letter topic, with its
// data and CloudPubSubDeadLetterSourceDeliveryCount 5.
func TestEventarcDeadLetterTopic(t *testing.T) {
	h := New(t)
	f := newEventarcFixture(t, h, "eventarc-dlq-probe")
	tr := f.create(t, "dlq-trigger", &eventarcpb.Trigger{Destination: f.toService("/fail/dlq"), EventFilters: []*eventarcpb.EventFilter{
		{Attribute: "type", Value: "google.cloud.pubsub.topic.v1.messagePublished"}}})
	sub := tr.GetTransport().GetPubsub().GetSubscription()
	if sub == "" {
		t.Fatalf("the trigger has no subscription: %v", tr)
	}
	dead := topic(t, h, f.pc, fmt.Sprintf("eventarc-dead-%d", time.Now().UnixNano()%1e6))
	deadSub := subscription(t, h, f.pc, fmt.Sprintf("eventarc-dead-sub-%d", time.Now().UnixNano()%1e6), dead)
	if _, err := f.pc.SubscriptionAdminClient.UpdateSubscription(f.ctx, &pubsubpb.UpdateSubscriptionRequest{
		Subscription: &pubsubpb.Subscription{Name: sub,
			DeadLetterPolicy: &pubsubpb.DeadLetterPolicy{DeadLetterTopic: dead, MaxDeliveryAttempts: 5},
			RetryPolicy:      &pubsubpb.RetryPolicy{MinimumBackoff: durationpb.New(time.Second), MaximumBackoff: durationpb.New(2 * time.Second)}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"dead_letter_policy", "retry_policy"}}}); err != nil {
		t.Fatalf("UpdateSubscription of the trigger's subscription: %v", err)
	}
	id, err := f.pc.Publisher(tr.GetTransport().GetPubsub().GetTopic()).Publish(f.ctx, &pubsub.Message{Data: []byte("undeliverable")}).Get(f.ctx)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	var dl *pubsubpb.ReceivedMessage
	for deadline := time.Now().Add(3 * time.Minute); dl == nil && time.Now().Before(deadline); {
		resp, err := f.pc.SubscriptionAdminClient.Pull(f.ctx, &pubsubpb.PullRequest{Subscription: deadSub, MaxMessages: 1, ReturnImmediately: true})
		if err != nil {
			t.Fatalf("Pull the dead-letter subscription: %v", err)
		}
		if len(resp.GetReceivedMessages()) > 0 {
			dl = resp.GetReceivedMessages()[0]
			break
		}
		time.Sleep(2 * time.Second)
	}
	if dl == nil {
		t.Fatal("the undeliverable event never reached the dead-letter topic")
	}
	if string(dl.GetMessage().GetData()) != "undeliverable" || dl.GetMessage().GetAttributes()["CloudPubSubDeadLetterSourceDeliveryCount"] != "5" {
		t.Errorf("the dead-lettered message = %v", dl.GetMessage())
	}
	_, all := probedEvents(t, f.base, f.host, func(probedEvent) bool { return false })
	n := 0
	for _, e := range all {
		if e.Path == "/fail/dlq" && e.Headers["ce-id"] == id {
			n++
		}
	}
	if n != 5 {
		t.Errorf("the destination got the event %d times; want the 5 delivery attempts", n)
	}
}
