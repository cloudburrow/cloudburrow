//go:build browser

package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	analyticshub "cloud.google.com/go/bigquery/analyticshub/apiv1"
	"cloud.google.com/go/bigquery/analyticshub/apiv1/analyticshubpb"
	"cloud.google.com/go/iam/apiv1/iampb"
	"github.com/chromedp/chromedp"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

const envAnalyticsHub = "CLOUDBURROW_TEST_ANALYTICSHUB"

// TestBigQueryAnalyticsHubThroughThePage (#1960), in the emulators shard:
// BigQuery's Analytics Hub page creates an exchange, which the official
// client reads back; the exchange's page creates a listing of a dataset with
// the data egress options, subscribes to it (the linked dataset is made),
// lists the subscription ACTIVE and revokes it.
func TestBigQueryAnalyticsHubThroughThePage(t *testing.T) {
	needService(t, "bigquery")
	ctx := context.Background()
	c, err := analyticshub.NewClient(ctx, option.WithEndpoint(endpoint(t, envAnalyticsHub)),
		option.WithoutAuthentication(), option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	p := open(t)
	project := servedProject(t)
	n := time.Now().UnixNano() % 1e12
	ds, linked, id := fmt.Sprintf("browser_ah_%d", n), fmt.Sprintf("browser_ah_linked_%d", n), fmt.Sprintf("browser_x_%d", n)
	q := "?project=" + url.QueryEscape(project)
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/bigquery"+q, `{"datasetId":"`+ds+`"}`); code != http.StatusOK {
		t.Fatalf("create a dataset through the console API = %d: %s", code, body)
	}
	t.Cleanup(func() { consoleDo(t, http.MethodDelete, "/api/resources/bigquery"+q+"&name="+ds, "") })
	exchange := "projects/" + project + "/locations/us/dataExchanges/" + id
	t.Cleanup(func() {
		_ = c.DeleteDataExchange(context.Background(), &analyticshubpb.DeleteDataExchangeRequest{Name: exchange})
	})

	p.navigate("/bigquery-sharing/create" + q)
	p.waitFor(`document.querySelector("#view #f-id") !== null`)
	setField(p, "#view", "f-id", id)
	setField(p, "#view", "f-displayName", "Browser exchange")
	setField(p, "#view", "f-primaryContact", "data@example.com")
	p.run(chromedp.Click(chromedp.CSS(`#view button[type="submit"]`)))
	p.waitFor(`document.querySelector("#view tbody tr") !== null`)
	if e, err := c.GetDataExchange(ctx, &analyticshubpb.GetDataExchangeRequest{Name: exchange}); err != nil || e.GetDisplayName() != "Browser exchange" {
		t.Fatalf("the exchange made in the browser: %v, %v", e, err)
	}

	p.navigate("/bigquery-sharing" + q)
	p.clickText("#view tbody a", "us/"+id)
	p.waitFor(`document.body.textContent.includes("Listings")`)
	p.clickText("#view .page-actions button", "Create listing")
	p.waitFor(`document.querySelector(".modal.is-open #f-dataset") !== null`)
	p.setField(".modal.is-open #f-id", "orders")
	p.setField(".modal.is-open #f-displayName", "Orders")
	p.setField(".modal.is-open #f-dataset", "projects/"+project+"/datasets/"+ds)
	setField(p, ".modal.is-open", "f-restrict", "true")
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal") === null`)
	l, err := c.GetListing(ctx, &analyticshubpb.GetListingRequest{Name: exchange + "/listings/orders"})
	if err != nil || !l.GetRestrictedExportConfig().GetEnabled() || l.GetBigqueryDataset().GetDataset() != "projects/"+project+"/datasets/"+ds {
		t.Fatalf("the listing made in the browser: %v, %v", l, err)
	}

	p.waitFor(`[...document.querySelectorAll("#view .page-actions button")].some((b) => b.textContent.includes("Subscribe"))`)
	p.clickText("#view .page-actions button", "Subscribe")
	p.waitFor(`document.querySelector(".modal.is-open #f-dataset") !== null`)
	p.setField(".modal.is-open #f-dataset", linked)
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal") === null`)
	p.clickText("#view [role=tab]", "Subscriptions")
	p.waitFor(fmt.Sprintf(`document.querySelector("#view").textContent.includes(%q) && document.querySelector("#view").textContent.includes("ACTIVE")`, project+"/datasets/"+linked))

	p.clickText("#view .page-actions button", "Revoke subscription")
	p.waitFor(`document.querySelector(".modal.is-open #f-subscription") !== null`)
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal") === null`)
	subs, err := c.ListSharedResourceSubscriptions(ctx, &analyticshubpb.ListSharedResourceSubscriptionsRequest{Resource: exchange + "/listings/orders"}).Next()
	if err != nil || subs.GetState() != analyticshubpb.Subscription_STATE_INACTIVE {
		t.Fatalf("after Revoke subscription: %v, %v; want INACTIVE", subs, err)
	}
}

// TestBigQueryAnalyticsHubEditThroughThePage (#1989), in the emulators
// shard: on an exchange's page Edit exchange changes what the official
// client reads; a listing's own page opens from the Listings tab and Edit
// listing turns on the data egress options; the listing's Permissions tab
// grants a role the service then holds; My subscriptions lists the
// subscription the client made, and Refresh subscription and Delete
// subscription act on it; and every screen meets WCAG as the others do.
func TestBigQueryAnalyticsHubEditThroughThePage(t *testing.T) {
	needService(t, "bigquery")
	ctx := context.Background()
	c, err := analyticshub.NewClient(ctx, option.WithEndpoint(endpoint(t, envAnalyticsHub)),
		option.WithoutAuthentication(), option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	project := servedProject(t)
	n := time.Now().UnixNano() % 1e12
	ds, linked, id := fmt.Sprintf("browser_ahe_%d", n), fmt.Sprintf("browser_ahe_linked_%d", n), fmt.Sprintf("browser_xe_%d", n)
	q := "?project=" + url.QueryEscape(project)
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/bigquery"+q, `{"datasetId":"`+ds+`"}`); code != http.StatusOK {
		t.Fatalf("create a dataset through the console API = %d: %s", code, body)
	}
	t.Cleanup(func() { consoleDo(t, http.MethodDelete, "/api/resources/bigquery"+q+"&name="+ds, "") })
	exchange := "projects/" + project + "/locations/us/dataExchanges/" + id
	if _, err := c.CreateDataExchange(ctx, &analyticshubpb.CreateDataExchangeRequest{Parent: "projects/" + project + "/locations/us",
		DataExchangeId: id, DataExchange: &analyticshubpb.DataExchange{DisplayName: "Before"}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = c.DeleteDataExchange(context.Background(), &analyticshubpb.DeleteDataExchangeRequest{Name: exchange})
	})
	listing := exchange + "/listings/orders"
	if _, err := c.CreateListing(ctx, &analyticshubpb.CreateListingRequest{Parent: exchange, ListingId: "orders",
		Listing: &analyticshubpb.Listing{DisplayName: "Orders", Source: &analyticshubpb.Listing_BigqueryDataset{
			BigqueryDataset: &analyticshubpb.Listing_BigQueryDatasetSource{Dataset: "projects/" + project + "/datasets/" + ds}}}}); err != nil {
		t.Fatal(err)
	}
	op, err := c.SubscribeListing(ctx, &analyticshubpb.SubscribeListingRequest{Name: listing,
		Destination: &analyticshubpb.SubscribeListingRequest_DestinationDataset{DestinationDataset: &analyticshubpb.DestinationDataset{
			Location: "US", DatasetReference: &analyticshubpb.DestinationDatasetReference{ProjectId: project, DatasetId: linked}}}})
	if err != nil {
		t.Fatalf("SubscribeListing: %v", err)
	}
	sub := op.GetSubscription().GetName()
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/bigquery"+q+"&name="+linked, "")
	})

	p := open(t)
	exPage := "/bigquery-sharing/" + url.PathEscape("us/"+id) + q
	p.navigate(exPage)
	p.pageAction("Edit exchange", map[string]string{"displayName": "After", "documentation": "Read me", "public": "true"}, "")
	if e, err := c.GetDataExchange(ctx, &analyticshubpb.GetDataExchangeRequest{Name: exchange}); err != nil || e.GetDisplayName() != "After" ||
		e.GetDocumentation() != "Read me" || e.GetDiscoveryType() != analyticshubpb.DiscoveryType_DISCOVERY_TYPE_PUBLIC {
		t.Fatalf("after Edit exchange: %v, %v", e, err)
	}

	p.navigate(exPage)
	p.clickText("#view tbody a", "orders")
	p.waitFor(`document.body.textContent.includes("Shared dataset")`)
	p.pageAction("Edit listing", map[string]string{"description": "edited", "restrict": "true", "restrictQuery": "true"}, "")
	if l, err := c.GetListing(ctx, &analyticshubpb.GetListingRequest{Name: listing}); err != nil || l.GetDescription() != "edited" ||
		!l.GetRestrictedExportConfig().GetEnabled() || !l.GetRestrictedExportConfig().GetRestrictQueryResult() {
		t.Fatalf("after Edit listing: %v, %v", l, err)
	}

	listingPage := "/bigquery-sharing/" + url.PathEscape("us/"+id) + "/orders" + q
	p.navigate(listingPage)
	p.clickText("#view .tab-strip .tab", "Permissions")
	p.waitFor(`document.querySelector("#permissions-note") !== null`)
	p.clickText("#view #grant-access", "Grant access")
	p.waitFor(`document.querySelector(".modal.is-open #f-members") !== null`)
	p.eval(`(() => { const q = (s) => document.querySelector(".modal.is-open " + s);
		q("#f-members").value = "user:ada@example.com"; q("#f-members").dispatchEvent(new Event("input", { bubbles: true }));
		q("#f-role").value = "roles/analyticshub.subscriber"; q("#f-role").dispatchEvent(new Event("input", { bubbles: true })); return true; })()`, nil)
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal") === null`)
	pol, err := c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: listing})
	b, _ := json.Marshal(pol.GetBindings())
	if err != nil || !strings.Contains(string(b), "user:ada@example.com") || !strings.Contains(string(b), "roles/analyticshub.subscriber") {
		t.Errorf("GetIamPolicy after Grant access = %s, %v", b, err)
	}

	mine := "/bigquery-sharing/subscriptions" + q
	p.navigate(mine)
	p.waitFor(fmt.Sprintf(`document.querySelector("#view").textContent.includes(%q) && document.querySelector("#view h1").textContent === "My subscriptions"`, sub))
	p.pageAction("Refresh subscription", map[string]string{"subscription": sub}, "")
	p.navigate(mine)
	p.pageAction("Delete subscription", map[string]string{"subscription": sub}, "")
	if _, err := c.GetSubscription(ctx, &analyticshubpb.GetSubscriptionRequest{Name: sub}); status.Code(err) != codes.NotFound {
		t.Errorf("GetSubscription after Delete subscription = %v; want NOT_FOUND", err)
	}

	auditScreens(t, []a11yPage{
		screen("Analytics Hub exchanges", "/bigquery-sharing", project),
		screen("an Analytics Hub exchange", "/bigquery-sharing/"+url.PathEscape("us/"+id), project),
		screen("an Analytics Hub exchange's Permissions", "/bigquery-sharing/"+url.PathEscape("us/"+id)+"?tab=permissions", project),
		screen("an Analytics Hub listing", "/bigquery-sharing/"+url.PathEscape("us/"+id)+"/orders", project),
		screen("an Analytics Hub listing's Permissions", "/bigquery-sharing/"+url.PathEscape("us/"+id)+"/orders?tab=permissions", project),
		screen("My subscriptions", "/bigquery-sharing/subscriptions", project),
	})
}
