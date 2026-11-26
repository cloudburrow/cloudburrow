//go:build browser

package browser

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/cloudsqlconn"
	"cloud.google.com/go/cloudsqlconn/postgres/pgxv5"
	"github.com/chromedp/chromedp"
	"golang.org/x/oauth2"
	backupdr "google.golang.org/api/backupdr/v1"
	"google.golang.org/api/option"
	sqladmin "google.golang.org/api/sqladmin/v1"
)

const envCloudSQLConnector = "CLOUDBURROW_TEST_CLOUDSQL_CONNECTOR"

// bdrPGRows runs stmts on a Cloud SQL for PostgreSQL instance through the
// official connector (a fresh connection each time: a restore ends the
// sessions it finds), then reads table's rows as id=name.
func bdrPGRows(t *testing.T, conn, table string, stmts ...string) []string {
	t.Helper()
	opts := []cloudsqlconn.Option{
		cloudsqlconn.WithAdminAPIEndpoint("http://" + endpoint(t, envSQLAdmin) + "/"),
		cloudsqlconn.WithTokenSource(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "cloudburrow", Expiry: time.Now().Add(time.Hour)})),
	}
	if listener := os.Getenv(envCloudSQLConnector); listener != "" {
		if host, port, err := net.SplitHostPort(listener); err == nil && port != "3307" {
			opts = append(opts, cloudsqlconn.WithDialFunc(func(ctx context.Context, network, addr string) (net.Conn, error) {
				if want := net.JoinHostPort(host, "3307"); addr != want {
					return nil, fmt.Errorf("the connector dialed %s, want %s", addr, want)
				}
				return (&net.Dialer{}).DialContext(ctx, network, listener)
			}))
		}
	}
	drv := fmt.Sprintf("bdr1844-%d", time.Now().UnixNano())
	cleanup, err := pgxv5.RegisterDriver(drv, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cleanup() }()
	db, err := sql.Open(drv, fmt.Sprintf("host=%s user=postgres password=%s dbname=postgres sslmode=disable", conn, "Root-pass1"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	rows, err := db.QueryContext(ctx, "SELECT id::text || '=' || name FROM "+table+" ORDER BY id")
	if err != nil {
		t.Fatalf("read %s: %v", table, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

// TestBackupDRConsoleRound2ThroughThePages (#1844), in the sql shard, with
// a Cloud SQL for PostgreSQL instance holding a row, protected through the
// API in a vault with a day's minimum enforced retention. On Google's pages:
//   - Vaulted backups has Filter table > Resource type (Compute Engine lists
//     nothing, Cloud SQL the instance), and the instance's row menu has
//     Create on-demand backup, whose dialog "Create an on-demand backup"
//     takes a backup backups.list reads ACTIVE;
//   - a second row is written; on the resource's page the backup row's
//     Delete is disabled with the enforced retention's reason, and its
//     Restore, Overwrite the source instance, brings back exactly the
//     backed-up row, read through the official connector;
//   - Jobs has Google's time range list and lists the restore;
//   - the plan's Edit is a page, Edit backup plan, ending in Save Changes,
//     whose description backupPlans.get reads back.
//
// Each screen is audited for WCAG 2.1 AA.
func TestBackupDRConsoleRound2ThroughThePages(t *testing.T) {
	needService(t, "backupdr")
	needService(t, "cloudsql-instances")
	bdr, err := backupdr.NewService(context.Background(), option.WithEndpoint("http://"+endpoint(t, envBackupDR)+"/"),
		option.WithoutAuthentication(), option.WithHTTPClient(http.DefaultClient))
	if err != nil {
		t.Fatal(err)
	}
	svc := sqlAdminSDK(t)
	project := servedProject(t)
	loc := "projects/" + project + "/locations/us-central1"
	n := time.Now().UnixNano() % 1e6
	inst, vault, plan := fmt.Sprintf("browser-bdr2-%d", n), fmt.Sprintf("browser-r2vault-%d", n), fmt.Sprintf("browser-r2plan-%d", n)
	vaultName, planName, assocName := loc+"/backupVaults/"+vault, loc+"/backupPlans/"+plan, loc+"/backupPlanAssociations/"+inst
	conn, table := project+":us-central1:"+inst, "bdr_round2"
	createSQL(t, svc, project, &sqladmin.DatabaseInstance{Name: inst, DatabaseVersion: "POSTGRES_17", RootPassword: "Root-pass1",
		Region: "us-central1", Settings: &sqladmin.Settings{Tier: "db-custom-1-3840"}})
	if got := bdrPGRows(t, conn, table, "CREATE TABLE "+table+" (id integer PRIMARY KEY, name text)",
		"INSERT INTO "+table+" VALUES (1, 'one')"); strings.Join(got, ",") != "1=one" {
		t.Fatalf("rows before the backup: %v", got)
	}
	wait := func(what string, op *backupdr.Operation, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		for end := time.Now().Add(5 * time.Minute); !op.Done && time.Now().Before(end); time.Sleep(time.Second) {
			if op, err = bdr.Projects.Locations.Operations.Get(op.Name).Do(); err != nil {
				t.Fatalf("%s: operations.get: %v", what, err)
			}
		}
		if !op.Done || op.Error != nil {
			t.Fatalf("%s: %+v", what, op)
		}
	}
	op, err := bdr.Projects.Locations.BackupVaults.Create(loc, &backupdr.BackupVault{BackupMinimumEnforcedRetentionDuration: "86400s"}).
		BackupVaultId(vault).Do()
	wait("backupVaults.create", op, err)
	op, err = bdr.Projects.Locations.BackupPlans.Create(loc, &backupdr.BackupPlan{BackupVault: vaultName, Description: "first",
		ResourceType: "sqladmin.googleapis.com/Instance",
		BackupRules: []*backupdr.BackupRule{{RuleId: "daily", BackupRetentionDays: 7,
			StandardSchedule: &backupdr.StandardSchedule{RecurrenceType: "DAILY", TimeZone: "UTC",
				BackupWindow: &backupdr.BackupWindow{StartHourOfDay: 0, EndHourOfDay: 24}}}}}).BackupPlanId(plan).Do()
	wait("backupPlans.create", op, err)
	op, err = bdr.Projects.Locations.BackupPlanAssociations.Create(loc, &backupdr.BackupPlanAssociation{BackupPlan: planName,
		Resource: "projects/" + project + "/instances/" + inst, ResourceType: "sqladmin.googleapis.com/Instance"}).
		BackupPlanAssociationId(inst).Do()
	wait("backupPlanAssociations.create", op, err)
	t.Cleanup(func() {
		_, _ = bdr.Projects.Locations.BackupPlanAssociations.Delete(assocName).Do()
		_, _ = bdr.Projects.Locations.BackupPlans.Delete(planName).Do()
		_, _ = bdr.Projects.Locations.BackupVaults.Delete(vaultName).Force(true).IgnoreBackupPlanReferences(true).Do()
	})
	a, err := bdr.Projects.Locations.BackupPlanAssociations.Get(assocName).Do()
	if err != nil {
		t.Fatal(err)
	}
	ds := a.DataSource[strings.LastIndex(a.DataSource, "/")+1:]

	// Vaulted backups: Filter table > Resource type, and the row's menu.
	p := open(t)
	list := "/backupdr-vaulted-backups?project=" + project
	p.navigate(list)
	p.waitFor(rowReady(inst) + ` && document.querySelector("#scope-type") !== null`)
	var types string
	p.eval(`[...document.querySelectorAll("#scope-type option")].map((o) => o.textContent).join(",")`, &types)
	if types != "All resource types,Cloud SQL,Compute Engine,Disk,AlloyDB,Filestore" {
		t.Errorf("Filter table > Resource type offers %q", types)
	}
	p.eval(`(() => { const s = document.querySelector("#scope-type"); s.value = "compute.googleapis.com/Instance";
		s.dispatchEvent(new Event("change")); return true; })()`, nil)
	p.waitFor(fmt.Sprintf(`new URLSearchParams(location.search).get("type") === "compute.googleapis.com/Instance" &&
		document.querySelector("#view .skeleton") === null && !(%s)`, rowReady(inst)))
	p.eval(`(() => { const s = document.querySelector("#scope-type"); s.value = "sqladmin.googleapis.com/Instance";
		s.dispatchEvent(new Event("change")); return true; })()`, nil)
	p.waitFor(rowReady(inst))
	row := fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr")].find((r) => r.textContent.includes(%q))`, inst)
	p.eval(row+`.querySelector(".overflow-trigger").click()`, nil)
	var items string
	p.eval(`[...document.querySelectorAll(".overflow-item")].map((b) => b.textContent.trim()).join(",")`, &items)
	if !strings.Contains(items, "Create on-demand backup") || !strings.Contains(items, "Remove backup plan") {
		t.Errorf("the resource's row menu is %q", items)
	}
	p.clickText(".overflow-item", "Create on-demand backup")
	p.waitFor(`document.querySelector(".modal.is-open form") !== null`)
	var title string
	p.eval(`document.querySelector(".modal.is-open h2").textContent`, &title)
	if !strings.Contains(title, "Create an on-demand backup") {
		t.Errorf("the dialog is titled %q, want Google's Create an on-demand backup", title)
	}
	setField(p, ".modal.is-open", "f-rule", "daily")
	p.clickText(".modal.is-open button[type=submit]", "Create")
	p.waitForWithin(`document.querySelector(".modal.is-open") === null`, time.Minute)
	backup := ""
	for deadline := time.Now().Add(5 * time.Minute); backup == "" && time.Now().Before(deadline); time.Sleep(2 * time.Second) {
		l, err := bdr.Projects.Locations.BackupVaults.DataSources.Backups.List(a.DataSource).Do()
		if err != nil {
			t.Fatalf("backups.list: %v", err)
		}
		for _, b := range l.Backups {
			if b.BackupType == "ON_DEMAND" && b.State == "ACTIVE" {
				backup = b.Name[strings.LastIndex(b.Name, "/")+1:]
			}
		}
	}
	if backup == "" {
		t.Fatal("the row's Create on-demand backup made no ACTIVE backup in five minutes")
	}

	// A row written after the backup, which the restore takes away.
	if got := bdrPGRows(t, conn, table, "INSERT INTO "+table+" VALUES (2, 'two')"); strings.Join(got, ",") != "1=one,2=two" {
		t.Fatalf("rows after the backup: %v", got)
	}

	// The backup's row: Delete disabled with its reason, then Restore.
	page := "/backupdr-vaulted-backups/" + vault + "/" + ds + "?project=" + project
	p.navigate(page)
	p.waitFor(rowReady(backup))
	brow := fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr")].find((r) => r.textContent.includes(%q))`, backup)
	p.eval(brow+`.querySelector(".overflow-trigger").click()`, nil)
	var del struct {
		Disabled bool
		Reason   string
	}
	p.eval(`(() => { const b = [...document.querySelectorAll(".overflow-item")].find((b) => b.textContent.trim() === "Delete");
		return { Disabled: !!b && b.disabled, Reason: b ? b.getAttribute("aria-description") || "" : "" }; })()`, &del)
	if !del.Disabled || !strings.Contains(del.Reason, "minimum enforced retention") {
		t.Errorf("the backup's Delete inside its enforced retention is %+v; want it disabled with the reason", del)
	}
	if _, err := bdr.Projects.Locations.BackupVaults.DataSources.Backups.Delete(a.DataSource + "/backups/" + backup).Do(); err == nil {
		t.Errorf("backups.delete inside the enforced retention succeeded")
	}
	p.clickText(".overflow-item", "Restore")
	p.waitFor(`document.querySelector(".modal.is-open form") !== null`)
	p.clickText(".modal.is-open button", "Restore")
	p.confirmByName(p.confirmWord())
	p.waitForWithin(`document.querySelector(".modal.is-open") === null`, time.Minute)
	waitLatestSQLOp(t, svc, project, inst, "RESTORE_VOLUME")
	if got := bdrPGRows(t, conn, table); strings.Join(got, ",") != "1=one" {
		t.Errorf("rows after Restore over the source instance: %v, want the backed-up 1=one", got)
	}

	// Jobs: Google's time range list, and the restore.
	jobs := "/backupdr-jobs?project=" + project
	restoreRow := fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q) &&
		r.textContent.includes("Restore"))`, inst)
	p.navigate(jobs)
	p.waitForWithin(restoreRow+` && document.querySelector("#scope-range") !== null`, time.Minute)
	var rng string
	p.eval(`document.querySelector("#scope-range").selectedOptions[0].textContent`, &rng)
	if rng != "Last 24 hours" {
		t.Errorf("the Jobs page's time range opens at %q, want Last 24 hours", rng)
	}

	// Edit backup plan, a page of its own ending in Save Changes.
	planPage := "/backupdr-plans/" + plan + "?project=" + project
	p.navigate(planPage)
	p.clickText("#view .page-actions a", "Edit")
	p.waitFor(`new URLSearchParams(location.search).get("edit") === "1" && document.querySelector("#edit-page-form #f-description") !== null`)
	var form struct{ Heading, Submit, Description string }
	p.eval(`(() => ({ Heading: document.querySelector("#view h1").textContent,
		Submit: document.querySelector("#edit-page-form button[type=submit]").textContent,
		Description: document.querySelector("#edit-page-form #f-description").value }))()`, &form)
	if form.Heading != "Edit backup plan" || form.Submit != "Save Changes" || form.Description != "first" {
		t.Errorf("the edit page is %+v; want Google's Edit backup plan, prefilled, ending in Save Changes", form)
	}
	p.eval(`(() => { const f = document.querySelector("#edit-page-form #f-description"); f.value = "edited on the page";
		f.dispatchEvent(new Event("input", { bubbles: true })); return true; })()`, nil)
	p.run(chromedp.Click(chromedp.CSS(`#edit-page-form button[type="submit"]`)))
	p.waitFor(`!new URLSearchParams(location.search).has("edit") && document.querySelector("#edit-page-form") === null`)
	if got, err := bdr.Projects.Locations.BackupPlans.Get(planName).Do(); err != nil || got.Description != "edited on the page" ||
		len(got.BackupRules) != 1 || got.BackupRules[0].RuleId != "daily" {
		t.Errorf("backupPlans.get after Save Changes: %v %+v", err, got)
	}

	openMenu := row + `.querySelector(".overflow-trigger").click()`
	auditScreens(t, []a11yPage{
		{"Vaulted backups with Resource type", list + "&type=sqladmin.googleapis.com/Instance", rowReady(inst), ""},
		{"Vaulted backups row menu", list, rowReady(inst), openMenu},
		{"Create an on-demand backup dialog", list, rowReady(inst), openMenu +
			`; [...document.querySelectorAll(".overflow-item")].find((b) => b.textContent.trim() === "Create on-demand backup").click()`},
		{"vaulted resource with a disabled Delete", page, rowReady(backup), brow + `.querySelector(".overflow-trigger").click()`},
		{"Jobs with its time range", jobs, restoreRow, ""},
		{"Edit backup plan page", planPage + "&edit=1", settled + ` && document.querySelector("#edit-page-form") !== null`, ""},
	})
}
