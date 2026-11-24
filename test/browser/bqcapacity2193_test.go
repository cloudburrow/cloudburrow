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
	"cloud.google.com/go/iam/apiv1/iampb"
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
	p.pageAction("Delete assignment", nil, "")
	if it := rc.ListAssignments(ctx, &reservationpb.ListAssignmentsRequest{Parent: resv}); func() bool { _, err := it.Next(); return err == nil }() {
		t.Errorf("an assignment is left after Delete assignment")
	}
	p.navigate("/bigquery-capacity" + q)
	p.openRowDelete("US/"+id, "Delete")
	p.confirmByName("US/" + id)
	p.waitFor(fmt.Sprintf(`![...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q))`, id))
	if _, err := rc.GetReservation(ctx, &reservationpb.GetReservationRequest{Name: resv}); err == nil {
		t.Errorf("the reservation is left after Delete")
	}
}

// TestBigQueryCapacityPagesThroughThePage (#2193): the rest of Capacity
// management and BI Engine, so every Reservation API method the instance
// serves is reachable from the console. In us-central1:
//   - Reservation groups: Create reservation group; a reservation joins it
//     with its Edit;
//   - Slot commitments: Create commitment (100 Enterprise slots for a year), its page,
//     Edit renewal plan, Split (50) and Merge back;
//   - a reservation's Upgrade edition: refused while its commitment stays
//     behind, done with it (Enterprise Plus); its Edit then sets a
//     secondary location and Failover swaps the two;
//   - an assignment's page: Edit (Gemini in BigQuery), Grant access on its
//     Permissions tab and on the reservation's, Move to another reservation;
//   - BI Engine: Create reservation (2 GiB) and Delete.
//
// WCAG 2.1 AA and the keyboard walk of the pages.
func TestBigQueryCapacityPagesThroughThePage(t *testing.T) {
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
	rc, err := reservation.NewRESTClient(ctx, option.WithEndpoint(bqAddr), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rc.Close() })
	n := time.Now().UnixNano() % 1e12
	const loc = "us-central1"
	parent := "projects/" + project + "/locations/" + loc
	gid, aid, bid := fmt.Sprintf("bg-%d", n), fmt.Sprintf("ba-%d", n), fmt.Sprintf("bb-%d", n)
	t.Cleanup(func() {
		bg := context.Background()
		for _, r := range []string{aid, bid} {
			it := rc.ListAssignments(bg, &reservationpb.ListAssignmentsRequest{Parent: parent + "/reservations/" + r})
			for a, err := it.Next(); err == nil; a, err = it.Next() {
				_ = rc.DeleteAssignment(bg, &reservationpb.DeleteAssignmentRequest{Name: a.GetName()})
			}
			_ = rc.DeleteReservation(bg, &reservationpb.DeleteReservationRequest{Name: parent + "/reservations/" + r})
		}
		_ = rc.DeleteReservationGroup(bg, &reservationpb.DeleteReservationGroupRequest{Name: parent + "/reservationGroups/" + gid})
	})
	p := open(t)

	// Reservation groups.
	p.navigate("/bigquery-reservation-groups" + q)
	p.clickText("#view button", "Create reservation group")
	p.submitForm(map[string]string{"id": gid, "location": loc}, "id")
	p.waitFor(fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q))`, gid))
	if _, err := rc.GetReservationGroup(ctx, &reservationpb.GetReservationGroupRequest{Name: parent + "/reservationGroups/" + gid}); err != nil {
		t.Fatalf("the group made in the browser: %v", err)
	}

	// A reservation, then its Edit puts it in the group.
	if _, err := rc.CreateReservation(ctx, &reservationpb.CreateReservationRequest{Parent: parent, ReservationId: aid,
		Reservation: &reservationpb.Reservation{SlotCapacity: 50, Edition: reservationpb.Edition_ENTERPRISE}}); err != nil {
		t.Fatal(err)
	}
	page := "/bigquery-capacity/" + url.PathEscape(loc+"/"+aid) + q
	p.navigate(page)
	p.pageAction("Edit", map[string]string{"group": gid}, "")
	if rv, err := rc.GetReservation(ctx, &reservationpb.GetReservationRequest{Name: parent + "/reservations/" + aid}); err != nil ||
		rv.GetReservationGroup() != parent+"/reservationGroups/"+gid || rv.GetSlotCapacity() != 50 {
		t.Fatalf("after Edit with the group: %v, %v", rv, err)
	}

	// Slot commitments.
	before := map[string]bool{}
	var leftEnterprise int64 // an earlier run's Enterprise slots, which cover this run's reservation
	bit := rc.ListCapacityCommitments(ctx, &reservationpb.ListCapacityCommitmentsRequest{Parent: parent})
	for c, err := bit.Next(); err == nil; c, err = bit.Next() {
		before[c.GetName()] = true
		if c.GetEdition() == reservationpb.Edition_ENTERPRISE {
			leftEnterprise += c.GetSlotCount()
		}
	}
	p.navigate("/bigquery-commitments/create" + q)
	p.waitFor(`document.querySelector("#view #f-location") !== null`)
	setField(p, "#view", "f-location", loc)
	setField(p, "#view", "f-edition", "ENTERPRISE")
	setField(p, "#view", "f-plan", "ANNUAL")
	setField(p, "#view", "f-slots", "100")
	p.run(chromedp.Click(chromedp.CSS(`#view button[type="submit"]`)))
	// This run's commitments: an ANNUAL one outlives the test, which
	// cannot delete it before its end time.
	commitments := func() []*reservationpb.CapacityCommitment {
		t.Helper()
		var out []*reservationpb.CapacityCommitment
		it := rc.ListCapacityCommitments(ctx, &reservationpb.ListCapacityCommitmentsRequest{Parent: parent})
		for c, err := it.Next(); err == nil; c, err = it.Next() {
			if !before[c.GetName()] {
				out = append(out, c)
			}
		}
		return out
	}
	p.waitFor(`location.pathname !== "/bigquery-commitments/create"`)
	cms := commitments()
	if len(cms) != 1 || cms[0].GetSlotCount() != 100 || cms[0].GetPlan() != reservationpb.CapacityCommitment_ANNUAL {
		t.Fatalf("the commitments after Create commitment: %v", cms)
	}
	cpage := "/bigquery-commitments/" + url.PathEscape(loc+"/"+lastSegment(cms[0].GetName())) + q
	p.navigate(cpage)
	p.pageAction("Edit", map[string]string{"renewal": "ANNUAL"}, "")
	p.navigate(cpage)
	p.pageAction("Split", map[string]string{"slots": "50"}, "")
	if cms = commitments(); len(cms) != 2 || cms[0].GetSlotCount() != 50 || cms[1].GetSlotCount() != 50 ||
		cms[0].GetRenewalPlan() != reservationpb.CapacityCommitment_ANNUAL {
		t.Fatalf("after Edit renewal plan and Split: %v", cms)
	}
	first, second := lastSegment(cms[0].GetName()), lastSegment(cms[1].GetName())
	p.navigate("/bigquery-commitments/" + url.PathEscape(loc+"/"+first) + q)
	p.pageAction("Merge", map[string]string{"with": second}, "")
	if cms = commitments(); len(cms) != 1 || cms[0].GetSlotCount() != 100 {
		t.Fatalf("after Merge: %v", cms)
	}
	merged := lastSegment(cms[0].GetName())

	// Upgrade edition: refused alone, done with the commitment.
	if leftEnterprise == 0 {
		p.expected = append(p.expected, "/api/actions/bigquery-capacity")
		p.navigate(page)
		p.openForm("Upgrade edition")
		p.submitForm(map[string]string{"target": "ENTERPRISE_PLUS"}, "target")
		if msg := p.modalRefusal(); !strings.Contains(msg, "flexible") {
			t.Errorf("Upgrade edition without the commitment: %q, want the flexible slots refusal", msg)
		}
		p.run(chromedp.KeyEvent(kb.Escape))
	} else {
		t.Logf("%d Enterprise slots of an earlier run's commitments in %s cover the reservation: its refusal alone is not checked", leftEnterprise, loc)
	}
	p.navigate(page)
	p.pageAction("Upgrade edition", map[string]string{"target": "ENTERPRISE_PLUS", "commitments": merged}, "")
	if rv, err := rc.GetReservation(ctx, &reservationpb.GetReservationRequest{Name: parent + "/reservations/" + aid}); err != nil ||
		rv.GetEdition() != reservationpb.Edition_ENTERPRISE_PLUS {
		t.Fatalf("after Upgrade edition: %v, %v", rv, err)
	}
	p.navigate(page)
	p.pageAction("Edit", map[string]string{"secondary": "us-west1"}, "")
	p.navigate(page)
	p.clickAction("Failover", "")
	if rv, err := rc.GetReservation(ctx, &reservationpb.GetReservationRequest{Name: parent + "/reservations/" + aid}); err != nil ||
		rv.GetPrimaryLocation() != "us-west1" || rv.GetSecondaryLocation() != loc {
		t.Fatalf("after Failover: %v, %v", rv, err)
	}

	// An assignment's page.
	if _, err := rc.CreateReservation(ctx, &reservationpb.CreateReservationRequest{Parent: parent, ReservationId: bid,
		Reservation: &reservationpb.Reservation{Edition: reservationpb.Edition_ENTERPRISE, Autoscale: &reservationpb.Reservation_Autoscale{MaxSlots: 50}}}); err != nil {
		t.Fatal(err)
	}
	a, err := rc.CreateAssignment(ctx, &reservationpb.CreateAssignmentRequest{Parent: parent + "/reservations/" + aid,
		Assignment: &reservationpb.Assignment{Assignee: "projects/" + project, JobType: reservationpb.Assignment_QUERY}})
	if err != nil {
		t.Fatal(err)
	}
	apage := "/bigquery-capacity/" + url.PathEscape(loc+"/"+aid) + "/" + url.PathEscape(lastSegment(a.GetName())) + q
	p.navigate(apage)
	p.pageAction("Edit", map[string]string{"gemini": "true"}, "")
	p.navigate(apage + "&tab=permissions")
	p.grantOnTab("user:capacity@example.com", "roles/bigquery.resourceViewer")
	if pol, err := rc.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: a.GetName()}); err != nil || !policyHas(pol, "user:capacity@example.com", "roles/bigquery.resourceViewer") {
		t.Errorf("the assignment's policy after Grant access: %v, %v", pol, err)
	}
	p.navigate(page + "&tab=permissions")
	p.grantOnTab("user:capacity@example.com", "roles/bigquery.resourceEditor")
	if pol, err := rc.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: parent + "/reservations/" + aid}); err != nil || !policyHas(pol, "user:capacity@example.com", "roles/bigquery.resourceEditor") {
		t.Errorf("the reservation's policy after Grant access: %v, %v", pol, err)
	}
	p.navigate(apage)
	p.pageAction("Move", map[string]string{"to": bid}, "")
	it := rc.ListAssignments(ctx, &reservationpb.ListAssignmentsRequest{Parent: parent + "/reservations/" + bid})
	if moved, err := it.Next(); err != nil || !moved.GetEnableGeminiInBigquery() {
		t.Errorf("the assignment after Edit and Move: %v, %v", moved, err)
	}

	auditScreens(t, []a11yPage{
		screen("Slot commitments", "/bigquery-commitments", project),
		screen("a commitment", "/bigquery-commitments/"+url.PathEscape(loc+"/"+merged), project),
		screen("Reservation groups", "/bigquery-reservation-groups", project),
		screen("a reservation's Permissions", "/bigquery-capacity/"+url.PathEscape(loc+"/"+aid)+"?tab=permissions", project),
		screen("BI Engine", "/bigquery-bi-engine", project),
	})

	// BI Engine.
	p.navigate("/bigquery-bi-engine" + q)
	p.clickText("#view button", "Create reservation")
	p.submitForm(map[string]string{"location": loc, "size": "2"}, "location")
	p.waitFor(fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q))`, loc))
	bi := parent + "/biReservation"
	if got, err := rc.GetBiReservation(ctx, &reservationpb.GetBiReservationRequest{Name: bi}); err != nil || got.GetSize() != 2<<30 {
		t.Fatalf("the BI reservation after Create reservation: %v, %v", got, err)
	}
	p.navigate("/bigquery-bi-engine" + q)
	p.openRowDelete(loc, "Delete")
	p.confirmByName(loc)
	p.waitFor(fmt.Sprintf(`![...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q))`, loc))
	if got, err := rc.GetBiReservation(ctx, &reservationpb.GetBiReservationRequest{Name: bi}); err != nil || got.GetSize() != 0 {
		t.Errorf("the BI reservation after Delete: %v, %v", got, err)
	}
}

func lastSegment(n string) string { return n[strings.LastIndex(n, "/")+1:] }
