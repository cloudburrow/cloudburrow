//go:build browser

package browser

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/localai"
)

// gardenModels is a local model manager for an in-process console: one
// pinned model a Deploy downloads, which then reads as downloading.
type gardenModels struct {
	mu        sync.Mutex
	downloads []string
	state     localai.ModelState
}

func (g *gardenModels) States() []localai.ModelState {
	g.mu.Lock()
	defer g.mu.Unlock()
	return []localai.ModelState{g.state,
		{ID: "gemma-3n-e2b-it", State: "not-pinned", Access: "gated", DownloadNote: "gated: accept the Gemma licence on Hugging Face"}}
}
func (g *gardenModels) Download(id string, n int64) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.downloads = append(g.downloads, fmt.Sprintf("%s %d", id, n))
	g.state.State, g.state.Downloaded = "downloading", n/4
	return nil
}
func (g *gardenModels) Cancel(string) error { return nil }
func (g *gardenModels) Load(string) error   { return localai.ErrNotLoadable }
func (g *gardenModels) Unload(string) error { return nil }
func (g *gardenModels) Delete(string) error { return nil }

func agentPlatformConsole(t *testing.T) (*tab, *gardenModels) {
	t.Helper()
	g := &gardenModels{state: localai.ModelState{ID: "gemma-4-e2b-it-community", Repo: "litert-community/gemma-4-E2B-it-litert-lm",
		Pinned: true, Bytes: 4000, Revision: "0123456789abcdef", State: "absent", License: "gemma",
		Files: []localai.FileState{{Path: "gemma-4-E2B-it.litertlm"}}}}
	s := console.New("", func(context.Context) console.Status { return console.Status{} })
	s.SetLocalModels(g)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	p := openAt(t, strings.TrimPrefix(srv.URL, "http://"))
	// This console has no Resource Manager, so the project picker's read
	// is a 404, and Chrome's own background requests are sinkholed: neither
	// is the page under test. Dropped as each test ends.
	t.Cleanup(p.forgive)
	return p, g
}

// TestAgentPlatformNavigationAndRedirects (#1839): the old /ai/ address
// lands on Agent Platform's Model Garden, whose product navigation carries
// Google's pages; a page with no backend says it is not available locally,
// and the overview tells served pages from absent ones.
func TestAgentPlatformNavigationAndRedirects(t *testing.T) {
	p, _ := agentPlatformConsole(t)
	p.navigate("/ai/models")
	p.waitFor(`location.pathname === "/agent-platform/model-garden" && document.querySelector("#garden-cards article") !== null`)
	var nav struct {
		Title string
		Pages []string
	}
	p.eval(`({ title: document.querySelector("#product-nav .product-nav-title")?.textContent,
		pages: [...document.querySelectorAll("#product-nav a")].map((a) => a.textContent.trim()) })`, &nav)
	if nav.Title != "Agent Platform" {
		t.Errorf("the product is titled %q; want Agent Platform", nav.Title)
	}
	for _, want := range []string{"Overview", "Model Garden", "Agent Garden", "Deployments", "Studio", "Model Registry",
		"Tuning", "Online prediction", "Colab Enterprise", "Workbench", "Pipelines", "Vector Search"} {
		if !contains(nav.Pages, want) {
			t.Errorf("Agent Platform's pages %v lack %q", nav.Pages, want)
		}
	}

	p.navigate("/agent-platform/models")
	p.waitFor(`document.querySelector("#not-served") !== null`)
	var text string
	p.eval(`document.querySelector("#view").textContent`, &text)
	if !strings.Contains(text, "Model Registry") || !strings.Contains(text, "Not available locally") ||
		!strings.Contains(text, "projects.locations.models") {
		t.Errorf("Model Registry reads %q; want it named as not available locally, with its API", text)
	}

	p.navigate("/agent-platform/overview")
	p.waitFor(`document.querySelector("#ap-overview li") !== null`)
	var states map[string]string
	p.eval(`Object.fromEntries([...document.querySelectorAll("#ap-overview li")].map((li) =>
		[li.querySelector("a").textContent, li.dataset.state]))`, &states)
	if states["Model Garden"] != "served" || states["Local models"] != "served" || states["Pipelines"] != "absent" ||
		states["Studio"] != "off" {
		t.Errorf("the overview's page states are %v", states)
	}
}

// TestModelGardenCataloguesAndDeploysOnlyLocalModels (#1839): Model Garden
// lists Google's cards with its filters; Deploy is enabled only on the card
// that runs locally, where it confirms the exact size and downloads, then
// shows the download's progress bar; every other card has Deploy disabled
// with the reason.
func TestModelGardenCataloguesAndDeploysOnlyLocalModels(t *testing.T) {
	p, g := agentPlatformConsole(t)
	p.navigate("/agent-platform/model-garden")
	p.waitFor(`document.querySelector('#garden-cards article[data-model="gemma-4"]') !== null`)

	type card struct {
		ID       string
		Disabled bool
		Why      string
	}
	cards := func() []card {
		var out []card
		p.eval(`[...document.querySelectorAll("#garden-cards article")].map((a) => ({ id: a.dataset.model,
			disabled: a.querySelector('button[data-action="deploy"]').disabled,
			why: a.querySelector(".unavailable")?.textContent || "" }))`, &out)
		return out
	}
	all := cards()
	if len(all) < 15 {
		t.Fatalf("Model Garden shows %d cards", len(all))
	}
	for _, c := range all {
		switch {
		case c.ID == "gemma-4":
			if c.Disabled {
				t.Error("Gemma 4 runs locally and its Deploy is disabled")
			}
		case !c.Disabled:
			t.Errorf("%s has Deploy enabled with no local runtime", c.ID)
		case !strings.Contains(c.Why, "Not available locally"):
			t.Errorf("%s says %q; want why Deploy is disabled", c.ID, c.Why)
		}
	}

	p.clickText(".garden-filter label", "Open models on Hugging Face")
	hf := cards()
	if len(hf) == 0 || len(hf) >= len(all) {
		t.Fatalf("the Hugging Face collection shows %d of %d cards", len(hf), len(all))
	}
	for _, c := range hf {
		if !strings.HasPrefix(c.ID, "hf-") {
			t.Errorf("the Hugging Face collection shows %s", c.ID)
		}
	}
	p.clickText(".garden-filter label", "Open models on Hugging Face")
	p.setField("#garden-search", "gemma 4")
	p.waitFor(`document.querySelectorAll("#garden-cards article").length === 1`)

	p.clickText(`#garden-cards article[data-model="gemma-4"] button`, "Deploy")
	p.confirmByName("gemma-4-e2b-it-community")
	p.waitFor(`document.querySelector('#garden-cards article[data-model="gemma-4"] progress') !== null`)
	var progress string
	p.eval(`document.querySelector('#garden-cards article[data-model="gemma-4"] .garden-status').textContent`, &progress)
	if !strings.Contains(progress, "Downloading 25%") {
		t.Errorf("the card's progress reads %q", progress)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.downloads) != 1 || g.downloads[0] != "gemma-4-e2b-it-community 4000" {
		t.Errorf("downloads %v; want one with the confirmed size", g.downloads)
	}
}

// TestAgentGardenOffersOnlyRealActions (#1839): each Agent Garden sample
// links to its public source, and Deploy is disabled with the reason.
func TestAgentGardenOffersOnlyRealActions(t *testing.T) {
	p, _ := agentPlatformConsole(t)
	p.navigate("/agent-platform/agent-garden")
	p.waitFor(`document.querySelector("#agent-cards article") !== null`)
	var agents []struct {
		Code     string
		Disabled bool
		Why      string
	}
	p.eval(`[...document.querySelectorAll("#agent-cards article")].map((a) => ({
		code: [...a.querySelectorAll("a")].find((l) => l.textContent === "View code")?.href || "",
		disabled: a.querySelector('button[data-action="deploy"]').disabled,
		why: a.querySelector(".unavailable").textContent }))`, &agents)
	if len(agents) < 5 {
		t.Fatalf("Agent Garden shows %d samples", len(agents))
	}
	for _, a := range agents {
		if !strings.HasPrefix(a.Code, "https://github.com/") || !a.Disabled || !strings.Contains(a.Why, "Agent Runtime") {
			t.Errorf("a sample reads %+v; want its source and a disabled Deploy with the reason", a)
		}
	}
}
