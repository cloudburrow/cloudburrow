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

// TestBigQueryEditDatasetThroughTheForm (#1205), in the emulators shard.
// The dataset's page offers Edit dataset, opened with the dataset's
// description; it changes the friendly name, description, a label, the
// default table expiration and the time travel window, and the dataset's
// page, read again, holds each (its Edit dataset form is drawn from the
// dataset as saved).
func TestBigQueryEditDatasetThroughTheForm(t *testing.T) {
	needService(t, "bigquery")
	p := open(t)
	project := servedProject(t)
	ds := fmt.Sprintf("browser_dsedit_%d", time.Now().UnixNano()%1e12)
	q := "?project=" + url.QueryEscape(project)
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/bigquery"+q, `{"datasetId":"`+ds+`","description":"before"}`); code != http.StatusOK {
		t.Fatalf("create a dataset through the console API = %d: %s", code, body)
	}
	t.Cleanup(func() { consoleDo(t, http.MethodDelete, "/api/resources/bigquery"+q+"&name="+ds, "") })

	p.navigate("/bigquery/" + ds + q)
	p.clickText("#view .page-actions button", "Edit dataset")
	p.waitFor(`document.querySelector(".modal #f-description") !== null && document.querySelector(".modal #f-description").value === "before"`)
	p.setField(".modal #f-friendlyName", "Browser dataset")
	p.setField(".modal #f-description", "after")
	p.setField(".modal #f-labels", "team=data")
	p.setField(".modal #f-defaultTableExpiration", "2")
	p.setField(".modal #f-maxTimeTravelHours", "72")
	p.run(chromedp.Click(chromedp.CSS(`.modal button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal") === null`)

	_, body := consoleDo(t, http.MethodGet, "/api/detail/bigquery?"+url.Values{"project": {project}, "name": {ds}}.Encode(), "")
	for _, want := range []string{`"Browser dataset"`, `"after"`, `team`, `"72"`} {
		if !strings.Contains(body, want) {
			t.Errorf("after Edit dataset the dataset's page does not hold %s: %.1500s", want, body)
		}
	}
	if strings.Contains(body, `"before"`) {
		t.Errorf("after Edit dataset the page still holds the old description: %.1500s", body)
	}
}
