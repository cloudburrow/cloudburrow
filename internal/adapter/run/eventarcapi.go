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
	"slices"
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

	"github.com/cloudburrow/cloudburrow/internal/aipfilter"
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
// functions' are. Cloud Audit Logs (eventarcaudit.go) and Firestore
// (eventarcfirestore.go) events are published by this process to a topic
// Eventarc makes; a destination can also be a Cloud Run function or an
// HTTP endpoint; match-path-pattern (eventarcpath.go) is the one operator
// beyond an exact match. Channels (eventarcchannels.go), GKE and Workflows
// destinations are UNIMPLEMENTED with the reason.

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

// eaProviderNames are the display names of the providers served.
var eaProviderNames = map[string]string{
	"pubsub.googleapis.com": "Cloud Pub/Sub", "storage.googleapis.com": "Cloud Storage",
	"secretmanager.googleapis.com": "Secret Manager", "cloudkms.googleapis.com": "Cloud Key Management Service",
	"cloudtasks.googleapis.com": "Cloud Tasks", "cloudscheduler.googleapis.com": "Cloud Scheduler",
	"run.googleapis.com": "Cloud Run Admin API", "cloudfunctions.googleapis.com": "Cloud Functions",
	"eventarc.googleapis.com": "Eventarc", "cloudbuild.googleapis.com": "Cloud Build",
	"artifactregistry.googleapis.com": "Artifact Registry", "securesourcemanager.googleapis.com": "Secure Source Manager",
	"developerconnect.googleapis.com": "Developer Connect", "logging.googleapis.com": "Cloud Logging",
	"firestore.googleapis.com": "Cloud Firestore", "datastore.googleapis.com": "Cloud Datastore",
	"bigtableadmin.googleapis.com": "Cloud Bigtable Admin", "bigtable.googleapis.com": "Cloud Bigtable",
	"spanner.googleapis.com": "Cloud Spanner", "redis.googleapis.com": "Memorystore for Redis",
	"memorystore.googleapis.com": "Memorystore", "cloudresourcemanager.googleapis.com": "Cloud Resource Manager",
	"iam.googleapis.com": "Identity and Access Management", "backupdr.googleapis.com": "Backup and DR",
}

var eaTypeDescriptions = map[string]string{
	pubsubPublished: "A message is published to the specified Pub/Sub topic",
	auditLogWritten: "An audit log is created that matches the trigger's filter criteria",
}

var eaSchemaURIs = map[string]string{
	pubsubPublished: "https://googleapis.github.io/google-cloudevents/jsonschema/google/events/cloud/pubsub/v1/MessagePublishedData.json",
	auditLogWritten: "https://googleapis.github.io/google-cloudevents/jsonschema/google/events/cloud/audit/v1/LogEntryData.json",
}

// eaEventType describes one event type served, with its filtering
// attributes.
func eaEventType(typ string) *eventarcpb.EventType {
	et := &eventarcpb.EventType{Type: typ, Description: eaTypeDescriptions[typ], EventSchemaUri: eaSchemaURIs[typ],
		FilteringAttributes: []*eventarcpb.FilteringAttribute{{Attribute: "type", Required: true}}}
	if strings.HasPrefix(typ, storageEventPrefix) {
		et.Description = "An object's " + strings.TrimPrefix(typ, storageEventPrefix) + " event"
		et.EventSchemaUri = "https://googleapis.github.io/google-cloudevents/jsonschema/google/events/cloud/storage/v1/StorageObjectData.json"
	}
	for _, a := range eaTypeAttrs[typ] {
		et.FilteringAttributes = append(et.FilteringAttributes, &eventarcpb.FilteringAttribute{Attribute: a.name,
			Description: a.description, Required: a.required, PathPatternSupported: a.pattern})
	}
	return et
}

// eaProviders are the providers served, with their event types: Pub/Sub's
// and Cloud Storage's direct events, Firestore's, and each served
// service's audit-log event.
func eaProviders(location string) []*eventarcpb.Provider {
	direct := map[string][]string{
		"pubsub.googleapis.com":  {pubsubPublished},
		"storage.googleapis.com": {storageEventPrefix + "finalized", storageEventPrefix + "deleted", storageEventPrefix + "archived", storageEventPrefix + "metadataUpdated"},
	}
	for typ := range eaTypeAttrs {
		if isFirestoreType(typ) {
			direct["firestore.googleapis.com"] = append(direct["firestore.googleapis.com"], typ)
		}
	}
	names := append(AuditServiceNames(), "storage.googleapis.com")
	sort.Strings(names)
	var out []*eventarcpb.Provider
	for _, n := range slices.Compact(names) {
		p := &eventarcpb.Provider{Name: location + "/providers/" + n, DisplayName: eaProviderNames[n]}
		types := append([]string(nil), direct[n]...)
		for _, typ := range append(types, auditLogWritten) {
			p.EventTypes = append(p.EventTypes, eaEventType(typ))
		}
		out = append(out, p)
	}
	return out
}

func (e *EventarcServer) op(target, verb string, response proto.Message) (*longrunningpb.Operation, error) {
	s := e.f.s
	loc := eaLocOf(target)
	op := s.ops.CreateNamed(loc+"/operations/"+uuid.NewString(), target)
	now := timestamppb.Now()
	_ = s.ops.SetMetadata(op.Name, &eventarcpb.OperationMetadata{CreateTime: now, EndTime: now, Target: target, Verb: verb, ApiVersion: "v1"})
	_ = s.ops.Succeed(op.Name, response)
	return s.toProtoOperation(op.Name)
}

// eaSpec is a trigger as the functions' transport takes it, or why it
// cannot be served.
func eaSpec(t *eventarcpb.Trigger, region string) (*functionspb.EventTrigger, error) {
	spec := &functionspb.EventTrigger{TriggerRegion: region, ServiceAccountEmail: t.GetServiceAccount(),
		RetryPolicy: functionspb.EventTrigger_RETRY_POLICY_RETRY, Channel: t.GetChannel()}
	for _, f := range t.GetEventFilters() {
		if op := f.GetOperator(); op != "" && op != pathPatternOp {
			return nil, apierror.InvalidArgument("event filter operator %q is not supported: an exact match (no operator) and %q are", op, pathPatternOp)
		}
		if f.GetAttribute() == "type" {
			if f.GetOperator() != "" {
				return nil, apierror.InvalidArgument("the event filter on \"type\" takes no operator")
			}
			spec.EventType = f.GetValue()
			continue
		}
		spec.EventFilters = append(spec.EventFilters, &functionspb.EventFilter{Attribute: f.GetAttribute(), Value: f.GetValue(), Operator: f.GetOperator()})
	}
	if spec.GetChannel() != "" {
		// A channel's events are custom or a provider's: any type, and
		// filters on any of their attributes (#1947).
		if err := eaCheckChannelTrigger(t, spec); err != nil {
			return nil, err
		}
	} else if err := eaCheckFilters(spec); err != nil {
		return nil, err
	}
	switch typ := spec.GetEventType(); {
	case spec.GetChannel() != "":
		if t.GetTransport().GetPubsub().GetTopic() != "" {
			return nil, apierror.InvalidArgument("transport.pubsub.topic is not set on a channel's trigger: its events come from the channel")
		}
	case typ == pubsubPublished:
		spec.PubsubTopic = t.GetTransport().GetPubsub().GetTopic()
		if spec.PubsubTopic != "" && !strings.Contains(spec.PubsubTopic, "/topics/") {
			return nil, apierror.InvalidArgument("transport.pubsub.topic %q must be projects/*/topics/*", spec.PubsubTopic)
		}
	default:
		if t.GetTransport().GetPubsub().GetTopic() != "" {
			return nil, apierror.InvalidArgument("transport.pubsub.topic is for Pub/Sub triggers; Eventarc makes a %s trigger's", typ)
		}
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
	case d.GetCloudFunction() != "":
		if _, _, err := parseFunctionName(d.GetCloudFunction()); err != nil {
			return nil, apierror.InvalidArgument("destination.cloudFunction %q must be projects/*/locations/*/functions/*", d.GetCloudFunction())
		}
	case d.GetHttpEndpoint() != nil:
		u, err := url.Parse(d.GetHttpEndpoint().GetUri())
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, apierror.InvalidArgument("destination.httpEndpoint.uri %q must be an http or https URI", d.GetHttpEndpoint().GetUri())
		}
		if d.GetNetworkConfig().GetNetworkAttachment() == "" {
			return nil, apierror.InvalidArgument("destination.networkConfig.networkAttachment is required for an HTTP endpoint destination")
		}
	case d.GetGke() != nil:
		return nil, apierror.Unimplemented("destination.gke is not supported: this instance serves no Google Kubernetes Engine " +
			"(its cluster is a local kind cluster, not a GKE cluster the Kubernetes Engine API names); route to a Cloud Run " +
			"service, a function or an HTTP endpoint")
	case d.GetWorkflow() != "":
		return nil, apierror.Unimplemented("destination.workflow is not supported: this instance does not serve Workflows")
	default:
		return nil, apierror.InvalidArgument("the trigger's destination must name a cloudRun service, a cloudFunction, " +
			"a gke service, a workflow or an httpEndpoint")
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
	if fn := t.GetDestination().GetCloudFunction(); fn != "" {
		if _, ok := eaFunction(fn); !ok {
			return nil, apierror.InvalidArgument("destination.cloudFunction %q not found", fn)
		}
	}
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
	if spec.GetChannel() != "" {
		return e.storeChannelTrigger(name, t, spec)
	}
	if sourcedEventType(spec.GetEventType()) {
		if _, ok := e.f.publisher(); !ok {
			return nil, apierror.FailedPrecondition("this instance cannot publish %s events to a trigger's topic", spec.GetEventType())
		}
	}
	if (spec.GetEventType() == pubsubPublished || sourcedEventType(spec.GetEventType())) && spec.GetPubsubTopic() == "" {
		spec.PubsubTopic = fmt.Sprintf("projects/%s/topics/eventarc-%s-%s-%03d", project, region, req.GetTriggerId(), rand.IntN(1000))
		if err := e.f.events.Transport.CreateTopic(ctx, spec.PubsubTopic, map[string]string{"goog-managed-by": "eventarc"}); err != nil {
			return nil, fmt.Errorf("create the trigger's topic %s: %w", spec.PubsubTopic, err)
		}
	}
	live, err := e.f.makeTransport(ctx, project, region, req.GetTriggerId(), spec)
	if err != nil {
		return nil, err
	}
	if in.GetTransport().GetPubsub().GetTopic() == "" && (spec.GetEventType() == pubsubPublished || sourcedEventType(spec.GetEventType())) {
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
	t.EventDataContentType = eaContentType(spec.GetEventType())
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
	eaState.mu.Lock()
	var all []*eventarcpb.Trigger
	for _, n := range sortedKeys(eaState.triggers) {
		if strings.HasPrefix(n, req.GetParent()+"/triggers/") || (strings.HasSuffix(req.GetParent(), "/locations/-") &&
			strings.HasPrefix(n, strings.TrimSuffix(req.GetParent(), "-"))) {
			all = append(all, proto.Clone(eaState.triggers[n].t).(*eventarcpb.Trigger))
		}
	}
	eaState.mu.Unlock()
	// AIP-160 filter (the reference's example is destination:gke) and an
	// AIP-132 order_by such as "name desc, trigger_id".
	all, err := aipfilter.List(req.GetFilter(), req.GetOrderBy(), all, aipfilter.Alias("trigger_id", "name"))
	if err != nil {
		return nil, err
	}
	items, next, err := page(all, req.GetPageSize(), req.GetPageToken(), req.GetParent()+"|"+req.GetFilter()+"|"+req.GetOrderBy())
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
	return nil, apierror.NotFound("provider %s not found", req.GetName())
}

// ListProviders lists the providers served, by name.
func (e *EventarcServer) ListProviders(_ context.Context, req *eventarcpb.ListProvidersRequest) (*eventarcpb.ListProvidersResponse, error) {
	ps := eaProviders(req.GetParent())
	sort.Slice(ps, func(i, j int) bool { return ps[i].GetName() < ps[j].GetName() })
	ps, err := aipfilter.List(req.GetFilter(), req.GetOrderBy(), ps, aipfilter.Alias("_id", "name"))
	if err != nil {
		return nil, err
	}
	items, next, err := page(ps, req.GetPageSize(), req.GetPageToken(), req.GetParent()+"|"+req.GetFilter()+"|"+req.GetOrderBy())
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
	return f.deliverEvent(ctx, t, ev)
}

// deliverEvent posts one CloudEvent, in binary content mode, to a
// trigger's destination.
func (f *FunctionsServer) deliverEvent(ctx context.Context, t *eventarcpb.Trigger, ev *CloudEvent) (int, error) {
	project := strings.Split(t.GetName(), "/")[1]
	d := t.GetDestination()
	if he := d.GetHttpEndpoint(); he != nil {
		// An internal HTTP endpoint: sent to its URI as it is, from this
		// host, with no token, as Eventarc sends to one.
		ctx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, he.GetUri(), strings.NewReader(string(ev.Data)))
		if err != nil {
			return 0, err
		}
		for k, v := range ev.Headers {
			req.Header[k] = v
		}
		resp, err := f.events.Client.Do(req)
		if err != nil {
			return 0, err
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode, nil
	}
	var target, path string
	timeout := time.Minute
	if name := d.GetCloudFunction(); name != "" {
		fn, ok := eaFunction(name)
		if !ok {
			return 0, fmt.Errorf("the destination function %s is not found", name)
		}
		target = fn.GetServiceConfig().GetUri()
		if s := fn.GetServiceConfig().GetTimeoutSeconds(); s > 0 {
			timeout = time.Duration(s) * time.Second
		}
	} else {
		run := d.GetCloudRun()
		path = run.GetPath()
		svc, err := f.s.GetService(ctx, &runpb.GetServiceRequest{Name: "projects/" + project + "/locations/" + run.GetRegion() + "/services/" + run.GetService()})
		if err != nil {
			return 0, fmt.Errorf("the destination service: %w", err)
		}
		target = svc.GetUri()
		if d := svc.GetTemplate().GetTimeout(); d != nil && d.AsDuration() > 0 {
			timeout = d.AsDuration()
		}
	}
	u, err := url.Parse(target)
	if err != nil || u.Host == "" {
		return 0, fmt.Errorf("the destination has no URL")
	}
	if f.events.Ingress == "" {
		return 0, fmt.Errorf("no ingress port is published on this machine, so the destination cannot be reached")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+f.events.Ingress+path, strings.NewReader(string(ev.Data)))
	if err != nil {
		return 0, err
	}
	if path == "" {
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

// eaMatching are the topics of a project's triggers of one of types whose
// filters (other than type) the event's attributes, read by value, match.
func eaMatching(project string, types []string, value func(attribute string) (string, bool)) []string {
	var out []string
	match := func(name string, live *liveTrigger) {
		if live == nil || !strings.HasPrefix(name, "projects/"+project+"/") || !slices.Contains(types, live.spec.GetEventType()) {
			return
		}
		if eaFiltersMatch(live.spec.GetEventFilters(), value) && live.topic != "" {
			out = append(out, live.topic)
		}
	}
	eaState.mu.Lock()
	for _, n := range sortedKeys(eaState.triggers) {
		match(n, eaState.triggers[n].live)
	}
	eaState.mu.Unlock()
	// And 2nd-gen functions' own audit-log and Firestore triggers (#1853).
	for _, live := range functionTriggers() {
		match(live.spec.GetTrigger(), live)
	}
	return out
}

// functionTriggers are the live triggers of the functions' event_trigger,
// ordered by function name.
func functionTriggers() []*liveTrigger {
	functions.mu.Lock()
	defer functions.mu.Unlock()
	var out []*liveTrigger
	for _, n := range sortedKeys(functions.fns) {
		if t := functions.fns[n].trigger; t != nil {
			out = append(out, t)
		}
	}
	return out
}

// eaFiltersMatch reports whether every filter matches the event.
func eaFiltersMatch(filters []*functionspb.EventFilter, value func(attribute string) (string, bool)) bool {
	for _, f := range filters {
		v, ok := value(f.GetAttribute())
		if !ok {
			return false
		}
		if f.GetOperator() == pathPatternOp {
			if !matchPathPattern(f.GetValue(), v) {
				return false
			}
		} else if f.GetValue() != v {
			return false
		}
	}
	return true
}

// eaFunction is a Cloud Run function a trigger delivers to.
func eaFunction(name string) (*functionspb.Function, bool) {
	functions.mu.Lock()
	defer functions.mu.Unlock()
	sf, ok := functions.fns[name]
	if !ok {
		return nil, false
	}
	return cloneFunction(sf.fn), true
}
