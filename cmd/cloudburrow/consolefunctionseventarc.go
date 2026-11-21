package main

// Cloud Run functions: Google's Eventarc trigger pane (#2049). Add trigger
// opens it, as on Google's Write a function and a function's Triggers tab
// (https://docs.cloud.google.com/run/docs/deploy-functions, read
// 2026-11-20): Trigger name; Trigger type, one of Google Sources, Custom
// and Third-party; Event provider; Event type; the event's filters;
// Region; Service account; Service URL path; and Save trigger.
//
// The pane is drawn from the served Eventarc API through its official
// client: the providers and their event types, with each event type's
// filtering attributes, from ListProviders, and the channels Custom and
// Third-party events arrive on from ListChannels. Save trigger is
// CreateTrigger, with the function's Cloud Run service as the
// destination, as Google's triggers for Cloud Run functions deliver to
// the service.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode"

	eventarcpb "cloud.google.com/go/eventarc/apiv1/eventarcpb"
	"google.golang.org/api/iterator"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

const (
	eventarcGoogleSources = "Google Sources"
	eventarcCustom        = "Custom"
	eventarcThirdParty    = "Third-party"

	eventarcPubSubProvider = "pubsub.googleapis.com"
	eventarcAuditEvent     = "google.cloud.audit.log.v1.written"

	// The pane's field names. Each provider's event type is
	// eventarcEventTypeField+provider, each of its filters
	// eventarcFilterField+provider+"."+attribute.
	eventarcEventTypeField = "eventType."
	eventarcFilterField    = "filter."
)

// eventarcPaneCatalog is what the pane offers: the providers and channels
// in the region.
type eventarcPaneCatalog struct {
	providers []*eventarcpb.Provider
	// custom are channels with no provider, thirdParty those with one.
	custom, thirdParty []*eventarcpb.Channel
	// err is why the catalog could not be read, "" when it was.
	err string
}

// eventarcCatalog reads the catalog through the official Eventarc client.
func (p functionsProvider) eventarcCatalog(ctx context.Context, project, region string) eventarcPaneCatalog {
	var cat eventarcPaneCatalog
	c, err := p.eventarcClient(ctx)
	if err != nil {
		cat.err = err.Error()
		return cat
	}
	defer func() { _ = c.Close() }()
	parent := "projects/" + project + "/locations/" + region
	pit := c.ListProviders(ctx, &eventarcpb.ListProvidersRequest{Parent: parent})
	for {
		pr, err := pit.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			cat.err = "cannot list Eventarc's event providers: " + err.Error()
			return cat
		}
		cat.providers = append(cat.providers, pr)
	}
	cit := c.ListChannels(ctx, &eventarcpb.ListChannelsRequest{Parent: parent})
	for {
		ch, err := cit.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			cat.err = "cannot list Eventarc's channels: " + err.Error()
			return cat
		}
		if ch.GetProvider() == "" {
			cat.custom = append(cat.custom, ch)
		} else {
			cat.thirdParty = append(cat.thirdParty, ch)
		}
	}
	return cat
}

// eventarcPaneFields is Google's Eventarc trigger pane, from the providers
// and channels the served Eventarc API lists in region.
func (p functionsProvider) eventarcPaneFields(ctx context.Context, project, region string) []console.Field {
	return eventarcPane(p.eventarcCatalog(ctx, project, region), region)
}

// eventarcAttrLabel is a filtering attribute as a label: methodName is
// "Method name".
func eventarcAttrLabel(attr string) string {
	var b strings.Builder
	for i, r := range attr {
		switch {
		case i == 0:
			b.WriteRune(unicode.ToUpper(r))
		case unicode.IsUpper(r):
			b.WriteRune(' ')
			b.WriteRune(unicode.ToLower(r))
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// eventarcFixedFilter reports a filter the pane sets itself: an audit-log
// event's serviceName is its provider's service.
func eventarcFixedFilter(eventType, attr string) bool {
	return eventType == eventarcAuditEvent && attr == "serviceName"
}

// eventarcChannelField is the Channel select of Custom or Third-party.
func eventarcChannelField(name, kind, region string, chans []*eventarcpb.Channel) console.Field {
	f := console.Field{Name: name, Label: "Channel", Type: "select", Required: true, Section: "Trigger",
		ShowWhen: "triggerType=" + kind, OptionLabels: map[string]string{}}
	for _, ch := range chans {
		f.Options = append(f.Options, ch.GetName())
		label := lastSegment(ch.GetName())
		if pr := ch.GetProvider(); pr != "" {
			label += " (" + lastSegment(pr) + ")"
		}
		f.OptionLabels[ch.GetName()] = label
	}
	if len(f.Options) == 0 {
		f.Options = []string{""}
		f.OptionLabels[""] = "No channel in " + region
	}
	if kind == eventarcCustom {
		f.Help = "A channel is a resource used as a pipeline to deliver custom events from producers to consumers; " +
			"create one on Eventarc's Channels page."
	} else {
		f.Help = "A channel of a non-Google provider that offers an Eventarc source."
	}
	return f
}

// eventarcPane is the pane's fields for a catalog, in Google's order.
func eventarcPane(cat eventarcPaneCatalog, region string) []console.Field {
	const sec = "Trigger"
	providers := append([]*eventarcpb.Provider(nil), cat.providers...)
	sort.Slice(providers, func(i, j int) bool {
		return providerLabel(providers[i]) < providerLabel(providers[j])
	})
	provider := console.Field{Name: "provider", Label: "Event provider", Type: "select", Required: true, Section: sec,
		ShowWhen: "triggerType=" + eventarcGoogleSources, OptionLabels: map[string]string{},
		Help: "The product that provides the event type that triggers the function."}
	if cat.err != "" {
		provider.Help += " " + cat.err
	}
	for _, pr := range providers {
		id := lastSegment(pr.GetName())
		provider.Options = append(provider.Options, id)
		provider.OptionLabels[id] = providerLabel(pr)
	}
	if containsString(provider.Options, eventarcPubSubProvider) {
		provider.Default = eventarcPubSubProvider
	} else if len(provider.Options) > 0 {
		provider.Default = provider.Options[0]
	}
	fields := []console.Field{
		{Name: "name", Label: "Trigger name", Type: "text", Required: true, Section: sec, Pattern: `^[a-z][a-z0-9\-]{0,62}$`,
			Help: "1 to 63 lowercase letters, digits and hyphens, starting with a letter."},
		{Name: "triggerType", Label: "Trigger type", Type: "select", Required: true, Section: sec,
			Options: []string{eventarcGoogleSources, eventarcCustom, eventarcThirdParty}, Default: eventarcGoogleSources,
			Help: "Google Sources: Pub/Sub, Cloud Storage, Firestore and the other Google event providers. Custom: events " +
				"your application code produces, through a channel. Third-party: a non-Google provider's events, through a channel."},
		provider,
	}
	var filters []console.Field
	for _, pr := range providers {
		id := lastSegment(pr.GetName())
		on := "triggerType=" + eventarcGoogleSources + "&provider=" + id
		et := console.Field{Name: eventarcEventTypeField + id, Label: "Event type", Type: "select", Required: true, Section: sec,
			ShowWhen: on, OptionLabels: map[string]string{}, Help: "The trigger's configuration varies by event type."}
		withAttr := map[string][]string{}
		var attrs []*eventarcpb.FilteringAttribute
		for _, t := range pr.GetEventTypes() {
			et.Options = append(et.Options, t.GetType())
			label := t.GetType()
			if t.GetType() == eventarcAuditEvent {
				label += " (via Cloud Audit Logs)"
			} else {
				label += " (Direct)"
			}
			et.OptionLabels[t.GetType()] = label
			for _, a := range t.GetFilteringAttributes() {
				if a.GetAttribute() == "type" || eventarcFixedFilter(t.GetType(), a.GetAttribute()) {
					continue
				}
				if _, seen := withAttr[a.GetAttribute()]; !seen {
					attrs = append(attrs, a)
				}
				withAttr[a.GetAttribute()] = append(withAttr[a.GetAttribute()], t.GetType())
			}
		}
		if len(et.Options) > 0 {
			et.Default = et.Options[0]
		}
		fields = append(fields, et)
		if id == eventarcPubSubProvider {
			fields = append(fields, console.Field{Name: "topic", Label: "Select a Cloud Pub/Sub topic", Type: "text", Section: sec,
				ShowWhen: on, Help: "A topic ID in this project, or projects/{project}/topics/{topic}; empty, Eventarc makes one."})
		}
		for _, a := range attrs {
			f := console.Field{Name: eventarcFilterField + id + "." + a.GetAttribute(), Label: eventarcAttrLabel(a.GetAttribute()),
				Type: "text", Required: a.GetRequired(), Section: sec,
				ShowWhen: on + "&" + et.Name + "=" + strings.Join(withAttr[a.GetAttribute()], "|"),
				Help:     a.GetDescription()}
			if !a.GetRequired() {
				f.Help = strings.TrimSpace("Optional. " + f.Help)
			}
			if a.GetPathPatternSupported() {
				f.Help += "; a path pattern such as projects/p/secrets/* matches as match-path-pattern"
			}
			filters = append(filters, f)
		}
	}
	fields = append(fields, filters...)
	fields = append(fields,
		eventarcChannelField("customChannel", eventarcCustom, region, cat.custom),
		console.Field{Name: "customEventType", Label: "Event type", Type: "text", Required: true, Section: sec,
			ShowWhen: "triggerType=" + eventarcCustom, Help: "The type of the custom events the channel delivers, such as com.example.order.created."},
		eventarcChannelField("thirdPartyChannel", eventarcThirdParty, region, cat.thirdParty),
		console.Field{Name: "thirdPartyEventType", Label: "Event type", Type: "text", Required: true, Section: sec,
			ShowWhen: "triggerType=" + eventarcThirdParty, Help: "One of the provider's event types."},
		console.Field{Name: "region", Label: "Region", Type: "select", Required: true, Section: sec, Options: []string{region}, Default: region,
			Help: "The trigger's location; the function is usually deployed in the same region."},
		console.Field{Name: "serviceAccount", Label: "Service account", Type: "text", Section: sec,
			Help: "The identity the trigger uses to invoke the function, with permission to invoke it; the Compute Engine default service account when empty."},
		console.Field{Name: "path", Label: "Service URL path", Type: "text", Section: sec,
			Help: "Optional. The relative path on the service that receives the events, such as /, /route, route or route/subroute."},
	)
	return fields
}

func providerLabel(pr *eventarcpb.Provider) string {
	if d := pr.GetDisplayName(); d != "" {
		return d
	}
	return lastSegment(pr.GetName())
}

func containsString(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// eventarcPaneTrigger is Save trigger's trigger, checked against the
// catalog: the event type must be one the provider lists, and each
// filter one of its filtering attributes.
func eventarcPaneTrigger(cat eventarcPaneCatalog, project, region, service string, v map[string]string) (*eventarcpb.Trigger, error) {
	val := func(k string) string { return strings.TrimSpace(v[k]) }
	if r := val("region"); r != "" && r != region {
		return nil, fmt.Errorf("region %q is not served: the trigger is made in %s", r, region)
	}
	if service == "" {
		return nil, errors.New("the trigger's destination needs the function's Cloud Run service")
	}
	t := &eventarcpb.Trigger{ServiceAccount: val("serviceAccount"),
		Destination: &eventarcpb.Destination{Descriptor_: &eventarcpb.Destination_CloudRun{CloudRun: &eventarcpb.CloudRun{
			Service: service, Region: region, Path: val("path")}}}}
	kind := val("triggerType")
	if kind == "" {
		kind = eventarcGoogleSources
	}
	switch kind {
	case eventarcCustom, eventarcThirdParty:
		chField, typeField, chans := "customChannel", "customEventType", cat.custom
		if kind == eventarcThirdParty {
			chField, typeField, chans = "thirdPartyChannel", "thirdPartyEventType", cat.thirdParty
		}
		ch, typ := val(chField), val(typeField)
		if ch == "" {
			return nil, fmt.Errorf("a %s trigger needs a channel", kind)
		}
		known := false
		for _, c := range chans {
			known = known || c.GetName() == ch
		}
		if !known {
			return nil, fmt.Errorf("channel %s is not a %s channel in %s", lastSegment(ch), kind, region)
		}
		if typ == "" {
			return nil, fmt.Errorf("a %s trigger needs its event type", kind)
		}
		t.Channel = ch
		t.EventFilters = []*eventarcpb.EventFilter{{Attribute: "type", Value: typ}}
		return t, nil
	case eventarcGoogleSources:
	default:
		return nil, fmt.Errorf("unknown trigger type %q", kind)
	}
	id := val("provider")
	var pr *eventarcpb.Provider
	for _, x := range cat.providers {
		if lastSegment(x.GetName()) == id {
			pr = x
		}
	}
	if pr == nil {
		return nil, fmt.Errorf("event provider %q is not offered in %s", id, region)
	}
	typ := val(eventarcEventTypeField + id)
	var et *eventarcpb.EventType
	for _, x := range pr.GetEventTypes() {
		if x.GetType() == typ {
			et = x
		}
	}
	if et == nil {
		return nil, fmt.Errorf("event type %q is not one of %s's", typ, providerLabel(pr))
	}
	t.EventFilters = []*eventarcpb.EventFilter{{Attribute: "type", Value: typ}}
	for _, a := range et.GetFilteringAttributes() {
		attr := a.GetAttribute()
		if attr == "type" {
			continue
		}
		value := val(eventarcFilterField + id + "." + attr)
		if eventarcFixedFilter(typ, attr) {
			value = id
		}
		if value == "" {
			if a.GetRequired() {
				return nil, fmt.Errorf("the %s event needs its %s", typ, eventarcAttrLabel(attr))
			}
			continue
		}
		f := &eventarcpb.EventFilter{Attribute: attr, Value: value}
		if a.GetPathPatternSupported() && strings.ContainsAny(value, "*{") {
			f.Operator = functionPathPatternOp
		}
		t.EventFilters = append(t.EventFilters, f)
	}
	if id == eventarcPubSubProvider {
		if topic := val("topic"); topic != "" {
			if !strings.HasPrefix(topic, "projects/") {
				topic = "projects/" + project + "/topics/" + topic
			}
			t.Transport = &eventarcpb.Transport{Intermediary: &eventarcpb.Transport_Pubsub{Pubsub: &eventarcpb.Pubsub{Topic: topic}}}
		}
	}
	return t, nil
}

// createEventarcTrigger is Save trigger: CreateTrigger in region, to the
// Cloud Run service, waited on. A function that has no service yet (its
// deploy has not run one) is the destination itself, as Eventarc's
// destination.cloud_function names a 2nd gen function.
func (p functionsProvider) createEventarcTrigger(ctx context.Context, project, region, service string, v map[string]string) error {
	return p.createEventarcTriggerTo(ctx, project, region, service, "", v)
}

func (p functionsProvider) createEventarcTriggerTo(ctx context.Context, project, region, service, function string, v map[string]string) error {
	id := strings.TrimSpace(v["name"])
	if id == "" {
		return errors.New("a trigger name is required")
	}
	cat := p.eventarcCatalog(ctx, project, region)
	if cat.err != "" {
		return errors.New(cat.err)
	}
	t, err := eventarcPaneTrigger(cat, project, region, service, v)
	if err != nil {
		return err
	}
	if function != "" {
		t.Destination = &eventarcpb.Destination{Descriptor_: &eventarcpb.Destination_CloudFunction{CloudFunction: function}}
	}
	c, err := p.eventarcClient(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	op, err := c.CreateTrigger(ctx, &eventarcpb.CreateTriggerRequest{Parent: "projects/" + project + "/locations/" + region,
		TriggerId: id, Trigger: t})
	if err != nil {
		return err
	}
	_, err = op.Wait(ctx)
	return err
}
