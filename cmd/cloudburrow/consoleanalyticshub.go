package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	ahpb "cloud.google.com/go/bigquery/analyticshub/apiv1/analyticshubpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/locations"
)

// analyticsHubProvider is BigQuery's Sharing (Analytics Hub) page (#1960),
// in Google's layout: the project's data exchanges, Create exchange; an
// exchange's page with its listings and their subscriptions, Create
// listing, Subscribe, Revoke subscription and Delete listing. Every change
// is a call on the service's own methods.
type analyticsHubProvider struct{ svc *analyticsHubSvc }

const analyticsHubNotStarted = "BigQuery sharing (Analytics Hub) has not started"

func (analyticsHubProvider) ID() string    { return "bigquery-sharing" }
func (analyticsHubProvider) Title() string { return "Analytics Hub" }

var analyticsHubColumns = []string{"Display name", "Region", "Listings", "Primary contact", "Discovery"}

// hubLocations are the regions an exchange's form offers: BigQuery's
// multi-regions and the regions Google lists first.
var hubLocations = []string{"us", "eu", "us-central1", "us-east1", "europe-west1", "asia-northeast1"}

func (p analyticsHubProvider) api() (*analyticsHubAPI, error) {
	if p.svc == nil || p.svc.API() == nil {
		return nil, errors.New(analyticsHubNotStarted)
	}
	return &analyticsHubAPI{p.svc}, nil
}

type analyticsHubAPI struct{ svc *analyticsHubSvc }

// exchanges are the project's exchanges in every location.
func (a *analyticsHubAPI) exchanges(ctx context.Context, project string) ([]*ahpb.DataExchange, error) {
	var out []*ahpb.DataExchange
	for _, loc := range hubAllLocations() {
		resp, err := a.svc.API().ListDataExchanges(ctx, &ahpb.ListDataExchangesRequest{Parent: "projects/" + project + "/locations/" + loc, PageSize: 1000})
		if err != nil {
			return nil, err
		}
		out = append(out, resp.GetDataExchanges()...)
	}
	return out, nil
}

func hubAllLocations() []string {
	return append([]string{"us", "eu"}, locations.Regions...)
}

func discoveryName(d ahpb.DiscoveryType) string {
	if d == ahpb.DiscoveryType_DISCOVERY_TYPE_PUBLIC {
		return "Public"
	}
	return "Private"
}

func (p analyticsHubProvider) List(ctx context.Context, project string) (console.Listing, error) {
	listing := console.Listing{Columns: analyticsHubColumns, NameColumn: "Exchange", Noun: "exchanges"}
	if project == "" {
		listing.Prompt = "Exchanges are listed per project. Choose one in the toolbar."
		return listing, nil
	}
	a, err := p.api()
	if err != nil {
		return console.Listing{}, err
	}
	exs, err := a.exchanges(ctx, project)
	if err != nil {
		return console.Listing{}, err
	}
	for _, e := range exs {
		parts := strings.Split(e.GetName(), "/")
		listing.Items = append(listing.Items, console.Resource{Name: parts[3] + "/" + parts[5], Fields: map[string]string{
			"Display name": e.GetDisplayName(), "Region": parts[3], "Listings": strconv.Itoa(int(e.GetListingCount())),
			"Primary contact": orDash(e.GetPrimaryContact()), "Discovery": discoveryName(e.GetDiscoveryType()),
		}})
	}
	listing.Total = len(listing.Items)
	listing.RowsOpenable = true
	return listing, nil
}

func (p analyticsHubProvider) CreateForm() (string, []console.Field) {
	return "Create exchange", []console.Field{
		{Name: "id", Label: "Exchange ID", Type: "text", Required: true, Pattern: `^[A-Za-z0-9_]{1,100}$`,
			Help: "Letters, numbers and underscores, up to 100 bytes."},
		{Name: "displayName", Label: "Display name", Type: "text", Required: true},
		{Name: "location", Label: "Region", Type: "select", Required: true, Options: hubLocations, Default: "us"},
		{Name: "primaryContact", Label: "Primary contact", Type: "text"},
		{Name: "description", Label: "Description", Type: "textarea"},
		{Name: "public", Label: "Make this exchange publicly discoverable", Type: "checkbox"},
	}
}

func (p analyticsHubProvider) Create(ctx context.Context, project string, v map[string]string) (string, error) {
	if project == "" {
		return "", errors.New("choose a project first")
	}
	a, err := p.api()
	if err != nil {
		return "", err
	}
	ex := &ahpb.DataExchange{DisplayName: v["displayName"], PrimaryContact: v["primaryContact"], Description: v["description"]}
	if v["public"] == "true" {
		ex.DiscoveryType = ahpb.DiscoveryType_DISCOVERY_TYPE_PUBLIC.Enum()
	}
	e, err := a.svc.API().CreateDataExchange(ctx, &ahpb.CreateDataExchangeRequest{Parent: "projects/" + project + "/locations/" + v["location"],
		DataExchangeId: v["id"], DataExchange: ex})
	if err != nil {
		return "", errors.New(consoleErr(err))
	}
	parts := strings.Split(e.GetName(), "/")
	return parts[3] + "/" + parts[5], nil
}

// exchangeName is the full name a row ("location/id") names.
func exchangeName(project, row string) (string, error) {
	loc, id, ok := strings.Cut(row, "/")
	if project == "" || !ok || strings.Contains(id, "/") {
		return "", fmt.Errorf("%q is not an exchange of this project", row)
	}
	return "projects/" + project + "/locations/" + loc + "/dataExchanges/" + id, nil
}

func (p analyticsHubProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) > 1 {
		return console.DeeperThan(1, path), nil
	}
	a, err := p.api()
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	name, err := exchangeName(project, path[0])
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	api := a.svc.API()
	e, err := api.GetDataExchange(ctx, &ahpb.GetDataExchangeRequest{Name: name})
	if err != nil {
		return console.Detail{Unavailable: "cannot read the exchange: " + consoleErr(err)}, nil
	}
	ls, err := api.ListListings(ctx, &ahpb.ListListingsRequest{Parent: name, PageSize: 1000})
	if err != nil {
		return console.Detail{Unavailable: "cannot list the listings: " + consoleErr(err)}, nil
	}
	listings := console.Listing{Columns: []string{"Display name", "Shared dataset", "Data egress", "Discovery", "Subscriptions"}, Noun: "listings"}
	for _, l := range ls.GetListings() {
		egress := "Not restricted"
		if c := l.GetRestrictedExportConfig(); c.GetEnabled() {
			egress = "Copy and export disabled"
			if c.GetRestrictQueryResult() {
				egress += ", query results too"
			}
		}
		subs, _ := api.ListSharedResourceSubscriptions(ctx, &ahpb.ListSharedResourceSubscriptionsRequest{Resource: l.GetName(), PageSize: 1000})
		listings.Items = append(listings.Items, console.Resource{Name: l.GetName()[strings.LastIndex(l.GetName(), "/")+1:], Fields: map[string]string{
			"Display name": l.GetDisplayName(), "Shared dataset": l.GetBigqueryDataset().GetDataset(), "Data egress": egress,
			"Discovery": discoveryName(l.GetDiscoveryType()), "Subscriptions": strconv.Itoa(len(subs.GetSharedResourceSubscriptions())),
		}})
	}
	listings.Total = len(listings.Items)
	subs, err := api.ListSharedResourceSubscriptions(ctx, &ahpb.ListSharedResourceSubscriptionsRequest{Resource: name, PageSize: 1000})
	if err != nil {
		return console.Detail{Unavailable: "cannot list the subscriptions: " + consoleErr(err)}, nil
	}
	subscriptions := console.Listing{Columns: []string{"Listing", "State", "Linked dataset", "Created"}, Noun: "subscriptions", AlwaysStatus: true}
	for _, s := range subs.GetSharedResourceSubscriptions() {
		linked := "—"
		for _, r := range s.GetLinkedResources() {
			linked = r.GetLinkedDataset()
		}
		state := strings.TrimPrefix(s.GetState().String(), "STATE_")
		subscriptions.Items = append(subscriptions.Items, console.Resource{Name: s.GetName(), Status: state, Fields: map[string]string{
			"Listing": s.GetListing()[strings.LastIndex(s.GetListing(), "/")+1:], "State": state, "Linked dataset": linked,
			"Created": s.GetCreationTime().AsTime().UTC().Format("2006-01-02 15:04:05 UTC")}})
	}
	subscriptions.Total = len(subscriptions.Items)
	return console.Detail{
		Summary: []console.Property{
			{Label: "Display name", Value: e.GetDisplayName()},
			{Label: "Region", Value: strings.Split(name, "/")[3]},
			{Label: "Primary contact", Value: orDash(e.GetPrimaryContact())},
			{Label: "Discovery", Value: discoveryName(e.GetDiscoveryType())},
		},
		Sections: []console.Section{
			{ID: "listings", Label: "Listings", Listing: listings},
			{ID: "subscriptions", Label: "Subscriptions", Listing: subscriptions},
			{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: []console.PropertyGroup{{Heading: "Exchange details", Properties: []console.Property{
				{Label: "Resource name", Value: e.GetName()},
				{Label: "Description", Value: orDash(e.GetDescription())},
				{Label: "Documentation", Value: orDash(e.GetDocumentation())},
			}}}},
		},
	}, nil
}

func (p analyticsHubProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	a, err := p.api()
	if err != nil || len(path) != 1 {
		return nil
	}
	name, err := exchangeName(project, path[0])
	if err != nil {
		return nil
	}
	acts := []console.Action{{ID: "create-listing", Label: "Create listing", Fields: []console.Field{
		{Name: "id", Label: "Listing ID", Type: "text", Required: true, Pattern: `^[A-Za-z0-9_]{1,100}$`},
		{Name: "displayName", Label: "Display name", Type: "text", Required: true},
		{Name: "dataset", Label: "Shared dataset", Type: "text", Required: true, Help: "projects/PROJECT/datasets/DATASET, in the exchange's region."},
		{Name: "description", Label: "Description", Type: "textarea"},
		{Name: "restrict", Label: "Disable copy and export of shared data", Type: "checkbox"},
		{Name: "restrictQuery", Label: "Disable copy and export of query results", Type: "checkbox"},
	}}}
	ls, err := a.svc.API().ListListings(ctx, &ahpb.ListListingsRequest{Parent: name, PageSize: 1000})
	if err == nil && len(ls.GetListings()) > 0 {
		var ids []string
		for _, l := range ls.GetListings() {
			ids = append(ids, l.GetName()[strings.LastIndex(l.GetName(), "/")+1:])
		}
		acts = append(acts,
			console.Action{ID: "subscribe", Label: "Subscribe", Fields: []console.Field{
				{Name: "listing", Label: "Listing", Type: "select", Required: true, Options: ids},
				{Name: "project", Label: "Project", Type: "text", Required: true, Default: project, Help: "The subscriber's project, where the linked dataset is made."},
				{Name: "dataset", Label: "Linked dataset name", Type: "text", Required: true, Pattern: `^[A-Za-z0-9_]{1,1024}$`},
			}},
			console.Action{ID: "delete-listing", Label: "Delete listing", Destructive: true, Fields: []console.Field{
				{Name: "listing", Label: "Listing", Type: "select", Required: true, Options: ids}}})
	}
	subs, err := a.svc.API().ListSharedResourceSubscriptions(ctx, &ahpb.ListSharedResourceSubscriptionsRequest{Resource: name, PageSize: 1000})
	if err == nil {
		var active []string
		for _, s := range subs.GetSharedResourceSubscriptions() {
			if s.GetState() == ahpb.Subscription_STATE_ACTIVE {
				active = append(active, s.GetName())
			}
		}
		if len(active) > 0 {
			acts = append(acts, console.Action{ID: "revoke", Label: "Revoke subscription", Destructive: true, Fields: []console.Field{
				{Name: "subscription", Label: "Subscription", Type: "select", Required: true, Options: active}}})
		}
	}
	return acts
}

func (p analyticsHubProvider) ActAt(ctx context.Context, project string, path []string, action string, v map[string]string) error {
	a, err := p.api()
	if err != nil {
		return err
	}
	if len(path) != 1 {
		return errors.New("these actions apply to an exchange")
	}
	name, err := exchangeName(project, path[0])
	if err != nil {
		return err
	}
	api := a.svc.API()
	switch action {
	case "create-listing":
		l := &ahpb.Listing{DisplayName: v["displayName"], Description: v["description"],
			Source: &ahpb.Listing_BigqueryDataset{BigqueryDataset: &ahpb.Listing_BigQueryDatasetSource{Dataset: strings.TrimSpace(v["dataset"])}}}
		if v["restrict"] == "true" || v["restrictQuery"] == "true" {
			l.RestrictedExportConfig = &ahpb.Listing_RestrictedExportConfig{Enabled: true, RestrictQueryResult: v["restrictQuery"] == "true"}
		}
		_, err = api.CreateListing(ctx, &ahpb.CreateListingRequest{Parent: name, ListingId: v["id"], Listing: l})
	case "subscribe":
		_, err = api.SubscribeListing(ctx, &ahpb.SubscribeListingRequest{Name: name + "/listings/" + v["listing"],
			Destination: &ahpb.SubscribeListingRequest_DestinationDataset{DestinationDataset: &ahpb.DestinationDataset{
				Location:         strings.Split(name, "/")[3],
				DatasetReference: &ahpb.DestinationDatasetReference{ProjectId: strings.TrimSpace(v["project"]), DatasetId: strings.TrimSpace(v["dataset"])}}}})
	case "delete-listing":
		_, err = api.DeleteListing(ctx, &ahpb.DeleteListingRequest{Name: name + "/listings/" + v["listing"]})
	case "revoke":
		s, gerr := api.GetSubscription(ctx, &ahpb.GetSubscriptionRequest{Name: v["subscription"]})
		if gerr != nil {
			return errors.New(consoleErr(gerr))
		}
		if !strings.HasPrefix(s.GetListing(), name+"/listings/") {
			return fmt.Errorf("%s is not a subscription of this exchange", v["subscription"])
		}
		_, err = api.RevokeSubscription(ctx, &ahpb.RevokeSubscriptionRequest{Name: v["subscription"]})
	default:
		return fmt.Errorf("unknown action %q", action)
	}
	if err != nil {
		return errors.New(consoleErr(err))
	}
	return nil
}

func (p analyticsHubProvider) Delete(ctx context.Context, project, row string) error {
	a, err := p.api()
	if err != nil {
		return err
	}
	name, err := exchangeName(project, row)
	if err != nil {
		return err
	}
	if _, err := a.svc.API().DeleteDataExchange(ctx, &ahpb.DeleteDataExchangeRequest{Name: name}); err != nil {
		return errors.New(consoleErr(err))
	}
	return nil
}
