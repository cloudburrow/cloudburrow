package run

// Pub/Sub build triggers (#1506), built as Google documents them: a trigger
// with pubsubConfig.topic gets a subscription on that topic, made for it
// (pubsubConfig.subscription, output only), and every message published to
// the topic runs the trigger's build. Its substitutions bind to the message
// as `$(body.message.data.tag)` and `$(body.message.attributes.bucketId)`
// do in Google's examples: body is the push envelope, whose message.data is
// decoded from base64 and, when it is JSON, read as an object.
//
// The subscription is a push subscription on this instance's Pub/Sub, to
// this port, as Google's is a subscription Cloud Build owns; its name,
// gcb-{trigger id}, is CloudBurrow's, since Google documents none. A
// disabled trigger's messages are acknowledged and dropped.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"

	cbpb "cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"

	"google.golang.org/grpc/codes"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// cloudBuildPushPrefix is the path a Pub/Sub trigger's subscription pushes
// to, followed by the trigger's resource name.
const cloudBuildPushPrefix = "/cloudburrow/cloudbuild/push/"

// triggerEvents is what Pub/Sub triggers need from the instance.
type triggerEvents struct {
	transport EventTransport
	pushBase  func() string
}

// WithTriggerEvents enables Pub/Sub triggers, through the instance's
// Pub/Sub; without it a pubsubConfig trigger is FAILED_PRECONDITION.
func (b *BuildsServer) WithTriggerEvents(t EventTransport, pushBase func() string) *BuildsServer {
	if t != nil && pushBase != nil {
		b.events = &triggerEvents{transport: t, pushBase: pushBase}
	}
	return b
}

// validPubsubConfig checks a trigger's pubsubConfig.
func validPubsubConfig(pc *cbpb.PubsubConfig) error {
	topic := pc.GetTopic()
	if !strings.HasPrefix(topic, "projects/") || !strings.Contains(topic, "/topics/") {
		return apierror.InvalidArgument("invalid trigger: pubsubConfig.topic %q must be projects/PROJECT/topics/TOPIC", topic)
	}
	return nil
}

// subscribeTrigger makes t's subscription on its topic and records it on
// t, or does nothing for a trigger with no pubsubConfig.
func (c *CloudBuildServer) subscribeTrigger(ctx context.Context, t *cbpb.BuildTrigger) error {
	pc := t.GetPubsubConfig()
	if pc == nil {
		return nil
	}
	e := c.events()
	if e == nil {
		return apierror.FailedPrecondition("a pubsubConfig trigger subscribes to its topic on this instance's Pub/Sub, " +
			"which is not served: start the instance with pubsub in --services")
	}
	base := e.pushBase()
	if base == "" {
		return &apierror.Error{Code: codes.Unavailable, Message: "the trigger's push endpoint is not published to the cluster yet; retry"}
	}
	project, _, _ := strings.Cut(strings.TrimPrefix(t.GetResourceName(), "projects/"), "/")
	sub := "projects/" + project + "/subscriptions/gcb-" + t.GetId()
	err := e.transport.CreatePushSubscription(ctx, PushSubscription{Name: sub, Topic: pc.GetTopic(),
		Endpoint: strings.TrimSuffix(base, "/") + cloudBuildPushPrefix + t.GetResourceName(),
		Labels:   map[string]string{"goog-managed-by": "cloudbuild"}})
	if err != nil {
		if strings.Contains(err.Error(), "NotFound") || strings.Contains(err.Error(), "not found") {
			return apierror.NotFound("pubsubConfig.topic %s not found", pc.GetTopic())
		}
		return apierror.FailedPrecondition("subscribe to pubsubConfig.topic %s: %v", pc.GetTopic(), err)
	}
	pc.Subscription = sub
	pc.State = cbpb.PubsubConfig_OK
	return nil
}

// unsubscribeTrigger removes the subscription a trigger was given.
func (c *CloudBuildServer) unsubscribeTrigger(ctx context.Context, t *cbpb.BuildTrigger) {
	sub := t.GetPubsubConfig().GetSubscription()
	if sub == "" || c.events() == nil {
		return
	}
	if err := c.events().transport.DeleteSubscription(ctx, sub); err != nil {
		slog.Warn("cloudbuild: delete the trigger's subscription", "subscription", sub, "err", err)
	}
}

func (c *CloudBuildServer) events() *triggerEvents {
	if c.builds == nil {
		return nil
	}
	return c.builds.events
}

// pushBody is the push envelope as a trigger's bindings read it: its
// message's data decoded, and parsed when it is JSON.
func pushBody(raw []byte) any {
	var env map[string]any
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil
	}
	msg, _ := env["message"].(map[string]any)
	if s, ok := msg["data"].(string); ok {
		if data, err := base64.StdEncoding.DecodeString(s); err == nil {
			var v any
			if json.Unmarshal(data, &v) == nil {
				msg["data"] = v
			} else {
				msg["data"] = string(data)
			}
		}
	}
	return env
}

// servePush takes one push of a Pub/Sub trigger's subscription and runs its
// build. A push for a trigger that is gone, disabled, or whose build is
// refused, is acknowledged, as a retry would fail the same way.
func servePush(c *CloudBuildServer, w http.ResponseWriter, r *http.Request) bool {
	name, ok := strings.CutPrefix(r.URL.Path, cloudBuildPushPrefix)
	if !ok || r.Method != http.MethodPost || c == nil {
		return false
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return true
	}
	triggerStore.mu.Lock()
	t, err := triggerOf(name, "", "")
	if err == nil && t.GetPubsubConfig() == nil {
		err = apierror.NotFound("trigger %s has no pubsubConfig", name)
	}
	triggerStore.mu.Unlock()
	if err != nil {
		w.WriteHeader(http.StatusNoContent)
		return true
	}
	if t.GetDisabled() {
		w.WriteHeader(http.StatusNoContent)
		return true
	}
	if _, err := c.runTrigger(withCaller(r).Context(), name, "", "", nil, pushBody(raw)); err != nil {
		slog.Warn("cloudbuild: a Pub/Sub trigger's build was refused", "trigger", name, "err", err)
	}
	w.WriteHeader(http.StatusNoContent)
	return true
}
