package vertexai

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"math/big"
	mrand "math/rand/v2"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/lro"
)

// The Model Registry and Endpoints (#1940): ModelService's uploaded models,
// their versions, aliases and evaluations, and EndpointService's endpoints,
// whose deployed models run their model's serving container as a real
// workload in the instance's cluster, deployed through the Cloud Run API
// (a ModelRunner), with the custom container contract's AIP_* environment
// and AIP_STORAGE_URI pointing at a copy of the model's artifacts in the
// instance's Cloud Storage (an ArtifactStore). Model Garden's Hugging Face
// deployments (modelgarden.go) are served beside these, by their own IDs.

// ModelRunner runs a deployed model's serving container.
type ModelRunner interface {
	// Run deploys spec in project and returns the URL its container is
	// served at once it answers its health route.
	Run(ctx context.Context, project string, spec ContainerRun) (string, error)
	// Remove deletes the workload; a missing one is already removed.
	Remove(ctx context.Context, project, id string) error
}

// ContainerRun is one deployed model's container.
type ContainerRun struct {
	ID                        string // the workload's ID
	Image                     string
	Command, Args             []string
	Env                       []EnvPair // the model's env, then the AIP_* contract variables
	Port                      int
	HealthRoute, PredictRoute string
	MinReplicas, MaxReplicas  int
	Labels                    map[string]string
}

// EnvPair is one environment variable.
type EnvPair struct{ Name, Value string }

// ArtifactStore copies model artifacts within the instance's Cloud Storage.
type ArtifactStore interface {
	// Copy copies every object under the gs:// directory src to the gs://
	// directory dst, failing when src holds none.
	Copy(ctx context.Context, project, src, dst string) error
	// ManagedBucket is the bucket the registry keeps artifact copies in,
	// Vertex's service-managed bucket.
	ManagedBucket(project string) string
}

// ModelImages is this instance's Artifact Registry, as the Model Registry
// uses it: ExportModel's image_destination and the prebuilt containers.
type ModelImages interface {
	// Export copies the image src to dst, an Artifact Registry path
	// ([:tag], "latest" when it has none), and returns the image's full
	// path.
	Export(ctx context.Context, src, dst string) (string, error)
	// Held reports whether ref names an image the registry holds.
	Held(ref string) bool
}

// registry keeps the uploaded models and the endpoints.
type registry struct {
	stateFile string
	runner    ModelRunner
	artifacts ArtifactStore
	images    ModelImages
	now       func() time.Time

	mu        sync.Mutex
	models    map[string]*regModel
	endpoints map[string]*regEndpoint
}

// regModel is one model: its versions, by version ID, and evaluations.
type regModel struct {
	Project, Location, ID string
	NextVersion           int
	Versions              []*aiplatformpb.Model
	Evaluations           []*aiplatformpb.ModelEvaluation
}

// regEndpoint is one endpoint and where each deployed model is served.
type regEndpoint struct {
	Project, Location, ID string
	Endpoint              *aiplatformpb.Endpoint
	// Serving maps a deployed model ID to its workload.
	Serving map[string]*serving
}

type serving struct {
	Workload     string `json:"workload"`
	URL          string `json:"url"`
	PredictRoute string `json:"predictRoute"`
	HealthRoute  string `json:"healthRoute"`
	Model        string `json:"model"` // the model version's name, models/{id}@{version}
	Failed       string `json:"failed,omitempty"`
}

// SetRegistry serves the Model Registry and Endpoints, persisted in
// stateFile, deployed models run by runner with artifacts copied by store
// (nil when Cloud Storage is not served: an upload with artifactUri is then
// FAILED_PRECONDITION).
func (s *Server) SetRegistry(stateFile string, runner ModelRunner, store ArtifactStore) {
	r := &registry{stateFile: stateFile, runner: runner, artifacts: store, now: func() time.Time { return time.Now().UTC() }}
	r.load()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reg = r
	if s.ops == nil {
		s.ops = lro.NewStore(nil)
	}
}

// SetModelImages names the Artifact Registry ExportModel pushes images to
// and DeployModel looks Google's prebuilt containers up in; call it after
// SetRegistry.
func (s *Server) SetModelImages(img ModelImages) {
	if r := s.registry(); r != nil {
		r.mu.Lock()
		r.images = img
		r.mu.Unlock()
	}
}

func (s *Server) registry() *registry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.reg
}

// ---- persistence

type persistedModel struct {
	Project     string            `json:"project"`
	Location    string            `json:"location"`
	ID          string            `json:"id"`
	NextVersion int               `json:"nextVersion"`
	Versions    []json.RawMessage `json:"versions"`
	Evaluations []json.RawMessage `json:"evaluations,omitempty"`
}

type persistedEndpoint struct {
	Project  string              `json:"project"`
	Location string              `json:"location"`
	ID       string              `json:"id"`
	Endpoint json.RawMessage     `json:"endpoint"`
	Serving  map[string]*serving `json:"serving"`
}

type persistedRegistry struct {
	Models    []persistedModel    `json:"models"`
	Endpoints []persistedEndpoint `json:"endpoints"`
}

func (r *registry) load() {
	r.models, r.endpoints = map[string]*regModel{}, map[string]*regEndpoint{}
	if r.stateFile == "" {
		return
	}
	b, err := os.ReadFile(r.stateFile)
	if err != nil {
		return
	}
	var p persistedRegistry
	if json.Unmarshal(b, &p) != nil {
		return
	}
	for _, m := range p.Models {
		x := &regModel{Project: m.Project, Location: m.Location, ID: m.ID, NextVersion: m.NextVersion}
		for _, v := range m.Versions {
			pm := &aiplatformpb.Model{}
			if protojson.Unmarshal(v, pm) == nil {
				x.Versions = append(x.Versions, pm)
			}
		}
		for _, v := range m.Evaluations {
			pe := &aiplatformpb.ModelEvaluation{}
			if protojson.Unmarshal(v, pe) == nil {
				x.Evaluations = append(x.Evaluations, pe)
			}
		}
		r.models[key3(m.Project, m.Location, m.ID)] = x
	}
	for _, e := range p.Endpoints {
		pe := &aiplatformpb.Endpoint{}
		if protojson.Unmarshal(e.Endpoint, pe) != nil {
			continue
		}
		if e.Serving == nil {
			e.Serving = map[string]*serving{}
		}
		r.endpoints[key3(e.Project, e.Location, e.ID)] = &regEndpoint{Project: e.Project, Location: e.Location, ID: e.ID, Endpoint: pe, Serving: e.Serving}
	}
}

// save writes the state file; the caller holds mu.
func (r *registry) save() {
	if r.stateFile == "" {
		return
	}
	p := persistedRegistry{Models: []persistedModel{}, Endpoints: []persistedEndpoint{}}
	for _, m := range r.models {
		pm := persistedModel{Project: m.Project, Location: m.Location, ID: m.ID, NextVersion: m.NextVersion}
		for _, v := range m.Versions {
			b, _ := protojson.Marshal(v)
			pm.Versions = append(pm.Versions, b)
		}
		for _, v := range m.Evaluations {
			b, _ := protojson.Marshal(v)
			pm.Evaluations = append(pm.Evaluations, b)
		}
		p.Models = append(p.Models, pm)
	}
	for _, e := range r.endpoints {
		b, _ := protojson.Marshal(e.Endpoint)
		p.Endpoints = append(p.Endpoints, persistedEndpoint{Project: e.Project, Location: e.Location, ID: e.ID, Endpoint: b, Serving: e.Serving})
	}
	sort.Slice(p.Models, func(i, j int) bool { return p.Models[i].ID < p.Models[j].ID })
	sort.Slice(p.Endpoints, func(i, j int) bool { return p.Endpoints[i].ID < p.Endpoints[j].ID })
	b, _ := json.MarshalIndent(p, "", "  ")
	_ = os.MkdirAll(filepath.Dir(r.stateFile), 0o755)
	tmp := r.stateFile + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, r.stateFile)
	}
}

func key3(project, location, id string) string { return project + "/" + location + "/" + id }

func regNumericID() string {
	n, _ := rand.Int(rand.Reader, big.NewInt(8e18))
	return strconv.FormatInt(n.Int64()+1e18, 10)
}

// ---- validation

// modelIDPattern is UploadModelRequest.model_id's rule: up to 63
// characters of [a-z0-9_-], not starting with a digit or hyphen.
var modelIDPattern = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,62}$`)

// endpointIDPattern is CreateEndpointRequest.endpoint_id's rule: up to 10
// digits, or up to 63 lowercase letters, digits and hyphens starting with a
// letter and not ending with a hyphen.
var endpointIDPattern = regexp.MustCompile(`^([0-9]{1,10}|[a-z]([a-z0-9-]{0,61}[a-z0-9])?)$`)

// aliasPattern is a version alias, [a-z][a-zA-Z0-9-]{0,126}[a-z0-9].
var aliasPattern = regexp.MustCompile(`^[a-z][a-zA-Z0-9-]{0,126}[a-z0-9]$`)

// deployedModelIDPattern is DeployedModel.id: 1 to 10 digits.
var deployedModelIDPattern = regexp.MustCompile(`^[0-9]{1,10}$`)

// labelKeyPattern and labelValuePattern are Google's label rules.
var (
	labelKeyPattern   = regexp.MustCompile(`^[\p{Ll}\p{Lo}][\p{Ll}\p{Lo}\p{N}_-]{0,62}$`)
	labelValuePattern = regexp.MustCompile(`^[\p{Ll}\p{Lo}\p{N}_-]{0,63}$`)
)

func checkLabels(labels map[string]string) error {
	if len(labels) > 64 {
		return apierror.InvalidArgument("labels has %d entries; at most 64 are allowed", len(labels))
	}
	for k, v := range labels {
		if !labelKeyPattern.MatchString(k) {
			return apierror.InvalidArgument("label key %q is invalid: it must start with a lowercase letter and hold at most 63 "+
				"lowercase letters, digits, underscores and dashes", k)
		}
		if !labelValuePattern.MatchString(v) {
			return apierror.InvalidArgument("label value %q (key %q) is invalid: at most 63 lowercase letters, digits, underscores and dashes", v, k)
		}
	}
	return nil
}

func checkDisplayName(field, name string) error {
	if name == "" {
		return apierror.InvalidArgument("%s is required", field)
	}
	if len([]rune(name)) > 128 {
		return apierror.InvalidArgument("%s is longer than 128 characters", field)
	}
	return nil
}

// reservedEnv reports whether a container env name is one Vertex sets.
func reservedEnv(name string) bool { return strings.HasPrefix(name, "AIP_") }

// checkContainerSpec validates a model's container spec as UploadModel
// does.
func checkContainerSpec(c *aiplatformpb.ModelContainerSpec) error {
	if c == nil {
		return apierror.InvalidArgument("model.container_spec is required: CloudBurrow serves models whose serving container is " +
			"given (a prebuilt or custom container's image_uri); AutoML and BigQuery ML models are not served")
	}
	if c.GetImageUri() == "" {
		return apierror.InvalidArgument("model.container_spec.image_uri is required")
	}
	if len(c.GetPorts()) > 1 {
		return apierror.InvalidArgument("model.container_spec.ports has %d entries; Vertex AI accepts at most one", len(c.GetPorts()))
	}
	for _, p := range c.GetPorts() {
		if n := p.GetContainerPort(); n < 1 || n > 65535 {
			return apierror.InvalidArgument("model.container_spec.ports[0].container_port %d is not in [1, 65535]", n)
		}
		switch p.GetContainerPort() {
		case 8100, 8101, 8102, 8103, 8205, 8888:
			// Reserved by Vertex AI's serving infrastructure.
			return apierror.InvalidArgument("model.container_spec.ports[0].container_port %d is reserved by Vertex AI; "+
				"ports 8100-8103, 8205 and 8888 cannot be used", p.GetContainerPort())
		}
	}
	for _, r := range []struct{ field, v string }{{"predict_route", c.GetPredictRoute()}, {"health_route", c.GetHealthRoute()}} {
		if r.v != "" && !strings.HasPrefix(r.v, "/") {
			return apierror.InvalidArgument("model.container_spec.%s %q must start with /", r.field, r.v)
		}
	}
	for _, e := range c.GetEnv() {
		if e.GetName() == "" {
			return apierror.InvalidArgument("model.container_spec.env has an entry with no name")
		}
		if reservedEnv(e.GetName()) {
			return apierror.InvalidArgument("model.container_spec.env %q is invalid: the AIP_ prefix is reserved for the variables "+
				"Vertex AI sets", e.GetName())
		}
	}
	return nil
}

// ---- models

func (r *registry) model(project, location, id string) (*regModel, error) {
	m, ok := r.models[key3(project, location, id)]
	if !ok {
		return nil, apierror.NotFound("Model projects/%s/locations/%s/models/%s not found", project, location, id)
	}
	return m, nil
}

func (m *regModel) name() string {
	return "projects/" + m.Project + "/locations/" + m.Location + "/models/" + m.ID
}

// version returns the version a reference names: a version ID, an alias,
// or "" for the default version.
func (m *regModel) version(ref string) (*aiplatformpb.Model, error) {
	if ref == "" {
		ref = "default"
	}
	for _, v := range m.Versions {
		if v.GetVersionId() == ref {
			return v, nil
		}
		for _, a := range v.GetVersionAliases() {
			if a == ref {
				return v, nil
			}
		}
	}
	return nil, apierror.NotFound("Model %s@%s not found", m.name(), ref)
}

// parseModelName parses projects/{p}/locations/{l}/models/{id}[@{ref}].
func parseModelName(field, name string) (project, location, id, ref string, err error) {
	base, ref, _ := strings.Cut(name, "@")
	project, location, id, err = parseChild(field, "models", base)
	return
}

func (r *registry) has(project, location, id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.models[key3(project, location, id)]
	return ok
}

// withDeployments returns a copy of v with its deployed_models filled.
func (r *registry) withDeployments(m *regModel, v *aiplatformpb.Model) *aiplatformpb.Model {
	out := proto.Clone(v).(*aiplatformpb.Model)
	out.DeployedModels = nil
	for _, e := range sortedEndpoints(r.endpoints) {
		for _, d := range e.Endpoint.GetDeployedModels() {
			mp, _, mid, ref, err := parseModelName("model", d.GetModel())
			if err != nil || mp != m.Project || mid != m.ID {
				continue
			}
			if d.GetModelVersionId() == v.GetVersionId() || (ref == v.GetVersionId()) {
				out.DeployedModels = append(out.DeployedModels, &aiplatformpb.DeployedModelRef{
					Endpoint: e.Endpoint.GetName(), DeployedModelId: d.GetId()})
			}
		}
	}
	return out
}

func sortedEndpoints(m map[string]*regEndpoint) []*regEndpoint {
	out := make([]*regEndpoint, 0, len(m))
	for _, e := range m {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// managedURI is where a model version's artifact copy is kept.
func (r *registry) managedURI(project, location, id, version string) string {
	return "gs://" + r.artifacts.ManagedBucket(project) + "/models/" + location + "/" + id + "/" + version
}

// upload creates a model, or a version of parent, from req; it copies the
// artifacts first, as Vertex populates AIP_STORAGE_URI's directory when the
// Model is created.
func (r *registry) upload(ctx context.Context, req *aiplatformpb.UploadModelRequest) (*aiplatformpb.UploadModelResponse, error) {
	project, location, err := parseParent("parent", req.GetParent())
	if err != nil {
		return nil, err
	}
	in := req.GetModel()
	if in == nil {
		return nil, apierror.InvalidArgument("model is required")
	}
	if err := checkDisplayName("model.display_name", in.GetDisplayName()); err != nil {
		return nil, err
	}
	if err := checkContainerSpec(in.GetContainerSpec()); err != nil {
		return nil, err
	}
	if err := checkLabels(in.GetLabels()); err != nil {
		return nil, err
	}
	for _, a := range in.GetVersionAliases() {
		if !aliasPattern.MatchString(a) {
			return nil, apierror.InvalidArgument("model.version_aliases %q does not match [a-z][a-zA-Z0-9-]{0,126}[a-z0-9]", a)
		}
	}
	art := in.GetArtifactUri()
	if art != "" && !strings.HasPrefix(art, "gs://") {
		return nil, apierror.InvalidArgument("model.artifact_uri %q is not a gs:// URI", art)
	}
	if art != "" && r.artifacts == nil {
		return nil, apierror.FailedPrecondition("model.artifact_uri is set, but Cloud Storage is not served on this instance " +
			"(start with --services including storage), so the artifacts cannot be copied")
	}

	r.mu.Lock()
	var m *regModel
	if pm := req.GetParentModel(); pm != "" {
		pp, pl, pid, ref, err := parseModelName("parent_model", pm)
		if err != nil {
			r.mu.Unlock()
			return nil, err
		}
		if ref != "" {
			r.mu.Unlock()
			return nil, apierror.InvalidArgument("parent_model %q must not name a version", pm)
		}
		if pp != project || pl != location {
			r.mu.Unlock()
			return nil, apierror.InvalidArgument("parent_model %q is not in %s", pm, req.GetParent())
		}
		if m, err = r.model(pp, pl, pid); err != nil {
			r.mu.Unlock()
			return nil, err
		}
		for _, a := range in.GetVersionAliases() {
			if a == "default" {
				continue
			}
			for _, v := range m.Versions {
				if contains(v.GetVersionAliases(), a) {
					r.mu.Unlock()
					return nil, apierror.AlreadyExists("version alias %q is already used by %s@%s", a, m.name(), v.GetVersionId())
				}
			}
		}
	} else {
		id := req.GetModelId()
		if id == "" {
			id = regNumericID()
		} else if !modelIDPattern.MatchString(id) {
			r.mu.Unlock()
			return nil, apierror.InvalidArgument("model_id %q is invalid: up to 63 characters of [a-z0-9_-], not starting with a digit or hyphen", id)
		}
		if _, ok := r.models[key3(project, location, id)]; ok {
			r.mu.Unlock()
			return nil, apierror.AlreadyExists("Model projects/%s/locations/%s/models/%s already exists", project, location, id)
		}
		m = &regModel{Project: project, Location: location, ID: id, NextVersion: 1}
	}
	version := strconv.Itoa(m.NextVersion)
	m.NextVersion++
	// Reserve the model (a new one) so a concurrent upload of the same ID
	// is refused; its first version lands below.
	r.models[key3(project, location, m.ID)] = m
	r.mu.Unlock()

	storage := ""
	if art != "" {
		storage = r.managedURI(project, location, m.ID, version)
		if err := r.artifacts.Copy(ctx, project, art, storage); err != nil {
			r.mu.Lock()
			if len(m.Versions) == 0 {
				delete(r.models, key3(project, location, m.ID))
			}
			r.mu.Unlock()
			return nil, err
		}
	}

	now := timestamppb.New(r.now())
	v := proto.Clone(in).(*aiplatformpb.Model)
	v.Name, v.VersionId = m.name(), version
	v.CreateTime, v.UpdateTime, v.VersionCreateTime, v.VersionUpdateTime = now, now, now, now
	v.DeployedModels = nil
	v.SupportedDeploymentResourcesTypes = []aiplatformpb.Model_DeploymentResourcesType{aiplatformpb.Model_DEDICATED_RESOURCES}
	v.SupportedInputStorageFormats = []string{"jsonl", "csv", "tf-record", "tf-record-gzip", "file-list"}
	v.SupportedOutputStorageFormats = []string{"jsonl"}
	v.ModelSourceInfo = &aiplatformpb.ModelSourceInfo{SourceType: aiplatformpb.ModelSourceInfo_CUSTOM}
	v.SupportedExportFormats = exportFormats(storage != "")
	if v.Labels == nil {
		v.Labels = map[string]string{}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	aliases := append([]string(nil), in.GetVersionAliases()...)
	if len(m.Versions) == 0 && !contains(aliases, "default") {
		aliases = append([]string{"default"}, aliases...)
	}
	if contains(aliases, "default") {
		for _, o := range m.Versions {
			o.VersionAliases = without(o.GetVersionAliases(), "default")
		}
	}
	v.VersionAliases = aliases
	m.Versions = append(m.Versions, v)
	// The model-level fields follow the newest version.
	for _, o := range m.Versions {
		o.UpdateTime = now
	}
	r.save()
	return &aiplatformpb.UploadModelResponse{Model: m.name(), ModelVersionId: version}, nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func without(list []string, s string) []string {
	var out []string
	for _, x := range list {
		if x != s {
			out = append(out, x)
		}
	}
	return out
}

// storageURI is the copy of a version's artifacts, "" when it has none.
func (r *registry) storageURI(project, location, id, version string) string {
	m, err := r.model(project, location, id)
	if err != nil {
		return ""
	}
	for _, v := range m.Versions {
		if v.GetVersionId() == version && v.GetArtifactUri() != "" && r.artifacts != nil {
			return r.managedURI(project, location, id, version)
		}
	}
	return ""
}

func (r *registry) get(name string) (*aiplatformpb.Model, error) {
	p, l, id, ref, err := parseModelName("name", name)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	m, err := r.model(p, l, id)
	if err != nil {
		return nil, err
	}
	v, err := m.version(ref)
	if err != nil {
		return nil, err
	}
	return r.withDeployments(m, v), nil
}

// list returns the default version of each model in a location.
func (r *registry) list(project, location string) []*aiplatformpb.Model {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*aiplatformpb.Model
	for _, m := range r.models {
		if m.Project != project || (location != "-" && m.Location != location) || len(m.Versions) == 0 {
			continue
		}
		if v, err := m.version(""); err == nil {
			out = append(out, r.withDeployments(m, v))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GetName() < out[j].GetName() })
	return out
}

func (r *registry) versions(name string) ([]*aiplatformpb.Model, error) {
	p, l, id, ref, err := parseModelName("name", name)
	if err != nil {
		return nil, err
	}
	if ref != "" {
		return nil, apierror.InvalidArgument("name %q must not name a version", name)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	m, err := r.model(p, l, id)
	if err != nil {
		return nil, err
	}
	var out []*aiplatformpb.Model
	for _, v := range m.Versions {
		out = append(out, r.withDeployments(m, v))
	}
	return out, nil
}

// update applies an UpdateModel field mask to the named version (the
// default when none is named). Model-level fields (display_name,
// description, labels) are one per version on Vertex too: each version
// carries its own copy.
func (r *registry) update(in *aiplatformpb.Model, paths []string) (*aiplatformpb.Model, error) {
	p, l, id, ref, err := parseModelName("model.name", in.GetName())
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return nil, apierror.InvalidArgument("update_mask is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	m, err := r.model(p, l, id)
	if err != nil {
		return nil, err
	}
	v, err := m.version(ref)
	if err != nil {
		return nil, err
	}
	next := proto.Clone(v).(*aiplatformpb.Model)
	for _, path := range paths {
		switch path {
		case "display_name", "displayName":
			if err := checkDisplayName("model.display_name", in.GetDisplayName()); err != nil {
				return nil, err
			}
			next.DisplayName = in.GetDisplayName()
		case "description":
			next.Description = in.GetDescription()
		case "version_description", "versionDescription":
			next.VersionDescription = in.GetVersionDescription()
		case "labels":
			if err := checkLabels(in.GetLabels()); err != nil {
				return nil, err
			}
			next.Labels = in.GetLabels()
		case "explanation_spec", "explanationSpec":
			next.ExplanationSpec = in.GetExplanationSpec()
		case "version_aliases", "versionAliases":
			return nil, apierror.InvalidArgument("version_aliases cannot be updated with UpdateModel; use MergeVersionAliases")
		default:
			return nil, apierror.InvalidArgument("update_mask path %q is not updatable; the updatable fields are display_name, "+
				"description, version_description, labels and explanation_spec", path)
		}
	}
	now := timestamppb.New(r.now())
	next.UpdateTime, next.VersionUpdateTime = now, now
	for i, o := range m.Versions {
		if o == v {
			m.Versions[i] = next
		}
	}
	r.save()
	return r.withDeployments(m, next), nil
}

// mergeAliases adds, and with a "-" prefix removes, a version's aliases.
func (r *registry) mergeAliases(name string, aliases []string) (*aiplatformpb.Model, error) {
	p, l, id, ref, err := parseModelName("name", name)
	if err != nil {
		return nil, err
	}
	if ref == "" || strings.Trim(ref, "0123456789") != "" {
		return nil, apierror.InvalidArgument("name %q must name the version by its ID: models/{model}@{version_id}", name)
	}
	if len(aliases) == 0 {
		return nil, apierror.InvalidArgument("version_aliases is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	m, err := r.model(p, l, id)
	if err != nil {
		return nil, err
	}
	v, err := m.version(ref)
	if err != nil {
		return nil, err
	}
	add, remove := map[string]bool{}, map[string]bool{}
	for _, a := range aliases {
		bare := strings.TrimPrefix(a, "-")
		if !aliasPattern.MatchString(bare) {
			return nil, apierror.InvalidArgument("version alias %q does not match [a-z][a-zA-Z0-9-]{0,126}[a-z0-9]", bare)
		}
		if strings.HasPrefix(a, "-") {
			remove[bare] = true
		} else {
			add[bare] = true
		}
	}
	for a := range add {
		if remove[a] {
			delete(add, a)
			delete(remove, a)
		}
	}
	if remove["default"] {
		return nil, apierror.InvalidArgument("the default alias cannot be removed: a model has exactly one default version; " +
			"add default to another version to move it")
	}
	for a := range add {
		if a == "default" {
			continue
		}
		for _, o := range m.Versions {
			if o != v && contains(o.GetVersionAliases(), a) {
				return nil, apierror.AlreadyExists("version alias %q is already used by %s@%s", a, m.name(), o.GetVersionId())
			}
		}
	}
	if add["default"] {
		for _, o := range m.Versions {
			o.VersionAliases = without(o.GetVersionAliases(), "default")
		}
	}
	var next []string
	for _, a := range v.GetVersionAliases() {
		if !remove[a] && !add[a] {
			next = append(next, a)
		}
	}
	keys := make([]string, 0, len(add))
	for a := range add {
		keys = append(keys, a)
	}
	sort.Strings(keys)
	v.VersionAliases = append(next, keys...)
	v.VersionUpdateTime = timestamppb.New(r.now())
	r.save()
	return r.withDeployments(m, v), nil
}

// deployedFrom reports the endpoint a model (one version, or any when
// version is "") is deployed on, "" when none.
func (r *registry) deployedFrom(project, location, id, version string) string {
	for _, e := range sortedEndpoints(r.endpoints) {
		for _, d := range e.Endpoint.GetDeployedModels() {
			mp, ml, mid, _, err := parseModelName("model", d.GetModel())
			if err == nil && mp == project && ml == location && mid == id && (version == "" || d.GetModelVersionId() == version) {
				return e.Endpoint.GetName()
			}
		}
	}
	return ""
}

func (r *registry) delete(name string) error {
	p, l, id, ref, err := parseModelName("name", name)
	if err != nil {
		return err
	}
	if ref != "" {
		return apierror.InvalidArgument("name %q names a version; use DeleteModelVersion", name)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	m, err := r.model(p, l, id)
	if err != nil {
		return err
	}
	if e := r.deployedFrom(p, l, id, ""); e != "" {
		return apierror.FailedPrecondition("Model %s is deployed on endpoint %s; undeploy it before deleting the model", m.name(), e)
	}
	delete(r.models, key3(p, l, id))
	r.save()
	return nil
}

func (r *registry) deleteVersion(name string) error {
	p, l, id, ref, err := parseModelName("name", name)
	if err != nil {
		return err
	}
	if ref == "" {
		return apierror.InvalidArgument("name %q must name a version: models/{model}@{version}", name)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	m, err := r.model(p, l, id)
	if err != nil {
		return err
	}
	v, err := m.version(ref)
	if err != nil {
		return err
	}
	if len(m.Versions) == 1 {
		return apierror.FailedPrecondition("%s@%s is the model's only version; use DeleteModel to delete the model", m.name(), v.GetVersionId())
	}
	if contains(v.GetVersionAliases(), "default") {
		return apierror.FailedPrecondition("%s@%s is the default version and cannot be deleted; move the default alias to another "+
			"version first (MergeVersionAliases)", m.name(), v.GetVersionId())
	}
	if e := r.deployedFrom(p, l, id, v.GetVersionId()); e != "" {
		return apierror.FailedPrecondition("%s@%s is deployed on endpoint %s; undeploy it before deleting the version", m.name(), v.GetVersionId(), e)
	}
	var keep []*aiplatformpb.Model
	for _, o := range m.Versions {
		if o != v {
			keep = append(keep, o)
		}
	}
	m.Versions = keep
	r.save()
	return nil
}

// ---- evaluations

func (r *registry) importEvaluation(parent string, in *aiplatformpb.ModelEvaluation) (*aiplatformpb.ModelEvaluation, error) {
	p, l, id, ref, err := parseModelName("parent", parent)
	if err != nil {
		return nil, err
	}
	if in == nil {
		return nil, apierror.InvalidArgument("model_evaluation is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	m, err := r.model(p, l, id)
	if err != nil {
		return nil, err
	}
	if _, err := m.version(ref); err != nil {
		return nil, err
	}
	e := proto.Clone(in).(*aiplatformpb.ModelEvaluation)
	e.Name = m.name() + "/evaluations/" + regNumericID()
	e.CreateTime = timestamppb.New(r.now())
	m.Evaluations = append(m.Evaluations, e)
	r.save()
	return e, nil
}

func (r *registry) evaluation(name string) (*aiplatformpb.ModelEvaluation, error) {
	model, _, ok := strings.Cut(name, "/evaluations/")
	if !ok {
		return nil, apierror.InvalidArgument("name %q is not projects/{p}/locations/{l}/models/{m}/evaluations/{e}", name)
	}
	p, l, id, _, err := parseModelName("name", model)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	m, err := r.model(p, l, id)
	if err != nil {
		return nil, err
	}
	for _, e := range m.Evaluations {
		if e.GetName() == name {
			return e, nil
		}
	}
	return nil, apierror.NotFound("ModelEvaluation %s not found", name)
}

func (r *registry) evaluations(parent string) ([]*aiplatformpb.ModelEvaluation, error) {
	p, l, id, _, err := parseModelName("parent", parent)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	m, err := r.model(p, l, id)
	if err != nil {
		return nil, err
	}
	return append([]*aiplatformpb.ModelEvaluation(nil), m.Evaluations...), nil
}

// ---- endpoints

func (r *registry) endpoint(project, location, id string) (*regEndpoint, error) {
	e, ok := r.endpoints[key3(project, location, id)]
	if !ok {
		return nil, apierror.NotFound("Endpoint projects/%s/locations/%s/endpoints/%s not found", project, location, id)
	}
	return e, nil
}

func (r *registry) hasEndpoint(project, location, id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.endpoints[key3(project, location, id)]
	return ok
}

func (r *registry) createEndpoint(parent, id string, in *aiplatformpb.Endpoint) (*aiplatformpb.Endpoint, error) {
	project, location, err := parseParent("parent", parent)
	if err != nil {
		return nil, err
	}
	if in == nil {
		return nil, apierror.InvalidArgument("endpoint is required")
	}
	if err := checkDisplayName("endpoint.display_name", in.GetDisplayName()); err != nil {
		return nil, err
	}
	if err := checkLabels(in.GetLabels()); err != nil {
		return nil, err
	}
	if len(in.GetDeployedModels()) > 0 || len(in.GetTrafficSplit()) > 0 {
		return nil, apierror.InvalidArgument("endpoint.deployed_models and traffic_split are set by DeployModel, not CreateEndpoint")
	}
	if id == "" {
		id = regNumericID()
	} else if !endpointIDPattern.MatchString(id) {
		return nil, apierror.InvalidArgument("endpoint_id %q is invalid: up to 10 digits, or up to 63 lowercase letters, digits and "+
			"hyphens starting with a letter and not ending with a hyphen", id)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.endpoints[key3(project, location, id)]; ok {
		return nil, apierror.AlreadyExists("Endpoint projects/%s/locations/%s/endpoints/%s already exists", project, location, id)
	}
	now := timestamppb.New(r.now())
	e := proto.Clone(in).(*aiplatformpb.Endpoint)
	e.Name = "projects/" + project + "/locations/" + location + "/endpoints/" + id
	e.CreateTime, e.UpdateTime = now, now
	e.Etag = etagOf(now)
	if e.Labels == nil {
		e.Labels = map[string]string{}
	}
	r.endpoints[key3(project, location, id)] = &regEndpoint{Project: project, Location: location, ID: id, Endpoint: e, Serving: map[string]*serving{}}
	r.save()
	return proto.Clone(e).(*aiplatformpb.Endpoint), nil
}

func etagOf(t *timestamppb.Timestamp) string {
	h := fnv.New64a()
	_, _ = fmt.Fprint(h, t.AsTime().UnixNano())
	return fmt.Sprintf("%x", h.Sum64())
}

func (r *registry) getEndpoint(name string) (*aiplatformpb.Endpoint, error) {
	p, l, id, err := parseChild("name", "endpoints", name)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	e, err := r.endpoint(p, l, id)
	if err != nil {
		return nil, err
	}
	return proto.Clone(e.Endpoint).(*aiplatformpb.Endpoint), nil
}

func (r *registry) listEndpoints(project, location string) []*aiplatformpb.Endpoint {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*aiplatformpb.Endpoint
	for _, e := range sortedEndpoints(r.endpoints) {
		if e.Project == project && (location == "-" || e.Location == location) {
			out = append(out, proto.Clone(e.Endpoint).(*aiplatformpb.Endpoint))
		}
	}
	return out
}

// checkTraffic checks a traffic split against the deployed model IDs.
func checkTraffic(split map[string]int32, ids map[string]bool) error {
	if len(split) == 0 {
		return nil
	}
	total := int32(0)
	for id, pct := range split {
		if !ids[id] {
			return apierror.InvalidArgument("traffic_split names deployed model %q, which is not deployed on the endpoint", id)
		}
		if pct < 0 || pct > 100 {
			return apierror.InvalidArgument("traffic_split[%q] is %d; a percentage is in [0, 100]", id, pct)
		}
		total += pct
	}
	if total != 100 {
		return apierror.InvalidArgument("traffic_split percentages add up to %d; they must add up to 100", total)
	}
	return nil
}

func deployedIDs(e *aiplatformpb.Endpoint) map[string]bool {
	ids := map[string]bool{}
	for _, d := range e.GetDeployedModels() {
		ids[d.GetId()] = true
	}
	return ids
}

func (r *registry) updateEndpoint(in *aiplatformpb.Endpoint, paths []string) (*aiplatformpb.Endpoint, error) {
	p, l, id, err := parseChild("endpoint.name", "endpoints", in.GetName())
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return nil, apierror.InvalidArgument("update_mask is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	e, err := r.endpoint(p, l, id)
	if err != nil {
		return nil, err
	}
	if in.GetEtag() != "" && in.GetEtag() != e.Endpoint.GetEtag() {
		return nil, apierror.Aborted("endpoint.etag %q does not match the endpoint's current etag", in.GetEtag())
	}
	next := proto.Clone(e.Endpoint).(*aiplatformpb.Endpoint)
	for _, path := range paths {
		switch path {
		case "display_name", "displayName":
			if err := checkDisplayName("endpoint.display_name", in.GetDisplayName()); err != nil {
				return nil, err
			}
			next.DisplayName = in.GetDisplayName()
		case "description":
			next.Description = in.GetDescription()
		case "labels":
			if err := checkLabels(in.GetLabels()); err != nil {
				return nil, err
			}
			next.Labels = in.GetLabels()
		case "traffic_split", "trafficSplit":
			if err := checkTraffic(in.GetTrafficSplit(), deployedIDs(next)); err != nil {
				return nil, err
			}
			if len(in.GetTrafficSplit()) == 0 && len(next.GetDeployedModels()) > 0 {
				next.TrafficSplit = nil
			} else {
				next.TrafficSplit = in.GetTrafficSplit()
			}
		default:
			return nil, apierror.InvalidArgument("update_mask path %q is not updatable; the updatable fields are display_name, "+
				"description, labels and traffic_split", path)
		}
	}
	now := timestamppb.New(r.now())
	next.UpdateTime, next.Etag = now, etagOf(now)
	e.Endpoint = next
	r.save()
	return proto.Clone(next).(*aiplatformpb.Endpoint), nil
}

func (r *registry) deleteEndpoint(name string) error {
	p, l, id, err := parseChild("name", "endpoints", name)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	e, err := r.endpoint(p, l, id)
	if err != nil {
		return err
	}
	if len(e.Endpoint.GetDeployedModels()) > 0 {
		return apierror.FailedPrecondition("Endpoint %s has %d deployed model(s); undeploy them before deleting the endpoint",
			name, len(e.Endpoint.GetDeployedModels()))
	}
	delete(r.endpoints, key3(p, l, id))
	r.save()
	return nil
}

// workloadID is the cluster workload a deployed model runs as: a Cloud Run
// service ID (at most 49 characters, starting with a letter).
func workloadID(endpointName, deployedModelID string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(endpointName))
	return fmt.Sprintf("aip-dm-%s-%08x", deployedModelID, h.Sum32())
}

// prepareDeploy validates a DeployModel request and records the deployed
// model, without traffic; deploy then runs its container.
func (r *registry) prepareDeploy(req *aiplatformpb.DeployModelRequest) (*regEndpoint, *aiplatformpb.DeployedModel, ContainerRun, error) {
	p, l, id, err := parseChild("endpoint", "endpoints", req.GetEndpoint())
	if err != nil {
		return nil, nil, ContainerRun{}, err
	}
	in := req.GetDeployedModel()
	if in == nil {
		return nil, nil, ContainerRun{}, apierror.InvalidArgument("deployed_model is required")
	}
	if r.runner == nil {
		return nil, nil, ContainerRun{}, apierror.Unimplemented("deploying models is not served on this instance: deployed models run " +
			"in the cluster, through Cloud Run (start with --services including run)")
	}
	mp, ml, mid, ref, err := parseModelName("deployed_model.model", in.GetModel())
	if err != nil {
		return nil, nil, ContainerRun{}, err
	}
	if mp != p || ml != l {
		return nil, nil, ContainerRun{}, apierror.InvalidArgument("deployed_model.model %q is not in the endpoint's project and location", in.GetModel())
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	e, err := r.endpoint(p, l, id)
	if err != nil {
		return nil, nil, ContainerRun{}, err
	}
	m, err := r.model(mp, ml, mid)
	if err != nil {
		return nil, nil, ContainerRun{}, err
	}
	v, err := m.version(ref)
	if err != nil {
		return nil, nil, ContainerRun{}, err
	}
	dmID := in.GetId()
	if dmID == "" {
		dmID = strconv.FormatInt(mrand.Int64N(9e9)+1e9, 10)
	} else if !deployedModelIDPattern.MatchString(dmID) {
		return nil, nil, ContainerRun{}, apierror.InvalidArgument("deployed_model.id %q is invalid: 1 to 10 digits", dmID)
	}
	if deployedIDs(e.Endpoint)[dmID] {
		return nil, nil, ContainerRun{}, apierror.AlreadyExists("deployed model %s already exists on endpoint %s", dmID, req.GetEndpoint())
	}
	if _, pending := e.Serving[dmID]; pending {
		return nil, nil, ContainerRun{}, apierror.AlreadyExists("deployed model %s is being deployed on endpoint %s", dmID, req.GetEndpoint())
	}
	split := req.GetTrafficSplit()
	if len(split) > 0 {
		ids := deployedIDs(e.Endpoint)
		ids["0"] = true
		if err := checkTraffic(split, ids); err != nil {
			return nil, nil, ContainerRun{}, err
		}
	}
	res := in.GetDedicatedResources()
	minR, maxR := int(res.GetMinReplicaCount()), int(res.GetMaxReplicaCount())
	if a := in.GetAutomaticResources(); a != nil {
		minR, maxR = int(a.GetMinReplicaCount()), int(a.GetMaxReplicaCount())
	}
	if minR < 1 {
		minR = 1
	}
	if maxR < minR {
		maxR = minR
	}
	machine := res.GetMachineSpec().GetMachineType()
	if res == nil && in.GetAutomaticResources() == nil {
		return nil, nil, ContainerRun{}, apierror.InvalidArgument("deployed_model.dedicated_resources is required for a model with a " +
			"custom or prebuilt container (supported_deployment_resources_types is DEDICATED_RESOURCES)")
	}
	if res != nil && machine == "" {
		return nil, nil, ContainerRun{}, apierror.InvalidArgument("deployed_model.dedicated_resources.machine_spec.machine_type is required")
	}

	spec := v.GetContainerSpec()
	if err := r.checkPrebuilt(spec.GetImageUri()); err != nil {
		return nil, nil, ContainerRun{}, err
	}
	port := 8080
	if ps := spec.GetPorts(); len(ps) > 0 {
		port = int(ps[0].GetContainerPort())
	}
	epID := id
	health, predict := spec.GetHealthRoute(), spec.GetPredictRoute()
	if health == "" {
		health = "/v1/endpoints/" + epID + "/deployedModels/" + dmID
	}
	if predict == "" {
		predict = "/v1/endpoints/" + epID + "/deployedModels/" + dmID + ":predict"
	}
	run := ContainerRun{ID: workloadID(e.Endpoint.GetName(), dmID), Image: spec.GetImageUri(), Command: spec.GetCommand(),
		Args: spec.GetArgs(), Port: port, HealthRoute: health, PredictRoute: predict, MinReplicas: minR, MaxReplicas: maxR,
		Labels: map[string]string{"cloudburrow-vertex-endpoint": epID, "cloudburrow-vertex-deployed-model": dmID}}
	for _, ev := range spec.GetEnv() {
		run.Env = append(run.Env, EnvPair{ev.GetName(), ev.GetValue()})
	}
	run.Env = append(run.Env,
		EnvPair{"AIP_STORAGE_URI", r.storageURI(mp, ml, mid, v.GetVersionId())},
		EnvPair{"AIP_HTTP_PORT", strconv.Itoa(port)},
		EnvPair{"AIP_HEALTH_ROUTE", health},
		EnvPair{"AIP_PREDICT_ROUTE", predict},
		EnvPair{"AIP_MODE", "PREDICTION"},
		EnvPair{"AIP_MODE_VERSION", "1.0.0"},
		EnvPair{"AIP_MODEL_NAME", epID},
		EnvPair{"AIP_DEPLOYED_MODEL_ID", dmID},
		EnvPair{"AIP_ENDPOINT_ID", epID},
		EnvPair{"AIP_PROJECT_NUMBER", p},
		EnvPair{"AIP_MACHINE_TYPE", machine},
		EnvPair{"AIP_FRAMEWORK", "CUSTOM_CONTAINER"},
		EnvPair{"AIP_VERSION_NAME", dmID})
	if at := res.GetMachineSpec().GetAcceleratorType(); at != aiplatformpb.AcceleratorType_ACCELERATOR_TYPE_UNSPECIFIED {
		run.Env = append(run.Env, EnvPair{"AIP_ACCELERATOR_TYPE", at.String()})
	}

	dm := proto.Clone(in).(*aiplatformpb.DeployedModel)
	dm.Id, dm.Model, dm.ModelVersionId = dmID, m.name(), v.GetVersionId()
	if dm.DisplayName == "" {
		dm.DisplayName = v.GetDisplayName()
	}
	dm.CreateTime = timestamppb.New(r.now())
	if res != nil {
		dr := proto.Clone(res).(*aiplatformpb.DedicatedResources)
		dr.MinReplicaCount, dr.MaxReplicaCount = int32(minR), int32(maxR)
		dm.PredictionResources = &aiplatformpb.DeployedModel_DedicatedResources{DedicatedResources: dr}
	}
	e.Serving[dmID] = &serving{Workload: run.ID, PredictRoute: predict, HealthRoute: health, Model: m.name() + "@" + v.GetVersionId()}
	return e, dm, run, nil
}

// deploy runs a prepared deployed model's container and, once it is
// healthy, adds it to the endpoint with the request's traffic split.
func (r *registry) deploy(ctx context.Context, req *aiplatformpb.DeployModelRequest, e *regEndpoint, dm *aiplatformpb.DeployedModel, run ContainerRun) (*aiplatformpb.DeployedModel, error) {
	url, err := r.runner.Run(ctx, e.Project, run)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.endpoints[key3(e.Project, e.Location, e.ID)] != e {
		return nil, apierror.Aborted("the endpoint was deleted while the model was being deployed")
	}
	if err != nil {
		delete(e.Serving, dm.GetId())
		r.save()
		go func() { _ = r.runner.Remove(context.Background(), e.Project, run.ID) }()
		return nil, err
	}
	e.Serving[dm.GetId()].URL = url
	next := proto.Clone(e.Endpoint).(*aiplatformpb.Endpoint)
	next.DeployedModels = append(next.DeployedModels, dm)
	if split := req.GetTrafficSplit(); len(split) > 0 {
		next.TrafficSplit = map[string]int32{}
		for id, pct := range split {
			if id == "0" {
				id = dm.GetId()
			}
			if pct > 0 {
				next.TrafficSplit[id] = pct
			}
		}
	} else if len(next.GetTrafficSplit()) == 0 {
		// The first deployed model takes all the traffic, as gcloud's
		// default --traffic-split=0=100 does.
		next.TrafficSplit = map[string]int32{dm.GetId(): 100}
	}
	now := timestamppb.New(r.now())
	next.UpdateTime, next.Etag = now, etagOf(now)
	e.Endpoint = next
	r.save()
	return proto.Clone(dm).(*aiplatformpb.DeployedModel), nil
}

// undeploy removes a deployed model and its workload.
func (r *registry) undeploy(ctx context.Context, req *aiplatformpb.UndeployModelRequest) error {
	p, l, id, err := parseChild("endpoint", "endpoints", req.GetEndpoint())
	if err != nil {
		return err
	}
	dmID := req.GetDeployedModelId()
	if dmID == "" {
		return apierror.InvalidArgument("deployed_model_id is required")
	}
	r.mu.Lock()
	e, err := r.endpoint(p, l, id)
	if err != nil {
		r.mu.Unlock()
		return err
	}
	ids := deployedIDs(e.Endpoint)
	if !ids[dmID] {
		r.mu.Unlock()
		return apierror.NotFound("deployed model %s is not deployed on endpoint %s", dmID, req.GetEndpoint())
	}
	next := proto.Clone(e.Endpoint).(*aiplatformpb.Endpoint)
	var keep []*aiplatformpb.DeployedModel
	for _, d := range next.GetDeployedModels() {
		if d.GetId() != dmID {
			keep = append(keep, d)
		}
	}
	next.DeployedModels = keep
	if split := req.GetTrafficSplit(); len(split) > 0 {
		delete(ids, dmID)
		if err := checkTraffic(split, ids); err != nil {
			r.mu.Unlock()
			return err
		}
		next.TrafficSplit = split
	} else if len(keep) == 0 {
		next.TrafficSplit = nil
	} else if next.GetTrafficSplit()[dmID] > 0 {
		r.mu.Unlock()
		return apierror.FailedPrecondition("deployed model %s receives %d%% of endpoint %s's traffic; set traffic_split to "+
			"move it to the other deployed models before undeploying it", dmID, next.GetTrafficSplit()[dmID], req.GetEndpoint())
	} else {
		delete(next.TrafficSplit, dmID)
	}
	workload := ""
	if s := e.Serving[dmID]; s != nil {
		workload = s.Workload
	}
	delete(e.Serving, dmID)
	now := timestamppb.New(r.now())
	next.UpdateTime, next.Etag = now, etagOf(now)
	e.Endpoint = next
	r.save()
	r.mu.Unlock()
	if workload != "" && r.runner != nil {
		if err := r.runner.Remove(ctx, p, workload); err != nil {
			return apierror.Internal(err, "removing deployed model %s's workload %s: %v", dmID, workload, err)
		}
	}
	return nil
}

// mutate updates a deployed model's mutable fields: its replica counts,
// display name is not mutable on Vertex, so only the fields Google names.
func (r *registry) mutate(req *aiplatformpb.MutateDeployedModelRequest, paths []string) (*aiplatformpb.DeployedModel, error) {
	p, l, id, err := parseChild("endpoint", "endpoints", req.GetEndpoint())
	if err != nil {
		return nil, err
	}
	in := req.GetDeployedModel()
	if in.GetId() == "" {
		return nil, apierror.InvalidArgument("deployed_model.id is required")
	}
	if len(paths) == 0 {
		return nil, apierror.InvalidArgument("update_mask is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	e, err := r.endpoint(p, l, id)
	if err != nil {
		return nil, err
	}
	next := proto.Clone(e.Endpoint).(*aiplatformpb.Endpoint)
	var dm *aiplatformpb.DeployedModel
	for _, d := range next.GetDeployedModels() {
		if d.GetId() == in.GetId() {
			dm = d
		}
	}
	if dm == nil {
		return nil, apierror.NotFound("deployed model %s is not deployed on endpoint %s", in.GetId(), req.GetEndpoint())
	}
	for _, path := range paths {
		switch path {
		case "dedicated_resources.min_replica_count", "dedicatedResources.minReplicaCount",
			"dedicated_resources.max_replica_count", "dedicatedResources.maxReplicaCount",
			"dedicated_resources.autoscaling_metric_specs", "dedicatedResources.autoscalingMetricSpecs":
			cur := dm.GetDedicatedResources()
			src := in.GetDedicatedResources()
			if cur == nil || src == nil {
				return nil, apierror.InvalidArgument("update_mask path %q needs deployed_model.dedicated_resources", path)
			}
			switch {
			case strings.Contains(path, "min"):
				cur.MinReplicaCount = src.GetMinReplicaCount()
			case strings.Contains(path, "max"):
				cur.MaxReplicaCount = src.GetMaxReplicaCount()
			default:
				cur.AutoscalingMetricSpecs = src.GetAutoscalingMetricSpecs()
			}
			if cur.GetMinReplicaCount() < 1 || cur.GetMaxReplicaCount() < cur.GetMinReplicaCount() {
				return nil, apierror.InvalidArgument("dedicated_resources: min_replica_count %d and max_replica_count %d are invalid",
					cur.GetMinReplicaCount(), cur.GetMaxReplicaCount())
			}
		case "disable_container_logging", "disableContainerLogging":
			dm.DisableContainerLogging = in.GetDisableContainerLogging()
		case "enable_access_logging", "enableAccessLogging":
			dm.EnableAccessLogging = in.GetEnableAccessLogging()
		default:
			return nil, apierror.InvalidArgument("update_mask path %q is not mutable; MutateDeployedModel updates "+
				"dedicated_resources.min_replica_count, max_replica_count, autoscaling_metric_specs, disable_container_logging "+
				"and enable_access_logging", path)
		}
	}
	now := timestamppb.New(r.now())
	next.UpdateTime, next.Etag = now, etagOf(now)
	e.Endpoint = next
	r.save()
	return proto.Clone(dm).(*aiplatformpb.DeployedModel), nil
}

// pick chooses the deployed model a request goes to, by traffic split, or
// the one named by deployedModelID (the console's and direct predicts').
func (r *registry) pick(project, location, id string) (string, *serving, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, err := r.endpoint(project, location, id)
	if err != nil {
		return "", nil, err
	}
	split := e.Endpoint.GetTrafficSplit()
	if len(split) == 0 {
		return "", nil, apierror.FailedPrecondition("Endpoint %s has no deployed model receiving traffic: deploy a model "+
			"(DeployModel) or set its traffic_split", e.Endpoint.GetName())
	}
	ids := make([]string, 0, len(split))
	for k := range split {
		ids = append(ids, k)
	}
	sort.Strings(ids)
	n := mrand.Int32N(100)
	chosen := ids[len(ids)-1]
	for _, k := range ids {
		if n < split[k] {
			chosen = k
			break
		}
		n -= split[k]
	}
	s := e.Serving[chosen]
	if s == nil || s.URL == "" {
		return "", nil, apierror.FailedPrecondition("deployed model %s on endpoint %s is not serving", chosen, e.Endpoint.GetName())
	}
	c := *s
	return chosen, &c, nil
}

// explanationSpec is the explanation spec Explain uses on a registry
// endpoint: the deployed model's, else its model version's.
func (r *registry) explanationSpec(project, location, id, deployedModelID string) *aiplatformpb.ExplanationSpec {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, err := r.endpoint(project, location, id)
	if err != nil {
		return nil
	}
	for _, d := range e.Endpoint.GetDeployedModels() {
		if deployedModelID != "" && d.GetId() != deployedModelID {
			continue
		}
		if d.GetExplanationSpec() != nil {
			return d.GetExplanationSpec()
		}
		mp, ml, mid, _, err := parseModelName("model", d.GetModel())
		if err != nil {
			continue
		}
		if m, err := r.model(mp, ml, mid); err == nil {
			if v, err := m.version(d.GetModelVersionId()); err == nil && v.GetExplanationSpec() != nil {
				return v.GetExplanationSpec()
			}
		}
	}
	return nil
}

// exportFormats is an uploaded model's supported_export_formats: the
// "custom-trained" format, its image always and its artifacts when it has
// some.
func exportFormats(artifacts bool) []*aiplatformpb.Model_ExportFormat {
	f := &aiplatformpb.Model_ExportFormat{Id: "custom-trained",
		ExportableContents: []aiplatformpb.Model_ExportFormat_ExportableContent{aiplatformpb.Model_ExportFormat_IMAGE}}
	if artifacts {
		f.ExportableContents = append([]aiplatformpb.Model_ExportFormat_ExportableContent{aiplatformpb.Model_ExportFormat_ARTIFACT},
			f.ExportableContents...)
	}
	return []*aiplatformpb.Model_ExportFormat{f}
}

// prebuiltHosts are the registries Google's prebuilt prediction containers
// are published in: Artifact Registry's us, europe and asia multi-regions,
// under vertex-ai/prediction and vertex-ai-restricted/prediction, each
// tagged :latest (re-read 2026-11-20:
// https://docs.cloud.google.com/vertex-ai/docs/predictions/pre-built-containers).
// The older gcr.io/cloud-aiplatform paths, which that page no longer
// names, are recognised too, so they are refused rather than pulled.
var prebuiltHosts = []string{"us-docker.pkg.dev/", "europe-docker.pkg.dev/", "asia-docker.pkg.dev/"}

// IsPrebuiltImage reports whether uri is one of Google's prebuilt
// prediction (or training) container images.
func IsPrebuiltImage(uri string) bool {
	u := strings.ToLower(uri)
	for _, h := range prebuiltHosts {
		if rest, ok := strings.CutPrefix(u, h); ok {
			return strings.HasPrefix(rest, "vertex-ai/") || strings.HasPrefix(rest, "vertex-ai-restricted/")
		}
	}
	for _, h := range []string{"gcr.io/", "us.gcr.io/", "eu.gcr.io/", "asia.gcr.io/"} {
		if strings.HasPrefix(u, h+"cloud-aiplatform/") {
			return true
		}
	}
	return false
}

// checkPrebuilt refuses to deploy one of Google's prebuilt containers that
// this instance's Artifact Registry does not hold. They are several GB
// each, in Google's registry, and nothing here downloads them: the image
// is served only when it was pushed to the local repository its path names
// (us-docker.pkg.dev/vertex-ai/prediction/IMAGE:TAG is the image IMAGE:TAG
// of the repository projects/vertex-ai/locations/us/repositories/prediction
// of this instance). A gcr.io path is never served (no Container Registry).
func (r *registry) checkPrebuilt(uri string) error {
	if !IsPrebuiltImage(uri) {
		return nil
	}
	if strings.Contains(strings.ToLower(uri), "gcr.io/") {
		return apierror.FailedPrecondition("the model's image %s is one of Google's prebuilt containers in Container Registry, which "+
			"this instance does not serve or download: use its Artifact Registry path (us-docker.pkg.dev/vertex-ai/prediction/...), "+
			"pushed to this instance's Artifact Registry, or a serving image of your own", uri)
	}
	if r.images != nil && r.images.Held(uri) {
		return nil
	}
	return apierror.FailedPrecondition("the model's image %s is one of Google's prebuilt prediction containers, which this instance "+
		"does not download (each is several GB, from Google's registry): push that image, or one serving the same contract, to the "+
		"repository its path names in this instance's Artifact Registry (create the DOCKER repository in project vertex-ai at that "+
		"location, then docker push it there), or upload the model with a serving image of your own", uri)
}
