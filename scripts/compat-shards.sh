# shellcheck shell=bash
# The variables it sets are read by whoever sources it.
# shellcheck disable=SC2034
# compat-shards.sh: what each shard of ci.yml's compat job starts and runs,
# written down once and sourced by that job and by scripts/verify-local.sh,
# the same flow on a developer's machine (#705). test/repo fails when the
# job's matrix and COMPAT_SHARDS differ, or when the job sets these lists
# itself. Bash 3.2 compatible: macOS runs it.
#
# The compatibility suite runs as seven shards, each an instance with only
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
#   served:    the remaining CloudBurrow-served APIs (kms, logging,
#              privateca, gke) and the
#              KMS restart probe, with Resource Manager as
#              LOGGING_RESOURCEMANAGER for the Logging folder tests and
#              Service Usage as GKE_SERVICEUSAGE for gcloud container; the
#              Kubernetes Engine API's tests make kind clusters beside the
#              instance's (about 5 minutes on a laptop, and 20 GiB free,
#              which ci.yml makes room for), and its console test runs in
#              this shard's browser step;
#   run:       Cloud Run with its Knative install; secretmanager too, for
#              the two tests that read a secret from a revision, so its
#              tests run here and in storage;
#   scale:     the tests that take minutes each, split from run, which
#              outgrew 60 minutes when its CPU-scaling tests began to pass
#              (#1821, main run 44377266506: 3,766 s of tests): Cloud Run's
#              autoscaling and manual-scaling tests (1,240 s), the
#              prediction containers' (292 s, 244 of them the startup
#              failure) and Secret Manager's 2,000 versions (286 s here,
#              422 s in storage, which ran it too). They read Cloud Run and
#              Secret Manager as SCALE_RUN and SCALE_SECRETS, which no other
#              shard exports, and this shard exports neither RUN nor
#              SECRETS, so each runs once: about 31 minutes here, and about
#              32 left in run and 26 in storage;
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
# An eighth, acceptance, runs no compat test: storage, pubsub and run, for
# the acceptance workflow (test/e2e) and test/k8s (#596).
# An eighth, iam, starts its instance with `up --iam-enforcement=on`
# (UP_FLAGS) and runs only the enforcement tests (COMPAT_RUN,
# ^TestIAMEnforcement), which skip on every other shard's instance, as
# those run with enforcement off (#1879): Cloud Storage, Pub/Sub, Cloud
# Tasks, Secret Manager, Cloud KMS, Cloud Run (for run.routes.invoke,
# iam.serviceAccounts.actAs and, since the sixth round, its admin API and
# Cloud Functions, Artifact Registry and Cloud Build on its port), Cloud
# Scheduler, Workflows, Cloud Logging, Cloud Monitoring, Memorystore, the
# Cloud SQL Admin API, Certificate Authority Service, Kubernetes Engine,
# Compute Engine, API Keys, Service Usage and BigQuery (for Pub/Sub's
# BigQuery subscriptions, #1998).

# An eleventh, crossservice (cross alone was the cross-compile job's
# name in ci.yml), is the run shard's instance again for
# COMPAT_CROSSSERVICE_TESTS, which the run shard skips: Eventarc's tests and
# Artifact Registry's cleanup policies, split from run when it reached its
# 60m (main run 44740769561), and the tests that need services no other
# shard exports together (Cloud Storage with BigQuery, Cloud KMS or Cloud
# Logging; Pub/Sub or Firestore with BigQuery and Service Usage).

# A tenth, sqlreplicas, is the sql shard's instance again for the Cloud SQL
# tests that make replicas, standbys and external primaries
# (COMPAT_SQL_REPLICA_TESTS), which the sql shard skips (COMPAT_SKIP): the
# sql shard took 3,185 s of its 60m in main run 44617241748 and passed 60m
# in 44645694075 (no test hung; TestTerraformSQLProvisionScript had run
# 16 s), and these 28 tests are 1,666 s of its 3,260.

# An eleventh, localai, is an instance of a -tags onnx build (make
# build-onnx, run with ONNXRUNTIME_LIB) started with the pinned Gemma 4 E2B
# generation model (--local-ai-model, the LiteRT-LM runtime image `make
# litert-lm` builds) and --local-ai-embeddings (EmbeddingGemma's community
# ONNX conversion), for the compat tests that need the local generation or
# embedding runtime (#1985), which skip on every other shard's instance:
# Vertex AI's generation and embeddings, BigQuery's AI and ML functions over
# them, ML.ANNOTATE_IMAGE's MediaPipe models, Document AI's Tesseract and
# Speech-to-Text's whisper.cpp, which the same endpoint serves, and BigQuery
# ML's imported ONNX models, which only an onnx build runs. COMPAT_RUN is
# that list alone, so the BigQuery and Cloud Storage suites, whose endpoints
# these tests read too, stay in their shards. The model's path is
# CLOUDBURROW_LOCALAI_MODEL, which ci.yml fetches by its pin
# (scripts/localai-models.sh) and verify-local.sh requires.
COMPAT_LOCALAI_TEST_NAMES="
  BigQueryAIAgg BigQueryAICountTokens BigQueryAIEmbed BigQueryAIGenerate
  BigQueryAIGenerateStructured BigQueryAIManagedFunctions
  BigQueryAutonomousEmbeddingsAndAISearch
  BigQueryConnectionGroupMembers1812 BigQueryMLAnnotateImage
  BigQueryMLAnnotateImageDetections BigQueryMLAnnotateImageLabels
  BigQueryMLImportedONNXModel BigQueryMLProcessDocument
  BigQueryMLTensorFlowImportGenerated
  BigQueryMLRemoteConnectionPermissions BigQueryMLRemoteDefaultConnection
  BigQueryMLRemoteGenerateEmbedding BigQueryMLRemoteGenerateText
  BigQueryMLRemoteGenerationOptions BigQueryMLTranscribe
  BigQueryMLTranslate BigQueryMLUnderstandText
  BigQueryObjectRefAIFunctions BigQueryObjectRefEmbeddingsAndSearch
  BigQueryVectorSearchAutonomousEmbeddings
  ConsoleEmbeddingsPlaygroundMatchesTheOfficialSDK
  ConsoleLocalModelsLoadEmbedUnload
  ConsoleLocalModelsUnloadDeleteDownloadLoad
  CountTokensAndComputeTokensThroughTheOfficialSDK DocumentAIBatchProcess
  DocumentAIBatchShardsAndFieldMask DocumentAIEvaluationsAndReview
  DocumentAIProcessorVersions DocumentAIProcessors
  TerraformDocumentAIProcessorDefaultVersion
  EmbedContentEmbeds16KVideoThroughThePredictionClient
  EmbedContentEmbeds2And5BitADPCMAndMP3WAVsThroughThePredictionClient
  EmbedContentEmbedsADPCMWAVAndLosslessAndMultichannelTracksThroughThePredictionClient
  EmbedContentEmbedsAFullParametricStereoTrackThroughThePredictionClient
  EmbedContentEmbedsAVIFThroughThePredictionClient
  EmbedContentEmbedsAVideosAudioTrackThroughThePredictionClient
  EmbedContentEmbedsArithmeticAnd12BitJPEGThroughThePredictionClient
  EmbedContentEmbedsAudioThroughThePredictionClient
  EmbedContentEmbedsG726WAVThroughThePredictionClient
  EmbedContentEmbedsGSM610WAVThroughThePredictionClient
  EmbedContentEmbedsHEICAndAnimatedWebPThroughThePredictionClient
  EmbedContentEmbedsImagesThroughThePredictionClient
  EmbedContentEmbedsMP3ThroughThePredictionClient
  EmbedContentEmbedsMPEG25MP3ThroughThePredictionClient
  EmbedContentEmbedsMPEGLayerIAndJointStereoWAVThroughThePredictionClient
  EmbedContentEmbedsMPEGLayerIIWAVThroughThePredictionClient
  EmbedContentEmbedsMultichannelWAVAndGrayVideoThroughThePredictionClient
  EmbedContentEmbedsSurroundAACAndG711TracksAnd3BitADPCMThroughThePredictionClient
  EmbedContentEmbedsVP9AndAV1ThroughThePredictionClient
  EmbedContentEmbedsVideoAndPDFThroughThePredictionClient
  EmbedContentEmbedsWebPAndBMPThroughThePredictionClient
  EmbedContentThroughThePredictionClient
  EmbeddingGemmaThroughTheOfficialSDK
  EmbeddingGemmaThroughThePredictionClient
  GenerateContentFunctionCallingRoundTripThroughTheOfficialSDK
  GenerateContentImagePartThroughTheOfficialSDK
  GenerateContentMultiTurnChatThroughTheOfficialSDK
  GenerateContentThroughTheOfficialSDK
  GenerationNeverUsesApplicationDefaultCredentials
  RawPredictOnTheEmbeddingModel RawPredictOnTheGenerationModel
  SpeechAdaptationBoost SpeechAdaptationResources SpeechBatchRecognize
  SpeechRecognitionFeatures1799 SpeechRecognizers SpeechStreamingRecognize SpeechV1
  StreamGenerateContentThroughTheOfficialSDK
  StreamGenerateContentThroughThePredictionClient
  StreamingPredictOnTheEmbeddingModel StreamingPredictOnTheGenerationModel
  V1Beta1PredictionClientServesWhatV1Serves"
COMPAT_LOCALAI_TESTS="^Test($(echo "$COMPAT_LOCALAI_TEST_NAMES" | tr -s ' \n' '||' | sed 's/^|//; s/|$//'))\$"

# The shards, in the order ci.yml's matrix lists them.
COMPAT_SHARDS="storage served run crossservice scale emulators memorystore sql sqlreplicas acceptance iam localai"

# The crossservice shard's tests (COMPAT_CROSSSERVICE_TESTS), which the run shard skips:
# Eventarc's, whose triggers route events from Cloud Storage, Pub/Sub,
# Firestore, Datastore, Memorystore and Cloud Audit Logs to Cloud Run, GKE
# and Workflows (about 620 s), and Artifact Registry's cleanup policies
# (240 s, CLOUDBURROW_ARTIFACT_REGISTRY_CLEANUP_INTERVAL): the run shard
# reached its 60m in main run 44740769561 with Landing 160's tests, and
# TestWorkflowsCustomerManagedKey, three Workflows tests and the restart
# probes after it ran in no shard. And the tests that need services no
# other shard serves together, by name: Storage Insights' dataset
# configurations and activity views (Cloud Storage with BigQuery), Storage
# Batch Operations' rewrite to a Cloud KMS key, Cloud Storage's Data Access
# audit logs (Cloud Logging), Pub/Sub's BigQuery subscriptions and gcloud's
# message storage policy (Pub/Sub with BigQuery and Service Usage), and
# BigQuery and Firestore refused while disabled (Service Usage); Cloud
# KMS Inventory's protected resources (#2053: a CMEK bucket and a BigQuery
# dataset beside the key, through the client and gcloud), and Firestore's
# and Cloud Tasks' metrics in Cloud Monitoring (#2050, Firestore or Cloud
# Tasks with Monitoring), and Cloud Asset's analyzeIamPolicyLongrunning to
# BigQuery (#2112, Resource Manager with BigQuery), which no other shard
# serves together.
COMPAT_CROSSSERVICE_TEST_NAMES="ArtifactRegistryCleanupPolicies
  CloudAssetAnalyzeIamPolicyLongrunningBigQuery
  FirestoreDocumentMetrics FirestoreStreamMetrics GcloudKMSInventory
  KMSInventoryCountsCMEKResources TasksQueueMetricsInCloudMonitoring
  GcloudPubSubTopicStoragePolicyUpdate GcloudStorageInsightsDatasetConfigs
  PubSubBigQuerySubscriptionPermissionDenied ServiceDisabledBigQuery
  ServiceDisabledFirestore StorageBatchOperationsRewriteToKMSKey
  StorageDataAccessAuditLogs StorageDataAccessAuditLogsOffByDefault
  StorageInsightsActivityViews StorageInsightsDatasetConfigChecks
  StorageInsightsLinkedDataset TerraformStorageInsightsDatasetConfig"
COMPAT_CROSSSERVICE_TESTS="^Test((Gcloud|Terraform|GcloudAndTerraform)?Eventarc[A-Za-z0-9_]*|$(echo "$COMPAT_CROSSSERVICE_TEST_NAMES" | tr -s ' \n' '||' | sed 's/^|//; s/|$//'))\$"

# The sqlreplicas shard's tests, by name: every Cloud SQL test of high
# availability, read replicas, blue-green deployments, external primaries,
# switchover, demotion, failover and promotion.
COMPAT_SQL_REPLICA_TESTS='^Test[A-Za-z]*SQL[A-Za-z0-9]*(HighAvailability|Replica|BlueGreen|External|Switchover|Demote|Failover|Promote)'

# The console's headless-browser suite (#594) runs in the storage, run,
# emulators, memorystore and sql shards: the Cloud KMS and Cloud Run
# screens are served only by the run shard's instance (#700), as are Cloud
# Logging's admin screens (TestLoggingAdmin*, #1226), and Secret Manager's
# screens (TestSecretManager*, #1850), whose regional secrets and CMEK need
# its Cloud KMS, and Eventarc's (TestEventarc*, #1877), whose triggers
# deliver to a Cloud Run service, and Document AI's and Speech-to-Text's
# (TestDocumentAI*, TestSpeech*, #1930), served by the local AI
# endpoint Cloud Run starts, and Workflows' (TestWorkflows*, #1986), whose
# instance is the run shard's; the BigQuery,
# Firestore, Datastore (#854) and Spanner screens and the Bigtable screen's
# actions (#1198) only by the emulators shard's; the Memorystore screens
# only by the memorystore shard's (#1568); and the Cloud SQL screens, MySQL
# (#868) and PostgreSQL (#995), only by the sql shard's (#1472), as is
# Certificate Authority Service beside Cloud KMS and Cloud Storage, for its
# Cloud KMS CA keys, CMEK pools and publishing buckets (TestPrivateCAKMS*,
# #1834). The tests these name run there and the rest in storage. The scale
# shard (#1821) runs no browser test: the Cloud Run screens stay in run,
# whose instance serves the services they read back.
# The accessibility audit (#1604) follows the same split: its Cloud Storage,
# Pub/Sub and Logs Explorer pages run in storage, and
# TestBigQueryScreensAreNamedAndLegible and TestBigQueryTransferScreensMeetWCAG
# (#1948) in emulators; the rest of the
# console's screens are TestCloudRunAndCICDScreensMeetWCAG (run),
# TestDocumentAIAndSpeechScreensMeetWCAG and
# TestSpeechAdaptationScreensMeetWCAG (run),
# TestCloudSQLAndBackupDRScreensMeetWCAG (sql),
# TestSpannerAndTheEmulatorScreensMeetWCAG (emulators),
# TestMemorystoreScreensMeetWCAG (memorystore) and
# TestSecretsSchedulerTasksProjectsIAMAndAIScreensMeetWCAG (storage, which
# serves Secret Manager, Cloud Scheduler and Cloud Tasks).
# The served shard's instance serves the Kubernetes Engine API (#1913), so
# the test that makes a cluster through the pages runs there, and
# TestKubernetesEnginePagesThroughTheBrowser, which holds with and without
# the API, runs there as well as in storage. Only the first is kept out of
# storage (COMPAT_BROWSER_SERVED_ONLY_TESTS), where it would skip. The
# served shard serves the Compute Engine API too, so the VPC network and
# Compute Engine pages' tests (TestVPCNetwork*, #1936; TestComputeEngine*,
# #1965, which boots a VM) run there and only there. Agent Platform's Model
# Registry and Endpoints screens are drawn only where Cloud Run is served,
# which starts the local AI endpoint they call (#1940): their test,
# TestModelRegistryAndEndpointsThroughThePages, runs in run, as the scale
# shard runs no browser test.
COMPAT_BROWSER_SHARDS="storage served run emulators memorystore sql localai"
COMPAT_BROWSER_SERVED_SHARD_TESTS='^Test(GKEServed|KubernetesEnginePages|VPCNetwork|ComputeEngine)'
COMPAT_BROWSER_SERVED_ONLY_TESTS='^Test(GKEServed|VPCNetwork|ComputeEngine)'
COMPAT_BROWSER_RUN_SHARD_TESTS='^Test(KMS|CloudRun|LoggingAdmin|SecretManager|Eventarc|DocumentAI|Speech|Workflows|ModelRegistry)'
COMPAT_BROWSER_EMULATORS_SHARD_TESTS='^Test(BigQuery|Firestore|Datastore|Bigtable|Spanner)'
COMPAT_BROWSER_MEMORYSTORE_SHARD_TESTS='^TestMemorystore'
COMPAT_BROWSER_SQL_SHARD_TESTS='^Test(CloudSQL|BackupDR|PrivateCAKMS)'
# The Studio's local models (#1396) and the Embeddings page's image (#1454),
# audio and Cloud Storage objects (#1651) and its scanned PDF read with
# document_ocr (#1733) need a -tags onnx build started with
# -local-ai-embeddings (without it the console has no ai-embed screen and
# the test skips) and EmbeddingGemma 2, which the compat suite's image and
# audio tests download and load first: the localai shard's (#1985). So does
# Speech-to-Text's workspace and playback (#2084), whose transcription needs
# the local models (CLOUDBURROW_TEST_LOCALAI): its TestSpeech name matches
# the run shard's pattern, which skips the localai shard's tests.
COMPAT_BROWSER_LOCALAI_SHARD_TESTS='^Test(StudioManagesLocalModelsAndTestsEmbeddings|EmbeddingsPageEmbedsAnImage|EmbeddingsPageEmbedsAudioAndCloudStorage|EmbeddingsPageReadsAScannedPDFWithOCR|SpeechWorkspaceAndPlaybackThroughThePage)$'
# Browser tests no shard runs: Functions' Edit, Download and Permissions builds a function from source with
# Google's builder, as the compat suite's Functions tests do, so it runs only
# when asked for (CLOUDBURROW_TEST_FUNCTIONS) on amd64 nodes.
COMPAT_BROWSER_NEVER_TESTS='^Test(CloudRunFunctionsEditDownloadAndPermissionsThroughThePage)$'

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
  # UP_FLAGS are `up` flags beyond the services and fixtures; COMPAT_RUN,
  # when set, is the -run pattern of the shard's compat suite (all of it
  # when empty), and COMPAT_SKIP its -skip pattern (none when empty).
  UP_FLAGS=()
  COMPAT_RUN="" COMPAT_SKIP=""
  # COMPAT_TIMEOUT is the suite's go test -timeout.
  COMPAT_TIMEOUT=60m
  # The localai shard's switches, unset for every other shard: verify-local.sh
  # runs the shards one after another in one shell.
  unset CLOUDBURROW_TEST_ONNX CLOUDBURROW_TEST_VISION_LABELS CLOUDBURROW_TEST_VISION_DETECT
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
      # privateca for the console's Certificate Authority Service page
      # (TestPrivateCAThroughThePage, #1834): PRIVATECA is not exported,
      # so its compat suite stays in served.
      # kms for Cloud Storage's customer-managed keys (#1938), exported as
      # CMEK_KMS only, so the KMS suite stays in served. monitoring for the
      # Metrics explorer (TestMonitoringThroughThePage, #1939), whose browser
      # step alone is given MONITORING; and as PUBSUB_MONITORING for
      # TestMonitoringPubSubChannelAndVerification (#1966), which needs
      # Pub/Sub beside it, so the Monitoring suite stays in served.
      # parametermanager (#2054), whose versions reference this shard's
      # Secret Manager and whose keys are its Cloud KMS.
      SERVICES=storage,pubsub,tasks,secretmanager,scheduler,logging,privateca,kms,monitoring,parametermanager
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
      # tests that need Cloud Storage beside it; and the API Keys API
      # (#1915), always served too.
      TEST_VARS+=,STORAGE,CORS_ORIGIN,PUBSUB,TASKS,SECRETS,SCHEDULER,TOFU,RESOURCEMANAGER,SERVICEUSAGE,APIKEYS,STORAGE_BATCH,STORAGE_INSIGHTS,STORAGE_TRANSFER,TASKS_LOGGING,PUBSUB_MONITORING,CMEK_KMS,PARAMETERMANAGER
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
      SERVICES=kms,logging,privateca,gke,monitoring,compute
      # The Kubernetes Engine API (#1913): its clusters are kind clusters
      # beside the instance's. Service Usage as GKE_SERVICEUSAGE, which
      # `gcloud container clusters create` asks whether
      # container.googleapis.com is enabled, so the Service Usage suite
      # stays in storage. Cloud Monitoring (#1939), in process, beside the
      # Cloud Logging its log-based metrics read.
      # The Compute Engine API's VPC networking (#1936), in process,
      # beside Kubernetes Engine, whose cluster networks it checks.
      # Certificate Authority Service (#1834), in process like KMS.
      # Cloud KMS (#309) and Cloud Logging (#304). Resource Manager's tests
      # run in the storage shard; the Logging folder and organization tests
      # (#1395) make their folder with it, which every instance serves, as
      # LOGGING_RESOURCEMANAGER, so they run here and nowhere else (CI run
      # 38910752877: in storage gcloud had no logging override).
      # Cloud KMS Autokey's folder and project tests (#1568) make their
      # folder with it too.
      TEST_VARS+=,KMS,LOGGING,LOGGING_RESOURCEMANAGER,PRIVATECA,GKE,GKE_SERVICEUSAGE,MONITORING,COMPUTE
      # Cloud KMS (#418): a key with ENABLED, DISABLED and
      # DESTROY_SCHEDULED versions and a ciphertext.
      SETUP_TESTS="TestKMSRestartSetup"
      PROBE_TESTS="TestKMSAcrossRestart"
      PROBE_VARS=KMS ;;
    run|crossservice)
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
      # Firestore, as RUN_FIRESTORE, for the Eventarc Firestore triggers (#1877).
      # Datastore, as RUN_DATASTORE, for its Datastore mode triggers, and
      # the Kubernetes Engine API, as RUN_GKE, for a GKE destination (#1877;
      # its test makes one Autopilot kind cluster beside the instance's).
      # Workflows (#1986): its workflows call a Cloud Run service with
      # http.post and Cloud Storage and Pub/Sub through connectors.
      # Memorystore, as RUN_MEMORYSTORE_ADMIN, for the Redis instances'
      # direct events (#1997). Cloud Monitoring, as the browser step's
      # MONITORING only, for Cloud Run's request, latency and instance
      # metrics on a service's and a function's Metrics tab (#2049).
      SERVICES=run,secretmanager,tasks,storage,pubsub,firestore,datastore,gke,memorystore,kms,scheduler,logging,bigquery,cloudsql,cloudsql-mysql,workflows,monitoring
      # Cloud Run (#336). For the revisions that reach Storage and Pub/Sub
      # with no client options (#576) and call KMS, Scheduler and Logging
      # (#681), the RUN_* names, so those suites do not run a second time
      # here. BigQuery for the revision that is refused by the validating
      # front at its injected CLOUDBURROW_BIGQUERY_ENDPOINT (#874, #902), as
      # RUN_BIGQUERY, with the one project its emulator serves.
      # PREDICTION, the local AI endpoint for the custom prediction
      # containers (#1193), is the scale shard's (#1821).
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
      # RUN_APIKEYS: the API Keys API (#1915), for the key a Cloud Build
      # webhook trigger's URL carries.
      FIXTURES=(--cloudbuild-host-steps --appengine-url http://127.0.0.1:18506)
      # Artifact Registry's cleanup policies run every minute rather than
      # Google's "approximately one day" (#1897): `up` reads the variable,
      # and TestArtifactRegistryCleanupPolicies, which skips without one
      # of 2m or less, reads the same, so it runs here.
      export CLOUDBURROW_ARTIFACT_REGISTRY_CLEANUP_INTERVAL=1m
      # RUN_LOCALAI (#1972): the local Vertex AI endpoint Cloud Run starts
      # without a model, for the Document AI and Speech-to-Text
      # customer-managed key tests (with RUN_KMS); LOCALAI, which the
      # generation tests read, is not exported, so they still skip.
      TEST_VARS+=,RUN,SECRETS,TASKS,RUN_STORAGE,RUN_PUBSUB,RUN_FIRESTORE,RUN_DATASTORE,RUN_GKE,RUN_MEMORYSTORE_ADMIN,RUN_KMS,RUN_LOCALAI,RUN_SCHEDULER,RUN_LOGGING,RUN_BIGQUERY,RUN_SQLADMIN,RUN_RESOURCEMANAGER,RUN_SERVICEUSAGE,RUN_APIKEYS,RUN_MYSQL,MYSQL_PASSWORD,BIGQUERY_PROJECT,APPENGINE_URL,WORKFLOWS
      if [ "$1" = crossservice ]; then
        # The run shard's instance again, for COMPAT_CROSSSERVICE_TESTS only, with
        # the plain names of the services they read: COMPAT_RUN keeps every
        # other suite those names would select to its own shard.
        TEST_VARS+=,STORAGE,STORAGE_BATCH,STORAGE_INSIGHTS,PUBSUB,BIGQUERY,FIRESTORE,SERVICEUSAGE,KMS,LOGGING,RESOURCEMANAGER,MONITORING
        COMPAT_RUN=$COMPAT_CROSSSERVICE_TESTS
      else
        COMPAT_SKIP=$COMPAT_CROSSSERVICE_TESTS
        # Workflows (#1996): an execution waiting on a callback when the
        # instance stops carries on after it starts again.
        SETUP_TESTS="TestWorkflowsRestartSetup"
        PROBE_TESTS="TestWorkflowsAcrossRestart"
        PROBE_VARS=WORKFLOWS
      fi ;;
    scale)
      # The tests that take minutes each (#1821), split from run: Cloud
      # Run's CPU, automatic and manual scaling and instance splits, through
      # the SDK, gcloud and Terraform; the custom prediction containers, on
      # Cloud Run, with PREDICTION, the local AI endpoint Cloud Run starts
      # for PredictionService (#1193); and Secret Manager's 2,000 versions
      # (#1596). Cloud Run as SCALE_RUN and Secret Manager as SCALE_SECRETS,
      # which only these tests read, so neither suite runs a second time
      # here; RUN_RESOURCEMANAGER and RUN_SERVICEUSAGE for gcloud's
      # overrides, as in run. storage as RUN_STORAGE for the Model
      # Registry's artifacts (#1940: TestModelRegistryDeploysACustomContainer,
      # TestModelRegistryExportsTheImageToArtifactRegistry,
      # TestPythonAIPlatformUploadAndDeploy); STORAGE is not exported, so
      # the storage suite does not run here.
      SERVICES=run,secretmanager,storage
      TEST_VARS+=,SCALE_RUN,SCALE_SECRETS,PREDICTION,RUN_STORAGE,RUN_RESOURCEMANAGER,RUN_SERVICEUSAGE ;;
    acceptance)
      # The acceptance workflow uploads, publishes and runs a Cloud Run
      # worker; test/k8s needs the cluster and Knative.
      SERVICES=storage,pubsub,run ;;
    iam)
      # IAM enforcement (#1879): every TestIAMEnforcement* test, against
      # an instance that enforces, and nothing else, so the shard is a few
      # minutes; the other suites ran with enforcement off in their shards.
      # BigQuery for BigQuery sharing's (Analytics Hub, #1989) and for the
      # Pub/Sub BigQuery subscriptions' writer roles (#1998), with the one
      # project its emulator serves.
      # Cloud Scheduler and Workflows since the fourth round; Cloud
      # Logging, Cloud Monitoring, Memorystore and the Cloud SQL Admin API
      # since the fifth; Certificate Authority Service, Kubernetes Engine
      # (no cluster is made), Compute Engine, API Keys and Service Usage
      # since the seventh; the local AI endpoint (Vertex AI, Document AI,
      # Speech-to-Text, Translation), which Cloud Run starts with no
      # model, since the eighth; Firestore, Spanner and Bigtable, whose
      # fronts check each call, since the ninth; the Datastore API since
      # the tenth; the BigQuery Data Transfer Service, Backup and DR and the Cloud
      # Storage companion APIs since the twelfth; Parameter Manager for its
      # renders as the parameter's identity (#2054); the Firebase Rules API,
      # served with Firestore, whose releases govern client SDK calls,
      # which IAM does not (#2048).
      SERVICES=storage,pubsub,tasks,secretmanager,kms,run,bigquery,scheduler,workflows,logging,monitoring,memorystore,cloudsql,privateca,gke,compute,firestore,datastore,spanner,bigtable,backupdr,parametermanager
      TEST_VARS+=,STORAGE,PUBSUB,TASKS,SECRETS,KMS,RUN,RESOURCEMANAGER,ANALYTICSHUB,BIGQUERY,BIGQUERY_PROJECT,SCHEDULER,WORKFLOWS,LOGGING,MONITORING,MEMORYSTORE_ADMIN,SQLADMIN,PRIVATECA,GKE,COMPUTE,APIKEYS,SERVICEUSAGE,LOCALAI,FIRESTORE,DATASTORE,SPANNER,BIGTABLE,DATATRANSFER,BACKUPDR,STORAGE_BATCH,STORAGE_INSIGHTS,STORAGE_TRANSFER,IAM_ENFORCEMENT,PARAMETERMANAGER,FIREBASERULES
      UP_FLAGS=(--iam-enforcement=on)
      # And Workload Identity Federation's federated principal reading a
      # bucket, refused until it is granted (#1941), and Workforce Identity
      # Federation's (#2056), its SAML exchange read through Cloud Storage
      # and its pools' own IAM policies (#2065), and SCIM-provisioned users and
      # groups read through Cloud Storage (#2068), which need enforcement.
      COMPAT_RUN='^Test(IAMEnforcement|WorkloadIdentityFederationStorage$|WorkforceIdentityFederationStorage$|WorkforceIdentityFederationSAML$|WorkforcePoolIAMPolicy$|WorkforceScimProvisioning$)' ;;
    localai)
      # The local AI tests (#1985): BigQuery and Cloud Storage for the AI
      # and ML functions' tables and object tables, Resource Manager (every
      # instance serves it) for the roles they grant the connection's
      # service account. COMPAT_RUN keeps the shard to the local AI tests.
      SERVICES=bigquery,storage
      TEST_VARS+=,LOCALAI,EMBEDDINGS,BIGQUERY,BIGQUERY_PROJECT,BIGQUERY_RESOURCEMANAGER,RESOURCEMANAGER,STORAGE
      UP_FLAGS=(--local-ai-model "${CLOUDBURROW_LOCALAI_MODEL-}" --local-ai-embeddings)
      # The switches compat-env.sh has no source for: the instance is an
      # onnx build (TestBigQueryMLImportedONNXModel), and may fetch
      # MediaPipe's pinned classifier and detectors (32.6 MB) to annotate
      # images.
      export CLOUDBURROW_TEST_ONNX=1 CLOUDBURROW_TEST_VISION_LABELS=1 CLOUDBURROW_TEST_VISION_DETECT=1
      COMPAT_RUN=$COMPAT_LOCALAI_TESTS
      # 1,327 s of tests on an 8-CPU Docker Desktop VM with an Apple M-series
      # host (2026-11-20), generation and whisper.cpp on the CPU: the
      # runner's four vCPUs are slower, so twice the other shards' 60m.
      COMPAT_TIMEOUT=120m ;;
    emulators)
      # pubsub for TestEmulatorsBehindAFrontRefuseOtherPods (#1114), which
      # dials both emulators from a pod; PUBSUB is not exported, so the
      # Pub/Sub tests stay in the storage shard.
      SERVICES=spanner,datastore,firestore,bigtable,bigquery,storage,pubsub,logging,kms
      # logging for the sinks that deliver to Cloud Storage, Pub/Sub and
      # BigQuery (#1404), as SINK_LOGGING, SINK_STORAGE and SINK_PUBSUB, so
      # those suites stay in their shards; the copies to a Cloud Storage
      # bucket (#1394) run here under the same names. Cloud Storage again as
      # FIRESTORE_STORAGE, for Firestore's exports and imports (#1500), and
      # as BQML_STORAGE, for BigQuery ML's imported models (#1761).
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
      # (#1568) read it, so the Resource Manager tests stay in storage;
      # so does FIRESTORE_RESOURCEMANAGER, for the Firestore with MongoDB
      # compatibility tests that grant a sign-in's principal its roles
      # (#1738, #1742, #1743), with FIRESTORE_MONGODB_CA, the CA of the
      # endpoint since it serves TLS only (without the two the MongoDB
      # tests ran in no shard, #1761).
      # The Logging tests that need BigQuery (linked datasets, Log Analytics)
      # or Cloud Storage (the copies) read SINK_LOGGING and SINK_STORAGE, and
      # the copies from a folder LOGGING_RESOURCEMANAGER (#1568).
      # Cloud KMS as CMEK_KMS, for Firestore's customer-managed keys
      # (#1881), so the KMS suite stays in served.
      # The BigQuery Data Transfer Service (#1948), served with BigQuery.
      # BigQuery sharing (Analytics Hub, #1960), served with BigQuery.
      # The Firebase Rules API (#2048), served with Firestore.
      TEST_VARS+=,DATATRANSFER,ANALYTICSHUB,FIREBASERULES,LOGGING_RESOURCEMANAGER,SPANNER,SPANNER_REST,DATASTORE,FIRESTORE,FIRESTORE_MONGODB,FIRESTORE_MONGODB_CA,FIRESTORE_RESOURCEMANAGER,BIGTABLE,BIGQUERY,BIGQUERY_STORAGE,BIGQUERY_PROJECT,BIGQUERY_RESOURCEMANAGER,SINK_LOGGING,SINK_STORAGE,SINK_PUBSUB,FIRESTORE_STORAGE,BQML_STORAGE,CMEK_KMS
      # Firestore's probe documents and named database (#1636): written
      # alone, as a front-state reset in the suite forgets the database.
      SETUP_TESTS="TestFirestoreRestartSetup"
      PROBE_TESTS="TestDatastoreAcrossRestart TestFirestoreAcrossRestart"
      PROBE_VARS=DATASTORE,FIRESTORE ;;
    memorystore)
      # Memorystore for Redis (#296), Redis Cluster and Valkey, split from
      # the emulators shard (#1568). Cloud Storage for the exports and
      # imports, whose tests find it with `cloudburrow env`; STORAGE is not
      # exported, so the Cloud Storage tests stay in the storage shard.
      # Cloud KMS as CMEK_KMS, for the customer-managed keys, so the KMS
      # suite stays in served.
      # Resource Manager, which every instance serves, as
      # MEMORYSTORE_RESOURCEMANAGER, for the principals the IAM auth tests
      # grant the connect roles to (#1614), so the Resource Manager suite
      # stays in storage.
      # Certificate Authority Service as MEMORYSTORE_PRIVATECA, for a Redis
      # cluster's customer-managed CA (#1907), so the privateca suite stays
      # in its own shard.
      # Compute Engine (#1936) as MEMORYSTORE_COMPUTE: networks and
      # reserved ranges are checked against its networks.
      SERVICES=memorystore,storage,kms,privateca,compute
      TEST_VARS+=,MEMORYSTORE,MEMORYSTORE_ADMIN,MEMORYSTORE_PROJECT,CMEK_KMS,MEMORYSTORE_RESOURCEMANAGER,MEMORYSTORE_PRIVATECA,MEMORYSTORE_COMPUTE
      # A created Memorystore instance follows the mode too (#1302), with RDB
      # persistence as well (#1384), and Redis clusters and Valkey instances
      # with and without persistence_config (#1557).
      PROBE_TESTS="TestMemorystoreAcrossRestart TestMemorystoreCreatedAcrossRestart TestMemorystoreRDBAcrossRestart TestMemorystoreClustersAcrossRestart"
      PROBE_VARS=MEMORYSTORE,MEMORYSTORE_ADMIN,MEMORYSTORE_PROJECT ;;
    sql|sqlreplicas)
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
      # keys too. Certificate Authority Service as SQL_PRIVATECA, for
      # CUSTOMER_MANAGED_CAS_CA's CA pools (#1834), so the privateca suite
      # stays in its own shard. Service Usage, always served, as
      # SQL_SERVICEUSAGE, for gcloud privateca roots create's
      # generateServiceIdentity of the CA service agent (#1834).
      # Cloud Logging as SQL_LOGGING, for Backup and DR's job logs (#1924);
      # LOGGING is not in TEST_VARS, so the Logging suites stay in theirs.
      # Compute Engine (#1936) as SQL_COMPUTE: private networks are
      # checked against its networks.
      SERVICES=cloudsql,cloudsql-mysql,backupdr,storage,secretmanager,kms,privateca,logging,compute
      TEST_VARS+=,MYSQL,MYSQL_PASSWORD,CLOUDSQL,SQLADMIN,BACKUPDR,CMEK_KMS,CLOUDSQL_SECRETS,NOPUBSUB_STORAGE,SQL_RESOURCEMANAGER,SQL_SERVICEUSAGE,SQL_PRIVATECA,SQL_LOGGING,SQL_COMPUTE
      # The replica tests run in sqlreplicas, against the same instance,
      # and the restart probes in sql.
      if [ "$1" = sqlreplicas ]; then
        COMPAT_RUN=$COMPAT_SQL_REPLICA_TESTS
      else
        COMPAT_SKIP=$COMPAT_SQL_REPLICA_TESTS
        # Cloud SQL for PostgreSQL (#701): a table with a row.
        SETUP_TESTS="TestCloudSQLRestartSetup"
        PROBE_TESTS="TestCloudSQLMySQLAcrossRestart TestCloudSQLAcrossRestart"
        PROBE_VARS=MYSQL,MYSQL_PASSWORD,CLOUDSQL
      fi ;;
    *) echo "unknown shard $1" >&2; return 1 ;;
  esac
  # The pinned gcloud, in the shards whose services its tests use (#694);
  # bq's (#696) run in the emulators shard, with BigQuery, and gcloud sql's
  # in the sql shard.
  case "$1" in
    storage|served|run|crossservice|scale|emulators|memorystore|sql|sqlreplicas) TEST_VARS+=,GCLOUD ;;
  esac
}

# compat_browser_prepare <shard>: readies the instance the browser step
# brought up again for its tests. In the localai shard (#1985), loads
# EmbeddingGemma 2, which the compat suite downloaded into the state
# directory and loaded, and which a restart leaves unloaded: the Embeddings
# page's image, audio and PDF tests skip without it. Reads
# CLOUDBURROW_TEST_CONTROL and CLOUDBURROW_TEST_ADMIN_TOKEN, and prints
# neither.
compat_browser_prepare() {
  case "$1" in
    localai)
      local control=${CLOUDBURROW_TEST_CONTROL:?} code
      case "$control" in http://*) ;; *) control="http://$control" ;; esac
      code=$(curl -sS -o /dev/null -w '%{http_code}' -X POST -H @- \
        "$control/admin/local-models/embeddinggemma-2-onnx-community/load" <<EOF2
Authorization: Bearer ${CLOUDBURROW_TEST_ADMIN_TOKEN:?}
EOF2
      )
      [ "$code" = 200 ] || { echo "loading embeddinggemma-2-onnx-community answered $code" >&2; return 1; } ;;
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
    run) export CLOUDBURROW_TEST_WORKFLOWS_PROBE="$2/workflows-probe.json" ;;
    sql) export CLOUDBURROW_TEST_CLOUDSQL_SETUP=1 ;;
    emulators) export CLOUDBURROW_TEST_FIRESTORE_SETUP=1 ;;
  esac
}

# compat_probe_expect <present|absent> <dir>: NAME=value lines, what each
# restart probe expects to find after the restart, and the storage probe's
# fixture in <dir>.
compat_probe_expect() {
  local v
  for v in MEMORYSTORE MYSQL CLOUDSQL DATASTORE FIRESTORE KMS SECRETS TASKS SCHEDULER WORKFLOWS; do
    echo "CLOUDBURROW_TEST_${v}_EXPECT=$1"
  done
  echo "CLOUDBURROW_TEST_STORAGE_RESTART_PROBE=$1:$2/storage-restart-probe.json"
}
