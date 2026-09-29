# shellcheck shell=bash
# The variables it sets are read by whoever sources it.
# shellcheck disable=SC2034
# compat-shards.sh: what each shard of ci.yml's compat job starts and runs,
# written down once and sourced by that job and by scripts/verify-local.sh,
# the same flow on a developer's machine (#705). test/repo fails when the
# job's matrix and COMPAT_SHARDS differ, or when the job sets these lists
# itself. Bash 3.2 compatible: macOS runs it.
#
# The compatibility suite runs as four shards, each an instance with only
# its services and only their CLOUDBURROW_TEST_* variables exported, so the
# tests for the other shards skip by the gating a developer's partial
# instance gets, not by a maintained list of names (#565). The membership
# follows the tests:
#   storage:   storage, pubsub, and every service the reset, seed,
#              Terraform-wrapper and scheduler tests reach in the same test
#              (tasks, secretmanager, scheduler), plus the Python suite, the
#              hooks and seed fixtures and the logs check, and the Secret
#              Manager, Cloud Tasks, Cloud Scheduler and in-cluster Storage
#              restart probes;
#   served:    the remaining CloudBurrow-served APIs (kms, logging, and
#              Resource Manager, which every instance serves) and the KMS
#              restart probe;
#   run:       Cloud Run with its Knative install and the prediction
#              fixtures, whose startup-failure test is minutes on its own;
#              secretmanager too, for the two tests that read a secret from
#              a revision, so its tests run here and in storage;
#   emulators: the upstream emulators and their restart probes.
# A fifth, acceptance, runs no compat test: storage, pubsub and run, for
# the acceptance workflow (test/e2e) and test/k8s (#596).

# The shards, in the order ci.yml's matrix lists them.
COMPAT_SHARDS="storage served run emulators acceptance"

# The console's headless-browser suite (#594) runs in the storage, run and
# emulators shards: the Cloud KMS and Cloud Run screens are served only by
# the run shard's instance (#700), and the BigQuery, Firestore, Datastore
# (#854) and Cloud SQL screens, MySQL (#868) and PostgreSQL (#995), only by
# the emulators shard's, so the tests these name run there and the rest in
# storage.
COMPAT_BROWSER_SHARDS="storage run emulators"
COMPAT_BROWSER_RUN_SHARD_TESTS='^Test(KMS|CloudRun)'
COMPAT_BROWSER_EMULATORS_SHARD_TESTS='^Test(BigQuery|Firestore|Datastore|CloudSQL)'

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
      SERVICES=storage,pubsub,tasks,secretmanager,scheduler
      # The hooks are the #285 fixtures (two ready.d scripts, the second
      # creating a bucket, and a shutdown.d script checked after stop), and
      # the seed file names tasks and storage.
      # One origin beyond loopback is allowed, so the in-cluster storage
      # server's allowlist is exercised (#677).
      FIXTURES=(--hooks-dir test/compat/testdata/hooks --hook-env RUNNER_TEMP --seed-file test/compat/testdata/seed.json
        --cors-allow-origin https://app.test:8443)
      # Cloud Scheduler (#302); and OpenTofu, so its wrapper tests fail
      # rather than skip where it is installed (#719).
      TEST_VARS+=,STORAGE,CORS_ORIGIN,PUBSUB,TASKS,SECRETS,SCHEDULER,TOFU
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
      # Cloud KMS (#309), Cloud Logging (#304), and Resource Manager v3
      # (#298): every instance serves it; its tests run here.
      TEST_VARS+=,KMS,LOGGING,RESOURCEMANAGER
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
      # refused by the front in its pod (#874, #902).
      SERVICES=run,secretmanager,tasks,storage,pubsub,kms,scheduler,logging,bigquery
      # Cloud Run (#336). For the revisions that reach Storage and Pub/Sub
      # with no client options (#576) and call KMS, Scheduler and Logging
      # (#681), the RUN_* names, so those suites do not run a second time
      # here. BigQuery for the revision that is refused by the validating
      # front at its injected CLOUDBURROW_BIGQUERY_ENDPOINT (#874, #902), as
      # RUN_BIGQUERY, with the one project its emulator serves.
      TEST_VARS+=,RUN,SECRETS,TASKS,RUN_STORAGE,RUN_PUBSUB,RUN_KMS,RUN_SCHEDULER,RUN_LOGGING,RUN_BIGQUERY,BIGQUERY_PROJECT ;;
    acceptance)
      # The acceptance workflow uploads, publishes and runs a Cloud Run
      # worker; test/k8s needs the cluster and Knative.
      SERVICES=storage,pubsub,run ;;
    emulators)
      # pubsub for TestEmulatorsBehindAFrontRefuseOtherPods (#1114), which
      # dials both emulators from a pod; PUBSUB is not exported, so the
      # Pub/Sub tests stay in the storage shard.
      SERVICES=spanner,datastore,firestore,bigtable,bigquery,memorystore,cloudsql,cloudsql-mysql,storage,pubsub
      # Datastore (#307), Firestore and Bigtable (#346), Memorystore
      # (#296), Cloud SQL for MySQL with its generated password (#297),
      # Cloud SQL for PostgreSQL (#311, #584), and BigQuery with the one
      # project its emulator serves (#277); the pod that dials its Service
      # on an instance without Cloud Run (#902) uses the cluster. Cloud
      # Storage runs for BigQuery's loads from gs:// URIs (#919), whose
      # test finds its endpoint with `cloudburrow env`; STORAGE is not
      # exported, so the Cloud Storage tests stay in the storage shard.
      TEST_VARS+=,SPANNER,DATASTORE,FIRESTORE,BIGTABLE,MEMORYSTORE,MYSQL,MYSQL_PASSWORD,CLOUDSQL,BIGQUERY,BIGQUERY_STORAGE,BIGQUERY_PROJECT
      # Cloud SQL for PostgreSQL (#701): a table with a row.
      SETUP_TESTS="TestCloudSQLRestartSetup"
      PROBE_TESTS="TestMemorystoreAcrossRestart TestCloudSQLMySQLAcrossRestart TestCloudSQLAcrossRestart TestDatastoreAcrossRestart"
      PROBE_VARS=MEMORYSTORE,MYSQL,MYSQL_PASSWORD,CLOUDSQL,DATASTORE ;;
    *) echo "unknown shard $1" >&2; return 1 ;;
  esac
  # The pinned gcloud, in the shards whose services its tests use (#694);
  # bq's (#696) run in the emulators shard, with BigQuery.
  case "$1" in
    storage|served|run|emulators) TEST_VARS+=,GCLOUD ;;
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
    emulators) export CLOUDBURROW_TEST_CLOUDSQL_SETUP=1 ;;
  esac
}

# compat_probe_expect <present|absent> <dir>: NAME=value lines, what each
# restart probe expects to find after the restart, and the storage probe's
# fixture in <dir>.
compat_probe_expect() {
  local v
  for v in MEMORYSTORE MYSQL CLOUDSQL DATASTORE KMS SECRETS TASKS SCHEDULER; do
    echo "CLOUDBURROW_TEST_${v}_EXPECT=$1"
  done
  echo "CLOUDBURROW_TEST_STORAGE_RESTART_PROBE=$1:$2/storage-restart-probe.json"
}
