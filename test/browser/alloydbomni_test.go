//go:build browser

package browser

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestAlloyDBOmniTermsCheckboxThroughThePage (#2207), in the sql shard,
// whose instance serves AlloyDB and was not started accepting the AlloyDB
// Omni terms. Create cluster has the terms' checkbox, unchecked, with
// Google's AlloyDB Omni License Terms linked beside it; a cluster created
// with it left unchecked runs PostgreSQL, which its page says. The test
// never checks the box: only the person creating a cluster may accept
// Google's terms. The page meets WCAG 2.1 AA, and the checkbox and the
// terms' link are reached from the keyboard.
func TestAlloyDBOmniTermsCheckboxThroughThePage(t *testing.T) {
	needService(t, "alloydb")
	project := uniqueProject(t)
	const terms = "https://cloud.google.com/terms/alloydb-omni-marketplace-terms"
	p := open(t)
	p.navigate("/alloydb/clusters?project=" + project)
	p.clickText("#view button", "Create cluster")
	p.waitFor(`document.getElementById("f-omniTerms") !== null`)
	var box struct {
		Type    string   `json:"type"`
		Checked bool     `json:"checked"`
		Label   string   `json:"label"`
		Links   []string `json:"links"`
	}
	p.eval(`(() => { const b = document.getElementById("f-omniTerms"); const row = b.closest(".form-row");
		return { type: b.type, checked: b.checked, label: row.textContent,
			links: [...row.querySelectorAll("a")].map((a) => a.href) }; })()`, &box)
	if box.Type != "checkbox" || box.Checked || !strings.Contains(box.Label, "I accept the AlloyDB Omni licence terms") || !contains(box.Links, terms) {
		t.Errorf("the terms' checkbox: %+v", box)
	}

	// Left unchecked, the cluster runs PostgreSQL.
	p.submitForm(map[string]string{"clusterId": "omni-off", "password": "burrow-pw", "instanceId": "primary"}, "clusterId")
	cluster := "projects/" + project + "/locations/us-central1/clusters/omni-off"
	p.waitFor(listedRow("/alloydb/clusters", "omni-off"))
	q := url.Values{"project": {project}, "name": {cluster}}
	deadline := time.Now().Add(time.Minute)
	for {
		code, body := consoleDo(t, http.MethodGet, "/api/detail/alloydb?"+q.Encode(), "")
		if code == http.StatusOK && strings.Contains(body, "PostgreSQL (AlloyDB Omni") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the cluster's Database engine is not PostgreSQL: %d %s", code, body)
		}
		time.Sleep(time.Second)
	}
	t.Cleanup(func() {
		_, _ = consoleDo(t, http.MethodPost, "/api/actions/alloydb?project="+url.QueryEscape(project),
			`{"Path":["`+cluster+`"],"Action":"delete","Values":{}}`)
	})

	t.Run("keyboard", func(t *testing.T) {
		k := open(t)
		k.navigate("/alloydb/clusters/create?project=" + project)
		k.waitFor(`document.getElementById("f-omniTerms") !== null`)
		k.tabTo(`a.id === "f-omniTerms"`)
		k.tabTo(`a.tagName === "A" && a.href === "` + terms + `"`)
	})
	t.Run("wcag", func(t *testing.T) {
		auditScreens(t, []a11yPage{screen("create cluster", "/alloydb/clusters/create", project)})
	})
}
