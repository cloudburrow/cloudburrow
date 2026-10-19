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

// TestBigQueryRoutinesThroughThePage (#782, #1177), in the emulators shard.
// A function and a table function made in the Read-write editor are rows
// of the dataset's Routines section, with their type and arguments; a
// routine's page shows its body; Delete routine on the page, confirmed by
// the routine's name, drops it and returns to the dataset, and Delete
// routine in the other's row menu drops that one.
func TestBigQueryRoutinesThroughThePage(t *testing.T) {
	needService(t, "bigquery")
	p := open(t)
	project := servedProject(t)
	ds := fmt.Sprintf("browser_routines_%d", time.Now().UnixNano()%1e12)
	q := "?project=" + url.QueryEscape(project)
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/bigquery"+q, `{"datasetId":"`+ds+`"}`); code != http.StatusOK {
		t.Fatalf("create a dataset through the console API = %d: %s", code, body)
	}
	t.Cleanup(func() { consoleDo(t, http.MethodDelete, "/api/resources/bigquery"+q+"&name="+ds, "") })
	for _, stmt := range []string{
		"CREATE FUNCTION " + ds + ".twice(x INT64) RETURNS INT64 AS (x * 2)",
		"CREATE TABLE FUNCTION " + ds + ".tf(n INT64) AS SELECT n AS y",
	} {
		b, _ := json.Marshal(map[string]any{"Path": []string{ds}, "Statement": stmt, "Mode": "read-write"})
		if code, body := consoleDo(t, http.MethodPost, "/api/query/bigquery"+q, string(b)); code != http.StatusOK {
			t.Fatalf("%s through the console API = %d: %s", stmt, code, body)
		}
	}
	routines := func() []string {
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
			if s.ID == "routines" {
				if s.Listing.Unavailable != "" {
					t.Errorf("the Routines section: %s", s.Listing.Unavailable)
				}
				for _, it := range s.Listing.Items {
					names = append(names, it.Name)
				}
			}
		}
		return names
	}

	p.navigate("/bigquery/" + ds + q)
	p.waitFor(`document.querySelector("#tab-routines") !== null`)
	p.run(chromedp.Click(chromedp.CSS(`#tab-routines`)))
	p.waitFor(`document.querySelector("#view").textContent.includes("Table function")`)
	var text string
	p.eval(`document.querySelector("#view").textContent`, &text)
	if !strings.Contains(text, "Routines") || !strings.Contains(text, "twice") || !strings.Contains(text, "x INT64") {
		t.Errorf("the dataset's page does not list the routines: %.600s", text)
	}

	p.navigate("/bigquery/" + ds + "/routines/twice" + q)
	p.waitFor(`document.querySelector("#view").textContent.includes("x * 2")`)
	p.eval(`document.querySelector("#view").textContent`, &text)
	if !strings.Contains(text, "Function") {
		t.Errorf("the routine's page does not show its type: %.600s", text)
	}
	p.clickText("#view .page-actions button", "Delete routine")
	p.waitFor(`document.activeElement === document.querySelector(".modal #confirm-input")`)
	p.run(chromedp.SendKeys(chromedp.CSS(`.modal #confirm-input`), "twice"), chromedp.KeyEvent(kb.Enter))
	p.waitFor(`document.querySelector(".modal") === null && location.pathname === "/bigquery/` + ds + `"`)
	if got := routines(); strings.Join(got, ",") != "tf" {
		t.Errorf("after Delete routine on its page the dataset lists %v, want [tf]", got)
	}

	p.navigate("/bigquery/" + ds + q + "&tab=routines")
	p.waitFor(`document.querySelector('button[aria-label="Actions for tf"]') !== null`)
	p.run(chromedp.Click(chromedp.CSS(`button[aria-label="Actions for tf"]`)))
	p.clickText(`.overflow-menu:not([hidden]) [role="menuitem"]`, "Delete routine")
	p.waitFor(`document.querySelector(".modal.is-open #confirm-input") !== null`)
	p.run(chromedp.SendKeys(chromedp.CSS(`.modal #confirm-input`), "tf"), chromedp.KeyEvent(kb.Enter))
	p.waitFor(`document.querySelector(".modal") === null`)
	if got := routines(); len(got) != 0 {
		t.Errorf("after Delete routine in its row the dataset lists %v", got)
	}
}
