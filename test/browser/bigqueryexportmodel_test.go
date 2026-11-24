//go:build browser

package browser

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// TestBigQueryExportModelThroughThePage (#2210), in the emulators shard: a
// model's page has Google's Export model, which asks for a Cloud Storage
// location and exports the model there in its format (an extract job of
// the model, bq extract -m): a LINEAR_REG model's TF SavedModel. The
// keyboard subtest opens Export model and submits it from the keyboard;
// the WCAG subtest audits the model's page and the open form.
func TestBigQueryExportModelThroughThePage(t *testing.T) {
	needService(t, "bigquery")
	needService(t, "storage")
	p := open(t)
	project := servedProject(t)
	stamp := time.Now().UnixNano() % 1e12
	ds := fmt.Sprintf("browser_export_%d", stamp)
	q := "?project=" + url.QueryEscape(project)
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/bigquery"+q, `{"datasetId":"`+ds+`"}`); code != http.StatusOK {
		t.Fatalf("create a dataset through the console API = %d: %s", code, body)
	}
	t.Cleanup(func() { consoleDo(t, http.MethodDelete, "/api/resources/bigquery"+q+"&name="+ds, "") })
	bucket := fmt.Sprintf("export-%d", stamp)
	fixture(t, "storage", project, `{"name":"`+bucket+`"}`, bucket)
	t.Cleanup(func() {
		for _, dir := range []string{"m", "k"} {
			for _, o := range []string{"saved_model.pb", "variables/variables.index", "variables/variables.data-00000-of-00001", "fingerprint.pb"} {
				v := url.Values{"project": {project}, "name": {bucket, dir + "/" + o}}
				consoleDo(t, http.MethodDelete, "/api/objects/storage?"+v.Encode(), "")
			}
		}
	})
	for _, stmt := range []string{
		"CREATE TABLE " + ds + ".train AS SELECT CAST(x AS FLOAT64) AS x, CAST(3 + 2 * x AS FLOAT64) AS y FROM UNNEST(GENERATE_ARRAY(1, 20)) AS x",
		"CREATE MODEL " + ds + ".m OPTIONS(model_type='linear_reg', input_label_cols=['y']) AS SELECT x, y FROM " + ds + ".train",
	} {
		b, _ := json.Marshal(map[string]any{"Path": []string{ds}, "Statement": stmt, "Mode": "read-write"})
		if code, body := consoleDo(t, http.MethodPost, "/api/query/bigquery"+q, string(b)); code != http.StatusOK {
			t.Fatalf("%s through the console API = %d: %s", stmt, code, body)
		}
	}
	// exported reports whether gs://bucket/dir/saved_model.pb is there.
	exported := func(dir string) bool {
		v := url.Values{"project": {project}, "name": {"_details", bucket, dir + "/saved_model.pb"}}
		code, body := consoleDo(t, http.MethodGet, "/api/detail/storage?"+v.Encode(), "")
		var d struct{ Unavailable string }
		return code == http.StatusOK && json.Unmarshal([]byte(body), &d) == nil && d.Unavailable == ""
	}
	modelPage := "/bigquery/" + ds + "/models/m" + q
	formOpen := `document.querySelector(".modal.is-open #f-uri") !== null`

	p.navigate(modelPage)
	p.waitFor(`document.querySelector("#view").textContent.includes("Features")`)
	p.clickText("#view .page-actions button", "Export model")
	p.waitFor(formOpen)
	p.setField(".modal #f-uri", "gs://"+bucket+"/m")
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitForWithin(`document.querySelector(".modal") === null`, 10*time.Minute)
	if !exported("m") {
		t.Errorf("Export model wrote no gs://%s/m/saved_model.pb", bucket)
	}

	t.Run("keyboard", func(t *testing.T) {
		parent := p.t
		p.t = t
		defer func() { p.t = parent }()
		p.navigate(modelPage)
		p.waitFor(`document.querySelector("#view").textContent.includes("Features")`)
		// Tab reaches Export model, Enter opens its form, which takes the
		// location typed and Enter submits it.
		p.tabTo(`a.closest(".page-actions") && a.textContent.trim() === "Export model"`)
		p.run(chromedp.KeyEvent(kb.Enter))
		p.waitFor(formOpen)
		p.run(chromedp.SendKeys(chromedp.CSS(`.modal.is-open #f-uri`), "gs://"+bucket+"/k"), chromedp.KeyEvent(kb.Enter))
		p.waitForWithin(`document.querySelector(".modal") === null`, 10*time.Minute)
		if !exported("k") {
			t.Errorf("Export model from the keyboard wrote no gs://%s/k/saved_model.pb", bucket)
		}
	})

	t.Run("WCAG", func(t *testing.T) {
		ready := `document.querySelector("#view").textContent.includes("Features")`
		auditScreens(t, []a11yPage{
			{"model", modelPage, ready, ""},
			{"export model dialog", modelPage, ready,
				`[...document.querySelectorAll("#view .page-actions button")].find((b) => b.textContent.trim() === "Export model").click()`},
		})
	})
}
