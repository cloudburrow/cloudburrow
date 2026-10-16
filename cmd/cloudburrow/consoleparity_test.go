package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// The feature parity audit (#782): "every feature we build should be
// accessible by the UI". Every method a coverage page classes Verified or
// Implemented, every CLI command and every admin route is in exactly one row
// below, and each row is one of three things:
//
//   - a console surface: the provider (or non-provider screen) that offers
//     it, what it offers, and the tests that exercise it;
//   - an exclusion, with the reason it has no console surface;
//   - a gap, naming the open issue that will build it.
//
// TestConsoleParityTables fails when a coverage page gains a Verified method,
// or the CLI a command, or the admin API a route, that no row claims, so a
// new feature cannot ship without either a screen, a reason or an issue. It
// also holds docs/console-parity.md §8 to these rows; regenerate it with
//
//	go test ./cmd/cloudburrow -run TestConsoleParityTables -update
//
// When a gap is built, its row loses Issue and gains Provider, Offers and
// Tests.

// parityRow is one feature group: methods of one resource, or CLI commands
// and admin routes that share a surface.
type parityRow struct {
	// Resource names the group as §8 shows it.
	Resource string
	// Service is the coverage page's key (coverage.json "Key") the Methods
	// belong to; empty for a CLI/admin row.
	Service string
	// Methods are coverage methods, shortened by shortMethod: the service
	// name without its package, then the method ("CloudTasks/CreateQueue"),
	// or the whole name for a method with no slash ("storage.buckets.get").
	Methods []string
	// Commands are `cloudburrow` commands, as the usage text lists them.
	Commands []string
	// Routes are admin routes, "METHOD /admin/path" as internal/admin
	// registers them.
	Routes []string

	// Provider is the console provider ID that offers the group, or Screen a
	// console route that is not a provider (the Logs Explorer).
	Provider string
	Screen   string
	// Offers says what the console offers for the group, in the words the
	// screen uses.
	Offers string
	// Tests name test functions that exercise the surface.
	Tests []string

	// Excluded is why the group has no console surface.
	Excluded string

	// Issue is the open issue that will build the missing surface.
	Issue int
}

var parityRows = []parityRow{
	// Backup and DR (#1433).
	{Resource: "Cloud SQL point-in-time restore from a Backup and DR data source", Service: "sqladmin",
		Methods:  []string{"sql.instances.pointInTimeRestore"},
		Provider: "backupdr", Offers: "Point-in-time restore on a Backup and DR data source's page: a new instance recovered to a time (#1433)",
		Tests: []string{"TestConsoleBackupDRVaultPlanAssociationRestore"}},
	{Resource: "Backup and DR vaults, plans and associations", Service: "backupdr",
		Methods: []string{"backupdr.projects.locations.backupVaults.create", "backupdr.projects.locations.backupVaults.get",
			"backupdr.projects.locations.backupVaults.list", "backupdr.projects.locations.backupVaults.delete",
			"backupdr.projects.locations.backupPlans.create", "backupdr.projects.locations.backupPlans.list",
			"backupdr.projects.locations.backupPlanAssociations.create", "backupdr.projects.locations.backupPlanAssociations.list",
			"backupdr.projects.locations.backupPlanAssociations.triggerBackup"},
		Provider: "backupdr", Offers: "Backup and DR page (#1433): backup vaults with Create backup vault and Delete; a vault's page with its data sources, the backup plans storing into it (rules shown) and their associations, Create backup plan, Associate an instance and Trigger backup",
		Tests: []string{"TestConsoleBackupDRVaultPlanAssociationRestore"}},
	{Resource: "Backup and DR data sources and backups", Service: "backupdr",
		Methods: []string{"backupdr.projects.locations.backupVaults.dataSources.get", "backupdr.projects.locations.backupVaults.dataSources.list",
			"backupdr.projects.locations.backupVaults.dataSources.backups.list"},
		Provider: "backupdr", Offers: "a data source's page with its backups (type, consistency time, expiry), Restore a backup (instances.restoreBackup with backupdrBackup) and Point-in-time restore (instances.pointInTimeRestore)",
		Tests: []string{"TestConsoleBackupDRVaultPlanAssociationRestore"}},
	{Resource: "Backup and DR vault and plan edits", Service: "backupdr",
		Methods:  []string{"backupdr.projects.locations.backupVaults.patch", "backupdr.projects.locations.backupPlans.patch", "backupdr.projects.locations.backupPlans.revisions.list"},
		Provider: "backupdr", Offers: "A vault's page: Edit vault (description, labels, minimum enforced retention); Edit backup plan (a rule's recurrence and retention, the description), each edit a new revision; a Plan revisions section (each revision's state, creation time and rules) (#1462)",
		Tests: []string{"TestConsoleBackupDREdits"}},
	{Resource: "Backup and DR association move", Service: "backupdr",
		Methods:  []string{"backupdr.projects.locations.backupPlanAssociations.patch"},
		Provider: "backupdr", Offers: "A vault's page: Move association to another backup plan; a plan in another vault gives the instance a data source there (#1462)",
		Tests: []string{"TestConsoleBackupDREdits"}},
	{Resource: "Backup and DR backup retention", Service: "backupdr",
		Methods:  []string{"backupdr.projects.locations.backupVaults.dataSources.backups.patch"},
		Provider: "backupdr", Offers: "A data source's page: Edit backup (expiry, enforced retention end; enforced retention can be extended, not reduced) (#1462)",
		Tests: []string{"TestConsoleBackupDREdits"}},
	{Resource: "Backup and DR views, IAM test and operation management", Service: "backupdr",
		Methods: []string{"backupdr.projects.locations.backupPlans.revisions.get", "backupdr.projects.locations.backupVaults.fetchUsable",
			"backupdr.projects.locations.backupVaults.testIamPermissions", "backupdr.projects.locations.backupPlanAssociations.fetchForResourceType",
			"backupdr.projects.locations.backupVaults.dataSources.backups.fetchForResourceType", "backupdr.projects.locations.dataSourceReferences.list",
			"backupdr.projects.locations.dataSourceReferences.get", "backupdr.projects.locations.dataSourceReferences.fetchForResourceType",
			"backupdr.projects.locations.backupVaults.dataSources.patch", "backupdr.projects.locations.operations.cancel", "backupdr.projects.locations.operations.delete"},
		Excluded: "alternate views of the vaults, associations, data sources and backups the pages already list; one revision's read (the Plan revisions section lists every revision with its rules); data source labels; permission checks with nothing enforced; and cancelling or deleting operations the page does not keep"},
	{Resource: "Backup and DR reads, deletes and operations", Service: "backupdr",
		Methods: []string{"backupdr.projects.locations.backupPlans.get", "backupdr.projects.locations.backupPlans.delete",
			"backupdr.projects.locations.backupPlanAssociations.get", "backupdr.projects.locations.backupPlanAssociations.delete",
			"backupdr.projects.locations.backupVaults.dataSources.backups.get", "backupdr.projects.locations.backupVaults.dataSources.backups.delete",
			"backupdr.projects.locations.operations.get", "backupdr.projects.locations.operations.list",
			"backupdr.projects.locations.list", "backupdr.projects.locations.get"},
		Excluded: "single-resource reads the vault and data source pages already show in their lists, deletes of plans, associations and backups that gcloud and Terraform make, and the poll of an operation the page does not wait on"},
	// Cloud Tasks
	{Resource: "Cloud Tasks queues", Service: "tasks",
		Methods:  []string{"CloudTasks/CreateQueue", "CloudTasks/DeleteQueue", "CloudTasks/GetQueue", "CloudTasks/ListQueues", "CloudTasks/PauseQueue", "CloudTasks/ResumeQueue", "CloudTasks/PurgeQueue"},
		Provider: "tasks", Offers: "Queues list (with a filter box in ListQueues' own syntax, #1255), queue detail with configuration, Create queue, Delete, Pause / Resume, Purge",
		Tests: []string{"TestConsoleQueueActionsFollowState", "TestConsoleQueueListFilter", "TestNoDetailShowsABlankProperty", "TestRowMenuClosesOnEscapeAndGivesFocusBack"}},
	{Resource: "Cloud Tasks tasks", Service: "tasks",
		Methods:  []string{"CloudTasks/GetTask", "CloudTasks/ListTasks", "CloudTasks/DeleteTask"},
		Provider: "tasks", Offers: "Tasks section on the queue page, task detail (request, headers redacted, body), Delete task",
		Tests: []string{"TestNoDetailShowsABlankProperty", "TestTaskHeadersAreRedacted"}},
	{Resource: "Cloud Tasks queue edit and task create", Service: "tasks",
		Methods:  []string{"CloudTasks/UpdateQueue", "CloudTasks/CreateTask"},
		Provider: "tasks", Offers: "Edit queue (rate limits, retry parameters; the logging sampling ratio, shown on the queue's page, #1256; the queue's HTTP target: URI override and its mode, HTTP method and header overrides, an OIDC token, shown on its Configuration tab, #1282) and Create task (HTTP target) on a queue's page (#784); Create task's Auth header: an OIDC token (service account, audience) or an OAuth token (service account, scope), shown on the task's page (#1243)",
		Tests: []string{"TestConsoleTasksEditQueueAndCreateTask", "TestTasksEditQueueThroughTheForm", "TestConsoleTaskAndJobTokens", "TestConsoleTasksLoggingSampling", "TestTasksQueueHTTPTargetThroughTheEditForm"}},
	{Resource: "Cloud Tasks run now", Service: "tasks",
		Methods:  []string{"CloudTasks/RunTask"},
		Provider: "tasks", Offers: "Run now on a task's page: RunTask, dispatched at once even on a paused queue (#1215)",
		Tests: []string{"TestConsoleTasksRunNow"}},
	{Resource: "Cloud Tasks batches", Service: "tasks",
		Methods:  []string{"CloudTasks/BatchCreateTasks", "CloudTasks/BatchDeleteTasks"},
		Excluded: "the many-at-once forms of Create task and Delete task, which the console offers one task at a time"},
	{Resource: "Cloud Tasks CMEK config", Service: "tasks",
		Methods:  []string{"CloudTasks/GetCmekConfig", "CloudTasks/UpdateCmekConfig"},
		Excluded: "a location setting CloudBurrow stores and never applies: nothing is encrypted with the key, so a console screen would show a protection that is not there"},

	// Secret Manager
	{Resource: "Secret Manager secrets", Service: "secretmanager",
		Methods:  []string{"SecretManagerService/CreateSecret", "SecretManagerService/DeleteSecret", "SecretManagerService/GetSecret", "SecretManagerService/ListSecrets", "SecretManagerService/UpdateSecret"},
		Provider: "secrets", Offers: "Secrets list, secret detail, Create secret (with its first value, the replication policy: Automatic, or User-managed with its locations, #857; and an expire time or TTL, #1225; and its Pub/Sub notification topics, #1259; and the rotation and destruction delay, #1278), Edit (labels, annotations, expire time or TTL, cleared when both are empty; #1225; the notification topics, #1259; the next rotation time, rotation period and destruction delay, #1278), the expiry, the topics, the rotation and the destruction delay on the secret's page, Advance clock (CloudBurrow extension) on a secret's page (#1278), customer-managed encryption keys on Create secret and the secret's page, and Change encryption keys (#1453), the expiry in the list, a filter box in Secret Manager's filter syntax on the list (#1228), Delete",
		Tests: []string{"TestSecretCreateDoesNotLeaveAnEmptySecretBehind", "TestSecretCreateKeepsTheReplicationPolicy", "TestConsoleSecretReplicationPolicy", "TestSecretCreateReplicationPolicyThroughTheForm", "TestSecretListingsNeverCarryPayloads", "TestNoDetailShowsABlankProperty", "TestConsoleSecretExpiry", "TestSecretExpiryThroughTheForms", "TestConsoleSecretListFilter", "TestSecretListFilterThroughTheBox", "TestConsoleSecretTopics", "TestConsoleSecretRotationAndDestroyDelay", "TestConsoleSecretCMEK"}},
	{Resource: "Secret Manager versions", Service: "secretmanager",
		Methods:  []string{"SecretManagerService/AddSecretVersion", "SecretManagerService/AccessSecretVersion", "SecretManagerService/EnableSecretVersion", "SecretManagerService/DisableSecretVersion", "SecretManagerService/DestroySecretVersion", "SecretManagerService/GetSecretVersion", "SecretManagerService/ListSecretVersions"},
		Provider: "secrets", Offers: "Versions section, version detail (with its scheduled destroy time, #1278), Add version, Enable / Disable / Destroy (Destroy (delayed) on a secret with a destruction delay, saying when; Enable or Keep disabled to cancel a scheduled destruction, #1278), Show value (recorded in Activity), the key version of each replica on the version's page (#1453)",
		Tests: []string{"TestVersionActionsFollowTheVersionsState", "TestRevealRefusesWhatTheAPIWouldRefuse", "TestConsoleSecretRotationAndDestroyDelay", "TestConsoleSecretCMEK"}},

	// Cloud Run
	{Resource: "Cloud Run services", Service: "run",
		Methods:  []string{"Services/CreateService", "Services/DeleteService", "Services/GetService", "Services/ListServices", "Services/UpdateService"},
		Provider: "run", Offers: "Services list, service detail, Deploy container and Edit and deploy new revision (with labels and variables from Secret Manager, #852; the service account, files from Secret Manager and in-memory volumes, #1297; execution environment, VPC connector and egress, encryption key and session affinity, stored without effect, #1346), Manage traffic (splits, LATEST, pinned revisions, tags; #1224; sent as an update_mask of traffic, #1260), Delete",
		Tests: []string{"TestConsoleRunEditAndDeployNewRevision", "TestRunEditDeploysTheFormThroughUpdateService", "TestCloudRunEditFormIsPrefilledAndGivesFocusBack",
			"TestConsoleRunServiceLabelsAndSecrets", "TestRunEditSetsLabelsAndSecretBackedVariables",
			"TestConsoleRunVolumesAndServiceAccount", "TestRunVolumeFieldsRoundTrip",
			"TestConsoleRunStoredSettings", "TestRunSettingsFieldsRoundTrip",
			"TestTrafficLinesRoundTrip", "TestTrafficListingShowsTags", "TestRunManageTrafficSendsATrafficMask", "TestCloudRunManageTrafficThroughTheBrowser"}},
	// The v1 (Knative) API (#1293) is the same services and revisions, read
	// from the same Knative objects through the same mapping: the console's
	// Cloud Run pages already reach every one, so nothing new is offered.
	{Resource: "Cloud Run v1 services", Service: "runv1",
		Methods: []string{"run.namespaces.services.create", "run.namespaces.services.get", "run.namespaces.services.list",
			"run.namespaces.services.replaceService", "run.namespaces.services.delete",
			"run.projects.locations.services.list", "run.projects.locations.services.get"},
		Provider: "run", Offers: "The v2 services surface above: Services list, service detail, Deploy container, Edit and deploy new revision, Delete; v1 is the same services in Knative's shape (#1293)",
		Tests: []string{"TestConsoleRunEditAndDeployNewRevision", "TestNoDetailShowsABlankProperty"}},
	{Resource: "Cloud Run v1 revisions, routes and configurations", Service: "runv1",
		Methods: []string{"run.namespaces.revisions.list", "run.namespaces.revisions.get", "run.namespaces.routes.list",
			"run.namespaces.routes.get", "run.namespaces.configurations.list", "run.namespaces.configurations.get"},
		Provider: "run", Offers: "Revision history, revision detail and the Traffic section on a service's page: a v1 route is a service's traffic and a configuration its template (#1293)",
		Tests: []string{"TestNoDetailShowsABlankProperty", "TestCloudRunRowSaysWhatIsServingAndWhyNot"}},
	{Resource: "Cloud Run v1 jobs", Service: "runv1",
		Methods: []string{"run.namespaces.jobs.create", "run.namespaces.jobs.get", "run.namespaces.jobs.list", "run.namespaces.jobs.delete",
			"run.namespaces.jobs.replaceJob", "run.namespaces.jobs.run"},
		Provider: "run-jobs", Offers: "The v2 jobs surface: Jobs page, job detail, Create job, Edit job, Execute, Delete; a v1 job is the same job in Knative's shape (#1349, #1379); Set delayed execution on a job's page, v1's spec.template.spec.delayExecution, which v2 has no field for, read and replaced through the v1 API (#1450)",
		Tests: []string{"TestConsoleRunJobsCreateExecuteAndCancel", "TestNoDetailShowsABlankProperty", "TestConsoleRunJobSetDelayedExecution"}},
	{Resource: "Cloud Run v1 executions and tasks", Service: "runv1",
		Methods: []string{"run.namespaces.executions.get", "run.namespaces.executions.list", "run.namespaces.executions.cancel",
			"run.namespaces.executions.delete", "run.namespaces.tasks.get", "run.namespaces.tasks.list"},
		Provider: "run-jobs", Offers: "The v2 executions and tasks surface: Executions section on a job's page, execution detail, Cancel, Delete, Tasks section and task detail; a v1 execution is the same execution, listed there whichever API ran it (#1379)",
		Tests: []string{"TestConsoleRunJobsCreateExecuteAndCancel", "TestRunJobExecutionPageShowsTheFailureAndItsTasksLogs"}},
	{Resource: "Cloud Run v1 service IAM policy", Service: "runv1",
		Methods:  []string{"run.projects.locations.services.getIamPolicy", "run.projects.locations.services.setIamPolicy", "run.projects.locations.services.testIamPermissions"},
		Provider: "run", Offers: "The Permissions tab on a service's page: the same policy v2's GetIamPolicy and SetIamPolicy read and write, Test permissions (#1349)",
		Tests: []string{"TestConsolePermissionsOnARunServiceAndJob", "TestPermissionsTest"}},
	{Resource: "Cloud Run v1 job IAM policy", Service: "runv1",
		Methods:  []string{"run.projects.locations.jobs.getIamPolicy", "run.projects.locations.jobs.setIamPolicy", "run.projects.locations.jobs.testIamPermissions"},
		Provider: "run-jobs", Offers: "The Permissions tab on a job's page: the same policy v2's Jobs GetIamPolicy and SetIamPolicy read and write, Test permissions (#1420)",
		Tests: []string{"TestConsolePermissionsOnARunServiceAndJob", "TestPermissionsTest"}},
	{Resource: "Cloud Run v1 locations", Service: "runv1",
		Methods:  []string{"run.projects.locations.list"},
		Excluded: "a list of the instance's regions; the Cloud Run forms' Region field offers the same regions"},
	{Resource: "Cloud Run v1 domain mappings", Service: "runv1",
		Methods: []string{"run.namespaces.domainmappings.create", "run.namespaces.domainmappings.get", "run.namespaces.domainmappings.list",
			"run.namespaces.domainmappings.delete", "run.projects.locations.domainmappings.list", "run.projects.authorizeddomains.list"},
		Provider: "run", Offers: "The Domain mappings tab on a service's page: each domain mapped to the service, its Ready, CertificateProvisioned and DomainRoutable conditions with their reasons, and its DNS records; Map a domain (domain, certificate mode) and Remove domain mapping. The instance's gateway routes a mapped domain; no public DNS is served and no certificate issued, which the tab says. Authorized domains are none locally (no Search Console), so Map a domain does not consult them (#1450)",
		Tests: []string{"TestConsoleRunDomainMappings", "TestDomainRowSaysWhyNotReady", "TestRunRevisionDeleteIsOfferedOnlyWhereTheAPIAccepts"}},
	{Resource: "Cloud Run revisions", Service: "run",
		Methods:  []string{"Revisions/GetRevision", "Revisions/ListRevisions"},
		Provider: "run", Offers: "Revision history and Traffic sections on the service page (each tag and its URL, #1224), revision detail",
		Tests: []string{"TestNoDetailShowsABlankProperty", "TestCloudRunRowSaysWhatIsServingAndWhyNot"}},
	{Resource: "Cloud Run jobs", Service: "run",
		Methods:  []string{"Jobs/CreateJob", "Jobs/DeleteJob", "Jobs/GetJob", "Jobs/ListJobs", "Jobs/RunJob", "Jobs/UpdateJob"},
		Provider: "run-jobs", Offers: "Jobs page (/run/jobs): jobs list, job detail with executions and configuration, Create job, Edit job, Execute, Delete (#785); labels and variables from Secret Manager on Create job and Edit job, and Execute with overrides (arguments, variables, task count, timeout) on a job's page (#852); the service account, files from Secret Manager and in-memory volumes on Create job and Edit job (#1297); execution environment, VPC connector and egress and encryption key, stored without effect (#1346)",
		Tests: []string{"TestConsoleRunJobsCreateExecuteAndCancel", "TestRunJobsCreateExecuteCancelAndDeleteThroughTheAPI", "TestRunJobEditKeepsWhatTheFormDoesNotShow", "TestCloudRunJobCreatedExecutedAndDeletedInTheBrowser",
			"TestConsoleRunJobLabelsSecretsAndOverrides", "TestRunJobLabelsSecretsAndExecuteWithOverrides", "TestCloudRunJobExecuteWithOverridesThroughTheForm",
			"TestConsoleRunVolumesAndServiceAccount", "TestRunVolumeFieldsRoundTrip",
			"TestConsoleRunStoredSettings", "TestRunSettingsFieldsRoundTrip"}},
	{Resource: "Cloud Run executions", Service: "run",
		Methods:  []string{"Executions/CancelExecution", "Executions/DeleteExecution", "Executions/GetExecution", "Executions/ListExecutions"},
		Provider: "run-jobs", Offers: "Executions section on a job's page, execution detail (status, task counts, conditions, failure message, Logs), Cancel while running, Delete when finished (#785)",
		Tests: []string{"TestConsoleRunJobsCreateExecuteAndCancel", "TestRunJobExecutionPageShowsTheFailureAndItsTasksLogs"}},
	{Resource: "Cloud Run tasks", Service: "run",
		Methods:  []string{"Tasks/GetTask", "Tasks/ListTasks"},
		Provider: "run-jobs", Offers: "Tasks section on an execution's page, task detail (status, index, retries, exit code, failure, conditions, containers, its pods' Logs) (#1223)",
		Tests: []string{"TestRunJobExecutionPageShowsTheFailureAndItsTasksLogs", "TestCloudRunExecutionTasksInTheBrowser"}},
	{Resource: "Cloud Run revision delete", Service: "run",
		Methods:  []string{"Revisions/DeleteRevision"},
		Provider: "run", Offers: "Delete revision on a Revision history row and a revision's page, for a revision that serves no traffic (#785)",
		Tests: []string{"TestConsoleRunEditAndDeployNewRevision", "TestRunRevisionDeleteIsOfferedOnlyWhereTheAPIAccepts"}},
	{Resource: "Cloud Run worker pools", Service: "run",
		Methods:  []string{"WorkerPools/CreateWorkerPool", "WorkerPools/DeleteWorkerPool", "WorkerPools/GetWorkerPool", "WorkerPools/ListWorkerPools", "WorkerPools/UpdateWorkerPool"},
		Provider: "run-worker-pools", Offers: "Worker pools page (/run/worker-pools): pools list, a pool's page with its revisions and configuration, Deploy worker pool, Edit worker pool (a new revision), Scale (the instance count, the revision kept), Split instances (instanceSplits across revisions, sent with an update_mask of instance_splits; each revision's instances in the revisions list, #1387), Delete (#1300)",
		Tests: []string{"TestConsoleRunWorkerPoolDeployEditScaleDelete", "TestNoDetailShowsABlankProperty", "TestConsoleRunWorkerPoolSplit"}},
	{Resource: "Cloud Run instances", Service: "run",
		Methods:  []string{"Instances/CreateInstance", "Instances/DeleteInstance", "Instances/GetInstance", "Instances/ListInstances", "Instances/StartInstance", "Instances/StopInstance"},
		Provider: "run-instances", Offers: "Instances page (/run/instances): instances list, an instance's page with its status, URL and configuration, Create instance, Stop, Start, Delete (#1299; the URL and operations that wait for the instance, #1348)",
		Tests: []string{"TestConsoleRunInstanceCreateStopStartDelete", "TestNoDetailShowsABlankProperty"}},
	{Resource: "Cloud Run builds", Service: "run",
		Methods:  []string{"Builds/SubmitBuild"},
		Provider: "run", Offers: "Deploy from source on a service's page: a .tar.gz uploaded to run-sources-{project}-{region} in this instance's Cloud Storage, built with its Dockerfile or Buildpacks through SubmitBuild, and deployed as a new revision (#1268); Create service from source on the list screen: a service name, the region and the same archive and build, then CreateService with the image (#1289)",
		Tests: []string{"TestRunDeployFromSourceIsOfferedWithStorage", "TestRunCreateServiceFromSourceIsAListAction", "TestConsoleRunCreateServiceFromSource"}},
	{Resource: "Cloud Build builds", Service: "cloudbuild",
		Methods:  []string{"CloudBuild/GetBuild", "CloudBuild/ListBuilds", "CloudBuild/GetDefaultServiceAccount", "CloudBuild/CreateBuild", "CloudBuild/CancelBuild", "CloudBuild/RetryBuild"},
		Provider: "run-builds", Offers: "Build history page (/run/builds): every build, newest first, with its status, location, image, digest and Cloud Storage source, and a filter box taking ListBuilds' filter (#1382); a build's page with each step's status, args and duration and the build log; Run build (a Dockerfile source in Cloud Storage built and its image pushed, as gcloud builds submit --tag), Cancel on a running build's page and Retry on a finished one CreateBuild created (#1382); a build also runs from Deploy from source, as SubmitBuild, under the default build service account (#1347)",
		Tests: []string{"TestConsoleSourceDeployScreens", "TestConsoleRunBuildAndCancel"}},
	{Resource: "Cloud Build triggers", Service: "cloudbuild",
		Methods:  []string{"CloudBuild/CreateBuildTrigger", "CloudBuild/ListBuildTriggers", "CloudBuild/RunBuildTrigger", "CloudBuild/DeleteBuildTrigger"},
		Provider: "cloud-build-triggers", Offers: "Triggers page (/cloud-build/triggers): every trigger with its region, event (manual or webhook), description and pool; Create trigger (an inline one-step build, an optional worker pool and an optional webhook secret), Run on an enabled manual trigger, and delete (#1422)",
		Tests: []string{"TestConsoleBuildTriggersAndWorkerPools"}},
	{Resource: "Cloud Build triggers (read, update and webhook)", Service: "cloudbuild",
		Methods:  []string{"CloudBuild/GetBuildTrigger", "CloudBuild/UpdateBuildTrigger", "CloudBuild/ReceiveTriggerWebhook"},
		Provider: "cloud-build-triggers", Offers: "the Triggers list shows each trigger as GetBuildTrigger reads it, Disabled when disabled; a webhook trigger runs from its :webhook URL, which a sender posts to, not from the console (#1422)",
		Tests: []string{"TestConsoleBuildTriggersAndWorkerPools"}},
	{Resource: "Cloud Build worker pools", Service: "cloudbuild",
		Methods:  []string{"CloudBuild/CreateWorkerPool", "CloudBuild/GetWorkerPool", "CloudBuild/ListWorkerPools", "CloudBuild/UpdateWorkerPool", "CloudBuild/DeleteWorkerPool"},
		Provider: "cloud-build-worker-pools", Offers: "Worker pools page (/cloud-build/worker-pools): every pool with its region, machine type, disk and display name; Create worker pool (name, region, machine type) and delete; a pool's display name and config are changed with gcloud or Terraform (#1422)",
		Tests: []string{"TestConsoleBuildTriggersAndWorkerPools"}},
	{Resource: "Cloud Build approvals", Service: "cloudbuild",
		Methods:  []string{"CloudBuild/ApproveBuild"},
		Provider: "run-builds", Offers: "A build awaiting approval is listed Awaiting approval; its page offers Approve and Reject, each with a comment, and Cancel, and shows its approval and comment; the Triggers page's Create trigger takes Require approval, and its list has an Approval column (#1449)",
		Tests: []string{"TestConsoleBuildApproval"}},
	{Resource: "Artifact Registry repositories", Service: "artifactregistry",
		Methods:  []string{"ArtifactRegistry/CreateRepository", "ArtifactRegistry/GetRepository", "ArtifactRegistry/ListRepositories", "ArtifactRegistry/DeleteRepository", "ArtifactRegistry/UpdateRepository"},
		Provider: "artifact-registry", Offers: "Repositories page (/artifact-registry): each repository's format, location, URL, description and age, and Delete (#1304); a repository's page with its push address and cleanup policies (#1383); a repository is created by gcloud run deploy --source, and its description, labels and cleanup policies changed by gcloud artifacts repositories update, from the console's terminal (#1347)",
		Tests: []string{"TestConsoleSourceDeployScreens", "TestConsoleArtifactRegistryImagesAndTags"}},
	{Resource: "Artifact Registry repository IAM", Service: "artifactregistry",
		Methods:  []string{"ArtifactRegistry/GetIamPolicy", "ArtifactRegistry/SetIamPolicy", "ArtifactRegistry/TestIamPermissions"},
		Provider: "artifact-registry", Offers: "The Permissions tab on a repository's page: the stored policy GetIamPolicy and SetIamPolicy read and write, Grant access, Remove principal and Test permissions, with the note that nothing is enforced (ADR-0006, #1439)",
		Tests: []string{"TestConsoleArtifactRegistryPermissions", "TestConsolePermissionsOnARepository"}},
	{Resource: "Artifact Registry images, versions, tags and files", Service: "artifactregistry",
		Methods: []string{"ArtifactRegistry/ListPackages", "ArtifactRegistry/GetPackage", "ArtifactRegistry/DeletePackage",
			"ArtifactRegistry/ListVersions", "ArtifactRegistry/GetVersion", "ArtifactRegistry/DeleteVersion",
			"ArtifactRegistry/ListTags", "ArtifactRegistry/GetTag", "ArtifactRegistry/CreateTag", "ArtifactRegistry/UpdateTag", "ArtifactRegistry/DeleteTag",
			"ArtifactRegistry/ListDockerImages", "ArtifactRegistry/GetDockerImage", "ArtifactRegistry/ListFiles", "ArtifactRegistry/GetFile"},
		Provider: "artifact-registry", Offers: "A repository's page (/artifact-registry/REPOSITORY): its Images, each with its versions, tags and last update; an image's page with its Versions (digest, tags, size, media type, upload time) and Tags, and Delete image; a version's page with its pull-by-digest URI and Files (config, layers, or an index's manifests), Add tag (a new tag, or one moved here), Remove tag and Delete version (#1383). Images are pushed with docker or crane to {region}-docker.pkg.localhost on the Run port, or by Cloud Build",
		Tests: []string{"TestConsoleArtifactRegistryImagesAndTags", "TestNoDetailShowsABlankProperty"}},

	{Resource: "Artifact Registry rules, attachments and settings", Service: "artifactregistry",
		Methods: []string{"ArtifactRegistry/CreateRule", "ArtifactRegistry/GetRule", "ArtifactRegistry/ListRules", "ArtifactRegistry/UpdateRule", "ArtifactRegistry/DeleteRule",
			"ArtifactRegistry/CreateAttachment", "ArtifactRegistry/GetAttachment", "ArtifactRegistry/ListAttachments", "ArtifactRegistry/DeleteAttachment",
			"ArtifactRegistry/GetProjectSettings", "ArtifactRegistry/UpdateProjectSettings", "ArtifactRegistry/GetVPCSCConfig", "ArtifactRegistry/UpdateVPCSCConfig"},
		Provider: "artifact-registry", Offers: "A repository's page lists its Rules (stored, not enforced, as its note says) and Attachments, and shows its location's VPC-SC policy and its project's settings; Add rule, Delete rule, Delete attachment, Set project pull percent and Set VPC-SC policy; a version's page has Add attachment (#1449). A rule is changed by deleting and adding it, or with the API",
		Tests: []string{"TestConsoleArtifactRegistryImagesAndTags"}},
	{Resource: "Artifact Registry metadata, batch delete and export", Service: "artifactregistry",
		Methods:  []string{"ArtifactRegistry/UpdatePackage", "ArtifactRegistry/UpdateVersion", "ArtifactRegistry/UpdateFile", "ArtifactRegistry/BatchDeleteVersions", "ArtifactRegistry/ExportArtifact"},
		Provider: "artifact-registry", Offers: "An image's page shows and sets its display name, and has Delete untagged versions; a version's page shows and sets its annotations and its files' annotations, and has Export to Cloud Storage (#1449)",
		Tests: []string{"TestConsoleArtifactRegistryImagesAndTags"}},
	{Resource: "Artifact Registry file delete", Service: "artifactregistry",
		Methods:  []string{"ArtifactRegistry/DeleteFile"},
		Excluded: "refused FAILED_PRECONDITION in DOCKER repositories, the only kind served, as Google allows it only in generic repositories: a console button would always fail; Delete version removes a version's files"},
	{Resource: "Cloud Functions", Service: "functions",
		Methods:  []string{"FunctionService/CreateFunction", "FunctionService/GetFunction", "FunctionService/ListFunctions", "FunctionService/DeleteFunction", "FunctionService/GenerateUploadUrl", "FunctionService/ListRuntimes", "FunctionService/UpdateFunction", "FunctionService/GenerateDownloadUrl"},
		Provider: "functions", Offers: "Functions page (/functions): each function's generation (1st gen for one made through the v1 API, #1428), state, region, runtime, trigger, URL and age; a function's page with its URL, runtime, entry point, source, build, Cloud Run service, a link to the service's page and its logs, memory, timeout, instances and environment; Create function (a .zip uploaded to a GenerateUploadUrl URL, then CreateFunction, waited on), its runtimes the ones ListRuntimes names, its Environment 2nd gen or 1st gen and its trigger HTTP, a Cloud Pub/Sub topic or a Cloud Storage event (#1428); Delete (#1377). An update (gcloud functions deploy of an existing function), the source download and IAM are reached with gcloud functions from the console's terminal",
		Tests: []string{"TestConsoleFunctionsPage"}},
	{Resource: "Cloud Functions v1 (1st gen)", Service: "functionsv1",
		Methods:  []string{"CloudFunctionsService/CreateFunction", "CloudFunctionsService/GetFunction", "CloudFunctionsService/ListFunctions", "CloudFunctionsService/UpdateFunction", "CloudFunctionsService/DeleteFunction", "CloudFunctionsService/CallFunction", "CloudFunctionsService/GenerateUploadUrl", "CloudFunctionsService/GenerateDownloadUrl", "CloudFunctionsService/GetIamPolicy", "CloudFunctionsService/SetIamPolicy", "CloudFunctionsService/TestIamPermissions"},
		Provider: "functions", Offers: "Functions page (/functions): a 1st-gen function is listed with the 2nd-gen ones as 1st gen, with its legacy trigger, and Delete, being the same stored function (#1428); Create function with Environment 1st gen: the zip uploaded to the v1 GenerateUploadUrl URL, then CreateFunction with an HTTP trigger, a Cloud Pub/Sub topic (google.pubsub.topic.publish) or a Cloud Storage event (google.storage.object.finalize, delete, archive, metadataUpdate), waited on; the function's page is its v1 CloudFunction (GetFunction: status, trigger and URL, source, build, version, memory in MB, timeout, service account, environment); Test function on a 1st-gen HTTP function calls CallFunction with the request body and shows its result or error and execution ID. Updating one, its source download and IAM are reached with gcloud functions --no-gen2 from the console's terminal",
		Tests: []string{"TestConsoleFunctionsPage", "TestConsoleFunctionsCreateGenerations", "TestConsoleFunctionsV1"}},

	// Cloud KMS
	{Resource: "Cloud KMS key rings and keys", Service: "kms",
		Methods:  []string{"KeyManagementService/CreateKeyRing", "KeyManagementService/GetKeyRing", "KeyManagementService/ListKeyRings", "KeyManagementService/CreateCryptoKey", "KeyManagementService/GetCryptoKey", "KeyManagementService/ListCryptoKeys"},
		Provider: "kms", Offers: "Key rings list (every location), ring detail with keys, key detail, Create key ring, Create key (labels, destroy scheduled duration, rotation period and next rotation time, #852)",
		Tests: []string{"TestConsoleKMSActsThroughTheAPIAnSDKSees", "TestKMSCreateKeyRingThroughTheForm",
			"TestConsoleKMSCreateKeyOptions", "TestKMSCreateKeyWithDestroyDurationAndRotation", "TestKMSCreateKeyOptionsThroughTheForm"}},
	{Resource: "Cloud KMS locations", Service: "kms",
		Methods:  []string{"Locations/ListLocations"},
		Provider: "kms", Offers: "Create key ring: the Location is chosen from the listed locations (#1291)",
		Tests: []string{"TestKMSCreateFormOffersListLocations", "TestKMSLocations"}},
	{Resource: "Cloud KMS location read", Service: "kms",
		Methods:  []string{"Locations/GetLocation"},
		Excluded: "a read of one listed location; the Create key ring form's Location shows every location ListLocations lists"},
	{Resource: "Cloud KMS key versions and crypto", Service: "kms",
		Methods: []string{"KeyManagementService/CreateCryptoKeyVersion", "KeyManagementService/GetCryptoKeyVersion", "KeyManagementService/ListCryptoKeyVersions", "KeyManagementService/UpdateCryptoKeyVersion",
			"KeyManagementService/DestroyCryptoKeyVersion", "KeyManagementService/RestoreCryptoKeyVersion", "KeyManagementService/UpdateCryptoKeyPrimaryVersion",
			"KeyManagementService/Encrypt", "KeyManagementService/Decrypt"},
		Provider: "kms", Offers: "Versions section, version detail, Add version, Make primary, Enable / Disable, Schedule destruction, Restore, Encrypt, Decrypt",
		Tests: []string{"TestConsoleKMSActsThroughTheAPIAnSDKSees"}},
	{Resource: "Cloud KMS asymmetric, MAC, raw and ML-KEM keys", Service: "kms",
		Methods: []string{"KeyManagementService/GetPublicKey", "KeyManagementService/AsymmetricSign", "KeyManagementService/AsymmetricDecrypt",
			"KeyManagementService/MacSign", "KeyManagementService/MacVerify", "KeyManagementService/RawEncrypt", "KeyManagementService/RawDecrypt",
			"KeyManagementService/Decapsulate", "KeyManagementService/GenerateRandomBytes"},
		Provider: "kms", Offers: "Create key's Purpose and Algorithm; on an ENABLED version's page, by purpose: Sign, Verify (against the public key), Get public key, Encrypt with public key, Decrypt, MAC sign, MAC verify, Raw encrypt, Raw decrypt, Decapsulate; Generate random bytes on a ring's page (#1212)",
		Tests: []string{"TestConsoleKMSCreateKeyOffersEveryPurpose", "TestConsoleKMSVersionCryptoActions", "TestKMSSignAndVerifyThroughTheForm"}},
	{Resource: "Cloud KMS key and version deletion", Service: "kms",
		Methods:  []string{"KeyManagementService/DeleteCryptoKey", "KeyManagementService/DeleteCryptoKeyVersion"},
		Provider: "kms", Offers: "Delete key on a key's page once it has no versions, Delete version on a DESTROYED version's page that was not imported (#1212)",
		Tests: []string{"TestConsoleKMSDeleteIsOfferedWhereAllowed"}},
	{Resource: "Cloud KMS import jobs", Service: "kms",
		Methods: []string{"KeyManagementService/CreateImportJob", "KeyManagementService/GetImportJob", "KeyManagementService/ListImportJobs",
			"KeyManagementService/ImportCryptoKeyVersion"},
		Provider: "kms", Offers: "Import jobs tab on a ring's page, an import job's page with its wrapping public key, Create import job (import method, protection level), " +
			"Import version on a key's page: key material as a file or base64, already wrapped or plain and wrapped in the console under the job's public key (#1219)",
		Tests: []string{"TestConsoleKMSImportJobsAndImportVersion", "TestConsoleKMSImportJobsAndRetiredResources", "TestKMSImportJobAndVersionThroughTheForm"}},
	{Resource: "Cloud KMS retired resources", Service: "kms",
		Methods:  []string{"KeyManagementService/ListRetiredResources", "KeyManagementService/GetRetiredResource"},
		Provider: "kms", Offers: "Retired resources tab on a ring's page, listing its deleted keys, and a retired resource's page (#1219)",
		Tests: []string{"TestConsoleKMSRetiredResources", "TestConsoleKMSImportJobsAndRetiredResources"}},
	{Resource: "Cloud KMS key edit", Service: "kms",
		Methods:  []string{"KeyManagementService/UpdateCryptoKey"},
		Provider: "kms", Offers: "Edit key on a key's page: labels (#794), rotation period and next rotation time (#816)",
		Tests: []string{"TestConsoleKMSEditKey", "TestKMSEditKeyThroughUpdateCryptoKey", "TestKMSEditKeyRotationThroughUpdateCryptoKey",
			"TestKMSEditKeyLabelsThroughTheForm", "TestKMSEditKeyRotationThroughTheForm"}},
	{Resource: "Cloud KMS EKM connections", Service: "kms",
		Methods:  []string{"EkmService/CreateEkmConnection", "EkmService/GetEkmConnection", "EkmService/ListEkmConnections", "EkmService/UpdateEkmConnection", "EkmService/GetEkmConfig", "EkmService/UpdateEkmConfig"},
		Provider: "kms-ekm", Offers: "EKM connections page (/kms/ekm): a project's connections in every location; Create EKM connection (location, Service Directory service, hostname, server certificate as PEM, key management mode, crypto space path); a connection's page with its service resolver, parsed certificate and whether it is its location's default; Edit (key management mode, crypto space path) and Set as default (the location's EkmConfig), with the note that nothing dials the EKM (#1452)",
		Tests: []string{"TestConsoleKMSEkmConnections"}},
	{Resource: "Cloud KMS Autokey", Service: "kms",
		Methods:  []string{"AutokeyAdmin/GetAutokeyConfig", "AutokeyAdmin/UpdateAutokeyConfig", "AutokeyAdmin/ShowEffectiveAutokeyConfig", "Autokey/CreateKeyHandle", "Autokey/ListKeyHandles"},
		Provider: "kms-autokey", Offers: "Autokey page (/kms/autokey): the effective key project, and the Autokey configurations the project resolves through, its own then its folders'; Edit configuration (resolution mode, key project) on each; the project's page lists its key handles; Create key handle (location, resource type, name), which makes an HSM key in the key project (#1452)",
		Tests: []string{"TestConsoleKMSAutokey"}},
	{Resource: "Cloud KMS key handle read", Service: "kms",
		Methods:  []string{"Autokey/GetKeyHandle"},
		Excluded: "a read of one key handle; the project's Autokey page lists every key handle with its key and resource type"},

	// Resource Manager
	{Resource: "Resource Manager projects", Service: "resourcemanager",
		Methods:  []string{"Projects/CreateProject", "Projects/DeleteProject", "Projects/GetProject", "Projects/ListProjects", "Projects/UpdateProject"},
		Provider: "projects", Offers: "Project registry list, project detail, Create project, Edit labels, Delete",
		Tests: []string{"TestResourceManagerV3ListProjects", "TestStateSaveResetLoadRestoresEverything", "TestNoDetailShowsABlankProperty"}},
	{Resource: "Resource Manager project undelete", Service: "resourcemanager",
		Methods:  []string{"Projects/UndeleteProject"},
		Provider: "projects", Offers: "Undelete project on the page of a project pending deletion, listed DELETE_REQUESTED for 30 days (#1216)",
		Tests: []string{"TestResourceManagerV3Projects", "TestProjectUndeleteIsOfferedOnlyWhenDeleted"}},
	// The Cloud SQL Admin API (#1301): the Cloud SQL pages are its
	// instances, and their databases and users are the API's.
	{Resource: "Cloud SQL instances", Service: "sqladmin",
		Methods:  []string{"sql.instances.list", "sql.instances.get"},
		Provider: "cloudsql", Offers: "The PostgreSQL and MySQL pages, one per running server; each list's note names the instance the Admin API serves it as (postgres, mysql)",
		Tests: []string{"TestConsoleCloudSQLCreateAndDropDatabase", "TestConsoleCloudSQLMySQLCreateAndDropDatabase"}},
	{Resource: "Service Usage APIs", Service: "serviceusage",
		Methods:  []string{"serviceusage.services.list", "serviceusage.services.get", "serviceusage.services.enable", "serviceusage.services.disable"},
		Provider: "serviceusage", Offers: "APIs & Services page (#1352): a project's APIs with their state, Enable and Disable on each row; disabling is stored and shown, not enforced",
		Tests: []string{"TestConsoleServiceUsageEnableDisable", "TestServiceUsageServices"}},
	{Resource: "Service Usage batch calls and operations", Service: "serviceusage",
		Methods:  []string{"serviceusage.services.batchEnable", "serviceusage.services.batchGet", "serviceusage.operations.get"},
		Excluded: "the many-at-once forms of Enable and the page's own read, which the page offers one API at a time, and the poll of an operation that is already done when the call returns"},
	{Resource: "Cloud SQL connector settings", Service: "sqladmin",
		Methods:  []string{"sql.connect.get", "sql.connect.generateEphemeral"},
		Excluded: "the Cloud SQL connectors' own handshake (#1330): the server CA and a short-lived client certificate a connector library asks for before it dials 3307, which a person does not call"},
	{Resource: "Cloud SQL instance create, delete and restart", Service: "sqladmin",
		Methods:  []string{"sql.instances.insert", "sql.instances.delete", "sql.instances.restart"},
		Provider: "cloudsql-instances", Offers: "Cloud SQL instances page: every instance the Admin API serves, Create instance (ID, database version, tier, MySQL root password, deletion protection), Delete, refused by deletion protection and for the instances `up` runs, and Restart on a created instance (#1330)",
		Tests: []string{"TestConsoleCloudSQLInstancesCreateRestartAndDelete"}},
	{Resource: "Cloud SQL instance edits", Service: "sqladmin",
		Methods:  []string{"sql.instances.patch", "sql.instances.update"},
		Provider: "cloudsql-instances", Offers: "Edit instance on a created instance's page: tier, labels, database flags, deletion protection, automated backups and their start time, sent as one instances.patch, which replaces what update would (#1330); Upgrade, a patch of databaseVersion, with Include replicas (includeReplicasForMajorVersionUpgrade, #1421)",
		Tests: []string{"TestConsoleCloudSQLInstanceEditAndBackups"}},
	{Resource: "Cloud SQL backups and clones", Service: "sqladmin",
		Methods:  []string{"sql.backupRuns.insert", "sql.backupRuns.list", "sql.backupRuns.get", "sql.backupRuns.delete", "sql.instances.restoreBackup", "sql.instances.clone"},
		Provider: "cloudsql-instances", Offers: "An instance's Backups tab, with Create backup, Restore and Delete backup, and Clone, on every instance's page (#1330)",
		Tests: []string{"TestConsoleCloudSQLInstanceEditAndBackups"}},
	{Resource: "Cloud SQL project-level and final backups", Service: "sqladmin",
		Methods:  []string{"sql.Backups.CreateBackup", "sql.Backups.ListBackups", "sql.Backups.GetBackup", "sql.Backups.UpdateBackup", "sql.Backups.DeleteBackup"},
		Provider: "cloudsql-instances", Offers: "Delete with final backup on a created instance's page; Edit project backup (description, a FINAL backup's retention), Restore project backup and Delete project backup on every instance's page, over the project's backups, a FINAL one whose instance is gone among them (#1462)",
		Tests: []string{"TestConsoleCloudSQLDataAPIAndProjectBackups"}},
	{Resource: "Cloud SQL Data API, upgrade pre-check and log truncation", Service: "sqladmin",
		Methods:  []string{"sql.instances.executeSql", "sql.instances.preCheckMajorVersionUpgrade", "sql.instances.truncateLog"},
		Provider: "cloudsql-instances", Offers: "Run SQL (Data API) on a created instance with dataApiAccess ALLOW_DATA_API, answered with the rows; Pre-check upgrade, answered with its findings; Truncate log on a MySQL instance (#1462)",
		Tests: []string{"TestConsoleCloudSQLDataAPIAndProjectBackups"}},
	{Resource: "Cloud SQL point-in-time recovery", Service: "sqladmin",
		Methods:  []string{"sql.projects.instances.getLatestRecoveryTime"},
		Provider: "cloudsql-instances", Offers: "Point-in-time recovery on Create instance and Edit instance; the earliest and latest recovery times on the instance's page, and Point-in-time restore, a clone to a time (#1330); on a MySQL instance with binary logging, Clone to binary log position, a clone to binLogCoordinates (#1421)",
		Tests: []string{"TestConsoleCloudSQLPointInTimeRestore"}},
	{Resource: "Cloud SQL read replicas", Service: "sqladmin",
		Methods:  []string{"sql.instances.promoteReplica", "sql.instances.stopReplica", "sql.instances.startReplica"},
		Provider: "cloudsql-instances", Offers: "Create read replica on a primary's page, the replica's primary and the primary's replicas shown, and Promote, Stop replication and Start replication on a replica's page (#1330)",
		Tests: []string{"TestConsoleCloudSQLReadReplica"}},
	{Resource: "Cloud SQL high availability", Service: "sqladmin",
		Methods:  []string{"sql.instances.failover"},
		Provider: "cloudsql-instances", Offers: "Zonal availability (ZONAL or REGIONAL) on Create instance and Edit instance; an instance's zone and secondary zone on its page; Failover, confirmed, on a REGIONAL instance, sent with its settingsVersion (#1408)",
		Tests: []string{"TestCloudSQLConsoleHighAvailability"}},
	{Resource: "Cloud SQL imports and exports", Service: "sqladmin",
		Methods:  []string{"sql.instances.export", "sql.instances.import"},
		Provider: "cloudsql-instances", Offers: "Export and Import on every instance's page: a SQL dump or a query's CSV to, and from, a gs:// file in the instance's Cloud Storage (#1380); Parallel and Threads, a folder of pg_dump's or MySQL Shell's files, MySQL's binary log coordinates (masterData) on Export, and PostgreSQL's Clean and If exists on Import (#1421)",
		Tests: []string{"TestConsoleCloudSQLImportAndExport", "TestConsoleCloudSQLParallelExportAndImport"}},
	{Resource: "Cloud SQL client certificates", Service: "sqladmin",
		Methods:  []string{"sql.sslCerts.insert", "sql.sslCerts.list", "sql.sslCerts.delete", "sql.instances.resetSslConfig", "sql.instances.listServerCas", "sql.instances.addServerCa", "sql.instances.rotateServerCa"},
		Provider: "cloudsql-instances", Offers: "An instance's Client certificates tab, Create client certificate, which shows the certificate, its private key and the server CA once, Delete client certificate, Reset SSL configuration, the instance's server CAs, and Add server CA and Rotate server CA (#1380)",
		Tests: []string{"TestConsoleCloudSQLClientCertificates"}},
	{Resource: "Cloud SQL client certificate reads", Service: "sqladmin",
		Methods:  []string{"sql.sslCerts.get", "sql.sslCerts.createEphemeral"},
		Excluded: "get reads one certificate the Client certificates tab already lists; createEphemeral is the deprecated form of the connectors' generateEphemeralCert, which a connector library calls with its own public key, not a person"},
	{Resource: "Cloud SQL tiers and flags", Service: "sqladmin",
		Methods:  []string{"sql.tiers.list", "sql.flags.list"},
		Excluded: "Google's documented reference lists of machine tiers and database flags (#1380), which the Tier and Database flags fields of Create instance and Edit instance take their values from; neither is a resource"},
	{Resource: "Cloud SQL operation cancel", Service: "sqladmin",
		Methods:  []string{"sql.operations.cancel"},
		Excluded: "cancels a running import or export (#1380), which the console starts and does not wait for; the console lists no operations to cancel, as the Cloud SQL operations row says"},
	{Resource: "Cloud SQL databases", Service: "sqladmin",
		Methods:  []string{"sql.databases.insert", "sql.databases.list", "sql.databases.get", "sql.databases.delete"},
		Provider: "cloudsql", Offers: "Databases list, database page, Create database, Delete, on the PostgreSQL and MySQL pages",
		Tests: []string{"TestConsoleCloudSQLCreateAndDropDatabase", "TestConsoleCloudSQLMySQLCreateAndDropDatabase"}},
	{Resource: "Cloud SQL database charset changes", Service: "sqladmin",
		Methods:  []string{"sql.databases.patch", "sql.databases.update"},
		Excluded: "a MySQL database's charset and collation, which the Read-write SQL editor changes with ALTER DATABASE; PostgreSQL fixes both at creation"},
	{Resource: "Cloud SQL users", Service: "sqladmin",
		Methods:  []string{"sql.users.insert", "sql.users.list", "sql.users.get", "sql.users.delete"},
		Provider: "cloudsql", Offers: "Users tab on a database's page, Add user and Delete user (#1301), on the PostgreSQL and MySQL pages, through the same statements as the API",
		Tests: []string{"TestConsoleCloudSQLUsersMatchTheAdminAPI"}},
	{Resource: "Cloud SQL user password changes", Service: "sqladmin",
		Methods:  []string{"sql.users.update"},
		Excluded: "a password change, which the Read-write SQL editor makes with ALTER ROLE or ALTER USER"},
	{Resource: "Cloud SQL operations", Service: "sqladmin",
		Methods:  []string{"sql.operations.get", "sql.operations.list"},
		Excluded: "an operation is DONE when it is returned, or, for an instance's create, patch, restart or delete (#1330), once the page lists the instance's new state; the console's own operations ledger records each console action"},
	{Resource: "Resource Manager project search", Service: "resourcemanager",
		Methods:  []string{"Projects/SearchProjects"},
		Excluded: "the query form of ListProjects over the same registry; the Projects list and its filter show the same projects"},
	{Resource: "Resource Manager project move", Service: "resourcemanager",
		Methods:  []string{"Projects/MoveProject"},
		Provider: "projects", Offers: "Parent on Create project, Move project on a project's page, and the Parent on its page (#1294)",
		Tests: []string{"TestConsoleResourceHierarchy", "TestResourceManagerV3Folders"}},
	{Resource: "Resource Manager folders", Service: "resourcemanager",
		Methods:  []string{"Folders/CreateFolder", "Folders/DeleteFolder", "Folders/GetFolder", "Folders/ListFolders", "Folders/MoveFolder", "Folders/UndeleteFolder", "Folders/UpdateFolder"},
		Provider: "hierarchy", Offers: "Resource hierarchy: folders list with each folder's path, folder detail with its folders and projects, Create folder (under the organization or a folder), Rename folder, Move folder, Delete, Undelete folder (#1294)",
		Tests: []string{"TestConsoleResourceHierarchy", "TestResourceManagerV3Folders", "TestNoDetailShowsABlankProperty"}},
	{Resource: "Resource Manager folder search", Service: "resourcemanager",
		Methods:  []string{"Folders/SearchFolders"},
		Excluded: "the query form of ListFolders over the same registry; the Resource hierarchy list and its filter show the same folders"},
	{Resource: "Resource Manager organization", Service: "resourcemanager",
		Methods:  []string{"Organizations/GetOrganization", "Organizations/SearchOrganizations"},
		Provider: "hierarchy", Offers: "Resource hierarchy names the local organization, every folder's path starts at it, and it is the first parent Create folder and Move folder offer (#1294)",
		Tests: []string{"TestConsoleResourceHierarchy", "TestResourceManagerV3Organizations"}},
	{Resource: "Resource Manager tag keys", Service: "resourcemanager",
		Methods:  []string{"TagKeys/CreateTagKey", "TagKeys/DeleteTagKey", "TagKeys/GetTagKey", "TagKeys/ListTagKeys", "TagKeys/UpdateTagKey"},
		Provider: "tags", Offers: "Tags: tag keys list, a key's page with its values, Create tag key (under the organization or a project), Edit description, Delete (#1295)",
		Tests: []string{"TestConsoleTagsAndLiens", "TestResourceManagerV3Tags", "TestNoDetailShowsABlankProperty"}},
	{Resource: "Resource Manager tag values", Service: "resourcemanager",
		Methods:  []string{"TagValues/CreateTagValue", "TagValues/DeleteTagValue", "TagValues/GetTagValue", "TagValues/ListTagValues", "TagValues/UpdateTagValue"},
		Provider: "tags", Offers: "Tags: a key's Values, a value's page, Create tag value, Edit description, Delete tag value (#1295)",
		Tests: []string{"TestConsoleTagsAndLiens", "TestResourceManagerV3Tags"}},
	{Resource: "Resource Manager namespaced tag lookup", Service: "resourcemanager",
		Methods:  []string{"TagKeys/GetNamespacedTagKey", "TagValues/GetNamespacedTagValue"},
		Excluded: "GetTagKey and GetTagValue by namespaced name; the Tags list and a key's page show every namespaced name"},
	{Resource: "Resource Manager tag bindings", Service: "resourcemanager",
		Methods:  []string{"TagBindings/CreateTagBinding", "TagBindings/DeleteTagBinding", "TagBindings/ListTagBindings", "TagBindings/ListEffectiveTags"},
		Provider: "projects", Offers: "Tags on a project's and a folder's page (bound here and inherited), Bind tag, Unbind tag (#1295)",
		Tests: []string{"TestConsoleTagsAndLiens", "TestResourceManagerV3Tags", "TestNoDetailShowsABlankProperty"}},
	{Resource: "Resource Manager project IAM policy", Service: "resourcemanager",
		Methods:  []string{"Projects/GetIamPolicy", "Projects/SetIamPolicy", "Projects/TestIamPermissions"},
		Provider: "iam", Offers: "IAM & Admin > IAM (#1368): the selected project's policy, its principals and their roles, and the Permissions tab with Grant access, Remove principal and Test permissions; stored, never enforced, and inherited by nothing (ADR-0006)",
		Tests: []string{"TestConsoleProjectIAM"}},
	{Resource: "Resource Manager folder and organization IAM policies", Service: "resourcemanager",
		Methods:  []string{"Folders/GetIamPolicy", "Folders/SetIamPolicy", "Folders/TestIamPermissions", "Organizations/GetIamPolicy", "Organizations/SetIamPolicy", "Organizations/TestIamPermissions"},
		Excluded: "a folder's and the organization's policies are stored as a project's is (#1368) and read back by the API, gcloud resource-manager folders and gcloud organizations; the console's IAM page is the selected project's, as Google's is"},
	{Resource: "Resource Manager tag holds", Service: "resourcemanager",
		Methods:  []string{"TagHolds/CreateTagHold", "TagHolds/DeleteTagHold", "TagHolds/ListTagHolds"},
		Excluded: "a hold is placed by the service that uses a value, not by a person; Delete tag value names the holds that block it"},
	// v1 REST and liens (#301, #1296, #1295), the JSON-only surface: the
	// same registry the v3 methods and the console's pages use.
	{Resource: "Resource Manager v1 projects", Service: "resourcemanagerv1",
		Methods:  []string{"cloudresourcemanager.projects.list", "cloudresourcemanager.projects.get", "cloudresourcemanager.projects.create", "cloudresourcemanager.projects.update", "cloudresourcemanager.projects.delete", "cloudresourcemanager.projects.undelete"},
		Provider: "projects", Offers: "The v3 projects surface above: Project registry list, project detail, Create project, Edit labels, Delete, Undelete project; v1 is the same registry in v1's shape (#301)",
		Tests: []string{"TestResourceManagerV1Projects", "TestProjectUndeleteIsOfferedOnlyWhenDeleted", "TestNoDetailShowsABlankProperty"}},
	{Resource: "Resource Manager v1 operations", Service: "resourcemanagerv1",
		Methods:  []string{"cloudresourcemanager.operations.get"},
		Excluded: "the poll of an operation that is already done when the call returns: every v1 create, move and folder change finishes before it answers"},
	{Resource: "Resource Manager v1 project IAM policy", Service: "resourcemanagerv1",
		Methods:  []string{"cloudresourcemanager.projects.getIamPolicy", "cloudresourcemanager.projects.setIamPolicy", "cloudresourcemanager.projects.testIamPermissions"},
		Provider: "iam", Offers: "IAM & Admin > IAM: the same project policy v3's methods read and write, which gcloud projects add-iam-policy-binding and Terraform's google_project_iam_* change (#1368)",
		Tests: []string{"TestConsoleProjectIAM"}},
	{Resource: "Resource Manager v1 organization IAM policy", Service: "resourcemanagerv1",
		Methods:  []string{"cloudresourcemanager.organizations.getIamPolicy", "cloudresourcemanager.organizations.setIamPolicy", "cloudresourcemanager.organizations.testIamPermissions"},
		Excluded: "the organization's policy, stored as a project's is (#1368); the console's IAM page is the selected project's, as Google's is"},
	{Resource: "Resource Manager v1 organization and ancestry", Service: "resourcemanagerv1",
		Methods:  []string{"cloudresourcemanager.organizations.get", "cloudresourcemanager.organizations.search", "cloudresourcemanager.projects.getAncestry"},
		Provider: "hierarchy", Offers: "Resource hierarchy names the local organization, every folder's path starts at it, and a project's page shows its Parent, the first step of the ancestry getAncestry returns (#1294)",
		Tests: []string{"TestConsoleResourceHierarchy", "TestGcloudFoldersAndOrganizations"}},
	{Resource: "Resource Manager organization policies", Service: "resourcemanagerv1",
		Methods:  []string{"cloudresourcemanager.projects.listOrgPolicies", "cloudresourcemanager.projects.getOrgPolicy", "cloudresourcemanager.projects.getEffectiveOrgPolicy", "cloudresourcemanager.projects.setOrgPolicy", "cloudresourcemanager.projects.clearOrgPolicy", "cloudresourcemanager.projects.listAvailableOrgPolicyConstraints"},
		Provider: "projects", Offers: "Organization policies on a project's page, each constraint's policy as listOrgPolicies lists it (a project's effective policy is its own: it has no organization policy above it); Set organization policy, a constraint from the set listAvailableOrgPolicyConstraints answers, Enforce for a boolean one, Allowed values or Deny all for a list one; Clear organization policy (#1296)",
		Tests: []string{"TestConsoleProjectOrgPolicies", "TestResourceManagerV1OrgPolicy"}},
	{Resource: "Resource Manager liens", Service: "resourcemanagerv1",
		Methods:  []string{"cloudresourcemanager.liens.create", "cloudresourcemanager.liens.get", "cloudresourcemanager.liens.list", "cloudresourcemanager.liens.delete"},
		Provider: "projects", Offers: "Liens on a project's page, Add lien (restrictions, reason, origin), Remove lien; Delete project is refused while a lien restricts it (#1295)",
		Tests: []string{"TestConsoleTagsAndLiens", "TestResourceManagerLiens"}},

	// Memorystore for Redis (#1302)
	{Resource: "Memorystore instances", Service: "memorystore",
		Methods:  []string{"CloudRedis/ListInstances", "CloudRedis/GetInstance", "CloudRedis/CreateInstance", "CloudRedis/UpdateInstance", "CloudRedis/DeleteInstance"},
		Provider: "memorystore", Offers: "Instances list (location, version, endpoint, tier, capacity, AUTH, state) read through ListInstances; an instance's page; Create instance (ID, display name, region, capacity, AUTH, labels, Redis configs); Edit (display name, capacity, labels, Redis configs); Delete; and Flush (FLUSHALL, confirmed) on the instance `up` runs; Create instance's Enable in-transit encryption (SERVER_AUTHENTICATION), and an instance's In-transit encryption and Server CA tab with server_ca_certs' PEM, serial, fingerprint and expiry (#1413); Create instance's Customer-managed encryption key (customer_managed_key, a Cloud KMS key in the instance's region), an instance's Encryption and, while the key is disabled or destroyed, Suspension reasons (CUSTOMER_MANAGED_KEY_ISSUE); Rotate server CA, confirmed, on an instance with in-transit encryption: Memorystore's rotation on a support request, for which Google has no API (#1463)",
		Tests: []string{"TestConsoleMemorystoreFlush", "TestConsoleMemorystoreCreateEditDelete", "TestConsoleMemorystoreInTransitEncryption", "TestConsoleMemorystoreCustomerManagedKey", "TestConsoleMemorystoreRotateServerCA"}},
	{Resource: "Memorystore AUTH string", Service: "memorystore",
		Methods:  []string{"CloudRedis/GetInstanceAuthString"},
		Provider: "memorystore", Offers: "the AUTH string on the page of an instance with AUTH",
		Tests: []string{"TestConsoleMemorystoreCreateEditDelete"}},
	{Resource: "Memorystore import and export", Service: "memorystore",
		Methods:  []string{"CloudRedis/ExportInstance", "CloudRedis/ImportInstance"},
		Provider: "memorystore", Offers: "Export and Import (confirmed: it overwrites) on a created instance's page, to and from a gs:// RDB file in this instance's Cloud Storage, each waiting for its operation; Edit's Persistence sets RDB snapshots (#1384)",
		Tests: []string{"TestConsoleMemorystoreImportExport"}},
	{Resource: "Memorystore versions and upgrade", Service: "memorystore",
		Methods:  []string{"CloudRedis/UpgradeInstance"},
		Provider: "memorystore", Offers: "Create instance's Version (REDIS_6_X, REDIS_7_0, REDIS_7_2) and Upgrade, confirmed, on a created instance's page, to a later version only, waiting for its operation (#1386)",
		Tests: []string{"TestConsoleMemorystoreUpgrade"}},
	{Resource: "Memorystore Standard tier and failover", Service: "memorystore",
		Methods:  []string{"CloudRedis/FailoverInstance"},
		Provider: "memorystore", Offers: "Create instance's Tier (BASIC or STANDARD_HA), Enable read replicas and Replicas; a Standard tier instance's page with its replicas, read endpoint, primary and alternative zones and nodes; Failover, confirmed, with LIMITED_DATA_LOSS or FORCE_DATA_LOSS, waiting for its operation; Edit's Replicas (UpdateInstance replica_count) or Enable read replicas (#1385)",
		Tests: []string{"TestConsoleMemorystoreStandardTier"}},
	{Resource: "Memorystore maintenance reschedule", Service: "memorystore",
		Methods:  []string{"CloudRedis/RescheduleMaintenance"},
		Excluded: "CloudBurrow never schedules maintenance, so RescheduleMaintenance always answers FAILED_PRECONDITION: a Reschedule button would only ever fail (#1386)"},
	// Cloud Scheduler
	{Resource: "Cloud Scheduler jobs", Service: "scheduler",
		Methods:  []string{"CloudScheduler/CreateJob", "CloudScheduler/DeleteJob", "CloudScheduler/GetJob", "CloudScheduler/ListJobs", "CloudScheduler/PauseJob", "CloudScheduler/ResumeJob", "CloudScheduler/RunJob"},
		Provider: "scheduler", Offers: "Jobs list, job detail, Create job (HTTP and Pub/Sub targets), Pause / Resume, Force run, Delete; an HTTP target's Auth header on Create job and Edit job: an OIDC token (service account, audience) or an OAuth token (service account, scope), shown on the job's page (#1243)",
		Tests: []string{"TestConsoleSchedulerFollowsTheSDK", "TestSchedulerPauseFromARow", "TestConsoleTaskAndJobTokens"}},
	{Resource: "Cloud Scheduler locations", Service: "scheduler",
		Methods:  []string{"Locations/ListLocations"},
		Provider: "scheduler", Offers: "Create job: the Region is chosen from the listed locations (#1237)",
		Tests: []string{"TestSchedulerCreateFormOffersListLocations", "TestSchedulerLocations"}},
	{Resource: "Cloud Scheduler location read", Service: "scheduler",
		Methods:  []string{"Locations/GetLocation"},
		Excluded: "a read of one listed location; the Create job form's Region shows every location ListLocations lists"},
	{Resource: "Cloud Scheduler job edit", Service: "scheduler",
		Methods:  []string{"CloudScheduler/UpdateJob"},
		Provider: "scheduler", Offers: "Edit job on a job's page: description, schedule, time zone, the target's fields and retry configuration, with the name, region and target type fixed (#795)",
		Tests: []string{"TestConsoleSchedulerEditJob", "TestSchedulerEditJobThroughUpdateJob", "TestSchedulerEditJobThroughTheForm"}},

	// Cloud Logging
	{Resource: "Cloud Logging entries", Service: "logging",
		Methods: []string{"LoggingServiceV2/ListLogEntries", "LoggingServiceV2/ListLogs"},
		Screen:  "/logs", Offers: "Logs Explorer: entries under source `logging/<log>`, filters in the URL, live stream; scoped to a folder or organization, its own entries (#1395); or Through a log view: a bucket and view of a project, folder, organization or billing account, read with the view's filter and the bucket's retention applied, the note naming both (#1410)",
		Tests: []string{"TestLoggingWriteAndRead", "TestFiltersNarrowTheView", "TestGcloudLoggingFolderAndOrganizationSinks", "TestParentScopeShowsThatParentsOwnEntries",
			"TestConsoleLogExplorerThroughView", "TestConsoleLogsExplorerThroughView", "TestLogsExplorerThroughViewAndParentPicker"}},
	{Resource: "Cloud Logging tail", Service: "logging",
		Methods: []string{"LoggingServiceV2/TailLogEntries"},
		Screen:  "/logs", Offers: "Logs Explorer's live stream, which shows each entry as it is written, with Pause / Resume (#1215); through a log view, the view's own TailLogEntries (#1410)",
		Tests: []string{"TestTheLogStreamStatesItsOwnHealth", "TestLoggingWriteAndRead", "TestConsoleLogExplorerThroughView", "TestConsoleLogsExplorerThroughView",
			"TestLogsExplorerThroughViewAndParentPicker"}},
	{Resource: "Cloud Logging resource descriptors", Service: "logging",
		Methods:  []string{"LoggingServiceV2/ListMonitoredResourceDescriptors"},
		Excluded: "a fixed catalogue of resource types for clients to read; the Logs Explorer shows each entry's resource"},
	{Resource: "Cloud Logging writes", Service: "logging",
		Methods:  []string{"LoggingServiceV2/WriteLogEntries"},
		Excluded: "the application's write path; the Logs Explorer shows what was written"},
	{Resource: "Cloud Logging log-based metrics", Service: "logging",
		Methods: []string{"MetricsServiceV2/CreateLogMetric", "MetricsServiceV2/DeleteLogMetric", "MetricsServiceV2/GetLogMetric",
			"MetricsServiceV2/ListLogMetrics", "MetricsServiceV2/UpdateLogMetric"},
		Provider: "logging-metrics", Offers: "Log-based metrics list and detail (configuration, matching entries over the last hour and day), Create metric (counter or distribution: filter, unit, value extractor, buckets, labels, disabled), Edit metric, Delete (#1226)",
		Tests: []string{"TestConsoleLoggingMetrics", "TestLoggingAdminMetricsThroughTheBrowser", "TestLoggingLogBasedMetrics"}},
	{Resource: "Cloud Logging sinks", Service: "logging",
		Methods: []string{"ConfigServiceV2/CreateSink", "ConfigServiceV2/DeleteSink", "ConfigServiceV2/GetSink",
			"ConfigServiceV2/ListSinks", "ConfigServiceV2/UpdateSink"},
		Provider: "logging-sinks", Offers: "Log router sinks list and detail, Create sink (destination, filter, description, disabled), Edit sink, Delete (#1226); each sink's writer identity and delivery status: entries delivered, the last delivery, entries waiting for the hourly Cloud Storage file, the last error (#1404), entries a BigQuery sink wrote to its dataset's export_errors table (#1451)",
		Tests: []string{"TestConsoleLoggingSinksAndExclusions", "TestLoggingSinks"}},
	{Resource: "Cloud Logging exclusions", Service: "logging",
		Methods: []string{"ConfigServiceV2/CreateExclusion", "ConfigServiceV2/DeleteExclusion", "ConfigServiceV2/GetExclusion",
			"ConfigServiceV2/ListExclusions", "ConfigServiceV2/UpdateExclusion"},
		Provider: "logging-exclusions", Offers: "Log exclusions list and detail, Create exclusion (filter, description, disabled), Edit exclusion, Delete (#1226)",
		Tests: []string{"TestConsoleLoggingSinksAndExclusions", "TestLoggingExclusions"}},
	{Resource: "Cloud Logging buckets", Service: "logging",
		Methods: []string{"ConfigServiceV2/CreateBucket", "ConfigServiceV2/DeleteBucket", "ConfigServiceV2/GetBucket",
			"ConfigServiceV2/ListBuckets", "ConfigServiceV2/UndeleteBucket", "ConfigServiceV2/UpdateBucket"},
		Provider: "logging-buckets", Offers: "Logs Storage list (_Default, _Required and user buckets) and bucket detail, Create log bucket (name, region, description, retention), Edit bucket (description, retention, lock, Log Analytics), Delete, Undelete on a bucket pending deletion (#1264, #1292); the page's retention is what reads through its views apply (#1410); a Parent picker (the toolbar's project, the organization, its folders, and the billing accounts Logging holds entries or configuration for, or any billing account named in the address as ?parent=) lists and changes a folder's, organization's or billing account's buckets (#1411)",
		Tests: []string{"TestConsoleLoggingBuckets", "TestLoggingBucketsAndViews", "TestConsoleLogBucketsParentScope", "TestConsoleLoggingCopyFromParents"}},
	{Resource: "Cloud Logging views", Service: "logging",
		Methods:  []string{"ConfigServiceV2/GetView", "ConfigServiceV2/ListViews"},
		Provider: "logging-buckets", Offers: "Views section on a bucket's page: each view's ID, filter and description (#1264), its row opening the Logs Explorer through it (#1410)",
		Tests: []string{"TestConsoleLoggingBuckets", "TestLoggingBucketsAndViews"}},
	{Resource: "Cloud Logging async bucket calls", Service: "logging",
		Methods:  []string{"ConfigServiceV2/CreateBucketAsync", "ConfigServiceV2/UpdateBucketAsync"},
		Excluded: "the long-running forms of CreateBucket and UpdateBucket, which Create log bucket and Edit bucket call"},
	{Resource: "Cloud Logging links", Service: "logging",
		Methods:  []string{"ConfigServiceV2/CreateLink", "ConfigServiceV2/DeleteLink", "ConfigServiceV2/ListLinks"},
		Provider: "logging-buckets", Offers: "Linked datasets section on a bucket's page; Create link and Delete link on a bucket with Log Analytics (#1292); a link's row opens Log Analytics, BigQuery's query editor on the linked dataset with a query of its _AllLogs view filled in (#1423)",
		Tests: []string{"TestConsoleLoggingLinks", "TestLoggingLinksSettingsAndAsyncBuckets", "TestLoggingLogAnalyticsLinkedDataset"}},
	{Resource: "Cloud Logging copy", Service: "logging",
		Methods:  []string{"ConfigServiceV2/CopyLogEntries"},
		Provider: "logging-buckets", Offers: "Copy log entries (Cloud Storage bucket, filter) on an active bucket's page, a Copy operations section listing its copies (destination, filter, state, progress, entries copied, started) and Cancel copy on one unfinished; the label is CloudBurrow's, as Google documents copying through gcloud alone (#1394); on a folder's, organization's or billing account's bucket too, chosen with the Parent picker (#1411)",
		Tests: []string{"TestConsoleLoggingCopy", "TestLoggingCopyLogEntries", "TestConsoleLogBucketsParentScope", "TestConsoleLoggingCopyFromParents",
			"TestLogsExplorerThroughViewAndParentPicker"}},
	{Resource: "Cloud Logging link read", Service: "logging",
		Methods:  []string{"ConfigServiceV2/GetLink"},
		Excluded: "a read of one link; the Linked datasets section shows every field ListLinks returns"},
	{Resource: "Cloud Logging settings", Service: "logging",
		Methods:  []string{"ConfigServiceV2/GetCmekSettings", "ConfigServiceV2/GetSettings", "ConfigServiceV2/UpdateSettings"},
		Provider: "logging-settings", Offers: "Logs Storage settings page (/logs/settings): the project's settings (storage location, default sink, KMS key and service account) and its CMEK settings as served; Edit settings (storage location, disable the default sink, KMS key, which the API refuses) (#1311)",
		Tests: []string{"TestConsoleLoggingSettings", "TestLoggingLinksSettingsAndAsyncBuckets"}},
	{Resource: "Cloud Logging CMEK settings change", Service: "logging",
		Methods:  []string{"ConfigServiceV2/UpdateCmekSettings"},
		Excluded: "its only accepted change is clearing a KMS key that is never set; a key is UNIMPLEMENTED, and the settings page shows the CMEK settings"},
	{Resource: "Cloud Logging view changes", Service: "logging",
		Methods:  []string{"ConfigServiceV2/CreateView", "ConfigServiceV2/DeleteView", "ConfigServiceV2/UpdateView"},
		Provider: "logging-buckets", Offers: "Create view (ID, filter, description) on a bucket's page, Edit (filter, description) and Delete on a view's row; not on _AllLogs or any of _Required's (#1283)",
		Tests: []string{"TestConsoleLoggingViews", "TestLoggingViewsThroughTheBrowser", "TestLoggingBucketsAndViews"}},
	{Resource: "Cloud Logging log delete", Service: "logging",
		Methods: []string{"LoggingServiceV2/DeleteLog"},
		Screen:  "/logs", Offers: "Delete log on a `logging/<log>` entry of the selected project, confirmed by typing the log ID; not offered on pod logs or CloudBurrow's own request log, which the Logging API does not hold (#799)",
		Tests: []string{"TestConsoleDeleteLogThroughDeleteLog", "TestConsoleDeleteLogIsSeenByTheOfficialClient"}},

	// Pub/Sub
	{Resource: "Pub/Sub topics", Service: "pubsub",
		Methods:  []string{"Publisher/CreateTopic", "Publisher/DeleteTopic", "Publisher/GetTopic", "Publisher/ListTopics", "Publisher/ListTopicSubscriptions", "Publisher/Publish"},
		Provider: "pubsub", Offers: "Topics list, topic detail with its subscriptions, Create topic (with a default subscription, message retention and schema, #852), Delete, Publish message",
		Tests: []string{"TestConsoleCreatedTopicIsVisibleToTheOfficialSDK", "TestSDKCreatedTopicAppearsInTheConsole", "TestConsolePubSubActions",
			"TestConsolePubSubCreateOptions", "TestPubSubCreateOptionsReachTheAPI", "TestPubSubCreateOptionsThroughTheForms"}},
	{Resource: "Pub/Sub subscriptions", Service: "pubsub",
		Methods:  []string{"Subscriber/CreateSubscription", "Subscriber/DeleteSubscription", "Subscriber/GetSubscription", "Subscriber/ListSubscriptions", "Subscriber/Pull", "Subscriber/Acknowledge"},
		Provider: "pubsub-subscriptions", Offers: "Subscriptions list and detail, with how long a subscription was idle and when expiration deletes it, read from CloudBurrow's front (#996), Delete; on a topic page: Create subscription (push endpoint and attributes, #996; ack deadline, retention, retained acknowledged messages, message ordering, filter, retry and dead-letter policies, #852; exactly-once delivery and expiration period, #873), Pull and ack, Pull without ack; Advance clock (CloudBurrow extension) on a subscription's page, confirmed, which moves the front's clock for the whole instance and lists the subscriptions that expired (#1040)",
		Tests: []string{"TestConsolePubSubSubscriptionsScreen", "TestConsolePubSubDeleteSubscription", "TestConsolePubSubActions", "TestSubscriptionsDeleteConfirmedByName",
			"TestConsolePubSubCreateOptions", "TestPubSubCreateOptionsReachTheAPI", "TestPubSubCreateSubscriptionDoesNotRetryAFilterRefusal",
			"TestPubSubCreateOptionsThroughTheForms", "TestPubSubSubscriptionPageShowsIdleTimeAndExpiry", "TestFormatIdle",
			"TestConsolePubSubIdleTimeAndPushAttributes", "TestPubSubIdleTimeAndPushAttributesThroughTheForms",
			"TestConsolePubSubAdvanceClock", "TestPubSubAdvanceClockReportsWhatExpired", "TestPubSubAdvanceClockThroughTheForm"}},
	{Resource: "Pub/Sub streaming pull and lease extension", Service: "pubsub",
		Methods:  []string{"Subscriber/StreamingPull", "Subscriber/ModifyAckDeadline"},
		Excluded: "a subscriber client's transport and lease management; the console pulls with Pull and either acknowledges at once or leaves the messages unacked"},
	{Resource: "Pub/Sub topic edit", Service: "pubsub",
		Methods:  []string{"Publisher/UpdateTopic"},
		Provider: "pubsub", Offers: "Edit topic on a topic's page: message retention, the one field the emulator's UpdateTopic applies (#786); labels, applied by CloudBurrow's front (#949) and held to Google's rules (#962); schema and message encoding, AVRO or PROTOCOL_BUFFER, or cleared (#1446)",
		Tests: []string{"TestConsolePubSubEditTopicAndSubscription", "TestPubSubEditTopicThroughUpdateTopic", "TestPubSubFormsHoldLabelsToGoogleRules", "TestConsolePubSubDetachAndProtobufSchema"}},
	{Resource: "Pub/Sub subscription edit", Service: "pubsub",
		Methods:  []string{"Subscriber/UpdateSubscription"},
		Provider: "pubsub-subscriptions", Offers: "Edit subscription on a subscription's page: push endpoint and attributes (pull to push and back), ack deadline, retention, retained acknowledged messages, retry and dead-letter policies, exactly-once delivery (#880); the expiration period, which CloudBurrow's front applies (#891); filter and ordering shown fixed with the reason (#786); labels, applied by CloudBurrow's front (#949) and held to Google's rules (#962); on a detached subscription too, its configuration kept by CloudBurrow's front (#1446)",
		Tests: []string{"TestConsolePubSubEditTopicAndSubscription", "TestConsolePubSubDetachAndProtobufSchema", "TestPubSubEditSubscriptionThroughUpdateSubscription", "TestPubSubEditSubscriptionThroughTheForm", "TestPubSubFormsHoldLabelsToGoogleRules",
			"TestPubSubEditSubscriptionExpirationThroughTheForm", "TestPubSubEditSubscriptionExactlyOnceThroughTheForm"}},
	{Resource: "Pub/Sub push configuration", Service: "pubsub",
		Methods:  []string{"Subscriber/ModifyPushConfig"},
		Excluded: "the one-field form of Edit subscription's push endpoint, which the console changes through UpdateSubscription's push_config, switching pull to push and back"},
	{Resource: "Pub/Sub snapshots and seek", Service: "pubsub",
		Methods:  []string{"Subscriber/CreateSnapshot", "Subscriber/DeleteSnapshot", "Subscriber/GetSnapshot", "Subscriber/ListSnapshots", "Subscriber/Seek", "Subscriber/UpdateSnapshot", "Publisher/ListTopicSnapshots"},
		Provider: "pubsub-snapshots", Offers: "Snapshots list and detail, Edit snapshot (labels and expiry, applied by CloudBurrow's front, #1406), Delete; on a subscription page: Create snapshot, Seek to snapshot, Seek to time; on a topic page: its Snapshots",
		Tests: []string{"TestConsolePubSubSnapshotsAndSeek", "TestPubSubSnapshotSeekAndDeleteThroughTheForms"}},
	{Resource: "Pub/Sub subscription detach", Service: "pubsub",
		Methods:  []string{"Publisher/DetachSubscription"},
		Provider: "pubsub-subscriptions", Offers: "Detach from topic on a subscription's page, confirmed, carried out by CloudBurrow's front since the emulator refuses it (#1406)",
		Tests: []string{"TestConsolePubSubDetachAndProtobufSchema"}},
	{Resource: "Pub/Sub topic IAM policy", Service: "pubsub",
		Methods:  []string{"pubsub.projects.topics.getIamPolicy", "pubsub.projects.topics.setIamPolicy", "pubsub.projects.topics.testIamPermissions"},
		Provider: "pubsub", Offers: "Permissions tab on a topic's page: principals and roles, Grant access, Remove principal and Test permissions, with the note that nothing is enforced (ADR-0006, #1367)",
		Tests: []string{"TestConsolePermissionsOnATopicAndASubscription", "TestPubSubIAMHandle"}},
	{Resource: "Pub/Sub subscription IAM policy", Service: "pubsub",
		Methods:  []string{"pubsub.projects.subscriptions.getIamPolicy", "pubsub.projects.subscriptions.setIamPolicy", "pubsub.projects.subscriptions.testIamPermissions"},
		Provider: "pubsub-subscriptions", Offers: "Permissions tab on a subscription's page: principals and roles, Grant access, Remove principal and Test permissions, with the note that nothing is enforced (ADR-0006, #1367)",
		Tests: []string{"TestConsolePermissionsOnATopicAndASubscription", "TestPubSubIAMHandle"}},
	{Resource: "Pub/Sub snapshot and schema IAM policy", Service: "pubsub",
		Methods: []string{"pubsub.projects.snapshots.getIamPolicy", "pubsub.projects.snapshots.setIamPolicy", "pubsub.projects.snapshots.testIamPermissions",
			"pubsub.projects.schemas.getIamPolicy", "pubsub.projects.schemas.setIamPolicy", "pubsub.projects.schemas.testIamPermissions"},
		Excluded: "Google's console has no Permissions panel on a snapshot or a schema; their policies are set by gcloud, Terraform (google_pubsub_schema_iam_*) or a client, stored and not enforced (ADR-0006, #1367)"},
	{Resource: "Pub/Sub schemas", Service: "pubsub",
		Methods: []string{"SchemaService/CommitSchema", "SchemaService/CreateSchema", "SchemaService/DeleteSchema", "SchemaService/DeleteSchemaRevision", "SchemaService/GetSchema",
			"SchemaService/ListSchemaRevisions", "SchemaService/ListSchemas", "SchemaService/RollbackSchema", "SchemaService/ValidateMessage", "SchemaService/ValidateSchema"},
		Provider: "pubsub-schemas", Offers: "Schemas list and detail (definition, revisions), Create schema (Avro, or Protocol Buffer served by CloudBurrow's front, #1406; validated first), Delete; on a schema page: Commit revision, Test message, Delete schema; on a revision row: Roll back to this revision, Delete revision",
		Tests: []string{"TestConsolePubSubSchemas", "TestPubSubSchemaCreateShowsTheValidationError", "TestConsolePubSubDetachAndProtobufSchema"}},

	// Cloud Storage
	{Resource: "Cloud Storage buckets", Service: "storage",
		Methods:  []string{"storage.buckets.insert", "storage.buckets.delete", "storage.buckets.get", "storage.buckets.list"},
		Provider: "storage", Offers: "Buckets list, bucket detail (configuration: retention, soft delete, lifecycle, labels), Create (labels, default storage class, uniform access, versioning, soft delete retention, object retention; #852), Delete",
		Tests: []string{"TestConsoleCreatedBucketIsVisibleToTheOfficialSDK", "TestCreateBucketThroughTheForm", "TestConsoleStorageShowsRetentionAndLifecycle",
			"TestConsoleStorageCreateBucketOptions", "TestStorageCreateBucketOptionsThroughTheAPI", "TestStorageCreateBucketOptionsThroughTheForm"}},
	{Resource: "Cloud Storage objects", Service: "storage",
		Methods:  []string{"storage.objects.list", "storage.objects.get", "storage.objects.insert", "storage.objects.delete"},
		Provider: "storage", Offers: "Objects and prefixes in the bucket browser, Upload file, Download, Preview, Delete; Show versions on the object list (versions=true), each generation's own page, and Delete version on a generation, confirmed by the object's name (#853)",
		Tests: []string{"TestConsoleStorageObjects", "TestConsoleStorageObjectVersionsHoldsAndRetention", "TestStorageObjectVersionsListOpenRestoreDelete", "TestStorageObjectVersionsThroughTheBrowser"}},
	{Resource: "Cloud Storage storage layout", Service: "storage",
		Methods:  []string{"storage.buckets.getStorageLayout"},
		Excluded: "a capability read a client makes to choose its request paths (location and hierarchical namespace); it is no resource or action of its own"},
	{Resource: "Cloud Storage bucket settings and retention lock", Service: "storage",
		Methods:  []string{"storage.buckets.patch", "storage.buckets.lockRetentionPolicy"},
		Provider: "storage", Offers: "Edit bucket on a bucket's page (labels, default storage class, versioning, soft delete retention, retention period, default event-based hold, lifecycle rules, CORS); Lock retention policy while a policy is unlocked, confirmed by the bucket's name after a warning that it is permanent (#789); Run lifecycle now, labelled a CloudBurrow extension, which is CloudBurrow's POST /_cloudburrow/lifecycle and shows what it did (#853)",
		Tests: []string{"TestConsoleStorageBucketSettings", "TestStorageBucketSettingsThroughTheForm", "TestStorageBucketSettingsThroughTheAPI", "TestStorageRunLifecycleNow"}},
	{Resource: "Cloud Storage soft-deleted objects", Service: "storage",
		Methods:  []string{"storage.objects.restore", "storage.objects.bulkRestore"},
		Provider: "storage", Offers: "Deleted objects tab on a bucket's page (generation, size, deleted and hard delete times), Restore on each (#789); Bulk restore deleted objects on a bucket's page (globs, a deleted-after and -before window, allow overwrite, copy source ACL), answering with its operation and counts (#1279)",
		Tests: []string{"TestConsoleStorageBucketSettings", "TestStorageSoftDeletedRestoreThroughTheAPI", "TestStorageBulkRestoreThroughTheConsole"}},
	{Resource: "Cloud Storage soft-deleted buckets", Service: "storage",
		Methods:  []string{"storage.buckets.restore"},
		Provider: "storage-deleted", Offers: "Deleted buckets page under Cloud Storage (generation, deleted and hard delete times), Restore on each (#789)",
		Tests: []string{"TestConsoleStorageBucketSettings", "TestStorageSoftDeletedRestoreThroughTheAPI"}},
	{Resource: "Cloud Storage managed folders", Service: "storage",
		Methods:  []string{"storage.managedFolders.list", "storage.managedFolders.insert", "storage.managedFolders.get", "storage.managedFolders.delete"},
		Provider: "storage", Offers: "Managed folders in the bucket browser, as folders whose type is Managed folder (#789); Create managed folder on a bucket's page and each folder's, under its prefix, and Delete managed folder on a managed folder's row, confirmed by its name after a warning that the objects under it are kept (#828)",
		Tests: []string{"TestStorageBrowserMarksManagedFolders", "TestStorageBrowserListsManagedFoldersFromTheServer", "TestStorageBrowserCreatesAndDeletesManagedFolders", "TestConsoleStorageManagedFolders", "TestStorageManagedFoldersThroughTheBrowser"}},
	{Resource: "Cloud Storage hierarchical-namespace folders", Service: "storage",
		Methods:  []string{"storage.folders.insert", "storage.folders.get", "storage.folders.list", "storage.folders.delete", "storage.folders.rename", "storage.folders.deleteRecursive"},
		Provider: "storage", Offers: "Enable hierarchical namespace on the Create form, which holds uniform bucket-level access on and versioning off; in such a bucket, its folders in the bucket browser from folders.list, typed Folder, each folder's page summarised from folders.get; Create folder on the bucket's page and each folder's; on a folder's row Rename folder (an operation, shown when it returns), Delete folder while it is empty, and Delete folder and contents, confirmed by the folder's name (#1203)",
		Tests: []string{"TestStorageCreateFormHierarchicalNamespace", "TestStorageHierarchicalFoldersThroughTheConsole", "TestConsoleStorageHierarchicalFolders", "TestStorageHierarchicalFoldersThroughTheBrowser"}},
	{Resource: "Cloud Storage bucket operations", Service: "storage",
		Methods:  []string{"storage.buckets.operations.list", "storage.buckets.operations.get"},
		Provider: "storage", Offers: "Operations tab on a hierarchical-namespace bucket's page, and on any bucket once it has a bulk restore (type, state, folder, destination, created, ended), each operation's own page (#1203, #1279); Filter operations on a bucket's page, an AIP-160 filter answering with the operations it keeps (#1280)",
		Tests: []string{"TestStorageHierarchicalFoldersThroughTheConsole", "TestConsoleStorageHierarchicalFolders", "TestStorageHierarchicalFoldersThroughTheBrowser", "TestStorageFilterOperationsThroughTheConsole"}},
	{Resource: "Cloud Storage Anywhere Caches, relocation and operation cancel", Service: "storage",
		Methods: []string{"storage.anywhereCaches.insert", "storage.anywhereCaches.get", "storage.anywhereCaches.list", "storage.anywhereCaches.update",
			"storage.anywhereCaches.pause", "storage.anywhereCaches.resume", "storage.anywhereCaches.disable",
			"storage.buckets.relocate", "storage.buckets.operations.advanceRelocateBucket", "storage.buckets.operations.cancel"},
		Provider: "storage", Offers: "Anywhere caches tab on a bucket's page (zone, state, TTL, admission policy, ingest on write, pending update), noting that no cache tier exists locally; Create Anywhere Cache and Change Anywhere Cache (pause, resume, disable, update) on a bucket's page; Relocate bucket (location, placement, dry run), Finalize relocation and Cancel operation, whose operations show on the Operations tab (#1444)",
		Tests: []string{"TestStorageAnywhereCachesThroughTheConsole"}},
	{Resource: "Cloud Storage full bucket replace", Service: "storage",
		Methods:  []string{"storage.buckets.update"},
		Excluded: "the replace-everything form of Edit bucket, which is buckets.patch: it changes the fields the form holds without clearing the ones it does not show"},
	{Resource: "Cloud Storage object copy, move, compose, rewrite, metadata", Service: "storage",
		Methods:  []string{"storage.objects.rewrite", "storage.objects.move", "storage.objects.compose", "storage.objects.patch"},
		Provider: "storage", Offers: "Object page (generation, metageneration, hashes, headers, custom metadata, holds); Copy, Move or rename, Edit metadata, Edit storage class on an object's row and page; Compose on checked objects; an existing destination replaced only when asked and confirmed (#790); Restore as live version on a noncurrent generation (rewrite); Edit holds and custom time on an object's and a generation's page, and Edit object retention (mode, retain-until, overrideUnlockedRetention) where the bucket has object retention (patch) (#853)",
		Tests: []string{"TestConsoleStorageObjectOperations", "TestStorageObjectMetadataAndCopyThroughTheForms", "TestStorageObjectActionsThroughTheAPI", "TestConsoleStorageObjectVersionsHoldsAndRetention", "TestStorageObjectHoldsAndRetention"}},
	{Resource: "Cloud Storage single-request copy and full metadata replace", Service: "storage",
		Methods:  []string{"storage.objects.copy", "storage.objects.update"},
		Excluded: "the one-request and replace-everything forms of edits the console makes another way: Copy is objects.rewrite, as the official client's Copier sends it, and Edit metadata is objects.patch, which changes the fields the form holds without clearing the ones it does not show"},
	// The gRPC API (#1321) is a second transport onto the JSON API's store,
	// answered by its handlers, so the Cloud Storage pages, which use the
	// JSON API, show and change what gRPC does.
	{Resource: "Cloud Storage buckets over gRPC", Service: "storagegrpc",
		Methods:  []string{"Storage/CreateBucket", "Storage/GetBucket", "Storage/ListBuckets", "Storage/UpdateBucket", "Storage/DeleteBucket", "Storage/LockBucketRetentionPolicy"},
		Provider: "storage", Offers: "the same Cloud Storage pages as the JSON API's buckets.insert, get, list, patch, delete and lockRetentionPolicy: Buckets list, bucket detail, Create, Edit bucket, Lock retention policy, Delete; a bucket made or changed over gRPC shows there, as both read one store",
		Tests: []string{"TestConsoleCreatedBucketIsVisibleToTheOfficialSDK", "TestConsoleStorageBucketSettings", "TestStorageGRPCBuckets"}},
	{Resource: "Cloud Storage objects over gRPC", Service: "storagegrpc",
		Methods: []string{"Storage/BidiWriteObject", "Storage/StartResumableWrite", "Storage/ReadObject", "Storage/BidiReadObject", "Storage/GetObject",
			"Storage/ListObjects", "Storage/UpdateObject", "Storage/DeleteObject", "Storage/ComposeObject", "Storage/RewriteObject", "Storage/MoveObject", "Storage/RestoreObject"},
		Provider: "storage", Offers: "the same Cloud Storage pages as the JSON API's objects methods: the bucket browser, Upload file, Download, Preview, the object page, Edit metadata, Copy, Move or rename, Compose, Delete, and Restore on the Deleted objects tab",
		Tests: []string{"TestConsoleStorageObjects", "TestConsoleStorageObjectOperations", "TestStorageGRPCObjects"}},
	{Resource: "Cloud Storage bucket IAM over gRPC", Service: "storagegrpc",
		Methods:  []string{"Storage/GetIamPolicy", "Storage/SetIamPolicy", "Storage/TestIamPermissions"},
		Provider: "storage", Offers: "the same Permissions tab on a bucket's page as the JSON API's buckets.getIamPolicy, setIamPolicy and testIamPermissions: principals and roles, Grant access, Remove principal, Test permissions, the one stored policy",
		Tests: []string{"TestConsolePermissionsOnABucket", "TestTestPermissionsThroughTheTab", "TestStorageGRPCBuckets"}},
	{Resource: "Cloud Storage gRPC resumable write steps", Service: "storagegrpc",
		Methods:  []string{"Storage/WriteObject", "Storage/QueryWriteStatus", "Storage/CancelResumableWrite"},
		Excluded: "steps of an upload a client takes to resume or abandon a broken stream; the console's Upload file is the JSON API's upload, and an upload is no resource of its own"},
	{Resource: "Cloud Storage notifications", Service: "storage",
		Methods:  []string{"storage.notifications.insert", "storage.notifications.get", "storage.notifications.list", "storage.notifications.delete"},
		Provider: "storage", Offers: "Notifications tab on a bucket's page (topic, event types, object name prefix, payload format, custom attributes); a notification's own page; Create notification, its topic chosen from the project's Pub/Sub topics, offered only where the storage server delivers to Pub/Sub; Delete on each row and page, confirmed by name (#791)",
		Tests: []string{"TestConsoleStorageNotifications", "TestStorageNotificationsThroughTheForm", "TestStorageNotificationsThroughTheAPI", "TestStorageNotificationsWithoutPubSub"}},
	{Resource: "Cloud Storage object change notification channels", Service: "storage",
		Methods:  []string{"storage.channels.stop"},
		Provider: "storage", Offers: "Channels tab on a bucket's page listing its active objects.watchAll channels (channel ID, address, the objects.list selection it watches (#1393), expiry, messages, created); Stop on each row, through channels.stop (#1281)",
		Tests: []string{"TestConsoleStorageChannels"}},
	{Resource: "Cloud Storage ACLs", Service: "storage",
		Methods: []string{"storage.bucketAccessControls.list", "storage.bucketAccessControls.update", "storage.bucketAccessControls.delete",
			"storage.defaultObjectAccessControls.list", "storage.defaultObjectAccessControls.update", "storage.defaultObjectAccessControls.delete",
			"storage.objectAccessControls.list", "storage.objectAccessControls.update", "storage.objectAccessControls.delete"},
		Provider: "storage", Offers: "Access control tab on a bucket's page (its ACL and its default object ACL: entity, role, which ACL) and on an object's page (its ACL), shown while uniform bucket-level access is off; Add ACL entry on both pages (entity, role); Remove on each row, confirmed by the entity; a predefined Default object ACL on Edit bucket (predefinedDefaultObjectAcl); stored, never enforced (#1233)",
		Tests: []string{"TestStorageConsoleACLs", "TestStorageACLsThroughTheBrowser"}},
	{Resource: "Cloud Storage single ACL entry reads and partial writes", Service: "storage",
		Methods: []string{"storage.bucketAccessControls.get", "storage.bucketAccessControls.insert", "storage.bucketAccessControls.patch",
			"storage.defaultObjectAccessControls.get", "storage.defaultObjectAccessControls.insert", "storage.defaultObjectAccessControls.patch",
			"storage.objectAccessControls.get", "storage.objectAccessControls.insert", "storage.objectAccessControls.patch"},
		Excluded: "other forms of what the Access control tab does another way: it lists every entry with list, and adds or changes one with update, as the official client's ACLHandle.Set sends it"},
	{Resource: "Cloud Storage HMAC keys and service account", Service: "storage",
		Methods:  []string{"storage.projects.hmacKeys.create", "storage.projects.hmacKeys.delete", "storage.projects.hmacKeys.get", "storage.projects.hmacKeys.list", "storage.projects.hmacKeys.update", "storage.projects.serviceAccount.get"},
		Provider: "storage-settings", Offers: "Settings page (/storage/settings): the project's Cloud Storage service account; HMAC keys list, Create key (the secret shown once, with Copy, recorded nowhere), Activate / Deactivate, Delete on an inactive key only (#792)",
		Tests: []string{"TestStorageSettingsHMACKeysThroughTheConsole", "TestConsoleStorageHMACKeys", "TestStorageHMACKeySecretShownOnce"}},

	// IAM, which four services serve the same way.
	{Resource: "Cloud Tasks IAM policy", Service: "tasks",
		Methods:  []string{"CloudTasks/GetIamPolicy", "CloudTasks/SetIamPolicy"},
		Provider: "tasks", Offers: "Permissions tab on a queue's page: principals and roles, Grant access, Remove principal, with the note that nothing is enforced (#793)",
		Tests: []string{"TestConsolePermissionsOnAQueue", "TestTasksPermissionsThroughGetAndSetIamPolicy", "TestTasksPermissionsThroughTheTab"}},
	{Resource: "Secret Manager IAM policy", Service: "secretmanager",
		Methods:  []string{"SecretManagerService/GetIamPolicy", "SecretManagerService/SetIamPolicy"},
		Provider: "secrets", Offers: "Permissions tab on a secret's page: principals and roles, Grant access, Remove principal, with the note that nothing is enforced (#793)",
		Tests: []string{"TestConsolePermissionsOnASecret", "TestSecretPermissionsThroughGetAndSetIamPolicy"}},
	{Resource: "Cloud KMS IAM policy", Service: "kms",
		Methods:  []string{"IAMPolicy/GetIamPolicy", "IAMPolicy/SetIamPolicy"},
		Provider: "kms", Offers: "Permissions tab on a key ring's page and a key's: principals and roles, Grant access, Remove principal, with the note that nothing is enforced (#793)",
		Tests: []string{"TestConsolePermissionsOnAKeyRingAndKey", "TestKMSPermissionsOnARingAndAKey"}},
	{Resource: "Cloud Storage bucket IAM policy", Service: "storage",
		Methods:  []string{"storage.buckets.getIamPolicy", "storage.buckets.setIamPolicy"},
		Provider: "storage", Offers: "Permissions tab on a bucket's page: principals and roles, Grant access, Remove principal, with the note that nothing is enforced (#793)",
		Tests: []string{"TestConsolePermissionsOnABucket", "TestStoragePermissionsThroughBucketIAM"}},
	{Resource: "Cloud Storage managed folder IAM policy", Service: "storage",
		Methods:  []string{"storage.managedFolders.getIamPolicy", "storage.managedFolders.setIamPolicy"},
		Provider: "storage", Offers: "Permissions tab on a managed folder's page, the folder page its row opens, typed Managed folder: principals and roles, Grant access, Remove principal, with the note that nothing is enforced; a plain folder has none (#847)",
		Tests: []string{"TestConsolePermissionsOnAManagedFolder", "TestStoragePermissionsOnAManagedFolder", "TestStorageManagedFolderPermissionsThroughTheTab"}},
	{Resource: "Cloud Run service IAM policy", Service: "run",
		Methods:  []string{"Services/GetIamPolicy", "Services/SetIamPolicy"},
		Provider: "run", Offers: "Permissions tab on a service's page: principals and roles, Grant access, Remove principal, with the note that nothing is enforced (#1216)",
		Tests: []string{"TestConsolePermissionsOnARunServiceAndJob"}},
	{Resource: "Cloud Run job IAM policy", Service: "run",
		Methods:  []string{"Jobs/GetIamPolicy", "Jobs/SetIamPolicy"},
		Provider: "run-jobs", Offers: "Permissions tab on a job's page: principals and roles, Grant access, Remove principal, with the note that nothing is enforced (#1216)",
		Tests: []string{"TestConsolePermissionsOnARunServiceAndJob"}},
	{Resource: "Cloud Run worker pool IAM policy", Service: "run",
		Methods:  []string{"WorkerPools/GetIamPolicy", "WorkerPools/SetIamPolicy", "WorkerPools/TestIamPermissions"},
		Provider: "run-worker-pools", Offers: "Permissions tab on a worker pool's page: principals and roles, Grant access, Remove principal, Test permissions, with the note that nothing is enforced (#1300)",
		Tests: []string{"TestConsoleRunWorkerPoolDeployEditScaleDelete"}},
	{Resource: "Cloud Run service permission check", Service: "run",
		Methods:  []string{"Services/TestIamPermissions"},
		Provider: "run", Offers: "Test permissions on the Permissions tab of a service's page: permissions typed in, each answered Held or Not held as the service's TestIamPermissions answers, the policy unchanged (#1242)",
		Tests: []string{"TestConsolePermissionsOnARunServiceAndJob", "TestPermissionsTest"}},
	{Resource: "Cloud Run job permission check", Service: "run",
		Methods:  []string{"Jobs/TestIamPermissions"},
		Provider: "run-jobs", Offers: "Test permissions on the Permissions tab of a job's page: permissions typed in, each answered Held or Not held as the service's TestIamPermissions answers, the policy unchanged (#1242)",
		Tests: []string{"TestConsolePermissionsOnARunServiceAndJob", "TestPermissionsTest"}},
	{Resource: "Cloud Tasks permission check", Service: "tasks",
		Methods:  []string{"CloudTasks/TestIamPermissions"},
		Provider: "tasks", Offers: "Test permissions on the Permissions tab of a queue's page: permissions typed in, each answered Held or Not held as the service's TestIamPermissions answers, the policy unchanged; every one of an existing resource is Held, since nothing is enforced (ADR-0006, #1242)",
		Tests: []string{"TestConsolePermissionsOnAQueue", "TestPermissionsTest"}},
	{Resource: "Secret Manager permission check", Service: "secretmanager",
		Methods:  []string{"SecretManagerService/TestIamPermissions"},
		Provider: "secrets", Offers: "Test permissions on the Permissions tab of a secret's page: permissions typed in, each answered Held or Not held as the service's TestIamPermissions answers, the policy unchanged; every one of an existing resource is Held, since nothing is enforced (ADR-0006, #1242)",
		Tests: []string{"TestConsolePermissionsOnASecret", "TestPermissionsTest"}},
	{Resource: "Cloud KMS permission check", Service: "kms",
		Methods:  []string{"IAMPolicy/TestIamPermissions"},
		Provider: "kms", Offers: "Test permissions on the Permissions tab of a key ring's page and a key's: permissions typed in, each answered Held or Not held as the service's TestIamPermissions answers, the policy unchanged; every one of an existing resource is Held, since nothing is enforced (ADR-0006, #1242)",
		Tests: []string{"TestConsolePermissionsOnAKeyRingAndKey", "TestPermissionsTest"}},
	{Resource: "Cloud Storage permission check", Service: "storage",
		Methods:  []string{"storage.buckets.testIamPermissions", "storage.managedFolders.testIamPermissions"},
		Provider: "storage", Offers: "Test permissions on the Permissions tab of a bucket's page and a managed folder's: permissions typed in, each answered Held or Not held as the service's TestIamPermissions answers, the policy unchanged; every one of an existing resource is Held, since nothing is enforced (ADR-0006, #1242)",
		Tests: []string{"TestConsolePermissionsOnABucket", "TestConsolePermissionsOnAManagedFolder", "TestTestPermissionsThroughTheTab", "TestPermissionsTest"}},

	// Firestore and Datastore
	{Resource: "Firestore documents (read)", Service: "firestore",
		Methods:  []string{"Firestore/BatchGetDocuments", "Firestore/RunQuery"},
		Provider: "firestore", Offers: "Collections, documents, per-field detail, query builder; every value inside a map or array field, each on its own page (#995)",
		Tests: []string{"TestFirestoreTypeDistinguishesWhatAFlatCellCannot", "TestQueryFormsCoverTheOperatorsTheyDocument", "TestFirestoreElementsAreListedAndAddressed"}},
	{Resource: "Firestore transactions", Service: "firestore",
		Methods: []string{"Firestore/BeginTransaction", "Firestore/Rollback"}, Excluded: transactionReason},
	{Resource: "Firestore count queries", Service: "firestore",
		Methods:  []string{"Firestore/RunAggregationQuery"},
		Provider: "firestore", Offers: "Count only in a collection's query builder: the filter's matching documents counted by Firestore with one COUNT aggregation query, shown as count(*) and its count, Limit and Order by not applied (#1198)",
		Tests: []string{"TestFirestoreCountOnlyRunsAnAggregationQuery", "TestConsoleFirestoreCountOnly", "TestFirestoreCountOnlyThroughTheBrowser"}},
	{Resource: "Firestore partitioned queries", Service: "firestore",
		Methods:  []string{"Firestore/PartitionQuery"},
		Excluded: "splits a collection-group query into cursors for parallel workers, another form of what the console already offers: the query builder runs a query whole with RunQuery (#1405)"},
	{Resource: "Firestore single-document calls", Service: "firestore",
		Methods:  []string{"Firestore/GetDocument", "Firestore/CreateDocument", "Firestore/UpdateDocument", "Firestore/DeleteDocument"},
		Excluded: "the one-document forms of reads and writes the console makes another way: the official client reads a document with BatchGetDocuments and creates, edits and deletes it with Commit, which every document page and action sends"},
	{Resource: "Firestore streaming and bulk writes", Service: "firestore",
		Methods:  []string{"Firestore/Write", "Firestore/Listen", "Firestore/BatchWrite"},
		Excluded: "a client's transport: Write is the streamed form of Commit, Listen the stream under real-time snapshot listeners, and BatchWrite the non-atomic bulk form a BulkWriter sends; a page reads what is stored when it opens, and writes one document at a time with Commit"},
	{Resource: "Firestore document writes", Service: "firestore",
		Methods:  []string{"Firestore/Commit"},
		Provider: "firestore", Offers: "Start collection, Add document (auto ID or named), Add field, Edit field and Delete field (typed: string, number, boolean, null, timestamp, geopoint, reference, bytes as base64, map, array), Delete document; Start collection on a document's page makes a subcollection with its first document (#854); a map or array field's Elements tab, with Edit value, Add value and Remove value on each value inside it, writing only that value (#995); Edit field refused, on the page and on the route, for a reference into another project or database (#1050)",
		Tests: []string{"TestConsoleFirestoreDocumentCreateEditDelete", "TestEditFormsRoundTripTheStoredType", "TestConsoleFirestoreSubcollections", "TestFirestoreStartASubcollectionOnADocumentPage",
			"TestConsoleFirestoreEditValuesInsideMapsAndArrays", "TestFirestoreEditValueSavedUnchangedWritesTheSameValue", "TestFirestoreAddAndRemoveValueChangeOnlyThatValue",
			"TestFirestoreValueWritesAreMaskedToWhatChanged", "TestFirestoreValueActionsRefuseAChangedField", "TestFirestoreBytesRoundTripAsBase64",
			"TestFirestoreEditAddRemoveAndBytesValuesThroughTheBrowser", "TestConsoleFirestoreEditFieldRefusesAForeignReference", "TestFirestoreEditFieldRefusesAForeignReference"}},
	{Resource: "Firestore databases", Service: "firestoreadmin",
		Methods:  []string{"FirestoreAdmin/ListDatabases", "FirestoreAdmin/GetDatabase", "FirestoreAdmin/CreateDatabase", "FirestoreAdmin/DeleteDatabase"},
		Provider: "firestore-databases", Offers: "Firestore databases screen (/firestore-databases): (default) and every created database with type, location, concurrency mode and delete protection; a database's page with its settings (edition, point-in-time recovery, version retention, App Engine integration, UID, creation time); Create database (ID, location, concurrency mode) and Delete database, which deletes its documents (#1331)",
		Tests: []string{"TestConsoleFirestoreDatabasesIndexesAndFields", "TestFirestoreAdminDatabases"}},
	{Resource: "Firestore database settings", Service: "firestoreadmin",
		Methods:  []string{"FirestoreAdmin/UpdateDatabase"},
		Provider: "firestore-databases", Offers: "Edit database on a database's page, prefilled from it: concurrency mode, point-in-time recovery, App Engine integration and delete protection, sent with the etag the form was drawn from, so a database changed since is refused (#1343)",
		Tests: []string{"TestConsoleFirestoreDatabaseEditsIndexesAndFieldOverrides", "TestFirestoreDatabaseEditAndIndexFormsThroughTheBrowser"}},
	{Resource: "Firestore indexes and field overrides (read)", Service: "firestoreadmin",
		Methods:  []string{"FirestoreAdmin/ListIndexes", "FirestoreAdmin/ListFields"},
		Provider: "firestore-databases", Offers: "A database's Indexes tab (every composite index in every collection group, with its fields, query scope and state) and Field overrides tab (each field with index settings or a TTL policy of its own), each saying that the emulator needs no index and deletes no expired document (#1331)",
		Tests: []string{"TestConsoleFirestoreDatabasesIndexesAndFields", "TestFirestoreAdminIndexes", "TestFirestoreAdminFields"}},
	{Resource: "Firestore backup schedules", Service: "firestoreadmin",
		Methods:  []string{"FirestoreAdmin/CreateBackupSchedule", "FirestoreAdmin/ListBackupSchedules", "FirestoreAdmin/UpdateBackupSchedule", "FirestoreAdmin/DeleteBackupSchedule"},
		Provider: "firestore-databases", Offers: "A database's Backup schedules tab (each schedule's recurrence, retention and creation time, saying the schedules take no backups); Create backup schedule on a database's page (daily or weekly on a day, retention in days), and Edit retention and Delete backup schedule on a schedule's row (#1461)",
		Tests: []string{"TestConsoleFirestoreBackupSchedules", "TestFirestoreAdminBackupSchedules"}},
	{Resource: "Firestore single backup schedule reads", Service: "firestoreadmin",
		Methods:  []string{"FirestoreAdmin/GetBackupSchedule"},
		Excluded: "the one-resource form of the Backup schedules tab, which lists every schedule with all that GetBackupSchedule returns"},
	{Resource: "Firestore single index and field reads", Service: "firestoreadmin",
		Methods:  []string{"FirestoreAdmin/GetIndex", "FirestoreAdmin/GetField"},
		Excluded: "the one-resource forms of the Indexes and Field overrides tabs, which list every index and override with all that GetIndex and GetField return"},
	{Resource: "Firestore index and field override writes", Service: "firestoreadmin",
		Methods:  []string{"FirestoreAdmin/CreateIndex", "FirestoreAdmin/DeleteIndex", "FirestoreAdmin/UpdateField"},
		Provider: "firestore-databases", Offers: "Create index on a database's page (collection ID, fields one per line with ascending, descending or array-contains, query scope) and Delete index on an index's row on the Indexes tab; Add field override on a database's page, and Edit field override and Delete field override on a field's row on the Field overrides tab: a TTL policy and single-field index exemptions (ascending, descending and array-contains at collection and collection group scope) (#1343)",
		Tests: []string{"TestConsoleFirestoreDatabaseEditsIndexesAndFieldOverrides", "TestFirestoreIndexFieldsParse", "TestFirestoreDatabaseEditAndIndexFormsThroughTheBrowser"}},
	{Resource: "Firestore subcollections", Service: "firestore",
		Methods:  []string{"Firestore/ListCollectionIds"},
		Provider: "firestore", Offers: "Collections tab on a document's page; a subcollection's page, addressed by its path, with its documents, their fields and the same writes as a top-level collection (#854)",
		Tests: []string{"TestConsoleFirestoreSubcollections", "TestFirestoreSubcollectionPagesNameEveryLevel", "TestFirestoreStartASubcollectionOnADocumentPage"}},
	{Resource: "Firestore document listing", Service: "firestore",
		Methods:  []string{"Firestore/ListDocuments"},
		Provider: "firestore", Offers: "A collection's documents, read with ListDocuments and show_missing and paged by its page token, so a document that does not exist but has subcollections is listed in italics (no fields — has subcollections) and its page's Collections tab opens them; Add field on it creates it (#875); the Documents counts on the Firestore screen and a Collections tab count the same list, so they include it (#882)",
		Tests: []string{"TestConsoleFirestoreListsMissingDocumentsWithSubcollections", "TestFirestoreMissingDocumentIsListedInItalics", "TestConsoleFirestoreCountsMissingDocuments", "TestFirestoreCollectionCountIncludesMissingDocuments"}},
	{Resource: "Datastore entities (read)", Service: "datastore",
		Methods:  []string{"Datastore/Lookup", "Datastore/RunQuery"},
		Provider: "datastore", Offers: "Kinds in every namespace (the Namespace column, a Namespaces page and each namespace's kinds), entities, child entities (an entity's Children tab, by Name/ID with a Kind column since #885), per-property detail, query builder (#854); an entity's page is addressed by its URL-safe encoded key, as on Google's console, so a root entity named like a key path opens (#875); a Parent column on a kind's entities and the query builder's results tells that root from the child its name names, and a segment is read as an encoded key only when it is the canonical encoding of a key of the page's project, namespace and kind (#882); a Name/ID column, as on Google's console, renders a name name=… and a numeric ID id=…, so a root named id=7 (name=id=7) and the numeric ID 7 (id=7) are told apart, and key paths use the same rendering, Customer/name=alice/Order/id=7 (#885); a kind's entities and the query builder's results are read with RunQuery on the v1 API, as the entity's page is read with Lookup, so a key value in another project or database, and an embedded entity's own key, are shown with where they are, Order/id=7 (project another-project), in the listing's Properties cell as on the entity's page (#904)",
		Tests: []string{"TestConsoleDatastoreListingAndQueryShowWhereAKeyIs", "TestDatastoreQueriesAreWrittenAsTheClientWritesThem", "TestDatastoreEmbeddedEntityKeyKeepsItsPartition", "TestDatastoreKeyRoundTripsWhatTheListingRendered", "TestQueryFormsCoverTheOperatorsTheyDocument", "TestDatastorePathsAddressNamespacesAndAncestors", "TestConsoleDatastoreNamespacesAndChildren", "TestDatastoreEntityAddressIsUnambiguous", "TestConsoleDatastoreOpensARootEntityNamedLikeAKeyPath", "TestDatastoreRootEntityNamedLikeAKeyPathOpens", "TestDatastoreEncodedKeyPrecedence", "TestDatastoreEntityRowsTellARootFromTheChild", "TestConsoleDatastoreRowsNameTheirParent", "TestDatastoreKindListShowsEachRowsParent", "TestDatastoreNameIDTellsANameFromAnID", "TestConsoleDatastoreNameIDTellsANameFromAnID", "TestDatastoreNameIDColumnTellsANameFromAnID"}},
	{Resource: "Datastore transactions", Service: "datastore",
		Methods: []string{"Datastore/BeginTransaction", "Datastore/Rollback"}, Excluded: transactionReason},
	{Resource: "Datastore aggregation queries", Service: "datastore",
		Methods:  []string{"Datastore/RunAggregationQuery"},
		Provider: "datastore", Offers: "Aggregate in a kind's query builder: COUNT, or SUM or AVG of a property, over the filter's matching entities, computed by one RunAggregationQuery and shown as one row, COUNT(*) or SUM(property) and its value, Limit not applied (#1405)",
		Tests: []string{"TestDatastoreAggregateRunsAnAggregationQuery", "TestConsoleDatastoreAggregate"}},
	{Resource: "Datastore ID allocation", Service: "datastore",
		Methods:  []string{"Datastore/AllocateIds", "Datastore/ReserveIds"},
		Excluded: "key allocation a client makes before a write: Create entity with an auto ID sends an incomplete key, which Commit completes, and a named or numeric key needs no reservation"},
	{Resource: "Datastore entity writes", Service: "datastore",
		Methods:  []string{"Datastore/Commit"},
		Provider: "datastore", Offers: "Create entity (key name, name=… or id=N as the Name/ID column shows them (#885), or auto ID; in a namespace from the Datastore screen or a namespace's page), Create child entity on an entity's page, Add property, Edit property and Delete property (typed, with Exclude from indexes), Delete entity (#854); a key value is shown and edited as Kind/name=… or Kind/id=…, ancestors first, with __namespace__/{namespace} first for a key in a namespace other than the default, so Edit property saved unchanged writes the same key back, a name like id=7 included (#887); Add, Edit and Delete property write the entity back through the v1 API, so a key value in another project or database, shown with where it is and offered no edit, and every value they do not touch are kept as they were (#893); a key in an array or embedded entity is shown as key(Kind/name=…) (#894); an array or embedded entity property's Elements tab lists every value inside it, each with its own page, and Edit value, typed as Edit property is, writes back that one value through the v1 API, every other value exactly as it was read; a key in another project or database is offered no Edit value (#905); an array or embedded entity inside a property is listed on the Elements tab too, Add value inserts into an array at an index or appends, and adds a property to an embedded entity or its key (__key__) when it has none, and Remove value, confirmed by typing the value's name back, removes a value, a property or an embedded entity's key, each written through the v1 API with every other value and index flag as it was read; a value excluded from indexes before an indexed one in the same array, which Datastore stores after them, is refused rather than moved (#911); a blob is shown and edited as base64, standard encoding as the v1 REST API's blobValue, with Edit property, Add property and Edit value, Edit value with an Exclude from indexes prefilled as the value's own, and a blob over 1,500 bytes is refused unless excluded, as the client refuses it (#912); Edit value, Add value, Remove value and Exclude from indexes carry a digest of the property as the page read it and are refused, in the same transaction, when another writer changed that property since, saying the property changed since the page was loaded and to reload it (#923); Exclude from indexes on an array or embedded entity property and on every array or embedded entity inside one sets or clears the flag on every value inside it in one write, keeping every value and its order, and indexing is refused while a string or blob inside is over 1,500 bytes (#924)",
		Tests: []string{"TestConsoleDatastoreEntityCreateEditDelete", "TestEditFormsRoundTripTheStoredType", "TestConsoleDatastoreNamespacesAndChildren", "TestDatastoreNamespaceAndChildEntityThroughTheBrowser", "TestConsoleDatastoreNameIDTellsANameFromAnID", "TestDatastoreNameIDColumnTellsANameFromAnID", "TestDatastoreKeyValuesRoundTripThroughEditProperty", "TestConsoleDatastoreKeyValuesRoundTripThroughEditProperty", "TestDatastoreKeyValueEditPropertyIsANoOp", "TestConsoleDatastoreKeepsAKeyValuesProjectAndDatabase", "TestDatastoreKeyValuesKeepTheirProjectAndDatabase", "TestDatastoreFormValuesAreWrittenAsTheClientWritesThem", "TestNestedDatastoreValuesRenderAsTheirType", "TestDatastoreNestedAndForeignKeysThroughTheBrowser", "TestConsoleDatastoreEditValueInsideArraysAndEntities", "TestDatastoreElementsAreListedAndAddressed", "TestDatastoreEditValueWritesOnlyThatValue", "TestDatastoreEditValueInsideAnArrayThroughTheBrowser", "TestConsoleDatastoreAddAndRemoveValues", "TestDatastoreAddAndRemoveValueChangeOnlyThatValue", "TestDatastoreValueActionsAreOfferedWhereTheyApply", "TestConsoleDatastoreBlobValuesAsBase64", "TestDatastoreBlobValuesRoundTripAsBase64", "TestDatastoreAddRemoveAndBlobValuesThroughTheBrowser", "TestConsoleDatastoreValueActionsRefuseAConcurrentChange", "TestDatastoreValueActionsRefuseAChangedProperty", "TestHiddenFieldsAreSentBackAsTheyCame", "TestConsoleDatastoreExcludeWholeArrayFromIndexes", "TestDatastoreExcludeFromIndexesSetsEveryValue", "TestDatastoreStaleValueActionIsRefusedAndWholeArrayExcludedThroughTheBrowser"}},

	// Bigtable
	{Resource: "Bigtable tables and rows (read)", Service: "bigtable",
		Methods:  []string{"BigtableTableAdmin/CreateTable", "BigtableTableAdmin/GetTable", "BigtableTableAdmin/ListTables", "BigtableTableAdmin/DeleteTable", "Bigtable/ReadRows"},
		Provider: "bigtable", Offers: "Tables, column families, rows, per-cell detail, row-range reader, Create table, Delete; in the default instance, cloudburrow, or in the one chosen in the screen's instance picker among those ListInstances lists, carried in the URL as ?instance= (#1360)",
		Tests: []string{"TestConsoleBigtableCreateAndDeleteTable", "TestConsoleBigtableInstancePicker", "TestBigtableInstancePickerThroughTheBrowser"}},
	{Resource: "Bigtable column families and row writes", Service: "bigtable",
		Methods:  []string{"BigtableTableAdmin/ModifyColumnFamilies", "Bigtable/MutateRow"},
		Provider: "bigtable", Offers: "Add column family, Edit GC policy (max versions, max age, either or both) and Delete column family; Write cell (row key, family, qualifier, value, optional timestamp), Delete cells in a column, Delete row",
		Tests: []string{"TestConsoleBigtableFamiliesAndRowWrites", "TestBigtableFamilyAndRowActionsThroughTheOfficialClients"}},
	{Resource: "Bigtable table upkeep", Service: "bigtable",
		Methods:  []string{"BigtableTableAdmin/DropRowRange", "BigtableTableAdmin/UpdateTable"},
		Provider: "bigtable", Offers: "On a table's page: Delete rows by prefix and Delete all rows, each confirmed by the table's name typed back, keeping the column families; Edit deletion protection, prefilled from the table, and Delete table refused with the emulator's message while it is on (#1198)",
		Tests: []string{"TestBigtableTableAndCellActionsThroughTheOfficialClients", "TestBigtableActionsAreOfferedWhereTheyApply", "TestConsoleBigtableTableAndCellActions", "TestBigtableDeleteRowsByPrefixAndAllRowsThroughTheBrowser", "TestBigtableDeletionProtectionRefusesDeleteTableThroughTheBrowser"}},
	{Resource: "Bigtable conditional and read-modify-write cells", Service: "bigtable",
		Methods:  []string{"Bigtable/CheckAndMutateRow", "Bigtable/ReadModifyWriteRow"},
		Provider: "bigtable", Offers: "Increment (a 64-bit integer cell, by a whole number, negative to decrease) and Append value on each cell of a row's page; Delete row and Delete cells applied in one conditional mutation, only when the row or the column holds a cell, and refused otherwise (#1198)",
		Tests: []string{"TestBigtableTableAndCellActionsThroughTheOfficialClients", "TestBigtableFamilyAndRowActionsThroughTheOfficialClients", "TestConsoleBigtableTableAndCellActions", "TestBigtableIncrementAndAppendValueThroughTheBrowser", "TestBigtableConditionalDeleteRowAndCellsThroughTheBrowser"}},
	{Resource: "Bigtable client transport", Service: "bigtable",
		Methods:  []string{"Bigtable/MutateRows", "Bigtable/SampleRowKeys", "Bigtable/PingAndWarm"},
		Excluded: "a client's transport: MutateRows is the batched form of MutateRow an ApplyBulk sends, SampleRowKeys the split points a client shards a scan by, and PingAndWarm the warm-up a client sends on a new connection; the console writes one row at a time with MutateRow and reads with ReadRows"},
	{Resource: "Bigtable replication consistency", Service: "bigtable",
		Methods:  []string{"BigtableTableAdmin/GenerateConsistencyToken", "BigtableTableAdmin/CheckConsistency"},
		Excluded: "replication plumbing: a token a client waits on until its writes reach every cluster of an instance; the emulator has one cluster, so a write is consistent once it returns, and there is nothing a page could wait for"},

	{Resource: "Bigtable instances (list)", Service: "bigtable",
		Methods:  []string{"BigtableInstanceAdmin/ListInstances", "BigtableInstanceAdmin/ListClusters"},
		Provider: "bigtable-instances", Offers: "Bigtable instances screen (/bigtable-instances): every instance of the project with its display name, type, state, cluster count and labels; the Bigtable screen's note lists them too (#1331, #1358), and its instance picker offers them beside cloudburrow (#1360)",
		Tests: []string{"TestConsoleBigtableInstancesScreen", "TestConsoleBigtableListsInstances", "TestConsoleBigtableInstanceAdmin", "TestConsoleBigtableInstancePicker"}},
	{Resource: "Bigtable instances", Service: "bigtable",
		Methods:  []string{"BigtableInstanceAdmin/CreateInstance", "BigtableInstanceAdmin/GetInstance", "BigtableInstanceAdmin/UpdateInstance", "BigtableInstanceAdmin/DeleteInstance"},
		Provider: "bigtable-instances", Offers: "Create instance (ID, display name, type, labels, and a first cluster: ID, zone, storage type, nodes or autoscaling); an instance's page with its display name, type, state and labels, and a Tables tab linking each of its tables to the Bigtable screen with the instance chosen (#1360); Edit instance (display name, type, labels; development can become production); Delete instance, confirmed by its ID, which deletes its clusters, app profiles and tables (#1358)",
		Tests: []string{"TestConsoleBigtableInstancesScreen", "TestConsoleBigtableInstanceAdmin", "TestBigtableInstanceAdminThroughTheBrowser", "TestBigtableInstancePickerThroughTheBrowser"}},
	{Resource: "Bigtable instance partial updates", Service: "bigtable",
		Methods:  []string{"BigtableInstanceAdmin/PartialUpdateInstance"},
		Excluded: "another form of UpdateInstance, which Edit instance sends: the form shows every field PartialUpdateInstance could change (display name, type, labels), so it replaces them all"},
	{Resource: "Bigtable clusters", Service: "bigtable",
		Methods:  []string{"BigtableInstanceAdmin/CreateCluster", "BigtableInstanceAdmin/GetCluster", "BigtableInstanceAdmin/UpdateCluster", "BigtableInstanceAdmin/PartialUpdateCluster", "BigtableInstanceAdmin/DeleteCluster"},
		Provider: "bigtable-instances", Offers: "An instance's Clusters tab (zone, nodes or autoscaling limits and CPU target, storage type, state); Add cluster on its page; Edit nodes on a cluster's row (a fixed size with UpdateCluster, or autoscaling on, changed or off with PartialUpdateCluster); Delete cluster, confirmed by its ID, refused for the last cluster or one an app profile routes to (#1358)",
		Tests: []string{"TestConsoleBigtableInstancesScreen", "TestConsoleBigtableInstanceAdmin", "TestBigtableInstanceAdminThroughTheBrowser"}},
	{Resource: "Bigtable app profiles", Service: "bigtable",
		Methods:  []string{"BigtableInstanceAdmin/CreateAppProfile", "BigtableInstanceAdmin/GetAppProfile", "BigtableInstanceAdmin/ListAppProfiles", "BigtableInstanceAdmin/UpdateAppProfile", "BigtableInstanceAdmin/DeleteAppProfile"},
		Provider: "bigtable-instances", Offers: "An instance's App profiles tab (description, routing, single-row transactions); Create app profile on its page and Edit app profile on a profile's row (description, multi-cluster or single-cluster routing to a chosen cluster, single-row transactions); Delete app profile, confirmed by its ID (#1358)",
		Tests: []string{"TestConsoleBigtableInstancesScreen", "TestConsoleBigtableInstanceAdmin"}},
	{Resource: "Bigtable instance IAM", Service: "bigtable",
		Methods:  []string{"BigtableInstanceAdmin/GetIamPolicy", "BigtableInstanceAdmin/SetIamPolicy", "BigtableInstanceAdmin/TestIamPermissions"},
		Provider: "bigtable-instances", Offers: "Permissions tab on an instance's page: Grant access, Remove principal and Test permissions through the client's instance IAM handle, stored and not enforced (ADR-0006) (#1358)",
		Tests: []string{"TestConsoleBigtableInstancesScreen", "TestConsoleBigtableInstanceAdmin"}},

	// Spanner
	{Resource: "Spanner instances, databases and reads", Service: "spanner",
		Methods: []string{"InstanceAdmin/CreateInstance", "InstanceAdmin/GetInstance", "DatabaseAdmin/CreateDatabase", "DatabaseAdmin/GetDatabase", "DatabaseAdmin/GetDatabaseDdl",
			"InstanceAdmin/ListInstances", "InstanceAdmin/DeleteInstance", "DatabaseAdmin/ListDatabases", "DatabaseAdmin/DropDatabase",
			"Spanner/ExecuteStreamingSql", "Spanner/StreamingRead"},
		Provider: "spanner", Offers: "Instances, databases, tables, columns and indexes, DDL, read-only query editor, Create database, Drop database, Delete instance",
		Tests: []string{"TestConsoleSpannerCreateAndDropDatabase"}},
	{Resource: "Spanner database drop protection", Service: "spanner",
		Methods:  []string{"DatabaseAdmin/UpdateDatabase"},
		Provider: "spanner", Offers: "Drop protection on a database's page, prefilled from it: UpdateDatabase's enable_drop_protection, which the call front in the Spanner pod applies and enforces on Drop database and Delete instance (#1461)",
		Tests: []string{"TestConsoleSpannerEditInstance", "TestSpannerDatabaseDropProtection"}},
	{Resource: "Spanner instance updates", Service: "spanner",
		Methods:  []string{"InstanceAdmin/UpdateInstance"},
		Provider: "spanner", Offers: "Edit instance on an instance's page, prefilled from it: display name and processing units, which the call front in the Spanner pod applies (the emulator refuses UpdateInstance) (#1461)",
		Tests: []string{"TestConsoleSpannerEditInstance", "TestSpannerUpdateInstance"}},
	{Resource: "Spanner sessions and transactions", Service: "spanner",
		Methods: []string{"Spanner/CreateSession", "Spanner/BeginTransaction", "Spanner/BatchCreateSessions", "Spanner/GetSession",
			"Spanner/ListSessions", "Spanner/DeleteSession", "Spanner/Rollback"}, Excluded: transactionReason},
	{Resource: "Spanner reads and writes the client library offers besides the console's", Service: "spanner",
		Methods: []string{"Spanner/Read", "Spanner/PartitionQuery", "Spanner/PartitionRead", "Spanner/BatchWrite", "Spanner/ExecuteBatchDml"},
		Excluded: "other forms of what the console already offers: Read is StreamingRead answered at once, PartitionQuery and PartitionRead " +
			"split a query or read for parallel workers, and BatchWrite and ExecuteBatchDml apply several writes in one call; the console " +
			"reads with ExecuteStreamingSql and StreamingRead and writes one confirmed DML statement at a time (#798)"},
	{Resource: "Spanner schema changes (DDL)", Service: "spanner",
		Methods:  []string{"DatabaseAdmin/UpdateDatabaseDdl"},
		Provider: "spanner", Offers: "Update schema on a database's page: CREATE, ALTER and DROP statements separated by semicolons, applied in order, confirmed with the database named; DML and queries refused with where they run (#1207)",
		Tests: []string{"TestConsoleSpannerSchemaConfigurationsAndPartitions", "TestSpannerDDLSplitter", "TestSpannerAdminActionsAreOfferedWhereTheyBelong",
			"TestSpannerSchemaAndPartitionsThroughThePage"}},
	{Resource: "Spanner instance configurations", Service: "spanner",
		Methods:  []string{"InstanceAdmin/ListInstanceConfigs", "InstanceAdmin/GetInstanceConfig"},
		Provider: "spanner", Offers: "An instance's Configuration tab shows its configuration (type, replicas, leader options, state), and its Instance configurations tab lists the project's, the one in use marked (#1207)",
		Tests: []string{"TestConsoleSpannerSchemaConfigurationsAndPartitions"}},
	{Resource: "Spanner instance partitions", Service: "spanner",
		Methods: []string{"InstanceAdmin/CreateInstancePartition", "InstanceAdmin/GetInstancePartition", "InstanceAdmin/ListInstancePartitions",
			"InstanceAdmin/DeleteInstancePartition", "InstanceAdmin/ListInstancePartitionOperations"},
		Provider: "spanner", Offers: "An instance's Instance partitions tab, whose rows open a partition's page and delete it; Create instance partition (ID, display name, configuration, nodes) on the instance's page; a Partition operations tab listing their long-running operations (#1207)",
		Tests: []string{"TestConsoleSpannerSchemaConfigurationsAndPartitions", "TestSpannerAdminActionsAreOfferedWhereTheyBelong",
			"TestSpannerSchemaAndPartitionsThroughThePage"}},
	{Resource: "Spanner DML", Service: "spanner",
		Methods:  []string{"Spanner/Commit", "Spanner/ExecuteSql"},
		Provider: "spanner", Offers: "Read-write mode in the query editor: one INSERT, UPDATE or DELETE, confirmed with the database named, committed in a read-write transaction, rows affected reported (#798)",
		Tests: []string{"TestConsoleSpannerDML", "TestSpannerStatementClassifier", "TestQueryModeChoosesTheCallNotTheText"}},

	// BigQuery (#993): its REST methods, from the coverage page the official
	// generated client's methods make (docs/coverage/bigquery.md).
	{Resource: "BigQuery datasets", Service: "bigquery",
		Methods:  []string{"bigquery.datasets.insert", "bigquery.datasets.get", "bigquery.datasets.list", "bigquery.datasets.delete"},
		Provider: "bigquery", Offers: "Datasets list with each one's table count, a dataset's page (tables, details), Create dataset (location, description, labels) and Delete dataset with its tables (#698, #854); the one project the emulator serves, and a prompt naming it for any other",
		Tests: []string{"TestConsoleBigQueryDatasetsSchemaAndQuery", "TestConsoleBigQueryDatasetTableAndRowWrites", "TestBigQueryWritesStayInTheServedProject"}},
	{Resource: "BigQuery dataset edit", Service: "bigquery",
		Methods:  []string{"bigquery.datasets.patch"},
		Provider: "bigquery", Offers: "Edit dataset on a dataset's page: friendly name, description, labels, default table and partition expiration, default collation, time travel window and access, sending only what changed with the dataset's etag, so a change made elsewhere meanwhile is refused (#1205)",
		Tests: []string{"TestBigQueryEditDataset", "TestBigQueryEditDatasetThroughTheForm"}},
	{Resource: "BigQuery dataset replace", Service: "bigquery",
		Methods:  []string{"bigquery.datasets.update"},
		Excluded: "the replacing form of the edit the console makes with datasets.patch, which changes the fields Edit dataset holds without clearing the ones it does not show"},
	{Resource: "BigQuery service account", Service: "bigquery",
		Methods:  []string{"bigquery.projects.getServiceAccount"},
		Provider: "bigquery", Offers: "The BigQuery page's Project summary: the project ID and the BigQuery service account projects.getServiceAccount names, the account a customer-managed encryption key is granted to (#1270)",
		Tests: []string{"TestBigQueryListNamesTheServiceAccount", "TestConsoleBigQueryNamesTheServiceAccount"}},
	{Resource: "BigQuery tables and rows", Service: "bigquery",
		Methods:  []string{"bigquery.tables.insert", "bigquery.tables.get", "bigquery.tables.list", "bigquery.tables.patch", "bigquery.tables.delete", "bigquery.tabledata.insertAll", "bigquery.tabledata.list"},
		Provider: "bigquery", Offers: "Tables on a dataset's page; a table's page with its schema, details (its time-unit or integer-range partitioning and clustering, as CREATE TABLE ... PARTITION BY ... CLUSTER BY makes them, #1272) and Preview (tabledata.list); a column-partitioned table's Partitions, each partition's ID and rows, opening the partition's page by its decorated name (`t$20240101`, tables.get), whose Preview reads that partition's rows only (#1273); Create table with the schema editor, RECORD fields to 15 levels and flexible field names included, or a view with its query; Insert rows (tabledata.insertAll) with Skip invalid rows and Ignore unknown values; Edit table (tables.patch: the description, friendly name and expiration, a label set, changed or removed, #1025; fields added after the table's own, as BigQuery adds them, a schema update with a new column before them refused 412, #1111, column descriptions and REQUIRED columns relaxed, what ALTER TABLE changes, #1227); Delete table (#698, #854, #874, #994); a materialized view's page with its definition (query, automatic refresh, last refreshed) and Refresh, which runs CALL BQ.REFRESH_MATERIALIZED_VIEW, and Create table of a MATERIALIZED_VIEW (#1390)",
		Tests: []string{"TestConsoleBigQueryDatasetsSchemaAndQuery", "TestConsoleBigQueryDatasetTableAndRowWrites", "TestConsoleBigQueryEditTableViewsAndInsertOptions",
			"TestBigQueryCreateViewAndEditTable", "TestBigQueryInsertRowsOptions", "TestBigQueryFieldNamesAreFlexible",
			"TestBigQueryCreateTableInsertRowsAndDeleteThroughTheForms", "TestBigQueryRecordColumnsThroughTheSchemaEditor",
			"TestBigQueryPartitionByAndClusterByReadBack", "TestConsoleBigQueryPartitions", "TestBigQueryPartitionDestinations",
			"TestBigQueryFlexibleNamesInsertOptionsEditTableAndViewThroughTheForms", "TestConsoleBigQueryMaterializedViews"}},
	{Resource: "BigQuery table update (tables.update)", Service: "bigquery",
		Methods:  []string{"bigquery.tables.update"},
		Provider: "bigquery", Offers: "Edit table on a table's page, which sends tables.patch, not tables.update: the front carries out both on one path (#1054), so the description and labels Edit table changes are what a tables.update of them changes; Edit table adds fields, sets column descriptions and relaxes REQUIRED columns too (#1227) (#1048)",
		Tests: []string{"TestConsoleBigQueryEditTableViewsAndInsertOptions", "TestBigQueryFlexibleNamesInsertOptionsEditTableAndViewThroughTheForms"}},
	{Resource: "BigQuery queries", Service: "bigquery",
		Methods:  []string{"bigquery.jobs.query", "bigquery.jobs.getQueryResults"},
		Provider: "bigquery", Offers: "The query editor on every dataset and table page, run with jobs.query: read-only by default, one SELECT or WITH … SELECT, at most 200 rows (#698); an opt-in, confirmed Read-write mode for DDL (a view's column list too, also in a script and with a column's description, #1240, #1323) and scripts with DECLARE (#994), and a DML statement of its own (INSERT, UPDATE, DELETE, MERGE, TRUNCATE TABLE) answered with the rows it changed (#1024); DML inside a script (#1028) and UPDATE … FROM (#1027) refused with the reason; in the editor a SELECT reads one partition by its decorator (`t$20240101`), the pseudo-columns _PARTITIONTIME and _PARTITIONDATE where BigQuery has them, and FOR SYSTEM_TIME AS OF, as the front answers them (#1273, #1276, #1277); the project and organization configurations, which have no settings page: ALTER PROJECT and ALTER ORGANIZATION SET OPTIONS, alone or in a script, in the Read-write mode, and `region-REGION`.INFORMATION_SCHEMA.PROJECT_OPTIONS, ORGANIZATION_OPTIONS and EFFECTIVE_PROJECT_OPTIONS read in the read-only editor (#1417); a dataset's and a region's INFORMATION_SCHEMA views read there as the front answers them, COLUMNS with async_generation_status and region TABLE_STORAGE with total_partitions, a dataset-qualified OBJECT_PRIVILEGES or SCHEMATA_LINKS and a query naming the engine's _PARTITION_BUFFERED or _PARTITION_STREAMED refused with the reason (#1458)",
		Tests: []string{"TestConsoleBigQueryDatasetsSchemaAndQuery", "TestTheBigQueryEditorRunsOnlyOneSelect", "TestTheBigQueryEditorsCarryProjectConfigurations",
			"TestConsoleBigQueryReadWriteEditorRunsDDLAndScripts", "TestConsoleBigQueryReadWriteEditorRunsDML",
			"TestTheBigQueryReadWriteEditorScreensStatements", "TestBigQueryDMLAnswerSaysWhatChanged",
			"TestBigQueryWriteReportSendsJobsQuery", "TestBigQueryReadWriteEditorThroughTheForms",
			"TestBigQueryPartitionPseudoColumns", "TestBigQueryPartitionDecorators", "TestBigQueryTimeTravelReads"}},
	{Resource: "BigQuery load and export jobs", Service: "bigquery",
		Methods:  []string{"bigquery.jobs.insert"},
		Provider: "bigquery", Offers: "Load from Cloud Storage on a dataset's and a table's page (gs:// URIs; CSV, JSON or Parquet; write preference; schema or auto-detect, for CSV and for JSON with nested and repeated fields (#1324); the CSV options the front serves, timeZone, the date and time formats and the UTF-16 and UTF-32 encodings among them, #1325) and Export to Cloud Storage on a table's page (a URI and More destination URIs; CSV, JSON, Avro or Parquet; GZIP, DEFLATE, SNAPPY or ZSTD as each format allows; Avro logical types; delimiter; header), each answered with its job, linked to Job history, and refused in the API's words (#993); Copy table on a table's page (a copy job to a destination dataset and table, with a write preference, and More source tables for a multi-source copy, answered with its job, #782; its Operation COPY, CLONE, SNAPSHOT or RESTORE, the job's operationType, #1002); Snapshot (with an expiration in hours) and Clone on a table's page, and Restore on a table snapshot's page, which offers Edit table, Export, Copy table, Restore and Delete table, not Insert rows or Load, as Google's console does (#1002); Load from a file on the same pages, the same options with a file chosen in the browser, streamed to the front as a multipart or resumable upload under the console's upload limit (#999)",
		Tests: []string{"TestConsoleBigQueryLoadExportAndJobHistory", "TestBigQueryLoadFormBecomesTheLoadsSource", "TestBigQueryCopyTableFormRunsACopyJob", "TestBigQueryCopyTableSendsEveryMoreSource", "TestBigQueryCopyTableSendsTheOperation", "TestConsoleBigQueryCopyTablesAndAvroExport", "TestConsoleBigQueryCloneAndSnapshotTables", "TestConsoleBigQuerySnapshotCloneAndRestoreActions", "TestBigQueryCopyTableAndAvroExportThroughTheForms", "TestBigQueryLoadExportAndJobHistoryThroughTheForms", "TestConsoleBigQueryParquetAndSeveralURIExports", "TestBigQueryExportFormOffersAvroAndParquet",
			"TestConsoleBigQueryLoadFromAFile", "TestBigQueryLoadFromAFileSendsTheFileAsTheLoadsData", "TestActionUploadStreamsTheFileToAnOfferedAction", "TestBigQueryLoadFromAFileThroughTheForm"}},
	{Resource: "BigQuery job history", Service: "bigquery",
		Methods:  []string{"bigquery.jobs.get", "bigquery.jobs.list", "bigquery.jobs.cancel", "bigquery.jobs.delete"},
		Provider: "bigquery-jobs", Offers: "Job history page (/bigquery-jobs): the project's jobs from jobs.list, newest first, with type, state, creation time and error; a job's page (jobs.get) with its state, times, errorResult, configuration, load statistics (output rows, bad records, input files and bytes, each only when reported) and status.errors; a query job's statement type and DML rows (affected, inserted, updated, deleted, each only when reported, #1041); Delete job on the page and the row; Cancel job only on a job that is not DONE (#993)",
		Tests: []string{"TestConsoleBigQueryLoadExportAndJobHistory", "TestBigQueryJobPageShowsWhatTheJobReported", "TestBigQueryJobActionsFollowTheJobsState", "TestBigQueryLoadExportAndJobHistoryThroughTheForms",
			"TestConsoleBigQueryJobPageShowsDMLRows", "TestBigQueryJobPageShowsAQueryJobsDMLRows"}},

	{Resource: "BigQuery routines (functions, table functions and stored procedures)", Service: "bigquery",
		Methods:  []string{"bigquery.routines.insert", "bigquery.routines.list", "bigquery.routines.get", "bigquery.routines.delete"},
		Provider: "bigquery", Offers: "Routines on a dataset's page (routines.list: each routine's type, language and arguments, a procedure's with their OUT and INOUT modes), a routine's page at [dataset, routines, routine] (routines.get: body, arguments with their modes, return type or table, details) and Delete routine on the page and the row (routines.delete, #782, #1177, #1217); routines are made in the query editor's Read-write mode (CREATE FUNCTION, CREATE TABLE FUNCTION, CREATE PROCEDURE)",
		Tests: []string{"TestConsoleBigQueryRoutines", "TestBigQueryRoutinesSectionAndPage", "TestBigQueryRoutinesThroughThePage"}},
	{Resource: "BigQuery row access policies", Service: "bigquery",
		Methods:  []string{"bigquery.rowAccessPolicies.list", "bigquery.rowAccessPolicies.get", "bigquery.rowAccessPolicies.insert", "bigquery.rowAccessPolicies.update", "bigquery.rowAccessPolicies.delete", "bigquery.rowAccessPolicies.batchDelete"},
		Provider: "bigquery", Offers: "Row access policies tab on a table's page (rowAccessPolicies.list: each policy's filter and grantees, and whom CloudBurrow takes the caller to be); Create row access policy (rowAccessPolicies.insert, or rowAccessPolicies.update with Replace an existing policy), Delete policy on each row (rowAccessPolicies.delete) and Delete all row access policies (rowAccessPolicies.batchDelete); rowAccessPolicies.get reads one back (#1218)",
		Tests: []string{"TestConsoleBigQueryRowAccessPolicies", "TestBigQueryRowAccessPoliciesTab"}},
	{Resource: "BigQuery table and routine IAM", Service: "bigquery",
		Methods:  []string{"bigquery.tables.getIamPolicy", "bigquery.tables.setIamPolicy", "bigquery.routines.getIamPolicy", "bigquery.routines.setIamPolicy"},
		Provider: "bigquery", Offers: "Permissions tab on a table's, a view's and a routine's page: principals and roles, Grant access, Remove principal, with the note that nothing is enforced (#793, #1231)",
		Tests: []string{"TestConsoleBigQueryRowAccessPolicies", "TestBigQueryRowAccessPoliciesTab"}},
	{Resource: "BigQuery row access policy IAM", Service: "bigquery",
		Methods: []string{"bigquery.rowAccessPolicies.getIamPolicy"},
		Excluded: "a row access policy's IAM policy is its grantees as roles/bigquery.filteredDataViewer, which the Row access policies tab lists in each policy's Grantees column and Create row access policy with Replace changes; " +
			"the API has no setIamPolicy to write it, and the Permissions tab's note that a binding grants nothing would be false, since the grantees decide which rows a query returns (#1231)"},
	{Resource: "BigQuery IAM permission checks", Service: "bigquery",
		Methods:  []string{"bigquery.tables.testIamPermissions", "bigquery.routines.testIamPermissions"},
		Provider: "bigquery", Offers: "Test permissions on the Permissions tab of a table's, a view's and a routine's page: permissions typed in, each answered Held or Not held as the service's TestIamPermissions answers, the policy unchanged (#1242)",
		Tests: []string{"TestConsoleBigQueryRowAccessPolicies", "TestPermissionsTest"}},
	{Resource: "BigQuery row access policy permission check", Service: "bigquery",
		Methods:  []string{"bigquery.rowAccessPolicies.testIamPermissions"},
		Excluded: "a row access policy has no Permissions tab (its IAM policy is its grantees, #1231), so there is no tab to ask it from"},
	{Resource: "BigQuery routine edit", Service: "bigquery",
		Methods:  []string{"bigquery.routines.update"},
		Provider: "bigquery", Offers: "Edit routine on a routine's page: its body and description, with its type, language, arguments and return type kept (routines.update, #1238)",
		Tests: []string{"TestBigQueryEditRoutine", "TestBigQueryRoutineUpdate"}},
	// The Storage Read and Write APIs (gRPC, google.cloud.bigquery.storage.v1)
	// have no coverage page; this row writes the exclusion down (#782).
	{Resource: "BigQuery models", Service: "bigquery",
		Methods: []string{"bigquery.models.list", "bigquery.models.get", "bigquery.models.patch", "bigquery.models.delete"},
		Excluded: "no model can exist: the emulator does not serve BigQuery ML's CREATE MODEL, so a dataset's models are always none " +
			"and there is nothing to show, edit or delete (#1200)"},
	{Resource: "BigQuery projects", Service: "bigquery",
		Methods:  []string{"bigquery.projects.list"},
		Excluded: "lists the one project the instance serves; the console's project picker already names it, from Resource Manager"},
	{Resource: "BigQuery Storage Read and Write APIs", Service: "bigquery",
		Excluded: "a client's read and write transport (Arrow and Avro read sessions, managedwriter default, COMMITTED, PENDING and BUFFERED streams, Verified in docs/compatibility.md); a table's Preview reads its rows and Insert rows and the load forms write them, the console's equivalents"},

	// The CLI and the admin API.
	{Resource: "Instance status",
		Commands: []string{"status"},
		Screen:   "/", Offers: "Dashboard: instance, cluster, endpoints, services, components",
		Tests: []string{"TestStatusReportsLiveInstanceState"}},
	{Resource: "Logs",
		Commands: []string{"logs"},
		Screen:   "/logs", Offers: "Logs Explorer: emulator, component and Cloud Run logs, live, filtered by source",
		Tests: []string{"TestFiltersNarrowTheView", "TestStreamSendsBacklogThenLiveEntries"}},
	{Resource: "Admin events",
		Commands: []string{"events"}, Routes: []string{"GET /admin/events"},
		Screen: "/requests", Offers: "Request Log: the recorder's request events, filtered by service, code and project, live, Pub/Sub's and Bigtable's calls through their fronts (#1266, #1267) and Spanner's, Firestore's and Datastore's through the call fronts in their pods, a pod's calls as well as the host's (#1288), among them, and on /monitoring their request rate, error and latency charts beside each emulator pod's container CPU and memory (#1267, #1288); its fault events are Recent faults on the Fault injection page (#800)",
		Tests: []string{"TestTheRequestLogShowsServedCalls", "TestPubSubCallsReachMetricsAndTheRequestLog",
			"TestFirestoreCallsReachMetricsAndTheRequestLog", "TestBigtableCallsReachMetricsAndTheRequestLog",
			"TestFirestoreOnMonitoringChartsItsCallsAndContainer", "TestSpannerCallsReachMetricsAndTheRequestLog",
			"TestAPodsFirestoreCallsReachMetricsAndTheRequestLog", "TestSpannerOnMonitoringChartsItsCalls"}},
	{Resource: "Fault injection",
		Routes: []string{"POST /admin/faults", "GET /admin/faults", "DELETE /admin/faults"},
		Screen: "/faults", Offers: "Fault injection: the rules held, Add rule, Delete per rule, Clear all, the refused services with the reason, Recent faults, with the admin token pasted once per tab (#800)",
		Tests: []string{"TestConsoleFaultsActThroughTheAdminAPI", "TestConsoleFaultRuleFailsTheSDKCall", "TestFaultRuleAddedAndDeletedThroughTheForm"}},
	{Resource: "State save and load, reset, seed",
		Commands: []string{"state", "reset", "seed"},
		Routes:   []string{"POST /admin/state/export", "POST /admin/state/import", "POST /admin/reset", "POST /admin/seed", "GET /admin/instance"},
		Screen:   "/instance", Offers: "Instance: Save state (download), Load state (upload, confirmed by the instance's name), " +
			"Reset (every service or those chosen, optionally one project, with Reseed, confirmed by the scope), Seed (upload, If not exists)",
		Tests: []string{"TestConsoleInstanceActsThroughTheAdminAPI", "TestConsoleStateSaveResetLoadRestores", "TestInstanceResetConfirmedByTypingTheScope"}},
	{Resource: "Connect, About and diagnose",
		Commands: []string{"env", "gcloud-setup", "terraform", "version", "diagnose"},
		Screen:   "/connect", Offers: "Connect: what `env` exports in each of its formats with Copy, client snippets, the gcloud-setup, gcloud-teardown and terraform commands to copy; About, as `version` prints it; Download bundle, the `diagnose` bundle (#802)",
		Tests: []string{"TestConsoleConnectIsTheInstancesEnv", "TestConnectVariablesAreEnvJSON", "TestAboutIsTheBuildsVersion", "TestConsoleDiagnoseBundleHoldsNoCredentials", "TestConnectPageShowsTheEnvironmentAndDownloadsTheBundle"}},
	{Resource: "Local AI models",
		Routes: []string{"GET /admin/local-models", "POST /admin/local-models/{id}/{action}"},
		Screen: "/ai/local-models", Offers: "Local models: each catalogued model's state (not pinned, not downloaded, " +
			"downloading with progress, downloaded, loaded), Download (a pinned, ungated model, its exact size confirmed, " +
			"each file checked against its pinned SHA-256), Cancel, Load, Unload and Delete (#1306), through the same " +
			"manager the admin routes drive; the Studio lists each model's state and links to it and to Embeddings (#1396)",
		Tests: []string{"TestAdminLocalModelsNeedTheTokenAndRunEachAction", "TestLocalModelsPageRoutesEachActionAndNeedsTheConfirmedSize",
			"TestStudioReachesModelManagementAndTheEmbeddingView", "TestConsoleLocalModelsLoadEmbedUnload",
			"TestConsoleLocalModelsUnloadDeleteDownloadLoad", "TestStudioManagesLocalModelsAndTestsEmbeddings"}},
	{Resource: "Vertex AI custom prediction (serving contract)",
		Screen: "/ai/predict", Offers: "Online prediction: the Cloud Run services configured with the contract's AIP_* variables, " +
			"JSON instances and parameters, Predict, the container's status and body verbatim, and Explain with " +
			"its Sampled Shapley spec (#1454); Vertex's Endpoint, the explanation methods that need gradients or an " +
			"Example index, and batch prediction named as not served (#869)",
		Tests: []string{"TestConsoleOnlinePredictionThroughTheConsoleAPI", "TestCloudRunPredictorAnswersOnlinePredictionInTheBrowser",
			"TestPredictRelaysThroughTheIngressAndShowsTheAnswerVerbatim", "TestPredictRefusesWhatItCannotSend"}},
	// PredictionService over gRPC (#1193) is a protocol for the requests
	// these screens already send, through the same handlers.
	{Resource: "Vertex AI PredictionService: Predict and RawPredict", Service: "vertexai",
		Methods: []string{"PredictionService/Predict", "PredictionService/RawPredict"},
		Screen:  "/ai/predict", Offers: "Online prediction sends a deployed container the request Predict relays " +
			"(instances and parameters, the container's answer verbatim); Embeddings sends the text-embedding :predict " +
			"a Predict on the publisher model runs, and Method RawPredict the same body unwrapped as the :rawPredict " +
			"a RawPredict on the publisher model runs (#1284)",
		Tests: []string{"TestConsoleOnlinePredictionThroughTheConsoleAPI", "TestPredictionClientReachesACustomEndpoint",
			"TestGRPCPredictEmbedsLikeTheJSONRoute", "TestEmbeddingsScreenSendsBothMethods", "TestRawPredictOnTheEmbeddingModel",
			"TestGRPCRawPredictOnTheEmbeddingModel", "TestRawPredictREST"}},
	{Resource: "Vertex AI PredictionService: Explain", Service: "vertexai",
		Methods: []string{"PredictionService/Explain"},
		Screen:  "/ai/predict", Offers: "Online prediction: Explain sends the instances and parameters with " +
			"explanation_spec_override (Sampled Shapley path count, each feature's input_baselines) to the endpoint's " +
			":explain, and shows the attributions per feature, the baseline and instance outputs, and the API's " +
			"body or refusal verbatim; integrated gradients, XRAI and example-based explanations are named as not " +
			"served (#1454)",
		Tests: []string{"TestConsoleExplainMatchesThePredictionClient", "TestPredictionClientReachesACustomEndpoint",
			"TestExplainSendsTheSpecOverrideAndShowsAttributionsPerFeature", "TestCloudRunPredictorAnswersOnlinePredictionInTheBrowser"}},
	{Resource: "Vertex AI PredictionService: EmbedContent", Service: "vertexai",
		Methods: []string{"PredictionService/EmbedContent"},
		Screen:  "/ai/embeddings", Offers: "Embeddings: Method EmbedContent sends each text as the :embedContent " +
			"an EmbedContent on the publisher model runs, with its task type and output dimensionality, and shows " +
			"the API's answer or refusal verbatim (#1244); the playground names the model (any ID is sent as typed, " +
			"and the API's 404 for text-embedding-005 or textembedding-gecko shown), and shows each whole vector " +
			"with its dimension and norm, a copy of it, the pairwise cosine similarity and the request sent (#1305), " +
			"and the token count and truncated flag as the API returns them (#1396)",
		Tests: []string{"TestEmbeddingsScreenSendsBothMethods", "TestEmbeddingsScreenSendsTheNamedModel", "TestEmbedContentThroughThePredictionClient",
			"TestStudioManagesLocalModelsAndTestsEmbeddings",
			"TestGRPCEmbedContent", "TestEmbedContentREST"}},
	{Resource: "Vertex AI PredictionService: GenerateContent", Service: "vertexai",
		Methods: []string{"PredictionService/GenerateContent"},
		Screen:  "/ai/playground", Offers: "Playground: Stream off sends the generateContent subset GenerateContent serves, through the same API path, and shows the one answer (#1257)",
		Tests: []string{"TestPlaygroundStreamSwitchPicksTheMethod", "TestStreamGenerateContentThroughThePredictionClient", "TestGRPCGenerateContentMatchesTheJSONRoute"}},
	{Resource: "Vertex AI PredictionService: StreamGenerateContent", Service: "vertexai",
		Methods: []string{"PredictionService/StreamGenerateContent"},
		Screen:  "/ai/playground", Offers: "Playground: Stream on (the default) sends streamGenerateContent, which StreamGenerateContent serves from the same code, and shows each chunk as it arrives (#1257)",
		Tests: []string{"TestPlaygroundGeneratesThroughTheRealAPIPath", "TestPlaygroundStreamSwitchPicksTheMethod",
			"TestStreamGenerateContentThroughThePredictionClient", "TestGRPCStreamGenerateContentEmitsOneMessagePerChunk"}},
	// Streaming and direct predict (#1285) carry the same requests in another
	// framing, served by the same code.
	{Resource: "Vertex AI PredictionService: StreamRawPredict", Service: "vertexai",
		Methods: []string{"PredictionService/StreamRawPredict"},
		Screen:  "/ai/playground", Offers: "Playground: Raw with Stream on sends the body as :streamRawPredict, which StreamRawPredict serves from the same code, and shows each chunk as it arrives; Raw with Stream off sends :rawPredict (#1284, #1285)",
		Tests: []string{"TestPlaygroundStreamSwitchPicksTheMethod", "TestStreamingPredictOnTheGenerationModel",
			"TestGRPCStreamRawPredictStreamsGeneration", "TestStreamRawPredictREST"}},
	{Resource: "Vertex AI PredictionService: tensor predict", Service: "vertexai",
		Methods: []string{"PredictionService/DirectPredict", "PredictionService/ServerStreamingPredict",
			"PredictionService/StreamingPredict", "PredictionService/StreamDirectPredict"},
		Screen: "/ai/playground", Offers: "Playground sends the prompt these methods carry as the Model Garden text instance, " +
			"and Embeddings the text-embedding instance, to the same local models through the same code; the tensor " +
			"methods are that request in Tensor form (#1285)",
		Tests: []string{"TestStreamingPredictOnTheGenerationModel", "TestStreamingPredictOnTheEmbeddingModel",
			"TestGRPCServerStreamingPredictStreamsGeneration", "TestGRPCDirectPredictEmbedsLikePredict"}},
	// A model's gRPC server (#1477): a custom endpoint whose model declares
	// grpcPorts, the Cloud Run port named h2c.
	{Resource: "Vertex AI PredictionService: DirectRawPredict", Service: "vertexai",
		Methods: []string{"PredictionService/DirectRawPredict"},
		Screen:  "/ai/predict", Offers: "Online prediction: on an endpoint whose model declares grpcPorts (its gRPC port in the endpoint's facts), " +
			"the DirectRawPredict form sends a method name and the input bytes (base64) to the container's gRPC server, as DirectRawPredict " +
			"relays them, and shows the output bytes (base64 and hex) or the container's status (#1477)",
		Tests: []string{"TestConsoleDirectRawPredictThroughTheConsoleAPI", "TestDirectRawPredictOnAModelWithGRPCPorts",
			"TestPredictionClientReachesAModelsGRPCServer", "TestDirectRawPredictRelaysToTheModelsGRPCServer"}},
	{Resource: "Vertex AI PredictionService: streaming raw predict on a gRPC model", Service: "vertexai",
		Methods:  []string{"PredictionService/StreamDirectRawPredict", "PredictionService/StreamingRawPredict"},
		Excluded: "the streaming forms of the DirectRawPredict the Online prediction page sends: the same method_name and input bytes on a stream held open both ways, which a request-and-answer form has no way to hold; the page's DirectRawPredict calls the same method on the same container (#1477)"},
	{Resource: "Starting the instance",
		Commands: []string{"up"},
		Excluded: "starts the process that serves the console, so the console cannot exist before it"},
	{Resource: "Stopping and destroying",
		Commands: []string{"stop", "delete"},
		Excluded: "ends the process that serves the console (stop) or destroys the cluster it runs on (delete); neither page could report its own outcome"},
	{Resource: "Workstation checks and offline cache",
		Commands: []string{"doctor", "prefetch"},
		Excluded: "run before an instance exists, to check prerequisites or fill the offline cache `up --offline` reads; a running instance's health is the dashboard's Components card"},
	{Resource: "Trust and lifecycle hooks",
		Commands: []string{"trust"},
		Excluded: "trusting ./cloudburrow.json and its hooks lets `up` run host scripts; that decision stays at the terminal of whoever owns the checkout, so no page can grant it"},
	{Resource: "Scripting and standalone tools",
		Commands: []string{"wait", "storage-server", "gcloud-teardown", "help"},
		Excluded: "a script's exit status (wait), a server run without a console by design (storage-server), removing host gcloud configuration the console never writes (gcloud-teardown), and usage text (help)"},
}

const (
	transactionReason = "session and transaction plumbing a client performs under a read or a write; " +
		"no resource or action of its own"
)

// TestConsoleParityTables holds parityRows to the coverage pages, the CLI's
// usage and the admin API's routes, and docs/console-parity.md §8 to the rows.
func TestConsoleParityTables(t *testing.T) {
	verified := verifiedCoverageMethods(t)
	commands := usageCommands(t)
	routes := adminRoutes(t)

	registry := map[string]bool{}
	d := coverageDeps(t)
	for _, p := range consoleProviders(d, clusterMetrics(d.cfg.KubeconfigPath()), console.NewSeries(console.SeriesLimit, nil)) {
		registry[p.ID()] = true
	}
	tests := repoTestNames(t)

	claimed := map[string]string{}
	claim := func(row parityRow, key string) {
		if prev, ok := claimed[key]; ok {
			t.Errorf("%s is in two rows: %q and %q", key, prev, row.Resource)
		}
		claimed[key] = row.Resource
	}
	for _, row := range parityRows {
		for _, m := range row.Methods {
			key := row.Service + " " + m
			claim(row, key)
			if _, ok := verified[key]; !ok {
				t.Errorf("row %q names %s, which docs/coverage/%s.md does not class Verified or Implemented", row.Resource, m, row.Service)
			}
		}
		for _, c := range row.Commands {
			claim(row, "command "+c)
			if !commands[c] {
				t.Errorf("row %q names command %q, which the usage text does not list", row.Resource, c)
			}
		}
		for _, r := range row.Routes {
			claim(row, "route "+r)
			if !routes[r] {
				t.Errorf("row %q names route %q, which internal/admin does not register", row.Resource, r)
			}
		}

		surface := row.Provider != "" || row.Screen != ""
		kinds := 0
		for _, set := range []bool{surface, row.Excluded != "", row.Issue != 0} {
			if set {
				kinds++
			}
		}
		if kinds != 1 {
			t.Errorf("row %q: set exactly one of a surface (Provider or Screen), Excluded and Issue", row.Resource)
		}
		if row.Provider != "" && !registry[row.Provider] {
			t.Errorf("row %q names provider %q, which consoleProviders does not build", row.Resource, row.Provider)
		}
		if surface && (row.Offers == "" || len(row.Tests) == 0) {
			t.Errorf("row %q has a console surface, so it needs Offers and at least one test", row.Resource)
		}
		for _, name := range row.Tests {
			if !tests[name] {
				t.Errorf("row %q cites %s, which no _test.go file in the repository defines", row.Resource, name)
			}
		}
	}

	for key, status := range verified {
		if _, ok := claimed[key]; !ok {
			t.Errorf("%s is %s on its coverage page and in no parityRows row: give it a console surface, "+
				"an exclusion or a gap issue (#782)", key, status)
		}
	}
	for c := range commands {
		if _, ok := claimed["command "+c]; !ok {
			t.Errorf("command %q is in no parityRows row: expose it in the console, or record why not", c)
		}
	}
	for r := range routes {
		if _, ok := claimed["route "+r]; !ok {
			t.Errorf("admin route %q is in no parityRows row: expose it in the console, or record why not", r)
		}
	}

	checkParitySection8(t, renderParityTables(parityRows))
}

// verifiedCoverageMethods reads the generated coverage JSON and returns every
// Verified or Implemented method, keyed "service shortMethod", with its
// status.
func verifiedCoverageMethods(t *testing.T) map[string]string {
	t.Helper()
	b, err := os.ReadFile("../../docs/coverage/coverage.json")
	if err != nil {
		t.Fatal(err)
	}
	var pages []struct {
		Key  string
		Rows []struct {
			Method string `json:"method"`
			Status string `json:"status"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(b, &pages); err != nil {
		t.Fatalf("coverage.json: %v", err)
	}
	out := map[string]string{}
	full := map[string]string{}
	for _, p := range pages {
		for _, r := range p.Rows {
			if r.Status != "Verified" && r.Status != "Implemented" {
				continue
			}
			key := p.Key + " " + shortMethod(r.Method)
			if prev, ok := full[key]; ok {
				t.Fatalf("%s and %s shorten to the same key %q", prev, r.Method, key)
			}
			full[key] = r.Method
			out[key] = r.Status
		}
	}
	if len(out) == 0 {
		t.Fatal("coverage.json has no Verified or Implemented method")
	}
	return out
}

// shortMethod drops a gRPC method's package: "google.cloud.tasks.v2.CloudTasks/CreateQueue"
// is "CloudTasks/CreateQueue". A name with no slash is kept whole.
func shortMethod(m string) string {
	i := strings.Index(m, "/")
	if i < 0 {
		return m
	}
	svc := m[:i]
	return svc[strings.LastIndex(svc, ".")+1:] + m[i:]
}

// usageCommands are the commands the usage text lists under "Commands:".
func usageCommands(t *testing.T) map[string]bool {
	t.Helper()
	_, section, ok := strings.Cut(usage, "\nCommands:\n")
	if !ok {
		t.Fatal("usage has no Commands: section")
	}
	out := map[string]bool{}
	for _, line := range strings.Split(section, "\n") {
		if line == "" {
			break
		}
		if m := regexp.MustCompile(`^  ([a-z][a-z-]*)(\s|$)`).FindStringSubmatch(line); m != nil {
			out[m[1]] = true
		}
	}
	if len(out) < 10 {
		t.Fatalf("read %d commands from the usage text, which is fewer than it lists", len(out))
	}
	return out
}

// adminRoutes are the routes internal/admin registers, read from its source
// so a new route cannot be missed.
func adminRoutes(t *testing.T) map[string]bool {
	t.Helper()
	files, err := filepath.Glob("../../internal/admin/*.go")
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`HandleFunc\("([A-Z]+ /admin/[^"]*)"`)
	out := map[string]bool{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range re.FindAllStringSubmatch(string(b), -1) {
			out[m[1]] = true
		}
	}
	if len(out) == 0 {
		t.Fatal("found no admin routes in internal/admin")
	}
	return out
}

// repoTestNames are the Test functions defined anywhere in the repository.
func repoTestNames(t *testing.T) map[string]bool {
	t.Helper()
	re := regexp.MustCompile(`(?m)^func (Test[A-Za-z0-9_]+)\(`)
	out := map[string]bool{}
	err := filepath.WalkDir("../..", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "bin":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, m := range re.FindAllStringSubmatch(string(b), -1) {
			out[m[1]] = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

const (
	parityBegin = "<!-- parity-table:begin (generated by TestConsoleParityTables; -update rewrites it) -->"
	parityEnd   = "<!-- parity-table:end -->"
)

// renderParityTables is §8's generated block: the service table, then the
// CLI and admin table.
func renderParityTables(rows []parityRow) string {
	var b strings.Builder
	cell := func(row parityRow) (string, string) {
		switch {
		case row.Issue != 0:
			return fmt.Sprintf("**Gap:** [#%d](https://github.com/cloudburrow/cloudburrow/issues/%d)", row.Issue, row.Issue), ""
		case row.Excluded != "":
			return "**Excluded:** " + row.Excluded, ""
		}
		where := "`" + row.Screen + "`"
		if row.Provider != "" {
			where = "`" + row.Provider + "` screen"
		}
		tests := make([]string, len(row.Tests))
		for i, n := range row.Tests {
			tests[i] = "`" + n + "`"
		}
		return where + ": " + row.Offers, strings.Join(tests, ", ")
	}
	code := func(xs []string) string {
		out := make([]string, len(xs))
		for i, x := range xs {
			out[i] = "`" + x + "`"
		}
		return strings.Join(out, ", ")
	}

	b.WriteString(parityBegin + "\n\n")
	b.WriteString("| Resource | Methods (Verified) | Console | Tests |\n|---|---|---|---|\n")
	for _, row := range rows {
		if row.Service == "" {
			continue
		}
		c, tests := cell(row)
		fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", row.Resource, code(row.Methods), c, tests)
	}
	b.WriteString("\n| Feature | Commands and admin routes | Console | Tests |\n|---|---|---|---|\n")
	for _, row := range rows {
		if row.Service != "" {
			continue
		}
		c, tests := cell(row)
		fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", row.Resource, code(slices.Concat(row.Commands, row.Routes)), c, tests)
	}
	b.WriteString("\n" + parityEnd)
	return b.String()
}

// checkParitySection8 compares the doc's generated block with want, or
// rewrites it under -update.
func checkParitySection8(t *testing.T, want string) {
	t.Helper()
	const path = "../../docs/console-parity.md"
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	doc := string(b)
	start := strings.Index(doc, parityBegin)
	end := strings.Index(doc, parityEnd)
	if start < 0 || end < start {
		t.Fatalf("console-parity.md has no generated parity block (%q … %q)", parityBegin, parityEnd)
	}
	got := doc[start : end+len(parityEnd)]
	if got == want {
		return
	}
	if *updateGolden {
		if err := os.WriteFile(path, []byte(doc[:start]+want+doc[end+len(parityEnd):]), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	t.Errorf("console-parity.md §8 is stale: run go test ./cmd/cloudburrow -run TestConsoleParityTables -update")
}
