//go:build browser

package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	aiplatform "cloud.google.com/go/aiplatform/apiv1"
	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// TestVertexAIDatasetsThroughThePages (#2210): Agent Platform > Model
// development > Datasets, as Google's "Create an image dataset" and
// "Create a tabular dataset" walk them. Create opens Create dataset
// (Dataset name, the data type and its objective, then Select an import
// file from Cloud Storage); an image dataset made with a JSON Lines
// classification file lists 2 items, and its page's Browse shows each
// item with its label, read back through the official DatasetClient. The
// dataset page's Import adds a CSV file's item, and its Export completes. A tabular dataset made from Select a table or view
// from BigQuery shows the table as its source. Delete removes both. The
// keyboard subtest chooses the data type with the arrow keys and opens a
// row from the keyboard; the WCAG subtest audits every screen.
func TestVertexAIDatasetsThroughThePages(t *testing.T) {
	for _, s := range []string{"vertex-datasets", "storage"} {
		needService(t, s)
	}
	project := servedProject(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	dc, err := aiplatform.NewDatasetClient(ctx, localAIDial(t)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dc.Close() })
	parent := "projects/" + project + "/locations/us-central1"
	q := "?project=" + url.QueryEscape(project) + "&region=us-central1"
	stamp := time.Now().UnixNano() % 1e6
	bucket := fmt.Sprintf("datasets-%d", stamp)
	fixture(t, "storage", project, `{"name":"`+bucket+`"}`, bucket)
	uploadObject(t, project, []string{bucket, "img"}, "a.jpg", "image a")
	uploadObject(t, project, []string{bucket, "img"}, "b.jpg", "image b")
	uploadObject(t, project, []string{bucket, "img"}, "c.jpg", "image c")
	uploadObject(t, project, []string{bucket}, "flowers.jsonl",
		`{"imageGcsUri": "gs://`+bucket+`/img/a.jpg", "classificationAnnotation": {"displayName": "daisy"}}`+"\n"+
			`{"imageGcsUri": "gs://`+bucket+`/img/b.jpg", "classificationAnnotation": {"displayName": "rose"}}`+"\n")
	uploadObject(t, project, []string{bucket}, "more.csv", "gs://"+bucket+"/img/c.jpg,tulip\n")

	type item struct {
		Name   string
		Fields map[string]string
	}
	find := func(name string) item {
		t.Helper()
		code, body := consoleDo(t, http.MethodGet, "/api/resources/vertex-datasets"+q, "")
		if code != http.StatusOK {
			t.Fatalf("list datasets = %d: %s", code, body)
		}
		var l struct{ Items []item }
		_ = json.Unmarshal([]byte(body), &l)
		for _, it := range l.Items {
			if it.Fields["Name"] == name {
				return it
			}
		}
		return item{}
	}
	viewHas := func(texts ...string) string {
		var parts []string
		for _, s := range texts {
			parts = append(parts, fmt.Sprintf(`document.querySelector("#view").textContent.includes(%q)`, s))
		}
		return strings.Join(parts, " && ")
	}
	inTable := func(text string) string {
		return fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q))`, text)
	}

	p := open(t)
	listPath := "/agent-platform/datasets" + q

	// Create dataset: an image classification dataset from a JSON Lines file.
	p.navigate(listPath)
	p.waitFor(settled)
	p.clickText("#view a, #view button", "Create")
	p.waitFor(`document.querySelector("#view h1") && document.querySelector("#view h1").textContent.includes("Create dataset")`)
	var createPath string
	p.eval(`location.pathname + location.search`, &createPath)
	imgName := fmt.Sprintf("browser flowers %d", stamp)
	p.submitForm(map[string]string{"displayName": imgName, "importPath": "gs://" + bucket + "/flowers.jsonl"}, "displayName")
	// The dataset is listed at once; its import is a long-running
	// operation, so its Items count follows.
	var img item
	for deadline := time.Now().Add(3 * time.Minute); img.Name == "" || img.Fields["Items"] == "0"; time.Sleep(time.Second) {
		if img = find(imgName); (img.Name == "" || img.Fields["Items"] == "0") && time.Now().After(deadline) {
			t.Fatalf("the dataset %q is not listed with its items: %+v", imgName, img)
		}
	}
	name := parent + "/datasets/" + img.Name
	t.Cleanup(func() {
		if op, err := dc.DeleteDataset(context.Background(), &aiplatformpb.DeleteDatasetRequest{Name: name}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	if img.Fields["Data type"] != "Image" || img.Fields["Items"] != "2" {
		t.Errorf("the dataset's row = %+v", img)
	}
	got, err := dc.GetDataset(ctx, &aiplatformpb.GetDatasetRequest{Name: name})
	if err != nil || got.GetMetadataSchemaUri() != "gs://google-cloud-aiplatform/schema/dataset/metadata/image_1.0.0.yaml" || got.GetDataItemCount() != 2 {
		t.Errorf("GetDataset = %v %v", got, err)
	}

	// Its page: Browse, then Import and Export.
	dsPath := "/agent-platform/datasets/" + url.PathEscape(img.Name) + q
	p.navigate(dsPath)
	p.waitFor(viewHas("gs://"+bucket+"/img/a.jpg", "daisy", "rose"))
	p.clickText("#view .page-actions a, #view .page-actions button", "Import")
	p.submitForm(map[string]string{"importPath": "gs://" + bucket + "/more.csv"}, "importPath")
	p.waitFor(`document.querySelector(".modal.is-open") === null`)
	p.navigate(dsPath)
	p.waitFor(viewHas("gs://"+bucket+"/img/c.jpg", "tulip"))
	p.clickText("#view .page-actions a, #view .page-actions button", "Export")
	p.submitForm(map[string]string{"exportPath": "gs://" + bucket + "/export"}, "exportPath")
	p.waitFor(`document.querySelector(".modal.is-open") === null`)
	if g, err := dc.GetDataset(ctx, &aiplatformpb.GetDatasetRequest{Name: name}); err != nil || g.GetDataItemCount() != 3 {
		t.Errorf("after Import: %v %v", g.GetDataItemCount(), err)
	}

	// A tabular dataset from a BigQuery table.
	tabName := fmt.Sprintf("browser table %d", stamp)
	p.navigate(createPath)
	p.waitFor(`document.querySelector("#view #f-displayName") !== null`)
	// The name field starts focused: leaving it empty shows its error,
	// which moves the radios down, so it is left before a radio is clicked.
	p.eval(`document.activeElement && document.activeElement.blur(), true`, nil)
	clickRadio(p, "dataType", 1) // Tabular
	p.waitFor(`document.querySelector("#view #f-dataSource-1") && document.querySelector("#view #f-dataSource-1").getClientRects().length > 0`)
	clickRadio(p, "dataSource", 1) // BigQuery
	bq := "bq://" + project + ".sales.orders"
	p.submitForm(map[string]string{"displayName": tabName, "bqPath": bq}, "displayName")
	var tab item
	for deadline := time.Now().Add(time.Minute); tab.Name == ""; time.Sleep(time.Second) {
		if tab = find(tabName); tab.Name == "" && time.Now().After(deadline) {
			t.Fatalf("the dataset %q is not listed", tabName)
		}
	}
	tabPath := "/agent-platform/datasets/" + url.PathEscape(tab.Name) + q
	p.navigate(tabPath)
	p.waitFor(viewHas(bq, "Tabular"))

	t.Run("keyboard", func(t *testing.T) {
		parent := p.t
		p.t = t
		defer func() { p.t = parent }()
		// The data type is a radio group: the arrow keys move within it.
		p.navigate(createPath)
		p.waitFor(`document.querySelector("#view #f-dataType-0") !== null`)
		p.tabTo(`a.id === "f-dataType-0"`)
		p.run(chromedp.KeyEvent(kb.ArrowDown))
		p.waitFor(`document.querySelector("#view #f-dataType-1").checked && document.querySelector("#view #f-dataSource-0").getClientRects().length > 0`)
		// Tab reaches the dataset's row, Enter opens its page.
		p.navigate(listPath)
		p.waitFor(rowReady(img.Name))
		p.tabTo(fmt.Sprintf(`a.textContent.trim() === %q`, img.Name))
		p.run(chromedp.KeyEvent(kb.Enter))
		p.waitFor(`location.pathname === "/agent-platform/datasets/` + img.Name + `"`)
	})

	t.Run("WCAG", func(t *testing.T) {
		auditScreens(t, []a11yPage{
			{"datasets", listPath, rowReady(img.Name), ""},
			{"create dataset", createPath, `document.querySelector("#view #f-displayName") !== null`, ""},
			{"image dataset", dsPath, settled, ""},
			{"tabular dataset", tabPath, settled, ""},
		})
	})

	// Delete each, its ID typed back.
	p = open(t)
	for _, id := range []string{img.Name, tab.Name} {
		p.navigate(listPath)
		p.waitFor(rowReady(id))
		p.openRowDelete(id, "Delete")
		p.confirmByName(id)
		p.waitFor(`document.querySelector(".modal.is-open") === null && !` + inTable(id))
	}
	if _, err := dc.GetDataset(ctx, &aiplatformpb.GetDatasetRequest{Name: name}); err == nil {
		t.Errorf("the dataset %s is still there after Delete", name)
	}
}
