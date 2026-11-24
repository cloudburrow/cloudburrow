//go:build browser

package browser

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	datatransfer "cloud.google.com/go/bigquery/datatransfer/apiv1"
	"cloud.google.com/go/bigquery/datatransfer/apiv1/datatransferpb"
	"github.com/chromedp/chromedp"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const envDataTransfer = "CLOUDBURROW_TEST_DATATRANSFER"

// TestBigQueryScheduledQueryThroughThePage (#1948), in the emulators shard:
// Scheduled queries' Create scheduled query makes a scheduled_query
// transfer config, which the official client reads back; Data transfers
// lists it too; its page's Run transfer now runs it on the local BigQuery,
// and its Run history shows the run SUCCEEDED with the job's message.
// The create page has Google's sections and labels (#2094: Transfer config
// name's Display name, Schedule options' Repeat frequency, Destination
// settings' Dataset), its Repeat frequency Hours every 6 is the schedule
// "every 6 hours", and the page is walked by keyboard; the transfer's page
// has Google's Disable.
func TestBigQueryScheduledQueryThroughThePage(t *testing.T) {
	needService(t, "bigquery")
	c, err := datatransfer.NewClient(context.Background(), option.WithEndpoint(endpoint(t, envDataTransfer)),
		option.WithoutAuthentication(), option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	p := open(t)
	project := servedProject(t)
	ds := fmt.Sprintf("browser_dts_%d", time.Now().UnixNano()%1e12)
	q := "?project=" + url.QueryEscape(project)
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/bigquery"+q, `{"datasetId":"`+ds+`"}`); code != http.StatusOK {
		t.Fatalf("create a dataset through the console API = %d: %s", code, body)
	}
	t.Cleanup(func() { consoleDo(t, http.MethodDelete, "/api/resources/bigquery"+q+"&name="+ds, "") })

	p.navigate("/bigquery-scheduled-queries/create" + q)
	p.waitFor(`document.querySelector("#view #f-query") !== null`)
	var labels string
	p.eval(`[...document.querySelectorAll("#view form label, #view form legend, #view form h2, #view form h3, #view form .form-section-title, #view form .create-step-title")].map((l) => l.textContent.trim()).join("|")`, &labels)
	for _, want := range []string{"Transfer config name", "Display name", "Schedule options", "Repeat frequency", "Destination settings", "Dataset", "Data source details"} {
		if !strings.Contains(labels, want) {
			t.Errorf("Create scheduled query has no %q: %s", want, labels)
		}
	}
	var repeats []string
	p.eval(`[...document.querySelectorAll("#view #f-repeat option")].map((o) => o.textContent.trim())`, &repeats)
	if strings.Join(repeats, "|") != "Hours|Days|Weeks|Months|Custom|On-demand" {
		t.Errorf("Repeat frequency offers %v, want Google's Hours, Days, Weeks, Months, Custom, On-demand", repeats)
	}
	p.run(chromedp.Focus(chromedp.CSS(`#view #f-display_name`)))
	for _, fl := range p.keyboardFailures() {
		t.Errorf("Create scheduled query: %s", fl)
	}
	setField(p, "#view", "f-display_name", "browser nightly")
	setField(p, "#view", "f-repeat", "Hours")
	setField(p, "#view", "f-frequency", "6")
	setField(p, "#view", "f-dataset", ds)
	setField(p, "#view", "f-query", "SELECT @run_date AS d, 1 AS n")
	setField(p, "#view", "f-table", `out_{run_time|"%Y%m%d"}`)
	setField(p, "#view", "f-write_disposition", "WRITE_TRUNCATE")
	p.run(chromedp.Click(chromedp.CSS(`#view button[type="submit"]`)))
	p.waitFor(`location.pathname === "/bigquery-scheduled-queries" && document.querySelector("#view tbody tr") !== null`)

	var cfg *datatransferpb.TransferConfig
	it := c.ListTransferConfigs(context.Background(), &datatransferpb.ListTransferConfigsRequest{Parent: "projects/" + project})
	for {
		got, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListTransferConfigs: %v", err)
		}
		if got.GetDestinationDatasetId() == ds {
			cfg = got
		}
	}
	if cfg == nil || cfg.GetDisplayName() != "browser nightly" || cfg.GetDataSourceId() != "scheduled_query" ||
		cfg.GetParams().GetFields()["write_disposition"].GetStringValue() != "WRITE_TRUNCATE" || cfg.GetSchedule() != "every 6 hours" {
		t.Fatalf("the config made in the browser: %v", cfg)
	}
	t.Cleanup(func() {
		_ = c.DeleteTransferConfig(context.Background(), &datatransferpb.DeleteTransferConfigRequest{Name: cfg.GetName()})
	})

	p.navigate("/bigquery-transfers" + q)
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("browser nightly"))`)

	p.navigate("/bigquery-scheduled-queries" + q)
	// The row is named by the config's ID, the last segment of its name.
	id := cfg.GetName()[strings.LastIndex(cfg.GetName(), "/")+1:]
	p.clickExpr(fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr a")].find((a) => a.textContent.trim() === %q)`, id))
	p.waitFor(`document.querySelector("#view h1") !== null && document.body.textContent.includes("Run history")`)
	var detail string
	p.eval(`location.pathname + location.search`, &detail)
	var actions string
	p.eval(`[...document.querySelectorAll("#view .page-actions button")].map((b) => b.textContent.trim()).join("|")`, &actions)
	if !strings.Contains("|"+actions+"|", "|Disable|") {
		t.Errorf("the transfer's page actions are %s, want Google's Disable", actions)
	}
	p.clickText("#view .page-actions button", "Run transfer now")
	deadline := time.Now().Add(90 * time.Second)
	var history string
	for time.Now().Before(deadline) {
		p.navigate(detail)
		p.waitFor(`document.body.textContent.includes("Run history")`)
		p.eval(`document.querySelector("#view").textContent`, &history)
		if strings.Contains(history, "SUCCEEDED") {
			break
		}
		time.Sleep(time.Second)
	}
	if !strings.Contains(history, "SUCCEEDED") || !strings.Contains(history, "Transfer run completed successfully.") {
		t.Fatalf("Run history after Run transfer now: %s", lastChars(history, 600))
	}
}

// TestBigQueryTransferScreensMeetWCAG (#1948), in the emulators shard: the
// Data transfers and Scheduled queries lists and their Create pages pass
// the console's WCAG 2.1 AA audit.
func TestBigQueryTransferScreensMeetWCAG(t *testing.T) {
	needService(t, "bigquery")
	project := servedProject(t)
	var pages []a11yPage
	pages = append(pages, listAndCreate("Data transfers", "/bigquery-transfers", project)...)
	pages = append(pages, listAndCreate("Scheduled queries", "/bigquery-scheduled-queries", project)...)
	auditScreens(t, pages)
}

func lastChars(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
