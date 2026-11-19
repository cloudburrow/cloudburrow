package modelgarden

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/hfmodels"
)

// Deployed-model states. Google's console shows Deploying, then Ready.
const (
	StateDeploying = "DEPLOYING"
	StateReady     = "READY"
	StateFailed    = "FAILED"
)

// Endpoint is an Endpoint resource.
type Endpoint struct {
	Project     string            `json:"project"`
	Location    string            `json:"location"`
	ID          string            `json:"id"`
	DisplayName string            `json:"displayName"`
	Dedicated   bool              `json:"dedicated"`
	Labels      map[string]string `json:"labels,omitempty"`
	Created     time.Time         `json:"created"`
	Updated     time.Time         `json:"updated"`
	// Deployed is the endpoint's one deployed model, nil once undeployed.
	Deployed *DeployedModel `json:"deployed,omitempty"`
}

// Name is the endpoint's resource name.
func (e *Endpoint) Name() string {
	return "projects/" + e.Project + "/locations/" + e.Location + "/endpoints/" + e.ID
}

// DeployedModel is a model served on an endpoint by one engine container.
type DeployedModel struct {
	ID          string    `json:"id"`
	ModelID     string    `json:"modelId"`
	DisplayName string    `json:"displayName"`
	MachineType string    `json:"machineType"`
	Container   string    `json:"container"`
	HostPort    int       `json:"hostPort"`
	State       string    `json:"state"`
	Message     string    `json:"message,omitempty"`
	Progress    int       `json:"progress"` // percent, while deploying
	Created     time.Time `json:"created"`
}

// Model is a Model registry entry.
type Model struct {
	Project     string            `json:"project"`
	Location    string            `json:"location"`
	ID          string            `json:"id"`
	DisplayName string            `json:"displayName"`
	HFModelID   string            `json:"hfModelId"`
	Revision    string            `json:"revision"`
	License     string            `json:"license"`
	PipelineTag string            `json:"pipelineTag"`
	Engine      string            `json:"engine"`
	Image       string            `json:"image"`
	Args        []string          `json:"args"`
	Env         map[string]string `json:"env"`
	Created     time.Time         `json:"created"`
	Updated     time.Time         `json:"updated"`
}

// Name is the model's resource name.
func (m *Model) Name() string {
	return "projects/" + m.Project + "/locations/" + m.Location + "/models/" + m.ID
}

// PublisherModel is Model Garden's name for the Hugging Face model:
// publishers/hf-{author}/models/{name}@001 (DeployRequest.publisher_model_name).
func PublisherModel(hfID string) string {
	author, name, _ := strings.Cut(strings.ToLower(hfID), "/")
	return "publishers/hf-" + author + "/models/" + name + "@001"
}

// Request is a Deploy request, already decoded from either protocol.
type Request struct {
	Project, Location   string
	HFModelID           string
	AcceptEULA          bool
	TokenSet            bool // hugging_face_access_token was set; it is refused, never kept
	ModelDisplayName    string
	ModelUserID         string
	EndpointDisplayName string
	EndpointUserID      string
	DedicatedDisabled   bool
	MachineType         string
	Labels              map[string]string
}

// Prepared is a validated deploy whose resources exist, ready to Run.
type Prepared struct {
	Endpoint, Model, DeployedModel string // IDs
	PublisherModel                 string
	EndpointName, ModelName        string
}

// Docker runs engine containers.
type Docker interface {
	Run(ctx context.Context, spec RunSpec) error
	HostPort(ctx context.Context, name string, port int) (int, error)
	Running(ctx context.Context, name string) bool
	Start(ctx context.Context, name string) error
	Stop(ctx context.Context, name string) error
	Remove(ctx context.Context, name string) error
	Logs(ctx context.Context, name string, lines int) string
}

// RunSpec is one engine container.
type RunSpec struct {
	Name, Image, Platform, ShmSize string
	Labels, Env                    map[string]string
	ModelDir                       string
	Port                           int
	Args                           []string
}

// Manager keeps the Model Garden deployments of one instance.
type Manager struct {
	Instance  string
	StateFile string
	Models    *hfmodels.Manager
	Docker    Docker
	Client    *http.Client
	// ReadyTimeout bounds an engine's start; 20 minutes when zero.
	ReadyTimeout time.Duration
	now          func() time.Time

	mu        sync.Mutex
	loaded    bool
	endpoints map[string]*Endpoint
	models    map[string]*Model
}

type persisted struct {
	Endpoints []*Endpoint `json:"endpoints"`
	Models    []*Model    `json:"models"`
}

func (m *Manager) clock() time.Time {
	if m.now != nil {
		return m.now()
	}
	return time.Now().UTC()
}

func (m *Manager) client() *http.Client {
	if m.Client != nil {
		return m.Client
	}
	return &http.Client{}
}

func key(project, location, id string) string { return project + "/" + location + "/" + id }

// load reads the state file once; the caller holds mu.
func (m *Manager) load() {
	if m.loaded {
		return
	}
	m.loaded = true
	m.endpoints, m.models = map[string]*Endpoint{}, map[string]*Model{}
	b, err := os.ReadFile(m.StateFile)
	if err != nil {
		return
	}
	var p persisted
	if json.Unmarshal(b, &p) != nil {
		return
	}
	for _, e := range p.Endpoints {
		m.endpoints[key(e.Project, e.Location, e.ID)] = e
	}
	for _, x := range p.Models {
		m.models[key(x.Project, x.Location, x.ID)] = x
	}
}

// save writes the state file; the caller holds mu.
func (m *Manager) save() {
	if m.StateFile == "" {
		return
	}
	p := persisted{Endpoints: []*Endpoint{}, Models: []*Model{}}
	for _, e := range m.endpoints {
		p.Endpoints = append(p.Endpoints, e)
	}
	for _, x := range m.models {
		p.Models = append(p.Models, x)
	}
	sort.Slice(p.Endpoints, func(i, j int) bool { return p.Endpoints[i].ID < p.Endpoints[j].ID })
	sort.Slice(p.Models, func(i, j int) bool { return p.Models[i].ID < p.Models[j].ID })
	b, _ := json.MarshalIndent(p, "", "  ")
	_ = os.MkdirAll(filepath.Dir(m.StateFile), 0o755)
	tmp := m.StateFile + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, m.StateFile)
	}
}

// numericID is a 19-digit ID, the shape of Vertex's endpoint and model IDs.
func numericID() string {
	n, _ := rand.Int(rand.Reader, big.NewInt(8e18))
	return fmt.Sprintf("%d", n.Int64()+1e18)
}

// userIDPattern is Vertex's endpoint_user_id and model_user_id rule: up to
// 63 characters, lowercase letters, digits and hyphens, starting with a
// letter (or all digits, up to 10, for an endpoint).
var userIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

func checkUserID(field, id string) error {
	if id == "" || userIDPattern.MatchString(id) || (len(id) <= 10 && strings.Trim(id, "0123456789") == "") {
		return nil
	}
	return apierror.InvalidArgument("%s %q is invalid: it must be at most 63 characters of lowercase letters, digits and hyphens, "+
		"starting with a letter", field, id)
}

// TokenRefusal is the answer to a request carrying hugging_face_access_token.
const TokenRefusal = "model_config.hugging_face_access_token is not accepted by CloudBurrow: a token sent in a request could be " +
	"logged or stored, so it is refused and was not kept. Set HF_TOKEN in the environment CloudBurrow runs in, restart it, and " +
	"deploy without the token; gated models are then downloaded with it"

// Prepare validates a deploy and creates its Endpoint and Model, with the
// DeployedModel in DEPLOYING; Run then brings the engine up.
func (m *Manager) Prepare(ctx context.Context, r Request) (Prepared, error) {
	if r.TokenSet {
		return Prepared{}, apierror.InvalidArgument("%s", TokenRefusal)
	}
	if r.HFModelID == "" {
		return Prepared{}, apierror.InvalidArgument("hugging_face_model_id is required: CloudBurrow deploys Hugging Face models; " +
			"publisher_model_name (Google's own Model Garden models) is not served")
	}
	if err := hfmodels.CheckID(r.HFModelID); err != nil {
		return Prepared{}, apierror.InvalidArgument("hugging_face_model_id %q is not a Hugging Face model ID ({author}/{model})", r.HFModelID)
	}
	if err := checkUserID("endpoint_config.endpoint_user_id", r.EndpointUserID); err != nil {
		return Prepared{}, err
	}
	if err := checkUserID("model_config.model_user_id", r.ModelUserID); err != nil {
		return Prepared{}, err
	}
	if m.Models == nil {
		return Prepared{}, apierror.FailedPrecondition("Hugging Face models are not available on this instance")
	}
	_, info, ok := m.Models.Downloaded(r.HFModelID)
	if !ok {
		var err error
		if info, err = m.Models.Lookup(ctx, r.HFModelID); err != nil {
			return Prepared{}, lookupError(r.HFModelID, err)
		}
	}
	if info.Gated != "" && !r.AcceptEULA {
		return Prepared{}, apierror.FailedPrecondition("%s is gated on Hugging Face and needs its licence accepted: set "+
			"model_config.accept_eula to true (and accept it on huggingface.co/%s, with HF_TOKEN set where CloudBurrow runs)",
			r.HFModelID, r.HFModelID)
	}
	eng, err := EngineFor(info.PipelineTag)
	if err != nil {
		return Prepared{}, apierror.InvalidArgument("cannot deploy %s: %v", r.HFModelID, err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.load()
	epID, modelID := r.EndpointUserID, r.ModelUserID
	if epID == "" {
		epID = numericID()
	}
	if modelID == "" {
		modelID = numericID()
	}
	if _, ok := m.endpoints[key(r.Project, r.Location, epID)]; ok {
		return Prepared{}, apierror.AlreadyExists("endpoint projects/%s/locations/%s/endpoints/%s already exists", r.Project, r.Location, epID)
	}
	if _, ok := m.models[key(r.Project, r.Location, modelID)]; ok {
		return Prepared{}, apierror.AlreadyExists("model projects/%s/locations/%s/models/%s already exists", r.Project, r.Location, modelID)
	}
	base := strings.ToLower(strings.ReplaceAll(r.HFModelID[strings.Index(r.HFModelID, "/")+1:], ".", "-"))
	now := m.clock()
	modelName := r.ModelDisplayName
	if modelName == "" {
		// Model Garden's generated names, e.g. gemma2-2b-it-mg-one-click-deploy.
		modelName = base + "-mg-one-click-deploy"
	}
	epName := r.EndpointDisplayName
	if epName == "" {
		epName = base + "-mg-one-click-deploy"
	}
	machine := r.MachineType
	if machine == "" {
		machine = "local-cpu"
	}
	model := &Model{Project: r.Project, Location: r.Location, ID: modelID, DisplayName: modelName, HFModelID: r.HFModelID,
		Revision: info.Revision, License: info.License, PipelineTag: info.PipelineTag, Engine: eng.Name, Image: eng.Image,
		Env: eng.Env(r.HFModelID), Created: now, Updated: now}
	dmID := numericID()
	ep := &Endpoint{Project: r.Project, Location: r.Location, ID: epID, DisplayName: epName, Dedicated: !r.DedicatedDisabled,
		Labels: r.Labels, Created: now, Updated: now,
		Deployed: &DeployedModel{ID: dmID, ModelID: modelID, DisplayName: modelName, MachineType: machine,
			Container: m.containerName(epID), State: StateDeploying, Message: "Preparing", Created: now}}
	m.models[key(r.Project, r.Location, modelID)] = model
	m.endpoints[key(r.Project, r.Location, epID)] = ep
	m.save()
	return Prepared{Endpoint: epID, Model: modelID, DeployedModel: dmID, PublisherModel: PublisherModel(r.HFModelID),
		EndpointName: ep.Name(), ModelName: model.Name()}, nil
}

func lookupError(id string, err error) error {
	switch {
	case errors.Is(err, hfmodels.ErrNotFound):
		return apierror.NotFound("Hugging Face model %s was not found (or is private)", id)
	case errors.Is(err, hfmodels.ErrBadID):
		return apierror.InvalidArgument("%v", err)
	}
	return &apierror.Error{Code: 14, Reason: "backendError", Message: fmt.Sprintf("looking up %s on huggingface.co: %v", id, err)}
}

func (m *Manager) containerName(epID string) string {
	inst := m.Instance
	if inst == "" {
		inst = "default"
	}
	return "cloudburrow-" + inst + "-mg-" + epID
}

// setProgress records a deploy step; the caller must not hold mu.
func (m *Manager) setProgress(project, location, epID, msg string, pct int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ep, ok := m.endpoints[key(project, location, epID)]; ok && ep.Deployed != nil {
		ep.Deployed.Message, ep.Deployed.Progress = msg, pct
		m.save()
	}
}

// Run downloads the model if needed, starts its engine and waits until it
// answers its health route. On failure the deployed model is FAILED and
// its container removed; the Endpoint and Model stay, as on Google.
func (m *Manager) Run(ctx context.Context, project, location string, p Prepared) error {
	err := m.run(ctx, project, location, p)
	m.mu.Lock()
	ep := m.endpoints[key(project, location, p.Endpoint)]
	var container string
	if ep != nil && ep.Deployed != nil && ep.Deployed.ID == p.DeployedModel {
		if err != nil {
			ep.Deployed.State, ep.Deployed.Message = StateFailed, err.Error()
			container = ep.Deployed.Container
		} else {
			ep.Deployed.State, ep.Deployed.Message, ep.Deployed.Progress = StateReady, "Ready", 100
		}
		ep.Updated = m.clock()
		m.save()
	}
	m.mu.Unlock()
	if container != "" {
		_ = m.Docker.Remove(context.Background(), container)
	}
	return err
}

func (m *Manager) run(ctx context.Context, project, location string, p Prepared) error {
	m.mu.Lock()
	model := m.models[key(project, location, p.Model)]
	ep := m.endpoints[key(project, location, p.Endpoint)]
	if model == nil || ep == nil || ep.Deployed == nil {
		m.mu.Unlock()
		return apierror.Aborted("the deployment was deleted while it was being deployed")
	}
	hfID, container := model.HFModelID, ep.Deployed.Container
	m.mu.Unlock()

	dir, err := m.ensureDownloaded(ctx, project, location, p.Endpoint, hfID)
	if err != nil {
		return err
	}
	eng, _ := EngineByName(model.Engine)
	args := eng.Args(dir, hfID)
	m.mu.Lock()
	model.Args = args
	m.save()
	m.mu.Unlock()

	m.setProgress(project, location, p.Endpoint, "Starting the "+eng.Title+" container", 0)
	_ = m.Docker.Remove(ctx, container)
	spec := RunSpec{Name: container, Image: eng.Image, Platform: eng.Platform, ShmSize: eng.ShmSize, ModelDir: dir,
		Port: containerPort, Args: args, Env: eng.Env(hfID), Labels: map[string]string{
			"cloudburrow.instance": m.Instance, "cloudburrow.modelgarden.endpoint": p.Endpoint,
			"cloudburrow.modelgarden.model": p.Model, "cloudburrow.modelgarden.hf-model": hfID}}
	if err := m.Docker.Run(ctx, spec); err != nil {
		return apierror.FailedPrecondition("starting the %s container: %v", eng.Title, err)
	}
	return m.waitReady(ctx, project, location, p.Endpoint, container, eng)
}

func (m *Manager) ensureDownloaded(ctx context.Context, project, location, epID, hfID string) (string, error) {
	if dir, _, ok := m.Models.Downloaded(hfID); ok {
		return dir, nil
	}
	m.setProgress(project, location, epID, "Downloading "+hfID+" from Hugging Face", 0)
	if _, err := m.Models.Start(ctx, hfID); err != nil && !errors.Is(err, hfmodels.ErrBusy) {
		switch {
		case errors.Is(err, hfmodels.ErrGated), errors.Is(err, hfmodels.ErrDiskSpace):
			return "", apierror.FailedPrecondition("%v", err)
		}
		return "", lookupError(hfID, err)
	}
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			_ = m.Models.Cancel(hfID)
			return "", ctx.Err()
		case <-t.C:
		}
		st, ok := m.Models.StateOf(hfID)
		if !ok {
			return "", apierror.Aborted("the download of %s disappeared", hfID)
		}
		switch st.State {
		case "downloaded":
			dir, _, _ := m.Models.Downloaded(hfID)
			return dir, nil
		case "failed":
			return "", apierror.FailedPrecondition("downloading %s failed: %s", hfID, st.Error)
		}
		pct := 0
		if st.Bytes > 0 {
			pct = int(st.Downloaded * 100 / st.Bytes)
		}
		m.setProgress(project, location, epID, fmt.Sprintf("Downloading %s from Hugging Face (%d%%)", hfID, pct), pct)
	}
}

func (m *Manager) waitReady(ctx context.Context, project, location, epID, container string, eng Engine) error {
	timeout := m.ReadyTimeout
	if timeout == 0 {
		timeout = 20 * time.Minute
	}
	deadline := time.Now().Add(timeout)
	m.setProgress(project, location, epID, "Loading the model in "+eng.Title, 0)
	for {
		if !m.Docker.Running(ctx, container) {
			return apierror.FailedPrecondition("the %s container stopped while loading the model: %s", eng.Title,
				m.Docker.Logs(context.Background(), container, 12))
		}
		port, err := m.Docker.HostPort(ctx, container, containerPort)
		if err == nil {
			m.mu.Lock()
			if ep := m.endpoints[key(project, location, epID)]; ep != nil && ep.Deployed != nil {
				ep.Deployed.HostPort = port
			}
			m.mu.Unlock()
			if m.healthy(ctx, port, eng) {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return &apierror.Error{Code: 4, Reason: "deadlineExceeded", Message: fmt.Sprintf(
				"the %s container did not become healthy within %s: %s", eng.Title, timeout, m.Docker.Logs(context.Background(), container, 12))}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func (m *Manager) healthy(ctx context.Context, port int, eng Engine) bool {
	c, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(c, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d%s", port, eng.HealthRoute), nil)
	resp, err := m.client().Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// Restore restarts the engines of READY deployments after an instance
// restart, in the background.
func (m *Manager) Restore() {
	m.mu.Lock()
	m.load()
	var todo []*Endpoint
	for _, ep := range m.endpoints {
		if ep.Deployed != nil && ep.Deployed.State != StateFailed {
			todo = append(todo, ep)
		}
	}
	m.mu.Unlock()
	for _, ep := range todo {
		ep := ep
		go func() {
			ctx := context.Background()
			m.mu.Lock()
			model := m.models[key(ep.Project, ep.Location, ep.Deployed.ModelID)]
			dm := *ep.Deployed
			m.mu.Unlock()
			if model == nil {
				return
			}
			p := Prepared{Endpoint: ep.ID, Model: model.ID, DeployedModel: dm.ID}
			if dm.State == StateReady && m.Docker.Running(ctx, dm.Container) {
				eng, _ := EngineByName(model.Engine)
				if port, err := m.Docker.HostPort(ctx, dm.Container, containerPort); err == nil && m.healthy(ctx, port, eng) {
					return
				}
			}
			m.mu.Lock()
			ep.Deployed.State, ep.Deployed.Message = StateDeploying, "Restarting after an instance restart"
			m.mu.Unlock()
			if dm.State == StateReady && m.Docker.Start(ctx, dm.Container) == nil {
				eng, _ := EngineByName(model.Engine)
				if err := m.waitReady(ctx, ep.Project, ep.Location, ep.ID, dm.Container, eng); err == nil {
					m.mu.Lock()
					ep.Deployed.State, ep.Deployed.Message = StateReady, "Ready"
					m.save()
					m.mu.Unlock()
					return
				}
			}
			_ = m.Run(ctx, ep.Project, ep.Location, p)
		}()
	}
}

// Close removes every engine container (the instance is stopping), so
// nothing of it outlives `cloudburrow stop` or `delete`; Restore starts
// them again from the deployments file and the model cache.
func (m *Manager) Close() {
	m.mu.Lock()
	m.load()
	var names []string
	for _, ep := range m.endpoints {
		if ep.Deployed != nil {
			names = append(names, ep.Deployed.Container)
		}
	}
	m.mu.Unlock()
	for _, n := range names {
		_ = m.Docker.Remove(context.Background(), n)
	}
}

// Endpoints lists a location's endpoints, by ID.
func (m *Manager) Endpoints(project, location string) []Endpoint {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.load()
	var out []Endpoint
	for _, e := range m.endpoints {
		if e.Project == project && (location == "-" || e.Location == location) {
			out = append(out, copyEndpoint(e))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// AllEndpoints lists every endpoint of every project, for the console.
func (m *Manager) AllEndpoints() []Endpoint {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.load()
	out := []Endpoint{}
	for _, e := range m.endpoints {
		out = append(out, copyEndpoint(e))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out
}

func copyEndpoint(e *Endpoint) Endpoint {
	c := *e
	if e.Deployed != nil {
		d := *e.Deployed
		c.Deployed = &d
	}
	return c
}

// Endpoint returns one endpoint.
func (m *Manager) Endpoint(project, location, id string) (Endpoint, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.load()
	e, ok := m.endpoints[key(project, location, id)]
	if !ok {
		return Endpoint{}, apierror.NotFound("endpoint projects/%s/locations/%s/endpoints/%s not found", project, location, id)
	}
	return copyEndpoint(e), nil
}

// Has reports whether id is one of this manager's endpoints.
func (m *Manager) Has(project, location, id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.load()
	_, ok := m.endpoints[key(project, location, id)]
	return ok
}

// ModelList lists a location's models.
func (m *Manager) ModelList(project, location string) []Model {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.load()
	var out []Model
	for _, x := range m.models {
		if x.Project == project && (location == "-" || x.Location == location) {
			out = append(out, *x)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Model returns one model.
func (m *Manager) Model(project, location, id string) (Model, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.load()
	x, ok := m.models[key(project, location, id)]
	if !ok {
		return Model{}, apierror.NotFound("model projects/%s/locations/%s/models/%s not found", project, location, id)
	}
	return *x, nil
}

// DeployedOn lists the endpoints a model is deployed on.
func (m *Manager) DeployedOn(project, location, modelID string) []Endpoint {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Endpoint
	for _, e := range m.endpoints {
		if e.Project == project && e.Location == location && e.Deployed != nil && e.Deployed.ModelID == modelID {
			out = append(out, copyEndpoint(e))
		}
	}
	return out
}

// Undeploy removes the deployed model and frees its container.
func (m *Manager) Undeploy(ctx context.Context, project, location, epID, deployedModelID string) error {
	m.mu.Lock()
	m.load()
	e, ok := m.endpoints[key(project, location, epID)]
	if !ok {
		m.mu.Unlock()
		return apierror.NotFound("endpoint projects/%s/locations/%s/endpoints/%s not found", project, location, epID)
	}
	if e.Deployed == nil || (deployedModelID != "" && e.Deployed.ID != deployedModelID) {
		m.mu.Unlock()
		return apierror.NotFound("deployed model %q is not deployed on endpoint %s", deployedModelID, e.Name())
	}
	container := e.Deployed.Container
	e.Deployed, e.Updated = nil, m.clock()
	m.save()
	m.mu.Unlock()
	if err := m.Docker.Remove(ctx, container); err != nil {
		return apierror.Internal(err, "removing the engine container %s: %v", container, err)
	}
	return nil
}

// DeleteEndpoint deletes an endpoint with no deployed model.
func (m *Manager) DeleteEndpoint(project, location, epID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.load()
	e, ok := m.endpoints[key(project, location, epID)]
	if !ok {
		return apierror.NotFound("endpoint projects/%s/locations/%s/endpoints/%s not found", project, location, epID)
	}
	if e.Deployed != nil {
		return apierror.FailedPrecondition("endpoint %s has a deployed model (%s); undeploy it before deleting the endpoint", e.Name(), e.Deployed.ID)
	}
	delete(m.endpoints, key(project, location, epID))
	m.save()
	return nil
}

// DeleteModel deletes a model that is not deployed. The downloaded files
// stay in the model cache: they are deleted only on request (the console's
// Delete files, or hfmodels' Delete).
func (m *Manager) DeleteModel(project, location, modelID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.load()
	x, ok := m.models[key(project, location, modelID)]
	if !ok {
		return apierror.NotFound("model projects/%s/locations/%s/models/%s not found", project, location, modelID)
	}
	for _, e := range m.endpoints {
		if e.Project == project && e.Location == location && e.Deployed != nil && e.Deployed.ModelID == modelID {
			return apierror.FailedPrecondition("model %s is deployed on endpoint %s; undeploy it before deleting the model", x.Name(), e.Name())
		}
	}
	delete(m.models, key(project, location, modelID))
	m.save()
	return nil
}

// Serving is where a ready endpoint's engine answers.
type Serving struct {
	Engine          string
	BaseURL         string
	ServedModel     string // the Hugging Face ID, vLLM's --served-model-name
	DeployedModelID string
	ModelName       string
	ModelDisplay    string
}

// Target returns a ready endpoint's engine; NotFound when the endpoint is
// not one of this manager's.
func (m *Manager) Target(project, location, epID string) (Serving, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.load()
	e, ok := m.endpoints[key(project, location, epID)]
	if !ok {
		return Serving{}, apierror.NotFound("endpoint projects/%s/locations/%s/endpoints/%s not found", project, location, epID)
	}
	if e.Deployed == nil {
		return Serving{}, apierror.FailedPrecondition("endpoint %s has no deployed model", e.Name())
	}
	if e.Deployed.State != StateReady {
		return Serving{}, apierror.FailedPrecondition("the model on endpoint %s is %s (%s), not ready", e.Name(),
			strings.ToLower(e.Deployed.State), e.Deployed.Message)
	}
	x := m.models[key(project, location, e.Deployed.ModelID)]
	if x == nil {
		return Serving{}, apierror.FailedPrecondition("endpoint %s's model is gone", e.Name())
	}
	return Serving{Engine: x.Engine, BaseURL: fmt.Sprintf("http://127.0.0.1:%d", e.Deployed.HostPort), ServedModel: x.HFModelID,
		DeployedModelID: e.Deployed.ID, ModelName: x.Name(), ModelDisplay: x.DisplayName}, nil
}
