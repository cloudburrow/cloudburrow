package components

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/config"
)

// LifecycleComponent installs backends and, when Cloud Run is enabled, Knative.
// It adapts to lifecycle.Component so the coordinator owns its ordering.
type LifecycleComponent struct {
	installer *Installer
	services  []config.Service
	mode      config.Mode
	timeout   time.Duration
	out       io.Writer
	// project is the instance's default project. Only BigQuery needs it; see
	// Backends.
	project string
	// mysql is the instance's generated MySQL credentials, for Cloud SQL for
	// MySQL.
	mysql MySQLCredentials
	// storageImage is the builtin storage server's locally built image
	// (#514), the only Cloud Storage backend (#519).
	storageImage string
}

// SetBuiltinStorageImage records the locally built image of the builtin
// Cloud Storage server, which `up` builds and loads before installing.
func (c *LifecycleComponent) SetBuiltinStorageImage(ref string) { c.storageImage = ref }

// SetMySQLCredentials supplies the passwords the MySQL backend is started
// with.
func (c *LifecycleComponent) SetMySQLCredentials(m MySQLCredentials) { c.mysql = m }

// NewLifecycleComponent builds the installer component for a configuration.
func NewLifecycleComponent(kubeconfig string, cfg config.Config, out io.Writer) *LifecycleComponent {
	return &LifecycleComponent{
		installer: &Installer{
			Kubeconfig: kubeconfig,
			Namespace:  cfg.Cluster.Namespace,
			Instance:   cfg.Name,
			Runner:     ExecRunner{},
			Out:        out,
		},
		services: cfg.EnabledServices(),
		project:  cfg.DefaultProject(),
		mode:     cfg.Mode,
		timeout:  time.Duration(cfg.ReadyTimeout),
		out:      out,
	}
}

func (c *LifecycleComponent) Name() string { return "components" }

// Backends returns the backend definitions for the enabled services.
//
// Cloud Tasks has no backend here: it has no upstream and is implemented by
// CloudBurrow (#15/#16), so nothing is deployed for it yet.
func (c *LifecycleComponent) Backends() []Backend {
	persistent := c.mode == config.ModePersistent
	var out []Backend
	for _, s := range c.services {
		switch s {
		case config.ServicePubSub:
			out = append(out, PubSubBackend("cloudburrow"))
		default:
			// The Google emulators serve any project, so the name they are
			// started with is only a default. BigQuery's serves the one it
			// is started with and no other (#277), so it must be the
			// instance's, the project every client is told to use.
			project := "cloudburrow"
			if s == config.ServiceBigQuery {
				project = c.project
			}
			if s == config.ServiceCloudSQLMySQL {
				out = append(out, CloudSQLMySQLBackend(persistent, c.mysql))
				continue
			}
			if b, ok := OptionalBackend(s, project, persistent); ok {
				out = append(out, b)
			}
		case config.ServiceStorage:
			// One Deployment serves the host and the cluster (#514).
			out = append(out, BuiltinStorageBackend(c.installer.Namespace, c.storageImage, persistent, c.enabled(config.ServicePubSub)))
		}
	}
	return out
}

func (c *LifecycleComponent) enabled(s config.Service) bool {
	for _, e := range c.services {
		if e == s {
			return true
		}
	}
	return false
}

// NeedsKnative reports whether Cloud Run is enabled.
func (c *LifecycleComponent) NeedsKnative() bool {
	for _, s := range c.services {
		if s == config.ServiceRun {
			return true
		}
	}
	return false
}

// Start installs everything the enabled services require.
func (c *LifecycleComponent) Start(ctx context.Context) error {
	// Before anything else, so an instance with no backend pod has the
	// namespace too (#571).
	if err := c.installer.EnsureNamespace(ctx); err != nil {
		return err
	}
	if backends := c.Backends(); len(backends) > 0 {
		if err := c.installer.InstallBackends(ctx, backends, c.timeout); err != nil {
			return err
		}
	}
	if c.NeedsKnative() {
		if c.installer.KnativeInstalled(ctx) {
			fmt.Fprintf(c.out, "  knative already installed\n")
			return nil
		}
		fmt.Fprintf(c.out, "  installing Knative Serving %s (this takes a minute)...\n", KnativeVersion)
		if err := c.installer.InstallKnative(ctx, c.timeout); err != nil {
			return err
		}
	}
	return nil
}

// Installer exposes the underlying installer.
func (c *LifecycleComponent) Installer() *Installer { return c.installer }

// HasStorage reports whether a storage backend was deployed.
func (c *LifecycleComponent) HasStorage() bool {
	for _, b := range c.Backends() {
		if b.Name == "storage" {
			return true
		}
	}
	return false
}

// ReadyTimeout is the bound used for component readiness.
func (c *LifecycleComponent) ReadyTimeout() time.Duration { return c.timeout }

// Stop is a no-op: components live in the cluster and outlive the CLI process.
// Removing them belongs to `reset`, which is explicit.
func (c *LifecycleComponent) Stop(context.Context) error { return nil }
