package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/identity-wael/cloudburrow/internal/cluster"
	"github.com/identity-wael/cloudburrow/internal/config"
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
// It deletes only the managed namespace, never the cluster and never a
// namespace CloudBurrow did not create.
func runReset(args []string, stdout, stderr io.Writer) error {
	cfg, err := config.Load(config.Options{Args: args, Output: stderr})
	if err != nil {
		return err
	}
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
	fmt.Fprintln(stdout, "the cluster itself is untouched; run `cloudburrow up` to recreate components")
	return nil
}
