package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/identity-wael/cloudburrow/internal/admin"
	"github.com/identity-wael/cloudburrow/internal/cluster"
	"github.com/identity-wael/cloudburrow/internal/components"
	"github.com/identity-wael/cloudburrow/internal/config"
	"github.com/identity-wael/cloudburrow/internal/lifecycle"
	"github.com/identity-wael/cloudburrow/internal/metadata"
	"github.com/identity-wael/cloudburrow/internal/netfwd"
)

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

	c, err := newCluster(cfg)
	if err != nil {
		return describeClusterError(err)
	}

	// The kind configuration is generated before the cluster is created,
	// because extraPortMappings can only be applied at creation time.
	kindConfig, err := cluster.WriteConfig(cfg.InstanceDir(), ingressMappings(cfg))
	if err != nil {
		return err
	}

	// Credentials are generated before anything starts, because the metadata
	// server and the ADC fixture must present the same key: a client that read
	// one and talked to the other would fail with an opaque signature error.
	creds, err := metadata.LoadOrCreate(cfg.InstanceDir(), cfg.Name, tokenURI(cfg))
	if err != nil {
		return err
	}
	adcPath, err := creds.WriteADC(cfg.InstanceDir())
	if err != nil {
		return err
	}
	metaSrv := metadata.NewServer(creds, cfg.Name, cfg.BindAddress, cfg.Endpoints.Metadata)

	coord := lifecycle.New(time.Duration(cfg.ShutdownTimeout))
	control := lifecycle.NewControlServer(cfg.Endpoints.Control, coord)
	clusterComp := cluster.NewComponent(c, kindConfig, time.Duration(cfg.ReadyTimeout), stdout)
	comps := components.NewLifecycleComponent(cfg.KubeconfigPath(), cfg, stdout)

	// Order matters: the control server first so health and readiness are
	// observable while the cluster comes up, then the cluster, then the
	// components that need it, then the tunnels that need those.
	// Host ports are reserved before anything is deployed, because the storage
	// backend must be told the address its clients will use. Discovering it
	// afterwards would mean patching the Deployment, which replaces the pod and
	// breaks the very tunnel that revealed the address.
	forwarders := buildForwarders(cfg)
	for _, f := range forwarders {
		if f.Name() == "forward:storage" {
			comps.SetStorageExternalURL("http://" + f.HostAddr())
		}
	}

	// Cloud Tasks has no upstream backend, so it runs in this process rather
	// than as a cluster workload.
	tasksSvc := newTasksService(cfg)
	tasksSvc.register(coord)

	// The Cloud Run adapter is ours even though the execution engine is the
	// cluster's, so it also runs in this process.
	runSvc := newRunService(cfg)
	runSvc.register(coord)

	// Secret Manager has no upstream emulator either, so it too is served
	// from this process.
	secretsSvc := newSecretsService(cfg)
	secretsSvc.register(coord)
	if secretsSvc != nil {
		runSvc.useSecrets(lazySecretResolver{svc: secretsSvc})
	}

	// Admin routes must be mounted before the control server starts: it builds
	// its mux at Start, so anything added afterwards is never routed.
	// The resetter and seeder resolve their store lazily, so registering them
	// before the services start is safe.
	recorder := admin.NewRecorder(1000, nil)
	mountAdmin(control, recorder, cfg, tasksSvc)

	coord.Register(control, metaSrv, clusterComp, comps)
	for _, f := range forwarders {
		coord.Register(f)
	}

	if err := coord.Start(ctx); err != nil {
		return describeClusterError(err)
	}

	// The dispatch worker exists only once the service has started.
	if w := tasksSvc.Worker(); w != nil {
		coord.RegisterWorker(w)
	}

	printStartup(stdout, cfg, control, clusterComp, forwarders, tasksSvc, runSvc, secretsSvc)

	// Reported after the endpoint block, because it is the one address whose
	// availability depends on how the cluster was created rather than on what
	// just started.
	if comps.NeedsKnative() {
		reachable, reason := checkIngress(ctx, cfg)
		printIngress(stdout, cfg, reachable, reason)
	}
	printCredentials(stdout, metaSrv, adcPath)

	// Last, so it is the final line of the block no matter which optional
	// sections printed above it.
	fmt.Fprintln(stdout, "\npress Ctrl-C to stop")

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
// buildForwarders returns a tunnel per service that has an in-cluster backend.
func buildForwarders(cfg config.Config) []*netfwd.Forwarder {
	var out []*netfwd.Forwarder
	for _, s := range cfg.EnabledServices() {
		var port, hostPort int
		switch s {
		case config.ServicePubSub:
			port, hostPort = components.PubSubPort, cfg.Endpoints.PubSub
		case config.ServiceStorage:
			port, hostPort = components.StoragePort, cfg.Endpoints.Storage
		default:
			if p := components.OptionalPort(s); p != 0 {
				port = p
				hostPort = 0 // OS-assigned; optional services have no fixed slot
			} else {
				// Cloud Tasks and Cloud Run have no backend Service.
				continue
			}
		}
		out = append(out, netfwd.New(netfwd.Target{
			Name:        string(s),
			Namespace:   cfg.Cluster.Namespace,
			ServicePort: port,
			HostPort:    hostPort,
		}, cfg.KubeconfigPath(), cfg.BindAddress))
	}
	return out
}

func printStartup(w io.Writer, cfg config.Config, control *lifecycle.ControlServer, cc *cluster.Component, fwds []*netfwd.Forwarder, tasksSvc *tasksService, runSvc *runService, secretsSvc *secretsService) {
	fmt.Fprintf(w, "cloudburrow %q\n", cfg.Name)
	fmt.Fprintf(w, "  control:    http://%s  (health: /healthz, readiness: /readyz)\n", control.Addr())
	fmt.Fprintf(w, "  admin:      http://%s/admin/{reset,seed,events}  (loopback only)\n", control.Addr())
	version := cc.ServerVersion()
	if version == "" {
		version = "version unknown"
	}
	fmt.Fprintf(w, "  cluster:    %s (%s, Kubernetes %s)\n", cfg.ClusterName(), cfg.Cluster.Provider, version)
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

	var eps []netfwd.Endpoint
	for _, f := range fwds {
		if addr := f.HostAddr(); addr != "" {
			name := strings.TrimPrefix(f.Name(), "forward:")
			inCluster := f.InClusterAddr()
			if name == "storage" {
				// Workloads use a second endpoint: one process can only match
				// its download path against a single host. See
				// components.StorageInternalBackend.
				inCluster = components.InClusterStorageHost(cfg.Cluster.Namespace)
			}
			eps = append(eps, netfwd.NewEndpoint(name, addr, inCluster))
		}
	}
	if addr := runSvc.Addr(); addr != "" {
		eps = append(eps, netfwd.NewEndpoint("run", addr, addr+" (served by the CLI, not the cluster)"))
	}
	if addr := tasksSvc.Addr(); addr != "" {
		// Cloud Tasks is served from this process, so it has no in-cluster
		// Service DNS name; workloads reach it through the host address.
		eps = append(eps, netfwd.NewEndpoint("tasks", addr, addr+" (served by the CLI, not the cluster)"))
	}
	if addr := secretsSvc.Addr(); addr != "" {
		eps = append(eps, netfwd.NewEndpoint("secretmanager", addr, addr+" (served by the CLI, not the cluster)"))
	}
	netfwd.PrintEndpoints(w, eps)

	fmt.Fprintf(w, "\n  kubectl --kubeconfig %s get nodes\n", cfg.KubeconfigPath())
	fmt.Fprintf(w, "\n  NOT VERIFIED: the backends are running, but no operation has been\n")
	fmt.Fprintf(w, "  demonstrated through an official Google SDK. Every operation is still\n")
	fmt.Fprintf(w, "  Planned in docs/compatibility.md until #10 proves otherwise.\n")
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
	c, err := newCluster(cfg)
	if err != nil {
		return describeClusterError(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	status, err := c.Status(ctx)
	if err != nil {
		fmt.Fprintf(stdout, "\ncluster state: unknown (%v)\n", err)
		return nil
	}
	fmt.Fprintf(stdout, "\ncluster state: %s\n", status)
	if status == cluster.StatusRunning {
		if v, err := c.ServerVersion(ctx); err == nil {
			fmt.Fprintf(stdout, "kubernetes:    %s\n", v)
		}
		fmt.Fprintf(stdout, "kubectl:       kubectl --kubeconfig %s get nodes\n", cfg.KubeconfigPath())
	}
	return nil
}
