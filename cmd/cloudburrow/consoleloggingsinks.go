package main

// The Log router screen (#1226), for a project's sinks and, since #1572, a
// folder's, organization's or billing account's, as Google's console Log
// Router shows the sinks of whichever resource the resource picker selects.
// The parent is chosen with the same Parent picker Log storage has (#1411)
// and travels in the URL as ?parent=; the folder's and organization's pages
// on the Resource hierarchy screen list their sinks and link here.
//
// The form carries what a folder or organization sink adds: include_children
// and intercept_children (#1395, #1521). intercept_children's documented
// rules (a folder or organization, include_children set, a project
// destination) are ConfigServiceV2's own INVALID_ARGUMENT, shown on the form
// as it came. The destination is chosen by its service, as Google's form
// chooses a sink service first, and the sink's exclusions are a name to
// filter map.

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/logging/apiv2/loggingpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/logging"
)

type loggingSinksProvider struct{ loggingAdminScreen }

func (loggingSinksProvider) ID() string    { return "logging-sinks" }
func (loggingSinksProvider) Title() string { return "Log router sinks" }

// ScopeKey implements console.Scoper: the Parent picker (#1572).
func (loggingSinksProvider) ScopeKey() string { return bucketScopeKey }

const sinkNote = "A sink delivers each matching entry: to a log bucket and a Pub/Sub topic at once, to a BigQuery dataset streamed in, to a Cloud Storage bucket in hourly files, and to another project's _Default bucket and sinks. A destination that is missing is a sink error, in the project's logging.googleapis.com/sink_error log. An entry that does not fit its BigQuery table's schema is written to the dataset's export_errors table (export_errors_YYYYMMDD for date-sharded tables)."

const sinkChildrenNote = "A folder's or organization's sink with include children routes the entries of every project and folder under it too. With intercept children as well, the entries it matches are kept out of the sinks of the resources under it, their _Default buckets included; its destination must then be a project."

// sinkLink is the Log router page of a sink of parent, for a link from
// another screen: the Resource hierarchy's folder and organization pages.
func sinkLink(name, parent string) string {
	return "/logs/sinks/" + url.PathEscape(name) + "?" + bucketScopeKey + "=" + url.QueryEscape(parent)
}

// sinkListLink is the Log router screen of parent.
func sinkListLink(parent string) string {
	return "/logs/sinks?" + bucketScopeKey + "=" + url.QueryEscape(parent)
}

// sinkOfParent checks a sink name against the screen's parent, so a
// hand-written request cannot reach another parent's sink from it.
func sinkOfParent(ctx context.Context, project, name string) error {
	parent, err := bucketParent(ctx, project)
	if err != nil {
		return err
	}
	id, ok := strings.CutPrefix(name, parent+"/sinks/")
	if !ok || id == "" || strings.Contains(id, "/") {
		return fmt.Errorf("%s is not one of %s's sinks", name, parent)
	}
	return nil
}

// hasChildren is whether a parent is a folder or organization, whose sinks
// take include_children and intercept_children.
func hasChildren(parent string) bool {
	return strings.HasPrefix(parent, "folders/") || strings.HasPrefix(parent, "organizations/")
}

// sinkDelivery is a sink's delivery status, for its row and its page.
func (s loggingAdminScreen) sinkDelivery(name string) (state, lastErr string) {
	if s.svc == nil || s.svc.api == nil {
		return "-", ""
	}
	st := s.svc.api.SinkStatus(name)
	if st == nil {
		return "Nothing delivered yet", ""
	}
	state = fmt.Sprintf("%d delivered", st.Delivered)
	if !st.LastDelivery.IsZero() {
		state += ", last " + st.LastDelivery.UTC().Format(time.RFC3339)
	}
	if st.Pending > 0 {
		state += fmt.Sprintf(", %d waiting for the hourly file", st.Pending)
	}
	if st.ErrorRows > 0 {
		state += fmt.Sprintf(", %d in the dataset's export_errors table", st.ErrorRows)
	}
	if st.LastError != "" {
		lastErr = st.LastErrorAt.UTC().Format(time.RFC3339) + " " + st.LastError
	}
	return state, lastErr
}

// sinksOf is parent's sinks, as ListSinks answers them.
func (s loggingAdminScreen) sinksOf(ctx context.Context, parent string) ([]*loggingpb.LogSink, error) {
	a, err := s.admin()
	if err != nil {
		return nil, err
	}
	if err := s.fault(ctx, loggingpb.ConfigServiceV2_ListSinks_FullMethodName, parent); err != nil {
		return nil, err
	}
	resp, err := a.ConfigServer().ListSinks(ctx, &loggingpb.ListSinksRequest{Parent: parent, PageSize: 1000})
	if err != nil {
		return nil, err
	}
	return resp.GetSinks(), nil
}

func (p loggingSinksProvider) List(ctx context.Context, project string) (console.Listing, error) {
	out := console.Listing{Columns: []string{"Destination", "Filter", "State", "Delivery", "Last error", "Updated"}, NameColumn: "Sink", Noun: "sinks", Note: sinkNote}
	if _, err := p.admin(); err != nil {
		return console.Listing{}, err
	}
	out.Scope = p.bucketScope(ctx, project)
	parent, err := bucketParent(ctx, project)
	if err != nil {
		out.Prompt = "Cloud Logging lists sinks per project, folder, organization or billing account. Choose a project in the toolbar, or a parent in the picker."
		return out, nil
	}
	children := hasChildren(parent)
	if children {
		out.Columns = []string{"Destination", "Filter", "Include children", "Intercept children", "State", "Delivery", "Last error", "Updated"}
		out.Note = sinkChildrenNote + " " + sinkNote
	}
	sinks, err := p.sinksOf(ctx, parent)
	if err != nil {
		return console.Listing{}, err
	}
	for _, s := range sinks {
		name := parent + "/sinks/" + s.GetName()
		delivery, lastErr := p.sinkDelivery(name)
		f := map[string]string{
			"Destination": s.GetDestination(), "Filter": orDash(s.GetFilter()), "State": enabledWord(s.GetDisabled()),
			"Delivery": delivery, "Last error": orDash(lastErr), "Updated": loggingTime(s.GetUpdateTime()),
		}
		if children {
			f["Include children"] = yesNo(s.GetIncludeChildren())
			f["Intercept children"] = yesNo(logging.SinkIntercepts(s))
		}
		out.Items = append(out.Items, console.Resource{Name: name, Fields: f})
	}
	out.Total = len(out.Items)
	out.RowsOpenable = true
	return out, nil
}

// sinkService is one choice of the form's sink service: its label, the
// destination's prefix, and what follows it.
type sinkService struct {
	label, prefix, rest string
}

var sinkServices = []sinkService{
	{"Cloud Logging bucket", "logging.googleapis.com/", "projects/[P]/locations/[L]/buckets/[B]"},
	{"BigQuery dataset", "bigquery.googleapis.com/", "projects/[P]/datasets/[D]"},
	{"Cloud Storage bucket", "storage.googleapis.com/", "[BUCKET]"},
	{"Pub/Sub topic", "pubsub.googleapis.com/", "projects/[P]/topics/[T]"},
	{"Other project", "logging.googleapis.com/", "projects/[P]"},
}

const otherProject = "Other project"

// sinkServiceOf is the service a stored destination is of.
func sinkServiceOf(dest string) string {
	if rest, ok := strings.CutPrefix(dest, "logging.googleapis.com/"); ok {
		if parts := strings.Split(rest, "/"); len(parts) == 2 && parts[0] == "projects" {
			return otherProject
		}
		return sinkServices[0].label
	}
	for _, s := range sinkServices {
		if strings.HasPrefix(dest, s.prefix) {
			return s.label
		}
	}
	return sinkServices[0].label
}

// sinkDestination is the destination a form names: the whole destination
// as the API takes it, or what follows the chosen service's prefix.
func sinkDestination(service, dest string) (string, error) {
	dest = strings.TrimSpace(dest)
	if strings.Contains(dest, ".googleapis.com/") {
		return dest, nil
	}
	if service == "" {
		service = sinkServices[0].label
	}
	for _, s := range sinkServices {
		if s.label != service {
			continue
		}
		switch service {
		case "Cloud Storage bucket":
			dest = strings.TrimSuffix(strings.TrimPrefix(dest, "gs://"), "/")
		case otherProject:
			if !strings.HasPrefix(dest, "projects/") {
				dest = "projects/" + dest
			}
		}
		return s.prefix + dest, nil
	}
	return "", fmt.Errorf("sink service %q is not one of the offered ones", service)
}

func sinkFields(s *loggingpb.LogSink) []console.Field {
	if s == nil {
		s = &loggingpb.LogSink{}
	}
	services := make([]string, 0, len(sinkServices))
	var help []string
	for _, sv := range sinkServices {
		services = append(services, sv.label)
		help = append(help, sv.label+": "+sv.rest)
	}
	exclusions := map[string]string{}
	for _, e := range s.GetExclusions() {
		exclusions[e.GetName()] = e.GetFilter()
	}
	return []console.Field{
		{Name: "name", Label: "Sink name", Type: "text", Required: true, Default: s.GetName(), Immutable: s.GetName() != "",
			Pattern: `^[A-Za-z0-9][A-Za-z0-9_\-.]{0,99}$`, Help: "Up to 100 letters, digits, _, - and ., starting with a letter or digit."},
		{Name: "description", Label: "Description", Type: "text", Default: s.GetDescription()},
		{Name: "service", Label: "Sink service", Type: "select", Options: services, Default: sinkServiceOf(s.GetDestination()), Section: "Sink destination",
			Help: "Where the sink routes entries. Other project is that project's _Default bucket and sinks; it is the one destination an intercepting sink takes."},
		{Name: "destination", Label: "Destination", Type: "text", Required: true, Default: s.GetDestination(), Section: "Sink destination",
			Help: "What follows the service's prefix (" + strings.Join(help, "; ") + "), or the whole destination, such as " +
				"logging.googleapis.com/projects/[P]. " + sinkNote},
		{Name: "filter", Label: "Inclusion filter", Type: "textarea", Default: s.GetFilter(), Section: "Logs to include",
			Help: "Optional; empty matches every entry."},
		{Name: "include_children", Label: "Include logs ingested by this resource and all child resources", Type: "checkbox",
			Default: strconv.FormatBool(s.GetIncludeChildren()), Section: "Logs to include",
			Help: "Folder and organization sinks only (include_children)."},
		{Name: "intercept_children", Label: "Intercept logs ingested by child resources", Type: "checkbox",
			Default: strconv.FormatBool(logging.SinkIntercepts(s)), Section: "Logs to include",
			Help: "Folder and organization sinks only (intercept_children). It requires include children and a project destination (Other project); the entries it matches are kept out of the sinks under it."},
		{Name: "exclusions", Label: "Exclusion filters", Type: "map", Default: console.FormatMap(exclusions), Section: "Logs to filter out",
			KeyPattern: `^[A-Za-z0-9][A-Za-z0-9_\-.]{0,99}$`, KeyHelp: "an exclusion name: up to 100 letters, digits, _, - and ., starting with a letter or digit",
			Help: "Each exclusion's name and its filter; an entry an exclusion matches is not routed by this sink."},
		{Name: "disabled", Label: "Disabled", Type: "checkbox", Default: strconv.FormatBool(s.GetDisabled())},
	}
}

func (loggingSinksProvider) CreateForm() (string, []console.Field) {
	return "Create sink", sinkFields(nil)
}

// CreateOnPage implements console.PageCreator: the form is grouped.
func (loggingSinksProvider) CreateOnPage() bool { return true }

// sinkFromForm builds the sink a form describes. old, on edit, is the
// stored sink, whose exclusions keep their order, description and state.
func sinkFromForm(id string, v map[string]string, old *loggingpb.LogSink) (*loggingpb.LogSink, error) {
	dest, err := sinkDestination(v["service"], v["destination"])
	if err != nil {
		return nil, err
	}
	ex, err := console.ParseMap(v["exclusions"])
	if err != nil {
		return nil, fmt.Errorf("exclusion filters: %w", err)
	}
	sk := &loggingpb.LogSink{Name: id, Destination: dest, Filter: v["filter"],
		Description: v["description"], Disabled: v["disabled"] == "true", IncludeChildren: v["include_children"] == "true"}
	for _, e := range old.GetExclusions() {
		if f, ok := ex[e.GetName()]; ok {
			sk.Exclusions = append(sk.Exclusions, &loggingpb.LogExclusion{Name: e.GetName(), Filter: f,
				Description: e.GetDescription(), Disabled: e.GetDisabled()})
			delete(ex, e.GetName())
		}
	}
	names := make([]string, 0, len(ex))
	for n := range ex {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		sk.Exclusions = append(sk.Exclusions, &loggingpb.LogExclusion{Name: n, Filter: ex[n]})
	}
	logging.SetSinkIntercept(sk, v["intercept_children"] == "true")
	return sk, nil
}

func (p loggingSinksProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	a, err := p.admin()
	if err != nil {
		return "", err
	}
	parent, err := bucketParent(ctx, project)
	if err != nil {
		return "", errors.New("choose a project, or a parent in the picker, before creating a sink")
	}
	sk, err := sinkFromForm(strings.TrimSpace(values["name"]), values, nil)
	if err != nil {
		return "", err
	}
	if err := p.fault(ctx, loggingpb.ConfigServiceV2_CreateSink_FullMethodName, parent); err != nil {
		return "", err
	}
	s, err := a.ConfigServer().CreateSink(ctx, &loggingpb.CreateSinkRequest{Parent: parent, Sink: sk})
	if err != nil {
		return "", err
	}
	return parent + "/sinks/" + s.GetName(), nil
}

func (p loggingSinksProvider) get(ctx context.Context, project, name string) (*loggingpb.LogSink, error) {
	a, err := p.admin()
	if err != nil {
		return nil, err
	}
	if err := sinkOfParent(ctx, project, name); err != nil {
		return nil, err
	}
	if err := p.fault(ctx, loggingpb.ConfigServiceV2_GetSink_FullMethodName, name); err != nil {
		return nil, err
	}
	return a.ConfigServer().GetSink(ctx, &loggingpb.GetSinkRequest{SinkName: name})
}

func (p loggingSinksProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) > 1 {
		return console.DeeperThan(1, path), nil
	}
	parent, err := bucketParent(ctx, project)
	if err != nil {
		return console.Detail{Prompt: "Choose a project in the toolbar, or a parent on the Log router screen."}, nil
	}
	s, err := p.get(ctx, project, path[0])
	if err != nil {
		return console.Detail{Unavailable: consoleErr(err)}, nil
	}
	delivery, lastErr := p.sinkDelivery(path[0])
	props := []console.Property{
		{Label: "Resource name", Value: path[0]},
		{Label: "Parent", Value: parent},
		{Label: "Destination", Value: s.GetDestination()},
		{Label: "Filter", Value: orDash(s.GetFilter())},
		{Label: "Description", Value: orDash(s.GetDescription())},
		{Label: "State", Value: enabledWord(s.GetDisabled())},
	}
	note := sinkNote
	if hasChildren(parent) {
		props = append(props, console.Property{Label: "Include children", Value: yesNo(s.GetIncludeChildren())},
			console.Property{Label: "Intercept children", Value: yesNo(logging.SinkIntercepts(s))})
		note = sinkChildrenNote + " " + sinkNote
	}
	props = append(props,
		console.Property{Label: "Writer identity", Value: orDash(s.GetWriterIdentity())},
		console.Property{Label: "Delivery", Value: delivery},
		console.Property{Label: "Last error", Value: orDash(lastErr)},
		console.Property{Label: "Created", Value: loggingTime(s.GetCreateTime())},
		console.Property{Label: "Updated", Value: loggingTime(s.GetUpdateTime())},
	)
	exclusions := console.Listing{Columns: []string{"Filter", "State", "Description"}, NameColumn: "Exclusion", Noun: "exclusions"}
	for _, e := range s.GetExclusions() {
		exclusions.Items = append(exclusions.Items, console.Resource{Name: e.GetName(), Fields: map[string]string{
			"Filter": e.GetFilter(), "State": enabledWord(e.GetDisabled()), "Description": orDash(e.GetDescription())}})
	}
	exclusions.Total = len(exclusions.Items)
	return console.Detail{
		Summary: []console.Property{{Label: "Destination", Value: s.GetDestination()}, {Label: "State", Value: enabledWord(s.GetDisabled())},
			{Label: "Delivery", Value: delivery}},
		Sections: []console.Section{
			{ID: "configuration", Label: "Configuration", Kind: console.KindProperties,
				Groups: []console.PropertyGroup{{Heading: "Sink", Properties: props}}, Note: note},
			{ID: "exclusions", Label: "Exclusions", Listing: exclusions},
		},
		Edit: &console.EditForm{Label: "Edit sink", Fields: sinkFields(s), Note: "The name is the sink's identity and cannot change."},
	}, nil
}

// sinkEditMask is every field the edit form sets.
var sinkEditMask = []string{"destination", "filter", "description", "disabled", "exclusions", "include_children", "intercept_children"}

func (p loggingSinksProvider) Edit(ctx context.Context, project string, path []string, values map[string]string) error {
	if len(path) != 1 {
		return errors.New("only a sink can be edited")
	}
	old, err := p.get(ctx, project, path[0])
	if err != nil {
		return err
	}
	sk, err := sinkFromForm(old.GetName(), values, old)
	if err != nil {
		return err
	}
	if err := p.fault(ctx, loggingpb.ConfigServiceV2_UpdateSink_FullMethodName, path[0]); err != nil {
		return err
	}
	a, err := p.admin()
	if err != nil {
		return err
	}
	_, err = a.ConfigServer().UpdateSink(ctx, &loggingpb.UpdateSinkRequest{SinkName: path[0], Sink: sk,
		UpdateMask: &fieldmaskpb.FieldMask{Paths: sinkEditMask}})
	return err
}

func (p loggingSinksProvider) Delete(ctx context.Context, project, name string) error {
	a, err := p.admin()
	if err != nil {
		return err
	}
	if err := sinkOfParent(ctx, project, name); err != nil {
		return err
	}
	if err := p.fault(ctx, loggingpb.ConfigServiceV2_DeleteSink_FullMethodName, name); err != nil {
		return err
	}
	_, err = a.ConfigServer().DeleteSink(ctx, &loggingpb.DeleteSinkRequest{SinkName: name})
	return err
}

// sinkRouterProperty is the Details tab's link to parent's Log router
// screen.
func sinkRouterProperty(parent string) console.Property {
	return console.Property{Label: "Log router", Value: "Sinks of " + parent, Link: sinkListLink(parent)}
}

// sinkSection is a folder's or organization's Log router tab on its
// Resource hierarchy page: its sinks, each opening its Log router page, and
// the Log router screen of that parent in the note's link.
func (s loggingAdminScreen) sinkSection(ctx context.Context, parent string) console.Section {
	sec := console.Section{ID: "log-router", Label: "Log router"}
	l := console.Listing{Columns: []string{"Destination", "Include children", "Intercept children", "State"}, NameColumn: "Sink", Noun: "sinks",
		Note: "Each sink opens on the Log router screen, where this parent's sinks are created, edited and deleted (the Log router link on the Details tab). " + sinkChildrenNote}
	sinks, err := s.sinksOf(ctx, parent)
	if err != nil {
		l.Unavailable = consoleErr(err)
		sec.Listing = l
		return sec
	}
	for _, sk := range sinks {
		name := parent + "/sinks/" + sk.GetName()
		l.Items = append(l.Items, console.Resource{Name: name, Link: sinkLink(name, parent), Fields: map[string]string{
			"Destination": sk.GetDestination(), "Include children": yesNo(sk.GetIncludeChildren()),
			"Intercept children": yesNo(logging.SinkIntercepts(sk)), "State": enabledWord(sk.GetDisabled())}})
	}
	l.Total = len(l.Items)
	sec.Listing = l
	return sec
}

var _ console.Scoper = loggingSinksProvider{}
var _ console.PageCreator = loggingSinksProvider{}
var _ console.Editor = loggingSinksProvider{}
var _ console.Deleter = loggingSinksProvider{}
var _ console.Driller = loggingSinksProvider{}
