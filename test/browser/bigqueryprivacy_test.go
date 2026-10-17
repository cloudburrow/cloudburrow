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

// TestBigQueryAnalysisRulesOnTheTablePage (#1580), in the emulators shard.
// A view whose CREATE VIEW … OPTIONS (privacy_policy = …) the Read-write
// editor runs shows its analysis rules in its Details tab (read from
// INFORMATION_SCHEMA.TABLE_OPTIONS): its aggregation threshold and join
// restriction, each with its settings, and where to edit them, since
// Google's console has no form for them. ALTER VIEW … SET OPTIONS
// (privacy_policy = NULL) in the editor removes them from the page.
func TestBigQueryAnalysisRulesOnTheTablePage(t *testing.T) {
	needService(t, "bigquery")
	p := open(t)
	project := servedProject(t)
	ds := fmt.Sprintf("browser_privacy_%d", time.Now().UnixNano()%1e12)
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
	sql("CREATE TABLE " + ds + ".exams AS SELECT 'Wang' AS last_name, 'U25' AS test_id, 500 AS test_score")
	sql("CREATE VIEW " + ds + ".rules OPTIONS (privacy_policy = '" +
		`{"aggregation_threshold_policy": {"threshold": 3, "privacy_unit_column": "last_name"}, ` +
		`"join_restriction_policy": {"join_condition": "JOIN_ANY", "join_allowed_columns": ["test_id"]}}` +
		"') AS SELECT * FROM " + ds + ".exams")

	var text string
	p.navigate("/bigquery/" + ds + "/rules" + q + "&tab=details")
	p.waitFor(`document.querySelector("#view").textContent.includes("Analysis rules")`)
	p.eval(`document.querySelector("#view").textContent`, &text)
	for _, want := range []string{"Aggregation threshold", "privacy_unit_column: last_name; threshold: 3",
		"Join restriction", "join_allowed_columns: test_id; join_condition: JOIN_ANY",
		"Edit analysis rules", "ALTER VIEW `" + ds + ".rules` SET OPTIONS (privacy_policy"} {
		if !strings.Contains(text, want) {
			t.Errorf("the view's Details do not show %q: %.1500s", want, text)
		}
	}

	p.navigate("/bigquery/" + ds + "/exams" + q + "&tab=details")
	p.waitFor(`document.querySelector("#view").textContent.includes("Table ID")`)
	p.eval(`document.querySelector("#view").textContent`, &text)
	if strings.Contains(text, "Analysis rules") {
		t.Errorf("a table with no privacy_policy shows analysis rules: %.800s", text)
	}

	sql("ALTER VIEW " + ds + ".rules SET OPTIONS (privacy_policy = NULL)")
	p.navigate("/bigquery/" + ds + "/rules" + q + "&tab=details")
	p.waitFor(`document.querySelector("#view").textContent.includes("Table ID")`)
	p.eval(`document.querySelector("#view").textContent`, &text)
	if strings.Contains(text, "Analysis rules") {
		t.Errorf("the view still shows analysis rules after privacy_policy = NULL: %.800s", text)
	}
}
