package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	runadapter "github.com/cloudburrow/cloudburrow/internal/adapter/run"
	"github.com/cloudburrow/cloudburrow/internal/cluster"
	"github.com/cloudburrow/cloudburrow/internal/config"
)

// newCluster builds the cluster for a configuration. Ownership rules live in
// internal/cluster, so an invalid or unowned target cannot be constructed here.
func newCluster(cfg config.Config) (*cluster.Cluster, error) {
	return cluster.New(cluster.Options{
		Name:       cfg.ClusterName(),
		NodeImage:  cfg.Cluster.NodeImage,
		Kubeconfig: cfg.KubeconfigPath(),
	})
}

// describeClusterError turns an infrastructure failure into something a
// developer can act on, rather than a raw command failure.
func describeClusterError(err error) error {
	switch {
	case errors.Is(err, cluster.ErrDockerUnavailable):
		return fmt.Errorf("%w\n\nCloudBurrow runs a local Kubernetes cluster, which needs a container runtime.", err)
	case errors.Is(err, cluster.ErrAbsent):
		return fmt.Errorf("%w\n\nRun `cloudburrow up` to create it.", err)
	case errors.Is(err, cluster.ErrNotOwned):
		return fmt.Errorf("%w\n\nCloudBurrow only manages clusters it created.", err)
	default:
		return err
	}
}

// runStop stops the cluster without destroying it. State held in volumes
// survives, which is what distinguishes stop from delete.
func runStop(args []string, stdout, stderr io.Writer) error {
	cfg, err := config.Load(config.Options{Args: args, Output: stderr})
	if err != nil {
		return err
	}
	// The `up` serving this instance goes first. Stopping the cluster under
	// it left a process holding ports and tunnels to nothing (#278).
	if err := stopRunning(cfg, stdout); err != nil {
		return err
	}
	c, err := newCluster(cfg)
	if err != nil {
		return describeClusterError(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.ShutdownTimeout))
	defer cancel()

	if err := c.Stop(ctx); err != nil {
		return describeClusterError(err)
	}
	fmt.Fprintf(stdout, "stopped cluster %s\n", c.Name())
	fmt.Fprintln(stdout, "state held in volumes is preserved; run `cloudburrow up` to start again")
	return nil
}

// runDelete destroys the cluster CloudBurrow created.
func runDelete(args []string, stdout, stderr io.Writer) error {
	cfg, err := config.Load(config.Options{Args: args, Output: stderr})
	if err != nil {
		return err
	}
	// As for stop: the `up` serving this instance goes first, or deleting its
	// cluster leaves a detached process serving tunnels to nothing (#282).
	if err := stopRunning(cfg, stdout); err != nil {
		return err
	}
	c, err := newCluster(cfg)
	if err != nil {
		return describeClusterError(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	if err := c.Delete(ctx); err != nil {
		return describeClusterError(err)
	}
	fmt.Fprintf(stdout, "deleted cluster %s and its state\n", c.Name())
	return nil
}

// runReset destroys CloudBurrow-managed state while keeping the cluster.
//
// With an `up` running, the reset goes through its admin API (#586): the
// services clear their own state and every pod, port-forward and tunnel
// stays up. Deleting the namespace beneath a live `up` left it serving
// forwards to nothing, and a foreground `up` then refused to start while the
// old one lived.
//
// With no `up` running, it deletes only the managed namespace, never the
// cluster and never a namespace CloudBurrow did not create.
func runReset(args []string, stdout, stderr io.Writer) error {
	opts, rest, err := resetFlags(args)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return errUsage
	}
	cfg, err := config.Load(config.Options{Args: rest, Output: stderr})
	if err != nil {
		return err
	}
	if info, ok := running(cfg); ok {
		return resetViaAdmin(cfg, info, opts, stdout)
	}
	// Scoping is the admin API's. Without it, a reset asked for one service
	// or one project must not become a wipe of everything.
	if opts.scoped() {
		return fmt.Errorf("instance %q is not running: -service, -project and -reseed need a running `cloudburrow up`; "+
			"without them, reset deletes the whole managed namespace", cfg.Name)
	}
	return resetNamespace(cfg, stdout)
}

// resetNamespace is the reset with no `up` running. A variable so a test can
// show a running `up` never reaches it.
var resetNamespace = func(cfg config.Config, stdout io.Writer) error {
	c, err := newCluster(cfg)
	if err != nil {
		return describeClusterError(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.ReadyTimeout))
	defer cancel()

	status, err := c.Status(ctx)
	if err != nil {
		return describeClusterError(err)
	}
	if status != cluster.StatusRunning {
		return fmt.Errorf("cluster %s is %s; reset needs a running cluster", c.Name(), status)
	}

	if err := c.DeleteNamespace(ctx, cfg.Cluster.Namespace); err != nil {
		return describeClusterError(err)
	}
	fmt.Fprintf(stdout, "reset: deleted namespace %s in cluster %s\n", cfg.Cluster.Namespace, c.Name())

	// Secret Manager's objects live in the workload namespace, because a
	// secretKeyRef cannot cross namespaces. Deleting the managed namespace
	// would leave them behind, so they are removed by ownership label —
	// never by name, and never touching an object CloudBurrow did not create.
	removed, err := c.DeleteOwnedSecrets(ctx, runadapter.WorkloadNamespace)
	if err != nil {
		return describeClusterError(err)
	}
	if removed > 0 {
		fmt.Fprintf(stdout, "reset: deleted %d Secret Manager secret(s) in namespace %s\n",
			removed, runadapter.WorkloadNamespace)
	}
	fmt.Fprintln(stdout, "the cluster itself is untouched; run `cloudburrow up` to recreate components")
	return nil
}
