package run

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	fv1 "cloud.google.com/go/functions/apiv1/functionspb"
	functionspb "cloud.google.com/go/functions/apiv2/functionspb"
	"github.com/google/uuid"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// callEvent (#1958) is v1 CallFunction of an event-triggered (background)
// function: Google passes data to the function as its event's data
// ("JSON string with data that will be passed to the function", gcloud
// functions call). A 1st-gen function runs here on the 2nd-gen machinery
// and takes CloudEvents, which the Functions Framework's event signature
// converts back to (data, context), so the call is delivered as the
// CloudEvent its trigger would deliver, with data as the event's data:
//
//   - Pub/Sub: data is the PubsubMessage (gcloud's example is
//     {"data": "<base64>"}), the message of MessagePublishedData, with a
//     new messageId and publishTime;
//   - Cloud Storage: data is the object resource; its bucket and name are
//     the ce-bucket and ce-subject (objects/{name}), defaulting to the
//     trigger's bucket.
//
// It goes through the delivery path a trigger's push takes (deliver), so
// its headers, token and ingress are the trigger's. The function's error
// status is the call's error; a success has no result, as a background
// function returns none.
func (v *FunctionsV1Server) callEvent(ctx context.Context, name string, fn *functionspb.Function, data string) (*fv1.CallFunctionResponse, error) {
	f := v.f
	if f.events == nil {
		return nil, apierror.FailedPrecondition("this instance delivers no events: start it with --services run,storage,pubsub to call %s", name)
	}
	var payload any = map[string]any{}
	if strings.TrimSpace(data) != "" {
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			return nil, apierror.InvalidArgument("data must be a JSON string for an event-triggered function: %v", err)
		}
	}
	spec := fn.GetEventTrigger()
	id := fmt.Sprintf("%d", time.Now().UnixNano()/1000)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	var msg map[string]any
	switch t := spec.GetEventType(); {
	case t == pubsubPublished:
		m, ok := payload.(map[string]any)
		if !ok {
			return nil, apierror.InvalidArgument("data for a Pub/Sub function must be a JSON object, the PubsubMessage")
		}
		msg = m
		msg["messageId"], msg["publishTime"] = id, now
	case storageNotificationTypes[t] != "":
		obj, ok := payload.(map[string]any)
		if !ok {
			return nil, apierror.InvalidArgument("data for a Cloud Storage function must be a JSON object, the object resource")
		}
		bucket := str(obj, "bucket")
		if bucket == "" {
			for _, ef := range spec.GetEventFilters() {
				if ef.GetAttribute() == "bucket" {
					bucket = ef.GetValue()
				}
			}
			obj["bucket"] = bucket
		}
		raw, _ := json.Marshal(obj)
		msg = map[string]any{"messageId": id, "publishTime": now, "data": base64.StdEncoding.EncodeToString(raw),
			"attributes": map[string]any{"bucketId": bucket, "objectId": str(obj, "name"), "eventTime": now}}
	default:
		return nil, apierror.Unimplemented("call of a function triggered by %s is not supported", t)
	}
	body, _ := json.Marshal(pushEnvelope{Message: msg, Subscription: "projects/" + strings.Split(name, "/")[1] + "/subscriptions/gcf-call"})
	cctx, cancel := context.WithTimeout(ctx, time.Duration(fn.GetServiceConfig().GetTimeoutSeconds())*time.Second+5*time.Second)
	defer cancel()
	out := &fv1.CallFunctionResponse{ExecutionId: strings.ReplaceAll(uuid.NewString(), "-", "")[:12]}
	code, err := f.deliver(cctx, fn, body)
	if err != nil {
		return nil, apierror.FailedPrecondition("call %s: %v", name, err)
	}
	if code >= 400 {
		out.Error = fmt.Sprintf("%d %s", code, http.StatusText(code))
	}
	return out, nil
}
