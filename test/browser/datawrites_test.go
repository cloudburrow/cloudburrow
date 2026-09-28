//go:build browser

package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/datastore"
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
// REQUIRED value with the row's number and writes nothing; two good rows are
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
	if refusal != "row 2: id is REQUIRED and has no value" {
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
// child the Children tab lists by its Name/ID; its row opens the child, and
// Delete entity there asks for its key path back and returns to the child's
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
	p.clickText("#view tbody a", "name=w1")
	p.clickText("#view .page-actions button", "Create child entity")
	p.waitFor(`document.querySelector(".modal #f-kind") !== null`)
	p.run(chromedp.SendKeys(`.modal #f-kind`, "Part", chromedp.ByQuery))
	p.run(chromedp.SendKeys(`.modal #f-key`, "p1", chromedp.ByQuery))
	p.run(chromedp.Click(`.modal button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(`document.querySelector(".modal") === null`)

	// Listed by its Name/ID on the Children tab, headed by its key path in
	// the same rendering (#885), and still opened by the older key-path link.
	const child, heading = "Widget/w1/Part/p1", "Widget/name=w1/Part/name=p1"
	p.run(chromedp.Click(`#tab-children`, chromedp.ByQuery))
	p.clickText("#view tbody a", "name=p1")
	p.waitFor(`document.querySelector("#view h1").textContent === "` + heading + `"`)
	if d := readDataPage(t, "datastore", project, "__namespace__", "tenant-b", "Part", child); d.summary("Parent") != "Widget/name=w1" {
		t.Errorf("the child's page names parent %q (%s)", d.summary("Parent"), d.Unavailable)
	}
	p.clickText("#view .page-actions button", "Delete entity")
	p.waitFor(`document.activeElement === document.querySelector(".modal #confirm-input")`)
	p.run(chromedp.SendKeys(`.modal #confirm-input`, heading, chromedp.ByQuery), chromedp.KeyEvent(kb.Enter))
	p.waitFor(`document.querySelector(".modal") === null && location.pathname === "/datastore/__namespace__/tenant-b/Part"`)
	if d := readDataPage(t, "datastore", project, "__namespace__", "tenant-b", "Part", child); !strings.Contains(d.Unavailable, "no such entity") {
		t.Errorf("after Delete entity the child's page reads %+v", d)
	}
}

// TestFirestoreMissingDocumentIsListedInItalics (#875). A document that
// does not exist but has a subcollection is listed on its collection's page
// in italics, its Fields cell saying it has none and has subcollections; its
// row opens its page, whose Collections tab opens the subcollection.
func TestFirestoreMissingDocumentIsListedInItalics(t *testing.T) {
	needService(t, "firestore")
	p := open(t)
	project := uniqueProject(t)
	q := "?project=" + project
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/firestore"+q,
		`{"collection":"users","documentId":"alice","field":"name","type":"string","value":"Alice"}`); code != http.StatusOK {
		t.Fatalf("start a collection through the console API = %d: %s", code, body)
	}
	// Start collection on users/ghost, which does not exist: its
	// subcollection's document does, and it does not.
	if code, body := consoleDo(t, http.MethodPost, "/api/actions/firestore"+q,
		`{"Path":["users","ghost"],"Action":"startcollection","Values":{"collection":"orders","documentId":"o1"}}`); code != http.StatusOK {
		t.Fatalf("start a subcollection under a missing document = %d: %s", code, body)
	}

	p.navigate("/firestore/users" + q)
	ghostRow := `[...document.querySelectorAll("#view tbody tr")].find((r) => r.querySelector("a") && r.querySelector("a").textContent === "ghost")`
	p.waitFor(ghostRow + ` !== undefined`)
	var got struct {
		Absent, AliceAbsent bool
		Style, Text         string
	}
	p.eval(`(() => { const r = `+ghostRow+`;
		const alice = [...document.querySelectorAll("#view tbody tr")].find((r) => r.textContent.includes("alice"));
		return { Absent: r.classList.contains("is-absent"), AliceAbsent: alice.classList.contains("is-absent"),
		         Style: getComputedStyle(r.querySelector("td a")).fontStyle, Text: r.textContent }; })()`, &got)
	if !got.Absent || got.AliceAbsent || got.Style != "italic" || !strings.Contains(got.Text, "no fields — has subcollections") {
		t.Errorf("the missing document's row is %+v; want it alone in italics, saying it has no fields and subcollections", got)
	}

	p.clickText("#view tbody a", "ghost")
	p.waitFor(`location.pathname === "/firestore/users/ghost" && document.querySelector("#tab-collections") !== null`)
	p.run(chromedp.Click(`#tab-collections`, chromedp.ByQuery))
	p.clickText("#view tbody a", "orders")
	p.waitFor(`location.pathname === "/firestore/users%2Fghost%2Forders" && ` +
		`[...document.querySelectorAll("#view tbody a")].some((a) => a.textContent === "o1")`)
}

// TestDatastoreRootEntityNamedLikeAKeyPathOpens (#875). A root entity named
// Customer/alice/Order/x and the child that path names are both listed in
// kind Order; each row opens its own entity's page, addressed by its
// encoded key and headed and crumbed by its key, and Delete entity on the
// root's page, confirmed by typing that key back, deletes the root and
// leaves the child.
func TestDatastoreRootEntityNamedLikeAKeyPathOpens(t *testing.T) {
	needService(t, "datastore")
	p := open(t)
	project := uniqueProject(t)
	q := "?project=" + project
	const name = "Customer/alice/Order/x"
	for _, body := range []string{
		`{"kind":"Customer","key":"alice"}`,
		`{"kind":"Order","key":"` + name + `","field":"who","type":"string","value":"root"}`,
	} {
		if code, out := consoleDo(t, http.MethodPost, "/api/resources/datastore"+q, body); code != http.StatusOK {
			t.Fatalf("create %s through the console API = %d: %s", body, code, out)
		}
	}
	if code, out := consoleDo(t, http.MethodPost, "/api/actions/datastore"+q,
		`{"Path":["Customer","alice"],"Action":"createchild","Values":{"kind":"Order","key":"x","field":"who","type":"string","value":"child"}}`); code != http.StatusOK {
		t.Fatalf("create the child through the console API = %d: %s", code, out)
	}

	// Where each row opens, and which entity that is, from the API.
	v := url.Values{"project": {project}, "name": {"Order"}}
	code, body := consoleDo(t, http.MethodGet, "/api/detail/datastore?"+v.Encode(), "")
	var kind struct {
		Sections []struct {
			Listing struct {
				Items []struct {
					Name   string
					Fields map[string]string
					Opens  []string
				}
			}
		}
	}
	if err := json.Unmarshal([]byte(body), &kind); code != http.StatusOK || err != nil || len(kind.Sections) == 0 {
		t.Fatalf("read kind Order = %d (%v): %s", code, err, body)
	}
	addr := map[string]string{}
	heading := map[string]string{"root": "name=" + name, "child": "Customer/name=alice/Order/name=x"}
	for _, it := range kind.Sections[0].Listing.Items {
		if len(it.Opens) != 2 {
			t.Fatalf("kind Order lists %+v, want each entity opening its own address", it)
		}
		addr[strings.TrimPrefix(it.Fields["Properties"], "who: ")] = it.Opens[1]
	}
	if len(addr) != 2 || addr["root"] == addr["child"] {
		t.Fatalf("the root and the child open %v, want two addresses", addr)
	}

	p.navigate("/datastore/Order" + q)
	for _, who := range []string{"child", "root"} {
		p.waitFor(fmt.Sprintf(`document.querySelector('#view tbody a[href^="/datastore/Order/%s"]') !== null`, addr[who]))
		p.run(chromedp.Click(fmt.Sprintf(`#view tbody a[href^="/datastore/Order/%s"]`, addr[who]), chromedp.ByQuery))
		p.waitFor(`location.pathname === "/datastore/Order/` + addr[who] + `" && ` +
			`document.querySelector("#view h1").textContent === "` + heading[who] + `" && ` +
			`document.querySelector("#view .breadcrumb").textContent === "Datastore/Order/` + heading[who] + `"`)
		p.waitFor(`document.querySelector("#view").textContent.includes("` + who + `")`)
		if who == "child" {
			p.navigate("/datastore/Order" + q)
		}
	}

	// On the root's page: Delete entity, confirmed by the key it is headed
	// by, not its address.
	p.clickText("#view .page-actions button", "Delete entity")
	p.waitFor(`document.activeElement === document.querySelector(".modal #confirm-input")`)
	p.run(chromedp.SendKeys(`.modal #confirm-input`, heading["root"], chromedp.ByQuery), chromedp.KeyEvent(kb.Enter))
	p.waitFor(`document.querySelector(".modal") === null && location.pathname === "/datastore/Order"`)
	if d := readDataPage(t, "datastore", project, "Order", addr["root"]); !strings.Contains(d.Unavailable, "no such entity") {
		t.Errorf("after Delete entity the root's page reads %+v", d)
	}
	if d := readDataPage(t, "datastore", project, "Order", addr["child"]); d.summary("Parent") != "Customer/name=alice" {
		t.Errorf("after the root's delete the child's page reads %+v", d)
	}
}

// TestDatastoreKindListShowsEachRowsParent (#882). A root entity named
// Customer/alice/Order/x and the child that key path names are both on kind
// Order's page; its Parent column tells them apart — none for the root,
// Customer/name=alice for the child — and each row's link opens its own
// entity's page.
func TestDatastoreKindListShowsEachRowsParent(t *testing.T) {
	needService(t, "datastore")
	p := open(t)
	project := uniqueProject(t)
	q := "?project=" + project
	const name = "Customer/alice/Order/x"
	for _, body := range []string{
		`{"kind":"Customer","key":"alice"}`,
		`{"kind":"Order","key":"` + name + `","field":"who","type":"string","value":"root"}`,
	} {
		if code, out := consoleDo(t, http.MethodPost, "/api/resources/datastore"+q, body); code != http.StatusOK {
			t.Fatalf("create %s through the console API = %d: %s", body, code, out)
		}
	}
	if code, out := consoleDo(t, http.MethodPost, "/api/actions/datastore"+q,
		`{"Path":["Customer","alice"],"Action":"createchild","Values":{"kind":"Order","key":"x","field":"who","type":"string","value":"child"}}`); code != http.StatusOK {
		t.Fatalf("create the child through the console API = %d: %s", code, out)
	}

	p.navigate("/datastore/Order" + q)
	p.waitFor(`document.querySelectorAll("#view tbody tr").length === 2`)
	var got struct {
		Header []string
		Rows   map[string]string // Properties cell → Parent cell
		Hrefs  map[string]string // Properties cell → link
	}
	p.eval(`(() => {
		const header = [...document.querySelectorAll("#view thead th")].map((th) => th.textContent.trim());
		const at = (name) => header.indexOf(name);
		const rows = {}, hrefs = {};
		for (const tr of document.querySelectorAll("#view tbody tr")) {
			const cells = [...tr.children].map((td) => td.textContent.trim());
			rows[cells[at("Properties")]] = cells[at("Parent")];
			hrefs[cells[at("Properties")]] = tr.querySelector("a").getAttribute("href");
		}
		return { Header: header, Rows: rows, Hrefs: hrefs };
	})()`, &got)
	if got.Rows["who: root"] != "none (root entity)" || got.Rows["who: child"] != "Customer/name=alice" {
		t.Fatalf("kind Order's rows read %v under header %v; want the root's Parent none and the child's Customer/name=alice",
			got.Rows, got.Header)
	}
	if got.Hrefs["who: root"] == got.Hrefs["who: child"] {
		t.Fatalf("both rows link to %s", got.Hrefs["who: root"])
	}

	p.run(chromedp.Click(`#view tbody a[href="`+got.Hrefs["who: child"]+`"]`, chromedp.ByQuery))
	p.waitFor(`document.querySelector("#view h1").textContent === "Customer/name=alice/Order/name=x" && ` +
		`document.querySelector("#view").textContent.includes("child")`)
}

// TestDatastoreNameIDColumnTellsANameFromAnID (#885). A root entity named "id=7"
// — made with Create entity's Key identifier name=id=7 — and the root entity
// whose numeric ID is 7 were listed alike, id=7 with no parent. Kind Order's
// Name/ID column now reads name=id=7 and id=7, as Google's console's does;
// each row opens its own page, headed and crumbed by that Name/ID, and
// Delete entity on the named one, confirmed by typing name=id=7, deletes it
// and leaves the numeric one. An old-form link, /datastore/Order/id=7,
// opens the numeric one.
func TestDatastoreNameIDColumnTellsANameFromAnID(t *testing.T) {
	needService(t, "datastore")
	p := open(t)
	project := uniqueProject(t)
	q := "?project=" + project
	for _, body := range []string{
		`{"kind":"Order","key":"name=id=7","field":"who","type":"string","value":"named"}`,
		`{"kind":"Order","key":"id=7","field":"who","type":"string","value":"numeric"}`,
	} {
		if code, out := consoleDo(t, http.MethodPost, "/api/resources/datastore"+q, body); code != http.StatusOK {
			t.Fatalf("create %s through the console API = %d: %s", body, code, out)
		}
	}

	p.navigate("/datastore/Order" + q)
	p.waitFor(`document.querySelectorAll("#view tbody tr").length === 2`)
	var got struct {
		Header []string
		Rows   map[string]string // Properties cell → Name/ID cell
		Hrefs  map[string]string // Properties cell → link
	}
	p.eval(`(() => {
		const header = [...document.querySelectorAll("#view thead th")].map((th) => th.textContent.trim());
		const rows = {}, hrefs = {};
		for (const tr of document.querySelectorAll("#view tbody tr")) {
			const cells = [...tr.children].map((td) => td.textContent.trim());
			rows[cells[header.indexOf("Properties")]] = cells[header.indexOf("Name/ID")];
			hrefs[cells[header.indexOf("Properties")]] = tr.querySelector("a").getAttribute("href");
		}
		return { Header: header, Rows: rows, Hrefs: hrefs };
	})()`, &got)
	if got.Rows["who: named"] != "name=id=7" || got.Rows["who: numeric"] != "id=7" {
		t.Fatalf("kind Order's rows read %v under header %v; want name=id=7 and id=7 in the Name/ID column", got.Rows, got.Header)
	}
	if got.Hrefs["who: named"] == got.Hrefs["who: numeric"] {
		t.Fatalf("both rows link to %s", got.Hrefs["who: named"])
	}

	for _, who := range []string{"numeric", "named"} {
		head := got.Rows["who: "+who]
		p.run(chromedp.Click(`#view tbody a[href="`+got.Hrefs["who: "+who]+`"]`, chromedp.ByQuery))
		p.waitFor(`document.querySelector("#view h1").textContent === "` + head + `" && ` +
			`document.querySelector("#view .breadcrumb").textContent === "Datastore/Order/` + head + `" && ` +
			`document.querySelector("#view").textContent.includes("` + who + `")`)
		if who == "numeric" {
			p.navigate("/datastore/Order" + q)
			p.waitFor(`document.querySelectorAll("#view tbody tr").length === 2`)
		}
	}

	// On the named one's page: Delete entity, confirmed by its Name/ID.
	p.clickText("#view .page-actions button", "Delete entity")
	p.waitFor(`document.activeElement === document.querySelector(".modal #confirm-input")`)
	p.run(chromedp.SendKeys(`.modal #confirm-input`, "name=id=7", chromedp.ByQuery), chromedp.KeyEvent(kb.Enter))
	p.waitFor(`document.querySelector(".modal") === null && location.pathname === "/datastore/Order"`)
	p.waitFor(`document.querySelectorAll("#view tbody tr").length === 1 && ` +
		`document.querySelector("#view tbody a").textContent.trim() === "id=7"`)

	// The old-form link opens the numeric ID, as it did before.
	p.navigate("/datastore/Order/id=7" + q)
	p.waitFor(`document.querySelector("#view h1") && document.querySelector("#view h1").textContent === "id=7" && ` +
		`document.querySelector("#view").textContent.includes("numeric")`)
}

// envDatastore is the Datastore emulator's address, which the suite needs
// only to write and read back an entity with the official client
// (TestDatastoreKeyValueEditPropertyIsANoOp).
const envDatastore = "CLOUDBURROW_TEST_DATASTORE"

// TestDatastoreKeyValueEditPropertyIsANoOp (#887). A key-valued property
// pointing at Order named "id=7" was shown as Order/id=7, as the key to the
// numeric ID 7 is, and saving Edit property unchanged rewrote it to the
// numeric key. An entity holds both, and a key in a namespace, whose name has
// a slash. Its page lists them as Order/name=id=7, Order/id=7 and
// __namespace__/tenant-a/Order/name=a%2Fb; on each property's page Edit
// property is prefilled with that, and saving it unchanged leaves the key as
// it was. With CLOUDBURROW_TEST_DATASTORE set, the entity is written and read
// back with the official client; without it, it is made with Create entity
// and Add property, which read that rendering, and read back through the
// console API.
func TestDatastoreKeyValueEditPropertyIsANoOp(t *testing.T) {
	needService(t, "datastore")
	p := open(t)
	project := uniqueProject(t)
	q := "?project=" + project
	named := datastore.NameKey("Order", "id=7", nil)
	numeric := datastore.IDKey("Order", 7, nil)
	namespaced := datastore.NameKey("Order", "a/b", nil)
	namespaced.Namespace = "tenant-a"
	want := map[string]*datastore.Key{"named": named, "numeric": numeric, "namespaced": namespaced}
	shown := map[string]string{"named": "Order/name=id=7", "numeric": "Order/id=7",
		"namespaced": "__namespace__/tenant-a/Order/name=a%2Fb"}
	holder := datastore.NameKey("Holder887", "h", nil)

	var client *datastore.Client
	if addr := strings.TrimSpace(os.Getenv(envDatastore)); addr != "" {
		t.Setenv("DATASTORE_EMULATOR_HOST", strings.TrimPrefix(addr, "http://"))
		c, err := datastore.NewClient(context.Background(), project)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		client = c
		props := datastore.PropertyList{}
		for name, k := range want {
			props = append(props, datastore.Property{Name: name, Value: k})
		}
		if _, err := c.Put(context.Background(), holder, &props); err != nil {
			t.Fatalf("Put %v with the official client: %v", holder, err)
		}
	} else {
		if code, body := consoleDo(t, http.MethodPost, "/api/resources/datastore"+q,
			`{"kind":"Holder887","key":"name=h","field":"named","type":"key","value":"Order/name=id=7"}`); code != http.StatusOK {
			t.Fatalf("create the entity through the console API = %d: %s", code, body)
		}
		for _, prop := range []string{"numeric", "namespaced"} {
			body, _ := json.Marshal(map[string]any{"Path": []string{"Holder887", holder.Encode()}, "Action": "addproperty",
				"Values": map[string]string{"field": prop, "type": "key", "value": shown[prop]}})
			if code, out := consoleDo(t, http.MethodPost, "/api/actions/datastore"+q, string(body)); code != http.StatusOK {
				t.Fatalf("Add property %s through the console API = %d: %s", prop, code, out)
			}
		}
	}

	p.navigate("/datastore/Holder887" + q)
	p.clickText("#view tbody a", "name=h")
	cells := func() map[string]string {
		p.waitFor(`document.querySelectorAll("#view tbody tr").length === 3`)
		var got map[string]string
		p.eval(`(() => {
			const header = [...document.querySelectorAll("#view thead th")].map((th) => th.textContent.trim());
			const out = {};
			for (const tr of document.querySelectorAll("#view tbody tr")) {
				const c = [...tr.children].map((td) => td.textContent.trim());
				out[c[header.indexOf("Property")]] = c[header.indexOf("Value")];
			}
			return out;
		})()`, &got)
		return got
	}
	if got := cells(); !reflect.DeepEqual(got, shown) {
		t.Errorf("the entity's properties read %v, want %v", got, shown)
	}
	for _, prop := range []string{"named", "numeric", "namespaced"} {
		p.clickText("#view tbody a", prop)
		// Every property's page offers Delete property; Edit property only
		// when the form can hold the value.
		p.waitFor(`document.querySelector("#view h1") && document.querySelector("#view h1").textContent === "` + prop + `" && ` +
			`[...document.querySelectorAll("#view .page-actions button")].some((b) => b.textContent.trim() === "Delete property")`)
		var editable bool
		p.eval(`[...document.querySelectorAll("#view .page-actions button")].some((b) => b.textContent.trim() === "Edit property")`, &editable)
		if !editable {
			t.Errorf("%s's page offers no Edit property", prop)
			p.navigate("/datastore/Holder887" + q)
			p.clickText("#view tbody a", "name=h")
			continue
		}
		p.clickText("#view .page-actions button", "Edit property")
		p.waitFor(`document.querySelector(".modal.is-open #f-value") !== null`)
		var form struct{ Type, Value string }
		p.eval(`(() => { const q = (s) => document.querySelector(".modal.is-open " + s);
			return { Type: q("#f-type").value, Value: q("#f-value").value }; })()`, &form)
		if form.Type != "key" || form.Value != shown[prop] {
			t.Errorf("%s's Edit property is prefilled %s %q, want key %q", prop, form.Type, form.Value, shown[prop])
		}
		// Saved unchanged.
		p.run(chromedp.Click(`.modal.is-open button[type="submit"]`, chromedp.ByQuery))
		p.waitFor(`document.querySelector(".modal") === null`)
		p.navigate("/datastore/Holder887" + q)
		p.clickText("#view tbody a", "name=h")
	}
	if got := cells(); !reflect.DeepEqual(got, shown) {
		t.Errorf("after an unchanged Edit property on each, the properties read %v, want %v", got, shown)
	}
	if client != nil {
		var props datastore.PropertyList
		if err := client.Get(context.Background(), holder, &props); err != nil {
			t.Fatalf("Get %v with the official client: %v", holder, err)
		}
		for _, prop := range props {
			if k, isKey := prop.Value.(*datastore.Key); !isKey || !k.Equal(want[prop.Name]) {
				t.Errorf("the official client reads %s back as %#v, want %v", prop.Name, prop.Value, want[prop.Name])
			}
		}
	}
}

// TestFirestoreCollectionCountIncludesMissingDocuments (#882). The Firestore
// screen's Documents column counts a document that does not exist but has
// subcollections, as the collection's page lists it, and the screen's note
// says so.
func TestFirestoreCollectionCountIncludesMissingDocuments(t *testing.T) {
	needService(t, "firestore")
	p := open(t)
	project := uniqueProject(t)
	q := "?project=" + project
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/firestore"+q,
		`{"collection":"users","documentId":"alice","field":"name","type":"string","value":"Alice"}`); code != http.StatusOK {
		t.Fatalf("start a collection through the console API = %d: %s", code, body)
	}
	if code, body := consoleDo(t, http.MethodPost, "/api/actions/firestore"+q,
		`{"Path":["users","ghost"],"Action":"startcollection","Values":{"collection":"orders","documentId":"o1"}}`); code != http.StatusOK {
		t.Fatalf("start a subcollection under a missing document = %d: %s", code, body)
	}

	p.navigate("/firestore" + q)
	usersRow := `[...document.querySelectorAll("#view tbody tr")].find((r) => r.querySelector("a") && r.querySelector("a").textContent === "users")`
	p.waitFor(usersRow + ` !== undefined`)
	var got struct{ Count, Page string }
	p.eval(`(() => {
		const header = [...document.querySelectorAll("#view thead th")].map((th) => th.textContent.trim());
		const r = `+usersRow+`;
		return { Count: r.children[header.indexOf("Documents")].textContent.trim(), Page: document.querySelector("#view").textContent };
	})()`, &got)
	if got.Count != "2" || !strings.Contains(got.Page, "does not exist but has subcollections") {
		t.Errorf("users counts %q documents; want 2, alice and the missing ghost, and a note saying so", got.Count)
	}
}
