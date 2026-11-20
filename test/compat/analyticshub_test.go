//go:build compat

package compat

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
	analyticshub "cloud.google.com/go/bigquery/analyticshub/apiv1"
	"cloud.google.com/go/bigquery/analyticshub/apiv1/analyticshubpb"
	"cloud.google.com/go/iam/apiv1/iampb"
	resourcemanager "cloud.google.com/go/resourcemanager/apiv3"
	rmpb "cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// EnvAnalyticsHub is the host address of BigQuery sharing, formerly
// Analytics Hub (analyticshub.googleapis.com, #1960), gRPC and JSON on one
// port, served with BigQuery.
const EnvAnalyticsHub = "CLOUDBURROW_TEST_ANALYTICSHUB"

// analyticsHubClients is the official client over gRPC and over REST.
func analyticsHubClients(t *testing.T, h *Harness) (g, r *analyticshub.Client) {
	t.Helper()
	addr := h.Endpoint(EnvAnalyticsHub)
	ctx := context.Background()
	g, err := analyticshub.NewClient(ctx, option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	r, err = analyticshub.NewRESTClient(ctx, option.WithEndpoint("http://"+addr), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = g.Close(); _ = r.Close() })
	return g, r
}

// subscriberProject creates a project in the instance's Resource Manager
// for a subscriber, and a BigQuery client of it.
func subscriberProject(t *testing.T, h *Harness, prefix string) (*bigquery.Client, string) {
	t.Helper()
	ctx := h.Context()
	rmEndpoint := shardEndpoint(h, "CLOUDBURROW_TEST_BIGQUERY_RESOURCEMANAGER", EnvResourceManager)
	pc, err := resourcemanager.NewProjectsClient(ctx, rmOptionsAt(rmEndpoint)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	id := v1ID(h, prefix)
	op, err := pc.CreateProject(ctx, &rmpb.CreateProjectRequest{Project: &rmpb.Project{ProjectId: id}})
	if err != nil {
		t.Fatalf("CreateProject %s: %v", id, err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatalf("CreateProject %s: %v", id, err)
	}
	t.Cleanup(func() {
		_, _ = pc.DeleteProject(context.Background(), &rmpb.DeleteProjectRequest{Name: "projects/" + id})
	})
	c, err := bigquery.NewClient(ctx, id, option.WithEndpoint(h.Endpoint(EnvBigQuery)), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, id
}

func readInts(t *testing.T, it *bigquery.RowIterator) []int64 {
	t.Helper()
	var out []int64
	for {
		var row []bigquery.Value
		err := it.Next(&row)
		if err == iterator.Done {
			return out
		}
		if err != nil {
			t.Fatalf("read rows: %v", err)
		}
		out = append(out, row[0].(int64))
	}
}

func accessDenied(err error) bool {
	var ge *googleapi.Error
	if errors.As(err, &ge) {
		return ge.Code == http.StatusForbidden
	}
	return err != nil && strings.Contains(err.Error(), "Access Denied")
}

// TestAnalyticsHubPublishSubscribeQuery (#1960), official Go clients: a
// publisher makes a data exchange and a listing of a BigQuery dataset;
// a subscriber in another project subscribes, which makes a linked dataset
// there; the bigquery client queries it, sees a row the publisher streams
// in after subscribing and a table added later, and reads the shared table
// through tabledata.list; writes into the linked dataset are refused; the
// IAM policy round-trips; subscriptions list and refresh; revoking leaves
// the dataset in place and refuses queries of it; deleting the
// subscription deletes it; deleting the listing and the exchange.
//
// covers: google.cloud.bigquery.analyticshub.v1.AnalyticsHubService/CreateDataExchange, google.cloud.bigquery.analyticshub.v1.AnalyticsHubService/GetDataExchange, google.cloud.bigquery.analyticshub.v1.AnalyticsHubService/ListDataExchanges, google.cloud.bigquery.analyticshub.v1.AnalyticsHubService/UpdateDataExchange, google.cloud.bigquery.analyticshub.v1.AnalyticsHubService/DeleteDataExchange, google.cloud.bigquery.analyticshub.v1.AnalyticsHubService/CreateListing, google.cloud.bigquery.analyticshub.v1.AnalyticsHubService/GetListing, google.cloud.bigquery.analyticshub.v1.AnalyticsHubService/ListListings, google.cloud.bigquery.analyticshub.v1.AnalyticsHubService/UpdateListing, google.cloud.bigquery.analyticshub.v1.AnalyticsHubService/DeleteListing, google.cloud.bigquery.analyticshub.v1.AnalyticsHubService/SubscribeListing, google.cloud.bigquery.analyticshub.v1.AnalyticsHubService/GetSubscription, google.cloud.bigquery.analyticshub.v1.AnalyticsHubService/ListSubscriptions, google.cloud.bigquery.analyticshub.v1.AnalyticsHubService/ListSharedResourceSubscriptions, google.cloud.bigquery.analyticshub.v1.AnalyticsHubService/RefreshSubscription, google.cloud.bigquery.analyticshub.v1.AnalyticsHubService/RevokeSubscription, google.cloud.bigquery.analyticshub.v1.AnalyticsHubService/DeleteSubscription, google.cloud.bigquery.analyticshub.v1.AnalyticsHubService/GetIamPolicy, google.cloud.bigquery.analyticshub.v1.AnalyticsHubService/SetIamPolicy, google.cloud.bigquery.analyticshub.v1.AnalyticsHubService/TestIamPermissions
func TestAnalyticsHubPublishSubscribeQuery(t *testing.T) {
	h := New(t)
	g, r := analyticsHubClients(t, h)
	pub, project := bigqueryClient(t, h)
	sub, subProject := subscriberProject(t, h, "ahsub-")
	ctx := h.Context()
	shared, orders := seedOrders(t, h, pub)

	parent := "projects/" + project + "/locations/us"
	exID := "ex_" + strings.ReplaceAll(h.Project(), "-", "_")
	ex, err := g.CreateDataExchange(ctx, &analyticshubpb.CreateDataExchangeRequest{Parent: parent, DataExchangeId: exID,
		DataExchange: &analyticshubpb.DataExchange{DisplayName: "Compat exchange", Description: "shared orders"}})
	if err != nil {
		t.Fatalf("CreateDataExchange: %v", err)
	}
	t.Cleanup(func() {
		_ = g.DeleteDataExchange(context.Background(), &analyticshubpb.DeleteDataExchangeRequest{Name: ex.GetName()})
	})
	if _, err := g.CreateDataExchange(ctx, &analyticshubpb.CreateDataExchangeRequest{Parent: parent, DataExchangeId: exID,
		DataExchange: &analyticshubpb.DataExchange{DisplayName: "again"}}); status.Code(err) != codes.AlreadyExists {
		t.Errorf("CreateDataExchange again = %v, want ALREADY_EXISTS", err)
	}
	if got, err := r.GetDataExchange(ctx, &analyticshubpb.GetDataExchangeRequest{Name: ex.GetName()}); err != nil || got.GetDisplayName() != "Compat exchange" {
		t.Fatalf("GetDataExchange (REST): %v, %v", got, err)
	}
	found := false
	for it := g.ListDataExchanges(ctx, &analyticshubpb.ListDataExchangesRequest{Parent: parent}); ; {
		e, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListDataExchanges: %v", err)
		}
		found = found || e.GetName() == ex.GetName()
	}
	if !found {
		t.Errorf("ListDataExchanges does not list %s", ex.GetName())
	}
	if _, err := r.UpdateDataExchange(ctx, &analyticshubpb.UpdateDataExchangeRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"description"}},
		DataExchange: &analyticshubpb.DataExchange{Name: ex.GetName(), Description: "orders, shared"}}); err != nil {
		t.Fatalf("UpdateDataExchange (REST): %v", err)
	}

	listing, err := g.CreateListing(ctx, &analyticshubpb.CreateListingRequest{Parent: ex.GetName(), ListingId: "orders",
		Listing: &analyticshubpb.Listing{DisplayName: "Orders", Source: &analyticshubpb.Listing_BigqueryDataset{
			BigqueryDataset: &analyticshubpb.Listing_BigQueryDatasetSource{Dataset: "projects/" + project + "/datasets/" + shared.DatasetID}}}})
	if err != nil {
		t.Fatalf("CreateListing: %v", err)
	}
	if listing.GetState() != analyticshubpb.Listing_ACTIVE || listing.GetResourceType() != analyticshubpb.SharedResourceType_BIGQUERY_DATASET {
		t.Errorf("listing = %v", listing)
	}
	if _, err := g.CreateListing(ctx, &analyticshubpb.CreateListingRequest{Parent: ex.GetName(), ListingId: "missing",
		Listing: &analyticshubpb.Listing{DisplayName: "Missing", Source: &analyticshubpb.Listing_BigqueryDataset{
			BigqueryDataset: &analyticshubpb.Listing_BigQueryDatasetSource{Dataset: "projects/" + project + "/datasets/no_such_dataset"}}}}); status.Code(err) != codes.NotFound {
		t.Errorf("CreateListing of a missing dataset = %v, want NOT_FOUND", err)
	}
	if got, err := r.GetListing(ctx, &analyticshubpb.GetListingRequest{Name: listing.GetName()}); err != nil || got.GetDisplayName() != "Orders" {
		t.Fatalf("GetListing (REST): %v, %v", got, err)
	}
	if it := r.ListListings(ctx, &analyticshubpb.ListListingsRequest{Parent: ex.GetName()}); true {
		l, err := it.Next()
		if err != nil || l.GetName() != listing.GetName() {
			t.Fatalf("ListListings (REST): %v, %v", l, err)
		}
	}
	if _, err := g.UpdateListing(ctx, &analyticshubpb.UpdateListingRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"description"}},
		Listing: &analyticshubpb.Listing{Name: listing.GetName(), Description: "every order"}}); err != nil {
		t.Fatalf("UpdateListing: %v", err)
	}
	policy, err := g.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: listing.GetName(), Policy: &iampb.Policy{
		Bindings: []*iampb.Binding{{Role: "roles/analyticshub.subscriber", Members: []string{"user:subscriber@example.com"}}}}})
	if err != nil {
		t.Fatalf("SetIamPolicy: %v", err)
	}
	if got, err := r.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: listing.GetName()}); err != nil || len(got.GetBindings()) != 1 || string(got.GetEtag()) != string(policy.GetEtag()) {
		t.Fatalf("GetIamPolicy (REST): %v, %v", got, err)
	}
	if perms, err := g.TestIamPermissions(ctx, &iampb.TestIamPermissionsRequest{Resource: ex.GetName(),
		Permissions: []string{"analyticshub.dataExchanges.get"}}); err != nil || len(perms.GetPermissions()) != 1 {
		t.Fatalf("TestIamPermissions: %v, %v", perms, err)
	}

	linkedID := "linked_orders"
	resp, err := r.SubscribeListing(ctx, &analyticshubpb.SubscribeListingRequest{Name: listing.GetName(),
		Destination: &analyticshubpb.SubscribeListingRequest_DestinationDataset{DestinationDataset: &analyticshubpb.DestinationDataset{
			Location: "US", DatasetReference: &analyticshubpb.DestinationDatasetReference{ProjectId: subProject, DatasetId: linkedID}}}})
	if err != nil {
		t.Fatalf("SubscribeListing (REST): %v", err)
	}
	subscription := resp.GetSubscription()
	if subscription.GetState() != analyticshubpb.Subscription_STATE_ACTIVE ||
		subscription.GetLinkedDatasetMap()[listing.GetName()].GetLinkedDataset() != "projects/"+subProject+"/datasets/"+linkedID {
		t.Fatalf("subscription = %v", subscription)
	}
	if _, err := g.SubscribeListing(ctx, &analyticshubpb.SubscribeListingRequest{Name: listing.GetName(),
		Destination: &analyticshubpb.SubscribeListingRequest_DestinationDataset{DestinationDataset: &analyticshubpb.DestinationDataset{
			Location: "US", DatasetReference: &analyticshubpb.DestinationDatasetReference{ProjectId: subProject, DatasetId: linkedID}}}}); status.Code(err) != codes.AlreadyExists {
		t.Errorf("SubscribeListing into an existing dataset = %v, want ALREADY_EXISTS", err)
	}

	linked := sub.Dataset(linkedID)
	if md, err := linked.Metadata(ctx); err != nil || md.Location != "US" {
		t.Fatalf("linked dataset: %v, %v", md, err)
	}
	q := sub.Query("SELECT id FROM `" + subProject + "." + linkedID + ".orders` ORDER BY id")
	it, err := q.Read(ctx)
	if err != nil {
		t.Fatalf("query the linked dataset: %v", err)
	}
	if got := readInts(t, it); len(got) != 4 {
		t.Fatalf("linked rows = %v, want the four shared rows", got)
	}
	// Live: a row the publisher writes after the subscription is read.
	if err := orders.Inserter().Put(ctx, []*order{{5, "eu", 2}}); err != nil {
		t.Fatal(err)
	}
	it, err = sub.Query("SELECT COUNT(*) FROM `" + subProject + "." + linkedID + ".orders`").Read(ctx)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if got := readInts(t, it); len(got) != 1 || got[0] != 5 {
		t.Errorf("linked count = %v, want 5", got)
	}
	// A table the publisher adds is listed and read.
	extra := shared.Table("extra")
	if err := extra.Create(ctx, &bigquery.TableMetadata{Schema: bigquery.Schema{{Name: "n", Type: bigquery.IntegerFieldType}}}); err != nil {
		t.Fatal(err)
	}
	var names []string
	for ti := linked.Tables(ctx); ; {
		tb, err := ti.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("tables.list of the linked dataset: %v", err)
		}
		names = append(names, tb.TableID)
	}
	if strings.Join(names, ",") != "extra,orders" {
		t.Errorf("linked tables = %v, want extra,orders", names)
	}
	if md, err := linked.Table("orders").Metadata(ctx); err != nil || md.Type != bigquery.RegularTable || len(md.Schema) != 3 {
		t.Errorf("linked table metadata: %v, %v", md, err)
	}
	if got := readInts(t, linked.Table("orders").Read(ctx)); len(got) != 5 {
		t.Errorf("tabledata.list of the linked table read %d rows, want 5", len(got))
	}
	// Read-only.
	if err := linked.Table("mine").Create(ctx, &bigquery.TableMetadata{Schema: bigquery.Schema{{Name: "n", Type: bigquery.IntegerFieldType}}}); !accessDenied(err) {
		t.Errorf("tables.insert into the linked dataset = %v, want 403", err)
	}
	job, err := sub.Query("INSERT INTO `" + subProject + "." + linkedID + ".orders` (id, region, amount) VALUES (9, 'x', 1)").Run(ctx)
	if err == nil {
		var st *bigquery.JobStatus
		st, err = job.Wait(ctx)
		if err == nil {
			err = st.Err()
		}
	}
	if !accessDenied(err) {
		t.Errorf("INSERT into the linked dataset = %v, want Access Denied", err)
	}

	subs, err := g.ListSubscriptions(ctx, &analyticshubpb.ListSubscriptionsRequest{Parent: "projects/" + subProject + "/locations/us",
		Filter: `listing="` + listing.GetName() + `"`}).Next()
	if err != nil || subs.GetName() != subscription.GetName() {
		t.Fatalf("ListSubscriptions: %v, %v", subs, err)
	}
	if s, err := g.ListSharedResourceSubscriptions(ctx, &analyticshubpb.ListSharedResourceSubscriptionsRequest{Resource: ex.GetName()}).Next(); err != nil || s.GetName() != subscription.GetName() {
		t.Fatalf("ListSharedResourceSubscriptions: %v, %v", s, err)
	}
	refresh, err := g.RefreshSubscription(ctx, &analyticshubpb.RefreshSubscriptionRequest{Name: subscription.GetName()})
	if err != nil {
		t.Fatalf("RefreshSubscription: %v", err)
	}
	if rr, err := refresh.Wait(ctx); err != nil || rr.GetSubscription().GetName() != subscription.GetName() {
		t.Fatalf("RefreshSubscription.Wait: %v, %v", rr, err)
	}

	// Revoked: the linked dataset stays, and is no longer queried.
	if _, err := g.RevokeSubscription(ctx, &analyticshubpb.RevokeSubscriptionRequest{Name: subscription.GetName()}); err != nil {
		t.Fatalf("RevokeSubscription: %v", err)
	}
	if got, err := r.GetSubscription(ctx, &analyticshubpb.GetSubscriptionRequest{Name: subscription.GetName()}); err != nil || got.GetState() != analyticshubpb.Subscription_STATE_INACTIVE {
		t.Fatalf("GetSubscription after revoke: %v, %v", got, err)
	}
	if _, err := linked.Metadata(ctx); err != nil {
		t.Errorf("the revoked subscription's linked dataset is gone: %v", err)
	}
	if _, err := sub.Query("SELECT id FROM `" + subProject + "." + linkedID + ".orders`").Read(ctx); !accessDenied(err) {
		t.Errorf("query of a revoked linked dataset = %v, want Access Denied", err)
	}
	del, err := g.DeleteSubscription(ctx, &analyticshubpb.DeleteSubscriptionRequest{Name: subscription.GetName()})
	if err != nil {
		t.Fatalf("DeleteSubscription: %v", err)
	}
	if err := del.Wait(ctx); err != nil {
		t.Fatalf("DeleteSubscription.Wait: %v", err)
	}
	var ge *googleapi.Error
	if _, err := linked.Metadata(ctx); !errors.As(err, &ge) || ge.Code != http.StatusNotFound {
		t.Errorf("the deleted subscription's linked dataset: %v, want 404", err)
	}
	if err := g.DeleteListing(ctx, &analyticshubpb.DeleteListingRequest{Name: listing.GetName()}); err != nil {
		t.Fatalf("DeleteListing: %v", err)
	}
	if err := r.DeleteDataExchange(ctx, &analyticshubpb.DeleteDataExchangeRequest{Name: ex.GetName()}); err != nil {
		t.Fatalf("DeleteDataExchange (REST): %v", err)
	}
	if _, err := g.GetDataExchange(ctx, &analyticshubpb.GetDataExchangeRequest{Name: ex.GetName()}); status.Code(err) != codes.NotFound {
		t.Errorf("GetDataExchange after delete = %v, want NOT_FOUND", err)
	}
}

// TestAnalyticsHubRestrictedExport (#1960): a listing with
// restrictedExportConfig.enabled; through the official bigquery client the
// subscriber queries the linked dataset, but a copy job, an extract job,
// a query job with a destination table, CREATE TABLE AS SELECT and
// tabledata.list of it are refused 403, as "Copy, clone, export, and
// snapshot APIs are unavailable" and "CREATE TABLE AS SELECT statements
// and writing to a destination table are unavailable".
//
// covers: google.cloud.bigquery.analyticshub.v1.AnalyticsHubService/CreateListing, google.cloud.bigquery.analyticshub.v1.AnalyticsHubService/SubscribeListing, google.cloud.bigquery.analyticshub.v1.AnalyticsHubService/DeleteListing
func TestAnalyticsHubRestrictedExport(t *testing.T) {
	h := New(t)
	g, _ := analyticsHubClients(t, h)
	pub, project := bigqueryClient(t, h)
	sub, subProject := subscriberProject(t, h, "ahegr-")
	ctx := h.Context()
	shared, _ := seedOrders(t, h, pub)

	ex, err := g.CreateDataExchange(ctx, &analyticshubpb.CreateDataExchangeRequest{Parent: "projects/" + project + "/locations/us",
		DataExchangeId: "egress_" + strings.ReplaceAll(h.Project(), "-", "_"), DataExchange: &analyticshubpb.DataExchange{DisplayName: "Restricted"}})
	if err != nil {
		t.Fatalf("CreateDataExchange: %v", err)
	}
	t.Cleanup(func() {
		_ = g.DeleteDataExchange(context.Background(), &analyticshubpb.DeleteDataExchangeRequest{Name: ex.GetName()})
	})
	listing, err := g.CreateListing(ctx, &analyticshubpb.CreateListingRequest{Parent: ex.GetName(), ListingId: "restricted",
		Listing: &analyticshubpb.Listing{DisplayName: "Restricted orders",
			RestrictedExportConfig: &analyticshubpb.Listing_RestrictedExportConfig{Enabled: true, RestrictQueryResult: true},
			Source: &analyticshubpb.Listing_BigqueryDataset{BigqueryDataset: &analyticshubpb.Listing_BigQueryDatasetSource{
				Dataset: "projects/" + project + "/datasets/" + shared.DatasetID}}}})
	if err != nil {
		t.Fatalf("CreateListing: %v", err)
	}
	if !listing.GetRestrictedExportConfig().GetRestrictDirectTableAccess() {
		t.Errorf("restrict_direct_table_access = false with restricted export on")
	}
	if _, err := g.SubscribeListing(ctx, &analyticshubpb.SubscribeListingRequest{Name: listing.GetName(),
		Destination: &analyticshubpb.SubscribeListingRequest_DestinationDataset{DestinationDataset: &analyticshubpb.DestinationDataset{
			Location: "US", DatasetReference: &analyticshubpb.DestinationDatasetReference{ProjectId: subProject, DatasetId: "restricted_orders"}}}}); err != nil {
		t.Fatalf("SubscribeListing: %v", err)
	}
	t.Cleanup(func() {
		_ = g.DeleteListing(context.Background(), &analyticshubpb.DeleteListingRequest{Name: listing.GetName()})
	})
	own := sub.Dataset("own")
	if err := own.Create(ctx, &bigquery.DatasetMetadata{Location: "US"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = own.DeleteWithContents(context.Background()) })
	linkedOrders := sub.Dataset("restricted_orders").Table("orders")
	ref := "`" + subProject + ".restricted_orders.orders`"

	// Reading is allowed.
	it, err := sub.Query("SELECT id FROM " + ref + " ORDER BY id").Read(ctx)
	if err != nil {
		t.Fatalf("query of the restricted linked dataset: %v", err)
	}
	if got := readInts(t, it); len(got) != 4 {
		t.Fatalf("rows = %v", got)
	}
	run := func(what string, j *bigquery.Job, err error) {
		t.Helper()
		if err == nil {
			var st *bigquery.JobStatus
			st, err = j.Wait(ctx)
			if err == nil {
				err = st.Err()
			}
		}
		if !accessDenied(err) {
			t.Errorf("%s = %v, want Access Denied", what, err)
		}
	}
	cp := own.Table("copied").CopierFrom(linkedOrders)
	j, err := cp.Run(ctx)
	run("copy job from the linked table", j, err)
	ext := linkedOrders.ExtractorTo(bigquery.NewGCSReference("gs://nowhere-" + h.Project() + "/out.csv"))
	j, err = ext.Run(ctx)
	run("extract job from the linked table", j, err)
	q := sub.Query("SELECT * FROM " + ref)
	q.Dst = own.Table("dest")
	j, err = q.Run(ctx)
	run("query job with a destination table", j, err)
	j, err = sub.Query("CREATE TABLE `" + subProject + ".own.ctas` AS SELECT * FROM " + ref).Run(ctx)
	run("CREATE TABLE AS SELECT", j, err)
	var rows []bigquery.Value
	if err := linkedOrders.Read(ctx).Next(&rows); !accessDenied(err) {
		t.Errorf("tabledata.list of the restricted linked table = %v, want Access Denied", err)
	}
	if _, err := own.Table("copied").Metadata(ctx); err == nil {
		t.Error("the refused copy made its destination table")
	}
}
