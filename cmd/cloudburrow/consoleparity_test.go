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
	// Cloud Tasks
	{Resource: "Cloud Tasks queues", Service: "tasks",
		Methods:  []string{"CloudTasks/CreateQueue", "CloudTasks/DeleteQueue", "CloudTasks/GetQueue", "CloudTasks/ListQueues", "CloudTasks/PauseQueue", "CloudTasks/ResumeQueue", "CloudTasks/PurgeQueue"},
		Provider: "tasks", Offers: "Queues list, queue detail with configuration, Create queue, Delete, Pause / Resume, Purge",
		Tests: []string{"TestConsoleQueueActionsFollowState", "TestNoDetailShowsABlankProperty", "TestRowMenuClosesOnEscapeAndGivesFocusBack"}},
	{Resource: "Cloud Tasks tasks", Service: "tasks",
		Methods:  []string{"CloudTasks/GetTask", "CloudTasks/ListTasks", "CloudTasks/DeleteTask"},
		Provider: "tasks", Offers: "Tasks section on the queue page, task detail (request, headers redacted, body), Delete task",
		Tests: []string{"TestNoDetailShowsABlankProperty", "TestTaskHeadersAreRedacted"}},
	{Resource: "Cloud Tasks queue edit and task create", Service: "tasks",
		Methods:  []string{"CloudTasks/UpdateQueue", "CloudTasks/CreateTask"},
		Provider: "tasks", Offers: "Edit queue (rate limits, retry parameters) and Create task (HTTP target) on a queue's page (#784)",
		Tests: []string{"TestConsoleTasksEditQueueAndCreateTask", "TestTasksEditQueueThroughTheForm"}},

	// Secret Manager
	{Resource: "Secret Manager secrets", Service: "secretmanager",
		Methods:  []string{"SecretManagerService/CreateSecret", "SecretManagerService/DeleteSecret", "SecretManagerService/GetSecret", "SecretManagerService/ListSecrets", "SecretManagerService/UpdateSecret"},
		Provider: "secrets", Offers: "Secrets list, secret detail, Create secret (with its first value and the replication policy: Automatic, or User-managed with its locations; #857), Edit (labels, annotations), Delete",
		Tests: []string{"TestSecretCreateDoesNotLeaveAnEmptySecretBehind", "TestSecretCreateKeepsTheReplicationPolicy", "TestConsoleSecretReplicationPolicy", "TestSecretCreateReplicationPolicyThroughTheForm", "TestSecretListingsNeverCarryPayloads", "TestNoDetailShowsABlankProperty"}},
	{Resource: "Secret Manager versions", Service: "secretmanager",
		Methods:  []string{"SecretManagerService/AddSecretVersion", "SecretManagerService/AccessSecretVersion", "SecretManagerService/EnableSecretVersion", "SecretManagerService/DisableSecretVersion", "SecretManagerService/DestroySecretVersion", "SecretManagerService/GetSecretVersion", "SecretManagerService/ListSecretVersions"},
		Provider: "secrets", Offers: "Versions section, version detail, Add version, Enable / Disable / Destroy, Show value (recorded in Activity)",
		Tests: []string{"TestVersionActionsFollowTheVersionsState", "TestRevealRefusesWhatTheAPIWouldRefuse"}},

	// Cloud Run
	{Resource: "Cloud Run services", Service: "run",
		Methods:  []string{"Services/CreateService", "Services/DeleteService", "Services/GetService", "Services/ListServices", "Services/UpdateService"},
		Provider: "run", Offers: "Services list, service detail, Deploy container and Edit and deploy new revision (with labels and variables from Secret Manager, #852), Delete",
		Tests: []string{"TestConsoleRunEditAndDeployNewRevision", "TestRunEditDeploysTheFormThroughUpdateService", "TestCloudRunEditFormIsPrefilledAndGivesFocusBack",
			"TestConsoleRunServiceLabelsAndSecrets", "TestRunEditSetsLabelsAndSecretBackedVariables"}},
	{Resource: "Cloud Run revisions", Service: "run",
		Methods:  []string{"Revisions/GetRevision", "Revisions/ListRevisions"},
		Provider: "run", Offers: "Revision history and Traffic sections on the service page, revision detail",
		Tests: []string{"TestNoDetailShowsABlankProperty", "TestCloudRunRowSaysWhatIsServingAndWhyNot"}},
	{Resource: "Cloud Run jobs", Service: "run",
		Methods:  []string{"Jobs/CreateJob", "Jobs/DeleteJob", "Jobs/GetJob", "Jobs/ListJobs", "Jobs/RunJob", "Jobs/UpdateJob"},
		Provider: "run-jobs", Offers: "Jobs page (/run/jobs): jobs list, job detail with executions and configuration, Create job, Edit job, Execute, Delete (#785); labels and variables from Secret Manager on Create job and Edit job, and Execute with overrides (arguments, variables, task count, timeout) on a job's page (#852)",
		Tests: []string{"TestConsoleRunJobsCreateExecuteAndCancel", "TestRunJobsCreateExecuteCancelAndDeleteThroughTheAPI", "TestRunJobEditKeepsWhatTheFormDoesNotShow", "TestCloudRunJobCreatedExecutedAndDeletedInTheBrowser",
			"TestConsoleRunJobLabelsSecretsAndOverrides", "TestRunJobLabelsSecretsAndExecuteWithOverrides", "TestCloudRunJobExecuteWithOverridesThroughTheForm"}},
	{Resource: "Cloud Run executions", Service: "run",
		Methods:  []string{"Executions/CancelExecution", "Executions/DeleteExecution", "Executions/GetExecution", "Executions/ListExecutions"},
		Provider: "run-jobs", Offers: "Executions section on a job's page, execution detail (status, task counts, conditions, failure message, Logs), Cancel while running, Delete when finished (#785)",
		Tests: []string{"TestConsoleRunJobsCreateExecuteAndCancel", "TestRunJobExecutionPageShowsTheFailureAndItsTasksLogs"}},
	{Resource: "Cloud Run revision delete", Service: "run",
		Methods:  []string{"Revisions/DeleteRevision"},
		Provider: "run", Offers: "Delete revision on a Revision history row and a revision's page, for a revision that serves no traffic (#785)",
		Tests: []string{"TestConsoleRunEditAndDeployNewRevision", "TestRunRevisionDeleteIsOfferedOnlyWhereTheAPIAccepts"}},

	// Cloud KMS
	{Resource: "Cloud KMS key rings and keys", Service: "kms",
		Methods:  []string{"KeyManagementService/CreateKeyRing", "KeyManagementService/GetKeyRing", "KeyManagementService/ListKeyRings", "KeyManagementService/CreateCryptoKey", "KeyManagementService/GetCryptoKey", "KeyManagementService/ListCryptoKeys"},
		Provider: "kms", Offers: "Key rings list (every location), ring detail with keys, key detail, Create key ring, Create key (labels, destroy scheduled duration, rotation period and next rotation time, #852)",
		Tests: []string{"TestConsoleKMSActsThroughTheAPIAnSDKSees", "TestKMSCreateKeyRingThroughTheForm",
			"TestConsoleKMSCreateKeyOptions", "TestKMSCreateKeyWithDestroyDurationAndRotation", "TestKMSCreateKeyOptionsThroughTheForm"}},
	{Resource: "Cloud KMS key versions and crypto", Service: "kms",
		Methods: []string{"KeyManagementService/CreateCryptoKeyVersion", "KeyManagementService/GetCryptoKeyVersion", "KeyManagementService/ListCryptoKeyVersions", "KeyManagementService/UpdateCryptoKeyVersion",
			"KeyManagementService/DestroyCryptoKeyVersion", "KeyManagementService/RestoreCryptoKeyVersion", "KeyManagementService/UpdateCryptoKeyPrimaryVersion",
			"KeyManagementService/Encrypt", "KeyManagementService/Decrypt"},
		Provider: "kms", Offers: "Versions section, version detail, Add version, Make primary, Enable / Disable, Schedule destruction, Restore, Encrypt, Decrypt",
		Tests: []string{"TestConsoleKMSActsThroughTheAPIAnSDKSees"}},
	{Resource: "Cloud KMS key edit", Service: "kms",
		Methods:  []string{"KeyManagementService/UpdateCryptoKey"},
		Provider: "kms", Offers: "Edit key on a key's page: labels (#794), rotation period and next rotation time (#816)",
		Tests: []string{"TestConsoleKMSEditKey", "TestKMSEditKeyThroughUpdateCryptoKey", "TestKMSEditKeyRotationThroughUpdateCryptoKey",
			"TestKMSEditKeyLabelsThroughTheForm", "TestKMSEditKeyRotationThroughTheForm"}},

	// Resource Manager
	{Resource: "Resource Manager projects", Service: "resourcemanager",
		Methods:  []string{"Projects/CreateProject", "Projects/DeleteProject", "Projects/GetProject", "Projects/ListProjects", "Projects/UpdateProject"},
		Provider: "projects", Offers: "Project registry list, project detail, Create project, Edit labels, Delete",
		Tests: []string{"TestResourceManagerV3ListProjects", "TestStateSaveResetLoadRestoresEverything", "TestNoDetailShowsABlankProperty"}},
	{Resource: "Resource Manager project search", Service: "resourcemanager",
		Methods:  []string{"Projects/SearchProjects"},
		Excluded: "the query form of ListProjects over the same registry; the Projects list and its filter show the same projects"},

	// Cloud Scheduler
	{Resource: "Cloud Scheduler jobs", Service: "scheduler",
		Methods:  []string{"CloudScheduler/CreateJob", "CloudScheduler/DeleteJob", "CloudScheduler/GetJob", "CloudScheduler/ListJobs", "CloudScheduler/PauseJob", "CloudScheduler/ResumeJob", "CloudScheduler/RunJob"},
		Provider: "scheduler", Offers: "Jobs list, job detail, Create job (HTTP and Pub/Sub targets), Pause / Resume, Force run, Delete",
		Tests: []string{"TestConsoleSchedulerFollowsTheSDK", "TestSchedulerPauseFromARow"}},
	{Resource: "Cloud Scheduler job edit", Service: "scheduler",
		Methods:  []string{"CloudScheduler/UpdateJob"},
		Provider: "scheduler", Offers: "Edit job on a job's page: description, schedule, time zone, the target's fields and retry configuration, with the name, region and target type fixed (#795)",
		Tests: []string{"TestConsoleSchedulerEditJob", "TestSchedulerEditJobThroughUpdateJob", "TestSchedulerEditJobThroughTheForm"}},

	// Cloud Logging
	{Resource: "Cloud Logging entries", Service: "logging",
		Methods: []string{"LoggingServiceV2/ListLogEntries", "LoggingServiceV2/ListLogs"},
		Screen:  "/logs", Offers: "Logs Explorer: entries under source `logging/<log>`, filters in the URL, live stream",
		Tests: []string{"TestLoggingWriteAndRead", "TestFiltersNarrowTheView"}},
	{Resource: "Cloud Logging writes", Service: "logging",
		Methods:  []string{"LoggingServiceV2/WriteLogEntries"},
		Excluded: "the application's write path; the Logs Explorer shows what was written"},
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
		Provider: "pubsub-subscriptions", Offers: "Subscriptions list and detail, with how long a subscription was idle and when expiration deletes it, read from CloudBurrow's front (#996), Delete; on a topic page: Create subscription (push endpoint and attributes, #996; ack deadline, retention, retained acknowledged messages, message ordering, filter, retry and dead-letter policies, #852; exactly-once delivery and expiration period, #873), Pull and ack, Pull without ack",
		Tests: []string{"TestConsolePubSubSubscriptionsScreen", "TestConsolePubSubDeleteSubscription", "TestConsolePubSubActions", "TestSubscriptionsDeleteConfirmedByName",
			"TestConsolePubSubCreateOptions", "TestPubSubCreateOptionsReachTheAPI", "TestPubSubCreateSubscriptionDoesNotRetryAFilterRefusal",
			"TestPubSubCreateOptionsThroughTheForms", "TestPubSubSubscriptionPageShowsIdleTimeAndExpiry", "TestFormatIdle",
			"TestConsolePubSubIdleTimeAndPushAttributes", "TestPubSubIdleTimeAndPushAttributesThroughTheForms"}},
	{Resource: "Pub/Sub streaming pull and lease extension", Service: "pubsub",
		Methods:  []string{"Subscriber/StreamingPull", "Subscriber/ModifyAckDeadline"},
		Excluded: "a subscriber client's transport and lease management; the console pulls with Pull and either acknowledges at once or leaves the messages unacked"},
	{Resource: "Pub/Sub topic edit", Service: "pubsub",
		Methods:  []string{"Publisher/UpdateTopic"},
		Provider: "pubsub", Offers: "Edit topic on a topic's page: message retention, the one field the emulator's UpdateTopic applies (#786); labels, applied by CloudBurrow's front (#949) and held to Google's rules (#962)",
		Tests: []string{"TestConsolePubSubEditTopicAndSubscription", "TestPubSubEditTopicThroughUpdateTopic", "TestPubSubFormsHoldLabelsToGoogleRules"}},
	{Resource: "Pub/Sub subscription edit", Service: "pubsub",
		Methods:  []string{"Subscriber/UpdateSubscription"},
		Provider: "pubsub-subscriptions", Offers: "Edit subscription on a subscription's page: push endpoint and attributes (pull to push and back), ack deadline, retention, retained acknowledged messages, retry and dead-letter policies, exactly-once delivery (#880); the expiration period, which CloudBurrow's front applies (#891); filter and ordering shown fixed with the reason (#786); labels, applied by CloudBurrow's front (#949) and held to Google's rules (#962)",
		Tests: []string{"TestConsolePubSubEditTopicAndSubscription", "TestPubSubEditSubscriptionThroughUpdateSubscription", "TestPubSubEditSubscriptionThroughTheForm", "TestPubSubFormsHoldLabelsToGoogleRules",
			"TestPubSubEditSubscriptionExpirationThroughTheForm", "TestPubSubEditSubscriptionExactlyOnceThroughTheForm"}},
	{Resource: "Pub/Sub push configuration", Service: "pubsub",
		Methods:  []string{"Subscriber/ModifyPushConfig"},
		Excluded: "the one-field form of Edit subscription's push endpoint, which the console changes through UpdateSubscription's push_config, switching pull to push and back"},
	{Resource: "Pub/Sub snapshots and seek", Service: "pubsub",
		Methods:  []string{"Subscriber/CreateSnapshot", "Subscriber/DeleteSnapshot", "Subscriber/GetSnapshot", "Subscriber/ListSnapshots", "Subscriber/Seek", "Publisher/ListTopicSnapshots"},
		Provider: "pubsub-snapshots", Offers: "Snapshots list and detail, Delete; on a subscription page: Create snapshot, Seek to snapshot, Seek to time; on a topic page: its Snapshots",
		Tests: []string{"TestConsolePubSubSnapshotsAndSeek", "TestPubSubSnapshotSeekAndDeleteThroughTheForms"}},
	{Resource: "Pub/Sub schemas", Service: "pubsub",
		Methods: []string{"SchemaService/CommitSchema", "SchemaService/CreateSchema", "SchemaService/DeleteSchema", "SchemaService/DeleteSchemaRevision", "SchemaService/GetSchema",
			"SchemaService/ListSchemaRevisions", "SchemaService/ListSchemas", "SchemaService/RollbackSchema", "SchemaService/ValidateMessage", "SchemaService/ValidateSchema"},
		Provider: "pubsub-schemas", Offers: "Schemas list and detail (definition, revisions), Create schema (Avro, validated first), Delete; on a schema page: Commit revision, Test message, Delete schema; on a revision row: Roll back to this revision, Delete revision",
		Tests: []string{"TestConsolePubSubSchemas", "TestPubSubSchemaCreateShowsTheValidationError"}},

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
		Methods:  []string{"storage.objects.restore"},
		Provider: "storage", Offers: "Deleted objects tab on a bucket's page (generation, size, deleted and hard delete times), Restore on each (#789)",
		Tests: []string{"TestConsoleStorageBucketSettings", "TestStorageSoftDeletedRestoreThroughTheAPI"}},
	{Resource: "Cloud Storage soft-deleted buckets", Service: "storage",
		Methods:  []string{"storage.buckets.restore"},
		Provider: "storage-deleted", Offers: "Deleted buckets page under Cloud Storage (generation, deleted and hard delete times), Restore on each (#789)",
		Tests: []string{"TestConsoleStorageBucketSettings", "TestStorageSoftDeletedRestoreThroughTheAPI"}},
	{Resource: "Cloud Storage managed folders", Service: "storage",
		Methods:  []string{"storage.managedFolders.list", "storage.managedFolders.insert", "storage.managedFolders.get", "storage.managedFolders.delete"},
		Provider: "storage", Offers: "Managed folders in the bucket browser, as folders whose type is Managed folder (#789); Create managed folder on a bucket's page and each folder's, under its prefix, and Delete managed folder on a managed folder's row, confirmed by its name after a warning that the objects under it are kept (#828)",
		Tests: []string{"TestStorageBrowserMarksManagedFolders", "TestStorageBrowserListsManagedFoldersFromTheServer", "TestStorageBrowserCreatesAndDeletesManagedFolders", "TestConsoleStorageManagedFolders", "TestStorageManagedFoldersThroughTheBrowser"}},
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
	{Resource: "Cloud Storage notifications", Service: "storage",
		Methods:  []string{"storage.notifications.insert", "storage.notifications.get", "storage.notifications.list", "storage.notifications.delete"},
		Provider: "storage", Offers: "Notifications tab on a bucket's page (topic, event types, object name prefix, payload format, custom attributes); a notification's own page; Create notification, its topic chosen from the project's Pub/Sub topics, offered only where the storage server delivers to Pub/Sub; Delete on each row and page, confirmed by name (#791)",
		Tests: []string{"TestConsoleStorageNotifications", "TestStorageNotificationsThroughTheForm", "TestStorageNotificationsThroughTheAPI", "TestStorageNotificationsWithoutPubSub"}},
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
	{Resource: "Cloud Tasks permission check", Service: "tasks",
		Methods: []string{"CloudTasks/TestIamPermissions"}, Excluded: testIamPermissionsReason},
	{Resource: "Secret Manager permission check", Service: "secretmanager",
		Methods: []string{"SecretManagerService/TestIamPermissions"}, Excluded: testIamPermissionsReason},
	{Resource: "Cloud KMS permission check", Service: "kms",
		Methods: []string{"IAMPolicy/TestIamPermissions"}, Excluded: testIamPermissionsReason},
	{Resource: "Cloud Storage permission check", Service: "storage",
		Methods: []string{"storage.buckets.testIamPermissions", "storage.managedFolders.testIamPermissions"}, Excluded: testIamPermissionsReason},

	// Firestore and Datastore
	{Resource: "Firestore documents (read)", Service: "firestore",
		Methods:  []string{"Firestore/BatchGetDocuments", "Firestore/RunQuery"},
		Provider: "firestore", Offers: "Collections, documents, per-field detail, query builder; every value inside a map or array field, each on its own page (#995)",
		Tests: []string{"TestFirestoreTypeDistinguishesWhatAFlatCellCannot", "TestQueryFormsCoverTheOperatorsTheyDocument", "TestFirestoreElementsAreListedAndAddressed"}},
	{Resource: "Firestore transactions", Service: "firestore",
		Methods: []string{"Firestore/BeginTransaction"}, Excluded: transactionReason},
	{Resource: "Firestore document writes", Service: "firestore",
		Methods:  []string{"Firestore/Commit"},
		Provider: "firestore", Offers: "Start collection, Add document (auto ID or named), Add field, Edit field and Delete field (typed: string, number, boolean, null, timestamp, geopoint, reference, bytes as base64, map, array), Delete document; Start collection on a document's page makes a subcollection with its first document (#854); a map or array field's Elements tab, with Edit value, Add value and Remove value on each value inside it, writing only that value (#995)",
		Tests: []string{"TestConsoleFirestoreDocumentCreateEditDelete", "TestEditFormsRoundTripTheStoredType", "TestConsoleFirestoreSubcollections", "TestFirestoreStartASubcollectionOnADocumentPage",
			"TestConsoleFirestoreEditValuesInsideMapsAndArrays", "TestFirestoreEditValueSavedUnchangedWritesTheSameValue", "TestFirestoreAddAndRemoveValueChangeOnlyThatValue",
			"TestFirestoreValueWritesAreMaskedToWhatChanged", "TestFirestoreValueActionsRefuseAChangedField", "TestFirestoreBytesRoundTripAsBase64",
			"TestFirestoreEditAddRemoveAndBytesValuesThroughTheBrowser"}},
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
		Methods: []string{"Datastore/BeginTransaction"}, Excluded: transactionReason},
	{Resource: "Datastore entity writes", Service: "datastore",
		Methods:  []string{"Datastore/Commit"},
		Provider: "datastore", Offers: "Create entity (key name, name=… or id=N as the Name/ID column shows them (#885), or auto ID; in a namespace from the Datastore screen or a namespace's page), Create child entity on an entity's page, Add property, Edit property and Delete property (typed, with Exclude from indexes), Delete entity (#854); a key value is shown and edited as Kind/name=… or Kind/id=…, ancestors first, with __namespace__/{namespace} first for a key in a namespace other than the default, so Edit property saved unchanged writes the same key back, a name like id=7 included (#887); Add, Edit and Delete property write the entity back through the v1 API, so a key value in another project or database, shown with where it is and offered no edit, and every value they do not touch are kept as they were (#893); a key in an array or embedded entity is shown as key(Kind/name=…) (#894); an array or embedded entity property's Elements tab lists every value inside it, each with its own page, and Edit value, typed as Edit property is, writes back that one value through the v1 API, every other value exactly as it was read; a key in another project or database is offered no Edit value (#905); an array or embedded entity inside a property is listed on the Elements tab too, Add value inserts into an array at an index or appends, and adds a property to an embedded entity or its key (__key__) when it has none, and Remove value, confirmed by typing the value's name back, removes a value, a property or an embedded entity's key, each written through the v1 API with every other value and index flag as it was read; a value excluded from indexes before an indexed one in the same array, which Datastore stores after them, is refused rather than moved (#911); a blob is shown and edited as base64, standard encoding as the v1 REST API's blobValue, with Edit property, Add property and Edit value, Edit value with an Exclude from indexes prefilled as the value's own, and a blob over 1,500 bytes is refused unless excluded, as the client refuses it (#912); Edit value, Add value, Remove value and Exclude from indexes carry a digest of the property as the page read it and are refused, in the same transaction, when another writer changed that property since, saying the property changed since the page was loaded and to reload it (#923); Exclude from indexes on an array or embedded entity property and on every array or embedded entity inside one sets or clears the flag on every value inside it in one write, keeping every value and its order, and indexing is refused while a string or blob inside is over 1,500 bytes (#924)",
		Tests: []string{"TestConsoleDatastoreEntityCreateEditDelete", "TestEditFormsRoundTripTheStoredType", "TestConsoleDatastoreNamespacesAndChildren", "TestDatastoreNamespaceAndChildEntityThroughTheBrowser", "TestConsoleDatastoreNameIDTellsANameFromAnID", "TestDatastoreNameIDColumnTellsANameFromAnID", "TestDatastoreKeyValuesRoundTripThroughEditProperty", "TestConsoleDatastoreKeyValuesRoundTripThroughEditProperty", "TestDatastoreKeyValueEditPropertyIsANoOp", "TestConsoleDatastoreKeepsAKeyValuesProjectAndDatabase", "TestDatastoreKeyValuesKeepTheirProjectAndDatabase", "TestDatastoreFormValuesAreWrittenAsTheClientWritesThem", "TestNestedDatastoreValuesRenderAsTheirType", "TestDatastoreNestedAndForeignKeysThroughTheBrowser", "TestConsoleDatastoreEditValueInsideArraysAndEntities", "TestDatastoreElementsAreListedAndAddressed", "TestDatastoreEditValueWritesOnlyThatValue", "TestDatastoreEditValueInsideAnArrayThroughTheBrowser", "TestConsoleDatastoreAddAndRemoveValues", "TestDatastoreAddAndRemoveValueChangeOnlyThatValue", "TestDatastoreValueActionsAreOfferedWhereTheyApply", "TestConsoleDatastoreBlobValuesAsBase64", "TestDatastoreBlobValuesRoundTripAsBase64", "TestDatastoreAddRemoveAndBlobValuesThroughTheBrowser", "TestConsoleDatastoreValueActionsRefuseAConcurrentChange", "TestDatastoreValueActionsRefuseAChangedProperty", "TestHiddenFieldsAreSentBackAsTheyCame", "TestConsoleDatastoreExcludeWholeArrayFromIndexes", "TestDatastoreExcludeFromIndexesSetsEveryValue", "TestDatastoreStaleValueActionIsRefusedAndWholeArrayExcludedThroughTheBrowser"}},

	// Bigtable
	{Resource: "Bigtable tables and rows (read)", Service: "bigtable",
		Methods:  []string{"BigtableTableAdmin/CreateTable", "BigtableTableAdmin/GetTable", "BigtableTableAdmin/ListTables", "Bigtable/ReadRows"},
		Provider: "bigtable", Offers: "Tables, column families, rows, per-cell detail, row-range reader, Create table, Delete",
		Tests: []string{"TestConsoleBigtableCreateAndDeleteTable"}},
	{Resource: "Bigtable column families and row writes", Service: "bigtable",
		Methods:  []string{"BigtableTableAdmin/ModifyColumnFamilies", "Bigtable/MutateRow"},
		Provider: "bigtable", Offers: "Add column family, Edit GC policy (max versions, max age, either or both) and Delete column family; Write cell (row key, family, qualifier, value, optional timestamp), Delete cells in a column, Delete row",
		Tests: []string{"TestConsoleBigtableFamiliesAndRowWrites", "TestBigtableFamilyAndRowActionsThroughTheOfficialClients"}},

	// Spanner
	{Resource: "Spanner instances, databases and reads", Service: "spanner",
		Methods: []string{"InstanceAdmin/CreateInstance", "InstanceAdmin/GetInstance", "DatabaseAdmin/CreateDatabase", "DatabaseAdmin/GetDatabase", "DatabaseAdmin/GetDatabaseDdl",
			"Spanner/ExecuteStreamingSql", "Spanner/StreamingRead"},
		Provider: "spanner", Offers: "Instances, databases, tables, columns and indexes, DDL, read-only query editor, Create database, Drop database, Delete instance",
		Tests: []string{"TestConsoleSpannerCreateAndDropDatabase"}},
	{Resource: "Spanner sessions and transactions", Service: "spanner",
		Methods: []string{"Spanner/CreateSession", "Spanner/BeginTransaction"}, Excluded: transactionReason},
	{Resource: "Spanner DML", Service: "spanner",
		Methods:  []string{"Spanner/Commit"},
		Provider: "spanner", Offers: "Read-write mode in the query editor: one INSERT, UPDATE or DELETE, confirmed with the database named, committed in a read-write transaction, rows affected reported (#798)",
		Tests: []string{"TestConsoleSpannerDML", "TestSpannerStatementClassifier", "TestQueryModeChoosesTheCallNotTheText"}},

	// BigQuery (#993): its REST methods, from the coverage page the official
	// generated client's methods make (docs/coverage/bigquery.md).
	{Resource: "BigQuery datasets", Service: "bigquery",
		Methods:  []string{"bigquery.datasets.insert", "bigquery.datasets.get", "bigquery.datasets.list", "bigquery.datasets.delete"},
		Provider: "bigquery", Offers: "Datasets list with each one's table count, a dataset's page (tables, details), Create dataset (location, description, labels) and Delete dataset with its tables (#698, #854); the one project the emulator serves, and a prompt naming it for any other",
		Tests: []string{"TestConsoleBigQueryDatasetsSchemaAndQuery", "TestConsoleBigQueryDatasetTableAndRowWrites", "TestBigQueryWritesStayInTheServedProject"}},
	{Resource: "BigQuery tables and rows", Service: "bigquery",
		Methods:  []string{"bigquery.tables.insert", "bigquery.tables.get", "bigquery.tables.list", "bigquery.tables.patch", "bigquery.tables.delete", "bigquery.tabledata.insertAll", "bigquery.tabledata.list"},
		Provider: "bigquery", Offers: "Tables on a dataset's page; a table's page with its schema, details and Preview (tabledata.list); Create table with the schema editor, RECORD fields to 15 levels and flexible field names included, or a view with its query; Insert rows (tabledata.insertAll) with Skip invalid rows and Ignore unknown values; Edit table's description and labels (tables.patch: a label set, changed or removed, the description changed or cleared, #1025; adding fields waits on #1013); Delete table (#698, #854, #874, #994)",
		Tests: []string{"TestConsoleBigQueryDatasetsSchemaAndQuery", "TestConsoleBigQueryDatasetTableAndRowWrites", "TestConsoleBigQueryEditTableViewsAndInsertOptions",
			"TestBigQueryCreateViewAndEditTable", "TestBigQueryInsertRowsOptions", "TestBigQueryFieldNamesAreFlexible",
			"TestBigQueryCreateTableInsertRowsAndDeleteThroughTheForms", "TestBigQueryRecordColumnsThroughTheSchemaEditor",
			"TestBigQueryFlexibleNamesInsertOptionsEditTableAndViewThroughTheForms"}},
	{Resource: "BigQuery table update (tables.update)", Service: "bigquery",
		Methods:  []string{"bigquery.tables.update"},
		Provider: "bigquery", Offers: "Edit table on a table's page, which sends tables.patch, not tables.update: the front carries out both on one path (#1054), so the description and labels Edit table changes are what a tables.update of them changes; adding fields in Edit table is #1013 (#1048)",
		Tests: []string{"TestConsoleBigQueryEditTableViewsAndInsertOptions", "TestBigQueryFlexibleNamesInsertOptionsEditTableAndViewThroughTheForms"}},
	{Resource: "BigQuery queries", Service: "bigquery",
		Methods:  []string{"bigquery.jobs.query"},
		Provider: "bigquery", Offers: "The query editor on every dataset and table page, run with jobs.query: read-only by default, one SELECT or WITH … SELECT, at most 200 rows (#698); an opt-in, confirmed Read-write mode for DDL and scripts with DECLARE (#994), and a DML statement of its own (INSERT, UPDATE, DELETE, MERGE, TRUNCATE TABLE) answered with the rows it changed (#1024); DML inside a script (#1028) and UPDATE … FROM (#1027) refused with the reason",
		Tests: []string{"TestConsoleBigQueryDatasetsSchemaAndQuery", "TestTheBigQueryEditorRunsOnlyOneSelect",
			"TestConsoleBigQueryReadWriteEditorRunsDDLAndScripts", "TestConsoleBigQueryReadWriteEditorRunsDML",
			"TestTheBigQueryReadWriteEditorScreensStatements", "TestBigQueryDMLAnswerSaysWhatChanged",
			"TestBigQueryWriteReportSendsJobsQuery", "TestBigQueryReadWriteEditorThroughTheForms"}},
	{Resource: "BigQuery load and export jobs", Service: "bigquery",
		Methods:  []string{"bigquery.jobs.insert"},
		Provider: "bigquery", Offers: "Load from Cloud Storage on a dataset's and a table's page (gs:// URIs; CSV, JSON or Parquet; write preference; schema or auto-detect; the CSV options the front serves) and Export to Cloud Storage on a table's page (one URI; CSV or JSON; GZIP; delimiter; header), each answered with its job, linked to Job history, and refused in the API's words (#993); a load of a file uploaded in the browser is #999",
		Tests: []string{"TestConsoleBigQueryLoadExportAndJobHistory", "TestBigQueryLoadFormBecomesTheLoadsSource", "TestBigQueryLoadExportAndJobHistoryThroughTheForms"}},
	{Resource: "BigQuery job history", Service: "bigquery",
		Methods:  []string{"bigquery.jobs.get", "bigquery.jobs.list", "bigquery.jobs.cancel", "bigquery.jobs.delete"},
		Provider: "bigquery-jobs", Offers: "Job history page (/bigquery-jobs): the project's jobs from jobs.list, newest first, with type, state, creation time and error; a job's page (jobs.get) with its state, times, errorResult, configuration, load statistics (output rows, bad records, input files and bytes, each only when reported) and status.errors; Delete job on the page and the row; Cancel job only on a job that is not DONE (#993)",
		Tests: []string{"TestConsoleBigQueryLoadExportAndJobHistory", "TestBigQueryJobPageShowsWhatTheJobReported", "TestBigQueryJobActionsFollowTheJobsState", "TestBigQueryLoadExportAndJobHistoryThroughTheForms"}},

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
		Screen: "/requests", Offers: "Request Log: the recorder's request events, filtered by service, code and project, live; its fault events are Recent faults on the Fault injection page (#800)",
		Tests: []string{"TestTheRequestLogShowsServedCalls"}},
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
	{Resource: "Vertex AI custom prediction (serving contract)",
		Screen: "/ai/predict", Offers: "Online prediction: the Cloud Run services configured with the contract's AIP_* variables, " +
			"JSON instances and parameters, Predict, the container's status and body verbatim; Vertex's Endpoint, " +
			"PredictionService and batch prediction named as not served (#869)",
		Tests: []string{"TestConsoleOnlinePredictionThroughTheConsoleAPI", "TestCloudRunPredictorAnswersOnlinePredictionInTheBrowser",
			"TestPredictRelaysThroughTheIngressAndShowsTheAnswerVerbatim", "TestPredictRefusesWhatItCannotSend"}},
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
	testIamPermissionsReason = "a caller asks which of the permissions it names it holds; CloudBurrow stores policies " +
		"and enforces none, so there is nothing a page could show"
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
