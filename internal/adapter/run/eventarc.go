package run

// Event triggers of 2nd-gen functions (#1414), built as Google builds them:
// a function's event_trigger is an Eventarc trigger, whose transport is a
// Pub/Sub push subscription Eventarc makes, and whose events reach the
// function's Cloud Run service as CloudEvents in binary content mode.
//
//   - google.cloud.pubsub.topic.v1.messagePublished: a push subscription on
//     the trigger's topic.
//   - google.cloud.storage.object.v1.*: a topic Eventarc makes, the bucket's
//     Pub/Sub notification on it for the event type, and a push subscription
//     on that topic.
//
// The subscription pushes to this port, not to the service: Google's push
// front end converts a push to a CloudEvent and attaches the trigger's
// service account token, and here this process does both, then POSTs the
// CloudEvent to the function's URL through the cluster's ingress. Its answer
// is the push's answer, so the emulator retries a failed event when the
// trigger's retry policy says to.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strings"
	"time"

	functionspb "cloud.google.com/go/functions/apiv2/functionspb"
	"google.golang.org/protobuf/proto"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// Event types a trigger can name.
const (
	pubsubPublished       = "google.cloud.pubsub.topic.v1.messagePublished"
	storageEventPrefix    = "google.cloud.storage.object.v1."
	eventarcPushPrefix    = "/cloudburrow/eventarc/push/"
	cloudEventSpecVersion = "1.0"
)

// storageNotificationTypes maps a Cloud Storage CloudEvent type to the
// notification event type its bucket notification names, as Google's
// Eventarc documents the pairs.
var storageNotificationTypes = map[string]string{
	storageEventPrefix + "finalized":       "OBJECT_FINALIZE",
	storageEventPrefix + "deleted":         "OBJECT_DELETE",
	storageEventPrefix + "archived":        "OBJECT_ARCHIVE",
	storageEventPrefix + "metadataUpdated": "OBJECT_METADATA_UPDATE",
}

// EventTransport makes and removes what a trigger delivers through, on the
// instance's Pub/Sub and Cloud Storage. The CLI implements it with the
// official Pub/Sub client and the Storage JSON API; the adapter declares it
// so it imports neither service.
type EventTransport interface {
	// CreateTopic makes a topic; one that exists already is not an error.
	CreateTopic(ctx context.Context, topic string, labels map[string]string) error
	DeleteTopic(ctx context.Context, topic string) error
	CreatePushSubscription(ctx context.Context, sub PushSubscription) error
	DeleteSubscription(ctx context.Context, name string) error
	// CreateNotification adds a JSON_API_V1 notification of eventType on
	// bucket to topic and returns its ID.
	CreateNotification(ctx context.Context, bucket, topic, eventType string) (string, error)
	DeleteNotification(ctx context.Context, bucket, id string) error
}

// PushSubscription is the subscription a trigger makes.
type PushSubscription struct {
	Name, Topic, Endpoint string
	Labels                map[string]string
}

// IDTokenMinter mints the ID token a delivery carries, signed by the
// instance's own key (internal/metadata).
type IDTokenMinter interface {
	ServiceAccountIDToken(account, audience string) (string, error)
}

// Events is what event triggers need from the instance.
type Events struct {
	Transport EventTransport
	Tokens    IDTokenMinter
	// PushBase is the URL the Pub/Sub emulator, in the cluster, reaches
	// this port at, or "" before it is published.
	PushBase func() string
	// Ingress is the host address of the cluster's ingress gateway, which
	// a delivery is sent to with the function's host; "" when no ingress
	// port is published.
	Ingress string
	Client  *http.Client
}

// WithEvents enables event triggers.
func (f *FunctionsServer) WithEvents(e Events) *FunctionsServer {
	if e.Client == nil {
		e.Client = &http.Client{}
	}
	f.events = &e
	return f
}

// liveTrigger is what a function's trigger made.
type liveTrigger struct {
	spec         *functionspb.EventTrigger
	subscription string
	// topic and notification are the storage transport's, empty for a
	// Pub/Sub trigger, whose topic is the user's.
	topic, bucket, notification string
}

// checkEventTrigger refuses a trigger this instance cannot honour.
func (f *FunctionsServer) checkEventTrigger(t *functionspb.EventTrigger) error {
	if t == nil {
		return nil
	}
	if f.events == nil {
		return apierror.Unimplemented("event_trigger needs Pub/Sub: a 2nd-gen trigger is an Eventarc trigger whose transport is a " +
			"Pub/Sub subscription, and this instance serves none; start it with --services run,storage,pubsub")
	}
	if t.GetChannel() != "" {
		return apierror.Unimplemented("event_trigger.channel is not supported: only Google sources are served")
	}
	if t.GetService() != "" {
		return apierror.Unimplemented("event_trigger.service is not supported")
	}
	switch typ := t.GetEventType(); {
	case typ == "":
		return apierror.InvalidArgument("event_trigger.event_type is required")
	case typ == pubsubPublished:
		if !strings.HasPrefix(t.GetPubsubTopic(), "projects/") || !strings.Contains(t.GetPubsubTopic(), "/topics/") {
			return apierror.InvalidArgument("event_trigger.pubsub_topic %q must be projects/*/topics/*", t.GetPubsubTopic())
		}
		if len(t.GetEventFilters()) > 0 {
			return apierror.Unimplemented("event_trigger.event_filters on a Pub/Sub trigger are not supported")
		}
	case storageNotificationTypes[typ] != "":
		if storageBucket(t) == "" {
			return apierror.InvalidArgument("event_trigger.event_filters must name the bucket: attribute \"bucket\"")
		}
		for _, ef := range t.GetEventFilters() {
			if ef.GetAttribute() != "bucket" || ef.GetOperator() != "" {
				return apierror.Unimplemented("event_trigger.event_filters other than an exact \"bucket\" are not supported")
			}
		}
	default:
		return apierror.Unimplemented("event_trigger.event_type %q is not supported: Pub/Sub's messagePublished and Cloud Storage's "+
			"finalized, deleted, archived and metadataUpdated are", typ)
	}
	return nil
}

func storageBucket(t *functionspb.EventTrigger) string {
	for _, ef := range t.GetEventFilters() {
		if ef.GetAttribute() == "bucket" {
			return ef.GetValue()
		}
	}
	return ""
}

// defaultEventTrigger fills what Google fills: the region, the default
// compute account and DO_NOT_RETRY. The trigger name is set when it is made.
func defaultEventTrigger(fn *functionspb.Function) {
	t := fn.GetEventTrigger()
	if t == nil {
		return
	}
	parts := strings.Split(fn.GetName(), "/")
	if t.TriggerRegion == "" && len(parts) > 3 {
		t.TriggerRegion = parts[3]
	}
	if t.ServiceAccountEmail == "" {
		t.ServiceAccountEmail = fn.GetServiceConfig().GetServiceAccountEmail()
	}
	if t.RetryPolicy == functionspb.EventTrigger_RETRY_POLICY_UNSPECIFIED {
		t.RetryPolicy = functionspb.EventTrigger_RETRY_POLICY_DO_NOT_RETRY
	}
}

// sameTrigger reports whether a live trigger already serves spec.
func sameTrigger(live *liveTrigger, spec *functionspb.EventTrigger) bool {
	if live == nil || spec == nil {
		return live == nil && spec == nil
	}
	a, b := proto.Clone(live.spec).(*functionspb.EventTrigger), proto.Clone(spec).(*functionspb.EventTrigger)
	a.Trigger, b.Trigger = "", ""
	return proto.Equal(a, b)
}

// syncTrigger makes the function's trigger match its event_trigger once its
// service is deployed: a changed trigger is replaced, a removed one torn
// down. It records the trigger's name in event_trigger.trigger.
func (f *FunctionsServer) syncTrigger(ctx context.Context, name string) error {
	functions.mu.Lock()
	sf, ok := functions.fns[name]
	if !ok {
		functions.mu.Unlock()
		return functionNotFound(name)
	}
	spec := proto.Clone(sf.fn.GetEventTrigger()).(*functionspb.EventTrigger)
	if sf.fn.GetEventTrigger() == nil {
		spec = nil
	}
	live := sf.trigger
	functions.mu.Unlock()
	if sameTrigger(live, spec) {
		if live != nil {
			functions.mu.Lock()
			if sf.fn.EventTrigger != nil {
				sf.fn.EventTrigger.Trigger = live.spec.GetTrigger()
			}
			functions.mu.Unlock()
		}
		return nil
	}
	if live != nil {
		f.teardown(ctx, live)
		functions.mu.Lock()
		sf.trigger = nil
		functions.mu.Unlock()
	}
	if spec == nil {
		return nil
	}
	made, err := f.makeTrigger(ctx, name, spec)
	if err != nil {
		return err
	}
	functions.mu.Lock()
	sf.trigger = made
	if sf.fn.EventTrigger != nil {
		sf.fn.EventTrigger.Trigger = made.spec.GetTrigger()
	}
	functions.mu.Unlock()
	return nil
}

// makeTrigger makes the trigger's transport, named as Eventarc names a
// function's: trigger {function}-{6 digits}, subscription
// eventarc-{region}-{trigger}-sub-{3 digits} and, for Cloud Storage, topic
// eventarc-{region}-{trigger}-{3 digits}, all in the function's project.
func (f *FunctionsServer) makeTrigger(ctx context.Context, fnName string, spec *functionspb.EventTrigger) (*liveTrigger, error) {
	parts := strings.Split(fnName, "/")
	project, region, id := parts[1], parts[3], parts[5]
	triggerID := fmt.Sprintf("%s-%06d", id, rand.IntN(1000000))
	spec = proto.Clone(spec).(*functionspb.EventTrigger)
	spec.Trigger = fmt.Sprintf("projects/%s/locations/%s/triggers/%s", project, spec.GetTriggerRegion(), triggerID)
	base := ""
	if f.events.PushBase != nil {
		base = f.events.PushBase()
	}
	if base == "" {
		return nil, apierror.FailedPrecondition("the event trigger's push endpoint is not published to the cluster yet")
	}
	labels := map[string]string{"goog-managed-by": "eventarc"}
	live := &liveTrigger{spec: spec,
		subscription: fmt.Sprintf("projects/%s/subscriptions/eventarc-%s-%s-sub-%03d", project, region, triggerID, rand.IntN(1000))}
	topic := spec.GetPubsubTopic()
	if bucket := storageBucket(spec); spec.GetEventType() != pubsubPublished {
		topic = fmt.Sprintf("projects/%s/topics/eventarc-%s-%s-%03d", project, region, triggerID, rand.IntN(1000))
		if err := f.events.Transport.CreateTopic(ctx, topic, labels); err != nil {
			return nil, fmt.Errorf("create the trigger's topic %s: %w", topic, err)
		}
		live.topic, live.bucket = topic, bucket
		nid, err := f.events.Transport.CreateNotification(ctx, bucket, topic, storageNotificationTypes[spec.GetEventType()])
		if err != nil {
			f.teardown(ctx, live)
			return nil, fmt.Errorf("add the notification on bucket %s for the trigger: %w", bucket, err)
		}
		live.notification = nid
	}
	endpoint := strings.TrimSuffix(base, "/") + eventarcPushPrefix + spec.GetTrigger()
	if err := f.events.Transport.CreatePushSubscription(ctx, PushSubscription{Name: live.subscription, Topic: topic,
		Endpoint: endpoint, Labels: labels}); err != nil {
		f.teardown(ctx, live)
		return nil, fmt.Errorf("create the trigger's subscription on %s: %w", topic, err)
	}
	return live, nil
}

// teardown removes what a trigger made; the user's topic is left.
func (f *FunctionsServer) teardown(ctx context.Context, live *liveTrigger) {
	if f.events == nil || live == nil {
		return
	}
	t := f.events.Transport
	if live.subscription != "" {
		if err := t.DeleteSubscription(ctx, live.subscription); err != nil {
			slog.Warn("functions: delete the trigger's subscription", "subscription", live.subscription, "err", err)
		}
	}
	if live.notification != "" {
		if err := t.DeleteNotification(ctx, live.bucket, live.notification); err != nil {
			slog.Warn("functions: delete the trigger's notification", "bucket", live.bucket, "err", err)
		}
	}
	if live.topic != "" {
		if err := t.DeleteTopic(ctx, live.topic); err != nil {
			slog.Warn("functions: delete the trigger's topic", "topic", live.topic, "err", err)
		}
	}
}

// pushEnvelope is the body of a Pub/Sub push.
type pushEnvelope struct {
	Message      map[string]any `json:"message"`
	Subscription string         `json:"subscription"`
}

// CloudEvent is one event in binary content mode: its ce-* headers and its
// data, with the data's content type.
type CloudEvent struct {
	Headers http.Header
	Data    []byte
}

func str(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// ToCloudEvent converts a push of a trigger's subscription to the CloudEvent
// Eventarc delivers for it: for Pub/Sub, MessagePublishedData (the message
// and the subscription); for Cloud Storage, the object resource the
// notification carries, with ce-subject objects/{name} and the bucket as
// the ce-bucket extension.
func ToCloudEvent(spec *functionspb.EventTrigger, body []byte) (*CloudEvent, error) {
	var env pushEnvelope
	if err := json.Unmarshal(body, &env); err != nil || env.Message == nil {
		return nil, fmt.Errorf("not a Pub/Sub push body: %v", err)
	}
	m := env.Message
	h := http.Header{}
	h.Set("ce-id", str(m, "messageId", "message_id"))
	h.Set("ce-specversion", cloudEventSpecVersion)
	h.Set("ce-type", spec.GetEventType())
	h.Set("Content-Type", "application/json")
	if spec.GetEventType() == pubsubPublished {
		h.Set("ce-source", "//pubsub.googleapis.com/"+spec.GetPubsubTopic())
		h.Set("ce-time", str(m, "publishTime", "publish_time"))
		// Google's MessagePublishedData carries both spellings of the
		// message's ID and time, as a push does.
		if id := str(m, "messageId", "message_id"); id != "" {
			m["messageId"], m["message_id"] = id, id
		}
		if pt := str(m, "publishTime", "publish_time"); pt != "" {
			m["publishTime"], m["publish_time"] = pt, pt
		}
		data, err := json.Marshal(map[string]any{"message": m, "subscription": env.Subscription})
		if err != nil {
			return nil, err
		}
		return &CloudEvent{Headers: h, Data: data}, nil
	}
	attrs, _ := m["attributes"].(map[string]any)
	bucket, object := str(attrs, "bucketId"), str(attrs, "objectId")
	raw, err := base64.StdEncoding.DecodeString(str(m, "data"))
	if err != nil {
		return nil, fmt.Errorf("the notification's data is not base64: %w", err)
	}
	h.Set("ce-source", "//storage.googleapis.com/projects/_/buckets/"+bucket)
	h.Set("ce-subject", "objects/"+object)
	h.Set("ce-bucket", bucket)
	t := str(attrs, "eventTime")
	if t == "" {
		t = str(m, "publishTime", "publish_time")
	}
	h.Set("ce-time", t)
	return &CloudEvent{Headers: h, Data: raw}, nil
}

// triggerFunction finds the function a trigger name belongs to.
func triggerFunction(trigger string) (*functionspb.Function, bool) {
	functions.mu.Lock()
	defer functions.mu.Unlock()
	for _, sf := range functions.fns {
		if sf.trigger != nil && sf.trigger.spec.GetTrigger() == trigger {
			return cloneFunction(sf.fn), true
		}
	}
	return nil, false
}

// push takes one push of a trigger's subscription and delivers its
// CloudEvent to the function. A failed delivery answers the function's
// status, so Pub/Sub retries it, only under RETRY_POLICY_RETRY; with
// DO_NOT_RETRY it is acknowledged and dropped, as Google drops it.
func (f *FunctionsServer) push(w http.ResponseWriter, r *http.Request) {
	trigger := strings.TrimPrefix(r.URL.Path, eventarcPushPrefix)
	fn, ok := triggerFunction(trigger)
	if !ok {
		// An unknown trigger's push is acknowledged: its subscription is
		// being torn down, and nothing would ever take it.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	spec := fn.GetEventTrigger()
	retry := spec.GetRetryPolicy() == functionspb.EventTrigger_RETRY_POLICY_RETRY
	code, err := f.deliver(r.Context(), fn, body)
	if err == nil && pushSucceeded(code) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	slog.Warn("functions: event delivery failed", "function", fn.GetName(), "trigger", trigger, "status", code, "err", err, "retry", retry)
	if !retry {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if code == 0 || pushSucceeded(code) {
		code = http.StatusBadGateway
	}
	w.WriteHeader(code)
}

func pushSucceeded(code int) bool {
	switch code {
	case 102, 200, 201, 202, 204:
		return true
	}
	return false
}

// deliver POSTs the CloudEvent of a push to the function's service, through
// the ingress, with an ID token for the trigger's service account whose
// audience is the service's URL.
func (f *FunctionsServer) deliver(ctx context.Context, fn *functionspb.Function, body []byte) (int, error) {
	ev, err := ToCloudEvent(fn.GetEventTrigger(), body)
	if err != nil {
		return http.StatusBadRequest, err
	}
	target := fn.GetServiceConfig().GetUri()
	u, err := url.Parse(target)
	if err != nil || u.Host == "" {
		return 0, fmt.Errorf("the function has no service URL")
	}
	if f.events.Ingress == "" {
		return 0, fmt.Errorf("no ingress port is published on this machine, so the function cannot be reached")
	}
	timeout := time.Duration(fn.GetServiceConfig().GetTimeoutSeconds()) * time.Second
	if timeout <= 0 {
		timeout = time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+f.events.Ingress+"/", strings.NewReader(string(ev.Data)))
	if err != nil {
		return 0, err
	}
	req.Host = u.Host
	for k, v := range ev.Headers {
		req.Header[k] = v
	}
	if f.events.Tokens != nil {
		tok, err := f.events.Tokens.ServiceAccountIDToken(fn.GetEventTrigger().GetServiceAccountEmail(), strings.TrimSuffix(target, "/"))
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
