//go:build browser

package browser

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// TestBigQueryLoadFromAFileThroughTheForm (#999), in the emulators shard,
// whose instance serves BigQuery. On a dataset's page, Load from a file
// offers a file control where Load from Cloud Storage has its URIs, and the
// same options. Submitted with no file, the form says the file is required
// and sends nothing. With a CSV chosen, a schema built in the editor and one
// header row to skip, it posts the file to the action upload route and
// answers with the job, linked to its page, and the table holds the file's
// rows.
func TestBigQueryLoadFromAFileThroughTheForm(t *testing.T) {
	needService(t, "bigquery")
	p := open(t)
	project := servedProject(t)
	stamp := time.Now().UnixNano() % 1e12
	ds := fmt.Sprintf("browser_loadfile_%d", stamp)
	q := "?project=" + url.QueryEscape(project)
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/bigquery"+q, `{"datasetId":"`+ds+`"}`); code != http.StatusOK {
		t.Fatalf("create a dataset through the console API = %d: %s", code, body)
	}
	t.Cleanup(func() { consoleDo(t, http.MethodDelete, "/api/resources/bigquery"+q+"&name="+ds, "") })
	file := filepath.Join(t.TempDir(), "people.csv")
	if err := os.WriteFile(file, []byte("id,name\n1,ann\n2,bob\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	p.navigate("/bigquery/" + ds + q)
	p.clickText("#view .page-actions button", "Load from a file")
	p.waitFor(`document.querySelector(".modal #f-file") !== null`)
	var shape struct {
		Type    string
		URIs    bool
		Options []string
	}
	p.eval(`(() => ({ Type: document.querySelector(".modal #f-file").type,
		URIs: document.querySelector(".modal #f-uris") !== null,
		Options: [...document.querySelectorAll(".modal #f-format option")].map((o) => o.value) }))()`, &shape)
	if shape.Type != "file" || shape.URIs || strings.Join(shape.Options, ",") != "CSV,NEWLINE_DELIMITED_JSON,PARQUET" {
		t.Errorf("Load from a file's form is %+v, want a file control, no URIs and Load from Cloud Storage's formats", shape)
	}
	p.run(chromedp.SendKeys(`.modal #f-tableId`, "people", chromedp.ByQuery))
	field := func(label string) string { return fmt.Sprintf(`.modal [aria-label=%q]`, label) }
	p.run(chromedp.SendKeys(field("Field 1 name"), "id", chromedp.ByQuery))
	p.setField(field("Field 1 type"), "INTEGER")
	p.clickText(".modal button", "Add field")
	p.run(chromedp.SendKeys(field("Field 2 name"), "name", chromedp.ByQuery))
	p.setField(".modal #f-skipLeadingRows", "1")

	p.run(chromedp.Click(`.modal button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(`!document.querySelector(".modal #e-file").hidden`)
	if sent := p.sent(http.MethodPost, "/api/actions/bigquery/upload"); len(sent) != 0 {
		t.Fatalf("the form with no file sent %v", sent)
	}

	p.run(chromedp.SetUploadFiles(`.modal #f-file`, []string{file}, chromedp.ByQuery))
	p.run(chromedp.Click(`.modal button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(`document.querySelector("#action-result-table a") !== null`)
	var link struct{ Text, Href string }
	p.eval(`(() => { const a = document.querySelector("#action-result-table a");
		return { text: a.textContent, href: a.getAttribute("href") }; })()`, &link)
	if want := "/bigquery-jobs/" + url.PathEscape(link.Text) + q; link.Text == "" || link.Href != want {
		t.Errorf("the load's result links %q to %q, want %q", link.Text, link.Href, want)
	}
	var cells []string
	p.eval(`[...document.querySelectorAll("#action-result-table td")].map((c) => c.textContent)`, &cells)
	if len(cells) < 3 || cells[1] != "Succeeded" || cells[2] != "2" {
		t.Errorf("the load's result row is %v, want the job, Succeeded and 2 output rows", cells)
	}
	if sent := p.sent(http.MethodPost, "/api/actions/bigquery/upload"); len(sent) != 1 {
		t.Errorf("the load sent %d uploads, want 1: %v", len(sent), sent)
	}
	p.clickText(".modal button", "Close")
	p.waitFor(`document.querySelector(".modal") === null`)
	if preview := readDataPage(t, "bigquery", project, ds, "people").rows("preview"); len(preview) != 2 {
		t.Errorf("the loaded table previews %v, want the file's 2 rows", preview)
	}
}
