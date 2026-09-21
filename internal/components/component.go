package components

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/identity-wael/cloudburrow/internal/config"
)

// LifecycleComponent installs backends and, when Cloud Run is enabled, Knative.
// It adapts to lifecycle.Component so the coordinator owns its ordering.
type LifecycleComponent struct {
	installer  *Installer
	services   []config.Service
	mode       config.Mode
	timeout    time.Duration
	out        io.Writer
	storageURL string
}

// SetStorageExternalURL records the address storage clients will use, so the
// backend advertises a download URL they can actually reach.
func (c *LifecycleComponent) SetStorageExternalURL(url string) { c.storageURL = url }

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
		case config.ServiceStorage:
			out = append(out,
				StorageBackend(c.installer.Namespace, persistent, c.storageURL),
				// A second endpoint for in-cluster clients; see
				// StorageInternalBackend for why one cannot serve both.
				StorageInternalBackend(c.installer.Namespace, persistent))
		}
	}
	return out
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
