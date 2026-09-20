package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/identity-wael/cloudburrow/internal/config"
	"github.com/identity-wael/cloudburrow/internal/lifecycle"
)

// errNotImplemented marks a command whose interface is defined here but whose
// cluster operations land in a later issue. It is a distinct error so that
// tests can assert the honest failure rather than matching on message text.
var errNotImplemented = errors.New("not implemented")

// notImplemented reports an unimplemented command, naming the tracking issue.
//
// Returning a clear error is the point: a command that silently did nothing, or
// reported success, would be worse than one that says what is missing.
func notImplemented(cmd, issue string) error {
	return fmt.Errorf("%w: `cloudburrow %s` needs cluster operations, tracked by issue %s", errNotImplemented, cmd, issue)
}

// runUp loads configuration, starts the lifecycle coordinator, and blocks until
// ctx is cancelled — by a signal in normal use, or by a test.
//
// Configuration is fully validated before the coordinator starts, so an invalid
// configuration fails without creating a cluster or binding a port.
func runUp(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	cfg, err := config.Load(config.Options{Args: args, Output: stderr})
	if err != nil {
		return err
	}

	coord := lifecycle.New(time.Duration(cfg.ShutdownTimeout))
	control := lifecycle.NewControlServer(cfg.Endpoints.Control, coord)
	coord.Register(control)

	if err := coord.Start(ctx); err != nil {
		return err
	}

	printStartup(stdout, cfg, control)

	<-ctx.Done()
	fmt.Fprintln(stdout, "\nshutting down...")

	// Shutdown runs on a fresh context: the signal that triggered it already
	// cancelled ctx, and reusing it would give the drain window zero time.
	stopErr := coord.Stop(context.WithoutCancel(ctx))
	if stopErr != nil {
		if errors.Is(stopErr, lifecycle.ErrShutdownTimeout) {
			return fmt.Errorf("shutdown incomplete after %s: %w", cfg.ShutdownTimeout, stopErr)
		}
		return stopErr
	}
	fmt.Fprintln(stdout, "stopped")
	return nil
}

// printStartup reports what actually started, and what did not.
//
// The unimplemented notice is not decoration: every service is still Planned in
// docs/compatibility.md, and a caller who saw only "ready" could reasonably
// assume Cloud Storage was listening.
func printStartup(w io.Writer, cfg config.Config, control *lifecycle.ControlServer) {
	fmt.Fprintf(w, "cloudburrow %q\n", cfg.Name)
	fmt.Fprintf(w, "  control:    http://%s  (health: /healthz, readiness: /readyz)\n", control.Addr())
	fmt.Fprintf(w, "  cluster:    %s (%s, %s)\n", cfg.ClusterName(), cfg.Cluster.Provider, cfg.Cluster.NodeImage)
	fmt.Fprintf(w, "  namespace:  %s\n", cfg.Cluster.Namespace)
	fmt.Fprintf(w, "  kubeconfig: %s\n", cfg.KubeconfigPath())
	fmt.Fprintf(w, "  mode:       %s\n", cfg.Mode)

	if !cfg.IsLoopback() {
		fmt.Fprintf(w, "\n  WARNING: bound to %s, which is not loopback.\n", cfg.BindAddress)
		fmt.Fprintf(w, "  CloudBurrow performs no authentication. Anyone who can reach this\n")
		fmt.Fprintf(w, "  address controls it and the cluster it manages.\n")
	}

	fmt.Fprintf(w, "\n  services: ")
	for i, s := range cfg.EnabledServices() {
		if i > 0 {
			fmt.Fprint(w, ", ")
		}
		fmt.Fprint(w, string(s))
	}
	fmt.Fprintln(w)

	// State that never survives is stated up front, not discovered later.
	if eph := cfg.EphemeralServices(); len(eph) > 0 {
		fmt.Fprintf(w, "  NOT PERSISTED: ")
		for i, s := range eph {
			if i > 0 {
				fmt.Fprint(w, ", ")
			}
			fmt.Fprint(w, string(s))
		}
		fmt.Fprintf(w, " — state is lost on restart.\n")
	}

	fmt.Fprintf(w, "\n  NOT STARTED: no cluster is created and no service listens yet. Cluster\n")
	fmt.Fprintf(w, "  lifecycle is issue #9; networking and image loading are #26. Every\n")
	fmt.Fprintf(w, "  operation is Planned in docs/compatibility.md.\n")
	fmt.Fprintf(w, "\npress Ctrl-C to stop\n")
}

// runStatus reports the configured instance and what is known about it.
//
// Configuration is resolved and printed even though cluster inspection is not
// implemented, because a wrong endpoint or instance name is the most common
// thing a developer needs to check.
func runStatus(args []string, stdout, stderr io.Writer) error {
	cfg, err := config.Load(config.Options{Args: args, Output: stderr})
	if err != nil {
		return err
	}

	fmt.Fprintf(stdout, "instance:   %s\n", cfg.Name)
	fmt.Fprintf(stdout, "cluster:    %s (%s)\n", cfg.ClusterName(), cfg.Cluster.NodeImage)
	fmt.Fprintf(stdout, "namespace:  %s\n", cfg.Cluster.Namespace)
	fmt.Fprintf(stdout, "kubeconfig: %s\n", cfg.KubeconfigPath())
	fmt.Fprintf(stdout, "mode:       %s\n", cfg.Mode)
	fmt.Fprintf(stdout, "bind:       %s\n", cfg.BindAddress)
	fmt.Fprintln(stdout, "\nservice persistence:")
	for _, s := range cfg.EnabledServices() {
		note := "survives restart in persistent mode"
		if s.Persistence() == config.PersistenceNone {
			note = "never survives restart (backend does not persist)"
		} else if cfg.Mode == config.ModeEphemeral {
			note = "not persisted in ephemeral mode"
		}
		fmt.Fprintf(stdout, "  %-8s %s\n", s, note)
	}
	fmt.Fprintf(stdout, "\ncluster state: unknown — inspection is not implemented (issue #9)\n")
	return nil
}

// runStop, runReset and runDelete define the command surface. Their semantics
// are documented here and in docs/configuration.md; the cluster operations they
// need belong to issue #9.
//
// They are registered rather than omitted so the distinction between them is
// established now: `stop` preserves state, `reset` destroys state but keeps the
// cluster, `delete` destroys the cluster. None implies another.
func runStop(args []string, _, stderr io.Writer) error {
	if _, err := config.Load(config.Options{Args: args, Output: stderr}); err != nil {
		return err
	}
	return notImplemented("stop", "#9")
}

func runReset(args []string, _, stderr io.Writer) error {
	if _, err := config.Load(config.Options{Args: args, Output: stderr}); err != nil {
		return err
	}
	return notImplemented("reset", "#9")
}

func runDelete(args []string, _, stderr io.Writer) error {
	if _, err := config.Load(config.Options{Args: args, Output: stderr}); err != nil {
		return err
	}
	return notImplemented("delete", "#9")
}
