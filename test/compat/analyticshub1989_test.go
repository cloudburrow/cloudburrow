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
	bqstorage "cloud.google.com/go/bigquery/storage/apiv1"
	"cloud.google.com/go/bigquery/storage/apiv1/storagepb"
	"cloud.google.com/go/iam/apiv1/iampb"
	"github.com/googleapis/gax-go/v2/apierror"
	"golang.org/x/oauth2"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// hubListingOf makes, as the instance's own account, a data exchange in
// project and a listing of shared in it, with cfg its restricted export
// configuration (nil for none), and deletes both at the end.
func hubListingOf(t *testing.T, h *Harness, g *analyticshub.Client, project, exchangeID string, shared *bigquery.Dataset,
	cfg *analyticshubpb.Listing_RestrictedExportConfig) (exchange, listing string) {
	t.Helper()
	ctx := h.Context()
	ex, err := g.CreateDataExchange(ctx, &analyticshubpb.CreateDataExchangeRequest{Parent: "projects/" + project + "/locations/us",
		DataExchangeId: exchangeID, DataExchange: &analyticshubpb.DataExchange{DisplayName: "Compat " + exchangeID}})
	if err != nil {
		t.Fatalf("CreateDataExchange: %v", err)
	}
	t.Cleanup(func() {
		_ = g.DeleteDataExchange(context.Background(), &analyticshubpb.DeleteDataExchangeRequest{Name: ex.GetName()})
	})
	l, err := g.CreateListing(ctx, &analyticshubpb.CreateListingRequest{Parent: ex.GetName(), ListingId: "orders",
		Listing: &analyticshubpb.Listing{DisplayName: "Orders", RestrictedExportConfig: cfg,
			Source: &analyticshubpb.Listing_BigqueryDataset{BigqueryDataset: &analyticshubpb.Listing_BigQueryDatasetSource{
				Dataset: "projects/" + project + "/datasets/" + shared.DatasetID}}}})
	if err != nil {
		t.Fatalf("CreateListing: %v", err)
	}
	t.Cleanup(func() {
		_ = g.DeleteListing(context.Background(), &analyticshubpb.DeleteListingRequest{Name: l.GetName()})
	})
	return ex.GetName(), l.GetName()
}

func subscribeTo(listing, project, dataset string) *analyticshubpb.SubscribeListingRequest {
	return &analyticshubpb.SubscribeListingRequest{Name: listing,
		Destination: &analyticshubpb.SubscribeListingRequest_DestinationDataset{DestinationDataset: &analyticshubpb.DestinationDataset{
			Location: "US", DatasetReference: &analyticshubpb.DestinationDatasetReference{ProjectId: project, DatasetId: dataset}}}}
}

// covers: google.cloud.bigquery.analyticshub.v1.AnalyticsHubService/GetListing, google.cloud.bigquery.analyticshub.v1.AnalyticsHubService/ListListings, google.cloud.bigquery.analyticshub.v1.AnalyticsHubService/UpdateListing, google.cloud.bigquery.analyticshub.v1.AnalyticsHubService/SubscribeListing, google.cloud.bigquery.analyticshub.v1.AnalyticsHubService/GetSubscription, google.cloud.bigquery.analyticshub.v1.AnalyticsHubService/TestIamPermissions, google.cloud.bigquery.analyticshub.v1.AnalyticsHubService/SetIamPolicy
//
// TestIAMEnforcementAnalyticsHub (#1989), with the official BigQuery
// sharing client over gRPC and REST and a token CloudBurrow minted for a
// service account of the test's own, against an instance started with
// --iam-enforcement=on: without a role, GetListing is PERMISSION_DENIED,
// over gRPC and as a 403 over JSON; roles/analyticshub.viewer on the
// exchange's own policy, which its listings inherit, lets GetListing and
// ListListings through but not UpdateListing or SubscribeListing, and
// TestIamPermissions answers only what it holds; roles/analyticshub.subscriber
// on the listing lets SubscribeListing past analyticshub.listings.subscribe,
// which then needs bigquery.datasets.create on the subscriber's project
// (roles/bigquery.user), and with it subscribes; the subscription is the
// subscriber's, who cannot read it without analyticshub.subscriptions.get;
// a token the instance did not mint is UNAUTHENTICATED; the instance's own
// account holds every permission.
func TestIAMEnforcementAnalyticsHub(t *testing.T) {
	h := New(t)
	addr := h.Endpoint(EnvAnalyticsHub)
	h.Endpoint(EnvResourceManager)
	h.Endpoint(EnvMetadata)
	requireIAMEnforcement(t)
	pub, publisher := bigqueryClient(t, h)
	ctx := h.Context()
	owner, err := analyticshub.NewClient(ctx, grpcAs(addr, "")...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Close() })
	shared, _ := seedOrders(t, h, pub)
	exchange, listing := hubListingOf(t, h, owner, publisher, "iam_"+strings.ReplaceAll(h.Project(), "-", "_"), shared, nil)

	subProject := iamProject(t, h, "ahiam-")
	p := newIAMPrincipal(t, h, subProject, "ahiam")
	as, err := analyticshub.NewClient(ctx, grpcAs(addr, p.token)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = as.Close() })
	asREST, err := analyticshub.NewRESTClient(ctx, option.WithEndpoint("http://"+addr),
		option.WithTokenSource(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: p.token, TokenType: "Bearer"})))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = asREST.Close() })

	getListing := func() error {
		_, err := as.GetListing(ctx, &analyticshubpb.GetListingRequest{Name: listing})
		return err
	}
	denied := func(perm, resource string) string {
		return "Permission '" + perm + "' denied on resource '" + resource + "' (or it may not exist)."
	}
	wantDenied(t, "GetListing without a role", getListing(), denied("analyticshub.listings.get", listing), "analyticshub.listings.get", true)
	_, err = asREST.GetListing(ctx, &analyticshubpb.GetListingRequest{Name: listing})
	var ae *apierror.APIError
	if !errors.As(err, &ae) || ae.HTTPCode() != http.StatusForbidden || !strings.Contains(ae.Error(), "analyticshub.listings.get") {
		t.Errorf("GetListing over JSON without a role = %v; want 403 naming analyticshub.listings.get", err)
	}

	// The exchange's own policy, which the listing inherits.
	undoViewer := bindOn(t, owner.GetIamPolicy, owner.SetIamPolicy, exchange,
		&iampb.Binding{Role: "roles/analyticshub.viewer", Members: []string{"serviceAccount:" + p.email}})
	waitGranted(t, "GetListing as a viewer on the exchange", getListing)
	if _, err := asREST.GetListing(ctx, &analyticshubpb.GetListingRequest{Name: listing}); err != nil {
		t.Errorf("GetListing over JSON as a viewer: %v", err)
	}
	if _, err := as.ListListings(ctx, &analyticshubpb.ListListingsRequest{Parent: exchange}).Next(); err != nil {
		t.Errorf("ListListings as a viewer: %v", err)
	}
	_, err = as.UpdateListing(ctx, &analyticshubpb.UpdateListingRequest{Listing: &analyticshubpb.Listing{Name: listing, Description: "no"},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"description"}}})
	wantDenied(t, "UpdateListing as a viewer", err, denied("analyticshub.listings.update", listing), "analyticshub.listings.update", true)
	_, err = as.SubscribeListing(ctx, subscribeTo(listing, subProject, "iam_linked"))
	wantDenied(t, "SubscribeListing as a viewer", err, denied("analyticshub.listings.subscribe", listing), "analyticshub.listings.subscribe", true)
	held, err := as.TestIamPermissions(ctx, &iampb.TestIamPermissionsRequest{Resource: listing,
		Permissions: []string{"analyticshub.listings.get", "analyticshub.listings.update", "analyticshub.listings.subscribe"}})
	if err != nil || len(held.GetPermissions()) != 1 || held.GetPermissions()[0] != "analyticshub.listings.get" {
		t.Errorf("TestIamPermissions as a viewer = %v, %v; want only analyticshub.listings.get", held.GetPermissions(), err)
	}
	undoViewer()
	wantDenied(t, "GetListing after the exchange's binding is gone", getListing(), denied("analyticshub.listings.get", listing), "analyticshub.listings.get", true)

	// Subscriber on the listing, then BigQuery User on the subscriber's
	// project for the linked dataset.
	undoSub := bindOn(t, owner.GetIamPolicy, owner.SetIamPolicy, listing,
		&iampb.Binding{Role: "roles/analyticshub.subscriber", Members: []string{"serviceAccount:" + p.email}})
	t.Cleanup(undoSub)
	waitGranted(t, "GetListing as a subscriber on the listing", getListing)
	_, err = as.SubscribeListing(ctx, subscribeTo(listing, subProject, "iam_linked"))
	wantDenied(t, "SubscribeListing without bigquery.datasets.create", err, denied("bigquery.datasets.create", "projects/"+subProject),
		"bigquery.datasets.create", true)
	p.grant("roles/bigquery.user")
	t.Cleanup(func() { p.revoke("roles/bigquery.user") })
	var resp *analyticshubpb.SubscribeListingResponse
	waitGranted(t, "SubscribeListing as a subscriber with BigQuery User", func() error {
		var err error
		resp, err = as.SubscribeListing(ctx, subscribeTo(listing, subProject, "iam_linked"))
		return err
	})
	sub := resp.GetSubscription().GetName()
	t.Cleanup(func() {
		_, _ = owner.DeleteSubscription(context.Background(), &analyticshubpb.DeleteSubscriptionRequest{Name: sub})
	})
	_, err = as.GetSubscription(ctx, &analyticshubpb.GetSubscriptionRequest{Name: sub})
	wantDenied(t, "GetSubscription without analyticshub.subscriptions.get", err, denied("analyticshub.subscriptions.get", sub),
		"analyticshub.subscriptions.get", true)
	if _, err := owner.GetSubscription(ctx, &analyticshubpb.GetSubscriptionRequest{Name: sub}); err != nil {
		t.Errorf("GetSubscription as the instance's account: %v", err)
	}

	forged, err := analyticshub.NewClient(ctx, grpcAs(addr, "cbl_not-minted-here")...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = forged.Close() })
	if _, err := forged.GetListing(ctx, &analyticshubpb.GetListingRequest{Name: listing}); status.Code(err) != codes.Unauthenticated {
		t.Errorf("GetListing with a token the instance did not mint = %v; want UNAUTHENTICATED", err)
	}
}

// covers: bigquery.jobs.insert, bigquery.jobs.get, google.cloud.bigquery.analyticshub.v1.AnalyticsHubService/SubscribeListing
//
// TestAnalyticsHubEgressStorageReadAndQueryResults (#1989), with the
// official BigQuery and BigQuery Storage Read clients: with restricted
// export ("restrict direct table access (read api/tabledata.list) on
// linked table"), CreateReadSession of the linked table is
// PERMISSION_DENIED, while a listing without restriction reads the shared
// rows through it; with restrict_query_result ("restrict export of query
// result derived from restricted linked dataset table"), the result table
// of a query of the linked dataset is refused to a copy job and an extract
// job, while its rows are read as any result is; without
// restrict_query_result the same result is copied, as "Disable copy and
// export of shared data" "permits exports of query results".
func TestAnalyticsHubEgressStorageReadAndQueryResults(t *testing.T) {
	h := New(t)
	g, _ := analyticsHubClients(t, h)
	rc, err := bqstorage.NewBigQueryReadClient(h.Context(), option.WithEndpoint(h.Endpoint(EnvBigQueryStorage)), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rc.Close() })
	pub, project := bigqueryClient(t, h)
	sub, subProject := subscriberProject(t, h, "aheg2-")
	ctx := h.Context()
	shared, _ := seedOrders(t, h, pub)
	suffix := strings.ReplaceAll(h.Project(), "-", "_")
	_, restricted := hubListingOf(t, h, g, project, "egr2_"+suffix, shared,
		&analyticshubpb.Listing_RestrictedExportConfig{Enabled: true, RestrictQueryResult: true})
	_, dataOnly := hubListingOf(t, h, g, project, "egr3_"+suffix, shared,
		&analyticshubpb.Listing_RestrictedExportConfig{Enabled: true})
	_, open := hubListingOf(t, h, g, project, "open_"+suffix, shared, nil)
	for listing, ds := range map[string]string{restricted: "r_orders", dataOnly: "d_orders", open: "o_orders"} {
		resp, err := g.SubscribeListing(ctx, subscribeTo(listing, subProject, ds))
		if err != nil {
			t.Fatalf("SubscribeListing %s: %v", listing, err)
		}
		name := resp.GetSubscription().GetName()
		t.Cleanup(func() {
			_, _ = g.DeleteSubscription(context.Background(), &analyticshubpb.DeleteSubscriptionRequest{Name: name})
		})
	}
	session := func(ds string) (*storagepb.ReadSession, error) {
		return rc.CreateReadSession(ctx, &storagepb.CreateReadSessionRequest{Parent: "projects/" + subProject,
			ReadSession: &storagepb.ReadSession{Table: "projects/" + subProject + "/datasets/" + ds + "/tables/orders",
				DataFormat: storagepb.DataFormat_AVRO}, MaxStreamCount: 1})
	}

	// The Storage Read API.
	if _, err := session("r_orders"); status.Code(err) != codes.PermissionDenied || !strings.Contains(err.Error(), "restrict_direct_table_access") {
		t.Errorf("CreateReadSession of a restricted linked table = %v; want PERMISSION_DENIED", err)
	}
	s, err := session("o_orders")
	if err != nil {
		t.Fatalf("CreateReadSession of a linked table without restriction: %v", err)
	}
	if len(s.GetStreams()) == 0 {
		t.Fatalf("the session has no stream")
	}
	rows, err := rc.ReadRows(ctx, &storagepb.ReadRowsRequest{ReadStream: s.GetStreams()[0].GetName()})
	if err != nil {
		t.Fatal(err)
	}
	var n int64
	for {
		r, err := rows.Recv()
		if err != nil {
			break
		}
		n += r.GetRowCount()
	}
	if n != 4 {
		t.Errorf("ReadRows of the open linked table read %d rows; want the shared table's 4", n)
	}

	// The query result.
	own := sub.Dataset("own2")
	if err := own.Create(ctx, &bigquery.DatasetMetadata{Location: "US"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = own.DeleteWithContents(context.Background()) })
	result := func(ds string) *bigquery.Table {
		t.Helper()
		j, err := sub.Query("SELECT id FROM `" + subProject + "." + ds + ".orders` ORDER BY id").Run(ctx)
		if err != nil {
			t.Fatalf("query of %s: %v", ds, err)
		}
		if st, err := j.Wait(ctx); err != nil || st.Err() != nil {
			t.Fatalf("query of %s: %v, %v", ds, err, st.Err())
		}
		it, err := j.Read(ctx)
		if err != nil {
			t.Fatalf("read the result of %s: %v", ds, err)
		}
		if got := readInts(t, it); len(got) != 4 {
			t.Fatalf("the result of %s = %v", ds, got)
		}
		cfg, err := j.Config()
		if err != nil {
			t.Fatal(err)
		}
		dst := cfg.(*bigquery.QueryConfig).Dst
		if dst == nil {
			t.Fatalf("the query job of %s names no result table", ds)
		}
		return sub.DatasetInProject(dst.ProjectID, dst.DatasetID).Table(dst.TableID)
	}
	run := func(j *bigquery.Job, err error) error {
		if err == nil {
			var st *bigquery.JobStatus
			st, err = j.Wait(ctx)
			if err == nil {
				err = st.Err()
			}
		}
		return err
	}
	res := result("r_orders")
	if err := run(own.Table("copied_result").CopierFrom(res).Run(ctx)); !accessDenied(err) || !strings.Contains(err.Error(), "restrict_query_result") {
		t.Errorf("copy job of a restricted query's result = %v; want Access Denied naming restrict_query_result", err)
	}
	if err := run(res.ExtractorTo(bigquery.NewGCSReference("gs://nowhere-" + h.Project() + "/r.csv")).Run(ctx)); !accessDenied(err) {
		t.Errorf("extract job of a restricted query's result = %v; want Access Denied", err)
	}
	if err := run(own.Table("copied_open").CopierFrom(result("d_orders")).Run(ctx)); err != nil {
		t.Errorf("copy job of a query's result without restrict_query_result: %v", err)
	}
}
