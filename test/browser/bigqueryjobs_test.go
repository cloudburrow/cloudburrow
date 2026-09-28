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
	"github.com/chromedp/chromedp/kb"
)

// TestBigQueryLoadExportAndJobHistoryThroughTheForms (#993), in the
// emulators shard, whose instance serves BigQuery and Cloud Storage. On a
// dataset's page, Load from Cloud Storage with a URI that does not exist
// shows the API's refusal on the form and makes no table; with a CSV the
// console uploaded to a bucket, a schema built in the editor and one header
// row to skip, it answers with the job, linked to its page, and the table
// holds the rows. On the table's page, Export to Cloud Storage with a pipe
// delimiter writes an object the console downloads. BigQuery's product
// navigation has Datasets and Job history; Job history lists both jobs, the
// load job's page shows its type and output rows, and its Delete job asks
// for the job's ID back and returns to Job history, which no longer lists
// it.
func TestBigQueryLoadExportAndJobHistoryThroughTheForms(t *testing.T) {
	needService(t, "bigquery")
	needService(t, "storage")
	p := open(t)
	project := servedProject(t)
	stamp := time.Now().UnixNano() % 1e12
	ds := fmt.Sprintf("browser_jobs_%d", stamp)
	bucket := fmt.Sprintf("browser-bq-%d", stamp)
	q := "?project=" + url.QueryEscape(project)
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/bigquery"+q, `{"datasetId":"`+ds+`"}`); code != http.StatusOK {
		t.Fatalf("create a dataset through the console API = %d: %s", code, body)
	}
	t.Cleanup(func() { consoleDo(t, http.MethodDelete, "/api/resources/bigquery"+q+"&name="+ds, "") })
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/storage"+q, `{"name":"`+bucket+`"}`); code != http.StatusOK {
		t.Fatalf("create a bucket through the console API = %d: %s", code, body)
	}
	t.Cleanup(func() {
		for _, o := range []string{"people.csv", "out.csv"} {
			consoleDo(t, http.MethodDelete, "/api/objects/storage?"+url.Values{"project": {project}, "name": {bucket, o}}.Encode(), "")
		}
		consoleDo(t, http.MethodDelete, "/api/resources/storage"+q+"&name="+bucket, "")
	})
	uploadObject(t, project, []string{bucket}, "people.csv", "id,name\n1,ann\n2,bob\n")

	p.navigate("/bigquery/" + ds + q)
	p.clickText("#view .page-actions button", "Load from Cloud Storage")
	p.waitFor(`document.querySelector(".modal #f-uris") !== null`)
	p.run(chromedp.SendKeys(`.modal #f-tableId`, "people", chromedp.ByQuery))
	p.setField(".modal #f-uris", "gs://"+bucket+"/missing.csv")
	field := func(label string) string { return fmt.Sprintf(`.modal [aria-label=%q]`, label) }
	p.run(chromedp.SendKeys(field("Field 1 name"), "id", chromedp.ByQuery))
	p.setField(field("Field 1 type"), "INTEGER")
	p.clickText(".modal button", "Add field")
	p.run(chromedp.SendKeys(field("Field 2 name"), "name", chromedp.ByQuery))
	p.setField(".modal #f-skipLeadingRows", "1")
	p.run(chromedp.Click(`.modal button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(`!document.querySelector(".modal .form-error").hidden`)
	var refusal string
	p.eval(`document.querySelector(".modal .form-error").textContent`, &refusal)
	if !strings.Contains(refusal, "missing.csv") || strings.Contains(refusal, "googleapi") {
		t.Errorf("a load of a missing object was refused with %q, want the API's words naming it", refusal)
	}
	p.forgive()
	if tables := readDataPage(t, "bigquery", project, ds).rows("tables"); len(tables) != 0 {
		t.Errorf("the refused load made %v", tables)
	}

	p.setField(".modal #f-uris", "gs://"+bucket+"/people.csv")
	p.run(chromedp.Click(`.modal button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(`document.querySelector("#action-result-table a") !== null`)
	var link struct{ Text, Href string }
	p.eval(`(() => { const a = document.querySelector("#action-result-table a");
		return { text: a.textContent, href: a.getAttribute("href") }; })()`, &link)
	loadID := link.Text
	if want := "/bigquery-jobs/" + url.PathEscape(loadID) + q; loadID == "" || link.Href != want {
		t.Errorf("the load's result links %q to %q, want %q", loadID, link.Href, want)
	}
	var cells []string
	p.eval(`[...document.querySelectorAll("#action-result-table td")].map((c) => c.textContent)`, &cells)
	if strings.Join(cells, "|") != loadID+"|Succeeded|2|1" {
		t.Errorf("the load's result row is %v, want the job, Succeeded, 2 output rows, 1 input file", cells)
	}
	p.clickText(".modal button", "Close")
	p.waitFor(`document.querySelector(".modal") === null`)
	if preview := readDataPage(t, "bigquery", project, ds, "people").rows("preview"); len(preview) != 2 {
		t.Errorf("the loaded table previews %v, want its 2 rows", preview)
	}

	p.navigate("/bigquery/" + ds + "/people" + q)
	p.clickText("#view .page-actions button", "Export to Cloud Storage")
	p.waitFor(`document.querySelector(".modal #f-uri") !== null`)
	p.run(chromedp.SendKeys(`.modal #f-uri`, "gs://"+bucket+"/out.csv", chromedp.ByQuery))
	p.setField(".modal #f-fieldDelimiter", "|")
	p.run(chromedp.Click(`.modal button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(`document.querySelector("#action-result-table a") !== null`)
	var exportID string
	p.eval(`document.querySelector("#action-result-table a").textContent`, &exportID)
	p.clickText(".modal button", "Close")
	p.waitFor(`document.querySelector(".modal") === null`)
	code, out := consoleDo(t, http.MethodGet, "/api/objects/storage/download?"+
		url.Values{"project": {project}, "name": {bucket, "out.csv"}}.Encode(), "")
	if lines := strings.Split(strings.TrimSpace(out), "\n"); code != http.StatusOK || len(lines) != 3 || lines[0] != "id|name" {
		t.Errorf("the export downloads as %d %q, want a header and two rows delimited by |", code, out)
	}

	// Job history: the product's second page.
	p.navigate("/bigquery-jobs" + q)
	for _, id := range []string{loadID, exportID} {
		p.waitFor(fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q))`, id))
	}
	var title string
	p.eval(`document.querySelector("#view h1").textContent`, &title)
	if !strings.Contains(title, "Job history") {
		t.Errorf("the Job history page is titled %q", title)
	}

	p.navigate("/bigquery-jobs/" + url.PathEscape(loadID) + q)
	p.waitFor(`document.querySelector("#view").textContent.includes("Output rows")`)
	var text string
	p.eval(`document.querySelector("#view").textContent`, &text)
	if !strings.Contains(text, "LOAD") || !strings.Contains(text, project+"."+ds+".people") {
		t.Errorf("the load job's page does not show its type and destination: %.400s", text)
	}
	p.clickText("#view .page-actions button", "Delete job")
	p.waitFor(`document.activeElement === document.querySelector(".modal #confirm-input")`)
	p.run(chromedp.SendKeys(`.modal #confirm-input`, loadID, chromedp.ByQuery), chromedp.KeyEvent(kb.Enter))
	p.waitFor(`document.querySelector(".modal") === null && location.pathname === "/bigquery-jobs"`)
	code, body := consoleDo(t, http.MethodGet, "/api/resources/bigquery-jobs"+q, "")
	var list struct{ Items []struct{ Name string } }
	_ = json.Unmarshal([]byte(body), &list)
	for _, it := range list.Items {
		if it.Name == loadID {
			t.Errorf("Job history still lists %s after Delete job (%d)", loadID, code)
		}
	}
}
