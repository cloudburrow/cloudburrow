# Compatibility tests

Black-box tests that drive CloudBurrow through **official Google Cloud SDKs**. They are the
only evidence that promotes an operation off `Planned` in
[`docs/compatibility.md`](../../docs/compatibility.md).

A test written against handwritten HTTP requests does not count: it verifies our reading of
an API, not a real client's.

## Running

```sh
cloudburrow up --detach --name ct --state-dir "$PWD/state"
cloudburrow wait --name ct --state-dir "$PWD/state"
eval "$(scripts/compat-env.sh --name ct --state-dir "$PWD/state")"
make test-compat
```

`up --detach` returns once the instance is ready and leaves it running in the background;
`wait` returns once it reports ready, at once when it already does. `cloudburrow stop --name ct --state-dir "$PWD/state"`
ends it.

[`scripts/compat-env.sh`](../../scripts/compat-env.sh) takes the flags the instance was started
with and prints an `export` for every `CLOUDBURROW_TEST_*` variable below that the instance has
a value for. It reads `cloudburrow env --format json` for the SDK endpoints,
`cloudburrow status --format json` for the kind cluster, and the instance directory for the
runtime file (the addresses `up` bound), the admin token and the kubeconfig. The variables it
cannot fill, because a service is not enabled, it names on stderr: their tests skip. CI's
compat shards run the same script, each with `--strict --only <its variables>`, which fails on
an empty value instead; `--list` prints every variable it sets and `--help` its options. It
needs bash and `jq`.

Pass an absolute `--state-dir`: the tests run the CLI from `test/compat` with the same flags
(`CLOUDBURROW_TEST_CLI_ARGS`), so a relative one would name a different directory.

### Which instance the CLI and gcloud tests reach

The tests that run `cloudburrow` or gcloud find the instance through `CLOUDBURROW_TEST_CLI_ARGS`
alone, and flags that leave out the instance resolve the default one and its ports, which may
be someone else's long-running instance (#927). Three guards keep them on the harness's
instance:

- `scripts/compat-env.sh` always puts `--name` and `--state-dir` in `CLOUDBURROW_TEST_CLI_ARGS`,
  taking any the flags leave out from the instance directory `env` reports, and refuses a flag
  containing whitespace, since the tests split the value on it. Its default output is quoted for
  `eval`. `--plain` output is not quoted: read it with `env(1)` or `mapfile`, never `source` it.
  The script warns about each value a shell would split or expand.
- When `CLOUDBURROW_TEST_CLI` is set, `TestMain` (`main_test.go`) runs
  `cloudburrow env --format json` and `cloudburrow status --format json` with
  `CLOUDBURROW_TEST_CLI_ARGS` before any test, and compares what they report with every
  endpoint variable set (`CONTROL`, `CONSOLE`, `METADATA`, `CREDENTIALS` and each service's).
  If any differs or is missing, or none is set, the whole run fails before any test runs, and the
  message names each difference (`checkTarget` in `target.go`, unit-tested in `target_test.go`
  without an instance).
- A gcloud test runs gcloud and `gcloud-setup` without the caller's `CLOUDSDK_*`, `GOOGLE_*`,
  `GCE_*`, `BOTO_*` or `*_EMULATOR_HOST` variables. It keeps only the endpoint overrides in
  `gcloud-setup`'s configuration that match the harness's endpoint for their service. An
  override for another endpoint fails the test before gcloud runs. An override for a service
  without a harness variable is dropped, so the egress guard refuses that call instead.

A test skips when a variable it needs is unset. An instance started with the default services
leaves most suites skipped, so start one with the services your change touches (`--services`),
and read the `--- SKIP` lines in `go test -v` output rather than a green `ok`.

### Every variable

Set by `scripts/compat-env.sh` from a running instance:

| Variable | From | Needed by |
| --- | --- | --- |
| `CLOUDBURROW_TEST_CREDENTIALS` | `GOOGLE_APPLICATION_CREDENTIALS` from `env` | the credentials test (`credentials_test.go`) and the signed URL tests |
| `CLOUDBURROW_TEST_METADATA` | runtime file, `metadata` | the credentials and IAM Credentials tests |
| `CLOUDBURROW_TEST_CONTROL` | runtime file, `control` | the admin API: events, faults, reset, seed, and the tests that inject faults or reset state through it |
| `CLOUDBURROW_TEST_CONSOLE` | runtime file, `console` | the console tests |
| `CLOUDBURROW_TEST_ADMIN_TOKEN` | `admin-token` in the instance directory | every `/admin` call (`adminauth_test.go`); read from the state directory in `CLOUDBURROW_TEST_CLI_ARGS` when unset |
| `CLOUDBURROW_TEST_KUBECONFIG` | `kubeconfig` in the instance directory, or `--kubeconfig` | the tests that restart a backend, run a pod or port-forward the Knative gateway: Spanner, prediction, Cloud Run, functions, the cluster host services |
| `CLOUDBURROW_TEST_CLUSTER` | `cluster.name` from `status` | the tests that `kind load` a fixture image into the instance's own cluster: prediction, Cloud Run environment, functions, Spanner, the cluster host services |
| `CLOUDBURROW_TEST_CLI` | `--cli`, else `bin/cloudburrow`, else `cloudburrow` on PATH | every test that runs a `cloudburrow` command: `env`, `status`, `logs`, `diagnose`, `state`, `terraform`, `gcloud-setup`, the hooks and the restart tests |
| `CLOUDBURROW_TEST_CLI_ARGS` | the flags given to the script, with `--name` and `--state-dir` added from the instance directory when they are missing | the same tests, to name this instance; checked against the endpoints before any test runs |
| `CLOUDBURROW_TEST_STORAGE` | `STORAGE_EMULATOR_HOST` | Cloud Storage |
| `CLOUDBURROW_TEST_CORS_ORIGIN` | the first `--cors-allow-origin` among the flags given to the script | `TestStorageCORSAllowlistedOriginWorks` |
| `CLOUDBURROW_TEST_PUBSUB` | `PUBSUB_EMULATOR_HOST` | Pub/Sub, and Storage notifications |
| `CLOUDBURROW_TEST_TASKS` | runtime file, `tasks` | Cloud Tasks, and the console and Terraform tests that use it |
| `CLOUDBURROW_TEST_SECRETS` | runtime file, `secretmanager` | Secret Manager, gcloud secrets, seed and Terraform |
| `CLOUDBURROW_TEST_SCHEDULER` | `CLOUDBURROW_SCHEDULER_ENDPOINT` | Cloud Scheduler, gcloud scheduler, Terraform |
| `CLOUDBURROW_TEST_RUN` | runtime file, `run` | Cloud Run |
| `CLOUDBURROW_TEST_KMS` | `CLOUDBURROW_KMS_ENDPOINT` | Cloud KMS, gcloud kms, Terraform |
| `CLOUDBURROW_TEST_LOGGING` | `CLOUDBURROW_LOGGING_ENDPOINT` | Cloud Logging, gcloud logging |
| `CLOUDBURROW_TEST_RESOURCEMANAGER` | `CLOUDBURROW_RESOURCEMANAGER_ENDPOINT` | Resource Manager |
| `CLOUDBURROW_TEST_SERVICEUSAGE` | `CLOUDBURROW_SERVICEUSAGE_ENDPOINT` | the Service Usage API (#1352), gcloud services and Terraform's `google_project_service` |
| `CLOUDBURROW_TEST_APIKEYS` | `CLOUDBURROW_APIKEYS_ENDPOINT` | the API Keys API v2 (#1915), gcloud services api-keys and Terraform's `google_apikeys_key` |
| `CLOUDBURROW_TEST_STORAGE_BATCH` | `CLOUDBURROW_STORAGEBATCHOPERATIONS_ENDPOINT` | the Storage Batch Operations API v1 (#1957), gcloud storage batch-operations |
| `CLOUDBURROW_TEST_STORAGE_INSIGHTS` | `CLOUDBURROW_STORAGEINSIGHTS_ENDPOINT` | the Storage Insights API v1's inventory reports (#1957), gcloud storage insights inventory-reports and Terraform's `google_storage_insights_report_config` |
| `CLOUDBURROW_TEST_STORAGE_TRANSFER` | `CLOUDBURROW_STORAGETRANSFER_ENDPOINT` | the Storage Transfer Service API v1 (#1957), gcloud transfer and Terraform's `google_storage_transfer_job` |
| `CLOUDBURROW_TEST_RUN_STORAGE`, `CLOUDBURROW_TEST_RUN_PUBSUB` | as `STORAGE` and `PUBSUB` | the Cloud Run revision tests; see below |
| `CLOUDBURROW_TEST_RUN_FIRESTORE` | as `FIRESTORE` | the Eventarc Firestore triggers (#1877), on the run shard |
| `CLOUDBURROW_TEST_RUN_DATASTORE` | as `DATASTORE` | the Eventarc Datastore mode triggers (#1877), on the run shard |
| `CLOUDBURROW_TEST_RUN_GKE` | as `GKE` | the Eventarc GKE destination (#1877), on the run shard |
| `CLOUDBURROW_TEST_RUN_MEMORYSTORE_ADMIN` | as `MEMORYSTORE_ADMIN` | the Eventarc Memorystore for Redis direct events (#1997), on the run shard |
| `CLOUDBURROW_TEST_RUN_KMS`, `CLOUDBURROW_TEST_RUN_SCHEDULER`, `CLOUDBURROW_TEST_RUN_LOGGING` | as `KMS`, `SCHEDULER` and `LOGGING` | the Cloud Run revision tests; see below |
| `CLOUDBURROW_TEST_RUN_LOCALAI` | as `LOCALAI`: the local Vertex AI endpoint, which Cloud Run starts without a model | the run shard's Document AI and Speech-to-Text customer-managed key tests (#1972), with `RUN_KMS` |
| `CLOUDBURROW_TEST_TASKS_LOGGING` | as `LOGGING` | the Cloud Tasks dispatch log (#1256), in the storage shard, so the Logging tests stay in served |
| `CLOUDBURROW_TEST_PUBSUB_MONITORING` | as `MONITORING` | the Pub/Sub notification channel test (#1966), in the storage shard beside its Pub/Sub, so the Monitoring tests stay in served |
| `CLOUDBURROW_TEST_BIGQUERY_RESOURCEMANAGER` | as `RESOURCEMANAGER` | BigQuery's `JOBS_BY_FOLDER` and `JOBS_BY_ORGANIZATION` (#1392) and the purged project's emulator data (#1568), in the emulators shard, so the Resource Manager tests stay in storage |
| `CLOUDBURROW_TEST_MEMORYSTORE_RESOURCEMANAGER` | as `RESOURCEMANAGER` | the principals the Memorystore IAM auth tests grant the connect roles to (#1614), in the memorystore shard, so the Resource Manager tests stay in storage |
| `CLOUDBURROW_TEST_FIRESTORE_RESOURCEMANAGER` | as `RESOURCEMANAGER` | the roles the Firestore with MongoDB compatibility tests grant a sign-in's principal (#1738, #1742, #1743), in the emulators shard, so the Resource Manager tests stay in storage (#1761) |
| `CLOUDBURROW_TEST_SQL_RESOURCEMANAGER` | as `RESOURCEMANAGER` | the purged project's Cloud SQL, Backup and DR and Artifact Registry, and Backup and DR's folder and organization fetch (#1568), in the sql shard, so the Resource Manager tests stay in storage |
| `CLOUDBURROW_TEST_SQL_LOGGING` | as `LOGGING` | Backup and DR's job logs in Cloud Logging (#1924), in the sql shard, so the Logging tests stay in theirs |
| `CLOUDBURROW_TEST_RUN_RESOURCEMANAGER`, `CLOUDBURROW_TEST_RUN_SERVICEUSAGE` | as `RESOURCEMANAGER` and `SERVICEUSAGE` | gcloud's Resource Manager and Service Usage overrides in the run shard: `gcloud functions deploy --trigger-service-account` reads the project number through the one and checks `run.googleapis.com` is enabled through the other (main run 38929852240), so neither suite runs twice; the run shard's browser suite enables the Container Scanning API through `RUN_SERVICEUSAGE` (#1912) |
| `CLOUDBURROW_TEST_RUN_APIKEYS` | as `APIKEYS` | the API key a Cloud Build webhook trigger's URL carries (`?key=`), made through the API Keys API in the run shard, so the API Keys suite stays in storage (#1915) |
| `CLOUDBURROW_TEST_GKE_SERVICEUSAGE` | as `SERVICEUSAGE` | gcloud's Service Usage override in the served shard: `gcloud container clusters create` checks `container.googleapis.com` is enabled through it (#1913), so the Service Usage suite does not run there too |
| `CLOUDBURROW_TEST_LOGGING_RESOURCEMANAGER` | as `RESOURCEMANAGER` | the Cloud Logging folder and organization tests, which make a folder (#1395), in the served shard, so the Resource Manager tests stay in storage |
| `CLOUDBURROW_TEST_SINK_LOGGING`, `CLOUDBURROW_TEST_SINK_STORAGE`, `CLOUDBURROW_TEST_SINK_PUBSUB` | as `LOGGING`, `STORAGE` and `PUBSUB` | the Cloud Logging sinks that deliver to Cloud Storage, Pub/Sub and BigQuery (#1404), in the emulators shard beside BigQuery, and `gcloud logging copy` and `CopyLogEntries` to a bucket (#1394) |
| `CLOUDBURROW_TEST_CMEK_KMS` | as `KMS` | the customer-managed keys of Memorystore (memorystore shard) and of Cloud SQL and a Backup and DR vault (sql shard) (#1568), and of Spanner's databases and backups (emulators shard, #1950), so the Cloud KMS tests stay in served |
| `CLOUDBURROW_TEST_SQL_SERVICEUSAGE` | as `SERVICEUSAGE` | gcloud privateca roots create's `generateServiceIdentity` of the CA service agent (#1834), in the sql shard, so the Service Usage tests stay in storage |
| `CLOUDBURROW_TEST_SQL_COMPUTE` | as `COMPUTE` | Cloud SQL's private networks checked against Compute Engine's (#1936), in the sql shard, so the Compute Engine suite stays in served |
| `CLOUDBURROW_TEST_MEMORYSTORE_COMPUTE` | as `COMPUTE` | Memorystore's networks and reserved ranges checked against Compute Engine's (#1936), in the memorystore shard |
| `CLOUDBURROW_TEST_SQL_PRIVATECA` | as `PRIVATECA` | the CA pools of Cloud SQL's `CUSTOMER_MANAGED_CAS_CA` instances (#1834), in the sql shard, so the Certificate Authority Service tests stay in their shard |
| `CLOUDBURROW_TEST_MEMORYSTORE_PRIVATECA` | as `PRIVATECA` | the CA pools of Redis clusters' `SERVER_CA_MODE_CUSTOMER_MANAGED_CAS_CA` (#1907), in the memorystore shard, so the Certificate Authority Service tests stay in their shard |
| `CLOUDBURROW_TEST_CLOUDSQL_SECRETS` | as `SECRETS` | Cloud SQL's Data API, whose password is a secret version, in the sql shard (#1568), so the Secret Manager tests stay in storage and run |
| `CLOUDBURROW_TEST_SCALE_RUN`, `CLOUDBURROW_TEST_SCALE_SECRETS` | as `RUN` and `SECRETS` | the tests that take minutes each, in the scale shard (#1821): Cloud Run's scaling and instance splits, the custom prediction containers, and Secret Manager's 2,000 versions; they read only these names, so they run once, and the Cloud Run and Secret Manager suites stay in the run and storage shards |
| `CLOUDBURROW_TEST_NOPUBSUB_STORAGE` | as `STORAGE` | Cloud Storage notifications refused 501 on an instance without Pub/Sub, in the sql shard, the one that serves none (#1568) |
| `CLOUDBURROW_TEST_FIRESTORE_STORAGE` | as `STORAGE` | the Cloud Storage Firestore's exports and imports go through (#1500), in the emulators shard beside Firestore |
| `CLOUDBURROW_TEST_BQML_STORAGE` | as `STORAGE` | the bucket BigQuery ML's imported models are read from (`TestBigQueryMLImportedXGBoostModel`), in the emulators shard beside BigQuery (#1761) |
| `CLOUDBURROW_TEST_RUN_BIGQUERY` | as `BIGQUERY` | the Cloud Run revision refused by the BigQuery front (#874); see below |
| `CLOUDBURROW_TEST_RUN_SQLADMIN` | as `SQLADMIN` | the Cloud Run services and job with a Cloud SQL volume (#1346), without the Cloud SQL suite |
| `CLOUDBURROW_TEST_RUN_MYSQL` | as `MYSQL` | the Cloud Run service with a Cloud SQL for MySQL volume (#1346, #1568), without the MySQL suite |
| `CLOUDBURROW_TEST_SPANNER` | `SPANNER_EMULATOR_HOST` | Spanner |
| `CLOUDBURROW_TEST_SPANNER_REST` | `CLOUDBURROW_SPANNER_REST_ENDPOINT` | Spanner's REST API, the Terraform provider's `spanner_custom_endpoint` (#1331) |
| `CLOUDBURROW_TEST_DATASTORE` | `DATASTORE_EMULATOR_HOST` | Datastore |
| `CLOUDBURROW_TEST_FIRESTORE` | `FIRESTORE_EMULATOR_HOST` | Firestore |
| `CLOUDBURROW_TEST_FIRESTORE_MONGODB` | `CLOUDBURROW_FIRESTORE_MONGODB_ENDPOINT` | Firestore with MongoDB compatibility's wire protocol, for the official MongoDB Go driver (#1703) |
| `CLOUDBURROW_TEST_FIRESTORE_MONGODB_CA` | `CLOUDBURROW_FIRESTORE_MONGODB_CA_FILE` | the CA certificate file of that endpoint's TLS, the driver's `tlsCAFile`: it serves TLS only, as Google's does (#1738) |
| `CLOUDBURROW_TEST_BIGTABLE` | `BIGTABLE_EMULATOR_HOST` | Bigtable |
| `CLOUDBURROW_TEST_MEMORYSTORE` | `REDIS_HOST`:`REDIS_PORT` | Memorystore |
| `CLOUDBURROW_TEST_MEMORYSTORE_ADMIN` | `CLOUDBURROW_MEMORYSTORE_ENDPOINT` | Memorystore admin API (#1302) |
| `CLOUDBURROW_TEST_MEMORYSTORE_PROJECT` | `GOOGLE_CLOUD_PROJECT` | the project the one Memorystore instance is in |
| `CLOUDBURROW_TEST_MYSQL`, `CLOUDBURROW_TEST_MYSQL_PASSWORD` | `MYSQL_HOST`:`MYSQL_PORT`, `MYSQL_PASSWORD` | Cloud SQL for MySQL |
| `CLOUDBURROW_TEST_CLOUDSQL` | `PGHOST`:`PGPORT` | Cloud SQL for PostgreSQL |
| `CLOUDBURROW_TEST_SQLADMIN` | `CLOUDBURROW_SQLADMIN_ENDPOINT` | the Cloud SQL Admin API (#1301), with either Cloud SQL service |
| `CLOUDBURROW_TEST_BACKUPDR` | `CLOUDBURROW_BACKUPDR_ENDPOINT` | the Backup and DR Service (#1433), `--services backupdr` with either Cloud SQL service; gcloud backup-dr and Terraform's `google_backup_dr_*` |
| `CLOUDBURROW_TEST_COMPUTE` | `CLOUDBURROW_COMPUTE_ENDPOINT` | the Compute Engine API's VPC networking (#1936), `--services compute`; JSON under `/compute/v1/`, in process |
| `CLOUDBURROW_TEST_GKE` | `CLOUDBURROW_GKE_ENDPOINT` | the Kubernetes Engine API (#1913), `--services gke`; gRPC and JSON on one port; its clusters are kind clusters, so it needs Docker, kind and kubectl |
| `CLOUDBURROW_TEST_DATATRANSFER` | `CLOUDBURROW_DATATRANSFER_ENDPOINT` | the BigQuery Data Transfer Service (#1948), served with `bigquery`; gRPC and JSON on one port |
| `CLOUDBURROW_TEST_ANALYTICSHUB` | `CLOUDBURROW_ANALYTICSHUB_ENDPOINT` | BigQuery sharing, formerly Analytics Hub (#1960), served with `bigquery`; gRPC and JSON on one port; its subscriptions make linked datasets in the instance's BigQuery |
| `CLOUDBURROW_TEST_PRIVATECA` | `CLOUDBURROW_PRIVATECA_ENDPOINT` | Certificate Authority Service (#1834), `--services privateca`; gRPC and JSON on one port |
| `CLOUDBURROW_TEST_WORKFLOWS` | `CLOUDBURROW_WORKFLOWS_ENDPOINT` | Workflows and Workflow Executions (#1986), `--services workflows`; gRPC and JSON on one port; run in the run shard, beside the Cloud Run, Cloud Storage and Pub/Sub its workflows call |
| `CLOUDBURROW_TEST_MONITORING` | `CLOUDBURROW_MONITORING_ENDPOINT` | Cloud Monitoring (#1939), `--services monitoring`; gRPC and JSON on one port |
| `CLOUDBURROW_TEST_CLOUDSQL_CONNECTOR` | `CLOUDBURROW_CLOUDSQL_CONNECTOR_ENDPOINT` | the Cloud SQL connectors' TLS listener (#1364); `TestCloudSQLConnectorDialsBothServers` redirects the connector's dial of port 3307 to it only when an instance was started with `--cloudsql-connector` on another port |
| `CLOUDBURROW_TEST_BIGQUERY`, `CLOUDBURROW_TEST_BIGQUERY_STORAGE`, `CLOUDBURROW_TEST_BIGQUERY_PROJECT` | `CLOUDBURROW_BIGQUERY_ENDPOINT`, `CLOUDBURROW_BIGQUERY_STORAGE_ENDPOINT`, `GOOGLE_CLOUD_PROJECT` | BigQuery, whose emulator serves the instance's one project |
| `CLOUDBURROW_TEST_LOCALAI` | the `local AI:` line of `up.log` | the generation tests and `TestBigQueryMLRemoteGenerateText` and `TestBigQueryMLRemoteDefaultConnection` (#1760, #1765; `TestBigQueryMLRemoteConnectionPermissions` needs only `CLOUDBURROW_TEST_BIGQUERY` and Resource Manager; BigQuery ML's remote models on the local runtime, with `CLOUDBURROW_TEST_BIGQUERY` and `--services bigquery`); only with `--local-ai-model` |
| `CLOUDBURROW_TEST_PREDICTION` | the `local AI:` line of `up.log` | `TestPredictionClientReachesACustomEndpoint` and the other prediction tests, with `SCALE_RUN`; any instance with Cloud Run, which starts the local AI endpoint for PredictionService (#1193); CI's scale shard sets it (#1821) |
| `CLOUDBURROW_TEST_APPENGINE_URL` | the `--appengine-url` the instance was started with, from its flags, such as `http://127.0.0.1:18506`, which the run shard passes (#1568), on a free port | `TestTasksAppEngineDelivery` and `TestSchedulerAppEngineDelivery` (#1506), which serve the stand-in App Engine app there; when it is set, `TestTasksAppEngineTargets` and `TestSchedulerAppEngineTarget` (the 404s of an instance without it) skip |
| `CLOUDBURROW_TEST_IAM_ENFORCEMENT` | `on` when the instance was started with `up --iam-enforcement=on`, from its runtime file (#1879) | every `TestIAMEnforcement*` test, which skips unless it is `on`; CI's `iam` shard runs them against an instance of their own. With it `on`, the suite's default HTTP transport adds the instance's own ID token to a request for a Cloud Run service's host that carries no `Authorization` (`ownerinvoke1879_test.go`), so the Cloud Run suites invoke as the owner |
| `CLOUDBURROW_TEST_EMBEDDINGS` | the `local AI:` line of `up.log`, set only when that block's `embeddings: POST …` line says the embedding runtime loaded (#1765; `embeddings: not loaded` leaves it unset) | `TestBigQueryMLRemoteGenerateEmbedding` (#1760, with `CLOUDBURROW_TEST_BIGQUERY`), `TestEmbeddingGemmaThroughTheOfficialSDK`, `TestEmbeddingGemmaThroughThePredictionClient`, `TestEmbedContentThroughThePredictionClient`, `TestRawPredictOnTheEmbeddingModel`, `TestStreamingPredictOnTheEmbeddingModel` and, with `CLOUDBURROW_TEST_CONSOLE`, `TestEmbedContentEmbedsImagesThroughThePredictionClient` (#1454, which downloads and loads `embeddinggemma-2-onnx-community`, 882 MB, through the console's Local models), `TestEmbedContentEmbedsAudioThroughThePredictionClient` (#1651, the same, and with `CLOUDBURROW_TEST_STORAGE`; it fetches the model card's 1.9 MB `jfk.wav` from Hugging Face, pinned by SHA-256), `TestEmbedContentEmbedsMP3ThroughThePredictionClient` (#1721, the same; it embeds `testdata/jfk.mp3` beside that WAV), `TestEmbedContentEmbedsMPEG25MP3ThroughThePredictionClient` (#1734, the same; it embeds `testdata/jfk-mpeg25-8k.mp3` and `testdata/jfk-mpeg25-11k.mp3`), `TestEmbedContentEmbedsAVideosAudioTrackThroughThePredictionClient` (#1732, the same; it embeds `testdata/turtle-jfk.mp4` with its audio track), `TestEmbedContentEmbedsVP9AndAV1ThroughThePredictionClient` (#1731, the same; VP9 and AV1 clips from `internal/embedding/testdata`), `TestEmbedContentEmbedsMultichannelWAVAndGrayVideoThroughThePredictionClient` (#1651, the same; a 5.1 copy of `jfk.wav`, `testdata/jfk-alaw.wav`, a monochrome H.265 clip and a 5.1 PCM MOV from `internal/embedding/testdata`), `TestEmbedContentEmbedsADPCMWAVAndLosslessAndMultichannelTracksThroughThePredictionClient` (#1651, the same; `testdata/jfk-ima.wav`, `testdata/jfk-ms.wav` and clips with FLAC, ALAC and 5.1 AAC tracks, `testdata/va-jfk-*.mp4`), `TestEmbedContentEmbedsSurroundAACAndG711TracksAnd3BitADPCMThroughThePredictionClient` (#1651, the same; clips with 7.1, 5.1 HE-AAC and µ-law tracks, `testdata/va-jfk-*`, and `testdata/jfk-ima3.wav`), `TestEmbedContentEmbeds2And5BitADPCMAndMP3WAVsThroughThePredictionClient` (#1651, the same; `testdata/jfk-ima2.wav`, `testdata/jfk-ima5.wav` and `testdata/jfk-mp3.wav`), `TestEmbedContentEmbedsHEICAndAnimatedWebPThroughThePredictionClient` (#1724, the same, with `CLOUDBURROW_TEST_STORAGE`), `TestConsoleEmbeddingsPlaygroundMatchesTheOfficialSDK` (#1305) and `TestConsoleLocalModelsUnloadDeleteDownloadLoad` (#1306, which downloads the model again) and `TestConsoleLocalModelsLoadEmbedUnload` (#1396, with `CLOUDBURROW_TEST_CONTROL` and the admin token); only from a `make build-onnx` binary started with `--local-ai-embeddings` and `ONNXRUNTIME_LIB` set (#41) |
| `CLOUDBURROW_TEST_ONNX` | set by hand (`1`) when the instance is a `make build-onnx` binary started with `ONNXRUNTIME_LIB` set | `TestBigQueryMLImportedONNXModel` (#1711, BigQuery ML's imported ONNX models, with `CLOUDBURROW_TEST_STORAGE`); without it, `TestBigQueryMLONNXNeedsTheOnnxBuild` checks the default build's 501 |
| `CLOUDBURROW_TEST_DOWNLOADS` | set by hand (`1`) to let tests download test data that is not cached yet; not an instance's | `TestEmbedContentEmbedsAFullParametricStereoTrackThroughThePredictionClient` (#1754, with `CLOUDBURROW_TEST_EMBEDDINGS` and `CLOUDBURROW_TEST_CONSOLE`, as the other EmbedContent tests): it reads ISO/IEC 14496-26's `al_sbr_ps_06.mp4` and `al_sbr_ps_06_ur.wav` through `internal/isoconformance`, which fetches them (about 3 MB, by HTTP range requests out of ISO's zips, pinned by SHA-256 and ISO's MD5) into the user's cache directory, never the repository; without the variable a file not cached yet skips the test |
| `CLOUDBURROW_TEST_GCLOUD` | `--gcloud`, else `gcloud` on PATH | the gcloud and gsutil tests |
| `CLOUDBURROW_TEST_TOFU` | `--tofu`, else `tofu` on PATH | the `TestTofu*` tests |

Not set by the script, because no instance has a value for them: each is a test mode or a
fixture a job makes for itself. `test/repo` fails when a variable in `test/compat` is in
neither list.

| Variable | Needed by |
| --- | --- |
| `CLOUDBURROW_TEST_KMS_PROBE`, `CLOUDBURROW_TEST_SECRETS_PROBE`, `CLOUDBURROW_TEST_TASKS_PROBE`, `CLOUDBURROW_TEST_SCHEDULER_PROBE`, `CLOUDBURROW_TEST_WORKFLOWS_PROBE` | the restart probes' setup: the file each writes its fixture to |
| `CLOUDBURROW_TEST_KMS_EXPECT`, `CLOUDBURROW_TEST_SECRETS_EXPECT`, `CLOUDBURROW_TEST_TASKS_EXPECT`, `CLOUDBURROW_TEST_SCHEDULER_EXPECT`, `CLOUDBURROW_TEST_DATASTORE_EXPECT`, `CLOUDBURROW_TEST_FIRESTORE_EXPECT`, `CLOUDBURROW_TEST_MEMORYSTORE_EXPECT`, `CLOUDBURROW_TEST_MYSQL_EXPECT`, `CLOUDBURROW_TEST_CLOUDSQL_EXPECT`, `CLOUDBURROW_TEST_WORKFLOWS_EXPECT` | the restart probes: `present` after a persistent restart, `absent` after an ephemeral one |
| `CLOUDBURROW_TEST_CLOUDSQL_SETUP` | `TestCloudSQLRestartSetup` |
| `CLOUDBURROW_TEST_FIRESTORE_SETUP` | `TestFirestoreRestartSetup` |
| `CLOUDBURROW_TEST_STORAGE_RESTART_PROBE` | `TestStorageAcrossRestart`: `<mode>:<file>` |
| `CLOUDBURROW_TEST_STORAGE_VERSIONING_PROBE` | `TestStorageVersioningPersistentMode`, against the builtin `storage-server` |
| `CLOUDBURROW_TEST_SIGNING_KEY`, `CLOUDBURROW_TEST_SIGNING_EMAIL` | the signed URL tests, against a builtin `storage-server` started with `--signing-cert` |
| `CLOUDBURROW_TEST_FUNCTIONS` | `TestFunctionsFrameworkBuiltWithBuildpacks`, `TestFunctionsRebuildReusesLayers` and `TestFunctionsBuildBrokenModulePathFails`, which run only when it is `1` |
| `CLOUDBURROW_TEST_STEP_IMAGE` | the Cloud Build arbitrary-step tests (#1382): the shell image their steps run, instead of `alpine:3.20`, for one already on the host |
| `CLOUDBURROW_TEST_CLOUD_SQL_PROXY` | `TestCloudSQLAuthProxyBinary` (#1421): Google's `cloud-sql-proxy` binary, pinned by SHA-256, as `make -s cloud-sql-proxy` downloads it and prints its path; CI's sql shard sets it |
| `CLOUDBURROW_TEST_OSV_REQUIRED` | `TestArtifactRegistryVulnerabilityScanning`, `TestGcloudArtifactsVulnerabilityScanning` (#1917), and the browser suite's `TestCloudRunArtifactRegistryVulnerabilitiesTabThroughThePage`: set, a scan that finds the scanner not prefetched fails the test instead of skipping it; CI's run shard sets it after `go run ./tools/osvmirror fetch -dir <state dir>/cache -ecosystems Alpine` |
| `CLOUDBURROW_TEST_VISION_LABELS` | `TestBigQueryMLAnnotateImageLabels` (#1799): when set, runs ML.ANNOTATE_IMAGE's `LABEL_DETECTION`, for which the instance downloads MediaPipe's EfficientNet-Lite0 classifier (18.6 MB, pinned by SHA-256) from storage.googleapis.com the first time; needs `CLOUDBURROW_TEST_LOCALAI` and `CLOUDBURROW_TEST_BIGQUERY` |
| `CLOUDBURROW_TEST_VISION_DETECT` | `TestBigQueryMLAnnotateImageDetections` (#1799): when set, runs ML.ANNOTATE_IMAGE's `FACE_DETECTION` and `OBJECT_LOCALIZATION`, for which the instance downloads MediaPipe's EfficientDet-Lite0 object detector (13.8 MB) and BlazeFace short-range face detector (0.2 MB), pinned by SHA-256, from storage.googleapis.com the first time; needs `CLOUDBURROW_TEST_LOCALAI` and `CLOUDBURROW_TEST_BIGQUERY` |
| `CLOUDBURROW_TEST_HF_DEPLOY` | `TestModelGardenDeploysHuggingFaceModels` (#1840): when set, deploys Qwen/Qwen2.5-0.5B-Instruct (0.95 GB) and BAAI/bge-small-en-v1.5 (0.38 GB) from huggingface.co through ModelGardenService.Deploy, which pulls the vLLM CPU (5.5 GB) and Text Embeddings Inference CPU (0.9 GB) images; needs `CLOUDBURROW_TEST_PREDICTION` (Cloud Run enabled) and Docker, and runs in no CI shard |
| `CLOUDBURROW_TEST_SKIP_RUN_PULL` | `TestArtifactRegistryDockerRegistry` (#1383): when set, skips deploying a Cloud Run service from the pushed image, the slower part of the test |
| `CLOUDBURROW_TEST_NAMESPACE` | the Spanner restart and in-cluster tests; defaults to `cloudburrow`, the namespace every instance uses |

The restart probes are driven by ci.yml's compat job, which runs each setup, stops the
instance, brings it up again and runs each probe with these set; see
[docs/compatibility.md](../../docs/compatibility.md).

`GOOGLE_APPLICATION_CREDENTIALS` is deliberately **not** exported for the run: the harness
refuses to start with cloud credentials in the environment, and the credentials test sets the
fixture for its own call only. That is what proves the fixture answered rather than a
developer's real gcloud login.

The cluster name is needed to `kind load` the fixture image into **CloudBurrow's own**
cluster, and the kubeconfig to port-forward the Knative gateway — CloudBurrow does not
publish it on a host port. Both are skips, not failures, when unset.

The gcloud, gsutil and bq tests run the gcloud named by `CLOUDBURROW_TEST_GCLOUD`, which must
then exist, and the gsutil and bq beside it, or else the ones on PATH, and skip when there are
none. CI
sets it to the Google Cloud CLI release it installs, 593.0.0, whose archive checksum
dependencies.json records. The Terraform and OpenTofu modules require `hashicorp/google`
exactly, and each test copies `testdata/terraform/.terraform.lock.hcl` into its module before
init, so a provider package that matches no locked hash fails init. Every one of these tests
logs `gcloud version`, `gsutil version`, `bq version`, or `terraform version -json`
(`tofu version -json`).

`TestCloudRunRevisionReachesStorageAndPubSubWithNoClientOptions` needs Cloud Run, Storage and
Pub/Sub in one instance. It reads `CLOUDBURROW_TEST_RUN_STORAGE` and `CLOUDBURROW_TEST_RUN_PUBSUB`
when set, else `CLOUDBURROW_TEST_STORAGE` and `CLOUDBURROW_TEST_PUBSUB`; CI's run shard sets the
first pair, so the Storage and Pub/Sub suites do not run there a second time.
`TestCloudRunRevisionUsesInjectedMetadataADCAndServedEndpoints` also needs KMS, Scheduler and
Logging, read from `CLOUDBURROW_TEST_RUN_KMS`, `CLOUDBURROW_TEST_RUN_SCHEDULER` and
`CLOUDBURROW_TEST_RUN_LOGGING` when set, else `CLOUDBURROW_TEST_KMS`, `CLOUDBURROW_TEST_SCHEDULER`
and `CLOUDBURROW_TEST_LOGGING`.
`TestCloudRunRevisionGetsBigQueryRefusalsThroughTheFront` and
`TestCloudRunRevisionDialingTheBigQueryServiceGetsTheFront` (#881) need Cloud Run and BigQuery in one
instance, and read `CLOUDBURROW_TEST_RUN_BIGQUERY` when set, else `CLOUDBURROW_TEST_BIGQUERY`,
with `CLOUDBURROW_TEST_BIGQUERY_PROJECT`.
`TestAPodDialingTheBigQueryServiceGetsTheFrontWithoutCloudRun` (#902) runs a one-off pod on an
instance without Cloud Run, and needs `CLOUDBURROW_TEST_BIGQUERY`, `CLOUDBURROW_TEST_BIGQUERY_PROJECT`,
`CLOUDBURROW_TEST_KUBECONFIG` and `CLOUDBURROW_TEST_CLUSTER`, with docker, kind and go to build the
env probe; it skips where Cloud Run is enabled, whose tests cover it.

The `TestTofu*` tests run `cloudburrow terraform --binary tofu`. They use the OpenTofu binary
named by `CLOUDBURROW_TEST_TOFU`, which must then exist, or else `tofu` on PATH, and skip when
there is neither. CI's storage shard sets the variable.

`TestFunctionsFrameworkBuiltWithBuildpacks` (#678) runs only with
`CLOUDBURROW_TEST_FUNCTIONS=1`, and then needs `CLOUDBURROW_TEST_CLUSTER`,
`CLOUDBURROW_TEST_KUBECONFIG`, `pack`, and cluster
nodes that are amd64, the only platform Google's builder publishes. It starts a local
registry on a Docker network of its own, builds `testdata/function` twice through
`internal/buildpacks`, and deploys both images through the Cloud Run client. It removes its
registry, network and the function images it made; the builder and run images `pack` pulls
stay cached for the next run. No CI shard runs it; see
[docs/compatibility.md](../../docs/compatibility.md#what-ci-does-not-run-and-why).

`TestFunctionsRebuildReusesLayers` and `TestFunctionsBuildBrokenModulePathFails` (#678) are
gated by the same variable and need `pack` and Docker, but no cluster or instance: each
builds into a local registry of its own and runs nothing. The first builds
`testdata/function` twice to one image and asserts the rebuild's log reports
`Reusing layer '…'`; the second builds a copy whose `go.mod` declares a module path with no
dot and asserts the `build failed` error carries the buildpack's message. Both remove their
registry, network and the `pack-cache-*` volumes they created.

`TestPredictionStartupFailureIsReported` takes about **ten minutes**: Knative declares a
revision failed only after its 600s progress deadline. That latency is the finding, not an
accident — see [docs/prediction.md](../../docs/prediction.md).

## Safety

The harness refuses to run against anything but a local instance:

- **Cloud credentials in the environment are a hard failure**, not a skip.
  `GOOGLE_APPLICATION_CREDENTIALS`, `GOOGLE_CLOUD_PROJECT` and `GCLOUD_PROJECT` all abort the
  run. A compatibility suite that silently talked to real GCP would be expensive and wrong.
- **Non-loopback endpoints are refused**, and any `*.googleapis.com` or `*.google.com` host
  is rejected outright.
- Clients are built with `WithoutAuthentication`; application default credentials are never
  loaded.

## Isolation

Every test uses a **unique project ID** derived from the clock, so concurrent runs cannot
collide and no test depends on another's leftovers. Buckets, topics and subscriptions are
removed in `t.Cleanup`. The default ports are fixed, from 9000, so a second instance beside
the first needs its own `--port-base` (for example `--port-base 9200`), or port 0 for a
service to have the OS assign one; the script reads whatever each instance bound.

Every call is made under a bounded context, so a hanging operation fails its test instead of
stalling the suite.

## What a skip means

A test skips when its service endpoint is not exported — the service may legitimately not be
deployed. That is not the same as a blanket skip: each service asks for **its own** endpoint,
so an unimplemented operation can never pass by being skipped wholesale.

A test asks for each endpoint (`h.Endpoint`, or a helper that does) before it uses it, and
never reads a `CLOUDBURROW_TEST_*` endpoint with `os.Getenv` without skipping when it is
empty. A subtest asks through a harness of its own, `h := h.For(t)`: the parent's skip, called
from the subtest, fails it instead (#1625). test/repo's
`TestCompatTestsSkipWhereTheirEndpointsAreNotServed` finds each of these.

## Unverified error codes

Some errors are not documented by Google: for example, the code Cloud KMS returns when a
disabled key version is used. A test that asserts one marks it in the test function's doc
comment:

```go
// covers: google.cloud.kms.v1.KeyManagementService/Decrypt
// unverified: google.cloud.kms.v1.KeyManagementService/Decrypt FAILED_PRECONDITION: a DISABLED version
func TestDecryptRefusesADisabledVersion(t *testing.T) { ... }
```

The format is `// unverified: <Service>/<Method> <CODE>: <case>`, where `<CODE>` is a
canonical gRPC code name. In-process tests under `internal/service` may carry it too, for
cases that only a fake clock can reach. `go run ./tools/coverage` lists every annotation in an
**Unverified error codes** table on the service's coverage page and marks the method's row.
`-check` fails, naming the file and line, for an unknown method, a code that is not a gRPC code
name, or an annotation outside a test's doc comment.

**Pinning one.** A maintainer records an observation of Google's real behaviour: a Google
documentation URL, or an issue comment with the captured request and response. They link it
from the test and remove the annotation. No CI job ever calls Google to find out.

UNIMPLEMENTED for a method CloudBurrow does not serve is CloudBurrow's own policy, not a claim
about Google, so it is never marked unverified.

