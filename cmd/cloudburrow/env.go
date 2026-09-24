package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"sort"
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
	format := fs.String("format", "shell", "output format: shell, json or plain")

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

	vars := envVars(cfg, proj, adcPath)
	// On stderr, so `eval "$(cloudburrow env)"` shows it instead of evaluating
	// it, and so a script reading stdout still gets only the variables.
	for _, s := range unexportableEmulators(cfg) {
		fmt.Fprintf(stderr, "cloudburrow env: %s is enabled with an OS-assigned port, which only "+
			"`up` knows; %s is not exported, so its clients would reach real Google. "+
			"Set --port-%s to a fixed port.\n", s, netfwd.EnvVarFor(string(s)), s)
	}
	switch *format {
	case "shell":
		writeShell(stdout, vars)
	case "plain":
		writePlain(stdout, vars)
	case "json":
		writeEnvJSON(stdout, vars)
	default:
		fmt.Fprintf(stderr, "unknown format %q; use shell, json or plain\n", *format)
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
		if s.IsOptional() && netfwd.EnvVarFor(string(s)) != "" && cfg.Endpoints.OptionalPort(s) == 0 {
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
