package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"sort"
	"strconv"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/components"
	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/metadata"
	"github.com/cloudburrow/cloudburrow/internal/netfwd"
)

// runEnv prints the environment that points Google tooling at this instance.
//
// It is a separate command rather than part of `up` because `up` runs in the
// foreground: its output cannot be evaluated by a shell. `eval "$(cloudburrow
// env)"` is the whole point.
func runEnv(_ context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("env", flag.ContinueOnError)
	fs.SetOutput(stderr)
	format := fs.String("format", "shell", "output format: shell, plain (the docker --env-file format), json, terraform or docker-compose")

	// The remaining arguments are the ordinary configuration flags, so `env`
	// reports the endpoints of the instance the developer actually started.
	rest, err := splitEnvArgs(fs, args)
	if err != nil {
		return errUsage
	}
	cfg, err := config.Load(config.Options{Args: rest, Output: stderr})
	if err != nil {
		return err
	}

	// The same project `up` uses, from the same function. `env` used to take a
	// --project of its own and otherwise fall back to the raw instance name, so
	// the two commands agreed only when nobody passed the flag and the name was a
	// valid project ID. --project is now a shared configuration flag, read by
	// both.
	proj := cfg.DefaultProject()

	creds, err := metadata.LoadOrCreate(cfg.InstanceDir(), proj, tokenURI(cfg))
	if err != nil {
		return err
	}
	adcPath, err := creds.WriteADC(cfg.InstanceDir())
	if err != nil {
		return err
	}

	// A running instance knows the ports it actually bound, including any
	// that were OS-assigned, which configuration alone cannot.
	if info, ok := running(cfg); ok {
		cfg = withLivePorts(cfg, info.Endpoints)
	}
	vars := envVars(cfg, proj, adcPath)
	// On stderr, so `eval "$(cloudburrow env)"` shows it instead of evaluating
	// it, and so a script reading stdout still gets only the variables.
	for _, s := range unexportableEmulators(cfg) {
		name := netfwd.EnvVarFor(string(s))
		if s == config.ServiceBigQuery {
			name = "CLOUDBURROW_BIGQUERY_ENDPOINT"
		}
		if s == config.ServiceMemorystore {
			name = "REDIS_PORT"
		}
		if s == config.ServiceCloudSQLMySQL {
			name = "MYSQL_PORT"
		}
		fmt.Fprintf(stderr, "cloudburrow env: %s is enabled with an OS-assigned port, which only "+
			"`up` knows; %s is not exported, so its clients would reach real Google. "+
			"Set --port-%s to a fixed port.\n", s, name, s)
	}
	switch *format {
	case "shell":
		writeShell(stdout, vars)
	case "plain":
		writePlain(stdout, vars)
	case "json":
		writeEnvJSON(stdout, vars)
	case "terraform":
		writeTerraformEnv(stdout, cfg)
	case "docker-compose":
		writeCompose(stdout, cfg, vars)
	default:
		fmt.Fprintf(stderr, "unknown format %q; use shell, json, plain, terraform or docker-compose\n", *format)
		return errUsage
	}
	return nil
}

// splitEnvArgs lets `env` take both its own flags and the shared
// configuration flags, in any order.
//
// The two sets are parsed by different flag sets — the shared one belongs to
// internal/config — so the arguments have to be separated first. Anything not
// recognised here is passed through, which keeps `env` accepting every flag
// `up` does without restating the list.
func splitEnvArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	// It takes a value, so a bare `--format shell` consumes the next
	// argument too. There are no boolean flags here, which is what makes
	// that unambiguous.
	own := map[string]bool{"format": true}

	var mine, rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		name, isFlag := strings.CutPrefix(a, "-")
		if !isFlag {
			rest = append(rest, a)
			continue
		}
		name = strings.TrimPrefix(name, "-")
		name, _, joined := strings.Cut(name, "=")
		if !own[name] {
			rest = append(rest, a)
			continue
		}
		mine = append(mine, a)
		if !joined && i+1 < len(args) {
			i++
			mine = append(mine, args[i])
		}
	}
	if err := fs.Parse(mine); err != nil {
		return nil, err
	}
	return rest, nil
}

func tokenURI(cfg config.Config) string {
	return fmt.Sprintf("http://%s/token",
		net.JoinHostPort(cfg.BindAddress, fmt.Sprint(cfg.Endpoints.Metadata)))
}

// envVar is one exported variable and why it is there.
type envVar struct {
	Name    string
	Value   string
	Comment string
}

func envVars(cfg config.Config, project, adcPath string) []envVar {
	host := cfg.BindAddress
	addr := func(port int) string { return net.JoinHostPort(host, fmt.Sprint(port)) }

	vars := []envVar{
		{"STORAGE_EMULATOR_HOST", "http://" + addr(cfg.Endpoints.Storage),
			"read by the official Cloud Storage clients"},
		{"PUBSUB_EMULATOR_HOST", addr(cfg.Endpoints.PubSub),
			"read by the official Pub/Sub clients"},
		{"GOOGLE_APPLICATION_CREDENTIALS", adcPath,
			"a locally generated fixture; it authorises nothing"},
		{"GOOGLE_CLOUD_PROJECT", project,
			"the project every local resource lives under"},
		{"GCE_METADATA_HOST", addr(cfg.Endpoints.Metadata),
			"points Google's metadata lookups at this instance, not 169.254.169.254"},
		{"CLOUDSDK_CORE_PROJECT", project,
			"gcloud's project"},
		{"CLOUDSDK_API_ENDPOINT_OVERRIDES_STORAGE", "http://" + addr(cfg.Endpoints.Storage) + "/storage/v1/",
			"gcloud storage"},
		{"CLOUDSDK_API_ENDPOINT_OVERRIDES_PUBSUB", "http://" + addr(cfg.Endpoints.PubSub) + "/",
			"gcloud pubsub"},
	}

	// The opt-in emulators, for the services actually enabled. Without these,
	// `eval "$(cloudburrow env)"` left a Firestore or Spanner client pointed at
	// real Google for exactly the services the developer had asked to emulate.
	// The variable names and value shapes come from netfwd's table, the one `up`
	// prints its endpoints from, so the two cannot disagree.
	for _, s := range cfg.EnabledServices() {
		name := netfwd.EnvVarFor(string(s))
		port := cfg.Endpoints.OptionalPort(s)
		// An OS-assigned port (0) is known only to the running `up`, so it is
		// left out here and reported by unexportableEmulators instead. Exporting
		// an empty value would be the worst option: most clients read an empty
		// variable as unset and fall back to real Google.
		if name == "" || !s.IsOptional() || port == 0 {
			continue
		}
		vars = append(vars, envVar{name, netfwd.EnvValueFor(string(s), addr(port)),
			"read by the official " + string(s) + " clients"})
	}

	// Cloud Tasks and Secret Manager have no emulator variable in any official
	// client either. These are for code that builds its own channel: every
	// language's client needs an explicit endpoint and a plaintext transport
	// for them (docs/credentials.md shows how, per language).
	for _, e := range []struct {
		s    config.Service
		name string
		port int
	}{
		{config.ServiceTasks, "CLOUDBURROW_TASKS_ENDPOINT", cfg.Endpoints.Tasks},
		{config.ServiceSecrets, "CLOUDBURROW_SECRETMANAGER_ENDPOINT", cfg.Endpoints.Secrets},
		{config.ServiceScheduler, "CLOUDBURROW_SCHEDULER_ENDPOINT", cfg.Endpoints.Scheduler},
	} {
		if serviceEnabled(cfg, e.s) && e.port != 0 {
			vars = append(vars, envVar{e.name, addr(e.port),
				"gRPC, plaintext; read by no client library: give it to a channel in code"})
		}
	}

	// BigQuery has no emulator variable in any official client library, so
	// what is exported is for code to read, and says so. gcloud does read
	// CLOUDSDK_API_ENDPOINT_OVERRIDES_BIGQUERY; the Go, Python and Java
	// clients do not, and need the endpoint passed in client options.
	if serviceEnabled(cfg, config.ServiceBigQuery) && cfg.Endpoints.BigQuery != 0 {
		rest := "http://" + addr(cfg.Endpoints.BigQuery)
		vars = append(vars,
			envVar{"CLOUDSDK_API_ENDPOINT_OVERRIDES_BIGQUERY", rest + "/",
				"read by gcloud only; client libraries ignore it"},
			envVar{"CLOUDBURROW_BIGQUERY_ENDPOINT", rest,
				"read by no client library: pass it to option.WithEndpoint, with this instance's project"})
		if cfg.Endpoints.BigQueryStorage != 0 {
			vars = append(vars, envVar{"CLOUDBURROW_BIGQUERY_STORAGE_ENDPOINT", addr(cfg.Endpoints.BigQueryStorage),
				"the Storage Read API (gRPC); read by no client library"})
		}
	}

	// Memorystore: the two variables Redis clients and their examples read
	// by convention. No Google library reads them — there is no Memorystore
	// data-plane client, applications use an ordinary Redis client.
	if serviceEnabled(cfg, config.ServiceMemorystore) && cfg.Endpoints.Memorystore != 0 {
		vars = append(vars,
			envVar{"REDIS_HOST", host, "Memorystore: a real Valkey server; not the Memorystore admin API"},
			envVar{"REDIS_PORT", fmt.Sprint(cfg.Endpoints.Memorystore), "Memorystore RESP port"})
	}

	// Cloud SQL for MySQL: the variables MySQL clients and their examples read
	// by convention, with the instance's generated password. No Google
	// library reads them; an application uses an ordinary MySQL driver.
	if serviceEnabled(cfg, config.ServiceCloudSQLMySQL) && cfg.Endpoints.CloudSQLMySQL != 0 {
		if creds, err := loadOrCreateMySQLCredentials(cfg); err == nil {
			vars = append(vars,
				envVar{"MYSQL_HOST", host, "Cloud SQL for MySQL: a local MySQL; not the Cloud SQL Admin API"},
				envVar{"MYSQL_PORT", fmt.Sprint(cfg.Endpoints.CloudSQLMySQL), "Cloud SQL for MySQL port"},
				envVar{"MYSQL_USER", components.CloudSQLUser, "Cloud SQL for MySQL user"},
				envVar{"MYSQL_PASSWORD", creds.Password, "generated for this instance; local only"},
				envVar{"MYSQL_DATABASE", components.CloudSQLDatabase, "Cloud SQL for MySQL default database"})
		}
	}

	// Resource Manager v3 (#298). gcloud reads the override; the client
	// libraries do not, and need the endpoint in client options. gcloud's
	// projects commands call v1, which is not served, so the override helps
	// only v3 callers.
	if cfg.Endpoints.ResourceManager != 0 {
		rm := addr(cfg.Endpoints.ResourceManager)
		vars = append(vars,
			envVar{"CLOUDSDK_API_ENDPOINT_OVERRIDES_CLOUDRESOURCEMANAGER", "http://" + rm + "/",
				"read by gcloud; only the v3 Projects API is served here"},
			envVar{"CLOUDBURROW_RESOURCEMANAGER_ENDPOINT", rm,
				"gRPC and REST, plaintext; read by no client library: pass it to option.WithEndpoint"})
	}

	// Ingress is reported only when it is actually published, or a developer
	// would export a URL nothing serves.
	if cfg.Endpoints.Ingress != 0 {
		vars = append(vars, envVar{
			"CLOUDBURROW_INGRESS", "http://" + addr(cfg.Endpoints.Ingress),
			"Cloud Run services are served here; see docs/networking.md",
		})
		vars = append(vars, envVar{
			"CLOUDBURROW_DOMAIN", components.DefaultDomain,
			"services are named <service>.<namespace>." + components.DefaultDomain,
		})
	}

	sort.SliceStable(vars, func(i, j int) bool { return vars[i].Name < vars[j].Name })
	return vars
}

// unexportableEmulators names the enabled emulators whose port `env` cannot
// know, because it is OS-assigned and so known only to the running `up`.
func unexportableEmulators(cfg config.Config) []config.Service {
	var out []config.Service
	for _, s := range cfg.EnabledServices() {
		exported := netfwd.EnvVarFor(string(s)) != "" || s == config.ServiceBigQuery ||
			s == config.ServiceMemorystore || s == config.ServiceCloudSQLMySQL
		if s.IsOptional() && exported && cfg.Endpoints.OptionalPort(s) == 0 {
			out = append(out, s)
		}
	}
	return out
}

func writeShell(w io.Writer, vars []envVar) {
	fmt.Fprintln(w, "# eval \"$(cloudburrow env)\" to apply these to the current shell.")
	fmt.Fprintln(w, "#")
	fmt.Fprintln(w, "# None of this authenticates anything: CloudBurrow serves every caller,")
	fmt.Fprintln(w, "# and the credentials file exists only so tools that insist on having one")
	fmt.Fprintln(w, "# can run offline. See docs/credentials.md.")
	for _, v := range vars {
		fmt.Fprintf(w, "export %s=%q  # %s\n", v.Name, v.Value, v.Comment)
	}
}

func writePlain(w io.Writer, vars []envVar) {
	for _, v := range vars {
		fmt.Fprintf(w, "%s=%s\n", v.Name, v.Value)
	}
}

func writeEnvJSON(w io.Writer, vars []envVar) {
	fmt.Fprintln(w, "{")
	for i, v := range vars {
		comma := ","
		if i == len(vars)-1 {
			comma = ""
		}
		fmt.Fprintf(w, "  %q: %q%s\n", v.Name, v.Value, comma)
	}
	fmt.Fprintln(w, "}")
}

// printCredentials reports the local identity, and what it is not.
func printCredentials(w io.Writer, srv *metadata.Server, adcPath string) {
	fmt.Fprintf(w, "  metadata:   http://%s  (Metadata-Flavor: Google required)\n", srv.Addr())
	fmt.Fprintf(w, "  credentials: %s\n", adcPath)
	fmt.Fprintln(w, "              generated locally and authorises nothing; CloudBurrow")
	fmt.Fprintln(w, "              authenticates no request. `cloudburrow env` exports these.")
}

func serviceEnabled(cfg config.Config, s config.Service) bool {
	for _, e := range cfg.EnabledServices() {
		if e == s {
			return true
		}
	}
	return false
}

// withLivePorts replaces configured ports with the ones a running `up`
// recorded. A service the instance did not report keeps its configured port.
func withLivePorts(cfg config.Config, live map[string]string) config.Config {
	e := &cfg.Endpoints
	for name, field := range map[string]*int{
		"storage": &e.Storage, "pubsub": &e.PubSub, "tasks": &e.Tasks, "run": &e.Run,
		"secretmanager": &e.Secrets, "scheduler": &e.Scheduler, "metadata": &e.Metadata, "control": &e.Control,
		"firestore": &e.Firestore, "datastore": &e.Datastore, "bigtable": &e.Bigtable,
		"spanner": &e.Spanner, "bigquery": &e.BigQuery, "bigquery-storage": &e.BigQueryStorage,
		"memorystore": &e.Memorystore, "cloudsql-mysql": &e.CloudSQLMySQL, "resourcemanager": &e.ResourceManager,
	} {
		addr, ok := live[name]
		if !ok {
			continue
		}
		_, port, err := net.SplitHostPort(addr)
		if err != nil {
			continue
		}
		if p, err := strconv.Atoi(port); err == nil && p > 0 {
			*field = p
		}
	}
	return cfg
}

// writeTerraformEnv prints the google provider's own environment variables,
// for the services whose Terraform support is Verified. It is rendered from
// the same table as `cloudburrow terraform`, so the two cannot disagree about
// which endpoints are safe to set (#284).
func writeTerraformEnv(w io.Writer, cfg config.Config) {
	set, skipped := terraformSettings(cfg)
	fmt.Fprintln(w, "# eval \"$(cloudburrow env --format terraform)\" before running terraform.")
	fmt.Fprintln(w, "# The token authorises nothing: CloudBurrow checks none.")
	fmt.Fprintf(w, "export GOOGLE_PROJECT=%q\n", cfg.DefaultProject())
	fmt.Fprintf(w, "export GOOGLE_OAUTH_ACCESS_TOKEN=%q\n", terraformAccessToken)
	for _, e := range set {
		fmt.Fprintf(w, "export %s=%q\n", e.env, e.url)
	}
	for _, s := range skipped {
		fmt.Fprintf(w, "# %s: no Verified Terraform support, so no endpoint is set; its resources would reach real Google\n", s)
	}
}

// composeHost is how a container reaches the host it runs on.
const composeHost = "host.docker.internal"

// writeCompose prints an `environment:` map for a docker-compose service.
//
// Loopback addresses are rewritten to host.docker.internal, since a
// container's loopback is the container. Whether the host's bound address
// is then reachable depends on the Docker engine; docs/configuration.md
// says which. The credentials fixture is a host path, which does not exist
// in the container, so it is left out rather than exported broken.
func writeCompose(w io.Writer, cfg config.Config, vars []envVar) {
	fmt.Fprintln(w, "# Paste under a service in docker-compose.yml. Generated by `cloudburrow env --format docker-compose`.")
	fmt.Fprintln(w, "# Loopback addresses are rewritten to host.docker.internal; see docs/configuration.md for when")
	fmt.Fprintln(w, "# a container can reach them. GOOGLE_APPLICATION_CREDENTIALS is omitted: it names a host path.")
	fmt.Fprintln(w, "environment:")
	for _, v := range vars {
		if v.Name == "GOOGLE_APPLICATION_CREDENTIALS" {
			continue
		}
		fmt.Fprintf(w, "  %s: %s\n", v.Name, strconv.Quote(toContainerHost(v.Value)))
	}
}

// toContainerHost rewrites a loopback host, bare or in a URL, to composeHost.
func toContainerHost(v string) string {
	for _, lo := range []string{"127.0.0.1", "localhost", "[::1]"} {
		v = strings.ReplaceAll(v, "//"+lo+":", "//"+composeHost+":")
		if strings.HasPrefix(v, lo+":") {
			v = composeHost + v[len(lo):]
		}
	}
	return v
}
