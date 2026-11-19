//go:build browser

package browser

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/hfmodels"
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

// TestModelGardenDownloadsAHuggingFaceModel (#1840): Model Garden's
// Hugging Face section reads a model's licence, access and size, refuses a
// download that would leave too little disk free, downloads one that fits
// with its progress, and deletes it. The hub is a local stand-in serving the
// two routes huggingface.co answers.
func TestModelGardenDownloadsAHuggingFaceModel(t *testing.T) {
	const rev = "0123456789abcdef0123456789abcdef01234567"
	weights := []byte("weights!")
	sum := sha256.Sum256(weights)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/models/org/tiny", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"id":"org/tiny","sha":%q,"gated":false,"pipeline_tag":"text-generation","cardData":{"license":"apache-2.0"},
			"siblings":[{"rfilename":"model.safetensors","size":8,"lfs":{"sha256":%q,"size":8}}]}`, rev, hex.EncodeToString(sum[:]))
	})
	mux.HandleFunc("GET /org/tiny/resolve/"+rev+"/model.safetensors", func(w http.ResponseWriter, r *http.Request) { w.Write(weights) })
	hub := httptest.NewServer(mux)
	t.Cleanup(hub.Close)

	var free atomic.Int64
	free.Store(50<<30 + 4) // too little: 8 bytes would leave under 50 GB
	hf := &hfmodels.Manager{Dir: t.TempDir(), Base: hub.URL, Token: func() string { return "" },
		FreeBytes: func(string) (int64, error) { return free.Load(), nil }}
	s := console.New("", func(context.Context) console.Status { return console.Status{} })
	s.SetHFModels(hf)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	p := openAt(t, strings.TrimPrefix(srv.URL, "http://"))
	t.Cleanup(p.forgive)

	p.navigate("/agent-platform/model-garden")
	p.waitFor(`document.querySelector("#hf-model-id") !== null`)
	check := func() {
		p.setField("#hf-model-id", "org/tiny")
		p.clickText("#hf-panel button", "Check model")
		p.waitFor(`document.querySelector("#hf-facts") !== null`)
	}
	check()
	var facts string
	p.eval(`document.querySelector("#hf-facts").textContent`, &facts)
	if !strings.Contains(facts, "apache-2.0") || !strings.Contains(facts, "Public") || !strings.Contains(facts, "text-generation") {
		t.Errorf("the model's facts read %q", facts)
	}
	var disabled bool
	p.eval(`document.querySelector('#hf-lookup button[data-action="download"]').disabled`, &disabled)
	if !disabled {
		t.Error("Download is enabled for a model that would leave under 50 GB free")
	}

	free.Store(60 << 30)
	check()
	p.clickText("#hf-lookup button", "Download")
	p.confirmByName("org/tiny")
	p.waitFor(`document.querySelector('#hf-models tr[data-hf-model="org/tiny"] td[data-state="downloaded"]') !== null`)
	if _, err := os.Stat(filepath.Join(hf.Dir, "org--tiny", "model.safetensors")); err != nil {
		t.Fatalf("the download is not on disk: %v", err)
	}
	p.clickText(`#hf-models tr[data-hf-model="org/tiny"] button`, "Delete")
	p.confirmByName("org/tiny")
	p.waitFor(`document.querySelector('#hf-models') === null`)
	if _, err := os.Stat(filepath.Join(hf.Dir, "org--tiny")); err == nil {
		t.Error("Delete left the model on disk")
	}
}
