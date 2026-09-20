package cluster

import (
	"context"
	"fmt"
	"io"
	"time"
)

// Component adapts a Cluster to the lifecycle.Component interface, so the
// coordinator owns cluster startup ordering and readiness like any other
// component.
//
// Start is deliberately allowed to take minutes: creating a cluster pulls a
// node image and waits for the API server. The bound comes from ReadyTimeout.
type Component struct {
	cluster      *Cluster
	configPath   string
	readyTimeout time.Duration
	out          io.Writer

	// serverVersion is recorded on a successful start, for reporting.
	serverVersion string
}

// NewComponent returns a lifecycle component for the given cluster.
func NewComponent(c *Cluster, configPath string, readyTimeout time.Duration, out io.Writer) *Component {
	return &Component{cluster: c, configPath: configPath, readyTimeout: readyTimeout, out: out}
}

func (c *Component) Name() string { return "cluster" }

// ServerVersion returns the Kubernetes version observed at start, if any.
func (c *Component) ServerVersion() string { return c.serverVersion }

// Cluster exposes the underlying cluster for callers that need it.
func (c *Component) Cluster() *Cluster { return c.cluster }

// Start creates or starts the cluster and waits for the Kubernetes API.
//
// Readiness is confirmed by asking the API server, not by assuming that a
// successful create means a usable cluster.
func (c *Component) Start(ctx context.Context) error {
	if c.out != nil {
		fmt.Fprintf(c.out, "cluster %s: ensuring it exists...\n", c.cluster.Name())
	}
	if err := c.cluster.Create(ctx, c.configPath); err != nil {
		return err
	}

	if c.out != nil {
		fmt.Fprintf(c.out, "cluster %s: waiting for the Kubernetes API...\n", c.cluster.Name())
	}
	if err := c.cluster.WaitReady(ctx, c.readyTimeout); err != nil {
		return err
	}

	version, err := c.cluster.ServerVersion(ctx)
	if err != nil {
		// The API answered readyz, so a version read failure is worth
		// reporting but is not fatal to startup.
		if c.out != nil {
			fmt.Fprintf(c.out, "cluster %s: could not read server version: %v\n", c.cluster.Name(), err)
		}
	}
	c.serverVersion = version
	return nil
}

// Stop is a no-op.
//
// Shutting down the CLI must not stop the cluster: `stop` and `delete` are
// explicit commands, and a Ctrl-C that silently tore down the developer's
// environment would be a serious surprise.
func (c *Component) Stop(context.Context) error { return nil }
