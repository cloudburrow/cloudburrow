package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/metadata"
)

// `cloudburrow gcloud-setup` and `gcloud-teardown` (#305): a gcloud
// configuration of this instance's own, so gcloud reaches CloudBurrow in any
// shell that selects it, rather than only in one that evaluated `env`.
//
// The configuration is written as the file gcloud keeps it in —
// <config dir>/configurations/config_cloudburrow-<name> — rather than through
// `gcloud config`, so setup needs no gcloud and cannot touch anything else:
// not active_config, not the default configuration, not any other. It is
// selected per shell with CLOUDSDK_ACTIVE_CONFIG_NAME, which setup prints and
// teardown unsets. The file carries a marker, and teardown removes only a
// file that has it.

const gcloudMarker = "# Written by `cloudburrow gcloud-setup`; `cloudburrow gcloud-teardown` removes it."

// resourceManagerService keys Resource Manager, which is always served and
// so is not one of the services --services selects.
const resourceManagerService config.Service = "resourcemanager"

// gcloudVerified are the services whose gcloud use docs/compatibility.md
// marks Verified; no other endpoint is written, so gcloud never believes a
// service is local that has not been shown to work.
var gcloudVerified = []struct {
	service  config.Service
	property string
	path     string
	// always is set for an endpoint served whatever --services selects.
	always bool
	// regional is set for an API some of whose gcloud commands call
	// {location}-{host}, deriving the host from this override. On a
	// loopback bind the host is then {property}.localhost, so the prefixed
	// name still resolves to loopback (RFC 6761), where {location}-127.0.0.1
	// resolves to nothing.
	regional bool
}{
	{config.ServiceStorage, "storage", "/storage/v1/", false, false},
	{config.ServicePubSub, "pubsub", "/", false, false},
	// Cloud KMS (#426): gcloud's apitools client appends v1/ itself, and
	// TestGcloudKMS drives keyrings, keys and versions through it.
	{config.ServiceKMS, "cloudkms", "/", false, false},
	// Secret Manager and Cloud Tasks (#590): JSON on the gRPC port, v1/ and
	// v2/ appended by gcloud. TestGcloudSecrets drives secrets and versions;
	// TestGcloudTasks drives queues and, since tasks are transcoded (#591),
	// tasks.
	{config.ServiceSecrets, "secretmanager", "/", false, false},
	{config.ServiceTasks, "cloudtasks", "/", false, false},
	// Cloud Scheduler and Cloud Logging (#591): JSON on the gRPC port
	// through the shared transcoder, v1/ and v2/ appended by gcloud.
	// TestGcloudScheduler drives jobs (with --location: gcloud otherwise
	// looks for an App Engine app); TestGcloudLogging writes, reads, lists
	// and deletes logs.
	{config.ServiceScheduler, "cloudscheduler", "/", false, false},
	{config.ServiceLogging, "logging", "/", false, false},
	// Resource Manager (#682): gcloud's projects commands call the v1 REST
	// API, served since #301 on the Resource Manager port beside v3.
	// TestGcloudProjectsThroughGcloudSetup drives list, create, describe and
	// delete through this override alone. Tag bindings and effective tags
	// given --location call {location}-cloudresourcemanager (#1350), which
	// TestGcloudRegionalTagBindings drives.
	{resourceManagerService, "cloudresourcemanager", "/", true, true},
	// Cloud Run (#1293): gcloud run calls the v1 (Knative) API, served on
	// the Run port beside v2. A command given --region calls
	// {region}-{host}, which gcloud derives from this override, so on
	// loopback the host is run.localhost. TestGcloudRunServicesList drives
	// services list, describe and revisions list.
	{config.ServiceRun, "run", "/", false, true},
	// The Cloud SQL Admin API (#1301): gcloud sql appends sql/v1beta4/.
	// TestGcloudSQL drives instances list and describe, and databases and
	// users create, list and delete, with either Cloud SQL service.
	{sqlAdminEndpoint, "sql", "/", false, false},
}

// gcloudFamilies is the gcloud-setup help's list of what it writes, from
// gcloudVerified, so the help names every override (#708).
func gcloudFamilies() string {
	names := make([]string, len(gcloudVerified))
	for i, v := range gcloudVerified {
		names[i] = v.property
	}
	return strings.Join(names, ", ")
}

func gcloudConfigName(cfg config.Config) string { return "cloudburrow-" + cfg.Name }

// gcloudConfigDir is where gcloud keeps its configurations.
func gcloudConfigDir() (string, error) {
	if d := os.Getenv("CLOUDSDK_CONFIG"); d != "" {
		return d, nil
	}
	if runtime.GOOS == "windows" {
		if d := os.Getenv("APPDATA"); d != "" {
			return filepath.Join(d, "gcloud"), nil
		}
		return "", errors.New("APPDATA is not set; set CLOUDSDK_CONFIG to gcloud's configuration directory")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "gcloud"), nil
}

func gcloudConfigPath(cfg config.Config) (string, error) {
	dir, err := gcloudConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "configurations", "config_"+gcloudConfigName(cfg)), nil
}

// gcloudConfiguration renders the configuration file.
func gcloudConfiguration(cfg config.Config, adcPath string) string {
	host := func(port int) string { return net.JoinHostPort(cfg.BindAddress, strconv.Itoa(port)) }
	ports := map[config.Service]int{config.ServiceStorage: cfg.Endpoints.Storage, config.ServicePubSub: cfg.Endpoints.PubSub,
		config.ServiceKMS: cfg.Endpoints.KMS, config.ServiceSecrets: cfg.Endpoints.Secrets, config.ServiceTasks: cfg.Endpoints.Tasks,
		config.ServiceScheduler: cfg.Endpoints.Scheduler, config.ServiceLogging: cfg.Endpoints.Logging,
		resourceManagerService: cfg.Endpoints.ResourceManager, config.ServiceRun: cfg.Endpoints.Run,
		sqlAdminEndpoint: cfg.Endpoints.SQLAdmin}
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n# Instance %q.\n", gcloudMarker, cfg.Name)
	// should_prompt_to_enable_api is off because gcloud would otherwise ask
	// Service Usage, which is not served, whether an API is enabled before
	// commands such as `run deploy` (#1297); every served API is enabled.
	fmt.Fprintf(&b, "[core]\nproject = %s\ndisable_usage_reporting = true\nshould_prompt_to_enable_api = false\n", cfg.DefaultProject())
	b.WriteString("\n[api_endpoint_overrides]\n")
	for _, v := range gcloudVerified {
		on := v.always || serviceEnabled(cfg, v.service) || (v.service == sqlAdminEndpoint && sqlAdminEnabled(cfg))
		if on && ports[v.service] != 0 {
			addr := host(ports[v.service])
			if ip := net.ParseIP(cfg.BindAddress); v.regional && ip != nil && ip.IsLoopback() {
				addr = net.JoinHostPort(v.property+".localhost", strconv.Itoa(ports[v.service]))
			}
			fmt.Fprintf(&b, "%s = http://%s%s\n", v.property, addr, v.path)
		}
	}
	// No credential is sent: CloudBurrow authenticates nothing. The ADC
	// fixture is named for tools that insist on one being configured.
	fmt.Fprintf(&b, "\n[auth]\ndisable_credentials = true\ncredential_file_override = %s\n", adcPath)
	b.WriteString("\n[component_manager]\ndisable_update_check = true\n")
	return b.String()
}

func runGcloudSetup(args []string, stdout, stderr io.Writer) error {
	cfg, err := config.Load(config.Options{Args: args, Output: stderr})
	if err != nil {
		return err
	}
	if info, ok := running(cfg); ok {
		cfg = withLivePorts(cfg, info.Endpoints)
	}
	creds, err := metadata.LoadOrCreate(cfg.InstanceDir(), cfg.DefaultProject(), tokenURI(cfg))
	if err != nil {
		return err
	}
	adcPath, err := creds.WriteADC(cfg.InstanceDir())
	if err != nil {
		return err
	}
	path, err := gcloudConfigPath(cfg)
	if err != nil {
		return err
	}
	if existing, err := os.ReadFile(path); err == nil && !strings.HasPrefix(string(existing), gcloudMarker) {
		return fmt.Errorf("%s exists and was not written by cloudburrow; it is left alone", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(gcloudConfiguration(cfg, adcPath)), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(stderr, "cloudburrow: wrote gcloud configuration %s (%s); your default configuration is unchanged\n",
		gcloudConfigName(cfg), path)
	fmt.Fprintf(stdout, "export CLOUDSDK_ACTIVE_CONFIG_NAME=%s\n", gcloudConfigName(cfg))
	return nil
}

func runGcloudTeardown(args []string, stdout, stderr io.Writer) error {
	cfg, err := config.Load(config.Options{Args: args, Output: stderr})
	if err != nil {
		return err
	}
	path, err := gcloudConfigPath(cfg)
	if err != nil {
		return err
	}
	existing, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		fmt.Fprintf(stderr, "cloudburrow: gcloud configuration %s does not exist; nothing to remove\n", gcloudConfigName(cfg))
	case err != nil:
		return err
	case !strings.HasPrefix(string(existing), gcloudMarker):
		return fmt.Errorf("%s was not written by cloudburrow; it is left alone", path)
	default:
		if err := os.Remove(path); err != nil {
			return err
		}
		fmt.Fprintf(stderr, "cloudburrow: removed gcloud configuration %s\n", gcloudConfigName(cfg))
	}
	fmt.Fprintln(stdout, "unset CLOUDSDK_ACTIVE_CONFIG_NAME")
	return nil
}
