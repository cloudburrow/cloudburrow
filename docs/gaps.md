# Remaining gaps

Every place [compatibility.md](compatibility.md) or [docs/coverage](coverage/README.md) says
something is not implemented (501, `UNIMPLEMENTED`, Unimplemented, Not served, Refused), was not
measured, or rests on an UNVERIFIED choice, as of main at a7982946 (2026-10-16). What an ADR or a
recorded maintainer decision excludes on purpose is in the last section. The per-operation detail
stays in compatibility.md; this page is the to-do view of it. Every row names the open issue that
tracks it; a closed issue is named only where it says what was built.

**Why it is open:**

- *not built yet*: nothing stops it but work;
- *no emulator support*: the upstream emulator refuses it, or there is nothing local for it to act on;
- *not documented by Google*: CloudBurrow had to choose a code, a message or a rule (`UNVERIFIED`);
- *needs live GCP*: only an observation of Google would settle it, and nothing here calls Google.

**Size:** S is a day or less, M a few days, L a new subsystem.

## Counts

| Service | Gaps | Not built yet | No emulator support | Not documented by Google | Needs live GCP |
|---|---|---|---|---|---|
| Cloud Storage | 2 | 0 | 1 | 1 | 0 |
| Pub/Sub | 2 | 0 | 0 | 1 | 1 |
| Cloud Tasks | 1 | 0 | 1 | 0 | 0 |
| Cloud Run | 2 | 0 | 1 | 0 | 1 |
| Cloud Scheduler | 1 | 0 | 0 | 0 | 1 |
| Cloud Logging | 1 | 0 | 0 | 1 | 0 |
| Cloud KMS | 5 | 0 | 1 | 3 | 1 |
| Secret Manager | 1 | 0 | 0 | 0 | 1 |
| Resource Manager | 1 | 0 | 0 | 1 | 0 |
| Vertex AI | 1 | 0 | 1 | 0 | 0 |
| BigQuery | 20 | 6 | 5 | 9 | 0 |
| Spanner, Bigtable, Firestore, Datastore | 4 | 0 | 3 | 0 | 1 |
| Cloud SQL, Memorystore | 4 | 3 | 1 | 0 | 0 |
| Functions, credentials, tooling | 1 | 1 | 0 | 0 | 0 |
| Console | 0 | 0 | 0 | 0 | 0 |
| **Total** | **46** | **10** | **14** | **16** | **6** |

## Cloud Storage

Closed: `objects.bulkRestore` (#1279), `buckets.operations.list` filters (#1280), `channels.stop`
and `objects.watchAll` (#1281), watchAll's listing parameters (#1393), channel delivery on an `up`
instance (#1290), the XML API's bucket `PUT`, service `GET` and form `POST` (#1258), the gRPC
`google.storage.v2` API (#1321), and its customer-supplied encryption keys, ACLs, managed-folder
IAM, `ComposeObject.delete_source_objects`, object contexts and `ListObjects.filter` (#1345). ACLs
and object IAM on the JSON API are excluded (below).

| Gap | Why open | Size |
|---|---|---|
| Notifications: the server with no Pub/Sub refuses a configuration 501, as it has no topic to verify or deliver to (#1444) (#1505); rechecked (#1521): the notifications insert reference lists no error, only the `storage.buckets.update` permission, and the Pub/Sub notifications guides describe a missing topic or publisher grant only as a delivery failure, so no documented error replaces the 501 | no emulator support (needs Pub/Sub) | S |
| UNVERIFIED error codes (a non-empty bucket delete, a repeat retention lock, managed-folder and folder refusals, batch limits, CORS on JSON endpoints and others; [storage.md](coverage/storage.md)) (#1445) | not documented by Google | S |

## Pub/Sub

Closed: `DetachSubscription`, `UpdateSnapshot` and Protocol Buffer schemas, served by CloudBurrow's
Pub/Sub front (#1406), and IAM on topics, subscriptions, snapshots and schemas, stored and never
enforced (#1367).

| Gap | Why open | Size |
|---|---|---|
| The front's messages for a refused `.proto` definition or message, and for the calls Google documents no answer to on a detached subscription (`FAILED_PRECONDITION` for a snapshot of it or a seek to a snapshot, `INVALID_ARGUMENT` for an immutable field), are its own (#1445; built in #1446) | needs live GCP: Google's messages not measured | S |
| UNVERIFIED error codes: subscription expiration, exactly-once and label refusals ([pubsub.md](coverage/pubsub.md); an invalid label's message is #982) (#1445) | not documented by Google | S |
## Cloud Tasks

Closed: `RunTask`, `BatchCreateTasks`, `BatchDeleteTasks` and the CMEK config (#1215), `oidcToken`
/ `oauthToken` on HTTP tasks and Cloud Scheduler jobs (#1243), `ListQueues` filters on any Queue
field (#1255), `stackdriver_logging_config`, with each sampled dispatch written to Cloud Logging
(#1256), and a queue-level `http_target` (#1282). Queue IAM conditions are excluded (below).

| Gap | Why open | Size |
|---|---|---|
| Delivery to an App Engine app (#1447) (#1506): with `--appengine-url` a local server stands in for the app and gets each attempt as `AppEngineHttpTarget` documents it (built in #1506), else every attempt fails `NOT_FOUND` (HTTP 404) (#1521) | no emulator support (no App Engine runtime runs locally, and Google documents no mapping to Cloud Run, so the stand-in is the developer's own server) | — |

## Cloud Run

Closed: IAM policy methods on Services and Jobs (#1216), and `UpdateService` with a partial
`update_mask` (#1260); container logs in Cloud Logging as `cloud_run_revision` and
`cloud_run_job` (#1298); the v1 (Knative) API, with `gcloud run services list`, `describe` and
`deploy --image` (#1293, #1297), its locations, IAM, jobs and domain mappings (#1349), job runs,
executions and tasks (#1379), and selectors, `overrides.delayExecution` and job IAM (#1420), and
domain mappings the gateway routes and `delayExecution` kept on jobs and executions (#1450);
`Builds.SubmitBuild` (#1268); `Instances.*` (#1299) and `WorkerPools.*` (#1300), with operations
that wait for readiness and an instance's URL (#1348), and a worker pool's `update_mask` and
instance splits (#1387); secret and in-memory volumes, the service account and v1 probes (#1297),
`subPath`, Cloud SQL volumes through the Auth Proxy sidecar, and VPC access, encryption keys,
binary authorization, execution environment and session affinity stored without effect (#1346);
the service account named to its workload by the metadata server (#1388); two projects' resources
of one ID (#1400); `gcloud run deploy --source` and `gcloud builds submit` with Artifact Registry
and Cloud Build (#1347), steps of any image (#1382), a Docker registry for built images (#1383),
`secretEnv`, `script` and `allowFailure` (#1407), Cloud Build triggers and worker pools (#1422),
and the default logs bucket (#1430); NFS volumes, Cloud Run's `K_*` and `CLOUD_RUN_TASK_ATTEMPT`
(#1448); Cloud Storage FUSE volumes from the instance's storage (#1474); the `h2c` port name (#1475).

| Gap | Why open | Size |
|---|---|---|
| A stale `etag` on a job is `ABORTED`; a failed `RunJob` is `FAILED_PRECONDITION`; `CancelExecution` of an ended execution is `FAILED_PRECONDITION` (#1445) | needs live GCP | S |
| Cloud Build: BuildTriggers whose source names a hosted git service (GitHub, GitLab, Bitbucket, Cloud Source Repositories) or a `filename` in one are refused 501 with the reason; Secure Source Manager repositories run their `.cloudbuild/triggers.yaml` triggers on push (#1521), in the console and with gcloud and Terraform (#1527); Developer Connect connections to this instance's Secure Source Manager and their git repository links are served, and a triggers-file trigger runs through its `devConnectGitRepositoryLink` (#1546), while a BuildTrigger's `developerConnectEventConfig` and Cloud Build v2 repositories stay refused, as Google documents no Secure Source Manager type for either (#1546); Pub/Sub triggers are built (#1506). Artifact Registry: every format and method is served (#1531, #1539), and remote `DOCKER`, `PYTHON`, `NPM` and `MAVEN` repositories over another of the instance's repositories (`commonRepository.uri`) pull through and cache (#1546), while remote repositories over public or custom upstreams, and remote `APT` and `YUM` ones, are refused, as egress is off (#1546) | no emulator support | M |

## Cloud Scheduler

Closed by the sweep: Locations (#1237).

| Gap | Why open | Size |
|---|---|---|
| `ResumeJob` on an enabled job and the default `retry_config` are inferred from the Terraform provider (#1445) | needs live GCP | S |

## Cloud Logging

Closed: `TailLogEntries`, `ListMonitoredResourceDescriptors` (#1215), and log-based metrics,
`MetricsServiceV2` (#1226); the Async bucket calls, links and settings of `ConfigServiceV2`
(#1292), and `CopyLogEntries` (#1394); the Terraform folder, organization and billing-account
resources (#1403); sink delivery to Cloud Storage, Pub/Sub, BigQuery and another project, and
`writer_identity` (#1404); log view filters and bucket retention applied on read, and
TailLogEntries on views and over JSON (#1410); `CopyLogEntries` from folder, organization and
billing-account buckets (#1411); Log Analytics, a link making a read-only BigQuery linked dataset
(#1423), with a view per log view, retention and DeleteLog applied to its rows, and links refused
on folder and organization buckets, as Google refuses them (#1435).

| Gap | Why open | Size |
|---|---|---|
| A BigQuery sink's `export_errors` column types and `schemaErrorDetail` text, `Any` fields but `serviceData` (left out), and the sink error codes but `topic_not_found` are CloudBurrow's (#1451; `export_errors` and `protoPayload` are built; `Duration` fields are `FLOAT` seconds, as Google's Log Analytics migration guide shows for `httpRequest.latency`, #1506); rechecked (#1521): "BigQuery schema for logs" names the error table's fields without types and says only that `schemaErrorDetail` "contains the complete error message returned by BigQuery"; the one `Any` mapping it states is audit logs' `request`, `response` and `metadata` as JSON strings in `*Json` columns, so the rest stays UNVERIFIED | not documented by Google (the export page lists the `export_errors` columns without types, and `schemaErrorDetail` only as BigQuery's message); needs a live sink | S |

## Cloud KMS

Closed: asymmetric, MAC and raw keys, random bytes, deletion and import jobs, the 19 Unimplemented
methods (#1212), and their console (#1219). Closed by the sweep: Locations (#1291).

| Gap | Why open | Size |
|---|---|---|
| REST: `alt` other than JSON, and `fields`/`$fields`, are `UNIMPLEMENTED` (#1445) | not documented by Google | S |
| UNVERIFIED error codes ([kms.md](coverage/kms.md)) (#1445) | not documented by Google | S |
| Project-number parents are refused; whether Google accepts them, and whether an empty key ring can be deleted (#1445) | needs live GCP | S |
| `key_access_justifications_policy` is enforced (#1506): encrypt, decrypt and sign fail under a policy with no reasons, as documented, or without `CUSTOMER_INITIATED_ACCESS`, a local call's reason; the refusal's code and text (`FAILED_PRECONDITION`) are CloudBurrow's, and project and organization default policies are not served (#1452) (#1521) | not documented by Google | S |
| Single-tenant HSM (#1469): instances and proposals never pass through their transitional states, since nothing is provisioned (HsmManagement and HSM_SINGLE_TENANT keys are served since #1469; HSM since #1452, without an HSM or an attestation; EXTERNAL and EXTERNAL_VPC since #1469, the instance acting as the EKM). Built in #1521, as kms v1.35.0's protos and the single-tenant guide define them: `upgrade_key_trust` proposals through the quorum approval, AES_WRAPPING (`AES_256_KWP`) keys, `trusted_wrapping_enabled`/`hsm_trusted`, HSM_SINGLE_TENANT import jobs, and `Import`/`ExportTrustedKeyWrappedCryptoKeyVersion` with RFC 5649 AES-KWP; the asymmetric material is PKCS #8 DER as for ordinary key import (UNVERIFIED: nothing states its encoding under trusted wrapping), and the key portability requirement is CloudBurrow's reading (UNVERIFIED). Still refused 501 with the reason: trusted re-import into an existing version, and ML-KEM keys, whose material encoding Google documents for no import | no emulator support (no HSM); the asymmetric encoding not documented by Google | S |

## Secret Manager

Closed: `RotateSecret`, `EnableManagedRotation` and `Secret.rotation` (#1215), `Secret.expire_time`
/ `ttl` (#1225), `ListSecrets` / `ListSecretVersions` filters (#1228), `Secret.topics` event
notifications to Pub/Sub (#1259), and `Secret.rotation` schedules and `version_destroy_ttl`
delayed destruction (#1278). IAM conditions are excluded (below).

| Gap | Why open | Size |
|---|---|---|
| Replication immutability is documented, not measured; 1 UNVERIFIED code (ephemeral restart) (#1445) | needs live GCP | S |
## Resource Manager

Closed: IAM policy methods, `MoveProject`, `UndeleteProject` (#1216); v1 org policy on projects
(#1296), and regional tag bindings and effective tags through the
`{location}-cloudresourcemanager` endpoints (#1350); project, folder and organization IAM, stored
and never enforced (#1368).

| Gap | Why open | Size |
|---|---|---|
| An app-enabled folder's management project is `google-mp-{folder number}`, shaped as the discovery document's one example: the project ID and display name Google assigns are not documented, and the APIs it enables on the project are not modelled (built in #1532) (#1445) | not documented by Google | S |

## Vertex AI

Closed: `PredictionService.EmbedContent`, over gRPC and REST `:embedContent` (#1244);
`PredictionService.StreamGenerateContent` over gRPC, and an official-SDK test of `GenerateContent`
over gRPC (#1257); `RawPredict` on a publisher model (#1284); `StreamRawPredict`, `DirectPredict`,
`ServerStreamingPredict`, `StreamingPredict` and `StreamDirectPredict` (#1285); `DirectRawPredict`,
`StreamDirectRawPredict` and `StreamingRawPredict`, relayed to a model's gRPC server on its `h2c` port (#1477); `Explain` with integrated gradients, XRAI or
examples, now `INVALID_ARGUMENT` on a custom container as Vertex documents (they are for TensorFlow
models only; Sampled Shapley, the custom-container method, is served) (#1454).

| Gap | Why open | Size |
|---|---|---|
| `EmbedContent` embeds text only: a non-text part, `document_ocr` and `audio_track_extraction` are `INVALID_ARGUMENT`, because the one embedding model served (EmbeddingGemma) is text-only, as Google's text-only `gemini-embedding-001` is; multimodal input needs a multimodal model (Google's `gemini-embedding-2`) run locally: an image, video, audio and PDF encoder sharing one vector space, which no local model here provides (#1454) | no emulator support (one text-only model) | L |

## BigQuery

Already tracked: #1427 (Iceberg files for `storage_uri`), and the rows below.

Closed: `CHANGES` and `APPENDS` (#1418), change history across table rebuilds and `CREATE OR REPLACE TABLE`, and the transaction, change data capture and one-day refusals (#1466), a table's `privacy_policy` aggregation threshold and join
restriction (#1426), materialized view statements in a script with a TEMP table or function
(#1434), `INTERVAL` arithmetic and the Write API's Arrow path (#1437), `CREATE TABLE` column
`OPTIONS` (#1438), copy jobs, snapshots and clones (#1002), extracting every column type (#957),
`EXECUTE IMMEDIATE` as a child job, `INTO`'s errors and `@@row_count` (#1037), scripting control
flow, handlers and failed statements' child jobs (#1201), procedures, script transactions and
RANGE variables (#1217), row access policies and table and routine IAM (#1218), infinite FLOAT64
in UNNEST (#1204), `ALTER TABLE` `ADD COLUMN`, `SET OPTIONS` and `ALTER COLUMN` (#1227),
`routines.update` (#1238), a lone `CREATE TABLE … LIKE` or `COPY` (#1239), a lone `CREATE VIEW`
with a column list (#1240), a `STRUCT` parameter with a `BYTES` field (#1241, #1082), a schema
update with a new column before the table's and a RANGE in a STRUCT parameter (#1111),
`CREATE TABLE … PARTITION BY … CLUSTER BY …` (#1272), partition decorators and the streaming
buffer (#1273), `INFORMATION_SCHEMA` `VIEWS` (#1274), the dataset views (#1286, #1328), region
`JOBS` and the region views (#1275, #1329), time travel `AS OF CURRENT_TIMESTAMP()` (#1276),
ingestion time (#1277), `RENAME TO`, `RENAME COLUMN`, `DROP COLUMN` (#1322), `SET DATA TYPE` and
keys (#1334), a view's column list in a script (#1323, #1335), a JSON load with `autodetect`
(#1324), CSV `timeZone`, SQL-style formats and encodings (#1325), `columnNameCharacterMap`
renaming (#1326, #1336, #1373), the Storage Write API's `arrow_rows` (#1327), C-style CSV formats
and formats with `autodetect` (#1338), a JSON load's field outside the schema (#1339), column
defaults with `SET DEFAULT` and `DROP DEFAULT` (#1340), `SCHEMATA_OPTIONS` and
`MATERIALIZED_VIEWS` (#1351), a script's child jobs (#1353), region `OBJECT_PRIVILEGES` and
`SCHEMATA_LINKS` (#1355), filtered `INFORMATION_SCHEMA.JOBS` queries (#1369), DML of rows
streamed in the last 30 minutes (#1370), project configurations (#1371, #1375, #1417), table
options and materialized views (#1389, #1390, #1401, #1409, #1415), `JOBS_BY_FOLDER`,
`JOBS_BY_ORGANIZATION` and unqualified `SCHEMATA_LINKS` (#1392), and DDL in dry runs, scripts and
`EXECUTE IMMEDIATE` (#1416).

| Gap | Why open | Size |
|---|---|---|
| `ALTER TABLE` is 501 for a foreign key to another project's table (an instance serves one project), `ALTER COLUMN SET OPTIONS (data_governance_tags)` (the engine's schema keeps no `dataGovernanceTagsInfo`), an `ALTER TABLE` in a dry run of a script (one in a script is carried out in its turn, #1459), and legacy SQL; a refusal the `tables.patch` path makes only when it is sent, in a statement of several actions, leaves the earlier actions done (every other refusal is checked first, #1456) | not built yet | S |
| A dry run of `CREATE VIEW` with a column list, without `OR REPLACE` or `IF NOT EXISTS`, of a view that exists is 501, as what BigQuery's dry run answers for it is not known (#1456); so is a STRUCT field's `data_policies`, as Google documents data policies on a column only (#1456); smart tuning's rewrite of a base-table query to read a materialized view is not built: the results are the same, and whether BigQuery chooses a view is its cost estimate (`rejectedReason` `COST`), which Google does not document (#1456) | not documented by Google | S |
| `columnNameCharacterMap` `V1` / `V2` when a detected CSV or Parquet name is still refused once its characters are replaced (a `V1` leading digit, a reserved prefix, an empty or too long name, a Parquet name with a period) and names the same once replaced are 501: BigQuery documents only that "invalid column names will be normalized" (JobConfigurationLoad read again 2026-10-17: V1 "Support alphanumeric + underscore characters and names must start with a letter or underscore", V2 "Support flexible column name", neither saying how a name still refused or two the same are named) (#1457) (#1547) | not documented by Google | S |
| Autodetect: a JSON column whose values mix types BigQuery does not document detecting (into a `JSON` column the schema gives, mixed values load since #1457), `skipLeadingRows` other than 1 into a new table, and a sample with a format or `timeZone` that BigQuery's detection is not documented for, are 501; CSV header detection where BigQuery and the emulator disagree (every column `STRING`, a name that reads as a number, boolean, date or timestamp) is 501; schema-detect read again 2026-10-17 (a sample of "up to the first 500 rows", the header rule, generic names "such as string_field_1") documents no more of these (#1457) (#1547) | not documented by Google | S |
| CSV: a quote outside ISO-8859-1, a delimiter of several bytes or outside ASCII in UTF-16 or UTF-32 data (the reference converts a quote or delimiter "to ISO-8859-1" and splits on "the first byte", and says nothing of other encodings), a `maxBadRecords` load the emulator still fails on a value the front passes (a number written as a fraction into an INTEGER, a DATETIME or TIMESTAMP), `sourceColumnMatch` `NAME` with no row skipped (ISO-8859-1 delimiters and quotes, a quote's first byte, `skipLeadingRows` N and the header with `autodetect`, `NAME` after skipped rows, and, in #1457's second part, a delimiter's first byte, a delimiter or quote outside ASCII in UTF-8 data and an empty `STRING` with a `nullMarker` and, under `maxBadRecords`, a BOOL, DATE, TIME or number its column refuses as a bad record are built); JSON: a null field outside the schema without `ignoreUnknownValues` (the reference does not say whether a null is an "extra value"; under `ignoreUnknownValues` it is ignored since #1457), a `maxBadRecords` load the emulator still fails on a value the front passes (a value its column refuses is a bad record since #1457) (#1547) | no emulator support | M |
| Parquet: the refused columns left (an INT96 with a part of a microsecond, a DECIMAL that would round, DECIMAL to STRING, a NULL element or a LIST of lists with list inference); a NaN loads and unlisted types are 400 since #1457; the Parquet loading page (read again 2026-10-17) maps LIST, ENUM, DECIMAL and INT96 but says nothing of these values, so they stay 501 (#1457) (#1547) | not documented by Google | S |
| Partition decorators: `tabledata.insertAll` into a partition of a column-partitioned table, `$__UNPARTITIONED__` of an ingestion-time partitioned table as a destination, a `CLONE`, `SNAPSHOT` or `RESTORE` of a partition, and through a partition a NULLABLE or the partitioning column left out without `ALLOW_FIELD_RELAXATION`, columns in another order or a RECORD's fields changed, are 501, none documented by BigQuery (docs read again 2026-10-16, and Google's docs are silent on each, so they stay 501: streaming to a decorator is documented for daily ingestion-time tables only, and schemaUpdateOptions name no rule for these) (#1458) | not documented by Google | S |
| `FOR SYSTEM_TIME AS OF` a past time of an ingestion-time partitioned table in a snapshot, clone or copy of `table@time` (a copy keeps no `_PARTITIONTIME`), or in a query whose select list has a `*` and that names `_PARTITIONTIME` or `_PARTITIONDATE` (#1458; queries otherwise read since #1458) (#1483) | not built yet | M |
| Script variable named as an alias, a column or a table; `WRITE_TRUNCATE_DATA` of a result of another type (#1445) | not documented by Google | S |
| BigQuery's wording of every front refusal (UNVERIFIED throughout the BigQuery rows) (#1445) | not documented by Google | S |
| Table options: `storage_uri` (#1427) is tracked on its own, and what #1426 and #1418 leave in the rows below (#1456) | not built yet | M |
| Project configurations: `default_kms_key_name` reaches `tables.insert`, a lone `CREATE TABLE` and a query, copy or JSON load job's new destination table, not a table a script makes or an upload's load; `ALTER PROJECT` / `ALTER ORGANIZATION` inside a block, `IF` or loop of a script is carried out in its turn (#1459), and 501 only in a dry run; `EFFECTIVE_PROJECT_OPTIONS` gives no `DEFAULT` rows, which Google does not document (#1456) | not built yet | S |
| A table made before the engine had the stream-time column (googlesqlite patch 0030; `CREATE TABLE … AS SELECT` has it since patch 0056, #1458; a temporary table cannot be streamed to) keeps no stream time, so its streamed rows are held from DML only while in the 20-second streaming buffer; a table restored from state saved before #1277 keeps no ingestion time (#1458) | no emulator support (the times were never recorded) | S |
| `PARTITIONS`' `last_modified_time` is NULL where the engine's change table holds no write (seven days kept), and for an ingestion-time partitioned table whose change table was made before googlesqlite patch 0050 (#1458); region `TABLE_STORAGE` has no physical byte columns, and an older table than 90 days no active or long-term split (#1458) | no emulator support (no compressed sizes; write times kept seven days) | S |
| Avro loads (built in #1457): a union of several types, a `map`, a recursive record, an array whose items may be null, a `decimal` made STRING by `decimalTargetTypes` and, for Avro and Parquet, a data file whose schema differs from its `referenceFileSchemaUri`'s (one of the reference's schema loads since #1457) are 501, as BigQuery does not document how it loads them; zstandard files are 501, as goavro reads null, deflate and snappy only (#1457) (#1547) | not documented by Google | S |
| `ORC` loads (no Go module in go.mod reads ORC), `DATASTORE_BACKUP` loads, `hivePartitioningOptions`, `jsonExtension` `GEOJSON` and a manifest `fileSetSpecType` are 501 (#1457) (#1547) | no emulator support | M |
| What #1426, #1455, #1482 and #1502 leave of `privacy_policy` (#1502 built: `SELECT WITH AGGREGATION_THRESHOLD` in a subquery, a CTE, after a `WITH` list and in `CREATE TABLE … AS`, scripts a statement at a time, joins that are not INNER by each `join_condition`'s wording): a read of an aggregation threshold or join restriction table or view in a subquery or CTE outside its own `SELECT WITH AGGREGATION_THRESHOLD`, or through another view, two clauses in one query, `JOIN_ANY` met only by an outer join, `JOIN_ALL` alongside one, a `NATURAL` join; of differential privacy, `COUNT(DISTINCT)`, `max_rows_contributed` and a DP aggregate inside an expression are 501: documented by neither BigQuery nor GoogleSQL (#1517) | not documented by Google | M |
| BQML: `LINEAR_REG`, binary and multiclass `LOGISTIC_REG`, `KMEANS`, `PCA`, explicit `MATRIX_FACTORIZATION`, `BOOSTED_TREE_REGRESSOR`, `BOOSTED_TREE_CLASSIFIER`, `RANDOM_FOREST_REGRESSOR`, `RANDOM_FOREST_CLASSIFIER`, `DNN_REGRESSOR`, `DNN_CLASSIFIER`, `AUTOENCODER` (#1548) and `ARIMA_PLUS` are trained by the front, with `TRANSFORM`, ML.PREDICT, ML.EVALUATE, ML.WEIGHTS, ML.FEATURE_INFO, ML.TRAINING_INFO, ML.CONFUSION_MATRIX, ML.ROC_CURVE, ML.CENTROIDS, ML.PRINCIPAL_COMPONENTS, ML.PRINCIPAL_COMPONENT_INFO, ML.RECOMMEND, ML.FEATURE_IMPORTANCE, ML.FORECAST, ML.EXPLAIN_FORECAST, ML.DETECT_ANOMALIES, ML.ARIMA_EVALUATE, ML.ARIMA_COEFFICIENTS, ML.EXPLAIN_PREDICT, ML.GLOBAL_EXPLAIN and models.* (#1465, #1481, #1504; ARIMA_PLUS's seasonal decomposition, spikes and dips, step changes and forecast limits #1518), and the preprocessing functions ML.BUCKETIZE, ML.POLYNOMIAL_EXPAND, ML.FEATURE_CROSS, ML.STANDARD_SCALER, ML.MIN_MAX_SCALER, ML.MAX_ABS_SCALER, ML.IMPUTER (mean), ML.DISTANCE, ML.LP_NORM and ML.NORMALIZER, in a query and in TRANSFORM (#1504), and ML.QUANTILE_BUCKETIZE, ML.ROBUST_SCALER, ML.NGRAMS and the vocabulary functions ML.LABEL_ENCODER, ML.ONE_HOT_ENCODER, ML.MULTI_HOT_ENCODER, ML.BAG_OF_WORDS and ML.TF_IDF (#1548); sampled boosted trees, wide-and-deep DNNs, a DNN's FTRL, CRELU, class weights and explanations, `ARIMA_PLUS_XREG` and ARIMA_PLUS's holiday regions, implicit matrix factorization, AutoML, imported and remote models, and ML.HASH_BUCKETIZE are 501 with the reason (#1548) (#1562) | no emulator support: training Google does not document, or Google's infrastructure | L |
| What #1503 leaves of change history: `ALTER SCHEMA … SET OPTIONS` in a multi-statement query with a TEMP table or function, or in a transaction, is 501 (#1522; any other script runs it in its turn since #1548); every table's rows are recorded twice, whatever its options (#1466) (#1548) (#1562) | not built yet | S |
| What #1483 leaves of undelete: `UNDROP SCHEMA` in a script with a TEMP table or function, or in a transaction, that has control flow or whose other statements name the dataset (any other script runs it in its turn since #1548), and in a dry run of a script (#1483) (#1503) (#1548) (#1562) | not built yet | M |

## Spanner, Bigtable, Firestore, Datastore

Closed: Datastore structured aggregations and Firestore `PartitionQuery` (#1405).

| Gap | Why open | Size |
|---|---|---|
| Whether production reorders an array's excluded values after its indexed ones, as the emulator does (#941) | needs live GCP | S |
| Firestore: `ExecutePipeline`, and the Firestore Admin user credentials (8 methods, each naming why; [firestoreadmin.md](coverage/firestoreadmin.md)); backups, restore, clone, `ExportDocuments`, `ImportDocuments` and `BulkDeleteDocuments` are served since #1500, and Datastore `explain_options` (#1500) (#1523) | no emulator support: pipelines and user credentials are Enterprise edition features (MongoDB compatibility), and the emulator is the Standard edition | L |
| Spanner: 2 RPCs refused, each naming why: `InternalUpdateGraphOperation` (Google's internal method) and `FetchCacheUpdate` (location-aware routing, with one server to route to; [spanner.md](coverage/spanner.md)). `AddSplitPoints` is a stored no-op of the call front since #1523 (the emulator never splits). Backups and restores, backup schedules, instance configurations (Google's and custom), `MoveInstance`, partition updates, database roles, the operation listings and IAM on backups and roles are served by the call front since #1498 (#1498) (#1523) | no emulator support: the emulator is one server | S |
| Bigtable: 4 RPCs refused, the session data plane: `GetClientConfiguration`, `OpenTable`, `OpenAuthorizedView`, `OpenMaterializedView` ([bigtable.md](coverage/bigtable.md)); change streams are served since #1523 from a change log the front keeps, with one partition and no garbage-collection records; SQL runs GoogleSQL for Bigtable's `SELECT` with views, with UNPACK, window functions and some documented functions refused (#1461) (#1499) (#1511) (#1523) | no emulator support | M |

## Cloud SQL, Memorystore

Closed: instance reads, databases, users and operations (#1301), instance writes, backups,
restores and clones, read replicas, point-in-time recovery and the connector path (#1330), imports
and exports, client certificates and server CA rotation, `tiers.list`, `flags.list` and
`operations.cancel` (#1380), database versions, major version upgrades, cascading and cross-region
replicas (#1381), high availability (#1408), Google's Auth Proxy binary (#1421),
`pointInTimeRestore` and Backup and DR backups through the Backup and DR Service (#1425, #1433);
the Memorystore admin API over Valkey (#1302), exports, imports and RDB persistence (#1384),
STANDARD_HA and failover (#1385), versions, upgrades and maintenance (#1386), in-transit
encryption (#1413), and customer-managed keys and server CA rotation (#1463). IAM database authentication is excluded (below).

| Gap | Why open | Size |
|---|---|---|
| Memorystore for Redis: `REDIS_3_2`, `REDIS_4_0` and `REDIS_5_0` stay 501, as they are end of life with no maintained image (the Docker official `redis` image last built them in 2018, 2020 and 2022); with a customer-managed key, the RDB snapshots and AOF Valkey writes on a server's own volume are not encrypted with it, and are served only while the key can be used (#1463) (#1501: still no maintained image, so they stay 501) (#1524) | no emulator support: no maintained image; Valkey writes its own files | S |
| Backup and DR: workload types other than Cloud SQL instances (Compute Engine instances and disks, AlloyDB, Filestore, which CloudBurrow does not run), management servers and the appliance protocol, and `serviceConfig.initialize` (15 methods 501, each naming what is missing in `tools/coverage/reasons.json`; `encryptionConfig` with this instance's Cloud KMS and `resourceBackupConfigs.list` of Cloud SQL instances are built since #1501, a project's, a folder's and the organization's `resourceBackupConfigs.fetch` with gcloud's filter, trials and `msComplianceMetadata` since #1536; [backupdr.md](coverage/backupdr.md)) (#1462) (#1501) (#1536) (#1524) | not built yet | L |
| Cloud SQL: a `PHYSICAL` external sync and `demote`/`demoteMaster` (the reverse direction, whose custom import needs Cloud SQL's `mysql.*` stored procedures) stay 501 (a MySQL external primary, its source representation instance, `verifyExternalSyncSettings` and `startExternalSync` are built since #1501; a PostgreSQL external primary by built-in logical replication, without pglogical, a MySQL replica seeded from `dumpFilePath`, and `OFFLINE` syncs since #1524); `MYSQL_5_6`, `MYSQL_5_7` and SQL Server versions, and `BAK` and `TDE` files (their images, `mysql:5.6`, `mysql:5.7` and `mcr.microsoft.com/mssql/server` 2022 and 2025, are linux/amd64 only, checked again with `docker manifest inspect` 2026-10-17); upgrading a PostgreSQL replica on its own (streaming replication does not cross major versions); high availability, read replicas and point-in-time recovery on PostgreSQL 9.6 to 11 (no `pg_promote()` or `recovery.signal`), each 501 naming why (PostgreSQL 9.6 to 12, a MySQL replica's own upgrade, Cloud SQL's 400 for a legacy failover replica and for a PostgreSQL database's charset change are built since #1462); 19 methods 501, each naming what is missing (blue-green deployments and `connect.resolve`, server and Entra ID certificates, `demote`, `switchover`, `reencrypt` and others; disk shrink and `resetReplicaSize` are built since #1535; [sqladmin.md](coverage/sqladmin.md)) (#1462) (#1501) (#1524) (#1535) | not built yet | L |
| Memorystore for Redis Cluster and for Valkey: cross-cluster (cross-instance) replication in CLUSTER mode or with in-transit encryption is 501 (a Valkey cluster-mode replica replicates only a primary of its own cluster), and migration into CLUSTER mode or from a source with AUTH; an engine upgrade does not carry writes made after its snapshot; IAM auth takes any AUTH string (IAM is not enforced); the nodes announce the host address to clients, so an in-cluster client must use pod addresses; a CLUSTER_DISABLED instance's reader endpoint is its first replica, with no automatic failover (#1519) (#1557) (#1563) | not built yet (the rest is built) | M |
## Functions, credentials, tooling

Closed: Functions in Node.js, Python, Java, Ruby, PHP and .NET, with a dated run (#1303, #1319);
Cloud Functions v2 and v1 APIs (#1377, #1428); Pub/Sub and project IAM in Terraform (#1367,
#1368).

| Gap | Why open | Size |
|---|---|---|
| Terraform resources beyond the verified set (#1304): a worker pool's `scaling_mode` (Google publishes no such field), `google_logging_folder_settings` (refused, as Google documents), `google_organization_iam_policy` (authoritative over the shared organization), `google_bigquery_data_transfer_config` (its endpoint is not set: not served), log bucket CMEK (refused), the Artifact Registry format data sources (Docker images, Python, npm and Maven packages), the Backup and DR and Cloud SQL data sources, and the other resources of the served services | not built yet (the rest: the eleven batches of #1304 and the verified rows of compatibility.md are built) | L |

## Console

Closed by the sweep: Test permissions on the Permissions tab (#1242), and the request-rate,
latency and resource charts (#1267); the Artifact Registry repository Permissions tab (#1439).

| Gap | Why open | Size |
|---|---|---|

## Excluded on purpose

Not gaps to close; each stays until its decision is amended.

- **Cloud Storage ACLs and object IAM** (`bucketAccessControls.*`, `objectAccessControls.*`,
  `defaultObjectAccessControls.*`, `objects.*IamPolicy`): 501 by [ADR-0006](adr/0006-iam-policy-surface.md), amended by #504.
- **IAM Conditions** on every policy (Tasks, Secret Manager, KMS, Storage, the console): refused
  by [ADR-0006](adr/0006-iam-policy-surface.md), amended by #365. **Audit configs** are refused
  too, except on projects, folders and the organization, where they are stored, not acted on (#1486).
- **IAM enforcement**: [ADR-0006](adr/0006-iam-policy-surface.md) stores policies, never enforces them.
- **IAM Credentials `signBlob`**: refused because a local signature makes signed URLs no Google
  service accepts ([ADR-0004](adr/0004-local-access-and-no-authentication.md)).
- **Cloud SQL IAM database authentication** (`generateEphemeralCert` with an access token,
  `cloudsql.iam_authentication`, users of an IAM type): 501, as CloudBurrow has no identities
  ([ADR-0004](adr/0004-local-access-and-no-authentication.md)).
- **Local AI and embeddings in per-PR CI**, and the default build's embeddings answering 501:
  maintainer decisions recorded in compatibility.md (§What CI does not run, §Local AI, #41).
