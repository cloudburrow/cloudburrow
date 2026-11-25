package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/unserved"
)

// `cloudburrow terraform` (#283): run Terraform or OpenTofu with the Google
// provider pointed at this instance.
//
// It writes a provider configuration next to the module, runs the binary with
// the remaining arguments, and removes the file however the run ends. Only
// services whose Terraform support is Verified in docs/compatibility.md get
// an endpoint; the rest are named in a warning rather than silently sent to
// real Google. A module declaring resources it cannot point here at all
// (unpointedResources, #1331) is refused outright.

// terraformEndpoint is one provider endpoint attribute.
type terraformEndpoint struct {
	service config.Service
	attr    string // the google provider's *_custom_endpoint argument
	env     string // the environment variable the provider reads it from
	path    string // appended to http://host:port
	// verified is true only for rows docs/compatibility.md marks Verified
	// with a test behind them. `env --format terraform` reads this table too.
	verified bool
	// regional names the provider's base paths that carry {{location}}
	// (Cloud Run v1's is https://{{location}}-run.googleapis.com/): on
	// loopback the host is {{location}}-<regional>.localhost, which the
	// provider fills in per resource and which resolves to loopback, so the
	// server reads the region from the host as it does for gcloud.
	regional string
}

var terraformEndpoints = []terraformEndpoint{
	{config.ServiceStorage, "storage_custom_endpoint", "GOOGLE_STORAGE_CUSTOM_ENDPOINT", "/storage/v1/", true, ""},
	// The Storage Control API's REST mapping (#1519), on the storage port:
	// google_storage_control_project_, _folder_ and
	// _organization_intelligence_config apply, plan clean, change and
	// destroy in TestTerraformStorageIntelligenceConfigs.
	{config.ServiceStorage, "storage_control_custom_endpoint", "GOOGLE_STORAGE_CONTROL_CUSTOM_ENDPOINT", "/v2/", true, ""},
	{config.ServicePubSub, "pubsub_custom_endpoint", "GOOGLE_PUBSUB_CUSTOM_ENDPOINT", "/v1/", true, ""},
	// Cloud Tasks (#366): a queue and its *_iam_member apply, plan clean and
	// destroy through the JSON API in TestTerraformAppliesAndDestroysThroughTheWrapper.
	{config.ServiceTasks, "cloud_tasks_custom_endpoint", "GOOGLE_CLOUD_TASKS_CUSTOM_ENDPOINT", "/v2/", true, ""},
	// Secret Manager (#365): a secret and its *_iam_member apply, plan clean
	// and destroy in TestTerraformAppliesAndDestroysThroughTheWrapper.
	{config.ServiceSecrets, "secret_manager_custom_endpoint", "GOOGLE_SECRET_MANAGER_CUSTOM_ENDPOINT", "/v1/", true, ""},
	// Regional secrets (#1519): the provider's base path is
	// secretmanager.{{location}}.rep.googleapis.com, and the resources'
	// URLs carry projects/{project}/locations/{location}, so the one port
	// serves every location. google_secret_manager_regional_secret, its
	// _version and _iam_member, _binding and _policy apply, plan clean,
	// update and destroy in TestTerraformRegionalSecret.
	{config.ServiceSecrets, "secret_manager_regional_custom_endpoint", "GOOGLE_SECRET_MANAGER_REGIONAL_CUSTOM_ENDPOINT", "/v1/", true, ""},
	{config.ServiceRun, "cloud_run_v2_custom_endpoint", "GOOGLE_CLOUD_RUN_V2_CUSTOM_ENDPOINT", "/v2/", true, ""},
	// Cloud Run v1 (#1293, #1304), the Knative-style API on the same port:
	// google_cloud_run_domain_mapping applies, becomes Ready, plans clean
	// and destroys in TestTerraformCloudRunDomainMapping. Its URLs are
	// regional, {{location}}-run.googleapis.com, and a v1 write needs a
	// region, so the host is {{location}}-run.localhost on loopback.
	{config.ServiceRun, "cloud_run_custom_endpoint", "GOOGLE_CLOUD_RUN_CUSTOM_ENDPOINT", "/", true, "run"},
	// Artifact Registry and Cloud Build v1 (#1304) are served on the Run
	// port beside Cloud Run (#1347, SourceDeployHandler).
	// google_artifact_registry_repository applies, plans clean, updates in
	// place and destroys in TestTerraformArtifactRegistryRepository;
	// google_cloudbuild_trigger (manual or webhook) and _worker_pool do in
	// TestTerraformCloudBuildTriggerAndWorkerPool (#1422).
	{config.ServiceRun, "artifact_registry_custom_endpoint", "GOOGLE_ARTIFACT_REGISTRY_CUSTOM_ENDPOINT", "/v1/", true, ""},
	{config.ServiceRun, "cloud_build_custom_endpoint", "GOOGLE_CLOUD_BUILD_CUSTOM_ENDPOINT", "/v1/", true, ""},
	// Cloud Build repositories, 2nd gen (#2197), at Google's /v2/ paths on
	// the Run port: google_cloudbuildv2_connection (a GitLab host at the
	// instance's git server) and google_cloudbuildv2_repository apply, plan
	// clean and destroy in TestTerraformCloudBuildV2.
	{config.ServiceRun, "cloudbuildv2_custom_endpoint", "GOOGLE_CLOUDBUILDV2_CUSTOM_ENDPOINT", "/v2/", true, ""},
	// Secure Source Manager (#1521, #1527), on the Run port under its
	// prefix: google_secure_source_manager_instance, _repository and their
	// *_iam_member (stored, never enforced) apply, plan clean, update and
	// destroy in TestTerraformSecureSourceManager.
	{config.ServiceRun, "secure_source_manager_custom_endpoint", "GOOGLE_SECURE_SOURCE_MANAGER_CUSTOM_ENDPOINT", "/securesourcemanager/v1/", true, ""},
	// Developer Connect (#1546, #1304), on the Run port under its prefix:
	// google_developer_connect_git_repository_link applies, plans clean and
	// destroys, and a google_developer_connect_connection imported from the
	// official client updates in place and destroys, in
	// TestTerraformDeveloperConnect.
	{config.ServiceRun, "developer_connect_custom_endpoint", "GOOGLE_DEVELOPER_CONNECT_CUSTOM_ENDPOINT", "/developerconnect/v1/", true, ""},
	{config.ServiceRun, "eventarc_custom_endpoint", "GOOGLE_EVENTARC_CUSTOM_ENDPOINT", "/eventarc/v1/", true, ""},
	// Cloud Functions v2 (#1377), on the Run port: a function is a
	// buildpacks build and a Cloud Run service. google_cloudfunctions2_function
	// applies, plans clean, updates in place and destroys in
	// TestTerraformCloudFunctions2Function, gated as the Functions tests are.
	{config.ServiceRun, "cloudfunctions2_custom_endpoint", "GOOGLE_CLOUDFUNCTIONS2_CUSTOM_ENDPOINT", "/v2/", true, ""},
	// Cloud Functions v1 (#1428), on the same port over the same machinery:
	// google_cloudfunctions_function reaches this instance, never Google:
	// TestTerraformCloudFunctionsFunction applies an HTTP and an event
	// function, gated as the Functions tests are.
	{config.ServiceRun, "cloud_functions_custom_endpoint", "GOOGLE_CLOUD_FUNCTIONS_CUSTOM_ENDPOINT", "/v1/", true, ""},
	// Cloud KMS (#425): a key ring, a key and a version apply, plan clean,
	// update their labels and destroy in TestTerraformKMS, over the JSON API
	// (#422-#424). The provider's discovery client strips the version from
	// the base path and appends v1/ itself, so both styles land on /v1/.
	{config.ServiceKMS, "kms_custom_endpoint", "GOOGLE_KMS_CUSTOM_ENDPOINT", "/v1/", true, ""},
	// Cloud Scheduler (#591): a google_cloud_scheduler_job applies, plans
	// clean, updates (a PATCH, then :pause) and destroys in
	// TestTerraformSchedulerAndSubscription, over the JSON API on the gRPC
	// port.
	{config.ServiceScheduler, "cloud_scheduler_custom_endpoint", "GOOGLE_CLOUD_SCHEDULER_CUSTOM_ENDPOINT", "/v1/", true, ""},
	// BigQuery (#1236): a dataset, a table, a view and a routine, with the
	// table, view and routine *_iam_member, *_iam_binding and *_iam_policy
	// (ADR-0006, stored, never enforced), apply, plan clean and destroy in
	// TestTerraformBigQueryIAM, through the front on the REST port. The
	// provider's argument is big_query_, not bigquery_.
	{config.ServiceBigQuery, "big_query_custom_endpoint", "GOOGLE_BIG_QUERY_CUSTOM_ENDPOINT", "/bigquery/v2/", true, ""},
	// Cloud Logging (#1246): a google_logging_metric, a
	// google_logging_project_sink and a google_logging_project_exclusion
	// apply, plan clean, update and destroy in TestTerraformLogging, over the
	// JSON API on the gRPC port (#591), against the metrics, sinks and
	// exclusions #1226 serves. Buckets, views, links and settings stay 501.
	{config.ServiceLogging, "logging_custom_endpoint", "GOOGLE_LOGGING_CUSTOM_ENDPOINT", "/v2/", true, ""},
	// The Observability API (#2117), observability.googleapis.com v1 on the
	// Logging port: google_observability_project_settings, _folder_settings,
	// _organization_settings, _bucket, _link and _trace_scope apply, plan
	// clean, update and destroy in TestTerraformObservability2117.
	{config.ServiceLogging, "observability_custom_endpoint", "GOOGLE_OBSERVABILITY_CUSTOM_ENDPOINT", "/v1/", true, ""},
	// google_project (#301): Resource Manager v1, and the Cloud Billing read
	// the provider makes on every refresh, both served on the Resource
	// Manager port.
	// The Cloud SQL Admin API (#1301): a google_sql_database and a
	// google_sql_user on an instance `up` runs apply, plan clean and destroy
	// in TestTerraformSQLDatabaseAndUser. The provider calls v1beta4.
	{sqlAdminEndpoint, "sql_custom_endpoint", "GOOGLE_SQL_CUSTOM_ENDPOINT", "/sql/v1beta4/", true, ""},
	// The Backup and DR Service (#1433): google_backup_dr_backup_vault,
	// _backup_plan and _backup_plan_association apply, plan clean and
	// destroy in TestTerraformBackupDR.
	{config.ServiceBackupDR, "backup_dr_custom_endpoint", "GOOGLE_BACKUP_DR_CUSTOM_ENDPOINT", "/v1/", true, ""},
	// Certificate Authority Service (#1834): google_privateca_ca_pool,
	// _certificate_authority (root and subordinate), _certificate_template
	// and _certificate apply, plan clean and destroy in
	// TestTerraformPrivateCA.
	{config.ServicePrivateCA, "privateca_custom_endpoint", "GOOGLE_PRIVATECA_CUSTOM_ENDPOINT", "/v1/", true, ""},
	// google_workflows_workflow (#1986): the Workflows API v1 on its own
	// port. TestTerraformWorkflowsWorkflow applies one, plans clean,
	// updates its source in place and destroys it.
	{config.ServiceWorkflows, "workflows_custom_endpoint", "GOOGLE_WORKFLOWS_CUSTOM_ENDPOINT", "/v1/", true, ""},
	// Parameter Manager (#2054): google_parameter_manager_parameter and
	// _parameter_version, and their regional twins, whose base path is
	// parametermanager.{{location}}.rep.googleapis.com, on one port.
	// TestTerraformParameterManager applies them, plans clean and destroys.
	{config.ServiceParameterManager, "parameter_manager_custom_endpoint", "GOOGLE_PARAMETER_MANAGER_CUSTOM_ENDPOINT", "/v1/", true, ""},
	{config.ServiceParameterManager, "parameter_manager_regional_custom_endpoint", "GOOGLE_PARAMETER_MANAGER_REGIONAL_CUSTOM_ENDPOINT", "/v1/", true, ""},
	// Dataform (#2105): google_dataform_repository and its release and
	// workflow configs. TestTerraformDataformRepository applies them, plans
	// clean and destroys.
	{config.ServiceDataform, "dataform_custom_endpoint", "GOOGLE_DATAFORM_CUSTOM_ENDPOINT", "/v1/", true, ""},
	// App Hub (#2110): google_apphub_application, _service and
	// _service_project_attachment, and the google_apphub_discovered_service
	// data source. TestTerraformAppHub applies them, plans clean and
	// destroys.
	{config.ServiceAppHub, "apphub_custom_endpoint", "GOOGLE_APPHUB_CUSTOM_ENDPOINT", "/v1/", true, ""},
	// AlloyDB for PostgreSQL (#2191): google_alloydb_cluster, _instance,
	// _user and _backup. TestTerraformAlloyDB applies them, plans clean and
	// destroys.
	{config.ServiceAlloyDB, "alloydb_custom_endpoint", "GOOGLE_ALLOYDB_CUSTOM_ENDPOINT", "/v1/", true, ""},
	// The Kubernetes Engine API (#1913): a google_container_cluster with
	// remove_default_node_pool and a google_container_node_pool apply, plan
	// clean, update in place and destroy in
	// TestTerraformContainerClusterAndNodePool.
	{config.ServiceGKE, "container_custom_endpoint", "GOOGLE_CONTAINER_CUSTOM_ENDPOINT", "/v1/", true, ""},
	// The BigQuery Data Transfer Service (#1948): a scheduled query's
	// google_bigquery_data_transfer_config applies, plans clean, updates in
	// place and destroys in TestTerraformBigQueryDataTransferConfig.
	{dataTransferService, "bigquery_data_transfer_custom_endpoint", "GOOGLE_BIGQUERY_DATA_TRANSFER_CUSTOM_ENDPOINT", "/v1/", true, ""},
	// The Firebase Rules API (#2048), served with Firestore: a
	// google_firebaserules_ruleset and a google_firebaserules_release
	// apply, plan clean, change and destroy in TestTerraformFirebaseRules.
	{firebaseRulesService, "firebaserules_custom_endpoint", "GOOGLE_FIREBASERULES_CUSTOM_ENDPOINT", "/v1/", true, ""},
	// BigQuery sharing (#1960): google_bigquery_analytics_hub_data_exchange,
	// _listing and _listing_subscription apply, plan clean, update in place
	// and destroy in TestTerraformBigQueryAnalyticsHub.
	{analyticsHubService, "bigquery_analytics_hub_custom_endpoint", "GOOGLE_BIGQUERY_ANALYTICS_HUB_CUSTOM_ENDPOINT", "/v1/", true, ""},
	// Compute Engine, for the one read google_container_node_pool makes of
	// it: a pool's node_count is its instance group managers' targetSize,
	// which the Kubernetes Engine port answers for its own pools' groups
	// (internal/service/gke/compute.go), and every other Compute call 501,
	// so a google_compute_* resource never reaches Google either. With
	// the compute service enabled the next row sets it instead.
	{config.ServiceGKE, "compute_custom_endpoint", "GOOGLE_COMPUTE_CUSTOM_ENDPOINT", "/compute/v1/", true, ""},
	// Filestore (#2192): google_filestore_instance, _snapshot and _backup
	// apply, plan clean, update in place and destroy in
	// TestTerraformFilestore.
	{config.ServiceFilestore, "filestore_custom_endpoint", "GOOGLE_FILESTORE_CUSTOM_ENDPOINT", "/v1/", true, ""},
	// Cloud Monitoring (#1939): google_monitoring_metric_descriptor.
	{config.ServiceMonitoring, "monitoring_custom_endpoint", "GOOGLE_MONITORING_CUSTOM_ENDPOINT", "/", true, ""},
	// The Compute Engine API's VPC networking (#1936), on its own port,
	// which also answers the node pools' instance group managers:
	// google_compute_network and google_compute_subnetwork apply, plan
	// clean, update and destroy in TestTerraformComputeNetworkAndSubnetwork.
	{config.ServiceCompute, "compute_custom_endpoint", "GOOGLE_COMPUTE_CUSTOM_ENDPOINT", "/compute/v1/", true, ""},
	// Vertex AI (#1940), on the local AI endpoint, served with Cloud Run:
	// google_vertex_ai_endpoint applies, is read back through the official
	// client, plans clean, updates its labels and destroys in
	// TestTerraformVertexAIEndpoint.
	{localAIEndpoint, "vertex_ai_custom_endpoint", "GOOGLE_VERTEX_AI_CUSTOM_ENDPOINT", "/v1/", true, ""},
	// Firestore (#1331): google_firestore_document applies, plans clean,
	// updates its fields and destroys in TestTerraformFirestoreDocument,
	// over the documents REST API the emulator serves on its port. On the
	// same port the call front serves the Firestore Admin API itself
	// (internal/firestoreadmin): google_firestore_database, _index and
	// _field apply, plan clean, update and destroy in
	// TestTerraformFirestoreDatabaseIndexAndField; _backup_schedule answers
	// 501, locally, never at Google.
	{config.ServiceFirestore, "firestore_custom_endpoint", "GOOGLE_FIRESTORE_CUSTOM_ENDPOINT", "/v1/", true, ""},
	// Spanner (#1331): google_spanner_instance and _database apply, read
	// back through the official admin clients, plan clean, update in place
	// and destroy in TestTerraformSpannerInstanceAndDatabase, over the
	// emulator's REST API, Spanner's second port (Endpoints.SpannerREST),
	// which the call front in its pod serves.
	{config.ServiceSpanner, "spanner_custom_endpoint", "GOOGLE_SPANNER_CUSTOM_ENDPOINT", "/v1/", true, ""},
	// Bigtable's admin REST API (#1440), on the Bigtable port beside gRPC
	// (internal/bigtablefront/rest.go): google_bigtable_app_profile and
	// google_bigtable_instance_iam_member, _binding and _policy apply, plan
	// clean, update and destroy in TestTerraformBigtableAppProfileAndIAM.
	// The provider's instance, table and GC policy resources use the gRPC
	// clients, which BIGTABLE_EMULATOR_HOST points here (bigtableHost).
	{config.ServiceBigtable, "bigtable_custom_endpoint", "GOOGLE_BIGTABLE_CUSTOM_ENDPOINT", "/v2/", true, ""},
	// Memorystore for Redis (#1302): a google_redis_instance applies, is
	// reached with its AUTH string, plans clean, updates its size and labels
	// in place and destroys in TestTerraformRedisInstance, over the admin
	// API's JSON on its port (Endpoints.MemorystoreAdmin), each instance a
	// server of its own, of its redis_version's image.
	{memorystoreAdminEndpoint, "redis_custom_endpoint", "GOOGLE_REDIS_CUSTOM_ENDPOINT", "/v1/", true, ""},
	// Memorystore for Valkey (#1519): google_memorystore_instance applies,
	// plans clean, changes and destroys in TestTerraformMemorystoreInstance,
	// over memorystore.googleapis.com's JSON, served under /memorystore/ on
	// the same port.
	{memorystoreAdminEndpoint, "memorystore_custom_endpoint", "GOOGLE_MEMORYSTORE_CUSTOM_ENDPOINT", "/memorystore/v1/", true, ""},
	// google_project_service (#1352): the Service Usage API v1 on its own
	// port, in TestTerraformProjectService.
	{serviceUsageService, "service_usage_custom_endpoint", "GOOGLE_SERVICE_USAGE_CUSTOM_ENDPOINT", "/v1/", true, ""},
	// google_apikeys_key (#1915): the API Keys API v2 on its own port.
	// TestTerraformAPIKeysKey applies one with restrictions, plans clean,
	// updates it in place and destroys it.
	{apiKeysService, "apikeys_custom_endpoint", "GOOGLE_APIKEYS_CUSTOM_ENDPOINT", "/v2/", true, ""},
	// google_storage_transfer_job and google_storage_insights_report_config
	// (#1957), on their own ports beside Cloud Storage.
	// TestTerraformStorageTransferJob and
	// TestTerraformStorageInsightsReportConfig apply, plan clean, update
	// and destroy them.
	{storageTransferService, "storage_transfer_custom_endpoint", "GOOGLE_STORAGE_TRANSFER_CUSTOM_ENDPOINT", "/v1/", true, ""},
	{storageInsightsService, "storage_insights_custom_endpoint", "GOOGLE_STORAGE_INSIGHTS_CUSTOM_ENDPOINT", "/v1/", true, ""},
	{resourceManagerEndpoint, "resource_manager_custom_endpoint", "GOOGLE_RESOURCE_MANAGER_CUSTOM_ENDPOINT", "/v1/", true, ""},
	// google_project_iam_custom_role and google_organization_iam_custom_role
	// (#1743): the IAM API's roles, on the Resource Manager port, in
	// TestTerraformIAMCustomRole. Provider 8.4.0's IAM client for them takes
	// its base from iam_beta_custom_endpoint (its log names the path it
	// uses), and ignores iam_custom_endpoint; both point here, so neither
	// reaches Google.
	{resourceManagerEndpoint, "iam_custom_endpoint", "GOOGLE_IAM_CUSTOM_ENDPOINT", "/v1/", true, ""},
	{resourceManagerEndpoint, "iam_beta_custom_endpoint", "GOOGLE_IAM_BETA_CUSTOM_ENDPOINT", "/v1/", true, ""},
	// Cloud Billing and the Budget API (#2109), on the Resource Manager
	// port: google_billing_project_info's base is core_billing's,
	// google_billing_budget's billing's (billingbudgets.googleapis.com),
	// google_project's billing read cloud_billing's.
	// TestTerraformBillingProjectInfoAndBudget applies both, plans clean,
	// updates the budget in place and destroys them.
	{resourceManagerEndpoint, "cloud_billing_custom_endpoint", "GOOGLE_CLOUD_BILLING_CUSTOM_ENDPOINT", "/v1/", true, ""},
	{resourceManagerEndpoint, "core_billing_custom_endpoint", "GOOGLE_CORE_BILLING_CUSTOM_ENDPOINT", "/v1/", true, ""},
	{resourceManagerEndpoint, "billing_custom_endpoint", "GOOGLE_BILLING_CUSTOM_ENDPOINT", "/v1/", true, ""},
	{resourceManagerEndpoint, "billing_budgets_custom_endpoint", "GOOGLE_BILLING_BUDGETS_CUSTOM_ENDPOINT", "/v1/", true, ""},
	// google_cloud_quotas_quota_preference and the Site Verification
	// resources (#2030), on the Resource Manager port.
	// TestTerraformCloudQuotasPreference and TestTerraformSiteVerification
	// apply them and plan clean.
	{resourceManagerEndpoint, "cloud_quotas_custom_endpoint", "GOOGLE_CLOUD_QUOTAS_CUSTOM_ENDPOINT", "/v1/", true, ""},
	{resourceManagerEndpoint, "site_verification_custom_endpoint", "GOOGLE_SITE_VERIFICATION_CUSTOM_ENDPOINT", "/siteVerification/v1/", true, ""},
	// google_folder (#1294): Resource Manager v3 Folders, on the same port,
	// in TestTerraformGoogleFolder.
	{resourceManagerEndpoint, "resource_manager_v3_custom_endpoint", "GOOGLE_RESOURCE_MANAGER_V3_CUSTOM_ENDPOINT", "/v3/", true, ""},
	// google_resource_manager_capability (#1519): v3 folders.capabilities,
	// which the provider reaches through its resource_manager3_ endpoint, in
	// TestTerraformResourceManagerCapability.
	{resourceManagerEndpoint, "resource_manager3_custom_endpoint", "GOOGLE_RESOURCE_MANAGER3_CUSTOM_ENDPOINT", "/v3/", true, ""},
	// google_tags_tag_key, _tag_value and _tag_binding (#1295): the tags
	// services are Resource Manager v3, which the provider reaches through
	// its own tags_ endpoint, in TestTerraformTagsAndLien.
	{resourceManagerEndpoint, "tags_custom_endpoint", "GOOGLE_TAGS_CUSTOM_ENDPOINT", "/v3/", true, ""},
	// google_tags_location_tag_binding (#1578): regional tag bindings, which
	// the provider sends to {{location}}-cloudresourcemanager.googleapis.com
	// by default; the one v3 server binds any location's resource, in
	// TestTerraformTagsLocationTagBinding.
	{resourceManagerEndpoint, "tags_location_custom_endpoint", "GOOGLE_TAGS_LOCATION_CUSTOM_ENDPOINT", "/v3/", true, ""},
	// The IAM and identity APIs the Resource Manager port serves beside
	// those (#1578), each with the provider's own endpoint argument:
	// Cloud Identity groups (#1812), google_cloud_identity_group and
	// _group_membership in TestTerraformCloudIdentityGroupAndMembership;
	// the Organization Policy API v2 (#1890), google_org_policy_policy and
	// _custom_constraint in TestTerraformOrgPolicyPolicyAndCustomConstraint;
	// IAM v2 deny policies, google_iam_deny_policy in
	// TestTerraformIAMDenyPolicy; IAM v3 principal access boundary policies
	// and their bindings (#2057), google_iam_principal_access_boundary_policy
	// and google_iam_projects_policy_binding in
	// TestTerraformIAMPrincipalAccessBoundary; workforce pools, whose
	// provider takes iam_workforce_pool_custom_endpoint, not iam_'s,
	// google_iam_workforce_pool and _provider in
	// TestTerraformIAMWorkforcePool. Until now each of these was refused
	// locally (unserved) though the instance serves it.
	{resourceManagerEndpoint, "cloud_identity_custom_endpoint", "GOOGLE_CLOUD_IDENTITY_CUSTOM_ENDPOINT", "/v1/", true, ""},
	{resourceManagerEndpoint, "org_policy_custom_endpoint", "GOOGLE_ORG_POLICY_CUSTOM_ENDPOINT", "/v2/", true, ""},
	{resourceManagerEndpoint, "iam2_custom_endpoint", "GOOGLE_IAM2_CUSTOM_ENDPOINT", "/v2/", true, ""},
	{resourceManagerEndpoint, "iam3_custom_endpoint", "GOOGLE_IAM3_CUSTOM_ENDPOINT", "/v3/", true, ""},
	{resourceManagerEndpoint, "iam_workforce_pool_custom_endpoint", "GOOGLE_IAM_WORKFORCE_POOL_CUSTOM_ENDPOINT", "/v1/", true, ""},
	// IAM Credentials (#1747), on the metadata port:
	// data.google_service_account_access_token mints a token for a
	// service account in TestTerraformServiceAccountAccessToken.
	{metadataEndpoint, "iam_credentials_custom_endpoint", "GOOGLE_IAM_CREDENTIALS_CUSTOM_ENDPOINT", "/v1/", true, ""},
	// Storage Batch Operations (#1957), on its own port beside Cloud
	// Storage: google_storage_batch_operations_job in
	// TestTerraformStorageBatchOperationsJob.
	{storageBatchService, "storage_batch_operations_custom_endpoint", "GOOGLE_STORAGE_BATCH_OPERATIONS_CUSTOM_ENDPOINT", "/v1/", true, ""},
	// Container Analysis (#1912), on the Run port under
	// /containeranalysis/ as gcloud's override is:
	// google_container_analysis_note and _occurrence in
	// TestTerraformContainerAnalysisNoteAndOccurrence.
	{config.ServiceRun, "container_analysis_custom_endpoint", "GOOGLE_CONTAINER_ANALYSIS_CUSTOM_ENDPOINT", "/containeranalysis/v1/", true, ""},
	// Network Connectivity's service connection policies (#2141), on the
	// Compute Engine port: google_network_connectivity_service_connection_policy
	// in TestTerraformNetworkConnectivityServiceConnectionPolicy.
	// The policy's base path is network_connectivityv1_'s; hubs, spokes and
	// the rest are network_connectivity_'s, which the same port answers
	// UNIMPLEMENTED, locally.
	{config.ServiceCompute, "network_connectivity_custom_endpoint", "GOOGLE_NETWORK_CONNECTIVITY_CUSTOM_ENDPOINT", "/v1/", true, ""},
	{config.ServiceCompute, "network_connectivityv1_custom_endpoint", "GOOGLE_NETWORK_CONNECTIVITYV1_CUSTOM_ENDPOINT", "/v1/", true, ""},
	// Document AI (#1953), on the local AI endpoint: the provider's base
	// path is https://{{location}}-documentai.googleapis.com/v1/, and the
	// resources' URLs carry projects/{project}/locations/{location}, so the
	// one port serves us and eu. google_document_ai_processor and
	// google_document_ai_processor_default_version apply, plan clean and
	// destroy in TestTerraformDocumentAIProcessorDefaultVersion.
	{localAIEndpoint, "document_ai_custom_endpoint", "GOOGLE_DOCUMENT_AI_CUSTOM_ENDPOINT", "/v1/", true, ""},
}

// localAIEndpoint (gcloudsetup.go) keys the local AI endpoint's rows here
// too, Vertex AI's (#1940) and Document AI's (#1953): the provider is given
// them whenever the endpoint is served, with a model, with embeddings or
// with Cloud Run (localAIServed).

// resourceManagerEndpoint keys the Resource Manager rows. It is not a
// selectable service: the project registry, and so its API, always runs.
const resourceManagerEndpoint config.Service = "resourcemanager"

// metadataEndpoint keys the IAM Credentials row (#1578): the metadata
// server, which every instance runs, serves it (#1747).
const metadataEndpoint config.Service = "metadata"

// sqlAdminEndpoint keys the Cloud SQL Admin API row: it is served when
// either Cloud SQL service is (#1301), and is not a service of its own.
const sqlAdminEndpoint config.Service = "sqladmin"

// memorystoreAdminEndpoint keys the Memorystore admin API's rows (#1302),
// apart from the RESP server's "memorystore": it is the CLI's own port,
// published to pods as cloudburrow-host's "memorystore-admin".
const memorystoreAdminEndpoint config.Service = "memorystore-admin"

// terraformAccessToken is what the provider presents. CloudBurrow
// authenticates nothing, so any token works for its own APIs; a fixed one
// keeps the provider from looking for credentials of its own.
//
// It is "owner" (#1331) because the Firestore emulator does read the token:
// any other bearer must be a JWT, or it answers 400 "invalid jwt". "owner"
// is the emulator's documented admin token, the one the official clients
// send it when FIRESTORE_EMULATOR_HOST is set. It is no Google credential.
const terraformAccessToken = "owner"

// generatedMarker heads every file this writes, so one left by a killed run
// is recognised as ours and a user's file of the same name never is.
const generatedMarker = "# Generated by `cloudburrow terraform`. Removed when it exits; do not edit."

// terraformURL is one endpoint the provider is given.
type terraformURL struct {
	terraformEndpoint
	url string
}

// terraformSettings resolves the table against an instance: the endpoints to
// set, and the enabled services deliberately left out. Both the provider file
// and `env --format terraform` are rendered from this, so they cannot differ.
func terraformSettings(cfg config.Config) ([]terraformURL, []config.Service) {
	host := func(port int) string { return net.JoinHostPort(cfg.BindAddress, strconv.Itoa(port)) }
	ports := map[config.Service]int{
		config.ServiceStorage: cfg.Endpoints.Storage, config.ServicePubSub: cfg.Endpoints.PubSub,
		config.ServiceTasks: cfg.Endpoints.Tasks, config.ServiceSecrets: cfg.Endpoints.Secrets, config.ServiceRun: cfg.Endpoints.Run,
		config.ServiceKMS: cfg.Endpoints.KMS, resourceManagerEndpoint: cfg.Endpoints.ResourceManager,
		serviceUsageService:     cfg.Endpoints.ServiceUsage,
		apiKeysService:          cfg.Endpoints.APIKeys,
		config.ServiceScheduler: cfg.Endpoints.Scheduler, config.ServiceLogging: cfg.Endpoints.Logging,
		config.ServiceBigQuery: cfg.Endpoints.BigQuery, sqlAdminEndpoint: cfg.Endpoints.SQLAdmin,
		config.ServiceFirestore: cfg.Endpoints.Firestore,
		// The REST port: the provider's Spanner resources use REST, the
		// client libraries the gRPC one.
		config.ServiceSpanner: cfg.Endpoints.SpannerREST,
		// The front serves the admin REST API on the gRPC port.
		config.ServiceBigtable: cfg.Endpoints.Bigtable,
		// The admin API's port, not the RESP one.
		memorystoreAdminEndpoint:       cfg.Endpoints.MemorystoreAdmin,
		config.ServiceBackupDR:         cfg.Endpoints.BackupDR,
		config.ServicePrivateCA:        cfg.Endpoints.PrivateCA,
		config.ServiceGKE:              cfg.Endpoints.GKE,
		analyticsHubService:            cfg.Endpoints.AnalyticsHub,
		config.ServiceMonitoring:       cfg.Endpoints.Monitoring,
		config.ServiceCompute:          cfg.Endpoints.Compute,
		config.ServiceFilestore:        cfg.Endpoints.Filestore,
		config.ServiceWorkflows:        cfg.Endpoints.Workflows,
		config.ServiceParameterManager: cfg.Endpoints.ParameterManager,
		config.ServiceDataform:         cfg.Endpoints.Dataform,
		config.ServiceAppHub:           cfg.Endpoints.AppHub,
		config.ServiceAlloyDB:          cfg.Endpoints.AlloyDB,
		localAIEndpoint:                cfg.Endpoints.LocalAI,
		storageTransferService:         cfg.Endpoints.StorageTransfer,
		storageInsightsService:         cfg.Endpoints.StorageInsights,
		dataTransferService:            cfg.Endpoints.DataTransfer,
		firebaseRulesService:           cfg.Endpoints.FirebaseRules,
		storageBatchService:            cfg.Endpoints.StorageBatchOperations,
		metadataEndpoint:               cfg.Endpoints.Metadata,
	}
	enabled := map[config.Service]bool{resourceManagerEndpoint: true, metadataEndpoint: true, serviceUsageService: true, apiKeysService: true, sqlAdminEndpoint: sqlAdminEnabled(cfg),
		memorystoreAdminEndpoint: serviceEnabled(cfg, config.ServiceMemorystore)}
	for _, s := range cfg.EnabledServices() {
		enabled[s] = true
	}
	enabled[config.ServiceBackupDR] = backupDREnabled(cfg)
	enabled[config.ServicePrivateCA] = privateCAEnabled(cfg)
	enabled[analyticsHubService] = analyticsHubEnabled(cfg)
	enabled[config.ServiceMonitoring] = monitoringEnabled(cfg)
	enabled[config.ServiceWorkflows] = workflowsEnabled(cfg)
	enabled[config.ServiceParameterManager] = parameterManagerEnabled(cfg)
	enabled[config.ServiceDataform] = dataformEnabled(cfg)
	enabled[config.ServiceAppHub] = appHubEnabled(cfg)
	enabled[config.ServiceAlloyDB] = alloyDBEnabled(cfg)
	enabled[localAIEndpoint] = localAIServed(cfg)
	enabled[storageTransferService] = storageJobsEnabled(cfg)
	enabled[storageInsightsService] = storageJobsEnabled(cfg)
	enabled[storageBatchService] = storageJobsEnabled(cfg)
	enabled[dataTransferService] = dataTransferEnabled(cfg)
	enabled[firebaseRulesService] = firebaseRulesEnabled(cfg)
	var set []terraformURL
	var skipped []config.Service
	for _, e := range terraformEndpoints {
		if !enabled[e.service] {
			continue
		}
		// Compute Engine's own port, when served, answers what the GKE
		// port's stub does (#1936).
		if e.service == config.ServiceGKE && e.attr == "compute_custom_endpoint" && enabled[config.ServiceCompute] && ports[config.ServiceCompute] != 0 {
			continue
		}
		if !e.verified || ports[e.service] == 0 {
			skipped = append(skipped, e.service)
			continue
		}
		addr := host(ports[e.service])
		if ip := net.ParseIP(cfg.BindAddress); e.regional != "" && ip != nil && ip.IsLoopback() {
			addr = net.JoinHostPort("{{location}}-"+e.regional+".localhost", strconv.Itoa(ports[e.service]))
		}
		set = append(set, terraformURL{e, "http://" + addr + e.path})
	}
	return set, skipped
}

// terraformProvider renders the google provider's configuration and names
// the enabled services it deliberately left out.
func terraformProvider(cfg config.Config, override bool) (string, []config.Service) {
	set, skipped := terraformSettings(cfg)
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n# Instance %q.\n", generatedMarker, cfg.Name)
	b.WriteString(terraformProviderBlock(cfg, "google", override, set))
	return b.String(), skipped
}

// terraformProviderBlock is one provider block: the project, the token, the
// endpoints set at this instance, and the refusals the provider reads from
// no environment variable (unserved.TerraformNoEnv), each limited to the
// arguments name's pinned schema has. Every other refusal is passed in the
// environment (terraformRefusalEnv), which a provider of another version
// ignores where it has no such argument, rather than failing on it.
func terraformProviderBlock(cfg config.Config, name string, override bool, set []terraformURL) string {
	return terraformProviderBlockAuth(cfg, name, override, set, terraformOwnerAuth)
}

// terraformProviderBlockAuth is terraformProviderBlock with auth as the
// provider's credentials (terraformauth.go).
func terraformProviderBlockAuth(cfg config.Config, name string, override bool, set []terraformURL, auth terraformAuth) string {
	has := map[string]bool{}
	for _, a := range unserved.TerraformEndpoints(name == "google-beta") {
		has[a] = true
	}
	var b strings.Builder
	fmt.Fprintf(&b, "provider %q {\n", name)
	fmt.Fprintf(&b, "  project      = %q\n", cfg.DefaultProject())
	if auth.credentials != "" {
		fmt.Fprintf(&b, "  credentials  = %q\n", auth.credentials)
		if override {
			// The provider refuses access_token alongside credentials.
			b.WriteString("  access_token = null\n")
		}
	} else {
		fmt.Fprintf(&b, "  access_token = %q\n", auth.accessToken)
		if override {
			// An override merges into the module's own block, and the provider
			// refuses credentials alongside access_token.
			b.WriteString("  credentials  = null\n")
		}
	}
	for _, e := range set {
		if name == "google" || has[e.attr] {
			fmt.Fprintf(&b, "  %s = %q\n", e.attr, e.url)
		}
	}
	for _, r := range terraformRefusals(cfg, set) {
		if has[r.attr] && unserved.TerraformNoEnv[r.attr] {
			fmt.Fprintf(&b, "  %s = %q # not served here: refused locally (#1975)\n", r.attr, r.url)
		}
	}
	b.WriteString("}\n")
	return b.String()
}

// terraformRefusals are the local refusals (#1975) for every endpoint
// argument of the pinned google and google-beta providers this instance
// does not point at itself: without one the provider would send that
// API's calls to its googleapis.com default.
func terraformRefusals(cfg config.Config, set []terraformURL) []terraformURL {
	local := map[string]bool{}
	for _, e := range set {
		local[e.attr] = true
	}
	base := refusalBase(cfg)
	var out []terraformURL
	for _, a := range unserved.AllTerraformEndpoints() {
		if !local[a] {
			out = append(out, terraformURL{terraformEndpoint{attr: a, env: unserved.TerraformEnv(a)}, unserved.URL(base, a)})
		}
	}
	return out
}

// terraformRefusalEnv is the environment the wrapper adds for the binary it
// runs: every refusal the provider reads from an environment variable. Both
// google and google-beta read the same variables, so a module using either
// is covered without a block of its own.
func terraformRefusalEnv(cfg config.Config) []string {
	set, _ := terraformSettings(cfg)
	var env []string
	for _, r := range terraformRefusals(cfg, set) {
		if !unserved.TerraformNoEnv[r.attr] {
			env = append(env, r.env+"="+r.url)
		}
	}
	return env
}

// refusalBase is the server the refusals point at: the Resource Manager
// port, which every instance serves and which answers them (unserved.Handler).
// An instance without one still gets a loopback URL, which then fails to
// connect, locally.
func refusalBase(cfg config.Config) string {
	port := cfg.Endpoints.ResourceManager
	if port == 0 {
		port = 1
	}
	return "http://" + net.JoinHostPort(cfg.BindAddress, strconv.Itoa(port))
}

var googleBetaReference = regexp.MustCompile(`google-beta`)

// moduleUsesGoogleBeta reports whether any *.tf file of the module names
// google-beta: a provider block, a required_providers entry or a
// resource's provider argument. Its block is written only then, since a
// block for a provider the module does not use would make init install it.
func moduleUsesGoogleBeta(dir string) bool {
	files, _ := filepath.Glob(filepath.Join(dir, "*.tf"))
	for _, f := range files {
		if strings.HasPrefix(filepath.Base(f), "cloudburrow_providers") {
			continue
		}
		if b, err := os.ReadFile(f); err == nil && googleBetaReference.Match(b) {
			return true
		}
	}
	return false
}

// unpointedPrefix is a family of the provider's resources and data sources
// the wrapper cannot point at the instance (#1331).
type unpointedPrefix struct{ prefix, why string }

// unpointedResources are the families the provider would send to real
// Google for this instance, whatever is enabled, so the wrapper refuses to
// run a module that declares one rather than warn and let it through.
//
// Firestore, Spanner and Bigtable, and Artifact Registry and Cloud Build,
// when the instance does not serve them (Spanner's REST port among it),
// since nothing then points them here; Cloud Build v2 and the
// google_storage_buckets, google_projects and google_folders data sources
// always.
func unpointedResources(cfg config.Config) []unpointedPrefix {
	var out []unpointedPrefix
	set, _ := terraformSettings(cfg)
	firestore, spanner := false, false
	for _, e := range set {
		firestore = firestore || e.service == config.ServiceFirestore
		spanner = spanner || e.service == config.ServiceSpanner
	}
	if !firestore {
		out = append(out, unpointedPrefix{"google_firestore_", "this instance does not serve Firestore"})
	}
	if !spanner {
		out = append(out, unpointedPrefix{"google_spanner_", "this instance does not serve Spanner's REST API"})
	}
	if bigtableHost(cfg) == "" {
		out = append(out, unpointedPrefix{"google_bigtable_", "this instance does not serve Bigtable"})
	}
	run, docai := false, false
	for _, e := range set {
		run = run || e.service == config.ServiceRun
		docai = docai || e.service == localAIEndpoint
	}
	if !docai {
		out = append(out, unpointedPrefix{"google_document_ai_", "this instance does not serve Document AI (it is on the local AI endpoint)"})
	}
	if !run {
		out = append(out,
			unpointedPrefix{"google_cloud_run_", "this instance does not serve Cloud Run"},
			unpointedPrefix{"google_artifact_registry_", "this instance does not serve Artifact Registry (it is on the Run port)"},
			unpointedPrefix{"google_cloudbuild_", "this instance does not serve Cloud Build (it is on the Run port)"},
			unpointedPrefix{"google_cloudbuildv2_", "this instance does not serve Cloud Build repositories (they are on the Run port)"},
			unpointedPrefix{"google_secure_source_manager_", "this instance does not serve Secure Source Manager (it is on the Run port)"},
			unpointedPrefix{"google_developer_connect_", "this instance does not serve Developer Connect (it is on the Run port)"},
			unpointedPrefix{"google_cloudfunctions2_", "this instance does not serve Cloud Functions (it is on the Run port)"},
			unpointedPrefix{"google_cloudfunctions_", "this instance does not serve Cloud Functions (it is on the Run port)"})
	}
	// Whatever is enabled: Cloud Functions v2 (#1377) and v1 (#1428) are served on the Run port.
	// Three list data sources call a fixed Google URL whatever the
	// provider's custom endpoints say (measured with hashicorp/google 8.4.0,
	// #1304): google_storage_buckets lists at
	// https://storage.googleapis.com/storage/v1/b, google_projects at
	// https://cloudresourcemanager.googleapis.com/v1/projects and
	// google_folders at https://cloudresourcemanager.googleapis.com/v3/folders,
	// so no setting points them here. Three more do the same (#1578, read
	// in 8.4.0's source and unchanged on the provider's main branch,
	// 2026-11-18): google_cloud_run_locations lists at
	// https://run.googleapis.com/v1/projects/{{project}}/locations,
	// google_artifact_registry_locations at
	// https://artifactregistry.googleapis.com/v1/projects/{{project}}/locations
	// and google_iam_testable_permissions posts to
	// https://iam.googleapis.com/v1/permissions:queryTestablePermissions.
	// Before #1578 the wrapper let them through, to real Google, whenever the
	// Run port or Resource Manager was served.
	out = append(out,
		unpointedPrefix{"google_storage_buckets", "the provider lists buckets at storage.googleapis.com, ignoring storage_custom_endpoint"},
		unpointedPrefix{"google_projects", "the provider lists projects at cloudresourcemanager.googleapis.com, ignoring resource_manager_custom_endpoint"},
		unpointedPrefix{"google_folders", "the provider lists folders at cloudresourcemanager.googleapis.com, ignoring resource_manager_v3_custom_endpoint"},
		unpointedPrefix{"google_cloud_run_locations", "the provider lists Cloud Run locations at run.googleapis.com, ignoring cloud_run_custom_endpoint"},
		unpointedPrefix{"google_artifact_registry_locations", "the provider lists Artifact Registry locations at artifactregistry.googleapis.com, ignoring artifact_registry_custom_endpoint"},
		unpointedPrefix{"google_iam_testable_permissions", "the provider posts to iam.googleapis.com, ignoring iam_custom_endpoint"})
	return out
}

// bigtableHost is the address the provider's Bigtable clients are given, or
// "" when the instance does not serve Bigtable. The provider's admin and
// data clients are the Go bigtable clients, which read
// BIGTABLE_EMULATOR_HOST and then dial it in plaintext, without
// credentials, as they dial the emulator for any program; the wrapper sets
// it for the binary it runs.
func bigtableHost(cfg config.Config) string {
	for _, s := range cfg.EnabledServices() {
		if s == config.ServiceBigtable && cfg.Endpoints.Bigtable != 0 {
			return net.JoinHostPort(cfg.BindAddress, strconv.Itoa(cfg.Endpoints.Bigtable))
		}
	}
	return ""
}

var declaredResource = regexp.MustCompile(`(?m)^\s*(?:resource|data)\s+"(google_[a-z0-9_]+)"`)

// unpointed names the resources and data sources in the module's *.tf files
// that would reach real Google, each with the reason.
func unpointed(dir string, families []unpointedPrefix) []string {
	files, _ := filepath.Glob(filepath.Join(dir, "*.tf"))
	seen := map[string]bool{}
	var out []string
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, m := range declaredResource.FindAllSubmatch(b, -1) {
			typ := string(m[1])
			for _, u := range families {
				if strings.HasPrefix(typ, u.prefix) && !seen[typ] {
					seen[typ] = true
					out = append(out, typ+" ("+u.why+")")
				}
			}
		}
	}
	return out
}

// offlineSubcommands never call a provider's API, so a module with an
// unpointed resource may still run them.
var offlineSubcommands = map[string]bool{"init": true, "fmt": true, "validate": true, "version": true, "providers": true, "get": true}

// subcommand is the first argument that is not a global flag.
func subcommand(theirs []string) string {
	for _, a := range theirs {
		if !strings.HasPrefix(a, "-") {
			return a
		}
	}
	return ""
}

// moduleDeclaresGoogle reports whether the module already configures the
// google provider. An override file needs a block to merge into, and a plain
// one would duplicate an existing block, so which file is written depends on
// the answer.
func moduleDeclaresGoogle(dir string) bool { return moduleDeclaresProvider(dir, "google") }

// moduleDeclaresProvider reports whether the module has a provider block
// for name.
func moduleDeclaresProvider(dir, name string) bool {
	block := regexp.MustCompile(`(?m)^\s*provider\s+"` + regexp.QuoteMeta(name) + `"\s*\{`)
	files, _ := filepath.Glob(filepath.Join(dir, "*.tf"))
	for _, f := range files {
		if strings.HasPrefix(filepath.Base(f), "cloudburrow_providers") {
			continue
		}
		if b, err := os.ReadFile(f); err == nil && block.Match(b) {
			return true
		}
	}
	return false
}

// terraformProviderFiles are the files the wrapper writes into dir, by
// name: a plain file for the providers the module does not configure and
// an override file for those it does (Terraform refuses an override with no
// block to merge into), each carrying generatedMarker. google is always
// written; google-beta only when the module uses it (#1975: before, a
// google-beta resource got no endpoint at all and went to real Google).
func terraformProviderFiles(cfg config.Config, dir string) (map[string]string, []config.Service) {
	return terraformProviderFilesAuth(cfg, dir, terraformOwnerAuth)
}

// terraformProviderFilesAuth is terraformProviderFiles with auth as the
// providers' credentials.
func terraformProviderFilesAuth(cfg config.Config, dir string, auth terraformAuth) (map[string]string, []config.Service) {
	set, skipped := terraformSettings(cfg)
	providers := []string{"google"}
	if moduleUsesGoogleBeta(dir) {
		providers = append(providers, "google-beta")
	}
	var plain, over strings.Builder
	for _, p := range providers {
		if moduleDeclaresProvider(dir, p) {
			over.WriteString(terraformProviderBlockAuth(cfg, p, true, set, auth))
		} else {
			plain.WriteString(terraformProviderBlockAuth(cfg, p, false, set, auth))
		}
	}
	head := fmt.Sprintf("%s\n# Instance %q.\n", generatedMarker, cfg.Name)
	files := map[string]string{}
	if plain.Len() > 0 {
		files["cloudburrow_providers.tf"] = head + plain.String()
	}
	if over.Len() > 0 {
		files["cloudburrow_providers_override.tf"] = head + over.String()
	}
	return files, skipped
}

// terraformArgs splits `cloudburrow terraform [flags] [--] args...`.
// CloudBurrow's own flags come before "--". Without one, every argument is
// the binary's, and the instance is chosen by the environment and config
// file, as for any command.
func terraformArgs(args []string) (ours, theirs []string, binary string, err error) {
	for i, a := range args {
		if a == "--" {
			ours, theirs = args[:i], args[i+1:]
			break
		}
	}
	if theirs == nil {
		theirs, ours = args, nil
	}
	v, found, ours, err := splitFlag(ours, "binary", false)
	if err != nil {
		return nil, nil, "", err
	}
	binary = "terraform"
	if found {
		binary = v
	}
	return ours, theirs, binary, nil
}

// moduleDir is the directory Terraform will read, honouring -chdir.
func moduleDir(theirs []string) string {
	for _, a := range theirs {
		if dir, ok := strings.CutPrefix(a, "-chdir="); ok {
			return dir
		}
		if !strings.HasPrefix(a, "-") {
			break // the subcommand: -chdir must come before it
		}
	}
	return "."
}

func runTerraform(args []string, stdout, stderr io.Writer) error {
	ours, theirs, binary, err := terraformArgs(args)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return errUsage
	}
	iv, intercept, ours, err := splitFlag(ours, interceptFlag, true)
	intercept = intercept && iv != "false"
	if err != nil {
		fmt.Fprintln(stderr, err)
		return errUsage
	}
	cfg, err := config.Load(config.Options{Args: ours, Output: stderr})
	if err != nil {
		return err
	}
	enforced := cfg.IAMEnforced()
	if info, ok := running(cfg); ok {
		// The services the instance serves, as `env` takes them (#652):
		// `--name x` alone would otherwise give the default services, and
		// an opt-in one such as BigQuery would get no endpoint (#1236).
		cfg = withLivePorts(servedConfig(cfg, info, true), info.Endpoints)
		enforced = info.IAMEnforcement == config.IAMEnforcementOn
	}

	dir := moduleDir(theirs)
	families := unpointedResources(cfg)
	if intercept {
		// The locations data sources it serves locally (#1578).
		families = interceptUnlocked(cfg, families)
	}
	if bad := unpointed(dir, families); len(bad) != 0 && !offlineSubcommands[subcommand(theirs)] {
		return fmt.Errorf("refusing to run %s %s: the module declares resources cloudburrow cannot point at this instance, "+
			"which the provider would send to real Google:\n  %s", binary, subcommand(theirs), strings.Join(bad, "\n  "))
	}
	// Under --iam-enforcement the provider signs in as a principal this
	// instance knows (terraformauth.go); otherwise as "owner", unless the
	// module calls the IAM Credentials API, which authenticates the caller
	// whatever the mode (moduleCallsIAMCredentials).
	auth := terraformOwnerAuth
	if (enforced || moduleCallsIAMCredentials(dir)) && !offlineSubcommands[subcommand(theirs)] {
		a, cleanup, err := terraformEnforcedAuth(cfg, dir, os.Getenv)
		if err != nil {
			return err
		}
		defer cleanup()
		auth = a
	}
	files, skipped := terraformProviderFilesAuth(cfg, dir, auth)
	for name := range files {
		path := filepath.Join(dir, name)
		if b, err := os.ReadFile(path); err == nil && !strings.HasPrefix(string(b), generatedMarker) {
			return fmt.Errorf("%s exists and was not written by cloudburrow; refusing to replace it", path)
		}
	}
	for _, s := range skipped {
		fmt.Fprintf(stderr, "cloudburrow terraform: %s is enabled but has no Verified Terraform support, "+
			"so its endpoints are refused locally rather than pointed here\n", s)
	}
	for name, body := range files {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			return err
		}
		defer func() { _ = os.Remove(path) }()
	}

	cmd := exec.Command(binary, theirs...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, stdout, stderr
	// Every endpoint argument the instance does not serve is refused
	// locally (#1975); the environment carries those the provider reads
	// from one, after the user's own, so these win.
	env := append(os.Environ(), terraformRefusalEnv(cfg)...)
	if h := bigtableHost(cfg); h != "" {
		env = append(env, "BIGTABLE_EMULATOR_HOST="+h)
	}
	// The opt-in intercept (#1578, terraform_intercept.go), for subcommands
	// that call the provider: init downloads providers, which it would refuse.
	if intercept && !offlineSubcommands[subcommand(theirs)] {
		ic, err := startIntercept(cfg, stderr)
		if err != nil {
			return err
		}
		defer ic.stop()
		if w := interceptTrustWarning(dir, runtime.GOOS); w != "" {
			fmt.Fprintln(stderr, w)
		}
		env = ic.env(env, cfg.BindAddress)
	}
	cmd.Env = env
	// Terraform handles Ctrl-C itself: the terminal delivers SIGINT to it
	// directly, and it stops cleanly and saves state. This process only has
	// to outlive it, so the file is removed afterwards rather than under a
	// run still reading it. SIGTERM, sent to this process alone, is passed on.
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("run %s: %w", binary, err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	for {
		select {
		case s := <-sigs:
			if s == syscall.SIGTERM {
				_ = cmd.Process.Signal(syscall.SIGTERM)
			}
		case err := <-done:
			var exit *exec.ExitError
			if errors.As(err, &exit) {
				// The binary's own status, and its own message already shown.
				return &exitError{code: exit.ExitCode(), err: err, quiet: true}
			}
			return err
		}
	}
}
