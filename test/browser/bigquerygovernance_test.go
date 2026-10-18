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
)

// TestBigQueryDataGovernanceTagsOnTheSchemaTab (#1456), in the emulators
// shard. A column's data_governance_tags, bound by ALTER TABLE … ALTER
// COLUMN … SET OPTIONS in the Read-write editor, shows in the table's
// Schema tab beside the column, with where to bind them, since Google's
// console shows them but has no form for them; NULL removes them from the
// page.
func TestBigQueryDataGovernanceTagsOnTheSchemaTab(t *testing.T) {
	needService(t, "bigquery")
	p := open(t)
	project := servedProject(t)
	ds := fmt.Sprintf("browser_govtags_%d", time.Now().UnixNano()%1e12)
	q := "?project=" + url.QueryEscape(project)
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/bigquery"+q, `{"datasetId":"`+ds+`"}`); code != http.StatusOK {
		t.Fatalf("create a dataset through the console API = %d: %s", code, body)
	}
	t.Cleanup(func() { consoleDo(t, http.MethodDelete, "/api/resources/bigquery"+q+"&name="+ds, "") })
	sql := func(stmt string) {
		t.Helper()
		b, _ := json.Marshal(map[string]any{"Path": []string{ds}, "Statement": stmt, "Mode": "read-write"})
		if code, body := consoleDo(t, http.MethodPost, "/api/query/bigquery"+q, string(b)); code != http.StatusOK {
			t.Fatalf("%s through the console API = %d: %s", stmt, code, body)
		}
	}
	sql("CREATE TABLE " + ds + ".people (phone STRING, n INT64)")
	sql("ALTER TABLE " + ds + ".people ALTER COLUMN phone SET OPTIONS (data_governance_tags = [('my-project/data_class', 'private')])")

	var text string
	p.navigate("/bigquery/" + ds + "/people" + q + "&tab=schema")
	p.waitFor(`document.querySelector("#view").textContent.includes("Data governance tags")`)
	p.eval(`document.querySelector("#view").textContent`, &text)
	for _, want := range []string{"Data governance tags", "my-project/data_class: private", "has no form to bind them",
		"SET OPTIONS (data_governance_tags"} {
		if !strings.Contains(text, want) {
			t.Errorf("the Schema tab does not show %q: %.1500s", want, text)
		}
	}

	sql("ALTER TABLE " + ds + ".people ALTER COLUMN phone SET OPTIONS (data_governance_tags = NULL)")
	p.navigate("/bigquery/" + ds + "/people" + q + "&tab=schema")
	p.waitFor(`document.querySelector("#view").textContent.includes("phone")`)
	p.eval(`document.querySelector("#view").textContent`, &text)
	if strings.Contains(text, "my-project/data_class") || strings.Contains(text, "Data governance tags") {
		t.Errorf("the Schema tab still shows the tags after data_governance_tags = NULL: %.800s", text)
	}
}
