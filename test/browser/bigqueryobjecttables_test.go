//go:build browser

package browser

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

// grantThroughConsole grants member role on service's resource at path
// through the console's permissions API, with the etag its page reads,
// and removes the grant when the test ends.
func grantThroughConsole(t *testing.T, service, project string, path []string, member, role string) {
	t.Helper()
	changePermission(t, service, project, path, "Grant", map[string]any{"Members": []string{member}, "Role": role})
	t.Cleanup(func() {
		changePermission(t, service, project, path, "Remove", map[string]any{"Member": member, "Role": role})
	})
}

// changePermission sends one change (Grant or Remove) of the Permissions
// tab of service's resource at path.
func changePermission(t *testing.T, service, project string, path []string, kind string, change map[string]any) {
	t.Helper()
	v := url.Values{"project": {project}, "name": path}
	code, body := consoleDo(t, http.MethodGet, "/api/detail/"+service+"?"+v.Encode(), "")
	var d struct {
		Sections []struct {
			Permissions *struct{ Etag string }
		}
	}
	if err := json.Unmarshal([]byte(body), &d); code != http.StatusOK || err != nil {
		t.Fatalf("read %s %v = %d (%v): %.300s", service, path, code, err, body)
	}
	etag, found := "", false
	for _, s := range d.Sections {
		if s.Permissions != nil {
			etag, found = s.Permissions.Etag, true
		}
	}
	if !found {
		t.Fatalf("%s %v has no Permissions section", service, path)
	}
	b, _ := json.Marshal(map[string]any{"Path": path, "Etag": etag, kind: change})
	if code, body := consoleDo(t, http.MethodPost, "/api/permissions/"+service+"?project="+url.QueryEscape(project), string(b)); code != http.StatusOK {
		t.Errorf("%s %v on %s %v = %d: %s", kind, change, service, path, code, body)
	}
}

// TestBigQueryObjectTablePage (#1783), in the emulators shard, whose
// instance serves BigQuery and Cloud Storage. A Cloud resource connection
// made by the console, whose service account is granted Storage Object
// Viewer on a bucket and whose caller holds BigQuery Connection User,
// backs Create object table on a dataset's page; the dataset lists the
// table as EXTERNAL, and the table's page shows the fixed schema, the
// connection, the URIs, the MANUAL cache mode and the staleness, and a
// Preview of one row per object. A new object is not previewed until
// Refresh metadata cache, after which it is.
func TestBigQueryObjectTablePage(t *testing.T) {
	needService(t, "bigquery")
	needService(t, "storage")
	p := open(t)
	project := servedProject(t)
	stamp := time.Now().UnixNano() % 1e12
	ds := fmt.Sprintf("browser_obj_%d", stamp)
	bucket := fmt.Sprintf("browser-bq-obj-%d", stamp)
	conn := fmt.Sprintf("browser_obj_%d", stamp)
	q := "?project=" + url.QueryEscape(project)
	act := func(path []string, action string, values map[string]string) {
		t.Helper()
		b, _ := json.Marshal(map[string]any{"Path": path, "Action": action, "Values": values})
		if code, body := consoleDo(t, http.MethodPost, "/api/actions/bigquery"+q, string(b)); code != http.StatusOK {
			t.Fatalf("console %s on %v = %d: %s", action, path, code, body)
		}
	}
	put := func(name, data string) {
		t.Helper()
		uploadObject(t, project, []string{bucket, "img"}, name, data)
	}
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/bigquery"+q, `{"datasetId":"`+ds+`"}`); code != http.StatusOK {
		t.Fatalf("create a dataset through the console API = %d: %s", code, body)
	}
	t.Cleanup(func() { consoleDo(t, http.MethodDelete, "/api/resources/bigquery"+q+"&name="+ds, "") })
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/storage"+q, `{"name":"`+bucket+`"}`); code != http.StatusOK {
		t.Fatalf("create a bucket through the console API = %d: %s", code, body)
	}
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/objects/storage?"+url.Values{"project": {project}, "name": {bucket, "img/a.txt"}}.Encode(), "")
		consoleDo(t, http.MethodDelete, "/api/objects/storage?"+url.Values{"project": {project}, "name": {bucket, "img/b.txt"}}.Encode(), "")
		consoleDo(t, http.MethodDelete, "/api/resources/storage"+q+"&name="+bucket, "")
	})
	act(nil, "createconnection", map[string]string{"connection": conn})
	t.Cleanup(func() { act(nil, "deleteconnection", map[string]string{"connection": conn}) })
	_, list := consoleDo(t, http.MethodGet, "/api/resources/bigquery"+q, "")
	sa := regexp.MustCompile(regexp.QuoteMeta(conn) + `"[^}]*service account ([^"\\]+)`).FindStringSubmatch(list)
	if sa == nil {
		t.Fatalf("the datasets page names no service account for %s: %.500s", conn, list)
	}
	grantThroughConsole(t, "storage", project, []string{bucket}, "serviceAccount:"+sa[1], "roles/storage.objectViewer")
	grantThroughConsole(t, "iam", project, []string{project}, "serviceAccount:cloudburrow-local@"+project+".iam.gserviceaccount.com",
		"roles/bigquery.connectionUser")
	put("a.txt", "hello")

	p.navigate("/bigquery/" + ds + q)
	p.pageAction("Create object table", map[string]string{"tableId": "objs", "connection": project + ".us." + conn,
		"uris": "gs://" + bucket + "/img/*", "cacheMode": "MANUAL", "stalenessMinutes": "240"}, "")
	if rows := readDataPage(t, "bigquery", project, ds).rows("tables"); rows["objs"]["Type"] != "EXTERNAL" {
		t.Errorf("the dataset lists the object table as %v, want EXTERNAL", rows["objs"])
	}

	p.navigate("/bigquery/" + ds + "/objs" + q)
	p.waitFor(`[...document.querySelectorAll("#view .page-actions button")].some((b) => b.textContent.includes("Refresh metadata cache"))`)
	var schema string
	p.eval(`document.querySelector("#view").textContent`, &schema)
	for _, want := range []string{"An object table's schema is fixed", "content_type", "md5_hash", "metadata.name", "TIMESTAMP"} {
		if !strings.Contains(schema, want) {
			t.Errorf("the object table's Schema tab does not show %q", want)
		}
	}
	p.clickText("#view [role=tab]", "Details")
	p.waitFor(`document.querySelector("#view").textContent.includes("Metadata cache mode")`)
	var text string
	p.eval(`document.querySelector("#view").textContent`, &text)
	for _, want := range []string{"Connection ID", project + ".us." + conn, "gs://" + bucket + "/img/*", "MANUAL", "Max staleness", "0-0 0 4:0:0"} {
		if !strings.Contains(text, want) {
			t.Errorf("the object table's Details tab does not show %q", want)
		}
	}
	p.clickText("#view [role=tab]", "Preview")
	p.waitFor(`document.querySelector("#view").textContent.includes("gs://` + bucket + `/img/a.txt")`)
	page := readDataPage(t, "bigquery", project, ds, "objs")
	if n := len(page.rows("preview")); n != 1 {
		t.Errorf("the Preview has %d rows, want 1", n)
	}

	put("b.txt", "world")
	if n := len(readDataPage(t, "bigquery", project, ds, "objs").rows("preview")); n != 1 {
		t.Errorf("before Refresh metadata cache the Preview has %d rows, want the cached 1", n)
	}
	p.navigate("/bigquery/" + ds + "/objs" + q)
	p.clickText("#view .page-actions button", "Refresh metadata cache")
	p.clickText("#view [role=tab]", "Preview")
	p.waitFor(`document.querySelector("#view").textContent.includes("gs://` + bucket + `/img/b.txt")`)
	if n := len(readDataPage(t, "bigquery", project, ds, "objs").rows("preview")); n != 2 {
		t.Errorf("after Refresh metadata cache the Preview has %d rows, want 2", n)
	}
}
