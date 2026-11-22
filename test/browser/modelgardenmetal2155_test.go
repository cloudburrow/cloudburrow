//go:build browser

package browser

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/chromedp"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/hfmodels"
	"github.com/cloudburrow/cloudburrow/internal/hostai"
	"github.com/cloudburrow/cloudburrow/internal/modelgarden"
	"github.com/cloudburrow/cloudburrow/internal/service/vertexai"
)

func (c *browserCluster) PublishHost(context.Context, string, int) error { return nil }

// browserHost stands in for a Mac with Apple silicon (#2155): Start serves
// llama.cpp's routes on the spec's health port.
type browserHost struct {
	mu    sync.Mutex
	metal bool
	srvs  []*httptest.Server
}

func (h *browserHost) Metal() (bool, string) {
	if h.metal {
		return true, ""
	}
	return false, "Metal is Apple's GPU API, on macOS only; this machine runs linux"
}
func (h *browserHost) Device() string { return "Apple M9 Max" }
func (h *browserHost) LlamaServer(context.Context, func(string)) (string, error) {
	return "/cache/llama-server", nil
}
func (h *browserHost) MLXPython(context.Context, func(string)) (string, error) {
	return "/cache/python", nil
}
func (h *browserHost) Reap() []string { return nil }
func (h *browserHost) Start(spec hostai.Spec) (modelgarden.HostProcess, error) {
	u, _ := url.Parse(spec.Health)
	l, err := net.Listen("tcp", u.Host)
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(http.ResponseWriter, *http.Request) {})
	mux.HandleFunc("POST /v1/completions", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"choices":[{"text":" Paris."}]}`) })
	s := &httptest.Server{Listener: l, Config: &http.Server{Handler: mux}}
	s.Start()
	h.mu.Lock()
	h.srvs = append(h.srvs, s)
	h.mu.Unlock()
	return &browserProc{s: s}, nil
}

type browserProc struct {
	once sync.Once
	s    *httptest.Server
	done bool
}

func (p *browserProc) Wait(context.Context, time.Duration) error { return nil }
func (p *browserProc) Healthy() bool                             { return !p.done }
func (p *browserProc) Running() bool                             { return !p.done }
func (p *browserProc) Tail(int) string                           { return "" }
func (p *browserProc) Stop(context.Context) error {
	p.once.Do(func() { p.done = true; p.s.Close() })
	return nil
}

// metalHub serves a GGUF model's card and its file, and a sentence
// transformer's card.
func metalHub(t *testing.T) *httptest.Server {
	t.Helper()
	const rev = "89abcdef0123456789abcdef0123456789abcdef"
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/models/acme/quantized", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"id":"acme/quantized","author":"acme","sha":%q,"gated":false,"downloads":10,"likes":1,
			"pipeline_tag":"text-generation","library_name":"gguf","tags":["gguf"],"cardData":{"license":"mit"},
			"siblings":[{"rfilename":"README.md","size":10},{"rfilename":"q-Q4_K_M.gguf","size":4}]}`, rev)
	})
	mux.HandleFunc("GET /api/models/acme/mini", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"id":"acme/mini","author":"acme","sha":%q,"gated":false,"downloads":10,"likes":1,
			"pipeline_tag":"sentence-similarity","library_name":"sentence-transformers","tags":["sentence-transformers","safetensors"],
			"cardData":{"license":"apache-2.0"},"siblings":[{"rfilename":"README.md","size":10},{"rfilename":"model.safetensors","size":100}]}`, rev)
	})
	mux.HandleFunc("GET /acme/quantized/resolve/"+rev+"/q-Q4_K_M.gguf", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "GGUF") })
	mux.HandleFunc("GET /acme/quantized/resolve/"+rev+"/README.md", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "# q\n") })
	mux.HandleFunc("GET /acme/mini/resolve/"+rev+"/README.md", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "# mini\n") })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func metalConsole(t *testing.T, metal bool) (*tab, *browserHost) {
	t.Helper()
	hub := metalHub(t)
	dir := t.TempDir()
	hf := &hfmodels.Manager{Dir: filepath.Join(dir, "hf"), Base: hub.URL, Token: func() string { return "" },
		FreeBytes: func(string) (int64, error) { return 1 << 50, nil }}
	host := &browserHost{metal: metal}
	garden := &modelgarden.Manager{Instance: "b", StateFile: filepath.Join(dir, "deployments.json"), Models: hf,
		Docker: &engineDocker{}, Cluster: &browserCluster{}, Host: host}
	t.Cleanup(garden.Close)
	ai := vertexai.NewServer(nil)
	ai.SetModelGarden(garden)
	s := console.New("", func(context.Context) console.Status { return console.Status{} })
	s.SetHFModels(hf)
	s.SetModelGarden(garden, ai, "browser-project")
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	p := openAt(t, strings.TrimPrefix(srv.URL, "http://"))
	t.Cleanup(p.forgive)
	return p, host
}

func openDeployForm(p *tab, model string) {
	p.navigate("/agent-platform/model-garden?view=hugging-face&model=" + model)
	p.waitFor(`document.querySelector('[data-action="hf-deploy"]') !== null`)
	p.clickText(`#hf-card button`, "Deploy")
	p.waitFor(`!document.querySelector("#hf-deploy-form").hidden && document.querySelector("#hf-accelerator") !== null`)
}

type accelForm struct {
	Label, Value, Machine, Count, Note, Options string
	MetalDisabled                               bool
}

func readAccelForm(p *tab) accelForm {
	var f accelForm
	p.eval(`(() => { const s = document.querySelector("#hf-accelerator");
		return { Label: document.querySelector('label[for="hf-accelerator"]').textContent, Value: s.value,
			Machine: document.querySelector("#hf-machine-type").textContent, Count: document.querySelector("#hf-accelerator-count").textContent,
			Note: document.querySelector("#hf-accelerator-note").textContent,
			Options: [...s.options].map((o) => o.textContent).join(" | "), MetalDisabled: s.options[0].disabled }; })()`, &f)
	return f
}

// TestModelGardenDeploysOnTheAppleGPU (#2155): on a Mac with Apple
// silicon the deploy form's Accelerator type offers "Apple GPU (Metal),
// local", chosen by default, as NVIDIA_L4 × 1 on machine type local-metal;
// Deploy runs the GGUF model on the host, and the Endpoints page and the
// deployed model's details show the GPU. Choosing None (CPU) keeps the
// cluster; a runtime with no Metal build says it runs on CPU.
func TestModelGardenDeploysOnTheAppleGPU(t *testing.T) {
	p, host := metalConsole(t, true)
	openDeployForm(p, "acme/quantized")
	f := readAccelForm(p)
	if f.Label != "Accelerator type" || f.Value != "NVIDIA_L4" || f.Machine != "local-metal" || f.Count != "1" || f.MetalDisabled ||
		!strings.Contains(f.Options, "Apple GPU (Metal), local · Apple M9 Max (as NVIDIA_L4)") || !strings.Contains(f.Options, "None (CPU)") ||
		!strings.Contains(f.Note, "this Mac's GPU") {
		t.Errorf("the accelerator field = %+v", f)
	}
	// None (CPU) changes the machine type and count.
	p.setField("#hf-accelerator", "none")
	p.waitFor(`document.querySelector("#hf-machine-type").textContent === "local-cpu"`)
	if f := readAccelForm(p); f.Count != "0" || !strings.Contains(f.Note, "CPU in the instance's cluster") {
		t.Errorf("with None (CPU): %+v", f)
	}
	p.setField("#hf-accelerator", "NVIDIA_L4")
	p.clickText(`#hf-deploy-form button`, "Deploy")
	p.waitFor(`location.pathname === "/agent-platform/endpoints" && document.querySelector("#hf-endpoint") !== null`)
	p.waitForWithin(`document.querySelector("#hf-endpoint-state") && document.querySelector("#hf-endpoint-state").textContent === "Ready"`, time.Minute)
	var ep struct{ Runtime, Machine, Accel, Count, Row, RunsOn string }
	p.eval(`({ Runtime: document.querySelector("#hf-endpoint-runtime").textContent, Machine: document.querySelector("#hf-endpoint-machine").textContent,
		Accel: document.querySelector("#hf-endpoint-accelerator").textContent, Count: document.querySelector("#hf-endpoint-accelerator-count").textContent,
		Row: document.querySelector('#hf-endpoints td[data-accelerator]').textContent, RunsOn: document.querySelector('#hf-endpoints td[data-runtime]').textContent })`, &ep)
	if !strings.Contains(ep.Runtime, "This Mac, on the Apple GPU") || ep.Machine != "local-metal" ||
		ep.Accel != "Apple GPU (Metal), local (Apple M9 Max GPU (Metal)), as NVIDIA_L4" || ep.Count != "1" ||
		!strings.Contains(ep.Row, "Apple GPU (Metal), local") || ep.RunsOn != "This Mac (Apple GPU)" {
		t.Errorf("the endpoint = %+v", ep)
	}
	host.mu.Lock()
	started := len(host.srvs)
	host.mu.Unlock()
	if started != 1 {
		t.Errorf("%d host servers started; want 1", started)
	}
	p.clickText("#hf-test button", "Predict")
	p.waitFor(`document.querySelector("#hf-test-output").textContent.includes("Paris")`)

	t.Run("WCAG", func(t *testing.T) {
		openDeployForm(p, "acme/quantized")
		for _, u := range p.unnamedControls() {
			t.Errorf("the deploy form: an unnamed %s", u)
		}
		for _, c := range p.contrastFailures() {
			t.Errorf("the deploy form: %s", c)
		}
		p.navigate("/agent-platform/endpoints")
		p.waitFor(`document.querySelector("#hf-endpoints") !== null`)
		for _, c := range p.contrastFailures() {
			t.Errorf("the Endpoints page: %s", c)
		}
	})

	t.Run("keyboard", func(t *testing.T) {
		openDeployForm(p, "acme/quantized")
		p.tabTo(`a.id === "hf-accelerator"`)
		// The select changes by typing an option's first letter, and the
		// form follows it.
		p.run(chromedp.KeyEvent("n"))
		p.waitFor(`document.querySelector("#hf-accelerator").value === "none" && document.querySelector("#hf-machine-type").textContent === "local-cpu"`)
		// A pause past the browser's type-ahead window, so "a" starts a new
		// search rather than continuing "n".
		p.run(chromedp.Sleep(1500*time.Millisecond), chromedp.KeyEvent("a"))
		p.waitFor(`document.querySelector("#hf-accelerator").value === "NVIDIA_L4" && document.querySelector("#hf-machine-type").textContent === "local-metal"`)
	})

	// A runtime with no Metal build: the form says it runs on CPU.
	openDeployForm(p, "acme/mini")
	if f := readAccelForm(p); f.Machine != "local-cpu" || !strings.Contains(f.Note, "no Metal build") {
		t.Errorf("TEI's accelerator field = %+v", f)
	}
}

// TestModelGardenAcceleratorWithoutMetal (#2155): on a machine with no
// Apple GPU, the choice is disabled with the reason and None (CPU) is
// chosen.
func TestModelGardenAcceleratorWithoutMetal(t *testing.T) {
	p, _ := metalConsole(t, false)
	openDeployForm(p, "acme/quantized")
	f := readAccelForm(p)
	if f.Value != "none" || !f.MetalDisabled || f.Machine != "local-cpu" || !strings.Contains(f.Note, "The Apple GPU is not available: Metal is Apple's GPU API") {
		t.Errorf("without Metal: %+v", f)
	}
}
