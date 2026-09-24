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

// gcloudVerified are the services whose gcloud use docs/compatibility.md
// marks Verified; no other endpoint is written, so gcloud never believes a
// service is local that has not been shown to work.
var gcloudVerified = []struct {
	service  config.Service
	property string
	path     string
}{
	{config.ServiceStorage, "storage", "/storage/v1/"},
	{config.ServicePubSub, "pubsub", "/"},
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
	ports := map[config.Service]int{config.ServiceStorage: cfg.Endpoints.Storage, config.ServicePubSub: cfg.Endpoints.PubSub}
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n# Instance %q.\n", gcloudMarker, cfg.Name)
	fmt.Fprintf(&b, "[core]\nproject = %s\ndisable_usage_reporting = true\n", cfg.DefaultProject())
	b.WriteString("\n[api_endpoint_overrides]\n")
	for _, v := range gcloudVerified {
		if serviceEnabled(cfg, v.service) && ports[v.service] != 0 {
			fmt.Fprintf(&b, "%s = http://%s%s\n", v.property, host(ports[v.service]), v.path)
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
