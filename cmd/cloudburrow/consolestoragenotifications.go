package main

// Cloud Storage bucket notifications in the console (#791): a Notifications
// tab on a bucket's page, listed by notifications.list; each configuration's
// own page, read by notifications.get; Create notification, whose topic is
// chosen from the project's Pub/Sub topics; and Delete, on each row and on
// the configuration's page, confirmed by typing its name.
//
// Every call goes to the storage server's own notificationConfigs API, the
// calls an application makes: insert, list and delete through the official
// client's BucketHandle.AddNotification, Notifications and
// DeleteNotification, and get on the JSON API, which the client has no call
// for. A refusal is the API's own message.
//
// The storage server delivers only when it was started with a Pub/Sub
// emulator; without one, notifications.insert answers 501. The console then
// offers no Create notification, and the tab says why in the server's own
// words, read from that refusal.
//
// A configuration is addressed [bucket, "notificationConfigs/<id>"], after
// the API's own resource name (b/<bucket>/notificationConfigs/<id>). A
// folder's path segment never contains a slash, so the address cannot be a
// folder's, and a row's actions, which go to the page's path plus the row's
// name, reach the same path as its page's.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	urlpkg "net/url"
	"sort"
	"strings"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"cloud.google.com/go/storage"
	"google.golang.org/api/iterator"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// notificationSegment is the path segment of one configuration, before its ID.
const notificationSegment = "notificationConfigs/"

// notificationEvents are the event types a configuration can name, in the
// order the form lists them, with the words it uses for each.
var notificationEvents = []struct{ Type, Label string }{
	{storage.ObjectFinalizeEvent, "Object finalized (created or overwritten)"},
	{storage.ObjectMetadataUpdateEvent, "Object metadata updated"},
	{storage.ObjectDeleteEvent, "Object deleted"},
	{storage.ObjectArchiveEvent, "Object archived (became noncurrent)"},
}

// notificationPath reads a configuration's address.
func notificationPath(path []string) (bucket, id string, ok bool) {
	if len(path) != 2 || path[0] == objectPage || !strings.HasPrefix(path[1], notificationSegment) {
		return "", "", false
	}
	id = strings.TrimPrefix(path[1], notificationSegment)
	if id == "" || strings.Contains(id, "/") {
		return "", "", false
	}
	return path[0], id, true
}

func notificationName(id string) string { return notificationSegment + id }

// notificationDeleteAction is Delete, on a configuration's row and its page.
func notificationDeleteAction() console.Action {
	return console.Action{ID: "deletenotification", Label: "Delete", Destructive: true, Leaves: true}
}

// notificationCreateAction is Create notification, its topic one of topics.
func notificationCreateAction(topics []string) console.Action {
	fields := []console.Field{
		{Name: "topic", Label: "Pub/Sub topic", Type: "select", Required: true, Options: topics,
			Default: topics[0], Section: "Destination",
			Help: "The project's topics. Messages are published to this one."},
		{Name: "payloadFormat", Label: "Payload format", Type: "select", Required: true,
			Options: []string{storage.JSONPayload, storage.NoPayload}, Default: storage.JSONPayload,
			Section: "Destination",
			Help:    "JSON_API_V1: the object's metadata as the message body. NONE: attributes only."},
	}
	for i, e := range notificationEvents {
		f := console.Field{Name: "event_" + e.Type, Label: e.Label + " — " + e.Type, Type: "checkbox",
			Section: "Event types"}
		if i == 0 {
			f.Help = "None checked: every event type."
		}
		fields = append(fields, f)
	}
	return console.Action{ID: "createnotification", Label: "Create notification", Fields: append(fields,
		console.Field{Name: "prefix", Label: "Object name prefix", Type: "text", Section: "Filter and attributes",
			Help: "Optional. Only objects whose names begin with it, such as uploads/."},
		console.Field{Name: "attributes", Label: "Custom attributes", Type: "map", Section: "Filter and attributes",
			Help: "Optional. One key=value per line, added to every message; at most 10."},
	)}
}

// notificationCreate is Create notification for a bucket, or why it is not
// offered: no project chosen, no Pub/Sub on this instance, or no topic in the
// project to publish to.
func (p storageProvider) notificationCreate(ctx context.Context, project, bucket string) (*console.Action, string) {
	if p.pubsub == "" {
		return nil, p.noPubSubReason(ctx, bucket)
	}
	if project == "" {
		return nil, "Choose a project in the toolbar to create a notification: its topics are the project's."
	}
	topics, err := p.projectTopics(ctx, project)
	if err != nil {
		return nil, "Create notification is not offered: the project's Pub/Sub topics could not be listed: " + err.Error()
	}
	if len(topics) == 0 {
		return nil, fmt.Sprintf("Project %s has no Pub/Sub topics, so there is nothing to publish to. "+
			"Create a topic on the Pub/Sub screen, then create a notification here.", project)
	}
	a := notificationCreateAction(topics)
	return &a, ""
}

// noPubSubReason is why a console with no Pub/Sub offers no create. A storage
// server with no emulator refuses notifications.insert with a 501 before it
// reads the request, so the reason is its own message, asked for with a
// request it refuses whatever the answer: a server that does have a
// publisher refuses the empty topic as a bad request and creates nothing.
func (p storageProvider) noPubSubReason(ctx context.Context, bucket string) string {
	const lead = "Create notification is not offered: "
	u := fmt.Sprintf("http://%s/storage/v1/b/%s/notificationConfigs", p.endpoint, urlpkg.PathEscape(bucket))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader([]byte(`{"topic":""}`)))
	if err != nil {
		return lead + err.Error()
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return lead + "the storage server could not be asked whether it delivers notifications: " + err.Error()
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotImplemented {
		return lead + "the storage server answers notifications.insert with 501: " + apiError(resp).Error()
	}
	return lead + "Pub/Sub is not enabled on this instance, so the console has no topics to choose from."
}

// projectTopics are the project's topics, in full, sorted.
func (p storageProvider) projectTopics(ctx context.Context, project string) ([]string, error) {
	c, err := pubsubProvider{endpoint: p.pubsub}.client(ctx, project)
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.Close() }()
	it := c.TopicAdminClient.ListTopics(ctx, &pubsubpb.ListTopicsRequest{Project: "projects/" + project})
	var out []string
	for {
		t, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return nil, err
		}
		out = append(out, t.GetName())
	}
	sort.Strings(out)
	return out, nil
}

// bucketNotificationActions are what a bucket's page offers for its
// notifications.
func (p storageProvider) bucketNotificationActions(ctx context.Context, project, bucket string) []console.Action {
	if a, _ := p.notificationCreate(ctx, project, bucket); a != nil {
		return []console.Action{*a}
	}
	return nil
}

// notificationsSection is a bucket's Notifications tab, and the actions its
// page offers for them.
func (p storageProvider) notificationsSection(ctx context.Context, project, bucket string) (console.Section, []console.Action) {
	sec := console.Section{ID: "notifications", Label: "Notifications"}
	create, why := p.notificationCreate(ctx, project, bucket)
	var actions []console.Action
	if create != nil {
		actions = append(actions, *create)
	}
	c, err := p.storageClient(ctx)
	if err != nil {
		sec.Unavailable = err.Error()
		return sec, actions
	}
	defer func() { _ = c.Close() }()
	all, err := c.Bucket(bucket).Notifications(ctx)
	if err != nil {
		sec.Unavailable = "notifications.list: " + err.Error()
		return sec, actions
	}
	ids := make([]string, 0, len(all))
	for id := range all {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		if len(ids[i]) != len(ids[j]) {
			return len(ids[i]) < len(ids[j])
		}
		return ids[i] < ids[j]
	})
	list := console.Listing{
		Columns:    []string{"Topic", "Event types", "Object name prefix", "Payload format", "Custom attributes"},
		NameColumn: "Notification",
		Noun:       "notifications",
		Items:      []console.Resource{},
		Note:       why,
	}
	for _, id := range ids {
		n := all[id]
		list.Items = append(list.Items, console.Resource{
			Name:    notificationName(id),
			Opens:   []string{bucket, notificationName(id)},
			Fields:  notificationFields(n),
			Actions: []console.Action{notificationDeleteAction()},
		})
	}
	list.Total = len(list.Items)
	sec.Listing = list
	return sec, actions
}

func notificationTopic(n *storage.Notification) string {
	return "projects/" + n.TopicProjectID + "/topics/" + n.TopicID
}

func notificationEventText(types []string) string {
	if len(types) == 0 {
		return "All event types"
	}
	return strings.Join(types, ", ")
}

func notificationPrefixText(prefix string) string {
	if prefix == "" {
		return "All objects"
	}
	return prefix
}

func notificationAttributesText(attrs map[string]string) string {
	if len(attrs) == 0 {
		return "None"
	}
	keys := make([]string, 0, len(attrs))
	for k := range attrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+attrs[k])
	}
	return strings.Join(parts, ", ")
}

func notificationFields(n *storage.Notification) map[string]string {
	return map[string]string{
		"Topic":              notificationTopic(n),
		"Event types":        notificationEventText(n.EventTypes),
		"Object name prefix": notificationPrefixText(n.ObjectNamePrefix),
		"Payload format":     n.PayloadFormat,
		"Custom attributes":  notificationAttributesText(n.CustomAttributes),
	}
}

// notificationJSON is a configuration as notifications.get returns it.
type notificationJSON struct {
	ID               string            `json:"id"`
	Topic            string            `json:"topic"`
	EventTypes       []string          `json:"event_types"`
	ObjectNamePrefix string            `json:"object_name_prefix"`
	CustomAttributes map[string]string `json:"custom_attributes"`
	PayloadFormat    string            `json:"payload_format"`
	Etag             string            `json:"etag"`
	SelfLink         string            `json:"selfLink"`
}

// getNotification is notifications.get on the JSON API. A refusal is the
// API's own message, with its status.
func (p storageProvider) getNotification(ctx context.Context, bucket, id string) (notificationJSON, int, error) {
	var n notificationJSON
	u := fmt.Sprintf("http://%s/storage/v1/b/%s/notificationConfigs/%s", p.endpoint,
		urlpkg.PathEscape(bucket), urlpkg.PathEscape(id))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return n, 0, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return n, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return n, resp.StatusCode, apiError(resp)
	}
	return n, resp.StatusCode, json.NewDecoder(resp.Body).Decode(&n)
}

// notificationDetail is one configuration's page.
func (p storageProvider) notificationDetail(ctx context.Context, bucket, id string) (console.Detail, error) {
	n, code, err := p.getNotification(ctx, bucket, id)
	if code == http.StatusNotFound {
		return console.Detail{Unavailable: fmt.Sprintf(
			"%s has no notification configuration %s: it was deleted, or never existed (%v)", bucket, id, err)}, nil
	}
	if err != nil {
		return console.Detail{Unavailable: fmt.Sprintf("notifications.get %s/%s: %v", bucket, id, err)}, nil
	}
	topic := strings.TrimPrefix(n.Topic, "//pubsub.googleapis.com/")
	props := []console.Property{
		{Label: "Topic", Value: topic},
		{Label: "Event types", Value: notificationEventText(n.EventTypes)},
		{Label: "Object name prefix", Value: notificationPrefixText(n.ObjectNamePrefix)},
		{Label: "Payload format", Value: n.PayloadFormat},
	}
	attrs := make([]console.Property, 0, len(n.CustomAttributes))
	for k, v := range n.CustomAttributes {
		if v == "" {
			v = "(empty value)"
		}
		attrs = append(attrs, console.Property{Label: k, Value: v})
	}
	sort.Slice(attrs, func(i, j int) bool { return attrs[i].Label < attrs[j].Label })
	if len(attrs) == 0 {
		attrs = []console.Property{{Label: "Keys", Value: "None"}}
	}
	return console.Detail{
		Summary: []console.Property{
			{Label: "Bucket", Value: bucket},
			{Label: "ID", Value: n.ID},
			{Label: "Topic", Value: topic},
		},
		Sections: []console.Section{{ID: "configuration", Label: "Configuration", Kind: console.KindProperties,
			Groups: []console.PropertyGroup{
				{Heading: "Delivery", Properties: props},
				{Heading: "Custom attributes", Properties: attrs},
			},
			Note: "Each message also carries the standard attributes: notificationConfig, eventType, " +
				"payloadFormat, bucketId, objectId, objectGeneration and eventTime. A configuration " +
				"cannot be changed; delete it and create another."}},
	}, nil
}

// notificationActions are the actions at a configuration's address, and
// whether the path is one.
func (p storageProvider) notificationActions(ctx context.Context, path []string) ([]console.Action, bool) {
	bucket, id, ok := notificationPath(path)
	if !ok {
		return nil, false
	}
	if _, _, err := p.getNotification(ctx, bucket, id); err != nil {
		return nil, true // gone: nothing can be done to it, and its page says why
	}
	return []console.Action{notificationDeleteAction()}, true
}

// actOnNotifications performs Create notification or Delete, and reports
// whether the action was one of them.
func (p storageProvider) actOnNotifications(ctx context.Context, project string, path []string, action string, values map[string]string) (bool, error) {
	switch action {
	case "createnotification":
		if len(path) != 1 || path[0] == objectPage {
			return true, errors.New("a notification is created on its bucket's page")
		}
		return true, p.createNotification(ctx, project, path[0], values)
	case "deletenotification":
		bucket, id, ok := notificationPath(path)
		if !ok {
			return true, errors.New("delete names one notification configuration")
		}
		c, err := p.storageClient(ctx)
		if err != nil {
			return true, err
		}
		defer func() { _ = c.Close() }()
		return true, c.Bucket(bucket).DeleteNotification(ctx, id)
	}
	return false, nil
}

// createNotification is notifications.insert through the official client,
// with the values the form submits.
func (p storageProvider) createNotification(ctx context.Context, project, bucket string, values map[string]string) error {
	topic := strings.TrimSpace(values["topic"])
	// One of the topics the form offered: the storage server does not check
	// that a topic exists, and a configuration naming one that does not would
	// publish nowhere.
	create, why := p.notificationCreate(ctx, project, bucket)
	if create == nil {
		return errors.New(why)
	}
	offered := false
	for _, t := range create.Fields[0].Options {
		offered = offered || t == topic
	}
	if !offered {
		return fmt.Errorf("topic %q is not one of project %s's Pub/Sub topics", topic, project)
	}
	parts := strings.Split(topic, "/")
	if len(parts) != 4 {
		return fmt.Errorf("topic %q is not projects/{project}/topics/{topic}", topic)
	}
	attrs, err := console.ParseMap(values["attributes"])
	if err != nil {
		return fmt.Errorf("custom attributes: %w", err)
	}
	for k := range attrs {
		if strings.TrimSpace(k) == "" {
			return errors.New("custom attributes: a key cannot be empty")
		}
	}
	n := &storage.Notification{
		TopicProjectID:   parts[1],
		TopicID:          parts[3],
		PayloadFormat:    strings.TrimSpace(values["payloadFormat"]),
		ObjectNamePrefix: values["prefix"],
	}
	if len(attrs) > 0 {
		n.CustomAttributes = attrs
	}
	for _, e := range notificationEvents {
		if values["event_"+e.Type] == "true" {
			n.EventTypes = append(n.EventTypes, e.Type)
		}
	}
	c, err := p.storageClient(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	_, err = c.Bucket(bucket).AddNotification(ctx, n)
	return err
}
