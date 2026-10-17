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

// TestBigQueryModelsThroughThePage (#1554, #1465), in the emulators shard.
// A LINEAR_REG model that CREATE MODEL trains in the Read-write editor is a
// row of the dataset's Models section, with its type (models.list); its page
// shows its features (models.get and ML.WEIGHTS); Edit model sets its
// description and friendly name (models.patch); Delete model on the page,
// confirmed by the model's name, drops it and returns to the dataset
// (models.delete).
func TestBigQueryModelsThroughThePage(t *testing.T) {
	needService(t, "bigquery")
	p := open(t)
	project := servedProject(t)
	ds := fmt.Sprintf("browser_models_%d", time.Now().UnixNano()%1e12)
	q := "?project=" + url.QueryEscape(project)
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/bigquery"+q, `{"datasetId":"`+ds+`"}`); code != http.StatusOK {
		t.Fatalf("create a dataset through the console API = %d: %s", code, body)
	}
	t.Cleanup(func() { consoleDo(t, http.MethodDelete, "/api/resources/bigquery"+q+"&name="+ds, "") })
	for _, stmt := range []string{
		"CREATE TABLE " + ds + ".train AS SELECT CAST(x AS FLOAT64) AS x, CAST(3 + 2 * x AS FLOAT64) AS y FROM UNNEST(GENERATE_ARRAY(1, 20)) AS x",
		"CREATE MODEL " + ds + ".m OPTIONS(model_type='linear_reg', input_label_cols=['y']) AS SELECT x, y FROM " + ds + ".train",
	} {
		b, _ := json.Marshal(map[string]any{"Path": []string{ds}, "Statement": stmt, "Mode": "read-write"})
		if code, body := consoleDo(t, http.MethodPost, "/api/query/bigquery"+q, string(b)); code != http.StatusOK {
			t.Fatalf("%s through the console API = %d: %s", stmt, code, body)
		}
	}
	models := func() []string {
		t.Helper()
		v := url.Values{"project": {project}, "name": {ds}}
		code, body := consoleDo(t, http.MethodGet, "/api/detail/bigquery?"+v.Encode(), "")
		var d struct {
			Sections []struct {
				ID      string
				Listing struct {
					Unavailable string
					Items       []struct{ Name string }
				}
			}
		}
		if code != http.StatusOK || json.Unmarshal([]byte(body), &d) != nil {
			t.Fatalf("the dataset's page = %d: %.300s", code, body)
		}
		var names []string
		for _, s := range d.Sections {
			if s.ID == "models" {
				if s.Listing.Unavailable != "" {
					t.Errorf("the Models section: %s", s.Listing.Unavailable)
				}
				for _, it := range s.Listing.Items {
					names = append(names, it.Name)
				}
			}
		}
		return names
	}

	p.navigate("/bigquery/" + ds + q)
	p.waitFor(`document.querySelector("#tab-models") !== null`)
	p.run(chromedp.Click(`#tab-models`, chromedp.ByQuery))
	p.waitFor(`document.querySelector("#view").textContent.includes("LINEAR_REGRESSION")`)
	p.waitFor(`document.querySelector('button[aria-label="Actions for m"]') !== null`)
	if got := models(); strings.Join(got, ",") != "m" {
		t.Errorf("the dataset's Models section lists %v, want [m]", got)
	}
	var text string

	p.navigate("/bigquery/" + ds + "/models/m" + q)
	p.waitFor(`document.querySelector("#view").textContent.includes("Features")`)
	p.eval(`document.querySelector("#view").textContent`, &text)
	if !strings.Contains(text, "LINEAR_REGRESSION") || !strings.Contains(text, "FLOAT64") {
		t.Errorf("the model's page does not show its type and features: %.800s", text)
	}

	p.clickText("#view .page-actions button", "Edit model")
	p.waitFor(`document.querySelector(".modal #f-description") !== null`)
	p.setField(".modal #f-description", "trained in the browser")
	p.setField(".modal #f-name", "Browser model")
	p.run(chromedp.Click(`.modal button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(`document.querySelector(".modal") === null`)
	p.navigate("/bigquery/" + ds + "/models/m" + q + "&tab=details")
	p.waitFor(`document.querySelector("#view").textContent.includes("trained in the browser")`)
	p.eval(`document.querySelector("#view").textContent`, &text)
	if !strings.Contains(text, "Browser model") {
		t.Errorf("after Edit model the page does not show the friendly name: %.800s", text)
	}

	p.clickText("#view .page-actions button", "Delete model")
	p.waitFor(`document.activeElement === document.querySelector(".modal #confirm-input")`)
	p.run(chromedp.SendKeys(`.modal #confirm-input`, "m", chromedp.ByQuery), chromedp.KeyEvent(kb.Enter))
	p.waitFor(`document.querySelector(".modal") === null && location.pathname === "/bigquery/` + ds + `"`)
	if got := models(); len(got) != 0 {
		t.Errorf("after Delete model the dataset lists models %v", got)
	}
}
