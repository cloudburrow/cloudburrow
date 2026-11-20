//go:build browser

package browser

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// TestSpannerInfoPanelLabelsAndUpgradeThroughThePage (#1859), in a shard
// whose instance serves Spanner: as Google's Instances list has it, checking
// an instance's checkbox opens the Info panel, whose Labels tab shows the
// instance's labels and saves new ones with Save, read back through the
// console API. The instance's page has Upgrade beside its Standard edition,
// which opens Update edition with only the higher editions and saves
// Enterprise Plus, after which there is no Upgrade. Neither Labels nor
// Update edition is a button beside the title. Both the panel and the
// dialog meet the WCAG audit (names, contrast, keyboard, motion, reflow).
func TestSpannerInfoPanelLabelsAndUpgradeThroughThePage(t *testing.T) {
	needService(t, "spanner")
	project := uniqueProject(t)
	q := "?project=" + project
	inst := fmt.Sprintf("browser-info-%d", time.Now().UnixNano()%1e8)
	body, _ := json.Marshal(map[string]string{"displayName": "Info panel instance", "instance": inst, "edition": "Standard",
		"configType": "Regional", "regionalConfig": "regional-us-central1", "unit": "Processing units", "processingUnits": "100",
		"scaling": "Manual allocation", "labels": `{"env":"dev"}`})
	if code, out := consoleDo(t, http.MethodPost, "/api/resources/spanner"+q, string(body)); code != http.StatusOK {
		t.Fatalf("Create instance through the console API = %d: %s", code, out)
	}
	t.Cleanup(func() {
		b, _ := json.Marshal(map[string]any{"Path": []string{inst}, "Action": "dropinstance"})
		consoleDo(t, http.MethodPost, "/api/actions/spanner"+q, string(b))
	})
	checkRow := fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr")].find((r) => r.textContent.includes(%q)).querySelector("input[type=checkbox]").click()`, inst)

	p := open(t)
	p.navigate("/spanner" + q)
	p.waitFor(rowReady(inst))
	p.eval(checkRow, nil)
	p.waitFor(fmt.Sprintf(`(() => { const s = document.querySelector("#info-panel"); return s && !s.hidden && s.textContent.includes(%q) && document.querySelector("#info-tab-labels") !== null })()`, inst))
	var tabs []string
	p.eval(`[...document.querySelectorAll("#info-panel [role=tab]")].map((t) => t.textContent)`, &tabs)
	if !slices.Equal(tabs, []string{"Details", "Labels"}) {
		t.Errorf("the Info panel's tabs are %v, want Details and Labels", tabs)
	}
	// The tablist moves by arrow key.
	p.run(chromedp.Focus(chromedp.CSS(`#info-tab-details`)), chromedp.KeyEvent(kb.ArrowRight))
	p.waitFor(`document.querySelector("#info-tab-labels").getAttribute("aria-selected") === "true" && document.activeElement.id === "info-tab-labels"`)
	p.waitFor(`document.querySelector("#info-tab-panel").textContent.includes("env") && document.querySelector("#info-tab-panel #f-labels") !== null`)
	var def string
	p.eval(`document.querySelector("#info-tab-panel #f-labels").value`, &def)
	if strings.TrimSpace(def) != "env=dev" {
		t.Errorf("the Labels tab's field holds %q, want the instance's env=dev", def)
	}
	setField(p, "#info-tab-panel", "f-labels", "env=prod\nteam=ops")
	p.clickText("#info-tab-panel button", "Save")
	p.waitFor(`document.querySelector("#info-tab-panel").textContent.includes("team") && document.querySelector("#info-tab-panel").textContent.includes("prod")`)
	var listing struct {
		Items []struct {
			Name   string
			Fields map[string]string
		}
	}
	code, list := consoleDo(t, http.MethodGet, "/api/resources/spanner"+q, "")
	if err := json.Unmarshal([]byte(list), &listing); code != http.StatusOK || err != nil {
		t.Fatalf("list instances through the console API = %d (%v): %s", code, err, list)
	}
	labels := "(not listed)"
	for _, it := range listing.Items {
		if it.Name == inst {
			labels = it.Fields["Labels"]
		}
	}
	if labels != "env: prod, team: ops" {
		t.Errorf("the console API lists the instance's labels as %q, want env: prod, team: ops", labels)
	}

	// Upgrade beside the edition.
	p.navigate("/spanner/" + inst + q)
	p.waitFor(`document.querySelector("#view a.property-action") !== null`)
	var buttons []string
	p.eval(`[...document.querySelectorAll("#view .page-actions button")].map((b) => b.textContent)`, &buttons)
	if slices.Contains(buttons, "Labels") || slices.Contains(buttons, "Update edition") {
		t.Errorf("the instance's page buttons are %v; Labels and Update edition are linked, not buttons", buttons)
	}
	var link string
	p.eval(`document.querySelector("#view a.property-action").textContent`, &link)
	if link != "Upgrade" {
		t.Errorf("the link beside the edition reads %q, want Upgrade", link)
	}
	p.run(chromedp.Click(chromedp.CSS(`#view a.property-action`)))
	p.waitFor(`document.querySelector(".modal.is-open #f-edition-0") !== null`)
	var options []string
	p.eval(`[...document.querySelectorAll(".modal.is-open input[type=radio]")].map((r) => r.value)`, &options)
	if !slices.Equal(options, []string{"Enterprise", "Enterprise Plus"}) {
		t.Errorf("Update edition offers %v, want Enterprise and Enterprise Plus", options)
	}
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open #f-edition-1`)))
	p.clickText(".modal.is-open button", "Save")
	p.waitFor(`document.querySelector(".modal") === null`)
	page := readDataPage(t, "spanner", project, inst)
	if got := page.summary("Edition"); got != "Enterprise Plus" {
		t.Errorf("after Update edition the instance's Edition = %q, want Enterprise Plus", got)
	}
	p.navigate("/spanner/" + inst + q)
	p.waitFor(`[...document.querySelectorAll("#view dd")].some((d) => d.textContent.includes("Enterprise Plus"))`)
	var upgrades int
	p.eval(`document.querySelectorAll("#view a.property-action").length`, &upgrades)
	if upgrades != 0 {
		t.Error("an Enterprise Plus instance still has Upgrade beside its edition")
	}

	// An Enterprise instance, for the dialog's audit.
	body, _ = json.Marshal(map[string]string{"displayName": "Upgrade audit", "instance": inst + "-a", "edition": "Enterprise",
		"configType": "Regional", "regionalConfig": "regional-us-central1", "unit": "Processing units", "processingUnits": "100",
		"scaling": "Manual allocation"})
	if code, out := consoleDo(t, http.MethodPost, "/api/resources/spanner"+q, string(body)); code != http.StatusOK {
		t.Fatalf("Create instance through the console API = %d: %s", code, out)
	}
	t.Cleanup(func() {
		b, _ := json.Marshal(map[string]any{"Path": []string{inst + "-a"}, "Action": "dropinstance"})
		consoleDo(t, http.MethodPost, "/api/actions/spanner"+q, string(b))
	})
	auditScreens(t, []a11yPage{
		{"Spanner Info panel Labels tab", "/spanner" + q, rowReady(inst),
			checkRow + `; document.querySelector("#info-tab-labels").click()`},
		// Upgrade's link is the page's own address with ?action=, which
		// reloads the page and opens the dialog once it has drawn: the
		// audit opens that address and waits for the dialog, as a click
		// on the link and a check straight after raced the reload (#2017).
		{"Spanner Update edition dialog", "/spanner/" + inst + "-a" + q + "&action=upgradeedition",
			`document.querySelector(".modal.is-open") !== null`, `void 0`},
	})
}
