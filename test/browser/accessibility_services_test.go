//go:build browser

package browser

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// The rest of the console's WCAG 2.1 AA audit (#1604): every product's
// screens beyond the Cloud Storage, Pub/Sub, Logging, Request Log and
// BigQuery ones of accessibility_test.go, measured with the same helpers.
// Each test is named into the shard whose instance serves its screens
// (scripts/compat-shards.sh): TestCloudRun… in run, TestCloudSQL… in sql,
// TestSpanner… in emulators, and the rest in storage.

// screen is an audited list or form at path, ready once it has settled.
func screen(name, path, project string) a11yPage {
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	return a11yPage{name, path + sep + "project=" + url.QueryEscape(project), settled, ""}
}

// listAndCreate is a list screen and its create form's own address.
func listAndCreate(name, path, project string) []a11yPage {
	return []a11yPage{screen(name, path, project), screen("create in "+name, path+"/create", project)}
}

// openCreateDialog presses a list's primary Create button, whichever product
// it is, so the dialog (or the form page) it opens is what is measured.
const openCreateDialog = `[...document.querySelectorAll("#view button, #view a")].find((b) => /^(Create|Deploy)/.test(b.textContent.trim())).click()`

// openDeleteDialog opens the first row's actions menu and chooses Delete,
// whose confirmation is a dialog.
const openDeleteDialog = `document.querySelector("#view tbody tr .overflow-trigger:not(:disabled)").click();
	[...document.querySelectorAll("#view .overflow-item")].find((b) => b.textContent.trim().startsWith("Delete")).click()`

// dialogFailures checks an open modal dialog: it is named, focus is inside
// it, Tab and Shift+Tab stay inside it, and Escape closes it (WCAG 2.1.2,
// 2.4.3, 4.1.2). A page with no dialog open fails only when one was wanted.
func (p *tab) dialogFailures(want bool) []string {
	p.t.Helper()
	var d struct {
		Open, Inside bool
		Role, Modal  string
		Name         string
	}
	p.eval(`(() => { const d = document.querySelector(".modal.is-open");
		if (!d) return { Open: false };
		const l = document.getElementById(d.getAttribute("aria-labelledby") || "");
		return { Open: true, Inside: d.contains(document.activeElement), Role: d.getAttribute("role"),
		         Modal: d.getAttribute("aria-modal"), Name: (l ? l.textContent : d.getAttribute("aria-label") || "").trim() }; })()`, &d)
	if !d.Open {
		if want {
			return []string{"no dialog opened"}
		}
		return nil
	}
	var out []string
	if d.Role != "dialog" || d.Modal != "true" || d.Name == "" {
		out = append(out, fmt.Sprintf("the dialog is role=%q aria-modal=%q named %q", d.Role, d.Modal, d.Name))
	}
	if !d.Inside {
		out = append(out, "focus did not move into the dialog")
	}
	inside := `document.querySelector(".modal.is-open") !== null && document.querySelector(".modal.is-open").contains(document.activeElement)`
	for i, back := range []bool{false, false, false, false, false, false, true, true} {
		if back {
			p.run(chromedp.KeyEvent(kb.Tab, chromedp.KeyModifiers(input.ModifierShift)))
		} else {
			p.run(chromedp.KeyEvent(kb.Tab))
		}
		var ok bool
		p.eval(inside, &ok)
		if !ok {
			out = append(out, fmt.Sprintf("key %d of a Tab walk took focus out of the dialog", i+1))
			return out
		}
	}
	p.run(chromedp.KeyEvent(kb.Escape))
	var closed bool
	p.eval(`new Promise((r) => setTimeout(() => r(document.querySelector(".modal.is-open") === null), 300))`, &closed, awaitPromise)
	if !closed {
		out = append(out, "Escape did not close the dialog")
	}
	return out
}

// keyboardFailures walks the freshly shown page with Tab: the first stop is
// the skip link, every visible, enabled control is reached, each stop is on
// screen with a focus indicator drawn, and focus comes back round rather
// than being trapped (WCAG 2.1.1, 2.1.2, 2.4.1, 2.4.7).
func (p *tab) keyboardFailures() []string {
	p.t.Helper()
	var want []string
	p.eval(tagControlsJS, &want)
	// A form that puts focus in its first field as it opens starts its walk
	// there; any other page from the top of the document, so its first stop
	// is the skip link.
	var autofocused bool
	p.eval(`document.activeElement !== null && document.activeElement !== document.body`, &autofocused)
	stops := p.walkByTab(500)
	if autofocused {
		// From the field to the end of the document; then from its top.
		stops = append(stops, p.walkByTab(500)...)
	}
	var out []string
	if !autofocused && (len(stops) == 0 || !strings.HasSuffix(stops[0].Key, "Skip to main content")) {
		first := "nothing"
		if len(stops) > 0 {
			first = stops[0].Key
		}
		out = append(out, fmt.Sprintf("the first Tab lands on %q, not the skip link", first))
	}
	reached := map[string]bool{}
	for _, s := range stops {
		reached[s.Key] = true
		if !s.Visible {
			out = append(out, fmt.Sprintf("Tab stops on %q, which is not on screen", s.Key))
		} else if !s.Ring {
			out = append(out, fmt.Sprintf("Tab stops on %q with no focus indicator drawn", s.Key))
		}
	}
	for _, w := range want {
		if !reached[w] {
			out = append(out, fmt.Sprintf("Tab never reaches %q", w))
		}
	}
	return out
}

// auditScreens runs the whole audit over pages, each part in a subtest:
// accessible names and table headers; contrast in the light and the dark
// theme; keyboard (a Tab walk of every page with nothing opened over it,
// and the dialog of every page that opens one); reduced motion; and reflow
// at 320 and 640 CSS pixels. A tab is reopened every few pages, so no
// subtest outlives one browser's deadline.
func auditScreens(t *testing.T, pages []a11yPage) {
	t.Helper()
	each := func(t *testing.T, setup func(p *tab), fn func(p *tab, pg a11yPage)) {
		t.Helper()
		var p *tab
		for i, pg := range pages {
			if i%10 == 0 {
				p = open(t)
				setup(p)
			}
			p.showAsked(pg)
			fn(p, pg)
		}
	}
	none := func(*tab) {}
	t.Run("names", func(t *testing.T) {
		each(t, none, func(p *tab, pg a11yPage) {
			for _, u := range p.unnamedControls() {
				t.Errorf("%s: an unnamed %s", pg.name, u)
			}
		})
	})
	for _, theme := range []string{"light", "dark"} {
		t.Run("contrast/"+theme, func(t *testing.T) {
			each(t, func(p *tab) {
				p.run(reducedMotion())
				p.navigate("/")
				p.waitFor(settled)
				p.setTheme(theme)
			}, func(p *tab, pg a11yPage) {
				for _, fl := range p.contrastFailures() {
					t.Errorf("%s, %s theme: %s", pg.name, theme, fl)
				}
			})
		})
	}
	t.Run("keyboard", func(t *testing.T) {
		each(t, func(p *tab) { p.run(reducedMotion()) }, func(p *tab, pg a11yPage) {
			fails := p.dialogFailures(strings.HasSuffix(pg.name, " dialog"))
			if pg.then == "" {
				fails = p.keyboardFailures()
			}
			for _, fl := range fails {
				t.Errorf("%s: %s", pg.name, fl)
			}
		})
	})
	t.Run("motion", func(t *testing.T) {
		each(t, func(p *tab) { p.run(reducedMotion()) }, func(p *tab, pg a11yPage) {
			for _, m := range p.motionFailures() {
				t.Errorf("%s: under reduced motion, %s", pg.name, m)
			}
		})
	})
	for _, width := range []int64{320, 640} {
		t.Run(fmt.Sprintf("reflow/%dpx", width), func(t *testing.T) {
			each(t, func(p *tab) { p.run(chromedp.EmulateViewport(width, 720), reducedMotion()) }, func(p *tab, pg a11yPage) {
				for _, fl := range p.reflowFailures() {
					t.Errorf("%s at %dpx: %s", pg.name, width, fl)
				}
			})
		})
	}
}

// showAsked is show, for a page that may ask for the admin token: the
// Instance and Fault injection pages answer 401 until it is given, and the
// form asking for it is what is audited, so those 401s are not the page's
// errors. Every other page's are kept.
func (p *tab) showAsked(pg a11yPage) {
	p.t.Helper()
	p.mu.Lock()
	before := len(p.errors)
	p.mu.Unlock()
	p.show(pg)
	if !strings.HasPrefix(pg.path, "/instance") && !strings.HasPrefix(pg.path, "/faults") {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	kept := p.errors[:before]
	for _, e := range p.errors[before:] {
		if !strings.Contains(e, "401 (Unauthorized)") {
			kept = append(kept, e)
		}
	}
	p.errors = kept
}

// fixture creates a resource through the console API and deletes it when
// the test ends.
func fixture(t *testing.T, service, project, body, name string) {
	t.Helper()
	q := "?project=" + url.QueryEscape(project)
	if code, resp := consoleDo(t, http.MethodPost, "/api/resources/"+service+q, body); code != http.StatusOK {
		t.Fatalf("create a %s fixture through the console API = %d: %s", service, code, resp)
	}
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/"+service+q+"&name="+url.QueryEscape(name), "")
	})
}

// TestCloudRunAndCICDScreensMeetWCAG: Cloud Run's services, jobs, worker
// pools, instances and builds; Cloud Functions; Artifact Registry; Cloud
// Build's triggers and worker pools; Secure Source Manager, whose instance
// and repository pages carry Developer Connect's connections and links;
// Cloud KMS's key rings, a key ring, EKM connections, Autokey and HSM; and
// Cloud Logging's admin screens, all served only by the run shard.
func TestCloudRunAndCICDScreensMeetWCAG(t *testing.T) {
	needService(t, "run")
	needService(t, "kms")
	project := uniqueProject(t)
	fixture(t, "kms", project, `{"keyRingId":"a11y-ring","location":"global"}`, "projects/"+project+"/locations/global/keyRings/a11y-ring")
	var pages []a11yPage
	for _, l := range []struct{ name, path string }{
		{"Cloud Run services", "/run"}, {"Cloud Run jobs", "/run/jobs"},
		{"Cloud Run worker pools", "/run/worker-pools"}, {"Cloud Run instances", "/run/instances"},
		{"Cloud Run builds", "/run/builds"}, {"Cloud Functions", "/functions"},
		{"Artifact Registry", "/artifact-registry"}, {"Cloud Build triggers", "/cloud-build/triggers"},
		{"Cloud Build worker pools", "/cloud-build/worker-pools"},
		{"Secure Source Manager", "/secure-source-manager"},
		{"KMS key rings", "/kms/keyrings"}, {"KMS EKM", "/kms/ekm"},
		{"KMS Autokey", "/kms/autokey"}, {"KMS HSM", "/kms/hsm"},
		{"log-based metrics", "/logs/metrics"}, {"log router", "/logs/sinks"},
		{"exclusions", "/logs/exclusions"}, {"logs storage", "/logs/storage"},
		{"saved queries", "/logs/saved-queries"}, {"log scopes", "/logs/log-scopes"},
	} {
		pages = append(pages, listAndCreate(l.name, l.path, project)...)
	}
	pages = append(pages,
		screen("logs storage settings", "/logs/settings", project),
		a11yPage{"key ring", "/kms/keyrings/global/a11y-ring?project=" + project, settled, ""},
	)
	auditScreens(t, pages)
}

// TestCloudSQLAndBackupDRScreensMeetWCAG: Cloud SQL's PostgreSQL and MySQL
// databases, its instances, and the Backup and DR Service, served only by
// the sql shard.
func TestCloudSQLAndBackupDRScreensMeetWCAG(t *testing.T) {
	needService(t, "cloudsql")
	needService(t, "backupdr")
	project := servedProject(t)
	var pages []a11yPage
	for _, l := range []struct{ name, path string }{
		{"PostgreSQL", "/cloudsql"}, {"MySQL", "/cloudsql-mysql"},
		{"Cloud SQL instances", "/cloudsql-instances"}, {"Backup and DR", "/backupdr"},
	} {
		pages = append(pages, listAndCreate(l.name, l.path, project)...)
	}
	pages = append(pages,
		screen("postgres database", "/cloudsql/postgres", project),
		a11yPage{"delete database dialog", "/cloudsql?project=" + project, rowReady("postgres"), openDeleteDialog},
	)
	auditScreens(t, pages)
}

// TestSpannerAndTheEmulatorScreensMeetWCAG: Spanner, Bigtable and its
// instances, Firestore and its databases, Datastore, and Memorystore for
// Redis, Redis Cluster and Valkey, served only by the emulators shard.
func TestSpannerAndTheEmulatorScreensMeetWCAG(t *testing.T) {
	for _, s := range []string{"spanner", "bigtable", "firestore", "datastore", "memorystore"} {
		needService(t, s)
	}
	project := servedProject(t)
	var pages []a11yPage
	for _, l := range []struct{ name, path string }{
		{"Spanner", "/spanner"}, {"Bigtable", "/bigtable"}, {"Bigtable instances", "/bigtable-instances"},
		{"Firestore", "/firestore"}, {"Firestore databases", "/firestore-databases"},
		{"Datastore", "/datastore"}, {"Memorystore", "/memorystore"},
		{"Memorystore Redis Cluster", "/memorystore-clusters"}, {"Memorystore for Valkey", "/memorystore-valkey"},
	} {
		pages = append(pages, listAndCreate(l.name, l.path, project)...)
	}
	pages = append(pages,
		a11yPage{"delete Firestore database dialog", "/firestore-databases?project=" + project, rowReady("(default)"), openDeleteDialog},
	)
	auditScreens(t, pages)
}

// TestSecretsSchedulerTasksProjectsIAMAndAIScreensMeetWCAG: Secret Manager,
// Cloud Scheduler and Cloud Tasks with a row each, Resource Manager's
// projects, hierarchy and tags, IAM, APIs & Services, Kubernetes Engine, the
// Vertex AI and local AI pages, Monitoring, Fault injection, Instance, and
// Cloud Storage's other pages: everything every instance or the storage
// shard's serves.
func TestSecretsSchedulerTasksProjectsIAMAndAIScreensMeetWCAG(t *testing.T) {
	for _, s := range []string{"secrets", "scheduler", "tasks", "storage"} {
		needService(t, s)
	}
	project := uniqueProject(t)
	fixture(t, "secrets", project, `{"secretId":"a11y-secret","payload":"x"}`, "a11y-secret")
	fixture(t, "tasks", project, `{"name":"a11y-queue","location":"us-central1"}`, "projects/"+project+"/locations/us-central1/queues/a11y-queue")
	createSchedulerJob(t, project, "a11y-job")
	var pages []a11yPage
	for _, l := range []struct{ name, path string }{
		{"Secret Manager", "/secrets"}, {"Cloud Scheduler", "/scheduler/jobs"}, {"Cloud Tasks", "/tasks/queues"},
		{"projects", "/projects"}, {"resource hierarchy", "/resource-hierarchy"}, {"tags", "/tags"},
		{"IAM", "/iam-admin/iam"}, {"APIs & Services", "/apis"},
		{"Kubernetes workloads", "/kubernetes/workloads"}, {"pods", "/kubernetes/pods"},
		{"Kubernetes services", "/kubernetes/services"}, {"Kubernetes jobs", "/kubernetes/jobs"},
		{"deleted buckets", "/storage/deleted"},
	} {
		pages = append(pages, listAndCreate(l.name, l.path, project)...)
	}
	for _, l := range []struct{ name, path string }{
		{"Service Usage operations", "/apis/operations"}, {"nodes", "/kubernetes/nodes"},
		{"Kubernetes storage", "/kubernetes/storage"}, {"events", "/kubernetes/events"},
		{"Storage Intelligence", "/storage/intelligence"},
		{"Model Garden", "/ai/models"}, {"Studio", "/ai/playground"}, {"Embeddings", "/ai/embeddings"},
		{"Local models", "/ai/local-models"}, {"Online prediction", "/ai/predict"},
		{"Monitoring", "/monitoring"}, {"Fault injection", "/faults"}, {"Instance", "/instance"},
		{"secret", "/secrets/a11y-secret"}, {"queue", "/tasks/queues/us-central1/a11y-queue"},
		{"scheduler job", "/scheduler/jobs/us-central1/a11y-job"}, {"project", "/projects/" + project},
	} {
		pages = append(pages, screen(l.name, l.path, project))
	}
	pages = append(pages,
		a11yPage{"secret row menu", "/secrets?project=" + project, rowReady("a11y-secret"),
			`document.querySelector("#view tbody tr .overflow-trigger").click()`},
		a11yPage{"delete secret dialog", "/secrets?project=" + project, rowReady("a11y-secret"), openDeleteDialog},
		a11yPage{"create queue dialog", "/tasks/queues?project=" + project, rowReady("a11y-queue"), openCreateDialog},
	)
	auditScreens(t, pages)
}
