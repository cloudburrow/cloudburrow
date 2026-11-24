package vertexai

// Colab Enterprise runtime templates and runtimes (#2105):
// google.cloud.aiplatform.v1.NotebookService's NotebookRuntimeTemplate and
// NotebookRuntime methods, over gRPC and REST (with the REST-only
// notebookRuntimeTemplates:setDefault of the v1 discovery document). A
// runtime is a notebook runtime of the instance's (internal/nbruntime): a
// kernel container on an internal Docker network whose one peer relays to
// the instance's BigQuery, started from its template's idle shutdown and
// environment variables. RUNNING is that kernel connected; Stop deletes the
// container and keeps the runtime STOPPED; Start makes a new one (a
// runtime's Python state does not survive a stop, as on Google, whose stop
// shuts the VM down).
//
// Documented behaviour (https://docs.cloud.google.com/colab/docs/create-runtime-template,
// create-runtime, manage-runtimes, runtimes, idle-shutdown; the v1
// discovery document, revision 20261114; read 2026-11-22):
//   - display_name is required, at most 128 characters;
//   - idle_shutdown_config.idle_timeout is 10 * 60 to 1440 * 60 seconds;
//     gcloud's default is 3h, which is the console's default runtime's
//     ("Enabled, set to 180 minutes"), and applies when none is set;
//   - machine_spec defaults to e2-standard-4; data_persistent_disk_spec to
//     pd-standard and 100 GB, 10 to 65536 GB;
//   - software_config.env holds at most 100 variables;
//   - labels: at most 64, keys and values of at most 64 lowercase letters,
//     digits, underscores and hyphens;
//   - an update's mask is required and names only display_name,
//     encryption_spec.kms_key_name, software_config.env,
//     software_config.colab_image.release_name and
//     software_config.post_startup_script_config's three fields;
//   - AssignNotebookRuntime "will either returns an existing assignment or
//     generates a new one"; a second one-click runtime of a user fails
//     "One click runtime already exists.";
//   - a runtime's expiration_time is 24 hours after creation for a
//     one-click (ONE_CLICK) runtime, which is then deleted, and 6 months
//     for a user-defined one;
//   - the list filters and order_by fields are those the discovery
//     document gives.
//
// Local differences, each said where it applies: a runtime runs on this
// machine's CPU, so a template with accelerators can be made but a
// runtime from it fails to start (FAILED_PRECONDITION); the runtime has no
// route to the internet whatever network_spec.enable_internet_access says
// (the runtime's network is --internal, as every CloudBurrow container
// that runs user code is); a post-startup script from a Cloud Storage URL
// is not fetched (FAILED_PRECONDITION at start), an inline one runs with
// bash in the runtime; proxy_uri is empty (the console's notebooks reach
// the kernel through the instance).
//
// UNVERIFIED (re-read 2026-11-22: https://docs.cloud.google.com/colab/docs/manage-runtimes):
// starting a running runtime and stopping a stopped one succeed without a
// change; Upgrade succeeds with nothing to do (is_upgradable is false: the
// runtime image is the instance's); a template's ID, when given, is 1 to 63
// lowercase letters, digits and hyphens starting with a letter, and a
// generated one is numeric; a template a runtime still uses can be deleted.

import (
	"context"
	"crypto/rand"
	"encoding/json"
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
	"unicode/utf8"

	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	betapb "cloud.google.com/go/aiplatform/apiv1beta1/aiplatformpb"
	"cloud.google.com/go/iam/apiv1/iampb"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	nbpb "cloud.google.com/go/notebooks/apiv2/notebookspb"
	"google.golang.org/genproto/googleapis/api/annotations"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/aipfilter"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/iampolicy"
	"github.com/cloudburrow/cloudburrow/internal/lro"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// NotebookHost runs Colab Enterprise runtimes: the instance's notebook
// runtime manager (internal/nbruntime), adapted in cmd/cloudburrow.
type NotebookHost interface {
	// Start starts the runtime key in project and returns at once.
	Start(key, project string, o NotebookRuntimeOptions)
	// State is the runtime's state: CONNECTED, STARTING, FAILED or
	// DISCONNECTED (nbruntime's), with a message when it failed.
	State(key string) (state, message string)
	// Exec runs Python on a connected runtime and fails when it raises.
	Exec(ctx context.Context, key, code string) error
	// Stop deletes the runtime's containers.
	Stop(key string) error
	// Version is the runtime image's version.
	Version() string
}

// NotebookRuntimeOptions are a runtime's settings from its template.
type NotebookRuntimeOptions struct {
	IdleTimeout  time.Duration
	IdleDisabled bool
	Env          map[string]string
	// Token is the bearer token the runtime's clients use: the identity
	// a run executes as (runas.go), "" for the instance's own.
	Token string
}

const (
	defaultIdleTimeout = 180 * time.Minute
	minIdleTimeout     = 10 * time.Minute
	maxIdleTimeout     = 1440 * time.Minute
	runtimeStartWait   = 35 * time.Minute
	oneClickLifetime   = 24 * time.Hour
	userRuntimeLife    = 183 * 24 * time.Hour
)

var (
	notebookIDRE   = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,61}[a-z0-9])?$|^[0-9]{1,19}$`)
	labelPartRE    = regexp.MustCompile(`^[a-z0-9_-]{0,64}$`)
	labelKeyRE     = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
	envNameRE      = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	machineTypeRE  = regexp.MustCompile(`^[a-z][a-z0-9]*-[a-z0-9-]+$`)
	networkRE      = regexp.MustCompile(`^projects/[^/]+/global/networks/[^/]+$`)
	subnetworkRE   = regexp.MustCompile(`^projects/[^/]+/regions/[^/]+/subnetworks/[^/]+$`)
	notebookDisks  = map[string]bool{"pd-ssd": true, "pd-standard": true, "pd-balanced": true, "pd-extreme": true, "hyperdisk-balanced": true, "hyperdisk-extreme": true, "hyperdisk-balanced-high-availability": true, "hyperdisk-ml": true, "hyperdisk-throughput": true}
	templateMasks  = map[string]bool{"display_name": true, "encryption_spec.kms_key_name": true, "software_config.env": true, "software_config.colab_image.release_name": true, "software_config.post_startup_script_config.post_startup_script": true, "software_config.post_startup_script_config.post_startup_script_url": true, "software_config.post_startup_script_config.post_startup_script_behavior": true}
	templateFilter = append([]aipfilter.Option{
		aipfilter.Fields("notebook_runtime_template", "display_name", "notebook_runtime_type", "machine_type", "accelerator_type", "labels"),
	}, bothCases(map[string]func(proto.Message) string{
		"notebook_runtime_template": func(m proto.Message) string { return lastSegment(m.(*aiplatformpb.NotebookRuntimeTemplate).GetName()) },
	}, map[string]string{"machine_type": "machine_spec.machine_type", "accelerator_type": "machine_spec.accelerator_type"})...)
	runtimeFilter = append([]aipfilter.Option{
		aipfilter.Fields("notebook_runtime", "display_name", "notebook_runtime_template", "health_state", "runtime_state", "runtime_user", "notebook_runtime_type", "machine_type", "accelerator_type", "ui_state", "labels"),
	}, bothCases(map[string]func(proto.Message) string{
		"notebook_runtime": func(m proto.Message) string { return lastSegment(m.(*aiplatformpb.NotebookRuntime).GetName()) },
		"notebook_runtime_template": func(m proto.Message) string {
			return lastSegment(m.(*aiplatformpb.NotebookRuntime).GetNotebookRuntimeTemplateRef().GetNotebookRuntimeTemplate())
		},
		"ui_state": func(proto.Message) string { return "UI_RESOURCE_STATE_ACTIVE" },
	}, map[string]string{"machine_type": "machine_spec.machine_type", "accelerator_type": "machine_spec.accelerator_type"})...)
	notebookOrder = aipfilter.Fields("display_name", "create_time", "update_time")
)

func lastSegment(n string) string { return n[strings.LastIndexByte(n, '/')+1:] }

// bothCases registers computed fields and aliases under their snake_case
// and camelCase names, which the discovery document allows both of.
func bothCases(computed map[string]func(proto.Message) string, aliases map[string]string) []aipfilter.Option {
	var out []aipfilter.Option
	for _, k := range sortedNames(computed) {
		out = append(out, aipfilter.Computed(k, computed[k]), aipfilter.Computed(snakeToCamel(k), computed[k]))
	}
	for _, k := range sortedNames(aliases) {
		out = append(out, aipfilter.Alias(k, aliases[k]), aipfilter.Alias(snakeToCamel(k), aliases[k]))
	}
	return out
}

func snakeToCamel(s string) string {
	parts := strings.Split(s, "_")
	for i := 1; i < len(parts); i++ {
		if parts[i] != "" {
			parts[i] = strings.ToUpper(parts[i][:1]) + parts[i][1:]
		}
	}
	return strings.Join(parts, "")
}

// notebookStore holds the templates and runtimes, by name.
type notebookStore struct {
	mu        sync.Mutex
	file      string
	host      NotebookHost
	templates map[string]*aiplatformpb.NotebookRuntimeTemplate
	runtimes  map[string]*runtimeEntry
	// defaults are the default template of each project and location
	// (Set as Default), by parent.
	defaults map[string]string
	now      func() time.Time
	// exec runs notebook executions (notebookjobs.go); jobs and
	// schedules are theirs (schedules.go), policies the templates' IAM
	// policies (notebookiam.go).
	exec      NotebookExecutor
	jobs      map[string]*jobEntry
	schedules map[string]*aiplatformpb.Schedule
	policies  map[string]*iampolicy.Stored
	// owners are the email of the user who made each schedule, "" for
	// the instance's own credentials: an execution_user schedule's runs
	// run as them (runas.go).
	owners map[string]string
	// pipelines are Vertex AI pipeline jobs (pipelines.go, #2173), run on
	// pipelineRunner.
	pipelines      map[string]*pipelineEntry
	pipelineRunner PipelineRunner
	// mon is Model Monitoring's resources (modelmonitor.go,
	// mdmjobs.go, #2214), saved beside file.
	mon *monitorState
	// md is Vertex ML Metadata (metadata.go, #2205).
	md *mdStore
	// pipelineCache are pipeline tasks' cache entries by parent and cache
	// key (pipelinerun.go, #2205); pipelineSlots the per-project quotas of
	// concurrent runs and parallel tasks.
	pipelineCache map[string]json.RawMessage
	pipelineSlots map[string]chan struct{}
	// customJobs, batchJobs and trainingPipelines are Vertex AI's custom
	// training and batch prediction (customjobs.go, batchprediction.go,
	// trainingpipelines.go, #2210), run on pipelineRunner too.
	customJobs        map[string]*customJobEntry
	batchJobs         map[string]*batchJobEntry
	trainingPipelines map[string]*trainingEntry
	// hpJobs are hyperparameter tuning jobs (hptuning.go, #2210).
	hpJobs map[string]*hpJobEntry
	// nasJobs are Neural Architecture Search jobs (nasjobs.go, #2214).
	nasJobs map[string]*nasJobEntry
	// ds are Vertex AI managed datasets (datasets.go, #2210).
	ds *dsStore
}

// runtimeEntry is a runtime and what its lifecycle needs.
type runtimeEntry struct {
	RT *aiplatformpb.NotebookRuntime
	// Running is whether the runtime should be running (started, not
	// stopped); Ready that its start (and post-startup script) finished.
	Running, Ready bool
	// Ran is whether its post-startup script has run once.
	Ran     bool
	Message string
}

func (s *Server) notebooks() *notebookStore {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.nb == nil {
		s.nb = &notebookStore{templates: map[string]*aiplatformpb.NotebookRuntimeTemplate{}, runtimes: map[string]*runtimeEntry{},
			defaults: map[string]string{}, now: s.clockNow, jobs: map[string]*jobEntry{}, schedules: map[string]*aiplatformpb.Schedule{},
			policies: map[string]*iampolicy.Stored{}, owners: map[string]string{}, pipelines: map[string]*pipelineEntry{}}
	}
	if s.ops == nil {
		s.ops = lro.NewStore(nil)
	}
	return s.nb
}

// SetNotebookRuntimes runs Colab Enterprise runtimes on host and keeps the
// templates and runtimes in file. A runtime running when the instance
// stopped is STOPPED: its container went with the instance.
func (s *Server) SetNotebookRuntimes(host NotebookHost, file string) {
	st := s.notebooks()
	st.mu.Lock()
	defer st.mu.Unlock()
	st.host, st.file = host, file
	st.loadMonitoringState()
	b, err := os.ReadFile(file)
	if err != nil {
		return
	}
	var saved struct {
		Templates []json.RawMessage `json:"templates"`
		Runtimes  []struct {
			Runtime json.RawMessage `json:"runtime"`
			Ran     bool            `json:"ran"`
		} `json:"runtimes"`
		Defaults  map[string]string            `json:"defaults"`
		Jobs      []json.RawMessage            `json:"jobs"`
		Schedules []json.RawMessage            `json:"schedules"`
		Policies  map[string]*iampolicy.Stored `json:"policies"`
		Owners    map[string]string            `json:"scheduleOwners"`
		Pipelines []json.RawMessage            `json:"pipelineJobs"`
		Metadata  json.RawMessage              `json:"metadata"`
		Cache     map[string]json.RawMessage   `json:"pipelineCache"`
		Training  map[string][]json.RawMessage `json:"trainingJobs"`
		Datasets  json.RawMessage              `json:"datasets"`
		NAS       []savedNASJob                `json:"nasJobs"`
	}
	if json.Unmarshal(b, &saved) != nil {
		return
	}
	for _, raw := range saved.Templates {
		t := &aiplatformpb.NotebookRuntimeTemplate{}
		if lenient.Unmarshal(raw, t) == nil && t.GetName() != "" {
			st.templates[t.GetName()] = t
		}
	}
	for _, r := range saved.Runtimes {
		rt := &aiplatformpb.NotebookRuntime{}
		if lenient.Unmarshal(r.Runtime, rt) == nil && rt.GetName() != "" {
			rt.RuntimeState = aiplatformpb.NotebookRuntime_STOPPED
			st.runtimes[rt.GetName()] = &runtimeEntry{RT: rt, Ran: r.Ran}
		}
	}
	for k, v := range saved.Defaults {
		st.defaults[k] = v
	}
	st.loadJobs(saved.Jobs, saved.Schedules, saved.Policies)
	for k, v := range saved.Owners {
		st.owners[k] = v
	}
	st.loadPipelines(saved.Pipelines)
	st.loadMetadata(saved.Metadata)
	st.pipelineCache = saved.Cache
	st.loadTrainingJobs(saved.Training)
	st.loadNASJobs(saved.NAS)
	st.loadDatasets(saved.Datasets)
}

// loadMonitoringState reads Model Monitoring's state, which
// SetNotebookRuntimes's file has beside it; the caller holds st.mu.
func (st *notebookStore) loadMonitoringState() {
	if st.file != "" {
		st.loadMonitoring()
	}
}

// save writes the store; the caller holds st.mu.
func (st *notebookStore) save() {
	if st.file == "" {
		return
	}
	type savedRuntime struct {
		Runtime json.RawMessage `json:"runtime"`
		Ran     bool            `json:"ran"`
	}
	out := struct {
		Templates []json.RawMessage            `json:"templates"`
		Runtimes  []savedRuntime               `json:"runtimes"`
		Defaults  map[string]string            `json:"defaults"`
		Jobs      []json.RawMessage            `json:"jobs"`
		Schedules []json.RawMessage            `json:"schedules"`
		Policies  map[string]*iampolicy.Stored `json:"policies"`
		Owners    map[string]string            `json:"scheduleOwners,omitempty"`
		Pipelines []json.RawMessage            `json:"pipelineJobs,omitempty"`
		Metadata  json.RawMessage              `json:"metadata,omitempty"`
		Cache     map[string]json.RawMessage   `json:"pipelineCache,omitempty"`
		Training  map[string][]json.RawMessage `json:"trainingJobs,omitempty"`
		Datasets  json.RawMessage              `json:"datasets,omitempty"`
		NAS       []savedNASJob                `json:"nasJobs,omitempty"`
	}{NAS: st.savedNASJobs(), Datasets: st.savedDatasets(), Training: st.savedJobs(), Cache: st.pipelineCache, Owners: st.owners, Pipelines: st.savedPipelines(), Metadata: st.savedMetadata(), Templates: []json.RawMessage{}, Runtimes: []savedRuntime{}, Defaults: st.defaults, Jobs: []json.RawMessage{}, Schedules: []json.RawMessage{},
		Policies: st.policies}
	for _, n := range sortedNames(st.jobs) {
		if b, err := protojson.Marshal(st.jobs[n].Job); err == nil {
			out.Jobs = append(out.Jobs, b)
		}
	}
	for _, n := range sortedNames(st.schedules) {
		if b, err := protojson.Marshal(st.schedules[n]); err == nil {
			out.Schedules = append(out.Schedules, b)
		}
	}
	for _, n := range sortedNames(st.templates) {
		if b, err := protojson.Marshal(st.templates[n]); err == nil {
			out.Templates = append(out.Templates, b)
		}
	}
	for _, n := range sortedNames(st.runtimes) {
		if b, err := protojson.Marshal(st.runtimes[n].RT); err == nil {
			out.Runtimes = append(out.Runtimes, savedRuntime{Runtime: b, Ran: st.runtimes[n].Ran})
		}
	}
	b, _ := json.Marshal(out)
	_ = os.MkdirAll(filepath.Dir(st.file), 0o755)
	tmp := st.file + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, st.file)
	}
}

func sortedNames[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// numericID is a generated resource ID, numeric as Vertex AI's are.
func numericID() string {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
	return n.String()
}

func runtimeKey(name string) string { return "colab/" + name }

// ---- validation

func checkNotebookLabels(labels map[string]string) error {
	if len(labels) > 64 {
		return apierror.InvalidArgument("labels: at most 64 labels are allowed")
	}
	for k, v := range labels {
		if !labelKeyRE.MatchString(k) || !utf8.ValidString(k) {
			return apierror.InvalidArgument("labels: key %q must be 1 to 64 lowercase letters, digits, underscores and hyphens, starting with a letter", k)
		}
		if !labelPartRE.MatchString(v) {
			return apierror.InvalidArgument("labels: value %q of %q must be at most 64 lowercase letters, digits, underscores and hyphens", v, k)
		}
	}
	return nil
}

func checkNotebookDisplayName(field, n string) error {
	if strings.TrimSpace(n) == "" {
		return apierror.InvalidArgument("%s is required", field)
	}
	if utf8.RuneCountInString(n) > 128 {
		return apierror.InvalidArgument("%s can be up to 128 characters long", field)
	}
	return nil
}

func checkSoftware(c *aiplatformpb.NotebookSoftwareConfig) error {
	if len(c.GetEnv()) > 100 {
		return apierror.InvalidArgument("software_config.env: the maximum limit is 100 environment variables")
	}
	seen := map[string]bool{}
	for _, e := range c.GetEnv() {
		if !envNameRE.MatchString(e.GetName()) {
			return apierror.InvalidArgument("software_config.env: %q is not a valid environment variable name", e.GetName())
		}
		if seen[e.GetName()] {
			return apierror.InvalidArgument("software_config.env: %q is set more than once", e.GetName())
		}
		seen[e.GetName()] = true
	}
	if u := c.GetPostStartupScriptConfig().GetPostStartupScriptUrl(); u != "" && !strings.HasPrefix(u, "gs://") {
		return apierror.InvalidArgument("software_config.post_startup_script_config.post_startup_script_url %q must be a gs:// URI", u)
	}
	return nil
}

// checkTemplate validates a template to create and fills its defaults.
func checkTemplate(t *aiplatformpb.NotebookRuntimeTemplate) error {
	if err := checkNotebookDisplayName("notebook_runtime_template.display_name", t.GetDisplayName()); err != nil {
		return err
	}
	if err := checkNotebookLabels(t.GetLabels()); err != nil {
		return err
	}
	if t.MachineSpec == nil {
		t.MachineSpec = &aiplatformpb.MachineSpec{}
	}
	if t.MachineSpec.MachineType == "" {
		t.MachineSpec.MachineType = "e2-standard-4"
	}
	if !machineTypeRE.MatchString(t.MachineSpec.GetMachineType()) {
		return apierror.InvalidArgument("machine_spec.machine_type %q is not a machine type", t.MachineSpec.GetMachineType())
	}
	if t.MachineSpec.GetAcceleratorCount() < 0 {
		return apierror.InvalidArgument("machine_spec.accelerator_count must not be negative")
	}
	if t.DataPersistentDiskSpec == nil {
		t.DataPersistentDiskSpec = &aiplatformpb.PersistentDiskSpec{}
	}
	d := t.DataPersistentDiskSpec
	if d.DiskType == "" {
		d.DiskType = "pd-standard"
	}
	if !notebookDisks[d.GetDiskType()] {
		return apierror.InvalidArgument("data_persistent_disk_spec.disk_type %q is not one of pd-ssd, pd-standard, pd-balanced, pd-extreme, hyperdisk-balanced, hyperdisk-extreme, hyperdisk-balanced-high-availability, hyperdisk-ml or hyperdisk-throughput", d.GetDiskType())
	}
	if d.DiskSizeGb == 0 {
		d.DiskSizeGb = 100
	}
	if d.GetDiskSizeGb() < 10 || d.GetDiskSizeGb() > 65536 {
		return apierror.InvalidArgument("data_persistent_disk_spec.disk_size_gb must be from 10 to 65536")
	}
	if n := t.GetNetworkSpec(); n != nil {
		if n.GetNetwork() != "" && !networkRE.MatchString(n.GetNetwork()) {
			return apierror.InvalidArgument("network_spec.network %q must be projects/{project}/global/networks/{network}", n.GetNetwork())
		}
		if n.GetSubnetwork() != "" && !subnetworkRE.MatchString(n.GetSubnetwork()) {
			return apierror.InvalidArgument("network_spec.subnetwork %q must be projects/{project}/regions/{region}/subnetworks/{subnetwork}", n.GetSubnetwork())
		}
	}
	if i := t.GetIdleShutdownConfig(); i != nil && !i.GetIdleShutdownDisabled() {
		d := i.GetIdleTimeout().AsDuration()
		if i.GetIdleTimeout() == nil || d < minIdleTimeout || d > maxIdleTimeout {
			return apierror.InvalidArgument("idle_shutdown_config.idle_timeout is required, and the range of idle_timeout (second) is: 10 * 60 ~ 1440 * 60")
		}
	}
	if t.IdleShutdownConfig == nil {
		t.IdleShutdownConfig = &aiplatformpb.NotebookIdleShutdownConfig{IdleTimeout: durationpb.New(defaultIdleTimeout)}
	}
	if t.NotebookRuntimeType == aiplatformpb.NotebookRuntimeType_NOTEBOOK_RUNTIME_TYPE_UNSPECIFIED {
		t.NotebookRuntimeType = aiplatformpb.NotebookRuntimeType_USER_DEFINED
	}
	if err := checkSoftware(t.GetSoftwareConfig()); err != nil {
		return err
	}
	// eucDisabled is input only; bypass_actas_check is output only.
	if t.EucConfig != nil {
		t.EucConfig.BypassActasCheck = false
	}
	return nil
}

func templateEtag(t *aiplatformpb.NotebookRuntimeTemplate) string {
	return fmt.Sprintf("%x", t.GetUpdateTime().AsTime().UnixNano())
}

// view is a template as the API shows it: eucDisabled is input only.
func templateView(t *aiplatformpb.NotebookRuntimeTemplate) *aiplatformpb.NotebookRuntimeTemplate {
	out := proto.Clone(t).(*aiplatformpb.NotebookRuntimeTemplate)
	if out.EucConfig != nil {
		out.EucConfig.EucDisabled = false
	}
	return out
}

// ---- the gRPC service

type notebookService struct {
	aiplatformpb.UnimplementedNotebookServiceServer
	s *Server
}

// operation records a finished operation on resource.
func (s *Server) finished(resource string, meta proto.Message, resp proto.Message, err error) (*longrunningpb.Operation, error) {
	name := s.newOp(resource)
	_ = s.ops.SetMetadata(name, meta)
	if err != nil {
		_ = s.ops.Fail(name, err)
	} else {
		_ = s.ops.Succeed(name, resp)
	}
	op, _ := s.ops.Get(name)
	out, e := opProto(op)
	return out, grpcError(e)
}

func generic() *aiplatformpb.GenericOperationMetadata {
	now := time.Now()
	return genericMeta(now, now)
}

func (n *notebookService) CreateNotebookRuntimeTemplate(_ context.Context, req *aiplatformpb.CreateNotebookRuntimeTemplateRequest) (*longrunningpb.Operation, error) {
	if _, _, err := parseParent("parent", req.GetParent()); err != nil {
		return nil, grpcError(err)
	}
	id := req.GetNotebookRuntimeTemplateId()
	if id != "" && !notebookIDRE.MatchString(id) {
		return nil, grpcError(apierror.InvalidArgument("notebook_runtime_template_id %q must be 1 to 63 lowercase letters, digits and hyphens, starting with a letter", id))
	}
	if id == "" {
		id = numericID()
	}
	t := proto.Clone(req.GetNotebookRuntimeTemplate()).(*aiplatformpb.NotebookRuntimeTemplate)
	if req.GetNotebookRuntimeTemplate() == nil {
		return nil, grpcError(apierror.InvalidArgument("notebook_runtime_template is required"))
	}
	if err := checkTemplate(t); err != nil {
		return nil, grpcError(err)
	}
	st := n.s.notebooks()
	st.mu.Lock()
	name := req.GetParent() + "/notebookRuntimeTemplates/" + id
	if _, ok := st.templates[name]; ok {
		st.mu.Unlock()
		return nil, grpcError(apierror.AlreadyExists("NotebookRuntimeTemplate %s already exists", name))
	}
	now := timestamppb.New(st.now())
	t.Name, t.CreateTime, t.UpdateTime, t.IsDefault, t.ServiceAccount = name, now, now, false, ""
	t.Etag = templateEtag(t)
	st.templates[name] = t
	st.save()
	view := templateView(t)
	st.mu.Unlock()
	return n.s.finished(name, &aiplatformpb.CreateNotebookRuntimeTemplateOperationMetadata{GenericMetadata: generic()}, view, nil)
}

func (st *notebookStore) template(name string) (*aiplatformpb.NotebookRuntimeTemplate, error) {
	if _, _, _, err := parseChild("name", "notebookRuntimeTemplates", name); err != nil {
		return nil, err
	}
	t, ok := st.templates[name]
	if !ok {
		return nil, apierror.NotFound("NotebookRuntimeTemplate %s not found", name)
	}
	return t, nil
}

func (n *notebookService) GetNotebookRuntimeTemplate(_ context.Context, req *aiplatformpb.GetNotebookRuntimeTemplateRequest) (*aiplatformpb.NotebookRuntimeTemplate, error) {
	st := n.s.notebooks()
	st.mu.Lock()
	defer st.mu.Unlock()
	t, err := st.template(req.GetName())
	if err != nil {
		return nil, grpcError(err)
	}
	return templateView(t), nil
}

func (n *notebookService) ListNotebookRuntimeTemplates(_ context.Context, req *aiplatformpb.ListNotebookRuntimeTemplatesRequest) (*aiplatformpb.ListNotebookRuntimeTemplatesResponse, error) {
	if _, _, err := parseParent("parent", req.GetParent()); err != nil {
		return nil, grpcError(err)
	}
	st := n.s.notebooks()
	st.mu.Lock()
	var all []*aiplatformpb.NotebookRuntimeTemplate
	for _, k := range sortedNames(st.templates) {
		if strings.HasPrefix(k, req.GetParent()+"/") {
			all = append(all, templateView(st.templates[k]))
		}
	}
	st.mu.Unlock()
	all, err := aipfilter.Apply(req.GetFilter(), all, templateFilter...)
	if err != nil {
		return nil, grpcError(err)
	}
	if err := aipfilter.SortBy(req.GetOrderBy(), all, notebookOrder); err != nil {
		return nil, grpcError(err)
	}
	items, next, err := aipfilter.Page("notebook-templates:"+req.GetParent()+"|"+req.GetFilter()+"|"+req.GetOrderBy(), all, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, grpcError(err)
	}
	if m := req.GetReadMask(); m != nil && len(m.GetPaths()) > 0 {
		for i, t := range items {
			items[i] = maskTemplate(t, m.GetPaths())
		}
	}
	return &aiplatformpb.ListNotebookRuntimeTemplatesResponse{NotebookRuntimeTemplates: items, NextPageToken: next}, nil
}

// maskTemplate keeps the read mask's top-level fields (and the name).
func maskTemplate(t *aiplatformpb.NotebookRuntimeTemplate, paths []string) *aiplatformpb.NotebookRuntimeTemplate {
	keep := map[string]bool{"name": true}
	for _, p := range paths {
		keep[strings.SplitN(p, ".", 2)[0]] = true
	}
	m := t.ProtoReflect()
	fields := m.Descriptor().Fields()
	for i := 0; i < fields.Len(); i++ {
		f := fields.Get(i)
		if !keep[string(f.Name())] && !keep[f.JSONName()] {
			m.Clear(f)
		}
	}
	return t
}

func (n *notebookService) DeleteNotebookRuntimeTemplate(_ context.Context, req *aiplatformpb.DeleteNotebookRuntimeTemplateRequest) (*longrunningpb.Operation, error) {
	st := n.s.notebooks()
	st.mu.Lock()
	if _, err := st.template(req.GetName()); err != nil {
		st.mu.Unlock()
		return nil, grpcError(err)
	}
	delete(st.templates, req.GetName())
	delete(st.policies, req.GetName())
	for k, v := range st.defaults {
		if v == req.GetName() {
			delete(st.defaults, k)
		}
	}
	st.save()
	st.mu.Unlock()
	return n.s.finished(req.GetName(), &aiplatformpb.DeleteOperationMetadata{GenericMetadata: generic()}, &emptypb.Empty{}, nil)
}

func (n *notebookService) UpdateNotebookRuntimeTemplate(_ context.Context, req *aiplatformpb.UpdateNotebookRuntimeTemplateRequest) (*aiplatformpb.NotebookRuntimeTemplate, error) {
	in := req.GetNotebookRuntimeTemplate()
	paths := req.GetUpdateMask().GetPaths()
	if len(paths) == 0 {
		return nil, grpcError(apierror.InvalidArgument("update_mask is required"))
	}
	st := n.s.notebooks()
	st.mu.Lock()
	defer st.mu.Unlock()
	cur, err := st.template(in.GetName())
	if err != nil {
		return nil, grpcError(err)
	}
	if e := in.GetEtag(); e != "" && e != cur.GetEtag() {
		return nil, grpcError(apierror.Aborted("the etag %q does not match the NotebookRuntimeTemplate's current etag", e))
	}
	next := proto.Clone(cur).(*aiplatformpb.NotebookRuntimeTemplate)
	for _, p := range paths {
		p = camelToSnake(p)
		if !templateMasks[p] {
			return nil, grpcError(apierror.InvalidArgument("update_mask path %q cannot be updated; the fields that can are display_name, encryption_spec.kms_key_name, software_config.env, software_config.colab_image.release_name and software_config.post_startup_script_config's post_startup_script, post_startup_script_url and post_startup_script_behavior", p))
		}
		switch p {
		case "display_name":
			if err := checkNotebookDisplayName("notebook_runtime_template.display_name", in.GetDisplayName()); err != nil {
				return nil, grpcError(err)
			}
			next.DisplayName = in.GetDisplayName()
		case "encryption_spec.kms_key_name":
			if in.GetEncryptionSpec().GetKmsKeyName() == "" {
				next.EncryptionSpec = nil
			} else {
				next.EncryptionSpec = &aiplatformpb.EncryptionSpec{KmsKeyName: in.GetEncryptionSpec().GetKmsKeyName()}
			}
		default:
			if next.SoftwareConfig == nil {
				next.SoftwareConfig = &aiplatformpb.NotebookSoftwareConfig{}
			}
			sc, ic := next.SoftwareConfig, in.GetSoftwareConfig()
			psc := func() *aiplatformpb.PostStartupScriptConfig {
				if sc.PostStartupScriptConfig == nil {
					sc.PostStartupScriptConfig = &aiplatformpb.PostStartupScriptConfig{}
				}
				return sc.PostStartupScriptConfig
			}
			switch p {
			case "software_config.env":
				sc.Env = ic.GetEnv()
			case "software_config.colab_image.release_name":
				sc.RuntimeImage = &aiplatformpb.NotebookSoftwareConfig_ColabImage{ColabImage: &aiplatformpb.ColabImage{ReleaseName: ic.GetColabImage().GetReleaseName()}}
			case "software_config.post_startup_script_config.post_startup_script":
				psc().PostStartupScript = ic.GetPostStartupScriptConfig().GetPostStartupScript()
			case "software_config.post_startup_script_config.post_startup_script_url":
				psc().PostStartupScriptUrl = ic.GetPostStartupScriptConfig().GetPostStartupScriptUrl()
			case "software_config.post_startup_script_config.post_startup_script_behavior":
				psc().PostStartupScriptBehavior = ic.GetPostStartupScriptConfig().GetPostStartupScriptBehavior()
			}
			if err := checkSoftware(sc); err != nil {
				return nil, grpcError(err)
			}
		}
	}
	next.UpdateTime = timestamppb.New(st.now())
	next.Etag = templateEtag(next)
	st.templates[next.GetName()] = next
	st.save()
	return templateView(next), nil
}

func camelToSnake(p string) string {
	var b strings.Builder
	for _, r := range p {
		if r >= 'A' && r <= 'Z' {
			b.WriteByte('_')
			r += 'a' - 'A'
		}
		b.WriteRune(r)
	}
	return b.String()
}

// SetDefaultNotebookRuntimeTemplate is notebookRuntimeTemplates:setDefault
// (REST only, in the v1 discovery document): name becomes its project and
// location's default template, the one-click runtimes' (Set as Default).
func (s *Server) SetDefaultNotebookRuntimeTemplate(name string) (*aiplatformpb.NotebookRuntimeTemplate, error) {
	st := s.notebooks()
	st.mu.Lock()
	defer st.mu.Unlock()
	t, err := st.template(name)
	if err != nil {
		return nil, err
	}
	st.defaults[name[:strings.Index(name, "/notebookRuntimeTemplates/")]] = name
	st.save()
	return templateView(t), nil
}

// DefaultNotebookRuntimeTemplate is parent's default template, "" for
// none.
func (s *Server) DefaultNotebookRuntimeTemplate(parent string) string {
	st := s.notebooks()
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.defaults[parent]
}

// NotebookRuntimeTemplateOptions resolves a template's runtime options, for
// a notebook run on it (Dataform's notebook actions, BigQuery Studio's
// scheduled notebooks).
func (s *Server) NotebookRuntimeTemplateOptions(name string) (NotebookRuntimeOptions, error) {
	st := s.notebooks()
	st.mu.Lock()
	defer st.mu.Unlock()
	t, err := st.template(name)
	if err != nil {
		return NotebookRuntimeOptions{}, err
	}
	if err := runnable(t.GetMachineSpec()); err != nil {
		return NotebookRuntimeOptions{}, err
	}
	if u := t.GetSoftwareConfig().GetPostStartupScriptConfig().GetPostStartupScriptUrl(); u != "" {
		return NotebookRuntimeOptions{}, scriptURLError(u)
	}
	return optionsOf(t.GetIdleShutdownConfig(), t.GetSoftwareConfig()), nil
}

func optionsOf(i *aiplatformpb.NotebookIdleShutdownConfig, sc *aiplatformpb.NotebookSoftwareConfig) NotebookRuntimeOptions {
	o := NotebookRuntimeOptions{IdleDisabled: i.GetIdleShutdownDisabled()}
	if d := i.GetIdleTimeout(); d != nil {
		o.IdleTimeout = d.AsDuration()
	}
	if len(sc.GetEnv()) > 0 {
		o.Env = map[string]string{}
		for _, e := range sc.GetEnv() {
			o.Env[e.GetName()] = e.GetValue()
		}
	}
	return o
}

func runnable(m *aiplatformpb.MachineSpec) error {
	if m.GetAcceleratorType() != aiplatformpb.AcceleratorType_ACCELERATOR_TYPE_UNSPECIFIED || m.GetAcceleratorCount() > 0 {
		return apierror.FailedPrecondition("the runtime template asks for %s accelerators, and this instance's runtimes run on the machine's CPU only", m.GetAcceleratorType())
	}
	return nil
}

func scriptURLError(u string) error {
	return apierror.FailedPrecondition("the post-startup script %s cannot be fetched: a runtime runs an inline post_startup_script only; set the script's text instead of its Cloud Storage URL", u)
}

// ---- runtimes

func (st *notebookStore) runtime(name string) (*runtimeEntry, error) {
	if _, _, _, err := parseChild("name", "notebookRuntimes", name); err != nil {
		return nil, err
	}
	e, ok := st.runtimes[name]
	if !ok {
		return nil, apierror.NotFound("NotebookRuntime %s not found", name)
	}
	return e, nil
}

// refresh brings e's state up to date with the host; the caller holds
// st.mu.
func (st *notebookStore) refresh(e *runtimeEntry) {
	rt := e.RT
	if !e.Running {
		rt.RuntimeState, rt.HealthState = aiplatformpb.NotebookRuntime_STOPPED, aiplatformpb.NotebookRuntime_HEALTH_STATE_UNSPECIFIED
		if e.Message != "" {
			rt.RuntimeState = aiplatformpb.NotebookRuntime_ERROR
		}
		return
	}
	state, msg := "DISCONNECTED", ""
	if st.host != nil {
		state, msg = st.host.State(runtimeKey(rt.GetName()))
	}
	switch {
	case state == "CONNECTED" && e.Ready:
		rt.RuntimeState, rt.HealthState = aiplatformpb.NotebookRuntime_RUNNING, aiplatformpb.NotebookRuntime_HEALTHY
	case state == "CONNECTED", state == "STARTING", state == "BUILDING":
		rt.RuntimeState, rt.HealthState = aiplatformpb.NotebookRuntime_BEING_STARTED, aiplatformpb.NotebookRuntime_HEALTH_STATE_UNSPECIFIED
	case state == "FAILED":
		e.Running, e.Message = false, msg
		rt.RuntimeState, rt.HealthState = aiplatformpb.NotebookRuntime_ERROR, aiplatformpb.NotebookRuntime_UNHEALTHY
	default:
		// Its idle shutdown stopped it.
		e.Running = false
		rt.RuntimeState, rt.HealthState = aiplatformpb.NotebookRuntime_STOPPED, aiplatformpb.NotebookRuntime_HEALTH_STATE_UNSPECIFIED
		st.save()
	}
}

// expire deletes the one-click runtimes past their expiration_time; the
// caller holds st.mu.
func (st *notebookStore) expire() {
	now := st.now()
	for k, e := range st.runtimes {
		if e.RT.GetNotebookRuntimeType() == aiplatformpb.NotebookRuntimeType_ONE_CLICK && e.RT.GetExpirationTime().AsTime().Before(now) {
			if st.host != nil {
				_ = st.host.Stop(runtimeKey(k))
			}
			delete(st.runtimes, k)
			st.save()
		}
	}
}

func (st *notebookStore) view(e *runtimeEntry) *aiplatformpb.NotebookRuntime {
	st.refresh(e)
	out := proto.Clone(e.RT).(*aiplatformpb.NotebookRuntime)
	if out.EucConfig != nil {
		out.EucConfig.EucDisabled = false
	}
	return out
}

func (n *notebookService) AssignNotebookRuntime(_ context.Context, req *aiplatformpb.AssignNotebookRuntimeRequest) (*longrunningpb.Operation, error) {
	project, location, err := parseParent("parent", req.GetParent())
	if err != nil {
		return nil, grpcError(err)
	}
	in := req.GetNotebookRuntime()
	if in == nil {
		return nil, grpcError(apierror.InvalidArgument("notebook_runtime is required"))
	}
	if err := checkNotebookDisplayName("notebook_runtime.display_name", in.GetDisplayName()); err != nil {
		return nil, grpcError(err)
	}
	if in.GetRuntimeUser() == "" {
		return nil, grpcError(apierror.InvalidArgument("notebook_runtime.runtime_user is required"))
	}
	if err := checkNotebookLabels(in.GetLabels()); err != nil {
		return nil, grpcError(err)
	}
	id := req.GetNotebookRuntimeId()
	if id != "" && !notebookIDRE.MatchString(id) {
		return nil, grpcError(apierror.InvalidArgument("notebook_runtime_id %q must be 1 to 63 lowercase letters, digits and hyphens, starting with a letter", id))
	}
	st := n.s.notebooks()
	st.mu.Lock()
	st.expire()
	tplName := req.GetNotebookRuntimeTemplate()
	if tplName == "" {
		st.mu.Unlock()
		return nil, grpcError(apierror.InvalidArgument("notebook_runtime_template is required"))
	}
	t, err := st.template(tplName)
	if err != nil {
		st.mu.Unlock()
		return nil, grpcError(err)
	}
	if p, l, _, _ := parseChild("notebook_runtime_template", "notebookRuntimeTemplates", tplName); p != project || l != location {
		st.mu.Unlock()
		return nil, grpcError(apierror.InvalidArgument("notebook_runtime_template %s must be in %s", tplName, req.GetParent()))
	}
	// An existing assignment: the runtime of that ID, or the user's
	// one-click runtime of the template.
	for _, k := range sortedNames(st.runtimes) {
		e := st.runtimes[k]
		same := id != "" && k == req.GetParent()+"/notebookRuntimes/"+id
		oneClick := id == "" && t.GetNotebookRuntimeType() == aiplatformpb.NotebookRuntimeType_ONE_CLICK &&
			e.RT.GetNotebookRuntimeType() == aiplatformpb.NotebookRuntimeType_ONE_CLICK && e.RT.GetRuntimeUser() == in.GetRuntimeUser() &&
			strings.HasPrefix(k, req.GetParent()+"/")
		if !same && !oneClick {
			continue
		}
		if e.RT.GetNotebookRuntimeTemplateRef().GetNotebookRuntimeTemplate() != tplName || e.RT.GetRuntimeUser() != in.GetRuntimeUser() {
			st.mu.Unlock()
			if oneClick {
				return nil, grpcError(apierror.AlreadyExists("One click runtime already exists."))
			}
			return nil, grpcError(apierror.AlreadyExists("NotebookRuntime %s already exists, assigned from another template or to another user", k))
		}
		v := st.view(e)
		st.mu.Unlock()
		return n.s.finished(k, &aiplatformpb.AssignNotebookRuntimeOperationMetadata{GenericMetadata: generic()}, v, nil)
	}
	if err := runnable(t.GetMachineSpec()); err != nil {
		st.mu.Unlock()
		return nil, grpcError(err)
	}
	if u := t.GetSoftwareConfig().GetPostStartupScriptConfig().GetPostStartupScriptUrl(); u != "" {
		st.mu.Unlock()
		return nil, grpcError(scriptURLError(u))
	}
	if st.host == nil {
		st.mu.Unlock()
		return nil, grpcError(runtimesUnavailable())
	}
	if id == "" {
		id = numericID()
	}
	name := req.GetParent() + "/notebookRuntimes/" + id
	now := st.now()
	life := userRuntimeLife
	if t.GetNotebookRuntimeType() == aiplatformpb.NotebookRuntimeType_ONE_CLICK {
		life = oneClickLifetime
	}
	labels := map[string]string{}
	for k, v := range in.GetLabels() {
		labels[k] = v
	}
	labels["aiplatform.googleapis.com/colab_enterprise_entry_service"] = "vertex"
	rt := &aiplatformpb.NotebookRuntime{
		Name: name, DisplayName: in.GetDisplayName(), Description: in.GetDescription(), RuntimeUser: in.GetRuntimeUser(),
		NotebookRuntimeTemplateRef: &aiplatformpb.NotebookRuntimeTemplateRef{NotebookRuntimeTemplate: tplName},
		CreateTime:                 timestamppb.New(now), UpdateTime: timestamppb.New(now), ExpirationTime: timestamppb.New(now.Add(life)),
		Labels: labels, NetworkTags: append(append([]string(nil), t.GetNetworkTags()...), in.GetNetworkTags()...),
		Version: st.host.Version(), NotebookRuntimeType: t.GetNotebookRuntimeType(),
		RuntimeState: aiplatformpb.NotebookRuntime_BEING_STARTED,
	}
	if t.MachineSpec != nil {
		rt.MachineSpec = proto.Clone(t.MachineSpec).(*aiplatformpb.MachineSpec)
	}
	if t.DataPersistentDiskSpec != nil {
		rt.DataPersistentDiskSpec = proto.Clone(t.DataPersistentDiskSpec).(*aiplatformpb.PersistentDiskSpec)
	}
	if t.NetworkSpec != nil {
		rt.NetworkSpec = proto.Clone(t.NetworkSpec).(*aiplatformpb.NetworkSpec)
	}
	if t.IdleShutdownConfig != nil {
		rt.IdleShutdownConfig = proto.Clone(t.IdleShutdownConfig).(*aiplatformpb.NotebookIdleShutdownConfig)
	}
	if t.EucConfig != nil {
		rt.EucConfig = proto.Clone(t.EucConfig).(*aiplatformpb.NotebookEucConfig)
	}
	if t.ShieldedVmConfig != nil {
		rt.ShieldedVmConfig = proto.Clone(t.ShieldedVmConfig).(*aiplatformpb.ShieldedVmConfig)
	}
	if t.EncryptionSpec != nil {
		rt.EncryptionSpec = proto.Clone(t.EncryptionSpec).(*aiplatformpb.EncryptionSpec)
	}
	if t.SoftwareConfig != nil {
		rt.SoftwareConfig = proto.Clone(t.SoftwareConfig).(*aiplatformpb.NotebookSoftwareConfig)
	}
	e := &runtimeEntry{RT: rt}
	st.runtimes[name] = e
	st.save()
	op := n.s.newOp(name)
	_ = n.s.ops.SetMetadata(op, &aiplatformpb.AssignNotebookRuntimeOperationMetadata{GenericMetadata: generic(), ProgressMessage: "Starting the runtime"})
	st.startLocked(e, project)
	st.mu.Unlock()
	go n.s.awaitRunning(op, e, func(v *aiplatformpb.NotebookRuntime) proto.Message { return v })
	return n.s.opView(op)
}

func runtimesUnavailable() error {
	return apierror.FailedPrecondition("Colab Enterprise runtimes are not served on this instance: they run on the instance's notebook runtime, which needs Docker and BigQuery (start with --services including bigquery)")
}

// startLocked starts e's runtime on the host; the caller holds st.mu.
func (st *notebookStore) startLocked(e *runtimeEntry, project string) {
	e.Running, e.Ready, e.Message = true, false, ""
	st.host.Start(runtimeKey(e.RT.GetName()), project, optionsOf(e.RT.GetIdleShutdownConfig(), e.RT.GetSoftwareConfig()))
}

func (s *Server) opView(op string) (*longrunningpb.Operation, error) {
	o, err := s.ops.Get(op)
	if err != nil {
		return nil, grpcError(err)
	}
	out, err := opProto(o)
	return out, grpcError(err)
}

// awaitRunning completes op when e's runtime is up, its post-startup
// script run, or fails it.
func (s *Server) awaitRunning(op string, e *runtimeEntry, resp func(*aiplatformpb.NotebookRuntime) proto.Message) {
	st := s.notebooks()
	key := runtimeKey(e.RT.GetName())
	deadline := time.Now().Add(runtimeStartWait)
	for {
		st.mu.Lock()
		state, msg := st.host.State(key)
		gone := st.runtimes[e.RT.GetName()] != e || !e.Running
		st.mu.Unlock()
		if gone {
			_ = s.ops.Fail(op, apierror.Aborted("the runtime was stopped or deleted before it started"))
			return
		}
		if state == "CONNECTED" {
			break
		}
		if state == "FAILED" || state == "DISCONNECTED" || time.Now().After(deadline) {
			if msg == "" {
				msg = "the runtime did not start"
			}
			st.mu.Lock()
			e.Running, e.Message = false, msg
			st.save()
			st.mu.Unlock()
			_ = s.ops.Fail(op, apierror.FailedPrecondition("%s", msg))
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	st.mu.Lock()
	sc := e.RT.GetSoftwareConfig().GetPostStartupScriptConfig()
	script := sc.GetPostStartupScript()
	behavior := sc.GetPostStartupScriptBehavior()
	run := script != "" && (!e.Ran || behavior == aiplatformpb.PostStartupScriptConfig_RUN_EVERY_START || behavior == aiplatformpb.PostStartupScriptConfig_DOWNLOAD_AND_RUN_EVERY_START)
	st.mu.Unlock()
	if run {
		q, _ := json.Marshal(script)
		code := "import subprocess as _cb_sp\n_cb_r = _cb_sp.run(['bash', '-c', " + string(q) + "], capture_output=True, text=True)\n" +
			"if _cb_r.returncode != 0:\n    raise RuntimeError('the post-startup script exited with status %d: %s' % (_cb_r.returncode, _cb_r.stderr[-2000:]))\ndel _cb_sp, _cb_r\n"
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		err := st.host.Exec(ctx, key, code)
		cancel()
		if err != nil {
			st.mu.Lock()
			_ = st.host.Stop(key)
			e.Running, e.Message = false, err.Error()
			st.save()
			st.mu.Unlock()
			_ = s.ops.Fail(op, apierror.FailedPrecondition("the post-startup script failed: %v", err))
			return
		}
	}
	st.mu.Lock()
	e.Ready, e.Ran = true, e.Ran || run
	e.RT.UpdateTime = timestamppb.New(st.now())
	v := st.view(e)
	st.save()
	st.mu.Unlock()
	_ = s.ops.Succeed(op, resp(v))
}

func (n *notebookService) GetNotebookRuntime(_ context.Context, req *aiplatformpb.GetNotebookRuntimeRequest) (*aiplatformpb.NotebookRuntime, error) {
	st := n.s.notebooks()
	st.mu.Lock()
	defer st.mu.Unlock()
	st.expire()
	e, err := st.runtime(req.GetName())
	if err != nil {
		return nil, grpcError(err)
	}
	return st.view(e), nil
}

func (n *notebookService) ListNotebookRuntimes(_ context.Context, req *aiplatformpb.ListNotebookRuntimesRequest) (*aiplatformpb.ListNotebookRuntimesResponse, error) {
	if _, _, err := parseParent("parent", req.GetParent()); err != nil {
		return nil, grpcError(err)
	}
	st := n.s.notebooks()
	st.mu.Lock()
	st.expire()
	var all []*aiplatformpb.NotebookRuntime
	for _, k := range sortedNames(st.runtimes) {
		if strings.HasPrefix(k, req.GetParent()+"/") {
			all = append(all, st.view(st.runtimes[k]))
		}
	}
	st.mu.Unlock()
	all, err := aipfilter.Apply(req.GetFilter(), all, runtimeFilter...)
	if err != nil {
		return nil, grpcError(err)
	}
	if err := aipfilter.SortBy(req.GetOrderBy(), all, notebookOrder); err != nil {
		return nil, grpcError(err)
	}
	items, next, err := aipfilter.Page("notebook-runtimes:"+req.GetParent()+"|"+req.GetFilter()+"|"+req.GetOrderBy(), all, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, grpcError(err)
	}
	return &aiplatformpb.ListNotebookRuntimesResponse{NotebookRuntimes: items, NextPageToken: next}, nil
}

func (n *notebookService) DeleteNotebookRuntime(_ context.Context, req *aiplatformpb.DeleteNotebookRuntimeRequest) (*longrunningpb.Operation, error) {
	st := n.s.notebooks()
	st.mu.Lock()
	e, err := st.runtime(req.GetName())
	if err != nil {
		st.mu.Unlock()
		return nil, grpcError(err)
	}
	e.Running = false
	delete(st.runtimes, req.GetName())
	st.save()
	host := st.host
	st.mu.Unlock()
	if host != nil {
		_ = host.Stop(runtimeKey(req.GetName()))
	}
	return n.s.finished(req.GetName(), &aiplatformpb.DeleteOperationMetadata{GenericMetadata: generic()}, &emptypb.Empty{}, nil)
}

func (n *notebookService) StartNotebookRuntime(_ context.Context, req *aiplatformpb.StartNotebookRuntimeRequest) (*longrunningpb.Operation, error) {
	st := n.s.notebooks()
	st.mu.Lock()
	e, err := st.runtime(req.GetName())
	if err != nil {
		st.mu.Unlock()
		return nil, grpcError(err)
	}
	if st.host == nil {
		st.mu.Unlock()
		return nil, grpcError(runtimesUnavailable())
	}
	meta := &aiplatformpb.StartNotebookRuntimeOperationMetadata{GenericMetadata: generic(), ProgressMessage: "Starting the runtime"}
	if e.Running {
		st.mu.Unlock()
		op := n.s.newOp(req.GetName())
		_ = n.s.ops.SetMetadata(op, meta)
		go n.s.awaitRunning(op, e, func(*aiplatformpb.NotebookRuntime) proto.Message { return &aiplatformpb.StartNotebookRuntimeResponse{} })
		return n.s.opView(op)
	}
	if err := runnable(e.RT.GetMachineSpec()); err != nil {
		st.mu.Unlock()
		return nil, grpcError(err)
	}
	project, _, _, _ := parseChild("name", "notebookRuntimes", req.GetName())
	st.startLocked(e, project)
	st.save()
	st.mu.Unlock()
	op := n.s.newOp(req.GetName())
	_ = n.s.ops.SetMetadata(op, meta)
	go n.s.awaitRunning(op, e, func(*aiplatformpb.NotebookRuntime) proto.Message { return &aiplatformpb.StartNotebookRuntimeResponse{} })
	return n.s.opView(op)
}

func (n *notebookService) StopNotebookRuntime(_ context.Context, req *aiplatformpb.StopNotebookRuntimeRequest) (*longrunningpb.Operation, error) {
	st := n.s.notebooks()
	st.mu.Lock()
	e, err := st.runtime(req.GetName())
	if err != nil {
		st.mu.Unlock()
		return nil, grpcError(err)
	}
	e.Running, e.Ready, e.Message = false, false, ""
	e.RT.UpdateTime = timestamppb.New(st.now())
	st.save()
	host := st.host
	st.mu.Unlock()
	if host != nil {
		_ = host.Stop(runtimeKey(req.GetName()))
	}
	return n.s.finished(req.GetName(), &aiplatformpb.StopNotebookRuntimeOperationMetadata{GenericMetadata: generic()}, &aiplatformpb.StopNotebookRuntimeResponse{}, nil)
}

func (n *notebookService) UpgradeNotebookRuntime(_ context.Context, req *aiplatformpb.UpgradeNotebookRuntimeRequest) (*longrunningpb.Operation, error) {
	st := n.s.notebooks()
	st.mu.Lock()
	_, err := st.runtime(req.GetName())
	st.mu.Unlock()
	if err != nil {
		return nil, grpcError(err)
	}
	return n.s.finished(req.GetName(), &aiplatformpb.UpgradeNotebookRuntimeOperationMetadata{GenericMetadata: generic(), ProgressMessage: "The runtime is up to date"},
		&aiplatformpb.UpgradeNotebookRuntimeResponse{}, nil)
}

// ---- registration and REST

func (s *Server) registerNotebooks(srv grpc.ServiceRegistrar) {
	aiplatformpb.RegisterNotebookServiceServer(srv, &notebookService{s: s})
	aiplatformpb.RegisterScheduleServiceServer(srv, &scheduleService{s: s})
	aiplatformpb.RegisterPipelineServiceServer(srv, &pipelineService{s: s})
	aiplatformpb.RegisterJobServiceServer(srv, &jobService{s: s})
	aiplatformpb.RegisterMetadataServiceServer(srv, &metadataService{s: s})
	aiplatformpb.RegisterDatasetServiceServer(srv, &datasetService{s: s})
	nbpb.RegisterNotebookServiceServer(srv, &workbenchService{s: s})
	// Model Monitoring and data labeling jobs (#2214): v1's JobService
	// (above), v1beta1's ModelMonitoringService and ScheduleService.
	betapb.RegisterModelMonitoringServiceServer(srv, &modelMonitoringService{s: s})
	betapb.RegisterScheduleServiceServer(srv, &betaScheduleService{s: s})
}

// notebookREST is the transcoder for the notebookRuntimeTemplates and
// notebookRuntimes paths and their operations.
func (s *Server) notebookREST() *rest.Transcoder {
	s.nbRESTOnce.Do(func() {
		t := &rest.Transcoder{Packages: []string{"google.cloud.aiplatform.v1", "google.cloud.aiplatform.v1beta1", "google.cloud.notebooks.v2"}, SnakeCaseMasks: true, Mixins: []*annotations.HttpRule{
			opRule("/v1/{name=projects/*/locations/*/notebookRuntimeTemplates/*/operations/*}"),
			opRule("/v1/{name=projects/*/locations/*/notebookRuntimes/*/operations/*}"),
			opRule("/v1/{name=projects/*/locations/*/notebookExecutionJobs/*/operations/*}"),
			opRule("/v1/{name=projects/*/locations/*/schedules/*/operations/*}"),
			opRule("/v1/{name=projects/*/locations/*/pipelineJobs/*/operations/*}"),
			opRule("/v1/{name=projects/*/locations/*/modelDeploymentMonitoringJobs/*/operations/*}"),
			opRule("/v1beta1/{name=projects/*/locations/*/modelMonitors/*/operations/*}"),
			opRule("/v1beta1/{name=projects/*/locations/*/modelMonitors/*/modelMonitoringJobs/*/operations/*}"),
			opRule("/v1beta1/{name=projects/*/locations/*/schedules/*/operations/*}"),
			opRule("/v1/{name=projects/*/locations/*/customJobs/*/operations/*}"),
			opRule("/v1/{name=projects/*/locations/*/batchPredictionJobs/*/operations/*}"),
			opRule("/v1/{name=projects/*/locations/*/trainingPipelines/*/operations/*}"),
			opRule("/v1/{name=projects/*/locations/*/hyperparameterTuningJobs/*/operations/*}"),
			opRule("/v1/{name=projects/*/locations/*/nasJobs/*/operations/*}"),
			opRule("/v1/{name=projects/*/locations/*/datasets/*/operations/*}"),
			opRule("/v1/{name=projects/*/locations/*/datasets/*/savedQueries/*/operations/*}"),
			opRule("/v1/{name=projects/*/locations/*/datasets/*/datasetVersions/*/operations/*}"),
			opRule("/v1/{name=projects/*/locations/*/metadataStores/*/operations/*}"),
			opRule("/v1/{name=projects/*/locations/*/metadataStores/*/artifacts/*/operations/*}"),
			opRule("/v1/{name=projects/*/locations/*/metadataStores/*/contexts/*/operations/*}"),
			opRule("/v1/{name=projects/*/locations/*/metadataStores/*/executions/*/operations/*}"),
		}}
		// The Python SDK (google-cloud-aiplatform) polls under /ui, the
		// first binding aiplatform_v1.yaml gives GetOperation (#1940): a
		// dataset's import is polled as /ui/{name=.../datasets/*/operations/*}.
		for _, m := range append([]*annotations.HttpRule(nil), t.Mixins...) {
			if g := m.GetGet(); strings.HasPrefix(g, "/v1/{name=projects/*/locations/*/") {
				t.Mixins = append(t.Mixins, opRule("/ui/"+strings.TrimPrefix(g, "/v1/")))
			}
		}
		t.Mixins = append(t.Mixins, NotebookIAMMixins()...)
		s.registerNotebooks(t)
		iampb.RegisterIAMPolicyServer(t, &notebookIAM{s: s})
		s.notebooks()
		longrunningpb.RegisterOperationsServer(t, &operations{s: s})
		s.nbTranscoder = t
	})
	return s.nbTranscoder
}

func (s *Server) serveNotebookREST(w http.ResponseWriter, r *http.Request) {
	s.notebookREST().ServeHTTP(w, r)
}

// setDefaultREST is POST .../notebookRuntimeTemplates:setDefault, body
// {"name": TEMPLATE}, answering {"notebookRuntimeTemplate": TEMPLATE}.
func (s *Server) setDefaultREST(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r)
	if err != nil {
		apierror.WriteJSON(w, err)
		return
	}
	var in struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		apierror.WriteJSON(w, apierror.InvalidArgument("the request body is not JSON: %v", err))
		return
	}
	parent := "projects/" + r.PathValue("project") + "/locations/" + r.PathValue("location")
	if !strings.HasPrefix(in.Name, parent+"/notebookRuntimeTemplates/") {
		apierror.WriteJSON(w, apierror.InvalidArgument("name %q must be a notebook runtime template of %s", in.Name, parent))
		return
	}
	t, err := s.SetDefaultNotebookRuntimeTemplate(in.Name)
	if err != nil {
		apierror.WriteJSON(w, err)
		return
	}
	b, _ := protojson.Marshal(t)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = fmt.Fprintf(w, `{"notebookRuntimeTemplate":%s}`, b)
}
