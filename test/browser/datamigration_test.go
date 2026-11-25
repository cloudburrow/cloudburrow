//go:build browser

package browser

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// dmsBrowserSource starts a PostgreSQL 15 source (the instance's own Cloud
// SQL image, so nothing else is pulled) on the kind network, with a
// database of 20 rows, and returns its address there.
func dmsBrowserSource(t *testing.T) (ip string, exec1 func(stmt string)) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("needs docker")
	}
	const image = "postgres:15-alpine@sha256:f7d23353e1b15400d22ebe31189f4d314b87a4c129cc400c8c2d8d4ca127bf81"
	name := fmt.Sprintf("cb-dms-browser-%d", time.Now().UnixNano()%1e9)
	if out, err := exec.Command("docker", "run", "-d", "--name", name, "--network", "kind", "-e", "POSTGRES_PASSWORD=Src-pass1", image,
		"-c", "wal_level=logical", "-c", "max_replication_slots=10", "-c", "max_wal_senders=10").CombinedOutput(); err != nil {
		t.Fatalf("docker run the source: %v: %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })
	out, err := exec.Command("docker", "inspect", "-f", "{{(index .NetworkSettings.Networks \"kind\").IPAddress}}", name).Output()
	if err != nil || strings.TrimSpace(string(out)) == "" {
		t.Fatalf("the source's address: %v %s", err, out)
	}
	run := func(db, stmt string) error {
		out, err := exec.Command("docker", "exec", "-e", "PGPASSWORD=Src-pass1", name, "psql", "-h", "127.0.0.1", "-U", "postgres",
			"-d", db, "-qAt", "-v", "ON_ERROR_STOP=1", "-c", stmt).CombinedOutput()
		if err != nil {
			return fmt.Errorf("%s: %v: %s", stmt, err, out)
		}
		return nil
	}
	for deadline := time.Now().Add(3 * time.Minute); run("postgres", "SELECT 1") != nil; {
		if time.Now().After(deadline) {
			t.Fatal("the source did not start")
		}
		time.Sleep(time.Second)
	}
	for _, q := range []string{"CREATE DATABASE shop"} {
		if err := run("postgres", q); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{"CREATE TABLE orders (id serial PRIMARY KEY, item text)", "INSERT INTO orders (item) SELECT 'i' || g FROM generate_series(1, 20) g"} {
		if err := run("shop", q); err != nil {
			t.Fatal(err)
		}
	}
	return strings.TrimSpace(string(out)), func(stmt string) {
		t.Helper()
		if err := run("shop", stmt); err != nil {
			t.Fatal(err)
		}
	}
}

// dmsDetail is a migration job's page as the console API answers it.
func dmsDetail(t *testing.T, project, name string) string {
	t.Helper()
	q := url.Values{"project": {project}, "name": {name}}
	code, body := consoleDo(t, http.MethodGet, "/api/detail/datamigration?"+q.Encode(), "")
	if code != http.StatusOK {
		t.Fatalf("the migration job's page = %d: %s", code, body)
	}
	return body
}

// TestDataMigrationThroughThePage (#2252): Database Migration Service's
// pages as Google's, in the sql shard. Reached from the drawer's
// Databases section, and from Migrate on Cloud SQL's Instances page; a
// PostgreSQL source (a container on the kind network) and a Cloud SQL
// destination are made on Connection profiles; Create migration job
// takes them, with native logical replication and Start now; the job
// copies the source and reaches its CDC phase; on its page Promote is
// reached from the keyboard and completes the job; Conversion workspaces
// says it is not served; and each screen passes the accessibility audit
// (names, contrast, keyboard, motion, reflow).
func TestDataMigrationThroughThePage(t *testing.T) {
	needService(t, "datamigration")
	needService(t, "cloudsql")
	project := uniqueProject(t)
	n := time.Now().UnixNano() % 1e6
	srcID, dstID, jobID := fmt.Sprintf("br-src-%d", n), fmt.Sprintf("br-dst-%d", n), fmt.Sprintf("br-job-%d", n)
	parent := "projects/" + project + "/locations/us-central1"
	ip, onSource := dmsBrowserSource(t)
	t.Cleanup(func() {
		act := func(service, name, action string, values map[string]string) {
			b, _ := json.Marshal(map[string]any{"Path": []string{name}, "Action": action, "Values": values})
			consoleDo(t, http.MethodPost, "/api/actions/"+service+"?project="+url.QueryEscape(project), string(b))
		}
		act("datamigration", parent+"/migrationJobs/"+jobID, "delete", map[string]string{"force": "true"})
		act("datamigration-profiles", parent+"/connectionProfiles/"+srcID, "delete", map[string]string{})
	})

	p := open(t)
	p.navigateByDrawer("Databases", "/dbmigrate/migrations", "Database Migration Service")
	// The product's own navigation has Google's four pages.
	for _, path := range []string{"/dbmigrate/migrations", "/dbmigrate/conversion-workspaces", "/dbmigrate/connection-profiles", "/dbmigrate/private-connections"} {
		p.waitFor(fmt.Sprintf(`[...document.querySelectorAll('a[href^=%q]')].length > 0`, path))
	}
	// Migrate on Cloud SQL's Instances page opens Migration jobs.
	p.navigate("/sql/instances?project=" + project)
	p.clickText("#view button, #view a", "Migrate")
	p.waitFor(`location.pathname === "/dbmigrate/migrations"`)

	p.navigate("/dbmigrate/connection-profiles/create?project=" + project)
	p.submitForm(map[string]string{"kind": "postgresql", "profileId": srcID, "displayName": "Browser source", "host": ip, "port": "5432",
		"username": "postgres", "password": "Src-pass1"}, "profileId")
	p.waitForWithin(listedRow("/dbmigrate/connection-profiles", srcID), time.Minute)
	p.navigate("/dbmigrate/connection-profiles/create?project=" + project)
	p.waitFor(fmt.Sprintf(`[...document.querySelectorAll("#f-sourceId option")].some((o) => o.value === %q)`, parent+"/connectionProfiles/"+srcID))
	p.submitForm(map[string]string{"kind": "cloudsql", "profileId": dstID, "sourceId": parent + "/connectionProfiles/" + srcID,
		"version": "POSTGRES_15", "tier": "db-custom-1-3840", "rootPassword": "Root-pass1"}, "profileId")
	p.waitForWithin(listedRow("/dbmigrate/connection-profiles", dstID), 8*time.Minute)
	// The destination's Cloud SQL instance becomes ready.
	for deadline := time.Now().Add(8 * time.Minute); ; {
		q := url.Values{"project": {project}, "name": {parent + "/connectionProfiles/" + dstID}}
		code, body := consoleDo(t, http.MethodGet, "/api/detail/datamigration-profiles?"+q.Encode(), "")
		if code == http.StatusOK && strings.Contains(body, `"READY"`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the destination profile is not ready: %d %s", code, body)
		}
		time.Sleep(3 * time.Second)
	}

	p.navigate("/dbmigrate/migrations/create?project=" + project)
	p.waitFor(fmt.Sprintf(`[...document.querySelectorAll("#f-destination option")].some((o) => o.value === %q)`, parent+"/connectionProfiles/"+dstID))
	p.submitForm(map[string]string{"jobId": jobID, "displayName": "Browser migration", "source": parent + "/connectionProfiles/" + srcID,
		"destination": parent + "/connectionProfiles/" + dstID, "native": "true", "startNow": "true"}, "jobId")
	p.waitForWithin(listedRow("/dbmigrate/migrations", jobID), time.Minute)
	job := parent + "/migrationJobs/" + jobID
	for deadline := time.Now().Add(8 * time.Minute); ; {
		body := dmsDetail(t, project, job)
		if strings.Contains(body, "Running (CDC)") {
			break
		}
		if strings.Contains(body, `"Failed"`) || time.Now().After(deadline) {
			t.Fatalf("the job did not reach its CDC phase: %s", body)
		}
		time.Sleep(3 * time.Second)
	}
	onSource("INSERT INTO orders (item) VALUES ('after-start')")

	jobPath := "/dbmigrate/migrations/" + url.PathEscape(job) + "?project=" + url.QueryEscape(project)
	p.navigate(jobPath)
	p.waitFor(`document.querySelector("#view").textContent.includes("Running (CDC)")`)

	t.Run("keyboard", func(t *testing.T) {
		// Tab from the page's title reaches Promote, with a focus
		// indicator, and Enter opens its dialog.
		p.eval(`document.querySelector("#view h1").setAttribute("tabindex", "-1"), document.querySelector("#view h1").focus(), true`, nil)
		reached := false
		for i := 0; i < 60 && !reached; i++ {
			p.run(chromedp.KeyEvent(kb.Tab))
			p.eval(`document.activeElement.matches("#view button") && document.activeElement.textContent.trim() === "Promote"`, &reached)
		}
		if !reached {
			t.Fatal("Tab never reaches Promote")
		}
		var ring bool
		p.eval(`(() => { const s = getComputedStyle(document.activeElement); return (s.outlineStyle !== "none" && parseFloat(s.outlineWidth) >= 1) || s.boxShadow !== "none"; })()`, &ring)
		if !ring {
			t.Error("the focused Promote draws no focus indicator")
		}
		p.run(chromedp.KeyEvent(kb.Enter))
		p.waitFor(`document.querySelector(".modal.is-open form") !== null`)
		p.run(chromedp.KeyEvent(kb.Escape))
		p.waitFor(`document.querySelector(".modal") === null`)
	})

	p.pageActionWithin("Promote", nil, jobID, 5*time.Minute)
	for deadline := time.Now().Add(5 * time.Minute); ; {
		body := dmsDetail(t, project, job)
		if strings.Contains(body, `"Completed"`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the job did not complete after Promote: %s", body)
		}
		time.Sleep(3 * time.Second)
	}

	p.navigate("/dbmigrate/conversion-workspaces?project=" + project)
	p.waitFor(`document.querySelector("#view").textContent.includes("UNIMPLEMENTED")`)

	t.Run("WCAG", func(t *testing.T) {
		auditScreens(t, []a11yPage{
			screen("Database Migration Service migration jobs", "/dbmigrate/migrations", project),
			screen("create a migration job", "/dbmigrate/migrations/create", project),
			{"a migration job", jobPath, settled, ""},
			screen("Database Migration Service connection profiles", "/dbmigrate/connection-profiles", project),
			screen("create a connection profile", "/dbmigrate/connection-profiles/create", project),
			screen("Database Migration Service private connectivity", "/dbmigrate/private-connections", project),
			screen("create a private connectivity configuration", "/dbmigrate/private-connections/create", project),
			screen("Database Migration Service conversion workspaces", "/dbmigrate/conversion-workspaces", project),
		})
	})
}
