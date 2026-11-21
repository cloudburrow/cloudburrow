package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	ahpb "cloud.google.com/go/bigquery/analyticshub/apiv1/analyticshubpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// cleanRoomsProvider is BigQuery sharing's Data clean rooms pages (#2101),
// in the layout of "Use data clean rooms" and "Query templates"
// (https://docs.cloud.google.com/bigquery/docs/data-clean-rooms,
// https://docs.cloud.google.com/bigquery/docs/query-templates, read
// 2026-11-21): the project's clean rooms and Create clean room (project,
// location, name, primary contact, description, Subscriber Email
// Logging); a clean room's page with its Details (Edit clean room
// details), its listings and Add data (Select dataset, Table/view name,
// the columns, the view name, an analysis rule, the data egress
// controls: "A view is created for your data and is added as a listing"),
// its Templates tab (Create Template, Edit template, Submit for review,
// Approve, which makes the table-valued function in the approver's
// dataset and lists it, Delete template), Subscribe, and its subscriptions.
// Every change is a call on the service's own methods, and the views and
// functions are made by queries of the instance's BigQuery.
type cleanRoomsProvider struct{ svc *analyticsHubSvc }

func (cleanRoomsProvider) ID() string    { return "bigquery-clean-rooms" }
func (cleanRoomsProvider) Title() string { return "Data clean rooms" }

func (p cleanRoomsProvider) hub() analyticsHubProvider { return analyticsHubProvider(p) }

// cleanRooms are the project's clean rooms in every location.
func (p cleanRoomsProvider) cleanRooms(ctx context.Context, project string) ([]*ahpb.DataExchange, error) {
	a, err := p.hub().api()
	if err != nil {
		return nil, err
	}
	exs, err := a.exchanges(ctx, project)
	if err != nil {
		return nil, err
	}
	var out []*ahpb.DataExchange
	for _, e := range exs {
		if e.GetSharingEnvironmentConfig().GetDcrExchangeConfig() != nil {
			out = append(out, e)
		}
	}
	return out, nil
}

func (p cleanRoomsProvider) List(ctx context.Context, project string) (console.Listing, error) {
	listing := console.Listing{Columns: []string{"Display name", "Location", "Shared resources", "Primary contact"}, NameColumn: "Clean room", Noun: "clean rooms"}
	if project == "" {
		listing.Prompt = "Clean rooms are listed per project. Choose one in the toolbar."
		return listing, nil
	}
	rooms, err := p.cleanRooms(ctx, project)
	if err != nil {
		return console.Listing{}, err
	}
	for _, e := range rooms {
		parts := strings.Split(e.GetName(), "/")
		listing.Items = append(listing.Items, console.Resource{Name: parts[3] + "/" + parts[5], Fields: map[string]string{
			"Display name": e.GetDisplayName(), "Location": parts[3], "Shared resources": strconv.Itoa(int(e.GetListingCount())),
			"Primary contact": orDash(e.GetPrimaryContact()),
		}})
	}
	listing.Total = len(listing.Items)
	listing.RowsOpenable = true
	return listing, nil
}

func (p cleanRoomsProvider) CreateForm() (string, []console.Field) {
	return "Create clean room", []console.Field{
		{Name: "id", Label: "Clean room ID", Type: "text", Required: true, Pattern: `^[A-Za-z0-9_]{1,100}$`,
			Help: "Letters, numbers and underscores, up to 100 bytes. Subscribers' linked datasets are named after it."},
		{Name: "displayName", Label: "Name", Type: "text", Required: true},
		{Name: "location", Label: "Location", Type: "select", Required: true, Options: hubLocations, Default: "us",
			Help: "You can only list resources in the data clean room that are in the same region as the data clean room."},
		{Name: "primaryContact", Label: "Primary contact", Type: "text"},
		{Name: "description", Label: "Description", Type: "textarea"},
		{Name: "logEmail", Label: "Subscriber Email Logging", Type: "checkbox",
			Help: "Once you enable and save email logging, you can't edit this setting."},
	}
}

func (p cleanRoomsProvider) Create(ctx context.Context, project string, v map[string]string) (string, error) {
	if project == "" {
		return "", errors.New("choose a project first")
	}
	a, err := p.hub().api()
	if err != nil {
		return "", err
	}
	ex := &ahpb.DataExchange{DisplayName: v["displayName"], PrimaryContact: v["primaryContact"], Description: v["description"],
		SharingEnvironmentConfig: &ahpb.SharingEnvironmentConfig{Environment: &ahpb.SharingEnvironmentConfig_DcrExchangeConfig_{
			DcrExchangeConfig: &ahpb.SharingEnvironmentConfig_DcrExchangeConfig{}}}}
	if checkboxOn(v["logEmail"]) {
		on := true
		ex.LogLinkedDatasetQueryUserEmail = &on
	}
	e, err := a.svc.API().CreateDataExchange(ctx, &ahpb.CreateDataExchangeRequest{Parent: "projects/" + project + "/locations/" + v["location"],
		DataExchangeId: v["id"], DataExchange: ex})
	if err != nil {
		return "", errors.New(consoleErr(err))
	}
	parts := strings.Split(e.GetName(), "/")
	return parts[3] + "/" + parts[5], nil
}

// cleanRoom reads a clean room's page row ("location/id").
func (p cleanRoomsProvider) cleanRoom(ctx context.Context, project string, path []string) (*analyticsHubAPI, *ahpb.DataExchange, error) {
	if len(path) != 1 {
		return nil, nil, fmt.Errorf("%q is not a clean room", strings.Join(path, "/"))
	}
	a, err := p.hub().api()
	if err != nil {
		return nil, nil, err
	}
	name, err := exchangeName(project, path[0])
	if err != nil {
		return nil, nil, err
	}
	e, err := a.svc.API().GetDataExchange(ctx, &ahpb.GetDataExchangeRequest{Name: name})
	if err != nil {
		return nil, nil, errors.New(consoleErr(err))
	}
	if e.GetSharingEnvironmentConfig().GetDcrExchangeConfig() == nil {
		return nil, nil, fmt.Errorf("%s is a data exchange, not a clean room: it is on the Analytics Hub page", path[0])
	}
	return a, e, nil
}

// templateStatus is a template's state as the Templates tab names it.
func templateStatus(s ahpb.QueryTemplate_State) string {
	switch s {
	case ahpb.QueryTemplate_DRAFTED:
		return "Draft"
	case ahpb.QueryTemplate_PENDING:
		return "Requires review"
	case ahpb.QueryTemplate_APPROVED:
		return "Approved"
	}
	return s.String()
}

func (p cleanRoomsProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) > 1 {
		return console.DeeperThan(1, path), nil
	}
	a, e, err := p.cleanRoom(ctx, project, path)
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	api := a.svc.API()
	ls, err := api.ListListings(ctx, &ahpb.ListListingsRequest{Parent: e.GetName(), PageSize: 1000})
	if err != nil {
		return console.Detail{Unavailable: "cannot list the listings: " + consoleErr(err)}, nil
	}
	listings := console.Listing{Columns: []string{"Display name", "Shared resource", "Type", "Data egress", "Primary contact"}, Noun: "listings"}
	for _, l := range ls.GetListings() {
		res, kind := "-", "Table or view"
		if rs := l.GetBigqueryDataset().GetSelectedResources(); len(rs) == 1 {
			res = rs[0].GetTable()
			if rs[0].GetRoutine() != "" {
				res, kind = rs[0].GetRoutine(), "Table-valued function"
			}
		}
		egress := "Copy and export of shared data disabled"
		if l.GetRestrictedExportConfig().GetRestrictQueryResult() {
			egress += ", query results too"
		}
		listings.Items = append(listings.Items, console.Resource{Name: lastSegment(l.GetName()), Fields: map[string]string{
			"Display name": l.GetDisplayName(), "Shared resource": res, "Type": kind, "Data egress": egress, "Primary contact": orDash(l.GetPrimaryContact()),
		}})
	}
	listings.Total = len(listings.Items)
	ts, err := api.ListQueryTemplates(ctx, &ahpb.ListQueryTemplatesRequest{Parent: e.GetName(), PageSize: 1000})
	if err != nil {
		return console.Detail{Unavailable: "cannot list the query templates: " + consoleErr(err)}, nil
	}
	templates := console.Listing{Columns: []string{"Display name", "Approval Status", "Description", "Primary contact", "Last modified"}, Noun: "templates"}
	for _, t := range ts.GetQueryTemplates() {
		templates.Items = append(templates.Items, console.Resource{Name: lastSegment(t.GetName()), Fields: map[string]string{
			"Display name": t.GetDisplayName(), "Approval Status": templateStatus(t.GetState()), "Description": orDash(t.GetDescription()),
			"Primary contact": orDash(t.GetPrimaryContact()), "Last modified": t.GetUpdateTime().AsTime().UTC().Format(time.RFC3339),
		}})
	}
	templates.Total = len(templates.Items)
	subs, err := api.ListSharedResourceSubscriptions(ctx, &ahpb.ListSharedResourceSubscriptionsRequest{Resource: e.GetName(), PageSize: 1000})
	if err != nil {
		return console.Detail{Unavailable: "cannot list the subscriptions: " + consoleErr(err)}, nil
	}
	return console.Detail{
		Edit: exchangeEditForm(e),
		Summary: []console.Property{
			{Label: "Name", Value: e.GetDisplayName()},
			{Label: "Location", Value: strings.Split(e.GetName(), "/")[3]},
			{Label: "Primary contact", Value: orDash(e.GetPrimaryContact())},
			{Label: "Shared resources", Value: strconv.Itoa(len(ls.GetListings())) + " of 100"},
		},
		Sections: []console.Section{
			{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: []console.PropertyGroup{{Heading: "Clean room details", Properties: []console.Property{
				{Label: "Resource name", Value: e.GetName()},
				{Label: "Description", Value: orDash(e.GetDescription())},
				{Label: "Subscriber Email Logging", Value: map[bool]string{true: "On", false: "Off"}[e.GetLogLinkedDatasetQueryUserEmail()]},
				{Label: "Linked datasets", Value: "One per subscription, holding every listing's resource"},
				{Label: "Data egress", Value: "Subscribers can't copy or export the shared data"},
			}}}},
			{ID: "listings", Label: "Listings", Listing: listings},
			{ID: "templates", Label: "Templates", Listing: templates},
			{ID: "subscriptions", Label: "Subscriptions", Listing: hubSubscriptionRows(subs.GetSharedResourceSubscriptions(), false)},
		},
	}, nil
}

// analysisRules are Add data's analysis rule choices.
var analysisRules = []string{"Aggregation threshold", "Differential privacy", "List overlap", "None"}

func (p cleanRoomsProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	a, e, err := p.cleanRoom(ctx, project, path)
	if err != nil {
		return nil
	}
	api := a.svc.API()
	acts := []console.Action{
		{ID: "add-data", Label: "Add data", Title: "Add data", Fields: []console.Field{
			{Name: "dataset", Label: "Select dataset", Type: "text", Required: true, Default: "projects/" + project + "/datasets/",
				Help: "projects/PROJECT/datasets/DATASET, in the clean room's location. The view is made in it."},
			{Name: "table", Label: "Table/view name", Type: "text", Required: true, Pattern: `^[A-Za-z0-9_]{1,1024}$`},
			{Name: "columns", Label: "Columns", Type: "text", Help: "The columns to share, separated by commas; empty shares every column."},
			{Name: "view", Label: "View name", Type: "text", Required: true, Pattern: `^[A-Za-z0-9_]{1,100}$`,
				Help: "A view is created for your data and is added as a listing. The source table or view itself isn't added."},
			{Name: "primaryContact", Label: "Primary contact", Type: "text"},
			{Name: "description", Label: "Description", Type: "textarea"},
			{Name: "rule", Label: "Analysis rule", Type: "select", Required: true, Options: analysisRules, Default: analysisRules[0],
				Help: "Choose an analysis rule for your listing. With None, subscribers can access raw data for that resource."},
			{Name: "privacyUnit", Label: "Privacy unit column", Type: "text", Help: "Aggregation threshold and differential privacy."},
			{Name: "threshold", Label: "Aggregation threshold", Type: "text", Pattern: `^[0-9]*$`, Default: "50"},
			{Name: "epsilon", Label: "Maximum epsilon per query", Type: "text", Default: "10", Help: "Differential privacy."},
			{Name: "epsilonBudget", Label: "Epsilon budget", Type: "text", Default: "100"},
			{Name: "delta", Label: "Delta per query", Type: "text", Default: "0.00001"},
			{Name: "deltaBudget", Label: "Delta budget", Type: "text", Default: "0.001"},
			{Name: "joinColumns", Label: "Join allowed columns", Type: "text", Help: "List overlap, separated by commas."},
			{Name: "joinCondition", Label: "Join condition", Type: "select", Options: []string{"JOIN_ANY", "JOIN_ALL"}, Default: "JOIN_ANY"},
			{Name: "restrictQuery", Label: "Disable copy and export of query results", Type: "checkbox",
				Help: "Copy and export of the shared data is always disabled. These controls can't be changed after the listing is created."},
		}},
		{ID: "create-template", Label: "Create Template", Fields: []console.Field{
			{Name: "id", Label: "Template ID", Type: "text", Required: true, Pattern: `^[A-Za-z0-9_]{1,100}$`},
			{Name: "displayName", Label: "Display name", Type: "text", Required: true, Help: "It can't be changed after creation."},
			{Name: "description", Label: "Description", Type: "textarea"},
			{Name: "primaryContact", Label: "Primary contact", Type: "text"},
			{Name: "definition", Label: "Query", Type: "textarea", Required: true,
				Help: "QUERY_TEMPLATE_ID(TVF arguments) AS (TVF_DEFINITION), for example by_year(y INT64) AS (SELECT ... WHERE year = y)"},
		}},
		{ID: "subscribe", Label: "Subscribe", Fields: []console.Field{
			{Name: "project", Label: "Destination project", Type: "text", Required: true, Default: project},
			{Name: "subscription", Label: "Subscription name", Type: "text", Required: true, Pattern: `^[A-Za-z0-9_\-]{1,100}$`},
			{Name: "dataset", Label: "Linked dataset name", Type: "text", Pattern: `^[A-Za-z0-9_]{0,1024}$`, Default: lastSegment(e.GetName()),
				Help: "It has the same name as the data clean room unless you name another."},
		}},
	}
	ls, err := api.ListListings(ctx, &ahpb.ListListingsRequest{Parent: e.GetName(), PageSize: 1000})
	if err == nil && len(ls.GetListings()) > 0 {
		var ids []string
		for _, l := range ls.GetListings() {
			ids = append(ids, lastSegment(l.GetName()))
		}
		acts = append(acts, console.Action{ID: "delete-listing", Label: "Delete listing", Destructive: true, Fields: []console.Field{
			{Name: "listing", Label: "Listing", Type: "select", Required: true, Options: ids}}})
	}
	byState := map[ahpb.QueryTemplate_State][]string{}
	if ts, err := api.ListQueryTemplates(ctx, &ahpb.ListQueryTemplatesRequest{Parent: e.GetName(), PageSize: 1000}); err == nil {
		for _, t := range ts.GetQueryTemplates() {
			byState[t.GetState()] = append(byState[t.GetState()], lastSegment(t.GetName()))
		}
	}
	if d := byState[ahpb.QueryTemplate_DRAFTED]; len(d) > 0 {
		acts = append(acts,
			console.Action{ID: "edit-template", Label: "Edit template", Fields: []console.Field{
				{Name: "template", Label: "Template", Type: "select", Required: true, Options: d},
				{Name: "description", Label: "Description", Type: "textarea"},
				{Name: "primaryContact", Label: "Primary contact", Type: "text"},
				{Name: "definition", Label: "Query", Type: "textarea", Help: "Empty keeps the template's query."}}},
			console.Action{ID: "submit-template", Label: "Submit for review", Fields: []console.Field{
				{Name: "template", Label: "Template", Type: "select", Required: true, Options: d}}},
			console.Action{ID: "delete-template", Label: "Delete template", Destructive: true, Fields: []console.Field{
				{Name: "template", Label: "Template", Type: "select", Required: true, Options: d}}})
	}
	if pend := byState[ahpb.QueryTemplate_PENDING]; len(pend) > 0 {
		acts = append(acts, console.Action{ID: "approve-template", Label: "Approve", Fields: []console.Field{
			{Name: "template", Label: "Template", Type: "select", Required: true, Options: pend},
			{Name: "dataset", Label: "Template location", Type: "text", Required: true, Default: "projects/" + project + "/datasets/",
				Help: "The approver's dataset, where the table-valued function is created and listed in the clean room."}}})
	}
	if subs, err := api.ListSharedResourceSubscriptions(ctx, &ahpb.ListSharedResourceSubscriptionsRequest{Resource: e.GetName(), PageSize: 1000}); err == nil {
		acts = append(acts, hubSubscriptionActions(subs.GetSharedResourceSubscriptions(), true)...)
	}
	return acts
}

var (
	datasetNameRE = regexp.MustCompile(`^projects/([^/]+)/datasets/([A-Za-z0-9_]+)$`)
	columnRE      = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	// templateHeadRE is a template's QUERY_TEMPLATE_ID( prefix.
	templateHeadRE = regexp.MustCompile(`^\s*([A-Za-z_][A-Za-z0-9_]*)\s*\(`)
)

func splitColumns(s string) ([]string, error) {
	var out []string
	for _, c := range strings.Split(s, ",") {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		if !columnRE.MatchString(c) {
			return nil, fmt.Errorf("%q is not a column name", c)
		}
		out = append(out, c)
	}
	return out, nil
}

// analysisRulePolicy is the privacy_policy JSON of Add data's choice.
func analysisRulePolicy(v map[string]string) (string, error) {
	num := func(field string) (float64, error) {
		f, err := strconv.ParseFloat(strings.TrimSpace(v[field]), 64)
		if err != nil {
			return 0, fmt.Errorf("%s must be a number", field)
		}
		return f, nil
	}
	var pol map[string]any
	switch v["rule"] {
	case "None", "":
		return "", nil
	case "Aggregation threshold":
		n, err := strconv.Atoi(strings.TrimSpace(v["threshold"]))
		if err != nil || n < 1 {
			return "", errors.New("the aggregation threshold must be a positive whole number")
		}
		pol = map[string]any{"aggregation_threshold_policy": map[string]any{"threshold": n, "privacy_unit_column": v["privacyUnit"]}}
	case "Differential privacy":
		dp := map[string]any{"privacy_unit_column": v["privacyUnit"]}
		for field, key := range map[string]string{"epsilon": "max_epsilon_per_query", "epsilonBudget": "epsilon_budget",
			"delta": "delta_per_query", "deltaBudget": "delta_budget"} {
			f, err := num(field)
			if err != nil {
				return "", err
			}
			dp[key] = f
		}
		pol = map[string]any{"differential_privacy_policy": dp}
	case "List overlap":
		cols, err := splitColumns(v["joinColumns"])
		if err != nil {
			return "", err
		}
		if len(cols) == 0 {
			return "", errors.New("list overlap needs at least one join allowed column")
		}
		cond := v["joinCondition"]
		if cond != "JOIN_ALL" {
			cond = "JOIN_ANY"
		}
		pol = map[string]any{"join_restriction_policy": map[string]any{"join_condition": cond, "join_allowed_columns": cols}}
	default:
		return "", fmt.Errorf("unknown analysis rule %q", v["rule"])
	}
	if unit, ok := v["privacyUnit"]; (v["rule"] == "Aggregation threshold" || v["rule"] == "Differential privacy") && (!ok || !columnRE.MatchString(unit)) {
		return "", errors.New("choose the privacy unit column")
	}
	b, err := json.Marshal(pol)
	return string(b), err
}

// runQuery runs a statement in the instance's BigQuery, in project.
func (p cleanRoomsProvider) runQuery(ctx context.Context, project, sql string) error {
	h := hubLinker{tunnel: p.svc.bigquery, client: &http.Client{Timeout: 2 * time.Minute}}
	if p.svc.bigquery == nil {
		return errors.New("BigQuery has not started on this instance")
	}
	_, err := h.do(ctx, http.MethodPost, "/projects/"+url.PathEscape(project)+"/queries", nil,
		map[string]any{"query": sql, "useLegacySql": false, "timeoutMs": 60000})
	if err != nil {
		return errors.New(consoleErr(err))
	}
	return nil
}

func bqPath(parts ...string) string { return "`" + strings.Join(parts, ".") + "`" }

func (p cleanRoomsProvider) ActAt(ctx context.Context, project string, path []string, action string, v map[string]string) error {
	a, e, err := p.cleanRoom(ctx, project, path)
	if err != nil {
		return err
	}
	api := a.svc.API()
	room := e.GetName()
	tmpl := room + "/queryTemplates/" + v["template"]
	switch action {
	case "add-data":
		m := datasetNameRE.FindStringSubmatch(strings.TrimSpace(v["dataset"]))
		if m == nil {
			return errors.New("Select dataset must be projects/PROJECT/datasets/DATASET")
		}
		cols, err := splitColumns(v["columns"])
		if err != nil {
			return err
		}
		pol, err := analysisRulePolicy(v)
		if err != nil {
			return err
		}
		sel := "*"
		if len(cols) > 0 {
			sel = strings.Join(cols, ", ")
		}
		ddl := "CREATE VIEW " + bqPath(m[1], m[2], v["view"])
		if pol != "" {
			ddl += " OPTIONS (privacy_policy = " + strconv.Quote(pol) + ")"
		}
		ddl += " AS (SELECT " + sel + " FROM " + bqPath(m[1], m[2], v["table"]) + ")"
		if err := p.runQuery(ctx, m[1], ddl); err != nil {
			return fmt.Errorf("creating the view: %w", err)
		}
		l := &ahpb.Listing{DisplayName: v["view"], PrimaryContact: v["primaryContact"], Description: v["description"],
			Source: &ahpb.Listing_BigqueryDataset{BigqueryDataset: &ahpb.Listing_BigQueryDatasetSource{Dataset: m[0],
				SelectedResources: []*ahpb.Listing_BigQueryDatasetSource_SelectedResource{{Resource: &ahpb.Listing_BigQueryDatasetSource_SelectedResource_Table{
					Table: m[0] + "/tables/" + v["view"]}}}}},
			RestrictedExportConfig: &ahpb.Listing_RestrictedExportConfig{Enabled: true, RestrictQueryResult: checkboxOn(v["restrictQuery"])}}
		_, err = api.CreateListing(ctx, &ahpb.CreateListingRequest{Parent: room, ListingId: v["view"], Listing: l})
	case "delete-listing":
		_, err = api.DeleteListing(ctx, &ahpb.DeleteListingRequest{Name: room + "/listings/" + v["listing"]})
	case "create-template":
		_, err = api.CreateQueryTemplate(ctx, &ahpb.CreateQueryTemplateRequest{Parent: room, QueryTemplateId: v["id"], QueryTemplate: &ahpb.QueryTemplate{
			DisplayName: v["displayName"], Description: v["description"], PrimaryContact: v["primaryContact"],
			Routine: &ahpb.Routine{RoutineType: ahpb.Routine_TABLE_VALUED_FUNCTION, DefinitionBody: strings.TrimSpace(v["definition"])}}})
	case "edit-template":
		in := &ahpb.QueryTemplate{Name: tmpl, Description: v["description"], PrimaryContact: v["primaryContact"]}
		paths := []string{"description", "primary_contact"}
		if d := strings.TrimSpace(v["definition"]); d != "" {
			in.Routine = &ahpb.Routine{RoutineType: ahpb.Routine_TABLE_VALUED_FUNCTION, DefinitionBody: d}
			paths = append(paths, "routine")
		}
		_, err = api.UpdateQueryTemplate(ctx, &ahpb.UpdateQueryTemplateRequest{QueryTemplate: in, UpdateMask: &fieldmaskpb.FieldMask{Paths: paths}})
	case "submit-template":
		_, err = api.SubmitQueryTemplate(ctx, &ahpb.SubmitQueryTemplateRequest{Name: tmpl})
	case "delete-template":
		_, err = api.DeleteQueryTemplate(ctx, &ahpb.DeleteQueryTemplateRequest{Name: tmpl})
	case "approve-template":
		// "(1) run the routine-creation query ..., (2) add the routine as
		// a listing, (3) approve."
		m := datasetNameRE.FindStringSubmatch(strings.TrimSpace(v["dataset"]))
		if m == nil {
			return errors.New("Template location must be projects/PROJECT/datasets/DATASET")
		}
		t, gerr := api.GetQueryTemplate(ctx, &ahpb.GetQueryTemplateRequest{Name: tmpl})
		if gerr != nil {
			return errors.New(consoleErr(gerr))
		}
		body := t.GetRoutine().GetDefinitionBody()
		hm := templateHeadRE.FindStringSubmatch(body)
		if hm == nil {
			return errors.New("the template's query must be QUERY_TEMPLATE_ID(TVF arguments) AS (TVF_DEFINITION)")
		}
		if err := p.runQuery(ctx, m[1], "CREATE OR REPLACE TABLE FUNCTION "+bqPath(m[1], m[2])+"."+strings.TrimSpace(body)); err != nil {
			return fmt.Errorf("creating the table function: %w", err)
		}
		l := &ahpb.Listing{DisplayName: t.GetDisplayName(), Description: t.GetDescription(),
			Source: &ahpb.Listing_BigqueryDataset{BigqueryDataset: &ahpb.Listing_BigQueryDatasetSource{Dataset: m[0],
				SelectedResources: []*ahpb.Listing_BigQueryDatasetSource_SelectedResource{{Resource: &ahpb.Listing_BigQueryDatasetSource_SelectedResource_Routine{
					Routine: m[0] + "/routines/" + hm[1]}}}}}}
		if _, err := api.CreateListing(ctx, &ahpb.CreateListingRequest{Parent: room, ListingId: hm[1], Listing: l}); err != nil {
			return errors.New(consoleErr(err))
		}
		_, err = api.ApproveQueryTemplate(ctx, &ahpb.ApproveQueryTemplateRequest{Name: tmpl})
	case "subscribe":
		sp := strings.TrimSpace(v["project"])
		req := &ahpb.SubscribeDataExchangeRequest{Name: room, Destination: "projects/" + sp + "/locations/" + strings.Split(room, "/")[3],
			Subscription: strings.TrimSpace(v["subscription"])}
		if ds := strings.TrimSpace(v["dataset"]); ds != "" {
			req.DestinationDataset = &ahpb.DestinationDataset{DatasetReference: &ahpb.DestinationDatasetReference{ProjectId: sp, DatasetId: ds}}
		}
		_, err = api.SubscribeDataExchange(ctx, req)
	case "refresh", "delete-subscription":
		subs, lerr := api.ListSharedResourceSubscriptions(ctx, &ahpb.ListSharedResourceSubscriptionsRequest{Resource: room, PageSize: 1000})
		if lerr != nil {
			return errors.New(consoleErr(lerr))
		}
		return hubSubscriptionAct(ctx, a, action, v["subscription"], subs.GetSharedResourceSubscriptions())
	case "revoke":
		s, gerr := api.GetSubscription(ctx, &ahpb.GetSubscriptionRequest{Name: v["subscription"]})
		if gerr != nil {
			return errors.New(consoleErr(gerr))
		}
		if s.GetDataExchange() != room {
			return fmt.Errorf("%s is not a subscription of this clean room", v["subscription"])
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

func (p cleanRoomsProvider) Edit(ctx context.Context, project string, path []string, v map[string]string) error {
	if _, _, err := p.cleanRoom(ctx, project, path); err != nil {
		return err
	}
	return p.hub().Edit(ctx, project, path, v)
}

func (p cleanRoomsProvider) Delete(ctx context.Context, project, row string) error {
	if _, _, err := p.cleanRoom(ctx, project, []string{row}); err != nil {
		return err
	}
	return p.hub().Delete(ctx, project, row)
}
