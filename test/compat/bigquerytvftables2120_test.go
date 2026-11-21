//go:build compat

package compat

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
	"cloud.google.com/go/bigquery/analyticshub/apiv1/analyticshubpb"
	bq "google.golang.org/api/bigquery/v2"
	"google.golang.org/api/googleapi"
)

// Table parameters of table functions (#2120).
//
// BigQuery: a table function's parameter may be a table, TABLE<column
// type, ...>, which "accepts an input table containing a superset of those
// columns in any order", or ANY TABLE; a call gives it with the TABLE
// keyword, TABLE dataset.table, or as a subquery
// (https://cloud.google.com/bigquery/docs/table-functions#table_parameters).
// A routine's argument of one is argumentKind FIXED_TABLE with its
// tableType, or ANY_TABLE
// (https://cloud.google.com/bigquery/docs/reference/rest/v2/routines#Argument).

// TestBigQueryTableFunctionTableParameters, official Go clients
// (cloud.google.com/go/bigquery, and google.golang.org/api/bigquery/v2 for
// the tableType the Go client's RoutineArgument does not carry): CREATE
// TABLE FUNCTION of a TABLE<...> parameter and of an ANY TABLE one; calls
// with TABLE dataset.table, a table with columns beyond the parameter's in
// another order, a subquery, and an INT64 column coerced to a FLOAT64
// parameter column; a column of a type that does not coerce, and one
// missing, fail the call with the analyzer's errors; routines.get gives
// the arguments' kinds and tableType; routines.insert makes one of each
// kind, and a second insert of the name is 409.
//
// covers: bigquery.routines.insert, bigquery.routines.get, bigquery.jobs.query
func TestBigQueryTableFunctionTableParameters(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	svc := bigqueryService(t, h)
	ctx := h.Context()
	d := "tvftables_" + strings.ReplaceAll(h.Project(), "-", "_")
	ds := c.Dataset(d)
	if err := ds.Create(ctx, &bigquery.DatasetMetadata{Location: "US"}); err != nil {
		t.Fatalf("create dataset: %v", err)
	}
	t.Cleanup(func() { _ = ds.DeleteWithContents(context.Background()) })
	q := func(sql string) {
		t.Helper()
		if _, err := cleanRoomRows(ctx, c, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	rows := func(sql, want string) {
		t.Helper()
		got, err := cleanRoomRows(ctx, c, sql)
		if err != nil || strings.Join(got, ",") != want {
			t.Errorf("%s = %v, %v; want %s", sql, got, err, want)
		}
	}
	fails := func(sql, want string) {
		t.Helper()
		_, err := cleanRoomRows(ctx, c, sql)
		var e *googleapi.Error
		if !errors.As(err, &e) || e.Code != http.StatusBadRequest || !strings.Contains(err.Error(), want) {
			t.Errorf("%s = %v; want 400 with %q", sql, err, want)
		}
	}
	q("CREATE TABLE `" + d + ".orders` AS SELECT * FROM UNNEST([STRUCT('apple' AS item, 2 AS sales, 'x' AS store), ('apple', 4, 'y'), ('pear', 1, 'x')])")
	q("CREATE TABLE `" + d + ".years` AS SELECT * FROM UNNEST([STRUCT('a' AS name, 2019 AS year), ('b', 2021), ('c', 2023)])")
	q("CREATE TABLE `" + d + ".syears` (year STRING)")

	// The table functions page's compute_sales.
	q("CREATE TABLE FUNCTION `" + d + ".compute_sales`(orders TABLE<sales INT64, item STRING>, item_name STRING) AS (" +
		"SELECT SUM(sales) AS total_sales, item FROM orders WHERE item = item_name GROUP BY item)")
	rows("SELECT * FROM `"+d+".compute_sales`(TABLE `"+d+".orders`, 'apple')", "6 apple")
	rows("SELECT * FROM `"+d+".compute_sales`((SELECT 'pear' AS item, 5 AS sales), 'pear')", "5 pear")
	q("CREATE OR REPLACE TABLE FUNCTION `" + d + ".after`(t1 TABLE<year INT64>, y INT64) AS (SELECT * FROM t1 WHERE year > y)")
	rows("SELECT * FROM `"+d+".after`(TABLE `"+d+".years`, 2020) ORDER BY year", "2021,2023")
	rows("SELECT * FROM `"+d+".after`((SELECT year FROM `"+d+".years` WHERE name != 'c'), 2020)", "2021")
	q("CREATE TABLE FUNCTION `" + d + ".halves`(t1 TABLE<year FLOAT64>) AS (SELECT year / 2 AS half FROM t1)")
	rows("SELECT * FROM `"+d+".halves`(TABLE `"+d+".years`) ORDER BY half", "1009.5,1010.5,1011.5")
	fails("SELECT * FROM `"+d+".after`(TABLE `"+d+".syears`, 2020)", `Invalid type STRING for column "year INT64" of argument 1`)
	fails("SELECT * FROM `"+d+".after`((SELECT 'x' AS name), 2020)", `Required column "year" not found in table passed as argument 1`)

	// ANY TABLE: the input must have the columns the body reads.
	q("CREATE TABLE FUNCTION `" + d + ".named_after`(t ANY TABLE, y INT64) AS (SELECT name FROM t WHERE year > y)")
	rows("SELECT * FROM `"+d+".named_after`(TABLE `"+d+".years`, 2020) ORDER BY name", "b,c")
	rows("SELECT * FROM `"+d+".named_after`((SELECT 'z' AS name, 2030 AS year, 1 AS extra), 2020)", "z")
	fails("SELECT * FROM `"+d+".named_after`((SELECT 2030 AS year), 2020)", "Unrecognized name: name")

	// routines.get: FIXED_TABLE with its tableType, and ANY_TABLE.
	md, err := ds.Routine("after").Metadata(ctx)
	if err != nil {
		t.Fatalf("routines.get after: %v", err)
	}
	if md.Type != "TABLE_VALUED_FUNCTION" || len(md.Arguments) != 2 || md.Arguments[0].Name != "t1" || md.Arguments[0].Kind != "FIXED_TABLE" ||
		md.Arguments[0].DataType != nil || md.Arguments[1].Kind != "FIXED_TYPE" || md.Arguments[1].DataType == nil || md.Arguments[1].DataType.TypeKind != "INT64" {
		t.Errorf("routines.get after: type %s, arguments %+v", md.Type, md.Arguments)
	}
	raw, err := svc.Routines.Get(project, d, "after").Context(ctx).Do()
	if err != nil {
		t.Fatalf("routines.get after (API client): %v", err)
	}
	if a := raw.Arguments; len(a) != 2 || a[0].TableType == nil || len(a[0].TableType.Columns) != 1 || a[0].TableType.Columns[0].Name != "year" ||
		a[0].TableType.Columns[0].Type == nil || a[0].TableType.Columns[0].Type.TypeKind != "INT64" {
		t.Errorf("routines.get after's arguments (API client) = %+v", raw.Arguments)
	}
	if md, err := ds.Routine("named_after").Metadata(ctx); err != nil || len(md.Arguments) != 2 || md.Arguments[0].Kind != "ANY_TABLE" || md.Arguments[0].DataType != nil {
		t.Errorf("routines.get named_after: %+v, %v", md, err)
	}

	// routines.insert: FIXED_TABLE through the API client (tableType),
	// ANY_TABLE through the Go client.
	ins := &bq.Routine{RoutineReference: &bq.RoutineReference{ProjectId: project, DatasetId: d, RoutineId: "inserted"},
		RoutineType: "TABLE_VALUED_FUNCTION", Language: "SQL", DefinitionBody: "SELECT item, sales * 10 AS tens FROM o WHERE sales > m",
		Arguments: []*bq.Argument{
			{Name: "o", ArgumentKind: "FIXED_TABLE", TableType: &bq.StandardSqlTableType{Columns: []*bq.StandardSqlField{
				{Name: "item", Type: &bq.StandardSqlDataType{TypeKind: "STRING"}}, {Name: "sales", Type: &bq.StandardSqlDataType{TypeKind: "INT64"}}}}},
			{Name: "m", ArgumentKind: "FIXED_TYPE", DataType: &bq.StandardSqlDataType{TypeKind: "INT64"}},
		}}
	if _, err := svc.Routines.Insert(project, d, ins).Context(ctx).Do(); err != nil {
		t.Fatalf("routines.insert of a FIXED_TABLE argument: %v", err)
	}
	rows("SELECT * FROM `"+d+".inserted`(TABLE `"+d+".orders`, 1) ORDER BY tens", "apple 20,apple 40")
	got, err := svc.Routines.Get(project, d, "inserted").Context(ctx).Do()
	if err != nil || len(got.Arguments) != 2 || got.Arguments[0].ArgumentKind != "FIXED_TABLE" || got.Arguments[0].TableType == nil ||
		len(got.Arguments[0].TableType.Columns) != 2 || got.Arguments[0].DataType != nil {
		t.Errorf("routines.get inserted = %+v, %v", got, err)
	}
	_, err = svc.Routines.Insert(project, d, ins).Context(ctx).Do()
	var e *googleapi.Error
	if !errors.As(err, &e) || e.Code != http.StatusConflict {
		t.Errorf("a second routines.insert of inserted = %v, want 409", err)
	}
	if err := ds.Routine("passthrough").Create(ctx, &bigquery.RoutineMetadata{Type: "TABLE_VALUED_FUNCTION", Language: "SQL",
		Body: "SELECT * FROM t", Arguments: []*bigquery.RoutineArgument{{Name: "t", Kind: "ANY_TABLE"}}}); err != nil {
		t.Fatalf("routines.insert of an ANY_TABLE argument: %v", err)
	}
	rows("SELECT name FROM `"+d+".passthrough`(TABLE `"+d+".years`) ORDER BY year", "a,b,c")
	if md, err := ds.Routine("passthrough").Metadata(ctx); err != nil || len(md.Arguments) != 1 || md.Arguments[0].Kind != "ANY_TABLE" {
		t.Errorf("routines.get passthrough: %+v, %v", md, err)
	}
}

// TestAnalyticsHubCleanRoomTableTemplate (#2120, #2101), official Go
// clients: the query templates page's example, a template whose function
// takes the subscriber's table (query_template1(t1 TABLE<year INT64>)),
// created, submitted and approved, its function made by the approver and
// listed in the clean room; the subscriber calls it in the linked dataset
// with TABLE of its own table, which has more columns than the template's,
// and with a subquery; a column of the wrong type fails the call. The
// routine forwarded to the linked dataset gives its TABLE argument as
// FIXED_TABLE with the template's tableType.
//
// covers: google.cloud.bigquery.analyticshub.v1.AnalyticsHubService/CreateQueryTemplate, google.cloud.bigquery.analyticshub.v1.AnalyticsHubService/SubmitQueryTemplate, google.cloud.bigquery.analyticshub.v1.AnalyticsHubService/ApproveQueryTemplate, google.cloud.bigquery.analyticshub.v1.AnalyticsHubService/SubscribeDataExchange
func TestAnalyticsHubCleanRoomTableTemplate(t *testing.T) {
	h := New(t)
	g, _ := analyticsHubClients(t, h)
	pub, project := bigqueryClient(t, h)
	svc := bigqueryService(t, h)
	sub, subProject := subscriberProject(t, h, "dcrtbl-")
	ctx := h.Context()

	dsID := "dcrtbl_" + strings.ReplaceAll(h.Project(), "-", "_")
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
	q(pub, "CREATE TABLE `"+project+"."+dsID+".events` AS SELECT * FROM UNNEST([STRUCT('launch' AS event, 2019 AS year), ('expansion', 2021), ('merger', 2023), ('ipo', 2023)])")

	roomID := "tblroom_" + strings.ReplaceAll(h.Project(), "-", "_")
	room, err := g.CreateDataExchange(ctx, &analyticshubpb.CreateDataExchangeRequest{Parent: "projects/" + project + "/locations/us", DataExchangeId: roomID,
		DataExchange: &analyticshubpb.DataExchange{DisplayName: "Table template clean room", PrimaryContact: "dcr@example.com",
			SharingEnvironmentConfig: &analyticshubpb.SharingEnvironmentConfig{Environment: &analyticshubpb.SharingEnvironmentConfig_DcrExchangeConfig_{
				DcrExchangeConfig: &analyticshubpb.SharingEnvironmentConfig_DcrExchangeConfig{}}}}})
	if err != nil {
		t.Fatalf("CreateDataExchange (clean room): %v", err)
	}
	t.Cleanup(func() {
		_ = g.DeleteDataExchange(context.Background(), &analyticshubpb.DeleteDataExchangeRequest{Name: room.GetName()})
	})

	// The template: the subscriber's years against the owner's events.
	tvf := "query_template1"
	body := tvf + "(t1 TABLE<year INT64>) AS (SELECT e.year, e.event FROM `" + project + "." + dsID + ".events` AS e WHERE e.year IN (SELECT year FROM t1))"
	tmpl, err := g.CreateQueryTemplate(ctx, &analyticshubpb.CreateQueryTemplateRequest{Parent: room.GetName(), QueryTemplateId: "years",
		QueryTemplate: &analyticshubpb.QueryTemplate{DisplayName: "Events in your years", PrimaryContact: "dcr@example.com",
			Routine: &analyticshubpb.Routine{RoutineType: analyticshubpb.Routine_TABLE_VALUED_FUNCTION, DefinitionBody: body}}})
	if err != nil {
		t.Fatalf("CreateQueryTemplate: %v", err)
	}
	if st, err := g.SubmitQueryTemplate(ctx, &analyticshubpb.SubmitQueryTemplateRequest{Name: tmpl.GetName()}); err != nil || st.GetState() != analyticshubpb.QueryTemplate_PENDING {
		t.Fatalf("SubmitQueryTemplate: %v, %v", st, err)
	}
	q(pub, "CREATE OR REPLACE TABLE FUNCTION `"+project+"."+dsID+"`."+body)
	if _, err := g.CreateListing(ctx, &analyticshubpb.CreateListingRequest{Parent: room.GetName(), ListingId: tvf, Listing: &analyticshubpb.Listing{DisplayName: tvf,
		Source: &analyticshubpb.Listing_BigqueryDataset{BigqueryDataset: &analyticshubpb.Listing_BigQueryDatasetSource{Dataset: "projects/" + project + "/datasets/" + dsID,
			SelectedResources: []*analyticshubpb.Listing_BigQueryDatasetSource_SelectedResource{{Resource: &analyticshubpb.Listing_BigQueryDatasetSource_SelectedResource_Routine{
				Routine: "projects/" + project + "/datasets/" + dsID + "/routines/" + tvf}}}}}}}); err != nil {
		t.Fatalf("CreateListing of the template's routine: %v", err)
	}
	if ap, err := g.ApproveQueryTemplate(ctx, &analyticshubpb.ApproveQueryTemplateRequest{Name: tmpl.GetName()}); err != nil || ap.GetState() != analyticshubpb.QueryTemplate_APPROVED {
		t.Fatalf("ApproveQueryTemplate: %v, %v", ap, err)
	}

	op, err := g.SubscribeDataExchange(ctx, &analyticshubpb.SubscribeDataExchangeRequest{Name: room.GetName(),
		Destination: "projects/" + subProject + "/locations/us", Subscription: "tbl_sub", SubscriberContact: "sub@example.com"})
	if err != nil {
		t.Fatalf("SubscribeDataExchange: %v", err)
	}
	resp, err := op.Wait(ctx)
	if err != nil {
		t.Fatalf("SubscribeDataExchange: %v", err)
	}
	t.Cleanup(func() {
		if op, err := g.DeleteSubscription(context.Background(), &analyticshubpb.DeleteSubscriptionRequest{Name: resp.GetSubscription().GetName()}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	linked := roomID

	// The subscriber's own table: more columns than the template's, in
	// another order.
	mine := sub.Dataset("mine_" + linked)
	if err := mine.Create(ctx, &bigquery.DatasetMetadata{Location: "US"}); err != nil {
		t.Fatalf("create the subscriber's dataset: %v", err)
	}
	t.Cleanup(func() { _ = mine.DeleteWithContents(context.Background()) })
	q(sub, "CREATE TABLE `"+subProject+".mine_"+linked+".my_years` AS SELECT * FROM UNNEST([STRUCT('ours' AS label, 2021 AS year), ('also', 2023)])")

	call := "SELECT * FROM `" + subProject + "." + linked + "." + tvf + "`"
	rows, err := cleanRoomRows(ctx, sub, call+"(TABLE `"+subProject+".mine_"+linked+".my_years`) ORDER BY year, event")
	if want := "2021 expansion,2023 ipo,2023 merger"; err != nil || strings.Join(rows, ",") != want {
		t.Errorf("the template with TABLE of the subscriber's table = %v, %v; want %s", rows, err, want)
	}
	rows, err = cleanRoomRows(ctx, sub, call+"((SELECT 2019 AS year)) ORDER BY event")
	if err != nil || strings.Join(rows, ",") != "2019 launch" {
		t.Errorf("the template with a subquery = %v, %v; want 2019 launch", rows, err)
	}
	_, err = cleanRoomRows(ctx, sub, call+"((SELECT 'x' AS year))")
	if err == nil || !strings.Contains(err.Error(), `Invalid type STRING for column "year INT64" of argument 1`) {
		t.Errorf("the template with a STRING year = %v, want Invalid type STRING", err)
	}
	r, err := svc.Routines.Get(subProject, linked, tvf).Context(ctx).Do()
	if err != nil || len(r.Arguments) != 1 || r.Arguments[0].Name != "t1" || r.Arguments[0].ArgumentKind != "FIXED_TABLE" || r.Arguments[0].TableType == nil ||
		len(r.Arguments[0].TableType.Columns) != 1 || r.Arguments[0].TableType.Columns[0].Name != "year" {
		t.Errorf("routines.get of the linked template = %+v, %v", r, err)
	}
}

// TestBqTableFunctionTableParameter (#2120): bq query makes a table
// function of a TABLE<...> parameter and calls it with TABLE of a table,
// and bq show --routine prints its argument as FIXED_TABLE with its
// tableType.
//
// covers: bigquery.jobs.insert, bigquery.routines.get
func TestBqTableFunctionTableParameter(t *testing.T) {
	h := New(t)
	vars := bqEnv(t, h)
	c, project := bigqueryClient(t, h)
	ctx := h.Context()
	d := "bqtvf_" + strings.ReplaceAll(h.Project(), "-", "_")
	ds := c.Dataset(d)
	if err := ds.Create(ctx, &bigquery.DatasetMetadata{Location: "US"}); err != nil {
		t.Fatalf("create dataset: %v", err)
	}
	t.Cleanup(func() { _ = ds.DeleteWithContents(context.Background()) })
	b := newBqSession(t, vars, "--nouse_google_auth", bqToken)
	b.must("query", "--nouse_legacy_sql", "CREATE TABLE `"+project+"."+d+".years` AS SELECT * FROM UNNEST([STRUCT('a' AS name, 2019 AS year), ('b', 2021)])")
	b.must("query", "--nouse_legacy_sql", "CREATE TABLE FUNCTION `"+project+"."+d+".after`(t1 TABLE<year INT64>, y INT64) AS (SELECT * FROM t1 WHERE year > y)")
	out := b.must("query", "--nouse_legacy_sql", "--format=json", "SELECT * FROM `"+project+"."+d+".after`(TABLE `"+project+"."+d+".years`, 2020)")
	if !strings.Contains(out, `"year":"2021"`) || strings.Contains(out, "2019") {
		t.Errorf("bq query of the table function printed %q, want the row of 2021 alone", out)
	}
	out = b.must("show", "--format=json", "--routine", project+":"+d+".after")
	if !strings.Contains(out, `"argumentKind":"FIXED_TABLE"`) || !strings.Contains(out, `"tableType":{"columns":[{"name":"year","type":{"typeKind":"INT64"}}]}`) {
		t.Errorf("bq show --routine printed %q, want t1 as FIXED_TABLE with its tableType", out)
	}
}
