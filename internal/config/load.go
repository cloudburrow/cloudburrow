package config

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/hostguard"
)

// EnvPrefix is the prefix for every CloudBurrow environment variable.
const EnvPrefix = "CLOUDBURROW_"

// DefaultFileName is the configuration file looked for in the working
// directory when no path is given.
const DefaultFileName = "cloudburrow.json"

// Options controls Load's inputs. Every external dependency is injectable so
// that tests never touch process globals or the real environment.
type Options struct {
	// Args are command-line arguments excluding the program and subcommand name.
	Args []string
	// Getenv looks up an environment variable. Nil means os.Getenv.
	Getenv func(string) string
	// WorkDir is the directory searched for DefaultFileName. Empty means the
	// process working directory.
	WorkDir string
	// Output receives flag parsing errors and usage. Nil means io.Discard.
	Output io.Writer
}

func (o Options) getenv(key string) string {
	if o.Getenv == nil {
		return os.Getenv(key)
	}
	return o.Getenv(key)
}

// Load resolves configuration using one documented precedence:
//
//	flags  >  environment  >  file  >  defaults
//
// The configuration file itself is located by the same rule: the --config flag,
// then CLOUDBURROW_CONFIG, then ./cloudburrow.json when it exists.
//
// Load validates the result, so a returned Config is always usable. A
// *ValidationError lists every problem at once.
func Load(opts Options) (Config, error) {
	// Flags are parsed first because --config decides which file to read, but
	// they are *applied* last so that they outrank every other source. fs.Visit
	// reports only the flags actually present on the command line, which is what
	// makes "unset flag does not override the environment" work.
	fs, raw := newFlagSet(opts.Output)
	if err := fs.Parse(opts.Args); err != nil {
		return Config{}, err
	}
	if extra := fs.Args(); len(extra) > 0 {
		return Config{}, fmt.Errorf("unexpected argument %q", extra[0])
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })

	cfg := Default()

	path, explicit := configFilePath(opts, raw, set)
	var data []byte
	if path != "" {
		var err error
		if data, err = readFile(path, explicit); err != nil {
			return Config{}, err
		}
	}

	// The port base moves the defaults, so it is resolved — by the same
	// precedence as everything else — before any source's own ports are
	// applied on top. That is what keeps an explicit port where it was put.
	base, err := resolvePortBase(data, opts.getenv, raw, set)
	if err != nil {
		return Config{}, err
	}
	// An out-of-range base is left for Validate to name; moving the defaults
	// by it would only add a list of endpoints the user never set.
	if base >= MinPortBase && base <= MaxPortBase() {
		cfg.Endpoints = cfg.Endpoints.withPortBase(base)
	}

	// File.
	var fileKeys map[string]json.RawMessage
	if data != nil {
		keys, err := applyFile(&cfg, path, explicit, data)
		if err != nil {
			return Config{}, err
		}
		fileKeys = keys
		cfg.Source.File, _ = filepath.Abs(path)
		cfg.Source.Discovered = !explicit
	}

	// Environment.
	if err := applyEnv(&cfg, opts.getenv); err != nil {
		return Config{}, err
	}

	// Flags.
	applyFlags(&cfg, raw, set)
	cfg.PortBase = base

	// Normalise before validating so that error messages name the path the
	// process will actually use.
	if cfg.StateDir != "" {
		if abs, err := filepath.Abs(cfg.StateDir); err == nil {
			cfg.StateDir = abs
		}
	}
	// Absolute, so a detached `up` whose working directory is the same
	// still finds it, and so status and logs name one path.
	if cfg.HooksDir != "" {
		if abs, err := filepath.Abs(cfg.HooksDir); err == nil {
			cfg.HooksDir = abs
		}
	}
	// The signed-URL certificates the same way, so `up` reads the files the
	// developer named wherever it was started from (#577).
	for email, path := range cfg.Storage.SigningCerts {
		if abs, err := filepath.Abs(path); err == nil {
			cfg.Storage.SigningCerts[email] = abs
		}
	}
	// The allowed origins in the form a browser sends them (#677); one that
	// is not an origin is left for Validate to name.
	for i, o := range cfg.Storage.CORSAllowOrigins {
		if norm, err := hostguard.ParseOrigin(o); err == nil {
			cfg.Storage.CORSAllowOrigins[i] = norm
		}
	}
	if cfg.SeedFile != "" {
		if abs, err := filepath.Abs(cfg.SeedFile); err == nil {
			cfg.SeedFile = abs
		}
	}
	if cfg.Cluster.Kubeconfig != "" {
		if abs, err := filepath.Abs(cfg.Cluster.Kubeconfig); err == nil {
			cfg.Cluster.Kubeconfig = abs
		}
	}

	// Provenance for the trust gate (#598). A setting the developer named,
	// by flag, environment or a file they pointed at, is their choice; one
	// that came from a discovered ./cloudburrow.json, or a default resolved
	// against the working directory, is the repository's.
	namedInFile := func(key string) bool {
		_, ok := fileKeys[key]
		return ok && explicit
	}
	cfg.Source.HooksDirNamed = set["hooks-dir"] || opts.getenv(EnvPrefix+"HOOKS_DIR") != "" || namedInFile("hooksDir")
	if set["state-dir"] || opts.getenv(EnvPrefix+"STATE_DIR") != "" || namedInFile("stateDir") {
		cfg.Source.TrustDir = cfg.StateDir
	} else if d := defaultStateDir(); filepath.IsAbs(d) {
		cfg.Source.TrustDir = d
	}

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// rawFlags holds flag values before precedence is applied.
type rawFlags struct {
	configPath      string
	name            string
	project         string
	bindAddress     string
	allowRemote     bool
	portBase        int
	control         int
	storage         int
	pubsub          int
	tasks           int
	run             int
	ingress         int
	secrets         int
	kms             int
	resourceManager int
	scheduler       int
	logging         int
	firestore       int
	datastore       int
	bigtable        int
	spanner         int
	bigquery        int
	bigqueryStorage int
	memorystore     int
	cloudsqlMySQL   int
	cloudsql        int
	metadata        int
	consolePort     int
	localAIPort     int
	localAIModel    string
	localAIModelID  string
	localAIImage    string
	localAIAliases  string
	localAIEmbed    bool
	provider        string
	nodeImage       string
	namespace       string
	kubeconfig      string
	mode            string
	stateDir        string
	hooksDir        string
	seedFile        string
	hookTimeout     time.Duration
	hookEnv         string
	services        string
	shutdownTimeout time.Duration
	readyTimeout    time.Duration
	logLevel        string
	corsOrigins     listFlag
}

// listFlag is a flag that may be repeated, each value also comma-separated;
// the values accumulate in order.
type listFlag []string

func (f *listFlag) String() string { return strings.Join(*f, ",") }

func (f *listFlag) Set(v string) error {
	*f = append(*f, splitList(v)...)
	return nil
}

func newFlagSet(out io.Writer) (*flag.FlagSet, *rawFlags) {
	if out == nil {
		out = io.Discard
	}
	r := &rawFlags{}
	fs := flag.NewFlagSet("up", flag.ContinueOnError)
	fs.SetOutput(out)

	fs.StringVar(&r.configPath, "config", "", "path to a JSON configuration file")
	fs.StringVar(&r.name, "name", "", "instance name; scopes the cluster, namespace and every owned resource")
	fs.StringVar(&r.project, "project", "", "default project ID (default: the instance name when it is a valid project ID, otherwise derived from it)")
	fs.StringVar(&r.bindAddress, "bind-address", "", "IP address to publish host endpoints on")
	fs.BoolVar(&r.allowRemote, "allow-remote", false, "permit binding a non-loopback address (unsafe)")
	fs.IntVar(&r.portBase, "port-base", 0, "move every fixed default port so the layout starts here (control here, ingress +80, console +90); explicit --port-* flags still win (0 = the defaults, from 9000)")
	fs.IntVar(&r.control, "port-control", 0, "control port for health, readiness and admin (0 = OS-assigned)")
	fs.IntVar(&r.storage, "port-storage", 0, "Cloud Storage host port (0 = OS-assigned)")
	fs.IntVar(&r.pubsub, "port-pubsub", 0, "Pub/Sub host port (0 = OS-assigned)")
	fs.IntVar(&r.tasks, "port-tasks", 0, "Cloud Tasks host port (0 = OS-assigned)")
	fs.IntVar(&r.run, "port-run", 0, "Cloud Run host port (0 = OS-assigned)")
	fs.IntVar(&r.secrets, "port-secrets", 0, "Secret Manager host port (0 = OS-assigned)")
	fs.IntVar(&r.kms, "port-kms", 0, "Cloud KMS host port (0 = OS-assigned; default 9018)")
	fs.IntVar(&r.resourceManager, "port-resourcemanager", 0, "Resource Manager v3 Projects API host port (0 = OS-assigned; default 9007)")
	fs.IntVar(&r.scheduler, "port-scheduler", 0, "Cloud Scheduler host port (0 = OS-assigned; default 9008)")
	fs.IntVar(&r.logging, "port-logging", 0, "Cloud Logging host port (0 = OS-assigned; default 9009)")
	fs.IntVar(&r.firestore, "port-firestore", 0, "Firestore emulator host port (0 = OS-assigned; default 9010)")
	fs.IntVar(&r.datastore, "port-datastore", 0, "Datastore emulator host port (0 = OS-assigned; default 9011)")
	fs.IntVar(&r.bigtable, "port-bigtable", 0, "Bigtable emulator host port (0 = OS-assigned; default 9012)")
	fs.IntVar(&r.spanner, "port-spanner", 0, "Spanner emulator host port (0 = OS-assigned; default 9013)")
	fs.IntVar(&r.bigquery, "port-bigquery", 0, "BigQuery emulator REST host port (0 = OS-assigned; default 9014)")
	fs.IntVar(&r.bigqueryStorage, "port-bigquery-storage", 0, "BigQuery Storage Read API gRPC host port (0 = OS-assigned; default 9015)")
	fs.IntVar(&r.memorystore, "port-memorystore", 0, "Memorystore (Valkey, RESP) host port (0 = OS-assigned; default 9016)")
	fs.IntVar(&r.cloudsqlMySQL, "port-cloudsql-mysql", 0, "Cloud SQL for MySQL host port (0 = OS-assigned; default 9017)")
	fs.IntVar(&r.cloudsql, "port-cloudsql", 0, "Cloud SQL for PostgreSQL host port (0 = OS-assigned; default 9019)")
	fs.IntVar(&r.ingress, "port-ingress", 0, "host port for the cluster ingress gateway (0 = OS-assigned; fixed at cluster creation)")
	fs.IntVar(&r.consolePort, "port-console", 0, "host port for the web console (0 = OS-assigned)")
	fs.IntVar(&r.metadata, "port-metadata", 0, "host port for the local metadata server (0 = OS-assigned)")
	fs.IntVar(&r.localAIPort, "port-localai", 0, "host port for the local generation endpoint (0 = OS-assigned; requires -local-ai-model)")
	fs.StringVar(&r.localAIModel, "local-ai-model", "", "host path to a .litertlm model; enables the local generation endpoint")
	fs.StringVar(&r.localAIModelID, "local-ai-model-id", "", "model ID clients must request (default: the catalogue entry for the artifact)")
	fs.StringVar(&r.localAIImage, "local-ai-image", "", "local AI runtime image (default: the image published with this release; for a dev build, the one make litert-lm builds)")
	fs.BoolVar(&r.localAIEmbed, "local-ai-embeddings", false, "serve text-embedding :predict for embeddinggemma-300m-onnx-community, a community ONNX conversion that is NOT an official Google artifact (needs a -tags onnx build)")
	fs.StringVar(&r.localAIAliases, "local-ai-alias", "", "comma-separated model IDs that resolve to the configured model (explicit substitution)")
	fs.StringVar(&r.provider, "cluster-provider", "", "cluster provider (only kind is supported)")
	fs.StringVar(&r.nodeImage, "node-image", "", "pinned kind node image, which fixes the Kubernetes version")
	fs.StringVar(&r.namespace, "namespace", "", "namespace for CloudBurrow-managed workloads")
	fs.StringVar(&r.kubeconfig, "kubeconfig", "", "explicit kubeconfig path (never the developer default)")
	fs.StringVar(&r.mode, "mode", "", "state mode: ephemeral or persistent")
	fs.StringVar(&r.stateDir, "state-dir", "", "host directory for the generated kubeconfig and other artifacts")
	fs.StringVar(&r.hooksDir, "hooks-dir", "", "directory of ready.d and shutdown.d hook scripts (default .cloudburrow/hooks)")
	fs.StringVar(&r.seedFile, "seed-file", "", "seed document (the /admin/seed body) applied when up starts")
	fs.DurationVar(&r.hookTimeout, "hook-timeout", 0, "time limit for each hook script (default 5m)")
	fs.StringVar(&r.hookEnv, "hook-env", "", "comma-separated variables of this environment passed to hooks beyond PATH, HOME, LANG, TMPDIR and the like")
	fs.StringVar(&r.services, "services", "", servicesUsage())
	fs.DurationVar(&r.shutdownTimeout, "shutdown-timeout", 0, "bounded time to drain on shutdown")
	fs.DurationVar(&r.readyTimeout, "ready-timeout", 0, "bounded time to wait for cluster components to become ready")
	fs.StringVar(&r.logLevel, "log-level", "", "log level: trace, debug, info, warn, error")
	fs.Var(&r.corsOrigins, "cors-allow-origin", "web origin (scheme://host[:port]) whose browser requests Cloud Storage answers, beyond loopback ones; repeatable or comma-separated (default: loopback origins only)")

	return fs, r
}

// configFilePath resolves which file to read and whether it was requested
// explicitly. An explicitly requested file that is missing is an error; the
// conventional ./cloudburrow.json is silently skipped when absent.
func configFilePath(opts Options, raw *rawFlags, set map[string]bool) (path string, explicit bool) {
	if set["config"] && raw.configPath != "" {
		return raw.configPath, true
	}
	if v := opts.getenv(EnvPrefix + "CONFIG"); v != "" {
		return v, true
	}
	candidate := filepath.Join(opts.WorkDir, DefaultFileName)
	if _, err := os.Stat(candidate); err == nil {
		return candidate, false
	}
	return "", false
}

// readFile reads the configuration file. A conventional file that has gone
// missing yields nil data and no error.
func readFile(path string, explicit bool) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) && !explicit {
			return nil, nil
		}
		return nil, fmt.Errorf("read config file %s: %w", path, err)
	}
	return data, nil
}

// resolvePortBase returns the port base: the --port-base flag, then
// CLOUDBURROW_PORT_BASE, then the file's portBase, then 0.
func resolvePortBase(data []byte, getenv func(string) string, raw *rawFlags, set map[string]bool) (int, error) {
	if set["port-base"] {
		return raw.portBase, nil
	}
	if v := getenv(EnvPrefix + "PORT_BASE"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return 0, fmt.Errorf("%sPORT_BASE: must be an integer (got %q)", EnvPrefix, v)
		}
		return n, nil
	}
	if data != nil {
		// Lenient: only portBase is wanted here. A malformed file is
		// reported by applyFile's strict decode.
		var f struct {
			PortBase int `json:"portBase"`
		}
		_ = json.Unmarshal(data, &f)
		return f.PortBase, nil
	}
	return 0, nil
}

// applyFile decodes the file's data onto cfg and returns the top-level keys
// it set, refusing exposure a discovered file may not set.
func applyFile(cfg *Config, path string, explicit bool, data []byte) (map[string]json.RawMessage, error) {
	// Decode onto the defaults so that absent keys keep their default value
	// rather than becoming zero.
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(cfg); err != nil {
		return nil, fmt.Errorf("parse config file %s: %w", path, err)
	}
	keys := map[string]json.RawMessage{}
	if err := json.Unmarshal(data, &keys); err != nil {
		return nil, fmt.Errorf("parse config file %s: %w", path, err)
	}
	if err := refuseFileExposure(cfg, path, explicit, keys); err != nil {
		return nil, err
	}
	return keys, nil
}

// refuseFileExposure keeps the decision to expose the emulator to the
// network with the developer (#598). A cloned repository's cloudburrow.json
// is read by `up` without being named, so were it able to set allowRemote
// and a non-loopback bindAddress, running `up` in that checkout would put an
// unauthenticated emulator on the LAN on the developer's behalf. So no file
// confirms allowRemote, and a discovered file cannot name a non-loopback
// address at all; a file the developer named with --config may, but
// Validate still wants --allow-remote or CLOUDBURROW_ALLOW_REMOTE for it.
func refuseFileExposure(cfg *Config, path string, explicit bool, keys map[string]json.RawMessage) error {
	if _, ok := keys["allowRemote"]; ok && cfg.AllowRemote {
		return fmt.Errorf("config file %s sets allowRemote, which a file cannot do: exposing the emulator "+
			"to the network is confirmed only by --allow-remote or %sALLOW_REMOTE=true; remove the key", path, EnvPrefix)
	}
	if !explicit && len(cfg.Storage.CORSAllowOrigins) > 0 {
		return fmt.Errorf("config file %s, found in the working directory, sets storage.corsAllowOrigins; "+
			"a discovered file cannot let other web sites call the emulator (#677): pass --cors-allow-origin "+
			"(or %sCORS_ALLOW_ORIGIN, or name the file with --config), and remove the key", path, EnvPrefix)
	}
	if _, ok := keys["bindAddress"]; ok && !explicit {
		if ip := net.ParseIP(cfg.BindAddress); ip != nil && !ip.IsLoopback() {
			return fmt.Errorf("config file %s, found in the working directory, sets bindAddress %q, a non-loopback address; "+
				"a discovered file cannot choose one: pass --bind-address with --allow-remote "+
				"(or %sBIND_ADDRESS with %sALLOW_REMOTE=true), and remove the key", path, cfg.BindAddress, EnvPrefix, EnvPrefix)
		}
	}
	return nil
}

func applyEnv(cfg *Config, getenv func(string) string) error {
	str := func(key string, dst *string) {
		if v := getenv(EnvPrefix + key); v != "" {
			*dst = v
		}
	}
	intVar := func(key string, dst *int) error {
		v := getenv(EnvPrefix + key)
		if v == "" {
			return nil
		}
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("%s%s: must be an integer (got %q)", EnvPrefix, key, v)
		}
		*dst = n
		return nil
	}

	str("NAME", &cfg.Name)
	str("PROJECT", &cfg.Project)
	str("BIND_ADDRESS", &cfg.BindAddress)

	if v := getenv(EnvPrefix + "ALLOW_REMOTE"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("%sALLOW_REMOTE: must be a boolean (got %q)", EnvPrefix, v)
		}
		cfg.AllowRemote = b
	}

	for _, p := range []struct {
		key string
		dst *int
	}{
		{"PORT_CONTROL", &cfg.Endpoints.Control},
		{"PORT_STORAGE", &cfg.Endpoints.Storage},
		{"PORT_PUBSUB", &cfg.Endpoints.PubSub},
		{"PORT_TASKS", &cfg.Endpoints.Tasks},
		{"PORT_RUN", &cfg.Endpoints.Run},
		{"PORT_SECRETS", &cfg.Endpoints.Secrets},
		{"PORT_KMS", &cfg.Endpoints.KMS},
		{"PORT_RESOURCEMANAGER", &cfg.Endpoints.ResourceManager},
		{"PORT_SCHEDULER", &cfg.Endpoints.Scheduler},
		{"PORT_LOGGING", &cfg.Endpoints.Logging},
		{"PORT_FIRESTORE", &cfg.Endpoints.Firestore},
		{"PORT_DATASTORE", &cfg.Endpoints.Datastore},
		{"PORT_BIGTABLE", &cfg.Endpoints.Bigtable},
		{"PORT_SPANNER", &cfg.Endpoints.Spanner},
		{"PORT_BIGQUERY", &cfg.Endpoints.BigQuery},
		{"PORT_BIGQUERY_STORAGE", &cfg.Endpoints.BigQueryStorage},
		{"PORT_MEMORYSTORE", &cfg.Endpoints.Memorystore},
		{"PORT_CLOUDSQL_MYSQL", &cfg.Endpoints.CloudSQLMySQL},
		{"PORT_CLOUDSQL", &cfg.Endpoints.CloudSQL},
		{"PORT_INGRESS", &cfg.Endpoints.Ingress},
		{"PORT_METADATA", &cfg.Endpoints.Metadata},
		{"PORT_CONSOLE", &cfg.Endpoints.Console},
	} {
		if err := intVar(p.key, p.dst); err != nil {
			return err
		}
	}

	if v := getenv(EnvPrefix + "MODE"); v != "" {
		cfg.Mode = Mode(v)
	}
	if v := getenv(EnvPrefix + "STORAGE_BACKEND"); v != "" {
		cfg.Storage.Backend = v // refused by Validate (#519)
	}
	str("CLUSTER_PROVIDER", &cfg.Cluster.Provider)
	str("NODE_IMAGE", &cfg.Cluster.NodeImage)
	str("NAMESPACE", &cfg.Cluster.Namespace)
	str("KUBECONFIG_PATH", &cfg.Cluster.Kubeconfig)
	str("STATE_DIR", &cfg.StateDir)
	str("HOOKS_DIR", &cfg.HooksDir)
	str("SEED_FILE", &cfg.SeedFile)
	if v := getenv(EnvPrefix + "HOOK_ENV"); v != "" {
		cfg.HookEnv = splitList(v)
	}
	if v := getenv(EnvPrefix + "HOOK_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("%sHOOK_TIMEOUT: must be a duration such as 2m (got %q)", EnvPrefix, v)
		}
		cfg.HookTimeout = Duration(d)
	}

	if v := getenv(EnvPrefix + "SERVICES"); v != "" {
		cfg.Services = parseServices(v)
	}

	if v := getenv(EnvPrefix + "SHUTDOWN_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("%sSHUTDOWN_TIMEOUT: must be a duration such as 15s (got %q)", EnvPrefix, v)
		}
		cfg.ShutdownTimeout = Duration(d)
	}

	if v := getenv(EnvPrefix + "READY_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("%sREADY_TIMEOUT: must be a duration such as 5m (got %q)", EnvPrefix, v)
		}
		cfg.ReadyTimeout = Duration(d)
	}

	str("LOG_LEVEL", &cfg.LogLevel)
	if v := getenv(EnvPrefix + "CORS_ALLOW_ORIGIN"); v != "" {
		cfg.Storage.CORSAllowOrigins = splitList(v)
	}
	return nil
}

func applyFlags(cfg *Config, raw *rawFlags, set map[string]bool) {
	if set["name"] {
		cfg.Name = raw.name
	}
	if set["project"] {
		cfg.Project = raw.project
	}
	if set["bind-address"] {
		cfg.BindAddress = raw.bindAddress
	}
	if set["allow-remote"] {
		cfg.AllowRemote = raw.allowRemote
	}
	for _, p := range []struct {
		name string
		src  int
		dst  *int
	}{
		{"port-control", raw.control, &cfg.Endpoints.Control},
		{"port-storage", raw.storage, &cfg.Endpoints.Storage},
		{"port-pubsub", raw.pubsub, &cfg.Endpoints.PubSub},
		{"port-tasks", raw.tasks, &cfg.Endpoints.Tasks},
		{"port-run", raw.run, &cfg.Endpoints.Run},
		{"port-secrets", raw.secrets, &cfg.Endpoints.Secrets},
		{"port-kms", raw.kms, &cfg.Endpoints.KMS},
		{"port-resourcemanager", raw.resourceManager, &cfg.Endpoints.ResourceManager},
		{"port-scheduler", raw.scheduler, &cfg.Endpoints.Scheduler},
		{"port-logging", raw.logging, &cfg.Endpoints.Logging},
		{"port-firestore", raw.firestore, &cfg.Endpoints.Firestore},
		{"port-datastore", raw.datastore, &cfg.Endpoints.Datastore},
		{"port-bigtable", raw.bigtable, &cfg.Endpoints.Bigtable},
		{"port-spanner", raw.spanner, &cfg.Endpoints.Spanner},
		{"port-bigquery", raw.bigquery, &cfg.Endpoints.BigQuery},
		{"port-bigquery-storage", raw.bigqueryStorage, &cfg.Endpoints.BigQueryStorage},
		{"port-memorystore", raw.memorystore, &cfg.Endpoints.Memorystore},
		{"port-cloudsql-mysql", raw.cloudsqlMySQL, &cfg.Endpoints.CloudSQLMySQL},
		{"port-cloudsql", raw.cloudsql, &cfg.Endpoints.CloudSQL},
		{"port-ingress", raw.ingress, &cfg.Endpoints.Ingress},
		{"port-metadata", raw.metadata, &cfg.Endpoints.Metadata},
		{"port-console", raw.consolePort, &cfg.Endpoints.Console},
		{"port-localai", raw.localAIPort, &cfg.Endpoints.LocalAI},
	} {
		if set[p.name] {
			*p.dst = p.src
		}
	}
	if set["local-ai-model"] {
		cfg.LocalAI.ModelPath = raw.localAIModel
	}
	if set["local-ai-model-id"] {
		cfg.LocalAI.ModelID = raw.localAIModelID
	}
	if set["local-ai-image"] {
		cfg.LocalAI.Image = raw.localAIImage
	}
	if set["local-ai-embeddings"] {
		cfg.LocalAI.Embeddings = raw.localAIEmbed
	}
	if set["local-ai-alias"] {
		cfg.LocalAI.Aliases = splitList(raw.localAIAliases)
	}
	if set["mode"] {
		cfg.Mode = Mode(raw.mode)
	}
	if set["cluster-provider"] {
		cfg.Cluster.Provider = raw.provider
	}
	if set["node-image"] {
		cfg.Cluster.NodeImage = raw.nodeImage
	}
	if set["namespace"] {
		cfg.Cluster.Namespace = raw.namespace
	}
	if set["kubeconfig"] {
		cfg.Cluster.Kubeconfig = raw.kubeconfig
	}
	if set["state-dir"] {
		cfg.StateDir = raw.stateDir
	}
	if set["hooks-dir"] {
		cfg.HooksDir = raw.hooksDir
	}
	if set["seed-file"] {
		cfg.SeedFile = raw.seedFile
	}
	if set["hook-timeout"] {
		cfg.HookTimeout = Duration(raw.hookTimeout)
	}
	if set["hook-env"] {
		cfg.HookEnv = splitList(raw.hookEnv)
	}
	if set["services"] {
		cfg.Services = parseServices(raw.services)
	}
	if set["shutdown-timeout"] {
		cfg.ShutdownTimeout = Duration(raw.shutdownTimeout)
	}
	if set["ready-timeout"] {
		cfg.ReadyTimeout = Duration(raw.readyTimeout)
	}
	if set["log-level"] {
		cfg.LogLevel = raw.logLevel
	}
	if set["cors-allow-origin"] {
		cfg.Storage.CORSAllowOrigins = append([]string(nil), raw.corsOrigins...)
	}
}

// parseServices splits a comma-separated list. Unknown names are preserved so
// that Validate can report them by name rather than silently dropping them.
func parseServices(v string) []Service {
	var out []Service
	for _, part := range strings.Split(v, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		out = append(out, Service(strings.ToLower(part)))
	}
	if out == nil {
		// An explicitly empty list is not the same as "unset"; keep it
		// non-nil so it does not silently fall back to "all services".
		return []Service{}
	}
	return out
}

// Usage writes the flag documentation for `cloudburrow up`.
func Usage(w io.Writer) {
	fs, _ := newFlagSet(w)
	fs.PrintDefaults()
}

// splitList splits a comma-separated flag value, dropping blanks.
func splitList(v string) []string {
	var out []string
	for _, part := range strings.Split(v, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// servicesUsage names the default services and the opt-in ones, from the
// same lists EnabledServices uses, so the help cannot drift from them (#708).
func servicesUsage() string {
	names := func(ss []Service) string {
		out := make([]string, len(ss))
		for i, s := range ss {
			out[i] = string(s)
		}
		return strings.Join(out, ",")
	}
	return "comma-separated services to start (default " + names(AllServices()) +
		"; opt-in: " + names(OptionalServices()) + ")"
}
