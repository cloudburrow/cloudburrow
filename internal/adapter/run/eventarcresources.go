package run

// Direct events of the Google sources this instance serves (#1997), as
// Google's "Event providers and destinations" page lists them and its
// supported-events reference names them, each with the data the
// google-cloudevents catalogue documents:
//
//   - Memorystore for Redis: google.cloud.redis.instance.v1.created,
//     updated and deleted, with InstanceEventData (the Instance as payload,
//     unset for a deletion) and the instance extension;
//   - Cloud Run functions: google.cloud.functions.function.v2.created,
//     updated and deleted, with FunctionEventData (the Function, unset for
//     a deletion) and the function extension;
//   - Eventarc: google.cloud.eventarc.trigger.v1, channel.v1 (created,
//     updated, deleted) and channelConnection.v1 (created, deleted), with
//     TriggerEventData, ChannelEventData and ChannelConnectionEventData
//     (the resource as payload) and the trigger, channel and
//     channelconnection extensions.
//
// The data is the payload message in JSON, the content type a trigger
// gets unless it asks for another (application/json). A trigger can filter
// on the extension, the resource's ID. Each source's resources are read
// each second (WatchResources) for every project a trigger of its types
// names; a resource is reported created once it has settled (Redis READY,
// a function ACTIVE or FAILED), updated when a settled read differs from
// the last, and deleted when it is gone. The first read when a trigger is
// made is the baseline, so only later changes are reported.
//
// Speech-to-Text's direct events are v1 Adaptation's custom classes and
// phrase sets (google.events.cloud.speech.v1), and this instance does not
// serve v1's Adaptation service, so they are refused with that reason; so
// are the Firebase providers' and every other source's (eventReason).
//
// UNVERIFIED (re-read 2026-11-20:
// <https://docs.cloud.google.com/eventarc/docs/reference/supported-events>,
// <https://github.com/googleapis/google-cloudevents/tree/main/proto/google/events/cloud>,
// which do not say): ce-source //SERVICE/projects/P/locations/L and
// ce-subject COLLECTION/ID, the extension being optional and exact-match
// only, and when "created" fires for a resource made by an operation.

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	functionspb "cloud.google.com/go/functions/apiv2/functionspb"
	redispb "cloud.google.com/go/redis/apiv1/redispb"
	"github.com/google/uuid"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// eaResourceSource is one kind of resource whose changes are direct
// events.
type eaResourceSource struct {
	service    string // redis.googleapis.com
	prefix     string // google.cloud.redis.instance.v1.
	attr       string // the CloudEvent extension: instance
	collection string // instances
	noun       string // a Memorystore for Redis instance
	schema     string // the catalogue's data.proto
	kinds      []string
	// deletePayload is whether a deletion's data carries the resource.
	deletePayload bool
	// list reads a project's resources, by name, and whether each has
	// settled.
	list func(ctx context.Context, project string) (map[string]eaResource, error)
}

type eaResource struct {
	msg     proto.Message
	settled bool
}

const cloudEventsProto = "https://googleapis.github.io/google-cloudevents/proto/google/events/cloud/"

// RedisInstanceLister reads a project's Memorystore for Redis instances in
// every location; the CLI sets it when it serves memorystore.
var redisLister struct {
	mu   sync.Mutex
	list func(ctx context.Context, project string) ([]*redispb.Instance, error)
}

// SetRedisInstanceLister gives Eventarc the Memorystore for Redis instances
// (#1997); without it their events are refused as not served.
func SetRedisInstanceLister(list func(ctx context.Context, project string) ([]*redispb.Instance, error)) {
	redisLister.mu.Lock()
	redisLister.list = list
	redisLister.mu.Unlock()
}

func redisServed() bool {
	redisLister.mu.Lock()
	defer redisLister.mu.Unlock()
	return redisLister.list != nil
}

var eaResourceSources = []*eaResourceSource{
	{service: "redis.googleapis.com", prefix: "google.cloud.redis.instance.v1.", attr: "instance", collection: "instances",
		noun: "A Memorystore for Redis instance", schema: cloudEventsProto + "redis/v1/data.proto",
		kinds: []string{"created", "updated", "deleted"},
		list: func(ctx context.Context, project string) (map[string]eaResource, error) {
			redisLister.mu.Lock()
			list := redisLister.list
			redisLister.mu.Unlock()
			if list == nil {
				return nil, fmt.Errorf("Memorystore for Redis is not served")
			}
			insts, err := list(ctx, project)
			if err != nil {
				return nil, err
			}
			out := map[string]eaResource{}
			for _, in := range insts {
				out[in.GetName()] = eaResource{msg: in, settled: in.GetState() == redispb.Instance_READY}
			}
			return out, nil
		}},
	{service: "cloudfunctions.googleapis.com", prefix: "google.cloud.functions.function.v2.", attr: "function", collection: "functions",
		noun: "A Cloud Run function", schema: cloudEventsProto + "functions/v2/data.proto",
		kinds: []string{"created", "updated", "deleted"},
		list: func(_ context.Context, project string) (map[string]eaResource, error) {
			functions.mu.Lock()
			defer functions.mu.Unlock()
			out := map[string]eaResource{}
			for n, sf := range functions.fns {
				if !strings.HasPrefix(n, "projects/"+project+"/") || sf.v1 != nil {
					continue
				}
				st := sf.fn.GetState()
				out[n] = eaResource{msg: cloneFunction(sf.fn), settled: st == functionspb.Function_ACTIVE || st == functionspb.Function_FAILED}
			}
			return out, nil
		}},
	{service: "eventarc.googleapis.com", prefix: "google.cloud.eventarc.trigger.v1.", attr: "trigger", collection: "triggers",
		noun: "An Eventarc trigger", schema: cloudEventsProto + "eventarc/v1/data.proto", deletePayload: true,
		kinds: []string{"created", "updated", "deleted"},
		list: func(_ context.Context, project string) (map[string]eaResource, error) {
			out := map[string]eaResource{}
			for _, t := range EventarcTriggers(project) {
				out[t.GetName()] = eaResource{msg: t, settled: true}
			}
			return out, nil
		}},
	{service: "eventarc.googleapis.com", prefix: "google.cloud.eventarc.channel.v1.", attr: "channel", collection: "channels",
		noun: "An Eventarc channel", schema: cloudEventsProto + "eventarc/v1/data.proto", deletePayload: true,
		kinds: []string{"created", "updated", "deleted"},
		list: func(_ context.Context, project string) (map[string]eaResource, error) {
			eaAdv.mu.Lock()
			defer eaAdv.mu.Unlock()
			out := map[string]eaResource{}
			for n, c := range eaAdv.channels.m {
				if strings.HasPrefix(n, "projects/"+project+"/") {
					out[n] = eaResource{msg: proto.Clone(c), settled: true}
				}
			}
			return out, nil
		}},
	{service: "eventarc.googleapis.com", prefix: "google.cloud.eventarc.channelConnection.v1.", attr: "channelconnection",
		collection: "channelConnections", noun: "An Eventarc channel connection", schema: cloudEventsProto + "eventarc/v1/data.proto",
		deletePayload: true, kinds: []string{"created", "deleted"},
		list: func(_ context.Context, project string) (map[string]eaResource, error) {
			eaAdv.mu.Lock()
			defer eaAdv.mu.Unlock()
			out := map[string]eaResource{}
			for n, c := range eaAdv.conns.m {
				if strings.HasPrefix(n, "projects/"+project+"/") {
					out[n] = eaResource{msg: proto.Clone(c), settled: true}
				}
			}
			return out, nil
		}},
}

func init() {
	for _, src := range eaResourceSources {
		for _, k := range src.kinds {
			typ := src.prefix + k
			eaTypeAttrs[typ] = []eaAttr{{name: src.attr, description: "The resource ID of the " +
				strings.TrimPrefix(strings.TrimPrefix(src.noun, "A "), "An ") + " triggering the event"}}
			eaTypeDescriptions[typ] = src.noun + " is " + k
			eaSchemaURIs[typ] = src.schema
		}
	}
}

// eaResourceSourceOf is the source of a direct resource event type.
func eaResourceSourceOf(typ string) (*eaResourceSource, bool) {
	for _, src := range eaResourceSources {
		if strings.HasPrefix(typ, src.prefix) {
			if _, ok := eaTypeAttrs[typ]; ok {
				return src, true
			}
		}
	}
	return nil, false
}

// ResourceEventAttribute is the extension a direct resource event type
// filters on (instance, function, trigger, ...), for the console.
func ResourceEventAttribute(typ string) (string, bool) {
	src, ok := eaResourceSourceOf(typ)
	if !ok {
		return "", false
	}
	return src.attr, true
}

// eaResourceServed refuses a resource event type whose service this
// instance does not serve.
func eaResourceServed(typ string) error {
	if src, ok := eaResourceSourceOf(typ); ok && src.service == "redis.googleapis.com" && !redisServed() {
		return fmt.Errorf("event type %q needs Memorystore for Redis, which this instance does not serve: start it with --services memorystore", typ)
	}
	return nil
}

// eaDirectTypes are the direct resource event types of a provider.
func eaDirectTypes(service string) []string {
	var out []string
	for _, src := range eaResourceSources {
		if src.service != service || (service == "redis.googleapis.com" && !redisServed()) {
			continue
		}
		for _, k := range src.kinds {
			out = append(out, src.prefix+k)
		}
	}
	return out
}

// eaResWatch holds the last settled read of each source in each project a
// trigger names.
var eaResWatch = struct {
	mu   sync.Mutex
	prev map[string]map[string]proto.Message // "prefix|project" -> name -> resource
}{prev: map[string]map[string]proto.Message{}}

// eaResourceWatches are the source prefixes and projects that triggers of
// resource event types name, as "prefix|project".
func eaResourceWatches() map[string]*eaResourceSource {
	want := map[string]*eaResourceSource{}
	add := func(name string, live *liveTrigger) {
		if live == nil {
			return
		}
		if src, ok := eaResourceSourceOf(live.spec.GetEventType()); ok {
			want[src.prefix+"|"+strings.Split(name, "/")[1]] = src
		}
	}
	eaState.mu.Lock()
	for n, x := range eaState.triggers {
		add(n, x.live)
	}
	eaState.mu.Unlock()
	for _, live := range functionTriggers() {
		add(live.spec.GetTrigger(), live)
	}
	return want
}

// baselineResources takes the first read of a source in a project, when a
// trigger of its types is made, so that a change right after is reported.
func (e *EventarcServer) baselineResources(ctx context.Context, project, typ string) {
	src, ok := eaResourceSourceOf(typ)
	if !ok {
		return
	}
	key := src.prefix + "|" + project
	eaResWatch.mu.Lock()
	_, have := eaResWatch.prev[key]
	eaResWatch.mu.Unlock()
	if have {
		return
	}
	cur, err := src.list(ctx, project)
	if err != nil {
		return
	}
	snap := map[string]proto.Message{}
	for n, r := range cur {
		if r.settled {
			snap[n] = r.msg
		}
	}
	eaResWatch.mu.Lock()
	if _, have := eaResWatch.prev[key]; !have {
		eaResWatch.prev[key] = snap
	}
	eaResWatch.mu.Unlock()
}

// WatchResources reads, each second until ctx ends, every source a
// trigger names and emits each change as its direct event.
func (e *EventarcServer) WatchResources(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		e.pollResources(ctx)
	}
}

func (e *EventarcServer) pollResources(ctx context.Context) {
	want := eaResourceWatches()
	eaResWatch.mu.Lock()
	// Read again under the lock, so that the baseline of a trigger made
	// since is not dropped.
	still := eaResourceWatches()
	for k := range eaResWatch.prev {
		if still[k] == nil {
			delete(eaResWatch.prev, k)
		}
	}
	eaResWatch.mu.Unlock()
	for key, src := range want {
		project := strings.SplitN(key, "|", 2)[1]
		rctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		cur, err := src.list(rctx, project)
		cancel()
		if err != nil {
			continue
		}
		eaResWatch.mu.Lock()
		prev, have := eaResWatch.prev[key]
		eaResWatch.mu.Unlock()
		next, changes := diffResources(prev, cur)
		eaResWatch.mu.Lock()
		eaResWatch.prev[key] = next
		eaResWatch.mu.Unlock()
		if !have {
			continue // the baseline
		}
		for _, c := range changes {
			if !containsKind(src.kinds, c.kind) {
				continue
			}
			ectx, done := context.WithTimeout(ctx, 30*time.Second)
			e.emitResource(ectx, src, project, c.kind, c.name, c.msg)
			done()
		}
	}
}

// eaChange is one resource's change between two reads.
type eaChange struct {
	kind, name string
	msg        proto.Message
}

// diffResources compares a read with the last settled one: a resource not
// yet settled keeps its last settled value (and is not reported until it
// settles), a new settled one is created, a changed one updated, and a
// gone one deleted, with its last settled value.
func diffResources(prev map[string]proto.Message, cur map[string]eaResource) (map[string]proto.Message, []eaChange) {
	next := map[string]proto.Message{}
	var changes []eaChange
	for _, n := range sortedKeys(cur) {
		r := cur[n]
		old, was := prev[n]
		switch {
		case !r.settled:
			if was {
				next[n] = old
			}
		case !was:
			next[n] = r.msg
			changes = append(changes, eaChange{"created", n, r.msg})
		default:
			next[n] = r.msg
			if !proto.Equal(old, r.msg) {
				changes = append(changes, eaChange{"updated", n, r.msg})
			}
		}
	}
	for _, n := range sortedKeys(prev) {
		if _, ok := cur[n]; !ok {
			changes = append(changes, eaChange{"deleted", n, prev[n]})
		}
	}
	return next, changes
}

func containsKind(kinds []string, k string) bool {
	for _, x := range kinds {
		if x == k {
			return true
		}
	}
	return false
}

// resourceEventData is the catalogue's XxxEventData in JSON: the resource
// as payload, left out of a deletion where the catalogue says so.
func resourceEventData(src *eaResourceSource, kind string, msg proto.Message) ([]byte, error) {
	if kind == "deleted" && !src.deletePayload {
		return []byte("{}"), nil
	}
	payload, err := protojson.Marshal(msg)
	if err != nil {
		return nil, err
	}
	return []byte(`{"payload":` + string(payload) + `}`), nil
}

// emitResource publishes one resource change to every trigger it matches.
func (e *EventarcServer) emitResource(ctx context.Context, src *eaResourceSource, project, kind, name string, msg proto.Message) int {
	typ := src.prefix + kind
	parts := strings.Split(name, "/")
	if len(parts) < 6 {
		return 0
	}
	location, id := parts[3], parts[len(parts)-1]
	topics := eaMatching(project, []string{typ}, func(attr string) (string, bool) {
		if attr == src.attr {
			return id, true
		}
		return "", false
	})
	if len(topics) == 0 {
		return 0
	}
	data, err := resourceEventData(src, kind, msg)
	if err != nil {
		slog.Warn("eventarc: encode a resource event", "type", typ, "err", err)
		return 0
	}
	attrs := map[string]string{
		"ce-id":          uuid.NewString(),
		"ce-source":      "//" + src.service + "/projects/" + project + "/locations/" + location,
		"ce-subject":     src.collection + "/" + id,
		"ce-time":        time.Now().UTC().Format(time.RFC3339Nano),
		"ce-" + src.attr: id,
		"content-type":   "application/json",
	}
	return e.f.publishSourced(ctx, topics, data, attrs)
}
