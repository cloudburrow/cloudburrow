//go:build browser

package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	secretmanager "cloud.google.com/go/secretmanager/apiv1"
	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"github.com/chromedp/chromedp"
	"google.golang.org/api/option"
	sqladmin "google.golang.org/api/sqladmin/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// The Cloud SQL Admin API, the Backup and DR Service and Secret Manager, as
// the compat harness names them; the sql shard's browser step exports them.
const (
	envSQLAdmin = "CLOUDBURROW_TEST_SQLADMIN"
	envBackupDR = "CLOUDBURROW_TEST_BACKUPDR"
)

func sqlAdminSDK(t *testing.T) *sqladmin.Service {
	t.Helper()
	svc, err := sqladmin.NewService(context.Background(), option.WithEndpoint("http://"+endpoint(t, envSQLAdmin)+"/"),
		option.WithoutAuthentication(), option.WithHTTPClient(http.DefaultClient))
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

// waitSQL polls a Cloud SQL operation until it is done, failing on its error.
func waitSQL(t *testing.T, svc *sqladmin.Service, project string, op *sqladmin.Operation, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for op.Status != "DONE" {
		if time.Now().After(deadline) {
			t.Fatalf("operation %s (%s) is not done after %v", op.Name, op.OperationType, within)
		}
		time.Sleep(time.Second)
		var err error
		if op, err = svc.Operations.Get(project, op.Name).Do(); err != nil {
			t.Fatal(err)
		}
	}
	if op.Error != nil && len(op.Error.Errors) > 0 {
		t.Fatalf("operation %s (%s): %s", op.Name, op.OperationType, op.Error.Errors[0].Message)
	}
}

// pageAction opens one of a resource page's actions, sets its fields, and
// submits it. confirm, when not empty, is the name the action's
// confirmation asks to be typed back. It returns the text of the rows the
// action answers with, "" when it answers none.
func (p *tab) pageAction(label string, values map[string]string, confirm string) string {
	p.t.Helper()
	p.waitFor(`document.querySelector(".modal") === null`)
	p.clickText("#view .page-actions button", label)
	p.waitFor(`document.querySelectorAll(".modal").length === 1 && document.querySelector(".modal.is-open form") !== null`)
	for id, v := range values {
		p.eval(fmt.Sprintf(`(() => { const f = document.querySelector(".modal.is-open #f-%s");
			if (f.type === "checkbox") { f.checked = %q === "true"; f.dispatchEvent(new Event("change", { bubbles: true })); return true; }
			f.value = %q; f.dispatchEvent(new Event("input", { bubbles: true })); f.dispatchEvent(new Event("change", { bubbles: true })); return true; })()`, id, v, v), nil)
	}
	p.run(chromedp.Click(`.modal.is-open button[type="submit"]`, chromedp.ByQuery))
	if confirm != "" {
		p.confirmByName(confirm)
	}
	p.waitFor(`document.querySelector(".modal.is-open") === null || document.querySelector("#action-result-table") !== null ||
		[...document.querySelectorAll(".modal.is-open .form-error")].some((e) => !e.hidden && e.textContent)`)
	var res struct{ Rows, Error string }
	p.eval(`(() => { const t = document.querySelector("#action-result-table");
		const e = [...document.querySelectorAll(".modal.is-open .form-error")].filter((e) => !e.hidden).map((e) => e.textContent).join(" ");
		return { Rows: t ? t.textContent : "", Error: e }; })()`, &res)
	if res.Error != "" {
		p.t.Fatalf("%s was refused: %s", label, res.Error)
	}
	if res.Rows != "" {
		p.clickText(".modal.is-open .modal-actions button", "Close")
	}
	p.waitFor(`document.querySelector(".modal") === null`)
	return res.Rows
}

// TestCloudSQLInstanceActionsThroughThePage (#1330, #1421, #1462), in the
// sql shard. The Instances page had a provider but no route, so none of
// these was reachable from the console. From the drawer's Cloud SQL row and
// its Instances page:
//   - Create instance's database versions are every served one, PostgreSQL
//     9.6 to 18 and MySQL 8.0 and 8.4;
//   - on an instance made with the Data API allowed, Run SQL (Data API)
//     answers executeSql's rows, read with a Secret Manager password;
//   - Edit project backup changes a backup GetBackup then reads, and Delete
//     project backup, confirmed by the instance's name, deletes it;
//   - a parallel Export writes a folder, and a parallel Import with Clean and
//     If exists, confirmed, loads it back, each a done operation;
//   - Pre-check upgrade answers its findings, and Upgrade with Include
//     replicas, confirmed, makes the instance POSTGRES_17.
func TestCloudSQLInstanceActionsThroughThePage(t *testing.T) {
	needService(t, "cloudsql-instances")
	svc := sqlAdminSDK(t)
	project := servedProject(t)
	ctx := context.Background()

	p := open(t)
	p.navigateByDrawer("Databases", "/cloudsql", "Cloud SQL")
	p.clickText("#product-nav a", "Instances")
	p.waitFor(`location.pathname === "/cloudsql-instances"`)

	// The versions Create instance offers.
	p.clickText("#view button", "Create instance")
	p.waitFor(`document.querySelector("#f-databaseVersion") !== null`)
	var versions []string
	p.eval(`[...document.querySelector("#f-databaseVersion").options].map((o) => o.value)`, &versions)
	for _, want := range []string{"POSTGRES_9_6", "POSTGRES_16", "POSTGRES_18", "MYSQL_8_0", "MYSQL_8_4"} {
		if !contains(versions, want) {
			t.Errorf("Create instance offers versions %q; want %s among them", versions, want)
		}
	}

	name := fmt.Sprintf("browser-pg-%d", time.Now().UnixNano()%1e6)
	op, err := svc.Instances.Insert(project, &sqladmin.DatabaseInstance{Name: name, DatabaseVersion: "POSTGRES_16",
		RootPassword: "Root-pass1", Settings: &sqladmin.Settings{Tier: "db-custom-1-3840", DataApiAccess: "ALLOW_DATA_API"}}).Do()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if d, err := svc.Instances.Delete(project, name).Do(); err == nil {
			waitSQL(t, svc, project, d, 5*time.Minute)
		}
	})
	waitSQL(t, svc, project, op, 10*time.Minute)
	op, err = svc.Users.Insert(project, name, &sqladmin.User{Name: "browserapi", Password: "Browser-pass1"}).Do()
	if err != nil {
		t.Fatal(err)
	}
	waitSQL(t, svc, project, op, time.Minute)

	sm, err := secretmanager.NewClient(ctx, option.WithEndpoint(endpoint(t, envSecrets)), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sm.Close() })
	secret := "projects/" + project + "/secrets/" + name + "-pw"
	if _, err := sm.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{Parent: "projects/" + project, SecretId: name + "-pw",
		Secret: &secretmanagerpb.Secret{Replication: &secretmanagerpb.Replication{
			Replication: &secretmanagerpb.Replication_Automatic_{Automatic: &secretmanagerpb.Replication_Automatic{}}}}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sm.DeleteSecret(context.Background(), &secretmanagerpb.DeleteSecretRequest{Name: secret}) })
	ver, err := sm.AddSecretVersion(ctx, &secretmanagerpb.AddSecretVersionRequest{Parent: secret,
		Payload: &secretmanagerpb.SecretPayload{Data: []byte("Browser-pass1")}})
	if err != nil {
		t.Fatal(err)
	}

	// A project backup of the instance, for Edit and Delete project backup.
	op, err = svc.Backups.CreateBackup("projects/"+project, &sqladmin.Backup{Instance: name, Description: "browser-" + name}).Do()
	if err != nil {
		t.Fatal(err)
	}
	waitSQL(t, svc, project, op, 5*time.Minute)
	list, err := svc.Backups.ListBackups("projects/" + project).Filter(`instance="` + name + `"`).Do()
	if err != nil {
		t.Fatal(err)
	}
	var backup string
	for _, b := range list.Backups {
		if b.Description == "browser-"+name {
			backup = b.Name
		}
	}
	if backup == "" {
		t.Fatalf("CreateBackup made no backup of %s: %+v", name, list.Backups)
	}
	t.Cleanup(func() { _, _ = svc.Backups.DeleteBackup(backup).Do() })

	page := "/cloudsql-instances/" + name + "?project=" + project
	p.navigate(page)
	p.waitFor(`document.querySelectorAll("#view .page-actions button").length > 0`)

	// Run SQL (Data API).
	rows := p.pageAction("Run SQL (Data API)", map[string]string{"database": "postgres", "user": "browserapi",
		"passwordSecretVersion": ver.GetName(), "sql": "SELECT 'browser-' || n AS v FROM generate_series(1, 2) AS n", "rowLimit": "10"}, "")
	if !strings.Contains(rows, "browser-1") || !strings.Contains(rows, "browser-2") {
		t.Errorf("Run SQL (Data API) answered %q; want the rows browser-1 and browser-2", rows)
	}

	// Edit project backup, then Delete project backup.
	p.navigate(page)
	p.pageAction("Edit project backup", map[string]string{"backup": backup, "description": "edited in the browser"}, "")
	if b, err := svc.Backups.GetBackup(backup).Do(); err != nil || b.Description != "edited in the browser" {
		t.Errorf("after Edit project backup, GetBackup = %+v, %v; want the new description", b, err)
	}
	p.navigate(page)
	p.pageAction("Delete project backup", map[string]string{"backup": backup}, name)
	if _, err := svc.Backups.GetBackup(backup).Do(); err == nil {
		t.Error("Delete project backup left the backup GetBackup reads")
	}

	// A parallel Export to the instance's Cloud Storage, and a parallel
	// Import of the folder it wrote.
	bucket := name + "-dumps"
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/storage?project="+project, `{"name":"`+bucket+`"}`); code != http.StatusOK {
		t.Fatalf("create bucket %s through the console API = %d: %s", bucket, code, body)
	}
	t.Cleanup(func() { emptyAndDeleteBucket(t, project, bucket) })
	folder := "gs://" + bucket + "/parallel"
	p.navigate(page)
	p.pageAction("Export", map[string]string{"uri": folder, "fileType": "SQL", "database": "postgres", "parallel": "true", "threads": "2"}, "")
	waitLatestSQLOp(t, svc, project, name, "EXPORT")
	p.navigate(page)
	p.pageAction("Import", map[string]string{"uri": folder, "fileType": "SQL", "database": "postgres", "parallel": "true", "threads": "2",
		"clean": "true", "ifExists": "true"}, name)
	waitLatestSQLOp(t, svc, project, name, "IMPORT")

	// Pre-check upgrade, then Upgrade.
	p.navigate(page)
	if rows := p.pageAction("Pre-check upgrade", map[string]string{"databaseVersion": "POSTGRES_17"}, ""); !strings.Contains(rows, "Nothing stops the upgrade") {
		t.Errorf("Pre-check upgrade answered %q", rows)
	}
	p.navigate(page)
	p.pageAction("Upgrade", map[string]string{"databaseVersion": "POSTGRES_17", "includeReplicas": "true"}, name)
	waitLatestSQLOp(t, svc, project, name, "MAJOR_VERSION_UPGRADE")
	if in, err := svc.Instances.Get(project, name).Do(); err != nil || in.DatabaseVersion != "POSTGRES_17" {
		t.Errorf("after Upgrade, instances.get = %v, %v; want POSTGRES_17", in, err)
	}
}

// waitLatestSQLOp waits for the instance's newest operation, which must be
// of kind, to be done without an error.
func waitLatestSQLOp(t *testing.T, svc *sqladmin.Service, project, instance, kind string) {
	t.Helper()
	ops, err := svc.Operations.List(project).Instance(instance).Do()
	if err != nil || len(ops.Items) == 0 {
		t.Fatalf("operations.list for %s: %v %+v", instance, err, ops)
	}
	if ops.Items[0].OperationType != kind {
		t.Fatalf("%s's newest operation is %s, want %s", instance, ops.Items[0].OperationType, kind)
	}
	waitSQL(t, svc, project, ops.Items[0], 10*time.Minute)
}

// emptyAndDeleteBucket deletes a bucket's objects and then the bucket,
// through the console API.
func emptyAndDeleteBucket(t *testing.T, project, bucket string) {
	for i := 0; i < 5; i++ {
		_, body := consoleDo(t, http.MethodGet, "/api/detail/storage?project="+project+"&name="+bucket, "")
		var d struct {
			Sections []struct {
				Listing struct{ Items []struct{ Name string } }
			}
		}
		_ = json.Unmarshal([]byte(body), &d)
		n := 0
		for _, s := range d.Sections {
			for _, it := range s.Listing.Items {
				consoleDo(t, http.MethodDelete, "/api/resources/storage?project="+project+"&name="+bucket+"/"+it.Name, "")
				n++
			}
		}
		if code, _ := consoleDo(t, http.MethodDelete, "/api/resources/storage?project="+project+"&name="+bucket, ""); code == http.StatusOK || n == 0 {
			return
		}
	}
}
