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
	{config.ServiceStorage, serviceusage.API{Name: "storage.googleapis.com", Title: "Cloud Storage API"}},
	{config.ServicePubSub, serviceusage.API{Name: "pubsub.googleapis.com", Title: "Cloud Pub/Sub API"}},
	{config.ServiceTasks, serviceusage.API{Name: "cloudtasks.googleapis.com", Title: "Cloud Tasks API"}},
	{config.ServiceRun, serviceusage.API{Name: "run.googleapis.com", Title: "Cloud Run Admin API"}},
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
	{config.ServiceMemorystore, serviceusage.API{Name: "redis.googleapis.com", Title: "Google Cloud Memorystore for Redis API"}},
	{sqlAdminEndpoint, serviceusage.API{Name: "sqladmin.googleapis.com", Title: "Cloud SQL Admin API"}},
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
	return serviceusage.NewServer(net.JoinHostPort(cfg.BindAddress, strconv.Itoa(cfg.Endpoints.ServiceUsage)),
		served, unserved, st, serviceUsageProjects{reg: reg})
}
