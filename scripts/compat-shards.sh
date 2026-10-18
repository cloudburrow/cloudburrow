# shellcheck shell=bash
# The variables it sets are read by whoever sources it.
# shellcheck disable=SC2034
# compat-shards.sh: what each shard of ci.yml's compat job starts and runs,
# written down once and sourced by that job and by scripts/verify-local.sh,
# the same flow on a developer's machine (#705). test/repo fails when the
# job's matrix and COMPAT_SHARDS differ, or when the job sets these lists
# itself. Bash 3.2 compatible: macOS runs it.
#
# The compatibility suite runs as five shards, each an instance with only
# its services and only their CLOUDBURROW_TEST_* variables exported, so the
# tests for the other shards skip by the gating a developer's partial
# instance gets, not by a maintained list of names (#565). The membership
# follows the tests:
#   storage:   storage, pubsub, and every service the reset, seed,
#              Terraform-wrapper and scheduler tests reach in the same test
#              (tasks, secretmanager, scheduler), and logging for the
#              Cloud Tasks dispatch log (#1256, as TASKS_LOGGING, so the
#              Logging suite stays in served), plus the Python suite, the
#              hooks and seed fixtures and the logs check, and the Secret
#              Manager, Cloud Tasks, Cloud Scheduler and in-cluster Storage
#              restart probes, and Resource Manager, which every instance
#              serves, for TestOneProjectNumberEverywhere, which compares
#              its project number with Cloud Storage's service agent;
#   served:    the remaining CloudBurrow-served APIs (kms, logging) and the
#              KMS restart probe, with Resource Manager as
#              LOGGING_RESOURCEMANAGER for the Logging folder tests;
#   run:       Cloud Run with its Knative install and the prediction
#              fixtures, whose startup-failure test is minutes on its own;
#              secretmanager too, for the two tests that read a secret from
#              a revision, so its tests run here and in storage;
#   emulators: the upstream emulators but Memorystore, and their restart
#              probes;
#   memorystore: Memorystore for Redis, Redis Cluster and Valkey, with
#              Cloud Storage for the exports and imports and Cloud KMS as
#              CMEK_KMS for the customer-managed keys, and its restart
#              probes: about 28 of the 60 minutes go test allows, split from
#              the emulators shard, which outgrew them (#1568, main run
#              39068203270: 3,593 s of tests, half of them Memorystore's);
#   sql:       Cloud SQL for PostgreSQL and MySQL, the Cloud SQL Admin API
#              over them and the Backup and DR Service over their backups,
#              with Cloud Storage for the imports and exports, and the Cloud
#              SQL restart probes: the slowest suites of what was the
#              emulators shard, which passed 45m with them (#1472).
# A seventh, acceptance, runs no compat test: storage, pubsub and run, for
# the acceptance workflow (test/e2e) and test/k8s (#596).

# The shards, in the order ci.yml's matrix lists them.
COMPAT_SHARDS="storage served run emulators memorystore sql acceptance"

# The console's headless-browser suite (#594) runs in the storage, run,
# emulators, memorystore and sql shards: the Cloud KMS and Cloud Run
# screens are served only by the run shard's instance (#700), as are Cloud
# Logging's admin screens (TestLoggingAdmin*, #1226); the BigQuery,
# Firestore, Datastore (#854) and Spanner screens and the Bigtable screen's
# actions (#1198) only by the emulators shard's; the Memorystore screens
# only by the memorystore shard's (#1568); and the Cloud SQL screens, MySQL
# (#868) and PostgreSQL (#995), only by the sql shard's (#1472). The tests
# these name run there and the rest in storage.
# The accessibility audit (#1604) follows the same split: its Cloud Storage,
# Pub/Sub and Logs Explorer pages run in storage, and
# TestBigQueryScreensAreNamedAndLegible in emulators; the rest of the
# console's screens are TestCloudRunAndCICDScreensMeetWCAG (run),
# TestCloudSQLAndBackupDRScreensMeetWCAG (sql),
# TestSpannerAndTheEmulatorScreensMeetWCAG (emulators),
# TestMemorystoreScreensMeetWCAG (memorystore) and
# TestSecretsSchedulerTasksProjectsIAMAndAIScreensMeetWCAG (storage, which
# serves Secret Manager, Cloud Scheduler and Cloud Tasks).
COMPAT_BROWSER_SHARDS="storage run emulators memorystore sql"
COMPAT_BROWSER_RUN_SHARD_TESTS='^Test(KMS|CloudRun|LoggingAdmin)'
COMPAT_BROWSER_EMULATORS_SHARD_TESTS='^Test(BigQuery|Firestore|Datastore|Bigtable|Spanner)'
COMPAT_BROWSER_MEMORYSTORE_SHARD_TESTS='^TestMemorystore'
COMPAT_BROWSER_SQL_SHARD_TESTS='^Test(CloudSQL|BackupDR)'
# Browser tests no shard runs: the Studio's local models (#1396) need a
# -tags onnx build started with -local-ai-embeddings, which no shard builds;
# they run locally, as the compat suite's local AI tests do (ci.yml never=).
# Functions' Edit, Download and Permissions builds a function from source with
# Google's builder, as the compat suite's Functions tests do, so it runs only
# when asked for (CLOUDBURROW_TEST_FUNCTIONS) on amd64 nodes.
COMPAT_BROWSER_NEVER_TESTS='^Test(StudioManagesLocalModelsAndTestsEmbeddings|CloudRunFunctionsEditDownloadAndPermissionsThroughThePage)$'

# compat_shard <shard>: sets, for that shard,
#   SERVICES     the instance's --services
#   FIXTURES     an array of the extra `up` flags only its tests read
#   TEST_VARS    the scripts/compat-env.sh --only list
#   SETUP_TESTS  the restart probes' setup tests, space-separated
#   PROBE_TESTS  the restart probes, run after a stop and an up
#   PROBE_VARS   the endpoints the probes read from the restarted instance
# and returns 1 for a shard it does not know. The hooks write to the
# directory named by RUNNER_TEMP, which verify-local.sh sets to its own.
compat_shard() {
  FIXTURES=()
  SETUP_TESTS="" PROBE_TESTS="" PROBE_VARS=""
  # What every instance has, whatever its services: the ADC fixture
  # (#346), the instance's own kubeconfig and kind cluster (#346, never
  # the default kubeconfig), the admin API, the metadata server (#303),
  # the console, the admin token (#553), and the CLI and the flags naming
  # this instance, for tests that drive a cloudburrow command, such as
  # `cloudburrow terraform`. A service adds its variable to its shard and
  # nothing else; the tests for the other shards see no variable and
  # skip, the way they do against a developer's instance without it.
  TEST_VARS=CREDENTIALS,KUBECONFIG,CLUSTER,CONTROL,METADATA,CONSOLE,ADMIN_TOKEN,CLI,CLI_ARGS
  case "$1" in
    storage)
      SERVICES=storage,pubsub,tasks,secretmanager,scheduler,logging
      # The hooks are the #285 fixtures (two ready.d scripts, the second
      # creating a bucket, and a shutdown.d script checked after stop), and
      # the seed file names tasks and storage.
      # One origin beyond loopback is allowed, so the in-cluster storage
      # server's allowlist is exercised (#677).
      FIXTURES=(--hooks-dir test/compat/testdata/hooks --hook-env RUNNER_TEMP --seed-file test/compat/testdata/seed.json
        --cors-allow-origin https://app.test:8443)
      # Cloud Scheduler (#302); and OpenTofu, so its wrapper tests fail
      # rather than skip where it is installed (#719).
      # Resource Manager v3 and v1 (#298), which every instance serves:
      # TestOneProjectNumberEverywhere (#1211) needs it and Cloud Storage in
      # one instance, so its tests run here.
      # Cloud Logging as TASKS_LOGGING (#1256): only the Cloud Tasks
      # dispatch log test reads it, so the Logging tests run in served.
      # Service Usage (#1352), always served, with the Terraform and gcloud
      # tests that need Cloud Storage beside it.
      TEST_VARS+=,STORAGE,CORS_ORIGIN,PUBSUB,TASKS,SECRETS,SCHEDULER,TOFU,RESOURCEMANAGER,SERVICEUSAGE,TASKS_LOGGING
      # The restart probes' fixtures, left just before stop so no
      # suite-level reset wipes them: Secret Manager (#483), a secret and a
      # version; Cloud Tasks and Cloud Scheduler (#596), a paused queue with
      # a task, and an enabled and a paused job; Cloud Storage (#596),
      # TestStorageAcrossRestart's setup against the in-cluster server
      # through the instance's tunnel.
      SETUP_TESTS="TestSecretManagerRestartSetup TestTasksRestartSetup TestSchedulerRestartSetup TestStorageAcrossRestart"
      PROBE_TESTS="TestSecretManagerAcrossRestart TestTasksAcrossRestart TestSchedulerAcrossRestart TestStorageAcrossRestart"
      PROBE_VARS=SECRETS,TASKS,SCHEDULER,STORAGE ;;
    served)
      SERVICES=kms,logging
      # Cloud KMS (#309) and Cloud Logging (#304). Resource Manager's tests
      # run in the storage shard; the Logging folder and organization tests
      # (#1395) make their folder with it, which every instance serves, as
      # LOGGING_RESOURCEMANAGER, so they run here and nowhere else (CI run
      # 38910752877: in storage gcloud had no logging override).
      # Cloud KMS Autokey's folder and project tests (#1568) make their
      # folder with it too.
      TEST_VARS+=,KMS,LOGGING,LOGGING_RESOURCEMANAGER
      # Cloud KMS (#418): a key with ENABLED, DISABLED and
      # DESTROY_SCHEDULED versions and a ciphertext.
      SETUP_TESTS="TestKMSRestartSetup"
      PROBE_TESTS="TestKMSAcrossRestart"
      PROBE_VARS=KMS ;;
    run)
      # tasks too: TestAPodReachesTheCLIHostedServices (#575) needs Secret
      # Manager and Tasks served to pods, which only Run's cluster has.
      # Storage and Pub/Sub for
      # TestCloudRunRevisionReachesStorageAndPubSubWithNoClientOptions
      # (#576): a revision uses them through the injected variables. KMS,
      # Scheduler and Logging for the revision that calls them at the
      # injected CLOUDBURROW_*_ENDPOINT (#681). BigQuery for the revisions
      # refused by the front in its pod (#874, #902). Cloud SQL for the
      # revision and task that query PostgreSQL, and Cloud SQL for MySQL for
      # the revision that queries MySQL, through a Cloud SQL volume (#1346).
      SERVICES=run,secretmanager,tasks,storage,pubsub,kms,scheduler,logging,bigquery,cloudsql,cloudsql-mysql
      # Cloud Run (#336). For the revisions that reach Storage and Pub/Sub
      # with no client options (#576) and call KMS, Scheduler and Logging
      # (#681), the RUN_* names, so those suites do not run a second time
      # here. BigQuery for the revision that is refused by the validating
      # front at its injected CLOUDBURROW_BIGQUERY_ENDPOINT (#874, #902), as
      # RUN_BIGQUERY, with the one project its emulator serves.
      # PREDICTION: the local AI endpoint, which Cloud Run starts, serving
      # PredictionService for the custom prediction containers (#1193).
      # RUN_SQLADMIN: the Cloud SQL volume tests (#1346), without the
      # Cloud SQL suite, which the sql shard runs; RUN_MYSQL and
      # MYSQL_PASSWORD for the MySQL one, which logs in as the cloudburrow
      # user (#1568).
      # --cloudbuild-host-steps (#1382): Cloud Build's arbitrary steps run
      # only with it (ADR-0004), so TestCloudBuildArbitraryStepsFilterAndRetry
      # and TestGcloudBuildsStepsAndList run here; the refusal without it is
      # TestCloudBuildHostStepsOff, which skips on this instance.
      # --appengine-url (#1506): Cloud Tasks and Cloud Scheduler deliver App
      # Engine targets to the stand-in app TestTasksAppEngineDelivery and
      # TestSchedulerAppEngineDelivery (with RUN_SCHEDULER) serve there, as
      # APPENGINE_URL; the 404s without it are the storage shard's (#1568).
      # The tests that need Cloud KMS, Cloud Storage or Pub/Sub beside
      # Secret Manager or Cloud Run read RUN_KMS, RUN_STORAGE and RUN_PUBSUB
      # (#1568): the customer-managed and regional secrets, Terraform's data
      # sources, Cloud Storage volumes and Pub/Sub build triggers.
      FIXTURES=(--cloudbuild-host-steps --appengine-url http://127.0.0.1:18506)
      TEST_VARS+=,RUN,SECRETS,TASKS,RUN_STORAGE,RUN_PUBSUB,RUN_KMS,RUN_SCHEDULER,RUN_LOGGING,RUN_BIGQUERY,RUN_SQLADMIN,RUN_RESOURCEMANAGER,RUN_SERVICEUSAGE,RUN_MYSQL,MYSQL_PASSWORD,BIGQUERY_PROJECT,PREDICTION,APPENGINE_URL ;;
    acceptance)
      # The acceptance workflow uploads, publishes and runs a Cloud Run
      # worker; test/k8s needs the cluster and Knative.
      SERVICES=storage,pubsub,run ;;
    emulators)
      # pubsub for TestEmulatorsBehindAFrontRefuseOtherPods (#1114), which
      # dials both emulators from a pod; PUBSUB is not exported, so the
      # Pub/Sub tests stay in the storage shard.
      SERVICES=spanner,datastore,firestore,bigtable,bigquery,storage,pubsub,logging,kms
      # logging for the sinks that deliver to Cloud Storage, Pub/Sub and
      # BigQuery (#1404), as SINK_LOGGING, SINK_STORAGE and SINK_PUBSUB, so
      # those suites stay in their shards; the copies to a Cloud Storage
      # bucket (#1394) run here under the same names. Cloud Storage again as
      # FIRESTORE_STORAGE, for Firestore's exports and imports (#1500).
      # Spanner with its REST API for Terraform (#1331), Datastore (#307),
      # Firestore and Bigtable (#346), and BigQuery with the one
      # project its emulator serves (#277); the pod that dials its Service
      # on an instance without Cloud Run (#902) uses the cluster. Cloud
      # Storage runs for BigQuery's loads from gs:// URIs (#919), whose
      # test finds its endpoint with `cloudburrow env`; STORAGE is not
      # exported, so the Cloud Storage tests stay in the storage shard.
      # Cloud SQL and Backup and DR run in the sql shard (#1472), and
      # Memorystore in the memorystore shard (#1568).
      # Resource Manager as BIGQUERY_RESOURCEMANAGER (#1392): only
      # BigQuery's JOBS_BY_FOLDER test and the purged project's data test
      # (#1568) read it, so the Resource Manager tests stay in storage.
      # The Logging tests that need BigQuery (linked datasets, Log Analytics)
      # or Cloud Storage (the copies) read SINK_LOGGING and SINK_STORAGE, and
      # the copies from a folder LOGGING_RESOURCEMANAGER (#1568).
      TEST_VARS+=,LOGGING_RESOURCEMANAGER,SPANNER,SPANNER_REST,DATASTORE,FIRESTORE,BIGTABLE,BIGQUERY,BIGQUERY_STORAGE,BIGQUERY_PROJECT,BIGQUERY_RESOURCEMANAGER,SINK_LOGGING,SINK_STORAGE,SINK_PUBSUB,FIRESTORE_STORAGE
      PROBE_TESTS="TestDatastoreAcrossRestart TestFirestoreAcrossRestart"
      PROBE_VARS=DATASTORE,FIRESTORE ;;
    memorystore)
      # Memorystore for Redis (#296), Redis Cluster and Valkey, split from
      # the emulators shard (#1568). Cloud Storage for the exports and
      # imports, whose tests find it with `cloudburrow env`; STORAGE is not
      # exported, so the Cloud Storage tests stay in the storage shard.
      # Cloud KMS as CMEK_KMS, for the customer-managed keys, so the KMS
      # suite stays in served.
      SERVICES=memorystore,storage,kms
      TEST_VARS+=,MEMORYSTORE,MEMORYSTORE_ADMIN,MEMORYSTORE_PROJECT,CMEK_KMS
      # A created Memorystore instance follows the mode too (#1302), with RDB
      # persistence as well (#1384), and Redis clusters and Valkey instances
      # with and without persistence_config (#1557).
      PROBE_TESTS="TestMemorystoreAcrossRestart TestMemorystoreCreatedAcrossRestart TestMemorystoreRDBAcrossRestart TestMemorystoreClustersAcrossRestart"
      PROBE_VARS=MEMORYSTORE,MEMORYSTORE_ADMIN,MEMORYSTORE_PROJECT ;;
    sql)
      # Cloud SQL for PostgreSQL (#311, #584) and for MySQL with its
      # generated password (#297), the Cloud SQL Admin API over both
      # servers (#1301), and the Backup and DR Service over their backups
      # (#1433), split from the emulators shard (#1472). Cloud Storage for
      # the imports, exports and Auth Proxy test's buckets, which the tests
      # find with `cloudburrow env`; STORAGE is not exported, so the Cloud
      # Storage tests stay in the storage shard. The connector listens at
      # its default 3307 (no --cloudsql-connector), the one port Google's
      # Auth Proxy dials, so TestCloudSQLAuthProxyBinary runs here with the
      # binary ci.yml installs from `make cloud-sql-proxy` (#1421).
      # Secret Manager for the console's Run SQL (Data API), whose password
      # is a secret version (TestCloudSQLInstanceActionsThroughThePage);
      # SECRETS is not in TEST_VARS, so the Secret Manager tests stay in
      # the storage and run shards.
      # Cloud KMS as CMEK_KMS, for a backup vault's customer-managed key;
      # Secret Manager as CLOUDSQL_SECRETS, for the Data API's password; and
      # Cloud Storage as NOPUBSUB_STORAGE, on an instance that serves no
      # Pub/Sub, for the notifications' refusal (#1568). Resource Manager,
      # which every instance serves, as SQL_RESOURCEMANAGER, for the purged
      # project's Cloud SQL and Backup and DR and for Backup and DR's
      # folder fetch (#1568); CMEK_KMS for Cloud SQL's customer-managed
      # keys too.
      SERVICES=cloudsql,cloudsql-mysql,backupdr,storage,secretmanager,kms
      TEST_VARS+=,MYSQL,MYSQL_PASSWORD,CLOUDSQL,SQLADMIN,BACKUPDR,CMEK_KMS,CLOUDSQL_SECRETS,NOPUBSUB_STORAGE,SQL_RESOURCEMANAGER
      # Cloud SQL for PostgreSQL (#701): a table with a row.
      SETUP_TESTS="TestCloudSQLRestartSetup"
      PROBE_TESTS="TestCloudSQLMySQLAcrossRestart TestCloudSQLAcrossRestart"
      PROBE_VARS=MYSQL,MYSQL_PASSWORD,CLOUDSQL ;;
    *) echo "unknown shard $1" >&2; return 1 ;;
  esac
  # The pinned gcloud, in the shards whose services its tests use (#694);
  # bq's (#696) run in the emulators shard, with BigQuery, and gcloud sql's
  # in the sql shard.
  case "$1" in
    storage|served|run|emulators|memorystore|sql) TEST_VARS+=,GCLOUD ;;
  esac
}

# compat_setup_exports <shard> <dir>: exports what the shard's restart
# probe setup tests read, their fixture files in <dir>. They stay exported
# for the probes after the restart.
compat_setup_exports() {
  case "$1" in
    served)  export CLOUDBURROW_TEST_KMS_PROBE="$2/kms-probe.json" ;;
    storage) export CLOUDBURROW_TEST_SECRETS_PROBE="$2/secrets-probe.json" \
               CLOUDBURROW_TEST_TASKS_PROBE="$2/tasks-probe.json" \
               CLOUDBURROW_TEST_SCHEDULER_PROBE="$2/scheduler-probe.json" \
               CLOUDBURROW_TEST_STORAGE_RESTART_PROBE="setup:$2/storage-restart-probe.json" ;;
    sql) export CLOUDBURROW_TEST_CLOUDSQL_SETUP=1 ;;
  esac
}

# compat_probe_expect <present|absent> <dir>: NAME=value lines, what each
# restart probe expects to find after the restart, and the storage probe's
# fixture in <dir>.
compat_probe_expect() {
  local v
  for v in MEMORYSTORE MYSQL CLOUDSQL DATASTORE FIRESTORE KMS SECRETS TASKS SCHEDULER; do
    echo "CLOUDBURROW_TEST_${v}_EXPECT=$1"
  done
  echo "CLOUDBURROW_TEST_STORAGE_RESTART_PROBE=$1:$2/storage-restart-probe.json"
}
