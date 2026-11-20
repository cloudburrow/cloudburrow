package serviceusage

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"net/http"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/resource"
)

// services.generateServiceIdentity (Service Usage v1beta1,
// POST v1beta1/{parent=*/*/services/*}:generateServiceIdentity) "Generates
// service identity for service": the service's agent in the project, the
// Google-managed service account the service acts as there. `gcloud
// privateca roots create --kms-key-version/--bucket` calls it for the
// Certificate Authority Service agent and grants it roles on the key and
// the bucket (#1834), and `gcloud beta services identity create` calls it
// directly.
//
// The answer is a done Operation whose response is a ServiceIdentity
// {email, uniqueId}. Which agent that is, Google documents per service:
// "Primary service agents are service agents whose email address is
// returned when you trigger service agent creation"
// (https://cloud.google.com/iam/docs/service-account-types#service-agents),
// and https://cloud.google.com/iam/docs/service-agents marks each service's
// primary agent. A service with none answers a done operation whose
// ServiceIdentity is empty: gcloud's GenerateServiceIdentity documents "If
// service does not have a default identity, the response will be an empty
// dictionary", and `services identity create` then prints "Service identity
// created" with no email (googlecloudsdk/api_lib/services/serviceusage.py,
// surface/services/identity/create.py). A served service in neither table
// is 501 naming it, so a newly served service is looked up, not guessed.

// serviceAgents is the domain of each served service's primary service
// agent, service-PROJECT_NUMBER@DOMAIN, as
// https://cloud.google.com/iam/docs/service-agents lists it (the row marked
// "Primary service agent for SERVICE"); a second source is given where the
// product documents calling generateServiceIdentity itself.
var serviceAgents = map[string]string{
	// Private CA Service Account; gcloud privateca roots create --kms-key-version
	// and --bucket call this method for it and grant it roles on the key and bucket.
	"privateca.googleapis.com": "gcp-sa-privateca.iam.gserviceaccount.com",
	// Workflows Service Agent, the primary agent of workflows.googleapis.com.
	"workflows.googleapis.com": "gcp-sa-workflows.iam.gserviceaccount.com",
	// Kubernetes Engine Service Agent, the primary agent of
	// container.googleapis.com.
	"container.googleapis.com": "container-engine-robot.iam.gserviceaccount.com",
	// Cloud Monitoring's notification service agent (#1939).
	"monitoring.googleapis.com": "gcp-sa-monitoring-notification.iam.gserviceaccount.com",
	// Compute Engine Service Agent, the primary agent of
	// compute.googleapis.com (#1936).
	"compute.googleapis.com": "compute-system.iam.gserviceaccount.com",
	// External Key Management Service Service Account, Cloud KMS's primary
	// agent, not the Cloud KMS Service Agent (gcp-sa-cloudkms, which is not
	// primary): https://cloud.google.com/kms/docs/create-ekm-connection runs
	// `gcloud beta services identity create --service=cloudkms.googleapis.com`
	// to create service-PROJECT_NUMBER@gcp-sa-ekms.iam.gserviceaccount.com.
	"cloudkms.googleapis.com": "gcp-sa-ekms.iam.gserviceaccount.com",
	// Cloud Pub/Sub Service Account.
	"pubsub.googleapis.com": "gcp-sa-pubsub.iam.gserviceaccount.com",
	// Secret Manager Service Account; https://cloud.google.com/secret-manager/docs/cmek
	// creates it with `gcloud beta services identity create`.
	"secretmanager.googleapis.com": "gcp-sa-secretmanager.iam.gserviceaccount.com",
	// Cloud SQL Service Account; https://cloud.google.com/sql/docs/mysql/configure-cmek
	// creates it with `gcloud beta services identity create`.
	"sqladmin.googleapis.com": "gcp-sa-cloud-sql.iam.gserviceaccount.com",
	// Analytics Hub Service Agent (#1960); https://cloud.google.com/iam/docs/service-agents
	"analyticshub.googleapis.com": "gcp-sa-analyticshub.iam.gserviceaccount.com",
	// Cloud Tasks Service Account.
	"cloudtasks.googleapis.com": "gcp-sa-cloudtasks.iam.gserviceaccount.com",
	// BigQuery Data Transfer Service Agent (#1948); https://cloud.google.com/iam/docs/service-agents
	"bigquerydatatransfer.googleapis.com": "gcp-sa-bigquerydatatransfer.iam.gserviceaccount.com",
	// Cloud Scheduler Service Account.
	"cloudscheduler.googleapis.com": "gcp-sa-cloudscheduler.iam.gserviceaccount.com",
	// Google Cloud Run Service Agent.
	"run.googleapis.com": "serverless-robot-prod.iam.gserviceaccount.com",
	// Cloud Firestore Service Agent (Firestore's, not Datastore's: no row
	// names datastore.googleapis.com).
	"firestore.googleapis.com":     "gcp-sa-firestore.iam.gserviceaccount.com",
	"firebaserules.googleapis.com": "firebase-rules.iam.gserviceaccount.com",
	// Cloud Bigtable Service Agent, primary for bigtableadmin.googleapis.com
	// only; the data API bigtable.googleapis.com has none.
	"bigtableadmin.googleapis.com": "gcp-sa-bigtable.iam.gserviceaccount.com",
	// Cloud Spanner Production Service Account.
	"spanner.googleapis.com": "gcp-sa-spanner.iam.gserviceaccount.com",
	// Cloud Memorystore Redis Service Agent.
	"redis.googleapis.com": "cloud-redis.iam.gserviceaccount.com",
	// Backup and DR Service Agent.
	"backupdr.googleapis.com": "gcp-sa-backupdr.iam.gserviceaccount.com",
	// API Keys Service Account.
	"apikeys.googleapis.com": "gcp-sa-apikeys.iam.gserviceaccount.com",
	// Container Analysis Service Agent.
	"containeranalysis.googleapis.com": "container-analysis.iam.gserviceaccount.com",
	// Storage Insights Service Account (#1957).
	"storageinsights.googleapis.com": "gcp-sa-storageinsights.iam.gserviceaccount.com",
	// Container Scanning Service Agent.
	"containerscanning.googleapis.com": "gcp-sa-containerscanning.iam.gserviceaccount.com",
}

// noPrimaryAgent are the served services
// https://cloud.google.com/iam/docs/service-agents gives no primary
// service agent, so generateServiceIdentity names none for them.
var noPrimaryAgent = map[string]string{
	// The Storage Transfer Service Service Agent (project-NUMBER@storage-
	// transfer-service) is not primary; googleServiceAccounts.get names it
	// (#1957).
	"storagetransfer.googleapis.com": "Storage Transfer Service",
	// The page lists no agent of Storage Batch Operations (#1957).
	"storagebatchoperations.googleapis.com": "Storage Batch Operations",
	// The Workflows Service Agent is workflows.googleapis.com's; the
	// executions API has none of its own.
	"workflowexecutions.googleapis.com": "Workflow Executions",
	// Its agents (gcp-sa-logging, service-folder-/service-org-, pNUMBER-ID)
	// are not primary; Logging's CMEK names its account through
	// getCmekSettings instead.
	"logging.googleapis.com": "Cloud Logging",
	// The Google Storage Service Agent (gs-project-accounts) is not primary;
	// Cloud Storage names it through projects.serviceAccount.get.
	"storage.googleapis.com": "Cloud Storage",
	// The BigQuery Encryption Service Agent (bq-PROJECT_NUMBER@bigquery-encryption)
	// and BigQuery's other agents are not primary.
	"bigquery.googleapis.com":        "BigQuery",
	"bigquerystorage.googleapis.com": "BigQuery Storage",
	"datastore.googleapis.com":       "Datastore",
	"bigtable.googleapis.com":        "Bigtable data",
	// Its only agent is per organization (service-org-...), not primary.
	"cloudresourcemanager.googleapis.com": "Resource Manager",
	"serviceusage.googleapis.com":         "Service Usage",
}

// ServiceAgent is the email of svc's primary service agent in project, or
// "" for a service with none in serviceAgents.
func ServiceAgent(project, svc string) string {
	d, ok := serviceAgents[svc]
	if !ok {
		return ""
	}
	return fmt.Sprintf("service-%d@%s", resource.ProjectNumber(project), d)
}

// IdentityLookedUp reports whether generateServiceIdentity answers for svc
// from Google's documentation (an agent, or Google's empty answer) rather
// than 501.
func IdentityLookedUp(svc string) bool {
	_, agent := serviceAgents[svc]
	_, none := noPrimaryAgent[svc]
	return agent || none
}

// uniqueID is the agent's numeric unique ID, "The unique, stable numeric
// ID for the service account" (IAM's ServiceAccount.uniqueId): 21 digits,
// as https://cloud.google.com/iam/docs/service-accounts-create's example
// "123456789012345678901" is, the same for the same email every time.
func uniqueID(email string) string {
	h := sha256.Sum256([]byte(email))
	return fmt.Sprintf("1%020d", binary.BigEndian.Uint64(h[:8]))
}

const typePrefixV1beta1 = "type.googleapis.com/google.api.serviceusage.v1beta1."

// routeV1beta1 serves the one v1beta1 method CloudBurrow serves,
// generateServiceIdentity; every other v1beta1 path is 501, unsupported.
func (s *Server) routeV1beta1(r *http.Request, path string) (string, any, error) {
	const method = "serviceusage.services.generateServiceIdentity"
	p := strings.Split(strings.Trim(path, "/"), "/")
	last := p[len(p)-1]
	svc, verb, _ := strings.Cut(last, ":")
	if len(p) != 4 || p[2] != "services" || verb != "generateServiceIdentity" || r.Method != http.MethodPost {
		return "", nil, errorf(http.StatusNotImplemented, "UNIMPLEMENTED",
			"%s %s: CloudBurrow serves only services.generateServiceIdentity of Service Usage v1beta1; use v1", r.Method, r.URL.Path)
	}
	if p[0] != "projects" {
		return method, nil, errorf(http.StatusNotImplemented, "UNIMPLEMENTED",
			"generateServiceIdentity for %s: CloudBurrow serves service identities of projects only", p[0]+"/"+p[1])
	}
	if err := decode(r, &struct{}{}); err != nil {
		return method, nil, err
	}
	project, err := s.project(p[1])
	if err != nil {
		return method, nil, err
	}
	if _, err := s.lookup(svc); err != nil {
		return method, nil, err
	}
	resp := map[string]any{"@type": typePrefixV1beta1 + "ServiceIdentity"}
	if email := ServiceAgent(project, svc); email != "" {
		resp["email"], resp["uniqueId"] = email, uniqueID(email)
	} else if _, none := noPrimaryAgent[svc]; !none {
		return method, nil, errorf(http.StatusNotImplemented, "UNIMPLEMENTED",
			"generateServiceIdentity for %s: CloudBurrow has not looked up this service's primary service agent, so it does not guess one", svc)
	}
	// Google answers this method with its finished-operation sentinel,
	// not an operation operations.get can read.
	return method, map[string]any{"name": "operations/finished.DONE_OPERATION", "done": true, "response": resp}, nil
}
