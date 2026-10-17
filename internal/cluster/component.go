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

	// nodes is the wait for a created cluster's nodes to be Ready, running
	// in the background from the create; nil when Start found the cluster
	// rather than creating it (#1588).
	nodes *nodesWait
}

// nodesWait is WaitNodesReady running in the background.
type nodesWait struct {
	cancel context.CancelFunc
	done   chan struct{}
	err    error
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
	created, err := c.cluster.Ensure(ctx, c.configPath)
	if err != nil {
		return err
	}
	if created {
		// `kind create` no longer waits for the node (#1588): it becomes
		// Ready while what needs only the API runs, and NodesReady,
		// registered before anything is scheduled, waits for it.
		wctx, cancel := context.WithCancel(ctx)
		w := &nodesWait{cancel: cancel, done: make(chan struct{})}
		c.nodes = w
		go func() {
			defer close(w.done)
			w.err = c.cluster.WaitNodesReady(wctx, NodesReadyTimeout)
		}()
	}

	if c.out != nil {
		fmt.Fprintf(c.out, "cluster %s: waiting for the Kubernetes API...\n", c.cluster.Name())
	}
	if err := c.cluster.WaitReady(ctx, c.readyTimeout); err != nil {
		return err
	}

	// A cluster that already existed may predate this CLI's pins: its node
	// image cannot change in place, so a mismatch stops here (#601).
	_, note, err := c.cluster.Reconcile(ctx, created)
	if err != nil {
		return err
	}
	if note != "" && c.out != nil {
		fmt.Fprintf(c.out, "cluster %s: %s\n", c.cluster.Name(), note)
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

// Stop leaves the cluster running.
//
// Shutting down the CLI must not stop the cluster: `stop` and `delete` are
// explicit commands, and a Ctrl-C that silently tore down the developer's
// environment would be a serious surprise.
//
// It does end a node wait still running, as when startup unwinds before
// NodesReady.
func (c *Component) Stop(context.Context) error {
	if w := c.nodes; w != nil {
		w.cancel()
		<-w.done
	}
	return nil
}

// NodesReady is the component that waits for the nodes of a cluster Start
// created to be Ready, what `kind create --wait` waited for (#1588).
// Registered after the work that needs only the API (the images, Knative's
// manifests) and before the backends, so that work overlaps the node's
// start and nothing is scheduled before it is Ready. Against a cluster
// Start found, it returns at once, as `up` always has.
func (c *Component) NodesReady() *NodesReadyPhase { return &NodesReadyPhase{c: c} }

// NodesReadyPhase is the lifecycle component NodesReady returns.
type NodesReadyPhase struct{ c *Component }

func (p *NodesReadyPhase) Name() string { return "cluster-nodes" }

func (p *NodesReadyPhase) Start(ctx context.Context) error {
	w := p.c.nodes
	if w == nil {
		return nil
	}
	select {
	case <-w.done:
		return w.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Stop cancels a wait still running when startup unwinds.
func (p *NodesReadyPhase) Stop(context.Context) error {
	if w := p.c.nodes; w != nil {
		w.cancel()
		<-w.done
	}
	return nil
}
