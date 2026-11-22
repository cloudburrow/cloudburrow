//go:build browser

package browser

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/hfmodels"
	"github.com/cloudburrow/cloudburrow/internal/modelgarden"
	"github.com/cloudburrow/cloudburrow/internal/service/vertexai"
)

// fakeHFToken is the only token the stub hub accepts. No real token is
// ever used by a test.
const fakeHFToken = "hf_browserFakeToken0000000000000000"

// stubHub is a local stand-in for huggingface.co's public API: a search
// with two pages, two model cards (one gated) with READMEs, and the gated
// model's config.json, which only fakeHFToken opens.
type stubHub struct {
	mu      sync.Mutex
	queries []url.Values
	srv     *httptest.Server
}

func (h *stubHub) lastQuery() url.Values {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.queries) == 0 {
		return url.Values{}
	}
	return h.queries[len(h.queries)-1]
}

func newStubHub(t *testing.T) *stubHub {
	t.Helper()
	h := &stubHub{}
	const rev = "0123456789abcdef0123456789abcdef01234567"
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/models", func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.queries = append(h.queries, r.URL.Query())
		h.mu.Unlock()
		if r.URL.Query().Get("cursor") == "" {
			w.Header().Set("Link", fmt.Sprintf(`<http://%s/api/models?cursor=page2>; rel="next"`, r.Host))
			fmt.Fprint(w, `[{"id":"acme/mini","author":"acme","gated":false,"downloads":1234567,"likes":89,"pipeline_tag":"sentence-similarity",
				"library_name":"sentence-transformers","tags":["sentence-transformers","safetensors","license:apache-2.0"]},
				{"id":"acme/gated-llm","author":"acme","gated":"manual","downloads":4200,"likes":7,"pipeline_tag":"text-generation",
				"library_name":"transformers","tags":["transformers","safetensors","license:llama3.1"]}]`)
			return
		}
		fmt.Fprint(w, `[{"id":"acme/quantized","author":"acme","gated":false,"downloads":10,"likes":1,"pipeline_tag":"text-generation",
			"library_name":"gguf","tags":["gguf","license:mit"]}]`)
	})
	mux.HandleFunc("GET /api/models/acme/mini", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"id":"acme/mini","author":"acme","sha":%q,"gated":false,"downloads":1234567,"likes":89,
			"pipeline_tag":"sentence-similarity","library_name":"sentence-transformers","tags":["sentence-transformers","safetensors"],
			"cardData":{"license":"apache-2.0"},"siblings":[{"rfilename":"README.md","size":100},{"rfilename":"config.json","size":20},
			{"rfilename":"model.safetensors","size":69569488},{"rfilename":"onnx/model.onnx","size":69000000}]}`, rev)
	})
	mux.HandleFunc("GET /api/models/acme/gated-llm", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"id":"acme/gated-llm","author":"acme","sha":%q,"gated":"manual","downloads":4200,"likes":7,
			"pipeline_tag":"text-generation","library_name":"transformers","tags":["transformers","safetensors"],
			"cardData":{"license":"llama3.1","extra_gated_prompt":"Agree to the <b>community</b> license."},
			"siblings":[{"rfilename":"README.md","size":10},{"rfilename":"config.json","size":20},{"rfilename":"model.safetensors","size":5000}]}`, rev)
	})
	mux.HandleFunc("GET /acme/mini/resolve/"+rev+"/README.md", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "---\nlicense: apache-2.0\n---\n# acme/mini\n\nMaps sentences to a **384** dimensional space.\n\n"+
			"<script>window.__pwned = true</script><img src=x onerror=\"window.__pwned = true\">\n\n"+
			"| Metric | Value |\n|---|---|\n| STS | 0.81 |\n\n- [Docs](https://www.sbert.net) and [evil](javascript:window.__pwned=true)\n\n```python\nmodel.encode(x)\n```\n")
	})
	mux.HandleFunc("GET /acme/gated-llm/resolve/"+rev+"/README.md", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "# Gated LLM\n")
	})
	mux.HandleFunc("/acme/gated-llm/resolve/"+rev+"/config.json", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+fakeHFToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		fmt.Fprint(w, `{"max_position_embeddings": 2048}`)
	})
	h.srv = httptest.NewServer(mux)
	t.Cleanup(h.srv.Close)
	return h
}

// browserCluster stands in for the instance's cluster: each workload is an
// httptest server answering TEI's and vLLM's routes, which reads as pulling,
// then loading, then ready.
type browserCluster struct {
	mu      sync.Mutex
	servers map[string]*httptest.Server
	tokens  map[string]string
	seen    map[string]int
}

func (c *browserCluster) Arch(context.Context) (string, error) { return "arm64", nil }
func (c *browserCluster) Apply(_ context.Context, w modelgarden.Workload, token string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(http.ResponseWriter, *http.Request) {})
	mux.HandleFunc("POST /embed", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `[[0.25,0.5,0.125]]`) })
	mux.HandleFunc("POST /v1/completions", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"choices":[{"text":" Paris."}]}`) })
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.servers == nil {
		c.servers, c.tokens, c.seen = map[string]*httptest.Server{}, map[string]string{}, map[string]int{}
	}
	c.servers[w.Name] = httptest.NewServer(mux)
	if token != "" {
		c.tokens[w.Name] = token
	}
	return nil
}
func (c *browserCluster) Observe(_ context.Context, name string) (modelgarden.Observation, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.servers[name]; !ok {
		return modelgarden.Observation{Phase: modelgarden.PhaseMissing}, nil
	}
	c.seen[name]++
	switch c.seen[name] {
	case 1, 2:
		return modelgarden.Observation{Phase: modelgarden.PhasePulling, Pulled: 40e6, PullTotal: 117e6}, nil
	case 3:
		return modelgarden.Observation{Phase: modelgarden.PhaseLoading, Message: "Downloading the weights from Hugging Face"}, nil
	}
	return modelgarden.Observation{Phase: modelgarden.PhaseReady}, nil
}
func (c *browserCluster) Forward(_ context.Context, name string) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.servers[name]
	var port int
	_, err := fmt.Sscanf(s.URL[strings.LastIndex(s.URL, ":")+1:], "%d", &port)
	return port, err
}
func (c *browserCluster) Unforward(string)                         {}
func (c *browserCluster) UnforwardAll()                            {}
func (c *browserCluster) Logs(context.Context, string, int) string { return "" }
func (c *browserCluster) Remove(_ context.Context, name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if s, ok := c.servers[name]; ok {
		s.Close()
		delete(c.servers, name)
	}
	delete(c.tokens, name)
	return nil
}
func (c *browserCluster) secrets() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, t := range c.tokens {
		out = append(out, t)
	}
	return out
}

func hubConsole(t *testing.T) (*tab, *stubHub, *browserCluster, string) {
	t.Helper()
	hub := newStubHub(t)
	dir := t.TempDir()
	hf := &hfmodels.Manager{Dir: filepath.Join(dir, "hf"), Base: hub.srv.URL, Token: func() string { return "" }}
	cluster := &browserCluster{}
	state := filepath.Join(dir, "deployments.json")
	garden := &modelgarden.Manager{Instance: "b", StateFile: state, Models: hf, Docker: &engineDocker{}, Cluster: cluster}
	ai := vertexai.NewServer(nil)
	ai.SetModelGarden(garden)
	s := console.New("", func(context.Context) console.Status { return console.Status{} })
	s.SetHFModels(hf)
	s.SetModelGarden(garden, ai, "browser-project")
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	p := openAt(t, strings.TrimPrefix(srv.URL, "http://"))
	t.Cleanup(p.forgive)
	return p, hub, cluster, state
}

// TestModelGardenBrowsesHuggingFace (#2148): Model Garden's Show more opens
// the Hugging Face tab, which searches the Hub with Google's chips and
// sorts, pages with Load more, marks gated models and says which ones
// deploy; a card shows its facts and a README with nothing of its HTML.
func TestModelGardenBrowsesHuggingFace(t *testing.T) {
	p, hub, _, _ := hubConsole(t)
	p.navigate("/agent-platform/model-garden")
	p.clickText("#hf-hub button", "Show more")
	p.waitFor(`document.querySelectorAll("#hf-hub-cards [data-hf-hub-model]").length === 2`)
	var gated, mini string
	p.eval(`document.querySelector('[data-hf-hub-model="acme/gated-llm"]').dataset.gated + " " + document.querySelector('[data-hf-hub-model="acme/gated-llm"]').textContent`, &gated)
	if !strings.HasPrefix(gated, "true") || !strings.Contains(gated, "Gated") || !strings.Contains(gated, "llama3.1") || !strings.Contains(gated, "Deploys on vLLM") {
		t.Errorf("the gated card reads %q", gated)
	}
	p.eval(`document.querySelector('[data-hf-hub-model="acme/mini"]').textContent`, &mini)
	if !strings.Contains(mini, "1.2M downloads") || !strings.Contains(mini, "Deploys on Text Embeddings Inference") || !strings.Contains(mini, "Sentence similarity") {
		t.Errorf("the public card reads %q", mini)
	}
	if q := hub.lastQuery(); q.Get("sort") != "trendingScore" || q.Get("direction") != "-1" {
		t.Errorf("the first search = %v; want trending, descending", q)
	}

	// The chips and the sort are the Hub's filters.
	p.clickText(`[data-chips="task"] button`, "Sentence similarity")
	p.waitFor(`location.search.includes("task=sentence-similarity")`)
	p.clickText(`[data-chips="library"] button`, "sentence-transformers")
	p.clickText(`[data-chips="license"] button`, "Apache 2.0")
	p.setField("#hf-hub-sort", "downloads")
	p.setField("#hf-hub-search", "mini")
	p.waitFor(`location.search.includes("q=mini") && location.search.includes("sort=downloads")`)
	p.waitFor(`document.querySelectorAll("#hf-hub-cards [data-hf-hub-model]").length === 2 && !document.querySelector("#hf-hub-more").hidden`)
	q := hub.lastQuery()
	for k, want := range map[string]string{"search": "mini", "pipeline_tag": "sentence-similarity", "library": "sentence-transformers",
		"filter": "license:apache-2.0", "sort": "downloads"} {
		if q.Get(k) != want {
			t.Errorf("search %s = %q, want %q", k, q.Get(k), want)
		}
	}
	var pressed string
	p.eval(`document.querySelector('[data-chips="task"] [data-chip="sentence-similarity"]').getAttribute("aria-pressed")`, &pressed)
	if pressed != "true" {
		t.Errorf("the chosen chip is aria-pressed=%q", pressed)
	}

	// Load more fetches the next page with the Hub's cursor; a GGUF-only
	// model says why it cannot deploy.
	p.clickText("#hf-hub-more", "Load more")
	p.waitFor(`document.querySelectorAll("#hf-hub-cards [data-hf-hub-model]").length === 3`)
	if c := hub.lastQuery().Get("cursor"); c != "page2" {
		t.Errorf("Load more sent cursor %q", c)
	}
	var quantized string
	p.eval(`document.querySelector('[data-hf-hub-model="acme/quantized"]').textContent`, &quantized)
	if !strings.Contains(quantized, "Deploy unavailable") || !strings.Contains(quantized, "gguf") {
		t.Errorf("the GGUF card reads %q", quantized)
	}

	// The card.
	p.clickText(`[data-hf-hub-model="acme/mini"] h2 a`, "acme/mini")
	p.waitFor(`document.querySelector("#hf-card-facts") !== null && document.querySelector("#hf-readme") !== null`)
	var facts string
	p.eval(`document.querySelector("#hf-card-facts").textContent`, &facts)
	for _, want := range []string{"Sentence similarity", "sentence-transformers", "apache-2.0", "Public", "1,234,567", "66 MiB of weights", "Text Embeddings Inference"} {
		if !strings.Contains(facts, want) {
			t.Errorf("the card's facts lack %q: %q", want, facts)
		}
	}
	var readme struct {
		H, Strong, Cell, Code, Links string
		Scripts, Images              int
		Pwned                        bool
	}
	p.eval(`(() => { const r = document.querySelector("#hf-readme");
		return { H: r.querySelector("h2").textContent, Strong: r.querySelector("strong").textContent,
			Cell: r.querySelector("td:last-child").textContent, Code: r.querySelector("pre code").textContent,
			Links: [...r.querySelectorAll("a")].map((a) => a.getAttribute("href")).join(" "),
			Scripts: r.querySelectorAll("script").length, Images: r.querySelectorAll("img").length, Pwned: !!window.__pwned }; })()`, &readme)
	if readme.H != "acme/mini" || readme.Strong != "384" || readme.Cell != "0.81" || readme.Code != "model.encode(x)" {
		t.Errorf("the README rendered %+v", readme)
	}
	if readme.Scripts != 0 || readme.Images != 0 || readme.Pwned || strings.Contains(readme.Links, "javascript:") || !strings.Contains(readme.Links, "https://www.sbert.net") {
		t.Errorf("the README is not sanitised: %+v", readme)
	}
}

// TestModelGardenPageDeploysAHubModelOnTheCluster (#2148): Deploy on a public
// model's card runs ModelGardenService.Deploy on the instance's cluster and
// opens its endpoint, which shows the image pull's progress and then
// Ready, on the cluster; its test panel embeds. A gated model's form asks
// for the licence and the end user's token: a token the Hub refuses is
// Google's error, and the field is emptied; the right (fake) token deploys,
// reaching the workload's Secret and nothing else.
func TestModelGardenPageDeploysAHubModelOnTheCluster(t *testing.T) {
	p, _, cluster, state := hubConsole(t)
	p.navigate("/agent-platform/model-garden?view=hugging-face&model=acme/mini")
	p.waitFor(`document.querySelector('[data-action="hf-deploy"]') !== null`)
	p.clickText(`#hf-card button`, "Deploy")
	p.waitFor(`!document.querySelector("#hf-deploy-form").hidden`)
	var hasToken bool
	p.eval(`document.querySelector("#hf-token") !== null`, &hasToken)
	if hasToken {
		t.Error("a public model's deploy form asks for a token")
	}
	p.clickText(`#hf-deploy-form button`, "Deploy")
	p.waitFor(`location.pathname === "/agent-platform/endpoints" && document.querySelector("#hf-endpoint") !== null`)
	p.waitForWithin(`document.querySelector("#hf-endpoint-state") && document.querySelector("#hf-endpoint-state").textContent === "Ready"`, time.Minute)
	var runtime string
	p.eval(`document.querySelector("#hf-endpoint-runtime").textContent + " | " + document.querySelector('#hf-endpoints td[data-runtime]').textContent`, &runtime)
	if !strings.Contains(runtime, "Kubernetes cluster") || !strings.Contains(runtime, "Instance cluster") {
		t.Errorf("the endpoint runs on %q", runtime)
	}
	p.clickText("#hf-test button", "Predict")
	p.waitFor(`document.querySelector("#hf-test-output").textContent.startsWith("3-dimensional embedding")`)

	// The gated model.
	p.navigate("/agent-platform/model-garden?view=hugging-face&model=acme/gated-llm")
	p.waitFor(`document.querySelector("#hf-card-access") !== null`)
	var access string
	p.eval(`document.querySelector("#hf-card-access").textContent`, &access)
	if !strings.Contains(access, "Gated") {
		t.Errorf("the gated card's access reads %q", access)
	}
	p.clickText(`#hf-card button`, "Deploy")
	p.waitFor(`!document.querySelector("#hf-deploy-form").hidden && document.querySelector("#hf-token") !== null`)
	var form struct {
		Type, Label, Note string
	}
	p.eval(`({ Type: document.querySelector("#hf-token").type, Label: document.querySelector('label[for="hf-token"]').textContent,
		Note: document.querySelector("#hf-gated-note").textContent })`, &form)
	if form.Type != "password" || form.Label != "Hugging Face access token" || !strings.Contains(form.Note, "Accept its license on huggingface.co") ||
		strings.Contains(form.Note, "<b>") {
		t.Errorf("the gated form = %+v", form)
	}
	p.setField("#hf-accept-eula", "true")
	p.setField("#hf-token", "hf_refusedByTheStubHub0000")
	p.clickText(`#hf-deploy-form button`, "Deploy")
	p.waitFor(`document.querySelector("#hf-deploy-message").textContent.includes("The token is not valid or does not have permission")`)
	var after struct{ Value, Message string }
	p.eval(`({ Value: document.querySelector("#hf-token").value, Message: document.querySelector("#hf-deploy-message").textContent })`, &after)
	if after.Value != "" || strings.Contains(after.Message, "hf_refusedByTheStubHub0000") {
		t.Errorf("after a refused token: %+v; want the field emptied and no token in the message", after)
	}
	p.setField("#hf-token", fakeHFToken)
	p.clickText(`#hf-deploy-form button`, "Deploy")
	p.waitFor(`location.pathname === "/agent-platform/endpoints" && document.querySelector("#hf-endpoint") !== null`)
	p.waitForWithin(`document.querySelector("#hf-endpoint-state") && document.querySelector("#hf-endpoint-state").textContent === "Ready"`, time.Minute)
	p.eval(`document.querySelector("#hf-endpoint-runtime").textContent`, &runtime)
	if !strings.Contains(runtime, "-hf-token") {
		t.Errorf("the gated endpoint's runtime reads %q; want its token Secret named", runtime)
	}
	if got := cluster.secrets(); len(got) != 1 || got[0] != fakeHFToken {
		t.Errorf("the cluster's Secrets hold %d tokens", len(got))
	}
	var page string
	p.eval(`document.documentElement.outerHTML`, &page)
	if strings.Contains(page, fakeHFToken) {
		t.Error("the page shows the token")
	}
	b, _ := os.ReadFile(state)
	if strings.Contains(string(b), fakeHFToken) {
		t.Error("the deployments file holds the token")
	}
}
