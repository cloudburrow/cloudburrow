// Package config defines the configuration model, its documented precedence
// (flags over environment over file over defaults), and validation. Invalid
// configuration must fail here, before a cluster is created or any listener is
// opened.
//
// This package is pure Go and must stay testable without a Kubernetes cluster.
// See docs/architecture.md §3.
package config

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/resource"
)

// Mode selects whether CloudBurrow provisions durable storage for backends that
// support it. It is not a promise that every service persists: see
// Service.Persistence.
type Mode string

const (
	// ModeEphemeral provisions no durable volumes. Nothing survives a delete.
	ModeEphemeral Mode = "ephemeral"
	// ModePersistent provisions PersistentVolumeClaims for backends that can
	// use them.
	ModePersistent Mode = "persistent"
)

// Service identifies an emulated Google Cloud service.
type Service string

const (
	ServiceStorage Service = "storage"
	ServicePubSub  Service = "pubsub"
	ServiceTasks   Service = "tasks"
	ServiceRun     Service = "run"
	// ServiceSecrets is Secret Manager. Like Cloud Tasks it has no upstream
	// emulator, so CloudBurrow implements it and it runs in the CLI process.
	ServiceSecrets Service = "secretmanager"

	// Optional services. Each wraps a Google-published emulator and is opt-in:
	// they are not part of the MVP, and starting them by default would spend a
	// developer's memory on databases they did not ask for.
	ServiceFirestore Service = "firestore"
	ServiceDatastore Service = "datastore"
	ServiceBigtable  Service = "bigtable"
	ServiceSpanner   Service = "spanner"
	// ServiceCloudSQL runs a real PostgreSQL in the cluster.
	//
	// It is unlike every other opt-in backend here: Google publishes no Cloud
	// SQL emulator, so there is nothing of theirs to reuse. What runs is the
	// database Cloud SQL runs underneath, which gives an application a real
	// SQL server at a stable local address — and none of the Cloud SQL Admin
	// API, which is stated everywhere it could be mistaken. See #121.
	ServiceCloudSQL Service = "cloudsql"
	// ServiceBigQuery runs goccy/bigquery-emulator, a community emulator.
	//
	// Google publishes no BigQuery emulator, so this is the one opt-in
	// service whose backend is neither Google's nor the real engine. It
	// serves a single project — the instance's own — and keeps nothing across
	// a restart; both were measured (#277), and both are stated wherever its
	// endpoint is shown.
	ServiceBigQuery Service = "bigquery"
	// ServiceMemorystore runs Valkey, a Redis-compatible server, in the
	// cluster (#296).
	//
	// Like Cloud SQL it is a real data plane and not an emulator: Google
	// publishes no Memorystore emulator, and what runs is an open-source
	// server of the kind Memorystore for Valkey and for Redis runs. None of
	// the Memorystore admin API (redis.googleapis.com) is served.
	ServiceMemorystore Service = "memorystore"
	// ServiceCloudSQLMySQL runs a real MySQL in the cluster (#297), the
	// second Cloud SQL engine beside PostgreSQL, on the same terms: a real
	// database at a local address, none of the Cloud SQL Admin API.
	ServiceCloudSQLMySQL Service = "cloudsql-mysql"
	// ServiceScheduler is Cloud Scheduler (#302). Google publishes no
	// emulator, so it is implemented here and runs in the CLI process, as
	// Cloud Tasks does. Opt-in: it fires jobs on a clock, which nobody
	// should find happening without having asked for it.
	ServiceScheduler Service = "scheduler"
)

// AllServices lists the default services in a stable order, so startup
// sequence and error messages do not vary between runs.
//
// Optional services are deliberately excluded; see OptionalServices.
func AllServices() []Service {
	return []Service{ServiceStorage, ServicePubSub, ServiceTasks, ServiceRun, ServiceSecrets}
}

// OptionalServices lists the opt-in services, in a stable order.
func OptionalServices() []Service {
	return []Service{ServiceFirestore, ServiceDatastore, ServiceBigtable, ServiceSpanner, ServiceCloudSQL, ServiceBigQuery, ServiceMemorystore, ServiceCloudSQLMySQL, ServiceScheduler}
}

// KnownServices lists every selectable service.
func KnownServices() []Service {
	return append(AllServices(), OptionalServices()...)
}

// IsOptional reports whether a service must be requested explicitly.
func (s Service) IsOptional() bool {
	for _, o := range OptionalServices() {
		if s == o {
			return true
		}
	}
	return false
}

// Persistence describes whether a service's state can survive a restart.
type Persistence string

const (
	// PersistenceNone means state never survives, whatever the mode.
	PersistenceNone Persistence = "none"
	// PersistenceVolume means state survives in ModePersistent, via a PVC.
	PersistenceVolume Persistence = "volume"
)

// Persistence reports the honest durability of a service's backing component.
//
// Pub/Sub is deliberately PersistenceNone: the upstream audit (#24) measured
// Google's emulator losing a topic across a restart *even when given
// --data-dir*. CloudBurrow cannot inherit a guarantee its backend does not
// have, so the model refuses to express one.
func (s Service) Persistence() Persistence {
	switch s {
	case ServicePubSub:
		return PersistenceNone
	case ServiceFirestore, ServiceDatastore, ServiceBigtable, ServiceSpanner, ServiceBigQuery:
		// Every one of these emulators is in-memory. Google documents them as
		// such, so provisioning a volume would imply durability they do not
		// have.
		return PersistenceNone
	case ServiceCloudSQL, ServiceCloudSQLMySQL, ServiceMemorystore:
		// The opt-in backends that are real servers rather than emulators, so
		// they can genuinely keep their data across a restart and are given a
		// volume to do it with.
		return PersistenceVolume
	case ServiceStorage, ServiceTasks, ServiceSecrets, ServiceScheduler:
		return PersistenceVolume
	case ServiceRun:
		// Workload definitions live in the Kubernetes API, which the cluster
		// persists; CloudBurrow provisions no volume of its own for them.
		return PersistenceVolume
	default:
		return PersistenceNone
	}
}

// Duration is a time.Duration that marshals as a human-readable string such as
// "15s". encoding/json renders time.Duration as an integer count of
// nanoseconds, which is unreadable in a hand-edited configuration file.
type Duration time.Duration

// UnmarshalJSON accepts either a duration string ("15s") or a number of
// nanoseconds, so that files written by a machine are still readable.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	switch value := v.(type) {
	case string:
		parsed, err := time.ParseDuration(value)
		if err != nil {
			return fmt.Errorf("parse duration %q: %w", value, err)
		}
		*d = Duration(parsed)
		return nil
	case float64:
		*d = Duration(time.Duration(value))
		return nil
	default:
		return fmt.Errorf("duration must be a string such as \"15s\", got %T", v)
	}
}

// MarshalJSON renders the duration as a string.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

func (d Duration) String() string { return time.Duration(d).String() }

// Endpoints holds the host-side port for each API surface plus the control
// port. A port of 0 requests an OS-assigned port, which is what allows two
// independently named instances to run at once.
type Endpoints struct {
	Control int `json:"control"`
	Storage int `json:"storage"`
	PubSub  int `json:"pubsub"`
	Tasks   int `json:"tasks"`
	Run     int `json:"run"`
	// Ingress is the host port the cluster's Knative gateway is published on.
	//
	// It is unlike the others: they are SDK endpoints reached through a
	// port-forward, while this is a real published port on the cluster node,
	// because a browser opening a Cloud Run URL cannot be asked to set a Host
	// header. It is fixed at cluster creation and cannot be changed without
	// recreating the cluster.
	Ingress int `json:"ingress"`
	// Secrets is the host port Secret Manager binds. It serves gRPC and JSON
	// on the same port, as Google's own endpoint does.
	Secrets int `json:"secrets"`
	// ResourceManager is the host port of the Resource Manager v3 Projects
	// API (#298), served from the project registry the console uses.
	ResourceManager int `json:"resourceManager"`
	// Scheduler is the host port of the Cloud Scheduler API.
	Scheduler int `json:"scheduler"`
	// Console is the host port the web console binds. Like the ingress, it
	// is a port a human types into a browser rather than one an SDK is
	// pointed at.
	Console int `json:"console"`
	// Metadata is the host port the local GCE metadata server binds.
	//
	// It exists so that tooling which insists on credentials — gcloud,
	// Terraform, the Google SDKs — can be run offline. It authenticates
	// nothing; see docs/credentials.md.
	Metadata int `json:"metadata"`
	// LocalAI is the host port the local generation endpoint binds.
	//
	// Unlike the others it is only bound when a model is configured. The
	// runtime is a multi-gigabyte download and a from-source build, so
	// starting it by default would impose that on every user of every other
	// service. See docs/generation.md.
	LocalAI int `json:"localAI"`

	// Firestore, Datastore, Bigtable and Spanner are the host ports of the
	// opt-in emulators.
	//
	// They used to be OS-assigned on every start, so only the running `up`
	// knew them and `cloudburrow env` could not export their *_EMULATOR_HOST
	// variables at all — leaving an application that ran `eval "$(cloudburrow
	// env)"` pointed at real Google for exactly the services it had asked to
	// emulate. Fixed defaults, like the core services have, let a separate
	// process give the same answer `up` does, before and across restarts. 0
	// still means OS-assigned, and then `env` says it cannot know the port.
	Firestore int `json:"firestore"`
	Datastore int `json:"datastore"`
	Bigtable  int `json:"bigtable"`
	Spanner   int `json:"spanner"`
	// BigQuery is the REST API; BigQueryStorage the gRPC Storage Read API,
	// which the Go client's result iterator uses for large reads.
	BigQuery        int `json:"bigquery"`
	BigQueryStorage int `json:"bigqueryStorage"`
	// Memorystore is the host port of the Valkey (RESP) endpoint.
	Memorystore int `json:"memorystore"`
	// CloudSQLMySQL is the host port of the MySQL endpoint. Fixed, unlike
	// PostgreSQL's, so `cloudburrow env` can export MYSQL_PORT without a
	// running instance.
	CloudSQLMySQL int `json:"cloudsqlMySQL"`
}

func (e Endpoints) named() []struct {
	Name string
	Port int
} {
	return []struct {
		Name string
		Port int
	}{
		{"control", e.Control},
		{"storage", e.Storage},
		{"pubsub", e.PubSub},
		{"tasks", e.Tasks},
		{"run", e.Run},
		{"secrets", e.Secrets},
		{"resourcemanager", e.ResourceManager},
		{"scheduler", e.Scheduler},
		{"ingress", e.Ingress},
		{"metadata", e.Metadata},
		{"console", e.Console},
		{"localai", e.LocalAI},
		{"firestore", e.Firestore},
		{"datastore", e.Datastore},
		{"bigtable", e.Bigtable},
		{"spanner", e.Spanner},
		{"bigquery", e.BigQuery},
		{"bigquery-storage", e.BigQueryStorage},
		{"memorystore", e.Memorystore},
		{"cloudsql-mysql", e.CloudSQLMySQL},
	}
}

// OptionalPort returns the configured host port for an opt-in emulator, or 0
// for a service that has none.
func (e Endpoints) OptionalPort(s Service) int {
	switch s {
	case ServiceFirestore:
		return e.Firestore
	case ServiceDatastore:
		return e.Datastore
	case ServiceBigtable:
		return e.Bigtable
	case ServiceSpanner:
		return e.Spanner
	case ServiceBigQuery:
		return e.BigQuery
	case ServiceMemorystore:
		return e.Memorystore
	case ServiceCloudSQLMySQL:
		return e.CloudSQLMySQL
	default:
		return 0
	}
}

// LocalAI configures the optional local generation endpoint.
//
// Everything here is off by default. The runtime is a from-source Bazel build
// and the model a multi-gigabyte download, so it is opted into rather than
// provided — see docs/generation.md.
type LocalAI struct {
	// ModelPath is the host path to a .litertlm artifact. Empty disables the
	// endpoint entirely; no port is bound and nothing is advertised.
	ModelPath string `json:"modelPath"`
	// ModelID is the identifier clients must request, and the one reported
	// back as the model that ran. It defaults to the catalogue entry for the
	// artifact's filename when that is recognised.
	ModelID string `json:"modelId"`
	// Image is the runtime image built by `make litert-lm`.
	Image string `json:"image"`
	// Aliases are additional model IDs that resolve to ModelID.
	//
	// An alias is the only way a request for one model is answered by
	// another, and it must be configured by hand. Nothing here is populated
	// by default, because a substitution nobody asked for is the failure
	// this whole surface is built to avoid.
	Aliases []string `json:"aliases"`
}

// DefaultLocalAIImage is the tag `make litert-lm` produces.
const DefaultLocalAIImage = "cloudburrow/litert-lm:local"

// Cluster describes the local Kubernetes environment CloudBurrow owns.
type Cluster struct {
	// Provider is the cluster provider. Only "kind" is supported; a second
	// provider is not added until one is demonstrably needed (ADR-0005).
	Provider string `json:"provider"`
	// NodeImage is the pinned kind node image, which fixes the Kubernetes
	// version. Must satisfy Knative's enforced minimum.
	NodeImage string `json:"nodeImage"`
	// Namespace holds CloudBurrow-managed workloads.
	Namespace string `json:"namespace"`
	// Kubeconfig is the explicit kubeconfig path CloudBurrow writes and uses.
	// It is never the developer's default file, and the global current context
	// is never changed.
	Kubeconfig string `json:"kubeconfig"`
}

// Config is the complete runtime configuration.
type Config struct {
	// Name identifies this instance. It scopes the cluster name, the namespace
	// and every owned resource, so two instances cannot collide.
	Name string `json:"name"`

	// Project is the instance's default Google project, when it should not be
	// derived from Name. Empty means derive it; see DefaultProject.
	Project string `json:"project,omitempty"`

	// BindAddress is the host address endpoints are published on.
	BindAddress string `json:"bindAddress"`
	// AllowRemote permits binding a non-loopback address. This exposes an
	// unauthenticated emulator, so it is opt-in.
	AllowRemote bool `json:"allowRemote"`

	Endpoints Endpoints `json:"endpoints"`
	Cluster   Cluster   `json:"cluster"`
	LocalAI   LocalAI   `json:"localAI"`

	Mode Mode `json:"mode"`
	// StateDir holds host-side artifacts such as the generated kubeconfig.
	// Application state lives in the cluster, not here.
	StateDir string `json:"stateDir"`

	// Services selects which services to start. Nil means all of them.
	Services []Service `json:"services"`

	ShutdownTimeout Duration `json:"shutdownTimeout"`
	// ReadyTimeout bounds how long to wait for cluster components to become
	// ready. External components cannot have their clocks advanced, so waiting
	// on them is bounded polling rather than an injected clock.
	ReadyTimeout Duration `json:"readyTimeout"`
	LogLevel     string   `json:"logLevel"`

	// HooksDir holds ready.d and shutdown.d, scripts run on the host when
	// the instance becomes ready and before it stops (#285). A missing
	// directory runs nothing. Relative paths are taken from the working
	// directory `up` runs in.
	HooksDir string `json:"hooksDir"`
	// HookTimeout bounds each hook script.
	HookTimeout Duration `json:"hookTimeout"`

	// SeedFile is a seed document, the /admin/seed body, that `up` applies
	// once the services start and before the instance reports ready (#286).
	// Empty applies nothing.
	SeedFile string `json:"seedFile"`
}

// DefaultNodeImage is the pinned Kubernetes node image. It is duplicated from
// dependencies.json, which remains the source of truth; #31 keeps them in step.
const DefaultNodeImage = "kindest/node:v1.36.4"

// DefaultName is the instance name used when none is given.
const DefaultName = "cloudburrow"

// Default returns the configuration used when nothing else is specified.
func Default() Config {
	return Config{
		Name:        DefaultName,
		BindAddress: "127.0.0.1",
		AllowRemote: false,
		Endpoints: Endpoints{
			Control: 9000,
			Storage: 9001,
			PubSub:  9002,
			Tasks:   9003,
			Run:     9004,
			Secrets: 9006,
			// Always on: the project registry always exists, so its API does.
			ResourceManager: 9007,
			// Cloud Scheduler, beside Cloud Tasks in the in-process block.
			Scheduler: 9008,
			// 9080 rather than the 900x block: this is the port a developer
			// types into a browser, not one an SDK is pointed at.
			Ingress: 9080,
			// Back in the 900x block: this is an endpoint a client library is
			// pointed at, not one a human types into a browser.
			Metadata: 9005,
			Console:  9090,
			// The opt-in emulators get their own block, clear of the core
			// services' 900x range so neither has to move when the other grows.
			Firestore: 9010,
			Datastore: 9011,
			Bigtable:  9012,
			Spanner:   9013,
			BigQuery:  9014,
			// The Storage Read API: a second port for the same service.
			BigQueryStorage: 9015,
			Memorystore:     9016,
			CloudSQLMySQL:   9017,
			// OS-assigned. The port is not what enables local AI —
			// LocalAI.ModelPath is. A configured port with no model binds
			// nothing, so there is no endpoint answering every request with
			// "no runtime configured".
			LocalAI: 0,
		},
		Cluster: Cluster{
			Provider:  "kind",
			NodeImage: DefaultNodeImage,
			Namespace: "cloudburrow",
		},
		Mode:            ModePersistent,
		StateDir:        defaultStateDir(),
		Services:        nil, // nil means all; resolved by EnabledServices
		ShutdownTimeout: Duration(30 * time.Second),
		ReadyTimeout:    Duration(5 * time.Minute),
		HooksDir:        filepath.Join(".cloudburrow", "hooks"),
		HookTimeout:     Duration(5 * time.Minute),
		LogLevel:        "info",
	}
}

func defaultStateDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ".cloudburrow"
	}
	return filepath.Join(home, ".cloudburrow")
}

// DefaultProject is the project this instance serves by default: the one the
// console opens on, the ADC fixture and metadata server report, `env` exports,
// and the project registry is seeded with.
//
// It used to be the instance name, unconditionally. But the two follow
// different rules — an instance name may be 1–32 characters and start with a
// digit, a project ID must be 6–30 and start with a letter — so `up --name demo`
// started a healthy instance whose own default project every Cloud Run and Cloud
// Tasks call then refused as malformed. Nothing said the name was the cause.
//
// So the name is used when it is also a valid project ID, which covers the
// default instance and every existing instance that worked. Otherwise a project
// is derived from it deterministically, so the same name always yields the same
// project. An explicit Project wins over both.
func (c Config) DefaultProject() string {
	if c.Project != "" {
		return c.Project
	}
	return ProjectForName(c.Name)
}

// ProjectForName derives a valid project ID from an instance name.
//
// The name itself when it qualifies. Otherwise the smallest change that makes it
// qualify, so the result is still recognisably the instance's: a letter prefix
// when it starts with a digit, a "-local" suffix when it is too short, and
// truncation when it is too long.
func ProjectForName(name string) string {
	if resource.ValidProjectID(name) {
		return name
	}
	p := name
	if p == "" || p[0] < 'a' || p[0] > 'z' {
		p = "cb-" + p
	}
	if len(p) > 30 {
		p = p[:30]
	}
	p = strings.TrimRight(p, "-")
	if len(p) < 6 {
		p += "-local"
	}
	if resource.ValidProjectID(p) {
		return p
	}
	// Unreachable for any name Validate accepts; kept so a future change to the
	// name rule cannot make this return something that is not a project ID.
	return DefaultName
}

// ClusterName returns the kind cluster name for this instance. It always
// begins with "cloudburrow" so that ownership is identifiable and an unrelated
// cluster can never be mistaken for ours.
func (c Config) ClusterName() string {
	if c.Name == DefaultName {
		return DefaultName
	}
	return DefaultName + "-" + c.Name
}

// KubeconfigPath returns the explicit kubeconfig path for this instance.
func (c Config) KubeconfigPath() string {
	if c.Cluster.Kubeconfig != "" {
		return c.Cluster.Kubeconfig
	}
	return filepath.Join(c.StateDir, c.Name, "kubeconfig")
}

// InstanceDir is the per-instance directory holding host-side artifacts: the
// generated kubeconfig, the kind configuration, and anything else that
// belongs to this instance and not another.
func (c Config) InstanceDir() string {
	return filepath.Join(c.StateDir, c.Name)
}

// OwnerLabels are applied to every resource CloudBurrow creates, so cleanup can
// never touch something it does not own.
func (c Config) OwnerLabels() map[string]string {
	return map[string]string{
		"cloudburrow.dev/owned":    "true",
		"cloudburrow.dev/instance": c.Name,
	}
}

// EnabledServices returns the services to start, in deterministic order. A nil
// selection means all services; Validate rejects an explicitly empty one.
func (c Config) EnabledServices() []Service {
	if len(c.Services) == 0 {
		return AllServices()
	}
	set := make(map[Service]bool, len(c.Services))
	for _, s := range c.Services {
		set[s] = true
	}
	var out []Service
	for _, s := range KnownServices() {
		if set[s] {
			out = append(out, s)
		}
	}
	return out
}

// EphemeralServices returns enabled services whose state never survives a
// restart, so the CLI can say so rather than letting a user assume otherwise.
func (c Config) EphemeralServices() []Service {
	var out []Service
	for _, s := range c.EnabledServices() {
		if s.Persistence() == PersistenceNone || c.Mode == ModeEphemeral {
			out = append(out, s)
		}
	}
	return out
}

// IsLoopback reports whether BindAddress is a loopback address.
func (c Config) IsLoopback() bool {
	ip := net.ParseIP(c.BindAddress)
	return ip != nil && ip.IsLoopback()
}

// instanceNameRE matches names usable as a Kubernetes label value and as part
// of a kind cluster name.
var instanceNameRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,30}[a-z0-9])?$`)

// nodeImageRE requires an explicit tag or digest. An untagged reference would
// resolve to a mutable "latest", which ADR-0005 forbids in anything
// reproducible.
var nodeImageRE = regexp.MustCompile(`^[^:@\s]+(:[^:@\s]+|@sha256:[a-f0-9]{64})$`)

// FieldError is a single configuration problem, naming the field, the offending
// value, and what is wrong with it.
type FieldError struct {
	Field   string
	Value   string
	Message string
}

func (e FieldError) Error() string {
	if e.Value == "" {
		return fmt.Sprintf("%s: %s", e.Field, e.Message)
	}
	return fmt.Sprintf("%s: %s (got %q)", e.Field, e.Message, e.Value)
}

// ValidationError aggregates every problem found in one pass.
//
// Reporting all problems at once is deliberate: a developer fixing a config
// file should not have to restart six times to discover six mistakes.
type ValidationError struct {
	Problems []FieldError
}

func (e *ValidationError) Error() string {
	if len(e.Problems) == 1 {
		return "invalid configuration: " + e.Problems[0].Error()
	}
	var b strings.Builder
	fmt.Fprintf(&b, "invalid configuration (%d problems):", len(e.Problems))
	for _, p := range e.Problems {
		b.WriteString("\n  - ")
		b.WriteString(p.Error())
	}
	return b.String()
}

// Validate checks the configuration and returns a *ValidationError listing
// every problem, or nil when the configuration is usable.
//
// This runs before a cluster is created or a listener opened, so a
// misconfigured instance fails with an actionable message rather than
// half-starting.
func (c *Config) Validate() error {
	var problems []FieldError
	add := func(field, value, msg string) {
		problems = append(problems, FieldError{Field: field, Value: value, Message: msg})
	}

	// Instance name. It becomes a cluster name and a label value, so its
	// character set is not ours to choose.
	switch {
	case c.Name == "":
		add("name", "", "must not be empty")
	case !instanceNameRE.MatchString(c.Name):
		add("name", c.Name, "must be lowercase alphanumeric with internal hyphens, at most 32 characters")
	}

	// An explicit project must be one every service will accept. Refused here
	// rather than at the first API call, which is where it used to surface — as
	// a malformed resource name, far from its cause.
	if c.Project != "" && !resource.ValidProjectID(c.Project) {
		add("project", c.Project, "must be a valid project ID: 6-30 lowercase letters, "+
			"digits and hyphens, starting with a letter and not ending with a hyphen")
	}

	// Bind address. Hostnames are rejected: resolution can change what the
	// process binds between runs, which is a poor property for a security
	// boundary to have.
	ip := net.ParseIP(c.BindAddress)
	switch {
	case c.BindAddress == "":
		add("bindAddress", "", "must not be empty")
	case ip == nil:
		add("bindAddress", c.BindAddress, "must be an IP address literal, not a hostname")
	case !ip.IsLoopback() && !c.AllowRemote:
		add("bindAddress", c.BindAddress,
			"binds a non-loopback address, which exposes an unauthenticated emulator "+
				"to the network; pass --allow-remote to confirm")
	}

	// Endpoints. 0 means "let the OS choose", so only non-zero ports are
	// checked for range and uniqueness.
	seen := map[int]string{}
	for _, np := range c.Endpoints.named() {
		field := "endpoints." + np.Name
		switch {
		case np.Port < 0 || np.Port > 65535:
			add(field, fmt.Sprint(np.Port), "must be between 0 and 65535")
		case np.Port == 0:
			// OS-assigned; duplicates are impossible.
		default:
			if other, dup := seen[np.Port]; dup {
				add(field, fmt.Sprint(np.Port),
					fmt.Sprintf("duplicates endpoints.%s; each surface needs its own port", other))
			} else {
				seen[np.Port] = np.Name
			}
		}
	}

	// Cluster.
	if c.Cluster.Provider != "kind" {
		add("cluster.provider", c.Cluster.Provider, `only "kind" is supported`)
	}
	switch {
	case c.Cluster.NodeImage == "":
		add("cluster.nodeImage", "", "must be set")
	case !nodeImageRE.MatchString(c.Cluster.NodeImage):
		add("cluster.nodeImage", c.Cluster.NodeImage,
			"must carry an explicit tag or digest; an untagged reference is a mutable target")
	}
	switch {
	case c.Cluster.Namespace == "":
		add("cluster.namespace", "", "must not be empty")
	case !instanceNameRE.MatchString(c.Cluster.Namespace):
		add("cluster.namespace", c.Cluster.Namespace, "must be a valid Kubernetes namespace name")
	}

	// Mode.
	switch c.Mode {
	case ModeEphemeral, ModePersistent:
	case "":
		add("mode", "", fmt.Sprintf("must be %q or %q", ModeEphemeral, ModePersistent))
	default:
		add("mode", string(c.Mode), fmt.Sprintf("must be %q or %q", ModeEphemeral, ModePersistent))
	}

	// State directory. Host-side only; an existing path must be usable.
	if strings.TrimSpace(c.StateDir) == "" {
		add("stateDir", "", "must be set")
	} else if info, err := os.Stat(c.StateDir); err == nil && !info.IsDir() {
		add("stateDir", c.StateDir, "exists but is not a directory")
	}

	// Services.
	known := map[Service]bool{}
	for _, s := range KnownServices() {
		known[s] = true
	}
	if c.Services != nil && len(c.Services) == 0 {
		// Distinguishable from nil, which means "unset, so start everything".
		add("services", "", "must name at least one service; omit the flag to start all of them")
	}
	dupes := map[Service]bool{}
	for _, s := range c.Services {
		switch {
		case !known[s]:
			add("services", string(s), "unknown service; must be one of "+joinServices(KnownServices()))
		case dupes[s]:
			add("services", string(s), "listed more than once")
		default:
			dupes[s] = true
		}
	}

	// Timeouts.
	if c.ShutdownTimeout <= 0 {
		add("shutdownTimeout", c.ShutdownTimeout.String(), "must be greater than zero")
	}
	if c.ReadyTimeout <= 0 {
		add("readyTimeout", c.ReadyTimeout.String(), "must be greater than zero")
	}

	// Log level.
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		add("logLevel", c.LogLevel, "must be one of debug, info, warn, error")
	}

	if len(problems) > 0 {
		sort.SliceStable(problems, func(i, j int) bool { return problems[i].Field < problems[j].Field })
		return &ValidationError{Problems: problems}
	}
	return nil
}

func joinServices(s []Service) string {
	parts := make([]string, len(s))
	for i, v := range s {
		parts[i] = string(v)
	}
	return strings.Join(parts, ", ")
}
