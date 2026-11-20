package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	ahpb "cloud.google.com/go/bigquery/analyticshub/apiv1/analyticshubpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// Analytics Hub's console, the rest of Google's layout (#1989): Edit
// exchange and Edit listing (UpdateDataExchange, UpdateListing), with an
// Upload icon of each; a listing's own page, under its exchange's; Refresh
// and Delete subscription (RefreshSubscription, DeleteSubscription); the
// subscriber's view, My subscriptions (ListSubscriptions) beside the
// exchanges the project's organization offers (ListOrgDataExchanges); and a
// Permissions tab on an exchange and a listing (GetIamPolicy, SetIamPolicy,
// TestIamPermissions). Every change is a call on the service's own methods.

// hubSubscriptionsRow is the path of the subscriber's view, My
// subscriptions; an exchange's row is always "location/id".
const hubSubscriptionsRow = "subscriptions"

// hubMaxIcon is the icon's limit, "Max Size: 3.0MiB" of its base64.
const hubMaxIcon = 3 << 20 / 4 * 3

func checkboxOn(v string) bool { return v == "true" || v == "on" }

func boolString(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// hubDiscovery is the discovery type a form's checkbox names.
func hubDiscovery(public string) *ahpb.DiscoveryType {
	if checkboxOn(public) {
		return ahpb.DiscoveryType_DISCOVERY_TYPE_PUBLIC.Enum()
	}
	return ahpb.DiscoveryType_DISCOVERY_TYPE_PRIVATE.Enum()
}

// exchangeEditForm is Edit exchange, prefilled: everything but the ID and
// the region, which "cannot be changed after you create the exchange".
func exchangeEditForm(e *ahpb.DataExchange) *console.EditForm {
	return &console.EditForm{
		Label: "Edit exchange",
		Fields: []console.Field{
			{Name: "displayName", Label: "Display name", Type: "text", Required: true, Default: e.GetDisplayName(),
				Help: "Letters, numbers, underscores, dashes, spaces and ampersands, up to 63 bytes."},
			{Name: "primaryContact", Label: "Primary contact", Type: "text", Default: e.GetPrimaryContact(),
				Help: "An email address or a URL."},
			{Name: "description", Label: "Description", Type: "textarea", Default: e.GetDescription()},
			{Name: "documentation", Label: "Documentation", Type: "textarea", Default: e.GetDocumentation(),
				Help: "Markdown, up to 2,000 bytes."},
			{Name: "public", Label: "Make this exchange publicly discoverable", Type: "checkbox",
				Default: boolString(e.GetDiscoveryType() == ahpb.DiscoveryType_DISCOVERY_TYPE_PUBLIC)},
			{Name: "logEmail", Label: "Log the email of each subscriber who queries a linked dataset", Type: "checkbox",
				Default: boolString(e.GetLogLinkedDatasetQueryUserEmail()),
				Help:    "Once on, it cannot be turned off."},
		},
		Note: "The exchange ID and region cannot be changed. The icon is changed with Upload icon.",
	}
}

// exchangeUpdate is the UpdateDataExchange of an Edit exchange
// submission: the mask names each submitted field that changed; nil when
// none did.
func exchangeUpdate(cur *ahpb.DataExchange, v map[string]string) *ahpb.UpdateDataExchangeRequest {
	in := &ahpb.DataExchange{Name: cur.GetName()}
	var paths []string
	text := func(key, path, was string, set func(string)) {
		if s, ok := v[key]; ok && s != was {
			set(s)
			paths = append(paths, path)
		}
	}
	text("displayName", "display_name", cur.GetDisplayName(), func(s string) { in.DisplayName = s })
	text("primaryContact", "primary_contact", cur.GetPrimaryContact(), func(s string) { in.PrimaryContact = s })
	text("description", "description", cur.GetDescription(), func(s string) { in.Description = s })
	text("documentation", "documentation", cur.GetDocumentation(), func(s string) { in.Documentation = s })
	if s, ok := v["public"]; ok && checkboxOn(s) != (cur.GetDiscoveryType() == ahpb.DiscoveryType_DISCOVERY_TYPE_PUBLIC) {
		in.DiscoveryType = hubDiscovery(s)
		paths = append(paths, "discovery_type")
	}
	if s, ok := v["logEmail"]; ok && checkboxOn(s) != cur.GetLogLinkedDatasetQueryUserEmail() {
		on := checkboxOn(s)
		in.LogLinkedDatasetQueryUserEmail = &on
		paths = append(paths, "log_linked_dataset_query_user_email")
	}
	if len(paths) == 0 {
		return nil
	}
	return &ahpb.UpdateDataExchangeRequest{DataExchange: in, UpdateMask: &fieldmaskpb.FieldMask{Paths: paths}}
}

// listingCategories are the categories Edit listing offers, by their
// names (CATEGORY_COMMERCE, ...).
func listingCategories() []string {
	var out []string
	for v, n := range ahpb.Listing_Category_name {
		if v != 0 {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

func categoryNames(cs []ahpb.Listing_Category) string {
	var out []string
	for _, c := range cs {
		out = append(out, c.String())
	}
	return strings.Join(out, ", ")
}

func parseCategories(s string) ([]ahpb.Listing_Category, error) {
	var out []ahpb.Listing_Category
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == '\n' || r == ' ' }) {
		n := strings.ToUpper(strings.TrimSpace(f))
		if !strings.HasPrefix(n, "CATEGORY_") {
			n = "CATEGORY_" + n
		}
		v, ok := ahpb.Listing_Category_value[n]
		if !ok || v == 0 {
			return nil, fmt.Errorf("%q is not a listing category: give one of %s", f, strings.Join(listingCategories(), ", "))
		}
		out = append(out, ahpb.Listing_Category(v))
	}
	return out, nil
}

// selectedTables are a listing's selected tables, by table ID.
func selectedTables(l *ahpb.Listing) string {
	var out []string
	for _, r := range l.GetBigqueryDataset().GetSelectedResources() {
		if t := r.GetTable(); t != "" {
			out = append(out, lastSegment(t))
		}
	}
	return strings.Join(out, "\n")
}

// listingEditForm is Edit listing, prefilled: everything but the shared
// dataset ("You can modify all values except the listing's shared
// dataset").
func listingEditForm(l *ahpb.Listing) *console.EditForm {
	c := l.GetRestrictedExportConfig()
	return &console.EditForm{
		Label: "Edit listing",
		Fields: []console.Field{
			{Name: "displayName", Label: "Display name", Type: "text", Required: true, Default: l.GetDisplayName(), Section: "Listing details"},
			{Name: "description", Label: "Description", Type: "textarea", Default: l.GetDescription(), Section: "Listing details"},
			{Name: "documentation", Label: "Documentation", Type: "textarea", Default: l.GetDocumentation(), Section: "Listing details",
				Help: "Markdown, up to 2,000 bytes."},
			{Name: "categories", Label: "Categories", Type: "text", Default: categoryNames(l.GetCategories()), Section: "Listing details",
				Help: "Comma-separated, at most two: " + strings.Join(listingCategories(), ", ") + "."},
			{Name: "tables", Label: "Selected tables", Type: "textarea", Default: selectedTables(l), Section: "Listing details",
				Help: "One table ID of the shared dataset per line; empty shares every table."},
			{Name: "primaryContact", Label: "Primary contact", Type: "text", Default: l.GetPrimaryContact(), Section: "Contact"},
			{Name: "requestAccess", Label: "Request access", Type: "text", Default: l.GetRequestAccess(), Section: "Contact",
				Help: "An email address or a URL subscribers ask for access at."},
			{Name: "providerName", Label: "Data provider name", Type: "text", Default: l.GetDataProvider().GetName(), Section: "Contact"},
			{Name: "providerContact", Label: "Data provider contact", Type: "text", Default: l.GetDataProvider().GetPrimaryContact(), Section: "Contact"},
			{Name: "publisherName", Label: "Publisher name", Type: "text", Default: l.GetPublisher().GetName(), Section: "Contact"},
			{Name: "publisherContact", Label: "Publisher contact", Type: "text", Default: l.GetPublisher().GetPrimaryContact(), Section: "Contact"},
			{Name: "public", Label: "Make this listing publicly discoverable", Type: "checkbox", Section: "Discovery",
				Default: boolString(l.GetDiscoveryType() == ahpb.DiscoveryType_DISCOVERY_TYPE_PUBLIC)},
			{Name: "logEmail", Label: "Log the email of each subscriber who queries the linked dataset", Type: "checkbox", Section: "Discovery",
				Default: boolString(l.GetLogLinkedDatasetQueryUserEmail())},
			{Name: "restrict", Label: "Disable copy and export of shared data", Type: "checkbox", Section: "Data egress options",
				Default: boolString(c.GetEnabled())},
			{Name: "restrictQuery", Label: "Disable copy and export of query results", Type: "checkbox", Section: "Data egress options",
				Default: boolString(c.GetRestrictQueryResult()), Help: "Selects Disable copy and export of shared data too."},
		},
		Note: "The shared dataset cannot be changed: create another listing for another dataset. The icon is changed with Upload icon.",
	}
}

// listingUpdate is the UpdateListing of an Edit listing submission; nil
// when nothing changed.
func listingUpdate(cur *ahpb.Listing, v map[string]string) (*ahpb.UpdateListingRequest, error) {
	in := &ahpb.Listing{Name: cur.GetName()}
	var paths []string
	text := func(key, path, was string, set func(string)) {
		if s, ok := v[key]; ok && strings.TrimSpace(s) != was {
			set(strings.TrimSpace(s))
			paths = append(paths, path)
		}
	}
	text("displayName", "display_name", cur.GetDisplayName(), func(s string) { in.DisplayName = s })
	text("description", "description", cur.GetDescription(), func(s string) { in.Description = s })
	text("documentation", "documentation", cur.GetDocumentation(), func(s string) { in.Documentation = s })
	text("primaryContact", "primary_contact", cur.GetPrimaryContact(), func(s string) { in.PrimaryContact = s })
	text("requestAccess", "request_access", cur.GetRequestAccess(), func(s string) { in.RequestAccess = s })
	if s, ok := v["categories"]; ok && strings.TrimSpace(s) != categoryNames(cur.GetCategories()) {
		cs, err := parseCategories(s)
		if err != nil {
			return nil, err
		}
		in.Categories = cs
		paths = append(paths, "categories")
	}
	pair := func(nameKey, contactKey, path string, cur *ahpb.DataProvider, set func(name, contact string)) {
		name, nok := v[nameKey]
		contact, cok := v[contactKey]
		if !nok && !cok {
			return
		}
		if !nok {
			name = cur.GetName()
		}
		if !cok {
			contact = cur.GetPrimaryContact()
		}
		if strings.TrimSpace(name) != cur.GetName() || strings.TrimSpace(contact) != cur.GetPrimaryContact() {
			set(strings.TrimSpace(name), strings.TrimSpace(contact))
			paths = append(paths, path)
		}
	}
	pair("providerName", "providerContact", "data_provider", cur.GetDataProvider(), func(n, c string) {
		in.DataProvider = &ahpb.DataProvider{Name: n, PrimaryContact: c}
	})
	pub := &ahpb.DataProvider{Name: cur.GetPublisher().GetName(), PrimaryContact: cur.GetPublisher().GetPrimaryContact()}
	pair("publisherName", "publisherContact", "publisher", pub, func(n, c string) {
		in.Publisher = &ahpb.Publisher{Name: n, PrimaryContact: c}
	})
	if s, ok := v["tables"]; ok && strings.Join(strings.Fields(s), "\n") != selectedTables(cur) {
		ds := cur.GetBigqueryDataset().GetDataset()
		src := &ahpb.Listing_BigQueryDatasetSource{Dataset: ds}
		for _, t := range strings.Fields(s) {
			src.SelectedResources = append(src.SelectedResources, &ahpb.Listing_BigQueryDatasetSource_SelectedResource{
				Resource: &ahpb.Listing_BigQueryDatasetSource_SelectedResource_Table{Table: ds + "/tables/" + t}})
		}
		in.Source = &ahpb.Listing_BigqueryDataset{BigqueryDataset: src}
		paths = append(paths, "bigquery_dataset.selected_resources")
	}
	if s, ok := v["public"]; ok && checkboxOn(s) != (cur.GetDiscoveryType() == ahpb.DiscoveryType_DISCOVERY_TYPE_PUBLIC) {
		in.DiscoveryType = hubDiscovery(s)
		paths = append(paths, "discovery_type")
	}
	if s, ok := v["logEmail"]; ok && checkboxOn(s) != cur.GetLogLinkedDatasetQueryUserEmail() {
		on := checkboxOn(s)
		in.LogLinkedDatasetQueryUserEmail = &on
		paths = append(paths, "log_linked_dataset_query_user_email")
	}
	c := cur.GetRestrictedExportConfig()
	restrict, rok := v["restrict"]
	query, qok := v["restrictQuery"]
	if rok || qok {
		on, q := c.GetEnabled(), c.GetRestrictQueryResult()
		if rok {
			on = checkboxOn(restrict)
		}
		if qok {
			q = checkboxOn(query)
		}
		on = on || q
		if on != c.GetEnabled() || q != c.GetRestrictQueryResult() {
			in.RestrictedExportConfig = &ahpb.Listing_RestrictedExportConfig{Enabled: on, RestrictQueryResult: q}
			paths = append(paths, "restricted_export_config")
		}
	}
	if len(paths) == 0 {
		return nil, nil
	}
	return &ahpb.UpdateListingRequest{Listing: in, UpdateMask: &fieldmaskpb.FieldMask{Paths: paths}}, nil
}

// Edit is Edit exchange on an exchange's page and Edit listing on a
// listing's.
func (p analyticsHubProvider) Edit(ctx context.Context, project string, path []string, v map[string]string) error {
	a, err := p.api()
	if err != nil {
		return err
	}
	api := a.svc.API()
	switch len(path) {
	case 1:
		name, err := exchangeName(project, path[0])
		if err != nil {
			return err
		}
		cur, err := api.GetDataExchange(ctx, &ahpb.GetDataExchangeRequest{Name: name})
		if err != nil {
			return errors.New(consoleErr(err))
		}
		if req := exchangeUpdate(cur, v); req != nil {
			if _, err := api.UpdateDataExchange(ctx, req); err != nil {
				return errors.New(consoleErr(err))
			}
		}
		return nil
	case 2:
		name, err := listingName(project, path)
		if err != nil {
			return err
		}
		cur, err := api.GetListing(ctx, &ahpb.GetListingRequest{Name: name})
		if err != nil {
			return errors.New(consoleErr(err))
		}
		req, err := listingUpdate(cur, v)
		if err != nil {
			return err
		}
		if req != nil {
			if _, err := api.UpdateListing(ctx, req); err != nil {
				return errors.New(consoleErr(err))
			}
		}
		return nil
	}
	return errors.New("only an exchange or a listing can be edited")
}

// listingName is the full name of a listing's page, [exchange, listing].
func listingName(project string, path []string) (string, error) {
	if len(path) != 2 || path[1] == "" || strings.Contains(path[1], "/") {
		return "", fmt.Errorf("%q is not a listing", strings.Join(path, "/"))
	}
	ex, err := exchangeName(project, path[0])
	if err != nil {
		return "", err
	}
	return ex + "/listings/" + path[1], nil
}

// hubSubscriptionRows are subscriptions as the Subscriptions tabs list them.
func hubSubscriptionRows(subs []*ahpb.Subscription, withExchange bool) console.Listing {
	cols := []string{"Listing", "State", "Linked dataset", "Created", "Last modified"}
	if withExchange {
		cols = []string{"Listing", "Exchange", "Region", "State", "Linked dataset", "Last modified"}
	}
	out := console.Listing{Columns: cols, Noun: "subscriptions", AlwaysStatus: true}
	for _, s := range subs {
		linked := "—"
		for _, r := range s.GetLinkedResources() {
			linked = r.GetLinkedDataset()
		}
		if linked == "—" {
			for _, r := range s.GetLinkedDatasetMap() {
				linked = r.GetLinkedDataset()
			}
		}
		state := strings.TrimPrefix(s.GetState().String(), "STATE_")
		region := strings.Split(s.GetName(), "/")
		f := map[string]string{"Listing": lastSegment(s.GetListing()), "State": state, "Linked dataset": linked,
			"Created":       s.GetCreationTime().AsTime().UTC().Format("2006-01-02 15:04:05 UTC"),
			"Last modified": s.GetLastModifyTime().AsTime().UTC().Format("2006-01-02 15:04:05 UTC"),
			"Exchange":      orDash(s.GetDataExchange()), "Region": region[min(3, len(region)-1)]}
		if s.GetListing() == "" {
			f["Listing"] = "—"
		}
		if s.GetDataExchange() == "" && s.GetListing() != "" {
			f["Exchange"] = s.GetListing()[:strings.Index(s.GetListing(), "/listings/")]
		}
		out.Items = append(out.Items, console.Resource{Name: s.GetName(), Status: state, Fields: f})
	}
	out.Total = len(out.Items)
	return out
}

// mySubscriptions are the project's subscriptions in every region.
func (a *analyticsHubAPI) mySubscriptions(ctx context.Context, project string) ([]*ahpb.Subscription, error) {
	var out []*ahpb.Subscription
	for _, loc := range hubAllLocations() {
		resp, err := a.svc.API().ListSubscriptions(ctx, &ahpb.ListSubscriptionsRequest{Parent: "projects/" + project + "/locations/" + loc, PageSize: 1000})
		if err != nil {
			return nil, err
		}
		out = append(out, resp.GetSubscriptions()...)
	}
	return out, nil
}

// subscriptionsPage is the subscriber's view: My subscriptions, and the
// exchanges of the project's organization it can subscribe from.
func (p analyticsHubProvider) subscriptionsPage(ctx context.Context, project string) (console.Detail, error) {
	if project == "" {
		return console.Detail{Prompt: "Subscriptions are listed per project. Choose one in the toolbar."}, nil
	}
	a, err := p.api()
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	subs, err := a.mySubscriptions(ctx, project)
	if err != nil {
		return console.Detail{Unavailable: "cannot list the subscriptions: " + consoleErr(err)}, nil
	}
	mine := hubSubscriptionRows(subs, true)
	mine.Note = "The project's subscriptions: each made a linked dataset in this project. Refresh subscription brings a linked dataset up to date; Delete subscription deletes it."
	org := console.Listing{Columns: []string{"Display name", "Project", "Region", "Listings", "Discovery"}, NameColumn: "Exchange", Noun: "exchanges"}
	orgName := ""
	if p.svc != nil && p.svc.orgOf != nil {
		orgName = p.svc.orgOf(project)
	}
	if orgName == "" {
		org.Note = "This project is in no organization, so no organization's exchanges are listed. Exchanges shared with you are reached from their project."
	} else {
		for _, loc := range hubAllLocations() {
			resp, err := a.svc.API().ListOrgDataExchanges(ctx, &ahpb.ListOrgDataExchangesRequest{Organization: orgName + "/locations/" + loc, PageSize: 1000})
			if err != nil {
				return console.Detail{Unavailable: "cannot list the organization's exchanges: " + consoleErr(err)}, nil
			}
			for _, e := range resp.GetDataExchanges() {
				parts := strings.Split(e.GetName(), "/")
				org.Items = append(org.Items, console.Resource{Name: e.GetName(), Fields: map[string]string{
					"Display name": e.GetDisplayName(), "Project": parts[1], "Region": parts[3],
					"Listings": strconv.Itoa(int(e.GetListingCount())), "Discovery": discoveryName(e.GetDiscoveryType())}})
			}
		}
		org.Note = "The data exchanges of " + orgName + ", in every project of it."
	}
	org.Total = len(org.Items)
	return console.Detail{
		Heading: "My subscriptions",
		Title:   "Project " + project,
		Trail:   []console.Crumb{{Label: "Analytics Hub", Path: []string{}}, {Label: "My subscriptions"}},
		Sections: []console.Section{
			{ID: "subscriptions", Label: "My subscriptions", Listing: mine},
			{ID: "organization", Label: "Organization exchanges", Listing: org},
		},
	}, nil
}

// listingDetail is a listing's page.
func (p analyticsHubProvider) listingDetail(ctx context.Context, project string, path []string) (console.Detail, error) {
	a, err := p.api()
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	name, err := listingName(project, path)
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	api := a.svc.API()
	l, err := api.GetListing(ctx, &ahpb.GetListingRequest{Name: name})
	if err != nil {
		return console.Detail{Unavailable: "cannot read the listing: " + consoleErr(err)}, nil
	}
	subs, err := api.ListSharedResourceSubscriptions(ctx, &ahpb.ListSharedResourceSubscriptionsRequest{Resource: name, PageSize: 1000})
	if err != nil {
		return console.Detail{Unavailable: "cannot list the subscriptions: " + consoleErr(err)}, nil
	}
	egress := "Not restricted"
	if c := l.GetRestrictedExportConfig(); c.GetEnabled() {
		egress = "Copy and export of shared data disabled"
		if c.GetRestrictQueryResult() {
			egress += "; copy and export of query results disabled"
		}
	}
	tables := strings.ReplaceAll(selectedTables(l), "\n", ", ")
	if tables == "" {
		tables = "Every table"
	}
	return console.Detail{
		Title: l.GetDisplayName(),
		Edit:  listingEditForm(l),
		Summary: []console.Property{
			{Label: "Display name", Value: l.GetDisplayName()},
			{Label: "Shared dataset", Value: l.GetBigqueryDataset().GetDataset()},
			{Label: "State", Value: strings.TrimPrefix(l.GetState().String(), "STATE_")},
			{Label: "Data egress", Value: egress},
		},
		Sections: []console.Section{
			{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: []console.PropertyGroup{
				{Heading: "Listing details", Properties: []console.Property{
					{Label: "Resource name", Value: l.GetName()},
					{Label: "Description", Value: orDash(l.GetDescription())},
					{Label: "Documentation", Value: orDash(l.GetDocumentation())},
					{Label: "Categories", Value: orDash(categoryNames(l.GetCategories()))},
					{Label: "Selected tables", Value: tables},
					{Label: "Discovery", Value: discoveryName(l.GetDiscoveryType())},
				}},
				{Heading: "Contact", Properties: []console.Property{
					{Label: "Primary contact", Value: orDash(l.GetPrimaryContact())},
					{Label: "Request access", Value: orDash(l.GetRequestAccess())},
					{Label: "Data provider", Value: orDash(strings.TrimSpace(l.GetDataProvider().GetName() + " " + l.GetDataProvider().GetPrimaryContact()))},
					{Label: "Publisher", Value: orDash(strings.TrimSpace(l.GetPublisher().GetName() + " " + l.GetPublisher().GetPrimaryContact()))},
				}},
			}},
			{ID: "subscriptions", Label: "Subscriptions", Listing: hubSubscriptionRows(subs.GetSharedResourceSubscriptions(), false)},
			{ID: "permissions", Kind: console.KindPermissions},
		},
	}, nil
}

// hubSubscriptionActions are Refresh and Delete subscription of subs (names),
// and Revoke of the active ones when revoke is set.
func hubSubscriptionActions(subs []*ahpb.Subscription, revoke bool) []console.Action {
	var all, active []string
	for _, s := range subs {
		all = append(all, s.GetName())
		if s.GetState() == ahpb.Subscription_STATE_ACTIVE {
			active = append(active, s.GetName())
		}
	}
	var acts []console.Action
	if len(active) > 0 {
		acts = append(acts, console.Action{ID: "refresh", Label: "Refresh subscription", Fields: []console.Field{
			{Name: "subscription", Label: "Subscription", Type: "select", Required: true, Options: active,
				Help: "Brings the linked dataset up to date with the listing's shared dataset."}}})
		if revoke {
			acts = append(acts, console.Action{ID: "revoke", Label: "Revoke subscription", Destructive: true, Fields: []console.Field{
				{Name: "subscription", Label: "Subscription", Type: "select", Required: true, Options: active}}})
		}
	}
	if len(all) > 0 {
		acts = append(acts, console.Action{ID: "delete-subscription", Label: "Delete subscription", Destructive: true,
			Fields: []console.Field{{Name: "subscription", Label: "Subscription", Type: "select", Required: true, Options: all,
				Help: "Deleting a subscription deletes its linked dataset from the subscriber's project."}}})
	}
	return acts
}

var hubIconAction = console.Action{ID: "upload-icon", Label: "Upload icon", Fields: []console.Field{
	{Name: "icon", Label: "Icon", Type: console.FileFieldType, Required: true, Help: "An image, at most 3.0 MiB once base64-encoded."}}}

// hubSubscriptionAct performs Refresh or Delete subscription of name, which
// must be one of subs.
func hubSubscriptionAct(ctx context.Context, a *analyticsHubAPI, action, name string, subs []*ahpb.Subscription) error {
	found := false
	for _, s := range subs {
		found = found || s.GetName() == name
	}
	if !found {
		return fmt.Errorf("%s is not a subscription listed here", name)
	}
	var err error
	switch action {
	case "refresh":
		_, err = a.svc.API().RefreshSubscription(ctx, &ahpb.RefreshSubscriptionRequest{Name: name})
	case "delete-subscription":
		_, err = a.svc.API().DeleteSubscription(ctx, &ahpb.DeleteSubscriptionRequest{Name: name})
	case "revoke":
		_, err = a.svc.API().RevokeSubscription(ctx, &ahpb.RevokeSubscriptionRequest{Name: name})
	default:
		return fmt.Errorf("unknown action %q", action)
	}
	if err != nil {
		return errors.New(consoleErr(err))
	}
	return nil
}

// ActAtFile is Upload icon of an exchange or a listing.
func (p analyticsHubProvider) ActAtFile(ctx context.Context, project string, path []string, action string, _ map[string]string, file console.UploadedFile) (*console.Listing, error) {
	if action != hubIconAction.ID {
		return nil, fmt.Errorf("unknown action %q", action)
	}
	a, err := p.api()
	if err != nil {
		return nil, err
	}
	icon, err := io.ReadAll(io.LimitReader(file, hubMaxIcon+1))
	if err != nil {
		return nil, err
	}
	if len(icon) > hubMaxIcon {
		return nil, errors.New("the icon is larger than 3.0 MiB once base64-encoded")
	}
	mask := &fieldmaskpb.FieldMask{Paths: []string{"icon"}}
	switch len(path) {
	case 1:
		name, err := exchangeName(project, path[0])
		if err != nil {
			return nil, err
		}
		_, err = a.svc.API().UpdateDataExchange(ctx, &ahpb.UpdateDataExchangeRequest{DataExchange: &ahpb.DataExchange{Name: name, Icon: icon}, UpdateMask: mask})
		if err != nil {
			return nil, errors.New(consoleErr(err))
		}
	case 2:
		name, err := listingName(project, path)
		if err != nil {
			return nil, err
		}
		_, err = a.svc.API().UpdateListing(ctx, &ahpb.UpdateListingRequest{Listing: &ahpb.Listing{Name: name, Icon: icon}, UpdateMask: mask})
		if err != nil {
			return nil, errors.New(consoleErr(err))
		}
	default:
		return nil, errors.New("an icon is an exchange's or a listing's")
	}
	return nil, nil
}

// The Permissions tab of an exchange and of a listing: the service's own
// policy of each, which, with --iam-enforcement=on, the service checks
// (#1989).

func (p analyticsHubProvider) PolicyOn(_ context.Context, path []string) *console.PolicyTarget {
	if len(path) == 0 || len(path) > 2 || path[0] == hubSubscriptionsRow {
		return nil
	}
	t := &console.PolicyTarget{Link: compatibilityDoc + "bigquery-sharing-analytics-hub--googlecloudbigqueryanalyticshubv1",
		RoleHelp: roleHelp("Analytics Hub", "roles/analyticshub.subscriber"), Conditions: true}
	if p.svc != nil && p.svc.cfg.IAMEnforced() {
		t.Enforced = "This instance runs with --iam-enforcement=on: every call to analyticshub.googleapis.com is checked against " +
			"these bindings, the exchange's (for a listing) and the project's, as Google documents each method's analyticshub.* " +
			"permission. A request with no access token, and this console, act as the instance's own account, which holds every permission."
	}
	return t
}

func (p analyticsHubProvider) policyName(project string, path []string) (iamServer, string, error) {
	a, err := p.api()
	if err != nil {
		return nil, "", err
	}
	var name string
	switch len(path) {
	case 1:
		name, err = exchangeName(project, path[0])
	case 2:
		name, err = listingName(project, path)
	default:
		err = errors.New("only an exchange and a listing have a Permissions tab")
	}
	if err != nil {
		return nil, "", err
	}
	return a.svc.API(), name, nil
}

func (p analyticsHubProvider) GetPolicy(ctx context.Context, project string, path []string) (console.Policy, error) {
	api, name, err := p.policyName(project, path)
	if err != nil {
		return console.Policy{}, err
	}
	return getProtoPolicy(ctx, api, name)
}

func (p analyticsHubProvider) SetPolicy(ctx context.Context, project string, path []string, pol console.Policy) error {
	api, name, err := p.policyName(project, path)
	if err != nil {
		return err
	}
	return setProtoPolicy(ctx, api, name, pol)
}

func (p analyticsHubProvider) TestPermissions(ctx context.Context, project string, path []string, perms []string) ([]string, error) {
	api, name, err := p.policyName(project, path)
	if err != nil {
		return nil, err
	}
	return testProtoPermissions(ctx, api, name, perms)
}
