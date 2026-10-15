//go:build browser

package browser

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// TestBigQueryCopyTableAndAvroExportThroughTheForms (#782), in the emulators
// shard, whose instance serves BigQuery and Cloud Storage. On a table's
// page, Copy table with a second table in More source tables answers with
// its job, linked to its page, and the destination previews the rows of
// both; Export to Cloud Storage of the destination in Avro with DEFLATE
// writes an object the console downloads as an Avro container file.
func TestBigQueryCopyTableAndAvroExportThroughTheForms(t *testing.T) {
	needService(t, "bigquery")
	needService(t, "storage")
	p := open(t)
	project := servedProject(t)
	stamp := time.Now().UnixNano() % 1e12
	ds := fmt.Sprintf("browser_copy_%d", stamp)
	bucket := fmt.Sprintf("browser-bq-copy-%d", stamp)
	q := "?project=" + url.QueryEscape(project)
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/bigquery"+q, `{"datasetId":"`+ds+`"}`); code != http.StatusOK {
		t.Fatalf("create a dataset through the console API = %d: %s", code, body)
	}
	t.Cleanup(func() { consoleDo(t, http.MethodDelete, "/api/resources/bigquery"+q+"&name="+ds, "") })
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/storage"+q, `{"name":"`+bucket+`"}`); code != http.StatusOK {
		t.Fatalf("create a bucket through the console API = %d: %s", code, body)
	}
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/objects/storage?"+url.Values{"project": {project}, "name": {bucket, "all.avro"}}.Encode(), "")
		consoleDo(t, http.MethodDelete, "/api/resources/storage"+q+"&name="+bucket, "")
	})
	act := func(path []string, action string, values map[string]string) {
		t.Helper()
		b, _ := json.Marshal(map[string]any{"Path": path, "Action": action, "Values": values})
		if code, body := consoleDo(t, http.MethodPost, "/api/actions/bigquery"+q, string(b)); code != http.StatusOK {
			t.Fatalf("console %s on %v = %d: %s", action, path, code, body)
		}
	}
	schema := `[{"name":"id","type":"INTEGER","mode":"NULLABLE"},{"name":"name","type":"STRING","mode":"NULLABLE"}]`
	for name, rows := range map[string]string{
		"a": `{"id":1,"name":"ann"}` + "\n" + `{"id":2,"name":"bob"}`,
		"b": `{"id":3,"name":"cy"}`,
	} {
		act([]string{ds}, "createtable", map[string]string{"tableId": name, "tableType": "TABLE", "schema": schema})
		act([]string{ds, name}, "insertrows", map[string]string{"rows": rows})
	}

	p.navigate("/bigquery/" + ds + "/a" + q)
	p.clickText("#view .page-actions button", "Copy table")
	p.waitFor(`document.querySelector(".modal #f-moreSources") !== null`)
	p.run(chromedp.SendKeys(`.modal #f-datasetId`, ds, chromedp.ByQuery))
	p.run(chromedp.SendKeys(`.modal #f-tableId`, "all", chromedp.ByQuery))
	p.setField(".modal #f-moreSources", ds+".b")
	p.run(chromedp.Click(`.modal button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(`document.querySelector("#action-result-table a") !== null`)
	var link struct{ Text, Href string }
	p.eval(`(() => { const a = document.querySelector("#action-result-table a");
		return { text: a.textContent, href: a.getAttribute("href") }; })()`, &link)
	if want := "/bigquery-jobs/" + url.PathEscape(link.Text) + q; link.Text == "" || link.Href != want {
		t.Errorf("the copy's result links %q to %q, want %q", link.Text, link.Href, want)
	}
	var cells []string
	p.eval(`[...document.querySelectorAll("#action-result-table td")].map((c) => c.textContent)`, &cells)
	if strings.Join(cells, "|") != link.Text+"|Succeeded" {
		t.Errorf("the copy's result row is %v, want the job and Succeeded", cells)
	}
	p.clickText(".modal button", "Close")
	p.waitFor(`document.querySelector(".modal") === null`)
	if preview := readDataPage(t, "bigquery", project, ds, "all").rows("preview"); len(preview) != 3 {
		t.Errorf("the multi-source copy's destination previews %v, want the 3 rows of both", preview)
	}

	p.navigate("/bigquery/" + ds + "/all" + q)
	p.clickText("#view .page-actions button", "Export to Cloud Storage")
	p.waitFor(`document.querySelector(".modal #f-uri") !== null`)
	p.run(chromedp.SendKeys(`.modal #f-uri`, "gs://"+bucket+"/all.avro", chromedp.ByQuery))
	p.setField(".modal #f-format", "AVRO")
	p.setField(".modal #f-compression", "DEFLATE")
	p.run(chromedp.Click(`.modal button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(`document.querySelector("#action-result-table a") !== null`)
	p.eval(`[...document.querySelectorAll("#action-result-table td")].map((c) => c.textContent)`, &cells)
	if len(cells) != 3 || cells[1] != "Succeeded" || cells[2] != "1" {
		t.Errorf("the Avro export's result row is %v, want Succeeded and 1 file written", cells)
	}
	p.clickText(".modal button", "Close")
	p.waitFor(`document.querySelector(".modal") === null`)
	code, out := consoleDo(t, http.MethodGet, "/api/objects/storage/download?"+
		url.Values{"project": {project}, "name": {bucket, "all.avro"}}.Encode(), "")
	if code != http.StatusOK || !strings.HasPrefix(out, "Obj\x01") || !strings.Contains(out, "deflate") {
		t.Errorf("the Avro export downloads as %d %.40q, want a deflate Avro container file", code, out)
	}
}
