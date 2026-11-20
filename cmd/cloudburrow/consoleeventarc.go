package main

// Eventarc's own console product (#1877): Google's Triggers page
// (/eventarc/triggers) lists every trigger of the project, Create trigger is
// Google's page (trigger name, the event provider and its event type with
// the type's filters, the region, the service account and the event
// destination), a trigger's page shows what GetTrigger reads of it, Edit
// changes what UpdateTrigger changes, and Delete deletes it, each through
// the official Eventarc client on this instance's Eventarc. Cloud Run's
// Triggers tab (consoleruntriggers.go) stays, as Google's does.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	eventarcpb "cloud.google.com/go/eventarc/apiv1/eventarcpb"
	"google.golang.org/api/iterator"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	runadapter "github.com/cloudburrow/cloudburrow/internal/adapter/run"
	"github.com/cloudburrow/cloudburrow/internal/console"
)

// eventarcProvider is Eventarc's Triggers page.
type eventarcProvider struct{ run runProvider }

func (eventarcProvider) ID() string    { return "eventarc" }
func (eventarcProvider) Title() string { return "Eventarc triggers" }

// eaEventType is one event type Create trigger offers, with Google's
// provider name.
type eaEventType struct{ typ, provider string }

// eaEventTypes are the event types this instance's Eventarc delivers.
var eaEventTypes = []eaEventType{
	{"google.cloud.pubsub.topic.v1.messagePublished", "Cloud Pub/Sub"},
	{"google.cloud.storage.object.v1.finalized", "Cloud Storage"},
	{"google.cloud.storage.object.v1.deleted", "Cloud Storage"},
	{"google.cloud.storage.object.v1.archived", "Cloud Storage"},
	{"google.cloud.storage.object.v1.metadataUpdated", "Cloud Storage"},
	{eaAuditLogType, "Cloud Audit Logs"},
	{"google.cloud.firestore.document.v1.created", "Cloud Firestore"},
	{"google.cloud.firestore.document.v1.updated", "Cloud Firestore"},
	{"google.cloud.firestore.document.v1.deleted", "Cloud Firestore"},
	{"google.cloud.firestore.document.v1.written", "Cloud Firestore"},
	{"google.cloud.firestore.document.v1.created.withAuthContext", "Cloud Firestore"},
	{"google.cloud.firestore.document.v1.updated.withAuthContext", "Cloud Firestore"},
	{"google.cloud.firestore.document.v1.deleted.withAuthContext", "Cloud Firestore"},
	{"google.cloud.firestore.document.v1.written.withAuthContext", "Cloud Firestore"},
}

// eaAuditLogType is Cloud Audit Logs' event type, whose provider is the
// service that wrote the entry.
const eaAuditLogType = "google.cloud.audit.log.v1.written"

// eaResourceChoices are Google's Resource select for an audit-log trigger.
var eaResourceChoices = []string{"Any resource", "Specific resource", "Path pattern"}

func eaProviderOf(typ string) string {
	for _, e := range eaEventTypes {
		if e.typ == typ {
			return e.provider
		}
	}
	return ""
}

// eaDestinations are the event destinations Create trigger offers, as
// Google's select names them.
var eaDestinations = []string{"Cloud Run", "Cloud Run functions", "HTTP endpoint"}

const eventarcNote = "Triggers deliver each event to their destination as a CloudEvent in binary content mode, over a Pub/Sub " +
	"push subscription Eventarc makes. Google sources' direct events of Cloud Pub/Sub and Cloud Storage, the Cloud Audit Logs (Admin Activity) entries of the calls this instance serves over gRPC, and Cloud Firestore's document events, are served; " +
	"a trigger on a channel delivers the custom or third-party events published to it (#1947). The columns follow Google's Triggers page (UNVERIFIED, re-read 2026-11-19: " +
	"https://docs.cloud.google.com/eventarc/standard/docs/overview names the page, not its columns)."

func (p eventarcProvider) project(project string) string { return p.run.projectOr(project) }

// triggers are the project's triggers in every location.
func (p eventarcProvider) triggers(ctx context.Context, project string) ([]*eventarcpb.Trigger, error) {
	c, err := p.run.eventarcClient(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.Close() }()
	var out []*eventarcpb.Trigger
	it := c.ListTriggers(ctx, &eventarcpb.ListTriggersRequest{Parent: "projects/" + p.project(project) + "/locations/-"})
	for {
		t, err := it.Next()
		if errors.Is(err, iterator.Done) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
}

// eaType is a trigger's event type, and its other filters.
func eaType(t *eventarcpb.Trigger) (string, []string) {
	typ, filters := "", []string{}
	for _, f := range t.GetEventFilters() {
		if f.GetAttribute() == "type" {
			typ = f.GetValue()
			continue
		}
		op := ""
		if f.GetOperator() != "" {
			op = " (" + f.GetOperator() + ")"
		}
		filters = append(filters, f.GetAttribute()+op+": "+f.GetValue())
	}
	return typ, filters
}

// eaDestination is a trigger's destination as Google's page shows it: its
// kind and what it names.
func eaDestination(d *eventarcpb.Destination) (kind, target, path string) {
	switch {
	case d.GetCloudRun() != nil:
		return "Cloud Run", d.GetCloudRun().GetService(), d.GetCloudRun().GetPath()
	case d.GetCloudFunction() != "":
		return "Cloud Run functions", lastSegment(d.GetCloudFunction()), ""
	case d.GetGke() != nil:
		return "Google Kubernetes Engine", d.GetGke().GetNamespace() + "/" + d.GetGke().GetService(), d.GetGke().GetPath()
	case d.GetWorkflow() != "":
		return "Workflows", lastSegment(d.GetWorkflow()), ""
	case d.GetHttpEndpoint() != nil:
		return "HTTP endpoint", d.GetHttpEndpoint().GetUri(), ""
	}
	return "", "", ""
}

func eaRegion(name string) string {
	parts := strings.Split(name, "/")
	if len(parts) > 3 {
		return parts[3]
	}
	return ""
}

func (p eventarcProvider) List(ctx context.Context, project string) (console.Listing, error) {
	l := console.Listing{NameColumn: "Name", Noun: "triggers", Note: eventarcNote, Items: []console.Resource{},
		Columns: []string{"Type", "Event provider", "Event type", "Destination", "Region", "Created"}}
	ts, err := p.triggers(ctx, project)
	if err != nil {
		l.Unavailable = "cannot read Eventarc triggers: " + consoleErr(err)
		return l, nil
	}
	for _, t := range ts {
		typ, _ := eaType(t)
		kind, target, _ := eaDestination(t.GetDestination())
		source := "Google sources"
		if t.GetChannel() != "" {
			source = "Third-party"
		}
		l.Items = append(l.Items, console.Resource{Name: lastSegment(t.GetName()), Status: "Active", Fields: map[string]string{
			"Type": source, "Event provider": orDash(eaProviderLabel(t)), "Event type": typ,
			"Destination": orDash(strings.TrimSpace(kind + ": " + target)), "Region": eaRegion(t.GetName()),
			"Created": stampAge(t.GetCreateTime())}})
	}
	l.Total = len(l.Items)
	return l, nil
}

// find resolves a row to its trigger, in whichever location it is.
func (p eventarcProvider) find(ctx context.Context, project, id string) (*eventarcpb.Trigger, error) {
	ts, err := p.triggers(ctx, project)
	if err != nil {
		return nil, err
	}
	for _, t := range ts {
		if lastSegment(t.GetName()) == id {
			return t, nil
		}
	}
	return nil, fmt.Errorf("trigger %q not found", id)
}

func eaEventTypeField() console.Field {
	types := make([]string, 0, len(eaEventTypes))
	labels := map[string]string{}
	for _, e := range eaEventTypes {
		types = append(types, e.typ)
		labels[e.typ] = e.provider + ": " + e.typ
	}
	return console.Field{Name: "eventType", Label: "Event type", Type: "select", Options: types, OptionLabels: labels,
		Default: types[0], Section: "Event provider", Help: "The event provider and its event type, as Google's Event provider and Event selects name them."}
}

// destinationFields are the Event destination section, prefilled from d.
func (p eventarcProvider) destinationFields(d *eventarcpb.Destination) []console.Field {
	kind, target, path := eaDestination(d)
	if kind == "" {
		kind = eaDestinations[0]
	}
	sec := "Event destination"
	return []console.Field{
		{Name: "destination", Label: "Event destination", Type: "select", Options: eaDestinations, Default: kind, Section: sec},
		{Name: "service", Label: "Select a Cloud Run service", Type: "text", Default: target, Section: sec,
			Pattern: `^[a-z]([a-z0-9\-]{0,47}[a-z0-9])?$`, Help: "Cloud Run: the service, in the trigger's region, the events are sent to."},
		{Name: "path", Label: "Service URL path", Type: "text", Default: path, Pattern: `^/.*$`, Section: sec,
			Help: "Optional. The path on the destination the events are sent to, beginning with /."},
		{Name: "function", Label: "Select a Cloud Run function", Type: "text", Default: lastSegment(d.GetCloudFunction()), Section: sec,
			Help: "Cloud Run functions: the function, in the trigger's region, the events are sent to."},
		{Name: "uri", Label: "Destination URI", Type: "text", Default: d.GetHttpEndpoint().GetUri(), Pattern: `^https?://.+$`, Section: sec,
			Help: "HTTP endpoint: the internal endpoint's URI, such as http://10.10.10.8:80/route; reached from this machine."},
		{Name: "networkAttachment", Label: "Network attachment", Type: "text", Default: d.GetNetworkConfig().GetNetworkAttachment(), Section: sec,
			Help: "HTTP endpoint: the network attachment, projects/P/regions/R/networkAttachments/NAME, as Google requires; recorded, not used."},
	}
}

func (p eventarcProvider) CreateForm() (string, []console.Field) {
	fields := []console.Field{
		{Name: "name", Label: "Trigger name", Type: "text", Required: true, Pattern: `^[a-z][a-z0-9\-]{0,62}$`, Section: "Trigger",
			Help: "1 to 63 lowercase letters, digits and hyphens, starting with a letter."},
		{Name: "triggerType", Label: "Trigger type", Type: "select", Options: []string{"Google sources", "Third-party"}, Default: "Google sources",
			Section: "Trigger", Help: "Third-party: the events published to a channel (#1947), your own custom events or a provider's."},
		{Name: "channel", Label: "Channel", Type: "text", Section: "Event provider",
			Help: "Third-party: the channel ID in the trigger's region, or projects/P/locations/L/channels/C."},
		{Name: "customEventType", Label: "Event type (third-party)", Type: "text", Section: "Event provider",
			Help: "Third-party: the CloudEvents type the trigger delivers, such as order.created."},
		eaEventTypeField(),
		{Name: "topic", Label: "Select a Cloud Pub/Sub topic", Type: "text", Section: "Event provider",
			Help: "Cloud Pub/Sub: the topic ID; empty to have Eventarc create one."},
		{Name: "bucket", Label: "Bucket", Type: "text", Pattern: `^[a-z0-9][a-z0-9._\-]{1,221}[a-z0-9]$`, Section: "Event provider",
			Help: "Cloud Storage: the bucket whose objects' events are delivered."},
		{Name: "serviceName", Label: "Service name", Type: "select", Options: append([]string{""}, runadapter.AuditServiceNames()...),
			Section: "Event provider", Help: "Cloud Audit Logs: the service whose Admin Activity audit log entries are delivered."},
		{Name: "methodName", Label: "Method name", Type: "text", Section: "Event provider",
			Help: "Cloud Audit Logs: the method, such as google.cloud.secretmanager.v1.SecretManagerService.CreateSecret."},
		{Name: "resource", Label: "Resource", Type: "select", Options: eaResourceChoices, Default: eaResourceChoices[0], Section: "Event provider",
			Help: "Cloud Audit Logs: every resource, one resource name, or a path pattern such as projects/_/secrets/{secret}."},
		{Name: "resourceName", Label: "Resource name or path pattern", Type: "text", Section: "Event provider"},
		{Name: "database", Label: "Database", Type: "text", Default: "(default)", Section: "Event provider",
			Help: "Cloud Firestore: the database whose documents' events are delivered."},
		{Name: "document", Label: "Document", Type: "text", Section: "Event provider",
			Help: "Cloud Firestore: optional. A document path, or a path pattern such as users/{userId}; empty for every document."},
		{Name: "documentMatch", Label: "Document match", Type: "select", Options: []string{"Path pattern", "Exact"}, Default: "Path pattern",
			Section: "Event provider", Help: "Cloud Firestore: whether Document is a path pattern (match-path-pattern) or one document."},
		{Name: "region", Label: "Region", Type: "select", Options: []string{p.run.location()}, Default: p.run.location(), Section: "Trigger"},
		{Name: "serviceAccount", Label: "Service account", Type: "text", Section: "Trigger",
			Help: "Optional. The account the delivery authenticates as; the default compute account when empty."},
	}
	fields = append(fields, p.destinationFields(nil)...)
	fields = append(fields, console.Field{Name: "labels", Label: "Labels", Type: "map", Section: "Labels",
		KeyPattern: `^[\p{Ll}\p{Lo}][\p{Ll}\p{Lo}\p{N}_\-]{0,62}$`, ValuePattern: `^[\p{Ll}\p{Lo}\p{N}_\-]{0,63}$`, MaxEntries: 64})
	return "Create trigger", fields
}

func (eventarcProvider) CreateOnPage() bool   { return true }
func (eventarcProvider) CreateTitle() string  { return "Create trigger" }
func (eventarcProvider) CreateSubmit() string { return "Create" }

// eaFormDestination is the destination the form names.
func eaFormDestination(project, region string, values map[string]string) (*eventarcpb.Destination, error) {
	path := strings.TrimSpace(values["path"])
	switch kind := strings.TrimSpace(values["destination"]); kind {
	case "", "Cloud Run":
		svc := strings.TrimSpace(values["service"])
		if svc == "" {
			return nil, errors.New("choose the Cloud Run service the events are sent to")
		}
		return &eventarcpb.Destination{Descriptor_: &eventarcpb.Destination_CloudRun{CloudRun: &eventarcpb.CloudRun{
			Service: svc, Region: region, Path: path}}}, nil
	case "Cloud Run functions":
		fn := strings.TrimSpace(values["function"])
		if fn == "" {
			return nil, errors.New("choose the Cloud Run function the events are sent to")
		}
		if !strings.HasPrefix(fn, "projects/") {
			fn = "projects/" + project + "/locations/" + region + "/functions/" + fn
		}
		return &eventarcpb.Destination{Descriptor_: &eventarcpb.Destination_CloudFunction{CloudFunction: fn}}, nil
	case "HTTP endpoint":
		uri, na := strings.TrimSpace(values["uri"]), strings.TrimSpace(values["networkAttachment"])
		if uri == "" || na == "" {
			return nil, errors.New("an HTTP endpoint destination needs its URI and network attachment")
		}
		return &eventarcpb.Destination{Descriptor_: &eventarcpb.Destination_HttpEndpoint{HttpEndpoint: &eventarcpb.HttpEndpoint{Uri: uri}},
			NetworkConfig: &eventarcpb.NetworkConfig{NetworkAttachment: na}}, nil
	default:
		return nil, fmt.Errorf("event destination %q is not served", kind)
	}
}

// eaFormTrigger is Create trigger's trigger.
func eaFormTrigger(project, region string, values map[string]string) (*eventarcpb.Trigger, error) {
	if values["triggerType"] == "Third-party" {
		return eaFormChannelTrigger(project, region, values)
	}
	typ := strings.TrimSpace(values["eventType"])
	if eaProviderOf(typ) == "" {
		return nil, fmt.Errorf("event type %q is not offered locally", typ)
	}
	t := &eventarcpb.Trigger{EventFilters: []*eventarcpb.EventFilter{{Attribute: "type", Value: typ}},
		ServiceAccount: strings.TrimSpace(values["serviceAccount"])}
	labels, err := console.ParseMap(values["labels"])
	if err != nil {
		return nil, err
	}
	if len(labels) > 0 {
		t.Labels = labels
	}
	if t.Destination, err = eaFormDestination(project, region, values); err != nil {
		return nil, err
	}
	topic, bucket := strings.TrimSpace(values["topic"]), strings.TrimSpace(values["bucket"])
	switch eaProviderOf(typ) {
	case "Cloud Pub/Sub":
		if bucket != "" {
			return nil, errors.New("a Cloud Pub/Sub trigger takes a topic, not a bucket")
		}
		if topic != "" {
			if !strings.HasPrefix(topic, "projects/") {
				topic = "projects/" + project + "/topics/" + topic
			}
			t.Transport = &eventarcpb.Transport{Intermediary: &eventarcpb.Transport_Pubsub{Pubsub: &eventarcpb.Pubsub{Topic: topic}}}
		}
	case "Cloud Storage":
		if bucket == "" {
			return nil, errors.New("a Cloud Storage trigger needs its bucket")
		}
		if topic != "" {
			return nil, errors.New("a Cloud Storage trigger takes a bucket, not a topic")
		}
		t.EventFilters = append(t.EventFilters, &eventarcpb.EventFilter{Attribute: "bucket", Value: bucket})
	case "Cloud Firestore":
		db := strings.TrimSpace(values["database"])
		if db == "" {
			db = "(default)"
		}
		t.EventFilters = append(t.EventFilters, &eventarcpb.EventFilter{Attribute: "database", Value: db})
		if doc := strings.TrimSpace(values["document"]); doc != "" {
			f := &eventarcpb.EventFilter{Attribute: "document", Value: doc}
			if values["documentMatch"] != "Exact" {
				f.Operator = "match-path-pattern"
			}
			t.EventFilters = append(t.EventFilters, f)
		}
	case "Cloud Audit Logs":
		svc, method := strings.TrimSpace(values["serviceName"]), strings.TrimSpace(values["methodName"])
		if svc == "" || method == "" {
			return nil, errors.New("a Cloud Audit Logs trigger needs its service name and method name")
		}
		t.EventFilters = append(t.EventFilters, &eventarcpb.EventFilter{Attribute: "serviceName", Value: svc},
			&eventarcpb.EventFilter{Attribute: "methodName", Value: method})
		res := strings.TrimSpace(values["resourceName"])
		switch values["resource"] {
		case "", "Any resource":
		case "Specific resource":
			t.EventFilters = append(t.EventFilters, &eventarcpb.EventFilter{Attribute: "resourceName", Value: res})
		case "Path pattern":
			t.EventFilters = append(t.EventFilters, &eventarcpb.EventFilter{Attribute: "resourceName", Value: res, Operator: "match-path-pattern"})
		default:
			return nil, fmt.Errorf("resource %q is not one of %v", values["resource"], eaResourceChoices)
		}
	}
	return t, nil
}

// eaFormChannelTrigger is a third-party trigger: a channel's events of
// one type (#1947).
func eaFormChannelTrigger(project, region string, values map[string]string) (*eventarcpb.Trigger, error) {
	ch := strings.TrimSpace(values["channel"])
	typ := strings.TrimSpace(values["customEventType"])
	if ch == "" || typ == "" {
		return nil, errors.New("a third-party trigger needs its channel and event type")
	}
	if !strings.HasPrefix(ch, "projects/") {
		ch = "projects/" + project + "/locations/" + region + "/channels/" + ch
	}
	t := &eventarcpb.Trigger{Channel: ch, EventFilters: []*eventarcpb.EventFilter{{Attribute: "type", Value: typ}},
		ServiceAccount: strings.TrimSpace(values["serviceAccount"])}
	labels, err := console.ParseMap(values["labels"])
	if err != nil {
		return nil, err
	}
	if len(labels) > 0 {
		t.Labels = labels
	}
	if t.Destination, err = eaFormDestination(project, region, values); err != nil {
		return nil, err
	}
	return t, nil
}

func (p eventarcProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	project = p.project(project)
	if project == "" {
		return "", errors.New("choose a project in the toolbar")
	}
	id := strings.TrimSpace(values["name"])
	region := strings.TrimSpace(values["region"])
	if region == "" {
		region = p.run.location()
	}
	if region != p.run.location() {
		return "", fmt.Errorf("region %q is not served: this instance's location is %s", region, p.run.location())
	}
	t, err := eaFormTrigger(project, region, values)
	if err != nil {
		return "", err
	}
	c, err := p.run.eventarcClient(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = c.Close() }()
	op, err := c.CreateTrigger(ctx, &eventarcpb.CreateTriggerRequest{Parent: "projects/" + project + "/locations/" + region,
		TriggerId: id, Trigger: t})
	if err != nil {
		return "", err
	}
	if _, err := op.Wait(ctx); err != nil {
		return "", err
	}
	return id, nil
}

func (p eventarcProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) > 1 {
		return console.DeeperThan(1, path), nil
	}
	t, err := p.find(ctx, project, path[0])
	if err != nil {
		return console.Detail{Unavailable: "cannot read the trigger: " + consoleErr(err)}, nil
	}
	typ, _ := eaType(t)
	kind, target, upath := eaDestination(t.GetDestination())
	labels := []string{}
	for k, v := range t.GetLabels() {
		labels = append(labels, k+": "+v)
	}
	sort.Strings(labels)
	retry := "-"
	if n := t.GetRetryPolicy().GetMaxAttempts(); n > 0 {
		retry = fmt.Sprintf("%d attempts", n)
	}
	props := []console.Property{
		{Label: "Trigger name", Value: lastSegment(t.GetName())},
		{Label: "Resource name", Value: t.GetName()},
		{Label: "Region", Value: eaRegion(t.GetName())},
		{Label: "Event provider", Value: orDash(eaProviderLabel(t))},
		{Label: "Event type", Value: typ},
		{Label: "Event data content type", Value: orDash(t.GetEventDataContentType())},
		{Label: "Event destination", Value: orDash(kind)},
		{Label: "Destination", Value: orDash(target)},
		{Label: "Service URL path", Value: orDash(upath)},
		{Label: "Service account", Value: orDash(t.GetServiceAccount())},
		{Label: "Transport topic", Value: orDash(t.GetTransport().GetPubsub().GetTopic())},
		{Label: "Subscription", Value: orDash(t.GetTransport().GetPubsub().GetSubscription())},
		{Label: "Channel", Value: orDash(t.GetChannel())},
		{Label: "Retry policy", Value: retry},
		{Label: "Labels", Value: orDash(strings.Join(labels, ", "))},
		{Label: "Created", Value: t.GetCreateTime().AsTime().Format("Jan 2, 2006, 3:04:05 PM UTC")},
		{Label: "Updated", Value: t.GetUpdateTime().AsTime().Format("Jan 2, 2006, 3:04:05 PM UTC")},
		{Label: "Etag", Value: orDash(t.GetEtag())},
	}
	filters := console.Listing{NameColumn: "Attribute", Columns: []string{"Operator", "Value"}, Noun: "event filters"}
	for _, f := range t.GetEventFilters() {
		filters.Items = append(filters.Items, console.Resource{Name: f.GetAttribute(), Fields: map[string]string{
			"Operator": orDash(f.GetOperator()), "Value": f.GetValue()}})
	}
	filters.Total = len(filters.Items)
	fields := []console.Field{
		{Name: "serviceAccount", Label: "Service account", Type: "text", Default: t.GetServiceAccount(), Section: "Trigger"},
	}
	fields = append(fields, p.destinationFields(t.GetDestination())...)
	fields = append(fields, console.Field{Name: "labels", Label: "Labels", Type: "map", Section: "Labels", Default: console.FormatMap(t.GetLabels()),
		KeyPattern: `^[\p{Ll}\p{Lo}][\p{Ll}\p{Lo}\p{N}_\-]{0,62}$`, ValuePattern: `^[\p{Ll}\p{Lo}\p{N}_\-]{0,63}$`, MaxEntries: 64})
	return console.Detail{Summary: props[:5], Sections: []console.Section{
		{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: []console.PropertyGroup{{Heading: "Trigger", Properties: props}}},
		{ID: "filters", Label: "Event filters", Listing: filters},
	}, Edit: &console.EditForm{Label: "Edit", Title: "Edit trigger", Submit: "Save", Fields: fields,
		Note: "The trigger's name, region, event provider, event type and filters cannot change, as on Google: delete and create the trigger."}}, nil
}

func (p eventarcProvider) Edit(ctx context.Context, project string, path []string, values map[string]string) error {
	if len(path) != 1 {
		return errors.New("only a trigger can be edited")
	}
	t, err := p.find(ctx, project, path[0])
	if err != nil {
		return err
	}
	dest, err := eaFormDestination(p.project(project), eaRegion(t.GetName()), values)
	if err != nil {
		return err
	}
	labels, err := console.ParseMap(values["labels"])
	if err != nil {
		return err
	}
	c, err := p.run.eventarcClient(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	op, err := c.UpdateTrigger(ctx, &eventarcpb.UpdateTriggerRequest{
		Trigger: &eventarcpb.Trigger{Name: t.GetName(), Destination: dest, ServiceAccount: strings.TrimSpace(values["serviceAccount"]),
			Labels: labels, Etag: t.GetEtag()},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"destination", "service_account", "labels"}}})
	if err != nil {
		return err
	}
	_, err = op.Wait(ctx)
	return err
}

func (p eventarcProvider) Delete(ctx context.Context, project, row string) error {
	t, err := p.find(ctx, project, row)
	if err != nil {
		return err
	}
	c, err := p.run.eventarcClient(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	op, err := c.DeleteTrigger(ctx, &eventarcpb.DeleteTriggerRequest{Name: t.GetName(), Etag: t.GetEtag()})
	if err != nil {
		return err
	}
	_, err = op.Wait(ctx)
	return err
}

var (
	_ console.PageCreator     = eventarcProvider{}
	_ console.CreateTitler    = eventarcProvider{}
	_ console.CreateSubmitter = eventarcProvider{}
	_ console.Driller         = eventarcProvider{}
	_ console.Editor          = eventarcProvider{}
	_ console.Deleter         = eventarcProvider{}
)

// eaProviderLabel is a trigger's event provider as Google's page shows it:
// for an audit-log trigger, the service whose entries it receives.
func eaProviderLabel(t *eventarcpb.Trigger) string {
	typ, _ := eaType(t)
	if typ == eaAuditLogType {
		for _, f := range t.GetEventFilters() {
			if f.GetAttribute() == "serviceName" {
				return "Cloud Audit Logs: " + f.GetValue()
			}
		}
	}
	return eaProviderOf(typ)
}
