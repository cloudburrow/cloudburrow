package config

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
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

	// File.
	path, explicit := configFilePath(opts, raw, set)
	if path != "" {
		if err := applyFile(&cfg, path, explicit); err != nil {
			return Config{}, err
		}
	}

	// Environment.
	if err := applyEnv(&cfg, opts.getenv); err != nil {
		return Config{}, err
	}

	// Flags.
	applyFlags(&cfg, raw, set)

	// Normalise before validating so that error messages name the path the
	// process will actually use.
	if cfg.StateDir != "" {
		if abs, err := filepath.Abs(cfg.StateDir); err == nil {
			cfg.StateDir = abs
		}
	}
	if cfg.Cluster.Kubeconfig != "" {
		if abs, err := filepath.Abs(cfg.Cluster.Kubeconfig); err == nil {
			cfg.Cluster.Kubeconfig = abs
		}
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
	bindAddress     string
	allowRemote     bool
	control         int
	storage         int
	pubsub          int
	tasks           int
	run             int
	ingress         int
	secrets         int
	metadata        int
	provider        string
	nodeImage       string
	namespace       string
	kubeconfig      string
	mode            string
	stateDir        string
	services        string
	shutdownTimeout time.Duration
	readyTimeout    time.Duration
	logLevel        string
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
	fs.StringVar(&r.bindAddress, "bind-address", "", "IP address to publish host endpoints on")
	fs.BoolVar(&r.allowRemote, "allow-remote", false, "permit binding a non-loopback address (unsafe)")
	fs.IntVar(&r.control, "port-control", 0, "control port for health, readiness and admin (0 = OS-assigned)")
	fs.IntVar(&r.storage, "port-storage", 0, "Cloud Storage host port (0 = OS-assigned)")
	fs.IntVar(&r.pubsub, "port-pubsub", 0, "Pub/Sub host port (0 = OS-assigned)")
	fs.IntVar(&r.tasks, "port-tasks", 0, "Cloud Tasks host port (0 = OS-assigned)")
	fs.IntVar(&r.run, "port-run", 0, "Cloud Run host port (0 = OS-assigned)")
	fs.IntVar(&r.secrets, "port-secrets", 0, "Secret Manager host port (0 = OS-assigned)")
	fs.IntVar(&r.ingress, "port-ingress", 0, "host port for the cluster ingress gateway (0 = OS-assigned; fixed at cluster creation)")
	fs.IntVar(&r.metadata, "port-metadata", 0, "host port for the local metadata server (0 = OS-assigned)")
	fs.StringVar(&r.provider, "cluster-provider", "", "cluster provider (only kind is supported)")
	fs.StringVar(&r.nodeImage, "node-image", "", "pinned kind node image, which fixes the Kubernetes version")
	fs.StringVar(&r.namespace, "namespace", "", "namespace for CloudBurrow-managed workloads")
	fs.StringVar(&r.kubeconfig, "kubeconfig", "", "explicit kubeconfig path (never the developer default)")
	fs.StringVar(&r.mode, "mode", "", "state mode: ephemeral or persistent")
	fs.StringVar(&r.stateDir, "state-dir", "", "host directory for the generated kubeconfig and other artifacts")
	fs.StringVar(&r.services, "services", "", "comma-separated services to start (default all)")
	fs.DurationVar(&r.shutdownTimeout, "shutdown-timeout", 0, "bounded time to drain on shutdown")
	fs.DurationVar(&r.readyTimeout, "ready-timeout", 0, "bounded time to wait for cluster components to become ready")
	fs.StringVar(&r.logLevel, "log-level", "", "log level: debug, info, warn, error")

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

func applyFile(cfg *Config, path string, explicit bool) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) && !explicit {
			return nil
		}
		return fmt.Errorf("read config file %s: %w", path, err)
	}

	// Decode onto the defaults so that absent keys keep their default value
	// rather than becoming zero.
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(cfg); err != nil {
		return fmt.Errorf("parse config file %s: %w", path, err)
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
		{"PORT_INGRESS", &cfg.Endpoints.Ingress},
		{"PORT_METADATA", &cfg.Endpoints.Metadata},
	} {
		if err := intVar(p.key, p.dst); err != nil {
			return err
		}
	}

	if v := getenv(EnvPrefix + "MODE"); v != "" {
		cfg.Mode = Mode(v)
	}
	str("CLUSTER_PROVIDER", &cfg.Cluster.Provider)
	str("NODE_IMAGE", &cfg.Cluster.NodeImage)
	str("NAMESPACE", &cfg.Cluster.Namespace)
	str("KUBECONFIG_PATH", &cfg.Cluster.Kubeconfig)
	str("STATE_DIR", &cfg.StateDir)

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
	return nil
}

func applyFlags(cfg *Config, raw *rawFlags, set map[string]bool) {
	if set["name"] {
		cfg.Name = raw.name
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
		{"port-ingress", raw.ingress, &cfg.Endpoints.Ingress},
		{"port-metadata", raw.metadata, &cfg.Endpoints.Metadata},
	} {
		if set[p.name] {
			*p.dst = p.src
		}
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
