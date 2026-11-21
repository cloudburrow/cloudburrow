package main

// Publish message on a bus's page (#2006): a message put on the bus with
// the Publishing API (Publisher/Publish, json_message), which then
// reaches each enrollment whose CEL match accepts it, as one published by
// an application or gcloud eventarc message-buses publish does. Google's
// console has no publish control (its Publish events directly guide gives
// gcloud and the API only), so this one is CloudBurrow's own, labelled so,
// and takes gcloud's --event-id, --event-source, --event-type,
// --event-data and --event-attributes.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	publishing "cloud.google.com/go/eventarc/publishing/apiv1"
	publishingpb "cloud.google.com/go/eventarc/publishing/apiv1/publishingpb"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

const eaPublishAction = "publish-message"

// eaPublishLabel names the action as CloudBurrow's own.
const eaPublishLabel = "Publish message (CloudBurrow extension)"

// eaPublishFields are gcloud eventarc message-buses publish's event flags.
func eaPublishFields() []console.Field {
	return []console.Field{
		{Name: "eventId", Label: "Event ID", Type: "text", Section: "Event",
			Help: "The CloudEvent's id (--event-id); empty for a new unique one."},
		{Name: "eventSource", Label: "Event source", Type: "text", Required: true, Default: "//cloudburrow/console", Section: "Event",
			Help: "The CloudEvent's source (--event-source)."},
		{Name: "eventType", Label: "Event type", Type: "text", Required: true, Section: "Event",
			Help: "The CloudEvent's type (--event-type), which an enrollment's CEL expression can match as message.type."},
		{Name: "eventData", Label: "Event data (JSON)", Type: "textarea", Default: "{}", Section: "Event",
			Help: "The CloudEvent's data (--event-data), sent with datacontenttype application/json."},
		{Name: "eventAttributes", Label: "Event attributes", Type: "map", Section: "Event",
			KeyPattern: `^[a-z0-9]{1,20}$`, MaxEntries: 64,
			Help: "Optional extension attributes (--event-attributes): lowercase letters and digits, at most 20."},
	}
}

// eaPublishMessage is the json_message the form's values make: a
// structured-mode CloudEvent.
func eaPublishMessage(values map[string]string, now time.Time) (string, error) {
	ev := map[string]any{"specversion": "1.0", "datacontenttype": "application/json"}
	id := strings.TrimSpace(values["eventId"])
	if id == "" {
		id = "console-" + strconv.FormatInt(now.UnixNano(), 36)
	}
	ev["id"] = id
	for name, key := range map[string]string{"eventSource": "source", "eventType": "type"} {
		v := strings.TrimSpace(values[name])
		if v == "" {
			return "", fmt.Errorf("the event needs its %s", key)
		}
		ev[key] = v
	}
	data := strings.TrimSpace(values["eventData"])
	if data == "" {
		data = "{}"
	}
	if !json.Valid([]byte(data)) {
		return "", errors.New("the event data is not JSON")
	}
	ev["data"] = json.RawMessage(data)
	attrs, err := console.ParseMap(values["eventAttributes"])
	if err != nil {
		return "", err
	}
	for k, v := range attrs {
		if _, taken := ev[k]; taken {
			return "", fmt.Errorf("attribute %q is a CloudEvent context attribute the form sets", k)
		}
		ev[k] = v
	}
	b, err := json.Marshal(ev)
	return string(b), err
}

// DetailActions offers Publish message on a bus's page.
func (p eaBusesProvider) DetailActions(_ context.Context, _ string, path []string) []console.Action {
	if len(path) != 1 {
		return nil
	}
	return []console.Action{{ID: eaPublishAction, Label: eaPublishLabel, Submit: "Publish", Fields: eaPublishFields()}}
}

// ActAt publishes the message.
func (p eaBusesProvider) ActAt(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	if action != eaPublishAction || len(path) != 1 {
		return fmt.Errorf("a bus has no action %q", action)
	}
	b, err := p.find(ctx, project, path[0])
	if err != nil {
		return err
	}
	msg, err := eaPublishMessage(values, time.Now())
	if err != nil {
		return err
	}
	if p.run.runEndpoint() == "" {
		return errors.New("the Cloud Run adapter is not running")
	}
	c, err := publishing.NewPublisherClient(ctx, option.WithEndpoint(p.run.runEndpoint()), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		return fmt.Errorf("connect to the Eventarc Publishing API: %w", err)
	}
	defer func() { _ = c.Close() }()
	_, err = c.Publish(ctx, &publishingpb.PublishRequest{MessageBus: b.GetName(),
		Format: &publishingpb.PublishRequest_JsonMessage{JsonMessage: msg}})
	return err
}

var _ console.PathActor = eaBusesProvider{}
