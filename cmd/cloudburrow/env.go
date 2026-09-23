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
	project := fs.String("project", "", "project ID the credentials name (default: the instance name)")

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

	proj := *project
	if proj == "" {
		proj = cfg.Name
	}

	creds, err := metadata.LoadOrCreate(cfg.InstanceDir(), proj, tokenURI(cfg))
	if err != nil {
		return err
	}
	adcPath, err := creds.WriteADC(cfg.InstanceDir())
	if err != nil {
		return err
	}

	vars := envVars(cfg, proj, adcPath)
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
	// Both take a value, so a bare `--format shell` consumes the next
	// argument too. There are no boolean flags here, which is what makes
	// that unambiguous.
	own := map[string]bool{"format": true, "project": true}

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
