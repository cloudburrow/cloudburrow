//go:build browser

package browser

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	reservation "cloud.google.com/go/bigquery/reservation/apiv1"
	"cloud.google.com/go/bigquery/reservation/apiv1/reservationpb"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
	"google.golang.org/api/option"
)

// TestBigQueryCapacityManagementThroughThePage (#2193): BigQuery's
// Capacity management page, as Google's reservations docs give it
// (https://docs.cloud.google.com/bigquery/docs/reservations-tasks and
// reservations-assignments): Create reservation (an Enterprise edition
// reservation of 100 slots in the US), the reservation's page and its
// Create assignment with the CONTINUOUS job type for the project; then
// the query editor's Edit > Query mode > Continuous query runs a
// continuous INSERT on it, answered with the job running, whose rows
// reach the destination table as they arrive. The job is cancelled, the
// assignment deleted from the page, and the reservation with Delete.
// WCAG 2.1 AA and the keyboard walk of the list, the create page and the
// reservation's page.
func TestBigQueryCapacityManagementThroughThePage(t *testing.T) {
	needService(t, "bigquery")
	ctx := context.Background()
	project := servedProject(t)
	q := "?project=" + url.QueryEscape(project)
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
	rc, err := reservation.NewRESTClient(ctx, option.WithEndpoint(bqAddr), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rc.Close() })

	n := time.Now().UnixNano() % 1e12
	id, ds := fmt.Sprintf("browser-cq-%d", n), fmt.Sprintf("browser_cq_%d", n)
	resv := "projects/" + project + "/locations/US/reservations/" + id
	t.Cleanup(func() {
		it := rc.ListAssignments(context.Background(), &reservationpb.ListAssignmentsRequest{Parent: resv})
		for a, err := it.Next(); err == nil; a, err = it.Next() {
			_ = rc.DeleteAssignment(context.Background(), &reservationpb.DeleteAssignmentRequest{Name: a.GetName()})
		}
		_ = rc.DeleteReservation(context.Background(), &reservationpb.DeleteReservationRequest{Name: resv})
	})
	dataset := bq.Dataset(ds)
	if err := dataset.Create(ctx, &bigquery.DatasetMetadata{Location: "US"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dataset.DeleteWithContents(context.Background()) })
	dml := func(sql string) {
		t.Helper()
		job, err := bq.Query(sql).Run(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if st, err := job.Wait(ctx); err != nil || st.Err() != nil {
			t.Fatalf("%s: %v, %v", sql, err, st)
		}
	}
	dml("CREATE TABLE `" + project + "." + ds + ".rides` (ride_id STRING, meters INT64)")
	dml("CREATE TABLE `" + project + "." + ds + ".long_rides` (ride_id STRING, meters INT64)")

	p := open(t)
	// Create reservation, on a page of its own.
	p.navigate("/bigquery-capacity/create" + q)
	p.waitFor(`document.querySelector("#view #f-id") !== null`)
	setField(p, "#view", "f-id", id)
	setField(p, "#view", "f-location", "US")
	setField(p, "#view", "f-edition", "ENTERPRISE")
	setField(p, "#view", "f-max", "100")
	setField(p, "#view", "f-baseline", "50")
	p.run(chromedp.Click(chromedp.CSS(`#view button[type="submit"]`)))
	p.waitFor(fmt.Sprintf(`document.querySelector("#view") !== null && document.querySelector("#view").textContent.includes(%q)`, id))
	rv, err := rc.GetReservation(ctx, &reservationpb.GetReservationRequest{Name: resv})
	if err != nil || rv.GetEdition() != reservationpb.Edition_ENTERPRISE || rv.GetSlotCapacity() != 50 || rv.GetAutoscale().GetMaxSlots() != 50 {
		t.Fatalf("the reservation made in the browser: %v, %v", rv, err)
	}

	page := "/bigquery-capacity/" + url.PathEscape("US/"+id) + q
	p.navigate(page)
	p.pageAction("Create assignment", map[string]string{"assigneeType": "projects", "assignee": project, "jobType": "CONTINUOUS"}, "")
	p.navigate(page)
	p.clickText("#view [role=tab]", "Assignments")
	p.waitFor(`document.querySelector("#view").textContent.includes("CONTINUOUS")`)

	// Edit > Query mode > Continuous query runs on it.
	p.navigate("/bigquery-studio" + q)
	p.waitFor(`document.querySelector(".bq-home") !== null && document.querySelector(".bq-tree") !== null`)
	p.clickText(".bq-new-query", "SQL query")
	p.waitFor(`[...document.querySelectorAll(".bq-editor")].some((e) => e.offsetParent !== null)`)
	p.eval(`[...document.querySelectorAll(".bq-editor")].find((e) => e.offsetParent !== null).id = "cq-editor"; true`, nil)
	p.eval(`[...document.querySelectorAll(".bq-toolbar .bq-menu-button[aria-label=Edit]")].find((b) => b.offsetParent !== null).id = "cq-edit"; true`, nil)
	p.run(chromedp.Focus(chromedp.CSS("#cq-edit")), chromedp.KeyEvent(kb.Enter))
	p.waitFor(`document.activeElement && document.activeElement.textContent === "Query settings"`)
	p.run(chromedp.KeyEvent(kb.ArrowDown), chromedp.KeyEvent(kb.ArrowRight), chromedp.KeyEvent(kb.ArrowDown), chromedp.KeyEvent(kb.Enter))
	p.waitFor(`document.querySelector(".modal.is-open") !== null`)
	p.pressConfirm("Confirm")
	p.waitFor(`[...document.querySelectorAll(".bq-mode")].some((m) => m.offsetParent !== null && m.textContent === "Continuous query")`)
	p.setField("#cq-editor", "INSERT INTO `"+project+"."+ds+".long_rides` SELECT ride_id, meters FROM `"+project+"."+ds+".rides` WHERE meters > 1000")
	p.clickExpr(`[...document.querySelectorAll(".bq-toolbar button.primary")].find((b) => b.textContent === "Run" && b.offsetParent !== null)`)
	p.waitFor(`document.querySelector(".modal.is-open") !== null`)
	p.clickText(".modal.is-open button", "Run statement")
	p.waitForWithin(`[...document.querySelectorAll(".bq-results")].some((r) => r.offsetParent !== null && (r.textContent.includes("Continuous query ") || r.querySelector(".form-error")))`, 2*time.Minute)
	var msg string
	p.eval(`[...document.querySelectorAll(".bq-results")].find((r) => r.offsetParent !== null).textContent`, &msg)
	m := regexp.MustCompile(`Continuous query (\S+) \(RUNNING\)`).FindStringSubmatch(msg)
	if m == nil {
		t.Fatalf("a continuous query's Run: %q, want its job running", msg)
	}
	job, err := bq.JobFromIDLocation(ctx, m[1], "US")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = job.Cancel(context.Background()) })
	if st, err := job.Status(ctx); err != nil || st.State != bigquery.Running {
		t.Fatalf("the continuous job: %v, %v; want RUNNING", st, err)
	}
	dml("INSERT INTO `" + project + "." + ds + ".rides` VALUES ('short', 10), ('long', 5000)")
	deadline := time.Now().Add(90 * time.Second)
	for {
		it, err := bq.Query("SELECT ride_id FROM `" + project + "." + ds + ".long_rides`").Read(ctx)
		var got []string
		if err == nil {
			for {
				var row []bigquery.Value
				if it.Next(&row) != nil {
					break
				}
				got = append(got, fmt.Sprint(row[0]))
			}
		}
		if strings.Join(got, ",") == "long" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("long_rides holds %v, %v; want the long ride the continuous query wrote", got, err)
		}
		time.Sleep(2 * time.Second)
	}
	if err := job.Cancel(ctx); err != nil {
		t.Fatal(err)
	}

	auditScreens(t, []a11yPage{
		screen("Capacity management", "/bigquery-capacity", project),
		screen("Create reservation", "/bigquery-capacity/create", project),
		screen("a reservation", "/bigquery-capacity/"+url.PathEscape("US/"+id), project),
		screen("a reservation's Assignments", "/bigquery-capacity/"+url.PathEscape("US/"+id)+"?tab=assignments", project),
		screen("Slot commitments", "/bigquery-commitments", project),
	})

	// The assignment, then the reservation, deleted from the page.
	p.navigate(page)
	p.pageAction("Delete assignment", nil, "US/"+id)
	if it := rc.ListAssignments(ctx, &reservationpb.ListAssignmentsRequest{Parent: resv}); func() bool { _, err := it.Next(); return err == nil }() {
		t.Errorf("an assignment is left after Delete assignment")
	}
	p.navigate(page)
	p.clickAction("Delete", "US/"+id)
	p.waitFor(`location.pathname === "/bigquery-capacity"`)
	if _, err := rc.GetReservation(ctx, &reservationpb.GetReservationRequest{Name: resv}); err == nil {
		t.Errorf("the reservation is left after Delete")
	}
}
