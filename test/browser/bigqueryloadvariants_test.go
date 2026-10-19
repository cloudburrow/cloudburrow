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

// TestBigQueryHiveAndGeoJSONLoadsThroughTheForm (#1591), in the emulators
// shard, whose instance serves BigQuery and Cloud Storage. On a dataset's
// page, Load from Cloud Storage of newline-delimited JSON under
// gs://B/t/dt=…/ with the schema auto-detected and Source data
// partitioning AUTO with the source URI prefix gs://B/t loads every file's
// rows with the dt key as a column; with JSON extension GEOJSON, a file of
// GeoJSON Features loads each geometry as a GEOGRAPHY and each property
// as a column, as bq load --hive_partitioning_mode and --json_extension do.
func TestBigQueryHiveAndGeoJSONLoadsThroughTheForm(t *testing.T) {
	needService(t, "bigquery")
	needService(t, "storage")
	p := open(t)
	project := servedProject(t)
	stamp := time.Now().UnixNano() % 1e12
	ds := fmt.Sprintf("browser_loads_%d", stamp)
	bucket := fmt.Sprintf("browser-bql-%d", stamp)
	q := "?project=" + url.QueryEscape(project)
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/bigquery"+q, `{"datasetId":"`+ds+`"}`); code != http.StatusOK {
		t.Fatalf("create a dataset through the console API = %d: %s", code, body)
	}
	t.Cleanup(func() { consoleDo(t, http.MethodDelete, "/api/resources/bigquery"+q+"&name="+ds, "") })
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/storage"+q, `{"name":"`+bucket+`"}`); code != http.StatusOK {
		t.Fatalf("create a bucket through the console API = %d: %s", code, body)
	}
	objects := map[string]string{
		"t/dt=2024-01-01/a.json": `{"name":"ann"}` + "\n",
		"t/dt=2024-01-02/b.json": `{"name":"bob"}` + "\n" + `{"name":"cy"}` + "\n",
		"geo/features.json": `{"type":"Feature","geometry":{"type":"Point","coordinates":[1,2]},"properties":{"label":"here"}}` + "\n" +
			`{"type":"Feature","geometry":null,"properties":{"label":"nowhere"}}` + "\n",
	}
	t.Cleanup(func() {
		for o := range objects {
			consoleDo(t, http.MethodDelete, "/api/objects/storage?"+url.Values{"project": {project}, "name": append([]string{bucket}, strings.Split(o, "/")...)}.Encode(), "")
		}
		consoleDo(t, http.MethodDelete, "/api/resources/storage"+q+"&name="+bucket, "")
	})
	for o, data := range objects {
		segs := strings.Split(o, "/")
		uploadObject(t, project, append([]string{bucket}, segs[:len(segs)-1]...), segs[len(segs)-1], data)
	}

	load := func(table, uris string, values map[string]string) []string {
		t.Helper()
		p.navigate("/bigquery/" + ds + q)
		p.clickText("#view .page-actions button", "Load from Cloud Storage")
		p.waitFor(`document.querySelector(".modal #f-uris") !== null`)
		p.run(chromedp.SendKeys(chromedp.CSS(`.modal #f-tableId`), table))
		p.setField(".modal #f-uris", uris)
		p.setField(".modal #f-format", "NEWLINE_DELIMITED_JSON")
		p.eval(`(() => { const f = document.querySelector(".modal #f-autodetect"); f.checked = true;
			f.dispatchEvent(new Event("change", { bubbles: true })); return true; })()`, nil)
		for f, v := range values {
			p.setField(".modal #f-"+f, v)
		}
		p.run(chromedp.Click(chromedp.CSS(`.modal button[type="submit"]`)))
		p.waitFor(`document.querySelector("#action-result-table a") !== null ||
			[...document.querySelectorAll(".modal .form-error")].some((e) => !e.hidden && e.textContent)`)
		var refusal string
		p.eval(`[...document.querySelectorAll(".modal .form-error")].filter((e) => !e.hidden).map((e) => e.textContent).join(" ")`, &refusal)
		if refusal != "" {
			t.Fatalf("the %s load was refused: %s", table, refusal)
		}
		var cells []string
		p.eval(`[...document.querySelectorAll("#action-result-table td")].map((c) => c.textContent)`, &cells)
		p.clickText(".modal button", "Close")
		p.waitFor(`document.querySelector(".modal") === null`)
		return cells
	}

	cells := load("hive", "gs://"+bucket+"/t/*", map[string]string{"hiveMode": "AUTO", "hiveSourceUriPrefix": "gs://" + bucket + "/t"})
	if len(cells) < 3 || cells[1] != "Succeeded" || cells[2] != "3" {
		t.Errorf("the hive-partitioned load's result row is %v, want Succeeded and 3 output rows", cells)
	}
	// A preview row is named by its first column's value: name, which the
	// files hold, comes before dt, which the paths do.
	byName := map[string]string{}
	for name, r := range readDataPage(t, "bigquery", project, ds, "hive").rows("preview") {
		byName[name] = r["dt"]
	}
	if len(byName) != 3 || byName["ann"] != "2024-01-01" || byName["bob"] != "2024-01-02" || byName["cy"] != "2024-01-02" {
		t.Errorf("the hive-partitioned table previews names to dt %v, want ann 2024-01-01 and bob, cy 2024-01-02", byName)
	}

	cells = load("geo", "gs://"+bucket+"/geo/features.json", map[string]string{"jsonExtension": "GEOJSON"})
	if len(cells) < 3 || cells[1] != "Succeeded" || cells[2] != "2" {
		t.Errorf("the GeoJSON load's result row is %v, want Succeeded and 2 output rows", cells)
	}
	// Named by geometry, the first column; label is a property's column.
	geo := map[string]string{}
	for geometry, r := range readDataPage(t, "bigquery", project, ds, "geo").rows("preview") {
		geo[r["label"]] = geometry
	}
	if len(geo) != 2 || !strings.HasPrefix(geo["here"], "POINT(1 2)") {
		t.Errorf("the GeoJSON table previews label to geometry %v, want here at POINT(1 2)", geo)
	}
}
