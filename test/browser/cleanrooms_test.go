//go:build browser

package browser

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	analyticshub "cloud.google.com/go/bigquery/analyticshub/apiv1"
	"cloud.google.com/go/bigquery/analyticshub/apiv1/analyticshubpb"
	"github.com/chromedp/chromedp"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// TestBigQueryCleanRoomsThroughThePage (#2101), in the emulators shard:
// BigQuery's Data clean rooms page creates a clean room, which the
// official client reads back as a dcr_exchange_config exchange; the clean
// room's Add data makes a view with an aggregation threshold analysis
// rule and lists it, egress restricted; its Templates tab's Create
// Template, Submit for review and Approve take a query template to
// APPROVED, its table-valued function made and listed; Subscribe makes
// the one linked dataset, which the official BigQuery client queries: a
// query without the clause is refused, the clause drops the small group,
// and the template runs. Every screen meets WCAG as the others do.
func TestBigQueryCleanRoomsThroughThePage(t *testing.T) {
	needService(t, "bigquery")
	ctx := context.Background()
	c, err := analyticshub.NewClient(ctx, option.WithEndpoint(endpoint(t, envAnalyticsHub)),
		option.WithoutAuthentication(), option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	project := servedProject(t)
	bqAddr := strings.TrimSpace(os.Getenv(envBigQuery))
	if bqAddr == "" {
		t.Skipf("%s is not set", envBigQuery)
	}
	if !strings.Contains(bqAddr, "://") {
		bqAddr = "http://" + bqAddr
	}
	bq, err := bigquery.NewClient(ctx, project, option.WithEndpoint(bqAddr), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bq.Close() })
	n := time.Now().UnixNano() % 1e12
	ds, id := fmt.Sprintf("browser_dcr_%d", n), fmt.Sprintf("browser_room_%d", n)
	run := func(sql string) ([]string, error) {
		it, err := bq.Query(sql).Read(ctx)
		if err != nil {
			return nil, err
		}
		var out []string
		for {
			var row []bigquery.Value
			if err := it.Next(&row); err == iterator.Done {
				return out, nil
			} else if err != nil {
				return nil, err
			}
			parts := make([]string, len(row))
			for i, v := range row {
				parts[i] = fmt.Sprint(v)
			}
			out = append(out, strings.Join(parts, " "))
		}
	}
	dataset := bq.Dataset(ds)
	if err := dataset.Create(ctx, &bigquery.DatasetMetadata{Location: "US"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dataset.DeleteWithContents(context.Background()) })
	if _, err := run("CREATE TABLE `" + project + "." + ds + ".visits` AS SELECT * FROM UNNEST([STRUCT('a' AS shop, 'u1' AS customer), " +
		"('a', 'u2'), ('a', 'u3'), ('b', 'u1')])"); err != nil {
		t.Fatal(err)
	}
	room := "projects/" + project + "/locations/us/dataExchanges/" + id
	t.Cleanup(func() {
		_ = c.DeleteDataExchange(context.Background(), &analyticshubpb.DeleteDataExchangeRequest{Name: room})
	})
	q := "?project=" + url.QueryEscape(project)

	p := open(t)
	// Clean rooms are reached from the sharing page, as Google's docs
	// reach them ("go to the Sharing (Analytics Hub) page"): its Clean
	// rooms tab, with Analytics Hub marked in BigQuery's menu, which has
	// no item of their own.
	p.navigate("/bigquery-sharing" + q)
	p.waitFor(`document.querySelector("#view .page-tabs") !== null`)
	p.clickText("#view .page-tabs a", "Clean rooms")
	p.waitFor(`location.pathname === "/bigquery-clean-rooms"`)
	var marked string
	p.eval(`document.querySelector("#product-nav a[aria-current=page]").textContent`, &marked)
	if marked != "Analytics Hub" {
		t.Errorf("on the Clean rooms tab BigQuery's menu marks %q, want Analytics Hub", marked)
	}
	p.navigate("/bigquery-clean-rooms/create" + q)
	p.waitFor(`document.querySelector("#view #f-id") !== null`)
	setField(p, "#view", "f-id", id)
	setField(p, "#view", "f-displayName", "Browser clean room")
	setField(p, "#view", "f-primaryContact", "dcr@example.com")
	p.run(chromedp.Click(chromedp.CSS(`#view button[type="submit"]`)))
	p.waitFor(`document.querySelector("#view tbody tr") !== null`)
	if e, err := c.GetDataExchange(ctx, &analyticshubpb.GetDataExchangeRequest{Name: room}); err != nil ||
		e.GetSharingEnvironmentConfig().GetDcrExchangeConfig() == nil || e.GetDisplayName() != "Browser clean room" {
		t.Fatalf("the clean room made in the browser: %v, %v", e, err)
	}

	roomPage := "/bigquery-clean-rooms/" + url.PathEscape("us/"+id) + q
	p.navigate(roomPage)
	p.pageAction("Add data", map[string]string{"dataset": "projects/" + project + "/datasets/" + ds, "table": "visits",
		"view": "visits_view", "rule": "Aggregation threshold", "privacyUnit": "customer", "threshold": "2"}, "")
	l, err := c.GetListing(ctx, &analyticshubpb.GetListingRequest{Name: room + "/listings/visits_view"})
	if err != nil || !l.GetRestrictedExportConfig().GetEnabled() || len(l.GetBigqueryDataset().GetSelectedResources()) != 1 {
		t.Fatalf("the listing Add data made: %v, %v", l, err)
	}

	p.navigate(roomPage)
	p.pageAction("Create Template", map[string]string{"id": "per_shop", "displayName": "Customers per shop",
		"definition": "per_shop(min_customers INT64) AS (SELECT shop, COUNT(DISTINCT customer) AS customers FROM `" + project + "." + ds +
			".visits` GROUP BY shop HAVING COUNT(DISTINCT customer) >= GREATEST(min_customers, 2))"}, "")
	p.navigate(roomPage)
	p.clickText("#view [role=tab]", "Templates")
	p.waitFor(`document.querySelector("#view").textContent.includes("Customers per shop") && document.querySelector("#view").textContent.includes("Draft")`)
	p.navigate(roomPage)
	p.pageAction("Submit for review", map[string]string{"template": "per_shop"}, "")
	p.navigate(roomPage)
	p.pageAction("Approve", map[string]string{"template": "per_shop", "dataset": "projects/" + project + "/datasets/" + ds}, "")
	if tm, err := c.GetQueryTemplate(ctx, &analyticshubpb.GetQueryTemplateRequest{Name: room + "/queryTemplates/per_shop"}); err != nil ||
		tm.GetState() != analyticshubpb.QueryTemplate_APPROVED {
		t.Fatalf("after Approve: %v, %v", tm, err)
	}

	p.navigate(roomPage)
	p.pageAction("Subscribe", map[string]string{"subscription": "browser_sub"}, "")
	sub := "projects/" + project + "/locations/us/subscriptions/browser_sub"
	if s, err := c.GetSubscription(ctx, &analyticshubpb.GetSubscriptionRequest{Name: sub}); err != nil || s.GetDataExchange() != room {
		t.Fatalf("the subscription Subscribe made: %v, %v", s, err)
	}
	t.Cleanup(func() {
		if op, err := c.DeleteSubscription(context.Background(), &analyticshubpb.DeleteSubscriptionRequest{Name: sub}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	linked := "`" + project + "." + id + "."
	if _, err := run("SELECT shop, COUNT(*) FROM " + linked + "visits_view` GROUP BY shop"); err == nil ||
		!strings.Contains(err.Error(), "SELECT WITH AGGREGATION_THRESHOLD") {
		t.Errorf("a query of the linked view without the clause = %v", err)
	}
	if rows, err := run("SELECT WITH AGGREGATION_THRESHOLD shop, COUNT(DISTINCT customer) FROM " + linked + "visits_view` GROUP BY shop"); err != nil ||
		strings.Join(rows, ",") != "a 3" {
		t.Errorf("SELECT WITH AGGREGATION_THRESHOLD = %v, %v; want a 3 alone", rows, err)
	}
	if rows, err := run("SELECT * FROM " + linked + "per_shop`(2)"); err != nil || strings.Join(rows, ",") != "a 3" {
		t.Errorf("the query template = %v, %v; want a 3", rows, err)
	}

	auditScreens(t, []a11yPage{
		screen("Data clean rooms", "/bigquery-clean-rooms", project),
		screen("Create clean room", "/bigquery-clean-rooms/create", project),
		screen("a data clean room", "/bigquery-clean-rooms/"+url.PathEscape("us/"+id), project),
		screen("a data clean room's Templates", "/bigquery-clean-rooms/"+url.PathEscape("us/"+id)+"?tab=templates", project),
		screen("a data clean room's Subscriptions", "/bigquery-clean-rooms/"+url.PathEscape("us/"+id)+"?tab=subscriptions", project),
	})
}
