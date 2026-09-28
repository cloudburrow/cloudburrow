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

// The data browsers' writes (#854), in the emulators shard, whose instance
// serves BigQuery, Firestore and Datastore.

// dataPage reads a page through the console API: its sections' rows by name
// and its summary by label, or its unavailable message.
type dataPage struct {
	Unavailable string
	Summary     []struct{ Label, Value string }
	Sections    []struct {
		ID      string
		Listing struct {
			Items []struct {
				Name   string
				Fields map[string]string
			}
		}
	}
}

func readDataPage(t *testing.T, service, project string, path ...string) dataPage {
	t.Helper()
	v := url.Values{"project": {project}, "name": path}
	code, body := consoleDo(t, http.MethodGet, "/api/detail/"+service+"?"+v.Encode(), "")
	var d dataPage
	if err := json.Unmarshal([]byte(body), &d); code != http.StatusOK || err != nil {
		t.Fatalf("read %v through the console API = %d (%v): %s", path, code, err, body)
	}
	return d
}

func (d dataPage) rows(section string) map[string]map[string]string {
	for _, s := range d.Sections {
		if s.ID == section {
			out := map[string]map[string]string{}
			for _, it := range s.Listing.Items {
				out[it.Name] = it.Fields
			}
			return out
		}
	}
	return nil
}

func (d dataPage) summary(label string) string {
	for _, p := range d.Summary {
		if p.Label == label {
			return p.Value
		}
	}
	return ""
}

// setField sets a dialog control's value as typing would, so the form's
// listeners see it.
func (p *tab) setField(sel, value string) {
	p.t.Helper()
	p.eval(fmt.Sprintf(`(() => { const f = document.querySelector(%q);
		f.value = %q; f.dispatchEvent(new Event("input", { bubbles: true }));
		f.dispatchEvent(new Event("change", { bubbles: true })); return true; })()`, sel, value), nil)
}

// servedProject is the one project the BigQuery emulator serves: the
// instance's default.
func servedProject(t *testing.T) string {
	t.Helper()
	code, body := consoleDo(t, http.MethodGet, "/api/status", "")
	var s struct{ DefaultProject string }
	if err := json.Unmarshal([]byte(body), &s); code != http.StatusOK || err != nil || s.DefaultProject == "" {
		t.Fatalf("read the console's status = %d (%v): %s", code, err, body)
	}
	return s.DefaultProject
}

// TestBigQueryCreateTableInsertRowsAndDeleteThroughTheForms (#854). On a
// dataset's page, Create table opens with one schema row; a second is added
// with Add field and a third named like the first but for case is refused on
// the form, naming the clash, with nothing sent. Removed, the table is created
// with the two fields, names, types and modes as chosen, which the console
// API's page for the table reads. Insert rows refuses a row missing its
// REQUIRED value with the row's number and the API's reason (the validating
// front's, #874) and writes nothing; two good rows are
// then inserted and previewed. Delete table asks for the table's name back and
// returns to the dataset, which no longer lists it.
func TestBigQueryCreateTableInsertRowsAndDeleteThroughTheForms(t *testing.T) {
	needService(t, "bigquery")
	p := open(t)
	project := servedProject(t)
	ds := fmt.Sprintf("browser_%d", time.Now().UnixNano()%1e12)
	q := "?project=" + url.QueryEscape(project)
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/bigquery"+q, `{"datasetId":"`+ds+`"}`); code != http.StatusOK {
		t.Fatalf("create a dataset through the console API = %d: %s", code, body)
	}
	t.Cleanup(func() { consoleDo(t, http.MethodDelete, "/api/resources/bigquery"+q+"&name="+ds, "") })

	p.navigate("/bigquery/" + ds + q)
	p.clickText("#view .page-actions button", "Create table")
	p.waitFor(`document.querySelector(".modal #f-tableId") !== null && document.querySelectorAll(".modal .schema-row").length === 1`)
	p.run(chromedp.SendKeys(`.modal #f-tableId`, "events", chromedp.ByQuery))
	row := func(n int, part string) string {
		return fmt.Sprintf(`.modal .schema-row:nth-child(%d) .schema-%s`, n, part)
	}
	p.run(chromedp.SendKeys(row(1, "name"), "id", chromedp.ByQuery))
	p.setField(row(1, "type"), "INTEGER")
	p.setField(row(1, "mode"), "REQUIRED")
	p.clickText(".modal button", "Add field")
	p.waitFor(`document.querySelectorAll(".modal .schema-row").length === 2 && document.activeElement === document.querySelector(` +
		fmt.Sprintf("%q", row(2, "name")) + `)`)
	p.run(chromedp.SendKeys(row(2, "name"), "tag", chromedp.ByQuery))
	p.setField(row(2, "mode"), "REPEATED")
	p.clickText(".modal button", "Add field")
	p.run(chromedp.SendKeys(row(3, "name"), "ID", chromedp.ByQuery))

	p.run(chromedp.Click(`.modal button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(`!document.querySelector(".modal .schema-field .form-field-error").hidden`)
	var fieldError string
	p.eval(`document.querySelector(".modal .schema-field .form-field-error").textContent`, &fieldError)
	if !strings.Contains(fieldError, `"ID" is named twice`) {
		t.Errorf("a field named like another but for case was refused with %q", fieldError)
	}
	if sent := p.sent(http.MethodPost, "/api/actions/bigquery"); len(sent) != 0 {
		t.Fatalf("the refused schema was sent: %v", sent)
	}
	p.run(chromedp.Click(`.modal button[aria-label="Remove field 3"]`, chromedp.ByQuery))
	p.waitFor(`document.querySelectorAll(".modal .schema-row").length === 2 && document.querySelector(".modal .schema-field .form-field-error").hidden`)
	p.run(chromedp.Click(`.modal button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(`document.querySelector(".modal") === null`)

	schema := readDataPage(t, "bigquery", project, ds, "events").rows("schema")
	if f := schema["id"]; f["Type"] != "INTEGER" || f["Mode"] != "REQUIRED" {
		t.Errorf("id is %v, want INTEGER REQUIRED", f)
	}
	if f := schema["tag"]; f["Type"] != "STRING" || f["Mode"] != "REPEATED" {
		t.Errorf("tag is %v, want STRING REPEATED", f)
	}
	if len(schema) != 2 {
		t.Errorf("the table has fields %v, want id and tag", schema)
	}

	p.navigate("/bigquery/" + ds + "/events" + q)
	p.clickText("#view .page-actions button", "Insert rows")
	p.waitFor(`document.querySelector(".modal #f-rows") !== null`)
	p.setField(".modal #f-rows", `{"id": 1, "tag": ["a"]}`+"\n"+`{"tag": ["b"]}`)
	p.run(chromedp.Click(`.modal button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(`!document.querySelector(".modal .form-error").hidden`)
	var refusal string
	p.eval(`document.querySelector(".modal .form-error").textContent`, &refusal)
	if refusal != "row 2: Missing required field: id." {
		t.Errorf("a row missing its REQUIRED id was refused with %q", refusal)
	}
	p.forgive()
	if preview := readDataPage(t, "bigquery", project, ds, "events").rows("preview"); len(preview) != 0 {
		t.Errorf("the refused insert wrote %v", preview)
	}
	p.setField(".modal #f-rows", `{"id": 1, "tag": ["a"]}`+"\n"+`{"id": 2, "tag": ["b", "c"]}`)
	p.run(chromedp.Click(`.modal button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(`document.querySelector(".modal") === null`)
	if preview := readDataPage(t, "bigquery", project, ds, "events").rows("preview"); len(preview) != 2 {
		t.Errorf("the table previews %v after two rows were inserted", preview)
	}

	p.clickText("#view .page-actions button", "Delete table")
	p.waitFor(`document.activeElement === document.querySelector(".modal #confirm-input")`)
	p.run(chromedp.SendKeys(`.modal #confirm-input`, "events", chromedp.ByQuery), chromedp.KeyEvent(kb.Enter))
	p.waitFor(`document.querySelector(".modal") === null && location.pathname === "/bigquery/` + ds + `"`)
	if tables := readDataPage(t, "bigquery", project, ds).rows("tables"); len(tables) != 0 {
		t.Errorf("the dataset still lists %v after Delete table", tables)
	}
}

// TestBigQueryRecordColumnsThroughTheSchemaEditor (#874). Create table's
// editor offers RECORD. Chosen, the row opens a list of nested fields under
// it; submitted with no nested field named, the form refuses the RECORD with
// nothing sent. A RECORD addr is then built with a REQUIRED city and a RECORD
// geo holding lat, two levels down, and the table's schema tab reads back
// every nested field by its dotted path. Insert rows takes a row whose addr
// is an object, and one missing a nested REQUIRED value is refused naming it,
// addr.city, with nothing written; the good row is then previewed with its
// nested values.
func TestBigQueryRecordColumnsThroughTheSchemaEditor(t *testing.T) {
	needService(t, "bigquery")
	p := open(t)
	project := servedProject(t)
	ds := fmt.Sprintf("browser_rec_%d", time.Now().UnixNano()%1e12)
	q := "?project=" + url.QueryEscape(project)
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/bigquery"+q, `{"datasetId":"`+ds+`"}`); code != http.StatusOK {
		t.Fatalf("create a dataset through the console API = %d: %s", code, body)
	}
	t.Cleanup(func() { consoleDo(t, http.MethodDelete, "/api/resources/bigquery"+q+"&name="+ds, "") })

	p.navigate("/bigquery/" + ds + q)
	p.clickText("#view .page-actions button", "Create table")
	p.waitFor(`document.querySelector(".modal #f-tableId") !== null && document.querySelectorAll(".modal .schema-row").length === 1`)
	p.run(chromedp.SendKeys(`.modal #f-tableId`, "people", chromedp.ByQuery))
	field := func(label string) string { return fmt.Sprintf(`.modal [aria-label=%q]`, label) }
	p.run(chromedp.SendKeys(field("Field 1 name"), "id", chromedp.ByQuery))
	p.setField(field("Field 1 type"), "INTEGER")
	p.clickText(".modal button", "Add field")
	p.run(chromedp.SendKeys(field("Field 2 name"), "addr", chromedp.ByQuery))
	p.setField(field("Field 2 type"), "RECORD")
	p.waitFor(`document.querySelector(` + fmt.Sprintf("%q", field("Field 2.1 name")) + `) !== null`)

	// A RECORD with no nested field named is refused on the form.
	p.run(chromedp.Click(`.modal button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(`!document.querySelector(".modal .schema-field .form-field-error").hidden`)
	var fieldError string
	p.eval(`document.querySelector(".modal .schema-field .form-field-error").textContent`, &fieldError)
	if !strings.Contains(fieldError, `"addr" is a RECORD with no fields`) {
		t.Errorf("a RECORD with no fields was refused with %q", fieldError)
	}
	if sent := p.sent(http.MethodPost, "/api/actions/bigquery"); len(sent) != 0 {
		t.Fatalf("the refused schema was sent: %v", sent)
	}

	p.run(chromedp.SendKeys(field("Field 2.1 name"), "city", chromedp.ByQuery))
	p.setField(field("Field 2.1 mode"), "REQUIRED")
	p.run(chromedp.Click(field("Add a nested field to field 2"), chromedp.ByQuery))
	p.waitFor(`document.activeElement === document.querySelector(` + fmt.Sprintf("%q", field("Field 2.2 name")) + `)`)
	p.run(chromedp.SendKeys(field("Field 2.2 name"), "geo", chromedp.ByQuery))
	p.setField(field("Field 2.2 type"), "RECORD")
	p.waitFor(`document.querySelector(` + fmt.Sprintf("%q", field("Field 2.2.1 name")) + `) !== null`)
	p.run(chromedp.SendKeys(field("Field 2.2.1 name"), "lat", chromedp.ByQuery))
	p.setField(field("Field 2.2.1 type"), "FLOAT")
	p.waitFor(`document.querySelector(".modal .schema-field .form-field-error").hidden`)
	p.run(chromedp.Click(`.modal button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(`document.querySelector(".modal") === null`)

	schema := readDataPage(t, "bigquery", project, ds, "people").rows("schema")
	for name, want := range map[string]string{
		"id": "INTEGER NULLABLE", "addr": "RECORD NULLABLE", "addr.city": "STRING REQUIRED",
		"addr.geo": "RECORD NULLABLE", "addr.geo.lat": "FLOAT NULLABLE",
	} {
		if f := schema[name]; f["Type"]+" "+f["Mode"] != want {
			t.Errorf("%s is %v, want %s", name, f, want)
		}
	}
	if len(schema) != 5 {
		t.Errorf("the table has fields %v, want id, addr and addr's three", schema)
	}

	p.navigate("/bigquery/" + ds + "/people" + q)
	p.clickText("#view .page-actions button", "Insert rows")
	p.waitFor(`document.querySelector(".modal #f-rows") !== null`)
	p.setField(".modal #f-rows", `{"id": 1, "addr": {"city": "Paris", "geo": {"lat": 48.85}}}`+"\n"+`{"id": 2, "addr": {"geo": {"lat": 1}}}`)
	p.run(chromedp.Click(`.modal button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(`!document.querySelector(".modal .form-error").hidden`)
	var refusal string
	p.eval(`document.querySelector(".modal .form-error").textContent`, &refusal)
	if refusal != "row 2: Missing required field: addr.city." {
		t.Errorf("a row missing its nested REQUIRED city was refused with %q", refusal)
	}
	p.forgive()
	if preview := readDataPage(t, "bigquery", project, ds, "people").rows("preview"); len(preview) != 0 {
		t.Errorf("the refused insert wrote %v", preview)
	}
	p.setField(".modal #f-rows", `{"id": 1, "addr": {"city": "Paris", "geo": {"lat": 48.85}}}`)
	p.run(chromedp.Click(`.modal button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(`document.querySelector(".modal") === null`)
	preview := readDataPage(t, "bigquery", project, ds, "people").rows("preview")
	if got := preview["1"]["addr"]; got != `{"city":"Paris","geo":{"lat":"48.85"}}` {
		t.Errorf("the row previews addr as %q (rows %v)", got, preview)
	}
}

// TestFirestoreStartASubcollectionOnADocumentPage (#854). A document's page
// offers Start collection; its first document made, the Collections tab lists
// the subcollection, and its row opens the subcollection's page, addressed by
// its path, whose breadcrumb leads back through the document and collection
// above it.
func TestFirestoreStartASubcollectionOnADocumentPage(t *testing.T) {
	needService(t, "firestore")
	p := open(t)
	project := uniqueProject(t)
	q := "?project=" + project
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/firestore"+q,
		`{"collection":"users","documentId":"alice","field":"name","type":"string","value":"Alice"}`); code != http.StatusOK {
		t.Fatalf("start a collection through the console API = %d: %s", code, body)
	}

	p.navigate("/firestore/users/alice" + q)
	p.clickText("#view .page-actions button", "Start collection")
	p.waitFor(`document.querySelector(".modal #f-collection") !== null`)
	p.run(chromedp.SendKeys(`.modal #f-collection`, "orders", chromedp.ByQuery))
	p.run(chromedp.SendKeys(`.modal #f-documentId`, "o1", chromedp.ByQuery))
	p.run(chromedp.Click(`.modal button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(`document.querySelector(".modal") === null`)

	p.run(chromedp.Click(`#tab-collections`, chromedp.ByQuery))
	p.clickText("#view tbody a", "orders")
	p.waitFor(`location.pathname === "/firestore/users%2Falice%2Forders" && document.querySelector("#view h1").textContent === "users/alice/orders"`)
	// The provider's trail replaces the one drawn from the path once the
	// page has loaded.
	p.waitFor(`document.querySelectorAll("#view .breadcrumb a").length === 3`)
	var crumbs []string
	p.eval(`[...document.querySelectorAll("#view .breadcrumb a")].map((a) => a.textContent + "=" + new URL(a.href).pathname)`, &crumbs)
	if want := "Firestore=/firestore,users=/firestore/users,alice=/firestore/users/alice"; strings.Join(crumbs, ",") != want {
		t.Errorf("the subcollection's breadcrumb links %v, want %s", crumbs, want)
	}
	p.waitFor(`[...document.querySelectorAll("#view tbody a")].some((a) => a.textContent === "o1")`)
	if d := readDataPage(t, "firestore", project, "users/alice/orders", "o1"); d.Unavailable != "" {
		t.Errorf("the subcollection's first document cannot be read: %s", d.Unavailable)
	}
}

// TestDatastoreNamespaceAndChildEntityThroughTheBrowser (#854). An entity in
// a namespace other than the default is listed on the Datastore screen with
// its namespace, and its row opens the kind in that namespace, whose
// breadcrumb names it. On the entity's page, Create child entity makes a
// child the Children tab lists by its key path; its row opens the child, and
// Delete entity there asks for that key path back and returns to the child's
// kind in the namespace.
func TestDatastoreNamespaceAndChildEntityThroughTheBrowser(t *testing.T) {
	needService(t, "datastore")
	p := open(t)
	project := uniqueProject(t)
	q := "?project=" + project
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/datastore"+q,
		`{"namespace":"tenant-b","kind":"Widget","key":"w1"}`); code != http.StatusOK {
		t.Fatalf("create an entity through the console API = %d: %s", code, body)
	}

	p.navigate("/datastore" + q)
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("Widget") && r.textContent.includes("tenant-b"))`)
	p.clickText("#view tbody a", "Widget")
	p.waitFor(`location.pathname === "/datastore/__namespace__/tenant-b/Widget" && ` +
		`document.querySelector("#view .breadcrumb").textContent.includes("Namespaces")`)
	var trail string
	p.eval(`document.querySelector("#view .breadcrumb").textContent`, &trail)
	if trail != "Datastore/Namespaces/tenant-b/Widget" {
		t.Errorf("the namespaced kind's breadcrumb reads %q", trail)
	}
	p.clickText("#view tbody a", "w1")
	p.clickText("#view .page-actions button", "Create child entity")
	p.waitFor(`document.querySelector(".modal #f-kind") !== null`)
	p.run(chromedp.SendKeys(`.modal #f-kind`, "Part", chromedp.ByQuery))
	p.run(chromedp.SendKeys(`.modal #f-key`, "p1", chromedp.ByQuery))
	p.run(chromedp.Click(`.modal button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(`document.querySelector(".modal") === null`)

	const child = "Widget/w1/Part/p1"
	p.run(chromedp.Click(`#tab-children`, chromedp.ByQuery))
	p.clickText("#view tbody a", child)
	p.waitFor(`document.querySelector("#view h1").textContent === "` + child + `"`)
	if d := readDataPage(t, "datastore", project, "__namespace__", "tenant-b", "Part", child); d.summary("Parent") != "Widget/w1" {
		t.Errorf("the child's page names parent %q (%s)", d.summary("Parent"), d.Unavailable)
	}
	p.clickText("#view .page-actions button", "Delete entity")
	p.waitFor(`document.activeElement === document.querySelector(".modal #confirm-input")`)
	p.run(chromedp.SendKeys(`.modal #confirm-input`, child, chromedp.ByQuery), chromedp.KeyEvent(kb.Enter))
	p.waitFor(`document.querySelector(".modal") === null && location.pathname === "/datastore/__namespace__/tenant-b/Part"`)
	if d := readDataPage(t, "datastore", project, "__namespace__", "tenant-b", "Part", child); !strings.Contains(d.Unavailable, "no such entity") {
		t.Errorf("after Delete entity the child's page reads %+v", d)
	}
}
