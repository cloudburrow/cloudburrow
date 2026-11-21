//go:build compat

package compat

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
	"cloud.google.com/go/bigquery/analyticshub/apiv1/analyticshubpb"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// cleanRoomRows runs sql and returns its rows, each as its values joined
// by spaces.
func cleanRoomRows(ctx context.Context, c *bigquery.Client, sql string) ([]string, error) {
	it, err := c.Query(sql).Read(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for {
		var row []bigquery.Value
		err := it.Next(&row)
		if err == iterator.Done {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		parts := make([]string, len(row))
		for i, v := range row {
			parts[i] = fmt.Sprint(v)
		}
		out = append(out, strings.Join(parts, " "))
	}
}

// TestAnalyticsHubDataCleanRoom (#2101), official Go clients
// (cloud.google.com/go/bigquery/analyticshub/apiv1 over gRPC and REST, and
// cloud.google.com/go/bigquery): a data contributor makes a data clean
// room (sharing_environment_config.dcr_exchange_config) and lists three
// views with analysis rules (an aggregation threshold, a list overlap join
// restriction and differential privacy), egress restricted automatically;
// a query template is created, edited, submitted and approved, its
// table-valued function made by the approver and listed as a routine; a
// subscriber in another project subscribes to the clean room, which makes
// one linked dataset, and queries it: a query without SELECT WITH
// AGGREGATION_THRESHOLD is refused, the clause drops the groups below the
// threshold, a threshold below the view's is Google's documented error,
// a join off the allowed columns is Google's documented error, an epsilon
// above the policy's is Google's documented error, CREATE TABLE AS SELECT
// and tabledata.list are refused, and the query template runs.
//
// covers: google.cloud.bigquery.analyticshub.v1.AnalyticsHubService/SubscribeDataExchange, google.cloud.bigquery.analyticshub.v1.AnalyticsHubService/CreateQueryTemplate, google.cloud.bigquery.analyticshub.v1.AnalyticsHubService/GetQueryTemplate, google.cloud.bigquery.analyticshub.v1.AnalyticsHubService/ListQueryTemplates, google.cloud.bigquery.analyticshub.v1.AnalyticsHubService/UpdateQueryTemplate, google.cloud.bigquery.analyticshub.v1.AnalyticsHubService/DeleteQueryTemplate, google.cloud.bigquery.analyticshub.v1.AnalyticsHubService/SubmitQueryTemplate, google.cloud.bigquery.analyticshub.v1.AnalyticsHubService/ApproveQueryTemplate
func TestAnalyticsHubDataCleanRoom(t *testing.T) {
	h := New(t)
	g, r := analyticsHubClients(t, h)
	pub, project := bigqueryClient(t, h)
	sub, subProject := subscriberProject(t, h, "dcrsub-")
	ctx := h.Context()

	dsID := "dcr_" + strings.ReplaceAll(h.Project(), "-", "_")
	ds := pub.Dataset(dsID)
	if err := ds.Create(ctx, &bigquery.DatasetMetadata{Location: "US"}); err != nil {
		t.Fatalf("create dataset: %v", err)
	}
	t.Cleanup(func() { _ = ds.DeleteWithContents(context.Background()) })
	q := func(c *bigquery.Client, sql string) {
		t.Helper()
		if _, err := cleanRoomRows(ctx, c, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	// The analysis rules page's ExamTable: P91 by 3 students, U25 by 3,
	// C83 by 2.
	q(pub, "CREATE TABLE `"+project+"."+dsID+".exam` AS SELECT * FROM UNNEST([STRUCT('Hansen' AS last_name, 'P91' AS test_id, 510 AS test_score), "+
		"('Wang', 'U25', 500), ('Wang', 'C83', 520), ('Wang', 'U25', 460), ('Hansen', 'C83', 420), ('Hansen', 'C83', 560), "+
		"('Devi', 'U25', 580), ('Devi', 'P91', 480), ('Ivanov', 'U25', 490), ('Ivanov', 'P91', 540), ('Silva', 'U25', 550)])")
	q(pub, "CREATE VIEW `"+project+"."+dsID+".exam_view` OPTIONS (privacy_policy = "+
		`'{"aggregation_threshold_policy": {"threshold": 3, "privacy_unit_column": "last_name"}}'`+") AS (SELECT * FROM `"+project+"."+dsID+".exam`)")
	q(pub, "CREATE VIEW `"+project+"."+dsID+".overlap_view` OPTIONS (privacy_policy = "+
		`'{"join_restriction_policy": {"join_condition": "JOIN_ANY", "join_allowed_columns": ["test_id"]}}'`+") AS (SELECT * FROM `"+project+"."+dsID+".exam`)")
	q(pub, "CREATE VIEW `"+project+"."+dsID+".dp_view` OPTIONS (privacy_policy = "+
		`'{"differential_privacy_policy": {"privacy_unit_column": "last_name", "max_epsilon_per_query": 10.01, "epsilon_budget": 1000, "delta_per_query": 0.01, "delta_budget": 1, "max_groups_contributed": 2}}'`+
		") AS (SELECT * FROM `"+project+"."+dsID+".exam`)")

	// The clean room.
	parent := "projects/" + project + "/locations/us"
	roomID := "room_" + strings.ReplaceAll(h.Project(), "-", "_")
	room, err := g.CreateDataExchange(ctx, &analyticshubpb.CreateDataExchangeRequest{Parent: parent, DataExchangeId: roomID,
		DataExchange: &analyticshubpb.DataExchange{DisplayName: "Compat clean room", PrimaryContact: "dcr@example.com",
			SharingEnvironmentConfig: &analyticshubpb.SharingEnvironmentConfig{Environment: &analyticshubpb.SharingEnvironmentConfig_DcrExchangeConfig_{
				DcrExchangeConfig: &analyticshubpb.SharingEnvironmentConfig_DcrExchangeConfig{}}}}})
	if err != nil {
		t.Fatalf("CreateDataExchange (clean room): %v", err)
	}
	t.Cleanup(func() {
		_ = g.DeleteDataExchange(context.Background(), &analyticshubpb.DeleteDataExchangeRequest{Name: room.GetName()})
	})
	if got, err := r.GetDataExchange(ctx, &analyticshubpb.GetDataExchangeRequest{Name: room.GetName()}); err != nil ||
		got.GetSharingEnvironmentConfig().GetDcrExchangeConfig() == nil || !got.GetSharingEnvironmentConfig().GetDcrExchangeConfig().GetSingleLinkedDatasetPerCleanroom() {
		t.Fatalf("GetDataExchange (REST) of the clean room: %v, %v", got, err)
	}
	resource := func(kind, id string) *analyticshubpb.Listing_BigQueryDatasetSource_SelectedResource {
		name := "projects/" + project + "/datasets/" + dsID + "/" + kind + "/" + id
		if kind == "routines" {
			return &analyticshubpb.Listing_BigQueryDatasetSource_SelectedResource{Resource: &analyticshubpb.Listing_BigQueryDatasetSource_SelectedResource_Routine{Routine: name}}
		}
		return &analyticshubpb.Listing_BigQueryDatasetSource_SelectedResource{Resource: &analyticshubpb.Listing_BigQueryDatasetSource_SelectedResource_Table{Table: name}}
	}
	listing := func(id string, sel *analyticshubpb.Listing_BigQueryDatasetSource_SelectedResource, restrictResults bool) *analyticshubpb.Listing {
		t.Helper()
		l := &analyticshubpb.Listing{DisplayName: id, Source: &analyticshubpb.Listing_BigqueryDataset{BigqueryDataset: &analyticshubpb.Listing_BigQueryDatasetSource{
			Dataset: "projects/" + project + "/datasets/" + dsID}}}
		if sel != nil {
			l.GetBigqueryDataset().SelectedResources = []*analyticshubpb.Listing_BigQueryDatasetSource_SelectedResource{sel}
		}
		if restrictResults {
			l.RestrictedExportConfig = &analyticshubpb.Listing_RestrictedExportConfig{RestrictQueryResult: true}
		}
		return l
	}
	if _, err := g.CreateListing(ctx, &analyticshubpb.CreateListingRequest{Parent: room.GetName(), ListingId: "whole", Listing: listing("whole", nil, false)}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("CreateListing of a whole dataset in a clean room = %v, want INVALID_ARGUMENT", err)
	}
	for _, v := range []string{"exam_view", "overlap_view", "dp_view"} {
		l, err := g.CreateListing(ctx, &analyticshubpb.CreateListingRequest{Parent: room.GetName(), ListingId: v, Listing: listing(v, resource("tables", v), v == "exam_view")})
		if err != nil {
			t.Fatalf("CreateListing %s: %v", v, err)
		}
		// "Data egress controls are automatically enabled".
		if c := l.GetRestrictedExportConfig(); !c.GetEnabled() || !c.GetRestrictDirectTableAccess() || c.GetRestrictQueryResult() != (v == "exam_view") {
			t.Errorf("listing %s's egress: %v", v, c)
		}
	}
	if _, err := g.UpdateListing(ctx, &analyticshubpb.UpdateListingRequest{Listing: &analyticshubpb.Listing{Name: room.GetName() + "/listings/overlap_view",
		RestrictedExportConfig: &analyticshubpb.Listing_RestrictedExportConfig{Enabled: false}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"restricted_export_config"}}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("UpdateListing of a clean room listing's egress = %v, want INVALID_ARGUMENT", err)
	}

	// A query template: created, edited and submitted by its creator,
	// approved by the data's owner, who makes its table-valued function and
	// lists it.
	tvf := "score_by_test"
	body := tvf + "(min_score INT64) AS (SELECT test_id, COUNT(DISTINCT last_name) AS students FROM `" + project + "." + dsID +
		".exam` WHERE test_score >= min_score GROUP BY test_id HAVING COUNT(DISTINCT last_name) >= 3)"
	tmpl, err := r.CreateQueryTemplate(ctx, &analyticshubpb.CreateQueryTemplateRequest{Parent: room.GetName(), QueryTemplateId: "scores",
		QueryTemplate: &analyticshubpb.QueryTemplate{DisplayName: "Scores by test", PrimaryContact: "dcr@example.com",
			Routine: &analyticshubpb.Routine{RoutineType: analyticshubpb.Routine_TABLE_VALUED_FUNCTION, DefinitionBody: body}}})
	if err != nil {
		t.Fatalf("CreateQueryTemplate (REST): %v", err)
	}
	if tmpl.GetState() != analyticshubpb.QueryTemplate_DRAFTED {
		t.Errorf("a new template is %v, want DRAFTED", tmpl.GetState())
	}
	draft, err := g.CreateQueryTemplate(ctx, &analyticshubpb.CreateQueryTemplateRequest{Parent: room.GetName(), QueryTemplateId: "scratch",
		QueryTemplate: &analyticshubpb.QueryTemplate{DisplayName: "Scratch"}})
	if err != nil {
		t.Fatalf("CreateQueryTemplate: %v", err)
	}
	if up, err := g.UpdateQueryTemplate(ctx, &analyticshubpb.UpdateQueryTemplateRequest{QueryTemplate: &analyticshubpb.QueryTemplate{Name: tmpl.GetName(),
		Description: "Students per test at or above a score"}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"description"}}}); err != nil ||
		up.GetDescription() != "Students per test at or above a score" || up.GetRoutine().GetDefinitionBody() != body {
		t.Errorf("UpdateQueryTemplate: %v, %v", up, err)
	}
	if _, err := g.UpdateQueryTemplate(ctx, &analyticshubpb.UpdateQueryTemplateRequest{QueryTemplate: &analyticshubpb.QueryTemplate{Name: tmpl.GetName(),
		DisplayName: "Renamed"}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"display_name"}}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("UpdateQueryTemplate of the display name = %v, want INVALID_ARGUMENT", err)
	}
	if err := g.DeleteQueryTemplate(ctx, &analyticshubpb.DeleteQueryTemplateRequest{Name: draft.GetName()}); err != nil {
		t.Errorf("DeleteQueryTemplate of a draft: %v", err)
	}
	if st, err := g.SubmitQueryTemplate(ctx, &analyticshubpb.SubmitQueryTemplateRequest{Name: tmpl.GetName()}); err != nil || st.GetState() != analyticshubpb.QueryTemplate_PENDING {
		t.Fatalf("SubmitQueryTemplate: %v, %v", st, err)
	}
	if err := g.DeleteQueryTemplate(ctx, &analyticshubpb.DeleteQueryTemplateRequest{Name: tmpl.GetName()}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("DeleteQueryTemplate of a submitted template = %v, want FAILED_PRECONDITION", err)
	}
	q(pub, "CREATE OR REPLACE TABLE FUNCTION `"+project+"."+dsID+"`."+body)
	if _, err := g.CreateListing(ctx, &analyticshubpb.CreateListingRequest{Parent: room.GetName(), ListingId: tvf, Listing: listing(tvf, resource("routines", tvf), false)}); err != nil {
		t.Fatalf("CreateListing of the template's routine: %v", err)
	}
	if ap, err := r.ApproveQueryTemplate(ctx, &analyticshubpb.ApproveQueryTemplateRequest{Name: tmpl.GetName()}); err != nil || ap.GetState() != analyticshubpb.QueryTemplate_APPROVED {
		t.Fatalf("ApproveQueryTemplate (REST): %v, %v", ap, err)
	}
	if got, err := g.GetQueryTemplate(ctx, &analyticshubpb.GetQueryTemplateRequest{Name: tmpl.GetName()}); err != nil || got.GetState() != analyticshubpb.QueryTemplate_APPROVED {
		t.Errorf("GetQueryTemplate: %v, %v", got, err)
	}
	var names []string
	it := g.ListQueryTemplates(ctx, &analyticshubpb.ListQueryTemplatesRequest{Parent: room.GetName()})
	for {
		qt, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListQueryTemplates: %v", err)
		}
		names = append(names, qt.GetName())
	}
	if len(names) != 1 || names[0] != tmpl.GetName() {
		t.Errorf("ListQueryTemplates = %v, want only %s", names, tmpl.GetName())
	}

	// The subscriber subscribes to the clean room: one linked dataset,
	// named after it.
	op, err := g.SubscribeDataExchange(ctx, &analyticshubpb.SubscribeDataExchangeRequest{Name: room.GetName(),
		Destination: "projects/" + subProject + "/locations/us", Subscription: "dcr_sub", SubscriberContact: "sub@example.com"})
	if err != nil {
		t.Fatalf("SubscribeDataExchange: %v", err)
	}
	resp, err := op.Wait(ctx)
	if err != nil {
		t.Fatalf("SubscribeDataExchange: %v", err)
	}
	s := resp.GetSubscription()
	t.Cleanup(func() {
		if op, err := g.DeleteSubscription(context.Background(), &analyticshubpb.DeleteSubscriptionRequest{Name: s.GetName()}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	if s.GetDataExchange() != room.GetName() || len(s.GetLinkedDatasetMap()) != 4 || s.GetState() != analyticshubpb.Subscription_STATE_ACTIVE {
		t.Fatalf("subscription: %v", s)
	}
	if got, err := g.GetSubscription(ctx, &analyticshubpb.GetSubscriptionRequest{Name: s.GetName()}); err != nil || got.GetDataExchange() != room.GetName() {
		t.Errorf("GetSubscription: %v, %v", got, err)
	}
	linked := roomID
	if md, err := sub.Dataset(linked).Metadata(ctx); err != nil {
		t.Fatalf("the linked dataset %s: %v", linked, err)
	} else if md.Location != "US" && md.Location != "us" {
		t.Errorf("linked dataset location = %q, want US", md.Location)
	}
	var tables []string
	tit := sub.Dataset(linked).Tables(ctx)
	for {
		tb, err := tit.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("list the linked dataset's tables: %v", err)
		}
		tables = append(tables, tb.TableID)
	}
	if strings.Join(tables, ",") != "dp_view,exam_view,overlap_view" {
		t.Errorf("linked tables = %v", tables)
	}
	ref := "`" + subProject + "." + linked + "."

	// Aggregation threshold: a query without the clause is refused.
	_, err = cleanRoomRows(ctx, sub, "SELECT test_id, COUNT(DISTINCT last_name) AS n FROM "+ref+"exam_view` GROUP BY test_id")
	if err == nil || !strings.Contains(err.Error(), "You must use SELECT WITH AGGREGATION_THRESHOLD for this query because a privacy policy is set on "+linked+".exam_view.") {
		t.Errorf("a query without SELECT WITH AGGREGATION_THRESHOLD = %v", err)
	}
	// The clause drops the groups of fewer than 3 students (C83).
	rows, err := cleanRoomRows(ctx, sub, "SELECT WITH AGGREGATION_THRESHOLD test_id, COUNT(DISTINCT last_name) AS n FROM "+ref+"exam_view` GROUP BY test_id ORDER BY test_id")
	if err != nil || strings.Join(rows, ",") != "P91 3,U25 4" {
		t.Errorf("SELECT WITH AGGREGATION_THRESHOLD = %v, %v; want P91 3,U25 4", rows, err)
	}
	_, err = cleanRoomRows(ctx, sub, "SELECT WITH AGGREGATION_THRESHOLD OPTIONS (threshold = 2) test_id, COUNT(*) AS n FROM "+ref+"exam_view` GROUP BY test_id")
	if err == nil || !strings.Contains(err.Error(), "Aggregation threshold is too low.") {
		t.Errorf("a threshold below the view's = %v, want Aggregation threshold is too low.", err)
	}
	// Restricted export: no CREATE TABLE AS SELECT, no tabledata.list.
	_, err = cleanRoomRows(ctx, sub, "CREATE TABLE `"+subProject+".mine_"+linked+".copy` AS SELECT WITH AGGREGATION_THRESHOLD test_id, COUNT(*) AS n FROM "+ref+"exam_view` GROUP BY test_id")
	if !accessDenied(err) {
		t.Errorf("CREATE TABLE AS SELECT of the linked dataset = %v, want 403", err)
	}
	if err := sub.Dataset(linked).Table("exam_view").Read(ctx).Next(&[]bigquery.Value{}); !accessDenied(err) {
		t.Errorf("tabledata.list of a linked view = %v, want 403", err)
	}

	// List overlap: a join must be on test_id.
	mine := sub.Dataset("mine_" + linked)
	if err := mine.Create(ctx, &bigquery.DatasetMetadata{Location: "US"}); err != nil {
		t.Fatalf("create the subscriber's dataset: %v", err)
	}
	t.Cleanup(func() { _ = mine.DeleteWithContents(context.Background()) })
	q(sub, "CREATE TABLE `"+subProject+".mine_"+linked+".tests` AS SELECT * FROM UNNEST([STRUCT('P91' AS test_id, 'Hansen' AS last_name), ('U25', 'Wang')])")
	_, err = cleanRoomRows(ctx, sub, "SELECT o.test_score FROM "+ref+"overlap_view` AS o INNER JOIN `"+subProject+".mine_"+linked+".tests` AS m USING (last_name)")
	if want := "Joining must occur on at least one of the following columns [test_id] on table " + linked + ".overlap_view."; err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("a join on last_name = %v, want %s", err, want)
	}
	rows, err = cleanRoomRows(ctx, sub, "SELECT COUNT(*) FROM "+ref+"overlap_view` AS o INNER JOIN `"+subProject+".mine_"+linked+".tests` AS m USING (test_id)")
	if err != nil || strings.Join(rows, ",") != "8" {
		t.Errorf("a join on test_id = %v, %v; want 8", rows, err)
	}

	// Differential privacy: the policy's epsilon bound holds.
	_, err = cleanRoomRows(ctx, sub, "SELECT WITH DIFFERENTIAL_PRIVACY OPTIONS (epsilon = 1e20, delta = 0.01) test_id, COUNT(*) AS n FROM "+ref+"dp_view` GROUP BY test_id")
	if want := "Epsilon is too high: 1e+20, policy for table " + linked + ".dp_view allows max 10.01"; err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("an epsilon above the policy's = %v, want %s", err, want)
	}

	// The approved query template runs in the linked dataset.
	rows, err = cleanRoomRows(ctx, sub, "SELECT * FROM "+ref+tvf+"`(500) ORDER BY test_id")
	if err != nil || strings.Join(rows, ",") != "U25 3" {
		t.Errorf("the query template = %v, %v; want U25 3", rows, err)
	}
	var routines []string
	rit := sub.Dataset(linked).Routines(ctx)
	for {
		rt, err := rit.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("list the linked dataset's routines: %v", err)
		}
		routines = append(routines, rt.RoutineID)
	}
	if strings.Join(routines, ",") != tvf {
		t.Errorf("linked routines = %v, want %s", routines, tvf)
	}

	var subs []string
	sit := g.ListSubscriptions(ctx, &analyticshubpb.ListSubscriptionsRequest{Parent: "projects/" + subProject + "/locations/us"})
	for {
		x, err := sit.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListSubscriptions: %v", err)
		}
		subs = append(subs, x.GetName())
	}
	if len(subs) != 1 || subs[0] != s.GetName() {
		t.Errorf("ListSubscriptions = %v", subs)
	}
}
