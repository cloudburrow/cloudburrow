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
		Provider: "secrets", Offers: "Secrets list, secret detail, Create secret (with its first value), Edit (labels, annotations), Delete",
		Tests: []string{"TestSecretCreateDoesNotLeaveAnEmptySecretBehind", "TestSecretListingsNeverCarryPayloads", "TestNoDetailShowsABlankProperty"}},
	{Resource: "Secret Manager versions", Service: "secretmanager",
		Methods:  []string{"SecretManagerService/AddSecretVersion", "SecretManagerService/AccessSecretVersion", "SecretManagerService/EnableSecretVersion", "SecretManagerService/DisableSecretVersion", "SecretManagerService/DestroySecretVersion", "SecretManagerService/GetSecretVersion", "SecretManagerService/ListSecretVersions"},
		Provider: "secrets", Offers: "Versions section, version detail, Add version, Enable / Disable / Destroy, Show value (recorded in Activity)",
		Tests: []string{"TestVersionActionsFollowTheVersionsState", "TestRevealRefusesWhatTheAPIWouldRefuse"}},

	// Cloud Run
	{Resource: "Cloud Run services", Service: "run",
		Methods:  []string{"Services/CreateService", "Services/DeleteService", "Services/GetService", "Services/ListServices", "Services/UpdateService"},
		Provider: "run", Offers: "Services list, service detail, Deploy container, Edit and deploy new revision, Delete",
		Tests: []string{"TestConsoleRunEditAndDeployNewRevision", "TestRunEditDeploysTheFormThroughUpdateService", "TestCloudRunEditFormIsPrefilledAndGivesFocusBack"}},
	{Resource: "Cloud Run revisions", Service: "run",
		Methods:  []string{"Revisions/GetRevision", "Revisions/ListRevisions"},
		Provider: "run", Offers: "Revision history and Traffic sections on the service page, revision detail",
		Tests: []string{"TestNoDetailShowsABlankProperty", "TestCloudRunRowSaysWhatIsServingAndWhyNot"}},
	{Resource: "Cloud Run jobs, executions and revision delete", Service: "run",
		Methods: []string{"Jobs/CreateJob", "Jobs/DeleteJob", "Jobs/GetJob", "Jobs/ListJobs", "Jobs/RunJob", "Jobs/UpdateJob",
			"Executions/CancelExecution", "Executions/DeleteExecution", "Executions/GetExecution", "Executions/ListExecutions",
			"Revisions/DeleteRevision"}, Issue: 785},

	// Cloud KMS
	{Resource: "Cloud KMS key rings and keys", Service: "kms",
		Methods:  []string{"KeyManagementService/CreateKeyRing", "KeyManagementService/GetKeyRing", "KeyManagementService/ListKeyRings", "KeyManagementService/CreateCryptoKey", "KeyManagementService/GetCryptoKey", "KeyManagementService/ListCryptoKeys"},
		Provider: "kms", Offers: "Key rings list (every location), ring detail with keys, key detail, Create key ring, Create key",
		Tests: []string{"TestConsoleKMSActsThroughTheAPIAnSDKSees", "TestKMSCreateKeyRingThroughTheForm"}},
	{Resource: "Cloud KMS key versions and crypto", Service: "kms",
		Methods: []string{"KeyManagementService/CreateCryptoKeyVersion", "KeyManagementService/GetCryptoKeyVersion", "KeyManagementService/ListCryptoKeyVersions", "KeyManagementService/UpdateCryptoKeyVersion",
			"KeyManagementService/DestroyCryptoKeyVersion", "KeyManagementService/RestoreCryptoKeyVersion", "KeyManagementService/UpdateCryptoKeyPrimaryVersion",
			"KeyManagementService/Encrypt", "KeyManagementService/Decrypt"},
		Provider: "kms", Offers: "Versions section, version detail, Add version, Make primary, Enable / Disable, Schedule destruction, Restore, Encrypt, Decrypt",
		Tests: []string{"TestConsoleKMSActsThroughTheAPIAnSDKSees"}},
	{Resource: "Cloud KMS key edit", Service: "kms",
		Methods:  []string{"KeyManagementService/UpdateCryptoKey"},
		Provider: "kms", Offers: "Edit key (labels) on a key's page; rotation is not offered, since UpdateCryptoKey refuses it here (#794)",
		Tests: []string{"TestConsoleKMSEditKey", "TestKMSEditKeyThroughUpdateCryptoKey", "TestKMSEditKeyLabelsThroughTheForm"}},

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
		Methods: []string{"CloudScheduler/UpdateJob"}, Issue: 795},

	// Cloud Logging
	{Resource: "Cloud Logging entries", Service: "logging",
		Methods: []string{"LoggingServiceV2/ListLogEntries", "LoggingServiceV2/ListLogs"},
		Screen:  "/logs", Offers: "Logs Explorer: entries under source `logging/<log>`, filters in the URL, live stream",
		Tests: []string{"TestLoggingWriteAndRead", "TestFiltersNarrowTheView"}},
	{Resource: "Cloud Logging writes", Service: "logging",
		Methods:  []string{"LoggingServiceV2/WriteLogEntries"},
		Excluded: "the application's write path; the Logs Explorer shows what was written"},
	{Resource: "Cloud Logging log delete", Service: "logging",
		Methods: []string{"LoggingServiceV2/DeleteLog"}, Issue: 799},

	// Pub/Sub
	{Resource: "Pub/Sub topics", Service: "pubsub",
		Methods:  []string{"Publisher/CreateTopic", "Publisher/DeleteTopic", "Publisher/GetTopic", "Publisher/ListTopics", "Publisher/ListTopicSubscriptions", "Publisher/Publish"},
		Provider: "pubsub", Offers: "Topics list, topic detail with its subscriptions, Create topic (with a default subscription), Delete, Publish message",
		Tests: []string{"TestConsoleCreatedTopicIsVisibleToTheOfficialSDK", "TestSDKCreatedTopicAppearsInTheConsole", "TestConsolePubSubActions"}},
	{Resource: "Pub/Sub subscriptions", Service: "pubsub",
		Methods:  []string{"Subscriber/CreateSubscription", "Subscriber/DeleteSubscription", "Subscriber/GetSubscription", "Subscriber/ListSubscriptions", "Subscriber/Pull", "Subscriber/Acknowledge"},
		Provider: "pubsub-subscriptions", Offers: "Subscriptions list and detail, Delete; on a topic page: Create subscription, Pull and ack, Pull without ack",
		Tests: []string{"TestConsolePubSubSubscriptionsScreen", "TestConsolePubSubDeleteSubscription", "TestConsolePubSubActions", "TestSubscriptionsDeleteConfirmedByName"}},
	{Resource: "Pub/Sub streaming pull and lease extension", Service: "pubsub",
		Methods:  []string{"Subscriber/StreamingPull", "Subscriber/ModifyAckDeadline"},
		Excluded: "a subscriber client's transport and lease management; the console pulls with Pull and either acknowledges at once or leaves the messages unacked"},
	{Resource: "Pub/Sub topic and subscription edit", Service: "pubsub",
		Methods: []string{"Publisher/UpdateTopic", "Subscriber/UpdateSubscription", "Subscriber/ModifyPushConfig"}, Issue: 786},
	{Resource: "Pub/Sub snapshots and seek", Service: "pubsub",
		Methods: []string{"Subscriber/CreateSnapshot", "Subscriber/DeleteSnapshot", "Subscriber/GetSnapshot", "Subscriber/ListSnapshots", "Subscriber/Seek", "Publisher/ListTopicSnapshots"}, Issue: 787},
	{Resource: "Pub/Sub schemas", Service: "pubsub",
		Methods: []string{"SchemaService/CommitSchema", "SchemaService/CreateSchema", "SchemaService/DeleteSchema", "SchemaService/DeleteSchemaRevision", "SchemaService/GetSchema",
			"SchemaService/ListSchemaRevisions", "SchemaService/ListSchemas", "SchemaService/RollbackSchema", "SchemaService/ValidateMessage", "SchemaService/ValidateSchema"}, Issue: 788},

	// Cloud Storage
	{Resource: "Cloud Storage buckets", Service: "storage",
		Methods:  []string{"storage.buckets.insert", "storage.buckets.delete", "storage.buckets.get", "storage.buckets.list"},
		Provider: "storage", Offers: "Buckets list, bucket detail (retention, lifecycle), Create, Delete",
		Tests: []string{"TestConsoleCreatedBucketIsVisibleToTheOfficialSDK", "TestCreateBucketThroughTheForm", "TestConsoleStorageShowsRetentionAndLifecycle"}},
	{Resource: "Cloud Storage objects", Service: "storage",
		Methods:  []string{"storage.objects.list", "storage.objects.get", "storage.objects.insert", "storage.objects.delete"},
		Provider: "storage", Offers: "Objects and prefixes in the bucket browser, Upload file, Download, Preview, Delete",
		Tests: []string{"TestConsoleStorageObjects"}},
	{Resource: "Cloud Storage storage layout", Service: "storage",
		Methods:  []string{"storage.buckets.getStorageLayout"},
		Excluded: "a capability read a client makes to choose its request paths (location and hierarchical namespace); it is no resource or action of its own"},
	{Resource: "Cloud Storage bucket settings, retention lock, soft delete, managed folders", Service: "storage",
		Methods: []string{"storage.buckets.patch", "storage.buckets.update", "storage.buckets.lockRetentionPolicy", "storage.buckets.restore", "storage.objects.restore", "storage.managedFolders.list"}, Issue: 789},
	{Resource: "Cloud Storage object copy, move, compose, rewrite, metadata", Service: "storage",
		Methods:  []string{"storage.objects.rewrite", "storage.objects.move", "storage.objects.compose", "storage.objects.patch"},
		Provider: "storage", Offers: "Object page (generation, metageneration, hashes, headers, custom metadata, holds); Copy, Move or rename, Edit metadata, Edit storage class on an object's row and page; Compose on checked objects; an existing destination replaced only when asked and confirmed (#790)",
		Tests: []string{"TestConsoleStorageObjectOperations", "TestStorageObjectMetadataAndCopyThroughTheForms", "TestStorageObjectActionsThroughTheAPI"}},
	{Resource: "Cloud Storage single-request copy and full metadata replace", Service: "storage",
		Methods:  []string{"storage.objects.copy", "storage.objects.update"},
		Excluded: "the one-request and replace-everything forms of edits the console makes another way: Copy is objects.rewrite, as the official client's Copier sends it, and Edit metadata is objects.patch, which changes the fields the form holds without clearing the ones it does not show"},
	{Resource: "Cloud Storage notifications", Service: "storage",
		Methods:  []string{"storage.notifications.insert", "storage.notifications.get", "storage.notifications.list", "storage.notifications.delete"},
		Provider: "storage", Offers: "Notifications tab on a bucket's page (topic, event types, object name prefix, payload format, custom attributes); a notification's own page; Create notification, its topic chosen from the project's Pub/Sub topics, offered only where the storage server delivers to Pub/Sub; Delete on each row and page, confirmed by name (#791)",
		Tests: []string{"TestConsoleStorageNotifications", "TestStorageNotificationsThroughTheForm", "TestStorageNotificationsThroughTheAPI", "TestStorageNotificationsWithoutPubSub"}},
	{Resource: "Cloud Storage HMAC keys and service account", Service: "storage",
		Methods: []string{"storage.projects.hmacKeys.create", "storage.projects.hmacKeys.delete", "storage.projects.hmacKeys.get", "storage.projects.hmacKeys.list", "storage.projects.hmacKeys.update", "storage.projects.serviceAccount.get"}, Issue: 792},

	// IAM, which four services serve the same way.
	{Resource: "Cloud Tasks IAM policy", Service: "tasks",
		Methods: []string{"CloudTasks/GetIamPolicy", "CloudTasks/SetIamPolicy"}, Issue: 793},
	{Resource: "Secret Manager IAM policy", Service: "secretmanager",
		Methods: []string{"SecretManagerService/GetIamPolicy", "SecretManagerService/SetIamPolicy"}, Issue: 793},
	{Resource: "Cloud KMS IAM policy", Service: "kms",
		Methods: []string{"IAMPolicy/GetIamPolicy", "IAMPolicy/SetIamPolicy"}, Issue: 793},
	{Resource: "Cloud Storage bucket IAM policy", Service: "storage",
		Methods: []string{"storage.buckets.getIamPolicy", "storage.buckets.setIamPolicy"}, Issue: 793},
	{Resource: "Cloud Tasks permission check", Service: "tasks",
		Methods: []string{"CloudTasks/TestIamPermissions"}, Excluded: testIamPermissionsReason},
	{Resource: "Secret Manager permission check", Service: "secretmanager",
		Methods: []string{"SecretManagerService/TestIamPermissions"}, Excluded: testIamPermissionsReason},
	{Resource: "Cloud KMS permission check", Service: "kms",
		Methods: []string{"IAMPolicy/TestIamPermissions"}, Excluded: testIamPermissionsReason},
	{Resource: "Cloud Storage permission check", Service: "storage",
		Methods: []string{"storage.buckets.testIamPermissions"}, Excluded: testIamPermissionsReason},

	// Firestore and Datastore
	{Resource: "Firestore documents (read)", Service: "firestore",
		Methods:  []string{"Firestore/BatchGetDocuments", "Firestore/RunQuery"},
		Provider: "firestore", Offers: "Collections, documents, per-field detail, query builder",
		Tests: []string{"TestFirestoreTypeDistinguishesWhatAFlatCellCannot", "TestQueryFormsCoverTheOperatorsTheyDocument"}},
	{Resource: "Firestore transactions", Service: "firestore",
		Methods: []string{"Firestore/BeginTransaction"}, Excluded: transactionReason},
	{Resource: "Firestore document writes", Service: "firestore",
		Methods:  []string{"Firestore/Commit"},
		Provider: "firestore", Offers: "Start collection, Add document (auto ID or named), Add field, Edit field and Delete field (typed: string, number, boolean, null, timestamp, geopoint, reference, map, array), Delete document",
		Tests: []string{"TestConsoleFirestoreDocumentCreateEditDelete", "TestEditFormsRoundTripTheStoredType"}},
	{Resource: "Datastore entities (read)", Service: "datastore",
		Methods:  []string{"Datastore/Lookup", "Datastore/RunQuery"},
		Provider: "datastore", Offers: "Kinds, entities, per-property detail, query builder",
		Tests: []string{"TestDatastoreKeyRoundTripsWhatTheListingRendered", "TestQueryFormsCoverTheOperatorsTheyDocument"}},
	{Resource: "Datastore transactions", Service: "datastore",
		Methods: []string{"Datastore/BeginTransaction"}, Excluded: transactionReason},
	{Resource: "Datastore entity writes", Service: "datastore",
		Methods:  []string{"Datastore/Commit"},
		Provider: "datastore", Offers: "Create entity (key name, id=N or auto ID), Add property, Edit property and Delete property (typed, with Exclude from indexes), Delete entity",
		Tests: []string{"TestConsoleDatastoreEntityCreateEditDelete", "TestEditFormsRoundTripTheStoredType"}},

	// Bigtable
	{Resource: "Bigtable tables and rows (read)", Service: "bigtable",
		Methods:  []string{"BigtableTableAdmin/CreateTable", "BigtableTableAdmin/GetTable", "BigtableTableAdmin/ListTables", "Bigtable/ReadRows"},
		Provider: "bigtable", Offers: "Tables, column families, rows, per-cell detail, row-range reader, Create table, Delete",
		Tests: []string{"TestConsoleBigtableCreateAndDeleteTable"}},
	{Resource: "Bigtable column families and row writes", Service: "bigtable",
		Methods: []string{"BigtableTableAdmin/ModifyColumnFamilies", "Bigtable/MutateRow"}, Issue: 797},

	// Spanner
	{Resource: "Spanner instances, databases and reads", Service: "spanner",
		Methods: []string{"InstanceAdmin/CreateInstance", "InstanceAdmin/GetInstance", "DatabaseAdmin/CreateDatabase", "DatabaseAdmin/GetDatabase", "DatabaseAdmin/GetDatabaseDdl",
			"Spanner/ExecuteStreamingSql", "Spanner/StreamingRead"},
		Provider: "spanner", Offers: "Instances, databases, tables, columns and indexes, DDL, read-only query editor, Create database, Drop database, Delete instance",
		Tests: []string{"TestConsoleSpannerCreateAndDropDatabase"}},
	{Resource: "Spanner sessions and transactions", Service: "spanner",
		Methods: []string{"Spanner/CreateSession", "Spanner/BeginTransaction"}, Excluded: transactionReason},
	{Resource: "Spanner DML", Service: "spanner",
		Methods: []string{"Spanner/Commit"}, Issue: 798},

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
		Screen: "/requests", Offers: "Request Log: the recorder's request events, filtered by service, code and project, live (fault events: #800)",
		Tests: []string{"TestTheRequestLogShowsServedCalls"}},
	{Resource: "Fault injection",
		Routes: []string{"POST /admin/faults", "GET /admin/faults", "DELETE /admin/faults"}, Issue: 800},
	{Resource: "State save and load, reset, seed",
		Commands: []string{"state", "reset", "seed"},
		Routes:   []string{"POST /admin/state/export", "POST /admin/state/import", "POST /admin/reset", "POST /admin/seed"}, Issue: 801},
	{Resource: "Connect, About and diagnose",
		Commands: []string{"env", "gcloud-setup", "terraform", "version", "diagnose"}, Issue: 802},
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
