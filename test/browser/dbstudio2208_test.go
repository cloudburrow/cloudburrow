//go:build browser

package browser

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
	"google.golang.org/api/googleapi"
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
	p.expected = append(p.expected, "/api/studio/")
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
			sfx := time.Now().UnixNano() % 1e6
			user := fmt.Sprintf("studio%d", sfx)
			password := "Studio-pw-2208"
			host := "%"
			if e.instance == "postgres" {
				host = ""
			}
			op, err := svc.Users.Insert(project, e.instance, &sqladmin.User{Name: user, Password: password, Host: host}).Do()
			if err != nil {
				t.Fatalf("users.insert: %v", err)
			}
			waitSQL(t, svc, project, op, 2*time.Minute)
			t.Cleanup(func() {
				call := svc.Users.Delete(project, e.instance).Name(user)
				if host != "" {
					call = call.Host(host)
				}
				if op, err := call.Do(); err == nil {
					waitSQL(t, svc, project, op, 2*time.Minute)
				}
			})
			database := "cloudburrow"
			if e.instance == "postgres" {
				// PostgreSQL 15 and later let only a database's owner
				// create in its public schema. A user users.insert makes
				// has CREATEDB, as Google's cloudsqlsuperuser members have
				// (https://docs.cloud.google.com/sql/docs/postgres/users),
				// so it makes a database of its own, signed in through
				// Studio, and owns it.
				database = fmt.Sprintf("studio_db%d", sfx)
				dbStudioAPI(t, "cloudsql-instances", project, e.instance, "cloudburrow", user, password, "CREATE DATABASE "+database+";")
				t.Cleanup(func() {
					if op, err := svc.Databases.Delete(project, e.instance, database).Do(); err == nil {
						waitSQL(t, svc, project, op, 2*time.Minute)
					}
				})
			}
			dbStudioThroughThePage(t, "/cloudsql-instances/"+e.instance+"?project="+url.QueryEscape(project), database, user, password,
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
	// A tab of its own: the wait above may outlive the first one's.
	p = open(t)
	p.navigate("/alloydb/clusters?project=" + project)
	p.follow(cluster)
	var clusterPath string
	p.eval(`location.pathname + location.search`, &clusterPath)
	p.waitFor(`[...document.querySelectorAll("#view [role=tab]")].some((t) => t.textContent.trim() === "AlloyDB Studio")`)
	dbStudioThroughThePage(t, clusterPath, "postgres", "postgres", "Studio-pw-2208", `password authentication failed for user "postgres"`)

	// IAM database authentication: the console's principal, once it is an
	// IAM user of the cluster (Add user account, Cloud IAM), signs in
	// without a password, as its role.
	t.Run("iam", func(t *testing.T) {
		code, body := consoleDo(t, http.MethodGet, "/api/status", "")
		var st struct {
			Identity struct{ ServiceAccount string }
		}
		if code != http.StatusOK || json.Unmarshal([]byte(body), &st) != nil || st.Identity.ServiceAccount == "" {
			t.Fatalf("the console's status = %d: %s", code, body)
		}
		role := strings.TrimSuffix(st.Identity.ServiceAccount, ".gserviceaccount.com")
		k := open(t)
		k.navigate(clusterPath + "&tab=studio")
		k.waitFor(`document.querySelector("#dbs-database option[value='postgres']") !== null`)
		// Not yet an IAM user: refused.
		k.expected = append(k.expected, "/api/studio/")
		signInIAM := `(() => { const d = document.getElementById("dbs-database"); d.value = "postgres";
			document.getElementById("dbs-auth-iam").click(); document.getElementById("dbs-authenticate").click(); return true; })()`
		k.eval(signInIAM, nil)
		k.waitFor(`(() => { const e = document.getElementById("dbs-signin-error"); return e && !e.hidden && e.textContent.length > 0; })()`)
		var msg string
		k.eval(`document.getElementById("dbs-signin-error").textContent`, &msg)
		if !strings.Contains(msg, `IAM authentication failed for user "`+role+`"`) {
			t.Errorf("an IAM sign-in as no IAM user was refused with %q", msg)
		}
		b, _ := json.Marshal(map[string]any{"Path": []string{cluster}, "Action": "add-user",
			"Values": map[string]string{"type": "ALLOYDB_IAM_USER", "user": st.Identity.ServiceAccount}})
		if code, body := consoleDo(t, http.MethodPost, "/api/actions/alloydb?project="+url.QueryEscape(project), string(b)); code != http.StatusOK {
			t.Fatalf("Add user account (Cloud IAM) = %d: %s", code, body)
		}
		k.eval(signInIAM, nil)
		k.waitFor(`document.querySelector("#dbs-explorer #dbs-current") !== null`)
		var current string
		k.eval(`document.getElementById("dbs-current").textContent`, &current)
		if current != "postgres · "+role {
			t.Errorf("signed in with IAM as %q; want postgres · %s", current, role)
		}
		k.dbStudioRun("SELECT current_user;")
		var cells []string
		k.eval(`[...document.querySelectorAll("#dbs-results-table td")].map((n) => n.textContent)`, &cells)
		if len(cells) != 1 || cells[0] != role {
			t.Errorf("current_user under IAM is %q; want %s", cells, role)
		}
	})
}

// dbStudioAPI signs in to a Studio's database as user through the calls
// the page makes (POST /api/studio/{service}), runs sql, and signs out,
// failing on a refusal.
func dbStudioAPI(t *testing.T, service, project, instance, database, user, password, sql string) {
	t.Helper()
	path := "/api/studio/" + service + "?project=" + url.QueryEscape(project) + "&call="
	b, _ := json.Marshal(map[string]any{"instance": instance, "database": database, "user": user, "password": password})
	code, body := consoleDo(t, http.MethodPost, path+"signin", string(b))
	var s struct{ Session string }
	if code != http.StatusOK || json.Unmarshal([]byte(body), &s) != nil || s.Session == "" {
		t.Fatalf("Studio sign-in to %s as %s = %d: %s", database, user, code, body)
	}
	defer func() {
		b, _ := json.Marshal(map[string]any{"instance": instance, "session": s.Session})
		_, _ = consoleDo(t, http.MethodPost, path+"signout", string(b))
	}()
	b, _ = json.Marshal(map[string]any{"instance": instance, "session": s.Session, "sql": sql})
	if code, body := consoleDo(t, http.MethodPost, path+"run", string(b)); code != http.StatusOK || strings.Contains(body, `"error"`) {
		t.Fatalf("Studio run of %q = %d: %s", sql, code, body)
	}
}

// TestCloudSQLStudioIAMSignIn (#2208), in the sql shard: Cloud SQL
// Studio's IAM database authentication on a PostgreSQL instance created
// with cloudsql.iam_authentication on, as Google documents it
// (https://docs.cloud.google.com/sql/docs/postgres/manage-data-using-studio,
// .../add-manage-iam-users). Studio signs in as the console's principal,
// which is a service account here (Google's console signs in as the
// person using it):
//   - before the principal is an IAM user of the instance, the sign-in is
//     refused;
//   - users.insert of type CLOUD_IAM_USER makes a person's IAM user, whose
//     role users.get answers as CLOUD_IAM_USER; it is not in the dialog's
//     built-in User list, and built-in authentication as it, with any
//     password, is refused: an IAM user signs in by token only;
//   - users.insert refuses the principal as a CLOUD_IAM_USER with Google's
//     words, as it is a service account;
//   - added as CLOUD_IAM_SERVICE_ACCOUNT, the principal is refused until
//     it holds roles/cloudsql.instanceUser (granted on the IAM page), then
//     signs in with no password, as the role Google names (its email without
//     .gserviceaccount.com), which current_user answers;
//   - the IAM sign-in is made from the keyboard alone, and the dialog with
//     IAM database authentication chosen meets WCAG 2.1 AA.
func TestCloudSQLStudioIAMSignIn(t *testing.T) {
	needService(t, "cloudsql-instances")
	svc := sqlAdminSDK(t)
	project := servedProject(t)
	name := fmt.Sprintf("studio-iam-%d", time.Now().UnixNano()%1e6)
	op, err := svc.Instances.Insert(project, &sqladmin.DatabaseInstance{Name: name, DatabaseVersion: "POSTGRES_17", RootPassword: "Root-pass1",
		Settings: &sqladmin.Settings{Tier: "db-custom-1-3840",
			DatabaseFlags: []*sqladmin.DatabaseFlags{{Name: "cloudsql.iam_authentication", Value: "on"}}}}).Do()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if d, err := svc.Instances.Delete(project, name).Do(); err == nil {
			waitSQL(t, svc, project, d, 5*time.Minute)
		}
	})
	waitSQL(t, svc, project, op, 10*time.Minute)

	code, body := consoleDo(t, http.MethodGet, "/api/status", "")
	var st struct {
		Identity struct{ ServiceAccount string }
	}
	if code != http.StatusOK || json.Unmarshal([]byte(body), &st) != nil || st.Identity.ServiceAccount == "" {
		t.Fatalf("the console's status = %d: %s", code, body)
	}
	principal := st.Identity.ServiceAccount
	role := strings.TrimSuffix(principal, ".gserviceaccount.com")
	page := "/cloudsql-instances/" + name + "?project=" + url.QueryEscape(project) + "&tab=studio"
	insert := func(u *sqladmin.User) error {
		t.Helper()
		op, err := svc.Users.Insert(project, name, u).Do()
		if err == nil {
			waitSQL(t, svc, project, op, 2*time.Minute)
		}
		return err
	}
	signInIAM := `(() => { const d = document.getElementById("dbs-database"); d.value = "postgres";
		d.dispatchEvent(new Event("change", { bubbles: true }));
		document.getElementById("dbs-auth-iam").click(); document.getElementById("dbs-authenticate").click(); return true; })()`
	signInError := func(p *tab) string {
		p.t.Helper()
		p.waitFor(`(() => { const e = document.getElementById("dbs-signin-error"); return e && !e.hidden && e.textContent.length > 0; })()`)
		var msg string
		p.eval(`document.getElementById("dbs-signin-error").textContent`, &msg)
		return msg
	}

	// Not yet an IAM user: refused.
	p := open(t)
	p.expected = append(p.expected, "/api/studio/")
	p.navigate(page)
	p.waitFor(`document.querySelector("#dbs-database option[value='postgres']") !== null`)
	var says string
	p.eval(`document.getElementById("dbs-auth-iam").click(); document.getElementById("dbs-iam-principal").textContent`, &says)
	if !strings.Contains(says, "Signs in as "+principal) {
		t.Errorf("IAM database authentication says %q; want it to name %s", says, principal)
	}
	p.eval(signInIAM, nil)
	if msg := signInError(p); !strings.Contains(msg, principal+" is not an IAM user of instance "+name) {
		t.Errorf("an IAM sign-in as no IAM user was refused with %q", msg)
	}

	// A person's IAM user: CLOUD_IAM_USER, signing in by token only.
	person := fmt.Sprintf("studio.person%d@example.com", time.Now().UnixNano()%1e6)
	if err := insert(&sqladmin.User{Name: person, Type: "CLOUD_IAM_USER"}); err != nil {
		t.Fatalf("users.insert of %s as CLOUD_IAM_USER: %v", person, err)
	}
	if u, err := svc.Users.Get(project, name, person).Do(); err != nil || u.Type != "CLOUD_IAM_USER" {
		t.Errorf("users.get %s: %+v, %v; want type CLOUD_IAM_USER", person, u, err)
	}
	// The principal is a service account: Google's refusal.
	err = insert(&sqladmin.User{Name: principal, Type: "CLOUD_IAM_USER"})
	var gerr *googleapi.Error
	if !errors.As(err, &gerr) || gerr.Code != http.StatusBadRequest || !strings.Contains(gerr.Message, "create it as type CLOUD_IAM_SERVICE_ACCOUNT") {
		t.Errorf("users.insert of the service account %s as CLOUD_IAM_USER: %v; want 400 naming CLOUD_IAM_SERVICE_ACCOUNT", principal, err)
	}
	p.navigate(page)
	p.waitFor(`document.querySelector("#dbs-database option[value='postgres']") !== null && document.querySelector("#dbs-user option[value='postgres']") !== null`)
	var builtIn []string
	p.eval(`[...document.querySelectorAll("#dbs-user option")].map((o) => o.value)`, &builtIn)
	if contains(builtIn, person) {
		t.Errorf("the built-in User list %q has the IAM user %s", builtIn, person)
	}
	// Built-in authentication as the IAM user's role, which the dialog
	// does not offer, sent as the page sends a sign-in: refused whatever
	// the password, as the server holds the role to one only the
	// connector's token sign-in knows.
	b, _ := json.Marshal(map[string]any{"instance": name, "database": "postgres", "user": person, "password": "Root-pass1"})
	code, body = consoleDo(t, http.MethodPost, "/api/studio/cloudsql-instances?project="+url.QueryEscape(project)+"&call=signin", string(b))
	if code != http.StatusBadRequest || !strings.Contains(body, `password authentication failed for user \"`+person+`\"`) {
		t.Errorf("built-in authentication as the IAM user %s = %d: %s; want the server's refusal", person, code, body)
	}

	// The principal as CLOUD_IAM_SERVICE_ACCOUNT: signed in as its role.
	if err := insert(&sqladmin.User{Name: principal, Type: "CLOUD_IAM_SERVICE_ACCOUNT"}); err != nil {
		t.Fatalf("users.insert of %s as CLOUD_IAM_SERVICE_ACCOUNT: %v", principal, err)
	}
	if u, err := svc.Users.Get(project, name, role).Do(); err != nil || u.Type != "CLOUD_IAM_SERVICE_ACCOUNT" {
		t.Errorf("users.get %s: %+v, %v; want type CLOUD_IAM_SERVICE_ACCOUNT", role, u, err)
	}
	// Without roles/cloudsql.instanceUser, which holds
	// cloudsql.instances.login: refused.
	p.navigate(page)
	p.waitFor(`document.querySelector("#dbs-database option[value='postgres']") !== null`)
	p.eval(signInIAM, nil)
	if msg := signInError(p); !strings.Contains(msg, principal+" does not have cloudsql.instances.login") {
		t.Errorf("an IAM sign-in without roles/cloudsql.instanceUser was refused with %q", msg)
	}
	// Granted on the project, as the IAM page's Grant access saves it.
	iamChange := func(add bool) {
		t.Helper()
		path := "/api/studio/iam?project=" + url.QueryEscape(project) + "&call="
		code, body := consoleDo(t, http.MethodPost, path+"policy", "{}")
		var pol struct{ Etag string }
		if code != http.StatusOK || json.Unmarshal([]byte(body), &pol) != nil || pol.Etag == "" {
			t.Fatalf("the IAM page's policy = %d: %s", code, body)
		}
		change := map[string]any{"etag": pol.Etag, "members": []string{"serviceAccount:" + principal}}
		roles := []map[string]string{{"role": "roles/cloudsql.instanceUser"}}
		if add {
			change["add"] = roles
		} else {
			change["remove"] = roles
		}
		b, _ := json.Marshal(change)
		if code, body := consoleDo(t, http.MethodPost, path+"change", string(b)); code != http.StatusOK {
			t.Fatalf("the IAM page's change (add %v) = %d: %s", add, code, body)
		}
	}
	iamChange(true)
	t.Cleanup(func() { iamChange(false) })
	p.navigate(page)
	p.waitFor(`document.querySelector("#dbs-database option[value='postgres']") !== null`)
	p.eval(signInIAM, nil)
	p.waitFor(`document.querySelector("#dbs-explorer #dbs-current") !== null || (document.getElementById("dbs-signin-error") && !document.getElementById("dbs-signin-error").hidden)`)
	var current string
	p.eval(`(document.getElementById("dbs-current") || {}).textContent || document.getElementById("dbs-signin-error").textContent`, &current)
	if current != "postgres · "+role {
		t.Fatalf("signed in with IAM as %q; want postgres · %s", current, role)
	}
	p.dbStudioRun("SELECT current_user;")
	var cells []string
	p.eval(`[...document.querySelectorAll("#dbs-results-table td")].map((n) => n.textContent)`, &cells)
	if len(cells) != 1 || cells[0] != role {
		t.Errorf("current_user under IAM is %q; want %s", cells, role)
	}

	t.Run("keyboard", func(t *testing.T) {
		k := open(t)
		k.run(reducedMotion())
		// A tab of its own has no sign-in yet: the page opens on the dialog.
		k.navigate(page)
		k.waitFor(`document.getElementById("dbs-signin-title") !== null && document.activeElement && document.activeElement.id === "dbs-database"` +
			` && document.querySelector("#dbs-database option[value='postgres']") !== null`)
		k.run(chromedp.KeyEvent("postgres"))
		k.tabTo(`a.id === "dbs-auth-builtin"`)
		// Arrow keys move a radio group's choice, as in Google's dialog.
		k.run(chromedp.KeyEvent(kb.ArrowDown))
		k.waitFor(`document.getElementById("dbs-auth-iam").checked && !document.getElementById("dbs-iam").hidden`)
		k.tabTo(`a.id === "dbs-authenticate"`)
		k.run(chromedp.KeyEvent(kb.Enter))
		k.waitFor(`document.querySelector("#dbs-explorer #dbs-current") !== null`)
		var current string
		k.eval(`document.getElementById("dbs-current").textContent`, &current)
		if current != "postgres · "+role {
			t.Errorf("signed in from the keyboard as %q; want postgres · %s", current, role)
		}
	})

	auditScreens(t, []a11yPage{
		{"Studio sign-in dialog, IAM database authentication", page,
			`document.getElementById("dbs-signin-title") !== null && document.querySelector("#dbs-database option[value='postgres']") !== null`,
			`document.getElementById("dbs-auth-iam").click();
			await new Promise((r) => { const end = Date.now() + 5000; const look = () =>
				(!document.getElementById("dbs-iam").hidden || Date.now() > end) ? r() : setTimeout(look, 50); look(); })`},
	})
}
