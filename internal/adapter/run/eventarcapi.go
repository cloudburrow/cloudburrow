package run

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	eventarcpb "cloud.google.com/go/eventarc/apiv1/eventarcpb"
	functionspb "cloud.google.com/go/functions/apiv2/functionspb"
	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	runpb "cloud.google.com/go/run/apiv2/runpb"
	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/resource"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// Eventarc (#1877): google.cloud.eventarc.v1's triggers, for Google
// sources delivered to Cloud Run, as Google's "Eventarc triggers" and
// "Route events to Cloud Run" pages document them, on the transport the
// 2nd-gen functions' triggers use (eventarc.go):
//
//   - google.cloud.pubsub.topic.v1.messagePublished: the trigger's
//     transport.pubsub.topic (made by Eventarc, eventarc-REGION-TRIGGER-NNN,
//     when none is given) and a push subscription on it Eventarc makes.
//   - google.cloud.storage.object.v1.finalized, deleted, archived and
//     metadataUpdated, filtered on the bucket: a topic Eventarc makes, the
//     bucket's notification on it and a push subscription.
//
// Each event reaches the destination's Cloud Run service (service, region,
// path) as a CloudEvent in binary content mode, with ce-id, ce-source,
// ce-specversion, ce-type, ce-time (and ce-subject for Cloud Storage) and
// the documented data (MessagePublishedData; the object), with an ID token
// for the trigger's service account. Triggers are kept in memory, as the
// functions' are. Channels, other providers, other destinations and
// operators other than an exact match are UNIMPLEMENTED, and
// ListProviders names the two providers served.

// EventarcServer serves Eventarc's triggers and providers.
type EventarcServer struct {
	eventarcpb.UnimplementedEventarcServer
	f *FunctionsServer
}

// Eventarc returns the Eventarc service, on the functions' event transport.
func (f *FunctionsServer) Eventarc() *EventarcServer { return &EventarcServer{f: f} }

// Register adds the service to a gRPC server or a JSON transcoder.
func (e *EventarcServer) Register(g grpc.ServiceRegistrar) { eventarcpb.RegisterEventarcServer(g, e) }

type eaTrigger struct {
	t    *eventarcpb.Trigger
	live *liveTrigger
}

var eaState = struct {
	mu       sync.Mutex
	triggers map[string]*eaTrigger
}{triggers: map[string]*eaTrigger{}}

var eaTriggerIDRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

// eaProviders are the providers served, with their event types.
func eaProviders(location string) []*eventarcpb.Provider {
	bucket := []*eventarcpb.FilteringAttribute{{Attribute: "type", Required: true}, {Attribute: "bucket", Required: true,
		Description: "The bucket's name"}}
	storage := &eventarcpb.Provider{Name: "storage.googleapis.com", DisplayName: "Cloud Storage"}
	for _, t := range []string{"finalized", "deleted", "archived", "metadataUpdated"} {
		storage.EventTypes = append(storage.EventTypes, &eventarcpb.EventType{Type: storageEventPrefix + t,
			Description: "An object's " + t + " event", FilteringAttributes: bucket,
			EventSchemaUri: "https://googleapis.github.io/google-cloudevents/jsonschema/google/events/cloud/storage/v1/StorageObjectData.json"})
	}
	pubsub := &eventarcpb.Provider{Name: "pubsub.googleapis.com", DisplayName: "Cloud Pub/Sub", EventTypes: []*eventarcpb.EventType{{
		Type: pubsubPublished, Description: "A message is published to the specified Pub/Sub topic",
		FilteringAttributes: []*eventarcpb.FilteringAttribute{{Attribute: "type", Required: true}},
		EventSchemaUri:      "https://googleapis.github.io/google-cloudevents/jsonschema/google/events/cloud/pubsub/v1/MessagePublishedData.json"}}}
	out := []*eventarcpb.Provider{pubsub, storage}
	for _, p := range out {
		p.Name = location + "/providers/" + p.Name
	}
	return out
}

func (e *EventarcServer) op(target, verb string, response proto.Message) (*longrunningpb.Operation, error) {
	s := e.f.s
	loc := strings.SplitN(target, "/triggers/", 2)[0]
	op := s.ops.CreateNamed(loc+"/operations/"+uuid.NewString(), target)
	now := timestamppb.Now()
	_ = s.ops.SetMetadata(op.Name, &eventarcpb.OperationMetadata{CreateTime: now, EndTime: now, Target: target, Verb: verb, ApiVersion: "v1"})
	_ = s.ops.Succeed(op.Name, response)
	return s.toProtoOperation(op.Name)
}

// eaSpec is a trigger as the functions' transport takes it, or why it
// cannot be served.
func eaSpec(t *eventarcpb.Trigger, region string) (*functionspb.EventTrigger, error) {
	if t.GetChannel() != "" {
		return nil, apierror.Unimplemented("trigger channel is not supported: only Google sources (direct events) are served")
	}
	spec := &functionspb.EventTrigger{TriggerRegion: region, ServiceAccountEmail: t.GetServiceAccount(),
		RetryPolicy: functionspb.EventTrigger_RETRY_POLICY_RETRY}
	for _, f := range t.GetEventFilters() {
		if f.GetOperator() != "" {
			return nil, apierror.Unimplemented("event filter operator %q is not supported: only an exact match is", f.GetOperator())
		}
		switch f.GetAttribute() {
		case "type":
			spec.EventType = f.GetValue()
		case "bucket":
			spec.EventFilters = append(spec.EventFilters, &functionspb.EventFilter{Attribute: "bucket", Value: f.GetValue()})
		default:
			return nil, apierror.InvalidArgument("event filter attribute %q is not one of the event type's filtering attributes", f.GetAttribute())
		}
	}
	switch typ := spec.GetEventType(); {
	case typ == "":
		return nil, apierror.InvalidArgument("the trigger's event filters must name its event type: attribute \"type\"")
	case typ == pubsubPublished:
		if len(spec.EventFilters) > 0 {
			return nil, apierror.InvalidArgument("google.cloud.pubsub.topic.v1.messagePublished takes no bucket filter")
		}
		spec.PubsubTopic = t.GetTransport().GetPubsub().GetTopic()
		if spec.PubsubTopic != "" && !strings.Contains(spec.PubsubTopic, "/topics/") {
			return nil, apierror.InvalidArgument("transport.pubsub.topic %q must be projects/*/topics/*", spec.PubsubTopic)
		}
	case storageNotificationTypes[typ] != "":
		if storageBucket(spec) == "" {
			return nil, apierror.InvalidArgument("a Cloud Storage trigger's event filters must name the bucket: attribute \"bucket\"")
		}
		if t.GetTransport().GetPubsub().GetTopic() != "" {
			return nil, apierror.InvalidArgument("transport.pubsub.topic is for Pub/Sub triggers; Eventarc makes a Cloud Storage trigger's")
		}
	default:
		return nil, apierror.Unimplemented("event type %q is not supported: Pub/Sub's messagePublished and Cloud Storage's "+
			"finalized, deleted, archived and metadataUpdated are; %s", typ, eventReason(typ))
	}
	if t.GetRetryPolicy() != nil && t.GetRetryPolicy().GetMaxAttempts() == 1 {
		spec.RetryPolicy = functionspb.EventTrigger_RETRY_POLICY_DO_NOT_RETRY
	}
	switch d := t.GetDestination(); {
	case d == nil:
		return nil, apierror.InvalidArgument("the trigger's destination is required")
	case d.GetCloudRun() != nil:
		if d.GetCloudRun().GetService() == "" {
			return nil, apierror.InvalidArgument("destination.cloudRun.service is required")
		}
		if p := d.GetCloudRun().GetPath(); p != "" && !strings.HasPrefix(p, "/") {
			return nil, apierror.InvalidArgument("destination.cloudRun.path %q must begin with /", p)
		}
	default:
		return nil, apierror.Unimplemented("only a Cloud Run destination (destination.cloudRun) is supported")
	}
	return spec, nil
}

// CreateTrigger makes a trigger and its transport.
func (e *EventarcServer) CreateTrigger(ctx context.Context, req *eventarcpb.CreateTriggerRequest) (*longrunningpb.Operation, error) {
	if e.f.events == nil {
		return nil, apierror.FailedPrecondition("Eventarc needs Pub/Sub, its transport, which this instance does not serve: start it with --services run,pubsub")
	}
	project, region, err := resource.ParseLocation(req.GetParent())
	if err != nil {
		return nil, apierror.InvalidArgument("%v", err)
	}
	if !eaTriggerIDRE.MatchString(req.GetTriggerId()) {
		return nil, apierror.InvalidArgument("trigger_id %q must be 1-63 lowercase letters, digits and hyphens, starting with a letter", req.GetTriggerId())
	}
	in := req.GetTrigger()
	if in == nil {
		return nil, apierror.InvalidArgument("trigger is required")
	}
	t := proto.Clone(in).(*eventarcpb.Trigger)
	if run := t.GetDestination().GetCloudRun(); run != nil && run.GetRegion() == "" {
		run.Region = region
	}
	spec, err := eaSpec(t, region)
	if err != nil {
		return nil, err
	}
	name := req.GetParent() + "/triggers/" + req.GetTriggerId()
	if run := t.GetDestination().GetCloudRun(); run != nil {
		if _, err := e.f.s.GetService(ctx, &runpb.GetServiceRequest{Name: "projects/" + project + "/locations/" +
			run.GetRegion() + "/services/" + run.GetService()}); err != nil {
			return nil, apierror.InvalidArgument("destination.cloudRun.service %q not found in %s", run.GetService(), run.GetRegion())
		}
	}
	eaState.mu.Lock()
	_, exists := eaState.triggers[name]
	eaState.mu.Unlock()
	if exists {
		return nil, apierror.AlreadyExists("trigger %s already exists", name)
	}
	if req.GetValidateOnly() {
		return e.op(name, "create", t)
	}
	spec.Trigger = name
	if spec.GetEventType() == pubsubPublished && spec.GetPubsubTopic() == "" {
		spec.PubsubTopic = fmt.Sprintf("projects/%s/topics/eventarc-%s-%s-%03d", project, region, req.GetTriggerId(), rand.IntN(1000))
		if err := e.f.events.Transport.CreateTopic(ctx, spec.PubsubTopic, map[string]string{"goog-managed-by": "eventarc"}); err != nil {
			return nil, fmt.Errorf("create the trigger's topic %s: %w", spec.PubsubTopic, err)
		}
	}
	live, err := e.f.makeTransport(ctx, project, region, req.GetTriggerId(), spec)
	if err != nil {
		return nil, err
	}
	if in.GetTransport().GetPubsub().GetTopic() == "" && spec.GetEventType() == pubsubPublished {
		live.topic = spec.GetPubsubTopic() // Eventarc's own, removed with the trigger
	}
	now := timestamppb.Now()
	t.Name, t.Uid, t.CreateTime, t.UpdateTime = name, uuid.NewString(), now, now
	topic := spec.GetPubsubTopic()
	if live.topic != "" {
		topic = live.topic
	}
	t.Transport = &eventarcpb.Transport{Intermediary: &eventarcpb.Transport_Pubsub{Pubsub: &eventarcpb.Pubsub{
		Topic: topic, Subscription: live.subscription}}}
	t.EventDataContentType = "application/json"
	t.Etag = eaEtag(t)
	eaState.mu.Lock()
	eaState.triggers[name] = &eaTrigger{t: t, live: live}
	eaState.mu.Unlock()
	return e.op(name, "create", proto.Clone(t))
}

func eaEtag(t *eventarcpb.Trigger) string {
	return fmt.Sprintf("%x", resource.ProjectNumber(t.GetName()+t.GetUpdateTime().AsTime().String()))
}

// GetTrigger reads a trigger.
func (e *EventarcServer) GetTrigger(_ context.Context, req *eventarcpb.GetTriggerRequest) (*eventarcpb.Trigger, error) {
	eaState.mu.Lock()
	defer eaState.mu.Unlock()
	x, ok := eaState.triggers[req.GetName()]
	if !ok {
		return nil, apierror.NotFound("trigger %s not found", req.GetName())
	}
	return proto.Clone(x.t).(*eventarcpb.Trigger), nil
}

// EventarcTriggers are a project's triggers in every location, for the
// console.
func EventarcTriggers(project string) []*eventarcpb.Trigger {
	eaState.mu.Lock()
	defer eaState.mu.Unlock()
	var out []*eventarcpb.Trigger
	for _, n := range sortedKeys(eaState.triggers) {
		if strings.HasPrefix(n, "projects/"+project+"/") {
			out = append(out, proto.Clone(eaState.triggers[n].t).(*eventarcpb.Trigger))
		}
	}
	return out
}

// ListTriggers lists a location's triggers, by name.
func (e *EventarcServer) ListTriggers(_ context.Context, req *eventarcpb.ListTriggersRequest) (*eventarcpb.ListTriggersResponse, error) {
	if req.GetFilter() != "" {
		return nil, apierror.Unimplemented("ListTriggers filter is not supported")
	}
	if ob := req.GetOrderBy(); ob != "" && ob != "name" {
		return nil, apierror.Unimplemented("ListTriggers order_by %q is not supported: triggers are listed by name", ob)
	}
	eaState.mu.Lock()
	var all []*eventarcpb.Trigger
	for _, n := range sortedKeys(eaState.triggers) {
		if strings.HasPrefix(n, req.GetParent()+"/triggers/") || (strings.HasSuffix(req.GetParent(), "/locations/-") &&
			strings.HasPrefix(n, strings.TrimSuffix(req.GetParent(), "-"))) {
			all = append(all, proto.Clone(eaState.triggers[n].t).(*eventarcpb.Trigger))
		}
	}
	eaState.mu.Unlock()
	items, next, err := page(all, req.GetPageSize(), req.GetPageToken(), req.GetParent())
	if err != nil {
		return nil, err
	}
	return &eventarcpb.ListTriggersResponse{Triggers: items, NextPageToken: next}, nil
}

// UpdateTrigger changes a trigger's destination, service account, labels
// and retry policy; its event filters and transport are fixed, as
// Google's are for a direct event's trigger.
func (e *EventarcServer) UpdateTrigger(ctx context.Context, req *eventarcpb.UpdateTriggerRequest) (*longrunningpb.Operation, error) {
	in := req.GetTrigger()
	eaState.mu.Lock()
	x, ok := eaState.triggers[in.GetName()]
	var cur *eventarcpb.Trigger
	if ok {
		cur = proto.Clone(x.t).(*eventarcpb.Trigger)
	}
	eaState.mu.Unlock()
	if !ok {
		if req.GetAllowMissing() {
			return nil, apierror.Unimplemented("UpdateTrigger allow_missing is not supported: create the trigger")
		}
		return nil, apierror.NotFound("trigger %s not found", in.GetName())
	}
	if in.GetEtag() != "" && in.GetEtag() != cur.GetEtag() {
		return nil, apierror.Aborted("the trigger's etag %q is not its current %q", in.GetEtag(), cur.GetEtag())
	}
	paths := req.GetUpdateMask().GetPaths()
	if len(paths) == 0 {
		paths = []string{"destination", "service_account", "labels", "retry_policy"}
	}
	next := proto.Clone(cur).(*eventarcpb.Trigger)
	for _, p := range paths {
		switch p {
		case "destination":
			next.Destination = in.GetDestination()
		case "destination.cloud_run", "destination.cloud_run.service", "destination.cloud_run.path", "destination.cloud_run.region":
			// gcloud eventarc triggers update names the field it changes.
			run := next.GetDestination().GetCloudRun()
			if run == nil {
				return nil, apierror.InvalidArgument("update_mask path %q: the trigger's destination is not Cloud Run", p)
			}
			from := in.GetDestination().GetCloudRun()
			switch p {
			case "destination.cloud_run":
				next.Destination = in.GetDestination()
			case "destination.cloud_run.service":
				run.Service = from.GetService()
			case "destination.cloud_run.path":
				run.Path = from.GetPath()
			case "destination.cloud_run.region":
				run.Region = from.GetRegion()
			}
		case "service_account":
			next.ServiceAccount = in.GetServiceAccount()
		case "labels":
			next.Labels = in.GetLabels()
		case "retry_policy":
			next.RetryPolicy = in.GetRetryPolicy()
		case "event_filters", "transport", "channel":
			return nil, apierror.InvalidArgument("the trigger's %s cannot be changed: delete and create the trigger", p)
		default:
			return nil, apierror.InvalidArgument("update_mask path %q is not a field of a trigger that can be changed", p)
		}
	}
	region := strings.Split(next.GetName(), "/")[3]
	if run := next.GetDestination().GetCloudRun(); run != nil && run.GetRegion() == "" {
		run.Region = region
	}
	if _, err := eaSpec(next, region); err != nil {
		return nil, err
	}
	if req.GetValidateOnly() {
		return e.op(next.GetName(), "update", next)
	}
	next.UpdateTime = timestamppb.Now()
	next.Etag = eaEtag(next)
	eaState.mu.Lock()
	if x, ok := eaState.triggers[next.GetName()]; ok {
		x.t = next
		x.live.spec.ServiceAccountEmail = next.GetServiceAccount()
	}
	eaState.mu.Unlock()
	return e.op(next.GetName(), "update", proto.Clone(next))
}

// DeleteTrigger removes a trigger and what it made; the user's topic is
// left.
func (e *EventarcServer) DeleteTrigger(ctx context.Context, req *eventarcpb.DeleteTriggerRequest) (*longrunningpb.Operation, error) {
	eaState.mu.Lock()
	x, ok := eaState.triggers[req.GetName()]
	if ok && req.GetEtag() != "" && req.GetEtag() != x.t.GetEtag() {
		eaState.mu.Unlock()
		return nil, apierror.Aborted("the trigger's etag %q is not its current %q", req.GetEtag(), x.t.GetEtag())
	}
	if ok && !req.GetValidateOnly() {
		delete(eaState.triggers, req.GetName())
	}
	eaState.mu.Unlock()
	if !ok {
		if req.GetAllowMissing() {
			return e.op(req.GetName(), "delete", &eventarcpb.Trigger{Name: req.GetName()})
		}
		return nil, apierror.NotFound("trigger %s not found", req.GetName())
	}
	if !req.GetValidateOnly() {
		e.f.teardown(ctx, x.live)
	}
	return e.op(req.GetName(), "delete", proto.Clone(x.t))
}

// GetProvider reads one of the providers served.
func (e *EventarcServer) GetProvider(_ context.Context, req *eventarcpb.GetProviderRequest) (*eventarcpb.Provider, error) {
	loc, _, _ := strings.Cut(req.GetName(), "/providers/")
	for _, p := range eaProviders(loc) {
		if p.GetName() == req.GetName() {
			return p, nil
		}
	}
	return nil, apierror.NotFound("provider %s not found: this instance serves pubsub.googleapis.com and storage.googleapis.com", req.GetName())
}

// ListProviders lists the providers served, by name.
func (e *EventarcServer) ListProviders(_ context.Context, req *eventarcpb.ListProvidersRequest) (*eventarcpb.ListProvidersResponse, error) {
	if req.GetFilter() != "" {
		return nil, apierror.Unimplemented("ListProviders filter is not supported")
	}
	ps := eaProviders(req.GetParent())
	sort.Slice(ps, func(i, j int) bool { return ps[i].GetName() < ps[j].GetName() })
	items, next, err := page(ps, req.GetPageSize(), req.GetPageToken(), req.GetParent())
	if err != nil {
		return nil, err
	}
	return &eventarcpb.ListProvidersResponse{Providers: items, NextPageToken: next}, nil
}

// eaTriggerOf finds the Eventarc trigger a push is for.
func eaTriggerOf(name string) (*eventarcpb.Trigger, *functionspb.EventTrigger, bool) {
	eaState.mu.Lock()
	defer eaState.mu.Unlock()
	x, ok := eaState.triggers[name]
	if !ok {
		return nil, nil, false
	}
	return proto.Clone(x.t).(*eventarcpb.Trigger), proto.Clone(x.live.spec).(*functionspb.EventTrigger), true
}

// pushEventarc delivers one push of an Eventarc trigger's subscription to
// its Cloud Run service, as push does a function's.
func (f *FunctionsServer) pushEventarc(w http.ResponseWriter, r *http.Request, t *eventarcpb.Trigger, spec *functionspb.EventTrigger) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	code, err := f.deliverEventarc(r.Context(), t, spec, body)
	if err == nil && pushSucceeded(code) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	retry := spec.GetRetryPolicy() == functionspb.EventTrigger_RETRY_POLICY_RETRY
	slog.Warn("eventarc: event delivery failed", "trigger", t.GetName(), "status", code, "err", err, "retry", retry)
	if !retry {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if code == 0 || pushSucceeded(code) {
		code = http.StatusBadGateway
	}
	w.WriteHeader(code)
}

func (f *FunctionsServer) deliverEventarc(ctx context.Context, t *eventarcpb.Trigger, spec *functionspb.EventTrigger, body []byte) (int, error) {
	ev, err := ToCloudEvent(spec, body)
	if err != nil {
		return http.StatusBadRequest, err
	}
	run := t.GetDestination().GetCloudRun()
	project := strings.Split(t.GetName(), "/")[1]
	svc, err := f.s.GetService(ctx, &runpb.GetServiceRequest{Name: "projects/" + project + "/locations/" + run.GetRegion() + "/services/" + run.GetService()})
	if err != nil {
		return 0, fmt.Errorf("the destination service: %w", err)
	}
	target := svc.GetUri()
	u, err := url.Parse(target)
	if err != nil || u.Host == "" {
		return 0, fmt.Errorf("the destination service has no URL")
	}
	if f.events.Ingress == "" {
		return 0, fmt.Errorf("no ingress port is published on this machine, so the service cannot be reached")
	}
	timeout := time.Minute
	if d := svc.GetTemplate().GetTimeout(); d != nil && d.AsDuration() > 0 {
		timeout = d.AsDuration()
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+f.events.Ingress+run.GetPath(), strings.NewReader(string(ev.Data)))
	if err != nil {
		return 0, err
	}
	if run.GetPath() == "" {
		req.URL.Path = "/"
	}
	req.Host = u.Host
	for k, v := range ev.Headers {
		req.Header[k] = v
	}
	if f.events.Tokens != nil {
		account := t.GetServiceAccount()
		if account == "" {
			account = projectNumberOf(project) + "-compute@developer.gserviceaccount.com"
		}
		tok, err := f.events.Tokens.ServiceAccountIDToken(account, strings.TrimSuffix(target, "/"))
		if err != nil {
			return 0, fmt.Errorf("mint the trigger's token: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := f.events.Client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

// projectNumberOf is a project's number, as the default compute account
// names it.
func projectNumberOf(project string) string { return fmt.Sprint(resource.ProjectNumber(project)) }

// eaJSONPrefix is where Eventarc's JSON API is served on this port: its
// v1 trigger paths (/v1/projects/*/locations/*/triggers) are Cloud Build's
// regional triggers', so gcloud's eventarc override and Terraform's
// eventarc_custom_endpoint carry it.
const eaJSONPrefix = "/eventarc"

// EventarcHandlerOr serves Eventarc's JSON API (register adds it and the
// Operations service it polls) and passes every other request to next.
func EventarcHandlerOr(register func(grpc.ServiceRegistrar), next http.Handler) http.Handler {
	t := &rest.Transcoder{Packages: []string{"google.cloud.eventarc.v1"}, Mixins: sourceDeployMixins}
	register(t)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, eaJSONPrefix+"/v1/") {
			next.ServeHTTP(w, r)
			return
		}
		r2 := r.Clone(r.Context())
		r2.URL.Path = strings.TrimPrefix(r.URL.Path, eaJSONPrefix)
		r2.URL.RawPath = ""
		t.ServeHTTP(w, r2)
	})
}
