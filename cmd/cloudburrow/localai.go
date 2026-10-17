package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	runadapter "github.com/cloudburrow/cloudburrow/internal/adapter/run"
	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/embedding"
	"github.com/cloudburrow/cloudburrow/internal/localai"
	"github.com/cloudburrow/cloudburrow/internal/service/vertexai"
)

// buildLocalAI returns the local generation endpoint, or nil when no model is
// configured.
//
// Absence is the default and is not an error. The runtime is a from-source
// build and the model a multi-gigabyte download; a developer using Cloud
// Storage should not be made to acquire either.
func buildLocalAI(cfg config.Config) (*vertexai.Server, error) {
	if strings.TrimSpace(cfg.LocalAI.ModelPath) == "" {
		if !cfg.LocalAI.Embeddings {
			return nil, nil
		}
		// Embeddings only: no generation runtime, so generateContent
		// answers FAILED_PRECONDITION as it does with no model.
		addr := net.JoinHostPort(cfg.BindAddress, strconv.Itoa(cfg.Endpoints.LocalAI))
		srv := vertexai.NewServerOn(addr, nil)
		attachEmbedder(srv, cfg)
		return srv, nil
	}
	modelPath, err := filepath.Abs(cfg.LocalAI.ModelPath)
	if err != nil {
		return nil, fmt.Errorf("resolve -local-ai-model: %w", err)
	}

	image := cfg.LocalAI.Image
	if image == "" {
		image = config.LocalAIImage()
	}
	if err := preflightLocalAI(modelPath, image); err != nil {
		return nil, err
	}
	modelID := cfg.LocalAI.ModelID
	if modelID == "" {
		modelID = catalogueIDFor(modelPath)
	}

	dir, file := filepath.Split(modelPath)
	gen := vertexai.NewDockerGenerator(image, filepath.Clean(dir), file, modelID)

	addr := net.JoinHostPort(cfg.BindAddress, strconv.Itoa(cfg.Endpoints.LocalAI))
	srv := vertexai.NewServerOn(addr, gen)
	for _, a := range cfg.LocalAI.Aliases {
		srv.Alias(a)
	}
	if cfg.LocalAI.Embeddings {
		attachEmbedder(srv, cfg)
	}
	return srv, nil
}

// withPredictionEndpoints makes srv serve custom prediction endpoints when
// Cloud Run is enabled, starting an endpoint with no model if srv is nil.
// Generation and embeddings on it answer as they do with nothing configured.
func withPredictionEndpoints(cfg config.Config, srv *vertexai.Server, run interface{ Addr() string }, ingress func() string) *vertexai.Server {
	enabled := false
	for _, s := range cfg.EnabledServices() {
		enabled = enabled || s == config.ServiceRun
	}
	if !enabled {
		return srv
	}
	if srv == nil {
		srv = vertexai.NewServerOn(net.JoinHostPort(cfg.BindAddress, strconv.Itoa(cfg.Endpoints.LocalAI)), nil)
	}
	srv.SetEndpoints(endpointResolver{src: predictionSource{run: runProvider{
		kubeconfig:     cfg.KubeconfigPath(),
		namespace:      runadapter.WorkloadNamespace,
		runAddr:        runAddrOf(run),
		defaultProject: cfg.DefaultProject(),
	}}}, ingress, nil)
	return srv
}

// embeddingCache is where pinned embedding models are kept.
func embeddingCache(cfg config.Config) localai.Cache {
	return localai.Cache{Dir: filepath.Join(cfg.StateDir, "models")}
}

// attachEmbedder installs the community EmbeddingGemma ONNX runtime, or the
// reason it is unavailable, which every :predict then returns as a 501.
//
// The pinned files are fetched over HTTPS at the pinned revision and verified
// by SHA-256 only when the runtime is compiled in: downloading 330 MB for a
// binary that cannot run it would be waste.
func attachEmbedder(srv *vertexai.Server, cfg config.Config) {
	m, err := localai.Lookup(localai.EmbeddingGemmaONNXID)
	if err != nil {
		srv.SetEmbedder(nil, err)
		return
	}
	if ok, why := m.Runnable(); !ok {
		srv.SetEmbedder(nil, fmt.Errorf("%s", why))
		return
	}
	cache := embeddingCache(cfg)
	if cache.VerifyPinned(m) != nil {
		fmt.Fprintf(os.Stderr, "fetching %s (%s @ %s, a community conversion, NOT an official Google artifact)...\n",
			m.ID, m.Repo, m.Pin.Revision[:12])
		if err := cache.FetchPinned(context.Background(), m, nil); err != nil {
			srv.SetEmbedder(nil, err)
			return
		}
	}
	e, err := embedding.Open(cache.PinnedDir(m), m.ID)
	srv.SetEmbedder(e, err)
}

// catalogueIDFor names the model from its artifact filename when the catalogue
// recognises it.
//
// Falling back to the bare filename is deliberate: an unrecognised artifact is
// still served, and is reported under a name that describes the file rather
// than under a catalogued ID it may not be.
func catalogueIDFor(modelPath string) string {
	base := filepath.Base(modelPath)
	for _, m := range localai.Models() {
		if m.Artifact == base {
			return m.ID
		}
	}
	return strings.TrimSuffix(base, filepath.Ext(base))
}

// printLocalAI reports the endpoint, and what the model actually is.
//
// The provenance line is not decoration. The only ungated model that runs
// today is a community conversion, and a developer who reads "gemma" on their
// screen should not have to look it up to learn it is not Google's.
func printLocalAI(w io.Writer, srv *vertexai.Server, cfg config.Config) {
	if srv == nil {
		return
	}
	fmt.Fprintf(w, "\nlocal AI:  http://%s\n", srv.Addr())
	if srv.Model() != "" {
		fmt.Fprintf(w, "  model:   %s\n", srv.Model())
	}

	if m, err := localai.Lookup(srv.Model()); err == nil && m.Publisher == localai.PublisherCommunity {
		fmt.Fprintf(w, "  note:    a COMMUNITY conversion, not published by Google\n")
	}
	if len(cfg.LocalAI.Aliases) > 0 {
		fmt.Fprintf(w, "  aliases: %s (explicitly configured substitutions)\n",
			strings.Join(cfg.LocalAI.Aliases, ", "))
	}
	if cfg.LocalAI.Embeddings {
		fmt.Fprintf(w, "  embeddings: POST /v1/projects/{p}/locations/{l}/publishers/google/models/%s:predict\n", localai.EmbeddingGemmaONNXID)
		fmt.Fprintf(w, "              a COMMUNITY ONNX int8 conversion, NOT an official Google artifact\n")
	}
	if srv.ServesEndpoints() {
		fmt.Fprintf(w, "  custom prediction: PredictionService (gRPC) and POST /v1/projects/{p}/locations/{l}/endpoints/{service}:predict\n")
		fmt.Fprintf(w, "              {service} is a deployed Cloud Run prediction container; see docs/prediction.md\n")
	}
	fmt.Fprintf(w, "  generation options are refused rather than ignored; see docs/generation.md\n")
}

// dockerRun runs a docker command; replaced in tests.
var dockerRun = func(args ...string) (string, error) {
	out, err := exec.Command("docker", args...).CombinedOutput()
	return string(out), err
}

// preflightLocalAI fails `up` at startup when the model or the runtime is
// missing (#602). Before, both were first touched by the first
// generateContent, which failed with a docker error far from the flag that
// caused it.
//
// The default image is the tag `make litert-lm` builds; it is published
// nowhere, so it is never pulled, and its absence says how to build it. A
// different image is pulled once before giving up.
func preflightLocalAI(modelPath, image string) error {
	info, err := os.Stat(modelPath)
	if err != nil {
		return fmt.Errorf("-local-ai-model %s: %w", modelPath, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("-local-ai-model %s is not a file", modelPath)
	}
	if _, err := dockerRun("image", "inspect", image); err == nil {
		return nil
	}
	if image == config.DefaultLocalAIImage {
		return fmt.Errorf("the local AI runtime image %s is not built: run `make litert-lm` in a CloudBurrow checkout, "+
			"or pass -local-ai-image with an image you have (no runtime image is published yet)", image)
	}
	if out, err := dockerRun("pull", image); err != nil {
		return fmt.Errorf("the local AI runtime image %s is not present and could not be pulled: %v\n%s", image, err, strings.TrimSpace(out))
	}
	return nil
}

// embedRuntime loads and unloads models into the running local AI endpoint
// for the console's Local models page (#1306). Only the embedding runtime
// can: the generation model is the container -local-ai-model started, which
// has no unload, so it is reported as loaded and offered nothing.
type embedRuntime struct{ srv *vertexai.Server }

func (r embedRuntime) Loads(m localai.Model) (bool, string) {
	if m.Runtime != localai.RuntimeONNX {
		if m.ID == r.srv.Model() {
			return false, "served by the LiteRT-LM container -local-ai-model started, which has no load or unload; restart without it to stop serving it"
		}
		return false, "only the embedding runtime loads and unloads models; a generation model is served with up -local-ai-model"
	}
	return m.Runnable()
}

func (r embedRuntime) Load(m localai.Model, dir string) error {
	if m.Modality == localai.ModalityMultimodalEmbedding {
		e, err := embedding.OpenCLIP(dir, m.ID)
		if err != nil {
			return err
		}
		if old := r.srv.SwapMultimodalEmbedder(e, nil); old != nil {
			_ = old.Close()
		}
		return nil
	}
	e, err := embedding.Open(dir, m.ID)
	if err != nil {
		return err
	}
	if old := r.srv.SwapEmbedder(e, nil); old != nil {
		_ = old.Close()
	}
	return nil
}

func (r embedRuntime) Unload(m localai.Model) error {
	if m.Modality == localai.ModalityMultimodalEmbedding {
		old := r.srv.SwapMultimodalEmbedder(nil, fmt.Errorf("%s was unloaded on the console's Local models page; load it there to serve it again", m.ID))
		if old != nil {
			return old.Close()
		}
		return nil
	}
	old := r.srv.SwapEmbedder(nil, fmt.Errorf("%s was unloaded on the console's Local models page; load it there to serve it again", m.ID))
	if old != nil {
		return old.Close()
	}
	return nil
}

func (r embedRuntime) Loaded(m localai.Model) bool {
	if m.Modality == localai.ModalityMultimodalEmbedding {
		return r.srv.MultimodalLoaded()
	}
	if m.Runtime == localai.RuntimeONNX {
		return m.ID == localai.EmbeddingGemmaONNXID && r.srv.EmbedderLoaded()
	}
	return m.ID == r.srv.Model()
}

// localModelsFor is the Local models page's manager: the cache
// -local-ai-embeddings fetches into, and the running endpoint, if any.
func localModelsFor(cfg config.Config, srv *vertexai.Server) *localai.Manager {
	g := &localai.Manager{Cache: embeddingCache(cfg)}
	if srv != nil {
		g.Runtime = embedRuntime{srv: srv}
		// The multimodal model's gs:// images come from this instance's
		// Cloud Storage, read through its JSON API as any client would.
		src := storageSourceFetcher{cfg: cfg, client: &http.Client{Timeout: 2 * time.Minute}}
		srv.SetObjectReader(func(ctx context.Context, bucket, object string) (io.ReadCloser, error) {
			return src.Fetch(ctx, bucket, object, 0)
		})
	}
	return g
}
