//go:build browser

package browser

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	analyticshub "cloud.google.com/go/bigquery/analyticshub/apiv1"
	"cloud.google.com/go/bigquery/analyticshub/apiv1/analyticshubpb"
	"github.com/chromedp/chromedp"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
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
