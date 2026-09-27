package main

import (
	"context"
	"errors"
	"fmt"
	"github.com/cloudburrow/cloudburrow/internal/metrics"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
	"github.com/cloudburrow/cloudburrow/internal/store"
	"github.com/cloudburrow/cloudburrow/internal/telemetry"
	grpctransport "github.com/cloudburrow/cloudburrow/internal/transport/grpc"
	"github.com/cloudburrow/cloudburrow/internal/version"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/admin"
	"github.com/cloudburrow/cloudburrow/internal/cluster"
	"github.com/cloudburrow/cloudburrow/internal/components"
	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/doctor"
	"github.com/cloudburrow/cloudburrow/internal/lifecycle"
	"github.com/cloudburrow/cloudburrow/internal/metadata"
	"github.com/cloudburrow/cloudburrow/internal/netfwd"
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

	if info, ok := running(cfg); ok {
		return fmt.Errorf("instance %q is %w (pid %d, control http://%s); "+
			"`cloudburrow stop` ends it", cfg.Name, errAlreadyRunning, info.PID, info.Control)
	}
	// Before anything is created: an invalid seed file must leave nothing
	// behind, not a cluster that then fails to seed.
	seedPlan, err := planSeedFile(cfg)
	if err != nil {
		return err
	}
	// Before anything is created, too: a taken port was otherwise found only
	// when its listener started, after the cluster existed (#587).
	if err := preflightPorts(cfg, doctor.RealEnv(), stderr); err != nil {
		return err
	}

	// up.log, timestamped, is where `cloudburrow logs` reads the in-process
	// services from. Opened only after the check above: a refused second
	// `up` must not truncate the running one's log.
	if os.Getenv(detachedEnv) != "" {
		// Detached, stdout is already up.log; one writer for both streams,
		// so a line from each cannot interleave mid-stamp.
		sw := newStampedWriter(stdout)
		stdout, stderr = sw, sw
	} else if err := os.MkdirAll(cfg.InstanceDir(), 0o700); err == nil {
		if f, err := os.OpenFile(upLogPath(cfg), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600); err == nil {
			defer func() { _ = f.Close() }()
			sw := newStampedWriter(f)
			stdout, stderr = io.MultiWriter(stdout, sw), io.MultiWriter(stderr, sw)
		}
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
	creds, err := metadata.LoadOrCreate(cfg.InstanceDir(), cfg.DefaultProject(), tokenURI(cfg))
	if err != nil {
		return err
	}
	adcPath, err := creds.WriteADC(cfg.InstanceDir())
	if err != nil {
		return err
	}
	metaSrv := metadata.NewServer(creds, cfg.DefaultProject(), cfg.BindAddress, cfg.Endpoints.Metadata)

	coord := lifecycle.New(time.Duration(cfg.ShutdownTimeout))
	control := lifecycle.NewControlServer(cfg.Endpoints.Control, coord)
	clusterComp := cluster.NewComponent(c, kindConfig, time.Duration(cfg.ReadyTimeout), stdout)
	comps := components.NewLifecycleComponent(cfg.KubeconfigPath(), cfg, stdout)
	signingKeys, err := storageSigningKeys(cfg, creds)
	if err != nil {
		return err
	}
	comps.SetStorageSigningKeys(signingKeys)
	var mysqlCreds components.MySQLCredentials
	if serviceEnabled(cfg, config.ServiceCloudSQLMySQL) {
		if mysqlCreds, err = loadOrCreateMySQLCredentials(cfg); err != nil {
			return fmt.Errorf("Cloud SQL for MySQL credentials: %w", err)
		}
		comps.SetMySQLCredentials(mysqlCreds)
	}

	// Order matters: the control server first so health and readiness are
	// observable while the cluster comes up, then the cluster, then the
	// components that need it, then the tunnels that need those.
	forwarders := buildForwarders(cfg)
	for _, f := range forwarders {
		// What each tunnel did, in up.log and so in a diagnose bundle (#526).
		f.Logf = func(format string, args ...any) {
			fmt.Fprintf(stderr, "cloudburrow: "+format+"\n", args...)
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
	kmsSvc := newKMSService(cfg)
	kmsSvc.register(coord)
	schedulerSvc := newSchedulerService(cfg, func() *netfwd.Forwarder { return forwarderFor(forwarders, "pubsub") })
	schedulerSvc.register(coord)
	loggingSvc := newLoggingService(cfg)
	loggingSvc.register(coord)
	if secretsSvc != nil {
		runSvc.useSecrets(lazySecretResolver{svc: secretsSvc})
	}

	// Admin routes must be mounted before the control server starts: it builds
	// its mux at Start, so anything added afterwards is never routed.
	// The resetter and seeder resolve their store lazily, so registering them
	// before the services start is safe.
	recorder := admin.NewRecorder(1000, nil)
	// The registry is opened below, after the routes are mounted, so the reset
	// reads it through this at reset time.
	var registered func() []string = func() []string { return nil }
	var projectRegistry *resourcemanager.Registry
	// The admin token (#553): minted now, required on every /admin route,
	// written beside the runtime file once the control server is up.
	adminToken, err := newAdminToken()
	if err != nil {
		return err
	}
	adminAPI := mountAdmin(control, recorder, cfg, adminDeps{
		tasks: tasksSvc, secrets: secretsSvc, forwarders: forwarders,
		kms:       kmsSvc,
		mysql:     mysqlCreds,
		scheduler: schedulerSvc,
		logging:   loggingSvc,
		projects:  func() []string { return registered() },
		projectStore: func() store.Store {
			if projectRegistry == nil {
				return nil
			}
			return projectRegistry.Backing()
		},
	}, adminToken)
	// Every API call on the ports CloudBurrow serves itself is recorded, so
	// /admin/events answers "did my call arrive" rather than returning [].
	// Traffic to Storage, Pub/Sub and the opt-in emulators goes through a raw
	// port-forward to an upstream process and cannot be observed here.
	// The same observers count every call into /metrics (#292), which is
	// served on the control port beside the admin API. Building an observer
	// marks its service measured, so the unmeasured set is whatever enabled
	// service ends up with none (#600), never a list kept here.
	requestMetrics := metrics.New(metricsServices(cfg)...)
	control.Mount(func(mux *http.ServeMux) { mux.Handle("GET /metrics", metricsHandler(requestMetrics)) })
	// Fault injection (#306) on the services CloudBurrow serves itself. The
	// builtin Cloud Storage server runs in the cluster, where a rule cannot
	// reach it, so storage is not interposed and its rules stay refused.
	faults := adminAPI.Faults()
	// One logger for the process, at --log-level (#314): the request log of
	// every service CloudBurrow serves, and anything else that uses slog.
	level, err := grpctransport.ParseLevel(cfg.LogLevel)
	if err != nil {
		return err
	}
	logger := slog.New(grpctransport.NewLineHandler(stderr, level))
	slog.SetDefault(logger)
	// OpenTelemetry traces (#313), only when an OTLP endpoint is configured.
	tracing, err := telemetry.Setup(ctx, os.Getenv, version.Get().Version)
	if err != nil {
		return err
	}
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tracing.Shutdown(sctx)
	}()
	if tracing.Enabled() {
		fmt.Fprintln(stderr, "tracing: exporting spans for tasks, run, secretmanager and kms to the configured OTLP endpoint")
	}
	if tasksSvc != nil {
		tasksSvc.tracing = tracing
	}
	if runSvc != nil {
		runSvc.tracing = tracing
	}
	if secretsSvc != nil {
		secretsSvc.tracing = tracing
	}
	if kmsSvc != nil {
		kmsSvc.tracing = tracing
	}
	if tasksSvc != nil {
		tasksSvc.calls = callEvents(recorder, requestMetrics, "tasks")
		tasksSvc.interpose = append(tasksSvc.interpose, grpctransport.LogInterceptor(logger, "tasks"), faults.Interceptor("tasks"))
	}
	if runSvc != nil {
		runSvc.calls = callEvents(recorder, requestMetrics, "run")
		runSvc.interpose = append(runSvc.interpose, grpctransport.LogInterceptor(logger, "run"), faults.Interceptor("run"))
	}
	if schedulerSvc != nil {
		schedulerSvc.calls = callEvents(recorder, requestMetrics, "scheduler")
		schedulerSvc.interpose = append(schedulerSvc.interpose, grpctransport.LogInterceptor(logger, "scheduler"))
	}
	if loggingSvc != nil {
		loggingSvc.calls = callEvents(recorder, requestMetrics, "logging")
		loggingSvc.interpose = append(loggingSvc.interpose, grpctransport.LogInterceptor(logger, "logging"))
	}
	if kmsSvc != nil {
		kmsSvc.calls = callEvents(recorder, requestMetrics, "kms")
		kmsSvc.interpose = append(kmsSvc.interpose, grpctransport.LogInterceptor(logger, "kms"), faults.Interceptor("kms"))
		kmsSvc.requests = requestEvents(recorder, requestMetrics, "kms")
	}
	if secretsSvc != nil {
		secretsSvc.calls = callEvents(recorder, requestMetrics, "secretmanager")
		secretsSvc.interpose = append(secretsSvc.interpose, grpctransport.LogInterceptor(logger, "secretmanager"), faults.Interceptor("secretmanager"))
		secretsSvc.requests = requestEvents(recorder, requestMetrics, "secretmanager")
	}

	// The runtime file straight after the control server, so it names an
	// address already listening and `wait` can follow startup from the start.
	runtime := &runtimeFile{cfg: cfg, control: control, detached: os.Getenv(detachedEnv) != "", token: adminToken}
	coord.Register(control, runtime, metaSrv, clusterComp)
	if serviceEnabled(cfg, config.ServiceStorage) {
		// Between the cluster and the components: the image must be in the
		// cluster before its Deployment is (#514).
		coord.Register(newStorageImageComponent(cfg.KubeconfigPath(), cfg.ClusterName(), comps, stdout))
	}
	coord.Register(comps)
	// Once the cluster answers: Cloud KMS and Secret Manager drop what --mode
	// says must not survive (#481, #483).
	kmsSvc.registerForget(coord)
	secretsSvc.registerForget(coord)
	for _, f := range forwarders {
		coord.Register(f)
	}
	if f := forwarderFor(forwarders, "storage"); f != nil {
		coord.Register(newStorageEventScraper(f, recorder, requestMetrics))
	}

	// The project registry, opened before the console because the console
	// lists it and preselects the instance's own project from it.
	projects, releaseProjects, err := openProjects(cfg)
	if err == nil && projects != nil {
		projectRegistry = projects
		registered = func() []string {
			list, err := projects.List()
			if err != nil {
				return nil
			}
			ids := make([]string, 0, len(list))
			for _, p := range list {
				ids = append(ids, p.ProjectID)
			}
			return ids
		}
	}
	// Deferred immediately, and before the error check: a failure partway
	// through opening still has to give back whatever it claimed, or the next
	// start refuses for a reason that no longer exists.
	defer releaseProjects()
	if err != nil {
		return err
	}
	// The Resource Manager v3 Projects API (#298), over the same registry the
	// console lists, so a project made through either is in both.
	rmSrv := resourcemanager.NewServer(
		net.JoinHostPort(cfg.BindAddress, strconv.Itoa(cfg.Endpoints.ResourceManager)), projects)
	rmSrv.Observe(callEvents(recorder, requestMetrics, "resourcemanager"),
		requestEvents(recorder, requestMetrics, "resourcemanager"))
	rmSrv.Interpose(grpctransport.LogInterceptor(logger, "resourcemanager"))
	coord.Register(rmSrv)

	// Built before the console so the console can offer its playground, and
	// registered after it for the same reason the other components are:
	// ownership by the coordinator rather than a goroutine beside it.
	localAISrv, err := buildLocalAI(cfg)
	if err != nil {
		return err
	}

	// The console reads through the objects above rather than owning any of
	// them, so it cannot answer from a store of its own.
	consoleSrv := buildConsole(consoleDeps{
		cfg: cfg, coord: coord, cluster: clusterComp, localAI: localAISrv,
		projects: projects,
		tasks:    tasksSvc, secrets: secretsSvc, forwarders: forwarders,
		kms: kmsSvc, scheduler: schedulerSvc, run: runSvc,
		metaAddr: metaSrv.Addr,
		ingress: func() string {
			if cfg.Endpoints.Ingress == 0 {
				return ""
			}
			return net.JoinHostPort(cfg.BindAddress, strconv.Itoa(cfg.Endpoints.Ingress))
		},
	})
	if consoleSrv != nil {
		// The Request Log reads the same recorder /admin/events does.
		consoleSrv.SetRequests(newConsoleRequests(recorder, cfg))
		consoleSrv.SetRequestMetrics(requestMetrics)
		coord.Register(consoleSrv)
		// The metric sampler runs on the instance's own clock, so the history
		// a chart draws exists whether or not anybody has the dashboard open.
		if sampler := consoleSampler(consoleSrv); sampler != nil {
			coord.Register(sampler)
		}
		// Pod logs are followed rather than read once: a log view that only
		// shows what existed when the page loaded never shows the line
		// explaining the failure that just happened.
		newLogCollector(cfg.KubeconfigPath(), nil, consoleSrv.Logs()).register(coord)
		tasksSvc.observeAttempts(taskLogger{recorder: consoleSrv.Logs()}.Attempt)
		// Entries written through the Logging API appear in the Logs
		// Explorer beside the pod logs (#304).
		loggingSvc.toConsole(consoleSrv.Logs())
	}

	if localAISrv != nil {
		coord.Register(localAISrv)
	}

	// hostComp publishes the CLI-hosted services to the cluster (#575); nil
	// without Cloud Run.
	var hostComp *clusterHost
	// Every bound address, as the runtime file records it and the hooks see it.
	liveEndpoints := func() map[string]string {
		live := map[string]string{"control": control.Addr(), "metadata": metaSrv.Addr()}
		if consoleSrv != nil && consoleSrv.Addr() != "" {
			live["console"] = consoleSrv.Addr()
		}
		if a := kmsSvc.Addr(); a != "" {
			live["kms"] = a
		}
		if a := rmSrv.Addr(); a != "" {
			live["resourcemanager"] = a
		}
		if a := schedulerSvc.Addr(); a != "" {
			live["scheduler"] = a
		}
		if a := loggingSvc.Addr(); a != "" {
			live["logging"] = a
		}
		for _, e := range startupEndpoints(cfg, forwarders, tasksSvc, runSvc, secretsSvc, kmsSvc, hostComp) {
			live[e.Service] = e.Host
		}
		return live
	}
	// Pods reach the CLI-hosted APIs and the metadata server through one
	// cluster name when a cluster workload can need them (#575): with Cloud
	// Run, whose containers are the reason. Registered after every service,
	// so each has its bound address. Control and admin are never included.
	if serviceEnabled(cfg, config.ServiceRun) {
		published := map[string]bool{"run": true, "tasks": true, "secretmanager": true, "kms": true,
			"scheduler": true, "logging": true, "resourcemanager": true, "metadata": true}
		hostComp = newClusterHost(cfg, stdout, func() map[string]string {
			out := map[string]string{}
			for k, v := range liveEndpoints() {
				if published[k] {
					out[k] = v
				}
			}
			return out
		})
		coord.Register(hostComp)
	}
	// Last: ready hooks run once everything above has started, and shutdown
	// hooks run first, before anything they use is stopped.
	if seedPlan != nil {
		adminAPI.SetStartupSeed(seedPlan)
		coord.Register(&seedComponent{api: adminAPI, plan: seedPlan, file: cfg.SeedFile, out: stdout})
	}
	coord.Register(&hooksComponent{cfg: cfg, env: hookEnvironment(cfg, liveEndpoints, adcPath), out: stdout, runtime: runtime})

	if err := coord.Start(ctx); err != nil {
		return describeClusterError(err)
	}
	fmt.Fprintln(stdout, readySummary(coord))

	// The dispatch worker exists only once the service has started.
	if w := tasksSvc.Worker(); w != nil {
		coord.RegisterWorker(w)
	}
	if w := schedulerSvc.Worker(); w != nil {
		coord.RegisterWorker(w)
	}

	printStartup(stdout, cfg, control, clusterComp, forwarders, tasksSvc, runSvc, secretsSvc, kmsSvc, hostComp)
	// Recorded once every address is bound, so `env` can export an
	// OS-assigned port, which configuration alone cannot know.
	if err := runtime.Publish(liveEndpoints(), hostComp.InClusterAll()); err != nil {
		fmt.Fprintf(stderr, "warning: could not record endpoints for `cloudburrow env`: %v\n", err)
	}

	// Reported after the endpoint block, because it is the one address whose
	// availability depends on how the cluster was created rather than on what
	// just started.
	if comps.NeedsKnative() {
		reachable, reason := checkIngress(ctx, cfg)
		printIngress(stdout, cfg, reachable, reason)
	}
	printCredentials(stdout, metaSrv, adcPath)
	printLocalAI(stdout, localAISrv, cfg)
	printConsole(stdout, consoleSrv)

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

// preflightPorts refuses to start when a port `up` would bind is taken,
// with the report `doctor` prints for it. The ingress port is left out: it is
// published by the cluster, and a cluster this instance already has holds it.
func preflightPorts(cfg config.Config, env doctor.Env, stderr io.Writer) error {
	opts := doctorOptions(cfg)
	delete(opts.Ports, "ingress")
	report := doctor.Ports(env, opts)
	if !report.Blocking() {
		return nil
	}
	var taken []string
	for _, r := range report.Results {
		if r.Level == doctor.LevelFail {
			taken = append(taken, strings.TrimPrefix(r.Name, "port ")+" ("+strings.TrimSuffix(r.Detail, " is in use")+")")
		}
	}
	fmt.Fprintf(stderr, "cloudburrow up: ports this instance would bind are in use; nothing was created\n\n")
	report.Write(stderr)
	return fmt.Errorf("ports in use: %s; nothing was created", strings.Join(taken, ", "))
}

// inCluster reports whether a service's backend runs in the cluster and is
// reached through a tunnel. Every other service is served by `up` itself,
// which is also what `cloudburrow logs` reads up.log for.
func inCluster(s config.Service) bool {
	return s == config.ServiceStorage || s == config.ServicePubSub || components.OptionalPort(s) != 0
}

// printStartup reports what actually started, and what did not.
//
// The closing notice is not decoration. "Running" is not "supported": a backend
// can be up and still not implement the operation a caller is about to try, and
// someone who saw only "ready" could reasonably assume it did. The notice points
// at the record of what an official SDK has actually been shown to do.
// buildForwarders returns a tunnel per service that has an in-cluster backend.
// buildForwarders returns a tunnel per service that has an in-cluster
// backend.
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
			if !inCluster(s) {
				// Cloud Tasks, Cloud Run and the others served in-process
				// have no backend Service.
				continue
			}
			port = components.OptionalPort(s)
			// The configured port, so `cloudburrow env` — a separate
			// process — can export the same address this binds. Cloud SQL
			// has no configured port and no emulator variable, so it stays
			// OS-assigned.
			hostPort = cfg.Endpoints.OptionalPort(s)
		}
		out = append(out, netfwd.New(netfwd.Target{
			Name:        string(s),
			Namespace:   cfg.Cluster.Namespace,
			ServicePort: port,
			HostPort:    hostPort,
		}, cfg.KubeconfigPath(), cfg.BindAddress))
		if s == config.ServiceBigQuery {
			// The Storage Read API is the same Service on a second port. It
			// gets its own tunnel, labelled apart from the REST one, because
			// the Go client's result iterator reads large results through it.
			out = append(out, netfwd.New(netfwd.Target{
				Name:        string(s),
				Label:       "bigquery-storage",
				Namespace:   cfg.Cluster.Namespace,
				ServicePort: components.BigQueryStoragePort,
				HostPort:    cfg.Endpoints.BigQueryStorage,
			}, cfg.KubeconfigPath(), cfg.BindAddress))
		}
	}
	return out
}

func printStartup(w io.Writer, cfg config.Config, control *lifecycle.ControlServer, cc *cluster.Component, fwds []*netfwd.Forwarder, tasksSvc *tasksService, runSvc *runService, secretsSvc *secretsService,
	kmsSvc *kmsService, host *clusterHost) {
	fmt.Fprintf(w, "cloudburrow %q\n", cfg.Name)
	fmt.Fprintf(w, "  project:    %s\n", projectLine(cfg))
	fmt.Fprintf(w, "  control:    http://%s  (health: /healthz, readiness: /readyz)\n", control.Addr())
	fmt.Fprintf(w, "  admin:      http://%s/admin/{reset,seed,events,state,faults}  (loopback only; Authorization: Bearer from %s)\n", control.Addr(), adminTokenPath(cfg))
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
	printKMSWarning(w, kmsSvc)

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

	eps := startupEndpoints(cfg, fwds, tasksSvc, runSvc, secretsSvc, kmsSvc, host)
	netfwd.PrintEndpoints(w, eps)

	fmt.Fprintf(w, "\n  kubectl --kubeconfig %s get nodes\n", cfg.KubeconfigPath())
	fmt.Fprintf(w, "\n  Running is not the same as supported. docs/compatibility.md records, per\n")
	fmt.Fprintf(w, "  operation, what an official Google SDK has been shown to do here; anything\n")
	fmt.Fprintf(w, "  not marked Verified there is not claimed.\n")
}

// startupEndpoints lists every endpoint `up` serves, with the address it
// actually bound. It is what the banner prints and what the runtime file
// records, so `env` and a CI job read the same addresses a person reads.
func startupEndpoints(cfg config.Config, fwds []*netfwd.Forwarder, tasksSvc *tasksService, runSvc *runService,
	secretsSvc *secretsService, kmsSvc *kmsService, host *clusterHost) []netfwd.Endpoint {
	// A CLI-hosted service's in-cluster address is the cloudburrow-host
	// name once that is published (#575); before, and without Cloud Run,
	// there is none, and the table says the CLI serves it.
	inCluster := func(service, addr string) string {
		if a := host.InCluster(service); a != "" {
			return a
		}
		return addr + " (served by the CLI, not the cluster)"
	}
	var eps []netfwd.Endpoint
	for _, f := range fwds {
		if addr := f.HostAddr(); addr != "" {
			name := strings.TrimPrefix(f.Name(), "forward:")
			inCluster := f.InClusterAddr()
			eps = append(eps, netfwd.NewEndpoint(name, addr, inCluster))
		}
	}
	if addr := runSvc.Addr(); addr != "" {
		eps = append(eps, netfwd.NewEndpoint("run", addr, inCluster("run", addr)))
	}
	if addr := tasksSvc.Addr(); addr != "" {
		eps = append(eps, netfwd.NewEndpoint("tasks", addr, inCluster("tasks", addr)))
	}
	if addr := secretsSvc.Addr(); addr != "" {
		eps = append(eps, netfwd.NewEndpoint("secretmanager", addr, inCluster("secretmanager", addr)))
	}
	if addr := kmsSvc.Addr(); addr != "" {
		eps = append(eps, netfwd.NewEndpoint("kms", addr, inCluster("kms", addr)))
	}
	return eps
}

// runStatus reports the configured instance and what is known about it.
//
// Configuration is resolved and printed even though cluster inspection is not
// implemented, because a wrong endpoint or instance name is the most common
// thing a developer needs to check.
func runStatus(args []string, stdout, stderr io.Writer) error {
	format, _, args, err := splitFlag(args, "format", false)
	if err != nil || (format != "" && format != "text" && format != "json") {
		fmt.Fprintf(stderr, "invalid -format %q: want text or json\n", format)
		return errUsage
	}
	cfg, err := config.Load(config.Options{Args: args, Output: stderr})
	if err != nil {
		return err
	}
	if format == "json" {
		return runStatusJSON(cfg, stdout)
	}

	fmt.Fprintf(stdout, "instance:   %s\n", cfg.Name)
	fmt.Fprintf(stdout, "project:    %s\n", projectLine(cfg))
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
	printConfiguredEndpoints(stdout, cfg)
	printHookResults(stdout, cfg)
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
		if stamp, err := c.ReadStamp(ctx); err != nil {
			fmt.Fprintf(stdout, "versions:      unknown (%v)\n", err)
		} else {
			printStampedVersions(stdout, cfg, stamp)
		}
		fmt.Fprintf(stdout, "kubectl:       kubectl --kubeconfig %s get nodes\n", cfg.KubeconfigPath())
	}
	return nil
}

// printStampedVersions prints what the cluster is stamped with, and what
// the next `up` will do about any difference from the pins (#601).
func printStampedVersions(w io.Writer, cfg config.Config, s cluster.Stamp) {
	if !s.Present {
		fmt.Fprintln(w, "versions:      not recorded (the cluster predates the version stamp; the next `up` records it)")
		return
	}
	unrecorded := func(v string) string {
		if v == "" {
			return "not recorded"
		}
		return v
	}
	fmt.Fprintf(w, "versions:      stamped by cloudburrow %s\n", unrecorded(s.CLIVersion))
	fmt.Fprintf(w, "  node image:  %s\n", unrecorded(s.NodeImage))
	if s.NodeImage != "" && !cluster.SameNodeImage(s.NodeImage, cfg.Cluster.NodeImage) {
		fmt.Fprintf(w, "               differs from the pinned %s; `up` refuses until `cloudburrow delete`\n", cfg.Cluster.NodeImage)
	}
	fmt.Fprintf(w, "  knative:     %s\n", unrecorded(s.KnativeVersion))
	if s.KnativeVersion != "" && s.KnativeVersion != components.KnativeVersion {
		if cmp, ok := cluster.CompareKnative(s.KnativeVersion, components.KnativeVersion); ok && cmp > 0 {
			fmt.Fprintf(w, "               newer than the pinned %s; `up` with run enabled refuses to downgrade it\n", components.KnativeVersion)
		} else {
			fmt.Fprintf(w, "               differs from the pinned %s; `up` with run enabled applies it in place\n", components.KnativeVersion)
		}
	}
}

// printConfiguredEndpoints lists each enabled service's host address as
// configured, which is what `up` binds when the port is fixed. `status` is a
// separate process and cannot ask a running `up` for an OS-assigned port, so it
// says that rather than guessing one.
func printConfiguredEndpoints(w io.Writer, cfg config.Config) {
	fmt.Fprintln(w, "\nendpoints (as configured):")
	for _, s := range cfg.EnabledServices() {
		for _, e := range configuredEndpoints(cfg, s) {
			addr := "OS-assigned; `up` prints it"
			if e.port != 0 {
				addr = net.JoinHostPort(cfg.BindAddress, strconv.Itoa(e.port))
			}
			fmt.Fprintf(w, "  %-16s %s\n", e.name, addr)
			if note := netfwd.ScopeNoteFor(e.name); note != "" {
				fmt.Fprintf(w, "  %-16s %s\n", "", note)
			}
			if s == config.ServiceCloudSQLMySQL {
				// The password is named, not printed: status output is what
				// people paste into issues, and `diagnose` collects it.
				fmt.Fprintf(w, "  %-16s user %s, database %s, password in %s (`cloudburrow env` exports it)\n", "",
					components.CloudSQLUser, components.CloudSQLDatabase, mysqlCredentialsPath(cfg))
			}
		}
	}
}

type configuredEndpoint struct {
	name string
	port int
}

func configuredEndpoints(cfg config.Config, s config.Service) []configuredEndpoint {
	e := cfg.Endpoints
	switch s {
	case config.ServiceStorage:
		return []configuredEndpoint{{"storage", e.Storage}}
	case config.ServicePubSub:
		return []configuredEndpoint{{"pubsub", e.PubSub}}
	case config.ServiceTasks:
		return []configuredEndpoint{{"tasks", e.Tasks}}
	case config.ServiceRun:
		return []configuredEndpoint{{"run", e.Run}}
	case config.ServiceSecrets:
		return []configuredEndpoint{{"secretmanager", e.Secrets}}
	case config.ServiceKMS:
		return []configuredEndpoint{{"kms", e.KMS}}
	case config.ServiceBigQuery:
		return []configuredEndpoint{{"bigquery", e.BigQuery}, {"bigquery-storage", e.BigQueryStorage}}
	case config.ServiceCloudSQL:
		// No configured port: the tunnel is always OS-assigned.
		return []configuredEndpoint{{"cloudsql", 0}}
	default:
		return []configuredEndpoint{{string(s), e.OptionalPort(s)}}
	}
}

// runStatusJSON is `status --format json`. Unlike the human form it exits
// with the state: 0 ready, 3 not running, 4 running but not ready.
func runStatusJSON(cfg config.Config, stdout io.Writer) error {
	clusterState, kubernetes := "unknown", ""
	var versions *statusVersions
	if c, err := newCluster(cfg); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if st, err := c.Status(ctx); err == nil {
			clusterState = st.String()
			if st == cluster.StatusRunning {
				kubernetes, _ = c.ServerVersion(ctx)
				if stamp, err := c.ReadStamp(ctx); err == nil {
					versions = stampVersions(stamp)
				}
			}
		}
	}
	r, code := buildStatusReport(cfg, liveStatus(cfg), clusterState, kubernetes)
	r.Cluster.Versions = versions
	return writeStatusJSON(stdout, r, code)
}

// projectLine names the default project, and says where it came from when that
// is not obvious.
//
// A derived project is the case that matters: an instance started as "demo"
// serves project "demo-local", and someone reading a resource name in an error
// needs to know that before they go looking for why "demo" was refused.
func projectLine(cfg config.Config) string {
	project := cfg.DefaultProject()
	switch {
	case cfg.Project != "":
		return project + " (set explicitly)"
	case project != cfg.Name:
		return fmt.Sprintf("%s (derived: the instance name %q is not a valid project ID; "+
			"set --project to choose another)", project, cfg.Name)
	default:
		return project
	}
}
