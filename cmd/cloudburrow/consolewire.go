package main

import (
	"context"
	"fmt"
	"github.com/cloudburrow/cloudburrow/internal/iameval"
	"io"
	"net"
	"path/filepath"
	"strconv"
	"time"

	runadapter "github.com/cloudburrow/cloudburrow/internal/adapter/run"
	"github.com/cloudburrow/cloudburrow/internal/admin"
	"github.com/cloudburrow/cloudburrow/internal/authplatform"
	"github.com/cloudburrow/cloudburrow/internal/components"
	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/firestoreadmin"
	"github.com/cloudburrow/cloudburrow/internal/hfmodels"
	"github.com/cloudburrow/cloudburrow/internal/lifecycle"
	"github.com/cloudburrow/cloudburrow/internal/localai"
	"github.com/cloudburrow/cloudburrow/internal/metadata"
	"github.com/cloudburrow/cloudburrow/internal/metrics"
	"github.com/cloudburrow/cloudburrow/internal/modelgarden"
	"github.com/cloudburrow/cloudburrow/internal/netfwd"
	"github.com/cloudburrow/cloudburrow/internal/service/apikeys"
	"github.com/cloudburrow/cloudburrow/internal/service/backupdr"
	"github.com/cloudburrow/cloudburrow/internal/service/billing"
	"github.com/cloudburrow/cloudburrow/internal/service/cloudquotas"
	"github.com/cloudburrow/cloudburrow/internal/service/datatransfer"
	"github.com/cloudburrow/cloudburrow/internal/service/monitoring"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
	"github.com/cloudburrow/cloudburrow/internal/service/serviceusage"
	"github.com/cloudburrow/cloudburrow/internal/service/siteverification"
	"github.com/cloudburrow/cloudburrow/internal/service/sqladmin"
	"github.com/cloudburrow/cloudburrow/internal/service/vertexai"
)

// consoleDeps are the running pieces the console reads through.
//
// They are passed rather than looked up because the console must never own a
// service: it is handed the same objects the services use, so there is no way
// for it to answer from anywhere else.
type consoleDeps struct {
	cfg config.Config
	// iam is the IAM evaluator Policy Troubleshooter and Policy Analyzer
	// answer from (#1879); nil leaves those pages without answers.
	iam     *iameval.Enforcer
	localAI *vertexai.Server
	// localModels manages the local AI models, shared with the admin API
	// (#1396); nil makes one for the console alone.
	localModels *localai.Manager
	// hfModels and garden are Model Garden's Hugging Face downloads and
	// deployments (#1840), shared with ModelGardenService; nil hfModels
	// makes a download manager for the console alone.
	hfModels  *hfmodels.Manager
	garden    *modelgarden.Manager
	projects  *resourcemanager.Registry
	coord     *lifecycle.Coordinator
	cluster   interface{ ServerVersion() string }
	tasks     *tasksService
	secrets   *secretsService
	kms       *kmsService
	scheduler *schedulerService
	// memorystore is the Memorystore admin API (#1302), whose instances the
	// Memorystore page creates, edits and deletes.
	memorystore *memorystoreAdminService
	// logging is Cloud Logging, whose metrics, sinks and exclusions have
	// screens of their own (#1226).
	logging *loggingService
	// faults are the instance's fault rules, applied to the screens that read
	// an in-process service's store as that service's List call (#594).
	faults *admin.Faults
	// run is the Cloud Run adapter, asked for the address it bound.
	run        interface{ Addr() string }
	forwarders []*netfwd.Forwarder
	metaAddr   func() string
	ingress    func() string
	// mysql are the instance's generated MySQL passwords, which the Cloud SQL
	// for MySQL screen reads and administers the server with (#868).
	mysql components.MySQLCredentials
	// serviceUsage is the Service Usage API (#1352), whose APIs & Services
	// page reads and changes the same server.
	serviceUsage *serviceusage.Server
	// apiKeys is the API Keys API (#1915), whose keys Credentials lists.
	apiKeys *apikeys.Server
	// cloudQuotas and siteVerify (#2030) back Quotas & System Limits and
	// Domain verification.
	cloudQuotas *cloudquotas.Server
	siteVerify  *siteverification.Server
	// billing (#2109) backs the Billing pages.
	billing *billing.Server
	// authPlatform backs the OAuth consent screen and OAuth clients.
	authPlatform *authplatform.Store
	// requestMetrics are the request counters, which APIs & Services
	// counts each API's calls from (#1866).
	requestMetrics *metrics.Registry
	// requestHistory is their one-minute history per project, which the
	// APIs & Services charts read over a time range (#1866).
	requestHistory *monitoring.RequestBuiltin
	// backupdr is the Backup and DR Service (#1433), nil unless enabled.
	backupdr *backupdr.Server
	// privateCA is Certificate Authority Service (#1834), nil unless enabled.
	privateCA *privateCAService
	// analyticsHub is BigQuery sharing (#1960), nil without BigQuery.
	analyticsHub *analyticsHubSvc
	// workflows is Workflows (#1986), nil unless enabled.
	workflows *workflowsService
	// parameterManager is Parameter Manager (#2054), nil unless enabled.
	parameterManager *parameterManagerService
	// dataform is Dataform (#2105), nil unless enabled.
	dataform *dataformService
	// appHub is App Hub (#2110), nil unless enabled.
	appHub *appHubService
	// trace is Cloud Trace (#2118), nil unless enabled.
	trace *traceService
	// gke is the Kubernetes Engine API (#1913), nil unless enabled.
	gke *gkeService
	// monitoring is Cloud Monitoring (#1939), nil unless enabled.
	monitoring *monitoringService
	// compute is the Compute Engine API (#1936), nil unless enabled.
	compute *computeService
	// dataTransfer is the BigQuery Data Transfer Service (#1948).
	dataTransfer *dataTransferSvc
	// sqlAdmin is the Cloud SQL Admin API, which restores Backup and DR
	// backups; nil unless a Cloud SQL service is enabled.
	sqlAdmin *sqladmin.Server
}

// runAddrOf is the adapter's live address, or nothing when there is no
// adapter. The configured port is not used: with --port-run 0 it is 0, and
// the console dialled 127.0.0.1:0 (#646).
func runAddrOf(run interface{ Addr() string }) func() string {
	if run == nil {
		return func() string { return "" }
	}
	return run.Addr
}

// buildConsole returns the console server, or nil when it is disabled.
func buildConsole(d consoleDeps) *console.Server {
	if d.cfg.Endpoints.Console < 0 {
		return nil
	}

	kubeconfig := d.cfg.KubeconfigPath()
	// One source, read by the dashboard's panel and joined onto the Pods
	// listing. Two readers of one kubelet call rather than two calls.
	metrics := clusterMetrics(kubeconfig)
	// The history is built before the providers, because a pod's detail page
	// charts its own past out of it: created here and handed both to the
	// provider that reads it and to the server that writes it, so there is one
	// ring rather than one per reader.
	series := console.NewSeries(console.SeriesLimit, nil)
	providers := consoleProviders(d, metrics, series)

	addr := net.JoinHostPort(d.cfg.BindAddress, strconv.Itoa(d.cfg.Endpoints.Console))
	srv := console.New(addr, consoleStatus(d), providers...)
	if d.projects != nil {
		srv.SetPolicyLinter(iamLinter{registry: d.projects}) // #1756, consoleiamaudit.go
	}
	srv.SetPlayground(playgroundFor(d))
	srv.SetPrediction(predictionFor(d))
	if d.localModels == nil {
		d.localModels = localModelsFor(d.cfg, d.localAI)
	}
	srv.SetLocalModels(d.localModels)
	// Model Garden's Hugging Face downloads (#1840), into the state dir's
	// model cache beside the pinned local models.
	if d.hfModels == nil {
		d.hfModels = &hfmodels.Manager{Dir: filepath.Join(d.cfg.StateDir, "models", "huggingface")}
	}
	srv.SetHFModels(d.hfModels)
	srv.SetModelGarden(d.garden, d.localAI, d.cfg.DefaultProject())
	srv.SetMetrics(metrics)
	// The history is the console's, not a browser tab's. Kept server-side so
	// it survives a reload and so the sampling rate does not depend on how
	// many people are looking.
	srv.SetSeries(series)
	return srv
}

// consoleProviders is the console's registry: every screen this instance
// serves, in navigation order.
//
// Separate from buildConsole so a test can build exactly the set a running
// instance would, rather than a hand-kept list that a new screen can be left
// out of. TestNoDetailShowsABlankProperty walks every one of them.
func consoleProviders(d consoleDeps, metrics console.MetricsSource, series *console.Series) []console.Provider {
	storageAddr := forwardedAddr(d.forwarders, "storage")
	pubsubAddr := forwardedAddr(d.forwarders, "pubsub")

	enabled := map[config.Service]bool{}
	for _, s := range d.cfg.EnabledServices() {
		enabled[s] = true
	}

	var providers []console.Provider
	// Resource Manager first: it is where the projects every other screen is
	// scoped to come from.
	if d.projects != nil {
		h := hierarchyProvider{registry: d.projects}
		if enabled[config.ServiceLogging] && d.logging != nil {
			// A folder's and organization's Log router tab (#1572).
			h.sinks = &loggingAdminScreen{svc: d.logging, faults: d.faults}
		}
		providers = append(providers, projectsProvider{registry: d.projects}, h, tagsProvider{registry: d.projects},
			projectIAMProvider{registry: d.projects, iam: d.iam}, iamRolesProvider{registry: d.projects}, iamGroupsProvider{registry: d.projects},
			iamSSOProfilesProvider{registry: d.projects}, iamSSOAssignmentsProvider{registry: d.projects},
			iamServiceAccountsProvider{registry: d.projects}, iamWorkloadIdentityProvider{registry: d.projects}, iamWorkforceIdentityProvider{registry: d.projects}, iamPABProvider{registry: d.projects}, iamAuditProvider{registry: d.projects},
			iamOrgPoliciesProvider{registry: d.projects}, iamLabelsProvider{registry: d.projects}, iamSettingsProvider{registry: d.projects})
	}
	if enabled[config.ServiceStorage] && storageAddr != "" {
		// Three screens of one product: buckets, the project's soft-deleted
		// buckets, which the bucket list no longer holds (#789), and Settings,
		// with the project's service account and HMAC keys (#792). With
		// Pub/Sub, the bucket browser offers the topics a bucket notification
		// publishes to (#791).
		sp := storageProvider{endpoint: storageAddr}
		if enabled[config.ServicePubSub] {
			sp.pubsub = pubsubAddr
		}
		// A bucket's Inventory reports tab (#1843) reads Storage Insights.
		sp.insights = companionAddr(d.cfg, d.cfg.Endpoints.StorageInsights)
		providers = append(providers, sp,
			storageDeletedProvider{endpoint: storageAddr},
			storageSettingsProvider{endpoint: storageAddr},
			// Storage Intelligence (#1519): the project's, the
			// organization's and each folder's configuration.
			storageIntelligenceProvider{endpoint: storageAddr, registry: d.projects})
		// The Cloud Storage companion APIs (#1957), served in process beside
		// Cloud Storage: Batch operations, Inventory reports, and Storage
		// Transfer Service's Transfer jobs and Agent pools.
		e := d.cfg.Endpoints
		providers = append(providers, storageJobsProviders(storageAddr, companionAddr(d.cfg, e.StorageBatchOperations),
			companionAddr(d.cfg, e.StorageInsights), companionAddr(d.cfg, e.StorageTransfer))...)
	}
	if enabled[config.ServicePubSub] && pubsubAddr != "" {
		// Four screens of one product: topics, every subscription of the
		// project whatever its topic (#595), its snapshots (#787) and its
		// schemas (#788).
		providers = append(providers, pubsubProvider{endpoint: pubsubAddr},
			pubsubSubscriptionsProvider{endpoint: pubsubAddr},
			pubsubSnapshotsProvider{endpoint: pubsubAddr},
			pubsubSchemasProvider{endpoint: pubsubAddr})
	}
	if enabled[config.ServiceTasks] && d.tasks != nil {
		tp := tasksProvider{svc: d.tasks, faults: d.faults}
		if enabled[config.ServiceMonitoring] && d.monitoring != nil {
			tp.metrics = monitoringScreen{api: monitoringAPIOf(d.monitoring)}
		}
		providers = append(providers, tp)
	}
	// workflowsEventarc is Eventarc, for Workflows' Eventarc triggers
	// (#2011); nil without it.
	var workflowsEventarc *eventarcProvider
	if enabled[config.ServiceRun] {
		// Agent Platform's Model Registry and Endpoints (#1940), through
		// the official aiplatform client on the local AI endpoint, which
		// Cloud Run starts.
		var api vertexAPI
		if d.localAI != nil {
			api.addr = d.localAI.Addr
		}
		providers = append(providers, vertexModelsProvider{api: api}, vertexEndpointsProvider{api: api})
	}
	if enabled[config.ServiceRun] {
		// Two screens of one product: services, and jobs with their
		// executions (#785).
		// The storage tunnel's bound address, not the configured port, which
		// is 0 when the OS assigns it (--port-storage 0).
		var runStorage, runPubSub string
		if enabled[config.ServiceStorage] && storageAddr != "" {
			runStorage = hostURL(storageAddr)
		}
		if enabled[config.ServicePubSub] {
			runPubSub = pubsubAddr
		}
		functions := &functionsProvider{defaultProject: d.cfg.DefaultProject(), runAddr: runAddrOf(d.run),
			storage: enabled[config.ServiceStorage]}
		providers = append(providers, runProvider{
			kubeconfig:     d.cfg.KubeconfigPath(),
			namespace:      runadapter.WorkloadNamespace,
			runAddr:        runAddrOf(d.run),
			defaultProject: d.cfg.DefaultProject(),
			storageAddr:    runStorage,
			pubsubAddr:     runPubSub,
			functions:      functions,
		}, runDomainsProvider{run: runProvider{
			kubeconfig:     d.cfg.KubeconfigPath(),
			namespace:      runadapter.WorkloadNamespace,
			runAddr:        runAddrOf(d.run),
			defaultProject: d.cfg.DefaultProject(),
		}}, runJobsProvider{
			kubeconfig:     d.cfg.KubeconfigPath(),
			namespace:      runadapter.WorkloadNamespace,
			runAddr:        runAddrOf(d.run),
			defaultProject: d.cfg.DefaultProject(),
		}, runWorkerPoolsProvider{
			runAddr:        runAddrOf(d.run),
			defaultProject: d.cfg.DefaultProject(),
			readyTimeout:   time.Duration(d.cfg.ReadyTimeout),
		}, runInstancesProvider{
			runAddr:        runAddrOf(d.run),
			defaultProject: d.cfg.DefaultProject(),
			readyTimeout:   time.Duration(d.cfg.ReadyTimeout),
		}, runBuildsProvider{defaultProject: d.cfg.DefaultProject(), runAddr: runAddrOf(d.run), hostSteps: d.cfg.CloudBuildHostSteps},
			// Cloud Build's triggers and worker pools (#1422).
			buildTriggersProvider{b: runBuildsProvider{defaultProject: d.cfg.DefaultProject(), runAddr: runAddrOf(d.run)}, registry: d.projects},
			buildWorkerPoolsProvider{b: runBuildsProvider{defaultProject: d.cfg.DefaultProject(), runAddr: runAddrOf(d.run)}},
			// Cloud Build's Dashboard, Repositories and Settings (#1854).
			buildDashboardProvider{b: runBuildsProvider{defaultProject: d.cfg.DefaultProject(), runAddr: runAddrOf(d.run)}},
			buildRepositoriesProvider{ssm: sourceManagerProvider{b: runBuildsProvider{defaultProject: d.cfg.DefaultProject(), runAddr: runAddrOf(d.run)}}},
			buildSettingsProvider{b: runBuildsProvider{defaultProject: d.cfg.DefaultProject(), runAddr: runAddrOf(d.run)}, registry: d.projects},
			// Secure Source Manager's instances and repositories (#1527).
			sourceManagerProvider{b: runBuildsProvider{defaultProject: d.cfg.DefaultProject(), runAddr: runAddrOf(d.run)}},
			// Artifact Registry's repositories are served on the Run port (#1347).
			artifactRegistryProvider{defaultProject: d.cfg.DefaultProject(), runAddr: runAddrOf(d.run)},
			// Its Settings page (#1855).
			artifactRegistrySettingsProvider{defaultProject: d.cfg.DefaultProject()},
			// Cloud Functions v2 is served on the Run port too (#1377).
			*functions)
		if runPubSub != "" {
			// Eventarc's own Triggers page (#1877), on the Run port, over
			// Pub/Sub, its transport.
			eaRun := runProvider{runAddr: runAddrOf(d.run), defaultProject: d.cfg.DefaultProject(), pubsubAddr: runPubSub}
			ea := eventarcProvider{run: eaRun}
			workflowsEventarc = &ea
			providers = append(providers, ea,
				// Eventarc Advanced's pages and its channels (#1947).
				eaBusesProvider{eaAdvBase{eaRun}}, eaPipelinesProvider{eaAdvBase{eaRun}}, eaEnrollmentsProvider{eaAdvBase{eaRun}},
				eaSourcesProvider{eaAdvBase{eaRun}}, eaChannelsProvider{eaAdvBase{eaRun}})
		}
	}
	if enabled[config.ServiceSecrets] && d.secrets != nil {
		sp := secretsProvider{svc: d.secrets, faults: d.faults}
		// Regional secrets (#1850) are their own page in Secret Manager's menu.
		providers = append(providers, sp, secretsRegionalProvider{sp})
	}
	// Cloud KMS and Cloud Scheduler run in this process too, so their screens
	// read the objects their APIs serve rather than going through a tunnel.
	if enabled[config.ServiceKMS] && d.kms != nil {
		providers = append(providers, kmsProvider{svc: d.kms}, kmsEkmProvider{svc: d.kms}, kmsAutokeyProvider{svc: d.kms}, kmsConfigProvider{svc: d.kms}, kmsInventoryProvider{svc: d.kms}, kmsHsmProvider{svc: d.kms})
	}
	// Certificate Authority Service (#1834) runs in this process too.
	// BigQuery's Sharing (Analytics Hub) page (#1960).
	if d.analyticsHub != nil {
		providers = append(providers, analyticsHubProvider{svc: d.analyticsHub}, cleanRoomsProvider{svc: d.analyticsHub}) // clean rooms: #2101
	}
	if enabled[config.ServicePrivateCA] && d.privateCA != nil {
		providers = append(providers, privateCAProvider{svc: d.privateCA}, privateCATemplatesProvider{svc: d.privateCA})
	}
	// Parameter Manager (#2054), in process, under Secret Manager.
	if enabled[config.ServiceParameterManager] && d.parameterManager != nil {
		providers = append(providers, parameterManagerProvider{svc: d.parameterManager},
			parameterTemplatesProvider{svc: d.parameterManager})
	}
	// Dataform (#2105), in process, under BigQuery.
	if enabled[config.ServiceDataform] && d.dataform != nil {
		df := dataformProvider{svc: d.dataform}
		providers = append(providers, df, dataformFoldersProvider{df: df}, dataformSettingsProvider{df: df})
	}
	// App Hub (#2110), in process: Applications, Services and workloads,
	// Settings.
	if enabled[config.ServiceAppHub] && d.appHub != nil {
		providers = append(providers, appHubProvider{svc: d.appHub}, appHubDiscoveredProvider{svc: d.appHub}, appHubSettingsProvider{svc: d.appHub})
	}
	// Workflows (#1986), in process.
	if enabled[config.ServiceWorkflows] && d.workflows != nil {
		wp := workflowsProvider{svc: d.workflows, logs: d.logging, eventarc: workflowsEventarc}
		// Its Create workflow's Cloud Scheduler triggers (#2011).
		if enabled[config.ServiceScheduler] && d.scheduler != nil {
			wp.scheduler = d.scheduler
		}
		providers = append(providers, wp)
	}
	// Cloud Trace (#2118), in process: the Trace Explorer and its trace
	// details, whose trace scopes and log entries are Cloud Logging's.
	if enabled[config.ServiceTrace] && d.trace != nil {
		tp := traceExplorerProvider{svc: d.trace}
		if enabled[config.ServiceLogging] {
			tp.logging = d.logging
		}
		providers = append(providers, tp)
	}
	// Cloud Monitoring (#1939) runs in this process too: Metrics explorer
	// and Metrics management.
	// A dashboard's Log Analytics chart runs its SQL through Log
	// Analytics (#2032), set below where the instance serves it.
	var runAnalytics func(ctx context.Context, project, sql string) (console.Listing, error)
	if enabled[config.ServiceMonitoring] && d.monitoring != nil {
		screen := monitoringScreen{api: monitoringAPIOf(d.monitoring)}
		providers = append(providers, monitoringExplorerProvider{screen}, monitoringMetricsProvider{screen})
		// Alerting, uptime checks, dashboards, groups, services and
		// snoozes (#1964).
		admin := monitoringAdmin{api: monitoringServerOf(d.monitoring)}
		providers = append(providers, monitoringAlertingProvider{admin}, monitoringIncidentsProvider{admin},
			monitoringChannelsProvider{admin}, monitoringUptimeProvider{admin},
			monitoringDashboardsProvider{monitoringAdmin: admin, metrics: screen, analytics: func(ctx context.Context, project, sql string) (console.Listing, error) {
				if runAnalytics == nil {
					return console.Listing{}, errAnalyticsUnavailable
				}
				return runAnalytics(ctx, project, sql)
			}}, monitoringGroupsProvider{admin},
			monitoringServicesProvider{admin}, monitoringSnoozesProvider{admin})
	}
	// VPC network (#1936): the Compute Engine API's networks, subnets,
	// firewall rules, routes and addresses, in this process.
	if enabled[config.ServiceCompute] && d.compute != nil {
		screen := computeScreen{svc: d.compute}
		providers = append(providers, computeNetworksProvider{screen}, computeFirewallsProvider{screen},
			computeRoutesProvider{screen}, computeAddressesProvider{screen},
			// Compute Engine (#1965): VM instances, disks, snapshots,
			// images and machine types, over the same API.
			computeInstancesProvider{screen}, computeDisksProvider{screen}, computeSnapshotsProvider{screen},
			computeImagesProvider{screen}, computeMachineTypesProvider{screen}, computeMetadataProvider{screen})
	}
	// BigQuery's Data transfers and Scheduled queries pages (#1948).
	if d.dataTransfer != nil {
		providers = append(providers, dataTransferProvider{svc: d.dataTransfer},
			dataTransferProvider{svc: d.dataTransfer, only: datatransfer.ScheduledQuery})
	}
	if enabled[config.ServiceScheduler] && d.scheduler != nil {
		providers = append(providers, schedulerProvider{svc: d.scheduler, faults: d.faults, logs: d.logging})
	}
	// Billing (#2109): always served, on the Resource Manager port.
	if d.billing != nil {
		bc := billingConsole{srv: d.billing, catalog: billingCatalogRef, projects: d.projects}
		providers = append(providers, billingAccountsProvider{bc}, billingProjectsProvider{bc}, billingBudgetsProvider{bc}, billingPricingProvider{bc})
	}
	// APIs & Services (#1352): always served, as projects are.
	if d.serviceUsage != nil {
		providers = append(providers, serviceUsageProvider{api: d.serviceUsage, metrics: d.requestMetrics, history: d.requestHistory, apiKeys: d.apiKeys, quotas: d.cloudQuotas, sites: d.siteVerify, auth: d.authPlatform, metaAddr: d.metaAddr}, serviceUsageOperationsProvider{api: d.serviceUsage})
	}
	// Backup and DR (#1433), in process, restoring through Cloud SQL.
	if enabled[config.ServiceBackupDR] && d.backupdr != nil {
		p := backupDRProvider{bdr: d.backupdr}
		if d.sqlAdmin != nil {
			p.sql = d.sqlAdmin
		}
		if enabled[config.ServiceCompute] && d.compute != nil {
			// Compute Engine VMs and disks (#2075).
			p.compute = d.compute.API
		}
		// Google's menu (#1844): Overview, Vaulted backups, Backup plans,
		// Backup plan associations, Backup vaults (p) and Jobs.
		providers = append(providers, bdrOverviewProvider{api: p}, bdrVaultedProvider{api: p}, bdrPlansProvider{api: p},
			bdrAssocsProvider{api: p}, p, bdrJobsProvider{api: p})
	}
	// Cloud Logging's admin screens, beside the Logs Explorer (#1226).
	if enabled[config.ServiceLogging] && d.logging != nil {
		screen := loggingAdminScreen{svc: d.logging, faults: d.faults}
		if enabled[config.ServiceMonitoring] && d.monitoring != nil {
			screen.monitoring = monitoringServerOf(d.monitoring)
		}
		metrics := loggingMetricsProvider{loggingAdminScreen: screen, monitoring: enabled[config.ServiceMonitoring] && d.monitoring != nil}
		providers = append(providers, metrics, loggingSinksProvider{screen}, loggingExclusionsProvider{screen}, loggingBucketsProvider{screen},
			loggingSettingsProvider{screen},
			// Saved queries and log scopes (#1519).
			loggingSavedQueriesProvider{screen},
			loggingLogScopesProvider{loggingAdminScreen: screen},
			// Trace scopes (#2108), a tab of Settings beside Log Scopes.
			loggingTraceScopesProvider{loggingAdminScreen: screen},
			// The Observability API's settings and buckets (#2117), tabs
			// of Settings too.
			observabilitySettingsProvider{screen}, observabilityBucketsProvider{screen})
		// Log Analytics (#1863), over the buckets' linked datasets in the
		// instance's BigQuery; without BigQuery the page says it is not
		// available.
		if addr := forwardedAddr(d.forwarders, string(config.ServiceBigQuery)); enabled[config.ServiceBigQuery] && addr != "" {
			la := loggingAnalyticsProvider{screen,
				bigqueryProvider{endpoint: addr, project: d.cfg.DefaultProject(), registry: d.projects}}
			providers = append(providers, la)
			runAnalytics = func(ctx context.Context, project, sql string) (console.Listing, error) {
				return la.Query(ctx, project, nil, sql)
			}
		}
	}
	// The opt-in databases. Each had a working backend and no screen, which
	// reads as "not implemented" to anyone looking at the console. The
	// forwarder knows their host addresses; a service that is enabled but has
	// no tunnel yet simply has no screen rather than a broken one.
	for _, db := range []struct {
		service config.Service
		build   func(addr string) console.Provider
	}{
		{config.ServiceFirestore, func(a string) console.Provider { return firestoreProvider{endpoint: a} }},
		// Databases, indexes and field overrides, through the Admin API the
		// call front serves (#1331).
		{config.ServiceFirestore, func(a string) console.Provider {
			return firestoreDatabasesProvider{endpoint: a, mongo: forwardedAddr(d.forwarders, "firestore-mongodb"),
				mongoCA: filepath.Join(d.cfg.InstanceDir(), firestoreadmin.MongoCAFile), projects: d.projects,
				rules: firebaseRulesAddr(d.cfg)}
		}},
		// The Firestore Admin API lists the Datastore mode databases the
		// page's database picker offers (#1883).
		{config.ServiceDatastore, func(a string) console.Provider {
			fs := ""
			if enabled[config.ServiceFirestore] {
				fs = forwardedAddr(d.forwarders, string(config.ServiceFirestore))
			}
			return datastoreProvider{endpoint: a, firestore: fs}
		}},
		{config.ServiceBigtable, func(a string) console.Provider { return bigtableProvider{endpoint: a} }},
		// Instances, clusters, app profiles and instance IAM, through the
		// instance admin API the Bigtable front serves (#1358).
		{config.ServiceBigtable, func(a string) console.Provider {
			return bigtableInstancesProvider{endpoint: a, savedQueries: filepath.Join(d.cfg.InstanceDir(), bigtableSavedQueriesFile)}
		}},
		{config.ServiceSpanner, func(a string) console.Provider { return spannerProvider{endpoint: a} }},
		{config.ServiceCloudSQL, func(a string) console.Provider { return cloudSQLProvider{endpoint: a} }},
		{config.ServiceCloudSQLMySQL, func(a string) console.Provider { return newCloudSQLMySQLProvider(a, d.mysql) }},
		// Memorystore's Instances page, through the admin API (#1302).
		{config.ServiceMemorystore, func(a string) console.Provider { return newMemorystoreProvider(d.cfg, a, d.memorystore) }},
		// Memorystore for Redis Cluster's page (#1519).
		{config.ServiceMemorystore, func(string) console.Provider { return redisClusterProvider{svc: d.memorystore} }},
		// Memorystore for Valkey's page (#1519).
		{config.ServiceMemorystore, func(string) console.Provider { return newValkeyProvider(d.memorystore) }},
		// The emulator serves the instance's default project and no other, so
		// the screen is told which one that is (#698).
		{config.ServiceBigQuery, func(a string) console.Provider {
			return bigqueryProvider{endpoint: a, project: d.cfg.DefaultProject(), registry: d.projects,
				savedQueries: filepath.Join(d.cfg.InstanceDir(), bqSavedQueriesFile)}
		}},
		// Job history, BigQuery's second screen (#993).
		{config.ServiceBigQuery, func(a string) console.Provider {
			return bigqueryJobsProvider{bq: bigqueryProvider{endpoint: a, project: d.cfg.DefaultProject()}}
		}},
	} {
		if !enabled[db.service] {
			continue
		}
		if addr := forwardedAddr(d.forwarders, string(db.service)); addr != "" {
			providers = append(providers, db.build(addr))
		}
	}

	// Cloud SQL's Instances page, through the Admin API (#1330).
	if sqlAdminEnabled(d.cfg) {
		var versions []string
		if enabled[config.ServiceCloudSQL] {
			versions = append(versions, sqladmin.EngineVersions("POSTGRES")...)
		}
		if enabled[config.ServiceCloudSQLMySQL] {
			// MYSQL_5_6 and MYSQL_5_7 (#1524) are served where the node
			// runs their linux/amd64 images; elsewhere a create is 501
			// saying why.
			versions = append(append(versions, "MYSQL_5_6", "MYSQL_5_7"), sqladmin.EngineVersions("MYSQL")...)
		}
		// SQL Server (#1916): created through the Admin API, on Microsoft's
		// image, once its user accepts Microsoft's licence.
		versions = append(versions, sqladmin.SQLServerVersions()...)
		host := d.cfg.BindAddress
		if host == "" || host == "0.0.0.0" || host == "::" {
			host = "127.0.0.1"
		}
		instances := cloudSQLInstancesProvider{
			endpoint: net.JoinHostPort(host, strconv.Itoa(d.cfg.Endpoints.SQLAdmin)), versions: versions}
		// Cloud SQL's Backups page (#1860), the project's backups.
		providers = append(providers, instances, cloudSQLBackupsProvider{instances: instances})
	}

	// The cluster views are read-only and always present: CloudBurrow owns
	// this cluster, and being able to see what is actually running in it is
	// the point of running one locally.
	// The AI area is always listed and never operational, so its absence is
	// explained rather than silently missing.
	providers = append(providers, aiProvider{})
	// Document AI and Speech-to-Text (#1930), on the local AI endpoint's
	// DocumentProcessorService and Speech v2 (#1799), when it runs.
	if d.localAI != nil {
		docs := docAI{defaultProject: d.cfg.DefaultProject(), addr: d.localAI.Addr}
		sp := speechSvc{defaultProject: d.cfg.DefaultProject(), addr: d.localAI.Addr}
		// New transcription saves to a workspace, a bucket of the served
		// Cloud Storage, as Google's does (#2084); Upload test document
		// opens its analysis page.
		transcriptions := speechTranscriptionsProvider{s: sp, held: &speechTranscriptions{}}
		if enabled[config.ServiceStorage] && storageAddr != "" {
			transcriptions.ws = speechWorkspaces{storage: storageProvider{endpoint: storageAddr}}
		}
		providers = append(providers, docAIOverviewProvider{d: docs}, docAIGalleryProvider{d: docs},
			docAIProcessorsProvider{d: docs, analyses: &docAIAnalyses{}},
			transcriptions, speechRecognizersProvider{s: sp},
			speechCustomClassesProvider{s: sp}, speechPhraseSetsProvider{s: sp})
	}

	kubeconfig := d.cfg.KubeconfigPath()
	// Kubernetes Engine's pages follow Google's (#1857): Workloads, the
	// Services and Ingress tabs and Storage's Persistent volume claims are
	// the GKE screens over the same cluster; Pods, Jobs, Nodes and Events
	// keep their pages, reached from a workload, a cluster's Nodes tab and
	// Object Browser.
	cluster, own := d.cfg.ClusterName(), d.cfg.Cluster.Namespace
	providers = append(providers,
		gkeClustersProvider{name: cluster, kubeconfig: kubeconfig, own: own, svc: d.gke, gkeDir: gkeDir(d.cfg)},
		gkeWorkloads(kubeconfig, cluster, own).withSeries(series),
		podsProvider(kubeconfig, metrics, series),
		gkeServices(kubeconfig, cluster, own),
		jobsProvider(kubeconfig),
		nodesProvider(kubeconfig),
		gkeClaims(kubeconfig, cluster, own),
		eventsProvider(kubeconfig),
		gkeConfig(kubeconfig, cluster, own),
		gkeGateways(kubeconfig, cluster, own),
		gkeIngress(kubeconfig, cluster, own),
		gkeStorageClasses(kubeconfig, cluster),
		gkeObjectBrowser{kubeconfig: kubeconfig},
	)
	return providers
}

// playgroundFor returns the playground configuration, which is empty unless
// local AI is running. An empty one is not an error state: the screen is
// simply not offered, and the rest of the console is unaffected.
func playgroundFor(d consoleDeps) *console.Playground {
	// An endpoint started only for custom prediction (#1193) has no model to
	// try, so it offers no playground.
	if d.localAI == nil || (d.localAI.Model() == "" && !d.cfg.LocalAI.Embeddings) {
		return nil
	}
	p := &console.Playground{
		// Resolved per request: the console is built before the endpoint
		// binds, so capturing the address here would capture an empty one.
		Addr:      d.localAI.Addr,
		Model:     d.localAI.Model(),
		Publisher: "unknown",
	}
	if d.cfg.LocalAI.Embeddings {
		p.EmbeddingModel = localai.EmbeddingGemmaONNXID
	}
	if m, err := localai.Lookup(p.Model); err == nil {
		p.Publisher = string(m.Publisher)
		p.Community = m.Publisher == localai.PublisherCommunity
	}
	return p
}

// predictionFor returns the Online prediction page's configuration (#869),
// which has no source unless Cloud Run is enabled: a prediction endpoint is a
// Cloud Run service, so without one the page is not offered.
func predictionFor(d consoleDeps) *console.Prediction {
	enabled := false
	for _, s := range d.cfg.EnabledServices() {
		enabled = enabled || s == config.ServiceRun
	}
	if !enabled {
		return nil
	}
	var ai func() string
	if d.localAI != nil {
		// Explain (#1454) is computed by the local AI endpoint's
		// PredictionService, which Cloud Run starts (#1193).
		ai = d.localAI.Addr
	}
	return &console.Prediction{
		AI: ai,
		Source: predictionSource{run: runProvider{
			kubeconfig:     d.cfg.KubeconfigPath(),
			namespace:      runadapter.WorkloadNamespace,
			runAddr:        runAddrOf(d.run),
			defaultProject: d.cfg.DefaultProject(),
		}},
		Ingress: d.ingress,
	}
}

// consoleStatus reports live instance state.
//
// Every field is read at request time from the thing that knows it. Nothing
// is cached, because a cached "ready" shown after a component failed is worse
// than a slow page.
func consoleStatus(d consoleDeps) console.StatusSource {
	return func(_ context.Context) console.Status {
		st := console.Status{
			Instance: d.cfg.Name,
			// The instance's own project, which is what the generated
			// credentials and the metadata server report. Selecting it is what
			// makes the console open on data instead of on "choose a project".
			DefaultProject: d.cfg.DefaultProject(),
			Cluster:        d.cfg.ClusterName(),
			Namespace:      d.cfg.Cluster.Namespace,
			Mode:           string(d.cfg.Mode),
			Endpoints:      map[string]string{},
			// The identity the instance actually issued, rather than a name
			// written into the HTML. Read from the same function that writes the
			// ADC fixture, so the account menu and the credentials on disk cannot
			// name different accounts.
			Identity: &console.Identity{
				ServiceAccount:  metadata.ServiceAccountEmail(d.cfg.DefaultProject()),
				Project:         d.cfg.DefaultProject(),
				CredentialsPath: filepath.Join(d.cfg.InstanceDir(), metadata.CredentialsFileName),
				// Never true. Sent as data rather than asserted in the UI's own
				// words, so the claim has one source.
				Authenticates: false,
			},
		}
		if d.coord != nil {
			st.Ready = d.coord.Ready()
			st.State = d.coord.State().String()
			names, ready := d.coord.SortedReady()
			st.Components = make(map[string]bool, len(names))
			for _, n := range names {
				st.Components[n] = ready[n]
			}
		}
		if d.cluster != nil {
			st.Kubernetes = d.cluster.ServerVersion()
		}

		for _, f := range d.forwarders {
			// Read now, not latched: a tunnel whose pod went away reports
			// not-running however well it started.
			st.Tunnels = append(st.Tunnels, console.TunnelStatus{
				Name:     trimForward(f.Name()),
				Host:     f.HostAddr(),
				Running:  f.Running(),
				Restarts: f.Restarts(),
			})
			if addr := f.HostAddr(); addr != "" {
				st.Endpoints[trimForward(f.Name())] = addr
			}
		}
		if d.cfg.Endpoints.Storage != 0 {
			st.Endpoints["storage"] = net.JoinHostPort(d.cfg.BindAddress,
				strconv.Itoa(d.cfg.Endpoints.Storage))
		}
		if d.tasks != nil {
			if addr := d.tasks.Addr(); addr != "" {
				st.Endpoints["tasks"] = addr
			}
		}
		if d.secrets != nil {
			if addr := d.secrets.Addr(); addr != "" {
				st.Endpoints["secretmanager"] = addr
			}
		}
		if addr := d.kms.Addr(); addr != "" {
			st.Endpoints["kms"] = addr
		}
		if addr := d.scheduler.Addr(); addr != "" {
			st.Endpoints["scheduler"] = addr
		}
		if d.metaAddr != nil {
			if addr := d.metaAddr(); addr != "" {
				st.Endpoints["metadata"] = addr
			}
		}
		if d.ingress != nil {
			if addr := d.ingress(); addr != "" {
				st.Endpoints["ingress"] = addr
			}
		}
		// The local AI endpoint (#1799), which Document AI's and
		// Speech-to-Text's pages read (#1930).
		if d.localAI != nil {
			if addr := d.localAI.Addr(); addr != "" {
				st.Endpoints["localai"] = addr
			}
		}

		// A service is listed whether or not it is enabled, and a disabled
		// one carries its reason: a greyed-out entry with no explanation is
		// the kind of thing a developer assumes is broken.
		for _, s := range config.KnownServices() {
			entry := console.ServiceStatus{ID: string(s), Title: serviceTitle(s)}
			for _, e := range d.cfg.EnabledServices() {
				if e == s {
					entry.Enabled = true
				}
			}
			if !entry.Enabled {
				if s.IsOptional() {
					entry.Reason = "opt-in; start with --services " + string(s)
				} else {
					entry.Reason = "not selected by --services"
				}
			}
			st.Services = append(st.Services, entry)
		}
		return st
	}
}

func trimForward(name string) string {
	const prefix = "forward:"
	if len(name) > len(prefix) && name[:len(prefix)] == prefix {
		return name[len(prefix):]
	}
	return name
}

// serviceTitles names every selectable service the way the Google Cloud
// console does. A service missing from here is listed on the dashboard by its
// config ID, which reads as a placeholder; TestEveryServiceHasAHumanTitle
// keeps the table complete.
var serviceTitles = map[config.Service]string{
	config.ServiceStorage:          "Cloud Storage",
	config.ServicePubSub:           "Pub/Sub",
	config.ServiceTasks:            "Cloud Tasks",
	config.ServiceRun:              "Cloud Run",
	config.ServiceSecrets:          "Secret Manager",
	config.ServiceFirestore:        "Firestore",
	config.ServiceDatastore:        "Datastore",
	config.ServiceBigtable:         "Bigtable",
	config.ServiceSpanner:          "Spanner",
	config.ServiceCloudSQL:         "Cloud SQL for PostgreSQL",
	config.ServiceBigQuery:         "BigQuery",
	config.ServiceMemorystore:      "Memorystore",
	config.ServiceCloudSQLMySQL:    "Cloud SQL for MySQL",
	config.ServiceScheduler:        "Cloud Scheduler",
	config.ServiceLogging:          "Cloud Logging",
	config.ServiceKMS:              "Cloud KMS",
	config.ServiceBackupDR:         "Backup and DR",
	config.ServicePrivateCA:        "Certificate Authority Service",
	config.ServiceGKE:              "Kubernetes Engine",
	config.ServiceMonitoring:       "Cloud Monitoring",
	config.ServiceCompute:          "VPC network",
	config.ServiceWorkflows:        "Workflows",
	config.ServiceParameterManager: "Parameter Manager",
	config.ServiceDataform:         "Dataform",
	config.ServiceAppHub:           "App Hub",
	config.ServiceTrace:            "Cloud Trace",
}

func serviceTitle(s config.Service) string {
	if t, ok := serviceTitles[s]; ok {
		return t
	}
	return string(s)
}

// printConsole reports where the console is, since a URL nobody is told about
// is a console nobody opens.
func printConsole(w io.Writer, srv *console.Server) {
	if srv == nil {
		return
	}
	if url := srv.URL(); url != "" {
		fmt.Fprintf(w, "  console:    %s\n", url)
	}
}

// forwardedAddr returns the host address of a named tunnel, or "".
func forwardedAddr(forwarders []*netfwd.Forwarder, name string) string {
	for _, f := range forwarders {
		if f.Name() == "forward:"+name {
			return f.HostAddr()
		}
	}
	return ""
}

// consoleSampler returns the component that keeps the metric history warm.
//
// Built here, where the console already is, rather than in up.go: the
// interval and the retention are console concerns and belong beside the
// server they configure.
func consoleSampler(srv *console.Server) *console.Sampler {
	if srv == nil {
		return nil
	}
	return console.NewSampler(srv, console.SampleInterval)
}
