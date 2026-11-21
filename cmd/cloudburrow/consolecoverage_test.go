package main

import (
	"github.com/cloudburrow/cloudburrow/internal/service/backupdr"

	"os"
	"slices"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
	"github.com/cloudburrow/cloudburrow/internal/service/vertexai"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// consoleCoverage is where each selectable service is in the console, which
// is what console parity §3 records: "the console covers exactly what
// CloudBurrow supports". Every service in config.KnownServices() has exactly
// one entry, and TestEveryServiceHasAParityRow holds it to the registry and
// to §3, so a service cannot be enabled, titled on the dashboard and have no
// screen without a documented reason.
type consoleCoverage struct {
	// Providers are the IDs of the screens built for the service.
	Providers []string
	// Screen is a console screen that is not a provider but shows the
	// service's data, such as the Logs Explorer for Cloud Logging, beside
	// any Providers.
	Screen string
	// Excluded is why the service has no screen at all. §3 carries the same
	// reason in the service's row.
	Excluded string
}

var consoleCoverages = map[config.Service]consoleCoverage{
	config.ServiceStorage: {Providers: []string{"storage", "storage-deleted", "storage-settings", "storage-intelligence",
		"storage-batch-operations", "storage-inventory-reports", "storage-insights-datasets", "storage-transfer",
		"storage-transfer-agent-pools"}},
	config.ServicePubSub:        {Providers: []string{"pubsub", "pubsub-subscriptions", "pubsub-snapshots", "pubsub-schemas"}},
	config.ServiceTasks:         {Providers: []string{"tasks"}},
	config.ServiceRun:           {Providers: []string{"run", "run-jobs", "run-worker-pools", "run-domains", "run-instances", "run-builds", "cloud-build-triggers", "cloud-build-worker-pools", "cloud-build-dashboard", "cloud-build-repositories", "cloud-build-settings", "secure-source-manager", "artifact-registry", "artifact-registry-settings", "functions", "eventarc", "eventarc-buses", "eventarc-pipelines", "eventarc-enrollments", "eventarc-google-api-sources", "eventarc-channels", "vertex-models", "vertex-endpoints"}},
	config.ServiceSecrets:       {Providers: []string{"secrets", "secrets-regional"}},
	config.ServiceFirestore:     {Providers: []string{"firestore", "firestore-databases"}},
	config.ServiceDatastore:     {Providers: []string{"datastore"}},
	config.ServiceBigtable:      {Providers: []string{"bigtable", "bigtable-instances"}},
	config.ServiceSpanner:       {Providers: []string{"spanner"}},
	config.ServiceCloudSQL:      {Providers: []string{"cloudsql", "cloudsql-instances", "cloudsql-backups"}},
	config.ServiceBigQuery:      {Providers: []string{"bigquery", "bigquery-jobs", "bigquery-transfers", "bigquery-scheduled-queries", "bigquery-sharing"}},
	config.ServiceScheduler:     {Providers: []string{"scheduler"}},
	config.ServiceKMS:           {Providers: []string{"kms", "kms-ekm", "kms-autokey", "kms-config", "kms-inventory", "kms-hsm"}},
	config.ServiceLogging:       {Providers: []string{"logging-metrics", "logging-sinks", "logging-exclusions", "logging-buckets", "logging-settings", "logging-saved-queries", "logging-log-scopes", "logging-analytics"}, Screen: "/logs"},
	config.ServiceMemorystore:   {Providers: []string{"memorystore", "memorystore-clusters", "memorystore-valkey"}},
	config.ServiceCloudSQLMySQL: {Providers: []string{"cloudsql-mysql", "cloudsql-instances", "cloudsql-backups"}},
	config.ServiceBackupDR: {Providers: []string{"backupdr-overview", "backupdr-vaulted", "backupdr-plans", "backupdr-associations",
		"backupdr", "backupdr-jobs"}},
	config.ServicePrivateCA:        {Providers: []string{"privateca", "privateca-templates"}},
	config.ServiceWorkflows:        {Providers: []string{"workflows"}},
	config.ServiceParameterManager: {Providers: []string{"parametermanager", "parametermanager-templates"}},
	config.ServiceAppHub:           {Providers: []string{"apphub", "apphub-discovered", "apphub-settings"}},
	// The Kubernetes Engine API (#1913): its clusters are on Kubernetes
	// clusters, beside the instance's own.
	config.ServiceGKE: {Providers: []string{"gkeclusters"}},
	config.ServiceMonitoring: {Providers: []string{"monitoring-explorer", "monitoring-metrics", "monitoring-alerting", "monitoring-incidents",
		"monitoring-channels", "monitoring-uptime", "monitoring-dashboards", "monitoring-groups", "monitoring-services", "monitoring-snoozes"}},
	// VPC network (#1936): the Compute Engine API's networking.
	config.ServiceCompute: {Providers: []string{"vpc-networks", "vpc-firewalls", "vpc-routes", "vpc-addresses",
		"compute-instances", "compute-disks", "compute-snapshots", "compute-images", "compute-machinetypes", "compute-metadata"}},
}

// clusterScreens are the registry's screens that belong to no selectable
// service: Resource Manager, the AI catalogue, Document AI and
// Speech-to-Text on the local AI endpoint (#1930), and CloudBurrow's own
// cluster.
var clusterScreens = []string{
	"projects", "hierarchy", "tags", "iam", "iam-roles", "iam-groups", "iam-sso-profiles", "iam-sso-assignments", "iam-service-accounts", "iam-workload-identity", "iam-workforce-identity", "iam-audit", "iam-orgpolicies", "iam-labels", "iam-settings", "serviceusage", "serviceusage-operations", "ai",
	"documentai-overview", "documentai-gallery", "documentai", "speech-transcriptions", "speech-recognizers",
	"speech-custom-classes", "speech-phrase-sets",
	"workloads", "pods", "k8sservices", "jobs", "nodes", "k8sstorage", "events",
	"gkeclusters", "k8sconfig", "k8sgateways", "k8singress", "k8sstorageclasses", "k8sobjects",
}

// TestEveryServiceHasAParityRow enforces console parity §3 (#698): "The
// console covers exactly what CloudBurrow supports."
//
// The dashboard titles every service in config.KnownServices() from
// serviceTitles and shows an enabled one as enabled, so a service with a
// working backend and no screen read as "not implemented" — BigQuery and
// Memorystore were exactly that, with no §3 row either. Here every service
// must be one of: built by the registry (with every service enabled and every
// tunnel present), shown on a non-provider screen, or excluded with a reason;
// and §3 must have a row titled as the dashboard titles it, which says
// "**No screen**" exactly when the service is excluded.
func TestEveryServiceHasAParityRow(t *testing.T) {
	d := coverageDeps(t)
	registry := map[string]bool{}
	for _, p := range consoleProviders(d, clusterMetrics(d.cfg.KubeconfigPath()), console.NewSeries(console.SeriesLimit, nil)) {
		registry[p.ID()] = true
	}

	rows := paritySection3(t)
	claimed := map[string]bool{}
	for _, s := range config.KnownServices() {
		title, ok := serviceTitles[s]
		if !ok {
			t.Errorf("%s has no entry in serviceTitles", s)
			continue
		}
		cov, ok := consoleCoverages[s]
		if !ok {
			t.Errorf("%s (%s) has no entry in consoleCoverages: give it a screen or a documented exclusion", s, title)
			continue
		}
		// Providers and a Screen may go together (Cloud Logging: its admin
		// screens and the Logs Explorer, #1226); Excluded goes alone.
		if shown := len(cov.Providers) > 0 || cov.Screen != ""; shown == (cov.Excluded != "") {
			t.Errorf("%s: set Providers or Screen, or else Excluded, got %+v", s, cov)
		}
		for _, id := range cov.Providers {
			claimed[id] = true
			if !registry[id] {
				t.Errorf("%s claims the %q screen, which consoleProviders does not build with every service enabled", s, id)
			}
		}
		if cov.Excluded != "" && registry[string(s)] {
			t.Errorf("%s is excluded, but the registry builds a %q screen", s, s)
		}

		row, ok := rows[title]
		if !ok {
			t.Errorf("console-parity.md §3 has no row for %q (%s)", title, s)
			continue
		}
		noScreen := strings.HasPrefix(row, "**No screen**")
		switch {
		case cov.Excluded != "" && !noScreen:
			t.Errorf("§3 row %q must begin **No screen**, since %s is excluded: %s", title, s, cov.Excluded)
		case cov.Excluded == "" && noScreen:
			t.Errorf("§3 row %q says **No screen**, but %s has one", title, s)
		}
	}
	for id := range registry {
		if !claimed[id] && !slices.Contains(clusterScreens, id) {
			t.Errorf("the registry builds %q, which no service in consoleCoverages claims", id)
		}
	}
}

// coverageDeps is an instance with every service enabled and a tunnel for
// each, so every screen the registry can build is built. Nothing is served:
// the registry is only constructed, never read.
func coverageDeps(t *testing.T) consoleDeps {
	t.Helper()
	cfg := config.Config{
		Name: "coverage", Project: "coverage", BindAddress: "127.0.0.1",
		StateDir: t.TempDir(), Mode: config.ModeEphemeral,
		Services: config.KnownServices(),
		// The Cloud Storage companion APIs' screens (#1957) are built for
		// their bound ports.
		Endpoints: config.Endpoints{StorageBatchOperations: 1, StorageInsights: 1, StorageTransfer: 1},
	}
	projects := resourcemanager.New(store.NewMemory())
	d := consoleDeps{
		cfg:              cfg,
		projects:         projects,
		tasks:            &tasksService{},
		secrets:          &secretsService{},
		kms:              &kmsService{},
		privateCA:        &privateCAService{},
		monitoring:       &monitoringService{},
		compute:          &computeService{},
		dataTransfer:     &dataTransferSvc{},
		analyticsHub:     &analyticsHubSvc{},
		scheduler:        &schedulerService{},
		logging:          &loggingService{},
		workflows:        &workflowsService{},
		parameterManager: &parameterManagerService{},
		appHub:           &appHubService{},
		// The APIs & Services page (#1352) reads the Service Usage server.
		serviceUsage: newServiceUsageServer(cfg, projects, store.NewMemory()),
		// The Backup and DR page (#1433) reads the Backup and DR server.
		backupdr: backupdr.NewServer("127.0.0.1:0", nil, ""),
		// Document AI and Speech-to-Text (#1930) read the local AI
		// endpoint, never started here.
		localAI: vertexai.NewServerOn("127.0.0.1:0", nil),
	}
	for _, s := range config.KnownServices() {
		d.forwarders = append(d.forwarders, namedForwarder(t, string(s), "127.0.0.1:1"))
	}
	return d
}

// paritySection3 returns §3's table as the "In scope" cell of each row, keyed
// by every name in its Area cell ("Firestore / Datastore" is two).
func paritySection3(t *testing.T) map[string]string {
	t.Helper()
	b, err := os.ReadFile("../../docs/console-parity.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(b)
	start := strings.Index(doc, "\n## 3.")
	if start < 0 {
		t.Fatal("console-parity.md has no §3")
	}
	section := doc[start+1:]
	if end := strings.Index(section, "\n## "); end >= 0 {
		section = section[:end]
	}
	rows := map[string]string{}
	for _, line := range strings.Split(section, "\n") {
		if !strings.HasPrefix(line, "| ") {
			continue
		}
		cells := strings.Split(strings.Trim(line, "|"), " | ")
		if len(cells) < 2 || strings.TrimSpace(cells[0]) == "Area" {
			continue
		}
		for _, name := range strings.Split(cells[0], " / ") {
			rows[strings.TrimSpace(name)] = strings.TrimSpace(cells[1])
		}
	}
	if len(rows) == 0 {
		t.Fatal("console-parity.md §3 has no table rows")
	}
	return rows
}
