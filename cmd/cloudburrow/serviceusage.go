package main

import (
	"net"
	"strconv"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/resource"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
	"github.com/cloudburrow/cloudburrow/internal/service/serviceusage"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// serviceUsageAPIs is every API CloudBurrow serves, by googleapis.com name,
// with the service that serves it (#1352). An empty service is always
// served.
var serviceUsageAPIs = []struct {
	service config.Service
	api     serviceusage.API
}{
	{"", serviceusage.API{Name: "cloudresourcemanager.googleapis.com", Title: "Cloud Resource Manager API"}},
	{"", serviceusage.API{Name: "serviceusage.googleapis.com", Title: "Service Usage API"}},
	{"", serviceusage.API{Name: "apikeys.googleapis.com", Title: "API Keys API"}},
	{config.ServiceStorage, serviceusage.API{Name: "storage.googleapis.com", Title: "Cloud Storage API"}},
	// The Cloud Storage companion APIs (#1957), served with Cloud Storage.
	{config.ServiceStorage, serviceusage.API{Name: "storagebatchoperations.googleapis.com", Title: "Storage Batch Operations API"}},
	{config.ServiceStorage, serviceusage.API{Name: "storageinsights.googleapis.com", Title: "Storage Insights API"}},
	{config.ServiceStorage, serviceusage.API{Name: "storagetransfer.googleapis.com", Title: "Storage Transfer API"}},
	{config.ServicePubSub, serviceusage.API{Name: "pubsub.googleapis.com", Title: "Cloud Pub/Sub API"}},
	{config.ServiceTasks, serviceusage.API{Name: "cloudtasks.googleapis.com", Title: "Cloud Tasks API"}},
	{config.ServiceRun, serviceusage.API{Name: "run.googleapis.com", Title: "Cloud Run Admin API"}},
	// Container Analysis and Artifact Registry's scanning (#1912), on the
	// Run port: the Container Scanning API is off until a project enables
	// it, as on Google, and turns on the scanning of pushed images.
	{config.ServiceRun, serviceusage.API{Name: "containeranalysis.googleapis.com", Title: "Container Analysis API"}},
	{config.ServiceRun, serviceusage.API{Name: "containerscanning.googleapis.com", Title: "Container Scanning API", Disabled: true}},
	{config.ServiceSecrets, serviceusage.API{Name: "secretmanager.googleapis.com", Title: "Secret Manager API"}},
	{config.ServiceKMS, serviceusage.API{Name: "cloudkms.googleapis.com", Title: "Cloud Key Management Service (KMS) API"}},
	{config.ServiceScheduler, serviceusage.API{Name: "cloudscheduler.googleapis.com", Title: "Cloud Scheduler API"}},
	{config.ServiceLogging, serviceusage.API{Name: "logging.googleapis.com", Title: "Cloud Logging API"}},
	{config.ServiceFirestore, serviceusage.API{Name: "firestore.googleapis.com", Title: "Cloud Firestore API"}},
	{config.ServiceDatastore, serviceusage.API{Name: "datastore.googleapis.com", Title: "Cloud Datastore API"}},
	{config.ServiceBigtable, serviceusage.API{Name: "bigtable.googleapis.com", Title: "Cloud Bigtable API"}},
	{config.ServiceBigtable, serviceusage.API{Name: "bigtableadmin.googleapis.com", Title: "Cloud Bigtable Admin API"}},
	{config.ServiceSpanner, serviceusage.API{Name: "spanner.googleapis.com", Title: "Cloud Spanner API"}},
	{config.ServiceBigQuery, serviceusage.API{Name: "bigquery.googleapis.com", Title: "BigQuery API"}},
	{config.ServiceBigQuery, serviceusage.API{Name: "bigquerystorage.googleapis.com", Title: "BigQuery Storage API"}},
	{config.ServiceBigQuery, serviceusage.API{Name: "bigquerydatatransfer.googleapis.com", Title: "BigQuery Data Transfer API"}},
	{config.ServiceBigQuery, serviceusage.API{Name: "analyticshub.googleapis.com", Title: "Analytics Hub API"}},
	{config.ServiceMemorystore, serviceusage.API{Name: "redis.googleapis.com", Title: "Google Cloud Memorystore for Redis API"}},
	{sqlAdminEndpoint, serviceusage.API{Name: "sqladmin.googleapis.com", Title: "Cloud SQL Admin API"}},
	{config.ServiceBackupDR, serviceusage.API{Name: "backupdr.googleapis.com", Title: "Backup and DR Service API"}},
	{config.ServicePrivateCA, serviceusage.API{Name: "privateca.googleapis.com", Title: "Certificate Authority API"}},
	{config.ServiceGKE, serviceusage.API{Name: "container.googleapis.com", Title: "Kubernetes Engine API"}},
	{config.ServiceMonitoring, serviceusage.API{Name: "monitoring.googleapis.com", Title: "Cloud Monitoring API"}},
	{config.ServiceCompute, serviceusage.API{Name: "compute.googleapis.com", Title: "Compute Engine API"}},
	{config.ServiceWorkflows, serviceusage.API{Name: "workflows.googleapis.com", Title: "Workflows API"}},
	{config.ServiceWorkflows, serviceusage.API{Name: "workflowexecutions.googleapis.com", Title: "Workflow Executions API"}},
}

// serviceUsageCatalog splits serviceUsageAPIs into what this instance
// serves and what it could serve with other --services, with the reason.
func serviceUsageCatalog(cfg config.Config) ([]serviceusage.API, []serviceusage.Unserved) {
	var served []serviceusage.API
	var unserved []serviceusage.Unserved
	for _, e := range serviceUsageAPIs {
		on := e.service == "" || serviceEnabled(cfg, e.service)
		why := "start the instance with --services " + string(e.service)
		if e.service == sqlAdminEndpoint {
			on = sqlAdminEnabled(cfg)
			why = "start the instance with --services cloudsql or cloudsql-mysql"
		}
		if e.service == config.ServiceBackupDR {
			on = backupDREnabled(cfg)
			why = "start the instance with --services backupdr and cloudsql or cloudsql-mysql"
		}
		if on {
			served = append(served, e.api)
		} else {
			unserved = append(unserved, serviceusage.Unserved{Name: e.api.Name, Reason: why})
		}
	}
	return served, unserved
}

// serviceUsageProjects resolves a project by ID, or by the number every
// CloudBurrow service reports for it, from the registry.
type serviceUsageProjects struct{ reg *resourcemanager.Registry }

func (p serviceUsageProjects) Resolve(s string) (string, error) {
	if s == "" || strings.ContainsAny(s, "/\\") {
		return "", serviceUsageNotFound(s)
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		// A project ID: every CloudBurrow service accepts any project.
		return s, nil
	}
	if p.reg != nil {
		list, err := p.reg.List()
		if err != nil {
			return "", err
		}
		for _, pr := range list {
			if resource.ProjectNumber(pr.ProjectID) == n {
				return pr.ProjectID, nil
			}
		}
	}
	return "", serviceUsageNotFound(s)
}

func serviceUsageNotFound(s string) error {
	return serviceusage.PermissionDenied("Project %s not found or permission denied.", s)
}

// newServiceUsageServer serves the Service Usage API (#1352) over st, the
// project registry's store, so its records persist with the instance's.
func newServiceUsageServer(cfg config.Config, reg *resourcemanager.Registry, st store.Store) *serviceusage.Server {
	if cfg.Endpoints.ServiceUsage < 0 {
		return nil
	}
	served, unserved := serviceUsageCatalog(cfg)
	srv := serviceusage.NewServer(net.JoinHostPort(cfg.BindAddress, strconv.Itoa(cfg.Endpoints.ServiceUsage)),
		served, unserved, st, serviceUsageProjects{reg: reg})
	srv.Dependents = serviceUsageDependents
	return srv
}

// serviceUsageDependents are the dependencies Google documents among the
// APIs CloudBurrow serves (#1914), by the API depended on. Google's
// documented dependents of the BigQuery API (Binary Authorization, Google
// Kubernetes Engine, Google Cloud APIs, Dataprep, Open Service Broker,
// Telecom Data Fabric: https://docs.cloud.google.com/bigquery/docs/enable-assets)
// are none of them served, and no other dependency among the served APIs
// is documented, so the table is empty; services.disable's
// disableDependentServices rules apply to whatever is added here.
var serviceUsageDependents = map[string][]string{}
