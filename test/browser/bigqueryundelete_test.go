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
)

// TestBigQueryUndeleteDatasetThroughTheList (#1552), in the emulators shard.
// A dataset deleted from the console comes back from the BigQuery list's
// Undelete dataset, which sends datasets.undelete for the ID typed: the list
// holds it again, with the description it had.
func TestBigQueryUndeleteDatasetThroughTheList(t *testing.T) {
	needService(t, "bigquery")
	p := open(t)
	project := servedProject(t)
	ds := fmt.Sprintf("browser_undelete_%d", time.Now().UnixNano()%1e12)
	q := "?project=" + url.QueryEscape(project)
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/bigquery"+q, `{"datasetId":"`+ds+`","description":"kept"}`); code != http.StatusOK {
		t.Fatalf("create a dataset through the console API = %d: %s", code, body)
	}
	t.Cleanup(func() { consoleDo(t, http.MethodDelete, "/api/resources/bigquery"+q+"&name="+ds, "") })
	if code, body := consoleDo(t, http.MethodDelete, "/api/resources/bigquery"+q+"&name="+ds, ""); code != http.StatusOK {
		t.Fatalf("delete the dataset through the console API = %d: %s", code, body)
	}

	p.navigate("/bigquery" + q)
	p.clickText("#view button", "Undelete dataset")
	p.waitFor(`document.querySelector(".modal.is-open #f-dataset") !== null`)
	p.setField(".modal.is-open #f-dataset", ds)
	p.run(chromedp.Click(`.modal.is-open button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(`document.querySelector(".modal") === null`)
	p.navigate("/bigquery" + q)
	p.waitFor(fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q))`, ds))
	_, body := consoleDo(t, http.MethodGet, "/api/detail/bigquery?"+url.Values{"project": {project}, "name": {ds}}.Encode(), "")
	if !strings.Contains(body, `"kept"`) {
		t.Errorf("the undeleted dataset's page does not hold its description: %.1500s", body)
	}
	if sent := p.posts("/api/actions/bigquery"); len(sent) != 1 {
		t.Errorf("Undelete dataset sent %d requests, want 1: %v", len(sent), sent)
	}
}
