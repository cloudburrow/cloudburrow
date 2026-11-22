package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"google.golang.org/grpc/codes"

	runadapter "github.com/cloudburrow/cloudburrow/internal/adapter/run"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/embedding"
	"github.com/cloudburrow/cloudburrow/internal/hfmodels"
	"github.com/cloudburrow/cloudburrow/internal/imagedetect"
	"github.com/cloudburrow/cloudburrow/internal/imagelabel"
	"github.com/cloudburrow/cloudburrow/internal/localai"
	"github.com/cloudburrow/cloudburrow/internal/modelgarden"
	"github.com/cloudburrow/cloudburrow/internal/ocr"
	"github.com/cloudburrow/cloudburrow/internal/service/vertexai"
	"github.com/cloudburrow/cloudburrow/internal/speaker"
	"github.com/cloudburrow/cloudburrow/internal/speech"
	"github.com/cloudburrow/cloudburrow/internal/syntaxnet"
)

// buildLocalAI returns the local generation endpoint, or nil when no model is
// configured.
//
// Absence is the default and is not an error. The runtime is a from-source
// build and the model a multi-gigabyte download; a developer using Cloud
// Storage should not be made to acquire either.
func buildLocalAI(cfg config.Config) (*vertexai.Server, error) {
	attachLabelModel(cfg)
	if strings.TrimSpace(cfg.LocalAI.ModelPath) == "" {
		if !cfg.LocalAI.Embeddings {
			return nil, nil
		}
		// Embeddings only: no generation runtime, so generateContent
		// answers FAILED_PRECONDITION as it does with no model.
		addr := net.JoinHostPort(cfg.BindAddress, strconv.Itoa(cfg.Endpoints.LocalAI))
		srv := newLocalAIServer(addr, nil)
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
	srv := newLocalAIServer(addr, gen)
	attachSpeechModel(cfg) // ML.TRANSCRIBE's route is on every local AI endpoint (#1799)
	attachSpeakerModel(cfg)
	for _, a := range cfg.LocalAI.Aliases {
		srv.Alias(a)
	}
	if cfg.LocalAI.Embeddings {
		attachEmbedder(srv, cfg)
	}
	return srv, nil
}

// localAIServed reports whether `up` serves the local Vertex AI endpoint:
// with a model, with embeddings, or with Cloud Run's prediction endpoints
// (buildLocalAI, withPredictionEndpoints).
func localAIServed(cfg config.Config) bool {
	return strings.TrimSpace(cfg.LocalAI.ModelPath) != "" || cfg.LocalAI.Embeddings || serviceEnabled(cfg, config.ServiceRun)
}

// pinLocalAIPort gives an OS-assigned (0) local AI port a fixed one before
// anything starts, so the BigQuery front can be told where to reach it
// through cloudburrow-host (#1760): the front's arguments are written
// before the endpoint binds. The port stays bound (holdPort) until the
// endpoint serves it.
func pinLocalAIPort(cfg *config.Config) error {
	if cfg.Endpoints.LocalAI != 0 {
		return nil
	}
	port, err := holdPort(cfg.BindAddress)
	if err != nil {
		return fmt.Errorf("choose a port for the local AI endpoint: %w", err)
	}
	cfg.Endpoints.LocalAI = port
	return nil
}

// heldPorts are the listeners of the ports chosen before anything starts
// (holdPort), by address, kept bound until their server takes them
// (takeHeldPort). A port chosen and closed at once is free for anything
// else to bind in the minutes before its server does: in main run
// 45023269046 the local AI endpoint's 41367 was taken while its embedding
// model downloaded, and `up` failed "bind: address already in use".
var heldPorts sync.Map

// holdPort binds an OS-assigned port of bind and keeps it bound; it
// returns the port.
func holdPort(bind string) (int, error) {
	l, err := net.Listen("tcp", net.JoinHostPort(bind, "0"))
	if err != nil {
		return 0, err
	}
	port := l.Addr().(*net.TCPAddr).Port
	heldPorts.Store(net.JoinHostPort(bind, strconv.Itoa(port)), l)
	return port, nil
}

// takeHeldPort returns the listener holdPort keeps bound to addr, once, or
// nil.
func takeHeldPort(addr string) net.Listener {
	if l, ok := heldPorts.LoadAndDelete(addr); ok {
		return l.(net.Listener)
	}
	return nil
}

// newLocalAIServer is the local AI endpoint on addr, serving the port
// pinLocalAIPort holds when it chose addr's.
func newLocalAIServer(addr string, gen vertexai.Generator) *vertexai.Server {
	srv := vertexai.NewServerOn(addr, gen)
	if l := takeHeldPort(addr); l != nil {
		srv.UseListener(l)
	}
	return srv
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
		attachLabelModel(cfg)
		srv = newLocalAIServer(net.JoinHostPort(cfg.BindAddress, strconv.Itoa(cfg.Endpoints.LocalAI)), nil)
		attachSpeechModel(cfg)
	}
	srv.SetEndpoints(endpointResolver{src: predictionSource{run: runProvider{
		kubeconfig:     cfg.KubeconfigPath(),
		namespace:      runadapter.WorkloadNamespace,
		runAddr:        runAddrOf(run),
		defaultProject: cfg.DefaultProject(),
	}}}, ingress, nil)
	return srv
}

// modelGardenFor returns the Hugging Face model cache and, when the local
// AI endpoint serves custom prediction (Cloud Run is enabled, so Docker
// runs the cluster), the Model Garden deployments it serves (#1840), their
// engines restarted from the deployments file.
func modelGardenFor(cfg config.Config, srv *vertexai.Server) (*hfmodels.Manager, *modelgarden.Manager) {
	hf := &hfmodels.Manager{Dir: filepath.Join(cfg.StateDir, "models", "huggingface")}
	if srv == nil || !srv.ServesEndpoints() {
		return hf, nil
	}
	g := &modelgarden.Manager{Instance: cfg.Name, StateFile: filepath.Join(cfg.StateDir, "modelgarden", "deployments.json"),
		Models: hf, Docker: modelgarden.DockerCLI{}}
	srv.SetModelGarden(g)
	g.Restore()
	return hf, g
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
// attachScriptModels lets document_ocr read a scanned page in a script
// other than Latin (#1733): tessdata_fast's model of that script is fetched
// the first time one is read, into <state-dir>/models, and checked against
// its pinned SHA-256.
func attachScriptModels(cfg config.Config) {
	cache := embeddingCache(cfg)
	ocr.SetScriptSource(func(ctx context.Context, name string) (string, error) {
		f, ok := localai.TessdataScript(name)
		if !ok {
			return "", fmt.Errorf("no tessdata_fast model %q is pinned", name)
		}
		if _, err := os.Stat(cache.PinnedPath(localai.TessdataFastScripts, f)); err != nil {
			fmt.Fprintf(os.Stderr, "fetching tessdata_fast %s (%s @ %s, Apache-2.0) for document_ocr...\n",
				f.Path, localai.TessdataFastRepo, localai.TessdataFastScripts.Pin.Revision[:12])
		}
		return cache.FetchPinnedFile(ctx, localai.TessdataFastScripts, f)
	})
}

// attachSpeechModel lets ML.TRANSCRIBE read audio (#1799): whisper.cpp's
// ggml Whisper base model is fetched the first time audio is transcribed,
// into <state-dir>/models, and checked against its pinned SHA-256.
func attachSpeechModel(cfg config.Config) {
	cache := embeddingCache(cfg)
	speech.SetModelSource(func(ctx context.Context) (string, error) {
		f := localai.Whisper.Pin.Files[0]
		if _, err := os.Stat(cache.PinnedPath(localai.Whisper, f)); err != nil {
			fmt.Fprintf(os.Stderr, "fetching %s (%s @ %s, MIT) for ML.TRANSCRIBE...\n",
				f.Path, localai.WhisperRepo, localai.Whisper.Pin.Revision[:12])
		}
		return cache.FetchPinnedFile(ctx, localai.Whisper, f)
	})
}

// attachSpeakerModel lets Speech-to-Text diarize audio (#2131) in the onnx
// build: 3D-Speaker's CAM++ speaker model is fetched the first time audio
// is diarized, into <state-dir>/models, and checked against its pinned
// SHA-256. The default build never fetches it: diarization is 501 there.
func attachSpeakerModel(cfg config.Config) {
	if !speaker.Available {
		return
	}
	cache := embeddingCache(cfg)
	speaker.SetModelSource(func(ctx context.Context) (string, error) {
		m := localai.SpeakerCAMPlusPlus
		f := m.Pin.Files[0]
		if _, err := os.Stat(cache.PinnedPath(m, f)); err != nil {
			fmt.Fprintf(os.Stderr, "fetching 3D-Speaker's CAM++ %s (%s release %s, Apache-2.0) for speaker diarization...\n",
				f.Path, m.Repo, m.Pin.Revision)
		}
		return cache.FetchPinnedFile(ctx, m, f)
	})
}

// attachLabelModel lets Vision's LABEL_DETECTION label an image (#1799):
// MediaPipe's EfficientNet-Lite0 classifier is fetched from
// storage.googleapis.com the first time one is labelled, into
// <state-dir>/models, and checked against its pinned SHA-256.
//
// It does the same for OBJECT_LOCALIZATION and FACE_DETECTION, whose
// MediaPipe EfficientDet-Lite0 and BlazeFace detectors are fetched the
// first time an image needs one.
func attachLabelModel(cfg config.Config) {
	cache := embeddingCache(cfg)
	pinned := func(m localai.Model, feature string) func(ctx context.Context) (string, error) {
		f := m.Pin.Files[0]
		return func(ctx context.Context) (string, error) {
			if _, err := os.Stat(cache.PinnedPath(m, f)); err != nil {
				fmt.Fprintf(os.Stderr, "fetching MediaPipe %s (%s, Apache-2.0) for Vision %s...\n", f.Path, m.Repo, feature)
			}
			return cache.FetchPinnedFile(ctx, m, f)
		}
	}
	imagelabel.SetSource(pinned(localai.EfficientNetLite0, "LABEL_DETECTION"))
	imagedetect.SetObjectSource(pinned(localai.EfficientDetLite0, "OBJECT_LOCALIZATION"))
	imagedetect.SetFaceSource(pinned(localai.BlazeFaceShortRange, "FACE_DETECTION"))
	attachSyntaxModel(cache)
}

// attachSyntaxModel lets Natural Language's analyzeSyntax parse English
// (#1799): SyntaxNet's Parsey McParseface is fetched from Google's
// tensorflow/models repository the first time a text is parsed, into
// <state-dir>/models, and checked against its pinned SHA-256s.
func attachSyntaxModel(cache localai.Cache) {
	m := localai.ParseyMcParseface
	syntaxnet.SetSource(func(ctx context.Context) (string, error) {
		if cache.VerifyPinned(m) != nil {
			fmt.Fprintf(os.Stderr, "fetching SyntaxNet's Parsey McParseface (tensorflow/models @ %s, Apache-2.0) for analyzeSyntax...\n",
				m.Pin.Revision[:12])
			if err := cache.FetchPinned(ctx, m, nil); err != nil {
				return "", err
			}
		}
		return cache.PinnedDir(m), nil
	})
}

func attachEmbedder(srv *vertexai.Server, cfg config.Config) {
	attachScriptModels(cfg)
	attachSpeechModel(cfg)
	attachSpeakerModel(cfg)
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
	// The embeddings line names the route only when the runtime loaded (an
	// onnx build with the model fetched): scripts/compat-env.sh exports
	// CLOUDBURROW_TEST_EMBEDDINGS from it (#1765), so a default build's
	// instance leaves the variable unset and its tests skip.
	if cfg.LocalAI.Embeddings && !srv.EmbedderLoaded() {
		fmt.Fprintf(w, "  embeddings: not loaded (every :predict answers 501; an onnx build with ONNXRUNTIME_LIB serves them)\n")
	}
	if cfg.LocalAI.Embeddings && srv.EmbedderLoaded() {
		fmt.Fprintf(w, "  embeddings: POST /v1/projects/{p}/locations/{l}/publishers/google/models/%s:predict\n", localai.EmbeddingGemmaONNXID)
		fmt.Fprintf(w, "              a COMMUNITY ONNX int8 conversion, NOT an official Google artifact\n")
	}
	if srv.ServesEndpoints() {
		fmt.Fprintf(w, "  custom prediction: PredictionService (gRPC) and POST /v1/projects/{p}/locations/{l}/endpoints/{service}:predict\n")
		fmt.Fprintf(w, "              {service} is a deployed Cloud Run prediction container; see docs/prediction.md\n")
	}
	fmt.Fprintf(w, "  sampling, maxOutputTokens, stopSequences, JSON schemas, candidateCount, thinking and context caching are applied; other options are refused; see docs/generation.md\n")
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
	e, err := embedding.Open(dir, m.ID)
	if err != nil {
		return err
	}
	if old := r.srv.SwapModelEmbedder(m.ID, e, nil); old != nil {
		_ = old.Close()
	}
	return nil
}

func (r embedRuntime) Unload(m localai.Model) error {
	old := r.srv.SwapModelEmbedder(m.ID, nil, fmt.Errorf("%s was unloaded on the console's Local models page; load it there to serve it again", m.ID))
	if old != nil {
		return old.Close()
	}
	return nil
}

func (r embedRuntime) Loaded(m localai.Model) bool {
	if m.Runtime == localai.RuntimeONNX {
		return r.srv.ModelEmbedderLoaded(m.ID)
	}
	return m.ID == r.srv.Model()
}

// localModelsFor is the Local models page's manager: the cache
// -local-ai-embeddings fetches into, and the running endpoint, if any.
func localModelsFor(cfg config.Config, srv *vertexai.Server) *localai.Manager {
	g := &localai.Manager{Cache: embeddingCache(cfg)}
	if srv != nil {
		g.Runtime = embedRuntime{srv: srv}
	}
	return g
}

// embedObjects reads EmbedContent's file_data objects (#1651) from this
// instance's Cloud Storage, with their content type. It reads only that
// local endpoint: with none bound it refuses, and it never falls back to
// Google's.
type embedObjects struct{ f storageSourceFetcher }

// hasEndpoint reports whether this instance's Cloud Storage endpoint is
// known: the tunnel's bound address, or a configured port.
func (o embedObjects) hasEndpoint() bool {
	if o.f.addr != nil && o.f.addr() != "" {
		return true
	}
	return o.f.cfg.Endpoints.Storage > 0
}

func (o embedObjects) Read(ctx context.Context, bucket, object string, limit int64) ([]byte, string, error) {
	if !o.hasEndpoint() {
		return nil, "", apierror.FailedPrecondition(
			"file_data gs://%s/%s: this instance's Cloud Storage has no endpoint; start it with the storage service, or send the bytes as inline_data", bucket, object)
	}
	rc, ct, err := o.f.fetchTyped(ctx, bucket, object)
	if err != nil {
		if e := apierror.From(err); e.Code == codes.NotFound {
			if o.bucketMissing(ctx, bucket) {
				return nil, "", apierror.NotFound("gs://%s/%s: bucket %q not found", bucket, object, bucket)
			}
			return nil, "", apierror.NotFound("gs://%s/%s not found", bucket, object)
		}
		return nil, "", err
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, limit+1))
	if err != nil {
		return nil, "", apierror.FailedPrecondition("read gs://%s/%s: %v", bucket, object, err)
	}
	if int64(len(data)) > limit {
		return nil, "", apierror.InvalidArgument("gs://%s/%s is larger than %d MiB, the most file_data reads", bucket, object, limit>>20)
	}
	return data, ct, nil
}

// Write writes an object to this instance's Cloud Storage, for the batch
// methods' outputs (#1930).
func (o embedObjects) Write(ctx context.Context, bucket, object, contentType string, data []byte) error {
	if !o.hasEndpoint() {
		return apierror.FailedPrecondition("gs://%s/%s: this instance's Cloud Storage has no endpoint", bucket, object)
	}
	w := storageObjectWriter{src: o.f, client: o.f.client}
	if err := w.WriteObject(ctx, bucket, object, contentType, data); err != nil {
		if o.bucketMissing(ctx, bucket) {
			return apierror.NotFound("gs://%s/%s: bucket %q not found", bucket, object, bucket)
		}
		return apierror.FailedPrecondition("write %v", err)
	}
	return nil
}

// List names the objects under prefix in bucket, for Document AI's
// gcs_prefix (#1930).
func (o embedObjects) List(ctx context.Context, bucket, prefix string) ([]string, error) {
	if !o.hasEndpoint() {
		return nil, apierror.FailedPrecondition("gs://%s/%s: this instance's Cloud Storage has no endpoint", bucket, prefix)
	}
	var names []string
	token := ""
	for {
		q := url.Values{"prefix": {prefix}, "fields": {"items(name),nextPageToken"}}
		if token != "" {
			q.Set("pageToken", token)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.f.base()+"/storage/v1/b/"+url.PathEscape(bucket)+"/o?"+q.Encode(), nil)
		if err != nil {
			return nil, apierror.InvalidArgument("list gs://%s/%s: %v", bucket, prefix, err)
		}
		resp, err := o.f.client.Do(req)
		if err != nil {
			return nil, apierror.FailedPrecondition("list gs://%s/%s: %v", bucket, prefix, err)
		}
		var page struct {
			Items         []struct{ Name string } `json:"items"`
			NextPageToken string                  `json:"nextPageToken"`
		}
		err = json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&page)
		resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound {
			return nil, apierror.NotFound("gs://%s: bucket %q not found", bucket, bucket)
		}
		if resp.StatusCode != http.StatusOK || err != nil {
			return nil, apierror.FailedPrecondition("list gs://%s/%s: HTTP %d", bucket, prefix, resp.StatusCode)
		}
		for _, it := range page.Items {
			names = append(names, it.Name)
		}
		if page.NextPageToken == "" {
			return names, nil
		}
		token = page.NextPageToken
	}
}

// bucketMissing reports whether this instance's Cloud Storage answers 404
// for the bucket itself, so a missing bucket is told from a missing object.
func (o embedObjects) bucketMissing(ctx context.Context, bucket string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.f.base()+"/storage/v1/b/"+url.PathEscape(bucket), nil)
	if err != nil {
		return false
	}
	resp, err := o.f.client.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusNotFound
}
