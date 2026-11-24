//go:build browser

package browser

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
	sqladmin "google.golang.org/api/sqladmin/v1"
)

// dbStudioSignIn fills the Studio's sign-in dialog and submits it.
func (p *tab) dbStudioSignIn(database, user, password string) {
	p.t.Helper()
	p.waitFor(`document.getElementById("dbs-signin-title") !== null && document.querySelector("#dbs-database option[value='` + database + `']") !== null`)
	p.eval(fmt.Sprintf(`(() => {
		const set = (id, v) => { const n = document.getElementById(id); n.value = v; n.dispatchEvent(new Event("change", { bubbles: true })); };
		set("dbs-database", %q); set("dbs-user", %q); set("dbs-password", %q);
		document.getElementById("dbs-authenticate").click(); return true; })()`, database, user, password), nil)
}

// dbStudioRun types sql into the editor and runs it, waiting for its
// results or its error.
func (p *tab) dbStudioRun(sql string) {
	p.t.Helper()
	p.waitFor(`document.getElementById("dbs-editor") !== null && document.getElementById("dbs-run") !== null`)
	p.eval(fmt.Sprintf(`(() => { const e = document.getElementById("dbs-editor"); e.value = %q;
		e.dispatchEvent(new Event("input", { bubbles: true })); document.getElementById("dbs-run").click(); return true; })()`, sql), nil)
	p.waitFor(`document.getElementById("dbs-results-table") !== null || document.getElementById("dbs-error") !== null || document.getElementById("dbs-message") !== null`)
}

// dbStudioSignInJS signs in from an audit's then: the dialog is open when
// it runs.
func dbStudioSignInJS(database, user, password string) string {
	return fmt.Sprintf(`await new Promise((r) => { const end = Date.now() + 20000; const look = () =>
		(document.querySelector("#dbs-database option[value='%[1]s']") || Date.now() > end) ? r() : setTimeout(look, 50); look(); });
	const set = (id, v) => { const n = document.getElementById(id); n.value = v; n.dispatchEvent(new Event("change", { bubbles: true })); };
	set("dbs-database", %[1]q); set("dbs-user", %[2]q); set("dbs-password", %[3]q);
	document.getElementById("dbs-authenticate").click();
	await new Promise((r) => { const end = Date.now() + 30000; const look = () =>
		(document.querySelector("#dbs-explorer #dbs-current") || Date.now() > end) ? r() : setTimeout(look, 50); look(); })`, database, user, password)
}

// dbStudioThroughThePage is one Studio's test: its page at path opens on
// the sign-in dialog; a wrong password is refused with the server's own
// message; the right one opens the Explorer and the editor, which runs a
// script (CREATE TABLE, INSERT, SELECT) whose rows are the results and
// whose table the Explorer then lists; Save puts the query under Saved
// queries. The sign-in and a run are made from the keyboard alone, and the
// sign-in dialog and the signed-in Studio meet WCAG 2.1 AA.
func dbStudioThroughThePage(t *testing.T, path, database, user, password, refused string) {
	t.Helper()
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	page := path + sep + "tab=studio"
	table := fmt.Sprintf("studio_t%d", time.Now().UnixNano()%1e6)
	p := open(t)
	p.navigate(page)
	p.waitFor(`document.getElementById("dbs-signin-title") !== null`)
	var heading string
	p.eval(`document.getElementById("dbs-signin-title").textContent`, &heading)
	if !strings.HasPrefix(heading, "Sign in to ") || !strings.HasSuffix(heading, " Studio") {
		t.Errorf("the sign-in dialog is titled %q", heading)
	}
	var labels []string
	p.eval(`[...document.querySelectorAll(".modal.is-open label, .modal.is-open legend")].map((n) => n.textContent.trim())`, &labels)
	for _, want := range []string{"Database", "Authentication", "Built-in database authentication", "IAM database authentication", "User", "Password"} {
		if !contains(labels, want) {
			t.Errorf("the sign-in dialog's labels %q lack %q", labels, want)
		}
	}

	// A wrong password: the server's refusal, and the dialog stays open.
	p.dbStudioSignIn(database, user, "wrong-"+password)
	p.waitFor(`(() => { const e = document.getElementById("dbs-signin-error"); return e && !e.hidden && e.textContent.length > 0; })()`)
	var msg string
	p.eval(`document.getElementById("dbs-signin-error").textContent`, &msg)
	if !strings.Contains(msg, refused) {
		t.Errorf("a wrong password was refused with %q; want the server's %q", msg, refused)
	}
	// IAM database authentication names the principal it signs in as.
	p.eval(`document.getElementById("dbs-auth-iam").click(); true`, nil)
	var principal string
	p.eval(`document.getElementById("dbs-iam").hidden ? "" : document.getElementById("dbs-iam-principal").textContent`, &principal)
	if !strings.Contains(principal, "Signs in as ") {
		t.Errorf("IAM database authentication says %q", principal)
	}
	p.eval(`document.getElementById("dbs-auth-builtin").click(); true`, nil)

	// The right password.
	p.dbStudioSignIn(database, user, password)
	p.waitFor(`document.querySelector("#dbs-explorer #dbs-current") !== null`)
	var current string
	p.eval(`document.getElementById("dbs-current").textContent`, &current)
	if current != database+" · "+user {
		t.Errorf("the Explorer says %q signed in; want %s · %s", current, database, user)
	}
	p.dbStudioRun(fmt.Sprintf("CREATE TABLE %[1]s (id int PRIMARY KEY, name varchar(20));\nINSERT INTO %[1]s VALUES (1, 'burrow'), (2, NULL);\nSELECT id, name FROM %[1]s ORDER BY id;", table))
	var cells []string
	p.eval(`[...document.querySelectorAll("#dbs-results-table th, #dbs-results-table td")].map((n) => n.textContent)`, &cells)
	if strings.Join(cells, ",") != "id,name,1,burrow,2,NULL" {
		var errText string
		p.eval(`(document.getElementById("dbs-error") || {}).textContent || ""`, &errText)
		t.Errorf("the script's results are %q (%s)", cells, errText)
	}
	// The Explorer lists the new table.
	p.waitFor(fmt.Sprintf(`[...document.querySelectorAll("#dbs-explorer .spanner-explorer-table summary .mono")].some((n) => n.textContent === %q)`, table))
	// A statement the server refuses: its message.
	p.dbStudioRun("SELECT * FROM no_such_table_2208;")
	p.waitFor(`document.getElementById("dbs-error") !== null`)
	p.eval(`document.getElementById("dbs-error").textContent`, &msg)
	if !strings.Contains(msg, "no_such_table_2208") {
		t.Errorf("a refused statement shows %q", msg)
	}
	// Export is offered on the results.
	p.dbStudioRun(fmt.Sprintf("SELECT name FROM %s WHERE id = 1;", table))
	p.waitFor(`document.getElementById("dbs-export") !== null`)
	// Save: a saved query, under Saved queries.
	name := "studio query " + table
	p.eval(`document.getElementById("dbs-save").click(); true`, nil)
	p.waitFor(`document.getElementById("bigtable-save-name") !== null`)
	p.eval(fmt.Sprintf(`(() => { const n = document.getElementById("bigtable-save-name"); n.value = %q;
		document.getElementById("bigtable-save-submit").click(); return true; })()`, name), nil)
	p.waitFor(fmt.Sprintf(`[...document.querySelectorAll("#dbs-explorer-saved button")].some((b) => b.textContent === %q)`, name))

	t.Run("keyboard", func(t *testing.T) {
		k := open(t)
		k.run(reducedMotion())
		k.navigate(page)
		k.waitFor(`document.getElementById("dbs-signin-title") !== null && document.activeElement && document.activeElement.id === "dbs-database"` +
			` && document.querySelector("#dbs-database option[value='` + database + `']") !== null`)
		// A select chooses its option as its name is typed.
		k.run(chromedp.KeyEvent(database))
		k.tabTo(`a.id === "dbs-auth-builtin"`)
		k.tabTo(`a.id === "dbs-user"`)
		k.run(chromedp.KeyEvent(user))
		k.tabTo(`a.id === "dbs-password"`)
		k.run(chromedp.KeyEvent(password), chromedp.KeyEvent(kb.Enter))
		k.waitFor(`document.querySelector("#dbs-explorer #dbs-current") !== null`)
		k.tabTo(`a.id === "dbs-editor"`)
		k.run(chromedp.KeyEvent(fmt.Sprintf("SELECT name FROM %s WHERE id = 1;", table)))
		// Ctrl+Enter runs, as in Google's editor.
		k.run(chromedp.KeyEvent(kb.Enter, chromedp.KeyModifiers(chromedp.ModifierCtrl)))
		k.waitFor(`[...document.querySelectorAll("#dbs-results-table td")].some((n) => n.textContent === "burrow")`)
	})

	auditScreens(t, []a11yPage{
		{"Studio sign-in dialog", page, `document.getElementById("dbs-signin-title") !== null && document.querySelector(".modal.is-open") !== null`, ""},
		{"signed-in Studio", page, `document.getElementById("dbs-signin-title") !== null`, dbStudioSignInJS(database, user, password)},
	})
	p.navigate(page)
	p.waitFor(`document.querySelector("#dbs-explorer #dbs-current") !== null || document.getElementById("dbs-signin-title") !== null`)
}

// TestCloudSQLStudioThroughThePage (#2208), in the sql shard: Cloud SQL
// Studio on the PostgreSQL and MySQL servers `up` runs, signed in as a
// user Add user account's call (users.insert) made with a password.
func TestCloudSQLStudioThroughThePage(t *testing.T) {
	svc := sqlAdminSDK(t)
	project := uniqueProject(t)
	engines := []struct{ service, instance, refused string }{
		{"cloudsql", "postgres", `password authentication failed for user "%s"`},
		{"cloudsql-mysql", "mysql", `Access denied for user '%s'`},
	}
	for _, e := range engines {
		t.Run(e.instance, func(t *testing.T) {
			needService(t, e.service)
			user := fmt.Sprintf("studio%d", time.Now().UnixNano()%1e6)
			password := "Studio-pw-2208"
			op, err := svc.Users.Insert(project, e.instance, &sqladmin.User{Name: user, Password: password, Host: "%"}).Do()
			if err != nil {
				t.Fatalf("users.insert: %v", err)
			}
			waitSQL(t, svc, project, op, 2*time.Minute)
			t.Cleanup(func() { _, _ = svc.Users.Delete(project, e.instance).Name(user).Host("%").Do() })
			dbStudioThroughThePage(t, "/cloudsql-instances/"+e.instance+"?project="+url.QueryEscape(project), "cloudburrow", user, password,
				fmt.Sprintf(e.refused, user))
		})
	}
}

// TestAlloyDBStudioThroughThePage (#2208), in the sql shard: AlloyDB
// Studio on a cluster Create cluster makes, signed in to its primary as
// postgres with the password the cluster was created with.
func TestAlloyDBStudioThroughThePage(t *testing.T) {
	needService(t, "alloydb")
	project := uniqueProject(t)
	detail := func(path ...string) string {
		t.Helper()
		q := url.Values{"project": {project}, "name": path}
		code, body := consoleDo(t, http.MethodGet, "/api/detail/alloydb?"+q.Encode(), "")
		if code != http.StatusOK {
			t.Fatalf("read %v through the console API = %d: %s", path, code, body)
		}
		return body
	}
	p := open(t)
	p.navigate("/alloydb/clusters?project=" + project)
	p.clickText("#view button", "Create cluster")
	p.waitFor(`["f-clusterId", "f-password", "f-instanceId"].every((id) => document.getElementById(id) !== null)`)
	p.submitForm(map[string]string{"clusterId": "studio-cl", "password": "Studio-pw-2208", "instanceId": "primary"}, "clusterId")
	cluster := "projects/" + project + "/locations/us-central1/clusters/studio-cl"
	p.waitFor(listedRow("/alloydb/clusters", "studio-cl"))
	deadline := time.Now().Add(6 * time.Minute)
	for !strings.Contains(detail(cluster), `"Ready"`) || strings.Contains(detail(cluster), `"Creating"`) {
		if time.Now().After(deadline) {
			t.Fatalf("the primary is not ready: %s", detail(cluster))
		}
		time.Sleep(2 * time.Second)
	}
	t.Cleanup(func() {
		b := fmt.Sprintf(`{"Path":[%q],"Action":"delete","Values":{}}`, cluster)
		_, _ = consoleDo(t, http.MethodPost, "/api/actions/alloydb?project="+url.QueryEscape(project), b)
	})
	p.follow(cluster)
	var clusterPath string
	p.eval(`location.pathname + location.search`, &clusterPath)
	p.waitFor(`[...document.querySelectorAll("#view [role=tab]")].some((t) => t.textContent.trim() === "AlloyDB Studio")`)
	dbStudioThroughThePage(t, clusterPath, "postgres", "postgres", "Studio-pw-2208", `password authentication failed for user "postgres"`)
}
