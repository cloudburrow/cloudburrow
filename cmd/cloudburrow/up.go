package main

import (
	loggingpb "cloud.google.com/go/logging/apiv2/loggingpb"
	"context"
	"errors"
	"fmt"
	runadapter "github.com/cloudburrow/cloudburrow/internal/adapter/run"
	"github.com/cloudburrow/cloudburrow/internal/authplatform"
	"github.com/cloudburrow/cloudburrow/internal/egress"
	"github.com/cloudburrow/cloudburrow/internal/faultgate"
	"github.com/cloudburrow/cloudburrow/internal/firestoreadmin"
	"github.com/cloudburrow/cloudburrow/internal/hostguard"
	"github.com/cloudburrow/cloudburrow/internal/iamcheck"
	"github.com/cloudburrow/cloudburrow/internal/iameval"
	"github.com/cloudburrow/cloudburrow/internal/metrics"
	"github.com/cloudburrow/cloudburrow/internal/orgpolicy"
	"github.com/cloudburrow/cloudburrow/internal/resource"
	"github.com/cloudburrow/cloudburrow/internal/service/apikeys"
	"github.com/cloudburrow/cloudburrow/internal/service/backupdr"
	"github.com/cloudburrow/cloudburrow/internal/service/billing"
	"github.com/cloudburrow/cloudburrow/internal/service/cloudquotas"
	"github.com/cloudburrow/cloudburrow/internal/service/firebaserules"
	"github.com/cloudburrow/cloudburrow/internal/service/logging"
	"github.com/cloudburrow/cloudburrow/internal/service/monitoring"
	"github.com/cloudburrow/cloudburrow/internal/service/parametermanager"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
	"github.com/cloudburrow/cloudburrow/internal/service/serviceusage"
	"github.com/cloudburrow/cloudburrow/internal/service/siteverification"
	"github.com/cloudburrow/cloudburrow/internal/service/sqladmin"
	"github.com/cloudburrow/cloudburrow/internal/service/storagejobs"
	"github.com/cloudburrow/cloudburrow/internal/store"
	"github.com/cloudburrow/cloudburrow/internal/telemetry"
	grpctransport "github.com/cloudburrow/cloudburrow/internal/transport/grpc"
	"github.com/cloudburrow/cloudburrow/internal/unserved"
	"github.com/cloudburrow/cloudburrow/internal/version"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
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
	"github.com/cloudburrow/cloudburrow/internal/prefetch"
)

// runUp loads configuration, starts the lifecycle coordinator, and blocks until
// ctx is cancelled — by a signal in normal use, or by a test.
//
// Configuration is fully validated before the coordinator starts, so an invalid
// configuration fails without creating a cluster or binding a port.
func runUp(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	offline, args, err := offlineFlag(args)
	if err != nil {
		return err
	}
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
	// Before anything is created too: with --offline, every artifact the
	// cluster would download must be in the cache, and the first that is
	// not is named here rather than found as a pull timeout minutes in
	// (#604). Without it, whatever is cached is still preferred.
	cached, err := newOfflineCache(ctx, cfg, offline, prefetch.ExecRunner{}, stdout)
	if err != nil {
		return err
	}
	// And before anything is created: a CLI built by a plain `go build` has
	// no storage server to put in the cluster (#686). The storage-image
	// component found that only after the cluster existed.
	if err := preflightStorage(cfg, cached.plan.Arch, stderr); err != nil {
		return err
	}
	// Likewise the BigQuery emulator, which this CLI embeds and builds its
	// image from (#1061).
	if err := preflightBigQuery(cfg, cached.plan.Arch, stderr); err != nil {
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
	} else if err := privateInstanceDir(cfg); err == nil {
		if f, err := os.OpenFile(upLogPath(cfg), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600); err == nil {
			defer func() { _ = f.Close() }()
			sw := newStampedWriter(f)
			stdout, stderr = io.MultiWriter(stdout, sw), io.MultiWriter(stderr, sw)
		}
	}

	cached.out = stdout

	c, err := newCluster(cfg)
	if err != nil {
		return describeClusterError(err)
	}

	// Every port at once, before the cluster is created: a collision found
	// by the first component to bind would leave a cluster behind and name
	// only that one port.
	if err := checkHostPorts(cfg, doctor.RealEnv().PortFree, func() bool {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		st, err := c.Status(ctx)
		return err == nil && st == cluster.StatusRunning
	}); err != nil {
		return err
	}
	// Free now is not free at bind time for a port in the ephemeral range:
	// any outgoing connection can take it first (#1526). Said, not refused.
	if r := ephemeralPortsResult(cfg); r.Level == doctor.LevelWarn {
		fmt.Fprintf(stderr, "cloudburrow up: warning: %s; %s\n", r.Detail, r.Remedy)
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
	// Secure Source Manager names a review's or a pull request's author
	// from the request's bearer token (#1927).
	runadapter.SetSourceManagerCaller(func(bearer string) (string, bool) { return creds.TokenAccount(bearer, time.Now()) }, creds.Email)
	// Cloud Logging's recent queries are the requesting user's (#2031).
	logging.SetCallerResolver(func(bearer string) (string, bool) { return creds.TokenAccount(bearer, time.Now()) }, creds.Email)

	coord := lifecycle.New(time.Duration(cfg.ShutdownTimeout))
	control := lifecycle.NewControlServer(cfg.Endpoints.Control, coord)
	clusterComp := cluster.NewComponent(c, kindConfig, time.Duration(cfg.ReadyTimeout), stdout)
	comps := components.NewLifecycleComponent(cfg.KubeconfigPath(), cfg, stdout)
	// A warm start replaces Knative's pods once the Service network
	// answers, rather than waiting out their restart backoff (#1610).
	comps.SetNodeRestart(c.Restarted, func(ctx context.Context) error {
		return c.WaitServiceNetwork(ctx, cluster.ServiceNetworkTimeout)
	})
	cached.use(comps, cfg)
	signingKeys, err := storageSigningKeys(cfg, creds)
	if err != nil {
		return err
	}
	comps.SetStorageSigningKeys(signingKeys)
	comps.SetEarlierStorage(&earlierStorage{kubeconfig: cfg.KubeconfigPath(), namespace: cfg.Cluster.Namespace,
		instance: cfg.InstanceDir(), project: cfg.DefaultProject(), out: stdout})
	if serviceEnabled(cfg, config.ServiceBigQuery) || serviceEnabled(cfg, config.ServiceStorage) ||
		(cfg.IAMEnforced() && (serviceEnabled(cfg, config.ServicePubSub) || serviceEnabled(cfg, config.ServiceRun))) {
		// BigQuery's front reads a project's folders and organization
		// from Resource Manager, through cloudburrow-host (#1392,
		// INFORMATION_SCHEMA.JOBS_BY_FOLDER and JOBS_BY_ORGANIZATION),
		// and the storage server a project's and a folder's parents, for
		// Storage Intelligence inheritance (#1532).
		comps.SetResourceManager(fmt.Sprintf("%s.%s.svc.cluster.local:%d", ClusterHostService, cfg.Cluster.Namespace,
			cfg.Endpoints.ResourceManager))
		// With --iam-enforcement=on the storage server (and Pub/Sub's
		// front) ask the host's evaluator there (#1879), as Kourier does
		// for run.routes.invoke, with or without storage and Pub/Sub.
		comps.SetIAMCheck(cfg.IAMEnforced())
	}
	if serviceEnabled(cfg, config.ServicePubSub) && serviceEnabled(cfg, config.ServiceKMS) && cfg.Endpoints.KMS > 0 {
		// The Pub/Sub front checks CMEK topics' keys against Cloud KMS,
		// through cloudburrow-host (#1874).
		comps.SetKMS(fmt.Sprintf("%s.%s.svc.cluster.local:%d", ClusterHostService, cfg.Cluster.Namespace,
			cfg.Endpoints.KMS))
	}
	if serviceEnabled(cfg, config.ServiceStorage) && cfg.Endpoints.Metadata > 0 {
		// Cloud Storage verifies a URL an IAM service account signed
		// (signBlob, or a user-managed key file) against the account's
		// certificates, which the metadata server serves, through
		// cloudburrow-host (#1787).
		comps.SetServiceAccountCerts(fmt.Sprintf("%s.%s.svc.cluster.local:%d", ClusterHostService, cfg.Cluster.Namespace,
			cfg.Endpoints.Metadata))
	}
	if onnxRunnerEnabled(cfg) && cfg.Endpoints.ONNX > 0 {
		// The onnx build's ONNX runner, through cloudburrow-host, runs
		// BigQuery ML's imported ONNX models for the front (#1711).
		comps.SetONNXRunner(fmt.Sprintf("%s.%s.svc.cluster.local:%d", ClusterHostService, cfg.Cluster.Namespace,
			cfg.Endpoints.ONNX))
	}
	if tfConverterEnabled(cfg) && cfg.Endpoints.TFConvert > 0 {
		// The TensorFlow SavedModel converter, through cloudburrow-host,
		// converts BigQuery ML's imported TENSORFLOW models (#1775).
		comps.SetTFConverter(fmt.Sprintf("%s.%s.svc.cluster.local:%d", ClusterHostService, cfg.Cluster.Namespace,
			cfg.Endpoints.TFConvert))
	}
	if sparkRunnerEnabled(cfg) && cfg.Endpoints.Spark > 0 {
		// The Spark procedure runner, through cloudburrow-host, runs
		// BigQuery's stored procedures for Apache Spark (#2104).
		comps.SetSparkRunner(fmt.Sprintf("%s.%s.svc.cluster.local:%d", ClusterHostService, cfg.Cluster.Namespace,
			cfg.Endpoints.Spark))
	}
	if serviceEnabled(cfg, config.ServiceBigQuery) && localAIServed(cfg) {
		if err := pinLocalAIPort(&cfg); err != nil {
			return err
		}
	}
	if serviceEnabled(cfg, config.ServiceBigQuery) && localAIServed(cfg) && cfg.Endpoints.LocalAI > 0 {
		// The local Vertex AI endpoint, through cloudburrow-host, runs
		// BigQuery ML's remote models for the front (#1760).
		comps.SetVertexAI(fmt.Sprintf("%s.%s.svc.cluster.local:%d", ClusterHostService, cfg.Cluster.Namespace,
			cfg.Endpoints.LocalAI))
	}
	if firestoreKMS(cfg) || storageKMS(cfg) || spannerKMS(cfg) {
		// Spanner's (#1950) encrypt through it too.
		// Firestore's customer-managed keys (#1881) are checked against
		// this process's Cloud KMS, and Cloud Storage's (#1938) encrypt
		// through it, through cloudburrow-host: its port is fixed now,
		// since the front's and the server's arguments name it.
		if err := pinKMSPort(&cfg); err != nil {
			return err
		}
		comps.SetKMS(fmt.Sprintf("%s.%s.svc.cluster.local:%d", ClusterHostService, cfg.Cluster.Namespace, cfg.Endpoints.KMS))
	}
	if serviceEnabled(cfg, config.ServiceFirestore) {
		// Firestore with MongoDB compatibility serves TLS only (#1738),
		// with a certificate of the instance's own CA, which
		// CLOUDBURROW_FIRESTORE_MONGODB_CA_FILE names for clients.
		ns := cfg.Cluster.Namespace
		cert, key, err := firestoreadmin.MongoServerTLS(cfg.InstanceDir(), time.Now(),
			"firestore", "firestore."+ns, "firestore."+ns+".svc", "firestore."+ns+".svc.cluster.local")
		if err != nil {
			return fmt.Errorf("Firestore with MongoDB compatibility's TLS certificate: %w", err)
		}
		comps.SetFirestoreMongoTLS(string(cert), string(key))
	}
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
	// BigQuery's signed URLs name the address clients reach Cloud Storage
	// at (#1786): the storage tunnel's, which New has already chosen when
	// --port-storage is 0. The configured port was 0 there, and every
	// signed URL named http://127.0.0.1:0 (main run 44357900271), as the
	// storage backend's advertised host once did (#269).
	if f := forwarderFor(forwarders, "storage"); f != nil && f.HostAddr() != "" {
		comps.SetStoragePublic("http://" + f.HostAddr())
	}
	// A database's tunnel is up once its user logs in through it (#1262).
	for _, s := range []config.Service{config.ServiceCloudSQLMySQL, config.ServiceCloudSQL} {
		if f := forwarderFor(forwarders, string(s)); f != nil {
			f.SetHandshake(databaseHandshake(s, mysqlCreds))
		}
	}
	for _, f := range forwarders {
		// What each tunnel did, in up.log and so in a diagnose bundle (#526).
		f.Logf = func(format string, args ...any) {
			fmt.Fprintf(stderr, "cloudburrow: "+format+"\n", args...)
		}
	}

	// Cloud Tasks has no upstream backend, so it runs in this process rather
	// than as a cluster workload.
	tasksSvc := newTasksService(cfg)
	tasksSvc.useTokens(creds)
	tasksSvc.register(coord)

	// The Cloud Run adapter is ours even though the execution engine is the
	// cluster's, so it also runs in this process.
	runSvc := newRunService(cfg)
	if runSvc != nil { // nil without Cloud Run
		// Each Cloud Run workload is told its own service account (#1388).
		metaSrv.WithAccounts(runSvc.metadataAccount)
		runSvc.storage = func() *netfwd.Forwarder { return forwarderFor(forwarders, "storage") }
		runSvc.tokens = creds
		// Functions' event triggers (#1414) deliver through Pub/Sub.
		if serviceEnabled(cfg, config.ServicePubSub) {
			runSvc.events = &eventsConfig{
				transport: eventTransport{
					pubsub:  func() *netfwd.Forwarder { return forwarderFor(forwarders, "pubsub") },
					storage: func() *netfwd.Forwarder { return forwarderFor(forwarders, "storage") },
					client:  &http.Client{Timeout: time.Minute},
				},
				tokens: creds,
			}
		}
	}
	runSvc.register(coord)

	// Secret Manager has no upstream emulator either, so it too is served
	// from this process.
	secretsSvc := newSecretsService(cfg)
	if secretsSvc != nil {
		secretsSvc.pubsub = func() *netfwd.Forwarder { return forwarderFor(forwarders, "pubsub") }
	}
	secretsSvc.register(coord)
	kmsSvc := newKMSService(cfg)
	kmsSvc.register(coord)
	if kmsSvc != nil {
		// The Inventory API reads key usage from the products (#2053).
		kmsSvc.inventorySources = &kmsInventoryWiring{cfg: cfg, secrets: secretsSvc,
			storage:  func() *netfwd.Forwarder { return forwarderFor(forwarders, "storage") },
			bigquery: func() *netfwd.Forwarder { return forwarderFor(forwarders, "bigquery") },
			pubsub:   func() *netfwd.Forwarder { return forwarderFor(forwarders, "pubsub") }}
	}
	if secretsSvc != nil && kmsSvc != nil {
		// A secret's customer-managed keys (#1453) are this instance's Cloud KMS.
		secretsSvc.kms = kmsSvc.API
	}
	// liveEndpoints, assigned below once every service exists, is every
	// bound address; boundAddrs reads it after the services start, so the
	// dispatchers below find an OS-assigned port (--port-storage 0) too.
	var liveEndpoints func() map[string]string
	boundAddrs := func() map[string]string {
		if liveEndpoints == nil {
			return nil
		}
		return liveEndpoints()
	}
	// Every outbound dispatcher sends a googleapis.com URL to this
	// instance's own endpoint of the API, and refuses any other Google host
	// (internal/egress, #2015).
	egress.SetLocal(localAPIs(cfg, boundAddrs))
	schedulerSvc := newSchedulerService(cfg, func() *netfwd.Forwarder { return forwarderFor(forwarders, "pubsub") })
	schedulerSvc.useTokens(creds)
	if schedulerSvc != nil {
		schedulerSvc.live = boundAddrs
	}
	schedulerSvc.register(coord)
	// Certificate Authority Service (#1834), in process.
	privateCASvc := newPrivateCAService(cfg)
	if privateCASvc != nil {
		// A CA key held in Cloud KMS and a CMEK pool use this instance's
		// Cloud KMS, and a CA's gcs_bucket its Cloud Storage, in process.
		privateCASvc.kms = kmsSvc
		privateCASvc.storage = func() *netfwd.Forwarder { return forwarderFor(forwarders, "storage") }
	}
	privateCASvc.register(coord)
	// BigQuery sharing (Analytics Hub, #1960), in process; its linked
	// datasets are made on the instance's BigQuery.
	analyticsHub := newAnalyticsHubService(cfg)
	if analyticsHub != nil {
		analyticsHub.bigquery = func() *netfwd.Forwarder { return forwarderFor(forwarders, "bigquery") }
	}
	analyticsHub.register(coord)
	// Workflows and Workflow Executions (#1986), in process; executions
	// call this instance's own endpoints, with tokens minted locally.
	workflowsSvc := newWorkflowsService(cfg)
	if workflowsSvc != nil {
		workflowsSvc.tokens = creds
		workflowsSvc.kms = kmsSvc
		workflowsSvc.live = boundAddrs
		// Eventarc's workflow destination (#1986).
		if runSvc != nil && runSvc.events != nil {
			runSvc.events.workflows = workflowsSvc
		}
	}
	workflowsSvc.register(coord)
	// Parameter Manager (#2054), in process; its versions' secret
	// references are read through Secret Manager.
	parameterManagerSvc := newParameterManagerService(cfg)
	if parameterManagerSvc != nil {
		parameterManagerSvc.secrets = secretsSvc
	}
	// The request quotas (#2063): Google's documented per-minute limits,
	// those the configuration sets, and the admin API's overrides.
	requestQuotas, err := newRequestQuotas(cfg)
	if err != nil {
		return err
	}
	if parameterManagerSvc != nil {
		parameterManagerSvc.quotas = requestQuotas
	}
	parameterManagerSvc.register(coord)
	// Dataform (#2105), in process; its invocations run on BigQuery, its
	// git remotes are Secure Source Manager's (Cloud Run's port) and its
	// access tokens are Secret Manager's.
	dataformSvc := newDataformService(cfg)
	if dataformSvc != nil {
		dataformSvc.secrets = secretsSvc
		dataformSvc.bigquery = func() *netfwd.Forwarder { return forwarderFor(forwarders, "bigquery") }
		if runSvc != nil {
			dataformSvc.gitBase = func() string {
				if a := runSvc.Addr(); a != "" {
					return hostURL(a)
				}
				return ""
			}
			dataformSvc.runAddr = runSvc.Addr
		}
	}
	dataformSvc.register(coord)
	// The Kubernetes Engine API (#1913), in process; its clusters are
	// kind clusters of their own.
	gkeSvc := newGKEService(cfg)
	// The Compute Engine API's VPC networking (#1936), in process, before
	// the services that check their networks against it.
	computeSvc := newComputeService(cfg)
	if computeSvc != nil {
		computeSvc.gke = gkeSvc
		if gkeSvc != nil {
			gkeSvc.networks = computeSvc.gkeNetworks
		}
	}
	computeSvc.register(coord)
	gkeSvc.register(coord)
	// App Hub (#2110), in process; it discovers the supported resources
	// of every served product (apphubdiscover.go).
	appHubSvc := newAppHubService(cfg)
	if appHubSvc != nil {
		appHubSvc.run = runSvc
		appHubSvc.gke = gkeSvc
		appHubSvc.live = boundAddrs
		appHubSvc.bigquery = func() *netfwd.Forwarder { return forwarderFor(forwarders, "bigquery") }
	}
	appHubSvc.register(coord)
	if gkeSvc != nil && runSvc != nil && runSvc.events != nil {
		// Eventarc's GKE destinations (#1877) reach its clusters' services.
		runSvc.events.gke = gkeSvc.clusterAccess
	}
	// The BigQuery Data Transfer Service (#1948), in process; its runs
	// execute on the instance's BigQuery and notify its Pub/Sub.
	dataTransfer := newDataTransferService(cfg)
	if dataTransfer != nil {
		dataTransfer.bigquery = func() *netfwd.Forwarder { return forwarderFor(forwarders, "bigquery") }
		dataTransfer.pubsub = func() *netfwd.Forwarder { return forwarderFor(forwarders, "pubsub") }
		// bq's Data Transfer calls arrive at BigQuery's address.
		if f := forwarderFor(forwarders, "bigquery"); f != nil {
			f.Local = dataTransfer.bigQueryLocal
		}
	}
	dataTransfer.register(coord)
	// The Memorystore admin API (#1302), over the Valkey server's tunnel.
	memorystoreAdminSvc := newMemorystoreAdminService(cfg, func() *netfwd.Forwarder { return forwarderFor(forwarders, "memorystore") })
	if memorystoreAdminSvc != nil {
		memorystoreAdminSvc.storage = func() *netfwd.Forwarder { return forwarderFor(forwarders, "storage") }
		memorystoreAdminSvc.kms = kmsSvc
		// A Redis cluster's customer-managed CA (#1907): this instance's
		// Certificate Authority Service, in process.
		memorystoreAdminSvc.privateca = privateCASvc
		// Networks and reserved ranges are checked against Compute
		// Engine's when compute is served (#1936).
		if computeSvc != nil {
			memorystoreAdminSvc.networks = computeSvc.memorystoreNetworks
		}
		// A Redis before 6.0 with in-transit encryption is served TLS by
		// a front from the storage image (#1768).
		memorystoreAdminSvc.storageImage = lazyStorageImage(newStorageImageComponent(cfg.KubeconfigPath(), cfg.ClusterName(), cfg.Name, comps, stdout), comps)
		// IAM auth (#1614) checks a token against the tokens these
		// credentials minted and the project's IAM policy.
		memorystoreAdminSvc.creds = creds
	}
	memorystoreAdminSvc.register(coord)
	loggingSvc := newLoggingService(cfg)
	// Log entries' App Hub metadata (#2096), in entries.list and the Logs
	// Explorer.
	appHubSvc.useLogging(loggingSvc)
	loggingSvc.register(coord)
	// Cloud Trace (#2118), in process: the Cloud Trace API v1 and v2 and
	// OTLP trace ingestion on one port,
	// after the Cloud Logging whose _Trace bucket takes its spans (#2117).
	traceSvc := newTraceService(cfg)
	if traceSvc != nil {
		traceSvc.logging = loggingSvc
	}
	traceSvc.register(coord)
	// Cloud Monitoring (#1939), in process: log-based metrics' values come
	// from this instance's Cloud Logging, request metrics from its counters.
	monitoringSvc := newMonitoringService(cfg)
	if monitoringSvc != nil {
		monitoringSvc.logging = loggingSvc
		// Cloud Tasks' queue metrics come from its dispatch (#2064).
		monitoringSvc.tasks = tasksSvc
		// Pub/Sub notification channels publish to this instance's Pub/Sub
		// (#1964).
		if serviceEnabled(cfg, config.ServicePubSub) {
			monitoringSvc.pubsub = func() *netfwd.Forwarder { return forwarderFor(forwarders, "pubsub") }
		}
		// Firestore's document metrics, from its front's counts (#2048).
		if serviceEnabled(cfg, config.ServiceFirestore) {
			monitoringSvc.firestore = func() string { return forwardedAddr(forwarders, "firestore") }
		}
	}
	monitoringSvc.register(coord)
	// Kubernetes Engine's container logs and system metrics, shipped to
	// this instance's Cloud Logging and Cloud Monitoring while both sides
	// are served, as GKE ships them (#2100).
	if gkeSvc != nil {
		newGKELogFeeder(gkeDir(cfg), gkeSvc, loggingSvc).register(coord)
		newGKEMetricsSampler(gkeDir(cfg), gkeSvc, monitoringSvc).register(coord)
	}
	if loggingSvc != nil {
		// CopyLogEntries writes to the instance's Cloud Storage (#1394).
		loggingSvc.storage = func() *netfwd.Forwarder { return forwarderFor(forwarders, "storage") }
		// Sinks deliver to Pub/Sub and BigQuery too (#1404).
		loggingSvc.pubsub = func() *netfwd.Forwarder { return forwarderFor(forwarders, "pubsub") }
		loggingSvc.bigquery = func() *netfwd.Forwarder { return forwarderFor(forwarders, "bigquery") }
		// A queue's sampled dispatch attempts go to the instance's Cloud
		// Logging through its own WriteLogEntries (#1256), so exclusions
		// apply to them as to any write.
		api := loggingSvc.api
		// Artifact Registry's cleanup runs write their audit entries the
		// same way (#1897).
		runadapter.UseCleanupLogging(func(ctx context.Context, e *loggingpb.LogEntry) error {
			_, err := api.WriteLogEntries(ctx, &loggingpb.WriteLogEntriesRequest{Entries: []*loggingpb.LogEntry{e}})
			return err
		})
		// Eventarc Advanced's platform logs (#1947).
		runadapter.UseEventarcLogging(func(ctx context.Context, e *loggingpb.LogEntry) error {
			_, err := api.WriteLogEntries(ctx, &loggingpb.WriteLogEntriesRequest{Entries: []*loggingpb.LogEntry{e}})
			return err
		})
		tasksSvc.useLogging(func(ctx context.Context, e *loggingpb.LogEntry) error {
			_, err := api.WriteLogEntries(ctx, &loggingpb.WriteLogEntriesRequest{Entries: []*loggingpb.LogEntry{e}})
			return err
		})
		// Cloud Scheduler's execution logs (#1849).
		schedulerSvc.useLogging(func(ctx context.Context, e *loggingpb.LogEntry) error {
			_, err := api.WriteLogEntries(ctx, &loggingpb.WriteLogEntriesRequest{Entries: []*loggingpb.LogEntry{e}})
			return err
		})
	}
	if secretsSvc != nil {
		runSvc.useSecrets(lazySecretResolver{svc: secretsSvc})
	}
	// A Cloud Build build's secrets (#1407).
	runSvc.useBuildSecrets(secretsSvc, kmsSvc)

	// Admin routes must be mounted before the control server starts: it builds
	// its mux at Start, so anything added afterwards is never routed.
	// The resetter and seeder resolve their store lazily, so registering them
	// before the services start is safe.
	recorder := admin.NewRecorder(1000, nil)
	// The registry is opened below, after the routes are mounted, so the reset
	// reads it through this at reset time.
	var registered func() []string = func() []string { return nil }
	var projectRegistry *resourcemanager.Registry
	if appHubSvc != nil {
		appHubSvc.registry = func() *resourcemanager.Registry { return projectRegistry }
	}
	if dataTransfer != nil {
		dataTransfer.registry = func() *resourcemanager.Registry { return projectRegistry }
	}
	if memorystoreAdminSvc != nil {
		memorystoreAdminSvc.registry = func() *resourcemanager.Registry { return projectRegistry }
	}
	if secretsSvc != nil {
		// Secret.tags are Resource Manager's tag bindings (#1961).
		secretsSvc.registry = func() *resourcemanager.Registry { return projectRegistry }
		if parameterManagerSvc != nil {
			parameterManagerSvc.registry = func() *resourcemanager.Registry { return projectRegistry }
		}
	}
	if serviceEnabled(cfg, config.ServiceFirestore) {
		// Firestore with MongoDB compatibility's token sign-in (#1738)
		// checks the tokens these credentials minted and the project's
		// IAM policy, as Memorystore's IAM auth does.
		fmIAM := &firestoreMongoIAM{creds: creds, client: &http.Client{Timeout: 5 * time.Second},
			registry: func() *resourcemanager.Registry { return projectRegistry },
			addr:     func() string { return forwardedAddr(forwarders, "firestore") }}
		fmIAM.start(ctx)
	}
	if serviceEnabled(cfg, config.ServiceBigQuery) {
		// BigQuery's per-project IAM of a caller with a minted token
		// (#1767, bigqueryfront/iamcheck.go).
		bqIAM := &bigqueryIAM{creds: creds, client: &http.Client{Timeout: 5 * time.Second},
			registry: func() *resourcemanager.Registry { return projectRegistry },
			addr:     func() string { return forwardedAddr(forwarders, "bigquery") }}
		bqIAM.start(ctx)
	}
	// Service Usage is built after the registry, so its reset reads it
	// through this too (#1537).
	var serviceUsageRef *serviceusage.Server
	// And the API Keys API (#1915).
	var apiKeysRef *apikeys.Server
	// And the Cloud Quotas and Site Verification APIs (#2030).
	var cloudQuotasRef *cloudquotas.Server
	var siteVerificationRef *siteverification.Server
	// And Cloud Billing and the Budget API (#2109).
	var billingRef *billing.Server
	var authPlatformRef *authplatform.Store
	// And the Cloud Storage companion APIs (#1957).
	var storageJobsRef *storageJobs
	// And the Firebase Rules API (#2048).
	var firebaseRulesRef *firebaserules.Server
	// And the Cloud SQL Admin API and Backup and DR (#1541, #1542).
	var sqlAdminRef *sqladmin.Server
	var backupDRRef *backupdr.Server
	// The admin token (#553): minted now, required on every /admin route,
	// written beside the runtime file once the control server is up.
	adminToken, err := newAdminToken()
	if err != nil {
		return err
	}
	// Every guarded listener serves browser requests from loopback origins,
	// its own, and these (#1586); storage's in-cluster server takes the same
	// list as its own flag (#677).
	if err := hostguard.AllowOrigins(cfg.Storage.CORSAllowOrigins); err != nil {
		return fmt.Errorf("--cors-allow-origin: %w", err)
	}
	adminAPI := mountAdmin(control, recorder, cfg, adminDeps{
		tasks: tasksSvc, secrets: secretsSvc, forwarders: forwarders,
		kms:              kmsSvc,
		mysql:            mysqlCreds,
		scheduler:        schedulerSvc,
		privateCA:        privateCASvc,
		analyticsHub:     analyticsHub,
		workflows:        workflowsSvc,
		parameterManager: parameterManagerSvc,
		dataform:         dataformSvc,
		appHub:           appHubSvc,
		trace:            traceSvc,
		gke:              gkeSvc,
		monitoring:       monitoringSvc,
		compute:          computeSvc,
		dataTransfer:     dataTransfer,
		logging:          loggingSvc,
		run:              runSvc,
		memorystore:      memorystoreAdminSvc,
		projects:         func() []string { return registered() },
		registry:         func() *resourcemanager.Registry { return projectRegistry },
		serviceUsage:     func() *serviceusage.Server { return serviceUsageRef },
		apiKeys:          func() *apikeys.Server { return apiKeysRef },
		cloudQuotas:      func() *cloudquotas.Server { return cloudQuotasRef },
		siteVerify:       func() *siteverification.Server { return siteVerificationRef },
		billing:          func() *billing.Server { return billingRef },
		authPlatform:     func() *authplatform.Store { return authPlatformRef },
		storageJobs:      func() *storageJobs { return storageJobsRef },
		firebaseRules:    func() *firebaserules.Server { return firebaseRulesRef },
		sqlAdmin:         func() *sqladmin.Server { return sqlAdminRef },
		backupDR:         func() *backupdr.Server { return backupDRRef },
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
	// The request history: one-minute deltas of the counters per project,
	// which Cloud Monitoring serves as serviceruntime.googleapis.com/api/*
	// and APIs & Services charts over a time range (#1866).
	requestHistory := monitoring.NewRequestBuiltin(monitoringCounters{reg: requestMetrics}, cfg.DefaultProject(), time.Now)
	coord.Register(&requestSampler{history: requestHistory})
	if monitoringSvc != nil {
		monitoringSvc.counters = requestMetrics
		monitoringSvc.reqs = requestHistory
	}
	// Fault injection (#306) on the services CloudBurrow serves itself. Each
	// faults.Interceptor below is what makes its service's rules accepted
	// (#600), so the fault-injectable set is exactly the wiring. The builtin
	// Cloud Storage server runs in the cluster, where a rule cannot reach it,
	// so storage is not interposed and its rules stay refused.
	adminAPI.SetQuotas(requestQuotas)
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
		fmt.Fprintln(stderr, "tracing: exporting spans for tasks, run, secretmanager, kms, scheduler, logging, resourcemanager, pubsub, bigtable, spanner, firestore and datastore to the configured OTLP endpoint")
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
	if schedulerSvc != nil {
		schedulerSvc.tracing = tracing
	}
	if privateCASvc != nil {
		privateCASvc.tracing = tracing
	}
	if workflowsSvc != nil {
		workflowsSvc.tracing = tracing
		workflowsSvc.calls = callEvents(recorder, requestMetrics, "workflows")
		workflowsSvc.interpose = append(workflowsSvc.interpose, grpctransport.LogInterceptor(logger, "workflows"), faults.Interceptor("workflows"))
		workflowsSvc.requests = requestEvents(recorder, requestMetrics, "workflows")
	}
	if appHubSvc != nil {
		appHubSvc.tracing = tracing
		appHubSvc.calls = callEvents(recorder, requestMetrics, "apphub")
		appHubSvc.interpose = append(appHubSvc.interpose, grpctransport.LogInterceptor(logger, "apphub"), faults.Interceptor("apphub"))
		appHubSvc.requests = requestEvents(recorder, requestMetrics, "apphub")
	}
	if parameterManagerSvc != nil {
		parameterManagerSvc.tracing = tracing
		parameterManagerSvc.calls = callEvents(recorder, requestMetrics, "parametermanager")
		parameterManagerSvc.interpose = append(parameterManagerSvc.interpose, grpctransport.LogInterceptor(logger, "parametermanager"), faults.Interceptor("parametermanager"))
		parameterManagerSvc.requests = requestEvents(recorder, requestMetrics, "parametermanager")
	}
	if dataformSvc != nil {
		dataformSvc.tracing = tracing
		dataformSvc.calls = callEvents(recorder, requestMetrics, "dataform")
		dataformSvc.interpose = append(dataformSvc.interpose, grpctransport.LogInterceptor(logger, "dataform"), faults.Interceptor("dataform"))
		dataformSvc.requests = requestEvents(recorder, requestMetrics, "dataform")
	}
	if traceSvc != nil {
		traceSvc.calls = callEvents(recorder, requestMetrics, "trace")
		traceSvc.interpose = append(traceSvc.interpose, grpctransport.LogInterceptor(logger, "trace"), faults.Interceptor("trace"))
		traceSvc.requests = requestEvents(recorder, requestMetrics, "trace")
	}
	if dataTransfer != nil {
		dataTransfer.tracing = tracing
		dataTransfer.calls = callEvents(recorder, requestMetrics, "datatransfer")
		dataTransfer.interpose = append(dataTransfer.interpose, grpctransport.LogInterceptor(logger, "datatransfer"), faults.Interceptor("datatransfer"))
		dataTransfer.requests = requestEvents(recorder, requestMetrics, "datatransfer")
	}
	if gkeSvc != nil {
		gkeSvc.tracing = tracing
		gkeSvc.calls = callEvents(recorder, requestMetrics, "gke")
		gkeSvc.interpose = append(gkeSvc.interpose, grpctransport.LogInterceptor(logger, "gke"), faults.Interceptor("gke"))
		gkeSvc.requests = requestEvents(recorder, requestMetrics, "gke")
	}
	if monitoringSvc != nil {
		monitoringSvc.tracing = tracing
	}
	if computeSvc != nil {
		computeSvc.requests = requestEvents(recorder, requestMetrics, "compute")
	}
	if loggingSvc != nil {
		loggingSvc.tracing = tracing
	}
	if tasksSvc != nil {
		tasksSvc.calls = callEvents(recorder, requestMetrics, "tasks")
		tasksSvc.interpose = append(tasksSvc.interpose, grpctransport.LogInterceptor(logger, "tasks"), faults.Interceptor("tasks"))
		tasksSvc.requests = requestEvents(recorder, requestMetrics, "tasks")
	}
	if runSvc != nil {
		runSvc.calls = callEvents(recorder, requestMetrics, "run")
		runSvc.interpose = append(runSvc.interpose, grpctransport.LogInterceptor(logger, "run"), faults.Interceptor("run"))
		runSvc.requests = requestEvents(recorder, requestMetrics, "run")
		if runSvc.events != nil {
			// Eventarc's Cloud Audit Logs events (#1877), from the calls
			// the services' fronts record.
			go feedAuditLogs(recorder, runSvc.functions.Load)
			// The direct events of the Google sources served (#1997):
			// Memorystore for Redis, Cloud Run functions and Eventarc.
			if memorystoreAdminSvc != nil {
				runadapter.SetRedisInstanceLister(memorystoreAdminSvc.listInstances)
			}
			go watchEventarcResources(runSvc.functions.Load)
			if slices.Contains(cfg.EnabledServices(), config.ServiceFirestore) {
				// Firestore events (#1877), from Firestore's Listen.
				go feedFirestoreEvents(func() *netfwd.Forwarder { return forwarderFor(forwarders, "firestore") },
					runSvc.functions.Load, "us-central1")
			}
			if slices.Contains(cfg.EnabledServices(), config.ServiceDatastore) {
				// Firestore in Datastore mode events (#1877), by polling.
				go feedDatastoreEvents(func() *netfwd.Forwarder { return forwarderFor(forwarders, string(config.ServiceDatastore)) },
					runSvc.functions.Load, "us-central1")
			}
		}
	}
	if schedulerSvc != nil {
		schedulerSvc.calls = callEvents(recorder, requestMetrics, "scheduler")
		schedulerSvc.interpose = append(schedulerSvc.interpose, grpctransport.LogInterceptor(logger, "scheduler"), faults.Interceptor("scheduler"))
		schedulerSvc.requests = requestEvents(recorder, requestMetrics, "scheduler")
	}
	if monitoringSvc != nil {
		monitoringSvc.calls = callEvents(recorder, requestMetrics, "monitoring")
		monitoringSvc.interpose = append(monitoringSvc.interpose, grpctransport.LogInterceptor(logger, "monitoring"), faults.Interceptor("monitoring"))
		monitoringSvc.requests = requestEvents(recorder, requestMetrics, "monitoring")
	}
	if analyticsHub != nil {
		analyticsHub.tracing = tracing
		analyticsHub.calls = callEvents(recorder, requestMetrics, "analyticshub")
		analyticsHub.interpose = append(analyticsHub.interpose, grpctransport.LogInterceptor(logger, "analyticshub"), faults.Interceptor("analyticshub"))
		analyticsHub.requests = requestEvents(recorder, requestMetrics, "analyticshub")
		analyticsHub.orgOf = func(project string) string {
			return organizationOf(registrySource{reg: func() *resourcemanager.Registry { return projectRegistry }}, project)
		}
	}
	if privateCASvc != nil {
		privateCASvc.calls = callEvents(recorder, requestMetrics, "privateca")
		privateCASvc.interpose = append(privateCASvc.interpose, grpctransport.LogInterceptor(logger, "privateca"), faults.Interceptor("privateca"))
		privateCASvc.requests = requestEvents(recorder, requestMetrics, "privateca")
	}
	if memorystoreAdminSvc != nil {
		memorystoreAdminSvc.calls = callEvents(recorder, requestMetrics, "memorystore")
		memorystoreAdminSvc.interpose = append(memorystoreAdminSvc.interpose, grpctransport.LogInterceptor(logger, "memorystore"), faults.Interceptor("memorystore"))
		memorystoreAdminSvc.requests = requestEvents(recorder, requestMetrics, "memorystore")
		memorystoreAdminSvc.events = recorder.Record
	}
	if loggingSvc != nil {
		loggingSvc.calls = callEvents(recorder, requestMetrics, "logging")
		loggingSvc.interpose = append(loggingSvc.interpose, grpctransport.LogInterceptor(logger, "logging"), faults.Interceptor("logging"))
		loggingSvc.requests = requestEvents(recorder, requestMetrics, "logging")
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
	// IAM enforcement (#1879), innermost, so the request log and fault
	// injection see a denied call as the client does. Off by default: the
	// interceptors pass every call through until --iam-enforcement=on.
	iamEnforcer := newIAMEnforcer(cfg, creds, func() *resourcemanager.Registry { return projectRegistry })
	// The Docker registry's token auth, checked by the same evaluator (#2111).
	useRegistryAuth(iamEnforcer, creds)
	defer runadapter.UseRegistryAuth(nil)
	useIAMEnforcement(iamEnforcer, secretsSvc, kmsSvc, tasksSvc, analyticsHub, schedulerSvc, workflowsSvc,
		iamEnforced{logging: loggingSvc, monitoring: monitoringSvc, memorystore: memorystoreAdminSvc, run: runSvc})
	iamEnforced7{privateCA: privateCASvc, gke: gkeSvc, compute: computeSvc, run: runSvc}.use(iamEnforcer)
	iamEnforced8{compute: computeSvc}.use(iamEnforcer)
	iamEnforced13{memorystore: memorystoreAdminSvc, gke: gkeSvc}.use(iamEnforcer)
	useIAMEnforcement12Source(iamEnforcer, runSvc)
	useIAMEnforcement12DataTransfer(iamEnforcer, dataTransfer)
	// Parameter Manager (#2054): its callers' permissions, its parameters'
	// principals' access to the secrets they reference, and its keys.
	if parameterManagerSvc != nil {
		parameterManagerSvc.enforcer = iamEnforcer
		parameterManagerSvc.keys = cmekKeys(kmsSvc, iamEnforcer)
		parameterManagerSvc.interpose = append(parameterManagerSvc.interpose, iamEnforcer.Interceptor(iameval.ParameterManager))
		parameterManagerSvc.wrapJSON = jsonEnforcer(iamEnforcer, iameval.ParameterManager, parametermanager.Mixins())
	}
	// Dataform repositories' customer-managed keys (#2105).
	if dataformSvc != nil {
		dataformSvc.keys = cmekKeys(kmsSvc, iamEnforcer)
	}
	// App Hub (#2110): each method's documented permissions, and the
	// policies of its applications.
	useIAMEnforcementAppHub(iamEnforcer, appHubSvc)
	// Log buckets' customer-managed keys (#1950).
	if loggingSvc != nil {
		keys := cmekKeys(kmsSvc, iamEnforcer)
		loggingSvc.keys = &keys
	}

	// The runtime file straight after the control server, so it names an
	// address already listening and `wait` can follow startup from the start.
	runtime := &runtimeFile{cfg: cfg, control: control, detached: os.Getenv(detachedEnv) != "", token: adminToken}
	// The cache on either side of the cluster: the node image must be in
	// Docker before kind creates the node, and the backend and Knative
	// images in the node before anything runs them.
	coord.Register(control, runtime, metaSrv, offlineHostComponent{cached}, clusterComp, offlineNodesComponent{cached})
	// Knative's install, the longest step of a cold start, begins as soon
	// as the cluster (and, offline, its images) is there, and runs while
	// the images load, the backends roll out and their tunnels bind; the
	// knative component below waits for it (#1588). Nothing without Cloud
	// Run.
	coord.Register(comps.KnativeStart())
	// The images are built and loaded at the same time (#1588): neither
	// needs the other, only the components after them need both.
	var imageComps []lifecycle.Component
	if needsStorageImage(cfg) {
		// Between the cluster and the components: the image must be in the
		// cluster before its Deployment is (#514). BigQuery's front (#902)
		// and Pub/Sub's front (#873) run from it too.
		imageComps = append(imageComps, newStorageImageComponent(cfg.KubeconfigPath(), cfg.ClusterName(), cfg.Name, comps, stdout))
	}
	if serviceEnabled(cfg, config.ServiceBigQuery) {
		// The emulator's image, built from the patched build this CLI
		// embeds (#1061), in the cluster before its Deployment.
		imageComps = append(imageComps, newBigQueryImageComponent(cfg.KubeconfigPath(), cfg.ClusterName(), cfg.Name, comps, stdout))
	}
	coord.RegisterConcurrent(imageComps...)
	// `kind create` returns before its node is Ready (#1588), so the
	// images above and Knative's manifests overlap the node's start; the
	// backends are scheduled only once it is Ready, as with --wait.
	coord.Register(clusterComp.NodesReady())
	coord.Register(comps)
	// Once the cluster answers: Cloud KMS and Secret Manager drop what --mode
	// says must not survive (#481, #483).
	kmsSvc.registerForget(coord)
	secretsSvc.registerForget(coord)
	// Every tunnel at once (#1588): each binds its own pod and proves it
	// carries a connection, which no other tunnel needs.
	tunnels := make([]lifecycle.Component, 0, len(forwarders))
	for _, f := range forwarders {
		tunnels = append(tunnels, f)
	}
	coord.RegisterConcurrent(tunnels...)
	// Ready only once Knative is, before anything that could deploy to it.
	coord.Register(comps.KnativeReady())
	var storageRequests *storageRequestScraper
	if f := forwarderFor(forwarders, "storage"); f != nil {
		scraper := newStorageEventScraper(f, recorder, requestMetrics)
		// Cloud Storage's audit log entries and request activity (#1979):
		// the registry, Logging and the companions are read at each batch,
		// as they are set up below.
		scraper.requests = &storageRequestScraper{tunnel: f, audit: &storageAudit{
			registry: func() *resourcemanager.Registry { return projectRegistry },
			write: func(ctx context.Context, entries []*loggingpb.LogEntry) error {
				if loggingSvc == nil {
					return nil
				}
				_, err := loggingSvc.api.WriteLogEntries(ctx, &loggingpb.WriteLogEntriesRequest{Entries: entries})
				return err
			},
			jobs: func() *storagejobs.Runtime {
				if storageJobsRef == nil {
					return nil
				}
				return storageJobsRef.rt
			},
		}}
		storageRequests = scraper.requests
		coord.Register(scraper)
		coord.Register(newStorageChannelRelay(f, stdout))
	}
	for _, svc := range []config.Service{config.ServicePubSub, config.ServiceBigtable,
		config.ServiceSpanner, config.ServiceFirestore, config.ServiceDatastore} {
		// Every call the Pub/Sub (#1266) and Bigtable (#1267) fronts and
		// the Spanner, Firestore and Datastore call fronts (#1288) served,
		// the host's and a pod's alike.
		if f := forwarderFor(forwarders, string(svc)); f != nil {
			coord.Register(newFrontCallScraper(string(svc), f, recorder, requestMetrics, tracing))
		}
	}

	// The project registry, opened before the console because the console
	// lists it and preselects the instance's own project from it.
	projects, releaseProjects, err := openProjects(cfg)
	if err == nil && projects != nil {
		projectRegistry = projects
		wirePurge(adminAPI, projects)
		wireMemberRewrites(projects, tasksSvc, secretsSvc, kmsSvc, loggingSvc, runSvc, forwarders)
		// Service accounts (#1747): a key file names this instance's
		// token endpoint, and IAM Credentials and the token endpoint
		// resolve accounts and keys in the registry.
		projects.SetServiceAccountTokenURI(tokenURI(cfg))
		creds.SetServiceAccounts(serviceAccountDirectory{reg: projects})
		// Workload Identity Federation (#1941): the STS exchanges
		// against the registry's pools and providers.
		creds.SetFederation(projects)
		projects.SetSignAuthorizer(signAuthorizer(creds, serviceAccountDirectory{reg: projects}))
		projects.SetCallerResolver(func(bearer string) (string, bool) { return creds.TokenAccount(bearer, time.Now()) })
		wireProjectNumbers(ctx, projects, map[string]func() *netfwd.Forwarder{
			"storage":  func() *netfwd.Forwarder { return forwarderFor(forwarders, "storage") },
			"bigquery": func() *netfwd.Forwarder { return forwarderFor(forwarders, "bigquery") }, // #1551
		})
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
	rmSrv.ServerOptions(tracing.ServerOptions()...)
	rmSrv.Interpose(grpctransport.LogInterceptor(logger, "resourcemanager"))
	rmSrv.Interpose(faults.Interceptor("resourcemanager"))
	// Fault rules (#2113) reach the servers in the cluster, whose fronts
	// ask the host for them on this port (internal/faultgate), so their
	// services' rules are accepted.
	rmSrv.HandleJSON(func(r *http.Request) bool { return faultgate.IsPath(r.URL.Path) }, faultgate.Handler(faults))
	for _, s := range cfg.EnabledServices() {
		switch s {
		case config.ServicePubSub, config.ServiceBigtable, config.ServiceSpanner, config.ServiceFirestore,
			config.ServiceDatastore, config.ServiceStorage:
			faults.InterposeRemote(string(s))
		}
	}
	// Policy Troubleshooter and Policy Analyzer (#1879) on the same port,
	// over the enforcement's evaluator.
	// Cloud Asset's analysis (#2112) writes analyzeIamPolicyLongrunning's
	// results to the instance's Cloud Storage and BigQuery.
	assetOut := &assetOutputs{
		bigquery:  func() *netfwd.Forwarder { return forwarderFor(forwarders, "bigquery") },
		forwarded: func(name string) string { return forwardedAddr(forwarders, name) },
		storage: func() string {
			if !serviceEnabled(cfg, config.ServiceStorage) {
				return ""
			}
			return storageSourceFetcher{cfg: cfg, addr: func() string {
				if f := forwarderFor(forwarders, "storage"); f != nil {
					return f.HostAddr()
				}
				return ""
			}}.base()
		},
	}
	rmSrv.HandleJSON(isIAMAnalysisPath, iamAnalysis{e: iamEnforcer, reg: func() *resourcemanager.Registry { return projects }, out: assetOut})
	// The in-cluster servers' checks (Cloud Storage, Pub/Sub's front,
	// internal/iamcheck), through cloudburrow-host, so every decision is
	// this evaluator's.
	rmSrv.HandleJSON(func(r *http.Request) bool { return r.URL.Path == iamcheck.Path }, http.HandlerFunc(iamEnforcer.ServeCheck))
	// Organization policies (#1890) are enforced whatever
	// --iam-enforcement: the in-process services ask the registry, the
	// in-cluster servers (Cloud Storage, Pub/Sub's and BigQuery's fronts)
	// ask it here through cloudburrow-host (internal/orgpolicy).
	orgPolicySrc := resourcemanager.OrgPolicySource{Reg: func() *resourcemanager.Registry { return projects }}
	orgpolicy.Use(orgPolicySrc)
	defer orgpolicy.Use(nil)
	rmSrv.HandleJSON(func(r *http.Request) bool { return r.URL.Path == orgpolicy.Path }, orgpolicy.Handler(orgPolicySrc))
	// Custom constraints (#1891), with their effective policies.
	rmSrv.HandleJSON(func(r *http.Request) bool { return r.URL.Path == orgpolicy.CustomPath }, orgpolicy.CustomHandler(orgPolicySrc))
	// Kourier's gateway asks before routing to a Cloud Run service
	// (run.routes.invoke, runinvoke.go).
	rmSrv.HandleUnguarded(isRunInvokeAuthz, runInvokeAuthz{e: iamEnforcer, creds: creds, run: runSvc})
	// The local-only guard's refusal (#1975): every endpoint setting
	// `cloudburrow terraform` and `gcloud-setup` write for an API this
	// instance does not serve points here, and is refused with a message,
	// whatever Host the tool derived (regional hosts among them).
	rmSrv.HandleUnguarded(unserved.Match, unserved.Handler())
	// The Cloud Quotas API and the Site Verification API (#2030), on the
	// same port, in the registry's store.
	cloudQuotasRef, siteVerificationRef = serveCloudQuotas(rmSrv, projects, func() *serviceusage.Server { return serviceUsageRef }, requestQuotas,
		creds.Email, func(bearer string) (string, bool) { return creds.TokenAccount(bearer, time.Now()) })
	// Cloud Billing and the Budget API (#2109), on the same port.
	billingRef = serveBilling(ctx, rmSrv, projects, func() *serviceusage.Server { return serviceUsageRef }, iamEnforcer,
		func() *netfwd.Forwarder { return forwarderFor(forwarders, "pubsub") })
	// Google Auth Platform's app and OAuth clients (#2030), whose
	// authorization code flow the metadata server's token endpoint serves.
	authPlatformRef = authplatform.NewStore(projects.Backing(), resource.ProjectNumber)
	metaSrv.WithOAuth(&authplatform.Endpoints{Store: authPlatformRef, User: creds.Email})
	// IAM enforcement (#1879) of Resource Manager, IAM, deny and
	// organization policies, on this port.
	useIAMEnforcement11(iamEnforcer, rmSrv, func() *resourcemanager.Registry { return projects })
	useIAMEnforcement12Tags(iamEnforcer, func() string {
		if f := forwarderFor(forwarders, "storage"); f != nil {
			return f.HostAddr()
		}
		return ""
	}, func() *resourcemanager.Registry { return projects })
	useIAMEnforcement12Identity(iamEnforcer, rmSrv, func() *resourcemanager.Registry { return projects })
	useIAMEnforcementWorkforce(iamEnforcer, rmSrv)
	coord.Register(rmSrv)
	// A build's serviceAccount names an IAM account of this registry (#1854).
	runadapter.UseServiceAccounts(func(name string) (string, bool, bool) {
		sa, err := projects.GetServiceAccount(name)
		if err != nil {
			return "", false, false
		}
		return sa.GetEmail(), sa.GetDisabled(), true
	})
	// Its folders and organization are Logging parents (#1395).
	loggingSvc.useHierarchy(projects)
	// And Autokey resolves folder configs through them (#1452).
	kmsSvc.useHierarchy(projects)

	// The Service Usage API (#1352), which APIs each project has enabled,
	// kept in the registry's store so it persists as the projects do.
	serviceUsageSrv := newServiceUsageServer(cfg, projects, projects.Backing())
	if serviceUsageSrv != nil {
		serviceUsageRef = serviceUsageSrv
		// Artifact Registry's scanning reads the Container Scanning API's
		// state in each project (#1912).
		scanningServiceUsage.Store(serviceUsageSrv)
		coord.Register(serviceUsageSrv)
	}
	// The API Keys API (#1915), each project's keys, in the same store.
	apiKeysSrv := newAPIKeysServer(cfg, projects, projects.Backing())
	// IAM enforcement (#1879) of Service Usage and API Keys.
	iamEnforced7{serviceUsage: serviceUsageSrv, apiKeys: apiKeysSrv}.use(iamEnforcer)
	iamEnforced8{serviceUsage: serviceUsageSrv, apiKeys: apiKeysSrv}.use(iamEnforcer)
	if apiKeysSrv != nil {
		apiKeysRef = apiKeysSrv
		coord.Register(apiKeysSrv)
	}

	// The Cloud Storage companion APIs (#1957): batch operations,
	// inventory reports and transfers over the instance's buckets.
	storageJobsSrv := newStorageJobs(cfg, func() *netfwd.Forwarder { return forwarderFor(forwarders, "storage") },
		func() *netfwd.Forwarder { return forwarderFor(forwarders, "pubsub") },
		func() *netfwd.Forwarder { return forwarderFor(forwarders, "bigquery") }, projects)
	if storageJobsSrv != nil {
		storageJobsRef = storageJobsSrv
		// A snapshot counts the requests up to its time (#1979).
		storageJobsSrv.rt.UseActivityFlush(storageRequests.Flush)
		for _, s := range storageJobsSrv.servers() {
			coord.Register(s)
		}
	}

	// The Firebase Rules API (#2048), over the Firestore emulator's own
	// rules evaluation.
	firebaseRulesSrv := newFirebaseRulesServer(cfg, func() *netfwd.Forwarder { return forwarderFor(forwarders, "firestore") })
	if firebaseRulesSrv != nil {
		firebaseRulesRef = firebaseRulesSrv
		coord.Register(firebaseRulesSrv)
	}

	// The Cloud SQL Admin API (#1301), over whichever database servers run.
	sqlAdminSrv := newSQLAdminServer(cfg, forwarders, mysqlCreds, stderr)
	if sqlAdminSrv != nil {
		sqlAdminRef = sqlAdminSrv
		// settings.ipConfiguration.privateNetwork is checked against
		// Compute Engine's networks when compute is served (#1936).
		if computeSvc != nil {
			sqlAdminSrv.SetNetworks(computeSvc.sqlNetworks)
		}
		// executeSql reads a user's password from Secret Manager (#1462).
		if secretsSvc != nil {
			sqlAdminSrv.SetSecretAccessor(buildSecretAccessor{svc: secretsSvc})
		}
		// diskEncryptionConfiguration (#1538) wraps an instance's data key
		// with this instance's Cloud KMS, in process.
		if kmsSvc != nil {
			sqlAdminSrv.UseKMS(sqlAdminKMS{svc: kmsSvc})
		}
		// IAM database authentication (#1944) checks a token this instance
		// minted, and the principal's cloudsql.instances.login.
		if creds != nil {
			sqlAdminSrv.UseIAM(sqlAdminIAM(creds, iamEnforcer))
			// The Admin API's own methods (#1879).
			sqlAdminSrv.Enforce = cloudSQLEnforcer(iamEnforcer)
		}
		// CUSTOMER_MANAGED_CAS_CA (#1834) has this instance's Certificate
		// Authority Service issue the server certificates, in process.
		if privateCASvc != nil {
			sqlAdminSrv.UseCertificateAuthorityService(sqlAdminCAS{svc: privateCASvc})
		}
		coord.Register(sqlAdminSrv)
	}
	// The Backup and DR Service (#1433), over the Admin API's backups.
	backupDRSrv := newBackupDRServer(cfg, sqlAdminSrv)
	if backupDRSrv != nil {
		// A folder's and the organization's resourceBackupConfigs.fetch
		// answer the projects under them (#1536), in process.
		backupDRSrv.UseHierarchy(backupDRHierarchy{reg: projects})
		backupDRRef = backupDRSrv
		// Compute Engine VMs and disks (#2075), backed up through the
		// Compute Engine API in process once it is started.
		backupDRSrv.SetCompute(func() backupdr.Compute {
			if api := computeSvc.API(); api != nil {
				return api
			}
			return nil
		})
		if kmsSvc != nil {
			// A vault's encryptionConfig (#1501) wraps its data key with
			// this instance's Cloud KMS, in process.
			backupDRSrv.UseKMS(memorystoreKMS{svc: kmsSvc})
		}
		if loggingSvc != nil {
			// Its backup, restore and backup plan job logs (#1924) go to
			// this instance's Cloud Logging, as Google's go to Cloud
			// Logging, so a sink routes them to BigQuery.
			backupDRSrv.UseJobLogs(backupDRJobLogger(loggingSvc.api))
		}
		coord.Register(backupDRSrv)
	}
	// Disabled APIs are enforced as Google enforces them (#1914): every
	// API is enabled in every project until someone disables it.
	useAPIEnablement(serviceUsageSrv, rmSrv, apiGated{tasks: tasksSvc, run: runSvc, scheduler: schedulerSvc,
		memorystore: memorystoreAdminSvc, logging: loggingSvc, kms: kmsSvc, secrets: secretsSvc, sqlAdmin: sqlAdminSrv, backupDR: backupDRSrv})
	// IAM enforcement (#1879) of Backup and DR and the Cloud Storage
	// companion APIs, inside the enablement gate.
	useIAMEnforcementBackup(iamEnforcer, backupDRSrv, storageJobsSrv)
	// API keys are checked where Google checks them (#1915).
	defer useAPIKeys(apiKeysSrv, rmSrv)()

	// Built before the console so the console can offer its playground, and
	// registered after it for the same reason the other components are:
	// ownership by the coordinator rather than a goroutine beside it.
	localAISrv, err := buildLocalAI(cfg)
	if err != nil {
		return err
	}

	// The ingress gateway's host address, empty when it is not published.
	// The runtime file records it with the rest, so `env` and `status` for an
	// instance started with --port-base report its port, not the default
	// their own configuration falls back to (#863).
	ingressAddr := func() string {
		if cfg.Endpoints.Ingress == 0 {
			return ""
		}
		return net.JoinHostPort(cfg.BindAddress, strconv.Itoa(cfg.Endpoints.Ingress))
	}

	// With Cloud Run, the local AI endpoint also serves PredictionService for
	// the custom prediction containers it runs (#1193), so it is started even
	// with no model: the official aiplatform client reaches them through it.
	localAISrv = withPredictionEndpoints(cfg, localAISrv, runSvc, ingressAddr)
	// Model Garden's Hugging Face downloads (#1840), shared by the console
	// and ModelGardenService.Deploy, which serves them on that endpoint.
	hfModels, garden := modelGardenFor(cfg, localAISrv)
	// Document AI's processors and Speech-to-Text's recognizers (#1799),
	// which ML.PROCESS_DOCUMENT and ML.TRANSCRIBE name, are kept with the
	// instance's state.
	if localAISrv != nil {
		localAISrv.SetDocumentAIState(filepath.Join(cfg.StateDir, "documentai", "processors.json"))
		localAISrv.SetSpeechState(filepath.Join(cfg.StateDir, "speech", "recognizers.json"))
		// Vertex AI's cached contents (#2107), until they expire.
		localAISrv.SetCachedContentState(filepath.Join(cfg.StateDir, "vertexai", "cachedcontents.json"))
		// Their customer-managed keys (#1950), through Cloud KMS.
		localAISrv.UseCMEK(cmekKeys(kmsSvc, iamEnforcer))
		// IAM enforcement (#1879) of Vertex AI, Document AI, Speech-to-Text
		// and Translation.
		iamEnforced8{localAI: localAISrv}.use(iamEnforcer)
	}
	// The Model Registry and Endpoints (#1940): deployed models run as
	// Cloud Run services; artifacts are copied in this Cloud Storage.
	withModelRegistry(cfg, localAISrv, runSvc, func() string {
		if f := forwarderFor(forwarders, "storage"); f != nil {
			return f.HostAddr()
		}
		return ""
	})
	if garden != nil {
		defer garden.Close()
	}
	// EmbedContent's file_data parts (#1651) read their gs:// objects
	// from this instance's Cloud Storage.
	if localAISrv != nil && serviceEnabled(cfg, config.ServiceStorage) {
		localAISrv.UseStorage(embedObjects{f: storageSourceFetcher{
			cfg: cfg, client: &http.Client{Timeout: 2 * time.Minute},
			addr: func() string {
				if f := forwarderFor(forwarders, "storage"); f != nil {
					return f.HostAddr()
				}
				return ""
			},
		}})
	}

	// multimodalembedding@001's AVI, FLV, MPEG and WMV video (#1651).
	if localAISrv != nil {
		localAISrv.SetVideoDecoder(newVideoDecoder(stdout))
	}

	// One manager of the local AI models for the console's Local models
	// page and /admin/local-models (#1396), so a download started on one is
	// the download the other reports.
	localModels := localModelsFor(cfg, localAISrv)
	adminAPI.SetLocalModels(localModels)

	// The console reads through the objects above rather than owning any of
	// them, so it cannot answer from a store of its own.
	consoleSrv := buildConsole(consoleDeps{
		cfg: cfg, iam: iamEnforcer, assetOut: assetOut, coord: coord, cluster: clusterComp, localAI: localAISrv, localModels: localModels,
		hfModels: hfModels, garden: garden,
		projects: projects,
		tasks:    tasksSvc, secrets: secretsSvc, forwarders: forwarders,
		kms: kmsSvc, scheduler: schedulerSvc, run: runSvc, logging: loggingSvc,
		memorystore:      memorystoreAdminSvc,
		faults:           faults,
		metaAddr:         metaSrv.Addr,
		ingress:          ingressAddr,
		mysql:            mysqlCreds,
		serviceUsage:     serviceUsageSrv,
		apiKeys:          apiKeysSrv,
		cloudQuotas:      cloudQuotasRef,
		siteVerify:       siteVerificationRef,
		billing:          billingRef,
		authPlatform:     authPlatformRef,
		requestMetrics:   requestMetrics,
		requestHistory:   requestHistory,
		backupdr:         backupDRSrv,
		sqlAdmin:         sqlAdminSrv,
		privateCA:        privateCASvc,
		analyticsHub:     analyticsHub,
		workflows:        workflowsSvc,
		parameterManager: parameterManagerSvc,
		dataform:         dataformSvc,
		appHub:           appHubSvc,
		trace:            traceSvc,
		gke:              gkeSvc,
		monitoring:       monitoringSvc,
		compute:          computeSvc,
		dataTransfer:     dataTransfer,
	})
	if consoleSrv != nil {
		// The Request Log reads the same recorder /admin/events does.
		consoleSrv.SetRequests(newConsoleRequests(recorder, requestMetrics))
		// The fault screen (#800) calls the admin API in process, with the
		// token the page sends: the console never adds one of its own, so it
		// is not a path around the admin token (#553).
		consoleSrv.SetFaults(adminHandler(adminAPI), adminTokenPath(cfg))
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
		// Connect and About (#802): env, gcloud-setup, terraform and
		// diagnose, from the functions those commands use. The diagnose
		// download reaches the admin API in process with the page's own
		// token; the console adds none (#553).
		consoleSrv.SetConnect(newConsoleConnect(cfg, adminAPI))
		// And a `logging/<log>` row offers Delete log, through DeleteLog on
		// that same service (#799). A nil deleter offers none.
		if d := loggingSvc.logDeleter(faults); d != nil {
			consoleSrv.SetLogDeleter(d)
		}
		// And the Explorer reads through a log view (#1410).
		if v := loggingSvc.logViewReader(faults); v != nil {
			consoleSrv.SetLogViewReader(v)
		}
		// And its Log name picker lists ListLogs (#1554).
		if l := loggingSvc.logNameLister(faults); l != nil {
			consoleSrv.SetLogNameLister(l)
		}
		// And the Query library's Recent queries are the user's, through
		// recentQueries.list (#2031).
		if q := loggingSvc.recentQueries(); q != nil {
			consoleSrv.SetRecentQueries(q)
		}
	}

	if localAISrv != nil {
		coord.Register(localAISrv)
	}
	// The onnx build's ONNX runner (#1711), for BigQuery's front; nil in
	// the default build.
	onnxRunner := newONNXRunner(cfg, stdout)
	if onnxRunner != nil {
		coord.Register(onnxRunner)
	}
	// The TensorFlow SavedModel converter (#1775), for BigQuery's front.
	tfConverter := newTFConverter(cfg, stdout)
	if tfConverter != nil {
		coord.Register(tfConverter)
	}
	// The Spark procedure runner (#2104), for BigQuery's front.
	sparkRunner := newSparkRunner(cfg, stdout, boundAddrs, func(email string) (string, error) {
		// An invoker's custom service account (#2104): one that exists
		// and is enabled, as IAM Credentials mints for.
		if projectRegistry != nil {
			a, ok := projectRegistry.ServiceAccountByName(email)
			switch {
			case !ok:
				return "", fmt.Errorf("Service account %s does not exist", email)
			case a.Disabled:
				return "", fmt.Errorf("Service account %s is disabled", email)
			}
			email = a.Email
		}
		return creds.MintAccessTokenFor(email, metadata.TokenTTL), nil
	}, cmekKeys(kmsSvc, iamEnforcer).Usable)
	if sparkRunner != nil {
		coord.Register(sparkRunner)
	}

	// hostComp publishes the CLI-hosted services to the cluster (#575); nil
	// without Cloud Run or BigQuery.
	var hostComp *clusterHost
	// Every bound address, as the runtime file records it and the hooks see it.
	liveEndpoints = func() map[string]string {
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
		if a := serviceUsageSrv.Addr(); a != "" {
			live["serviceusage"] = a
		}
		if a := privateCASvc.Addr(); a != "" {
			live["privateca"] = a
		}
		if a := workflowsSvc.Addr(); a != "" {
			live["workflows"] = a
		}
		if a := parameterManagerSvc.Addr(); a != "" {
			live["parametermanager"] = a
		}
		if a := dataformSvc.Addr(); a != "" {
			live["dataform"] = a
		}
		if a := appHubSvc.Addr(); a != "" {
			live["apphub"] = a
		}
		if a := traceSvc.Addr(); a != "" {
			live["trace"] = a
		}
		if a := gkeSvc.Addr(); a != "" {
			live["gke"] = a
		}
		if a := analyticsHub.Addr(); a != "" {
			live["analyticshub"] = a
		}
		if a := computeSvc.Addr(); a != "" {
			live["compute"] = a
		}
		if a := dataTransfer.Addr(); a != "" {
			live["datatransfer"] = a
		}
		if a := apiKeysSrv.Addr(); a != "" {
			live["apikeys"] = a
		}
		if a := monitoringSvc.Addr(); a != "" {
			live["monitoring"] = a
		}
		storageJobsSrv.live(live)
		if a := firebaseRulesSrv.Addr(); a != "" {
			live["firebaserules"] = a
		}
		if backupDRSrv != nil && backupDRSrv.Addr() != "" {
			live["backupdr"] = backupDRSrv.Addr()
		}
		if sqlAdminSrv != nil && sqlAdminSrv.Addr() != "" {
			live["sqladmin"] = sqlAdminSrv.Addr()
			if a := sqlAdminSrv.ConnectorAddr(); a != "" {
				live["cloudsql-connector"] = a
			}
		}
		if a := loggingSvc.Addr(); a != "" {
			live["logging"] = a
		}
		if a := memorystoreAdminSvc.Addr(); a != "" {
			live["memorystore-admin"] = a
		}
		if a := ingressAddr(); a != "" {
			live["ingress"] = a
		}
		if a := onnxRunner.Addr(); a != "" {
			live["onnx"] = a
		}
		if a := tfConverter.Addr(); a != "" {
			live["tfconvert"] = a
		}
		if a := sparkRunner.Addr(); a != "" {
			live["spark"] = a
		}
		if localAISrv != nil && localAISrv.Addr() != "" {
			live["localai"] = localAISrv.Addr()
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
	// With BigQuery or Cloud Storage and no Cloud Run, Resource Manager
	// alone is published: the BigQuery front reads a project's ancestry
	// from it (#1392), and the storage server a project's and a folder's
	// parents (#1532). So is the onnx build's ONNX runner, which the
	// BigQuery front runs imported ONNX models on (#1711), and, with Cloud
	// Storage, the metadata server, whose service account certificates
	// verify the URLs those accounts sign (#1787).
	runEnabled := serviceEnabled(cfg, config.ServiceRun)
	pubsubKMS := serviceEnabled(cfg, config.ServicePubSub) && serviceEnabled(cfg, config.ServiceKMS)
	if runEnabled || serviceEnabled(cfg, config.ServiceBigQuery) || serviceEnabled(cfg, config.ServiceStorage) || pubsubKMS || firestoreKMS(cfg) || spannerKMS(cfg) || memorystoreCMEK(cfg) || computeEnabled(cfg) {
		// The Cloud SQL Admin API and its connector are published for Cloud
		// SQL volumes (#1346): the Auth Proxy a revision runs reaches both.
		published := map[string]bool{"run": true, "tasks": true, "secretmanager": true, "kms": true,
			"scheduler": true, "logging": true, "resourcemanager": true, "serviceusage": true, "apikeys": true, "metadata": true,
			"storagebatchoperations": true, "storageinsights": true, "storagetransfer": true,
			"sqladmin": true, "cloudsql-connector": true, "onnx": true, "tfconvert": true, "spark": true, "localai": true, "workflows": true, "parametermanager": true, "dataform": true, "apphub": true, "trace": true}
		if !runEnabled {
			published = map[string]bool{"resourcemanager": true, "onnx": true, "tfconvert": true, "spark": true, "localai": true}
			// The storage server reads IAM service accounts' certificates
			// from the metadata server (#1787).
			if serviceEnabled(cfg, config.ServiceStorage) {
				published["metadata"] = true
			}
			// The Pub/Sub front reads CMEK topics' keys (#1874).
			if pubsubKMS {
				published["kms"] = true
			}
			// The Firestore front checks customer-managed keys (#1881).
			if firestoreKMS(cfg) {
				published["kms"] = true
			}
			// Cloud Storage (#1938) and Spanner (#1950) encrypt with
			// customer-managed keys.
			if storageKMS(cfg) || spannerKMS(cfg) {
				published["kms"] = true
			}
			// A CMEK Memorystore server's pod unwraps its data key (#2113).
			if memorystoreCMEK(cfg) {
				published["kms"] = true
			}
		}
		// A VM reads its metadata from the Compute port and its service
		// account's tokens from the metadata server (#1963).
		if computeEnabled(cfg) {
			published["compute"], published["metadata"] = true, true
		}
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
		if runEnabled {
			// A revision's and a task's output in Cloud Logging, as Google
			// writes it (#1298), while Logging is served too.
			newRunLogFeeder(cfg.KubeconfigPath(), cfg.ClusterName()+"-control-plane", loggingSvc).register(coord)
			// Each request to a revision a span in Cloud Trace, and a
			// traceparent for the container (#2118), while Cloud Trace is
			// served.
			coord.Register(&runTracing{cfg: cfg, host: hostComp, out: stdout, trace: traceSvc})
		}
	}
	// Every revision and job task is given the in-cluster addresses (#576):
	// the tunnelled backends' Service names, known now, and the CLI-hosted
	// services' cloudburrow-host name, known once hostComp has started —
	// after the adapter — so the table is read per request, not at Start.
	if computeSvc != nil {
		computeSvc.guestBase = func() string {
			if a := hostComp.InCluster("compute"); a != "" {
				return "http://" + a
			}
			return ""
		}
		computeSvc.metadataHost = func() string { return hostComp.InCluster("metadata") }
	}
	if memorystoreAdminSvc != nil && kmsSvc != nil {
		// A CMEK Memorystore server's pod unwraps its data key with Cloud
		// KMS, through cloudburrow-host (#2113).
		memorystoreAdminSvc.kmsInCluster = func() string { return hostComp.InCluster("kms") }
	}
	if runSvc != nil {
		// A build step reaches the metadata server as its build's account (#1854).
		runSvc.stepMetadata = func() string { return hostComp.HostAddr("metadata") }
	}
	runSvc.useEnvironment(func() map[string]string {
		return podEnvMap(podAddresses(forwarders, hostComp))
	})
	if runSvc != nil && runSvc.events != nil {
		// The Pub/Sub emulator pushes a trigger's events to the Run port
		// at its cloudburrow-host name.
		runSvc.events.pushBase = func() string {
			if a := hostComp.InCluster("run"); a != "" {
				return "http://" + a
			}
			return ""
		}
	}
	// Cloud SQL volumes (#1346): the Auth Proxy beside a revision asks the
	// Admin API, through cloudburrow-host, and is told to dial the
	// connector at that Service's cluster IP, port 3307, which forwards to
	// the pods' own connector listener.
	// Pods have a connector listener of their own, on any port: the
	// Service maps 3307 to it, so a Cloud SQL volume works even while
	// another instance on this machine holds 3307.
	if sqlAdminSrv != nil && hostComp != nil && runEnabled {
		sqlAdminSrv.ServeConnectorToPods(net.JoinHostPort(cfg.BindAddress, "0"), hostComp.ClusterIP)
		runSvc.useCloudSQL(cloudSQLForPods(sqlAdminSrv, hostComp))
	}
	// The console's terminal (#781) runs in a pod given the same addresses,
	// read when the pod is made.
	termWarm := &terminalWarm{}
	if consoleSrv != nil {
		consoleSrv.SetTerminal(newConsoleTerminal(cfg, func() map[string]string {
			return podAddresses(forwarders, hostComp)
		}, termWarm))
		// The Instance page (#801): state save and load, reset and seed,
		// through the admin API in process with the token the page sends,
		// never one the console holds (#553).
		consoleSrv.SetInstance(newConsoleInstance(cfg, adminAPI), adminTokenPath(cfg))
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
	// The terminal's image, when prefetched, goes into the node in the
	// background: nothing that has started needs it (#824).
	if consoleSrv != nil {
		if art := prefetch.TerminalArtifact(cached.plan.Terminal); cached.plan.Terminal != "" && cached.cache.Has(art) {
			termWarm.start(ctx, func(ctx context.Context) error {
				_, err := cached.loader().LoadOptional(ctx, art)
				return err
			}, stdout)
		}
	}

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
// buildForwarders returns a tunnel per service that has an in-cluster
// backend.
func buildForwarders(cfg config.Config) []*netfwd.Forwarder {
	var out []*netfwd.Forwarder
	for _, s := range cfg.EnabledServices() {
		for _, t := range forwardTargets(cfg, s) {
			out = append(out, netfwd.New(t, cfg.KubeconfigPath(), cfg.BindAddress))
		}
	}
	return out
}

// forwardTargets are the in-cluster Services a service's tunnels reach, or
// none for a service served in-process. Their InClusterAddr is what a pod
// uses, which `env --format kubernetes` and the Cloud Run adapter's
// injected environment read from here (#576).
func forwardTargets(cfg config.Config, s config.Service) []netfwd.Target {
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
			return nil
		}
		port = components.OptionalPort(s)
		// The configured port, so `cloudburrow env` — a separate
		// process — can export the same address this binds.
		hostPort = cfg.Endpoints.OptionalPort(s)
	}
	out := []netfwd.Target{{
		Name:        string(s),
		Namespace:   cfg.Cluster.Namespace,
		ServicePort: port,
		HostPort:    hostPort,
		Guarded:     guardedTunnel(s),
	}}
	if s == config.ServiceFirestore {
		// Firestore with MongoDB compatibility (#1703), the same Service
		// on a second port, which the call front serves itself. It is
		// the MongoDB wire protocol, not HTTP, so it is left raw, as
		// Memorystore's and Cloud SQL's are.
		out = append(out, netfwd.Target{
			Name:        string(s),
			Label:       "firestore-mongodb",
			Namespace:   cfg.Cluster.Namespace,
			ServicePort: components.FirestoreMongoDBPort,
			HostPort:    cfg.Endpoints.FirestoreMongoDB,
		})
	}
	if s == config.ServiceSpanner {
		// Spanner's REST API (#1331), the same Service on a second port,
		// which the call front in the pod serves (#1288) into the same
		// call log as the gRPC port. Its own tunnel, labelled apart: the
		// Terraform provider reaches it as spanner_custom_endpoint. It is
		// REST over HTTP/1.1, which a browser can send, so it is guarded.
		out = append(out, netfwd.Target{
			Name:        string(s),
			Label:       "spanner-rest",
			Namespace:   cfg.Cluster.Namespace,
			ServicePort: components.SpannerRESTPort,
			HostPort:    cfg.Endpoints.SpannerREST,
			Guarded:     true,
		})
	}
	if s == config.ServiceBigQuery {
		// The tunnel needs no front of its own: the validating front
		// (internal/bigqueryfront, #861) serves the Service's REST port in
		// the emulator's pod (#902), so this tunnel and every pod reach it.
		// The Storage Read API is the same Service on a second port. It
		// gets its own tunnel, labelled apart from the REST one, because
		// the Go client's result iterator reads large results through it.
		out = append(out, netfwd.Target{
			Name:        string(s),
			Label:       "bigquery-storage",
			Namespace:   cfg.Cluster.Namespace,
			ServicePort: components.BigQueryStoragePort,
			HostPort:    cfg.Endpoints.BigQueryStorage,
		})
	}
	return out
}

// guardedTunnel reports whether a service's tunnel gets the Host check,
// the DNS-rebinding defence (#725, ADR-0004). These are the upstream
// emulators whose port serves REST over HTTP/1.1, which a browser can
// send: Pub/Sub, Firestore and Datastore (REST and gRPC on one port) and
// BigQuery's REST port (measured against the pinned images).
//
// The rest are left raw on purpose. Spanner's tunnelled port, Bigtable and
// BigQuery's Storage Read port speak gRPC only and do not answer
// HTTP/1.1, and browsers do not speak cleartext HTTP/2. Cloud SQL and
// Memorystore are database wire protocols, not HTTP. Storage's own server
// checks Host itself (#676). Whatever HTTP/1.1 the fronts in the pods serve
// on a raw tunnel's port, such as Bigtable's REST admin API, they check Host
// and Origin themselves (hostguard.WrapService, #1587).
func guardedTunnel(s config.Service) bool {
	switch s {
	case config.ServicePubSub, config.ServiceFirestore, config.ServiceDatastore, config.ServiceBigQuery:
		return true
	}
	return false
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
	fmt.Fprintf(w, "  iam:        enforcement %s\n", iamEnforcementMode(cfg))

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
	iam := iamEnforcementMode(cfg)
	if info, err := readRuntime(cfg); err == nil && info.IAMEnforcement != "" {
		iam = info.IAMEnforcement
	}
	fmt.Fprintf(stdout, "iam:        enforcement %s\n", iam)
	// Microsoft's SQL Server licence (#1916): who accepted it, when, how.
	for _, l := range mssqlEULALines(readMSSQLEULAStatus(cfg)) {
		fmt.Fprintln(stdout, l)
	}
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
			if s == config.ServiceCloudSQL {
				fmt.Fprintf(w, "  %-16s user %s, database %s, no password (trust auth; `cloudburrow env` exports PG*)\n", "",
					components.CloudSQLUser, components.CloudSQLDatabase)
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
	case config.ServiceSpanner:
		return []configuredEndpoint{{"spanner", e.Spanner}, {"spanner-rest", e.SpannerREST}}
	case config.ServiceFirestore:
		return []configuredEndpoint{{"firestore", e.Firestore}, {"firestore-mongodb", e.FirestoreMongoDB}}
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
